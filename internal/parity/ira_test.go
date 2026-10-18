package parity_test

import (
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/parity"
)

// iraMonth is internal/orderplan's TestIRAEntriesExitsAndSellsFirstInInstrumentOrder
// as an IRA run's export: CCC sold at the rebalance, the buys a session
// later scaled to the $7,500 the sale left (worst case 7,671 with $2 a
// buy). The orders are what the run placed.
func iraMonth() (parity.IRAMonth, []parity.Order) {
	scale := 7500.0 / 7671.0
	m := parity.IRAMonth{
		Month: "2020-04", Decision: "2020-04-01", BuySession: "2020-04-02", Delay: 1, Waited: 1,
		NLV: 10000, Band: 0.25, PriceBand: 0.05, MaxFees: 2, Budget: 7500, Scale: scale,
		Targets: map[string]float64{"AAA": 0.25, "BBB": 0.24, "DDD": 0.25, "IEF": 0.24},
		Held:    map[string]int64{"CCC": 40, "DDD": 100},
		Prices:  map[string]float64{"AAA": 50, "BBB": 30, "CCC": 20, "DDD": 25, "IEF": 100},
		Working: map[string]int64{},
		Sells:   []parity.IRAOrder{{Ticker: "CCC", Shares: 40, Limit: 19}},
		Buys:    []parity.IRAOrder{{Ticker: "AAA", Shares: 50, Limit: 52.5}, {Ticker: "BBB", Shares: 80, Limit: 31.5}, {Ticker: "IEF", Shares: 24, Limit: 105}},
		Kept:    []parity.IRAKept{{Ticker: "AAA", Shares: 48}, {Ticker: "BBB", Shares: 78}, {Ticker: "IEF", Shares: 23}},
	}
	orders := []parity.Order{
		{ID: 1, Ticker: "CCC", Quantity: -40, Status: parity.OrderFilled, Time: "2020-04-01T13:00:00Z", LastFillTime: "2020-04-01T20:00:00Z", Tag: "IRAORD1 2020-04 sell"},
		{ID: 2, Ticker: "AAA", Quantity: 48, Status: parity.OrderFilled, Time: "2020-04-02T13:00:00Z", LastFillTime: "2020-04-02T20:00:00Z", Tag: "IRAORD1 2020-04 buy"},
		{ID: 3, Ticker: "BBB", Quantity: 78, Status: parity.OrderFilled, Time: "2020-04-02T13:00:00Z", LastFillTime: "2020-04-02T20:00:00Z", Tag: "IRAORD1 2020-04 buy"},
		{ID: 4, Ticker: "IEF", Quantity: 23, Status: parity.OrderFilled, Time: "2020-04-02T13:00:00Z", LastFillTime: "2020-04-02T20:00:00Z", Tag: "IRAORD1 2020-04 buy"},
		{ID: 5, Ticker: "SPY", Quantity: 1, Status: 5, Time: "2020-04-02T13:00:00Z", Tag: "IRAPARITY1 {}"},
	}
	return m, orders
}

// noSellMonth is an account holding only cash, as in
// TestIRAPlanWithNoSellsHasOnlyBuys, so the buys go at the rebalance; AAA
// rounds 0.001 x 1,000 / 7 to no share, as in TestIRARoundsTowardZero.
func noSellMonth() (parity.IRAMonth, []parity.Order) {
	m := parity.IRAMonth{
		Month: "2020-05", Decision: "2020-05-01", BuySession: "2020-05-01", Delay: 1,
		NLV: 1000, Band: 0.25, PriceBand: 0.05, MaxFees: 2, Budget: 1000, Scale: 1,
		Targets: map[string]float64{"AAA": 0.001, "BBB": 0.45, "IEF": 0.45},
		Held:    map[string]int64{}, Working: map[string]int64{},
		Prices:  map[string]float64{"AAA": 7, "BBB": 7, "IEF": 98},
		Buys:    []parity.IRAOrder{{Ticker: "BBB", Shares: 64, Limit: 7.35}, {Ticker: "IEF", Shares: 4, Limit: 102.9}},
		Skipped: []parity.IRASkip{{Ticker: "AAA", Side: "buy", Reason: "below-one-share", Shares: float64(0.001*1000) / 7}},
		Kept:    []parity.IRAKept{{Ticker: "BBB", Shares: 64}, {Ticker: "IEF", Shares: 4}},
	}
	orders := []parity.Order{
		{ID: 6, Ticker: "BBB", Quantity: 64, Status: parity.OrderFilled, Time: "2020-05-01T13:00:00Z", LastFillTime: "2020-05-01T20:00:00Z", Tag: "IRAORD1 2020-05 buy"},
		{ID: 7, Ticker: "IEF", Quantity: 4, Status: parity.OrderFilled, Time: "2020-05-01T13:00:00Z", LastFillTime: "2020-05-01T20:00:00Z", Tag: "IRAORD1 2020-05 buy"},
	}
	return m, orders
}

