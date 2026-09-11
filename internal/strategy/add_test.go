package strategy_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// This file holds #14's tests: the Add Ladder. Every fixture below builds on
// #11/#12/#13's breakoutBars/openingFill fixtures (campaign_test.go), which
// open a 133-share AAPL Campaign at day(56) with campaignN ==
// breakoutFixtureN(t, cfg) and campaignFillPrice == 201.25 — and on #13's
// addOpportunityBar-shaped bars (see below), whose Low stays comfortably
// above the warmed-up Exit Channel's 100 so an Add opportunity is never
// accidentally shadowed by an exit breach unless a test deliberately wants
// one (TestExitTakesPrecedenceOverAddOnSameBar).
//
// The Turtle Rules p.19: "add 1 Unit every 1/2N measured from the actual
// fill of the previous Unit ... up to the maximum; slippage on the first
// fill pushes later adds out accordingly; all four could be added in one
// day." Every rung below is computed via sizing.NextAddLevel/AddLadder
// rather than hand-typed, so a fixture can never silently drift from the
// production arithmetic it is meant to exercise (ADR 0006).

// addOpportunityBar builds a bar for AAPL (or any instrument) after its
// Campaign has opened, with an explicit High and a Low fixed comfortably
// above the fixture's warmed-up Exit Channel level (100, see
// exit_test.go's header) so an Add rung can be reached without also
// breaching the Exit Channel.
func addOpportunityBar(instrumentID string, periodEnd time.Time, high float64) event.CompletedBarPayload {
	return completedBar(instrumentID, periodEnd, high, 150, 150)
}

// addProposalID mirrors exitProposalID for #14's Add proposal — but, unlike
// an entry or exit proposal, an Add proposal's decisionID also varies by
// UnitIndex, since more than one Add proposal can belong to the same bar
// (the same-bar chain, TestFourUnitsAddedWithinOneBarViaTheSameBarChain).
func addProposalID(instrumentID string, periodEnd time.Time, unitIndex int) string {
	return testDecisionID(fmt.Sprintf("add-proposal-unit-%d", unitIndex), instrumentID, periodEnd)
}

// addFill builds an add-kind fill for instrumentID, executing the Add
// proposal raised at periodEnd for unitIndex.
func addFill(instrumentID, campaignID string, unitIndex int, periodEnd time.Time, fillID string, price float64, quantity int64, filledAt time.Time) event.FillPayload {
	return event.FillPayload{
		InstrumentID: instrumentID,
		Kind:         event.FillKindAdd,
		CampaignID:   campaignID,
		ProposalID:   addProposalID(instrumentID, periodEnd, unitIndex),
		FillID:       fillID,
		Direction:    event.DirectionLong,
		Quantity:     quantity,
		Price:        price,
		FilledAt:     filledAt,
	}
}

func decodeAddProposal(t *testing.T, envelope event.Envelope) event.AddProposalPayload {
	t.Helper()
	if envelope.Type != event.AddProposalEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.AddProposalEventType)
	}
	var payload event.AddProposalPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload
}

func decodeCampaignUnitAdded(t *testing.T, envelope event.Envelope) event.CampaignUnitAddedPayload {
	t.Helper()
	if envelope.Type != event.CampaignUnitAddedEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.CampaignUnitAddedEventType)
	}
	var payload event.CampaignUnitAddedPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload
}

// --- One rung per bar, measured from the actual fill, up to four Units,
// --- then never a fifth -----------------------------------------------

