"""The paper rehearsal: synthetic paper days, end to end, with the real engine, against the fake gateway.

Run by deploy/rehearse-paper.sh before the first real paper order
(docs/runbooks/first-paper-order.md, B3; docs/manual/paper-trading.md, "A
paper day"). It runs the data job (adapter/marketdata/tidata.py) on
synthetic Massive answers, the real engine (cmd/engine, Boost, in paper
mode, built here or taken from TI_REHEARSAL_ENGINE) on a Unix-domain socket,
and `python3 -m adapter.ibkr run` (cli.main) exactly as the server's unit
does, with --streams, on a paper configuration with sending switched on and
a paper-shaped account read from a secrets file. Only Massive and IB
Gateway are replaced: Massive by a synthetic client, IB Gateway by
tests/fake_tws.py. Nothing reaches Massive or IBKR.

It proves, in order, and stops at the first that fails, exiting 1:

1. the data job writes the run's warm-up, stamped with the run's identity
   from the engine's own durable log, and the adapter, started the evening
   before the first paper day, delivers it behind one account reading as of
   that evening's close, with the start-of-run reconciliation, which the
   engine matches;
2. the day's stream reaches the engine as the owner decided on 2026-10-18:
   the account snapshot and the broker snapshot as of the day's close, read
   after it (nothing working at IBKR, no execution since), then the bar,
   the mark from the bar's official close (IBKR's last price beside it),
   then the close; the engine decides, and its durable log shows the
   release of one order;
3. that order goes out once, only after the release, as released: a
   whole-share SPY DAY limit order for the account, tagged with its client
   order ID; IBKR's acceptance reaches the engine as a lifecycle input;
4. a dropped link is reconnected and reconciled again, and a restart of
   the adapter on the same files reconciles and sends nothing again; the
   engine matches both reconciliations;
5. the next day the order fills after the close: the adapter journals both
   fills, then finds an execution since the close, so it stamps that day's
   reading at its true read time and says why; the engine withholds the
   session as stale-account-snapshot, ordering nothing;
6. a crash of the engine, resumed under a new run name (a new TI_RUN,
   engine -resume: the new log holds the crashed run's records, so the
   run's identity carries over, ADR 0027): the data job writes the next
   day's stream into the run's own folder, the adapter, started on the new
   log, reconciles and delivers it, once, as of its close, and the resumed
   engine decides it; nothing earlier is sent again;
7. a holding that changed with no order to explain it (a trade by hand) is
   found at the next start's reconciliation, and the engine leaves Normal
   (ADR 0019: Degraded on the instrument) and journals why.

ibapi is not installed here, so the imported ibapi's version is given as
the pinned one; everything else the start command checks is checked. The
days are the latest NYSE sessions on or before today (New York) and the two
after, and the adapter is told each one's date as it runs.

The account is synthetic (DU0000001) unless TI_REHEARSAL_ACCOUNT holds one
beginning DU (deploy/rehearse-paper.sh asks for it at a hidden prompt); it
is never printed.
"""
import atexit
import contextlib
import datetime as dt
import io
import json
import os
import platform
import shutil
import signal
import subprocess
import sys
import tempfile
import time
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

from book_basis import cash_change  # noqa: E402
from cli import main as adapter_main  # noqa: E402
from config import is_paper_account  # noqa: E402
from fake_tws import FakeTWS, contract as fake_contract  # noqa: E402
from ibapi_pin import _archive_pin, read_pin  # noqa: E402
from marketdata import nyse_calendar, tidata  # noqa: E402
from relay import RELEASED  # noqa: E402
from reports import FILL_TYPE, LIFECYCLE_TYPE  # noqa: E402
from snapshot import ACCOUNT_SNAPSHOT_TYPE, BROKER_SNAPSHOT_TYPE, MARK_TYPE, RECONCILIATION_TYPE  # noqa: E402

