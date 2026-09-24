package fills_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// This file covers the sell side of the book: each held Unit rests ONE sell
// order, its Exit Order (CONTEXT.md: "Exit Order"), at the level the
// reducer's strategy.exit-order.set decision last recorded for it — the
// higher of its Protective Stop and, while an Exit-Channel exit is proposed,
// that exit's level. ADR 0005's three rules apply to that one order.

// --- stand-in producer decisions -------------------------------------------

const exitOrderCampaign = "campaign:AAPL:day-56"

func exitOrderSet(t *testing.T, unitIndex int, source string, stop, exitLevel float64, quantity int64) event.Envelope {
	t.Helper()
	level := stop
	if source == event.ExitOrderSourceExitChannel {
		level = exitLevel
	}
	return envelope(t, "exit-order-set:AAPL:unit-"+strconv.Itoa(unitIndex)+":"+source, event.ExitOrderSetEventType, event.ExitOrderSetSchemaVersion, day(56), event.ExitOrderSetPayload{
		CampaignID:       exitOrderCampaign,
		InstrumentID:     testInstrument,
		UnitIndex:        unitIndex,
		Level:            level,
		Quantity:         quantity,
		Source:           source,
		ProtectiveStop:   stop,
		ExitChannelLevel: exitLevel,
		AsOf:             day(56),
		Rule:             event.RuleExitOrderHigherOfStopAndExitChannel,
		ADR:              event.ADRExitOrderRestsAtTheLevel,
	})
}

func exitProposal(t *testing.T, id string, level float64) event.Envelope {
	t.Helper()
	return envelope(t, id, event.ExitProposalEventType, event.ExitProposalSchemaVersion, day(56), event.ExitProposalPayload{
		CampaignID:   exitOrderCampaign,
		InstrumentID: testInstrument,
		PeriodEnd:    day(56),
		Reason:       event.ExitReasonExitChannel,
		Level:        level,
		Quantity:     fixtureUnitQuantity,
		Rule:         event.RuleExitChannelBreach,
		ADR:          event.ADRExitChannelBreach,
	})
}

// closingOnFill is a stand-in for the reducer that answers any closing fill
// by closing the whole Campaign, so the order leaves the book and the
// fixpoint ends.
func closingOnFill(t *testing.T) replay.Handler {
	t.Helper()
	return replay.HandlerFunc(func(_ context.Context, in event.Envelope) ([]event.Envelope, error) {
		if in.Type != event.FillEventType {
			return nil, nil
		}
		var fill event.FillPayload
		decodeInto(t, in, &fill)
		return []event.Envelope{envelope(t, "exited:"+fill.FillID, event.CampaignExitedEventType, event.CampaignExitedSchemaVersion, fill.FilledAt, event.CampaignExitedPayload{
			CampaignID:   fill.CampaignID,
			InstrumentID: fill.InstrumentID,
			FillID:       fill.FillID,
		})}, nil
	})
}

// --- ADR 0005 applied to the one order ---------------------------------------

// TestABarCoveringBothLevelsFillsAtTheExitLevelWhenItIsHigher: the four
// Units' stops sit around 154.9 and the Exit Channel at 159.3, so while the
// exit is proposed every Unit's Exit Order rests at 159.3. A bar reaching
// down to 150 passes 159.3 first; one order per Unit sells there, less
// slippage, and there is no second order left at the stop to sell anything
// again.
func TestABarCoveringBothLevelsFillsAtTheExitLevelWhenItIsHigher(t *testing.T) {
	t.Parallel()

	bars := campaignLifeBars()
	bars[len(bars)-1] = bar(day(80), 163.0, 163.1, 150, 152)
	run := runComposed(t, baselineConfig(), bars)

	got := fillPayloads(t, run.Inputs)
	if len(got) != 5 {
		t.Fatalf("got %d fill(s), want 5 (entry, three Adds, one exit)%s", len(got), describe(envelopesOfType(run.Inputs, event.FillEventType)))
	}
	exit := got[4]
	if exit.Kind != event.FillKindExit {
		t.Fatalf("final fill Kind = %q, want %q: every Unit's Exit Order rests at the higher exit level", exit.Kind, event.FillKindExit)
	}
	assertPrice(t, "exit fill level", exit.Level, 159.3)
	// min(159.3, open 163.0) - 0.075.
	assertPrice(t, "exit fill price", exit.Price, 159.225)
	if want := 4 * fixtureUnitQuantity; exit.Quantity != want {
		t.Errorf("exit fill quantity = %d, want %d", exit.Quantity, want)
	}
	if len(exit.UnitIDs) != 0 {
		t.Errorf("exit fill names units %v, want none: an exit fill closes whatever the Campaign still holds", exit.UnitIDs)
	}
	if stopped := envelopesOfType(run.Decisions, event.CampaignUnitsStoppedEventType); len(stopped) != 0 {
		t.Errorf("got %d units-stopped event(s), want none: no Unit's order rested at its stop", len(stopped))
	}
	var exited event.CampaignExitedPayload
	decodeInto(t, onlyOfType(t, run.Decisions, event.CampaignExitedEventType), &exited)
	if exited.Reason != event.ExitReasonExitChannel {
		t.Errorf("exited.Reason = %q, want %q", exited.Reason, event.ExitReasonExitChannel)
	}
	assertNeverOversold(t, got)
}

