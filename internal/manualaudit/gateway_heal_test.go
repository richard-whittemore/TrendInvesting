package manualaudit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayHealerManualStatesGatesAndInstallConsent(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "manual", "server.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	for _, want := range []string{"ti-gateway-heal@.service", "gateway-heal:", "two hours", "ten minutes", "pending.json.lock", "deploy/provision.sh services", "owner's yes", "last-restart.json", "once per incident"} {
		if !strings.Contains(text, want) {
			t.Errorf("manual lacks %q", want)
		}
	}
}

func TestGatewayHealerVetoRecoveryAndLoginContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "manual", "server.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	for _, want := range []string{
		"contains the exact", "sudo systemctl reset-failed <adapter>", "owner-stopped", "another-healer", "evidence-error",
		"only its own adapter", "already-restarted", "same unattended login", "every nightly and Sunday restart", "phone approval", "existing page stands",
		"10-minute margin", "/etc/trend-investing/gateway.env", "/var/lib/ti-gateway-heal/last-restart.json", "provisional attempt", "before publishing",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("manual lacks %q", want)
		}
	}
	for _, path := range []string{"docs/manual/server.md", "deploy/systemd/ti-adapter-boost.service", "deploy/systemd/ti-adapter@.service"} {
		raw, err := os.ReadFile(filepath.Join(repoRoot(t), path))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "stop its healer") || strings.Contains(string(raw), "Alternatively stop the healer") {
			t.Errorf("%s documents stopping a healer as a veto", path)
		}
	}

	if strings.Contains(text, "every eligible failed adapter") {
		t.Error("manual still promises cross-account recovery")
	}
	if strings.Contains(text, "invocation journal ends with") {
		t.Error("manual still requires terminal remedy")
	}
}

func TestGatewayHealerSandboxAndRehearsalContract(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(repoRoot(t), path))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	unit := read("deploy/systemd/ti-gateway-heal@.service")
	for _, want := range []string{"ReadWritePaths=/var/lib/ti-gateway-heal", "ReadOnlyPaths=/var/lib/trend-investing/adapters", "CapabilityBoundingSet=CAP_DAC_READ_SEARCH", "ProtectKernelModules=true", "ProtectKernelLogs=true", "RestrictNamespaces=true", "LockPersonality=true", "MemoryDenyWriteExecute=true", "SystemCallFilter=@system-service", "RestrictAddressFamilies=AF_UNIX"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q", want)
		}
	}
	provision := read("deploy/provision.sh")
	for _, want := range []string{"install -d -o root -g root -m 0700 /var/lib/ti-gateway-heal", "install_adapter_lock", "pending.json.lock"} {
		if !strings.Contains(provision, want) {
			t.Errorf("provision lacks %q", want)
		}
	}
	drill := read("deploy/rehearsal/gateway-heal.sh")
	for _, want := range []string{"row=rate-limit step=no-action", "ti-unit-failed@ti-adapter-boost.service.service", "heal-clock-offset-seconds", "provisional attempt", "CapabilityBoundingSet", "ReadOnlyPaths", "pending.json.lock"} {
		if !strings.Contains(drill, want) {
			t.Errorf("drill lacks %q", want)
		}
	}
	if strings.Contains(drill, "(rate-limit|same-incident)") {
		t.Error("drill accepts same-incident instead of rate-limit")
	}
}

func TestGatewayHealerRehearsalProvidesReadSearchCapability(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "rehearse.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "DAC_READ_SEARCH") {
		t.Fatal("rehearsal container cannot grant the healer CAP_DAC_READ_SEARCH")
	}
}

func TestGatewayProvisionedRelayLockPreservesInodeAndBytes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "provision.sh"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(raw), "install_adapter_lock() {")
	if !ok {
		t.Fatal("missing lock provisioner")
	}
	body, _, ok := strings.Cut(rest, "\n}\n")
	if !ok {
		t.Fatal("missing function end")
	}

	// Run the actual embedded Python as the current unprivileged test user.
	// The runuser stand-in checks the requested identity; root metadata tools
	// only record forbidden calls (server.md, Gateway healer).
	script := `install_adapter_lock() {` + body + `
}
record=$2
runuser() { printf 'runuser %s\n' "$*" >>"$record"; test "$1 $2 $3" = '-u ti-trader --' || return 1; shift 3; "$@"; }
python3() { printf 'python3\n' >>"$record"; command python3 "$@"; }
chown() { printf 'FORBIDDEN chown\n' >>"$record"; return 1; }
chmod() { printf 'FORBIDDEN chmod\n' >>"$record"; return 1; }
install() { printf 'FORBIDDEN install\n' >>"$record"; return 1; }
log() { :; }
install_adapter_lock "$1"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "pending.json.lock")
	record := filepath.Join(dir, "calls")
	run := func() error { return exec.Command("bash", "-c", script, "lock-test", path, record).Run() }

	if err := run(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("synthetic retained evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !os.SameFile(before, after) || string(got) != "synthetic retained evidence" || after.Mode().Perm() != 0o600 {
		t.Fatalf("provision replaced/truncated lock: %q %v", got, err)
	}

	calls, err := os.ReadFile(record)
	if err != nil || strings.Contains(string(calls), "FORBIDDEN") || strings.Count(string(calls), "runuser -u ti-trader -- python3") != 2 || strings.Count(string(calls), "\npython3\n") != 2 {
		t.Fatalf("unsafe creation calls: %q %v", calls, err)
	}
	// Concurrent provisioners must observe the same existing inode and bytes.
	commands := make([]*exec.Cmd, 8)
	for i := range commands {
		commands[i] = exec.Command("bash", "-c", script, "lock-test", path, record)
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	after, err = os.Stat(path)
	got, readErr := os.ReadFile(path)
	if err != nil || readErr != nil || !os.SameFile(before, after) || string(got) != "synthetic retained evidence" {
		t.Fatal("concurrent provisioning replaced the lock")
	}

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := run(); err == nil {
		t.Fatal("provision repaired or accepted unsafe lock permissions")
	}
	after, err = os.Lstat(path)
	if err != nil || after.Mode().Perm() != 0o640 || !os.SameFile(before, after) {
		t.Fatal("refusal mutated lock", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing"), path); err != nil {
		t.Fatal(err)
	}
	if err := run(); err == nil {
		t.Fatal("provision accepted a symlink lock")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "protected")
	if err := os.WriteFile(target, []byte("retained target"), 0o400); err != nil {
		t.Fatal(err)
	}
	targetBefore, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := run(); err == nil {
		t.Fatal("provision accepted a link to an existing file")
	}
	targetAfter, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(target)
	if err != nil || !os.SameFile(targetBefore, targetAfter) || targetAfter.Mode() != targetBefore.Mode() || string(got) != "retained target" {
		t.Fatal("symlink target changed", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := run(); err == nil {
		t.Fatal("provision accepted a directory lock")
	}

}

func TestGatewayHealerCaseTableNamesNonemptyCounter(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "internal", "gatewayheal", "heal_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "nonzero login-failures counter") || !strings.Contains(text, "any nonempty login-failures counter file") || !strings.Contains(text, "including retained reset metadata") {
		t.Fatal("case table must say any nonempty login-failures counter file, including retained reset metadata")
	}
}
