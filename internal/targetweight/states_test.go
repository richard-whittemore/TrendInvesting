package targetweight_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// recordedState is an earlier run's strategy.engine.state record, as a
// restart reads it back from the journal.
func recordedState(t *testing.T, id string, payload event.EngineStatePayload, version uint32) event.Envelope {
	t.Helper()
	s := &stream{hash: event.TargetWeightConfigurationHash(testConfiguration())}
	e := s.envelope(t, event.EngineStateEventType, version, id, sessionEnd(17), payload)
	e.Source = "target-weight"
	return e
}

func degradedOnSPY(t *testing.T) event.Envelope {
	return recordedState(t, "engine-state:joint:recorded", event.EngineStatePayload{
		State: event.EngineStateDegraded, Reason: event.EngineStateReasonRiskLimitBreached,
		Detail: "recorded: SPY quantity in dispute", ScopeInstrumentIDs: []string{"SPY"},
	}, event.EngineStateScopedVersion)
}

func haltedState(t *testing.T) event.Envelope {
	return recordedState(t, "engine-state:joint:recorded", event.EngineStatePayload{
		State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "recorded: untagged order",
	}, event.EngineStateSchemaVersion)
}

// restartAt17 restarts the full run from its boosted decision of session 17,
// feeding the full run's account and broker snapshots, with prelude records.
func restartAt17(t *testing.T, full run, opts options) run {
	t.Helper()
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[17]
	opts.recovery, opts.snapshots, opts.brokers = &recovery, full.inputs, true
	return simulate(t, opts)
}

func released(t *testing.T, es []event.Envelope) []event.OrderReleasedPayload {
	t.Helper()
	var out []event.OrderReleasedPayload
	for _, e := range ofType(es, event.OrderReleasedEventType) {
		var p event.OrderReleasedPayload
		decode(t, e, &p)
		out = append(out, p)
	}
	return out
}

func sales(t *testing.T, es []event.Envelope) []event.OrderPlannedPayload {
	t.Helper()
	var out []event.OrderPlannedPayload
	for _, o := range planned(t, es) {
		if o.Sale != (event.SaleFigures{}) {
			out = append(out, o)
		}
	}
	return out
}

func TestDegradedOnSPYWhileBoostedSellsBackTo98(t *testing.T) {
	full := simulate(t, options{brokers: true})
	state := degradedOnSPY(t)
	r := restartAt17(t, full, options{prelude: []event.Envelope{state}})

	refused := ofType(r.decisions, event.OrderRefusedEventType)
	if len(refused) != 1 {
		t.Fatalf("want Boost's exit refused while SPY is frozen, got %d refusals", len(refused))
	}
	var exit event.OrderRefusedPayload
	decode(t, refused[0], &exit)
	if exit.Reason != event.RefusedEngineDegraded || exit.Side != event.OrderSideSell {
		t.Errorf("exit: %+v", exit)
	}
	sold := sales(t, r.decisions)
	if len(sold) != 1 {
		t.Fatalf("one sale back to 98%%, got %+v", sold)
	}
	sale := sold[0]
	if sale.Tag != state.ID || sale.Sale.Kind != event.SaleKindIncident || sale.Sale.Trigger != event.SaleTriggerExit {
		t.Errorf("sale identity: %+v", sale)
	}
	// Sized from the broker snapshot of the exit session, at its own mark.
	var b event.BrokerSnapshotPayload
	for _, in := range full.inputs {
		if in.ID == "broker:20" {
			decode(t, in, &b)
		}
	}
	keep := int64(event.KeepShares(0.98, b.NetLiquidation, b.Positions[0].Mark))
	if sale.Quantity != b.Positions[0].Quantity-keep || sale.ReferencePrice != b.Positions[0].Mark || sale.NetLiquidation != b.NetLiquidation {
		t.Errorf("sale %+v from broker %+v (keep %d)", sale, b, keep)
	}
	if rel := released(t, r.decisions); len(rel) != 1 || rel[0].ClientOrderID != sale.ClientOrderID || !rel[0].Figures.RiskReducing {
		t.Errorf("the sale is released: %+v", rel)
	}
	if states := ofType(r.decisions, event.EngineStateEventType); len(states) != 0 {
		t.Errorf("the account stays Degraded; no new state: %d", len(states))
	}
}

