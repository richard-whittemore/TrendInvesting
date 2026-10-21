package targetweight_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func approval(latched bool) event.OperatorRunApprovedPayload {
	return event.OperatorRunApprovedPayload{
		AccountID: "joint", PreviousLog: "run-1.log",
		PreviousLogFinalHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ApprovedBy:           "owner", ApprovedAt: time.Date(2025, time.January, 2, 14, 0, 0, 0, time.UTC),
		Boost: event.BoostStateInForce{Latched: new(latched)},
	}
}

// TestAnApprovedFreshRunStartsFromTheApprovedLatch: a fresh run started
// after the owner's approval takes Boost's latch from it, never from the
// bars (ADR 0026, point 5), so a run approved while boosted targets 1.47
// (0.98 x BOOST_SIZE 1.5, plan.md section 2) on its first decision.
func TestAnApprovedFreshRunStartsFromTheApprovedLatch(t *testing.T) {
	at := sessionEnd(0)
	h, s := configured(t)
	mustApply(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "approval", at, approval(true)))
	mustApply(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, "b", at, bar(100, at)))
	mustApply(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "a", at, (&account{cash: 4000}).snapshot(at, 100)))
	mustApply(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, "m", at, event.MarkRecordedPayload{InstrumentID: "SPY", AsOf: at, Price: 100, Source: event.AdapterIBKR}))
	out := mustApply(t, h, s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "s", at, event.SessionClosedPayload{PeriodEnd: at, InstrumentIDs: []string{"SPY"}}))
	decided := targets(t, out)
	if len(decided) != 1 || !decided[0].Boost.LatchedBefore || decided[0].Targets[0].Weight != 1.47 {
		t.Fatalf("decided %+v, want the approved latch in force and a target of 1.47", decided)
	}
}

func TestApprovalInputRules(t *testing.T) {
	at := sessionEnd(0)
	h, s := configured(t)
	mustRefuse(t, h, s.envelope(t, event.OperatorRunApprovedEventType, 1, "x", at, approval(false)), "schema version")
	mustRefuse(t, h, s.envelope(t, event.OperatorRunApprovedEventType, 4, "x", at, approval(false)), "schema version")
	mustRefuse(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "x", at, event.OperatorRunApprovedPayload{}), "invalid")
	other := approval(false)
	other.AccountID = "ira"
	mustRefuse(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "x", at, other), "another account")
	foreign := s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "x", at, approval(false))
	foreign.ConfigurationHash = "sha256:other"
	mustRefuse(t, h, foreign, "another configuration")
	mustApply(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "x", at, approval(false)))
	mustRefuse(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "y", at, approval(false)), "comes once")

	late, ls := configured(t)
	mustApply(t, late, ls.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, "b", at, bar(100, at)))
	mustRefuse(t, late, ls.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "x", at, approval(false)), "comes once")
}

// TestAFreshRunNeverAttemptsAnIncidentsSaleTwice: the incident in force and
// whether its one sale back to 98% was attempted (ADR 0019, amended
// 2026-10-15) come from the owner's approval, since a fresh run does not
// replay the previous run's log. Approved Halted while boosted, the first
// session refuses Boost's exit, which fires the incident's sale from the
// latest broker snapshot, and the next session's broker snapshot turns the
// margin alarm urgent, which fires it again. Approved with the sale
// attempted, the fresh run plans no sale; approved with it not attempted,
// the sale is planned once and never again.
func TestAFreshRunNeverAttemptsAnIncidentsSaleTwice(t *testing.T) {
	for _, c := range []struct {
		name      string
		attempted bool
		order     string
		want      int
	}{
		{"attempted and sent", true, "joint-sale-1", 0},
		{"attempted and withheld", true, "", 0},
		{"not attempted", false, "", 1},
	} {
		h, s := configured(t)
		p := approval(false)
		p.IncidentInForce = &event.IncidentInForce{IncidentID: "engine-state:joint:1", State: event.EngineStateHalted, SaleAttempted: new(c.attempted), SaleClientOrderID: c.order}
		mustApply(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "approval", sessionEnd(0), p))
		// Boosted: 60 shares at 100 against 4,000 of net liquidation, and
		// nothing fills, since only the sale is ever released while Halted.
		held := &account{cash: -2000, held: 60}
		var out []event.Envelope
		for i := range 2 {
			at := sessionEnd(i)
			out = append(out, mustApply(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, fmt.Sprintf("b%d", i), at, bar(100, at)))...)
			out = append(out, mustApply(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, fmt.Sprintf("a%d", i), at, held.snapshot(at, 100)))...)
			b := held.broker(at, 100)
			if i == 1 {
				b.ExcessLiquidity = 0.1 * b.NetLiquidation
			}
			out = append(out, mustApply(t, h, s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, fmt.Sprintf("k%d", i), at, b))...)
			out = append(out, mustApply(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, fmt.Sprintf("m%d", i), at, event.MarkRecordedPayload{InstrumentID: "SPY", AsOf: at, Price: 100, Source: event.AdapterIBKR}))...)
			out = append(out, mustApply(t, h, s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, fmt.Sprintf("s%d", i), at, event.SessionClosedPayload{PeriodEnd: at, InstrumentIDs: []string{"SPY"}}))...)
		}
		if n := len(ofType(out, event.OrderRefusedEventType)); n != 1 {
			t.Fatalf("%s: refused %d order(s), want Boost's exit refused", c.name, n)
		}
		if len(ofType(out, event.MarginAlarmEventType)) == 0 {
			t.Fatalf("%s: the urgent alarm is journalled", c.name)
		}
		sold := sales(t, out)
		if len(sold) != c.want {
			t.Errorf("%s: planned %d sale(s) back to 98%%, want %d: %+v", c.name, len(sold), c.want, sold)
		}
		for _, sale := range sold {
			if sale.Tag != "engine-state:joint:1" || sale.Sale.Kind != event.SaleKindIncident {
				t.Errorf("%s: sale %+v, want the incident's", c.name, sale)
			}
		}
	}
}

