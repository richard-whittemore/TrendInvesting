package event_test

import (
	"encoding/json"
	"math"
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
		},
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
	const want = "sha256:6d0fe0ab1983edd2e2737c8ce6da5eb7116865b3b16361bf3b8ff3ed3e5d5496"
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
		event.WithheldInsufficientTrendHistory, event.WithheldNoEligibleNames,
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
