package fills_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// This file is #18's event seam: the real reducer and the real simulator,
// composed by fills.RunBar, driven over a real bar series. Everything
// asserted here is asserted through events, because the ticket's criterion is
// about events — "fill events are indistinguishable in shape from
// adapter-produced fills".

// --- The full Campaign life ----------------------------------------------

// TestBreakoutBarFillsTheEntryInsideThatBar is ADR 0005's headline: the entry
// is a resting order that fills INSIDE the breakout bar, not at the next
// open. The fill price is the level the proposal named plus 0.05 N, and the
// Campaign opens at that price, not at the level.
//
// #79: the level the proposal names is the Entry Channel high (155.5) that
// the breakout exceeded, not the breakout bar's own high (157) — the bar's
// high sits a full 1 N above the channel here, and the fill lands at the
// channel plus slippage, never at the bar's high.
func TestBreakoutBarFillsTheEntryInsideThatBar(t *testing.T) {
	t.Parallel()

	bars := append(warmUpBars(), breakoutBar())
	run := runComposed(t, baselineConfig(), bars)

	var proposal event.TradeProposalPayload
	decodeInto(t, onlyOfType(t, run.Decisions, event.TradeProposalEventType), &proposal)
	if !closeTo(proposal.EntryLevel, 155.5) {
		t.Fatalf("proposal.EntryLevel = %v, want 155.5 (the Entry Channel high the breakout exceeded, not its own high of 157)", proposal.EntryLevel)
	}
	if !closeTo(proposal.N, fixtureN) {
		t.Fatalf("proposal.N = %v, want %v", proposal.N, fixtureN)
	}
	if proposal.Quantity != fixtureUnitQuantity {
		t.Fatalf("proposal.Quantity = %d, want %d", proposal.Quantity, fixtureUnitQuantity)
	}

	got := fillPayloads(t, run.Inputs)
	if len(got) != 1 {
		t.Fatalf("got %d fill(s), want exactly 1 (the entry)", len(got))
	}
	fill := got[0]
	if fill.Kind != event.FillKindEntry {
		t.Errorf("fill.Kind = %q, want %q", fill.Kind, event.FillKindEntry)
	}
	assertPrice(t, "fill.Level", fill.Level, 155.5)
	// max(level 155.5, open 155.5) + 0.075: the bar did not gap through (the
	// bar's own open sits exactly at the channel), so the order executed
	// where it rested and slippage pushed it against us. It did NOT execute
	// at the bar's own high of 157.
	assertPrice(t, "fill.Price", fill.Price, 155.575)
	assertPrice(t, "fill.SlippageApplied", fill.SlippageApplied, fixtureSlippage)
	// 3333 x 0.005, above the 1.00 floor and far below 1 % of the trade.
	assertPrice(t, "fill.Commission", fill.Commission, 16.665)
	if fill.Quantity != fixtureUnitQuantity {
		t.Errorf("fill.Quantity = %d, want %d", fill.Quantity, fixtureUnitQuantity)
	}
	if !fill.FilledAt.Equal(day(56)) {
		t.Errorf("fill.FilledAt = %s, want the breakout bar's own period end %s", fill.FilledAt, day(56))
	}

	var opened event.CampaignOpenedPayload
	decodeInto(t, onlyOfType(t, run.Decisions, event.CampaignOpenedEventType), &opened)
	assertPrice(t, "campaign entry price", opened.EntryPrice, 155.575)
	// 2 N below the ACTUAL fill (The Turtle Rules p.22, ADR 0013).
	assertPrice(t, "campaign protective stop", opened.ProtectiveStop, 152.575)
}

