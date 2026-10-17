package momentum_test

import (
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// smallBook is F_COMBO with three picks and one per sector, so a test can
// exercise the cap, ties and exclusions with a handful of names.
func smallBook() momentum.Config {
	cfg := momentum.FCombo()
	cfg.TopN = 3
	cfg.SectorCap = 1
	return cfg
}

func capCandidates() []momentum.Candidate {
	return []momentum.Candidate{
		{InstrumentID: "F", Sector: "W", Score: 0.1, Volatility: 0.03},
		{InstrumentID: "C", Sector: "Y", Score: 0.4, Volatility: 0.04}, // ties B; C after B by ID
		{InstrumentID: "B", Sector: "X", Score: 0.4, Volatility: 0.05}, // X already holds A: capped
		{InstrumentID: "A", Sector: "X", Score: 0.5, Volatility: 0.02},
		{InstrumentID: "D", Sector: "", Score: 0.3, Volatility: 0.02},  // no label: left out
		{InstrumentID: "G", Sector: "V", Score: 0.25, Volatility: 0},   // zero volatility: left out
		{InstrumentID: "E", Sector: "Z", Score: 0.2, Volatility: 0.01}, // third pick
	}
}

func TestSelectBookRanksCapsAndExcludes(t *testing.T) {
	book, err := momentum.SelectBook(smallBook(), capCandidates(), false)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		id       string
		rank     int
		status   momentum.Status
		backtest bool
	}
	want := []row{
		{"A", 1, momentum.StatusPicked, true},
		{"B", 2, momentum.StatusSectorCapped, false},
		{"C", 3, momentum.StatusPicked, true},
		{"D", 4, momentum.StatusNoSector, true},        // uncapped in the backtest: its third pick
		{"G", 5, momentum.StatusZeroVolatility, false}, // the backtest's book is full by then
		{"E", 6, momentum.StatusPicked, false},
		{"F", 7, momentum.StatusBeyondTopN, false},
	}
	if len(book.Ranked) != len(want) {
		t.Fatalf("ranked = %+v", book.Ranked)
	}
	for i, w := range want {
		got := book.Ranked[i]
		mustEqual(t, "id", got.InstrumentID, w.id)
		mustEqual(t, w.id+" rank", got.ScoreRank, w.rank)
		mustEqual(t, w.id+" status", got.Status, w.status)
		mustEqual(t, w.id+" backtest pick", got.BacktestWouldPick, w.backtest)
	}
	// Inverse volatility over A, C, E: 50, 25 and 100 of 175, of 0.98.
	weights := map[string]float64{}
	for _, tg := range book.Targets {
		weights[tg.InstrumentID] = tg.Weight
	}
	near(t, "A", weights["A"], 0.28, 1e-15)
	near(t, "C", weights["C"], 0.14, 1e-15)
	near(t, "E", weights["E"], 0.56, 1e-15)
	mustEqual(t, "targets", len(book.Targets), 3)
	mustEqual(t, "target order", book.Targets[0].InstrumentID+book.Targets[1].InstrumentID+book.Targets[2].InstrumentID, "ACE")
}

func TestSelectBookRiskOffMovesHalfToTheBond(t *testing.T) {
	book, err := momentum.SelectBook(smallBook(), capCandidates(), true)
	if err != nil {
		t.Fatal(err)
	}
	weights := map[string]float64{}
	for _, tg := range book.Targets {
		weights[tg.InstrumentID] = tg.Weight
	}
	near(t, "A", weights["A"], 0.14, 1e-15)
	near(t, "IEF", weights["IEF"], 0.49, 1e-15)
	mustEqual(t, "targets", len(book.Targets), 4)
	mustEqual(t, "bond last", book.Targets[3].InstrumentID, "IEF")
}

