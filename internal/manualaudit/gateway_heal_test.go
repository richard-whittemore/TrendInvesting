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
		"every eligible failed adapter", "same unattended login", "every nightly and Sunday restart", "phone approval", "existing page stands",
		"10-minute margin", "/etc/trend-investing/gateway.env", "/var/lib/ti-gateway-heal/last-restart.json", "provisional attempt", "before publishing",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("manual lacks %q", want)
		}
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

func TestProvisionedRelayLockPreservesInodeAndBytes(t *testing.T) {
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
	script := "install_adapter_lock() {" + body + "\n}\nrecord=$2; chown() { printf '%s\\n' \"$*\" >>\"$record\"; }; install() { local p=\"${@: -1}\"; : >\"$p\"; chmod 0600 \"$p\"; }; log() { :; }; install_adapter_lock \"$1\""
	dir := t.TempDir()
	path := filepath.Join(dir, "pending.json.lock")
	record := filepath.Join(dir, "chown-calls")
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
	owners, err := os.ReadFile(record)
	if err != nil || string(owners) != strings.Repeat("ti-trader:ti-trader "+path+"\n", 2) {
		t.Fatalf("ownership calls: %q %v", owners, err)
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
