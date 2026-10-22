package event_test

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

var twSession = time.Date(2025, time.March, 4, 21, 0, 0, 0, time.UTC)

func validTargetWeightConfiguration() event.TargetWeightConfigurationPayload {
	return event.TargetWeightConfigurationPayload{
		StrategyID: event.TargetWeightStrategyBoost,
		AccountID:  "joint",
		Mode:       event.TradingModeDryRun,
		Adapter:    event.AdapterIBKR,
		Boost: event.BoostConfiguration{
			InstrumentID:     "SPY",
			InvestedFraction: 0.98,
			BoostSize:        1.5,
			RSIPeriod:        2,
			RSIEntry:         10,
			TrendDays:        200,
			ExitSMA:          5,
			WarmUpStart:      "1998-01-02",
		},
		Limits: event.MarginLimits{
			MaxExposure:          1.5,
			MaxOrdersPerDecision: 1,
			MaxOrderFraction:     0.6,
			PriceBand:            0.05,

			ExcessLiquidityWarning:      0.4,
			ExcessLiquidityUrgent:       0.2,
			ExcessLiquidityStaleMinutes: 30,
		},
		Execution: event.ExecutionConfiguration{MaxSnapshotAge: 30},
	}
}

func validAccountPositionsSnapshot() event.AccountPositionsSnapshotPayload {
	return event.AccountPositionsSnapshotPayload{
		AccountID:      "joint",
		AsOf:           twSession,
		NetLiquidation: 4000,
		Currency:       "USD",
		Positions:      []event.AccountPosition{{InstrumentID: "SPY", Quantity: 7}},
	}
}

func validMarkRecorded() event.MarkRecordedPayload {
	return event.MarkRecordedPayload{InstrumentID: "SPY", AsOf: twSession, Price: 560.25, Source: event.AdapterIBKR}
}

func validTargetsDecided() event.TargetsDecidedPayload {
	return event.TargetsDecidedPayload{
		AccountID:        "joint",
		StrategyID:       event.TargetWeightStrategyBoost,
		DecisionTime:     twSession,
		SessionPeriodEnd: twSession,
		Outcome:          event.TargetsOutcomeDecided,
		Targets:          []event.TargetWeight{{InstrumentID: "SPY", Weight: 1.47}},
		Boost: event.BoostFigures{
			InstrumentID: "SPY", Close: 555, RSI: 4.2, RSIReady: true,
			TrendMean: 520, TrendMeanReady: true, ExitMean: 561, ExitMeanReady: true,
			LatchedBefore: false, LatchedAfter: true, TargetInForce: 0.98, TargetInForceSet: true,
		},
	}
}

// validTargetsDecidedWatching is validTargetsDecided watching an order
// cancelled with part filled, its executions recorded.
func validTargetsDecidedWatching() event.TargetsDecidedPayload {
	p := validTargetsDecided()
	p.Boost.Watched = event.BoostWatchedOrder{ClientOrderID: "tw-1", Side: event.OrderSideBuy, Released: 39, LimitPrice: 105, Filled: 20,
		Fills:  []event.BoostWatchedFill{{FillID: "f1", Quantity: 20, Price: 104, Commission: 1, FilledAt: twSession.Add(-2 * time.Hour)}},
		Status: event.OrderLifecycleStatusCanceled, EndedAt: twSession.Add(-time.Hour)}
	p.Boost.WorkingOrders = []event.BoostWatchedOrder{{ClientOrderID: "tw-0", Side: event.OrderSideBuy, Released: 9, LimitPrice: 100, Filled: 4,
		Fills: []event.BoostWatchedFill{{FillID: "f0", Quantity: 4, Price: 99, FilledAt: twSession.Add(-3 * time.Hour)}}}}
	p.Boost.RetiredOrders = []event.BoostRetiredOrder{{ClientOrderID: "tw-r", Side: event.OrderSideSell, Released: 5, LimitPrice: 101, RetiredAt: twSession.Add(-4 * time.Hour)}}
	return p
}

// validTargetsDecidedSaleWorking is a decision beside the watched and
// working orders that records the run's own live sale, named and part
// filled.
func validTargetsDecidedSaleWorking() event.TargetsDecidedPayload {
	p := validTargetsDecidedWatching()
	p.Boost.RestoredSale = &event.BoostRestoredSale{IncidentID: "engine-state:joint:1", Attempted: true, RetiredAttempts: 1, Order: namedSaleOrder()}
	return p
}

func validTargetsWithheld() event.TargetsDecidedPayload {
	p := validTargetsDecided()
	p.Outcome = event.TargetsOutcomeWithheld
	p.WithheldReason = event.WithheldMissingBars
	p.Detail = "no SPY bar in the session closing 2025-03-04"
	p.Targets = nil
	p.Boost = event.BoostFigures{InstrumentID: "SPY", LatchedBefore: true, LatchedAfter: true, TargetInForce: 1.47, TargetInForceSet: true}
	return p
}

func validOrderPlanned() event.OrderPlannedPayload {
	return event.OrderPlannedPayload{
		AccountID: "joint", Tag: "targets-decided:joint:2025-03-04", ClientOrderID: "tw-0123456789abcdef01234567",
		InstrumentID: "SPY", Side: event.OrderSideBuy, Phase: event.OrderSideBuy, OrderType: event.OrderTypeLimit,
		Quantity: 3, LimitPrice: 588.26, TargetWeight: 1.47, NetLiquidation: 4000, HeldQuantity: 7,
		ReferencePrice: 560.25, PriceBand: 0.05,
	}
}

func validRiskFigures() event.RiskFigures {
	return event.RiskFigures{
		NetLiquidation: 4000, ReferencePrice: 560.25, ExposureBefore: 0.98, ExposureAfter: 1.4, OrderFraction: 0.42,
		Mark: 560, MarkSet: true, MarkDeviation: 0.0004, OrdersInDecision: 1,
	}
}

func validOrderReleased() event.OrderReleasedPayload {
	return event.OrderReleasedPayload{
		AccountID: "joint", PlanID: "order-planned:tw-1", ClientOrderID: "tw-1", Tag: "targets-decided:joint:2025-03-04",
		InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 3, LimitPrice: 588.26,
		Mode: event.TradingModeDryRun, Send: false, Figures: validRiskFigures(),
	}
}

func validOrderRefused() event.OrderRefusedPayload {
	return event.OrderRefusedPayload{
		AccountID: "joint", PlanID: "order-planned:tw-1", ClientOrderID: "tw-1", Tag: "targets-decided:joint:2025-03-04",
		InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 3, LimitPrice: 588.26,
		Reason: event.RefusedMarginExposure, Detail: "exposure after 1.5001 exceeds 1.5", Figures: validRiskFigures(),
	}
}

func validDegradedEngineState() event.EngineStatePayload {
	return event.EngineStatePayload{
		State: event.EngineStateDegraded, Reason: event.EngineStateReasonRiskLimitBreached,
		Detail: "order tw-1 refused: margin-exposure", ScopeInstrumentIDs: []string{"SPY"},
	}
}

type validator interface{ Validate() error }

// expectInvalid checks that mutate makes p invalid with a message containing want.
func expectInvalid[T validator](t *testing.T, name string, valid T, mutate func(*T), want string) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		p := valid
		mutate(&p)
		err := p.Validate()
		if err == nil {
			t.Fatalf("Validate accepted %+v", p)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	})
}

func TestTargetWeightPayloadFixturesAreValid(t *testing.T) {
	for _, p := range []validator{
		validTargetWeightConfiguration(), validAccountPositionsSnapshot(), validMarkRecorded(),
		validTargetsDecided(), validTargetsWithheld(), validOrderPlanned(), validOrderReleased(),
		validOrderRefused(), validDegradedEngineState(),
	} {
		if err := p.Validate(); err != nil {
			t.Errorf("%T: %v", p, err)
		}
	}
}

func TestTargetWeightConfigurationValidate(t *testing.T) {
	v := validTargetWeightConfiguration()
	type P = event.TargetWeightConfigurationPayload
	expectInvalid(t, "strategy", v, func(p *P) { p.StrategyID = "turtle" }, "strategy")
	expectInvalid(t, "account", v, func(p *P) { p.AccountID = "" }, "account")
	expectInvalid(t, "mode", v, func(p *P) { p.Mode = "yolo" }, "mode")
	expectInvalid(t, "adapter", v, func(p *P) { p.Adapter = "lean" }, "adapter")
	expectInvalid(t, "instrument", v, func(p *P) { p.Boost.InstrumentID = "" }, "instrument")
	expectInvalid(t, "invested zero", v, func(p *P) { p.Boost.InvestedFraction = 0 }, "invested")
	expectInvalid(t, "invested above one", v, func(p *P) { p.Boost.InvestedFraction = 1.01 }, "invested")
	expectInvalid(t, "invested nan", v, func(p *P) { p.Boost.InvestedFraction = math.NaN() }, "invested")
	expectInvalid(t, "boost size", v, func(p *P) { p.Boost.BoostSize = 0.9 }, "boost size")
	expectInvalid(t, "rsi period", v, func(p *P) { p.Boost.RSIPeriod = 0 }, "rsi period")
	expectInvalid(t, "rsi entry", v, func(p *P) { p.Boost.RSIEntry = 100 }, "rsi entry")
	expectInvalid(t, "trend days", v, func(p *P) { p.Boost.TrendDays = 0 }, "trend days")
	expectInvalid(t, "exit sma", v, func(p *P) { p.Boost.ExitSMA = 0 }, "exit sma")
	expectInvalid(t, "warm-up", v, func(p *P) { p.Boost.WarmUpStart = "1998-1-2" }, "warm-up")
	expectInvalid(t, "max exposure", v, func(p *P) { p.Limits.MaxExposure = 0 }, "max exposure")
	expectInvalid(t, "max orders", v, func(p *P) { p.Limits.MaxOrdersPerDecision = 0 }, "max orders")
	expectInvalid(t, "max order fraction", v, func(p *P) { p.Limits.MaxOrderFraction = math.Inf(1) }, "max order fraction")
	expectInvalid(t, "price band zero", v, func(p *P) { p.Limits.PriceBand = 0 }, "price band")
	expectInvalid(t, "price band one", v, func(p *P) { p.Limits.PriceBand = 1 }, "price band")
	expectInvalid(t, "paper on a live account", v, func(p *P) { p.Mode, p.AccountID = event.TradingModePaper, "U1234567" }, `begins "DU"`)
	expectInvalid(t, "live on a paper account", v, func(p *P) { p.Mode, p.AccountID = event.TradingModeLive, "DU1234567" }, "paper account")
	for _, ok := range []struct{ mode, account string }{
		{event.TradingModePaper, "DU1234567"}, {event.TradingModeLive, "U1234567"},
		{event.TradingModeDryRun, "DU1234567"}, {event.TradingModeDryRun, "U1234567"},
	} {
		p := v
		p.Mode, p.AccountID = ok.mode, ok.account
		if err := p.Validate(); err != nil {
			t.Errorf("mode %s with account %s: %v", ok.mode, ok.account, err)
		}
	}
}

