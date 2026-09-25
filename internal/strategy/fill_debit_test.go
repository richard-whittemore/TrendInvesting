package strategy_test

import (
	"math"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// This file holds the fill-debit tests of ADR 0020: "the cash available to a
// Unit is the previous close's figure less what this bar's earlier fills
// have already spent". A buy fill's actual cost, commission included, is
// debited from snapshot-backed spendable cash as soon as the fill is
// recorded. A snapshot replaces the figure only for fills it can already
// reflect, those filled at or before its own as-of. Exit proceeds are never
// credited by a fill; they return only through a later snapshot ("debit
// within the bar, credit at the close").
//
// The fixtures reproduce the shape in which an Add Ladder spent more cash
// than the account held: every Unit was compared with one unmoved figure, so
// four Units could each pass while together costing several times the cash.

// fillCost is a fill's actual cost as ADR 0020 debits it: quantity x the
// executed price x dollars per point, plus the commission charged.
func fillCost(cfg event.ConfigurationPayload, fill event.FillPayload) float64 {
	return float64(float64(fill.Quantity)*fill.Price*cfg.DollarsPerPoint) + fill.Commission
}

// addRungs returns the Add Ladder's rungs 2, 3 and 4 for the breakout
// fixture's Campaign, each measured from a fill landing exactly on the rung
// before it (ADR 0006).
func addRungs(t *testing.T, cfg event.ConfigurationPayload) (rung2, rung3, rung4 float64) {
	t.Helper()
	campaignN := breakoutFixtureN(t, cfg)
	var err error
	if rung2, err = sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong); err != nil {
		t.Fatalf("NextAddLevel(rung 2) error = %v", err)
	}
	if rung3, err = sizing.NextAddLevel(rung2, campaignN, sizing.DirectionLong); err != nil {
		t.Fatalf("NextAddLevel(rung 3) error = %v", err)
	}
	if rung4, err = sizing.NextAddLevel(rung3, campaignN, sizing.DirectionLong); err != nil {
		t.Fatalf("NextAddLevel(rung 4) error = %v", err)
	}
	return rung2, rung3, rung4
}

// onlyInsufficientCashDecline asserts exactly one decline was emitted, for
// insufficient cash, and returns it.
func onlyInsufficientCashDecline(t *testing.T, emitted []event.Envelope) event.ProposalDeclinedPayload {
	t.Helper()
	declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1", len(declines))
	}
	if declines[0].SchemaVersion != event.ProposalDeclinedSchemaVersion {
		t.Errorf("decline schema = %d, want %d", declines[0].SchemaVersion, event.ProposalDeclinedSchemaVersion)
	}
	decline := decodeProposalDeclined(t, declines[0])
	if decline.Reason != event.DeclineReasonInsufficientCash {
		t.Fatalf("Reason = %q, want %q", decline.Reason, event.DeclineReasonInsufficientCash)
	}
	if err := decline.Validate(); err != nil {
		t.Errorf("emitted decline fails its own Validate(): %v", err)
	}
	return decline
}

