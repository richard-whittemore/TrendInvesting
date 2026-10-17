package report_test

import (
	"errors"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

func (s *stream) sessionClosed(n int) {
	s.add(event.SessionClosedEventType, event.SessionClosedSchemaVersion, "session:"+day(n).Format(time.DateOnly), day(n),
		event.SessionClosedPayload{PeriodEnd: day(n), InstrumentIDs: []string{"SPY"}})
}

// A day's decision, orders and gate records are written in the group the
// session's close opens (ADR 0027, section 2), so a day is complete once its
// market.session.closed record is in the log, and not before.
func TestTheDayASummaryReportsIsTheLatestClosedSession(t *testing.T) {
	s := newStream(t)
	if _, ok := report.ClosedSessionDate(s.out); ok {
		t.Fatal("a log with no session closed has no complete day")
	}
	s.bar(0, "SPY", 100)
	s.sessionClosed(0)
	s.decided(0, 0.98)
	s.bar(1, "SPY", 110)
	s.snapshot(1, 4400, 3)
	if got, ok := report.ClosedSessionDate(s.out); !ok || got != "2024-01-02" {
		t.Fatalf("got %q %v, want 2024-01-02: the second day has inputs but no close yet", got, ok)
	}
	if report.SessionClosedOn(s.out, "2024-01-03") || !report.SessionClosedOn(s.out, "2024-01-02") {
		t.Fatal("SessionClosedOn disagrees with the records")
	}
	s.add(event.SessionClosedEventType, 9, "session:future", day(1), event.SessionClosedPayload{PeriodEnd: day(1), InstrumentIDs: []string{"SPY"}})
	if got, _ := report.ClosedSessionDate(s.out); got != "2024-01-02" {
		t.Fatalf("a session record this build does not read closes no day, got %q", got)
	}
	s.sessionClosed(1)
	if got, _ := report.ClosedSessionDate(s.out); got != "2024-01-03" {
		t.Fatalf("got %q, want 2024-01-03", got)
	}
}

func TestTheTextSummarySaysWhetherAReleasedOrderWasSent(t *testing.T) {
	text := report.RenderText(report.Summary{AccountID: "joint", Date: "2024-01-02", Orders: []report.OrderLine{
		{ClientOrderID: "tw-a", Side: event.OrderSideBuy, Quantity: 39, InstrumentID: "SPY", LimitPrice: 101, Status: report.OrderReleased, Send: true},
		{ClientOrderID: "tw-b", Side: event.OrderSideBuy, Quantity: 1, InstrumentID: "SPY", LimitPrice: 101, Status: report.OrderReleased},
	}})
	if !contains(text, "tw-a buy 39 SPY limit 101.00: released (sent)") || !contains(text, "tw-b buy 1 SPY limit 101.00: released (not sent)") {
		t.Fatalf("the summary must say whether each released order went to the broker:\n%s", text)
	}
}

func TestAJournalTheReporterCannotReadIsItselfAnUrgentAlert(t *testing.T) {
	at := time.Date(2026, time.October, 17, 12, 0, 0, 0, time.UTC)
	a := report.JournalRefusedAlert("joint", errors.New("report: chain broken at record 7"), at)
	if a.Kind != report.KindJournalRefused || a.Severity != report.Urgent || a.AccountID != "joint" || !contains(a.Body, "chain broken at record 7") {
		t.Fatalf("alert = %+v", a)
	}
	again := report.JournalRefusedAlert("joint", errors.New("report: chain broken at record 7"), at.Add(time.Hour))
	other := report.JournalRefusedAlert("joint", errors.New("report: chain broken at record 9"), at)
	if a.Key != again.Key || a.Key == other.Key {
		t.Fatal("one failure must be one incident, and a different failure another")
	}
}
