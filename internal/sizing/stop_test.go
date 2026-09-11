package sizing_test

import (
	"math"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// TestProtectiveStopLevelCrudeGolden is #12's required golden: Faith's own
// worked single-Unit Crude example, transcribed rather than invented
// (AGENTS.md rule 1).
//
// The Turtle Rules p.22: entry 28.30, N 1.20, the Baseline's Stop Multiple of
// 2 (ADR 0003) -> a Protective Stop of 25.90.
//
// The want value is derived from float64 VARIABLES, in the exact expression
// order ProtectiveStopLevel documents (entryPrice - stopMultiple*campaignN),
// deliberately NOT the untyped constant literal 25.90. Go evaluates untyped
// constant arithmetic in arbitrary precision and rounds once at the end, so
// "28.30 - 2*1.20" as a constant differs in the last bit from the identical
// expression evaluated at run time in float64 (28.30-2*1.20 = 25.900000000000002,
// not the nearest float64 to the decimal 25.90 — they happen to be the same
// float64 here, but the fixture must not depend on that coincidence, which is
// why exact bit equality is asserted against the run-time derivation, not the
// literal). This is the same discipline #10 recorded for the fixed-risk-at-stop
// proposal fixture after its first draft silently tested constant folding
// instead of the producer's arithmetic.
func TestProtectiveStopLevelCrudeGolden(t *testing.T) {
	t.Parallel()

	entryPrice := 28.30
	campaignN := 1.20
	stopMultiple := 2.0
	want := entryPrice - stopMultiple*campaignN

	got, err := sizing.ProtectiveStopLevel(entryPrice, campaignN, stopMultiple, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("ProtectiveStopLevel() error = %v, want nil", err)
	}
	if got != want {
		t.Fatalf("ProtectiveStopLevel() = %v (bits %x), want exactly %v (bits %x) (The Turtle Rules p.22)",
			got, math.Float64bits(got), want, math.Float64bits(want))
	}
	// Pinned so a future change to the expression order or to float64
	// rounding behaviour is caught even if `want` above were ever computed
	// differently by accident: 28.30 - 2*1.20 is 25.900000000000002 in
	// float64, one ULP above the decimal 25.90.
	const wantBits = 0x4039e66666666667
	if math.Float64bits(got) != wantBits {
		t.Errorf("ProtectiveStopLevel() bits = %x, want %x (25.900000000000002, The Turtle Rules p.22's 25.90)", math.Float64bits(got), wantBits)
	}
}

// TestProtectiveStopLevelHandComputedCases covers wider stops and non-round
// numbers, each hand-derived in its own comment.
func TestProtectiveStopLevelHandComputedCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		entryPrice   float64
		campaignN    float64
		stopMultiple float64
	}{
		{
			// A Stop Multiple of 3 widens the stop from the same entry and N:
			// 100 - 3*5 = 85, further from entry than the Baseline's 2N (90).
			name:         "stop multiple 3 gives a wider stop than the baseline's 2",
			entryPrice:   100,
			campaignN:    5,
			stopMultiple: 3,
		},
		{
			name:         "small n, tight stop",
			entryPrice:   50,
			campaignN:    0.25,
			stopMultiple: 2,
		},
		{
			name:         "large equity price",
			entryPrice:   4321.55,
			campaignN:    62.125,
			stopMultiple: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			want := tt.entryPrice - tt.stopMultiple*tt.campaignN
			got, err := sizing.ProtectiveStopLevel(tt.entryPrice, tt.campaignN, tt.stopMultiple, sizing.DirectionLong)
			if err != nil {
				t.Fatalf("ProtectiveStopLevel() error = %v, want nil", err)
			}
			if got != want {
				t.Errorf("ProtectiveStopLevel() = %v, want exactly %v", got, want)
			}
		})
	}
}

