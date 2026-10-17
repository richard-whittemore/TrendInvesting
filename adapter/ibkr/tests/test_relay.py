"""Re-delivery: a reply lost to a crash is asked for again, and its order is sent once.

ADR 0027 section 4: the engine flushes an input's decisions before it
replies, so a crash can leave a released order durable but unsent. The
adapter asks for that input's committed reply by sending the input again
under its ID; the engine answers with the reply it recorded, and never
applies the input twice. The adapter sends each released order once.
"""
import copy
import json
import sys
import tempfile
import unittest
from datetime import date
from decimal import Decimal
from pathlib import Path

tests_dir = str(Path(__file__).resolve().parent)
if tests_dir not in sys.path:
    sys.path.insert(0, tests_dir)

from orders import BrokerOrders
from relay import (EngineUnavailable, HandoverRecordInvalid, HandoverUnconfirmed, InputChanged, PendingInput, Relay,
                   ResolveRefused, main, resolve)
from sender import OrderRequest


def released(client_order_id, send=True):
    return {"type": "risk.order.released", "payload": {
        "account_id": "DU1234567", "client_order_id": client_order_id, "instrument_id": "SPY",
        "side": "buy", "quantity": 9, "limit_price": Decimal("401.5"), "mode": "paper" if send else "dry-run",
        "send": send}}


class FakeEngine:
    """The engine's side of the exchange: a log of committed replies by input ID.

    decide commits the reply for a new input, then fails if told to crash
    before replying; an input it has committed is answered from the log.
    """

    def __init__(self, log, decisions, crash_before_reply=()):
        self.log = log
        self.decisions = decisions
        self.crash_before_reply = set(crash_before_reply)
        self.asked = []

    def decide(self, envelope):
        self.asked.append(envelope["id"])
        if envelope["id"] in self.log:
            committed, reply = self.log[envelope["id"]]
            if committed != envelope:
                raise ValueError("input {} was committed with different content".format(envelope["id"]))
            return copy.deepcopy(reply)
        reply = {"id": "decisions:" + envelope["id"], "causation_id": envelope["id"],
                 "payload": {"decisions": self.decisions.get(envelope["id"], [])}}
        self.log[envelope["id"]] = (copy.deepcopy(envelope), reply)
        if envelope["id"] in self.crash_before_reply:
            self.crash_before_reply.discard(envelope["id"])
            raise EngineUnavailable("engine crashed after committing {}".format(envelope["id"]))
        return copy.deepcopy(reply)


class Restarts:
    """Connects to the engine; each reconnect is a restarted engine on the same log."""

    def __init__(self, log, decisions, crash_before_reply=()):
        self.log, self.decisions = log, decisions
        self.engines = []
        self.crash = list(crash_before_reply)

    def __call__(self):
        engine = FakeEngine(self.log, self.decisions, self.crash)
        self.crash = []
        self.engines.append(engine)
        return engine


class Tagged:
    """An open order or execution as the broker reports it, by its tag alone."""

    def __init__(self, client_order_id, client_id=7):
        self.client_order_id = client_order_id
        self.client_id = client_id


def holding(open_orders=(), executions=()):
    """A broker whose open orders and executions carry these tags."""
    return lambda: BrokerOrders(open_orders=[Tagged(c) for c in open_orders], executions=[Tagged(c) for c in executions])


class CrashingSender:
    """Records each order handed over, then fails as a process dying mid-send would."""

    def __init__(self, sent, crash_on=()):
        self.sent = sent
        self.crash_on = set(crash_on)

    def __call__(self, request):
        self.sent.append(request)
        if request.client_order_id in self.crash_on:
            self.crash_on.discard(request.client_order_id)
            raise ConnectionError("adapter stopped after handing over {}".format(request.client_order_id))


def to_request(order):
    return OrderRequest(client_order_id=order["client_order_id"], account_id=order["account_id"], con_id=756733,
                        symbol=order["instrument_id"], action=order["side"].upper(), quantity=Decimal(order["quantity"]),
                        order_type="LMT", limit_price=order["limit_price"], tif="DAY")


