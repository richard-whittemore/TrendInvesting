package momentum_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// decideInput is a month with four universe members: two that score, one a
// year too young, one with a gap, plus an ETF and a $4 stock that never
// join, and SPY above its mean.
func decideInput() momentum.Input {
	cal := calendar(300)
	last := cal[len(cal)-1]
	closes := map[string]map[string]float64{
		"SPY": series(cal, trending(300, 1.001)),
		"AAA": series(cal, wobbling(300, 1.002, 0.01)),
		"BBB": series(cal, wobbling(300, 1.001, 0.02)),
		"YNG": series(cal, trending(100, 1.01)),
		"GAP": series(cal, trending(300, 1.003)),
	}
	delete(closes["GAP"], cal[280])
	return momentum.Input{
		Sessions: cal,
		Closes:   closes,
		Universe: momentum.Snapshot{Session: last, Rows: []momentum.Row{
			{InstrumentID: "AAA", Type: "CS", Close: 50, Volume: 1e6, Sector: "101", SectorSource: "gics-iwv"},
			{InstrumentID: "BBB", Type: "CS", Close: 40, Volume: 1e6, Sector: "102", SectorSource: "nasdaq"},
			{InstrumentID: "YNG", Type: "CS", Close: 30, Volume: 1e6, Sector: "103", SectorSource: "gics-iwv"},
			{InstrumentID: "GAP", Type: "CS", Close: 20, Volume: 1e6, Sector: "104", SectorSource: "gics-iwv"},
			{InstrumentID: "SPY", Type: "ETF", Close: 400, Volume: 1e8},
			{InstrumentID: "CHP", Type: "CS", Close: 4, Volume: 1e9, Sector: "105", SectorSource: "gics-iwv"},
		}},
	}
}

func TestDecideRecordsEveryRankedName(t *testing.T) {
	in := decideInput()
	d, err := momentum.Decide(momentum.FCombo(), in)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "outcome", d.Outcome, momentum.OutcomeDecided)
	mustEqual(t, "close session", d.CloseSession, in.Sessions[len(in.Sessions)-1])
	mustEqual(t, "risk-off", d.Trend.RiskOff, false)
	mustEqual(t, "universe", d.Universe, momentum.UniverseCounts{Rows: 6, CommonStock: 5, AtOrAboveFloor: 4, Members: 4})

	wantStatus := map[string]momentum.Status{
		"AAA": momentum.StatusPicked,
		"BBB": momentum.StatusPicked,
		"YNG": momentum.StatusShortHistory,
		"GAP": momentum.StatusGap,
	}
	if len(d.Names) != 4 {
		t.Fatalf("names = %+v", d.Names)
	}
	for i, n := range d.Names {
		mustEqual(t, n.InstrumentID+" universe rank", n.UniverseRank, i+1)
		mustEqual(t, n.InstrumentID+" status", n.Status, wantStatus[n.InstrumentID])
	}
	aaa, yng := d.Names[0], d.Names[2]
	mustEqual(t, "AAA sector source", aaa.SectorSource, "gics-iwv")
	mustEqual(t, "AAA dollar volume", aaa.DollarVolume, 5e7)
	mustEqual(t, "AAA score rank", aaa.ScoreRank, 1)
	if aaa.Weight <= 0 || aaa.Score <= 0 || aaa.Volatility <= 0 {
		t.Errorf("AAA = %+v", aaa)
	}
	mustEqual(t, "YNG closes", yng.Closes, 100)
	mustEqual(t, "YNG score rank", yng.ScoreRank, 0)

	sum := 0.0
	for _, tg := range d.Targets {
		sum += tg.Weight
	}
	near(t, "sum", sum, 0.98, 1e-15)
	mustEqual(t, "targets", len(d.Targets), 2)
}

func TestDecideRiskOff(t *testing.T) {
	in := decideInput()
	cal := in.Sessions
	prices := trending(300, 1.001)
	prices[299] = 50
	in.Closes["SPY"] = series(cal, prices)
	d, err := momentum.Decide(momentum.FCombo(), in)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "risk-off", d.Trend.RiskOff, true)
	mustEqual(t, "bond", d.Targets[len(d.Targets)-1], momentum.Target{InstrumentID: "IEF", Weight: 0.49})
}

