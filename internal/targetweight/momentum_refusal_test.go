package targetweight_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

func momentumHandler(t *testing.T) (*targetweight.Handler, *stream) {
	t.Helper()
	cfg := momentumConfiguration()
	h, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: closeOf("2025-01-02")})
	if err != nil {
		t.Fatal(err)
	}
	s := &stream{hash: h.ConfigurationHash()}
	mustApply(t, h, s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", closeOf("2025-01-02"), cfg))
	return h, s
}

func calendarInput(t *testing.T, s *stream, id string, sessions ...string) event.Envelope {
	return s.envelope(t, event.SessionCalendarEventType, event.SessionCalendarSchemaVersion, id, closeOf("2025-01-02"),
		event.SessionCalendarPayload{Source: "fixture", Sessions: sessions})
}

func spyBar(session string, price float64) event.CompletedBarAdjustedPayload {
	p := bar(price, closeOf(session))
	return p
}

func TestMomentumCalendarRules(t *testing.T) {
	h, s := momentumHandler(t)
	mustApply(t, h, calendarInput(t, s, "c1", "2025-01-02", "2025-01-03", "2025-01-06"))
	// An overlapping calendar must agree where both speak; one that extends
	// the held calendar is merged.
	mustRefuse(t, h, calendarInput(t, s, "c2", "2025-01-03", "2025-01-07"), "disagrees")
	mustRefuse(t, h, calendarInput(t, s, "c3", "2025-01-02", "2025-01-06"), "disagrees")
	mustApply(t, h, calendarInput(t, s, "c4", "2025-01-06", "2025-01-07"))
	mustRefuse(t, h, calendarInput(t, s, "c5"), "invalid")
	// A calendar that shares no session with the one held would leave a hole
	// between them, and a date in that hole would read as no session at all:
	// it is refused, not merged.
	mustRefuse(t, h, calendarInput(t, s, "c6", "2025-01-09", "2025-01-10"), "does not overlap")

	closeSession := func(session string, ids ...string) event.Envelope {
		return s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "s:"+session, closeOf(session),
			event.SessionClosedPayload{PeriodEnd: closeOf(session), InstrumentIDs: ids})
	}
	mustRefuse(t, h, closeSession("2025-01-01", "SPY"), "not on the calendar")
	mustRefuse(t, h, closeSession("2025-01-02", "SPY"), "bars arrived for []")
	mustRefuse(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarSchemaVersion, "b1", closeOf("2025-01-02"), spyBar("2025-01-02", 500).CompletedBarPayload), "schema version 1 is not the version 2")
	mustApply(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, "b1", closeOf("2025-01-02"), spyBar("2025-01-02", 500)))
	mustRefuse(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, "b1", closeOf("2025-01-02"), spyBar("2025-01-02", 500)), "second bar")
	mustApply(t, h, closeSession("2025-01-02", "SPY"))
	mustRefuse(t, h, closeSession("2025-01-02", "SPY"), "does not follow")
	mustRefuse(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, "b0", closeOf("2025-01-02"), spyBar("2025-01-02", 500)), "does not follow")
	mustRefuse(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, "bx", closeOf("2025-01-03"), event.CompletedBarAdjustedPayload{}), "invalid")
	// A bar for a later session cannot close an earlier one.
	rebased := spyBar("2025-01-06", 500)
	rebased.AdjustedBase = "2025-01-03"
	mustRefuse(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, "b3", closeOf("2025-01-06"), rebased), "based at 2025-01-03")
	mustApply(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, "b3", closeOf("2025-01-06"), spyBar("2025-01-06", 500)))
	mustRefuse(t, h, closeSession("2025-01-03", "SPY"), "is not the session")
	mustRefuse(t, h, closeSession("2025-01-07", "SPY"), "calendar ends")
	mustRefuse(t, h, s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "sx", closeOf("2025-01-06"), event.SessionClosedPayload{}), "invalid")
}

