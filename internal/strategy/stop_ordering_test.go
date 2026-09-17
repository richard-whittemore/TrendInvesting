package strategy_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// A Campaign reports one Protective Stop: the minimum across its held Units,
// the level at which its protection is FIRST breached. Every Unit's stop sits
// the same distance below its own fill, Adds rest half an N above the
// previous fill, and the Stop Ladder raises earlier Units by exactly half an
// N — so on paper the minimum is always Unit 1's, and the scan across the
// other Units can never find anything lower.
//
// It is not always Unit 1's. The two levels being compared are the same three
// terms added in a different order:
//
//	the new Unit's own stop   (previousFill + halfN) - stopDistance
//	an earlier Unit's, raised (previousFill - stopDistance) + halfN
//
// Float64 addition is not associative, so these differ by one unit in the
// last place for a great many ordinary prices, and when they differ it is the
// NEWEST Unit that is lower. The fixture below is the Baseline's own N with
// an entry at 199.65 and four Units each filling exactly on its rung, which
// is the undisturbed case Faith prints — no gap, no skid.
//
// The consequence is small and the classification is not: the scan is live
// code on ordinary inputs, not a defensive loop that cannot execute, and a
// comment claiming a Campaign's stop is "always Unit 1's own" would be wrong
// in the last bits.

// exactRungEntryPrice is an entry price for which the arithmetic above
// inverts by the fourth Unit. Prices for which it does not are equally
// ordinary — 201.25, the price every other fixture in this package uses, is
// one — which is exactly why the ordering cannot be assumed either way.
const exactRungEntryPrice = 199.65

// openingFillAt is openingFill at a chosen price, for the fixtures whose
// whole point is which price the ladder is measured from.
func openingFillAt(instrumentID string, price float64) event.FillPayload {
	fill := openingFill(instrumentID)
	fill.Price = price
	return fill
}

type exactRungFixture struct {
	campaignID string
	campaignN  float64
	unitFillID [4]string
	stream     *stream
}

// buildExactRungCampaign opens a Campaign at exactRungEntryPrice and adds
// three more Units, each filling EXACTLY on its own rung — the ladder as
// printed, with no slippage anywhere. Every rung comes from
// sizing.NextAddLevel, so the fixture cannot drift from the arithmetic it
// exercises.
func buildExactRungCampaign(t *testing.T) exactRungFixture {
	t.Helper()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)

	nextRung := func(previousFill float64) float64 {
		t.Helper()
		rung, err := sizing.NextAddLevel(previousFill, campaignN, sizing.DirectionLong)
		if err != nil {
			t.Fatalf("NextAddLevel(%v) error = %v", previousFill, err)
		}
		return rung
	}

	rung2 := nextRung(exactRungEntryPrice)
	rung3 := nextRung(rung2)
	rung4 := nextRung(rung3)

	s := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFillAt("AAPL", exactRungEntryPrice)).
		bar(addOpportunityBar("AAPL", day(57), rung2+5)).
		fill(addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", rung2, 133, day(57))).
		bar(addOpportunityBar("AAPL", day(58), rung3+5)).
		fill(addFill("AAPL", campaignID, 3, day(58), "sim-fill-add-3", rung3, 133, day(58))).
		bar(addOpportunityBar("AAPL", day(59), rung4+5)).
		fill(addFill("AAPL", campaignID, 4, day(59), "sim-fill-add-4", rung4, 133, day(59)))

	return exactRungFixture{
		campaignID: campaignID,
		campaignN:  campaignN,
		unitFillID: [4]string{"sim-fill-0001", "sim-fill-add-2", "sim-fill-add-3", "sim-fill-add-4"},
		stream:     s,
	}
}

