"""The adapter's side of re-delivery: a reply is never lost, and no order is sent twice.

ADR 0027 section 4: the engine flushes an input's decisions to its durable
log before it replies, so durable means possibly sent, and a crash between
the flush and the reply leaves a released order on disk that the adapter
never received. The adapter asks for the committed reply to an input by
sending that input again under its ID; the engine answers with the reply it
recorded, byte for byte, and never applies the input twice, so the same
input never yields a second order.

The adapter keeps the input awaiting its reply on disk (PendingInput) until
it has acted on the reply, so its own restart asks for that input again
before sending anything new. It sends its next input only once it holds the
reply to its last, which is what lets the log show the adapter has an order
(the order's state "released").

Each released order marked send is handed to the sender once, under its
client order ID (IBKR's orderRef). Write-ahead (plan.md section 5, "No order
sent twice"): once an order's request is built and just before it is handed
over, the relay records on disk that it is about to send that client order
ID, and once the sender returns, that it sent it; a request that cannot be
built is never marked, since it never left the process. A recorded order is never handed over again. One recorded as
about to be sent when the adapter stopped may or may not have reached the
broker, so it is matched against IBKR's open orders and executions (ADR 0025
point 5): if either carries its tag the broker has it; if neither does, it
may have been taken and cancelled, or never arrived, which the relay cannot
tell apart, so it raises HandoverUnconfirmed, sends nothing more, and the
adapter stops for a person to decide. An order the broker already shows,
open or executed under any client ID, is never sent either: a second order
with the same tag is unknown to reconciliation. A dry-run release (send
false) is never sent (ADR 0026 point 3, "Mode").

The relay asks the sender's own check (a sender is an object with check and
send) before it marks an order, whatever to_request does, so an order the
sender would refuse is refused before any mark. One the sender refuses for
good (released for an earlier session, or one that can never be shaped)
is never marked and stops every exchange; the relay records which order it
is refusing, for which input, beside the pending file, and the owner settles
that order, and only that one, as never sent (`resolve ... --never-sent`), a
refusal kept with its reason, never a cancel of an order at IBKR. The relay
passes over a never-sent order only after the same check at IBKR as any
other: if IBKR shows an order or execution carrying its tag, the owner's
judgement that it never left the process is contradicted, so the relay
raises HandoverUnconfirmed (NeverSentAtBroker) and stops for a person, who
marks it sent once IBKR is checked. The engine still holds a never-sent
order as working: nothing at IBKR will ever report on it.

A person settles a stopped hand-over with the resolve command, never by
editing the record: `python3 adapter/ibkr/relay.py resolve PENDING_FILE
CLIENT_ORDER_ID --sent | --resend | --never-sent --reason TEXT`. It writes only a known
mark, settles only an order the record marks about to be sent or allowed to
be sent again (a sent mark is the relay's own proof the sender returned),
or, as never sent, the unmarked order the relay is refusing, or, as sent,
a never-sent order IBKR shows after all, and appends the decision, its
reason, its UTC time and the session date it was judged against to a
sidecar file. A resend is allowed only for an order committed in today's
session (in New York): an order from an earlier session may have filled or
been cancelled where today's open orders and executions no longer show it,
so it is marked sent instead. For the same reason a resend is recorded as a
permission dated with that session, and an adapter started on a later day
does not send it: it marks the order about to be sent again and stops
(ResendExpired). A permission can be withdrawn the same way it was given,
by marking the order sent, for an owner who finds the order at IBKR after
allowing it. A permission is spent by the send it authorises; if the adapter
crashes mid-send the order reverts to handing-over, and the same day it may
be allowed again after another check at IBKR. The record is edited
read-modify-write, so each relay holds an exclusive lock on the pending
file's lock file from construction until close, and resolve holds it while
it runs, refusing while a relay has it; a closed relay neither edits the
record nor sends. The record is read strictly (fail closed on unknown, AGENTS.md): anything but a
JSON object from client order ID to a known mark, a resend permission
included only in exactly the shape resolve writes, raises
HandoverRecordInvalid, and the adapter stops before sending.

An input ID names one input (ADR 0027 section 4): a retry under the ID of
the input awaiting its reply, with other content, is refused (InputChanged)
after the saved original is settled, and never replaces it on disk.

The owner acknowledges IBKR's correction of an execution, which stops the
adapter at every start while IBKR's replay carries it (reports.py), with
`python3 adapter/ibkr/relay.py acknowledge-correction PENDING_FILE --journal
ENGINE_LOG EXEC_ID --corrects ORIGINAL_EXEC_ID --reason TEXT`, under the
same lock, into a sidecar file read strictly at start
(PendingInput.acknowledged_corrections). ENGINE_LOG is the engine's durable
log (its -log file), which it streams as it runs (ADR 0027), so it is read
while the engine runs (journalled_fills). Only a correction of an execution
the engine's durable log already holds as a committed fill is
acknowledged: the engine keeps that original fill, and one never recorded leaves the shares in no
fill, which only a reconciliation by hand can settle (ADR 0019).

The owner acknowledges a trade IBKR reported on an order after the order's
cancellation or refusal was journalled, which also stops the adapter at
every start while IBKR's replay carries it (reports.py), with
`python3 adapter/ibkr/relay.py acknowledge-late-fill PENDING_FILE --journal
ENGINE_LOG EXEC_ID --after CLIENT_ORDER_ID --reason TEXT`, under the same
lock, into a sidecar file read strictly at start
(PendingInput.acknowledged_late_fills). Only a trade the engine's durable
log does not hold as a fill, after an order it holds a committed
cancellation or refusal of, is acknowledged. The engine refuses a fill on an
order already ended (internal/event OrderReports), so the trade is never
journalled: the acknowledgement is its record for reconciliation (ADR
0019), and the morning reconciliation shows its shares as a position
difference.
"""
from collections import namedtuple
import argparse
import fcntl
import hashlib
import json
import math
import os
import re
import sys
import threading
from datetime import date, datetime, timezone
from zoneinfo import ZoneInfo

