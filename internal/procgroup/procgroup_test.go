package procgroup

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestOnlyAStartedCommandsOwnGroupIsATarget: a pid that is not a started
// command's (0, 1, or below) is refused before kill(2) is reached, so the
// signal can never reach every process of the user (-1) or the caller's own
// group (0). No signal is sent here: only the target is computed.
func TestOnlyAStartedCommandsOwnGroupIsATarget(t *testing.T) {
	for _, pid := range []int{1, 0, -1, -4242} {
		if got, err := target(pid); err == nil {
			t.Errorf("target(%d) = %d, want a refusal", pid, got)
		}
	}
	if got, err := target(4242); err != nil || got != -4242 {
		t.Errorf("target(4242) = %d, %v; want -4242", got, err)
	}
}

// TestKillSendsToTheGroupOnlyAndNeverForARefusedPid: a refused pid sends no
// signal at all; a started command's pid sends SIGKILL to its group, -pid.
// The signal goes to a recorder: nothing is sent here.
func TestKillSendsToTheGroupOnlyAndNeverForARefusedPid(t *testing.T) {
	var sent []int
	record := func(target int, sig syscall.Signal) error {
		if sig != syscall.SIGKILL {
			t.Errorf("signal %v", sig)
		}
		sent = append(sent, target)
		return nil
	}
	for _, pid := range []int{1, 0, -1} {
		if err := kill(pid, record); err == nil {
			t.Errorf("kill(%d) was not refused", pid)
		}
	}
	if err := kill(4242, record); err != nil || len(sent) != 1 || sent[0] != -4242 {
		t.Fatalf("sent %v, err %v; want only -4242", sent, err)
	}
}

// TestKillEndsAStartedCommandsGroup: a command started in a group of its
// own, with a child, is ended whole by Kill.
func TestKillEndsAStartedCommandsGroup(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 30 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if cmd.Process.Pid <= 1 {
		t.Fatalf("pid %d", cmd.Process.Pid)
	}
	if err := Kill(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the command was not killed")
		}
		if err := WaitGone(cmd.Process.Pid, time.Second); err != nil {
			t.Fatalf("group survived cleanup: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the command outlived Kill")
	}
}

// Waits use only a guarded group probe (docs/development.md, "Ending processes").
func TestWaitGonePollsUntilAbsentAndBoundsAStuckGroup(t *testing.T) {
	for _, pid := range []int{0, 1, -1} {
		if err := waitGone(pid, time.Millisecond, func(int, syscall.Signal) error { t.Fatal("unsafe probe"); return nil }); err == nil {
			t.Errorf("pid %d accepted", pid)
		}
	}
	probes := 0
	err := waitGone(4242, time.Second, func(target int, sig syscall.Signal) error {
		if target != -4242 || sig != 0 {
			t.Fatalf("probe target %d signal %d", target, sig)
		}
		probes++
		if probes < 3 {
			return nil
		}
		return syscall.ESRCH
	})
	if err != nil || probes != 3 {
		t.Fatalf("probes=%d err=%v", probes, err)
	}
	if err := waitGone(4242, time.Millisecond, func(int, syscall.Signal) error { return nil }); err == nil {
		t.Fatal("live group passed the deadline")
	}
	if err := waitGone(4242, time.Second, func(int, syscall.Signal) error { return syscall.EPERM }); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("probe error=%v", err)
	}
}
