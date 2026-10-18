"""Re-delivery: a reply lost to a crash is asked for again, and its order is sent once.

ADR 0027 section 4: the engine flushes an input's decisions before it
replies, so a crash can leave a released order durable but unsent. The
adapter asks for that input's committed reply by sending the input again
under its ID; the engine answers with the reply it recorded, and never
applies the input twice. The adapter sends each released order once.
"""
import copy
import hashlib
import json
import sys
import tempfile
import threading
import unittest
from datetime import date, datetime, timedelta, timezone
from decimal import Decimal
from pathlib import Path

tests_dir = str(Path(__file__).resolve().parent)
if tests_dir not in sys.path:
    sys.path.insert(0, tests_dir)

from orders import BrokerOrders
from relay import (MAX_CLOSE_TIMEOUT, EngineUnavailable, HandoverRecordInvalid, HandoverUnconfirmed, InputChanged,
                   NeverSentAtBroker, PendingInput, PendingInUse, Relay, RelayCloseTimeout, ResendExpired, ResolveRefused,
                   acknowledge_correction, acknowledge_late_fill, journalled_fills, journalled_reports, main,
                   resolve)
from sender import OrderRequest

TESTDATA = Path(__file__).resolve().parent / "testdata"


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


class Sending:
    """A sender as the relay takes one: check refuses what send would, before any mark; send hands it over."""

    def __init__(self, send, check=None):
        self._send = send
        self._check = check

    def check(self, request):
        if self._check is not None:
            self._check(request)
        return request

    def send(self, request):
        return self._send(request)


def to_request(order, session=None):
    return OrderRequest(client_order_id=order["client_order_id"], account_id=order["account_id"], con_id=756733,
                        symbol=order["instrument_id"], action=order["side"].upper(), quantity=Decimal(order["quantity"]),
                        order_type="LMT", limit_price=order["limit_price"], tif="DAY", session=session)


# The fills the engine's durable log holds, for the acknowledgement tests.
FILLS = frozenset({"e.1.01"})
# The engine's durable log as internal/journal writes it (ADR 0027): a
# header, then each input's group of records closed by a commit.
LOG_CHAIN = ("seed=sha256(canonical_log_header_bytes); "
             "record=sha256(previous_record_hash||kind||canonical_envelope_bytes)")
LOG_CONFIGURATION = "sha256:" + "0" * 64
LOG_STRATEGY = "target-weight-ira/1.0.0+test"


def log_header():
    return json.dumps({"log_version": 1, "chain_algorithm": LOG_CHAIN, "configuration_hash": LOG_CONFIGURATION,
                       "strategy_version": LOG_STRATEGY}, separators=(",", ":")) + "\n"


def fill_payload(fill_id, account_id="DU1234567"):
    return {"account_id": account_id, "client_order_id": "tw-1", "fill_id": fill_id, "instrument_id": "SPY",
            "side": "buy", "quantity": 4, "price": 401.25, "commission": 1.0, "filled_at": "2026-10-17T13:30:01Z"}


def log_group(sequence, entries, committed=True):
    """One group of the engine's durable log from sequence on: (kind, type, schema version, payload) lines, then its commit.

    The record hashes are stand-ins: the relay reads the log's shape and each
    payload against its hash, and the chain is the engine's to verify
    (testdata/engine_log.jsonl is a log the engine's own writer made).
    """
    lines = []
    for n, (kind, event_type, schema_version, payload) in enumerate(entries, sequence):
        raw = json.dumps(payload, separators=(",", ":"))
        envelope = json.dumps({"id": "e:{}".format(n), "type": event_type, "envelope_version": 1,
                               "schema_version": schema_version, "event_time": "2026-10-17T13:30:00Z",
                               "recorded_at": "2026-10-17T13:30:00Z", "sequence": n, "source": "ibkr-adapter",
                               "strategy_version": LOG_STRATEGY, "configuration_hash": LOG_CONFIGURATION,
                               "payload_hash": hashlib.sha256(raw.encode()).hexdigest()}, separators=(",", ":"))
        record_hash = "sha256:" + hashlib.sha256("record {}".format(n).encode()).hexdigest()
        lines.append('{{"record":{{"sequence":{},"kind":"{}","envelope":{},"payload":{}}},"record_hash":"{}"}}}}\n'
                     .format(n, kind, envelope[:-1], raw, record_hash))
    if committed:
        lines.append(json.dumps({"commit": {"sequence": sequence + len(entries) - 1, "record_hash": record_hash}},
                                separators=(",", ":")) + "\n")
    return lines


def write_engine_journal(path, fill_ids):
    """The engine's durable log holding a committed input fill for each of fill_ids, among other groups."""
    lines = [log_header()]
    lines += log_group(1, [("input", "market.session.closed", 1, {"session": "2026-10-16"}),
                           ("decision", "risk.order.released", 1, {"client_order_id": "tw-1"})])
    for n, fill_id in enumerate(fill_ids, 3):
        lines += log_group(n, [("input", "execution.fill", 5, fill_payload(fill_id))])
    Path(path).write_text("".join(lines), encoding="utf-8")
    return str(path)


