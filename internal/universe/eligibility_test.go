package universe_test

import (
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/indicator"
	"github.com/richard-whittemore/TrendInvesting/internal/universe"
)

// baselineCriteria mirrors ADR 0009's declared Baseline thresholds: at
// least $5, at least $5,000,000 of 20-day median dollar volume, and at
// least 250 completed bars of history.
func baselineCriteria() universe.Criteria {
	return universe.Criteria{
		MinPrice:        5,
		MinDollarVolume: 5_000_000,
		MinHistoryBars:  250,
	}
}

// fullWindow returns indicator.DollarVolumeWindow raw closes and volumes
// whose product is exactly dollarVolume at every bar, so the window is
// ready and its median is dollarVolume without a caller having to reason
// about medianOf's own tie-breaking.
func fullWindow(dollarVolume float64) (closes, volumes []float64) {
	closes = make([]float64, indicator.DollarVolumeWindow)
	volumes = make([]float64, indicator.DollarVolumeWindow)
	for i := range closes {
		closes[i] = 1
		volumes[i] = dollarVolume
	}
	return closes, volumes
}

// eligibleInput is a fully-qualifying instrument: common stock on a US
// primary exchange, priced and voluminous well past the Baseline's floors,
// with ample history. Each test below excludes exactly one criterion from
// this starting point.
func eligibleInput() universe.Input {
	closes, volumes := fullWindow(10_000_000)
	return universe.Input{
		Classification:      universe.Classification{SecurityType: universe.SecurityTypeCommonStock, USPrimaryExchange: true},
		Price:               50,
		DollarVolumeCloses:  closes,
		DollarVolumeVolumes: volumes,
		CompletedBars:       300,
	}
}

func TestEvaluateEligibleInstrumentPassesEveryCriterion(t *testing.T) {
	t.Parallel()
	result := universe.Evaluate(eligibleInput(), baselineCriteria())
	if !result.Eligible {
		t.Fatalf("Evaluate(eligibleInput()) = %+v, want Eligible", result)
	}
	if !result.ClassificationEligible || !result.PriceEligible || !result.DollarVolumeEligible || !result.HistoryEligible {
		t.Fatalf("Evaluate(eligibleInput()) = %+v, want every criterion eligible", result)
	}
}

// TestEvaluateExcludesEachCriterionInTurn covers the ticket's own list: an
// ETF, an ADR, and a SPAC each fail the classification criterion; a foreign
// primary exchange fails it too; a price and a dollar volume below the
// Baseline's floors fail their own criteria; and thin history fails the
// history criterion. Every other criterion in each case still passes, so
// Result reports exactly one failing criterion, never a false accusation
// from a criterion this input actually satisfies.
func TestEvaluateExcludesEachCriterionInTurn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		modify      func(universe.Input) universe.Input
		wantFailing string // which *Eligible field must be false
	}{
		{
			name: "etf",
			modify: func(in universe.Input) universe.Input {
				in.Classification = universe.Classification{SecurityType: universe.SecurityTypeETF, USPrimaryExchange: true}
				return in
			},
			wantFailing: "classification",
		},
		{
			name: "adr",
			modify: func(in universe.Input) universe.Input {
				in.Classification = universe.Classification{SecurityType: universe.SecurityTypeADR, USPrimaryExchange: true}
				return in
			},
			wantFailing: "classification",
		},
		{
			name: "spac",
			modify: func(in universe.Input) universe.Input {
				in.Classification = universe.Classification{SecurityType: universe.SecurityTypeSPAC, USPrimaryExchange: true}
				return in
			},
			wantFailing: "classification",
		},
		{
			name: "common stock on a non-US primary exchange",
			modify: func(in universe.Input) universe.Input {
				in.Classification = universe.Classification{SecurityType: universe.SecurityTypeCommonStock, USPrimaryExchange: false}
				return in
			},
			wantFailing: "classification",
		},
		{
			name: "never classified at all",
			modify: func(in universe.Input) universe.Input {
				in.Classification = universe.Classification{}
				return in
			},
			wantFailing: "classification",
		},
		{
			name: "price below the floor",
			modify: func(in universe.Input) universe.Input {
				in.Price = 4.99
				return in
			},
			wantFailing: "price",
		},
		{
			name: "dollar volume below the floor",
			modify: func(in universe.Input) universe.Input {
				closes, volumes := fullWindow(4_999_999)
				in.DollarVolumeCloses, in.DollarVolumeVolumes = closes, volumes
				return in
			},
			wantFailing: "dollarVolume",
		},
		{
			name: "fewer than 250 completed bars",
			modify: func(in universe.Input) universe.Input {
				in.CompletedBars = 249
				return in
			},
			wantFailing: "history",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := universe.Evaluate(tc.modify(eligibleInput()), baselineCriteria())
			if result.Eligible {
				t.Fatalf("Evaluate(...) = %+v, want ineligible", result)
			}
			got := map[string]bool{
				"classification": result.ClassificationEligible,
				"price":          result.PriceEligible,
				"dollarVolume":   result.DollarVolumeEligible,
				"history":        result.HistoryEligible,
			}
			for name, eligible := range got {
				want := name != tc.wantFailing
				if eligible != want {
					t.Errorf("criterion %q eligible = %v, want %v (only %q should fail)", name, eligible, want, tc.wantFailing)
				}
			}
		})
	}
}

