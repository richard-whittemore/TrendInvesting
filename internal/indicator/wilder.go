package indicator

import "fmt"

// DefaultPeriod is the lookback length for N, counted in completed bars
// (CONTEXT.md: "Completed bar"), never calendar days.
//
// The Turtle Rules p.13: "N = (19 x PDN + TR) / 20 ... seeded with a 20-day
// simple average of TR" — Faith's period is 20. It is exposed here as a
// named default rather than baked into WilderNext or NewWilderAverage so
// that a period other than 20 (a declared Variant, never a silent
// substitution) is a parameter, not a rewrite.
const DefaultPeriod = 20

// SMASeed returns the simple average of values: the seed for the Wilder
// recursion (The Turtle Rules p.13: "seeded with a 20-day simple average of
// True Range"). The caller supplies exactly the first Period True Range
// values of the series, in bar order.
func SMASeed(values []float64) float64 {
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

// WilderNext applies one step of the Wilder recursion to advance N by one
// completed bar.
//
// The Turtle Rules p.13: N = (19 x previousN + TR) / 20. That printed
// formula is the period-20 instance of the general Wilder recursion
// N = ((period-1) x previousN + TR) / period; WilderNext takes period as a
// parameter so a declared Variant can use a different length without
// duplicating this formula, while DefaultPeriod (20) reproduces Faith's
// printed constant exactly.
func WilderNext(previousN, tr float64, period int) float64 {
	return (float64(period-1)*previousN + tr) / float64(period)
}

// WilderAverage computes N — the Wilder-smoothed average of True Range — one
// completed bar at a time, warmed up in bar count rather than calendar time
// (CONTEXT.md: "Completed bar"; "N").
//
// The Turtle Rules p.13: N is seeded with a simple average of the first
// Period True Range values (SMASeed), then updated by the Wilder recursion
// (WilderNext) from the (Period+1)th completed bar onward.
//
// Ready reports only whether bar-count warm-up is complete — whether at
// least Period values have been added — not whether Value is a *usable*
// volatility reading. Value before Ready is the zero value (0), which must
// never be used for a decision. But Value can also legitimately be exactly 0
// once Ready is true: Period flat bars (high==low==close, True Range 0 every
// time) produce a seed of 0, and the Wilder recursion carries a 0 forward
// until a non-zero True Range arrives. A genuine N is never negative, but it
// can be zero. Distinguishing "warm-up complete" from "N is usable" is
// therefore the caller's responsibility — see
// event.SetupEvaluatedPayload's NReady, which internal/strategy.Reducer
// computes as Ready() && Value() > 0.
//
// Ordering is also the caller's responsibility, and it matters here as much
// as EntryChannel's Extreme-then-Add does: a caller deciding a bar must read
// Value (and Ready) BEFORE calling Add with that bar's True Range, or the
// bar changes the N it is about to be decided against, and a wide bar
// shrinks its own Unit and tightens its own Protective Stop (CONTEXT.md:
// "Completed bar" — the decision bar is never an input to its own decision).
// This type cannot enforce that, since Value and Add are legitimately
// independent operations; internal/strategy.Reducer.applyCompletedBar is the
// one call site, and its evaluate and advance blocks are laid out so the
// order is visible at a glance.
type WilderAverage struct {
	period int
	seed   []float64 // buffered True Range values until the seed is computed
	count  int
	value  float64
}

// NewWilderAverage returns a WilderAverage over the given period, measured
// in completed bars. period must be positive.
func NewWilderAverage(period int) (*WilderAverage, error) {
	if period <= 0 {
		return nil, fmt.Errorf("indicator: period must be positive, got %d", period)
	}
	return &WilderAverage{period: period}, nil
}

// Add feeds one more True Range value, in bar order, into the average. Each
// call advances the bar-count warm-up by exactly one, regardless of how much
// calendar time the bar spans or how far apart consecutive bars are dated —
// warm-up is counted in completed bars, never calendar days.
func (w *WilderAverage) Add(tr float64) {
	w.count++
	if w.count < w.period {
		w.seed = append(w.seed, tr)
		return
	}
	if w.count == w.period {
		w.seed = append(w.seed, tr)
		w.value = SMASeed(w.seed)
		w.seed = nil // the buffered seed values are never needed again
		return
	}
	w.value = WilderNext(w.value, tr, w.period)
}

// Ready reports whether at least Period True Range values have been added,
// i.e. whether bar-count warm-up is complete. This is not the same as
// "Value is a usable volatility reading" — see the type's doc comment.
func (w *WilderAverage) Ready() bool {
	return w.count >= w.period
}

// Value returns the current N. Before Ready reports true this is the zero
// value (0) and must not be used for a decision.
func (w *WilderAverage) Value() float64 {
	return w.value
}
