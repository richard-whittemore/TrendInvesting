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
# The adapter's note, queued in the Inbox, that it has just asked for every
# open order (see Connection.ask_all_open_orders).
OPEN_ORDERS_ASKED = "open_orders_asked"
# Where one open-order list stands (Connection.open_order_list): callbacks
# still older than the request, the list being answered, or its end taken.
LIST_BEFORE = "before"
LIST_READING = "reading"
LIST_ENDED = "ended"
# As a session starts, Gateway sends this client's open orders and an end of
# list that nothing asked for. connect takes them before it returns, so that
# end can never land inside the first list asked for: it waits for that end,
# but no longer than VOLUNTEER_QUIET seconds with no volunteered row (or
# request_timeout, if shorter) and VOLUNTEER_MAX seconds in all.
VOLUNTEER_QUIET = 2.0
VOLUNTEER_MAX = 10.0
# The callbacks of an open-order list: the Inbox is quiet for the list once
# none of them has arrived for VOLUNTEER_QUIET seconds (see
# Connection.resync_open_orders).
LIST_MESSAGES = ("open_order", "open_order_end")

# The first request ID: IBKR answers a request's error under its request ID
# and an order's rejection under its order ID, with message codes shared by
# both, so request IDs start far above any order ID an account reaches
# (IBKR's are 32-bit) and an error on a request never names an order.
REQUEST_ID_BASE = 1 << 30

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


class AccountNotManaged(Exception):
    """The configured account is not among the accounts this session's IBKR login manages (managedAccounts)."""


class NotConnected(Exception):
    """A request was attempted while the link is down."""