// TestACampaignsProtectiveStopIsNotAlwaysTheFirstUnitsOwn is the scan across
// held Units, reached by the arithmetic rather than by a corrupted fixture:
// the newest Unit's stop lands one unit in the last place BELOW the earlier
// Units' raised stops, and the Campaign reports that lower level.
func TestACampaignsProtectiveStopIsNotAlwaysTheFirstUnitsOwn(t *testing.T) {
	t.Parallel()

	fixture := buildExactRungCampaign(t)
	emitted := fixture.stream.
		bar(addOpportunityBar("AAPL", day(60), 300)).
		mustRun()

	evaluations := envelopesOfType(emitted, event.CampaignEvaluatedEventType)
	if len(evaluations) == 0 {
		t.Fatal("the run emitted no campaign-evaluated event")
	}
	var evaluated event.CampaignEvaluatedPayload
	decodeEnvelopePayload(t, evaluations[len(evaluations)-1], &evaluated)

	if len(evaluated.Units) != 4 {
		t.Fatalf("the campaign holds %d unit(s), want 4", len(evaluated.Units))
	}
	first, last := evaluated.Units[0].ProtectiveStop, evaluated.Units[3].ProtectiveStop
	if last >= first {
		t.Fatalf("unit 4's stop is %v and unit 1's is %v; this fixture exists because the newest unit's is strictly lower, and the arithmetic it depends on has changed", last, first)
	}
	if evaluated.ProtectiveStop != last {
		t.Errorf("ProtectiveStop = %v, want the minimum across the units, %v (unit 4's own)", evaluated.ProtectiveStop, last)
	}
	if err := evaluated.Validate(); err != nil {
		t.Errorf("the emitted payload fails its own Validate(): %v", err)
	}
}

// TestAStopFillRecordsTheLowestLevelAcrossTheUnitsItCloses is the same scan
// on the closing side. The level recorded is the one that was in force for
// the Units this fill actually names — never the Campaign's own, which may
// belong to a Unit the fill does not close.
func TestAStopFillRecordsTheLowestLevelAcrossTheUnitsItCloses(t *testing.T) {
	t.Parallel()

	fixture := buildExactRungCampaign(t)
	emitted := fixture.stream.
		bar(addOpportunityBar("AAPL", day(60), 300)).
		fill(stopFillForUnits("AAPL", fixture.campaignID, "sim-fill-stop-all",
			fixture.unitFillID[:], 180, 4*133, day(60))).
		mustRun()

	exits := envelopesOfType(emitted, event.CampaignExitedEventType)
	if len(exits) != 1 {
		t.Fatalf("got %d campaign-exited event(s), want exactly 1", len(exits))
	}
	exited := decodeCampaignExited(t, exits[0])

	evaluations := envelopesOfType(emitted, event.CampaignEvaluatedEventType)
	var evaluated event.CampaignEvaluatedPayload
	decodeEnvelopePayload(t, evaluations[len(evaluations)-1], &evaluated)

	if exited.ProtectiveStopLevel != evaluated.Units[3].ProtectiveStop {
		t.Errorf("ProtectiveStopLevel = %v, want the lowest across the four units it closes, %v", exited.ProtectiveStopLevel, evaluated.Units[3].ProtectiveStop)
	}
	if exited.ProtectiveStopLevel >= evaluated.Units[0].ProtectiveStop {
		t.Errorf("ProtectiveStopLevel = %v is not below unit 1's %v; the scan across the closing units did nothing", exited.ProtectiveStopLevel, evaluated.Units[0].ProtectiveStop)
	}
}

// TestAStopFillPredatingTheCampaignsOwnOpeningFillIsRejected: a Campaign
// cannot be closed before it opened. The reducer polices a closing fill's
// timestamp on both sides, and this is the earlier side — without it a fill
// dated before the entry would be accepted and recorded as the Campaign's
// exit, producing a Campaign whose life runs backwards.
func TestAStopFillPredatingTheCampaignsOwnOpeningFillIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	stopLevel := campaignFillPrice - float64(cfg.StopMultiple*campaignN)

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(addOpportunityBar("AAPL", day(57), 210)).
		fill(stopFillForUnits("AAPL", campaignID, "sim-fill-stop-early",
			[]string{"sim-fill-0001"}, stopLevel-1, 133, day(56).Add(-time.Hour))).
		wantRunError("predates campaign", "cannot be closed before it opened")
}

// decodeEnvelopePayload decodes an emitted envelope's payload, failing the
// test rather than returning an error: a fixture that cannot read what the
// reducer emitted has nothing to assert.
func decodeEnvelopePayload(t *testing.T, envelope event.Envelope, into any) {
	t.Helper()
	if err := json.Unmarshal(envelope.Payload, into); err != nil {
		t.Fatalf("decode %s payload: %v", envelope.Type, err)
	}
}
