package sizing_test

import (
	"math"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// TestUnitQuantityHeatingOilGolden is the ticket's required golden: Faith's
// own worked Unit-sizing example, transcribed rather than invented
// (AGENTS.md rule 1).
//
// The Turtle Rules p.14 gives the formula — one Unit is 1 % of the account
// divided by the market's dollar volatility, N x dollars per point — and
// p.15 works it for Heating Oil on 2002-12-04: N = 0.0141, a $1,000,000
// account, a 42,000-gallon contract.
//
//	  1,000,000 x 0.01       10,000
//	--------------------- = --------- = 16.88... -> 16 contracts
//	   0.0141 x 42,000        592.2
//
// Faith truncates; 16.88 becomes 16, never 17. N = 0.0141 on that date is
// the value #8 pinned from the same table, so this golden is continuous with
// the one before it: the N test proves the input, this test proves the
// arithmetic that turns it into a position.
func TestUnitQuantityHeatingOilGolden(t *testing.T) {
	t.Parallel()

	got, err := sizing.UnitQuantity(1_000_000, 0.01, 0.0141, 42_000)
	if err != nil {
		t.Fatalf("UnitQuantity() error = %v, want nil", err)
	}
	if got != 16 {
		t.Fatalf("UnitQuantity() = %d, want 16 contracts (The Turtle Rules p.15: 16.88 truncated to 16)", got)
	}
}

// TestUnitQuantityEquityCases covers the multiplier-of-one form the Baseline
// actually trades (CONTEXT.md: the platform is stock-first; for shares
// dollars-per-point is exactly 1), with every expected value hand-computed
// in its own comment.
func TestUnitQuantityEquityCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                   string
		notionalAccount        float64
		unitVolatilityFraction float64
		n                      float64
		dollarsPerPoint        float64
		want                   int64
	}{
		{
			// 100,000 x 0.005 = 500 of 1N budget; 500 / (2.5 x 1) = 200
			// exactly. Nothing to truncate, so a truncation bug cannot hide
			// here — which is why the next two cases exist.
			name:                   "exact division needs no truncation",
			notionalAccount:        100_000,
			unitVolatilityFraction: 0.005,
			n:                      2.5,
			dollarsPerPoint:        1,
			want:                   200,
		},
		{
			// 100,000 x 0.005 = 500; 500 / (3 x 1) = 166.666... -> 166.
			// The fractional part is .666, comfortably ABOVE .5: an
			// implementation that rounded to nearest would return 167, so
			// this case pins the truncation *direction*, not merely that
			// some rounding happens. It is also the equity shape of the
			// reconstructed Sublime example recorded in
			// docs/methodology/Methodology_Analysis.md (100k, 1 %, entry 50,
			// stop 44 -> 166 shares); see
			// TestFixedRiskAtStopQuantitySublimeReconstructedExample for that
			// example in its own mode.
			name:                   "truncates down from a fractional part above one half",
			notionalAccount:        100_000,
			unitVolatilityFraction: 0.005,
			n:                      3,
			dollarsPerPoint:        1,
			want:                   166,
		},
		{
			// Faith's Heating Oil inputs with the equity multiplier of 1, as
			// the ticket asks: 1,000,000 x 0.01 = 10,000;
			// 10,000 / (0.0141 x 1) = 709,219.858... -> 709,219. A second
			// fractional part above .5, at a completely different order of
			// magnitude from the case above.
			name:                   "heating oil inputs with an equity multiplier of one",
			notionalAccount:        1_000_000,
			unitVolatilityFraction: 0.01,
			n:                      0.0141,
			dollarsPerPoint:        1,
			want:                   709_219,
		},
		{
			// An account too small for a single share: 100 x 0.005 = 0.5;
			// 0.5 / (40 x 1) = 0.0125 -> 0. Zero is a legitimate arithmetic
			// result, not an error — The Turtle Rules p.15 notes small
			// accounts lose diversification precisely because truncation is
			// coarse. Declining the trade is the caller's job (see
			// event.ProposalDeclinedPayload), so this function must report 0
			// cleanly rather than fail.
			name:                   "an account too small for one share yields zero, not an error",
			notionalAccount:        100,
			unitVolatilityFraction: 0.005,
			n:                      40,
			dollarsPerPoint:        1,
			want:                   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := sizing.UnitQuantity(tt.notionalAccount, tt.unitVolatilityFraction, tt.n, tt.dollarsPerPoint)
			if err != nil {
				t.Fatalf("UnitQuantity() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Fatalf("UnitQuantity() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestUnitQuantityTruncatesToTheTrueFloorAtAFloatBoundary pins the one place
// where "floor the quotient" is not the same thing as "floor of the
// mathematical quotient".
//
// Floating-point division is correctly rounded to nearest, so a quotient
// whose true value sits just below an integer can round UP to that integer,
// and flooring the rounded value then returns one share too many — a
// position that risks slightly more than the configured fraction. The error
// only ever points this way (a true quotient at or above an integer can
// never round below it), so a single correction downward is enough.
//
// The fixture below is constructed, not observed in the wild: the budget is
// the largest float64 strictly below 4437 x N, so the true quotient is
// fractionally under 4437 while the rounded quotient is exactly 4437.
// UnitQuantity must return 4436. The test also asserts that the naive
// computation still returns 4437, so that if a future change to the
// arithmetic stops exercising the boundary, this test says so rather than
// passing vacuously.
func TestUnitQuantityTruncatesToTheTrueFloorAtAFloatBoundary(t *testing.T) {
	t.Parallel()

	const (
		notionalAccount        = 81772.54788536497
		unitVolatilityFraction = 0.5
		n                      = 9.214846504999434
		dollarsPerPoint        = 1.0
	)

	budget := notionalAccount * unitVolatilityFraction
	if naive := math.Floor(budget / (n * dollarsPerPoint)); naive != 4437 {
		t.Fatalf("fixture no longer exercises the float boundary: naive floor = %v, want 4437", naive)
	}

	got, err := sizing.UnitQuantity(notionalAccount, unitVolatilityFraction, n, dollarsPerPoint)
	if err != nil {
		t.Fatalf("UnitQuantity() error = %v, want nil", err)
	}
	if got != 4436 {
		t.Fatalf("UnitQuantity() = %d, want 4436 (the true floor; 4437 risks more than the configured fraction)", got)
	}
	if risked := float64(got) * (n * dollarsPerPoint); risked > budget {
		t.Fatalf("quantity %d risks %v of 1N budget, which exceeds the budget %v", got, risked, budget)
	}
}

// TestUnitQuantityFailsClosed covers the inputs that must never produce a
// position: .greptile/rules.md — "A zero, negative, or not-yet-warm
// volatility value must fail closed — never size a position from it."
// Non-finite values are rejected explicitly, before any ordered comparison,
// because every ordered comparison against NaN is false.
func TestUnitQuantityFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                   string
		notionalAccount        float64
		unitVolatilityFraction float64
		n                      float64
		dollarsPerPoint        float64
		wantErr                string
	}{
		{"zero n", 1_000_000, 0.005, 0, 1, "n must be positive"},
		{"negative n", 1_000_000, 0.005, -2.5, 1, "n must be positive"},
		{"NaN n", 1_000_000, 0.005, math.NaN(), 1, "n must be finite"},
		{"positive infinite n", 1_000_000, 0.005, math.Inf(1), 1, "n must be finite"},
		{"negative infinite n", 1_000_000, 0.005, math.Inf(-1), 1, "n must be finite"},
		{"zero notional account", 0, 0.005, 2.5, 1, "notional account must be positive"},
		{"negative notional account", -1_000_000, 0.005, 2.5, 1, "notional account must be positive"},
		{"NaN notional account", math.NaN(), 0.005, 2.5, 1, "notional account must be finite"},
		{"infinite notional account", math.Inf(1), 0.005, 2.5, 1, "notional account must be finite"},
		{"zero unit volatility fraction", 1_000_000, 0, 2.5, 1, "unit volatility fraction must be greater than zero and at most one"},
		{"negative unit volatility fraction", 1_000_000, -0.005, 2.5, 1, "unit volatility fraction must be greater than zero and at most one"},
		{"unit volatility fraction above one", 1_000_000, 1.5, 2.5, 1, "unit volatility fraction must be greater than zero and at most one"},
		{"NaN unit volatility fraction", 1_000_000, math.NaN(), 2.5, 1, "unit volatility fraction must be finite"},
		{"infinite unit volatility fraction", 1_000_000, math.Inf(1), 2.5, 1, "unit volatility fraction must be finite"},
		{"zero dollars per point", 1_000_000, 0.005, 2.5, 0, "dollars per point must be positive"},
		{"negative dollars per point", 1_000_000, 0.005, 2.5, -1, "dollars per point must be positive"},
		{"NaN dollars per point", 1_000_000, 0.005, 2.5, math.NaN(), "dollars per point must be finite"},
		{"infinite dollars per point", 1_000_000, 0.005, 2.5, math.Inf(1), "dollars per point must be finite"},
		{
			// A near-zero N with a large account produces a quantity no
			// int64 can hold exactly. Converting a float64 above 2^53 to
			// int64 silently loses the very precision the truncation rule
			// depends on, and above 2^63 the conversion is undefined, so the
			// only safe answer is to refuse.
			name:                   "quantity beyond exact integer precision",
			notionalAccount:        1e18,
			unitVolatilityFraction: 1,
			n:                      1e-9,
			dollarsPerPoint:        1,
			wantErr:                "exceeds the largest exactly representable whole quantity",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := sizing.UnitQuantity(tt.notionalAccount, tt.unitVolatilityFraction, tt.n, tt.dollarsPerPoint)
			if err == nil {
				t.Fatalf("UnitQuantity() = %d, error = nil; want an error containing %q", got, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("UnitQuantity() error = %v, want substring %q", err, tt.wantErr)
			}
			if got != 0 {
				t.Fatalf("UnitQuantity() = %d alongside an error, want 0: a failed sizing must never hand back a quantity", got)
			}
		})
	}
}

// TestFixedRiskAtStopQuantitySublimeReconstructedExample transcribes the
// Sublime sizing form [M p.56]: shares such that the entry-to-stop distance
// times the share count does not exceed the chosen risk percentage of the
// account. docs/methodology/Methodology_Analysis.md records the worked
// example as $100,000 at 1 %, entry 50, stop 44 -> 166 shares.
//
// Expressed in this function's parameters the entry-to-stop distance is
// StopMultiple x N x dollars per point, so Sublime's roughly 3xATR stop
// [M p.55-56] with N = 2 reproduces the distance of 6:
//
//	100,000 x 0.01       1,000
//	-------------- = ------------- = 166.66... -> 166 shares
//	   3 x 2 x 1           6
//
// This is NOT the Baseline. ADR 0003 makes the Baseline volatility-normalised
// and this form the Sublime Variant; the two agree only at Stop Multiple 2
// (see TestFixedRiskAtStopQuantityEqualsVolatilityNormalisedAtStopMultipleTwo).
func TestFixedRiskAtStopQuantitySublimeReconstructedExample(t *testing.T) {
	t.Parallel()

	got, err := sizing.FixedRiskAtStopQuantity(100_000, 0.01, 3, 2, 1)
	if err != nil {
		t.Fatalf("FixedRiskAtStopQuantity() error = %v, want nil", err)
	}
	if got != 166 {
		t.Fatalf("FixedRiskAtStopQuantity() = %d, want 166 shares [M p.56]", got)
	}
}

// TestFixedRiskAtStopQuantityEqualsVolatilityNormalisedAtStopMultipleTwo is
// ADR 0003's identity, asserted rather than assumed: the Sublime form
// `account x risk / (stopMultiple x N)` is algebraically the Turtle form
// `account x fraction / N` exactly when Stop Multiple is 2 and the risk
// fraction is twice the Unit Volatility Fraction. ADR 0003 exists because
// both the Notion notes and the QuantConnect prototype collapsed the two into
// this single case and then diverged silently the moment the stop widened.
func TestFixedRiskAtStopQuantityEqualsVolatilityNormalisedAtStopMultipleTwo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                   string
		notionalAccount        float64
		unitVolatilityFraction float64
		n                      float64
		dollarsPerPoint        float64
	}{
		{"equities", 100_000, 0.005, 3, 1},
		{"faith's fraction on equities", 1_000_000, 0.01, 40.69890254048816, 1},
		{"heating oil", 1_000_000, 0.01, 0.0141, 42_000},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const stopMultiple = 2.0

			volatilityNormalised, err := sizing.UnitQuantity(tt.notionalAccount, tt.unitVolatilityFraction, tt.n, tt.dollarsPerPoint)
			if err != nil {
				t.Fatalf("UnitQuantity() error = %v", err)
			}
			fixed, err := sizing.FixedRiskAtStopQuantity(tt.notionalAccount, tt.unitVolatilityFraction*stopMultiple, stopMultiple, tt.n, tt.dollarsPerPoint)
			if err != nil {
				t.Fatalf("FixedRiskAtStopQuantity() error = %v", err)
			}
			if volatilityNormalised != fixed {
				t.Fatalf("at stop multiple 2 the two modes disagree: volatility-normalised = %d, fixed-risk-at-stop = %d (ADR 0003 says they coincide here)", volatilityNormalised, fixed)
			}
		})
	}
}