// TestTargetWeightConfigurationHashStability pins the fixture's hash (ADR
// 0016): changing the pin is a deliberate act that accompanies a schema bump
// or an encoding change.
func TestTargetWeightConfigurationHashStability(t *testing.T) {
	got := event.TargetWeightConfigurationHash(validTargetWeightConfiguration())
	const want = "sha256:cbe13a7346255f651df696fa89851de0ae3b4b628b8abfe3233208d75fdd2b45"
	if got != want {
		t.Fatalf("TargetWeightConfigurationHash = %s, want %s", got, want)
	}
}

func TestTargetWeightConfigurationHashChangesWithEveryField(t *testing.T) {
	base := event.TargetWeightConfigurationHash(validTargetWeightConfiguration())
	type P = event.TargetWeightConfigurationPayload
	for name, mutate := range map[string]func(*P){
		"strategy":   func(p *P) { p.StrategyID = "x" },
		"account":    func(p *P) { p.AccountID = "x" },
		"mode":       func(p *P) { p.Mode = event.TradingModePaper },
		"adapter":    func(p *P) { p.Adapter = "x" },
		"instrument": func(p *P) { p.Boost.InstrumentID = "QQQ" },
		"invested":   func(p *P) { p.Boost.InvestedFraction = 0.97 },
		"boost":      func(p *P) { p.Boost.BoostSize = 2 },
		"rsi period": func(p *P) { p.Boost.RSIPeriod = 3 },
		"rsi entry":  func(p *P) { p.Boost.RSIEntry = 5 },
		"trend":      func(p *P) { p.Boost.TrendDays = 100 },
		"exit":       func(p *P) { p.Boost.ExitSMA = 10 },
		"warm-up":    func(p *P) { p.Boost.WarmUpStart = "1999-01-04" },
		"exposure":   func(p *P) { p.Limits.MaxExposure = 1.6 },
		"orders":     func(p *P) { p.Limits.MaxOrdersPerDecision = 2 },
		"fraction":   func(p *P) { p.Limits.MaxOrderFraction = 0.5 },
		"band":       func(p *P) { p.Limits.PriceBand = 0.04 },
		"el warning": func(p *P) { p.Limits.ExcessLiquidityWarning = 0.45 },
		"el urgent":  func(p *P) { p.Limits.ExcessLiquidityUrgent = 0.25 },
		"el stale":   func(p *P) { p.Limits.ExcessLiquidityStaleMinutes = 20 },
	} {
		p := validTargetWeightConfiguration()
		mutate(&p)
		if event.TargetWeightConfigurationHash(p) == base {
			t.Errorf("changing %s left the hash unchanged", name)
		}
	}
}

func TestAccountPositionsSnapshotValidate(t *testing.T) {
	v := validAccountPositionsSnapshot()
	type P = event.AccountPositionsSnapshotPayload
	expectInvalid(t, "account", v, func(p *P) { p.AccountID = "" }, "account")
	expectInvalid(t, "as of", v, func(p *P) { p.AsOf = time.Time{} }, "as of")
	// The pending-sell rule orders snapshots and fills by the whole second
	// IBKR stamps; a finer time would be read as an order it cannot show.
	expectInvalid(t, "sub-second as of", v, func(p *P) { p.AsOf = p.AsOf.Add(time.Millisecond) }, "whole second")
	expectInvalid(t, "net liquidation zero", v, func(p *P) { p.NetLiquidation = 0 }, "net liquidation")
	expectInvalid(t, "net liquidation nan", v, func(p *P) { p.NetLiquidation = math.NaN() }, "net liquidation")
	expectInvalid(t, "currency", v, func(p *P) { p.Currency = "" }, "currency")
	expectInvalid(t, "zero quantity", v, func(p *P) { p.Positions = []event.AccountPosition{{InstrumentID: "SPY"}} }, "quantity")
	expectInvalid(t, "empty instrument", v, func(p *P) {
		p.Positions = []event.AccountPosition{{InstrumentID: "", Quantity: 1}}
	}, "instrument")
	expectInvalid(t, "unsorted", v, func(p *P) {
		p.Positions = []event.AccountPosition{{InstrumentID: "SPY", Quantity: 1}, {InstrumentID: "IEF", Quantity: 1}}
	}, "ascending")
	empty := v
	empty.Positions = nil
	if err := empty.Validate(); err != nil {
		t.Errorf("an account holding only cash is valid: %v", err)
	}
	if got := v.Quantity("SPY"); got != 7 {
		t.Errorf("Quantity(SPY) = %d, want 7", got)
	}
	if got := v.Quantity("IEF"); got != 0 {
		t.Errorf("Quantity(IEF) = %d, want 0", got)
	}
}

func TestMarkRecordedValidate(t *testing.T) {
	v := validMarkRecorded()
	type P = event.MarkRecordedPayload
	expectInvalid(t, "instrument", v, func(p *P) { p.InstrumentID = "" }, "instrument")
	expectInvalid(t, "as of", v, func(p *P) { p.AsOf = time.Time{} }, "as of")
	expectInvalid(t, "price", v, func(p *P) { p.Price = 0 }, "price")
	expectInvalid(t, "price inf", v, func(p *P) { p.Price = math.Inf(1) }, "price")
	expectInvalid(t, "source", v, func(p *P) { p.Source = "" }, "source")
}

func TestTargetsDecidedValidate(t *testing.T) {
	v := validTargetsDecided()
	type P = event.TargetsDecidedPayload
	expectInvalid(t, "account", v, func(p *P) { p.AccountID = "" }, "account")
	expectInvalid(t, "strategy", v, func(p *P) { p.StrategyID = "" }, "strategy")
	expectInvalid(t, "decision time", v, func(p *P) { p.DecisionTime = time.Time{} }, "decision time")
	expectInvalid(t, "session", v, func(p *P) { p.SessionPeriodEnd = time.Time{} }, "session")
	expectInvalid(t, "decision before session", v, func(p *P) { p.DecisionTime = twSession.Add(-time.Hour) }, "before")
	expectInvalid(t, "outcome", v, func(p *P) { p.Outcome = "maybe" }, "outcome")
	expectInvalid(t, "decided without targets", v, func(p *P) { p.Targets = nil }, "target")
	expectInvalid(t, "decided with reason", v, func(p *P) { p.WithheldReason = event.WithheldMissingBars }, "reason")
	expectInvalid(t, "negative weight", v, func(p *P) { p.Targets = []event.TargetWeight{{InstrumentID: "SPY", Weight: -0.1}} }, "weight")
	expectInvalid(t, "nan weight", v, func(p *P) { p.Targets = []event.TargetWeight{{InstrumentID: "SPY", Weight: math.NaN()}} }, "weight")
	expectInvalid(t, "empty target instrument", v, func(p *P) { p.Targets = []event.TargetWeight{{Weight: 1}} }, "instrument")
	expectInvalid(t, "unsorted targets", v, func(p *P) {
		p.Targets = []event.TargetWeight{{InstrumentID: "SPY", Weight: 1}, {InstrumentID: "IEF", Weight: 1}}
	}, "ascending")
	expectInvalid(t, "decided without close", v, func(p *P) { p.Boost.Close = 0 }, "close")
	expectInvalid(t, "boost instrument", v, func(p *P) { p.Boost.InstrumentID = "" }, "instrument")
	expectInvalid(t, "rsi without ready", v, func(p *P) { p.Boost.RSIReady = false }, "rsi")
	expectInvalid(t, "trend without ready", v, func(p *P) { p.Boost.TrendMeanReady = false }, "trend")
	expectInvalid(t, "exit without ready", v, func(p *P) { p.Boost.ExitMeanReady = false }, "exit")
	expectInvalid(t, "rsi nan", v, func(p *P) { p.Boost.RSI = math.NaN() }, "rsi")
	expectInvalid(t, "in force without set", v, func(p *P) { p.Boost.TargetInForceSet = false }, "in force")
	expectInvalid(t, "in force nan", v, func(p *P) { p.Boost.TargetInForce = math.NaN() }, "in force")

	w := validTargetsWithheld()
	expectInvalid(t, "withheld with targets", w, func(p *P) { p.Targets = v.Targets }, "target")
	expectInvalid(t, "withheld without reason", w, func(p *P) { p.WithheldReason = "" }, "reason")
	expectInvalid(t, "withheld unknown reason", w, func(p *P) { p.WithheldReason = "tired" }, "reason")
	expectInvalid(t, "withheld without detail", w, func(p *P) { p.Detail = "" }, "detail")
	expectInvalid(t, "negative close", w, func(p *P) { p.Boost.Close = -1 }, "close")
	for _, reason := range []string{
		event.WithheldStaleAccountSnapshot, event.WithheldMissingUniverse,
		event.WithheldInsufficientTrendHistory, event.WithheldNoEligibleNames, event.WithheldHistoriesMissing,
		event.WithheldLabelsMissing,
	} {
		other := w
		other.WithheldReason = reason
		if err := other.Validate(); err != nil {
			t.Errorf("%s is a recognised reason: %v", reason, err)
		}
	}
}

func TestOrderPlannedValidate(t *testing.T) {
	v := validOrderPlanned()
	type P = event.OrderPlannedPayload
	expectInvalid(t, "account", v, func(p *P) { p.AccountID = "" }, "account")
	expectInvalid(t, "tag", v, func(p *P) { p.Tag = "" }, "tag")
	expectInvalid(t, "client order id", v, func(p *P) { p.ClientOrderID = "" }, "client order id")
	expectInvalid(t, "instrument", v, func(p *P) { p.InstrumentID = "" }, "instrument")
	expectInvalid(t, "side", v, func(p *P) { p.Side = "short" }, "side")
	expectInvalid(t, "phase", v, func(p *P) { p.Phase = "later" }, "phase")
	expectInvalid(t, "type", v, func(p *P) { p.OrderType = "market" }, "order type")
	expectInvalid(t, "quantity", v, func(p *P) { p.Quantity = 0 }, "quantity")
	expectInvalid(t, "limit", v, func(p *P) { p.LimitPrice = 0 }, "limit")
	expectInvalid(t, "weight", v, func(p *P) { p.TargetWeight = -1 }, "target weight")
	expectInvalid(t, "net liquidation", v, func(p *P) { p.NetLiquidation = 0 }, "net liquidation")
	expectInvalid(t, "held", v, func(p *P) { p.HeldQuantity = -1 }, "held")
	expectInvalid(t, "reference", v, func(p *P) { p.ReferencePrice = math.NaN() }, "reference")
	expectInvalid(t, "band", v, func(p *P) { p.PriceBand = 1 }, "price band")
	expectInvalid(t, "buy limit below reference", v, func(p *P) { p.LimitPrice = 500 }, "limit")
	expectInvalid(t, "sell limit above reference", v, func(p *P) { p.Side = event.OrderSideSell; p.Phase = event.OrderSideSell }, "limit")
}

