"""One engine's connection to IB Gateway: connect, reconnect with backoff, and notice a lost link.

ADR 0025: each engine connects with its own client ID, and on every start and
reconnect the account's open orders, executions, positions and cash are read
and matched before any order is accepted. ADR 0019 (amended 2026-09-24): a
heartbeat watched from outside the system catches a wedged adapter; the
ConnectionStatus records reported here are what that heartbeat is built from.
"""
import logging
import time
from collections import deque, namedtuple
from datetime import datetime, timezone

from tws import Inbox

log = logging.getLogger("ibkr")

# IBKR TWS API message codes this module acts on ("Message Codes" in IBKR's
# TWS API documentation).
CODE_CLIENT_ID_IN_USE = 326
CODE_CONNECT_FAIL = 502
CODE_NOT_CONNECTED = 504
CODE_SOCKET_PORT_RESET = 1300
CODE_CONNECTIVITY_LOST = 1100
CODE_RESTORED_DATA_LOST = 1101
CODE_RESTORED_DATA_MAINTAINED = 1102
CODE_SERVER_LINK_BROKEN = 2110
# 2100 and above are warnings and notices (market-data farm status and the
# like); none of them ends a connection attempt.
FIRST_NOTICE_CODE = 2100

ConnectionStatus = namedtuple(
    "ConnectionStatus", "state reason client_id at attempt ibkr_code data_lost")
"""One change in the link's state, for the heartbeat and alerts.

state is one of: connected, disconnected, connectivity-lost (Gateway is up
but has lost IBKR's servers), refused, reconnecting, gave-up. at is the
adapter's own UTC wall-clock time; data_lost is set only on a restoration.
"""


class ConnectFailed(Exception):
    """IB Gateway could not be reached or did not complete the handshake."""


class ClientIdInUse(ConnectFailed):
    """Another connection holds this engine's client ID (IBKR code 326).

    Never retried and never worked around with another ID: the ID is this
    engine's identity (ADR 0025 point 3), and a second holder means either a
    second copy of the engine or a misconfiguration, both a person's call.
    """


class NotConnected(Exception):
    """A request was attempted while the link is down."""


def _utc_now():
    return datetime.now(timezone.utc)


