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
	}, event.EngineStateScopedVersion)
}

// restartAt17 restarts the full run from its boosted decision of session 17,
// feeding the full run's account and broker snapshots, after the recorded
// engine states in opts.prelude.
func restartAt17(t *testing.T, full run, opts options) run {
	t.Helper()
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[17]
	opts.recovery, opts.snapshots, opts.brokers = &recovery, full.inputs, true
	return simulate(t, opts)
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

// TestHaltedSellsOncePerIncident: a later urgent alarm in the same incident
// sells nothing; the incident's one sale is spent (ADR 0019, amended
// 2026-10-15; amendment of 2026-10-17: Degraded to Halted is one incident,
// with one sale per instrument for the whole episode).
func TestHaltedSellsOncePerIncident(t *testing.T) {
	full := simulate(t, options{brokers: true})
	state := haltedState(t)
	urgent := map[int]func(*event.BrokerSnapshotPayload){22: func(b *event.BrokerSnapshotPayload) { b.ExcessLiquidity = 0.1 * b.NetLiquidation }}
	r := restartAt17(t, full, options{prelude: []event.Envelope{state}, brokerMutate: urgent})
	sold := sales(t, r.decisions)
	if len(sold) != 1 || sold[0].Tag != state.ID {
		t.Fatalf("Halted: one sale tagged with the incident, got %+v", sold)
	}
	if n := len(released(t, r.decisions)); n != 1 {
		t.Fatalf("only the sale is released while Halted, got %d", n)
	}
	if len(ofType(r.decisions, event.MarginAlarmEventType)) == 0 {
		t.Fatal("the urgent alarm is journalled")
	}
	if got := ofType(r.decisions, event.SaleWithheldEventType); len(got) != 0 {
		t.Fatalf("nothing withheld either: the attempt is spent: %d", len(got))
	}
}

// TestAWithheldSaleIsAnAttempt: a sale withheld because IBKR's open orders
// were unreadable asked the owner to act by hand, so it is never attempted
// again in the incident (ADR 0019, amendment of 2026-10-17: a withheld sale
// counts as the attempt when the broker's values cannot be read).
func TestAWithheldSaleIsAnAttempt(t *testing.T) {
	full := simulate(t, options{brokers: true})
	mutate := map[int]func(*event.BrokerSnapshotPayload){
		20: func(b *event.BrokerSnapshotPayload) { b.Unreadable = []string{event.BrokerComponentOpenOrders} },
		22: func(b *event.BrokerSnapshotPayload) { b.ExcessLiquidity = 0.1 * b.NetLiquidation },
	}
	r := restartAt17(t, full, options{prelude: []event.Envelope{haltedState(t)}, brokerMutate: mutate})
	if withheld := ofType(r.decisions, event.SaleWithheldEventType); len(withheld) != 1 {
		t.Fatalf("withheld %d, want 1", len(withheld))
	}
	if got := sales(t, r.decisions); len(got) != 0 {
		t.Fatalf("a sale the owner was asked to make by hand is never attempted again: %+v", got)
	}
}

// TestAStaleWithholdIsNotAnAttempt: a sale Boost's exit fired, withheld
// because the latest broker snapshot was older than MaxSnapshotAge, changes
// no state and spends no attempt. While the account still refuses the exit,
// the next readable, positive broker snapshot decides it again under the
// same tag, before any later session (here, the earlier run's holding at
// session 21 is already at 98%, so it finds nothing to sell).
func TestAStaleWithholdIsNotAnAttempt(t *testing.T) {
	full := simulate(t, options{brokers: true})
	state := haltedState(t)
	mutate := map[int]func(*event.BrokerSnapshotPayload){
		20: func(b *event.BrokerSnapshotPayload) { b.AsOf = b.AsOf.Add(-2 * time.Hour) },
	}
	r := restartAt17(t, full, options{prelude: []event.Envelope{state}, brokerMutate: mutate})
	withheld := ofType(r.decisions, event.SaleWithheldEventType)
	if len(withheld) < 2 {
		t.Fatalf("withheld %d, want the stale one and the retry", len(withheld))
	}
	var stale, retry event.SaleWithheldPayload
	decode(t, withheld[0], &stale)
	decode(t, withheld[1], &retry)
	if stale.Reason != event.SaleWithheldStaleBrokerSnapshot || stale.SnapshotID != "broker:20" || stale.Trigger != event.SaleTriggerExit {
		t.Errorf("first: %+v", stale)
	}
	if withheld[1].ID != "sale-withheld:"+retry.ClientOrderID+":broker:21" || retry.Tag != state.ID || retry.Trigger != event.SaleTriggerExit || retry.SnapshotID != "broker:21" {
		t.Errorf("the exit's sale is decided again from the next snapshot, under the incident: %s %+v", withheld[1].ID, retry)
	}
	if got := ofType(r.decisions, event.EngineStateEventType); len(got) != 0 {
		t.Fatalf("the withhold changes no state: %d", len(got))
	}
}

// TestAStaleSnapshotRefusesTheExitAndAlertsOnce: in a Normal account Boost's
// exit checked against a stale latest broker snapshot is refused
// (stale-broker-snapshot) with the urgent alert and changes no state; the
// next session's decision plans afresh.
func TestAStaleSnapshotRefusesTheExitAndAlertsOnce(t *testing.T) {
	full := simulate(t, options{brokers: true})
	exitAt := -1
	for i, e := range full.decisions {
		if e.Type == event.OrderReleasedEventType {
			var p event.OrderReleasedPayload
			decode(t, e, &p)
			if p.Side == event.OrderSideSell {
				exitAt = i
				break
			}
		}
	}
	if exitAt < 0 {
		t.Fatal("setup: the full run has no exit")
	}
	var session int
	for i := 0; i < 60; i++ {
		if full.decisions[exitAt].EventTime.Equal(sessionEnd(i)) {
			session = i
		}
	}
	r := simulate(t, options{brokers: true, brokerMutate: map[int]func(*event.BrokerSnapshotPayload){
		session: func(b *event.BrokerSnapshotPayload) { b.AsOf = b.AsOf.Add(-2 * time.Hour) },
	}})
	refused := ofType(r.decisions, event.OrderRefusedEventType)
	if len(refused) != 1 {
		t.Fatalf("refused %d, want the exit once", len(refused))
	}
	var p event.OrderRefusedPayload
	decode(t, refused[0], &p)
	if p.Reason != event.RefusedStaleBrokerSnapshot || !p.Alert || p.Side != event.OrderSideSell {
		t.Errorf("refusal: %+v", p)
	}
	if got := ofType(r.decisions, event.EngineStateEventType); len(got) != 0 {
		t.Errorf("no state change: %d", len(got))
	}
	// The refusal leaves the plan to the next decision, which is made as usual.
	var next bool
	for _, d := range targets(t, r.decisions) {
		next = next || d.SessionPeriodEnd.After(refused[0].EventTime) && d.Outcome == event.TargetsOutcomeDecided
	}
	if !next {
		t.Error("the next session decides afresh")
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
	// A crash resumes by replaying the durable log's inputs, never from
	// recorded decisions (ADR 0027, section 6; owner decision 2026-10-16), so
	// an earlier run's planned orders and alarms are not inputs.
	refuses("a recorded planned order", nil, func(s *stream) event.Envelope {
		return s.envelope(t, event.OrderPlannedEventType, event.OrderPlannedSchemaVersion, "order-planned:x", start, event.OrderPlannedPayload{
			AccountID: cfg.AccountID, Tag: "t", ClientOrderID: "tw-1", InstrumentID: "SPY", Side: event.OrderSideBuy, Phase: event.OrderSideBuy,
			OrderType: event.OrderTypeLimit, Quantity: 1, LimitPrice: 1, TargetWeight: 1, NetLiquidation: 1, ReferencePrice: 1, PriceBand: 0.05,
		})
	})
	refuses("a recorded margin alarm", nil, func(s *stream) event.Envelope {
		return s.envelope(t, event.MarginAlarmEventType, event.MarginAlarmSchemaVersion, "margin-alarm:x", start, event.MarginAlarmPayload{})
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
			event.BrokerSnapshotPayload{AccountID: "ira", AsOf: start, NetLiquidation: 1, Positions: []event.BrokerPosition{}, OpenOrders: []event.BrokerOrder{}})
	})
	// A venue report names an order the gate released, at the version a
	// target-weight account's orders are reported at.
	fill := event.TargetWeightFillPayload{AccountID: cfg.AccountID, ClientOrderID: "tw-1", FillID: "f1", InstrumentID: "SPY", Side: event.OrderSideSell, Quantity: 1, Price: 1, FilledAt: start}
	refuses("a fill of an order never released", nil, func(s *stream) event.Envelope {
		return s.envelope(t, event.FillEventType, event.FillTargetWeightVersion, "fill:x", start, fill)
	})
	refuses("a Turtle fill version", nil, func(s *stream) event.Envelope {
		return s.envelope(t, event.FillEventType, event.FillSchemaVersion, "fill:x", start, fill)
	})
	report := event.LimitOrderLifecyclePayload{AccountID: cfg.AccountID, ClientOrderID: "tw-1", OrderID: "ib-1", InstrumentID: "SPY", Side: event.OrderSideSell, Status: event.OrderLifecycleStatusCanceled, Quantity: 1, LimitPrice: 1, OccurredAt: start}
	refuses("a lifecycle report on an order never released", nil, func(s *stream) event.Envelope {
		return s.envelope(t, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "lifecycle:x", start, report)
	})
	refuses("a Turtle lifecycle version", nil, func(s *stream) event.Envelope {
		return s.envelope(t, event.OrderLifecycleEventType, event.OrderLifecycleSchemaVersion, "lifecycle:x", start, report)
	})
	refuses("a Halted record before a Degraded one", func(h *targetweight.Handler, s *stream) {
		if _, err := h.Apply(context.Background(), s.envelope(t, event.EngineStateEventType, event.EngineStateScopedVersion, "engine-state:a", start, halted)); err != nil {
			t.Fatal(err)
		}
	}, func(s *stream) event.Envelope {
		return s.envelope(t, event.EngineStateEventType, event.EngineStateScopedVersion, "engine-state:b", start, event.EngineStatePayload{
			State: event.EngineStateDegraded, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "x", ScopeInstrumentIDs: []string{"SPY"},
		})
	})
}

// TestEachDecisionIsJournalledAtTheVersionThatHoldsIt: a sale, figures with a
// broker snapshot and a safety refusal reason are journalled at version 2 of
// their types; everything else stays at version 1, byte for byte (ADR 0015).
func TestEachDecisionIsJournalledAtTheVersionThatHoldsIt(t *testing.T) {
	full := simulate(t, options{brokers: true})
	r := restartAt17(t, full, options{prelude: []event.Envelope{degradedOnSPY(t)}})
	plain := simulate(t, options{})
	for name, c := range map[string]struct {
		es   []event.Envelope
		kind string
		want uint32
	}{
		"a sale": {r.decisions, event.OrderPlannedEventType, 0},
		"a refusal while Degraded with a broker snapshot": {r.decisions, event.OrderRefusedEventType, event.OrderRefusedSafetyVersion},
		"the sale's release":                              {r.decisions, event.OrderReleasedEventType, event.OrderReleasedSafetyVersion},
		"an ordinary order":                               {plain.decisions, event.OrderPlannedEventType, event.OrderPlannedSchemaVersion},
		"an ordinary release":                             {plain.decisions, event.OrderReleasedEventType, event.OrderReleasedSchemaVersion},
	} {
		es := ofType(c.es, c.kind)
		if len(es) == 0 {
			t.Fatalf("%s: no %s", name, c.kind)
		}
		for _, e := range es {
			want := c.want
			if c.kind == event.OrderReleasedEventType {
				// A risk-reducing sell records its exemption, a version 3 field.
				var p event.OrderReleasedPayload
				decode(t, e, &p)
				if p.Figures.RiskReducing {
					want = event.OrderReleasedSafetyVersion
				}
			}
			if want == 0 {
				var p event.OrderPlannedPayload
				decode(t, e, &p)
				want = p.SchemaVersion()
				if p.Sale != (event.SaleFigures{}) && want != event.OrderPlannedSafetyVersion {
					t.Errorf("%s: sale %s needs version 2", name, e.ID)
				}
			}
			if e.SchemaVersion != want {
				t.Errorf("%s: %s %s at version %d, want %d", name, e.Type, e.ID, e.SchemaVersion, want)
			}
		}
	}
}

// An urgent sale released from a broker snapshot is still in flight when the
// same session's Boost exit is planned: the two together never take the
// holding below the shares the sale keeps (ADR 0019 line 241, "back to 98%
// of net liquidation and never below it"; ADR 0026 line 224).
func TestAnUrgentSaleAndTheExitTogetherNeverSellBelow98(t *testing.T) {
	urgent := map[int]func(*event.BrokerSnapshotPayload){20: func(b *event.BrokerSnapshotPayload) { b.ExcessLiquidity = 0.15 * b.NetLiquidation }}
	r := simulate(t, options{brokers: true, brokerMutate: urgent})
	sold := sales(t, r.decisions)
	if len(sold) != 1 || sold[0].Sale.Kind != event.SaleKindUrgent {
		t.Fatalf("urgent sale in the exit session: %+v", sold)
	}
	keep := sold[0].Sale.KeepShares
	held := sold[0].HeldQuantity
	at := sessionEnd(20)
	for _, e := range ofType(r.decisions, event.OrderReleasedEventType) {
		var p event.OrderReleasedPayload
		decode(t, e, &p)
		if p.Side == event.OrderSideSell && !e.EventTime.After(at) && !e.EventTime.Before(at.Add(-time.Hour)) {
			held -= p.Quantity
		}
	}
	if held < keep {
		t.Fatalf("the sale and the exit together leave %d shares; the sale keeps %d", held, keep)
	}
}

// A sell the account snapshot already shows filled is no longer pending, so
// a later exit in a run fed only account snapshots is sized to the full
// target: Boost's exit trades want x V / p - q (ADR 0026, point 2, "Boost"),
// net only of sells the snapshot cannot show (ADR 0026, point 2.5).
func TestAFilledSellDoesNotShrinkALaterExit(t *testing.T) {
	closes := append(syntheticCloses(), 158, 161, 164, 167, 170, 173, 176, 179, 182, 185, 178, 176, 175, 174.5, 174.4, 179, 181)
	r := simulate(t, options{closes: closes})
	var exits []event.OrderPlannedPayload
	for _, p := range planned(t, r.decisions) {
		if p.Side == event.OrderSideSell {
			exits = append(exits, p)
		}
	}
	if len(exits) != 2 {
		t.Fatalf("two exits expected: %+v", exits)
	}
	for _, p := range exits {
		if want := int64(float64(p.HeldQuantity) - p.TargetWeight*p.NetLiquidation/p.ReferencePrice); p.Quantity != want {
			t.Errorf("exit %s sells %d of %d; the target needs %d", p.ClientOrderID, p.Quantity, p.HeldQuantity, want)
		}
	}
}

// TestARepeatedNothingToSellWithholdHasItsOwnID: nothing to sell is not an
// attempt, so a later trigger in the same incident may withhold again; each
// withhold is its own record, named for the input that triggered it.
func TestARepeatedNothingToSellWithholdHasItsOwnID(t *testing.T) {
	full := simulate(t, options{brokers: true})
	none := func(b *event.BrokerSnapshotPayload) { b.Positions = []event.BrokerPosition{} }
	mutate := map[int]func(*event.BrokerSnapshotPayload){
		20: none,
		22: func(b *event.BrokerSnapshotPayload) { none(b); b.ExcessLiquidity = 0.1 * b.NetLiquidation },
	}
	r := restartAt17(t, full, options{prelude: []event.Envelope{haltedState(t)}, brokerMutate: mutate})
	withheld := ofType(r.decisions, event.SaleWithheldEventType)
	if len(withheld) != 2 {
		t.Fatalf("withheld %d, want 2", len(withheld))
	}
	if withheld[0].ID == withheld[1].ID {
		t.Errorf("two withholds share the ID %s", withheld[0].ID)
	}
	for _, w := range withheld {
		var p event.SaleWithheldPayload
		decode(t, w, &p)
		if p.Reason != event.SaleWithheldNothingToSell {
			t.Errorf("%s: reason %s", w.ID, p.Reason)
		}
	}
}

// TestAWithheldSaleLapsesWhenTheExitIsNoLongerRefused: the kill switch
// refuses Boost's exit, which fires the sale; the latest broker snapshot is
// stale, so the sale is withheld, opening the stale episode with its alert.
// The switch is released before the next snapshot, so the exit is no longer
// in force: the gate journals one risk.sale.withheld with reason lapsed, the
// attempt unspent, and the released switch lets the next exit go as usual.
func TestAWithheldSaleLapsesWhenTheExitIsNoLongerRefused(t *testing.T) {
	r := simulate(t, options{
		brokers:      true,
		kill:         map[int]bool{19: true, 21: false},
		brokerMutate: map[int]func(*event.BrokerSnapshotPayload){20: func(b *event.BrokerSnapshotPayload) { b.AsOf = b.AsOf.Add(-2 * time.Hour) }},
	})
	withheld := ofType(r.decisions, event.SaleWithheldEventType)
	if len(withheld) != 2 {
		t.Fatalf("withheld %d, want the stale one and its lapse", len(withheld))
	}
	var stale, lapsed event.SaleWithheldPayload
	decode(t, withheld[0], &stale)
	decode(t, withheld[1], &lapsed)
	if stale.Reason != event.SaleWithheldStaleBrokerSnapshot || !stale.Alert || stale.Tag != "kill:19" {
		t.Errorf("the stale withhold opens the episode with the alert: %+v", stale)
	}
	if withheld[1].ID != "sale-lapsed:"+stale.ClientOrderID+":broker:21" || lapsed.Reason != event.SaleWithheldLapsed ||
		lapsed.Tag != stale.Tag || lapsed.ClientOrderID != stale.ClientOrderID || lapsed.Alert {
		t.Errorf("the lapse: %s %+v", withheld[1].ID, lapsed)
	}
	if got := sales(t, r.decisions); len(got) != 0 {
		t.Errorf("no sale was planned: %+v", got)
	}
}

// TestAWithheldSaleLapsesWhenTheDecisionNoLongerPlansTheSell: Halted, Boost's
// exit is refused and fires the sale, withheld because the latest broker
// snapshot is stale. The next snapshot reports a mark of zero, so it cannot
// size the sale and nothing is retried; the next session's decision, its
// holding already at the target, plans no sell, so the exit is no longer in
// force and the sale lapses, journalled for that session.
func TestAWithheldSaleLapsesWhenTheDecisionNoLongerPlansTheSell(t *testing.T) {
	full := simulate(t, options{brokers: true})
	mutate := map[int]func(*event.BrokerSnapshotPayload){
		20: func(b *event.BrokerSnapshotPayload) { b.AsOf = b.AsOf.Add(-2 * time.Hour) },
		21: func(b *event.BrokerSnapshotPayload) { b.Positions[0].Mark = 0 },
	}
	r := restartAt17(t, full, options{prelude: []event.Envelope{haltedState(t)}, brokerMutate: mutate})
	withheld := ofType(r.decisions, event.SaleWithheldEventType)
	if len(withheld) != 2 {
		t.Fatalf("withheld %d, want the stale one and its lapse", len(withheld))
	}
	var stale, lapsed event.SaleWithheldPayload
	decode(t, withheld[0], &stale)
	decode(t, withheld[1], &lapsed)
	if stale.Reason != event.SaleWithheldStaleBrokerSnapshot || lapsed.Reason != event.SaleWithheldLapsed || lapsed.ClientOrderID != stale.ClientOrderID {
		t.Fatalf("stale then lapsed: %+v %+v", stale, lapsed)
	}
	if !strings.HasPrefix(withheld[1].ID, "sale-lapsed:"+stale.ClientOrderID+":") || !withheld[1].EventTime.Equal(sessionEnd(21)) {
		t.Errorf("journalled for the session that no longer plans the sell: %s at %s", withheld[1].ID, withheld[1].EventTime)
	}
}
