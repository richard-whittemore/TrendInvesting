package risklimit_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/reconcile"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
)

// The reconciliation seam: a recorded account.reconciliation goes in, and
// the gate answers with the classification and, when the engine state moves,
// the strategy.engine.state record (ADR 0019, amended 2026-09-24).

// booked is a gate whose book is an account snapshot at the fixture time
// holding 10 SPY and $1,000.00 of cash.
func booked(t *testing.T) *risklimit.Gate {
	t.Helper()
	g := gate(t, limits(), event.TradingModeDryRun)
	g.SetBook(reconcile.CashBook("snapshot:1", event.AccountCashSnapshotPayload{
		AccountPositionsSnapshotPayload: account(10, 0), SettledCash: 1000, TotalCash: 1000,
	}))
	return g
}

// morningOf is IBKR's view an hour after the fixture time.
func morningOf(cash float64, spy int64) event.ReconciliationPayload {
	p := event.ReconciliationPayload{
		AccountID: "joint", ReconciliationID: "morning", Trigger: event.ReconciliationTriggerMorning,
		AsOf: at.Add(time.Hour), Unreadable: []string{}, Positions: []event.AccountPosition{}, TotalCash: cash, Currency: "USD",
	}
	if spy != 0 {
		p.Positions = []event.AccountPosition{{InstrumentID: "SPY", Quantity: spy}}
	}
	return p
}