func TestMomentumInputRules(t *testing.T) {
	h, s := momentumHandler(t)
	at := closeOf("2025-01-02")
	universe := func(session, source, sectorSource string) event.UniverseSnapshotPayload {
		return event.UniverseSnapshotPayload{Session: session, Source: source, Rows: []event.UniverseRow{
			{InstrumentID: "AAA", Type: "CS", Close: 10, Volume: 1, Sector: "Tech", SectorSource: sectorSource},
		}}
	}
	mustRefuse(t, h, s.envelope(t, event.UniverseSnapshotEventType, event.UniverseSnapshotSchemaVersion, "u", at, universe("2025-01-02", "other", "gics")), "this run reads")
	mustRefuse(t, h, s.envelope(t, event.UniverseSnapshotEventType, event.UniverseSnapshotSchemaVersion, "u", at, universe("2025-01-02", "fixture-universe", "rumour")), "not a configured sector source")
	mustRefuse(t, h, s.envelope(t, event.UniverseSnapshotEventType, event.UniverseSnapshotSchemaVersion, "u", at, event.UniverseSnapshotPayload{}), "invalid")
	mustApply(t, h, s.envelope(t, event.UniverseSnapshotEventType, event.UniverseSnapshotSchemaVersion, "u", at, universe("2025-01-02", "fixture-universe", "gics")))
	mustRefuse(t, h, s.envelope(t, event.UniverseSnapshotEventType, event.UniverseSnapshotSchemaVersion, "u", at, universe("2025-01-02", "fixture-universe", "gics")), "does not follow")

	acct := &ira{settled: 1000, held: map[string]int64{}}
	snap := acct.snapshot(at, nil)
	wrong := snap
	wrong.AccountID = "joint"
	mustRefuse(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotCashVersion, "a", at, wrong), "this run decides")
	mustRefuse(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "a", at, snap.AccountPositionsSnapshotPayload), "schema version")
	mustApply(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotCashVersion, "a", at, snap))
	mustRefuse(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotCashVersion, "a", at, snap), "does not follow")

	mark := event.MarkRecordedPayload{InstrumentID: "AAA", AsOf: at, Price: 10, Source: event.AdapterIBKR}
	mustRefuse(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, "m", at, event.MarkRecordedPayload{}), "invalid")
	mustApply(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, "m", at, mark))
	mustRefuse(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, "m", at, mark), "does not follow")

	mustRefuse(t, h, s.envelope(t, event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, "r", at, event.TargetsDecidedPayload{}), "not one a momentum run reads")
	if out := mustApply(t, h, s.envelope(t, event.RunCompletedEventType, event.RunCompletedSchemaVersion, "done", at, event.RunCompletedPayload{})); out != nil {
		t.Error("run completion decides nothing")
	}
}

