package parity_test

import (
	"math"
	"strings"
	"testing"
	"time"

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
		{ID: 5, Ticker: "SPY", Quantity: 1, Status: 5, Time: "2020-04-02T13:00:00Z", Tag: "IRAPARITY1 {}", FillsKnown: true},
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

// iraSessions is the weekdays of March to June 2020 less Good Friday
// (2020-04-10) and Memorial Day (2020-05-25).
var iraSessions = func() parity.Sessions {
	var dates []string
	for d := time.Date(2020, 3, 2, 0, 0, 0, 0, time.UTC); d.Month() < 7; d = d.AddDate(0, 0, 1) {
		day := d.Format(time.DateOnly)
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday && day != "2020-04-10" && day != "2020-05-25" {
			dates = append(dates, day)
		}
	}
	s, err := parity.SessionList(dates)
	if err != nil {
		panic(err)
	}
	return s
}()

func iraFailsOnly(t *testing.T, ms []parity.IRAMonth, orders []parity.Order, names ...string) {
	t.Helper()
	r := parity.CompareIRA("I", iraPlan, ms, orders, iraSessions)
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
	r := parity.CompareIRA("I", iraPlan, ms, orders, iraSessions)
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
	if r := parity.CompareIRA("I", iraPlan, ms, orders, iraSessions); !r.Pass() {
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
	if r := parity.CompareIRA("I", iraPlan, ms, orders, iraSessions); !r.Pass() {
		t.Fatalf("security ID month failed:\n%s", r.Markdown())
	}
}

func TestAnOrderOfARenamedStockIsMatchedByItsOrderID(t *testing.T) {
	// The orders listing names AAA by its later ticker, AAA2.
	ms, orders := iraRun()
	orders[1].Ticker = "AAA2"
	iraFailsOnly(t, ms, orders, parity.CheckIRAOrders)
	ms[0].Placed = map[int]string{2: "AAA"}
	if r := parity.CompareIRA("I", iraPlan, ms, orders, iraSessions); !r.Pass() {
		t.Fatalf("renamed stock failed:\n%s", r.Markdown())
	}
}

func TestBuysBeforeTheSellsFilledOrOnAnotherSessionFailTheSequence(t *testing.T) {
	ms, orders := iraRun()
	orders[0].LastFillTime = "2020-04-02T14:00:00Z"
	iraFailsOnly(t, ms, orders, parity.CheckIRASequence)
	ms, orders = iraRun()
	orders[0].Status, orders[0].LastFillTime = 5, ""
	iraFailsOnly(t, ms, orders, parity.CheckIRASequence, parity.CheckIRAOrders)
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
			orders[0].LastFillTime = "2020-04-02T20:00:00Z" // still working at 8:00 on the 2nd
		} else {
			ms[0].ForPrice = 1
		}
		for i := 1; i < 4; i++ {
			orders[i].Time = "2020-04-03T13:00:00Z"
		}
		r := parity.CompareIRA("I", iraPlan, ms, orders, iraSessions)
		if c := checkNamed(r, parity.CheckIRASequence); !c.Pass() || c.MaxDiff != 1 {
			t.Errorf("%s: sequence %+v", reason, c)
		}
		ms[0].ForSells, ms[0].ForPrice = 0, 0
		if c := checkNamed(parity.CompareIRA("I", iraPlan, ms, orders, iraSessions), parity.CheckIRASequence); c.Pass() {
			t.Errorf("%s: a wait no reason accounts for passed", reason)
		}
	}
	// With no sell, the buys wait only for a price.
	ms, orders := iraRun()
	ms[1].Waited, ms[1].ForPrice, ms[1].BuySession = 1, 1, "2020-05-04"
	for i := 5; i < 7; i++ {
		orders[i].Time = "2020-05-04T13:00:00Z"
	}
	if c := checkNamed(parity.CompareIRA("I", iraPlan, ms, orders, iraSessions), parity.CheckIRASequence); !c.Pass() || c.MaxDiff != 1 {
		t.Errorf("no sell, waited for a price: %+v", c)
	}
}

func TestAWaitForAPriceIsAnUnlistedDifference(t *testing.T) {
	// Live trading sizes from the previous close and never waits for a
	// price, and ADR 0026 point 8 does not list the wait.
	ms, orders := iraRun()
	ms[1].Waited, ms[1].ForPrice, ms[1].BuySession = 1, 1, "2020-05-04"
	for i := 5; i < 7; i++ {
		orders[i].Time = "2020-05-04T13:00:00Z"
	}
	r := parity.CompareIRA("I", iraPlan, ms, orders, iraSessions)
	if len(r.Differences) != 1 || r.Differences[0] != (parity.Difference{Run: "I", Where: "2020-05", Kind: parity.KindPriceWait, Size: "1 sessions"}) {
		t.Errorf("differences %+v", r.Differences)
	}
	if c := checkNamed(r, parity.CheckDifferences); c.Pass() {
		t.Errorf("a price wait passed the differences check: %+v", c)
	}
}

func TestTheRecordedWaitMustBeTheSessionsBetweenTheDecisionAndTheBuys(t *testing.T) {
	// Every date and time moved to the 30th, the counters kept at one
	// session: the calendar has 20.
	ms, orders := iraRun()
	ms[0].BuySession = "2020-04-30"
	for i := 1; i < 4; i++ {
		orders[i].Time, orders[i].LastFillTime = "2020-04-30T13:00:00Z", "2020-04-30T20:00:00Z"
	}
	iraFailsOnly(t, ms, orders, parity.CheckIRASequence)
	// Across Good Friday: decided on the 9th, bought on the 13th, one
	// session later.
	ms, orders = iraRun()
	m := &ms[0]
	m.Decision, m.BuySession = "2020-04-09", "2020-04-13"
	orders[0].Time, orders[0].LastFillTime = "2020-04-09T13:00:00Z", "2020-04-09T20:00:00Z"
	for i := 1; i < 4; i++ {
		orders[i].Time, orders[i].LastFillTime = "2020-04-13T13:00:00Z", "2020-04-13T20:00:00Z"
	}
	if c := checkNamed(parity.CompareIRA("I", iraPlan, ms, orders, iraSessions), parity.CheckIRASequence); !c.Pass() {
		t.Errorf("one session across a holiday: %+v", c)
	}
	m.Waited, m.ForSells = 2, 1
	if c := checkNamed(parity.CompareIRA("I", iraPlan, ms, orders, iraSessions), parity.CheckIRASequence); c.Pass() {
		t.Errorf("a holiday counted as a session passed: %+v", c)
	}
	// No calendar covers the span.
	ms, orders = iraRun()
	if c := checkNamed(parity.CompareIRA("I", iraPlan, ms, orders, nil), parity.CheckIRASequence); c.Pass() ||
		!strings.Contains(c.First, "no session calendar") {
		t.Errorf("no calendar: %+v", c)
	}
}

