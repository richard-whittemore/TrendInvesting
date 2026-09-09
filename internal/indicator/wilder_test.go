package indicator_test

import (
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/indicator"
)

// heatingOilRow is one row of Faith's Heating Oil (HO03H) table, The Turtle
// Rules p.14-15, prices in dollars per gallon. This is a Golden Scenario
// (CONTEXT.md): transcribed, not invented.
//
// The table starts mid-series: row 0's TR of 0.0096 already implies a
// previous close not printed here, and row 0's N of 0.0134 already implies a
// 20-day simple-average seed computed on bars not printed here. Row 0 is
// therefore usable as a *previous* row for row 1 (its High/Low/Close/TR/N
// are all given), but it cannot itself be recomputed from this table alone.
var heatingOilTable = []struct {
	date             string
	high, low, close float64
	tr, n            float64
}{
	{"2002-11-01", 0.7220, 0.7124, 0.7124, 0.0096, 0.0134},
	{"2002-11-04", 0.7170, 0.7073, 0.7073, 0.0097, 0.0132},
	{"2002-11-05", 0.7099, 0.6923, 0.6923, 0.0176, 0.0134},
	{"2002-11-06", 0.6930, 0.6800, 0.6838, 0.0130, 0.0134},
	{"2002-11-07", 0.6960, 0.6736, 0.6736, 0.0224, 0.0139},
	{"2002-11-08", 0.6820, 0.6706, 0.6706, 0.0114, 0.0137},
	{"2002-11-11", 0.6820, 0.6710, 0.6710, 0.0114, 0.0136},
	{"2002-11-12", 0.6795, 0.6720, 0.6744, 0.0085, 0.0134},
	{"2002-11-13", 0.6760, 0.6550, 0.6616, 0.0210, 0.0138},
	{"2002-11-14", 0.6650, 0.6585, 0.6627, 0.0065, 0.0134},
	{"2002-11-15", 0.6701, 0.6620, 0.6701, 0.0081, 0.0131},
	{"2002-11-18", 0.6965, 0.6750, 0.6965, 0.0264, 0.0138},
	{"2002-11-19", 0.7065, 0.6944, 0.6944, 0.0121, 0.0137},
	{"2002-11-20", 0.7115, 0.6944, 0.7087, 0.0171, 0.0139},
	{"2002-11-21", 0.7168, 0.7100, 0.7124, 0.0081, 0.0136},
	{"2002-11-22", 0.7265, 0.7120, 0.7265, 0.0145, 0.0136},
	{"2002-11-25", 0.7265, 0.7098, 0.7098, 0.0167, 0.0138},
	{"2002-11-26", 0.7184, 0.7110, 0.7184, 0.0086, 0.0135},
	{"2002-11-27", 0.7280, 0.7200, 0.7228, 0.0096, 0.0133},
	{"2002-12-02", 0.7375, 0.7227, 0.7359, 0.0148, 0.0134},
	{"2002-12-03", 0.7447, 0.7310, 0.7389, 0.0137, 0.0134},
	// #10's golden sizing example (The Turtle Rules p.15) uses N=0.0141 from
	// this row; the assertions below pin that this package produces it.
	{"2002-12-04", 0.7420, 0.7140, 0.7162, 0.0280, 0.0141},
}

// round4 rounds to four decimal places, matching the precision Faith's table
// is printed at. The small epsilon guards against a value landing exactly on
// a .00005 boundary due to binary floating-point representation.
func round4(v float64) float64 {
	return math.Round(v*10000+1e-9) / 10000
}

func TestHeatingOilTrueRangeMatchesTableToFourDecimals(t *testing.T) {
	t.Parallel()

	for i := 1; i < len(heatingOilTable); i++ {
		row := heatingOilTable[i]
		previousClose := heatingOilTable[i-1].close
		t.Run(row.date, func(t *testing.T) {
			t.Parallel()
			got := round4(indicator.TrueRange(row.high, row.low, previousClose, true))
			if got != row.tr {
				t.Fatalf("TrueRange() = %v, want %v (The Turtle Rules p.14-15, Heating Oil %s)", got, row.tr, row.date)
			}
		})
	}
}