// TestModesDivergeAwayFromStopMultipleTwo is the other half of ADR 0003, and
// the reason the Sizing Mode is an explicit configuration value rather than
// an implementation detail. Widening the stop from 2N to 3N:
//
//   - fixed-risk-at-stop keeps Risk at Stop constant and buys FEWER shares
//     (100,000 x 0.01 / (3 x 3 x 1) = 111.1... -> 111, down from 166);
//   - volatility-normalised keeps the SAME share count (166; the stop is not
//     an input to the size at all) and Risk at Stop rises from 0.01 to
//     0.015.
//
// Collapsing the two would silently re-scale the whole book the moment the
// 3xATR stop experiment runs.
func TestModesDivergeAwayFromStopMultipleTwo(t *testing.T) {
	t.Parallel()

	const (
		notionalAccount        = 100_000.0
		unitVolatilityFraction = 0.005
		riskAtStopFraction     = 0.01 // = 0.005 x 2, the Stop-Multiple-2 equivalent
		n                      = 3.0
		dollarsPerPoint        = 1.0
	)

	fixedAtTwo, err := sizing.FixedRiskAtStopQuantity(notionalAccount, riskAtStopFraction, 2, n, dollarsPerPoint)
	if err != nil {
		t.Fatalf("FixedRiskAtStopQuantity(stop multiple 2) error = %v", err)
	}
	fixedAtThree, err := sizing.FixedRiskAtStopQuantity(notionalAccount, riskAtStopFraction, 3, n, dollarsPerPoint)
	if err != nil {
		t.Fatalf("FixedRiskAtStopQuantity(stop multiple 3) error = %v", err)
	}
	if fixedAtTwo != 166 {
		t.Errorf("fixed-risk-at-stop at 2N = %d, want 166", fixedAtTwo)
	}
	if fixedAtThree != 111 {
		t.Errorf("fixed-risk-at-stop at 3N = %d, want 111 (a wider stop buys fewer shares)", fixedAtThree)
	}
	if !(fixedAtThree < fixedAtTwo) {
		t.Errorf("fixed-risk-at-stop: widening the stop must reduce the share count, got %d then %d", fixedAtTwo, fixedAtThree)
	}

	// Risk at Stop is unchanged under fixed-risk-at-stop: it is the input.
	for _, stopMultiple := range []float64{2, 3} {
		riskAtStop, err := sizing.RiskAtStop(sizing.ModeFixedRiskAtStop, unitVolatilityFraction, stopMultiple, riskAtStopFraction)
		if err != nil {
			t.Fatalf("RiskAtStop(fixed, stop multiple %v) error = %v", stopMultiple, err)
		}
		if riskAtStop != riskAtStopFraction {
			t.Errorf("RiskAtStop(fixed, stop multiple %v) = %v, want %v (constant by construction)", stopMultiple, riskAtStop, riskAtStopFraction)
		}
	}

	// Volatility-normalised: same share count at both stop multiples, and a
	// Risk at Stop that visibly rises with the stop.
	volatilityNormalised, err := sizing.UnitQuantity(notionalAccount, unitVolatilityFraction, n, dollarsPerPoint)
	if err != nil {
		t.Fatalf("UnitQuantity() error = %v", err)
	}
	if volatilityNormalised != 166 {
		t.Errorf("volatility-normalised = %d, want 166 at every stop multiple", volatilityNormalised)
	}
	atTwo, err := sizing.RiskAtStop(sizing.ModeVolatilityNormalised, unitVolatilityFraction, 2, 0)
	if err != nil {
		t.Fatalf("RiskAtStop(volatility-normalised, 2) error = %v", err)
	}
	atThree, err := sizing.RiskAtStop(sizing.ModeVolatilityNormalised, unitVolatilityFraction, 3, 0)
	if err != nil {
		t.Fatalf("RiskAtStop(volatility-normalised, 3) error = %v", err)
	}
	if atTwo != unitVolatilityFraction*2 || atThree != unitVolatilityFraction*3 {
		t.Fatalf("RiskAtStop(volatility-normalised) = %v then %v, want %v then %v", atTwo, atThree, unitVolatilityFraction*2, unitVolatilityFraction*3)
	}
	if !(atThree > atTwo) {
		t.Errorf("volatility-normalised: widening the stop must raise Risk at Stop, got %v then %v", atTwo, atThree)
	}
}

