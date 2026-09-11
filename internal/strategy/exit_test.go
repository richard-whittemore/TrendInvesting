package strategy_test

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// This file holds #13's tests: the Exit-Channel exit. Every fixture below
// builds on #11/#12's breakoutBars/openingFill fixtures (campaign_test.go),
// which open a 133-share AAPL Campaign at day(56) with campaignN ==
// breakoutFixtureN(t, cfg) and an Exit Channel that is ALREADY warm the
// moment the Campaign opens: breakoutBars feeds 56 completed bars, each with
// split-adjusted Low == 100 (syntheticBar's doc comment), into the Exit
// Channel regardless of Campaign state (see the reinterpretation stated on
// issue #13 and in the PR: the window must be warm the instant a Campaign
// opens). So from the very first post-entry bar, ExitChannel.Extreme() ==
// (100, true) — the lowest low of the last 20 of those 56 identical-100
// bars — with no further warm-up needed in these fixtures.
//
// # The reinterpretation this file exists to prove
//
// "A close below the prior 20 completed bars' low closes the whole
// Campaign" (the ticket's literal text) is read as: the reducer PROPOSES the
// exit when a bar's LOW trades through the preceding-20-bar low (ADR 0005,
// The Turtle Rules p.26 — an intraday resting order, not a close-price
// decision), and the Campaign closes on the FILL that executes that
// proposal, never on the detection bar itself. Every test below that
// combines a breach with a fill keeps the two as separate input events for
// exactly this reason.

// postEntryBar builds a bar for AAPL (or any instrument) after its Campaign
// has opened, with an explicit Low — unlike syntheticBar, which fixes Low at
// 100 for the whole warm-up/breakout fixture. High and Close sit comfortably
// above Low so the bar is internally consistent (bar.go's cross-field
// checks) regardless of how low Low itself is.
func postEntryBar(instrumentID string, periodEnd time.Time, low float64) event.CompletedBarPayload {
	return completedBar(instrumentID, periodEnd, low+50, low, low+25)
}

// freshBreakoutBar builds a bar guaranteed to be a fresh Entry Channel
// breakout for instrumentID at periodEnd, for fixtures below that need one
// AFTER a Campaign has already closed (proving the instrument is a Setup
// again). Unlike nextBreakoutBar (campaign_test.go, fixed at day(57)), which
// these fixtures' own breach/exit bars already occupy, this can be placed at
// any later day: its high (250) comfortably exceeds every high in
// breakoutFixtureHighs (topping out at 200) and every postEntryBar high used
// below, so it breaks out regardless of what the intervening bars added to
// the Entry Channel.
func freshBreakoutBar(instrumentID string, periodEnd time.Time) event.CompletedBarPayload {
	return completedBar(instrumentID, periodEnd, 250, 150, 200)
}

// exitProposalID is the deterministic id #13's exit proposal for
// instrumentID on periodEnd carries, mirroring testDecisionID's role for
// entry proposals.
func exitProposalID(instrumentID string, periodEnd time.Time) string {
	return testDecisionID("exit-proposal", instrumentID, periodEnd)
}

// exitFillGap is how far BELOW the Exit Channel level the exit fill fixtures
// below actually fill, mirroring campaignStopGap: it proves the recorded
// exit price is what filled, not the level (ADR 0005's gap rule), applied
// here to the exit-channel fill just as #12 applied it to the stop fill.
const exitFillGap = 0.5

// closingExitFill is the fill that closes the Campaign openingFill opened,
// executing the exit proposal raised at breachPeriodEnd for a breach at
// channelLow, gapped through by exitFillGap.
func closingExitFill(instrumentID, campaignID string, channelLow float64, breachPeriodEnd, filledAt time.Time) event.FillPayload {
	return event.FillPayload{
		InstrumentID: instrumentID,
		Kind:         event.FillKindExit,
		CampaignID:   campaignID,
		ProposalID:   exitProposalID(instrumentID, breachPeriodEnd),
		FillID:       "sim-fill-exit-0001",
		Direction:    event.DirectionLong,
		Quantity:     133,
		Price:        channelLow - exitFillGap,
		FilledAt:     filledAt,
	}
}

func decodeCampaignEvaluated(t *testing.T, envelope event.Envelope) event.CampaignEvaluatedPayload {
	t.Helper()
	if envelope.Type != event.CampaignEvaluatedEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.CampaignEvaluatedEventType)
	}
	var payload event.CampaignEvaluatedPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload
}

func decodeExitProposal(t *testing.T, envelope event.Envelope) event.ExitProposalPayload {
	t.Helper()
	if envelope.Type != event.ExitProposalEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.ExitProposalEventType)
	}
	var payload event.ExitProposalPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload
}

// --- The per-bar Campaign-evaluated event (#12's gap, filled here) --------

