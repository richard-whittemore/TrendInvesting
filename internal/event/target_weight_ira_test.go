package event_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func validMomentumConfiguration() event.TargetWeightConfigurationPayload {
	return event.TargetWeightConfigurationPayload{
		StrategyID: event.TargetWeightStrategyMomentum,
		AccountID:  "ira-traditional",
		Mode:       event.TradingModeDryRun,
		Adapter:    event.AdapterIBKR,
		Momentum: event.MomentumConfiguration{
			Rule: event.MomentumRuleTrend, ListingType: "CS", UniverseSize: 500, MinPrice: 5,
			ScoreLookback: 252, ScoreSkip: 21, VolatilityReturns: 252, TopN: 25, SectorCap: 3,
			InverseVolatility: true, Invested: 0.98, TrendInstrument: "SPY", TrendCloses: 200,
			RiskOffInstrument: "IEF", RiskOffFraction: 0.5, RebalanceBand: 0.25,
			UniverseSource: "massive", SectorSources: []string{"ishares-iwv-gics", "nasdaq-screener"},
		},
		Glide:     synthGlide(),
		Execution: event.ExecutionConfiguration{CashRule: event.CashRuleSettled, ShareQuantity: event.ShareQuantityMinimumLot, MaxFeesPerOrder: 1},
		Limits:    event.MarginLimits{MaxExposure: 1, MaxOrdersPerDecision: 60, MaxOrderFraction: 0.5, PriceBand: 0.05},
	}
}

func validMomentumDecided() event.TargetsDecidedPayload {
	return event.TargetsDecidedPayload{
		AccountID: "ira-traditional", StrategyID: event.TargetWeightStrategyMomentum,
		DecisionTime: twSession, SessionPeriodEnd: twSession, Outcome: event.TargetsOutcomeDecided,
		Targets: []event.TargetWeight{
			{InstrumentID: "AAA", Weight: 0.49, Shares: []event.SleeveShare{{Sleeve: event.SleeveMomentum, Weight: 0.49}}},
			{InstrumentID: "IEF", Weight: 0.49, Shares: []event.SleeveShare{{Sleeve: event.SleeveMomentum, Weight: 0.49}}},
		},
		Glide: event.GlideFigures{RebalanceSession: "2025-03-05", AgeMonths: 30 * 12, BondShare: 0},
		Momentum: event.MomentumFigures{
			CloseSession: "2025-03-04", TrendInstrument: "SPY", TrendCloses: 300, TrendClose: 99, TrendMean: 100, RiskOff: true,
			UniverseRows: 3, UniverseListingType: 2, UniverseAtOrAboveFloor: 2, UniverseMembers: 2,
			Names: []event.MomentumName{
				{InstrumentID: "AAA", UniverseRank: 1, DollarVolume: 5e8, Sector: "Tech", SectorSource: "ishares-iwv-gics",
					Status: event.MomentumStatusPicked, Closes: 300, Score: 0.3, Volatility: 0.02, ScoreRank: 1, Weight: 0.49, BacktestWouldPick: true},
				{InstrumentID: "BBB", UniverseRank: 2, DollarVolume: 4e8, Status: event.MomentumStatusShortHistory, Closes: 100},
			},
		},
	}
}

func validIRARefused() event.OrderRefusedPayload {
	p := validOrderRefused()
	p.Reason = event.RefusedInsufficientCash
	p.Figures.CashChecked, p.Figures.BuyCost, p.Figures.CashAvailable = true, 1029, 980
	return p
}

func validUniverseSnapshot() event.UniverseSnapshotPayload {
	return event.UniverseSnapshotPayload{Session: "2025-03-04", Source: "massive", Rows: []event.UniverseRow{
		{InstrumentID: "AAA", Type: "CS", Close: 50, Volume: 1e7, Sector: "Tech", SectorSource: "ishares-iwv-gics"},
		{InstrumentID: "BBB", Type: "CS", Close: 5, Volume: 2e6},
	}}
}

func validSessionCalendar() event.SessionCalendarPayload {
	return event.SessionCalendarPayload{Source: "fixture", Sessions: []string{"2025-03-03", "2025-03-04", "2025-03-05"}}
}

func validAccountCashSnapshot() event.AccountCashSnapshotPayload {
	p := validAccountPositionsSnapshot()
	p.AccountID = "ira-traditional"
	return event.AccountCashSnapshotPayload{AccountPositionsSnapshotPayload: p, SettledCash: 900, TotalCash: 1000}
}