// TestFixedRiskAtStopQuantityFailsClosed mirrors TestUnitQuantityFailsClosed
// for the Sublime form, which takes the Stop Multiple as an extra divisor and
// so has one more way to divide by zero.
func TestFixedRiskAtStopQuantityFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		notionalAccount    float64
		riskAtStopFraction float64
		stopMultiple       float64
		n                  float64
		dollarsPerPoint    float64
		wantErr            string
	}{
		{"zero n", 1_000_000, 0.01, 2, 0, 1, "n must be positive"},
		{"negative n", 1_000_000, 0.01, 2, -1, 1, "n must be positive"},
		{"NaN n", 1_000_000, 0.01, 2, math.NaN(), 1, "n must be finite"},
		{"zero stop multiple", 1_000_000, 0.01, 0, 2.5, 1, "stop multiple must be positive"},
		{"negative stop multiple", 1_000_000, 0.01, -2, 2.5, 1, "stop multiple must be positive"},
		{"NaN stop multiple", 1_000_000, 0.01, math.NaN(), 2.5, 1, "stop multiple must be finite"},
		{"zero notional account", 0, 0.01, 2, 2.5, 1, "notional account must be positive"},
		{"NaN notional account", math.NaN(), 0.01, 2, 2.5, 1, "notional account must be finite"},
		{"zero risk at stop fraction", 1_000_000, 0, 2, 2.5, 1, "risk at stop fraction must be greater than zero and at most one"},
		{"risk at stop fraction above one", 1_000_000, 1.5, 2, 2.5, 1, "risk at stop fraction must be greater than zero and at most one"},
		{"NaN risk at stop fraction", 1_000_000, math.NaN(), 2, 2.5, 1, "risk at stop fraction must be finite"},
		{"zero dollars per point", 1_000_000, 0.01, 2, 2.5, 0, "dollars per point must be positive"},
		{"infinite dollars per point", 1_000_000, 0.01, 2, 2.5, math.Inf(1), "dollars per point must be finite"},
		{"quantity beyond exact integer precision", 1e18, 1, 1, 1e-9, 1, "exceeds the largest exactly representable whole quantity"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := sizing.FixedRiskAtStopQuantity(tt.notionalAccount, tt.riskAtStopFraction, tt.stopMultiple, tt.n, tt.dollarsPerPoint)
			if err == nil {
				t.Fatalf("FixedRiskAtStopQuantity() = %d, error = nil; want an error containing %q", got, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("FixedRiskAtStopQuantity() error = %v, want substring %q", err, tt.wantErr)
			}
			if got != 0 {
				t.Fatalf("FixedRiskAtStopQuantity() = %d alongside an error, want 0", got)
			}
		})
	}
}

