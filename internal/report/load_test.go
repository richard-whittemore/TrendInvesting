package report_test

import (
	"bytes"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// journalBytes writes the stream as a real ADR 0017 journal: market, account
// and configuration events are inputs, the strategy's and gate's are decisions.
func journalBytes(t *testing.T, envelopes []event.Envelope) []byte {
	t.Helper()
	var entries []journal.Entry
	for _, e := range envelopes {
		kind := journal.KindInput
		switch e.Type {
		case event.TargetsDecidedEventType, event.OrderPlannedEventType, event.OrderReleasedEventType,
			event.OrderRefusedEventType, event.EngineStateEventType:
			kind = journal.KindDecision
		}
		entries = append(entries, journal.Entry{Kind: kind, Envelope: e})
	}
	header := journal.NewHeader("h", "v", envelopes[0].EventTime, envelopes[0].EventTime)
	for _, e := range envelopes {
		if e.Type == event.CompletedBarEventType || e.Type == event.AccountSnapshotEventType {
			if e.EventTime.After(header.SpanEnd) {
				header.SpanEnd = e.EventTime
			}
		}
	}
	var buf bytes.Buffer
	if err := journal.Write(&buf, header, entries); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestLoadReturnsTheEnvelopesOfAVerifiedJournal(t *testing.T) {
	envelopes := buildDay(t)
	_, got, err := report.Load(journalBytes(t, envelopes))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(envelopes) || got[3].ID != envelopes[3].ID {
		t.Fatalf("got %d envelopes", len(got))
	}
}

func TestLoadRefusesATamperedJournal(t *testing.T) {
	raw := journalBytes(t, buildDay(t))
	tampered := bytes.Replace(raw, []byte(`"quantity":39`), []byte(`"quantity":40`), 1)
	if bytes.Equal(raw, tampered) {
		t.Fatal("the tamper did not apply")
	}
	if _, _, err := report.Load(tampered); err == nil {
		t.Fatal("a tampered journal must be refused")
	}
	if _, _, err := report.Load([]byte("not a journal")); err == nil {
		t.Fatal("garbage must be refused")
	}
}
