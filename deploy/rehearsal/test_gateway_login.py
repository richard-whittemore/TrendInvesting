"""Login selection and secret lifecycle (docs/runbooks/gateway-login.md, ADR 0028)."""

# CASE TABLE (one test per row; no processes are signalled):
# joint present -> selected config: test_selector_joint
# trad-ira in joint selector -> own unit named: test_selector_trad_ira
# roth-ira in joint selector -> own unit named: test_selector_roth_ira
# live present -> selected config: test_selector_live
# selected file absent -> 64, file and selector named: test_selected_absent
# selected file empty -> 64, file and selector named: test_selected_empty
# selector absent -> refuse: test_selector_absent
# selector garbage -> refuse: test_selector_garbage
# live without read-only key -> refuse: test_live_requires_readonly
# second factor present -> set: test_second_factor_present
# second factor absent -> empty: test_second_factor_absent
# migration old only -> rename encrypted and unlocked: test_migration_old_only
# migration new only -> unchanged: test_migration_new_only
# migration both -> refuse: test_migration_both
# migration neither -> unchanged: test_migration_neither
# edit old name -> refuse, point to joint: test_edit_old_name
# optional trad-ira first edit -> paper template: test_create_trad_ira
# optional roth-ira first edit -> paper template: test_create_roth_ira
# optional live first edit -> live/IB Key template: test_create_live
# wrong passphrase -> no file/editor: test_create_wrong_passphrase
# no existing encrypted secret -> refuse: test_create_without_existing_secret
# editor failure -> no new file: test_create_editor_failure
# optional files -> enumerated: test_optional_enumeration
# selector command no argument -> current: test_command_show
# selector command valid -> atomic replacement, restart notice: test_command_set
# selector command invalid -> unchanged: test_command_invalid
# selector command invalid stored value -> refuse: test_command_show_invalid
# selector path is directory -> refuse: test_command_directory
# provisioning -> selector created once: test_provision_selector_once
# selected link -> refuse: test_selected_link
# staged file -> mode 0600, Gateway stand-in owner: test_selector_joint
# credential handover -> selected file only, secrets inaccessible: test_unit_credentials

import os
import pwd
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock
import stat
import sys

import test_roth


DEPLOY = Path(__file__).resolve().parents[1]


class GatewayLoginTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.etc = self.root / "etc"
        self.run = self.root / "run"
        self.cred = self.root / "cred"
        for directory in (self.etc, self.run, self.cred):
            directory.mkdir()
        self.selector = self.etc / "gateway-login"
        self.selector.write_text("joint\n")
        self.template = self.root / "template.ini"
        self.template.write_text("\n".join(key + "=template-value" for key in (
            "IbLoginId", "IbPassword", "TradingMode", "ReadOnlyApi",
            "SecondFactorDevice", "AutoRestartTime", "ColdRestartTime",
            "ReloginAfterSecondFactorAuthenticationTimeout",
            "AcceptNonBrokerageAccountWarning", "LoginDialogDisplayTimeout",
            "CommandServerPort", "AcceptIncomingConnectionAction",
            "TrustedTwsApiClientIPs")) + "\n")

    def shell(self, script, *args):
        return subprocess.run(["bash", "-c", script, "test", *args], text=True,
                              capture_output=True, env={**os.environ, "TMPDIR": str(self.root)})

    def stage(self):
        path = DEPLOY / "libexec/stage-gateway-login.sh"
        self.assertTrue(path.exists(), "stage-gateway-login.sh missing")
        # The local user stands in for the service identity on an unprivileged host.
        script = path.read_text().replace('pwd.getpwnam("ti-gateway")',
                                         f'pwd.getpwnam({pwd.getpwuid(os.getuid()).pw_name!r})')
        return subprocess.run(["bash", "-c", script], text=True, capture_output=True,
                              env={**os.environ,
                                   "TI_GATEWAY_LOGIN_FILE": str(self.selector),
                                   "TI_GATEWAY_SECRETS_DIRECTORY": str(self.cred),
                                   "RUNTIME_DIRECTORY": str(self.run)})

    def render(self, stage=True):
        if stage:
            result = self.stage()
            if result.returncode:
                return result
        return subprocess.run(["bash", str(DEPLOY / "libexec/render-ibc-config.sh")],
                              text=True, capture_output=True, env={**os.environ,
                              "TI_GATEWAY_LOGIN_FILE": str(self.selector),
                              "CREDENTIALS_DIRECTORY": str(self.cred),
                              "RUNTIME_DIRECTORY": str(self.run),
                              "IBC_TEMPLATE": str(self.template)})

    def login(self, selection="joint", readonly=True, factor=None):
        content = (f"TWS_USERID=fixture-{selection}\nTWS_PASSWORD=synthetic\n"
                   f"TWS_TRADING_MODE={'live' if selection == 'live' else 'paper'}\n")
        if readonly:
            content += "TWS_READONLY_API=yes\n"
        if factor is not None:
            content += f"TWS_SECOND_FACTOR_DEVICE={factor}\n"
        path = self.cred / f"ibkr-login-{selection}.env"
        path.write_text(content)
        return path

    def selected(self, selection):
        for name in ("joint", "trad-ira", "roth-ira", "live"):
            self.login(name)
        (self.cred / "secrets.env").write_text("MASSIVE_API_KEY=unrelated")
        (self.cred / "accounts").mkdir()
        (self.cred / "accounts/joint.env").write_text("IBKR_ACCOUNT=unrelated")
        self.selector.write_text(selection + "\n")
        result = self.stage()
        self.assertEqual(result.returncode, 0, result.stderr)
        staged = self.run / "login.env"
        self.assertEqual(staged.read_bytes(), (self.cred / f"ibkr-login-{selection}.env").read_bytes())
        self.assertEqual(staged.stat().st_mode & 0o777, 0o600)
        self.assertEqual(staged.stat().st_uid, os.getuid())
        self.assertEqual(staged.stat().st_gid, pwd.getpwuid(os.getuid()).pw_gid)
        self.assertEqual(list(self.run.iterdir()), [staged])
        # Remove the source tree and selector from the renderer's view.
        self.cred.rename(self.root / "hidden-secrets")
        self.selector.unlink()
        result = self.render(stage=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        config = (self.run / "config.ini").read_text()
        self.assertIn(f"IbLoginId=fixture-{selection}\n", config)
        self.assertIn(f"TradingMode={'live' if selection == 'live' else 'paper'}\n", config)
        self.assertIn("ReadOnlyApi=yes\n", config)
        self.assertEqual((self.run / "config.ini").stat().st_mode & 0o777, 0o600)
        self.assertNotIn("synthetic", result.stdout + result.stderr)

    def test_selector_joint(self):
        self.selected("joint")

    def test_selector_trad_ira(self):
        self.refused_ira_selection("trad-ira")

    def test_selector_roth_ira(self):
        self.refused_ira_selection("roth-ira")

    def refused_ira_selection(self, instance):
        self.login(instance)
        self.selector.write_text(instance + "\n")
        for result in (self.stage(), self.command(), self.command(instance)):
            self.assertEqual(result.returncode, 64, result.stderr)
            self.assertIn(f"ti-gateway@{instance}.service", result.stderr)

    def test_selector_live(self):
        self.selected("live")

    def missing_selected(self):
        result = self.stage()
        self.assertEqual(result.returncode, 64, result.stderr)
        self.assertIn("ibkr-login-joint.env", result.stderr)
        self.assertIn(str(self.selector), result.stderr)

    def test_selected_absent(self):
        self.missing_selected()

    def test_selected_empty(self):
        self.login().write_text("")
        self.missing_selected()

    def test_selected_link(self):
        selected = self.login()
        target = self.cred / "secrets.env"
        target.write_text("unrelated-secret")
        selected.unlink()
        selected.symlink_to(target)
        self.missing_selected()
        self.assertFalse((self.run / "login.env").exists())

    def test_selected_directory(self):
        (self.cred / "ibkr-login-joint.env").mkdir()
        self.missing_selected()

    def test_staged_link_is_replaced(self):
        target = self.root / "untouched"
        target.write_text("original")
        (self.run / "login.env").symlink_to(target)
        self.login()
        result = self.stage()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(target.read_text(), "original")
        self.assertFalse((self.run / "login.env").is_symlink())

    def test_selector_absent(self):
        self.selector.unlink()
        self.login()
        result = self.render()
        self.assertEqual(result.returncode, 64, result.stderr)
        self.assertIn(str(self.selector), result.stderr)

    def test_selector_garbage(self):
        self.login()
        for value in ("", "unknown", "../live", "joint live", "joint\nlive", " joint", "joint\n\n", "joint\x00"):
            with self.subTest(value=value):
                self.selector.write_text(value)
                result = self.render()
                self.assertEqual(result.returncode, 64, result.stderr)
                self.assertIn(str(self.selector), result.stderr)

    def test_live_requires_readonly(self):
        self.selector.write_text("live\n")
        self.login("live", readonly=False)
        result = self.render()
        self.assertEqual(result.returncode, 64, result.stderr)
        self.assertIn("TWS_READONLY_API", result.stderr)

    def test_second_factor_present(self):
        self.login(factor="IB Key")
        result = self.render()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("SecondFactorDevice=IB Key\n", (self.run / "config.ini").read_text())

    def test_second_factor_absent(self):
        self.login()
        result = self.render()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("SecondFactorDevice=\n", (self.run / "config.ini").read_text())

    def secrets(self, command, phrase="verified"):
        # Exercise real lifecycle functions without root, mounts or crypto;
        # inside-secrets.sh covers GnuPG and tmpfs (ADR 0028). The mapfile shim
        # lets these function tests run on macOS's Bash 3 as well as Linux.
        source = (DEPLOY / "secrets.sh").read_text().split("\nneed_root\n")[0]
        # No lock is taken here, so lockfd is empty; Linux Bash then rejects
        # the editor's {lockfd}>&- redirect. Close a fixed descriptor instead,
        # as test_roth.py does; the real script always holds the lock.
        source = source.replace("{lockfd}>&-", "9>&-")
        harness = r'''
etc=$1 run=$2 journal=$1/.passphrase-change fixture_phrase=$3
mapfile() {
  local line array=$2
  eval "$array=()"
  while IFS= read -r line; do eval "$array+=(\"\$line\")"; done
}
sync() { :; }
ensure_tmpfs() { mkdir -p "$run"; }
read_secret() {
  test "$fixture_phrase" != must-not-be-read || die 'unexpected passphrase prompt'
  printf -v "$1" '%s' "$fixture_phrase"
}
decrypt_to() {
  test "$1" = verified || return 1
  cp "$2" "$3"
}
encrypt_checked() {
  local output=$etc/.encrypted
  cp "$3" "$output"
  printf -v "$1" '%s' "$output"
}
place() { cp "$1" "$run/$2"; }
export SUDO_EDITOR=$etc/editor
'''
        return self.shell(source + harness + command, str(self.etc), str(self.run), phrase)

    def test_migration_old_only(self):
        for directory in (self.etc, self.run):
            suffix = ".gpg" if directory == self.etc else ""
            (directory / ("ibkr-login.env" + suffix)).write_text("original")
        result = self.secrets("cmd_migrate", phrase="must-not-be-read")
        self.assertEqual(result.returncode, 0, result.stderr)
        for directory in (self.etc, self.run):
            suffix = ".gpg" if directory == self.etc else ""
            self.assertFalse((directory / ("ibkr-login.env" + suffix)).exists())
            self.assertEqual((directory / ("ibkr-login-joint.env" + suffix)).read_text(), "original")
        self.assertEqual(self.secrets("cmd_migrate").returncode, 0)

    def test_migration_new_only(self):
        path = self.etc / "ibkr-login-joint.env.gpg"
        path.write_text("original")
        result = self.secrets("cmd_migrate", phrase="must-not-be-read")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(path.read_text(), "original")

    def test_migration_both(self):
        for name in ("ibkr-login.env.gpg", "ibkr-login-joint.env.gpg"):
            (self.etc / name).write_text(name)
        (self.run / "ibkr-login.env").write_text("unlocked-original")
        result = self.secrets("cmd_migrate")
        self.assertNotEqual(result.returncode, 0)
        for name in ("ibkr-login.env.gpg", "ibkr-login-joint.env.gpg"):
            self.assertIn(name, result.stderr)
            self.assertEqual((self.etc / name).read_text(), name)
        self.assertEqual((self.run / "ibkr-login.env").read_text(), "unlocked-original")
        self.assertFalse((self.run / "ibkr-login-joint.env").exists())

    def test_migration_neither(self):
        before = sorted(self.etc.iterdir())
        result = self.secrets("cmd_migrate", phrase="must-not-be-read")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(sorted(self.etc.iterdir()), before)

    def test_edit_old_name(self):
        result = self.secrets("cmd_edit ibkr-login.env")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ibkr-login-joint.env", result.stderr)

    def test_diff_existing_old_name_is_read_only(self):
        encrypted = self.etc / "ibkr-login.env.gpg"
        plain = self.etc / "ibkr-login.env"
        encrypted.write_text("synthetic-encrypted\n")
        for contents, status in (("synthetic-encrypted\n", 0), ("synthetic-conflict\n", 1)):
            with self.subTest(status=status):
                plain.write_text(contents)
                # Stand in for terminal stdout while capturing synthetic diff output.
                result = self.secrets('''
test() {
  if [[ $# = 2 && $1 = -t && $2 = 1 ]]; then return 0; fi
  builtin test "$@"
}
cmd_diff ibkr-login.env
''')
                self.assertEqual(result.returncode, status, result.stderr)
                if status == 0:
                    self.assertIn("hold the same text", result.stdout)
                else:
                    self.assertIn("-synthetic-encrypted", result.stdout)
                    self.assertIn("+synthetic-conflict", result.stdout)
                self.assertEqual(encrypted.read_text(), "synthetic-encrypted\n")
                self.assertEqual(plain.read_text(), contents)
                self.assertEqual(list(self.run.iterdir()), [])

    def test_edit_existing_old_name_is_refused(self):
        encrypted = self.etc / "ibkr-login.env.gpg"
        plain = self.etc / "ibkr-login.env"
        encrypted.write_text("synthetic-encrypted\n")
        plain.write_text("synthetic-conflict\n")
        result = self.secrets("cmd_edit ibkr-login.env", phrase="must-not-be-read")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("legacy ibkr-login.env remains", result.stderr)
        self.assertNotIn("unexpected passphrase prompt", result.stderr)
        self.assertEqual(encrypted.read_text(), "synthetic-encrypted\n")
        self.assertEqual(plain.read_text(), "synthetic-conflict\n")
        self.assertEqual(list(self.run.iterdir()), [])

    def editor(self, status=0, change=False):
        path = self.etc / "editor"
        edit = 'printf "TWS_USERID=synthetic-user\\n" >>"$1"\n' if change else ''
        path.write_text(f'#!/bin/sh\ncp "$1" "{self.root}/editor-input"\n{edit}exit {status}\n')
        path.chmod(0o700)

    def create(self, selection):
        (self.etc / "secrets.env.gpg").write_text("fixture-existing")
        self.editor(change=True)
        name = f"ibkr-login-{selection}.env"
        result = self.secrets(f"cmd_edit {name}")
        self.assertEqual(result.returncode, 0, result.stderr)
        expected = ("# ti-gateway reads this file when selected (docs/runbooks/gateway-login.md).\nTWS_USERID=\nTWS_PASSWORD=\n"
                    f"TWS_TRADING_MODE={'live' if selection == 'live' else 'paper'}\n"
                    "TWS_READONLY_API=yes\n")
        if selection == "live":
            expected += "TWS_SECOND_FACTOR_DEVICE=IB Key\n"
        self.assertEqual((self.root / "editor-input").read_text(), expected)
        self.assertEqual((self.etc / (name + ".gpg")).read_text(), expected + "TWS_USERID=synthetic-user\n")
        self.assertFalse((self.etc / name).exists())

    def test_create_trad_ira(self):
        self.create("trad-ira")

    def test_create_roth_ira(self):
        self.create("roth-ira")

    def test_create_live(self):
        self.create("live")

    def test_create_wrong_passphrase(self):
        (self.etc / "secrets.env.gpg").write_text("fixture-existing")
        self.editor()
        result = self.secrets("cmd_edit ibkr-login-live.env", phrase="wrong")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("passphrase", result.stderr)
        self.assertFalse((self.root / "editor-input").exists())
        self.assertFalse((self.etc / "ibkr-login-live.env.gpg").exists())

    def test_create_without_existing_secret(self):
        self.editor()
        result = self.secrets("cmd_edit ibkr-login-live.env")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.root / "editor-input").exists())
        self.assertFalse((self.etc / "ibkr-login-live.env.gpg").exists())

    def test_create_editor_failure(self):
        (self.etc / "secrets.env.gpg").write_text("fixture-existing")
        self.editor(status=1)
        result = self.secrets("cmd_edit ibkr-login-live.env")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("editor exited", result.stderr)
        self.assertFalse((self.etc / "ibkr-login-live.env.gpg").exists())

    def test_optional_enumeration(self):
        names = [f"ibkr-login-{name}.env" for name in ("joint", "trad-ira", "roth-ira", "live")]
        for name in names:
            (self.etc / (name + ".gpg")).touch()
        result = self.secrets("encrypted_names")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(set(result.stdout.splitlines()), set(names))

    def command(self, *args):
        path = DEPLOY / "gateway-login.sh"
        self.assertTrue(path.exists(), "gateway-login.sh missing")
        script = path.read_text().replace("/etc/trend-investing/gateway-login", str(self.selector))
        return self.shell('id() { echo 0; }\n' + script, *args)

    def test_command_show(self):
        result = self.command()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "joint")

    def test_command_set(self):
        (self.etc / "ibkr-login-live.env.gpg").touch()
        old = self.selector.open()
        self.addCleanup(old.close)
        result = self.command("live")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.selector.read_text(), "live\n")
        self.assertEqual(old.read(), "joint\n")
        self.assertEqual(self.selector.stat().st_mode & 0o777, 0o644)
        self.assertIn("restart", result.stdout.lower())
        self.assertEqual(sorted(p.name for p in self.etc.iterdir()), ["gateway-login", "ibkr-login-live.env.gpg"])

    def test_command_invalid(self):
        for args in (("garbage",), ("joint", "live")):
            result = self.command(*args)
            self.assertEqual(result.returncode, 64, result.stderr)
            self.assertEqual(self.selector.read_text(), "joint\n")

    def test_command_show_invalid(self):
        for value in ("unknown", "joint\x00", "joint\n\n"):
            self.selector.write_text(value)
            result = self.command()
            self.assertEqual(result.returncode, 64, result.stderr)

    def test_command_directory(self):
        self.selector.unlink()
        self.selector.mkdir()
        result = self.command("live")
        self.assertEqual(result.returncode, 64, result.stderr)
        self.assertEqual(list(self.selector.iterdir()), [])

    def test_provision_selector_once(self):
        source = (DEPLOY / "provision.sh").read_text()
        start = source.find("init_gateway_login() {")
        self.assertGreaterEqual(start, 0, "init_gateway_login missing")
        function = source[start:source.index("\n}", start) + 2]
        self.selector.unlink()
        result = self.shell(function + '\netc=$1; init_gateway_login', str(self.etc))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.selector.read_text(), "joint\n")
        self.selector.write_text("live\n")
        result = self.shell(function + '\netc=$1; init_gateway_login', str(self.etc))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.selector.read_text(), "live\n")

    def test_legacy_enumeration_and_ordinary_refusal(self):
        (self.etc / "ibkr-login.env.gpg").write_text("synthetic")
        self.assertIn("ibkr-login.env", self.secrets("encrypted_names").stdout.splitlines())
        for command in ("cmd_unlock", "cmd_edit ibkr-login-live.env", "cmd_passphrase 0"):
            with self.subTest(command=command):
                result = self.secrets(command, phrase="must-not-be-read")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("migrate", result.stderr)
                self.assertNotIn("unexpected passphrase prompt", result.stderr)

    def test_migration_partial_unlocked_both_directions(self):
        for encrypted_new in (False, True):
            with self.subTest(encrypted_new=encrypted_new):
                enc = self.etc / ("ibkr-login-joint.env.gpg" if encrypted_new else "ibkr-login.env.gpg")
                plain = self.run / ("ibkr-login.env" if encrypted_new else "ibkr-login-joint.env")
                enc.write_text("synthetic")
                plain.write_text("synthetic-stale" if encrypted_new else "synthetic")
                result = self.secrets("cmd_migrate", phrase="must-not-be-read")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertFalse((self.run / "ibkr-login.env").exists())
                self.assertFalse((self.etc / "ibkr-login.env.gpg").exists())
                self.assertEqual((self.etc / "ibkr-login-joint.env.gpg").read_text(), "synthetic")
                if encrypted_new:
                    self.assertFalse((self.run / "ibkr-login-joint.env").exists())
                    self.assertIn(
                        "removed a leftover unlocked ibkr-login.env whose encrypted file was already renamed; "
                        "sudo ti-unlock makes ibkr-login-joint.env's unlocked copy from its encrypted file",
                        result.stdout,
                    )
                else:
                    self.assertEqual((self.run / "ibkr-login-joint.env").read_text(), "synthetic")
                    (self.run / "ibkr-login-joint.env").unlink()
                (self.etc / "ibkr-login-joint.env.gpg").unlink()

    def test_migration_partial_disk_pair_both_directions(self):
        for encrypted_new in (True, False):
            with self.subTest(encrypted_new=encrypted_new):
                enc = self.etc / ("ibkr-login-joint.env.gpg" if encrypted_new else "ibkr-login.env.gpg")
                plain = self.etc / ("ibkr-login.env" if encrypted_new else "ibkr-login-joint.env")
                enc.write_text("synthetic")
                plain.write_text("synthetic")
                result = self.secrets("migrate_login_name")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertFalse((self.etc / "ibkr-login.env").exists())
                self.assertFalse((self.etc / "ibkr-login.env.gpg").exists())
                for suffix in ("", ".gpg"):
                    (self.etc / ("ibkr-login-joint.env" + suffix)).unlink()

    def test_migration_partial_disk_conflict_preserved(self):
        for encrypted_new in (True, False):
            with self.subTest(encrypted_new=encrypted_new):
                enc = self.etc / ("ibkr-login-joint.env.gpg" if encrypted_new else "ibkr-login.env.gpg")
                plain = self.etc / ("ibkr-login.env" if encrypted_new else "ibkr-login-joint.env")
                enc.write_text("synthetic-encrypted")
                plain.write_text("synthetic-conflict")
                result = self.secrets("cmd_migrate")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("differ", result.stderr)
                self.assertEqual(enc.read_text(), "synthetic-encrypted")
                self.assertEqual(plain.read_text(), "synthetic-conflict")
                enc.unlink()
                plain.unlink()

    def test_command_sync_failure_preserves_old_selector(self):
        source = (DEPLOY / "gateway-login.sh").read_text()
        code = source.split("<<'PY'\n", 1)[1].split("\nPY", 1)[0]
        with mock.patch.object(sys, "argv", ["-", str(self.selector), "live"]), mock.patch.object(os, "fsync", side_effect=OSError("synthetic sync failure")):
            with self.assertRaises(OSError):
                exec(compile(code, "selector-durability", "exec"), {})
        self.assertEqual(self.selector.read_text(), "joint\n")
        self.assertEqual(list(self.etc.iterdir()), [self.selector])

    def test_command_missing_login(self):
        result = self.command("live")
        self.assertEqual(result.returncode, 64, result.stderr)
        self.assertIn("ti-secrets edit ibkr-login-live.env", result.stderr)
        self.assertEqual(self.selector.read_text(), "joint\n")

    def test_command_restart_notice(self):
        (self.etc / "ibkr-login-live.env.gpg").touch()
        result = self.command("live")
        self.assertEqual(result.returncode, 0, result.stderr)
        for phrase in ("next start of any kind", "owner restart", "healer", "ti-unlock", "systemd restart", "restart at once", "switch back to joint"):
            self.assertIn(phrase, result.stdout)

    def test_command_durable_replace(self):
        source = (DEPLOY / "gateway-login.sh").read_text()
        self.assertIn("os.fsync", source)
        code = source.split("<<'PY'\n", 1)[1].split("\nPY", 1)[0]
        events = []
        real_sync, real_replace = os.fsync, os.replace
        def sync(fd):
            events.append("directory" if stat.S_ISDIR(os.fstat(fd).st_mode) else "file")
            real_sync(fd)
        def replace(src, dst):
            self.assertEqual(Path(src).parent, self.selector.parent)
            events.append("rename")
            real_replace(src, dst)
        with mock.patch.object(sys, "argv", ["-", str(self.selector), "live"]), mock.patch.object(os, "fsync", sync), mock.patch.object(os, "replace", replace):
            exec(compile(code, "selector-durability", "exec"), {})
        self.assertEqual(events, ["file", "rename", "directory"])
        self.assertEqual(self.selector.read_text(), "live\n")

    def test_staging_rejects_mode_mismatch(self):
        for selection in ("joint", "live"):
            for mode in ("live" if selection != "live" else "paper", "", "unknown"):
                with self.subTest(selection=selection, mode=mode):
                    self.selector.write_text(selection + "\n")
                    self.login(selection).write_text(f"TWS_USERID=synthetic\nTWS_PASSWORD=synthetic\nTWS_TRADING_MODE={mode}\nTWS_READONLY_API=yes\n")
                    (self.run / "login.env").write_text("previous")
                    result = self.stage()
                    self.assertEqual(result.returncode, 64, result.stderr)
                    self.assertIn("TWS_TRADING_MODE", result.stderr)
                    self.assertIn(selection, result.stderr)
                    self.assertFalse((self.run / "login.env").exists())

    def test_staging_live_requires_readonly(self):
        self.selector.write_text("live\n")
        self.login("live", readonly=False)
        result = self.stage()
        self.assertEqual(result.returncode, 64, result.stderr)
        self.assertIn("TWS_READONLY_API", result.stderr)

    def test_unit_limits_failed_starts_and_pages(self):
        unit = (DEPLOY / "systemd/ti-gateway.service").read_text()
        section = unit.split("[Service]", 1)[0]
        for line in ("StartLimitIntervalSec=600", "StartLimitBurst=3", "OnFailure=ti-unit-failed@%n.service"):
            self.assertIn(line, section)
        self.assertIn("RestartPreventExitStatus=64 65 77 78", unit)

    def test_provision_refusal_before_any_install(self):
        source = (DEPLOY / "provision.sh").read_text()
        start = source.index("step_services() {")
        function = source[start:source.index("\n}", start) + 2]
        harness = r'''set -e
here=$1
log() { echo "$*"; }
die() { echo "$*" >&2; exit 1; }
require_native_programs() { :; }
secrets_migrate() { echo migration-refused >&2; return 64; }
install() { echo installed >&2; exit 99; }
step_services
'''
        result = self.shell(function + "\n" + harness, str(DEPLOY))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("migration-refused", result.stderr)
        self.assertNotIn("installed", result.stderr)
        self.assertIn("before installing Gateway", result.stdout + result.stderr)

    def test_provision_coordinated_install(self):
        source = (DEPLOY / "provision.sh").read_text()
        start = source.index("step_services() {")
        body = source[start:source.index('log "directories and config"', start)]
        self.assertIn("secrets_migrate", body)
        for name in ("stage-gateway-login.sh", "render-ibc-config.sh", "ti-gateway.service"):
            self.assertIn(name, body)
        self.assertLess(body.index("secrets_migrate"), body.index("install "))
        self.assertIn("systemctl daemon-reload", body)
        self.assertIn("systemctl is-failed --quiet ti-gateway.service", source)

    def test_systemd_rehearsal_stages_real_unit(self):
        source = (DEPLOY / "rehearsal/inside-systemd.sh").read_text()
        for text in ("/deploy/libexec/stage-gateway-login.sh", "gateway-probe.py", "systemctl start ti-gateway.service", "ti-gateway:ti-gateway:600", "systemctl stop ti-gateway.service", "test ! -e /run/ti-gateway/login.env", "os.listdir", 'read_bytes()'):
            self.assertIn(text, source)

    def test_operator_docs_match_consumption(self):
        root = DEPLOY.parent
        for name in ("docs/runbooks/gateway-login.md", "docs/manual/server.md"):
            text = (root / name).read_text()
            for phrase in ("unchanged first-edit template creates nothing", "next start of any kind", "healer", "ti-unlock", "systemd restart", "switch back to joint"):
                with self.subTest(name=name, phrase=phrase):
                    self.assertIn(phrase, text)
        text = (root / "docs/runbooks/lightsail-deployment.md").read_text()
        with self.subTest(name="lightsail handoff"):
            self.assertNotIn("all unlocked secrets as private systemd credentials", text)
            self.assertIn("ExecStartPre", text)
        text = (DEPLOY / "etc/ibkr-login-joint.env.example").read_text()
        with self.subTest(name="example handoff"):
            self.assertNotIn("LoadCredential", text)
            self.assertIn("when selected", text)

    def test_unit_credentials(self):
        unit = (DEPLOY / "systemd/ti-gateway.service").read_text()
        directives = [line for line in unit.splitlines() if line and not line.startswith("#")]
        self.assertFalse(any(line.startswith(("LoadCredential", "ImportCredential=", "SetCredential"))
                             for line in directives), directives)
        self.assertIn("ExecStartPre=+/usr/local/libexec/trend-investing/stage-gateway-login.sh", directives)
        self.assertIn("InaccessiblePaths=/run/trend-investing/secrets", directives)
        self.assertIn("RuntimeDirectory=ti-gateway", directives)
        self.assertIn("RuntimeDirectoryMode=0700", directives)
        self.assertFalse(any(line.startswith("RuntimeDirectoryPreserve=") and line != "RuntimeDirectoryPreserve=no"
                             for line in directives))
        self.assertIn("User=ti-gateway", directives)
        provision = (DEPLOY / "provision.sh").read_text()
        self.assertIn('install -m 0755 "$here/libexec/stage-gateway-login.sh" "$here/libexec/render-ibc-config.sh" /usr/local/libexec/trend-investing/', provision)
        self.assertIn("AssertPathExists=/run/trend-investing/secrets/secrets.env\n", unit)