// TestProtectiveStopLevelWiderStopIsFartherFromEntry pins the direction of
// ADR 0003's Stop Multiple: from the SAME entry and N, Stop Multiple 3 must
// sit farther below the entry than Stop Multiple 2.
func TestProtectiveStopLevelWiderStopIsFartherFromEntry(t *testing.T) {
	t.Parallel()

	baseline, err := sizing.ProtectiveStopLevel(100, 5, 2, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("ProtectiveStopLevel(stop multiple 2) error = %v", err)
	}
	wider, err := sizing.ProtectiveStopLevel(100, 5, 3, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("ProtectiveStopLevel(stop multiple 3) error = %v", err)
	}
	if !(wider < baseline) {
		t.Errorf("stop multiple 3 gave %v, want it below stop multiple 2's %v (a wider stop multiple sits farther from entry)", wider, baseline)
	}
}

// TestProtectiveStopLevelFailsClosed covers every non-finite and non-positive
// input .greptile/rules.md requires to fail closed, plus the derived-result
// guard: a long equity cannot be stopped out at or below zero.
func TestProtectiveStopLevelFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		entryPrice   float64
		campaignN    float64
		stopMultiple float64
		direction    string
		wantErr      string
	}{
		{
			name:         "zero entry price",
			entryPrice:   0,
			campaignN:    1,
			stopMultiple: 2,
			direction:    sizing.DirectionLong,
			wantErr:      "entry price must be positive",
		},
		{
			name:         "negative entry price",
			entryPrice:   -1,
			campaignN:    1,
			stopMultiple: 2,
			direction:    sizing.DirectionLong,
			wantErr:      "entry price must be positive",
		},
		{
			name:         "non-finite entry price",
			entryPrice:   math.NaN(),
			campaignN:    1,
			stopMultiple: 2,
			direction:    sizing.DirectionLong,
			wantErr:      "entry price must be finite",
		},
		{
			name:         "zero campaign n",
			entryPrice:   100,
			campaignN:    0,
			stopMultiple: 2,
			direction:    sizing.DirectionLong,
			wantErr:      "n must be positive",
		},
		{
			name:         "negative campaign n",
			entryPrice:   100,
			campaignN:    -1,
			stopMultiple: 2,
			direction:    sizing.DirectionLong,
			wantErr:      "n must be positive",
		},
		{
			name:         "non-finite campaign n",
			entryPrice:   100,
			campaignN:    math.Inf(1),
			stopMultiple: 2,
			direction:    sizing.DirectionLong,
			wantErr:      "n must be finite",
		},
		{
			name:         "zero stop multiple",
			entryPrice:   100,
			campaignN:    5,
			stopMultiple: 0,
			direction:    sizing.DirectionLong,
			wantErr:      "stop multiple must be positive",
		},
		{
			name:         "negative stop multiple",
			entryPrice:   100,
			campaignN:    5,
			stopMultiple: -2,
			direction:    sizing.DirectionLong,
			wantErr:      "stop multiple must be positive",
		},
		{
			name:         "non-finite stop multiple",
			entryPrice:   100,
			campaignN:    5,
			stopMultiple: math.NaN(),
			direction:    sizing.DirectionLong,
			wantErr:      "stop multiple must be finite",
		},
		{
			name:         "unrecognised direction",
			entryPrice:   100,
			campaignN:    5,
			stopMultiple: 2,
			direction:    "short",
			wantErr:      "not implemented",
		},
		{
			name:         "missing direction",
			entryPrice:   100,
			campaignN:    5,
			stopMultiple: 2,
			direction:    "",
			wantErr:      "not implemented",
		},
		{
			// A stop multiple wide enough against a large N puts the derived
			// level at or below zero: a long equity cannot be stopped out
			// there, so the Unit would in fact risk the whole position. The
			// same rule #10's DeclineReasonStopIntentNotPositive encodes for
			// the proposal's stop intent.
			name:         "derived level at or below zero",
			entryPrice:   100,
			campaignN:    60,
			stopMultiple: 2,
			direction:    sizing.DirectionLong,
			wantErr:      "not positive",
		},
		{
			name:         "derived level exactly zero",
			entryPrice:   100,
			campaignN:    50,
			stopMultiple: 2,
			direction:    sizing.DirectionLong,
			wantErr:      "not positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := sizing.ProtectiveStopLevel(tt.entryPrice, tt.campaignN, tt.stopMultiple, tt.direction)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ProtectiveStopLevel() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// TestProtectiveStopLevelDirectionLongMatchesTheEventContract pins the one
// seam where this package's own Direction vocabulary could drift from
// internal/event's: internal/sizing deliberately imports nothing from
// internal/event (the same rule internal/indicator follows, and the same
// reasoning TestSizingModeConstantsMatchTheEventContract in internal/strategy
// records for sizing.Mode), so it declares its own DirectionLong constant.
// internal/strategy.TestSizingDirectionConstantsMatchTheEventContract pins the
// string values equal from the other side.
func TestProtectiveStopLevelDirectionLongMatchesTheEventContract(t *testing.T) {
	t.Parallel()

	if sizing.DirectionLong != "long" {
		t.Errorf("sizing.DirectionLong = %q, want %q", sizing.DirectionLong, "long")
	}
}

// --- #15: the Stop Ladder's arithmetic seam --------------------------------
//
// The Turtle Rules p.22-23, Crude Oil, N = 1.20, Stop Multiple 2, transcribed
// exactly (see internal/strategy/campaign.go's applyAddFill and
// docs referencing this ticket for the citation):
//
//	One Unit:    First 28.30  Stop 25.90
//	Two Units:   First 28.30  Stop 26.50   Second 28.90  Stop 26.50
//	Three Units: First 28.30  Stop 27.10   Second 28.90  Stop 27.10   Third 29.50  Stop 27.10
//	Four Units:  First 28.30  Stop 27.70   Second 28.90  Stop 27.70   Third 29.50  Stop 27.70   Fourth 30.10 Stop 27.70
//	Gap case:    First 28.30  Stop 27.70   Second 28.90  Stop 27.70   Third 29.50  Stop 27.70   Fourth 30.80 Stop 28.40

// Deliberately `var`, not `const`: an expression built only from untyped
// constants is evaluated by the Go compiler in arbitrary precision and
// rounded once at the end, which can differ in the last bit from the
// IDENTICAL expression evaluated at run time in float64 (the same
// constant-folding-vs-runtime-float64 discipline
// TestProtectiveStopLevelCrudeGolden's own doc comment states, and #10's
// original finding). Declaring these as package-level variables forces
// every arithmetic expression built from them to run at float64 runtime
// precision, matching the production code's own arithmetic exactly.
var (
	crudeN            = 1.20
	crudeStopMultiple = 2.0
	crudeFirstFill    = 28.30
	crudeSecondFill   = 28.90
	crudeThirdFill    = 29.50
	crudeFourthFill   = 30.10
	// crudeGapFourthFill is p.23's gap case: the fourth Unit fills much
	// farther from the third than the standard 1/2N rung, because the
	// market opened gapping up.
	crudeGapFourthFill = 30.80
)

// crudeStop derives one Unit's own initial stop via sizing.ProtectiveStopLevel
// rather than hand-typing it, so a fixture can never silently drift from the
// production arithmetic it exercises (the same discipline add_test.go's
// house style already applies to the Add Ladder).
func crudeStop(t *testing.T, fill float64) float64 {
	t.Helper()
	stop, err := sizing.ProtectiveStopLevel(fill, crudeN, crudeStopMultiple, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("ProtectiveStopLevel(%v) error = %v", fill, err)
	}
	return stop
}

// crudeEpsilon bounds the comparisons below against the printed decimal
// table values: RaisedStop applied repeatedly (a chain of independent
// float64 additions, each rounding once) is not expected to be BIT-identical
// to a single closed-form expression computing the same nominal decimal —
// two mathematically-equal quantities reached by different float64
// operation sequences may differ by a handful of ULPs, which the Crude
// table's two-decimal-place printing cannot see and this fixture must not
// mistake for a defect. Exact float64 equality is still used, elsewhere in
// this package, wherever two call sites are expected to run the IDENTICAL
// expression (ProtectiveStopLevel's own golden, or two calls of the SAME
// function on the SAME inputs); this is not that case.
const crudeEpsilon = 1e-9

// TestRaisedStopReproducesFaithsCrudeTablesRowByRow is the arithmetic-seam
// golden: all five printed tables (The Turtle Rules p.22-23), reproduced
// from RaisedStop applied to each earlier Unit once per Add, and
// ProtectiveStopLevel for each Unit's own initial stop.
//
// The gap-case row (crudeGapFourthFill) is the row that distinguishes the
// two readings RaisedStop's own doc comment names: "raise the earlier
// Units' stops by 1/2N" (this function) keeps Units one through three at
// 27.70 even though the fourth Unit fills at 30.80 rather than 30.10 — the
// OTHER reading, "set every stop to 2N below the newest fill", would put
// Units one through three at 30.80-2.40=28.40 instead, contradicting the
// printed table.
func TestRaisedStopReproducesFaithsCrudeTablesRowByRow(t *testing.T) {
	t.Parallel()

	// One Unit: just the initial stop, no raise yet.
	unit1Stop := crudeStop(t, crudeFirstFill)
	if math.Abs(unit1Stop-25.90) > crudeEpsilon {
		t.Fatalf("one-unit stop = %v, want ~25.90 (The Turtle Rules p.22)", unit1Stop)
	}

	// Two Units: the second Unit adds, raising Unit 1 by 1/2N; Unit 2's own
	// initial stop lands at the SAME nominal level (the tables coincide in
	// the non-gap case).
	unit1StopAtTwo, err := sizing.RaisedStop(unit1Stop, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit2Stop := crudeStop(t, crudeSecondFill)
	for name, got := range map[string]float64{"unit 1 (raised)": unit1StopAtTwo, "unit 2 (own)": unit2Stop} {
		if math.Abs(got-26.50) > crudeEpsilon {
			t.Errorf("two-unit stop (%s) = %v, want ~26.50 (The Turtle Rules p.22)", name, got)
		}
	}

	// Three Units: Units 1 and 2 each raise again; Unit 3's own initial stop
	// again lands at the same nominal level.
	unit1StopAtThree, err := sizing.RaisedStop(unit1StopAtTwo, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit2StopAtThree, err := sizing.RaisedStop(unit2Stop, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit3Stop := crudeStop(t, crudeThirdFill)
	for name, got := range map[string]float64{
		"unit 1": unit1StopAtThree,
		"unit 2": unit2StopAtThree,
		"unit 3": unit3Stop,
	} {
		if math.Abs(got-27.10) > crudeEpsilon {
			t.Errorf("three-unit stop (%s) = %v, want ~27.10 (The Turtle Rules p.22)", name, got)
		}
	}

	// Four Units, normal case: every earlier Unit raises once more; Unit 4's
	// own initial stop lands at the same nominal level, 27.70 — matching the
	// ticket's hand-derived per-unit risks 0.60/1.20/1.80/2.40 (1/2N, 1N,
	// 3/2N, 2N).
	unit1StopAtFour, err := sizing.RaisedStop(unit1StopAtThree, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit2StopAtFour, err := sizing.RaisedStop(unit2StopAtThree, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit3StopAtFour, err := sizing.RaisedStop(unit3Stop, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit4Stop := crudeStop(t, crudeFourthFill)
	for name, got := range map[string]float64{
		"unit 1": unit1StopAtFour,
		"unit 2": unit2StopAtFour,
		"unit 3": unit3StopAtFour,
		"unit 4": unit4Stop,
	} {
		if math.Abs(got-27.70) > crudeEpsilon {
			t.Errorf("four-unit stop (%s) = %v, want ~27.70 (The Turtle Rules p.22)", name, got)
		}
	}
	if got, want := unit1StopAtFour-unit1Stop, 3*0.5*crudeN; math.Abs(got-want) > crudeEpsilon {
		t.Errorf("unit 1's total rise = %v, want %v (three raises of 1/2N each)", got, want)
	}

	// --- The gap case (p.23): the fourth Unit fills at 30.80, not 30.10 ----
	//
	// Units 1-3 still only rise by the STANDARD 1/2N each — identical to the
	// normal four-unit case above, computed from the identical (unraised)
	// three-unit levels, NEVER re-derived from the fourth Unit's own
	// (gapped) fill. Unit 4's OWN stop is computed independently, from its
	// own actual fill and the frozen N: this is what makes it diverge.
	gapUnit1Stop, err := sizing.RaisedStop(unit1StopAtThree, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	gapUnit2Stop, err := sizing.RaisedStop(unit2StopAtThree, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	gapUnit3Stop, err := sizing.RaisedStop(unit3Stop, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	gapUnit4Stop := crudeStop(t, crudeGapFourthFill)

	for name, got := range map[string]float64{"unit 1": gapUnit1Stop, "unit 2": gapUnit2Stop, "unit 3": gapUnit3Stop} {
		if math.Abs(got-27.70) > crudeEpsilon {
			t.Errorf("gap case stop (%s) = %v, want ~27.70 (The Turtle Rules p.23: earlier units still raise by only 1/2N)", name, got)
		}
	}
	if math.Abs(gapUnit4Stop-28.40) > crudeEpsilon {
		t.Errorf("gap case stop (unit 4, own) = %v, want ~28.40 (The Turtle Rules p.23: 30.80 - 2x1.20 = 28.40)", gapUnit4Stop)
	}

	// The headline negative: the OTHER reading ("set every stop to 2N below
	// the newest fill") would put units 1-3 at 30.80-2.40=28.40 instead of
	// 27.70 — a full 0.70 away from this fixture's 27.70, far outside any
	// float64 rounding tolerance. Assert the two readings genuinely differ,
	// so this test would fail under that wrong implementation.
	wrongReadingStop, err := sizing.ProtectiveStopLevel(crudeGapFourthFill, crudeN, crudeStopMultiple, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("ProtectiveStopLevel() error = %v", err)
	}
	if math.Abs(gapUnit1Stop-wrongReadingStop) < 0.1 {
		t.Fatalf("gap case unit 1 stop %v is too close to the WRONG reading's %v; this fixture must keep the two clearly distinguishable", gapUnit1Stop, wrongReadingStop)
	}
}

// TestRaisedStopHandComputedCases covers the arithmetic in isolation, away
// from the Crude fixture.
func TestRaisedStopHandComputedCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		previousStop float64
		campaignN    float64
	}{
		{name: "round numbers", previousStop: 100, campaignN: 4},
		{name: "small n", previousStop: 50, campaignN: 0.25},
		{name: "large equity price", previousStop: 4321.55, campaignN: 62.125},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			want := tt.previousStop + 0.5*tt.campaignN
			got, err := sizing.RaisedStop(tt.previousStop, tt.campaignN)
			if err != nil {
				t.Fatalf("RaisedStop() error = %v, want nil", err)
			}
			if got != want {
				t.Errorf("RaisedStop() = %v, want exactly %v", got, want)
			}
			if !(got > tt.previousStop) {
				t.Errorf("RaisedStop() = %v, want it strictly ABOVE the previous stop %v: the baseline's stop ladder only ever raises a long campaign's stop", got, tt.previousStop)
			}
		})
	}
}

// TestRaisedStopFailsClosed covers every non-finite and non-positive input
// .greptile/rules.md requires to fail closed.
func TestRaisedStopFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		previousStop float64
		campaignN    float64
		wantErr      string
	}{
		{name: "zero previous stop", previousStop: 0, campaignN: 1, wantErr: "previous stop must be positive"},
		{name: "negative previous stop", previousStop: -1, campaignN: 1, wantErr: "previous stop must be positive"},
		{name: "non-finite previous stop", previousStop: math.NaN(), campaignN: 1, wantErr: "previous stop must be finite"},
		{name: "zero campaign n", previousStop: 100, campaignN: 0, wantErr: "n must be positive"},
		{name: "negative campaign n", previousStop: 100, campaignN: -1, wantErr: "n must be positive"},
		{name: "non-finite campaign n", previousStop: 100, campaignN: math.Inf(1), wantErr: "n must be finite"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := sizing.RaisedStop(tt.previousStop, tt.campaignN)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("RaisedStop() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}
