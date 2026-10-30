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
From that one reading come the engine's account.snapshot (version 3, or cash versions 4/6,
account_snapshot_record) and the broker snapshot (version 2, or fractional version 3,
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

from account import EXCESS_LIQUIDITY, NET_LIQUIDATION, SETTLED_CASH, TOTAL_CASH, account_tags
from account import SnapshotIncomplete, SnapshotInvalid, _add_position, _server_time, ask_account_values
from account import end_account_values
from connection import LIST_ENDED, LIST_READING, NotConnected
from masking import mask_accounts
from orders import FINISHED, UnrecognisedOrderReport, open_order_record, order_key, order_label
from sender import CURRENCY, EXCHANGE, SEC_TYPE
from spelling import ibkr_symbol
from values import to_decimal
from quantities import quantity_scale, units, shares

log = logging.getLogger("ibkr")

BROKER_SNAPSHOT_TYPE = "account.broker.snapshot"
# internal/event BrokerSnapshotReadAtVersion: the read time beside the stamp.
BROKER_SNAPSHOT_VERSION = 2
# internal/event AccountSnapshotPositionsVersion: what Boost reads.
ACCOUNT_SNAPSHOT_TYPE = "account.snapshot"
ACCOUNT_SNAPSHOT_VERSION = 3
# internal/event AccountSnapshotCashVersion: version 3 with IBKR's settled and
# total cash, what the momentum engine reads (ADR 0026 point 2).
ACCOUNT_SNAPSHOT_CASH_VERSION = 4
# internal/event MarkRecordedEvidenceVersion: IBKR's last price beside the mark.
MARK_TYPE = "market.mark.recorded"
MARK_VERSION = 2
# internal/event MarkRecordedContractVersion: version 2 with the IBKR contract
# the mark is for, as reqContractDetails resolved it (ADR 0026 point 3).
MARK_CONTRACT_VERSION = 3
RECONCILIATION_TYPE = "account.reconciliation"
RECONCILIATION_VERSION = 1
MORNING = "morning"

# The payloads' unreadable markers (internal/event).
EXCESS = "excess-liquidity"
NET = "net-liquidation"
OPEN_ORDERS = "open-orders"
POSITIONS = "positions"
CASH = "total-cash"
# Version 4's settled cash: unread in a reading for a configuration that
# needs it (account.account_tags) when IBKR did not give it in full.
SETTLED = "settled-cash"

# How many market-data snapshots are asked for at once: IBKR's market-data
# lines (TWS API "Market Data Lines": at least 100 concurrent requests per
# account, more only with more commissions or booster packs). An IRA marks
# its 500 ranked members, so they are asked for in batches well inside it;
# Boost's few holdings are one batch, as before.
MARK_BATCH = 50
# The one kind of contract a mark is resolved to, the one the engine trades
# (sender.SEC_TYPE, CURRENCY, EXCHANGE): a US-dollar stock routed by SMART.
CONTRACT_SEC_TYPE, CONTRACT_CURRENCY, CONTRACT_EXCHANGE = SEC_TYPE, CURRENCY, EXCHANGE

# IBKR's tick type for the last traded price (TWS API "Tick Types": 4, LAST),
# the only tick read_marks takes as a mark.
LAST_PRICE_TICK = 4

# The currency the engine's balances are in; a balance in another is unreadable.
BASE_CURRENCY = "USD"
# Each balance's unread marker.
MARKERS = {NET_LIQUIDATION: NET, EXCESS_LIQUIDITY: EXCESS, TOTAL_CASH: CASH, SETTLED_CASH: SETTLED}

# The unended marker of an account-values download IBKR had not ended: its
# accountDownloadEnd carries no request ID, so a late one would end a later
# read's download early (AccountReading.unended).
ACCOUNT_VALUES = "account-values"

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
    "account_id positions balances open_orders unread as_of unended summary quantity_scale"), defaults=(frozenset(), "", 0))
