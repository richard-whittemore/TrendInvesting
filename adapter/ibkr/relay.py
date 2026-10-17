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
client order ID (IBKR's orderRef). One the broker already holds, found by
reconnect matching (orders.match_on_reconnect), is never sent again. A
dry-run release (send false) is never sent (ADR 0026 point 3, "Mode").
"""
import json
import os

RELEASED = "risk.order.released"


class EngineUnavailable(Exception):
    """The engine did not answer an exchange; its reply is asked for again on reconnect."""


class PendingInput:
    """The one input awaiting its reply, kept in a file that survives the adapter's restart."""

    def __init__(self, path):
        self.path = path

    def load(self):
        try:
            with open(self.path, encoding="utf-8") as f:
                return json.load(f)
        except FileNotFoundError:
            return None

    def save(self, envelope):
        tmp = self.path + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            # No fallback encoder: the input is asked for again exactly as it
            # was sent, and the engine refuses a different input under its ID.
            json.dump(envelope, f, separators=(",", ":"), allow_nan=False)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, self.path)
        self._sync_dir()

    def clear(self):
        try:
            os.remove(self.path)
        except FileNotFoundError:
            return
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
    placed(client_order_id) reports whether the broker already holds an
    order with that tag. to_request maps a released order's payload to an
    OrderRequest. reconnects bounds how often one exchange reconnects and
    asks again before EngineUnavailable reaches the caller.
    """

    def __init__(self, connect, send, pending, placed, to_request, reconnects=1):
        self._connect = connect
        self._send = send
        self._pending = pending
        self._placed = placed
        self._to_request = to_request
        self._reconnects = reconnects
        self._engine = None
        self._sent = set()

    def exchange(self, envelope):
        """Send one input and act on its reply, after settling any input still awaiting one."""
        waiting = self._pending.load()
        if waiting is not None and waiting.get("id") != envelope.get("id"):
            self._settle(waiting)
        return self._settle(envelope)

    def _settle(self, envelope):
        self._pending.save(envelope)
        reply = self._ask(envelope)
        for order in released_orders(reply):
            cid = order["client_order_id"]
            if cid in self._sent or self._placed(cid):
                continue
            self._send(self._to_request(order))
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


def released_orders(reply):
    """The released orders a reply carries that are to be sent, in emission order."""
    for decision in reply["payload"]["decisions"]:
        if decision.get("type") == RELEASED and decision["payload"].get("send") is True:
            yield decision["payload"]