func validLimitOrderLifecycle() event.LimitOrderLifecyclePayload {
	return event.LimitOrderLifecyclePayload{
		AccountID: "ira-traditional", ClientOrderID: "tw-1", OrderID: "ib-7", InstrumentID: "AAA", Side: event.OrderSideSell,
		Status: event.OrderLifecycleStatusSubmitted, Quantity: 10, LimitPrice: 47.5, OccurredAt: twSession,
	}
}

func validTargetWeightFill() event.TargetWeightFillPayload {
	return event.TargetWeightFillPayload{
		AccountID: "ira-traditional", ClientOrderID: "tw-1", FillID: "f-1", InstrumentID: "AAA", Side: event.OrderSideSell,
		Quantity: 4, Price: 49.9, Commission: 0.35, FilledAt: twSession.Add(time.Hour),
	}
}

func TestMomentumPayloadFixturesAreValid(t *testing.T) {
	for _, p := range []validator{
		validMomentumConfiguration(), validMomentumDecided(), validIRARefused(), validUniverseSnapshot(),
		validSessionCalendar(), validAccountCashSnapshot(), validLimitOrderLifecycle(), validTargetWeightFill(),
	} {
		if err := p.Validate(); err != nil {
			t.Errorf("%T: %v", p, err)
		}
	}
}

func TestMomentumConfigurationValidate(t *testing.T) {
	v := validMomentumConfiguration()
	type P = event.TargetWeightConfigurationPayload
	expectInvalid(t, "boost section", v, func(p *P) { p.Boost.InstrumentID = "SPY" }, "no boost section")
	expectInvalid(t, "ira margin", v, func(p *P) { p.Limits.MaxExposure = 1.01 }, "never uses margin")
	expectInvalid(t, "rule", v, func(p *P) { p.Momentum.Rule = "mom" }, "rule")
	expectInvalid(t, "listing type", v, func(p *P) { p.Momentum.ListingType = "" }, "listing type")
	expectInvalid(t, "universe size", v, func(p *P) { p.Momentum.UniverseSize = 0 }, "universe size")
	expectInvalid(t, "top n", v, func(p *P) { p.Momentum.TopN = 0 }, "top n")
	expectInvalid(t, "score skip", v, func(p *P) { p.Momentum.ScoreSkip = 252 }, "score skip")
	expectInvalid(t, "volatility returns", v, func(p *P) { p.Momentum.VolatilityReturns = 1 }, "volatility returns")
	expectInvalid(t, "min price", v, func(p *P) { p.Momentum.MinPrice = math.NaN() }, "min price")
	expectInvalid(t, "inverse volatility", v, func(p *P) { p.Momentum.InverseVolatility = false }, "inverse volatility")
	expectInvalid(t, "invested", v, func(p *P) { p.Momentum.Invested = 0 }, "invested")
	expectInvalid(t, "same instruments", v, func(p *P) { p.Momentum.RiskOffInstrument = "SPY" }, "distinct")
	expectInvalid(t, "risk-off fraction", v, func(p *P) { p.Momentum.RiskOffFraction = 1.5 }, "risk-off fraction")
	expectInvalid(t, "band", v, func(p *P) { p.Momentum.RebalanceBand = 0 }, "rebalance band")
	expectInvalid(t, "universe source", v, func(p *P) { p.Momentum.UniverseSource = "" }, "universe source")
	expectInvalid(t, "no sector sources", v, func(p *P) { p.Momentum.SectorSources = nil }, "sector sources")
	expectInvalid(t, "repeated sector source", v, func(p *P) { p.Momentum.SectorSources = []string{"a", "a"} }, "repeated")
	expectInvalid(t, "cash rule", v, func(p *P) { p.Execution.CashRule = "borrowed" }, "cash rule")
	expectInvalid(t, "share quantity", v, func(p *P) { p.Execution.ShareQuantity = "fractional" }, "share quantity")
	expectInvalid(t, "fees", v, func(p *P) { p.Execution.MaxFeesPerOrder = -1 }, "fees")

	b := validTargetWeightConfiguration()
	expectInvalid(t, "boost with momentum", b, func(p *P) { p.Momentum.TopN = 25 }, "no momentum or execution")
	expectInvalid(t, "boost with execution", b, func(p *P) { p.Execution.CashRule = event.CashRuleTotal }, "no momentum or execution")
	total := v
	total.Execution.CashRule = event.CashRuleTotal
	if err := total.Validate(); err != nil {
		t.Errorf("the total-cash rule is valid: %v", err)
	}
}