class OptionalLoginFirstEditTests(test_roth.IRAFirstEditTests):
    """Apply the installed IRA lifecycle contract to every optional login."""

    names = (*test_roth.IRALoginSecretsTests.names, "ibkr-login-live.env")

    def test_partial_plaintext_migration_completes_with_real_gpg(self):
        for encrypted_new in (True, False):
            with self.subTest(encrypted_new=encrypted_new):
                name = "ibkr-login-joint.env" if encrypted_new else "ibkr-login.env"
                self.seed(name)
                plain = self.folder / ("ibkr-login.env" if encrypted_new else "ibkr-login-joint.env")
                plain.write_text("SYNTHETIC=existing\n")
                # The macOS harness replaces Linux shredding with removal of synthetic
                # bytes; inside-secrets.sh checks the OS boundary (ADR 0028).
                self.ok(self.command('shred() { test "$1" = -u; shift; rm "$@"; }; cmd_migrate',
                                     phrases=(self.phrase, self.phrase)))
                self.assertFalse(plain.exists())
                self.assertFalse((self.folder / "ibkr-login.env.gpg").exists())
                self.assertEqual(self.ok(self.decrypt("ibkr-login-joint.env")).stdout, "SYNTHETIC=existing\n")
                (self.folder / "ibkr-login-joint.env.gpg").unlink()

    def test_rotation_resume_includes_legacy_login(self):
        self.seed("ibkr-login.env")
        (self.folder / ".passphrase-change").write_text("change started\nrenaming\n")
        self.ok(self.command("cmd_passphrase 1", phrases=(self.phrase, self.new_phrase, self.new_phrase)))
        self.ok(self.decrypt("ibkr-login.env", self.new_phrase))
        self.assertNotEqual(self.decrypt("ibkr-login.env", self.phrase).returncode, 0)
        self.assertFalse((self.folder / ".passphrase-change").exists())

    def test_first_edit_refuses_existing_nonregular_paths_before_editor(self):
        for name in self.names:
            for suffix, kind in ((".gpg", "directory"), (".gpg", "link"), ("", "link")):
                with self.subTest(name=name, suffix=suffix, kind=kind):
                    path = self.folder / (name + suffix)
                    if kind == "directory":
                        path.mkdir()
                    else:
                        path.symlink_to(self.folder / "absent-target")
                    try:
                        before = self.snapshot()
                        result = self.command('cmd_edit "$1"', name, phrases=(self.phrase,), edit="unchanged")
                        self.assertNotEqual(result.returncode, 0)
                        self.assertIn("nothing changed", result.stderr)
                        self.assertEqual(self.snapshot(), before)
                        self.assertFalse((self.folder / "editor-before").exists())
                        self.assertEqual(list(self.run.iterdir()), [])
                        self.assertTrue(path.is_dir() if kind == "directory" else path.is_symlink())
                    finally:
                        (self.folder / "editor-before").unlink(missing_ok=True)
                        if kind == "directory":
                            path.rmdir()
                        else:
                            path.unlink()

    def test_first_edit_creates_then_second_edit_decrypts_existing_content(self):
        for name in self.names:
            with self.subTest(name=name):
                self.ok(self.command('cmd_edit "$1"', name, phrases=(self.phrase,)))
                template = (self.folder / "editor-before").read_text()
                fields = dict(line.split("=", 1) for line in template.splitlines()
                              if not line.startswith("#"))
                expected = {"TWS_USERID": "", "TWS_PASSWORD": "",
                            "TWS_TRADING_MODE": "paper", "TWS_READONLY_API": "yes"}
                if name == "ibkr-login-live.env":
                    expected.update(TWS_TRADING_MODE="live", TWS_SECOND_FACTOR_DEVICE="IB Key")
                self.assertEqual(fields, expected)
                first = self.ok(self.decrypt(name)).stdout
                self.assertEqual(first, template + "TWS_USERID=synthetic-user\n")
                self.assertFalse((self.folder / name).exists())
                self.assertEqual((self.folder / (name + ".gpg")).stat().st_mode & 0o777, 0o600)
                self.assertEqual(list(self.run.iterdir()), [])
                self.ok(self.command('cmd_edit "$1"', name, phrases=(self.phrase,), edit="second"))
                self.assertEqual((self.folder / "editor-before").read_text(), first)
                self.assertEqual(self.ok(self.decrypt(name)).stdout, first + "TWS_PASSWORD=synthetic-password\n")


