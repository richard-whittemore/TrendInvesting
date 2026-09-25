package strategy_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// This file holds the tests for a split's cash in lieu (CONTEXT.md: "Cash in
// lieu"; ADR 0023). The engine's split-adjusted view is adjusted for every
// split already (ADR 0004, as amended), so a split changes none of its
// figures; the corporate action carries only the whole raw shares the
// broker could not deliver and the cash it paid for them, and the reducer
// takes one raw share off each of the most recent Units.
//
// The fixture is add_test.go's three-Unit Campaign: 133 split-adjusted
// shares a Unit, opened at day(56) with Adds at day(57) and day(58). Its
// bars state both views alike, so after the split one engine share is one
// raw share. The headline case is a synthetic 7-for-1 whose rounded factor
// left the broker one share short, paid for as 0.99 of a share at the
// split-adjusted close.

// splitCampaign is the three-Unit Campaign, its fill prices, and the stream
// that opened it, ending after day(58)'s bar and Add fill.
type splitCampaign struct {
	stream     *stream
	campaignID string
	fills      [3]float64
}

func newSplitCampaign(t *testing.T) splitCampaign {
	t.Helper()
	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}
	rung3, err := sizing.NextAddLevel(rung2, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}
	s := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(addOpportunityBar("AAPL", day(57), rung2+5)).
		fill(addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", rung2, 133, day(57))).
		bar(addOpportunityBar("AAPL", day(58), rung3+5)).
		fill(addFill("AAPL", campaignID, 3, day(58), "sim-fill-add-3", rung3, 133, day(58)))
	return splitCampaign{stream: s, campaignID: campaignID, fills: [3]float64{campaignFillPrice, rung2, rung3}}
}

// splitAt is the moment between day(58)'s Session and day(59)'s at which the
// fixtures' splits take effect.
var splitAt = day(58).Add(12 * time.Hour)

// splitAction is a synthetic 7-for-1 split of AAPL at when, one engine share
// a raw share after it, that lost lost raw shares and paid cash for them.
func splitAction(when time.Time, lost int64, cash float64) event.CorporateActionPayload {
	return event.CorporateActionPayload{
		InstrumentID:            "AAPL",
		Kind:                    event.CorporateActionKindSplit,
		EffectiveAt:             when,
		NewShares:               7,
		OldShares:               1,
		EngineSharesPerRawShare: 1,
		RawSharesLost:           lost,
		CashInLieu:              cash,
		Currency:                "USD",
	}
}

// causedBy is every emission whose cause is the input with the given id.
func causedBy(emitted []event.Envelope, inputID string) []event.Envelope {
	var out []event.Envelope
	for _, e := range emitted {
		if e.CausationID == inputID {
			out = append(out, e)
		}
	}
	return out
}

func decodeCashInLieu(t *testing.T, envelope event.Envelope) event.CampaignCashInLieuPayload {
	t.Helper()
	if envelope.Type != event.CampaignCashInLieuEventType || envelope.SchemaVersion != event.CampaignCashInLieuSchemaVersion {
		t.Fatalf("envelope = %s/%d, want %s/%d", envelope.Type, envelope.SchemaVersion, event.CampaignCashInLieuEventType, event.CampaignCashInLieuSchemaVersion)
	}
	var payload event.CampaignCashInLieuPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	if err := payload.Validate(); err != nil {
		t.Fatalf("emitted cash in lieu payload fails its own Validate(): %v", err)
	}
	return payload
}

// lastExitOrderFor is the Exit Order last set for unitIndex among emitted.
func lastExitOrderFor(t *testing.T, emitted []event.Envelope, unitIndex int) event.ExitOrderSetPayload {
	t.Helper()
	var last *event.ExitOrderSetPayload
	for _, o := range exitOrdersIn(t, emitted) {
		if o.UnitIndex == unitIndex {
			o := o
			last = &o
		}
	}
	if last == nil {
		t.Fatalf("no exit order was ever set for unit %d", unitIndex)
	}
	return *last
}