// TestCampaignEvaluatedEmittedEveryBarWhileCampaignOpenWithNoBreach covers
// several ordinary bars in a row, none of which breach: the per-bar event
// must still appear on every one of them, reporting the level in force, and
// no exit proposal.
func TestCampaignEvaluatedEmittedEveryBarWhileCampaignOpenWithNoBreach(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(postEntryBar("AAPL", day(57), 105)).
		bar(postEntryBar("AAPL", day(58), 110)).
		mustRun()

	evaluated := envelopesOfType(emitted, event.CampaignEvaluatedEventType)
	if len(evaluated) != 2 {
		t.Fatalf("got %d Campaign-evaluated event(s), want exactly 2 (one per bar)", len(evaluated))
	}
	for i, e := range evaluated {
		payload := decodeCampaignEvaluated(t, e)
		if payload.CampaignID != campaignID {
			t.Errorf("evaluated[%d].CampaignID = %q, want %q", i, payload.CampaignID, campaignID)
		}
		if payload.InstrumentID != "AAPL" {
			t.Errorf("evaluated[%d].InstrumentID = %q, want AAPL", i, payload.InstrumentID)
		}
		wantStop := campaignFillPrice - cfg.StopMultiple*campaignN
		if payload.ProtectiveStop != wantStop {
			t.Errorf("evaluated[%d].ProtectiveStop = %v, want %v", i, payload.ProtectiveStop, wantStop)
		}
		if !payload.ExitChannelReady {
			t.Errorf("evaluated[%d].ExitChannelReady = false, want true (56 bars already precede the campaign)", i)
		}
		if payload.ExitChannelLow != 100 {
			t.Errorf("evaluated[%d].ExitChannelLow = %v, want 100 (the low of every one of the 56 warm-up/breakout bars)", i, payload.ExitChannelLow)
		}
		if payload.ExitConditionMet {
			t.Errorf("evaluated[%d].ExitConditionMet = true, want false: neither bar's low fell below 100", i)
		}
	}

	if got := len(envelopesOfType(emitted, event.ExitProposalEventType)); got != 0 {
		t.Errorf("got %d exit proposal(s), want 0", got)
	}
	// An instrument in a Campaign is not a Setup (CONTEXT.md): confirms these
	// two bars produced no Setup-evaluated event either, the same invariant
	// #11/#12 already established.
	if got := countFor(t, emitted, event.SetupEvaluatedEventType, "AAPL"); got != 56 {
		t.Errorf("got %d Setup-evaluated event(s), want 56 (bars 1..56 only)", got)
	}
}

// TestExitChannelTieIsNotABreach: a bar whose low exactly equals the Exit
// Channel low (100) must not be read as a breach — The Turtle Rules p.26's
// "falls below", mirroring the Entry Channel's strict "exceeds" (ADR 0002).
func TestExitChannelTieIsNotABreach(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(postEntryBar("AAPL", day(57), 100)).
		mustRun()

	evaluated := decodeCampaignEvaluated(t, onlyEnvelopeOfType(t, emitted, event.CampaignEvaluatedEventType))
	if evaluated.ExitConditionMet {
		t.Error("ExitConditionMet = true on a tie, want false (a tie is not a breach)")
	}
	if got := len(envelopesOfType(emitted, event.ExitProposalEventType)); got != 0 {
		t.Errorf("got %d exit proposal(s) on a tie, want 0", got)
	}
}

// --- The exit proposal -----------------------------------------------------

// TestExitChannelBreachEmitsExitProposalForFullQuantity is #13's primary
// event-seam test: a bar whose low trades below the preceding-20-bar low
// proposes closing the WHOLE Campaign, at the channel level, with reason
// exit-channel — a proposal only, no Campaign state has moved yet (the
// reinterpretation this file's header states).
func TestExitChannelBreachEmitsExitProposalForFullQuantity(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	breachAt := day(57)

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(postEntryBar("AAPL", breachAt, 99)).
		mustRun()

	evaluated := decodeCampaignEvaluated(t, onlyEnvelopeOfType(t, emitted, event.CampaignEvaluatedEventType))
	if !evaluated.ExitConditionMet {
		t.Fatal("ExitConditionMet = false, want true: the bar's low (99) is below the channel low (100)")
	}

	proposalEnvelope := onlyEnvelopeOfType(t, emitted, event.ExitProposalEventType)
	wantID := exitProposalID("AAPL", breachAt)
	if proposalEnvelope.ID != wantID {
		t.Errorf("exit proposal ID = %q, want %q (deterministic: exit-proposal:<instrument>:<period-end>)", proposalEnvelope.ID, wantID)
	}
	if !proposalEnvelope.EventTime.Equal(breachAt) {
		t.Errorf("exit proposal EventTime = %v, want %v", proposalEnvelope.EventTime, breachAt)
	}

	proposal := decodeExitProposal(t, proposalEnvelope)
	if proposal.CampaignID != campaignID {
		t.Errorf("CampaignID = %q, want %q", proposal.CampaignID, campaignID)
	}
	if proposal.InstrumentID != "AAPL" {
		t.Errorf("InstrumentID = %q, want AAPL", proposal.InstrumentID)
	}
	if proposal.Reason != event.ExitReasonExitChannel {
		t.Errorf("Reason = %q, want %q", proposal.Reason, event.ExitReasonExitChannel)
	}
	if proposal.Level != 100 {
		t.Errorf("Level = %v, want 100 (the channel low, not the bar's own low)", proposal.Level)
	}
	if proposal.Quantity != 133 {
		t.Errorf("Quantity = %d, want the campaign's whole 133-share holding (no partial exit)", proposal.Quantity)
	}
	if proposal.Rule != event.RuleExitChannelBreach {
		t.Errorf("Rule = %q, want %q", proposal.Rule, event.RuleExitChannelBreach)
	}
	if proposal.ADR != "0002" {
		t.Errorf("ADR = %q, want %q (The Turtle Rules p.26, System 2's exit)", proposal.ADR, "0002")
	}
	if err := proposal.Validate(); err != nil {
		t.Errorf("emitted exit proposal payload fails its own Validate(): %v", err)
	}

	// The proposal alone must not have closed the Campaign — the
	// reinterpretation's whole point.
	if got := len(envelopesOfType(emitted, event.CampaignExitedEventType)); got != 0 {
		t.Errorf("got %d Campaign-exited event(s) from the proposal alone, want 0: the campaign closes on the FILL, not on detection", got)
	}
}

