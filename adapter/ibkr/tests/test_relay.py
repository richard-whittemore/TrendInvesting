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
import os
import sys
import tempfile
import threading
import unittest
from unittest import mock
from datetime import date, datetime, timedelta, timezone
from decimal import Decimal
from pathlib import Path

tests_dir = str(Path(__file__).resolve().parent)
if tests_dir not in sys.path:
    sys.path.insert(0, tests_dir)

from orders import BrokerOrders
from relay import (MAX_CLOSE_TIMEOUT, OUTBOX_SUFFIX, log_chain_next, log_chain_seed, EngineUnavailable, HandoverRecordInvalid, HandoverUnconfirmed, InputChanged,
                   NeverSentAtBroker, OrderUnsendable, Outbox, PendingInput, PendingInUse, Relay, RelayCloseTimeout, ResendExpired, ResolveRefused,
                   acknowledge_correction, acknowledge_late_fill, earlier_run, journalled_fills, journalled_reports,
                   main, resolve, resolve_run_ended, retire, run_chain, run_identity, run_logs)
import relay as relay_module
from sender import OrderRequest, SendRefused, SessionOver, UnsendableOrder

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
        self.closed = False

    def close(self):
        self.closed = True

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


class NotHandedOver(HandoverUnconfirmed):
    """A send that knows nothing reached the broker, as the real client's socket closed before placeOrder."""

    handed_over = False

    def __init__(self, client_order_id):
        Exception.__init__(self, "socket closed before placeOrder: {}".format(client_order_id))
        self.client_order_id = client_order_id


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
# The session the run-chain tests are read against (New York date).
TODAY = date(2026, 10, 17)


def log_header():
    return json.dumps({"log_version": 1, "chain_algorithm": LOG_CHAIN, "configuration_hash": LOG_CONFIGURATION,
                       "strategy_version": LOG_STRATEGY}, separators=(",", ":")) + "\n"


def fill_payload(fill_id, account_id="DU1234567"):
    return {"account_id": account_id, "client_order_id": "tw-1", "fill_id": fill_id, "instrument_id": "SPY",
            "side": "buy", "quantity": 4, "price": 401.25, "commission": 1.0, "filled_at": "2026-10-17T13:30:01Z"}


def log_group(sequence, entries, committed=True, at="2026-10-17T13:30:00Z"):
    """One group of the engine's durable log from sequence on: (kind, type, schema version, payload) lines, then its commit.

    The record hashes are stand-ins: the relay reads the log's shape and each
    payload against its hash; chained gives a log the real chain, for the
    readers that verify it (testdata/engine_log.jsonl is a log the engine's
    own writer made).
    """
    lines = []
    for n, (kind, event_type, schema_version, payload) in enumerate(entries, sequence):
        raw = json.dumps(payload, separators=(",", ":"))
        envelope = json.dumps({"id": "e:{}".format(n), "type": event_type, "envelope_version": 1,
                               "schema_version": schema_version, "event_time": at,
                               "recorded_at": at, "sequence": n, "source": "ibkr-adapter",
                               "strategy_version": LOG_STRATEGY, "configuration_hash": LOG_CONFIGURATION,
                               "payload_hash": hashlib.sha256(raw.encode()).hexdigest()}, separators=(",", ":"))
        record_hash = "sha256:" + hashlib.sha256("record {}".format(n).encode()).hexdigest()
        lines.append('{{"record":{{"sequence":{},"kind":"{}","envelope":{},"payload":{}}},"record_hash":"{}"}}}}\n'
                     .format(n, kind, envelope[:-1], raw, record_hash))
    if committed:
        lines.append(json.dumps({"commit": {"sequence": sequence + len(entries) - 1, "record_hash": record_hash}},
                                separators=(",", ":")) + "\n")
    return lines


