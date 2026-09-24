import hashlib
import json
import unittest
from datetime import datetime, timedelta
from types import SimpleNamespace

from publisher import Publisher, raw_view


class Client:
    def __init__(self):
        self.sent = []

    def decide(self, envelope):
        self.sent.append(envelope)
        return {"type": "engine.decisions", "envelope_version": 1,
                "schema_version": 1, "sequence": envelope["sequence"],
                "causation_id": envelope["id"], "configuration_hash": "hash",
                "strategy_version": "version", "payload": {"decisions": []}}


def bar(day):
    return SimpleNamespace(EndTime=datetime(2014, 6, day, 16),
                           Open=23, High=24, Low=22, Close=23.056, Volume=2800)


class Frame:
    empty = False
    def __init__(self, end):
        self.index = [("AAPL", end)]
        self.iloc = [{"open": 644, "high": 646, "low": 640,
                      "close": 645.57, "volume": 100}]
    def __len__(self):
        return len(self.index)


class PublisherTests(unittest.TestCase):
    def test_configuration_then_bars_and_labelled_views(self):
        client = Client()
        pub = Publisher(client, "hash", "version", "run")
        for day in (6, 9):
            b = bar(day)
            raw = raw_view(Frame(b.EndTime), b.EndTime)
            pub.publish("AAPL", b, raw, "2014-06-{:02d}T20:00:00Z".format(day))
        self.assertEqual([e["sequence"] for e in client.sent], [2, 3])
        for e in client.sent:
            self.assertEqual(e["type"], "market.bar.completed")
            self.assertEqual(e["payload"]["split_adjusted"]["view"], "split-adjusted")
            self.assertEqual(e["payload"]["raw"]["view"], "raw")
            self.assertNotEqual(e["payload"]["raw"]["close"],
                                e["payload"]["split_adjusted"]["close"])
            encoded = json.dumps(e["payload"], separators=(",", ":"), allow_nan=False).encode()
            self.assertEqual(e["payload_hash"], hashlib.sha256(encoded).hexdigest())
            self.assertEqual(e["event_time"], e["payload"]["period_end"])

    def test_raw_must_match_completed_period_exactly(self):
        end = bar(9).EndTime
        for frame in (None, Frame(end - timedelta(days=3))):
            with self.assertRaises(ValueError):
                raw_view(frame, end)
        frame = Frame(end)
        frame.index.append(("AAPL", end))
        with self.assertRaises(ValueError):
            raw_view(frame, end)

    def test_fail_closed_on_duplicate_bar_or_wrong_engine_identity(self):
        client = Client()
        pub = Publisher(client, "hash", "version", "run")
        b = bar(6)
        raw = raw_view(Frame(b.EndTime), b.EndTime)
        pub.publish("AAPL", b, raw, "2014-06-06T20:00:00Z")
        with self.assertRaises(ValueError):
            pub.publish("AAPL", b, raw, "2014-06-06T20:00:00Z")
        self.assertEqual(len(client.sent), 1)
        pub = Publisher(client, "other", "version", "run")
        with self.assertRaises(ValueError):
            pub.publish("AAPL", b, raw, "2014-06-06T20:00:00Z")


if __name__ == "__main__":
    unittest.main()
