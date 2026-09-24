package fills_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// This file is the event seam of the equity-native Golden Scenario: the real
// reducer and the real fill simulator, driven bar by bar, over a synthetic
// instrument built so that every number below can be worked out on paper.
// Its arithmetic-seam counterpart is internal/sizing/equity_golden_test.go,
// which pins the same constants without any event machinery.
//
// Faith's worked examples are futures — Heating Oil carries a 42,000
// multiplier (The Turtle Rules p.15) — so they prove the arithmetic and
// leave the equity path untested. This fixture exercises what is specific to
// a share: a contract multiplier of ONE, whole-share truncation, ADR 0010's
// cash-skip rule on an Add Ladder rung, and ADR 0009's Delisting Exit.
//
// # The configuration
//
//	Sizing Mode                 volatility-normalised   (ADR 0003, the Baseline)
//	Unit Volatility Fraction    0.005                   (ADR 0003: 0.5 % per N)
//	Stop Multiple               2                       (ADR 0003; T p.22)
//	Entry / Exit Channel        55 / 20                 (ADR 0002: System 2)
//	Max Units                   4                       (ADR 0008)
//	Slippage                    0.05 N                  (ADR 0013)
//	Dollars per point           1                       (a share, not a contract)
//	Notional Account            8,400                   (ADR 0007, configured)
//	Available cash              5,016.99                (ADR 0010, observed)
//
// # The bars, and why they are these bars
//
// Warm-up bars 1..55: bar i opens and closes at 100+i, highs at 101+i and
// lows at 98.5+i. Every True Range is therefore max(2.5, 2, 0.5) = 2.5 (the
// first bar, with no previous close, is high - low = 2.5), so the 20-bar
// simple average that seeds N is 2.5 and Wilder's step holds it there —
// (19 x 2.5 + 2.5)/20 = 2.5, exact on paper and exact in float64 (T p.13).
// Every price is a multiple of 0.5 and so is exactly representable, which is
// what lets the derivation below be checked rather than approximated.
//
// At the end of warm-up the Entry Channel (55 bars) stands at bar 55's high
// of 156 and the Exit Channel (20 bars) at bar 36's low of 134.5.
//
//	bar 56  O 155.0  H 157.0  L 154.5  C 156.5   the breakout
//	bar 57  O 156.5  H 158.0  L 155.5  C 157.5   reaches rung 2; cash short
//	bar 58  O 157.5  H 158.5  L 156.0  C 158.0   reaches rung 2 again; still short
//	        delisting effective at bar 58's period end
//
// # The derivation, step by step
//
// **Bar 56 — the Signal and the Unit.** The bar's high of 157 strictly
// exceeds the 156 Entry Channel (T p.19; a tie is not a Breakout), so the
// Setup is Tier A and a Signal fires. Sizing is decided from the bars BEFORE
// it (CONTEXT.md: "Completed bar"), so N is 2.5:
//
//	budget   = 8,400 x 0.005                 = 42
//	quantity = floor(42 / (2.5 x 1)) = floor(16.8) = 16 shares
//
// **Bar 56 — the fill.** The order rests at the Entry Channel high of 156,
// not at the breakout bar's own high (ADR 0005: a resting buy-stop sits at
// the level). The bar opened at 155, below the level, so it is not a gap:
// the fill is at the level plus ADR 0013's 0.05 N of slippage against the
// trader.
//
//	fill      = 156 + 0.05 x 2.5   = 156.125
//	stop      = 156.125 - 2 x 2.5  = 151.125   (T p.22)
//	rung 2    = 156.125 + 0.5 x 2.5 = 157.375  (T p.19, from the ACTUAL fill)
//	commission = max(16 x 0.005, 1.00) = 1.00, under the 1 % cap of 24.98
//
// Bar 56's own high of 157 falls short of rung 2, so no Unit is added inside
// the breakout bar.
//
// **Bars 57 and 58 — the skipped rung.** Both bars reach rung 2 (158 and
// 158.5 are each above 157.375), so on each the cash check runs against the
// snapshot less the entry fill's actual cost (ADR 0020):
//
//	rung 2 cost = 16 x 157.375 x 1           = 2,518.00
//	cash        = 5,016.99 - (2,498.00 + 1.00) = 2,517.99
//
// One cent short, so the whole Unit is skipped — no partial Unit, no
// borrowing, no deferred queue (ADR 0010) — and the rejection is journalled
// with both figures. It is skipped on BOTH bars: a skip does not poison the
// ladder, and the rung is re-offered on its own merits each bar.
//
// **The Delisting Exit.** A delisting effective at bar 58's period end forces
// the Campaign closed at the last available price, which is bar 58's close of
// 158 (ADR 0009; the payload carries no price of its own).
//
//	realised result      = 16 x (158 - 156.125) x 1 = 30.00
//	average move in N    = 1.875 / 2.5              = 0.75
//	result in Unit N     = 30 / (16 x 2.5 x 1)      = 0.75
//
// # Why each constant here can fail
//
//   - **16 shares.** 42 / 2.5 is 16.8. Rounding buys 17, which costs 42.50
//     against a 42 budget and realises 1.0119 % at the stop against a declared
//     1 % — so this constant is only satisfied by truncation (T p.14-15), and
//     a quotient that rounded the same either way would test nothing.
//   - **A multiplier of one.** At Faith's 42,000 the same account sizes 0.0004
//     of a contract and the Signal produces a decline, not a position.
//   - **2,517.99 of cash after the entry.** It sits in the window (2,498,
//     2,518]. Costing the Add from the entry level (16 x 156 = 2,496) or from
//     the previous fill (16 x 156.125 = 2,498) rather than from the rung would
//     make the Unit affordable and no skip would happen; so would reading the
//     8,400 Notional Account as the cash basis instead of the snapshot's
//     observed figure, or the snapshot's 5,016.99 without the entry fill's
//     debit (ADR 0020). A cash figure comfortably clear of the rung
//     distinguishes none of those.
//   - **156, not 157.** Entering at the breakout bar's own high instead of the
//     Entry Channel high would fill at 157.125, put rung 2 at 158.375, and
//     bar 57's high of 158 would no longer reach it.
//   - **158, not 157.5 or 158.5.** The Delisting Exit's price is the last
//     completed bar's CLOSE; the previous close or the bar's own high would
//     each give a different realised result.

