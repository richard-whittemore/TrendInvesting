package fills_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
)

// This file covers the resting-order book itself: what Observe puts in it and
// takes out of it, what Resting reports, and the one ordering case among
// competing sells that the full-life fixtures cannot reach on their own.

// TestWhenABarReachesBothTheStopsAndTheExitChannelTheStopsFillFirst is the
// enumerated case RunBar's doc comment names "stop (sell) + Exit-Channel exit
// (sell)".
//
// Both orders close the same Campaign and only one of them can close all of
// it, so the bar's range cannot say which happened; worst-price-first is the
// pessimistic tie-break. Here the four Units' own stops sit around 156.3 and
// the Exit Channel at 159.3, so the stops are the worse close and take the
// Campaign off one Unit at a time. The exit proposal is then cancelled — its
// Campaign no longer exists — and never fills.
//
// It also pins the per-Unit shape of a stop fill: every Unit's stop is its
// own resting order at its own level, so four Units produce four fills, each
// naming exactly one Unit.
func TestWhenABarReachesBothTheStopsAndTheExitChannelTheStopsFillFirst(t *testing.T) {
	t.Parallel()

	bars := campaignLifeBars()
	// The same bar as the full-life fixture's last, but reaching 150 instead
	// of 157: deep enough to take out every Protective Stop on the way to
	// breaking the Exit Channel.
	bars[len(bars)-1] = bar(day(80), 163.0, 163.1, 150, 152)
	run := runComposed(t, baselineConfig(), bars)

	got := fillPayloads(t, run.Inputs)
	if len(got) != 8 {
		t.Fatalf("got %d fill(s), want 8 (entry, three Adds, four stops)%s", len(got), describe(envelopesOfType(run.Inputs, event.FillEventType)))
	}
	stops := got[4:]

	// Worst price first, one Unit at a time: Unit 1's stop is the lowest
	// (it has risen by half N three times from the lowest fill), Unit 4's
	// the highest.
	wantPrices := []float64{156.25, 156.325, 156.4, 156.475}
	wantUnits := []string{got[0].FillID, got[1].FillID, got[2].FillID, got[3].FillID}
	for i, stop := range stops {
		if stop.Kind != event.FillKindStop {
			t.Fatalf("fill %d Kind = %q, want %q", 4+i, stop.Kind, event.FillKindStop)
		}
		assertPrice(t, "stop fill price", stop.Price, wantPrices[i])
		if stop.Quantity != fixtureUnitQuantity {
			t.Errorf("stop fill %d quantity = %d, want one Unit (%d)", i, stop.Quantity, fixtureUnitQuantity)
		}
		if len(stop.UnitIDs) != 1 || stop.UnitIDs[0] != wantUnits[i] {
			t.Errorf("stop fill %d names units %v, want exactly [%s]", i, stop.UnitIDs, wantUnits[i])
		}
	}
	for i := 1; i < len(stops); i++ {
		if stops[i].Price < stops[i-1].Price {
			t.Errorf("stop fill %d executed at %v, below the one before it at %v: sells are ordered worst price first", i, stops[i].Price, stops[i-1].Price)
		}
	}

	// The exit was proposed — the bar did break the channel — and never
	// filled, because the stops had already emptied the Campaign.
	if got := envelopesOfType(run.Decisions, event.ExitProposalEventType); len(got) != 1 {
		t.Fatalf("got %d exit proposal(s), want 1: the bar did break the Exit Channel", len(got))
	}
	for _, fill := range got {
		if fill.Kind == event.FillKindExit {
			t.Error("an exit fill was produced for a Campaign the stops had already closed")
		}
	}

	var exited event.CampaignExitedPayload
	decodeInto(t, onlyOfType(t, run.Decisions, event.CampaignExitedEventType), &exited)
	if exited.Reason != event.ExitReasonStop {
		t.Errorf("exited.Reason = %q, want %q", exited.Reason, event.ExitReasonStop)
	}
	if exited.Units != 4 {
		t.Errorf("exited.Units = %d, want 4 over the Campaign's whole life", exited.Units)
	}
}

