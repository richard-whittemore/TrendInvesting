package report

import (
	"strings"
	"testing"
	"time"
)

func TestInstanceGatewayFailureUsesGatewayRemedyAndSessionSafetyClass(t *testing.T) {
	a := UnitFailedAlert("synthetic", "ti-gateway@trad-ira.service", "synthetic-run", "synthetic failure", time.Time{})
	if a.Class(true) != CapitalSafety || a.Class(false) != Operational {
		t.Errorf("wrong paging classes: %v / %v", a.Class(true), a.Class(false))
	}
	for _, s := range []string{"systemd starts the Gateway again after most stops", `"The Traditional IRA's Gateway"`} {
		if !strings.Contains(a.Body, s) {
			t.Errorf("missing %s: %s", s, a.Body)
		}
	}
}