// TestASplitsCashInLieuReducesTheMostRecentUnit is the headline reducer-seam
// case: a rounded 7-for-1 left the broker one share short of the Units, and
// paid for it. The most recent Unit, and only it, loses one raw share, its
// Exit Order is re-stated at its unchanged level for its new quantity, and
// the Campaign's later exit reports its whole life, the lost share and its
// cash included (ADR 0023).
func TestASplitsCashInLieuReducesTheMostRecentUnit(t *testing.T) {
	t.Parallel()

	c := newSplitCampaign(t)
	cfg := c.stream.cfg
	const cash = 0.99 * 150 // the fraction LEAN kept, at the split-adjusted close
	s := c.stream.corporateAction(splitAction(splitAt, 1, cash))
	splitID := s.envelopes[len(s.envelopes)-1].ID
	before := s.mustRun()
	unit3Before := lastExitOrderFor(t, before[:len(before)-len(causedBy(before, splitID))], 3)

	fromSplit := causedBy(before, splitID)
	if len(fromSplit) != 2 {
		t.Fatalf("the split caused %d decision(s), want 2 (the cash in lieu, then unit 3's Exit Order): %v", len(fromSplit), fromSplit)
	}
	got := decodeCashInLieu(t, fromSplit[0])
	want := event.CampaignCashInLieuPayload{
		CampaignID:              c.campaignID,
		InstrumentID:            "AAPL",
		CorporateActionID:       splitID,
		EffectiveAt:             splitAt,
		NewShares:               7,
		OldShares:               1,
		EngineSharesPerRawShare: 1,
		RawSharesLost:           1,
		EngineSharesLost:        1,
		CashInLieu:              cash,
		Currency:                "USD",
		Reductions:              []event.UnitReduction{{UnitIndex: 3, QuantityBefore: 133, QuantityAfter: 132}},
		QuantityBefore:          399,
		QuantityAfter:           398,
		Rule:                    event.RuleCashInLieuMostRecentUnitsFirst,
		ADR:                     event.ADRSplitCashInLieu,
	}
	if gotJSON, wantJSON := mustMarshal(t, got), mustMarshal(t, want); string(gotJSON) != string(wantJSON) {
		t.Fatalf("cash in lieu =\n  %s\nwant\n  %s", gotJSON, wantJSON)
	}
	order := decodeExitOrder(t, fromSplit[1])
	wantExitOrder(t, order, 3, unit3Before.Level, unit3Before.Source, unit3Before.ProtectiveStop, unit3Before.ExitChannelLevel, 132)
	if !order.AsOf.Equal(splitAt) {
		t.Errorf("exit order as of %s, want the split's own %s", order.AsOf, splitAt)
	}

	// The exit that follows sells what is held, 398, and the Campaign's
	// whole life counts all 399 shares: the lost one at its Unit's own entry
	// price and at the cash paid for it, exactly as a partial stop's are
	// (campaignState.lifeAggregate).
	breachAt := day(59)
	exitFill := closingExitFill("AAPL", c.campaignID, 100, breachAt, day(60))
	exitFill.Quantity = 398
	emitted := s.bar(postEntryBar("AAPL", breachAt, 99)).fill(exitFill).mustRun()
	proposal := decodeExitProposal(t, onlyEnvelopeOfType(t, emitted, event.ExitProposalEventType))
	if proposal.Quantity != 398 {
		t.Errorf("exit proposal quantity = %d, want the 398 the Campaign holds after the split", proposal.Quantity)
	}
	exited := decodeCampaignExited(t, onlyEnvelopeOfType(t, emitted, event.CampaignExitedEventType))
	closedEntry := sizing.Product(1, c.fills[2])
	closedExit := cash / cfg.DollarsPerPoint
	var heldEntry float64
	for i, q := range []int64{133, 133, 132} {
		heldEntry += sizing.Product(float64(q), c.fills[i])
	}
	wantEntry := (closedEntry + heldEntry) / 399
	wantExit := (closedExit + sizing.Product(398, exitFill.Price)) / 399
	if exited.Quantity != 399 || exited.EntryPrice != wantEntry || exited.ExitPrice != wantExit {
		t.Fatalf("exited = {quantity %d, entry %v, exit %v}, want {399, %v, %v}", exited.Quantity, exited.EntryPrice, exited.ExitPrice, wantEntry, wantExit)
	}
	if want := float64(399) * (wantExit - wantEntry) * cfg.DollarsPerPoint; exited.RealisedResult != want {
		t.Fatalf("realised result = %v, want %v", exited.RealisedResult, want)
	}
	if exited.Units != 3 {
		t.Errorf("Units = %d, want 3: a reduced Unit is still a Unit", exited.Units)
	}
}