const (
	equityGoldenInstrument = "SYNTH"

	// equityGoldenN is N throughout the decision that matters, exact by the
	// warm-up's construction (see above).
	equityGoldenN = 2.5
	// equityGoldenNotionalAccount is ADR 0007's configured figure.
	equityGoldenNotionalAccount = 8_400.0
	// equityGoldenOpeningCash is ADR 0010's cash basis: an OBSERVED figure
	// from an account.snapshot, deliberately different from the Notional
	// Account above so that a fixture reading one for the other fails.
	equityGoldenOpeningCash = 5_016.99
	// equityGoldenAvailableCash is what the Add is checked against: the
	// opening cash less the entry fill's actual cost, 16 x 156.125 plus the
	// 1.00 commission (ADR 0020).
	equityGoldenAvailableCash = 2_517.99

	// equityGoldenEntryChannelHigh is bar 55's high, the level a resting
	// buy-stop sits at (ADR 0005).
	equityGoldenEntryChannelHigh = 156.0
	// equityGoldenBreakoutHigh is bar 56's own high, which the Signal
	// reports and the entry deliberately does NOT rest at.
	equityGoldenBreakoutHigh = 157.0
	// equityGoldenUnitQuantity is floor(8,400 x 0.005 / (2.5 x 1)) =
	// floor(16.8) (T p.14-15: Faith truncates).
	equityGoldenUnitQuantity int64 = 16
	// equityGoldenEntryFill is 156 + 0.05 x 2.5 (ADR 0013).
	equityGoldenEntryFill = 156.125
	// equityGoldenSlippage is 0.05 N.
	equityGoldenSlippage = 0.125
	// equityGoldenCommission is max(16 x 0.005, 1.00), under the 1 % of
	// trade value ceiling of 24.98 (ADR 0013).
	equityGoldenCommission = 1.00
	// equityGoldenStopIntent is the stop the PROPOSAL states, measured from
	// the resting level: 156 - 2 x 2.5.
	equityGoldenStopIntent = 151.0
	// equityGoldenUnitOneStop is the stop the CAMPAIGN carries, measured
	// from the actual fill: 156.125 - 2 x 2.5 (T p.22).
	equityGoldenUnitOneStop = 151.125
	// equityGoldenRungTwo is 156.125 + 0.5 x 2.5 (T p.19).
	equityGoldenRungTwo = 157.375
	// equityGoldenRungTwoCost is 16 x 157.375 x 1 — one cent above the cash.
	equityGoldenRungTwoCost = 2_518.0

	// equityGoldenLastPrice is bar 58's close: the last available price a
	// Delisting Exit closes at (ADR 0009).
	equityGoldenLastPrice = 158.0
	// equityGoldenRealisedResult is 16 x (158 - 156.125) x 1.
	equityGoldenRealisedResult = 30.0
	// equityGoldenAverageMoveInN is (158 - 156.125) / 2.5.
	equityGoldenAverageMoveInN = 0.75
	// equityGoldenResultInUnitN is 30 / (16 x 2.5 x 1).
	equityGoldenResultInUnitN = 0.75
)

