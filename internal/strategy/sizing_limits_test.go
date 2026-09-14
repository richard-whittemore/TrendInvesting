package strategy_test

import (
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestABreakoutSizingBeyondTheExactlyRepresentableRangeStopsTheRun is the one
// outcome sizing.SizeUnit reports as an error rather than as a decline, taken
// through the reducer that has to decide what to do with it.
//
// Above 2^53 a float64 can no longer represent consecutive integers, so the
// fractional part the Turtle truncation discards does not exist at that
// magnitude and the conversion to int64 has nothing meaningful to round. The
// Notional Account is required to be finite and positive and nothing more —
// correctly, since no rule caps it — so an account large enough relative to N
// reaches it: a figure recorded in cents rather than dollars, a currency
// whose unit is small, or a zero too many.
//
// It is not a decline. A decline is a fact about the account, journalled and
// carried on from; this is arithmetic the reducer believed sound producing a
// figure it cannot use, so the run stops.
func TestABreakoutSizingBeyondTheExactlyRepresentableRangeStopsTheRun(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	// The breakout fixture's N is about 37.58, and the Baseline risks 0.5 %
	// of the account per 1N move, so a quantity at or above 2^53 needs an
	// account above roughly 6.8e19.
	cfg.NotionalAccount.StartingEquity = 1e20

	// ADR 0010's cash basis: a Unit is never sized without an
	// account.snapshot ever having supplied an available-cash figure. This
	// test's subject is the sizing arithmetic's own representable-range
	// limit, not affordability, so the figure is generous.
	envelopes := []event.Envelope{accountSnapshotEnvelopeFor(t, cfg, 2, event.AccountSnapshotPayload{
		AsOf: day(0), Equity: cfg.NotionalAccount.StartingEquity, AvailableCash: 1e30, Currency: "USD",
	})}
	for i, bar := range breakoutBars("AAPL") {
		envelopes = append(envelopes, barEnvelopeFor(t, cfg, uint64(i+3), bar))
	}

	_, err := runReducerOverAccountEvents(t, cfg, envelopes)
	if err == nil {
		t.Fatal("Run() error = nil, want a quantity beyond the exactly representable range to stop the run")
	}
	if !strings.Contains(err.Error(), "exceeds the largest exactly representable whole quantity") {
		t.Fatalf("Run() error = %v, want it to name the truncation it refused", err)
	}
	// Named, so an operator reading the journal knows which bar of which
	// instrument produced it rather than only that sizing failed somewhere.
	if !strings.Contains(err.Error(), "AAPL") {
		t.Errorf("Run() error = %v, want it to name the instrument", err)
	}
}

// TestTheSameBreakoutSizesNormallyAtTheBaselineAccount is the control: the
// fixture above differs from every other breakout fixture in this package by
// one field, and without this a defect that refused to size ANY breakout
// would satisfy it.
func TestTheSameBreakoutSizesNormallyAtTheBaselineAccount(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()

	// ADR 0010's cash basis — see the sibling test above for why this
	// file supplies one unconditionally.
	envelopes := []event.Envelope{accountSnapshotEnvelopeFor(t, cfg, 2, event.AccountSnapshotPayload{
		AsOf: day(0), Equity: cfg.NotionalAccount.StartingEquity, AvailableCash: 1_000_000_000, Currency: "USD",
	})}
	for i, bar := range breakoutBars("AAPL") {
		envelopes = append(envelopes, barEnvelopeFor(t, cfg, uint64(i+3), bar))
	}

	emitted, err := runReducerOverAccountEvents(t, cfg, envelopes)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("got %d trade proposal(s), want exactly 1", len(proposals))
	}
	if quantity := decodeTradeProposal(t, proposals[0]).Quantity; quantity != 133 {
		t.Errorf("Quantity = %d, want the fixture's 133 shares", quantity)
	}
}
