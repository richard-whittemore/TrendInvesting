"""Balance-key diagnostics follow the manual's read-only and L12 privacy rules."""
import contextlib
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))

from fake_tws import ACCOUNT, OTHER_ACCOUNT, FakeTWS
from test_connection import config
from cli import main


class AccountKeysTests(unittest.TestCase):
    def setUp(self):
        self.folder = tempfile.TemporaryDirectory()
        self.addCleanup(self.folder.cleanup)
        self.path = Path(self.folder.name) / "adapter.json"
        self.cfg = config(account_snapshot_version=6)
        values = self.cfg._asdict()
        values["con_ids"] = dict(values["con_ids"])
        self.path.write_text(json.dumps(values))
        self.fake = FakeTWS()
        self.fake.ledger_rows = False
        self.fake.summary = [(ACCOUNT, key, "987654.321", "USD") for key in
                             ("TotalCashValue", "SettledCash", "NetLiquidation", "ExcessLiquidity")]
        self.out = io.StringIO()
        self.err = io.StringIO()
        self.seen_config = None

    def run_command(self, *extra):
        def factory(cfg):
            self.seen_config = cfg
            return self.fake
        status = main(["account-keys", "--config", str(self.path), "--client-id", "19",
                       "--timeout", "0.01", *extra], api_factory=factory, out=self.out, err=self.err,
                      engine_connect=lambda *args: self.fail("diagnostic connected to engine"))
        self.assertLessEqual({call[0] for call in self.fake.calls},
                             {"connect", "req_account_updates", "disconnect"})
        return status, self.out.getvalue() + self.err.getvalue()

    def test_sorted_distinct_pairs_without_amounts_or_accounts_and_read_only_requests(self):
        self.fake.summary += [(ACCOUNT, "CashBalance", "987654.321", "BASE"),
                              (ACCOUNT, "CashBalance", "987654.321", "USD"),
                              (ACCOUNT, "CashBalance", "987654.321", "USD"),
                              (OTHER_ACCOUNT, "WrongAccount", "987654.321", "USD")]
        status, output = self.run_command()
        self.assertEqual(status, 0, output)
        pairs = [line for line in output.splitlines() if line.startswith("account-keys: key=")]
        self.assertEqual(pairs, ["account-keys: key=CashBalance currency=BASE",
                                "account-keys: key=CashBalance currency=USD",
                                "account-keys: key=ExcessLiquidity currency=USD",
                                "account-keys: key=NetLiquidation currency=USD",
                                "account-keys: key=SettledCash currency=USD",
                                "account-keys: key=TotalCashValue currency=USD"])
        for secret in (ACCOUNT, OTHER_ACCOUNT, "987654.321", "WrongAccount"):
            self.assertNotIn(secret, output)
        self.assertIn("account-keys: download ended", output)
        for key in ("NetLiquidation", "TotalCashValue", "ExcessLiquidity", "SettledCash"):
            self.assertIn(f"account-keys: needed {key} present", output)
        self.assertEqual(self.fake.called("connect"), [("connect", self.cfg.host, self.cfg.port, 19)])
        self.assertEqual(self.fake.called("req_account_updates"),
                         [("req_account_updates", True, ACCOUNT), ("req_account_updates", False, ACCOUNT)])
        self.assertFalse(self.seen_config.paper_sending_enabled)
        self.assertFalse(self.fake.connected)

    def test_segment_settled_key_does_not_replace_missing_total(self):
        self.fake.summary = [(a, "SettledCash-S" if k == "SettledCash" else k, v, c)
                             for a, k, v, c in self.fake.summary]
        self.fake.summary.append((ACCOUNT, "unSETTLED", "987654.321", "USD"))
        status, output = self.run_command()
        self.assertEqual(status, 1)
        self.assertIn("account-keys: needed SettledCash MISSING", output)
        self.assertIn("account-keys: Settled key=SettledCash-S currency=USD", output)
        self.assertIn("account-keys: Settled key=unSETTLED currency=USD", output)

    def test_download_never_ends(self):
        self.fake.end_summary = False
        status, output = self.run_command()
        self.assertEqual(status, 1)
        self.assertIn("account-keys: download did NOT end within 0.01s", output)
        self.assertEqual(self.fake.called("req_account_updates")[-1], ("req_account_updates", False, ACCOUNT))
        self.assertFalse(self.fake.connected)

    def test_connection_failure_has_reason_and_exits_two(self):
        self.fake.connect_behaviours = ["socket-error"]
        status, output = self.run_command()
        self.assertEqual(status, 2)
        self.assertIn("connection failed", output)
        self.assertIn("synthetic connection refused", output)
        self.assertFalse(self.fake.connected)

    def test_disconnect_during_download_fails_closed(self):
        self.fake.drop_during_summary = True
        status, output = self.run_command()
        self.assertEqual(status, 2)
        self.assertIn("connection failed", output)
        self.assertFalse(self.fake.connected)

    def test_account_update_error_fails_closed_and_masks_accounts(self):
        self.fake.summary_error = 2101
        self.fake.summary_error_message = "refused " + ACCOUNT + " " + OTHER_ACCOUNT
        status, output = self.run_command()
        self.assertEqual(status, 1)
        self.assertIn("2101", output)
        self.assertNotIn(ACCOUNT, output)
        self.assertNotIn(OTHER_ACCOUNT, output)
        self.assertFalse(self.fake.connected)

    def test_masks_accounts_even_in_key_and_currency_fields(self):
        self.fake.summary.append((ACCOUNT, "key " + ACCOUNT, "987654.321", OTHER_ACCOUNT))
        status, output = self.run_command()
        self.assertEqual(status, 0)
        self.assertNotIn(ACCOUNT, output)
        self.assertNotIn(OTHER_ACCOUNT, output)

    def test_refuses_adapters_own_client_id_before_connecting(self):
        status, output = self.run_command("--client-id", str(self.cfg.client_id))
        self.assertEqual(status, 1)
        self.assertIn("unused", output)
        self.assertEqual(self.fake.calls, [])

    def test_secret_file_configuration_uses_same_loader(self):
        values = json.loads(self.path.read_text())
        del values["account_id"]
        self.path.write_text(json.dumps(values))
        secret = Path(self.folder.name) / "account.env"
        secret.write_text("IBKR_ACCOUNT_TRAD_IRA=" + ACCOUNT + "\n")
        status, output = self.run_command("--secrets", str(secret), "--account-key", "IBKR_ACCOUNT_TRAD_IRA")
        self.assertEqual(status, 0, output)
        self.assertNotIn(ACCOUNT, output)

    def test_required_unused_positive_client_id_and_finite_positive_timeout(self):
        for extra in (("--client-id", "0"), ("--client-id", "-1"),
                      ("--timeout", "0"), ("--timeout", "nan"), ("--timeout", "inf")):
            with self.subTest(extra=extra):
                status, output = self.run_command(*extra)
                self.assertEqual(status, 1)
                self.assertEqual(self.fake.calls, [])
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as caught:
            main(["account-keys", "--config", str(self.path)])
        self.assertEqual(caught.exception.code, 2)

    def test_help_states_read_only_scope_and_default_timeout(self):
        output = io.StringIO()
        with contextlib.redirect_stdout(output), self.assertRaises(SystemExit) as caught:
            main(["account-keys", "--help"])
        self.assertEqual(caught.exception.code, 0)
        text = output.getvalue()
        for phrase in ("read-only", "orders", "positions", "engine", "60", "unused"):
            self.assertIn(phrase, text)

    def test_only_this_downloads_rows_and_its_own_end_count(self):
        connect = self.fake.connect
        subscribe = self.fake.req_account_updates

        def with_earlier_rows(*args):
            connect(*args)
            self.fake.callbacks.account_value("Earlier", "987654.321", "USD", ACCOUNT)
            self.fake.callbacks.account_download_end(ACCOUNT)

        def with_later_rows(enabled, account):
            if enabled:
                self.fake.callbacks.account_download_end(OTHER_ACCOUNT)
            subscribe(enabled, account)
            if enabled:
                self.fake.callbacks.account_value("Later", "987654.321", "USD", ACCOUNT)

        with patch.object(self.fake, "connect", side_effect=with_earlier_rows), \
                patch.object(self.fake, "req_account_updates", side_effect=with_later_rows):
            status, output = self.run_command()
        self.assertEqual(status, 0, output)
        self.assertNotIn("Earlier", output)
        self.assertNotIn("Later", output)

    def test_unsubscribe_failure_still_disconnects_and_returns_two(self):
        subscribe = self.fake.req_account_updates

        def fail_unsubscribe(enabled, account):
            if not enabled:
                raise OSError("unsubscribe failed for " + ACCOUNT)
            subscribe(enabled, account)

        with patch.object(self.fake, "req_account_updates", side_effect=fail_unsubscribe):
            status, output = self.run_command()
        self.assertEqual(status, 2)
        self.assertIn("unsubscribe failed", output)
        self.assertNotIn(ACCOUNT, output)
        self.assertFalse(self.fake.connected)