// TestAddLadderOneRungPerBarUpToFourUnitsThenNoFifth is #14's primary
// event-seam test: a rising fixture whose highs reach one rung per bar
// produces exactly one Add proposal per bar, each measured from the
// PREVIOUS Unit's ACTUAL fill (each fill deliberately slipped above its own
// rung, so a rung measured from the intended level rather than the fill
// would be visibly wrong), four Units total, and then no fifth even as
// price keeps rising far beyond every remaining rung.
func TestAddLadderOneRungPerBarUpToFourUnitsThenNoFifth(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)

	// Every rung computed from the PREVIOUS fill, each fill slipped a
	// little above its own rung (never exactly on it) so a wrongly
	// "intended-level" rung would read differently at the NEXT step.
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(rung 2) error = %v", err)
	}
	fill2Price := rung2 + 0.06
	rung3, err := sizing.NextAddLevel(fill2Price, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(rung 3) error = %v", err)
	}
	fill3Price := rung3 + 0.05
	rung4, err := sizing.NextAddLevel(fill3Price, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(rung 4) error = %v", err)
	}
	fill4Price := rung4 + 0.04

	bar57 := addOpportunityBar("AAPL", day(57), rung2+5) // clears rung2, not rung3
	fill2 := addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", fill2Price, 133, day(57))
	bar58 := addOpportunityBar("AAPL", day(58), rung3+5) // clears rung3, not rung4
	fill3 := addFill("AAPL", campaignID, 3, day(58), "sim-fill-add-3", fill3Price, 133, day(58))
	bar59 := addOpportunityBar("AAPL", day(59), rung4+5) // clears rung4
	fill4 := addFill("AAPL", campaignID, 4, day(59), "sim-fill-add-4", fill4Price, 133, day(59))
	// Price keeps rising a great deal further, well past any conceivable
	// fifth rung: the ticket's "no fifth Unit is ever proposed" criterion.
	bar60 := addOpportunityBar("AAPL", day(60), fill4Price+10_000)

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(bar57).
		fill(fill2).
		bar(bar58).
		fill(fill3).
		bar(bar59).
		fill(fill4).
		bar(bar60).
		mustRun()

	proposals := envelopesOfType(emitted, event.AddProposalEventType)
	if len(proposals) != 3 {
		t.Fatalf("got %d add proposal(s), want exactly 3 (units 2, 3 and 4 — never a fifth)", len(proposals))
	}
	added := envelopesOfType(emitted, event.CampaignUnitAddedEventType)
	if len(added) != 3 {
		t.Fatalf("got %d unit-added event(s), want exactly 3", len(added))
	}
	// One Protective-Stop-set for the opening fill, plus one per Add.
	if got := len(envelopesOfType(emitted, event.ProtectiveStopSetEventType)); got != 4 {
		t.Errorf("got %d protective-stop-set event(s), want exactly 4 (unit 1's own, plus one per add)", got)
	}

	wantRungs := []float64{rung2, rung3, rung4}
	wantPreviousFills := []float64{campaignFillPrice, fill2Price, fill3Price}
	wantPeriodEnds := []time.Time{day(57), day(58), day(59)}
	wantFillPrices := []float64{fill2Price, fill3Price, fill4Price}

	for i := 0; i < 3; i++ {
		unitIndex := i + 2
		proposal := decodeAddProposal(t, proposals[i])
		if proposal.CampaignID != campaignID {
			t.Errorf("proposal[%d].CampaignID = %q, want %q", i, proposal.CampaignID, campaignID)
		}
		if proposal.UnitIndex != unitIndex {
			t.Errorf("proposal[%d].UnitIndex = %d, want %d", i, proposal.UnitIndex, unitIndex)
		}
		if !proposal.PeriodEnd.Equal(wantPeriodEnds[i]) {
			t.Errorf("proposal[%d].PeriodEnd = %v, want %v", i, proposal.PeriodEnd, wantPeriodEnds[i])
		}
		if proposal.PreviousUnitFill != wantPreviousFills[i] {
			t.Errorf("proposal[%d].PreviousUnitFill = %v, want %v (the ACTUAL previous fill, not the intended rung)", i, proposal.PreviousUnitFill, wantPreviousFills[i])
		}
		if proposal.Level != wantRungs[i] {
			t.Errorf("proposal[%d].Level = %v, want %v", i, proposal.Level, wantRungs[i])
		}
		if proposal.Quantity != 133 {
			t.Errorf("proposal[%d].Quantity = %d, want the campaign's frozen unit quantity 133", i, proposal.Quantity)
		}
		if proposal.Rule != event.RuleAddLadderHalfN {
			t.Errorf("proposal[%d].Rule = %q, want %q", i, proposal.Rule, event.RuleAddLadderHalfN)
		}
		if proposal.ADR != "0006" {
			t.Errorf("proposal[%d].ADR = %q, want %q (ADR 0006, the frozen ladder)", i, proposal.ADR, "0006")
		}
		if err := proposal.Validate(); err != nil {
			t.Errorf("proposal[%d] fails its own Validate(): %v", i, err)
		}
		wantProposalID := addProposalID("AAPL", wantPeriodEnds[i], unitIndex)
		if proposals[i].ID != wantProposalID {
			t.Errorf("proposal[%d] envelope ID = %q, want %q", i, proposals[i].ID, wantProposalID)
		}

		unit := decodeCampaignUnitAdded(t, added[i])
		if unit.UnitIndex != unitIndex {
			t.Errorf("added[%d].UnitIndex = %d, want %d", i, unit.UnitIndex, unitIndex)
		}
		if unit.FillPrice != wantFillPrices[i] {
			t.Errorf("added[%d].FillPrice = %v, want %v", i, unit.FillPrice, wantFillPrices[i])
		}
		if unit.Units != unitIndex {
			t.Errorf("added[%d].Units = %d, want %d (the count after this add)", i, unit.Units, unitIndex)
		}
		wantStop := wantFillPrices[i] - cfg.StopMultiple*campaignN
		if unit.ProtectiveStop != wantStop {
			t.Errorf("added[%d].ProtectiveStop = %v, want %v", i, unit.ProtectiveStop, wantStop)
		}
		if err := unit.Validate(); err != nil {
			t.Errorf("added[%d] fails its own Validate(): %v", i, err)
		}
	}

	// The headline negative embedded in the positive fixture: proposal[1]
	// (unit 3) must NOT equal what an "intended level" implementation would
	// have produced — rung2 + 0.5N, ignoring the 0.06 slip on unit 2's own
	// fill.
	wrongRung3 := rung2 + 0.5*campaignN
	if proposal := decodeAddProposal(t, proposals[1]); proposal.Level == wrongRung3 {
		t.Errorf("proposal[1].Level = %v equals the INTENDED-level rung %v; it must be measured from the actual (slipped) fill instead", proposal.Level, wrongRung3)
	}

	// Bar 60: price clears every remaining rung by a huge margin, yet the
	// campaign already holds its configured maximum of 4 Units, so nothing
	// further is proposed.
	if got := len(envelopesOfType(emitted, event.AddProposalEventType)); got != 3 {
		t.Errorf("got %d add proposal(s) after bar 60, want still exactly 3: a fifth unit is never proposed", got)
	}
}