// --- The exit fill closes the Campaign -------------------------------------

// TestExitFillClosesCampaignWithReasonExitChannel is #13's other primary
// event-seam test: a breach, then a fill executing the exit proposal, gapped
// BELOW the channel level (ADR 0005's gap rule), closes the Campaign with
// reason exit-channel and a hand-derivable realised result.
func TestExitFillClosesCampaignWithReasonExitChannel(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	breachAt := day(57)
	exitFill := closingExitFill("AAPL", campaignID, 100, breachAt, day(58))

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(postEntryBar("AAPL", breachAt, 99)).
		fill(exitFill).
		mustRun()

	exitEnvelope := onlyEnvelopeOfType(t, emitted, event.CampaignExitedEventType)
	if !exitEnvelope.EventTime.Equal(exitFill.FilledAt) {
		t.Errorf("Campaign exited EventTime = %v, want the fill's %v", exitEnvelope.EventTime, exitFill.FilledAt)
	}

	exited := decodeCampaignExited(t, exitEnvelope)
	if exited.CampaignID != campaignID {
		t.Errorf("CampaignID = %q, want %q", exited.CampaignID, campaignID)
	}
	if exited.FillID != exitFill.FillID {
		t.Errorf("FillID = %q, want the exit fill's %q", exited.FillID, exitFill.FillID)
	}
	if exited.Reason != event.ExitReasonExitChannel {
		t.Errorf("Reason = %q, want %q", exited.Reason, event.ExitReasonExitChannel)
	}
	if exited.Rule != event.RuleCampaignExitedByExitChannel {
		t.Errorf("Rule = %q, want %q", exited.Rule, event.RuleCampaignExitedByExitChannel)
	}
	if exited.EntryPrice != campaignFillPrice {
		t.Errorf("EntryPrice = %v, want the opening fill's %v", exited.EntryPrice, campaignFillPrice)
	}
	// The headline assertion: the exit price is what actually filled, gapped
	// THROUGH (below) the channel level, so it is strictly below it.
	if exited.ExitPrice != exitFill.Price {
		t.Errorf("ExitPrice = %v, want the exit fill's own price %v", exited.ExitPrice, exitFill.Price)
	}
	if !(exited.ExitPrice < 100) {
		t.Fatalf("the fixture no longer gaps the exit price below the channel level (100); the headline assertion above is vacuous without it")
	}
	if exited.Quantity != 133 {
		t.Errorf("Quantity = %d, want the whole 133-share campaign", exited.Quantity)
	}
	if exited.CampaignN != campaignN {
		t.Errorf("CampaignN = %v, want the campaign's frozen %v", exited.CampaignN, campaignN)
	}

	wantRealisedResult := float64(133) * (exitFill.Price - campaignFillPrice) * cfg.DollarsPerPoint
	if exited.RealisedResult != wantRealisedResult {
		t.Errorf("RealisedResult = %v, want exactly %v", exited.RealisedResult, wantRealisedResult)
	}
	if !(exited.RealisedResult < 0) {
		t.Errorf("RealisedResult = %v, want negative (the fixture exits well below entry)", exited.RealisedResult)
	}
	// A single-Unit Campaign: AverageMoveInN (the per-share average) and
	// RealisedResultInUnitN (the aggregate Unit-N result) coincide
	// NUMERICALLY (PR #74 review response to "N Result Ignores Units"), but
	// each is asserted against sizing's own function — the same one the
	// producer calls — since the two formulas are not guaranteed to agree
	// bit-for-bit in float64 (they multiply and divide in a different
	// order) even when they agree mathematically.
	wantMoveInN, err := sizing.AverageMoveInN(exitFill.Price, campaignFillPrice, campaignN)
	if err != nil {
		t.Fatalf("sizing.AverageMoveInN() error = %v", err)
	}
	if exited.AverageMoveInN != wantMoveInN {
		t.Errorf("AverageMoveInN = %v, want exactly %v", exited.AverageMoveInN, wantMoveInN)
	}
	wantResultInUnitN, err := sizing.RealisedResultInUnitN(wantRealisedResult, 133, campaignN, cfg.DollarsPerPoint)
	if err != nil {
		t.Fatalf("sizing.RealisedResultInUnitN() error = %v", err)
	}
	if exited.RealisedResultInUnitN != wantResultInUnitN {
		t.Errorf("RealisedResultInUnitN = %v, want exactly %v", exited.RealisedResultInUnitN, wantResultInUnitN)
	}
	if math.Abs(wantMoveInN-wantResultInUnitN) > 1e-9 {
		t.Errorf("AverageMoveInN (%v) and RealisedResultInUnitN (%v) should coincide numerically for a single, fully-filled unit", wantMoveInN, wantResultInUnitN)
	}
	if err := exited.Validate(); err != nil {
		t.Errorf("emitted Campaign-exited payload fails its own Validate(): %v", err)
	}
}

