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
	"fmt"
	"syscall"
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
