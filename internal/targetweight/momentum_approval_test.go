package targetweight_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// decemberDecision is a rebalance an earlier run of the IRA decided, whose
// orders were working when that run ended.
const decemberDecision = "targets-decided:" + iraAccount + ":2024-12-31T21:00:00Z"

// iraApproval is the owner's approval of a fresh IRA run after an earlier
// one, stating the rebalance in progress when that run ended, or none.
func iraApproval(rebalance *event.RebalanceInProgress) *event.OperatorRunApprovedPayload {
	return &event.OperatorRunApprovedPayload{
		AccountID: iraAccount, PreviousLog: "run-1.log",
		PreviousLogFinalHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ApprovedBy:           "owner", ApprovedAt: time.Date(2025, time.January, 2, 14, 0, 0, 0, time.UTC),
		Momentum: &event.MomentumStateInForce{RebalanceInProgress: rebalance},
	}
}

// december is December's rebalance, its sells working and its buys pending.
func december() *event.RebalanceInProgress {
	return &event.RebalanceInProgress{
		DecisionID: decemberDecision, DecisionTime: closeOf("2024-12-31"),
		Phase: event.RebalancePhaseSellsWorking, ClientOrderIDs: []string{"DUIRA-dec-1", "DUIRA-dec-2"},
	}
}

// TestAnApprovedFreshIRARunDecidesAsAFirstRun: an IRA's fresh run reads the
// owner's approval as its second input (ADR 0027, amended 2026-10-17); with
// no rebalance in progress it decides each month as the run would have.
func TestAnApprovedFreshIRARunDecidesAsAFirstRun(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{approval: iraApproval(nil)})
	if got := withheldReasons(t, r); !equalStrings(got, []string{"decided:", "decided:"}) {
		t.Fatalf("decisions %v, want both months decided", got)
	}
}

// TestAFreshIRARunMidRebalanceHoldsUntilCleared: a restart mid-rebalance
// sends nothing for that decision time until the owner clears it (ADR 0026,
// point 5). The fresh run cannot see the earlier run's working orders, so
// the approval states them, and every decision is withheld as
// rebalance-in-progress, nothing planned, until the owner's
// operator.rebalance.cleared names that rebalance and exactly those orders;
// the next decision time then decides afresh.
func TestAFreshIRARunMidRebalanceHoldsUntilCleared(t *testing.T) {
	held := simulateMomentum(t, momentumOptions{approval: iraApproval(december())})
	if got := withheldReasons(t, held); !equalStrings(got, []string{"withheld:rebalance-in-progress", "withheld:rebalance-in-progress"}) {
		t.Fatalf("decisions %v, want both months held by December's rebalance", got)
	}
	if n := len(planned(t, held.decisions)); n != 0 {
		t.Fatalf("a held fresh run planned %d orders", n)
	}
	if d := targets(t, held.decisions)[0]; !strings.Contains(d.Detail, decemberDecision) || !strings.Contains(d.Detail, "DUIRA-dec-1") {
		t.Errorf("the withheld decision's detail %q does not name the rebalance and its orders", d.Detail)
	}

	cleared := simulateMomentum(t, momentumOptions{approval: iraApproval(december()), beforeClose: func(s *stream, session string) []event.Envelope {
		if session != "2025-02-28" {
			return nil
		}
		return []event.Envelope{clearedAt(t, s, "cleared", session, iraAccount, decemberDecision, december().ClientOrderIDs)}
	}})
	if got := withheldReasons(t, cleared); !equalStrings(got, []string{"withheld:rebalance-in-progress", "decided:"}) {
		t.Fatalf("decisions %v, want January held and February decided once December is cleared", got)
	}
	if n := len(planned(t, cleared.decisions)); n == 0 {
		t.Error("February planned nothing after the clearance")
	}
}

