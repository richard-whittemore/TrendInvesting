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

var clockSkew = regexp.MustCompile(`\bskew (?:of )?(-?\d+)s\b`)

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

// ClockDriftAlert reports a failed clock check: the measured skew (or that
// none was measured) and the manual's remedy (docs/manual/server.md, "The
// clock"). The key names the account only, so one failure is one incident
// however many checks fail in a row; the caller forgets the key when a check
// passes, which makes the next failure a new incident.
func ClockDriftAlert(account string, c ClockCheck) Alert {
	skew := "No skew was measured: the check failed before it could compare the clock with a reference."
	if s, ok := ClockSkewSeconds(c.Output); ok {
		skew = fmt.Sprintf("Measured skew: %+ds (this clock minus the reference).", s)
	}
	return Alert{
		Key: "clock-drift:" + account, Kind: KindClockDrift, Severity: Urgent, AccountID: account, At: c.At,
		Title: fmt.Sprintf("%s: the server's clock failed its check", account),
		Body: fmt.Sprintf("The clock check failed at %s. %s The check said: %s While the clock is wrong the engine counts IBKR's account reading as stale and withholds every sell (ADR 0019). "+
			"Remedy (docs/manual/server.md, \"The clock\"): run /usr/local/libexec/trend-investing/clock-check.sh on the server, then fix the time syncing (on the Mac mini, turn on the Mac's automatic date and time). You are told once; the next failure after a passing check is told again.",
			c.At.UTC().Format(time.RFC3339), skew, strings.TrimSpace(c.Output)),
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
