"""The adapter, the real engine and an offline replay, end to end, with no LEAN.

The algorithm runs against the real cmd/engine over a real Unix-domain socket
(ADR 0014). LEAN is played by the order-book fake in test_algorithm.py, which
behaves as the pinned image was observed to, and fills orders against a
synthetic (non-market) daily series under ADR 0005's rules: a buy stop fills
at max(level, open) and a sell stop at min(level, open), less or plus the
adapter's own slippage. The engine's journal is then verified and replayed by
cmd/backtest, and must replay to byte-identical decisions: the property the
LEAN acceptance run establishes on real data, here checked on every test run
without LEAN, docker or network.
"""
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import types
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest.mock import patch
from zoneinfo import ZoneInfo

tests_dir = str(Path(__file__).resolve().parent)
if tests_dir not in sys.path:
    sys.path.insert(0, tests_dir)

import test_algorithm as scaffold
from test_orders import OrderTestCase

algorithm = scaffold.algorithm
REPO = Path(__file__).resolve().parents[3]
CONFIGURATION = REPO / "cmd" / "engine" / "testdata" / "configuration.json"


def synthetic_series():
    """A range, a steady climb that breaks out and pyramids, then a slide.

    Not market data: every figure is generated here. Each entry is
    (open, high, low, close).
    """
    bars = []
    for i in range(25):
        close = 20 + (0.5 if i % 2 else -0.5)
        bars.append((close, close + 0.5, close - 0.5, close))
    close = bars[-1][3]
    for i in range(20):
        opening = close + 0.1
        close = opening + 0.7
        # The breakout bar trades far above its close, past the rung the
        # next session's entry fill sets, so the engine chains an Add from
        # that fill against the breakout bar (ADR 0021, section 7).
        bars.append((opening, close + (2.0 if i == 0 else 0.3), opening - 0.3, close))
    for _ in range(15):
        opening = close - 0.2
        close = opening - 1.3
        bars.append((opening, opening + 0.2, close - 0.3, close))
    return bars


class AdjustedHistory:
    """LEAN's one-bar split-adjusted History for bar: the same prices, no split after it."""
    empty = False

    def __init__(self, bar):
        self.index = [("AAPL", bar.EndTime)]
        self.iloc = [{"open": bar.Open, "high": bar.High, "low": bar.Low,
                      "close": bar.Close, "volume": bar.Volume}]

    def __len__(self):
        return 1