func iraRun() ([]parity.IRAMonth, []parity.Order) {
	a, ao := iraMonth()
	b, bo := noSellMonth()
	return []parity.IRAMonth{a, b}, append(ao, bo...)
}

var iraPlan = []string{"2020-04"}

func iraFailsOnly(t *testing.T, ms []parity.IRAMonth, orders []parity.Order, names ...string) {
	t.Helper()
	r := parity.CompareIRA("I", iraPlan, ms, orders)
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

func TestAMatchingIRAExportPassesEveryCheck(t *testing.T) {
	ms, orders := iraRun()
	r := parity.CompareIRA("I", iraPlan, ms, orders)
	if !r.Pass() {
		t.Fatalf("matching IRA export failed:\n%s", r.Markdown())
	}
	want := map[string]int{
		parity.CheckIRAMonths: 1, parity.CheckIRAExport: 2, parity.CheckIRAPlan: (1 + 3) + (2 + 1),
		parity.CheckIRAScale: (1 + 3) + (1 + 2), parity.CheckIRAOrders: 4 + 2, parity.CheckIRASequence: 2,
	}
	for name, n := range want {
		if c := checkNamed(r, name); c.Compared != n {
			t.Errorf("%s compared %d, want %d", name, c.Compared, n)
		}
	}
}

func TestAPlannedQuantityOrLimitOffFailsThePlan(t *testing.T) {
	ms, orders := iraRun()
	ms[0].Sells[0].Shares = 39
	iraFailsOnly(t, ms, orders, parity.CheckIRAPlan)
	ms, orders = iraRun()
	ms[0].Buys[1].Limit = 31.49
	iraFailsOnly(t, ms, orders, parity.CheckIRAPlan)
	ms, orders = iraRun()
	ms[1].Skipped = nil
	iraFailsOnly(t, ms, orders, parity.CheckIRAPlan)
	ms, orders = iraRun()
	ms[1].Skipped[0].Reason = "held-by-working-sells"
	iraFailsOnly(t, ms, orders, parity.CheckIRAPlan)
}

func TestAScaleOrScaledQuantityOffFailsTheScaling(t *testing.T) {
	ms, orders := iraRun()
	ms[0].Scale += 1e-11
	iraFailsOnly(t, ms, orders, parity.CheckIRAScale)
	ms, orders = iraRun()
	ms[0].Kept[0].Shares = 49
	orders[1].Quantity = 49
	iraFailsOnly(t, ms, orders, parity.CheckIRAScale, parity.CheckIRAOrders)
	ms, orders = iraRun()
	ms[0].ScaledSkipped = []parity.IRAScaledSkip{{Ticker: "AAA", Shares: 0.5}}
	iraFailsOnly(t, ms, orders, parity.CheckIRAScale)
}

func TestAScaledBuyRecordedAsSkippedMatchesGo(t *testing.T) {
	// A $60 budget scales BBB's 64 and IEF's 4 shares (worst case
	// 64 x 7.35 + 4 x 102.9 + 2 x $2 = 886) to 4 and 0: IEF is skipped.
	ms, orders := iraRun()
	m := &ms[1]
	m.Budget = 60
	m.Scale = 60.0 / 886.0
	m.Kept = []parity.IRAKept{{Ticker: "BBB", Shares: 4}}
	m.ScaledSkipped = []parity.IRAScaledSkip{{Ticker: "IEF", Shares: 4 * m.Scale}}
	orders = append(orders[:5], parity.Order{ID: 6, Ticker: "BBB", Quantity: 4, Status: parity.OrderFilled,
		Time: "2020-05-01T13:00:00Z", LastFillTime: "2020-05-01T20:00:00Z", Tag: "IRAORD1 2020-05 buy"})
	if r := parity.CompareIRA("I", iraPlan, ms, orders); !r.Pass() {
		t.Fatalf("scaled month failed:\n%s", r.Markdown())
	}
}

func TestAnOrderThatIsNotThePlansFailsTheOrders(t *testing.T) {
	ms, orders := iraRun()
	orders[0].Quantity = -41
	iraFailsOnly(t, ms, orders, parity.CheckIRAOrders)
	ms, orders = iraRun()
	iraFailsOnly(t, ms, append(orders[:3:3], orders[4:]...), parity.CheckIRAOrders)
	ms, orders = iraRun()
	orders = append(orders, parity.Order{ID: 9, Ticker: "ZZZ", Quantity: 1, Status: parity.OrderFilled,
		Time: "2020-04-02T13:00:00Z", Tag: "IRAORD1 2020-04 buy"})
	iraFailsOnly(t, ms, orders, parity.CheckIRAOrders)
	ms, orders = iraRun()
	orders = append(orders, parity.Order{ID: 9, Ticker: "ZZZ", Quantity: 1, Status: parity.OrderFilled,
		Time: "2020-06-01T13:00:00Z", Tag: "IRAORD1 2020-06 buy"})
	iraFailsOnly(t, ms, orders, parity.CheckIRAOrders)
	ms, orders = iraRun()
	orders[2].Quantity = 78.5
	iraFailsOnly(t, ms, orders, parity.CheckIRAOrders)
}

func TestAnOrderNamedByItsSecurityIDIsMatched(t *testing.T) {
	ms, orders := iraRun()
	m := &ms[0]
	m.Held["CCC X1"] = m.Held["CCC"]
	m.Prices["CCC X1"] = m.Prices["CCC"]
	delete(m.Held, "CCC")
	delete(m.Prices, "CCC")
	m.Sells[0].Ticker = "CCC X1"
	orders[0].SecurityID = "CCC X1"
	if r := parity.CompareIRA("I", iraPlan, ms, orders); !r.Pass() {
		t.Fatalf("security ID month failed:\n%s", r.Markdown())
	}
}

func TestBuysBeforeTheSellsFilledOrOnAnotherSessionFailTheSequence(t *testing.T) {
	ms, orders := iraRun()
	orders[0].LastFillTime = "2020-04-02T14:00:00Z"
	iraFailsOnly(t, ms, orders, parity.CheckIRASequence)
	ms, orders = iraRun()
	orders[0].Status, orders[0].LastFillTime = 5, ""
	iraFailsOnly(t, ms, orders, parity.CheckIRASequence)
	ms, orders = iraRun()
	ms[0].Waited = 0
	iraFailsOnly(t, ms, orders, parity.CheckIRASequence)
	ms, orders = iraRun()
	ms[0].BuySession = "2020-04-03"
	iraFailsOnly(t, ms, orders, parity.CheckIRASequence)
	ms, orders = iraRun()
	orders[0].Time = "2020-03-31T13:00:00Z"
	iraFailsOnly(t, ms, orders, parity.CheckIRASequence)
	ms, orders = iraRun()
	ms[1].Waited, ms[1].BuySession = 1, "2020-05-04"
	for i := 5; i < 7; i++ {
		orders[i].Time = "2020-05-04T13:00:00Z"
	}
	iraFailsOnly(t, ms, orders, parity.CheckIRASequence)
}

func TestALongerWaitWhileASellWorkedOrForAPricePassesAndIsMeasured(t *testing.T) {
	for _, reason := range []string{"sells", "price"} {
		ms, orders := iraRun()
		ms[0].Waited, ms[0].BuySession = 2, "2020-04-03"
		if reason == "sells" {
			ms[0].ForSells = 1
		} else {
			ms[0].ForPrice = 1
		}
		for i := 1; i < 4; i++ {
			orders[i].Time = "2020-04-03T13:00:00Z"
		}
		r := parity.CompareIRA("I", iraPlan, ms, orders)
		if c := checkNamed(r, parity.CheckIRASequence); !c.Pass() || c.MaxDiff != 1 {
			t.Errorf("%s: sequence %+v", reason, c)
		}
		ms[0].ForSells, ms[0].ForPrice = 0, 0
		if c := checkNamed(parity.CompareIRA("I", iraPlan, ms, orders), parity.CheckIRASequence); c.Pass() {
			t.Errorf("%s: a wait no reason accounts for passed", reason)
		}
	}
	// With no sell, the buys wait only for a price.
	ms, orders := iraRun()
	ms[1].Waited, ms[1].ForPrice, ms[1].BuySession = 1, 1, "2020-05-04"
	for i := 5; i < 7; i++ {
		orders[i].Time = "2020-05-04T13:00:00Z"
	}
	if c := checkNamed(parity.CompareIRA("I", iraPlan, ms, orders), parity.CheckIRASequence); !c.Pass() || c.MaxDiff != 1 {
		t.Errorf("no sell, waited for a price: %+v", c)
	}
}

func TestAnUnplacedBuyFailsTheOrders(t *testing.T) {
	ms, orders := iraRun()
	ms[0].Unplaced = []string{"IEF"}
	iraFailsOnly(t, ms, append(orders[:3:3], orders[4:]...), parity.CheckIRAOrders)
}

func TestAMissingPlannedIRAMonthOrAMalformedOneFails(t *testing.T) {
	ms, orders := iraRun()
	iraFailsOnly(t, ms[1:], orders[5:], parity.CheckIRAMonths)
	for name, alter := range map[string]func(*parity.IRAMonth){
		"bad decision":       func(m *parity.IRAMonth) { m.Decision = "2020-04-xx" },
		"decision elsewhere": func(m *parity.IRAMonth) { m.Decision, m.BuySession = "2020-05-01", "2020-05-02" },
		"buys before":        func(m *parity.IRAMonth) { m.BuySession = "2020-03-31" },
		"no targets":         func(m *parity.IRAMonth) { m.Targets = map[string]float64{} },
		"negative delay":     func(m *parity.IRAMonth) { m.Delay = -1 },
		"negative wait":      func(m *parity.IRAMonth) { m.ForPrice = -1 },
		"no price":           func(m *parity.IRAMonth) { delete(m.Prices, "BBB") },
		"bad band":           func(m *parity.IRAMonth) { m.Band = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			ms, orders := iraRun()
			alter(&ms[0])
			if c := checkNamed(parity.CompareIRA("I", iraPlan, ms, orders), parity.CheckIRAExport); c.Pass() {
				t.Errorf("export passed: %+v", c)
			}
		})
	}
}

