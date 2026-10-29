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
import contextlib
import hashlib
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from datetime import date, datetime, timedelta, timezone
from pathlib import Path
from unittest import mock

tests_dir = Path(__file__).resolve().parent
# The longest any `go build`, `go run` or boost-demo run may take here, in
# seconds (TI_TEST_GO_TIMEOUT): generous, so a cold runner passes; a process
# that outlasts it is killed and the test fails with what it printed. The
# engine's own start and stop are bounded by ENGINE_TIMEOUT.
GO_TIMEOUT = float(os.environ.get("TI_TEST_GO_TIMEOUT", "300"))
# How long a real engine may take to listen, and to stop (TI_TEST_ENGINE_TIMEOUT).
ENGINE_TIMEOUT = float(os.environ.get("TI_TEST_ENGINE_TIMEOUT", "120"))
if str(tests_dir) not in sys.path:
    sys.path.insert(0, str(tests_dir))

from decimal import Decimal

from engine_double import CONFIGURATION_HASH, STRATEGY_VERSION, LoggedEngine, run_approval
from fake_tws import ACCOUNT, FakeTWS, contract, execution
from test_connection import Clock, config
from test_loop import COMMISSION_WAIT, RECEIVED, SUMMARY, Ids, Unanswered
from delivery import (DELIVERED_SUFFIX, DeliveryRecordInvalid, SnapshotRefused, StreamRefused, market_open,
                      read_stream)
from loop import BROKER_SNAPSHOT_ATTEMPTS, AdapterLoop, Outbox, ReconcileIncomplete, ReconcileMismatch
from relay import RELEASED, EngineUnavailable, OutboxInvalid, PendingInput, read_engine_log
from reports import FILL_TYPE, LIFECYCLE_TYPE, UnreportableOrder
from snapshot import BROKER_SNAPSHOT_TYPE

RUN = "engine"
BAR = "market.bar.completed"
CLOSED = "market.session.closed"
ACCOUNT_SNAPSHOT = "account.snapshot"
MARK = "market.mark.recorded"
RECONCILIATION = "account.reconciliation"
# What one account read puts ahead of a batch's lines: the engine's account
# snapshot and the broker snapshot, and, in a run's first batch (the engine
# holding no book yet), the start-of-run reconciliation; and the last
# session of a batch, its mark between its bar and its close (the owner's
# decision of 2026-10-18; delivery.py).
HEAD = [ACCOUNT_SNAPSHOT, BROKER_SNAPSHOT_TYPE]
FIRST = HEAD + [RECONCILIATION]
# A later batch: the reading's reconciliation against the engine's book
# first, then the snapshots that replace it (ADR 0019).
LATER = [RECONCILIATION] + HEAD
DAY = [BAR, MARK, CLOSED]
SPY_CON_ID = 756733
# The fake's server time is 2027-01-15 08:00 UTC, before the open in New York.
FRIDAY = date(2027, 1, 14)
# The account's first run is approved with the book the owner checked in
# IBKR, what the fake shows, so its first batch is compared with it (ADR 0019
# and ADR 0027, amended 2026-10-18).
FIRST_RUN = run_approval(ACCOUNT, starting_book=({"SPY": 147}, 100000.0))


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


def commit(engine, ident, kind="test.input", payload=None, version=1):
    """An input committed to engine's durable log under ident, as the engine commits one."""
    payload = payload or {"input": ident}
    raw = json.dumps(payload, separators=(",", ":"))
    engine.decide({"id": ident, "type": kind, "envelope_version": 1, "schema_version": version,
                   "event_time": "2027-01-14T22:05:00Z", "recorded_at": "2027-01-14T22:05:00Z",
                   "sequence": engine.next_sequence, "source": "engine",
                   "configuration_hash": engine.configuration_hash, "strategy_version": engine.strategy_version,
                   "payload": payload, "payload_hash": hashlib.sha256(raw.encode()).hexdigest()})


def commit_bar(engine, ident):
    """A market input as the data job writes one, committed as a stream's copy in a resumed log holds it."""
    commit(engine, ident, kind="market.bar.completed", version=2)


