package parity_test

import (
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/parity"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// lotConfig is F_COMBO with a book of 3 and one pick per sector, small
// enough to value by hand.
func lotConfig() momentum.Config {
	c := momentum.FCombo()
	c.TopN, c.SectorCap = 3, 1
	return c
}

var lotVol = map[string]float64{"A": 0.02, "B": 0.05, "C": 0.04, "E": 0.01, "F": 0.03}

// lotWeights restates defensive_book's inverse-volatility weights
// (defensive_v2.py:76-80): 0.98 x (1/vol) / the sum over the picks, in
// pick order.
func lotWeights(picks ...string) map[string]float64 {
	total := 0.0
	for _, p := range picks {
		total += 1 / lotVol[p]
	}
	out := map[string]float64{}
	for _, p := range picks {
		out[p] = 0.98 * (1 / lotVol[p]) / total
	}
	return out
}

// lotMonth is a D7-L month at a $1,000 account: A, C and E picked (B capped
// behind A in sector X), every share $100 but F's $50.
func lotMonth(month string) parity.LotMonth {
	return parity.LotMonth{
		Month: month, Mode: "held", NLV: 1000,
		Ranked: []parity.LotEntry{
			{Ticker: "A", Score: 0.5, Volatility: 0.02, Sector: "311"},
			{Ticker: "B", Score: 0.45, Volatility: 0.05, Sector: "311"},
			{Ticker: "C", Score: 0.4, Volatility: 0.04, Sector: "205"},
			{Ticker: "E", Score: 0.2, Volatility: 0.01, Sector: "206"},
			{Ticker: "F", Score: 0.1, Volatility: 0.03, Sector: "101"},
		},
		Prices:  map[string]float64{"A": 100, "B": 100, "C": 100, "E": 100, "F": 50},
		Held:    map[string]int64{},
		Weights: lotWeights("A", "C", "E"),
	}
}

// lotMonths: a month with nothing skipped; one where C's $150 share
// exceeds its $140 target, so F takes its place; and one where C is held
// (4 shares), so it is kept.
func lotMonths() []parity.LotMonth {
	plain := lotMonth("2020-03")
	skip := lotMonth("2020-04")
	skip.Prices["C"] = 150
	skip.Skipped, skip.Weights = []string{"C"}, lotWeights("A", "E", "F")
	kept := lotMonth("2020-05")
	kept.Prices["C"], kept.Held["C"] = 150, 4
	kept.Kept = []string{"C"}
	return []parity.LotMonth{plain, skip, kept}
}

var lotPlan = []string{"2020-03"}

func compareLot(ms []parity.LotMonth) parity.Report {
	return parity.CompareMinimumLot(lotConfig(), "L", lotPlan, ms)
}

func TestAMatchingLotExportPassesEveryCheck(t *testing.T) {
	r := compareLot(lotMonths())
	if !r.Pass() {
		t.Fatalf("matching lot export failed:\n%s", r.Markdown())
	}
	want := map[string]int{
		parity.CheckLotMonths: 1, parity.CheckLotExport: 3, parity.CheckLotPicks: 3 * (1 + 3),
		parity.CheckLotSkipped: 3, parity.CheckLotKept: 3, parity.CheckLotWeights: 3 + 3 + 3,
		parity.CheckDifferences: 0,
	}
	for name, n := range want {
		if c := checkNamed(r, name); c.Compared != n {
			t.Errorf("%s compared %d, want %d", name, c.Compared, n)
		}
	}
}

func lotFailsOnly(t *testing.T, ms []parity.LotMonth, names ...string) {
	t.Helper()
	r := compareLot(ms)
	failing := map[string]bool{}
	for _, n := range names {
		failing[n] = true
	}
	for _, c := range r.Checks {
		if c.Pass() == failing[c.Name] {
			t.Errorf("%s: pass=%v, want %v (%s)", c.Name, c.Pass(), !failing[c.Name], c.First)
		}
	}
}

func TestAPickGoSkippedFailsPicksSkippedAndWeights(t *testing.T) {
	// QuantConnect kept C in the book at its $150 share.
	ms := lotMonths()
	ms[1].Skipped, ms[1].Weights = nil, lotWeights("A", "C", "E")
	lotFailsOnly(t, ms, parity.CheckLotPicks, parity.CheckLotSkipped, parity.CheckLotWeights)
}

func TestAHeldNameSkippedWhereGoKeepsItFails(t *testing.T) {
	ms := lotMonths()
	ms[2].Kept, ms[2].Skipped, ms[2].Weights = nil, []string{"C"}, lotWeights("A", "E", "F")
	lotFailsOnly(t, ms, parity.CheckLotPicks, parity.CheckLotSkipped, parity.CheckLotKept, parity.CheckLotWeights)
}

func TestTheLiteralRuleSkipsAHeldName(t *testing.T) {
	ms := lotMonths()
	ms[2].Mode, ms[2].Kept, ms[2].Skipped, ms[2].Weights = "literal", nil, []string{"C"}, lotWeights("A", "E", "F")
	if r := compareLot(ms); !r.Pass() {
		t.Fatalf("literal month failed:\n%s", r.Markdown())
	}
}

func TestALotWeightOffByMoreThanTheToleranceFails(t *testing.T) {
	ms := lotMonths()
	ms[1].Weights["F"] += 1e-11
	lotFailsOnly(t, ms, parity.CheckLotWeights)
}

func TestAWithheldMonthMustBeGosToo(t *testing.T) {
	ms := lotMonths()
	all := lotMonth("2020-06")
	for k := range all.Prices {
		all.Prices[k] = 5000
	}
	all.Weights, all.Withheld = nil, true
	all.Skipped = []string{"A", "C", "E", "F", "B"}
	ms = append(ms, all)
	if r := compareLot(ms); !r.Pass() {
		t.Fatalf("withheld month failed:\n%s", r.Markdown())
	}
	ms[3].Prices["F"] = 50
	if checkNamed(compareLot(ms), parity.CheckLotPicks).Pass() {
		t.Error("QuantConnect withheld a month in which Go picks F")
	}
}

func TestALotMonthGoCannotValueFailsTheExport(t *testing.T) {
	for name, alter := range map[string]func(*parity.LotMonth){
		"no price":         func(m *parity.LotMonth) { delete(m.Prices, "E") },
		"no net liq":       func(m *parity.LotMonth) { m.NLV = 0 },
		"unknown mode":     func(m *parity.LotMonth) { m.Mode = "none" },
		"ranked twice":     func(m *parity.LotMonth) { m.Ranked = append(m.Ranked, m.Ranked[0]) },
		"held none":        func(m *parity.LotMonth) { m.Held["A"] = 0 },
		"weights withheld": func(m *parity.LotMonth) { m.Withheld = true },
	} {
		t.Run(name, func(t *testing.T) {
			ms := lotMonths()
			alter(&ms[0])
			if c := checkNamed(compareLot(ms), parity.CheckLotExport); c.Pass() {
				t.Errorf("export passed: %+v", c)
			}
		})
	}
}

func TestAMissingPlannedLotMonthFails(t *testing.T) {
	ms := lotMonths()
	lotFailsOnly(t, ms[1:], parity.CheckLotMonths)
}

func TestATieAndAZeroVolatilityInALotMonthAreListedDifferences(t *testing.T) {
	ms := lotMonths()
	ms[0].Ranked[4].Score = ms[0].Ranked[3].Score // E and F tie; Go orders them by ticker, as listed
	r := compareLot(ms)
	if !r.Pass() || len(r.Differences) != 1 || r.Differences[0].Kind != parity.KindTie {
		t.Fatalf("tie: %+v\n%s", r.Differences, r.Markdown())
	}
	ms = lotMonths()
	ms[0].Ranked[1].Volatility = 0 // B, capped
	r = compareLot(ms)
	if len(r.Differences) != 1 || r.Differences[0].Kind != parity.KindZeroVolatility ||
		!strings.Contains(r.Differences[0].Where, "B") {
		t.Fatalf("zero volatility: %+v", r.Differences)
	}
}

func TestALotPayloadDecodesExactly(t *testing.T) {
	raw := `{"month":"2020-04","mode":"held","nlv":30000.5,"risk_off":true,` +
		`"ranked":[["B",0.6,0.02,null],["A",0.5,0.01,311]],"prices":{"A":120.5,"B":10.0},"held":{"A":3},` +
		`"weights":{"B":0.49,"IEF":0.49},"skipped":["C"],"kept":["A"]}`
	m, err := parity.DecodeLot([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.Month != "2020-04" || m.Mode != "held" || m.NLV != 30000.5 || !m.RiskOff || len(m.Ranked) != 2 ||
		m.Ranked[0] != (parity.LotEntry{Ticker: "B", Score: 0.6, Volatility: 0.02}) ||
		m.Ranked[1].Sector != "311" || m.Prices["A"] != 120.5 || m.Held["A"] != 3 || m.Weights["IEF"] != 0.49 ||
		m.Withheld || m.Skipped[0] != "C" || m.Kept[0] != "A" {
		t.Errorf("decoded %+v", m)
	}
	w, err := parity.DecodeLot([]byte(strings.Replace(raw, `"weights":{"B":0.49,"IEF":0.49}`, `"weights":null`, 1)))
	if err != nil || !w.Withheld || w.Weights != nil {
		t.Errorf("withheld: %+v %v", w, err)
	}
}

func TestAMalformedLotPayloadIsRefused(t *testing.T) {
	good := `{"month":"2020-04","mode":"held","nlv":1.0,"risk_off":false,"ranked":[["A",0.5,0.01,311]],` +
		`"prices":{"A":1.0},"held":{},"weights":{"A":0.98},"skipped":[],"kept":[]}`
	for name, raw := range map[string]string{
		"unknown field":  strings.Replace(good, `"nlv":`, `"x":1,"nlv":`, 1),
		"no nlv":         strings.Replace(good, `"nlv":1.0,`, ``, 1),
		"short row":      strings.Replace(good, `["A",0.5,0.01,311]`, `["A",0.5,0.01]`, 1),
		"null score":     strings.Replace(good, `["A",0.5,`, `["A",null,`, 1),
		"empty ticker":   strings.Replace(good, `["A",0.5,`, `["",0.5,`, 1),
		"part sector":    strings.Replace(good, `0.01,311]`, `0.01,311.5]`, 1),
		"null price":     strings.Replace(good, `"prices":{"A":1.0}`, `"prices":{"A":null}`, 1),
		"no held":        strings.Replace(good, `"held":{},`, ``, 1),
		"no kept":        strings.Replace(good, `,"kept":[]`, ``, 1),
		"no month":       strings.Replace(good, `"month":"2020-04",`, ``, 1),
		"no weights key": strings.Replace(good, `"weights":{"A":0.98},`, ``, 1),
		"null weight":    strings.Replace(good, `"weights":{"A":0.98}`, `"weights":{"A":null}`, 1),
		"weights a list": strings.Replace(good, `"weights":{"A":0.98}`, `"weights":[0.98]`, 1),
		"null holding":   strings.Replace(good, `"held":{}`, `"held":{"A":null}`, 1),
		"number ticker":  strings.Replace(good, `["A",0.5,`, `[1,0.5,`, 1),
	} {
		if _, err := parity.DecodeLot([]byte(raw)); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

func TestTheMonthsInWhichTheExceptionFiredAreReportedApart(t *testing.T) {
	// Three months compared; C skipped in one, kept in another.
	r := compareLot(lotMonths())
	for _, name := range []string{parity.CheckLotSkipped, parity.CheckLotKept} {
		if c := checkNamed(r, name); c.Compared != 3 || c.Fired != 1 || !c.CountsFired {
			t.Errorf("%s: %+v", name, c)
		}
	}
	for _, want := range []string{"- L d7l-skipped: fired in 1 of the 3 months compared",
		"- L d7l-kept: fired in 1 of the 3 months compared"} {
		if !strings.Contains(r.Markdown(), want) {
			t.Errorf("report lacks %q:\n%s", want, r.Markdown())
		}
	}
	// A month in which only one side names a name fired too.
	ms := lotMonths()
	ms[0].Skipped = []string{"F"}
	if c := checkNamed(compareLot(ms), parity.CheckLotSkipped); c.Fired != 2 || c.Pass() {
		t.Errorf("one-sided: %+v", c)
	}
}