// TestAnExitLevelBelowTheStopsLeavesEveryUnitAtItsStop is the other side of
// the same rule. Early in a Campaign the Exit Channel (139) sits far below
// every stop (~154.8-155.05), so the stop is the higher level and each Unit's
// Exit Order stays there; a bar reaching both sells every Unit at its own
// stop, never at the lower channel.
func TestAnExitLevelBelowTheStopsLeavesEveryUnitAtItsStop(t *testing.T) {
	t.Parallel()

	bars := campaignLifeBars()[:59]
	bars = append(bars, bar(day(60), 159, 159.2, 138, 140))
	run := runComposed(t, baselineConfig(), bars)

	got := fillPayloads(t, run.Inputs)
	if len(got) != 8 {
		t.Fatalf("got %d fill(s), want 8 (entry, three Adds, four stops)%s", len(got), describe(envelopesOfType(run.Inputs, event.FillEventType)))
	}
	for i, stop := range got[4:] {
		if stop.Kind != event.FillKindStop {
			t.Fatalf("fill %d Kind = %q, want %q", 4+i, stop.Kind, event.FillKindStop)
		}
		if closeTo(stop.Level, 139) {
			t.Errorf("fill %d rests at the Exit Channel 139, below its own stop", 4+i)
		}
		// min(stop, open 159) - 0.075: the bar opened above every stop.
		assertPrice(t, "stop fill price", stop.Price, stop.Level-fixtureSlippage)
		if len(stop.UnitIDs) != 1 || stop.UnitIDs[0] != got[i].FillID {
			t.Errorf("stop fill %d names units %v, want exactly [%s]", 4+i, stop.UnitIDs, got[i].FillID)
		}
	}
	if exits := envelopesOfType(run.Decisions, event.ExitProposalEventType); len(exits) != 1 {
		t.Fatalf("got %d exit proposal(s), want 1: the bar did break the Exit Channel", len(exits))
	}
	var exited event.CampaignExitedPayload
	decodeInto(t, onlyOfType(t, run.Decisions, event.CampaignExitedEventType), &exited)
	if exited.Reason != event.ExitReasonStop {
		t.Errorf("exited.Reason = %q, want %q", exited.Reason, event.ExitReasonStop)
	}
	assertNeverOversold(t, got)
}

// TestABarOpeningBelowTheExitLevelFillsTheExitOrderAtTheOpen: the exit is
// proposed at 159.3, above every stop (~155), and the breach bar opens at
// 158 — already below the exit level and never trading back up to it. ADR
// 0005 rule 1: the order fills at min(level, open), so 158 less slippage.
func TestABarOpeningBelowTheExitLevelFillsTheExitOrderAtTheOpen(t *testing.T) {
	t.Parallel()

	bars := campaignLifeBars()
	bars[len(bars)-1] = bar(day(80), 158, 158.5, 156, 157)
	run := runComposed(t, baselineConfig(), bars)

	got := fillPayloads(t, run.Inputs)
	if len(got) != 5 {
		t.Fatalf("got %d fill(s), want 5 (entry, three Adds, one exit)%s", len(got), describe(envelopesOfType(run.Inputs, event.FillEventType)))
	}
	exit := got[4]
	if exit.Kind != event.FillKindExit {
		t.Fatalf("final fill Kind = %q, want %q", exit.Kind, event.FillKindExit)
	}
	assertPrice(t, "exit fill level", exit.Level, 159.3)
	assertPrice(t, "exit fill price", exit.Price, 158-fixtureSlippage)
	assertNeverOversold(t, got)
}

