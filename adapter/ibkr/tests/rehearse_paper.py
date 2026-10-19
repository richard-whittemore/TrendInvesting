"""The paper rehearsal: one whole synthetic paper day, end to end, against the fake gateway and an engine stand-in.

Run by deploy/rehearse-paper.sh before the first real paper order
(docs/runbooks/first-paper-order.md, B3; docs/manual/paper-trading.md, "A
paper day"). It runs the data job (adapter/marketdata/tidata.py) on
synthetic Massive answers, then starts `python3 -m adapter.ibkr run`
(cli.main) exactly as the server's unit does, with --streams, on a paper
configuration with sending switched on and a paper-shaped account read from
a secrets file, with only Massive, IB Gateway and the engine replaced:
Massive by a synthetic client, IB Gateway by tests/fake_tws.py, and the
engine by tests/engine_double.py behind a real Unix-domain socket, so the
adapter's socket client is the one the server uses. Nothing reaches
Massive or IBKR.

It proves, in order, and stops at the first that fails, exiting 1:

1. the data job writes the run's warm-up and, once the day has settled,
   the day's stream, stamped with the run's identity from the engine's log;
2. the adapter starts, reads and matches the account before anything else,
   and delivers the warm-up behind one broker snapshot, in file order;
3. the day's stream reaches the engine as its broker snapshot (with IBKR's
   mark for the holding), its bar, then its close; the stand-in releases
   one order on that close only if the snapshot is fresh at the close's
   recorded time (ADR 0019's snapshot rule, as the engine checks a sell),
   its durable log shows that release, and the order goes out once, only
   after it, as released: a whole-share DAY limit order for the account,
   tagged with its client order ID;
4. IBKR's acceptance reaches the engine as a lifecycle input, and each of
   two fills as a fill input once its commission arrives;
5. a dropped link is reconnected and matched again, and neither the order
   nor any stream input is sent again;
6. a restart of the adapter on the same files sends nothing again and
   records nothing twice;
7. a crash of the engine, resumed under a new run name (a new TI_RUN,
   engine -resume: the new log holds the crashed run's records, so the
   run's identity carries over, ADR 0027): the data job writes the next
   day's stream into the run's own folder, and the adapter, started on the
   new log, delivers it to the resumed run behind one broker snapshot,
   once, sending nothing earlier again.

ibapi is not installed here, so the imported ibapi's version is given as
the pinned one; everything else the start command checks is checked. The
day is the latest NYSE session on or before today (New York), and the
adapter is told it is that session's date, so the order is one released in
"today's" session as on the server.

The account is synthetic (DU0000001) unless TI_REHEARSAL_ACCOUNT holds one
beginning DU (deploy/rehearse-paper.sh asks for it at a hidden prompt); it
is never printed.
"""
import contextlib
import datetime as dt
import hashlib
import io
import json
import os
import socket
import sys
import tempfile
import threading
from datetime import datetime, timezone
from decimal import Decimal
from pathlib import Path
from zoneinfo import ZoneInfo

tests_dir = Path(__file__).resolve().parent
adapter_dir = tests_dir.parent
repo = adapter_dir.parents[1]
for p in (str(tests_dir), str(adapter_dir), str(adapter_dir.parent)):
    if p not in sys.path:
        sys.path.insert(0, p)

from cli import main as adapter_main  # noqa: E402
from config import is_paper_account  # noqa: E402
from engine_double import EngineRefused, LoggedEngine  # noqa: E402
from fake_tws import FakeTWS, contract as fake_contract  # noqa: E402
from ibapi_pin import read_pin  # noqa: E402
from marketdata import nyse_calendar, tidata  # noqa: E402
from relay import RELEASED  # noqa: E402
from reports import FILL_TYPE, LIFECYCLE_TYPE  # noqa: E402
from snapshot import BROKER_SNAPSHOT_TYPE  # noqa: E402