// TestRiskAtStopDerivation covers CONTEXT.md's "Risk at Stop" and ADR 0003's
// central rule: under volatility-normalised sizing Risk at Stop is DERIVED
// (Unit Volatility Fraction x Stop Multiple) and supplying it as an input is
// an error, not an override; under fixed-risk-at-stop it IS the input.
func TestRiskAtStopDerivation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                   string
		mode                   sizing.Mode
		unitVolatilityFraction float64
		stopMultiple           float64
		riskAtStopFraction     float64
		want                   float64
		wantErr                string
	}{
		{
			// The Baseline: 0.5 % per N with a 2N stop (ADR 0003) risks 1 %
			// of the Notional Account at the Protective Stop.
			name:                   "baseline volatility-normalised",
			mode:                   sizing.ModeVolatilityNormalised,
			unitVolatilityFraction: 0.005,
			stopMultiple:           2,
			want:                   0.005 * 2,
		},
		{
			// Faith's own fraction, retained as a declared Variant (ADR
			// 0003): 1 % per N with a 2N stop is the 2 % at the stop that
			// The Turtle Rules p.14 and p.22 describe.
			name:                   "faith's fraction as a declared variant",
			mode:                   sizing.ModeVolatilityNormalised,
			unitVolatilityFraction: 0.01,
			stopMultiple:           2,
			want:                   0.01 * 2,
		},
		{
			// The whole point of deriving rather than configuring: widening
			// the stop to 3N raises Risk at Stop by half, visibly, instead
			// of silently re-scaling the book.
			name:                   "a wider stop raises the derived risk",
			mode:                   sizing.ModeVolatilityNormalised,
			unitVolatilityFraction: 0.005,
			stopMultiple:           3,
			want:                   0.005 * 3,
		},
		{
			name:                   "fixed-risk-at-stop returns its input",
			mode:                   sizing.ModeFixedRiskAtStop,
			unitVolatilityFraction: 0.005,
			stopMultiple:           3,
			riskAtStopFraction:     0.02,
			want:                   0.02,
		},
		{
			// The ticket's named invariant, enforced at the arithmetic seam
			// as well as at configuration time: Risk at Stop must not be
			// configurable in the Baseline mode.
			name:                   "a configured risk at stop is rejected under volatility-normalised",
			mode:                   sizing.ModeVolatilityNormalised,
			unitVolatilityFraction: 0.005,
			stopMultiple:           2,
			riskAtStopFraction:     0.01,
			wantErr:                "risk at stop must not be configured under volatility-normalised sizing",
		},
		{
			name:                   "unrecognised mode",
			mode:                   "risk-parity",
			unitVolatilityFraction: 0.005,
			stopMultiple:           2,
			wantErr:                "is not a recognised sizing mode",
		},
		{
			name:                   "zero unit volatility fraction",
			mode:                   sizing.ModeVolatilityNormalised,
			unitVolatilityFraction: 0,
			stopMultiple:           2,
			wantErr:                "unit volatility fraction must be greater than zero and at most one",
		},
		{
			name:                   "NaN unit volatility fraction",
			mode:                   sizing.ModeVolatilityNormalised,
			unitVolatilityFraction: math.NaN(),
			stopMultiple:           2,
			wantErr:                "unit volatility fraction must be finite",
		},
		{
			name:                   "zero stop multiple",
			mode:                   sizing.ModeVolatilityNormalised,
			unitVolatilityFraction: 0.005,
			stopMultiple:           0,
			wantErr:                "stop multiple must be positive",
		},
		{
			name:                   "infinite stop multiple",
			mode:                   sizing.ModeVolatilityNormalised,
			unitVolatilityFraction: 0.005,
			stopMultiple:           math.Inf(1),
			wantErr:                "stop multiple must be finite",
		},
		{
			// A derived Risk at Stop above 1 means one Unit would lose more
			// than the entire Notional Account at its Protective Stop. That
			// is a configuration defect, not a market condition, so it fails
			// closed here rather than reaching a proposal.
			name:                   "a derived risk above the whole account is rejected",
			mode:                   sizing.ModeVolatilityNormalised,
			unitVolatilityFraction: 0.5,
			stopMultiple:           3,
			wantErr:                "derived risk at stop",
		},
		{
			name:                   "zero risk at stop fraction under fixed-risk-at-stop",
			mode:                   sizing.ModeFixedRiskAtStop,
			unitVolatilityFraction: 0.005,
			stopMultiple:           3,
			riskAtStopFraction:     0,
			wantErr:                "risk at stop fraction must be greater than zero and at most one",
		},
		{
			name:                   "NaN risk at stop fraction under fixed-risk-at-stop",
			mode:                   sizing.ModeFixedRiskAtStop,
			unitVolatilityFraction: 0.005,
			stopMultiple:           3,
			riskAtStopFraction:     math.NaN(),
			wantErr:                "risk at stop fraction must be finite",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := sizing.RiskAtStop(tt.mode, tt.unitVolatilityFraction, tt.stopMultiple, tt.riskAtStopFraction)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("RiskAtStop() error = %v, want nil", err)
				}
				// Exact equality is deliberate here, not a price comparison:
				// the point of the invariant is that the stated Risk at Stop
				// is the identical float64 the derivation produces, so a
				// tolerance would defeat it.
				if got != tt.want {
					t.Fatalf("RiskAtStop() = %v, want %v", got, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("RiskAtStop() = %v, error = nil; want an error containing %q", got, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("RiskAtStop() error = %v, want substring %q", err, tt.wantErr)
			}
			if got != 0 {
				t.Fatalf("RiskAtStop() = %v alongside an error, want 0", got)
			}
		})
	}
}