class Connection:
    """The adapter's single link to IB Gateway, consumed on one thread.

    pump() takes the next callback from the Inbox, applies its effect on the
    link's state, then hands it to every subscriber, so an order report that
    arrives during an account read is never lost.

    Invariant: needs_reconciliation is set on every connect and every
    restoration, and only a caller that has matched IBKR's orders, executions
    and balances to the journal may clear it (ADR 0025 point 5).

    The adapter's loop calls maintain() regularly; it is the one place a lost
    link is brought back.
    """

    def __init__(self, api, config, status_sink, sleep=time.sleep, monotonic=time.monotonic,
                 wall_clock=_utc_now):
        self.api = api
        self.config = config
        self._status_sink = status_sink
        self._sleep = sleep
        self._monotonic = monotonic
        self._wall_clock = wall_clock
        self._subscribers = []
        self._pending = deque()
        self.inbox = Inbox()
        self.connected = False
        self.needs_reconciliation = True
        self.next_order_id = None
        self.last_server_time = None
        self._probe_sent_at = None
        self._last_probe_at = None
        # Monotonic time Gateway reported losing IBKR's servers (1100/2110);
        # None while it has not, or once it is restored or the link is dropped.
        self._lost_at = None
        self._next_req_id = 1
        self.time_requests = 0
        self.time_answers = 0
        # Set by a restoration that found server-time requests unanswered:
        # how many, and when it happened. See _write_off_lost_time_answers.
        self._time_unanswered_at_restore = 0
        self._time_restored_at = None

    # -- state reporting -------------------------------------------------
    def _report(self, state, reason="", attempt=None, ibkr_code=None, data_lost=None):
        status = ConnectionStatus(state, reason, self.config.client_id, self._wall_clock(),
                                  attempt, ibkr_code, data_lost)
        log.info("ibkr link state=%s reason=%s client_id=%d attempt=%s code=%s", state, reason,
                 self.config.client_id, attempt, ibkr_code)
        self._status_sink(status)

    def subscribe(self, handler):
        """Register handler(event) to receive every callback pump() takes."""
        self._subscribers.append(handler)

    def require_connected(self):
        if not self.connected:
            raise NotConnected("IB Gateway link is down")

    def mark_reconciled(self):
        self.needs_reconciliation = False

    def now_utc(self):
        """The adapter's own wall-clock time, for receipt stamps; never IBKR's."""
        return self._wall_clock()

    def request_server_time(self):
        """Ask IBKR for its server time; return the ticket its answer will carry.

        IBKR answers reqCurrentTime in request order, so the answer to ticket n
        is the n-th current_time callback since connect: once time_answers
        reaches n, that callback is this request's answer, not an earlier one.
        """
        self._write_off_lost_time_answers()
        self.time_requests += 1
        self.api.req_current_time()
        return self.time_requests

    def _write_off_lost_time_answers(self):
        """Count answers lost in an outage as given, once they are too late to be expected.

        An outage (1100/2110) can swallow a reqCurrentTime answer, which would
        leave every later ticket one answer short for good. Within
        request_timeout of the restoration an old answer may still arrive,
        and counting stays exact, so no old answer is read as fresh. After
        that, the answers still outstanding, up to the number unanswered at
        the restoration, are presumed lost and counted as given. Answers
        alone cannot show whether an old one arrived (a fresh answer fills
        the lost one's place in the count), so this waits for the deadline
        rather than for the count.
        """
        if self._time_restored_at is None:
            return
        if self._monotonic() - self._time_restored_at < self.config.request_timeout:
            return
        outstanding = self.time_requests - self.time_answers
        self.time_answers += min(self._time_unanswered_at_restore, outstanding)
        self._time_unanswered_at_restore = 0
        self._time_restored_at = None

    def new_req_id(self):
        req_id = self._next_req_id
        self._next_req_id += 1
        return req_id

    # -- connecting ------------------------------------------------------
    def connect(self):
        """Open the link and wait for IBKR's first valid order ID, which marks it ready."""
        cfg = self.config
        log.info("ibkr connecting host=%s port=%d client_id=%d", cfg.host, cfg.port, cfg.client_id)
        # A fresh Inbox per attempt: a callback from an abandoned session can
        # never be read as part of this one.
        self.inbox = Inbox()
        self._pending.clear()
        self._probe_sent_at = None
        self._last_probe_at = None
        self._lost_at = None
        self.time_requests = 0
        self.time_answers = 0
        self._time_unanswered_at_restore = 0
        self._time_restored_at = None
        # One deadline covers the socket connect, IBKR's handshake and the
        # wait for nextValidId: EClient.connect is synchronous, so a deadline
        # started after it returns would not bound a stalled socket.
        deadline = self._monotonic() + cfg.connect_timeout
        try:
            self.api.connect(cfg.host, cfg.port, cfg.client_id, self.inbox, cfg.connect_timeout)
        except (ConnectFailed, OSError) as err:
            self._abandon()
            raise ConnectFailed("socket connect failed: {}".format(err)) from None
        while True:
            remaining = deadline - self._monotonic()
            event = self.inbox.get(remaining) if remaining > 0 else None
            if event is None:
                self._abandon()
                raise ConnectFailed("no handshake within connect_timeout")
            if event.name == "next_valid_id":
                self.next_order_id = event.args[0]
                self.connected = True
                self.needs_reconciliation = True
                self._report("connected")
                return
            if event.name == "connection_closed":
                self._abandon()
                raise ConnectFailed("Gateway closed the connection during the handshake")
            if event.name == "error":
                code = event.args[1]
                if code == CODE_CLIENT_ID_IN_USE:
                    self._abandon()
                    self._report("refused", "client-id-in-use", ibkr_code=code)
                    raise ClientIdInUse("client_id {} is already connected".format(cfg.client_id))
                if code < FIRST_NOTICE_CODE:
                    self._abandon()
                    raise ConnectFailed("IBKR code {} during the handshake".format(code))
            # Anything else (notices, the open orders Gateway volunteers on
            # connect) is kept for pump() rather than dropped.
            self._pending.append(event)

    def connect_with_backoff(self):
        """Connect, retrying a failed attempt after an exponentially growing, capped delay."""
        cfg = self.config
        delay = cfg.backoff_initial
        last = None
        for attempt in range(1, cfg.max_connect_attempts + 1):
            if attempt > 1:
                self._report("reconnecting", "retry", attempt=attempt)
                self._sleep(delay)
                delay = min(delay * 2, cfg.backoff_max)
            try:
                self.connect()
                return
            except ClientIdInUse:
                raise
            except ConnectFailed as err:
                last = err
                log.info("ibkr connect attempt=%d failed: %s", attempt, err)
        self._report("gave-up", str(last), attempt=cfg.max_connect_attempts)
        raise ConnectFailed("gave up after {} attempts: {}".format(cfg.max_connect_attempts, last))

    def reconnect(self):
        """Drop whatever is left of the old link and connect again with the same client ID."""
        self._abandon()
        self.connect_with_backoff()

    def maintain(self):
        """Keep the link up: probe its heartbeat, and reconnect it once it is lost.

        While Gateway reports it has lost IBKR's servers (1100/2110), Gateway
        reconnects to them itself, so the adapter waits for its restoration
        (1101/1102) up to restore_timeout, then drops the link and
        reconnects. Any other lost link is reconnected at once, with
        backoff. ClientIdInUse and a give-up propagate: both are a person's
        call (ADR 0025 point 3).
        """
        if self.connected:
            self.check_heartbeat()
        if self._lost_at is not None:
            if self._monotonic() - self._lost_at < self.config.restore_timeout:
                return
            self._report("disconnected", "connectivity-not-restored")
            self.reconnect()
            return
        if not self.connected:
            self.reconnect()

    def _abandon(self):
        self.connected = False
        self._lost_at = None
        try:
            self.api.disconnect()
        except Exception as err:  # the link is being discarded either way
            log.info("ibkr disconnect during cleanup failed: %s", type(err).__name__)

    # -- consuming callbacks ---------------------------------------------
    def pump(self, timeout):
        """Take one callback (waiting up to timeout seconds), apply it, and fan it out; None if none."""
        event = self._pending.popleft() if self._pending else self.inbox.get(timeout)
        if event is None:
            return None
        self._apply(event)
        for handler in self._subscribers:
            handler(event)
        return event

    def events(self, timeout):
        """Yield callbacks until timeout seconds have passed with the stream not yet finished."""
        deadline = self._monotonic() + timeout
        while True:
            remaining = deadline - self._monotonic()
            if remaining <= 0:
                return
            event = self.pump(remaining)
            if event is None:
                return
            yield event

    def _apply(self, event):
        if event.name == "connection_closed":
            if self.connected or self._lost_at is not None:
                self.connected = False
                self._lost_at = None
                self._report("disconnected", "connection-closed")
        elif event.name == "current_time":
            self.time_answers += 1
            self.last_server_time = event.args[0]
            self._probe_sent_at = None
        elif event.name == "error":
            self._apply_error(event.args[1])

    def _apply_error(self, code):
        if code in (CODE_CONNECTIVITY_LOST, CODE_SERVER_LINK_BROKEN):
            self.connected = False
            if self._lost_at is None:
                self._lost_at = self._monotonic()
            self._report("connectivity-lost", "gateway-lost-ibkr", ibkr_code=code)
        elif code in (CODE_RESTORED_DATA_LOST, CODE_RESTORED_DATA_MAINTAINED):
            self.connected = True
            self.needs_reconciliation = True
            self._lost_at = None
            # A probe sent before the outage has a deadline that ran out
            # during it; the restored link gets a fresh probe instead.
            self._probe_sent_at = None
            self._last_probe_at = None
            if self.time_answers < self.time_requests:
                self._time_unanswered_at_restore = self.time_requests - self.time_answers
                self._time_restored_at = self._monotonic()
            self._report("connected", "restored", ibkr_code=code,
                         data_lost=code == CODE_RESTORED_DATA_LOST)
        elif code in (CODE_NOT_CONNECTED, CODE_SOCKET_PORT_RESET):
            if self.connected or self._lost_at is not None:
                self.connected = False
                self._lost_at = None
                self._report("disconnected", "not-connected", ibkr_code=code)

    # -- heartbeat -------------------------------------------------------
    def check_heartbeat(self):
        """Probe IBKR's server time on its interval; an unanswered probe past its deadline is a disconnect.

        Call it regularly from the adapter's loop. The answer is applied by
        pump(), which records last_server_time.
        """
        if not self.connected:
            return
        cfg = self.config
        now = self._monotonic()
        if self._probe_sent_at is not None:
            if now - self._probe_sent_at >= cfg.heartbeat_timeout:
                self._abandon()
                self._report("disconnected", "heartbeat-timeout")
            return
        if self._last_probe_at is None or now - self._last_probe_at >= cfg.heartbeat_interval:
            self._probe_sent_at = now
            self._last_probe_at = now
            self.request_server_time()