NEW_YORK = ZoneInfo("America/New_York")
SYNTHETIC_ACCOUNT = "DU0000001"
SPY_CON_ID = 756733
# The most loop steps one phase may take before the rehearsal calls it stuck.
MAX_STEPS = 600
BAR = "market.bar.completed"
CLOSED = "market.session.closed"
DECIDED = "strategy.targets.decided"
RESULT = "risk.reconciliation.result"
STATE = "strategy.engine.state"
RUN = "run-rehearsal"
# The warm-up's sessions: the shortest history the configuration below
# accepts (its longest indicator window, five sessions) and one more.
WARM_SESSIONS = 6
# The paper account's synthetic holding before the first day, its net
# liquidation, and IBKR's synthetic last price for SPY (the price band's comparison).
HELD = 10
BEFORE_APPROVAL = 1  # shares of the order struck before the approval
# The inputs the engine records before the adapter's first: its
# configuration, then a first run's approval.
HEAD = 2
CONFIGURATION_TYPE = "strategy.target-weight.configuration"
RUN_APPROVED_TYPE = "operator.run.approved"
NET_LIQUIDATION = "1000000.00"
IBKR_LAST = 401.17
# A synthetic stand-in for the Massive key: not a credential.
SYNTHETIC_KEY = "synthetic-rehearsal-key"
# How long the engine may take to build, listen and stop, in seconds.
ENGINE_TIMEOUT = float(os.environ.get("TI_TEST_ENGINE_TIMEOUT", "120"))
GO_TIMEOUT = float(os.environ.get("TI_TEST_GO_TIMEOUT", "300"))
# Set by the rehearsal's own test: the day's stream is altered after the data
# job wrote it, which the adapter must refuse.
STREAM_TAMPER = False
# Set by the suite (test_rehearsal): the first run's approval states fewer
# shares than the account holds, a change the owner did not check, so the
# first reading is never taken as the book and the real engine halts.
BOOK_MISMATCH = False


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


def official_close(day):
    """The day's synthetic official close, as SyntheticMassive answers it."""
    return 400.0 + day.toordinal() % 7


def boost_configuration(warm_up_start, account):
    """The shipped Boost configuration in paper for account, with indicators short enough for a few sessions' warm-up."""
    cfg = json.loads((repo / "deploy" / "etc" / "engines" / "boost.config").read_text())
    cfg["mode"] = "paper"
    cfg["account_id"] = account
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


def stamp(at):
    return at.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


class RehearsalFailed(Exception):
    """One of the rehearsal's checks failed."""


def check(ok, what):
    if not ok:
        raise RehearsalFailed(what)
    print("PASS: " + what)


_BUILT = {}


def engine_binary():
    """The engine to rehearse with: TI_REHEARSAL_ENGINE (deploy/rehearse-paper.sh builds it with make engine), else
    built here once per process, stamped "rehearsal" (paper mode refuses an unstamped build)."""
    given = os.environ.get("TI_REHEARSAL_ENGINE")
    if given:
        return given
    if "path" in _BUILT:
        return _BUILT["path"]
    folder = tempfile.mkdtemp(prefix="ti-rehearse-engine-")
    atexit.register(shutil.rmtree, folder, True)
    binary = os.path.join(folder, "ti-engine")
    try:
        subprocess.run(["go", "build", "-ldflags",
                        "-X github.com/richard-whittemore/TrendInvesting/internal/buildinfo.Version=rehearsal",
                        "-o", binary, "./cmd/engine"], cwd=str(repo), env=dict(os.environ, GOTOOLCHAIN="local"),
                       timeout=GO_TIMEOUT, capture_output=True, text=True, check=True)
    except FileNotFoundError:
        raise RehearsalFailed("go is not on PATH: the rehearsal builds the real engine, or set "
                              "TI_REHEARSAL_ENGINE to a built one") from None
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as err:
        raise RehearsalFailed("the engine did not build: {}".format(getattr(err, "stderr", err))) from None
    _BUILT["path"] = binary
    return binary


class Engine:
    """The real engine (cmd/engine) in paper mode on a Unix-domain socket, one process per run."""

    def __init__(self, binary, work, config, as_of, socket_path):
        self.binary, self.work, self.config, self.as_of, self.socket = binary, work, config, as_of, socket_path
        self.process = None

    def start(self, name, *extra):
        out = self.work / (name + ".out")
        with open(out, "w") as sink:
            self.process = subprocess.Popen(
                [self.binary, "-mode", "paper", "-config", str(self.config), "-as-of", self.as_of,
                 "-socket", self.socket, "-log", str(self.work / "engines" / (name + ".log")),
                 "-out", str(self.work / (name + ".journal"))] + list(extra),
                stdout=sink, stderr=subprocess.STDOUT)
        deadline = time.monotonic() + ENGINE_TIMEOUT
        while time.monotonic() < deadline:
            if "listening on" in out.read_text():
                return self.work / "engines" / (name + ".log")
            if self.process.poll() is not None:
                raise RehearsalFailed("the engine stopped at its start: {}".format(out.read_text().strip()))
            time.sleep(0.05)
        raise RehearsalFailed("the engine did not listen within {} s".format(ENGINE_TIMEOUT))

    def stop(self, sig=signal.SIGTERM):
        if self.process is None:
            return
        if self.process.poll() is None:
            self.process.send_signal(sig)
        try:
            self.process.wait(timeout=ENGINE_TIMEOUT)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait()
        self.process = None


