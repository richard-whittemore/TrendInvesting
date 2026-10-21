"""The Flex cash-movement reader: IBKR's Cash Transactions, read once a day, each transfer into the instance's ledger.

`python3 -m adapter.ibkr flex ...` (cli.py), run by ti-flex@<instance>.timer
after the close (docs/manual/reconciliation.md, "Deposits and withdrawals,
read from IBKR"). One reading:

1. asks IBKR's Flex Web Service for the owner's saved Flex query
   (SendRequest, then GetStatement, version 3), the token and query ID read
   from the instance's secrets and sent to an interactivebrokers.com address
   only, never written to a log, an error or a file;
2. reads the statement's Cash Transactions section into typed postings
   (parse_statement): deposits and withdrawals, dividends, broker interest
   paid and received, commission adjustments, other fees, each with IBKR's
   transaction ID, date, amount, currency and type. A statement whose
   account is not the instance's, that has no Cash Transactions section, or
   that is not as IBKR writes one is refused whole;
3. writes one account.cash-movement, version 2 (internal/event
   CashMovementTransferVersion; ADR 0007, ADR 0015), per deposit or
   withdrawal not yet in the instance's ledger, with the equity before it
   (equity_before), found back through the chain of runs (relay.run_chain).
   The first book is the starting book the owner states at the first run's
   approval (ADR 0027, starting-book amendment), so the approval's
   approved_at is the cutoff: a transfer dated before it is in that book and
   is not written (counted as in_first_book); one at or after it is always
   written, or the reading is set aside. A transfer IBKR dates by day alone
   on the approval's own New York day is taken as after it (counted as
   date_only_on_approval_day) when the approval came before that session's
   official close: the owner read the book before the day's business, so the
   book cannot hold it. After the close (an evening approval, or a day with
   no session) the day's business may be in the book or not: the reading is
   set aside as an ambiguous transfer (state "ambiguous-transfer", the
   transaction and its amount named) until the owner records which side it
   is on (record_resolutions, `flex --resolve ID=in-first-book` or
   `=after-approval`; the RESOLUTIONS_SUFFIX file beside the ledger), which
   every later reading honours (counted as resolved). Postings other than
   transfers are
   parsed, and kept on the Statement, but not written; a type KINDS does not
   know is counted in the status file (unknown_types) and said.

Each reading's period is kept beside the ledger (COVERAGE_SUFFIX): its
fromDate and covered_to, the day before its toDate. The last day of a
statement is not complete when it is read (IBKR may post a day's transfer
after its own end-of-day processing, after the evening's reading), so it is
covered only once a later statement reads it again; the ledger's IDs keep a
transfer read twice from being written twice. A statement that starts later
than the day after covered_to leaves days in neither, and a transfer on them
would be lost when it drops out of the query's window: the reading is
refused as a coverage gap (state "coverage-gap", both days named), nothing
is written, and the kept period is not moved on. With no period kept (the
first reading, or the coverage file lost), the statement must start on or
before the New York day of the first run's approval, since every transfer
from that day on is to be written; otherwise it is a coverage gap naming
both days, read again by a statement whose period reaches back to it.

The equity before a transfer (equity_before) is, in order:

- the net liquidation of the last close snapshot before it, plus the
  transfers between that snapshot and it (a close snapshot is read after
  the close and holds that day's later transfers; ADR 0019, "the reading
  after the close counts as the close");
- for a transfer timed after the close, that day's close snapshot less the
  transfers of that evening from it on, itself included (that snapshot,
  read later, already holds it): equity_before_basis
  "same-day-snapshot-less-amount";
- for a transfer in the first run's window, before any snapshot, the first
  snapshot after it less the transfers up to that snapshot, itself
  included: equity_before_basis "next-snapshot-less-amount". The market's
  move between the transfer and that snapshot is in the figure; a transfer
  is never left unwritten for it (the shadow account that reads the ledger
  needs its amount and date). With no snapshot yet, the reading is set
  aside until the first close snapshot is committed.

Both approximations assume the close snapshot was read after the transfer,
as the post-close reading is (17:05 New York or later); a transfer IBKR
times later that evening is taken as held by it. A close snapshot is one
stamped as of its session's official close (snapshot.py: 4 p.m., 1 p.m. on
an early close), found by the data job's NYSE calendar, the one the adapter
stamps by (adapter/marketdata/nyse_calendar.py).

A record once written is never changed. A transfer IBKR adds a reading late
can fall in the window an already-written figure counts (a later transfer's
figure adds those since its snapshot; an earlier one's takes those from it
on, up to its snapshot's reading, off that snapshot, whatever the basis of
each): it is written,
and its record says so (note, LATER_WRITTEN_FIRST or
EARLIER_WRITTEN_FIRST; counted as out_of_order), the earlier-written
record's equity_before not revised.

The ledger is the instance's own file of cash movements, one JSON record a
line, beside its status file (STATUS_SUFFIX). It is the seen-IDs state: a
record's ID is ID_PREFIX and IBKR's transaction ID, so a transfer already
in it is never written again, however often its statement is read, and one
IBKR restates with another amount, date or currency refuses the reading
rather than being written twice or silently changed. It is not the engine's
durable log: the engine writes that alone (ADR 0027), and nothing the
engine decides reads a cash movement, so the reader cannot change a
decision. The cash-drift check (ADR 0019) stays the cross-check: a transfer
the reader missed still shows as drift.

Fail closed (AGENTS.md): any refusal, a token IBKR rejects or a service
error sets the reader aside for this reading. Nothing is written to the
ledger, the status file records "set-aside" with why (and IBKR's error
code), and read_once returns False, which the command turns into exit
status 1, a failed unit. The next firing reads again; a good reading
clears it. The engine, the adapter and the trading loop do not depend on
it. Standard library only, and the adapter's own reader of the engine's
durable log (relay.py).
"""
import fcntl
import json
import math
import os
import re
import time
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ElementTree
from collections import namedtuple
from datetime import datetime, time as clock, timedelta, timezone
from decimal import Decimal, InvalidOperation
from zoneinfo import ZoneInfo

from relay import LOG_INPUT, RUN_APPROVED, ResolveRefused, marketdata_module, run_chain

nyse_calendar = marketdata_module("nyse_calendar")