// --- Four Units added within one bar, via the same-bar chain --------------

// TestFourUnitsAddedWithinOneBarViaTheSameBarChain is the ticket's "all four
// could be added in one day" criterion: a single bar whose high clears every
// remaining rung, followed by a chain of add fills — each one triggering the
// reducer to immediately re-evaluate and propose the NEXT rung, still
// attributed to that same bar — ends the bar with four Units, each rung
// faithfully measured from the fill immediately before it (fills land
// EXACTLY on their own rung here, via sizing.AddLadder's intended ladder,
// which is the simplest fixture that clears every rung from one bar's high).
func TestFourUnitsAddedWithinOneBarViaTheSameBarChain(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)

	ladder, err := sizing.AddLadder(campaignFillPrice, campaignN, cfg.MaxUnits, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("AddLadder() error = %v", err)
	}
	if len(ladder) != 4 {
		t.Fatalf("len(ladder) = %d, want 4", len(ladder))
	}

	bigBar := addOpportunityBar("AAPL", day(57), ladder[3]+1) // clears every rung at once
	fill2 := addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", ladder[1], 133, day(57))
	fill3 := addFill("AAPL", campaignID, 3, day(57), "sim-fill-add-3", ladder[2], 133, day(57))
	fill4 := addFill("AAPL", campaignID, 4, day(57), "sim-fill-add-4", ladder[3], 133, day(57))

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(bigBar).
		fill(fill2).
		fill(fill3).
		fill(fill4).
		mustRun()

	proposals := envelopesOfType(emitted, event.AddProposalEventType)
	if len(proposals) != 3 {
		t.Fatalf("got %d add proposal(s), want exactly 3, ALL attributed to bar %v", len(proposals), day(57))
	}
	for i, p := range proposals {
		proposal := decodeAddProposal(t, p)
		if !proposal.PeriodEnd.Equal(day(57)) {
			t.Errorf("proposal[%d].PeriodEnd = %v, want %v: every proposal in the chain is attributed to the SAME bar that produced the opportunity", i, proposal.PeriodEnd, day(57))
		}
		if !p.EventTime.Equal(day(57)) {
			t.Errorf("proposal[%d] envelope EventTime = %v, want %v", i, p.EventTime, day(57))
		}
	}
	// The rung values themselves, each measured from the prior fill —
	// here, since every fill lands exactly on its own rung, identical to
	// AddLadder's intended ladder.
	wantLevels := []float64{ladder[1], ladder[2], ladder[3]}
	for i, p := range proposals {
		proposal := decodeAddProposal(t, p)
		if proposal.Level != wantLevels[i] {
			t.Errorf("proposal[%d].Level = %v, want %v", i, proposal.Level, wantLevels[i])
		}
	}

	added := envelopesOfType(emitted, event.CampaignUnitAddedEventType)
	if len(added) != 3 {
		t.Fatalf("got %d unit-added event(s), want exactly 3: all four units (1 opening + 3 adds) by the end of this one bar", len(added))
	}
	for i, a := range added {
		unit := decodeCampaignUnitAdded(t, a)
		if unit.Units != i+2 {
			t.Errorf("added[%d].Units = %d, want %d", i, unit.Units, i+2)
		}
	}

	// Emission ORDER proves the chain: each add proposal precedes the fill
	// that executes it, which precedes the NEXT proposal in the chain, all
	// within the input stream's single bar. Sequence is the engine's own
	// contiguous output counter (docs/architecture.md), so strictly
	// increasing sequence numbers across
	// [proposal(2), added(2), proposal(3), added(3), proposal(4), added(4)]
	// is exactly the chain shape the ticket describes.
	wantOrder := []event.Envelope{proposals[0], added[0], proposals[1], added[1], proposals[2], added[2]}
	for i := 1; i < len(wantOrder); i++ {
		if wantOrder[i-1].Sequence >= wantOrder[i].Sequence {
			t.Errorf("emission %d (Sequence %d) is not strictly before emission %d (Sequence %d): the same-bar chain must interleave proposal->fill->proposal->fill->...",
				i-1, wantOrder[i-1].Sequence, i, wantOrder[i].Sequence)
		}
	}

	// No fifth proposal: the campaign is now fully loaded (4 units).
	if got := len(envelopesOfType(emitted, event.AddProposalEventType)); got != 3 {
		t.Errorf("got %d add proposal(s) total, want still exactly 3 after the fourth unit was added", got)
	}
}

