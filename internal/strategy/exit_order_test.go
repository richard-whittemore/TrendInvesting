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

// This file tests strategy.exit-order.set: each held Unit's Exit Order
// (CONTEXT.md: "Exit Order") rests at the higher of its own Protective Stop
// and, while an Exit-Channel exit is proposed for its Campaign, that exit's
// level — a tie naming the stop — and the reducer records every change of it.
//
// The fixtures build on campaign_test.go's single-Unit Campaign (opened at
// day(56), fill 201.25, stop 201.25 - 2N, Exit Channel warm at 100) and
// stop_ladder_test.go's four-Unit gap Campaign.

func decodeExitOrder(t *testing.T, envelope event.Envelope) event.ExitOrderSetPayload {
	t.Helper()
	if envelope.Type != event.ExitOrderSetEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.ExitOrderSetEventType)
	}
	if envelope.SchemaVersion != event.ExitOrderSetSchemaVersion {
		t.Fatalf("envelope.SchemaVersion = %d, want %d", envelope.SchemaVersion, event.ExitOrderSetSchemaVersion)
	}
	var payload event.ExitOrderSetPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	if err := payload.Validate(); err != nil {
		t.Fatalf("emitted exit order payload fails its own Validate(): %v", err)
	}
	return payload
}

// exitOrdersIn decodes every exit-order.set among emitted, in order.
func exitOrdersIn(t *testing.T, emitted []event.Envelope) []event.ExitOrderSetPayload {
	t.Helper()
	var orders []event.ExitOrderSetPayload
	for _, e := range envelopesOfType(emitted, event.ExitOrderSetEventType) {
		orders = append(orders, decodeExitOrder(t, e))
	}
	return orders
}

// exitOrdersAsOf is exitOrdersIn restricted to the ones in force from at.
func exitOrdersAsOf(t *testing.T, emitted []event.Envelope, at time.Time) []event.ExitOrderSetPayload {
	t.Helper()
	var orders []event.ExitOrderSetPayload
	for _, o := range exitOrdersIn(t, emitted) {
		if o.AsOf.Equal(at) {
			orders = append(orders, o)
		}
	}
	return orders
}

// wantExitOrder asserts one decoded order's Unit, level, source and the two
// levels it restates.
func wantExitOrder(t *testing.T, got event.ExitOrderSetPayload, unitIndex int, level float64, source string, stop, exitLevel float64, quantity int64) {
	t.Helper()
	if got.UnitIndex != unitIndex || got.Level != level || got.Source != source ||
		got.ProtectiveStop != stop || got.ExitChannelLevel != exitLevel || got.Quantity != quantity {
		t.Errorf("exit order = {unit %d, level %v, source %s, stop %v, exit %v, quantity %d}, want {unit %d, level %v, source %s, stop %v, exit %v, quantity %d}",
			got.UnitIndex, got.Level, got.Source, got.ProtectiveStop, got.ExitChannelLevel, got.Quantity,
			unitIndex, level, source, stop, exitLevel, quantity)
	}
}

// flatBars is count consecutive bars from day(first), each with the given
// low, a high 10 above it and a close between: enough of them (the Exit
// Channel length) moves the Exit Channel to exactly low without breaching
// it, since a tie is not a breach.
func flatBars(instrumentID string, first, count int, low float64) []event.CompletedBarPayload {
	bars := make([]event.CompletedBarPayload, 0, count)
	for i := 0; i < count; i++ {
		bars = append(bars, completedBar(instrumentID, day(first+i), low+10, low, low+5))
	}
	return bars
}

