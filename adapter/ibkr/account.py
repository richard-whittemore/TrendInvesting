"""One account snapshot assembled from IBKR's position and account-value callbacks.

ADR 0025 point 5: on every start and reconnect the adapter reports the
account's positions and cash. ADR 0019: an unavailable balance is not zero,
values are exact decimals with their currency, and a snapshot records the
times that bound it. plan.md section 5: Excess Liquidity feeds the
margin-cushion alarm.

The balances come from IBKR's account updates (TWS API reqAccountUpdates),
not its account summary (reqAccountSummary). IBKR allows two account-summary
subscriptions per connection, and on 2026-10-25 IB Gateway 10.50 freed
neither when it was cancelled: exactly two summaries were answered on a
connection and every later one was refused (error 322) for minutes. An
account-updates subscription is one per client and may be asked for again
freely, so each read is a fresh download that does not depend on IBKR
honouring a cancel: subscribe, take every updateAccountValue for the
account until its accountDownloadEnd, unsubscribe (AccountValues).

IBKR does not timestamp positions or account values. The snapshot is
therefore bracketed by two IBKR server times, one requested before the
position and account-value requests and one after both have finished, plus
the adapter's own receipt time.
"""
from collections import namedtuple

from connection import NotConnected
from values import to_decimal

NET_LIQUIDATION = "NetLiquidation"
TOTAL_CASH = "TotalCashValue"
EXCESS_LIQUIDITY = "ExcessLiquidity"
SUMMARY_TAGS = (NET_LIQUIDATION, TOTAL_CASH, EXCESS_LIQUIDITY)
# IBKR's pseudo-currency for a value in the account's base currency
# (reqAccountUpdates sends TotalCashValue once per currency held and once
# as "BASE", the total in the base currency, which is what the account
# summary's TotalCashValue is).
BASE_LABEL = "BASE"
# IBKR's notices under no request ID that say nothing about the account
# values (TWS API "Message Codes" 2102 to 2199: market-data farm and
# similar connection notices). Any other error under no request ID while
# the values download, 2100 (this client unsubscribed from the account's
# data) and 2101 (another account subscribed) among them, fails the
# download closed (ADR 0019: an unavailable balance is not zero).
NOTICE_CODES = range(2102, 2200)
NO_REQUEST_ID = -1

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


class AccountValues:
    """One download of the account's values (reqAccountUpdates): the rows for one account until its end.

    take applies a callback: an updateAccountValue row for the account (rows
    for another account and other keys are skipped), its accountDownloadEnd,
    or an error under no request ID that is not a notice (NOTICE_CODES),
    which fails it. balances() gives the three balances and the tags that
    could not be read.

    Rule: ADR 0019 (an unavailable balance is not zero; values are exact
    decimals with their currency): a value is taken only from this
    download's own rows; a tag whose rows conflict or cannot be read is
    invalid, never guessed.
    """

    def __init__(self, account):
        self.account = account
        self.ended = False
        self.error = None  # (code, message)
        self._rows = {}  # tag -> {currency: value text}
        self._conflicts = set()

    @property
    def answered(self):
        return self.ended or self.error is not None

    def take(self, event):
        """Apply one callback; False for one that is not the download's (so the caller may take it)."""
        name, args = event.name, event.args
        if name == "account_value" and len(args) == 4:
            key, value, currency, row_account = args
            if row_account == self.account and key in SUMMARY_TAGS:
                rows = self._rows.setdefault(key, {})
                if currency in rows and rows[currency] != value:
                    self._conflicts.add(key)
                rows[currency] = value
            return True
        if name == "account_download_end" and args:
            if args[0] == self.account:
                self.ended = True
            return True
        if name == "error" and len(args) >= 2 and args[0] == NO_REQUEST_ID and args[1] not in NOTICE_CODES:
            self.error = (args[1], str(args[2]) if len(args) > 2 else "")
            return True
        return False

    def balances(self):
        """(balances, invalid): tag to Money for each tag read, and the tags whose rows could not be read.

        NetLiquidation and ExcessLiquidity are each IBKR's one row in the
        account's base currency. TotalCashValue is the "BASE" row (the
        total, as the account summary gives it) in NetLiquidation's
        currency, or, with no BASE row, the one currency's row if it is the
        only one. A tag with more than one candidate, a row restated with
        another value, or a value that is not a number is invalid. A tag
        with no row is in neither: it is missing.
        """
        balances, invalid = {}, set()
        for tag in (NET_LIQUIDATION, EXCESS_LIQUIDITY, TOTAL_CASH):
            rows = self._rows.get(tag)
            if not rows:
                continue
            if tag in self._conflicts:
                invalid.add(tag)
                continue
            real = {c: v for c, v in rows.items() if c != BASE_LABEL}
            if tag == TOTAL_CASH and BASE_LABEL in rows:
                base = balances.get(NET_LIQUIDATION)
                if base is None:
                    invalid.add(tag)
                    continue
                currency, value = base.currency, rows[BASE_LABEL]
            elif len(real) == 1:
                (currency, value), = real.items()
            else:
                invalid.add(tag)
                continue
            if not currency:
                invalid.add(tag)
                continue
            try:
                balances[tag] = Money(to_decimal(value, tag), currency)
            except ValueError:
                invalid.add(tag)
        return balances, invalid


def ask_account_values(conn, account):
    """Subscribe to account's values (reqAccountUpdates) and return the AccountValues that takes the download."""
    conn.api.req_account_updates(True, account)
    return AccountValues(account)


def end_account_values(conn, account):
    """Unsubscribe from account's values, if the link is up (a dead socket ends every subscription itself)."""
    if conn.connected:
        conn.api.req_account_updates(False, account)


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

    conn.api.req_positions()
    values = ask_account_values(conn, account)
    positions = {}
    positions_done = False
    try:
        for event in conn.events(timeout):
            if not conn.connected:
                break
            name, args = event.name, event.args
            if name == "position":
                _add_position(positions, account, *args)
            elif name == "position_end":
                positions_done = True
            elif values.take(event) and values.error is not None:
                break
            if positions_done and values.ended:
                break
    finally:
        if conn.connected:
            conn.api.cancel_positions()
        end_account_values(conn, account)

    balances, invalid = values.balances()
    if invalid:
        raise SnapshotInvalid("{} could not be read from IBKR's account values".format(", ".join(sorted(invalid))))
    missing = []
    if not conn.connected:
        missing.append("connection")
    if not positions_done:
        missing.append("positions")
    if not values.ended or values.error is not None:
        missing.append("account-values")
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