func reconcileWith(t *testing.T, g *risklimit.Gate, id string, p event.ReconciliationPayload) risklimit.Reconciliation {
	t.Helper()
	r, err := g.Reconcile(id, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Result.Validate(); err != nil {
		t.Fatalf("invalid result: %v", err)
	}
	if r.ResultID != "reconciliation-result:joint:"+id {
		t.Errorf("result id %q", r.ResultID)
	}
	if r.State != nil {
		if err := r.State.ValidateSchema(r.StateSchemaVersion); err != nil {
			t.Fatalf("invalid engine state: %v", err)
		}
	}
	return r
}

func TestAMatchingReconciliationLeavesTheStateUnchanged(t *testing.T) {
	g := booked(t)
	r := reconcileWith(t, g, "reconciliation:1", morningOf(1000, 10))
	if r.Result.Classification != event.ReconciliationMatched || r.State != nil {
		t.Fatalf("got %s with state %+v", r.Result.Classification, r.State)
	}
	if g.State() != risklimit.StateNormal || g.Incident() != "" {
		t.Errorf("a match leaves the gate %s, incident %q", g.State(), g.Incident())
	}
	released(t, check(t, g, order(event.OrderSideBuy, 1, 0))[0])
}

func TestAnUnexplainedHoldingDegradesOnlyItsInstrument(t *testing.T) {
	g := booked(t)
	r := reconcileWith(t, g, "reconciliation:1", morningOf(1000, 9))
	s := r.State
	if s == nil || s.State != event.EngineStateDegraded || s.Reason != event.EngineStateReasonReconciliationUnexplained {
		t.Fatalf("want Degraded for the reconciliation, got %+v", s)
	}
	if !slices.Equal(s.ScopeInstrumentIDs, []string{"SPY"}) || len(s.ScopeCashComponents) != 0 {
		t.Errorf("scope %v %v", s.ScopeInstrumentIDs, s.ScopeCashComponents)
	}
	if r.StateID != "engine-state:joint:reconciliation:1" || g.Incident() != r.StateID || r.StateSchemaVersion != event.EngineStateScopedVersion {
		t.Errorf("the record %q (v%d) is the incident %q", r.StateID, r.StateSchemaVersion, g.Incident())
	}
	if !strings.Contains(s.Detail, "SPY: book 10, expected 10, broker 9") {
		t.Errorf("the record states the difference: %q", s.Detail)
	}
	refusedFor(t, check(t, g, order(event.OrderSideBuy, 1, 0))[0], event.RefusedEngineDegraded)
	refusedFor(t, check(t, g, order(event.OrderSideSell, 1, 10))[0], event.RefusedEngineDegraded)

	// The same difference again is the same state: the result is recorded
	// and no new state record is.
	again := reconcileWith(t, g, "reconciliation:2", morningOf(1000, 9))
	if again.Result.Action != event.ReconciliationActionDegrade || again.State != nil {
		t.Errorf("a repeat: %s, state %+v", again.Result.Action, again.State)
	}
	// A later match never returns the account to Normal.
	if m := reconcileWith(t, g, "reconciliation:3", morningOf(1000, 10)); m.State != nil || g.State() != risklimit.StateDegraded {
		t.Errorf("leaving Degraded is never automatic: %+v, %s", m.State, g.State())
	}
}

func TestACashOnlyDifferenceBlocksBuysAndLeavesSells(t *testing.T) {
	g := booked(t)
	r := reconcileWith(t, g, "reconciliation:1", morningOf(999.99, 10))
	if r.State == nil || len(r.State.ScopeInstrumentIDs) != 0 || !slices.Equal(r.State.ScopeCashComponents, []string{event.ReconciliationComponentCash}) {
		t.Fatalf("want Degraded on cash alone, got %+v", r.State)
	}
	refusedFor(t, check(t, g, order(event.OrderSideBuy, 1, 0))[0], event.RefusedEngineDegraded)
	observe(t, g, "broker:1", broker(10))
	released(t, check(t, g, order(event.OrderSideSell, 1, 10))[0])

	// A later holding difference widens the scope to both, in one record.
	wider := reconcileWith(t, g, "reconciliation:2", morningOf(999.99, 9))
	if wider.State == nil || !slices.Equal(wider.State.ScopeInstrumentIDs, []string{"SPY"}) || !slices.Equal(wider.State.ScopeCashComponents, []string{event.ReconciliationComponentCash}) {
		t.Fatalf("widened: %+v", wider.State)
	}
	if g.Incident() != r.StateID {
		t.Errorf("a wider scope is the same incident: %q", g.Incident())
	}
}

func TestUnverifiableEvidenceHaltsOnce(t *testing.T) {
	g := booked(t)
	blind := morningOf(0, 0)
	blind.Unreadable, blind.Currency = []string{event.ReconciliationComponentPositions, event.ReconciliationComponentCash}, ""
	r := reconcileWith(t, g, "reconciliation:1", blind)
	if r.State == nil || r.State.State != event.EngineStateHalted || r.State.Reason != event.EngineStateReasonReconciliationUnverifiable {
		t.Fatalf("want Halted, got %+v", r.State)
	}
	if again := reconcileWith(t, g, "reconciliation:2", blind); again.State != nil {
		t.Errorf("Halted again is the same state: %+v", again.State)
	}
	if d := reconcileWith(t, g, "reconciliation:3", morningOf(1000, 9)); d.State != nil || g.State() != risklimit.StateHalted {
		t.Errorf("an unexplained result never loosens Halted: %+v, %s", d.State, g.State())
	}
	refusedFor(t, check(t, g, order(event.OrderSideSell, 1, 10))[0], event.RefusedEngineHalted)

	// No book at all is unverifiable too.
	n := gate(t, limits(), event.TradingModeDryRun)
	if r := reconcileWith(t, n, "reconciliation:1", morningOf(1000, 10)); r.Result.Classification != event.ReconciliationUnverifiable || n.State() != risklimit.StateHalted {
		t.Errorf("no book: %s, %s", r.Result.Classification, n.State())
	}
	// A Degraded account that becomes unverifiable halts: the same incident.
	d := booked(t)
	first := reconcileWith(t, d, "reconciliation:1", morningOf(1000, 9))
	if h := reconcileWith(t, d, "reconciliation:2", blind); h.State == nil || h.State.State != event.EngineStateHalted || d.Incident() != first.StateID {
		t.Errorf("Degraded to Halted: %+v, incident %q", h.State, d.Incident())
	}
}

func TestJournalledEventsSinceTheBookExplainIt(t *testing.T) {
	g := booked(t)
	released(t, check(t, g, order(event.OrderSideSell, 4, 10))[0])
	if err := g.ObserveFill(event.TargetWeightFillPayload{
		AccountID: "joint", ClientOrderID: "tw-sell", FillID: "exec-1", InstrumentID: "SPY",
		Side: event.OrderSideSell, Quantity: 4, Price: 1, Commission: 0.5, FilledAt: at.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	dividend := event.CorporateActionPayload{InstrumentID: "SPY", Kind: event.CorporateActionKindDividend, EffectiveAt: at.Add(2 * time.Minute), CashAmount: 1.2, Currency: "USD"}
	if err := g.ObserveCorporateAction("ca:1", dividend); err != nil {
		t.Fatal(err)
	}
	// 1000 + 4 - 0.50 + 1.20 = 1004.70.
	r := reconcileWith(t, g, "reconciliation:1", morningOf(1004.7, 6))
	if r.Result.Classification != event.ReconciliationExplained || r.State != nil {
		t.Fatalf("got %s: %s", r.Result.Classification, r.Result.Detail)
	}
	// A book taken later replaces the anchor; the fill is then in it.
	g.SetBook(reconcile.CashBook("snapshot:2", event.AccountCashSnapshotPayload{
		AccountPositionsSnapshotPayload: account(6, 30), SettledCash: 1004.7, TotalCash: 1004.7,
	}))
	if r := reconcileWith(t, g, "reconciliation:2", morningOf(1004.7, 6)); r.Result.Classification != event.ReconciliationMatched || r.Result.AnchorID != "snapshot:2" {
		t.Errorf("against the later book: %s from %s", r.Result.Classification, r.Result.AnchorID)
	}
}

func TestReconcileRefusesWhatItCannotRead(t *testing.T) {
	g := booked(t)
	for name, c := range map[string]struct {
		id string
		p  event.ReconciliationPayload
	}{
		"invalid":       {"reconciliation:1", event.ReconciliationPayload{}},
		"no id":         {"", morningOf(1000, 10)},
		"other account": {"reconciliation:1", func() event.ReconciliationPayload { p := morningOf(1000, 10); p.AccountID = "ira"; return p }()},
	} {
		if _, err := g.Reconcile(c.id, c.p); err == nil {
			t.Errorf("%s: reconciled", name)
		}
	}
	if err := g.ObserveCorporateAction("ca:1", event.CorporateActionPayload{}); err == nil {
		t.Error("an invalid corporate action was recorded")
	}
	if err := g.ObserveCorporateAction("", event.CorporateActionPayload{InstrumentID: "SPY", Kind: event.CorporateActionKindDelisting, EffectiveAt: at}); err == nil {
		t.Error("a corporate action with no envelope id was recorded")
	}
	if g.State() != risklimit.StateNormal {
		t.Errorf("a refused input changes no state: %s", g.State())
	}
}

// TestARecordedCashScopeIsRestored: a restart while Degraded on cash alone
// resumes in that scope (ADR 0019, amended 2026-09-24, "The state survives
// a restart"): buys refused, sells released.
func TestARecordedCashScopeIsRestored(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	cash := event.EngineStatePayload{State: event.EngineStateDegraded, Reason: event.EngineStateReasonReconciliationUnexplained, Detail: "x", ScopeCashComponents: []string{event.ReconciliationComponentCash}}
	if err := g.RestoreState("engine-state:joint:r", cash, event.EngineStateScopedVersion); err != nil {
		t.Fatal(err)
	}
	refusedFor(t, check(t, g, order(event.OrderSideBuy, 1, 0))[0], event.RefusedEngineDegraded)
	observe(t, g, "broker:1", broker(10))
	released(t, check(t, g, order(event.OrderSideSell, 1, 10))[0])
	// A limit breach then names its instrument and keeps the cash scope.
	big := order(event.OrderSideSell, 11, 10)
	out := check(t, g, big)[0]
	if out.State == nil || !slices.Equal(out.State.ScopeInstrumentIDs, []string{"SPY"}) || !slices.Equal(out.State.ScopeCashComponents, []string{event.ReconciliationComponentCash}) {
		t.Errorf("the breach's record states the whole scope: %+v", out.State)
	}
}

// TestAnApprovedCashIncidentStillRefusesBuys: a fresh run approved with a
// Degraded incident whose scope covers cash resumes in that same scope (ADR
// 0019, amended 2026-09-24, "The state survives a restart"): a cash-only
// scope is restored, not refused, and a scope of an instrument and cash
// keeps the cash, so buys stay refused and sells go.
func TestAnApprovedCashIncidentStillRefusesBuys(t *testing.T) {
	cash := []string{event.ReconciliationComponentCash}
	for name, instruments := range map[string][]string{"cash alone": nil, "instrument and cash": {"SPY"}} {
		g := gate(t, limits(), event.TradingModeDryRun)
		if err := g.RestoreIncidentState("engine-state:joint:1", event.EngineStateDegraded, instruments, cash); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if g.State() != risklimit.StateDegraded {
			t.Fatalf("%s: the gate is %s", name, g.State())
		}
		refusedFor(t, check(t, g, order(event.OrderSideBuy, 1, 0))[0], event.RefusedEngineDegraded)
	}
	sells := gate(t, limits(), event.TradingModeDryRun)
	if err := sells.RestoreIncidentState("engine-state:joint:1", event.EngineStateDegraded, nil, cash); err != nil {
		t.Fatal(err)
	}
	observe(t, sells, "broker:1", broker(10))
	released(t, check(t, sells, order(event.OrderSideSell, 1, 10))[0])
	halted := gate(t, limits(), event.TradingModeDryRun)
	if err := halted.RestoreIncidentState("engine-state:joint:1", event.EngineStateHalted, nil, cash); err == nil {
		t.Error("a Halted incident naming a cash scope was restored")
	}
}
