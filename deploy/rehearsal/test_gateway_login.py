"""Login selection and secret lifecycle (docs/runbooks/gateway-login.md, ADR 0028)."""

# CASE TABLE (one test per row; no processes are signalled):
# joint present -> selected config: test_selector_joint
# trad-ira present -> selected config: test_selector_trad_ira
# roth-ira present -> selected config: test_selector_roth_ira
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
        self.selected("trad-ira")

    def test_selector_roth_ira(self):
        self.selected("roth-ira")

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

    def editor(self, status=0):
        path = self.etc / "editor"
        path.write_text(f'#!/bin/sh\ncp "$1" "{self.root}/editor-input"\nexit {status}\n')
        path.chmod(0o700)

    def create(self, selection):
        (self.etc / "secrets.env.gpg").write_text("fixture-existing")
        self.editor()
        name = f"ibkr-login-{selection}.env"
        result = self.secrets(f"cmd_edit {name}")
        self.assertEqual(result.returncode, 0, result.stderr)
        expected = ("TWS_USERID=\nTWS_PASSWORD=\n"
                    f"TWS_TRADING_MODE={'live' if selection == 'live' else 'paper'}\n"
                    "TWS_READONLY_API=yes\n")
        if selection == "live":
            expected += "TWS_SECOND_FACTOR_DEVICE=IB Key\n"
        self.assertEqual((self.root / "editor-input").read_text(), expected)
        self.assertEqual((self.etc / (name + ".gpg")).read_text(), expected)
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
        old = self.selector.open()
        self.addCleanup(old.close)
        result = self.command("live")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.selector.read_text(), "live\n")
        self.assertEqual(old.read(), "joint\n")
        self.assertEqual(self.selector.stat().st_mode & 0o777, 0o644)
        self.assertIn("restart", result.stdout.lower())
        self.assertEqual(sorted(p.name for p in self.etc.iterdir()), ["gateway-login"])

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
        self.assertIn('install -m 0755 "$here"/libexec/*.sh /usr/local/libexec/trend-investing/', provision)
        self.assertIn("AssertPathExists=/run/trend-investing/secrets/secrets.env\n", unit)


if __name__ == "__main__":
    unittest.main()
