"""`python3 -m adapter.ibkr run`: the adapter's start command, read strictly before anything connects."""
import io
import json
import re
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
from pathlib import Path

tests_dir = Path(__file__).resolve().parent
adapter_dir = tests_dir.parent
repo = adapter_dir.parents[1]
for p in (str(tests_dir), str(adapter_dir)):
    if p not in sys.path:
        sys.path.insert(0, p)

from cli import _parser, main
from connection import REQUEST_ID_BASE
from config import AdapterConfig
from engine_double import LoggedEngine, run_approval
from fake_tws import ACCOUNT, FakeTWS
import ibapi_pin
from test_ibapi_pin import PINS, SHA, record
from test_loop import SUMMARY

CONFIG = dict(host="127.0.0.1", port=4002, client_id=11, mode="paper", paper_sending_enabled=True,
              connect_timeout=0.05, request_timeout=0.05, heartbeat_interval=30.0, heartbeat_timeout=10.0,
              restore_timeout=60.0, backoff_initial=1.0, backoff_max=8.0, max_connect_attempts=2,
              commission_wait=30.0, mark_timeout=15.0, con_ids={"SPY": 756733})


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
        self.secrets.write_text("IBKR_ACCOUNT_BOOST={}\n".format(ACCOUNT))
        self.versions.write_text("TWSAPI_VERSION=10.51.01\nTWSAPI_SHA256={}\n".format(SHA) + PINS)
        self.record.write_text(record())
        # The record names the installer, the artefact an x86_64 machine installs from.
        machine = mock.patch.object(ibapi_pin.platform, "machine", return_value="x86_64")
        machine.start()
        self.addCleanup(machine.stop)
        # The account's first run, approved with the book the fake shows.
        self.engine = LoggedEngine(str(self.engine_log), approval=run_approval(ACCOUNT, starting_book=({}, 100000.0)))
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
                    should_stop=Steps(steps), err=self.err, poll=0.01, ibapi_version=lambda: (10, 51, 1),
                    protobuf_version=lambda: "5.29.5")

    def test_run_connects_reconciles_and_stops_cleanly(self):
        self.assertEqual(self.run_main(self.argv()), 0, self.err.getvalue())
        self.assertEqual(len(self.made), 1)
        cfg = self.made[0]
        self.assertEqual((cfg.mode, cfg.account_id, cfg.paper_sending_enabled), ("paper", ACCOUNT, True))
        self.assertEqual(self.fake.called("connect"), [("connect", "127.0.0.1", 4002, 11)])
        self.assertIn(("req_executions", REQUEST_ID_BASE + 1, ACCOUNT), self.fake.calls)
        self.assertNotIn(ACCOUNT, self.err.getvalue(), "the account number is never written to the log")

    def test_the_help_names_every_file_and_the_switch_that_enables_sending(self):
        out = subprocess.run([sys.executable, "-m", "adapter.ibkr", "run", "--help"], cwd=str(repo),
                             capture_output=True, text=True, check=True).stdout
        for word in ("--config", "--pending", "--engine-log", "--socket", "--secrets", "--account-key",
                     "--ibapi-record", "--versions", "--streams", "--cash-ledger", "paper_sending_enabled"):
            self.assertIn(word, out)

    def test_an_ibapi_other_than_the_pinned_build_refuses_to_start_before_connecting(self):
        self.record.write_text(record(version="10.50.02"))
        self.assertEqual(self.run_main(self.argv()), 1)
        self.assertIn("provision.sh twsapi", self.err.getvalue())
        self.assertEqual(self.made, [])
        self.record.unlink()
        self.assertEqual(self.run_main(self.argv()), 1)
        self.assertEqual(self.made, [])

    def test_missing_new_york_refuses_even_when_legacy_eastern_resolves(self):
        from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

        asked = []

        def no_new_york(name):
            asked.append(name)
            if name == "America/New_York":
                raise ZoneInfoNotFoundError(name)
            return ZoneInfo("UTC")

        status = main(self.argv(), api_factory=self.api, engine_connect=lambda path, timeout: self.engine,
                      should_stop=Steps(3), err=self.err, poll=0.01, ibapi_version=lambda: (10, 51, 1),
                      protobuf_version=lambda: "5.29.5", zone_lookup=no_new_york)
        self.assertEqual(status, 1)
        self.assertEqual(asked, ["America/New_York"])
        message = self.err.getvalue()
        for word in ("America/New_York", "execution", "connection", "tzdata", "provision.sh base"):
            self.assertIn(word, message)
        self.assertNotIn("tzdata-legacy", message)
        self.assertEqual(self.made, [], "nothing connects when New York's zone is missing")

    def test_new_york_resolves_without_legacy_eastern_and_startup_succeeds(self):
        from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

        asked = []

        def no_legacy_eastern(name):
            asked.append(name)
            if name == "US/Eastern":
                raise ZoneInfoNotFoundError(name)
            return ZoneInfo(name)

        status = main(self.argv(), api_factory=self.api, engine_connect=lambda path, timeout: self.engine,
                      should_stop=Steps(3), err=self.err, poll=0.01, ibapi_version=lambda: (10, 51, 1),
                      protobuf_version=lambda: "5.29.5", zone_lookup=no_legacy_eastern)
        self.assertEqual(status, 0, self.err.getvalue())
        self.assertEqual(asked, ["America/New_York"])
        self.assertEqual(len(self.made), 1)

    def test_an_unreadable_ibkr_time_zone_file_is_reported_as_corrupt_not_as_not_installed(self):
        def corrupt_zone(name):
            raise ValueError("ZoneInfo keys must be normalized relative paths")

        status = main(self.argv(), api_factory=self.api, engine_connect=lambda path, timeout: self.engine,
                      should_stop=Steps(3), err=self.err, poll=0.01, ibapi_version=lambda: (10, 51, 1),
                      zone_lookup=corrupt_zone)
        self.assertEqual(status, 1)
        message = self.err.getvalue()
        self.assertIn("America/New_York", message)
        self.assertIn("corrupt", message)
        self.assertNotIn("not installed", message)
        self.assertNotIn("cannot be read", message)
        self.assertIn("cannot be parsed", message)
        self.assertIn("reinstall", message)
        self.assertIn("restore the zone database", message)
        self.assertEqual(self.made, [], "nothing connects when a zone IBKR names cannot be read")

    def test_an_unreadable_ibkr_time_zone_file_is_refused_not_a_traceback(self):
        for error in (PermissionError(13, "Permission denied"), OSError(5, "Input/output error")):
            with self.subTest(error=type(error).__name__):
                def unreadable_zone(name, error=error):
                    raise error

                self.err = io.StringIO()
                status = main(self.argv(), api_factory=self.api, engine_connect=lambda path, timeout: self.engine,
                              should_stop=Steps(3), err=self.err, poll=0.01, ibapi_version=lambda: (10, 51, 1),
                              zone_lookup=unreadable_zone)
                self.assertEqual(status, 1)
                message = self.err.getvalue()
                self.assertIn("America/New_York", message)
                self.assertIn(error.strerror, message)
                self.assertIn("cannot be read", message)
                self.assertEqual(self.made, [], "nothing connects when a zone IBKR names cannot be read")

    def test_a_directory_where_an_ibkr_zone_file_should_be_is_refused_as_not_installed(self):
        # zoneinfo skips anything that is not a regular file, so a directory at
        # the zone's path reads as a missing zone, not as an unreadable one.
        import zoneinfo as zoneinfo_module
        from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

        saved = zoneinfo_module.TZPATH
        self.addCleanup(zoneinfo_module.reset_tzpath, to=saved)
        zone_root = self.dir / "zoneinfo"
        (zone_root / "America" / "New_York").mkdir(parents=True)
        zoneinfo_module.reset_tzpath(to=[str(zone_root)])
        # With no tzdata pip package installed, a host has no fallback for the zone; a
        # machine that has one would resolve the name from it, so the fallback is switched off here.
        no_fallback = mock.patch("zoneinfo._common.load_tzdata",
                                 side_effect=ZoneInfoNotFoundError("America/New_York"))
        no_fallback.start()
        self.addCleanup(no_fallback.stop)

        status = main(self.argv(), api_factory=self.api, engine_connect=lambda path, timeout: self.engine,
                      should_stop=Steps(3), err=self.err, poll=0.01, ibapi_version=lambda: (10, 51, 1),
                      protobuf_version=lambda: "5.29.5", zone_lookup=ZoneInfo.no_cache)
        self.assertEqual(status, 1)
        message = self.err.getvalue()
        for word in ("America/New_York", "is not installed", "tzdata", "provision.sh base"):
            self.assertIn(word, message)
        self.assertNotIn("cannot be read", message)
        self.assertEqual(self.made, [], "nothing connects when a zone IBKR names is a directory")

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

    def test_the_account_line_is_read_bare_and_one_with_quotes_or_inner_spaces_is_refused(self):
        # Surrounding whitespace (a trailing space, a CRLF line end) is not
        # part of the account; quotes or a space inside it are refused, by
        # the shape expected, before anything connects.
        self.secrets.write_text("IBKR_ACCOUNT_BOOST= {} \r\n".format(ACCOUNT))
        self.assertEqual(self.run_main(self.argv()), 0, self.err.getvalue())
        self.assertEqual(self.made[-1].account_id, ACCOUNT)
        for line in ('"{}"'.format(ACCOUNT), "'{}'".format(ACCOUNT), "DU00 00001", "{}\t#boost".format(ACCOUNT)):
            with self.subTest(line=line):
                self.made = []
                self.err = io.StringIO()
                self.secrets.write_text("IBKR_ACCOUNT_BOOST={}\n".format(line))
                self.assertEqual(self.run_main(self.argv()), 1)
                self.assertIn("KEY=VALUE", self.err.getvalue())
                self.assertNotIn(ACCOUNT, self.err.getvalue())
                self.assertNotIn("DU00 00001", self.err.getvalue())
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

    def test_the_runs_stream_folder_is_delivered_from(self):
        from test_delivery import DAY, FIRST, session_envelopes, write_stream
        from engine_double import CONFIGURATION_HASH, STRATEGY_VERSION
        streams = self.dir / "streams"
        envelopes = session_envelopes([__import__("datetime").date(2027, 1, 14)], run="run-1",
                                      configuration_hash=CONFIGURATION_HASH, strategy_version=STRATEGY_VERSION)
        write_stream(streams / "run-1", __import__("datetime").date(2027, 1, 14), envelopes)
        self.assertEqual(self.run_main(self.argv("--streams", str(streams)), steps=10), 0, self.err.getvalue())
        self.assertEqual([e["type"] for e in self.engine.inputs()], FIRST + DAY)

    def test_a_run_with_no_stream_folder_stops_and_names_the_warm_up(self):
        streams = self.dir / "streams"
        streams.mkdir()
        self.assertEqual(self.run_main(self.argv("--streams", str(streams))), 1)
        self.assertIn(str(streams / "run-1"), self.err.getvalue())
        self.assertIn("StreamRefused", self.err.getvalue())

    def test_another_instances_flex_ledger_is_refused_before_connecting(self):
        # The ledger's records carry no account: each instance's is in its
        # own folder, named as the engine's state folder is.
        other = self.dir / "roth-ira"
        other.mkdir()
        self.assertEqual(self.run_main(self.argv("--cash-ledger", str(other / "cash-movements.jsonl"))), 1)
        self.assertIn("roth-ira", self.err.getvalue())
        self.assertEqual(self.fake.called("connect"), [])

    def test_the_instances_own_flex_ledger_is_read(self):
        own = self.dir / "flex" / self.engine_log.parent.name
        own.mkdir(parents=True)
        self.assertEqual(self.run_main(self.argv("--cash-ledger", str(own / "cash-movements.jsonl"))), 0,
                         self.err.getvalue())

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
        self.assertEqual(args.streams, "/var/lib/trend-investing/data/streams/boost")
        self.assertEqual(args.cash_ledger, "/var/lib/trend-investing/flex/boost/cash-movements.jsonl")
        self.assertIn("-/var/lib/trend-investing/flex/boost", unit)
        self.assertIn("LoadCredential=account:/run/trend-investing/secrets/accounts/boost.env", unit)
        self.assertEqual(args.secrets, "/run/credentials/ti-adapter-boost.service/account")
        self.assertIn("Restart=no", unit)
        self.assertNotRegex(unit, r"\bD?U[0-9]{5,}", "no account number in the unit")

    def test_the_data_job_writes_the_stream_folder_the_adapter_reads(self):
        # The data job writes STREAMS/boost/<TI_RUN>/ (adapter/marketdata/
        # tidata.py); the adapter reads <--streams>/<run>, the run named by
        # its --engine-log, from the same run.env.
        data = (repo / "deploy" / "systemd" / "ti-data.service").read_text()
        adapter = (repo / "deploy" / "systemd" / "ti-adapter-boost.service").read_text()
        data_dir = re.search(r"^Environment=TI_DATA_DIR=(\S+)$", data, re.M).group(1)
        written = re.search(r"--streams (\S+)", data).group(1).replace("${TI_DATA_DIR}", data_dir)
        read = re.search(r"--streams (\S+)", adapter).group(1)
        self.assertEqual(read, written + "/boost")
        for unit in (data, adapter):
            self.assertIn("EnvironmentFile=", unit)
            self.assertIn("/var/lib/trend-investing/engines/boost/run.env", unit)
            self.assertIn("--engine-log /var/lib/trend-investing/engines/boost/${TI_RUN}.log", unit)
        self.assertIn("ReadOnlyPaths=/var/lib/trend-investing/engines/boost -" + read, adapter)

    def test_the_adapter_is_installed_with_the_calendar_the_input_table_and_the_log_reader_it_reads(self):
        provision = (repo / "deploy" / "provision.sh").read_text()
        body = provision[provision.index("step_adapter() {"):]
        body = body[:body.index("\n}\n")]
        for name in ("nyse_calendar.py", "kinds.py", "engine_log.py"):
            self.assertIn('"$here/../adapter/marketdata/{}" "$adapter_root.new/adapter/marketdata/"'.format(name), body)

    def test_the_adapter_is_handed_its_own_account_file_and_nothing_else(self):
        # One engine per account (ADR 0026 point 8): the Boost adapter never
        # sees another account's number or the alert URLs.
        unit = (repo / "deploy" / "systemd" / "ti-adapter-boost.service").read_text()
        loads = [line for line in unit.splitlines() if line.startswith("LoadCredential=")]
        # Loaded from the copy ti-unlock decrypts into a root-only tmpfs after
        # each boot; until then systemd refuses to start the adapter, naming
        # the file (ADR 0028: the account file rests on disk
        # only encrypted).
        self.assertEqual(loads, ["LoadCredential=account:/run/trend-investing/secrets/accounts/boost.env"])
        self.assertRegex(unit, r"(?m)^AssertPathExists=/run/trend-investing/secrets/accounts/boost\.env$")
        self.assertNotIn("secrets.env", "\n".join(l for l in unit.splitlines() if not l.startswith("#")))
        example = (repo / "deploy" / "etc" / "accounts" / "boost.env.example").read_text()
        keys = [l.split("=", 1)[0] for l in example.splitlines() if l and not l.startswith("#")]
        self.assertEqual(keys, ["IBKR_ACCOUNT_BOOST"])
        self.assertNotRegex(example, r"\bD?U[0-9]{5,}")
        secrets = (repo / "deploy" / "etc" / "secrets.env.example").read_text()
        self.assertNotIn("IBKR_ACCOUNT_BOOST=", secrets)
        provision = (repo / "deploy" / "provision.sh").read_text()
        self.assertIn('write_stub "$etc/accounts/boost.env" "$here/etc/accounts/boost.env.example"', provision)

    def test_the_tws_api_installer_runs_unprivileged_and_only_its_python_client_is_installed_as_root(self):
        provision = (repo / "deploy" / "provision.sh").read_text()
        body = provision[provision.index("step_twsapi() {"):]
        body = body[:body.index("\n}\n")]
        runs = [l.strip() for l in body.splitlines()
                if re.search(r'\bsh "[^"]*installer[^"]*" -q', l) and not l.strip().startswith("#")]
        self.assertEqual(len(runs), 1, runs)
        self.assertTrue(runs[0].startswith("(cd / && runuser -u "), "the vendor installer never runs as root: " + runs[0])
        self.assertRegex(body, r"\binstall -d .*-o root")

    def test_the_shipped_adapter_configuration_is_valid_and_sends_nothing(self):
        values = json.loads((repo / "deploy" / "etc" / "adapters" / "boost.json.example").read_text())
        self.assertNotIn("account_id", values, "the account comes from secrets.env")
        cfg = AdapterConfig.from_mapping(dict(values, account_id=ACCOUNT))
        self.assertEqual((cfg.mode, cfg.paper_sending_enabled), ("dry-run", False))
        self.assertEqual(cfg.client_id, 11, "Boost's client ID (deploy/etc/engines/boost.env)")


if __name__ == "__main__":
    unittest.main()