// TestAClearanceMustNameTheRebalanceTheApprovalStated: a clearance of the
// rebalance carried in from the earlier run must name its decision and
// every order of it, exactly; anything else is refused and the hold stands.
func TestAClearanceMustNameTheRebalanceTheApprovalStated(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{
		approval: iraApproval(december()),
		refusals: map[string]string{
			"other decision": "the rebalance in progress is " + decemberDecision,
			"short":          "name exactly",
			"other account":  "is for account",
		},
		beforeClose: func(s *stream, session string) []event.Envelope {
			if session != "2025-01-31" {
				return nil
			}
			return []event.Envelope{
				clearedAt(t, s, "other decision", session, iraAccount, januaryDecision, december().ClientOrderIDs),
				clearedAt(t, s, "short", session, iraAccount, decemberDecision, december().ClientOrderIDs[:1]),
				clearedAt(t, s, "other account", session, "joint", decemberDecision, december().ClientOrderIDs),
			}
		},
	})
	if got := withheldReasons(t, r); !equalStrings(got, []string{"withheld:rebalance-in-progress", "withheld:rebalance-in-progress"}) {
		t.Fatalf("decisions %v, want the hold to stand after refused clearances", got)
	}
}

// TestAMomentumRunReadsOnlyAnIRAApproval: a momentum run awaiting its
// approval reads nothing else first, refuses a Boost-shaped approval (a
// latch and no momentum state, which says nothing of a rebalance in
// progress), and reads the approval once, straight after the configuration.
func TestAMomentumRunReadsOnlyAnIRAApproval(t *testing.T) {
	cfg := momentumConfiguration()
	start := closeOf("2025-01-02").Add(-12 * time.Hour)
	fresh := func() (*targetweight.Handler, *stream) {
		h, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: closeOf("2025-01-02")})
		if err != nil {
			t.Fatal(err)
		}
		s := &stream{hash: event.TargetWeightConfigurationHash(cfg)}
		h.AwaitApproval()
		mustApply(t, h, s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", start, cfg))
		return h, s
	}
	calendar := func(s *stream) event.Envelope {
		return s.envelope(t, event.SessionCalendarEventType, event.SessionCalendarSchemaVersion, "calendar", start, event.SessionCalendarPayload{Source: "fixture", Sessions: momentumCalendar()})
	}
	approve := func(s *stream, id string, p event.OperatorRunApprovedPayload) event.Envelope {
		return s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, id, start, p)
	}

	h, s := fresh()
	mustRefuse(t, h, calendar(s), "before the owner's approval")
	boost := *iraApproval(nil)
	boost.Momentum, boost.Boost = nil, event.BoostStateInForce{Latched: new(false)}
	mustRefuse(t, h, approve(s, "boost", boost), "Boost")
	other := *iraApproval(nil)
	other.AccountID = "DUOTHER"
	mustRefuse(t, h, approve(s, "other", other), "another account")
	mustApply(t, h, approve(s, "ira", *iraApproval(nil)))
	mustRefuse(t, h, approve(s, "again", *iraApproval(nil)), "comes once")
	mustApply(t, h, calendar(s))
}

// TestBoostRefusesAnIRAApproval: a Boost run's state in force is its latch,
// so an approval stating an IRA's momentum state is refused.
func TestBoostRefusesAnIRAApproval(t *testing.T) {
	at := sessionEnd(0)
	h, s := configured(t)
	ira := *iraApproval(nil)
	ira.AccountID = "joint"
	mustRefuse(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "x", at, ira), "momentum")
}

// TestAFreshIRARunResumesTheIncidentItsApprovalCarries: the state survives a
// restart (ADR 0019, amended 2026-09-24): an IRA approved with its earlier
// run's Halted incident in force refuses every order of the month, as the
// Halted account it is.
func TestAFreshIRARunResumesTheIncidentItsApprovalCarries(t *testing.T) {
	approval := iraApproval(nil)
	approval.IncidentInForce = &event.IncidentInForce{IncidentID: "engine-state:" + iraAccount + ":1", State: event.EngineStateHalted,
		ScopeInstrumentIDs: []string{}, ScopeCashComponents: []string{}, SaleAttempted: new(false)}
	r := simulateMomentum(t, momentumOptions{approval: approval, stopAfter: "2025-01-31"})
	if n := len(released(t, r.decisions)); n != 0 {
		t.Fatalf("a Halted IRA released %d orders", n)
	}
	if n := len(ofType(r.decisions, event.OrderRefusedEventType)); n == 0 {
		t.Fatal("a Halted IRA refused nothing: the incident was not restored")
	}
}

