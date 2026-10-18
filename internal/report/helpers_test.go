package report_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// day returns the 21:00 UTC session close n sessions after 2024-01-02.
func day(n int) time.Time {
	return time.Date(2024, time.January, 2+n, 21, 0, 0, 0, time.UTC)
}

// stream builds a synthetic envelope stream in recording order.
type stream struct {
	t   *testing.T
	seq uint64
	out []event.Envelope
}

func newStream(t *testing.T) *stream {
	s := &stream{t: t}
	s.add(event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", day(0).Add(-time.Hour),
		event.TargetWeightConfigurationPayload{StrategyID: event.TargetWeightStrategyBoost, AccountID: "joint", Mode: event.TradingModeDryRun, Adapter: event.AdapterIBKR, Boost: event.BoostConfiguration{InstrumentID: "SPY", InvestedFraction: 0.98, BoostSize: 1.5, RSIPeriod: 2, RSIEntry: 10, TrendDays: 200, ExitSMA: 5, WarmUpStart: "2024-01-02"},
			Limits:    event.MarginLimits{MaxExposure: 1.5, MaxOrdersPerDecision: 1, MaxOrderFraction: 1, PriceBand: 0.05, ExcessLiquidityWarning: 0.4, ExcessLiquidityUrgent: 0.2, ExcessLiquidityStaleMinutes: 30},
			Execution: event.ExecutionConfiguration{MaxSnapshotAge: 30}})
	return s
}

func (s *stream) add(eventType string, version uint32, id string, at time.Time, payload any) {
	s.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		s.t.Fatal(err)
	}
	s.seq++
	s.out = append(s.out, event.Envelope{
		ID: id, Type: eventType, EnvelopeVersion: event.CurrentEnvelopeVersion, SchemaVersion: version,
		EventTime: at, RecordedAt: at, Sequence: s.seq, Source: "test", StrategyVersion: "v", ConfigurationHash: "h",
		PayloadHash: event.HashPayload(raw), Payload: raw,
	})
}

func (s *stream) bar(n int, instrument string, closePrice float64) {
	view := func(label string) event.PriceView {
		return event.PriceView{View: label, Open: closePrice, High: closePrice, Low: closePrice, Close: closePrice, Volume: 1}
	}
	s.add(event.CompletedBarEventType, event.CompletedBarSchemaVersion, fmt.Sprintf("bar:%d", n), day(n),
		event.CompletedBarPayload{InstrumentID: instrument, PeriodEnd: day(n), SplitAdjusted: view(event.ViewSplitAdjusted), Raw: view(event.ViewRaw)})
}

func (s *stream) snapshot(n int, netLiquidation float64, held int64) {
	p := event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: day(n), Currency: "USD", NetLiquidation: netLiquidation}
	if held != 0 {
		p.Positions = []event.AccountPosition{{InstrumentID: "SPY", Quantity: held}}
	}
	s.add(event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, fmt.Sprintf("snapshot:%d", n), day(n), p)
}

func (s *stream) decided(n int, weight float64) {
	s.add(event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, fmt.Sprintf("targets:%d", n), day(n),
		event.TargetsDecidedPayload{AccountID: "joint", StrategyID: event.TargetWeightStrategyBoost, DecisionTime: day(n), SessionPeriodEnd: day(n),
			Outcome: event.TargetsOutcomeDecided, Targets: []event.TargetWeight{{InstrumentID: "SPY", Weight: weight}},
			Boost: event.BoostFigures{InstrumentID: "SPY", Close: 100}})
}

func (s *stream) withheld(n int, reason string) {
	s.add(event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, fmt.Sprintf("targets:%d", n), day(n),
		event.TargetsDecidedPayload{AccountID: "joint", StrategyID: event.TargetWeightStrategyBoost, DecisionTime: day(n), SessionPeriodEnd: day(n),
			Outcome: event.TargetsOutcomeWithheld, WithheldReason: reason, Detail: "detail for " + reason,
			Boost: event.BoostFigures{InstrumentID: "SPY"}})
}

func (s *stream) planned(n int, clientID string, quantity int64) {
	s.add(event.OrderPlannedEventType, event.OrderPlannedSchemaVersion, "plan:"+clientID, day(n),
		event.OrderPlannedPayload{AccountID: "joint", Tag: fmt.Sprintf("targets:%d", n), ClientOrderID: clientID, InstrumentID: "SPY",
			Side: event.OrderSideBuy, Phase: event.OrderSideBuy, OrderType: event.OrderTypeLimit, Quantity: quantity, LimitPrice: 101,
			TargetWeight: 0.98, NetLiquidation: 4000, ReferencePrice: 100, PriceBand: 0.05})
}

// figures are risk figures that validate, as the gate records them.
var figures = event.RiskFigures{NetLiquidation: 4000, ReferencePrice: 100, ExposureBefore: 0.5, ExposureAfter: 0.98, OrderFraction: 0.48, OrdersInDecision: 1}

func (s *stream) released(n int, clientID string, quantity int64) {
	s.add(event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, "release:"+clientID, day(n),
		event.OrderReleasedPayload{AccountID: "joint", PlanID: "plan:" + clientID, Tag: fmt.Sprintf("targets:%d", n), ClientOrderID: clientID,
			InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: quantity, LimitPrice: 101, Mode: event.TradingModeDryRun, Figures: figures})
}

func (s *stream) refused(n int, clientID, reason string) {
	s.add(event.OrderRefusedEventType, event.OrderRefusedSchemaVersion, "refuse:"+clientID, day(n),
		event.OrderRefusedPayload{AccountID: "joint", PlanID: "plan:" + clientID, Tag: fmt.Sprintf("targets:%d", n), ClientOrderID: clientID,
			InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 39, LimitPrice: 101, Reason: reason, Detail: "figures", Figures: figures})
}

// flaggedRefusal is a kill-switch or stale-broker-snapshot refusal, which
// the gate journals at the safety version, carrying alert as the gate set it.
func (s *stream) flaggedRefusal(n int, clientID, reason string, alert bool) {
	s.add(event.OrderRefusedEventType, event.OrderRefusedSafetyVersion, "refuse:"+clientID, day(n),
		event.OrderRefusedPayload{AccountID: "joint", PlanID: "plan:" + clientID, Tag: fmt.Sprintf("targets:%d", n), ClientOrderID: clientID,
			InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 39, LimitPrice: 101, Reason: reason, Detail: "figures", Figures: figures, Alert: alert})
}

// staleSaleWithheld is a sale back to 98% the gate withheld because the
// latest broker snapshot was too old, carrying alert as the gate set it.
func (s *stream) staleSaleWithheld(n int, clientID string, alert bool) {
	s.add(event.SaleWithheldEventType, event.SaleWithheldSchemaVersion, "sale-withheld:"+clientID, day(n),
		event.SaleWithheldPayload{AccountID: "joint", Tag: fmt.Sprintf("targets:%d", n), ClientOrderID: clientID, InstrumentID: "SPY",
			Kind: event.SaleKindIncident, Trigger: event.SaleTriggerExit, Reason: event.SaleWithheldStaleBrokerSnapshot,
			Detail: "snapshot too old", SnapshotID: "broker:1", Alert: alert})
}

func (s *stream) engineState(n int, state, reason string, scope ...string) {
	p := event.EngineStatePayload{State: state, Reason: reason, Detail: "figures"}
	if state == event.EngineStateDegraded {
		p.ScopeInstrumentIDs = scope
	}
	s.add(event.EngineStateEventType, event.EngineStateScopedVersion, fmt.Sprintf("state:%d", n), day(n), p)
}
