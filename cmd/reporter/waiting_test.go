package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The wait for an account's first run: before the engine has been started
// once there is no run.env, so there is no log to read and nothing to report.
// That is a quiet, successful state, not a fault: the firing says it is
// waiting, alerts nobody, and pings the outside heartbeat so the server is
// shown alive (ADR 0019, "Alerting": the outside heartbeat). Once run.env
// exists the firing is unchanged.

func waitingRun(t *testing.T, runEnv string, extra []string, out *bytes.Buffer) (*standInEndpoint, error) {
	t.Helper()
	e, server := newStandInEndpoint(t)
	dir := t.TempDir()
	args := []string{"-journal", filepath.Join(dir, ".log"), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-account", "boost", "-run-env", runEnv}
	args = append(args, extra...)
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL, "TI_HEARTBEAT_URL": server.URL + heartbeatPath})
	err := run(args, environment, func() time.Time { return t0.Add(time.Hour) }, out, server.Client())
	return e, err
}

func TestAMissingRunEnvIsAQuietSuccessThatPingsTheHeartbeatOnce(t *testing.T) {
	var out bytes.Buffer
	e, err := waitingRun(t, filepath.Join(t.TempDir(), "run.env"), nil, &out)
	if err != nil {
		t.Fatalf("waiting for the first run failed the firing: %v", err)
	}
	if !strings.Contains(out.String(), "waiting for the account's first run") {
		t.Errorf("no waiting line: %q", out.String())
	}
	if n := strings.Count(out.String(), "\n"); n != 1 {
		t.Errorf("%d lines, want exactly one: %q", n, out.String())
	}
	if e.count("ping") != 1 {
		t.Errorf("pings = %d, want 1 (events %v)", e.count("ping"), e.events)
	}
	if e.count("ntfy") != 0 {
		t.Errorf("a channel was sent to while waiting: %v", e.events)
	}
	if strings.Contains(out.String(), heartbeatPath) {
		t.Errorf("the heartbeat address was printed: %q", out.String())
	}
}

func TestAnEmptyRunEnvIsTheSameQuietWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.env")
	writeFileT(t, path, " \n")
	var out bytes.Buffer
	e, err := waitingRun(t, path, nil, &out)
	if err != nil {
		t.Fatalf("an empty run.env failed the firing: %v", err)
	}
	if e.count("ping") != 1 || e.count("ntfy") != 0 || !strings.Contains(out.String(), "waiting for the account's first run") {
		t.Errorf("pings %d, sends %d, out %q", e.count("ping"), e.count("ntfy"), out.String())
	}
}

