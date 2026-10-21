package main

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// clockCheckTimeout bounds one run of the clock check. The check tries three
// references for at most 5 seconds each; this is well inside the unit's
// TimeoutStartSec, so a hung check fails as a failed check, not as a run
// systemd stops.
const clockCheckTimeout = 90 * time.Second

// maxClockOutput is the most of the check's output kept for an alert.
const maxClockOutput = 1000

// runClockCheck runs the clock-check command (deploy/libexec/clock-check.sh:
// exit 0 is a good clock, anything else is not) and returns what it said. A
// command that cannot be started or is stopped at the timeout is a failed
// check: an unchecked clock is not a checked one.
func runClockCheck(command string, now time.Time) report.ClockCheck {
	ctx, cancel := context.WithTimeout(context.Background(), clockCheckTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, command).CombinedOutput()
	said := strings.TrimSpace(string(out))
	if len(said) > maxClockOutput {
		said = said[:maxClockOutput]
	}
	c := report.ClockCheck{At: now, Passed: err == nil, Output: said}
	if err != nil && said == "" {
		c.Output = "the check could not be run: " + err.Error()
	}
	return c
}

// forgetAlert removes every channel's record of the alert keyed key, so the
// incident is told afresh the next time it arises.
func forgetAlert(key string, seen map[string]bool, sent map[string]time.Time, repeats map[string]int) {
	suffix := channelKey("", key)
	for k := range seen {
		if strings.HasSuffix(k, suffix) {
			delete(seen, k)
		}
	}
	for k := range sent {
		if strings.HasSuffix(k, suffix) {
			delete(sent, k)
		}
	}
	for k := range repeats {
		if strings.HasSuffix(k, suffix) {
			delete(repeats, k)
		}
	}
}