// TestWhenTheExitChannelIsTheWorseCloseItFillsAndTheStopsAreCancelled is the
// other half of the same enumerated case.
//
// Early in a Campaign the Exit Channel sits well BELOW every Protective Stop,
// so a bar deep enough to break the channel has already passed every stop on
// the way down — and selling everything at the channel is worse than stopping
// out above it. Worst-price-first therefore fills the exit, which closes the
// whole Campaign and cancels the stops.
func TestWhenTheExitChannelIsTheWorseCloseItFillsAndTheStopsAreCancelled(t *testing.T) {
	t.Parallel()

	bars := campaignLifeBars()[:59] // through day(59): four Units, stops ~156.3-156.55
	// The Exit Channel over bars 40..59 stands at 139, far below every stop.
	bars = append(bars, bar(day(60), 159, 159.2, 138, 140))
	run := runComposed(t, baselineConfig(), bars)

	got := fillPayloads(t, run.Inputs)
	if len(got) != 5 {
		t.Fatalf("got %d fill(s), want 5 (entry, three Adds, the exit)%s", len(got), describe(envelopesOfType(run.Inputs, event.FillEventType)))
	}
	exit := got[4]
	if exit.Kind != event.FillKindExit {
		t.Fatalf("final fill Kind = %q, want %q: the exit is the worse close, so it happened first", exit.Kind, event.FillKindExit)
	}
	assertPrice(t, "exit fill level", exit.Level, 139)
	// min(139, open 159) - 0.075: the bar traded down through the channel.
	assertPrice(t, "exit fill price", exit.Price, 138.925)
	if want := 4 * fixtureUnitQuantity; exit.Quantity != want {
		t.Errorf("exit fill quantity = %d, want %d", exit.Quantity, want)
	}

	if stopped := envelopesOfType(run.Decisions, event.CampaignUnitsStoppedEventType); len(stopped) != 0 {
		t.Errorf("got %d units-stopped event(s), want none: the exit closed the Campaign first", len(stopped))
	}
	var exited event.CampaignExitedPayload
	decodeInto(t, onlyOfType(t, run.Decisions, event.CampaignExitedEventType), &exited)
	if exited.Reason != event.ExitReasonExitChannel {
		t.Errorf("exited.Reason = %q, want %q", exited.Reason, event.ExitReasonExitChannel)
	}
	if exited.RealisedResult >= 0 {
		t.Errorf("exited.RealisedResult = %v, want a loss: exiting at 138.925 is far below the average entry", exited.RealisedResult)
	}
}

// TestRestingReportsEveryOrderInForce: the book, read back. #19's driver
// needs it to report what was left outstanding when a run ended, and a test
// needs it to see the book without reaching inside the package.
func TestRestingReportsEveryOrderInForce(t *testing.T) {
	t.Parallel()

	bars := append(warmUpBars(), breakoutBar(), bar(day(57), 157.2, 158.2, 157, 158))
	simulator := observeRun(t, bars)

	resting := simulator.Resting(testInstrument)
	if len(resting) != 2 {
		t.Fatalf("got %d resting order(s), want 2 (one Protective Stop per held Unit)%v", len(resting), resting)
	}
	for i, want := range []struct {
		level    float64
		unit     int
		quantity int64
	}{
		// Unit 1 entered at 157.075 with its stop 3 below, raised by half N
		// when Unit 2 was added; Unit 2 entered at 157.9 with its own stop 3
		// below.
		{154.825, 1, fixtureUnitQuantity},
		{154.9, 2, fixtureUnitQuantity},
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
// It has to be direct, because this reducer never reaches it: every proposal
// it raises is covered by the bar that raised it, so the simulator always
// fills one before ADR 0011's next-bar expiry can arrive (see
// TestProposalsAreAlwaysCoveredByTheBarThatRaisedThem). The path exists for
// the producers that are not this reducer — a Variant whose entry rests at
// the Entry Channel level rather than the breakout bar's own high would leave
// proposals unfilled routinely — and for #15's second expiry path, a pending
// Add cancelled the instant a stop fill partially closes the Campaign.
func TestObserveRemovesAnExpiredProposal(t *testing.T) {
	t.Parallel()

	simulator, err := fills.New(baselineConfig(), testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	proposal := event.TradeProposalPayload{
		InstrumentID:           testInstrument,
		PeriodEnd:              day(56),
		SignalID:               "signal:AAPL",
		Rule:                   event.RuleUnitSizingVolatilityNormalised,
		ADR:                    event.ADRUnitSizing,
		Direction:              event.DirectionLong,
		EntryLevel:             157,
		Quantity:               fixtureUnitQuantity,
		N:                      fixtureN,
		SizingMode:             event.SizingModeVolatilityNormalised,
		UnitVolatilityFraction: 0.005,
		StopMultiple:           2,
		RiskAtStop:             0.005 * 2,
		RealisedRiskAtStop:     float64(fixtureUnitQuantity) * 2 * fixtureN * 1 / 1_000_000,
		DollarsPerPoint:        1,
		NotionalAccount:        1_000_000,
		ProtectiveStopIntent:   157 - 2*fixtureN,
	}
	if err := proposal.Validate(); err != nil {
		t.Fatalf("the fixture proposal is invalid: %v", err)
	}
	proposed := envelope(t, "proposal:AAPL:day-56", event.TradeProposalEventType, event.TradeProposalSchemaVersion, day(56), proposal)
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