RELEASED = "risk.order.released"
# The write-ahead record holds one of four marks for a client order ID:
# HANDING_OVER, SENT, NEVER_SENT, or a resend permission "resend:YYYY-MM-DD".
HANDING_OVER = "handing-over"
SENT = "sent"
# The owner's settlement of an order the sender refused for good: it was
# never marked, so it never left the process, and it is never sent.
NEVER_SENT = "never-sent"
MARKS = (HANDING_OVER, SENT, NEVER_SENT)
# A person's permission to send an order again, dated with the session it
# was given in: "resend:YYYY-MM-DD". It is acted on only on that date.
RESEND = "resend:"
# The only shape of a resend permission the record accepts: the one resolve
# writes, so a hand edit the interpreter's date reader happens to take (the
# basic or week-date forms) is refused, not honoured (fail closed, AGENTS.md).
RESEND_SHAPE = re.compile(r"resend:[0-9]{4}-[0-9]{2}-[0-9]{2}")
# The trading session's calendar: a session's date is its New York date.
SESSION_ZONE = ZoneInfo("America/New_York")
# How long close waits, by default, for the step in flight (seconds).
CLOSE_TIMEOUT = 60


# The longest a close may be told to wait (seconds): far below the limit of
# threading's lock acquire, so a huge timeout is refused, never an overflow.
MAX_CLOSE_TIMEOUT = 24 * 60 * 60


def _positive_seconds(name, value):
    """value as a finite number of seconds above zero and at most MAX_CLOSE_TIMEOUT, else ValueError naming the argument."""
    if (isinstance(value, bool) or not isinstance(value, (int, float)) or value != value
            or value <= 0 or value > MAX_CLOSE_TIMEOUT):
        raise ValueError("{} must be a number of seconds above zero and at most {}, not {!r}".format(
            name, MAX_CLOSE_TIMEOUT, value))
    return value


# The person's decisions on a stopped hand-over.
DECIDE_SENT = "sent"
DECIDE_RESEND = "resend"
DECIDE_NEVER_SENT = "never-sent"
DECISIONS = (DECIDE_SENT, DECIDE_RESEND, DECIDE_NEVER_SENT)
# The fields of one acknowledged correction, as acknowledge_correction writes it.
CORRECTION_FIELDS = ("at", "corrects", "exec_id", "reason")
# The fields of the relay's record of the order it is refusing.
REFUSAL_FIELDS = ("client_order_id", "input_id", "reason")
# The engine's durable log (internal/journal durable_log.go; ADR 0027;
# docs/runbooks/durable-log.md), which the engine streams as it runs: a
# header line, then for each input one group (the input's record, the
# records of the decisions it caused, then a commit naming the group's last
# record). Only a committed group was made durable; the lines after the last
# commit are a group the engine is still writing, or a crash's torn tail.
LOG_VERSION = 1
LOG_CHAIN_ALGORITHM = ("seed=sha256(canonical_log_header_bytes); "
                       "record=sha256(previous_record_hash||kind||canonical_envelope_bytes)")
LOG_HEADER_FIELDS = ("chain_algorithm", "configuration_hash", "log_version", "strategy_version")
LOG_RECORD_FIELDS = ("envelope", "kind", "record_hash", "sequence")
LOG_COMMIT_FIELDS = ("record_hash", "sequence")
LOG_INPUT = "input"
LOG_DECISION = "decision"
RECORD_HASH = re.compile(r"sha256:[0-9a-f]{64}")
# An event envelope (internal/event Envelope): its required fields, and the
# two it leaves out when empty.
ENVELOPE_VERSION = 1
ENVELOPE_FIELDS = frozenset(("id", "type", "envelope_version", "schema_version", "event_time", "recorded_at",
                             "sequence", "source", "strategy_version", "configuration_hash", "payload_hash",
                             "payload"))
ENVELOPE_OPTIONAL = frozenset(("correlation_id", "causation_id"))
PAYLOAD_HASH = re.compile(r"[0-9a-f]{64}")
TIME = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-][0-9]{2}:[0-9]{2})")
WHOLE_SECOND = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(Z|[+-][0-9]{2}:[0-9]{2})")
# A fill input, as the IBKR adapter journals it (reports.FILL_TYPE and
# FILL_VERSION: internal/event TargetWeightFillPayload).
FILL_TYPE = "execution.fill"
FILL_VERSION = 5
FILL_FIELDS = ("account_id", "client_order_id", "commission", "fill_id", "filled_at", "instrument_id", "price",
               "quantity", "side")
# A lifecycle input, as the IBKR adapter journals it (reports.LIFECYCLE_TYPE
# and LIFECYCLE_VERSION), and the statuses that end an order (reports.TERMINAL).
LIFECYCLE_TYPE = "execution.order.lifecycle"
LIFECYCLE_VERSION = 2
LIFECYCLE_TERMINAL = ("canceled", "invalid")
# The fields of one acknowledged late fill, as acknowledge_late_fill writes it.
LATE_FILL_FIELDS = ("after", "at", "exec_id", "reason")
# What the engine's durable log holds of the venue's reports: the fill IDs of
# its committed fills, and the client order IDs of the orders a committed
# lifecycle input ended.
JournalledReports = namedtuple("JournalledReports", ("fills", "ended"))


class EngineUnavailable(Exception):
    """The engine did not answer an exchange; its reply is asked for again on reconnect."""


class HandoverUnconfirmed(Exception):
    """An order was being handed over when the adapter stopped, and IBKR shows it neither open nor executed.

    It is never sent again: the adapter stops, and a person checks IBKR for
    an order whose orderRef is client_order_id before anything else is sent.
    """

    def __init__(self, client_order_id):
        super().__init__(
            "order {0} was being handed to the broker when the adapter stopped, and IBKR shows it neither open "
            "nor executed; it is not sent again and nothing else is sent: check IBKR for an order whose orderRef "
            "is {0}, then settle it with relay.py resolve (docs/manual/redelivery.md)".format(client_order_id))
        self.client_order_id = client_order_id


class NeverSentAtBroker(HandoverUnconfirmed):
    """An order settled as never sent appears in IBKR's open orders or executions; nothing more is sent.

    The never-sent settlement says the order never left the process; IBKR
    holding its tag contradicts that, so the relay stops for a person
    rather than pass over an order the broker has.
    """

    def __init__(self, client_order_id):
        Exception.__init__(
            self,
            "order {0} was settled as never sent, but IBKR shows an open order or execution whose orderRef is {0}; "
            "nothing else is sent: check the order at IBKR, then, with the adapter stopped, mark it sent with "
            "python3 adapter/ibkr/relay.py resolve PENDING_FILE {0} --sent --reason \"what IBKR shows\" "
            "(docs/manual/redelivery.md)".format(client_order_id))
        self.client_order_id = client_order_id