// equityGoldenConfig is the Baseline at a contract multiplier of one, over a
// Notional Account small enough that whole-share truncation is material
// rather than a rounding artefact (T p.15 names small accounts as where
// truncation bites).
func equityGoldenConfig() event.ConfigurationPayload {
	return event.ConfigurationPayload{
		StrategyID:             "turtle-baseline",
		SizingMode:             event.SizingModeVolatilityNormalised,
		UnitVolatilityFraction: 0.005,
		StopMultiple:           2.0,
		EntryChannelLength:     55,
		ExitChannelLength:      20,
		MaxUnits:               4,
		SlippageN:              0.05,
		TierBDistanceInN:       1.0,
		DollarsPerPoint:        1,
		RiskAtStopFraction:     0,
		NotionalAccount: event.NotionalAccountConfig{
			StartingEquity: equityGoldenNotionalAccount,
			RebasingMonth:  1,
			RebasingDay:    1,
		},
		Commission: event.CommissionConfig{
			PerShare:                    0.005,
			MinimumPerOrder:             1.00,
			MaximumFractionOfTradeValue: 0.01,
		},
	}
}

// equityGoldenBar builds one bar of the fixture with identical split-adjusted
// and raw views. ADR 0004 keeps the two apart — signals on split-adjusted
// prices, accounting on raw ones — and the Baseline's own raw-view accounting
// is not slice 1's; making them equal here keeps this fixture about the rules
// it does exercise.
func equityGoldenBar(periodEnd time.Time, open, high, low, closeAt float64) event.CompletedBarPayload {
	view := func(label string) event.PriceView {
		return event.PriceView{View: label, Open: open, High: high, Low: low, Close: closeAt, Volume: 1_000_000}
	}
	return event.CompletedBarPayload{
		InstrumentID:  equityGoldenInstrument,
		PeriodEnd:     periodEnd,
		SplitAdjusted: view(event.ViewSplitAdjusted),
		Raw:           view(event.ViewRaw),
	}
}

// equityGoldenBars is the whole fixture: 55 warm-up bars at a True Range of
// exactly 2.5, the breakout, and the two bars that reach the second Add
// Ladder rung the account cannot afford. See this file's own derivation.
func equityGoldenBars() []event.CompletedBarPayload {
	bars := make([]event.CompletedBarPayload, 0, 58)
	for i := 1; i <= 55; i++ {
		base := 100 + float64(i)
		bars = append(bars, equityGoldenBar(day(i), base, base+1, base-1.5, base))
	}
	return append(bars,
		equityGoldenBar(day(56), 155.0, 157.0, 154.5, 156.5),
		equityGoldenBar(day(57), 156.5, 158.0, 155.5, 157.5),
		equityGoldenBar(day(58), 157.5, 158.5, 156.0, 158.0),
	)
}

// equityGoldenDelisting is the corporate action that ends the instrument's
// life in this run, effective at the last bar's own period end — the earliest
// moment a delisting may take effect without predating the bar whose close is
// the last available price (ADR 0009).
func equityGoldenDelisting() event.CorporateActionPayload {
	return event.CorporateActionPayload{
		InstrumentID: equityGoldenInstrument,
		Kind:         event.CorporateActionKindDelisting,
		EffectiveAt:  day(58),
	}
}

