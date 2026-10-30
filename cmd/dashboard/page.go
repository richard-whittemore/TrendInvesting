package main

import (
	"fmt"
	"html/template"
	"strings"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// verdictClass colours the verdict: green ahead, red behind, neutral level.
// Its arrow carries the same meaning, so colour is never the only cue.
func verdictClass(tone string) string {
	switch tone {
	case report.VerdictAhead:
		return "verdict pos"
	case report.VerdictBehind:
		return "verdict neg"
	}
	return "verdict level"
}

func verdictMark(tone string) string {
	switch tone {
	case report.VerdictAhead:
		return "▲"
	case report.VerdictBehind:
		return "▼"
	}
	return "="
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
	"weight":       func(v float64) string { return fmt.Sprintf("%.0f%%", v*100) },
	"shares":       event.FormatSkippedShares,
	"quantity":     event.FormatQuantity,
	"pct":          func(v float64) string { return fmt.Sprintf("%+.2f%%", v*100) },
	"money":        func(v float64) string { return fmt.Sprintf("%.2f", v) },
	"share":        func(v float64) string { return fmt.Sprintf("%.2f%%", v*100) },
	"signed":       report.SignedMoney,
	"verdictClass": verdictClass,
	"verdictMark":  verdictMark,
	"tone":         tone,
	"arrow":        arrow,
	"stateClass":   stateClass,
	"first":        func(e report.Equity) report.EquityPoint { return e.Points[0] },
	"last":         lastPoint,
	"before":       beforeLast,
	"count":        func(e report.Equity) int { return len(e.Points) },
	"chart":        equityChart,
	"strip":        dailyChangeStrip,
}).Parse(pageHTML))
