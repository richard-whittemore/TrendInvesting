package report_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

var clockAt = time.Date(2026, time.October, 21, 14, 5, 0, 0, time.UTC)

const driftOutput = "clock: skew of -7s against https://www.cloudflare.com (this clock minus the reference), more than the 2s tolerance. The host's clock is wrong."

func TestClockDriftAlertIsOneUrgentIncidentWithTheMeasuredSkewAndTheRemedy(t *testing.T) {
	a := report.ClockDriftAlert("boost", report.ClockCheck{At: clockAt, Output: driftOutput})
	if a.Kind != report.KindClockDrift || a.Severity != report.Urgent || a.AccountID != "boost" {
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
	if !strings.Contains(a.Title, "boost") || !strings.Contains(strings.ToLower(a.Title), "clock") {
		t.Errorf("title %q", a.Title)
	}
}

func TestClockDriftAlertKeyIsStableWhileTheFailureLasts(t *testing.T) {
	first := report.ClockDriftAlert("boost", report.ClockCheck{At: clockAt, Output: driftOutput})
	later := report.ClockDriftAlert("boost", report.ClockCheck{At: clockAt.Add(25 * time.Minute), Output: strings.Replace(driftOutput, "-7s", "-31s", 1)})
	if first.Key != later.Key {
		t.Errorf("one failure must be one key: %q vs %q", first.Key, later.Key)
	}
	if other := report.ClockDriftAlert("roth", report.ClockCheck{At: clockAt, Output: driftOutput}); other.Key == first.Key {
		t.Error("two accounts must not share a key")
	}
}

func TestClockDriftAlertSaysWhenNoSkewWasMeasured(t *testing.T) {
	out := "clock: not synchronised: timedatectl does not report NTPSynchronized=yes (got 'no')."
	a := report.ClockDriftAlert("boost", report.ClockCheck{At: clockAt, Output: out})
	if !strings.Contains(a.Body, "No skew was measured") || !strings.Contains(a.Body, "NTPSynchronized=yes") {
		t.Errorf("body:\n%s", a.Body)
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