func TestHaltedSellsOnceAcrossARestart(t *testing.T) {
	full := simulate(t, options{brokers: true})
	state := haltedState(t)
	first := restartAt17(t, full, options{prelude: []event.Envelope{state}})
	sold := sales(t, first.decisions)
	if len(sold) != 1 || sold[0].Tag != state.ID {
		t.Fatalf("Halted: one sale tagged with the incident, got %+v", sold)
	}
	if n := len(released(t, first.decisions)); n != 1 {
		t.Fatalf("only the sale is released while Halted, got %d", n)
	}

	// Restart again, after the sale, reading back the state and the sale's
	// records. A later urgent alarm in the same incident sells nothing.
	var prelude []event.Envelope
	prelude = append(prelude, state)
	for _, e := range first.decisions {
		switch e.Type {
		case event.OrderPlannedEventType, event.OrderReleasedEventType, event.OrderRefusedEventType:
			prelude = append(prelude, e)
		}
	}
	urgent := map[int]func(*event.BrokerSnapshotPayload){22: func(b *event.BrokerSnapshotPayload) { b.ExcessLiquidity = 0.1 * b.NetLiquidation }}
	second := restartAt17(t, full, options{prelude: prelude, brokerMutate: urgent})
	alarms := ofType(second.decisions, event.MarginAlarmEventType)
	if len(alarms) == 0 {
		t.Fatal("the urgent alarm is journalled")
	}
	if got := sales(t, second.decisions); len(got) != 0 {
		t.Fatalf("attempted once per incident, across a restart: %+v", got)
	}
	if got := ofType(second.decisions, event.SaleWithheldEventType); len(got) != 0 {
		t.Fatalf("nothing withheld either: the attempt is spent: %d", len(got))
	}

	// Without the sale's records, the same restart would sell: the records
	// are what stop it.
	control := restartAt17(t, full, options{prelude: []event.Envelope{state}, brokerMutate: urgent})
	if got := sales(t, control.decisions); len(got) != 1 {
		t.Fatalf("control: %+v", got)
	}
}

func TestAWithheldSaleIsRecoveredAsAnAttempt(t *testing.T) {
	full := simulate(t, options{brokers: true})
	state := haltedState(t)
	// The earlier run withheld the incident's sale: IBKR's open orders were
	// unreadable, and the alert asked the owner to act by hand.
	unreadable := map[int]func(*event.BrokerSnapshotPayload){20: func(b *event.BrokerSnapshotPayload) {
		b.Unreadable = []string{event.BrokerComponentOpenOrders}
	}}
	first := restartAt17(t, full, options{prelude: []event.Envelope{state}, brokerMutate: unreadable})
	withheld := ofType(first.decisions, event.SaleWithheldEventType)
	if len(withheld) != 1 || len(sales(t, first.decisions)) != 0 {
		t.Fatalf("withheld %d, sales %d", len(withheld), len(sales(t, first.decisions)))
	}
	urgent := map[int]func(*event.BrokerSnapshotPayload){22: func(b *event.BrokerSnapshotPayload) { b.ExcessLiquidity = 0.1 * b.NetLiquidation }}
	second := restartAt17(t, full, options{prelude: []event.Envelope{state, withheld[0]}, brokerMutate: urgent})
	if got := sales(t, second.decisions); len(got) != 0 {
		t.Fatalf("a sale the owner was asked to make by hand is never attempted again: %+v", got)
	}
}

func TestKillSwitchStopsRoutineOrdersButNotTheSaleBackTo98(t *testing.T) {
	r := simulate(t, options{brokers: true, kill: map[int]bool{19: true}})
	var reasons []string
	for _, e := range ofType(r.decisions, event.OrderRefusedEventType) {
		var p event.OrderRefusedPayload
		decode(t, e, &p)
		reasons = append(reasons, p.Reason)
	}
	if len(reasons) != 1 || reasons[0] != event.RefusedKillSwitch {
		t.Fatalf("Boost's exit refused by the kill switch: %v", reasons)
	}
	sold := sales(t, r.decisions)
	if len(sold) != 1 || sold[0].Tag != "kill:19" {
		t.Fatalf("the sale back to 98%% goes through the switch: %+v", sold)
	}
	if states := ofType(r.decisions, event.EngineStateEventType); len(states) != 0 {
		t.Errorf("the kill switch changes no state: %d", len(states))
	}
	// Engaged and released before the entry, it leaves every order as it was.
	r = simulate(t, options{brokers: true, kill: map[int]bool{15: true, 16: false}})
	if got := released(t, r.decisions); len(got) != 3 {
		t.Fatalf("first purchase, entry and exit, after the switch is released: %d", len(got))
	}
}