func TestRiskOrderValidate(t *testing.T) {
	r := validOrderReleased()
	type R = event.OrderReleasedPayload
	expectInvalid(t, "plan", r, func(p *R) { p.PlanID = "" }, "plan")
	expectInvalid(t, "tag", r, func(p *R) { p.Tag = "" }, "tag")
	expectInvalid(t, "account", r, func(p *R) { p.AccountID = "" }, "account")
	expectInvalid(t, "client", r, func(p *R) { p.ClientOrderID = "" }, "client order id")
	expectInvalid(t, "instrument", r, func(p *R) { p.InstrumentID = "" }, "instrument")
	expectInvalid(t, "side", r, func(p *R) { p.Side = "" }, "side")
	expectInvalid(t, "quantity", r, func(p *R) { p.Quantity = -3 }, "quantity")
	expectInvalid(t, "limit", r, func(p *R) { p.LimitPrice = math.NaN() }, "limit")
	expectInvalid(t, "mode", r, func(p *R) { p.Mode = "" }, "mode")
	expectInvalid(t, "dry-run sends", r, func(p *R) { p.Send = true }, "dry-run")
	expectInvalid(t, "figures", r, func(p *R) { p.Figures.ExposureAfter = math.NaN() }, "exposure after")
	expectInvalid(t, "mark without set", r, func(p *R) { p.Figures.MarkSet = false }, "mark")
	expectInvalid(t, "net liquidation", r, func(p *R) { p.Figures.NetLiquidation = 0 }, "net liquidation")
	expectInvalid(t, "orders", r, func(p *R) { p.Figures.OrdersInDecision = -1 }, "orders")
	paper := r
	paper.Mode = event.TradingModePaper
	paper.Send = true
	if err := paper.Validate(); err != nil {
		t.Errorf("a paper release may be sent: %v", err)
	}

	f := validOrderRefused()
	type F = event.OrderRefusedPayload
	expectInvalid(t, "reason", f, func(p *F) { p.Reason = "nope" }, "reason")
	expectInvalid(t, "detail", f, func(p *F) { p.Detail = "" }, "detail")
	expectInvalid(t, "refused plan", f, func(p *F) { p.PlanID = "" }, "plan")
	expectInvalid(t, "refused figures", f, func(p *F) { p.Figures.Mark = math.Inf(1) }, "mark")
	untagged := f
	untagged.Tag = ""
	untagged.Reason = event.RefusedUndecidedOrder
	if err := untagged.Validate(); err != nil {
		t.Errorf("an untagged order can be refused (that is the refusal): %v", err)
	}
	for _, reason := range []string{
		event.RefusedUndecidedOrder, event.RefusedMarginExposure, event.RefusedOrderCount, event.RefusedOrderSize,
		event.RefusedPriceBand, event.RefusedMissingMark, event.RefusedEngineDegraded, event.RefusedEngineHalted,
	} {
		p := f
		p.Reason = reason
		if err := p.Validate(); err != nil {
			t.Errorf("reason %s: %v", reason, err)
		}
	}
}

func TestEngineStateScopes(t *testing.T) {
	d := validDegradedEngineState()
	type P = event.EngineStatePayload
	expectInvalid(t, "degraded without scope", d, func(p *P) { p.ScopeInstrumentIDs = nil }, "scope")
	expectInvalid(t, "halted with scope", d, func(p *P) { p.State = event.EngineStateHalted }, "scope")
	expectInvalid(t, "unsorted scope", d, func(p *P) { p.ScopeInstrumentIDs = []string{"SPY", "IEF"} }, "ascending")
	expectInvalid(t, "empty scope id", d, func(p *P) { p.ScopeInstrumentIDs = []string{""} }, "empty")

	if err := d.ValidateSchema(event.EngineStateScopedVersion); err != nil {
		t.Errorf("degraded is valid at schema 2: %v", err)
	}
	if err := d.ValidateSchema(event.EngineStateSchemaVersion); err == nil {
		t.Error("schema 1 knows only halted with no scope; degraded must be refused")
	}
	if err := d.ValidateSchema(3); err == nil {
		t.Error("an unknown schema version must be refused")
	}
	halted := P{State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "untagged order"}
	if err := halted.ValidateSchema(event.EngineStateScopedVersion); err != nil {
		t.Errorf("halted, unscoped, at schema 2: %v", err)
	}
	// risk-limit-breached arrived with schema 2; a version-1 reader knows
	// only campaign-without-protective-stop, so a version-1 record cannot
	// carry it.
	if err := halted.ValidateSchema(event.EngineStateSchemaVersion); err == nil {
		t.Error("risk-limit-breached must be refused at schema 1")
	}
	turtle := P{State: event.EngineStateHalted, Reason: event.EngineStateReasonCampaignWithoutProtectiveStop, Detail: "no stop"}
	for _, version := range []uint32{event.EngineStateSchemaVersion, event.EngineStateScopedVersion} {
		if err := turtle.ValidateSchema(version); err != nil {
			t.Errorf("the Turtle halt at schema %d: %v", version, err)
		}
	}
	if err := (P{}).ValidateSchema(event.EngineStateScopedVersion); err == nil {
		t.Error("an empty payload must be refused")
	}
	// A halted record without scope keeps its schema-1 bytes: the scope
	// fields are omitted, so the Turtle reducer's halts are unchanged.
	raw, err := json.Marshal(halted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "scope") {
		t.Errorf("unscoped halt encodes a scope field: %s", raw)
	}
}

func TestMarginLimitsAndModesStandAlone(t *testing.T) {
	if err := validTargetWeightConfiguration().Limits.Validate(); err != nil {
		t.Errorf("valid limits refused: %v", err)
	}
	if err := (event.MarginLimits{}).Validate(); err == nil || !strings.Contains(err.Error(), "margin limits") {
		t.Errorf("zero limits accepted: %v", err)
	}
	for mode, want := range map[string]bool{event.TradingModeDryRun: true, event.TradingModePaper: true, event.TradingModeLive: true, "": false, "yolo": false} {
		if event.ValidTradingMode(mode) != want {
			t.Errorf("ValidTradingMode(%q) != %v", mode, want)
		}
	}
}

// TestARefusalAlertBelongsToAKillSwitchOrStaleRefusal: the alert flag marks
// the one refusal that pages the owner for a kill-switch engagement (ADR
// 0026, point 3, kill switch: "alert once") or a stale episode (ADR 0019,
// amendment of 2026-10-17, reading 10) or a pending cash decrease (ADR
// 0019, amended 2026-10-21); no other reason carries it.
func TestARefusalAlertBelongsToAKillSwitchOrStaleRefusal(t *testing.T) {
	for _, reason := range []string{event.RefusedKillSwitch, event.RefusedStaleBrokerSnapshot, event.RefusedPendingCashDecrease} {
		p := validOrderRefused()
		p.Reason, p.Alert = reason, true
		if err := p.ValidateSchema(p.SchemaVersion()); err != nil {
			t.Errorf("%s with the alert: %v", reason, err)
		}
	}
	expectInvalid(t, "alert on another reason", validOrderRefused(), func(p *event.OrderRefusedPayload) { p.Alert = true }, "alert")
}

// TestBoostReplanValidate: a re-plan names its cause, refused or
// part-filled; a part-fill names the order and filled shares strictly
// between none and the released shares, and a refusal names neither (ADR
// 0026, point 2, "Boost", amended 2026-10-18).
func TestBoostReplanValidate(t *testing.T) {
	v := validTargetsDecided()
	v.Boost.Replan = event.BoostReplan{Cause: event.ReplanCausePartFilled, ClientOrderID: "tw-1", Filled: 20, Released: 39}
	if err := v.Validate(); err != nil {
		t.Fatalf("a part-fill re-plan is valid: %v", err)
	}
	refused := v
	refused.Boost.Replan = event.BoostReplan{Cause: event.ReplanCauseRefused}
	if err := refused.Validate(); err != nil {
		t.Fatalf("a refusal re-plan is valid: %v", err)
	}
	type P = event.TargetsDecidedPayload
	expectInvalid(t, "unknown cause", v, func(p *P) { p.Boost.Replan.Cause = "bored" }, "re-plan cause")
	expectInvalid(t, "figures without a cause", v, func(p *P) { p.Boost.Replan.Cause = "" }, "re-plan cause")
	expectInvalid(t, "part-fill without order", v, func(p *P) { p.Boost.Replan.ClientOrderID = "" }, "order")
	expectInvalid(t, "part-fill filled nothing", v, func(p *P) { p.Boost.Replan.Filled = 0 }, "filled")
	expectInvalid(t, "part-fill filled in full", v, func(p *P) { p.Boost.Replan.Filled = 39 }, "filled")
	unfilled := v
	unfilled.Boost.Replan = event.BoostReplan{Cause: event.ReplanCauseUnfilled, ClientOrderID: "tw-1", Released: 39}
	if err := unfilled.Validate(); err != nil {
		t.Fatalf("an unfilled re-plan is valid: %v", err)
	}
	expectInvalid(t, "unfilled with a fill", unfilled, func(p *P) { p.Boost.Replan.Filled = 1 }, "filled")
	expectInvalid(t, "unfilled without order", unfilled, func(p *P) { p.Boost.Replan.ClientOrderID = "" }, "order")
	expectInvalid(t, "unfilled released nothing", unfilled, func(p *P) { p.Boost.Replan.Released = 0 }, "released")
	expectInvalid(t, "refusal with figures", refused, func(p *P) { p.Boost.Replan.Filled = 1 }, "refus")
}

// TestBoostGapSessionsValidate: a Boost decision's gap sessions are dates,
// strictly ascending and before the session it decided, whose bar the
// indicators read (ADR 0026, point 5, amended 2026-10-18).
func TestBoostGapSessionsValidate(t *testing.T) {
	v := validTargetsDecided()
	v.Boost.GapSessions = []string{"2025-02-28", "2025-03-03"}
	if err := v.Validate(); err != nil {
		t.Fatalf("gap sessions before the session are valid: %v", err)
	}
	type P = event.TargetsDecidedPayload
	expectInvalid(t, "not a date", v, func(p *P) { p.Boost.GapSessions = []string{"yesterday"} }, "gap session")
	expectInvalid(t, "descending", v, func(p *P) { p.Boost.GapSessions = []string{"2025-03-03", "2025-02-28"} }, "ascending")
	expectInvalid(t, "the session itself", v, func(p *P) { p.Boost.GapSessions = []string{"2025-03-04"} }, "before")
	m := validTargetsDecided()
	m.Boost = event.BoostFigures{}
	if !m.Boost.IsZero() || v.Boost.IsZero() {
		t.Error("IsZero reports only empty Boost figures")
	}
}