def chained(lines):
    """The log lines joined, each record's stand-in hash replaced by the one LOG_CHAIN_ALGORITHM gives, commits too.

    For a reader that verifies the record hash chain (relay.run_chain, ADR
    0027); a line that is not a record as log_group writes one is kept as it
    is, and the chain past it is left unverifiable, as a tampered log is.
    """
    text = "".join(lines)
    out = text.splitlines(keepends=True)
    if not out:
        return text
    try:
        chain = log_chain_seed(json.loads(out[0]))
    except (ValueError, KeyError, TypeError):
        return text
    real = {}
    for line in out[1:]:
        try:
            record = json.loads(line)["record"]
        except (ValueError, KeyError, TypeError):
            continue
        payload = json.dumps(record["envelope"]["payload"], separators=(",", ":")).encode("utf-8")
        chain = log_chain_next(chain, record["kind"], record["envelope"], payload)
        if chain is None:
            break
        real[record["record_hash"]] = "sha256:" + chain.hex()
    for stand_in, actual in real.items():
        text = text.replace(stand_in, actual)
    return text


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

    def test_close_closes_the_engine_connection_so_the_next_adapter_can_connect(self):
        # The engine takes one connection at a time (cmd/engine), so an
        # adapter that stops closes its own before another starts.
        connect = Restarts({}, {})
        relay = self.relay(connect)
        relay.exchange(envelope(0))
        self.assertFalse(connect.engines[0].closed)
        relay.close()
        self.assertTrue(connect.engines[0].closed)

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

    def stopped_not_handed_over(self, log, decisions):
        """The sender's send says nothing reached the broker (IB Gateway's socket closed before placeOrder)."""
        def closed_before(request):
            raise NotHandedOver(request.client_order_id)

        stopped = self.relay(Restarts(log, decisions), send=closed_before)
        with self.assertRaises(HandoverUnconfirmed):
            stopped.exchange(envelope(0))
        stopped.close()

    def test_a_send_that_says_nothing_was_handed_over_is_recorded_beside_its_mark(self):
        log, decisions = {}, {"session:0": [released("tw-1")]}
        self.stopped_not_handed_over(log, decisions)
        self.assertEqual(self.pending.handovers(), {"tw-1": "handing-over"})
        self.assertEqual(self.pending.not_handed_over(), {"client_order_id": "tw-1", "input_id": "session:0",
                                                          "reason": "socket closed before placeOrder: tw-1"})

    def test_a_send_that_cannot_say_whether_it_handed_over_records_nothing_and_clears_an_earlier_record(self):
        # An earlier attempt's "before" stays true only until the order is
        # handed over again: a later attempt that may have reached IBKR must
        # not leave it behind for resolve to read.
        log, decisions = {}, {"session:0": [released("tw-1")]}
        self.stopped_not_handed_over(log, decisions)
        self.pending.mark("tw-1", "resend:2026-10-16")

        def closed_during(request):
            raise HandoverUnconfirmed(request.client_order_id)

        during = self.relay(Restarts(log, decisions), send=closed_during)
        with self.assertRaises(HandoverUnconfirmed):
            during.exchange(envelope(0))
        during.close()
        self.assertEqual(self.pending.handovers(), {"tw-1": "handing-over"})
        self.assertIsNone(self.pending.not_handed_over())
        with self.assertRaises(ResolveRefused):
            resolve(self.pending, "tw-1", "not-handed-over", "IBKR shows nothing", today=date(2026, 10, 17))

    def test_an_order_not_handed_over_is_settled_on_a_later_day_and_the_relay_goes_on(self):
        log, decisions = {}, {"session:0": [released("tw-1")]}
        self.stopped_not_handed_over(log, decisions)
        later = date(2026, 10, 17)
        with self.assertRaises(ResolveRefused):
            resolve(self.pending, "tw-1", "resend", "a day late", today=later)
        self.assertEqual(main(["resolve", self.pending.path, "tw-1", "--not-handed-over", "--reason",
                               "IBKR's order history shows no order tagged tw-1"], today=later), 0)
        self.assertEqual(self.pending.handovers(), {"tw-1": "never-sent"})
        [recorded] = self.pending.decisions()
        self.assertEqual((recorded["decision"], recorded["session_date"]), ("not-handed-over", "2026-10-17"))
        goes_on = self.relay(Restarts(log, decisions), today=later)
        goes_on.exchange(envelope(1))
        goes_on.close()
        self.assertEqual(self.sent, [], "an order never handed over is not sent")
        self.assertIsNone(self.pending.load())
        self.assertIsNone(self.pending.not_handed_over(), "the record goes with its input")

    def test_not_handed_over_is_refused_without_the_relays_record_for_that_order_and_input(self):
        today = date(2026, 10, 17)
        with self.assertRaises(ResolveRefused):
            resolve(self.pending, "tw-1", "not-handed-over", "nothing is pending", today=today)
        decisions = {"session:0": [released("tw-1")]}
        # Stopped mid-send: it may be at IBKR, and the relay recorded nothing.
        self.stopped_mid_send(envelope(0), decisions)
        with self.assertRaises(ResolveRefused) as raised:
            resolve(self.pending, "tw-1", "not-handed-over", "IBKR shows nothing", today=today)
        self.assertIn("--sent", str(raised.exception))
        # Only an order marked about to be sent can have been not handed over.
        with self.assertRaises(ResolveRefused) as raised:
            resolve(self.pending, "tw-9", "not-handed-over", "a typo", today=today)
        self.assertIn("'handing-over'", str(raised.exception))
        for entry in ({"input_id": "session:9", "client_order_id": "tw-1", "reason": "before"},
                      {"input_id": "session:0", "client_order_id": "tw-2", "reason": "before"}):
            with self.subTest(entry=entry):
                with open(self.pending.not_handed_over_path, "w", encoding="utf-8") as f:
                    json.dump(entry, f)
                with self.assertRaises(ResolveRefused):
                    resolve(self.pending, "tw-1", "not-handed-over", "IBKR shows nothing", today=today)
        for bad in ("{not json", "[]", json.dumps({"input_id": "session:0", "client_order_id": "tw-1"})):
            with self.subTest(bad=bad):
                with open(self.pending.not_handed_over_path, "w", encoding="utf-8") as f:
                    f.write(bad)
                with self.assertRaises(HandoverRecordInvalid):
                    resolve(self.pending, "tw-1", "not-handed-over", "IBKR shows nothing", today=today)
        self.assertEqual(self.pending.handovers(), {"tw-1": "handing-over"})
        self.assertEqual(self.pending.decisions(), [])

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
        refusals = (("e.5.01", "tw-2", "an order the engine never recorded ended", "neither in the engine"),
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

    def test_a_trade_after_the_cancellation_awaiting_its_reply_is_acknowledged(self):
        # The adapter journalled order tw-2's cancellation and stopped before
        # the engine committed it, so it is the input awaiting its reply and
        # not yet in the engine's log; the adapter's journal holds the order
        # ended, so a late trade on it stops every start, and the command
        # must clear that stop rather than refuse it.
        ended, fills = frozenset({"tw-1"}), frozenset()

        def waiting(client_order_id, status, event_type="execution.order.lifecycle", schema_version=2):
            self.pending.save({"id": "lifecycle:{}".format(client_order_id), "type": event_type,
                               "schema_version": schema_version,
                               "payload": lifecycle_payload(client_order_id, status)})

        for name, args in (("a working order's status", ("tw-3", "submitted")),
                           ("another type", ("tw-3", "canceled", "market.session.closed")),
                           ("another schema", ("tw-3", "canceled", "execution.order.lifecycle", 1))):
            with self.subTest(name), self.assertRaises(ResolveRefused) as raised:
                waiting(*args)
                acknowledge_late_fill(self.pending, "e.7.01", "tw-3", "IBKR shows a trade", fills=fills, ended=ended)
            self.assertIn("awaiting its reply", str(raised.exception))
            self.assertIn("docs/runbooks/reconciliation.md", str(raised.exception))
        waiting("tw-2", "canceled")
        acknowledge_late_fill(self.pending, "e.5.01", "tw-2", "IBKR shows 4 SPY bought after the cancel",
                              fills=fills, ended=ended)
        self.assertEqual(self.pending.acknowledged_late_fills(), {"e.5.01": "tw-2"})
        journal = write_ended_journal(Path(self.dir.name) / "journal.jsonl", [], [("tw-1", "canceled")])
        self.assertEqual(main(["acknowledge-late-fill", self.pending.path, "--journal", journal, "e.6.01",
                               "--after", "tw-2", "--reason", "x"]), 0)
        self.assertEqual(self.pending.acknowledged_late_fills(), {"e.5.01": "tw-2", "e.6.01": "tw-2"})

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


# When the relay retires an order itself, and when the owner does.
RETIRED_AT = datetime(2026, 10, 17, 13, 30, 0, tzinfo=timezone.utc)


def session_over(request):
    raise SessionOver("order {} was released for the session of 2026-10-16, not today's (2026-10-17); its "
                      "session is over, so it is never sent".format(request.client_order_id))


def no_contract(order, session):
    raise UnsendableOrder("no IBKR contract ID is configured for {!r}".format(order["instrument_id"]))


class RetireTests(unittest.TestCase):
    """The owner's decision of 2026-10-22: an order IBKR can never accept is retired, never left to wedge the run."""

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.pending = PendingInput(str(Path(self.dir.name) / "pending.json"))
        self.outbox = Outbox(self.pending.path + OUTBOX_SUFFIX)
        self.sent = []

    def relay(self, connect, broker=holding(), check=None, mapper=to_request, **options):
        relay = Relay(connect=connect, sender=Sending(self.sent.append, check=check), pending=self.pending,
                      broker_orders=broker, to_request=mapper, today=lambda: date(2026, 10, 17),
                      now=lambda: RETIRED_AT, **options)
        self.addCleanup(relay.close)
        return relay

    def retired_entries(self):
        return [e for e in self.outbox.load() if e["event_type"] == "execution.order.retired"]

    def test_an_order_whose_session_is_over_is_retired_by_the_relay_which_goes_on(self):
        # Refused before any mark for its session being over, the order is
        # retired without the owner: journalled for the engine as a refusal
        # for "session over" by the adapter, never a cancel of an order at
        # IBKR, settled as never sent, and the relay goes on to its next
        # order and its next input.
        log = {}
        decisions = {"session:0": [released("tw-1"), released("tw-2")]}

        def tw1_over(request):
            if request.client_order_id == "tw-1":
                session_over(request)

        relay = self.relay(Restarts(log, decisions), check=tw1_over)
        relay.exchange(envelope(0))
        self.assertEqual([r.client_order_id for r in self.sent], ["tw-2"], "the next order still goes out")
        self.assertIsNone(self.pending.load(), "the relay went on past the input")
        [entry] = self.retired_entries()
        self.assertEqual(entry, {
            "id": "order-retired:tw-1", "event_type": "execution.order.retired", "schema_version": 1,
            "event_time": "2026-10-17T13:30:00Z",
            "payload": {"account_id": "DU1234567", "client_order_id": "tw-1", "instrument_id": "SPY", "side": "buy",
                        "quantity": 9, "limit_price": 401.5, "reason": "session over",
                        "detail": "order tw-1 was released for the session of 2026-10-16, not today's (2026-10-17); "
                                  "its session is over, so it is never sent",
                        "retired_by": "adapter", "retired_at": "2026-10-17T13:30:00Z"}})
        [decision] = self.pending.decisions()
        self.assertEqual((decision["client_order_id"], decision["decision"], decision["retired_by"]),
                         ("tw-1", "retired", "adapter"))
        self.assertIn("tw-1", self.pending.settled_orders().ended)

    def test_a_retired_order_is_never_sent_when_its_input_is_asked_for_again(self):
        # The retirement is recorded for the engine first and the order
        # marked never sent last. A stop before the mark retires it again on
        # restart without a second record; a stop after it, before the input
        # is cleared, asks for the input's reply again and passes over the
        # order marked never sent. Either way it is never sent.
        for stop in ("mark", "clear"):
            with self.subTest(stop=stop):
                self.pending.clear()
                Path(self.outbox.path).unlink(missing_ok=True)
                Path(self.pending.decisions_path).unlink(missing_ok=True)
                log = {}
                decisions = {"session:0": [released("tw-1")]}
                first = self.relay(Restarts(log, decisions), check=session_over)
                real = getattr(self.pending, stop)

                def die(*args, real=real, stop=stop):
                    if stop == "clear" or args[1:] == ("never-sent",):
                        raise OSError("stopped at the " + stop)
                    return real(*args)

                setattr(self.pending, stop, die)
                with self.assertRaises(OSError):
                    first.exchange(envelope(0))
                first.close()
                setattr(self.pending, stop, real)
                self.assertEqual(self.pending.handovers(), {} if stop == "mark" else {"tw-1": "never-sent"})
                again = self.relay(Restarts(log, decisions), check=session_over)
                again.exchange(envelope(1))
                again.close()
                self.assertEqual(len(self.retired_entries()), 1, "the retirement is journalled once")
                self.assertIsNone(self.pending.load())
                self.assertEqual(self.sent, [])

    def test_an_earlier_inputs_retirement_never_stops_a_later_order_under_the_same_id(self):
        # A retirement recorded among the decisions for another input, an
        # earlier run's under a reused client order ID, is not this input's:
        # the order released now is judged and sent as any other, never
        # marked never sent with nothing told to the engine (fail closed,
        # AGENTS.md). So too when an unmarked retirement is followed by one
        # recorded for another input, which released the order again later.
        legacy = {"client_order_id": "tw-1", "decision": "retired", "reason": "session over", "retired": "session over",
                  "retired_by": "adapter", "at": "2026-10-16T13:30:00Z", "session_date": "2026-10-16"}
        for earlier in ([dict(legacy, input_id="session:old")], [legacy, dict(legacy, input_id="session:old")]):
            with self.subTest(earlier=earlier):
                self.pending.clear()
                Path(self.pending.decisions_path).unlink(missing_ok=True)
                del self.sent[:]
                for d in earlier:
                    self.pending.record_decision(d)
                relay = self.relay(Restarts({}, {"session:0": [released("tw-1")]}))
                relay.exchange(envelope(0))
                relay.close()
                self.assertEqual([r.client_order_id for r in self.sent], ["tw-1"])
                self.assertEqual(self.retired_entries(), [])

    def test_a_retirement_recorded_with_no_input_is_this_orders(self):
        # A retirement recorded with no input_id (by a relay before the field,
        # or edited by hand) that no later input's record follows is unknown,
        # not another input's: it is read as this order's, so the order is
        # never sent (fail closed on missing, AGENTS.md).
        for missing in ({}, {"input_id": None}, {"input_id": ""}):
            with self.subTest(missing=missing):
                self.pending.clear()
                Path(self.pending.decisions_path).unlink(missing_ok=True)
                del self.sent[:]
                self.pending.record_decision(dict({"client_order_id": "tw-1", "decision": "retired", "reason": "session over",
                                                   "retired": "session over", "retired_by": "adapter",
                                                   "at": "2026-10-16T13:30:00Z", "session_date": "2026-10-16"}, **missing))
                relay = self.relay(Restarts({}, {"session:0": [released("tw-1")]}))
                relay.exchange(envelope(0))
                relay.close()
                self.assertEqual(self.sent, [])
                self.assertEqual(self.retired_entries(), [])

    def test_an_order_whose_session_is_over_that_ibkr_shows_is_never_retired(self):
        # Checked at IBKR first, as every order is: one IBKR shows under its
        # tag was sent, so it is taken as sent, never retired.
        log = {}
        decisions = {"session:0": [released("tw-1")]}
        relay = self.relay(Restarts(log, decisions), broker=holding(executions=["tw-1"]), check=session_over)
        relay.exchange(envelope(0))
        self.assertEqual(self.retired_entries(), [])
        self.assertEqual(self.sent, [])

    def stuck_on_unsendable(self, log, decisions, mapper=no_contract):
        relay = self.relay(Restarts(log, decisions), mapper=mapper)
        with self.assertRaises(OrderUnsendable) as stopped:
            relay.exchange(envelope(0))
        relay.close()
        return stopped.exception

    def test_an_order_ibkr_can_never_accept_stops_the_relay_for_the_owner(self):
        # Not a limit order, a fraction of a share, or no contract ID: never
        # marked, never sent, and the relay stops at that order every time,
        # naming the retire command; it records the order it is refusing,
        # with the release, for that command.
        log = {}
        decisions = {"session:0": [released("tw-1")]}
        for shape in ("a market order", "a fraction of a share", "no contract ID"):
            with self.subTest(shape=shape):
                def unsendable(order, session, shape=shape):
                    raise UnsendableOrder("order {} is {}".format(order["client_order_id"], shape))

                stopped = self.stuck_on_unsendable(log, decisions, mapper=unsendable)
                self.assertIsInstance(stopped, ValueError)
                self.assertIn("relay.py retire", str(stopped))
                self.assertIn("tw-1", str(stopped))
                refusal = self.pending.refusal()
                self.assertEqual((refusal["client_order_id"], refusal["input_id"], refusal["unsendable"]),
                                 ("tw-1", "session:0", True))
                self.assertEqual(refusal["order"]["client_order_id"], "tw-1")
        self.assertEqual(self.pending.handovers(), {})
        self.assertEqual(self.sent, [])
        self.assertEqual(self.retired_entries(), [])

    def test_the_owners_retire_clears_the_stopped_order_and_the_relay_goes_on(self):
        log = {}
        decisions = {"session:0": [released("tw-1")]}
        self.stuck_on_unsendable(log, decisions)
        retire(self.pending, "tw-1", "richard", "no IBKR contract for SPY configured; added for the next run",
               now=lambda: RETIRED_AT, today=date(2026, 10, 17))
        [entry] = self.retired_entries()
        self.assertEqual(entry["payload"]["reason"], "owner retired")
        self.assertEqual(entry["payload"]["retired_by"], "richard")
        self.assertEqual(entry["payload"]["detail"], "no IBKR contract for SPY configured; added for the next run")
        self.assertEqual(self.pending.handovers(), {"tw-1": "never-sent"}, "marked while its input awaits its reply")
        [decision] = self.pending.decisions()
        self.assertEqual((decision["decision"], decision["retired_by"], decision["session_date"]),
                         ("retired", "richard", "2026-10-17"))
        goes_on = self.relay(Restarts(log, decisions), mapper=no_contract)
        goes_on.exchange(envelope(1))
        self.assertEqual(self.sent, [])
        self.assertIsNone(self.pending.load(), "the relay went on to the next input")

    def test_an_owners_retirement_a_stop_left_unfinished_is_finished_and_never_sent(self):
        # The retire command records the retirement for the engine first and
        # marks the order never sent last. Stopped between the two, and the
        # cause then fixed, the adapter started again finds the retirement
        # begun and finishes it before it asks whether the order can now be
        # sent: the engine is told the order was never sent, so it is never
        # sent (the owner's decision of 2026-10-22; fail closed, AGENTS.md).
        for stop in ("record_decision", "mark"):
            with self.subTest(stop=stop):
                self.pending.clear()
                self.sent.clear()
                for path in (self.outbox.path, self.pending.decisions_path, self.pending.handover_path):
                    Path(path).unlink(missing_ok=True)
                log = {}
                decisions = {"session:0": [released("tw-1")]}
                self.stuck_on_unsendable(log, decisions)
                real = getattr(self.pending, stop)

                def die(*args, **kwargs):
                    raise OSError("stopped at the " + stop)

                setattr(self.pending, stop, die)
                with self.assertRaises(OSError):
                    retire(self.pending, "tw-1", "richard", "no IBKR contract for SPY configured",
                           now=lambda: RETIRED_AT, today=date(2026, 10, 17))
                setattr(self.pending, stop, real)
                self.assertEqual(self.pending.handovers(), {}, "the stop left the order unmarked")
                fixed = self.relay(Restarts(log, decisions), mapper=to_request)
                fixed.exchange(envelope(1))
                fixed.close()
                self.assertEqual(self.sent, [], "an order retired for the engine is never sent")
                self.assertEqual(len(self.retired_entries()), 1, "the retirement is journalled once")
                self.assertEqual([d["decision"] for d in self.pending.decisions()], ["retired"])
                self.assertIsNone(self.pending.load(), "the relay went on to the next input")

    def test_never_sent_is_refused_for_an_order_ibkr_can_never_accept(self):
        # An order of a shape IBKR can never accept is settled only by the
        # retire command, which tells the engine; settling it as never sent
        # would leave the engine holding it as released (the owner's decision
        # of 2026-10-22).
        log = {}
        decisions = {"session:0": [released("tw-1")]}
        self.stuck_on_unsendable(log, decisions)
        with self.assertRaises(ResolveRefused) as refused:
            resolve(self.pending, "tw-1", "never-sent", "no contract configured", today=date(2026, 10, 17))
        self.assertIn("relay.py retire", str(refused.exception))
        self.assertEqual(self.pending.handovers(), {})
        self.assertEqual(self.pending.decisions(), [])

    def test_the_retire_command_refuses_an_order_that_is_not_stopped_as_unsendable(self):
        today = date(2026, 10, 17)
        with self.assertRaises(ResolveRefused):
            retire(self.pending, "tw-1", "richard", "nothing is pending", today=today)
        log = {}
        decisions = {"session:0": [released("tw-1"), released("tw-2")]}

        def tw2_unsendable(order, session):
            if order["client_order_id"] == "tw-2":
                no_contract(order, session)
            return to_request(order, session)

        self.stuck_on_unsendable(log, decisions, mapper=tw2_unsendable)
        for cid, why in (("tw-1", "sent"), ("tw-3", "tw-2")):
            with self.subTest(cid=cid), self.assertRaises(ResolveRefused) as refused:
                retire(self.pending, cid, "richard", "a typo", today=today)
            self.assertIn(why, str(refused.exception))
        for by, reason in (("richard", " "), ("", "no contract"), ("adapter", "no contract")):
            with self.subTest(by=by, reason=reason), self.assertRaises(ResolveRefused):
                retire(self.pending, "tw-2", by, reason, today=today)
        holder = self.relay(Restarts(log, decisions), mapper=tw2_unsendable)
        with self.assertRaises(ResolveRefused) as held:
            retire(self.pending, "tw-2", "richard", "no contract", today=today)
        self.assertIn("stop it first", str(held.exception))
        holder.close()
        self.assertEqual(self.retired_entries(), [])
        self.assertEqual(self.pending.handovers(), {"tw-1": "sent"})
        self.assertEqual([d["decision"] for d in self.pending.decisions()], [])

    def test_the_retire_command_refuses_a_refusal_that_is_not_a_shape_ibkr_can_never_accept(self):
        # A login that does not manage the account, or no order ID yet, is
        # the link's to fix, not the order's: it is not retired.
        log = {}
        decisions = {"session:0": [released("tw-1")]}

        def unmanaged(request):
            raise SendRefused("account 'DU1234567' is not among the accounts this IBKR login manages")

        relay = self.relay(Restarts(log, decisions), check=unmanaged)
        with self.assertRaises(SendRefused):
            relay.exchange(envelope(0))
        relay.close()
        self.assertFalse(self.pending.refusal()["unsendable"])
        with self.assertRaises(ResolveRefused) as refused:
            retire(self.pending, "tw-1", "richard", "guessing", today=date(2026, 10, 17))
        self.assertIn("can never accept", str(refused.exception))
        # A refusal as the relay wrote one before retirement existed holds no
        # shape, so it is not retired either.
        with open(self.pending.refused_path, "w", encoding="utf-8") as f:
            json.dump({"input_id": "session:0", "client_order_id": "tw-1", "reason": "no contract"}, f)
        with self.assertRaises(ResolveRefused):
            retire(self.pending, "tw-1", "richard", "guessing", today=date(2026, 10, 17))
        self.assertEqual(self.retired_entries(), [])

    def test_the_retire_command_refuses_anyone_but_the_owner(self):
        # The owner runs the adapter's commands as the user that owns its
        # files (sudo -u ti-trader on the server); run as any other user, or
        # as root, the command records nothing.
        log = {}
        decisions = {"session:0": [released("tw-1")]}
        self.stuck_on_unsendable(log, decisions)
        owner = os.stat(self.pending.path).st_uid
        for user in (owner + 1, 0):
            with self.subTest(user=user), mock.patch.object(relay_module, "_effective_user", return_value=user):
                with self.assertRaises(ResolveRefused) as refused:
                    retire(self.pending, "tw-1", "richard", "no contract", today=date(2026, 10, 17))
                self.assertIn("owner", str(refused.exception))
        self.assertEqual(self.retired_entries(), [])
        self.assertEqual(self.pending.handovers(), {})

    def test_the_retire_command_line(self):
        log = {}
        decisions = {"session:0": [released("tw-1")]}
        self.stuck_on_unsendable(log, decisions)
        path = self.pending.path
        self.assertEqual(main(["retire", path, "tw-9", "--by", "richard", "--reason", "typo"],
                              today=date(2026, 10, 17)), 1)
        self.assertEqual(main(["retire", path, "tw-1", "--by", "richard", "--reason", "no contract for SPY"],
                              today=date(2026, 10, 17)), 0)
        self.assertEqual(self.pending.handovers(), {"tw-1": "never-sent"})
        self.assertEqual(len(self.retired_entries()), 1)

    def test_a_committed_retirement_ends_its_order_in_the_run_chain(self):
        # A run whose only released order was retired has no order open, so
        # the walk of earlier runs reads past it like a run whose orders
        # ended at IBKR.
        path = Path(self.dir.name) / "engine.log"
        retired = {"account_id": "DU1234567", "client_order_id": "tw-1", "instrument_id": "SPY", "side": "buy",
                   "quantity": 9, "limit_price": 401.5, "reason": "session over", "detail": "over",
                   "retired_by": "adapter", "retired_at": "2026-10-17T13:30:00Z"}
        lines = [log_header()]
        lines += log_group(1, [("input", "market.session.closed", 1, {"session": "2026-10-14"}),
                               ("decision", "risk.order.released", 3, released("tw-1")["payload"] | {"limit_price": 401.5})],
                           at="2026-10-14T20:00:00Z")
        lines += log_group(3, [("input", "execution.order.retired", 1, retired)], at="2026-10-14T21:00:00Z")
        path.write_text("".join(lines))
        self.assertIn("tw-1", journalled_reports(str(path)).ended)


class RunLogsTests(unittest.TestCase):
    """The chain of runs a fresh run's Run approvals name, read strictly."""

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)

    def write(self, name, previous=(), at="2026-10-17T13:30:00Z", final_hash=None):
        """A run's log: one Run approval per name in previous, each with that log's final record hash, else one input."""
        lines = [log_header()]
        lines += log_group(1, [("input", "operator.run.approved", 2, {
            "previous_log": p, "previous_log_final_hash": final_hash or self.final_hash(p)}) for p in previous]
                           or [("input", "market.session.closed", 1, {"session": "2026-10-16"})], at=at)
        path = Path(self.dir.name) / name
        path.write_text(chained(lines), encoding="utf-8")
        return str(path)

    def final_hash(self, name):
        """The record hash of the last commit in the log called name, or a stand-in for one not written."""
        path = Path(self.dir.name) / str(name)
        if not isinstance(name, str) or not name or not path.is_file():
            return "sha256:" + "2" * 64
        commits = [json.loads(line)["commit"] for line in path.read_text(encoding="utf-8").splitlines()
                   if line.startswith('{"commit"')]
        return commits[-1]["record_hash"] if commits else "sha256:" + "2" * 64

    def test_the_chain_runs_from_the_newest_run_back_to_the_first(self):
        first = self.write("a.log")
        second = self.write("b.log", ["a.log"])
        third = self.write("c.log", ["b.log"])
        self.assertEqual(run_logs(third, today=TODAY), [third, second, first])
        self.assertEqual(run_logs(first, today=TODAY), [first])

    def test_every_reader_of_the_chain_refuses_a_log_whose_record_hash_chain_is_broken(self):
        # A record changed in place keeps its log's stored final record hash,
        # so only the recomputed chain finds it (ADR 0027): the start
        # reconciliation and the report journal read run_logs every start,
        # long after the engine checked the chain at the run's own start.
        first = self.write("a.log")
        second = self.write("b.log", ["a.log"])
        for tampered, name in ((first, "a.log"), (second, "b.log")):
            with self.subTest(name):
                path = Path(tampered)
                text = path.read_text(encoding="utf-8")
                changed = text.replace('"envelope_version":1,"schema_version":', '"envelope_version":1,"schema_version":9', 1)
                self.assertNotEqual(changed, text, "setup: a record changed")
                path.write_text(changed, encoding="utf-8")
                for read in (lambda: run_logs(second, today=TODAY), lambda: list(run_chain(second))):
                    with self.assertRaises(ResolveRefused) as caught:
                        read()
                    self.assertIn(name, str(caught.exception))
                    self.assertIn("record hash chain", str(caught.exception))
                path.write_text(text, encoding="utf-8")

    def test_a_first_runs_approval_ends_the_chain_and_every_run_is_walked(self):
        # A first run's approval (schema version 3) names no previous log and
        # states the starting book; the comparison of a fresh run's first
        # reading walks back however old the runs are (ADR 0019 and ADR
        # 0027, amended 2026-10-18), where run_logs stops at IBKR's lists.
        lines = [log_header()] + log_group(1, [("input", "operator.run.approved", 3, {
            "previous_log": "", "previous_log_final_hash": "",
            "starting_book": {"positions": [], "total_cash": 1000.0, "currency": "USD"}})], at="2026-10-01T13:30:00Z")
        first = Path(self.dir.name) / "a.log"
        first.write_text(chained(lines), encoding="utf-8")
        second = self.write("b.log", ["a.log"])
        self.assertEqual(run_logs(second, today=date(2026, 10, 20)), [second])
        walked = [(path, approval["previous_log"] if approval else None) for path, _, approval in run_chain(second)]
        self.assertEqual(walked, [(second, "a.log"), (str(first), "")])
        self.assertEqual(run_identity(str(first)), "e:1", "the first run is named by its approval")

    def test_an_approval_naming_no_log_and_no_starting_book_is_refused(self):
        lines = [log_header()] + log_group(1, [("input", "operator.run.approved", 3, {
            "previous_log": "", "previous_log_final_hash": "", "starting_book": None})])
        path = Path(self.dir.name) / "a.log"
        path.write_text(chained(lines), encoding="utf-8")
        with self.assertRaises(ResolveRefused):
            list(run_chain(str(path)))

    def test_the_chain_stops_at_a_run_that_ended_before_yesterdays_session(self):
        # IBKR's lists reach back only to today's session (the day's DAY
        # orders, executions since midnight), and a DAY order released after
        # yesterday's close works today, so a run whose last input is of a
        # New York date before yesterday's session (the weekday before
        # today) explains nothing in them, and nothing older than it is
        # read: an archived first run does not stop a start.
        first = self.write("a.log", at="2026-10-15T15:00:00Z")
        # 03:30 UTC on the 17th is still Friday the 16th in New York.
        second = self.write("b.log", ["a.log"], at="2026-10-17T03:30:00Z")
        third = self.write("c.log", ["b.log"])
        self.assertEqual(run_logs(third, today=date(2026, 10, 16)), [third, second, first])
        # Saturday and Monday: yesterday's session is Friday's.
        self.assertEqual(run_logs(third, today=TODAY), [third, second])
        self.assertEqual(run_logs(third, today=date(2026, 10, 19)), [third, second])
        self.assertEqual(run_logs(third, today=date(2026, 10, 20)), [third])
        # The run the walk stops at is read; the runs before it never are.
        os.remove(first)
        self.assertEqual(run_logs(third, today=date(2026, 10, 20)), [third])

    def log(self, name, groups, at):
        """A run's log of the given groups of (kind, type, schema version, payload), each recorded at at."""
        lines = [log_header()]
        n = 1
        for entries in groups:
            lines += log_group(n, entries, at=at)
            n += len(entries)
        path = Path(self.dir.name) / name
        path.write_text(chained(lines), encoding="utf-8")
        return str(path)

    def test_a_run_of_yesterdays_session_with_nothing_open_never_hides_an_earlier_one_with_an_order_working(self):
        # Run a released tw-1 after Friday's close, a DAY order that works
        # Monday; run b, started the same evening, released nothing; run c
        # starts on Saturday (a reboot) or Monday. IBKR still shows tw-1, so
        # a must be read: b of yesterday's session never ends the walk.
        first = self.log("a.log", [[("input", "market.session.closed", 1, {"session": "2026-10-16"}),
                                    ("decision", "risk.order.released", 1, {"client_order_id": "tw-1", "quantity": 9,
                                                                             "send": True})]],
                         at="2026-10-16T21:00:00Z")
        second = self.log("b.log", [[("input", "operator.run.approved", 2, {
            "previous_log": "a.log", "previous_log_final_hash": self.final_hash("a.log")})]],
            at="2026-10-16T23:00:00Z")
        third = self.write("c.log", ["b.log"])
        for today in (TODAY, date(2026, 10, 19)):
            with self.subTest(today=today.isoformat()):
                self.assertEqual(run_logs(third, today=today), [third, second, first])

    def test_a_dry_run_engines_synthetic_report_in_an_earlier_log_is_its_decision_never_read_as_an_input(self):
        # The dry-run engine records a synthetic terminal report (version 3,
        # synthetic: true) for each order it releases and never sends, as its
        # own decision (CONTEXT.md, "Order lifecycle report"); the first paper
        # run's approval names that run's log.
        dry = self.log("dry.log", [[("input", "market.session.closed", 1, {"session": "2026-10-16"}),
                                    ("decision", "risk.order.released", 1, {"client_order_id": "tw-dry-1",
                                                                             "quantity": 9, "send": False}),
                                    ("decision", "execution.order.lifecycle", 3, {
                                        "client_order_id": "tw-dry-1", "status": "canceled", "synthetic": True}),
                                    ("decision", "execution.fill", 3, {"client_order_id": "tw-dry-1",
                                                                       "synthetic": True})]],
                       at="2026-10-16T21:00:00Z")
        paper = self.write("paper.log", ["dry.log"])
        self.assertEqual(run_logs(paper, today=TODAY), [paper, dry])

    def test_an_order_the_adapters_own_records_settle_has_ended_for_the_chain(self):
        # Nothing at IBKR ever reports on an order settled as never sent or
        # not handed over, or one a run-ended settlement left unsent, and a
        # report struck off at that settlement is in no engine's log: the
        # adapter's own durable records end those orders.
        canceled = {"client_order_id": "tw-1", "status": "canceled", "occurred_at": "2026-10-15T21:00:01Z"}

        def struck(*reports):
            return {"unsent": [], "outbox": [{"id": "ibkr:{}:{}".format(t, n), "event_type": t, "schema_version": v,
                                              "event_time": "2026-10-15T21:00:01Z", "payload": p}
                                             for n, (t, v, p) in enumerate(reports)]}
        cases = {
            "settled never sent": ({"decisions": ["never-sent"]}, False),
            "settled not handed over": ({"decisions": ["not-handed-over"]}, False),
            "never sent, then found at IBKR": ({"decisions": ["never-sent", "sent"]}, True),
            "marked never sent while its input waits": ({"marks": {"tw-1": "never-sent"}}, False),
            "left unsent at a run-ended settlement": ({"run_ended": {"unsent": ["tw-1"], "outbox": []}}, False),
            "a cancellation struck at a settlement": ({"run_ended": struck(("execution.order.lifecycle", 2,
                                                                           canceled))}, False),
            "fills for the whole quantity struck at a settlement": ({"run_ended": struck(
                ("execution.fill", 5, dict(fill_payload("e.1.01"), quantity=4)),
                ("execution.fill", 5, dict(fill_payload("e.1.02"), quantity=5)))}, False),
            "a working status struck at a settlement": ({"run_ended": struck(
                ("execution.order.lifecycle", 2, dict(canceled, status="submitted")))}, True),
            "a cancellation settled as the input awaiting its reply": ({"run_ended": {
                "unsent": [], "outbox": [], "input": {"id": "ibkr:execution.order.lifecycle:9",
                                                      "type": "execution.order.lifecycle", "schema_version": 2,
                                                      "payload": canceled}}}, False),
            "the last of the fills settled as the input awaiting its reply": ({"run_ended": dict(
                struck(("execution.fill", 5, dict(fill_payload("e.1.01"), quantity=4))),
                input={"id": "ibkr:execution.fill:9", "type": "execution.fill", "schema_version": 5,
                       "payload": dict(fill_payload("e.1.02"), quantity=5)})}, False),
            "a working status settled as the input awaiting its reply": ({"run_ended": {
                "unsent": [], "outbox": [], "input": {"id": "ibkr:execution.order.lifecycle:9",
                                                      "type": "execution.order.lifecycle", "schema_version": 2,
                                                      "payload": dict(canceled, status="submitted")}}}, True),
            "nothing recorded": ({}, True),
        }
        first = self.earlier_day_run("a.log")
        second = self.write("b.log", ["a.log"])
        for name, (records, kept) in cases.items():
            with self.subTest(name):
                pending = PendingInput(str(Path(self.dir.name) / (name.replace(" ", "-") + ".json")))
                for decision in records.get("decisions", ()):
                    pending.record_decision({"client_order_id": "tw-1", "decision": decision, "reason": "checked",
                                             "at": "2026-10-16T12:00:00+00:00", "session_date": "2026-10-16"})
                for cid, mark in records.get("marks", {}).items():
                    pending.mark(cid, mark)
                if "run_ended" in records:
                    pending.record_run_ended(dict({"input": {"id": "session:9"}}, **records["run_ended"],
                                                  refused="run ended", reason="a fresh run", run="run-0",
                                                  at="2026-10-16T12:00:00+00:00", session_date="2026-10-16"))
                self.assertEqual(run_logs(second, today=TODAY, settled=pending.settled_orders()),
                                 [second, first] if kept else [second])

    def test_a_record_line_that_cannot_be_read_is_named_by_its_line_in_the_file(self):
        # A blank line counts, so the number in the stop is the line the
        # owner finds in the file.
        never_sent = {"client_order_id": "tw-1", "decision": "never-sent", "reason": "checked",
                      "at": "2026-10-16T12:00:00+00:00", "session_date": "2026-10-16"}
        pending = PendingInput(str(Path(self.dir.name) / "numbered.json"))
        pending.record_decision(never_sent)
        with open(pending.decisions_path, "a", encoding="utf-8") as f:
            f.write("\n" + '{"client_order_id": "tw-1", "decision": "se' + "\n")
        with self.assertRaises(HandoverRecordInvalid) as raised:
            pending.settled_orders()
        self.assertIn("{} line 3 is not JSON".format(pending.decisions_path), str(raised.exception))
        pending.record_run_ended({"input": {"id": "session:9"}, "refused": "run ended", "reason": "a fresh run",
                                  "run": "run-0", "unsent": [], "outbox": [], "at": "2026-10-16T12:00:00+00:00",
                                  "session_date": "2026-10-16"})
        with open(pending.run_ended_path, "a", encoding="utf-8") as f:
            f.write("\n\n" + '{"input": {"id": "sess' + "\n")
        with self.assertRaises(HandoverRecordInvalid) as raised:
            pending.run_ended()
        self.assertIn("{} line 4 is not JSON".format(pending.run_ended_path), str(raised.exception))

    def test_a_settlement_with_no_recorded_run_is_refused_naming_its_line(self):
        # The adapter records the run an input is sent to before it saves
        # the input, so a settlement naming no run is not one resolve
        # --run-ended writes: where its records fall among the runs cannot
        # be known, and it is never guessed.
        pending = PendingInput(str(Path(self.dir.name) / "no-run.json"))
        pending.record_run_ended({"input": {"id": "session:9"}, "refused": "run ended", "reason": "a fresh run",
                                  "run": None, "unsent": [], "outbox": [], "at": "2026-10-16T12:00:00+00:00",
                                  "session_date": "2026-10-16"})
        with self.assertRaises(HandoverRecordInvalid) as raised:
            pending.run_ended()
        for part in ("{} line 1".format(pending.run_ended_path), "no recorded run"):
            self.assertIn(part, str(raised.exception))

    def test_a_decision_line_that_cannot_be_read_stops_rather_than_settling_its_order(self):
        # An order settled --never-sent and then found at IBKR (--sent): if
        # the later line were skipped, the earlier one would end the order
        # although IBKR has it. Every line is read strictly, so any damage
        # stops the start (fail closed).
        first = self.earlier_day_run("a.log")
        second = self.write("b.log", ["a.log"])
        never_sent = {"client_order_id": "tw-1", "decision": "never-sent", "reason": "checked",
                      "at": "2026-10-16T12:00:00+00:00", "session_date": "2026-10-16"}
        bad_lines = {
            "not JSON": '{"client_order_id": "tw-1", "decision": "se',
            "not an object": '["tw-1", "sent"]',
            "no client order ID": json.dumps(dict(never_sent, client_order_id=None, decision="sent")),
            "an unknown decision": json.dumps(dict(never_sent, decision="maybe")),
        }
        for name, line in bad_lines.items():
            with self.subTest(name):
                pending = PendingInput(str(Path(self.dir.name) / (name.replace(" ", "-") + ".json")))
                pending.record_decision(never_sent)
                with open(pending.decisions_path, "a", encoding="utf-8") as f:
                    f.write(line + "\n")
                with self.assertRaises(HandoverRecordInvalid) as raised:
                    pending.settled_orders()
                self.assertIn(pending.decisions_path, str(raised.exception))
        pending = PendingInput(str(Path(self.dir.name) / "readable.json"))
        pending.record_decision(never_sent)
        self.assertEqual(run_logs(second, today=TODAY, settled=pending.settled_orders()), [second])
        self.assertTrue(os.path.exists(first))

    def test_the_chain_is_read_whole_on_the_first_days_of_the_calendar(self):
        # No session before the first date the calendar holds: every run is
        # read, never a date out of range.
        first = self.write("a.log")
        second = self.write("b.log", ["a.log"])
        for today in (date.min, date.min + timedelta(days=1), date.min + timedelta(days=3)):
            with self.subTest(today=today.isoformat()):
                self.assertEqual(run_logs(second, today=today), [second, first])

    def test_an_input_awaiting_its_reply_with_no_readable_recorded_time_is_refused_not_settled(self):
        # The session the waiting input was recorded in bounds the chain an
        # earlier run is looked for in: without it, the adapter stops with a
        # reason rather than guess.
        self.write("a.log")
        second = self.write("b.log", ["a.log"])
        for recorded_at in (None, "", "yesterday", "2026-10-16T21:00:00"):
            with self.subTest(recorded_at=recorded_at):
                pending = PendingInput(str(Path(self.dir.name) / "pending-{}.json".format(abs(hash(recorded_at)))))
                waiting = {"id": "session:9", "type": "market.session.closed", "schema_version": 1,
                           "event_time": "2026-10-16T20:00:00Z", "payload": {}}
                if recorded_at is not None:
                    waiting["recorded_at"] = recorded_at
                pending.save(waiting)
                with self.assertRaises(ResolveRefused) as raised:
                    earlier_run(pending, second)
                self.assertIn("recorded_at", str(raised.exception))
                with self.assertRaises(ResolveRefused):
                    resolve_run_ended(pending, "session:9", "a fresh run", second, today=TODAY)
                self.assertEqual(pending.load()["id"], "session:9")
                self.assertEqual(pending.run_ended(), [])

    def test_an_input_awaiting_its_reply_with_no_record_of_its_run_is_refused_not_settled(self):
        # A settlement records the run it ends, so without the adapter's
        # .run record for the input nothing is written: the line would name
        # no run, and every later start would stop on it.
        self.write("a.log")
        second = self.write("b.log", ["a.log"])
        for name, run_of in (("missing", None), ("another input", "session:8")):
            with self.subTest(name):
                pending = PendingInput(str(Path(self.dir.name) / "pending-{}.json".format(name.replace(" ", "-"))))
                if run_of is not None:
                    pending.record_run(run_of, "run-0", "a.log")
                pending.save({"id": "session:9", "type": "market.session.closed", "schema_version": 1,
                              "event_time": "2026-10-16T20:00:00Z", "recorded_at": "2026-10-16T20:00:00Z",
                              "payload": {}})
                with self.assertRaises(ResolveRefused) as raised:
                    resolve_run_ended(pending, "session:9", "a fresh run", second, today=TODAY)
                for part in ("session:9", pending.run_path):
                    self.assertIn(part, str(raised.exception))
                self.assertEqual(pending.load()["id"], "session:9")
                self.assertFalse(Path(pending.run_ended_path).exists())

    def earlier_day_run(self, name, reports=()):
        """A run of an earlier day (the 15th) whose one decision released tw-1 for 9, then the reports in reports.

        With a fill of more than 9 among reports, the release's quantity is
        written as one that cannot be read, so fills never end it.
        """
        quantity = "9" if any(r[2].get("quantity", 0) > 9 for r in reports) else 9
        lines = [log_header()]
        lines += log_group(1, [("input", "market.session.closed", 1, {"session": "2026-10-15"}),
                               ("decision", "risk.order.released", 1, {"client_order_id": "tw-1",
                                                                        "quantity": quantity, "send": True})],
                           at="2026-10-15T21:00:00Z")
        for n, (event_type, version, payload) in enumerate(reports, 3):
            lines += log_group(n, [("input", event_type, version, payload)], at="2026-10-15T21:00:00Z")
        path = Path(self.dir.name) / name
        path.write_text(chained(lines), encoding="utf-8")
        return str(path)

    def test_the_chain_keeps_an_earlier_day_run_whose_released_order_has_no_terminal_report(self):
        # A DAY order placed after the close works the next session, so
        # IBKR's open orders still show it on the day after its run's last
        # input: that run's log is read until its order ends.
        canceled = {"client_order_id": "tw-1", "status": "canceled"}
        cases = {
            "no report at all": ((), True),
            "a partial fill": ((("execution.fill", 5, dict(fill_payload("e.1.01"), quantity=4)),), True),
            "a working status only": ((("execution.order.lifecycle", 2, dict(canceled, status="submitted")),), True),
            "a cancellation": ((("execution.order.lifecycle", 2, canceled),), False),
            "a refusal": ((("execution.order.lifecycle", 2, dict(canceled, status="invalid")),), False),
            "fills for the whole quantity": ((("execution.fill", 5, dict(fill_payload("e.1.01"), quantity=4)),
                                              ("execution.fill", 5, dict(fill_payload("e.1.02"), quantity=5))), False),
            "fills beyond a quantity that cannot be read": ((("execution.fill", 5, dict(fill_payload("e.1.01"),
                                                                                         quantity=20)),), True),
        }
        for name, (reports, kept) in cases.items():
            with self.subTest(name):
                first = self.earlier_day_run("a.log", reports)
                second = self.write("b.log", ["a.log"])
                self.assertEqual(run_logs(second, today=TODAY), [second, first] if kept else [second])

    def test_the_chain_walks_on_past_each_earlier_day_run_that_still_has_an_order_open(self):
        # The report that ends an earlier run's order may be journalled into
        # a later run's log: the adapter records IBKR's reports into the run
        # now running.
        first = self.earlier_day_run("a.log")
        canceled = {"client_order_id": "tw-1", "status": "canceled"}
        for name, b_reports, kept in (("still open", [], True),
                                      ("ended in the later run", [("execution.order.lifecycle", 2, canceled)], False)):
            with self.subTest(name):
                lines = [log_header()]
                lines += log_group(1, [("input", "operator.run.approved", 2, {
                    "previous_log": "a.log", "previous_log_final_hash": self.final_hash("a.log")}),
                    ("decision", "risk.order.released", 1, {"client_order_id": "tw-2", "quantity": 1,
                                                             "send": True})], at="2026-10-16T21:00:00Z")
                for n, (event_type, version, payload) in enumerate(b_reports, 3):
                    lines += log_group(n, [("input", event_type, version, payload)], at="2026-10-16T21:00:00Z")
                second = Path(self.dir.name) / "b.log"
                second.write_text(chained(lines), encoding="utf-8")
                third = self.write("c.log", ["b.log"])
                self.assertEqual(run_logs(third, today=TODAY),
                                 [third, str(second), first] if kept else [third, str(second)])

    def test_an_approval_whose_final_hash_is_not_the_named_logs_is_refused(self):
        self.write("a.log")
        wrong = self.write("b.log", ["a.log"], final_hash="sha256:" + "f" * 64)
        with self.assertRaises(ResolveRefused) as raised:
            run_logs(wrong, today=TODAY)
        self.assertIn("final record hash", str(raised.exception))
        for bad in ("", "f" * 64, 7, None):
            with self.subTest(final_hash=bad):
                path = Path(self.dir.name) / "bad.log"
                path.write_text(chained([log_header()] + log_group(1, [("input", "operator.run.approved", 2, {
                    "previous_log": "a.log", "previous_log_final_hash": bad})])), encoding="utf-8")
                with self.assertRaises(ResolveRefused):
                    run_logs(str(path), today=TODAY)
        # A named log with no committed group has no final record to match.
        Path(self.dir.name, "empty.log").write_text(log_header(), encoding="utf-8")
        with self.assertRaises(ResolveRefused):
            run_logs(self.write("e.log", ["empty.log"]), today=TODAY)

    def test_a_log_with_two_approvals_or_a_chain_that_loops_is_refused(self):
        self.write("a.log")
        for name, previous in (("two.log", ["a.log", "a.log"]), ("self.log", ["self.log"])):
            with self.subTest(name), self.assertRaises(ResolveRefused):
                run_logs(self.write(name, previous), today=TODAY)
        self.write("x.log", ["y.log"])
        with self.assertRaises(ResolveRefused):
            run_logs(self.write("y.log", ["x.log"]), today=TODAY)

    def test_an_approval_naming_no_log_file_name_is_refused(self):
        for previous in ("", ".", "..", "../a.log", "d/a.log", 7):
            with self.subTest(previous=previous), self.assertRaises(ResolveRefused):
                run_logs(self.write("n.log", [previous]), today=TODAY)