class ResendExpired(HandoverUnconfirmed):
    """A person allowed an order to be sent again on an earlier day; it is not sent, and is unsettled again.

    The order may have filled or been cancelled since where today's open
    orders and executions no longer show it, so the permission lapses with
    its day and the adapter stops for another check.
    """

    def __init__(self, client_order_id, allowed_on, today):
        Exception.__init__(
            self,
            "order {0} was allowed to be sent again on {1}, not today ({2}); it is not sent and nothing else is "
            "sent: check IBKR's order and trade history for every day since for an order whose orderRef is {0}, "
            "then settle it again with relay.py resolve (docs/manual/redelivery.md)".format(
                client_order_id, allowed_on, today))
        self.client_order_id = client_order_id


class PendingInUse(Exception):
    """Another relay, or the resolve command, holds the pending file; or this relay was closed and holds it no more."""


class RelayCloseTimeout(Exception):
    """close waited for the sender or a record edit in flight and gave up; the relay still holds the pending file."""


class HandoverRecordInvalid(ValueError):
    """The write-ahead record is not a JSON object from client order ID to a known mark; nothing is sent."""


class ResolveRefused(ValueError):
    """The resolve command refused a decision and changed nothing."""


class InputChanged(Exception):
    """A retry under the ID of the input awaiting its reply carried other content; the saved original was kept."""


class PendingInput:
    """The one input awaiting its reply, kept in a file that survives the adapter's restart."""

    def __init__(self, path):
        self.path = path
        self.handover_path = path + ".handover"
        self.decisions_path = path + ".handover.decisions"
        self.corrections_path = path + ".corrections"
        self.late_fills_path = path + ".late-fills"
        self.refused_path = path + ".refused"
        # Never removed: a lock lives on the file's inode, so deleting it
        # under a live holder would let a second writer lock a new file.
        self.lock_path = path + ".lock"

    def load(self):
        try:
            with open(self.path, encoding="utf-8") as f:
                return json.load(f)
        except FileNotFoundError:
            return None

    def save(self, envelope):
        # No fallback encoder: the input is asked for again exactly as it was
        # sent, and the engine refuses a different input under its ID.
        self._write(self.path, envelope)

    def handovers(self):
        """The write-ahead record: each client order ID's mark, one of MARKS or "resend:YYYY-MM-DD".

        Anything else raises HandoverRecordInvalid: a mark the relay does
        not know is never read as no mark, which would send the order again.
        """
        try:
            with open(self.handover_path, encoding="utf-8") as f:
                marks = json.load(f)
        except FileNotFoundError:
            return {}
        except ValueError as err:
            raise HandoverRecordInvalid("{} is not JSON: {}".format(self.handover_path, err)) from None
        if not isinstance(marks, dict):
            raise HandoverRecordInvalid("{} holds {}, not an object of marks".format(
                self.handover_path, type(marks).__name__))
        for cid, mark in marks.items():
            if mark not in MARKS and resend_date(mark) is None:
                raise HandoverRecordInvalid("{} marks order {} {!r}, which is none of {}".format(
                    self.handover_path, cid, mark, ", ".join(map(repr, MARKS + (RESEND + "YYYY-MM-DD",)))))
        return marks

    def mark(self, client_order_id, state):
        """Record an order's mark durably, before the call it precedes returns.

        The mark is HANDING_OVER (about to be handed over), SENT (the sender
        returned, or a person found it at IBKR), NEVER_SENT (a person settled
        an order the sender refused for good) or "resend:YYYY-MM-DD" (a
        person allowed it to be sent again in that session).
        """
        marks = self.handovers()
        marks[client_order_id] = state
        self._write(self.handover_path, marks)

    def decisions(self):
        """The person's recorded decisions on stopped hand-overs, oldest first."""
        try:
            with open(self.decisions_path, encoding="utf-8") as f:
                return [json.loads(line) for line in f if line.strip()]
        except FileNotFoundError:
            return []

    def record_decision(self, decision):
        """Append one decision durably; the file is kept for the owner and never read by the relay."""
        with open(self.decisions_path, "a", encoding="utf-8") as f:
            f.write(json.dumps(decision, sort_keys=True, separators=(",", ":")) + "\n")
            f.flush()
            os.fsync(f.fileno())
        self._sync_dir()

    def refusal(self):
        """The order the relay is refusing, as {client_order_id, input_id, reason}, or None.

        Read strictly (fail closed, AGENTS.md): anything but what
        record_refusal writes raises HandoverRecordInvalid.
        """
        try:
            with open(self.refused_path, encoding="utf-8") as f:
                entry = json.load(f)
        except FileNotFoundError:
            return None
        except ValueError as err:
            raise HandoverRecordInvalid("{} is not JSON: {}".format(self.refused_path, err)) from None
        if (not isinstance(entry, dict) or tuple(sorted(entry)) != REFUSAL_FIELDS
                or not all(isinstance(entry[k], str) for k in REFUSAL_FIELDS)
                or not entry["client_order_id"] or not entry["input_id"]):
            raise HandoverRecordInvalid("{} is not a refusal as the relay writes one".format(self.refused_path))
        return entry

    def record_refusal(self, input_id, client_order_id, reason):
        """Record durably which order of which input the relay is refusing, replacing any earlier refusal."""
        self._write(self.refused_path, {"client_order_id": client_order_id, "input_id": input_id,
                                        "reason": reason})

    def acknowledged_corrections(self):
        """The owner's acknowledged corrections: each correction's execId to the execId it corrects.

        Read strictly (fail closed, AGENTS.md): a line that is not exactly
        what acknowledge_correction writes raises HandoverRecordInvalid.
        """
        try:
            with open(self.corrections_path, encoding="utf-8") as f:
                lines = [line for line in f if line.strip()]
        except FileNotFoundError:
            return {}
        acknowledged = {}
        for n, line in enumerate(lines, 1):
            try:
                entry = json.loads(line)
            except ValueError as err:
                raise HandoverRecordInvalid("{} line {} is not JSON: {}".format(self.corrections_path, n, err)) from None
            if (not isinstance(entry, dict) or tuple(sorted(entry)) != CORRECTION_FIELDS
                    or not all(isinstance(entry[k], str) and entry[k] for k in CORRECTION_FIELDS)
                    or acknowledged.get(entry["exec_id"], entry["corrects"]) != entry["corrects"]):
                raise HandoverRecordInvalid("{} line {} is not an acknowledged correction as "
                                            "acknowledge-correction writes one".format(self.corrections_path, n))
            acknowledged[entry["exec_id"]] = entry["corrects"]
        return acknowledged

    def acknowledged_late_fills(self):
        """The owner's acknowledged late fills: each execId to the client order ID of the order it followed.

        Read strictly (fail closed, AGENTS.md): a line that is not exactly
        what acknowledge_late_fill writes raises HandoverRecordInvalid.
        """
        from reports import check_late_fills
        try:
            with open(self.late_fills_path, encoding="utf-8") as f:
                lines = [line for line in f if line.strip()]
        except FileNotFoundError:
            return {}
        acknowledged = {}
        for n, line in enumerate(lines, 1):
            try:
                entry = json.loads(line)
            except ValueError as err:
                raise HandoverRecordInvalid("{} line {} is not JSON: {}".format(self.late_fills_path, n, err)) from None
            valid = (isinstance(entry, dict) and tuple(sorted(entry)) == LATE_FILL_FIELDS
                     and all(isinstance(entry[k], str) and entry[k] for k in LATE_FILL_FIELDS)
                     and acknowledged.get(entry["exec_id"], entry["after"]) == entry["after"])
            if valid:
                try:
                    check_late_fills({entry["exec_id"]: entry["after"]})
                except ValueError:
                    valid = False
            if not valid:
                raise HandoverRecordInvalid("{} line {} is not an acknowledged late fill as "
                                            "acknowledge-late-fill writes one".format(self.late_fills_path, n))
            acknowledged[entry["exec_id"]] = entry["after"]
        return acknowledged

    def record_late_fill(self, entry):
        """Append one acknowledged late fill durably; never removed, as IBKR's execution IDs are never reused."""
        with open(self.late_fills_path, "a", encoding="utf-8") as f:
            f.write(json.dumps(entry, sort_keys=True, separators=(",", ":")) + "\n")
            f.flush()
            os.fsync(f.fileno())
        self._sync_dir()

    def record_correction(self, entry):
        """Append one acknowledged correction durably; never removed, as IBKR's execution IDs are never reused."""
        with open(self.corrections_path, "a", encoding="utf-8") as f:
            f.write(json.dumps(entry, sort_keys=True, separators=(",", ":")) + "\n")
            f.flush()
            os.fsync(f.fileno())
        self._sync_dir()

    def clear(self):
        """Remove the input, then its write-ahead record, then the record of any order refused for it.

        In that order: a stop between the two leaves marks for orders whose
        input is settled, which are harmless since a client order ID names one
        order; the other order would leave an input to settle again with no
        record of what was handed over.
        """
        for path in (self.path, self.handover_path, self.refused_path):
            try:
                os.remove(path)
            except FileNotFoundError:
                continue
            self._sync_dir()

    def _write(self, path, value):
        tmp = path + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(value, f, separators=(",", ":"), allow_nan=False)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
        self._sync_dir()

    def _sync_dir(self):
        fd = os.open(os.path.dirname(os.path.abspath(self.path)), os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)