// TestBoostRejectedReplanValidate: a venue rejection's re-plan, and the
// re-plan withheld after a second rejection, name the order and its released
// shares, with fewer filled; the withheld one is a rejection only (ADR 0026,
// point 2, "Boost", amended 2026-10-18).
func TestBoostRejectedReplanValidate(t *testing.T) {
	v := validTargetsDecided()
	v.Boost.Replan = event.BoostReplan{Cause: event.ReplanCauseRejected, ClientOrderID: "tw-1", Released: 39}
	v.Boost.ReplanWithheld = event.BoostReplan{Cause: event.ReplanCauseRejected, ClientOrderID: "tw-0", Released: 39}
	if err := v.Validate(); err != nil {
		t.Fatalf("a rejection's re-plan and a withheld one are valid: %v", err)
	}
	type P = event.TargetsDecidedPayload
	expectInvalid(t, "rejected without order", v, func(p *P) { p.Boost.Replan.ClientOrderID = "" }, "order")
	expectInvalid(t, "rejected released nothing", v, func(p *P) { p.Boost.Replan.Released = 0 }, "released")
	expectInvalid(t, "rejected filled in full", v, func(p *P) { p.Boost.Replan.Filled = 39 }, "filled")
	expectInvalid(t, "withheld for a part-fill", v, func(p *P) {
		p.Boost.ReplanWithheld = event.BoostReplan{Cause: event.ReplanCausePartFilled, ClientOrderID: "tw-0", Filled: 1, Released: 39}
	}, "withheld")
	expectInvalid(t, "withheld without order", v, func(p *P) { p.Boost.ReplanWithheld.ClientOrderID = "" }, "order")
	if v.Boost.IsZero() {
		t.Error("figures with a withheld re-plan are not empty")
	}
}

// TestBoostWatchedOrderValidate: a watched order names its client order ID,
// side and positive limit, with fewer shares filled than released, and its
// status, when set, is canceled or invalid (ADR 0026, points 2 and 5,
// amended 2026-10-18).
func TestBoostWatchedOrderValidate(t *testing.T) {
	v := validTargetsDecided()
	filledAt := time.Date(2026, 3, 2, 19, 0, 0, 0, time.UTC)
	v.Boost.Watched = event.BoostWatchedOrder{ClientOrderID: "tw-1", Side: event.OrderSideBuy, Released: 39, LimitPrice: 105, Filled: 20,
		Fills: []event.BoostWatchedFill{
			{FillID: "f1", Quantity: 15, Price: 104, FilledAt: filledAt},
			{FillID: "f2", Quantity: 5, Price: 104.5, Commission: 1, FilledAt: filledAt.Add(time.Minute)},
		},
		Status: event.OrderLifecycleStatusCanceled, EndedAt: filledAt.Add(time.Hour)}
	if err := v.Validate(); err != nil {
		t.Fatalf("a watched order is valid: %v", err)
	}
	type P = event.TargetsDecidedPayload
	expectInvalid(t, "fills short of filled", v, func(p *P) { p.Boost.Watched.Fills = p.Boost.Watched.Fills[:1] }, "sum to")
	expectInvalid(t, "filled with no fills", v, func(p *P) { p.Boost.Watched.Fills = nil }, "sum to")
	expectInvalid(t, "a fill id twice", v, func(p *P) {
		p.Boost.Watched.Fills = slices.Clone(p.Boost.Watched.Fills)
		p.Boost.Watched.Fills[1].FillID = "f1"
	}, "fill id f1 twice")
	expectInvalid(t, "an unnamed fill", v, func(p *P) {
		p.Boost.Watched.Fills = slices.Clone(p.Boost.Watched.Fills)
		p.Boost.Watched.Fills[0].FillID = ""
	}, "fill id")
	expectInvalid(t, "an empty fill", v, func(p *P) {
		p.Boost.Watched.Fills = slices.Clone(p.Boost.Watched.Fills)
		p.Boost.Watched.Fills[0].Quantity, p.Boost.Watched.Fills[1].Quantity = 0, 20
	}, "positive quantity")
	expectInvalid(t, "a fill with no price", v, func(p *P) {
		p.Boost.Watched.Fills = slices.Clone(p.Boost.Watched.Fills)
		p.Boost.Watched.Fills[0].Price = 0
	}, "price")
	expectInvalid(t, "a negative commission", v, func(p *P) {
		p.Boost.Watched.Fills = slices.Clone(p.Boost.Watched.Fills)
		p.Boost.Watched.Fills[0].Commission = -1
	}, "commission")
	expectInvalid(t, "a fill at a fraction of a second", v, func(p *P) {
		p.Boost.Watched.Fills = slices.Clone(p.Boost.Watched.Fills)
		p.Boost.Watched.Fills[0].FilledAt = filledAt.Add(time.Millisecond)
	}, "whole second")
	expectInvalid(t, "ended with no status", v, func(p *P) { p.Boost.Watched.Status = "" }, "ended")
	expectInvalid(t, "a status with no end", v, func(p *P) { p.Boost.Watched.EndedAt = time.Time{} }, "ended")
	expectInvalid(t, "no order", v, func(p *P) { p.Boost.Watched.ClientOrderID = "" }, "client order ID")
	expectInvalid(t, "no side", v, func(p *P) { p.Boost.Watched.Side = "short" }, "buy or a sell")
	expectInvalid(t, "no limit", v, func(p *P) { p.Boost.Watched.LimitPrice = 0 }, "limit")
	expectInvalid(t, "filled in full", v, func(p *P) { p.Boost.Watched.Filled, p.Boost.Watched.Fills = 39, nil }, "fewer filled")
	expectInvalid(t, "negative fill", v, func(p *P) { p.Boost.Watched.Filled, p.Boost.Watched.Fills = -1, nil }, "fewer filled")
	expectInvalid(t, "working status", v, func(p *P) { p.Boost.Watched.Status = event.OrderLifecycleStatusSubmitted }, "status")
	// Fills whose sum wraps past the largest int64 to the recorded figure
	// are refused before the sum can wrap: 1 filled of 39, from fills of
	// MaxInt64, MaxInt64 and 3.
	expectInvalid(t, "fills that overflow their sum", v, func(p *P) {
		p.Boost.Watched.Filled = 1
		p.Boost.Watched.Fills = []event.BoostWatchedFill{
			{FillID: "f1", Quantity: math.MaxInt64, Price: 104, FilledAt: filledAt},
			{FillID: "f2", Quantity: math.MaxInt64, Price: 104, FilledAt: filledAt},
			{FillID: "f3", Quantity: 3, Price: 104, FilledAt: filledAt},
		}
	}, "more shares than released")
	if v.Boost.IsZero() {
		t.Error("figures with a watched order are not empty")
	}
}

// TestBoostWatchedOrderEqualAndFill: two records of a watched order are equal
// when every field and execution is, each instant the same in any UTC
// offset, and an execution restates the fill report it came from.
func TestBoostWatchedOrderEqualAndFill(t *testing.T) {
	w := validTargetsDecidedWatching().Boost.Watched
	moved := w
	moved.Fills = slices.Clone(w.Fills)
	moved.Fills[0].FilledAt = w.Fills[0].FilledAt.In(time.FixedZone("EST", -5*3600))
	moved.EndedAt = w.EndedAt.In(time.FixedZone("EST", -5*3600))
	if !w.Equal(moved) {
		t.Error("the same record in another offset is equal")
	}
	other := w
	other.Fills = slices.Clone(w.Fills)
	other.Fills[0].Commission = 2
	if w.Equal(other) || w.Equal(event.BoostWatchedOrder{}) || w.IsZero() || !(event.BoostWatchedOrder{}).IsZero() {
		t.Error("a record with another execution, or none, is not equal")
	}
	f := w.Fill("joint", "SPY", w.Fills[0])
	want := event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "tw-1", FillID: "f1", InstrumentID: "SPY", Side: event.OrderSideBuy,
		Quantity: 20, Price: 104, Commission: 1, FilledAt: w.Fills[0].FilledAt}
	if !f.SameExecution(want) {
		t.Errorf("Fill = %+v, want %+v", f, want)
	}
}

// TestBoostVenueRejectionsValidate: a Boost decision counts the venue's
// rejections in a row as a whole number, never below none and never above
// two, and names exactly the orders it counts, each once; figures with a
// count are not empty (ADR 0026, point 2, "Boost", amended 2026-10-18).
func TestBoostVenueRejectionsValidate(t *testing.T) {
	v := validTargetsDecided()
	v.Boost.VenueRejections = 2
	v.Boost.RejectedInARow = []event.BoostRejectedOrder{{ClientOrderID: "tw-0", VenueOrderID: "ib-0"}, {ClientOrderID: "tw-1"}}
	if err := v.Validate(); err != nil {
		t.Fatalf("two rejections in a row are valid: %v", err)
	}
	if (event.BoostFigures{VenueRejections: 1}).IsZero() || (event.BoostFigures{RejectedInARow: v.Boost.RejectedInARow}).IsZero() {
		t.Error("figures with a rejection count are not empty")
	}
	type P = event.TargetsDecidedPayload
	expectInvalid(t, "negative count", v, func(p *P) { p.Boost.VenueRejections = -1 }, "rejections")
	// Once reached, the count stays at two (ADR 0026, point 2, "Boost",
	// amended 2026-10-18).
	expectInvalid(t, "a count above two", v, func(p *P) {
		p.Boost.VenueRejections = 3
		p.Boost.RejectedInARow = append(slices.Clone(p.Boost.RejectedInARow), event.BoostRejectedOrder{ClientOrderID: "tw-2"})
	}, "two")
	expectInvalid(t, "a count naming fewer orders", v, func(p *P) { p.Boost.RejectedInARow = p.Boost.RejectedInARow[:1] }, "names 1")
	expectInvalid(t, "an order named without a count", v, func(p *P) { p.Boost.VenueRejections = 0 }, "names 2")
	expectInvalid(t, "unnamed", v, func(p *P) {
		p.Boost.RejectedInARow = []event.BoostRejectedOrder{{ClientOrderID: "tw-0"}, {VenueOrderID: "ib-1"}}
	}, "client order ID")
	expectInvalid(t, "named twice", v, func(p *P) {
		p.Boost.RejectedInARow = []event.BoostRejectedOrder{{ClientOrderID: "tw-0"}, {ClientOrderID: "tw-0"}}
	}, "twice")
}

