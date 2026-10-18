package momentum_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// The backtest-compatible functions reproduce defensive_v2.py's handling of
// the cases live trading departs from, so the parity comparison can match
// QuantConnect's export exactly and list each live departure apart.

func TestBacktestBookKeepsTiesInInputOrderAndLeavesNoSectorUncapped(t *testing.T) {
	book, err := momentum.BacktestBook(smallBook(), capCandidates(), false)
	if err != nil {
		t.Fatal(err)
	}
	// Stable by score: C before B as given; D has no sector and is picked,
	// counting toward no sector's cap; the book is then full.
	want := []struct {
		id     string
		status momentum.Status
	}{
		{"A", momentum.StatusPicked}, {"C", momentum.StatusPicked}, {"B", momentum.StatusSectorCapped},
		{"D", momentum.StatusPicked}, {"G", momentum.StatusBeyondTopN}, {"E", momentum.StatusBeyondTopN},
		{"F", momentum.StatusBeyondTopN},
	}
	if len(book.Ranked) != len(want) {
		t.Fatalf("ranked %d names", len(book.Ranked))
	}
	for i, w := range want {
		r := book.Ranked[i]
		if r.InstrumentID != w.id || r.Status != w.status || r.ScoreRank != i+1 ||
			r.BacktestWouldPick != (w.status == momentum.StatusPicked) {
			t.Errorf("rank %d: %+v, want %s %s", i+1, r, w.id, w.status)
		}
	}
	total := 1/0.02 + 1/0.04 + 1/0.02
	wantTargets := []momentum.Target{
		{InstrumentID: "A", Weight: 0.98 * (1 / 0.02) / total},
		{InstrumentID: "C", Weight: 0.98 * (1 / 0.04) / total},
		{InstrumentID: "D", Weight: 0.98 * (1 / 0.02) / total},
	}
	if len(book.Targets) != len(wantTargets) {
		t.Fatalf("targets %+v", book.Targets)
	}
	for i, w := range wantTargets {
		if book.Targets[i] != w {
			t.Errorf("target %d: %+v, want %+v", i, book.Targets[i], w)
		}
	}
}

func TestBacktestBookWeightsAZeroVolatilityAsOne(t *testing.T) {
	cands := []momentum.Candidate{
		{InstrumentID: "A", Sector: "X", Score: 0.5, Volatility: 0},
		{InstrumentID: "B", Sector: "Y", Score: 0.4, Volatility: 0.5},
	}
	book, err := momentum.BacktestBook(smallBook(), cands, true)
	if err != nil {
		t.Fatal(err)
	}
	part := float64(0.98 * (1 - 0.5))
	want := []momentum.Target{
		{InstrumentID: "A", Weight: part * 1 / (1 + 1/0.5)},
		{InstrumentID: "B", Weight: part * (1 / 0.5) / (1 + 1/0.5)},
		{InstrumentID: "IEF", Weight: 0.98 - part},
	}
	if len(book.Targets) != len(want) {
		t.Fatalf("targets %+v", book.Targets)
	}
	for i, w := range want {
		if book.Targets[i] != w {
			t.Errorf("target %d: %+v, want %+v", i, book.Targets[i], w)
		}
	}
	if book.Ranked[0].Status != momentum.StatusPicked {
		t.Errorf("a zero volatility is picked as defensive_book picked it: %+v", book.Ranked[0])
	}
}