func TestWaitingWithNoHeartbeatURLPingsNothingAndFailsNothing(t *testing.T) {
	e, server := newStandInEndpoint(t)
	dir := t.TempDir()
	args := []string{"-journal", filepath.Join(dir, ".log"), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-account", "boost", "-run-env", filepath.Join(dir, "run.env")}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL})
	if err := run(args, environment, func() time.Time { return t0 }, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatal(err)
	}
	if e.count("ping") != 0 || e.count("ntfy") != 0 {
		t.Errorf("events %v", e.events)
	}
}

func TestWaitingHeartbeatOffOverridesSharedSecrets(t *testing.T) {
	e, server := newStandInEndpoint(t)
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets")
	writeFileT(t, secrets, "TI_HEARTBEAT_URL="+server.URL+heartbeatPath+"\nTI_NTFY_TOPIC=t\nTI_NTFY_SERVER="+server.URL+"\n")
	args := []string{"-journal", filepath.Join(dir, ".log"), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-secrets-file", secrets, "-account", "boost", "-run-env", filepath.Join(dir, "run.env")}
	if err := run(args, env(map[string]string{"TI_HEARTBEAT": "off"}), func() time.Time { return t0 }, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatal(err)
	}
	if e.count("ping") != 0 {
		t.Errorf("pings = %d, want 0", e.count("ping"))
	}
}

func TestWaitingStillFailsWhenTheSecretsFileCannotBeRead(t *testing.T) {
	e, server := newStandInEndpoint(t)
	dir := t.TempDir()
	args := []string{"-journal", filepath.Join(dir, ".log"), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-secrets-file", filepath.Join(dir, "missing"), "-account", "boost", "-run-env", filepath.Join(dir, "run.env")}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL, "TI_HEARTBEAT_URL": server.URL + heartbeatPath})
	if err := run(args, environment, func() time.Time { return t0 }, &bytes.Buffer{}, server.Client()); err == nil {
		t.Fatal("an unreadable secrets file should still fail the firing")
	}
	if e.count("ping") != 0 {
		t.Errorf("a faulty firing pinged: %v", e.events)
	}
}

func TestARunEnvThatCannotBeReadFailsTheFiring(t *testing.T) {
	dir := t.TempDir()
	// A directory where the file should be: present, but not readable as one.
	if err := os.Mkdir(filepath.Join(dir, "run.env"), 0o755); err != nil {
		t.Fatal(err)
	}
	e, err := waitingRun(t, filepath.Join(dir, "run.env"), nil, &bytes.Buffer{})
	if err == nil {
		t.Fatal("a run.env that cannot be read is a fault, not a wait")
	}
	if e.count("ping") != 0 {
		t.Errorf("pinged: %v", e.events)
	}
}

func TestWithARunEnvPresentTheFiringIsUnchangedAndAMissingLogStillFails(t *testing.T) {
	dir := t.TempDir()
	runEnv := filepath.Join(dir, "run.env")
	writeFileT(t, runEnv, "TI_RUN=r1\n")
	var out bytes.Buffer
	e, err := waitingRun(t, runEnv, nil, &out)
	if err == nil {
		t.Fatal("a log named by run.env that is missing must still fail the firing")
	}
	if strings.Contains(out.String(), "waiting") {
		t.Errorf("said it was waiting with a run.env present: %q", out.String())
	}
	if e.count("ping") != 0 {
		t.Errorf("a failed firing pinged: %v", e.events)
	}
}

func TestWithARunEnvPresentAndTheLogReadableTheFiringAlertsAndPingsAsBefore(t *testing.T) {
	e, server := newStandInEndpoint(t)
	dir := t.TempDir()
	runEnv := filepath.Join(dir, "run.env")
	writeFileT(t, runEnv, "TI_RUN=r1\n")
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-account", "boost", "-run-env", runEnv}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL, "TI_HEARTBEAT_URL": server.URL + heartbeatPath})
	if err := run(args, environment, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatal(err)
	}
	if e.count("ntfy") == 0 || e.count("ping") != 1 {
		t.Errorf("events %v", e.events)
	}
}

// A waiting firing is a normal firing without the journal step: the channels,
// the trading units, the clock and the reporter's own failure page are all
// handled as on any firing, and only the reading of the journal is skipped.

func TestWaitingWithAnEmptyChannelSecretSetsTheChannelAsideAndFails(t *testing.T) {
	e, server := newStandInEndpoint(t)
	dir := t.TempDir()
	args := []string{"-journal", filepath.Join(dir, ".log"), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pushover", "-account", "boost", "-run-env", filepath.Join(dir, "run.env")}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL, "TI_PUSHOVER_TOKEN": "", "TI_PUSHOVER_USER": "", "TI_HEARTBEAT_URL": server.URL + heartbeatPath})
	err := run(args, environment, func() time.Time { return t0 }, &bytes.Buffer{}, server.Client())
	if err == nil || !strings.Contains(err.Error(), "pushover") {
		t.Fatalf("err = %v, want one naming the pushover channel", err)
	}
	if e.count("ping") != 0 {
		t.Errorf("a firing with a channel set aside pinged: %v", e.events)
	}
}

func TestWaitingWithAFailedTradingUnitSendsNoPing(t *testing.T) {
	e, server := newStandInEndpoint(t)
	systemd := newStandInSystemd(t)
	systemd.fail("ti-engine@boost.service")
	dir := t.TempDir()
	args := []string{"-journal", filepath.Join(dir, ".log"), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-account", "boost", "-run-env", filepath.Join(dir, "run.env"),
		"-units", "ti-engine@boost.service,ti-adapter-boost.service", "-systemctl", systemd.systemctl(), "-journalctl", systemd.journalctl()}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL, "TI_HEARTBEAT_URL": server.URL + heartbeatPath})
	if err := run(args, environment, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatal(err)
	}
	if e.count("ping") != 0 {
		t.Errorf("a waiting firing that found the engine failed pinged: %v", e.events)
	}
	if e.count("ntfy") == 0 {
		t.Errorf("the failed engine was not paged: %v", e.events)
	}
}

func TestWaitingRunsTheHostClockCheck(t *testing.T) {
	check := newStandInCheck(t)
	if _, err := waitingRun(t, filepath.Join(t.TempDir(), "run.env"), []string{"-clock-check", check.path()}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if check.runs() != 1 {
		t.Errorf("the clock check ran %d times, want 1", check.runs())
	}
}

func TestWaitingResolvesTheReportersEarlierFailurePage(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	systemd.fail()
	dir := t.TempDir()
	const self = "ti-reporter@boost.service"
	if err := notify(t, r, dir, systemd, twoChannels(), self); err != nil {
		t.Fatal(err)
	}
	args := []string{"-journal", filepath.Join(dir, ".log"), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty",
		"-account", "boost", "-run-env", filepath.Join(t.TempDir(), "run.env"), "-self-unit", self}
	if err := run(args, twoChannels(), func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, &http.Client{Transport: r}); err != nil {
		t.Fatal(err)
	}
	if resolves := count(r.to(pdHost), `"event_action":"resolve"`); resolves != 1 {
		t.Errorf("PagerDuty resolves = %d, want 1", resolves)
	}
}

// A run.env that is missing or empty proves no run only when nothing else
// shows one: a run log in the engine folder, or a delivery the reporter has
// made from a journal, means the settings file was lost, which is a fault.

func TestALostRunEnvWithARunLogIsAnAlertAndFailsWithNoPing(t *testing.T) {
	engine := t.TempDir()
	writeFileT(t, filepath.Join(engine, "r1.log"), "")
	var out bytes.Buffer
	e, err := waitingRun(t, filepath.Join(engine, "run.env"), nil, &out)
	if err == nil || !strings.Contains(err.Error(), "settings file is missing but a run exists") {
		t.Fatalf("err = %v, want the lost-settings fault", err)
	}
	if e.count("ping") != 0 {
		t.Errorf("pinged: %v", e.events)
	}
	if e.count("ntfy") == 0 {
		t.Errorf("no alert was sent: %v", e.events)
	}
	if strings.Contains(out.String(), "waiting") {
		t.Errorf("said it was waiting: %q", out.String())
	}
}

func TestAnEmptyRunEnvWithARunLogIsTheSameAlert(t *testing.T) {
	engine := t.TempDir()
	writeFileT(t, filepath.Join(engine, "r1.log"), "")
	writeFileT(t, filepath.Join(engine, "run.env"), "\n")
	e, err := waitingRun(t, filepath.Join(engine, "run.env"), nil, &bytes.Buffer{})
	if err == nil || e.count("ping") != 0 {
		t.Fatalf("err = %v, events %v", err, e.events)
	}
}

func TestADanglingRunEnvSymlinkIsRefusedNotAWait(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.env")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), path); err != nil {
		t.Fatal(err)
	}
	e, err := waitingRun(t, path, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "run.env") {
		t.Fatalf("a dangling run.env symlink was treated as absent: err = %v", err)
	}
	if e.count("ping") != 0 {
		t.Errorf("pinged: %v", e.events)
	}
}

func TestAnOversizedRunEnvIsRefusedNotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.env")
	writeFileT(t, path, strings.Repeat("A=1\n", 40000))
	e, err := waitingRun(t, path, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "run.env") || e.count("ping") != 0 {
		t.Fatalf("err = %v, events %v", err, e.events)
	}
}

func TestARunEnvThatIsNotARegularFileIsRefusedWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.env")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := waitingRun(t, path, nil, &bytes.Buffer{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO at run.env was accepted")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the firing blocked on a run.env that is a FIFO")
	}
}

