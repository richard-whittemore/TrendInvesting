package indicator_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/indicator"
)

func TestNewExitChannelRejectsNonPositiveLength(t *testing.T) {
	t.Parallel()

	for _, length := range []int{0, -1, -20} {
		if _, err := indicator.NewExitChannel(length); err == nil {
			t.Fatalf("NewExitChannel(%d) error = nil, want error", length)
		}
	}
}

func TestExitChannelNotReadyBeforeLengthValues(t *testing.T) {
	t.Parallel()

	c, err := indicator.NewExitChannel(3)
	if err != nil {
		t.Fatalf("NewExitChannel() error = %v", err)
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

func TestExitChannelReadyAtExactlyLengthValuesWithCorrectMin(t *testing.T) {
	t.Parallel()

	c, err := indicator.NewExitChannel(3)
	if err != nil {
		t.Fatalf("NewExitChannel() error = %v", err)
	}
	c.Add(5)
	c.Add(3)
	c.Add(8)

	value, ready := c.Extreme()
	if !ready {
		t.Fatal("Extreme() ready = false after 3 of 3 values, want true")
	}
	if value != 3 {
		t.Fatalf("Extreme() value = %v, want 3", value)
	}
}

// TestExitChannelRollsOffOldestValue confirms the window is a genuine rolling
// window: once a 4th value is added to a length-3 channel, the oldest value
// (3, the min) leaves the window, and the reported min must change to
// reflect only the last 3 values added.
func TestExitChannelRollsOffOldestValue(t *testing.T) {
	t.Parallel()

	c, err := indicator.NewExitChannel(3)
	if err != nil {
		t.Fatalf("NewExitChannel() error = %v", err)
	}
	c.Add(3) // oldest; will roll off
	c.Add(5)
	c.Add(9)
	c.Add(7) // window is now [5, 9, 7]

	value, ready := c.Extreme()
	if !ready {
		t.Fatal("Extreme() ready = false, want true")
	}
	if value != 5 {
		t.Fatalf("Extreme() value = %v, want 5 (3 has rolled off the window)", value)
	}
}

// TestExitChannelTiesAreHandled confirms two equal lows in the window do not
// confuse the min computation: the reported extreme is that shared value,
// not an error or an arbitrary pick.
func TestExitChannelTiesAreHandled(t *testing.T) {
	t.Parallel()

	c, err := indicator.NewExitChannel(3)
	if err != nil {
		t.Fatalf("NewExitChannel() error = %v", err)
	}
	c.Add(4)
	c.Add(4)
	c.Add(9)

	value, ready := c.Extreme()
	if !ready || value != 4 {
		t.Fatalf("Extreme() = (%v, %v), want (4, true)", value, ready)
	}
}

// TestExitChannelAddDoesNotRetroactivelyChangeAPreviousExtremeReading is the
// arithmetic-seam expression of the evaluate-then-add ordering this type
// exists to enforce (see the type's doc comment): a value returned by
// Extreme is a plain float64, so a later Add can never mutate it, but the
// *next* call to Extreme must reflect the newly Added value.
func TestExitChannelAddDoesNotRetroactivelyChangeAPreviousExtremeReading(t *testing.T) {
	t.Parallel()

	c, err := indicator.NewExitChannel(3)
	if err != nil {
		t.Fatalf("NewExitChannel() error = %v", err)
	}
	c.Add(3)
	c.Add(2)
	c.Add(1)

	before, ready := c.Extreme()
	if !ready || before != 1 {
		t.Fatalf("Extreme() before Add(-100) = (%v, %v), want (1, true)", before, ready)
	}

	c.Add(-100) // a new lower value; window is now [2, 1, -100]

	if before != 1 {
		t.Fatalf("previously returned Extreme() value changed to %v, want 1 (unaffected by a later Add)", before)
	}
	after, ready := c.Extreme()
	if !ready || after != -100 {
		t.Fatalf("Extreme() after Add(-100) = (%v, %v), want (-100, true)", after, ready)
	}
}

// TestExitChannelTwentyBarBaselineLengthBreach is a hand-derivable fixture at
// the ticket's Baseline length (20, ADR 0002): 20 completed bars with lows
// 20..1 descending (Extreme = 1, ready), then a 21st bar whose low is 0.
// Evaluated correctly (Extreme called before Add), 0 < 1 is a breach.
// The event-seam fixture in internal/strategy uses the same shape; this pins
// the arithmetic independently at the seam that owns it.
func TestExitChannelTwentyBarBaselineLengthBreach(t *testing.T) {
	t.Parallel()

	c, err := indicator.NewExitChannel(20)
	if err != nil {
		t.Fatalf("NewExitChannel() error = %v", err)
	}
	for i := 20; i >= 1; i-- {
		c.Add(float64(i))
	}

	channelLow, ready := c.Extreme()
	if !ready {
		t.Fatal("Extreme() ready = false after 20 values, want true")
	}
	if channelLow != 1 {
		t.Fatalf("Extreme() value = %v, want 1", channelLow)
	}

	const breachLow = 0.0
	if !(breachLow < channelLow) {
		t.Fatalf("breach condition failed: %v is not < %v", breachLow, channelLow)
	}
}

// TestExitChannelTieIsNotABreach locks in Faith's word choice (The Turtle
// Rules p.26: "falls below"), mirroring the Entry Channel's "exceeds": a low
// exactly equal to the channel low must not satisfy a strict-less-than
// breach check. ExitChannel itself has no opinion on what counts as a breach
// (that is internal/strategy.Reducer's job), but this pins the arithmetic
// fact the reducer's strict comparison relies on: Extreme reports the tied
// value, and a caller comparing with a strict `<` correctly finds no breach.
func TestExitChannelTieIsNotABreach(t *testing.T) {
	t.Parallel()

	c, err := indicator.NewExitChannel(3)
	if err != nil {
		t.Fatalf("NewExitChannel() error = %v", err)
	}
	c.Add(6)
	c.Add(8)
	c.Add(10)

	channelLow, ready := c.Extreme()
	if !ready || channelLow != 6 {
		t.Fatalf("Extreme() = (%v, %v), want (6, true)", channelLow, ready)
	}

	tieLow := 6.0
	if tieLow < channelLow {
		t.Fatalf("a tied low (%v) must not fall below the channel low (%v)", tieLow, channelLow)
	}
}
