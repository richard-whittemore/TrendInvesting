// Package procgroup is the repository's one way to end a whole process
// group: a command started in a group of its own (Setpgid), killed with
// every process it started when its time is up, so a child cannot keep it
// running. It refuses any process id that would make the signal reach more
// than that group: kill(2) with -1 signals every process the caller may
// signal, and 0 the caller's own group (docs/development.md, "Ending
// processes"). Nothing else in cmd/ or internal/ may call kill(2)
// (TestNoKillOutsideTheGuardedFunctions).
package procgroup

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

// target is the kill(2) target of pid's own process group: -pid, for a
// started process (pid > 1). Anything else is refused.
func target(pid int) (int, error) {
	if pid <= 1 {
		return 0, fmt.Errorf("procgroup: refused to signal the process group of pid %d: only a started command's own group is ended", pid)
	}
	return -pid, nil
}

// Kill ends the process group whose leader is pid with SIGKILL.
func Kill(pid int) error { return kill(pid, syscall.Kill) }

// kill is Kill with the signal sent through send.
func kill(pid int, send func(int, syscall.Signal) error) error {
	t, err := target(pid)
	if err != nil {
		return err
	}
	return send(t, syscall.SIGKILL)
}

// WaitGone polls only the started command's guarded process group until ESRCH,
// bounded by timeout (docs/development.md, "Ending processes"). The caller must
// reap its child concurrently; even a zombie must disappear before relaunch.
func WaitGone(pid int, timeout time.Duration) error { return waitGone(pid, timeout, syscall.Kill) }

func waitGone(pid int, timeout time.Duration, probe func(int, syscall.Signal) error) error {
	t, err := target(pid)
	if err != nil {
		return err
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		err := probe(t, 0)
		// kill(2) to a group fails with ESRCH when no process has that group
		// and with EPERM when none of the processes that have it may be
		// signalled by the caller. Every program here runs as one unprivileged
		// user under NoNewPrivileges (deploy/systemd/*.service), so nothing a
		// caller started can take on credentials the caller cannot signal:
		// EPERM can only mean the id now belongs to another session, and the
		// guarded group is gone (docs/development.md, "Ending processes").
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-deadline.C:
			return fmt.Errorf("procgroup: group %d still exists after %s", pid, timeout)
		case <-poll.C:
		}
	}
}
