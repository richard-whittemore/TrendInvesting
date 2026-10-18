"""`python3 -m adapter.ibkr run`: the adapter's start command, read strictly before anything connects."""
import io
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

tests_dir = Path(__file__).resolve().parent
adapter_dir = tests_dir.parent
repo = adapter_dir.parents[1]
for p in (str(tests_dir), str(adapter_dir)):
    if p not in sys.path:
        sys.path.insert(0, p)

from cli import _parser, main
from config import AdapterConfig
from engine_double import LoggedEngine
from fake_tws import ACCOUNT, FakeTWS
from test_ibapi_pin import SHA
from test_loop import SUMMARY

CONFIG = dict(host="127.0.0.1", port=4002, client_id=11, mode="paper", paper_sending_enabled=True,
              connect_timeout=0.05, request_timeout=0.05, heartbeat_interval=30.0, heartbeat_timeout=10.0,
              restore_timeout=60.0, backoff_initial=1.0, backoff_max=8.0, max_connect_attempts=2,
              commission_wait=30.0, con_ids={"SPY": 756733})


class Steps:
    """should_stop for the loop: true after n steps."""

    def __init__(self, n):
        self.n = n

    def __call__(self):
        self.n -= 1
        return self.n < 0


class CommandTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        self.config = self.dir / "boost.json"
        self.secrets = self.dir / "secrets.env"
        self.record = self.dir / "INSTALLED"
        self.versions = self.dir / "versions.env"
        self.pending = self.dir / "pending.json"
        self.engine_log = self.dir / "run-1.log"
        self.write_config(dict(CONFIG))
        self.secrets.write_text("IBKR_ACCOUNT_TRAD_IRA=\nIBKR_ACCOUNT_BOOST={}\nNTFY_URL=\n".format(ACCOUNT))
        self.versions.write_text("TWSAPI_VERSION=10.51.01\nTWSAPI_SHA256={}\n".format(SHA))
        self.record.write_text("TWSAPI_VERSION=10.51.01\nTWSAPI_SHA256={}\n".format(SHA))
        self.engine = LoggedEngine(str(self.engine_log))
        self.fake = FakeTWS()
        self.fake.summary = list(SUMMARY)
        self.made = []
        self.err = io.StringIO()

    def write_config(self, values):
        self.config.write_text(json.dumps(values))

    def argv(self, *extra, account=True):
        args = ["run", "--config", str(self.config), "--pending", str(self.pending), "--engine-log",
                str(self.engine_log), "--socket", str(self.dir / "engine.sock"), "--ibapi-record", str(self.record),
                "--versions", str(self.versions)]
        if account:
            args += ["--secrets", str(self.secrets), "--account-key", "IBKR_ACCOUNT_BOOST"]
        return args + list(extra)

    def api(self, cfg):
        self.made.append(cfg)
        return self.fake

    def run_main(self, argv, steps=3):
        return main(argv, api_factory=self.api, engine_connect=lambda path, timeout: self.engine,
                    should_stop=Steps(steps), err=self.err, poll=0.01)

    def test_run_connects_reconciles_and_stops_cleanly(self):
        self.assertEqual(self.run_main(self.argv()), 0, self.err.getvalue())
        self.assertEqual(len(self.made), 1)
        cfg = self.made[0]
        self.assertEqual((cfg.mode, cfg.account_id, cfg.paper_sending_enabled), ("paper", ACCOUNT, True))
        self.assertEqual(self.fake.called("connect"), [("connect", "127.0.0.1", 4002, 11)])
        self.assertIn(("req_executions", 2, ACCOUNT), self.fake.calls)
        self.assertNotIn(ACCOUNT, self.err.getvalue(), "the account number is never written to the log")

    def test_the_help_names_every_file_and_the_switch_that_enables_sending(self):
        out = subprocess.run([sys.executable, "-m", "adapter.ibkr", "run", "--help"], cwd=str(repo),
                             capture_output=True, text=True, check=True).stdout
        for word in ("--config", "--pending", "--engine-log", "--socket", "--secrets", "--account-key",
                     "--ibapi-record", "--versions", "paper_sending_enabled"):
            self.assertIn(word, out)

    def test_an_ibapi_other_than_the_pinned_build_refuses_to_start_before_connecting(self):
        self.record.write_text("TWSAPI_VERSION=10.50.02\nTWSAPI_SHA256={}\n".format(SHA))
        self.assertEqual(self.run_main(self.argv()), 1)
        self.assertIn("provision.sh twsapi", self.err.getvalue())
        self.assertEqual(self.made, [])
        self.record.unlink()
        self.assertEqual(self.run_main(self.argv()), 1)
        self.assertEqual(self.made, [])

    def test_the_account_comes_from_one_place_only_and_is_never_echoed(self):
        both = dict(CONFIG, account_id=ACCOUNT)
        self.write_config(both)
        self.assertEqual(self.run_main(self.argv()), 1)
        self.assertIn("one place", self.err.getvalue())
        self.err = io.StringIO()
        self.write_config(dict(CONFIG))
        self.assertEqual(self.run_main(self.argv(account=False)), 1)
        self.assertIn("account", self.err.getvalue())
        self.err = io.StringIO()
        self.secrets.write_text("IBKR_ACCOUNT_BOOST=\n")
        self.assertEqual(self.run_main(self.argv()), 1)
        self.assertIn("IBKR_ACCOUNT_BOOST", self.err.getvalue())
        self.err = io.StringIO()
        self.secrets.write_text("IBKR_ACCOUNT_BOOST=U9999999\n")
        self.assertEqual(self.run_main(self.argv()), 1)
        self.assertNotIn("U9999999", self.err.getvalue())
        self.assertEqual(self.made, [])

    def test_an_account_in_the_configuration_file_needs_no_secrets_file(self):
        self.write_config(dict(CONFIG, account_id=ACCOUNT))
        self.assertEqual(self.run_main(self.argv(account=False)), 0, self.err.getvalue())

    def test_a_configuration_that_is_not_a_json_object_or_not_valid_is_refused(self):
        for text in ("not json", "[]", json.dumps(dict(CONFIG, paper_sending_enabled="yes")),
                     json.dumps(dict(CONFIG, mode="live"))):
            with self.subTest(text=text):
                self.config.write_text(text)
                self.err = io.StringIO()
                self.assertEqual(self.run_main(self.argv()), 1)
                self.assertIn("refused", self.err.getvalue())
        self.assertEqual(self.made, [])

    def test_a_stop_for_a_person_exits_one_and_says_why(self):
        self.fake.summary = SUMMARY[:1]
        self.assertEqual(self.run_main(self.argv()), 1)
        self.assertIn("stopped", self.err.getvalue())
        self.assertIn("total-cash", self.err.getvalue())

    def test_a_stop_message_never_shows_the_account_number(self):
        self.fake.managed_accounts = ("DU0000002",)
        self.assertEqual(self.run_main(self.argv()), 1)
        self.assertIn("<account>", self.err.getvalue())
        self.assertNotIn(ACCOUNT, self.err.getvalue())
        self.assertNotIn("DU0000002", self.err.getvalue(), "nor the login's other accounts")

    def test_an_engine_log_that_cannot_be_read_stops_before_connecting(self):
        self.engine_log.unlink()
        self.assertEqual(self.run_main(self.argv()), 1)
        self.assertIn("durable log", self.err.getvalue())
        self.assertEqual(self.fake.called("connect"), [])


