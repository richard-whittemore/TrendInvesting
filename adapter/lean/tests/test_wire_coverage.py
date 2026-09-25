"""Issue #31: a test that fails if a Go event type or schema version exists
that the LEAN adapter's fixtures don't cover.

internal/event has no runtime registry of its own event types (there is no
event-type "list" to import), so tests/testdata/wire_coverage.go is the
explicit list ADR 0015 and the ticket ask for: it parses internal/event's own
source for every `XxxEventType`/`XxxSchemaVersion` pair, and fails closed if
that discovered set disagrees with its own two hand-maintained lists -- what
the adapter's fixtures cover, and what deliberately does not cross the LEAN
boundary yet. This test only has to run it and check it is happy; the
substance of the check lives in the Go file, in the same place as every other
*_contract.go check in this directory.
"""
import subprocess
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parents[3]


class WireCoverageTests(unittest.TestCase):
    def test_every_go_event_type_is_covered_or_explicitly_excused(self):
        result = subprocess.run(
            ["go", "run", "./adapter/lean/tests/testdata/wire_coverage.go"],
            cwd=REPO, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
