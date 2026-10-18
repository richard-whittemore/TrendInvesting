"""The paper rehearsal: the adapter's start command, end to end, against the fake gateway and an engine stand-in.

Run by deploy/rehearse-paper.sh before the first real paper order
(docs/runbooks/first-paper-order.md, B3; docs/manual/paper-trading.md). It
starts `python3 -m adapter.ibkr run` (cli.main) exactly as the server's unit
does, on a paper configuration with sending switched on and a paper-shaped
account read from a secrets file, with only IB Gateway and the engine
replaced: IB Gateway by tests/fake_tws.py, and the engine by
tests/engine_double.py behind a real Unix-domain socket, so the adapter's
socket client is the one the server uses. Nothing reaches IBKR.

It proves, in order, and stops at the first that fails, exiting 1:

1. the adapter starts, reads and matches the account before anything else;
2. one released SPY order goes out once: a whole-share DAY limit order for
   the account, tagged with its client order ID;
3. IBKR's acceptance reaches the engine as a lifecycle input, and each of
   two fills as a fill input once its commission arrives;
4. a dropped link is reconnected and matched again, and the order is not
   sent again;
5. a restart of the adapter on the same files sends nothing again and
   records nothing twice.

The session's close, which a producer not yet built will send, is recorded
in the adapter's outbox before the first start, as that producer would:
through loop.Outbox.append, under the outbox's lock.

ibapi is not installed here, so the imported ibapi's version is given as
the pinned one; everything else the start command checks is checked.

The account is synthetic (DU0000001) unless TI_REHEARSAL_ACCOUNT holds one
beginning DU (deploy/rehearse-paper.sh asks for it at a hidden prompt); it
is never printed.
"""
import hashlib
import json
import os
import socket
import sys
import tempfile
import threading
from datetime import datetime, time as clock_time, timezone
from pathlib import Path
from zoneinfo import ZoneInfo

tests_dir = Path(__file__).resolve().parent
adapter_dir = tests_dir.parent
repo = adapter_dir.parents[1]
for p in (str(tests_dir), str(adapter_dir)):
    if p not in sys.path:
        sys.path.insert(0, p)

from cli import main as adapter_main  # noqa: E402
from config import is_paper_account  # noqa: E402
from engine_double import EngineRefused, LoggedEngine  # noqa: E402
from fake_tws import FakeTWS  # noqa: E402
from ibapi_pin import read_pin  # noqa: E402
from loop import OUTBOX_SUFFIX, Outbox  # noqa: E402
from relay import RELEASED  # noqa: E402
from reports import FILL_TYPE, LIFECYCLE_TYPE  # noqa: E402

NEW_YORK = ZoneInfo("America/New_York")
SYNTHETIC_ACCOUNT = "DU0000001"
CLIENT_ORDER_ID = "rehearsal-spy-1"
SPY_CON_ID = 756733
# The most loop steps one phase may take before the rehearsal calls it stuck.
MAX_STEPS = 400


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
    closed = datetime.combine(today, clock_time(16, 0), NEW_YORK).astimezone(timezone.utc)
    closed_at = closed.strftime("%Y-%m-%dT%H:%M:%SZ")
    stamp = today.strftime("%Y%m%d")
    released = {"account_id": account, "plan_id": "order-planned:" + CLIENT_ORDER_ID,
                "tag": "targets-decided:rehearsal", "client_order_id": CLIENT_ORDER_ID, "instrument_id": "SPY",
                "side": "buy", "quantity": 9, "limit_price": 401.5, "mode": "paper", "send": True}

    engine_log = work / "run-rehearsal.log"
    engine = LoggedEngine(str(engine_log), {"session:rehearsal": [{"type": RELEASED, "payload": released}]})
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
        "connect_timeout": 2.0, "request_timeout": 0.2, "heartbeat_interval": 30.0, "heartbeat_timeout": 10.0,
        "restore_timeout": 60.0, "backoff_initial": 0.01, "backoff_max": 0.05, "max_connect_attempts": 3,
        "commission_wait": 30.0, "con_ids": {"SPY": SPY_CON_ID}}, indent=2))
    versions = repo / "deploy" / "versions.env"
    version, sha = read_pin(str(versions))
    record = work / "INSTALLED"
    record.write_text("TWSAPI_VERSION={}\nTWSAPI_SHA256={}\n".format(version, sha))
    # The session's close, as the producer that sends it will record it.
    Outbox(str(pending) + OUTBOX_SUFFIX).append({
        "id": "session:rehearsal", "event_type": "market.session.closed", "schema_version": 1,
        "event_time": closed_at, "payload": {"period_end": closed_at, "instrument_ids": ["SPY"]}})

    gateway = FakeTWS()
    gateway.managed_accounts = (account,)
    gateway.summary = [(account, "NetLiquidation", "1000000.00", "USD"),
                       (account, "TotalCashValue", "1000000.00", "USD"),
                       (account, "ExcessLiquidity", "1000000.00", "USD")]
    gateway.on_place = gateway.accept
    argv = ["run", "--config", str(adapter_config), "--pending", str(pending), "--engine-log", str(engine_log),
            "--socket", sock_path, "--secrets", str(secrets), "--account-key", "IBKR_ACCOUNT_BOOST",
            "--ibapi-record", str(record), "--versions", str(versions)]
    log = (work / "adapter.log").open("w")

    def run(phase):
        status = adapter_main(argv, api_factory=lambda cfg: gateway, should_stop=phase, err=log, poll=0.01,
                              ibapi_version=lambda: tuple(int(p) for p in version.split(".")))
        log.flush()
        if status != 0:
            raise RehearsalFailed("the adapter stopped (exit {}): {}".format(
                status, (work / "adapter.log").read_text().strip().splitlines()[-1:]))

    def types():
        return [e["type"] for e in engine.inputs()]

    def placed():
        return len(gateway.placed)

    def fill():
        gateway.fill(7, shares="4", price=401.25, exec_id="0001f4e8.rehearsal.01.01", cum_qty="4", commission="1.00",
                     time="{} 09:30:01 US/Eastern".format(stamp))
        gateway.fill(7, shares="5", price=401.3, exec_id="0001f4e8.rehearsal.02.01", cum_qty="9", commission="0.35",
                     time="{} 09:30:04 US/Eastern".format(stamp))

    matched = {}

    def drop():
        matched["executions"] = len(gateway.called("req_executions"))
        gateway.drop()

    try:
        run(Phase([
            (lambda: len(types()) >= 2, fill, "the order's acceptance to reach the engine"),
            (lambda: types().count(FILL_TYPE) == 2, drop, "both fills to reach the engine"),
            (lambda: len(gateway.called("connect")) == 2
             and len(gateway.called("req_executions")) > matched["executions"], None,
             "the reconnect to be matched again"),
        ]))
        calls = [c[0] for c in gateway.calls]
        check(calls.index("req_account_summary") < calls.index("place_order")
              and calls.index("req_executions") < calls.index("place_order"),
              "the account, open orders and executions were read before the order was sent")
        check(placed() == 1, "one order was sent")
        _, contract, order = gateway.placed[0]
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

        before = list(types())
        restart = Phase([])
        restart.actions = [(lambda: restart.steps > 20, None, "the restarted adapter to run")]
        run(restart)
        check(placed() == 1 and types() == before,
              "a restart on the same files sent nothing again and recorded nothing twice")
        check(engine.inputs()[0]["id"] == "session:rehearsal" and types() == [
            "market.session.closed", LIFECYCLE_TYPE, FILL_TYPE, FILL_TYPE],
            "the engine holds the session's close, the acceptance and two fills, in that order")
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
