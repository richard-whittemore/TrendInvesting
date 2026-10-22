package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	hookUnitPath    = "../../deploy/systemd/ti-unit-failed@.service"
	engineUnitPath  = "../../deploy/systemd/ti-engine@.service"
	adapterUnitPath = "../../deploy/systemd/ti-adapter-boost.service"
)

// directive returns every value of key in section of a unit file.
func directive(t *testing.T, path, section, key string) []string {
	t.Helper()
	var values []string
	for _, d := range unitDirectives(t, path) {
		if d.section == section && d.key == key {
			values = append(values, d.value)
		}
	}
	return values
}

// TestEveryTradingUnitHandsItsFailureToTheHook: the engine, the adapter and
// the reporter are never restarted by systemd (the reporter is fired by its
// timer), so a failed one is paged by the hook with its own name.
func TestEveryTradingUnitHandsItsFailureToTheHook(t *testing.T) {
	for _, path := range []string{engineUnitPath, adapterUnitPath, reporterUnitPath} {
		if got := directive(t, path, "[Unit]", "OnFailure"); len(got) != 1 || got[0] != "ti-unit-failed@%n.service" {
			t.Errorf("%s: OnFailure = %v, want ti-unit-failed@%%n.service", path, got)
		}
	}
	for _, path := range []string{engineUnitPath, adapterUnitPath} {
		if got := directive(t, path, "[Service]", "Restart"); len(got) != 1 || got[0] != "no" {
			t.Errorf("%s: Restart = %v; the page replaces no restart, it announces one the owner makes", path, got)
		}
	}
	if got := directive(t, hookUnitPath, "[Unit]", "OnFailure"); len(got) != 0 {
		t.Errorf("the hook names an OnFailure %v: a failing hook would page about itself without end", got)
	}
}

func TestTheHookUnitRunsTheReportersPageAsTheReporterUser(t *testing.T) {
	one := func(section, key string) string {
		t.Helper()
		got := directive(t, hookUnitPath, section, key)
		if len(got) != 1 {
			t.Fatalf("%s %s = %v, want one", section, key, got)
		}
		return got[0]
	}
	if one("[Service]", "Type") != "oneshot" || one("[Service]", "User") != "ti-trader" {
		t.Error("the hook is a oneshot run as ti-trader, the user the reporter's state belongs to")
	}
	if one("[Service]", "SupplementaryGroups") != "systemd-journal" {
		t.Error("the hook cannot read the failed unit's last line without the journal group")
	}
	if one("[Service]", "LoadCredential") != "secrets:"+unlockedSecrets {
		t.Error("the hook reads the same credential file as the reporter")
	}
	if one("[Service]", "ReadWritePaths") != "/var/lib/trend-investing/reports" {
		t.Error("the hook writes only the reports directory")
	}
	if !strings.Contains(one("[Service]", "InaccessiblePaths"), "-/var/lib/trend-investing/engines/boost/operator") {
		t.Error("the hook can reach the operator inbox")
	}
	if got := directive(t, hookUnitPath, "[Service]", "Restart"); len(got) != 0 {
		t.Errorf("Restart = %v: the reporter's next firing retries a page the hook could not deliver", got)
	}
}