func TestUrgentAlarmSellsBackTo98InNormal(t *testing.T) {
	urgent := map[int]func(*event.BrokerSnapshotPayload){18: func(b *event.BrokerSnapshotPayload) { b.ExcessLiquidity = 0.15 * b.NetLiquidation }}
	r := simulate(t, options{brokers: true, brokerMutate: urgent})
	alarms := ofType(r.decisions, event.MarginAlarmEventType)
	if len(alarms) != 2 {
		t.Fatalf("urgent, then clear: %d alarms", len(alarms))
	}
	var alarm event.MarginAlarmPayload
	decode(t, alarms[0], &alarm)
	if alarm.Level != event.MarginAlarmUrgent || alarms[0].ID != "margin-alarm:joint:broker:18" {
		t.Errorf("alarm %s: %+v", alarms[0].ID, alarm)
	}
	sold := sales(t, r.decisions)
	if len(sold) != 1 || sold[0].Sale.Kind != event.SaleKindUrgent || sold[0].Tag != alarms[0].ID {
		t.Fatalf("urgent sale: %+v", sold)
	}
	if !strings.HasPrefix(sold[0].ClientOrderID, "tw-") || sold[0].ClientOrderID != orderplan.SaleClientOrderID(alarms[0].ID, "SPY") {
		t.Errorf("client id: %s", sold[0].ClientOrderID)
	}
	if states := ofType(r.decisions, event.EngineStateEventType); len(states) != 0 {
		t.Errorf("the urgent sale changes no state: %d", len(states))
	}
}

func TestSafetyRunsReplayByteIdentically(t *testing.T) {
	full := simulate(t, options{brokers: true})
	urgent := map[int]func(*event.BrokerSnapshotPayload){22: func(b *event.BrokerSnapshotPayload) { b.ExcessLiquidity = 0.1 * b.NetLiquidation }}
	for name, build := range map[string]func() run{
		"degraded": func() run { return restartAt17(t, full, options{prelude: []event.Envelope{degradedOnSPY(t)}}) },
		"halted": func() run {
			return restartAt17(t, full, options{prelude: []event.Envelope{haltedState(t)}, brokerMutate: urgent})
		},
		"kill":   func() run { return simulate(t, options{brokers: true, kill: map[int]bool{19: true}}) },
		"urgent": func() run { return simulate(t, options{brokers: true, brokerMutate: urgent}) },
	} {
		t.Run(name, func(t *testing.T) {
			first := writeJournal(t, build())
			if !bytes.Equal(first, writeJournal(t, build())) {
				t.Fatal("rerunning the same inputs must write a byte-identical journal")
			}
			if _, err := journal.Verify(bytes.NewReader(first)); err != nil {
				t.Fatal(err)
			}
			_, records, err := journal.Read(bytes.NewReader(first))
			if err != nil {
				t.Fatal(err)
			}
			inputs, decisions, err := journal.Split(records)
			if err != nil {
				t.Fatal(err)
			}
			handler, err := targetweight.New(strategyVersion, testConfiguration(), targetweight.Options{DecideFrom: sessionEnd(0)})
			if err != nil {
				t.Fatal(err)
			}
			engine, err := replay.New(handler)
			if err != nil {
				t.Fatal(err)
			}
			replayed, err := engine.Run(context.Background(), inputs)
			if err != nil {
				t.Fatal(err)
			}
			if d := replay.Equivalent(decisions, replayed); d != nil {
				t.Fatalf("replay diverged: %+v", d)
			}
		})
	}
}

