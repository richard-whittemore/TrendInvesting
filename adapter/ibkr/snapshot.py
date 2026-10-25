"""account.broker.snapshot, account.snapshot, market.mark.recorded and account.reconciliation payloads from one IBKR read.

ADR 0019 and its amendment of 2026-10-17 (reading 11): the broker snapshot is
IBKR's holdings with marks, net liquidation, Excess Liquidity and every open
order, taken together; the reconciliation is IBKR's positions and cash for
the engine to classify against its book. Both are recorded inputs, built
here as plain records that the transport maps to envelopes.

Every field is stated (the engine refuses a snapshot that omits positions,
open_orders or market_open, and fails closed): a list with nothing in it is
[], and a component that could not be read is named in "unreadable" and
carries no value. An unavailable balance is not zero (ADR 0019). Times are
whole seconds, as IBKR stamps them.

IBKR does not timestamp positions or balances, so as_of is IBKR's server time
asked for after every component was read; if the link failed before IBKR
could answer, it is the adapter's own receipt time, and every component is
then unreadable.

The owner's decision of 2026-10-18 (ADR 0019, ADR 0025 point 5 and ADR
0026 point 1, amended): one reading taken after a session's close and
before the next open, with no order working or pending at IBKR for the
account and no execution since the close, is the account as of that close.
From that one reading come the engine's account.snapshot (version 3,
account_snapshot_record) and the broker snapshot (version 2,
broker_snapshot_record), both stamped as of the close, the broker snapshot
stating the true read time beside it as evidence; when the reading cannot
stand for the close (the adapter checks the condition, loop.py), both are
stamped at the read time and the broker snapshot says why, so the engine
withholds the session as stale (ADR 0019's snapshot rule).

One price basis per snapshot, and per rule (ADR 0019, amended 2026-10-18).
A snapshot as of the close is valued at the close: each holding's mark is
the delivered bar's official close, the price the backtest marks at, and
its net liquidation, in both snapshots, is the total cash plus each holding
times that close (close_valuation), the basis of the sale back to 98% and
of Boost's sizing; IBKR's own net liquidation at the read is recorded
beside it (read_net_liquidation), the margin alarm's basis with Excess
Liquidity read with it. A snapshot at its read time is IBKR's own reading:
its net liquidation is IBKR's and each holding is marked at IBKR's last
price, mark_set false when IBKR gave none (ADR 0019: the sale back to 98%
is then withheld and the owner acts by hand). The market.mark.recorded
(version 2, mark_record) is the official close, with IBKR's last price
beside it, which the engine's price band compares a checked order with when
it is set (ADR 0026 point 3, amended 2026-10-18).

IBKR's last price (read_marks) is its LAST tick for the contract, from a
one-off market-data snapshot: a quote, a previous close or a delayed price
is never taken for it, and only a request that completed with no price that
is not valid gives one.
"""
import logging
import math
from collections import namedtuple
from datetime import datetime, timezone
from types import SimpleNamespace

from account import EXCESS_LIQUIDITY, NET_LIQUIDATION, SUMMARY_GROUP, SUMMARY_TAGS, TOTAL_CASH
from account import SnapshotIncomplete, SnapshotInvalid, _add_balance, _add_position, _server_time
from connection import LIST_ENDED, LIST_READING, NotConnected
from masking import mask_accounts
from orders import FINISHED, UnrecognisedOrderReport, open_order_record, order_key, order_label
from sender import CURRENCY, EXCHANGE, SEC_TYPE
from values import to_decimal

log = logging.getLogger("ibkr")

BROKER_SNAPSHOT_TYPE = "account.broker.snapshot"
# internal/event BrokerSnapshotReadAtVersion: the read time beside the stamp.
BROKER_SNAPSHOT_VERSION = 2
# internal/event AccountSnapshotPositionsVersion: what Boost reads.
ACCOUNT_SNAPSHOT_TYPE = "account.snapshot"
ACCOUNT_SNAPSHOT_VERSION = 3
# internal/event MarkRecordedEvidenceVersion: IBKR's last price beside the mark.
MARK_TYPE = "market.mark.recorded"
MARK_VERSION = 2
RECONCILIATION_TYPE = "account.reconciliation"
RECONCILIATION_VERSION = 1
MORNING = "morning"