def approve(engine, previous_log):
    """The run's Run approval committed first, naming previous_log, as cmd/engine records a fresh run's."""
    lines = Path(previous_log).read_text(encoding="utf-8").splitlines()
    final = [json.loads(line)["commit"]["record_hash"] for line in lines if line.startswith('{"commit"')][-1]
    commit(engine, "operator-run-approved:" + final, kind="operator.run.approved", payload={
        "account_id": ACCOUNT, "previous_log": Path(previous_log).name, "previous_log_final_hash": final,
        "incident_id": "", "approved_by": "owner", "approved_at": "2027-01-14T14:00:00Z",
        "boost": {"latched": False}, "incident_in_force": None})


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
        self.engine = LoggedEngine(self.engine_log, approval=FIRST_RUN)
        self.fake = FakeTWS()
        self.fake.summary = list(SUMMARY)
        self.fake.positions = [(ACCOUNT, contract(), Decimal("147"), 312.5)]
        self.fake.marks = {SPY_CON_ID: 401.17}
        self.new_id = Ids()  # one sequence of input IDs across restarts, as uuid4 gives

    def loop(self, engine=None, fake=None, streams=True, engine_log=None, now=RECEIVED):
        clock = Clock()

        gateway = fake or self.fake

        def sleep(seconds):
            clock.now += seconds
            gateway.waited()
        loop = AdapterLoop(config(mode="paper", paper_sending_enabled=True, commission_wait=COMMISSION_WAIT),
                           gateway, engine or self.engine, self.pending, engine_log or self.engine_log,
                           now=now if callable(now) else (lambda: now), today=lambda: FRIDAY, sleep=sleep, monotonic=clock, poll=0,
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

    def brokers(self):
        return self.engine.inputs(BROKER_SNAPSHOT_TYPE)

    def test_a_stream_file_reaches_the_engine_after_the_broker_snapshot_in_file_order_as_written(self):
        written = session_envelopes([date(2027, 1, 13), FRIDAY])
        write_stream(self.folder, FRIDAY, written)
        loop = self.loop()
        loop.start()
        self.settle(loop)
        self.assertEqual(self.types(), FIRST + [BAR, CLOSED] + DAY)
        sent = self.engine.inputs()
        # The engine's sequence, set as each is sent: its configuration is 1.
        self.assertEqual([e["sequence"] for e in sent], list(range(3, 11)), "after the configuration and the approval")
        for want, got in zip(written, [e for e in sent if e["source"] == "marketdata"]):
            self.assertEqual({k: v for k, v in got.items() if k not in ("sequence", "recorded_at")},
                             {k: v for k, v in want.items() if k not in ("sequence", "recorded_at")},
                             "sent as written")
        snapshot = self.brokers()[0]["payload"]
        self.assertEqual(snapshot["account_id"], ACCOUNT)
        # Read on a later New York date, the snapshot is stamped at its read
        # time and is IBKR's own reading: the holding is marked at IBKR's
        # last price, the basis of its net liquidation (ADR 0019, amended
        # 2026-10-18: one price basis per rule).
        self.assertFalse(snapshot["as_of_close"])
        self.assertEqual(snapshot["positions"], [{"instrument_id": "SPY", "quantity": 147, "mark": 401.17,
                                                  "mark_set": True}])
        self.assertIs(snapshot["market_open"], False, "08:00 UTC is before the open")
        self.assertEqual(snapshot["unreadable"], [])
        self.assertEqual(self.brokers()[0]["event_time"], snapshot["as_of"])
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

    def test_a_holding_ibkr_gives_no_last_price_for_is_still_marked_at_the_official_close(self):
        # A reading as of the close needs no IBKR market-data permission (the
        # owner's decision of 2026-10-18): it is marked at the official close.
        self.fake.server_time = int(datetime(2027, 1, 14, 22, 5, tzinfo=timezone.utc).timestamp())
        self.fake.marks = {}
        self.fake.mark_errors = {SPY_CON_ID: (354, "synthetic: requested market data is not subscribed")}
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        self.settle(loop)
        snapshot = self.brokers()[0]["payload"]
        self.assertEqual(snapshot["positions"], [{"instrument_id": "SPY", "quantity": 147, "mark": 400.5,
                                                  "mark_set": True}])

    def test_a_holding_with_no_contract_number_is_stated_without_a_mark_and_not_asked_for(self):
        Path(self.engine_log).unlink()
        self.engine = LoggedEngine(self.engine_log, approval=run_approval(
            ACCOUNT, starting_book=({"IEF": 5, "SPY": 147}, 100000.0)))
        self.fake.positions.append((ACCOUNT, contract(con_id=8314, symbol="IEF"), Decimal("5"), 95.1))
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        self.settle(loop)
        # IEF has no bar in the stream, so no official close to mark it at.
        positions = self.brokers()[0]["payload"]["positions"]
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
        snapshot = "ibkr:account.broker.snapshot:1"
        self.assertEqual(self.ids(), [
            snapshot + ":account.snapshot", snapshot, snapshot + ":account.reconciliation",
            "marketdata:bar:SPY:2027-01-12", "marketdata:session-closed:2027-01-12",
            "marketdata:bar:SPY:2027-01-13", snapshot + ":market.mark.recorded:SPY",
            "marketdata:session-closed:2027-01-13"])
        loop.close()
        again = self.loop()
        again.start()
        self.settle(again)
        # The restart reconciles against the engine's book; nothing of the
        # stream goes twice.
        self.assertEqual(self.types()[8:], [RECONCILIATION], "nothing delivered twice across a restart")
        self.fake.server_time += 3600
        write_stream(self.folder, third, [dict(e, sequence=n) for n, e in enumerate(envelopes[4:], 1)])
        self.settle(again)
        self.assertEqual(self.types()[9:], LATER + DAY)
        snapshots = self.engine.inputs(BROKER_SNAPSHOT_TYPE)
        self.assertLess(snapshots[0]["payload"]["as_of"], snapshots[1]["payload"]["as_of"])

    def test_files_that_are_not_streams_are_left_alone(self):
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        (self.folder / "tmpab12.part").write_text("{not json")
        (self.folder / "README").write_text("synthetic")
        loop = self.loop()
        loop.start()
        self.settle(loop)
        self.assertEqual(self.types(), FIRST + DAY)

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
        # The restart's reconciliation, against the book the engine now
        # holds, goes ahead of the stream's next line.
        self.assertEqual(self.types(), FIRST + [RECONCILIATION] + [BAR, CLOSED] * 2 + DAY)
        self.assertEqual(len(set(self.ids())), 11)
        self.assertEqual([e["sequence"] for e in self.engine.inputs()], list(range(3, 14)))

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
        self.assertEqual(self.types(), FIRST + DAY)

    def test_a_crash_after_the_outbox_write_and_before_the_record_closes_queues_nothing_twice(self):
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        import delivery
        with mock.patch.object(delivery.DeliveryRecord, "mark_queued",
                               side_effect=OSError("synthetic: the machine stopped")):
            with self.assertRaises(OSError):
                loop.step()
        self.assertEqual(len(loop.outbox.load()), 6)
        loop.close()
        restarted = self.loop()
        restarted.start()
        self.settle(restarted)
        self.assertEqual(self.types(), FIRST + DAY)
        self.assertEqual(len(self.fake.called("req_mkt_data")), 1, "one snapshot: the one recorded")

    def test_a_crash_before_the_record_closes_on_a_reading_never_checked_queues_nothing_twice(self):
        # The reading is not the starting book, so the batch's
        # reconciliation goes alone, its snapshots withheld (delivery.py):
        # the reconciliation, not the broker snapshot, shows it was queued.
        Path(self.engine_log).unlink()
        self.engine = LoggedEngine(self.engine_log, approval=run_approval(
            ACCOUNT, starting_book=({"SPY": 100}, 100000.0)))
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        import delivery
        with mock.patch.object(delivery.DeliveryRecord, "mark_queued",
                               side_effect=OSError("synthetic: the machine stopped")):
            with self.assertRaises(OSError):
                loop.step()
        self.assertEqual(len(loop.outbox.load()), 4)
        loop.close()
        restarted = self.loop()
        restarted.start()
        self.settle(restarted)
        self.assertEqual(self.types(), [RECONCILIATION] + DAY)

    def test_a_crash_after_the_engine_committed_and_before_the_reply_asks_again_under_the_same_id(self):
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop(engine=Unanswered(self.engine, commits=True))
        loop.start()
        with self.assertRaises(EngineUnavailable):
            loop.step()
        waiting = PendingInput(self.pending).load()
        self.assertEqual(waiting["type"], ACCOUNT_SNAPSHOT)
        loop.close()
        restarted = self.loop()
        restarted.start()
        self.settle(restarted)
        # The re-delivery is the committed-reply request: the same envelope,
        # byte for byte, answered from the log and applied once.
        self.assertIn(waiting, self.engine.received[1:])
        # The engine holds the account snapshot, its book, so the restart
        # reconciles too, ahead of the stream's lines.
        self.assertEqual(self.types(), FIRST + [RECONCILIATION] + DAY)
        self.assertEqual(read_engine_log(self.engine_log).next_sequence, 10)

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
        self.assertEqual(self.types(), FIRST + DAY)

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
        self.assertEqual(self.types(), FIRST + DAY)
        self.assertEqual(self.brokers()[0]["payload"]["unreadable"], [])

    def test_the_delivery_record_is_read_strictly(self):
        for text in ("{not json", "[]", json.dumps({"batches": [], "extra": 1}),
                     json.dumps({"batches": [{"run": RUN, "snapshot": "s", "files": [], "state": "queued"}]}),
                     json.dumps({"batches": [{"run": RUN, "snapshot": "s", "state": "maybe",
                                              "files": [{"name": "boost-2027-01-14.jsonl", "sha256": "0" * 64,
                                                         "lines": 2}]}]}),
                     json.dumps({"batches": [{"run": RUN, "snapshot": "s", "state": "queued",
                                              "journal_checked": "something-else",
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

    def test_every_batch_is_recorded_as_journal_checked_and_an_earlier_adapters_batch_is_read_unmarked(self):
        # A batch this adapter records ran the journal check of the close window (ADR 0019, amended 2026-10-28); an
        # earlier adapter's carries no mark, is still read, and bounds nothing (loop._journal_fills).
        from delivery import DeliveryRecord, JOURNAL_CHECKED
        record = DeliveryRecord(self.pending + DELIVERED_SUFFIX)
        record.begin({"run": RUN, "snapshot": "legacy", "files": [{"name": "boost-2027-01-13.jsonl",
                                                                  "sha256": "0" * 64, "lines": 1}]})
        record.mark_queued()
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        self.settle(loop)
        batches = record.load()
        self.assertNotIn("journal_checked", batches[0])
        self.assertEqual(batches[-1]["journal_checked"], JOURNAL_CHECKED)
        self.assertEqual(loop.delivery.journal_checked_snapshots(), frozenset([batches[-1]["snapshot"]]))

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
        self.assertEqual(self.types(), FIRST + DAY)
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

    # -- one batch, read whole before anything is written ------------------
    def assert_refused_and_nothing_recorded(self, path):
        loop = self.loop()
        try:
            loop.start()
            with self.assertRaises(StreamRefused) as caught:
                self.settle(loop)
        finally:
            loop.close()
        self.assertIn(str(path), str(caught.exception))
        self.assertEqual(self.types(), [])
        self.assertEqual(Outbox(self.pending + ".outbox").load(), [])
        self.assertFalse(Path(self.pending + DELIVERED_SUFFIX).exists(), "nothing of the batch is recorded")
        return caught.exception

    def test_two_files_of_one_session_in_one_batch_are_refused_and_nothing_is_recorded(self):
        envelopes = session_envelopes([FRIDAY])
        write_stream(self.folder, FRIDAY, envelopes)
        copy = write_stream(self.folder, FRIDAY, envelopes, prefix="boost-copy")
        self.assert_refused_and_nothing_recorded(copy)

    def test_files_sharing_an_input_id_in_one_batch_are_refused_before_anything_is_written(self):
        thursday = date(2027, 1, 13)
        first = session_envelopes([thursday])
        write_stream(self.folder, thursday, first)
        second = write_stream(self.folder, FRIDAY, [dict(e, sequence=n) for n, e in enumerate(
            first[:1] + session_envelopes([FRIDAY]), 1)])
        refused = self.assert_refused_and_nothing_recorded(second)
        self.assertIn(first[0]["id"], str(refused))

    def test_a_file_whose_name_is_not_the_session_its_content_closes_is_refused(self):
        cases = {
            "it closes another session": session_envelopes([date(2027, 1, 13)]),
            "it does not end with a session's close": session_envelopes([FRIDAY])[:1],
            "its close has no period_end that can be read": session_envelopes([FRIDAY])[:1] + [_envelope(
                CLOSED, 1, "marketdata:session-closed:2027-01-14", "2027-01-14T21:00:00Z",
                {"period_end": "Friday", "instrument_ids": ["SPY"]}, sequence=2)],
        }
        for why, envelopes in cases.items():
            with self.subTest(why):
                for p in self.folder.iterdir():
                    p.unlink()
                Path(self.pending + DELIVERED_SUFFIX).unlink(missing_ok=True)
                path = write_stream(self.folder, FRIDAY, envelopes)
                self.assert_refused_and_nothing_recorded(path)

    def test_an_event_type_schema_version_or_source_the_data_job_does_not_write_is_refused(self):
        good = session_envelopes([FRIDAY])
        cases = {
            "an unknown type": dict(good[0], type="market.bar.unknown"),
            "an unknown schema version": dict(good[0], schema_version=999),
            "another type's version": dict(good[1], schema_version=2),
            "another source": dict(good[0], source="ibkr"),
        }
        for why, line in cases.items():
            with self.subTest(why):
                for p in self.folder.iterdir():
                    p.unlink()
                envelopes = [line, good[1]] if line["id"] == good[0]["id"] else [good[0], line]
                path = write_stream(self.folder, FRIDAY, envelopes)
                self.assert_refused_and_nothing_recorded(path)

    # -- the snapshot's time against its trigger's ----------------------------
    def test_every_stream_input_is_recorded_at_its_send_time_never_before_its_snapshot(self):
        # The data job wrote the file at 22:05 UTC; IBKR's clock reads the
        # snapshot at 08:00 UTC the next day, and the adapter sends at 08:30.
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        sending = datetime(2027, 1, 15, 8, 30, tzinfo=timezone.utc)
        loop = self.loop(now=sending)
        loop.start()
        self.settle(loop)
        sent = self.engine.inputs()
        self.assertEqual(self.types(), FIRST + DAY)
        self.assertEqual([e["recorded_at"] for e in sent], ["2027-01-15T08:30:00Z"] * 6)
        as_of = self.brokers()[0]["payload"]["as_of"]
        for e in sent[2:]:
            self.assertLessEqual(as_of, e["recorded_at"], "the snapshot is never after the input it serves")

    # -- an earlier run's batch ---------------------------------------------
    def queue_unsent(self):
        """This run's batch recorded in the outbox, and the adapter stopped before sending any of it."""
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        with mock.patch.object(AdapterLoop, "_drain"):
            loop.step()
        loop.close()
        self.assertEqual(len(loop.outbox.load()), 6)

    def fresh_run(self, name="run-b"):
        """A fresh run after this one: its own Run approval, so its own identity, and its warm-up's folder."""
        log = str(Path(self.dir.name) / (name + ".log"))
        engine = LoggedEngine(log)
        approve(engine, self.engine_log)
        write_stream(self.streams / name, FRIDAY, session_envelopes([FRIDAY], run=name))
        return log, engine

    def test_an_earlier_runs_whole_batch_left_in_the_outbox_never_reaches_a_fresh_run(self):
        self.queue_unsent()
        log, engine = self.fresh_run()
        fresh = self.loop(engine=engine, engine_log=log)
        fresh.start()
        self.settle(fresh)
        sent = engine.inputs()[1:]  # after its Run approval
        self.assertEqual([e["type"] for e in sent], FIRST + DAY)
        self.assertEqual([e["id"] for e in sent if e["source"] == "marketdata"],
                         ["marketdata:bar:SPY:2027-01-14", "marketdata:session-closed:2027-01-14"])
        self.assertEqual(len(engine.inputs(BROKER_SNAPSHOT_TYPE)), 1, "only the fresh run's own snapshot")
        self.assertEqual(self.types(), [], "nothing reached the earlier run either")
        self.assertEqual(fresh.outbox.load(), [])

    def test_a_resumed_run_gets_the_files_a_fresh_run_struck_off_unsent(self):
        self.queue_unsent()
        log, engine = self.fresh_run()
        fresh = self.loop(engine=engine, engine_log=log)
        fresh.start()
        self.settle(fresh)
        fresh.close()
        resumed = self.loop()
        resumed.start()
        self.settle(resumed)
        self.assertEqual(self.types(), FIRST + DAY)
        batches = json.loads(Path(self.pending + DELIVERED_SUFFIX).read_text())["batches"]
        self.assertEqual([(b["run"], b["state"]) for b in batches],
                         [(RUN, "dropped"), ("run-b", "queued"), (RUN, "queued")])

    def test_a_resumed_run_gets_the_files_of_its_batch_a_fresh_run_dropped_unqueued(self):
        earlier = "run-earlier"
        path = write_stream(self.streams / earlier, date(2027, 1, 13), session_envelopes([date(2027, 1, 13)],
                                                                                           run=earlier))
        Path(self.pending + DELIVERED_SUFFIX).write_text(json.dumps({"batches": [{
            "run": earlier, "snapshot": "ibkr:account.broker.snapshot:old", "state": "queuing",
            "files": [{"name": path.name, "sha256": hashlib.sha256(path.read_bytes()).hexdigest(), "lines": 2}]}]}))
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        self.settle(loop)
        loop.close()
        log = str(Path(self.dir.name) / (earlier + ".log"))
        # The earlier run: another as-of time, so another identity.
        engine = LoggedEngine(log, started_at="2027-01-13T13:30:00Z", approval=FIRST_RUN)
        resumed = self.loop(engine=engine, engine_log=log)
        resumed.start()
        self.settle(resumed)
        self.assertEqual([e["type"] for e in engine.inputs()], FIRST + DAY)
        self.assertEqual(engine.inputs()[3]["id"], "marketdata:bar:SPY:2027-01-13")

    # -- a crash resume and a fresh run ------------------------------------
    def crash_resume(self, name="run-c"):
        """The crashed run resumed (engine -resume) under a new TI_RUN: a new log holding the crashed log's copy."""
        log = str(Path(self.dir.name) / (name + ".log"))
        return log, LoggedEngine.resumed(self.engine_log, log)

    def records(self):
        return [(b["run"], b["state"]) for b in json.loads(Path(self.pending + DELIVERED_SUFFIX).read_text())["batches"]]

    def test_a_crash_resume_keeps_the_saved_batch_and_delivers_the_rest_of_it_once(self):
        write_stream(self.folder, FRIDAY, session_envelopes([date(2027, 1, 12), date(2027, 1, 13), FRIDAY]))

        class StopsAtThird:
            def __init__(self, engine):
                self.engine, self.n = engine, 0

            def __call__(self):
                return self

            def decide(self, envelope):
                self.n += 1
                if self.n >= 3:
                    raise EngineUnavailable("synthetic: the engine crashed")
                return self.engine.decide(envelope)
        loop = self.loop(engine=StopsAtThird(self.engine))
        loop.start()
        with self.assertRaises(EngineUnavailable):
            self.settle(loop)
        loop.close()
        first = self.ids()
        self.assertEqual(self.types(), HEAD, "the account and broker snapshots")
        log, engine = self.crash_resume()
        reads = len(self.fake.called("req_mkt_data"))
        resumed = self.loop(engine=engine, engine_log=log)
        resumed.start()
        self.settle(resumed)
        rest = [e["id"] for e in engine.inputs()]
        # The batch's own reconciliation, then the resume's against the book
        # the copied log holds, ahead of the stream's next line.
        self.assertEqual([e["type"] for e in engine.inputs()],
                         [RECONCILIATION, RECONCILIATION, BAR, CLOSED, BAR, CLOSED] + DAY)
        self.assertEqual([e["id"] for e in engine.inputs() if e["source"] == "marketdata"], [
            e["id"] for e in session_envelopes([date(2027, 1, 12), date(2027, 1, 13), FRIDAY])])
        self.assertEqual(len(set(first + rest)), 11, "each input once, across the crash")
        self.assertEqual([e["sequence"] for e in engine.inputs()], list(range(5, 14)))
        self.assertEqual(len(self.fake.called("req_mkt_data")), reads, "no second snapshot: the batch continues")
        self.assertEqual(resumed.outbox.load(), [])
        self.assertEqual(self.records(), [(RUN, "queued")], "the batch is the resumed run's, never dropped")
        self.assertFalse((self.streams / "run-c").exists(), "the resumed run keeps the first run's folder")

    def test_a_crash_resume_after_every_input_was_committed_sends_nothing_again(self):
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))

        class CrashesAfterTheLast:
            def __init__(self, engine):
                self.engine, self.n = engine, 0

            def __call__(self):
                return self

            def decide(self, envelope):
                self.n += 1
                if self.n > len(FIRST + DAY):
                    raise EngineUnavailable("synthetic: the engine is down")
                reply = self.engine.decide(envelope)
                if self.n == len(FIRST + DAY):
                    raise EngineUnavailable("synthetic: the engine crashed after committing, before replying")
                return reply
        loop = self.loop(engine=CrashesAfterTheLast(self.engine))
        loop.start()
        with self.assertRaises(EngineUnavailable):
            self.settle(loop)
        loop.close()
        self.assertEqual(self.types(), FIRST + DAY)
        self.assertEqual(len(loop.outbox.load()), 1, "the close is committed but not yet struck off")
        log, engine = self.crash_resume()
        reads = len(self.fake.called("req_mkt_data"))
        resumed = self.loop(engine=engine, engine_log=log)
        resumed.start()
        self.settle(resumed)
        # Only the resume's reconciliation, against the copied book, is new.
        self.assertEqual([e["type"] for e in engine.inputs()], [RECONCILIATION], "nothing is committed twice")
        self.assertEqual([e["id"] for e in engine.received][:1], ["marketdata:session-closed:2027-01-14"],
                         "only the input awaiting its reply is asked for again, answered from the copied log")
        self.assertEqual(len(self.fake.called("req_mkt_data")), reads, "no new snapshot")
        self.assertEqual(resumed.outbox.load(), [])
        self.assertEqual(self.records(), [(RUN, "queued")])
        # The next day's file goes to the resumed run from the same folder.
        write_stream(self.folder, date(2027, 1, 15), session_envelopes([date(2027, 1, 15)]))
        self.settle(resumed)
        self.assertEqual([e["type"] for e in engine.inputs()], [RECONCILIATION] + LATER + DAY)
        self.assertEqual(self.records(), [(RUN, "queued"), (RUN, "queued")])

    def test_a_fresh_run_drops_the_earlier_runs_batch_and_needs_its_own_warm_up_folder(self):
        self.queue_unsent()
        log = str(Path(self.dir.name) / "run-b.log")
        # A fresh run after the owner's approval: a new identity.
        engine = LoggedEngine(log, started_at="2027-01-15T13:30:00Z", approval=FIRST_RUN)
        fresh = self.loop(engine=engine, engine_log=log)
        with self.assertRaises(StreamRefused) as caught:
            fresh.start()
        self.assertIn(str(self.streams / "run-b"), str(caught.exception))
        fresh.close()
        write_stream(self.streams / "run-b", FRIDAY, session_envelopes([FRIDAY], run="run-b"))
        fresh = self.loop(engine=engine, engine_log=log)
        fresh.start()
        self.settle(fresh)
        self.assertEqual([e["type"] for e in engine.inputs()], FIRST + DAY)
        self.assertEqual(self.types(), [])
        self.assertEqual(self.records(), [(RUN, "dropped"), ("run-b", "queued")])

    def test_a_log_beside_another_stream_folder_that_cannot_be_read_stops_the_start_with_a_warning(self):
        (self.streams / "run-x").mkdir()
        Path(self.dir.name, "run-x.log").write_text("not a durable log\n")
        loop = self.loop()
        with self.assertLogs("ibkr", level="WARNING") as logs, self.assertRaises(StreamRefused) as caught:
            loop.start()
        self.assertIn(str(self.streams / "run-x"), "\n".join(logs.output))
        self.assertIn("cannot be matched to any run", str(caught.exception))

    def test_two_stream_folders_of_one_run_stop_the_start(self):
        log, _ = self.crash_resume()
        other = str(Path(self.dir.name) / "run-d.log")
        Path(other).write_bytes(Path(self.engine_log).read_bytes())
        (self.streams / "run-d").mkdir()
        loop = self.loop(engine_log=log)
        with self.assertRaises(StreamRefused) as caught:
            loop.start()
        self.assertIn(str(self.folder), str(caught.exception))
        self.assertIn(str(self.streams / "run-d"), str(caught.exception))

    def test_a_resume_with_a_folder_of_its_own_name_and_the_first_runs_stops_the_start(self):
        commit(self.engine, "input-2")
        log, _ = self.crash_resume()
        (self.streams / "run-c").mkdir()
        loop = self.loop(engine_log=log)
        with self.assertRaises(StreamRefused) as caught:
            loop.start()
        self.assertIn(str(self.folder), str(caught.exception))
        self.assertIn(str(self.streams / "run-c"), str(caught.exception))

    def test_an_unapproved_second_start_of_the_first_run_never_takes_its_folder(self):
        # A first run started without an approval (a dry run's), named by its
        # configuration and as-of time.
        Path(self.engine_log).unlink()
        self.engine = LoggedEngine(self.engine_log)
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        self.settle(loop)
        loop.close()
        # The same configuration and as-of time, no Run approval: not a
        # resume (its log holds no copy of the first run's inputs).
        log = str(Path(self.dir.name) / "run-b.log")
        engine = LoggedEngine(log)
        again = self.loop(engine=engine, engine_log=log)
        with self.assertRaises(StreamRefused) as caught:
            again.start()
        self.assertIn(self.engine_log, str(caught.exception))
        self.assertIn(log, str(caught.exception))
        self.assertEqual(engine.inputs(), [])

    def test_the_folder_is_found_from_the_log_only_once_the_engine_listens_after_its_copy(self):
        # An approved fresh run crashed; its resume is still copying its log.
        first, _ = self.fresh_run()
        log = str(Path(self.dir.name) / "run-c.log")
        engine = LoggedEngine.resumed(first, log)
        whole = Path(log).read_bytes()
        # engine -resume is still copying: only the configuration so far.
        Path(log).write_bytes(b"".join(whole.splitlines(keepends=True)[:3]))

        class CopiesThenListens:
            def __call__(self):
                Path(log).write_bytes(whole)
                return engine()
        loop = self.loop(engine=CopiesThenListens(), engine_log=log)
        loop.start()
        self.settle(loop)
        self.assertEqual(loop.delivery.run, "run-b")
        self.assertEqual([e["type"] for e in engine.inputs()], FIRST + DAY)
        self.assertFalse((self.streams / "run-c").exists())

    # -- the account read the snapshot is made from -----------------------
    def test_a_partial_account_read_is_read_again_and_never_delivered_as_the_snapshot(self):
        class PartialOnce(FakeTWS):
            reads = 0

            def req_account_updates(self, subscribe, account_id):
                # The second read (the first is the start's reconciliation)
                # times out: IBKR never ends the account values.
                self.reads += subscribe
                self.end_summary = self.reads != 2
                super().req_account_updates(subscribe, account_id)
        fake = PartialOnce()
        fake.summary, fake.positions, fake.marks = self.fake.summary, self.fake.positions, self.fake.marks
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop(fake=fake)
        loop.start()
        self.settle(loop)
        self.assertEqual(self.types(), FIRST + DAY)
        self.assertEqual(self.brokers()[0]["payload"]["unreadable"], [])
        # The start's reconciliation and the partial read; its values never
        # ended (their end carries no request ID), so the link is made again
        # and reconciled (the third), then a complete read and the read
        # after the marks that confirms nothing changed.
        self.assertEqual(fake.reads, 5)
        self.assertEqual(len(fake.called("connect")), 2)

    def test_an_account_read_that_stays_partial_stops_for_a_person_and_records_nothing(self):
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        self.fake.end_summary = False
        import delivery
        # The values never end: the link is made again and its reconcile
        # finds them unread too, so the adapter stops.
        with self.assertRaises((getattr(delivery, "SnapshotRefused", StreamRefused), ReconcileIncomplete)) as caught:
            self.settle(loop)
        self.assertIn("net-liquidation", str(caught.exception))
        self.assertEqual(self.types(), [])
        self.assertEqual(loop.outbox.load(), [])
        self.assertFalse(Path(self.pending + DELIVERED_SUFFIX).exists())

    def test_the_reconcile_and_both_snapshot_reads_on_one_connection_are_complete_and_delivered(self):
        # The server's dry-run start of 2026-10-25, after a fresh IB Gateway
        # restart: the reconcile's account summary and the snapshot's first
        # were answered, and every later one on the connection was refused
        # with error 322 for minutes, although each had been cancelled (the
        # fake gateway answers two summaries a connection, as IB Gateway
        # 10.50 did), so the adapter stopped (SnapshotRefused). The balances
        # never depend on IBKR honouring a cancel (ADR 0019: a fresh reading).
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        self.settle(loop)
        self.assertEqual(self.types(), FIRST + DAY)
        self.assertEqual(self.brokers()[0]["payload"]["unreadable"], [])
        self.assertEqual(self.fake.called("connect"), [self.fake.calls[0]], "one connection throughout")

    def test_the_snapshots_attempts_are_spaced_and_each_logs_why_the_summary_was_not_read(self):
        class Timed(FakeTWS):
            asked_at = []

            def req_account_updates(self, subscribe, account_id):
                if subscribe:
                    self.asked_at.append(self.clock())
                super().req_account_updates(subscribe, account_id)
        fake = Timed()
        fake.summary, fake.positions, fake.marks = self.fake.summary, self.fake.positions, self.fake.marks
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop(fake=fake)
        fake.clock = loop._monotonic
        loop.start()
        fake.summary = [row for row in fake.summary if row[1] != "ExcessLiquidity"]
        import delivery
        with self.assertLogs("ibkr", level="INFO") as logs, \
                self.assertRaises(getattr(delivery, "SnapshotRefused", StreamRefused)) as caught:
            self.settle(loop)
        attempts = fake.asked_at[1:]  # the first is the start's reconcile
        gaps = [later - earlier for earlier, later in zip(attempts, attempts[1:])]
        self.assertEqual(len(attempts), 4, attempts)
        for gap, wait in zip(gaps, (5, 15, 30)):
            self.assertGreaterEqual(gap, wait, gaps)
        self.assertLessEqual(attempts[-1] - attempts[0], 60, "the stated total")
        self.assertIn("ExcessLiquidity missing", "\n".join(o for o in logs.output if "read again" in o))
        self.assertIn("ExcessLiquidity missing", str(caught.exception))

    def test_a_link_lost_and_restored_during_the_reconciles_account_read_reconciles_again(self):
        # Gateway loses IBKR's servers and restores the same socket while
        # the start's reconcile downloads the account values, and an order
        # placed by hand executes meanwhile: the reconcile's orders and
        # executions, read before the outage, are not matched with an
        # account read after it; the restored link is reconciled again from
        # fresh reads, which find the execution (ADR 0025 point 5).
        ask, fired = self.fake.req_account_updates, []

        def with_outage(subscribe, account_id):
            if subscribe and not fired:
                fired.append(True)
                self.fake.executions.append((contract(), execution(exec_id="0000beef.h.01.01", order_id=0,
                                                                   perm_id=777, order_ref="", client_id=0)))
                self.fake.callbacks.error(-1, 1100, "Connectivity between IB and Trader Workstation has been lost.")
                self.fake.callbacks.error(-1, 1101, "Connectivity between IB and Trader Workstation has been "
                                                    "restored - data lost.")
            ask(subscribe, account_id)
        self.fake.req_account_updates = with_outage
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        with self.assertRaises(ReconcileMismatch) as caught:
            loop.start()
            self.settle(loop)
        self.assertEqual(fired, [True])
        self.assertIn("0000beef.h.01.01", str(caught.exception))
        self.assertEqual(self.types(), [])

    def test_a_link_restored_while_the_reconcile_pumps_the_open_orders_quiet_reconciles_again(self):
        # The account read was complete and unchanged; then, while the
        # reconcile pumps the open orders quiet after it, Gateway reports
        # its link to IBKR's servers restored with data lost (1101), a loss
        # the reads never saw begin, and an order placed by hand executed
        # during it. The link is connected throughout the pump (an 1100
        # there stops it at once, NotConnected), so only the link's change
        # of state shows the outage: the orders, executions and account
        # read before it are not matched, and the link is reconciled again
        # from fresh reads, which find the execution (ADR 0025 point 5;
        # ADR 0019: one cutoff).
        import loop as loop_module
        from connection import Connection
        armed, fired = [], []
        read_account, resync = loop_module.read_account, Connection.resync_open_orders

        def read_then_arm(conn):
            reading = read_account(conn)
            armed.append(True)
            return reading

        def resync_with_an_outage(conn):
            if armed and not fired:
                fired.append(True)
                self.fake.executions.append((contract(), execution(exec_id="0000beef.h.01.02", order_id=0,
                                                                   perm_id=778, order_ref="", client_id=0)))
                self.fake.callbacks.error(-1, 1101, "Connectivity between IB and Trader Workstation has been "
                                                    "restored - data lost.")
            return resync(conn)
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        with mock.patch.object(loop_module, "read_account", read_then_arm), \
                mock.patch.object(Connection, "resync_open_orders", resync_with_an_outage), \
                self.assertRaises(ReconcileMismatch) as caught:
            loop.start()
            self.settle(loop)
        self.assertEqual(fired, [True], "the restoration came while the reconcile pumped the open orders quiet")
        self.assertIn("0000beef.h.01.02", str(caught.exception))
        self.assertEqual(self.types(), [])

    def place_and_short_the_next_values(self, fill):
        """The day's order is placed and accepted; the next day's first account values lack a balance, fill at the wait after."""
        self.place_the_days_order()
        loop = self.loop()
        loop.start()
        self.settle(loop)
        self.assertEqual(len(self.fake.placed), 1)
        snapshots = self.types().count(BROKER_SNAPSHOT_TYPE)

        waits = []

        def filled():
            waits.append(True)
            fill()
            self.fake.positions = [(ACCOUNT, contract(), Decimal("156"), 312.5)]
        self.fake.omit_next = "ExcessLiquidity"
        self.fake.after_refusal = filled
        write_stream(self.folder, date(2027, 1, 15), session_envelopes([date(2027, 1, 15)]))
        with self.assertLogs("ibkr", level="INFO") as logs:
            self.settle(loop)
        self.assertEqual(waits, [True], "the fill came during the wait after the refusal")
        self.assertIn("ibkr broker snapshot read again (attempt 1 of", "\n".join(logs.output))
        types = self.types()
        later = [n for n, t in enumerate(types) if t == BROKER_SNAPSHOT_TYPE][snapshots:]
        self.assertEqual(len(later), 1, "one snapshot for the day")
        self.assertLess(types.index(FILL_TYPE), later[0], "the fill is journalled before the snapshot")
        self.assertEqual(self.engine.inputs()[later[0]]["payload"]["positions"][0]["quantity"], 156,
                         "the snapshot was read again after the fill, so it holds it")

    def day_fill(self):
        self.fake.fill(7, shares="9", price=401.25, exec_id="0001f4e8.t.01.01", cum_qty="9",
                       time="20270114 15:59:00 US/Eastern")

    def test_a_fill_during_the_wait_between_snapshot_attempts_is_journalled_before_the_snapshot(self):
        # A first attempt left a balance unread, and a fill arrives during
        # the 5 s wait before the next.
        self.place_and_short_the_next_values(self.day_fill)

    def test_the_snapshots_stop_states_the_time_its_attempts_took(self):
        # IBKR's values lack a balance every time: the attempts take the
        # 50 s of waits between them and the reads' own time.
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.start()
        self.fake.summary = [row for row in self.fake.summary if row[1] != "ExcessLiquidity"]
        started = loop._monotonic()
        with self.assertRaises(SnapshotRefused) as caught:
            self.settle(loop)
        took = loop._monotonic() - started
        self.assertGreaterEqual(took, 50)
        self.assertIn("over {:.0f} seconds".format(took), str(caught.exception))

    # -- the broker's reports and the stream --------------------------------
    def place_the_days_order(self):
        """FRIDAY's close releases the paper order; IBKR accepts it."""
        released = json.loads((tests_dir / "testdata" / "paper_released.json").read_text())
        self.engine.decisions = {"marketdata:session-closed:{}".format(FRIDAY): [{"type": RELEASED,
                                                                               "payload": released}]}
        self.fake.on_place = self.fake.accept
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))

    def test_a_fill_during_the_snapshots_reads_makes_the_adapter_read_the_account_again(self):
        class FillsDuringMarks(FakeTWS):
            fill_next = False

            def req_mkt_data(self, req_id, contract_):
                if self.fill_next:
                    self.fill_next = False
                    self.fill(7, shares="9", price=401.25, exec_id="0001f4e8.t.01.01", cum_qty="9",
                              time="20270114 15:59:00 US/Eastern")
                    self.positions = [(ACCOUNT, contract(), Decimal("156"), 312.5)]
                super().req_mkt_data(req_id, contract_)
        fake = FillsDuringMarks()
        fake.summary, fake.positions, fake.marks = self.fake.summary, self.fake.positions, self.fake.marks
        self.fake = fake
        self.place_the_days_order()
        loop = self.loop(fake=fake)
        loop.start()
        self.settle(loop)
        self.assertEqual(len(fake.placed), 1)
        fake.fill_next = True
        write_stream(self.folder, date(2027, 1, 15), session_envelopes([date(2027, 1, 15)]))
        self.settle(loop)
        types = self.types()
        second = [n for n, t in enumerate(types) if t == BROKER_SNAPSHOT_TYPE][1]
        self.assertLess(types.index(FILL_TYPE), second, "the fill is journalled before the snapshot")
        self.assertEqual(self.engine.inputs()[second]["payload"]["positions"][0]["quantity"], 156,
                         "the snapshot was read again after the fill, so it holds it")

    def test_a_snapshot_overtaken_by_reports_on_every_attempt_waits_for_the_next_step(self):
        class FillsDuringEveryMark(FakeTWS):
            fills = 0

            def req_mkt_data(self, req_id, contract_):
                # One fill during every attempt's marks: two shares of the
                # nine each time, so every attempt is overtaken.
                if 0 < self.fills <= BROKER_SNAPSHOT_ATTEMPTS:
                    n = self.fills
                    self.fills += 1
                    self.fill(7, shares="2", price=401.25, exec_id="0001f4e8.t.0{}.01".format(n), cum_qty=str(2 * n),
                              time="20270114 15:59:00 US/Eastern")
                super().req_mkt_data(req_id, contract_)
        fake = FillsDuringEveryMark()
        fake.summary, fake.positions, fake.marks = self.fake.summary, self.fake.positions, self.fake.marks
        self.fake = fake
        self.place_the_days_order()
        loop = self.loop(fake=fake)
        loop.start()
        self.settle(loop)
        fake.fills = 1
        write_stream(self.folder, date(2027, 1, 15), session_envelopes([date(2027, 1, 15)]))
        loop.step()
        self.assertEqual(self.types().count(BROKER_SNAPSHOT_TYPE), 1, "no snapshot overtaken by a fill is sent")
        self.assertEqual(self.types().count(FILL_TYPE), BROKER_SNAPSHOT_ATTEMPTS)
        self.settle(loop)
        self.assertEqual(self.types()[-6:], LATER + DAY)

    def test_no_snapshot_is_taken_while_an_execution_awaits_its_commission(self):
        self.place_the_days_order()
        loop = self.loop()
        loop.start()
        self.settle(loop)
        self.fake.fill(7, shares="9", price=401.25, exec_id="0001f4e8.t.01.01", cum_qty="9", commission=None,
                       time="20270114 15:59:00 US/Eastern")
        write_stream(self.folder, date(2027, 1, 15), session_envelopes([date(2027, 1, 15)]))
        self.settle(loop, steps=5)
        self.assertEqual(self.types().count(BROKER_SNAPSHOT_TYPE), 1, "the snapshot waits for the fill")
        self.fake.callbacks.commission_report(mock.Mock(execId="0001f4e8.t.01.01", commission=Decimal("1.00"),
                                                        currency="USD"))
        self.settle(loop)
        types = self.types()
        self.assertLess(types.index(FILL_TYPE), len(types) - 5)
        self.assertEqual(types[-6:], LATER + DAY)

    def test_a_file_naming_an_input_the_outbox_already_holds_is_refused(self):
        path = write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop()
        loop.outbox.append({"id": "marketdata:bar:SPY:2027-01-14", "event_type": "t", "schema_version": 1,
                            "event_time": "2027-01-14T21:00:00Z", "payload": {}})
        loop.start()
        with self.assertRaises(StreamRefused) as caught:
            loop.step()
        self.assertIn(str(path), str(caught.exception))
        self.assertIn("outbox already holds", str(caught.exception))
        self.assertEqual(self.types(), [])

    def test_a_fill_received_during_a_bar_reaches_the_engine_before_the_next_stream_input(self):
        class FillsOn:
            def __init__(self, engine, input_id, act):
                self.engine, self.input_id, self.act = engine, input_id, act

            def __call__(self):
                self.engine()
                return self

            def decide(self, envelope):
                if envelope["id"] == self.input_id and self.act is not None:
                    act, self.act = self.act, None
                    act()
                return self.engine.decide(envelope)
        self.place_the_days_order()
        later = [date(2027, 1, 15), date(2027, 1, 19)]
        engine = FillsOn(self.engine, "marketdata:bar:SPY:2027-01-15", lambda: self.fake.fill(
            7, shares="9", price=401.25, exec_id="0001f4e8.t.01.01", cum_qty="9", time="20270114 15:59:00 US/Eastern"))
        loop = self.loop(engine=engine)
        loop.start()
        self.settle(loop)
        self.assertEqual(len(self.fake.placed), 1)
        write_stream(self.folder, later[-1], session_envelopes(later))
        self.settle(loop)
        ids = self.ids()
        self.assertLess(self.types().index(FILL_TYPE), ids.index("marketdata:session-closed:2027-01-15"),
                        "the fill reached the engine before the session it arrived in closed")

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
        self.assertEqual(self.types()[:6], FIRST + DAY)


    # -- an execution whose commission is still to come ---------------------
    def fill_awaiting_commission_during_a_bar(self):
        """FRIDAY's order is placed; during the next file's first bar it executes, its commission not yet sent."""
        class FillsOn:
            def __init__(self, engine, input_id, act):
                self.engine, self.input_id, self.act = engine, input_id, act

            def __call__(self):
                self.engine()
                return self

            def decide(self, envelope):
                if envelope["id"] == self.input_id and self.act is not None:
                    act, self.act = self.act, None
                    act()
                return self.engine.decide(envelope)
        self.place_the_days_order()
        later = [date(2027, 1, 15), date(2027, 1, 19)]
        return FillsOn(self.engine, "marketdata:bar:SPY:2027-01-15", lambda: self.fake.fill(
            7, shares="9", price=401.25, exec_id="0001f4e8.t.01.01", cum_qty="9", commission=None,
            time="20270114 15:59:00 US/Eastern")), later

    def test_the_stream_waits_while_an_execution_awaits_its_commission_and_the_fill_goes_first(self):
        engine, later = self.fill_awaiting_commission_during_a_bar()
        loop = self.loop(engine=engine)
        loop.start()
        self.settle(loop)
        write_stream(self.folder, later[-1], session_envelopes(later))
        self.settle(loop, steps=10)
        self.assertNotIn("marketdata:session-closed:2027-01-15", self.ids(),
                         "no stream input goes ahead of a fill the engine has not been told about")
        self.fake.callbacks.commission_report(mock.Mock(execId="0001f4e8.t.01.01", commission=Decimal("1.00"),
                                                        currency="USD"))
        self.settle(loop)
        ids = self.ids()
        self.assertLess(self.types().index(FILL_TYPE), ids.index("marketdata:session-closed:2027-01-15"))
        self.assertEqual(ids[-1], "marketdata:session-closed:2027-01-19")

    def test_a_commission_that_never_comes_stops_the_adapter_with_the_stream_still_held(self):
        engine, later = self.fill_awaiting_commission_during_a_bar()
        clock = {"now": RECEIVED}
        loop = self.loop(engine=engine, now=lambda: clock["now"])
        loop.start()
        self.settle(loop)
        write_stream(self.folder, later[-1], session_envelopes(later))
        self.settle(loop, steps=5)
        clock["now"] = RECEIVED + timedelta(seconds=COMMISSION_WAIT)
        with self.assertRaises(UnreportableOrder):
            self.settle(loop, steps=5)
        self.assertNotIn("marketdata:session-closed:2027-01-15", self.ids())

    # -- activity in the account outside this adapter's orders ----------------
    def test_a_trade_placed_outside_the_adapter_during_the_snapshots_reads_makes_it_read_again(self):
        class TradedByHandDuringMarks(FakeTWS):
            trade_next = False
            reads = 0

            def req_positions(self):
                self.reads += 1
                super().req_positions()

            def req_mkt_data(self, req_id, contract_):
                traded = self.trade_next
                if traded:
                    # An order placed in IBKR's own app fills: IBKR reports
                    # its execution to that app, not to this client, and the
                    # account's holdings and cash change all the same.
                    self.trade_next = False
                    self.positions = [(ACCOUNT, contract(), Decimal("156"), 312.5)]
                    self.summary = [(a, t, "96388.75" if t == "TotalCashValue" else v, c) for a, t, v, c in
                                    self.summary]
                super().req_mkt_data(req_id, contract_)
                if traded:
                    # The price moves after this mark, so only marks read
                    # again after the trade carry the new one.
                    self.marks = {SPY_CON_ID: 402.5}
        fake = TradedByHandDuringMarks()
        fake.summary, fake.positions, fake.marks = self.fake.summary, self.fake.positions, self.fake.marks
        self.fake = fake
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop(fake=fake)
        loop.start()
        self.settle(loop)
        fake.trade_next, fake.reads = True, 0
        write_stream(self.folder, date(2027, 1, 15), session_envelopes([date(2027, 1, 15)]))
        self.settle(loop)
        snapshots = self.engine.inputs(BROKER_SNAPSHOT_TYPE)
        self.assertEqual(len(snapshots), 2)
        self.assertEqual(fake.reads, 4, "read, confirm, then read and confirm again: the reads differed")
        self.assertEqual(snapshots[1]["payload"]["positions"],
                         [{"instrument_id": "SPY", "quantity": 156, "mark": 402.5, "mark_set": True}],
                         "the snapshot is the second attempt's, holding the trade; stamped at its read time, it is "
                         "marked at IBKR's last price of that attempt")
        self.assertEqual(self.engine.inputs(MARK)[1]["payload"]["broker_last"], 402.5,
                         "IBKR's last price beside the mark is the second attempt's")

    def test_an_execution_of_another_clients_order_between_the_snapshots_reads_makes_it_read_again(self):
        class ForeignExecutionDuringMarks(FakeTWS):
            execute_next = False
            reads = 0

            def req_positions(self):
                self.reads += 1
                super().req_positions()

            def req_mkt_data(self, req_id, contract_):
                if self.execute_next:
                    # Another client's order in the account executes; this
                    # client sees its execution but the holdings it reads
                    # are the same both times.
                    self.execute_next = False
                    self.callbacks.exec_details(-1, contract(), execution(
                        exec_id="0001f4e8.other.01.01", order_id=3, perm_id=5005, order_ref="", client_id=42,
                        time="20270115 15:59:00 US/Eastern"))
                super().req_mkt_data(req_id, contract_)
        fake = ForeignExecutionDuringMarks()
        fake.summary, fake.positions, fake.marks = self.fake.summary, self.fake.positions, self.fake.marks
        self.fake = fake
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop(fake=fake)
        loop.start()
        self.settle(loop)
        fake.execute_next, fake.reads = True, 0
        write_stream(self.folder, date(2027, 1, 15), session_envelopes([date(2027, 1, 15)]))
        self.settle(loop)
        self.assertEqual(len(self.engine.inputs(BROKER_SNAPSHOT_TYPE)), 2)
        self.assertEqual(fake.reads, 4, "the execution came between the reads, so the account was read again")

    # -- a read whose answers carry no request ID -----------------------------
    def test_an_open_order_read_that_timed_out_is_never_retried_on_the_same_link(self):
        class LateOrderEnd(FakeTWS):
            connects = 0
            stall = False
            stalled_on = None

            def connect(self, *args):
                self.connects += 1
                super().connect(*args)

            def req_all_open_orders(self):
                if self.stall:
                    # IBKR is slow: the list's end comes after the read gave up.
                    self.stall, self.stalled_on = False, self.connects
                    self.calls.append(("req_all_open_orders",))
                    return
                if self.stalled_on == self.connects:
                    # The abandoned read's end arrives first, on the same link,
                    # ahead of this read's rows.
                    self.stalled_on = None
                    self.callbacks.open_order_end()
                super().req_all_open_orders()
        fake = LateOrderEnd()
        fake.summary, fake.positions, fake.marks = self.fake.summary, self.fake.positions, self.fake.marks
        self.fake = fake
        self.place_the_days_order()
        loop = self.loop(fake=fake)
        loop.start()
        self.settle(loop)
        self.assertEqual(len(fake.placed), 1)
        fake.stall = True
        write_stream(self.folder, date(2027, 1, 15), session_envelopes([date(2027, 1, 15)]))
        self.settle(loop)
        snapshots = self.engine.inputs(BROKER_SNAPSHOT_TYPE)
        self.assertEqual(len(snapshots), 2)
        self.assertEqual([o["client_order_id"] for o in snapshots[1]["payload"]["open_orders"]],
                         [json.loads((tests_dir / "testdata" / "paper_released.json").read_text())["client_order_id"]],
                         "the working order is in the snapshot: no late answer completed a later read")
        self.assertGreater(fake.connects, 1, "the link was made again before the account was read again")

    def test_an_account_read_cut_off_by_gateway_losing_ibkrs_servers_is_never_followed_on_the_same_link(self):
        class LosesIbkrDuringOrders(FakeTWS):
            connects = 0
            lose = False
            lost_on = None

            def connect(self, *args):
                self.connects += 1
                super().connect(*args)

            def req_all_open_orders(self):
                if self.lose:
                    # Gateway loses IBKR's servers mid-list and restores
                    # them on the same socket; the list's end comes later.
                    self.lose, self.lost_on = False, self.connects
                    self.calls.append(("req_all_open_orders",))
                    self.callbacks.error(-1, 1100, "Connectivity between IB and Trader Workstation has been lost.")
                    self.callbacks.error(-1, 1102, "Connectivity between IB and Trader Workstation has been "
                                                   "restored - data maintained.")
                    return
                if self.lost_on == self.connects:
                    # The abandoned list's end arrives first, on the same
                    # link, ahead of this read's rows.
                    self.lost_on = None
                    self.callbacks.open_order_end()
                super().req_all_open_orders()
        fake = LosesIbkrDuringOrders()
        fake.summary, fake.positions, fake.marks = self.fake.summary, self.fake.positions, self.fake.marks
        self.fake = fake
        self.place_the_days_order()
        loop = self.loop(fake=fake)
        loop.start()
        self.settle(loop)
        self.assertEqual(len(fake.placed), 1)
        fake.lose = True
        write_stream(self.folder, date(2027, 1, 15), session_envelopes([date(2027, 1, 15)]))
        self.settle(loop)
        self.assertGreater(fake.connects, 1, "the link was made again, not taken back as Gateway restored it")
        snapshots = self.engine.inputs(BROKER_SNAPSHOT_TYPE)
        self.assertEqual(len(snapshots), 2)
        self.assertEqual([o["client_order_id"] for o in snapshots[1]["payload"]["open_orders"]],
                         [json.loads((tests_dir / "testdata" / "paper_released.json").read_text())["client_order_id"]],
                         "the working order is in the snapshot: no late answer completed a later read")

    def test_a_reconcile_cut_off_during_its_positions_is_never_read_again_on_the_same_link(self):
        class LosesIbkrDuringPositions(FakeTWS):
            connects = 0
            lose = True
            lost_on = None

            def connect(self, *args):
                self.connects += 1
                super().connect(*args)

            def req_positions(self):
                if self.lose:
                    # Gateway loses IBKR's servers during the start's
                    # reconcile's positions and restores them on the same
                    # socket; the positions' end comes later.
                    self.lose, self.lost_on = False, self.connects
                    self.calls.append(("req_positions",))
                    self.callbacks.error(-1, 1100, "Connectivity between IB and Trader Workstation has been lost.")
                    self.callbacks.error(-1, 1102, "Connectivity between IB and Trader Workstation has been "
                                                   "restored - data maintained.")
                    return
                if self.lost_on == self.connects:
                    # The abandoned read's end arrives first, on the same
                    # link; this read's own positions come after the rest
                    # of its answers.
                    self.lost_on = None
                    self.calls.append(("req_positions",))
                    self.callbacks.position_end()
                    for account, con, pos, avg in self.positions:
                        self._later.append(lambda a=account, c=con, p=pos, v=avg: self.callbacks.position(a, c, p, v))
                    self._later.append(self.callbacks.position_end)
                    return
                super().req_positions()
        fake = LosesIbkrDuringPositions()
        fake.summary, fake.positions, fake.marks = self.fake.summary, self.fake.positions, self.fake.marks
        self.fake = fake
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        loop = self.loop(fake=fake)
        with self.assertLogs("ibkr", level="INFO") as logs:
            loop.start()
            self.settle(loop)
        reconciled = [o for o in logs.output if "ibkr reconciled" in o]
        self.assertTrue(reconciled, logs.output)
        self.assertIn("positions=1 ", reconciled[0],
                      "the reconcile holds the account's position: no late end completed a later read")
        self.assertGreater(fake.connects, 1, "the link was made again, not taken back as Gateway restored it")

    # -- a fresh run strikes an earlier run's batch -------------------------
    def test_a_stop_while_a_fresh_run_strikes_an_earlier_batch_never_parts_its_snapshot_from_its_stream(self):
        self.queue_unsent()
        log, engine = self.fresh_run()
        fresh = self.loop(engine=engine, engine_log=log)
        fresh.start()

        class Stop(Exception):
            pass
        real = Outbox._write
        writes = []

        def stop_on_the_second(outbox, entries):
            writes.append(entries)
            if len(writes) == 2:
                raise Stop("the power went")
            real(outbox, entries)
        with mock.patch.object(Outbox, "_write", stop_on_the_second):
            try:
                fresh.step()
            except Stop:
                pass
        fresh.close()
        left = [e["id"] for e in Outbox(self.pending + ".outbox").load()]
        snapshot = "ibkr:account.broker.snapshot:1"
        self.assertIn(left, [[], [snapshot + ":account.snapshot", snapshot, snapshot + ":account.reconciliation",
                                  "marketdata:bar:SPY:2027-01-14", snapshot + ":market.mark.recorded:SPY",
                                  "marketdata:session-closed:2027-01-14"]],
                      "the earlier batch is whole in the outbox or wholly gone")
        resumed = self.loop()
        resumed.start()
        self.settle(resumed)
        self.assertEqual(self.types()[:6], FIRST + DAY, "the resumed run gets its stream behind its snapshot")

    # -- a compact stream repeats each retraction its calendar covers --------
    def compact_files(self, reason=None):
        """Two compact momentum files as the data job's encoder writes them, each repeating the 2026-11-25 retraction."""
        adapter_dir = str(tests_dir.parents[1])
        if adapter_dir not in sys.path:
            sys.path.insert(0, adapter_dir)
        from marketdata import encode
        from marketdata.tests.test_compact import FakeRanker, retract
        from marketdata.tests.test_encode import FIRST, LAST, build
        data = tempfile.TemporaryDirectory()
        self.addCleanup(data.cleanup)
        build(data.name)
        retract(data.name, "2026-11-25")
        identity = encode.Identity(CONFIGURATION_HASH, STRATEGY_VERSION, RUN)
        spans = [(FIRST, date(2026, 11, 24)), (date(2026, 11, 27), LAST)]
        out = []
        for first, last in spans:
            envelopes = encode.encode(data.name, first, last, identity, required=["SPY", "IEF"], optional=[],
                                      base=FIRST, first_sequence=1, ranker=FakeRanker())
            if reason is not None and last == LAST:
                for e in envelopes:
                    if e["type"] == "market.session.retracted":
                        e["payload"] = dict(e["payload"], reason=reason)
                        raw = json.dumps(e["payload"], separators=(",", ":"), allow_nan=False)
                        e["payload_hash"] = hashlib.sha256(raw.encode()).hexdigest()
            out.append(write_stream(self.folder, last, envelopes, prefix="momentum"))
        return out

    def test_a_compact_stream_repeating_a_retraction_in_each_file_is_delivered_with_the_retraction_once(self):
        files = self.compact_files()
        retractions = [[e for e in map(json.loads, f.read_text().splitlines())
                        if e["type"] == "market.session.retracted"] for f in files]
        self.assertEqual([[e["id"] for e in r] for r in retractions], [["marketdata:retracted:2026-11-25"]] * 2,
                         "the encoder repeats the retraction in every file its calendar covers")
        loop = self.loop()
        loop.start()
        with self.assertLogs("ibkr", level="INFO") as logs:
            self.settle(loop, steps=80)
        self.assertEqual(self.ids().count("marketdata:retracted:2026-11-25"), 1)
        queued = [m for m in logs.output if "ibkr stream queued" in m]
        self.assertEqual(len(queued), 1)
        self.assertTrue(queued[0].endswith(" inputs={}".format(len(self.ids()))),
                        "the log counts the inputs queued, the repeated retraction once: " + queued[0])
        self.assertEqual(self.ids()[-1], "marketdata:session-closed:2026-11-30")
        self.assertEqual(loop.outbox.load(), [])

    def test_a_retraction_repeated_with_other_content_is_refused(self):
        files = self.compact_files(reason="another reason")
        loop = self.loop()
        loop.start()
        with self.assertRaises(StreamRefused) as caught:
            loop.step()
        self.assertIn(str(files[1]), str(caught.exception))
        self.assertEqual(self.types(), [])


class StreamReadTests(unittest.TestCase):
    def test_a_valid_file_is_read_in_order(self):
        with tempfile.TemporaryDirectory() as d:
            written = session_envelopes([FRIDAY])
            path = write_stream(Path(d), FRIDAY, written)
            digest = hashlib.sha256(path.read_bytes()).hexdigest()
            stream = read_stream(str(path), RUN, CONFIGURATION_HASH, STRATEGY_VERSION)
        self.assertEqual(stream.envelopes, written)
        self.assertEqual((stream.name, stream.session, stream.sha256), (path.name, FRIDAY, digest))


    def test_a_close_stamped_with_an_offset_closes_its_new_york_session(self):
        with tempfile.TemporaryDirectory() as d:
            written = session_envelopes([FRIDAY])
            written[1] = _envelope(CLOSED, 1, written[1]["id"], written[1]["event_time"],
                                   {"period_end": "2027-01-14T16:00:00-05:00", "instrument_ids": ["SPY"]}, sequence=2)
            path = write_stream(Path(d), FRIDAY, written)
            stream = read_stream(str(path), RUN, CONFIGURATION_HASH, STRATEGY_VERSION)
        self.assertEqual((stream.first, stream.session), (FRIDAY, FRIDAY))


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


class StreamFolderRuleTests(unittest.TestCase):
    """The adapter and the data job find a run's stream folder by one rule: each case is laid out once, read by both."""

    def setUp(self):
        adapter_dir = str(tests_dir.parents[1])
        if adapter_dir not in sys.path:
            sys.path.insert(0, adapter_dir)
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.streams = self.root / "boost"
        self.streams.mkdir()
        repo = tests_dir.parents[2]
        self.cfg = json.loads((repo / "deploy" / "etc" / "engines" / "boost.config").read_text())

    def engine(self, name):
        return LoggedEngine(str(self.root / (name + ".log")), configuration=self.cfg)

    def folder(self, name):
        (self.streams / name).mkdir()

    def found(self, engine):
        """(the adapter's folder name, the data job's), "refused" where one refuses."""
        from delivery import stream_run
        from marketdata import tidata
        log = str(engine.path)
        try:
            adapter = stream_run(str(self.streams), log)
        except StreamRefused:
            adapter = "refused"
        try:
            data = tidata.stream_run(str(self.streams), log, tidata.read_engine_log(log))
        except tidata.Refused:
            data = "refused"
        return adapter, data

    def first_run(self, approved=False):
        first = self.engine("run-a")
        if approved:
            approve(first, str(self.engine("run-0").path))
        commit_bar(first, "input-a")
        self.folder("run-a")
        return first

    def test_a_fresh_run_with_its_own_folder_takes_it(self):
        self.assertEqual(self.found(self.first_run()), ("run-a", "run-a"))

    def test_a_crash_resume_keeps_the_first_runs_folder(self):
        for approved in (False, True):
            with self.subTest(approved=approved):
                self.setUp()
                first = self.first_run(approved)
                resumed = LoggedEngine.resumed(str(first.path), str(self.root / "run-c.log"))
                commit(resumed, "input-c")
                self.assertEqual(self.found(resumed), ("run-a", "run-a"))

    def test_a_crash_resume_with_a_folder_of_its_own_name_too_is_refused(self):
        first = self.first_run()
        resumed = LoggedEngine.resumed(str(first.path), str(self.root / "run-c.log"))
        self.folder("run-c")
        self.assertEqual(self.found(resumed), ("refused", "refused"))

    def test_an_unapproved_second_start_with_the_same_configuration_and_as_of_is_refused(self):
        self.first_run()
        again = self.engine("run-b")
        commit(again, "input-b")
        self.assertEqual(self.found(again), ("refused", "refused"))

    def test_a_resume_of_a_run_that_committed_only_its_configuration_is_refused(self):
        first = self.engine("run-a")
        self.folder("run-a")
        resumed = LoggedEngine.resumed(str(first.path), str(self.root / "run-c.log"))
        self.assertEqual(self.found(resumed), ("refused", "refused"))

    def test_a_copy_still_being_written_is_never_taken_for_the_run(self):
        first = self.first_run()
        resumed = LoggedEngine.resumed(str(first.path), str(self.root / "run-c.log"))
        lines = Path(resumed.path).read_bytes().splitlines(keepends=True)
        Path(resumed.path).write_bytes(b"".join(lines[:3]))
        self.assertEqual(self.found(resumed), ("refused", "refused"))

    def test_an_approved_fresh_run_never_takes_an_earlier_runs_folder(self):
        self.first_run()
        fresh = self.engine("run-b")
        approve(fresh, str(self.root / "run-a.log"))
        self.assertEqual(self.found(fresh), ("run-b", "run-b"))

    def test_a_start_repeating_the_runs_input_ids_with_other_records_is_refused(self):
        self.first_run()
        again = self.engine("run-b")
        commit(again, "input-a", payload={"input": "another payload"})
        self.assertEqual(self.found(again), ("refused", "refused"))

    def test_a_resume_of_an_approved_run_that_crashed_after_its_approval_keeps_its_folder(self):
        first = self.engine("run-a")
        approve(first, str(self.engine("run-0").path))
        self.folder("run-a")
        resumed = LoggedEngine.resumed(str(first.path), str(self.root / "run-c.log"))
        self.assertEqual(self.found(resumed), ("run-a", "run-a"))

    def test_an_unreadable_log_of_another_runs_folder_is_warned_of_and_stops_the_start(self):
        self.folder("run-old")
        (self.root / "run-old.log").write_text("written by another build\n")
        other = LoggedEngine(str(self.root / "run-older.log"), configuration=self.cfg,
                             started_at="2026-10-15T13:30:00Z")
        commit(other, "input-x")
        with open(other.path, "a", encoding="utf-8") as f:
            f.write("a damaged tail\n")
        self.folder("run-older")
        fresh = self.first_run(approved=True)
        with self.assertLogs("ibkr", level="WARNING") as logs, contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(self.found(fresh), ("refused", "refused"))
        self.assertEqual(len(logs.output), 1, logs.output)
        self.assertIn(str(self.streams / "run-old"), logs.output[0])
        self.assertIn(str(self.streams / "run-old"), err.getvalue())
        self.assertNotIn("run-older", logs.output[0] + err.getvalue(),
                         "a log whose first records name another run is never read further")

    def test_a_logs_head_names_the_identity_its_whole_log_does(self):
        from marketdata import tidata
        from relay import ResolveRefused, head_identity, run_identity
        plain = self.first_run()
        approved = self.engine("run-b")
        approve(approved, str(plain.path))
        commit(approved, "input-b")
        for engine in (plain, approved):
            with self.subTest(log=engine.path.name):
                log = str(engine.path)
                self.assertEqual(head_identity(log), run_identity(log))
                self.assertEqual(tidata.head_identity(log), tidata.read_engine_log(log).identity)
        def record(kind, ident):
            return ('{"record":{"kind":"%s","envelope":{"id":"%s","type":"t","recorded_at":"2026-10-16T13:30:00Z"}}}'
                    '\n' % (kind, ident))
        # The second group opening with a decision: no group but an input's.
        decision_first = ('{"log_version":1}\n' + record("input", "configuration:x") + '{"commit":{}}\n'
                          + record("decision", "d") + '{"commit":{}}\n')
        for text in ("", '{"log_version":1}', '{"log_version":1}\n', '{"log_version":1}\n{"commit":{}}\n',
                     decision_first):
            with self.subTest(log=text):
                Path(self.root, "bad.log").write_text(text)
                with self.assertRaises(ResolveRefused):
                    head_identity(str(self.root / "bad.log"))
                with self.assertRaises(tidata.Refused):
                    tidata.head_identity(str(self.root / "bad.log"))

    def test_an_unreadable_log_whose_first_records_name_the_run_is_refused(self):
        first = self.first_run()
        resumed = LoggedEngine.resumed(str(first.path), str(self.root / "run-c.log"))
        commit(resumed, "input-c")
        with open(first.path, "a", encoding="utf-8") as f:
            f.write("a damaged tail\n")
        self.assertEqual(self.found(resumed), ("refused", "refused"))

    def test_a_log_whose_records_do_not_follow_their_record_hashes_is_never_taken_for_the_run(self):
        # An envelope field changed after the engine wrote it, its stored
        # record hash kept: the stored hashes still match, the chain over the
        # records does not.
        for edited in ("the first run's", "the resume's", "both"):
            with self.subTest(edited=edited):
                self.setUp()
                first = self.first_run()
                commit(first, "input-b")
                resumed = LoggedEngine.resumed(str(first.path), str(self.root / "run-c.log"))
                commit(resumed, "input-c")
                logs = {"the first run's": [first.path], "the resume's": [resumed.path],
                        "both": [first.path, resumed.path]}[edited]
                for path in logs:
                    text = Path(path).read_text(encoding="utf-8")
                    self.assertIn('"id":"input-b"', text)
                    Path(path).write_text(text.replace('"id":"input-b"', '"id":"input-z"', 1), encoding="utf-8")
                self.assertEqual(self.found(resumed), ("refused", "refused"))

    def test_a_resume_whose_first_logs_head_cannot_be_read_stops_and_is_never_sent_to_a_warm_up(self):
        from delivery import stream_run
        from marketdata import tidata
        first = self.first_run()
        resumed = LoggedEngine.resumed(str(first.path), str(self.root / "run-c.log"))
        commit(resumed, "input-c")
        lines = Path(first.path).read_bytes().splitlines(keepends=True)
        Path(first.path).write_bytes(b"a damaged header\n" + b"".join(lines[1:]))
        log = str(resumed.path)
        with contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(StreamRefused) as adapter:
                stream_run(str(self.streams), log)
            with self.assertRaises(tidata.Refused) as data:
                tidata.stream_run(str(self.streams), log, tidata.read_engine_log(log), warn=lambda text: None)
        for refusal in (str(adapter.exception), str(data.exception)):
            self.assertIn(str(self.streams / "run-a"), refusal)
            self.assertIn("restore", refusal)

    def test_a_resume_whose_first_log_is_damaged_is_still_stopped_when_a_folder_of_its_own_name_exists(self):
        from delivery import stream_run
        from marketdata import tidata
        first = self.first_run()
        resumed = LoggedEngine.resumed(str(first.path), str(self.root / "run-c.log"))
        commit(resumed, "input-c")
        lines = Path(first.path).read_bytes().splitlines(keepends=True)
        Path(first.path).write_bytes(b"a damaged header\n" + b"".join(lines[1:]))
        self.folder("run-c")
        log = str(resumed.path)
        with contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(StreamRefused) as adapter:
                stream_run(str(self.streams), log)
            with self.assertRaises(tidata.Refused) as data:
                tidata.stream_run(str(self.streams), log, tidata.read_engine_log(log), warn=lambda text: None)
        for refusal in (str(adapter.exception), str(data.exception)):
            self.assertIn(str(self.streams / "run-a"), refusal)
            self.assertIn("cannot be matched to any run", refusal)

    def test_a_resume_whose_first_log_is_missing_is_told_it_is_a_resume_never_to_run_a_warm_up(self):
        from delivery import Deliverer, stream_run
        from marketdata import tidata
        first = self.first_run(approved=True)
        resumed = LoggedEngine.resumed(str(first.path), str(self.root / "run-c.log"))
        commit(resumed, "input-c")
        os.remove(first.path)
        log = str(resumed.path)
        with self.assertRaises(StreamRefused) as adapter:
            stream_run(str(self.streams), log)
        with self.assertRaises(tidata.Refused) as data:
            tidata.stream_run(str(self.streams), log, tidata.read_engine_log(log))
        for refusal in (str(adapter.exception), str(data.exception)):
            self.assertIn("crash resume of an earlier run", refusal)
            self.assertIn("restore that run's log beside it", refusal)
            self.assertIn("committed delivered inputs beyond its configuration", refusal)
            self.assertIn(str(self.streams / "run-a"), refusal)
            self.assertIn("do not run a warm-up under this name", refusal)
            self.assertNotIn("Run it once", refusal)
        check = Deliverer(str(self.streams), str(self.root / "record.json"), None, log, None, None)
        with self.assertRaises(StreamRefused) as checked:
            check.check()
        self.assertNotIn("Run it once", str(checked.exception))

    def test_a_resume_with_no_folder_of_any_run_and_no_first_log_still_gets_the_resume_message(self):
        from delivery import stream_run
        from marketdata import tidata
        first = self.engine("run-a")
        commit_bar(first, "input-a")
        resumed = LoggedEngine.resumed(str(first.path), str(self.root / "run-c.log"))
        os.remove(first.path)
        log = str(resumed.path)
        with self.assertRaises(StreamRefused) as adapter:
            stream_run(str(self.streams), log)
        with self.assertRaises(tidata.Refused) as data:
            tidata.stream_run(str(self.streams), log, tidata.read_engine_log(log))
        for refusal in (str(adapter.exception), str(data.exception)):
            self.assertIn("crash resume of an earlier run", refusal)
            self.assertNotIn("Run it once", refusal)

    def test_a_fresh_run_whose_log_holds_an_operator_input_is_told_to_run_the_warm_up_not_refused_as_a_resume(self):
        from delivery import Deliverer, stream_run
        from marketdata import tidata
        fresh = self.engine("run-b")
        commit(fresh, "operator-kill-switch:1", kind="operator.kill_switch.set")
        log = str(fresh.path)
        self.assertEqual(stream_run(str(self.streams), log), "run-b")
        self.assertEqual(tidata.stream_run(str(self.streams), log, tidata.read_engine_log(log)), "run-b")
        check = Deliverer(str(self.streams), str(self.root / "record.json"), None, log, None, None)
        with self.assertRaises(StreamRefused) as checked:
            check.check()
        self.assertIn("Run it once", str(checked.exception))

    def test_a_resume_holding_copied_bars_gets_the_resume_message_counting_delivered_inputs_only(self):
        from delivery import stream_run
        from marketdata import tidata
        first = self.engine("run-a")
        commit_bar(first, "input-a")
        commit(first, "operator-kill-switch:1", kind="operator.kill_switch.set")
        resumed = LoggedEngine.resumed(str(first.path), str(self.root / "run-c.log"))
        os.remove(first.path)
        log = str(resumed.path)
        with self.assertRaises(StreamRefused) as adapter:
            stream_run(str(self.streams), log)
        with self.assertRaises(tidata.Refused) as data:
            tidata.stream_run(str(self.streams), log, tidata.read_engine_log(log))
        for refusal in (str(adapter.exception), str(data.exception)):
            self.assertIn("already holds 1 committed delivered inputs beyond its configuration", refusal)

    def test_a_resume_that_crashed_after_the_broker_snapshot_and_before_the_first_bar_is_told_it_is_a_resume(self):
        import snapshot
        from delivery import stream_run
        from marketdata import tidata
        first = self.engine("run-a")
        commit(first, "broker-snapshot-1", kind=snapshot.BROKER_SNAPSHOT_TYPE,
               version=snapshot.BROKER_SNAPSHOT_VERSION, payload={"as_of": "2027-01-14T22:05:00Z"})
        resumed = LoggedEngine.resumed(str(first.path), str(self.root / "run-c.log"))
        os.remove(first.path)
        log = str(resumed.path)
        with self.assertRaises(StreamRefused) as adapter:
            stream_run(str(self.streams), log)
        with self.assertRaises(tidata.Refused) as data:
            tidata.stream_run(str(self.streams), log, tidata.read_engine_log(log))
        for refusal in (str(adapter.exception), str(data.exception)):
            self.assertIn("already holds 1 committed delivered inputs beyond its configuration", refusal)
            self.assertIn("crash resume of an earlier run", refusal)
            self.assertNotIn("Run it once", refusal)

    def test_a_fresh_run_with_only_configuration_approval_and_a_kill_switch_is_told_to_run_the_warm_up(self):
        from delivery import Deliverer
        from marketdata import tidata
        self.first_run()
        fresh = self.engine("run-b")
        approve(fresh, str(self.root / "run-a.log"))
        commit(fresh, "operator-kill-switch:1", kind="operator.kill_switch.set")
        self.assertEqual(self.found(fresh), ("run-b", "run-b"))
        check = Deliverer(str(self.streams), str(self.root / "record.json"), None, str(fresh.path), None, None)
        with self.assertRaises(StreamRefused) as checked:
            check.check()
        self.assertIn("Run it once", str(checked.exception))

    def test_the_delivered_kinds_are_one_table_for_both_readers(self):
        import snapshot
        from delivery import DELIVERED_KINDS, kinds
        # The adapter's own inputs in a batch: the account snapshot (Boost's
        # version 3 or an IRA's 4), the broker snapshot (at either version a
        # log may hold) and the mark (Boost's 2 or an IRA's 3, with its contract).
        self.assertEqual(set(DELIVERED_KINDS), set(kinds.KINDS) | {
            (snapshot.BROKER_SNAPSHOT_TYPE, 1), (snapshot.BROKER_SNAPSHOT_TYPE, snapshot.BROKER_SNAPSHOT_VERSION),
            (snapshot.ACCOUNT_SNAPSHOT_TYPE, snapshot.ACCOUNT_SNAPSHOT_VERSION),
            (snapshot.ACCOUNT_SNAPSHOT_TYPE, snapshot.ACCOUNT_SNAPSHOT_CASH_VERSION),
            (snapshot.MARK_TYPE, snapshot.MARK_VERSION), (snapshot.MARK_TYPE, snapshot.MARK_CONTRACT_VERSION)})
        self.assertIs(kinds.DELIVERED_KINDS, DELIVERED_KINDS)

    def test_a_true_fresh_run_with_no_folder_is_still_told_to_run_the_warm_up(self):
        from delivery import Deliverer
        self.first_run()
        fresh = self.engine("run-b")
        approve(fresh, str(self.root / "run-a.log"))
        self.assertEqual(self.found(fresh), ("run-b", "run-b"))
        check = Deliverer(str(self.streams), str(self.root / "record.json"), None, str(fresh.path), None, None)
        with self.assertRaises(StreamRefused) as checked:
            check.check()
        self.assertIn("Run it once", str(checked.exception))

    def test_a_skipped_logs_remedy_names_the_engines_log_folder_not_the_streams(self):
        from delivery import stream_run
        from marketdata import tidata
        self.folder("run-old")
        (self.root / "run-old.log").write_text("written by another build\n")
        fresh = self.first_run(approved=True)
        log = str(fresh.path)
        warnings = []
        with self.assertLogs("ibkr", level="WARNING") as logs:
            with self.assertRaises(StreamRefused) as adapter:
                stream_run(str(self.streams), log)
        with self.assertRaises(tidata.Refused) as data:
            tidata.stream_run(str(self.streams), log, tidata.read_engine_log(log), warn=warnings.append)
        for text in (logs.output[0], warnings[0], str(adapter.exception), str(data.exception)):
            self.assertIn("cannot be matched to any run", text)
            self.assertIn("the engine's log folder {}".format(self.root), text)
        for refusal in (str(adapter.exception), str(data.exception)):
            self.assertTrue(refusal.startswith("nothing is "), refusal)
            self.assertIn("once its run has ended, move the folder out of", refusal)


class LogChainTests(unittest.TestCase):
    """Both readers recompute a durable log's record hash chain as internal/journal does (LOG_CHAIN_ALGORITHM)."""

    def setUp(self):
        adapter_dir = str(tests_dir.parents[1])
        if adapter_dir not in sys.path:
            sys.path.insert(0, adapter_dir)
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)

    def breaks(self, path):
        """(the adapter's chain_break, the data job's) for the log at path."""
        from marketdata import tidata
        from relay import _committed_groups
        return _committed_groups(str(path)).chain_break, tidata.read_engine_log(str(path)).chain_break

    def canonical_cases(self):
        """The envelopes (and payloads) the canonical bytes are held to: Go's string escapes (<, >, &, U+2028,
        U+2029, control bytes), a time stated at an offset, the largest sequence."""
        base = {"id": "a", "type": "t", "envelope_version": 1, "schema_version": 2,
                "event_time": "2026-10-16T09:30:00.120000000-04:00",
                "recorded_at": "2026-10-16T09:30:00Z", "sequence": 3, "causation_id": "c",
                "source": "s", "strategy_version": "v", "configuration_hash": "h", "payload_hash": "p"}
        payloads = ['{"x":"<&>\\u2028"}', '{"k":"\u2028\u2029 \u00e9 \\u0001"}', '{}']
        return base, [
            (dict(base, id="a<b>&c\u2028d\u2029\u00e9\x01\x08\x0c\t\n\r\x1f\x7f\"\\/"), payloads[0]),
            (dict(base, sequence=18446744073709551615, correlation_id="run<1>"), payloads[1]),
            (dict(base, event_time="2026-10-16T23:45:10.5+05:30", recorded_at="2026-10-17T00:10:00.100200-23:59"),
             payloads[2]),
            (dict(base, event_time="2026-10-16T09:30:00.000000001Z", recorded_at="2026-12-31T23:59:59+00:00"),
             payloads[2]),
        ]

    def test_both_readers_are_one_module(self):
        # The data job and the adapter read the durable log with one reader
        # (adapter/marketdata/engine_log.py): the adapter loads it by its
        # path, the data job from its package.
        import relay
        from marketdata import tidata
        self.assertEqual(os.path.realpath(relay.engine_log.__file__), os.path.realpath(tidata.engine_log.__file__))
        self.assertIs(relay.log_chain_next, relay.engine_log.chain_next)
        self.assertIs(relay.CommittedLog, relay.engine_log.CommittedLog)

    def assert_canonical_bytes(self, cases, wants):
        """Both Python readers (one module, loaded by each) hash and render each case as wants (dicts of canonical
        text and sha256) say."""
        import relay
        from marketdata import tidata
        previous = bytes(32)
        self.assertEqual(len(wants), len(cases))
        for (envelope, payload), want in zip(cases, wants):
            with self.subTest(envelope["id"]):
                digest = hashlib.sha256(previous + b"input" + want["canonical"].encode("utf-8")).digest()
                self.assertEqual(relay.log_chain_next(previous, "input", envelope, payload.encode("utf-8")), digest)
                self.assertEqual(tidata.engine_log.chain_next(previous, "input", envelope, payload.encode("utf-8")),
                                 digest)
                fields = {k: envelope.get(k, "") for k in (relay.ENVELOPE_FIELDS | relay.ENVELOPE_OPTIONAL) - {"payload"}}
                fields.update(event_time=relay._utc(envelope["event_time"]), recorded_at=relay._utc(envelope["recorded_at"]),
                              payload=payload)
                self.assertEqual(relay._canonical_bytes(fields).decode("utf-8"), want["canonical"])
                self.assertEqual(tidata.engine_log.canonical_bytes(fields).decode("utf-8"), want["canonical"])
                self.assertEqual(hashlib.sha256(relay._canonical_bytes(fields)).hexdigest(), want["sha256"])

    def test_the_canonical_envelope_bytes_are_the_engines_own_output(self):
        # Go's own output, run here: the authoritative check. Without a Go
        # toolchain it fails, as the data job's Go-driven tests do.
        import relay
        from marketdata import tidata
        base, cases = self.canonical_cases()
        sent = "".join(json.dumps(envelope, ensure_ascii=False)[:-1] + ',"payload":' + payload + "}\n"
                       for envelope, payload in cases)
        try:
            run = subprocess.run(["go", "run", "./adapter/ibkr/tests/testdata/canonical_vector.go"],
                                 cwd=str(tests_dir.parents[2]), input=sent, text=True, capture_output=True,
                                 timeout=GO_TIMEOUT, env=dict(os.environ, GOTOOLCHAIN="local"))
        except FileNotFoundError:
            self.fail("no Go toolchain on PATH: the canonical bytes are checked against Go's own output")
        self.assertEqual(run.returncode, 0, run.stderr)
        self.assert_canonical_bytes(cases, [json.loads(line) for line in run.stdout.splitlines()])
        previous = bytes(32)
        for unchained in ({}, dict(base, event_time="2026-13-16T09:30:00Z")):
            self.assertIsNone(relay.log_chain_next(previous, "input", unchained, b"{}"))
        with self.assertRaises(TypeError):
            relay._canonical_bytes({"x": 1.5})
        with self.assertRaises(TypeError):
            tidata.engine_log.canonical_bytes({"x": True})

    def test_the_canonical_envelope_bytes_match_a_vector_frozen_from_the_engine(self):
        # Printed once by adapter/ibkr/tests/testdata/canonical_vector.go (internal/event
        # CanonicalEnvelopeBytes, as of the engine at commit 40b2851, hash chain algorithm
        # LOG_CHAIN_ALGORITHM) for canonical_cases(); checked with no Go toolchain.
        _, cases = self.canonical_cases()
        self.assert_canonical_bytes(cases, [
            {"canonical": '{"causation_id":"c","configuration_hash":"h","correlation_id":"","envelope_version":1,"event_time":"2026-10-16T13:30:00.12Z","id":"a\\u003cb\\u003e\\u0026c\\u2028d\\u2029\u00e9\\u0001\\b\\f\\t\\n\\r\\u001f\x7f\\"\\\\/","payload":"{\\"x\\":\\"\\u003c\\u0026\\u003e\\\\u2028\\"}","payload_hash":"p","recorded_at":"2026-10-16T09:30:00Z","schema_version":2,"sequence":3,"source":"s","strategy_version":"v","type":"t"}',
             "sha256": "23a3e9e571eb30718c9fd7193d3de7c3ddb67d820eb7c3760379bab08ebf4a7a"},
            {"canonical": '{"causation_id":"c","configuration_hash":"h","correlation_id":"run\\u003c1\\u003e","envelope_version":1,"event_time":"2026-10-16T13:30:00.12Z","id":"a","payload":"{\\"k\\":\\"\\u2028\\u2029 \u00e9 \\\\u0001\\"}","payload_hash":"p","recorded_at":"2026-10-16T09:30:00Z","schema_version":2,"sequence":18446744073709551615,"source":"s","strategy_version":"v","type":"t"}',
             "sha256": "99a78a4ada7a7eea39d2ba100c97c4c650152603b5cb14cc963253fa96ebbc17"},
            {"canonical": '{"causation_id":"c","configuration_hash":"h","correlation_id":"","envelope_version":1,"event_time":"2026-10-16T18:15:10.5Z","id":"a","payload":"{}","payload_hash":"p","recorded_at":"2026-10-18T00:09:00.1002Z","schema_version":2,"sequence":3,"source":"s","strategy_version":"v","type":"t"}',
             "sha256": "95e35539c413516cd4014f52b81d0c330426dad98afc12f50c4b4103d74ae2ff"},
            {"canonical": '{"causation_id":"c","configuration_hash":"h","correlation_id":"","envelope_version":1,"event_time":"2026-10-16T09:30:00.000000001Z","id":"a","payload":"{}","payload_hash":"p","recorded_at":"2026-12-31T23:59:59Z","schema_version":2,"sequence":3,"source":"s","strategy_version":"v","type":"t"}',
             "sha256": "b7e76cefed5b008baef8afa9c5c1af09c809c2d67b874d9830e903692cae1d45"},
        ])

    def test_a_time_with_an_impossible_offset_follows_no_chain(self):
        import relay
        from marketdata import tidata
        base = {"id": "a", "type": "t", "envelope_version": 1, "schema_version": 2,
                "event_time": "2026-10-16T13:30:00Z", "recorded_at": "2026-10-16T13:30:00Z", "sequence": 3,
                "causation_id": "c", "source": "s", "strategy_version": "v", "configuration_hash": "h",
                "payload_hash": "p"}
        previous = bytes(32)
        self.assertIsNotNone(relay.log_chain_next(previous, "input", base, b"{}"))
        for bad in ("2026-10-17T16:30:00+27:00", "2026-10-16T13:30:00-24:00", "2026-10-16T13:30:00+05:60",
                    "2026-10-16T13:30:00.5-00:99"):
            with self.subTest(bad):
                envelope = dict(base, event_time=bad)
                self.assertIsNone(relay.log_chain_next(previous, "input", envelope, b"{}"))
                self.assertIsNone(tidata.engine_log.chain_next(previous, "input", envelope, b"{}"))
                self.assertFalse(relay._is_time(bad, relay.TIME))
        self.assertTrue(relay._is_time("2026-10-16T13:30:00+23:59", relay.TIME))

    def test_an_engine_log_and_the_doubles_follow_the_chain(self):
        engine = LoggedEngine(str(self.root / "run-a.log"), configuration=json.loads(
            (tests_dir.parents[2] / "deploy" / "etc" / "engines" / "boost.config").read_text()))
        commit(engine, "input-a")
        self.assertEqual(self.breaks(engine.path), (None, None))
        self.assertEqual(self.breaks(tests_dir.parents[1] / "marketdata" / "tests" / "testdata" / "boost-fixture.log"),
                         (None, None))

    def test_a_record_that_does_not_follow_the_chain_is_named_and_a_torn_tail_is_not(self):
        engine = LoggedEngine(str(self.root / "run-a.log"), configuration=json.loads(
            (tests_dir.parents[2] / "deploy" / "etc" / "engines" / "boost.config").read_text()))
        commit(engine, "input-a")
        commit(engine, "input-b")
        good = Path(engine.path).read_text(encoding="utf-8")
        for name, text, want in (
                ("an edited id", good.replace('"id":"input-a"', '"id":"input-z"'), 2),
                ("an edited record in the uncommitted tail", good + good.splitlines(keepends=True)[-2].replace(
                    '"sequence":3', '"sequence":4', 1), None)):
            with self.subTest(name):
                Path(engine.path).write_text(text, encoding="utf-8")
                self.assertEqual(self.breaks(engine.path), (want, want))
        # A time no engine writes, which both readers (one module) refuse
        # outright: its envelope is not one the engine writes.
        from marketdata import tidata
        from relay import ResolveRefused
        Path(engine.path).write_text(good.replace('"event_time":"2027-01-14T22:05:00Z"',
                                                  '"event_time":"2027-13-14T22:05:00Z"', 1), encoding="utf-8")
        with self.assertRaises(tidata.Refused) as data:
            tidata.read_engine_log(str(engine.path))
        with self.assertRaises(ResolveRefused) as adapter:
            read_engine_log(str(engine.path))
        for refusal in (str(data.exception), str(adapter.exception)):
            self.assertIn("its envelope is not an event envelope as the engine writes one", refusal)

    def test_both_readers_refuse_a_record_whose_payload_is_not_last(self):
        from marketdata import tidata
        from relay import ResolveRefused
        engine = LoggedEngine(str(self.root / "run-a.log"), configuration=json.loads(
            (tests_dir.parents[2] / "deploy" / "etc" / "engines" / "boost.config").read_text()))
        lines = Path(engine.path).read_text(encoding="utf-8").splitlines()
        record = json.loads(lines[1])
        record["record"]["envelope"] = dict(reversed(list(record["record"]["envelope"].items())))
        lines[1] = json.dumps(record, separators=(",", ":"))
        Path(engine.path).write_text("\n".join(lines) + "\n", encoding="utf-8")
        with self.assertRaises(tidata.Refused) as data:
            tidata.read_engine_log(str(engine.path))
        with self.assertRaises(ResolveRefused) as adapter:
            read_engine_log(str(engine.path))
        for refusal in (str(data.exception), str(adapter.exception)):
            self.assertIn("its payload does not match its payload hash", refusal)


class RealEngineResumeTests(unittest.TestCase):
    """The record hash chain rule rests on what cmd/engine -resume writes: the real engine, built here, crashed and resumed."""

    def test_a_real_resume_keeps_every_copied_record_hash_and_both_chains_verify(self):
        import signal
        import time
        from delivery import stream_run
        from relay import _committed_groups
        adapter_dir = str(tests_dir.parents[1])
        if adapter_dir not in sys.path:
            sys.path.insert(0, adapter_dir)
        from marketdata import tidata
        repo = tests_dir.parents[2]
        config = str(repo / "deploy" / "etc" / "engines" / "boost.config")
        work = tempfile.TemporaryDirectory()
        self.addCleanup(work.cleanup)
        root = Path(work.name)
        sockets = tempfile.mkdtemp(dir=os.environ.get("TI_TEST_SOCKET_DIR", "/tmp"))
        self.addCleanup(lambda: shutil.rmtree(sockets, True))
        env = dict(os.environ, GOTOOLCHAIN="local")

        def run(command, **kwargs):
            """command to completion within GO_TIMEOUT, else killed and the test fails with what it printed."""
            try:
                return subprocess.run(command, timeout=GO_TIMEOUT, capture_output=True, text=True, check=True, **kwargs)
            except subprocess.TimeoutExpired as err:
                self.fail("{} did not finish within {} s (TI_TEST_GO_TIMEOUT): {}".format(
                    command[0], GO_TIMEOUT, (err.stdout or b"") + (err.stderr or b"")))
            except FileNotFoundError:
                self.fail("{} is not on PATH: the real engine is built here, so a Go toolchain is required".format(command[0]))
            except subprocess.CalledProcessError as err:
                self.fail("{} failed: {}{}".format(command[0], err.stdout, err.stderr))

        engine_bin, demo_bin = str(root / "engine"), str(root / "boost-demo")
        run(["go", "build", "-ldflags",
             "-X github.com/richard-whittemore/TrendInvesting/internal/buildinfo.Version=resume-test",
             "-o", engine_bin, "./cmd/engine"], cwd=str(repo), env=env)
        run(["go", "build", "-o", demo_bin, "./cmd/boost-demo"], cwd=str(repo), env=env)
        logs = root / "engines"
        logs.mkdir()
        streams = root / "streams" / "boost"
        (streams / "run-a").mkdir(parents=True)
        socket = os.path.join(sockets, "s.sock")

        def stop(engine, sig):
            if engine.poll() is None:
                engine.send_signal(sig)
            try:
                engine.wait(timeout=ENGINE_TIMEOUT)
            except subprocess.TimeoutExpired:
                engine.kill()
                engine.wait()
                self.fail("the engine did not stop within {} s (TI_TEST_ENGINE_TIMEOUT)".format(ENGINE_TIMEOUT))

        def start(name, *extra):
            out = root / (name + ".out")
            with open(out, "w") as sink:
                engine = subprocess.Popen([engine_bin, "-mode", "dry-run", "-config", config,
                                           "-as-of", "2025-01-02T21:00:00Z", "-socket", socket,
                                           "-log", str(logs / (name + ".log")), "-out", str(root / (name + ".journal")),
                                           *extra], stdout=sink, stderr=subprocess.STDOUT)
            self.addCleanup(lambda: engine.poll() is None and (engine.kill(), engine.wait()))
            deadline = time.monotonic() + ENGINE_TIMEOUT
            while time.monotonic() < deadline:
                if "listening on" in out.read_text():
                    return engine
                self.assertIsNone(engine.poll(), out.read_text())
                time.sleep(0.05)
            self.fail("the engine did not listen within {} s (TI_TEST_ENGINE_TIMEOUT): {}".format(
                ENGINE_TIMEOUT, out.read_text()))

        crashed = start("run-a")
        run([demo_bin, "-socket", socket, "-log", str(logs / "run-a.log"), "-config", config, "-sessions", "40"])
        stop(crashed, signal.SIGKILL)
        resumed = start("run-c", "-resume", str(logs / "run-a.log"))
        first, copy = str(logs / "run-a.log"), str(logs / "run-c.log")
        a, c = _committed_groups(first), _committed_groups(copy)
        self.assertGreater(len(a.groups), 2)
        self.assertEqual(c.record_hashes[:len(a.record_hashes)], a.record_hashes)
        self.assertEqual((a.chain_break, c.chain_break), (None, None))
        self.assertEqual((tidata.read_engine_log(first).chain_break, tidata.read_engine_log(copy).chain_break),
                         (None, None))
        self.assertEqual(stream_run(str(streams), copy), "run-a")
        self.assertEqual(tidata.stream_run(str(streams), copy, tidata.read_engine_log(copy)), "run-a")
        stop(resumed, signal.SIGTERM)


if __name__ == "__main__":
    unittest.main()


class OutboxExtendTests(unittest.TestCase):
    def test_an_extend_that_would_name_an_input_twice_writes_nothing(self):
        with tempfile.TemporaryDirectory() as d:
            outbox = Outbox(str(Path(d) / "outbox"))
            entry = {"id": "a", "event_type": "t", "schema_version": 1, "event_time": "2027-01-14T21:00:00Z",
                     "payload": {}}
            outbox.append(entry)
            with self.assertRaises(OutboxInvalid):
                outbox.extend([dict(entry, payload={"x": 1})])
            with self.assertRaises(OutboxInvalid):
                outbox.extend([{"id": "b"}])
            self.assertEqual(outbox.load(), [entry])