func writeJournal(t *testing.T, r run) []byte {
	t.Helper()
	header, err := r.recorder.Header(r.hash, strategyVersion)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := journal.Write(&buf, header, r.entries); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRecordedDecisionsAndSafetyInputsAreChecked(t *testing.T) {
	cfg := testConfiguration()
	hash := event.TargetWeightConfigurationHash(cfg)
	start := sessionEnd(0).Add(-time.Hour)
	fresh := func() (*targetweight.Handler, *stream) {
		h, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: sessionEnd(0)})
		if err != nil {
			t.Fatal(err)
		}
		s := &stream{hash: hash}
		if _, err := h.Apply(context.Background(), s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", start, cfg)); err != nil {
			t.Fatal(err)
		}
		return h, s
	}
	refuses := func(name string, prepare func(*targetweight.Handler, *stream), in func(*stream) event.Envelope) {
		t.Helper()
		h, s := fresh()
		if prepare != nil {
			prepare(h, s)
		}
		if _, err := h.Apply(context.Background(), in(s)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	halted := event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "x"}
	planned := event.OrderPlannedPayload{
		AccountID: "ira", Tag: "t", ClientOrderID: "tw-1", InstrumentID: "SPY", Side: event.OrderSideBuy, Phase: event.OrderSideBuy,
		OrderType: event.OrderTypeLimit, Quantity: 1, LimitPrice: 1, TargetWeight: 1, NetLiquidation: 1, ReferencePrice: 1, PriceBand: 0.05,
	}
	started := func(h *targetweight.Handler, s *stream) {
		if _, err := h.Apply(context.Background(), s.envelope(t, event.CompletedBarEventType, event.CompletedBarSchemaVersion, "bar:0", sessionEnd(0), bar(100, sessionEnd(0)))); err != nil {
			t.Fatal(err)
		}
	}
	refuses("a recorded state after the run began", started, func(s *stream) event.Envelope {
		return s.envelope(t, event.EngineStateEventType, event.EngineStateSchemaVersion, "engine-state:x", start, halted)
	})
	refuses("a recorded state of another configuration", nil, func(s *stream) event.Envelope {
		e := s.envelope(t, event.EngineStateEventType, event.EngineStateSchemaVersion, "engine-state:x", start, halted)
		e.ConfigurationHash = "sha256:other"
		return e
	})
	refuses("an unknown engine-state version", nil, func(s *stream) event.Envelope {
		return s.envelope(t, event.EngineStateEventType, 3, "engine-state:x", start, halted)
	})
	refuses("an undecodable engine state", nil, func(s *stream) event.Envelope {
		e := s.envelope(t, event.EngineStateEventType, event.EngineStateSchemaVersion, "engine-state:x", start, halted)
		e.Payload = []byte("[")
		return e
	})
	refuses("an invalid engine state", nil, func(s *stream) event.Envelope {
		return s.envelope(t, event.EngineStateEventType, event.EngineStateSchemaVersion, "engine-state:x", start, event.EngineStatePayload{State: "fine"})
	})
	refuses("another account's planned order", nil, func(s *stream) event.Envelope {
		return s.envelope(t, event.OrderPlannedEventType, event.OrderPlannedSchemaVersion, "order-planned:x", start, planned)
	})
	refuses("an invalid release", nil, func(s *stream) event.Envelope {
		return s.envelope(t, event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, "risk-order-released:x", start, event.OrderReleasedPayload{})
	})
	refuses("an invalid refusal", nil, func(s *stream) event.Envelope {
		return s.envelope(t, event.OrderRefusedEventType, event.OrderRefusedSchemaVersion, "risk-order-refused:x", start, event.OrderRefusedPayload{})
	})
	refuses("an invalid withheld sale", nil, func(s *stream) event.Envelope {
		return s.envelope(t, event.SaleWithheldEventType, event.SaleWithheldSchemaVersion, "sale-withheld:x", start, event.SaleWithheldPayload{})
	})
	refuses("an invalid alarm", nil, func(s *stream) event.Envelope {
		return s.envelope(t, event.MarginAlarmEventType, event.MarginAlarmSchemaVersion, "margin-alarm:x", start, event.MarginAlarmPayload{})
	})
	refuses("another account's alarm", nil, func(s *stream) event.Envelope {
		return s.envelope(t, event.MarginAlarmEventType, event.MarginAlarmSchemaVersion, "margin-alarm:x", start, event.MarginAlarmPayload{
			AccountID: "ira", SnapshotID: "b", AsOf: start, Level: event.MarginAlarmUrgent, Cause: event.MarginAlarmCauseNoReading,
			Warning: 0.4, Urgent: 0.2, StaleMinutes: 30, Detail: "x",
		})
	})
	refuses("an invalid kill switch", nil, func(s *stream) event.Envelope {
		return s.envelope(t, event.KillSwitchEventType, event.KillSwitchSchemaVersion, "kill:x", start, event.KillSwitchPayload{})
	})
	refuses("another account's kill switch", nil, func(s *stream) event.Envelope {
		return s.envelope(t, event.KillSwitchEventType, event.KillSwitchSchemaVersion, "kill:x", start,
			event.KillSwitchPayload{AccountID: "ira", AsOf: start, Engaged: true, Operator: "o", Detail: "d"})
	})
	refuses("an invalid broker snapshot", nil, func(s *stream) event.Envelope {
		return s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "broker:x", start, event.BrokerSnapshotPayload{})
	})
	refuses("another account's broker snapshot", nil, func(s *stream) event.Envelope {
		return s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "broker:x", start,
			event.BrokerSnapshotPayload{AccountID: "ira", AsOf: start, NetLiquidation: 1})
	})
	refuses("a Halted record before a Degraded one", func(h *targetweight.Handler, s *stream) {
		if _, err := h.Apply(context.Background(), s.envelope(t, event.EngineStateEventType, event.EngineStateSchemaVersion, "engine-state:a", start, halted)); err != nil {
			t.Fatal(err)
		}
	}, func(s *stream) event.Envelope {
		return s.envelope(t, event.EngineStateEventType, event.EngineStateScopedVersion, "engine-state:b", start, event.EngineStatePayload{
			State: event.EngineStateDegraded, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "x", ScopeInstrumentIDs: []string{"SPY"},
		})
	})
}