// --- Exit precedence over Add on the same bar (ties off #13's deferred
// --- criterion) -------------------------------------------------------

// TestExitTakesPrecedenceOverAddOnSameBar is the ticket's ADR 0010 ordering
// criterion, and the one issue #13 left explicitly unticked pending this
// ticket: a bar whose LOW breaches the Exit Channel AND whose HIGH reaches
// the next Add rung must produce the exit proposal ONLY.
func TestExitTakesPrecedenceOverAddOnSameBar(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)

	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}

	// Low 99 breaches the warmed-up Exit Channel (100, exit_test.go's
	// header); High comfortably clears rung2 — both conditions true on the
	// SAME bar.
	bothBar := completedBar("AAPL", day(57), rung2+5, 99, 99)

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(bothBar).
		mustRun()

	evaluated := decodeCampaignEvaluated(t, onlyEnvelopeOfType(t, emitted, event.CampaignEvaluatedEventType))
	if !evaluated.ExitConditionMet {
		t.Fatalf("ExitConditionMet = false, want true: the fixture's whole point is a bar that breaches AND reaches the next add rung")
	}

	exitProposal := onlyEnvelopeOfType(t, emitted, event.ExitProposalEventType)
	exited := decodeExitProposal(t, exitProposal)
	if exited.CampaignID != campaignID {
		t.Errorf("exit proposal CampaignID = %q, want %q", exited.CampaignID, campaignID)
	}
	if exited.Quantity != 133 {
		t.Errorf("exit proposal Quantity = %d, want 133 (the campaign's whole, still single-Unit, holding)", exited.Quantity)
	}

	if got := len(envelopesOfType(emitted, event.AddProposalEventType)); got != 0 {
		t.Errorf("got %d add proposal(s), want 0: the exit takes precedence over the add on the same bar (ADR 0010)", got)
	}
}

