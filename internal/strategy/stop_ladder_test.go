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

// This file holds #15's own event-seam tests: the Stop Ladder (The Turtle
// Rules p.22-23), including the gap case, and the per-Unit stop fill it
// requires. Every fixture below builds on #11/#12/#13/#14's
// breakoutBars/openingFill/addFill fixtures (campaign_test.go, add_test.go),
// which open a 133-share AAPL Campaign at day(56) with campaignN ==
// breakoutFixtureN(t, cfg) and campaignFillPrice == 201.25.
//
// stopLadderEpsilon mirrors internal/sizing/stop_test.go's crudeEpsilon: two
// mathematically-equal stop levels reached via different chains of RaisedStop
// calls (or a fresh sizing.ProtectiveStopLevel call) need not be bit-identical
// float64 values, so comparisons against a hand-derived nominal level use a
// small tolerance rather than exact equality — see that file's own doc
// comment for why.
const stopLadderEpsilon = 1e-9

func decodeCampaignUnitsStopped(t *testing.T, envelope event.Envelope) event.CampaignUnitsStoppedPayload {
	t.Helper()
	if envelope.Type != event.CampaignUnitsStoppedEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.CampaignUnitsStoppedEventType)
	}
	var payload event.CampaignUnitsStoppedPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload
}

// stopFillForUnits builds a stop-kind fill closing exactly the named Units
// (#15) of campaignID.
func stopFillForUnits(instrumentID, campaignID, fillID string, unitIDs []string, price float64, quantity int64, filledAt time.Time) event.FillPayload {
	return event.FillPayload{
		InstrumentID: instrumentID,
		Kind:         event.FillKindStop,
		CampaignID:   campaignID,
		FillID:       fillID,
		UnitIDs:      unitIDs,
		Direction:    event.DirectionLong,
		Quantity:     quantity,
		Price:        price,
		FilledAt:     filledAt,
	}
}

// gapCampaignFixture is the ticket's own Crude-shaped four-Unit Campaign,
// with the fourth Unit filling well past its own rung — a large slip
// standing in for The Turtle Rules p.23's opening-gap fourth Unit, in this
// fixture's own (non-1.20) campaignN, exactly as add_test.go's own fixtures
// use a slip to prove "measured from the actual fill" rather than hand-typing
// Faith's literal Crude numbers into a fixture whose N is a different value.
type gapCampaignFixture struct {
	cfg        event.ConfigurationPayload
	campaignID string
	campaignN  float64

	unit1FillID, unit2FillID, unit3FillID, unit4FillID string
	unit1Fill, unit2Fill, unit3Fill, unit4Fill         float64

	// unit1Stop/unit2Stop/unit3Stop/unit4Stop are each Unit's own stop
	// level once all four Units exist: units 1-3 raised the STANDARD 1/2N
	// per Add regardless of unit 4's own gap, and unit 4's own is computed
	// fresh from its own (gapped) fill — The Turtle Rules p.23's own
	// distinction.
	unit1Stop, unit2Stop, unit3Stop, unit4Stop float64
}