// equityGoldenRun drives the whole scenario through the real simulator and
// the real reducer behind a journal.Recorder, in the order the backtest loop
// delivers inputs: configuration, the cash basis, every bar, then the
// delisting. The recorder is the loop's own way of collecting decisions, and
// the only one that stamps them as the journal holds them (see composed).
func equityGoldenRun(t *testing.T) composed {
	t.Helper()

	cfg := equityGoldenConfig()
	hash := event.ConfigurationHash(cfg)
	simulator, err := fills.New(cfg, testStrategyVersion, hash)
	if err != nil {
		t.Fatalf("fills.New() error = %v", err)
	}
	reducer, err := strategy.NewReducer(testStrategyVersion, cfg)
	if err != nil {
		t.Fatalf("strategy.NewReducer() error = %v", err)
	}
	recorder := journal.NewRecorder(reducer)

	ctx := context.Background()
	deliver := func(what string, e event.Envelope) {
		t.Helper()
		if _, err := fills.Deliver(ctx, simulator, recorder, e); err != nil {
			t.Fatalf("Deliver(%s) error = %v", what, err)
		}
	}

	deliver("configuration", equityGoldenEnvelope(t, hash, "cfg-1", event.ConfigurationEventType, event.ConfigurationSchemaVersion, day(0), cfg))
	deliver("account snapshot", equityGoldenEnvelope(t, hash, "snapshot-1", event.AccountSnapshotEventType, event.AccountSnapshotSchemaVersion, day(0),
		event.AccountSnapshotPayload{
			AsOf:          day(0),
			Equity:        equityGoldenNotionalAccount,
			AvailableCash: equityGoldenOpeningCash,
			Currency:      "USD",
		}))

	for i, b := range equityGoldenBars() {
		if _, err := fills.RunBar(ctx, simulator, recorder, equityGoldenEnvelope(t, hash,
			"bar:"+b.PeriodEnd.Format(time.RFC3339), event.CompletedBarEventType, event.CompletedBarSchemaVersion, b.PeriodEnd, b)); err != nil {
			t.Fatalf("RunBar(bar %d, period end %s) error = %v", i+1, b.PeriodEnd.Format(time.RFC3339), err)
		}
	}

	deliver("delisting", equityGoldenEnvelope(t, hash, "delisting-1", event.MarketCorporateActionEventType, event.MarketCorporateActionSchemaVersion, day(58), equityGoldenDelisting()))
	return recorded(t, recorder)
}

// equityGoldenEnvelope wraps one payload as an input envelope stamped with
// this fixture's own configuration hash — never the package's baseline hash,
// which belongs to a different configuration and which the reducer would
// refuse.
func equityGoldenEnvelope(t *testing.T, hash, id, eventType string, schemaVersion uint32, at time.Time, payload any) event.Envelope {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return event.Envelope{
		ID:                id,
		Type:              eventType,
		SchemaVersion:     schemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         at,
		RecordedAt:        at,
		Source:            "fixture",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: hash,
		PayloadHash:       event.HashPayload(encoded),
		Payload:           encoded,
	}
}