def _account_list(text):
    """IBKR's managedAccounts list ("DU1,DU2,") as a set of account IDs."""
    return frozenset(a.strip() for a in str(text).split(",") if a.strip())


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
        self.managed_accounts = frozenset()
        self.last_server_time = None
        self._probe_sent_at = None
        self._last_probe_at = None
        # Monotonic time Gateway reported losing IBKR's servers (1100/2110);
        # None while it has not, or once it is restored or the link is dropped.
        self._lost_at = None
        # The reason for a drop asked for during such an outage, made once
        # Gateway restores the link (see drop).
        self._drop_when_restored = None
        self._next_req_id = REQUEST_ID_BASE
        self.time_requests = 0
        self.time_answers = 0
        # Monotonic time of a restoration that found server-time requests
        # unanswered; see _settle_time_answers.
        self._time_restored_at = None
        # Open-order lists asked for on this link, their notes taken from
        # the Inbox, and the end markers counted as their answers; see
        # ask_all_open_orders.
        self._lists_requested = 0
        self._lists_noted = 0
        self._list_ends = 0
        self._reset_list_alignment()

    def _reset_list_alignment(self):
        # Open-order rows taken while the newest noted list was outstanding;
        # the volunteered rows and end connect kept for pump(), which are
        # Gateway's own list, not strays; the count of strays (an end with
        # no list outstanding, a row with none outstanding); and whether the
        # link must be pumped quiet before the next list is asked.
        self._rows_in_list = 0
        self._volunteered = 0
        self.open_order_strays = 0
        self._resync_due = False

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
        """Record that IBKR's orders, executions and balances were matched; refused for an unmanaged account.

        Matching the journal against a login that does not trade the
        configured account proves nothing about that account, so the link
        stays unreconciled (fail closed).
        """
        if self.config.account_id not in self.managed_accounts:
            raise AccountNotManaged("account {!r} is not among the accounts this IBKR login manages ({}); the "
                                    "link is not reconciled".format(self.config.account_id,
                                                                    ", ".join(sorted(self.managed_accounts)) or
                                                                    "none reported"))
        self.needs_reconciliation = False

    def now_utc(self):
        """The adapter's own wall-clock time, for receipt stamps; never IBKR's."""
        return self._wall_clock()

    def request_server_time(self):
        """Ask IBKR for its server time; return the ticket its answer will carry.

        IBKR answers reqCurrentTime in request order, so the answer to ticket n
        is the n-th current_time callback since connect: once time_answers
        reaches n, that callback is this request's answer, not an earlier one.
        Raises NotConnected if the link had to be dropped because that count
        could no longer be trusted (see _settle_time_answers).
        """
        if not self._settle_time_answers():
            raise NotConnected("server-time answers could not be matched to requests")
        self.time_requests += 1
        self.api.req_current_time()
        return self.time_requests

    def _settle_time_answers(self):
        """Drop the link once an outage leaves server-time answers unmatchable; False if dropped.

        An outage (1100/2110) can swallow a reqCurrentTime answer, or only
        delay it, and IBKR's answers carry no request ID. Within
        request_timeout of the restoration an old answer may still arrive
        and the count stays exact, so a read fails closed rather than taking
        an old answer as fresh. Once that deadline has passed with any
        request still unanswered, a lost answer and a late one can no longer
        be told apart (a fresh answer fills the lost one's place in the
        count), so the link is dropped: the reconnected session has a new
        Inbox, which no old answer can reach, and counts from zero.
        """
        if self._time_restored_at is None:
            return True
        if self._monotonic() - self._time_restored_at < self.config.request_timeout:
            return True
        self._time_restored_at = None
        if self.time_answers >= self.time_requests:
            return True
        self._abandon()
        self._report("disconnected", "time-answers-unmatched")
        return False

    def ask_all_open_orders(self):
        """Ask IBKR for every open order (reqAllOpenOrders); return the ticket open_order_list reads it by.

        IBKR's open orders and the end of their list carry no request ID,
        so at most one list is outstanding on the link at a time: while an
        earlier list has not ended, this waits for its end (taking its late
        rows as that list's, never this one's) up to request_timeout, and
        if it does not come drops the link and raises NotConnected, since no
        later list on the link could be told from it (ADR 0019: a list not
        read in full is unread). Gateway's volunteered list was taken by
        connect. After an end with no list outstanding, a list that ended
        before any row of its own (the shape of a list ended by an end not
        its own), or a connect that saw no volunteered end, the link is
        pumped quiet first (resync_open_orders), so a link one list behind
        is brought level before it is read again. The request is noted in the Inbox just before it is sent:
        every callback queued ahead of the note arrived before the request
        left and is no part of its answer, and an end taken with no list
        outstanding ends nothing. NotConnected while the link is down.
        """
        self.require_connected()
        if self._list_ends < self._lists_requested:
            self._await_list_end()
        if self._resync_due:
            self.resync_open_orders()
        self._lists_requested += 1
        self.inbox.note(OPEN_ORDERS_ASKED)
        self.api.req_all_open_orders()
        return self._lists_requested

    def _await_list_end(self):
        """Take callbacks until the outstanding open-order list ends; drop the link if it does not in request_timeout."""
        for _ in self.events(self.config.request_timeout):
            if not self.connected or self._list_ends >= self._lists_requested:
                break
        if not self.connected:
            raise NotConnected("IB Gateway link is down")
        if self._list_ends < self._lists_requested:
            self.drop("open-order-list-unended")
            raise NotConnected("an earlier open-order list never ended, so no later one on this link can be read")

    def resync_open_orders(self):
        """Take callbacks until no open-order row or end has arrived for VOLUNTEER_QUIET seconds (or request_timeout).

        Waits first for any list still outstanding to end (as
        ask_all_open_orders does). An end not a list's own that landed
        inside a list leaves the link one list behind, each list ending on
        the one before's end; once the Inbox is quiet, every late row and
        end has been taken with no list outstanding, counted as strays
        (open_order_strays), and the next list is answered by its own end
        (ADR 0025 point 5: matched in full before anything is sent). A link
        never quiet within VOLUNTEER_MAX seconds is dropped (ADR 0019: a
        list not read in full is unread). NotConnected while the link is
        down or once dropped.
        """
        self.require_connected()
        if self._list_ends < self._lists_requested:
            self._await_list_end()
        quiet = min(VOLUNTEER_QUIET, self.config.request_timeout)
        now = self._monotonic()
        until, quiet_until = now + VOLUNTEER_MAX, now + quiet
        while True:
            now = self._monotonic()
            if now >= quiet_until:
                break
            if now >= until:
                self.drop("open-orders-never-quiet")
                raise NotConnected("IBKR's open orders never stopped arriving, so no list on this link can be read")
            event = self.pump(min(until, quiet_until) - now)
            if not self.connected:
                raise NotConnected("IB Gateway link is down")
            if event is None:
                break
            if event.name in LIST_MESSAGES:
                quiet_until = self._monotonic() + quiet
        self._resync_due = False

    def open_order_list(self, ticket):
        """Where the list ask_all_open_orders gave ticket for stands: LIST_BEFORE, LIST_READING or LIST_ENDED.

        Read after each callback pump() takes: an open order taken while
        the list is LIST_READING is part of its answer.
        """
        if self._lists_noted < ticket:
            return LIST_BEFORE
        if self._list_ends >= ticket:
            return LIST_ENDED
        return LIST_READING

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
        self.managed_accounts = frozenset()
        self._probe_sent_at = None
        self._last_probe_at = None
        self._lost_at = None
        # A drop deferred on the old link belongs to it: the new link starts
        # with none, whichever path led here.
        self._drop_when_restored = None
        self.time_requests = 0
        self.time_answers = 0
        self._time_restored_at = None
        self._lists_requested = 0
        self._lists_noted = 0
        self._list_ends = 0
        self._reset_list_alignment()
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
                self._take_volunteered_list()
                self.connected = True
                self.needs_reconciliation = True
                self._report("connected")
                return
            if event.name == "managed_accounts":
                # The session's own state, read here: nothing else needs it.
                self._apply(event)
                continue
            self._fail_handshake(event)
            # Anything else (notices, the open orders Gateway volunteers on
            # connect) is kept for pump() rather than dropped.
            self._keep(event)

    def _fail_handshake(self, event):
        """Abandon the attempt (ConnectFailed, or ClientIdInUse) on a close or a code that ends a connect."""
        if event.name == "connection_closed":
            self._abandon()
            raise ConnectFailed("Gateway closed the connection during the handshake")
        if event.name == "error":
            code = event.args[1]
            if code == CODE_CLIENT_ID_IN_USE:
                self._abandon()
                self._report("refused", "client-id-in-use", ibkr_code=code)
                raise ClientIdInUse("client_id {} is already connected".format(self.config.client_id))
            if code < FIRST_NOTICE_CODE:
                self._abandon()
                raise ConnectFailed("IBKR code {} during the handshake".format(code))

    def _keep(self, event):
        """Keep a callback connect took for pump(); a row or end of Gateway's volunteered list is no stray."""
        if event.name in LIST_MESSAGES:
            self._volunteered += 1
        self._pending.append(event)

    def _take_volunteered_list(self):
        """Keep for pump() the open orders and end Gateway volunteers as the session starts (see VOLUNTEER_QUIET).

        Taken before the first list is asked for, they stand ahead of its
        note in the queue, so the volunteered end ends nothing. When no end
        comes in time the session goes on without it; a start's or
        reconnect's read of the open orders stands only once two reads in a
        row agree (orders.collect_settled_broker_orders), and the first list
        asked is pumped quiet first (resync_open_orders). A close or a code
        that ends a connect, taken here, fails the attempt as it does
        during the handshake, so connect_with_backoff retries it: a link
        already lost is never reported connected.
        """
        quiet = min(VOLUNTEER_QUIET, self.config.request_timeout)
        now = self._monotonic()
        until, quiet_until = now + VOLUNTEER_MAX, now + quiet
        while True:
            wait = min(until, quiet_until) - self._monotonic()
            event = self.inbox.get(wait) if wait > 0 else None
            if event is None:
                self._resync_due = True
                return
            self._fail_handshake(event)
            self._keep(event)
            if event.name == "open_order_end":
                return
            if event.name in ("open_order", "order_status"):
                quiet_until = self._monotonic() + quiet

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

    def drop(self, reason):
        """Drop the link on purpose; maintain() then connects again, and the new link needs reconciling.

        For a link on which answers that carry no request ID can no longer
        be matched to their requests: the reconnected session has a new
        Inbox, which no answer of the old one can reach. While Gateway has
        lost IBKR's servers (1100/2110) the drop waits for the restoration
        (1101/1102) or restore_timeout, as maintain() does, so an outage
        Gateway restores in time does not use up the connect attempts; the
        restored socket is then dropped, never read on.
        """
        if self._lost_at is not None:
            self._drop_when_restored = reason
            return
        self._abandon()
        self._report("disconnected", reason)

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
        self._drop_when_restored = None
        try:
            self.api.disconnect()
        except Exception as err:  # the link is being discarded either way
            log.info("ibkr disconnect during cleanup failed: %s", type(err).__name__)

    # -- consuming callbacks ---------------------------------------------
    def pump(self, timeout):
        """Take one callback (waiting up to timeout seconds), apply it, and fan it out; None if none.

        The adapter's own notes (OPEN_ORDERS_ASKED) are applied and returned
        but never fanned out: they are no callback of IBKR's.
        """
        event = self._pending.popleft() if self._pending else self.inbox.get(timeout)
        if event is None:
            return None
        self._apply(event)
        if event.name == OPEN_ORDERS_ASKED:
            return event  # the adapter's own note, not IBKR's callback
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
        if event.name == "managed_accounts":
            self.managed_accounts = _account_list(event.args[0])
        elif event.name == "connection_closed":
            if self.connected or self._lost_at is not None:
                self.connected = False
                self._lost_at = None
                self._report("disconnected", "connection-closed")
        elif event.name == OPEN_ORDERS_ASKED:
            self._lists_noted += 1
            self._rows_in_list = 0
        elif event.name in LIST_MESSAGES and self._list_ends >= self._lists_noted and self._volunteered:
            self._volunteered -= 1  # Gateway's volunteered list, which connect took
        elif event.name == "open_order":
            if self._list_ends < self._lists_noted:
                self._rows_in_list += 1
            else:
                self.open_order_strays += 1
        elif event.name == "open_order_end":
            if self._list_ends < self._lists_noted:
                self._list_ends += 1
                if not self._rows_in_list:
                    self._resync_due = True
            else:
                self.open_order_strays += 1
                self._resync_due = True
                log.info("ibkr open-order list end with no list outstanding ignored")
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
            if self._drop_when_restored is not None:
                reason = self._drop_when_restored
                self._abandon()
                self._report("disconnected", reason)
                return
            self.connected = True
            self.needs_reconciliation = True
            self._lost_at = None
            # A probe sent before the outage has a deadline that ran out
            # during it; the restored link gets a fresh probe instead.
            self._probe_sent_at = None
            self._last_probe_at = None
            if self.time_answers < self.time_requests:
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
        if not self.connected or not self._settle_time_answers():
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