// buildGapCampaign returns the stream builder positioned right after all
// four Units have been added (the last one gapped), plus every figure a
// test needs to assert against, each derived from the SAME production
// functions (sizing.NextAddLevel/ProtectiveStopLevel/RaisedStop) the reducer
// itself uses — never hand-typed — so a fixture can never silently drift
// from the arithmetic it exercises.
func buildGapCampaign(t *testing.T) (*stream, gapCampaignFixture) {
	t.Helper()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)

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
	// The gap: a slip roughly 20x the size of units 2/3's own (0.06/0.05),
	// large enough that no reasonable float64 tolerance could mistake it
	// for "on the rung" — The Turtle Rules p.23's own Crude gap (30.10 ->
	// 30.80) is itself about 20x the ladder's own 0.60 spacing.
	gapFill4Price := rung4 + 20*campaignN

	bar57 := addOpportunityBar("AAPL", day(57), rung2+5)
	fill2 := addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", fill2Price, 133, day(57))
	bar58 := addOpportunityBar("AAPL", day(58), rung3+5)
	fill3 := addFill("AAPL", campaignID, 3, day(58), "sim-fill-add-3", fill3Price, 133, day(58))
	bar59 := addOpportunityBar("AAPL", day(59), gapFill4Price+5)
	fill4 := addFill("AAPL", campaignID, 4, day(59), "sim-fill-add-4", gapFill4Price, 133, day(59))

	s := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(bar57).
		fill(fill2).
		bar(bar58).
		fill(fill3).
		bar(bar59).
		fill(fill4)

	stop1_0, err := sizing.ProtectiveStopLevel(campaignFillPrice, campaignN, cfg.StopMultiple, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("ProtectiveStopLevel(unit 1) error = %v", err)
	}
	stop1_1, err := sizing.RaisedStop(stop1_0, campaignN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	stop2_0, err := sizing.ProtectiveStopLevel(fill2Price, campaignN, cfg.StopMultiple, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("ProtectiveStopLevel(unit 2) error = %v", err)
	}
	stop1_2, err := sizing.RaisedStop(stop1_1, campaignN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	stop2_1, err := sizing.RaisedStop(stop2_0, campaignN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	stop3_0, err := sizing.ProtectiveStopLevel(fill3Price, campaignN, cfg.StopMultiple, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("ProtectiveStopLevel(unit 3) error = %v", err)
	}
	stop1_3, err := sizing.RaisedStop(stop1_2, campaignN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	stop2_2, err := sizing.RaisedStop(stop2_1, campaignN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	stop3_1, err := sizing.RaisedStop(stop3_0, campaignN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	stop4_0, err := sizing.ProtectiveStopLevel(gapFill4Price, campaignN, cfg.StopMultiple, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("ProtectiveStopLevel(unit 4) error = %v", err)
	}

	return s, gapCampaignFixture{
		cfg:         cfg,
		campaignID:  campaignID,
		campaignN:   campaignN,
		unit1FillID: "sim-fill-0001",
		unit2FillID: "sim-fill-add-2",
		unit3FillID: "sim-fill-add-3",
		unit4FillID: "sim-fill-add-4",
		unit1Fill:   campaignFillPrice,
		unit2Fill:   fill2Price,
		unit3Fill:   fill3Price,
		unit4Fill:   gapFill4Price,
		unit1Stop:   stop1_3,
		unit2Stop:   stop2_2,
		unit3Stop:   stop3_1,
		unit4Stop:   stop4_0,
	}
}

// --- The normal ladder: a stop-moved event per earlier Unit, each Add -----

// TestStopLadderRaisesEveryEarlierUnitByHalfNOnEachAdd is the ticket's
// primary event-seam test: after each Add fill, every EARLIER Unit's stop
// rises by exactly RaisedStop(previous, campaignN) — never a fresh 2N below
// the newest fill — and the per-bar Campaign-evaluated event's aggregate
// open risk matches the hand-derived figure at each rung count.
func TestStopLadderRaisesEveryEarlierUnitByHalfNOnEachAdd(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)

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

	bar57 := addOpportunityBar("AAPL", day(57), rung2+5)
	fill2 := addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", fill2Price, 133, day(57))
	bar58 := addOpportunityBar("AAPL", day(58), rung3+5)
	fill3 := addFill("AAPL", campaignID, 3, day(58), "sim-fill-add-3", fill3Price, 133, day(58))
	bar59 := addOpportunityBar("AAPL", day(59), rung4+5)
	fill4 := addFill("AAPL", campaignID, 4, day(59), "sim-fill-add-4", fill4Price, 133, day(59))
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

	stopSets := envelopesOfType(emitted, event.ProtectiveStopSetEventType)
	// 1 (unit 1's own, from the opening fill) + [1 initial + 1 raise] (add 2)
	// + [1 initial + 2 raises] (add 3) + [1 initial + 3 raises] (add 4) = 10.
	if len(stopSets) != 10 {
		t.Fatalf("got %d protective-stop-set event(s), want exactly 10", len(stopSets))
	}

	stop1_0, err := sizing.ProtectiveStopLevel(campaignFillPrice, campaignN, cfg.StopMultiple, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("ProtectiveStopLevel() error = %v", err)
	}

	// index 0: unit 1's own initial stop, from the opening fill.
	unit1Initial := decodeProtectiveStopSet(t, stopSets[0])
	if unit1Initial.UnitIndex != 1 {
		t.Errorf("stopSets[0].UnitIndex = %d, want 1", unit1Initial.UnitIndex)
	}
	if unit1Initial.Reason != event.ProtectiveStopReasonInitial {
		t.Errorf("stopSets[0].Reason = %q, want %q", unit1Initial.Reason, event.ProtectiveStopReasonInitial)
	}
	if unit1Initial.PreviousLevel != 0 {
		t.Errorf("stopSets[0].PreviousLevel = %v, want 0", unit1Initial.PreviousLevel)
	}

	// indices 1-2: add 2's own [unit 2 initial, unit 1 raise].
	unit2Initial := decodeProtectiveStopSet(t, stopSets[1])
	if unit2Initial.UnitIndex != 2 || unit2Initial.Reason != event.ProtectiveStopReasonInitial {
		t.Errorf("stopSets[1] = {UnitIndex:%d, Reason:%q}, want {2, %q}", unit2Initial.UnitIndex, unit2Initial.Reason, event.ProtectiveStopReasonInitial)
	}
	unit1RaiseAt2 := decodeProtectiveStopSet(t, stopSets[2])
	if unit1RaiseAt2.UnitIndex != 1 || unit1RaiseAt2.Reason != event.ProtectiveStopReasonAddLadder {
		t.Errorf("stopSets[2] = {UnitIndex:%d, Reason:%q}, want {1, %q}", unit1RaiseAt2.UnitIndex, unit1RaiseAt2.Reason, event.ProtectiveStopReasonAddLadder)
	}
	if unit1RaiseAt2.PreviousLevel != unit1Initial.Level {
		t.Errorf("stopSets[2].PreviousLevel = %v, want unit 1's initial level %v", unit1RaiseAt2.PreviousLevel, unit1Initial.Level)
	}
	wantUnit1RaisedAt2, err := sizing.RaisedStop(stop1_0, campaignN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	if unit1RaiseAt2.Level != wantUnit1RaisedAt2 {
		t.Errorf("stopSets[2].Level = %v, want exactly %v (RaisedStop applied to the SAME PreviousLevel)", unit1RaiseAt2.Level, wantUnit1RaisedAt2)
	}
	if err := unit1RaiseAt2.Validate(); err != nil {
		t.Errorf("stopSets[2] fails its own Validate(): %v", err)
	}

	// indices 3-5: add 3's own [unit 3 initial, unit 1 raise, unit 2 raise].
	unit3Initial := decodeProtectiveStopSet(t, stopSets[3])
	if unit3Initial.UnitIndex != 3 || unit3Initial.Reason != event.ProtectiveStopReasonInitial {
		t.Errorf("stopSets[3] = {UnitIndex:%d, Reason:%q}, want {3, %q}", unit3Initial.UnitIndex, unit3Initial.Reason, event.ProtectiveStopReasonInitial)
	}
	unit1RaiseAt3 := decodeProtectiveStopSet(t, stopSets[4])
	if unit1RaiseAt3.UnitIndex != 1 || unit1RaiseAt3.Reason != event.ProtectiveStopReasonAddLadder {
		t.Errorf("stopSets[4] = {UnitIndex:%d, Reason:%q}, want {1, %q}", unit1RaiseAt3.UnitIndex, unit1RaiseAt3.Reason, event.ProtectiveStopReasonAddLadder)
	}
	if unit1RaiseAt3.PreviousLevel != unit1RaiseAt2.Level {
		t.Errorf("stopSets[4].PreviousLevel = %v, want unit 1's PRIOR raised level %v (raises compound, never recomputed from scratch)", unit1RaiseAt3.PreviousLevel, unit1RaiseAt2.Level)
	}
	unit2RaiseAt3 := decodeProtectiveStopSet(t, stopSets[5])
	if unit2RaiseAt3.UnitIndex != 2 || unit2RaiseAt3.Reason != event.ProtectiveStopReasonAddLadder {
		t.Errorf("stopSets[5] = {UnitIndex:%d, Reason:%q}, want {2, %q}", unit2RaiseAt3.UnitIndex, unit2RaiseAt3.Reason, event.ProtectiveStopReasonAddLadder)
	}
	if unit2RaiseAt3.PreviousLevel != unit2Initial.Level {
		t.Errorf("stopSets[5].PreviousLevel = %v, want unit 2's initial level %v", unit2RaiseAt3.PreviousLevel, unit2Initial.Level)
	}

	// These fills are deliberately SLIPPED above their own rungs (0.06,
	// 0.05, 0.04 — add_test.go's own house style, proving the ladder is
	// measured from the ACTUAL fill rather than the intended rung), so
	// unlike the exact-rung Crude table, units 1-3 do NOT converge on one
	// shared level here: each carries its own accumulated slip. Checked
	// instead against the SAME chained RaisedStop/ProtectiveStopLevel
	// sequence this test already used to build wantUnit1RaisedAt2, for
	// EXACT float64 equality (both this test and the reducer perform the
	// identical sequence of operations).
	wantUnit1RaisedAt3, err := sizing.RaisedStop(wantUnit1RaisedAt2, campaignN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	if unit1RaiseAt3.Level != wantUnit1RaisedAt3 {
		t.Errorf("stopSets[4].Level = %v, want exactly %v", unit1RaiseAt3.Level, wantUnit1RaisedAt3)
	}
	wantUnit2RaisedAt3, err := sizing.RaisedStop(unit2Initial.Level, campaignN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	if unit2RaiseAt3.Level != wantUnit2RaisedAt3 {
		t.Errorf("stopSets[5].Level = %v, want exactly %v", unit2RaiseAt3.Level, wantUnit2RaisedAt3)
	}

	// indices 6-9: add 4's own [unit 4 initial, unit 1 raise, unit 2 raise, unit 3 raise].
	unit4Initial := decodeProtectiveStopSet(t, stopSets[6])
	if unit4Initial.UnitIndex != 4 || unit4Initial.Reason != event.ProtectiveStopReasonInitial {
		t.Errorf("stopSets[6] = {UnitIndex:%d, Reason:%q}, want {4, %q}", unit4Initial.UnitIndex, unit4Initial.Reason, event.ProtectiveStopReasonInitial)
	}
	unit1RaiseAt4 := decodeProtectiveStopSet(t, stopSets[7])
	unit2RaiseAt4 := decodeProtectiveStopSet(t, stopSets[8])
	unit3RaiseAt4 := decodeProtectiveStopSet(t, stopSets[9])
	for i, want := range []struct {
		envelope  event.ProtectiveStopSetPayload
		unitIndex int
	}{
		{unit1RaiseAt4, 1},
		{unit2RaiseAt4, 2},
		{unit3RaiseAt4, 3},
	} {
		if want.envelope.UnitIndex != want.unitIndex || want.envelope.Reason != event.ProtectiveStopReasonAddLadder {
			t.Errorf("raise[%d] = {UnitIndex:%d, Reason:%q}, want {%d, %q}", i, want.envelope.UnitIndex, want.envelope.Reason, want.unitIndex, event.ProtectiveStopReasonAddLadder)
		}
	}
	// Each earlier unit's own fourth-round raise, checked against the SAME
	// chained formula, for exact equality (see the three-unit block above
	// for why these slipped fills do not converge on one shared level).
	wantUnit1RaisedAt4, err := sizing.RaisedStop(wantUnit1RaisedAt3, campaignN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	if unit1RaiseAt4.Level != wantUnit1RaisedAt4 {
		t.Errorf("stopSets[7].Level = %v, want exactly %v", unit1RaiseAt4.Level, wantUnit1RaisedAt4)
	}
	wantUnit2RaisedAt4, err := sizing.RaisedStop(wantUnit2RaisedAt3, campaignN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	if unit2RaiseAt4.Level != wantUnit2RaisedAt4 {
		t.Errorf("stopSets[8].Level = %v, want exactly %v", unit2RaiseAt4.Level, wantUnit2RaisedAt4)
	}
	wantUnit3RaisedAt4, err := sizing.RaisedStop(unit3Initial.Level, campaignN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	if unit3RaiseAt4.Level != wantUnit3RaisedAt4 {
		t.Errorf("stopSets[9].Level = %v, want exactly %v", unit3RaiseAt4.Level, wantUnit3RaisedAt4)
	}

	// The per-bar aggregate open risk, at each rung count, hand-derived from
	// the SAME production functions and compared for EXACT equality (the
	// reducer's own aggregateOpenRisk and this test replicate the identical
	// sequence of RaisedStop/ProtectiveStopLevel calls, so the two floats
	// are produced by the identical operations, not merely equal in value).
	evaluated := envelopesOfType(emitted, event.CampaignEvaluatedEventType)
	// One per bar from day(57) onward (day(56) itself has no evaluated
	// event: the campaign only opens ON that bar's fill, not before it).
	wantEvaluatedCount := 4 // bars 57, 58, 59, 60
	if len(evaluated) != wantEvaluatedCount {
		t.Fatalf("got %d campaign-evaluated event(s), want %d", len(evaluated), wantEvaluatedCount)
	}

	// bar58's own evaluated event reflects the state AFTER fill2 (2 units).
	twoUnitAggregate, err := sizing.AggregateOpenRisk([]sizing.UnitOpenRisk{
		{EntryPrice: campaignFillPrice, ProtectiveStop: unit1RaiseAt2.Level, Quantity: 133},
		{EntryPrice: fill2Price, ProtectiveStop: unit2Initial.Level, Quantity: 133},
	}, cfg.DollarsPerPoint)
	if err != nil {
		t.Fatalf("AggregateOpenRisk(2 units) error = %v", err)
	}
	bar58Evaluated := decodeCampaignEvaluated(t, evaluated[1])
	if bar58Evaluated.AggregateOpenRisk != twoUnitAggregate {
		t.Errorf("bar 58 evaluated AggregateOpenRisk = %v, want exactly %v", bar58Evaluated.AggregateOpenRisk, twoUnitAggregate)
	}
	if len(bar58Evaluated.Units) != 2 {
		t.Errorf("bar 58 evaluated Units count = %d, want 2", len(bar58Evaluated.Units))
	}

	// bar59's own evaluated event reflects the state AFTER fill3 (3 units).
	threeUnitAggregate, err := sizing.AggregateOpenRisk([]sizing.UnitOpenRisk{
		{EntryPrice: campaignFillPrice, ProtectiveStop: unit1RaiseAt3.Level, Quantity: 133},
		{EntryPrice: fill2Price, ProtectiveStop: unit2RaiseAt3.Level, Quantity: 133},
		{EntryPrice: fill3Price, ProtectiveStop: unit3Initial.Level, Quantity: 133},
	}, cfg.DollarsPerPoint)
	if err != nil {
		t.Fatalf("AggregateOpenRisk(3 units) error = %v", err)
	}
	bar59Evaluated := decodeCampaignEvaluated(t, evaluated[2])
	if bar59Evaluated.AggregateOpenRisk != threeUnitAggregate {
		t.Errorf("bar 59 evaluated AggregateOpenRisk = %v, want exactly %v", bar59Evaluated.AggregateOpenRisk, threeUnitAggregate)
	}

	// bar60's own evaluated event reflects the state AFTER fill4 (4 units) —
	// and, since the campaign is already at its configured maximum, no add
	// proposal despite this bar's enormous high.
	fourUnitAggregate, err := sizing.AggregateOpenRisk([]sizing.UnitOpenRisk{
		{EntryPrice: campaignFillPrice, ProtectiveStop: unit1RaiseAt4.Level, Quantity: 133},
		{EntryPrice: fill2Price, ProtectiveStop: unit2RaiseAt4.Level, Quantity: 133},
		{EntryPrice: fill3Price, ProtectiveStop: unit3RaiseAt4.Level, Quantity: 133},
		{EntryPrice: fill4Price, ProtectiveStop: unit4Initial.Level, Quantity: 133},
	}, cfg.DollarsPerPoint)
	if err != nil {
		t.Fatalf("AggregateOpenRisk(4 units) error = %v", err)
	}
	bar60Evaluated := decodeCampaignEvaluated(t, evaluated[3])
	if bar60Evaluated.AggregateOpenRisk != fourUnitAggregate {
		t.Errorf("bar 60 evaluated AggregateOpenRisk = %v, want exactly %v", bar60Evaluated.AggregateOpenRisk, fourUnitAggregate)
	}
	if got := len(envelopesOfType(emitted, event.AddProposalEventType)); got != 3 {
		t.Errorf("got %d add proposal(s), want exactly 3 (never a fifth, even on bar 60's huge high)", got)
	}
}

// --- The gap case: the fourth Unit's own gap does not distort the earlier
// --- Units' raise -----------------------------------------------------

// TestStopLadderGapCaseKeepsEarlierUnitsAtTheStandardRaise is the ticket's
// required gap-case test, the one that distinguishes the two readings named
// on sizing.RaisedStop's own doc comment: Units 1-3 raise by the STANDARD
// 1/2N per Add regardless of how far Unit 4's own fill lands from its rung,
// while Unit 4's own stop is computed fresh from ITS OWN (gapped) fill.
func TestStopLadderGapCaseKeepsEarlierUnitsAtTheStandardRaise(t *testing.T) {
	t.Parallel()

	stream, fixture := buildGapCampaign(t)
	// One further bar, after the gapped fourth add, so its own per-bar
	// Campaign-evaluated event reports the aggregate over all four units.
	emitted := stream.
		bar(addOpportunityBar("AAPL", day(60), fixture.unit4Fill+5)).
		mustRun()

	stopSets := envelopesOfType(emitted, event.ProtectiveStopSetEventType)
	if len(stopSets) != 10 {
		t.Fatalf("got %d protective-stop-set event(s), want exactly 10", len(stopSets))
	}

	// index 6: unit 4's own initial stop, from its gapped fill.
	unit4Initial := decodeProtectiveStopSet(t, stopSets[6])
	if math.Abs(unit4Initial.Level-fixture.unit4Stop) > stopLadderEpsilon {
		t.Errorf("unit 4's own stop = %v, want ~%v", unit4Initial.Level, fixture.unit4Stop)
	}

	// indices 7-9: units 1, 2, 3's own raises, triggered by unit 4's add.
	unit1Raise := decodeProtectiveStopSet(t, stopSets[7])
	unit2Raise := decodeProtectiveStopSet(t, stopSets[8])
	unit3Raise := decodeProtectiveStopSet(t, stopSets[9])

	for name, got := range map[string]event.ProtectiveStopSetPayload{
		"unit 1": unit1Raise, "unit 2": unit2Raise, "unit 3": unit3Raise,
	} {
		if got.Reason != event.ProtectiveStopReasonAddLadder {
			t.Errorf("%s.Reason = %q, want %q", name, got.Reason, event.ProtectiveStopReasonAddLadder)
		}
	}

	// The headline assertion: units 1-3 raise to the SAME level as the
	// NON-gap four-unit case would produce (fixture.unit1Stop/2Stop/3Stop,
	// each independently hand-derived in buildGapCampaign from the standard
	// 1/2N raise chain) — NOT unit 4's own gapped level minus 2N, which is
	// what the WRONG reading ("set every stop to 2N below the newest fill")
	// would produce instead.
	wrongReading, err := sizing.ProtectiveStopLevel(fixture.unit4Fill, fixture.campaignN, fixture.cfg.StopMultiple, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("ProtectiveStopLevel() error = %v", err)
	}
	for name, got := range map[string]float64{"unit 1": unit1Raise.Level, "unit 2": unit2Raise.Level, "unit 3": unit3Raise.Level} {
		wantStop := map[string]float64{"unit 1": fixture.unit1Stop, "unit 2": fixture.unit2Stop, "unit 3": fixture.unit3Stop}[name]
		if math.Abs(got-wantStop) > stopLadderEpsilon {
			t.Errorf("%s raised stop = %v, want ~%v (the STANDARD 1/2N raise, unaffected by unit 4's own gap)", name, got, wantStop)
		}
		if math.Abs(got-wrongReading) < 0.1 {
			t.Errorf("%s raised stop %v is suspiciously close to the WRONG reading %v (2N below unit 4's own gapped fill); this fixture must keep the two clearly distinguishable", name, got, wrongReading)
		}
	}
	// And unit 4's own gapped stop must itself differ, clearly, from units
	// 1-3's own (the printed gap table's whole point).
	if math.Abs(unit4Initial.Level-unit1Raise.Level) < 0.1 {
		t.Errorf("unit 4's own stop %v is suspiciously close to unit 1's raised stop %v; the gap case must leave them clearly apart", unit4Initial.Level, unit1Raise.Level)
	}

	// The per-bar aggregate open risk after the gapped fourth add, hand-derived
	// from the SAME four stops this test just decoded.
	wantAggregate, err := sizing.AggregateOpenRisk([]sizing.UnitOpenRisk{
		{EntryPrice: fixture.unit1Fill, ProtectiveStop: unit1Raise.Level, Quantity: 133},
		{EntryPrice: fixture.unit2Fill, ProtectiveStop: unit2Raise.Level, Quantity: 133},
		{EntryPrice: fixture.unit3Fill, ProtectiveStop: unit3Raise.Level, Quantity: 133},
		{EntryPrice: fixture.unit4Fill, ProtectiveStop: unit4Initial.Level, Quantity: 133},
	}, fixture.cfg.DollarsPerPoint)
	if err != nil {
		t.Fatalf("AggregateOpenRisk() error = %v", err)
	}
	evaluatedEnvelopes := envelopesOfType(emitted, event.CampaignEvaluatedEventType)
	lastEvaluated := decodeCampaignEvaluated(t, evaluatedEnvelopes[len(evaluatedEnvelopes)-1])
	if lastEvaluated.AggregateOpenRisk != wantAggregate {
		t.Errorf("gap-case bar's own AggregateOpenRisk = %v, want exactly %v", lastEvaluated.AggregateOpenRisk, wantAggregate)
	}
	if len(lastEvaluated.Units) != 4 {
		t.Fatalf("gap-case bar's own Units count = %d, want 4", len(lastEvaluated.Units))
	}
	// The reported (minimum) ProtectiveStop is unit 1's own (the smallest of
	// the four once the gap has separated unit 4's level from the rest).
	if math.Abs(lastEvaluated.ProtectiveStop-unit1Raise.Level) > stopLadderEpsilon {
		t.Errorf("gap-case bar's own ProtectiveStop = %v, want ~%v (unit 1's own, the minimum)", lastEvaluated.ProtectiveStop, unit1Raise.Level)
	}
}

// --- A stop fill for a single (gapped) Unit leaves the Campaign open,
// --- with no further Adds ever proposed ---------------------------------

// TestStopFillClosingOneUnitLeavesTheCampaignOpenWithNoFurtherAdds is the
// ticket's required "stop fill for Unit 4 alone" case: the Campaign
// continues with three Units, and no further Add is proposed even as price
// keeps rising well past any conceivable fifth rung.
func TestStopFillClosingOneUnitLeavesTheCampaignOpenWithNoFurtherAdds(t *testing.T) {
	t.Parallel()

	stream, fixture := buildGapCampaign(t)

	stopPrice := fixture.unit4Stop - 0.37 // ADR 0005's gap rule: filled below the level
	stopFill := stopFillForUnits("AAPL", fixture.campaignID, "sim-fill-stop-4", []string{fixture.unit4FillID}, stopPrice, 133, day(60))
	// A bar with a huge high, well past any conceivable fifth rung, and a
	// low comfortably above the warmed-up Exit Channel — the ticket's "no
	// further Adds proposed even as price rises" criterion.
	risingBar := addOpportunityBar("AAPL", day(61), fixture.unit4Fill+10_000)

	emitted := stream.
		fill(stopFill).
		bar(risingBar).
		mustRun()

	unitsStopped := envelopesOfType(emitted, event.CampaignUnitsStoppedEventType)
	if len(unitsStopped) != 1 {
		t.Fatalf("got %d units-stopped event(s), want exactly 1", len(unitsStopped))
	}
	payload := decodeCampaignUnitsStopped(t, unitsStopped[0])
	if payload.CampaignID != fixture.campaignID {
		t.Errorf("CampaignID = %q, want %q", payload.CampaignID, fixture.campaignID)
	}
	if len(payload.UnitIndexes) != 1 || payload.UnitIndexes[0] != 4 {
		t.Errorf("UnitIndexes = %v, want [4]", payload.UnitIndexes)
	}
	if payload.QuantityClosed != 133 {
		t.Errorf("QuantityClosed = %d, want 133", payload.QuantityClosed)
	}
	if payload.FillPrice != stopPrice {
		t.Errorf("FillPrice = %v, want %v", payload.FillPrice, stopPrice)
	}
	if payload.EntryPrice != fixture.unit4Fill {
		t.Errorf("EntryPrice = %v, want %v (unit 4's own fill)", payload.EntryPrice, fixture.unit4Fill)
	}
	wantRealised := 133.0 * (stopPrice - fixture.unit4Fill) * fixture.cfg.DollarsPerPoint
	if payload.RealisedResult != wantRealised {
		t.Errorf("RealisedResult = %v, want exactly %v", payload.RealisedResult, wantRealised)
	}
	if payload.RemainingUnits != 3 {
		t.Errorf("RemainingUnits = %d, want 3", payload.RemainingUnits)
	}
	wantRemainingAggregate, err := sizing.AggregateOpenRisk([]sizing.UnitOpenRisk{
		{EntryPrice: fixture.unit1Fill, ProtectiveStop: fixture.unit1Stop, Quantity: 133},
		{EntryPrice: fixture.unit2Fill, ProtectiveStop: fixture.unit2Stop, Quantity: 133},
		{EntryPrice: fixture.unit3Fill, ProtectiveStop: fixture.unit3Stop, Quantity: 133},
	}, fixture.cfg.DollarsPerPoint)
	if err != nil {
		t.Fatalf("AggregateOpenRisk() error = %v", err)
	}
	if payload.AggregateOpenRiskAfter != wantRemainingAggregate {
		t.Errorf("AggregateOpenRiskAfter = %v, want exactly %v", payload.AggregateOpenRiskAfter, wantRemainingAggregate)
	}
	if err := payload.Validate(); err != nil {
		t.Errorf("emitted units-stopped payload fails its own Validate(): %v", err)
	}

	// The Campaign itself is NOT closed: no campaign-exited event anywhere.
	if got := len(envelopesOfType(emitted, event.CampaignExitedEventType)); got != 0 {
		t.Errorf("got %d campaign-exited event(s), want 0: the campaign continues with 3 units", got)
	}

	// risingBar's own evaluated event reports exactly 3 units, and NO add
	// proposal despite the enormous high.
	evaluatedAfterStop := envelopesOfType(emitted, event.CampaignEvaluatedEventType)
	last := decodeCampaignEvaluated(t, evaluatedAfterStop[len(evaluatedAfterStop)-1])
	if len(last.Units) != 3 {
		t.Errorf("got %d unit(s) on the post-stop bar's evaluated event, want 3", len(last.Units))
	}
	// Exactly the 3 add proposals the campaign's own build-up already
	// produced (units 2, 3 and 4) — none further, despite risingBar's
	// enormous high and the campaign now holding fewer than its configured
	// maximum: the Baseline never re-enters after a partial stop-out (The
	// Turtle Rules p.23-24's Whipsaw variant is out of scope, ADR 0012).
	if got := len(envelopesOfType(emitted, event.AddProposalEventType)); got != 3 {
		t.Errorf("got %d add proposal(s) total, want still exactly 3 after a partial stop-out", got)
	}
}

// --- A stop fill for the remaining Units empties the Campaign, with the
// --- aggregated whole-life result ---------------------------------------

// TestStopFillClosingTheRemainingUnitsExitsWithTheAggregatedResult
// continues TestStopFillClosingOneUnitLeavesTheCampaignOpenWithNoFurtherAdds's
// scenario: a SECOND stop fill, closing Units 1-3 together, empties the
// Campaign and reports the RealisedResult aggregated across BOTH stop fills
// — the ticket's required "subsequent stop fill for Units 1-3 ->
// Campaign-exited with reason stop and the aggregated realised result".
func TestStopFillClosingTheRemainingUnitsExitsWithTheAggregatedResult(t *testing.T) {
	t.Parallel()

	stream, fixture := buildGapCampaign(t)

	stop4Price := fixture.unit4Stop - 0.37
	stop4 := stopFillForUnits("AAPL", fixture.campaignID, "sim-fill-stop-4", []string{fixture.unit4FillID}, stop4Price, 133, day(60))

	// Units 1-3 share the SAME (converged) stop level, so one bar's low can
	// legitimately trade through all three at once.
	stop123Price := fixture.unit1Stop - 0.20
	stop123 := stopFillForUnits("AAPL", fixture.campaignID, "sim-fill-stop-123",
		[]string{fixture.unit1FillID, fixture.unit2FillID, fixture.unit3FillID}, stop123Price, 399, day(62))

	emitted := stream.
		fill(stop4).
		fill(stop123).
		mustRun()

	unitsStopped := envelopesOfType(emitted, event.CampaignUnitsStoppedEventType)
	if len(unitsStopped) != 2 {
		t.Fatalf("got %d units-stopped event(s), want exactly 2", len(unitsStopped))
	}
	second := decodeCampaignUnitsStopped(t, unitsStopped[1])
	if second.UnitIndexes[0] != 1 || second.UnitIndexes[1] != 2 || second.UnitIndexes[2] != 3 {
		t.Errorf("second units-stopped UnitIndexes = %v, want [1 2 3]", second.UnitIndexes)
	}
	if second.RemainingUnits != 0 {
		t.Errorf("second units-stopped RemainingUnits = %d, want 0", second.RemainingUnits)
	}
	if second.AggregateOpenRiskAfter != 0 {
		t.Errorf("second units-stopped AggregateOpenRiskAfter = %v, want exactly 0", second.AggregateOpenRiskAfter)
	}

	exited := decodeCampaignExited(t, onlyEnvelopeOfType(t, emitted, event.CampaignExitedEventType))
	if exited.Reason != event.ExitReasonStop {
		t.Errorf("Reason = %q, want %q", exited.Reason, event.ExitReasonStop)
	}
	if exited.Units != 4 {
		t.Errorf("Units = %d, want 4 (the campaign's WHOLE life, even though only 3 units remained at the final close)", exited.Units)
	}
	if exited.Quantity != 532 {
		t.Errorf("Quantity = %d, want 532 (133 x 4, across BOTH closing fills)", exited.Quantity)
	}

	// The whole-life aggregate: EntryPrice/ExitPrice are quantity-weighted
	// across BOTH stop fills (see CampaignExitedPayload's own doc comment,
	// "Accumulating partial stop-outs"). Compared with a small tolerance,
	// not exact float64 equality: the reducer accumulates these across TWO
	// separate Apply calls, and a hand-derived sum of the identical
	// underlying terms need not reproduce the IDENTICAL rounding at every
	// step (the same reason internal/sizing/stop_test.go's own chained
	// RaisedStop comparisons use a tolerance rather than exact equality) —
	// what this test asserts is the VALUE, and Validate()'s own exact
	// re-derivation (checked below) is what pins the internal bit-for-bit
	// consistency between RealisedResult and the payload's own stated
	// EntryPrice/ExitPrice/Quantity.
	wantEntryPrice := (133.0*fixture.unit4Fill + 133.0*fixture.unit1Fill + 133.0*fixture.unit2Fill + 133.0*fixture.unit3Fill) / 532.0
	wantExitPrice := (133.0*stop4Price + 399.0*stop123Price) / 532.0
	if math.Abs(exited.EntryPrice-wantEntryPrice) > 1e-9 {
		t.Errorf("EntryPrice = %v, want ~%v", exited.EntryPrice, wantEntryPrice)
	}
	if math.Abs(exited.ExitPrice-wantExitPrice) > 1e-9 {
		t.Errorf("ExitPrice = %v, want ~%v", exited.ExitPrice, wantExitPrice)
	}
	wantRealisedResult := 532.0 * (wantExitPrice - wantEntryPrice) * fixture.cfg.DollarsPerPoint
	if math.Abs(exited.RealisedResult-wantRealisedResult) > 1e-6 {
		t.Errorf("RealisedResult = %v, want ~%v", exited.RealisedResult, wantRealisedResult)
	}
	// The SAME aggregate, independently: the sum of each fill's own realised
	// share (decoded from the units-stopped events) must equal the whole-life
	// RealisedResult exactly, proving the accumulation is genuinely additive.
	first := decodeCampaignUnitsStopped(t, unitsStopped[0])
	if math.Abs((first.RealisedResult+second.RealisedResult)-exited.RealisedResult) > 1e-6 {
		t.Errorf("sum of each fill's own realised share (%v + %v = %v) does not match the whole-life RealisedResult %v",
			first.RealisedResult, second.RealisedResult, first.RealisedResult+second.RealisedResult, exited.RealisedResult)
	}
	if err := exited.Validate(); err != nil {
		t.Errorf("emitted campaign exited payload fails its own Validate(): %v", err)
	}

	// The instrument is a Setup again: campaign fully closed.
	if got := len(envelopesOfType(emitted, event.CampaignEvaluatedEventType)); got == 0 {
		t.Fatal("got 0 campaign-evaluated events, want at least the bars before the final close")
	}
}

// --- Fail-closed and idempotency ----------------------------------------

// TestStopFillNamingAnUnknownUnitFailsClosed covers the ticket's "a stop
// fill naming an unknown Unit fails closed" criterion.
func TestStopFillNamingAnUnknownUnitFailsClosed(t *testing.T) {
	t.Parallel()

	stream, fixture := buildGapCampaign(t)
	stray := stopFillForUnits("AAPL", fixture.campaignID, "sim-fill-stop-stray", []string{"never-existed"}, fixture.unit4Stop-1, 133, day(60))

	stream.fill(stray).wantRunError("never-existed", "unknown or already closed")
}

// TestStopFillNamingAnAlreadyClosedUnitFailsClosed covers the other half:
// a Unit a PREVIOUS stop fill already closed.
func TestStopFillNamingAnAlreadyClosedUnitFailsClosed(t *testing.T) {
	t.Parallel()

	stream, fixture := buildGapCampaign(t)
	stop4 := stopFillForUnits("AAPL", fixture.campaignID, "sim-fill-stop-4", []string{fixture.unit4FillID}, fixture.unit4Stop-0.37, 133, day(60))
	again := stopFillForUnits("AAPL", fixture.campaignID, "sim-fill-stop-4-again", []string{fixture.unit4FillID}, fixture.unit4Stop-0.50, 133, day(61))

	stream.fill(stop4).fill(again).wantRunError(fixture.unit4FillID, "unknown or already closed")
}

// TestDuplicateStopFillForASubsetOfUnitsIsAnIdempotentNoOp covers the
// ticket's "duplicate stop fill no-op" criterion for the per-Unit (gap-case)
// shape specifically.
func TestDuplicateStopFillForASubsetOfUnitsIsAnIdempotentNoOp(t *testing.T) {
	t.Parallel()

	stream, fixture := buildGapCampaign(t)
	stop4 := stopFillForUnits("AAPL", fixture.campaignID, "sim-fill-stop-4", []string{fixture.unit4FillID}, fixture.unit4Stop-0.37, 133, day(60))

	emitted := stream.fill(stop4).fill(stop4).mustRun()

	if got := len(envelopesOfType(emitted, event.CampaignUnitsStoppedEventType)); got != 1 {
		t.Fatalf("got %d units-stopped event(s), want exactly 1 despite the duplicate delivery", got)
	}
}

// --- Replay equivalence --------------------------------------------------

// TestReplayingTheGapFixtureTwiceYieldsByteIdenticalEmissions covers the
// ticket's replay-equivalence requirement for the gap fixture's WHOLE life:
// open, four Units (the last gapped), a partial stop, and the final close.
func TestReplayingTheGapFixtureTwiceYieldsByteIdenticalEmissions(t *testing.T) {
	t.Parallel()

	build := func() []event.Envelope {
		stream, fixture := buildGapCampaign(t)
		stop4 := stopFillForUnits("AAPL", fixture.campaignID, "sim-fill-stop-4", []string{fixture.unit4FillID}, fixture.unit4Stop-0.37, 133, day(60))
		stop123 := stopFillForUnits("AAPL", fixture.campaignID, "sim-fill-stop-123",
			[]string{fixture.unit1FillID, fixture.unit2FillID, fixture.unit3FillID}, fixture.unit1Stop-0.20, 399, day(62))
		return stream.fill(stop4).fill(stop123).mustRun()
	}

	first, second := build(), build()
	if len(first) != len(second) {
		t.Fatalf("emission counts differ: %d and %d", len(first), len(second))
	}
	if got := len(envelopesOfType(first, event.CampaignExitedEventType)); got != 1 {
		t.Fatalf("got %d campaign-exited event(s) in the first run, want exactly 1", got)
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