# The payloads' unreadable markers (internal/event).
EXCESS = "excess-liquidity"
NET = "net-liquidation"
OPEN_ORDERS = "open-orders"
POSITIONS = "positions"
CASH = "total-cash"

# IBKR's tick type for the last traded price (TWS API "Tick Types": 4, LAST),
# the only tick read_marks takes as a mark.
LAST_PRICE_TICK = 4

# The currency the engine's balances are in; a balance in another is unreadable.
BASE_CURRENCY = "USD"

# IBKR's refusal of an account summary while it still counts two as active
# (TWS API "Message Codes" 322, "Maximum number of account summary requests
# exceeded"; reqAccountSummary allows two subscriptions at a time). It says
# nothing about the balances, so it is waited out and the summary asked
# again after each of SUMMARY_LIMIT_WAITS seconds (20 s more at most in one
# read); only a refusal that outlasts them leaves the balances unread.
SUMMARY_LIMIT_CODE = 322
SUMMARY_LIMIT_WAITS = (5.0, 15.0)

# IBKR's order statuses as an open order's status in the snapshot: one that
# can still execute is working, pending or cancel-pending (a cancel-pending
# order may still fill). Inactive is pending, since IBKR also uses it for an
# order it holds back, which may yet execute: counting it can only shrink a
# sale, never sell a share not held. Finished orders are not open.
OPEN_STATUS = {
    "PreSubmitted": "working",
    "Submitted": "working",
    "PendingSubmit": "pending",
    "ApiPending": "pending",
    "Inactive": "pending",
    "PendingCancel": "cancel-pending",
}

Record = namedtuple("Record", "event_type schema_version payload")
"""One recorded input: its event type, schema version and JSON-ready payload."""

AccountReading = namedtuple("AccountReading", (
    "account_id positions balances open_orders unread as_of unended summary"), defaults=(frozenset(), ""))
"""One read of the account. positions maps symbol to whole shares, balances
tag to Money, open_orders lists open-order payload rows; unread holds the
components that could not be read; as_of is a whole-second UTC datetime.
unended names the components whose answers carry no request ID (positions,
open orders) that were asked for and that IBKR had not ended when the read
gave up, however it gave up (a timeout, Gateway losing IBKR's servers,
which it restores on the same socket, or the link closing): their late
answers may still arrive on that link, where nothing tells them from a
later read's, so no later read on that link can be trusted to be complete.
summary says what became of the account summary, as logged: it ended
(naming any balance missing, in another currency or unreadable), IBKR
answered an error under it (its code and message, account numbers
masked), it did not end in time, or the link went first."""


class _Summary:
    """One account-summary request's answer: its rows, whether it ended, and any error IBKR sent under its ID."""

    def __init__(self, req_id):
        self.id = req_id
        self.ended = False
        self.error = None  # (code, message)
        self.invalid = set()  # the unread markers of rows whose value could not be read

    @property
    def answered(self):
        return self.ended or self.error is not None


