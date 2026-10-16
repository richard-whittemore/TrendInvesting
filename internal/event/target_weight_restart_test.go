package event_test

import (
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func validRunOptions() event.RunOptionsPayload {
	return event.RunOptionsPayload{DecideFrom: twSession, RecoveredRecords: 12}
}

func validRestartRecovered() event.RestartRecoveredPayload {
	return event.RestartRecoveredPayload{
		AccountID: "ira-traditional", StrategyID: event.TargetWeightStrategyMomentum, RecoveredRecords: 12,
		LastDecisionID: "targets-decided:ira:t", QuietUntil: twSession, BookDecisionID: "targets-decided:ira:t",
		Book: []event.TargetWeight{
			{InstrumentID: "AAA", Weight: 0.49, Shares: []event.SleeveShare{{Sleeve: event.SleeveMomentum, Weight: 0.49}}},
			{InstrumentID: "IEF", Weight: 0.49, Shares: []event.SleeveShare{{Sleeve: event.SleeveMomentum, Weight: 0.49}}},
		},
		Glide:       event.GlideFigures{RebalanceSession: "2025-03-03", AgeMonths: 400, BondShare: 0},
		EngineState: event.EngineStateDegraded, Scope: []string{"AAA"},
		Rebalance: event.RecoveredRebalance{DecisionID: "targets-decided:ira:t", Phase: event.RebalanceSellsWorking, Held: true, Working: []string{"tw-1", "tw-2"}},
	}
}

func validRebalanceCleared() event.RebalanceClearedPayload {
	return event.RebalanceClearedPayload{AccountID: "ira-traditional", DecisionID: "targets-decided:ira:t", Action: event.RebalanceResume, ClearedAt: twSession}
}

func TestRestartPayloadsValidate(t *testing.T) {
	for _, p := range []validator{validRunOptions(), validRestartRecovered(), validRebalanceCleared()} {
		if err := p.Validate(); err != nil {
			t.Errorf("%T: %v", p, err)
		}
	}
	fresh := event.RunOptionsPayload{DecideFrom: twSession}
	if err := fresh.Validate(); err != nil {
		t.Errorf("a fresh run's options: %v", err)
	}
	boost := event.RestartRecoveredPayload{AccountID: "joint", StrategyID: event.TargetWeightStrategyBoost, RecoveredRecords: 3, BoostLatched: true, EngineState: event.EngineStateNormal}
	if err := boost.Validate(); err != nil {
		t.Errorf("a Boost restart: %v", err)
	}

	type O = event.RunOptionsPayload
	expectInvalid(t, "no first session", validRunOptions(), func(p *O) { p.DecideFrom = time.Time{} }, "decide from")
	expectInvalid(t, "negative prelude", validRunOptions(), func(p *O) { p.RecoveredRecords = -1 }, "recovered records")

	type R = event.RestartRecoveredPayload
	v := validRestartRecovered()
	expectInvalid(t, "account", v, func(p *R) { p.AccountID = "" }, "account id")
	expectInvalid(t, "strategy", v, func(p *R) { p.StrategyID = "turtle" }, "strategy id")
	expectInvalid(t, "no records", v, func(p *R) { p.RecoveredRecords = 0 }, "recovered records")
	expectInvalid(t, "last decision without session", v, func(p *R) { p.QuietUntil = time.Time{} }, "quiet until")
	expectInvalid(t, "last decision alone", v, func(p *R) { p.LastDecisionID = "" }, "come together")
	expectInvalid(t, "negative weight", v, func(p *R) { p.Book[0].Weight = -1 }, "weight")
	expectInvalid(t, "book out of order", v, func(p *R) { p.Book[0], p.Book[1] = p.Book[1], p.Book[0] }, "book instrument id")
	expectInvalid(t, "book without decision", v, func(p *R) { p.BookDecisionID = "" }, "names the decision")
	expectInvalid(t, "momentum latch", v, func(p *R) { p.BoostLatched = true }, "latch")
	expectInvalid(t, "engine state", v, func(p *R) { p.EngineState = "paused" }, "engine state")
	expectInvalid(t, "degraded without scope", v, func(p *R) { p.Scope = nil }, "names its scope")
	expectInvalid(t, "normal with scope", v, func(p *R) { p.EngineState = event.EngineStateNormal }, "names no scope")
	expectInvalid(t, "rebalance without decision", v, func(p *R) { p.Rebalance.DecisionID = "" }, "rebalance decision id")
	expectInvalid(t, "phase", v, func(p *R) { p.Rebalance.Phase = "selling" }, "phase")
	expectInvalid(t, "held complete", v, func(p *R) { p.Rebalance.Phase, p.Rebalance.Working = event.RebalanceComplete, nil }, "not held")
	expectInvalid(t, "working without orders", v, func(p *R) { p.Rebalance.Working = nil }, "names its working orders")
	expectInvalid(t, "pending with orders", v, func(p *R) { p.Rebalance.Phase = event.RebalanceBuysPending }, "no working order")
	expectInvalid(t, "working out of order", v, func(p *R) { p.Rebalance.Working = []string{"tw-2", "tw-1"} }, "working order")

	type C = event.RebalanceClearedPayload
	c := validRebalanceCleared()
	expectInvalid(t, "clear account", c, func(p *C) { p.AccountID = "" }, "account id")
	expectInvalid(t, "clear decision", c, func(p *C) { p.DecisionID = "" }, "decision id")
	expectInvalid(t, "clear action", c, func(p *C) { p.Action = "retry" }, "action")
	expectInvalid(t, "clear time", c, func(p *C) { p.ClearedAt = time.Time{} }, "cleared at")
}