CASH_MOVEMENT = ("account.cash-movement", 2)   # internal/event CashMovementTransferVersion
ACCOUNT_SNAPSHOT = ("account.snapshot", 3)      # internal/event AccountSnapshotPositionsVersion
FIRST_RUN_APPROVAL_VERSION = 3                  # the operator.run.approved that states the starting book
# internal/event EquityBeforeNextSnapshotLessAmount and EquityBeforeSameDaySnapshotLessAmount.
NEXT_SNAPSHOT_LESS_AMOUNT = "next-snapshot-less-amount"
SAME_DAY_SNAPSHOT_LESS_AMOUNT = "same-day-snapshot-less-amount"
BASES = (NEXT_SNAPSHOT_LESS_AMOUNT, SAME_DAY_SNAPSHOT_LESS_AMOUNT)
SOURCE = "ibkr-flex"
ID_PREFIX = "ibkr-flex:"
STATUS_SUFFIX = ".status.json"
COVERAGE_SUFFIX = ".coverage.json"
COVERAGE_FIELDS = ("from_date", "covered_to")
# The owner's decisions on transfers dated by day alone on an evening approval's day: {transaction ID: decision}.
RESOLUTIONS_SUFFIX = ".resolutions.json"
IN_FIRST_BOOK = "in-first-book"
AFTER_APPROVAL = "after-approval"
DECISIONS = (IN_FIRST_BOOK, AFTER_APPROVAL)
LOCK_SUFFIX = ".lock"

# IBKR's Flex Web Service, version 3. GetStatement's address comes from
# SendRequest's answer; the token is sent only to an https address on
# IBKR_HOST, and a redirect is never followed (_NoRedirect): the token rides
# in the query string, which a redirect would re-send to its Location.
SEND_REQUEST_URL = "https://ndcdyn.interactivebrokers.com/AccountManagement/FlexWebService/SendRequest"
FLEX_VERSION = "3"
IBKR_HOST = re.compile(r"(?:[a-z0-9-]+\.)*interactivebrokers\.com")
USER_AGENT = "TrendInvesting-flex-reader/1"
REQUEST_TIMEOUT = 60
# A statement IBKR is still generating (code 1019) is asked for again every
# GENERATION_WAIT seconds, GENERATION_ATTEMPTS times in all. With each
# request taking up to REQUEST_TIMEOUT, a reading's worst case is SendRequest
# and every attempt to their timeout plus the waits (about 36 minutes);
# ti-flex@.service's TimeoutStartSec is above it, and a reading stopped
# anyway leaves its status file saying "running" (read_once).
GENERATION_WAIT = 10
GENERATION_ATTEMPTS = 30
GENERATING = "1019"
# IBKR's error codes that say the token, its account or the query is not
# accepted: the owner's setup must change, so the reading is set aside as
# rejected. Every other code is the service's own (busy, not ready, a
# statement that could not be generated): set aside as unavailable.
REJECTED_CODES = frozenset(("1010", "1011", "1012", "1013", "1014", "1015", "1016", "1020"))

# A posting is dated by its dateTime alone, the only date field read (a
# report date or settle date is not when the cash moved). IBKR's
# documentation gives yyyyMMdd, or yyyyMMdd;HHmmss with a time, as the Flex
# query's default formats, which the owner keeps. That it is New York time
# is an assumption until the first live statement is checked against the
# owner's activity statement (docs/manual/reconciliation.md). A date alone
# is taken as the start of that New York day, and the posting is marked as
# dated by day alone (Posting.dated_only).
NEW_YORK = ZoneInfo("America/New_York")
FLEX_DATE = re.compile(r"([0-9]{4})([0-9]{2})([0-9]{2})(?:;([0-9]{2})([0-9]{2})([0-9]{2}))?")
FLEX_DAY = re.compile(r"[0-9]{8}")

# The kinds of posting, from IBKR's Cash Transactions type. Only TRANSFER is
# written; the others are kept on the Statement for a costs ledger.
TRANSFER = "transfer"
DIVIDEND = "dividend"
WITHHOLDING_TAX = "withholding-tax"
INTEREST_PAID = "interest-paid"
INTEREST_RECEIVED = "interest-received"
COMMISSION = "commission"
FEE = "fee"
OTHER = "other"
KINDS = {
    "Deposits/Withdrawals": TRANSFER,
    "Deposits & Withdrawals": TRANSFER,
    "Dividends": DIVIDEND,
    "Payment In Lieu Of Dividends": DIVIDEND,
    "Withholding Tax": WITHHOLDING_TAX,
    "Broker Interest Paid": INTEREST_PAID,
    "Broker Interest Received": INTEREST_RECEIVED,
    "Commission Adjustments": COMMISSION,
    "Other Fees": FEE,
    "Advisor Fees": FEE,
}

Posting = namedtuple("Posting", "transaction_id kind ibkr_type as_of amount currency dated_only")
"""One Cash Transactions row: IBKR's transaction ID, the kind (KINDS, else OTHER), IBKR's own type, when (UTC,
a whole second), the amount (a Decimal, signed as IBKR states it), its currency, and whether IBKR dated it by day
alone (as_of then the start of that New York day)."""

Statement = namedtuple("Statement", "account_id from_date to_date postings")
"""The instance's Flex statement: its account, the period IBKR states (yyyyMMdd, from_date not after to_date), and
every detail posting in order."""

ACCOUNT_NUMBER = re.compile(r"\b[A-Z]{1,2}[0-9]{5,9}\b")
RECORD_FIELDS = ("id", "type", "schema_version", "event_time", "source", "payload")
OPTIONAL_RECORD_FIELDS = ("note",)
# A record's note, beside the engine's payload (internal/event CashMovementPayload, which has no field for it):
# the transfer was added a reading late, after a record from the same snapshot whose figure it belongs in.
LATER_WRITTEN_FIRST = ("a later transfer in the same interval was written first; its equity_before omits this one "
                       "and is not revised")
EARLIER_WRITTEN_FIRST = ("an earlier transfer in the same interval was written first; its equity_before omits this "
                         "one and is not revised")
NOTES = (LATER_WRITTEN_FIRST, EARLIER_WRITTEN_FIRST)
PAYLOAD_FIELDS = ("as_of", "amount", "equity_before", "currency", "transaction_id")
OPTIONAL_PAYLOAD_FIELDS = ("equity_before_basis",)


class StatementRefused(ValueError):
    """The statement, or the ledger beside it, cannot be read as written; nothing of it is taken."""


class CoverageGap(StatementRefused):
    """The statement starts later than the day after the last accepted reading's covered_to (or, with none kept,
    after the first run's approval day): days are in neither."""


class AmbiguousTransfer(StatementRefused):
    """A transfer dated by day alone on the day of a first-run approval given after that session's close, with no
    decision recorded by the owner: whether the starting book holds it is not known."""

    def __init__(self, message, ids=()):
        super().__init__(message)
        self.ids = tuple(ids)


