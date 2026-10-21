package main

import (
	"fmt"
	"math"
	"strings"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// The charts are inline SVG computed here from the paired points the summary
// already carries: no script, no library and no external asset (the page's
// Content-Security-Policy allows none). This file only computes geometry as
// text; the page template draws it, so every value is escaped by
// html/template like any other. Colours come from the page's theme variables
// through classes, so one drawing serves both themes. The account is a solid
// line with circles and the benchmark a dashed line with squares, so the two
// never rely on colour alone.

const (
	chartLeft, chartRight = 56.0, 400.0
	chartTop, chartBottom = 40.0, 160.0
)

func f(v float64) string { return fmt.Sprintf("%.1f", v) }

// tick is one labelled horizontal gridline.
type tick struct{ Y, TextY, Text string }

// marker is a point on a line: the circle's centre, and the square's corner.
type marker struct{ CX, CY, SX, SY string }

func markerAt(x, y float64) marker {
	return marker{CX: f(x), CY: f(y), SX: f(x - 4), SY: f(y - 4)}
}

// equityView is the equity chart: each series as the growth of its first
// paired point rebased to 100, one path per series with one vertex per paired
// point. Drawn is false when there are fewer than two paired points, and the
// page then says so instead of drawing.
type equityView struct {
	Drawn                            bool
	Label                            string
	Benchmark                        string
	Ticks                            []tick
	Left, Right, LabelX, EndX, BaseY string
	AccountPath, BenchmarkPath       string
	AccountFirst, AccountLast        marker
	BenchmarkFirst, BenchmarkLast    marker
	AccountEnd, BenchmarkEnd         string
	AccountEndY, BenchmarkEndY       string
	StartDate, EndDate               string
}

func equityChart(s report.Summary) equityView {
	pts := s.Equity.Points
	if len(pts) < 2 {
		return equityView{}
	}
	acct, bench := make([]float64, len(pts)), make([]float64, len(pts))
	lo, hi := 100.0, 100.0
	for i, p := range pts {
		acct[i], bench[i] = 100*p.Net/pts[0].Net, 100*p.Benchmark/pts[0].Benchmark
		lo, hi = math.Min(lo, math.Min(acct[i], bench[i])), math.Max(hi, math.Max(acct[i], bench[i]))
	}
	if hi-lo < 0.01 {
		lo, hi = lo-0.5, hi+0.5
	}
	pad := (hi - lo) * 0.08
	lo, hi = lo-pad, hi+pad
	y := func(v float64) float64 { return chartBottom - float64((v-lo)/(hi-lo)*(chartBottom-chartTop)) }
	x := func(i int) float64 { return chartLeft + float64(i)*(chartRight-chartLeft)/float64(len(pts)-1) }
	path := func(series []float64) string {
		var b strings.Builder
		for i, v := range series {
			cmd := " L"
			if i == 0 {
				cmd = "M"
			}
			b.WriteString(cmd + f(x(i)) + " " + f(y(v)))
		}
		return b.String()
	}
	last := len(pts) - 1
	v := equityView{
		Drawn: true, Benchmark: s.Benchmark,
		Left: f(chartLeft), Right: f(chartRight), LabelX: f(chartLeft - 8), EndX: f(chartRight + 10), BaseY: f(y(100)),
		AccountPath: path(acct), BenchmarkPath: path(bench),
		AccountFirst: markerAt(x(0), y(acct[0])), AccountLast: markerAt(x(last), y(acct[last])),
		BenchmarkFirst: markerAt(x(0), y(bench[0])), BenchmarkLast: markerAt(x(last), y(bench[last])),
		AccountEnd: fmt.Sprintf("%.2f", acct[last]), BenchmarkEnd: fmt.Sprintf("%.2f", bench[last]),
		AccountEndY: f(y(acct[last]) + 4), BenchmarkEndY: f(y(bench[last]) + 4),
		StartDate: pts[0].Date, EndDate: pts[last].Date,
		Label: fmt.Sprintf("%s, equity against %s, both rebased to 100.00 on %s. On %s the account stands at %.2f and %s at %.2f.",
			s.AccountID, s.Benchmark, pts[0].Date, pts[last].Date, acct[last], s.Benchmark, bench[last]),
	}
	for i := 0; i < 4; i++ {
		t := lo + (hi-lo)*float64(i)/3
		v.Ticks = append(v.Ticks, tick{Y: f(y(t)), TextY: f(y(t) + 4), Text: fmt.Sprintf("%.2f", t)})
	}
	return v
}

// bar is one day's two bars, the account's then the benchmark's.
type bar struct{ AX, AY, AH, BX, BY, BH string }

// stripView is the daily-change strip: for each day between consecutive
// paired points, the account's change and the benchmark's as a pair of bars
// about a zero line, above it for a rise and below for a fall, on one scale.
type stripView struct {
	Drawn                             bool
	Label                             string
	Benchmark                         string
	Bars                              []bar
	Width                             string
	Left, Right, TopY, MidY, BottomY  string
	LabelX, TopText, MidText, BotText string
	TopLabel, BottomLabel             string
	StartDate, EndDate                string
}

func dailyChangeStrip(s report.Summary) stripView {
	pts := s.Equity.Points
	if len(pts) < 2 {
		return stripView{}
	}
	days := len(pts) - 1
	acct, bench := make([]float64, days), make([]float64, days)
	maxAbs := 0.0
	for i := 1; i < len(pts); i++ {
		acct[i-1], bench[i-1] = pts[i].Net/pts[i-1].Net-1, pts[i].Benchmark/pts[i-1].Benchmark-1
		maxAbs = math.Max(maxAbs, math.Max(math.Abs(acct[i-1]), math.Abs(bench[i-1])))
	}
	if maxAbs == 0 {
		maxAbs = 0.01
	}
	mid, half := (chartTop+chartBottom)/2, (chartBottom-chartTop)/2
	slot := (chartRight - chartLeft) / float64(days)
	// geometry returns a bar's top edge and height: up from the zero line
	// for a rise, down from it for a fall, at least a hairline so a day with
	// no change is still drawn.
	geometry := func(v float64) (top, h float64) {
		h = math.Max(math.Abs(v)/maxAbs*half, 1)
		if v < 0 {
			return mid, h
		}
		return mid - h, h
	}
	out := stripView{
		Drawn: true, Benchmark: s.Benchmark, Width: f(slot * 0.4),
		Left: f(chartLeft), Right: f(chartRight), TopY: f(chartTop), MidY: f(mid), BottomY: f(chartBottom),
		LabelX: f(chartLeft - 8), TopText: f(chartTop + 4), MidText: f(mid + 4), BotText: f(chartBottom + 4),
		TopLabel: fmt.Sprintf("+%.2f%%", maxAbs*100), BottomLabel: fmt.Sprintf("−%.2f%%", maxAbs*100),
		StartDate: pts[1].Date, EndDate: pts[days].Date,
		Label: fmt.Sprintf("%s, daily change from %s to %s, %d days: the account's change as one bar and %s's as the next, above the zero line for a rise and below for a fall, on a scale of plus or minus %.2f percent.",
			s.AccountID, pts[0].Date, pts[days].Date, days, s.Benchmark, maxAbs*100),
	}
	for i := 0; i < days; i++ {
		left := chartLeft + float64(float64(i)*slot) + float64(slot*0.1)
		at, ah := geometry(acct[i])
		bt, bh := geometry(bench[i])
		out.Bars = append(out.Bars, bar{AX: f(left), AY: f(at), AH: f(ah), BX: f(left + float64(slot*0.4)), BY: f(bt), BH: f(bh)})
	}
	return out
}
