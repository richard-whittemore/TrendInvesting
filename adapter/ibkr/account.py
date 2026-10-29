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

from connection import ACCOUNT_VALUES_ASKED, CODE_ACCOUNT_UNSUBSCRIBED, NotConnected
from spelling import engine_symbol
from values import to_decimal

NET_LIQUIDATION = "NetLiquidation"
TOTAL_CASH = "TotalCashValue"
EXCESS_LIQUIDITY = "ExcessLiquidity"
SUMMARY_TAGS = (NET_LIQUIDATION, TOTAL_CASH, EXCESS_LIQUIDITY)
# IBKR's settled cash ("Cash recognized at the time of settlement", TWS API
# "Account Value Keys"; sent as a string in the account's base currency, as
# every account value is, adapter/ibkr/tests/testdata/ibapi_shapes.json), what
# an IRA's buys are budgeted from (ADR 0026 point 2, "Budget"). Read only
# for a configuration whose engine reads account.snapshot version 4
# (config.account_snapshot_version): Boost's reading is unchanged.
SETTLED_CASH = "SettledCash"
CASH_TAGS = SUMMARY_TAGS + (SETTLED_CASH,)


def account_tags(config):
    """The balance keys a reading for config needs: SUMMARY_TAGS, and SettledCash for account.snapshot version 4."""
    return CASH_TAGS if getattr(config, "account_snapshot_version", 3) == 4 else SUMMARY_TAGS
# What IBKR documents of these keys (TWS API, EWrapper::updateAccountValue
# and "Account Updates"; ibapi 10.51.01): each is the account's total ("no
# suffix"; the -S and -C keys are its security and commodity segments, and
# are not read), sent with "the currency on which the value is expressed",
# the account's base currency. The account summary's TotalCashValue, which
# gave the engine's approved starting book, is the same key. IBKR's
# documentation gives none of them a "BASE" row; a BASE row, if one comes,
# must agree with the base-currency row, or the balance is unreadable.
#
# The per-currency cash ledger: CashBalance ("Cash recognized at the time
# of trade + futures PNL") and TotalCashBalance ("Total Cash Balance
# including Future PNL"), one row per currency held plus, in IBKR's own
# sample client (apidemo AccountInfoPanel), a "BASE" total. The engine's
# book is in US dollars only (ADR 0019: values are exact decimals with
# their currency), so cash held in any other currency is a balance it
# cannot represent: a non-zero ledger row in another currency, or one that
# is not a number, makes the total cash unreadable, never a guessed sum.
CASH_LEDGER = ("CashBalance", "TotalCashBalance")
# IBKR's pseudo-currency label for a ledger row's base-currency total.
BASE_LABEL = "BASE"
# IBKR's notices under no request ID that its "Message Codes" list as
# informational about its data farms, and that say nothing about the
# account's values: 2104 (market data farm connection is OK), 2106 (a
# historical data farm is connected) and 2158 (sec-def data farm connection
# is OK), notifications expected on connecting; 2107 and 2108 (a historical
# or market data farm connection inactive "but should be available upon
# demand", which IBKR says is normal and safe to ignore). Every other error
# under no request ID while the values download fails the download closed,
# 2100 (unsubscribed from account data) and 2101 among them, as do 2103,
# 2105 and 2110, which IBKR describes as a problem reaching its servers
# (ADR 0019: an unavailable balance is not zero; AGENTS.md: fail closed on
# uncertain evidence).
NOTICE_CODES = frozenset({2104, 2106, 2107, 2108, 2158})
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
    """One download of the account's values (reqAccountUpdates): the rows for one account between its subscription and its end.

    take applies a callback. IBKR's account values carry no request ID, so
    the download is bounded by the adapter's own note (connection.ACCOUNT_VALUES_ASKED, with
    this download's ticket), queued just before the subscription was sent,
    and by the download's accountDownloadEnd:

    - before the note, every account-values callback (a row, an end, IBKR's
      2100) is an earlier subscription's and is discarded;
    - between the note and the end, an updateAccountValue row for the
      account's balance keys (tags: account_tags) or cash ledger (CASH_LEDGER) is
      the download's, other keys and accounts are skipped, and an error
      under no request ID that is not a notice (NOTICE_CODES) fails it;
    - after the end the download is frozen: later rows are IBKR's
      incremental updates, which are ignored, neither filling a balance the
      download lacked nor unsettling one it read; and errors are left to
      the caller.

    balances() gives the three balances and the tags that could not be read.

    Rule: ADR 0019 (one coherent cutoff; an unavailable balance is not
    zero; values are exact decimals with their currency) and ADR 0025
    point 5 (a late answer must not complete a later read): a value is
    taken only from this download's own rows; a tag whose rows conflict or
    cannot be read is invalid, never guessed.
    """

    def __init__(self, account, ticket=None, tags=SUMMARY_TAGS):
        self.account = account
        self.ticket = ticket
        # The balance keys this download takes (account_tags).
        self.tags = tuple(tags)
        self.open = ticket is None
        self.ended = False
        self.error = None  # (code, message)
        self._rows = {}  # key -> {currency: Decimal}
        self._invalid = set()  # keys with a row that is not a number or conflicts
        self.foreign_cash = ()  # currencies of cash held other than the balances' (set by balances())

    @property
    def answered(self):
        return self.ended or self.error is not None

    def take(self, event):
        """Apply one callback; False for one that is not the download's (so the caller may take it)."""
        name, args = event.name, event.args
        if name == ACCOUNT_VALUES_ASKED:
            if args and args[0] == self.ticket:
                self.open = True
            return True
        is_value = name == "account_value" and len(args) == 4
        is_end = name == "account_download_end" and bool(args)
        is_error = name == "error" and len(args) >= 2 and args[0] == NO_REQUEST_ID
        if not self.open:
            # An earlier subscription's, queued before this one was asked
            # (2100 among them: IBKR's answer to the adapter's own
            # unsubscription).
            return is_value or is_end or (is_error and args[1] == CODE_ACCOUNT_UNSUBSCRIBED)
        if self.ended:
            return is_value or is_end
        if is_value:
            key, value, currency, row_account = args
            if row_account == self.account and (key in self.tags or key in CASH_LEDGER):
                self._add(key, value, currency)
            return True
        if is_end:
            if args[0] == self.account:
                self.ended = True
            return True
        if is_error and args[1] not in NOTICE_CODES:
            self.error = (args[1], str(args[2]) if len(args) > 2 else "")
            return True
        return False

    def _add(self, key, value, currency):
        """One row of the download, compared as an exact amount, not as text.

        The same amount written another way ("512.30", "512.300") is the
        same row. A different amount for the same key and currency within
        one download leaves the key invalid: one download has one cutoff,
        and nothing tells which of two figures IBKR meant as of it (ADR
        0019: inconsistent evidence is unverifiable), so the balance is
        unread, never the later or the earlier figure.
        """
        try:
            amount = to_decimal(value, key)
        except ValueError:
            self._invalid.add(key)
            return
        rows = self._rows.setdefault(key, {})
        if currency in rows and rows[currency] != amount:
            self._invalid.add(key)
        rows.setdefault(currency, amount)

    def balances(self):
        """(balances, invalid): tag to Money for each tag read, and the tags whose rows could not be read.

        Each balance is IBKR's one row in the account's base currency (a
        real currency, not BASE). Every row received for the tag is
        validated, not only the one taken: a tag with a row that is not a
        number, a row restated with another amount, rows in two real
        currencies (ambiguous), a BASE row that disagrees with the
        base-currency row, or no real currency at all is invalid. The total
        cash is also invalid when the cash ledger shows cash held in a
        currency other than the balances' (foreign_cash names them) or a
        ledger row that is not a number (see CASH_LEDGER). A tag with no row
        is in neither: it is missing.
        """
        balances, invalid = {}, set()
        extra = tuple(t for t in self.tags if t not in SUMMARY_TAGS)
        for tag in (NET_LIQUIDATION, EXCESS_LIQUIDITY, TOTAL_CASH) + extra:
            rows = self._rows.get(tag)
            if tag in self._invalid:
                invalid.add(tag)
                continue
            if not rows:
                continue
            real = {c: v for c, v in rows.items() if c != BASE_LABEL}
            if len(real) != 1:
                invalid.add(tag)
                continue
            (currency, amount), = real.items()
            if not currency or (BASE_LABEL in rows and rows[BASE_LABEL] != amount):
                invalid.add(tag)
                continue
            balances[tag] = Money(amount, currency)
        cash = balances.get(TOTAL_CASH)
        if cash is not None:
            foreign = sorted({c for key in CASH_LEDGER for c, v in self._rows.get(key, {}).items()
                              if c not in (cash.currency, BASE_LABEL) and v != 0})
            self.foreign_cash = tuple(foreign)
            if foreign or any(key in self._invalid for key in CASH_LEDGER):
                del balances[TOTAL_CASH]
                invalid.add(TOTAL_CASH)
                # Settled cash is a cash total too: with cash held in another
                # currency, or a ledger row that cannot be read, it is no
                # more readable than the total (ADR 0019).
                if SETTLED_CASH in balances:
                    del balances[SETTLED_CASH]
                    invalid.add(SETTLED_CASH)
        return balances, invalid


