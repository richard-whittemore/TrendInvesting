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
