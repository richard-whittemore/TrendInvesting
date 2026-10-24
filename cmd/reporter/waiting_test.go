package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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
	args := []string{"-journal", filepath.Join(dir, ".log"), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-run-env", runEnv}
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
	args := []string{"-journal", filepath.Join(dir, ".log"), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-run-env", filepath.Join(dir, "run.env")}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL})
	if err := run(args, environment, func() time.Time { return t0 }, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatal(err)
	}
	if e.count("ping") != 0 || e.count("ntfy") != 0 {
		t.Errorf("events %v", e.events)
	}
}

func TestWaitingReadsTheHeartbeatAddressFromTheSecretsFile(t *testing.T) {
	e, server := newStandInEndpoint(t)
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets")
	writeFileT(t, secrets, "TI_HEARTBEAT_URL="+server.URL+heartbeatPath+"\n")
	args := []string{"-journal", filepath.Join(dir, ".log"), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pushover", "-secrets-file", secrets, "-run-env", filepath.Join(dir, "run.env")}
	if err := run(args, env(nil), func() time.Time { return t0 }, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatal(err)
	}
	if e.count("ping") != 1 {
		t.Errorf("pings = %d, want 1", e.count("ping"))
	}
}

func TestWaitingStillFailsWhenTheSecretsFileCannotBeRead(t *testing.T) {
	e, server := newStandInEndpoint(t)
	dir := t.TempDir()
	args := []string{"-journal", filepath.Join(dir, ".log"), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-secrets-file", filepath.Join(dir, "missing"), "-run-env", filepath.Join(dir, "run.env")}
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
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-run-env", runEnv}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL, "TI_HEARTBEAT_URL": server.URL + heartbeatPath})
	if err := run(args, environment, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatal(err)
	}
	if e.count("ntfy") == 0 || e.count("ping") != 1 {
		t.Errorf("events %v", e.events)
	}
}
