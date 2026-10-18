"""The adapter's configuration names where to connect and never carries a credential."""
import sys
import unittest
from pathlib import Path

tests_dir = str(Path(__file__).resolve().parent)
if tests_dir not in sys.path:
    sys.path.insert(0, tests_dir)

from test_connection import config
from config import AdapterConfig, ConfigError


class ConfigTests(unittest.TestCase):
    def test_valid_configuration(self):
        c = config()
        self.assertEqual((c.host, c.port, c.client_id, c.mode), ("127.0.0.1", 4002, 11, "dry-run"))

    def test_credential_keys_are_refused_without_echoing_the_value(self):
        for key in ("password", "ibkr_password", "token", "secret", "api_key"):
            with self.assertRaises(ConfigError) as caught:
                config(**{key: "hunter2-synthetic"})
            self.assertNotIn("hunter2", str(caught.exception))
            self.assertIn(key, str(caught.exception))

    def test_unknown_keys_are_refused(self):
        with self.assertRaises(ConfigError):
            config(colour="blue")

    def test_every_timing_value_is_required(self):
        values = dict(host="127.0.0.1", port=4002, client_id=11, account_id="DU0000001",
                      mode="dry-run")
        with self.assertRaises(ConfigError) as caught:
            AdapterConfig.from_mapping(values)
        self.assertIn("connect_timeout", str(caught.exception))

    def test_client_id_zero_is_refused(self):
        with self.assertRaises(ConfigError):
            config(client_id=0)

    def test_bad_port_and_mode_are_refused(self):
        for bad in (dict(port=0), dict(port=70000), dict(port="4002"), dict(mode="yolo"),
                    dict(client_id=True), dict(account_id=""), dict(connect_timeout=0),
                    dict(backoff_max=0.5), dict(max_connect_attempts=0)):
            with self.subTest(bad=bad), self.assertRaises(ConfigError):
                config(**bad)

    def test_the_account_is_bound_to_the_mode(self):
        # IBKR paper account IDs begin "DU" and live ones do not, so a paper
        # configuration can never point the adapter at a live account.
        self.assertEqual(config(mode="paper", account_id="DU0000001").mode, "paper")
        for bad in (dict(mode="paper", account_id="U0000001"), dict(mode="live", account_id="DU0000001")):
            with self.subTest(bad=bad), self.assertRaises(ConfigError) as caught:
                config(**bad)
            self.assertIn("DU", str(caught.exception))

    def test_timing_values_must_be_finite(self):
        # NaN and infinity both pass a "> 0" test, and either would switch the
        # heartbeat deadline or the connect timeout off.
        for name in ("connect_timeout", "request_timeout", "heartbeat_interval",
                     "heartbeat_timeout", "restore_timeout", "backoff_initial", "backoff_max"):
            for bad in (float("nan"), float("inf")):
                with self.subTest(name=name, value=bad), self.assertRaises(ConfigError):
                    config(**{name: bad})


if __name__ == "__main__":
    unittest.main()