"""One read of the account. positions maps symbol to exact Decimal shares (legacy whole shares stay ints), balances
tag to Money, open_orders lists open-order payload rows; unread holds the
components that could not be read; as_of is a whole-second UTC datetime.
unended names the components whose answers carry no request ID (positions,
open orders, the account values) that were asked for and that IBKR had not
ended when the read gave up, however it gave up (a timeout, an error,
Gateway losing IBKR's servers, which it restores on the same socket, or the
link closing): their late answers may still arrive on that link, where
nothing tells them from a later read's, so no later read on that link can
be trusted to be complete.
summary says what became of the account values (account.AccountValues), as
logged: they ended (naming any balance missing, in another currency or
unreadable), IBKR answered an error while they downloaded (its code and
message, account numbers masked), they did not end in time, or the link
went first.

Rule: ADR 0019 (an unavailable balance is not zero; collection failures with
explicit missing components). Callers must treat every component in unread
as unavailable, never as zero, and must not combine a reading with one taken
before a link change."""


def read_account(conn):
    """Read positions, the account values and every open order in one pass.

    The balances are a fresh download of IBKR's account values
    (account.AccountValues: subscribed, taken to its end, unsubscribed), so
    no read depends on IBKR honouring an earlier read's cancel.

    Nothing that fails here raises: each component that did not arrive, came
    in a form the engine cannot represent, or was lost with the link is
    marked unread, so the record still states what could be read. A link
    that changed state at all during the read (Connection.link_changes:
    lost, even if Gateway restored it, or dropped) leaves every component
    unread: nothing read before an outage is combined with anything read
    after it (ADR 0019: one cutoff; ADR 0025 point 5).

    Callers must treat each component the record marks unread as unavailable,
    not zero, and reconcile from a fresh read before using any of them (ADR
    0019: an unavailable balance is not zero).
    """
    account = conn.config.account_id
    # The balances this reading needs, each with its unread marker: Boost's
    # three, and an IRA's settled cash (account.account_tags).
    needed = tuple((tag, MARKERS[tag]) for tag in account_tags(conn.config))
    balance_markers = {component for _, component in needed}
    unread = set()
    reasons = []
    scale = quantity_scale(conn.config)
    positions, balances, orders = {}, {}, {}
    received = conn.now_utc()
    done = {"positions": False, "summary": False, "orders": False}
    summary = None
    changes = conn.link_changes
    asked = conn.connected
    if asked:
        # The account values first, before anything is asked: what is pumped
        # settling them is consumed (Connection.settle_account_values).
        try:
            conn.settle_account_values()
        except NotConnected:
            asked = False
    if asked:
        # The open orders are this read's own list, ended only by its own
        # end, and asked first: an earlier list still outstanding is waited
        # for before anything else is asked (Connection.ask_all_open_orders).
        try:
            ticket = conn.ask_all_open_orders()
        except NotConnected:
            asked = False
    if asked:
        conn.api.req_positions()
        try:
            summary = ask_account_values(conn, account)
            for event in conn.events(conn.config.request_timeout):
                if not conn.connected:
                    break
                _take(event, account, summary, positions, orders, unread, done, conn.open_order_list(ticket), scale, reasons)
                done["orders"] = conn.open_order_list(ticket) == LIST_ENDED
                if all(done.values()):
                    break
        except NotConnected:
            pass  # the link went before the values were asked for: every component is unread below
        finally:
            if conn.connected:
                conn.api.cancel_positions()
            end_account_values(conn, account, summary)
        if summary is not None:
            balances, invalid = summary.balances()
            _log_summary(conn, summary, balances, invalid)
    as_of = None
    if conn.connected:
        try:
            as_of = datetime.fromtimestamp(_server_time(conn, conn.config.request_timeout), timezone.utc)
        except SnapshotIncomplete:
            pass
    link_changed = conn.link_changes != changes
    if not conn.connected or as_of is None or link_changed:
        # A read with no server time after it is not bounded by IBKR's clock.
        unread.update({POSITIONS, OPEN_ORDERS} | balance_markers)
        as_of = received
    if not done["positions"]:
        unread.add(POSITIONS)
    if not done["orders"]:
        unread.add(OPEN_ORDERS)
    if summary is None or not summary.ended or summary.error is not None:
        unread.update(balance_markers)
    else:
        unread.update(MARKERS[tag] for tag in invalid)
    for tag, component in needed:
        if tag not in balances:
            unread.add(component)
        elif balances[tag].currency != BASE_CURRENCY:
            unread.add(component)
    unended = set()
    if asked:
        if not done["positions"]:
            unended.add(POSITIONS)
        if not done["orders"]:
            unended.add(OPEN_ORDERS)
        if summary is not None and not summary.ended:
            unended.add(ACCOUNT_VALUES)
    return AccountReading(
        quantity_scale=scale,
        unended=frozenset(unended),
        account_id=account,
        positions={} if POSITIONS in unread else positions,
        balances={} if link_changed else balances,
        open_orders=[] if OPEN_ORDERS in unread else sorted(orders.values(), key=lambda o: o["broker_order_id"]),
        unread=frozenset(unread),
        as_of=as_of.replace(microsecond=0),
        summary=("the link to IB Gateway was lost or dropped during the read, so none of it stands"
                 if link_changed else _summary_outcome(conn, summary, balances, invalid) if summary is not None else
                 "not asked: the link to IB Gateway was down") + ("; " + "; ".join(reasons) if reasons else ""))


