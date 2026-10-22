package targetweight_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// retiredAt is the adapter's execution.order.retired for the released order
// o, at session's close: an order the adapter never sent, its session over.
func retiredAt(t *testing.T, s *stream, id, session string, o event.OrderReleasedPayload) event.Envelope {
	t.Helper()
	return s.envelope(t, event.OrderRetiredEventType, event.OrderRetiredSchemaVersion, id, closeOf(session),
		event.OrderRetiredPayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, InstrumentID: o.InstrumentID, Side: o.Side,
			Quantity: o.Quantity, LimitPrice: o.LimitPrice, Reason: event.OrderRetiredSessionOver, Detail: "released for an earlier session",
			RetiredBy: event.OrderRetiredByAdapter, RetiredAt: closeOf(session)})
}

// januaryReleases returns the orders January's rebalance released into a
// venue that never reports on them.
func januaryReleases(t *testing.T) []event.OrderReleasedPayload {
	t.Helper()
	all := released(t, simulateMomentum(t, momentumOptions{silentVenue: true, stopAfter: "2025-01-31"}).decisions)
	if len(all) == 0 {
		t.Fatal("setup: January's rebalance released nothing")
	}
	return all
}

// TestRetiredOrdersEndARebalanceInProgress: January's orders are released
// but never sent, so February's decision would be withheld
// (rebalance-in-progress) while they are not terminal. The adapter retires
// each as an order whose session is over (execution.order.retired): no
// venue received it, so it is terminal with nothing filled, the rebalance
// ends, and February decides and plans afresh (the owner's decision of
// 2026-10-22; ADR 0026, point 5).
func TestRetiredOrdersEndARebalanceInProgress(t *testing.T) {
	orders := januaryReleases(t)
	r := simulateMomentum(t, momentumOptions{silentVenue: true, beforeClose: func(s *stream, session string) []event.Envelope {
		if session != "2025-02-28" {
			return nil
		}
		var out []event.Envelope
		for _, o := range orders {
			out = append(out, retiredAt(t, s, "retired:"+o.ClientOrderID, session, o))
		}
		return out
	}})
	if got := withheldReasons(t, r); !equalStrings(got, []string{"decided:", "decided:"}) {
		t.Fatalf("decisions %v, want February decided once January's orders are retired", got)
	}
	var february int
	for _, o := range planned(t, r.decisions) {
		if o.Tag != januaryDecision {
			february++
		}
	}
	if february == 0 {
		t.Error("February planned no orders after the retirements")
	}
}

// TestAMomentumRetirementMustNameAnOrderNoVenueReceived: a retirement is
// refused, and the hold stands, unless it is for this account and names an
// order the run released, as released, that is not terminal; a dry-run,
// which sends nothing and ends each release itself, refuses every one.
func TestAMomentumRetirementMustNameAnOrderNoVenueReceived(t *testing.T) {
	orders := januaryReleases(t)
	o := orders[0]
	other, moved, unknown := o, o, o
	other.AccountID = "joint"
	moved.Quantity++
	unknown.ClientOrderID = "tw-nothing"
	r := simulateMomentum(t, momentumOptions{
		silentVenue: true,
		refusals: map[string]string{
			"other account": "is for account",
			"moved":         "describes no order",
			"unknown":       "never released",
			"twice":         "already terminal",
			"other version": "schema version",
		},
		beforeClose: func(s *stream, session string) []event.Envelope {
			if session != "2025-02-28" {
				return nil
			}
			bad := retiredAt(t, s, "other version", session, o)
			bad.SchemaVersion = 2
			return []event.Envelope{
				bad,
				retiredAt(t, s, "other account", session, other),
				retiredAt(t, s, "moved", session, moved),
				retiredAt(t, s, "unknown", session, unknown),
				retiredAt(t, s, "once", session, o),
				retiredAt(t, s, "twice", session, o),
			}
		},
	})
	if len(orders) > 1 {
		if got := withheldReasons(t, r); !equalStrings(got, []string{"decided:", "withheld:rebalance-in-progress"}) {
			t.Fatalf("decisions %v, want February still held while January's other orders work", got)
		}
	}

	dry := dryRun(event.TradingModeDryRun)
	all := released(t, simulateMomentum(t, dry).decisions)
	if len(all) == 0 {
		t.Fatal("setup: the dry-run released nothing")
	}
	dry.refusals = map[string]string{"dry": "dry-run"}
	dry.beforeClose = func(s *stream, session string) []event.Envelope {
		if session != "2025-02-28" {
			return nil
		}
		return []event.Envelope{retiredAt(t, s, "dry", session, all[0])}
	}
	simulateMomentum(t, dry)
}

// TestRetiringARebalancesSellsNeverStartsItsBuys: the IRA holds FFF, which
// January's rebalance sells, and its buys wait on that sell. The sell is
// never sent, and the adapter retires it at February's close as an order
// whose session is over. The retirement ends the rebalance: its buy phase,
// planned from January's targets, prices and cash, is never started, and
// February decides and plans afresh (the owner's decision of 2026-10-22;
// ADR 0026, point 5, amended 2026-10-22).
func TestRetiringARebalancesSellsNeverStartsItsBuys(t *testing.T) {
	setup := momentumOptions{silentVenue: true, held: map[string]int64{"FFF": 100}, stopAfter: "2025-01-31"}
	january := released(t, simulateMomentum(t, setup).decisions)
	var sells []event.OrderReleasedPayload
	for _, o := range january {
		if o.Side != event.OrderSideSell {
			t.Fatalf("setup: January released %s %s before its sells ended", o.Side, o.InstrumentID)
		}
		sells = append(sells, o)
	}
	if len(sells) == 0 {
		t.Fatal("setup: January's rebalance released no sell")
	}
	r := simulateMomentum(t, momentumOptions{silentVenue: true, held: map[string]int64{"FFF": 100}, beforeClose: func(s *stream, session string) []event.Envelope {
		if session != "2025-02-28" {
			return nil
		}
		var out []event.Envelope
		for _, o := range sells {
			out = append(out, retiredAt(t, s, "retired:"+o.ClientOrderID, session, o))
		}
		return out
	}})
	for _, o := range planned(t, r.decisions) {
		if o.Tag == januaryDecision && o.Side == event.OrderSideBuy {
			t.Errorf("January's buy of %s was planned after its sells were retired", o.InstrumentID)
		}
	}
	if got := withheldReasons(t, r); !equalStrings(got, []string{"decided:", "decided:"}) {
		t.Fatalf("decisions %v, want February decided once January's sells are retired", got)
	}
	var february int
	for _, o := range planned(t, r.decisions) {
		if o.Tag != januaryDecision {
			february++
		}
	}
	if february == 0 {
		t.Error("February planned no orders after the retirements")
	}
}
