package report_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// TestAnIRADataJobIsWordedAsTheDataJob: an IRA's data job instance
// (ti-data@<instance>.service) is run again by its timer like Boost's, so its
// page says so and gives the data job's manual row (ADR 0019, "Alerting").
func TestAnIRADataJobIsWordedAsTheDataJob(t *testing.T) {
	at := time.Date(2026, 10, 22, 15, 4, 0, 0, time.UTC)
	page := report.UnitFailedAlert("lightsail", "ti-data@trad-ira.service", "0123456789abcdef0123456789abcdef", "refused", at)
	for _, text := range []string{page.Body, page.PublicBody} {
		if strings.Contains(text, "does not restart") || !strings.Contains(text, "timer") {
			t.Errorf("the IRA data job's page does not say its timer runs it again:\n%s", text)
		}
	}
	if row := report.UnitManualRow("ti-data@trad-ira.service"); !strings.Contains(row, "The data job fails") {
		t.Errorf("the IRA data job's manual row is %q", row)
	}
	a, _ := report.WithFailedUnits(report.Alert{AccountID: "trad-ira"}, false, []string{"ti-data@trad-ira.service"})
	if strings.Contains(a.Body, "which systemd does not restart: ti-data") {
		t.Errorf("the state-at-start line words the IRA data job as not restarted:\n%s", a.Body)
	}
}

// TestMemorySampleIsWordedAsATimerJob preserves the retry and episode wording
// for measurement failures (lessons-2026-10.md L1/L8; ADR 0019, "Alerting").
func TestMemorySampleIsWordedAsATimerJob(t *testing.T) {
	page := report.UnitFailedAlert("lightsail", "ti-memory.service", "0123456789abcdef0123456789abcdef", "refused", time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC))
	for _, text := range []string{page.Body, page.PublicBody} {
		for _, want := range []string{"timer at the next firing", "The memory sample fails", "resolved when a run ends well"} {
			if !strings.Contains(text, want) {
				t.Errorf("missing %q in %s", want, text)
			}
		}
	}
}
