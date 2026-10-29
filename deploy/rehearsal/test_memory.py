"""Memory evidence seam, synthetic samples only (docs/live-trading/lessons-2026-10.md L1)."""
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from datetime import datetime, timedelta, timezone

SCRIPT = Path(__file__).resolve().parents[1] / "memory.py"
START = datetime(2026, 10, 19, tzinfo=timezone.utc)


class MemoryRecordTests(unittest.TestCase):
    def setUp(self):
        folder = tempfile.TemporaryDirectory()
        self.addCleanup(folder.cleanup)
        self.record = Path(folder.name) / "memory.jsonl"

    def samples(self, missing=None):
        rows = []
        for day in range(7):
            if day == missing:
                continue
            for minute, current in ((0, 100), (5, 300), (10, 500)):
                rows.append(dict(schema_version=1, week_start="2026-10-19",
                                 at=(START + timedelta(days=day, minutes=minute)).isoformat(),
                                 schedule={"ti-memory.timer": "OnCalendar=*-*-* *:0/5:00 UTC"},
                                 units={"ti-engine@roth-ira.service":
                                        dict(current_bytes=current, peak_bytes=600, active_state="active")},
                                 server=dict(total_bytes=2048, available_bytes=2048-current,
                                             used_bytes=current)))
        self.record.write_text("".join(json.dumps(row) + "\n" for row in rows))

    def report(self):
        return subprocess.run([sys.executable, str(SCRIPT), "report", "--record", str(self.record),
                               "--week-start", "2026-10-19"], capture_output=True, text=True)

    def test_full_week_peak_median_and_schedule_are_in_the_record(self):
        self.samples()
        result = self.report()
        self.assertEqual(result.returncode, 0, result.stderr)
        report = json.loads(result.stdout)
        self.assertEqual(report["server"]["peak_bytes"], 500)
        self.assertEqual(report["server"]["median_bytes"], 300)
        unit = report["units"]["ti-engine@roth-ira.service"]
        self.assertEqual((unit["peak_bytes"], unit["median_bytes"], unit["sampled_peak_bytes"]), (600, 300, 500))
        self.assertEqual(report["samples_per_day"], {f"2026-10-{day}": 3 for day in range(19, 26)})
        self.assertIn("OnCalendar=", report["schedules"][0]["ti-memory.timer"])

    def test_each_missing_day_fails_loudly(self):
        for day in range(7):
            with self.subTest(day=day):
                self.samples(missing=day)
                result = self.report()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(f"missing samples for 2026-10-{19+day}", result.stderr)
                self.assertEqual(result.stdout, "", "never publish a partial week's report")

    def test_sampler_appends_actual_counters_server_total_and_schedule(self):
        spec = importlib.util.spec_from_file_location("memory", SCRIPT)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        calls = []

        def systemctl(*args):
            calls.append(args)
            if args[0] == "list-units":
                return "ti-engine@roth-ira.service loaded active running Engine\n"
            if args[0] == "list-unit-files":
                return "ti-memory.timer enabled enabled\n"
            if args[0] == "cat":
                return "[Timer]\nOnCalendar=*-*-* *:0/5:00 UTC\n"
            return "LoadState=loaded\nActiveState=active\nMemoryCurrent=512\nMemoryPeak=768\n"

        meminfo = self.record.parent / "meminfo"
        meminfo.write_text("MemTotal: 2048 kB\nMemAvailable: 1024 kB\n")
        for minute in (0, 5):
            module.sample(self.record, "2026-10-19", now=START + timedelta(minutes=minute),
                          systemctl=systemctl, meminfo=meminfo)
        rows = [json.loads(line) for line in self.record.read_text().splitlines()]
        self.assertEqual(len(rows), 2)
        self.assertEqual(rows[0]["server"]["used_bytes"], 1024 * 1024)
        self.assertEqual(rows[0]["units"]["ti-engine@roth-ira.service"]["peak_bytes"], 768)
        self.assertIn("ti-gateway.service", rows[0]["units"])
        self.assertIn("OnCalendar=", rows[0]["schedule"]["ti-memory.timer"])
        module.sample(self.record, "2026-10-19", now=START + timedelta(days=7),
                      systemctl=systemctl, meminfo=meminfo)
        self.assertEqual(len(self.record.read_text().splitlines()), 2, "week ends automatically")


if __name__ == "__main__":
    unittest.main()