def read_account(conn):
    """Read positions, the account summary and every open order in one pass.

    Nothing that fails here raises: each component that did not arrive, came
    in a form the engine cannot represent, or was lost with the link is
    marked unread, so the record still states what could be read. A link
    that changed state at all during the read (Connection.link_changes:
    lost, even if Gateway restored it, or dropped) leaves every component
    unread: nothing read before an outage is combined with anything read
    after it (ADR 0019: one cutoff; ADR 0025 point 5).
    """
    account = conn.config.account_id
    unread = set()
    positions, balances, orders = {}, {}, {}
    received = conn.now_utc()
    done = {"positions": False, "summary": False, "orders": False}
    summary = None
    changes = conn.link_changes
    if conn.connected:
        conn.pace_account_summary()
    asked = conn.connected
    if asked:
        # The open orders are this read's own list, ended only by its own
        # end, and asked first: an earlier list still outstanding is waited
        # for before anything else is asked (Connection.ask_all_open_orders).
        try:
            ticket = conn.ask_all_open_orders()
        except NotConnected:
            asked = False
    if asked:
        summary = _Summary(conn.new_req_id())
        conn.api.req_positions()
        conn.api.req_account_summary(summary.id, SUMMARY_GROUP, ",".join(SUMMARY_TAGS))
        try:
            for event in conn.events(conn.config.request_timeout):
                if not conn.connected:
                    break
                _take(event, account, summary, positions, balances, orders, unread, done,
                      conn.open_order_list(ticket))
                done["orders"] = conn.open_order_list(ticket) == LIST_ENDED
                if all(done.values()):
                    break
        finally:
            if conn.connected:
                conn.api.cancel_positions()
                conn.cancel_account_summary(summary.id)
        _log_summary(conn, summary, balances)
        summary = _ask_past_limit(conn, account, summary, balances, changes)
    as_of = None
    if conn.connected:
        try:
            as_of = datetime.fromtimestamp(_server_time(conn, conn.config.request_timeout), timezone.utc)
        except SnapshotIncomplete:
            pass
    link_changed = conn.link_changes != changes
    if not conn.connected or as_of is None or link_changed:
        # A read with no server time after it is not bounded by IBKR's clock.
        unread.update({POSITIONS, NET, EXCESS, CASH, OPEN_ORDERS})
        as_of = received
    if not done["positions"]:
        unread.add(POSITIONS)
    if not done["orders"]:
        unread.add(OPEN_ORDERS)
    if summary is None or not summary.ended or summary.error is not None:
        unread.update({NET, EXCESS, CASH})
    else:
        unread.update(summary.invalid)
    for tag, component in ((NET_LIQUIDATION, NET), (EXCESS_LIQUIDITY, EXCESS), (TOTAL_CASH, CASH)):
        if tag not in balances:
            unread.add(component)
    for component, tag in ((NET, NET_LIQUIDATION), (EXCESS, EXCESS_LIQUIDITY), (CASH, TOTAL_CASH)):
        if tag in balances and balances[tag].currency != BASE_CURRENCY:
            unread.add(component)
    unended = set()
    if asked:
        if not done["positions"]:
            unended.add(POSITIONS)
        if not done["orders"]:
            unended.add(OPEN_ORDERS)
    return AccountReading(
        unended=frozenset(unended),
        account_id=account,
        positions={} if POSITIONS in unread else positions,
        balances={} if link_changed else balances,
        open_orders=[] if OPEN_ORDERS in unread else sorted(orders.values(), key=lambda o: o["broker_order_id"]),
        unread=frozenset(unread),
        as_of=as_of.replace(microsecond=0),
        summary=("the link to IB Gateway was lost or dropped during the read, so none of it stands"
                 if link_changed else _summary_outcome(conn, summary, balances) if summary is not None else
                 "not asked: the link to IB Gateway was down"))


def _ask_past_limit(conn, account, summary, balances, changes):
    """The summary asked again after each of SUMMARY_LIMIT_WAITS while IBKR refuses it with its limit (322).

    Never once the link changed since the read began (changes, its
    link_changes then): that read stands for nothing (read_account).
    """
    for wait in SUMMARY_LIMIT_WAITS:
        if (not conn.connected or conn.link_changes != changes or summary.error is None
                or summary.error[0] != SUMMARY_LIMIT_CODE):
            return summary
        log.info("ibkr account summary refused by IBKR's limit of two at a time (error %d): asked again in %.0fs",
                 SUMMARY_LIMIT_CODE, wait)
        conn.pause(wait)
        if not conn.connected or conn.link_changes != changes:
            return summary
        conn.pace_account_summary()
        if not conn.connected or conn.link_changes != changes:
            return summary
        balances.clear()
        summary = _Summary(conn.new_req_id())
        conn.api.req_account_summary(summary.id, SUMMARY_GROUP, ",".join(SUMMARY_TAGS))
        try:
            for event in conn.events(conn.config.request_timeout):
                if not conn.connected:
                    break
                _take_summary(event, account, summary, balances)
                if summary.answered:
                    break
        finally:
            if conn.connected:
                conn.cancel_account_summary(summary.id)
        _log_summary(conn, summary, balances)
    return summary


