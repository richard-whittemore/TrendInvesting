package main

import (
	"fmt"
	"html/template"
	"strings"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// held is what the first paired point's balance would be worth today had it
// been held in the benchmark: the same price return the benchmark column
// shows.
func held(e report.Equity) string {
	if len(e.Points) < 2 {
		return ""
	}
	first, last := e.Points[0], e.Points[len(e.Points)-1]
	return fmt.Sprintf("%.2f", first.Net*last.Benchmark/first.Benchmark)
}

// tone names the class that colours a figure by its sign; the arrow beside it
// carries the sign too, so colour is never the only cue.
func tone(v float64) string {
	if v < 0 {
		return "neg"
	}
	return "pos"
}

func arrow(v float64) string {
	if v < 0 {
		return "▼"
	}
	return "▲"
}

// stateClass groups the engine's states by how much attention each needs.
func stateClass(state string) string {
	switch strings.ToLower(state) {
	case report.StateNormal:
		return "ok"
	case event.EngineStateDegraded:
		return "warn"
	}
	return "bad"
}

func lastPoint(e report.Equity) report.EquityPoint { return e.Points[len(e.Points)-1] }

func beforeLast(e report.Equity) report.EquityPoint { return e.Points[len(e.Points)-2] }

var page = template.Must(template.New("page").Funcs(template.FuncMap{
	"weight":     func(v float64) string { return fmt.Sprintf("%.0f%%", v*100) },
	"shares":     event.FormatSkippedShares,
	"pct":        func(v float64) string { return fmt.Sprintf("%+.2f%%", v*100) },
	"money":      func(v float64) string { return fmt.Sprintf("%.2f", v) },
	"held":       held,
	"tone":       tone,
	"arrow":      arrow,
	"stateClass": stateClass,
	"first":      func(e report.Equity) report.EquityPoint { return e.Points[0] },
	"last":       lastPoint,
	"before":     beforeLast,
	"count":      func(e report.Equity) int { return len(e.Points) },
	"chart":      equityChart,
	"strip":      dailyChangeStrip,
	"assumption": func() string { return equityAssumption },
}).Parse(pageHTML))