// TestBoostUndecidedOrdersValidate: a Boost decision records the released
// orders the venue has not yet ruled on, each named once and none of them
// among the rejections counted; each but the watched one carries its side,
// shares and limit as released, so a restart reads the venue's reports on
// it, and the watched one, the latest released, comes last, with no
// execution or end, either of which is the venue's word on it (ADR 0026,
// points 2 and 5, amended 2026-10-18).
func TestBoostUndecidedOrdersValidate(t *testing.T) {
	v := validTargetsDecided()
	v.Boost.VenueRejections = 1
	v.Boost.RejectedInARow = []event.BoostRejectedOrder{{ClientOrderID: "tw-0"}}
	v.Boost.Watched = event.BoostWatchedOrder{ClientOrderID: "tw-2", Side: event.OrderSideSell, Released: 12, LimitPrice: 550}
	v.Boost.UndecidedOrders = []event.BoostUndecidedOrder{
		{ClientOrderID: "tw-1", Side: event.OrderSideBuy, Released: 37, LimitPrice: 105, VenueOrderID: "ib-1"},
		{ClientOrderID: "tw-2"},
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("a rejection, an undecided order and the watched order are valid: %v", err)
	}
	if (event.BoostFigures{UndecidedOrders: v.Boost.UndecidedOrders}).IsZero() {
		t.Error("figures with an undecided order are not empty")
	}
	type P = event.TargetsDecidedPayload
	at := func(i int, f func(*event.BoostUndecidedOrder)) func(*P) {
		return func(p *P) {
			p.Boost.UndecidedOrders = slices.Clone(p.Boost.UndecidedOrders)
			f(&p.Boost.UndecidedOrders[i])
		}
	}
	expectInvalid(t, "unnamed", v, at(0, func(o *event.BoostUndecidedOrder) { o.ClientOrderID = "" }), "names")
	expectInvalid(t, "named twice", v, at(0, func(o *event.BoostUndecidedOrder) { o.ClientOrderID = "tw-2" }), "twice")
	expectInvalid(t, "also rejected", v, at(0, func(o *event.BoostUndecidedOrder) { o.ClientOrderID = "tw-0" }), "both")
	expectInvalid(t, "without its release", v, at(0, func(o *event.BoostUndecidedOrder) { o.Released = 0 }), "released")
	expectInvalid(t, "with no side", v, at(0, func(o *event.BoostUndecidedOrder) { o.Side = "" }), "released")
	expectInvalid(t, "with no limit", v, at(0, func(o *event.BoostUndecidedOrder) { o.LimitPrice = 0 }), "released")
	expectInvalid(t, "the watched order with its release", v, at(1, func(o *event.BoostUndecidedOrder) { o.Side = event.OrderSideSell }), "watched")
	expectInvalid(t, "the watched order before another", v, func(p *P) {
		p.Boost.UndecidedOrders = []event.BoostUndecidedOrder{{ClientOrderID: "tw-2"}, {ClientOrderID: "tw-1", Side: event.OrderSideBuy, Released: 37, LimitPrice: 105}}
	}, "last")
	expectInvalid(t, "the watched order executed", v, func(p *P) {
		p.Boost.Watched.Filled = 1
		p.Boost.Watched.Fills = []event.BoostWatchedFill{{FillID: "f1", Quantity: 1, Price: 550, FilledAt: twSession.Add(-2 * time.Hour)}}
	}, "ruled")
	expectInvalid(t, "the watched order ended", v, func(p *P) {
		p.Boost.Watched.Status, p.Boost.Watched.EndedAt = event.OrderLifecycleStatusCanceled, twSession.Add(-time.Hour)
	}, "ruled")
}

// TestBoostClearedOrdersValidate: an order the owner settled as never sent
// records its release, as released, and no venue order ID, since no venue
// named it; it is named once and is not also undecided (ADR 0026, point 2,
// "Boost", amended 2026-10-18, and point 5).
func TestBoostClearedOrdersValidate(t *testing.T) {
	v := validTargetsDecided()
	v.Boost.UndecidedOrders = []event.BoostUndecidedOrder{{ClientOrderID: "tw-3", Side: event.OrderSideBuy, Released: 5, LimitPrice: 100}}
	v.Boost.ClearedOrders = []event.BoostUndecidedOrder{
		{ClientOrderID: "tw-1", Side: event.OrderSideSell, Released: 12, LimitPrice: 550},
		{ClientOrderID: "tw-2", Side: event.OrderSideBuy, Released: 37, LimitPrice: 105},
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("cleared orders beside an undecided one are valid: %v", err)
	}
	type P = event.TargetsDecidedPayload
	at := func(i int, f func(*event.BoostUndecidedOrder)) func(*P) {
		return func(p *P) {
			p.Boost.ClearedOrders = slices.Clone(p.Boost.ClearedOrders)
			f(&p.Boost.ClearedOrders[i])
		}
	}
	expectInvalid(t, "unnamed", v, at(0, func(o *event.BoostUndecidedOrder) { o.ClientOrderID = "" }), "cleared")
	expectInvalid(t, "named twice", v, at(0, func(o *event.BoostUndecidedOrder) { o.ClientOrderID = "tw-2" }), "twice")
	expectInvalid(t, "also undecided", v, at(0, func(o *event.BoostUndecidedOrder) { o.ClientOrderID = "tw-3" }), "undecided")
	expectInvalid(t, "without its release", v, at(0, func(o *event.BoostUndecidedOrder) { o.Released = 0 }), "released")
	expectInvalid(t, "with no side", v, at(0, func(o *event.BoostUndecidedOrder) { o.Side = "" }), "released")
	expectInvalid(t, "with no limit", v, at(0, func(o *event.BoostUndecidedOrder) { o.LimitPrice = 0 }), "released")
	expectInvalid(t, "with a venue order ID", v, at(0, func(o *event.BoostUndecidedOrder) { o.VenueOrderID = "ib-1" }), "venue")
}

// TestBoostRetiredOrdersValidate: an order retired as never sent records its
// release, as released, and when it was retired; it is named once and is
// in no other list but Watched, where it ended canceled with nothing filled
// (ADR 0026, point 2, "Boost", and point 5, amended 2026-10-22).
func TestBoostRetiredOrdersValidate(t *testing.T) {
	v := validTargetsDecided()
	v.Boost.UndecidedOrders = []event.BoostUndecidedOrder{{ClientOrderID: "tw-3", Side: event.OrderSideBuy, Released: 5, LimitPrice: 100}}
	v.Boost.ClearedOrders = []event.BoostUndecidedOrder{{ClientOrderID: "tw-4", Side: event.OrderSideBuy, Released: 6, LimitPrice: 100}}
	v.Boost.Watched = event.BoostWatchedOrder{ClientOrderID: "tw-2", Side: event.OrderSideBuy, Released: 37, LimitPrice: 105,
		Status: event.OrderLifecycleStatusCanceled, EndedAt: twSession.Add(-time.Hour)}
	v.Boost.RetiredOrders = []event.BoostRetiredOrder{
		{ClientOrderID: "tw-1", Side: event.OrderSideSell, Released: 12, LimitPrice: 550, RetiredAt: twSession.Add(-2 * time.Hour)},
		{ClientOrderID: "tw-2", Side: event.OrderSideBuy, Released: 37, LimitPrice: 105, RetiredAt: twSession.Add(-time.Hour)},
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("retired orders beside undecided and cleared ones, the watched one among them, are valid: %v", err)
	}
	if (event.BoostFigures{RetiredOrders: v.Boost.RetiredOrders}).IsZero() {
		t.Error("figures listing a retired order are not empty")
	}
	type P = event.TargetsDecidedPayload
	at := func(i int, f func(*event.BoostRetiredOrder)) func(*P) {
		return func(p *P) {
			p.Boost.RetiredOrders = slices.Clone(p.Boost.RetiredOrders)
			f(&p.Boost.RetiredOrders[i])
		}
	}
	expectInvalid(t, "unnamed", v, at(0, func(o *event.BoostRetiredOrder) { o.ClientOrderID = "" }), "retired")
	expectInvalid(t, "named twice", v, at(0, func(o *event.BoostRetiredOrder) { o.ClientOrderID = "tw-2" }), "twice")
	expectInvalid(t, "also undecided", v, at(0, func(o *event.BoostRetiredOrder) { o.ClientOrderID = "tw-3" }), "undecided")
	expectInvalid(t, "also cleared", v, at(0, func(o *event.BoostRetiredOrder) { o.ClientOrderID = "tw-4" }), "cleared")
	expectInvalid(t, "also rejected", v, func(p *P) {
		p.Boost.VenueRejections, p.Boost.RejectedInARow = 1, []event.BoostRejectedOrder{{ClientOrderID: "tw-1"}}
	}, "rejected")
	expectInvalid(t, "also working", v, func(p *P) {
		p.Boost.WorkingOrders = []event.BoostWatchedOrder{{ClientOrderID: "tw-1", Side: event.OrderSideSell, Released: 12, LimitPrice: 550}}
	}, "working")
	expectInvalid(t, "without its release", v, at(0, func(o *event.BoostRetiredOrder) { o.Released = 0 }), "released")
	expectInvalid(t, "with no side", v, at(0, func(o *event.BoostRetiredOrder) { o.Side = "" }), "released")
	expectInvalid(t, "with no limit", v, at(0, func(o *event.BoostRetiredOrder) { o.LimitPrice = 0 }), "released")
	expectInvalid(t, "with no time", v, at(0, func(o *event.BoostRetiredOrder) { o.RetiredAt = time.Time{} }), "when")
	expectInvalid(t, "the watched one still working", v, func(p *P) { p.Boost.Watched.Status, p.Boost.Watched.EndedAt = "", time.Time{} }, "canceled")
	expectInvalid(t, "in a dry-run", v, func(p *P) {
		p.Boost.UndecidedOrders, p.Boost.ClearedOrders, p.Boost.NoVenue = nil, nil, event.TradingModeDryRun
	}, "retire")
}

// TestBoostUndecidedBeforeRejectionValidate: a Boost decision counts how
// many of the undecided orders were released before the latest rejection it
// counts, never below none, never more than are listed, never the watched
// order, the latest released, and only beside a rejection counted (ADR
// 0026, point 2, "Boost", amended 2026-10-18).
func TestBoostUndecidedBeforeRejectionValidate(t *testing.T) {
	v := validTargetsDecided()
	v.Boost.VenueRejections = 1
	v.Boost.RejectedInARow = []event.BoostRejectedOrder{{ClientOrderID: "tw-0"}}
	v.Boost.Watched = event.BoostWatchedOrder{ClientOrderID: "tw-2", Side: event.OrderSideSell, Released: 12, LimitPrice: 550}
	v.Boost.UndecidedOrders = []event.BoostUndecidedOrder{
		{ClientOrderID: "tw-1", Side: event.OrderSideBuy, Released: 37, LimitPrice: 105},
		{ClientOrderID: "tw-2"},
	}
	v.Boost.UndecidedBeforeRejection = 1
	if err := v.Validate(); err != nil {
		t.Fatalf("an undecided order released before the rejection is valid: %v", err)
	}
	if (event.BoostFigures{UndecidedBeforeRejection: 1}).IsZero() {
		t.Error("figures counting an order older than the rejection are not empty")
	}
	type P = event.TargetsDecidedPayload
	expectInvalid(t, "negative", v, func(p *P) { p.Boost.UndecidedBeforeRejection = -1 }, "before")
	expectInvalid(t, "the watched order", v, func(p *P) { p.Boost.UndecidedBeforeRejection = 2 }, "before")
	expectInvalid(t, "more than listed", v, func(p *P) {
		p.Boost.Watched, p.Boost.UndecidedOrders = event.BoostWatchedOrder{}, p.Boost.UndecidedOrders[:1]
		p.Boost.UndecidedBeforeRejection = 2
	}, "before")
	expectInvalid(t, "with no rejection counted", v, func(p *P) { p.Boost.VenueRejections, p.Boost.RejectedInARow = 0, nil }, "before")
}

