package report_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

var clockAt = time.Date(2026, time.October, 21, 14, 5, 0, 0, time.UTC)

const driftOutput = "clock: skew of -7s against https://www.cloudflare.com (this clock minus the reference), more than the 2s tolerance. The host's clock is wrong."

func TestClockDriftAlertIsOneUrgentIncidentWithTheMeasuredSkewAndTheRemedy(t *testing.T) {
	a := report.ClockDriftAlert("ti-server", report.ClockCheck{At: clockAt, Output: driftOutput}, 2)
	if a.Kind != report.KindClockDrift || a.Severity != report.Urgent || a.AccountID != "ti-server" {
		t.Fatalf("got %+v", a)
	}
	// ADR 0019, "Alerting": only a Degraded, Halted or reconciliation alert
	// repeats; a clock failure is told once per failure.
	if a.Repeat {
		t.Error("a clock-drift alert must not repeat")
	}
	for _, want := range []string{"-7s", "2026-10-21T14:05:00Z", "withholds every sell", "clock-check.sh", "automatic date and time"} {
		if !strings.Contains(a.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, a.Body)
		}
	}
	if !strings.Contains(a.Title, "ti-server") || !strings.Contains(strings.ToLower(a.Title), "clock") {
		t.Errorf("title %q", a.Title)
	}
}

func TestClockDriftAlertKeyIsTheHostsAndStableWhileTheFailureLasts(t *testing.T) {
	first := report.ClockDriftAlert("ti-server", report.ClockCheck{At: clockAt, Output: driftOutput}, 2)
	later := report.ClockDriftAlert("ti-server", report.ClockCheck{At: clockAt.Add(25 * time.Minute), Output: strings.Replace(driftOutput, "-7s", "-31s", 1)}, 2)
	if first.Key != later.Key {
		t.Errorf("one failure must be one key: %q vs %q", first.Key, later.Key)
	}
	// The clock is the host's: one fault is one incident, whichever account's
	// reporter saw it, and the caller clears it by the same key.
	if first.Key != "clock-drift:ti-server" || report.ClockDriftKey("ti-server") != first.Key {
		t.Errorf("key %q, want clock-drift:ti-server", first.Key)
	}
	if !strings.Contains(first.Title, "ti-server") {
		t.Errorf("title %q does not name the host", first.Title)
	}
	if other := report.ClockDriftAlert("other-server", report.ClockCheck{At: clockAt, Output: driftOutput}, 2); other.Key == first.Key {
		t.Error("two hosts must not share a key")
	}
}

func TestClockDriftAlertSaysWhenTheOutputCannotBeRead(t *testing.T) {
	for _, out := range []string{"clock: skew: -7s against https://x", "something else entirely", "clock: skew +3s"} {
		a := report.ClockDriftAlert("ti-server", report.ClockCheck{At: clockAt, Output: out}, 2)
		if !strings.Contains(a.Body, "No skew could be read") || strings.Contains(a.Body, "No skew was measured") {
			t.Errorf("%q: body:\n%s", out, a.Body)
		}
	}
}

func TestClockDriftAlertSaysWhenNoSkewWasMeasured(t *testing.T) {
	for _, out := range []string{
		"clock: not synchronised: timedatectl does not report NTPSynchronized=yes (got 'no').",
		"clock: no reference clock could be measured (tried: https://x). Check the network, then run this again.",
		"clock: synchronised by the system's NTP service; checking against a reference (TI_CLOCK_VERIFY=1)\nclock: no reference clock could be measured (tried: https://x).",
		"clock: TI_CLOCK_TOLERANCE_SECONDS must be whole seconds from 1 to 60, not 'x'",
		"the check could not be run: /x/clock-check.sh is missing",
		"the check timed out after 40s",
	} {
		a := report.ClockDriftAlert("ti-server", report.ClockCheck{At: clockAt, Output: out}, 2)
		if !strings.Contains(a.Body, "No skew was measured") || !strings.Contains(a.Body, strings.SplitN(out, "\n", 2)[0]) {
			t.Errorf("%q: body:\n%s", out, a.Body)
		}
	}
}

func TestClockSkewSecondsReadsTheCheckOutput(t *testing.T) {
	for out, want := range map[string]struct {
		skew int
		ok   bool
	}{
		driftOutput: {-7, true},
		"clock: ok, skew 1s against https://www.apple.com, within 2s": {1, true},
		"clock: ok, synchronised by the system's NTP service":         {0, false},
		"clock: skew of 99999999999999999999s against https://x":      {0, false}, // too large to be a reading
		"": {0, false},
	} {
		if got, ok := report.ClockSkewSeconds(out); got != want.skew || ok != want.ok {
			t.Errorf("%q: got %d, %v", out, got, ok)
		}
	}
}

func TestClockSummaryLineShowsTheLastCheckTimeAndResult(t *testing.T) {
	pass := report.ClockSummaryLine(report.ClockCheck{At: clockAt, Passed: true, Output: "clock: ok, skew 1s against https://www.apple.com, within 2s"})
	for _, want := range []string{"Clock check", "2026-10-21 14:05Z", "ok", "skew 1s"} {
		if !strings.Contains(pass, want) {
			t.Errorf("passing line lacks %q: %s", want, pass)
		}
	}
	fail := report.ClockSummaryLine(report.ClockCheck{At: clockAt, Output: driftOutput})
	for _, want := range []string{"Clock check", "2026-10-21 14:05Z", "FAILED", "-7s"} {
		if !strings.Contains(fail, want) {
			t.Errorf("failing line lacks %q: %s", want, fail)
		}
	}
	if strings.Contains(pass, "\n") || strings.Contains(fail, "\n") {
		t.Error("the line is one line")
	}
}

func TestClockDriftAlertStatesTheActualCountOfFailedChecks(t *testing.T) {
	for _, n := range []int{2, 3, 11} {
		a := report.ClockDriftAlert("ti-server", report.ClockCheck{At: clockAt, Output: driftOutput}, n)
		want := fmt.Sprintf("%d checks in a row have failed", n)
		if !strings.Contains(a.Body, want) {
			t.Errorf("failures %d: the body does not say %q: %s", n, want, a.Body)
		}
		if strings.Contains(a.Body, "second check") {
			t.Errorf("failures %d: the body hard-codes the second check: %s", n, a.Body)
		}
	}
}