// --- A multi-Unit Campaign exiting via the Exit Channel: aggregated
// --- quantity and result -----------------------------------------------

// TestMultiUnitCampaignExitsViaExitChannelWithAggregatedQuantityAndResult
// covers a three-Unit Campaign (one opening fill, two Adds) closing through
// the Exit Channel: one Campaign-exited event whose Quantity is the SUM of
// every held Unit and whose EntryPrice is their quantity-weighted average —
// both hand-derived here, replicating the EXACT operation order
// campaignState.entryPrice() uses (see that method's own doc comment) so
// the comparison is exact float64 equality, not merely "close enough".
func TestMultiUnitCampaignExitsViaExitChannelWithAggregatedQuantityAndResult(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)

	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}
	fill2Price := rung2
	rung3, err := sizing.NextAddLevel(fill2Price, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}
	fill3Price := rung3

	bar57 := addOpportunityBar("AAPL", day(57), rung2+5)
	fill2 := addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", fill2Price, 133, day(57))
	bar58 := addOpportunityBar("AAPL", day(58), rung3+5)
	fill3 := addFill("AAPL", campaignID, 3, day(58), "sim-fill-add-3", fill3Price, 133, day(58))

	breachAt := day(59)
	breachBar := postEntryBar("AAPL", breachAt, 99)
	exitFill := closingExitFill("AAPL", campaignID, 100, breachAt, day(60))
	exitFill.Quantity = 399 // 3 units x 133

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(bar57).
		fill(fill2).
		bar(bar58).
		fill(fill3).
		bar(breachBar).
		fill(exitFill).
		mustRun()

	if got := len(envelopesOfType(emitted, event.CampaignUnitAddedEventType)); got != 2 {
		t.Fatalf("got %d unit-added event(s), want exactly 2 (this fixture must actually hold 3 units before it exits)", got)
	}

	exited := decodeCampaignExited(t, onlyEnvelopeOfType(t, emitted, event.CampaignExitedEventType))

	if exited.Units != 3 {
		t.Errorf("Units = %d, want 3", exited.Units)
	}
	if exited.Quantity != 399 {
		t.Errorf("Quantity = %d, want 399 (133 x 3 units)", exited.Quantity)
	}

	// The quantity-weighted average, computed in the EXACT operation order
	// campaignState.entryPrice() uses: multiply each fill by its own
	// quantity (as float64 variables, never folded constants — #10's
	// constant-folding-vs-runtime-float64 discipline), sum left to right,
	// divide once at the end.
	q := float64(133)
	weighted := q*campaignFillPrice + q*fill2Price + q*fill3Price
	wantEntryPrice := weighted / float64(399)
	if exited.EntryPrice != wantEntryPrice {
		t.Errorf("EntryPrice = %v, want exactly %v (the quantity-weighted average fill price)", exited.EntryPrice, wantEntryPrice)
	}

	wantRealisedResult := float64(399) * (exitFill.Price - wantEntryPrice) * cfg.DollarsPerPoint
	if exited.RealisedResult != wantRealisedResult {
		t.Errorf("RealisedResult = %v, want exactly %v", exited.RealisedResult, wantRealisedResult)
	}
	wantRealisedResultInN := (exitFill.Price - wantEntryPrice) / campaignN
	if exited.RealisedResultInN != wantRealisedResultInN {
		t.Errorf("RealisedResultInN = %v, want exactly %v", exited.RealisedResultInN, wantRealisedResultInN)
	}

	// The Protective Stop reported is the MINIMUM across all three units —
	// which, since every fill under ADR 0005's resting-order model lands at
	// or above its own rung, is always Unit 1's own (the lowest fill, and
	// so the lowest stop): campaignFillPrice - 2N.
	wantStopLevel, err := sizing.ProtectiveStopLevel(campaignFillPrice, campaignN, cfg.StopMultiple, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("ProtectiveStopLevel() error = %v", err)
	}
	if exited.ProtectiveStopLevel != wantStopLevel {
		t.Errorf("ProtectiveStopLevel = %v, want %v (unit 1's own stop, the minimum across all held units)", exited.ProtectiveStopLevel, wantStopLevel)
	}

	if err := exited.Validate(); err != nil {
		t.Errorf("emitted campaign exited payload fails its own Validate(): %v", err)
	}
}