// TestBoostNoVenueValidate: a dry-run Boost decision records that no venue
// receives its orders, and so lists none undecided and counts no verdict;
// no other mode is recorded so (ADR 0026, point 3, "Mode").
func TestBoostNoVenueValidate(t *testing.T) {
	v := validTargetsDecided()
	v.Boost.NoVenue = event.TradingModeDryRun
	if err := v.Validate(); err != nil {
		t.Fatalf("a dry-run decision is valid: %v", err)
	}
	if (event.BoostFigures{NoVenue: event.TradingModeDryRun}).IsZero() {
		t.Error("figures recording a dry-run are not empty")
	}
	type P = event.TargetsDecidedPayload
	expectInvalid(t, "another mode", v, func(p *P) { p.Boost.NoVenue = event.TradingModePaper }, "dry-run")
	expectInvalid(t, "an undecided order", v, func(p *P) {
		p.Boost.UndecidedOrders = []event.BoostUndecidedOrder{{ClientOrderID: "tw-1", Side: event.OrderSideBuy, Released: 37, LimitPrice: 105}}
	}, "dry-run")
	expectInvalid(t, "a rejection counted", v, func(p *P) {
		p.Boost.VenueRejections, p.Boost.RejectedInARow = 1, []event.BoostRejectedOrder{{ClientOrderID: "tw-0"}}
	}, "dry-run")
	// Nothing is sent, so the owner has nothing to clear and no order is
	// accepted and working (ADR 0026, point 3, "Mode").
	expectInvalid(t, "a cleared order", v, func(p *P) {
		p.Boost.ClearedOrders = []event.BoostUndecidedOrder{{ClientOrderID: "tw-1", Side: event.OrderSideBuy, Released: 37, LimitPrice: 105}}
	}, "dry-run")
	expectInvalid(t, "a working order", v, func(p *P) {
		p.Boost.WorkingOrders = []event.BoostWatchedOrder{{ClientOrderID: "tw-1", Side: event.OrderSideBuy, Released: 37, LimitPrice: 105}}
	}, "dry-run")
	// The refusal names every list a dry-run leaves empty, a named order's
	// among them.
	expectInvalid(t, "a named order", v, func(p *P) { p.Boost.NamedOrders = []string{"tw-1"} }, "retired, named or working order")
}

// TestBoostWorkingOrdersValidate: an order the venue accepted that still
// works besides the watched one records its release, its executions and its
// venue order ID as the watched order does, and no end, being still
// working; it is named once and is neither the watched order, nor
// undecided, nor cleared (ADR 0026, point 5).
func TestBoostWorkingOrdersValidate(t *testing.T) {
	v := validTargetsDecided()
	v.Boost.Watched = event.BoostWatchedOrder{ClientOrderID: "tw-9", Side: event.OrderSideBuy, Released: 9, LimitPrice: 100}
	v.Boost.UndecidedOrders = []event.BoostUndecidedOrder{{ClientOrderID: "tw-9"}}
	v.Boost.ClearedOrders = []event.BoostUndecidedOrder{{ClientOrderID: "tw-5", Side: event.OrderSideSell, Released: 12, LimitPrice: 550}}
	v.Boost.WorkingOrders = []event.BoostWatchedOrder{
		{ClientOrderID: "tw-1", Side: event.OrderSideBuy, Released: 39, LimitPrice: 105, Filled: 20, VenueOrderID: "ib-1",
			Fills: []event.BoostWatchedFill{{FillID: "f1", Quantity: 20, Price: 104, FilledAt: twSession.Add(-2 * time.Hour)}}},
		{ClientOrderID: "tw-2", Side: event.OrderSideSell, Released: 12, LimitPrice: 550},
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("working orders beside the watched, undecided and cleared ones are valid: %v", err)
	}
	if (event.BoostFigures{WorkingOrders: v.Boost.WorkingOrders}).IsZero() {
		t.Error("figures recording a working order are not empty")
	}
	type P = event.TargetsDecidedPayload
	at := func(i int, f func(*event.BoostWatchedOrder)) func(*P) {
		return func(p *P) {
			p.Boost.WorkingOrders = slices.Clone(p.Boost.WorkingOrders)
			f(&p.Boost.WorkingOrders[i])
		}
	}
	expectInvalid(t, "named twice", v, at(1, func(o *event.BoostWatchedOrder) { o.ClientOrderID = "tw-1" }), "twice")
	expectInvalid(t, "the watched order", v, at(1, func(o *event.BoostWatchedOrder) { o.ClientOrderID = "tw-9" }), "watched")
	expectInvalid(t, "also undecided", v, func(p *P) {
		p.Boost.UndecidedOrders = []event.BoostUndecidedOrder{{ClientOrderID: "tw-2", Side: event.OrderSideSell, Released: 12, LimitPrice: 550}, {ClientOrderID: "tw-9"}}
	}, "undecided")
	expectInvalid(t, "also cleared", v, at(1, func(o *event.BoostWatchedOrder) { o.ClientOrderID = "tw-5" }), "cleared")
	expectInvalid(t, "also rejected", v, func(p *P) {
		p.Boost.VenueRejections, p.Boost.RejectedInARow = 1, []event.BoostRejectedOrder{{ClientOrderID: "tw-2"}}
	}, "rejected")
	expectInvalid(t, "ended", v, at(1, func(o *event.BoostWatchedOrder) {
		o.Status, o.EndedAt = event.OrderLifecycleStatusCanceled, twSession.Add(-time.Hour)
	}), "working")
	expectInvalid(t, "fully filled", v, at(0, func(o *event.BoostWatchedOrder) { o.Filled, o.Fills = 39, nil }), "filled")
	expectInvalid(t, "without its release", v, at(1, func(o *event.BoostWatchedOrder) { o.Released = 0 }), "released")
	expectInvalid(t, "naming nothing", v, func(p *P) {
		p.Boost.WorkingOrders = append(slices.Clone(p.Boost.WorkingOrders), event.BoostWatchedOrder{})
	}, "client order ID")
	early := event.BoostWatchedFill{FillID: "f1", Quantity: 1, Price: 550, FilledAt: twSession.Add(-2 * time.Hour)}
	expectInvalid(t, "a fill of another working order", v, at(1, func(o *event.BoostWatchedOrder) {
		o.Filled, o.Fills = 1, []event.BoostWatchedFill{early}
	}), "two orders")
	expectInvalid(t, "a fill of the watched order", v, func(p *P) {
		p.Boost.Watched.Filled, p.Boost.Watched.Fills, p.Boost.UndecidedOrders = 1, []event.BoostWatchedFill{early}, nil
	}, "two orders")
}

// TestBoostRejectionsWhileStoppedValidate: once two rejections in a row
// have stopped Boost's buys, each later rejection is recorded once, cause
// rejected, beside the count of them so far (ADR 0026, points 2, "Boost",
// amended 2026-10-18, and 4).
func TestBoostRejectionsWhileStoppedValidate(t *testing.T) {
	v := validTargetsDecided()
	v.Boost.VenueRejections = 2
	v.Boost.RejectedInARow = []event.BoostRejectedOrder{{ClientOrderID: "tw-0"}, {ClientOrderID: "tw-1"}}
	v.Boost.RejectionsWhileStopped = 1
	v.Boost.RejectedWhileStopped = []event.BoostReplan{{Cause: event.ReplanCauseRejected, ClientOrderID: "tw-2", Released: 12}}
	if err := v.Validate(); err != nil {
		t.Fatalf("a rejection after the stop is valid: %v", err)
	}
	if (event.BoostFigures{RejectionsWhileStopped: 1}).IsZero() || (event.BoostFigures{RejectedWhileStopped: v.Boost.RejectedWhileStopped}).IsZero() {
		t.Error("figures with a rejection after the stop are not empty")
	}
	type P = event.TargetsDecidedPayload
	expectInvalid(t, "before the stop", v, func(p *P) { p.Boost.VenueRejections = 1 }, "stopped")
	expectInvalid(t, "more recorded than counted", v, func(p *P) { p.Boost.RejectionsWhileStopped = 0 }, "stopped")
	expectInvalid(t, "negative count", v, func(p *P) { p.Boost.RejectionsWhileStopped = -1 }, "stopped")
	expectInvalid(t, "not a rejection", v, func(p *P) {
		p.Boost.RejectedWhileStopped = []event.BoostReplan{{Cause: event.ReplanCauseRefused}}
	}, "rejection")
	expectInvalid(t, "without its order", v, func(p *P) {
		p.Boost.RejectedWhileStopped = []event.BoostReplan{{Cause: event.ReplanCauseRejected, Released: 12}}
	}, "order")
}

// TestBoostVenueOrderIDsNameOneOrderEach: a venue order ID is the venue's
// ID for one order, so no two of the watched, undecided and working orders
// carry the same one: a restarted run matches IBKR's unechoed listing by
// it, and a shared ID would match the wrong order (ADR 0026, point 5,
// amended 2026-10-18; fail closed, AGENTS.md).
func TestBoostVenueOrderIDsNameOneOrderEach(t *testing.T) {
	v := validTargetsDecided()
	v.Boost.Watched = event.BoostWatchedOrder{ClientOrderID: "tw-9", Side: event.OrderSideBuy, Released: 9, LimitPrice: 100, VenueOrderID: "ib-9"}
	v.Boost.UndecidedOrders = []event.BoostUndecidedOrder{
		{ClientOrderID: "tw-3", Side: event.OrderSideBuy, Released: 3, LimitPrice: 100, VenueOrderID: "ib-3"},
		{ClientOrderID: "tw-9"},
	}
	v.Boost.WorkingOrders = []event.BoostWatchedOrder{
		{ClientOrderID: "tw-1", Side: event.OrderSideBuy, Released: 39, LimitPrice: 105, VenueOrderID: "ib-1"},
		{ClientOrderID: "tw-2", Side: event.OrderSideBuy, Released: 12, LimitPrice: 105, VenueOrderID: "ib-2"},
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("distinct venue order IDs are valid: %v", err)
	}
	type P = event.TargetsDecidedPayload
	expectInvalid(t, "two working orders", v, func(p *P) {
		p.Boost.WorkingOrders = slices.Clone(p.Boost.WorkingOrders)
		p.Boost.WorkingOrders[1].VenueOrderID = "ib-1"
	}, "venue order")
	expectInvalid(t, "a working and the watched order", v, func(p *P) {
		p.Boost.WorkingOrders = slices.Clone(p.Boost.WorkingOrders)
		p.Boost.WorkingOrders[0].VenueOrderID = "ib-9"
	}, "venue order")
	expectInvalid(t, "an undecided and a working order", v, func(p *P) {
		p.Boost.WorkingOrders = slices.Clone(p.Boost.WorkingOrders)
		p.Boost.WorkingOrders[0].VenueOrderID = "ib-3"
	}, "venue order")
}

