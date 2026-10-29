package report_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// withholdPick records the per-pick evidence of ADR 0026's amendment of
// 2026-10-29 without changing the month's declared target weights.
func withholdPick(t *testing.T, s *stream, month int, id, instrument, failure, detail string) {
	t.Helper()
	payload := iraDecision(month)
	payload.WithheldPicks = []event.WithheldPick{{InstrumentID: instrument, Reason: event.RefusedMissingMark, Detail: detail, Failure: failure}}
	s.add(event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, id, monthEnd(month), payload)
}

func TestMissingMarkPickPagesOncePerMonthAndInstrument(t *testing.T) {
	s := newIRAStream(t)
	detail := "IBKR's answer to its contract request did not end within 5s, asked twice"
	withholdPick(t, s, 2, "first", "AAA", "transient", detail)
	withholdPick(t, s, 2, "repeat", "AAA", "transient", detail)
	withholdPick(t, s, 2, "other", "IEF", "final", "IBKR answered its contract request with error 200: No security definition")
	withholdPick(t, s, 3, "next", "AAA", "transient", detail)
	pages := pagesOf(alertsOf(t, s))
	if len(pages) != 3 {
		t.Fatalf("pages = %+v, want one per month and instrument (3)", pages)
	}
	keys := map[string]bool{}
	for i, p := range pages {
		if keys[p.Key] {
			t.Errorf("repeated key %q", p.Key)
		}
		keys[p.Key] = true
		instrument, classification := "AAA", "transient"
		if i == 1 {
			instrument, classification = "IEF", "final"
		}
		prefix := fmt.Sprintf("%s: %s withheld this month (missing mark: %s", iraInstance, instrument, classification)
		if !strings.HasPrefix(p.Title, prefix) || !p.Repeat || p.Severity != report.Urgent {
			t.Errorf("page = %+v", p)
		}
		for _, want := range []string{"cash", "does not degrade", p.Key} {
			if !strings.Contains(p.Body, want) {
				t.Errorf("page body missing %q: %s", want, p.Body)
			}
		}
	}
	if !strings.Contains(pages[0].Title, detail) {
		t.Errorf("adapter detail missing from title: %s", pages[0].Title)
	}
}

func TestMissingMarkPickRepeatsUntilAcknowledged(t *testing.T) {
	s := newIRAStream(t)
	withholdPick(t, s, 2, "decision", "AAA", "final", "IBKR answered its contract request with error 200: No security definition")
	pages := pagesOf(alertsOf(t, s))
	if len(pages) != 1 {
		t.Fatalf("pages = %+v, want one", pages)
	}
	page := pages[0]
	if !report.Due(page, true, monthEnd(2), monthEnd(2).Add(time.Hour), time.Minute) {
		t.Fatal("unacknowledged page did not repeat")
	}
	s.ack("ack", event.AlertAcknowledgedSchemaVersion, event.AlertAcknowledgedPayload{
		AccountID: iraInstance, AlertKey: page.Key, AcknowledgedBy: "owner", AcknowledgedAt: monthEnd(2).Add(time.Minute),
	})
	page = pagesOf(alertsOf(t, s))[0]
	if !page.Acknowledged || report.Due(page, true, monthEnd(2), monthEnd(2).Add(time.Hour), time.Minute) {
		t.Fatalf("acknowledged page repeats: %+v", page)
	}
}

func TestMissingMarkPickWithoutRecordedReasonDoesNotGuess(t *testing.T) {
	s := newIRAStream(t)
	withholdPick(t, s, 2, "decision", "AAA", "unknown", "no mark recorded for AAA at or after the session's close")
	pages := pagesOf(alertsOf(t, s))
	if len(pages) != 1 || !strings.Contains(pages[0].Title, "missing mark: unknown") {
		t.Fatalf("pages = %+v, want unknown cause", pages)
	}
}

func TestMissingMarkPickRefusalSharesItsDecisionPage(t *testing.T) {
	s := newIRAStream(t)
	withholdPick(t, s, 2, "decision", "AAA", "final", "IBKR answered its contract request with error 200: No security definition")
	p := event.OrderRefusedPayload{AccountID: iraInstance, PlanID: "plan", Tag: "decision", ClientOrderID: "order",
		InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: 1, LimitPrice: 101, Reason: event.RefusedMissingMark, Detail: "no mark", Figures: figures}
	s.add(event.OrderRefusedEventType, p.SchemaVersion(), "refusal", monthEnd(2), p)
	pages := pagesOf(alertsOf(t, s))
	if len(pages) != 1 || pages[0].Kind != report.KindPickWithheld {
		t.Fatalf("pages = %+v, want only the per-pick incident", pages)
	}

	// An unmatched refusal still alerts; old journals record no per-pick decision.
	p.Tag = "legacy-decision"
	s.add(event.OrderRefusedEventType, p.SchemaVersion(), "legacy-refusal", monthEnd(2), p)
	pages = pagesOf(alertsOf(t, s))
	if len(pages) != 3 {
		t.Fatalf("pages = %+v, want per-pick page and both legacy refusal alerts", pages)
	}
}
