"""The Flex cash-movement reader: IBKR's Cash Transactions, read once a day, each transfer into the instance's ledger.

`python3 -m adapter.ibkr flex ...` (cli.py), run by ti-flex-boost.timer after
the close (docs/manual/reconciliation.md, "Deposits and withdrawals, read
from IBKR"). One reading:

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
   withdrawal not yet in the instance's ledger, with the equity before it:
   the net liquidation of the engine's last account snapshot before it,
   found back through the chain of runs (relay.run_chain). A transfer dated
   before the chain's first snapshot is already in the first book and is
   not written. Postings other than transfers are parsed, and kept on the
   Statement, but not written.

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
import os
import re
import time
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ElementTree
from collections import namedtuple
from datetime import datetime, timezone
from decimal import Decimal, InvalidOperation
from zoneinfo import ZoneInfo

from relay import LOG_INPUT, ResolveRefused, run_chain

CASH_MOVEMENT = ("account.cash-movement", 2)   # internal/event CashMovementTransferVersion
SOURCE = "ibkr-flex"
ID_PREFIX = "ibkr-flex:"
STATUS_SUFFIX = ".status.json"
LOCK_SUFFIX = ".lock"

# IBKR's Flex Web Service, version 3. GetStatement's address comes from
# SendRequest's answer; the token is sent only to an https address on
# IBKR_HOST.
SEND_REQUEST_URL = "https://ndcdyn.interactivebrokers.com/AccountManagement/FlexWebService/SendRequest"
FLEX_VERSION = "3"
IBKR_HOST = re.compile(r"(?:[a-z0-9-]+\.)*interactivebrokers\.com")
USER_AGENT = "TrendInvesting-flex-reader/1"
REQUEST_TIMEOUT = 60
# A statement IBKR is still generating (code 1019) is asked for again every
# GENERATION_WAIT seconds, GENERATION_ATTEMPTS times in all (5 minutes).
GENERATION_WAIT = 10
GENERATION_ATTEMPTS = 30
GENERATING = "1019"
# IBKR's error codes that say the token, its account or the query is not
# accepted: the owner's setup must change, so the reading is set aside as
# rejected. Every other code is the service's own (busy, not ready, a
# statement that could not be generated): set aside as unavailable.
REJECTED_CODES = frozenset(("1010", "1011", "1012", "1013", "1014", "1015", "1016", "1020"))

# IBKR states a Flex date as yyyyMMdd and a date and time as yyyyMMdd;HHmmss
# (the Flex query's default formats, which the owner keeps), in New York
# time. A date alone is the start of that New York day.
NEW_YORK = ZoneInfo("America/New_York")
FLEX_DATE = re.compile(r"([0-9]{4})([0-9]{2})([0-9]{2})(?:;([0-9]{2})([0-9]{2})([0-9]{2}))?")

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

Posting = namedtuple("Posting", "transaction_id kind ibkr_type as_of amount currency")
"""One Cash Transactions row: IBKR's transaction ID, the kind (KINDS, else OTHER), IBKR's own type, when (UTC,
a whole second), the amount (a Decimal, signed as IBKR states it) and its currency."""

Statement = namedtuple("Statement", "account_id from_date to_date postings")
"""The instance's Flex statement: its account, the period IBKR states, and every detail posting in order."""

ACCOUNT_NUMBER = re.compile(r"\b[A-Z]{1,2}[0-9]{5,9}\b")
RECORD_FIELDS = ("id", "type", "schema_version", "event_time", "source", "payload")
PAYLOAD_FIELDS = ("as_of", "amount", "equity_before", "currency", "transaction_id")


class StatementRefused(ValueError):
    """The statement, or the ledger beside it, cannot be read as written; nothing of it is taken."""


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
    return local.astimezone(timezone.utc)


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
        when = row.get("dateTime") if row.get("dateTime") is not None else row.get("date")
        postings.append(Posting(transaction_id, KINDS.get(ibkr_type, OTHER), ibkr_type,
                                _flex_time(when, "{} (transaction {})".format(where, transaction_id)), amount,
                                currency))
    return Statement(account_id, statement.get("fromDate"), statement.get("toDate"), postings)


def _get(url, params):
    """The body IBKR answers a GET of url with params; OSError (urllib's) on any failure."""
    request = urllib.request.Request(url + "?" + urllib.parse.urlencode(params), headers={"User-Agent": USER_AGENT})
    with urllib.request.urlopen(request, timeout=REQUEST_TIMEOUT) as response:
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


def account_snapshots(engine_log_path):
    """[(as_of, net liquidation, currency)] of every committed account snapshot (version 3) in the chain of runs."""
    found = []
    for path, groups, _ in run_chain(engine_log_path):
        for group in groups:
            for _, kind, envelope in group:
                if kind != LOG_INPUT or (envelope["type"], envelope["schema_version"]) != ("account.snapshot", 3):
                    continue
                payload = envelope["payload"]
                as_of = _parse_stamp(payload.get("as_of")) if isinstance(payload, dict) else None
                value = payload.get("net_liquidation") if isinstance(payload, dict) else None
                if (as_of is None or isinstance(value, bool) or not isinstance(value, (int, float)) or value <= 0
                        or not isinstance(payload.get("currency"), str)):
                    raise StatementRefused("the account snapshot {} in {} has no as-of time, positive net "
                                           "liquidation and currency".format(envelope["id"], path))
                found.append((as_of, float(value), payload["currency"]))
    return sorted(found)


def _equity_before(snapshots, as_of):
    before = [s for s in snapshots if s[0] < as_of]
    return before[-1] if before else None