// --- Partial add fill accepted for the filled quantity ---------------------

// TestPartialAddFillAcceptedForFilledQuantity mirrors #11's own partial-fill
// rule on the Add side: a partial fill for an Add proposal is accepted for
// the quantity that actually executed, mirroring #11's rule for the
// opening fill (a second partial for the SAME add proposal is deferred to
// #67, the identical limitation applyFill already enforces on the entry
// side).
func TestPartialAddFillAcceptedForFilledQuantity(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}

	bar57 := addOpportunityBar("AAPL", day(57), rung2+5)
	partial := addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", rung2, 80, day(57))

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(bar57).
		fill(partial).
		mustRun()

	unit := decodeCampaignUnitAdded(t, onlyEnvelopeOfType(t, emitted, event.CampaignUnitAddedEventType))
	if unit.Quantity != 80 {
		t.Errorf("Quantity = %d, want the filled 80, not the proposed 133", unit.Quantity)
	}
	if unit.Units != 2 {
		t.Errorf("Units = %d, want 2", unit.Units)
	}
}

// --- Duplicate add fill is an idempotent no-op ------------------------------

func TestDuplicateAddFillIsAnIdempotentNoOp(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}

	bar57 := addOpportunityBar("AAPL", day(57), rung2+5)
	fill2 := addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", rung2, 133, day(57))

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(bar57).
		fill(fill2).
		fill(fill2).
		mustRun()

	if got := len(envelopesOfType(emitted, event.CampaignUnitAddedEventType)); got != 1 {
		t.Fatalf("got %d unit-added event(s), want exactly 1 despite the duplicate delivery", got)
	}
}

// TestAddFillReusingAFillIDWithDifferentContentsIsRejected mirrors the
// entry/exit/stop-fill precedent: a fill id reused with different contents
// is a reconciliation failure, not a duplicate.
func TestAddFillReusingAFillIDWithDifferentContentsIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}

	bar57 := addOpportunityBar("AAPL", day(57), rung2+5)
	fill2 := addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", rung2, 133, day(57))
	repriced := fill2
	repriced.Price = fill2.Price + 1

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(bar57).
		fill(fill2).
		fill(repriced).
		wantRunError(fill2.FillID, "differ")
}

// --- Never a fifth Unit: an add fill arriving anyway fails closed ---------

// TestAddFillForAFullCampaignFailsClosed builds a genuinely full,
// four-Unit Campaign (via the same-bar chain) and then delivers one more
// add-kind fill: with no outstanding add proposal (the campaign is already
// at its configured maximum, so evaluateAdd proposed nothing), the fill is
// rejected.
func TestAddFillForAFullCampaignFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	ladder, err := sizing.AddLadder(campaignFillPrice, campaignN, cfg.MaxUnits, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("AddLadder() error = %v", err)
	}

	bigBar := addOpportunityBar("AAPL", day(57), ladder[3]+1)
	fill2 := addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", ladder[1], 133, day(57))
	fill3 := addFill("AAPL", campaignID, 3, day(57), "sim-fill-add-3", ladder[2], 133, day(57))
	fill4 := addFill("AAPL", campaignID, 4, day(57), "sim-fill-add-4", ladder[3], 133, day(57))

	// A stray fifth fill, naming a proposal id that (correctly) was never
	// raised: the campaign is already at maxUnits, so evaluateAdd proposed
	// nothing for it to reference.
	stray := addFill("AAPL", campaignID, 5, day(57), "sim-fill-add-5", ladder[3]+100, 133, day(57))

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(bigBar).
		fill(fill2).
		fill(fill3).
		fill(fill4).
		fill(stray).
		wantRunError("AAPL", "configured maximum")
}