def _summary_outcome(conn, summary, balances, invalid):
    """What became of the account values (account.AccountValues), for the log and the reading, account numbers masked.

    Rule: ADR 0019 (collection failures with explicit missing components):
    values not read in full say why: IBKR's error, no end in time, the link
    gone, or, when they ended, each balance missing, in another currency or
    unreadable.
    """
    if summary.error is not None:
        code, message = summary.error
        text = "errored: IBKR code {}: {}".format(code, message)
    elif not summary.ended:
        text = ("did not end within {}s".format(conn.config.request_timeout) if conn.connected
                else "the link to IB Gateway went before it ended")
    else:
        problems = []
        for tag in summary.tags:
            if tag in invalid and tag == TOTAL_CASH and summary.foreign_cash:
                problems.append("{} unreadable: cash in {}".format(MARKERS[tag], ", ".join(summary.foreign_cash)))
            elif tag in invalid:
                problems.append("{} unreadable".format(MARKERS[tag]))
            elif tag not in balances:
                problems.append("{} missing".format(tag))
            elif balances[tag].currency != BASE_CURRENCY:
                problems.append("{} in {}".format(tag, balances[tag].currency))
        text = "ended" + ("; " + ", ".join(problems) if problems else "")
    return mask_accounts(text, conn.config.account_id)


def _log_summary(conn, summary, balances, invalid):
    """Log what became of one account-values download (_summary_outcome); one not read in full is a warning.

    Rule: ADR 0019 (collection failures with explicit missing components):
    values not read in full are logged as a warning, never as success.
    """
    outcome = _summary_outcome(conn, summary, balances, invalid)
    level = logging.INFO if outcome == "ended" else logging.WARNING
    log.log(level, "ibkr account values %s", outcome)


def _take(event, account, summary, positions, orders, unread, done, open_order_list, scale=0, reasons=None):
    """Apply one callback to the read; an open order counts only while the read's own list is answered."""
    name, args = event.name, event.args
    if name == "position":
        _take_position(positions, account, unread, *args, scale=scale, reasons=reasons)
    elif name == "position_end":
        done["positions"] = True
    elif summary.take(event):
        done["summary"] = summary.answered
    elif name == "open_order" and open_order_list == LIST_READING:
        _take_order(orders, account, unread, *args, scale=scale, reasons=reasons)


def _take_position(positions, account, unread, row_account, contract, quantity, avg_cost, scale=0, reasons=None):
    """One stock position under the configured D7 policy (ADR 0026), never rounded."""
    if row_account != account:
        return
    rows = {}
    try:
        _add_position(rows, account, row_account, contract, quantity, avg_cost)
    except SnapshotInvalid as err:
        _quantity_refused(unread, POSITIONS, str(err), reasons)
        return
    row = rows[contract.conId]
    try:
        count = units(row.quantity, scale, "position " + row.symbol)
    except ValueError as err:
        _quantity_refused(unread, POSITIONS, str(err), reasons)
        return
    if count == 0:
        return  # an empty holding is stated by absence
    if (row.sec_type != "STK" or row.currency != BASE_CURRENCY or row.symbol in positions):
        unread.add(POSITIONS)
        return
    positions[row.symbol] = shares(count, scale) if scale else count