def _summary_outcome(conn, summary, balances):
    """What became of summary, for the log and the reading (AccountReading.summary), account numbers masked."""
    if summary.error is not None:
        code, message = summary.error
        text = "errored: IBKR code {}: {}".format(code, message)
    elif not summary.ended:
        text = ("did not end within {}s".format(conn.config.request_timeout) if conn.connected
                else "the link to IB Gateway went before it ended")
    else:
        problems = []
        for tag in SUMMARY_TAGS:
            if tag not in balances:
                problems.append("{} missing".format(tag))
            elif balances[tag].currency != BASE_CURRENCY:
                problems.append("{} in {}".format(tag, balances[tag].currency))
        problems.extend("{} unreadable".format(c) for c in sorted(summary.invalid))
        text = "ended" + ("; " + ", ".join(problems) if problems else "")
    return mask_accounts(text, conn.config.account_id)


def _log_summary(conn, summary, balances):
    """Log what became of one account-summary request (_summary_outcome); a summary not read in full is a warning."""
    outcome = _summary_outcome(conn, summary, balances)
    level = logging.INFO if outcome == "ended" else logging.WARNING
    log.log(level, "ibkr account summary req_id=%d %s", summary.id, outcome)


def _take(event, account, summary, positions, balances, orders, unread, done, open_order_list):
    """Apply one callback to the read; an open order counts only while the read's own list is answered."""
    name, args = event.name, event.args
    if name == "position":
        _take_position(positions, account, unread, *args)
    elif name == "position_end":
        done["positions"] = True
    elif _take_summary(event, account, summary, balances):
        done["summary"] = summary.answered
    elif name == "open_order" and open_order_list == LIST_READING:
        _take_order(orders, account, unread, *args)


def _take_summary(event, account, summary, balances):
    """Apply one callback under summary's request ID (a row, its end, or IBKR's error); False for any other."""
    name, args = event.name, event.args
    if name not in ("account_summary", "account_summary_end", "error") or not args or args[0] != summary.id:
        return False
    if name == "account_summary":
        try:
            _add_balance(balances, account, *args[1:])
        except SnapshotInvalid:
            summary.invalid.add({NET_LIQUIDATION: NET, EXCESS_LIQUIDITY: EXCESS, TOTAL_CASH: CASH}[args[2]])
    elif name == "account_summary_end":
        summary.ended = True
    else:
        # Any error under the summary's ID fails it closed (ADR 0019: an
        # unavailable balance is not zero); its code and message are kept
        # for the log.
        summary.error = (args[1] if len(args) > 1 else None, str(args[2]) if len(args) > 2 else "")
    return True


def _take_position(positions, account, unread, row_account, contract, quantity, avg_cost):
    """One position row as the engine names it: whole shares of a US-dollar stock, by symbol."""
    if row_account != account:
        return
    rows = {}
    try:
        _add_position(rows, account, row_account, contract, quantity, avg_cost)
    except SnapshotInvalid:
        unread.add(POSITIONS)
        return
    row = rows[contract.conId]
    if row.quantity == 0:
        return  # an empty holding is stated by absence
    if (row.sec_type != "STK" or row.currency != BASE_CURRENCY or row.quantity != row.quantity.to_integral_value()
            or row.symbol in positions):
        unread.add(POSITIONS)
        return
    positions[row.symbol] = int(row.quantity)


