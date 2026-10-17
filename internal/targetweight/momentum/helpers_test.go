package momentum_test

import (
	"fmt"
	"math"
	"testing"
	"time"
)

// calendar returns n consecutive weekday sessions from 2001-01-01, as
// YYYY-MM-DD strings, oldest first.
func calendar(n int) []string {
	out := make([]string, 0, n)
	day := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	for len(out) < n {
		if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday {
			out = append(out, day.Format("2006-01-02"))
		}
		day = day.AddDate(0, 0, 1)
	}
	return out
}

// series maps the last len(prices) sessions of cal to prices, oldest first.
func series(cal []string, prices []float64) map[string]float64 {
	out := make(map[string]float64, len(prices))
	start := len(cal) - len(prices)
	for i, p := range prices {
		out[cal[start+i]] = p
	}
	return out
}

// constant returns n copies of price.
func constant(n int, price float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = price
	}
	return out
}

// alternating returns n closes 100, 200, 100, ... : daily returns of +1 and
// -0.5 in turn, whose population standard deviation is exactly 0.75 over an
// even number of returns, and whose every partial sum is exact in binary.
func alternating(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = 100
		if i%2 == 1 {
			out[i] = 200
		}
	}
	return out
}

// trending returns n closes growing by factor each session from 100, so a
// name's 12-1 score rises with factor and its volatility is near zero but
// not zero (the growth is not exact in binary).
func trending(n int, factor float64) []float64 {
	out := make([]float64, n)
	p := 100.0
	for i := range out {
		out[i] = p
		p = float64(p * factor)
	}
	return out
}

// wobbling returns n closes that alternate around a level growing by
// growth each session, swinging by swing: a name with a chosen score and a
// volatility that grows with swing.
func wobbling(n int, growth, swing float64) []float64 {
	out := make([]float64, n)
	level := 100.0
	for i := range out {
		f := 1 - swing
		if i%2 == 1 {
			f = 1 + swing
		}
		out[i] = float64(level * f)
		level = float64(level * growth)
	}
	return out
}

func near(t *testing.T, label string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Errorf("%s = %v, want %v (±%g)", label, got, want, tol)
	}
}

func mustEqual[T comparable](t *testing.T, label string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}

func ids(n int, prefix string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%03d", prefix, i)
	}
	return out
}
