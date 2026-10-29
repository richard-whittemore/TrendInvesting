"""The same-day probe: after an adapter install, read what the adapter logged and what the engine journalled, and say plainly whether the account's link works.

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

The checks, in the order a start makes them true:

1. the adapter started, in the mode its configuration names, with its own
   client ID, sending off unless the mode is paper;
2. it connected to IB Gateway with that client ID;
3. it reconciled the account with that client ID;
4. it queued a batch of the run's stream (the warm-up, or a later day's);
5. the engine journalled an account snapshot at the version the engine
   reads (4 for an IRA: settled and total cash, both numbers) and a
   reconciliation;
6. the engine journalled the batch's marks, each at version 3 with its IBKR
   contract when the snapshot version is 4, and the session closes;
7. no order the engine released is marked to be sent in dry-run, and no
   order was refused for a missing mark (missing-mark);
8. the engine's latest decision, decided or withheld with its reason, or
   that none is due yet: reported, never a failure by itself.

Contracts the adapter could not resolve are listed from its warnings, with
IBKR's reason: each such name has no mark, by design (ADR 0026 point 3).
"""
import re

from masking import mask_accounts
from relay import LOG_DECISION, LOG_INPUT, ResolveRefused, _committed_groups

STARTED = re.compile(r"ibkr adapter starting mode=(\S+) client_id=(\d+) sending=(on|off) ibapi=(\S+)")
CONNECTED = re.compile(r"ibkr link state=connected reason=(\S*) client_id=(\d+)")
RECONCILED = re.compile(r"ibkr reconciled client_id=(\d+) positions=(\d+) open_orders=(\d+) executions=(\d+)")
QUEUED = re.compile(r"ibkr stream queued run=(\S+) files=(\S+) inputs=(\d+)")
UNRESOLVED = re.compile(r"ibkr contract for (\S+) not resolved")
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
    return isinstance(value, (int, float)) and not isinstance(value, bool)


def check_journal(lines, client_id, mode, report):
    """Checks 1 to 4 over the adapter's journal lines for one start; the names it could not resolve."""
    text = [line.rstrip("\n") for line in lines]
    for line in text:
        for pattern, what in ((STOPPED, "the adapter stopped"), (REFUSED, "the adapter refused to start")):
            m = pattern.search(line)
            if m:
                report.fail("{}: {}".format(what, m.group(1)))
    started = [STARTED.search(line) for line in text]
    started = [m for m in started if m]
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
    for pattern, what in ((CONNECTED, "connected to IB Gateway"), (RECONCILED, "reconciled the account")):
        found = [m for m in (pattern.search(line) for line in text) if m]
        ids = {int(m.group(2) if pattern is CONNECTED else m.group(1)) for m in found}
        if not found:
            report.fail("no line shows the adapter {}".format(what))
        elif ids != {client_id}:
            report.fail("the adapter {} as client_id {}, not {}".format(what, sorted(ids), client_id))
        elif pattern is RECONCILED:
            m = found[-1]
            report.ok("reconciled client_id={}: positions={} open_orders={} executions={}".format(*m.groups()))
        else:
            report.ok("connected to IB Gateway as client_id={} ({} time(s))".format(client_id, len(found)))
    queued = [m for m in (QUEUED.search(line) for line in text) if m]
    if not queued:
        report.fail("no stream batch was queued: the warm-up was not delivered (is the run's stream folder "
                    "written? docs/manual/market-data.md, \"A job for the IRAs\")")
    else:
        for m in queued:
            report.ok("queued run={} files={} inputs={}".format(*m.groups()))
    return sorted({m.group(1) for m in (UNRESOLVED.search(line) for line in text) if m})