func TestSelectBookWeightsSumTo98Percent(t *testing.T) {
	cfg := momentum.FCombo()
	var cands []momentum.Candidate
	for i, id := range ids(60, "N") {
		cands = append(cands, momentum.Candidate{
			InstrumentID: id,
			Sector:       string(rune('a' + i%11)),
			Score:        float64(i) / 7,
			Volatility:   0.01 + float64(i%13)/997,
		})
	}
	for _, riskOff := range []bool{false, true} {
		book, err := momentum.SelectBook(cfg, cands, riskOff)
		if err != nil {
			t.Fatal(err)
		}
		picks, sum := 0, 0.0
		for _, tg := range book.Targets {
			sum += tg.Weight
			if tg.InstrumentID != "IEF" {
				picks++
			}
		}
		mustEqual(t, "picks", picks, 25)
		near(t, "sum", sum, 0.98, 1e-15)
		perSector := map[string]int{}
		for _, r := range book.Ranked {
			if r.Status == momentum.StatusPicked {
				perSector[r.Sector]++
			}
		}
		for s, n := range perSector {
			if n > 3 {
				t.Errorf("sector %s holds %d", s, n)
			}
		}
	}
}

func TestSelectBookRefusesMalformedCandidates(t *testing.T) {
	cases := map[string]momentum.Candidate{
		"empty id":        {Sector: "X", Score: 0.1, Volatility: 0.1},
		"NaN score":       {InstrumentID: "A", Sector: "X", Score: math.NaN(), Volatility: 0.1},
		"negative vol":    {InstrumentID: "A", Sector: "X", Score: 0.1, Volatility: -0.1},
		"infinite vol":    {InstrumentID: "A", Sector: "X", Score: 0.1, Volatility: math.Inf(1)},
		"the bond itself": {InstrumentID: "IEF", Sector: "X", Score: 0.1, Volatility: 0.1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := momentum.SelectBook(smallBook(), []momentum.Candidate{c}, false); err == nil {
				t.Fatal("want an error")
			}
		})
	}
	dup := momentum.Candidate{InstrumentID: "A", Sector: "X", Score: 0.1, Volatility: 0.1}
	if _, err := momentum.SelectBook(smallBook(), []momentum.Candidate{dup, dup}, false); err == nil {
		t.Fatal("duplicate: want an error")
	}
}

func TestConfigValidate(t *testing.T) {
	if err := momentum.FCombo().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*momentum.Config){
		"universe":       func(c *momentum.Config) { c.UniverseSize = 0 },
		"floor":          func(c *momentum.Config) { c.MinPrice = math.NaN() },
		"lookback":       func(c *momentum.Config) { c.ScoreLookback = 0 },
		"skip":           func(c *momentum.Config) { c.ScoreSkip = 252 },
		"negative skip":  func(c *momentum.Config) { c.ScoreSkip = -1 },
		"vol returns":    func(c *momentum.Config) { c.VolatilityReturns = 253 },
		"no vol returns": func(c *momentum.Config) { c.VolatilityReturns = 1 },
		"top":            func(c *momentum.Config) { c.TopN = 0 },
		"cap":            func(c *momentum.Config) { c.SectorCap = 0 },
		"invested":       func(c *momentum.Config) { c.Invested = 1.01 },
		"fraction":       func(c *momentum.Config) { c.RiskOffFraction = -0.1 },
		"trend":          func(c *momentum.Config) { c.TrendCloses = 0 },
		"trend id":       func(c *momentum.Config) { c.TrendInstrument = "" },
		"bond id":        func(c *momentum.Config) { c.RiskOffInstrument = "" },
		"same ids":       func(c *momentum.Config) { c.RiskOffInstrument = c.TrendInstrument },
		"type":           func(c *momentum.Config) { c.ListingType = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := momentum.FCombo()
			mutate(&cfg)
			if cfg.Validate() == nil {
				t.Fatal("want an error")
			}
			if _, err := momentum.SelectBook(cfg, nil, false); err == nil {
				t.Fatal("SelectBook: want an error")
			}
		})
	}
}
