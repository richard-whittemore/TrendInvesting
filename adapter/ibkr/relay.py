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
mark, refuses an order the record does not hold, and appends the decision
and its reason to a sidecar file. A resend is allowed only for an order
committed in today's session (in New York): an order from an earlier
session may have filled or been cancelled where today's open orders and
executions no longer show it, so it is marked sent instead. The record is
read strictly (fail closed on unknown, AGENTS.md): anything but a JSON object
from client order ID to one of the two marks raises HandoverRecordInvalid,
and the adapter stops before sending.

An input ID names one input (ADR 0027 section 4): a retry under the ID of
the input awaiting its reply, with other content, is refused (InputChanged)
after the saved original is settled, and never replaces it on disk.
"""
import argparse
import json
import os
import sys
from datetime import datetime
from zoneinfo import ZoneInfo

RELEASED = "risk.order.released"
# The two marks the write-ahead record holds for a client order ID.
HANDING_OVER = "handing-over"
SENT = "sent"
MARKS = (HANDING_OVER, SENT)
# The trading session's calendar: a session's date is its New York date.
SESSION_ZONE = ZoneInfo("America/New_York")
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
        """The write-ahead record: each client order ID's mark, HANDING_OVER or SENT.

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
            if mark not in MARKS:
                raise HandoverRecordInvalid("{} marks order {} {!r}, which is neither {!r} nor {!r}".format(
                    self.handover_path, cid, mark, HANDING_OVER, SENT))
        return marks

    def mark(self, client_order_id, state):
        """Record durably, before the call it precedes returns, that an order is being or was handed over."""
        marks = self.handovers()
        marks[client_order_id] = state
        self._write(self.handover_path, marks)

    def unmark(self, client_order_id):
        """Remove an order's mark, so the relay treats it as never handed over."""
        marks = self.handovers()
        del marks[client_order_id]
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
    asks again before EngineUnavailable reaches the caller.
    """

    def __init__(self, connect, send, pending, broker_orders, to_request, reconnects=1):
        self._connect = connect
        self._send = send
        self._pending = pending
        self._broker_orders = broker_orders
        self._to_request = to_request
        self._reconnects = reconnects
        self._engine = None
        self._sent = set()

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

    def _settle(self, envelope):
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
            if marks.get(cid) == HANDING_OVER:
                raise HandoverUnconfirmed(cid)
            # Built before the mark: a request that cannot be built never
            # reaches the sender, so it leaves no mark to stop a restart.
            request = self._to_request(order)
            self._pending.mark(cid, HANDING_OVER)
            self._send(request)
            self._pending.mark(cid, SENT)
            self._sent.add(cid)
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
    """Settle a stopped hand-over: mark it sent, or remove its mark so it is sent again.

    Refuses (ResolveRefused, nothing changed) an order the record does not
    hold, an unknown decision, an empty reason, and a resend of an order not
    committed in today's session. The order's committed session is its
    input's event time as a New York date: the engine commits the order only
    once it receives that input, so no earlier. A missing or unreadable
    event time refuses the resend. today defaults to today in New York.
    """
    if decision not in (DECIDE_SENT, DECIDE_RESEND):
        raise ResolveRefused("decision {!r} is neither {!r} nor {!r}".format(decision, DECIDE_SENT, DECIDE_RESEND))
    if not reason or not reason.strip():
        raise ResolveRefused("a reason is required: it is kept with the decision")
    marks = pending.handovers()
    if client_order_id not in marks:
        raise ResolveRefused("order {} is not in the hand-over record {}; check the client order ID".format(
            client_order_id, pending.handover_path))
    if today is None:
        today = datetime.now(SESSION_ZONE).date()
    if decision == DECIDE_RESEND:
        committed = _session_date(pending.load())
        if committed != today:
            raise ResolveRefused(
                "order {} was not committed in today's session ({}; committed {}): an order from an earlier "
                "session may have filled or been cancelled where IBKR's open orders and today's trades no longer "
                "show it. Check IBKR's order and trade history for every day since, then mark it sent".format(
                    client_order_id, today, committed or "unknown"))
        pending.record_decision({"client_order_id": client_order_id, "decision": decision, "reason": reason})
        pending.unmark(client_order_id)
    else:
        pending.record_decision({"client_order_id": client_order_id, "decision": decision, "reason": reason})
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
