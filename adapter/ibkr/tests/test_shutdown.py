"""SIGTERM at the engine reply boundary: settle acknowledgements, preserve work still owed (ADR 0027 section 4)."""
import sys
import threading
import time
import unittest
from pathlib import Path
from types import SimpleNamespace

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from connection import StopRequested
from loop import _AskedOnItsOwnThread
from relay import RELEASED


class ExchangeStopTests(unittest.TestCase):
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
