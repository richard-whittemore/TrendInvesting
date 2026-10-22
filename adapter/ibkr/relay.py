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
good is never marked. The owner's decision of 2026-10-22 splits it in two.
One released for a session that is over (sender.SessionOver) is retired by
the relay itself (retire_order): journalled for the engine as a refusal for
"session over" by the adapter (execution.order.retired), never a cancel of
an order at IBKR, recorded as a retirement, marked never sent, and the
relay goes on. One IBKR can never accept as it stands
(sender.UnsendableOrder: not a limit order, a fraction of a share, no
contract ID) stops every exchange (OrderUnsendable), and the owner retires
it with `relay.py retire` once the cause is found, which journals the same
refusal with the owner's reason; that is its only settlement, since only it
tells the engine. A retirement a stop left unmarked is finished on restart
before the order is judged sendable again. Any other refusal for good stops
every exchange too. Each stop records which order the relay is refusing, for
which input, beside the pending file, and the owner settles that order, and
only that one, as never sent (`resolve ... --never-sent`), a refusal kept
with its reason, never a cancel of an order at IBKR; resolve refuses it for
an order IBKR can never accept, whose remedies are fixing the cause and
`retire`. The relay
passes over a never-sent order only after the same check at IBKR as any
other: if IBKR shows an order or execution carrying its tag, the owner's
judgement that it never left the process is contradicted, so the relay
raises HandoverUnconfirmed (NeverSentAtBroker) and stops for a person, who
marks it sent once IBKR is checked. The engine still holds a never-sent
order as working: nothing at IBKR will ever report on it.

A sender whose send raises a HandoverUnconfirmed with handed_over False
says nothing reached the broker (the real client's socket closed before
placeOrder): the order stays marked about to be sent, and the relay records
that fact for that order and input beside the pending file
(PendingInput.not_handed_over), which only resolve's not-handed-over
decision reads. The record is removed before every hand-over, so it never
outlives a later attempt that may have reached the broker.

