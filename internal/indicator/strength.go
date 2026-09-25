package indicator

import (
	"fmt"
	"slices"
)

// RollingWindow is a bounded FIFO of the most recently Added values, holding
// at most Capacity of them: the shared shape behind Strength's
// StrengthLookbackBars+1 close history and the DollarVolumeWindow raw
// close/volume history ADR 0009's eligibility test and ADR 0010's ranking
// tie-break both read (CONTEXT.md: "Strength"; The Turtle Rules p.29, ADR
// 0010, as amended by the owner's decision of 2026-09-25). Unlike
// EntryChannel and ExitChannel, which report only a running extreme, a
// caller here needs the whole window's own values, in order.
type RollingWindow struct {
	capacity int
	values   []float64
	count    int
}

// NewRollingWindow returns a RollingWindow over the given capacity, measured
// in completed Sessions (CONTEXT.md: "Completed bar"), never calendar days.
// capacity must be positive.
func NewRollingWindow(capacity int) (*RollingWindow, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("indicator: rolling window capacity must be positive, got %d", capacity)
	}
	return &RollingWindow{capacity: capacity}, nil
}

// Add feeds one more completed Session's value into the window, in Session
// order, evicting the oldest value once the window is Full.
func (w *RollingWindow) Add(v float64) {
	if len(w.values) < w.capacity {
		w.values = append(w.values, v)
		w.count++
		return
	}
	w.values[w.count%w.capacity] = v
	w.count++
}

// Full reports whether at least Capacity values have been Added.
func (w *RollingWindow) Full() bool {
	return w.count >= w.capacity
}

// Values returns every value currently held, in chronological order (oldest
// first): fewer than Capacity of them before the window is Full, exactly
// Capacity once it is. The result is a copy; mutating it never changes the
// window.
func (w *RollingWindow) Values() []float64 {
	if len(w.values) < w.capacity {
		return slices.Clone(w.values)
	}
	out := make([]float64, w.capacity)
	for i := range out {
		out[i] = w.values[(w.count+i)%w.capacity]
	}
	return out
}

// Clone returns an independent accumulator, including its buffered values.
// Reducer transactions must not advance committed completed-bar inputs on
// rejection (CONTEXT.md: "Completed bar"). A nil receiver stays nil.
func (w *RollingWindow) Clone() *RollingWindow {
	if w == nil {
		return nil
	}
	cloned := *w
	cloned.values = slices.Clone(w.values)
	return &cloned
}

// StrengthLookbackBars is the number of completed Sessions Faith's Strength
// measure looks back (CONTEXT.md: "Strength"; The Turtle Rules p.29, ADR
// 0010): for a Signal decided at the close of Session d, Strength is
// (close(d) - close(d-StrengthLookbackBars)) / N(d) (the owner's decision of
// 2026-09-25, recorded in ADR 0010).
const StrengthLookbackBars = 63

// Strength is Faith's mechanical ranking measure, applied when several
// instruments signal at once (CONTEXT.md: "Strength"; The Turtle Rules p.29,
// ADR 0010, as amended by the owner's decision of 2026-09-25): the change in
// the split-adjusted close (ADR 0004) over the preceding StrengthLookbackBars
// completed Sessions, divided by N.
//
// closes must be in chronological order (oldest first); only its last
// StrengthLookbackBars+1 elements matter (close(d-StrengthLookbackBars)
// through close(d)), so a longer history is accepted and its older entries
// ignored. Fewer than StrengthLookbackBars+1 elements reports not ready: an
// instrument with insufficient price history cannot be ranked, and its Signal
// is declined rather than ranked last or given a fabricated Strength (the
// owner's decision — an incomparable instrument has no place in a total
// order).
//
// n must be N(d), the Session's own N (the volatility reading once this
// Session's own bar has already been folded into it — deliberately not the
// pre-advance N a Signal's own entry sizing uses, which ADR 0005 requires to
// exclude this Session's bar because the entry can still fill inside it;
// ranking runs only once the Session has fully closed, so this Session's own
// close and N are already completed facts by then). The caller is
// responsible for n being a usable, positive volatility reading before
// calling this: a zero or negative N here would produce a non-finite or
// meaningless Strength, which this function does not itself guard against.
func Strength(closes []float64, n float64) (value float64, ready bool) {
	if len(closes) < StrengthLookbackBars+1 {
		return 0, false
	}
	last := len(closes) - 1
	return (closes[last] - closes[last-StrengthLookbackBars]) / n, true
}

// DollarVolumeWindow is the number of completed Sessions the median dollar
// volume measure averages over, ending with the decision Session, inclusive —
// the one definition ADR 0009's $5M eligibility test and ADR 0010's ranking
// tie-break both read (ADR 0010, as amended by the owner's decision of
// 2026-09-25).
const DollarVolumeWindow = 20

// MedianDollarVolume is the median of raw close x raw volume (ADR 0004: the
// raw view, because dollar volume measures real money traded, not the
// split-adjusted view Strength and the other signal arithmetic use) over the
// DollarVolumeWindow completed Sessions ending at the decision Session,
// inclusive.
//
// closes and volumes must be the same length, in chronological order (oldest
// first), and fed in lockstep — one raw close and one raw volume per
// completed Session, so a length mismatch is a caller defect, not a data
// condition, and panics rather than silently reporting a misleading result.
// Only the last DollarVolumeWindow elements matter, matching Strength's own
// tail-window convention. Fewer than DollarVolumeWindow elements reports not
// ready, for the identical reason Strength's own insufficient-history case
// does (the owner's decision of 2026-09-25): the instrument cannot be ranked,
// and ADR 0009's eligibility test cannot admit it either.
//
// The median of DollarVolumeWindow's always-even-length window is the mean of
// the two middle values (the owner's decision of 2026-09-25).
func MedianDollarVolume(closes, volumes []float64) (value float64, ready bool) {
	if len(closes) != len(volumes) {
		panic(fmt.Sprintf("indicator: median dollar volume: %d closes but %d volumes: the two windows must be fed in lockstep, one raw close and one raw volume per completed Session", len(closes), len(volumes)))
	}
	if len(closes) < DollarVolumeWindow {
		return 0, false
	}
	start := len(closes) - DollarVolumeWindow
	dollarVolumes := make([]float64, DollarVolumeWindow)
	for i := range dollarVolumes {
		// The product feeds medianOf's addition below whenever the window's
		// length is even — DollarVolumeWindow's own length, always — so it is
		// rounded here, a barrier the two architectures that fuse
		// differently would otherwise disagree across (docs/development.md,
		// floating-point determinism).
		dollarVolumes[i] = float64(closes[start+i] * volumes[start+i])
	}
	return medianOf(dollarVolumes), true
}

// medianOf returns the median of values: the middle element of an odd-length
// slice, the mean of the two middle elements of an even-length one (the
// owner's decision of 2026-09-25, ADR 0010). It sorts a private copy; the
// caller's slice is never mutated.
func medianOf(values []float64) float64 {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}
