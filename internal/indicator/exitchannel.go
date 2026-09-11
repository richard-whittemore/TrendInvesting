package indicator

import "fmt"

// ExitChannel is a rolling window that reports the LOWEST low among the last
// Length completed bars Added to it (CONTEXT.md: "Exit Channel"). The Turtle
// Rules p.26: System 2 closes an open Campaign in full when price falls
// below the lowest low of the preceding 20 completed bars (ADR 0002).
//
// This is a sibling type to EntryChannel, not a generalisation of it. The two
// share only a few lines of linear min/max scanning over a ring buffer, which
// is not enough shared behaviour to justify threading an "extreme selector"
// through one generic type — doing so would touch EntryChannel's own file and
// put its already-passing tests at risk for no benefit here. Keeping them as
// two small, independent types means EntryChannel's tests and this ticket's
// are both untouched by the other's change.
//
// # Evaluate-then-add: the same ordering EntryChannel enforces, mirrored
//
// Extreme reports the channel low computed from the values Added so far, and
// nothing more. Callers MUST call Extreme to evaluate the bar under decision
// BEFORE calling Add with that same bar's low — see EntryChannel's doc
// comment for the look-ahead bug this ordering exists to prevent (the same
// defect, mirrored: a channel that already contains the current bar's own low
// is being asked "does today's low fall below a channel that includes
// today's low?", which reduces to "is today's low strictly less than
// itself?" — never true).
//
// This type's two-method shape (Extreme, then Add) makes the correct order
// the only one that reads naturally at the call site — see
// internal/strategy.Reducer.applyCompletedBar, which calls Extreme before Add
// on every bar, whether or not the instrument is currently in a Campaign (the
// window must already be warm the moment a Campaign opens).
//
// A low exactly equal to the channel low is not a breach: The Turtle Rules
// p.26 says price "falls below" the channel, mirroring p.19's "exceeds" for
// the Entry Channel, so the caller's comparison must be strict (<), never
// (<=). ExitChannel itself has no opinion on what counts as a breach — it
// only reports the extreme value — but Extreme correctly returns the tied
// value so that comparison behaves as intended.
type ExitChannel struct {
	length int
	values []float64
	count  int
}

// NewExitChannel returns an ExitChannel over the given length, measured in
// completed bars (never calendar days — CONTEXT.md: "Completed bar"). length
// must be positive. In the Baseline this is
// event.ConfigurationPayload.ExitChannelLength (20, ADR 0002), never
// hard-coded.
func NewExitChannel(length int) (*ExitChannel, error) {
	if length <= 0 {
		return nil, fmt.Errorf("indicator: exit channel length must be positive, got %d", length)
	}
	return &ExitChannel{length: length, values: make([]float64, length)}, nil
}

// Extreme returns the lowest low among the values Added so far (the last
// Length of them once the window is full), and whether at least Length
// values have been Added. Before any value has been Added, it returns
// (0, false). Call this before Add for the bar under decision — see the
// type's doc comment.
//
// The window is a straightforward ring buffer scanned linearly on every
// call; at the Baseline's length (20, ADR 0002) this is a handful of
// arithmetic operations and is not worth optimising (ADR 0011: per-bar
// evaluation across the whole universe is dominated by data loading, not
// this) — the same reasoning EntryChannel.Extreme documents.
func (c *ExitChannel) Extreme() (value float64, ready bool) {
	if c.count == 0 {
		return 0, false
	}
	n := c.count
	if n > c.length {
		n = c.length
	}
	lowest := c.values[0]
	for i := 1; i < n; i++ {
		if c.values[i] < lowest {
			lowest = c.values[i]
		}
	}
	return lowest, c.count >= c.length
}

// Add feeds one more completed bar's low into the window, in bar order. Call
// this AFTER Extreme has been used to evaluate the bar under decision — see
// the type's doc comment. Add does not itself validate low: the caller is
// responsible for supplying a finite value, exactly as EntryChannel.Add
// trusts its callers (bar.go's PriceView.validate already rejects a
// non-finite or non-positive low before it reaches this package).
func (c *ExitChannel) Add(low float64) {
	c.values[c.count%c.length] = low
	c.count++
}