// TestGapUpBreakoutBarFillsAtTheOpenPlusSlippage is #79's clearest evidence:
// under the old defect, EntryLevel was the breakout bar's own high, which by
// construction the bar itself always reaches, so a breakout bar could never
// gap OVER its own entry level — ADR 0005 rule 1 ("gaps fill at the open")
// was unreachable for an entry, no matter what fixture was tried. With
// EntryLevel the Entry Channel high (a level fixed before the bar opened),
// a bar that opens above the channel is a genuine gap, expressible for the
// first time.
//
// It also isolates ADR 0013's rule from ADR 0006's: the proposal's
// ProtectiveStopIntent is derived from EntryLevel and moves with it, but the
// Campaign's ACTUAL Protective Stop is measured from the fill that actually
// executed (here, the gapped-open price, well above both EntryLevel and the
// proposal's stop intent) — the two are deliberately different numbers below.
func TestGapUpBreakoutBarFillsAtTheOpenPlusSlippage(t *testing.T) {
	t.Parallel()

	// The Entry Channel (from warmUpBars) tops out at 155.5. This bar opens
	// at 156.5 — already through the channel — and its high of 157.2 is what
	// makes it a Signal at all (strictly exceeding 155.5, The Turtle Rules
	// p.19's "exceeds"), while staying below 157.325 — the Unit 2 rung half
	// an N above the 156.575 gapped entry fill — so this bar produces the
	// entry alone rather than also chaining an Add (PR #85 review round).
	gapUp := bar(day(56), 156.5, 157.2, 156, 157.0)
	bars := append(warmUpBars(), gapUp)
	run := runComposed(t, baselineConfig(), bars)

	var proposal event.TradeProposalPayload
	decodeInto(t, onlyOfType(t, run.Decisions, event.TradeProposalEventType), &proposal)
	if !closeTo(proposal.EntryLevel, 155.5) {
		t.Fatalf("proposal.EntryLevel = %v, want 155.5 (the Entry Channel high, fixed before this bar opened)", proposal.EntryLevel)
	}
	// The proposal's OWN stop intent is derived from EntryLevel (event.
	// TradeProposalPayload.Validate re-derives it), never from what actually
	// fills.
	wantStopIntent := 155.5 - 2*fixtureN
	if !closeTo(proposal.ProtectiveStopIntent, wantStopIntent) {
		t.Fatalf("proposal.ProtectiveStopIntent = %v, want %v (entry level - 2N)", proposal.ProtectiveStopIntent, wantStopIntent)
	}

	got := fillPayloads(t, run.Inputs)
	if len(got) != 1 {
		t.Fatalf("got %d fill(s), want exactly 1 (the entry)", len(got))
	}
	fill := got[0]
	if fill.Kind != event.FillKindEntry {
		t.Errorf("fill.Kind = %q, want %q", fill.Kind, event.FillKindEntry)
	}
	assertPrice(t, "fill.Level", fill.Level, 155.5)
	// max(level 155.5, open 156.5) + 0.075: the bar gapped OVER the level, so
	// the order executed at the open, never at the level it rested at.
	assertPrice(t, "fill.Price", fill.Price, 156.575)

	var opened event.CampaignOpenedPayload
	decodeInto(t, onlyOfType(t, run.Decisions, event.CampaignOpenedEventType), &opened)
	assertPrice(t, "campaign entry price", opened.EntryPrice, 156.575)
	// The Campaign's ACTUAL stop is 2 N below the ACTUAL fill (ADR 0013),
	// not below EntryLevel: 156.575 - 3 = 153.575, deliberately different
	// from the proposal's own stop intent of 152.5 above.
	wantActualStop := 156.575 - 2*fixtureN
	assertPrice(t, "campaign protective stop", opened.ProtectiveStop, wantActualStop)
	if closeTo(opened.ProtectiveStop, proposal.ProtectiveStopIntent) {
		t.Error("the Campaign's actual stop equals the proposal's stop intent; ADR 0013 requires it to come from the actual fill, which gapped away from the proposed level here")
	}
}

// TestAddLadderRungsAreMeasuredFromTheSlippedFill walks the whole Add chain.
// Faith is explicit that the ladder is measured from the actual fill and that
// slippage on one fill pushes every later rung out accordingly [T p.19-20],
// so each rung here is the PREVIOUS fill plus 0.75, never the intended level
// plus 0.75.
func TestAddLadderRungsAreMeasuredFromTheSlippedFill(t *testing.T) {
	t.Parallel()

	bars := campaignLifeBars()
	run := runComposed(t, baselineConfig(), bars[:59]) // through day(59), Unit 4

	got := fillPayloads(t, run.Inputs)
	if len(got) != 4 {
		t.Fatalf("got %d fill(s), want 4 (the entry and three Adds)%s", len(got), describe(envelopesOfType(run.Inputs, event.FillEventType)))
	}

	wants := []struct {
		kind  string
		level float64
		price float64
	}{
		{event.FillKindEntry, 155.5, 155.575},
		// 155.575 + 0.75; bar 57 opened at 155.7, below the rung.
		{event.FillKindAdd, 156.325, 156.4},
		// 156.4 + 0.75; bar 58 opened at 156.8, below the rung.
		{event.FillKindAdd, 157.15, 157.225},
		// 157.225 + 0.75; bar 59 opened at 157.5, below the rung.
		{event.FillKindAdd, 157.975, 158.05},
	}
	for i, want := range wants {
		if got[i].Kind != want.kind {
			t.Errorf("fill %d Kind = %q, want %q", i, got[i].Kind, want.kind)
		}
		assertPrice(t, "fill level", got[i].Level, want.level)
		assertPrice(t, "fill price", got[i].Price, want.price)
		assertPrice(t, "fill slippage", got[i].SlippageApplied, fixtureSlippage)
	}

	// ADR 0008: four Units and no fifth, ever.
	units := envelopesOfType(run.Decisions, event.CampaignUnitAddedEventType)
	if len(units) != 3 {
		t.Fatalf("got %d unit-added event(s), want 3", len(units))
	}
	var last event.CampaignUnitAddedPayload
	decodeInto(t, units[len(units)-1], &last)
	if last.Units != 4 {
		t.Errorf("final unit count = %d, want 4", last.Units)
	}
}