def ask_account_values(conn, account):
    """Subscribe to account's values (reqAccountUpdates) and return the AccountValues that takes the download.

    The caller settles the link first (Connection.settle_account_values),
    before it asks for anything else.
    """
    return AccountValues(account, conn.ask_account_values(account), account_tags(conn.config))


def end_account_values(conn, account, values):
    """Unsubscribe from account's values, if the link is up, noting whether IBKR ended the download.

    Always called once a read has subscribed, however it ends (a finally):
    the next read on the link then settles it first, waiting out a download
    IBKR had not ended (Connection.settle_account_values; ADR 0025 point 5).
    """
    conn.end_account_values(account, values is not None and values.ended)


def read_snapshot(conn):
    """Read the configured account's positions and balances through conn, failing closed.

    Positions and summary rows for other accounts under the same IBKR login
    are skipped: each engine owns one account (ADR 0026 point 8).
    """
    if not conn.connected:
        raise SnapshotIncomplete(["connection"])
    account = conn.config.account_id
    timeout = conn.config.request_timeout
    try:
        # First, before anything is asked: what is pumped here is consumed.
        conn.settle_account_values()
    except NotConnected:
        raise SnapshotIncomplete(["connection"]) from None
    requested = _server_time(conn, timeout)

    conn.api.req_positions()
    values = None
    positions = {}
    positions_done = False
    try:
        values = ask_account_values(conn, account)
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
    except NotConnected:
        raise SnapshotIncomplete(["connection"]) from None
    finally:
        # However the read ends, the subscription is ended and the link is
        # marked, so the next read settles it first (ADR 0025 point 5).
        if conn.connected:
            conn.api.cancel_positions()
        end_account_values(conn, account, values)

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
            account_id=row_account, con_id=contract.conId, symbol=engine_symbol(contract.symbol),
            sec_type=contract.secType, currency=contract.currency,
            quantity=to_decimal(quantity, "position"), avg_cost=to_decimal(avg_cost, "avg_cost"))
    except ValueError as err:
        raise SnapshotInvalid(str(err)) from None