// The run evidence is an explicit durable marker, <account>.ran beside the
// reporter's state file, written the first time a journal named by run.env is
// read, plus the logs in the engine folder (docs/manual/server.md, "The
// reporter before the first run"). A stray file or a state key proves nothing.

type runFiring struct {
	e       *standInEndpoint
	server  *httptest.Server
	reports string
	engine  string
	journal string
}

func newFiring(t *testing.T) *runFiring {
	t.Helper()
	e, server := newStandInEndpoint(t)
	return &runFiring{e: e, server: server, reports: t.TempDir(), engine: filepath.Join(t.TempDir(), "engine"), journal: writeJournal(t, t.TempDir())}
}

func (f *runFiring) fire(t *testing.T, out *bytes.Buffer) error {
	t.Helper()
	args := []string{"-journal", f.journal, "-state", filepath.Join(f.reports, "boost.reporter.json"), "-channel", "ntfy", "-account", "boost", "-run-env", filepath.Join(f.engine, "run.env")}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": f.server.URL, "TI_HEARTBEAT_URL": f.server.URL + heartbeatPath})
	return run(args, environment, func() time.Time { return t0.Add(time.Hour) }, out, f.server.Client())
}

func (f *runFiring) state(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.reports, "boost.reporter.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestACleanFirstReadWritesTheMarkerAndALaterLostRunEnvIsAnAlertNotAWait(t *testing.T) {
	f := newFiring(t)
	if err := os.MkdirAll(f.engine, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(f.engine, "run.env"), "TI_RUN=r1\n")
	if err := f.fire(t, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(f.reports, "boost.ran")
	info, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("no marker after the first journal read: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("marker mode %v, want the state file's 0600", info.Mode().Perm())
	}
	// The settings file is lost, and no log is left in the folder.
	if err := os.Remove(filepath.Join(f.engine, "run.env")); err != nil {
		t.Fatal(err)
	}
	f.e.events = nil
	var out bytes.Buffer
	err = f.fire(t, &out)
	if err == nil || !strings.Contains(err.Error(), "settings file is missing but a run exists") {
		t.Fatalf("err = %v, want the lost-settings fault", err)
	}
	if f.e.count("ping") != 0 || strings.Contains(out.String(), "waiting") {
		t.Errorf("pinged or waited: %v %q", f.e.events, out.String())
	}
	if !strings.Contains(f.state(t), "run-env-missing:boost:") {
		t.Errorf("the lost-settings alert was not delivered: %s", f.state(t))
	}
}

func TestAStrayLogLaterDeletedDoesNotLatchTheWait(t *testing.T) {
	f := newFiring(t)
	if err := os.MkdirAll(f.engine, 0o755); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(f.engine, "stray.log")
	writeFileT(t, stray, "")
	if err := f.fire(t, &bytes.Buffer{}); err == nil {
		t.Fatal("a log with no run.env should alert")
	}
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	f.e.events = nil
	var out bytes.Buffer
	if err := f.fire(t, &out); err != nil {
		t.Fatalf("the wait latched into a fault after the stray log was removed: %v", err)
	}
	if !strings.Contains(out.String(), "waiting for the account's first run") || f.e.count("ping") != 1 {
		t.Errorf("out %q events %v", out.String(), f.e.events)
	}
	if _, err := os.Stat(filepath.Join(f.reports, "boost.ran")); err == nil {
		t.Error("a marker was written without a journal read")
	}
}

func TestAnAbsentEngineFolderWithNoMarkerIsAWait(t *testing.T) {
	f := newFiring(t)
	var out bytes.Buffer
	if err := f.fire(t, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "waiting") || f.e.count("ping") != 1 {
		t.Errorf("out %q events %v", out.String(), f.e.events)
	}
}

func TestAnAbsentEngineFolderWithTheMarkerIsAnAlert(t *testing.T) {
	f := newFiring(t)
	writeFileT(t, filepath.Join(f.reports, "boost.ran"), "")
	err := f.fire(t, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "settings file is missing but a run exists") || f.e.count("ping") != 0 {
		t.Fatalf("err = %v events %v", err, f.e.events)
	}
}

func TestAnEngineFolderThatCannotBeListedIsAFaultNotAWait(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root lists anything")
	}
	f := newFiring(t)
	if err := os.MkdirAll(f.engine, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.engine, 0o311); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.engine, 0o755) })
	err := f.fire(t, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "cannot be listed") || f.e.count("ping") != 0 {
		t.Fatalf("err = %v events %v", err, f.e.events)
	}
}