// TestAChainedAddIsCheckedAgainstCashLessTheFillsBeforeIt is the fill-chained
// path (evaluateAdd called from applyAddFill; ADR 0021 section 7). One bar
// covers every rung, so Units 2, 3 and 4 are each proposed in reply to the
// fill before them. The snapshot funds every Unit alone, and Units 1 to 3
// together, but not all four: after three fills the fourth rung cannot be
// funded, and it is declined carrying the balance that remained.
//
// Each fill carries a commission, and the cash is short of Unit 4's cost by
// less than the three commissions together, so a debit that ignored
// commission would still propose Unit 4.
func TestAChainedAddIsCheckedAgainstCashLessTheFillsBeforeIt(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	rung2, rung3, rung4 := addRungs(t, cfg)
	const commission = 1.5

	opening := openingFill("AAPL")
	opening.Commission = commission
	fill2 := addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", rung2, 133, day(57))
	fill2.Commission = commission
	fill3 := addFill("AAPL", campaignID, 3, day(57), "sim-fill-add-3", rung3, 133, day(57))
	fill3.Commission = commission

	unit4Cost := wantHold(cfg, 133, rung4, breakoutFixtureN(t, cfg))
	cash := fillCost(cfg, opening) + fillCost(cfg, fill2) + fillCost(cfg, fill3) + unit4Cost - 0.01
	wantAvailable := cash - fillCost(cfg, opening) - fillCost(cfg, fill2) - fillCost(cfg, fill3)

	emitted := newStream(t, cfg).
		snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), cash)).
		bars(breakoutBars("AAPL")).
		fill(opening).
		bar(addOpportunityBar("AAPL", day(57), rung4+1)).
		fill(fill2).
		fill(fill3).
		mustRun()

	proposed := envelopesOfType(emitted, event.AddProposalEventType)
	if len(proposed) != 2 {
		t.Fatalf("got %d add proposal(s), want 2 (units 2 and 3): unit 4 cannot be funded once three fills have spent the cash", len(proposed))
	}
	decline := onlyInsufficientCashDecline(t, emitted)
	if decline.Kind != event.ProposalDeclinedKindAdd {
		t.Errorf("Kind = %q, want %q", decline.Kind, event.ProposalDeclinedKindAdd)
	}
	if decline.RequiredCash != unit4Cost {
		t.Errorf("RequiredCash = %v, want exactly %v (the hold unit 4's rung would place)", decline.RequiredCash, unit4Cost)
	}
	if decline.AvailableCash != wantAvailable {
		t.Errorf("AvailableCash = %v, want exactly %v (the snapshot less three fills' actual costs and commissions)", decline.AvailableCash, wantAvailable)
	}
}

// TestASessionCloseAddIsCheckedAgainstCashLessAFillTheSnapshotPredates is the
// session-close path (ADR 0021 section 3) in the stream order a LEAN run
// delivers: the fills of the orders one Session placed arrive at the start
// of the next slice, and only then the snapshot of that Session's close,
// whose figures were read before those fills. Unit 3 fills during Session
// 59's trading; the snapshot as of 58 therefore cannot reflect it, although
// it arrives after it. Unit 4 is decided at Session 59's close against the
// snapshot less Unit 3's cost, and declined.
//
// A snapshot that replaced the debit outright on arrival would restore the
// pre-fill balance and propose Unit 4, which is how a real run proposed a
// 4th Unit costing 1.7 times the cash the account held.
func TestASessionCloseAddIsCheckedAgainstCashLessAFillTheSnapshotPredates(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	rung2, rung3, rung4 := addRungs(t, cfg)

	fill2 := addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", rung2, 133, day(57))
	// Unit 3's order rests after Session 58 closes and fills during the next
	// Session's trading, before that Session's bar.
	fill3 := addFill("AAPL", campaignID, 3, day(58), "sim-fill-add-3", rung3, 133, day(58).Add(14*time.Hour))

	unit4Cost := wantHold(cfg, 133, rung4, breakoutFixtureN(t, cfg))
	cashAt58 := fillCost(cfg, fill3) + unit4Cost - 0.01
	wantAvailable := cashAt58 - fillCost(cfg, fill3)

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(addOpportunityBar("AAPL", day(57), (rung2+rung3)/2)).
		fill(fill2).
		bar(addOpportunityBar("AAPL", day(58), (rung3+rung4)/2)).
		fill(fill3).
		snapshot(cashSnapshot(cfg, day(58), cashAt58)).
		bar(addOpportunityBar("AAPL", day(59), rung4+1)).
		mustRun()

	if proposed := envelopesOfType(emitted, event.AddProposalEventType); len(proposed) != 2 {
		t.Fatalf("got %d add proposal(s), want 2 (units 2 and 3)", len(proposed))
	}
	decline := onlyInsufficientCashDecline(t, emitted)
	if decline.RequiredCash != unit4Cost {
		t.Errorf("RequiredCash = %v, want exactly %v", decline.RequiredCash, unit4Cost)
	}
	if decline.AvailableCash != wantAvailable {
		t.Errorf("AvailableCash = %v, want exactly %v (the snapshot as of 58 less unit 3's fill, which it predates)", decline.AvailableCash, wantAvailable)
	}
	if !decline.PeriodEnd.Equal(day(59)) {
		t.Errorf("PeriodEnd = %v, want %v (decided at Session 59's close)", decline.PeriodEnd, day(59))
	}
}

