package indicator_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/indicator"
)

func TestNewEntryChannelRejectsNonPositiveLength(t *testing.T) {
	t.Parallel()

	for _, length := range []int{0, -1, -55} {
		if _, err := indicator.NewEntryChannel(length); err == nil {
			t.Fatalf("NewEntryChannel(%d) error = nil, want error", length)
		}
	}
}

func TestEntryChannelNotReadyBeforeLengthValues(t *testing.T) {
	t.Parallel()

	c, err := indicator.NewEntryChannel(3)
	if err != nil {
		t.Fatalf("NewEntryChannel() error = %v", err)
	}

	if _, ready := c.Extreme(); ready {
		t.Fatal("Extreme() ready = true before any Add, want false")
	}

	c.Add(5)
	if _, ready := c.Extreme(); ready {
		t.Fatal("Extreme() ready = true after 1 of 3 values, want false")
	}

	c.Add(3)
	if _, ready := c.Extreme(); ready {
		t.Fatal("Extreme() ready = true after 2 of 3 values, want false")
	}
}

func TestEntryChannelReadyAtExactlyLengthValuesWithCorrectMax(t *testing.T) {
	t.Parallel()

	c, err := indicator.NewEntryChannel(3)
	if err != nil {
		t.Fatalf("NewEntryChannel() error = %v", err)
	}
	c.Add(5)
	c.Add(3)
	c.Add(1)

	value, ready := c.Extreme()
	if !ready {
		t.Fatal("Extreme() ready = false after 3 of 3 values, want true")
	}
	if value != 5 {
		t.Fatalf("Extreme() value = %v, want 5", value)
	}
}

// TestEntryChannelRollsOffOldestValue confirms the window is a genuine
// rolling window: once a 4th value is added to a length-3 channel, the
// oldest value (5, the max) leaves the window, and the reported max must
// change to reflect only the last 3 values added.
func TestEntryChannelRollsOffOldestValue(t *testing.T) {
	t.Parallel()

	c, err := indicator.NewEntryChannel(3)
	if err != nil {
		t.Fatalf("NewEntryChannel() error = %v", err)
	}
	c.Add(5) // oldest; will roll off
	c.Add(3)
	c.Add(1)
	c.Add(2) // window is now [3, 1, 2]

	value, ready := c.Extreme()
	if !ready {
		t.Fatal("Extreme() ready = false, want true")
	}
	if value != 3 {
		t.Fatalf("Extreme() value = %v, want 3 (5 has rolled off the window)", value)
	}
}

// TestEntryChannelTiesAreHandled confirms two equal highs in the window do
// not confuse the max computation: the reported extreme is that shared
// value, not an error or an arbitrary pick.
func TestEntryChannelTiesAreHandled(t *testing.T) {
	t.Parallel()

	c, err := indicator.NewEntryChannel(3)
	if err != nil {
		t.Fatalf("NewEntryChannel() error = %v", err)
	}
	c.Add(4)
	c.Add(4)
	c.Add(2)

	value, ready := c.Extreme()
	if !ready || value != 4 {
		t.Fatalf("Extreme() = (%v, %v), want (4, true)", value, ready)
	}
}

// TestEntryChannelAddDoesNotRetroactivelyChangeAPreviousExtremeReading is the
// arithmetic-seam expression of the evaluate-then-add ordering this type
// exists to enforce (see the type's doc comment): a value returned by
// Extreme is a plain float64, so a later Add can never mutate it, but the
// *next* call to Extreme must reflect the newly Added value.
func TestEntryChannelAddDoesNotRetroactivelyChangeAPreviousExtremeReading(t *testing.T) {
	t.Parallel()

	c, err := indicator.NewEntryChannel(3)
	if err != nil {
		t.Fatalf("NewEntryChannel() error = %v", err)
	}
	c.Add(1)
	c.Add(2)
	c.Add(3)

	before, ready := c.Extreme()
	if !ready || before != 3 {
		t.Fatalf("Extreme() before Add(100) = (%v, %v), want (3, true)", before, ready)
	}

	c.Add(100) // a new higher value; window is now [2, 3, 100]

	if before != 3 {
		t.Fatalf("previously returned Extreme() value changed to %v, want 3 (unaffected by a later Add)", before)
	}
	after, ready := c.Extreme()
	if !ready || after != 100 {
		t.Fatalf("Extreme() after Add(100) = (%v, %v), want (100, true)", after, ready)
	}
}

// TestEntryChannelFiftyFiveBarBaselineLengthBreakout is a hand-derivable
// fixture at the ticket's Baseline length (55, ADR 0002): 55 completed bars
// with highs 1..55 (Extreme = 55, ready), then a 56th bar whose high is 56.
// Evaluated correctly (Extreme called before Add), 56 > 55 is a breakout.
// The event-seam fixture in internal/strategy uses the same shape; this pins
// the arithmetic independently at the seam that owns it.
func TestEntryChannelFiftyFiveBarBaselineLengthBreakout(t *testing.T) {
	t.Parallel()

	c, err := indicator.NewEntryChannel(55)
	if err != nil {
		t.Fatalf("NewEntryChannel() error = %v", err)
	}
	for i := 1; i <= 55; i++ {
		c.Add(float64(i))
	}

	channelHigh, ready := c.Extreme()
	if !ready {
		t.Fatal("Extreme() ready = false after 55 values, want true")
	}
	if channelHigh != 55 {
		t.Fatalf("Extreme() value = %v, want 55", channelHigh)
	}

	const breakoutHigh = 56.0
	if !(breakoutHigh > channelHigh) {
		t.Fatalf("breakout condition failed: %v is not > %v", breakoutHigh, channelHigh)
	}
}

// TestEntryChannelTieIsNotABreakout locks in Faith's word choice (The Turtle
// Rules p.19: "exceeds"): a high exactly equal to the channel high must not
// satisfy a strict-greater-than breakout check. EntryChannel itself has no
// opinion on what counts as a breakout (that is internal/strategy.Reducer's
// job), but this pins the arithmetic fact the reducer's strict comparison
// relies on: Extreme reports the tied value, and a caller comparing with a
// strict `>` correctly finds no breakout.
func TestEntryChannelTieIsNotABreakout(t *testing.T) {
	t.Parallel()

	c, err := indicator.NewEntryChannel(3)
	if err != nil {
		t.Fatalf("NewEntryChannel() error = %v", err)
	}
	c.Add(10)
	c.Add(8)
	c.Add(6)

	channelHigh, ready := c.Extreme()
	if !ready || channelHigh != 10 {
		t.Fatalf("Extreme() = (%v, %v), want (10, true)", channelHigh, ready)
	}

	tieHigh := 10.0
	if tieHigh > channelHigh {
		t.Fatalf("a tied high (%v) must not exceed the channel high (%v)", tieHigh, channelHigh)
	}
}