def lifecycle_payload(client_order_id, status, account_id="DU1234567"):
    return {"account_id": account_id, "client_order_id": client_order_id, "order_id": "order:11:7",
            "instrument_id": "SPY", "side": "buy", "status": status, "quantity": 9, "limit_price": 401.5,
            "occurred_at": "2026-10-17T13:30:00Z", "message": ""}


def write_ended_journal(path, fill_ids, statuses):
    """The engine's durable log holding committed fills, then a lifecycle input (client order ID, status) for each of statuses."""
    write_engine_journal(path, fill_ids)
    lines = []
    for n, (client_order_id, status) in enumerate(statuses, 3 + len(fill_ids)):
        lines += log_group(n, [("input", "execution.order.lifecycle", 2, lifecycle_payload(client_order_id, status))])
    with open(path, "a", encoding="utf-8") as f:
        f.write("".join(lines))
    return str(path)


def envelope(i):
    return {"id": "session:{}".format(i), "type": "market.session.closed", "sequence": 2 + i}


class RelayTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.pending = PendingInput(str(Path(self.dir.name) / "pending.json"))
        self.sent = []

    def relay(self, connect, broker=holding(), reconnects=1, send=None, mapper=to_request, today=date(2026, 10, 16),
              **options):
        sender = send if isinstance(send, Sending) else Sending(send or self.sent.append)
        relay = Relay(connect=connect, sender=sender, pending=self.pending, broker_orders=broker,
                      to_request=mapper, reconnects=reconnects, today=lambda: today, **options)
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

        def unshapeable(order, session):
            raise ValueError("cannot shape {}".format(order["client_order_id"]))

        unshaping = self.relay(Restarts(log, decisions), mapper=unshapeable)
        with self.assertRaises(ValueError):
            unshaping.exchange(envelope(0))
        unshaping.close()
        self.assertEqual(self.pending.handovers(), {}, "no hand-over is recorded for a request never built")

        self.relay(Restarts(log, decisions)).exchange(envelope(1))
        self.assertEqual([r.client_order_id for r in self.sent], ["tw-1"])
        self.assertIsNone(self.pending.load())

    def test_each_request_is_built_with_the_session_of_the_input_that_released_it(self):
        # A released order is for its own session (ADR 0026 point 2): the
        # request carries its input's New York date, or none when the input
        # has no readable time, and the sender refuses it in another session.
        log = {}
        decisions = {"session:0": [released("tw-1")], "session:1": [released("tw-2")]}
        relay = self.relay(Restarts(log, decisions))
        relay.exchange(dict(envelope(0), event_time="2026-10-17T01:30:00Z"))
        relay.exchange(envelope(1))
        self.assertEqual([(r.client_order_id, r.session) for r in self.sent],
                         [("tw-1", date(2026, 10, 16)), ("tw-2", None)])

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
            json.dump({"tw-1": "Sent"}, f)
        with self.assertRaises(HandoverRecordInvalid) as unknown:
            self.pending.handovers()
        for mark in ("handing-over", "sent", "never-sent", "resend:YYYY-MM-DD"):
            self.assertIn(repr(mark), str(unknown.exception), "the message names every mark the record may hold")
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

    def test_close_from_another_thread_waits_for_an_order_being_sent_and_leaves_it_marked_sent(self):
        decisions = {"session:0": [released("tw-1")]}
        in_send, let_send_return = threading.Event(), threading.Event()
        close_returned = threading.Event()

        def blocking_send(request):
            self.sent.append(request)
            in_send.set()
            let_send_return.wait(timeout=5)

        # Stubbed only so the record can be read after the settlement, which
        # would otherwise clear it; the real save and clear paths are covered
        # by the tests that pause inside them.
        self.pending.clear = lambda: None
        relay = self.relay(Restarts({}, decisions), send=blocking_send)
        settling = threading.Thread(target=relay.exchange, args=(envelope(0),))
        closing = threading.Thread(target=lambda: (relay.close(), close_returned.set()))
        settling.start()
        self.assertTrue(in_send.wait(timeout=5))
        closing.start()
        self.assertFalse(close_returned.wait(timeout=0.3), "close waits for the send in flight to return")
        let_send_return.set()
        settling.join(timeout=5)
        closing.join(timeout=5)
        self.assertTrue(close_returned.is_set())
        self.assertEqual(self.pending.handovers(), {"tw-1": "sent"})
        with self.assertRaises(ResolveRefused) as refused:
            resolve(PendingInput(self.pending.path), "tw-1", "sent", "already sent", today=date(2026, 10, 16))
        self.assertNotIn("stop it first", str(refused.exception), "the lock was released once the send was marked")

    def _close_waits_inside(self, method, decisions, check):
        """Close from another thread while the real PendingInput.<method> is running must wait for it.

        The real method runs after a pause (it is wrapped, not replaced), so
        what is covered is the file edit itself; check sees the record once
        close has returned.
        """
        inside, let_go = threading.Event(), threading.Event()
        close_returned = threading.Event()
        real = getattr(self.pending, method)

        def paused(*args):
            inside.set()
            let_go.wait(timeout=5)
            return real(*args)

        setattr(self.pending, method, paused)
        relay = self.relay(Restarts({}, decisions))
        settling = threading.Thread(target=relay.exchange, args=(envelope(0),))
        closing = threading.Thread(target=lambda: (relay.close(), close_returned.set()))
        settling.start()
        self.assertTrue(inside.wait(timeout=5))
        closing.start()
        self.assertFalse(close_returned.wait(timeout=0.3), "close waits for the {} in flight".format(method))
        let_go.set()
        settling.join(timeout=5)
        closing.join(timeout=5)
        self.assertTrue(close_returned.is_set())
        check()

    def test_close_from_another_thread_waits_for_the_input_being_saved(self):
        def saved():
            # The save finished whole before close released the lock; the
            # settlement then either finished too or stopped at its next
            # step, the relay being closed. Never a torn record.
            self.assertIn(self.pending.load(), (envelope(0), None))
            self.assertEqual(self.pending.handovers(), {})

        self._close_waits_inside("save", {"session:0": []}, saved)

    def test_close_from_another_thread_waits_for_the_record_being_cleared(self):
        def cleared():
            self.assertIsNone(self.pending.load())
            self.assertEqual(self.pending.handovers(), {}, "both files are removed, never half")

        self._close_waits_inside("clear", {"session:0": [released("tw-1")]}, cleared)

    def test_close_waits_for_the_resend_expired_mark(self):
        self.pending.save(envelope(0))
        self.pending.mark("tw-1", "resend:2026-10-15")
        inside, let_go = threading.Event(), threading.Event()
        close_returned = threading.Event()
        real = self.pending.mark

        def paused(cid, state):
            inside.set()
            let_go.wait(timeout=5)
            return real(cid, state)

        self.pending.mark = paused
        relay = self.relay(Restarts({}, {"session:0": [released("tw-1")]}))
        errors = []

        def settle():
            try:
                relay.exchange(envelope(0))
            except ResendExpired as err:
                errors.append(err)

        settling = threading.Thread(target=settle)
        closing = threading.Thread(target=lambda: (relay.close(), close_returned.set()))
        settling.start()
        self.assertTrue(inside.wait(timeout=5))
        closing.start()
        self.assertFalse(close_returned.wait(timeout=0.3), "close waits for the mark in flight")
        let_go.set()
        settling.join(timeout=5)
        closing.join(timeout=5)
        self.assertEqual(len(errors), 1)
        self.assertEqual(real.__self__.handovers(), {"tw-1": "handing-over"})

    def test_close_gives_up_after_its_timeout_while_the_sender_runs_and_works_once_it_returns(self):
        in_send, let_send_return = threading.Event(), threading.Event()

        def blocking_send(request):
            self.sent.append(request)
            in_send.set()
            let_send_return.wait(timeout=5)

        relay = self.relay(Restarts({}, {"session:0": [released("tw-1")]}), send=blocking_send, close_timeout=60)
        settling = threading.Thread(target=relay.exchange, args=(envelope(0),))
        settling.start()
        self.assertTrue(in_send.wait(timeout=5))
        with self.assertRaises(RelayCloseTimeout):
            relay.close(timeout=0.2)
        with self.assertRaises(ResolveRefused) as refused:
            resolve(PendingInput(self.pending.path), "tw-1", "sent", "still held", today=date(2026, 10, 16))
        self.assertIn("stop it first", str(refused.exception), "a timed-out close leaves the lock held")
        let_send_return.set()
        settling.join(timeout=5)
        relay.close()

    def test_a_close_timeout_that_is_not_a_finite_positive_number_is_refused_at_construction(self):
        for bad in (None, -1, 0, float("nan"), float("inf"), True, "5"):
            with self.subTest(close_timeout=bad):
                with self.assertRaises(ValueError) as refused:
                    self.relay(Restarts({}, {}), close_timeout=bad)
                self.assertIn("close_timeout", str(refused.exception))
        self.relay(Restarts({}, {}), close_timeout=0.5)

    def test_a_close_timeout_that_is_not_a_finite_positive_number_is_refused_by_close(self):
        relay = self.relay(Restarts({}, {}))
        for bad in (-1, 0, float("nan"), float("inf"), True, "5"):
            with self.subTest(timeout=bad):
                with self.assertRaises(ValueError) as refused:
                    relay.close(timeout=bad)
                self.assertIn("timeout", str(refused.exception))
        with self.assertRaises(PendingInUse):
            self.relay(Restarts({}, {}))

    def test_a_timeout_above_the_stated_maximum_is_a_value_error_not_an_overflow(self):
        for too_long in (MAX_CLOSE_TIMEOUT + 1, 1e10, 10 ** 400):
            with self.subTest(timeout=too_long):
                with self.assertRaises(ValueError):
                    self.relay(Restarts({}, {}), close_timeout=too_long)
                relay = self.relay(Restarts({}, {}))
                with self.assertRaises(ValueError):
                    relay.close(timeout=too_long)
                relay.close(timeout=MAX_CLOSE_TIMEOUT)

    def test_close_uses_the_configured_timeout_by_default(self):
        in_send, let_send_return = threading.Event(), threading.Event()

        def blocking_send(request):
            in_send.set()
            let_send_return.wait(timeout=5)

        relay = self.relay(Restarts({}, {"session:0": [released("tw-1")]}), send=blocking_send, close_timeout=0.2)
        settling = threading.Thread(target=relay.exchange, args=(envelope(0),))
        settling.start()
        self.assertTrue(in_send.wait(timeout=5))
        with self.assertRaises(RelayCloseTimeout):
            relay.close()
        let_send_return.set()
        settling.join(timeout=5)

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
                Relay(connect=Restarts(log, decisions), sender=Sending(self.sent.append), pending=PendingInput(other.path),
                      broker_orders=holding(), to_request=to_request)
            except PendingInUse as err:
                blocked.append(err)
            record(decision)

        other.record_decision = record_while_starting_an_adapter
        resolve(other, "tw-1", "sent", "IBKR shows it filled", today=date(2026, 10, 16))
        self.assertEqual(len(blocked), 1, "an adapter cannot start while resolve edits the record")

        first = self.relay(Restarts(log, decisions))
        with self.assertRaises(PendingInUse):
            Relay(connect=Restarts(log, decisions), sender=Sending(self.sent.append), pending=PendingInput(self.pending.path),
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

    def test_an_order_the_sender_refuses_for_good_is_settled_never_sent_and_the_relay_goes_on(self):
        # A release the sender refuses every time it is asked (one for an
        # earlier session, or one it can never shape) leaves no mark, so the
        # owner settles it as never sent: a refusal with its reason, which
        # the relay skips, never a cancel of an order at IBKR.
        log = {}
        decisions = {"session:0": [released("tw-1")]}

        def refused(order, session):
            raise ValueError("order {} was released for an earlier session".format(order["client_order_id"]))

        stuck = self.relay(Restarts(log, decisions), mapper=refused)
        with self.assertRaises(ValueError):
            stuck.exchange(envelope(0))
        stuck.close()
        self.assertEqual(self.pending.handovers(), {})
        with self.assertRaises(ResolveRefused):
            resolve(self.pending, "tw-1", "never-sent", " ", today=date(2026, 10, 16))
        resolve(self.pending, "tw-1", "never-sent", "released 2026-10-15, refused as stale", today=date(2026, 10, 16))
        self.assertEqual(self.pending.handovers(), {"tw-1": "never-sent"})
        [recorded] = self.pending.decisions()
        recorded.pop("at")
        self.assertEqual(recorded, {"client_order_id": "tw-1", "decision": "never-sent", "session_date": "2026-10-16",
                                    "reason": "released 2026-10-15, refused as stale"})

        goes_on = self.relay(Restarts(log, decisions), mapper=refused)
        goes_on.exchange(envelope(1))
        self.assertEqual(self.sent, [], "an order settled as never sent is never handed over")
        self.assertIsNone(self.pending.load(), "the relay went on to the next input")

    def settled_never_sent(self, log, decisions):
        """The sender refuses tw-1 for good, and the owner settles it as never sent."""
        def refused(order, session):
            raise ValueError("order {} was released for an earlier session".format(order["client_order_id"]))

        stuck = self.relay(Restarts(log, decisions), mapper=refused)
        with self.assertRaises(ValueError):
            stuck.exchange(envelope(0))
        stuck.close()
        resolve(self.pending, "tw-1", "never-sent", "IBKR shows no order tagged tw-1", today=date(2026, 10, 16))
        return refused

    def test_a_never_sent_order_ibkr_shows_open_or_executed_stops_for_a_person(self):
        # A never-sent mark is the owner's judgement that the order never
        # left the process; IBKR showing its tag contradicts it, so the relay
        # stops rather than pass over an order the broker holds.
        for where in ("open_orders", "executions"):
            with self.subTest(where=where):
                self.pending.clear()
                for path in (self.pending.handover_path, self.pending.refused_path):
                    Path(path).unlink(missing_ok=True)
                log, decisions = {}, {"session:0": [released("tw-1")]}
                refused = self.settled_never_sent(log, decisions)
                broker = holding(**{where: ["tw-1"]})
                contradicted = self.relay(Restarts(log, decisions), broker=broker, mapper=refused)
                with self.assertRaises(HandoverUnconfirmed) as raised:
                    contradicted.exchange(envelope(1))
                contradicted.close()
                self.assertIn("never sent", str(raised.exception))
                self.assertEqual(self.sent, [])
                self.assertEqual(self.pending.load(), envelope(0), "the input still awaits its reply")
                # The owner, having found it at IBKR, marks it sent; then the
                # relay goes on without sending it.
                with self.assertRaises(ResolveRefused):
                    resolve(self.pending, "tw-1", "resend", "IBKR has it", today=date(2026, 10, 16))
                resolve(self.pending, "tw-1", "sent", "IBKR shows it cancelled", today=date(2026, 10, 16))
                self.assertEqual(self.pending.handovers(), {"tw-1": "sent"})
                goes_on = self.relay(Restarts(log, decisions), broker=broker, mapper=refused)
                goes_on.exchange(envelope(1))
                goes_on.close()
                self.assertEqual(self.sent, [])

    def test_never_sent_is_refused_for_any_order_but_the_one_the_relay_is_refusing(self):
        log = {}
        decisions = {"session:0": [released("tw-1"), released("tw-2")]}

        def refused(order, session):
            if order["client_order_id"] == "tw-2":
                raise ValueError("no IBKR contract for {}".format(order["instrument_id"]))
            return to_request(order, session)

        stuck = self.relay(Restarts(log, decisions), mapper=refused)
        with self.assertRaises(ValueError):
            stuck.exchange(envelope(0))
        stuck.close()
        self.assertEqual([r.client_order_id for r in self.sent], ["tw-1"])
        # tw-1 went out and is marked; tw-3 is a typo the relay never refused.
        for cid, named in (("tw-1", "sent"), ("tw-3", "tw-2")):
            with self.subTest(cid=cid), self.assertRaises(ResolveRefused) as raised:
                resolve(self.pending, cid, "never-sent", "a typo", today=date(2026, 10, 16))
            self.assertIn(named, str(raised.exception))
        self.assertEqual(self.pending.handovers(), {"tw-1": "sent"})
        self.assertEqual(self.pending.decisions(), [])
        resolve(self.pending, "tw-2", "never-sent", "no contract configured", today=date(2026, 10, 16))
        self.assertEqual(self.pending.handovers(), {"tw-1": "sent", "tw-2": "never-sent"})

    def test_never_sent_is_refused_when_the_relay_recorded_no_refusal_for_the_waiting_input(self):
        # An input awaits its reply because the engine did not answer, not
        # because the sender refused an order: nothing is refused, so nothing
        # can be settled as never sent.
        log = {}
        decisions = {"session:0": [released("tw-1")]}
        down = self.relay(Restarts(log, decisions, crash_before_reply=["session:0"]), reconnects=0)
        with self.assertRaises(EngineUnavailable):
            down.exchange(envelope(0))
        down.close()
        with self.assertRaises(ResolveRefused):
            resolve(self.pending, "tw-1", "never-sent", "guessing", today=date(2026, 10, 16))
        # A refusal recorded for another input does not carry over.
        with open(self.pending.refused_path, "w", encoding="utf-8") as f:
            json.dump({"input_id": "session:9", "client_order_id": "tw-1", "reason": "stale"}, f)
        with self.assertRaises(ResolveRefused):
            resolve(self.pending, "tw-1", "never-sent", "guessing", today=date(2026, 10, 16))
        for bad in ("{not json", "[]", json.dumps({"input_id": "session:0", "client_order_id": "tw-1"})):
            with self.subTest(bad=bad):
                with open(self.pending.refused_path, "w", encoding="utf-8") as f:
                    f.write(bad)
                with self.assertRaises(HandoverRecordInvalid):
                    resolve(self.pending, "tw-1", "never-sent", "guessing", today=date(2026, 10, 16))
        self.assertEqual(self.pending.handovers(), {})

    def test_never_sent_is_refused_for_an_order_with_a_mark_or_with_no_input_awaiting_its_reply(self):
        today = date(2026, 10, 16)
        with self.assertRaises(ResolveRefused):
            resolve(self.pending, "tw-1", "never-sent", "nothing is pending", today=today)
        decisions = {"session:0": [released("tw-1")]}
        self.stopped_mid_send(envelope(0), decisions)
        # Marked about to be sent: it may have reached IBKR, so it is settled
        # only as sent or resent, after a check at IBKR.
        with self.assertRaises(ResolveRefused):
            resolve(self.pending, "tw-1", "never-sent", "IBKR shows nothing", today=today)
        resolve(self.pending, "tw-1", "sent", "IBKR shows it cancelled", today=today)
        with self.assertRaises(ResolveRefused):
            resolve(self.pending, "tw-1", "never-sent", "too late", today=today)
        self.assertEqual(self.pending.handovers(), {"tw-1": "sent"})
        self.assertEqual([d["decision"] for d in self.pending.decisions()], ["sent"])

    def test_the_resolve_command_settles_a_never_sent_order(self):
        log = {}
        decisions = {"session:0": [released("tw-1")]}

        def refused(order, session):
            raise ValueError("no contract ID")

        stuck = self.relay(Restarts(log, decisions), mapper=refused)
        with self.assertRaises(ValueError):
            stuck.exchange(envelope(0))
        stuck.close()
        path = self.pending.path
        self.assertEqual(main(["resolve", path, "tw-1", "--never-sent", "--reason", "no IBKR contract for SPY"],
                              today=date(2026, 10, 16)), 0)
        self.assertEqual(self.pending.handovers(), {"tw-1": "never-sent"})

    def test_a_sender_without_a_check_is_refused_before_anything_is_held(self):
        # The relay itself asks the sender's check before every mark, so a
        # sender that offers none is refused rather than trusted.
        class NoCheck:
            def send(self, request):
                raise AssertionError("never called")

        for sender in (self.sent.append, NoCheck()):
            with self.subTest(sender=sender), self.assertRaises(TypeError):
                Relay(connect=Restarts({}, {}), sender=sender, pending=self.pending, broker_orders=holding(),
                      to_request=to_request)
        # The pending file was never held, so a well-formed relay starts.
        self.relay(Restarts({}, {}))

    def test_the_relay_asks_the_senders_check_before_the_mark_whatever_the_mapper(self):
        log = {}
        decisions = {"session:0": [released("tw-1")]}

        def refuse(request):
            raise ValueError("the link is not reconciled")

        relay = self.relay(Restarts(log, decisions), send=Sending(self.sent.append, check=refuse))
        with self.assertRaises(ValueError):
            relay.exchange(envelope(0))
        self.assertEqual(self.pending.handovers(), {}, "a request the sender refuses is never marked")
        self.assertEqual(self.sent, [])

    def test_a_correction_of_an_execution_never_journalled_as_a_fill_is_not_acknowledged(self):
        # The engine keeps the original fill only if it was recorded; a
        # correction of one never recorded (its commission had not arrived)
        # leaves the shares in no fill, so it is reconciled by hand instead.
        with self.assertRaises(ResolveRefused) as raised:
            acknowledge_correction(self.pending, "e.1.02", "e.1.01", "IBKR corrected the price", fills={"e.2.01"})
        message = str(raised.exception)
        self.assertIn("e.1.01", message)
        self.assertIn("never recorded", message)
        self.assertIn("reconcil", message)
        self.assertEqual(self.pending.acknowledged_corrections(), {})

    def test_an_acknowledged_correction_is_kept_and_read_back_exactly(self):
        self.assertEqual(self.pending.acknowledged_corrections(), {})
        acknowledge_correction(self.pending, "e.1.02", "e.1.01", "IBKR corrected the price", fills=FILLS)
        acknowledge_correction(self.pending, "e.1.02", "e.1.01", "the same, again", fills=FILLS)
        self.assertEqual(self.pending.acknowledged_corrections(), {"e.1.02": "e.1.01"})
        for exec_id, corrects, reason in (("e.1.02", "e.1.01", " "), ("e.1.01", "e.1.01", "itself"),
                                          ("e.2.02", "e.1.01", "another base"), ("nodots", "e.1.01", "no base"),
                                          ("e.1.02", "e.1.00", "another original")):
            with self.subTest(exec_id=exec_id, corrects=corrects), self.assertRaises(ResolveRefused):
                acknowledge_correction(self.pending, exec_id, corrects, reason, fills=FILLS | {corrects})
        self.assertEqual(self.pending.acknowledged_corrections(), {"e.1.02": "e.1.01"})
        running = self.relay(Restarts({}, {}))
        with self.assertRaises(ResolveRefused):
            acknowledge_correction(self.pending, "e.1.03", "e.1.01", "while the adapter runs", fills=FILLS)
        running.close()

    def test_a_corrections_file_not_as_acknowledge_correction_writes_it_is_refused(self):
        good = {"exec_id": "e.1.02", "corrects": "e.1.01", "reason": "r", "at": "2026-10-17T14:00:00+00:00"}
        for lines in (["{not json"], ["[]"], [json.dumps(dict(good, extra="x"))], [json.dumps(dict(good, reason=""))],
                      [json.dumps(dict(good, corrects=7))],
                      [json.dumps(good), json.dumps(dict(good, corrects="e.1.00"))]):
            with self.subTest(lines=lines):
                with open(self.pending.corrections_path, "w", encoding="utf-8") as f:
                    f.write("\n".join(lines) + "\n")
                with self.assertRaises(HandoverRecordInvalid):
                    self.pending.acknowledged_corrections()

    def test_the_acknowledge_correction_command_refuses_with_a_nonzero_exit(self):
        path = self.pending.path
        journal = write_engine_journal(Path(self.dir.name) / "journal.jsonl", ["e.1.01"])
        ack = ["acknowledge-correction", path, "--journal", journal]
        self.assertEqual(main(ack + ["e.2.02", "--corrects", "e.1.01", "--reason", "x"]), 1)
        self.assertEqual(main(ack + ["e.3.02", "--corrects", "e.3.01", "--reason", "never journalled"]), 1)
        self.assertEqual(main(["acknowledge-correction", path, "--journal", str(Path(self.dir.name) / "missing.jsonl"),
                               "e.1.02", "--corrects", "e.1.01", "--reason", "x"]), 1)
        self.assertEqual(self.pending.acknowledged_corrections(), {})
        self.assertEqual(main(ack + ["e.1.02", "--corrects", "e.1.01", "--reason", "x"]), 0)
        self.assertEqual(self.pending.acknowledged_corrections(), {"e.1.02": "e.1.01"})
        with open(self.pending.corrections_path, "w", encoding="utf-8") as f:
            f.write("[]\n")
        self.assertEqual(main(ack + ["e.1.03", "--corrects", "e.1.01", "--reason", "x"]), 1)

    def test_the_engines_log_names_each_order_it_recorded_ended(self):
        journal = Path(self.dir.name) / "log.jsonl"
        write_ended_journal(journal, ["e.1.01"], [("tw-1", "submitted"), ("tw-1", "canceled"), ("tw-2", "invalid"),
                                                  ("tw-3", "submitted")])
        reports = journalled_reports(str(journal))
        self.assertEqual((reports.fills, reports.ended), ({"e.1.01"}, {"tw-1", "tw-2"}))
        good = journal.read_text(encoding="utf-8")
        uncommitted = "".join(log_group(8, [("input", "execution.order.lifecycle", 2,
                                             lifecycle_payload("tw-3", "canceled"))], committed=False))
        journal.write_text(good + uncommitted, encoding="utf-8")
        self.assertEqual(journalled_reports(str(journal)).ended, {"tw-1", "tw-2"}, "a group without its commit")
        for name, payload, version in (("no client order ID", dict(lifecycle_payload("tw-3", "canceled"),
                                                                   client_order_id=""), 2),
                                       ("no status", {k: v for k, v in lifecycle_payload("tw-3", "canceled").items()
                                                      if k != "status"}, 2),
                                       ("another schema", lifecycle_payload("tw-3", "canceled"), 1)):
            with self.subTest(name):
                journal.write_text(good + "".join(log_group(8, [("input", "execution.order.lifecycle", version,
                                                                 payload)])), encoding="utf-8")
                with self.assertRaises(ResolveRefused):
                    journalled_reports(str(journal))

    def test_a_trade_after_an_order_ended_is_acknowledged_only_as_the_engines_log_records_it(self):
        ended, fills = frozenset({"tw-1"}), frozenset({"e.1.01"})
        refusals = (("e.5.01", "tw-2", "an order the engine never recorded ended", "never recorded"),
                    ("e.1.01", "tw-1", "a trade the engine holds as a fill", "is recorded as a fill"),
                    ("e.5.01", "tw-1", " ", "reason"),
                    ("nodots", "tw-1", "no base", "base"),
                    ("e.5.01", "", "no order", "order"))
        for exec_id, after, reason, why in refusals:
            with self.subTest(why), self.assertRaises(ResolveRefused) as raised:
                acknowledge_late_fill(self.pending, exec_id, after, reason, fills=fills, ended=ended)
            self.assertIn(why, str(raised.exception))
        self.assertEqual(self.pending.acknowledged_late_fills(), {})
        acknowledge_late_fill(self.pending, "e.5.01", "tw-1", "IBKR shows 4 SPY bought after the cancel",
                              fills=fills, ended=ended)
        acknowledge_late_fill(self.pending, "e.5.01", "tw-1", "the same, again", fills=fills, ended=ended)
        self.assertEqual(self.pending.acknowledged_late_fills(), {"e.5.01": "tw-1"})
        with self.assertRaises(ResolveRefused):
            acknowledge_late_fill(self.pending, "e.5.01", "tw-9", "another order", fills=fills, ended=ended | {"tw-9"})
        running = self.relay(Restarts({}, {}))
        with self.assertRaises(ResolveRefused):
            acknowledge_late_fill(self.pending, "e.6.01", "tw-1", "while the adapter runs", fills=fills, ended=ended)
        running.close()
        self.assertEqual(self.pending.acknowledged_late_fills(), {"e.5.01": "tw-1"})

    def test_a_late_fills_file_not_as_acknowledge_late_fill_writes_it_is_refused(self):
        good = {"exec_id": "e.5.01", "after": "tw-1", "reason": "r", "at": "2026-10-17T14:00:00+00:00"}
        for lines in (["{not json"], ["[]"], [json.dumps(dict(good, extra="x"))], [json.dumps(dict(good, reason=""))],
                      [json.dumps(dict(good, after=7))], [json.dumps(dict(good, exec_id="nodots"))],
                      [json.dumps(good), json.dumps(dict(good, after="tw-2"))]):
            with self.subTest(lines=lines):
                with open(self.pending.late_fills_path, "w", encoding="utf-8") as f:
                    f.write("\n".join(lines) + "\n")
                with self.assertRaises(HandoverRecordInvalid):
                    self.pending.acknowledged_late_fills()

    def test_the_acknowledge_late_fill_command_refuses_with_a_nonzero_exit(self):
        path = self.pending.path
        journal = write_ended_journal(Path(self.dir.name) / "journal.jsonl", ["e.1.01"], [("tw-1", "canceled")])
        ack = ["acknowledge-late-fill", path, "--journal", journal]
        self.assertEqual(main(ack + ["e.5.01", "--after", "tw-2", "--reason", "x"]), 1)
        self.assertEqual(main(ack + ["e.1.01", "--after", "tw-1", "--reason", "x"]), 1)
        self.assertEqual(main(["acknowledge-late-fill", path, "--journal", str(Path(self.dir.name) / "missing.jsonl"),
                               "e.5.01", "--after", "tw-1", "--reason", "x"]), 1)
        self.assertEqual(self.pending.acknowledged_late_fills(), {})
        self.assertEqual(main(ack + ["e.5.01", "--after", "tw-1", "--reason", "x"]), 0)
        self.assertEqual(self.pending.acknowledged_late_fills(), {"e.5.01": "tw-1"})

    def test_the_never_sent_at_broker_stop_names_a_resolve_command_that_runs(self):
        message = str(NeverSentAtBroker("tw-1"))
        self.assertIn("relay.py resolve PENDING_FILE tw-1 --sent --reason", message)

    def test_the_fills_are_read_from_the_engines_durable_log_as_its_own_writer_makes_it(self):
        # testdata/engine_log.jsonl was written by internal/journal's
        # LogWriter (internal/journal pins it with RecoverLog): one committed
        # fill, and a torn tail holding a fill record not yet committed,
        # which is no proof the engine recorded that fill.
        self.assertEqual(journalled_fills(str(TESTDATA / "engine_log.jsonl")), {"0001f4e8.paper.01.01"})

    def test_only_committed_fills_count_and_a_line_still_being_written_is_not_read(self):
        journal = Path(write_engine_journal(Path(self.dir.name) / "log.jsonl", ["e.1.01", "e.2.01"]))
        self.assertEqual(journalled_fills(str(journal)), {"e.1.01", "e.2.01"})
        good = journal.read_text(encoding="utf-8")
        uncommitted = "".join(log_group(5, [("input", "execution.fill", 5, fill_payload("e.3.01"))], committed=False))
        journal.write_text(good + uncommitted, encoding="utf-8")
        self.assertEqual(journalled_fills(str(journal)), {"e.1.01", "e.2.01"}, "a group without its commit")
        journal.write_text(good + uncommitted[:40], encoding="utf-8")
        self.assertEqual(journalled_fills(str(journal)), {"e.1.01", "e.2.01"}, "a line the engine is writing")

    def test_a_durable_log_not_as_the_engine_writes_it_is_reported_unreadable_never_as_no_fills(self):
        journal = Path(self.dir.name) / "log.jsonl"
        write_engine_journal(journal, ["e.1.01"])
        good = journal.read_text(encoding="utf-8").splitlines(keepends=True)
        header, groups = good[0], good[1:]

        def fill(payload, version=5, kind="input", sequence=5, committed=True):
            return "".join(log_group(sequence, [(kind, "execution.fill", version, payload)], committed=committed))

        partial = {"fill_id": "e.1.01"}
        record = json.loads(groups[-2])
        bad_hash = json.dumps(dict(record, record=dict(record["record"], envelope=dict(
            record["record"]["envelope"], payload_hash="0" * 64))), separators=(",", ":")) + "\n"
        cases = {
            "empty": "",
            "a header only being written": header[:-1],
            "an end-of-run journal": json.dumps({"journal_version": 1, "chain_algorithm": LOG_CHAIN}) + "\n",
            "another log version": header.replace('"log_version":1', '"log_version":2'),
            "a header field missing": header.replace('"strategy_version"', '"strategy"'),
            "not JSON": header + "{not json\n",
            "a bare object": header + "{}\n",
            "a record and a commit": header + json.dumps({"record": {}, "commit": {}}) + "\n",
            "a sequence gap": header + "".join(groups) + fill(fill_payload("e.2.01"), sequence=6),
            "a decision first": header + fill(fill_payload("e.2.01"), kind="decision", sequence=1),
            "a commit naming another record": header + "".join(groups[:-1]) + json.dumps(
                {"commit": {"sequence": 3, "record_hash": "sha256:" + "f" * 64}}) + "\n",
            "a commit with nothing to commit": header + "".join(groups) + groups[-1],
            "a fill with only a fill ID": header + "".join(groups) + fill(partial),
            "a fill of another schema": header + "".join(groups) + fill(fill_payload("e.2.01"), version=4),
            "a fill of no shares": header + "".join(groups) + fill(dict(fill_payload("e.2.01"), quantity=0)),
            "a fill at no price": header + "".join(groups) + fill(dict(fill_payload("e.2.01"), price=0)),
            "a fill with a negative commission": header + "".join(groups) + fill(
                dict(fill_payload("e.2.01"), commission=-1)),
            "a fill of no side": header + "".join(groups) + fill(dict(fill_payload("e.2.01"), side="short")),
            "a fill with an extra field": header + "".join(groups) + fill(dict(fill_payload("e.2.01"), extra=1)),
            "a fill time with no zone": header + "".join(groups) + fill(
                dict(fill_payload("e.2.01"), filled_at="2026-10-17T13:30:01")),
            "a fill decided, not input": header + groups[0][:0] + "".join(log_group(1, [
                ("input", "market.session.closed", 1, {}), ("decision", "execution.fill", 5,
                                                            fill_payload("e.2.01"))])),
            "a payload that does not match its hash": header + "".join(groups[:-2]) + bad_hash + groups[-1],
        }
        for name, text in cases.items():
            with self.subTest(name):
                journal.write_text(text, encoding="utf-8")
                with self.assertRaises(ResolveRefused) as raised:
                    journalled_fills(str(journal))
                self.assertIn("durable log", str(raised.exception))
        with self.assertRaises(ResolveRefused):
            journalled_fills(str(journal) + ".missing")

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