// TestAGapBelowBothLevelsFillsTheExitOrderAtTheOpen: an Exit Order already
// resting at the proposed exit level (155) above the Unit's stop (153) when
// the bar opens at 150, below both, and never trades back up to either. The
// bar covers only the stop's side of the book in the sense that its whole
// range lies below the exit level; the one order still fills, at the open
// less slippage, as the exit it rests as — and at the open instant, before
// the bar itself is delivered.
func TestAGapBelowBothLevelsFillsTheExitOrderAtTheOpen(t *testing.T) {
	t.Parallel()

	simulator := newSimulator(t)
	proposal := exitProposal(t, "exit-proposal:AAPL:day-56", 155)
	if err := observeDecisions(t, simulator,
		campaignOpened(t, exitOrderCampaign, 156, fixtureN),
		protectiveStopSet(t, exitOrderCampaign, 1, 153),
		exitOrderSet(t, 1, event.ExitOrderSourceProtectiveStop, 153, 0, fixtureUnitQuantity),
		proposal,
		exitOrderSet(t, 1, event.ExitOrderSourceExitChannel, 153, 155, fixtureUnitQuantity),
	); err != nil {
		t.Fatalf("observing the fixture's decisions: %v", err)
	}

	result, err := fills.RunBar(context.Background(), simulator, closingOnFill(t), barEnvelope(t, bar(day(57), 150, 151, 149, 150)))
	if err != nil {
		t.Fatalf("RunBar() error = %v", err)
	}
	got := fillPayloads(t, result.Inputs)
	if len(got) != 1 {
		t.Fatalf("got %d fill(s), want exactly 1: one Unit, one order%s", len(got), describe(result.Inputs))
	}
	if result.Inputs[0].Type != event.FillEventType {
		t.Errorf("the gapped order did not fill at the open instant, before the bar: %s", describe(result.Inputs))
	}
	fill := got[0]
	if fill.Kind != event.FillKindExit || fill.ProposalID != proposal.ID {
		t.Errorf("fill is a %q for proposal %q, want an exit for %q", fill.Kind, fill.ProposalID, proposal.ID)
	}
	assertPrice(t, "fill level", fill.Level, 155)
	// min(155, open 150) - 0.075.
	assertPrice(t, "fill price", fill.Price, 149.925)
	if fill.Quantity != fixtureUnitQuantity {
		t.Errorf("fill quantity = %d, want the Unit's own %d", fill.Quantity, fixtureUnitQuantity)
	}
}

