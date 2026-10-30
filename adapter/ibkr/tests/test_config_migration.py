"""Execute the installed IRA policy migration documented in first-dry-run-day.md.

AGENTS.md durability and ADR 0026 D7-F require complete files and a stopped
adapter until both policies agree; fixtures contain no account identifiers.
"""
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch


class ConfigMigrationTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.engine = self.root / "engine.config"
        self.adapter = self.root / "adapter.json"
        self.engine.write_text(json.dumps({"execution": {"share_quantity": "minimum-lot"}}) + "\n")
        self.adapter.write_text(json.dumps({"account_snapshot_version": 4}) + "\n")
        self.engine.chmod(0o640)
        self.adapter.chmod(0o600)
        self.original = (self.engine.read_bytes(), self.adapter.read_bytes())
        manual = Path(__file__).resolve().parents[3] / "docs/runbooks/first-dry-run-day.md"
        block = manual.read_text().split("<!-- rehearsed: ira-account -->", 1)[1].split("```", 2)[1]
        self.code = block.split("<<'PY'\n", 1)[1].split("\nPY\n", 1)[0]
        self.code = self.code.replace('"/etc/trend-investing/adapters/trad-ira.json"', repr(str(self.adapter)))

    def migrate(self):
        with patch.object(sys, "argv", ["migration", str(self.engine), str(self.adapter)]):
            exec(compile(self.code, "runbook-ira-migration", "exec"), {})

    def test_unreadable_or_malformed_adapter_does_not_switch_engine(self):
        for contents in (None, "{broken", "[]"):
            with self.subTest(contents=contents):
                self.engine.write_bytes(self.original[0])
                if contents is None:
                    self.adapter.unlink(missing_ok=True)
                else:
                    self.adapter.write_text(contents)
                with self.assertRaises((OSError, ValueError, TypeError)):
                    self.migrate()
                self.assertEqual(self.engine.read_bytes(), self.original[0])

    def test_staging_failure_preserves_both_installed_files(self):
        for synced in (0, 1):
            with self.subTest(synced=synced):
                with patch.object(os, "fsync", side_effect=[None] * synced + [OSError("synthetic disk failure")]):
                    with self.assertRaisesRegex(OSError, "synthetic disk failure"):
                        self.migrate()
                self.assertEqual((self.engine.read_bytes(), self.adapter.read_bytes()), self.original)
                self.assertEqual(sorted(p.name for p in self.root.iterdir()), ["adapter.json", "engine.config"])

    def test_replace_failure_preserves_complete_installed_files(self):
        with patch.object(os, "replace", side_effect=OSError("synthetic replace failure")):
            with self.assertRaisesRegex(OSError, "synthetic replace failure"):
                self.migrate()
        self.assertEqual((self.engine.read_bytes(), self.adapter.read_bytes()), self.original)

    def test_second_replace_failure_is_repaired_by_rerunning_while_stopped(self):
        replace = os.replace

        def fail_adapter(source, destination):
            if destination == self.adapter:
                raise OSError("synthetic adapter replace failure")
            replace(source, destination)

        with patch.object(os, "replace", side_effect=fail_adapter):
            with self.assertRaisesRegex(OSError, "synthetic adapter replace failure"):
                self.migrate()
        self.assertEqual(json.loads(self.engine.read_text())["execution"]["share_quantity"], "fractional")
        self.assertEqual(self.adapter.read_bytes(), self.original[1])
        self.migrate()
        self.assertEqual(json.loads(self.adapter.read_text())["account_snapshot_version"], 6)
        self.assertEqual(sorted(p.name for p in self.root.iterdir()), ["adapter.json", "engine.config"])

    def test_migration_updates_both_policies_and_preserves_permissions(self):
        self.migrate()
        self.assertEqual(json.loads(self.engine.read_text())["execution"], {
            "share_quantity": "fractional", "fractional_increment": "0.0001"})
        self.assertEqual(json.loads(self.adapter.read_text())["account_snapshot_version"], 6)
        self.assertEqual(self.engine.stat().st_mode & 0o777, 0o640)
        self.assertEqual(self.adapter.stat().st_mode & 0o777, 0o600)
        self.assertEqual(sorted(p.name for p in self.root.iterdir()), ["adapter.json", "engine.config"])