def _record(posting, equity, currency):
    payload = {"as_of": _stamp(posting.as_of), "amount": float(posting.amount), "equity_before": equity,
               "currency": currency, "transaction_id": posting.transaction_id}
    return {"id": ID_PREFIX + posting.transaction_id, "type": CASH_MOVEMENT[0], "schema_version": CASH_MOVEMENT[1],
            "event_time": payload["as_of"], "source": SOURCE, "payload": payload}


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
        if (not isinstance(record, dict) or tuple(sorted(record)) != tuple(sorted(RECORD_FIELDS))
                or (record["type"], record["schema_version"], record["source"]) != CASH_MOVEMENT + (SOURCE,)
                or not isinstance(payload, dict) or tuple(sorted(payload)) != tuple(sorted(PAYLOAD_FIELDS))
                or not isinstance(payload["transaction_id"], str)
                or record["id"] != ID_PREFIX + payload["transaction_id"] or record["id"] in records
                or _parse_stamp(payload["as_of"]) is None or record["event_time"] != payload["as_of"]):
            raise StatementRefused("the ledger {} line {} is not a cash movement as this reader writes it; it is "
                                   "never appended to until it is put back".format(path, n))
        records[record["id"]] = record
    return records


def new_movements(statement, ledger, snapshots):
    """(records to write, transfers not written and why): each transfer of statement not yet in ledger."""
    written, skipped = [], []
    for posting in sorted((p for p in statement.postings if p.kind == TRANSFER),
                          key=lambda p: (p.as_of, p.transaction_id)):
        known = ledger.get(ID_PREFIX + posting.transaction_id)
        if known is not None:
            stated = known["payload"]
            if (Decimal(repr(stated["amount"])) != posting.amount or stated["currency"] != posting.currency
                    or stated["as_of"] != _stamp(posting.as_of)):
                raise StatementRefused(
                    "transaction {} is in the ledger as {} {} on {}, but IBKR now states {} {} on {}; the ledger is "
                    "not changed (docs/manual/reconciliation.md)".format(
                        posting.transaction_id, stated["amount"], stated["currency"], stated["as_of"],
                        posting.amount, posting.currency, _stamp(posting.as_of)))
            continue
        if posting.amount == 0:
            raise StatementRefused("transfer {} is of nothing".format(posting.transaction_id))
        equity = _equity_before(snapshots, posting.as_of)
        if equity is None:
            skipped.append((posting.transaction_id, "dated {}, before the engine's first account snapshot, so it is "
                                                    "already in the first book".format(_stamp(posting.as_of))))
            continue
        _, value, currency = equity
        if currency != posting.currency:
            raise StatementRefused("transfer {} is in {}, but the account's net liquidation is in {}".format(
                posting.transaction_id, posting.currency, currency))
        record = _record(posting, value, currency)
        if not value + record["payload"]["amount"] > 0:
            raise StatementRefused("transfer {} of {} would take the equity before it, {}, to zero or below".format(
                posting.transaction_id, posting.amount, value))
        written.append(record)
    return written, skipped


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


def read_once(token, query_id, account_id, engine_log_path, ledger_path, get=_get, sleep=time.sleep,
              now=lambda: datetime.now(timezone.utc), err=None):
    """One reading (see the module): True when the statement was read and every new transfer written, False when
    the reader was set aside. Either way the status file beside ledger_path says which, and why."""
    def say(text):
        if err is not None:
            print(mask(text, token, account_id), file=err)

    status = {"state": "read", "checked_at": _stamp(now())}
    with open(ledger_path + LOCK_SUFFIX, "a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        try:
            ledger = read_ledger(ledger_path)
            statement = parse_statement(fetch_statement(token, query_id, get=get, sleep=sleep), account_id)
            written, skipped = new_movements(statement, ledger, account_snapshots(engine_log_path))
            for transaction_id, why in skipped:
                say("flex: transfer {} not written: {}".format(transaction_id, why))
            if written:
                kept = ""
                if ledger:
                    with open(ledger_path, encoding="utf-8") as f:
                        kept = f.read()
                _write_whole(ledger_path, kept + "".join(_line(r) for r in written))
            status.update(written=len(written), period=[statement.from_date, statement.to_date],
                          postings=len(statement.postings))
            say("flex: read {} postings for {} to {}; wrote {} cash movement(s)".format(
                len(statement.postings), statement.from_date, statement.to_date, len(written)))
            ok = True
        except (StatementRefused, FlexRejected, FlexUnavailable, ResolveRefused, OSError) as refused:
            # Each sets the reader aside, named; anything else is a defect and stops the command.
            reason = mask(str(refused), token, account_id)
            if isinstance(refused, FlexRejected):
                reason += "; check the token and query ID in the secrets file (TI_IBKR_FLEX_TOKEN, " \
                          "TI_IBKR_FLEX_QUERY_ID) and the token's expiry in Client Portal"
            status.update(state="set-aside", reason=reason, code=getattr(refused, "code", None))
            say("flex: set aside: " + reason)
            ok = False
        _write_whole(ledger_path + STATUS_SUFFIX, json.dumps(status, sort_keys=True) + "\n")
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


def command(args, err, get=_get, sleep=time.sleep):
    """`flex`'s exit status: 0 for a reading, 1 for a refusal or a set-aside (why on err)."""
    try:
        token = read_key(args.secrets, "TI_IBKR_FLEX_TOKEN")
        query_id = read_key(args.secrets, "TI_IBKR_FLEX_QUERY_ID")
        account_id = read_key(args.account_secrets, args.account_key)
    except StatementRefused as refusal:
        print("refused: {}".format(mask(str(refusal))), file=err)
        return 1
    return 0 if read_once(token, query_id, account_id, args.engine_log, args.ledger, get=get, sleep=sleep,
                          err=err) else 1
