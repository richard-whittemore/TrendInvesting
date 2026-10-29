"""The IRA's adapter feeds the real momentum engine (cmd/engine, built here) and the engine decides a dry-run month.

ADR 0026 points 2 and 3: the engine plans an order only for an instrument
with a mark as of the close, and the compact IRA stream carries no bar for a
stock newly picked (docs/manual/market-data.md, "A job for the IRAs"). The
adapter here is the real one (loop.AdapterLoop with the IRA's configuration)
against the scripted Gateway (fake_tws.py, IBKR's shapes from ibapi's own
decoder, testdata/ibapi_shapes.json), delivering a synthetic compact month
as the data job writes it: SPY, IEF and SHY bars every session, and at the
month's last session the universe snapshot and the ranked stocks' close
histories, no stock bar at all. The engine must decide the month and release
a buy for a stock it holds no bar for, valued against the mark the adapter
gave it with its IBKR contract; and a stock whose contract IBKR cannot
resolve gets no mark, so its order is refused before it is released
(missing-mark), never sized on a guess. Invented prices and contract IDs.
"""
import json
import math
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

tests_dir = Path(__file__).resolve().parent
if str(tests_dir) not in sys.path:
    sys.path.insert(0, str(tests_dir))

from fake_tws import ACCOUNT, FakeTWS, contract_details
from test_connection import Clock, config
from test_delivery import ENGINE_TIMEOUT, GO_TIMEOUT, _envelope, write_stream
from test_loop import Ids
from engine_client import connector
from loop import AdapterLoop
from relay import LOG_DECISION, LOG_INPUT, _committed_groups

REPO = tests_dir.parents[2]
# The month's names, as cmd/engine's synthetic month (momentum_test.go monthNames): listing type, sector, volume,
# first price and growth per session. Invented.
NAMES = [("AAA", "CS", "Tech", 3e6, 50, 1.02), ("BBB", "CS", "Tech", 2e6, 40, 1.018),
         ("CCC", "CS", "Health", 1.5e6, 30, 1.015), ("DDD", "CS", "Energy", 9e6, 20, 0.99),
         ("EEE", "CS", "Utilities", 1e6, 60, 1.005), ("IEF", "ETF", "", 1e7, 95, 1.0005),
         ("SHY", "ETF", "", 1e7, 80, 1.0002), ("SPY", "ETF", "", 1e8, 500, 1.002)]
CON_IDS = {"AAA": 900001, "BBB": 900002, "CCC": 900003, "DDD": 900004, "EEE": 900005, "IEF": 8314, "SHY": 8315,
           "SPY": 756733}
LAST = date(2025, 1, 31)
READ_AT = datetime(2025, 1, 31, 22, 5, tzinfo=timezone.utc)


def sessions():
    day, out = date(2025, 1, 2), []
    while day <= date(2025, 2, 3):
        if day.weekday() < 5 and day != date(2025, 1, 20) and day != date(2025, 1, 9):
            out.append(day)
        day += timedelta(days=1)
    return out


def price(base, growth, i):
    return round(base * math.pow(growth, i) * (1 + 0.002 * ((i * 7) % 5)) * 100) / 100


def month(run, configuration_hash, strategy_version):
    """The compact month's envelopes, as the data job writes them for one warm-up file."""
    cal = sessions()
    out = [("market.session.calendar", 1, "marketdata:calendar", "2025-01-02T09:00:00Z",
            {"source": "nyse", "sessions": [d.isoformat() for d in cal]})]
    closes = {n[0]: [] for n in NAMES}
    for i, day in enumerate(cal[:-1]):
        end = "{}T21:00:00Z".format(day.isoformat())
        for n in NAMES:
            closes[n[0]].append((day.isoformat(), price(n[4], n[5], i)))
        etfs = sorted(n[0] for n in NAMES if n[1] == "ETF")
        for symbol in etfs:
            p = closes[symbol][-1][1]
            volume = [n for n in NAMES if n[0] == symbol][0][3]
            view = lambda label: {"view": label, "open": p, "high": p, "low": p, "close": p, "volume": volume}  # noqa: E731
            out.append(("market.bar.completed", 2, "marketdata:bar:{}:{}".format(symbol, day), end, {
                "instrument_id": symbol, "period_end": end, "split_adjusted": view("split-adjusted"),
                "raw": view("raw"), "adjusted": view("split-and-dividend-adjusted"), "adjusted_base": "2025-01-02"}))
        if day == LAST:
            rows = [{"instrument_id": n[0], "type": n[1], "close": closes[n[0]][-1][1], "volume": n[3],
                     "sector": n[2], "sector_source": "ishares-iwv-gics" if n[2] else ""} for n in NAMES]
            out.append(("market.universe.snapshot", 1, "marketdata:universe:{}".format(day), end,
                        {"session": day.isoformat(), "source": "massive", "rows": rows}))
            for n in NAMES:
                if n[1] == "CS":
                    out.append(("market.close.history", 1, "marketdata:history:{}:{}".format(n[0], day), end, {
                        "instrument_id": n[0], "session": day.isoformat(), "adjusted_base": "2025-01-02",
                        "closes": [{"session": s, "close": c} for s, c in closes[n[0]]]}))
        out.append(("market.session.closed", 1, "marketdata:session-closed:{}".format(day), end,
                    {"period_end": end, "instrument_ids": etfs}))
    return [_envelope(*e, sequence=n, run=run, configuration_hash=configuration_hash,
                      strategy_version=strategy_version) for n, e in enumerate(out, 1)]


class RealEngineMonthTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.work = tempfile.mkdtemp()
        env = dict(os.environ, GOTOOLCHAIN="local")
        for name, package in (("engine", "./cmd/engine"), ("operator", "./cmd/operator")):
            try:
                subprocess.run(["go", "build", "-ldflags",
                                "-X github.com/richard-whittemore/TrendInvesting/internal/buildinfo.Version=ira-test",
                                "-o", os.path.join(cls.work, name), package], cwd=str(REPO), env=env,
                               timeout=GO_TIMEOUT, capture_output=True, text=True, check=True)
            except FileNotFoundError:
                raise unittest.SkipTest("no Go toolchain: the real engine is built here")
            except subprocess.CalledProcessError as err:
                raise AssertionError("go build {} failed: {}{}".format(package, err.stdout, err.stderr))

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.work, True)

    def run_month(self, **fake_overrides):
        root = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: shutil.rmtree(root, True))
        sockets = tempfile.mkdtemp(dir=os.environ.get("TI_TEST_SOCKET_DIR", "/tmp"))
        self.addCleanup(lambda: shutil.rmtree(sockets, True))
        cfg = json.loads((REPO / "cmd" / "engine" / "testdata" / "momentum-dry-run.json").read_text())
        cfg["account_id"] = ACCOUNT
        cfg["momentum"]["universe_input"] = "compact"
        config_path = root / "trad-ira.config"
        config_path.write_text(json.dumps(cfg))
        approval = root / "approval.json"
        subprocess.run([os.path.join(self.work, "operator"), "approve", "-first-run", "-strategy", "momentum",
                        "-account", ACCOUNT, "-by", "owner", "-positions", "", "-cash", "10000.00", "-currency",
                        "USD", "-out", str(approval), "-at", "2025-01-02T14:00:00Z"],
                       check=True, capture_output=True, timeout=GO_TIMEOUT)
        log = root / "run-1.log"
        socket = os.path.join(sockets, "s.sock")
        out = root / "engine.out"
        with open(out, "w") as sink:
            engine = subprocess.Popen([os.path.join(self.work, "engine"), "-mode", "dry-run", "-config",
                                       str(config_path), "-as-of", "2025-01-02T21:00:00Z", "-socket", socket,
                                       "-log", str(log), "-out", str(root / "run-1.journal"), "-approval",
                                       str(approval)], stdout=sink, stderr=subprocess.STDOUT)
        self.addCleanup(lambda: engine.poll() is None and (engine.kill(), engine.wait()))
        deadline = time.monotonic() + ENGINE_TIMEOUT
        while "listening on" not in out.read_text():
            self.assertIsNone(engine.poll(), out.read_text())
            self.assertLess(time.monotonic(), deadline, out.read_text())
            time.sleep(0.05)
        header = json.loads(log.read_text().splitlines()[0])
        write_stream(root / "streams" / "run-1", LAST, month("run-1", header["configuration_hash"],
                                                             header["strategy_version"]), prefix="trad-ira")
        fake = FakeTWS()
        fake.server_time = int(READ_AT.timestamp())
        fake.summary = [(ACCOUNT, "NetLiquidation", "10000.00", "USD"), (ACCOUNT, "TotalCashValue", "10000.00", "USD"),
                        (ACCOUNT, "ExcessLiquidity", "10000.00", "USD"), (ACCOUNT, "SettledCash", "10000.00", "USD")]
        fake.contract_details = {s: [contract_details(s, c)] for s, c in CON_IDS.items()}
        for k, v in fake_overrides.items():
            setattr(fake, k, v)
        clock = Clock()

        def sleep(seconds):
            clock.now += seconds
            fake.waited()
        loop = AdapterLoop(config(client_id=12, account_snapshot_version=4, resolve_contracts=True, con_ids={}),
                           fake, connector(socket, 30.0), str(root / "pending.json"), str(log),
                           now=lambda: READ_AT, today=lambda: LAST, sleep=sleep, monotonic=clock, poll=0,
                           new_id=Ids(), streams=str(root / "streams"))
        self.addCleanup(loop.close)
        loop.start()
        for _ in range(400):
            loop.step()
            if not loop.outbox.load() and "session-closed:2025-01-31" in log.read_text():
                break
        engine.send_signal(signal.SIGTERM)
        engine.wait(timeout=ENGINE_TIMEOUT)
        groups = _committed_groups(str(log))[1]
        inputs = [g[0][2] for g in groups if g[0][1] == LOG_INPUT]
        decisions = [e for g in groups for _, kind, e in g if kind == LOG_DECISION]
        self.assertIn("marketdata:session-closed:2025-01-31", [e["id"] for e in inputs], out.read_text())
        self.assertEqual(fake.placed, [], "a dry-run sends nothing to IBKR")
        return inputs, decisions

    def test_the_engine_decides_the_month_and_buys_a_stock_it_has_no_bar_for(self):
        inputs, decisions = self.run_month()
        decided = [d["payload"] for d in decisions if d["type"] == "strategy.targets.decided"]
        self.assertEqual([(d["outcome"], d["momentum"]["close_session"]) for d in decided], [("decided", "2025-01-31")],
                         decided)
        stocks = {t["instrument_id"] for t in decided[0]["targets"]} & {n[0] for n in NAMES if n[1] == "CS"}
        self.assertTrue(stocks, decided[0]["targets"])
        released = [d["payload"] for d in decisions if d["type"] == "risk.order.released"]
        refused = [d["payload"] for d in decisions if d["type"] == "risk.order.refused"]
        self.assertEqual([r for r in refused if r["reason"] == "missing-mark"], [])
        bought = {r["instrument_id"] for r in released if r["side"] == "buy"}
        self.assertTrue(stocks & bought, (released, refused))
        self.assertTrue(all(r["send"] is False for r in released))
        marks = {e["payload"]["instrument_id"]: e for e in inputs if e["type"] == "market.mark.recorded"}
        for stock in stocks & bought:
            self.assertEqual(marks[stock]["schema_version"], 3)
            self.assertEqual(marks[stock]["payload"]["contract"]["con_id"], CON_IDS[stock])
        snapshots = [e for e in inputs if e["type"] == "account.snapshot"]
        self.assertTrue(snapshots and all(e["schema_version"] == 4 for e in snapshots))
        self.assertEqual(snapshots[-1]["payload"]["settled_cash"], 10000.0)
        synthetic = [d for d in decisions if d["type"] == "execution.order.lifecycle"]
        self.assertEqual(len(synthetic), len(released), "each dry-run release ends with its synthetic cancel")
        self.assertTrue(all(d["payload"]["synthetic"] and d["payload"]["status"] == "canceled" for d in synthetic))

    def test_a_pick_whose_contract_cannot_be_resolved_is_refused_for_its_missing_mark_never_sized(self):
        first, _ = self.run_month()
        _, decisions = self.run_month()
        picks = [t["instrument_id"] for d in decisions if d["type"] == "strategy.targets.decided"
                 for t in d["payload"]["targets"] if t["instrument_id"] in CON_IDS and CON_IDS[t["instrument_id"]] > 900000]
        self.assertTrue(picks)
        details = {s: [contract_details(s, c)] for s, c in CON_IDS.items() if s != picks[0]}
        _, decisions = self.run_month(contract_details=details, contract_detail_errors={
            picks[0]: (200, "No security definition has been found for the request")})
        refused = [d["payload"] for d in decisions if d["type"] == "risk.order.refused"]
        released = [d["payload"]["instrument_id"] for d in decisions if d["type"] == "risk.order.released"]
        self.assertIn((picks[0], "missing-mark"), [(r["instrument_id"], r["reason"]) for r in refused], refused)
        self.assertNotIn(picks[0], released)


if __name__ == "__main__":
    unittest.main()