// TestHeatingOilWilderStepMatchesTable checks the recursion formula itself,
// one step at a time, using the table's own previous-row N and the table's
// own TR at each step: this isolates the formula from any accumulation of
// rounding error across multiple steps, since every input to WilderNext here
// is read straight from the printed table.
//
// Two rows are known to differ from the printed table by one unit in the
// fourth decimal even under this "clean inputs" test: 2002-11-08 and
// 2002-11-12. Faith's table is itself printed already rounded to four
// decimals, and a single Wilder step computed from those rounded inputs does
// not always reproduce his next rounded output — this is a property of
// re-deriving from a rounded published table, not a defect in WilderNext.
// The exact drifted values are pinned here (rather than loosening the
// tolerance) so a future change to the formula is still caught.
func TestHeatingOilWilderStepMatchesTable(t *testing.T) {
	t.Parallel()

	knownDrift := map[string]float64{
		"2002-11-08": 0.0138, // table prints 0.0137
		"2002-11-12": 0.0133, // table prints 0.0134
	}

	for i := 1; i < len(heatingOilTable); i++ {
		row := heatingOilTable[i]
		previousN := heatingOilTable[i-1].n
		want := row.n
		if drifted, ok := knownDrift[row.date]; ok {
			want = drifted
		}
		t.Run(row.date, func(t *testing.T) {
			t.Parallel()
			got := round4(indicator.WilderNext(previousN, row.tr, indicator.DefaultPeriod))
			if got != want {
				t.Fatalf("WilderNext(%v, %v, 20) rounded = %v, want %v (The Turtle Rules p.14-15, Heating Oil %s)", previousN, row.tr, got, want, row.date)
			}
		})
	}
}

// TestHeatingOilWilderChainedFromRowOneMatchesTable runs the recursion
// chained forward from row 0's N, carrying full (unrounded) precision
// between steps and rounding only for the final comparison against each
// printed row. TR at each step is recomputed from High/Low and the previous
// row's Close, at full precision, rather than read from the table.
//
// One row is known to drift by one unit in the fourth decimal even here:
// 2002-11-13. This is accumulated-rounding drift against a table that is
// itself printed rounded (see TestHeatingOilWilderStepMatchesTable), pinned
// rather than papered over with a looser tolerance.
func TestHeatingOilWilderChainedFromRowOneMatchesTable(t *testing.T) {
	t.Parallel()

	knownDrift := map[string]float64{
		"2002-11-13": 0.0137, // table prints 0.0138
	}

	n := heatingOilTable[0].n
	for i := 1; i < len(heatingOilTable); i++ {
		row := heatingOilTable[i]
		previousClose := heatingOilTable[i-1].close
		tr := indicator.TrueRange(row.high, row.low, previousClose, true)
		n = indicator.WilderNext(n, tr, indicator.DefaultPeriod)

		want := row.n
		if drifted, ok := knownDrift[row.date]; ok {
			want = drifted
		}
		t.Run(row.date, func(t *testing.T) {
			got := round4(n)
			if got != want {
				t.Fatalf("chained N rounded = %v, want %v (The Turtle Rules p.14-15, Heating Oil %s)", got, want, row.date)
			}
		})
	}
}

// TestHeatingOilFinalRowFeedsTicket10 pins the exact acceptance requirement
// from #8's ticket body: N on 2002-12-04 must be 0.0141 so #10's sizing
// golden (16 contracts at $1,000,000 and 42,000 dollars per point) can build
// on it.
func TestHeatingOilFinalRowFeedsTicket10(t *testing.T) {
	t.Parallel()

	last := heatingOilTable[len(heatingOilTable)-1]
	if last.date != "2002-12-04" || last.n != 0.0141 {
		t.Fatalf("fixture's final row = %s N=%v, want 2002-12-04 N=0.0141", last.date, last.n)
	}
}

// syntheticTrueRanges is a hand-computable 25-value True Range series used
// to test the seed and the first few recursion steps independently of the
// Heating Oil table, which starts mid-series and so cannot exercise the
// seed itself (see the package doc comment on heatingOilTable). The first
// 20 values are 1..20 (sum 210, so the seed is exactly 210/20 = 10.5); the
// remaining 5 are a constant 5, chosen so every subsequent step is
// hand-computable by long division by 20.
var syntheticTrueRanges = func() []float64 {
	trs := make([]float64, 0, 25)
	for i := 1; i <= 20; i++ {
		trs = append(trs, float64(i))
	}
	for i := 0; i < 5; i++ {
		trs = append(trs, 5)
	}
	return trs
}()

func TestSMASeedOfSyntheticSeries(t *testing.T) {
	t.Parallel()

	got := indicator.SMASeed(syntheticTrueRanges[:20])
	want := 10.5 // sum(1..20)=210, 210/20=10.5
	if got != want {
		t.Fatalf("SMASeed() = %v, want %v", got, want)
	}
}