def _take_order(orders, account, unread, order_id, contract, order, order_state, scale=0, reasons=None):
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
    try:
        count = units(record.quantity, scale, "open order " + record.symbol)
    except ValueError as err:
        _quantity_refused(unread, OPEN_ORDERS, str(err), reasons)
        return
    if status not in OPEN_STATUS or side is None or count <= 0:
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
        "remaining_quantity": count,
    }


def read_marks(conn, con_ids):
    """IBKR's mark for each symbol of con_ids (symbol to IBKR's contract ID): {symbol: Decimal}.

    One market-data snapshot per contract, asked MARK_BATCH at a time (in
    symbol order), each batch together and waited for up
    to the configuration's mark_timeout, which outlasts the 11-second window
    at whose end IBKR ends a snapshot (config.MARK_TIMEOUT). A symbol has a mark only when
    its request completed (IBKR ended its snapshot) and every LAST price it
    carried was finite and positive, the mark being the last of them. A
    request refused (IBKR's error under it, such as no market-data
    permission), one that ended with no LAST price or carried any price
    that is not finite and positive, and one not ended in time (cancelled)
    give no mark: an uncertain price is never stated as the mark (ADR 0019).
    Nothing here raises: with the link down there are no marks, not even a
    batch's read before it went.
    """
    symbols = sorted(con_ids)
    prices = {}
    for start in range(0, len(symbols), MARK_BATCH):
        batch = {s: con_ids[s] for s in symbols[start:start + MARK_BATCH]}
        prices.update(_read_mark_batch(conn, batch))
        if not conn.connected:
            return {}
    return prices


def _read_mark_batch(conn, con_ids):
    """read_marks for one batch of at most MARK_BATCH contracts."""
    if not conn.connected or not con_ids:
        return {}
    asked = {}
    for symbol in sorted(con_ids):
        req_id = conn.new_req_id()
        asked[req_id] = symbol
        conn.paced(conn.api.req_mkt_data, req_id, SimpleNamespace(
            conId=con_ids[symbol], symbol=ibkr_symbol(symbol), secType=SEC_TYPE, currency=CURRENCY, exchange=EXCHANGE))
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
                conn.paced(conn.api.cancel_mkt_data, req_id)
    if not conn.connected:
        return {}
    return {asked[r]: prices[r] for r in sorted(ended - failed) if r in prices}


Contract = namedtuple("Contract", "symbol con_id exchange primary_exchange currency")
"""One IBKR contract a name resolved to (resolve_contracts): IBKR's conId, the SMART routing it was asked on, its
primary exchange and currency."""

# How many contract-details requests are outstanding at once. Their rate is
# the connection's shared pacer's (Connection.paced: under IBKR's 50
# messages a second, cancels included), so a batch answered at once and the
# next one never make a burst together.
CONTRACT_BATCH = 40
# How many times a name whose answer did not end is asked: once, then once
# more after the rest of the batch. An answer IBKR has not finished is not a
# refusal (IBKR's error, such as 200, which is final and never asked again),
# so a slow Gateway does not leave a pick without a mark; one that does not
# end the second time gets none (ADR 0025, amended 2026-10-29).
CONTRACT_ASKS = 2


