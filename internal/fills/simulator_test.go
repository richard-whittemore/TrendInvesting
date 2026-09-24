package fills_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
)

// This file covers the resting-order book itself: what Observe puts in it and
// takes out of it, and what Resting reports. The sell side — each Unit's one
// Exit Order — has its own file, exit_order_fill_test.go.

// TestRestingReportsEveryOrderInForce: the book, read back. #19's driver
// needs it to report what was left outstanding when a run ended, and a test
// needs it to see the book without reaching inside the package.
func TestRestingReportsEveryOrderInForce(t *testing.T) {
	t.Parallel()

	// #79 moves the entry fill (and so this Add's rung) down by 1 N; this
	// bar is shifted down by the same 1.5 (matching campaignLifeBars' own
	// bar 57) so the Add still fills at its rung rather than gapping.
	bars := append(warmUpBars(), breakoutBar(), bar(day(57), 155.7, 156.7, 155.5, 156.5))
	simulator := observeRun(t, bars)

	resting := simulator.Resting(testInstrument)
	if len(resting) != 2 {
		t.Fatalf("got %d resting order(s), want 2 (one Exit Order per held Unit, each at its Protective Stop)%v", len(resting), resting)
	}
	for i, want := range []struct {
		level    float64
		unit     int
		quantity int64
	}{
		// Unit 1 entered at 155.575 with its stop 3 below, raised by half N
		// when Unit 2 was added; Unit 2 entered at 156.4 with its own stop 3
		// below.
		{153.325, 1, fixtureUnitQuantity},
		{153.4, 2, fixtureUnitQuantity},
	} {
		if resting[i].Kind != event.FillKindStop || resting[i].Side != fills.SideSell {
			t.Errorf("resting[%d] is a %s %s, want a sell stop", i, resting[i].Side, resting[i].Kind)
		}
		assertPrice(t, "resting level", resting[i].Level, want.level)
		if len(resting[i].UnitIndexes) != 1 || resting[i].UnitIndexes[0] != want.unit {
			t.Errorf("resting[%d] names units %v, want [%d]", i, resting[i].UnitIndexes, want.unit)
		}
		if resting[i].Quantity != want.quantity {
			t.Errorf("resting[%d] quantity = %d, want %d", i, resting[i].Quantity, want.quantity)
		}
	}

	if got := simulator.Resting("NEVER-SEEN"); got != nil {
		t.Errorf("Resting(unknown instrument) = %v, want nil", got)
	}
}

// TestObserveRemovesAnExpiredProposal covers the expiry path directly.
//
// It has to be direct, because this reducer never reaches it for an entry:
// a Signal already guarantees the bar's high strictly exceeds the Entry
// Channel high it rests at (#79), so the SAME bar that raises the proposal
// always covers it too, and the simulator always fills one before ADR 0011's
// next-bar expiry can arrive (see TestProposalsAreAlwaysCoveredByTheBarThat
// RaisedThem). The path exists for a proposal built directly, as this test
// does, standing in for a producer this reducer's invariant does not
// constrain — #30's adapter — and for #15's second expiry path, a pending
// Add cancelled the instant a stop fill partially closes the Campaign.
func TestObserveRemovesAnExpiredProposal(t *testing.T) {
	t.Parallel()

	simulator, err := fills.New(baselineConfig(), testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	proposed := restingEntryProposal(t, 157)
	if err := simulator.Observe(proposed); err != nil {
		t.Fatalf("Observe(trade proposal) error = %v", err)
	}
	if got := simulator.Resting(testInstrument); len(got) != 1 || got[0].Kind != event.FillKindEntry {
		t.Fatalf("after the proposal, Resting = %v, want one entry order", got)
	}

	expiry := event.ProposalExpiredPayload{
		InstrumentID:   testInstrument,
		Kind:           event.ProposalKindEntry,
		ProposalID:     proposed.ID,
		SignalID:       "signal:AAPL",
		PeriodEnd:      day(56),
		ExpiredAt:      day(57),
		EarliestFillAt: day(55),
		Rule:           event.RuleSignalExpiresWithItsBar,
		ADR:            event.ADRSignalExpiry,
		Reason:         event.ExpiryReasonSupersededByNextBar,
		Quantity:       fixtureUnitQuantity,
		Level:          157,
	}
	if err := expiry.Validate(); err != nil {
		t.Fatalf("the fixture expiry is invalid: %v", err)
	}
	if err := simulator.Observe(envelope(t, "expiry-1", event.ProposalExpiredEventType, event.ProposalExpiredSchemaVersion, day(57), expiry)); err != nil {
		t.Fatalf("Observe(expiry) error = %v", err)
	}
	if got := simulator.Resting(testInstrument); len(got) != 0 {
		t.Errorf("after the expiry, Resting = %v, want nothing: an order the strategy has withdrawn must not still fill", got)
	}

	// An expiry naming a proposal this simulator never held leaves the book
	// alone rather than clearing an unrelated order.
	if err := simulator.Observe(proposed); err != nil {
		t.Fatalf("Observe(trade proposal, again) error = %v", err)
	}
	stale := expiry
	stale.ProposalID = "proposal:AAPL:some-other-bar"
	if err := simulator.Observe(envelope(t, "expiry-2", event.ProposalExpiredEventType, event.ProposalExpiredSchemaVersion, day(57), stale)); err != nil {
		t.Fatalf("Observe(stale expiry) error = %v", err)
	}
	if got := simulator.Resting(testInstrument); len(got) != 1 {
		t.Errorf("an expiry for an unrelated proposal cleared the book: Resting = %v", got)
	}
}

// TestObserveIgnoresTheEmissionsThatCreateNoOrder pins the allowlist: these
// are recognised and deliberately without effect, so a reader can tell "we
// considered this" from "we never thought about it".
func TestObserveIgnoresTheEmissionsThatCreateNoOrder(t *testing.T) {
	t.Parallel()

	simulator, err := fills.New(baselineConfig(), testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for _, eventType := range []string{
		event.SetupEvaluatedEventType,
		event.SignalEventType,
		event.ProposalDeclinedEventType,
		event.CampaignEvaluatedEventType,
		event.EngineStateEventType,
		event.DrawdownStepAppliedEventType,
		event.NotionalAccountRebasedEventType,
		event.NotionalAccountRecoveredEventType,
		event.NotionalAccountCashAdjustedEventType,
		event.ConfigurationEventType,
		event.CompletedBarEventType,
		event.FillEventType,
		event.AccountSnapshotEventType,
		event.CashMovementEventType,
	} {
		ignored := envelope(t, "ignored-"+eventType, eventType, 1, day(1), map[string]string{"instrument_id": testInstrument})
		if err := simulator.Observe(ignored); err != nil {
			t.Errorf("Observe(%s) error = %v, want nil", eventType, err)
		}
	}
	if got := simulator.Resting(testInstrument); len(got) != 0 {
		t.Errorf("Resting = %v, want nothing: none of those events creates an order", got)
	}
}

// observeRun drives the composed loop over bars and returns the simulator
// afterwards, so a test can inspect the book it was left holding.
func observeRun(t *testing.T, bars []event.CompletedBarPayload) *fills.Simulator {
	t.Helper()
	cfg := baselineConfig()
	simulator, reducer := newComposed(t, cfg)
	driveComposed(t, simulator, reducer, cfg, bars)
	return simulator
}