// TestEquityGoldenScenarioSizesAWholeShareUnitAtAMultiplierOfOne is the
// entry: a Signal on bar 56 and, behind it, the Unit the Turtle rule sizes
// for an instrument whose multiplier is one.
func TestEquityGoldenScenarioSizesAWholeShareUnitAtAMultiplierOfOne(t *testing.T) {
	t.Parallel()

	run := equityGoldenRun(t)

	signal := onlyOfType(t, run.Decisions, event.SignalEventType)
	var signalled event.SignalPayload
	decodeInto(t, signal, &signalled)
	if signalled.PeriodEnd != day(56) {
		t.Errorf("Signal PeriodEnd = %s, want bar 56 (%s)", signalled.PeriodEnd, day(56))
	}
	assertPrice(t, "Signal EntryChannelHigh", signalled.EntryChannelHigh, equityGoldenEntryChannelHigh)
	assertPrice(t, "Signal BreakoutHigh", signalled.BreakoutHigh, equityGoldenBreakoutHigh)
	assertPrice(t, "Signal N", signalled.N, equityGoldenN)

	proposal := onlyOfType(t, run.Decisions, event.TradeProposalEventType)
	var proposed event.TradeProposalPayload
	decodeInto(t, proposal, &proposed)

	if proposed.Quantity != equityGoldenUnitQuantity {
		t.Errorf("Quantity = %d, want %d: floor(8,400 x 0.005 / (2.5 x 1)) = floor(16.8), truncated not rounded (T p.14-15)",
			proposed.Quantity, equityGoldenUnitQuantity)
	}
	if proposed.DollarsPerPoint != 1 {
		t.Errorf("DollarsPerPoint = %v, want 1: one point of price is one dollar a share", proposed.DollarsPerPoint)
	}
	assertPrice(t, "EntryLevel", proposed.EntryLevel, equityGoldenEntryChannelHigh)
	assertPrice(t, "N", proposed.N, equityGoldenN)
	assertPrice(t, "NotionalAccount", proposed.NotionalAccount, equityGoldenNotionalAccount)
	assertPrice(t, "ProtectiveStopIntent", proposed.ProtectiveStopIntent, equityGoldenStopIntent)
	// ADR 0003: Risk at Stop is derived, never configured — 0.005 x 2.
	assertPrice(t, "RiskAtStop", proposed.RiskAtStop, 0.005*2)
	// 16 x 2 x 2.5 x 1 / 8,400, stated as the hand-derived rational.
	assertPrice(t, "RealisedRiskAtStop", proposed.RealisedRiskAtStop, 80.0/8_400.0)
	if proposed.RealisedRiskAtStop >= proposed.RiskAtStop {
		t.Errorf("RealisedRiskAtStop %v is not below the declared %v: the gap IS the truncation of 16.8 to 16",
			proposed.RealisedRiskAtStop, proposed.RiskAtStop)
	}

	if declines := envelopesOfType(run.Decisions, event.ProposalDeclinedEventType); len(declines) != 2 {
		// Both declines belong to the Add Ladder, asserted in their own
		// test; an entry-side decline here would mean the Unit was never
		// bought at all.
		t.Fatalf("got %d decline(s), want exactly 2 (both Add-side)", len(declines))
	}
}

// TestEquityGoldenScenarioFillsTheRestingOrderAndFreezesTheCampaign is ADR
// 0005 and ADR 0006 on the equity path: the order rests at the Entry Channel
// high, slippage is charged against the trader, and the Campaign freezes the
// N and Unit size it opened with.
func TestEquityGoldenScenarioFillsTheRestingOrderAndFreezesTheCampaign(t *testing.T) {
	t.Parallel()

	run := equityGoldenRun(t)

	executed := fillPayloads(t, run.Inputs)
	if len(executed) != 1 {
		t.Fatalf("got %d fill(s), want exactly 1: the entry filled and no Add ever could", len(executed))
	}
	fill := executed[0]
	if fill.Kind != event.FillKindEntry {
		t.Errorf("fill Kind = %q, want %q", fill.Kind, event.FillKindEntry)
	}
	if fill.Quantity != equityGoldenUnitQuantity {
		t.Errorf("fill Quantity = %d, want %d", fill.Quantity, equityGoldenUnitQuantity)
	}
	assertPrice(t, "fill Level", fill.Level, equityGoldenEntryChannelHigh)
	assertPrice(t, "fill Price", fill.Price, equityGoldenEntryFill)
	assertPrice(t, "fill SlippageApplied", fill.SlippageApplied, equityGoldenSlippage)
	assertPrice(t, "fill Commission", fill.Commission, equityGoldenCommission)

	opened := onlyOfType(t, run.Decisions, event.CampaignOpenedEventType)
	var campaign event.CampaignOpenedPayload
	decodeInto(t, opened, &campaign)
	if campaign.UnitQuantity != equityGoldenUnitQuantity {
		t.Errorf("CampaignOpened UnitQuantity = %d, want %d", campaign.UnitQuantity, equityGoldenUnitQuantity)
	}
	if campaign.Units != 1 {
		t.Errorf("CampaignOpened Units = %d, want 1", campaign.Units)
	}
	assertPrice(t, "CampaignOpened CampaignN", campaign.CampaignN, equityGoldenN)
	assertPrice(t, "CampaignOpened EntryPrice", campaign.EntryPrice, equityGoldenEntryFill)
	assertPrice(t, "CampaignOpened ProtectiveStop", campaign.ProtectiveStop, equityGoldenUnitOneStop)
}

