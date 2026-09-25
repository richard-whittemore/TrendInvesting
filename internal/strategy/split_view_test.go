package strategy_test

import (
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds the event-seam test issue #38 asks for alongside a split's
// own tests (split_test.go): that signal computation reads only the
// split-adjusted view, confirmed by a split-shaped view disagreement, and
// that using the raw view for signals is something this fixture would
// actually catch (ADR 0004).
//
// ADR 0004's Context states the failure mode directly: "a 2-for-1 reads as a
// 50% crash" if raw, unadjusted prices feed the rules. This file's positive
// case shows the fixture is INSENSITIVE to the raw view (whatever it holds,
// the decisions are the same), and its negative case shows the fixture IS
// sensitive to which view is actually wired into the reducer's indicators —
// which is what makes the positive claim a real test rather than a vacuous
// one.

// syntheticBarWithViewFactor is syntheticBar with the raw view scaled by
// factor relative to the split-adjusted one, exactly the shape an
// unadjusted split (or a producer that swapped the two views) leaves behind:
// the split-adjusted view is the same at every factor, and the raw view is
// not.
func syntheticBarWithViewFactor(instrumentID string, periodEnd time.Time, tr, factor float64) event.CompletedBarPayload {
	return completedBarWithDistinctViews(instrumentID, periodEnd, 100+tr, 100, 100, factor)
}

// breakoutBarsWithViewFactor is breakoutBars with every bar's raw view
// scaled by factor relative to its split-adjusted view, otherwise identical.
func breakoutBarsWithViewFactor(instrumentID string, factor float64) []event.CompletedBarPayload {
	highs := breakoutFixtureHighs()
	ramp, breakoutHigh := highs[:55], highs[55]

	bars := make([]event.CompletedBarPayload, 0, len(ramp)+breakoutHistoryPreamble+1)
	for i, high := range ramp {
		bars = append(bars, syntheticBarWithViewFactor(instrumentID, day(i+1), high-100, factor))
	}
	n := stableRampWilderValue()
	for i := 1; i <= breakoutHistoryPreamble; i++ {
		bars = append(bars, syntheticBarWithViewFactor(instrumentID, day(55).Add(time.Duration(i)*time.Hour), n, factor))
	}
	bars = append(bars, syntheticBarWithViewFactor(instrumentID, day(56), breakoutHigh-100, factor))
	return bars
}

// swapViews returns bars with each one's SplitAdjusted and Raw views
// exchanged (labels included), simulating a producer defect that wired the
// raw view into the slot the rules read (ADR 0004).
func swapViews(bars []event.CompletedBarPayload) []event.CompletedBarPayload {
	out := make([]event.CompletedBarPayload, len(bars))
	for i, b := range bars {
		out[i] = b
		out[i].SplitAdjusted, out[i].Raw = b.Raw, b.SplitAdjusted
		out[i].SplitAdjusted.View, out[i].Raw.View = event.ViewSplitAdjusted, event.ViewRaw
	}
	return out
}

// TestSignalsReadOnlyTheSplitAdjustedView is issue #38's split-view seam
// test: a fixture confirming channel levels — and so the resulting entry
// proposal — are unaffected by whatever the raw view holds, WITH a negative
// case proving that claim is not vacuous.
func TestSignalsReadOnlyTheSplitAdjustedView(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()

	// POSITIVE: the raw view is scaled 2x relative to the split-adjusted
	// view in the second run, exactly the shape an unadjusted 2-for-1 split
	// would leave (ADR 0004's own example). Every decision this fixture
	// reaches — Setup evaluations, the Signal, and the trade proposal's own
	// EntryLevel — is byte-for-byte identical to the factor-1 run.
	factor1 := newStream(t, cfg).bars(breakoutBarsWithViewFactor("AAPL", 1)).mustRun()
	factor2 := newStream(t, cfg).bars(breakoutBarsWithViewFactor("AAPL", 2)).mustRun()
	if len(factor1) != len(factor2) {
		t.Fatalf("%d decision(s) with the raw view scaled 2x, want %d: split-adjusted, not raw, must be what the rules see (ADR 0004)", len(factor2), len(factor1))
	}
	for i := range factor1 {
		if factor1[i].Type != factor2[i].Type || string(factor1[i].Payload) != string(factor2[i].Payload) {
			t.Fatalf("decision %d differs only because the raw view was scaled:\n  factor 1: %s %s\n  factor 2: %s %s",
				i, factor1[i].Type, factor1[i].Payload, factor2[i].Type, factor2[i].Payload)
		}
	}
	correctProposal := decodeTradeProposal(t, onlyEnvelopeOfType(t, factor1, event.TradeProposalEventType))

	// NEGATIVE: swapping which view is fed into SplitAdjusted — using the
	// factor-2 run's bars with their two views exchanged, so SplitAdjusted
	// now holds what was scaled 2x — changes the resulting EntryLevel. This
	// is "using the raw view for signals must fail the fixture" made
	// concrete: if this reducer ever read the wrong view, this fixture's own
	// pinned EntryLevel would no longer match, and the test above would fail
	// exactly as this one demonstrates.
	corrupted := newStream(t, cfg).bars(swapViews(breakoutBarsWithViewFactor("AAPL", 2))).mustRun()
	corruptedProposal := decodeTradeProposal(t, onlyEnvelopeOfType(t, corrupted, event.TradeProposalEventType))
	if corruptedProposal.EntryLevel == correctProposal.EntryLevel {
		t.Fatalf("using the raw view for signals produced the SAME entry level %v as the correct split-adjusted run; this fixture must be sensitive to which view feeds the rules (ADR 0004)", correctProposal.EntryLevel)
	}
	if corruptedProposal.EntryLevel != correctProposal.EntryLevel*2 {
		t.Fatalf("entry level with the views swapped = %v, want exactly %v (the correct level, scaled by the same factor the corrupted view was)", corruptedProposal.EntryLevel, correctProposal.EntryLevel*2)
	}
}
