package fills_test

import (
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
)

// This file holds ADR 0005's stop-limit, as amended 2026-09-24: every entry
// and Add of the Baseline rests as a buy stop at its level whose limit is
// its price cap, level + GapBufferN x N (CONTEXT.md: "Price cap").

// TestExecuteStopLimit is the fill rule, case by case, for a buy stop at 100
// capped at 102 with a slippage of 0.1.
func TestExecuteStopLimit(t *testing.T) {
	t.Parallel()

	const level, priceCap, slippage = 100.0, 102.0, 0.1
	tests := []struct {
		name        string
		r           fills.Range
		filled      bool
		price       float64
		atReference bool
	}{
		{"never reaches the level", fills.Range{Reference: 98, High: 99.9, Low: 97}, false, 0, false},
		{"trades through the level", fills.Range{Reference: 98, High: 101, Low: 97}, true, 100 + slippage, false},
		{"gaps above the level, within the cap", fills.Range{Reference: 101, High: 103, Low: 100.5}, true, 101 + slippage, true},
		{"opens exactly at the cap", fills.Range{Reference: 102, High: 103, Low: 101}, true, 102 + slippage, true},
		{"gaps above the cap and never trades back down to it", fills.Range{Reference: 103, High: 104, Low: 102.5}, false, 0, false},
		{"gaps above the cap and trades back down to it", fills.Range{Reference: 103, High: 104, Low: 102}, true, 102 + slippage, false},
		{"gaps above the cap and trades back through it", fills.Range{Reference: 103, High: 104, Low: 99}, true, 102 + slippage, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := fills.ExecuteStopLimit(level, priceCap, tt.r, slippage)
			if err != nil {
				t.Fatalf("ExecuteStopLimit() error = %v", err)
			}
			if got.Filled != tt.filled || (tt.filled && (got.Price != tt.price || got.AtReference != tt.atReference)) {
				t.Fatalf("ExecuteStopLimit() = %+v, want filled %v at %v (at reference %v)", got, tt.filled, tt.price, tt.atReference)
			}
		})
	}
}

// TestAStopLimitNeverFillsAboveItsCapPlusSlippage is the property ADR
// 0020's hold rests on, over every shape of bar: whatever the bar did, a
// capped buy's recorded price is at most the cap plus slippage, which is
// the per-share price its hold reserved.
func TestAStopLimitNeverFillsAboveItsCapPlusSlippage(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewSource(5))
	for range 20_000 {
		level := 10 + float64(rng.Float64()*200)
		priceCap := level + float64(rng.Float64()*5)
		slippage := 0.01 + float64(rng.Float64()*0.5)
		low := level - 10 + float64(rng.Float64()*20)
		high := low + float64(rng.Float64()*20)
		reference := low + float64(rng.Float64()*(high-low))
		got, err := fills.ExecuteStopLimit(level, priceCap, fills.Range{Reference: reference, High: high, Low: low}, slippage)
		if err != nil {
			t.Fatalf("ExecuteStopLimit() error = %v", err)
		}
		if got.Filled && got.Price > priceCap+slippage {
			t.Fatalf("a buy capped at %v filled at %v, above the cap plus slippage %v (bar %v/%v/%v)", priceCap, got.Price, priceCap+slippage, reference, high, low)
		}
	}
}

// TestExecuteStopLimitRefusesAMalformedCap: a cap below the level could
// never fill, and a cap that is not a finite positive price is not a price.
func TestExecuteStopLimitRefusesAMalformedCap(t *testing.T) {
	t.Parallel()

	r := fills.Range{Reference: 100, High: 101, Low: 99}
	for _, tt := range []struct {
		priceCap float64
		want     string
	}{
		{99, "below its own level"},
		{0, "price cap must be positive"},
		{math.NaN(), "price cap must be finite"},
	} {
		if _, err := fills.ExecuteStopLimit(100, tt.priceCap, r, 0.1); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("ExecuteStopLimit(cap %v) error = %v, want %q", tt.priceCap, err, tt.want)
		}
	}
	if _, err := fills.ExecuteStopLimit(100, 102, r, 0); err == nil {
		t.Error("ExecuteStopLimit() accepted zero slippage, which ADR 0013 declares invalid")
	}
}