func TestARunEnvWithoutAnAccountRefusesToStart(t *testing.T) {
	e, server := newStandInEndpoint(t)
	dir := t.TempDir()
	args := []string{"-journal", filepath.Join(dir, ".log"), "-state", filepath.Join(dir, "boost.reporter.json"), "-channel", "ntfy", "-run-env", filepath.Join(dir, "run.env")}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL, "TI_HEARTBEAT_URL": server.URL + heartbeatPath})
	err := run(args, environment, func() time.Time { return t0 }, &bytes.Buffer{}, server.Client())
	if err == nil || !strings.Contains(err.Error(), "-account") || !strings.Contains(err.Error(), "-run-env") {
		t.Fatalf("err = %v, want a refusal naming -account and -run-env", err)
	}
	if len(e.events) != 0 {
		t.Errorf("a refused start sent or pinged: %v", e.events)
	}
}

func TestARefusedJournalReadWritesNoMarker(t *testing.T) {
	f := newFiring(t)
	if err := os.MkdirAll(f.engine, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(f.engine, "run.env"), "TI_RUN=r1\n")
	writeFileT(t, f.journal, "this is not a journal\n")
	if err := f.fire(t, &bytes.Buffer{}); err == nil {
		t.Fatal("an unreadable journal should fail the firing")
	}
	if _, err := os.Stat(filepath.Join(f.reports, "boost.ran")); err == nil {
		t.Error("a marker was written although the journal read was refused")
	}
}

func TestAMarkerThatCannotBeWrittenFailsTheFiringAndIsRetriedNextFiring(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	f := newFiring(t)
	if err := os.MkdirAll(f.engine, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(f.engine, "run.env"), "TI_RUN=r1\n")
	if err := os.Chmod(f.reports, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.reports, 0o755) })
	err := f.fire(t, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "first-run marker") {
		t.Fatalf("err = %v, want the failed marker named", err)
	}
	if err := os.Chmod(f.reports, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.fire(t, &bytes.Buffer{}); err != nil {
		t.Fatalf("the next firing did not recover: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.reports, "boost.ran")); err != nil {
		t.Errorf("the marker was not retried: %v", err)
	}
}
