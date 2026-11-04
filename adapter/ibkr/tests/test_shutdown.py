"""SIGTERM at the engine reply boundary: settle acknowledgements, preserve work still owed (ADR 0027 section 4)."""
import hashlib
import json
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path
from types import SimpleNamespace
from datetime import date
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
sys.path.insert(0, str(Path(__file__).resolve().parent))

from connection import StopRequested
from engine_double import LoggedEngine
from fake_tws import ACCOUNT
from loop import _AskedOnItsOwnThread
from orders import collect_broker_orders
from relay import RELEASED, PendingInput, Relay
from sender import PaperSender, release_to_request
from test_connection import Harness


class ExchangeStopTests(unittest.TestCase):
    def test_stop_during_post_reply_open_order_read_preserves_input_for_restart(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            engine = LoggedEngine(root / "engine.log", decisions={"input-1": [{
                "type": RELEASED, "payload": {
                    "account_id": ACCOUNT, "client_order_id": "tag-1", "instrument_id": "SPY",
                    "side": "buy", "quantity": 9, "limit_price": 401.5, "mode": "paper", "send": True}}]})
            payload = {"input": "input-1"}
            envelope = {"id": "input-1", "type": "test.input", "envelope_version": 1, "schema_version": 1,
                        "event_time": "2026-10-16T13:30:00Z", "recorded_at": "2026-10-16T13:30:00Z",
                        "sequence": engine.next_sequence, "source": "ibkr-adapter",
                        "configuration_hash": engine.configuration_hash, "strategy_version": engine.strategy_version,
                        "payload": payload,
                        "payload_hash": hashlib.sha256(json.dumps(payload, separators=(",", ":")).encode()).hexdigest()}
            pending_path = str(root / "pending.json")
            settled, sent = [], []
            session = date(2026, 10, 16)

            def start(engine):
                h = Harness(mode="paper")
                h.conn.connect()
                h.conn.mark_reconciled()
                pending = PendingInput(pending_path)
                relay = Relay(engine, PaperSender(h.conn, sent.append, today=lambda: session), pending,
                              lambda: collect_broker_orders(h.conn),
                              lambda order, day: release_to_request(order, "paper", h.conn.config.con_ids, session=day),
                              settled=settled.append)
                self.addCleanup(relay.close)
                return h, pending, relay

            h, pending, relay = start(engine)
            stop = threading.Event()
            h.conn.stop_event = stop
            read_open_orders = h.fake.req_all_open_orders

            def stop_during_read():
                self.assertEqual([e["id"] for e in engine.received], [envelope["id"]])
                read_open_orders()
                stop.set()

            # Stop unwinds from the real pump before handover; the input is
            # owed until its released order is sent (ADR 0027 section 4).
            with mock.patch.object(h.fake, "req_all_open_orders", side_effect=stop_during_read):
                with self.assertRaises(StopRequested):
                    relay.exchange(envelope)
            self.assertEqual(h.fake.called("place_order"), [])
            self.assertEqual(pending.load(), envelope)
            self.assertEqual(pending.handovers(), {})
            self.assertEqual(settled, [])
            self.assertEqual(sent, [])
            relay.close()

            resumed = LoggedEngine.resumed(engine.path, root / "resumed.log")
            restarted, pending, relay = start(resumed)
            relay.exchange(pending.load())
            relay.exchange(envelope)
            self.assertEqual(len(restarted.fake.called("place_order")), 1)
            self.assertEqual(restarted.fake.placed[0][2].orderRef, "tag-1")
            self.assertEqual(len(sent), 1)
            self.assertIsNone(pending.load())
            self.assertEqual(settled, [envelope, envelope])
            self.assertEqual(resumed.received, [envelope, envelope])
            self.assertEqual(resumed.inputs(), [])

    def stopped_after_reply(self, worker):
        worker.join(1)
        self.assertFalse(worker.is_alive())
        raise StopRequested()

    def test_completed_reply_without_orders_can_be_settled(self):
        reply = {"payload": {"decisions": []}}
        engine = SimpleNamespace(decide=lambda envelope: reply)
        client = _AskedOnItsOwnThread(engine, self.stopped_after_reply)
        self.assertIs(client.decide({}), reply)

    def test_completed_reply_releasing_an_order_stays_pending_on_stop(self):
        reply = {"payload": {"decisions": [{"type": RELEASED, "payload": {"send": True}}]}}
        engine = SimpleNamespace(decide=lambda envelope: reply)
        client = _AskedOnItsOwnThread(engine, self.stopped_after_reply)
        with self.assertRaises(StopRequested):
            client.decide({})

    def test_unanswered_exchange_stops_promptly_and_stays_pending(self):
        release = threading.Event()
        started = threading.Event()
        workers = []

        def decide(envelope):
            started.set()
            release.wait()
            return {"payload": {"decisions": []}}

        def stop(worker):
            workers.append(worker)
            self.assertTrue(started.wait(1))
            raise StopRequested()

        client = _AskedOnItsOwnThread(SimpleNamespace(decide=decide), stop)
        try:
            before = time.monotonic()
            with self.assertRaises(StopRequested):
                client.decide({})
            self.assertLess(time.monotonic() - before, 1)
        finally:
            release.set()
            for worker in workers:
                worker.join(1)


if __name__ == "__main__":
    unittest.main()
