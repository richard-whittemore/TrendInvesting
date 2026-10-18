package momentum_test

import (
	"slices"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// name returns the decision's record of id.
func name(t *testing.T, d momentum.Decision, id string) momentum.NameRecord {
	t.Helper()
	for _, n := range d.Names {
		if n.InstrumentID == id {
			return n
		}
	}
	t.Fatalf("no record of %s in %+v", id, d.Names)
	return momentum.NameRecord{}
}

// TestDecideScoresAHeldNameAcrossItsGapAsTheBacktestDoes encodes the owner's
// decision of 2026-10-18 (ADR 0026 point 5, amended): a held name whose
// window has a gap is scored, and its volatility computed, from the last
// ScoreLookback+1 closes it has, across the gap, as defensive_v2.py:286-291
// did (BacktestScore), so it is kept or sold on its merits; the sessions it
// is missing are recorded. Held or not, a name with a gap names its missing
// sessions.
func TestDecideScoresAHeldNameAcrossItsGapAsTheBacktestDoes(t *testing.T) {
	cfg := momentum.FCombo()
	in := decideInput()
	missing := in.Sessions[280]

	unheld, err := momentum.Decide(cfg, in)
	if err != nil {
		t.Fatal(err)
	}
	gap := name(t, unheld, "GAP")
	mustEqual(t, "unheld status", gap.Status, momentum.StatusGap)
	mustEqual(t, "unheld score rank", gap.ScoreRank, 0)
	if !slices.Equal(gap.GapSessions, []string{missing}) {
		t.Errorf("unheld gap sessions = %v, want [%s]", gap.GapSessions, missing)
	}

	in.Held = map[string]bool{"GAP": true}
	held, err := momentum.Decide(cfg, in)
	if err != nil {
		t.Fatal(err)
	}
	want, err := momentum.BacktestScore(cfg, in.Sessions, in.Closes["GAP"])
	if err != nil {
		t.Fatal(err)
	}
	gap = name(t, held, "GAP")
	mustEqual(t, "held status", gap.Status, momentum.StatusPicked)
	mustEqual(t, "held score", gap.Score, want.Score)
	mustEqual(t, "held volatility", gap.Volatility, want.Volatility)
	if gap.Weight <= 0 {
		t.Errorf("held GAP has no weight: %+v", gap)
	}
	if !slices.Equal(gap.GapSessions, []string{missing}) {
		t.Errorf("held gap sessions = %v, want [%s]", gap.GapSessions, missing)
	}
	if !slices.ContainsFunc(held.Targets, func(tg momentum.Target) bool { return tg.InstrumentID == "GAP" }) {
		t.Errorf("held GAP is not targeted: %+v", held.Targets)
	}
	for _, id := range []string{"AAA", "BBB"} {
		if n := name(t, held, id); len(n.GapSessions) != 0 {
			t.Errorf("%s with a complete window records gap sessions %v", id, n.GapSessions)
		}
	}
}

// TestAHeldNameWithTooFewClosesIsStillShortHistory: holding a name does not
// score it from fewer than ScoreLookback+1 closes, which the backtest left
// out too (defensive_v2.py:287-288).
func TestAHeldNameWithTooFewClosesIsStillShortHistory(t *testing.T) {
	in := decideInput()
	delete(in.Closes["YNG"], in.Sessions[250])
	in.Held = map[string]bool{"YNG": true}
	d, err := momentum.Decide(momentum.FCombo(), in)
	if err != nil {
		t.Fatal(err)
	}
	yng := name(t, d, "YNG")
	mustEqual(t, "status", yng.Status, momentum.StatusShortHistory)
	mustEqual(t, "closes", yng.Closes, 99)
	if len(yng.GapSessions) != 0 {
		t.Errorf("a short history records gap sessions %v", yng.GapSessions)
	}
}

// TestAHeldNameWithAGapBeforeItsWindowIsScoredAsUsual: a gap the live
// window does not reach changes nothing, held or not.
func TestAHeldNameWithAGapBeforeItsWindowIsScoredAsUsual(t *testing.T) {
	cfg := momentum.FCombo()
	in := decideInput()
	in.Closes["GAP"] = series(in.Sessions, wobbling(300, 1.003, 0.015))
	delete(in.Closes["GAP"], in.Sessions[10])
	plain, err := momentum.Decide(cfg, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Held = map[string]bool{"GAP": true}
	held, err := momentum.Decide(cfg, in)
	if err != nil {
		t.Fatal(err)
	}
	a, b := name(t, plain, "GAP"), name(t, held, "GAP")
	mustEqual(t, "status", b.Status, momentum.StatusPicked)
	mustEqual(t, "score", b.Score, a.Score)
	if len(b.GapSessions) != 0 {
		t.Errorf("gap sessions %v outside the window", b.GapSessions)
	}
}

// TestAHeldNameMissingMoreSessionsThanItsHistorySpanHoldsIsShortHistory:
// across the gap, the closes read are those of the last
// BacktestHistorySessions sessions, the history the backtest requested
// (defensive_v2.py:271); a name with fewer than ScoreLookback+1 of them is
// short of history, held or not.
func TestAHeldNameMissingMoreSessionsThanItsHistorySpanHoldsIsShortHistory(t *testing.T) {
	cfg := momentum.FCombo()
	in := decideInput()
	span := in.Sessions[len(in.Sessions)-momentum.BacktestHistorySessions:]
	slack := momentum.BacktestHistorySessions - (cfg.ScoreLookback + 1)
	for _, s := range span[100 : 100+slack+1] {
		delete(in.Closes["GAP"], s)
	}
	in.Held = map[string]bool{"GAP": true}
	d, err := momentum.Decide(cfg, in)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "status", name(t, d, "GAP").Status, momentum.StatusShortHistory)
}