// TestSizeUnitMatchesTheStandaloneArithmetic confirms the one-call entry
// point the reducer uses agrees with the two primitives it is built from, in
// both modes, rather than being a second implementation that could drift.
func TestSizeUnitMatchesTheStandaloneArithmetic(t *testing.T) {
	t.Parallel()

	t.Run("volatility-normalised", func(t *testing.T) {
		t.Parallel()

		unit, err := sizing.SizeUnit(sizing.Inputs{
			Mode:                   sizing.ModeVolatilityNormalised,
			NotionalAccount:        1_000_000,
			UnitVolatilityFraction: 0.01,
			StopMultiple:           2,
			N:                      0.0141,
			DollarsPerPoint:        42_000,
		})
		if err != nil {
			t.Fatalf("SizeUnit() error = %v", err)
		}
		if unit.Quantity != 16 {
			t.Errorf("Quantity = %d, want 16 (The Turtle Rules p.15)", unit.Quantity)
		}
		if unit.RiskAtStop != 0.01*2 {
			t.Errorf("RiskAtStop = %v, want %v", unit.RiskAtStop, 0.01*2)
		}
	})

	t.Run("fixed-risk-at-stop", func(t *testing.T) {
		t.Parallel()

		unit, err := sizing.SizeUnit(sizing.Inputs{
			Mode:                   sizing.ModeFixedRiskAtStop,
			NotionalAccount:        100_000,
			UnitVolatilityFraction: 0.005,
			StopMultiple:           3,
			RiskAtStopFraction:     0.01,
			N:                      2,
			DollarsPerPoint:        1,
		})
		if err != nil {
			t.Fatalf("SizeUnit() error = %v", err)
		}
		if unit.Quantity != 166 {
			t.Errorf("Quantity = %d, want 166 [M p.56]", unit.Quantity)
		}
		if unit.RiskAtStop != 0.01 {
			t.Errorf("RiskAtStop = %v, want 0.01 (the input, under this mode)", unit.RiskAtStop)
		}
	})
}

