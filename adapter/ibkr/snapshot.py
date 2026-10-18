"""account.broker.snapshot and account.reconciliation payloads from one IBKR read.

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
"""
from collections import namedtuple
from datetime import datetime, timezone

from account import EXCESS_LIQUIDITY, NET_LIQUIDATION, SUMMARY_GROUP, SUMMARY_TAGS, TOTAL_CASH
from account import SnapshotIncomplete, SnapshotInvalid, _add_balance, _add_position, _server_time
from orders import UnrecognisedOrderReport, open_order_record, order_key
from values import to_decimal

BROKER_SNAPSHOT_TYPE = "account.broker.snapshot"
BROKER_SNAPSHOT_VERSION = 1
RECONCILIATION_TYPE = "account.reconciliation"
RECONCILIATION_VERSION = 1
MORNING = "morning"

# The payloads' unreadable markers (internal/event).
EXCESS = "excess-liquidity"
NET = "net-liquidation"
OPEN_ORDERS = "open-orders"
POSITIONS = "positions"
CASH = "total-cash"

# The currency the engine's balances are in; a balance in another is unreadable.
BASE_CURRENCY = "USD"

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
FINISHED = frozenset({"Cancelled", "ApiCancelled", "Filled"})

Record = namedtuple("Record", "event_type schema_version payload")
"""One recorded input: its event type, schema version and JSON-ready payload."""

AccountReading = namedtuple("AccountReading", (
    "account_id positions balances open_orders unread as_of"))
"""One read of the account. positions maps symbol to whole shares, balances
tag to Money, open_orders lists open-order payload rows; unread holds the
components that could not be read; as_of is a whole-second UTC datetime."""


def read_account(conn):
    """Read positions, the account summary and every open order in one pass.

    Nothing that fails here raises: each component that did not arrive, came
    in a form the engine cannot represent, or was lost with the link is
    marked unread, so the record still states what could be read.
    """
    account = conn.config.account_id
    unread = set()
    positions, balances, orders = {}, {}, {}
    received = conn.now_utc()
    done = {"positions": False, "summary": False, "orders": False}
    if conn.connected:
        summary_id = conn.new_req_id()
        conn.api.req_positions()
        conn.api.req_account_summary(summary_id, SUMMARY_GROUP, ",".join(SUMMARY_TAGS))
        conn.api.req_all_open_orders()
        try:
            for event in conn.events(conn.config.request_timeout):
                if not conn.connected:
                    break
                _take(event, account, summary_id, positions, balances, orders, unread, done)
                if all(done.values()):
                    break
        finally:
            if conn.connected:
                conn.api.cancel_positions()
                conn.api.cancel_account_summary(summary_id)
    as_of = None
    if conn.connected:
        try:
            as_of = datetime.fromtimestamp(_server_time(conn, conn.config.request_timeout), timezone.utc)
        except SnapshotIncomplete:
            pass
    if not conn.connected or as_of is None:
        # A read with no server time after it is not bounded by IBKR's clock.
        unread.update({POSITIONS, NET, EXCESS, CASH, OPEN_ORDERS})
        as_of = received
    if not done["positions"]:
        unread.add(POSITIONS)
    if not done["orders"]:
        unread.add(OPEN_ORDERS)
    if not done["summary"]:
        unread.update({NET, EXCESS, CASH})
    for tag, component in ((NET_LIQUIDATION, NET), (EXCESS_LIQUIDITY, EXCESS), (TOTAL_CASH, CASH)):
        if tag not in balances:
            unread.add(component)
    for component, tag in ((NET, NET_LIQUIDATION), (EXCESS, EXCESS_LIQUIDITY), (CASH, TOTAL_CASH)):
        if tag in balances and balances[tag].currency != BASE_CURRENCY:
            unread.add(component)
    return AccountReading(
        account_id=account,
        positions={} if POSITIONS in unread else positions,
        balances=balances,
        open_orders=[] if OPEN_ORDERS in unread else sorted(orders.values(), key=lambda o: o["broker_order_id"]),
        unread=frozenset(unread),
        as_of=as_of.replace(microsecond=0))


def _take(event, account, summary_id, positions, balances, orders, unread, done):
    name, args = event.name, event.args
    if name == "position":
        _take_position(positions, account, unread, *args)
    elif name == "position_end":
        done["positions"] = True
    elif name == "account_summary" and args[0] == summary_id:
        try:
            _add_balance(balances, account, *args[1:])
        except SnapshotInvalid:
            unread.add({NET_LIQUIDATION: NET, EXCESS_LIQUIDITY: EXCESS, TOTAL_CASH: CASH}[args[2]])
    elif name == "account_summary_end" and args[0] == summary_id:
        done["summary"] = True
    elif name == "error" and args[0] == summary_id:
        unread.update({NET, EXCESS, CASH})
        done["summary"] = True
    elif name == "open_order":
        _take_order(orders, account, unread, *args)
    elif name == "open_order_end":
        done["orders"] = True


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
    broker_id = "perm:{}".format(key[2]) if key[0] == "perm" else "order:{}:{}".format(key[1], key[2])
    orders[key] = {
        "broker_order_id": broker_id,
        "client_order_id": record.client_order_id or "",
        "instrument_id": record.symbol,
        "side": side,
        "status": OPEN_STATUS[status],
        # IBKR's openOrder carries the order's total, not what is left; the
        # total is never less than the remainder, so a sale sized net of it
        # can only be smaller, never sell a share not held.
        "remaining_quantity": int(record.quantity),
    }


def _stamp(at):
    return at.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _amount(reading, tag, component):
    return 0 if component in reading.unread else float(reading.balances[tag].amount)


def broker_snapshot_record(reading, market_open, marks=None):
    """The account.broker.snapshot of reading.

    market_open is the adapter's own statement that the market was open at
    as_of, and must be a bool. marks maps a symbol to IBKR's current mark for
    it; a holding with no mark has mark_set false.
    """
    if not isinstance(market_open, bool):
        raise TypeError("market_open must be stated as True or False")
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
        "as_of": _stamp(reading.as_of),
        "market_open": market_open,
        "unreadable": sorted(reading.unread & {EXCESS, NET, OPEN_ORDERS, POSITIONS}),
        "net_liquidation": _amount(reading, NET_LIQUIDATION, NET),
        "excess_liquidity": _amount(reading, EXCESS_LIQUIDITY, EXCESS),
        "positions": positions,
        "open_orders": list(reading.open_orders),
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