// TestMixedUnitsEachFillAtTheirOwnOrdersLevel is the Crude-style gap case
// (The Turtle Rules p.22-23) meeting an Exit-Channel exit. Unit 2 fills far
// above its rung, so its stop (157.075) sits well above Unit 1's (153.325).
// Twenty bars later the Exit Channel (155, bar 56's low) lies between them:
// Unit 1's Exit Order moves up to 155 while Unit 2's stays at its own higher
// stop. A bar reaching 154 covers both orders, and each Unit sells at its own
// order's level — Unit 2 as a stop, then Unit 1 as the exit that closes what
// the Campaign still holds.
func TestMixedUnitsEachFillAtTheirOwnOrdersLevel(t *testing.T) {
	t.Parallel()

	run := runComposed(t, baselineConfig(), mixedUnitsBars())

	var mixed int
	for _, e := range envelopesOfType(run.Decisions, event.ExitOrderSetEventType) {
		var p event.ExitOrderSetPayload
		decodeInto(t, e, &p)
		if p.ExitChannelLevel > 0 && p.Source == event.ExitOrderSourceExitChannel {
			mixed++
			if p.UnitIndex != 1 {
				t.Errorf("unit %d moved to the exit level, want only Unit 1 (Unit 2's stop is higher)", p.UnitIndex)
			}
		}
	}
	if mixed != 1 {
		t.Fatalf("got %d Unit(s) moved to the exit level, want exactly Unit 1: the fixture no longer places the exit between the two stops", mixed)
	}

	got := fillPayloads(t, run.Inputs)
	if len(got) != 4 {
		t.Fatalf("got %d fill(s), want 4 (entry, gapped Add, Unit 2's stop, Unit 1's exit)%s", len(got), describe(envelopesOfType(run.Inputs, event.FillEventType)))
	}
	stop, exit := got[2], got[3]
	if stop.Kind != event.FillKindStop {
		t.Fatalf("third fill Kind = %q, want %q", stop.Kind, event.FillKindStop)
	}
	assertPrice(t, "stop fill level", stop.Level, 157.075)
	assertPrice(t, "stop fill price", stop.Price, 157.0)
	if len(stop.UnitIDs) != 1 || stop.UnitIDs[0] != got[1].FillID {
		t.Errorf("stop fill names units %v, want Unit 2's own [%s]", stop.UnitIDs, got[1].FillID)
	}
	if exit.Kind != event.FillKindExit {
		t.Fatalf("fourth fill Kind = %q, want %q", exit.Kind, event.FillKindExit)
	}
	assertPrice(t, "exit fill level", exit.Level, 155)
	assertPrice(t, "exit fill price", exit.Price, 154.925)
	if exit.Quantity != fixtureUnitQuantity {
		t.Errorf("exit fill quantity = %d, want Unit 1's own %d", exit.Quantity, fixtureUnitQuantity)
	}

	var exited event.CampaignExitedPayload
	decodeInto(t, onlyOfType(t, run.Decisions, event.CampaignExitedEventType), &exited)
	if exited.Reason != event.ExitReasonExitChannel {
		t.Errorf("exited.Reason = %q, want %q", exited.Reason, event.ExitReasonExitChannel)
	}
	if exited.Quantity != 2*fixtureUnitQuantity {
		t.Errorf("exited.Quantity = %d, want both Units over the Campaign's life", exited.Quantity)
	}
	// Quantity-weighted over the two closing fills.
	assertPrice(t, "exited.ExitPrice", exited.ExitPrice, (157.0+154.925)/2)
	assertNeverOversold(t, got)
}

// mixedUnitsBars is the fixture for TestMixedUnitsEachFillAtTheirOwnOrdersLevel.
func mixedUnitsBars() []event.CompletedBarPayload {
	bars := append(warmUpBars(),
		breakoutBar(),
		// Gaps above the 156.325 rung: Unit 2 fills at the open, 160.075,
		// so its stop is 157.075 while Unit 1's rises only to 153.325.
		bar(day(57), 160, 160.5, 159.5, 160.2),
	)
	// Quiet bars: every low above Unit 2's stop, every high below the next
	// rung (160.825), until bar 56 (low 155) is the oldest bar in the
	// twenty-bar Exit Channel.
	for k := 58; k <= 75; k++ {
		bars = append(bars, bar(day(k), 159.5, 160, 159, 159.5))
	}
	// The Exit Channel stands at 155. This bar breaks it without reaching
	// Unit 1's own stop.
	return append(bars, bar(day(76), 159, 159.5, 154, 154.5))
}

// TestNoUnitFillsWithoutARecordedExitOrder: a Protective Stop and an exit
// proposal are the reducer's inputs to a Unit's Exit Order, not orders of
// their own. Until strategy.exit-order.set records where that Unit's one sell
// order rests, nothing is resting for it, and a bar far below both levels
// sells nothing.
func TestNoUnitFillsWithoutARecordedExitOrder(t *testing.T) {
	t.Parallel()

	simulator := newSimulator(t)
	if err := observeDecisions(t, simulator,
		campaignOpened(t, exitOrderCampaign, 156, fixtureN),
		protectiveStopSet(t, exitOrderCampaign, 1, 153),
		exitProposal(t, "exit-proposal:AAPL:day-56", 155),
	); err != nil {
		t.Fatalf("observing the fixture's decisions: %v", err)
	}
	if resting := simulator.Resting(testInstrument); len(resting) != 0 {
		t.Errorf("Resting() = %+v, want nothing: no Exit Order has been recorded", resting)
	}

	result, err := fills.RunBar(context.Background(), simulator, closingOnFill(t), barEnvelope(t, bar(day(57), 150, 151, 140, 145)))
	if err != nil {
		t.Fatalf("RunBar() error = %v", err)
	}
	if got := envelopesOfType(result.Inputs, event.FillEventType); len(got) != 0 {
		t.Errorf("got %d fill(s), want none without a recorded Exit Order%s", len(got), describe(got))
	}
}