// TestBoostRetiredSaleAndNamedOrdersValidate: a retired sale back to 98%
// records its tag and the attempt retired, the first being 1, both or
// neither; the orders a venue named are named once, strictly ascending,
// each the watched, an undecided or a working order, so a run restarted
// from the decision refuses their retirement as the full run does (ADR 0026,
// point 2, "Boost", and point 5, amended 2026-10-22).
func TestBoostRetiredSaleAndNamedOrdersValidate(t *testing.T) {
	v := validTargetsDecidedWatching()
	v.Boost.RetiredOrders[0].SaleTag, v.Boost.RetiredOrders[0].SaleAttempt = "engine-state:joint:1", 2
	v.Boost.NamedOrders = []string{"tw-0", "tw-1"}
	if err := v.Validate(); err != nil {
		t.Fatalf("a retired sale with its attempt and the watched and working orders named are valid: %v", err)
	}
	if (event.BoostFigures{NamedOrders: []string{"tw-0"}}).IsZero() {
		t.Error("figures naming an order are not empty")
	}
	type P = event.TargetsDecidedPayload
	expectInvalid(t, "a sale's tag with no attempt", v, func(p *P) {
		p.Boost.RetiredOrders = slices.Clone(p.Boost.RetiredOrders)
		p.Boost.RetiredOrders[0].SaleAttempt = 0
	}, "attempt")
	expectInvalid(t, "an attempt with no tag", v, func(p *P) {
		p.Boost.RetiredOrders = slices.Clone(p.Boost.RetiredOrders)
		p.Boost.RetiredOrders[0].SaleTag = ""
	}, "attempt")
	expectInvalid(t, "a negative attempt", v, func(p *P) {
		p.Boost.RetiredOrders = slices.Clone(p.Boost.RetiredOrders)
		p.Boost.RetiredOrders[0].SaleAttempt = -1
	}, "attempt")
	expectInvalid(t, "named out of order", v, func(p *P) { p.Boost.NamedOrders = []string{"tw-1", "tw-0"} }, "named")
	expectInvalid(t, "named twice", v, func(p *P) { p.Boost.NamedOrders = []string{"tw-0", "tw-0"} }, "named")
	expectInvalid(t, "an order not listed", v, func(p *P) { p.Boost.NamedOrders = []string{"tw-9"} }, "named")
	expectInvalid(t, "a retired order", v, func(p *P) { p.Boost.NamedOrders = []string{"tw-r"} }, "named")
}

// TestBoostRestoredSaleValidate: the sale attempt an approval restored names
// its incident and counts no fewer than no retired attempts, and figures
// recording one are not empty (ADR 0026, point 2, "Order identity", and
// point 5).
func TestBoostRestoredSaleValidate(t *testing.T) {
	v := validTargetsDecided()
	v.Boost.RestoredSale = &event.BoostRestoredSale{IncidentID: "engine-state:joint:1", RetiredAttempts: 1}
	if err := v.Validate(); err != nil {
		t.Fatalf("a restored sale is valid: %v", err)
	}
	if (event.BoostFigures{RestoredSale: &event.BoostRestoredSale{IncidentID: "engine-state:joint:1"}}).IsZero() {
		t.Error("figures recording a restored sale are empty")
	}
	type P = event.TargetsDecidedPayload
	expectInvalid(t, "no incident", v, func(p *P) { p.Boost.RestoredSale = &event.BoostRestoredSale{RetiredAttempts: 1} }, "restored sale")
	expectInvalid(t, "fewer than none retired", v, func(p *P) {
		p.Boost.RestoredSale = &event.BoostRestoredSale{IncidentID: "engine-state:joint:1", RetiredAttempts: -1}
	}, "restored sale")
}

// TestBoostRestoredSaleOrderValidate: a restored sale attempt that records
// the run's own live sale is attempted, and the order names its client order
// ID, side, shares, limit, a sale's trigger and its attempt, whether a venue
// has named it, its venue order ID and its executions so far, so a run
// restarted from the decision holds it as that run's own (ADR 0026, point 5:
// recovered, never recomputed).
func TestBoostRestoredSaleOrderValidate(t *testing.T) {
	order := func() *event.BoostRestoredSaleOrder {
		return &event.BoostRestoredSaleOrder{ClientOrderID: "tw-sale", Side: event.OrderSideSell, Released: 17, LimitPrice: 143.45,
			Trigger: event.SaleTriggerExit, Attempt: 1}
	}
	sale := func(o *event.BoostRestoredSaleOrder) *event.BoostRestoredSale {
		return &event.BoostRestoredSale{IncidentID: "engine-state:joint:1", Attempted: true, Order: o}
	}
	v := validTargetsDecided()
	v.Boost.RestoredSale = sale(order())
	if err := v.ValidateWritten(); err != nil {
		t.Fatalf("a restored sale with its own order is valid: %v", err)
	}
	named := v
	named.Boost.RestoredSale = sale(namedSaleOrder())
	if err := named.ValidateWritten(); err != nil {
		t.Fatalf("a restored sale a venue named and part filled is valid: %v", err)
	}
	type P = event.TargetsDecidedPayload
	expectInvalid(t, "not attempted", v, func(p *P) { p.Boost.RestoredSale = sale(order()); p.Boost.RestoredSale.Attempted = false }, "attempted")
	for name, bad := range map[string]func(*event.BoostRestoredSaleOrder){
		"no client order ID": func(o *event.BoostRestoredSaleOrder) { o.ClientOrderID = "" },
		"no shares":          func(o *event.BoostRestoredSaleOrder) { o.Released = 0 },
		"no limit":           func(o *event.BoostRestoredSaleOrder) { o.LimitPrice = 0 },
	} {
		expectInvalid(t, name, v, func(p *P) { o := order(); bad(o); p.Boost.RestoredSale = sale(o) }, "restored sale's order")
	}
	expectInvalid(t, "another trigger", v, func(p *P) { o := order(); o.Trigger = "manual"; p.Boost.RestoredSale = sale(o) }, "exit or margin-urgent")
	for name, c := range map[string]struct {
		bad  func(*event.BoostRestoredSaleOrder)
		want string
	}{
		"a buy":                       {func(o *event.BoostRestoredSaleOrder) { o.Side = event.OrderSideBuy }, "sell"},
		"a negative attempt":          {func(o *event.BoostRestoredSaleOrder) { o.Attempt = -1 }, "attempt"},
		"fills short of the filled":   {func(o *event.BoostRestoredSaleOrder) { o.Filled = 6 }, "fills sum"},
		"filled past its shares":      {func(o *event.BoostRestoredSaleOrder) { o.Filled, o.Fills[0].Quantity = 18, 18 }, "more shares than released"},
		"filled in full, no venue ID": {func(o *event.BoostRestoredSaleOrder) { o.Filled, o.Fills[0].Quantity, o.VenueOrderID = 17, 17, "" }, "venue order ID"},
		"ended, no venue ID": {func(o *event.BoostRestoredSaleOrder) {
			o.Status, o.EndedAt, o.VenueOrderID = event.OrderLifecycleStatusCanceled, twSession.Add(-time.Hour), ""
		}, "venue order ID"},
		"ended with no time": {func(o *event.BoostRestoredSaleOrder) { o.Status = event.OrderLifecycleStatusCanceled }, "ended_at"},
		"a time with no end": {func(o *event.BoostRestoredSaleOrder) { o.EndedAt = twSession.Add(-time.Hour) }, "ended_at"},
		"ended as submitted": {func(o *event.BoostRestoredSaleOrder) {
			o.Status, o.EndedAt = event.OrderLifecycleStatusSubmitted, twSession.Add(-time.Hour)
		}, "status"},
		"filled but not named":     {func(o *event.BoostRestoredSaleOrder) { o.Named, o.VenueOrderID = false, "" }, "named"},
		"a venue ID but not named": {func(o *event.BoostRestoredSaleOrder) { o.Named, o.Filled, o.Fills = false, 0, nil }, "named"},
	} {
		expectInvalid(t, name, v, func(p *P) { o := namedSaleOrder(); c.bad(o); p.Boost.RestoredSale = sale(o) }, c.want)
	}
	// The live sale is no other order the decision lists, and shares no
	// fill id or venue order ID with one (ADR 0019, "Explained means"; ADR
	// 0026, point 5, amended 2026-10-18).
	w := validTargetsDecidedWatching()
	w.Boost.RestoredSale = sale(namedSaleOrder())
	if err := w.ValidateWritten(); err != nil {
		t.Fatalf("a live sale beside a watched, a working and a retired order is valid: %v", err)
	}
	for name, c := range map[string]struct {
		bad  func(*event.BoostRestoredSaleOrder)
		want string
	}{
		"the watched order":  {func(o *event.BoostRestoredSaleOrder) { o.ClientOrderID = "tw-1" }, "restored sale"},
		"a working order":    {func(o *event.BoostRestoredSaleOrder) { o.ClientOrderID = "tw-0" }, "restored sale"},
		"a retired order":    {func(o *event.BoostRestoredSaleOrder) { o.ClientOrderID = "tw-r" }, "restored sale"},
		"another's fill id":  {func(o *event.BoostRestoredSaleOrder) { o.Fills[0].FillID = "f0" }, "two orders"},
		"another's venue ID": {func(o *event.BoostRestoredSaleOrder) { o.VenueOrderID = "ib-0" }, "venue order"},
	} {
		expectInvalid(t, name, w, func(p *P) {
			p.Boost.WorkingOrders = slices.Clone(p.Boost.WorkingOrders)
			p.Boost.WorkingOrders[0].VenueOrderID = "ib-0"
			o := namedSaleOrder()
			c.bad(o)
			p.Boost.RestoredSale = sale(o)
		}, c.want)
	}
}

// namedSaleOrder is the run's own live sale as a decision records it once a
// venue acknowledged it and executed 5 of its 17 shares.
func namedSaleOrder() *event.BoostRestoredSaleOrder {
	return &event.BoostRestoredSaleOrder{ClientOrderID: "tw-sale", Side: event.OrderSideSell, Released: 17, LimitPrice: 143.45,
		Trigger: event.SaleTriggerMarginUrgent, Attempt: 2, Named: true, VenueOrderID: "ib-sale", Filled: 5,
		Fills: []event.BoostWatchedFill{{FillID: "f-sale", Quantity: 5, Price: 143.5, Commission: 1, FilledAt: twSession.Add(-2 * time.Hour)}}}
}