// TestTheHookAsItsUnitRunsItPagesOnceThroughTheChannels: run with the unit's
// own command line, expanded as systemd does for the engine's failure, with
// stand-in channels and journal.
func TestTheHookAsItsUnitRunsItPagesOnceThroughTheChannels(t *testing.T) {
	execStart := directive(t, hookUnitPath, "[Service]", "ExecStart")
	if len(execStart) != 1 {
		t.Fatalf("ExecStart = %v", execStart)
	}
	reports, credentials := t.TempDir(), t.TempDir()
	line := strings.ReplaceAll(execStart[0], "/var/lib/trend-investing/reports", reports)
	args := expandExecStart(line, map[string]string{"%i": "ti-engine@boost.service", "%d": credentials}, nil)
	if len(args) == 0 || args[0] != "/opt/trend-investing/bin/ti-reporter" || args[1] != "notify-unit-failed" {
		t.Fatalf("ExecStart runs %v", args)
	}
	if args[len(args)-1] != "ti-engine@boost.service" {
		t.Errorf("the failed unit must be the last argument: %v", args)
	}
	var ntfy, pager countingServer
	ntfyServer := ntfy.serve(t, http.StatusOK, "")
	pagerServer := pager.serve(t, http.StatusAccepted, `{"status":"success"}`)
	secrets := "TI_NTFY_TOPIC=topic\nTI_NTFY_SERVER=" + ntfyServer.URL + "\nTI_PAGERDUTY_ROUTING_KEY=key\nTI_PAGERDUTY_URL=" + pagerServer.URL + "\n"
	if err := os.WriteFile(filepath.Join(credentials, "secrets"), []byte(secrets), 0o600); err != nil {
		t.Fatal(err)
	}
	// The server's journalctl is not on this machine: a stand-in.
	systemd := newStandInSystemd(t)
	args = append([]string{args[1]}, append([]string{"-journalctl", systemd.journalctl()}, args[2:]...)...)
	// systemd hands the hook the failed unit and its invocation.
	monitor := env(map[string]string{"MONITOR_UNIT": "ti-engine@boost.service", "MONITOR_INVOCATION_ID": firstRun})
	for i := 0; i < 2; i++ {
		if err := run(args, monitor, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, http.DefaultClient); err != nil {
			t.Fatalf("the hook as the unit runs it: %v", err)
		}
	}
	if ntfy.count() != 1 || pager.count() != 1 {
		t.Errorf("ntfy took %d and PagerDuty %d pages for one failure, want 1 each", ntfy.count(), pager.count())
	}
	if _, err := os.Stat(filepath.Join(reports, "unit-failures.json")); err != nil {
		t.Errorf("no record beside the reporters' state: %v", err)
	}
}

// TestTheSecretsExampleNamesTheHeartbeatURL: the owner fills TI_HEARTBEAT_URL
// in on the server, and the example no longer says there is no such line.
func TestTheSecretsExampleNamesTheHeartbeatURL(t *testing.T) {
	values := readAssignments(t, secretsExample)
	if v, ok := values["TI_HEARTBEAT_URL"]; !ok || v != "" {
		t.Errorf("TI_HEARTBEAT_URL = %q (present %v), want a named, empty line", v, ok)
	}
	raw, err := os.ReadFile(secretsExample)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "is not built") {
		t.Error("the secrets example still says the outside heartbeat is not built")
	}
}

// TestEveryUnitThatLoadsASecretRefusesToStartWhileTheSecretsAreLocked: the
// secret files rest on disk only encrypted, and ti-unlock decrypts them after
// each boot into a root-only tmpfs (owner decision 2026-10-22). Every unit
// that loads a secret loads it from that tmpfs, never from /etc, and asserts
// the decrypted file exists, so until the owner unlocks, systemd refuses to
// start it ("Assertion failed"; AssertPathExists=; a refused timer
// firing leaves the timer running, so the next firing after the unlock
// runs). The reporter therefore cannot run while locked: its heartbeat pings
// stop and the outside check pages the owner.
func TestEveryUnitThatLoadsASecretRefusesToStartWhileTheSecretsAreLocked(t *testing.T) {
	units, err := filepath.Glob("../../deploy/systemd/*.service")
	if err != nil || len(units) == 0 {
		t.Fatalf("no units found: %v", err)
	}
	loaders := 0
	for _, path := range units {
		asserted := map[string]bool{}
		for _, a := range directive(t, path, "[Unit]", "AssertPathExists") {
			asserted[a] = true
		}
		for _, load := range directive(t, path, "[Service]", "LoadCredential") {
			loaders++
			_, source, _ := strings.Cut(load, ":")
			if !strings.HasPrefix(source, unlockedDir) {
				t.Errorf("%s: LoadCredential=%s loads from outside %s, where ti-unlock decrypts the secrets", path, load, unlockedDir)
			}
			if !asserted[source] {
				t.Errorf("%s: LoadCredential=%s but no AssertPathExists=%s: the unit would start, or fail at credential setup (Failed to set up credentials), which does not say the secrets are locked", path, load, source)
			}
		}
		for _, d := range unitDirectives(t, path) {
			if strings.Contains(d.value, "/etc/trend-investing/secrets.env") || strings.Contains(d.value, "/etc/trend-investing/ibkr-login.env") ||
				strings.Contains(d.value, "/etc/trend-investing/accounts/") {
				t.Errorf("%s: %s=%s names a secret file on disk; only its encrypted copy rests there", path, d.key, d.value)
			}
		}
	}
	// The gateway, the engines, the Boost adapter, the data job, the reporter,
	// the failed-unit page and the Flex reader (secrets and account).
	if loaders != 8 {
		t.Errorf("%d LoadCredential= lines across the units, want 8", loaders)
	}
}
