"""Synthetic deployment checks: ADR 0026 account isolation and docs/live-trading/lessons-2026-10.md L3/L8."""
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest
from types import SimpleNamespace

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "adapter/ibkr"))
from cli import Refused, load_config


class IRALoginSecretsTests(unittest.TestCase):
    """Stored IRA logins remain unused by Gateways (docs/manual/server.md)."""

    names = ("ibkr-login-trad-ira.env", "ibkr-login-roth-ira.env")

    def secrets(self, script, folder, *args):
        # Load the real functions without the root-only command dispatcher.
        source = (ROOT / "deploy/secrets.sh").read_text().split("\nneed_root\n")[0]
        return subprocess.run(["bash", "-c", source + '\netc=$1\nshift\n' + script,
                               "secrets-test", str(folder), *args],
                              capture_output=True, text=True)

    def test_only_declared_login_names_are_accepted(self):
        for name in ("ibkr-login-joint.env", *self.names):
            with self.subTest(name=name):
                result = self.secrets('valid_name "$1"', ROOT, name)
                self.assertEqual(result.returncode, 0, f"valid_name refused {name}")
        for name in ("ibkr-login-boost.env", "ibkr-login-x.env", "ibkr-login-trad-ira.env.gpg"):
            with self.subTest(name=name):
                result = self.secrets('valid_name "$1"', ROOT, name)
                self.assertNotEqual(result.returncode, 0, f"valid_name accepted {name}")

    def test_plain_and_encrypted_discovery_include_only_declared_logins(self):
        with tempfile.TemporaryDirectory() as folder:
            for name in ("ibkr-login-joint.env", *self.names, "ibkr-login-x.env"):
                for suffix in ("", ".gpg"):
                    (Path(folder) / (name + suffix)).touch()
            for function in ("plain_names", "encrypted_names"):
                with self.subTest(function=function):
                    result = self.secrets(function, folder)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(set(result.stdout.splitlines()), {"ibkr-login-joint.env", *self.names})

    def test_provisioning_never_creates_optional_logins(self):
        provision = (ROOT / "deploy/provision.sh").read_text()
        for name in self.names:
            self.assertFalse(f'write_stub "$etc/{name}"' in provision, f"{name} must not be provisioned")
            self.assertFalse((ROOT / "deploy/etc" / (name + ".example")).exists())

    def test_no_service_consumes_ira_logins_and_boost_keeps_its_login(self):
        for unit in (ROOT / "deploy/systemd").rglob("*"):
            if unit.is_file():
                for name in self.names:
                    self.assertNotIn(name, unit.read_text(), str(unit))
        gateway = (ROOT / "deploy/systemd/ti-gateway.service").read_text()
        self.assertIn("ExecStartPre=+/usr/local/libexec/trend-investing/stage-gateway-login.sh", gateway)
        provision = (ROOT / "deploy/provision.sh").read_text()
        self.assertIn('install -m 0644 /dev/stdin "$etc/gateway-login" <<\'CONF\'\njoint\nCONF', provision)


