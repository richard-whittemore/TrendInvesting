package main

import (
	"errors"
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
		return []byte("ActiveState=failed\nExecMainCode=1\nExecMainStatus=3\nInvocationID=abcdef\nExecMainExitTimestamp=Mon 2026-11-02 14:59:00 UTC\n"), nil
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
	want := []string{"journalctl", "--no-pager", "-o", "cat", "-n", "2000", "_SYSTEMD_UNIT=ti-gateway.service", "_SYSTEMD_INVOCATION_ID=abcdef"}
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
	unlock, err := store.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err = store.lock(); err == nil {
		t.Fatal("parallel healer entered")
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
	r := files{schedule: filepath.Join(root, "schedule"), counter: filepath.Join(root, "counter")}
	if _, err := r.Read(); err == nil {
		t.Fatal("missing schedule accepted")
	}
	if err := os.WriteFile(r.schedule, []byte(`{"windows":[]}`), 0o600); err != nil {
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
	}
	if err := os.WriteFile(filepath.Join(root, "boost", "pending.json.handover"), []byte(`{"synthetic-order":"handing-over"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if release, err := (relayGuard{root: root}).Acquire(); err == nil {
		release()
		t.Fatal("unsettled hand-over allowed restart")
	}
}