A person settles a stopped hand-over with the resolve command, never by
editing the record: `python3 adapter/ibkr/relay.py resolve PENDING_FILE
CLIENT_ORDER_ID --sent | --resend | --never-sent | --not-handed-over
--reason TEXT`. It writes only a known
mark, settles only an order the record marks about to be sent or allowed to
be sent again (a sent mark is the relay's own proof the sender returned),
or, as never sent, the unmarked order the relay is refusing or a marked
order the relay recorded was not handed over, or, as sent, a never-sent
order IBKR shows after all, and appends the decision, its
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

Re-delivery asks only the run the input was sent to (ADR 0027, the
restart model: a crash resumes the same run from its log; every other
restart is a fresh run that decides afresh from the reconciled account).
The adapter records which run it sends each input to (PendingInput.record_run,
run_identity); an input awaiting its reply from an earlier run is never
sent to a fresh run, and an order that run released but the adapter never
sent is never sent: the adapter stops for a person (EarlierRunInput), who
settles it with `python3 adapter/ibkr/relay.py resolve --run-ended
PENDING_FILE INPUT_ID --journal ENGINE_LOG --reason TEXT`
(resolve_run_ended), refused for an input of the run whose durable log is
ENGINE_LOG.

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
cancellation or refusal of, or the input awaiting its reply cancels or
refuses (the journal holds that input among its own, so both judge "ended"
alike), is acknowledged. The engine refuses a fill on an order already
ended (internal/event OrderReports), so the trade is never journalled: the
acknowledgement is its record for reconciliation. The next start's
reconciliation (ADR 0025 point 5) reports its shares as an unexplained
position difference and the engine goes Degraded then (ADR 0019) until the
reconciliation runbook's recovery settles it.
"""
from collections import namedtuple
import argparse
import contextlib
import fcntl
import json
import math
import os
import re
import sys
import threading
import importlib.util
from datetime import date, datetime, timedelta, timezone
from zoneinfo import ZoneInfo

from sender import SessionOver, UnsendableOrder

_LOADED = {}


def marketdata_module(name):
    """The data job's module adapter/marketdata/<name>.py, loaded from its file, once per process.

    The adapter is installed beside it in the same layout (provision.sh
    adapter), so the data job and the adapter share one reader of the
    engine's durable log (engine_log.py), one NYSE calendar (nyse_calendar.py)
    and one table of the inputs a stream delivers (kinds.py).
    """
    if name not in _LOADED:
        path = os.path.join(os.path.dirname(os.path.abspath(__file__)), os.pardir, "marketdata", name + ".py")
        spec = importlib.util.spec_from_file_location("marketdata_" + name, path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        _LOADED[name] = module
    return _LOADED[name]


engine_log = marketdata_module("engine_log")

RELEASED = "risk.order.released"
# The owner's Run approval, a fresh run's second input: its previous_log
# names, without a directory, the durable log of the run before it, beside
# the fresh run's own (internal/event.OperatorRunApprovedPayload).
RUN_APPROVED = engine_log.RUN_APPROVED
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
# A marked order the relay recorded its sender did not hand over: settled
# as never sent, on any day, after the owner checks IBKR's order history.
DECIDE_NOT_HANDED_OVER = "not-handed-over"
DECISIONS = (DECIDE_SENT, DECIDE_RESEND, DECIDE_NEVER_SENT, DECIDE_NOT_HANDED_OVER)
# A retirement of an order the sender can never send (retire_order), which
# the decisions file records beside the person's decisions above: the
# relay's own for an order whose session is over, the owner's (retire) for
# one IBKR can never accept as it stands. The owner's decision of 2026-10-22.
DECIDE_RETIRED = "retired"
RECORDED_DECISIONS = DECISIONS + (DECIDE_RETIRED,)
# The input a retirement is journalled as (internal/event OrderRetiredPayload):
# a refusal of an order that never left the adapter, never a cancel of an
# order at IBKR. Its reason is one of two, and who retired it fits the reason.
RETIRED_TYPE = "execution.order.retired"
RETIRED_VERSION = 1
RETIRED_SESSION_OVER = "session over"
RETIRED_BY_OWNER = "owner retired"
RETIRED_BY_ADAPTER = "adapter"
# A retirement's outbox entry is named after its order, so a stop between
# recording it and marking the order retires the order again without a
# second record (Outbox.append_once).
RETIRED_ID = "order-retired:"
# The input an earlier engine run left awaiting its reply (resolve_run_ended),
# settled apart from the order decisions above.
DECIDE_RUN_ENDED = "run-ended"
# The fields of one acknowledged correction, as acknowledge_correction writes it.
CORRECTION_FIELDS = ("at", "corrects", "exec_id", "reason")
# The settlement of an input an earlier engine run left awaiting its reply
# (resolve_run_ended): the input and the orders it released unsent are
# refused for this reason, never sent and never sent again.
RUN_ENDED = "run ended"
# The fields of the adapter's record of which run an input was sent to
# (PendingInput.record_run), and of one settled input (record_run_ended).
RUN_FIELDS = ("input_id", "log", "run")
RUN_ENDED_FIELDS = ("at", "input", "outbox", "reason", "refused", "run", "session_date", "unsent")
# The adapter's outbox (Outbox), beside the pending file, and the fields of
# one of its entries.
OUTBOX_SUFFIX = ".outbox"
OUTBOX_FIELDS = ("event_time", "event_type", "id", "payload", "schema_version")
# An entry from a market-data stream (delivery.py) also carries the
# envelope as the data job wrote it, which is sent as written but for its
# sequence and recorded time; a stream batch's broker snapshot carries the
# run it was read for, so neither is ever sent to another run.
OUTBOX_STREAM_FIELDS = tuple(sorted(OUTBOX_FIELDS + ("envelope",)))
OUTBOX_RUN_FIELDS = tuple(sorted(OUTBOX_FIELDS + ("run",)))
# An input that awaits its reply from an earlier run (earlier_run): its ID,
# the earlier run's log file name (None when no log of the chain holds it
# and the adapter recorded none), whether that run committed it, the orders
# its committed reply released that the record does not mark sent, as
# (client order ID, mark), and the run the adapter recorded sending it to.
EarlierRun = namedtuple("EarlierRun", ("input_id", "log", "committed", "unsent", "run"))
# The fields of the relay's record of the order it is refusing: as the relay
# first wrote it, and with whether the order is of a shape IBKR can never
# accept (unsendable) and, for one that is, the release itself (order), which
# the owner's retire command journals.
REFUSAL_FIELDS = ("client_order_id", "input_id", "reason")
REFUSAL_SHAPE_FIELDS = ("client_order_id", "input_id", "order", "reason", "unsendable")
# The engine's durable log (internal/journal durable_log.go; ADR 0027;
# docs/runbooks/durable-log.md), which the engine streams as it runs: a
# header line, then for each input one group (the input's record, the
# records of the decisions it caused, then a commit naming the group's last
# record). Only a committed group was made durable; the lines after the last
# commit are a group the engine is still writing, or a crash's torn tail.
LOG_VERSION = engine_log.LOG_VERSION
LOG_CHAIN_ALGORITHM = engine_log.LOG_CHAIN_ALGORITHM
LOG_HEADER_FIELDS = engine_log.LOG_HEADER_FIELDS
LOG_RECORD_FIELDS = engine_log.LOG_RECORD_FIELDS
LOG_COMMIT_FIELDS = engine_log.LOG_COMMIT_FIELDS
LOG_INPUT = engine_log.LOG_INPUT
LOG_DECISION = engine_log.LOG_DECISION
RECORD_HASH = engine_log.RECORD_HASH
# An event envelope (internal/event Envelope): its required fields, and the
# two it leaves out when empty.
ENVELOPE_VERSION = engine_log.ENVELOPE_VERSION
ENVELOPE_FIELDS = engine_log.ENVELOPE_FIELDS
ENVELOPE_OPTIONAL = engine_log.ENVELOPE_OPTIONAL
PAYLOAD_HASH = engine_log.PAYLOAD_HASH
TIME = engine_log.TIME
TIME_PARTS = engine_log.TIME_PARTS
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
# The orders the adapter's own durable records settle (PendingInput.settled_orders),
# which no engine's log may hold a report on: the client order IDs ended, and
# each struck-off fill as fill ID to (client order ID, quantity).
SettledOrders = namedtuple("SettledOrders", ("ended", "fills"))
# One committed input in the engine's durable log, and the log as the adapter
# reads it (read_engine_log).
CommittedInput = namedtuple("CommittedInput", ("id", "event_type", "schema_version", "sequence", "payload"))
EngineLog = namedtuple("EngineLog", ("configuration_hash", "strategy_version", "inputs", "next_sequence"))
# The durable log as _committed_groups reads it (engine_log.CommittedLog).
CommittedLog = engine_log.CommittedLog


class EngineUnavailable(Exception):
    """The engine did not answer an exchange; its reply is asked for again on reconnect."""


class HandoverUnconfirmed(Exception):
    """An order was being handed over when the adapter stopped, and IBKR shows it neither open nor executed.

    It is never sent again: the adapter stops, and a person checks IBKR for
    an order whose orderRef is client_order_id before anything else is sent.
    handed_over is None: whether the broker has the order is not known. A
    sender's subclass sets it False only when it knows nothing reached the
    broker, which the relay records for resolve's not-handed-over decision.
    """

    handed_over = None

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


class OrderUnsendable(ValueError):
    """An order IBKR can never accept as it stands was refused before any mark; the adapter stops at it for the owner.

    Not a limit order, a fraction of a share, or no IBKR contract ID
    (sender.UnsendableOrder): the owner's decision of 2026-10-22 stops the
    adapter at that order, and the owner, once the cause is found, retires
    it with `relay.py retire` (retire), which journals the refusal with the
    owner's reason so the adapter goes on.
    """

    def __init__(self, client_order_id, pending_path, why):
        super().__init__(
            "order {0} can never be sent as it stands ({1}); it is never sent and the adapter stops here. Find "
            "the cause, then, with the adapter stopped, retire it: python3 adapter/ibkr/relay.py retire {2} {0} "
            "--by NAME --reason \"why\" (docs/manual/paper-trading.md)".format(client_order_id, why, pending_path))
        self.client_order_id = client_order_id


class PendingInUse(Exception):
    """Another relay, or the resolve command, holds the pending file; or this relay was closed and holds it no more."""


class RelayCloseTimeout(Exception):
    """close waited for the sender or a record edit in flight and gave up; the relay still holds the pending file."""


class HandoverRecordInvalid(ValueError):
    """The write-ahead record is not a JSON object from client order ID to a known mark; nothing is sent."""


class ResolveRefused(ValueError):
    """The resolve command refused a decision and changed nothing."""


class EarlierRunInput(Exception):
    """The input awaiting its reply was sent to an earlier engine run, not the one now running; nothing is sent.

    ADR 0027, amendment of 2026-10-17 (the restart model): a crash resumes
    the same run from its log, and every other restart is a fresh run that
    decides afresh from the reconciled account. A fresh run's engine refuses
    an earlier run's input as out of sequence, so it is never carried into
    one, and an order the earlier run released but the adapter never sent is
    never sent by it. The adapter stops for a person, who settles the input
    with `relay.py resolve --run-ended` (resolve_run_ended). ended is the
    EarlierRun that names it.
    """

    def __init__(self, ended, pending_path, engine_log_path):
        if ended.committed and ended.unsent:
            orders = ", ".join("{} ({})".format(cid, "it may have reached IBKR: it was being handed over"
                                                if mark == HANDING_OVER else "never sent")
                               for cid, mark in ended.unsent)
            what = "committed it and released order(s) the adapter never sent: {}".format(orders)
        elif ended.committed:
            what = "committed it, and every order it released was sent"
        else:
            what = "did not commit it"
        super().__init__(
            "input {0} awaits its reply from an earlier engine run ({1}), not from this run ({2}); that run {3}. "
            "A fresh run never takes an earlier run's input, and never sends an earlier run's orders: nothing is "
            "sent. If this start was meant to resume a crashed run, stop and resume it instead "
            "(docs/runbooks/durable-log.md). Otherwise check IBKR for the orders named, and cancel at IBKR any of them "
            "still working, and any order the earlier run sent that is still open, confirming each is cancelled; "
            "then, with the adapter stopped, settle the input (with the inputs recorded behind it): "
            "python3 adapter/ibkr/relay.py resolve --run-ended {4} {0} --journal {2} "
            "--reason \"a fresh run after ...\" (docs/manual/paper-trading.md)".format(
                ended.input_id, ended.log or "a log the adapter has no record of", engine_log_path, what,
                pending_path))
        self.ended = ended


class OutboxInvalid(ValueError):
    """The outbox file is not as the adapter writes it; the adapter stops before sending anything."""


class Outbox:
    """The inputs recorded for the engine and not yet struck off, oldest first, in a JSON file rewritten atomically.

    Every change is a read-modify-write of the whole file, made only under
    an exclusive lock on lock_path (path + ".lock"), held for that one
    change: the adapter and any producer of its inputs in another process
    (the session's close, say) write it only through append, so neither
    loses the other's entry. Never edit the file by hand while either runs.
    Reads need no lock: each write replaces the file whole.
    """

    def __init__(self, path):
        self.path = path
        self.lock_path = path + ".lock"

    def load(self):
        try:
            with open(self.path, encoding="utf-8") as f:
                entries = json.load(f)
        except FileNotFoundError:
            return []
        except ValueError as err:
            raise OutboxInvalid("{} is not JSON: {}".format(self.path, err)) from None
        if not isinstance(entries, list) or not all(_is_outbox_entry(e) for e in entries):
            raise OutboxInvalid("{} is not a list of inputs as the adapter records them".format(self.path))
        if len({e["id"] for e in entries}) != len(entries):
            raise OutboxInvalid("{} names one input twice".format(self.path))
        return entries

    def append(self, entry):
        self.extend([entry])

    def extend(self, entries):
        """Add entries behind every entry already recorded, in order, in one write: all of them or none.

        The whole list is checked as load reads it before anything is
        written (OutboxInvalid), so an entry the outbox could not be read
        with is never recorded.
        """
        with self._locked():
            merged = self.load() + list(entries)
            if not all(_is_outbox_entry(e) for e in merged):
                raise OutboxInvalid("an input for {} is not as the adapter records one; nothing was recorded".format(
                    self.path))
            if len({e["id"] for e in merged}) != len(merged):
                raise OutboxInvalid("an input for {} names an input the outbox or the same write already names; "
                                    "nothing was recorded".format(self.path))
            self._write(merged)

    def append_once(self, entry):
        """Add entry behind every entry recorded, unless one under its ID is already there; True when added.

        Checked and written under the lock, as extend is, so a retirement
        recorded before a stop is never recorded twice when it is made again.
        """
        with self._locked():
            entries = self.load()
            if any(e["id"] == entry["id"] for e in entries):
                return False
            if not _is_outbox_entry(entry):
                raise OutboxInvalid("an input for {} is not as the adapter records one; nothing was recorded".format(
                    self.path))
            self._write(entries + [entry])
            return True

    def strike(self, input_id):
        """Remove the entry for input_id; one not there is already struck off."""
        with self._locked():
            entries = self.load()
            kept = [e for e in entries if e["id"] != input_id]
            if len(kept) != len(entries):
                self._write(kept)

    def strike_all(self, input_ids):
        """Remove the entries for every one of input_ids in one write: all of them or none."""
        gone = set(input_ids)
        with self._locked():
            entries = self.load()
            kept = [e for e in entries if e["id"] not in gone]
            if len(kept) != len(entries):
                self._write(kept)

    @contextlib.contextmanager
    def _locked(self):
        # Never removed: the lock lives on the file's inode.
        fd = os.open(self.lock_path, os.O_RDWR | os.O_CREAT, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX)
            yield
        finally:
            os.close(fd)

    def _write(self, entries):
        tmp = self.path + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(entries, f, separators=(",", ":"), allow_nan=False)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, self.path)
        fd = os.open(os.path.dirname(os.path.abspath(self.path)), os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)


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
        self.not_handed_over_path = path + ".not-handed-over"
        self.run_path = path + ".run"
        self.run_ended_path = path + ".run-ended"
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
        an order the sender refused for good, or the order was retired:
        retire_order) or "resend:YYYY-MM-DD" (a
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
        """Append one decision durably; the file is kept for the owner, and read only by settled_orders."""
        with open(self.decisions_path, "a", encoding="utf-8") as f:
            f.write(json.dumps(decision, sort_keys=True, separators=(",", ":")) + "\n")
            f.flush()
            os.fsync(f.fileno())
        self._sync_dir()

    def refusal(self):
        """The order the relay is refusing, as {client_order_id, input_id, reason, unsendable, order}, or None.

        unsendable says whether IBKR can never accept the order as it
        stands, and order is then its release, for the owner's retire
        command. A refusal as the relay wrote one before it recorded either
        reads as not unsendable, with no order. Read strictly (fail closed,
        AGENTS.md): anything but what record_refusal writes raises
        HandoverRecordInvalid.
        """
        try:
            with open(self.refused_path, encoding="utf-8") as f:
                entry = json.load(f)
        except FileNotFoundError:
            return None
        except ValueError as err:
            raise HandoverRecordInvalid("{} is not JSON: {}".format(self.refused_path, err)) from None
        if isinstance(entry, dict) and tuple(sorted(entry)) == REFUSAL_FIELDS:
            entry = dict(entry, unsendable=False, order=None)
        if (not isinstance(entry, dict) or tuple(sorted(entry)) != REFUSAL_SHAPE_FIELDS
                or not all(isinstance(entry[k], str) for k in REFUSAL_FIELDS)
                or not entry["client_order_id"] or not entry["input_id"]
                or not isinstance(entry["unsendable"], bool)
                or entry["unsendable"] != isinstance(entry["order"], dict)
                or entry["order"] is not None and entry["order"].get("client_order_id") != entry["client_order_id"]):
            raise HandoverRecordInvalid("{} is not a refusal as the relay writes one".format(self.refused_path))
        return entry

    def _read_entry(self, path, what):
        try:
            with open(path, encoding="utf-8") as f:
                entry = json.load(f)
        except FileNotFoundError:
            return None
        except ValueError as err:
            raise HandoverRecordInvalid("{} is not JSON: {}".format(path, err)) from None
        if (not isinstance(entry, dict) or tuple(sorted(entry)) != REFUSAL_FIELDS
                or not all(isinstance(entry[k], str) for k in REFUSAL_FIELDS)
                or not entry["client_order_id"] or not entry["input_id"]):
            raise HandoverRecordInvalid("{} is not {} as the relay writes one".format(path, what))
        return entry

    def record_refusal(self, input_id, client_order_id, reason, order=None):
        """Record durably which order of which input the relay is refusing, replacing any earlier refusal.

        order, given only for an order IBKR can never accept as it stands, is
        its release, which the owner's retire command journals.
        """
        self._write(self.refused_path, {"client_order_id": client_order_id, "input_id": input_id,
                                        "reason": reason, "unsendable": order is not None, "order": order})

    def not_handed_over(self):
        """The marked order whose last hand-over the sender said reached nothing, as {client_order_id, input_id, reason}, or None.

        Read strictly (fail closed, AGENTS.md), as refusal is.
        """
        return self._read_entry(self.not_handed_over_path, "a record of an order not handed over")

    def record_not_handed_over(self, input_id, client_order_id, reason):
        """Record durably that the sender handed nothing over for this order of this input."""
        self._write(self.not_handed_over_path, {"client_order_id": client_order_id, "input_id": input_id,
                                                "reason": reason})

    def forget_not_handed_over(self):
        """Remove the record before a hand-over, which may reach the broker."""
        try:
            os.remove(self.not_handed_over_path)
        except FileNotFoundError:
            return
        self._sync_dir()

    def record_run(self, input_id, run, log):
        """Record durably, before the input is first sent, the run it is sent to: run_identity of the log named log."""
        self._write(self.run_path, {"input_id": input_id, "log": log, "run": run})

    def run_of(self):
        """The run the input awaiting its reply was sent to, as {input_id, log, run}, or None.

        Read strictly (fail closed, AGENTS.md): anything but what record_run
        writes raises HandoverRecordInvalid.
        """
        try:
            with open(self.run_path, encoding="utf-8") as f:
                entry = json.load(f)
        except FileNotFoundError:
            return None
        except ValueError as err:
            raise HandoverRecordInvalid("{} is not JSON: {}".format(self.run_path, err)) from None
        if (not isinstance(entry, dict) or tuple(sorted(entry)) != RUN_FIELDS
                or not all(isinstance(entry[k], str) for k in RUN_FIELDS) or not entry["input_id"]):
            raise HandoverRecordInvalid("{} is not a record of the run an input was sent to".format(self.run_path))
        return entry

    def run_ended(self):
        """The inputs settled as left by an earlier run (resolve_run_ended), oldest first.

        Each holds the input, and under "outbox" the outbox entries struck
        off with it (IBKR's reports on orders an earlier run released),
        recorded before the settlement.

        Read strictly (fail closed, AGENTS.md): a line that is not exactly
        what record_run_ended writes raises HandoverRecordInvalid, and so
        does one with no recorded run. The adapter records the run an input
        is sent to before it saves the input (record_run), so a settlement
        naming no run is hand-made or another tool's: its place among the
        runs, and so which of an order's statuses is the latest, cannot be
        known, and none is guessed.
        """
        try:
            with open(self.run_ended_path, encoding="utf-8") as f:
                # Numbered as the file's own lines, blank ones included, so
                # a stop names the line the owner finds.
                lines = [(n, line) for n, line in enumerate(f, 1) if line.strip()]
        except FileNotFoundError:
            return []
        entries = []
        for n, line in lines:
            try:
                entry = json.loads(line)
            except ValueError as err:
                raise HandoverRecordInvalid("{} line {} is not JSON: {}".format(self.run_ended_path, n, err)) from None
            if (not isinstance(entry, dict) or tuple(sorted(entry)) != RUN_ENDED_FIELDS
                    or not isinstance(entry["input"], dict) or not _is_text(entry["input"].get("id"))
                    or entry["refused"] != RUN_ENDED
                    or not all(_is_text(entry[k]) for k in ("at", "reason", "session_date"))
                    or not (entry["run"] is None or isinstance(entry["run"], str))
                    or not isinstance(entry["unsent"], list) or not all(_is_text(c) for c in entry["unsent"])
                    or not isinstance(entry["outbox"], list) or not all(_is_outbox_entry(e) for e in entry["outbox"])):
                raise HandoverRecordInvalid("{} line {} is not a settled input as resolve --run-ended writes "
                                            "one".format(self.run_ended_path, n))
            if not _is_text(entry["run"]):
                raise HandoverRecordInvalid(
                    "{} line {} settles input {} with no recorded run: resolve --run-ended writes the run the "
                    "input was sent to, so the line is not one the adapter wrote; check it before starting "
                    "(docs/manual/redelivery.md)".format(self.run_ended_path, n, entry["input"]["id"]))
            entries.append(entry)
        return entries

    def record_run_ended(self, entry):
        """Append one settled input durably; never removed, so it is never sent or journalled again."""
        with open(self.run_ended_path, "a", encoding="utf-8") as f:
            f.write(json.dumps(entry, sort_keys=True, separators=(",", ":"), allow_nan=False) + "\n")
            f.flush()
            os.fsync(f.fileno())
        self._sync_dir()

    def settled_orders(self):
        """The orders the adapter's own durable records settle, as a SettledOrders (run_logs reads it).

        ended holds each client order ID whose last recorded decision
        settles it as never sent (--never-sent, --not-handed-over or a
        retirement), that
        the hand-over record marks never sent, that a run-ended settlement
        left unsent, or that a cancellation or refusal settled at such a
        settlement ends (the input it settled, or an outbox entry struck off
        with it); fills each fill settled there. A report settled there is
        in no engine's log, so only these records end such an order. An
        order left unsent at a run-ended settlement is every order its
        input's reply released that the hand-over record did not mark sent:
        one marked handing-over may have reached IBKR, and if IBKR still
        shows it once its run is left out of the chain, reconciliation stops
        on it ("no order the engine released explains"). Every record is
        read strictly, as handovers and run_ended read theirs (fail closed,
        AGENTS.md): a decision line that is not JSON, or not an object with
        a text client order ID and one of DECISIONS, raises
        HandoverRecordInvalid, so a damaged later decision never leaves an
        earlier one settling its order.
        """
        last = {}
        try:
            with open(self.decisions_path, encoding="utf-8") as f:
                # Numbered as the file's own lines, blank ones included, so
                # a stop names the line the owner finds.
                lines = [(n, line) for n, line in enumerate(f, 1) if line.strip()]
        except FileNotFoundError:
            lines = []
        for n, line in lines:
            try:
                decision = json.loads(line)
            except ValueError as err:
                raise HandoverRecordInvalid("{} line {} is not JSON: {}".format(self.decisions_path, n, err)) from None
            if (not isinstance(decision, dict) or not _is_text(decision.get("client_order_id"))
                    or decision.get("decision") not in RECORDED_DECISIONS):
                raise HandoverRecordInvalid("{} line {} is not a decision as relay.py resolve writes one".format(
                    self.decisions_path, n))
            last[decision["client_order_id"]] = decision["decision"]
        ended = {cid for cid, decision in last.items()
                 if decision in (DECIDE_NEVER_SENT, DECIDE_NOT_HANDED_OVER, DECIDE_RETIRED)}
        ended |= {cid for cid, mark in self.handovers().items() if mark == NEVER_SENT}
        fills = {}
        for entry in self.run_ended():
            ended.update(entry["unsent"])
            settled = entry["input"]
            reports = [(settled.get("type"), settled.get("payload"))]
            reports += [(struck["event_type"], struck["payload"]) for struck in entry["outbox"]]
            for event_type, payload in reports:
                cid = payload.get("client_order_id") if isinstance(payload, dict) else None
                if not _is_text(cid):
                    continue
                if event_type == LIFECYCLE_TYPE and payload.get("status") in LIFECYCLE_TERMINAL:
                    ended.add(cid)
                elif (event_type == FILL_TYPE and _is_text(payload.get("fill_id"))
                      and _is_count(payload.get("quantity"))):
                    fills[payload["fill_id"]] = (cid, payload["quantity"])
        return SettledOrders(frozenset(ended), fills)

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
        """Remove the input, then its write-ahead record, then the records of any order refused or not handed over for it, and of its run.

        In that order: a stop between the two leaves marks for orders whose
        input is settled, which are harmless since a client order ID names one
        order; the other order would leave an input to settle again with no
        record of what was handed over.
        """
        for path in (self.path, self.handover_path, self.refused_path, self.not_handed_over_path, self.run_path):
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
    asks again before EngineUnavailable reaches the caller. settled, when
    given, is called with each input once its reply has been acted on and
    before the input awaiting its reply is cleared (the loop strikes it off
    its outbox there), so a stop between the two leaves the input awaiting
    its reply, never an outbox entry no input awaiting a reply accounts
    for. today returns the session's date (New York), against which a resend permission is
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
                 close_timeout=CLOSE_TIMEOUT, settled=None, now=None):
        _positive_seconds("close_timeout", close_timeout)
        if not callable(getattr(sender, "check", None)) or not callable(getattr(sender, "send", None)):
            raise TypeError("the relay's sender must have check and send: it asks check before every mark")
        self._held = _lock(pending.lock_path)
        if self._held is None:
            raise PendingInUse("{} is held by another adapter or by relay.py resolve".format(pending.path))
        self._today = today or _session_today
        self._now = now or _utc_now
        self._connect = connect
        self._sender = sender
        self._pending = pending
        self._broker_orders = broker_orders
        self._to_request = to_request
        self._reconnects = reconnects
        self._settled = settled
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
        decide what to do. The connection to the engine is closed too: the
        engine takes one connection at a time (cmd/engine), so the next
        adapter's start is never refused for this one's.
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
            engine, self._engine = self._engine, None
            if engine is not None and hasattr(engine, "close"):
                engine.close()
        finally:
            self._step.release()

    def connect_engine(self):
        """Open the connection to the engine now, as the first exchange would; EngineUnavailable if none listens."""
        self._require_held()
        if self._engine is None:
            self._engine = self._connect()

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
            if mark is None:
                # A retirement a stop left unmarked is finished before the
                # order could be judged sendable again: the engine is told it
                # was never sent, so it never is. Finished under the step, as
                # every edit of the record is, so never after close released
                # the lock (Relay.close), and judged on what IBKR shows first.
                with self._step:
                    self._require_held()
                    if _finish_retirement(self._pending, order, envelope.get("id"), self._now(), self._today(),
                                          cid in broker):
                        mark = NEVER_SENT
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
            except SessionOver as err:
                # Its session is over, so it is never sent: retired here,
                # without the owner (the owner's decision of 2026-10-22).
                with self._step:
                    self._require_held()
                    retire_order(self._pending, order, RETIRED_SESSION_OVER, str(err), RETIRED_BY_ADAPTER,
                                 self._now(), self._today(), envelope.get("id"))
                continue
            except Exception as err:
                # Recorded so the owner can settle this order, and only this
                # one: one IBKR can never accept is the owner's to retire.
                unsendable = isinstance(err, UnsendableOrder)
                with self._step:
                    self._require_held()
                    self._pending.record_refusal(envelope.get("id"), cid, str(err),
                                                 order=_plain(order) if unsendable else None)
                if unsendable:
                    raise OrderUnsendable(cid, self._pending.path, err) from err
                raise
            with self._step:
                self._require_held()
                self._pending.forget_not_handed_over()
                self._pending.mark(cid, HANDING_OVER)
                try:
                    self._sender.send(request)
                except HandoverUnconfirmed as err:
                    # The sender's own word that nothing reached the broker:
                    # recorded so the owner can settle the order truthfully on
                    # a later day, when a resend is refused.
                    if err.handed_over is False:
                        self._pending.record_not_handed_over(envelope.get("id"), cid, str(err))
                    raise
                self._require_held()
                self._pending.mark(cid, SENT)
            self._sent.add(cid)
        with self._step:
            self._require_held()
            if self._settled is not None:
                self._settled(envelope)
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

    not-handed-over settles, as never sent, an order the record marks
    HANDING_OVER whose last hand-over the relay recorded its sender said
    reached nothing (PendingInput.not_handed_over), for the input awaiting
    its reply; any other mark, no such record, a record for another order or
    input, or no input awaiting a reply refuses it. It is accepted on any
    day: the owner checks IBKR's order history first, and the relay passes
    over the order, as any never-sent one, only while IBKR shows nothing
    under its tag.

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
    if decision == DECIDE_NOT_HANDED_OVER:
        waiting = pending.load()
        if waiting is None:
            raise ResolveRefused("no input awaits its reply in {}, so no order is stuck; check the pending "
                                 "file".format(pending.path))
        if marks.get(client_order_id) != HANDING_OVER:
            raise ResolveRefused("order {} is marked {!r}, not {!r}: only an order stopped as it was handed over can "
                                 "have been not handed over; check the client order ID".format(
                                     client_order_id, marks.get(client_order_id), HANDING_OVER))
        record = pending.not_handed_over()
        if (record is None or record["input_id"] != waiting.get("id")
                or record["client_order_id"] != client_order_id):
            raise ResolveRefused("the adapter recorded no stop before placeOrder for order {}: it may have reached "
                                 "IBKR, so settle it with --sent, or --resend in the session it was committed, after "
                                 "checking IBKR".format(client_order_id))
        pending.record_decision(recorded)
        pending.mark(client_order_id, NEVER_SENT)
        return
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
        if refusal["unsendable"]:
            raise ResolveRefused("the adapter refused order {} as a shape IBKR can never accept ({}): settle it only "
                                 "with relay.py retire once the cause is found, which tells the engine it was never "
                                 "sent (docs/manual/ibkr-adapter.md)".format(client_order_id, refusal["reason"]))
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


def _plain(order):
    """A released order's payload with every number a JSON number, as the outbox and the refusal record hold it."""
    return json.loads(json.dumps(order, default=float, allow_nan=False))


def _stamp(at):
    return at.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _utc_now():
    return datetime.now(timezone.utc)


def retire_order(pending, order, reason, detail, by, at, today, input_id, entry_id=None):
    """Retire a released order that never left the adapter: journal the refusal, record it, and mark it never sent.

    The owner's decision of 2026-10-22. In that order: the refusal is
    recorded first in the adapter's outbox as the engine's next
    execution.order.retired input (internal/event OrderRetiredPayload),
    named after the order (Outbox.append_once), then the retirement among
    the decisions, then the order marked never sent, so a stop before the
    mark retires the order again with no second record, and the relay passes
    over an order marked never sent, as any other, only while IBKR shows
    nothing under its tag. order is the release's payload; reason is
    RETIRED_SESSION_OVER (by the adapter) or RETIRED_BY_OWNER (by the owner,
    named); detail what refused it or why the owner retired it; at when;
    today the session it was judged in; input_id the input awaiting its
    reply that released it, recorded with the retirement so that only that
    input's settlement reads it as begun (_finish_retirement); entry_id the
    outbox entry's ID, named after the order unless given.
    """
    order = _plain(order)
    cid = order["client_order_id"]
    stamp = _stamp(at)
    payload = {"account_id": order.get("account_id"), "client_order_id": cid,
               "instrument_id": order.get("instrument_id"), "side": order.get("side"),
               "quantity": order.get("quantity"), "limit_price": order.get("limit_price"), "reason": reason,
               "detail": detail, "retired_by": by, "retired_at": stamp}
    Outbox(pending.path + OUTBOX_SUFFIX).append_once({
        "id": entry_id or RETIRED_ID + cid, "event_type": RETIRED_TYPE, "schema_version": RETIRED_VERSION,
        "event_time": stamp, "payload": payload})
    pending.record_decision({"client_order_id": cid, "decision": DECIDE_RETIRED, "reason": detail,
                             "retired": reason, "retired_by": by, "at": stamp, "session_date": today.isoformat(),
                             "input_id": input_id})
    pending.mark(cid, NEVER_SENT)


def _finish_retirement(pending, order, input_id, now, today, at_broker):
    """Finish a retirement of cid that a stop left unmarked, and say whether one was begun.

    retire_order records the retirement for the engine first (the outbox),
    then among the decisions, then marks the order never sent. A stop
    before the mark leaves an order the engine will be told was never sent
    that the record does not mark, so it is finished here, before the order
    is judged sendable again: the decision recorded from the outbox entry if
    the stop came before it, and the order marked never sent (the owner's
    decision of 2026-10-22; fail closed, AGENTS.md). A retirement recorded
    for input_id, the input awaiting its reply, was begun for this order; one
    recorded for another input, an earlier run's under a reused client order
    ID, is not this order's, which is judged as any other. A retirement
    recorded with no input_id (by a relay before the field, or edited by
    hand) is unknown, not another input's: it is read as this order's unless
    a later retirement recorded for another input follows it, so the order
    is never sent on a record that cannot say whose it is (fail closed on
    missing, AGENTS.md). When such a record's outbox entry has already
    reached the engine, the engine is told of this input's release (order,
    at now) under an entry named after the order and the input, and the
    retirement is recorded for input_id, as retire_order does, so the engine
    ends the order and the record says whose it was; the entry's ID finds it
    again after a stop before the record or the mark, so it is queued once
    (ADR 0026, amended 2026-10-22, "The adapter"). at_broker says whether
    IBKR shows an order or execution under cid's tag, which wins over the
    record: no retirement is queued then, the order is only marked never
    sent, and the relay stops at it (NeverSentAtBroker), since an order IBKR
    shows is never retired (ADR 0026, amended 2026-10-22, "The adapter").
    The record is turned into an engine input only once it reads as a
    retirement the relay records (_recorded_retirement), so the engine is
    never sent one it would refuse (fail closed, AGENTS.md).
    """
    cid = order["client_order_id"]
    retired = [d for d in pending.decisions()
               if d.get("client_order_id") == cid and d.get("decision") == DECIDE_RETIRED]
    own = any(d.get("input_id") == input_id for d in retired)
    unknown = not own and bool(retired) and not retired[-1].get("input_id")
    begun = own or unknown
    entries = {e["id"]: e for e in Outbox(pending.path + OUTBOX_SUFFIX).load()}
    this_input = entries.get(RETIRED_ID + cid + ":" + input_id) if unknown else None
    entry = this_input or entries.get(RETIRED_ID + cid)
    if not begun and entry is None:
        return False
    if unknown and entry is None:
        if at_broker:
            pending.mark(cid, NEVER_SENT)
            return True
        reason, detail, by = _recorded_retirement(pending, retired[-1])
        retire_order(pending, order, reason, detail, by, now, today, input_id,
                     entry_id=RETIRED_ID + cid + ":" + input_id)
        return True
    if not begun or this_input is not None:
        p = entry["payload"]
        pending.record_decision({"client_order_id": cid, "decision": DECIDE_RETIRED, "reason": p.get("detail"),
                                 "retired": p.get("reason"), "retired_by": p.get("retired_by"),
                                 "at": p.get("retired_at"), "session_date": today.isoformat(), "input_id": input_id})
    pending.mark(cid, NEVER_SENT)
    return True


def _recorded_retirement(pending, d):
    """The reason, detail and retiring identity of retirement record d, as retire_order records them.

    One of the two reasons, a detail, and who retired it fitting the reason
    (the adapter for the session over, a named owner, never the adapter, for
    the owner's), as the engine's retirement input requires (internal/event
    OrderRetiredPayload.Validate); anything else raises HandoverRecordInvalid,
    so a record that cannot be one is never sent to the engine (fail closed,
    AGENTS.md).
    """
    reason, detail, by = d.get("retired"), d.get("reason"), d.get("retired_by")
    named = isinstance(by, str) and bool(by.strip())
    fits = (reason == RETIRED_SESSION_OVER and by == RETIRED_BY_ADAPTER
            or reason == RETIRED_BY_OWNER and named and by != RETIRED_BY_ADAPTER)
    if not fits or not isinstance(detail, str) or not detail.strip():
        raise HandoverRecordInvalid(
            "{} records order {}'s retirement, with no input, as {!r} by {!r} because {!r}: not a retirement as the "
            "relay records one ({!r} by {!r}, or {!r} by the owner named, with its reason), so it is not sent to the "
            "engine; correct the record, then start the adapter (docs/manual/redelivery.md)".format(
                pending.decisions_path, d.get("client_order_id"), reason, by, detail, RETIRED_SESSION_OVER,
                RETIRED_BY_ADAPTER, RETIRED_BY_OWNER))
    return reason, detail, by


def _effective_user():
    return os.geteuid()


def retire(pending, client_order_id, by, reason, now=None, today=None):
    """The owner's retirement of the order the adapter stopped at because IBKR can never accept it as it stands.

    The owner's decision of 2026-10-22: the adapter stopped at an order that
    is not a limit order, is for a fraction of a share, or has no IBKR
    contract ID (OrderUnsendable); once the owner has found the cause, this
    journals the refusal with the owner's reason (retire_order, reason
    RETIRED_BY_OWNER, by the owner named), so the adapter, started again,
    goes on to its next input and the engine re-plans at its next decision.
    Refuses (ResolveRefused, nothing changed) an empty reason, no name or the
    adapter's own, a run as any user but the one that owns the adapter's
    files (the owner's account for them, `sudo -u ti-trader` on the server)
    or as root, while an adapter holds the pending file, no input awaiting
    its reply, any order but the one the relay recorded it is refusing for
    that input, a refusal that is not of a shape IBKR can never accept, and
    an order the record marks. now defaults to the time now, today to today
    in New York.
    """
    if not reason or not reason.strip():
        raise ResolveRefused("a reason is required: it is journalled with the retirement")
    if not by or not by.strip() or by == RETIRED_BY_ADAPTER:
        raise ResolveRefused("--by must name the owner retiring the order; {!r} is not a person".format(by))
    user = _effective_user()
    try:
        owner = os.stat(pending.path).st_uid
    except FileNotFoundError:
        raise ResolveRefused("no input awaits its reply in {}, so no order is stopped; check the pending "
                             "file".format(pending.path)) from None
    if user == 0 or user != owner:
        raise ResolveRefused("only the owner retires an order, as the user that owns the adapter's files (user "
                             "{}, `sudo -u ti-trader` on the server), never as root or another user; this runs as "
                             "user {}".format(owner, user))
    held = _lock(pending.lock_path)
    if held is None:
        raise ResolveRefused("the adapter holds the pending file {}; stop it first".format(pending.path))
    try:
        waiting = pending.load()
        if waiting is None:
            raise ResolveRefused("no input awaits its reply in {}, so no order is stopped; check the pending "
                                 "file".format(pending.path))
        marks = pending.handovers()
        if client_order_id in marks:
            raise ResolveRefused("order {} is marked {!r}: it was handed over or settled already, so it is not "
                                 "an order the adapter stopped at before sending".format(
                                     client_order_id, marks[client_order_id]))
        refusal = pending.refusal()
        if refusal is None or refusal["input_id"] != waiting.get("id"):
            raise ResolveRefused("the adapter has recorded no refused order for the input awaiting its reply ({}); "
                                 "check the adapter's stop message".format(waiting.get("id")))
        if refusal["client_order_id"] != client_order_id:
            raise ResolveRefused("the adapter is refusing order {} ({}), not {}; check the client order ID".format(
                refusal["client_order_id"], refusal["reason"], client_order_id))
        if not refusal["unsendable"]:
            raise ResolveRefused("the adapter refused order {} for a reason that is not a shape IBKR can never "
                                 "accept ({}): fix that cause and start the adapter again, or settle the order "
                                 "with resolve --never-sent (docs/manual/redelivery.md)".format(
                                     client_order_id, refusal["reason"]))
        retire_order(pending, refusal["order"], RETIRED_BY_OWNER, reason, by, (now or _utc_now)(),
                     today or _session_today(), waiting.get("id"))
    finally:
        os.close(held)


def run_identity(path):
    """The identity of the run whose durable log is at path: its Run approval's ID, else its first committed input's.

    Every fresh run holds one Run approval of its own (cmd/engine records it
    before it listens), named by the run it follows; so does a first run in
    paper or live mode, which does not start without one, named by its
    declared start (as-of) and its payload's hash
    (operator-run-approved:first-run:<as-of>:<payload hash>; ADR 0027,
    amended 2026-10-18). A resumed run's log holds a copy of the crashed
    run's records, its approval included (ADR 0027, section 6), so a crash's
    resume keeps the identity and a fresh run has its own. Only a dry run's
    first run may hold no approval: it is named by its log's first committed
    input, the configuration, which a resume copies first: its ID (the
    configuration's hash) and its recorded time (the run's as-of time), so
    two unapproved starts of other configurations or other as-of times are
    two runs, never one. The residual, for a dry run only: two unapproved
    starts with the same configuration and as-of time (a first dry run
    started again the same day from the same run.env) share an identity, so
    the second is taken for a resume of the first and is asked for the
    first one's waiting input; nothing in the two logs tells them apart by
    identity (delivering market data, delivery.stream_run stops such a
    start: its log does not begin with the first run's record hash chain).
    A crashed first run is resumed, and any other restart is a fresh run
    with its approval (docs/manual/paper-trading.md, "Starting the
    adapter"). Read as journalled_reports reads the log. Fail closed
    (ResolveRefused): a log with no committed input names no run, and is
    never read as any run's.
    """
    return _identity(path, _committed_groups(path).groups)


def _identity(path, groups):
    """run_identity of the log at path from its committed groups (all of them, or its first two: head_identity)."""
    found = engine_log.identity(groups)
    if found is None:
        raise ResolveRefused("the engine's durable log {} holds no committed input, so it names no run: the engine "
                             "commits its configuration before it listens; check that this is the log of the "
                             "engine now running".format(path))
    return found


def head_identity(path):
    """The run identity the first records of the durable log at path name, read no further than its second commit.

    A run's identity is in its log's first two committed inputs: the
    configuration, and a fresh run's Run approval, which cmd/engine records
    as its second input before it listens (a resume copies both first), so
    this is run_identity's answer for every log the engine writes, read
    without the rest of the log (engine_log.head, the reader the data job
    shares). It sorts the logs beside a run's by identity without reading
    any other run's log whole: only the first lines' shape is checked, so a
    log the head names as the run's is then read strictly (run_identity,
    _committed_groups). Fail closed (ResolveRefused): first lines that
    cannot be read as the engine writes them name no run.
    """
    try:
        groups = engine_log.head(path)
    except engine_log.LogUnreadable as err:
        raise _refused(err) from None
    return _identity(path, groups)


def earlier_run(pending, engine_log_path):
    """The input awaiting its reply as an EarlierRun when it was sent to an earlier run, or None when it is this run's.

    It is this run's (the one whose durable log is engine_log_path) when
    that log commits it, or when the adapter recorded sending it to a run of
    the same identity (PendingInput.record_run, run_identity). Anything else
    is an earlier run's, an input with no record of its run included (fail
    closed, AGENTS.md): the logs of the runs the Run approvals chain back
    to, from the session it was recorded in (run_logs), say whether that run
    committed it and which orders its reply released that the write-ahead
    record does not mark sent. A log that cannot be read raises
    ResolveRefused.
    """
    waiting = pending.load()
    if waiting is None:
        return None
    input_id = waiting.get("id")
    if input_id in {i.id for i in read_engine_log(engine_log_path).inputs}:
        return None
    sent_to = pending.run_of()
    if sent_to is not None and sent_to["input_id"] != input_id:
        sent_to = None
    if sent_to is not None and sent_to["run"] == run_identity(engine_log_path):
        return None
    run = sent_to["run"] if sent_to is not None else None
    for path in _earlier_logs(pending, engine_log_path, waiting):
        for group in _committed_groups(path).groups:
            if group[0][2]["id"] != input_id:
                continue
            marks = pending.handovers()
            released = [envelope["payload"]["client_order_id"] for _, kind, envelope in group[1:]
                        if kind == LOG_DECISION and envelope["type"] == RELEASED
                        and isinstance(envelope["payload"], dict) and envelope["payload"].get("send") is True
                        and _is_text(envelope["payload"].get("client_order_id"))]
            unsent = tuple((cid, marks.get(cid)) for cid in released if marks.get(cid) != SENT)
            return EarlierRun(input_id, os.path.basename(path), True, unsent, run)
    return EarlierRun(input_id, sent_to["log"] if sent_to is not None else None, False, (), run)


def _earlier_logs(pending, engine_log_path, waiting):
    """The logs of the earlier runs the chain from engine_log_path reaches, from the session waiting was recorded in (run_logs).

    The input's recorded_at (the loop's envelope always holds one) bounds
    the chain; one that cannot be read raises ResolveRefused (fail closed,
    AGENTS.md): no session is guessed for it.
    """
    since = _session_date({"event_time": waiting.get("recorded_at")})
    if since is None:
        raise ResolveRefused(
            "the input awaiting its reply in {} ({}) has no recorded_at that can be read (a time with its zone), "
            "so the session it was recorded in, and the earlier run it may belong to, cannot be found: the pending "
            "file is not as the adapter writes it; check it before starting".format(pending.path, waiting.get("id")))
    return run_logs(engine_log_path, since, settled=pending.settled_orders())[1:]


def resolve_run_ended(pending, input_id, reason, engine_log_path, today=None):
    """Settle the input an earlier run left awaiting its reply: refused, with what it released, for "run ended".

    The input, and every order its committed reply released that the
    write-ahead record does not mark sent, are recorded as refused for
    RUN_ENDED (PendingInput.record_run_ended), never sent and never sent
    again: the adapter journals nothing of the input again, strikes it off
    its outbox unsent, and the next start's reconciliation (ADR 0025 point
    5) sees the account as it is. Of the other entries the outbox holds,
    only those that provably belong to the earlier run are settled with it:
    IBKR's reports (lifecycle and fill inputs) on an order a run of the
    chain before engine_log_path released (run_logs from the input's
    session), recorded while that run's engine decided. Each is recorded
    with the settlement and struck off unsent, never drained into the fresh
    run under its identity. Every other entry stays in the outbox and is
    sent to the fresh run: a producer in another process (the session's
    close) may have recorded it for the fresh run while the adapter was
    stopped, and an input that may be the running run's is never discarded
    (fail closed, AGENTS.md rule 5). The pending file is then cleared, so
    the adapter goes on with the fresh run's own inputs. Refuses (ResolveRefused,
    nothing changed) while an adapter holds the pending file, an empty
    reason, no input awaiting its reply, another input than the one
    awaiting, an input of the run whose durable log is engine_log_path
    (earlier_run: that run is asked for its reply again, never settled),
    and an input with no record of the run it was sent to (the settlement
    would name no run, which run_ended refuses).
    today defaults to today in New York.
    """
    if not reason or not reason.strip():
        raise ResolveRefused("a reason is required: it is kept with the settlement")
    held = _lock(pending.lock_path)
    if held is None:
        raise ResolveRefused("the adapter holds the pending file {}; stop it first".format(pending.path))
    try:
        waiting = pending.load()
        if waiting is None:
            raise ResolveRefused("no input awaits its reply in {}, so there is nothing to settle".format(pending.path))
        if waiting.get("id") != input_id:
            raise ResolveRefused("the input awaiting its reply is {}, not {}; check the input ID the adapter's stop "
                                 "names".format(waiting.get("id"), input_id))
        ended = earlier_run(pending, engine_log_path)
        if ended is None:
            raise ResolveRefused(
                "input {} belongs to the run now running (its durable log {}): it is asked for its reply again "
                "from that run, never settled as run ended; start the adapter".format(input_id, engine_log_path))
        if ended.run is None:
            # The adapter records the run before it saves the input, so a
            # settlement here would name no run, and run_ended refuses such
            # a line at every later start (fail closed, AGENTS.md).
            raise ResolveRefused(
                "input {} has no record of the run it was sent to ({} is missing or names another input): the "
                "adapter records it before it saves the input, so the pending file is not as the adapter writes "
                "it; check it against the engine's logs and IBKR before starting".format(input_id, pending.run_path))
        outbox = Outbox(pending.path + OUTBOX_SUFFIX)
        settled = pending.run_ended()
        recorded = {e["input"]["id"] for e in settled} | {o["id"] for e in settled for o in e["outbox"]}
        earlier = set()
        for path in _earlier_logs(pending, engine_log_path, waiting):
            earlier |= released_client_order_ids(path)
        behind = [e for e in outbox.load() if e["id"] != input_id and e["id"] not in recorded
                  and e["event_type"] in (LIFECYCLE_TYPE, FILL_TYPE)
                  and e["payload"].get("client_order_id") in earlier]
        if input_id not in recorded or behind:
            pending.record_run_ended({
                "input": waiting, "refused": RUN_ENDED, "reason": reason, "run": ended.run,
                "unsent": [cid for cid, _ in ended.unsent], "outbox": behind,
                "at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
                "session_date": (today or _session_today()).isoformat()})
        # Recorded first, so a stop before the strike still keeps every
        # entry it names from the fresh run (the loop strikes what a
        # settlement names).
        for entry in [{"id": input_id}] + behind:
            outbox.strike(entry["id"])
        pending.clear()
    finally:
        os.close(held)


def journalled_fills(path):
    """The fill IDs of every committed fill input in the engine's durable log at path (journalled_reports)."""
    return journalled_reports(path).fills


def journalled_reports(path):
    """The committed fills and ended orders in the engine's durable log at path (the engine's -log file).

    A JournalledReports: the fill ID of every committed fill input, and the
    client order ID of every order a committed lifecycle input ended
    (cancelled or invalid) or a committed retirement input retired. A lifecycle input not as the IBKR adapter
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
    fills = set()
    ended = set()
    for group in _committed_groups(path).groups:
        for at, kind, envelope in group:
            if envelope["type"] == FILL_TYPE:
                fills.add(_fill_id(path, at, kind, envelope))
            elif envelope["type"] == LIFECYCLE_TYPE:
                client_order_id, status = _lifecycle(path, at, kind, envelope)
                if status in LIFECYCLE_TERMINAL:
                    ended.add(client_order_id)
            elif envelope["type"] == RETIRED_TYPE and kind == LOG_INPUT:
                ended.add(_retired(path, at, envelope))
    return JournalledReports(fills, ended)


def released_client_order_ids(path):
    """The client order ID of every order a committed decision in the engine's durable log at path released to be sent.

    Read as journalled_reports reads the log (ResolveRefused for one that
    cannot be read). These are the orders IBKR may show under this engine's
    client ID; reconciliation matches IBKR's open orders and executions
    against them (orders.match_on_reconnect).
    """
    released = set()
    for group in _committed_groups(path).groups:
        for _, kind, envelope in group:
            payload = envelope["payload"]
            if (kind == LOG_DECISION and envelope["type"] == RELEASED and isinstance(payload, dict)
                    and payload.get("send") is True and _is_text(payload.get("client_order_id"))):
                released.add(payload["client_order_id"])
    return released


def run_logs(path, today=None, settled=None):
    """The engine's durable log at path, then the log of each earlier run its Run approvals name, newest first.

    A fresh run's log holds one committed Run approval, whose previous_log
    is the run before it in the same directory, with that log's final
    record hash; that run's log may hold one in turn, back to the first run,
    which holds none or a first run's approval naming no previous log
    (run_chain, which walks the chain). IBKR's lists reach back past a restart (the day's
    executions, the session's open orders), so reconciliation and the
    report journal read the earlier runs of the chain too, but only those
    IBKR's lists can still show: the walk stops at the first earlier run
    whose committed inputs all fall on a New York date before yesterday's
    session (by event time and recorded time alike; yesterday's session is
    the weekday before today) and every order it released has ended, and
    leaves it and every run before it out. Every run of today's or
    yesterday's session is read whatever it released, so a run that
    released nothing never hides an earlier run whose order released after
    yesterday's close works today. An order has ended when a committed
    report ends it (cancelled or refused) or its committed fills reach its
    quantity, in that run's log or a later one of the chain (the adapter
    journals IBKR's reports into the run now running), or when the
    adapter's own durable records settle it (settled, a SettledOrders from
    PendingInput.settled_orders: an order never sent, left unsent by a
    run-ended settlement, or ended by reports settled there). Only an
    input's reports count: a decision (the dry-run engine's synthetic
    terminal report) is the engine's own, never IBKR's. A calendar date is
    never taken as proof an order ended: an earlier day's run with an order
    not ended is kept, and the walk goes on past it. The weekday is not the
    exchange's calendar: when the weekday before today was a holiday, a run
    of the session before it can be left out although an order it released
    after that close is still working; reconciliation then stops on that
    order ("no order the engine released explains"), never sends past it
    (docs/manual/paper-trading.md). today defaults to today in New York.
    Fail closed (ResolveRefused): a log that cannot be
    read, more than one Run approval in a log, a previous_log that is not a
    bare file name, a previous_log_final_hash that is not the named log's
    last committed record hash (the engine checks the same at the fresh
    run's start; a log that differs is not the one approved), a named log
    with no committed record, a chain that names a log twice, or a log
    whose committed records break their record hash chain (run_chain).
    """
    yesterday = _yesterdays_session(today or _session_today())
    settled = settled or SettledOrders(frozenset(), {})
    ended, fills = set(settled.ended), dict(settled.fills)
    logs = []
    for current, groups, _ in run_chain(path):
        ended, fills = _order_ends(current, groups, ended, fills)
        if logs and _last_session(groups) < yesterday and not _open_orders(groups, ended, fills):
            return logs
        logs.append(current)
    return logs


def run_chain(path):
    """Each run of the chain from the engine's durable log at path, newest first: (log path, committed groups, approval).

    approval is the payload of the run's one committed Run approval, or None
    for a run with none. A fresh run's approval names the run before it
    (previous_log, a bare file name beside it, with its final record hash),
    and the walk goes on to it; a first run's approval (schema version 3)
    names none and states the starting book, and the chain ends there, as it
    does at a run with no approval. Unlike run_logs, every run of the chain
    is given, however old: the adapter's comparison of a run's first reading
    after a close reads back to the last account snapshot or starting book
    of the chain (ADR 0019 and ADR 0027, amended 2026-10-18). Each log is
    read only when the walk reaches it. Fail closed (ResolveRefused), as
    run_logs states. Each log's committed records must also hold their
    record hash chain (LOG_CHAIN_ALGORITHM) before its groups are given: a
    log's final record hash binds its history only when every record's own
    hash is the one the chain gives (ADR 0027), so a record changed in place
    is refused, naming the log, never read as approved history. Every reader
    of the chain verifies it, each time it reads: the comparison's walk
    (book_basis), and run_logs, so the start reconciliation's order matching
    and the report journal it seeds, which run at every adapter start long
    after the engine checked the chain at the run's own start (AGENTS.md:
    fail closed).
    """
    def read(log):
        found = _committed_groups(log)
        if found.chain_break is not None and found.chain_break <= len(found.record_hashes):
            raise ResolveRefused(
                "the engine's durable log {} breaks its record hash chain at sequence {}: a record there is not "
                "the one the engine wrote, so its history is not read as approved (docs/runbooks/durable-log.md)"
                .format(log, found.chain_break))
        return found

    seen = {os.path.realpath(path)}
    current, groups = path, read(path).groups
    while True:
        approvals = [envelope for group in groups for _, kind, envelope in group
                     if kind == LOG_INPUT and envelope["type"] == RUN_APPROVED]
        if len(approvals) > 1:
            raise ResolveRefused("the engine's durable log {} holds {} Run approvals; a run holds at most one".format(
                current, len(approvals)))
        payload = approvals[0]["payload"] if approvals else None
        yield current, groups, payload
        if payload is None:
            return
        name = payload.get("previous_log") if isinstance(payload, dict) else None
        if name == "" and isinstance(payload.get("starting_book"), dict) and not payload.get(
                "previous_log_final_hash"):
            return  # a first run's approval: no run before it
        if not _is_text(name) or os.path.basename(name) != name or name in (".", ".."):
            raise ResolveRefused("the Run approval in {} names previous_log {!r}, not a log file name in its "
                                 "directory".format(current, name))
        approved_hash = payload.get("previous_log_final_hash")
        previous = os.path.join(os.path.dirname(path), name)
        if os.path.realpath(previous) in seen:
            raise ResolveRefused("the Run approvals from {} name {} twice; the chain of runs must end at the first "
                                 "run".format(path, name))
        try:
            earlier = read(previous)
        except ResolveRefused as err:
            # An earlier run's log is evidence the comparison and the
            # reconciliation read back to (ADR 0027, amended 2026-10-18):
            # one missing or unreadable stops for a person, named.
            raise ResolveRefused(
                "the Run approval in {} names {}, the run before it, but that log cannot be read ({}): every "
                "earlier run's log of the chain stays beside the engine's, so put it back as the engine wrote it "
                "(docs/runbooks/durable-log.md)".format(current, name, err)) from None
        if earlier.final_hash is None or earlier.final_hash != approved_hash:
            raise ResolveRefused(
                "the Run approval in {} names {} with final record hash {!r}, but that log's final record hash is "
                "{}: it is not the log the owner approved, so its orders and reports are not taken as this "
                "engine's (docs/manual/paper-trading.md)".format(current, name, approved_hash,
                                                                 earlier.final_hash or "none (it holds no commit)"))
        seen.add(os.path.realpath(previous))
        current, groups = previous, earlier.groups


def _yesterdays_session(today):
    """The weekday before today: the session whose DAY orders released after its close may still work today.

    Near the first date the calendar holds, date.min: every run is then read.
    """
    if today <= date.min + timedelta(days=3):
        return date.min
    day = today - timedelta(days=1)
    while day.weekday() >= 5:
        day -= timedelta(days=1)
    return day


def _order_ends(path, groups, ended, fills):
    """ended and fills with the reports committed as inputs in groups (the log at path) added, as new values.

    ended is the client order IDs a committed lifecycle input ended; fills
    each committed fill input's fill ID to (client order ID, quantity), so
    a fill both struck at a settlement and committed counts once. Only an
    input is IBKR's report: a decision (the dry-run engine's synthetic
    terminal report, version 3, synthetic: true) is skipped. Each input is
    read as journalled_reports reads it (ResolveRefused for one that cannot
    be read).
    """
    ended, fills = set(ended), dict(fills)
    for group in groups:
        for at, kind, envelope in group:
            if kind != LOG_INPUT:
                continue
            if envelope["type"] == FILL_TYPE:
                fill_id = _fill_id(path, at, kind, envelope)
                fills[fill_id] = (envelope["payload"]["client_order_id"], envelope["payload"]["quantity"])
            elif envelope["type"] == LIFECYCLE_TYPE:
                client_order_id, status = _lifecycle(path, at, kind, envelope)
                if status in LIFECYCLE_TERMINAL:
                    ended.add(client_order_id)
            elif envelope["type"] == RETIRED_TYPE:
                ended.add(_retired(path, at, envelope))
    return ended, fills


def _open_orders(groups, ended, fills):
    """The client order IDs of the orders the decisions in groups released to be sent that have not ended.

    An order has ended when it is in ended, or when its fills in fills
    reach its released quantity; a release whose quantity cannot be read
    as a count is never taken as filled (fail closed: the run is kept).
    """
    filled = {}
    for cid, quantity in fills.values():
        filled[cid] = filled.get(cid, 0) + quantity
    still = set()
    for group in groups:
        for _, kind, envelope in group:
            payload = envelope["payload"]
            if not (kind == LOG_DECISION and envelope["type"] == RELEASED and isinstance(payload, dict)
                    and payload.get("send") is True and _is_text(payload.get("client_order_id"))):
                continue
            cid = payload["client_order_id"]
            quantity = payload.get("quantity")
            if cid in ended or (_is_count(quantity) and filled.get(cid, 0) >= quantity):
                continue
            still.add(cid)
    return still


def _last_session(groups):
    """The latest New York date among the event and recorded times of the committed inputs in groups.

    A time that cannot be read counts as no earlier than any day, so the
    walk goes on past it rather than leave a run out.
    """
    latest = date.min
    for group in groups:
        envelope = group[0][2]
        for k in ("event_time", "recorded_at"):
            latest = max(latest, _session_date({"event_time": envelope[k]}) or date.max)
    return latest


def read_engine_log(path):
    """The run's identity and every committed input in the engine's durable log at path, read as journalled_reports reads it.

    An EngineLog: the header's configuration hash and strategy version,
    which every input the adapter sends carries (ADR 0016), and each
    committed group's input in log order. next_sequence is the sequence the
    engine expects of its next input: one past the last committed input's
    (1 for a log with none). A group not yet committed holds no input. A log
    that cannot be read as the engine writes it raises ResolveRefused, never
    read as holding no inputs.
    """
    header, groups = _committed_groups(path)[:2]
    inputs = []
    for group in groups:
        envelope = group[0][2]
        inputs.append(CommittedInput(envelope["id"], envelope["type"], envelope["schema_version"],
                                     envelope["sequence"], envelope["payload"]))
    last = max((i.sequence for i in inputs), default=0)
    return EngineLog(header["configuration_hash"], header["strategy_version"], tuple(inputs), last + 1)


def _committed_groups(path):
    """The durable log at path as an engine_log.CommittedLog, read strictly by the reader the data job shares.

    Its header, its committed groups (each a list of (line number, kind,
    envelope)), its last commit's record hash (None for a log with no
    commit), every committed record's hash and where the chain first breaks.
    Fail closed (ResolveRefused) on a log not as the engine writes it.
    """
    try:
        return engine_log.read(path)
    except engine_log.LogUnreadable as err:
        raise _refused(err) from None


def _refused(err):
    """ResolveRefused for the shared reader's engine_log.LogUnreadable."""
    if err.line is None:
        return ResolveRefused("cannot read the engine's durable log {}: {}".format(err.path, err.why))
    return _unreadable(err.path, err.line, err.why)