class EndToEndTests(OrderTestCase):
    @classmethod
    def setUpClass(cls):
        cls.work = tempfile.mkdtemp()
        cls.engine = os.path.join(cls.work, "engine")
        cls.backtest = os.path.join(cls.work, "backtest")
        for binary, package in ((cls.engine, "./cmd/engine"), (cls.backtest, "./cmd/backtest")):
            subprocess.run(["go", "build", "-o", binary, package], cwd=REPO, check=True)
        identity = subprocess.run(
            ["go", "run", "./adapter/lean/tests/testdata/configuration_identity.go",
             str(CONFIGURATION), "dev"], cwd=REPO, check=True, capture_output=True, text=True)
        cls.configuration_hash, cls.strategy_version = identity.stdout.split()

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.work, ignore_errors=True)

    def serve(self):
        """Start cmd/engine on a socket in a private directory short enough
        for sun_path (ADR 0014, consequence 2)."""
        socket_dir = tempfile.mkdtemp(dir="/tmp")
        self.addCleanup(shutil.rmtree, socket_dir, True)
        self.socket = os.path.join(socket_dir, "s.sock")
        self.journal = os.path.join(self.work, "journal-{}.jsonl".format(self.id().rsplit(".", 1)[-1]))
        engine = subprocess.Popen([self.engine, "-socket", self.socket, "-config", str(CONFIGURATION),
                                   "-out", self.journal, "-as-of", "2014-01-01T00:00:00Z"],
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        self.addCleanup(lambda: engine.poll() is None and engine.kill())
        self.assertIn("listening on", engine.stdout.readline())
        return engine

    def start_against(self, engine):
        settings = {"socket": self.socket, "configuration_hash": self.configuration_hash,
                    "strategy_version": self.strategy_version, "run_id": "end-to-end",
                    "symbol": "AAPL", "start": "2014-01-01", "end": "2014-03-31",
                    "warmup_bars": 21, "cash": 1000000,
                    "lean_image": scaffold.VALID_LEAN_IMAGE, "slippage_n": 0.05}
        algo = algorithm.CompletedBarsAlgorithm()
        with patch.object(algorithm, "load_settings", return_value=settings):
            algo.Initialize()
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        return algo

    def lean_fills(self, algo, bar):
        """LEAN's fill of every order working before this bar, against it."""
        for ticket in [t for t in algo.Transactions.tickets if t.Status == "submitted"]:
            if ticket.Quantity > 0 and bar.High >= ticket.StopPrice:
                reference = max(ticket.StopPrice, bar.Open)
                sign = 1
            elif ticket.Quantity < 0 and bar.Low <= ticket.StopPrice:
                reference = min(ticket.StopPrice, bar.Open)
                sign = -1
            else:
                continue
            slippage = algo.security.slippage_model.GetSlippageApproximation(
                algo.security, types.SimpleNamespace(Tag=ticket.Tag, Id=ticket.OrderId))
            price = reference + sign * slippage
            algo.Portfolio.Cash -= ticket.Quantity * price
            ticket.QuantityFilled = ticket.Quantity
            ticket.Status = "filled"
            algo.Portfolio.holdings["AAPL"] = algo.Portfolio.holdings.get("AAPL", 0) + ticket.Quantity
            algo.Transactions.emit(ticket, "filled", fill_quantity=float(ticket.Quantity),
                                   fill_price=price, fee=max(1.0, 0.005 * abs(ticket.Quantity)))

    def run_backtest(self, algo):
        start = datetime(2014, 1, 1, 16)
        for i, (opening, high, low, close) in enumerate(synthetic_series()):
            bar = types.SimpleNamespace(EndTime=start + timedelta(days=i), Open=opening, High=high,
                                        Low=low, Close=close, Volume=1000000)
            book = algo.Transactions
            # The previous slice's deferred reports, then LEAN's clock at this
            # bar's close, in UTC as LEAN's UtcTime states it.
            book.settle()
            book.now = bar.EndTime.replace(tzinfo=ZoneInfo("America/New_York")).astimezone(timezone.utc)
            self.lean_fills(algo, bar)
            algo.IsWarmingUp = i < 21
            algo.History = lambda *args, _bar=bar, **kwargs: AdjustedHistory(_bar)
            algo.OnData(scaffold.slice_of({"AAPL": bar}))
            self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        algo.Transactions.settle()
        algo.OnEndOfAlgorithm()

    def test_a_run_with_fills_replays_to_byte_identical_decisions(self):
        engine = self.serve()
        algo = self.start_against(engine)
        self.run_backtest(algo)
        engine.send_signal(signal.SIGINT)
        out, err = engine.communicate(timeout=60)
        self.assertEqual(engine.returncode, 0, out + err)

        with open(self.journal) as journal:
            records = [json.loads(line) for line in journal.read().splitlines()[1:]]
        inputs = [r["envelope"] for r in records if r["kind"] == "input"]
        kinds = {e["payload"]["kind"] for e in inputs if e["type"] == "execution.fill"}
        # The series exercises an entry, the Adds it pyramids into, an Add
        # chained from a fill, and the Exit-Channel exit of every Unit as one
        # fill on the way down.
        self.assertEqual(kinds, {"entry", "add", "exit"})
        decisions = [r["envelope"] for r in records if r["kind"] == "decision"]
        chained = [d for d in decisions if d["type"] == "strategy.add.proposed"
                   and d["causation_id"].split(":")[-2] == "fill"]
        self.assertTrue(chained, "no Add was chained from a fill")
        statuses = {e["payload"]["status"] for e in inputs if e["type"] == "execution.order.lifecycle"}
        self.assertTrue({"submitted", "updated"} <= statuses, statuses)
        self.assertEqual(inputs[-1]["type"], "replay.run.completed")

        verify = subprocess.run([self.backtest, "-verify", self.journal],
                                capture_output=True, text=True)
        self.assertEqual(verify.returncode, 0, verify.stdout + verify.stderr)
        self.assertIn("chain              verified", verify.stdout)
        self.assertIn("run                complete", verify.stdout)
        replay = subprocess.run([self.backtest, "-replay", self.journal],
                                capture_output=True, text=True)
        self.assertEqual(replay.returncode, 0, replay.stdout + replay.stderr)
        self.assertIn("replays byte-identically", replay.stdout)

    def test_an_add_the_cash_cannot_fund_is_declined_and_never_placed(self):
        """ADR 0010 and ADR 0020: a Unit that costs more than the cash left
        once earlier fills are debited is declined with insufficient-cash,
        and no order is placed for it.

        The account holds 100,000 of cash, enough for the entry (about 71,000)
        but not for the entry and Unit 2 (about 73,600) together; equity is
        left at 1,000,000 so the Notional Account and the Unit size are those
        of the other test. The entry's fill chains Unit 2 against the breakout
        bar, whose snapshot predates the fill, so only the fill's debit can
        decline it; later rungs are checked against snapshots that already
        show the spend.
        """
        engine = self.serve()
        algo = self.start_against(engine)
        algo.Portfolio.Cash = 100000.0
        self.run_backtest(algo)
        engine.send_signal(signal.SIGINT)
        out, err = engine.communicate(timeout=60)
        self.assertEqual(engine.returncode, 0, out + err)

        with open(self.journal) as journal:
            records = [json.loads(line) for line in journal.read().splitlines()[1:]]
        inputs = [r["envelope"] for r in records if r["kind"] == "input"]
        decisions = [r["envelope"] for r in records if r["kind"] == "decision"]
        kinds = [e["payload"]["kind"] for e in inputs if e["type"] == "execution.fill"]
        self.assertIn("entry", kinds)
        self.assertNotIn("add", kinds)

        declines = [d["payload"] for d in decisions if d["type"] == "strategy.proposal.declined"
                    and d["payload"]["kind"] == "add"]
        self.assertTrue(declines, "no Add was declined")
        for decline in declines:
            self.assertEqual(decline["reason"], "insufficient-cash")
            self.assertLess(decline["available_cash"], decline["required_cash"])
        chained = [d for d in decisions if d["type"] == "strategy.proposal.declined"
                   and d["causation_id"].split(":")[-2] == "fill"]
        self.assertTrue(chained, "the Add chained from the entry fill was not declined")
        self.assertFalse([d for d in decisions if d["type"] == "strategy.add.proposed"])

        add_tags = [t.Tag for t in algo.Transactions.tickets if t.Tag.startswith("add-proposal")]
        self.assertEqual(add_tags, [])
        add_lifecycle = [e["payload"] for e in inputs if e["type"] == "execution.order.lifecycle"
                         and e["payload"]["tag"].startswith("add-proposal")]
        self.assertEqual(add_lifecycle, [])

        replay = subprocess.run([self.backtest, "-replay", self.journal],
                                capture_output=True, text=True)
        self.assertEqual(replay.returncode, 0, replay.stdout + replay.stderr)


if __name__ == "__main__":
    unittest.main()
