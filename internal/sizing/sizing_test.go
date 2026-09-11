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
		{"faith's fraction on equities", 1_000_000, 0.01, 37.57779214788228, 1},
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
	if fixedAtThree >= fixedAtTwo {
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
	if atThree <= atTwo {
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
		if want := 16 * (2 * 0.0141 * 42_000) / 1_000_000; unit.RealisedRiskAtStop != want {
			t.Errorf("RealisedRiskAtStop = %v, want %v", unit.RealisedRiskAtStop, want)
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
		if want := 166 * (3 * 2.0 * 1) / 100_000; unit.RealisedRiskAtStop != want {
			t.Errorf("RealisedRiskAtStop = %v, want %v", unit.RealisedRiskAtStop, want)
		}
	})
}

// TestSizeUnitRealisedRiskAtStopShowsWhatTruncationLeftBehind covers the
// distinction a reviewer of PR #64 asked for.
//
// RiskAtStop is the *declared budget* — the parameter the Sizing Mode is
// keyed to, and under volatility-normalised it must stay exactly Unit
// Volatility Fraction x Stop Multiple (ADR 0003; #10's acceptance criteria).
// It is what the strategy set out to risk. RealisedRiskAtStop is what the
// whole-share quantity that came out of the truncation actually risks. The
// gap between the two is the truncation, and it always points the same way:
// a truncated position risks LESS than the budget, never more.
//
// Faith's Heating Oil Unit makes the gap visible. The budget is 2 % (1 % per
// N with a 2N stop, The Turtle Rules p.14 and p.22), but the truncation from
// 16.88 to 16 contracts leaves the realised figure at
// 16 x (2 x 0.0141 x 42,000) / 1,000,000 = 1.895 %. A journal that recorded
// only the budget would overstate what this Unit stands to lose by roughly
// five per cent of the figure.
func TestSizeUnitRealisedRiskAtStopShowsWhatTruncationLeftBehind(t *testing.T) {
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

	if unit.RiskAtStop != 0.02 {
		t.Errorf("RiskAtStop = %v, want exactly 0.02: the declared budget must not be adjusted for truncation (ADR 0003)", unit.RiskAtStop)
	}
	// 16 contracts x (2 x 0.0141 x 42,000) = 18,950.4, against a 1,000,000
	// notional account.
	if want := 16 * (2 * 0.0141 * 42_000) / 1_000_000; unit.RealisedRiskAtStop != want {
		t.Errorf("RealisedRiskAtStop = %v, want %v", unit.RealisedRiskAtStop, want)
	}
	if !(unit.RealisedRiskAtStop < unit.RiskAtStop) {
		t.Errorf("RealisedRiskAtStop %v is not below the declared RiskAtStop %v; truncation must leave a gap here, not close it",
			unit.RealisedRiskAtStop, unit.RiskAtStop)
	}

	// An exactly-dividing case has no truncation, so the two coincide: the
	// gap is the truncation and nothing else. 100,000 x 0.005 = 500;
	// 500 / 2.5 = 200 exactly; 200 x (2 x 2.5 x 1) / 100,000 = 0.01 = the
	// declared 0.005 x 2.
	exact, err := sizing.SizeUnit(sizing.Inputs{
		Mode:                   sizing.ModeVolatilityNormalised,
		NotionalAccount:        100_000,
		UnitVolatilityFraction: 0.005,
		StopMultiple:           2,
		N:                      2.5,
		DollarsPerPoint:        1,
	})
	if err != nil {
		t.Fatalf("SizeUnit(exact) error = %v", err)
	}
	if exact.Quantity != 200 {
		t.Fatalf("Quantity = %d, want 200", exact.Quantity)
	}
	if exact.RealisedRiskAtStop != exact.RiskAtStop {
		t.Errorf("RealisedRiskAtStop = %v, RiskAtStop = %v; with nothing truncated away the two must coincide",
			exact.RealisedRiskAtStop, exact.RiskAtStop)
	}
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
			if unit.Quantity != 0 || unit.RiskAtStop != 0 || unit.RealisedRiskAtStop != 0 {
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
//     Its per-account form is RealisedRiskAtStop <= RiskAtStop, which is
//     asserted alongside it: the realised figure is exactly that product
//     divided by the Notional Account, and it may never exceed the declared
//     budget in either mode.
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
	nValues := []float64{0.0141, 0.25, 1, 2.5, 3, 37.57779214788228, 137.5}
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
						if want := float64(volatilityNormalised.Quantity) * (stopMultiple * n * dollarsPerPoint) / account; volatilityNormalised.RealisedRiskAtStop != want {
							t.Fatalf("RealisedRiskAtStop = %v, want exactly %v", volatilityNormalised.RealisedRiskAtStop, want)
						}
						if volatilityNormalised.RealisedRiskAtStop > volatilityNormalised.RiskAtStop {
							t.Fatalf("realised risk %v exceeds the declared budget %v at account=%v fraction=%v n=%v stopMultiple=%v dollarsPerPoint=%v: truncation may only ever risk less",
								volatilityNormalised.RealisedRiskAtStop, volatilityNormalised.RiskAtStop, account, fraction, n, stopMultiple, dollarsPerPoint)
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
						if want := float64(fixed.Quantity) * (stopMultiple * n * dollarsPerPoint) / account; fixed.RealisedRiskAtStop != want {
							t.Fatalf("RealisedRiskAtStop = %v, want exactly %v", fixed.RealisedRiskAtStop, want)
						}
						if fixed.RealisedRiskAtStop > fixed.RiskAtStop {
							t.Fatalf("realised risk %v exceeds the declared budget %v at account=%v fraction=%v n=%v stopMultiple=%v dollarsPerPoint=%v",
								fixed.RealisedRiskAtStop, fixed.RiskAtStop, account, fraction, n, stopMultiple, dollarsPerPoint)
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

// TestDrawdownSteppedNotionalFaithsLadder is the direct, in-package test for
// #16's shared derivation: internal/strategy.NotionalAccount.Observe and
// event.DrawdownStepAppliedPayload.Validate both call this function so a
// Drawdown Step's before/after figures are computed identically by producer
// and validator (see the function's own doc comment). Faith's ladder (The
// Turtle Rules p.17): $1,000,000 -> $800,000 -> $640,000, plus the derived
// third step, $512,000.
func TestDrawdownSteppedNotionalFaithsLadder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		before float64
		want   float64
	}{
		{before: 1_000_000, want: 800_000},
		{before: 800_000, want: 640_000},
		{before: 640_000, want: 512_000}, // derived, not printed in the source
	}
	for _, tt := range tests {
		if got := sizing.DrawdownSteppedNotional(tt.before); got != tt.want {
			t.Errorf("DrawdownSteppedNotional(%v) = %v, want %v", tt.before, got, tt.want)
		}
	}
}

// TestCashMovementScaledFigureDepositExample is #17's hand-derived deposit
// fixture: after one Drawdown Step at equity 890,000 (yearly starting figure
// S 1,000,000, measurement base B 900,000, Notional Account A 800,000), a
// deposit of 200,000 scales every figure by (890,000+200,000)/890,000. The
// expected values are pinned by math.Float64bits, found by running the
// computation once (the same discipline #16's asymptote test and #10's
// truncation-boundary test use for a figure not practical to hand-derive to
// the last bit), not re-derived from sizing.CashMovementScaledFigure itself
// — that would make the assertion tautological.
func TestCashMovementScaledFigureDepositExample(t *testing.T) {
	t.Parallel()

	const (
		equityBefore = 890_000.0
		amount       = 200_000.0
		equityAfter  = equityBefore + amount // 1,090,000

		startingFigureBefore = 1_000_000.0
		baseBefore           = 900_000.0
		notionalBefore       = 800_000.0
	)

	tests := []struct {
		name   string
		before float64
		want   float64
		bits   uint64
	}{
		{name: "starting figure", before: startingFigureBefore, want: 1_224_719.1011235956, bits: 0x4132b00f19e33c68},
		{name: "measurement base", before: baseBefore, want: 1_102_247.191011236, bits: 0x4130d1a730e61cc4},
		{name: "notional account", before: notionalBefore, want: 979_775.2808988765, bits: 0x412de67e8fd1fa40},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sizing.CashMovementScaledFigure(tt.before, equityBefore, equityAfter)
			if got != tt.want {
				t.Fatalf("CashMovementScaledFigure(%v, %v, %v) = %v, want %v", tt.before, equityBefore, equityAfter, got, tt.want)
			}
			if gotBits := math.Float64bits(got); gotBits != tt.bits {
				t.Fatalf("CashMovementScaledFigure(%v, %v, %v) bits = %#x, want %#x", tt.before, equityBefore, equityAfter, gotBits, tt.bits)
			}
		})
	}
}