// TestEverySplitOfAnInstrumentApplies: a second, later split is applied on
// its own terms, the most recent Units first again, one raw share each.
func TestEverySplitOfAnInstrumentApplies(t *testing.T) {
	t.Parallel()

	c := newSplitCampaign(t)
	second := splitAction(splitAt.Add(time.Hour), 2, 2*0.99*150)
	second.NewShares, second.OldShares = 2, 1
	s := c.stream.corporateAction(splitAction(splitAt, 1, 148.5)).corporateAction(second)
	secondID := s.envelopes[len(s.envelopes)-1].ID
	emitted := s.mustRun()

	fromSecond := causedBy(emitted, secondID)
	if len(fromSecond) != 3 {
		t.Fatalf("the second split caused %d decision(s), want 3", len(fromSecond))
	}
	got := decodeCashInLieu(t, fromSecond[0])
	want := []event.UnitReduction{{UnitIndex: 3, QuantityBefore: 132, QuantityAfter: 131}, {UnitIndex: 2, QuantityBefore: 133, QuantityAfter: 132}}
	if string(mustMarshal(t, got.Reductions)) != string(mustMarshal(t, want)) {
		t.Fatalf("reductions = %+v, want %+v", got.Reductions, want)
	}
	if got.QuantityBefore != 398 || got.QuantityAfter != 396 {
		t.Fatalf("holding %d -> %d, want 398 -> 396", got.QuantityBefore, got.QuantityAfter)
	}
	if o2, o3 := decodeExitOrder(t, fromSecond[1]), decodeExitOrder(t, fromSecond[2]); o2.UnitIndex != 2 || o2.Quantity != 132 || o3.UnitIndex != 3 || o3.Quantity != 131 {
		t.Fatalf("exit orders = {unit %d: %d, unit %d: %d}, want {unit 2: 132, unit 3: 131}", o2.UnitIndex, o2.Quantity, o3.UnitIndex, o3.Quantity)
	}
}

// TestARawShareIsTheEnginesSharesPerRawShare: after a split whose adjusted
// view still has four engine shares a raw share, one raw share lost is four
// engine shares off each reduced Unit.
func TestARawShareIsTheEnginesSharesPerRawShare(t *testing.T) {
	t.Parallel()

	c := newSplitCampaign(t)
	action := splitAction(splitAt, 2, 300)
	action.EngineSharesPerRawShare = 4
	s := c.stream.corporateAction(action)
	id := s.envelopes[len(s.envelopes)-1].ID
	got := decodeCashInLieu(t, causedBy(s.mustRun(), id)[0])
	want := []event.UnitReduction{{UnitIndex: 3, QuantityBefore: 133, QuantityAfter: 129}, {UnitIndex: 2, QuantityBefore: 133, QuantityAfter: 129}}
	if string(mustMarshal(t, got.Reductions)) != string(mustMarshal(t, want)) || got.EngineSharesLost != 8 || got.QuantityAfter != 391 {
		t.Fatalf("cash in lieu = %+v, want reductions %+v, 8 engine shares lost, 391 held", got, want)
	}
}

// TestCashWithNoShareLostReducesNoUnit is the 2005 2-for-1 shape: the broker
// paid for a fraction without losing a whole share. The cash is recorded; no
// Unit and no Exit Order changes, and the Campaign's exit counts the cash.
func TestCashWithNoShareLostReducesNoUnit(t *testing.T) {
	t.Parallel()

	c := newSplitCampaign(t)
	s := c.stream.corporateAction(splitAction(splitAt, 0, 2.75))
	id := s.envelopes[len(s.envelopes)-1].ID
	breachAt := day(59)
	exitFill := closingExitFill("AAPL", c.campaignID, 100, breachAt, day(60))
	exitFill.Quantity = 399
	emitted := s.bar(postEntryBar("AAPL", breachAt, 99)).fill(exitFill).mustRun()

	fromSplit := causedBy(emitted, id)
	if len(fromSplit) != 1 {
		t.Fatalf("the split caused %d decision(s), want only the cash in lieu", len(fromSplit))
	}
	got := decodeCashInLieu(t, fromSplit[0])
	if len(got.Reductions) != 0 || got.QuantityBefore != 399 || got.QuantityAfter != 399 || got.CashInLieu != 2.75 {
		t.Fatalf("cash in lieu = %+v, want no reduction, 399 held, 2.75 paid", got)
	}
	exited := decodeCampaignExited(t, onlyEnvelopeOfType(t, emitted, event.CampaignExitedEventType))
	var heldEntry float64
	for _, price := range c.fills {
		heldEntry += sizing.Product(133, price)
	}
	wantExit := (2.75/c.stream.cfg.DollarsPerPoint + sizing.Product(399, exitFill.Price)) / 399
	if exited.Quantity != 399 || exited.EntryPrice != heldEntry/399 || exited.ExitPrice != wantExit {
		t.Fatalf("exited = {quantity %d, entry %v, exit %v}, want {399, %v, %v}", exited.Quantity, exited.EntryPrice, exited.ExitPrice, heldEntry/399, wantExit)
	}
}