// TestSizeUnitFailsClosed covers the paths that belong to SizeUnit itself
// rather than to the primitives: an unrecognised mode, and a Risk at Stop
// configured under volatility-normalised.
func TestSizeUnitFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		inputs  sizing.Inputs
		wantErr string
	}{
		{
			name: "unrecognised mode",
			inputs: sizing.Inputs{
				Mode:                   "risk-parity",
				NotionalAccount:        1_000_000,
				UnitVolatilityFraction: 0.005,
				StopMultiple:           2,
				N:                      2.5,
				DollarsPerPoint:        1,
			},
			wantErr: "is not a recognised sizing mode",
		},
		{
			name: "risk at stop configured under volatility-normalised",
			inputs: sizing.Inputs{
				Mode:                   sizing.ModeVolatilityNormalised,
				NotionalAccount:        1_000_000,
				UnitVolatilityFraction: 0.005,
				StopMultiple:           2,
				RiskAtStopFraction:     0.02,
				N:                      2.5,
				DollarsPerPoint:        1,
			},
			wantErr: "risk at stop must not be configured under volatility-normalised sizing",
		},
		{
			name: "not-yet-warm n",
			inputs: sizing.Inputs{
				Mode:                   sizing.ModeVolatilityNormalised,
				NotionalAccount:        1_000_000,
				UnitVolatilityFraction: 0.005,
				StopMultiple:           2,
				N:                      0,
				DollarsPerPoint:        1,
			},
			wantErr: "n must be positive",
		},
		{
			name: "missing risk at stop fraction under fixed-risk-at-stop",
			inputs: sizing.Inputs{
				Mode:                   sizing.ModeFixedRiskAtStop,
				NotionalAccount:        1_000_000,
				UnitVolatilityFraction: 0.005,
				StopMultiple:           2,
				N:                      2.5,
				DollarsPerPoint:        1,
			},
			wantErr: "risk at stop fraction must be greater than zero and at most one",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			unit, err := sizing.SizeUnit(tt.inputs)
			if err == nil {
				t.Fatalf("SizeUnit() = %+v, error = nil; want an error containing %q", unit, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("SizeUnit() error = %v, want substring %q", err, tt.wantErr)
			}
			if unit.Quantity != 0 || unit.RiskAtStop != 0 {
				t.Fatalf("SizeUnit() = %+v alongside an error, want the zero Unit", unit)
			}
		})
	}
}