// TestACommissionThatCannotBeStatedFailsClosed: a charge that leaves the
// float64 range is refused rather than recorded.
func TestACommissionThatCannotBeStatedFailsClosed(t *testing.T) {
	t.Parallel()

	model := fills.CommissionModel{PerShare: math.MaxFloat64, MinimumPerOrder: 0, MaximumFractionOfTradeValue: 1}
	if _, err := model.Charge(10, math.MaxFloat64, 10); err == nil || !strings.Contains(err.Error(), "not a finite figure") {
		t.Fatalf("Charge() error = %v, want refused", err)
	}
}

// The composed loop, under baselineConfig: the entry rests at the 155.5
// Entry Channel with N 1.5, so its price cap is 157 and its slippage 0.075.

// TestAGapAboveThePriceCapDoesNotFill: a breakout bar that opens above the
// cap and never trades back down to it fills nothing, and the proposal
// expires with the next bar (ADR 0011). The declared Variant "uncapped"
// fills the same bar at the open, as the stop-market entry always has.
func TestAGapAboveThePriceCapDoesNotFill(t *testing.T) {
	t.Parallel()

	gapped := bar(day(56), 158, 159, 157.5, 158.5)
	bars := append(warmUpBars(), gapped, bar(day(57), 158.5, 158.8, 158, 158.6))

	baseline := runComposed(t, baselineConfig(), bars)
	if got := fillPayloads(t, baseline.Inputs); len(got) != 0 {
		t.Fatalf("got %d fill(s), want none: the bar gapped above the 157 cap and never traded back to it%s", len(got), describe(envelopesOfType(baseline.Inputs, event.FillEventType)))
	}
	var expired event.ProposalExpiredPayload
	decodeInto(t, onlyOfType(t, baseline.Decisions, event.ProposalExpiredEventType), &expired)
	if expired.Kind != event.ProposalKindEntry {
		t.Errorf("expired Kind = %q, want the unfilled entry", expired.Kind)
	}

	uncapped := runComposed(t, uncappedConfig(), bars)
	got := fillPayloads(t, uncapped.Inputs)
	if len(got) == 0 || got[0].Kind != event.FillKindEntry {
		t.Fatalf("uncapped: want the entry filled at the open%s", describe(envelopesOfType(uncapped.Inputs, event.FillEventType)))
	}
	assertPrice(t, "uncapped entry price", got[0].Price, 158+fixtureSlippage)
}

// TestAGapWithinThePriceCapFillsAtTheOpen: a bar that gaps above the level
// but opens at or below the cap fills at the open, rule 1 unchanged.
func TestAGapWithinThePriceCapFillsAtTheOpen(t *testing.T) {
	t.Parallel()

	bars := append(warmUpBars(), bar(day(56), 156.5, 157.5, 156.2, 157))
	got := fillPayloads(t, runComposed(t, baselineConfig(), bars).Inputs)
	if len(got) == 0 || got[0].Kind != event.FillKindEntry {
		t.Fatal("want the entry filled")
	}
	assertPrice(t, "entry price", got[0].Price, 156.5+fixtureSlippage)
	assertPrice(t, "entry level", got[0].Level, 155.5)
}

// TestAGapAboveThePriceCapThatTradesBackFillsAtTheCap: a bar that opens
// above the cap and later trades down to it fills at the cap, never above
// it, plus slippage: exactly the price the entry's hold reserved.
func TestAGapAboveThePriceCapThatTradesBackFillsAtTheCap(t *testing.T) {
	t.Parallel()

	bars := append(warmUpBars(), bar(day(56), 158, 159, 156.5, 158.5))
	got := fillPayloads(t, runComposed(t, baselineConfig(), bars).Inputs)
	if len(got) == 0 || got[0].Kind != event.FillKindEntry {
		t.Fatal("want the entry filled")
	}
	assertPrice(t, "entry price", got[0].Price, 157+fixtureSlippage)
}

