"""Paper rehearsal failure evidence (docs/runbooks/first-paper-order.md, B3)."""
import importlib.util
import io
import os
from contextlib import redirect_stdout
from pathlib import Path
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).resolve().parents[2] / "adapter/ibkr/tests/rehearse_paper.py"
spec = importlib.util.spec_from_file_location("paper_diagnostic", SCRIPT)
paper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(paper)


class PaperDiagnosticTests(unittest.TestCase):
    def run_failure(self, adapter_log=None):
        failure = paper.RehearsalFailed("synthetic engine build failed")

        def fail_before_start(work, account):
            if adapter_log is not None:
                (work / "adapter.log").write_text(adapter_log)
            (work / "engine.out").write_text("synthetic build output")
            raise failure

        output = io.StringIO()
        with patch.dict(os.environ, TI_REHEARSAL_ACCOUNT=""), \
                patch.object(paper, "rehearse", side_effect=fail_before_start) as rehearse, \
                redirect_stdout(output):
            status = paper.main()
        rehearse.assert_called_once()
        self.assertEqual(status, 1)
        return output.getvalue()

    def test_failure_before_adapter_log_exists_preserves_original_diagnostic(self):
        self.assertEqual(self.run_failure(),
                         "FAIL: synthetic engine build failed\n"
                         "adapter.log does not exist yet\n"
                         "engine.out:\nsynthetic build output\n")

    def test_existing_adapter_log_follows_original_failure(self):
        self.assertEqual(self.run_failure("synthetic adapter output"),
                         "FAIL: synthetic engine build failed\n"
                         "adapter log:\nsynthetic adapter output\n"
                         "engine.out:\nsynthetic build output\n")


if __name__ == "__main__":
    unittest.main()