// TestEquityGoldenScenarioSkipsTheRungItCannotAfford is ADR 0010's cash-skip
// rule: the Unit costs 2,518.00 at its rung and the account holds 2,517.99
// once the entry fill is debited (ADR 0020), so the whole Unit is skipped and the rejection carries both figures. It is
// skipped on each of the two bars that reach the rung — a skip does not
// poison the ladder.
func TestEquityGoldenScenarioSkipsTheRungItCannotAfford(t *testing.T) {
	t.Parallel()

	run := equityGoldenRun(t)

	if proposals := envelopesOfType(run.Decisions, event.AddProposalEventType); len(proposals) != 0 {
		t.Fatalf("got %d add proposal(s), want 0: an unaffordable Unit is never proposed, partial or otherwise", len(proposals))
	}

	declines := envelopesOfType(run.Decisions, event.ProposalDeclinedEventType)
	if len(declines) != 2 {
		t.Fatalf("got %d decline(s), want exactly 2: bars 57 and 58 each reach rung 2 and each skip it", len(declines))
	}
	// Bar 56's own high of 157 falls short of the 157.375 rung, so the first
	// skip belongs to bar 57 and the second to bar 58 — the rung is offered
	// again, on its own merits, rather than being poisoned by the first
	// refusal.
	wantBars := []time.Time{day(57), day(58)}
	for i, e := range declines {
		var declined event.ProposalDeclinedPayload
		decodeInto(t, e, &declined)
		if declined.Kind != event.ProposalDeclinedKindAdd {
			t.Errorf("decline %d Kind = %q, want %q", i, declined.Kind, event.ProposalDeclinedKindAdd)
		}
		if declined.Reason != event.DeclineReasonInsufficientCash {
			t.Errorf("decline %d Reason = %q, want %q", i, declined.Reason, event.DeclineReasonInsufficientCash)
		}
		if !declined.PeriodEnd.Equal(wantBars[i]) {
			t.Errorf("decline %d PeriodEnd = %s, want %s", i, declined.PeriodEnd, wantBars[i])
		}
		assertPrice(t, "decline RequiredCash", declined.RequiredCash, equityGoldenRungTwoCost)
		assertPrice(t, "decline AvailableCash", declined.AvailableCash, equityGoldenAvailableCash)
		if declined.RequiredCash <= declined.AvailableCash {
			t.Errorf("decline %d claims insufficient cash but %v does not exceed %v", i, declined.RequiredCash, declined.AvailableCash)
		}
	}

	// The window this cash figure sits in is what makes the skip
	// discriminating rather than decorative: costed from the entry level or
	// from the previous Unit's fill instead of from the rung, the same Unit
	// is affordable and no skip happens at all. Asserted rather than merely
	// claimed in a comment, so the fixture stops being load-bearing loudly.
	for _, wrong := range []struct {
		basis float64
		name  string
	}{
		{equityGoldenEntryChannelHigh, "the entry level"},
		{equityGoldenEntryFill, "the previous unit's fill"},
	} {
		if cost := float64(equityGoldenUnitQuantity) * wrong.basis; cost > equityGoldenAvailableCash {
			t.Errorf("costing the Add from %s gives %v, already above the %v available: this fixture's cash no longer distinguishes that basis from the rung",
				wrong.name, cost, equityGoldenAvailableCash)
		}
	}
	if cost := equityGoldenRungTwoCost; cost > equityGoldenOpeningCash {
		t.Errorf("the rung's cost %v is above the opening cash %v: this fixture's cash no longer distinguishes the entry fill's debit from none", cost, equityGoldenOpeningCash)
	}
	if cost := float64(equityGoldenUnitQuantity) * equityGoldenRungTwo; !closeTo(cost, equityGoldenRungTwoCost) {
		t.Fatalf("the fixture's own arithmetic is wrong: 16 x %v = %v, not %v", equityGoldenRungTwo, cost, equityGoldenRungTwoCost)
	}
}