class Relay:
    """Exchanges inputs with the engine and sends each released order once.

    connect returns an engine client whose decide(envelope) returns the reply
    envelope or raises EngineUnavailable. sender has check(request), which
    raises for any request its send would refuse, and send(request); the
    relay calls check before it marks the order and send after (TypeError at
    construction for a sender without both).
    broker_orders returns IBKR's open orders and executions
    (orders.BrokerOrders, as orders.collect_broker_orders builds it); an
    open order or execution in either list carrying a released order's client
    order ID means the broker already has that order. to_request maps a
    released order's payload and the session it was released in (its
    input's event time as a New York date, None when that cannot be read)
    to an OrderRequest; it need refuse nothing the sender's check refuses.
    reconnects bounds how often one exchange reconnects and
    asks again before EngineUnavailable reaches the caller. today returns
    the session's date (New York), against which a resend permission is
    checked. Each relay takes its own exclusive lock on the pending file at
    construction and holds it until close (PendingInUse if another relay or
    resolve has it, in this process or another); once closed it raises
    PendingInUse rather than edit the record or send. close_timeout (a finite
    number of seconds greater than zero and at most MAX_CLOSE_TIMEOUT (24
    hours); other values raise ValueError) bounds how long close waits for
    the step in flight; a close that times out raises
    RelayCloseTimeout and keeps the lock, leaving the relay open.
    """

    def __init__(self, connect, sender, pending, broker_orders, to_request, reconnects=1, today=None,
                 close_timeout=CLOSE_TIMEOUT):
        _positive_seconds("close_timeout", close_timeout)
        if not callable(getattr(sender, "check", None)) or not callable(getattr(sender, "send", None)):
            raise TypeError("the relay's sender must have check and send: it asks check before every mark")
        self._held = _lock(pending.lock_path)
        if self._held is None:
            raise PendingInUse("{} is held by another adapter or by relay.py resolve".format(pending.path))
        self._today = today or _session_today
        self._connect = connect
        self._sender = sender
        self._pending = pending
        self._broker_orders = broker_orders
        self._to_request = to_request
        self._reconnects = reconnects
        self._engine = None
        self._sent = set()
        # Serialises close against each step that edits the record or sends (the
        # save, the marks around a send, the clear), so the lock is never released while an order is going out
        # (resolve could otherwise settle it concurrently). Reentrant: a send
        # callback may close its own relay.
        self._step = threading.RLock()
        self._close_timeout = close_timeout

    def close(self, timeout=None):
        """Release the pending file, as the adapter stopping does; the closed relay edits and sends nothing more.

        Safe from any thread: it waits for the step in flight (an order's
        hand-over and its marks, or one edit of the record) to finish before
        it releases the lock, so it waits on the sender for as long as the
        sender runs. It waits at most timeout seconds (the relay's
        close_timeout when None) and then raises RelayCloseTimeout, leaving
        the relay open and holding the pending file, so a supervisor can
        decide what to do.
        """
        wait = self._close_timeout if timeout is None else _positive_seconds("timeout", timeout)
        if not self._step.acquire(timeout=wait):
            raise RelayCloseTimeout("close waited {} s for the step in flight (the sender, or an edit of {}) and "
                                    "gave up; the relay is still open and holds the pending file".format(
                                        wait, self._pending.path))
        try:
            if self._held is not None:
                os.close(self._held)
                self._held = None
        finally:
            self._step.release()

    def holds(self):
        """Whether this relay still holds the adapter's exclusive lock on the pending file (until close)."""
        return self._held is not None

    def exchange(self, envelope):
        """Send one input and act on its reply, after settling any input still awaiting one."""
        waiting = self._pending.load()
        if waiting is not None:
            if waiting.get("id") != envelope.get("id"):
                self._settle(waiting)
            elif _canonical(waiting) != _canonical(envelope):
                self._settle(waiting)
                raise InputChanged("input {} was retried with other content; the saved original was settled and "
                                   "this copy is refused".format(envelope.get("id")))
        return self._settle(envelope)

    def _require_held(self):
        if self._held is None:
            raise PendingInUse("this relay was closed and no longer holds {}; it edits and sends nothing".format(
                self._pending.path))

    def _settle(self, envelope):
        with self._step:
            self._require_held()
            self._pending.save(envelope)
        reply = self._ask(envelope)
        marks = self._pending.handovers()
        broker = None
        for order in released_orders(reply):
            cid = order["client_order_id"]
            mark = marks.get(cid)
            if cid in self._sent or mark == SENT:
                continue
            if broker is None:
                broker = _tags(self._broker_orders())
            if mark == NEVER_SENT:
                # Honoured only once IBKR shows no order or execution under
                # its tag: the settlement says it never left the process.
                if cid in broker:
                    raise NeverSentAtBroker(cid)
                continue
            if cid in broker:
                self._sent.add(cid)
                continue
            if mark == HANDING_OVER:
                raise HandoverUnconfirmed(cid)
            if mark is not None:
                allowed_on, today = resend_date(mark), self._today()
                if allowed_on != today:
                    with self._step:
                        self._require_held()
                        self._pending.mark(cid, HANDING_OVER)
                    raise ResendExpired(cid, allowed_on, today)
            # Built and checked by the sender before the mark: a request that
            # cannot be built, or that the sender would refuse, never reaches
            # the sender's send, so it leaves no mark to stop a restart.
            try:
                request = self._to_request(order, _session_date(envelope))
                self._sender.check(request)
            except Exception as err:
                # Recorded so the owner can settle this order, and only this
                # one, as never sent.
                with self._step:
                    self._require_held()
                    self._pending.record_refusal(envelope.get("id"), cid, str(err))
                raise
            with self._step:
                self._require_held()
                self._pending.mark(cid, HANDING_OVER)
                self._sender.send(request)
                self._require_held()
                self._pending.mark(cid, SENT)
            self._sent.add(cid)
        with self._step:
            self._require_held()
            self._pending.clear()
        return reply

    def _ask(self, envelope):
        for attempt in range(self._reconnects + 1):
            if self._engine is None:
                self._engine = self._connect()
            try:
                return self._engine.decide(envelope)
            except EngineUnavailable:
                self._engine = None
                if attempt == self._reconnects:
                    raise
        raise AssertionError("unreachable")