class RunIdentityTests(unittest.TestCase):
    """A run's identity: its Run approval's ID, else its log's first committed input, which a resume copies."""

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)

    def write(self, name, groups, at="2026-10-16T13:30:00Z"):
        lines = [log_header()]
        n = 1
        for entries in groups:
            lines += log_group(n, entries, at=at)
            n += len(entries)
        path = Path(self.dir.name) / name
        path.write_text("".join(lines), encoding="utf-8")
        return str(path)

    def test_a_run_with_no_approval_is_named_by_its_first_committed_input_never_by_nothing(self):
        configuration = [("input", "strategy.target-weight.configuration", 7, {"strategy_id": "boost"})]
        first = self.write("a.log", [configuration])
        resumed = self.write("a-resumed.log", [configuration])
        later = self.write("b.log", [configuration], at="2026-10-17T13:30:00Z")
        self.assertNotEqual(run_identity(first), "")
        self.assertEqual(run_identity(first), run_identity(resumed))
        self.assertNotEqual(run_identity(first), run_identity(later))

    def test_a_log_with_no_committed_input_has_no_identity_and_is_refused(self):
        empty = Path(self.dir.name) / "empty.log"
        empty.write_text(log_header(), encoding="utf-8")
        with self.assertRaises(ResolveRefused) as raised:
            run_identity(str(empty))
        self.assertIn("no committed input", str(raised.exception))

    def test_a_run_with_an_approval_is_named_by_it(self):
        path = self.write("c.log", [[("input", "strategy.target-weight.configuration", 7, {})],
                                    [("input", "operator.run.approved", 2, {"previous_log": "a.log"})]])
        self.assertEqual(run_identity(path), "e:2")


if __name__ == "__main__":
    unittest.main()
