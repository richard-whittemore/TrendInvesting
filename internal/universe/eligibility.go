package universe

import (
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/indicator"
)

// Criteria carries ADR 0009's Baseline universe thresholds. Every threshold
// is a parameter, tested at other values as a declared Variant (ADR 0009's
// own Consequences: "the thresholds are parameters and are
// sensitivity-tested") — never a constant a caller would have to edit code
// to change.
type Criteria struct {
	// MinPrice is ADR 0009's price floor: at least $5 in the Baseline, read
	// from the raw price view (ADR 0004, as amended: "absolute price floors
	// in universe eligibility" are one of the quantities that need the raw
	// view, since a split-adjusted level is not invariant under the
	// adjustment factor).
	MinPrice float64
	// MinDollarVolume is ADR 0009's liquidity floor: at least $5,000,000 of
	// 20-day median dollar volume in the Baseline, the same raw-view measure
	// ADR 0010's ranking tie-break reads (indicator.MedianDollarVolume).
	MinDollarVolume float64
	// MinHistoryBars is ADR 0009's history floor: at least 250 completed
	// bars in the Baseline.
	MinHistoryBars int
}

// Input is one instrument's point-in-time inputs to ADR 0009's eligibility
// test, as of one monthly evaluation.
type Input struct {
	// Classification is the instrument's declared security type and primary
	// exchange (Port.Classify). Its zero value fails the classification
	// criterion by construction (Classification's own doc comment), which is
	// how an instrument this run's Port has never classified is evaluated:
	// ineligible on that criterion alone, never a separate "unknown" case.
	Classification Classification
	// Price is the instrument's raw close on the evaluation date (ADR 0004,
	// as amended): the same day's own completed bar once it has closed,
	// mirroring ADR 0010's ranking tie-break, which reads the decision
	// Session's own close (see this package's own doc comment on "which
	// day").
	Price float64
	// DollarVolumeCloses and DollarVolumeVolumes are the raw closes and
	// volumes indicator.MedianDollarVolume reads: the same length, in
	// chronological order (oldest first), fed in lockstep. Only the last
	// indicator.DollarVolumeWindow elements matter; fewer than that reports
	// not ready (see MedianDollarVolume's own doc comment).
	DollarVolumeCloses  []float64
	DollarVolumeVolumes []float64
	// CompletedBars is the total number of completed bars this run has ever
	// accepted for the instrument, up to and including the evaluation date.
	CompletedBars int
}

// Result records ADR 0009's eligibility decision for one instrument, and the
// individual criterion the overall Eligible value was decided from — so an
// emitted eligibility event can show a reviewer exactly which criterion
// excluded an instrument, not only the outcome.
type Result struct {
	Eligible bool

	ClassificationEligible bool

	Price         float64
	PriceEligible bool

	DollarVolume         float64
	DollarVolumeReady    bool
	DollarVolumeEligible bool

	CompletedBars   int
	HistoryEligible bool
}

// Evaluate applies ADR 0009's Baseline universe test to input under
// criteria: common stock on a US primary exchange (no ETFs, ADRs, or
// SPACs), price at least MinPrice, 20-day median dollar volume at least
// MinDollarVolume, and at least MinHistoryBars completed bars of history.
// Every criterion is evaluated independently and reported in Result, so a
// caller can see which one excluded the instrument even when more than one
// does.
//
// "At least" is inclusive (ADR 0009's own wording): a price or dollar volume
// exactly at its threshold passes.
//
// DollarVolumeEligible is false whenever the window is not yet ready
// (fewer than indicator.DollarVolumeWindow completed Sessions), regardless
// of the dollar volume the ready portion would compute — an instrument with
// fewer than DollarVolumeWindow bars also fails HistoryEligible whenever
// MinHistoryBars is at least DollarVolumeWindow (the Baseline's 250 comfortably
// is), so this case is never eligibility's only reason, but it is reported
// honestly regardless of how MinHistoryBars happens to be configured.
func Evaluate(input Input, criteria Criteria) Result {
	dollarVolume, ready := indicator.MedianDollarVolume(input.DollarVolumeCloses, input.DollarVolumeVolumes)

	result := Result{
		ClassificationEligible: input.Classification.CommonStockOnUSPrimaryExchange(),
		Price:                  input.Price,
		PriceEligible:          input.Price >= criteria.MinPrice,
		DollarVolume:           dollarVolume,
		DollarVolumeReady:      ready,
		DollarVolumeEligible:   ready && dollarVolume >= criteria.MinDollarVolume,
		CompletedBars:          input.CompletedBars,
		HistoryEligible:        input.CompletedBars >= criteria.MinHistoryBars,
	}
	result.Eligible = result.ClassificationEligible && result.PriceEligible &&
		result.DollarVolumeEligible && result.HistoryEligible
	return result
}

// FirstOfMonth reports whether periodEnd is the first trading day of its
// calendar month among the Sessions the strategy has closed so far (ADR
// 0009: eligibility is "re-evaluated point-in-time on the first trading day
// of each month"). hasPrevious and previousPeriodEnd describe the
// immediately preceding closed Session; Sessions follow one another
// strictly (ADR 0021), so comparing consecutive Sessions is sufficient — no
// Session's period end can be skipped without an earlier one recording it.
//
// No previous Session at all (hasPrevious false) reports true: the first
// Session a run ever closes is, vacuously, the first trading day of its
// month, since no earlier Session that month exists to have already
// evaluated it.
func FirstOfMonth(periodEnd time.Time, hasPrevious bool, previousPeriodEnd time.Time) bool {
	if !hasPrevious {
		return true
	}
	previousYear, previousMonth, _ := previousPeriodEnd.Date()
	year, month, _ := periodEnd.Date()
	return year != previousYear || month != previousMonth
}