def check_engine_log(path, snapshot_version, mode, report):
    """Checks 5 to 8 over the engine's durable log at path."""
    try:
        groups = _committed_groups(path)[1]
    except ResolveRefused as err:
        report.fail(str(err))
        return
    inputs = [g[0][2] for g in groups if g[0][1] == LOG_INPUT]
    decisions = [e for g in groups for _, kind, e in g if kind == LOG_DECISION]
    snapshots = [e for e in inputs if e["type"] == SNAPSHOT]
    if not snapshots:
        report.fail("the engine journalled no account snapshot")
    else:
        last = snapshots[-1]
        payload = last["payload"]
        wrong = [e["id"] for e in snapshots if e["schema_version"] != snapshot_version]
        cash = ("settled_cash", "total_cash") if snapshot_version == 4 else ()
        malformed = [k for k in ("net_liquidation",) + cash if not _number(payload.get(k))]
        if wrong:
            report.fail("{} account snapshot(s) are not at version {}: {}".format(len(wrong), snapshot_version,
                                                                                  ", ".join(wrong)))
        elif malformed:
            report.fail("the latest account snapshot {} carries no number for {}".format(last["id"],
                                                                                        ", ".join(malformed)))
        else:
            report.ok("account snapshot version {} as of {}: {}".format(
                snapshot_version, payload.get("as_of"),
                ", ".join("{} {}".format(k, payload[k]) for k in ("net_liquidation",) + cash)))
    if not [e for e in inputs if e["type"] == RECONCILIATION]:
        report.fail("the engine journalled no reconciliation")
    else:
        report.ok("reconciliation journalled ({})".format(len([e for e in inputs if e["type"] == RECONCILIATION])))
    marks = [e for e in inputs if e["type"] == MARK]
    closes = [e for e in inputs if e["type"] == CLOSED]
    want = 3 if snapshot_version == 4 else 2
    bad = [e["id"] for e in marks if e["schema_version"] != want or (
        want == 3 and not (isinstance(e["payload"].get("contract"), dict)
                           and isinstance(e["payload"]["contract"].get("con_id"), int)
                           and e["payload"]["contract"]["con_id"] > 0))]
    if not marks or not closes:
        report.fail("the engine journalled {} mark(s) and {} session close(s): the stream did not reach it".format(
            len(marks), len(closes)))
    elif bad:
        report.fail("{} mark(s) are not at version {} with their IBKR contract: {}".format(len(bad), want,
                                                                                         ", ".join(bad[:5])))
    else:
        report.ok("{} mark(s) at version {}, {} session close(s), the last {}".format(
            len(marks), want, len(closes), closes[-1]["payload"].get("period_end")))
    sent = [e for e in decisions if e["type"] == RELEASED and e["payload"].get("send") is True]
    if mode == "dry-run" and sent:
        report.fail("{} order(s) released to be sent in a dry-run".format(len(sent)))
    missing = [e["payload"].get("instrument_id") for e in decisions
               if e["type"] == ORDER_REFUSED and e["payload"].get("reason") == "missing-mark"]
    if missing:
        report.fail("{} order(s) refused for a missing mark: {}".format(len(missing), ", ".join(map(str, missing))))
    elif not (mode == "dry-run" and sent):
        report.ok("no order sent and none refused for a missing mark")
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


def run(journal_path, engine_log_path, client_id, snapshot_version=4, mode="dry-run", account=""):
    """(lines, status) of the probe: every check's line, then passed (0) or FAILED (1)."""
    report = Report()
    try:
        with open(journal_path, encoding="utf-8") as f:
            journal = f.read().splitlines()
    except (OSError, UnicodeDecodeError) as err:
        report.fail("cannot read the adapter's journal {}: {}".format(journal_path, getattr(err, "strerror", err)))
        journal = None
    if journal is not None:
        if not [line for line in journal if line.strip()]:
            report.fail("the adapter's journal {} is empty: give the lines of its start".format(journal_path))
        unresolved = check_journal(journal, client_id, mode, report)
        if unresolved:
            report.note("contracts not resolved (no mark, so no order for them): {}".format(", ".join(unresolved)))
    check_engine_log(engine_log_path, snapshot_version, mode, report)
    if report.failures:
        report.lines.append("probe: FAILED, {} check(s)".format(report.failures))
    else:
        report.lines.append("probe: passed")
    return [mask_accounts(line, account) for line in report.lines], 1 if report.failures else 0
