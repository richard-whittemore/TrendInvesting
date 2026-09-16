package strategy_test

import (
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// A fill names the Campaign it belongs to, and the reducer holds exactly one
// open Campaign per instrument. When the two disagree there is no safe
// reading: the fill belongs to a position this strategy is not running, and
// applying it to the Campaign that happens to be open would close or enlarge
// the wrong position. These are reconciliation failures
// (docs/architecture.md), distinct from the already-covered case of a fill
// arriving when no Campaign is open at all — that one says the position is
// gone, this one says the books disagree about which position it is.

// strayCampaignID is a Campaign this reducer never opened, in the same shape
// the reducer's own ids take so nothing is rejected merely for being
// malformed.
const strayCampaignID = "campaign:AAPL:1999-01-01T00:00:00.000000000Z"

func TestAnExitFillNamingADifferentCampaignThanTheOpenOneIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	breachAt := day(57)

	stray := event.FillPayload{
		InstrumentID: "AAPL",
		Kind:         event.FillKindExit,
		CampaignID:   strayCampaignID,
		ProposalID:   exitProposalID("AAPL", breachAt),
		FillID:       "sim-fill-exit-stray",
		Direction:    event.DirectionLong,
		Quantity:     133,
		Price:        99.5,
		FilledAt:     day(58),
	}

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(postEntryBar("AAPL", breachAt, 99)).
		fill(stray).
		wantRunError("exit fill", strayCampaignID, "but the open campaign is", "reconciliation failure")
}

func TestAnAddFillNamingADifferentCampaignThanTheOpenOneIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignN := breakoutFixtureN(t, cfg)
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}

	stray := addFill("AAPL", strayCampaignID, 2, day(57), "sim-fill-add-stray", rung2, 133, day(57))

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(addOpportunityBar("AAPL", day(57), rung2+5)).
		fill(stray).
		wantRunError("add fill", strayCampaignID, "but the open campaign is", "reconciliation failure")
}

// TestAnExitFillPredatingAnEarlierClosingFillIsRejected is the chronology
// rule between two closing fills of the same Campaign. The rule against a
// closing fill predating the Campaign's OPEN says nothing about the order of
// a second closing fill against the first, so without this an exit fill
// delivered after a partial stop but timestamped before it would be
// accepted, leaving the Campaign's own closing history non-chronological —
// and the whole-life exit price is a quantity-weighted average across both
// fills, so the order they executed in is not a presentational detail.
//
// Equal timestamps stay allowed: several Units can close at the same instant
// (The Turtle Rules p.19's "all four could be added in one day", mirrored
// here for closing).
func TestAnExitFillPredatingAnEarlierClosingFillIsRejected(t *testing.T) {
	t.Parallel()

	stream, fixture := buildGapCampaign(t)

	stoppedAt := day(60)
	stop4 := stopFillForUnits("AAPL", fixture.campaignID, "sim-fill-stop-4",
		[]string{fixture.unit4FillID}, fixture.unit4Stop-0.37, 133, stoppedAt)

	breachAt := day(61)
	backdated := event.FillPayload{
		InstrumentID: "AAPL",
		Kind:         event.FillKindExit,
		CampaignID:   fixture.campaignID,
		ProposalID:   exitProposalID("AAPL", breachAt),
		FillID:       "sim-fill-exit-backdated",
		Direction:    event.DirectionLong,
		Quantity:     399,
		Price:        99.5,
		// After the Campaign opened, and before the stop fill that has
		// already closed part of it.
		FilledAt: stoppedAt.Add(-time.Hour),
	}

	stream.
		fill(stop4).
		bar(postEntryBar("AAPL", breachAt, 99)).
		fill(backdated).
		wantRunError("exit fill", "predates campaign", "most recently accepted closing fill",
			"a later closing fill cannot have executed before an earlier one")
}

// TestAnAddFillThatWouldLeaveTheNewUnitUnprotectedIsRejected is the same rule
// the opening fill is held to, applied to a later Unit: a Unit whose stop
// would sit at or below zero is not protected in fact however protected it
// looks in the journal. The Add Ladder measures the stop from the Unit's own
// actual fill, so a fill reported far below the rung it was resting at —
// a reconciliation failure on the broker's side, not a market move — is
// exactly the input that produces one.
func TestAnAddFillThatWouldLeaveTheNewUnitUnprotectedIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}

	// Positive, and less than the 2N the stop sits below it.
	unprotected := float64(cfg.StopMultiple*campaignN) - 1
	fill := addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-unprotected", unprotected, 133, day(57))

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(addOpportunityBar("AAPL", day(57), rung2+5)).
		fill(fill).
		wantRunError("add fill", "cannot compute a protective stop", "not positive")
}