def resend_date(mark):
    """The session date of a resend permission, or None when the mark is not one in the shape resolve writes."""
    if not isinstance(mark, str) or RESEND_SHAPE.fullmatch(mark) is None:
        return None
    text = mark[len(RESEND):]
    try:
        allowed_on = date.fromisoformat(text)
    except ValueError:
        return None
    return allowed_on if allowed_on.isoformat() == text else None


def _session_today():
    return datetime.now(SESSION_ZONE).date()


def _lock(path):
    """An exclusive advisory lock on path, as an open descriptor, or None when another holder has it."""
    fd = os.open(path, os.O_RDWR | os.O_CREAT, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        os.close(fd)
        return None
    return fd


def _tags(broker):
    """Every tag IBKR's open orders and executions carry, under any client ID."""
    return {r.client_order_id for r in list(broker.open_orders) + list(broker.executions)}


def _canonical(envelope):
    """An input's content, independent of key order, for telling a retry from a changed input."""
    return json.dumps(envelope, sort_keys=True, separators=(",", ":"), allow_nan=False)


def released_orders(reply):
    """The released orders a reply carries that are to be sent, in emission order."""
    for decision in reply["payload"]["decisions"]:
        if decision.get("type") == RELEASED and decision["payload"].get("send") is True:
            yield decision["payload"]


def resolve(pending, client_order_id, decision, reason, today=None):
    """Settle a stopped hand-over: mark it sent, allow it to be sent again in today's session only, or never sent.

    never-sent settles an order the sender refused for good: the record
    holds no mark for it (it was never handed over), an input awaits its
    reply, and the order is the one the relay recorded it is refusing for
    that input; any mark, no input awaiting a reply, or any other order
    refuses it. It is kept as a refusal with its reason, and the relay
    passes over the order while IBKR shows nothing under its tag. A
    never-sent order IBKR shows after all may be marked sent.

    Refuses (ResolveRefused, nothing changed) while an adapter holds the
    pending file, an order the record does not hold or does not mark
    HANDING_OVER or a resend permission (a SENT mark is the relay's proof
    the sender returned), an unknown decision, an empty reason, a resend of
    an order already allowed one, and a resend of an order not committed in
    today's session. A resend permission can only be withdrawn, by marking
    the order sent: the owner found it at IBKR after allowing it. The order's committed session is its
    input's event time as a New York date: the engine commits the order only
    once it receives that input, so no earlier. A missing or unreadable
    event time refuses the resend. A resend is recorded as a permission
    dated today, which the relay acts on only on that date. Each decision is
    kept with its UTC time and the session date it was judged against.
    today defaults to today in New York.
    """
    if decision not in DECISIONS:
        raise ResolveRefused("decision {!r} is not one of {}".format(decision, ", ".join(map(repr, DECISIONS))))
    if not reason or not reason.strip():
        raise ResolveRefused("a reason is required: it is kept with the decision")
    held = _lock(pending.lock_path)
    if held is None:
        raise ResolveRefused("the adapter holds the pending file {}; stop it first".format(pending.path))
    try:
        _resolve(pending, client_order_id, decision, reason, today or _session_today())
    finally:
        os.close(held)


def _resolve(pending, client_order_id, decision, reason, today):
    marks = pending.handovers()
    recorded = {"client_order_id": client_order_id, "decision": decision, "reason": reason,
                "at": datetime.now(timezone.utc).isoformat(timespec="seconds"), "session_date": today.isoformat()}
    if decision == DECIDE_NEVER_SENT:
        waiting = pending.load()
        if waiting is None:
            raise ResolveRefused("no input awaits its reply in {}, so no order is stuck; check the pending "
                                 "file".format(pending.path))
        if client_order_id in marks:
            raise ResolveRefused("order {} is marked {!r}: it was handed over, or settled already, so it may be at "
                                 "IBKR; settle a stopped hand-over with --sent or --resend after checking "
                                 "IBKR".format(client_order_id, marks[client_order_id]))
        refusal = pending.refusal()
        if refusal is None or refusal["input_id"] != waiting.get("id"):
            raise ResolveRefused("the adapter has recorded no refused order for the input awaiting its reply ({}), "
                                 "so no order is refused for good; check the client order ID and the adapter's "
                                 "stop message".format(waiting.get("id")))
        if refusal["client_order_id"] != client_order_id:
            raise ResolveRefused("the adapter is refusing order {} ({}), not {}; check the client order ID".format(
                refusal["client_order_id"], refusal["reason"], client_order_id))
        pending.record_decision(recorded)
        pending.mark(client_order_id, NEVER_SENT)
        return
    if client_order_id not in marks:
        raise ResolveRefused("order {} is not in the hand-over record {}; check the client order ID".format(
            client_order_id, pending.handover_path))
    mark = marks[client_order_id]
    if mark == NEVER_SENT and decision == DECIDE_SENT:
        # IBKR shows the order after all (NeverSentAtBroker): it is never
        # sent, and the record says it may be at IBKR.
        pending.record_decision(recorded)
        pending.mark(client_order_id, SENT)
        return
    if mark != HANDING_OVER and resend_date(mark) is None:
        raise ResolveRefused("order {} is marked {!r}, not {!r}: it is not a stopped hand-over, so there is "
                             "nothing to settle; check the client order ID".format(
                                 client_order_id, mark, HANDING_OVER))
    if mark != HANDING_OVER and decision == DECIDE_RESEND:
        raise ResolveRefused("order {} is already allowed to be sent again ({!r}); only --sent is accepted, to "
                             "withdraw that permission if IBKR shows the order".format(client_order_id, mark))
    if decision == DECIDE_RESEND:
        committed = _session_date(pending.load())
        if committed != today:
            raise ResolveRefused(
                "order {} was not committed in today's session ({}; committed {}): an order from an earlier "
                "session may have filled or been cancelled where IBKR's open orders and today's trades no longer "
                "show it. Check IBKR's order and trade history for every day since, then mark it sent".format(
                    client_order_id, today, committed or "unknown"))
        pending.record_decision(recorded)
        pending.mark(client_order_id, RESEND + today.isoformat())
    else:
        pending.record_decision(recorded)
        pending.mark(client_order_id, SENT)


def journalled_fills(path):
    """The fill IDs of every committed fill input in the engine's durable log at path (journalled_reports)."""
    return journalled_reports(path).fills


def journalled_reports(path):
    """The committed fills and ended orders in the engine's durable log at path (the engine's -log file).

    A JournalledReports: the fill ID of every committed fill input, and the
    client order ID of every order a committed lifecycle input ended
    (cancelled or invalid). A lifecycle input not as the IBKR adapter
    journals one (version 2, a client order ID and a status) is refused
    like a fill.

    The engine streams that log as it runs (ADR 0027), so it is read while
    the engine runs: the lines after the last commit are a group not yet
    made durable, and a last line without its newline is one still being
    written; neither holds a fill the engine recorded. Everything else is
    read strictly (fail closed, AGENTS.md): a log that cannot be opened, a
    header, record or commit not exactly as internal/journal writes it, a
    broken sequence or group, an envelope whose payload does not match its
    hash, or a fill input that is not a whole execution.fill version 5
    raises ResolveRefused naming the line. An unreadable log is never read
    as holding no fills. The record chain is the engine's own to verify
    (engine -resume, journal-query); this reads the shape and each payload.
    """
    try:
        with open(path, "rb") as f:
            data = f.read()
    except OSError as err:
        raise ResolveRefused("cannot read the engine's durable log {}: {}".format(path, err)) from None
    # The last element is empty when the log ends with a newline, else a
    # line the engine is still writing.
    lines = data.split(b"\n")[:-1]
    if not lines:
        raise _unreadable(path, 1, "it has no complete header line")
    header = _log_json(path, 1, lines[0])
    if isinstance(header, dict) and "journal_version" in header:
        raise _unreadable(path, 1, "it is the journal the engine writes when it stops, not the durable log it "
                                   "streams as it runs (its -log file)")
    if (not isinstance(header, dict) or tuple(sorted(header)) != LOG_HEADER_FIELDS
            or not _is_count(header["log_version"]) or header["log_version"] != LOG_VERSION
            or header["chain_algorithm"] != LOG_CHAIN_ALGORITHM or not _is_text(header["configuration_hash"])
            or not _is_text(header["strategy_version"])):
        raise _unreadable(path, 1, "it is not a durable log header of version {}".format(LOG_VERSION))
    fills = set()
    ended = set()
    group = []
    sequence = 0
    last_hash = None
    for n, raw in enumerate(lines[1:], 2):
        line = _log_json(path, n, raw)
        if not isinstance(line, dict) or len(line) != 1 or not set(line) <= {"record", "commit"}:
            raise _unreadable(path, n, "a line is exactly one of a record and a commit")
        if "record" in line:
            record = line["record"]
            if not isinstance(record, dict) or tuple(sorted(record)) != LOG_RECORD_FIELDS:
                raise _unreadable(path, n, "it is not a record as the engine writes one")
            if not _is_count(record["sequence"]) or record["sequence"] != sequence + 1:
                raise _unreadable(path, n, "its sequence does not follow {}".format(sequence))
            if record["kind"] != (LOG_DECISION if group else LOG_INPUT):
                raise _unreadable(path, n, "a group is one input followed by its decisions")
            if not isinstance(record["record_hash"], str) or not RECORD_HASH.fullmatch(record["record_hash"]):
                raise _unreadable(path, n, "its record hash is not a SHA-256")
            _check_envelope(path, n, raw, record)
            sequence, last_hash = record["sequence"], record["record_hash"]
            group.append((n, record["kind"], record["envelope"]))
            continue
        commit = line["commit"]
        if (not isinstance(commit, dict) or tuple(sorted(commit)) != LOG_COMMIT_FIELDS or not group
                or commit["sequence"] != sequence or commit["record_hash"] != last_hash):
            raise _unreadable(path, n, "it is not the commit of the group before it")
        for at, kind, envelope in group:
            if envelope["type"] == FILL_TYPE:
                fills.add(_fill_id(path, at, kind, envelope))
            elif envelope["type"] == LIFECYCLE_TYPE:
                client_order_id, status = _lifecycle(path, at, kind, envelope)
                if status in LIFECYCLE_TERMINAL:
                    ended.add(client_order_id)
        group = []
    return JournalledReports(fills, ended)


def _unreadable(path, n, why):
    return ResolveRefused(
        "the engine's durable log {} cannot be read at line {}: {}. It is not read as holding no fills, and "
        "nothing is acknowledged: give the engine's -log file as the engine writes it "
        "(docs/runbooks/durable-log.md)".format(path, n, why))


def _log_json(path, n, raw):
    try:
        return json.loads(raw.decode("utf-8"))
    except ValueError as err:
        raise _unreadable(path, n, "it is not JSON ({})".format(err)) from None


def _check_envelope(path, n, raw, record):
    """Refuse an envelope not as internal/event writes one, or whose payload's bytes do not match its hash."""
    envelope = record["envelope"]
    if (not isinstance(envelope, dict) or not ENVELOPE_FIELDS <= set(envelope)
            or not set(envelope) <= ENVELOPE_FIELDS | ENVELOPE_OPTIONAL
            or not all(_is_text(envelope[k]) for k in ("id", "type", "source", "strategy_version",
                                                       "configuration_hash"))
            or envelope["envelope_version"] != ENVELOPE_VERSION or not _is_count(envelope["envelope_version"])
            or not _is_count(envelope["schema_version"]) or not _is_count(envelope["sequence"])
            or not all(_is_time(envelope[k], TIME) for k in ("event_time", "recorded_at"))
            or not all(isinstance(envelope.get(k, ""), str) for k in ENVELOPE_OPTIONAL)
            or not isinstance(envelope["payload_hash"], str) or not PAYLOAD_HASH.fullmatch(envelope["payload_hash"])):
        raise _unreadable(path, n, "its envelope is not an event envelope as the engine writes one")
    # The payload's bytes exactly as stored, which its hash attests: the
    # writer puts payload last in the envelope, straight after its hash,
    # and the record hash last in the record.
    marker = '"payload_hash":"{}","payload":'.format(envelope["payload_hash"]).encode()
    suffix = '}},"record_hash":"{}"}}}}'.format(record["record_hash"]).encode()
    start = raw.find(marker)
    if start < 0 or not raw.endswith(suffix) or hashlib.sha256(
            raw[start + len(marker):len(raw) - len(suffix)]).hexdigest() != envelope["payload_hash"]:
        raise _unreadable(path, n, "its payload does not match its payload hash")


def _fill_id(path, n, kind, envelope):
    """The fill ID of a committed fill input exactly as the engine reads one, else ResolveRefused."""
    payload = envelope["payload"]
    if (kind != LOG_INPUT or envelope["schema_version"] != FILL_VERSION or not isinstance(payload, dict)
            or tuple(sorted(payload)) != FILL_FIELDS
            or not all(_is_text(payload[k]) for k in ("account_id", "client_order_id", "fill_id", "instrument_id"))
            or payload["side"] not in ("buy", "sell") or not _is_count(payload["quantity"])
            or not _is_number(payload["price"]) or payload["price"] <= 0
            or not _is_number(payload["commission"]) or payload["commission"] < 0
            or not _is_time(payload["filled_at"], WHOLE_SECOND)):
        raise _unreadable(path, n, "it is a fill but not an input {} version {} as the engine reads one".format(
            FILL_TYPE, FILL_VERSION))
    return payload["fill_id"]


def _lifecycle(path, n, kind, envelope):
    """(client order ID, status) of a committed lifecycle input as the IBKR adapter journals one, else ResolveRefused."""
    payload = envelope["payload"]
    if (kind != LOG_INPUT or envelope["schema_version"] != LIFECYCLE_VERSION or not isinstance(payload, dict)
            or not _is_text(payload.get("client_order_id")) or not _is_text(payload.get("status"))):
        raise _unreadable(path, n, "it is a lifecycle report but not an input {} version {} as the adapter "
                                   "journals one".format(LIFECYCLE_TYPE, LIFECYCLE_VERSION))
    return payload["client_order_id"], payload["status"]


def _is_text(value):
    return isinstance(value, str) and bool(value)


def _is_count(value):
    """Whether value is a whole number above zero (not a bool)."""
    return isinstance(value, int) and not isinstance(value, bool) and value > 0


def _is_number(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def _is_time(value, shape):
    """Whether value is an RFC 3339 time with its zone, of the given shape, naming a real instant."""
    if not isinstance(value, str) or not shape.fullmatch(value):
        return False
    try:
        datetime.strptime(value[:19], "%Y-%m-%dT%H:%M:%S")
    except ValueError:
        return False
    return True


def acknowledge_correction(pending, exec_id, corrects, reason, fills):
    """Record the owner's acknowledgement that IBKR's execution exec_id corrects execution corrects.

    fills holds the fill IDs the engine's durable log holds (journalled_fills).
    Refuses (ResolveRefused, nothing changed) while an adapter holds the
    pending file, an empty reason, a pair that is not a correction (the same
    execId base, another last segment: reports.check_acknowledged), a
    correction of an execution the journal does not hold as a fill (the
    engine has no original to keep, so the shares are reconciled by hand),
    and a correction already acknowledged as correcting another execution. The
    journal then passes over that correction on every start; it is never a
    fill, so the engine keeps the original's figures and the owner
    reconciles the difference (ADR 0019).
    """
    from reports import check_acknowledged
    if not reason or not reason.strip():
        raise ResolveRefused("a reason is required: it is kept with the acknowledgement")
    try:
        check_acknowledged({exec_id: corrects})
    except ValueError as err:
        raise ResolveRefused(str(err)) from None
    if corrects not in fills:
        raise ResolveRefused(
            "execution {} was never recorded as a fill in the engine's durable log, so there is no original fill for "
            "the engine to keep: the correction is not acknowledged. Reconcile the order's shares by hand "
            "(docs/runbooks/reconciliation.md) before the adapter starts again".format(corrects))
    held = _lock(pending.lock_path)
    if held is None:
        raise ResolveRefused("the adapter holds the pending file {}; stop it first".format(pending.path))
    try:
        known = pending.acknowledged_corrections().get(exec_id)
        if known is not None and known != corrects:
            raise ResolveRefused("execution {} is already acknowledged as a correction of {}".format(exec_id, known))
        pending.record_correction({"exec_id": exec_id, "corrects": corrects, "reason": reason,
                                   "at": datetime.now(timezone.utc).isoformat(timespec="seconds")})
    finally:
        os.close(held)


def acknowledge_late_fill(pending, exec_id, after, reason, fills, ended):
    """Record the owner's acknowledgement that IBKR's execution exec_id was reported after order after ended.

    fills and ended are what the engine's durable log holds
    (journalled_reports). Refuses (ResolveRefused, nothing changed) while an
    adapter holds the pending file, an empty reason, an execId without the
    base and last segment IBKR's have or an empty order
    (reports.check_late_fills), a trade the log holds as a fill, an order
    the log does not hold a committed cancellation or refusal of, and a
    trade already acknowledged as after another order. The journal then
    passes over that trade on every start. The engine refuses a fill on an
    order already ended (internal/event OrderReports), so it is never a
    fill: this record and the morning reconciliation, which shows its shares
    as a position difference, are how it is settled (ADR 0019).
    """
    from reports import check_late_fills
    if not reason or not reason.strip():
        raise ResolveRefused("a reason is required: it is kept with the acknowledgement")
    try:
        check_late_fills({exec_id: after})
    except ValueError as err:
        raise ResolveRefused(str(err)) from None
    if exec_id in fills:
        raise ResolveRefused("execution {} is recorded as a fill in the engine's durable log: it is not a trade "
                             "after its order ended, so there is nothing to acknowledge".format(exec_id))
    if after not in ended:
        raise ResolveRefused(
            "order {} was never recorded as cancelled or refused in the engine's durable log, so execution {} is "
            "not a trade after it ended: check the order's trades in IBKR (docs/manual/paper-trading.md)".format(
                after, exec_id))
    held = _lock(pending.lock_path)
    if held is None:
        raise ResolveRefused("the adapter holds the pending file {}; stop it first".format(pending.path))
    try:
        known = pending.acknowledged_late_fills().get(exec_id)
        if known is not None and known != after:
            raise ResolveRefused("execution {} is already acknowledged as a trade after order {}".format(
                exec_id, known))
        pending.record_late_fill({"exec_id": exec_id, "after": after, "reason": reason,
                                  "at": datetime.now(timezone.utc).isoformat(timespec="seconds")})
    finally:
        os.close(held)


def _session_date(envelope):
    """The New York date of an input's event time, or None when it has none that can be read."""
    text = (envelope or {}).get("event_time")
    if not isinstance(text, str):
        return None
    try:
        at = datetime.fromisoformat(text.replace("Z", "+00:00"))
    except ValueError:
        return None
    if at.tzinfo is None:
        return None
    return at.astimezone(SESSION_ZONE).date()


def main(argv=None, today=None):
    """The relay's operator command; returns the process exit status."""
    parser = argparse.ArgumentParser(prog="relay.py",
                                     description="The owner's settlements while the adapter is stopped.")
    commands = parser.add_subparsers(dest="command", required=True)
    cmd = commands.add_parser("resolve", help="mark a stopped hand-over sent, allow it to be sent again, or "
                                              "settle an order the sender refused for good as never sent")
    cmd.add_argument("pending_file", help="the adapter's pending-input file (not the .handover file)")
    cmd.add_argument("client_order_id")
    which = cmd.add_mutually_exclusive_group(required=True)
    which.add_argument("--sent", dest="decision", action="store_const", const=DECIDE_SENT,
                       help="IBKR shows the order at all (working, filled or cancelled): never send it again; "
                            "also for a never-sent order IBKR shows after all")
    which.add_argument("--resend", dest="decision", action="store_const", const=DECIDE_RESEND,
                       help="IBKR has no trace of it and it was committed today: send it again at its original limit")
    which.add_argument("--never-sent", dest="decision", action="store_const", const=DECIDE_NEVER_SENT,
                       help="the order the adapter is refusing before any mark, which it always will: never "
                            "send it")
    cmd.add_argument("--reason", required=True, help="what IBKR showed, or why it was refused; kept with the decision")
    ack = commands.add_parser("acknowledge-correction",
                              help="acknowledge IBKR's correction of an execution, so the adapter can start again")
    ack.add_argument("pending_file", help="the adapter's pending-input file")
    ack.add_argument("--journal", required=True, metavar="ENGINE_LOG",
                     help="the engine's durable log (its -log file, which it writes as it runs; ADR 0027), which "
                          "must hold the corrected execution as a committed fill")
    ack.add_argument("exec_id", help="the correction's execution ID")
    ack.add_argument("--corrects", required=True, help="the execution ID it corrects (the fill the engine holds)")
    ack.add_argument("--reason", required=True, help="what IBKR's trades show; kept with the acknowledgement")
    late = commands.add_parser("acknowledge-late-fill",
                               help="acknowledge a trade IBKR reported after its order was recorded cancelled or "
                                    "refused, so the adapter can start again; it is left to reconciliation")
    late.add_argument("pending_file", help="the adapter's pending-input file")
    late.add_argument("--journal", required=True, metavar="ENGINE_LOG",
                      help="the engine's durable log (its -log file), which must hold the order's cancellation or "
                           "refusal and not the trade as a fill")
    late.add_argument("exec_id", help="the trade's execution ID")
    late.add_argument("--after", required=True, metavar="CLIENT_ORDER_ID",
                      help="the client order ID of the order the trade was reported after")
    late.add_argument("--reason", required=True, help="what IBKR's trades show; kept with the acknowledgement")
    args = parser.parse_args(argv)
    pending = PendingInput(args.pending_file)
    try:
        if args.command == "acknowledge-late-fill":
            reports = journalled_reports(args.journal)
            acknowledge_late_fill(pending, args.exec_id, args.after, args.reason, reports.fills, reports.ended)
            print("execution {} acknowledged as a trade after order {} ended; it is not recorded as a fill: "
                  "reconcile the account".format(args.exec_id, args.after))
            return 0
        if args.command == "acknowledge-correction":
            acknowledge_correction(pending, args.exec_id, args.corrects, args.reason,
                                   journalled_fills(args.journal))
            print("execution {} acknowledged as a correction of {}".format(args.exec_id, args.corrects))
            return 0
        resolve(pending, args.client_order_id, args.decision, args.reason, today=today)
    except (ResolveRefused, HandoverRecordInvalid) as err:
        print("refused: {}".format(err), file=sys.stderr)
        return 1
    print("order {} resolved: {}".format(args.client_order_id, args.decision))
    return 0


if __name__ == "__main__":
    sys.exit(main())
