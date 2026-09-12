package fills_test

import (
	"math"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/fills"
)

// This file is #18's arithmetic seam: ADR 0005's three rules and ADR 0013's
// cost model, as pure functions, with no events, no reducer and no bar
// stream. The event seam — the same rules driving the real reducer through
// replay.Engine.Run — is runbar_test.go.
//
// # The one-sided coverage test, and why "low <= level <= high" is not it
//
// ADR 0005 rule 1 says an order fills in the first bar whose range covers its
// level, and in the same breath that a gap through the level fills at the
// open. Those two sentences are only consistent if the coverage test is read
// ONE-SIDED, per side:
//
//	buy  fills iff high >= level      sell fills iff low <= level
//
// A two-sided "low <= level <= high" would make a gap-up bar — one that
// opened above a buy-stop and never traded back down to it — fail the test
// and never fill at all, which contradicts the ADR's own gap rule and Faith's
// "entered on the open if the market gapped through" [T p.18]. The one-sided
// form is the union of the two cases the ADR names: the level inside the
// range, and the bar already beyond it. TestGapThroughAlwaysFills below is
// the fixture that fails if anyone ever "tightens" this to the two-sided
// form.

const testSlippage = 0.075 // 0.05 N with N = 1.5, the event-seam fixture's N

func TestExecute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		side string
		// level is where the order rests; the Range is what the bar did.
		level     float64
		reference float64
		high      float64
		low       float64
		slippage  float64
		want      fills.Execution
	}{
		// --- Buys: the level inside the bar's range.
		{
			name: "buy fills at its level plus slippage when the level is inside the range",
			side: fills.SideBuy, level: 100, reference: 98, high: 105, low: 97, slippage: 0.5,
			want: fills.Execution{Filled: true, Price: 100.5},
		},
		{
			// ADR 0005 rule 1 is boundary-INCLUSIVE: Faith's "traded at the
			// level" [T p.18]. A bar that reached the level exactly, and no
			// further, filled the order.
			name: "buy fills on an exact touch at the high",
			side: fills.SideBuy, level: 105, reference: 98, high: 105, low: 97, slippage: 0.5,
			want: fills.Execution{Filled: true, Price: 105.5},
		},
		{
			name: "buy does not fill when the bar never reached the level",
			side: fills.SideBuy, level: 105.01, reference: 98, high: 105, low: 97, slippage: 0.5,
			want: fills.Execution{Filled: false},
		},
		// --- Buys: gapped through.
		{
			// The reference (here the bar's open) was already above the
			// level, so the order was triggered at the first instant of the
			// bar and executed there — never at the level it rested at.
			name: "buy gapped through fills at the reference, not the level",
			side: fills.SideBuy, level: 100, reference: 102, high: 105, low: 101, slippage: 0.5,
			want: fills.Execution{Filled: true, Price: 102.5, AtReference: true},
		},
		{
			// The boundary between "at the level" and "gapped": a reference
			// exactly at the level is not a gap — max(level, reference) is
			// the level either way, and the order executed where it rested.
			name: "buy with the reference exactly at the level is not a gap",
			side: fills.SideBuy, level: 100, reference: 100, high: 105, low: 97, slippage: 0.5,
			want: fills.Execution{Filled: true, Price: 100.5},
		},
		{
			// The whole bar traded above the level: the two-sided coverage
			// test would say "no fill", which is the defect this case exists
			// to prevent (see the file comment).
			name: "buy fills even though the level sits below the bar's low",
			side: fills.SideBuy, level: 90, reference: 102, high: 105, low: 101, slippage: 0.5,
			want: fills.Execution{Filled: true, Price: 102.5, AtReference: true},
		},
		// --- Sells: the level inside the bar's range.
		{
			name: "sell fills at its level minus slippage when the level is inside the range",
			side: fills.SideSell, level: 100, reference: 102, high: 105, low: 97, slippage: 0.5,
			want: fills.Execution{Filled: true, Price: 99.5},
		},
		{
			name: "sell fills on an exact touch at the low",
			side: fills.SideSell, level: 97, reference: 102, high: 105, low: 97, slippage: 0.5,
			want: fills.Execution{Filled: true, Price: 96.5},
		},
		{
			name: "sell does not fill when the bar never reached the level",
			side: fills.SideSell, level: 96.99, reference: 102, high: 105, low: 97, slippage: 0.5,
			want: fills.Execution{Filled: false},
		},
		// --- Sells: gapped through.
		{
			name: "sell gapped through fills at the reference, not the level",
			side: fills.SideSell, level: 100, reference: 95, high: 99, low: 88, slippage: 0.5,
			want: fills.Execution{Filled: true, Price: 94.5, AtReference: true},
		},
		{
			name: "sell with the reference exactly at the level is not a gap",
			side: fills.SideSell, level: 100, reference: 100, high: 105, low: 97, slippage: 0.5,
			want: fills.Execution{Filled: true, Price: 99.5},
		},
		{
			// The mirror of the buy case above: the whole bar traded below
			// the level, so a two-sided coverage test would refuse the fill.
			name: "sell fills even though the level sits above the bar's high",
			side: fills.SideSell, level: 110, reference: 95, high: 99, low: 88, slippage: 0.5,
			want: fills.Execution{Filled: true, Price: 94.5, AtReference: true},
		},
		// --- Slippage sign, per side, at the same level and bar.
		{
			name: "slippage is added to a buy",
			side: fills.SideBuy, level: 157, reference: 155.5, high: 158, low: 155, slippage: testSlippage,
			want: fills.Execution{Filled: true, Price: 157.075},
		},
		{
			name: "slippage is subtracted from a sell",
			side: fills.SideSell, level: 157, reference: 158.5, high: 159, low: 155, slippage: testSlippage,
			want: fills.Execution{Filled: true, Price: 156.925},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := fills.Execute(tt.side, tt.level, fills.Range{
				Reference: tt.reference,
				High:      tt.high,
				Low:       tt.low,
			}, tt.slippage)
			if err != nil {
				t.Fatalf("Execute() error = %v, want nil", err)
			}
			if got.Filled != tt.want.Filled {
				t.Fatalf("Execute().Filled = %v, want %v", got.Filled, tt.want.Filled)
			}
			if !tt.want.Filled {
				return
			}
			if math.Abs(got.Price-tt.want.Price) > 1e-9 {
				t.Errorf("Execute().Price = %v, want %v", got.Price, tt.want.Price)
			}
			if got.AtReference != tt.want.AtReference {
				t.Errorf("Execute().AtReference = %v, want %v", got.AtReference, tt.want.AtReference)
			}
		})
	}
}

