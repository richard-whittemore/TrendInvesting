package indicator_test

import (
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/indicator"
)

func TestTrueRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		high, low        float64
		previousClose    float64
		hasPreviousClose bool
		want             float64
	}{
		{
			name:             "normal bar: own range dominates",
			high:             105,
			low:              99,
			previousClose:    100,
			hasPreviousClose: true,
			want:             6, // high-low=6, high-PDC=5, PDC-low=1
		},
		{
			name:             "gap up: previous close below the bar's own low",
			high:             110,
			low:              105,
			previousClose:    100,
			hasPreviousClose: true,
			want:             10, // high-low=5, high-PDC=10, PDC-low=-5
		},
		{
			name:             "gap down: previous close above the bar's own high",
			high:             95,
			low:              90,
			previousClose:    100,
			hasPreviousClose: true,
			want:             10, // high-low=5, high-PDC=-5, PDC-low=10
		},
		{
			name:             "no previous close: defined as the bar's own range",
			high:             105,
			low:              99,
			previousClose:    1_000_000, // must be ignored entirely
			hasPreviousClose: false,
			want:             6,
		},
		{
			name:             "no previous close, gap-shaped high/low still just high-low",
			high:             110,
			low:              105,
			previousClose:    0, // must be ignored entirely
			hasPreviousClose: false,
			want:             5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := indicator.TrueRange(tt.high, tt.low, tt.previousClose, tt.hasPreviousClose)
			if got != tt.want {
				t.Fatalf("TrueRange(%v, %v, %v, %v) = %v, want %v", tt.high, tt.low, tt.previousClose, tt.hasPreviousClose, got, tt.want)
			}
		})
	}
}

// A non-finite input must propagate as a non-finite result rather than
// silently participate in a max() comparison as if it were an ordinary
// number: ordered comparisons against NaN are always false in Go, and
// math.Max is specified to propagate NaN, so a poisoned bar surfaces as a
// poisoned True Range that downstream validation (event.isFinite) can
// reject, rather than a plausible-looking but wrong number.
func TestTrueRangeNonFiniteInputPropagates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		high, low        float64
		previousClose    float64
		hasPreviousClose bool
	}{
		{name: "NaN high", high: math.NaN(), low: 99, previousClose: 100, hasPreviousClose: true},
		{name: "NaN low", high: 105, low: math.NaN(), previousClose: 100, hasPreviousClose: true},
		{name: "NaN previous close", high: 105, low: 99, previousClose: math.NaN(), hasPreviousClose: true},
		{name: "+Inf high", high: math.Inf(1), low: 99, previousClose: 100, hasPreviousClose: true},
		{name: "-Inf low", high: 105, low: math.Inf(-1), previousClose: 100, hasPreviousClose: true},
		{name: "NaN high, no previous close", high: math.NaN(), low: 99, previousClose: 0, hasPreviousClose: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := indicator.TrueRange(tt.high, tt.low, tt.previousClose, tt.hasPreviousClose)
			if !math.IsNaN(got) && !math.IsInf(got, 0) {
				t.Fatalf("TrueRange(%v, %v, %v, %v) = %v, want non-finite", tt.high, tt.low, tt.previousClose, tt.hasPreviousClose, got)
			}
		})
	}
}
