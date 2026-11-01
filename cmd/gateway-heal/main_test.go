package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/gatewayheal"
)

func TestSystemctlAndJournalAreInvocationScoped(t *testing.T) {
	var calls [][]string
	run := func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte("ActiveState=failed\nExecMainCode=1\nExecMainStatus=3\nInvocationID=abcdef\nExecMainExitTimestamp=Mon 2026-11-02 14:59:00 UTC\nExecMainStartTimestamp=Mon 2026-11-02 14:58:00 UTC\n"), nil
	}
	s := systemd{run: run}
	state, err := s.Status(gatewayheal.AdapterUnits[0])
	if err != nil || state.Exit != 3 || state.Code != 1 || state.Invocation != "abcdef" || state.Finished.IsZero() {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if err := s.Act("restart", gatewayheal.GatewayUnit); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Lines(gatewayheal.GatewayUnit, "abcdef"); err != nil {
		t.Fatal(err)
	}
	want := []string{"journalctl", "--no-pager", "-o", "cat", "_SYSTEMD_UNIT=ti-gateway.service", "_SYSTEMD_INVOCATION_ID=abcdef"}
	if !reflect.DeepEqual(calls[2], want) {
		t.Fatalf("journal=%v", calls[2])
	}
	if err = s.Act("restart", "unrelated.service"); err == nil || len(calls) != 3 {
		t.Fatal("unapproved operation reached runner")
	}
	if _, err = s.Lines(gatewayheal.GatewayUnit, ""); err == nil {
		t.Fatal("empty invocation accepted")
	}
}
func TestStateIsDurablePrivateAndSerialized(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := diskState{dir: dir, uid: uint32(os.Getuid())}
	unlock, err := store.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err = store.Lock(); err == nil || strings.Contains(err.Error(), "order") || !strings.Contains(err.Error(), "another healer") {
		t.Fatalf("parallel healer did not report its own collision: %v", err)
	}
	state := gatewayheal.State{LastRestart: time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC), Incident: "synthetic"}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil || got != state {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	info, err := os.Stat(filepath.Join(dir, "last-restart.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
	if err := os.WriteFile(filepath.Join(dir, "last-restart.json"), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Load(); err == nil {
		t.Fatal("broken state accepted")
	}
}
func TestRelayLocksExcludeAllAccountsAndRelease(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"boost", "trad-ira", "roth-ira"} {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pending.json.lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	guard := relayGuard{root: root}
	f, err := os.OpenFile(filepath.Join(root, "trad-ira", "pending.json.lock"), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, err = guard.Acquire(); err == nil {
		t.Fatal("active relay allowed restart")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	release, err := guard.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatal("healer did not exclude relay")
	}
	release()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal("relay lock leaked", err)
	}
}
func TestInputsRefuseMissingScheduleAndNonregularCounter(t *testing.T) {
	root := t.TempDir()
	r := files{schedule: filepath.Join(root, "schedule"), counter: filepath.Join(root, "counter"), gatewayEnv: filepath.Join(root, "gateway.env")}
	if _, err := r.Read(); err == nil {
		t.Fatal("missing schedule accepted")
	}
	if err := os.WriteFile(r.schedule, []byte(`{"windows":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.gatewayEnv, []byte("TI_GATEWAY_COLD_RESTART=09:00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(r.schedule, r.counter); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(); err == nil {
		t.Fatal("symlink counter accepted")
	}
}
func TestUnitProvisionBuildAndRehearsalWiring(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	unit := read("deploy/systemd/ti-gateway-heal@.service")
	for _, line := range []string{"Type=oneshot", "User=root", "UMask=0077", "ti-gateway-heal -adapter %i", "TimeoutStartSec=infinity"} {
		if !strings.Contains(unit, line) {
			t.Errorf("unit missing %s", line)
		}
	}
	for _, path := range []string{"deploy/systemd/ti-adapter-boost.service", "deploy/systemd/ti-adapter@.service"} {
		s := read(path)
		for _, hook := range []string{"OnFailure=ti-unit-failed@%n.service", "OnFailure=ti-gateway-heal@%n.service"} {
			if !strings.Contains(s, hook) {
				t.Errorf("%s missing %s", path, hook)
			}
		}
	}
	for _, path := range []string{"deploy/build-engine.sh", "deploy/rehearse.sh", "deploy/provision.sh"} {
		if !strings.Contains(read(path), "ti-gateway-heal") {
			t.Errorf("%s omits healer", path)
		}
	}
	if !strings.Contains(read("deploy/rehearsal/inside-systemd.sh"), "rehearse_gateway_heal") {
		t.Fatal("rehearsal not wired")
	}
}
func TestSystemctlReadFailuresAreNotSuccess(t *testing.T) {
	s := systemd{run: func(string, ...string) ([]byte, error) { return nil, errors.New("synthetic") }}
	if _, err := s.Status(gatewayheal.GatewayUnit); err == nil {
		t.Fatal("failed status accepted")
	}
	if _, err := s.Lines(gatewayheal.GatewayUnit, "abc"); err == nil {
		t.Fatal("failed journal accepted")
	}
}

func TestAbandonedHandoversStillPreventRestart(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"boost", "trad-ira", "roth-ira"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "pending.json.lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "boost", "pending.json.handover"), []byte(`{"synthetic-order":"handing-over"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if release, err := (relayGuard{root: root}).Acquire(); !errors.Is(err, gatewayheal.ErrOrderBusy) {
		if release != nil {
			release()
		}
		t.Fatal("unsettled hand-over was not classified as order-busy", err)
	}
}

func TestRestartStateRejectsAmbiguousFields(t *testing.T) {
	dir := t.TempDir()
	store := diskState{dir: dir, uid: uint32(os.Getuid())}
	for _, raw := range []string{
		`{"last_restart":"2026-11-02T15:00:00Z","last_restart":"2026-10-02T15:00:00Z","incident":"a"}`,
		`{"last_restart":"2026-11-02T15:00:00Z","incident":"a","incident":"b"}`,
		`{"LAST_RESTART":"2026-11-02T15:00:00Z","incident":"a"}`,
		`{"last_restart":"2026-11-02T15:00:00Z","Incident":"a"}`,
		`{"last_restart":"2026-11-02T15:00:00Z","incident":"a","unknown":1}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, "last-restart.json"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); err == nil {
			t.Errorf("accepted ambiguous state: %s", raw)
		}
	}
}
func TestMissingRelayLockIsNeverCreated(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"boost", "trad-ira", "roth-ira"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	release, err := (relayGuard{root: root}).Acquire()
	if release != nil {
		release()
	}
	if err == nil {
		t.Error("missing relay locks accepted")
	}
	for _, name := range []string{"boost", "trad-ira", "roth-ira"} {
		if _, err := os.Stat(filepath.Join(root, name, "pending.json.lock")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("created relay lock for %s", name)
		}
	}
}

func TestExternalCommandsHaveDeadlineAndBoundedDrain(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"context.WithTimeout", "exec.CommandContext", "cmd.WaitDelay"} {
		if !strings.Contains(string(raw), required) {
			t.Errorf("command bound missing %s", required)
		}
	}
}

func TestCommandDeadlineCapsRecoveryAndUnwinds(t *testing.T) {
	now := time.Date(2026, 11, 2, 17, 0, 0, 0, time.UTC)
	r := commandRunner{now: func() time.Time { return now }, deadline: now.Add(5 * time.Second)}
	calls := 0
	r.execute = func(ctx context.Context, _ string, _ []string, _ io.Writer) error {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("missing remaining recovery bound")
		}
		return context.DeadlineExceeded
	}
	if _, err := r.run("systemctl", "show"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Second)
	if _, err := r.run("systemctl", "show"); !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatal("expired deadline ran a command", err, calls)
	}
}
func TestInvocationStreamRetainsLoginBeyondOrdinaryNoise(t *testing.T) {
	r := commandRunner{now: time.Now, execute: func(_ context.Context, _ string, _ []string, out io.Writer) error {
		_, err := io.WriteString(out, "IBC: Login has completed\n"+strings.Repeat("ordinary message\n", 3000))
		return err
	}}
	raw, err := r.run("journalctl", "-o", "cat")
	if err != nil || !gatewayheal.LoggedIn(strings.Split(string(raw), "\n")) {
		t.Fatal("login lost behind noise", err)
	}
	if len(raw) > 100 {
		t.Fatal("journal not reduced with bounded memory", len(raw))
	}
}

func TestLossUsesCurrentInvocationAndFirstUnrestoredLoss(t *testing.T) {
	s := systemd{run: func(name string, args ...string) ([]byte, error) {
		query := strings.Join(args, " ")
		if name != "journalctl" || !strings.Contains(query, "_SYSTEMD_INVOCATION_ID=failed-id") || strings.Contains(query, "-n ") {
			t.Fatal(query)
		}
		return []byte("2026-11-02T15:00:00+00:00 host adapter: ibkr link state=connectivity-lost\n" +
			"2026-11-02T15:01:00+00:00 host adapter: ibkr link state=connected reason=restored\n" +
			"2026-11-02T15:02:00+00:00 host adapter: ibkr link state=connectivity-lost\n" +
			"2026-11-02T15:03:00+00:00 host adapter: ibkr link state=connectivity-lost\n"), nil
	}}
	got, err := s.Loss(gatewayheal.AdapterUnits[0], "failed-id")
	if err != nil || !got.Equal(time.Date(2026, 11, 2, 15, 2, 0, 0, time.UTC)) {
		t.Fatal(got, err)
	}
}
func TestCommandStreamRefusesOversizedLineAndRetainsLogout(t *testing.T) {
	for _, content := range []string{"IBC: Login has completed\n" + strings.Repeat("noise\n", 3000) + "IBC: LoginState is LOGGED_OUT", strings.Repeat("x", 1024*1024+1)} {
		r := commandRunner{now: time.Now, execute: func(_ context.Context, _ string, _ []string, out io.Writer) error {
			_, err := io.Copy(out, strings.NewReader(content))
			return err
		}}
		raw, err := r.run("journalctl", "-o", "cat")
		if len(content) > 1024*1024 {
			if err == nil {
				t.Fatal("oversized line accepted")
			}
			continue
		}
		if err != nil || gatewayheal.LoggedIn(strings.Split(string(raw), "\n")) {
			t.Fatal("logout lost", err)
		}
	}
}
func TestGatewayEnvironmentIsRequired(t *testing.T) {
	dir := t.TempDir()
	r := files{schedule: filepath.Join(dir, "schedule"), counter: filepath.Join(dir, "counter"), gatewayEnv: filepath.Join(dir, "env")}
	if err := os.WriteFile(r.schedule, []byte(`{"windows":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(); err == nil {
		t.Fatal("missing Gateway environment accepted")
	}
}

func TestSystemdTimestampsPreserveFailureAfterLossWithinSecond(t *testing.T) {
	s := systemd{run: func(_ string, args ...string) ([]byte, error) {
		if !containsArg(args, "--timestamp=us") {
			t.Error("systemctl timestamp precision not requested")
		}
		return []byte("ActiveState=failed\nExecMainCode=1\nExecMainStatus=3\nInvocationID=abcdef\nExecMainExitTimestamp=Mon 2026-11-02 14:59:00.654321 UTC\nExecMainStartTimestamp=Mon 2026-11-02 14:58:00.123456 UTC\n"), nil
	}}
	got, err := s.Status(gatewayheal.AdapterUnits[0])
	if err != nil || got.Finished.Nanosecond() != 654321000 || got.Started.Nanosecond() != 123456000 {
		t.Fatal(got, err)
	}
}

// A unit that has run reports when it started; a missing start time is
// incomplete evidence, never time zero (server.md, Gateway healer).
func TestSystemdStatusRefusesARunWithoutItsStartTime(t *testing.T) {
	s := systemd{run: func(_ string, _ ...string) ([]byte, error) {
		return []byte("ActiveState=active\nExecMainCode=0\nExecMainStatus=0\nInvocationID=abcdef\nExecMainExitTimestamp=\nExecMainStartTimestamp=\n"), nil
	}}
	if got, err := s.Status("ti-gateway.service"); err == nil {
		t.Fatalf("a run without its start time was accepted: %+v", got)
	}
}

// Submitted jobs keep exclusion until systemctl returns (server.md, Gateway healer).
func TestGatewayMutationWaitsForManagerWithoutClientDeadline(t *testing.T) {
	for _, action := range []string{"restart", "start"} {
		t.Run(action, func(t *testing.T) {
			now := time.Now()
			r := commandRunner{now: func() time.Time { return now }, deadline: now.Add(time.Second)}
			r.execute = func(ctx context.Context, name string, args []string, _ io.Writer) error {
				if _, bounded := ctx.Deadline(); bounded {
					t.Error("submitted systemd mutation has a client deadline")
				}
				if !containsArg(args, "--job-mode=fail") {
					t.Error("mutation can replace an existing systemd job")
				}
				now = now.Add(time.Hour)
				return nil
			}
			unit := gatewayheal.GatewayUnit
			if action == "start" {
				unit = gatewayheal.AdapterUnits[0]
			}
			s := systemd{run: r.run}
			if err := s.Act(action, unit); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The harmless client stand-in records completion after the policy deadline;
// it never signals a process (server.md, Gateway healer).
func TestGatewayMutationClientReturnsAfterPolicyDeadline(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >\"$HEAL_CALLS\"\nsleep 0.1\nprintf 'returned\\n' >>\"$HEAL_CALLS\"\n"
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("HEAL_CALLS", calls)
	runner := commandRunner{now: time.Now, deadline: time.Now().Add(50 * time.Millisecond), execute: executeCommand}
	ctl := systemd{run: runner.run}
	if err := ctl.Act("restart", gatewayheal.GatewayUnit); err != nil {
		t.Fatal("abandoned submitted mutation", err)
	}
	raw, err := os.ReadFile(calls)
	if err != nil || string(raw) != "--job-mode=fail restart ti-gateway.service\nreturned\n" {
		t.Fatalf("client did not return: %q %v", raw, err)
	}
	if time.Now().Before(runner.deadline) {
		t.Fatal("stand-in did not outlive recovery deadline")
	}
}

// The healer's submitted jobs are bounded by systemd, not by a client timeout:
// each target unit keeps its stop bound, and no unit overrides systemd's
// default start timeout to unbounded (docs/manual/server.md, Gateway healer).
func TestGatewayUnitMutationTimeoutsAreOwnedBySystemd(t *testing.T) {
	for _, unit := range []string{"ti-gateway.service", "ti-adapter-boost.service", "ti-adapter@.service"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "systemd", unit))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "TimeoutStopSec=60") {
			t.Errorf("%s missing its stop bound TimeoutStopSec=60", unit)
		}
		for _, unbounded := range []string{"TimeoutStartSec=infinity", "TimeoutStartSec=0", "TimeoutSec=infinity", "TimeoutSec=0"} {
			if strings.Contains(string(raw), unbounded) {
				t.Errorf("%s makes its start unbounded (%s)", unit, unbounded)
			}
		}
	}
}