func TestBacktestBookWithNoMomentumPartPutsItAllInTheBond(t *testing.T) {
	cfg := smallBook()
	cfg.RiskOffFraction = 1
	book, err := momentum.BacktestBook(cfg, capCandidates(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(book.Targets) != 1 || book.Targets[0] != (momentum.Target{InstrumentID: "IEF", Weight: 0.98}) {
		t.Errorf("targets %+v", book.Targets)
	}
	empty, err := momentum.BacktestBook(smallBook(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Targets) != 1 || empty.Targets[0] != (momentum.Target{InstrumentID: "IEF", Weight: 0.98}) {
		t.Errorf("no pick: targets %+v", empty.Targets)
	}
}

func TestBacktestBookRefusesWhatSelectBookRefuses(t *testing.T) {
	bad := smallBook()
	bad.TopN = 0
	if _, err := momentum.BacktestBook(bad, capCandidates(), false); err == nil {
		t.Error("an invalid configuration must be refused")
	}
	for name, cands := range map[string][]momentum.Candidate{
		"repeated": {{InstrumentID: "A", Sector: "X", Score: 1, Volatility: 1}, {InstrumentID: "A", Sector: "X", Score: 1, Volatility: 1}},
		"invalid":  {{InstrumentID: "", Sector: "X", Score: 1, Volatility: 1}},
	} {
		if _, err := momentum.BacktestBook(smallBook(), cands, false); err == nil {
			t.Errorf("%s candidates must be refused", name)
		}
	}
}

func TestBacktestBookChangesNoLiveDecision(t *testing.T) {
	cands := capCandidates()
	before, err := momentum.SelectBook(smallBook(), cands, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := momentum.BacktestBook(smallBook(), cands, false); err != nil {
		t.Fatal(err)
	}
	after, err := momentum.SelectBook(smallBook(), cands, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := range before.Ranked {
		if before.Ranked[i] != after.Ranked[i] {
			t.Errorf("rank %d changed: %+v then %+v", i+1, before.Ranked[i], after.Ranked[i])
		}
	}
	if cands[0].InstrumentID != "F" {
		t.Error("the candidates must not be reordered in place")
	}
}

func TestBacktestScoreShiftsItsWindowAcrossAGap(t *testing.T) {
	cfg := momentum.FCombo()
	cal := calendar(300)
	full := wobbling(300, 1.001, 0.01)
	closes := series(cal, full)
	gapped := series(cal, full)
	delete(gapped, cal[290])
	live, err := momentum.Score(cfg, cal, gapped)
	if err != nil || live.Status != momentum.StatusGap {
		t.Fatalf("live leaves the gapped name out: %+v %v", live, err)
	}
	h, err := momentum.BacktestScore(cfg, cal, gapped)
	if err != nil {
		t.Fatal(err)
	}
	// The last 253 closes it has: sessions 46..299 without 290.
	var px []float64
	for i, p := range full {
		if i != 290 {
			px = append(px, p)
		}
	}
	ref, err := momentum.Score(cfg, calendar(299), series(calendar(299), px))
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != momentum.StatusScored || h.Closes != 299 || h.Score != ref.Score || h.Volatility != ref.Volatility {
		t.Errorf("backtest score %+v, want %+v over the closes it has", h, ref)
	}
	same, err := momentum.BacktestScore(cfg, cal, closes)
	want, _ := momentum.Score(cfg, cal, closes)
	if err != nil || same != want {
		t.Errorf("without a gap it is Score: %+v %+v %v", same, want, err)
	}
	short, err := momentum.BacktestScore(cfg, cal, series(cal, full[:252]))
	if err != nil || short.Status != momentum.StatusShortHistory || short.Closes != 252 {
		t.Errorf("252 closes is short: %+v %v", short, err)
	}
	if _, err := momentum.BacktestScore(cfg, cal, map[string]float64{"2001-01-01": -1}); err == nil {
		t.Error("a malformed close must be refused")
	}
}

func TestBacktestTrendTreatsAShortHistoryAsRiskOnAndDropsMissingCloses(t *testing.T) {
	cfg := momentum.FCombo()
	cal := calendar(260)
	falling := make([]float64, 150)
	for i := range falling {
		falling[i] = float64(300 - i)
	}
	tr, err := momentum.BacktestTrend(cfg, cal, series(cal, falling))
	if err != nil {
		t.Fatal(err)
	}
	sum := 0.0
	for _, p := range falling {
		sum += p
	}
	if tr.RiskOff || tr.Closes != 150 || tr.Close != 151 || tr.Mean != sum/150 {
		t.Errorf("fewer than 200 closes is risk-on, mean of what it has: %+v", tr)
	}
	prices := trending(260, 1.001)
	gapped := series(cal, prices)
	delete(gapped, cal[250])
	tr, err = momentum.BacktestTrend(cfg, cal, gapped)
	if err != nil {
		t.Fatal(err)
	}
	var window []float64
	for i := 59; i < 260; i++ {
		if i != 250 {
			window = append(window, prices[i])
		}
	}
	sum = 0
	for _, p := range window {
		sum += p
	}
	if tr.Closes != 259 || tr.Close != prices[259] || tr.Mean != sum/200 || tr.RiskOff {
		t.Errorf("the last 200 closes it has: %+v, mean want %v", tr, sum/200)
	}
	full, err := momentum.BacktestTrend(cfg, cal, series(cal, prices))
	live, _, _ := momentum.EvaluateTrend(cfg, cal, series(cal, prices))
	if err != nil || full != live {
		t.Errorf("with a full window it is EvaluateTrend: %+v %+v %v", full, live, err)
	}
	none, err := momentum.BacktestTrend(cfg, cal, map[string]float64{})
	if err != nil || none != (momentum.Trend{}) {
		t.Errorf("no close: %+v %v", none, err)
	}
	if _, err := momentum.BacktestTrend(cfg, nil, nil); err == nil {
		t.Error("an empty calendar must be refused")
	}
	bad := cfg
	bad.TrendCloses = 0
	if _, err := momentum.BacktestTrend(bad, cal, gapped); err == nil {
		t.Error("an invalid configuration must be refused")
	}
}