func TestASessionRecordedAsWaitingForASellTheOrdersDoNotShowFails(t *testing.T) {
	// The sell filled on the rebalance session, so nothing was working at
	// 8:00 on the 2nd: that session cannot have waited for it.
	ms, orders := iraRun()
	ms[0].Waited, ms[0].ForSells, ms[0].BuySession = 2, 1, "2020-04-03"
	for i := 1; i < 4; i++ {
		orders[i].Time = "2020-04-03T13:00:00Z"
	}
	iraFailsOnly(t, ms, orders, parity.CheckIRASequence)
	// A session recorded as a price wait while a sell was working.
	ms[0].ForSells, ms[0].ForPrice = 0, 1
	orders[0].LastFillTime = "2020-04-02T20:00:00Z"
	iraFailsOnly(t, ms, orders, parity.CheckIRASequence, parity.CheckDifferences)
}

func TestWaitCountersThatOverflowFailWithoutAPanic(t *testing.T) {
	// MaxInt + MaxInt + 3 wraps to 1, the sessions waited.
	ms, orders := iraRun()
	ms[0].Delay, ms[0].ForSells, ms[0].ForPrice = math.MaxInt, math.MaxInt, 3
	iraFailsOnly(t, ms, orders, parity.CheckIRASequence, parity.CheckDifferences)
}

func TestTheDecisionAndTheBuysMustBeOnTradingSessions(t *testing.T) {
	// Decided on Thursday 2020-04-02, bought on Saturday the 4th: Friday is
	// the one session between them, but Saturday is not one.
	ms, orders := iraRun()
	m := &ms[0]
	m.Decision, m.BuySession = "2020-04-02", "2020-04-04"
	orders[0].Time, orders[0].LastFillTime = "2020-04-02T13:00:00Z", "2020-04-02T20:00:00Z"
	for i := 1; i < 4; i++ {
		orders[i].Time, orders[i].LastFillTime = "2020-04-04T13:00:00Z", "2020-04-04T20:00:00Z"
	}
	if c := checkNamed(parity.CompareIRA("I", iraPlan, ms, orders, iraSessions), parity.CheckIRASequence); c.Pass() ||
		!strings.Contains(c.First, "not a trading session") {
		t.Errorf("Saturday buys: %+v", c)
	}
	// With no sell the buys go on the rebalance's day, which must itself be
	// a session: not a Saturday, nor Memorial Day.
	for _, day := range []string{"2020-05-02", "2020-05-25"} {
		ms, orders := iraRun()
		ms[1].Decision, ms[1].BuySession = day, day
		for i := 5; i < 7; i++ {
			orders[i].Time, orders[i].LastFillTime = day+"T13:00:00Z", day+"T20:00:00Z"
		}
		if c := checkNamed(parity.CompareIRA("I", iraPlan, ms, orders, iraSessions), parity.CheckIRASequence); c.Pass() ||
			!strings.Contains(c.First, "not a trading session") {
			t.Errorf("%s: %+v", day, c)
		}
	}
}