NEW_YORK = ZoneInfo("America/New_York")
SYNTHETIC_ACCOUNT = "DU0000001"
CLIENT_ORDER_ID = "rehearsal-spy-1"
SPY_CON_ID = 756733
# The most loop steps one phase may take before the rehearsal calls it stuck.
MAX_STEPS = 400
BAR = "market.bar.completed"
CLOSED = "market.session.closed"
RUN = "run-rehearsal"
# The warm-up's sessions: the shortest history the configuration below
# accepts (its longest indicator window, five sessions) and one more.
WARM_SESSIONS = 6
# The paper account's synthetic holding before the day, and IBKR's synthetic
# last price for SPY, the mark the broker snapshot carries.
HELD = 10
MARK = 401.17
# A synthetic stand-in for the Massive key: not a credential.
SYNTHETIC_KEY = "synthetic-rehearsal-key"
# Set by the rehearsal's own test: the day's stream is altered after the data
# job wrote it, which the adapter must refuse.
STREAM_TAMPER = False


class SyntheticMassive:
    """Massive's three endpoints the data job uses, answered with invented SPY bars and no corporate actions."""

    def grouped(self, day):
        close = 400.0 + day.toordinal() % 7
        return {"status": "OK", "results": [{"T": "SPY", "o": close, "h": close + 1, "l": close - 1, "c": close,
                                             "v": 1000, "t": 0}]}

    def splits(self, first, last):
        return [{"results": []}]

    def all_dividends(self, first, last):
        return [{"results": []}]


def boost_configuration(warm_up_start):
    """The shipped Boost configuration in paper, with indicators short enough for a few sessions' warm-up."""
    cfg = json.loads((repo / "deploy" / "etc" / "engines" / "boost.config").read_text())
    cfg["mode"] = "paper"
    cfg["boost"].update(warm_up_start=warm_up_start.isoformat(), trend_days=5, exit_sma=5)
    return cfg


def data_job(work, config, engine_log, now, *extra):
    """Run ti-data once at now; the stream's path, or RehearsalFailed with its refusal."""
    out, err = io.StringIO(), io.StringIO()
    argv = ["--data", str(work / "data"), "--streams", str(work / "data" / "streams"), "--config", str(config),
            "--engine-log", str(engine_log), "--secrets-file", str(work / "secrets.env")] + list(extra)
    with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
        status = tidata.main(argv, now=lambda: now, client_factory=lambda key: SyntheticMassive())
    if status != 0:
        raise RehearsalFailed("the data job refused: {}".format(err.getvalue().strip().splitlines()[-1:]))
    return Path(out.getvalue().strip())


def settled(day):
    """A time after day's close plus the data job's settle minutes."""
    return nyse_calendar.session_close(day).astimezone(timezone.utc) + dt.timedelta(minutes=65)


class RehearsalFailed(Exception):
    """One of the rehearsal's checks failed."""


def check(ok, what):
    if not ok:
        raise RehearsalFailed(what)
    print("PASS: " + what)


class SocketEngine:
    """engine_double.LoggedEngine served on a Unix-domain socket, replying as cmd/engine lays a reply out."""

    def __init__(self, path, engine):
        self.engine = engine
        self.server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.server.bind(path)
        self.server.listen(4)
        self.thread = threading.Thread(target=self._serve, daemon=True)
        self.thread.start()

    def close(self):
        self.server.close()

    def _serve(self):
        while True:
            try:
                conn, _ = self.server.accept()
            except OSError:
                return
            threading.Thread(target=self._connection, args=(conn,), daemon=True).start()

    def _connection(self, conn):
        buf = b""
        with conn:
            while True:
                while b"\n" not in buf:
                    chunk = conn.recv(65536)
                    if not chunk:
                        return
                    buf += chunk
                line, _, buf = buf.partition(b"\n")
                envelope = json.loads(line)
                try:
                    reply = self.engine.decide(envelope)
                except EngineRefused as refused:
                    conn.sendall(json.dumps({"error": {"code": "invalid", "message": str(refused),
                                                       "causation_id": envelope.get("id", "")}}).encode() + b"\n")
                    continue
                raw = json.dumps(reply["payload"], separators=(",", ":")).encode()
                head = {k: v for k, v in reply.items() if k != "payload"}
                head["payload_hash"] = hashlib.sha256(raw).hexdigest()
                text = json.dumps(head, separators=(",", ":"))
                conn.sendall(b'{"envelope":' + text[:-1].encode() + b',"payload":' + raw + b"}}\n")


class Phase:
    """should_stop for one run of the adapter: acts on the fake gateway as each condition is met, then stops."""

    def __init__(self, actions):
        self.actions = list(actions)
        self.steps = 0

    def __call__(self):
        self.steps += 1
        if self.steps > MAX_STEPS:
            raise RehearsalFailed("the adapter made no progress within {} steps (waiting for: {})".format(
                MAX_STEPS, self.actions[0][2] if self.actions else "nothing"))
        while self.actions and self.actions[0][0]():
            _, act, _ = self.actions.pop(0)
            if act is None:
                return True
            act()
        return False


