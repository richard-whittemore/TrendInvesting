package main

import (
	"context"
	"errors"
	"fmt"
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

// TestRecoveryIsSerializedAcrossGateways holds every healer to one recovery
// lock, whichever Gateway's state it keeps (server.md, Gateway healer): a
// trad-ira recovery that holds every relay's lock must keep Boost's healer
// from starting its adapter meanwhile.
func TestRecoveryIsSerializedAcrossGateways(t *testing.T) {
	root := t.TempDir()
	instance := filepath.Join(root, "trad-ira")
	for _, dir := range []string{root, instance} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	uid := uint32(os.Getuid())
	ira := diskState{dir: instance, lockDir: root, uid: uid}
	boost := diskState{dir: root, lockDir: root, uid: uid}
	unlock, err := ira.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := boost.Lock(); err == nil || !strings.Contains(err.Error(), "another healer") {
		t.Fatalf("Boost's healer ran during the IRA's recovery: %v", err)
	}
	unlock()
	release, err := boost.Lock()
	if err != nil {
		t.Fatalf("Boost's healer could not run after the IRA's recovery ended: %v", err)
	}
	release()
}

// TestALinkedStateDirectoryIsRefused keeps each Gateway's reservation its own
// (server.md, Gateway healer): with the lock shared, the state directory is
// still checked, so a link from the IRA's state to the joint's is refused.
func TestALinkedStateDirectoryIsRefused(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(root, "trad-ira")
	if err := os.Symlink(root, linked); err != nil {
		t.Fatal(err)
	}
	store := diskState{dir: linked, lockDir: root, uid: uint32(os.Getuid())}
	if unlock, err := store.Lock(); err == nil {
		unlock()
		t.Fatal("a linked state directory was accepted")
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
func TestEveryGatewayRecoveryLocksEveryRelay(t *testing.T) {
	for _, account := range []string{"boost", "trad-ira"} {
		t.Run(account, func(t *testing.T) {
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
			guard := relayGuard{root: root, account: account}
			for _, name := range []string{"boost", "trad-ira", "roth-ira"} {
				f, err := os.Open(filepath.Join(root, name, "pending.json.lock"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = f.Close() })
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
				release, err := guard.Acquire()
				if release != nil {
					release()
				}
				if !errors.Is(err, gatewayheal.ErrOrderBusy) {
					t.Fatalf("%s active: %v", name, err)
				}
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
					t.Fatal(err)
				}
				release, err = guard.Acquire()
				if err != nil {
					t.Fatal(err)
				}
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
					t.Errorf("%s not locked", name)
				}
				release()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
					t.Fatal(err)
				}
			}
		})
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
	if release, err := (relayGuard{root: root, account: "boost"}).Acquire(); !errors.Is(err, gatewayheal.ErrOrderBusy) {
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
	release, err := (relayGuard{root: root, account: "boost"}).Acquire()
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

// Command diagnostics identify the failed evidence or action without unbounded
// output (docs/manual/server.md, Gateway healer). Stand-ins only record calls.
func TestExternalCommandFailureIncludesBoundedStderr(t *testing.T) {
	for _, name := range []string{"systemctl", "journalctl"} {
		for _, diagnostic := range []string{"  permission denied  ", strings.Repeat("x", 5000)} {
			t.Run(name+"/"+diagnostic[:3], func(t *testing.T) {
				dir := t.TempDir()
				calls := filepath.Join(dir, "calls")
				script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >\"$HEAL_CALLS\"\nprintf '%s\\n' \"$HEAL_DIAGNOSTIC\" >&2\nexit 1\n"
				if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
				t.Setenv("HEAL_CALLS", calls)
				t.Setenv("HEAL_DIAGNOSTIC", diagnostic)
				args := []string{"show", gatewayheal.GatewayUnit}
				if name == "journalctl" {
					args = []string{"-u", gatewayheal.GatewayUnit}
				}
				err := executeCommand(context.Background(), name, args, io.Discard)
				want := name + " " + strings.Join(args, " ") + ": exit status 1: " + strings.TrimSpace(diagnostic[:min(len(diagnostic), 4096)])
				if err == nil || err.Error() != want {
					t.Fatalf("error=%v, want %q", err, want)
				}
				raw, readErr := os.ReadFile(calls)
				if readErr != nil || string(raw) != strings.Join(args, " ")+"\n" {
					t.Fatalf("calls=%q err=%v", raw, readErr)
				}
			})
		}
	}
}

// Synthetic files exercise the recovery paths in server.md, Gateway healer.
// Only the selected Gateway may receive a read, restart or durable reservation.
func TestHealerPathsAndOwnRelay(t *testing.T) {
	for _, tc := range []struct{ unit, name, home, state, env, gateway string }{
		{"ti-adapter-boost.service", "boost", "/var/lib/ti-gateway", "/var/lib/ti-gateway-heal", "/etc/trend-investing/gateway.env", "ti-gateway.service"},
		{"ti-adapter@trad-ira.service", "trad-ira", "/var/lib/ti-gateway-trad-ira", "/var/lib/ti-gateway-heal/trad-ira", "/etc/trend-investing/gateway-trad-ira.env", "ti-gateway@trad-ira.service"},
	} {
		for _, window := range []bool{false, true} {
			for _, bound := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/window=%v/bound=%v", tc.name, window, bound), func(t *testing.T) {
					paths, err := pathsForAdapter(tc.unit)
					if err != nil || paths.state != tc.state || paths.counter != tc.home+"/login-failures" || paths.env != tc.env || paths.account != tc.name {
						t.Fatalf("paths=%+v err=%v", paths, err)
					}
					root := t.TempDir()
					path := func(absolute string) string { return filepath.Join(root, strings.TrimPrefix(absolute, "/")) }
					put := func(name, content string) {
						t.Helper()
						if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					counter := ""
					if bound {
						counter = "1793631600\n1793631601\n1793631602\n"
					}
					put(path(paths.counter), counter)
					put(path(paths.env), "TI_GATEWAY_COLD_RESTART=09:00\n")
					schedule := path("/etc/trend-investing/ibkr-maintenance.json")
					raw := `{"windows":[]}`
					if window {
						raw = `{"windows":[{"days":["Mon"],"start":"15:00","end":"16:00","zone":"UTC"}]}`
					}
					put(schedule, raw)
					// Peer state is deliberately malformed: reading it must fail the run.
					peerCounter := "/var/lib/ti-gateway-trad-ira/login-failures"
					peerState := "/var/lib/ti-gateway-heal/trad-ira/last-restart.json"
					if tc.name != "boost" {
						peerCounter = "/var/lib/ti-gateway/login-failures"
						peerState = "/var/lib/ti-gateway-heal/last-restart.json"
					}
					put(path(peerCounter), "peer counter sentinel")
					put(path(peerState), "peer reservation sentinel")
					peerInfo, err := os.Stat(path(peerState))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(path(paths.state), 0o700); err != nil {
						t.Fatal(err)
					}
					for _, name := range []string{"boost", "trad-ira", "roth-ira"} {
						put(filepath.Join(root, "adapters", name, "pending.json.lock"), "")
					}
					scenario := &isolatedRecovery{t: t, unit: tc.unit, gateway: tc.gateway, at: time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC)}
					scenario.failed = gatewayheal.Status{Active: "failed", Code: 1, Exit: 3, Invocation: "failed-run", Finished: scenario.at.Add(-time.Minute)}
					scenario.current = gatewayheal.Status{Active: "active", Invocation: "old-login"}
					scenario.loss = scenario.at.Add(-10 * time.Minute)
					reader := files{schedule: schedule, counter: path(paths.counter), gatewayEnv: path(paths.env)}
					store := diskState{dir: path(paths.state), uid: uint32(os.Getuid())}
					guard := relayGuard{root: filepath.Join(root, "adapters"), account: paths.account}
					if err := gatewayheal.Run(tc.unit, scenario, reader, store, scenario, scenario, guard, func(string) error { return nil }); err != nil {
						t.Fatal(err)
					}
					want := []string{"restart " + tc.gateway, "start " + tc.unit}
					if bound {
						want = nil
					}
					if !reflect.DeepEqual(scenario.calls, want) {
						t.Fatalf("calls=%v want=%v", scenario.calls, want)
					}
					entries, err := os.ReadDir(store.dir)
					if err != nil {
						t.Fatal(err)
					}
					var names []string
					for _, entry := range entries {
						if entry.IsDir() {
							continue
						}
						names = append(names, entry.Name())
					}
					expected := []string{"last-restart.json", "lock"}
					if bound {
						expected = []string{"lock"}
					}
					if !reflect.DeepEqual(names, expected) {
						t.Fatalf("state files=%v want=%v", names, expected)
					}
					for name, wantRaw := range map[string]string{paths.counter: counter, peerCounter: "peer counter sentinel", peerState: "peer reservation sentinel"} {
						got, err := os.ReadFile(path(name))
						if err != nil || string(got) != wantRaw {
							t.Fatalf("state changed: %s", name)
						}
					}
					after, err := os.Stat(path(peerState))
					if err != nil || !after.ModTime().Equal(peerInfo.ModTime()) {
						t.Fatal("peer state rewritten")
					}
					if window && (len(scenario.waits) != 1 || scenario.waits[0] != time.Hour) {
						t.Fatalf("waits=%v", scenario.waits)
					}
				})
			}
		}
	}
	for _, unit := range []string{"", "ti-adapter@../boost.service", "ti-adapter@unknown.service", "ti-gateway.service"} {
		if _, err := pathsForAdapter(unit); err == nil {
			t.Fatalf("accepted %q", unit)
		}
	}
}