func TestAnIRAPayloadDecodesExactly(t *testing.T) {
	raw := `{"month":"2020-04","decision":"2020-04-01","nlv":10000.0,"targets":{"AAA":0.25},"held":{"CCC":40},` +
		`"prices":{"AAA":50.0,"CCC":20.0},"working":{"CCC":0},"unpriced":["NEW"],"sells":[["CCC",40,19.0]],` +
		`"buys":[["AAA",50,52.5]],"skipped":[["ZZZ","buy","below-one-share",0.25]],"waited":1,` +
		`"buy_session":"2020-04-02","delay":1,"band":0.25,"price_band":0.05,"max_fees":2.0,"budget":7500.5,` +
		`"scale":0.9,"kept":[["AAA",45]],"scaled_skipped":[["BBB",0.5]],"for_sells":2,"for_price":3,"unplaced":["Q"]}`
	m, err := parity.DecodeIRA([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.Month != "2020-04" || m.Decision != "2020-04-01" || m.BuySession != "2020-04-02" || m.NLV != 10000 ||
		m.Targets["AAA"] != 0.25 || m.Held["CCC"] != 40 || m.Prices["CCC"] != 20 || m.Unpriced[0] != "NEW" ||
		m.Sells[0] != (parity.IRAOrder{Ticker: "CCC", Shares: 40, Limit: 19}) || m.Buys[0].Limit != 52.5 ||
		m.Skipped[0] != (parity.IRASkip{Ticker: "ZZZ", Side: "buy", Reason: "below-one-share", Shares: 0.25}) ||
		m.Waited != 1 || m.Delay != 1 || m.Band != 0.25 || m.PriceBand != 0.05 || m.MaxFees != 2 ||
		m.Budget != 7500.5 || m.Scale != 0.9 || m.Kept[0] != (parity.IRAKept{Ticker: "AAA", Shares: 45}) ||
		m.ScaledSkipped[0] != (parity.IRAScaledSkip{Ticker: "BBB", Shares: 0.5}) || m.ForSells != 2 || m.ForPrice != 3 ||
		m.Unplaced[0] != "Q" {
		t.Errorf("decoded %+v", m)
	}
}

func TestAMalformedIRAPayloadIsRefused(t *testing.T) {
	good := `{"month":"2020-04","decision":"2020-04-01","nlv":1.0,"targets":{"A":0.5},"held":{},"prices":{"A":1.0},` +
		`"working":{},"unpriced":[],"sells":[],"buys":[["A",1,1.05]],"skipped":[],"waited":0,"buy_session":"2020-04-01",` +
		`"delay":1,"band":0.25,"price_band":0.05,"max_fees":2.0,"budget":1.0,"scale":1.0,"kept":[["A",1]],"scaled_skipped":[],` +
		`"for_sells":0,"for_price":0,"unplaced":[]}`
	if _, err := parity.DecodeIRA([]byte(good)); err != nil {
		t.Fatalf("good payload: %v", err)
	}
	for name, raw := range map[string]string{
		"unknown field":   strings.Replace(good, `"nlv":`, `"x":1,"nlv":`, 1),
		"no budget":       strings.Replace(good, `"budget":1.0,`, ``, 1),
		"null scale":      strings.Replace(good, `"scale":1.0`, `"scale":null`, 1),
		"short order":     strings.Replace(good, `["A",1,1.05]`, `["A",1]`, 1),
		"part share":      strings.Replace(good, `["A",1,1.05]`, `["A",1.5,1.05]`, 1),
		"null ticker":     strings.Replace(good, `["A",1,1.05]`, `[null,1,1.05]`, 1),
		"short skip":      strings.Replace(good, `"skipped":[]`, `"skipped":[["A","buy",0.5]]`, 1),
		"short kept":      strings.Replace(good, `"kept":[["A",1]]`, `"kept":[["A"]]`, 1),
		"short scaled":    strings.Replace(good, `"scaled_skipped":[]`, `"scaled_skipped":[["A"]]`, 1),
		"null weight":     strings.Replace(good, `"targets":{"A":0.5}`, `"targets":{"A":null}`, 1),
		"no month":        strings.Replace(good, `"month":"2020-04",`, ``, 1),
		"no kept":         strings.Replace(good, `,"kept":[["A",1]]`, ``, 1),
		"no unplaced":     strings.Replace(good, `,"unplaced":[]`, ``, 1),
		"no wait reasons": strings.Replace(good, `"for_sells":0,`, ``, 1),
		"bad skip shares": strings.Replace(good, `"skipped":[]`, `"skipped":[["A","buy","x","y"]]`, 1),
		"empty ticker":    strings.Replace(good, `["A",1,1.05]`, `["",1,1.05]`, 1),
		"null holding":    strings.Replace(good, `"held":{}`, `"held":{"A":null}`, 1),
		"null price":      strings.Replace(good, `"prices":{"A":1.0}`, `"prices":{"A":null}`, 1),
		"null working":    strings.Replace(good, `"working":{}`, `"working":{"A":null}`, 1),
	} {
		if _, err := parity.DecodeIRA([]byte(raw)); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

func TestOrdersDecodeFromQuantConnectsOrderObjects(t *testing.T) {
	raw := `[{"id":3,"symbol":{"value":"AAA","id":"AAA R735QTJ8XC9X"},"quantity":-40.0,"status":3,` +
		`"time":"2020-04-01T13:00:00Z","lastFillTime":"2020-04-01T20:00:00Z","tag":"IRAORD1 2020-04 sell","price":1}]`
	orders, err := parity.DecodeOrders([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := parity.Order{ID: 3, Ticker: "AAA", SecurityID: "AAA R735QTJ8XC9X", Quantity: -40, Status: 3,
		Time: "2020-04-01T13:00:00Z", LastFillTime: "2020-04-01T20:00:00Z", Tag: "IRAORD1 2020-04 sell"}
	if len(orders) != 1 || orders[0] != want {
		t.Errorf("decoded %+v", orders)
	}
	if _, err := parity.DecodeOrders([]byte(`{}`)); err == nil {
		t.Error("an object decoded as orders")
	}
}
