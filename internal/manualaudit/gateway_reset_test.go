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
	for _, want := range []string{"at most 4 uncounted reset refusals in any rolling 24 hours", "three counted refusals", "gateway-start: reset wait:", "waiting Nm until HH:MM ET (consumed reset allowances M of 4 in 24h)", "resumes the remaining wait", "interrupted by stop", "whole seconds", "ignoring invalid reset wait", "permanent safety stop"} {
		if !strings.Contains(manual, want) {
			t.Errorf("manual lacks %q", want)
		}
	}
}
