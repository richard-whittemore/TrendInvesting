package report

// The maintenance window (internal/maintenance) holds back the pages that an
// install makes for nothing, and only these: the alerts of a unit that
// failed (the data job's included), of a Gateway logged out, and of a
// reconciliation that did not match. They are recorded and not delivered,
// then judged again at the firing after the window closes, so nothing held is
// forgotten (ADR 0019, "Alerting": a fault that persists is not left silent).
// Every other alert is delivered as it always is.

// NeverHeld are the capital-safety alerts: a window never holds one, and no
// edit to the held list may add one. An order refused (the kill switch is a
// refusal), a Halted or Degraded account, a decision withheld, an order the
// venue did not acknowledge, a sale that failed or must be made by hand, and
// a margin alarm all concern the money and wait for no one (AGENTS.md, rule
// 5; ADR 0019, "Alerting"; docs/manual/alerts-and-dashboard.md,
// "Maintenance windows").
var NeverHeld = []Kind{
	KindRefusal, KindHalted, KindDegraded, KindWithheld, KindUnacknowledged,
	KindSaleFailed, KindSaleByHand, KindMarginAlarm,
}

// heldDuringMaintenance are the kinds a window holds.
var heldDuringMaintenance = []Kind{KindUnitFailed, KindGatewayLoggedOut, KindReconciliation}

// HeldDuringMaintenance reports whether a window holds alert a: only the
// three kinds above, never one that NeverHeld names, and a reconciliation
// only when it moved no account state (Alert.StateUnchanged). A reconciliation
// that Degrades or Halts the account is a Degraded or Halted alert in all but
// name, and is capital safety.
func HeldDuringMaintenance(a Alert) bool {
	for _, never := range NeverHeld {
		if a.Kind == never {
			return false
		}
	}
	if a.Kind == KindReconciliation && !a.StateUnchanged {
		return false
	}
	for _, held := range heldDuringMaintenance {
		if a.Kind == held {
			return true
		}
	}
	return false
}