// TestASnapshotStatedAfterAFillAlreadyReflectsIt is the other side of the
// replacement rule: a snapshot as of the fill's own time or later states a
// balance that already includes it, so the fill is not debited again. Unit 2
// is checked against that snapshot's figure exactly.
func TestASnapshotStatedAfterAFillAlreadyReflectsIt(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	rung2, rung3, _ := addRungs(t, cfg)
	unit2Cost := wantHold(cfg, 133, rung2, breakoutFixtureN(t, cfg))
	cashAt56 := unit2Cost - 0.01

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		snapshot(cashSnapshot(cfg, day(56), cashAt56)).
		bar(addOpportunityBar("AAPL", day(57), (rung2+rung3)/2)).
		mustRun()

	decline := onlyInsufficientCashDecline(t, emitted)
	if decline.AvailableCash != cashAt56 {
		t.Errorf("AvailableCash = %v, want exactly %v: the opening fill at day 56 is already in a snapshot as of day 56", decline.AvailableCash, cashAt56)
	}
}

// TestAnEntryIsCheckedAgainstCashLessAnotherInstrumentsFillAndNoExitCredit is
// the entry path (sizeUnit) across instruments. AAPL's entry fill spends
// cash; its stop fill the next day returns proceeds, which ADR 0020 credits
// only at the next previous close, through a snapshot, never from the fill.
// MSFT's breakout at that Session's close is checked against the snapshot
// less AAPL's entry cost exactly: neither credited with the stop's proceeds
// nor debited for the sale.
func TestAnEntryIsCheckedAgainstCashLessAnotherInstrumentsFillAndNoExitCredit(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)

	aapl := breakoutBars("AAPL")
	aapl = append(aapl, syntheticBar("AAPL", day(57), 50))
	msft := breakoutBars("MSFT")
	msft[len(msft)-1] = syntheticBar("MSFT", day(56), 50) // below its Entry Channel
	msft = append(msft, syntheticBar("MSFT", day(57), 100))

	opening := openingFill("AAPL")
	// Both entries alone cost about 20,600 at their Entry Channel level of
	// 155; AAPL's actual fill costs 26,766.25.
	const cash = 30_000.0
	wantAvailable := cash - fillCost(cfg, opening)

	emitted := newStream(t, cfg).
		snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), cash)).
		lockstep(aapl[:56], msft[:56]).
		fill(opening).
		fill(closingStopFill("AAPL", campaignID, campaignN, day(57))).
		lockstep(aapl[56:], msft[56:]).
		mustRun()

	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 || decodeTradeProposal(t, proposals[0]).InstrumentID != "AAPL" {
		t.Fatalf("got %d trade proposal(s), want exactly AAPL's", len(proposals))
	}
	decline := onlyInsufficientCashDecline(t, emitted)
	if decline.InstrumentID != "MSFT" || decline.Kind != event.ProposalDeclinedKindEntry {
		t.Fatalf("decline = %+v, want MSFT's entry", decline)
	}
	if decline.AvailableCash != wantAvailable {
		t.Errorf("AvailableCash = %v, want exactly %v: AAPL's entry is debited, its stop's proceeds are not credited", decline.AvailableCash, wantAvailable)
	}
}