// assertExitOrdersCoverHoldings replays emitted and checks, at every
// Campaign-evaluated decision, that the most recent Exit Order of each
// Campaign names exactly the Units the Campaign holds, each for that Unit's
// own quantity — so the Campaign's Exit Orders sum to its holding — and that
// a Unit governed by its stop rests exactly at the stop the evaluation
// reports.
func assertExitOrdersCoverHoldings(t *testing.T, emitted []event.Envelope) {
	t.Helper()
	orders := map[string]map[int]event.ExitOrderSetPayload{}
	checked := 0
	for _, e := range emitted {
		switch e.Type {
		case event.ExitOrderSetEventType:
			o := decodeExitOrder(t, e)
			if orders[o.CampaignID] == nil {
				orders[o.CampaignID] = map[int]event.ExitOrderSetPayload{}
			}
			orders[o.CampaignID][o.UnitIndex] = o
		case event.CampaignUnitsStoppedEventType:
			stopped := decodeCampaignUnitsStopped(t, e)
			for _, i := range stopped.UnitIndexes {
				delete(orders[stopped.CampaignID], i)
			}
		case event.CampaignExitedEventType:
			delete(orders, decodeCampaignExited(t, e).CampaignID)
		case event.CampaignEvaluatedEventType:
			evaluated := decodeCampaignEvaluated(t, e)
			held := orders[evaluated.CampaignID]
			if len(held) != len(evaluated.Units) {
				t.Fatalf("%s: %d Exit Order(s) in force, want one per held Unit (%d)", e.ID, len(held), len(evaluated.Units))
			}
			var holding, covered int64
			for _, u := range evaluated.Units {
				o, ok := held[u.UnitIndex]
				if !ok {
					t.Fatalf("%s: held Unit %d has no Exit Order", e.ID, u.UnitIndex)
				}
				if o.Quantity != u.Quantity {
					t.Errorf("%s: Unit %d Exit Order quantity %d, want the Unit's own %d", e.ID, u.UnitIndex, o.Quantity, u.Quantity)
				}
				if o.Source == event.ExitOrderSourceProtectiveStop && o.Level != u.ProtectiveStop {
					t.Errorf("%s: Unit %d Exit Order at %v, want its Protective Stop %v", e.ID, u.UnitIndex, o.Level, u.ProtectiveStop)
				}
				holding += u.Quantity
				covered += o.Quantity
			}
			if covered != holding {
				t.Errorf("%s: Exit Orders cover %d shares, want the holding %d", e.ID, covered, holding)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no Campaign-evaluated decision to check the Exit Orders against")
	}
}

// TestAUnitsExitOrderIsSetWithItsFirstStopAndNotRepeated: opening a Campaign
// sets Unit 1's Exit Order at its Protective Stop, emitted directly after
// the stop itself; ordinary bars with nothing proposed change no level and
// emit nothing.
func TestAUnitsExitOrderIsSetWithItsFirstStopAndNotRepeated(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	stop := campaignFillPrice - float64(cfg.StopMultiple*breakoutFixtureN(t, cfg))

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(postEntryBar("AAPL", day(57), 105)).
		bar(postEntryBar("AAPL", day(58), 110)).
		mustRun()

	envelopes := envelopesOfType(emitted, event.ExitOrderSetEventType)
	if len(envelopes) != 1 {
		t.Fatalf("got %d exit-order.set event(s), want exactly 1: the first stop sets it, and bars that change no level emit none", len(envelopes))
	}
	envelope := envelopes[0]
	order := decodeExitOrder(t, envelope)
	wantExitOrder(t, order, 1, stop, event.ExitOrderSourceProtectiveStop, stop, 0, 133)
	if order.CampaignID != campaignID || order.InstrumentID != "AAPL" {
		t.Errorf("exit order names campaign %q, instrument %q; want %q, AAPL", order.CampaignID, order.InstrumentID, campaignID)
	}
	if !order.AsOf.Equal(day(56)) || !envelope.EventTime.Equal(day(56)) {
		t.Errorf("AsOf %v, EventTime %v; want both the opening fill's %v", order.AsOf, envelope.EventTime, day(56))
	}
	if order.Rule != event.RuleExitOrderHigherOfStopAndExitChannel || order.ADR != event.ADRExitOrderRestsAtTheLevel {
		t.Errorf("Rule %q, ADR %q", order.Rule, order.ADR)
	}
	if envelope.Source != "reducer" || envelope.StrategyVersion != testStrategyVersion || envelope.ConfigurationHash != event.ConfigurationHash(cfg) {
		t.Errorf("provenance = %q/%q/%q, want the reducer's own", envelope.Source, envelope.StrategyVersion, envelope.ConfigurationHash)
	}
	wantID := testDecisionID("exit-order-set-unit-1-sim-fill-0001", "AAPL", day(56))
	if envelope.ID != wantID {
		t.Errorf("ID = %q, want %q", envelope.ID, wantID)
	}

	// Directly after the Protective-Stop-set decision it combines.
	for i, e := range emitted {
		if e.Type == event.ExitOrderSetEventType {
			if i == 0 || emitted[i-1].Type != event.ProtectiveStopSetEventType {
				t.Errorf("exit-order.set at emission %d does not directly follow the protective-stop.set", i)
			}
		}
	}
	assertExitOrdersCoverHoldings(t, emitted)
}

// TestTheExitOrderRestsAtTheHigherLevel covers each combination: an Exit
// Channel that is not ready, one proposed below the stop, and one proposed
// above it — and the proposal's expiry, which returns the order to the stop.
func TestTheExitOrderRestsAtTheHigherLevel(t *testing.T) {
	t.Parallel()

	t.Run("exit channel not ready: the stop governs and no bar moves it", func(t *testing.T) {
		t.Parallel()

		cfg := validConfigurationPayload()
		cfg.EntryChannelLength = 3
		// Comfortably above the fixture's own ~65 total bars (#34's history
		// preamble below), so the Exit Channel is still genuinely unready the
		// first time the Campaign is evaluated — see
		// TestNoExitProposalWhileExitChannelNotReady's identical reasoning.
		cfg.ExitChannelLength = 100
		var bars []event.CompletedBarPayload
		for i := 1; i <= compactHistoryPreamble; i++ {
			bars = append(bars, syntheticBar("AAPL", day(i), 0))
		}
		rampStart := compactHistoryPreamble
		for i := 1; i <= 20; i++ {
			bars = append(bars, syntheticBar("AAPL", day(rampStart+i), float64(i)))
		}
		breakoutDay := day(rampStart + 21)
		bars = append(bars, completedBar("AAPL", breakoutDay, 300, 100, 250))
		fill := event.FillPayload{
			InstrumentID: "AAPL", Kind: event.FillKindEntry, ProposalID: testDecisionID("proposal", "AAPL", breakoutDay),
			FillID: "sim-fill-notready", Direction: event.DirectionLong, Quantity: 1, Price: 300, FilledAt: breakoutDay,
		}
		emitted := newStream(t, cfg).bars(bars).fill(fill).bar(completedBar("AAPL", breakoutDay.AddDate(0, 0, 1), 50, 1, 25)).mustRun()

		orders := exitOrdersIn(t, emitted)
		if len(orders) != 1 {
			t.Fatalf("got %d exit order(s), want only the first stop's", len(orders))
		}
		if orders[0].Source != event.ExitOrderSourceProtectiveStop || orders[0].ExitChannelLevel != 0 {
			t.Errorf("exit order = %+v, want the Protective Stop with no exit level", orders[0])
		}
		assertExitOrdersCoverHoldings(t, emitted)
	})

	t.Run("stop above the exit level: the stop governs, so the level does not change", func(t *testing.T) {
		t.Parallel()

		cfg := validConfigurationPayload()
		stop := campaignFillPrice - float64(cfg.StopMultiple*breakoutFixtureN(t, cfg))
		if stop <= 100 {
			t.Fatalf("fixture stop %v must sit above the warm Exit Channel (100) for this case", stop)
		}
		emitted := newStream(t, cfg).
			bars(breakoutBars("AAPL")).
			fill(openingFill("AAPL")).
			bar(postEntryBar("AAPL", day(57), 99)).  // breach: exit proposed at 100
			bar(postEntryBar("AAPL", day(58), 110)). // the proposal expires
			mustRun()

		if got := len(envelopesOfType(emitted, event.ExitProposalEventType)); got != 1 {
			t.Fatalf("got %d exit proposal(s), want 1", got)
		}
		orders := exitOrdersIn(t, emitted)
		if len(orders) != 1 {
			t.Fatalf("got %d exit order(s), want only the first stop's: an exit level below the stop leaves the order where it is", len(orders))
		}
		wantExitOrder(t, orders[0], 1, stop, event.ExitOrderSourceProtectiveStop, stop, 0, 133)
		assertExitOrdersCoverHoldings(t, emitted)
	})

	t.Run("exit level above the stop: the exit level governs until its proposal expires", func(t *testing.T) {
		t.Parallel()

		cfg := validConfigurationPayload()
		stop := campaignFillPrice - float64(cfg.StopMultiple*breakoutFixtureN(t, cfg))
		exitLevel := 140.0
		if exitLevel <= stop {
			t.Fatalf("fixture exit level %v must sit above the stop %v for this case", exitLevel, stop)
		}
		emitted := newStream(t, cfg).
			bars(breakoutBars("AAPL")).
			fill(openingFill("AAPL")).
			bars(flatBars("AAPL", 57, 20, exitLevel)).                                  // days 57..76 lift the Exit Channel to 140
			bar(completedBar("AAPL", day(77), exitLevel, exitLevel-1, exitLevel-0.5)).  // breach: exit proposed at 140
			bar(completedBar("AAPL", day(78), exitLevel+10, exitLevel+1, exitLevel+5)). // no breach; the proposal expires
			mustRun()

		proposal := decodeExitProposal(t, onlyEnvelopeOfType(t, emitted, event.ExitProposalEventType))
		if proposal.Level != exitLevel {
			t.Fatalf("exit proposal level = %v, want %v", proposal.Level, exitLevel)
		}
		if got := exitOrdersAsOf(t, emitted, day(76)); len(got) != 0 {
			t.Errorf("got %d exit order(s) on a bar that proposed nothing, want 0", len(got))
		}
		breach := exitOrdersAsOf(t, emitted, day(77))
		if len(breach) != 1 {
			t.Fatalf("got %d exit order(s) on the breach bar, want 1", len(breach))
		}
		wantExitOrder(t, breach[0], 1, exitLevel, event.ExitOrderSourceExitChannel, stop, exitLevel, 133)
		expiry := exitOrdersAsOf(t, emitted, day(78))
		if len(expiry) != 1 {
			t.Fatalf("got %d exit order(s) on the bar that expired the proposal, want 1", len(expiry))
		}
		wantExitOrder(t, expiry[0], 1, stop, event.ExitOrderSourceProtectiveStop, stop, 0, 133)

		// On the breach bar, after the exit proposal it combines.
		for i, e := range emitted {
			if e.Type == event.ExitOrderSetEventType && e.EventTime.Equal(day(77)) && emitted[i-1].Type != event.ExitProposalEventType {
				t.Errorf("breach-bar exit-order.set at emission %d does not directly follow the exit proposal", i)
			}
		}
		assertExitOrdersCoverHoldings(t, emitted)
	})

	t.Run("exit level equal to the stop: a tie names the stop, so the order does not move", func(t *testing.T) {
		t.Parallel()

		cfg := validConfigurationPayload()
		stop := campaignFillPrice - float64(cfg.StopMultiple*breakoutFixtureN(t, cfg))
		emitted := newStream(t, cfg).
			bars(breakoutBars("AAPL")).
			fill(openingFill("AAPL")).
			bars(flatBars("AAPL", 57, 20, stop)).
			bar(completedBar("AAPL", day(77), stop, stop-1, stop-0.5)).
			mustRun()

		if proposal := decodeExitProposal(t, onlyEnvelopeOfType(t, emitted, event.ExitProposalEventType)); proposal.Level != stop {
			t.Fatalf("exit proposal level = %v, want exactly the stop %v", proposal.Level, stop)
		}
		orders := exitOrdersIn(t, emitted)
		if len(orders) != 1 {
			t.Fatalf("got %d exit order(s), want only the first stop's: a tie leaves the stop governing", len(orders))
		}
		assertExitOrdersCoverHoldings(t, emitted)
	})

	t.Run("the end of the stream expires the proposal and returns the order to the stop", func(t *testing.T) {
		t.Parallel()

		cfg := validConfigurationPayload()
		stop := campaignFillPrice - float64(cfg.StopMultiple*breakoutFixtureN(t, cfg))
		exitLevel := 140.0
		emitted := newStream(t, cfg).
			bars(breakoutBars("AAPL")).
			fill(openingFill("AAPL")).
			bars(flatBars("AAPL", 57, 20, exitLevel)).
			bar(completedBar("AAPL", day(77), exitLevel, exitLevel-1, exitLevel-0.5)).
			endOfStream(day(77)).
			mustRun()

		last := lastEmission(t, emitted)
		order := decodeExitOrder(t, last)
		wantExitOrder(t, order, 1, stop, event.ExitOrderSourceProtectiveStop, stop, 0, 133)
		if want := testDecisionID("exit-order-set-unit-1-end-of-stream", "AAPL", day(77)); last.ID != want {
			t.Errorf("ID = %q, want %q", last.ID, want)
		}
		if emitted[len(emitted)-2].Type != event.ProposalExpiredEventType {
			t.Errorf("the end-of-stream exit order does not directly follow the expiry it results from")
		}
	})
}

// TestTheStopLadderMovesEachUnitsExitOrder: every Add sets the new Unit's
// Exit Order and moves every earlier Unit's with its raised stop, emitted
// after the stop decisions; the bars between the Adds move nothing.
func TestTheStopLadderMovesEachUnitsExitOrder(t *testing.T) {
	t.Parallel()

	s, fixture := buildGapCampaign(t)
	emitted := s.mustRun()

	orders := exitOrdersIn(t, emitted)
	// Unit 1 at entry; then 2 per Add of Unit 2, 3 of Unit 3, 4 of Unit 4.
	if len(orders) != 1+2+3+4 {
		t.Fatalf("got %d exit order(s), want 10", len(orders))
	}
	for _, at := range []time.Time{day(56), day(57), day(58), day(59)} {
		for _, o := range exitOrdersAsOf(t, emitted, at) {
			if o.Source != event.ExitOrderSourceProtectiveStop || o.Level != o.ProtectiveStop {
				t.Errorf("%v: Unit %d exit order %+v, want its Protective Stop", at, o.UnitIndex, o)
			}
		}
	}
	final := exitOrdersAsOf(t, emitted, day(59))
	if len(final) != 4 {
		t.Fatalf("got %d exit order(s) at the fourth Add, want one per Unit", len(final))
	}
	wantStops := map[int]float64{1: fixture.unit1Stop, 2: fixture.unit2Stop, 3: fixture.unit3Stop, 4: fixture.unit4Stop}
	for _, o := range final {
		if o.Level != wantStops[o.UnitIndex] || o.Quantity != 133 {
			t.Errorf("Unit %d exit order at %v for %d, want %v for 133", o.UnitIndex, o.Level, o.Quantity, wantStops[o.UnitIndex])
		}
	}
	// Order within the fourth Add's emissions: the new Unit first, then the
	// raised Units ascending — the order of the stop decisions they follow.
	var got []int
	for _, o := range final {
		got = append(got, o.UnitIndex)
	}
	if fmt.Sprint(got) != "[4 1 2 3]" {
		t.Errorf("exit order Units in emission order = %v, want [4 1 2 3]", got)
	}
	for i, e := range emitted {
		if e.Type == event.ExitOrderSetEventType && emitted[i-1].Type != event.ProtectiveStopSetEventType && emitted[i-1].Type != event.ExitOrderSetEventType {
			t.Errorf("exit-order.set at emission %d does not follow the stop decisions", i)
		}
	}
	assertExitOrdersCoverHoldings(t, emitted)
}

// TestAnExitLevelBetweenUnitsStopsMovesOnlyTheUnitsBelowIt: within one
// Campaign, a proposed exit level above some Units' stops and below
// another's moves only the Units it is above; the Unit whose stop is higher
// keeps its level and gets no event.
func TestAnExitLevelBetweenUnitsStopsMovesOnlyTheUnitsBelowIt(t *testing.T) {
	t.Parallel()

	s, fixture := buildGapCampaign(t)
	exitLevel := fixture.unit3Stop + 5
	if exitLevel <= fixture.unit1Stop || exitLevel <= fixture.unit2Stop || exitLevel >= fixture.unit4Stop {
		t.Fatalf("fixture exit level %v must sit above Units 1-3's stops and below Unit 4's %v", exitLevel, fixture.unit4Stop)
	}
	emitted := s.
		bars(flatBars("AAPL", 60, 20, exitLevel)).
		bar(completedBar("AAPL", day(80), exitLevel, exitLevel-1, exitLevel-0.5)).
		mustRun()

	breach := exitOrdersAsOf(t, emitted, day(80))
	if len(breach) != 3 {
		t.Fatalf("got %d exit order(s) on the breach bar, want 3 (Units 1-3)", len(breach))
	}
	stops := map[int]float64{1: fixture.unit1Stop, 2: fixture.unit2Stop, 3: fixture.unit3Stop}
	for i, o := range breach {
		wantExitOrder(t, o, i+1, exitLevel, event.ExitOrderSourceExitChannel, stops[i+1], exitLevel, 133)
	}
	assertExitOrdersCoverHoldings(t, emitted)
}

// TestAPartialStopOutEndsTheStoppedUnitsExitOrders: once a stop fill closes
// Unit 4, no later decision moves an Exit Order for it — even an exit level
// above every Unit's stop moves only the Units still held — and the Exit
// Orders in force keep summing to the remaining holding.
func TestAPartialStopOutEndsTheStoppedUnitsExitOrders(t *testing.T) {
	t.Parallel()

	s, fixture := buildGapCampaign(t)
	exitLevel := fixture.unit4Stop + 10
	stop4 := stopFillForUnits("AAPL", fixture.campaignID, "sim-fill-stop-4", []string{fixture.unit4FillID}, fixture.unit4Stop-0.37, 133, day(60))
	emitted := s.
		fill(stop4).
		bars(flatBars("AAPL", 61, 20, exitLevel)).
		bar(completedBar("AAPL", day(81), exitLevel, exitLevel-1, exitLevel-0.5)).
		mustRun()

	if got := exitOrdersAsOf(t, emitted, day(60)); len(got) != 0 {
		t.Errorf("got %d exit order(s) from the partial stop fill, want 0: the Units still held keep their levels", len(got))
	}
	breach := exitOrdersAsOf(t, emitted, day(81))
	if len(breach) != 3 {
		t.Fatalf("got %d exit order(s) on the breach bar, want 3 (Units 1-3; Unit 4 is closed)", len(breach))
	}
	for _, o := range breach {
		if o.UnitIndex == 4 {
			t.Errorf("an exit order was emitted for stopped Unit 4: %+v", o)
		}
		if o.Source != event.ExitOrderSourceExitChannel || o.Level != exitLevel {
			t.Errorf("Unit %d exit order %+v, want the exit level %v", o.UnitIndex, o, exitLevel)
		}
	}
	assertExitOrdersCoverHoldings(t, emitted)
}

// TestExitOrdersCoverAPartiallyFilledAdd: a Unit's Exit Order is for the
// shares that Unit actually holds, so a partial Add's order is for its
// filled quantity and the Campaign's orders still sum to its holding.
func TestExitOrdersCoverAPartiallyFilledAdd(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	rung2, err := sizing.NextAddLevel(campaignFillPrice, breakoutFixtureN(t, cfg), sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}
	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(addOpportunityBar("AAPL", day(57), rung2+5)).
		fill(addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", rung2, 80, day(57))).
		bar(postEntryBar("AAPL", day(58), 150)).
		mustRun()

	quantities := map[int]int64{}
	for _, o := range exitOrdersIn(t, emitted) {
		quantities[o.UnitIndex] = o.Quantity
	}
	if quantities[1] != 133 || quantities[2] != 80 {
		t.Errorf("exit order quantities = %v, want Unit 1: 133, Unit 2: 80", quantities)
	}
	assertExitOrdersCoverHoldings(t, emitted)
}

// TestAnExitFillEndsTheCampaignsExitOrders: once the exit fill closes the
// Campaign, nothing further is emitted for its Units.
func TestAnExitFillEndsTheCampaignsExitOrders(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	exitLevel := 140.0
	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bars(flatBars("AAPL", 57, 20, exitLevel)).
		bar(completedBar("AAPL", day(77), exitLevel, exitLevel-1, exitLevel-0.5)).
		fill(closingExitFill("AAPL", campaignID, exitLevel, day(77), day(77))).
		bar(completedBar("AAPL", day(78), exitLevel+10, exitLevel+1, exitLevel+5)).
		endOfStream(day(78)).
		mustRun()

	orders := exitOrdersIn(t, emitted)
	if len(orders) != 2 {
		t.Fatalf("got %d exit order(s), want 2: the first stop and the exit level, nothing after the exit fill", len(orders))
	}
	if orders[1].Source != event.ExitOrderSourceExitChannel {
		t.Errorf("second exit order = %+v, want the exit level", orders[1])
	}
}

// TestReplayingTheExitOrderFixtureTwiceYieldsByteIdenticalEmissions: every
// exit order — first stops, ladder raises, an exit level, an expiry, a
// partial stop and a final close — replays byte for byte.
func TestReplayingTheExitOrderFixtureTwiceYieldsByteIdenticalEmissions(t *testing.T) {
	t.Parallel()

	build := func() []event.Envelope {
		s, fixture := buildGapCampaign(t)
		exitLevel := fixture.unit3Stop + 5
		stop4 := stopFillForUnits("AAPL", fixture.campaignID, "sim-fill-stop-4", []string{fixture.unit4FillID}, fixture.unit4Stop-0.37, 133, day(80))
		return s.
			bars(flatBars("AAPL", 60, 20, exitLevel)).
			bar(completedBar("AAPL", day(80), exitLevel, exitLevel-1, exitLevel-0.5)).
			fill(stop4).
			bar(completedBar("AAPL", day(81), exitLevel+10, exitLevel+1, exitLevel+5)).
			endOfStream(day(81)).
			mustRun()
	}

	first, second := build(), build()
	if got := len(envelopesOfType(first, event.ExitOrderSetEventType)); got < 10+3+3 {
		t.Fatalf("got %d exit order(s), want at least 16 to make the replay comparison mean something", got)
	}
	if len(first) != len(second) {
		t.Fatalf("emission counts differ: %d and %d", len(first), len(second))
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
	assertExitOrdersCoverHoldings(t, first)
}