func TestDecideWithholds(t *testing.T) {
	cases := map[string]struct {
		mutate func(*momentum.Input)
		want   momentum.WithheldReason
	}{
		"SPY under 200 closes": {
			func(in *momentum.Input) { in.Closes["SPY"] = series(in.Sessions, trending(199, 1.001)) },
			momentum.ReasonInsufficientTrendHistory,
		},
		"SPY missing its last bar": {
			func(in *momentum.Input) { delete(in.Closes["SPY"], in.Sessions[len(in.Sessions)-1]) },
			momentum.ReasonMissingBars,
		},
		"universe from another session": {
			func(in *momentum.Input) { in.Universe.Session = in.Sessions[len(in.Sessions)-2] },
			momentum.ReasonMissingUniverse,
		},
		"empty universe": {
			func(in *momentum.Input) { in.Universe.Rows = nil },
			momentum.ReasonMissingUniverse,
		},
		"nothing scores": {
			func(in *momentum.Input) {
				in.Closes["AAA"] = series(in.Sessions, trending(20, 1.001))
				in.Closes["BBB"] = series(in.Sessions, trending(20, 1.001))
			},
			momentum.ReasonNoEligibleNames,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in := decideInput()
			c.mutate(&in)
			d, err := momentum.Decide(momentum.FCombo(), in)
			if err != nil {
				t.Fatal(err)
			}
			mustEqual(t, "outcome", d.Outcome, momentum.OutcomeWithheld)
			mustEqual(t, "reason", d.Reason, c.want)
			mustEqual(t, "targets", len(d.Targets), 0)
		})
	}
}

func TestDecideRefusesMalformedInput(t *testing.T) {
	cases := map[string]func(*momentum.Input){
		"no sessions":        func(in *momentum.Input) { in.Sessions = nil },
		"sessions unordered": func(in *momentum.Input) { in.Sessions[3], in.Sessions[4] = in.Sessions[4], in.Sessions[3] },
		"session repeated":   func(in *momentum.Input) { in.Sessions[4] = in.Sessions[3] },
		"session not a date": func(in *momentum.Input) { in.Sessions[0] = "2001-13-01" },
		"bad close":          func(in *momentum.Input) { in.Closes["AAA"][in.Sessions[10]] = -1 },
		"bad SPY close":      func(in *momentum.Input) { in.Closes["SPY"][in.Sessions[10]] = 0 },
		"bad row":            func(in *momentum.Input) { in.Universe.Rows[0].Close = -1 },
		"bond as a member": func(in *momentum.Input) {
			in.Universe.Rows[0].InstrumentID = "IEF"
			in.Closes["IEF"] = in.Closes["AAA"]
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := decideInput()
			mutate(&in)
			if _, err := momentum.Decide(momentum.FCombo(), in); err == nil {
				t.Fatal("want an error")
			}
		})
	}
	cfg := momentum.FCombo()
	cfg.TopN = 0
	if _, err := momentum.Decide(cfg, decideInput()); err == nil {
		t.Fatal("bad config: want an error")
	}
}

func TestIsDecisionSession(t *testing.T) {
	cases := []struct {
		previous, session string
		want              bool
	}{
		{"2026-09-30", "2026-10-01", true},
		{"2026-10-01", "2026-10-02", false},
		{"2025-12-31", "2026-01-02", true},
	}
	for _, c := range cases {
		got, err := momentum.IsDecisionSession(c.previous, c.session)
		if err != nil {
			t.Fatal(err)
		}
		mustEqual(t, c.previous+"→"+c.session, got, c.want)
	}
	for _, bad := range [][2]string{{"x", "2026-10-01"}, {"2026-10-01", "y"}, {"2026-10-02", "2026-10-01"}} {
		if _, err := momentum.IsDecisionSession(bad[0], bad[1]); err == nil {
			t.Errorf("%v: want an error", bad)
		}
	}
}