// TestAWithdrawalFloorsTheBasisNotTheFillDebit pins ADR 0020's two-step
// invariant: basis = max(0, snapshot - withdrawals), available = basis -
// fills. A withdrawal that consumes the snapshot's cash floors the basis at
// zero, and the fill already recorded still leaves the available figure
// below it; the negative remainder is recorded, not floored.
func TestAWithdrawalFloorsTheBasisNotTheFillDebit(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	rung2, rung3, _ := addRungs(t, cfg)
	opening := openingFill("AAPL")
	const cash = 30_000.0
	equity := cfg.NotionalAccount.StartingEquity

	emitted := newStream(t, cfg).
		snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), cash)).
		bars(breakoutBars("AAPL")).
		fill(opening).
		movement(cashMovementPayload(day(56).Add(time.Hour), -40_000, equity)).
		bar(addOpportunityBar("AAPL", day(57), (rung2+rung3)/2)).
		mustRun()

	decline := onlyInsufficientCashDecline(t, emitted)
	if want := -fillCost(cfg, opening); decline.AvailableCash != want {
		t.Errorf("AvailableCash = %v, want exactly %v (a zero basis less the opening fill)", decline.AvailableCash, want)
	}
}

// TestAFillWhoseCostCannotBeStatedFailsClosed: a fill's actual cost that
// leaves the float64 range cannot be debited, and a balance ADR 0020 cannot
// state must not be read as any figure at all, so the run stops at the fill.
// Each order rests affordably at its level; its fill reports a price ten
// orders of magnitude above it. Both buy paths are covered, the entry fill
// and the Add fill.
func TestAFillWhoseCostCannotBeStatedFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := cashSkipOverflowConfiguration()
	cfg.NotionalAccount.StartingEquity = 2e297
	cfg.DollarsPerPoint = 1e280
	quantity, err := sizing.UnitQuantity(cfg.NotionalAccount.StartingEquity, cfg.UnitVolatilityFraction, overflowTrueRange, cfg.DollarsPerPoint)
	if err != nil {
		t.Fatalf("sizing.UnitQuantity() error = %v", err)
	}
	opening := event.FillPayload{
		InstrumentID: "AAPL",
		Kind:         event.FillKindEntry,
		ProposalID:   testDecisionID("proposal", "AAPL", day(56)),
		FillID:       "sim-fill-0001",
		Direction:    event.DirectionLong,
		Quantity:     quantity,
		Price:        1e21,
		FilledAt:     day(56),
	}

	t.Run("entry", func(t *testing.T) {
		t.Parallel()
		newStream(t, cfg).
			snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), math.MaxFloat64)).
			bars(cashSkipOverflowBars("AAPL")).
			fill(opening).
			wantRunError(`fill "sim-fill-0001"`, "leaves the representable range of spendable cash", "0020")
	})

	t.Run("add", func(t *testing.T) {
		t.Parallel()
		affordable := opening
		affordable.Price = overflowChannelHigh
		rung2, err := sizing.NextAddLevel(affordable.Price, overflowTrueRange, sizing.DirectionLong)
		if err != nil {
			t.Fatalf("NextAddLevel(rung 2) error = %v", err)
		}
		low := overflowChannelHigh - overflowTrueRange
		campaignID := testDecisionID("campaign", "AAPL", day(56))
		newStream(t, cfg).
			snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), math.MaxFloat64)).
			bars(cashSkipOverflowBars("AAPL")).
			fill(affordable).
			bar(completedBar("AAPL", day(57), rung2, low, low)).
			fill(addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", 1e21, quantity, day(57))).
			wantRunError(`fill "sim-fill-add-2"`, "leaves the representable range of spendable cash", "0020")
	})
}

