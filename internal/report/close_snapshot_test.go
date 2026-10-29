package report_test

import (
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/reconcile"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// unexplainedCloseSnapshot records session n's close snapshot holding held
// SPY where the book of session n-1 held 147 and no fill was journalled, the
// check the engine journals for it (reconcile.Holdings, ADR 0019, amended
// 2026-10-21) and the Degraded state it caused, each caused by the snapshot.
func (s *stream) unexplainedCloseSnapshot(n int, held int64) {
	s.t.Helper()
	book := reconcile.Book{AnchorID: "snapshot:0", AsOf: day(n - 1), Positions: map[string]int64{"SPY": 147}, Currency: "USD"}
	s.snapshot(n, 4000, held)
	id := s.out[len(s.out)-1].ID
	next := reconcile.Book{AnchorID: id, AsOf: day(n), Positions: map[string]int64{"SPY": held}, Currency: "USD"}
	r := reconcile.Holdings("joint", &book, nil, next)
	if err := r.Validate(); err != nil {
		s.t.Fatal(err)
	}
	s.add(event.ReconciliationResultEventType, event.ReconciliationResultSchemaVersion, "reconciliation-result:joint:"+id, day(n), r)
	s.caused(id)
	s.add(event.EngineStateEventType, event.EngineStateScopedVersion, "engine-state:joint:"+id, day(n), event.EngineStatePayload{
		State: event.EngineStateDegraded, Reason: event.EngineStateReasonReconciliationUnexplained, Detail: "reconciliation " + r.ReconciliationID + " " + r.Detail,
		ScopeInstrumentIDs: []string{"SPY"},
	})
	s.caused(id)
}

// TestACloseSnapshotNoFillExplainsIsOneUrgentAlert: a trade the engine did
// not place, absorbed into the close snapshot, pages the owner once, and the
// engine state it caused is that incident, not a second alert.
func TestACloseSnapshotNoFillExplainsIsOneUrgentAlert(t *testing.T) {
	s := newStream(t)
	s.snapshot(0, 4000, 147)
	s.unexplainedCloseSnapshot(1, 150)
	got, err := report.Alerts(s.out, "boost")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != report.KindReconciliation || got[0].Severity != report.Urgent || !got[0].Repeat {
		t.Fatalf("alerts = %+v", got)
	}
	a := got[0]
	if !strings.Contains(a.Title, reconcile.CloseSnapshotPrefix+"snapshot:1") || !strings.Contains(a.Title, event.ReconciliationUnexplained) {
		t.Errorf("the title names the close snapshot's check: %q", a.Title)
	}
	for _, want := range []string{"against the book snapshot:0", "a trade the engine did not place", "SPY: book 147, expected 147, broker 150", "Degraded on instruments [SPY]", "docs/runbooks/reconciliation.md"} {
		if !strings.Contains(a.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, a.Body)
		}
	}
	if strings.Contains(a.PublicBody, "150") || !strings.Contains(a.PublicBody, "SPY") {
		t.Errorf("the public text names what is in doubt and no figure:\n%s", a.PublicBody)
	}
}
