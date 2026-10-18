"""The day's market-data stream, delivered to the engine through the adapter's outbox, after the broker snapshot.

The data job (adapter/marketdata/tidata.py) writes each run's stream files
to STREAMS/<run>/<engine>-<last session>.jsonl, one envelope per line in
delivery order, each file's sequence a file-local ordinal 1..N. The adapter
picks up each file it has not delivered, in session order, reads the
account and IBKR's marks for its holdings, and records the broker snapshot
and then every envelope of the file in its outbox in one write, so the
snapshot reaches the engine first and the session closes after its bars
(ADR 0019's snapshot rule, ADR 0021). The engine's sequence is set as each
envelope is sent; every other field goes as written. Synthetic figures
only; nothing here reaches IBKR.
"""
import hashlib
import json
import sys
import tempfile
import unittest
from datetime import date, datetime, timezone
from pathlib import Path
from unittest import mock

tests_dir = Path(__file__).resolve().parent
if str(tests_dir) not in sys.path:
    sys.path.insert(0, str(tests_dir))

from decimal import Decimal

from engine_double import CONFIGURATION_HASH, STRATEGY_VERSION, LoggedEngine
from fake_tws import ACCOUNT, FakeTWS, contract
from test_connection import Clock, config
from test_loop import COMMISSION_WAIT, RECEIVED, SUMMARY, Ids, Unanswered
from delivery import (DELIVERED_SUFFIX, DeliveryRecordInvalid, StreamRefused, market_open, read_stream)
from loop import AdapterLoop, Outbox
from relay import RELEASED, EngineUnavailable, PendingInput, read_engine_log
from snapshot import BROKER_SNAPSHOT_TYPE

RUN = "engine"
BAR = "market.bar.completed"
CLOSED = "market.session.closed"
SPY_CON_ID = 756733
# The fake's server time is 2027-01-15 08:00 UTC, before the open in New York.
FRIDAY = date(2027, 1, 14)


def _envelope(kind, version, ident, event_time, payload, sequence, run=RUN, configuration_hash=CONFIGURATION_HASH,
              strategy_version=STRATEGY_VERSION):
    raw = json.dumps(payload, separators=(",", ":"), allow_nan=False)
    return {"id": ident, "type": kind, "envelope_version": 1, "schema_version": version, "event_time": event_time,
            "recorded_at": "2027-01-14T22:05:00Z", "sequence": sequence, "correlation_id": run, "source": "marketdata",
            "strategy_version": strategy_version, "configuration_hash": configuration_hash,
            "payload_hash": hashlib.sha256(raw.encode()).hexdigest(), "payload": payload}


def session_envelopes(sessions, **identity):
    """The Boost stream's envelopes for sessions, as the encoder writes them: each session's bar, then its close."""
    out = []
    for day in sessions:
        closed = "{}T21:00:00Z".format(day.isoformat())
        out.append((BAR, 2, "marketdata:bar:SPY:{}".format(day), closed,
                    {"instrument_id": "SPY", "period_end": closed, "raw": {"close": 400.5}}))
        out.append((CLOSED, 1, "marketdata:session-closed:{}".format(day), closed,
                    {"period_end": closed, "instrument_ids": ["SPY"]}))
    return [_envelope(*e, sequence=n, **identity) for n, e in enumerate(out, 1)]


def write_stream(folder, last, envelopes, prefix="boost"):
    folder.mkdir(parents=True, exist_ok=True)
    path = folder / "{}-{}.jsonl".format(prefix, last.isoformat())
    path.write_text("".join(json.dumps(e, separators=(",", ":")) + "\n" for e in envelopes))
    return path


class DeliveryTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        root = Path(self.dir.name)
        self.pending = str(root / "pending.json")
        self.engine_log = str(root / (RUN + ".log"))
        self.streams = root / "streams"
        self.folder = self.streams / RUN
        self.folder.mkdir(parents=True)
        self.engine = LoggedEngine(self.engine_log)
        self.fake = FakeTWS()
        self.fake.summary = list(SUMMARY)
        self.fake.positions = [(ACCOUNT, contract(), Decimal("147"), 312.5)]
        self.fake.marks = {SPY_CON_ID: 401.17}
        self.new_id = Ids()  # one sequence of input IDs across restarts, as uuid4 gives

    def loop(self, engine=None, fake=None, streams=True):
        clock = Clock()

        def sleep(seconds):
            clock.now += seconds
        loop = AdapterLoop(config(mode="paper", paper_sending_enabled=True, commission_wait=COMMISSION_WAIT),
                           fake or self.fake, engine or self.engine, self.pending, self.engine_log,
                           now=lambda: RECEIVED, today=lambda: FRIDAY, sleep=sleep, monotonic=clock, poll=0,
                           new_id=self.new_id, streams=str(self.streams) if streams else None)
        self.addCleanup(loop.close)
        return loop

    def settle(self, loop, steps=40):
        for _ in range(steps):
            loop.step()

    def types(self):
        return [e["type"] for e in self.engine.inputs()]

    def ids(self):
        return [e["id"] for e in self.engine.inputs()]

    def test_a_stream_file_reaches_the_engine_after_the_broker_snapshot_in_file_order_as_written(self):
        written = session_envelopes([date(2027, 1, 13), FRIDAY])
        write_stream(self.folder, FRIDAY, written)
        loop = self.loop()
        loop.start()
        self.settle(loop)
        self.assertEqual(self.types(), [BROKER_SNAPSHOT_TYPE, BAR, CLOSED, BAR, CLOSED])
        sent = self.engine.inputs()
        # The engine's sequence, set as each is sent: its configuration is 1.
        self.assertEqual([e["sequence"] for e in sent], [2, 3, 4, 5, 6])
        for want, got in zip(written, sent[1:]):
            self.assertEqual({k: v for k, v in got.items() if k != "sequence"},
                             {k: v for k, v in want.items() if k != "sequence"}, "sent as written")
        snapshot = sent[0]["payload"]
        self.assertEqual(snapshot["account_id"], ACCOUNT)
        self.assertEqual(snapshot["positions"], [{"instrument_id": "SPY", "quantity": 147, "mark": 401.17,
                                                  "mark_set": True}])
        self.assertIs(snapshot["market_open"], False, "08:00 UTC is before the open")
        self.assertEqual(snapshot["unreadable"], [])
        self.assertEqual(sent[0]["event_time"], snapshot["as_of"])
        self.assertEqual(loop.outbox.load(), [])
        self.assertIsNone(PendingInput(self.pending).load())

    def test_the_mark_is_asked_of_ibkr_for_each_holding_by_its_contract(self):
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        self.settle(loop)
        asked = self.fake.called("req_mkt_data")
        self.assertEqual([(c[2].conId, c[2].symbol, c[2].secType, c[2].currency, c[2].exchange) for c in asked],
                         [(SPY_CON_ID, "SPY", "STK", "USD", "SMART")])

    def test_a_holding_ibkr_gives_no_mark_for_is_stated_without_one(self):
        self.fake.marks = {}
        self.fake.mark_errors = {SPY_CON_ID: (354, "synthetic: requested market data is not subscribed")}
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        self.settle(loop)
        snapshot = self.engine.inputs()[0]["payload"]
        self.assertEqual(snapshot["positions"], [{"instrument_id": "SPY", "quantity": 147, "mark": 0,
                                                  "mark_set": False}])

    def test_a_holding_with_no_contract_number_is_stated_without_a_mark_and_not_asked_for(self):
        self.fake.positions.append((ACCOUNT, contract(con_id=8314, symbol="IEF"), Decimal("5"), 95.1))
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        self.settle(loop)
        positions = self.engine.inputs()[0]["payload"]["positions"]
        self.assertEqual([(p["instrument_id"], p["mark_set"]) for p in positions], [("IEF", False), ("SPY", True)])
        self.assertEqual([c[2].conId for c in self.fake.called("req_mkt_data")], [SPY_CON_ID])

    def test_files_go_in_session_order_once_each_and_a_later_file_follows_its_own_snapshot(self):
        first, second, third = date(2027, 1, 12), date(2027, 1, 13), FRIDAY
        envelopes = session_envelopes([first, second, third])
        write_stream(self.folder, second, [dict(e, sequence=n) for n, e in enumerate(envelopes[2:4], 1)])
        write_stream(self.folder, first, [dict(e, sequence=n) for n, e in enumerate(envelopes[:2], 1)])
        loop = self.loop()
        loop.start()
        self.settle(loop)
        self.assertEqual(self.ids(), [
            "ibkr:account.broker.snapshot:1", "marketdata:bar:SPY:2027-01-12", "marketdata:session-closed:2027-01-12",
            "marketdata:bar:SPY:2027-01-13", "marketdata:session-closed:2027-01-13"])
        loop.close()
        again = self.loop()
        again.start()
        self.settle(again)
        self.assertEqual(len(self.ids()), 5, "nothing delivered twice across a restart")
        self.fake.server_time += 3600
        write_stream(self.folder, third, [dict(e, sequence=n) for n, e in enumerate(envelopes[4:], 1)])
        self.settle(again)
        self.assertEqual(self.types()[5:], [BROKER_SNAPSHOT_TYPE, BAR, CLOSED])
        snapshots = self.engine.inputs(BROKER_SNAPSHOT_TYPE)
        self.assertLess(snapshots[0]["payload"]["as_of"], snapshots[1]["payload"]["as_of"])

    def test_files_that_are_not_streams_are_left_alone(self):
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        (self.folder / "tmpab12.part").write_text("{not json")
        (self.folder / "README").write_text("synthetic")
        loop = self.loop()
        loop.start()
        self.settle(loop)
        self.assertEqual(self.types(), [BROKER_SNAPSHOT_TYPE, BAR, CLOSED])

    def test_without_a_streams_folder_nothing_is_delivered(self):
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop(streams=False)
        loop.start()
        self.settle(loop)
        self.assertEqual(self.types(), [])

    def test_a_run_with_no_stream_folder_yet_stops_at_start_naming_the_warm_up(self):
        self.folder.rmdir()
        loop = self.loop()
        with self.assertRaises(StreamRefused) as caught:
            loop.start()
        self.assertIn(str(self.folder), str(caught.exception))
        self.assertIn("--backfill-from", str(caught.exception))

    def test_a_partly_delivered_file_resumes_exactly_where_it_stopped_after_a_crash(self):
        write_stream(self.folder, FRIDAY, session_envelopes([date(2027, 1, 12), date(2027, 1, 13), FRIDAY]))

        class StopsAtThird:
            def __init__(self, engine):
                self.engine, self.n = engine, 0

            def __call__(self):
                return self

            def decide(self, envelope):
                self.n += 1
                if self.n >= 3:
                    raise EngineUnavailable("synthetic: the engine stopped")
                return self.engine.decide(envelope)
        loop = self.loop(engine=StopsAtThird(self.engine))
        loop.start()
        with self.assertRaises(EngineUnavailable):
            self.settle(loop)
        self.assertEqual(len(self.ids()), 2)
        loop.close()
        restarted = self.loop()
        restarted.start()
        self.settle(restarted)
        self.assertEqual(self.types(), [BROKER_SNAPSHOT_TYPE] + [BAR, CLOSED] * 3)
        self.assertEqual(len(set(self.ids())), 7)
        self.assertEqual([e["sequence"] for e in self.engine.inputs()], list(range(2, 9)))

    def test_a_crash_after_the_record_and_before_the_outbox_write_delivers_once_from_a_fresh_snapshot(self):
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        with mock.patch.object(Outbox, "extend", side_effect=OSError("synthetic: the machine stopped")):
            with self.assertRaises(OSError):
                loop.step()
        self.assertEqual(loop.outbox.load(), [])
        self.assertTrue(Path(self.pending + DELIVERED_SUFFIX).exists(), "the record is written before the outbox")
        loop.close()
        restarted = self.loop()
        restarted.start()
        self.settle(restarted)
        self.assertEqual(self.types(), [BROKER_SNAPSHOT_TYPE, BAR, CLOSED])

    def test_a_crash_after_the_outbox_write_and_before_the_record_closes_queues_nothing_twice(self):
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        import delivery
        with mock.patch.object(delivery.DeliveryRecord, "mark_queued",
                               side_effect=OSError("synthetic: the machine stopped")):
            with self.assertRaises(OSError):
                loop.step()
        self.assertEqual(len(loop.outbox.load()), 3)
        loop.close()
        restarted = self.loop()
        restarted.start()
        self.settle(restarted)
        self.assertEqual(self.types(), [BROKER_SNAPSHOT_TYPE, BAR, CLOSED])
        self.assertEqual(len(self.fake.called("req_mkt_data")), 1, "one snapshot: the one recorded")

    def test_a_crash_after_the_engine_committed_and_before_the_reply_asks_again_under_the_same_id(self):
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop(engine=Unanswered(self.engine, commits=True))
        loop.start()
        with self.assertRaises(EngineUnavailable):
            loop.step()
        waiting = PendingInput(self.pending).load()
        self.assertEqual(waiting["type"], BROKER_SNAPSHOT_TYPE)
        loop.close()
        restarted = self.loop()
        restarted.start()
        self.settle(restarted)
        # The re-delivery is the committed-reply request: the same envelope,
        # byte for byte, answered from the log and applied once.
        self.assertIn(waiting, self.engine.received[1:])
        self.assertEqual(self.types(), [BROKER_SNAPSHOT_TYPE, BAR, CLOSED])
        self.assertEqual(read_engine_log(self.engine_log).next_sequence, 5)

    def test_a_file_of_another_run_is_refused_naming_it_and_nothing_is_sent(self):
        for identity in ({"run": "run-other"}, {"configuration_hash": "sha256:" + "2" * 64},
                         {"strategy_version": "target-weight-boost/0.9.0"}):
            with self.subTest(**identity):
                for p in self.folder.iterdir():
                    p.unlink()
                path = write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY], **identity))
                loop = self.loop()
                loop.start()
                with self.assertRaises(StreamRefused) as caught:
                    self.settle(loop)
                loop.close()
                self.assertIn(str(path), str(caught.exception))
                self.assertIn("not this run's", str(caught.exception))
                self.assertEqual(self.types(), [])
                self.assertEqual(Outbox(self.pending + ".outbox").load(), [])

    def test_a_file_that_fails_the_strict_read_is_refused_and_nothing_from_it_is_sent(self):
        good = session_envelopes([FRIDAY])
        bad = {
            "not JSON": lambda lines: lines[:1] + ["{not json"],
            "a line that is not an object": lambda lines: lines[:1] + ["[1]"],
            "a missing field": lambda lines: [json.dumps({k: v for k, v in good[0].items() if k != "source"})]
            + lines[1:],
            "an unknown field": lambda lines: [json.dumps(dict(good[0], extra=1))] + lines[1:],
            "a sequence that skips": lambda lines: [lines[0], json.dumps(dict(good[1], sequence=3))],
            "a sequence not from 1": lambda lines: [json.dumps(dict(e, sequence=n + 2)) for n, e in enumerate(good)],
            "a payload that does not match its hash": lambda lines: [lines[0], json.dumps(dict(
                good[1], payload={"period_end": "2027-01-14T21:00:00Z", "instrument_ids": []}))],
            "an ID twice": lambda lines: [lines[0], json.dumps(dict(good[0], sequence=2))],
            "no envelopes": lambda lines: [],
            "a torn last line": lambda lines: [lines[0], lines[1][:20]],
        }
        for why, mangle in bad.items():
            with self.subTest(why):
                for p in self.folder.iterdir():
                    p.unlink()
                lines = mangle([json.dumps(e, separators=(",", ":")) for e in good])
                path = self.folder / "boost-{}.jsonl".format(FRIDAY)
                path.write_text("".join(line + "\n" for line in lines) if why != "a torn last line"
                                else lines[0] + "\n" + lines[1])
                loop = self.loop()
                loop.start()
                with self.assertRaises(StreamRefused) as caught:
                    self.settle(loop)
                loop.close()
                self.assertIn(str(path), str(caught.exception))
                self.assertEqual(self.types(), [])
                self.assertEqual(Outbox(self.pending + ".outbox").load(), [])

    def test_a_file_older_than_one_already_delivered_is_refused(self):
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        self.settle(loop)
        late = write_stream(self.folder, date(2027, 1, 13), session_envelopes([date(2027, 1, 13)]))
        with self.assertRaises(StreamRefused) as caught:
            self.settle(loop)
        self.assertIn(str(late), str(caught.exception))
        self.assertEqual(self.types(), [BROKER_SNAPSHOT_TYPE, BAR, CLOSED])

    def test_nothing_is_recorded_or_sent_when_the_link_drops_during_the_snapshot_and_all_of_it_goes_once_back(self):
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        self.fake.drop_on_mark = True
        loop.step()
        self.assertEqual(self.types(), [])
        self.assertEqual(loop.outbox.load(), [])
        self.assertFalse(Path(self.pending + DELIVERED_SUFFIX).exists())
        self.fake.drop_on_mark = False
        self.settle(loop)
        self.assertEqual(self.types(), [BROKER_SNAPSHOT_TYPE, BAR, CLOSED])
        self.assertEqual(self.engine.inputs()[0]["payload"]["unreadable"], [])

    def test_the_delivery_record_is_read_strictly(self):
        for text in ("{not json", "[]", json.dumps({"batches": [], "extra": 1}),
                     json.dumps({"batches": [{"run": RUN, "snapshot": "s", "files": [], "state": "queued"}]}),
                     json.dumps({"batches": [{"run": RUN, "snapshot": "s", "state": "maybe",
                                              "files": [{"name": "boost-2027-01-14.jsonl", "sha256": "0" * 64,
                                                         "lines": 2}]}]})):
            with self.subTest(text):
                Path(self.pending + DELIVERED_SUFFIX).write_text(text)
                loop = self.loop()
                with self.assertRaises(DeliveryRecordInvalid):
                    loop.start()
                    loop.step()
                loop.close()
                self.assertEqual(self.types(), [])

    def test_an_earlier_runs_stream_left_in_the_outbox_is_struck_off_unsent(self):
        # The owner started a fresh run before the last run's stream was
        # sent: the new run's own stream carries its history again.
        loop = self.loop()
        stale = session_envelopes([FRIDAY], run="run-earlier")[0]
        loop.outbox.extend([{"id": stale["id"], "event_type": stale["type"], "schema_version": 2,
                             "event_time": stale["event_time"], "payload": stale["payload"], "envelope": stale}])
        loop.start()
        self.settle(loop)
        self.assertEqual(self.types(), [])
        self.assertEqual(loop.outbox.load(), [])

    def test_an_earlier_runs_batch_a_stop_left_unqueued_is_dropped_and_this_runs_files_go(self):
        # The adapter stopped between recording an earlier run's batch and
        # its outbox write, and the owner then started a fresh run: nothing
        # of that batch was ever recorded or sent, and it is never sent now.
        Path(self.pending + DELIVERED_SUFFIX).write_text(json.dumps({"batches": [{
            "run": "run-earlier", "snapshot": "ibkr:account.broker.snapshot:old", "state": "queuing",
            "files": [{"name": "boost-2027-01-13.jsonl", "sha256": "0" * 64, "lines": 2}]}]}))
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        self.settle(loop)
        self.assertEqual(self.types(), [BROKER_SNAPSHOT_TYPE, BAR, CLOSED])
        batches = json.loads(Path(self.pending + DELIVERED_SUFFIX).read_text())["batches"]
        self.assertEqual([(b["run"], b["state"]) for b in batches], [("run-earlier", "dropped"), (RUN, "queued")])

    def test_a_payload_that_is_not_finite_json_is_refused(self):
        line = json.dumps(session_envelopes([FRIDAY])[0], separators=(",", ":")).replace('"close":400.5',
                                                                                          '"close":NaN')
        path = self.folder / "boost-{}.jsonl".format(FRIDAY)
        path.write_text(line + "\n")
        loop = self.loop()
        loop.start()
        with self.assertRaises(StreamRefused) as caught:
            self.settle(loop)
        self.assertIn(str(path), str(caught.exception))
        self.assertEqual(self.types(), [])

    def test_the_released_order_of_the_days_close_goes_out_once(self):
        released = json.loads((tests_dir / "testdata" / "paper_released.json").read_text())
        self.engine.decisions = {"marketdata:session-closed:{}".format(FRIDAY): [{"type": RELEASED,
                                                                               "payload": released}]}
        self.fake.on_place = self.fake.accept
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        self.settle(loop)
        self.assertEqual(len(self.fake.placed), 1)
        self.assertEqual(self.types()[:3], [BROKER_SNAPSHOT_TYPE, BAR, CLOSED])


