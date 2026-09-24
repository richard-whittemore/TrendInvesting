import hashlib
import json
import subprocess
import sys
import unittest
from datetime import datetime, timedelta
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock

adapter_dir = str(Path(__file__).resolve().parents[1])
if adapter_dir not in sys.path:
    sys.path.insert(0, adapter_dir)

from client import Client as WireClient
from publisher import Publisher, raw_view


class Client:
    def __init__(self, reply_overrides=None, after_reply=None):
        self.sent = []
        self.closed = False
        self.reply_overrides = reply_overrides or {}
        self.after_reply = after_reply

    def close(self):
        self.closed = True

    def decide(self, envelope):
        self.sent.append(envelope)
        reply = {"type": "engine.decisions", "envelope_version": 1,
                "schema_version": 1, "sequence": envelope["sequence"],
                "causation_id": envelope["id"], "correlation_id": envelope["correlation_id"],
                "configuration_hash": "hash",
                "strategy_version": "version", "payload": {"decisions": []}}
        reply.update(self.reply_overrides.get(envelope["type"], {}))
        encoded = json.dumps(reply["payload"], separators=(",", ":")).encode()
        reply.setdefault("payload_hash", hashlib.sha256(encoded).hexdigest())
        # Exercise the production client's exact-byte hash check with the
        # fake engine's reply; no socket server is needed for these cases.
        wire = WireClient.__new__(WireClient)
        wire._broken = False
        wire.max_frame_bytes = 1 << 20
        wire._sock = Mock()
        wire._read_line = lambda: json.dumps({"envelope": reply}, separators=(",", ":")).encode()
        result = wire.decide(envelope)
        if self.after_reply is not None:
            self.after_reply(envelope)
        return result


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
    def test_snapshot_payload_matches_go_contract(self):
        client = Client()
        pub = Publisher(client, "hash", "version", "test")
        for day, cash in ((6, 75000.25), (9, 0)):
            end = "2014-06-{:02d}T20:00:00Z".format(day)
            b = bar(day)
            pub.publish("AAPL", b, raw_view(Frame(b.EndTime), b.EndTime), end)
            pub.publish_snapshot(SimpleNamespace(TotalPortfolioValue=123456.75, Cash=cash), end)
            envelope = client.sent[-1]
            self.assertEqual(envelope["payload"], {
                "as_of": end, "equity": 123456.75, "available_cash": cash, "currency": "USD"})
            self.assertEqual(envelope["event_time"], end)
            self.assertEqual(envelope["recorded_at"], end)
            result = subprocess.run(
                ["go", "run", "./adapter/lean/tests/testdata/snapshot_contract.go"],
                cwd=Path(__file__).resolve().parents[3],
                input=json.dumps(envelope, separators=(",", ":")),
                text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_snapshot_rejects_duplicate_or_decreasing_as_of(self):
        client = Client()
        pub = Publisher(client, "hash", "version", "test")
        portfolio = SimpleNamespace(TotalPortfolioValue=100000, Cash=50000)
        pub.publish_snapshot(portfolio, "2014-06-09T20:00:00Z")
        for end in ("2014-06-09T20:00:00Z", "2014-06-06T20:00:00Z"):
            with self.subTest(end=end), self.assertRaises(ValueError):
                pub.publish_snapshot(portfolio, end)
        self.assertEqual(len(client.sent), 1)
        self.assertEqual(pub.sequence, 2)

    def test_invalid_portfolio_figures_are_not_published(self):
        for equity, cash in ((0, 1), (-1, 1), (float("nan"), 1),
                             (float("inf"), 1), (1, -1),
                             (1, float("nan")), (1, float("inf"))):
            with self.subTest(equity=equity, cash=cash):
                client = Client()
                pub = Publisher(client, "hash", "version", "test")
                with self.assertRaises(ValueError):
                    pub.publish_snapshot(SimpleNamespace(TotalPortfolioValue=equity, Cash=cash),
                                         "2014-06-09T20:00:00Z")
                self.assertEqual(client.sent, [])
                self.assertEqual(pub.sequence, 1)

    def test_recorded_at_is_the_bars_own_period_end(self):
        """Two runs over the same bars write the same input envelopes."""
        client = Client()
        Publisher(client, "hash", "version", "test").publish(
            "AAPL", bar(9), raw_view(Frame(bar(9).EndTime), bar(9).EndTime), "2014-06-09T20:00:00Z")
        self.assertEqual(client.sent[0]["recorded_at"], "2014-06-09T20:00:00Z")

    def test_reply_with_another_runs_correlation_id_is_rejected(self):
        client = Client()
        answer = client.decide
        client.decide = lambda e: dict(answer(e), correlation_id="another-run")
        with self.assertRaises(ValueError):
            Publisher(client, "hash", "version", "test").publish(
                "AAPL", bar(9), raw_view(Frame(bar(9).EndTime), bar(9).EndTime), "2014-06-09T20:00:00Z")

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

    def test_run_stopped_payload_matches_go_contract(self):
        client = Client()
        pub = Publisher(client, "hash", "version", "test")
        b = bar(9)
        pub.publish("AAPL", b, raw_view(Frame(b.EndTime), b.EndTime), "2014-06-09T20:00:00Z")
        pub.publish_run_stopped("delisted", "LEAN reports AAPL DELISTED", "AAPL")
        envelope = client.sent[-1]
        self.assertEqual(envelope["type"], "adapter.run.stopped")
        self.assertEqual(envelope["payload"], {
            "reason": "delisted", "instrument_id": "AAPL", "detail": "LEAN reports AAPL DELISTED"})
        result = subprocess.run(
            ["go", "run", "./adapter/lean/tests/testdata/run_stopped_contract.go"],
            cwd=Path(__file__).resolve().parents[3],
            input=json.dumps(envelope, separators=(",", ":")),
            text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_run_stopped_is_sent_immediately_before_completion(self):
        client = Client()
        pub = Publisher(client, "hash", "version", "test")
        b = bar(9)
        pub.publish("AAPL", b, raw_view(Frame(b.EndTime), b.EndTime), "2014-06-09T20:00:00Z")
        pub.publish_run_stopped("delisted", "reason", "AAPL")
        pub.publish_run_completed()
        stop, completed = client.sent[-2], client.sent[-1]
        self.assertEqual(stop["type"], "adapter.run.stopped")
        self.assertEqual(completed["type"], "replay.run.completed")
        self.assertEqual(stop["sequence"] + 1, completed["sequence"])
        self.assertEqual(stop["event_time"], "2014-06-09T20:00:00Z")

    def test_run_stopped_rejects_an_unrecognised_reason(self):
        client = Client()
        pub = Publisher(client, "hash", "version", "test")
        pub.publish("AAPL", bar(9), raw_view(Frame(bar(9).EndTime), bar(9).EndTime), "2014-06-09T20:00:00Z")
        with self.assertRaises(ValueError):
            pub.publish_run_stopped("invalid-startup", "reason", "AAPL")
        self.assertEqual(len(client.sent), 1)

    def test_run_stopped_requires_detail(self):
        client = Client()
        pub = Publisher(client, "hash", "version", "test")
        pub.publish("AAPL", bar(9), raw_view(Frame(bar(9).EndTime), bar(9).EndTime), "2014-06-09T20:00:00Z")
        with self.assertRaises(ValueError):
            pub.publish_run_stopped("delisted", "", "AAPL")
        self.assertEqual(len(client.sent), 1)

    def test_run_stopped_requires_an_instrument_for_delisted(self):
        client = Client()
        pub = Publisher(client, "hash", "version", "test")
        pub.publish("AAPL", bar(9), raw_view(Frame(bar(9).EndTime), bar(9).EndTime), "2014-06-09T20:00:00Z")
        with self.assertRaises(ValueError):
            pub.publish_run_stopped("delisted", "reason", None)
        self.assertEqual(len(client.sent), 1)

    def test_run_stopped_with_no_prior_input_raises(self):
        client = Client()
        pub = Publisher(client, "hash", "version", "test")
        with self.assertRaises(ValueError):
            pub.publish_run_stopped("delisted", "reason", "AAPL")
        self.assertEqual(client.sent, [])

    def test_nothing_may_follow_completion_including_a_stop(self):
        client = Client()
        pub = Publisher(client, "hash", "version", "test")
        pub.publish("AAPL", bar(9), raw_view(Frame(bar(9).EndTime), bar(9).EndTime), "2014-06-09T20:00:00Z")
        pub.publish_run_completed()
        with self.assertRaises(ValueError):
            pub.publish_run_stopped("delisted", "reason", "AAPL")

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