def _take_order(orders, account, unread, order_id, contract, order, order_state):
    """One open order as the payload lists it, keyed by its identity across the login."""
    if order.account != account:
        return
    status = getattr(order_state, "status", None)
    if status in FINISHED:
        return
    try:
        record = open_order_record(order_id, contract, order, order_state)
        key = order_key(record.client_id, record.broker_order_id, record.perm_id)
    except (UnrecognisedOrderReport, ValueError):
        unread.add(OPEN_ORDERS)
        return
    side = {"BUY": "buy", "SELL": "sell"}.get(record.action)
    if (status not in OPEN_STATUS or side is None or record.quantity <= 0
            or record.quantity != record.quantity.to_integral_value()):
        unread.add(OPEN_ORDERS)
        return
    orders[key] = {
        "broker_order_id": order_label(key),
        "client_order_id": record.client_order_id or "",
        "instrument_id": record.symbol,
        "side": side,
        "status": OPEN_STATUS[status],
        # IBKR's openOrder carries the order's total, not what is left; the
        # total is never less than the remainder, so a sale sized net of it
        # can only be smaller, never sell a share not held.
        "remaining_quantity": int(record.quantity),
    }


def read_marks(conn, con_ids):
    """IBKR's mark for each symbol of con_ids (symbol to IBKR's contract ID): {symbol: Decimal}.

    One market-data snapshot per contract, asked together and waited for up
    to the configuration's mark_timeout, which outlasts the 11-second window
    at whose end IBKR ends a snapshot (config.MARK_TIMEOUT). A symbol has a mark only when
    its request completed (IBKR ended its snapshot) and every LAST price it
    carried was finite and positive, the mark being the last of them. A
    request refused (IBKR's error under it, such as no market-data
    permission), one that ended with no LAST price or carried any price
    that is not finite and positive, and one not ended in time (cancelled)
    give no mark: an uncertain price is never stated as the mark (ADR 0019).
    Nothing here raises: with the link down there are no marks.
    """
    if not conn.connected or not con_ids:
        return {}
    asked = {}
    for symbol in sorted(con_ids):
        req_id = conn.new_req_id()
        asked[req_id] = symbol
        conn.api.req_mkt_data(req_id, SimpleNamespace(conId=con_ids[symbol], symbol=symbol, secType=SEC_TYPE,
                                                      currency=CURRENCY, exchange=EXCHANGE))
    prices, failed, ended = {}, set(), set()
    try:
        for event in conn.events(conn.config.mark_timeout):
            if not conn.connected:
                break
            # Another callback (an order report, say) may carry no request ID.
            req_id = event.args[0] if event.args else None
            if not isinstance(req_id, int) or req_id not in asked or req_id in ended:
                continue
            if event.name == "tick_price" and event.args[1] == LAST_PRICE_TICK:
                price = event.args[2]
                if isinstance(price, (int, float)) and not isinstance(price, bool) and math.isfinite(price) \
                        and price > 0:
                    prices[req_id] = to_decimal(price, "mark")
                else:
                    failed.add(req_id)
            elif event.name in ("tick_snapshot_end", "error"):
                if event.name == "error":
                    failed.add(req_id)
                ended.add(req_id)
            if len(ended) == len(asked):
                break
    finally:
        if conn.connected:
            for req_id in sorted(set(asked) - ended):
                conn.api.cancel_mkt_data(req_id)
    if not conn.connected:
        return {}
    return {asked[r]: prices[r] for r in sorted(ended - failed) if r in prices}


def _stamp(at):
    return at.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _amount(reading, tag, component):
    return 0 if component in reading.unread else float(reading.balances[tag].amount)