def rehearse(work, account):
    today = datetime.now(NEW_YORK).date()
    session = today
    while not nyse_calendar.is_trading_day(session):
        session -= dt.timedelta(days=1)
    before = nyse_calendar.sessions(session - dt.timedelta(days=30), session)[-(WARM_SESSIONS + 1):-1]
    warm_up_start, eve = before[0], before[-1]
    stamp = session.strftime("%Y%m%d")
    released = {"account_id": account, "plan_id": "order-planned:" + CLIENT_ORDER_ID,
                "tag": "targets-decided:rehearsal", "client_order_id": CLIENT_ORDER_ID, "instrument_id": "SPY",
                "side": "buy", "quantity": 9, "limit_price": 401.5, "mode": "paper", "send": True}
    day_close = "marketdata:session-closed:{}".format(session)

    cfg = boost_configuration(warm_up_start)
    config = work / "boost.config"
    config.write_text(json.dumps(cfg))
    (work / "secrets.env").write_text("MASSIVE_API_KEY={}\n".format(SYNTHETIC_KEY))
    engine_log = work / (RUN + ".log")
    # The stand-in releases on the day's close only if the broker snapshot
    # ahead of it is fresh at its recorded time, as the engine sizes a sell.
    engine = LoggedEngine(str(engine_log), {day_close: [{"type": RELEASED, "payload": released}]},
                          configuration=cfg,
                          snapshot_age=dt.timedelta(minutes=cfg["execution"]["max_snapshot_age_minutes"]))
    streams = work / "data" / "streams" / "boost"
    warm = data_job(work, config, engine_log, settled(eve), "--backfill-from", warm_up_start.isoformat())
    sock_path = str(work / "engine.sock")
    server = SocketEngine(sock_path, engine)
    pending = work / "adapter" / "pending.json"
    pending.parent.mkdir()
    secrets = work / "boost.env"
    secrets.write_text("IBKR_ACCOUNT_BOOST={}\n".format(account))
    os.chmod(str(secrets), 0o600)
    adapter_config = work / "boost.json"
    adapter_config.write_text(json.dumps({
        "host": "127.0.0.1", "port": 4002, "client_id": 11, "mode": "paper", "paper_sending_enabled": True,
        "connect_timeout": 2.0, "request_timeout": 0.2, "mark_timeout": 0.2, "heartbeat_interval": 30.0, "heartbeat_timeout": 10.0,
        "restore_timeout": 60.0, "backoff_initial": 0.01, "backoff_max": 0.05, "max_connect_attempts": 3,
        "commission_wait": 30.0, "con_ids": {"SPY": SPY_CON_ID}}, indent=2))
    versions = repo / "deploy" / "versions.env"
    version, sha = read_pin(str(versions))
    record = work / "INSTALLED"
    record.write_text("TWSAPI_VERSION={}\nTWSAPI_SHA256={}\n".format(version, sha))

    gateway = FakeTWS()
    gateway.managed_accounts = (account,)
    gateway.summary = [(account, "NetLiquidation", "1000000.00", "USD"),
                       (account, "TotalCashValue", "1000000.00", "USD"),
                       (account, "ExcessLiquidity", "1000000.00", "USD")]
    gateway.positions = [(account, fake_contract(), Decimal(HELD), 395.0)]
    gateway.marks = {SPY_CON_ID: MARK}
    # IBKR's clock: the evening of the day, after the data job has run.
    gateway.server_time = int(settled(session).timestamp()) + 600
    gateway.on_place = gateway.accept

    def argv(engine_log):
        return ["run", "--config", str(adapter_config), "--pending", str(pending), "--engine-log", str(engine_log),
                "--socket", sock_path, "--streams", str(streams), "--secrets", str(secrets),
                "--account-key", "IBKR_ACCOUNT_BOOST", "--ibapi-record", str(record), "--versions", str(versions)]
    log = (work / "adapter.log").open("w")

    def run(phase, engine_log=engine_log):
        status = adapter_main(argv(engine_log), api_factory=lambda cfg: gateway, should_stop=phase, err=log,
                              poll=0.01,
                              ibapi_version=lambda: tuple(int(p) for p in version.split(".")),
                              today=lambda: session,
                              now=lambda: datetime.fromtimestamp(gateway.server_time, timezone.utc))
        log.flush()
        if status != 0:
            raise RehearsalFailed("the adapter stopped (exit {}): {}".format(
                status, (work / "adapter.log").read_text().strip().splitlines()[-1:]))

    def types():
        return [e["type"] for e in engine.inputs()]

    def ids():
        return [e["id"] for e in engine.inputs()]

    def placed():
        return len(gateway.placed)

    written = {}

    def the_day():
        written["day"] = data_job(work, config, engine_log, settled(session))
        if STREAM_TAMPER:
            lines = written["day"].read_text().splitlines()
            lines[0] = lines[0].replace('"SPY"', '"SPX"', 1)
            os.chmod(str(written["day"]), 0o644)
            written["day"].write_text("\n".join(lines) + "\n")
        written["placed_before"] = placed()

    def fill():
        gateway.fill(7, shares="4", price=401.25, exec_id="0001f4e8.rehearsal.01.01", cum_qty="4", commission="1.00",
                     time="{} 09:30:01 US/Eastern".format(stamp))
        gateway.fill(7, shares="5", price=401.3, exec_id="0001f4e8.rehearsal.02.01", cum_qty="9", commission="0.35",
                     time="{} 09:30:04 US/Eastern".format(stamp))

    matched = {}

    def drop():
        matched["executions"] = len(gateway.called("req_executions"))
        gateway.drop()

    warm_inputs = 1 + 2 * WARM_SESSIONS
    try:
        run(Phase([
            (lambda: len(types()) >= warm_inputs, the_day, "the warm-up to reach the engine"),
            (lambda: LIFECYCLE_TYPE in types(), fill, "the order's acceptance to reach the engine"),
            (lambda: types().count(FILL_TYPE) == 2, drop, "both fills to reach the engine"),
            (lambda: len(gateway.called("connect")) == 2
             and len(gateway.called("req_executions")) > matched["executions"], None,
             "the reconnect to be matched again"),
        ]))
        check(warm.parent == streams / RUN and written["day"] == streams / RUN / "boost-{}.jsonl".format(session)
              and warm.name == "boost-{}.jsonl".format(eve),
              "the data job wrote the warm-up and the day's stream for the run")
        calls = [c[0] for c in gateway.calls]
        check(calls.index("req_account_summary") < calls.index("place_order")
              and calls.index("req_executions") < calls.index("place_order"),
              "the account, open orders and executions were read before the order was sent")
        sent = engine.inputs()
        check(types()[:warm_inputs] == [BROKER_SNAPSHOT_TYPE] + [BAR, CLOSED] * WARM_SESSIONS
              and [e["id"] for e in sent[1:warm_inputs]] == [json.loads(line)["id"] for line in
                                                             warm.read_text().splitlines()]
              and [e["sequence"] for e in sent[:warm_inputs]] == list(range(2, 2 + warm_inputs)),
              "the warm-up reached the engine behind one broker snapshot, in file order")
        day = sent[warm_inputs:warm_inputs + 3]
        snapshot = day[0]["payload"] if day else {}
        check([e["type"] for e in day] == [BROKER_SNAPSHOT_TYPE, BAR, CLOSED]
              and day[1]["id"] == "marketdata:bar:SPY:{}".format(session) and day[2]["id"] == day_close
              and snapshot.get("positions") == [{"instrument_id": "SPY", "quantity": HELD, "mark": MARK,
                                                 "mark_set": True}]
              and snapshot.get("market_open") is False and snapshot.get("unreadable") == [],
              "the day's stream reached the engine as its broker snapshot, its bar, then its close")
        committed = engine.decisions_for(day_close)
        _, contract, order = gateway.placed[0] if gateway.placed else (None, None, None)
        check(written["placed_before"] == 0 and placed() == 1 and committed == [(RELEASED, released)]
              and not engine.withheld
              and (order.orderRef, str(order.totalQuantity), order.lmtPrice) == (
                  released["client_order_id"], str(released["quantity"]), released["limit_price"]),
              "the order went out only after the day's close, which the engine's log shows released it "
              "(its broker snapshot fresh at the close)")
        check((contract.conId, contract.secType, contract.currency, contract.exchange) == (SPY_CON_ID, "STK", "USD",
                                                                                           "SMART")
              and (order.orderType, order.tif, order.transmit, str(order.totalQuantity), order.lmtPrice)
              == ("LMT", "DAY", True, "9", 401.5)
              and order.account == account and order.orderRef == CLIENT_ORDER_ID,
              "it was a whole-share SPY DAY limit order for the account, tagged with its client order ID")
        lifecycle = engine.inputs(LIFECYCLE_TYPE)
        check(len(lifecycle) == 1 and lifecycle[0]["payload"]["status"] == "submitted"
              and lifecycle[0]["payload"]["client_order_id"] == CLIENT_ORDER_ID,
              "IBKR's acceptance reached the engine as a lifecycle input (submitted)")
        fills = engine.inputs(FILL_TYPE)
        check([f["payload"]["fill_id"] for f in fills] == ["0001f4e8.rehearsal.01.01", "0001f4e8.rehearsal.02.01"]
              and [f["payload"]["commission"] for f in fills] == [1.0, 0.35]
              and sum(f["payload"]["quantity"] for f in fills) == 9,
              "both fills reached the engine with their commissions, 9 shares in all")
        check(len(gateway.called("connect")) == 2 and placed() == 1,
              "the dropped link was reconnected and matched again, and the order was not sent again")

        before_restart = list(types())
        restart = Phase([])
        restart.actions = [(lambda: restart.steps > 20, None, "the restarted adapter to run")]
        run(restart)
        check(placed() == 1 and types() == before_restart,
              "a restart on the same files sent nothing again and recorded nothing twice")
        check(len(ids()) == len(set(ids())) and types().count(BROKER_SNAPSHOT_TYPE) == 2,
              "no stream input reached the engine twice across the reconnect and the restart")
        check(types() == [BROKER_SNAPSHOT_TYPE] + [BAR, CLOSED] * WARM_SESSIONS
              + [BROKER_SNAPSHOT_TYPE, BAR, CLOSED, LIFECYCLE_TYPE, FILL_TYPE, FILL_TYPE],
              "the engine holds the warm-up, the day's snapshot, bar and close, the acceptance and two fills, "
              "in that order")

        # The engine crashes and is resumed under a new run name; the next
        # session settles, and the data job and the adapter run on its log.
        resumed_log = work / (RUN + "-resumed.log")
        resumed = LoggedEngine.resumed(str(engine_log), str(resumed_log))
        server.engine = resumed
        following = nyse_calendar.sessions_after(session, 1)[0]
        nxt = data_job(work, config, resumed_log, settled(following))
        gateway.server_time = int(settled(following).timestamp()) + 600
        following_close = "marketdata:session-closed:{}".format(following)
        run(Phase([(lambda: following_close in [e["id"] for e in resumed.inputs()], None,
                    "the next day's stream to reach the resumed run")]), engine_log=resumed_log)
        check(nxt == streams / RUN / "boost-{}.jsonl".format(following) and sorted(os.listdir(streams)) == [RUN]
              and [e["type"] for e in resumed.inputs()] == [BROKER_SNAPSHOT_TYPE, BAR, CLOSED]
              and not set(e["id"] for e in resumed.inputs()) & set(ids()) and placed() == 1,
              "after a crash resume under a new run name, the next day's stream went from the run's own folder "
              "to the resumed run, once, and nothing earlier was sent again")
    finally:
        server.close()
        log.close()


def main(argv=None):
    account = os.environ.get("TI_REHEARSAL_ACCOUNT") or SYNTHETIC_ACCOUNT
    if not is_paper_account(account):
        print("FAIL: TI_REHEARSAL_ACCOUNT does not begin DU, so it is not an IBKR paper account")
        return 1
    # A short directory: a Unix-domain socket path is limited to about 100 bytes.
    base = "/tmp" if os.path.isdir("/tmp") and os.access("/tmp", os.W_OK) else None
    with tempfile.TemporaryDirectory(prefix="ti-rehearse-", dir=base) as work:
        try:
            rehearse(Path(work), account)
        except RehearsalFailed as failed:
            print("FAIL: {}".format(failed))
            print("adapter log:\n" + (Path(work) / "adapter.log").read_text())
            return 1
    print("rehearsal passed: nothing was sent to IBKR")
    return 0


if __name__ == "__main__":
    sys.exit(main())
