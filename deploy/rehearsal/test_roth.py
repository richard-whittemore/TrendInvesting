"""Synthetic deployment checks: ADR 0026 account isolation and docs/live-trading/lessons-2026-10.md L3/L8."""
import json
from pathlib import Path
import re
import sys
import tempfile
import unittest
from types import SimpleNamespace

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "adapter/ibkr"))
from cli import Refused, load_config


class RothDeploymentTests(unittest.TestCase):
    def test_roth_adapter_uses_its_own_settings_and_refuses_other_clients(self):
        settings = dict(line.split("=", 1) for line in
                        (ROOT / "deploy/etc/engines/roth-ira.env").read_text().splitlines()
                        if line and not line.startswith("#"))
        self.assertEqual(settings["TI_ADAPTER_CLIENT_ID"], "13")
        self.assertEqual(settings["TI_MODE"], "dry-run")
        self.assertEqual(settings["TI_ADAPTER_ACCOUNT_KEY"], "IBKR_ACCOUNT_ROTH_IRA")
        unit = (ROOT / "deploy/systemd/ti-adapter@.service").read_text()
        self.assertIn("EnvironmentFile=/etc/trend-investing/engines/%i.env", unit)
        self.assertIn("--config /etc/trend-investing/adapters/%i.json --client-id ${TI_ADAPTER_CLIENT_ID}", unit)
        self.assertIn("LoadCredential=account:/run/trend-investing/secrets/accounts/%i.env", unit)
        values = json.loads((ROOT / "deploy/etc/adapters/roth-ira.json.example").read_text())
        self.assertNotIn("account_id", values)
        with tempfile.TemporaryDirectory() as folder:
            config, secret = Path(folder) / "roth-ira.json", Path(folder) / "account"
            secret.write_text("IBKR_ACCOUNT_ROTH_IRA=synthetic-roth\n")
            args = SimpleNamespace(config=str(config), secrets=str(secret),
                                   account_key=settings["TI_ADAPTER_ACCOUNT_KEY"], client_id=13)
            config.write_text(json.dumps(values))
            cfg = load_config(args)
            self.assertEqual((cfg.client_id, cfg.account_id, cfg.account_snapshot_version,
                              cfg.resolve_contracts, cfg.paper_sending_enabled),
                             (13, "synthetic-roth", 4, True, False))
            for other in (11, 12):
                values["client_id"] = other
                config.write_text(json.dumps(values))
                with self.assertRaisesRegex(Refused, "each account's adapter has its own client ID"):
                    load_config(args)

    def test_flex_start_windows_are_disjoint_and_data_follows_roth(self):
        windows = set()
        for account, minute, hours in (("boost", 30, (5, 6, 7)),
                                       ("trad-ira", 45, (5, 6, 7)),
                                       ("roth-ira", 0, (6, 7, 8))):
            text = (ROOT / f"deploy/systemd/ti-flex@{account}.timer.d/stagger.conf").read_text()
            self.assertIn("OnCalendar=\n", text, "reset the template's calendar")
            times = re.findall(r"^OnCalendar=Mon\.\.Fri (\d+):(\d+) America/New_York$", text, re.M)
            self.assertEqual([(int(h), int(m)) for h, m in times], [(h, minute) for h in hours])
            # L3's 15-minute stagger separates starts; a slow Flex query can
            # outlive it, so this asserts the scheduled windows, not completion.
            for h, m in times:
                window = set(range(int(h) * 60 + int(m), int(h) * 60 + int(m) + 15))
                self.assertFalse(windows & window, account)
                windows |= window
        text = (ROOT / "deploy/systemd/ti-data@roth-ira.timer.d/stagger.conf").read_text()
        self.assertIn("OnCalendar=\n", text)
        self.assertEqual(re.findall(r"^OnCalendar=(.+)$", text, re.M),
                         [f"Mon..Fri {h:02}:30 America/New_York" for h in (6, 7, 8)])

    def test_roth_failures_page_and_reporters_watch_them(self):
        for kind in ("engine", "adapter", "flex", "data"):
            text = (ROOT / f"deploy/systemd/ti-{kind}@.service").read_text()
            self.assertIn("OnFailure=ti-unit-failed@%n.service", text)
        text = (ROOT / "deploy/systemd/ti-reporter@.service").read_text()
        for flag, units in (("units", ("engine", "adapter")), ("resolve-units", ("data", "flex"))):
            watched = re.search(rf" -{flag} (\S+)", text).group(1).split(",")
            for kind in units:
                self.assertIn(f"ti-{kind}@roth-ira.service", watched)

    def test_measurement_installed_with_calendar_but_owner_enables_it(self):
        unit = (ROOT / "deploy/systemd/ti-memory.service").read_text()
        self.assertIn("memory.py sample", unit)
        self.assertIn("OnFailure=ti-unit-failed@%n.service", unit)
        timer = (ROOT / "deploy/systemd/ti-memory.timer").read_text()
        self.assertIn("OnCalendar=*-*-* *:0/5:00 UTC", timer)


if __name__ == "__main__":
    unittest.main()