// TestExitChannelBreachFillsAtTheLevelLessSlippageAndClosesTheCampaign runs
// the whole fixture: entry, three Adds, twenty quiet bars, and the
// Exit-Channel exit that closes every Unit together.
func TestExitChannelBreachFillsAtTheLevelLessSlippageAndClosesTheCampaign(t *testing.T) {
	t.Parallel()

	run := runComposed(t, baselineConfig(), campaignLifeBars())

	got := fillPayloads(t, run.Inputs)
	if len(got) != 5 {
		t.Fatalf("got %d fill(s), want 5 (entry, three Adds, the exit)%s", len(got), describe(envelopesOfType(run.Inputs, event.FillEventType)))
	}
	exit := got[4]
	if exit.Kind != event.FillKindExit {
		t.Fatalf("final fill Kind = %q, want %q", exit.Kind, event.FillKindExit)
	}
	// The Exit Channel low over bars 60..79.
	assertPrice(t, "exit fill level", exit.Level, 159.3)
	// min(level 159.3, open 163.0) - 0.075: the bar did not gap through the
	// level, so the order executed where it rested and slippage took the
	// price further down.
	assertPrice(t, "exit fill price", exit.Price, 159.225)
	if want := 4 * fixtureUnitQuantity; exit.Quantity != want {
		t.Errorf("exit fill quantity = %d, want %d (every Unit exits together)", exit.Quantity, want)
	}

	var exited event.CampaignExitedPayload
	decodeInto(t, onlyOfType(t, run.Decisions, event.CampaignExitedEventType), &exited)
	if exited.Reason != event.ExitReasonExitChannel {
		t.Errorf("exited.Reason = %q, want %q", exited.Reason, event.ExitReasonExitChannel)
	}
	if exited.Units != 4 {
		t.Errorf("exited.Units = %d, want 4", exited.Units)
	}
	// (155.575 + 156.4 + 157.225 + 158.05) / 4.
	assertPrice(t, "exited.EntryPrice", exited.EntryPrice, 156.8125)
	assertPrice(t, "exited.ExitPrice", exited.ExitPrice, 159.225)
	if exited.RealisedResult <= 0 {
		t.Errorf("exited.RealisedResult = %v, want a profit: the fixture exits above its average entry", exited.RealisedResult)
	}
}

// --- ADR 0005 rule 3: same-bar ambiguity resolves pessimistically ---------

// TestBarCoveringBothEntryAndStopEntersThenStops is the ticket's headline
// criterion. The breakout bar's range covers the entry level AND, once the
// entry has filled, the Protective Stop that entry sets — so the Campaign is
// assumed to have entered and THEN been stopped, and the result is a loss.
// The favourable ordering (stopped first, so never entered, so no loss) is
// never assumed.
//
// The fixture also pins Range.Reference's own rule. The bar opens at 152.0,
// BELOW the stop the entry sets at 152.575. Referenced to the bar's open, the
// stop would look gapped-through and fill at 151.925 — a price that occurred
// before the stop existed. Referenced to the entry fill that created it, it
// fills at its level less slippage, 152.5.
func TestBarCoveringBothEntryAndStopEntersThenStops(t *testing.T) {
	t.Parallel()

	// #79: the entry level is now the Entry Channel high (155.5), not this
	// bar's own high, so the entry fills at 155.5 + slippage regardless of
	// the bar's own range — this bar's open (152.0) and low (151.5) exist
	// only to pin the same-bar-ambiguity and Range.Reference rules below. The
	// high (156.0) stays below 156.325 — the Unit 2 rung half an N above the
	// 155.575 entry fill — so this bar produces the entry and stop alone
	// (PR #85 review round: a higher high would also chain an Add here).
	bars := append(warmUpBars(), bar(day(56), 152.0, 156.0, 151.5, 153.0))
	run := runComposed(t, baselineConfig(), bars)

	got := fillPayloads(t, run.Inputs)
	if len(got) != 2 {
		t.Fatalf("got %d fill(s), want 2 (entered, then stopped)%s", len(got), describe(envelopesOfType(run.Inputs, event.FillEventType)))
	}
	if got[0].Kind != event.FillKindEntry || got[1].Kind != event.FillKindStop {
		t.Fatalf("fill kinds = %q then %q, want entry then stop: the pessimistic ordering is entered-then-stopped", got[0].Kind, got[1].Kind)
	}
	assertPrice(t, "entry fill price", got[0].Price, 155.575)
	assertPrice(t, "stop fill level", got[1].Level, 152.575)
	assertPrice(t, "stop fill price", got[1].Price, 152.5)
	if closeTo(got[1].Price, 151.925) {
		t.Error("stop filled at the bar's open: a stop created by a fill inside the bar cannot have executed before it existed")
	}
	// Both fills belong to the same bar and carry the same timestamp; the
	// ORDER they were delivered in is what records which happened first.
	if !got[0].FilledAt.Equal(got[1].FilledAt) {
		t.Errorf("fills are timestamped %s and %s, want both at the bar's period end", got[0].FilledAt, got[1].FilledAt)
	}

	var exited event.CampaignExitedPayload
	decodeInto(t, onlyOfType(t, run.Decisions, event.CampaignExitedEventType), &exited)
	if exited.Reason != event.ExitReasonStop {
		t.Errorf("exited.Reason = %q, want %q", exited.Reason, event.ExitReasonStop)
	}
	if exited.RealisedResult >= 0 {
		t.Errorf("exited.RealisedResult = %v, want a loss: entering at 155.575 and stopping at 152.5 loses money", exited.RealisedResult)
	}
	// 3333 x (152.5 - 155.575).
	assertPrice(t, "exited.RealisedResult", exited.RealisedResult, float64(fixtureUnitQuantity)*(152.5-155.575))
}

// --- ADR 0005 rule 1: gaps ------------------------------------------------