def _unreadable(path, n, why):
    return ResolveRefused(
        "the engine's durable log {} cannot be read at line {}: {}. It is not read as holding no fills, and "
        "nothing is acknowledged: give the engine's -log file as the engine writes it "
        "(docs/runbooks/durable-log.md)".format(path, n, why))


# The record hash chain (LOG_CHAIN_ALGORITHM), as the shared reader computes it.
log_chain_seed = engine_log.chain_seed
log_chain_next = engine_log.chain_next
_canonical_bytes = engine_log.canonical_bytes
_utc = engine_log.utc


def _fill_id(path, n, kind, envelope):
    """The fill ID of a committed fill input exactly as the engine reads one, else ResolveRefused."""
    payload = envelope["payload"]
    if kind != LOG_INPUT or envelope["schema_version"] != FILL_VERSION or not is_fill(payload):
        raise _unreadable(path, n, "it is a fill but not an input {} version {} as the engine reads one".format(
            FILL_TYPE, FILL_VERSION))
    return payload["fill_id"]


def is_fill(payload):
    """Whether payload is a fill input's payload exactly as the engine reads one (internal/event TargetWeightFillPayload)."""
    return (isinstance(payload, dict) and tuple(sorted(payload)) == FILL_FIELDS
            and all(_is_text(payload[k]) for k in ("account_id", "client_order_id", "fill_id", "instrument_id"))
            and payload["side"] in ("buy", "sell") and _is_count(payload["quantity"])
            and _is_number(payload["price"]) and payload["price"] > 0
            and _is_number(payload["commission"]) and payload["commission"] >= 0
            and _is_time(payload["filled_at"], WHOLE_SECOND))