// TestAFreshRunContinuesTheIncidentsRestrictions: approved with an incident
// in force, a fresh run resumes in the incident's state and scope before any
// market input: "a crash or restart while Degraded or Halted resumes in that
// same state and scope", and leaving either is never automatic (ADR 0019,
// amended 2026-09-24, "The state survives a restart"; ADR 0026, point 5).
// Degraded on SPY, Degraded on cash alone, or Halted, the first session's
// buy is refused, never released.
func TestAFreshRunContinuesTheIncidentsRestrictions(t *testing.T) {
	at := sessionEnd(0)
	for name, in := range map[string]event.IncidentInForce{
		"degraded on SPY":  {IncidentID: "engine-state:joint:1", State: event.EngineStateDegraded, ScopeInstrumentIDs: []string{"SPY"}, SaleAttempted: new(false)},
		"halted":           {IncidentID: "engine-state:joint:1", State: event.EngineStateHalted, SaleAttempted: new(false)},
		"degraded on cash": {IncidentID: "engine-state:joint:1", State: event.EngineStateDegraded, ScopeCashComponents: []string{event.ReconciliationComponentCash}, SaleAttempted: new(false)},
	} {
		h, s := configured(t)
		p := approval(false)
		p.IncidentInForce = &in
		mustApply(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "approval", at, p))
		mustApply(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, "b", at, bar(100, at)))
		mustApply(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "a", at, (&account{cash: 4000}).snapshot(at, 100)))
		mustApply(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, "m", at, event.MarkRecordedPayload{InstrumentID: "SPY", AsOf: at, Price: 100, Source: event.AdapterIBKR}))
		out := mustApply(t, h, s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "s", at, event.SessionClosedPayload{PeriodEnd: at, InstrumentIDs: []string{"SPY"}}))
		if n := len(ofType(out, event.OrderReleasedEventType)); n != 0 {
			t.Errorf("%s: the fresh run released %d order(s) while the incident is in force", name, n)
		}
		if n := len(ofType(out, event.OrderRefusedEventType)); n != 1 {
			t.Errorf("%s: the fresh run refused %d order(s), want the buy refused", name, n)
		}
	}
}

// TestARecoveredDecisionNeverFollowsAnApproval: the approval states the
// latch in force, restored and never changed afterwards (ADR 0026, point 5),
// so a recorded strategy.targets.decided arriving after it is refused, just
// as an approval after a recovered decision is; the decision-prelude model
// that once fed such decisions in is dropped (ADR 0027, amended 2026-10-17).
func TestARecoveredDecisionNeverFollowsAnApproval(t *testing.T) {
	full := simulate(t, options{})
	decided := ofType(full.decisions, event.TargetsDecidedEventType)[3]
	at := sessionEnd(0)
	h, s := configured(t)
	mustApply(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "approval", at, approval(true)))
	mustRefuse(t, h, s.envelope(t, decided.Type, decided.SchemaVersion, decided.ID, at, decided.Payload), "comes once")
}

