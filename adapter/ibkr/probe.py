"""The same-day probe: after an adapter install, read what one start of the adapter logged and what the engine journalled for it, and say plainly whether the account's link works.

docs/live-trading/lessons-2026-10.md L5 and L10: every server defect of the
Boost dry run surfaced one per morning, and the test doubles had returned
what was expected rather than what IBKR sends; so each install ends the
same day with this check on the real server, against the real Gateway's
answers (docs/manual/ibkr-adapter.md, "The same-day probe").

    journalctl _SYSTEMD_INVOCATION_ID=<the adapter's> -o cat > adapter.txt
    python3 -m adapter.ibkr probe --journal adapter.txt --engine-log <run>.log --client-id 12 --snapshot-version 4

It reads two files and talks to nothing: the adapter's journal lines for one
start (the messages as the adapter logs them, without journald's prefix) and
the engine's durable log. Each check prints one line, "probe: ok ..." or
"probe: FAIL ...", and the last line is "probe: passed" (exit 0) or "probe:
FAILED, n check(s)" (exit 1). A file that cannot be read, a line the adapter
writes in another shape, or a record the engine journalled without what it
must carry fails loudly, never passes for lack of evidence (fail closed,
AGENTS.md). It prints no account number (masking.mask_accounts; L12).

Its evidence is this start's only: the engine's records it checks are the
inputs this start delivered, named in the start's own journal lines
("ibkr input=<id> type=<type> answered ..."), so an earlier start's or
another run's records never pass for it. The checks, in the order a start
makes them true:

1. the adapter started, in the mode its configuration names, with its own
   client ID, sending off unless the mode is paper; the server version the
   Gateway negotiated is reported (which of ibapi's decoders reads IBKR);
2. it connected to IB Gateway with that client ID;
3. it reconciled the account with that client ID;
4. it queued a batch of the run's stream, and the latest batch reached the
   engine whole, by the adapter's own outcome line for it ("ibkr stream batch
   files=... inputs=N answered=A struck=S outcome=delivered": every input it
   queued answered or struck off as already committed, A + S = N, counted
   by the adapter, never by grepping answered lines, so a reconciliation or
   broker report answered meanwhile pads nothing). A batch whose newest word
   is outcome=waiting, or that has no outcome line, fails: an earlier batch
   delivered never covers a later one still waiting. A start that queued
   none passes only on the adapter's own word that its stream is caught up
   ("ibkr stream no new file since ...; caught up"), and fails when it is
   still waiting for its reading (the Flex proof, or between attempts), when
   --expect-batch says this start must deliver one, or when nothing says
   either way;
5. for the latest batch, the engine journalled, among the inputs it delivered, an
   account snapshot at the version the engine reads (4 for an IRA: settled
   and total cash), a reconciliation, the batch's marks (version 3 with the
   IBKR contract for an IRA) and its session closes, each with every field
   the engine reads, valid (a field that is not is named);
6. no order released in reply to this start's inputs is marked to be sent
   in dry-run (in paper, the count of those marked to be sent is reported),
   and none was refused for a missing mark (missing-mark);
7. the engine's latest decision among this start's, decided or withheld
   with its reason, or that none is due yet: reported, never a failure by
   itself.

Contracts the adapter could not resolve are listed from its warnings, with
IBKR's reason: each such name has no mark, by design (ADR 0026 point 3).
"""
import math
import re
from datetime import datetime

from masking import mask_accounts
from relay import LOG_DECISION, LOG_INPUT, ResolveRefused, _committed_groups

STARTED = re.compile(r"ibkr adapter starting mode=(\S+) client_id=(\d+) sending=(on|off) ibapi=(\S+)")
CONNECTED = re.compile(r"ibkr link state=connected reason=(\S*) client_id=(\d+)")
SERVER_VERSION = re.compile(r"ibkr gateway server_version=(\S+) contract_data=(\S+)")
CLOCK_RECHECK = re.compile(r"ibkr gateway clock recheck (.*)$")
RECONCILED = re.compile(r"ibkr reconciled client_id=(\d+) positions=(\d+) open_orders=(\d+) executions=(\d+)")
QUEUED = re.compile(r"ibkr stream queued run=(\S+) files=(\S+) inputs=(\d+)( resumed=1)?")
ANSWERED = re.compile(r"ibkr input=(\S+) type=(\S+) answered with (\d+) decisions")
UNRESOLVED = re.compile(r"ibkr contract for (\S+) not resolved")
OUTCOME = re.compile(r"ibkr stream batch files=(\S+) inputs=(\d+) answered=(\d+) struck=(\d+)(?: resumed=1)? "
                     r"outcome=(\S+) reason=(.*)$")