// TestEvaluatePriceBoundaryIsInclusive covers the ticket's own "exactly $5"
// case: ADR 0009 says "at least $5", so a price of exactly the floor passes,
// and one tick below it fails.
func TestEvaluatePriceBoundaryIsInclusive(t *testing.T) {
	t.Parallel()
	in := eligibleInput()

	in.Price = 5
	if result := universe.Evaluate(in, baselineCriteria()); !result.PriceEligible || !result.Eligible {
		t.Fatalf("price exactly at the $5 floor: PriceEligible = %v, Eligible = %v, want both true", result.PriceEligible, result.Eligible)
	}

	in.Price = 4.999999
	if result := universe.Evaluate(in, baselineCriteria()); result.PriceEligible || result.Eligible {
		t.Fatalf("price just below the $5 floor: PriceEligible = %v, Eligible = %v, want both false", result.PriceEligible, result.Eligible)
	}
}

// TestEvaluateDollarVolumeBoundaryIsInclusive covers the ticket's own
// "exactly $5M" case.
func TestEvaluateDollarVolumeBoundaryIsInclusive(t *testing.T) {
	t.Parallel()
	in := eligibleInput()

	closes, volumes := fullWindow(5_000_000)
	in.DollarVolumeCloses, in.DollarVolumeVolumes = closes, volumes
	result := universe.Evaluate(in, baselineCriteria())
	if !result.DollarVolumeEligible || !result.Eligible {
		t.Fatalf("dollar volume exactly at the $5,000,000 floor: DollarVolumeEligible = %v, Eligible = %v, want both true", result.DollarVolumeEligible, result.Eligible)
	}

	closes, volumes = fullWindow(4_999_999.99)
	in.DollarVolumeCloses, in.DollarVolumeVolumes = closes, volumes
	result = universe.Evaluate(in, baselineCriteria())
	if result.DollarVolumeEligible || result.Eligible {
		t.Fatalf("dollar volume just below the $5,000,000 floor: DollarVolumeEligible = %v, Eligible = %v, want both false", result.DollarVolumeEligible, result.Eligible)
	}
}

// TestEvaluateDollarVolumeWindowNotReadyFailsEvenAboveThreshold covers a
// window with fewer than indicator.DollarVolumeWindow completed Sessions:
// indicator.MedianDollarVolume reports not ready, and DollarVolumeEligible
// must be false regardless of what the partial window's own product would
// suggest.
func TestEvaluateDollarVolumeWindowNotReadyFailsEvenAboveThreshold(t *testing.T) {
	t.Parallel()
	in := eligibleInput()
	in.DollarVolumeCloses = []float64{100}
	in.DollarVolumeVolumes = []float64{1_000_000}
	result := universe.Evaluate(in, baselineCriteria())
	if result.DollarVolumeReady {
		t.Fatalf("Evaluate with a %d-long window: DollarVolumeReady = true, want false", len(in.DollarVolumeCloses))
	}
	if result.DollarVolumeEligible || result.Eligible {
		t.Fatalf("Evaluate with an unready dollar-volume window: DollarVolumeEligible = %v, Eligible = %v, want both false", result.DollarVolumeEligible, result.Eligible)
	}
}

// TestEvaluateHistoryBoundaryIsInclusive: exactly MinHistoryBars passes.
func TestEvaluateHistoryBoundaryIsInclusive(t *testing.T) {
	t.Parallel()
	in := eligibleInput()
	in.CompletedBars = 250
	if result := universe.Evaluate(in, baselineCriteria()); !result.HistoryEligible || !result.Eligible {
		t.Fatalf("completed bars exactly at the 250-bar floor: HistoryEligible = %v, Eligible = %v, want both true", result.HistoryEligible, result.Eligible)
	}
}

// TestFirstOfMonthWithNoPreviousSessionIsTrue: a run's very first Session is
// vacuously the first trading day of its month.
func TestFirstOfMonthWithNoPreviousSessionIsTrue(t *testing.T) {
	t.Parallel()
	periodEnd := time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC)
	if !universe.FirstOfMonth(periodEnd, false, time.Time{}) {
		t.Fatal("FirstOfMonth with no previous Session = false, want true")
	}
}

// TestFirstOfMonthDetectsTheMonthBoundary covers the ticket's own "a month
// boundary" case: the last trading day of February and the first of March
// (skipping the weekend) must be told apart correctly, and two Sessions in
// the same month must not be.
func TestFirstOfMonthDetectsTheMonthBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		previous  time.Time
		current   time.Time
		wantFirst bool
	}{
		{
			name:      "same month, later day",
			previous:  time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC),
			current:   time.Date(2024, time.March, 4, 0, 0, 0, 0, time.UTC),
			wantFirst: false,
		},
		{
			name:      "february's last trading day into march's first",
			previous:  time.Date(2024, time.February, 29, 0, 0, 0, 0, time.UTC),
			current:   time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC),
			wantFirst: true,
		},
		{
			name:      "december into january, crossing a year boundary",
			previous:  time.Date(2023, time.December, 29, 0, 0, 0, 0, time.UTC),
			current:   time.Date(2024, time.January, 2, 0, 0, 0, 0, time.UTC),
			wantFirst: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := universe.FirstOfMonth(tc.current, true, tc.previous); got != tc.wantFirst {
				t.Errorf("FirstOfMonth(%s, true, %s) = %v, want %v", tc.current, tc.previous, got, tc.wantFirst)
			}
		})
	}
}