class IRAFirstEditTests(unittest.TestCase):
    """Real GPG and command functions with synthetic secrets, no root or network.

    Only OS boundaries are replaced: mounts, ownership, sync and systemd. Bash 3 on
    macOS also needs mapfile and the dynamic close-descriptor spelling shim.
    The root/container harness checks those Linux boundaries separately.
    """

    names = IRALoginSecretsTests.names
    phrase = "synthetic existing passphrase"
    new_phrase = "synthetic replacement passphrase"

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.folder = Path(self.temp.name)
        self.run = self.folder / "run"
        self.run.mkdir()
        self.editor = self.folder / "editor"
        self.editor.write_text('''#!/bin/sh
cp "$1" "$TEST_ROOT/editor-before"
case "$TEST_EDIT" in
  change) printf 'TWS_USERID=synthetic-user\\n' >>"$1" ;;
  second) printf 'TWS_PASSWORD=synthetic-password\\n' >>"$1" ;;
  unchanged) : ;;
  fail) exit 1 ;;
esac
''')
        self.editor.chmod(0o700)
        self.seed("secrets.env")

    def command(self, script, *args, phrases=(), edit="change"):
        source = (ROOT / "deploy/secrets.sh").read_text().split("\nneed_root\n")[0]
        source = source.replace("{lockfd}>&-", "9>&-")
        shim = '''
etc=$1
run=$etc/run
journal=$etc/.passphrase-change
shift
passfd=0
exec 9>/dev/null
ensure_tmpfs() { mkdir -p "$run"; }
mountpoint() { return 1; }
systemctl() { return 1; }
chown() { :; }
sync() { :; }
install() {
  local args=()
  while test "$#" -gt 0; do
    case $1 in
      -o | -g) shift 2 ;;
      *) args+=("$1"); shift ;;
    esac
  done
  command install "${args[@]}"
}
if ! type mapfile >/dev/null 2>&1; then
  mapfile() {
    local line
    eval "$2=()"
    while IFS= read -r line; do
      eval "$2+=(\\"\\$line\\")"
    done
  }
fi
'''
        env = dict(os.environ, TEST_ROOT=str(self.folder), TEST_EDIT=edit,
                   SUDO_EDITOR=str(self.editor))
        for key in ("TI_PASSPHRASE_FD", "TI_PROC_SWAPS"):
            env.pop(key, None)
        return subprocess.run(["bash", "-c", source + shim + script,
                               "secrets-test", str(self.folder), *args],
                              input="".join(p + "\n" for p in phrases),
                              capture_output=True, text=True, env=env)

    def ok(self, result):
        self.assertEqual(result.returncode, 0, result.stderr)
        return result

    def seed(self, name):
        self.ok(self.command('''new_work stage
printf 'SYNTHETIC=existing\\n' >"$stage/plain"
read_secret phrase test
encrypt_checked tmp "$phrase" "$stage/plain" "$1"
commit "$tmp" "$1"''', name, phrases=(self.phrase,)))

    def decrypt(self, name, phrase=None):
        return self.command('''new_work stage
read_secret phrase test
decrypt_to "$phrase" "$etc/$1.gpg" "$stage/plain"
cat "$stage/plain"''', name, phrases=(phrase or self.phrase,))

    def snapshot(self):
        return {str(p.relative_to(self.folder)): p.read_bytes()
                for p in self.folder.rglob("*") if p.is_file()}

    def test_first_edit_creates_then_second_edit_decrypts_existing_content(self):
        for name in self.names:
            with self.subTest(name=name):
                self.ok(self.command('cmd_edit "$1"', name, phrases=(self.phrase,)))
                template = (self.folder / "editor-before").read_text()
                lines = template.splitlines()
                self.assertEqual(len([line for line in lines if line.startswith("#")]), 1)
                self.assertIn("no service reads it yet", template.lower())
                self.assertEqual(dict(line.split("=", 1) for line in lines if not line.startswith("#")),
                                 {"TWS_USERID": "", "TWS_PASSWORD": "",
                                  "TWS_TRADING_MODE": "paper", "TWS_READONLY_API": "yes"})
                first = self.ok(self.decrypt(name)).stdout
                self.assertEqual(first, template + "TWS_USERID=synthetic-user\n")
                self.assertFalse((self.folder / name).exists())
                self.assertEqual((self.folder / (name + ".gpg")).stat().st_mode & 0o777, 0o600)
                self.assertEqual(list(self.run.iterdir()), [])
                self.ok(self.command('cmd_edit "$1"', name, phrases=(self.phrase,), edit="second"))
                self.assertEqual((self.folder / "editor-before").read_text(), first)
                self.assertEqual(self.ok(self.decrypt(name)).stdout, first + "TWS_PASSWORD=synthetic-password\n")

    def test_wrong_passphrase_first_edit_changes_nothing_and_runs_no_editor(self):
        for name in self.names:
            with self.subTest(name=name):
                before = self.snapshot()
                result = self.command('cmd_edit "$1"', name, phrases=("mistyped passphrase",))
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("nothing changed", result.stderr)
                self.assertEqual(self.snapshot(), before)

    def test_unchanged_template_writes_nothing_locked_or_unlocked(self):
        for unlocked in (False, True):
            if unlocked:
                (self.run / "secrets.env").write_text("SYNTHETIC=existing\n")
            for name in self.names:
                with self.subTest(name=name, unlocked=unlocked):
                    before = self.snapshot()
                    result = self.ok(self.command('cmd_edit "$1"', name, phrases=(self.phrase,), edit="unchanged"))
                    self.assertIn("unchanged; nothing written", result.stdout)
                    after = self.snapshot()
                    after.pop("editor-before")
                    before.pop("editor-before", None)
                    self.assertEqual(after, before)

    def test_first_edit_places_unlocked_copy_and_rotation_includes_optional_files(self):
        self.ok(self.command('cmd_unlock', phrases=(self.phrase,)))
        for name in self.names:
            self.ok(self.command('cmd_edit "$1"', name, phrases=(self.phrase,)))
            self.assertEqual((self.run / name).read_text(), self.ok(self.decrypt(name)).stdout)
        before = {name: (self.folder / (name + ".gpg")).read_bytes() for name in self.names}
        self.ok(self.command('cmd_passphrase 0', phrases=(self.phrase, self.new_phrase, self.new_phrase)))
        for name in self.names:
            self.assertNotEqual((self.folder / (name + ".gpg")).read_bytes(), before[name])
            self.assertNotEqual(self.decrypt(name).returncode, 0)
            self.assertEqual(self.ok(self.decrypt(name, self.new_phrase)).stdout, (self.run / name).read_text())
        self.ok(self.command('cmd_lock'))
        self.assertEqual(list(self.run.iterdir()), [])
        self.ok(self.command('cmd_unlock', phrases=(self.new_phrase,)))
        for name in self.names:
            self.assertEqual((self.run / name).read_text(), self.ok(self.decrypt(name, self.new_phrase)).stdout)

    def test_absent_optional_files_are_clean_for_status_unlock_lock_and_rotation(self):
        self.seed("ibkr-login.env")
        self.ok(self.command("cmd_migrate"))
        result = self.ok(self.command('cmd_status'))
        self.assertNotIn("missing", result.stdout)
        for name in self.names:
            self.assertNotIn(name, result.stdout)
        self.ok(self.command('cmd_unlock', phrases=(self.phrase,)))
        self.ok(self.command('cmd_lock'))
        self.ok(self.command('cmd_passphrase 0', phrases=(self.phrase, self.new_phrase, self.new_phrase)))
        for name in self.names:
            self.assertFalse((self.folder / (name + ".gpg")).exists())

    def test_verification_uses_secrets_then_login_and_refuses_without_either(self):
        self.seed("ibkr-login.env")
        self.ok(self.command("cmd_migrate"))
        secrets = self.folder / "secrets.env.gpg"
        secrets.write_bytes(b"damaged encrypted secret")
        before = self.snapshot()
        result = self.command('cmd_edit "$1"', self.names[0], phrases=(self.phrase,))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("nothing changed", result.stderr)
        self.assertEqual(self.snapshot(), before)
        secrets.unlink()
        self.ok(self.command('cmd_edit "$1"', self.names[0], phrases=(self.phrase,)))
        (self.folder / "ibkr-login-joint.env.gpg").unlink()
        before = self.snapshot()
        result = self.command('cmd_edit "$1"', self.names[1], phrases=(self.phrase,))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("nothing changed", result.stderr)
        self.assertEqual(self.snapshot(), before)

    def test_plain_optional_and_missing_other_names_remain_refused(self):
        for name in self.names:
            (self.folder / name).write_text("owner's plaintext\n")
        for name in (*self.names, "ibkr-login-joint.env", "accounts/missing.env", "ibkr-login-boost.env"):
            with self.subTest(name=name):
                before = self.snapshot()
                result = self.command('cmd_edit "$1"', name, phrases=(self.phrase,))
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.snapshot(), before)
        (self.folder / "secrets.env.gpg").unlink()
        self.assertNotEqual(self.command('cmd_edit secrets.env', phrases=(self.phrase,)).returncode, 0)

    def test_editor_failure_on_first_edit_creates_nothing(self):
        result = self.command('cmd_edit "$1"', self.names[0], phrases=(self.phrase,), edit="fail")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("the editor exited with an error; nothing changed", result.stderr)
        self.assertFalse((self.folder / (self.names[0] + ".gpg")).exists())
        self.assertEqual(list(self.run.iterdir()), [])


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