CAUGHT_UP = re.compile(r"ibkr stream no new file since (\S+); caught up")
WAITS = re.compile(r"(ibkr broker snapshot waits.*)$")
# The adapter's stamp: whole seconds in UTC (snapshot._stamp), as every mark's as_of.
STAMP = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z")
# The largest contract ID the engine holds (MarkContract.ConID is an int64).
MAX_INT64 = 2 ** 63 - 1
STOPPED = re.compile(r"^adapter stopped: (.*)$")
REFUSED = re.compile(r"^refused: (.*)$")

SNAPSHOT, MARK, CLOSED = "account.snapshot", "market.mark.recorded", "market.session.closed"
RECONCILIATION, DECIDED = "account.reconciliation", "strategy.targets.decided"
RELEASED, ORDER_REFUSED = "risk.order.released", "risk.order.refused"


class Report:
    def __init__(self):
        self.lines, self.failures = [], 0

    def ok(self, text):
        self.lines.append("probe: ok " + text)

    def fail(self, text):
        self.failures += 1
        self.lines.append("probe: FAIL " + text)

    def note(self, text):
        self.lines.append("probe: " + text)


def _number(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def _count(value):
    return isinstance(value, int) and not isinstance(value, bool)


def _text(value):
    return isinstance(value, str) and bool(value)


def _positions(payload):
    rows = payload.get("positions")
    if not isinstance(rows, list):
        return "positions"
    for i, row in enumerate(rows):
        if not isinstance(row, dict) or not _text(row.get("instrument_id")) or not _count(row.get("quantity")):
            return "positions[{}]".format(i)
    return ""


def snapshot_fault(payload, version):
    """The first field of an account snapshot the engine would not read as valid, or ""."""
    if not isinstance(payload, dict):
        return "payload"
    for name, ok in (("account_id", _text(payload.get("account_id"))), ("as_of", _text(payload.get("as_of"))),
                     ("net_liquidation", _number(payload.get("net_liquidation")) and payload["net_liquidation"] > 0),
                     ("currency", payload.get("currency") == "USD")):
        if not ok:
            return name
    if _positions(payload):
        return _positions(payload)
    if version == 4:
        for name in ("settled_cash", "total_cash"):
            if not _number(payload.get(name)):
                return name
    return ""


def reconciliation_fault(payload):
    """The first field of a reconciliation the engine would not read as valid, or ""."""
    if not isinstance(payload, dict):
        return "payload"
    unreadable = payload.get("unreadable")
    if not isinstance(unreadable, list):
        return "unreadable"
    if not _text(payload.get("account_id")):
        return "account_id"
    if "total-cash" not in unreadable and not _number(payload.get("total_cash")):
        return "total_cash"
    if "positions" not in unreadable and _positions(payload):
        return _positions(payload)
    return ""


def _stamp_ok(text):
    """Whether text is a real instant in the adapter's whole-second UTC stamp, never Go's zero time.

    Parsed, not matched by shape: an impossible date (2027-02-30) does not
    decode into the engine's time.Time, and internal/event's requiredTime
    refuses the zero time 0001-01-01T00:00:00Z.
    """
    if not (isinstance(text, str) and STAMP.fullmatch(text)):
        return False
    try:
        at = datetime.strptime(text, "%Y-%m-%dT%H:%M:%SZ")
    except ValueError:
        return False
    return at != datetime(1, 1, 1)


def mark_fault(payload, version, close=None):
    """The first field of a mark the engine would not read as valid at version, or "".

    The engine's rule is internal/event MarkRecordedPayload.ValidateSchema:
    an instrument, an as-of time, a finite positive price and a source; a
    broker last that is finite, positive when set and zero when not; version
    1 with no broker last, and only version 3 with a contract (a positive
    int64 contract ID, SMART, a primary exchange, US dollars). The probe
    asks one thing more of the adapter's own marks: as_of is the batch's
    close (close, the last session close's period_end), in the adapter's
    whole-second UTC stamp, since a mark as of any other time is not the
    close the engine values the batch at (ADR 0026 point 3).
    """
    if not isinstance(payload, dict):
        return "payload"
    if not _text(payload.get("instrument_id")):
        return "instrument_id"
    if not _stamp_ok(payload.get("as_of")) or (close is not None and payload["as_of"] != close):
        return "as_of"
    if not (_number(payload.get("price")) and payload["price"] > 0):
        return "price"
    if not _text(payload.get("source")):
        return "source"
    last, last_set = payload.get("broker_last", 0), payload.get("broker_last_set", False)
    if not isinstance(last_set, bool):
        return "broker_last_set"
    if not _number(last) or (last_set and not last > 0) or (not last_set and last != 0):
        return "broker_last"
    if version == 1 and (last_set or last != 0):
        return "broker_last"
    contract = payload.get("contract")
    if version != 3:
        return "contract" if contract is not None else ""
    if not isinstance(contract, dict):
        return "contract"
    if not (_count(contract.get("con_id")) and 0 < contract["con_id"] <= MAX_INT64):
        return "contract.con_id"
    for name, ok in (("contract.exchange", contract.get("exchange") == "SMART"),
                     ("contract.primary_exchange", _text(contract.get("primary_exchange"))),
                     ("contract.currency", contract.get("currency") == "USD")):
        if not ok:
            return name
    return ""


def check_journal(lines, client_id, mode, report, expect_batch=False):
    """Checks 1 to 4 over the adapter's journal lines for one start.

    Returns (queued, latest, answered, unresolved): whether this start queued
    a batch; the input IDs answered after the latest batch's queued line and
    before its outcome line (the records that batch is judged by, check 5);
    every input ID this start delivered (whose decisions check 6 reads); and
    the names whose contract the adapter could not resolve.
    """
    text = [line.rstrip("\n") for line in lines]
    for line in text:
        for pattern, what in ((STOPPED, "the adapter stopped"), (REFUSED, "the adapter refused to start")):
            m = pattern.search(line)
            if m:
                report.fail("{}: {}".format(what, m.group(1)))
    started = [m for m in (STARTED.search(line) for line in text) if m]
    if len(started) != 1:
        report.fail("the journal holds {} start line(s) (\"ibkr adapter starting ...\"), not one: give the lines of "
                    "one start (journalctl _SYSTEMD_INVOCATION_ID=...)".format(len(started)))
    else:
        got_mode, got_id, sending = started[0].group(1), int(started[0].group(2)), started[0].group(3)
        if got_id != client_id or (mode and got_mode != mode) or (sending == "on" and got_mode != "paper"):
            report.fail("the adapter started mode={} client_id={} sending={}; want mode={} client_id={} and sending "
                        "on only in paper".format(got_mode, got_id, sending, mode or got_mode, client_id))
        else:
            report.ok("the adapter started: mode={} client_id={} sending={} ibapi={}".format(
                got_mode, got_id, sending, started[0].group(4)))
    versions = [m for m in (SERVER_VERSION.search(line) for line in text) if m]
    if versions:
        report.note("Gateway server version {} (contract details read from IBKR's {} messages)".format(
            versions[-1].group(1), versions[-1].group(2)))
    clock_checks = [m for m in (CLOCK_RECHECK.search(line) for line in text) if m]
    if clock_checks:
        # ADR 0019: clock evidence is an observation, not a new probe gate or engine input.
        report.note("Gateway clock recheck " + clock_checks[-1].group(1))
    connected = [m for m in (CONNECTED.search(line) for line in text) if m]
    reconciled = [m for m in (RECONCILED.search(line) for line in text) if m]
    for found, what, ids in ((connected, "connected to IB Gateway", {int(m.group(2)) for m in connected}),
                             (reconciled, "reconciled the account", {int(m.group(1)) for m in reconciled})):
        if not found:
            report.fail("no line shows the adapter {}".format(what))
        elif ids != {client_id}:
            report.fail("the adapter {} as client_id {}, not {}".format(what, sorted(ids), client_id))
        elif found is reconciled:
            report.ok("reconciled client_id={}: positions={} open_orders={} executions={}".format(
                *found[-1].groups()))
        else:
            report.ok("connected to IB Gateway as client_id={} ({} time(s))".format(client_id, len(found)))
    # The batches this start queued, each with the inputs answered after its
    # queued line (the records the latest is judged by) and the outcome line
    # the adapter states for it from its own accounting (delivery.Deliverer
    # settled): the probe never counts answered lines itself.
    batches, current, answered, outcomes = [], None, [], []
    for line in text:
        q = QUEUED.search(line)
        if q:
            current = {"run": q.group(1), "files": q.group(2), "inputs": int(q.group(3)), "answered": [],
                       "outcome": None, "resumed": bool(q.group(4))}
            batches.append(current)
            continue
        o = OUTCOME.search(line)
        if o:
            outcome = {"files": o.group(1), "inputs": int(o.group(2)), "answered": int(o.group(3)),
                       "struck": int(o.group(4)), "outcome": o.group(5), "reason": o.group(6)}
            outcomes.append(outcome)
            if current is not None and current["files"] == outcome["files"] and current["outcome"] is None:
                current["outcome"] = outcome
            continue
        a = ANSWERED.search(line)
        if a:
            answered.append(a.group(1))
            if current is not None and current["outcome"] is None:
                current["answered"].append(a.group(1))
    waits = [m.group(1) for m in (WAITS.search(line) for line in text) if m]
    caught = [m for m in (CAUGHT_UP.search(line) for line in text) if m]
    last = outcomes[-1] if outcomes else None
    latest = batches[-1] if batches else None
    if last is not None and last["outcome"] == "waiting" and (latest is None or latest["outcome"] is not last):
        # The newest word on the stream is a batch still waiting for its reading: an earlier batch delivered
        # never covers it.
        report.fail("the batch files={} waits for its reading: {}".format(last["files"], last["reason"]))
    elif latest is not None:
        o = latest["outcome"]
        if o is None:
            report.fail("the batch run={} files={} this start queued has no outcome line from the adapter (\"ibkr "
                        "stream batch ... outcome=...\"): it is not known to have reached the engine".format(
                            latest["run"], latest["files"]))
        elif o["outcome"] != "delivered" or o["answered"] + o["struck"] != o["inputs"] or o["inputs"] != latest["inputs"]:
            report.fail("the batch files={} ended {} with {} of {} inputs answered and {} struck off ({})".format(
                o["files"], o["outcome"], o["answered"], o["inputs"], o["struck"], o["reason"]))
        elif latest["resumed"]:
            # Taken up after a stop: its lines count the inputs that were still
            # to send (delivery.Deliverer.resume); the start that queued it sent
            # the rest, its records among them.
            report.ok("queued run={} files={} resumed after a stop, its remaining {} input(s) delivered ({} answered, "
                      "{} already committed)".format(latest["run"], latest["files"], latest["inputs"], o["answered"],
                                                     o["struck"]))
        else:
            report.ok("queued run={} files={} inputs={} (delivered: {} answered, {} already committed)".format(
                latest["run"], latest["files"], latest["inputs"], o["answered"], o["struck"]))
    else:
        if waits:
            report.fail("no stream batch was queued: this start is still waiting for its reading ({})".format(waits[-1]))
        elif expect_batch:
            report.fail("no stream batch was queued by this start, and --expect-batch says one is due (is the run's "
                        "stream folder written? docs/manual/market-data.md, \"A job for the IRAs\")")
        elif caught and connected and reconciled:
            report.ok("no new batch since start; caught up (every file through {} delivered); link connected, "
                      "reconciled".format(caught[-1].group(1)))
        else:
            report.fail("no stream batch was queued and the adapter did not say its stream is caught up (\"ibkr "
                        "stream no new file since ...; caught up\"): nothing shows this start delivered or needed to")
    unresolved = sorted({m.group(1) for m in (UNRESOLVED.search(line) for line in text) if m})
    # A resumed batch's records went before the stop: its own checks (5) were the queuing start's.
    return bool(batches) and not latest["resumed"], (latest["answered"] if latest else []), answered, unresolved


def check_engine_log(path, snapshot_version, mode, report, queued, answered, latest=()):
    """Checks 5 to 7 over the engine's durable log at path: the latest batch's records among the inputs it delivered
    (latest), and the decisions in reply to every input this start delivered (answered)."""
    try:
        groups = _committed_groups(path)[1]
    except ResolveRefused as err:
        report.fail(str(err))
        return
    mine = set(answered)
    own = [g for g in groups if g[0][1] == LOG_INPUT and g[0][2]["id"] in mine]
    decisions = [e for g in own for _, kind, e in g if kind == LOG_DECISION]
    if queued:
        batch = set(latest)
        _check_batch([g[0][2] for g in own if g[0][2]["id"] in batch], snapshot_version, report)
    sent = [e for e in decisions if e["type"] == RELEASED and e["payload"].get("send") is True]
    missing = [e["payload"].get("instrument_id") for e in decisions
               if e["type"] == ORDER_REFUSED and e["payload"].get("reason") == "missing-mark"]
    if mode == "dry-run" and sent:
        report.fail("{} order(s) released in reply to this start's inputs are marked to be sent in a dry-run: {}".format(
            len(sent), ", ".join(str(e["payload"].get("instrument_id")) for e in sent)))
    elif sent:
        report.note("{} order(s) released in reply to this start's inputs are marked to be sent ({}); whether IBKR "
                    "acknowledged each is in the order reports, not here".format(len(sent), mode))
    if missing:
        report.fail("{} order(s) refused for a missing mark: {}".format(len(missing), ", ".join(map(str, missing))))
    if not missing and not sent:
        report.ok("no order released to be sent and none refused for a missing mark")
    decided = [e for e in decisions if e["type"] == DECIDED]
    if not decided:
        report.note("no decision yet: the first is made at the close of a month's last session, from the "
                    "morning delivery after it")
    else:
        p = decided[-1]["payload"]
        report.note("latest decision: session {} {}{}".format(
            p.get("session_period_end"), p.get("outcome"),
            " ({}: {})".format(p.get("withheld_reason"), p.get("detail")) if p.get("outcome") == "withheld" else
            " with {} target(s)".format(len(p.get("targets") or []))))


def _check_batch(inputs, snapshot_version, report):
    """Check 5: the batch this start queued reached the engine, each record valid."""
    snapshots = [e for e in inputs if e["type"] == SNAPSHOT]
    if not snapshots:
        report.fail("the batch this start queued has no account snapshot among the inputs it delivered: the engine "
                    "did not receive it (an earlier start's records are never taken for this one)")
    else:
        wrong = [e["id"] for e in snapshots if e["schema_version"] != snapshot_version]
        faults = [(e["id"], snapshot_fault(e["payload"], snapshot_version)) for e in snapshots]
        faults = [(i, f) for i, f in faults if f]
        last = snapshots[-1]["payload"]
        if wrong:
            report.fail("{} account snapshot(s) are not at version {}: {}".format(len(wrong), snapshot_version,
                                                                                  ", ".join(wrong)))
        elif faults:
            report.fail("account snapshot {} has no valid {}".format(*faults[0]))
        else:
            cash = ("settled_cash", "total_cash") if snapshot_version == 4 else ()
            report.ok("account snapshot version {} as of {}: {}".format(
                snapshot_version, last.get("as_of"),
                ", ".join("{} {}".format(k, last[k]) for k in ("net_liquidation",) + cash)))
    reconciliations = [e for e in inputs if e["type"] == RECONCILIATION]
    faults = [(e["id"], reconciliation_fault(e["payload"])) for e in reconciliations]
    faults = [(i, f) for i, f in faults if f]
    if not reconciliations:
        report.fail("the batch this start queued delivered no reconciliation")
    elif faults:
        report.fail("reconciliation {} has no valid {}".format(*faults[0]))
    else:
        report.ok("reconciliation journalled ({})".format(len(reconciliations)))
    marks = [e for e in inputs if e["type"] == MARK]
    closes = [e for e in inputs if e["type"] == CLOSED]
    want = 3 if snapshot_version == 4 else 2
    wrong = [e["id"] for e in marks if e["schema_version"] != want]
    close = closes[-1]["payload"].get("period_end") if closes else None
    faults = [(e["id"], mark_fault(e["payload"], e["schema_version"], close)) for e in marks]
    faults = [(i, f) for i, f in faults if f]
    if not marks or not closes:
        report.fail("the batch this start queued delivered {} mark(s) and {} session close(s): it did not reach the "
                    "engine".format(len(marks), len(closes)))
    elif wrong:
        report.fail("{} mark(s) are not at version {}: {}".format(len(wrong), want, ", ".join(wrong[:5])))
    elif faults:
        report.fail("mark {} has no valid {}".format(*faults[0]))
    else:
        report.ok("{} mark(s) at version {}, {} session close(s), the last {}".format(
            len(marks), want, len(closes), closes[-1]["payload"].get("period_end")))


def run(journal_path, engine_log_path, client_id, snapshot_version=4, mode="dry-run", account="", expect_batch=False):
    """(lines, status) of the probe: every check's line, then passed (0) or FAILED (1)."""
    report = Report()
    queued, latest, answered = False, [], []
    try:
        with open(journal_path, encoding="utf-8") as f:
            journal = f.read().splitlines()
    except (OSError, UnicodeDecodeError) as err:
        report.fail("cannot read the adapter's journal {}: {}".format(journal_path, getattr(err, "strerror", err)))
        journal = None
    if journal is not None:
        if not [line for line in journal if line.strip()]:
            report.fail("the adapter's journal {} is empty: give the lines of its start".format(journal_path))
        queued, latest, answered, unresolved = check_journal(journal, client_id, mode, report, expect_batch)
        if unresolved:
            report.note("contracts not resolved (no mark, so no order for them): {}".format(", ".join(unresolved)))
    check_engine_log(engine_log_path, snapshot_version, mode, report, queued, answered, latest)
    if report.failures:
        report.lines.append("probe: FAILED, {} check(s)".format(report.failures))
    else:
        report.lines.append("probe: passed")
    return [mask_accounts(line, account) for line in report.lines], 1 if report.failures else 0
