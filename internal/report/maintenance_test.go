package report_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// A capital-safety alert is never held by a maintenance window, whatever else
// is held (ADR 0019, "Alerting").
func TestNoCapitalSafetyAlertIsHeldByAMaintenanceWindow(t *testing.T) {
	for _, k := range report.NeverHeld {
		if report.HeldDuringMaintenance(report.Alert{Kind: k, StateUnchanged: true}) {
			t.Errorf("%s is capital safety and is held by a window", k)
		}
	}
	for _, k := range []report.Kind{report.KindRefusal, report.KindHalted, report.KindDegraded, report.KindWithheld, report.KindUnacknowledged, report.KindSaleFailed, report.KindSaleByHand, report.KindMarginAlarm} {
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

// A reconciliation that Degrades or Halts the account is a Degraded or Halted
// alert in all but name, and is never held; one that moved no state (a cash
// read the Flex reader has not covered) is.
func TestAReconciliationIsHeldOnlyWhenItMovedNoAccountState(t *testing.T) {
	if !report.HeldDuringMaintenance(report.Alert{Kind: report.KindReconciliation, StateUnchanged: true}) {
		t.Error("a reconciliation that moved no state is not held")
	}
	if report.HeldDuringMaintenance(report.Alert{Kind: report.KindReconciliation}) {
		t.Error("a reconciliation that Degrades or Halts the account is held")
	}
}

func TestOnlyAPendingCashReadMovesNoState(t *testing.T) {
	pending := newStream(t)
	pending.pendingRead(1, "reconciliation:1", 50000, "")
	got, err := report.Alerts(pending.out)
	if err != nil || len(got) != 1 || !got[0].StateUnchanged || !report.HeldDuringMaintenance(got[0]) {
		t.Fatalf("a pending cash read: %+v, %v; want one alert marked as moving no state, held by a window", got, err)
	}
	mismatch := newStream(t)
	mismatch.mismatch(1, "reconciliation:1")
	got, err = report.Alerts(mismatch.out)
	if err != nil || len(got) == 0 {
		t.Fatalf("a mismatch: %+v, %v", got, err)
	}
	for _, a := range got {
		if a.StateUnchanged || report.HeldDuringMaintenance(a) {
			t.Errorf("an alert of a mismatch that Degrades the account is held by a window: %s", a.Title)
		}
	}
}