class FlexRejected(Exception):
    """IBKR rejected the token, its account or the query: code and IBKR's message, never the token."""

    def __init__(self, code, message):
        super().__init__("IBKR's Flex Web Service rejected the token or the query (error {}: {})".format(
            code, message))
        self.code = code


class FlexUnavailable(Exception):
    """IBKR's Flex Web Service did not give the statement: code (None for a network failure) and why."""

    def __init__(self, code, why):
        super().__init__("IBKR's Flex Web Service gave no statement ({}{})".format(
            "error {}: ".format(code) if code else "", why))
        self.code = code


def mask(text, *secrets):
    """text with each secret, and anything shaped like an IBKR account number, replaced: errors are pasted."""
    for secret in secrets:
        if secret:
            text = text.replace(secret, "<secret>")
    return ACCOUNT_NUMBER.sub("<account>", text)


def _flex_time(text, where):
    match = FLEX_DATE.fullmatch(text or "")
    if not match:
        raise StatementRefused("{} has the date {!r}, not yyyyMMdd or yyyyMMdd;HHmmss (keep the Flex query's "
                               "default date and time formats)".format(where, text))
    parts = [int(p) for p in match.groups() if p is not None]
    try:
        local = datetime(*parts, tzinfo=NEW_YORK)
    except ValueError:
        raise StatementRefused("{} has the date {!r}, which is not a date".format(where, text)) from None
    return local.astimezone(timezone.utc), len(parts) == 3


def _flex_day(text):
    """The date of a yyyyMMdd text, or None."""
    if not FLEX_DAY.fullmatch(text or ""):
        return None
    try:
        return datetime.strptime(text, "%Y%m%d").date()
    except ValueError:
        return None