// TestGapThroughAlwaysFills is the property behind the file comment, asserted
// directly rather than only through the table: whenever the reference price
// already sits beyond the level, the order fills at the reference, whatever
// the rest of the bar did. It is the case a two-sided coverage test silently
// drops.
func TestGapThroughAlwaysFills(t *testing.T) {
	t.Parallel()

	buy, err := fills.Execute(fills.SideBuy, 100, fills.Range{Reference: 120, High: 121, Low: 119}, 0.25)
	if err != nil {
		t.Fatalf("Execute(buy) error = %v", err)
	}
	if !buy.Filled || !buy.AtReference || math.Abs(buy.Price-120.25) > 1e-9 {
		t.Errorf("Execute(buy) = %+v, want a fill at the reference 120 plus slippage", buy)
	}

	sell, err := fills.Execute(fills.SideSell, 100, fills.Range{Reference: 80, High: 81, Low: 79}, 0.25)
	if err != nil {
		t.Fatalf("Execute(sell) error = %v", err)
	}
	if !sell.Filled || !sell.AtReference || math.Abs(sell.Price-79.75) > 1e-9 {
		t.Errorf("Execute(sell) = %+v, want a fill at the reference 80 minus slippage", sell)
	}
}

// TestACloseOnlyStopCheckDoesNotMatchTheIntrabarFixture is the ticket's named
// negative, and .greptile/rules.md's "close-only stop checks" failure mode
// made executable: a bar whose LOW traded through the stop but whose CLOSE
// finished comfortably above it must fill the stop. The close-only oracle
// below is what the predecessor prototype did; it disagrees, which is the
// point.
func TestACloseOnlyStopCheckDoesNotMatchTheIntrabarFixture(t *testing.T) {
	t.Parallel()

	const (
		stopLevel = 154.075
		open      = 156.0
		high      = 157.0
		low       = 154.0
		closeAt   = 156.5
	)

	got, err := fills.Execute(fills.SideSell, stopLevel, fills.Range{Reference: open, High: high, Low: low}, testSlippage)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !got.Filled {
		t.Fatal("Execute().Filled = false, want true: the bar's low traded through the stop")
	}
	if want := stopLevel - testSlippage; math.Abs(got.Price-want) > 1e-9 {
		t.Errorf("Execute().Price = %v, want %v (the level, less slippage — the bar did not gap through it)", got.Price, want)
	}

	// The negative the ticket names: a stop evaluated against the close alone
	// would let this bar pass, understating gap risk and letting price trade
	// through the level intraday and recover.
	closeOnlyWouldFill := closeAt <= stopLevel
	if closeOnlyWouldFill {
		t.Fatal("the fixture is wrong: its close must sit ABOVE the stop, or it does not distinguish the two checks")
	}
}

func TestExecuteFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		side      string
		level     float64
		reference float64
		high      float64
		low       float64
		slippage  float64
		wantErr   string
	}{
		{
			name: "unrecognised side",
			side: "hold", level: 100, reference: 100, high: 105, low: 95, slippage: 0.5,
			wantErr: "side",
		},
		{
			name: "empty side",
			side: "", level: 100, reference: 100, high: 105, low: 95, slippage: 0.5,
			wantErr: "side",
		},
		{
			name: "non-finite level",
			side: fills.SideBuy, level: math.NaN(), reference: 100, high: 105, low: 95, slippage: 0.5,
			wantErr: "level",
		},
		{
			name: "non-positive level",
			side: fills.SideBuy, level: 0, reference: 100, high: 105, low: 95, slippage: 0.5,
			wantErr: "level",
		},
		{
			name: "non-finite reference",
			side: fills.SideBuy, level: 100, reference: math.Inf(1), high: 105, low: 95, slippage: 0.5,
			wantErr: "reference",
		},
		{
			name: "non-positive reference",
			side: fills.SideBuy, level: 100, reference: 0, high: 105, low: 95, slippage: 0.5,
			wantErr: "reference",
		},
		{
			name: "non-positive high",
			side: fills.SideBuy, level: 100, reference: 100, high: 0, low: 95, slippage: 0.5,
			wantErr: "high",
		},
		{
			name: "non-positive low",
			side: fills.SideBuy, level: 100, reference: 100, high: 105, low: -1, slippage: 0.5,
			wantErr: "low",
		},
		{
			name: "high below low",
			side: fills.SideBuy, level: 100, reference: 100, high: 95, low: 105, slippage: 0.5,
			wantErr: "high",
		},
		{
			// ADR 0013: "A backtest run with zero slippage is invalid by
			// construction." The arithmetic refuses it here as well as the
			// constructor, so no caller can reach an unslipped fill by any
			// route.
			name: "zero slippage",
			side: fills.SideBuy, level: 100, reference: 100, high: 105, low: 95, slippage: 0,
			wantErr: "slippage",
		},
		{
			name: "negative slippage",
			side: fills.SideBuy, level: 100, reference: 100, high: 105, low: 95, slippage: -0.5,
			wantErr: "slippage",
		},
		{
			// A sell whose slippage would take the executed price to or
			// below zero: not a fill any consumer could record (a price must
			// be positive), so it fails rather than being clamped.
			name: "sell slipped to a non-positive price",
			side: fills.SideSell, level: 0.25, reference: 0.3, high: 0.3, low: 0.2, slippage: 0.5,
			wantErr: "price",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := fills.Execute(tt.side, tt.level, fills.Range{
				Reference: tt.reference,
				High:      tt.high,
				Low:       tt.low,
			}, tt.slippage)
			if err == nil {
				t.Fatalf("Execute() error = nil, want an error naming %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Execute() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// --- ADR 0013's commission model ----------------------------------------

// baselineCommission is the Baseline's declared schedule: Interactive
// Brokers' published US-stock Fixed pricing. See the package doc comment for
// the provenance and the date it was taken.
func baselineCommission() fills.CommissionModel {
	return fills.CommissionModel{
		PerShare:                    0.005,
		MinimumPerOrder:             1.00,
		MaximumFractionOfTradeValue: 0.01,
	}
}

func TestCommissionModelCharge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		model           fills.CommissionModel
		quantity        int64
		price           float64
		dollarsPerPoint float64
		want            float64
		why             string
	}{
		{
			name: "per-share rate binds", model: baselineCommission(),
			quantity: 3333, price: 157.075, dollarsPerPoint: 1,
			want: 16.665,
			why:  "3333 x 0.005, comfortably above the 1.00 floor and far below 1 % of a 523,530 trade",
		},
		{
			name: "minimum binds", model: baselineCommission(),
			quantity: 100, price: 157.075, dollarsPerPoint: 1,
			want: 1.00,
			why:  "100 x 0.005 = 0.50, lifted to the 1.00 floor; 1 % of a 15,707 trade is far above it",
		},
		{
			// The order the two bounds are applied in is the whole content of
			// this case: IB's cap is a cap on the CHARGE, so it overrides the
			// floor rather than the other way round. Applying the floor last
			// would charge 1.00 on a 50-dollar trade — 2 % of it.
			name: "cap overrides the minimum on a tiny order", model: baselineCommission(),
			quantity: 100, price: 0.50, dollarsPerPoint: 1,
			want: 0.50,
			why:  "1 % of a 50.00 trade is 0.50, below the 1.00 floor: the cap wins",
		},
		{
			name: "cap binds on a large low-priced order", model: baselineCommission(),
			quantity: 100_000, price: 0.10, dollarsPerPoint: 1,
			want: 100.00,
			why:  "100,000 x 0.005 = 500, capped at 1 % of a 10,000 trade",
		},
		{
			// The contract multiplier is part of trade value (#10/#63): a
			// futures contract's 1 % cap is computed on the notional the
			// multiplier implies, not on the quoted price.
			name: "dollars per point enters the trade value", model: baselineCommission(),
			quantity: 1, price: 2.50, dollarsPerPoint: 42_000,
			want: 1.00,
			why:  "0.005 lifted to the floor; 1 % of 105,000 does not bind",
		},
		{
			// A commission-free Variant: nothing in the methodology requires
			// a commission to exist, unlike slippage (ADR 0013).
			name:     "a commission-free schedule charges nothing",
			model:    fills.CommissionModel{PerShare: 0, MinimumPerOrder: 0, MaximumFractionOfTradeValue: 1},
			quantity: 3333, price: 157.075, dollarsPerPoint: 1,
			want: 0,
			why:  "no rate and no floor",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := tt.model.Charge(tt.quantity, tt.price, tt.dollarsPerPoint)
			if err != nil {
				t.Fatalf("Charge() error = %v, want nil", err)
			}
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("Charge() = %v, want %v (%s)", got, tt.want, tt.why)
			}
		})
	}
}

func TestCommissionModelChargeFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		model           fills.CommissionModel
		quantity        int64
		price           float64
		dollarsPerPoint float64
		wantErr         string
	}{
		{"zero quantity", baselineCommission(), 0, 100, 1, "quantity"},
		{"negative quantity", baselineCommission(), -1, 100, 1, "quantity"},
		{"non-finite price", baselineCommission(), 100, math.NaN(), 1, "price"},
		{"non-positive price", baselineCommission(), 100, 0, 1, "price"},
		{"non-positive dollars per point", baselineCommission(), 100, 100, 0, "dollars per point"},
		{
			"negative per-share rate",
			fills.CommissionModel{PerShare: -0.005, MinimumPerOrder: 1, MaximumFractionOfTradeValue: 0.01},
			100, 100, 1, "per share",
		},
		{
			"negative minimum",
			fills.CommissionModel{PerShare: 0.005, MinimumPerOrder: -1, MaximumFractionOfTradeValue: 0.01},
			100, 100, 1, "minimum",
		},
		{
			"zero cap",
			fills.CommissionModel{PerShare: 0.005, MinimumPerOrder: 1, MaximumFractionOfTradeValue: 0},
			100, 100, 1, "maximum",
		},
		{
			"cap above one",
			fills.CommissionModel{PerShare: 0.005, MinimumPerOrder: 1, MaximumFractionOfTradeValue: 1.5},
			100, 100, 1, "maximum",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := tt.model.Charge(tt.quantity, tt.price, tt.dollarsPerPoint)
			if err == nil {
				t.Fatalf("Charge() error = nil, want an error naming %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Charge() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}