// TestEquityGoldenScenarioClosesTheCampaignOnDelisting is ADR 0009: the
// instrument stopped trading, so the Campaign is forced closed at the last
// available price — bar 58's close — with no proposal and no fill in
// between.
func TestEquityGoldenScenarioClosesTheCampaignOnDelisting(t *testing.T) {
	t.Parallel()

	run := equityGoldenRun(t)

	exited := onlyOfType(t, run.Decisions, event.CampaignExitedEventType)
	var payload event.CampaignExitedPayload
	decodeInto(t, exited, &payload)

	if payload.Reason != event.ExitReasonDelisting {
		t.Errorf("Reason = %q, want %q", payload.Reason, event.ExitReasonDelisting)
	}
	if payload.ADR != event.ADRDelistingForcesExit {
		t.Errorf("ADR = %q, want %q", payload.ADR, event.ADRDelistingForcesExit)
	}
	if !payload.ExitedAt.Equal(day(58)) {
		t.Errorf("ExitedAt = %s, want %s", payload.ExitedAt, day(58))
	}
	if payload.Quantity != equityGoldenUnitQuantity {
		t.Errorf("Quantity = %d, want %d: the one Unit the cash allowed", payload.Quantity, equityGoldenUnitQuantity)
	}
	if payload.Units != 1 {
		t.Errorf("Units = %d, want 1", payload.Units)
	}
	assertPrice(t, "EntryPrice", payload.EntryPrice, equityGoldenEntryFill)
	assertPrice(t, "ExitPrice", payload.ExitPrice, equityGoldenLastPrice)
	assertPrice(t, "CampaignN", payload.CampaignN, equityGoldenN)
	assertPrice(t, "ProtectiveStopLevel", payload.ProtectiveStopLevel, equityGoldenUnitOneStop)
	assertPrice(t, "RealisedResult", payload.RealisedResult, equityGoldenRealisedResult)
	assertPrice(t, "AverageMoveInN", payload.AverageMoveInN, equityGoldenAverageMoveInN)
	assertPrice(t, "RealisedResultInUnitN", payload.RealisedResultInUnitN, equityGoldenResultInUnitN)
	if payload.DollarsPerPoint != 1 {
		t.Errorf("DollarsPerPoint = %v, want 1", payload.DollarsPerPoint)
	}

	// A delisting is a direct close, never a proposal that awaits its own
	// fill, and nothing executed for it.
	if exits := envelopesOfType(run.Decisions, event.ExitProposalEventType); len(exits) != 0 {
		t.Errorf("got %d exit proposal(s), want 0: a Campaign is forced closed by a delisting, never proposed first", len(exits))
	}
	if executed := fillPayloads(t, run.Inputs); len(executed) != 1 {
		t.Errorf("got %d fill(s), want exactly 1: a delisting has no execution to reconcile against", len(executed))
	}
}

// TestEquityGoldenScenarioReplaysByteIdentically: the input stream this run
// produced is a journal, and replaying it through a fresh reducer must
// reproduce every decision byte for byte (ADR 0017).
func TestEquityGoldenScenarioReplaysByteIdentically(t *testing.T) {
	t.Parallel()

	run := equityGoldenRun(t)

	first := equityGoldenReplay(t, run.Inputs)
	second := equityGoldenReplay(t, run.Inputs)
	if divergence := replay.Equivalent(first, second); divergence != nil {
		t.Fatalf("two replays of the same input stream diverged at decision %d", divergence.Index)
	}

	report, err := replay.Diff(run.Decisions, first)
	if err != nil {
		t.Fatalf("replay.Diff() error = %v", err)
	}
	if report != nil {
		t.Fatalf("the run's journalled decisions and a replay of its own inputs differ: %s", report)
	}
}

// equityGoldenReplay runs inputs through a fresh reducer configured from this
// fixture's own configuration — never the package baseline's, whose hash the
// reducer would refuse.
func equityGoldenReplay(t *testing.T, inputs []event.Envelope) []event.Envelope {
	t.Helper()
	reducer, err := strategy.NewReducer(testStrategyVersion, equityGoldenConfig())
	if err != nil {
		t.Fatalf("strategy.NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}
	emitted, err := engine.Run(context.Background(), inputs)
	if err != nil {
		t.Fatalf("Engine.Run() error = %v", err)
	}
	return emitted
}