// TestAfterAnExitChannelExitTheInstrumentSignalsAgain covers "the instrument
// is a Setup again": once the Campaign exits via the Exit Channel, the very
// next breakout bar produces a fresh Signal and proposal.
func TestAfterAnExitChannelExitTheInstrumentSignalsAgain(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	breachAt := day(57)
	exitFill := closingExitFill("AAPL", campaignID, 100, breachAt, day(58))

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(postEntryBar("AAPL", breachAt, 99)).
		fill(exitFill).
		bar(freshBreakoutBar("AAPL", day(59))).
		mustRun()

	if got := countFor(t, emitted, event.CampaignExitedEventType, "AAPL"); got != 1 {
		t.Fatalf("got %d Campaign-exited event(s), want exactly 1", got)
	}
	if got := countFor(t, emitted, event.SignalEventType, "AAPL"); got != 2 {
		t.Errorf("AAPL emitted %d Signal(s), want 2 (the original entry, and a fresh one after the exit)", got)
	}
	if got := countFor(t, emitted, event.TradeProposalEventType, "AAPL"); got != 2 {
		t.Errorf("AAPL emitted %d proposal(s), want 2", got)
	}
}

// --- The exit proposal expires with its bar (ADR 0011, reused) ------------

// TestExitProposalExpiresWhenNextBarArrivesWithoutAFill mirrors #11's
// TestProposalWithNoFillOpensNoCampaignAndExpiresWithItsBar, on the exit
// side: a breach with no fill, superseded by the next completed bar, expires
// (Kind exit), emitted before that bar's own Campaign-evaluated decision —
// and the Campaign stays open throughout, since nothing closed it.
func TestExitProposalExpiresWhenNextBarArrivesWithoutAFill(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	breachAt := day(57)
	nextAt := day(58)

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(postEntryBar("AAPL", breachAt, 99)).
		bar(postEntryBar("AAPL", nextAt, 110)). // no breach itself
		mustRun()

	expiredEnvelope := onlyEnvelopeOfType(t, emitted, event.ProposalExpiredEventType)
	expired := decodeProposalExpired(t, expiredEnvelope)
	if expired.Kind != event.ProposalKindExit {
		t.Errorf("Kind = %q, want %q", expired.Kind, event.ProposalKindExit)
	}
	if expired.SignalID != "" {
		t.Errorf("SignalID = %q, want empty: an exit proposal is not sized from a signal", expired.SignalID)
	}
	wantProposalID := exitProposalID("AAPL", breachAt)
	if expired.ProposalID != wantProposalID {
		t.Errorf("ProposalID = %q, want %q", expired.ProposalID, wantProposalID)
	}
	if !expired.PeriodEnd.Equal(breachAt) {
		t.Errorf("PeriodEnd = %v, want %v (the breach bar)", expired.PeriodEnd, breachAt)
	}
	if !expired.ExpiredAt.Equal(nextAt) {
		t.Errorf("ExpiredAt = %v, want %v (the bar that superseded it)", expired.ExpiredAt, nextAt)
	}
	if expired.Reason != event.ExpiryReasonSupersededByNextBar {
		t.Errorf("Reason = %q, want %q", expired.Reason, event.ExpiryReasonSupersededByNextBar)
	}
	if expired.Level != 100 {
		t.Errorf("Level = %v, want 100 (the channel level that was proposed)", expired.Level)
	}
	if expired.Quantity != 133 {
		t.Errorf("Quantity = %d, want 133", expired.Quantity)
	}

	// Emitted BEFORE the superseding bar's own Campaign-evaluated decision
	// (ADR 0010's ordering, mirroring how an entry proposal's expiry
	// precedes the superseding bar's own decisions).
	nextBarEvaluated := envelopesOfType(emitted, event.CampaignEvaluatedEventType)
	if len(nextBarEvaluated) != 2 {
		t.Fatalf("got %d Campaign-evaluated event(s), want 2 (one per bar)", len(nextBarEvaluated))
	}
	lastEvaluated := nextBarEvaluated[len(nextBarEvaluated)-1]
	if expiredEnvelope.Sequence >= lastEvaluated.Sequence {
		t.Errorf("expiry Sequence %d is not before the superseding bar's own Campaign-evaluated Sequence %d", expiredEnvelope.Sequence, lastEvaluated.Sequence)
	}

	// Nothing closed the Campaign: no fill ever arrived.
	if got := len(envelopesOfType(emitted, event.CampaignExitedEventType)); got != 0 {
		t.Errorf("got %d Campaign-exited event(s), want 0: no exit fill ever arrived", got)
	}
	// Exactly one exit proposal total (the expired one); the second bar did
	// not itself breach, so no new one replaced it.
	if got := len(envelopesOfType(emitted, event.ExitProposalEventType)); got != 1 {
		t.Errorf("got %d exit proposal(s), want 1", got)
	}
}

