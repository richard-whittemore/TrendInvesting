"""deploy/rehearse-paper.sh's rehearsal passes here, and fails, exiting 1, when the adapter stops."""
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
        self.assertEqual(out.count("PASS:"), 8)
        self.assertNotIn("FAIL", out)

    def test_an_account_not_beginning_du_fails_before_anything_runs(self):
        status, out = self.run_rehearsal(TI_REHEARSAL_ACCOUNT="U1234567")
        self.assertEqual(status, 1)
        self.assertNotIn("U1234567", out, "the account is never printed")

    def test_the_owners_account_is_used_and_never_printed(self):
        status, out = self.run_rehearsal(TI_REHEARSAL_ACCOUNT="DU7654321")
        self.assertEqual(status, 0, out)
        self.assertNotIn("DU7654321", out)

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