class StreamReadTests(unittest.TestCase):
    def test_a_valid_file_is_read_in_order(self):
        with tempfile.TemporaryDirectory() as d:
            written = session_envelopes([FRIDAY])
            path = write_stream(Path(d), FRIDAY, written)
            digest = hashlib.sha256(path.read_bytes()).hexdigest()
            stream = read_stream(str(path), RUN, CONFIGURATION_HASH, STRATEGY_VERSION)
        self.assertEqual(stream.envelopes, written)
        self.assertEqual((stream.name, stream.session, stream.sha256), (path.name, FRIDAY, digest))


class MarketOpenTests(unittest.TestCase):
    def test_open_only_from_the_open_to_the_close_of_an_nyse_session(self):
        cases = [
            (datetime(2027, 1, 14, 14, 29, 59, tzinfo=timezone.utc), False),   # 09:29:59 New York
            (datetime(2027, 1, 14, 14, 30, tzinfo=timezone.utc), True),        # the open
            (datetime(2027, 1, 14, 20, 59, 59, tzinfo=timezone.utc), True),
            (datetime(2027, 1, 14, 21, 0, tzinfo=timezone.utc), False),        # the close
            (datetime(2027, 1, 16, 15, 0, tzinfo=timezone.utc), False),        # a Saturday
            (datetime(2026, 11, 26, 15, 0, tzinfo=timezone.utc), False),       # Thanksgiving
            (datetime(2026, 11, 27, 17, 59, tzinfo=timezone.utc), True),       # the early close day, 12:59
            (datetime(2026, 11, 27, 18, 0, tzinfo=timezone.utc), False),       # its 1 p.m. close
            (datetime(2026, 7, 14, 13, 45, tzinfo=timezone.utc), True),        # summer time: 09:45 New York
        ]
        for at, want in cases:
            with self.subTest(at=at):
                self.assertIs(market_open(at), want)


if __name__ == "__main__":
    unittest.main()