def _as_of(reading, close, why):
    """The stamp of a reading: (as_of, as_of_close), as of close or at its read time (see the module)."""
    if (close is None) == (not why):
        raise ValueError("a snapshot is stamped either as of a close or at its read time with why it is not, "
                         "not both or neither")
    if close is None:
        return reading.as_of, False
    if close.tzinfo is None:
        raise ValueError("the close is a time with its zone")
    if close.replace(microsecond=0) != close or not close < reading.as_of:
        raise ValueError("a reading stands for a close only when taken after it, and a close is a whole second")
    return close, True


def close_valuation(reading, closes):
    """The account's net liquidation as of the close: its total cash plus each holding times its official close.

    closes maps a symbol to the delivered bar's official close. ADR 0019,
    amended 2026-10-18 (one price basis per rule): a reading that stands
    for the close is valued at the close, the price its marks are, never at
    IBKR's own valuation at the read, which moves with after-hours trading.
    A holding with no official close, or a total cash that could not be
    read, gives no valuation (ValueError).
    """
    if CASH in reading.unread or POSITIONS in reading.unread:
        raise ValueError("the account cannot be valued at the close: its {} could not be read".format(
            ", ".join(sorted(reading.unread & {CASH, POSITIONS}))))
    missing = sorted(s for s in reading.positions if s not in closes)
    if missing:
        raise ValueError("the account cannot be valued at the close: {} has no official close".format(
            ", ".join(missing)))
    total = reading.balances[TOTAL_CASH].amount
    for symbol in sorted(reading.positions):
        total += reading.positions[symbol] * to_decimal(closes[symbol], "close")
    return total


def broker_snapshot_record(reading, market_open, marks=None, close=None, why="", net_liquidation=None,
                           cash_drift=None, unexplained_cash=None):
    """The account.broker.snapshot (version 2) of reading, as of close or at its read time (see the module).

    With close, an aware datetime before the read, the snapshot is as of
    that close, its read time beside it, valued at the close: net_liquidation
    (close_valuation, which must be given) is its net liquidation, IBKR's
    own at the read stated beside it as read_net_liquidation, cash_drift
    the cash drift the adapter accepted and unexplained_cash that drift
    added up (book_basis; each zero when not given). Otherwise why, which
    must be stated, says why the reading cannot stand for the close; it is
    stamped at its read time and is IBKR's own reading, so none of
    net_liquidation, cash_drift and unexplained_cash may be given. A
    snapshot as of the close with net liquidation read states IBKR's own,
    a positive figure (the margin alarm's basis; the engine refuses one
    without it, ADR 0019: an unavailable balance is not zero). market_open is the
    adapter's own statement that the market was open at the stamp, and must
    be a bool. marks maps a symbol to its mark, on the snapshot's own basis;
    a holding with no mark has mark_set false.
    """
    if not isinstance(market_open, bool):
        raise TypeError("market_open must be stated as True or False")
    as_of, as_of_close = _as_of(reading, close, why)
    if as_of_close and net_liquidation is None:
        raise ValueError("a snapshot as of the close is valued at the close: its net liquidation is stated")
    if not as_of_close and (net_liquidation is not None or cash_drift is not None or unexplained_cash is not None):
        raise ValueError("a snapshot at its read time is IBKR's own reading: no valuation at the close, cash "
                         "drift or unexplained cash is stated")
    read_net = _amount(reading, NET_LIQUIDATION, NET)
    if as_of_close and NET not in reading.unread and not read_net > 0:
        raise ValueError("a snapshot as of the close states IBKR's own net liquidation at the read, a positive "
                         "figure; IBKR gave {}".format(read_net))
    marks = marks or {}
    positions = []
    for symbol in sorted(reading.positions):
        mark = marks.get(symbol)
        positions.append({
            "instrument_id": symbol, "quantity": reading.positions[symbol],
            "mark": 0 if mark is None else float(to_decimal(mark, "mark")), "mark_set": mark is not None,
        })
    return Record(BROKER_SNAPSHOT_TYPE, BROKER_SNAPSHOT_VERSION, {
        "account_id": reading.account_id,
        "as_of": _stamp(as_of),
        "market_open": market_open,
        "unreadable": sorted(reading.unread & {EXCESS, NET, OPEN_ORDERS, POSITIONS}),
        "net_liquidation": float(net_liquidation) if as_of_close else read_net,
        "excess_liquidity": _amount(reading, EXCESS_LIQUIDITY, EXCESS),
        "positions": positions,
        "open_orders": list(reading.open_orders),
        "read_at": _stamp(reading.as_of),
        "as_of_close": as_of_close,
        "not_as_of_close": "" if as_of_close else why,
        "read_net_liquidation": read_net,
        "cash_drift": float(cash_drift) if cash_drift is not None else 0,
        "unexplained_cash": float(unexplained_cash) if unexplained_cash is not None else 0,
    })


