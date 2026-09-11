package sizing_test

import (
	"math"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// TestNextAddLevelGoldGolden is #14's required golden, transcribed from The
// Turtle Rules p.20 (AGENTS.md rule 1: "transcribe, don't invent"):
//
//	First Unit added  310.00
//	Second Unit       310.00 + 1/2 x 2.50 = 311.25
//	Third Unit        311.25 + 1/2 x 2.50 = 312.50
//	Fourth Unit       312.50 + 1/2 x 2.50 = 313.75
//
// Each rung is measured from float64 VARIABLES, in the exact expression
// order NextAddLevel documents (previousFill + 0.5*campaignN), never from an
// untyped constant literal — the same discipline
// TestProtectiveStopLevelCrudeGolden records, since Go folds untyped
// constant arithmetic in arbitrary precision and rounds once at the end.
func TestNextAddLevelGoldGolden(t *testing.T) {
	t.Parallel()

	campaignN := 2.50
	firstFill := 310.00

	second, err := sizing.NextAddLevel(firstFill, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(unit 2) error = %v", err)
	}
	if want := firstFill + 0.5*campaignN; second != want {
		t.Errorf("second unit = %v, want exactly %v (The Turtle Rules p.20)", second, want)
	}

	third, err := sizing.NextAddLevel(second, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(unit 3) error = %v", err)
	}
	if want := second + 0.5*campaignN; third != want {
		t.Errorf("third unit = %v, want exactly %v (The Turtle Rules p.20)", third, want)
	}

	fourth, err := sizing.NextAddLevel(third, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(unit 4) error = %v", err)
	}
	if want := third + 0.5*campaignN; fourth != want {
		t.Errorf("fourth unit = %v, want exactly %v (The Turtle Rules p.20)", fourth, want)
	}

	// The printed decimals, to the precision Faith states them, confirm this
	// is genuinely her example and not just an internally consistent
	// arithmetic chain.
	for i, tt := range []struct {
		got, want float64
	}{
		{second, 311.25},
		{third, 312.50},
		{fourth, 313.75},
	} {
		if math.Abs(tt.got-tt.want) > 1e-9 {
			t.Errorf("unit %d = %v, want approximately %v (The Turtle Rules p.20, Gold ladder)", i+2, tt.got, tt.want)
		}
	}
}

// TestNextAddLevelCrudeGolden is #14's other required golden, The Turtle
// Rules p.20:
//
//	First Unit   28.30
//	Second Unit  28.30 + 1/2 x 1.20 = 28.90
//	Third Unit   28.90 + 1/2 x 1.20 = 29.50
//	Fourth Unit  29.50 + 1/2 x 1.20 = 30.10
func TestNextAddLevelCrudeGolden(t *testing.T) {
	t.Parallel()

	campaignN := 1.20
	firstFill := 28.30

	second, err := sizing.NextAddLevel(firstFill, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(unit 2) error = %v", err)
	}
	third, err := sizing.NextAddLevel(second, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(unit 3) error = %v", err)
	}
	fourth, err := sizing.NextAddLevel(third, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(unit 4) error = %v", err)
	}

	for i, tt := range []struct {
		got, want float64
	}{
		{second, 28.90},
		{third, 29.50},
		{fourth, 30.10},
	} {
		if math.Abs(tt.got-tt.want) > 1e-9 {
			t.Errorf("unit %d = %v, want approximately %v (The Turtle Rules p.20, Crude ladder)", i+2, tt.got, tt.want)
		}
	}
}

// TestNextAddLevelSlippedFirstFillShiftsEveryLaterRung is the ticket's named
// negative: a first fill slipped by 0.06 from the Crude golden's 28.30
// (28.36) must shift EVERY later rung by exactly the same 0.06, since each
// rung is measured from the actual fill before it, never from the intended
// level (The Turtle Rules p.19: "slippage on the first fill pushes later
// adds out accordingly").
func TestNextAddLevelSlippedFirstFillShiftsEveryLaterRung(t *testing.T) {
	t.Parallel()

	campaignN := 1.20
	const slip = 0.06
	exactFirst := 28.30
	slippedFirst := exactFirst + slip

	exactSecond, err := sizing.NextAddLevel(exactFirst, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(exact) error = %v", err)
	}
	exactThird, err := sizing.NextAddLevel(exactSecond, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(exact) error = %v", err)
	}
	exactFourth, err := sizing.NextAddLevel(exactThird, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(exact) error = %v", err)
	}

	slippedSecond, err := sizing.NextAddLevel(slippedFirst, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(slipped) error = %v", err)
	}
	slippedThird, err := sizing.NextAddLevel(slippedSecond, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(slipped) error = %v", err)
	}
	slippedFourth, err := sizing.NextAddLevel(slippedThird, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(slipped) error = %v", err)
	}

	for i, tt := range []struct {
		exact, slipped float64
	}{
		{exactSecond, slippedSecond},
		{exactThird, slippedThird},
		{exactFourth, slippedFourth},
	} {
		got := tt.slipped - tt.exact
		if math.Abs(got-slip) > 1e-9 {
			t.Errorf("unit %d: slipped - exact = %v, want exactly the %v slip carried forward unchanged", i+2, got, slip)
		}
	}
}

// TestNextAddLevelFailsClosed covers every non-finite and non-positive
// input .greptile/rules.md requires to fail closed.
func TestNextAddLevelFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		previousFill float64
		campaignN    float64
		direction    string
		wantErr      string
	}{
		{name: "zero previous fill", previousFill: 0, campaignN: 1, direction: sizing.DirectionLong, wantErr: "entry price must be positive"},
		{name: "negative previous fill", previousFill: -1, campaignN: 1, direction: sizing.DirectionLong, wantErr: "entry price must be positive"},
		{name: "non-finite previous fill", previousFill: math.NaN(), campaignN: 1, direction: sizing.DirectionLong, wantErr: "entry price must be finite"},
		{name: "zero campaign n", previousFill: 100, campaignN: 0, direction: sizing.DirectionLong, wantErr: "n must be positive"},
		{name: "negative campaign n", previousFill: 100, campaignN: -1, direction: sizing.DirectionLong, wantErr: "n must be positive"},
		{name: "non-finite campaign n", previousFill: 100, campaignN: math.Inf(1), direction: sizing.DirectionLong, wantErr: "n must be finite"},
		{name: "unrecognised direction", previousFill: 100, campaignN: 5, direction: "short", wantErr: "not implemented"},
		{name: "missing direction", previousFill: 100, campaignN: 5, direction: "", wantErr: "not implemented"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := sizing.NextAddLevel(tt.previousFill, tt.campaignN, tt.direction)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("NextAddLevel() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// TestAddLadderGoldGolden covers AddLadder against the same Gold example,
// confirming the whole-ladder helper agrees with the rung-by-rung
// NextAddLevel calls above.
func TestAddLadderGoldGolden(t *testing.T) {
	t.Parallel()

	campaignN := 2.50
	firstFill := 310.00

	ladder, err := sizing.AddLadder(firstFill, campaignN, 4, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("AddLadder() error = %v", err)
	}
	if len(ladder) != 4 {
		t.Fatalf("len(ladder) = %d, want 4", len(ladder))
	}
	want := []float64{310.00, 311.25, 312.50, 313.75}
	for i := range want {
		if math.Abs(ladder[i]-want[i]) > 1e-9 {
			t.Errorf("ladder[%d] = %v, want approximately %v (The Turtle Rules p.20, Gold ladder)", i, ladder[i], want[i])
		}
	}
	if ladder[0] != firstFill {
		t.Errorf("ladder[0] = %v, want exactly the first fill %v", ladder[0], firstFill)
	}

	// Every rung after the first must agree bit for bit with the same chain
	// built one NextAddLevel call at a time, since AddLadder documents itself
	// as making the identical exact-fill assumption.
	rung := firstFill
	for i := 1; i < len(ladder); i++ {
		next, err := sizing.NextAddLevel(rung, campaignN, sizing.DirectionLong)
		if err != nil {
			t.Fatalf("NextAddLevel() error = %v", err)
		}
		if ladder[i] != next {
			t.Errorf("ladder[%d] = %v, want exactly NextAddLevel's %v", i, ladder[i], next)
		}
		rung = next
	}
}

// TestAddLadderCrudeGolden is the Crude counterpart, The Turtle Rules p.20.
func TestAddLadderCrudeGolden(t *testing.T) {
	t.Parallel()

	ladder, err := sizing.AddLadder(28.30, 1.20, 4, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("AddLadder() error = %v", err)
	}
	want := []float64{28.30, 28.90, 29.50, 30.10}
	if len(ladder) != len(want) {
		t.Fatalf("len(ladder) = %d, want %d", len(ladder), len(want))
	}
	for i := range want {
		if math.Abs(ladder[i]-want[i]) > 1e-9 {
			t.Errorf("ladder[%d] = %v, want approximately %v (The Turtle Rules p.20, Crude ladder)", i, ladder[i], want[i])
		}
	}
}

// TestAddLadderFailsClosed covers AddLadder's own guards, including maxUnits
// below 1 (NextAddLevel has no equivalent parameter to guard).
func TestAddLadderFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		firstFill float64
		campaignN float64
		maxUnits  int
		direction string
		wantErr   string
	}{
		{name: "zero first fill", firstFill: 0, campaignN: 1, maxUnits: 4, direction: sizing.DirectionLong, wantErr: "entry price must be positive"},
		{name: "non-finite campaign n", firstFill: 100, campaignN: math.NaN(), maxUnits: 4, direction: sizing.DirectionLong, wantErr: "n must be finite"},
		{name: "unrecognised direction", firstFill: 100, campaignN: 5, maxUnits: 4, direction: "short", wantErr: "not implemented"},
		{name: "zero max units", firstFill: 100, campaignN: 5, maxUnits: 0, direction: sizing.DirectionLong, wantErr: "max units"},
		{name: "negative max units", firstFill: 100, campaignN: 5, maxUnits: -1, direction: sizing.DirectionLong, wantErr: "max units"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := sizing.AddLadder(tt.firstFill, tt.campaignN, tt.maxUnits, tt.direction)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("AddLadder() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// TestAddLadderSingleUnit covers the degenerate maxUnits==1 case: just the
// first fill, no rungs computed at all.
func TestAddLadderSingleUnit(t *testing.T) {
	t.Parallel()

	ladder, err := sizing.AddLadder(100, 5, 1, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("AddLadder() error = %v", err)
	}
	if len(ladder) != 1 || ladder[0] != 100 {
		t.Fatalf("AddLadder() = %v, want [100]", ladder)
	}
}