// TestLateExitFillAfterExpiryIsRejected mirrors #11's
// TestFillArrivingAfterItsProposalExpiredIsRejected on the exit side: once
// the next bar has superseded the exit proposal, a fill for it is rejected.
func TestLateExitFillAfterExpiryIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	breachAt := day(57)
	late := closingExitFill("AAPL", campaignID, 100, breachAt, day(59))

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(postEntryBar("AAPL", breachAt, 99)).
		bar(postEntryBar("AAPL", day(58), 110)). // supersedes the exit proposal
		fill(late).
		wantRunError("AAPL", "no outstanding exit proposal")
}

// --- Exit fill rejections ---------------------------------------------------

// TestExitFillWithWrongQuantityIsRejected covers the "no partial exit"
// criterion from the fill side: a partial exit fill is rejected rather than
// closing part of the position.
func TestExitFillWithWrongQuantityIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	breachAt := day(57)
	partial := closingExitFill("AAPL", campaignID, 100, breachAt, day(58))
	partial.Quantity = 100

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(postEntryBar("AAPL", breachAt, 99)).
		fill(partial).
		wantRunError("AAPL", "100", "133")
}

// TestExitFillForAnUnknownCampaignIsRejected: no campaign of any id has ever
// existed for AAPL (no opening fill was ever sent), mirroring
// TestStopFillForAnUnknownCampaignIsRejected.
func TestExitFillForAnUnknownCampaignIsRejected(t *testing.T) {
	t.Parallel()

	stray := event.FillPayload{
		InstrumentID: "AAPL",
		Kind:         event.FillKindExit,
		CampaignID:   "campaign:AAPL:1999-01-01T00:00:00.000000000Z",
		ProposalID:   "exit-proposal:AAPL:1999-01-02T00:00:00.000000000Z",
		FillID:       "sim-fill-9999",
		Direction:    event.DirectionLong,
		Quantity:     133,
		Price:        100,
		FilledAt:     day(57),
	}

	newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(stray).
		wantRunError("AAPL", "1999-01-01", "no open campaign")
}

// TestExitFillForAnInstrumentWithNoHistoryIsRejected is the same rule for an
// instrument the reducer has never evaluated at all, mirroring
// TestStopFillForAnInstrumentWithNoHistoryIsRejected.
func TestExitFillForAnInstrumentWithNoHistoryIsRejected(t *testing.T) {
	t.Parallel()

	stray := event.FillPayload{
		InstrumentID: "TSLA",
		Kind:         event.FillKindExit,
		CampaignID:   "campaign:TSLA:2026-02-27T00:00:00.000000000Z",
		ProposalID:   "exit-proposal:TSLA:2026-03-20T00:00:00.000000000Z",
		FillID:       "sim-fill-9999",
		Direction:    event.DirectionLong,
		Quantity:     133,
		Price:        100,
		FilledAt:     day(57),
	}

	newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		fill(stray).
		wantRunError("TSLA")
}

// TestExitFillNamingAWrongProposalIsRejected: an exit fill whose ProposalID
// does not match the currently outstanding exit proposal.
func TestExitFillNamingAWrongProposalIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	breachAt := day(57)
	wrong := closingExitFill("AAPL", campaignID, 100, breachAt, day(58))
	wrong.ProposalID = "exit-proposal:AAPL:1999-01-01T00:00:00.000000000Z"

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(postEntryBar("AAPL", breachAt, 99)).
		fill(wrong).
		wantRunError("AAPL", "1999-01-01")
}

// TestExitFillWithMismatchedDirectionIsRejected mirrors
// TestStopFillWithMismatchedDirectionIsRejected on the exit side.
func TestExitFillWithMismatchedDirectionIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	breachAt := day(57)
	mismatched := closingExitFill("AAPL", campaignID, 100, breachAt, day(58))
	mismatched.Direction = "short"

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(postEntryBar("AAPL", breachAt, 99)).
		fill(mismatched).
		wantRunError("direction")
}

// TestDuplicateExitFillIsAnIdempotentNoOp mirrors
// TestDuplicateStopFillIsAnIdempotentNoOp for an exit fill.
func TestDuplicateExitFillIsAnIdempotentNoOp(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	breachAt := day(57)
	exitFill := closingExitFill("AAPL", campaignID, 100, breachAt, day(58))

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(postEntryBar("AAPL", breachAt, 99)).
		fill(exitFill).
		fill(exitFill).
		mustRun()

	if got := len(envelopesOfType(emitted, event.CampaignExitedEventType)); got != 1 {
		t.Fatalf("got %d Campaign-exited event(s), want exactly 1 despite the duplicate delivery", got)
	}
}