// TestAnExactSplitChangesNothing: a split that lost nothing and paid nothing
// is recorded as an input and decides nothing; the Campaign exits exactly as
// it would have without it.
func TestAnExactSplitChangesNothing(t *testing.T) {
	t.Parallel()

	c := newSplitCampaign(t)
	s := c.stream.corporateAction(splitAction(splitAt, 0, 0))
	id := s.envelopes[len(s.envelopes)-1].ID
	if got := causedBy(s.mustRun(), id); len(got) != 0 {
		t.Fatalf("an exact split caused %d decision(s), want none", len(got))
	}
}

// TestASplitShortfallOtherThanCashInLieuHalts pins ADR 0023's bound, and ADR
// 0019's "never adopt a balance" behind it: a shortfall of more than one raw
// share per Unit, a reduction that would empty a Unit, a shortfall or cash
// with nothing held, a repeated or stale split, and cash in another
// currency all fail closed, and change nothing.
func TestASplitShortfallOtherThanCashInLieuHalts(t *testing.T) {
	t.Parallel()

	wholeUnit := splitAction(splitAt, 1, 100)
	wholeUnit.EngineSharesPerRawShare = 133
	otherCurrency := splitAction(splitAt, 1, 100)
	otherCurrency.Currency = "EUR"
	tests := []struct {
		name   string
		action event.CorporateActionPayload
		want   []string
	}{
		{"more than one raw share per Unit", splitAction(splitAt, 4, 400), []string{"4 raw share(s)", "3 Unit(s)", "ADR 0019"}},
		{"a Unit left with nothing", wholeUnit, []string{"unit 3", "at least one share"}},
		{"cash in another currency", otherCurrency, []string{"EUR", "USD"}},
		{"a split before the last completed bar", splitAction(day(57), 1, 100), []string{"predates", "last completed bar"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			newSplitCampaign(t).stream.corporateAction(tt.action).wantRunError(tt.want...)
		})
	}

	t.Run("a split before a fill the Campaign accepted", func(t *testing.T) {
		t.Parallel()
		// Unit 3 is stopped by a fill stamped at the next Session's close,
		// delivered before that Session's bar as a producer reports it; a
		// split stated to take effect before that fill cannot have changed
		// the shares it sold.
		c := newSplitCampaign(t)
		stop := event.FillPayload{
			InstrumentID: "AAPL",
			Kind:         event.FillKindStop,
			CampaignID:   c.campaignID,
			FillID:       "sim-fill-stop-3",
			UnitIDs:      []string{"sim-fill-add-3"},
			Direction:    event.DirectionLong,
			Quantity:     133,
			Price:        150,
			FilledAt:     day(59),
		}
		c.stream.fill(stop).corporateAction(splitAction(splitAt, 1, 100)).
			wantRunError("predates", "fill")
	})
	t.Run("the same split twice", func(t *testing.T) {
		t.Parallel()
		newSplitCampaign(t).stream.
			corporateAction(splitAction(splitAt, 1, 100)).
			corporateAction(splitAction(splitAt, 1, 100)).
			wantRunError("applies once", splitAt.Format(time.RFC3339))
	})
	t.Run("an earlier split after a later one", func(t *testing.T) {
		t.Parallel()
		newSplitCampaign(t).stream.
			corporateAction(splitAction(splitAt, 0, 0)).
			corporateAction(splitAction(splitAt.Add(-time.Hour), 0, 0)).
			wantRunError("applies once")
	})
	t.Run("a shortfall with no Campaign open", func(t *testing.T) {
		t.Parallel()
		newStream(t, validConfigurationPayload()).
			bars(breakoutBars("AAPL")[:10]).
			corporateAction(splitAction(day(10).Add(time.Hour), 1, 100)).
			wantRunError("no open Campaign", "ADR 0019")
	})
	t.Run("cash with no Campaign open", func(t *testing.T) {
		t.Parallel()
		newStream(t, validConfigurationPayload()).
			bars(breakoutBars("AAPL")[:10]).
			corporateAction(splitAction(day(10).Add(time.Hour), 0, 2.75)).
			wantRunError("no open Campaign")
	})
	t.Run("a shortfall for an instrument never seen", func(t *testing.T) {
		t.Parallel()
		newStream(t, validConfigurationPayload()).
			corporateAction(splitAction(day(10), 1, 100)).
			wantRunError("no open Campaign")
	})
	t.Run("a split of a delisted instrument", func(t *testing.T) {
		t.Parallel()
		newStream(t, validConfigurationPayload()).
			bars(breakoutBars("AAPL")[:10]).
			corporateAction(delistingAction("AAPL", day(10))).
			corporateAction(splitAction(day(10).Add(time.Hour), 0, 0)).
			wantRunError("delisted")
	})
	t.Run("a split inside an open Session", func(t *testing.T) {
		t.Parallel()
		bars := breakoutBars("AAPL")
		newStream(t, validConfigurationPayload()).
			bars(bars[:10]).
			barOnly(bars[10]).
			corporateAction(splitAction(bars[10].PeriodEnd, 0, 0)).
			wantRunError("between Sessions")
	})
}