// TestBoostRestoredSaleOrderAtVersion7: decision schema version 7 recorded
// the live sale's client order ID, shares, limit and trigger only, for a
// sale no venue had named and nothing had filled. A version 7 record is read
// as it was written, and refused if it carries a field of version 8; a
// record written now states the sale's side and attempt (ADR 0015).
func TestBoostRestoredSaleOrderAtVersion7(t *testing.T) {
	if got, want := event.TargetsDecidedVersions(), []uint32{event.TargetsDecidedRetiredVersion, event.TargetsDecidedLiveSaleVersion, event.TargetsDecidedSchemaVersion}; !slices.Equal(got, want) {
		t.Fatalf("decision versions read are %v, want %v", got, want)
	}
	if event.TargetsDecidedRetiredVersion != 7 || event.TargetsDecidedLiveSaleVersion != 8 || event.TargetsDecidedSchemaVersion != 9 {
		t.Fatalf("decision versions are %d, %d and %d, want 7, 8 and 9", event.TargetsDecidedRetiredVersion, event.TargetsDecidedLiveSaleVersion, event.TargetsDecidedSchemaVersion)
	}
	old := validTargetsDecided()
	old.Boost.RestoredSale = &event.BoostRestoredSale{IncidentID: "engine-state:joint:1", Attempted: true,
		Order: &event.BoostRestoredSaleOrder{ClientOrderID: "tw-sale", Released: 17, LimitPrice: 143.45, Trigger: event.SaleTriggerExit}}
	if err := old.ValidateSchema(event.TargetsDecidedRetiredVersion); err != nil {
		t.Errorf("a version 7 live sale is read: %v", err)
	}
	if err := old.Validate(); err != nil {
		t.Errorf("a version 7 live sale validates: %v", err)
	}
	if err := old.ValidateWritten(); err == nil || !strings.Contains(err.Error(), "side") {
		t.Errorf("a live sale written now with no side or attempt is refused; got %v", err)
	}
	named := validTargetsDecided()
	named.Boost.RestoredSale = &event.BoostRestoredSale{IncidentID: "engine-state:joint:1", Attempted: true, Order: namedSaleOrder()}
	if err := named.ValidateSchema(event.TargetsDecidedLiveSaleVersion); err != nil {
		t.Errorf("a version 8 live sale is read: %v", err)
	}
	for name, o := range map[string]event.BoostRestoredSaleOrder{
		"its side":    {ClientOrderID: "tw-sale", Side: event.OrderSideSell, Released: 17, LimitPrice: 143.45, Trigger: event.SaleTriggerExit},
		"its attempt": {ClientOrderID: "tw-sale", Released: 17, LimitPrice: 143.45, Trigger: event.SaleTriggerExit, Attempt: 1},
		"named":       {ClientOrderID: "tw-sale", Released: 17, LimitPrice: 143.45, Trigger: event.SaleTriggerExit, Named: true},
		"a venue ID":  {ClientOrderID: "tw-sale", Released: 17, LimitPrice: 143.45, Trigger: event.SaleTriggerExit, Named: true, VenueOrderID: "ib-sale"},
		"its fills":   *namedSaleOrder(),
	} {
		p := validTargetsDecided()
		p.Boost.RestoredSale = &event.BoostRestoredSale{IncidentID: "engine-state:joint:1", Attempted: true, Order: &o}
		if err := p.ValidateSchema(event.TargetsDecidedRetiredVersion); err == nil || !strings.Contains(err.Error(), "version 8") {
			t.Errorf("a version 7 live sale recording %s is refused as a version 8 field; got %v", name, err)
		}
	}
	for _, version := range []uint32{0, event.TargetsDecidedSchemaVersion + 1} {
		if err := old.ValidateSchema(version); err == nil {
			t.Errorf("decision schema version %d is refused", version)
		}
	}
	// Version 8 records the live sale's side and attempt, so a version 8
	// record read without either is refused, as one written now is.
	for name, drop := range map[string]func(*event.BoostRestoredSaleOrder){
		"no side":    func(o *event.BoostRestoredSaleOrder) { o.Side = "" },
		"no attempt": func(o *event.BoostRestoredSaleOrder) { o.Attempt = 0 },
	} {
		p := validTargetsDecided()
		o := namedSaleOrder()
		drop(o)
		p.Boost.RestoredSale = &event.BoostRestoredSale{IncidentID: "engine-state:joint:1", Attempted: true, Order: o}
		for _, version := range []uint32{event.TargetsDecidedLiveSaleVersion, event.TargetsDecidedSchemaVersion} {
			if err := p.ValidateSchema(version); err == nil || !strings.Contains(err.Error(), "side") {
				t.Errorf("a version %d live sale with %s is read: %v, want it refused", version, name, err)
			}
		}
	}
}

// endedSaleOrders are the run's own sale back to 98% once it is no longer
// live, as version 9 records it: canceled after a part fill, rejected, and
// filled in full, each under its venue order ID.
func endedSaleOrders() map[string]*event.BoostRestoredSaleOrder {
	canceled, invalid, filled := namedSaleOrder(), namedSaleOrder(), namedSaleOrder()
	canceled.Status, canceled.EndedAt = event.OrderLifecycleStatusCanceled, twSession.Add(-time.Hour)
	invalid.Status, invalid.EndedAt = event.OrderLifecycleStatusInvalid, twSession.Add(-time.Hour)
	filled.Filled, filled.Fills[0].Quantity = 17, 17
	return map[string]*event.BoostRestoredSaleOrder{"canceled": canceled, "invalid": invalid, "filled in full": filled}
}

// TestBoostRestoredSaleOrderAtVersion9: decision schema version 9 records
// the run's own sale once it has ended or filled in full, with its end and
// executions, under the venue order ID a venue gave it, so a restart reads
// a late fill after its cancel as the full run does (ADR 0022; ADR 0026,
// point 5). Version 8 recorded the sale only while it was live, so a
// version 8 record of an ended sale is refused, and a live one is still
// read (ADR 0015).
func TestBoostRestoredSaleOrderAtVersion9(t *testing.T) {
	for name, o := range endedSaleOrders() {
		p := validTargetsDecided()
		p.Boost.RestoredSale = &event.BoostRestoredSale{IncidentID: "engine-state:joint:1", Attempted: true, Order: o}
		if err := p.ValidateWritten(); err != nil {
			t.Errorf("a sale %s is written at version 9: %v", name, err)
		}
		if err := p.ValidateSchema(event.TargetsDecidedSchemaVersion); err != nil {
			t.Errorf("a version 9 sale %s is read: %v", name, err)
		}
		if err := p.ValidateSchema(event.TargetsDecidedLiveSaleVersion); err == nil || !strings.Contains(err.Error(), "version 9") {
			t.Errorf("a version 8 record of a sale %s is refused as a version 9 field; got %v", name, err)
		}
		if err := p.ValidateSchema(event.TargetsDecidedRetiredVersion); err == nil || !strings.Contains(err.Error(), "version 8") {
			t.Errorf("a version 7 record of a sale %s is refused; got %v", name, err)
		}
		if o.Live() {
			t.Errorf("a sale %s is reported live", name)
		}
	}
	if !namedSaleOrder().Live() {
		t.Error("a part-filled sale with no end is reported not live")
	}
	live := validTargetsDecided()
	live.Boost.RestoredSale = &event.BoostRestoredSale{IncidentID: "engine-state:joint:1", Attempted: true, Order: namedSaleOrder()}
	if err := live.ValidateSchema(event.TargetsDecidedSchemaVersion); err != nil {
		t.Errorf("a version 9 live sale is read: %v", err)
	}
	a, b := endedSaleOrders()["canceled"], endedSaleOrders()["canceled"]
	b.EndedAt = b.EndedAt.Add(time.Second)
	if a.Equal(*b) {
		t.Error("two records of a sale ended at different times are equal")
	}
	b.EndedAt, b.Status = a.EndedAt, event.OrderLifecycleStatusInvalid
	if a.Equal(*b) {
		t.Error("two records of a sale ended differently are equal")
	}
}

// TestBoostRestoredSaleEqual: two records of a sale attempt are equal when
// they state the same incident, attempt, retired attempts and own order.
func TestBoostRestoredSaleEqual(t *testing.T) {
	a := &event.BoostRestoredSale{IncidentID: "engine-state:joint:1", Attempted: true,
		Order: &event.BoostRestoredSaleOrder{ClientOrderID: "tw-sale", Side: event.OrderSideSell, Released: 17, LimitPrice: 143.45, Trigger: event.SaleTriggerExit, Attempt: 1}}
	same := *a
	order := *a.Order
	same.Order = &order
	if !a.Equal(&same) {
		t.Error("the same record is not equal")
	}
	b, c := &event.BoostRestoredSale{IncidentID: "engine-state:joint:1", Attempted: true, Order: namedSaleOrder()},
		&event.BoostRestoredSale{IncidentID: "engine-state:joint:1", Attempted: true, Order: namedSaleOrder()}
	c.Order.Fills[0].FilledAt = c.Order.Fills[0].FilledAt.In(time.FixedZone("EDT", -4*3600))
	if !b.Equal(c) {
		t.Error("the same executions written in another zone are not equal")
	}
	var none *event.BoostRestoredSale
	if !none.Equal(nil) || none.Equal(a) || a.Equal(nil) {
		t.Error("an absent record equals only an absent one")
	}
	for name, change := range map[string]func(*event.BoostRestoredSale){
		"another incident": func(r *event.BoostRestoredSale) { r.IncidentID = "engine-state:joint:2" },
		"not attempted":    func(r *event.BoostRestoredSale) { r.Attempted = false },
		"one retired":      func(r *event.BoostRestoredSale) { r.RetiredAttempts = 1 },
		"no order":         func(r *event.BoostRestoredSale) { r.Order = nil },
		"another order":    func(r *event.BoostRestoredSale) { o := *r.Order; o.Released = 16; r.Order = &o },
		"named":            func(r *event.BoostRestoredSale) { o := *r.Order; o.Named = true; r.Order = &o },
		"another attempt":  func(r *event.BoostRestoredSale) { o := *r.Order; o.Attempt = 2; r.Order = &o },
		"a venue ID":       func(r *event.BoostRestoredSale) { o := *r.Order; o.VenueOrderID = "ib-sale"; r.Order = &o },
		"part filled": func(r *event.BoostRestoredSale) {
			o := *r.Order
			o.Filled, o.Fills = 5, []event.BoostWatchedFill{{FillID: "f-sale", Quantity: 5, Price: 143.5, FilledAt: twSession}}
			r.Order = &o
		},
	} {
		other := *a
		change(&other)
		if a.Equal(&other) {
			t.Errorf("a record with %s is equal", name)
		}
	}
}