func TestMomentumConfigurationHashCoversItsSections(t *testing.T) {
	base := event.TargetWeightConfigurationHash(validMomentumConfiguration())
	type P = event.TargetWeightConfigurationPayload
	for name, mutate := range map[string]func(*P){
		"top n":          func(p *P) { p.Momentum.TopN = 24 },
		"band":           func(p *P) { p.Momentum.RebalanceBand = 0.2 },
		"sector sources": func(p *P) { p.Momentum.SectorSources = []string{"nasdaq-screener", "ishares-iwv-gics"} },
		"cash rule":      func(p *P) { p.Execution.CashRule = event.CashRuleTotal },
		"fees":           func(p *P) { p.Execution.MaxFeesPerOrder = 2 },
		"universe":       func(p *P) { p.Momentum.UniverseSource = "ibkr" },
	} {
		p := validMomentumConfiguration()
		mutate(&p)
		if event.TargetWeightConfigurationHash(p) == base {
			t.Errorf("changing %s left the hash unchanged", name)
		}
	}
}

// TestBoostPayloadsOmitMomentumSections checks that a Boost configuration and
// decision encode no momentum or execution field, and a momentum decision no
// boost field.
func TestBoostPayloadsOmitMomentumSections(t *testing.T) {
	for _, c := range []struct {
		payload any
		absent  []string
	}{
		{validTargetWeightConfiguration(), []string{`"momentum"`, `"execution"`}},
		{validTargetsDecided(), []string{`"momentum"`}},
		{validMomentumDecided(), []string{`"boost"`}},
		{validOrderReleased(), []string{"cash_checked", "buy_cost", "cash_available"}},
	} {
		raw, err := json.Marshal(c.payload)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range c.absent {
			if strings.Contains(string(raw), field) {
				t.Errorf("%T encodes %s: %s", c.payload, field, raw)
			}
		}
	}
}

func TestMomentumDecisionValidate(t *testing.T) {
	v := validMomentumDecided()
	type P = event.TargetsDecidedPayload
	expectInvalid(t, "boost figures", v, func(p *P) { p.Boost.InstrumentID = "SPY" }, "no boost figures")
	expectInvalid(t, "close session", v, func(p *P) { p.Momentum.CloseSession = "March" }, "close session")
	expectInvalid(t, "trend instrument", v, func(p *P) { p.Momentum.TrendInstrument = "" }, "trend instrument")
	expectInvalid(t, "trend mean", v, func(p *P) { p.Momentum.TrendMean = math.NaN() }, "trend close and mean")
	expectInvalid(t, "counts", v, func(p *P) { p.Momentum.UniverseRows = -1 }, "counts")
	expectInvalid(t, "names count", v, func(p *P) { p.Momentum.UniverseMembers = 3 }, "names for 3")
	name := func(mutate func(*event.MomentumName)) func(*P) {
		return func(p *P) {
			names := append([]event.MomentumName(nil), p.Momentum.Names...)
			mutate(&names[0])
			p.Momentum.Names = names
		}
	}
	expectInvalid(t, "name instrument", v, name(func(n *event.MomentumName) { n.InstrumentID = "" }), "instrument")
	expectInvalid(t, "name rank", v, name(func(n *event.MomentumName) { n.UniverseRank = 2 }), "universe rank")
	expectInvalid(t, "name status", v, name(func(n *event.MomentumName) { n.Status = "liked" }), "status")
	expectInvalid(t, "name sector source", v, name(func(n *event.MomentumName) { n.SectorSource = "" }), "sector label")
	expectInvalid(t, "name closes", v, name(func(n *event.MomentumName) { n.Closes = -1 }), "closes")
	expectInvalid(t, "name volatility", v, name(func(n *event.MomentumName) { n.Volatility = math.Inf(1) }), "volatility")
	expectInvalid(t, "name score", v, name(func(n *event.MomentumName) { n.Score = math.NaN() }), "score")
	expectInvalid(t, "weight without pick", v, name(func(n *event.MomentumName) { n.Status = event.MomentumStatusSectorCapped }), "picked")

	b := validTargetsDecided()
	expectInvalid(t, "boost with momentum figures", b, func(p *P) { p.Momentum.TrendCloses = 1 }, "no momentum figures")

	withheld := v
	withheld.Outcome, withheld.Targets = event.TargetsOutcomeWithheld, nil
	withheld.WithheldReason, withheld.Detail = event.WithheldRebalanceInProgress, "an order of the last rebalance is working"
	withheld.Momentum.Names, withheld.Momentum.UniverseMembers = nil, 0
	if err := withheld.Validate(); err != nil {
		t.Errorf("a withheld momentum decision without names is valid: %v", err)
	}
	statuses := []string{
		event.MomentumStatusScored, event.MomentumStatusSectorCapped, event.MomentumStatusBeyondTopN,
		event.MomentumStatusNoSector, event.MomentumStatusZeroVolatility, event.MomentumStatusGap,
	}
	for _, status := range statuses {
		p := validMomentumDecided()
		p.Momentum.Names = append([]event.MomentumName(nil), p.Momentum.Names...)
		p.Momentum.Names[1].Status = status
		if err := p.Validate(); err != nil {
			t.Errorf("status %s: %v", status, err)
		}
	}
}