// TestTheHandlerReadsAnApprovalByTheDecodersRule: the handler reads an
// approval as the approval's own decoder does (DecodeRecordedOperatorRunApproved),
// so a momentum state that does not state its rebalance key, `{}`, is
// refused rather than read as no rebalance in progress (ADR 0026, point 5;
// ADR 0027, note of 2026-10-28), and so is a key left out of a Boost one.
func TestTheHandlerReadsAnApprovalByTheDecodersRule(t *testing.T) {
	cfg := momentumConfiguration()
	start := closeOf("2025-01-02").Add(-12 * time.Hour)
	h, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: closeOf("2025-01-02")})
	if err != nil {
		t.Fatal(err)
	}
	s := &stream{hash: event.TargetWeightConfigurationHash(cfg)}
	h.AwaitApproval()
	mustApply(t, h, s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", start, cfg))
	base := `"account_id":"` + iraAccount + `","previous_log":"run-1.log","previous_log_final_hash":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","incident_id":"","approved_by":"owner","approved_at":"2025-01-02T14:00:00Z","starting_book":null,"incident_in_force":null,"boost":null`
	mustRefuse(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "empty", start, json.RawMessage(`{`+base+`,"momentum":{}}`)), "rebalance_in_progress")
	mustApply(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "stated", start, json.RawMessage(`{`+base+`,"momentum":{"rebalance_in_progress":null}}`)))

	b, bs := configured(t)
	raw := `{"account_id":"joint","previous_log":"run-1.log","previous_log_final_hash":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","incident_id":"","approved_by":"owner","approved_at":"2025-01-02T14:00:00Z","boost":{"latched":true},"incident_in_force":null}`
	mustRefuse(t, b, bs.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "no-book", sessionEnd(0), json.RawMessage(raw)), "starting_book")
}

// TestAMomentumRunRecordsTheApprovalItAccepted: accepting the owner's
// approval, a momentum run records it as a decision naming that approval and
// the rebalance it carries in (ADR 0026, point 5; ADR 0027, note of
// 2026-10-28), so a restart reads the fact from the log; a refused approval
// records nothing.
func TestAMomentumRunRecordsTheApprovalItAccepted(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{approval: iraApproval(december()), stopAfter: "2025-01-03"})
	var recorded []event.Envelope
	for _, e := range r.entries {
		recorded = append(recorded, e.Envelope)
	}
	accepted := ofType(recorded, event.RunApprovalAcceptedEventType)
	if len(accepted) != 1 || accepted[0].SchemaVersion != event.RunApprovalAcceptedSchemaVersion || accepted[0].CausationID != "operator-run-approved" {
		t.Fatalf("recorded %+v, want one acceptance caused by the approval", accepted)
	}
	p, err := event.DecodeRunApprovalAccepted(accepted[0].Payload, accepted[0].SchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	if p.AccountID != iraAccount || p.ApprovalID != "operator-run-approved" || p.ApprovalSequence != 2 || p.RebalanceInProgress == nil || p.RebalanceInProgress.DecisionID != decemberDecision {
		t.Fatalf("recorded %+v, want the approval at sequence 2 carrying December", p)
	}

	cfg := momentumConfiguration()
	start := closeOf("2025-01-02").Add(-12 * time.Hour)
	h, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: closeOf("2025-01-02")})
	if err != nil {
		t.Fatal(err)
	}
	s := &stream{hash: event.TargetWeightConfigurationHash(cfg)}
	mustApply(t, h, s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", start, cfg))
	other := *iraApproval(nil)
	other.AccountID = "DUOTHER"
	if out, err := h.Apply(context.Background(), s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "other", start, other)); err == nil || len(out) != 0 {
		t.Fatalf("a refused approval: %v, %d decisions, want a refusal recording nothing", err, len(out))
	}
}