// TestAddFillForAClosedCampaignFailsClosed: once a Campaign has exited, an
// add-kind fill referencing it is a reconciliation failure — the identical
// fail-closed answer applyStopFill and applyExitFill already give.
func TestAddFillForAClosedCampaignFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	stop := closingStopFill("AAPL", campaignID, breakoutFixtureN(t, cfg), day(57))

	stray := event.FillPayload{
		InstrumentID: "AAPL",
		Kind:         event.FillKindAdd,
		CampaignID:   campaignID,
		ProposalID:   addProposalID("AAPL", day(58), 2),
		FillID:       "sim-fill-add-stray",
		Direction:    event.DirectionLong,
		Quantity:     133,
		Price:        300,
		FilledAt:     day(58),
	}

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		fill(stop).
		fill(stray).
		wantRunError("AAPL", "no open campaign")
}

// TestAddFillNamingAnUnknownProposalIsRejected covers a fill naming a
// proposal id that was never actually raised, while a DIFFERENT add
// proposal genuinely is outstanding.
func TestAddFillNamingAnUnknownProposalIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}

	bar57 := addOpportunityBar("AAPL", day(57), rung2+5)
	wrong := addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", rung2, 133, day(57))
	wrong.ProposalID = "add-proposal-unit-2:AAPL:1999-01-01T00:00:00.000000000Z"

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(bar57).
		fill(wrong).
		wantRunError("AAPL", "1999-01-01")
}

// TestAddFillOverExecutionIsRejected: a fill quantity above the proposed
// unit quantity is never absorbed (ADR 0003).
func TestAddFillOverExecutionIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}

	bar57 := addOpportunityBar("AAPL", day(57), rung2+5)
	overExecuted := addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", rung2, 200, day(57))

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(bar57).
		fill(overExecuted).
		wantRunError("AAPL", "200", "133")
}

// TestAddFillWithMismatchedDirectionIsRejected mirrors the entry/stop/exit
// precedent.
func TestAddFillWithMismatchedDirectionIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}

	bar57 := addOpportunityBar("AAPL", day(57), rung2+5)
	mismatched := addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", rung2, 133, day(57))
	mismatched.Direction = "short"

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(bar57).
		fill(mismatched).
		wantRunError("direction")
}

// --- The add proposal's own lifecycle (ADR 0011, reused) -------------------