// TestGapDownThroughAStopFillsAtTheOpen: a bar that opens BELOW a stop that
// was already resting took that stop out at the open. The fill price is the
// open less slippage, never the stop level — the level was not reachable once
// the market gapped past it.
//
// Because the fill happened at the first instant of the bar, it reaches the
// reducer BEFORE the bar does: the Campaign was already closed for the whole
// of that session, so no Campaign-evaluated event is journalled for it.
func TestGapDownThroughAStopFillsAtTheOpen(t *testing.T) {
	t.Parallel()

	bars := append(warmUpBars(), breakoutBar(), bar(day(57), 150, 151, 148, 149))
	run := runComposed(t, baselineConfig(), bars)

	got := fillPayloads(t, run.Inputs)
	if len(got) != 2 {
		t.Fatalf("got %d fill(s), want 2 (the entry and the gapped stop)%s", len(got), describe(envelopesOfType(run.Inputs, event.FillEventType)))
	}
	stop := got[1]
	if stop.Kind != event.FillKindStop {
		t.Fatalf("second fill Kind = %q, want %q", stop.Kind, event.FillKindStop)
	}
	// #79 moves the stop to 152.575 (2 N below the new 155.575 entry fill,
	// was 154.075 below the old 157.075), but the open (150) still gaps
	// below it either way, so the executed price is unchanged.
	assertPrice(t, "stop fill level", stop.Level, 152.575)
	// min(level 152.575, open 150) - 0.075.
	assertPrice(t, "stop fill price", stop.Price, 149.925)

	// The gap fill precedes the bar in the composed input stream, and the
	// reducer therefore never evaluates the Campaign for that bar.
	var sawStopFill bool
	for _, e := range run.Inputs {
		switch e.Type {
		case event.FillEventType:
			if decodeFill(t, e).Kind == event.FillKindStop {
				sawStopFill = true
			}
		case event.CompletedBarEventType:
			if e.EventTime.Equal(day(57)) && !sawStopFill {
				t.Error("bar 57 reached the reducer before the stop that the bar's own open took out")
			}
		}
	}
	for _, e := range envelopesOfType(run.Decisions, event.CampaignEvaluatedEventType) {
		if e.EventTime.Equal(day(57)) {
			t.Error("a campaign-evaluated event was journalled for a bar whose open had already closed the campaign")
		}
	}
}

// TestGapFillsCarryTheProvenanceOfTheBarWhoseOpenProducedThem is the
// provenance rule for the open-instant pass, stated once and unmistakably.
//
// A gap fill is delivered to the reducer BEFORE the bar it belongs to, which
// is correct — the order executed at that bar's open, the first instant of
// the session. But it is still a fact ABOUT that bar, produced from that
// bar's own open, so its EventTime and its RecordedAt are that bar's, and so
// are those of every decision the reducer makes because of it (the reducer
// stamps an emission with its input's own RecordedAt).
//
// Taking RecordedAt from the last input the simulator happened to have
// delivered would date a gap fill, and the Campaign exit it causes, to the
// PREVIOUS bar — a journal claiming an execution was recorded before the
// session that produced it, which is exactly the kind of chronological
// nonsense the reducer's own bar-versus-fill checks exist to catch on the
// input side.
func TestGapFillsCarryTheProvenanceOfTheBarWhoseOpenProducedThem(t *testing.T) {
	t.Parallel()

	gapping := bar(day(57), 150, 151, 148, 149)
	bars := append(warmUpBars(), breakoutBar(), gapping)
	run := runComposed(t, baselineConfig(), bars)

	fillEnvelopes := envelopesOfType(run.Inputs, event.FillEventType)
	if len(fillEnvelopes) != 2 {
		t.Fatalf("got %d fill(s), want 2 (the entry and the gapped stop)%s", len(fillEnvelopes), describe(fillEnvelopes))
	}
	stop := fillEnvelopes[1]
	if decodeFill(t, stop).Kind != event.FillKindStop {
		t.Fatalf("second fill is not the stop%s", describe(fillEnvelopes))
	}

	// The bar whose open produced it, not the one before.
	wantRecordedAt := barEnvelope(t, gapping).RecordedAt
	if !stop.EventTime.Equal(gapping.PeriodEnd) {
		t.Errorf("gap fill EventTime = %s, want the gapping bar's period end %s", stop.EventTime, gapping.PeriodEnd)
	}
	if !stop.RecordedAt.Equal(wantRecordedAt) {
		t.Errorf("gap fill RecordedAt = %s, want the gapping bar's own %s (day 56 is the bar BEFORE the one whose open took the stop out)", stop.RecordedAt, wantRecordedAt)
	}

	// And every decision the reducer made because of it.
	exited := onlyOfType(t, run.Decisions, event.CampaignExitedEventType)
	if !exited.RecordedAt.Equal(wantRecordedAt) {
		t.Errorf("campaign-exited RecordedAt = %s, want %s: a decision inherits its input's recording moment", exited.RecordedAt, wantRecordedAt)
	}
	for _, e := range envelopesOfType(run.Decisions, event.CampaignUnitsStoppedEventType) {
		if !e.RecordedAt.Equal(wantRecordedAt) {
			t.Errorf("units-stopped RecordedAt = %s, want %s", e.RecordedAt, wantRecordedAt)
		}
	}

	// Delivery order is unchanged by any of this: the fill still precedes the
	// bar, because it happened at that bar's open.
	var sawStop bool
	for _, e := range run.Inputs {
		if e.Type == event.FillEventType && decodeFill(t, e).Kind == event.FillKindStop {
			sawStop = true
		}
		if e.Type == event.CompletedBarEventType && e.EventTime.Equal(day(57)) && !sawStop {
			t.Fatal("the gapping bar reached the reducer before the stop its own open took out")
		}
	}
}