// TestMomentumVenueReportRules feeds the run to its January decision, whose
// two buys are released, and checks how venue reports on them are read.
func TestMomentumVenueReportRules(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{stopAfter: "2025-01-31"})
	buys := released(t, r.decisions)
	if len(buys) != 2 {
		t.Fatalf("%d releases at the January decision", len(buys))
	}
	o := buys[0]
	s, at := r.stream, closeOf("2025-02-03")
	apply := func(eventType string, version uint32, id string, p any) ([]event.Envelope, error) {
		return r.recorder.Apply(context.Background(), s.envelope(t, eventType, version, id, at, p))
	}
	refuse := func(eventType string, version uint32, id string, p any, want string) {
		t.Helper()
		if _, err := apply(eventType, version, id, p); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: error %v, want one mentioning %q", id, err, want)
		}
	}
	lifecycle := event.LimitOrderLifecyclePayload{
		AccountID: iraAccount, ClientOrderID: o.ClientOrderID, OrderID: "ib-1", InstrumentID: o.InstrumentID, Side: o.Side,
		Status: event.OrderLifecycleStatusSubmitted, Quantity: o.Quantity, LimitPrice: o.LimitPrice, OccurredAt: at,
	}
	mutated := func(mutate func(*event.LimitOrderLifecyclePayload)) event.LimitOrderLifecyclePayload {
		p := lifecycle
		mutate(&p)
		return p
	}
	refuse(event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "l1", mutated(func(p *event.LimitOrderLifecyclePayload) { p.AccountID = "joint" }), "this run decides")
	refuse(event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "l2", mutated(func(p *event.LimitOrderLifecyclePayload) { p.ClientOrderID = "tw-unknown" }), "never released")
	refuse(event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "l3", mutated(func(p *event.LimitOrderLifecyclePayload) { p.Side = event.OrderSideSell }), "describes")
	refuse(event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "l4", mutated(func(p *event.LimitOrderLifecyclePayload) { p.Quantity++ }), "reports")
	refuse(event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "l4b", mutated(func(p *event.LimitOrderLifecyclePayload) { p.LimitPrice = o.LimitPrice + 0.01 }), "reports")
	refuse(event.OrderLifecycleEventType, event.OrderLifecycleSchemaVersion, "l5", lifecycle, "schema version")
	// The venue's echo of the limit need not be bit-equal to the engine's
	// (.greptile/rules.md, "Numbers and money").
	echoed := mutated(func(p *event.LimitOrderLifecyclePayload) { p.LimitPrice = o.LimitPrice + 1e-9 })
	if _, err := apply(event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "l6", echoed); err != nil {
		t.Fatal(err)
	}
	fill := event.TargetWeightFillPayload{
		AccountID: iraAccount, ClientOrderID: o.ClientOrderID, FillID: "f1", InstrumentID: o.InstrumentID, Side: o.Side,
		Quantity: o.Quantity - 1, Price: o.LimitPrice, Commission: 0.5, FilledAt: at,
	}
	refuse(event.FillEventType, event.FillSchemaVersion, "f0", fill, "schema version")
	joint := fill
	joint.AccountID = "joint"
	refuse(event.FillEventType, event.FillTargetWeightVersion, "f-joint", joint, "this run decides")
	over := fill
	over.Quantity = o.Quantity + 1
	refuse(event.FillEventType, event.FillTargetWeightVersion, "f-over", over, "past its")
	if _, err := apply(event.FillEventType, event.FillTargetWeightVersion, "f1", fill); err != nil {
		t.Fatal(err)
	}
	// The same execution delivered again is ignored; the same fill id for a
	// different execution is a producer defect.
	if out, err := apply(event.FillEventType, event.FillTargetWeightVersion, "f1-again", fill); err != nil || out != nil {
		t.Fatalf("a redelivered fill: %v %v", out, err)
	}
	// One instant written in another UTC offset is the same execution.
	offset := fill
	offset.FilledAt = fill.FilledAt.In(time.FixedZone("EST", -5*3600))
	if out, err := apply(event.FillEventType, event.FillTargetWeightVersion, "f1-offset", offset); err != nil || out != nil {
		t.Fatalf("a redelivery in another offset: %v %v", out, err)
	}
	reused := fill
	reused.Quantity = 1
	refuse(event.FillEventType, event.FillTargetWeightVersion, "f1-reused", reused, "reuses fill id")
	// A fill id names one execution across the account, not per order: the
	// same id on the other released order is refused, so one execution never
	// moves two orders' fills and budget.
	other := buys[1]
	elsewhere := event.TargetWeightFillPayload{
		AccountID: iraAccount, ClientOrderID: other.ClientOrderID, FillID: "f1", InstrumentID: other.InstrumentID, Side: other.Side,
		Quantity: 1, Price: other.LimitPrice, Commission: 0.5, FilledAt: at,
	}
	refuse(event.FillEventType, event.FillTargetWeightVersion, "f1-elsewhere", elsewhere, "reuses fill id")
	last := fill
	last.FillID, last.Quantity = "f2", 1
	if _, err := apply(event.FillEventType, event.FillTargetWeightVersion, "f2", last); err != nil {
		t.Fatal(err)
	}
	// The order is now filled, so terminal: nothing more is read about it.
	refuse(event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "l7", mutated(func(p *event.LimitOrderLifecyclePayload) { p.Status = event.OrderLifecycleStatusCanceled }), "already terminal")
	third := fill
	third.FillID, third.Quantity = "f3", 1
	refuse(event.FillEventType, event.FillTargetWeightVersion, "f3", third, "already terminal")
	unknown := fill
	unknown.ClientOrderID, unknown.FillID = "tw-unknown", "f4"
	refuse(event.FillEventType, event.FillTargetWeightVersion, "f4", unknown, "never released")
}