type isolatedRecovery struct {
	t               *testing.T
	unit, gateway   string
	at, loss        time.Time
	current, failed gatewayheal.Status
	calls           []string
	waits           []time.Duration
}

func (r *isolatedRecovery) Now() time.Time { return r.at }
func (r *isolatedRecovery) Wait(d time.Duration) error {
	r.waits = append(r.waits, d)
	r.at = r.at.Add(d)
	return nil
}
func (r *isolatedRecovery) Status(unit string) (gatewayheal.Status, error) {
	if unit == r.unit {
		return r.failed, nil
	}
	if unit != r.gateway {
		r.t.Fatalf("queried other Gateway %s", unit)
	}
	return r.current, nil
}
func (r *isolatedRecovery) Act(action, unit string) error {
	if (action == "restart" && unit != r.gateway) || (action == "start" && unit != r.unit) {
		r.t.Fatalf("cross-account action %s %s", action, unit)
	}
	r.calls = append(r.calls, action+" "+unit)
	if action == "restart" {
		r.current = gatewayheal.Status{Active: "active", Invocation: "new-login", Started: r.at}
	}
	return nil
}
func (r *isolatedRecovery) Lines(unit, _ string) ([]string, error) {
	if unit == r.unit {
		return []string{gatewayheal.Remedy}, nil
	}
	if unit != r.gateway {
		r.t.Fatalf("read other Gateway journal %s", unit)
	}
	return []string{"IBC: Login has completed"}, nil
}
func (r *isolatedRecovery) Loss(unit, _ string) (time.Time, error) {
	if unit != r.unit {
		r.t.Fatalf("read other adapter %s", unit)
	}
	return r.loss, nil
}

