package risklimit

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestALedgerHoldingTwoSellsUnderOneVenueOrderIDMatchesNoListingToEither:
// the gate refuses a report that would give a second sell a venue order ID
// another carries, so the ledger never holds two; were it to, a listing
// with no client order ID cannot say whose it is, and byVenue matches
// neither: the listing is counted beside them, an over-count, which sells
// less (ADR 0019, amended 2026-10-15; fail closed, AGENTS.md).
func TestALedgerHoldingTwoSellsUnderOneVenueOrderIDMatchesNoListingToEither(t *testing.T) {
	l := newSellLedger()
	l.release("tw-a", "SPY", 60)
	l.release("tw-b", "SPY", 54)
	listing := event.BrokerOrder{BrokerOrderID: "ib-dup", InstrumentID: "SPY", Side: event.OrderSideSell, RemainingQuantity: 30}
	l.sells["tw-a"].venueID = "ib-dup"
	if got := l.byVenue(listing); got != l.sells["tw-a"] {
		t.Fatalf("one sell under ib-dup: byVenue = %v, want tw-a", got)
	}
	l.sells["tw-b"].venueID = "ib-dup"
	if got := l.byVenue(listing); got != nil {
		t.Fatalf("two sells under ib-dup: byVenue = %v, want none", got)
	}
}
