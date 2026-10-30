"""The IRA rehearsal and documented probe follow ADR 0026's D7-F/D7-L rule."""
import json
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[3]
CONFIG = "/etc/trend-investing/engines/trad-ira.config"


class ProbeRehearsalTests(unittest.TestCase):
    def version_assignment(self, path):
        text = (ROOT / path).read_text()
        match = re.search(r"^snapshot_version=\$\(.*?\n\)", text, re.M | re.S)
        self.assertIsNotNone(match, f"{path} must read the engine's quantity rule")
        return match.group()

    def test_rehearsal_and_documented_probe_select_the_configured_version(self):
        for path in ("deploy/rehearsal/first-day.sh",
                     "docs/runbooks/first-dry-run-day.md", "docs/manual/ibkr-adapter.md"):
            assignment = self.version_assignment(path)
            with tempfile.TemporaryDirectory() as folder:
                config = Path(folder) / "engine.config"
                for rule, version in (("fractional", "6"), ("minimum-lot", "4"),
                                      ("unknown", None), (None, None)):
                    with self.subTest(path=path, rule=rule):
                        config.write_text(json.dumps({"execution": {"share_quantity": rule}}))
                        command = assignment.replace(CONFIG, str(config)).replace("sudo python3", "python3")
                        result = subprocess.run(["bash", "-eu", "-c", command + '\nprintf "%s" "$snapshot_version"'],
                                                capture_output=True, text=True)
                        if version is None:
                            self.assertNotEqual(result.returncode, 0)
                        else:
                            self.assertEqual(result.returncode, 0, result.stderr)
                            self.assertEqual(result.stdout, version)

    def test_rehearsal_checks_the_selected_snapshot_version(self):
        text = (ROOT / "deploy/rehearsal/first-day.sh").read_text()
        assertion = next(line for line in text.splitlines()
                         if line.startswith('grep ') and '"type":"account.snapshot"' in line)
        with tempfile.TemporaryDirectory() as folder:
            log = Path(folder) / "run-ira-2026-10-29.log"
            for expected in (4, 6):
                for actual in (4, 6):
                    with self.subTest(expected=expected, actual=actual):
                        log.write_text(json.dumps({"type": "account.snapshot", "schema_version": actual},
                                                  separators=(",", ":")) + "\n")
                        command = 'fail() { exit 1; }; ira_engines=$1; snapshot_version=$2; ' + assertion
                        result = subprocess.run(["bash", "-c", command, "test", folder, str(expected)],
                                                capture_output=True, text=True)
                        self.assertEqual(result.returncode, 0 if expected == actual else 1, result.stderr)

    def test_probe_expectations_require_the_configured_version_and_holdings_policy(self):
        text = (ROOT / "deploy/rehearsal/first-day.sh").read_text()
        start = text.index('remap_expr="s/account snapshot version')
        setup = text[start:text.index("step ira-probe", start)]
        runbook = (ROOT / "docs/runbooks/first-dry-run-day.md").read_text()
        expected = runbook.split("<!-- expect: ira-probe -->", 1)[1].split("```text\n", 1)[1].split("```", 1)[0]
        for version in (4, 6):
            with self.subTest(version=version):
                result = subprocess.run(["bash", "-eu", "-c",
                                         'snapshot_version=$1; ' + setup + '\nsed -e "$remap_expr"',
                                         "test", str(version)], input=expected, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(f"account snapshot version {version} as of", result.stdout)
                self.assertNotIn(f"account snapshot version {10 - version} as of", result.stdout)
                self.assertEqual("probe: ok holdings:" in result.stdout, version == 6)