def log_groups(path):
    """The engine's durable log at path as its committed groups: [(input envelope, [decision envelopes])]."""
    groups, group = [], []
    for line in Path(path).read_text(encoding="utf-8").splitlines()[1:]:
        entry = json.loads(line)
        if "record" in entry:
            group.append(entry["record"]["envelope"])
        else:
            groups.append((group[0], group[1:]))
            group = []
    return groups


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
    second, third = nyse_calendar.sessions_after(session, 2)
    day_close = "marketdata:session-closed:{}".format(session)
    close_at = nyse_calendar.session_close(session)

    cfg = boost_configuration(warm_up_start, account)
    config = work / "boost.config"
    config.write_text(json.dumps(cfg))
    (work / "secrets.env").write_text("MASSIVE_API_KEY={}\n".format(SYNTHETIC_KEY))
    (work / "engines").mkdir()
    sock_path = str(work / "engine.sock")
    # The first paper day is the first session decided: the warm-up's
    # sessions only feed the indicators (the engine's -as-of).
    engine = Engine(engine_binary(), work, config, stamp(close_at), sock_path)
    streams = work / "data" / "streams" / "boost"
    pending = work / "adapter" / "pending.json"
    pending.parent.mkdir()
    secrets = work / "boost.env"
    secrets.write_text("IBKR_ACCOUNT_BOOST={}\n".format(account))
    os.chmod(str(secrets), 0o600)
    adapter_config = work / "boost.json"
    adapter_config.write_text(json.dumps({
        "host": "127.0.0.1", "port": 4002, "client_id": 11, "mode": "paper", "paper_sending_enabled": True,
        "connect_timeout": 2.0, "request_timeout": 0.2, "mark_timeout": 0.2, "heartbeat_interval": 30.0,
        "heartbeat_timeout": 10.0, "restore_timeout": 60.0, "backoff_initial": 0.01, "backoff_max": 0.05,
        "max_connect_attempts": 3, "commission_wait": 30.0, "con_ids": {"SPY": SPY_CON_ID}}, indent=2))
    versions = repo / "deploy" / "versions.env"
    version, sha, protobuf, protobuf_sha = read_pin(str(versions))
    record = work / "INSTALLED"
    # This rehearsal runs on the owner's machine: the record names the pinned file that machine installs from.
    arm64 = platform.machine().lower() in ("arm64", "aarch64")
    archive_sha = _archive_pin(str(versions)) if arm64 else None
    record.write_text("TWSAPI_VERSION={}\nTWSAPI_SHA256={}\nTWSAPI_ARTEFACT={}\nPROTOBUF_VERSION={}\nPROTOBUF_SHA256={}\n".format(
        version, archive_sha or sha, "archive" if arm64 else "installer", protobuf, protobuf_sha))

    gateway = FakeTWS()
    gateway.managed_accounts = (account,)
    gateway.summary = [(account, "NetLiquidation", NET_LIQUIDATION, "USD"),
                       (account, "TotalCashValue", NET_LIQUIDATION, "USD"),
                       (account, "ExcessLiquidity", NET_LIQUIDATION, "USD")]
    gateway.positions = [(account, fake_contract(), Decimal(HELD), 395.0)]
    gateway.marks = {SPY_CON_ID: IBKR_LAST}
    gateway.on_place = gateway.accept
    calendar = {"day": eve}

    def evening(day):
        """IBKR's clock and the adapter's session date: the evening of day, after the data job has run."""
        calendar["day"] = day
        gateway.server_time = int(settled(day).timestamp()) + 600

    def argv(engine_log):
        return ["run", "--config", str(adapter_config), "--pending", str(pending), "--engine-log", str(engine_log),
                "--socket", sock_path, "--streams", str(streams), "--secrets", str(secrets),
                "--account-key", "IBKR_ACCOUNT_BOOST", "--ibapi-record", str(record), "--versions", str(versions)]
    log = (work / "adapter.log").open("w")

    def run(phase, engine_log):
        status = adapter_main(argv(engine_log), api_factory=lambda cfg: gateway, should_stop=phase, err=log,
                              poll=0.01,
                              ibapi_version=lambda: tuple(int(p) for p in version.split(".")), protobuf_version=lambda: protobuf,
                              today=lambda: calendar["day"],
                              now=lambda: datetime.fromtimestamp(gateway.server_time, timezone.utc))
        log.flush()
        if status != 0:
            raise RehearsalFailed("the adapter stopped (exit {}): {}".format(
                status, (work / "adapter.log").read_text().strip().splitlines()[-1:]))

    def inputs(path):
        return [i for i, _ in log_groups(path)]

    def types(path):
        return [i["type"] for i in inputs(path)]

    def decisions(path, input_id):
        return [d for i, ds in log_groups(path) if i["id"] == input_id for d in ds]

    def of(path, event_type):
        return [i for i in inputs(path) if i["type"] == event_type]

    def placed():
        return len(gateway.placed)

    try:
        # -- 1. the warm-up, the evening before the first paper day ----------
        # The account's first run, approved with the book the owner checked
        # in IBKR: the fake gateway's holding and cash, which the first
        # reading after a close is compared with (ADR 0019 and ADR 0027,
        # amended 2026-10-18). The owner approves right after reading the
        # book, the evening before, ahead of the warm-up's reading
        # (docs/runbooks/first-paper-order.md): the approval's time is the
        # book's cutoff, so every fill after it is counted against the book.
        approval = work / "first-run-approval.json"
        approved_at = settled(eve)
        approval.write_text(json.dumps({
            "account_id": account, "previous_log": "", "previous_log_final_hash": "", "incident_id": "",
            "approved_by": "rehearsal", "approved_at": stamp(approved_at), "boost": {"latched": False},
            "incident_in_force": None,
            "starting_book": {"positions": [{"instrument_id": "SPY", "quantity": HELD - 3 if BOOK_MISMATCH else HELD}],
                              "total_cash": float(NET_LIQUIDATION), "currency": "USD"}}))
        run_log = engine.start(RUN, "-approval", str(approval))
        evening(eve)
        warm = data_job(work, config, run_log, settled(eve), "--backfill-from", warm_up_start.isoformat())
        warm_types = [ACCOUNT_SNAPSHOT_TYPE, BROKER_SNAPSHOT_TYPE, RECONCILIATION_TYPE] + [BAR, CLOSED] * (
            WARM_SESSIONS - 1) + [BAR, MARK_TYPE, CLOSED]
        if BOOK_MISMATCH:
            # A reading never checked sends its reconciliation alone, its
            # snapshots withheld (delivery._entries).
            warm_types = [RECONCILIATION_TYPE] + warm_types[3:]
        run(Phase([(lambda: len(types(run_log)) >= HEAD + len(warm_types), None, "the warm-up to reach the engine")]),
            run_log)
        check(warm == streams / RUN / "boost-{}.jsonl".format(eve),
              "the data job wrote the run's warm-up, stamped with the engine's own identity")
        if BOOK_MISMATCH:
            sent = inputs(run_log)[HEAD:]
            first = sent[0]
            outcome = decisions(run_log, first["id"])
            result = [d["payload"] for d in outcome if d["type"] == RESULT]
            state = [d["payload"] for d in outcome if d["type"] == STATE]
            check([i["type"] for i in sent] == warm_types
                  and not [i for i in sent if i["type"] in (ACCOUNT_SNAPSHOT_TYPE, BROKER_SNAPSHOT_TYPE)]
                  and result and result[0]["classification"] == "unverifiable" and result[0]["action"] == "halt"
                  and state and state[0]["state"] == "halted",
                  "a first reading that is not the owner's starting book was never taken as the book: only its "
                  "reconciliation went, no snapshot of it was committed, and the engine, holding no book, halted")
            return
        check(types(run_log)[:HEAD] == [CONFIGURATION_TYPE, RUN_APPROVED_TYPE]
              and inputs(run_log)[1]["payload"]["starting_book"]["positions"] == [
                  {"instrument_id": "SPY", "quantity": HELD}],
              "the account's first run began with the owner's approval stating its starting book")
        sent = inputs(run_log)[HEAD:]
        eve_close = stamp(nyse_calendar.session_close(eve))
        result = [d for i in sent if i["type"] == RECONCILIATION_TYPE for d in decisions(run_log, i["id"])]
        check([i["type"] for i in sent] == warm_types
              and [i["id"] for i in sent if i["source"] == "marketdata"] == [
                  json.loads(line)["id"] for line in warm.read_text().splitlines()]
              and sent[0]["payload"]["as_of"] == eve_close and sent[1]["payload"]["as_of_close"] is True
              and [d["type"] for d in result] == [RESULT] and result[0]["payload"]["action"] == "continue",
              "the warm-up reached the engine behind one reading as of that evening's close, in file order, and "
              "the engine matched the start-of-run reconciliation")

        # -- 2. and 3. the first paper day -----------------------------------
        written = {}

        def the_day():
            evening(session)
            written["day"] = data_job(work, config, run_log, settled(session))
            if STREAM_TAMPER:
                lines = written["day"].read_text().splitlines()
                lines[0] = lines[0].replace('"SPY"', '"SPX"', 1)
                os.chmod(str(written["day"]), 0o644)
                written["day"].write_text("\n".join(lines) + "\n")
            written["placed_before"] = placed()

        matched = {}

        def drop():
            matched["reconciliations"] = len(of(run_log, RECONCILIATION_TYPE))
            matched["connects"] = len(gateway.called("connect"))
            gateway.drop()

        run(Phase([
            (lambda: True, the_day, "the day's stream"),
            (lambda: LIFECYCLE_TYPE in types(run_log), drop, "the order's acceptance to reach the engine"),
            (lambda: len(gateway.called("connect")) == matched["connects"] + 1
             and len(of(run_log, RECONCILIATION_TYPE)) > matched["reconciliations"], None,
             "the reconnect to be reconciled again"),
        ]), run_log)
        # The start's and the reconnect's reconciliations go ahead of, or
        # between, the day's own inputs, as the adapter records them.
        day = [i for i in inputs(run_log)[HEAD + len(warm_types):]
               if i["type"] not in (LIFECYCLE_TYPE, RECONCILIATION_TYPE)][:5]
        account_in, broker_in = (day + [{}, {}])[:2]
        broker = broker_in.get("payload", {})
        mark = day[3]["payload"] if len(day) > 3 else {}
        check([i["type"] for i in day] == [ACCOUNT_SNAPSHOT_TYPE, BROKER_SNAPSHOT_TYPE, BAR, MARK_TYPE, CLOSED]
              and account_in["payload"]["as_of"] == stamp(close_at)
              and account_in["payload"]["positions"] == [{"instrument_id": "SPY", "quantity": HELD}]
              and (broker["as_of"], broker["as_of_close"], broker["not_as_of_close"]) == (stamp(close_at), True, "")
              and broker["read_at"] > stamp(close_at)
              and (broker["cash_drift"], broker["unexplained_cash"]) == (0, 0)
              and broker["positions"] == [{"instrument_id": "SPY", "quantity": HELD,
                                           "mark": official_close(session), "mark_set": True}]
              and (mark["price"], mark["broker_last"], mark["source"]) == (official_close(session), IBKR_LAST,
                                                                           "marketdata")
              and day[4]["id"] == day_close,
              "the day's stream reached the engine as its account and broker snapshots as of the close (read after "
              "it, its cash that of the starting book: no drift, none unexplained), its bar, its mark from the "
              "bar's official close, then its close")
        decided = decisions(run_log, day_close)
        releases = [d["payload"] for d in decided if d["type"] == RELEASED]
        targets = [d["payload"] for d in decided if d["type"] == DECIDED]
        released = releases[0] if releases else {}
        check(targets and targets[0]["outcome"] == "decided" and len(releases) == 1 and released["send"] is True
              and released["side"] == "buy" and released["account_id"] == account and released["mode"] == "paper",
              "the engine decided the day and its durable log shows one buy released to be sent")
        _, contract, order = gateway.placed[0] if gateway.placed else (None, None, None)
        check(written["placed_before"] == 0 and placed() == 1
              and (order.orderRef, str(order.totalQuantity), order.lmtPrice) == (
                  released["client_order_id"], str(released["quantity"]), released["limit_price"]),
              "the order went out once, only after the day's close released it, as released")
        check((contract.conId, contract.secType, contract.currency, contract.exchange) == (SPY_CON_ID, "STK", "USD",
                                                                                           "SMART")
              and (order.orderType, order.tif, order.transmit) == ("LMT", "DAY", True)
              and order.account == account,
              "it was a whole-share SPY DAY limit order for the account, tagged with its client order ID")
        lifecycle = of(run_log, LIFECYCLE_TYPE)
        check(len(lifecycle) == 1 and lifecycle[0]["payload"]["status"] == "submitted"
              and lifecycle[0]["payload"]["client_order_id"] == released["client_order_id"],
              "IBKR's acceptance reached the engine as a lifecycle input (submitted)")

        # -- 4. a reconnect and a restart -------------------------------------
        before_restart = [i["id"] for i in inputs(run_log)]
        connects = len(gateway.called("connect"))
        restart = Phase([])
        restart.actions = [(lambda: restart.steps > 20, None, "the restarted adapter to run")]
        run(restart, run_log)
        after = inputs(run_log)
        added = [i["type"] for i in after if i["id"] not in before_restart]
        # Five so far: the warm-up's start-of-run reconciliation, the day
        # adapter's start, the day's own batch (every later batch reconciles
        # the reading against the book before its snapshot replaces it), the
        # reconnect and the restart.
        results = [d["payload"]["action"] for i in of(run_log, RECONCILIATION_TYPE)
                   for d in decisions(run_log, i["id"]) if d["type"] == RESULT]
        check(len(gateway.called("connect")) == connects + 1 and placed() == 1 and added == [RECONCILIATION_TYPE]
              and results == ["continue"] * 5 and len({i["id"] for i in after}) == len(after),
              "the reconnect and a restart reconciled the account again, the engine matched every reconciliation "
              "(each batch's too), and nothing was sent twice")

        # -- 5. the next day: a fill after the close ------------------------
        quantity = released["quantity"]
        fill_price = official_close(session)
        stamp_day = second.strftime("%Y%m%d")
        second_close = "marketdata:session-closed:{}".format(second)

        def late_fills():
            evening(second)
            # One share was struck before the owner stated the book, which
            # already holds it: IBKR replays it among the day's executions,
            # and it is not counted again (ADR 0019: a book and the
            # movements that explain it share one cutoff).
            first = (quantity - BEFORE_APPROVAL) // 2
            gateway.fill(7, shares=str(BEFORE_APPROVAL), price=fill_price, exec_id="0001f4e8.rehearsal.00.01",
                         cum_qty=str(BEFORE_APPROVAL), commission="0.50",
                         time="{} 10:00:00 US/Eastern".format(eve.strftime("%Y%m%d")))
            gateway.fill(7, shares=str(first), price=fill_price, exec_id="0001f4e8.rehearsal.01.01",
                         cum_qty=str(BEFORE_APPROVAL + first), commission="1.00",
                         time="{} 16:30:01 US/Eastern".format(stamp_day))
            gateway.fill(7, shares=str(quantity - BEFORE_APPROVAL - first), price=fill_price,
                         exec_id="0001f4e8.rehearsal.02.01", cum_qty=str(quantity), commission="0.35",
                         time="{} 16:30:04 US/Eastern".format(stamp_day))
            gateway.positions = [(account, fake_contract(), Decimal(HELD + quantity - BEFORE_APPROVAL), 395.0)]
            written["second"] = data_job(work, config, run_log, settled(second))

        run(Phase([
            (lambda: True, late_fills, "the next day"),
            (lambda: second_close in [i["id"] for i in inputs(run_log)], None, "the next day's close"),
        ]), run_log)
        fills = of(run_log, FILL_TYPE)
        check([f["payload"]["fill_id"] for f in fills] == ["0001f4e8.rehearsal.00.01", "0001f4e8.rehearsal.01.01",
                                                         "0001f4e8.rehearsal.02.01"]
              and sum(f["payload"]["quantity"] for f in fills) == quantity,
              "all three fills reached the engine with their commissions, the whole order in all")
        late = of(run_log, BROKER_SNAPSHOT_TYPE)[-1]["payload"]
        late_account = of(run_log, ACCOUNT_SNAPSHOT_TYPE)[-1]["payload"]
        withheld = [d["payload"] for d in decisions(run_log, second_close) if d["type"] == DECIDED]
        ids = [i["id"] for i in inputs(run_log)]
        check(late["as_of_close"] is False and late["as_of"] == late["read_at"] == late_account["as_of"]
              and "0001f4e8.rehearsal.01.01" in late["not_as_of_close"]
              and "execution(s) in the account since the close" in late["not_as_of_close"]
              and max(ids.index(f["id"]) for f in fills) < ids.index(second_close)
              and withheld and withheld[0]["outcome"] == "withheld"
              and withheld[0]["withheld_reason"] == "stale-account-snapshot"
              and not [d for d in decisions(run_log, second_close) if d["type"] == RELEASED] and placed() == 1,
              "with an execution since the close, the reading was stamped at its read time with the reason, and "
              "the engine withheld the session as stale-account-snapshot, ordering nothing")

        # -- 6. a crash resume under a new run name --------------------------
        engine.stop(signal.SIGKILL)
        resumed_log = engine.start(RUN + "-resumed", "-resume", str(run_log))
        third_close = "marketdata:session-closed:{}".format(third)
        evening(third)
        nxt = data_job(work, config, resumed_log, settled(third))
        copied = len(inputs(resumed_log))
        run(Phase([(lambda: third_close in [i["id"] for i in inputs(resumed_log)], None,
                    "the next day's stream to reach the resumed run")]), resumed_log)
        new = inputs(resumed_log)[copied:]
        # The fake gateway's cash never moves, so the order's fills after
        # the approval's cutoff (not the one struck before it, which the
        # book already counts) are cash the book cannot explain: their
        # value and commissions, added up since the starting book.
        cost = -cash_change([f["payload"] for f in fills if f["payload"]["fill_id"] != "0001f4e8.rehearsal.00.01"])
        resumed_broker = new[3]["payload"] if len(new) > 3 else {}
        resumed_decided = [d["payload"] for d in decisions(resumed_log, third_close) if d["type"] == DECIDED]
        check(nxt == streams / RUN / "boost-{}.jsonl".format(third) and sorted(os.listdir(streams)) == [RUN]
              and [i["type"] for i in new] == [RECONCILIATION_TYPE, RECONCILIATION_TYPE, ACCOUNT_SNAPSHOT_TYPE,
                                               BROKER_SNAPSHOT_TYPE, BAR, MARK_TYPE, CLOSED]
              and new[2]["payload"]["as_of"] == stamp(nyse_calendar.session_close(third))
              and (resumed_broker.get("cash_drift"), resumed_broker.get("unexplained_cash")) == (0, float(cost))
              and resumed_decided and resumed_decided[0]["outcome"] == "decided"
              and len({i["id"] for i in inputs(resumed_log)}) == len(inputs(resumed_log)),
              "after a crash resume under a new run name, the next day's stream went from the run's own folder "
              "to the resumed run, once, as of its close, with the order's fills since the approval counted as "
              "cash unexplained, and the resumed engine decided it; nothing earlier was sent again")

        # -- 7. a holding no order explains --------------------------------
        gateway.positions = [(account, fake_contract(), Decimal(HELD + quantity - BEFORE_APPROVAL + 5), 395.0)]
        reconciled = len(of(resumed_log, RECONCILIATION_TYPE))
        run(Phase([(lambda: len(of(resumed_log, RECONCILIATION_TYPE)) > reconciled, None,
                    "the start's reconciliation")]), resumed_log)
        last = of(resumed_log, RECONCILIATION_TYPE)[-1]
        outcome = decisions(resumed_log, last["id"])
        result = [d["payload"] for d in outcome if d["type"] == RESULT]
        state = [d["payload"] for d in outcome if d["type"] == STATE]
        check(result and result[0]["classification"] == "unexplained" and result[0]["action"] == "degrade"
              and state and state[0]["state"] == "degraded" and "SPY" in state[0]["scope_instrument_ids"],
              "a holding no journalled order explains was found at the next start's reconciliation, and the "
              "engine left Normal: Degraded on SPY (ADR 0019)")
    finally:
        engine.stop()
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
            for out in sorted(Path(work).glob("*.out")):
                print("{}:\n{}".format(out.name, out.read_text()))
            return 1
    print("rehearsal passed: nothing was sent to IBKR")
    return 0


if __name__ == "__main__":
    sys.exit(main())