func TestMomentumMissingMarkRefusesAndDegrades(t *testing.T) {
	// No CCC mark at the January decision: its buy is refused and the account
	// is Degraded with CCC in scope; AAA's buy, checked first, is released.
	r := simulateMomentum(t, momentumOptions{noMark: map[string]string{"2025-01-31": "CCC"}, stopAfter: "2025-01-31"})
	refused := ofType(r.decisions, event.OrderRefusedEventType)
	states := ofType(r.decisions, event.EngineStateEventType)
	if len(released(t, r.decisions)) != 1 || len(refused) != 1 || len(states) != 1 {
		t.Fatalf("%d released, %d refused, %d states", len(released(t, r.decisions)), len(refused), len(states))
	}
	var p event.OrderRefusedPayload
	decode(t, refused[0], &p)
	if p.Reason != event.RefusedMissingMark || p.InstrumentID != "CCC" {
		t.Errorf("refusal: %+v", p)
	}
	if states[0].SchemaVersion != event.EngineStateScopedVersion || !strings.HasPrefix(states[0].ID, "engine-state:"+iraAccount+":tw-") {
		t.Errorf("state %s at schema %d", states[0].ID, states[0].SchemaVersion)
	}
}

// TestAMomentumRestartWhileDegradedStaysDegraded: a restart while Degraded
// resumes in that state and scope (ADR 0019, amended 2026-09-24; ADR 0026,
// point 5). The restarted run, given the recorded state, refuses every buy
// of the January decision and releases nothing.
func TestAMomentumRestartWhileDegradedStaysDegraded(t *testing.T) {
	full := simulateMomentum(t, momentumOptions{noMark: map[string]string{"2025-01-31": "CCC"}, stopAfter: "2025-01-31"})
	states := ofType(full.decisions, event.EngineStateEventType)
	restarted := simulateMomentum(t, momentumOptions{recorded: states, stopAfter: "2025-01-31"})
	if n := len(released(t, restarted.decisions)); n != 0 {
		t.Fatalf("the restarted run released %d orders; Degraded is never left automatically", n)
	}
	refused := ofType(restarted.decisions, event.OrderRefusedEventType)
	if len(refused) == 0 || len(ofType(restarted.decisions, event.EngineStateEventType)) != 0 {
		t.Fatalf("%d refused, want every buy refused with no new state", len(refused))
	}
	for _, e := range refused {
		var p event.OrderRefusedPayload
		decode(t, e, &p)
		if p.Reason != event.RefusedEngineDegraded {
			t.Errorf("refusal: %+v", p)
		}
	}
}

func TestMomentumRecordedStateComesFirst(t *testing.T) {
	full := simulateMomentum(t, momentumOptions{noMark: map[string]string{"2025-01-31": "CCC"}, stopAfter: "2025-01-31"})
	state := ofType(full.decisions, event.EngineStateEventType)[0]
	h, s := momentumHandler(t)
	at := closeOf("2025-01-02")
	mustApply(t, h, s.envelope(t, state.Type, state.SchemaVersion, state.ID, at, state.Payload))
	mustApply(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, "b", at, spyBar("2025-01-02", 500)))
	mustRefuse(t, h, s.envelope(t, state.Type, state.SchemaVersion, state.ID, at, state.Payload), "after the run began")
}
