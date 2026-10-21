package report

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// KindClockDrift is the server's clock failing its check: ADR 0019 treats a
// negative broker-snapshot age as stale with no tolerance, so a clock behind
// IBKR's withholds every sell. Only a Degraded, Halted or reconciliation alert
// repeats (ADR 0019, "Alerting"), so this one is told once per failure.
const KindClockDrift Kind = "clock-drift"

// ClockCheck is the outcome of one run of the clock check: when it ran,
// whether it passed, and what it said (its output, or why it could not run).
type ClockCheck struct {
	At     time.Time
	Passed bool
	Output string
}

// clockSkew reads the skew from the check's output. deploy/libexec/clock-check.sh
// documents the lines it prints (its "Output" contract): a measured skew is
// "skew <N>s" or "skew of <N>s", N a whole number of seconds with an optional
// minus sign.
var clockSkew = regexp.MustCompile(`\bskew (?:of )?(-?\d+)s\b`)

// clockUnmeasured matches the lines that say no skew was measured: the
// script's refusals before or without a comparison (its "Output" contract),
// and the reporter's own words for a check that could not run or timed out.
var clockUnmeasured = regexp.MustCompile(`(?m)^(?:clock: (?:not synchronised|no reference clock could be measured|TI_CLOCK_)|the clock stepped back|the check (?:could not be run|timed out|failed and said nothing))`)

// ClockSkewSeconds returns the skew in whole seconds that the check's output
// states (this clock minus the reference), and whether it states one: a clock
// that is not synchronised fails without measuring any.
func ClockSkewSeconds(output string) (int, bool) {
	m := clockSkew.FindStringSubmatch(output)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// ClockDriftKey is the incident key of a clock failure on host. The clock is
// the host's, not an account's, so every reporter on the host pages and
// clears under this one key.
func ClockDriftKey(host string) string { return "clock-drift:" + host }

// ClockDriftAlert reports a failed clock check on host, the failures-th in a row (the
// caller's count, not a fixed one, since a channel may take the page on a later failure): the measured skew (or
// that none was measured, or that the output could not be read) and the
// manual's remedy (docs/manual/server.md, "The clock"). The key names the
// host only, so one failure is one incident however many checks fail in a row
// and whichever account's reporter saw it; the caller resolves and forgets the
// key when a check passes, which makes the next failure a new incident.
func ClockDriftAlert(host string, c ClockCheck, failures int) Alert {
	skew := "No skew could be read from the check's output: it is not in a form clock-check.sh documents, so read what the check said below."
	if s, ok := ClockSkewSeconds(c.Output); ok {
		skew = fmt.Sprintf("Measured skew: %+ds (this clock minus the reference).", s)
	} else if clockUnmeasured.MatchString(c.Output) {
		skew = "No skew was measured: the check failed before it could compare the clock with a reference."
	}
	return Alert{
		Key: ClockDriftKey(host), Kind: KindClockDrift, Severity: Urgent, AccountID: host, At: c.At,
		Title: fmt.Sprintf("%s: the server's clock failed its check", host),
		Body: fmt.Sprintf("The clock check failed at %s, %d checks in a row have failed. %s The check said: %s While the clock is wrong every account's engine on this server counts IBKR's account reading as stale and withholds every sell (ADR 0019). "+
			"Remedy (docs/manual/server.md, \"The clock\"): run /usr/local/libexec/trend-investing/clock-check.sh on the server, then fix the time syncing (on the Mac mini, turn on the Mac's automatic date and time). You are told once for the whole server; the incident is resolved when a check passes, and the next failure after that is told again.",
			c.At.UTC().Format(time.RFC3339), failures, skew, strings.TrimSpace(c.Output)),
	}
}

// ClockSummaryLine is the daily summary's single line for the last clock
// check: when it ran and whether it passed, with what it said.
func ClockSummaryLine(c ClockCheck) string {
	result := "ok"
	if !c.Passed {
		result = "FAILED"
	}
	said := strings.Join(strings.Fields(c.Output), " ")
	return fmt.Sprintf("Clock check: %s at %s (%s)", result, c.At.UTC().Format("2006-01-02 15:04Z"), said)
}
