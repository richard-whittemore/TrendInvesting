package targetweight_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// januaryDecision is the synthetic run's first rebalance decision.
const januaryDecision = "targets-decided:ira:2025-01-31T21:00:00Z"

// clearedAt is an operator.rebalance.cleared input for the IRA at
// session's close, naming decision and orders.
func clearedAt(t *testing.T, s *stream, id, session, account, decision string, orders []string) event.Envelope {
	t.Helper()
	return s.envelope(t, event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, id, closeOf(session),
		event.RebalanceClearedPayload{AccountID: account, AsOf: closeOf(session), DecisionID: decision, ClientOrderIDs: orders, Operator: "owner", Detail: "cancelled at IBKR"})
}

// januaryOrders returns the client order IDs January's rebalance released
// into a venue that never reports on them.
func januaryOrders(t *testing.T) []string {
	t.Helper()
	r := simulateMomentum(t, momentumOptions{silentVenue: true, stopAfter: "2025-01-31"})
	var ids []string
	for _, o := range released(t, r.decisions) {
		ids = append(ids, o.ClientOrderID)
	}
	if len(ids) == 0 {
		t.Fatal("setup: January's rebalance released nothing")
	}
	return ids
}

// TestTheOwnersClearanceEndsARebalanceInProgress: January's orders are
// released but the venue never reports on them, so February's decision is
// withheld (rebalance-in-progress) while they are not terminal; a rebalance
// in progress sends nothing more until the owner clears it (ADR 0026, point
// 5 and "A new rebalance is withheld while an earlier one's orders are
// working"). The owner's operator.rebalance.cleared, naming January's
// decision and every order of it not terminal, ends the hold, so February
// decides and plans afresh.
func TestTheOwnersClearanceEndsARebalanceInProgress(t *testing.T) {
	orders := januaryOrders(t)
	held := simulateMomentum(t, momentumOptions{silentVenue: true})
	if got := withheldReasons(t, held); !equalStrings(got, []string{"decided:", "withheld:rebalance-in-progress"}) {
		t.Fatalf("setup: decisions %v, want February held", got)
	}

	r := simulateMomentum(t, momentumOptions{silentVenue: true, beforeClose: func(s *stream, session string) []event.Envelope {
		if session != "2025-02-28" {
			return nil
		}
		return []event.Envelope{clearedAt(t, s, "cleared", session, iraAccount, januaryDecision, orders)}
	}})
	if got := withheldReasons(t, r); !equalStrings(got, []string{"decided:", "decided:"}) {
		t.Fatalf("decisions %v, want February decided once January is cleared", got)
	}
	var february int
	for _, o := range planned(t, r.decisions) {
		if o.Tag != januaryDecision {
			february++
		}
	}
	if february == 0 {
		t.Error("February planned no orders after the clearance")
	}
}

// TestAClearanceMustNameTheRebalanceInProgress: a clearance is refused, and
// the hold stands, unless it is for this account, names the rebalance in
// progress and every one of its orders not terminal, exactly; one with
// nothing in progress is refused too, so a clearance is never read as more
// than the owner stated.
func TestAClearanceMustNameTheRebalanceInProgress(t *testing.T) {
	orders := januaryOrders(t)
	r := simulateMomentum(t, momentumOptions{
		silentVenue: true,
		refusals: map[string]string{
			"early":         "no rebalance is in progress",
			"other account": "is for account",
			"other version": "schema version",
			"invalid":       "invalid rebalance cleared payload",
			"other":         "the rebalance in progress is " + januaryDecision,
			"short":         "name exactly",
		},
		beforeClose: func(s *stream, session string) []event.Envelope {
			switch session {
			case "2025-01-31":
				return []event.Envelope{clearedAt(t, s, "early", session, iraAccount, januaryDecision, orders)}
			case "2025-02-28":
				bad := clearedAt(t, s, "other version", session, iraAccount, januaryDecision, orders)
				bad.SchemaVersion = 2
				return []event.Envelope{
					clearedAt(t, s, "other account", session, "joint", januaryDecision, orders),
					bad,
					clearedAt(t, s, "invalid", session, iraAccount, januaryDecision, nil),
					clearedAt(t, s, "other", session, iraAccount, "targets-decided:ira:2024-12-31T21:00:00Z", orders),
					clearedAt(t, s, "short", session, iraAccount, januaryDecision, orders[:len(orders)-1]),
				}
			}
			return nil
		},
	})
	if got := withheldReasons(t, r); !equalStrings(got, []string{"decided:", "withheld:rebalance-in-progress"}) {
		t.Fatalf("decisions %v, want February still held after refused clearances", got)
	}
}

// TestBoostReadsNoClearance: Boost decides one target with one order and
// never holds a rebalance, so the clearance is not an input it reads.
func TestBoostReadsNoClearance(t *testing.T) {
	h, s := configured(t)
	mustRefuse(t, h, s.envelope(t, event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, "cleared", sessionEnd(0),
		event.RebalanceClearedPayload{AccountID: "joint", AsOf: sessionEnd(0), DecisionID: "d", ClientOrderIDs: []string{"o"}, Operator: "owner", Detail: "x"}),
		"not one this handler reads")
}
