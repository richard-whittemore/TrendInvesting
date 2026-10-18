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

A person settles a stopped hand-over with the resolve command, never by
editing the record: `python3 adapter/ibkr/relay.py resolve PENDING_FILE
CLIENT_ORDER_ID --sent | --resend --reason TEXT`. It writes only a known
mark, settles only an order the record marks about to be sent or allowed to
be sent again (a sent mark is the relay's own proof the sender returned),
and appends the decision, its
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
"""
import argparse
import fcntl
import json
import math
import os
import re
import sys
import threading
from datetime import date, datetime, timezone
from zoneinfo import ZoneInfo

RELEASED = "risk.order.released"
# The write-ahead record holds one of three marks for a client order ID:
# HANDING_OVER, SENT, or a resend permission "resend:YYYY-MM-DD".
HANDING_OVER = "handing-over"
SENT = "sent"
MARKS = (HANDING_OVER, SENT)
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


def _positive_seconds(name, value):
    """value as a finite number of seconds greater than zero, else ValueError naming the argument."""
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or value <= 0:
        raise ValueError("{} must be a finite number of seconds greater than zero, not {!r}".format(name, value))
    return value
# The person's decisions on a stopped hand-over.
DECIDE_SENT = "sent"
DECIDE_RESEND = "resend"


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
        """The write-ahead record: each client order ID's mark, HANDING_OVER, SENT or "resend:YYYY-MM-DD".

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
                raise HandoverRecordInvalid("{} marks order {} {!r}, which is neither {!r}, {!r} nor {!r}".format(
                    self.handover_path, cid, mark, HANDING_OVER, SENT, RESEND + "YYYY-MM-DD"))
        return marks

    def mark(self, client_order_id, state):
        """Record an order's mark durably, before the call it precedes returns.

        The mark is HANDING_OVER (about to be handed over), SENT (the sender
        returned) or "resend:YYYY-MM-DD" (a person allowed it to be sent
        again in that session).
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

    def clear(self):
        """Remove the input, then its write-ahead record.

        In that order: a stop between the two leaves marks for orders whose
        input is settled, which are harmless since a client order ID names one
        order; the other order would leave an input to settle again with no
        record of what was handed over.
        """
        for path in (self.path, self.handover_path):
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
    envelope or raises EngineUnavailable. send takes an OrderRequest.
    broker_orders returns IBKR's open orders and executions
    (orders.BrokerOrders, as orders.collect_broker_orders builds it); an
    open order or execution in either list carrying a released order's client
    order ID means the broker already has that order. to_request maps a
    released order's payload to an OrderRequest. reconnects bounds how often one exchange reconnects and
    asks again before EngineUnavailable reaches the caller. today returns
    the session's date (New York), against which a resend permission is
    checked. Each relay takes its own exclusive lock on the pending file at
    construction and holds it until close (PendingInUse if another relay or
    resolve has it, in this process or another); once closed it raises
    PendingInUse rather than edit the record or send. close_timeout (a finite
    number of seconds greater than zero, else ValueError) bounds how long
    close waits for the step in flight; a close that times out raises
    RelayCloseTimeout and keeps the lock, leaving the relay open.
    """

    def __init__(self, connect, send, pending, broker_orders, to_request, reconnects=1, today=None,
                 close_timeout=CLOSE_TIMEOUT):
        _positive_seconds("close_timeout", close_timeout)
        self._held = _lock(pending.lock_path)
        if self._held is None:
            raise PendingInUse("{} is held by another adapter or by relay.py resolve".format(pending.path))
        self._today = today or _session_today
        self._connect = connect
        self._send = send
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
            if cid in self._sent or marks.get(cid) == SENT:
                continue
            if broker is None:
                broker = _tags(self._broker_orders())
            if cid in broker:
                self._sent.add(cid)
                continue
            mark = marks.get(cid)
            if mark == HANDING_OVER:
                raise HandoverUnconfirmed(cid)
            if mark is not None:
                allowed_on, today = resend_date(mark), self._today()
                if allowed_on != today:
                    with self._step:
                        self._require_held()
                        self._pending.mark(cid, HANDING_OVER)
                    raise ResendExpired(cid, allowed_on, today)
            # Built before the mark: a request that cannot be built never
            # reaches the sender, so it leaves no mark to stop a restart.
            request = self._to_request(order)
            with self._step:
                self._require_held()
                self._pending.mark(cid, HANDING_OVER)
                self._send(request)
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
    """Settle a stopped hand-over: mark it sent, or allow it to be sent again in today's session only.

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
    if decision not in (DECIDE_SENT, DECIDE_RESEND):
        raise ResolveRefused("decision {!r} is neither {!r} nor {!r}".format(decision, DECIDE_SENT, DECIDE_RESEND))
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
    if client_order_id not in marks:
        raise ResolveRefused("order {} is not in the hand-over record {}; check the client order ID".format(
            client_order_id, pending.handover_path))
    mark = marks[client_order_id]
    if mark != HANDING_OVER and resend_date(mark) is None:
        raise ResolveRefused("order {} is marked {!r}, not {!r}: it is not a stopped hand-over, so there is "
                             "nothing to settle; check the client order ID".format(
                                 client_order_id, mark, HANDING_OVER))
    if mark != HANDING_OVER and decision == DECIDE_RESEND:
        raise ResolveRefused("order {} is already allowed to be sent again ({!r}); only --sent is accepted, to "
                             "withdraw that permission if IBKR shows the order".format(client_order_id, mark))
    recorded = {"client_order_id": client_order_id, "decision": decision, "reason": reason,
                "at": datetime.now(timezone.utc).isoformat(timespec="seconds"), "session_date": today.isoformat()}
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
    parser = argparse.ArgumentParser(prog="relay.py", description="Settle a stopped hand-over (adapter stopped).")
    commands = parser.add_subparsers(dest="command", required=True)
    cmd = commands.add_parser("resolve", help="mark a stopped hand-over sent, or allow it to be sent again")
    cmd.add_argument("pending_file", help="the adapter's pending-input file (not the .handover file)")
    cmd.add_argument("client_order_id")
    which = cmd.add_mutually_exclusive_group(required=True)
    which.add_argument("--sent", dest="decision", action="store_const", const=DECIDE_SENT,
                       help="IBKR shows the order at all (working, filled or cancelled): never send it again")
    which.add_argument("--resend", dest="decision", action="store_const", const=DECIDE_RESEND,
                       help="IBKR has no trace of it and it was committed today: send it again at its original limit")
    cmd.add_argument("--reason", required=True, help="what IBKR showed; kept with the decision")
    args = parser.parse_args(argv)
    try:
        resolve(PendingInput(args.pending_file), args.client_order_id, args.decision, args.reason, today=today)
    except (ResolveRefused, HandoverRecordInvalid) as err:
        print("refused: {}".format(err), file=sys.stderr)
        return 1
    print("order {} resolved: {}".format(args.client_order_id, args.decision))
    return 0


if __name__ == "__main__":
    sys.exit(main())
