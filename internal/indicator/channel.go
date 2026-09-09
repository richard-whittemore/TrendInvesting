package indicator

import "fmt"

// EntryChannel is a rolling window that reports the highest high among the
// last Length completed bars Added to it (CONTEXT.md: "Entry Channel"). The
// Turtle Rules p.19: System 2 enters when price exceeds the highest high of
// the preceding 55 completed bars (ADR 0002).
//
// # Evaluate-then-add: the ordering that fixes the prototype's headline bug
//
// Extreme reports the channel high computed from the values Added so far,
// and nothing more. Callers MUST call Extreme to evaluate the bar under
// decision BEFORE calling Add with that same bar's high. Getting this
// backwards — adding the current bar to the window and then reading
// Extreme — is the exact defect the legacy QuantConnect prototype shipped
// with (docs/methodology/Plan_and_Ticket_Review.md: "look-ahead in the
// Donchian read", in its bug taxonomy): a channel that already contains the
// current bar's own high is being asked "does today's high exceed a channel
// that includes today's high?", which reduces to "is today's high strictly
// greater than itself?" — never true, except in the degenerate case where a
// later, unrelated bar happens to tie it. The Signal that should have fired
// on the breakout bar silently does not.
//
// This type's two-method shape (Extreme, then Add) makes the correct order
// the only one that reads naturally at the call site — see
// internal/strategy.Reducer.applyCompletedBar, which calls Extreme before
// Add on every bar.
//
// A high exactly equal to the channel high is not a breakout: The Turtle
// Rules p.19 says a Breakout "exceeds" the channel, so the caller's
// comparison must be strict (>), never (>=). EntryChannel itself has no
// opinion on what counts as a breakout — it only reports the extreme value —
// but Extreme correctly returns the tied value so that comparison behaves
// as intended.
type EntryChannel struct {
	length int
	values []float64
	count  int
}

// NewEntryChannel returns an EntryChannel over the given length, measured in
// completed bars (never calendar days — CONTEXT.md: "Completed bar"). length
// must be positive.
func NewEntryChannel(length int) (*EntryChannel, error) {
	if length <= 0 {
		return nil, fmt.Errorf("indicator: entry channel length must be positive, got %d", length)
	}
	return &EntryChannel{length: length, values: make([]float64, length)}, nil
}

// Extreme returns the highest high among the values Added so far (the last
// Length of them once the window is full), and whether at least Length
// values have been Added. Before any value has been Added, it returns
// (0, false). Call this before Add for the bar under decision — see the
// type's doc comment.
//
// The window is a straightforward ring buffer scanned linearly on every
// call; at the Baseline's length (55, ADR 0002) this is a handful of
// arithmetic operations and is not worth optimising (ADR 0011: per-bar
// evaluation across the whole universe is dominated by data loading, not
// this).
func (c *EntryChannel) Extreme() (value float64, ready bool) {
	if c.count == 0 {
		return 0, false
	}
	n := c.count
	if n > c.length {
		n = c.length
	}
	highest := c.values[0]
	for i := 1; i < n; i++ {
		if c.values[i] > highest {
			highest = c.values[i]
		}
	}
	return highest, c.count >= c.length
}

// Add feeds one more completed bar's high into the window, in bar order.
// Call this AFTER Extreme has been used to evaluate the bar under decision —
// see the type's doc comment. Add does not itself validate high: the caller
// is responsible for supplying a finite value, exactly as TrueRange and
// WilderNext trust their callers (bar.go's PriceView.validate already
// rejects a non-finite or non-positive high before it reaches this
// package).
func (c *EntryChannel) Add(high float64) {
	c.values[c.count%c.length] = high
	c.count++
}