def resolve_contracts(conn, symbols, configured):
    """(contracts, refused) for symbols, each resolved by name with IBKR's reqContractDetails; None if the link went.

    ADR 0026 point 3 and ADR 0025 (amended 2026-10-29): an IRA's mark states
    the IBKR contract it is for, and a stock newly picked is in no
    configuration, so its contract is looked up by name, in IBKR's spelling
    (spelling.ibkr_symbol), as a US-dollar stock on SMART. contracts maps a symbol to
    its Contract when IBKR answered with exactly one contract, and that
    contract is the name's own: IBKR's symbol and local symbol are the
    spelling asked, a stock (STK), in US dollars, on SMART, with a positive
    conId and a primary exchange, and the conId configured for it
    (configured, config.con_ids) when there is one. refused maps every other
    symbol to why: IBKR's error under the request (200, no security
    definition; final for this reading attempt), an answer that did not end within
    the request_timeout when asked CONTRACT_ASKS times, more than one
    contract (ambiguous: never a guess), one that is not the name's own, or
    one that contradicts the configured conId. A refused name is given no
    mark, so the engine refuses any order for it (missing-mark), never sizes
    it on another contract. Every request goes through the connection's
    pacer. A link that changed during the requests gives None: nothing read
    on it stands (ADR 0019, one cutoff), and the caller waits for it to be
    reconciled.
    """
    contracts, refused = {}, {}
    changes = conn.link_changes
    pending = sorted(set(symbols))
    for ask in range(1, CONTRACT_ASKS + 1):
        unended = []
        for start in range(0, len(pending), CONTRACT_BATCH):
            answered = _ask_contracts(conn, pending[start:start + CONTRACT_BATCH], changes)
            if answered is None:
                return None
            for symbol, (state, value) in answered.items():
                if state == "unended":
                    unended.append(symbol)
                elif state == "error":
                    code, message = value
                    refused[symbol] = "IBKR answered its contract request with error {}: {}".format(
                        code, mask_accounts(str(message), conn.config.account_id))
                else:
                    contract, why = _one_contract(symbol, value, configured.get(symbol))
                    if contract is None:
                        refused[symbol] = why
                    else:
                        contracts[symbol] = contract
        pending = unended
        if not pending:
            break
    for symbol in pending:
        refused[symbol] = "IBKR's answer to its contract request did not end within {}s, asked {}".format(
            conn.config.request_timeout, {2: "twice"}.get(CONTRACT_ASKS, "{} times".format(CONTRACT_ASKS)))
    return contracts, refused


def _ask_contracts(conn, names, changes):
    """{symbol: (state, value)} for one batch of contract lookups: ("answered", details), ("error", (code, message))
    or ("unended", None); None if the link went or changed (see resolve_contracts)."""
    if not conn.connected or conn.link_changes != changes:
        return None
    asked, answers, ended, errors = {}, {}, set(), {}
    try:
        for symbol in names:
            req_id = conn.new_req_id()
            asked[req_id] = symbol
            answers[req_id] = []
            conn.paced(conn.api.req_contract_details, req_id, SimpleNamespace(
                symbol=ibkr_symbol(symbol), secType=CONTRACT_SEC_TYPE, currency=CONTRACT_CURRENCY,
                exchange=CONTRACT_EXCHANGE))
            if not conn.connected:
                return None
        for event in conn.events(conn.config.request_timeout):
            if not conn.connected:
                break
            req_id = event.args[0] if event.args else None
            if not isinstance(req_id, int) or isinstance(req_id, bool) or req_id not in asked or req_id in ended:
                continue
            if event.name == "contract_details" and len(event.args) >= 2:
                answers[req_id].append(event.args[1])
            elif event.name == "contract_details_end":
                ended.add(req_id)
            elif event.name == "error":
                errors[req_id] = (tuple(event.args[1:3]) + ("", ""))[:2]
                ended.add(req_id)
            if len(ended) == len(asked):
                break
    except NotConnected:
        return None
    if not conn.connected or conn.link_changes != changes:
        return None
    out = {}
    for req_id, symbol in sorted(asked.items()):
        if req_id in errors:
            out[symbol] = ("error", errors[req_id])
        elif req_id not in ended:
            out[symbol] = ("unended", None)
        else:
            out[symbol] = ("answered", answers[req_id])
    return out