// TestRestingReportsEachUnitsExitOrder: the book read back while an exit is
// proposed between two Units' stops — one order per Unit, each at its own
// Exit Order's level and kind.
func TestRestingReportsEachUnitsExitOrder(t *testing.T) {
	t.Parallel()

	simulator := newSimulator(t)
	proposal := exitProposal(t, "exit-proposal:AAPL:day-56", 155)
	if err := observeDecisions(t, simulator,
		campaignOpened(t, exitOrderCampaign, 156, fixtureN),
		exitOrderSet(t, 1, event.ExitOrderSourceProtectiveStop, 153, 0, fixtureUnitQuantity),
		unitAdded(t, 2, "sim-fill-0002"),
		exitOrderSet(t, 2, event.ExitOrderSourceProtectiveStop, 157, 0, fixtureUnitQuantity),
		proposal,
		exitOrderSet(t, 1, event.ExitOrderSourceExitChannel, 153, 155, fixtureUnitQuantity),
		exitOrderSet(t, 2, event.ExitOrderSourceProtectiveStop, 157, 155, fixtureUnitQuantity),
	); err != nil {
		t.Fatalf("observing the fixture's decisions: %v", err)
	}

	resting := simulator.Resting(testInstrument)
	if len(resting) != 2 {
		t.Fatalf("got %d resting order(s), want one per held Unit%+v", len(resting), resting)
	}
	for i, want := range []struct {
		kind, proposalID, unitID string
		level                    float64
	}{
		{event.FillKindExit, proposal.ID, "sim-fill-0001", 155},
		{event.FillKindStop, "", "sim-fill-0002", 157},
	} {
		got := resting[i]
		if got.Kind != want.kind || got.Side != fills.SideSell || got.ProposalID != want.proposalID {
			t.Errorf("resting[%d] = %s %s for proposal %q, want a sell %s for %q", i, got.Side, got.Kind, got.ProposalID, want.kind, want.proposalID)
		}
		assertPrice(t, "resting level", got.Level, want.level)
		if got.Quantity != fixtureUnitQuantity || len(got.UnitIDs) != 1 || got.UnitIDs[0] != want.unitID || got.UnitIndexes[0] != i+1 {
			t.Errorf("resting[%d] = %d shares of units %v %v, want %d of Unit %d (%s)", i, got.Quantity, got.UnitIndexes, got.UnitIDs, fixtureUnitQuantity, i+1, want.unitID)
		}
	}
}

func unitAdded(t *testing.T, unitIndex int, fillID string) event.Envelope {
	t.Helper()
	return envelope(t, "unit-added:AAPL:"+fillID, event.CampaignUnitAddedEventType, event.CampaignUnitAddedSchemaVersion, day(56), event.CampaignUnitAddedPayload{
		CampaignID:   exitOrderCampaign,
		InstrumentID: testInstrument,
		UnitIndex:    unitIndex,
		FillID:       fillID,
		Quantity:     fixtureUnitQuantity,
	})
}

