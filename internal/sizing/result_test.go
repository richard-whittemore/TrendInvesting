package sizing_test

import (
	"math"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// TestAverageMoveInNCrudeGolden is the single-Unit case, where
// AverageMoveInN and RealisedResultInUnitN must coincide (PR #74 review
// response): entry 28.30, exit 31.30, N 1.20 -> a move of 3.00, or 2.5N.
func TestAverageMoveInNCrudeGolden(t *testing.T) {
	t.Parallel()

	entry := 28.30
	exit := 31.30
	n := 1.20

	got, err := sizing.AverageMoveInN(exit, entry, n)
	if err != nil {
		t.Fatalf("AverageMoveInN() error = %v", err)
	}
	want := (exit - entry) / n
	if got != want {
		t.Fatalf("AverageMoveInN() = %v, want exactly %v", got, want)
	}
	if math.Abs(got-2.5) > 1e-9 {
		t.Errorf("AverageMoveInN() = %v, want approximately 2.5", got)
	}
}

// TestRealisedResultInUnitNCoincidesWithAverageMoveInNForASingleFullUnit
// pins the coordinator's stated invariant: for a single Unit filled in
// full (quantity == unitQuantity), RealisedResultInUnitN must equal
// AverageMoveInN exactly.
func TestRealisedResultInUnitNCoincidesWithAverageMoveInNForASingleFullUnit(t *testing.T) {
	t.Parallel()

	entry := 28.30
	exit := 31.30
	n := 1.20
	dpp := 1.0
	var unitQuantity int64 = 133

	realisedResult := float64(unitQuantity) * (exit - entry) * dpp

	avgMove, err := sizing.AverageMoveInN(exit, entry, n)
	if err != nil {
		t.Fatalf("AverageMoveInN() error = %v", err)
	}
	resultInUnitN, err := sizing.RealisedResultInUnitN(realisedResult, unitQuantity, n, dpp)
	if err != nil {
		t.Fatalf("RealisedResultInUnitN() error = %v", err)
	}
	if resultInUnitN != avgMove {
		t.Errorf("RealisedResultInUnitN() = %v, want exactly AverageMoveInN() = %v for a single fully-filled unit", resultInUnitN, avgMove)
	}
}

// TestRealisedResultInUnitNFourUnitCrudeLadderGolden is the coordinator's
// hand-derived four-Unit case: Faith's Crude ladder (28.30, 28.90, 29.50,
// 30.10, N = 1.20), each Unit 133 shares, exited at 31.30 -> per-Unit moves
// 3.00, 2.40, 1.80, 1.20, which at N 1.20 are 2.5 + 2.0 + 1.5 + 1.0 = 7.0
// Unit-N.
func TestRealisedResultInUnitNFourUnitCrudeLadderGolden(t *testing.T) {
	t.Parallel()

	n := 1.20
	dpp := 1.0
	var unitQuantity int64 = 133
	exit := 31.30
	fills := []float64{28.30, 28.90, 29.50, 30.10}

	var realisedResult float64
	var wantUnitN float64
	for _, fill := range fills {
		realisedResult += float64(unitQuantity) * (exit - fill) * dpp
		wantUnitN += (exit - fill) / n
	}
	if math.Abs(wantUnitN-7.0) > 1e-9 {
		t.Fatalf("hand-derived unit-n total = %v, want approximately 7.0 (fixture error, not the function under test)", wantUnitN)
	}

	got, err := sizing.RealisedResultInUnitN(realisedResult, unitQuantity, n, dpp)
	if err != nil {
		t.Fatalf("RealisedResultInUnitN() error = %v", err)
	}
	if math.Abs(got-wantUnitN) > 1e-9 {
		t.Errorf("RealisedResultInUnitN() = %v, want approximately %v (2.5 + 2.0 + 1.5 + 1.0)", got, wantUnitN)
	}
}

func TestAverageMoveInNFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		exit      float64
		entry     float64
		campaignN float64
		wantErr   string
	}{
		{name: "non-finite exit price", exit: math.NaN(), entry: 100, campaignN: 5, wantErr: "exit price must be finite"},
		{name: "non-finite entry price", exit: 100, entry: math.Inf(1), campaignN: 5, wantErr: "entry price must be finite"},
		{name: "zero campaign n", exit: 100, entry: 90, campaignN: 0, wantErr: "n must be positive"},
		{name: "negative campaign n", exit: 100, entry: 90, campaignN: -1, wantErr: "n must be positive"},
		{name: "non-finite campaign n", exit: 100, entry: 90, campaignN: math.NaN(), wantErr: "n must be finite"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := sizing.AverageMoveInN(tt.exit, tt.entry, tt.campaignN)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("AverageMoveInN() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestRealisedResultInUnitNFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		realisedResult  float64
		unitQuantity    int64
		campaignN       float64
		dollarsPerPoint float64
		wantErr         string
	}{
		{name: "non-finite realised result", realisedResult: math.NaN(), unitQuantity: 133, campaignN: 1.2, dollarsPerPoint: 1, wantErr: "realised result must be finite"},
		{name: "zero unit quantity", realisedResult: 100, unitQuantity: 0, campaignN: 1.2, dollarsPerPoint: 1, wantErr: "unit quantity"},
		{name: "negative unit quantity", realisedResult: 100, unitQuantity: -1, campaignN: 1.2, dollarsPerPoint: 1, wantErr: "unit quantity"},
		{name: "zero campaign n", realisedResult: 100, unitQuantity: 133, campaignN: 0, dollarsPerPoint: 1, wantErr: "n must be positive"},
		{name: "zero dollars per point", realisedResult: 100, unitQuantity: 133, campaignN: 1.2, dollarsPerPoint: 0, wantErr: "dollars per point must be positive"},
		{name: "non-finite dollars per point", realisedResult: 100, unitQuantity: 133, campaignN: 1.2, dollarsPerPoint: math.Inf(1), wantErr: "dollars per point must be finite"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := sizing.RealisedResultInUnitN(tt.realisedResult, tt.unitQuantity, tt.campaignN, tt.dollarsPerPoint)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("RealisedResultInUnitN() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}