// TestWilderAverageSyntheticSeriesSeedAndFirstSteps hand-verifies the seed
// and the first five post-seed recursion steps:
//
//	N20 (seed)             = 210/20             = 10.5
//	N21 = (19*10.5+5)/20   = 204.5/20            = 10.225
//	N22 = (19*10.225+5)/20 = 199.275/20          = 9.96375
//	N23 = (19*9.96375+5)/20 = 194.31125/20       = 9.7155625
//	N24 = (19*9.7155625+5)/20 = 189.5956875/20   = 9.479784375
//	N25 = (19*9.479784375+5)/20 = 185.115903125/20 = 9.25579515625
func TestWilderAverageSyntheticSeriesSeedAndFirstSteps(t *testing.T) {
	t.Parallel()

	want := []float64{
		10.5,
		10.225,
		9.96375,
		9.7155625,
		9.479784375,
		9.25579515625,
	}

	avg, err := indicator.NewWilderAverage(indicator.DefaultPeriod)
	if err != nil {
		t.Fatalf("NewWilderAverage() error = %v", err)
	}

	const epsilon = 1e-9
	var got []float64
	for i, tr := range syntheticTrueRanges {
		avg.Add(tr)
		if i+1 == 19 && avg.Ready() {
			t.Fatalf("Ready() = true after 19 bars, want false")
		}
		if i+1 >= 20 {
			got = append(got, avg.Value())
		}
	}
	if !avg.Ready() {
		t.Fatal("Ready() = false after 25 bars, want true")
	}
	if len(got) != len(want) {
		t.Fatalf("got %d values, want %d", len(got), len(want))
	}
	for i := range want {
		if diff := math.Abs(got[i] - want[i]); diff > epsilon {
			t.Errorf("N after bar %d = %v, want %v (diff %v)", 20+i, got[i], want[i], diff)
		}
	}
}

func TestWilderAverageReadyAtExactlyPeriodBars(t *testing.T) {
	t.Parallel()

	avg, err := indicator.NewWilderAverage(indicator.DefaultPeriod)
	if err != nil {
		t.Fatalf("NewWilderAverage() error = %v", err)
	}

	for i := 0; i < indicator.DefaultPeriod-1; i++ {
		avg.Add(1.0)
		if avg.Ready() {
			t.Fatalf("Ready() = true after %d bars, want false (period is %d)", i+1, indicator.DefaultPeriod)
		}
	}

	avg.Add(1.0)
	if !avg.Ready() {
		t.Fatalf("Ready() = false after %d bars, want true", indicator.DefaultPeriod)
	}
}

func TestWilderAverageValueBeforeReadyIsZero(t *testing.T) {
	t.Parallel()

	avg, err := indicator.NewWilderAverage(indicator.DefaultPeriod)
	if err != nil {
		t.Fatalf("NewWilderAverage() error = %v", err)
	}
	avg.Add(999.0)
	if got := avg.Value(); got != 0 {
		t.Fatalf("Value() before Ready = %v, want 0", got)
	}
}

func TestNewWilderAverageRejectsNonPositivePeriod(t *testing.T) {
	t.Parallel()

	for _, period := range []int{0, -1, -20} {
		if _, err := indicator.NewWilderAverage(period); err == nil {
			t.Fatalf("NewWilderAverage(%d) error = nil, want error", period)
		}
	}
}

// TestSimpleMovingAverageOfTrueRangeDoesNotMatchWilder is the negative case
// AGENTS.md and the ticket both call out by name: a plain simple moving
// average of True Range is not N, and must not coincide with it. The SMA is
// computed independently here, in the test, deliberately not by calling any
// production helper for it (none exists, and none should).
func TestSimpleMovingAverageOfTrueRangeDoesNotMatchWilder(t *testing.T) {
	t.Parallel()

	avg, err := indicator.NewWilderAverage(indicator.DefaultPeriod)
	if err != nil {
		t.Fatalf("NewWilderAverage() error = %v", err)
	}
	for _, tr := range syntheticTrueRanges {
		avg.Add(tr)
	}
	wilderN := avg.Value()

	// Plain rolling SMA of the last 20 True Range values (bars 6..25).
	window := syntheticTrueRanges[len(syntheticTrueRanges)-20:]
	var sum float64
	for _, tr := range window {
		sum += tr
	}
	sma := sum / 20

	if wilderN == sma {
		t.Fatalf("Wilder N (%v) unexpectedly equals a plain 20-bar SMA of True Range (%v); they must diverge", wilderN, sma)
	}
	// Pin the actual values so this negative test cannot be satisfied by
	// coincidence if the arithmetic changes.
	if round4(wilderN) != 9.2558 {
		t.Fatalf("Wilder N = %v, want ~9.2558 (see TestWilderAverageSyntheticSeriesSeedAndFirstSteps)", wilderN)
	}
	if sma != 11.0 {
		t.Fatalf("plain SMA = %v, want 11.0 (sum(6..20)+5*5=220, /20=11.0)", sma)
	}
}