def parse_statement(data, account_id):
    """The Statement for account_id in the Flex query response data (bytes); StatementRefused otherwise."""
    try:
        root = ElementTree.fromstring(data)
    except ElementTree.ParseError:
        raise StatementRefused("the Flex statement is not XML as IBKR writes it") from None
    if root.tag != "FlexQueryResponse":
        raise StatementRefused("the Flex answer is a {}, not a statement (FlexQueryResponse)".format(root.tag))
    statements = root.findall("./FlexStatements/FlexStatement")
    if not statements:
        raise StatementRefused("the Flex statement holds no FlexStatement")
    mine = [s for s in statements if s.get("accountId") == account_id]
    if not mine:
        raise StatementRefused("the Flex statement is for {} account(s), not this instance's account; the query "
                               "must cover this instance's account".format(len(statements)))
    if len(mine) > 1:
        raise StatementRefused("the Flex statement holds this instance's account {} times".format(len(mine)))
    statement = mine[0]
    section = statement.find("CashTransactions")
    if section is None:
        raise StatementRefused("the Flex statement has no Cash Transactions section; add it to the Flex query in "
                               "Client Portal (docs/manual/reconciliation.md)")
    postings, seen = [], set()
    for n, row in enumerate(section.findall("CashTransaction"), 1):
        where = "Cash Transactions row {}".format(n)
        if row.get("levelOfDetail", "DETAIL") not in ("DETAIL", ""):
            continue  # a SUMMARY row totals the detail rows beside it
        if row.get("accountId", account_id) != account_id:
            raise StatementRefused("{} is for another account".format(where))
        transaction_id = row.get("transactionID") or ""
        if not transaction_id.strip() or transaction_id != transaction_id.strip():
            raise StatementRefused("{} has no transaction ID".format(where))
        if transaction_id in seen:
            raise StatementRefused("the Flex statement states transaction {} twice".format(transaction_id))
        seen.add(transaction_id)
        ibkr_type = row.get("type") or ""
        if not ibkr_type:
            raise StatementRefused("{} (transaction {}) has no type".format(where, transaction_id))
        currency = row.get("currency") or ""
        if not re.fullmatch(r"[A-Z]{3}", currency):
            raise StatementRefused("{} (transaction {}) has no currency".format(where, transaction_id))
        try:
            amount = Decimal(row.get("amount") or "")
        except InvalidOperation:
            raise StatementRefused("{} (transaction {}) has an amount that is not a number".format(
                where, transaction_id)) from None
        if not amount.is_finite():
            raise StatementRefused("{} (transaction {}) has an amount that is not finite".format(where, transaction_id))
        as_of, dated_only = _flex_time(row.get("dateTime"), "{} (transaction {})".format(where, transaction_id))
        postings.append(Posting(transaction_id, KINDS.get(ibkr_type, OTHER), ibkr_type, as_of, amount, currency,
                                dated_only))
    from_date, to_date = statement.get("fromDate"), statement.get("toDate")
    first, last = _flex_day(from_date), _flex_day(to_date)
    if first is None or last is None or first > last:
        raise StatementRefused("the Flex statement does not state its period as fromDate and toDate (yyyyMMdd, the "
                               "first not after the last), so the days it covers cannot be checked")
    return Statement(account_id, from_date, to_date, postings)


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """Refuses every redirect: urllib then raises the 3xx as an HTTPError, an OSError, so the reading is set aside."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


_OPENER = urllib.request.build_opener(_NoRedirect)


def _get(url, params):
    """The body IBKR answers a GET of url with params, never following a redirect; OSError (urllib's) on any
    failure."""
    request = urllib.request.Request(url + "?" + urllib.parse.urlencode(params), headers={"User-Agent": USER_AGENT})
    with _OPENER.open(request, timeout=REQUEST_TIMEOUT) as response:
        return response.read()


def _answer(data, step):
    """(status, code, message, element) of a FlexStatementResponse; None when data is not one."""
    try:
        root = ElementTree.fromstring(data)
    except ElementTree.ParseError:
        raise FlexUnavailable(None, "its {} answer is not XML".format(step)) from None
    if root.tag != "FlexStatementResponse":
        return None
    return ((root.findtext("Status") or "").strip(), (root.findtext("ErrorCode") or "").strip(),
            (root.findtext("ErrorMessage") or "").strip(), root)


def _failed(code, message):
    if code in REJECTED_CODES:
        return FlexRejected(code, message)
    return FlexUnavailable(code or None, message or "no reason given")


def _ask(get, url, params, step, token):
    try:
        return get(url, params)
    except (OSError, ValueError) as err:  # urllib's URLError and HTTPError are OSErrors
        raise FlexUnavailable(None, "its {} could not be reached: {}".format(step, mask(
            getattr(err, "reason", None) and str(err.reason) or type(err).__name__, token))) from None


def fetch_statement(token, query_id, get=_get, sleep=time.sleep):
    """The Flex query response (bytes) for query_id; FlexRejected or FlexUnavailable otherwise, never quoting token."""
    params = {"t": token, "q": query_id, "v": FLEX_VERSION}
    answer = _answer(_ask(get, SEND_REQUEST_URL, params, "SendRequest", token), "SendRequest")
    if answer is None:
        raise FlexUnavailable(None, "its SendRequest answer is not a FlexStatementResponse")
    status, code, message, root = answer
    if status != "Success":
        raise _failed(code, mask(message, token))
    reference = (root.findtext("ReferenceCode") or "").strip()
    url = (root.findtext("Url") or "").strip()
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme != "https" or not IBKR_HOST.fullmatch(parsed.hostname or "") or parsed.query:
        raise FlexUnavailable(None, "its SendRequest answer names a statement address that is not IBKR's, so the "
                                    "token is not sent there")
    if not reference.isdigit():
        raise FlexUnavailable(None, "its SendRequest answer has no reference code")
    for attempt in range(GENERATION_ATTEMPTS):
        data = _ask(get, url, {"t": token, "q": reference, "v": FLEX_VERSION}, "GetStatement", token)
        answer = _answer(data, "GetStatement")
        if answer is None:
            return data
        _, code, message, _ = answer
        if code != GENERATING:
            raise _failed(code, mask(message, token))
        if attempt + 1 < GENERATION_ATTEMPTS:
            sleep(GENERATION_WAIT)
    raise FlexUnavailable(GENERATING, "the statement was still being generated after {} seconds".format(
        GENERATION_WAIT * GENERATION_ATTEMPTS))


def _stamp(moment):
    return moment.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _parse_stamp(text):
    try:
        return datetime.strptime(text, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
    except (TypeError, ValueError):
        return None


def read_chain(engine_log_path):
    """(approved_at, snapshots) of the chain of runs: the first run's approval time, the starting book's cutoff, and
    [(as_of, net liquidation, currency)] of every committed account snapshot, in time order.

    StatementRefused for an account snapshot at a version the reader does not know (ADR 0015: a version is read
    only as it is stated, never skipped as if absent), one without what the reader needs, or a chain that ends
    without a first run's approval stating the starting book."""
    found, first = [], None
    for path, groups, approval in run_chain(engine_log_path):
        # The walk ends at the first run, whose approval names no run before it and states the starting book; a
        # fresh run's approval may restate a book, but its time is not the first book's.
        first = None
        for group in groups:
            for _, kind, envelope in group:
                if kind != LOG_INPUT:
                    continue
                if (envelope["type"] == RUN_APPROVED and isinstance(approval, dict)
                        and approval.get("previous_log") == "" and isinstance(approval.get("starting_book"), dict)):
                    first = (path, envelope)
                if envelope["type"] != ACCOUNT_SNAPSHOT[0]:
                    continue
                if envelope["schema_version"] != ACCOUNT_SNAPSHOT[1]:
                    raise StatementRefused("the account snapshot {} in {} is at version {}, which this reader does not "
                                           "read (it reads version {})".format(envelope["id"], path,
                                                                               envelope["schema_version"],
                                                                               ACCOUNT_SNAPSHOT[1]))
                payload = envelope["payload"]
                as_of = _parse_stamp(payload.get("as_of")) if isinstance(payload, dict) else None
                value = payload.get("net_liquidation") if isinstance(payload, dict) else None
                if (as_of is None or not _number(value) or value <= 0
                        or not isinstance(payload.get("currency"), str)):
                    raise StatementRefused("the account snapshot {} in {} has no as-of time, positive net "
                                           "liquidation and currency".format(envelope["id"], path))
                found.append((as_of, Decimal(repr(float(value))), payload["currency"]))
    if first is None:
        raise StatementRefused("the chain of runs from {} ends without a first run's approval stating the starting "
                               "book, so the first book's time is not known".format(engine_log_path))
    path, envelope = first
    approved_at = None
    if envelope["schema_version"] == FIRST_RUN_APPROVAL_VERSION and isinstance(
            envelope["payload"].get("approved_at"), str):
        try:
            approved_at = datetime.fromisoformat(envelope["payload"]["approved_at"].replace("Z", "+00:00"))
        except ValueError:
            approved_at = None
    if approved_at is None or approved_at.tzinfo is None:
        raise StatementRefused("the first run's approval in {} (the starting book) has no approved_at time at version "
                               "{}".format(path, FIRST_RUN_APPROVAL_VERSION))
    return approved_at, sorted(found)


def _number(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def _is_close_stamp(moment):
    """Whether moment is its New York session's official close (4 p.m., 1 p.m. on an early close), the as-of a close
    snapshot is stamped with (snapshot.py; ADR 0019, amended 2026-10-18), by the adapter's NYSE calendar."""
    local = moment.astimezone(NEW_YORK)
    try:
        if not nyse_calendar.is_trading_day(local.date()):
            return False
        return local == nyse_calendar.session_close(local.date())
    except nyse_calendar.OutOfRange:
        raise StatementRefused("the snapshot as of {} is in a year the NYSE calendar does not hold, so whether it is "
                               "a close is not known".format(_stamp(moment))) from None


def _after_the_close(moment):
    """Whether moment is at or after its New York session's official close, or on a day with no session: the day's
    business is then before it, by the adapter's NYSE calendar."""
    local = moment.astimezone(NEW_YORK)
    try:
        if not nyse_calendar.is_trading_day(local.date()):
            return True
        return local >= nyse_calendar.session_close(local.date())
    except nyse_calendar.OutOfRange:
        raise StatementRefused("the first run's approval at {} is in a year the NYSE calendar does not hold, so "
                               "whether it came after that session's close is not known".format(_stamp(moment))
                               ) from None


def _held_until(snapshot_as_of):
    """The moment up to which a snapshot's net liquidation holds every transfer: a close snapshot is read after the
    close, so it holds the rest of its New York day; any other reading holds what came before it."""
    if not _is_close_stamp(snapshot_as_of):
        return snapshot_as_of
    day = snapshot_as_of.astimezone(NEW_YORK).date() + timedelta(days=1)
    return datetime.combine(day, clock(0), tzinfo=NEW_YORK).astimezone(timezone.utc)


def _interval(snapshots, at):
    """(snapshot, basis) a transfer at the moment at takes its equity before from: the last snapshot before it (basis
    None), that day's close snapshot (SAME_DAY_SNAPSHOT_LESS_AMOUNT), or with neither the first after it
    (NEXT_SNAPSHOT_LESS_AMOUNT; snapshot None when there is none yet)."""
    before = [s for s in snapshots if s[0] < at]
    # A transfer at the close's own second is after it, as one later that evening is.
    before += [s for s in snapshots if s[0] == at and _is_close_stamp(s[0])]
    if before:
        snapshot = before[-1]
        if _is_close_stamp(snapshot[0]) and snapshot[0].astimezone(NEW_YORK).date() == at.astimezone(NEW_YORK).date():
            return snapshot, SAME_DAY_SNAPSHOT_LESS_AMOUNT
        return snapshot, None
    after = [s for s in snapshots if s[0] >= at]  # none is the close at at's own second
    return (after[0] if after else None), NEXT_SNAPSHOT_LESS_AMOUNT


def _equity_before(snapshots, transfers, posting):
    """(equity before posting, its currency, basis) from snapshots and transfers [(as_of, transaction ID, amount)];
    StatementRefused when no snapshot is committed yet."""
    key = (posting.as_of, posting.transaction_id)
    day = posting.as_of.astimezone(NEW_YORK).date()
    snapshot, basis = _interval(snapshots, posting.as_of)
    if snapshot is None:
        raise StatementRefused("transfer {}, dated {}, is after the first run's approval, but no close snapshot is "
                               "committed yet to give the equity before it; the first reading after the first close "
                               "snapshot writes it".format(posting.transaction_id, _stamp(posting.as_of)))
    as_of, value, currency = snapshot
    if basis == SAME_DAY_SNAPSHOT_LESS_AMOUNT:
        # At or after the close: that day's close snapshot, read later, already holds it.
        evening = sum((amount for at, ident, amount in transfers
                       if (at, ident) >= key and at >= as_of and at.astimezone(NEW_YORK).date() == day),
                      Decimal(0))
        return value - evening, currency, basis
    if basis is None:
        since = sum((amount for at, ident, amount in transfers if at >= _held_until(as_of) and (at, ident) < key),
                    Decimal(0))
        return value + since, currency, None
    upto = sum((amount for at, ident, amount in transfers if (at, ident) >= key and at < _held_until(as_of)),
               Decimal(0))
    return value - upto, currency, NEXT_SNAPSHOT_LESS_AMOUNT


def _record(posting, equity, currency, basis, note=None):
    payload = {"as_of": _stamp(posting.as_of), "amount": float(posting.amount), "equity_before": float(equity),
               "currency": currency, "transaction_id": posting.transaction_id}
    if basis is not None:
        payload["equity_before_basis"] = basis
    record = {"id": ID_PREFIX + posting.transaction_id, "type": CASH_MOVEMENT[0], "schema_version": CASH_MOVEMENT[1],
              "event_time": payload["as_of"], "source": SOURCE, "payload": payload}
    if note is not None:
        record["note"] = note
    return record


def _payload_as_written(payload):
    """Whether payload holds the fields _record writes, each of the type it writes: a transaction ID, an as-of
    stamp, a finite non-zero amount, a finite positive equity before that the amount leaves positive, a three-letter
    currency, and a named basis or none (internal/event CashMovementPayload.ValidateSchema at version 2)."""
    fields = set(payload) - set(OPTIONAL_PAYLOAD_FIELDS)
    if fields != set(PAYLOAD_FIELDS):
        return False
    amount, equity = payload["amount"], payload["equity_before"]
    return (isinstance(payload["transaction_id"], str) and bool(payload["transaction_id"])
            and _parse_stamp(payload["as_of"]) is not None
            and _number(amount) and amount != 0 and _number(equity) and equity > 0 and equity + amount > 0
            and isinstance(payload["currency"], str) and re.fullmatch(r"[A-Z]{3}", payload["currency"]) is not None
            and payload.get("equity_before_basis", BASES[0]) in BASES)


def read_ledger(path):
    """{record ID: record} of the ledger at path ({} when it does not exist); StatementRefused when one line is not
    a record as this reader writes it."""
    try:
        with open(path, encoding="utf-8") as f:
            lines = f.read().split("\n")
    except FileNotFoundError:
        return {}
    except OSError as err:
        raise StatementRefused("the ledger {} cannot be read: {}".format(path, err.strerror or err)) from None
    if lines[-1] != "":
        raise StatementRefused("the ledger {} does not end with a whole line".format(path))
    records = {}
    for n, line in enumerate(lines[:-1], 1):
        try:
            record = json.loads(line)
        except ValueError:
            record = None
        payload = record.get("payload") if isinstance(record, dict) else None
        if (not isinstance(record, dict) or set(record) - set(OPTIONAL_RECORD_FIELDS) != set(RECORD_FIELDS)
                or record.get("note", NOTES[0]) not in NOTES
                or (record["type"], record["schema_version"], record["source"]) != CASH_MOVEMENT + (SOURCE,)
                or not isinstance(payload, dict) or not _payload_as_written(payload)
                or record["id"] != ID_PREFIX + payload["transaction_id"] or record["id"] in records
                or record["event_time"] != payload["as_of"]):
            raise StatementRefused("the ledger {} line {} is not a cash movement as this reader writes it; it is "
                                   "never appended to until it is put back".format(path, n))
        records[record["id"]] = record
    return records


def _after_approval(posting, approved_at, resolutions):
    """(after, resolved) for posting against the first run's approval (ADR 0027, starting-book amendment), or None
    when it cannot be told: dated at or after the approval is after it; dated by day alone on the approval's New York
    day is after it when the approval came before that session's close (the book the owner read then cannot hold
    it), and otherwise on the side the owner recorded in resolutions (resolved), or not known. A recorded decision
    the statement now contradicts (the transfer is dated otherwise than when it was decided) is StatementRefused:
    the owner's word and IBKR's date cannot both stand."""
    decision = resolutions.get(posting.transaction_id)
    if posting.as_of >= approved_at:
        after, ambiguous = True, False
    elif not (posting.dated_only and posting.as_of.astimezone(NEW_YORK).date()
              == approved_at.astimezone(NEW_YORK).date()):
        after, ambiguous = False, False
    elif not _after_the_close(approved_at):
        after, ambiguous = True, False
    else:
        after, ambiguous = None, True
    if ambiguous:
        return None if decision is None else (decision == AFTER_APPROVAL, True)
    if decision is not None and (decision == AFTER_APPROVAL) != after:
        raise StatementRefused(
            "transfer {} was recorded by the owner as {}, but IBKR now dates it {}, {} the first run's approval at "
            "{}; nothing is written. Tell the developer (docs/manual/reconciliation.md)".format(
                posting.transaction_id, decision, _stamp(posting.as_of), "after" if after else "before",
                _stamp(approved_at)))
    return after, False


def _written_first(snapshots, written, posting):
    """The note for posting when it falls in the window a record already written counts in its figure (see the
    module; _equity_before), else None."""
    key = (posting.as_of, posting.transaction_id)
    day = posting.as_of.astimezone(NEW_YORK).date()
    for at, ident, _ in written:
        snapshot, basis = _interval(snapshots, at)
        if snapshot is None:
            continue
        theirs = (at, ident)
        if basis is None:
            inside = key < theirs and posting.as_of >= _held_until(snapshot[0])
        elif basis == SAME_DAY_SNAPSHOT_LESS_AMOUNT:
            inside = key > theirs and posting.as_of >= snapshot[0] and day == at.astimezone(NEW_YORK).date()
        else:
            inside = key > theirs and posting.as_of < _held_until(snapshot[0])
        if inside:
            return LATER_WRITTEN_FIRST if theirs > key else EARLIER_WRITTEN_FIRST
    return None


def new_movements(statement, ledger, approved_at, snapshots, resolutions=None):
    """(records to write, transfers not written as in the first book and why, IDs of transfers dated by day alone on
    the approval's day taken as after it, IDs of transfers placed by the owner's resolutions): each transfer of
    statement not yet in ledger and after the first run's approval (_after_approval); AmbiguousTransfer naming each
    one whose side is not known, StatementRefused when one cannot be written."""
    resolutions = resolutions or {}
    transfers = sorted((p for p in statement.postings if p.kind == TRANSFER), key=lambda p: (p.as_of, p.transaction_id))
    known = {r["payload"]["transaction_id"]: (_parse_stamp(r["payload"]["as_of"]), r["payload"]["transaction_id"],
                                              Decimal(repr(float(r["payload"]["amount"]))))
             for r in ledger.values()}
    already = sorted(known.values())
    sides = {p.transaction_id: _after_approval(p, approved_at, resolutions) for p in transfers
             if p.transaction_id not in known}
    unknown = [p for p in transfers if p.transaction_id in sides and sides[p.transaction_id] is None]
    if unknown:
        raise AmbiguousTransfer(
            "the first run was approved at {}, after that session's close, and IBKR dates {} by that day alone, so "
            "whether the starting book holds it is not known. Nothing is written. Check in IBKR when it was made, "
            "then record which side it is on: {} (docs/manual/reconciliation.md)".format(
                _stamp(approved_at),
                "; ".join("transfer {} of {} {}".format(p.transaction_id, p.amount, p.currency) for p in unknown),
                "; ".join("--resolve {0}={1} (the book you stated holds it) or --resolve {0}={2}".format(
                    p.transaction_id, IN_FIRST_BOOK, AFTER_APPROVAL) for p in unknown)),
            ids=[p.transaction_id for p in unknown])
    for posting in transfers:
        if posting.transaction_id in sides and sides[posting.transaction_id][0]:
            known[posting.transaction_id] = (posting.as_of, posting.transaction_id, posting.amount)
    moved = sorted(known.values())
    written, skipped, date_only, resolved = [], [], [], []
    for posting in transfers:
        recorded = ledger.get(ID_PREFIX + posting.transaction_id)
        if recorded is not None:
            stated = recorded["payload"]
            if (Decimal(repr(float(stated["amount"]))) != posting.amount or stated["currency"] != posting.currency
                    or stated["as_of"] != _stamp(posting.as_of)):
                raise StatementRefused(
                    "transaction {} is in the ledger as {} {} on {}, but IBKR now states {} {} on {}; the ledger is "
                    "not changed (docs/manual/reconciliation.md)".format(
                        posting.transaction_id, stated["amount"], stated["currency"], stated["as_of"],
                        posting.amount, posting.currency, _stamp(posting.as_of)))
            continue
        after, by_owner = sides[posting.transaction_id]
        if by_owner:
            resolved.append(posting.transaction_id)
        if not after:
            skipped.append((posting.transaction_id, "recorded by the owner as in the starting book" if by_owner else
                            "dated {}, before the first run's approval at {}, so it is in the starting book".format(
                                _stamp(posting.as_of), _stamp(approved_at))))
            continue
        if posting.amount == 0:
            raise StatementRefused("transfer {} is of nothing".format(posting.transaction_id))
        equity, currency, basis = _equity_before(snapshots, moved, posting)
        if currency != posting.currency:
            raise StatementRefused("transfer {} is in {}, but the account's net liquidation is in {}".format(
                posting.transaction_id, posting.currency, currency))
        if not equity > 0 or not equity + posting.amount > 0:
            raise StatementRefused("transfer {} of {} would take the equity before it, {}, to zero or below".format(
                posting.transaction_id, posting.amount, equity))
        if posting.as_of < approved_at and not by_owner:
            date_only.append(posting.transaction_id)
        written.append(_record(posting, equity, currency, basis, _written_first(snapshots, already, posting)))
    return written, skipped, date_only, resolved


def _write_whole(path, text):
    """text at path, whole or not at all: a temporary file beside it, flushed, then renamed over it."""
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        f.write(text)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)
    directory = os.open(os.path.dirname(os.path.abspath(path)), os.O_RDONLY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)


def _line(record):
    return json.dumps(record, separators=(",", ":"), allow_nan=False) + "\n"


def read_coverage(path):
    """{"from_date", "covered_to"} of the last accepted reading, kept at path (None when there is none yet);
    StatementRefused when the file is not as this reader writes it."""
    try:
        with open(path, encoding="utf-8") as f:
            text = f.read()
    except FileNotFoundError:
        return None
    except OSError as err:
        raise StatementRefused("the coverage file {} cannot be read: {}".format(path, err.strerror or err)) from None
    try:
        coverage = json.loads(text)
    except ValueError:
        coverage = None
    if (not isinstance(coverage, dict) or set(coverage) != set(COVERAGE_FIELDS)
            or not all(isinstance(coverage[k], str) and _flex_day(coverage[k]) for k in COVERAGE_FIELDS)):
        raise StatementRefused("the coverage file {} is not as this reader writes it, so the days the last reading "
                               "covered are not known; put it back from a backup".format(path))
    return coverage


def _day_text(day):
    return day.strftime("%Y%m%d")


def _check_coverage(statement, coverage, approved_at):
    """CoverageGap when statement starts later than the day after coverage's covered_to (the last accepted
    reading's), or, with no coverage kept, later than the New York day of the first run's approval at approved_at."""
    first = _flex_day(statement.from_date)
    if coverage is None:
        approval_day = approved_at.astimezone(NEW_YORK).date()
        if first > approval_day:
            raise CoverageGap(
                "the Flex statement starts on {}, but no earlier reading is kept (the first reading, or its coverage "
                "file lost) and the first run was approved on {}: a transfer between the two would be in no "
                "statement. Nothing is written; set the Flex query's period in Client Portal to a custom range "
                "from {}, start this reader once, then set it back (docs/manual/reconciliation.md)".format(
                    statement.from_date, _day_text(approval_day), _day_text(approval_day)))
        return
    reread = _flex_day(coverage["covered_to"]) + timedelta(days=1)
    if first > reread:
        raise CoverageGap("the Flex statement starts on {}, but the last reading covered up to {} (its last day, {}, "
                          "is read again): the days between are in neither, so a transfer on them would be lost. "
                          "Nothing is written; widen the Flex query's period in Client Portal to reach back to {} "
                          "and start this reader again (docs/manual/reconciliation.md)".format(
                              statement.from_date, coverage["covered_to"], _day_text(reread), _day_text(reread)))


def read_resolutions(path):
    """{transaction ID: decision} the owner recorded, kept at path ({} when there is none); StatementRefused when
    the file is not as this reader writes it."""
    try:
        with open(path, encoding="utf-8") as f:
            text = f.read()
    except FileNotFoundError:
        return {}
    except OSError as err:
        raise StatementRefused("the resolutions file {} cannot be read: {}".format(path, err.strerror or err)) from None
    try:
        resolutions = json.loads(text)
    except ValueError:
        resolutions = None
    if (not isinstance(resolutions, dict)
            or not all(isinstance(k, str) and k.strip() == k and k and v in DECISIONS for k, v in resolutions.items())):
        raise StatementRefused("the resolutions file {} is not as this reader writes it, so the owner's decisions on "
                               "ambiguous transfers are not known; put it back from a backup".format(path))
    return resolutions


LOCK_WAIT = 10


def _status_of(ledger_path):
    """The status file beside ledger_path as a dict, or {} when there is none or it is not one."""
    try:
        with open(ledger_path + STATUS_SUFFIX, encoding="utf-8") as f:
            status = json.loads(f.read())
    except (OSError, ValueError):
        return {}
    return status if isinstance(status, dict) else {}


def record_resolutions(ledger_path, decisions, wait=LOCK_WAIT, sleep=time.sleep):
    """decisions ({transaction ID: IN_FIRST_BOOK or AFTER_APPROVAL}) added to the resolutions file beside
    ledger_path, under the ledger's lock (waited for at most wait seconds: StatementRefused when a reading holds it
    longer); StatementRefused, nothing recorded, when one would change a decision already recorded (a transfer
    written as after the approval stays so), names a transfer already in the ledger, or names one the last
    reading did not set aside as ambiguous (a decision no later reading would consult)."""
    path = ledger_path + RESOLUTIONS_SUFFIX
    with open(ledger_path + LOCK_SUFFIX, "a") as lock:
        waited = 0
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if waited >= wait:
                    raise StatementRefused("a reading is running, try again in a few minutes") from None
                sleep(1)
                waited += 1
        resolutions = read_resolutions(path)
        ledger = read_ledger(ledger_path)
        status = _status_of(ledger_path)
        waiting = status.get("ambiguous") if status.get("state") == "ambiguous-transfer" else None
        waiting = [i for i in waiting if isinstance(i, str)] if isinstance(waiting, list) else []
        for transaction_id, decision in sorted(decisions.items()):
            if decision not in DECISIONS:
                raise StatementRefused("{} is not a decision ({} or {})".format(decision, IN_FIRST_BOOK,
                                                                                AFTER_APPROVAL))
            if resolutions.get(transaction_id, decision) != decision:
                raise StatementRefused("transfer {} is already recorded as {}; a recorded decision is never "
                                       "changed (docs/manual/reconciliation.md)".format(
                                           transaction_id, resolutions[transaction_id]))
            if ID_PREFIX + transaction_id in ledger:
                raise StatementRefused("transfer {} is already journalled as {}{}; a decision now would change "
                                       "nothing (docs/manual/reconciliation.md)".format(
                                           transaction_id, ID_PREFIX, transaction_id))
            if transaction_id not in waiting and resolutions.get(transaction_id) != decision:
                raise StatementRefused(
                    "the reader is {}; transfer {} is not one it set aside as ambiguous, so a decision on it would "
                    "never be consulted (docs/manual/reconciliation.md)".format(
                        "waiting on a decision for " + ", ".join(waiting) if waiting
                        else "not waiting on a decision", transaction_id))
        merged = dict(resolutions, **decisions)
        if merged != resolutions:
            _write_whole(path, json.dumps(merged, sort_keys=True) + "\n")


def read_once(token, query_id, account_id, engine_log_path, ledger_path, get=_get, sleep=time.sleep,
              now=lambda: datetime.now(timezone.utc), err=None):
    """One reading (see the module): True when the statement was read and every new transfer written, False when
    the reader was set aside. Either way the status file beside ledger_path says which, and why."""
    def say(text):
        if err is not None:
            print(mask(text, token, account_id), file=err)

    checked_at = _stamp(now())
    status_path = ledger_path + STATUS_SUFFIX
    coverage_path = ledger_path + COVERAGE_SUFFIX
    status = {"state": "read", "checked_at": checked_at}
    with open(ledger_path + LOCK_SUFFIX, "a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        # Said first, so a reading stopped part way (its unit's timeout, a kill) leaves "running" with its time,
        # never the previous reading's "read" (docs/manual/reconciliation.md, "The status file").
        _write_whole(status_path, json.dumps({"state": "running", "checked_at": checked_at}, sort_keys=True) + "\n")
        try:
            ledger = read_ledger(ledger_path)
            coverage = read_coverage(coverage_path)
            resolutions = read_resolutions(ledger_path + RESOLUTIONS_SUFFIX)
            statement = parse_statement(fetch_statement(token, query_id, get=get, sleep=sleep), account_id)
            approved_at, snapshots = read_chain(engine_log_path)
            _check_coverage(statement, coverage, approved_at)
            written, skipped, date_only, resolved = new_movements(statement, ledger, approved_at, snapshots,
                                                                  resolutions)
            for transaction_id in resolved:
                say("flex: transfer {} placed by the owner's recorded decision ({})".format(
                    transaction_id, resolutions[transaction_id]))
            for transaction_id, why in skipped:
                say("flex: transfer {} not written: {}".format(transaction_id, why))
            for transaction_id in date_only:
                say("flex: transfer {} is dated by day alone on the first run's approval day, so taken as after "
                    "the approval and written".format(transaction_id))
            out_of_order = [r for r in written if "note" in r]
            for record in out_of_order:
                say("flex: transfer {} written with the note: {}".format(record["payload"]["transaction_id"],
                                                                        record["note"]))
            unknown = {}
            for posting in statement.postings:
                if posting.kind == OTHER:
                    unknown[posting.ibkr_type] = unknown.get(posting.ibkr_type, 0) + 1
            for ibkr_type, count in sorted(unknown.items()):
                say("flex: {} posting(s) of type {!r}, which the reader does not classify, not written".format(
                    count, ibkr_type))
            if written:
                kept = ""
                if ledger:
                    with open(ledger_path, encoding="utf-8") as f:
                        kept = f.read()
                _write_whole(ledger_path, kept + "".join(_line(r) for r in written))
            # After the ledger: a reading stopped between the two leaves the period behind, which only asks the
            # next reading to cover more, and a transfer written twice is never possible (its ID is in the ledger).
            # The statement's last day is read again by the next one (see the module).
            covered_to = _day_text(_flex_day(statement.to_date) - timedelta(days=1))
            if coverage is not None and coverage["covered_to"] > covered_to:
                covered_to = coverage["covered_to"]
            _write_whole(coverage_path, json.dumps({"from_date": statement.from_date, "covered_to": covered_to},
                                                   sort_keys=True) + "\n")
            status.update(written=len(written), period=[statement.from_date, statement.to_date],
                          covered_to=covered_to, postings=len(statement.postings), in_first_book=len(skipped),
                          date_only_on_approval_day=len(date_only), resolved=len(resolved),
                          out_of_order=len(out_of_order), unknown_types=unknown)
            say("flex: read {} postings for {} to {}; wrote {} cash movement(s)".format(
                len(statement.postings), statement.from_date, statement.to_date, len(written)))
            ok = True
        except (StatementRefused, FlexRejected, FlexUnavailable, ResolveRefused, OSError) as refused:
            # Each sets the reader aside, named; anything else is a defect and stops the command.
            reason = mask(str(refused), token, account_id)
            if isinstance(refused, FlexRejected):
                reason += "; check the token and query ID in the secrets file (TI_IBKR_FLEX_TOKEN, " \
                          "TI_IBKR_FLEX_QUERY_ID) and the token's expiry in Client Portal"
            state = ("coverage-gap" if isinstance(refused, CoverageGap)
                     else "ambiguous-transfer" if isinstance(refused, AmbiguousTransfer) else "set-aside")
            status.update(state=state, reason=reason, code=getattr(refused, "code", None))
            if isinstance(refused, AmbiguousTransfer):
                status["ambiguous"] = sorted(refused.ids)
            say("flex: set aside: " + reason)
            ok = False
        _write_whole(status_path, json.dumps(status, sort_keys=True) + "\n")
    return ok


def read_key(path, key):
    """The bare value of key's one line in the KEY=VALUE file at path; StatementRefused, never echoing a value."""
    try:
        with open(path, encoding="utf-8") as f:
            lines = f.read().splitlines()
    except OSError as err:
        raise StatementRefused("cannot read the secrets file {}: {}".format(path, err.strerror or err)) from None
    found = [line.split("=", 1)[1].strip() for line in lines if "=" in line and line.split("=", 1)[0] == key]
    if len(found) != 1 or not found[0]:
        raise StatementRefused("the secrets file {} does not hold exactly one filled-in {} line".format(path, key))
    if re.search(r"[\s\"']", found[0]):
        raise StatementRefused("the {} line of {} is not KEY=VALUE with a bare value: no quotes, no spaces".format(
            key, path))
    return found[0]


INSTANCES = ("boost", "trad-ira", "roth-ira")


def account_key(instance):
    """The KEY of instance's IBKR account line: IBKR_ACCOUNT_BOOST, IBKR_ACCOUNT_TRAD_IRA, IBKR_ACCOUNT_ROTH_IRA."""
    return "IBKR_ACCOUNT_" + instance.upper().replace("-", "_")


RESOLUTION = re.compile(r"([^\s=]+)=(.*)")


def _resolutions(texts):
    """{transaction ID: decision} of `--resolve ID=DECISION` texts; StatementRefused for one not so."""
    decisions = {}
    for text in texts:
        match = RESOLUTION.fullmatch(text)
        if not match or match.group(2) not in DECISIONS:
            raise StatementRefused("--resolve {!r} is not TRANSACTION_ID={} or TRANSACTION_ID={}".format(
                text, IN_FIRST_BOOK, AFTER_APPROVAL))
        if decisions.get(match.group(1), match.group(2)) != match.group(2):
            raise StatementRefused("--resolve names transfer {} twice, with both decisions".format(match.group(1)))
        decisions[match.group(1)] = match.group(2)
    return decisions


def command(args, err, get=_get, sleep=time.sleep):
    """`flex`'s exit status: 0 for a reading or a recorded decision, 1 for a refusal or a set-aside (why on err).

    With --resolve, the owner's decisions are recorded beside the ledger and nothing is read: the next reading
    honours them."""
    if args.resolve:
        try:
            decisions = _resolutions(args.resolve)
            record_resolutions(args.ledger, decisions)
        except (StatementRefused, OSError) as refusal:
            print("refused: {}".format(mask(str(refusal))), file=err)
            return 1
        for transaction_id, decision in sorted(decisions.items()):
            print("flex: transfer {} recorded as {}; the next reading honours it".format(transaction_id, decision),
                  file=err)
        return 0
    missing = [flag for flag, value in (("--secrets", args.secrets), ("--account-secrets", args.account_secrets),
                                        ("--instance", args.instance), ("--engine-log", args.engine_log))
               if value is None]
    if missing:
        print("refused: a reading needs {}".format(", ".join(missing)), file=err)
        return 1
    try:
        token = read_key(args.secrets, "TI_IBKR_FLEX_TOKEN")
        query_id = read_key(args.secrets, "TI_IBKR_FLEX_QUERY_ID")
        account_id = read_key(args.account_secrets, account_key(args.instance))
    except StatementRefused as refusal:
        print("refused: {}".format(mask(str(refusal))), file=err)
        return 1
    return 0 if read_once(token, query_id, account_id, args.engine_log, args.ledger, get=get, sleep=sleep,
                          err=err) else 1
