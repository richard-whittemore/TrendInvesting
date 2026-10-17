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
from datetime import date, datetime, timedelta, timezone
from decimal import Decimal
from pathlib import Path

tests_dir = str(Path(__file__).resolve().parent)
if tests_dir not in sys.path:
    sys.path.insert(0, tests_dir)

from orders import BrokerOrders
from relay import (EngineUnavailable, HandoverRecordInvalid, HandoverUnconfirmed, InputChanged, PendingInput,
                   PendingInUse, Relay, ResendExpired, ResolveRefused, main, resolve)
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

    def relay(self, connect, broker=holding(), reconnects=1, send=None, mapper=to_request, today=date(2026, 10, 16)):
        relay = Relay(connect=connect, send=send or self.sent.append, pending=self.pending, broker_orders=broker,
                      to_request=mapper, reconnects=reconnects, today=lambda: today)
        self.addCleanup(relay.close)
        return relay

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
            relay = self.relay(connect, broker=broker)
            relay.exchange(envelope(0))
            relay.close()
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
        crashed.close()

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
        crashed.close()

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

        unshaping = self.relay(Restarts(log, decisions), mapper=unshapeable)
        with self.assertRaises(ValueError):
            unshaping.exchange(envelope(0))
        unshaping.close()
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
        crashed.close()

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
        crashed.close()
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

    def stopped_mid_send(self, inp, decisions, crash_on="tw-1"):
        """The adapter stops after handing crash_on over; IBKR then shows nothing, and the relay stops for a person.

        Each relay is closed as its process ending would, releasing the pending file.
        """
        log = {}
        crashed = self.relay(Restarts(log, decisions), send=CrashingSender(self.sent, crash_on=[crash_on]))
        with self.assertRaises(ConnectionError):
            crashed.exchange(inp)
        crashed.close()
        restarted = self.relay(Restarts(log, decisions))
        with self.assertRaises(HandoverUnconfirmed):
            restarted.exchange(envelope(1))
        restarted.close()
        return log

    def test_a_handover_record_with_an_unknown_mark_stops_the_relay_and_sends_nothing(self):
        # A hand-edited mark such as "Sent" is neither mark: it is refused,
        # never read as "no mark" and the order sent again.
        decisions = {"session:0": [released("tw-1")]}
        log = self.stopped_mid_send(envelope(0), decisions)
        for record in ({"tw-1": "Sent"}, {"tw-1": None}, ["tw-1"], "sent"):
            with open(self.pending.handover_path, "w", encoding="utf-8") as f:
                json.dump(record, f)
            restarted = self.relay(Restarts(log, decisions))
            with self.assertRaises(HandoverRecordInvalid):
                restarted.exchange(envelope(1))
            restarted.close()
        with open(self.pending.handover_path, "w", encoding="utf-8") as f:
            f.write('{"tw-1": sent')
        with self.assertRaises(HandoverRecordInvalid):
            self.relay(Restarts(log, decisions)).exchange(envelope(1))
        self.assertEqual([r.client_order_id for r in self.sent], ["tw-1"], "an unknown mark never sends the order again")
        self.assertEqual(self.pending.load(), envelope(0))

    def test_resolving_a_handover_as_sent_lets_the_relay_go_on_without_sending_it(self):
        decisions = {"session:0": [released("tw-1")]}
        log = self.stopped_mid_send(envelope(0), decisions)
        before = datetime.now(timezone.utc).replace(microsecond=0)
        resolve(self.pending, "tw-1", "sent", "IBKR shows tw-1 cancelled unfilled", today=date(2026, 10, 16))
        self.assertEqual(self.pending.handovers(), {"tw-1": "sent"})
        [recorded] = self.pending.decisions()
        at = recorded.pop("at")
        self.assertEqual(recorded, {"client_order_id": "tw-1", "decision": "sent", "session_date": "2026-10-16",
                                    "reason": "IBKR shows tw-1 cancelled unfilled"})
        when = datetime.fromisoformat(at)
        self.assertEqual(when.utcoffset(), timedelta(0), "the decision's time is kept in UTC")
        self.assertEqual(when.microsecond, 0, "to the second")
        self.assertTrue(before <= when <= datetime.now(timezone.utc))

        self.relay(Restarts(log, decisions)).exchange(envelope(1))
        self.assertEqual([r.client_order_id for r in self.sent], ["tw-1"], "a resolved hand-over is never sent again")
        self.assertIsNone(self.pending.load())

    def test_resolving_a_same_session_handover_for_resend_sends_it_once_more(self):
        inp = dict(envelope(0), event_time="2026-10-16T20:00:00Z")
        decisions = {"session:0": [released("tw-1")]}
        log = self.stopped_mid_send(inp, decisions)
        resolve(self.pending, "tw-1", "resend", "no trace in IBKR today", today=date(2026, 10, 16))
        self.assertEqual(self.pending.handovers(), {"tw-1": "resend:2026-10-16"}, "a resend permission, dated")
        [recorded] = self.pending.decisions()
        self.assertEqual((recorded["decision"], recorded["session_date"]), ("resend", "2026-10-16"))

        self.relay(Restarts(log, decisions)).exchange(envelope(1))
        sent = [(r.client_order_id, r.limit_price) for r in self.sent]
        self.assertEqual(sent, [("tw-1", Decimal("401.5"))] * 2, "resent once, at the original limit")

    def test_a_resend_permission_expires_with_its_day_and_stops_for_another_check(self):
        # Permission is given on the committed day; an adapter started on a
        # later day must not act on it, since the order may have filled or
        # been cancelled where that day's lists no longer show it.
        inp = dict(envelope(0), event_time="2026-10-16T20:00:00Z")
        decisions = {"session:0": [released("tw-1")]}
        log = self.stopped_mid_send(inp, decisions)
        resolve(self.pending, "tw-1", "resend", "no trace in IBKR today", today=date(2026, 10, 16))

        late = self.relay(Restarts(log, decisions), today=date(2026, 10, 17))
        with self.assertRaises(ResendExpired) as raised:
            late.exchange(envelope(1))
        late.close()
        self.assertIsInstance(raised.exception, HandoverUnconfirmed, "it stops the adapter as an unsettled hand-over")
        self.assertIn("tw-1", str(raised.exception))
        self.assertEqual([r.client_order_id for r in self.sent], ["tw-1"], "an expired permission sends nothing")
        self.assertEqual(self.pending.handovers(), {"tw-1": "handing-over"}, "the order is unsettled again")
        with self.assertRaises(ResolveRefused):
            resolve(self.pending, "tw-1", "resend", "still no trace", today=date(2026, 10, 17))
        resolve(self.pending, "tw-1", "sent", "IBKR history shows it cancelled on the 16th", today=date(2026, 10, 17))
        self.assertEqual(self.pending.handovers(), {"tw-1": "sent"})

    def test_an_order_marked_sent_cannot_be_resolved(self):
        # A "sent" mark is written only once the sender returned: the relay's
        # own proof the order was handed over, which no decision may undo.
        inp = dict(envelope(0), event_time="2026-10-16T20:00:00Z")
        decisions = {"session:0": [released("tw-1"), released("tw-2")]}
        log = self.stopped_mid_send(inp, decisions, crash_on="tw-2")
        self.assertEqual(self.pending.handovers(), {"tw-1": "sent", "tw-2": "handing-over"})
        for decision in ("resend", "sent"):
            with self.assertRaises(ResolveRefused) as raised:
                resolve(self.pending, "tw-1", decision, "mistyped ID", today=date(2026, 10, 16))
            self.assertIn("sent", str(raised.exception))
        self.assertEqual(self.pending.handovers(), {"tw-1": "sent", "tw-2": "handing-over"})
        self.assertEqual(self.pending.decisions(), [])

        with self.assertRaises(HandoverUnconfirmed):
            self.relay(Restarts(log, decisions)).exchange(envelope(1))
        self.assertEqual([r.client_order_id for r in self.sent], ["tw-1", "tw-2"], "nothing is sent again")

    def test_a_resend_permission_can_be_withdrawn_by_marking_the_order_sent(self):
        # The owner allowed a resend, then found the order in IBKR's audit
        # trail: the permission must be withdrawable, or the next start that
        # day sends the order again (fail closed, AGENTS.md).
        inp = dict(envelope(0), event_time="2026-10-16T14:00:00Z")
        decisions = {"session:0": [released("tw-1")]}
        log = self.stopped_mid_send(inp, decisions)
        resolve(self.pending, "tw-1", "resend", "no trace in IBKR at 10:00", today=date(2026, 10, 16))
        with self.assertRaises(ResolveRefused):
            resolve(self.pending, "tw-1", "resend", "asked again", today=date(2026, 10, 16))
        self.assertEqual(self.pending.handovers(), {"tw-1": "resend:2026-10-16"})

        resolve(self.pending, "tw-1", "sent", "audit trail shows it accepted then cancelled", today=date(2026, 10, 16))
        self.assertEqual(self.pending.handovers(), {"tw-1": "sent"}, "the permission is withdrawn")
        self.assertEqual([d["decision"] for d in self.pending.decisions()], ["resend", "sent"], "both are kept")

        self.relay(Restarts(log, decisions)).exchange(envelope(1))
        self.assertEqual([r.client_order_id for r in self.sent], ["tw-1"], "a withdrawn permission sends nothing")

    def test_a_resend_mark_is_read_only_in_the_shape_resolve_writes(self):
        # A date reader that also takes the basic and week-date forms, as
        # Python 3.11 and later do: the record's shape must not depend on it.
        import relay as relay_module

        class Lenient(date):
            @classmethod
            def fromisoformat(cls, text):
                text = text.strip()
                if len(text) == 8 and text.isdigit():
                    return date(int(text[:4]), int(text[4:6]), int(text[6:]))
                if len(text) == 10 and text[4:6] == "-W":
                    return date.fromisocalendar(int(text[:4]), int(text[6:8]), int(text[9]))
                return date.fromisoformat(text)

        original = relay_module.date
        relay_module.date = Lenient
        self.addCleanup(setattr, relay_module, "date", original)
        for mark in ("resend:20261016", "resend:2026-W42-5", "resend:2026-10-16\n", "resend: 2026-10-16",
                     "resend:2026-02-30"):
            with self.subTest(mark=mark):
                with open(self.pending.handover_path, "w", encoding="utf-8") as f:
                    json.dump({"tw-1": mark}, f)
                with self.assertRaises(HandoverRecordInvalid):
                    self.pending.handovers()
        with open(self.pending.handover_path, "w", encoding="utf-8") as f:
            json.dump({"tw-1": "resend:2026-10-16"}, f)
        self.assertEqual(self.pending.handovers(), {"tw-1": "resend:2026-10-16"})

    def test_each_relay_holds_its_own_lock_and_a_closed_relay_never_edits_or_sends(self):
        decisions = {"session:0": [released("tw-1")]}
        first = self.relay(Restarts({}, decisions))
        with self.assertRaises(PendingInUse):
            self.relay(Restarts({}, decisions))
        first.close()

        second = self.relay(Restarts({}, decisions))
        first.close()
        with self.assertRaises(ResolveRefused):
            resolve(PendingInput(self.pending.path), "tw-1", "sent", "closing the old relay", today=date(2026, 10, 16))
        with self.assertRaises(PendingInUse):
            first.exchange(envelope(0))
        self.assertEqual(self.sent, [], "a closed relay sends nothing")
        self.assertIsNone(self.pending.load(), "and edits nothing")
        second.close()

    def test_a_relay_closed_mid_settlement_sends_no_further_order(self):
        decisions = {"session:0": [released("tw-1"), released("tw-2")]}
        holder = []

        def send_then_close(request):
            self.sent.append(request)
            holder[0].close()

        holder.append(self.relay(Restarts({}, decisions), send=send_then_close))
        with self.assertRaises(PendingInUse):
            holder[0].exchange(envelope(0))
        self.assertEqual([r.client_order_id for r in self.sent], ["tw-1"], "the lock was released, so tw-2 waits")
        self.assertEqual(self.pending.handovers(), {"tw-1": "handing-over"}, "and the closed relay edits nothing")

    def test_resolve_is_refused_while_an_adapter_holds_the_pending_file(self):
        inp = dict(envelope(0), event_time="2026-10-16T20:00:00Z")
        decisions = {"session:0": [released("tw-1")]}
        log = self.stopped_mid_send(inp, decisions)
        running = self.relay(Restarts(log, decisions))
        for decision in ("sent", "resend"):
            with self.assertRaises(ResolveRefused) as raised:
                resolve(PendingInput(self.pending.path), "tw-1", decision, "IBKR shows nothing",
                        today=date(2026, 10, 16))
            self.assertIn("stop it first", str(raised.exception))
        self.assertEqual(self.pending.handovers(), {"tw-1": "handing-over"})
        self.assertEqual(self.pending.decisions(), [])

        running.close()
        resolve(PendingInput(self.pending.path), "tw-1", "sent", "IBKR shows it filled", today=date(2026, 10, 16))
        self.assertEqual(self.pending.handovers(), {"tw-1": "sent"})

    def test_an_adapter_cannot_start_while_resolve_or_another_adapter_holds_the_pending_file(self):
        decisions = {"session:0": [released("tw-1")]}
        log = self.stopped_mid_send(envelope(0), decisions)
        blocked = []
        other = PendingInput(self.pending.path)
        record = other.record_decision

        def record_while_starting_an_adapter(decision):
            try:
                Relay(connect=Restarts(log, decisions), send=self.sent.append, pending=PendingInput(other.path),
                      broker_orders=holding(), to_request=to_request)
            except PendingInUse as err:
                blocked.append(err)
            record(decision)

        other.record_decision = record_while_starting_an_adapter
        resolve(other, "tw-1", "sent", "IBKR shows it filled", today=date(2026, 10, 16))
        self.assertEqual(len(blocked), 1, "an adapter cannot start while resolve edits the record")

        first = self.relay(Restarts(log, decisions))
        with self.assertRaises(PendingInUse):
            Relay(connect=Restarts(log, decisions), send=self.sent.append, pending=PendingInput(self.pending.path),
                  broker_orders=holding(), to_request=to_request)
        first.close()

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
