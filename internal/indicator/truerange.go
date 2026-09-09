// Package indicator computes the pure, deterministic arithmetic behind the
// project's strategy signals: True Range and N (CONTEXT.md: "True Range",
// "N"). It has no knowledge of events, instruments, or replay — it takes
// numbers in and returns numbers out, so it can be tested directly against
// primary-source golden fixtures without any transport machinery.
package indicator

import "math"

// TrueRange returns the True Range of a bar with the given high and low,
// using previousClose as the prior completed bar's close when
// hasPreviousClose is true.
//
// The Turtle Rules p.13: True Range = max(high-low, high-previousClose,
// previousClose-low). This measures a bar's movement including any opening
// gap: the second term captures a gap up (today's high stretching further
// above yesterday's close than today's own range shows), the third a gap
// down.
//
// When hasPreviousClose is false — the first bar in a series, where no
// prior close is available — the gap terms are undefined, so True Range is
// defined here as high-low: the bar's own range is the only movement that
// can be measured without a previous close. This is a deliberate choice for
// that case, not one implied by any source: Faith's Heating Oil table
// (p.14-15) starts mid-series, with a previous close already available for
// every printed row, so it does not settle this case either way.
func TrueRange(high, low, previousClose float64, hasPreviousClose bool) float64 {
	if !hasPreviousClose {
		return high - low
	}
	return math.Max(high-low, math.Max(high-previousClose, previousClose-low))
}