// TestAnExitOrderTheBookCannotPlaceFailsClosed: each refusal is a producer
// whose Exit Order disagrees with the rest of what it told the book. Placing
// such an order anyway would sell shares the Unit does not hold, or report an
// exit fill for a proposal that is not the one outstanding.
func TestAnExitOrderTheBookCannotPlaceFailsClosed(t *testing.T) {
	t.Parallel()

	opened := func(t *testing.T) event.Envelope {
		t.Helper()
		return campaignOpened(t, exitOrderCampaign, 156, fixtureN)
	}
	strayUnit := exitOrderSet(t, 3, event.ExitOrderSourceProtectiveStop, 153, 0, fixtureUnitQuantity)
	strayCampaignOrder := exitOrderSet(t, 1, event.ExitOrderSourceProtectiveStop, 153, 0, fixtureUnitQuantity)
	strayCampaignOrder = rewritePayload(t, strayCampaignOrder, func(p *event.ExitOrderSetPayload) { p.CampaignID = strayCampaign })
	unknownSource := rewritePayload(t, exitOrderSet(t, 1, event.ExitOrderSourceProtectiveStop, 153, 0, fixtureUnitQuantity), func(p *event.ExitOrderSetPayload) { p.Source = "somewhere-else" })

	tests := []struct {
		name      string
		decisions []event.Envelope
		wantErr   string
	}{
		{"campaign it does not hold", []event.Envelope{opened(t), strayCampaignOrder}, strayCampaign},
		{"unit it does not hold", []event.Envelope{opened(t), strayUnit}, "unit 3"},
		{"quantity the unit does not hold", []event.Envelope{opened(t), exitOrderSet(t, 1, event.ExitOrderSourceProtectiveStop, 153, 0, fixtureUnitQuantity+1)}, "quantity"},
		{"unrecognised source", []event.Envelope{opened(t), unknownSource}, "somewhere-else"},
		{"exit channel with no exit proposed", []event.Envelope{opened(t), exitOrderSet(t, 1, event.ExitOrderSourceExitChannel, 153, 155, fixtureUnitQuantity)}, "no exit proposal"},
		{"exit channel at another level", []event.Envelope{opened(t), exitProposal(t, "exit-proposal:AAPL:day-56", 154), exitOrderSet(t, 1, event.ExitOrderSourceExitChannel, 153, 155, fixtureUnitQuantity)}, "154"},
		{"undecodable", []event.Envelope{malformed(t, event.ExitOrderSetEventType, event.ExitOrderSetSchemaVersion)}, "decode " + event.ExitOrderSetEventType + " payload"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := observeDecisions(t, newSimulator(t), tt.decisions...)
			if err == nil {
				t.Fatal("observing the exit order returned nil, want it to fail closed")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func rewritePayload(t *testing.T, e event.Envelope, edit func(*event.ExitOrderSetPayload)) event.Envelope {
	t.Helper()
	var p event.ExitOrderSetPayload
	decodeInto(t, e, &p)
	edit(&p)
	return envelope(t, e.ID, e.Type, e.SchemaVersion, e.EventTime, p)
}

// TestTotalQuantitySoldNeverExceedsTheHolding runs every scenario above that
// closes a Campaign through the composed loop and checks the running
// position: no sell ever takes it below zero, and every Campaign ends flat.
func TestTotalQuantitySoldNeverExceedsTheHolding(t *testing.T) {
	t.Parallel()

	both := campaignLifeBars()
	both[len(both)-1] = bar(day(80), 163.0, 163.1, 150, 152)
	lowExit := campaignLifeBars()[:59]
	lowExit = append(lowExit, bar(day(60), 159, 159.2, 138, 140))

	for name, bars := range map[string][]event.CompletedBarPayload{
		"exit above the stops":         both,
		"exit below the stops":         lowExit,
		"exit between the Units":       mixedUnitsBars(),
		"exit channel breach, no stop": campaignLifeBars(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := fillPayloads(t, runComposed(t, baselineConfig(), bars).Inputs)
			assertNeverOversold(t, got)
			var held int64
			for _, f := range got {
				if f.Kind == event.FillKindEntry || f.Kind == event.FillKindAdd {
					held += f.Quantity
				} else {
					held -= f.Quantity
				}
			}
			if held != 0 {
				t.Errorf("the run ends holding %d shares, want the Campaign closed flat", held)
			}
		})
	}
}

// assertNeverOversold walks fills in delivery order and fails the first time
// the shares sold exceed the shares held.
func assertNeverOversold(t *testing.T, got []event.FillPayload) {
	t.Helper()
	var held int64
	for i, f := range got {
		switch f.Kind {
		case event.FillKindEntry, event.FillKindAdd:
			held += f.Quantity
		default:
			held -= f.Quantity
		}
		if held < 0 {
			t.Fatalf("fill %d (%s, %d shares) sells %d more than the Campaign held", i, f.Kind, f.Quantity, -held)
		}
	}
}

// TestExitOrderRunsReplayByteIdentically: the input streams the exit-order
// scenarios produce replay through a fresh reducer to exactly the decisions
// the run journalled.
func TestExitOrderRunsReplayByteIdentically(t *testing.T) {
	t.Parallel()

	both := campaignLifeBars()
	both[len(both)-1] = bar(day(80), 163.0, 163.1, 150, 152)
	for name, bars := range map[string][]event.CompletedBarPayload{
		"exit above the stops":   both,
		"exit between the Units": mixedUnitsBars(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run := runComposed(t, baselineConfig(), bars)
			first := replayThrough(t, run.Inputs)
			assertIdentical(t, first, replayThrough(t, run.Inputs))
			report, err := replay.Diff(run.Decisions, first)
			if err != nil {
				t.Fatalf("replay.Diff() error = %v", err)
			}
			if report != nil {
				t.Fatalf("the run's journalled decisions and a replay of its own inputs differ: %s", report)
			}
		})
	}
}

// stoppingOnFill is a stand-in for the reducer that answers a stop fill by
// closing exactly the Unit it names, and an exit fill by closing the
// Campaign, so each order leaves the book once it has filled.
func stoppingOnFill(t *testing.T) replay.Handler {
	t.Helper()
	indexOf := map[string]int{"sim-fill-0001": 1, "sim-fill-0002": 2}
	closing := closingOnFill(t)
	return replay.HandlerFunc(func(ctx context.Context, in event.Envelope) ([]event.Envelope, error) {
		if in.Type != event.FillEventType {
			return nil, nil
		}
		var fill event.FillPayload
		decodeInto(t, in, &fill)
		if fill.Kind != event.FillKindStop {
			return closing.Apply(ctx, in)
		}
		return []event.Envelope{envelope(t, "stopped:"+fill.FillID, event.CampaignUnitsStoppedEventType, event.CampaignUnitsStoppedSchemaVersion, fill.FilledAt, event.CampaignUnitsStoppedPayload{
			CampaignID:   fill.CampaignID,
			InstrumentID: fill.InstrumentID,
			FillID:       fill.FillID,
			UnitIndexes:  []int{indexOf[fill.UnitIDs[0]]},
		})}, nil
	})
}

// TestStopsFillWorstPriceFirst: two Units resting at their own stops, the
// LOWER one on the later Unit, both reached by one bar. Each sells its own
// shares at its own level, whatever the order; the order they are delivered
// in is worst price first (ADR 0005 rule 3), so Unit 2's fill comes first.
func TestStopsFillWorstPriceFirst(t *testing.T) {
	t.Parallel()

	simulator := newSimulator(t)
	if err := observeDecisions(t, simulator,
		campaignOpened(t, exitOrderCampaign, 160, fixtureN),
		exitOrderSet(t, 1, event.ExitOrderSourceProtectiveStop, 157, 0, fixtureUnitQuantity),
		unitAdded(t, 2, "sim-fill-0002"),
		exitOrderSet(t, 2, event.ExitOrderSourceProtectiveStop, 153, 0, fixtureUnitQuantity),
	); err != nil {
		t.Fatalf("observing the fixture's decisions: %v", err)
	}

	result, err := fills.RunBar(context.Background(), simulator, stoppingOnFill(t), barEnvelope(t, bar(day(57), 158, 158.5, 150, 151)))
	if err != nil {
		t.Fatalf("RunBar() error = %v", err)
	}
	got := fillPayloads(t, result.Inputs)
	if len(got) != 2 {
		t.Fatalf("got %d fill(s), want one per Unit%s", len(got), describe(result.Inputs))
	}
	for i, want := range []struct {
		unitID string
		price  float64
	}{
		{"sim-fill-0002", 153 - fixtureSlippage},
		{"sim-fill-0001", 157 - fixtureSlippage},
	} {
		if got[i].Kind != event.FillKindStop || len(got[i].UnitIDs) != 1 || got[i].UnitIDs[0] != want.unitID {
			t.Errorf("fill %d is a %s of units %v, want a stop of [%s]", i, got[i].Kind, got[i].UnitIDs, want.unitID)
		}
		assertPrice(t, "stop fill price", got[i].Price, want.price)
	}
	assertNeverOversold(t, append([]event.FillPayload{{Kind: event.FillKindEntry, Quantity: 2 * fixtureUnitQuantity}}, got...))
}

// TestAnExpiredExitProposalNoLongerFillsAnExitOrder: an exit proposal that
// expires leaves the book, and a Unit whose Exit Order still names the Exit
// Channel — because the producer never moved it back to its stop — cannot
// be reported as an exit of a proposal that is no longer outstanding. The
// reducer always moves it back in the same Apply return as the expiry.
func TestAnExpiredExitProposalNoLongerFillsAnExitOrder(t *testing.T) {
	t.Parallel()

	simulator := newSimulator(t)
	proposal := exitProposal(t, "exit-proposal:AAPL:day-56", 155)
	if err := observeDecisions(t, simulator,
		campaignOpened(t, exitOrderCampaign, 156, fixtureN),
		exitOrderSet(t, 1, event.ExitOrderSourceProtectiveStop, 153, 0, fixtureUnitQuantity),
		proposal,
		exitOrderSet(t, 1, event.ExitOrderSourceExitChannel, 153, 155, fixtureUnitQuantity),
		envelope(t, "expired:"+proposal.ID, event.ProposalExpiredEventType, event.ProposalExpiredSchemaVersion, day(57), event.ProposalExpiredPayload{
			InstrumentID: testInstrument,
			Kind:         event.ProposalKindExit,
			ProposalID:   proposal.ID,
		}),
	); err != nil {
		t.Fatalf("observing the fixture's decisions: %v", err)
	}
	if resting := simulator.Resting(testInstrument); len(resting) != 1 || resting[0].Kind != event.FillKindStop || resting[0].ProposalID != "" {
		t.Errorf("Resting() = %+v, want the one order reported against no proposal once the exit has expired", resting)
	}

	_, err := fills.RunBar(context.Background(), simulator, closingOnFill(t), barEnvelope(t, bar(day(57), 156, 156.5, 150, 151)))
	if err == nil || !strings.Contains(err.Error(), "no exit proposal is outstanding") {
		t.Fatalf("RunBar() error = %v, want an Exit Order at an expired exit to fail closed", err)
	}
}

// TestExitOrdersAtTheExitChannelMustAllRestAtTheOneProposal: a second exit
// proposal replaced the first, and only one Unit's Exit Order moved to it.
// Those two orders cannot be reported as one exit fill.
func TestExitOrdersAtTheExitChannelMustAllRestAtTheOneProposal(t *testing.T) {
	t.Parallel()

	simulator := newSimulator(t)
	if err := observeDecisions(t, simulator,
		campaignOpened(t, exitOrderCampaign, 160, fixtureN),
		unitAdded(t, 2, "sim-fill-0002"),
		exitProposal(t, "exit-proposal:AAPL:first", 155),
		exitOrderSet(t, 1, event.ExitOrderSourceExitChannel, 153, 155, fixtureUnitQuantity),
		exitOrderSet(t, 2, event.ExitOrderSourceExitChannel, 154, 155, fixtureUnitQuantity),
		exitProposal(t, "exit-proposal:AAPL:second", 156),
		exitOrderSet(t, 1, event.ExitOrderSourceExitChannel, 153, 156, fixtureUnitQuantity),
	); err != nil {
		t.Fatalf("observing the fixture's decisions: %v", err)
	}

	_, err := fills.RunBar(context.Background(), simulator, closingOnFill(t), barEnvelope(t, bar(day(57), 158, 158.5, 150, 151)))
	if err == nil || !strings.Contains(err.Error(), "unit 2") {
		t.Fatalf("RunBar() error = %v, want the stale Exit Order of unit 2 to fail closed", err)
	}
}

// TestAnExitFillMustCloseEverythingTheCampaignHolds: the exit level is
// reached while a held Unit has no Exit Order at all. An exit fill closes
// whatever the Campaign still holds, so it cannot describe selling only
// Unit 1, and the bar fails closed rather than overselling or leaving Unit
// 2 silently unaccounted for.
func TestAnExitFillMustCloseEverythingTheCampaignHolds(t *testing.T) {
	t.Parallel()

	simulator := newSimulator(t)
	if err := observeDecisions(t, simulator,
		campaignOpened(t, exitOrderCampaign, 160, fixtureN),
		unitAdded(t, 2, "sim-fill-0002"),
		exitProposal(t, "exit-proposal:AAPL:day-56", 155),
		exitOrderSet(t, 1, event.ExitOrderSourceExitChannel, 153, 155, fixtureUnitQuantity),
	); err != nil {
		t.Fatalf("observing the fixture's decisions: %v", err)
	}

	_, err := fills.RunBar(context.Background(), simulator, closingOnFill(t), barEnvelope(t, bar(day(57), 158, 158.5, 150, 151)))
	if err == nil || !strings.Contains(err.Error(), "only 1 rest at the exit") {
		t.Fatalf("RunBar() error = %v, want an exit that cannot close the whole Campaign to fail closed", err)
	}
}