def _lifecycle(path, n, kind, envelope):
    """(client order ID, status) of a committed lifecycle input as the IBKR adapter journals one, else ResolveRefused."""
    payload = envelope["payload"]
    if (kind != LOG_INPUT or envelope["schema_version"] != LIFECYCLE_VERSION or not isinstance(payload, dict)
            or not _is_text(payload.get("client_order_id")) or not _is_text(payload.get("status"))):
        raise _unreadable(path, n, "it is a lifecycle report but not an input {} version {} as the adapter "
                                   "journals one".format(LIFECYCLE_TYPE, LIFECYCLE_VERSION))
    return payload["client_order_id"], payload["status"]


def _retired(path, n, envelope):
    """The client order ID a committed retirement input names, else ResolveRefused."""
    payload = envelope["payload"]
    if (envelope["schema_version"] != RETIRED_VERSION or not isinstance(payload, dict)
            or not _is_text(payload.get("client_order_id"))):
        raise _unreadable(path, n, "it is a retirement but not an input {} version {} as the adapter journals "
                                   "one".format(RETIRED_TYPE, RETIRED_VERSION))
    return payload["client_order_id"]


_is_text = engine_log.is_text


def _is_outbox_entry(entry):
    """Whether entry is one outbox entry as Outbox writes it.

    A stream entry's envelope must name the same input as the entry itself:
    its ID, type, schema version, event time and payload.
    """
    if not isinstance(entry, dict) or tuple(sorted(entry)) not in (OUTBOX_FIELDS, OUTBOX_STREAM_FIELDS,
                                                                   OUTBOX_RUN_FIELDS):
        return False
    if not _is_text(entry["id"]) or not isinstance(entry["payload"], dict):
        return False
    if "run" in entry:
        return _is_text(entry["run"])
    if "envelope" not in entry:
        return True
    envelope = entry["envelope"]
    return isinstance(envelope, dict) and all(
        envelope.get(k) == entry[f] for k, f in (("id", "id"), ("type", "event_type"),
                                                  ("schema_version", "schema_version"),
                                                  ("event_time", "event_time"), ("payload", "payload")))