// TestCashMovementScaledFigureWithdrawalExample is the symmetric withdrawal
// fixture: a withdrawal of 100,000 from the same starting state as the
// deposit example above, scaling by (890,000-100,000)/890,000.
func TestCashMovementScaledFigureWithdrawalExample(t *testing.T) {
	t.Parallel()

	const (
		equityBefore = 890_000.0
		amount       = -100_000.0
		equityAfter  = equityBefore + amount // 790,000

		startingFigureBefore = 1_000_000.0
		baseBefore           = 900_000.0
		notionalBefore       = 800_000.0
	)

	tests := []struct {
		name   string
		before float64
		want   float64
		bits   uint64
	}{
		{name: "starting figure", before: startingFigureBefore, want: 887_640.4494382022, bits: 0x412b16b0e61cc398},
		{name: "measurement base", before: baseBefore, want: 798_876.404494382, bits: 0x41286138cf19e33c},
		{name: "notional account", before: notionalBefore, want: 710_112.3595505618, bits: 0x4125abc0b81702e0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sizing.CashMovementScaledFigure(tt.before, equityBefore, equityAfter)
			if got != tt.want {
				t.Fatalf("CashMovementScaledFigure(%v, %v, %v) = %v, want %v", tt.before, equityBefore, equityAfter, got, tt.want)
			}
			if gotBits := math.Float64bits(got); gotBits != tt.bits {
				t.Fatalf("CashMovementScaledFigure(%v, %v, %v) bits = %#x, want %#x", tt.before, equityBefore, equityAfter, gotBits, tt.bits)
			}
		})
	}
}
