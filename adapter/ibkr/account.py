"""One account snapshot assembled from IBKR's position and account-summary callbacks.

ADR 0025 point 5: on every start and reconnect the adapter reports the
account's positions and cash. ADR 0019: an unavailable balance is not zero,
values are exact decimals with their currency, and a snapshot records the
times that bound it. plan.md section 5: Excess Liquidity feeds the
margin-cushion alarm.

IBKR does not timestamp positions or account-summary values. The snapshot is
therefore bracketed by two IBKR server times, one requested before the
position and summary requests and one after both have finished, plus the
adapter's own receipt time.
"""
from collections import namedtuple

from connection import NotConnected
from values import to_decimal

SUMMARY_GROUP = "All"
NET_LIQUIDATION = "NetLiquidation"
TOTAL_CASH = "TotalCashValue"
EXCESS_LIQUIDITY = "ExcessLiquidity"
SUMMARY_TAGS = (NET_LIQUIDATION, TOTAL_CASH, EXCESS_LIQUIDITY)

Money = namedtuple("Money", "amount currency")

Position = namedtuple("Position", "account_id con_id symbol sec_type currency quantity avg_cost")
"""One position row as IBKR reported it; a zero quantity is kept, not dropped."""

AccountSnapshot = namedtuple("AccountSnapshot", (
    "account_id positions net_liquidation total_cash excess_liquidity "
    "server_time_requested server_time_completed received_at"))
"""Positions (sorted by IBKR contract ID) and three balances for one account.

server_time_* are IBKR epoch seconds from reqCurrentTime; received_at is the
adapter's UTC clock when the snapshot completed.
"""


class SnapshotIncomplete(Exception):
    """A component did not arrive; missing names which (ADR 0019: unverifiable, not zero)."""

    def __init__(self, missing):
        super().__init__("account snapshot incomplete: missing {}".format(", ".join(missing)))
        self.missing = tuple(missing)


class SnapshotInvalid(Exception):
    """IBKR reported something this snapshot cannot represent faithfully."""


def read_snapshot(conn):
    """Read the configured account's positions and balances through conn, failing closed.

    Positions and summary rows for other accounts under the same IBKR login
    are skipped: each engine owns one account (ADR 0026 point 8).
    """
    if not conn.connected:
        raise SnapshotIncomplete(["connection"])
    account = conn.config.account_id
    timeout = conn.config.request_timeout
    requested = _server_time(conn, timeout)

    summary_id = conn.new_req_id()
    conn.api.req_positions()
    conn.api.req_account_summary(summary_id, SUMMARY_GROUP, ",".join(SUMMARY_TAGS))
    positions = {}
    balances = {}
    positions_done = summary_done = summary_failed = False
    try:
        for event in conn.events(timeout):
            if not conn.connected:
                break
            name, args = event.name, event.args
            if name == "position":
                _add_position(positions, account, *args)
            elif name == "position_end":
                positions_done = True
            elif name == "account_summary" and args[0] == summary_id:
                _add_balance(balances, account, *args[1:])
            elif name == "account_summary_end" and args[0] == summary_id:
                summary_done = True
            elif name == "error" and args[0] == summary_id:
                summary_failed = True
                break
            if positions_done and summary_done:
                break
    finally:
        if conn.connected:
            conn.api.cancel_positions()
            conn.api.cancel_account_summary(summary_id)

    missing = []
    if not conn.connected:
        missing.append("connection")
    if not positions_done:
        missing.append("positions")
    if not summary_done or summary_failed:
        missing.append("account-summary")
    missing.extend(tag for tag in SUMMARY_TAGS if tag not in balances)
    if missing:
        raise SnapshotIncomplete(missing)

    completed = _server_time(conn, timeout)
    return AccountSnapshot(
        account_id=account,
        positions=tuple(positions[k] for k in sorted(positions)),
        net_liquidation=balances[NET_LIQUIDATION],
        total_cash=balances[TOTAL_CASH],
        excess_liquidity=balances[EXCESS_LIQUIDITY],
        server_time_requested=requested,
        server_time_completed=completed,
        received_at=conn.now_utc(),
    )


def _server_time(conn, timeout):
    try:
        ticket = conn.request_server_time()
    except NotConnected:
        raise SnapshotIncomplete(["connection"]) from None
    for event in conn.events(timeout):
        if not conn.connected:
            break
        if event.name == "current_time" and conn.time_answers >= ticket:
            return event.args[0]
    raise SnapshotIncomplete(["server-time"])


def _add_position(positions, account, row_account, contract, quantity, avg_cost):
    if row_account != account:
        return
    if contract.conId in positions:
        raise SnapshotInvalid("two position rows for contract {}".format(contract.conId))
    try:
        positions[contract.conId] = Position(
            account_id=row_account, con_id=contract.conId, symbol=contract.symbol,
            sec_type=contract.secType, currency=contract.currency,
            quantity=to_decimal(quantity, "position"), avg_cost=to_decimal(avg_cost, "avg_cost"))
    except ValueError as err:
        raise SnapshotInvalid(str(err)) from None


def _add_balance(balances, account, row_account, tag, value, currency):
    if row_account != account or tag not in SUMMARY_TAGS:
        return
    if not currency:
        raise SnapshotInvalid("{} has no currency".format(tag))
    try:
        money = Money(to_decimal(value, tag), currency)
    except ValueError as err:
        raise SnapshotInvalid(str(err)) from None
    if tag in balances and balances[tag] != money:
        raise SnapshotInvalid("{} reported twice with different values".format(tag))
    balances[tag] = money