func TestIRARiskFiguresValidate(t *testing.T) {
	v := validIRARefused()
	type P = event.OrderRefusedPayload
	expectInvalid(t, "cost nan", v, func(p *P) { p.Figures.BuyCost = math.NaN() }, "buy cost")
	expectInvalid(t, "cost negative", v, func(p *P) { p.Figures.BuyCost = -1 }, "buy cost")
	expectInvalid(t, "cash inf", v, func(p *P) { p.Figures.CashAvailable = math.Inf(-1) }, "cash available")
	expectInvalid(t, "unchecked cash", v, func(p *P) { p.Figures.CashChecked = false }, "not checked")
	for _, reason := range []string{event.RefusedIRAExposure, event.RefusedSellExceedsHolding, event.RefusedUnvaluedHolding} {
		p := v
		p.Reason = reason
		if err := p.Validate(); err != nil {
			t.Errorf("reason %s: %v", reason, err)
		}
	}
	negative := v
	negative.Figures.CashAvailable = -50
	if err := negative.Validate(); err != nil {
		t.Errorf("a debit is a cash figure the gate can refuse against: %v", err)
	}
}

func TestUniverseSnapshotValidate(t *testing.T) {
	v := validUniverseSnapshot()
	type P = event.UniverseSnapshotPayload
	row := func(mutate func(*event.UniverseRow)) func(*P) {
		return func(p *P) {
			rows := append([]event.UniverseRow(nil), p.Rows...)
			mutate(&rows[0])
			p.Rows = rows
		}
	}
	expectInvalid(t, "session", v, func(p *P) { p.Session = "2025-3-4" }, "session")
	expectInvalid(t, "source", v, func(p *P) { p.Source = "" }, "source")
	expectInvalid(t, "type", v, row(func(r *event.UniverseRow) { r.Type = "" }), "type")
	expectInvalid(t, "close", v, row(func(r *event.UniverseRow) { r.Close = -1 }), "close")
	expectInvalid(t, "volume", v, row(func(r *event.UniverseRow) { r.Volume = math.NaN() }), "volume")
	expectInvalid(t, "sector without source", v, row(func(r *event.UniverseRow) { r.SectorSource = "" }), "sector")
	expectInvalid(t, "unsorted", v, row(func(r *event.UniverseRow) { r.InstrumentID = "ZZZ" }), "ascending")
	expectInvalid(t, "empty id", v, row(func(r *event.UniverseRow) { r.InstrumentID = "" }), "empty")
	empty := v
	empty.Rows = nil
	if err := empty.Validate(); err != nil {
		t.Errorf("an empty snapshot is valid; the module withholds on it: %v", err)
	}
}

func TestSessionCalendarValidate(t *testing.T) {
	v := validSessionCalendar()
	type P = event.SessionCalendarPayload
	expectInvalid(t, "source", v, func(p *P) { p.Source = "" }, "source")
	expectInvalid(t, "empty", v, func(p *P) { p.Sessions = nil }, "at least one")
	expectInvalid(t, "date", v, func(p *P) { p.Sessions = []string{"2025-03-32"} }, "date")
	expectInvalid(t, "order", v, func(p *P) { p.Sessions = []string{"2025-03-04", "2025-03-04"} }, "ascending")
}