def _one_contract(symbol, answers, configured):
    """(Contract, "") for the one answer that is symbol's own US-dollar stock on SMART, else (None, why).

    The answer's symbol and local symbol must both be the name in IBKR's
    spelling (ibkr_symbol): a class share asked as "BRK B" is taken only from
    an answer IBKR spells "BRK B" itself, and is recorded under the data
    job's name, BRK.B.
    """
    if len(answers) != 1:
        return None, "IBKR gave {} contracts for it, not one{}".format(
            len(answers), " (ambiguous: none is taken)" if answers else "")
    details = answers[0]
    c = getattr(details, "contract", None)
    con_id = getattr(c, "conId", None)
    fields = {name: getattr(c, name, None) for name in ("symbol", "localSymbol", "secType", "currency", "exchange",
                                                         "primaryExchange")}
    spelled = ibkr_symbol(symbol)
    if fields["symbol"] != spelled:
        return None, "IBKR's contract is for {!r}, not {!r}".format(fields["symbol"], spelled)
    if fields["localSymbol"] != spelled:
        return None, "IBKR's contract's local symbol is {!r}, not {!r}".format(fields["localSymbol"], spelled)
    if (fields["secType"], fields["currency"], fields["exchange"]) != (
            CONTRACT_SEC_TYPE, CONTRACT_CURRENCY, CONTRACT_EXCHANGE):
        return None, "IBKR's contract is a {} {} contract on {}, not a {} {} contract on {}".format(
            fields["currency"], fields["secType"], fields["exchange"], CONTRACT_CURRENCY, CONTRACT_SEC_TYPE,
            CONTRACT_EXCHANGE)
    if not isinstance(con_id, int) or isinstance(con_id, bool) or con_id <= 0:
        return None, "IBKR's contract has no positive conId ({!r})".format(con_id)
    if not isinstance(fields["primaryExchange"], str) or not fields["primaryExchange"]:
        return None, "IBKR's contract names no primary exchange"
    if configured is not None and configured != con_id:
        return None, "IBKR resolved it to conId {}, but the configuration names {} (con_ids)".format(con_id, configured)
    return Contract(symbol, con_id, fields["exchange"], fields["primaryExchange"], fields["currency"]), ""


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
            "instrument_id": symbol, "quantity": units(reading.positions[symbol], reading.quantity_scale),
            "mark": 0 if mark is None else float(to_decimal(mark, "mark")), "mark_set": mark is not None,
        })
    return _scaled_record(BROKER_SNAPSHOT_TYPE, BROKER_SNAPSHOT_VERSION, 3, reading, {
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


def account_snapshot_record(reading, as_of, net_liquidation=None, version=ACCOUNT_SNAPSHOT_VERSION):
    """The engine's account.snapshot of reading, as of as_of: version 3 positions, version 4 cash, or D7-F version 6.

    Positions and net liquidation must have been read in full: an unavailable balance is not zero
    (ADR 0019), so a reading without them gives no snapshot (ValueError).
    net_liquidation, for a reading as of the close, is its valuation at the
    close (close_valuation; ADR 0019, amended 2026-10-18); without it the
    snapshot states IBKR's own at the read. Version 4 (an IRA's momentum
    engine, ADR 0026 point 2) adds IBKR's settled cash and total cash from
    the same reading, and needs both: a reading missing either gives no
    snapshot at all, never one with a zero or with only one of them. Version 6
    uses the same cash fields with integer ten-thousandths and quantity_scale
    10000 (ADR 0015; ADR 0026 D7-F).
    """
    if version not in (ACCOUNT_SNAPSHOT_VERSION, ACCOUNT_SNAPSHOT_CASH_VERSION, 6):
        raise ValueError("account.snapshot version {} is not one the adapter writes".format(version))
    if (version == 6) != (reading.quantity_scale == 10000):
        raise ValueError("account snapshot version and reading quantity scale policy disagree")
    needed = {POSITIONS, NET} | ({CASH, SETTLED} if version in (ACCOUNT_SNAPSHOT_CASH_VERSION, 6) else set())
    if reading.unread & needed:
        raise ValueError("an account snapshot at version {} needs {}, and {} could not be read".format(
            version, ", ".join(sorted(needed)), ", ".join(sorted(reading.unread & needed))))
    if version in (ACCOUNT_SNAPSHOT_CASH_VERSION, 6) and not {TOTAL_CASH, SETTLED_CASH} <= set(reading.balances):
        raise ValueError("an account snapshot at version {} needs IBKR's settled and total cash, and the reading "
                         "holds {}".format(version, ", ".join(sorted(reading.balances)) or "no balance"))
    balance = reading.balances[NET_LIQUIDATION]
    payload = {
        "account_id": reading.account_id,
        "as_of": _stamp(as_of),
        "net_liquidation": float(balance.amount if net_liquidation is None else net_liquidation),
        "currency": balance.currency,
        "positions": [{"instrument_id": s, "quantity": units(reading.positions[s], reading.quantity_scale)}
                      for s in sorted(reading.positions)],
    }
    if version in (ACCOUNT_SNAPSHOT_CASH_VERSION, 6):
        payload["settled_cash"] = float(reading.balances[SETTLED_CASH].amount)
        payload["total_cash"] = float(reading.balances[TOTAL_CASH].amount)
    if reading.quantity_scale:
        payload["quantity_scale"] = reading.quantity_scale
    return Record(ACCOUNT_SNAPSHOT_TYPE, version, payload)


def mark_record(symbol, as_of, price, source, broker_last=None, contract=None):
    """The market.mark.recorded of symbol at as_of: price, the delivered official close; version 2, or 3 with contract.

    source names who reported the price (the data job); broker_last is
    IBKR's last price beside it, None when IBKR gave none. Nothing is sized
    from it, but the engine's price band compares a checked order's
    reference price with it when it is set, and with price otherwise (ADR
    0026 point 3, amended 2026-10-18). contract, a Contract that
    resolve_contracts gave for symbol, makes it version 3: the IBKR contract
    the mark is for (its conId, the SMART routing it was resolved on, its
    primary exchange and currency), recorded in the journal with it.
    """
    price = to_decimal(price, "mark")
    if not price > 0:
        raise ValueError("a mark is a positive price")
    payload = {
        "instrument_id": symbol,
        "as_of": _stamp(as_of),
        "price": float(price),
        "source": source,
        "broker_last": 0 if broker_last is None else float(to_decimal(broker_last, "broker last")),
        "broker_last_set": broker_last is not None,
    }
    if contract is None:
        return Record(MARK_TYPE, MARK_VERSION, payload)
    if contract.symbol != symbol:
        raise ValueError("the contract resolved for {} is not {}'s".format(contract.symbol, symbol))
    payload["contract"] = {"con_id": contract.con_id, "exchange": contract.exchange,
                           "primary_exchange": contract.primary_exchange, "currency": contract.currency}
    return Record(MARK_TYPE, MARK_CONTRACT_VERSION, payload)


def mark_unavailable_record(symbol, as_of, detail):
    """Record why a name has no mark, retaining the adapter's exact resolution evidence.

    ADR 0026, amendment of 2026-10-29: a missing mark withholds one pick
    and pages with whether resolution timed out or definitively failed.
    Only resolve_contracts' own timeout detail means transient; an IBKR
    error message mentioning a timeout is still a final IBKR refusal.
    """
    transient = detail.startswith("IBKR's answer to its contract request did not end within ")
    return Record("market.mark.unavailable", 1, {
        "instrument_id": symbol, "as_of": _stamp(as_of), "source": "ibkr",
        "detail": detail, "failure": "transient" if transient else "final",
    })


def reconciliation_record(reading, reconciliation_id, trigger=MORNING):
    """The account.reconciliation of reading: its positions and total cash, for the engine to classify."""
    if trigger != MORNING:
        raise ValueError("trigger {!r} is not {!r}".format(trigger, MORNING))
    if not reconciliation_id:
        raise ValueError("a reconciliation names its id")
    cash_read = CASH not in reading.unread
    return _scaled_record(RECONCILIATION_TYPE, RECONCILIATION_VERSION, 2, reading, {
        "account_id": reading.account_id,
        "reconciliation_id": reconciliation_id,
        "trigger": trigger,
        "as_of": _stamp(reading.as_of),
        "unreadable": sorted(reading.unread & {POSITIONS, CASH}),
        "positions": [{"instrument_id": s, "quantity": units(reading.positions[s], reading.quantity_scale)}
                      for s in sorted(reading.positions)],
        "total_cash": float(reading.balances[TOTAL_CASH].amount) if cash_read else 0,
        "currency": reading.balances[TOTAL_CASH].currency if cash_read else "",
    })


def _quantity_refused(unread, component, reason, reasons):
    """Retain why broker quantities cannot be represented (ADR 0019; ADR 0026 D7)."""
    unread.add(component)
    log.warning("ibkr %s unreadable: %s", component, reason)
    if reasons is not None:
        reasons.append(component + ": " + reason)


def _scaled_record(kind, legacy, fractional, reading, payload):
    """Select the engine's explicit quantity schema (ADR 0015; ADR 0026 D7-F)."""
    if reading.quantity_scale:
        payload["quantity_scale"] = reading.quantity_scale
    return Record(kind, fractional if reading.quantity_scale else legacy, payload)
