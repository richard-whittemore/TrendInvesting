"""The adapter reads the engine's durable log for the run's identity and the inputs it has committed (ADR 0027)."""
import sys
import tempfile
import unittest
from pathlib import Path

tests_dir = str(Path(__file__).resolve().parent)
if tests_dir not in sys.path:
    sys.path.insert(0, tests_dir)

from relay import ResolveRefused, journalled_reports, read_engine_log
from test_relay import LOG_CONFIGURATION, LOG_STRATEGY, fill_payload, log_group, log_header

TESTDATA = Path(__file__).resolve().parent / "testdata"


class EngineLogTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.path = Path(self.dir.name) / "engine.log"

    def write(self, *groups, tail=""):
        self.path.write_text(log_header() + "".join("".join(g) for g in groups) + tail)

    def test_the_run_identity_and_every_committed_input_in_order(self):
        self.write(log_group(1, [("input", "market.session.closed", 1, {"period_end": "x"}),
                                 ("decision", "risk.order.released", 1, {"send": True})]),
                   log_group(3, [("input", "execution.fill", 5, fill_payload("e.1.01"))]))
        log = read_engine_log(str(self.path))
        self.assertEqual((log.configuration_hash, log.strategy_version), (LOG_CONFIGURATION, LOG_STRATEGY))
        self.assertEqual([(i.id, i.event_type, i.schema_version, i.sequence) for i in log.inputs],
                         [("e:1", "market.session.closed", 1, 1), ("e:3", "execution.fill", 5, 3)])
        self.assertEqual(log.inputs[1].payload, fill_payload("e.1.01"))
        self.assertEqual(log.next_sequence, 4)

    def test_a_group_not_yet_committed_holds_no_input(self):
        self.write(log_group(1, [("input", "market.session.closed", 1, {"period_end": "x"})]),
                   log_group(2, [("input", "execution.fill", 5, fill_payload("e.1.01"))], committed=False))
        log = read_engine_log(str(self.path))
        self.assertEqual([i.id for i in log.inputs], ["e:1"])
        self.assertEqual(log.next_sequence, 2)

    def test_a_log_with_no_input_yet_starts_at_sequence_one(self):
        self.write()
        self.assertEqual(read_engine_log(str(self.path)).next_sequence, 1)

    def test_the_engines_own_log_is_read(self):
        log = read_engine_log(str(TESTDATA / "engine_log.jsonl"))
        self.assertTrue(log.inputs)
        self.assertEqual(log.next_sequence, max(i.sequence for i in log.inputs) + 1)
        # The same reading as the owner's commands make of it.
        fills = journalled_reports(str(TESTDATA / "engine_log.jsonl")).fills
        self.assertEqual({i.payload["fill_id"] for i in log.inputs if i.event_type == "execution.fill"}, set(fills))

    def test_an_unreadable_log_is_refused_never_read_as_empty(self):
        with self.assertRaises(ResolveRefused):
            read_engine_log(str(self.path))
        self.path.write_text('{"journal_version":1}\n')
        with self.assertRaises(ResolveRefused):
            read_engine_log(str(self.path))
        self.write(log_group(2, [("input", "market.session.closed", 1, {})]))
        with self.assertRaises(ResolveRefused):
            read_engine_log(str(self.path))


if __name__ == "__main__":
    unittest.main()