func TestAccountCashSnapshotValidate(t *testing.T) {
	v := validAccountCashSnapshot()
	type P = event.AccountCashSnapshotPayload
	expectInvalid(t, "positions part", v, func(p *P) { p.NetLiquidation = 0 }, "net liquidation")
	expectInvalid(t, "settled", v, func(p *P) { p.SettledCash = math.NaN() }, "cash")
	expectInvalid(t, "total", v, func(p *P) { p.TotalCash = math.Inf(1) }, "cash")
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"net_liquidation":4000`) || !strings.Contains(string(raw), `"settled_cash":900`) {
		t.Errorf("version 4 encodes version 3's fields inline beside the cash: %s", raw)
	}
}

func TestLimitOrderLifecycleValidate(t *testing.T) {
	v := validLimitOrderLifecycle()
	type P = event.LimitOrderLifecyclePayload
	expectInvalid(t, "identity", v, func(p *P) { p.ClientOrderID = "" }, "client order id")
	expectInvalid(t, "order id", v, func(p *P) { p.OrderID = "" }, "order id")
	expectInvalid(t, "updated", v, func(p *P) { p.Status = event.OrderLifecycleStatusUpdated }, "status")
	expectInvalid(t, "time", v, func(p *P) { p.OccurredAt = time.Time{} }, "occurred at")
	for status, terminal := range map[string]bool{
		event.OrderLifecycleStatusSubmitted: false, event.OrderLifecycleStatusCancelPending: false,
		event.OrderLifecycleStatusCanceled: true, event.OrderLifecycleStatusInvalid: true,
	} {
		p := v
		p.Status = status
		if err := p.Validate(); err != nil {
			t.Errorf("%s: %v", status, err)
		}
		if p.Terminal() != terminal {
			t.Errorf("%s: Terminal() = %v", status, !terminal)
		}
	}
}

func TestTargetWeightFillValidate(t *testing.T) {
	v := validTargetWeightFill()
	type P = event.TargetWeightFillPayload
	expectInvalid(t, "fill id", v, func(p *P) { p.FillID = "" }, "fill id")
	expectInvalid(t, "price", v, func(p *P) { p.Price = 0 }, "price")
	expectInvalid(t, "commission", v, func(p *P) { p.Commission = -0.01 }, "commission")
	expectInvalid(t, "time", v, func(p *P) { p.FilledAt = time.Time{} }, "filled at")
	expectInvalid(t, "quantity", v, func(p *P) { p.Quantity = 0 }, "quantity")
}

// TestSameLimitPriceIsWithinHalfACent: two limit prices are the same within
// half the cent a limit is rounded to, and a NaN is the same as nothing
// (.greptile/rules.md, "Numbers and money").
func TestSameLimitPriceIsWithinHalfACent(t *testing.T) {
	for _, c := range []struct {
		a, b float64
		want bool
	}{
		{401.5, 401.5, true},
		{401.5, 401.500000001, true},
		{401.5, 401.504, true},
		{401.5, 401.51, false},
		{401.5, 401.49, false},
		{401.5, math.NaN(), false},
	} {
		if got := event.SameLimitPrice(c.a, c.b); got != c.want {
			t.Errorf("SameLimitPrice(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestALifecycleReportDescribesAnOrderAsReleased: a report describes a
// released order when it names its client order ID, account, instrument,
// side and quantity, and its limit within half a cent.
func TestALifecycleReportDescribesAnOrderAsReleased(t *testing.T) {
	o := event.OrderReleasedPayload{ClientOrderID: "tw-1", AccountID: "joint", InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 9, LimitPrice: 101}
	report := event.LimitOrderLifecyclePayload{ClientOrderID: "tw-1", AccountID: "joint", InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 9, LimitPrice: 101.000000001}
	if !report.Describes(o) {
		t.Fatalf("%+v does not describe %+v", report, o)
	}
	for name, mutate := range map[string]func(*event.LimitOrderLifecyclePayload){
		"another order":      func(p *event.LimitOrderLifecyclePayload) { p.ClientOrderID = "tw-2" },
		"another account":    func(p *event.LimitOrderLifecyclePayload) { p.AccountID = "ira" },
		"another instrument": func(p *event.LimitOrderLifecyclePayload) { p.InstrumentID = "QQQ" },
		"the other side":     func(p *event.LimitOrderLifecyclePayload) { p.Side = event.OrderSideSell },
		"another quantity":   func(p *event.LimitOrderLifecyclePayload) { p.Quantity = 8 },
		"a cent away":        func(p *event.LimitOrderLifecyclePayload) { p.LimitPrice = 101.01 },
	} {
		p := report
		mutate(&p)
		if p.Describes(o) {
			t.Errorf("%s: %+v describes %+v", name, p, o)
		}
	}
}