// TestAFreshRunReadsNothingBeforeItsApproval: a fresh run after an earlier
// one decides nothing until the owner's approval is recorded (ADR 0027,
// amended 2026-10-17), so every input but the configuration and the
// approval is refused until then. A broker snapshot that would turn the
// margin alarm urgent while Halted plans no sale; the same snapshot after
// the approval is read.
func TestAFreshRunReadsNothingBeforeItsApproval(t *testing.T) {
	at := sessionEnd(0)
	h, s := newHandler(t)
	h.AwaitApproval()
	mustApply(t, h, s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", at, testConfiguration()))
	held := &account{cash: -2000, held: 60}
	urgent := held.broker(at, 100)
	urgent.ExcessLiquidity = 0.1 * urgent.NetLiquidation
	const want = "before the owner's approval"
	if out, err := h.Apply(context.Background(), s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "k", at, urgent)); err == nil || !strings.Contains(err.Error(), want) || len(sales(t, out)) != 0 {
		t.Fatalf("broker snapshot before the approval: error %v, %d sale(s); want it refused, mentioning %q, and no sale", err, len(sales(t, out)), want)
	}
	for _, in := range []event.Envelope{
		haltedState(t),
		s.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, "b", at, bar(100, at)),
		s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "a", at, held.snapshot(at, 100)),
		s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, "m", at, event.MarkRecordedPayload{InstrumentID: "SPY", AsOf: at, Price: 100, Source: event.AdapterIBKR}),
		s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "s", at, event.SessionClosedPayload{PeriodEnd: at, InstrumentIDs: []string{"SPY"}}),
		s.envelope(t, event.KillSwitchEventType, 1, "kill", at, struct{}{}),
		s.envelope(t, event.FillEventType, 5, "fill", at, struct{}{}),
		s.envelope(t, event.OrderLifecycleEventType, 2, "lifecycle", at, struct{}{}),
		s.envelope(t, event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, "decided", at, struct{}{}),
		s.envelope(t, event.RunCompletedEventType, 1, "done", at, struct{}{}),
	} {
		mustRefuse(t, h, in, want)
	}
	p := approval(false)
	p.IncidentInForce = &event.IncidentInForce{IncidentID: "engine-state:joint:1", State: event.EngineStateHalted, SaleAttempted: new(false)}
	mustApply(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "approval", at, p))
	if sold := sales(t, mustApply(t, h, s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "k2", at, urgent))); len(sold) != 1 || sold[0].Tag != "engine-state:joint:1" {
		t.Fatalf("broker snapshot after the approval planned %+v, want the incident's one sale", sold)
	}
}

// TestAnApprovalIsTheFirstInputAfterTheConfiguration: an approval read
// after any other input is refused, so no recorded state or sale of this run
// can precede it. A recorded Halted incident followed by an urgent broker
// snapshot fires that incident's sale; an approval naming the incident
// afterwards is refused for its place in the stream, before the gate is
// asked to restore an attempt this run already made.
func TestAnApprovalIsTheFirstInputAfterTheConfiguration(t *testing.T) {
	at := sessionEnd(0)
	h, s := configured(t)
	mustApply(t, h, haltedState(t))
	held := &account{cash: -2000, held: 60}
	urgent := held.broker(at, 100)
	urgent.ExcessLiquidity = 0.1 * urgent.NetLiquidation
	if sold := sales(t, mustApply(t, h, s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "k", at, urgent))); len(sold) != 1 {
		t.Fatalf("planned %d sale(s), want the recorded incident's one sale", len(sold))
	}
	p := approval(false)
	p.IncidentInForce = &event.IncidentInForce{IncidentID: "engine-state:joint:recorded", State: event.EngineStateHalted, SaleAttempted: new(false)}
	mustRefuse(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "approval", at, p), "straight after the configuration")
}

// TestAFirstRunsApprovalAndAnEarlierVersionApply: an approval stating the
// starting book (schema version 3), a first run's included, applies as a
// fresh run's does, and a version 2 approval an earlier build recorded still
// replays (ADR 0015; ADR 0027, amended 2026-10-18).
func TestAFirstRunsApprovalAndAnEarlierVersionApply(t *testing.T) {
	at := sessionEnd(0)
	first := approval(false)
	first.PreviousLog, first.PreviousLogFinalHash = "", ""
	first.StartingBook = &event.StartingBook{Positions: []event.AccountPosition{}, TotalCash: 4000, Currency: "USD"}
	h, s := configured(t)
	mustApply(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "first", at, first))
	earlier, es := configured(t)
	mustApply(t, earlier, es.envelope(t, event.OperatorRunApprovedEventType, 2, "v2", at, approval(true)))
	stated, ss := configured(t)
	mustRefuse(t, stated, ss.envelope(t, event.OperatorRunApprovedEventType, 2, "v2-book", at, first), "starting book")
}