class DeployTests(unittest.TestCase):
    """The server's unit and the shipped configuration are ones the command takes."""

    def test_the_units_start_command_is_this_command_with_every_file_named(self):
        unit = (repo / "deploy" / "systemd" / "ti-adapter-boost.service").read_text()
        exec_start = [line for line in unit.splitlines() if line.startswith("ExecStart=")]
        self.assertEqual(len(exec_start), 1)
        words = exec_start[0][len("ExecStart="):].split()
        self.assertEqual(words[:3], ["/opt/trend-investing/adapter-venv/bin/python3", "-m", "adapter.ibkr"])
        args = _parser().parse_args([w.replace("%d", "/run/credentials/ti-adapter-boost.service")
                                     .replace("${TI_RUN}", "run-1") for w in words[3:]])
        self.assertEqual(args.command, "run")
        self.assertEqual(args.account_key, "IBKR_ACCOUNT_BOOST")
        self.assertEqual(args.engine_log, "/var/lib/trend-investing/engines/boost/run-1.log")
        self.assertEqual(args.socket, "/run/ti-engine-boost/engine.sock")
        self.assertEqual(args.pending, "/var/lib/trend-investing/adapters/boost/pending.json")
        self.assertIn("LoadCredential=secrets:/etc/trend-investing/secrets.env", unit)
        self.assertIn("Restart=no", unit)
        self.assertNotRegex(unit, r"\bD?U[0-9]{5,}", "no account number in the unit")

    def test_the_shipped_adapter_configuration_is_valid_and_sends_nothing(self):
        values = json.loads((repo / "deploy" / "etc" / "adapters" / "boost.json.example").read_text())
        self.assertNotIn("account_id", values, "the account comes from secrets.env")
        cfg = AdapterConfig.from_mapping(dict(values, account_id=ACCOUNT))
        self.assertEqual((cfg.mode, cfg.paper_sending_enabled), ("dry-run", False))
        self.assertEqual(cfg.client_id, 11, "Boost's client ID (deploy/etc/engines/boost.env)")


if __name__ == "__main__":
    unittest.main()