// TestAGapUpBuyInTheOpenInstantPassCarriesTheSameProvenance is the buy side
// of the rule above.
//
// It uses a stub handler rather than the reducer, because this reducer can
// never reach the case: a Signal already guarantees the bar's high strictly
// exceeds the level the entry rests at (the Entry Channel high, #79), so the
// SAME bar that raises the proposal always covers it too, and no proposal
// ever rests into the next bar to be gapped over (see
// TestProposalsAreAlwaysCoveredByTheBarThatRaisedThem). The path is reachable
// for a proposal built directly, as this test does, standing in for a
// producer this reducer's invariant does not constrain — #30's adapter — and
// the stamping must be right for it too, so it is tested at the seam #19 and
// #30 will actually compose.
func TestAGapUpBuyInTheOpenInstantPassCarriesTheSameProvenance(t *testing.T) {
	t.Parallel()

	simulator, err := fills.New(baselineConfig(), testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	proposed := restingEntryProposal(t, 157)
	if err := simulator.Observe(proposed); err != nil {
		t.Fatalf("Observe(trade proposal) error = %v", err)
	}

	// Opens at 160, well above the 157 the order rests at: the buy-stop was
	// triggered at the open and executed there.
	gapping := bar(day(57), 160, 161, 159, 160.5)
	handler := openCampaignOnEntryFill(t, proposed.ID)

	result, err := fills.RunBar(context.Background(), simulator, handler, barEnvelope(t, gapping))
	if err != nil {
		t.Fatalf("RunBar() error = %v", err)
	}

	fillEnvelopes := envelopesOfType(result.Inputs, event.FillEventType)
	if len(fillEnvelopes) != 1 {
		t.Fatalf("got %d fill(s), want exactly 1%s", len(fillEnvelopes), describe(result.Inputs))
	}
	entry := fillEnvelopes[0]
	if result.Inputs[0].Type != event.FillEventType {
		t.Errorf("the gapped buy did not precede the bar: %s", describe(result.Inputs))
	}
	// max(level 157, open 160) + 0.075.
	assertPrice(t, "gapped buy fill price", decodeFill(t, entry).Price, 160.075)

	wantRecordedAt := barEnvelope(t, gapping).RecordedAt
	if !entry.EventTime.Equal(gapping.PeriodEnd) {
		t.Errorf("gap fill EventTime = %s, want %s", entry.EventTime, gapping.PeriodEnd)
	}
	if !entry.RecordedAt.Equal(wantRecordedAt) {
		t.Errorf("gap fill RecordedAt = %s, want the gapping bar's own %s", entry.RecordedAt, wantRecordedAt)
	}
}

// TestIntrabarStopFillsEvenThoughTheBarClosedAboveIt is the ticket's named
// negative at the event seam: a bar whose low traded through the stop but
// whose close finished above it must still fill the stop. A close-only check
// — .greptile/rules.md's named failure mode — would keep the Campaign open.
func TestIntrabarStopFillsEvenThoughTheBarClosedAboveIt(t *testing.T) {
	t.Parallel()

	// #79 moves the stop to 152.575 (2 N below the entry fill of 155.575,
	// was 154.075 below the old 157.075), so the low must reach below that
	// new level to still cover it. The high is also lowered, to 156.2, so
	// this bar stays below the Add rung of 156.325 (0.75 above the entry
	// fill) — otherwise this bar would ALSO raise and fill an Add, which is
	// not what this test is about.
	intrabar := bar(day(57), 156, 156.2, 152.0, 156.1)
	bars := append(warmUpBars(), breakoutBar(), intrabar)
	run := runComposed(t, baselineConfig(), bars)

	if intrabar.SplitAdjusted.Close <= 152.575 {
		t.Fatal("the fixture is wrong: the bar must CLOSE above the stop, or it does not distinguish the two checks")
	}

	got := fillPayloads(t, run.Inputs)
	if len(got) != 2 || got[1].Kind != event.FillKindStop {
		t.Fatalf("want an entry followed by a stop fill, got %d fill(s)%s", len(got), describe(envelopesOfType(run.Inputs, event.FillEventType)))
	}
	// The bar did NOT gap through the stop (it opened at 156, above the
	// level), so the fill is at the level less slippage.
	assertPrice(t, "stop fill price", got[1].Price, 152.5)

	var exited event.CampaignExitedPayload
	decodeInto(t, onlyOfType(t, run.Decisions, event.CampaignExitedEventType), &exited)
	if exited.Reason != event.ExitReasonStop {
		t.Errorf("exited.Reason = %q, want %q", exited.Reason, event.ExitReasonStop)
	}
}

// --- #15's gap case: per-Unit stops at genuinely different levels ---------

// TestOnlyTheUnitsWhoseOwnStopWasReachedAreStopped is The Turtle Rules
// p.22-23's Crude example in miniature. Unit 2 fills far above its rung
// because the bar gapped, so its own stop sits 2.25 above Unit 1's raised
// stop; the next bar reaches Unit 2's stop and not Unit 1's, and exactly one
// Unit closes.
func TestOnlyTheUnitsWhoseOwnStopWasReachedAreStopped(t *testing.T) {
	t.Parallel()

	bars := append(warmUpBars(),
		breakoutBar(),
		// #79: the rung is now 156.325 (0.75 above the 155.575 entry fill,
		// was 157.825 above the old 157.075 one). This bar still gaps above
		// it either way: Unit 2 fills at the OPEN, 160.075, leaving its own
		// stop at 157.075 (unchanged: the gap fill price never depended on
		// the rung) while Unit 1's rises only to 153.325 (was 154.825).
		bar(day(57), 160, 160.5, 159.5, 160.2),
		// Reaches 156: below Unit 2's stop, above Unit 1's.
		bar(day(58), 160, 160.2, 156, 157),
	)
	run := runComposed(t, baselineConfig(), bars)

	got := fillPayloads(t, run.Inputs)
	if len(got) != 3 {
		t.Fatalf("got %d fill(s), want 3 (entry, gapped Add, partial stop)%s", len(got), describe(envelopesOfType(run.Inputs, event.FillEventType)))
	}
	add := got[1]
	if add.Kind != event.FillKindAdd {
		t.Fatalf("second fill Kind = %q, want %q", add.Kind, event.FillKindAdd)
	}
	assertPrice(t, "add fill level", add.Level, 156.325)
	// max(rung 156.325, open 160) + 0.075: the gap rule, applied to a buy.
	assertPrice(t, "add fill price", add.Price, 160.075)

	stop := got[2]
	if stop.Kind != event.FillKindStop {
		t.Fatalf("third fill Kind = %q, want %q", stop.Kind, event.FillKindStop)
	}
	assertPrice(t, "stop fill level", stop.Level, 157.075)
	assertPrice(t, "stop fill price", stop.Price, 157.0)
	if stop.Quantity != fixtureUnitQuantity {
		t.Errorf("stop fill quantity = %d, want one Unit (%d)", stop.Quantity, fixtureUnitQuantity)
	}
	if len(stop.UnitIDs) != 1 {
		t.Fatalf("stop fill names %d unit(s), want exactly 1 (Unit 2 alone)", len(stop.UnitIDs))
	}
	if stop.UnitIDs[0] != add.FillID {
		t.Errorf("stop fill names unit %q, want Unit 2's own opening fill %q", stop.UnitIDs[0], add.FillID)
	}

	var stopped event.CampaignUnitsStoppedPayload
	decodeInto(t, onlyOfType(t, run.Decisions, event.CampaignUnitsStoppedEventType), &stopped)
	if stopped.RemainingUnits != 1 {
		t.Errorf("stopped.RemainingUnits = %d, want 1: Unit 1's own stop was never reached", stopped.RemainingUnits)
	}
	if got := envelopesOfType(run.Decisions, event.CampaignExitedEventType); len(got) != 0 {
		t.Errorf("got %d campaign-exited event(s), want none: the Campaign still holds Unit 1", len(got))
	}
}

// --- Indistinguishability and replay -------------------------------------

// TestSimulatorFillsAreIndistinguishableFromAdapterFills is the ticket's
// "fill events are indistinguishable in shape from adapter-produced fills"
// criterion, asserted the only way that claim can be: feed the reducer the
// identical payload from a simulator-sourced envelope and from an
// adapter-sourced one, and require byte-identical emissions.
//
// What differs between the two runs is the envelope's Source — the one field
// that says who produced the fill. If the reducer's behaviour depended on the
// producer at all, this test would see it.
func TestSimulatorFillsAreIndistinguishableFromAdapterFills(t *testing.T) {
	t.Parallel()

	bars := append(warmUpBars(), breakoutBar(), bar(day(57), 150, 151, 148, 149))
	run := runComposed(t, baselineConfig(), bars)

	simulated := run.Inputs
	adapterShaped := make([]event.Envelope, len(simulated))
	copy(adapterShaped, simulated)
	var rewritten int
	for i, e := range adapterShaped {
		if e.Type != event.FillEventType {
			continue
		}
		// Every fill the simulator produced must already satisfy the shared
		// contract on its own, with no help from this package.
		if err := e.Validate(); err != nil {
			t.Fatalf("simulator fill envelope is invalid: %v", err)
		}
		if err := decodeFill(t, e).Validate(); err != nil {
			t.Fatalf("simulator fill payload is invalid: %v", err)
		}
		if e.Source != "simulator" {
			t.Errorf("fill envelope Source = %q, want %q", e.Source, "simulator")
		}
		adapterShaped[i].Source = "lean-adapter"
		rewritten++
	}
	if rewritten == 0 {
		t.Fatal("the fixture produced no fills to re-source")
	}

	fromSimulator := replayThrough(t, simulated)
	fromAdapter := replayThrough(t, adapterShaped)
	assertIdentical(t, fromSimulator, fromAdapter)
}

// TestComposedRunReplaysByteIdentically: the input stream the backtest loop
// produced is a journal, and replaying a journal must reproduce the decisions
// exactly (#20). Two independent replays through a fresh reducer each time,
// compared byte for byte.
func TestComposedRunReplaysByteIdentically(t *testing.T) {
	t.Parallel()

	run := runComposed(t, baselineConfig(), campaignLifeBars())

	first := replayThrough(t, run.Inputs)
	second := replayThrough(t, run.Inputs)
	assertIdentical(t, first, second)

	// And the loop's own observation of what the reducer emitted matches
	// what a replay of its input stream produces — the property #19's
	// backtest loop depends on when it writes one journal and expects a
	// later replay of it to agree.
	if len(first) != len(run.Decisions) {
		t.Fatalf("replay emitted %d decision(s), the composed run observed %d", len(first), len(run.Decisions))
	}
	for i := range first {
		if first[i].Type != run.Decisions[i].Type {
			t.Fatalf("decision %d type = %q on replay, %q in the composed run", i, first[i].Type, run.Decisions[i].Type)
		}
		if !bytes.Equal(first[i].Payload, run.Decisions[i].Payload) {
			t.Fatalf("decision %d (%s) payload differs between the composed run and its replay", i, first[i].Type)
		}
	}
}

// TestTheComposedInputStreamIsContiguous: replay.Engine.Run refuses a stream
// with a gap, so the loop must number what it produces. The bar producer's
// own sequence is deliberately overwritten — the simulator interleaves fills
// into the stream, so only the loop can know the running order.
func TestTheComposedInputStreamIsContiguous(t *testing.T) {
	t.Parallel()

	run := runComposed(t, baselineConfig(), campaignLifeBars())
	for i, e := range run.Inputs {
		if e.Sequence != uint64(i+1) {
			t.Fatalf("input %d has sequence %d, want %d", i, e.Sequence, i+1)
		}
	}
}

func assertIdentical(t *testing.T, a, b []event.Envelope) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("emitted %d envelope(s) and %d envelope(s)%s%s", len(a), len(b), describe(a), describe(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Type != b[i].Type || a[i].Sequence != b[i].Sequence ||
			a[i].CausationID != b[i].CausationID || a[i].CorrelationID != b[i].CorrelationID ||
			a[i].PayloadHash != b[i].PayloadHash || !bytes.Equal(a[i].Payload, b[i].Payload) {
			t.Fatalf("envelope %d differs:\n  %+v\n  %+v", i, a[i], b[i])
		}
	}
}

// --- Construction and fail-closed behaviour ------------------------------

// TestNewRefusesAZeroSlippageConfiguration is the ticket's second named
// negative. ADR 0013: "A backtest run with zero slippage is invalid by
// construction and must be rejected by the run registry" — and by the
// simulator, which is the component that would otherwise quietly produce
// unslipped fills.
func TestNewRefusesAZeroSlippageConfiguration(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		slippageN float64
	}{
		{"zero", 0},
		{"negative", -0.05},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := baselineConfig()
			cfg.SlippageN = tc.slippageN

			_, err := fills.New(cfg, testStrategyVersion, testConfigurationHash)
			if err == nil {
				t.Fatal("New() error = nil, want a refusal")
			}
			if !strings.Contains(err.Error(), "slippage") {
				t.Errorf("New() error = %v, want it to name the slippage", err)
			}
		})
	}
}

func TestNewFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		cfg               func() event.ConfigurationPayload
		strategyVersion   string
		configurationHash string
		wantErr           string
	}{
		{
			name:            "missing strategy version",
			cfg:             baselineConfig,
			strategyVersion: "", configurationHash: testConfigurationHash,
			wantErr: "strategy version",
		},
		{
			name:            "missing configuration hash",
			cfg:             baselineConfig,
			strategyVersion: testStrategyVersion, configurationHash: "",
			wantErr: "configuration hash",
		},
		{
			name: "invalid configuration",
			cfg: func() event.ConfigurationPayload {
				cfg := baselineConfig()
				cfg.MaxUnits = 0
				return cfg
			},
			strategyVersion: testStrategyVersion, configurationHash: testConfigurationHash,
			wantErr: "maximum units",
		},
		{
			name: "commission model without a cap",
			cfg: func() event.ConfigurationPayload {
				cfg := baselineConfig()
				cfg.Commission.MaximumFractionOfTradeValue = 0
				return cfg
			},
			strategyVersion: testStrategyVersion, configurationHash: testConfigurationHash,
			wantErr: "commission",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := fills.New(tt.cfg(), tt.strategyVersion, tt.configurationHash)
			if err == nil {
				t.Fatalf("New() error = nil, want an error naming %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("New() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// TestObserveFailsClosedOnAnUnrecognisedEventType: the simulator learns its
// whole resting-order book from the reducer's emissions, so an emission it
// does not recognise might be one that creates or cancels an order. Silently
// ignoring it would leave the book quietly wrong — the same fail-closed rule
// Reducer.Apply applies to its own inputs (docs/development.md principle 4).
func TestObserveFailsClosedOnAnUnrecognisedEventType(t *testing.T) {
	t.Parallel()

	simulator, err := fills.New(baselineConfig(), testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	stray := envelope(t, "stray-1", "strategy.something.new", 1, day(1), map[string]string{"instrument_id": testInstrument})
	if err := simulator.Observe(stray); err == nil {
		t.Fatal("Observe() error = nil, want a refusal for an unrecognised event type")
	} else if !strings.Contains(err.Error(), "strategy.something.new") {
		t.Errorf("Observe() error = %v, want it to name the unrecognised type", err)
	}
}

// TestRunBarRequiresACompletedBar: RunBar is the per-bar protocol, and every
// step of it is about one completed bar. Handing it anything else is a
// programming error in the driver, not an input to absorb.
func TestRunBarRequiresACompletedBar(t *testing.T) {
	t.Parallel()

	simulator, err := fills.New(baselineConfig(), testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	reducer, err := strategy.NewReducer(testStrategyVersion, baselineConfig())
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}

	_, err = fills.RunBar(context.Background(), simulator, reducer, configurationEnvelope(t, baselineConfig()))
	if err == nil {
		t.Fatal("RunBar() error = nil, want a refusal for a non-bar envelope")
	}
	if !strings.Contains(err.Error(), event.CompletedBarEventType) {
		t.Errorf("RunBar() error = %v, want it to name the event type it requires", err)
	}
}

// TestDeliverRefusesACompletedBar is RunBar's own guard from the other side:
// the two entry points are not interchangeable, because only one of them
// applies the per-bar protocol. A bar delivered through Deliver would reach
// the reducer with no fills around it at all.
func TestDeliverRefusesACompletedBar(t *testing.T) {
	t.Parallel()

	simulator, reducer := newComposed(t, baselineConfig())
	_, err := fills.Deliver(context.Background(), simulator, reducer, barEnvelope(t, breakoutBar()))
	if err == nil {
		t.Fatal("Deliver() error = nil, want a refusal for a completed bar")
	}
	if !strings.Contains(err.Error(), "RunBar") {
		t.Errorf("Deliver() error = %v, want it to name RunBar", err)
	}
}

// TestTheDriverEntryPointsRequireASimulatorAndAHandler: a nil either side is
// a programming error in the driver, and it fails rather than panicking.
func TestTheDriverEntryPointsRequireASimulatorAndAHandler(t *testing.T) {
	t.Parallel()

	simulator, reducer := newComposed(t, baselineConfig())
	ctx := context.Background()

	if _, err := fills.RunBar(ctx, nil, reducer, barEnvelope(t, breakoutBar())); err == nil {
		t.Error("RunBar(nil simulator) error = nil, want a refusal")
	}
	if _, err := fills.RunBar(ctx, simulator, nil, barEnvelope(t, breakoutBar())); err == nil {
		t.Error("RunBar(nil handler) error = nil, want a refusal")
	}
	if _, err := fills.Deliver(ctx, nil, reducer, configurationEnvelope(t, baselineConfig())); err == nil {
		t.Error("Deliver(nil simulator) error = nil, want a refusal")
	}
	if _, err := fills.Deliver(ctx, simulator, nil, configurationEnvelope(t, baselineConfig())); err == nil {
		t.Error("Deliver(nil handler) error = nil, want a refusal")
	}
}

// TestRunBarRefusesAnInvalidBar: the bar is the one input the whole protocol
// reads, and it is validated before a single order is priced against it — an
// inconsistent bar (a low above its high, say) would otherwise decide fills.
func TestRunBarRefusesAnInvalidBar(t *testing.T) {
	t.Parallel()

	simulator, reducer := newComposed(t, baselineConfig())
	broken := bar(day(56), 155.5, 157, 155, 156.5)
	broken.SplitAdjusted.Low = 200 // above its own high

	_, err := fills.RunBar(context.Background(), simulator, reducer, barEnvelope(t, broken))
	if err == nil {
		t.Fatal("RunBar() error = nil, want a refusal for an internally inconsistent bar")
	}
	if !strings.Contains(err.Error(), "high must be at least low") {
		t.Errorf("RunBar() error = %v, want it to name the inconsistency", err)
	}
}

// TestRunBarReturnsTheReducersErrorAndWhatItEmittedAlongsideIt keeps
// replay.Handler's contract (as of #12): a handler that fails closed may emit
// a final event explaining why, and that event must still reach the journal.
// Here the reducer refuses a bar before any configuration has been applied.
func TestRunBarReturnsTheReducersErrorAndWhatItEmittedAlongsideIt(t *testing.T) {
	t.Parallel()

	simulator, reducer := newComposed(t, baselineConfig())
	result, err := fills.RunBar(context.Background(), simulator, reducer, barEnvelope(t, breakoutBar()))
	if err == nil {
		t.Fatal("RunBar() error = nil, want the reducer's own refusal")
	}
	if !strings.Contains(err.Error(), "configuration") {
		t.Errorf("RunBar() error = %v, want the reducer's reason", err)
	}
	if len(result.Inputs) != 1 {
		t.Errorf("got %d input(s), want the bar itself: what was applied must still be journalled", len(result.Inputs))
	}
}

// TestProposalsAreAlwaysCoveredByTheBarThatRaisedThem records a structural
// finding, as a test so it cannot rot: under ADR 0005's fill model this
// reducer never raises a proposal that its own bar does not already cover —
// UNCHANGED by #79, though the reasoning for the entry case is now different.
//
// Before #79, an entry rested at the breakout bar's own high, which that
// bar's own range trivially reaches (High >= High) — always true, but for
// the wrong reason. After #79 an entry rests at the Entry Channel high
// instead, a level fixed BEFORE the bar opened; what makes the bar still
// cover it is the Signal's own invariant (SignalPayload.Validate: BreakoutHigh
// strictly exceeds EntryChannelHigh, The Turtle Rules p.19's "exceeds"), which
// guarantees bar.High > EntryLevel for the very bar that raised the proposal.
// An Add fills once the bar's high has reached its rung, and an exit once the
// bar's low has broken the Exit Channel — so every proposal still fills
// inside the bar that raised it, and ADR 0011's next-bar expiry remains
// unreached by the simulator for any of the four proposal kinds (recorded in
// issue #79's Findings).
//
// The consequence matters for the protocol: the only orders that ever rest
// from one bar into the next are Protective Stops.
func TestProposalsAreAlwaysCoveredByTheBarThatRaisedThem(t *testing.T) {
	t.Parallel()

	run := runComposed(t, baselineConfig(), campaignLifeBars())

	if got := envelopesOfType(run.Decisions, event.ProposalExpiredEventType); len(got) != 0 {
		t.Errorf("got %d proposal-expired event(s), want none%s", len(got), describe(got))
	}
	proposals := 0
	for _, e := range run.Decisions {
		switch e.Type {
		case event.TradeProposalEventType, event.AddProposalEventType, event.ExitProposalEventType:
			proposals++
		}
	}
	fillCount := len(envelopesOfType(run.Inputs, event.FillEventType))
	if proposals != fillCount {
		t.Errorf("the fixture raised %d proposal(s) and produced %d fill(s); every proposal should have filled in its own bar", proposals, fillCount)
	}
}
