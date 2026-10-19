"""deploy/rehearse-paper.sh's rehearsal passes here, and fails, exiting 1, when the adapter stops.

The rehearsal runs synthetic paper days with the real engine (cmd/engine,
Boost, paper mode): the data job's warm-up and day's stream written by
ti-data from synthetic Massive answers, delivered by the adapter behind one
account reading as of each close (the owner's decision of 2026-10-18), the
engine's decision and the release of one order, the order sent once,
accepted, reconciled across a reconnect and a restart; the next day's fills
after the close, which make the reading stand at its read time and the
engine withhold; a crash resume of the engine under a new run name that
takes the next day's stream from the run's own folder; and a holding no
order explains, which takes the engine out of Normal at the next start.
"""
import io
import os
import sys
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from unittest import mock

tests_dir = str(Path(__file__).resolve().parent)
if tests_dir not in sys.path:
    sys.path.insert(0, tests_dir)

import rehearse_paper
from fake_tws import FakeTWS


class RehearsalTests(unittest.TestCase):
    def run_rehearsal(self, **env):
        out = io.StringIO()
        with mock.patch.dict(os.environ, env), redirect_stdout(out):
            status = rehearse_paper.main()
        return status, out.getvalue()

    def test_the_rehearsal_passes_against_the_fake_gateway(self):
        status, out = self.run_rehearsal(TI_REHEARSAL_ACCOUNT="")
        self.assertEqual(status, 0, out)
        self.assertEqual(out.count("PASS:"), 12)
        self.assertNotIn("FAIL", out)
        for line in ("PASS: the warm-up reached the engine behind one reading as of that evening's close, in file "
                     "order, and the engine matched the start-of-run reconciliation",
                     "PASS: the day's stream reached the engine as its account and broker snapshots as of the close "
                     "(read after it), its bar, its mark from the bar's official close, then its close",
                     "PASS: the engine decided the day and its durable log shows one buy released to be sent",
                     "PASS: the order went out once, only after the day's close released it, as released",
                     "PASS: the reconnect and a restart reconciled the account again, the engine matched both, and "
                     "nothing was sent twice",
                     "PASS: with an execution since the close, the reading was stamped at its read time with the "
                     "reason, and the engine withheld the session as stale-account-snapshot, ordering nothing",
                     "PASS: after a crash resume under a new run name, the next day's stream went from the run's own "
                     "folder to the resumed run, once, as of its close, and the resumed engine decided it; nothing "
                     "earlier was sent again",
                     "PASS: a holding no journalled order explains was found at the next start's reconciliation, and "
                     "the engine left Normal: Degraded on SPY (ADR 0019)"):
            self.assertIn(line, out)

    def test_an_account_not_beginning_du_fails_before_anything_runs(self):
        status, out = self.run_rehearsal(TI_REHEARSAL_ACCOUNT="U1234567")
        self.assertEqual(status, 1)
        self.assertNotIn("U1234567", out, "the account is never printed")

    def test_the_owners_account_is_used_and_never_printed(self):
        status, out = self.run_rehearsal(TI_REHEARSAL_ACCOUNT="DU7654321")
        self.assertEqual(status, 0, out)
        self.assertNotIn("DU7654321", out)

    def test_a_stream_the_adapter_refuses_fails_the_rehearsal(self):
        with mock.patch.object(rehearse_paper, "STREAM_TAMPER", True):
            status, out = self.run_rehearsal(TI_REHEARSAL_ACCOUNT="")
        self.assertEqual(status, 1)
        self.assertIn("FAIL: the adapter stopped", out)
        self.assertIn("StreamRefused", out)

    def test_an_adapter_that_stops_fails_the_rehearsal(self):
        class Refusing(FakeTWS):
            def place_order(self, order_id, contract_, order_):
                raise ConnectionError("synthetic: the gateway refused the socket write")
        with mock.patch.object(rehearse_paper, "FakeTWS", Refusing):
            status, out = self.run_rehearsal(TI_REHEARSAL_ACCOUNT="")
        self.assertEqual(status, 1)
        self.assertIn("FAIL: the adapter stopped", out)


if __name__ == "__main__":
    unittest.main()