// reflectedFillStream opens two Campaigns from one Session's breakouts, at a
// multiplier large enough that each fill costs about 1e308. A snapshot as of
// the Session's close then arrives; AAPL's fill after it is debited, and
// MSFT's fill at the close itself, which the snapshot already reflects, is
// not (ADR 0020). msftPrice sets MSFT's fill price.
func reflectedFillStream(t *testing.T, msftPrice float64) (s *stream, cfg event.ConfigurationPayload, aapl event.FillPayload) {
	t.Helper()
	cfg = validConfigurationPayload()
	cfg.NotionalAccount.StartingEquity = 1.7e308
	cfg.DollarsPerPoint = 1e304

	aapl = openingFill("AAPL")
	aapl.Quantity, aapl.Price, aapl.FilledAt = 2, 5000, day(56).Add(time.Hour)
	msft := openingFill("MSFT")
	msft.FillID = "sim-fill-msft"
	msft.Quantity, msft.Price, msft.FilledAt = 2, msftPrice, day(56)

	s = newStream(t, cfg).
		snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), math.MaxFloat64)).
		lockstep(breakoutBars("AAPL"), breakoutBars("MSFT")).
		snapshot(cashSnapshot(cfg, day(56), math.MaxFloat64)).
		fill(aapl).
		fill(msft)
	return s, cfg, aapl
}

// TestAFillTheSnapshotReflectsIsNeverDebitedWhateverIsOutstanding: a fill at
// or before the snapshot's as-of is already in the basis, so it is accepted
// and not debited even when adding its cost to the debits outstanding would
// leave the float64 range. The next Adds are then checked against the
// snapshot less AAPL's fill alone.
func TestAFillTheSnapshotReflectsIsNeverDebitedWhateverIsOutstanding(t *testing.T) {
	t.Parallel()

	s, cfg, aapl := reflectedFillStream(t, 5000)
	emitted := s.session(
		completedBar("AAPL", day(57), 5100, 4990, 4990),
		completedBar("MSFT", day(57), 5100, 4990, 4990),
	).mustRun()

	if opened := envelopesOfType(emitted, event.CampaignOpenedEventType); len(opened) != 2 {
		t.Fatalf("got %d campaign(s) opened, want 2: the reflected fill must be accepted", len(opened))
	}
	declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(declines) != 2 {
		t.Fatalf("got %d decline(s), want both Adds declined", len(declines))
	}
	want := math.MaxFloat64 - fillCost(cfg, aapl)
	for _, d := range declines {
		if got := decodeProposalDeclined(t, d).AvailableCash; got != want {
			t.Errorf("AvailableCash = %v, want %v: only AAPL's fill is outstanding", got, want)
		}
	}
}

// TestAReflectedFillWhoseOwnCostCannotBeStatedFailsClosed: a fill's own cost
// is validated whether or not it is debited, because a fact this system
// cannot state is not accepted on the strength of a snapshot's timestamp.
func TestAReflectedFillWhoseOwnCostCannotBeStatedFailsClosed(t *testing.T) {
	t.Parallel()

	s, _, _ := reflectedFillStream(t, 1e10)
	s.wantRunError(`fill "sim-fill-msft"`, "leaves the representable range", "0020")
}

// TestDebitsWhoseTotalCannotBeStatedFailClosed: two fills the snapshot does
// not reflect, each with a finite cost, whose debits together leave the
// float64 range. Spendable cash could no longer be stated, so the run stops
// at the second fill.
func TestDebitsWhoseTotalCannotBeStatedFailClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	cfg.NotionalAccount.StartingEquity = 1.7e308
	cfg.DollarsPerPoint = 1e304
	aapl := openingFill("AAPL")
	aapl.Quantity, aapl.Price, aapl.FilledAt = 2, 5000, day(56).Add(time.Hour)
	msft := openingFill("MSFT")
	msft.FillID = "sim-fill-msft"
	msft.Quantity, msft.Price, msft.FilledAt = 2, 5000, day(56).Add(2*time.Hour)

	newStream(t, cfg).
		snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), math.MaxFloat64)).
		lockstep(breakoutBars("AAPL"), breakoutBars("MSFT")).
		snapshot(cashSnapshot(cfg, day(56), math.MaxFloat64)).
		fill(aapl).
		fill(msft).
		wantRunError(`fill "sim-fill-msft"`, "leaves the representable range", "0020")
}
