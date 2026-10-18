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

    def test_the_wait_for_a_commission_report_is_required(self):
        values = config()._asdict()
        del values["commission_wait"]
        with self.assertRaises(ConfigError) as caught:
            AdapterConfig.from_mapping(values)
        self.assertIn("commission_wait", str(caught.exception))

    def test_the_contract_map_is_configuration_read_strictly(self):
        # The engine's instrument ID to IBKR's contract ID: the only way the
        # adapter names a contract, so it is required and read strictly.
        self.assertEqual(dict(config(con_ids={"SPY": 756733}).con_ids), {"SPY": 756733})
        values = config()._asdict()
        del values["con_ids"]
        with self.assertRaises(ConfigError) as caught:
            AdapterConfig.from_mapping(values)
        self.assertIn("con_ids", str(caught.exception))
        for bad in (["SPY", 756733], {"SPY": 0}, {"SPY": -1}, {"SPY": True}, {"SPY": "756733"}, {"SPY": 7.5},
                    {"": 756733}, {7: 756733}):
            with self.subTest(con_ids=bad), self.assertRaises(ConfigError):
                config(con_ids=bad)
        source = {"SPY": 756733}
        c = config(con_ids=source)
        source["SPY"] = 1
        self.assertEqual(c.con_ids["SPY"], 756733, "the configuration keeps its own copy")
        with self.assertRaises(TypeError):
            c.con_ids["QQQ"] = 320227571

    def test_a_contract_map_giving_two_instruments_one_contract_id_is_refused_naming_both(self):
        # One contract ID for two instruments would send one instrument's
        # order on the other's contract.
        with self.assertRaises(ConfigError) as caught:
            config(con_ids={"SPY": 756733, "VOO": 756733, "QQQ": 320227571})
        message = str(caught.exception)
        self.assertIn("SPY", message)
        self.assertIn("VOO", message)
        self.assertIn("756733", message)

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

    def test_paper_sending_is_off_unless_the_configuration_switches_it_on(self):
        # The one switch that lets the real client place an order: absent
        # means off, and only a strict true on a paper configuration is on.
        self.assertIs(config().paper_sending_enabled, False)
        self.assertIs(config(mode="paper").paper_sending_enabled, False)
        self.assertIs(config(mode="paper", paper_sending_enabled=False).paper_sending_enabled, False)
        self.assertIs(config(mode="paper", paper_sending_enabled=True).paper_sending_enabled, True)
        for bad in (1, 0, "true", "yes", None, [True]):
            with self.subTest(value=bad), self.assertRaises(ConfigError) as caught:
                config(mode="paper", paper_sending_enabled=bad)
            self.assertIn("paper_sending_enabled", str(caught.exception))
        for mode in ("dry-run",):
            with self.subTest(mode=mode), self.assertRaises(ConfigError) as caught:
                config(mode=mode, paper_sending_enabled=True)
            self.assertIn("paper", str(caught.exception))
        with self.assertRaises(ConfigError):
            config(mode="live", account_id="U0000001", paper_sending_enabled=True)
        # The switch survives a round trip through the mapping, as a test
        # or the command line rebuilds a configuration.
        values = config(mode="paper", paper_sending_enabled=True)._asdict()
        self.assertIs(AdapterConfig.from_mapping(values).paper_sending_enabled, True)

    def test_timing_values_must_be_finite(self):
        # NaN and infinity both pass a "> 0" test, and either would switch the
        # heartbeat deadline or the connect timeout off.
        for name in ("connect_timeout", "request_timeout", "heartbeat_interval",
                     "heartbeat_timeout", "restore_timeout", "backoff_initial", "backoff_max",
                     "commission_wait"):
            for bad in (float("nan"), float("inf"), 0, -1):
                with self.subTest(name=name, value=bad), self.assertRaises(ConfigError):
                    config(**{name: bad})


if __name__ == "__main__":
    unittest.main()