class GatewayInstanceTests(unittest.TestCase):
    setUp = GatewayLoginTests.setUp
    shell = GatewayLoginTests.shell
    stage = GatewayLoginTests.stage
    render = GatewayLoginTests.render
    login = GatewayLoginTests.login

    def test_instance_logins_and_ports_do_not_collide(self):
        self.template.write_text(self.template.read_text() + "OverrideTwsApiPort=\n")
        self.selector.write_text("invalid-selector-must-not-be-read")
        script = (DEPLOY / "libexec/stage-gateway-login.sh").read_text().replace(
            'pwd.getpwnam("ti-gateway")', f'pwd.getpwnam({pwd.getpwuid(os.getuid()).pw_name!r})')
        for instance, port in (("trad-ira", "4003"), ("roth-ira", "4004")):
            self.login(instance)
            runtime = self.root / instance
            runtime.mkdir()
            env = {**os.environ, "TI_GATEWAY_INSTANCE": instance,
                   "TI_GATEWAY_API_PORT": port, "RUNTIME_DIRECTORY": str(runtime),
                   "TI_GATEWAY_LOGIN_FILE": str(self.selector),
                   "TI_GATEWAY_SECRETS_DIRECTORY": str(self.cred), "IBC_TEMPLATE": str(self.template)}
            staged = subprocess.run(["bash", "-c", script], env=env, capture_output=True)
            self.assertEqual(staged.returncode, 0, "instance staging failed")
            rendered = subprocess.run(["bash", str(DEPLOY / "libexec/render-ibc-config.sh")],
                                      env=env, capture_output=True)
            self.assertEqual(rendered.returncode, 0, "instance render failed")
            config = (runtime / "config.ini").read_text()
            self.assertTrue(f"IbLoginId=fixture-{instance}\n" in config)
            self.assertTrue(f"OverrideTwsApiPort={port}\n" in config)
        self.assertTrue((self.root / "trad-ira/login.env").exists())
        self.assertFalse((self.run / "login.env").exists())

    def test_instance_unit_and_provisioning(self):
        unit = (DEPLOY / "systemd/ti-gateway@.service").read_text()
        for line in ("EnvironmentFile=/etc/trend-investing/gateway-%i.env",
                     "Environment=TI_GATEWAY_INSTANCE=%i", "Environment=HOME=/var/lib/ti-gateway-%i",
                     "RuntimeDirectory=ti-gateway-%i", "StateDirectory=ti-gateway-%i",
                     "LogsDirectory=ti-gateway-%i", "PrivateTmp=true", "KillMode=control-group"):
            self.assertIn(line, unit)
        adapter = (DEPLOY / "systemd/ti-adapter@.service").read_text()
        self.assertIn("After=ti-gateway@%i.service ti-engine@%i.service", adapter)
        self.assertIn("Wants=ti-gateway@%i.service", adapter)
        self.assertIn("Wants=ti-gateway.service", (DEPLOY / "systemd/ti-adapter-boost.service").read_text())
        provision = (DEPLOY / "provision.sh").read_text()
        for port in (4003, 4004):
            self.assertIn(f"ufw deny in proto tcp to any port {port}", provision)
        self.assertNotRegex(provision, r"systemctl (?:enable|start).*ti-gateway@")
        self.assertIn('"$etc/gateway-trad-ira.env"', provision)
        self.assertIn("TI_GATEWAY_API_PORT=4003", provision)
        self.assertIn("/var/lib/ti-gateway-heal/trad-ira", provision)

    def test_joint_renderer_leaves_port_unchanged(self):
        self.login()
        self.template.write_text(self.template.read_text() + "OverrideTwsApiPort=4002\n")
        self.assertEqual(self.render().returncode, 0)
        self.assertTrue("OverrideTwsApiPort=4002\n" in (self.run / "config.ini").read_text())

    def test_instance_rejects_unknown_and_port_invalid(self):
        self.login()
        for instance in ("boost", "live", "unknown", "../joint"):
            with mock.patch.dict(os.environ, {"TI_GATEWAY_INSTANCE": instance}):
                self.assertEqual(self.stage().returncode, 64)
                self.assertFalse((self.run / "login.env").exists())
        self.assertEqual(self.stage().returncode, 0)
        for port in ("0", "65536", "abc"):
            result = subprocess.run(["bash", str(DEPLOY / "libexec/render-ibc-config.sh")],
                                    env={**os.environ, "RUNTIME_DIRECTORY": str(self.run),
                                         "IBC_TEMPLATE": str(self.template), "TI_GATEWAY_API_PORT": port},
                                    capture_output=True)
            self.assertEqual(result.returncode, 64)

    def test_instance_shares_only_application_writes(self):
        unit = (DEPLOY / "systemd/ti-gateway@.service").read_text()
        self.assertIn("Environment=TI_GATEWAY_APP_HOME=/var/lib/ti-gateway", unit)
        self.assertIn("ProtectSystem=strict", unit)
        writable = {path for line in unit.splitlines() if line.startswith("ReadWritePaths=")
                    for path in line.split("=", 1)[1].split()}
        self.assertEqual(writable, {"/var/lib/ti-gateway-%i", "/var/log/ti-gateway-%i",
                                    "-/var/lib/ti-gateway/Jts/ibgateway"})
        provision = (DEPLOY / "provision.sh").read_text()
        self.assertNotIn("init_gateway_instance", provision)
        self.assertNotIn("instance-copy", provision)
        self.assertNotIn("instance-heap", provision)
        self.assertIn("/var/lib/ti-gateway-trad-ira/Jts", provision)

    def test_installer_rehearsal_accepts_shared_application(self):
        # Execute the installed-layout checks without root or an IBKR login
        # (docs/manual/server.md, "The Traditional IRA's Gateway"). Only GNU
        # stat's ownership/mode output is simulated; paths and inodes are real.
        source = (DEPLOY / "rehearsal/inside.sh").read_text()
        block = source[source.index("# The second Gateway remains"):source.index("# A corrupt cached download")]
        import re
        block = re.sub(r"/(?:etc|var|run)/", lambda m: str(self.root) + m[0], block)
        unit = self.root / "etc/systemd/system/ti-gateway@.service"
        unit.parent.mkdir(parents=True)
        text = (DEPLOY / "systemd/ti-gateway@.service").read_text()
        text = re.sub(r"/(?:etc|var|run)/", lambda m: str(self.root) + m[0], text)
        unit.write_text(text)
        env = self.root / "etc/trend-investing/gateway-trad-ira.env"
        env.parent.mkdir()
        env.write_text("TI_GATEWAY_API_PORT=4003\n")
        settings = self.root / "var/lib/ti-gateway-trad-ira/Jts"
        for name in ("run/ti-gateway-trad-ira", "var/log/ti-gateway-trad-ira",
                     "var/lib/ti-gateway/Jts", "var/lib/ti-gateway-heal/trad-ira",
                     "var/lib/ti-gateway-trad-ira/Jts"):
            (self.root / name).mkdir(parents=True)
        harness = """set -eu
fail() { echo "$*" >&2; exit 1; }
pass() { :; }
stat() {
  case "$2" in
    %U:%G) echo ti-gateway:ti-gateway ;;
    %U:%a) echo root:700 ;;
    *) return 1 ;;
  esac
}
export TI_REHEARSAL_SKIP_INSTALLER=0
"""
        result = self.shell(harness + block)
        self.assertEqual(result.returncode, 0, result.stderr)
        wrong_owner = harness.replace("echo ti-gateway:ti-gateway", "echo root:root")
        self.assertNotEqual(self.shell(wrong_owner + block).returncode, 0)
        settings.rmdir()
        settings.symlink_to(self.root / "var/lib/ti-gateway/Jts", target_is_directory=True)
        self.assertNotEqual(self.shell(harness + block).returncode, 0)
        settings.unlink()
        settings.mkdir()
        env.write_text("TI_GATEWAY_API_PORT=4003\nTI_GATEWAY_APP_HOME=/another/application\n")
        self.assertNotEqual(self.shell(harness + block).returncode, 0)
        env.write_text("TI_GATEWAY_API_PORT=4003\n")
        for writable in ("/var/lib/ti-gateway", "/var/lib/ti-gateway/Jts"):
            unit.write_text(text + f"\nReadWritePaths={self.root}{writable}\n")
            self.assertNotEqual(self.shell(harness + block).returncode, 0)

    def test_instance_port_required(self):
        self.login()
        self.assertEqual(self.stage().returncode, 0)
        for port in (None, ""):
            env = {**os.environ, "TI_GATEWAY_INSTANCE": "trad-ira", "RUNTIME_DIRECTORY": str(self.run), "IBC_TEMPLATE": str(self.template)}
            env.pop("TI_GATEWAY_API_PORT", None)
            if port is not None:
                env["TI_GATEWAY_API_PORT"] = port
            result = subprocess.run(["bash", str(DEPLOY / "libexec/render-ibc-config.sh")], env=env, capture_output=True)
            self.assertEqual(result.returncode, 64)

    def test_adapter_port_guard(self):
        import json
        settings = self.etc / "gateway-trad-ira.env"
        settings.write_text("TI_GATEWAY_API_PORT=4003\n")
        config = self.etc / "trad-ira.json"
        script = DEPLOY / "libexec/check-adapter-port.py"
        self.assertTrue(script.exists())
        for value in ("4003", '"4003"', "'4003'", '  "4003"  ', "  '4003'  ", " 4003 "):
            for port, status in ((4002, 64), (4003, 0)):
                with self.subTest(value=value, port=port):
                    settings.write_text(f"TI_GATEWAY_API_PORT={value}\n" if value == "4003" else f"  TI_GATEWAY_API_PORT = {value}\n")
                    config.write_text(json.dumps({"port": port}))
                    result = subprocess.run([sys.executable, str(script), str(config), str(settings)], text=True, capture_output=True)
                    self.assertEqual(result.returncode, status, result.stderr)
                    if status:
                        self.assertIn(str(config), result.stderr)
                        self.assertIn("4003", result.stderr)
        unit = (DEPLOY / "systemd/ti-adapter@.service").read_text()
        self.assertIn("ExecStartPre=/usr/bin/python3 /usr/local/libexec/trend-investing/check-adapter-port.py /etc/trend-investing/adapters/%i.json /etc/trend-investing/gateway-%i.env", unit)

    def test_staggered_display_and_restart_settings(self):
        source = (DEPLOY / "provision.sh").read_text().split('"$etc/gateway-trad-ira.env"', 1)[1].split('CONF', 2)[1]
        for line in ("TI_GATEWAY_DISPLAY=100", "TI_GATEWAY_RESTART_TIME=11:55 PM", "TI_GATEWAY_COLD_RESTART=09:20"):
            self.assertIn(line, source)

    def test_mac_masks_template_and_uses_external_adapter_override(self):
        source = (DEPLOY / "provision.sh").read_text()
        self.assertIn("systemctl mask ti-gateway.service ti-gateway@.service", source)
        self.assertIn('ti-gateway@.service) mac_mini && continue', source)
        start = source.index("install_mac_adapter_unit() {")
        function = source[start:source.index("\n}\n", start) + 2]
        result = self.shell(function + '\nhere=$1; unit_dir=$2; install_mac_adapter_unit', str(DEPLOY), str(self.etc))
        self.assertEqual(result.returncode, 0, result.stderr)
        text = (self.etc / "ti-adapter@.service").read_text()
        self.assertNotIn("Wants=ti-gateway@", text)
        self.assertNotIn("After=ti-gateway@", text)
        self.assertIn("After=ti-engine@%i.service", text)
        self.assertIn("/etc/trend-investing/adapters/%i.json 4002", text)
        self.assertIn('test "%i" != boost', text)




if __name__ == "__main__":
    unittest.main()