def envelope(i):
    return {"id": "session:{}".format(i), "type": "market.session.closed", "sequence": 2 + i}


class RelayTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.pending = PendingInput(str(Path(self.dir.name) / "pending.json"))
        self.sent = []

    def relay(self, connect, broker=holding(), reconnects=1, send=None, mapper=to_request):
        return Relay(connect=connect, send=send or self.sent.append, pending=self.pending, broker_orders=broker,
                     to_request=mapper, reconnects=reconnects)

    def test_a_reply_lost_to_a_crash_is_asked_for_again_and_its_order_sent_once(self):
        log = {}
        connect = Restarts(log, {"session:0": [released("tw-1")]}, crash_before_reply=["session:0"])
        relay = self.relay(connect)
        reply = relay.exchange(envelope(0))
        self.assertEqual(reply["causation_id"], "session:0")
        self.assertEqual([r.client_order_id for r in self.sent], ["tw-1"])
        self.assertEqual(len(connect.engines), 2, "the adapter reconnects to the restarted engine")
        self.assertEqual(connect.engines[1].asked, ["session:0"], "it asks again for the same input, by its ID")
        self.assertIsNone(self.pending.load())

    def test_an_input_answered_again_never_sends_its_order_twice(self):
        log = {}
        connect = Restarts(log, {"session:0": [released("tw-1")]})
        relay = self.relay(connect)
        relay.exchange(envelope(0))
        relay.exchange(envelope(0))
        self.assertEqual([r.client_order_id for r in self.sent], ["tw-1"])

    def test_an_order_the_broker_already_holds_is_not_sent_again(self):
        log = {}
        connect = Restarts(log, {"session:0": [released("tw-1")]}, crash_before_reply=["session:0"])
        for broker in (holding(open_orders=["tw-1"]), holding(executions=["tw-1"])):
            self.relay(connect, broker=broker).exchange(envelope(0))
        self.assertEqual(self.sent, [], "an open order or an execution carrying the tag is the broker's already")

    def test_an_order_handed_over_before_a_crash_is_never_sent_again(self):
        # The adapter dies after the broker took tw-1 and before it recorded
        # the reply as acted on; by the restart tw-1 has filled, so it is not
        # an open order, and only its execution shows the broker had it.
        log = {}
        decisions = {"session:0": [released("tw-1"), released("tw-2")]}
        crashed = self.relay(Restarts(log, decisions), send=CrashingSender(self.sent, crash_on=["tw-1"]))
        with self.assertRaises(ConnectionError):
            crashed.exchange(envelope(0))

        self.relay(Restarts(log, decisions), broker=holding(executions=["tw-1"])).exchange(envelope(1))
        self.assertEqual([r.client_order_id for r in self.sent], ["tw-1", "tw-2"])
        self.assertIsNone(self.pending.load())

    def test_a_handover_the_broker_does_not_show_stops_the_relay_rather_than_resend(self):
        # The order may have been taken and cancelled, or never arrived: the
        # relay cannot tell which, so it sends nothing more and a person decides.
        log = {}
        decisions = {"session:0": [released("tw-1")]}
        crashed = self.relay(Restarts(log, decisions), send=CrashingSender(self.sent, crash_on=["tw-1"]))
        with self.assertRaises(ConnectionError):
            crashed.exchange(envelope(0))

        connect = Restarts(log, decisions)
        restarted = self.relay(connect)
        for _ in range(2):
            with self.assertRaises(HandoverUnconfirmed) as raised:
                restarted.exchange(envelope(1))
            self.assertIn("tw-1", str(raised.exception))
        self.assertEqual([r.client_order_id for r in self.sent], ["tw-1"], "the handed-over order is never sent twice")
        self.assertEqual(self.pending.load(), envelope(0), "its input stays on disk for the person to settle")
        self.assertEqual(connect.engines[0].asked, ["session:0", "session:0"], "nothing new is sent while it is unsettled")

    def test_an_order_the_mapper_cannot_shape_leaves_no_mark_and_is_sent_after_a_restart(self):
        # Nothing reaches the sender when the request cannot be built, so the
        # order provably never left the process and a restart sends it.
        log = {}
        decisions = {"session:0": [released("tw-1")]}

        def unshapeable(order):
            raise ValueError("cannot shape {}".format(order["client_order_id"]))

        with self.assertRaises(ValueError):
            self.relay(Restarts(log, decisions), mapper=unshapeable).exchange(envelope(0))
        self.assertEqual(self.pending.handovers(), {}, "no hand-over is recorded for a request never built")

        self.relay(Restarts(log, decisions)).exchange(envelope(1))
        self.assertEqual([r.client_order_id for r in self.sent], ["tw-1"])
        self.assertIsNone(self.pending.load())

    def test_a_changed_retry_under_a_pending_id_is_refused_and_the_original_settled(self):
        log = {}
        decisions = {"session:0": [released("tw-1")]}
        crashed = self.relay(Restarts(log, decisions, crash_before_reply=["session:0"]), reconnects=0)
        with self.assertRaises(EngineUnavailable):
            crashed.exchange(envelope(0))

        changed = dict(envelope(0), sequence=99)
        connect = Restarts(log, decisions)
        with self.assertRaises(InputChanged):
            self.relay(connect).exchange(changed)
        self.assertEqual(log["session:0"][0], envelope(0), "the engine was asked for the input it committed")
        self.assertEqual([r.client_order_id for r in self.sent], ["tw-1"], "the original's order is sent, once")
        self.assertIsNone(self.pending.load(), "the original is settled, never overwritten by the caller's copy")

    def test_a_restarted_adapter_asks_for_its_pending_input_before_the_next(self):
        log = {}
        decisions = {"session:0": [released("tw-1")]}
        # The adapter itself dies with the engine: no reconnect.
        crashed = self.relay(Restarts(log, decisions, crash_before_reply=["session:0"]), reconnects=0)
        with self.assertRaises(EngineUnavailable):
            crashed.exchange(envelope(0))
        self.assertEqual(self.pending.load(), envelope(0), "the input awaiting its reply survives the adapter")
        self.assertEqual(self.sent, [])

        connect = Restarts(log, decisions)
        restarted = self.relay(connect)
        restarted.exchange(envelope(1))
        self.assertEqual(connect.engines[0].asked, ["session:0", "session:1"])
        self.assertEqual([r.client_order_id for r in self.sent], ["tw-1"])

    def test_a_dry_run_release_is_never_sent(self):
        connect = Restarts({}, {"session:0": [released("tw-1", send=False)]})
        self.relay(connect).exchange(envelope(0))
        self.assertEqual(self.sent, [])

    def stopped_mid_send(self, inp, decisions):
        """The adapter stops after handing tw-1 over; IBKR then shows nothing, and the relay stops for a person."""
        log = {}
        crashed = self.relay(Restarts(log, decisions), send=CrashingSender(self.sent, crash_on=["tw-1"]))
        with self.assertRaises(ConnectionError):
            crashed.exchange(inp)
        with self.assertRaises(HandoverUnconfirmed):
            self.relay(Restarts(log, decisions)).exchange(envelope(1))
        return log

    def test_a_handover_record_with_an_unknown_mark_stops_the_relay_and_sends_nothing(self):
        # A hand-edited mark such as "Sent" is neither mark: it is refused,
        # never read as "no mark" and the order sent again.
        decisions = {"session:0": [released("tw-1")]}
        log = self.stopped_mid_send(envelope(0), decisions)
        for record in ({"tw-1": "Sent"}, {"tw-1": None}, ["tw-1"], "sent"):
            with open(self.pending.handover_path, "w", encoding="utf-8") as f:
                json.dump(record, f)
            with self.assertRaises(HandoverRecordInvalid):
                self.relay(Restarts(log, decisions)).exchange(envelope(1))
        with open(self.pending.handover_path, "w", encoding="utf-8") as f:
            f.write('{"tw-1": sent')
        with self.assertRaises(HandoverRecordInvalid):
            self.relay(Restarts(log, decisions)).exchange(envelope(1))
        self.assertEqual([r.client_order_id for r in self.sent], ["tw-1"], "an unknown mark never sends the order again")
        self.assertEqual(self.pending.load(), envelope(0))

    def test_resolving_a_handover_as_sent_lets_the_relay_go_on_without_sending_it(self):
        decisions = {"session:0": [released("tw-1")]}
        log = self.stopped_mid_send(envelope(0), decisions)
        resolve(self.pending, "tw-1", "sent", "IBKR shows tw-1 cancelled unfilled", today=date(2026, 10, 16))
        self.assertEqual(self.pending.handovers(), {"tw-1": "sent"})
        self.assertEqual(self.pending.decisions(), [{"client_order_id": "tw-1", "decision": "sent",
                                                     "reason": "IBKR shows tw-1 cancelled unfilled"}])

        self.relay(Restarts(log, decisions)).exchange(envelope(1))
        self.assertEqual([r.client_order_id for r in self.sent], ["tw-1"], "a resolved hand-over is never sent again")
        self.assertIsNone(self.pending.load())

    def test_resolving_a_same_session_handover_for_resend_sends_it_once_more(self):
        inp = dict(envelope(0), event_time="2026-10-16T20:00:00Z")
        decisions = {"session:0": [released("tw-1")]}
        log = self.stopped_mid_send(inp, decisions)
        resolve(self.pending, "tw-1", "resend", "no trace in IBKR today", today=date(2026, 10, 16))
        self.assertEqual(self.pending.handovers(), {})

        self.relay(Restarts(log, decisions)).exchange(envelope(1))
        sent = [(r.client_order_id, r.limit_price) for r in self.sent]
        self.assertEqual(sent, [("tw-1", Decimal("401.5"))] * 2, "resent once, at the original limit")

    def test_a_resend_is_refused_unless_the_order_was_committed_in_todays_session(self):
        for inp in (dict(envelope(0), event_time="2026-10-15T20:00:00Z"), envelope(0),
                    dict(envelope(0), event_time="not a time"), dict(envelope(0), event_time="2026-10-16T16:00:00")):
            with self.subTest(inp=inp):
                self.pending.clear()
                decisions = {"session:0": [released("tw-1")]}
                self.stopped_mid_send(inp, decisions)
                with self.assertRaises(ResolveRefused):
                    resolve(self.pending, "tw-1", "resend", "no trace in IBKR", today=date(2026, 10, 16))
                self.assertEqual(self.pending.handovers(), {"tw-1": "handing-over"}, "the mark is left as it was")

    def test_resolving_an_order_the_record_does_not_hold_is_refused(self):
        decisions = {"session:0": [released("tw-1")]}
        self.stopped_mid_send(envelope(0), decisions)
        for cid, decision, reason in (("tw-9", "sent", "typo"), ("tw-1", "maybe", "unsure"), ("tw-1", "sent", " ")):
            with self.assertRaises(ResolveRefused):
                resolve(self.pending, cid, decision, reason, today=date(2026, 10, 16))
        self.assertEqual(self.pending.handovers(), {"tw-1": "handing-over"})
        self.assertEqual(self.pending.decisions(), [])

    def test_the_resolve_command_writes_the_decision_and_refuses_with_a_nonzero_exit(self):
        decisions = {"session:0": [released("tw-1")]}
        self.stopped_mid_send(envelope(0), decisions)
        path = self.pending.path
        self.assertEqual(main(["resolve", path, "tw-9", "--sent", "--reason", "typo"], today=date(2026, 10, 16)), 1)
        self.assertEqual(main(["resolve", path, "tw-1", "--sent", "--reason", "IBKR shows it filled"],
                              today=date(2026, 10, 16)), 0)
        self.assertEqual(self.pending.handovers(), {"tw-1": "sent"})


if __name__ == "__main__":
    unittest.main()