// TestSizeUnitGridInvariants is the ticket's property test. It walks a
// deterministic grid — deliberately not math/rand, which .golangci.yml's
// forbidigo rules ban anywhere in internal/ because randomness must never
// influence a trading decision — and asserts, for every combination:
//
//  1. Under volatility-normalised, Risk at Stop is EXACTLY Unit Volatility
//     Fraction x Stop Multiple. Exact float64 equality is the point: the
//     journal's stated Risk at Stop must be the identical value the
//     derivation produces, so any tolerance would let a differently-derived
//     number through.
//  2. Under fixed-risk-at-stop, Risk at Stop is exactly the configured
//     fraction.
//  3. In both modes, Quantity x StopMultiple x N x DollarsPerPoint <=
//     NotionalAccount x RiskAtStop — truncation may only ever risk LESS than
//     the derived fraction, never more. This is the invariant that a
//     rounding bug (or the float boundary in
//     TestUnitQuantityTruncatesToTheTrueFloorAtAFloatBoundary) would break.
//  4. In volatility-normalised mode the tighter 1N form also holds:
//     Quantity x N x DollarsPerPoint <= NotionalAccount x
//     UnitVolatilityFraction. That is the budget The Turtle Rules p.14
//     actually states, and the one event.TradeProposalPayload.Validate
//     re-checks.
func TestSizeUnitGridInvariants(t *testing.T) {
	t.Parallel()

	accounts := []float64{10_000, 100_000, 1_000_000, 7_531_842.75}
	fractions := []float64{0.0025, 0.005, 0.01, 0.02}
	// Including Faith's Heating Oil N and the N the reducer fixture in
	// internal/strategy produces, so the grid spans four orders of magnitude.
	nValues := []float64{0.0141, 0.25, 1, 2.5, 3, 40.69890254048816, 137.5}
	stopMultiples := []float64{1, 1.5, 2, 3, 4}
	dollarsPerPoints := []float64{1, 100, 42_000}

	combinations := 0
	for _, account := range accounts {
		for _, fraction := range fractions {
			for _, n := range nValues {
				for _, stopMultiple := range stopMultiples {
					for _, dollarsPerPoint := range dollarsPerPoints {
						combinations++

						volatilityNormalised, err := sizing.SizeUnit(sizing.Inputs{
							Mode:                   sizing.ModeVolatilityNormalised,
							NotionalAccount:        account,
							UnitVolatilityFraction: fraction,
							StopMultiple:           stopMultiple,
							N:                      n,
							DollarsPerPoint:        dollarsPerPoint,
						})
						if err != nil {
							t.Fatalf("SizeUnit(volatility-normalised, account=%v fraction=%v n=%v stopMultiple=%v dollarsPerPoint=%v) error = %v",
								account, fraction, n, stopMultiple, dollarsPerPoint, err)
						}
						if volatilityNormalised.RiskAtStop != fraction*stopMultiple {
							t.Fatalf("RiskAtStop = %v, want exactly %v (fraction %v x stop multiple %v)",
								volatilityNormalised.RiskAtStop, fraction*stopMultiple, fraction, stopMultiple)
						}
						if volatilityNormalised.Quantity < 0 {
							t.Fatalf("Quantity = %d, want a non-negative whole number", volatilityNormalised.Quantity)
						}
						if risked := float64(volatilityNormalised.Quantity) * (n * dollarsPerPoint); risked > account*fraction {
							t.Fatalf("volatility-normalised 1N budget exceeded: %d x %v x %v = %v > %v",
								volatilityNormalised.Quantity, n, dollarsPerPoint, risked, account*fraction)
						}
						if atStop := float64(volatilityNormalised.Quantity) * (stopMultiple * n * dollarsPerPoint); atStop > account*volatilityNormalised.RiskAtStop {
							t.Fatalf("volatility-normalised risk at stop exceeded: %v > %v",
								atStop, account*volatilityNormalised.RiskAtStop)
						}

						fixed, err := sizing.SizeUnit(sizing.Inputs{
							Mode:                   sizing.ModeFixedRiskAtStop,
							NotionalAccount:        account,
							UnitVolatilityFraction: fraction,
							StopMultiple:           stopMultiple,
							RiskAtStopFraction:     fraction,
							N:                      n,
							DollarsPerPoint:        dollarsPerPoint,
						})
						if err != nil {
							t.Fatalf("SizeUnit(fixed-risk-at-stop, account=%v fraction=%v n=%v stopMultiple=%v dollarsPerPoint=%v) error = %v",
								account, fraction, n, stopMultiple, dollarsPerPoint, err)
						}
						if fixed.RiskAtStop != fraction {
							t.Fatalf("RiskAtStop = %v, want exactly the configured %v", fixed.RiskAtStop, fraction)
						}
						if fixed.Quantity < 0 {
							t.Fatalf("Quantity = %d, want a non-negative whole number", fixed.Quantity)
						}
						if atStop := float64(fixed.Quantity) * (stopMultiple * n * dollarsPerPoint); atStop > account*fixed.RiskAtStop {
							t.Fatalf("fixed-risk-at-stop risk at stop exceeded: %v > %v", atStop, account*fixed.RiskAtStop)
						}

						// ADR 0003's identity, re-asserted across the whole
						// grid rather than at one point: at Stop Multiple 2
						// the two modes agree when the risk fraction is
						// twice the Unit Volatility Fraction.
						if stopMultiple == 2 {
							equivalent, err := sizing.SizeUnit(sizing.Inputs{
								Mode:                   sizing.ModeFixedRiskAtStop,
								NotionalAccount:        account,
								UnitVolatilityFraction: fraction,
								StopMultiple:           2,
								RiskAtStopFraction:     fraction * 2,
								N:                      n,
								DollarsPerPoint:        dollarsPerPoint,
							})
							if err != nil {
								t.Fatalf("SizeUnit(fixed-risk-at-stop equivalent) error = %v", err)
							}
							if equivalent.Quantity != volatilityNormalised.Quantity {
								t.Fatalf("ADR 0003 identity broken at account=%v fraction=%v n=%v dollarsPerPoint=%v: %d != %d",
									account, fraction, n, dollarsPerPoint, equivalent.Quantity, volatilityNormalised.Quantity)
							}
						}
					}
				}
			}
		}
	}

	if combinations != len(accounts)*len(fractions)*len(nValues)*len(stopMultiples)*len(dollarsPerPoints) {
		t.Fatalf("grid walked %d combinations, want %d", combinations,
			len(accounts)*len(fractions)*len(nValues)*len(stopMultiples)*len(dollarsPerPoints))
	}
}