// TestExitFillReusingAFillIDWithDifferentContentsIsRejected mirrors
// TestStopFillReusingAFillIDWithDifferentContentsIsRejected.
func TestExitFillReusingAFillIDWithDifferentContentsIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	breachAt := day(57)
	exitFill := closingExitFill("AAPL", campaignID, 100, breachAt, day(58))
	repriced := exitFill
	repriced.Price = exitFill.Price + 1

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(postEntryBar("AAPL", breachAt, 99)).
		fill(exitFill).
		fill(repriced).
		wantRunError(exitFill.FillID, "differ")
}

// --- Stop and exit racing for the same Campaign ----------------------------

// TestStopFillThenExitFillForSameCampaignSecondFails is the ticket's
// "stop and exit in the same bar" requirement, at the seam this ticket
// actually owns: the reducer processes fills in the sequence it is handed
// them (ADR 0005 leaves resolving which one the SIMULATOR sends first to
// #18), and whichever closing fill arrives first closes the Campaign — a
// second closing fill for an already-closed Campaign fails closed, exactly
// as #12 already established for two stop fills.
func TestStopFillThenExitFillForSameCampaignSecondFails(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	stop := closingStopFill("AAPL", campaignID, campaignN, day(57))
	breachAt := day(57)
	exitFill := closingExitFill("AAPL", campaignID, 100, breachAt, day(58))

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		fill(stop).
		fill(exitFill).
		wantRunError("AAPL", "no open campaign")
}

// --- Exit Channel not ready ---------------------------------------------

// TestNoExitProposalWhileExitChannelNotReady covers the case the brief
// describes as possible only when a Campaign opens very early: an
// EntryChannelLength far shorter than ExitChannelLength lets a Campaign open
// before ExitChannelLength completed bars have ever been seen, so the Exit
// Channel is genuinely not ready the first time the Campaign is evaluated.
// No exit proposal may be raised, and the event says so.
func TestNoExitProposalWhileExitChannelNotReady(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	cfg.EntryChannelLength = 3
	cfg.ExitChannelLength = 30

	// 20 warm-up bars (TR 1..20, Low fixed at 100 via syntheticBar) make N
	// ready for the first time on bar 21; the 3-bar Entry Channel is long
	// since warm. Bar 21 is a breakout by construction (300 exceeds any
	// 3-bar window built from highs 101..120).
	var bars []event.CompletedBarPayload
	for i := 1; i <= 20; i++ {
		bars = append(bars, syntheticBar("AAPL", day(i), float64(i)))
	}
	breakoutBar := completedBar("AAPL", day(21), 300, 100, 250)
	bars = append(bars, breakoutBar)

	proposalID := testDecisionID("proposal", "AAPL", day(21))
	fill := event.FillPayload{
		InstrumentID: "AAPL",
		Kind:         event.FillKindEntry,
		ProposalID:   proposalID,
		FillID:       "sim-fill-notready",
		Direction:    event.DirectionLong,
		Quantity:     1,
		Price:        300,
		FilledAt:     day(21),
	}
	// A bar whose low (1) would unambiguously "look like" a breach against
	// any real channel level, arriving the very next day: proves the
	// suppression is about readiness, not merely about the level chosen.
	nextBar := completedBar("AAPL", day(22), 50, 1, 25)

	emitted := newStream(t, cfg).
		bars(bars).
		fill(fill).
		bar(nextBar).
		mustRun()

	evaluated := decodeCampaignEvaluated(t, onlyEnvelopeOfType(t, emitted, event.CampaignEvaluatedEventType))
	if evaluated.ExitChannelReady {
		t.Fatal("ExitChannelReady = true, want false: only 21 completed bars have ever been seen against a 30-bar Exit Channel")
	}
	if evaluated.ExitChannelLow != 0 {
		t.Errorf("ExitChannelLow = %v, want 0 while not ready", evaluated.ExitChannelLow)
	}
	if evaluated.ExitConditionMet {
		t.Error("ExitConditionMet = true, want false: an unready channel has no real level to have fallen below")
	}
	if got := len(envelopesOfType(emitted, event.ExitProposalEventType)); got != 0 {
		t.Errorf("got %d exit proposal(s) while the exit channel is not ready, want 0", got)
	}
}

// --- Second instrument unaffected, and replay equivalence -----------------

// TestASecondInstrumentIsUnaffectedByAnothersExitChannelExit mirrors
// TestASecondInstrumentIsUnaffectedByAnothersStopExit for an exit-channel
// close.
func TestASecondInstrumentIsUnaffectedByAnothersExitChannelExit(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	breachAt := day(57)
	exitFill := closingExitFill("AAPL", campaignID, 100, breachAt, day(58))

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bars(breakoutBars("MSFT")).
		bar(postEntryBar("AAPL", breachAt, 99)).
		fill(exitFill).
		mustRun()

	if got := countFor(t, emitted, event.CampaignExitedEventType, "AAPL"); got != 1 {
		t.Errorf("AAPL has %d Campaign-exited event(s), want 1", got)
	}
	if got := countFor(t, emitted, event.CampaignEvaluatedEventType, "MSFT"); got != 0 {
		t.Errorf("MSFT has %d Campaign-evaluated event(s), want 0: MSFT never opened a campaign", got)
	}
	if got := countFor(t, emitted, event.TradeProposalEventType, "MSFT"); got != 1 {
		t.Errorf("MSFT emitted %d proposal(s), want 1", got)
	}
}