def account_snapshot_record(reading, as_of, net_liquidation=None):
    """The engine's account.snapshot (version 3) of reading, as of as_of: its positions and net liquidation.

    Both must have been read in full: an unavailable balance is not zero
    (ADR 0019), so a reading without them gives no snapshot (ValueError).
    net_liquidation, for a reading as of the close, is its valuation at the
    close (close_valuation; ADR 0019, amended 2026-10-18); without it the
    snapshot states IBKR's own at the read.
    """
    if reading.unread & {POSITIONS, NET}:
        raise ValueError("an account snapshot needs the positions and the net liquidation, and {} could not be "
                         "read".format(", ".join(sorted(reading.unread & {POSITIONS, NET}))))
    balance = reading.balances[NET_LIQUIDATION]
    return Record(ACCOUNT_SNAPSHOT_TYPE, ACCOUNT_SNAPSHOT_VERSION, {
        "account_id": reading.account_id,
        "as_of": _stamp(as_of),
        "net_liquidation": float(balance.amount if net_liquidation is None else net_liquidation),
        "currency": balance.currency,
        "positions": [{"instrument_id": s, "quantity": reading.positions[s]} for s in sorted(reading.positions)],
    })


def mark_record(symbol, as_of, price, source, broker_last=None):
    """The market.mark.recorded (version 2) of symbol at as_of: price, the delivered bar's official close.

    source names who reported the price (the data job); broker_last is
    IBKR's last price beside it, None when IBKR gave none. Nothing is sized
    from it, but the engine's price band compares a checked order's
    reference price with it when it is set, and with price otherwise (ADR
    0026 point 3, amended 2026-10-18).
    """
    price = to_decimal(price, "mark")
    if not price > 0:
        raise ValueError("a mark is a positive price")
    return Record(MARK_TYPE, MARK_VERSION, {
        "instrument_id": symbol,
        "as_of": _stamp(as_of),
        "price": float(price),
        "source": source,
        "broker_last": 0 if broker_last is None else float(to_decimal(broker_last, "broker last")),
        "broker_last_set": broker_last is not None,
    })


def reconciliation_record(reading, reconciliation_id, trigger=MORNING):
    """The account.reconciliation of reading: its positions and total cash, for the engine to classify."""
    if trigger != MORNING:
        raise ValueError("trigger {!r} is not {!r}".format(trigger, MORNING))
    if not reconciliation_id:
        raise ValueError("a reconciliation names its id")
    cash_read = CASH not in reading.unread
    return Record(RECONCILIATION_TYPE, RECONCILIATION_VERSION, {
        "account_id": reading.account_id,
        "reconciliation_id": reconciliation_id,
        "trigger": trigger,
        "as_of": _stamp(reading.as_of),
        "unreadable": sorted(reading.unread & {POSITIONS, CASH}),
        "positions": [{"instrument_id": s, "quantity": reading.positions[s]} for s in sorted(reading.positions)],
        "total_cash": float(reading.balances[TOTAL_CASH].amount) if cash_read else 0,
        "currency": reading.balances[TOTAL_CASH].currency if cash_read else "",
    })
