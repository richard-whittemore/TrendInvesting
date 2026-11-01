package manualaudit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayResetManualStatesDurableBoundAndActualWait(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "manual", "server.md"))
	if err != nil {
		t.Fatal(err)
	}
	manual := strings.Join(strings.Fields(string(raw)), " ")
	for _, obsolete := range []string{"uncounted reset waits up to 4 per night", "waiting 15 minutes, not counted", "Reset refusals do not use up the bound"} {
		if strings.Contains(manual, obsolete) {
			t.Errorf("obsolete reset promise: %s", obsolete)
		}
	}
	for _, want := range []string{"at most 4 uncounted reset refusals in any rolling 24 hours", "three counted refusals", "gateway-start: reset wait:", "waiting Nm until HH:MM ET (consumed reset allowances M of 4 in 24h)", "resumes the remaining wait", "interrupted by stop", "whole seconds", "two minutes ahead of the clock", "exit 77 and leave the file", "permanent safety stop"} {
		if !strings.Contains(manual, want) {
			t.Errorf("manual lacks %q", want)
		}
	}
}

func TestPagingManualWindowsAndReminders(t *testing.T) {
	for file, wants := range map[string][]string{
		"server.md":               {"daily 23:40–01:00", "Saturday 00:00–24:00", "Sunday 00:00–14:00", "Friday 23:40 to Sunday 14:00"},
		"alerts-and-dashboard.md": {"never shorter than the existing hourly interval", "receipt cancel API", "same night's", "three continuous hours", "timer-job failures do not repeat", "ten-minute"},
	} {
		raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "manual", file))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(string(raw)), " ")
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", file, want)
			}
		}
	}
}