// TestAddProposalExpiresWhenNextBarArrivesWithoutAFill mirrors #13's own
// exit-side test: an add proposal with no fill, superseded by the next
// completed bar, expires (Kind add), emitted before that bar's own
// decisions — and the campaign stays at its prior Unit count throughout.
func TestAddProposalExpiresWhenNextBarArrivesWithoutAFill(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignN := breakoutFixtureN(t, cfg)
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}

	proposedAt := day(57)
	nextAt := day(58)
	bar57 := addOpportunityBar("AAPL", proposedAt, rung2+5)
	// High (200) stays below rung2 (~220) and Low (150) stays above the
	// warmed-up Exit Channel (100): no add opportunity and no exit breach,
	// so this bar's only decision is the expiry itself.
	bar58 := completedBar("AAPL", nextAt, 200, 150, 150)

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(bar57).
		bar(bar58).
		mustRun()

	expiredEnvelope := onlyEnvelopeOfType(t, emitted, event.ProposalExpiredEventType)
	expired := decodeProposalExpired(t, expiredEnvelope)
	if expired.Kind != event.ProposalKindAdd {
		t.Errorf("Kind = %q, want %q", expired.Kind, event.ProposalKindAdd)
	}
	if expired.SignalID != "" {
		t.Errorf("SignalID = %q, want empty: an add proposal is not sized from a signal", expired.SignalID)
	}
	wantProposalID := addProposalID("AAPL", proposedAt, 2)
	if expired.ProposalID != wantProposalID {
		t.Errorf("ProposalID = %q, want %q", expired.ProposalID, wantProposalID)
	}
	if !expired.PeriodEnd.Equal(proposedAt) {
		t.Errorf("PeriodEnd = %v, want %v", expired.PeriodEnd, proposedAt)
	}
	if !expired.ExpiredAt.Equal(nextAt) {
		t.Errorf("ExpiredAt = %v, want %v", expired.ExpiredAt, nextAt)
	}
	if expired.Level != rung2 {
		t.Errorf("Level = %v, want %v", expired.Level, rung2)
	}
	if expired.Quantity != 133 {
		t.Errorf("Quantity = %d, want 133", expired.Quantity)
	}

	// Emitted BEFORE the superseding bar's own Campaign-evaluated decision
	// (ADR 0010's ordering).
	evaluatedEnvelopes := envelopesOfType(emitted, event.CampaignEvaluatedEventType)
	if len(evaluatedEnvelopes) != 2 {
		t.Fatalf("got %d campaign-evaluated event(s), want 2", len(evaluatedEnvelopes))
	}
	last := evaluatedEnvelopes[len(evaluatedEnvelopes)-1]
	if expiredEnvelope.Sequence >= last.Sequence {
		t.Errorf("expiry Sequence %d is not before the superseding bar's own Campaign-evaluated Sequence %d", expiredEnvelope.Sequence, last.Sequence)
	}

	// Never added: no fill ever arrived.
	if got := len(envelopesOfType(emitted, event.CampaignUnitAddedEventType)); got != 0 {
		t.Errorf("got %d unit-added event(s), want 0: no add fill ever arrived", got)
	}
}

// TestLateAddFillAfterExpiryIsRejected mirrors #13's own exit-side test:
// once the next bar has superseded the add proposal, a fill for it is
// rejected.
func TestLateAddFillAfterExpiryIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}

	proposedAt := day(57)
	late := addFill("AAPL", campaignID, 2, proposedAt, "sim-fill-add-2", rung2, 133, day(59))

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(addOpportunityBar("AAPL", proposedAt, rung2+5)).
		bar(completedBar("AAPL", day(58), 200, 150, 150)). // supersedes the add proposal
		fill(late).
		wantRunError("AAPL", "no outstanding add proposal")
}

// --- Replay equivalence -----------------------------------------------

// TestReplayingTheFourUnitAddFixtureTwiceYieldsByteIdenticalEmissions
// covers the ticket's replay-equivalence requirement for a full
// open-then-four-Units life.
func TestReplayingTheFourUnitAddFixtureTwiceYieldsByteIdenticalEmissions(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)

	build := func() []event.Envelope {
		ladder, err := sizing.AddLadder(campaignFillPrice, campaignN, cfg.MaxUnits, sizing.DirectionLong)
		if err != nil {
			t.Fatalf("AddLadder() error = %v", err)
		}
		bigBar := addOpportunityBar("AAPL", day(57), ladder[3]+1)
		fill2 := addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", ladder[1], 133, day(57))
		fill3 := addFill("AAPL", campaignID, 3, day(57), "sim-fill-add-3", ladder[2], 133, day(57))
		fill4 := addFill("AAPL", campaignID, 4, day(57), "sim-fill-add-4", ladder[3], 133, day(57))

		return newStream(t, cfg).
			bars(breakoutBars("AAPL")).
			fill(openingFill("AAPL")).
			bar(bigBar).
			fill(fill2).
			fill(fill3).
			fill(fill4).
			mustRun()
	}

	first, second := build(), build()
	if len(first) != len(second) {
		t.Fatalf("emission counts differ: %d and %d", len(first), len(second))
	}
	if got := len(envelopesOfType(first, event.CampaignUnitAddedEventType)); got != 3 {
		t.Fatalf("got %d unit-added event(s) in the first run, want 3: the fixture must actually add three units", got)
	}
	for i := range first {
		a, err := json.Marshal(first[i])
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		b, err := json.Marshal(second[i])
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("emission %d differs between replays:\n  first:  %s\n  second: %s", i, a, b)
		}
	}
}