func TestASellEndingAFractionOfASecondAfterABuyStillHoldsIt(t *testing.T) {
	// The orders listing gives event times in fractional seconds: a sell
	// cancelled at 13:00:00.5 was working at a buy placed at 13:00:00.
	raw := `[{"id":0,"symbol":{"value":"OLD","id":"OLD X"},"quantity":-5.0,"status":5,"time":"2020-03-31T13:00:00Z",` +
		`"events":[{"time":1585832400.5,"status":"canceled"}]}]`
	old, err := parity.DecodeOrders([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	ms, orders := iraRun()
	// An untagged order is outside the plan, so the orders fail too.
	iraFailsOnly(t, ms, append(orders, old...), parity.CheckIRASequence, parity.CheckIRAOrders)
}

func TestABuyPlacedWhileAnySellWasStillWorkingFails(t *testing.T) {
	// An earlier sell, untagged, still working when the buys went. An
	// untagged order is outside the plan, so each fails the orders too.
	ms, orders := iraRun()
	orders = append(orders, parity.Order{ID: 0, Ticker: "OLD", Quantity: -5, Status: parity.OrderFilled,
		Time: "2020-03-31T13:00:00Z", LastFillTime: "2020-04-02T15:00:00Z"})
	iraFailsOnly(t, ms, orders, parity.CheckIRASequence, parity.CheckIRAOrders)
	// One that never became terminal.
	ms, orders = iraRun()
	orders = append(orders, parity.Order{ID: 0, Ticker: "OLD", Quantity: -5, Status: 1, Time: "2020-03-02T13:00:00Z"})
	iraFailsOnly(t, ms, orders, parity.CheckIRASequence, parity.CheckIRAOrders)
	// One of no known time, never terminal.
	ms, orders = iraRun()
	orders = append(orders, parity.Order{ID: 0, Ticker: "OLD", Quantity: -5, Status: 1})
	iraFailsOnly(t, ms, orders, parity.CheckIRASequence, parity.CheckIRAOrders)
	// A sell placed with the buys of a month with no planned sell.
	ms, orders = iraRun()
	orders = append(orders, parity.Order{ID: 5, Ticker: "OLD", Quantity: -5, Status: parity.OrderFilled,
		Time: "2020-05-01T13:00:00Z", LastFillTime: "2020-05-01T20:00:00Z"})
	iraFailsOnly(t, ms, orders, parity.CheckIRASequence, parity.CheckIRAOrders)
	// One that was cancelled before the buys does not hold them.
	ms, orders = iraRun()
	orders = append(orders, parity.Order{ID: 0, Ticker: "OLD", Quantity: -5, Status: 5, Time: "2020-03-31T13:00:00Z",
		Terminal: "2020-03-31T14:00:00Z"})
	if c := checkNamed(parity.CompareIRA("I", iraPlan, ms, orders, iraSessions), parity.CheckIRASequence); !c.Pass() {
		t.Fatalf("a sell cancelled before the rebalance failed the sequence: %+v", c)
	}
}

func TestAnOrderAgainstItsPhasesDirectionFailsTheOrders(t *testing.T) {
	// +58 and -10 tagged as the buy net to the 48 planned.
	ms, orders := iraRun()
	orders[1].Quantity = 58
	orders = append(orders, parity.Order{ID: 8, Ticker: "AAA", Quantity: -10, Status: parity.OrderFilled,
		Time: "2020-04-02T13:00:00Z", LastFillTime: "2020-04-02T20:00:00Z", Tag: "IRAORD1 2020-04 buy"})
	iraFailsOnly(t, ms, orders, parity.CheckIRAOrders)
	// A sell phase order that buys.
	ms, orders = iraRun()
	orders[0].Quantity = 40
	iraFailsOnly(t, ms, orders, parity.CheckIRAOrders)
}

func TestFractionalOrdersCannotCancelOut(t *testing.T) {
	// Two half shares beside the 48 planned: no sentinel of theirs may sum
	// back to 48.
	ms, orders := iraRun()
	for id := 8; id < 10; id++ {
		orders = append(orders, parity.Order{ID: id, Ticker: "AAA", Quantity: 0.5, Status: parity.OrderFilled,
			Time: "2020-04-02T13:00:00Z", LastFillTime: "2020-04-02T20:00:00Z", Tag: "IRAORD1 2020-04 buy"})
	}
	iraFailsOnly(t, ms, orders, parity.CheckIRAOrders)
	ms, orders = iraRun()
	orders[1].Quantity = 1e300
	iraFailsOnly(t, ms, orders, parity.CheckIRAOrders)
}

func TestAnOrderOfAnUnknownPhaseFailsTheOrders(t *testing.T) {
	for _, tag := range []string{"IRAORD1 2020-04 Buy", "IRAORD1 2020-04", "IRAORD1 2020-04 buy extra"} {
		ms, orders := iraRun()
		orders = append(orders, parity.Order{ID: 8, Ticker: "AAA", Quantity: 1, Status: parity.OrderFilled,
			Time: "2020-04-02T13:00:00Z", LastFillTime: "2020-04-02T20:00:00Z", Tag: tag})
		r := parity.CompareIRA("I", iraPlan, ms, orders, iraSessions)
		if c := checkNamed(r, parity.CheckIRAOrders); c.Pass() || !strings.Contains(c.First, "phase") {
			t.Errorf("%q: orders %+v", tag, c)
		}
	}
}

func TestABuyTheBacktestDidNotFillIsNotBought(t *testing.T) {
	for _, status := range []int{5, 7} { // Canceled, Invalid
		ms, orders := iraRun()
		orders[1].Status, orders[1].LastFillTime = status, ""
		r := parity.CompareIRA("I", iraPlan, ms, orders, iraSessions)
		if c := checkNamed(r, parity.CheckIRAOrders); c.Pass() || !strings.Contains(c.First, "status") {
			t.Errorf("status %d: orders %+v", status, c)
		}
	}
}

func TestABuyTheBacktestCouldNotTradeIsListedWithItsRecordedDetection(t *testing.T) {
	// ADR 0026 point 8, as amended 2026-10-18: a stock delisted or acquired
	// at the rebalance, which the backtest could not trade. Each departure
	// is listed with the detection the export records.
	without := func() ([]parity.IRAMonth, []parity.Order) {
		ms, orders := iraRun()
		return ms, append(orders[:3:3], orders[4:]...) // no IEF buy
	}
	ms, orders := without()
	ms[0].Refused = []parity.IRARefusal{{Ticker: "IEF", Phase: "buy", Reason: "The security with symbol 'IEF X' is marked as non-tradable."}}
	r := parity.CompareIRA("I", iraPlan, ms, orders, iraSessions)
	want := parity.Difference{Run: "I", Where: "2020-04 buy IEF", Kind: parity.KindDelisted,
		Size: "refused by QuantConnect: The security with symbol 'IEF X' is marked as non-tradable."}
	if !r.Pass() || len(r.Differences) != 1 || r.Differences[0] != want {
		t.Errorf("non-tradable: differences %+v\n%s", r.Differences, r.Markdown())
	}
	if !parity.KindDelisted.Listed() {
		t.Error("the delisted cause is not listed")
	}
	// No price within five sessions: the buys waited the delay and the
	// five sessions the export records, which the calendar confirms.
	ms, orders = without()
	m := &ms[0]
	m.Unplaced, m.ForPrice, m.Waited, m.BuySession = []string{"IEF"}, parity.IRAPriceWait, 6, "2020-04-09"
	for i := 1; i < 3; i++ {
		orders[i].Time = "2020-04-09T13:00:00Z"
	}
	r = parity.CompareIRA("I", iraPlan, ms, orders, iraSessions)
	want = parity.Difference{Run: "I", Where: "2020-04 buy IEF", Kind: parity.KindDelisted, Size: "no price within five sessions (unplaced)"}
	if len(r.Differences) != 2 || r.Differences[0] != want || r.Differences[1].Kind != parity.KindPriceWait {
		t.Errorf("unplaced: differences %+v", r.Differences)
	}
	iraFailsOnly(t, ms, orders, parity.CheckDifferences) // the price wait is not listed
	// A recorded detection excuses only the order it names.
	ms, orders = iraRun()
	ms[0].Unplaced = []string{"IEF"}
	orders[3].Quantity = 22
	iraFailsOnly(t, ms, orders, parity.CheckIRAOrders)
}

func TestAnUnplacedBuyIsListedOnlyAfterTheFiveSessionPriceWait(t *testing.T) {
	// Removing a buy and naming it unplaced is no evidence by itself: the
	// export must record the five sessions waited for a price.
	for _, forPrice := range []int{0, parity.IRAPriceWait - 1} {
		ms, orders := iraRun()
		orders = append(orders[:3:3], orders[4:]...)
		m := &ms[0]
		m.Unplaced, m.ForPrice, m.Waited = []string{"IEF"}, forPrice, 1+forPrice
		if forPrice > 0 {
			m.BuySession = "2020-04-08"
			for i := 1; i < 3; i++ {
				orders[i].Time = "2020-04-08T13:00:00Z"
			}
		}
		r := parity.CompareIRA("I", iraPlan, ms, orders, iraSessions)
		if c := checkNamed(r, parity.CheckIRAOrders); c.Pass() || !strings.Contains(c.First, "unplaced") {
			t.Errorf("for_price %d: orders %+v", forPrice, c)
		}
		for _, d := range r.Differences {
			if d.Kind == parity.KindDelisted {
				t.Errorf("for_price %d: listed %+v", forPrice, d)
			}
		}
	}
	// An unplaced name with no planned buy to excuse fails too.
	ms, orders := iraRun()
	ms[0].Unplaced, ms[0].ForPrice = []string{"ZZZ"}, parity.IRAPriceWait
	if c := checkNamed(parity.CompareIRA("I", iraPlan, ms, orders, iraSessions), parity.CheckIRAOrders); c.Pass() || !strings.Contains(c.First, "ZZZ") {
		t.Errorf("unplaced with no buy: %+v", c)
	}
}

func TestEveryRefusalIsListedUnderACauseOrFails(t *testing.T) {
	without := func() ([]parity.IRAMonth, []parity.Order) {
		ms, orders := iraRun()
		return ms, append(orders[:3:3], orders[4:]...) // no IEF buy
	}
	nonTradable := "The security with symbol 'IEF X' is marked as non-tradable."
	for name, c := range map[string]struct {
		refusal parity.IRARefusal
		filled  bool // the IEF buy filled after all
		want    string
	}{
		// A placeholder order ID says QuantConnect refused, not why.
		"placeholder ID alone":        {parity.IRARefusal{Ticker: "IEF", Phase: "buy", ID: -10}, false, "no reason recorded"},
		"another reason":              {parity.IRARefusal{Ticker: "IEF", Phase: "buy", Reason: "Insufficient buying power"}, false, "Insufficient buying power"},
		"no reason":                   {parity.IRARefusal{Ticker: "IEF", Phase: "buy"}, false, "no reason recorded"},
		"another reason, then a fill": {parity.IRARefusal{Ticker: "IEF", Phase: "buy", Reason: "Insufficient buying power"}, true, "Insufficient buying power"},
		"another reason, unplanned":   {parity.IRARefusal{Ticker: "ZZZ", Phase: "buy", Reason: "Insufficient buying power"}, true, "ZZZ"},
		"non-tradable, then a fill":   {parity.IRARefusal{Ticker: "IEF", Phase: "buy", Reason: nonTradable}, true, "non-tradable"},
		"non-tradable, unplanned":     {parity.IRARefusal{Ticker: "ZZZ", Phase: "buy", Reason: nonTradable}, true, "ZZZ"},
		"non-tradable unpriced sale":  {parity.IRARefusal{Ticker: "NEW", Phase: "unpriced", Reason: nonTradable}, true, "NEW"},
	} {
		ms, orders := without()
		if c.filled {
			ms, orders = iraRun()
		}
		ms[0].Refused = []parity.IRARefusal{c.refusal}
		r := parity.CompareIRA("I", iraPlan, ms, orders, iraSessions)
		if o := checkNamed(r, parity.CheckIRAOrders); o.Pass() || !strings.Contains(o.First, c.want) || !strings.Contains(o.First, "refused") {
			t.Errorf("%s: orders %+v", name, o)
		}
		for _, d := range r.Differences {
			if d.Kind == parity.KindDelisted {
				t.Errorf("%s: listed %+v", name, d)
			}
		}
	}
}

func TestAnUnpricedNameIsAnUnlistedDifference(t *testing.T) {
	ms, orders := iraRun()
	ms[0].Unpriced = []string{"NEW"}
	r := parity.CompareIRA("I", iraPlan, ms, orders, iraSessions)
	want := parity.Difference{Run: "I", Where: "2020-04 NEW", Kind: parity.KindUnpriced, Size: "no reference price: traded outside the planner"}
	if len(r.Differences) != 1 || r.Differences[0] != want {
		t.Errorf("differences %+v", r.Differences)
	}
	if c := checkNamed(r, parity.CheckDifferences); c.Pass() {
		t.Errorf("an unpriced name passed: %+v", c)
	}
}

func TestAnUnpricedHoldingsSaleMustBeOfAnUnpricedName(t *testing.T) {
	ms, orders := iraRun()
	ms[0].Version, ms[0].Unpriced = parity.IRAVersion, []string{"NEW"}
	sale := parity.Order{ID: 0, Ticker: "NEW", Quantity: -3, Status: parity.OrderFilled, Time: "2020-04-01T13:00:00Z",
		LastFillTime: "2020-04-01T20:00:00Z", Tag: "IRAORD1 2020-04 unpriced"}
	r := parity.CompareIRA("I", iraPlan, ms, append(orders, sale), iraSessions)
	if c := checkNamed(r, parity.CheckIRAOrders); !c.Pass() || c.Compared != 4+2+1 {
		t.Errorf("an unpriced holding's sale: %+v", c)
	}
	sale.Ticker = "AAA"
	iraFailsOnly(t, ms, append(orders, sale), parity.CheckIRAOrders, parity.CheckDifferences)
	sale.Ticker, sale.Quantity = "NEW", 3
	iraFailsOnly(t, ms, append(orders, sale), parity.CheckIRAOrders, parity.CheckDifferences)
}

func TestTheSaleOfAnUnpricedPendingTargetFails(t *testing.T) {
	// NEW has no reference price and is a target: the template keeps it
	// pending and never sells it.
	ms, orders := iraRun()
	ms[0].Version, ms[0].Unpriced, ms[0].Pending = parity.IRAVersion, []string{"NEW"}, []string{"NEW"}
	sale := parity.Order{ID: 0, Ticker: "NEW", Quantity: -3, Status: parity.OrderFilled, Time: "2020-04-01T13:00:00Z",
		LastFillTime: "2020-04-01T20:00:00Z", Tag: "IRAORD1 2020-04 unpriced"}
	r := parity.CompareIRA("I", iraPlan, ms, append(orders, sale), iraSessions)
	if c := checkNamed(r, parity.CheckIRAOrders); c.Pass() || !strings.Contains(c.First, "pending target") {
		t.Errorf("a pending target's sale: %+v", c)
	}
	// An export before version 3 does not record which unpriced names
	// were targets, so no unpriced sale of it can be verified.
	ms[0].Version, ms[0].Pending = 2, nil
	r = parity.CompareIRA("I", iraPlan, ms, append(orders, sale), iraSessions)
	if c := checkNamed(r, parity.CheckIRAOrders); c.Pass() || !strings.Contains(c.First, "version 2") {
		t.Errorf("a version 2 unpriced sale: %+v", c)
	}
}

func TestUnpricedSaleFailuresReportInNameOrder(t *testing.T) {
	ms, orders := iraRun()
	ms[0].Version = parity.IRAVersion
	for _, n := range []string{"ZZZ", "YYY", "XXX", "WWW"} {
		orders = append(orders, parity.Order{ID: 0, Ticker: n, Quantity: -3, Status: parity.OrderFilled, Time: "2020-04-01T13:00:00Z",
			LastFillTime: "2020-04-01T20:00:00Z", Tag: "IRAORD1 2020-04 unpriced"})
	}
	for i := 0; i < 50; i++ {
		if c := checkNamed(parity.CompareIRA("I", iraPlan, ms, orders, iraSessions), parity.CheckIRAOrders); !strings.Contains(c.First, "WWW") {
			t.Fatalf("run %d: first failure %q, not the first name's", i, c.First)
		}
	}
}

func TestOrdersOutsideThePlanAreAccountedFor(t *testing.T) {
	// A filled order that is neither an IRAORD1 order nor a never-filled
	// export carrier is a trade the plan does not explain.
	for name, extra := range map[string]parity.Order{
		"untagged buy":   {ID: 9, Ticker: "ZZZ", Quantity: 5, Status: parity.OrderFilled, Time: "2020-04-02T13:00:00Z", LastFillTime: "2020-04-02T20:00:00Z"},
		"filled carrier": {ID: 9, Ticker: "SPY", Quantity: 1, Status: parity.OrderFilled, Time: "2020-04-02T13:00:00Z", LastFillTime: "2020-04-02T20:00:00Z", Tag: "IRAPARITY1 {}"},
		"other tag":      {ID: 9, Ticker: "AAA", Quantity: 5, Status: parity.OrderFilled, Time: "2020-04-02T13:00:00Z", LastFillTime: "2020-04-02T20:00:00Z", Tag: "rebalance"},
		"unfilled other": {ID: 9, Ticker: "AAA", Quantity: 5, Status: parity.OrderCanceled, Time: "2020-04-02T13:00:00Z", Tag: "rebalance"},
	} {
		ms, orders := iraRun()
		r := parity.CompareIRA("I", iraPlan, ms, append(orders, extra), iraSessions)
		if c := checkNamed(r, parity.CheckIRAOrders); c.Pass() || !strings.Contains(c.First, "order 9") {
			t.Errorf("%s: orders %+v", name, c)
		}
	}
	// LEAN's own sale of a holding at its delisting is listed, and fails
	// the differences check: ADR 0026 point 8 does not list it.
	ms, orders := iraRun()
	orders = append(orders, parity.Order{ID: 9, Ticker: "DDD", Quantity: -100, Status: parity.OrderFilled,
		Time: "2020-04-20T04:00:00Z", LastFillTime: "2020-04-20T04:00:00Z", Tag: "Liquidate from delisting"})
	r := parity.CompareIRA("I", iraPlan, ms, orders, iraSessions)
	want := parity.Difference{Run: "I", Where: "2020-04-20 DDD", Kind: parity.KindLiquidated, Size: "order 9: -100 shares"}
	if len(r.Differences) != 1 || r.Differences[0] != want || parity.KindLiquidated.Listed() {
		t.Errorf("a delisting liquidation: %+v", r.Differences)
	}
	iraFailsOnly(t, ms, orders, parity.CheckDifferences)
	// The buy of a pending target is listed as unpriced
	// (TestABuyOutsideThePlannerIsExcusedOnlyForAPendingTargetBySecurity).
	// A buy of a name no earlier rebalance left unpriced is not explained.
	ms, orders = iraRun()
	ms[1].Unpriced = []string{"NEW"}
	orders = append(orders, parity.Order{ID: 9, Ticker: "NEW", Quantity: 5, Status: parity.OrderFilled,
		Time: "2020-04-21T20:00:00Z", LastFillTime: "2020-04-21T20:00:00Z"})
	iraFailsOnly(t, ms, orders, parity.CheckIRAOrders, parity.CheckDifferences)
}

func TestAnUnfinishedMonthFailsItsBuysAndItsSequence(t *testing.T) {
	// The sell never filled; the next rebalance recorded the month.
	ms, orders := iraRun()
	m := &ms[0]
	m.Unfinished, m.Kept, m.Budget, m.Scale = true, nil, 0, 0
	orders[0].Status, orders[0].LastFillTime = 1, ""
	r := parity.CompareIRA("I", iraPlan, ms, append(orders[:1:1], orders[4:]...), iraSessions)
	// The working sell (not filled, so not sold), and each of the three buys.
	if c := checkNamed(r, parity.CheckIRAOrders); c.Failures != 2+3 {
		t.Errorf("orders: %+v", c)
	}
	if c := checkNamed(r, parity.CheckIRASequence); c.Pass() || !strings.Contains(c.First, "never placed") {
		t.Errorf("sequence: %+v", c)
	}
	if c := checkNamed(r, parity.CheckIRAScale); c.Compared != 1+2 {
		t.Errorf("an unfinished month was scaled: %+v", c)
	}
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
			if c := checkNamed(parity.CompareIRA("I", iraPlan, ms, orders, iraSessions), parity.CheckIRAExport); c.Pass() {
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
		`"scale":0.9,"kept":[["AAA",45]],"scaled_skipped":[["BBB",0.5]],"for_sells":2,"for_price":3,"unplaced":["Q"],` +
		`"placed":[["CCC","sell",7],["AAA","buy",9]]}`
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
		m.Unplaced[0] != "Q" || m.Placed[7] != "CCC" || m.Placed[9] != "AAA" || len(m.Placed) != 2 {
		t.Errorf("decoded %+v", m)
	}
}

func TestAMalformedIRAPayloadIsRefused(t *testing.T) {
	good := v1Good
	for _, ok := range []string{good, v2Good, v3()} {
		if _, err := parity.DecodeIRA([]byte(ok)); err != nil {
			t.Fatalf("good payload: %v", err)
		}
	}
	for name, raw := range map[string]string{
		"unknown field":         strings.Replace(good, `"nlv":`, `"x":1,"nlv":`, 1),
		"no budget":             strings.Replace(good, `"budget":1.0,`, ``, 1),
		"null scale":            strings.Replace(good, `"scale":1.0`, `"scale":null`, 1),
		"short order":           strings.Replace(good, `["A",1,1.05]`, `["A",1]`, 1),
		"part share":            strings.Replace(good, `["A",1,1.05]`, `["A",1.5,1.05]`, 1),
		"null ticker":           strings.Replace(good, `["A",1,1.05]`, `[null,1,1.05]`, 1),
		"short skip":            strings.Replace(good, `"skipped":[]`, `"skipped":[["A","buy",0.5]]`, 1),
		"short kept":            strings.Replace(good, `"kept":[["A",1]]`, `"kept":[["A"]]`, 1),
		"short scaled":          strings.Replace(good, `"scaled_skipped":[]`, `"scaled_skipped":[["A"]]`, 1),
		"null weight":           strings.Replace(good, `"targets":{"A":0.5}`, `"targets":{"A":null}`, 1),
		"no month":              strings.Replace(good, `"month":"2020-04",`, ``, 1),
		"no kept":               strings.Replace(good, `,"kept":[["A",1]]`, ``, 1),
		"no unplaced":           strings.Replace(good, `,"unplaced":[]`, ``, 1),
		"no placed":             strings.Replace(good, `,"placed":[]`, ``, 1),
		"short placed":          strings.Replace(good, `"placed":[]`, `"placed":[["A","buy"]]`, 1),
		"placed twice":          strings.Replace(good, `"placed":[]`, `"placed":[["A","buy",1],["B","buy",1]]`, 1),
		"unknown phase":         strings.Replace(good, `"placed":[]`, `"placed":[["A","Buy",1]]`, 1),
		"sell twice":            strings.Replace(good, `"sells":[]`, `"sells":[["A",1,1.0],["A",2,1.0]]`, 1),
		"buy twice":             strings.Replace(good, `["A",1,1.05]`, `["A",1,1.05],["A",1,1.05]`, 1),
		"skip twice":            strings.Replace(good, `"skipped":[]`, `"skipped":[["B","buy","x",0.5],["B","buy","y",0.5]]`, 1),
		"kept twice":            strings.Replace(good, `"kept":[["A",1]]`, `"kept":[["A",1],["A",1]]`, 1),
		"scaled twice":          strings.Replace(good, `"scaled_skipped":[]`, `"scaled_skipped":[["B",0.5],["B",0.5]]`, 1),
		"unpriced twice":        strings.Replace(good, `"unpriced":[]`, `"unpriced":["N","N"]`, 1),
		"unplaced twice":        strings.Replace(good, `"unplaced":[]`, `"unplaced":["N","N"]`, 1),
		"v1 refused":            strings.Replace(good, `"placed":[]`, `"placed":[],"refused":[]`, 1),
		"v1 unfinished":         strings.Replace(good, `"placed":[]`, `"placed":[],"unfinished":false`, 1),
		"unknown version":       strings.Replace(good, `"month":`, `"v":5,"month":`, 1),
		"v2 no refused":         strings.Replace(good, `"month":`, `"v":2,"unfinished":false,"month":`, 1),
		"v2 no unfinished":      strings.Replace(good, `"month":`, `"v":2,"refused":[],"month":`, 1),
		"v2 refused id":         v2(`"placed":[]`, `"placed":[["A","buy",-10]]`),
		"v2 short refusal":      v2(`"refused":[]`, `"refused":[["A","buy"]]`),
		"refused unknown phase": v2(`"refused":[]`, `"refused":[["A","Buy","x"]]`),
		"refused no phase":      v2(`"refused":[]`, `"refused":[["A","","x"]]`),
		"v2 pending":            v2(`"refused":[]`, `"refused":[],"pending":[]`),
		"v3 no pending":         strings.Replace(v2Good, `"v":2`, `"v":3`, 1),
		"v3 null pending":       v3(`"pending":[]`, `"pending":null`),
		"pending twice":         v3(`"unpriced":[]`, `"unpriced":["N"]`, `"pending":[]`, `"pending":["N","N"]`),
		"pending not unpriced":  v3(`"pending":[]`, `"pending":["N"]`),
		"no wait reasons":       strings.Replace(good, `"for_sells":0,`, ``, 1),
		"bad skip shares":       strings.Replace(good, `"skipped":[]`, `"skipped":[["A","buy","x","y"]]`, 1),
		"empty ticker":          strings.Replace(good, `["A",1,1.05]`, `["",1,1.05]`, 1),
		"null holding":          strings.Replace(good, `"held":{}`, `"held":{"A":null}`, 1),
		"null price":            strings.Replace(good, `"prices":{"A":1.0}`, `"prices":{"A":null}`, 1),
		"null working":          strings.Replace(good, `"working":{}`, `"working":{"A":null}`, 1),
	} {
		if _, err := parity.DecodeIRA([]byte(raw)); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

// v1Good is a good payload of version 1, which had no version field.
const v1Good = `{"month":"2020-04","decision":"2020-04-01","nlv":1.0,"targets":{"A":0.5},"held":{},"prices":{"A":1.0},` +
	`"working":{},"unpriced":[],"sells":[],"buys":[["A",1,1.05]],"skipped":[],"waited":0,"buy_session":"2020-04-01",` +
	`"delay":1,"band":0.25,"price_band":0.05,"max_fees":2.0,"budget":1.0,"scale":1.0,"kept":[["A",1]],"scaled_skipped":[],` +
	`"for_sells":0,"for_price":0,"unplaced":[],"placed":[]}`

// v2Good is the good payload of TestAMalformedIRAPayloadIsRefused in
// version 2.
const v2Good = `{"v":2,"month":"2020-04","decision":"2020-04-01","nlv":1.0,"targets":{"A":0.5},"held":{},"prices":{"A":1.0},` +
	`"working":{},"unpriced":[],"sells":[],"buys":[["A",1,1.05]],"skipped":[],"waited":0,"buy_session":"2020-04-01",` +
	`"delay":1,"band":0.25,"price_band":0.05,"max_fees":2.0,"budget":1.0,"scale":1.0,"kept":[["A",1]],"scaled_skipped":[],` +
	`"for_sells":0,"for_price":0,"unplaced":[],"placed":[],"refused":[],"unfinished":false}`

// v2 is v2Good with from replaced by to.
func v2(from, to string) string { return strings.Replace(v2Good, from, to, 1) }

// v3 is v2Good in version 3 (an empty pending list), with each pair of
// pairs replaced: from, to, from, to...
func v3(pairs ...string) string {
	s := strings.Replace(strings.Replace(v2Good, `"v":2`, `"v":3`, 1), `"unfinished":false`, `"unfinished":false,"pending":[]`, 1)
	for i := 0; i+1 < len(pairs); i += 2 {
		s = strings.Replace(s, pairs[i], pairs[i+1], 1)
	}
	return s
}

func TestAVersion3PayloadRecordsThePendingTargets(t *testing.T) {
	m, err := parity.DecodeIRA([]byte(v3(`"unpriced":[]`, `"unpriced":["N","H"]`, `"pending":[]`, `"pending":["N"]`)))
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != 3 || len(m.Pending) != 1 || m.Pending[0] != "N" {
		t.Errorf("decoded version %d, pending %v", m.Version, m.Pending)
	}
	for raw, want := range map[string]int{v2Good: 2, v1Good: 1} {
		if m, err := parity.DecodeIRA([]byte(raw)); err != nil || m.Version != want {
			t.Errorf("version %d decoded as %d (%v)", want, m.Version, err)
		}
	}
}

func TestTwoRefusalsInAMonthDecodeAndKeepTheirReasons(t *testing.T) {
	// Before version 2 a refusal was recorded among the placed orders with
	// QuantConnect's placeholder ID.
	v1 := `{"month":"2020-04","decision":"2020-04-01","nlv":1.0,"targets":{"A":0.5},"held":{},"prices":{"A":1.0},` +
		`"working":{},"unpriced":[],"sells":[],"buys":[["A",1,1.05]],"skipped":[],"waited":0,"buy_session":"2020-04-01",` +
		`"delay":1,"band":0.25,"price_band":0.05,"max_fees":2.0,"budget":1.0,"scale":1.0,"kept":[["A",1]],"scaled_skipped":[],` +
		`"for_sells":0,"for_price":0,"unplaced":[],"placed":[["A","buy",-10],["B","buy",-10],["C","buy",4]]}`
	m, err := parity.DecodeIRA([]byte(v1))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Placed) != 1 || m.Placed[4] != "C" || len(m.Refused) != 2 ||
		m.Refused[1] != (parity.IRARefusal{Ticker: "B", Phase: "buy", ID: -10}) {
		t.Errorf("decoded placed %v, refused %+v", m.Placed, m.Refused)
	}
	m, err = parity.DecodeIRA([]byte(v2(`"refused":[]`, `"refused":[["A","buy","Security price is zero"],["B","buy",""]]`)))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Refused) != 2 || m.Refused[0] != (parity.IRARefusal{Ticker: "A", Phase: "buy", Reason: "Security price is zero"}) {
		t.Errorf("decoded refused %+v", m.Refused)
	}
	m, err = parity.DecodeIRA([]byte(v2(`"unfinished":false`, `"unfinished":true`)))
	if err != nil || !m.Unfinished {
		t.Errorf("unfinished: %+v, %v", m, err)
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
		Time: "2020-04-01T13:00:00Z", LastFillTime: "2020-04-01T20:00:00Z", Terminal: "2020-04-01T20:00:00Z", Tag: "IRAORD1 2020-04 sell"}
	if len(orders) != 1 || orders[0] != want {
		t.Errorf("decoded %+v", orders)
	}
	// A cancelled order ends at its last terminal event, to the fraction of
	// a second; a working one has none.
	raw = `[{"id":4,"symbol":{"value":"B","id":"B X"},"quantity":-5.0,"status":5,"time":"2020-04-01T13:00:00Z",` +
		`"events":[{"time":1585746000.0,"status":"submitted"},{"time":1585749600.5,"status":"canceled"}]},` +
		`{"id":5,"symbol":{"value":"C","id":"C X"},"quantity":-5.0,"status":1,"time":"2020-04-01T13:00:00Z",` +
		`"events":[{"time":1585746000.0,"status":"submitted"}]}]`
	orders, err = parity.DecodeOrders([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if orders[0].Terminal != "2020-04-01T14:00:00.5Z" || orders[1].Terminal != "" {
		t.Errorf("terminal %q, %q", orders[0].Terminal, orders[1].Terminal)
	}
	if orders, _ := parity.DecodeOrders([]byte(`[{"id":1,"status":3,"lastFillTime":"2020-04-01T20:00:00Z"}]`)); orders[0].Terminal != "2020-04-01T20:00:00Z" {
		t.Errorf("a filled order without events ends at its last fill: %q", orders[0].Terminal)
	}
	if _, err := parity.DecodeOrders([]byte(`{}`)); err == nil {
		t.Error("an object decoded as orders")
	}
}

func TestACarrierIsSkippedOnlyWhenItsEventsShowItNeverFilled(t *testing.T) {
	// A cancelled or refused carrier whose events fill nothing is skipped.
	ms, orders := iraRun()
	orders[4].Status = parity.OrderInvalid
	iraFailsOnly(t, ms, orders)
	for name, alter := range map[string]func(*parity.Order){
		"partly filled, then cancelled": func(o *parity.Order) { o.FilledQuantity = 1 },
		"filled before the cancel":      func(o *parity.Order) { o.LastFillTime = "2020-04-02T14:00:00Z" },
		"still working":                 func(o *parity.Order) { o.Status = 1 },
		"cancel pending":                func(o *parity.Order) { o.Status = 8 },
		"unknown status":                func(o *parity.Order) { o.Status = 6 },
		"no events read":                func(o *parity.Order) { o.FillsKnown = false },
	} {
		ms, orders := iraRun()
		alter(&orders[4])
		r := parity.CompareIRA("I", iraPlan, ms, orders, iraSessions)
		if c := checkNamed(r, parity.CheckIRAOrders); c.Pass() || !strings.Contains(c.First, "order 5") {
			t.Errorf("%s: orders %+v", name, c)
		}
	}
}

func TestOrdersCountTheirFillsFromTheirEvents(t *testing.T) {
	raw := `[{"id":1,"status":5,"events":[{"time":1585746000.0,"status":"submitted","fillQuantity":0.0},` +
		`{"time":1585746001.0,"status":"partiallyFilled","fillQuantity":-2.0},{"time":1585749600.0,"status":"canceled","fillQuantity":0.0}]},` +
		`{"id":2,"status":5,"events":[{"time":1585746000.0,"status":"submitted"},{"time":1585749600.0,"status":"canceled","fillQuantity":0.0}]},` +
		`{"id":3,"status":5},` +
		`{"id":4,"status":5,"events":[{"time":1585746000.0,"status":"submitted","fillQuantity":null}]}]`
	orders, err := parity.DecodeOrders([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct {
		filled float64
		known  bool
	}{{2, true}, {0, false}, {0, false}, {0, false}} {
		if orders[i].FilledQuantity != want.filled || orders[i].FillsKnown != want.known {
			t.Errorf("order %d: filled %v (known %v), want %v (%v)", i+1, orders[i].FilledQuantity, orders[i].FillsKnown, want.filled, want.known)
		}
	}
}

func TestATerminalEventWithNoUsableTimeIsRefused(t *testing.T) {
	for name, ev := range map[string]string{
		"missing":   `{"status":"canceled"}`,
		"null":      `{"time":null,"status":"filled"}`,
		"zero":      `{"time":0.0,"status":"canceled"}`,
		"negative":  `{"time":-5.0,"status":"invalid"}`,
		"too large": `{"time":1e300,"status":"filled"}`,
	} {
		raw := `[{"id":1,"status":5,"time":"2020-04-01T13:00:00Z","events":[` + ev + `]}]`
		if orders, err := parity.DecodeOrders([]byte(raw)); err == nil {
			t.Errorf("%s: decoded %+v", name, orders)
		}
	}
	// A non-terminal event's time is not read.
	if _, err := parity.DecodeOrders([]byte(`[{"id":1,"status":1,"events":[{"status":"submitted"}]}]`)); err != nil {
		t.Errorf("a submitted event with no time: %v", err)
	}
}

func TestTheFirstListedSessionCanBeTheRebalance(t *testing.T) {
	var dates []string
	for d := time.Date(2020, 4, 1, 0, 0, 0, 0, time.UTC); d.Month() < 6; d = d.AddDate(0, 0, 1) {
		day := d.Format(time.DateOnly)
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday && day != "2020-04-10" && day != "2020-05-25" {
			dates = append(dates, day)
		}
	}
	list, err := parity.SessionList(dates)
	if err != nil {
		t.Fatal(err)
	}
	ms, orders := iraRun()
	if c := checkNamed(parity.CompareIRA("I", iraPlan, ms, orders, list), parity.CheckIRASequence); !c.Pass() {
		t.Errorf("a rebalance on the list's first session: %+v", c)
	}
}

func TestAMonthThatPlacedNoOrderStillHasItsSessionsCounted(t *testing.T) {
	ms, orders := iraRun()
	quiet := ms[1]
	quiet.Month, quiet.Decision, quiet.BuySession = "2020-06", "2020-06-01", "2020-06-01"
	quiet.Targets = map[string]float64{"AAA": 0.001}
	quiet.Buys, quiet.Kept = nil, nil
	r := parity.CompareIRA("I", iraPlan, append(ms, quiet), orders, iraSessions)
	if c := checkNamed(r, parity.CheckIRASequence); !c.Pass() || c.Compared != 3 {
		t.Errorf("sequence %+v", c)
	}
	// On a Saturday it is no session, order or none.
	quiet.Decision, quiet.BuySession = "2020-06-06", "2020-06-06"
	r = parity.CompareIRA("I", iraPlan, append(ms, quiet), orders, iraSessions)
	if c := checkNamed(r, parity.CheckIRASequence); c.Pass() || !strings.Contains(c.First, "2020-06-06") {
		t.Errorf("a Saturday rebalance with no order: %+v", c)
	}
	// Every planned buy refused as non-tradable: no order, yet the buys'
	// recorded day must be a session.
	ms, orders = iraRun()
	m := &ms[1]
	m.BuySession = "2020-05-02"
	m.Refused = []parity.IRARefusal{{Ticker: "BBB", Phase: parity.PhaseBuy, Reason: "non-tradable"}, {Ticker: "IEF", Phase: parity.PhaseBuy, Reason: "non-tradable"}}
	r = parity.CompareIRA("I", iraPlan, ms, orders[:5], iraSessions)
	if c := checkNamed(r, parity.CheckIRASequence); c.Pass() || !strings.Contains(c.First, "2020-05-02") {
		t.Errorf("a Saturday buy day with every buy refused: %+v", c)
	}
}

func TestAnUnplacedBuyOfAMonthWithNoOrderIsListedOnlyOnTheCalendarsCount(t *testing.T) {
	// Every buy of the month unplaced, no sell: no order at all. The five
	// sessions the export records must still be the calendar's.
	month := func(buySession string) ([]parity.IRAMonth, []parity.Order) {
		ms, orders := iraRun()
		m := &ms[1]
		m.Unplaced, m.ForPrice, m.Waited, m.BuySession = []string{"BBB", "IEF"}, parity.IRAPriceWait, parity.IRAPriceWait, buySession
		return ms, orders[:5]
	}
	ms, orders := month("2020-05-08")
	r := parity.CompareIRA("I", iraPlan, ms, orders, iraSessions)
	listed := 0
	for _, d := range r.Differences {
		if d.Kind == parity.KindDelisted {
			listed++
		}
	}
	if c := checkNamed(r, parity.CheckIRAOrders); !c.Pass() || listed != 2 {
		t.Errorf("five sessions on the calendar: orders %+v, %d listed", c, listed)
	}
	ms, orders = month("2020-05-01")
	r = parity.CompareIRA("I", iraPlan, ms, orders, iraSessions)
	for _, d := range r.Differences {
		if d.Kind == parity.KindDelisted {
			t.Errorf("listed with no session waited: %+v", d)
		}
	}
	if c := checkNamed(r, parity.CheckIRAOrders); c.Pass() || !strings.Contains(c.First, "unplaced") {
		t.Errorf("orders %+v", c)
	}
	if c := checkNamed(r, parity.CheckIRASequence); c.Pass() {
		t.Errorf("sequence %+v", c)
	}
}

func TestABuyOutsideThePlannerIsExcusedOnlyForAPendingTargetBySecurity(t *testing.T) {
	buy := parity.Order{ID: 9, Ticker: "NEW2", SecurityID: "NEW X1", Quantity: 5, Status: parity.OrderFilled,
		Time: "2020-04-21T20:00:00Z", LastFillTime: "2020-04-21T20:00:00Z"}
	run := func(alter func(*parity.IRAMonth, *parity.Order)) parity.Report {
		ms, orders := iraRun()
		m, o := &ms[0], buy
		m.Version, m.Unpriced, m.Pending, m.PendingIDs = parity.IRAVersion, []string{"HHH", "NEW"}, []string{"NEW"}, map[string]string{"NEW": "NEW X1"}
		alter(m, &o)
		return parity.CompareIRA("I", iraPlan, ms, append(orders, o), iraSessions)
	}
	// The pending target bought under the ticker it later took: matched by
	// its security ID.
	r := run(func(*parity.IRAMonth, *parity.Order) {})
	if c := checkNamed(r, parity.CheckIRAOrders); !c.Pass() || len(r.Differences) != 3 ||
		r.Differences[2] != (parity.Difference{Run: "I", Where: "2020-04 NEW", Kind: parity.KindUnpriced, Size: "order 9: 5 shares bought outside the planner"}) {
		t.Errorf("a pending target's buy: orders %+v, differences %+v", c, r.Differences)
	}
	// Version 3, the name the security ID: matched by it.
	r = run(func(m *parity.IRAMonth, _ *parity.Order) {
		m.Version, m.Unpriced, m.Pending, m.PendingIDs = 3, []string{"NEW X1"}, []string{"NEW X1"}, nil
	})
	if c := checkNamed(r, parity.CheckIRAOrders); !c.Pass() {
		t.Errorf("version 3, named by security ID: %+v", c)
	}
	for name, alter := range map[string]func(*parity.IRAMonth, *parity.Order){
		"an unpriced holding, not a target": func(m *parity.IRAMonth, o *parity.Order) {
			m.Pending, m.PendingIDs, o.Ticker = []string{}, map[string]string{}, "NEW"
		},
		"another security, same ticker":     func(m *parity.IRAMonth, o *parity.Order) { o.Ticker, o.SecurityID = "NEW", "NEW Y2" },
		"version 3, matched by ticker only": func(m *parity.IRAMonth, o *parity.Order) { m.Version, m.PendingIDs, o.Ticker = 3, nil, "NEW" },
		"version 2, no pending recorded": func(m *parity.IRAMonth, o *parity.Order) {
			m.Version, m.Pending, m.PendingIDs, o.Ticker = 2, nil, nil, "NEW"
		},
	} {
		r := run(alter)
		if c := checkNamed(r, parity.CheckIRAOrders); c.Pass() || !strings.Contains(c.First, "order 9") || strings.Contains(c.First, "not an order of the IRA plan") {
			t.Errorf("%s: orders %+v", name, c)
		}
	}
}

// v4 is v3() in version 4 (no pending name, so no security ID), with each
// pair of pairs replaced: from, to, from, to...
func v4(pairs ...string) string {
	s := strings.Replace(strings.Replace(v3(), `"v":3`, `"v":4`, 1), `"pending":[]`, `"pending":[],"pending_ids":{}`, 1)
	for i := 0; i+1 < len(pairs); i += 2 {
		s = strings.Replace(s, pairs[i], pairs[i+1], 1)
	}
	return s
}

func TestAVersion4PayloadRecordsThePendingTargetsSecurityIDs(t *testing.T) {
	m, err := parity.DecodeIRA([]byte(v4(`"unpriced":[]`, `"unpriced":["N","H"]`, `"pending":[]`, `"pending":["N"]`,
		`"pending_ids":{}`, `"pending_ids":{"N":"N X1"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != 4 || m.PendingIDs["N"] != "N X1" || len(m.PendingIDs) != 1 {
		t.Errorf("decoded version %d, pending IDs %v", m.Version, m.PendingIDs)
	}
	for name, raw := range map[string]string{
		"v3 pending ids":            v3(`"pending":[]`, `"pending":[],"pending_ids":{}`),
		"v4 no pending ids":         strings.Replace(v3(), `"v":3`, `"v":4`, 1),
		"v4 null ids":               v4(`"pending_ids":{}`, `"pending_ids":null`),
		"an id not pending":         v4(`"pending_ids":{}`, `"pending_ids":{"N":"N X1"}`),
		"a pending name with no id": v4(`"unpriced":[]`, `"unpriced":["N"]`, `"pending":[]`, `"pending":["N"]`),
		"an empty id": v4(`"unpriced":[]`, `"unpriced":["N"]`, `"pending":[]`, `"pending":["N"]`,
			`"pending_ids":{}`, `"pending_ids":{"N":""}`),
		"unknown version": strings.Replace(v4(), `"v":4`, `"v":5`, 1),
	} {
		if _, err := parity.DecodeIRA([]byte(raw)); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}