// TestARefusedSplitLeavesTheUnitsUntouched is the transaction boundary (the
// reducer's copy-on-write discipline, docs/development.md): after a split is
// refused, the same reducer still holds all 399 shares, so an exit for 399
// is accepted and one for 398 would not be.
func TestARefusedSplitLeavesTheUnitsUntouched(t *testing.T) {
	t.Parallel()

	c := newSplitCampaign(t)
	reducer := c.stream.mustApply()
	refused := corporateActionEnvelope(t, c.stream.seq+1, splitAction(splitAt, 4, 400))
	if _, err := reducer.Apply(t.Context(), refused); err == nil {
		t.Fatal("Apply(split losing 4 of 3 Units' shares) error = nil, want a refusal")
	}
	reduced := corporateActionEnvelope(t, c.stream.seq+1, splitAction(splitAt, 1, 100))
	emitted, err := reducer.Apply(t.Context(), reduced)
	if err != nil {
		t.Fatalf("Apply(valid split after a refused one) error = %v", err)
	}
	got := decodeCashInLieu(t, emitted[0])
	if got.QuantityBefore != 399 || got.Reductions[0].QuantityBefore != 133 {
		t.Fatalf("after a refused split the Campaign held %d with unit 3 at %d, want 399 and 133", got.QuantityBefore, got.Reductions[0].QuantityBefore)
	}
}

// TestAnExactSplitWithNothingHeldIsANoOp: a split for an idle instrument, or
// one this reducer has never seen, that states no shortfall and no cash is
// accepted and decides nothing.
func TestAnExactSplitWithNothingHeldIsANoOp(t *testing.T) {
	t.Parallel()

	unseen := splitAction(day(10), 0, 0)
	unseen.InstrumentID = "MSFT"
	emitted := newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")[:10]).
		corporateAction(splitAction(day(10).Add(time.Hour), 0, 0)).
		corporateAction(unseen).
		mustRun()
	if got := len(envelopesOfType(emitted, event.CampaignCashInLieuEventType)); got != 0 {
		t.Fatalf("got %d cash in lieu decision(s), want none", got)
	}
}

// TestASchemaOneDelistingIsReadForward is ADR 0015's upcaster at the reducer
// seam: a delisting recorded at schema 1 is applied as the delisting it is.
func TestASchemaOneDelistingIsReadForward(t *testing.T) {
	t.Parallel()

	emitted := newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		corporateActionAtSchema(delistingAction("AAPL", day(56)), 1).
		mustRun()
	exited := decodeCampaignExited(t, onlyEnvelopeOfType(t, emitted, event.CampaignExitedEventType))
	if exited.Reason != event.ExitReasonDelisting {
		t.Fatalf("exit reason = %q, want %q", exited.Reason, event.ExitReasonDelisting)
	}
}