_is_count = engine_log.is_count


def _is_number(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


_is_time = engine_log.is_time


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
    neither the log holds a committed cancellation or refusal of nor the
    input awaiting its reply in pending cancels or refuses (the journal
    seeds its ended orders from that input too, so a stop before the engine
    commits it is still cleared here), and a trade already acknowledged as
    after another order. The journal then passes over that trade on every
    start. The engine refuses a fill on an order already ended
    (internal/event OrderReports), so it is never a fill: the next start's
    reconciliation (ADR 0025 point 5) reports its shares as a position
    difference, the engine goes Degraded then, and the reconciliation
    runbook's recovery settles it (ADR 0019), with this record as the
    trade's evidence.
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
    held = _lock(pending.lock_path)
    if held is None:
        raise ResolveRefused("the adapter holds the pending file {}; stop it first".format(pending.path))
    try:
        if after not in ended and not _ends(pending.load(), after):
            raise ResolveRefused(
                "order {0} is recorded as cancelled or refused neither in the engine's durable log nor as the input "
                "awaiting its reply in {1}, so execution {2} is not a trade after it ended and is not acknowledged. "
                "Give --after the order the adapter's stop names; if it is that order, the adapter and the engine "
                "disagree on it: leave the adapter stopped, check order {0}'s trades in IBKR and reconcile the "
                "account by hand (docs/runbooks/reconciliation.md)".format(after, pending.path, exec_id))
        known = pending.acknowledged_late_fills().get(exec_id)
        if known is not None and known != after:
            raise ResolveRefused("execution {} is already acknowledged as a trade after order {}".format(
                exec_id, known))
        pending.record_late_fill({"exec_id": exec_id, "after": after, "reason": reason,
                                  "at": datetime.now(timezone.utc).isoformat(timespec="seconds")})
    finally:
        os.close(held)


def _ends(envelope, client_order_id):
    """Whether envelope, the input awaiting its reply, is a lifecycle input cancelling or refusing client_order_id.

    The adapter's journal seeds its ended orders from every input it
    journalled, the one awaiting its reply among them, while the engine's
    log holds that one only once it is committed; reading it here keeps the
    command's judgement of "ended" the same as the journal's.
    """
    if not isinstance(envelope, dict) or envelope.get("type") != LIFECYCLE_TYPE:
        return False
    payload = envelope.get("payload")
    return (envelope.get("schema_version") == LIFECYCLE_VERSION and isinstance(payload, dict)
            and payload.get("client_order_id") == client_order_id and payload.get("status") in LIFECYCLE_TERMINAL)


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
    cmd.add_argument("client_order_id", metavar="ID", help="the client order ID; with --run-ended, the input ID")
    which = cmd.add_mutually_exclusive_group(required=True)
    which.add_argument("--sent", dest="decision", action="store_const", const=DECIDE_SENT,
                       help="IBKR shows the order at all (working, filled or cancelled): never send it again; "
                            "also for a never-sent order IBKR shows after all")
    which.add_argument("--resend", dest="decision", action="store_const", const=DECIDE_RESEND,
                       help="IBKR has no trace of it and it was committed today: send it again at its original limit")
    which.add_argument("--never-sent", dest="decision", action="store_const", const=DECIDE_NEVER_SENT,
                       help="the order the adapter is refusing before any mark, which it always will: never "
                            "send it")
    which.add_argument("--not-handed-over", dest="decision", action="store_const", const=DECIDE_NOT_HANDED_OVER,
                       help="an order the adapter stopped on before placeOrder (it recorded that nothing was handed "
                            "to IB Gateway), once IBKR's order history shows no order under its tag: never send it")
    which.add_argument("--run-ended", dest="decision", action="store_const", const=DECIDE_RUN_ENDED,
                       help="the input an earlier engine run left awaiting its reply, which the adapter stopped on at "
                            "a fresh run's start: refuse it and the orders it released unsent, never send them")
    cmd.add_argument("--journal", metavar="ENGINE_LOG",
                     help="with --run-ended: the durable log of the engine run now running (its -log file)")
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
    ret = commands.add_parser("retire", help="retire the order the adapter stopped at because IBKR can never "
                                             "accept it as it stands, once its cause is found, so the adapter "
                                             "goes on; journalled as a refusal with your reason")
    ret.add_argument("pending_file", help="the adapter's pending-input file")
    ret.add_argument("client_order_id", metavar="ID", help="the client order ID the adapter's stop names")
    ret.add_argument("--by", required=True, help="your name, the owner retiring it; journalled with it")
    ret.add_argument("--reason", required=True, help="why it is retired and what was fixed; journalled with it")
    args = parser.parse_args(argv)
    pending = PendingInput(args.pending_file)
    try:
        if args.command == "retire":
            retire(pending, args.client_order_id, args.by, args.reason, today=today)
            print("order {} retired: journalled for the engine as a refusal ({}), never sent; start the adapter "
                  "and it goes on".format(args.client_order_id, RETIRED_BY_OWNER))
            return 0
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
        if args.decision == DECIDE_RUN_ENDED:
            if not args.journal:
                raise ResolveRefused("--run-ended needs --journal, the durable log of the engine run now running, to "
                                     "tell an earlier run's input from that run's")
            resolve_run_ended(pending, args.client_order_id, args.reason, args.journal, today=today)
            print("input {} settled: refused for {}; nothing of it is sent".format(args.client_order_id, RUN_ENDED))
            return 0
        if args.journal:
            raise ResolveRefused("--journal is only for --run-ended")
        resolve(pending, args.client_order_id, args.decision, args.reason, today=today)
    except (ResolveRefused, HandoverRecordInvalid, OutboxInvalid) as err:
        print("refused: {}".format(err), file=sys.stderr)
        return 1
    print("order {} resolved: {}".format(args.client_order_id, args.decision))
    return 0


if __name__ == "__main__":
    sys.exit(main())