func TestRelayGuardRefusesMissingAccount(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "boost"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "boost", "pending.json.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := (relayGuard{root: root}).Acquire()
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("missing account silently acquired Boost's relay lock")
	}
}

func TestPreStartFailureReportsOtherExit(t *testing.T) {
	for _, unit := range []string{"ti-adapter-boost.service", "ti-adapter@trad-ira.service"} {
		t.Run(unit, func(t *testing.T) {
			calls := 0
			ctl := systemd{run: func(name string, args ...string) ([]byte, error) {
				calls++
				if name != "systemctl" || !containsArg(args, "show") {
					t.Fatalf("unexpected action: %s %v", name, args)
				}
				return []byte("ActiveState=failed\nExecMainCode=0\nExecMainStatus=0\nInvocationID=abcdef\nExecMainExitTimestamp=\nExecMainStartTimestamp=\n"), nil
			}}
			var lines []string
			err := gatewayheal.Run(unit, nil, nil, nil, ctl, ctl, nil, func(line string) error { lines = append(lines, line); return nil })
			if err != nil || calls != 1 || !reflect.DeepEqual(lines, []string{"gateway-heal: row=other-exit step=no-action"}) {
				t.Fatalf("err=%v calls=%d lines=%v", err, calls, lines)
			}
		})
	}
}