// TestAProposalOfAnUnknownOrderTypeIsRefused: the simulator rests only the
// two order types ADR 0005 declares, and refuses to guess at any other,
// for an entry and an Add alike.
func TestAProposalOfAnUnknownOrderTypeIsRefused(t *testing.T) {
	t.Parallel()

	entry := tradeProposal(t, "proposal:AAPL:day-56", 156, fixtureUnitQuantity, fixtureN)
	var trade event.TradeProposalPayload
	decodeInto(t, entry, &trade)
	trade.OrderType = "limit"
	add := addProposal(t, "add-proposal:AAPL:day-56", "campaign:AAPL:day-56", 157, fixtureUnitQuantity, fixtureN)
	var addPayload event.AddProposalPayload
	decodeInto(t, add, &addPayload)
	addPayload.OrderType = ""
	for name, e := range map[string]event.Envelope{
		"entry": envelope(t, entry.ID, entry.Type, entry.SchemaVersion, day(56), trade),
		"add":   envelope(t, add.ID, add.Type, add.SchemaVersion, day(56), addPayload),
	} {
		if err := newSimulator(t).Observe(e); err == nil || !strings.Contains(err.Error(), "cannot fill") {
			t.Errorf("%s: Observe() error = %v, want the unknown order type refused", name, err)
		}
	}
}

// TestAStopLimitProposalWithoutAUsableCapIsRefused: a stop-limit's cap is
// the most it may pay (ADR 0005, as amended 2026-09-24), so a proposal whose
// cap is zero, negative or below its own level is refused rather than
// rested as an order that could fill above the cap its hold reserved. (A
// non-finite cap cannot be written as JSON, so no envelope can carry one.)
func TestAStopLimitProposalWithoutAUsableCapIsRefused(t *testing.T) {
	t.Parallel()

	for _, priceCap := range []float64{0, -1, 155.9} {
		entry := cappedTradeProposal(t, "proposal:AAPL:day-56", 156, priceCap, fixtureUnitQuantity, fixtureN)
		if err := newSimulator(t).Observe(entry); err == nil || !strings.Contains(err.Error(), "price cap") {
			t.Errorf("entry capped at %v: Observe() error = %v, want the cap refused", priceCap, err)
		}
		add := addProposal(t, "add-proposal:AAPL:day-56", "campaign:AAPL:day-56", 156, fixtureUnitQuantity, fixtureN)
		var payload event.AddProposalPayload
		decodeInto(t, add, &payload)
		payload.OrderType, payload.PriceCap = event.OrderTypeStopLimit, priceCap
		add = envelope(t, add.ID, add.Type, add.SchemaVersion, day(56), payload)
		if err := newSimulator(t).Observe(add); err == nil || !strings.Contains(err.Error(), "price cap") {
			t.Errorf("add capped at %v: Observe() error = %v, want the cap refused", priceCap, err)
		}
	}
}

// TestAStopMarketProposalStatingACapIsRefused: a stop-market order has no
// cap (ADR 0005, as amended 2026-09-24), so a proposal stating one is
// contradictory, and resting it uncapped would ignore the cap it states.
func TestAStopMarketProposalStatingACapIsRefused(t *testing.T) {
	t.Parallel()

	entry := tradeProposal(t, "proposal:AAPL:day-56", 156, fixtureUnitQuantity, fixtureN)
	var trade event.TradeProposalPayload
	decodeInto(t, entry, &trade)
	trade.PriceCap = 157.5
	add := addProposal(t, "add-proposal:AAPL:day-56", "campaign:AAPL:day-56", 156, fixtureUnitQuantity, fixtureN)
	var addPayload event.AddProposalPayload
	decodeInto(t, add, &addPayload)
	addPayload.PriceCap = 157.5
	for name, e := range map[string]event.Envelope{
		"entry": envelope(t, entry.ID, entry.Type, entry.SchemaVersion, day(56), trade),
		"add":   envelope(t, add.ID, add.Type, add.SchemaVersion, day(56), addPayload),
	} {
		if err := newSimulator(t).Observe(e); err == nil || !strings.Contains(err.Error(), "stop-market") {
			t.Errorf("%s: Observe() error = %v, want the stated cap refused", name, err)
		}
	}
}