// TestReplayingTheExitChannelFixtureTwiceYieldsByteIdenticalEmissions covers
// the ticket's replay-equivalence requirement for a full open-then-exit
// life through the Exit Channel.
func TestReplayingTheExitChannelFixtureTwiceYieldsByteIdenticalEmissions(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	breachAt := day(57)
	exitFill := closingExitFill("AAPL", campaignID, 100, breachAt, day(58))

	build := func() []event.Envelope {
		return newStream(t, cfg).
			bars(breakoutBars("AAPL")).
			fill(openingFill("AAPL")).
			bar(postEntryBar("AAPL", breachAt, 99)).
			fill(exitFill).
			bar(freshBreakoutBar("AAPL", day(59))).
			mustRun()
	}

	first, second := build(), build()
	if len(first) != len(second) {
		t.Fatalf("emission counts differ: %d and %d", len(first), len(second))
	}
	if got := len(envelopesOfType(first, event.CampaignExitedEventType)); got != 1 {
		t.Fatalf("got %d Campaign-exited event(s) in the first run, want 1: the fixture must actually exercise an exit-channel exit", got)
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

// --- The look-ahead negative (this ticket's headline) ----------------------

// TestLookAheadExitChannelWouldMissTheBreach is #13's headline negative
// test, mirroring #9's TestLookAheadEntryChannelWouldMissTheBreakout on the
// exit side: a fixture where the decision bar's own low IS the new 20-bar
// minimum. A correct implementation (the channel excludes the decision bar)
// proposes the exit. A look-ahead implementation (the channel includes the
// decision bar) computes the channel low AS the bar's own low, sees "not
// below", and proposes nothing.
//
// The fixture: 20 "filler" bars at a low of 150 — comfortably above the
// original 56-bar window's low of 100, so none of them breach, and after 20
// of them the Exit Channel's whole window is exactly 20x150 — followed by
// one bar at a low of 70, strictly the new minimum.
func TestLookAheadExitChannelWouldMissTheBreach(t *testing.T) {
	t.Parallel()

	const exitChannelLength = 20
	const fillerLow = 150.0
	const breachLow = 70.0

	lows := make([]float64, 0, exitChannelLength+1)
	for i := 0; i < exitChannelLength; i++ {
		lows = append(lows, fillerLow)
	}
	lows = append(lows, breachLow)

	// lookAheadBreaches reproduces the bug entirely locally: for each low it
	// adds the CURRENT bar's low to the window BEFORE computing the channel
	// low, so the channel always includes the bar it is supposedly
	// deciding. It does not call anything in internal/indicator or
	// internal/strategy, so this test cannot pass by accidentally
	// exercising the production code twice.
	lookAheadBreaches := func(lows []float64, length int) int {
		var window []float64
		breaches := 0
		for _, low := range lows {
			window = append(window, low) // the bug: added before the read
			if len(window) < length {
				continue
			}
			start := len(window) - length
			channelLow := window[start]
			for _, v := range window[start:] {
				if v < channelLow {
					channelLow = v
				}
			}
			if low < channelLow {
				breaches++
			}
		}
		return breaches
	}

	if got := lookAheadBreaches(lows, exitChannelLength); got != 0 {
		t.Fatalf("look-ahead channel computed %d breach(es) on the fixture, want 0 (a channel that already contains the bar's own low can never be fallen below by it)", got)
	}

	// The production reducer, on the identical shape, must find exactly one:
	// this confirms the two are genuinely opposite outcomes on the same
	// input, and pins the defence (evaluate-then-add ordering,
	// indicator.ExitChannel) against regressing back to the look-ahead
	// shape above.
	cfg := validConfigurationPayload()
	var bars []event.CompletedBarPayload
	for i, low := range lows {
		bars = append(bars, postEntryBar("AAPL", day(57+i), low))
	}

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bars(bars).
		mustRun()

	if got := countFor(t, emitted, event.ExitProposalEventType, "AAPL"); got != 1 {
		t.Fatalf("production reducer emitted %d exit proposal(s), want exactly 1 (the exclude-the-decision-bar exit channel correctly finds the breach)", got)
	}

	proposal := decodeExitProposal(t, onlyEnvelopeOfType(t, emitted, event.ExitProposalEventType))
	if proposal.Level != fillerLow {
		t.Errorf("exit proposal Level = %v, want %v (the 20 filler bars' low, excluding the decision bar's own %v)", proposal.Level, fillerLow, breachLow)
	}
}

// --- The exit fill's own execution window (PR #73 review round) ----------
//
// The entry-fill path bounds a fill's timestamp to
// (the period end of the bar before the decision bar, the period end of the
// next bar for that instrument] — see Reducer.applyFill's own doc comment
// for why (ADR 0005's resting order and the two places the bounds are
// enforced). An exit fill needs the identical bound against the BREACH bar
// (the bar whose evaluation produced the outstanding exit proposal) rather
// than the entry's decision bar, for the same reason: a fill claiming to
// have executed before the order for it could have existed, or after a bar
// the stream has not reached yet, would journal a Campaign-exited event
// whose ExitedAt is inconsistent with the bar stream. Every test below is
// the exit-side mirror of one of applyFill's own window tests in
// campaign_test.go.

// TestExitFillInsideTheBreachBarClosesTheCampaignAtThatIntrabarTime mirrors
// TestFillInsideTheDecisionBarOpensTheCampaignAtThatIntrabarTime: a fill
// timestamped strictly inside the breach bar (after the previous bar's
// period end, before the breach bar's own) is accepted, and ExitedAt carries
// that intrabar time — ADR 0005's ordinary case.
func TestExitFillInsideTheBreachBarClosesTheCampaignAtThatIntrabarTime(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	breachAt := day(57)
	intrabar := breachAt.Add(-6 * time.Hour)
	exitFill := closingExitFill("AAPL", campaignID, 100, breachAt, intrabar)

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(postEntryBar("AAPL", breachAt, 99)).
		fill(exitFill).
		mustRun()

	exited := decodeCampaignExited(t, onlyEnvelopeOfType(t, emitted, event.CampaignExitedEventType))
	if !exited.ExitedAt.Equal(intrabar) {
		t.Errorf("ExitedAt = %v, want the intrabar fill time %v", exited.ExitedAt, intrabar)
	}
}

// TestExitFillAtTheBreachBarsPeriodEndIsAccepted mirrors
// TestFillAtTheDecisionBarsPeriodEndIsAccepted: a fill at the very close of
// the breach bar is still a fill that happened within that bar.
func TestExitFillAtTheBreachBarsPeriodEndIsAccepted(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	breachAt := day(57)
	exitFill := closingExitFill("AAPL", campaignID, 100, breachAt, breachAt)

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(postEntryBar("AAPL", breachAt, 99)).
		fill(exitFill).
		mustRun()

	if got := len(envelopesOfType(emitted, event.CampaignExitedEventType)); got != 1 {
		t.Fatalf("got %d Campaign-exited event(s), want exactly 1", got)
	}
}

// TestExitFillPredatingTheBarTheExitOrderCouldHaveExecutedInIsRejected
// mirrors TestFillPredatingTheBarTheOrderCouldHaveExecutedInIsRejected: a
// fill timestamped at or before the bar BEFORE the breach bar (day 56, the
// moment the breach bar opened) predates any order the breach could have
// produced, and is rejected — the lower bound of the exit fill's window.
func TestExitFillPredatingTheBarTheExitOrderCouldHaveExecutedInIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	breachAt := day(57)

	tests := []struct {
		name     string
		filledAt time.Time
	}{
		{
			// Exactly the previous bar's period end: the breach bar had not
			// opened yet, so the boundary is exclusive.
			name:     "at the previous bar's period end",
			filledAt: day(56),
		},
		{
			name:     "long before the breach bar existed",
			filledAt: day(1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			exitFill := closingExitFill("AAPL", campaignID, 100, breachAt, tt.filledAt)

			newStream(t, cfg).
				bars(breakoutBars("AAPL")).
				fill(openingFill("AAPL")).
				bar(postEntryBar("AAPL", breachAt, 99)).
				fill(exitFill).
				wantRunError("AAPL", "predates")
		})
	}
}

// TestBarPredatingTheCampaignsClosingFillFailsClosed mirrors
// TestBarPredatingTheCampaignsOpeningFillFailsClosed on the closing side: a
// fill claiming a moment the stream has not reached yet cannot be rejected
// when it arrives (there is no bar-length configuration, and the next bar
// does not exist yet), so it is accepted then — and the contradiction is
// caught by the very next bar for that instrument, which fails the run
// rather than continuing as though nothing were wrong. Both halves are
// asserted, for the same reason the entry-side test asserts both: the point
// is the split between "accepted at fill time" and "caught at the next bar".
func TestBarPredatingTheCampaignsClosingFillFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	breachAt := day(57)
	future := day(59)
	exitFill := closingExitFill("AAPL", campaignID, 100, breachAt, future)
	nextBar := postEntryBar("AAPL", day(58), 110)

	s := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(postEntryBar("AAPL", breachAt, 99)).
		fill(exitFill)

	// The fill alone is accepted: nothing about it is knowable as wrong at
	// the moment it arrives.
	accepted := s.mustRun()
	if got := len(envelopesOfType(accepted, event.CampaignExitedEventType)); got != 1 {
		t.Fatalf("got %d Campaign-exited event(s) from the fill alone, want exactly 1", got)
	}

	// The next bar's own period end (58) is BEFORE the closing fill's
	// timestamp (59): an execution cannot have happened after a bar that had
	// not yet completed, so the bar stream is now inconsistent with the fill
	// it already accepted.
	s.bar(nextBar).wantRunError("AAPL", "predates")
}
