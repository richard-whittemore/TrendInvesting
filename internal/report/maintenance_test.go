package report_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// A capital-safety alert is never held by a maintenance window, whatever else
// is held (ADR 0019, "Alerting").
func TestNoCapitalSafetyAlertIsHeldByAMaintenanceWindow(t *testing.T) {
	for _, k := range report.NeverHeld {
		if report.HeldDuringMaintenance(report.Alert{Kind: k}) {
			t.Errorf("%s is capital safety and is held by a window", k)
		}
	}
	for _, k := range []report.Kind{report.KindRefusal, report.KindHalted, report.KindDegraded, report.KindWithheld, report.KindUnacknowledged, report.KindSaleFailed, report.KindSaleByHand, report.KindMarginAlarm, report.KindReconciliation} {
		if report.HeldDuringMaintenance(report.Alert{Kind: k}) {
			t.Errorf("%s is held by a window", k)
		}
	}
}

func TestTheAlertsAWindowHoldsAreExactlyTheInstallsOwnNoise(t *testing.T) {
	for _, k := range []report.Kind{report.KindUnitFailed, report.KindGatewayLoggedOut} {
		if !report.HeldDuringMaintenance(report.Alert{Kind: k}) {
			t.Errorf("%s is not held by a window", k)
		}
	}
	for _, k := range []report.Kind{report.KindHeartbeat, report.KindJournalRefused, report.KindRunEnvMissing, report.KindMissingData, report.KindChannelRefused, report.KindHistoryUnavailable, "daily-summary"} {
		if report.HeldDuringMaintenance(report.Alert{Kind: k}) {
			t.Errorf("%s is held by a window: a window changes no other alert", k)
		}
	}
}

// No install causes a reconciliation, and a cash decrease is a money signal:
// no reconciliation is held, whether it moved the account's state or not.
func TestNoReconciliationIsHeldByAMaintenanceWindow(t *testing.T) {
	pending := newStream(t)
	pending.pendingRead(1, "reconciliation:1", -50000, "")
	mismatch := newStream(t)
	mismatch.mismatch(1, "reconciliation:1")
	for name, s := range map[string]*stream{"pending cash": pending, "mismatch": mismatch} {
		got, err := report.Alerts(s.out, "boost")
		if err != nil || len(got) == 0 {
			t.Fatalf("%s: %+v, %v", name, got, err)
		}
		for _, a := range got {
			if report.HeldDuringMaintenance(a) {
				t.Errorf("%s: %q is held by a window", name, a.Title)
			}
		}
	}
}
