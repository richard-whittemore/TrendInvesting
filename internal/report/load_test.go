package report_test

import (
	"bytes"
	"strings"
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

// syncBuffer is the SyncWriter a durable log is written to in tests.
type syncBuffer struct{ bytes.Buffer }

func (*syncBuffer) Sync() error { return nil }

// logBytes writes the stream as a real ADR 0027 durable log: each input
// starts a group and the decisions after it join that group. When torn is set
// the final commit line is dropped, leaving the last group uncommitted.
func logBytes(t *testing.T, envelopes []event.Envelope, torn bool) []byte {
	t.Helper()
	var groups [][]journal.Entry
	for _, e := range envelopes {
		kind := journal.KindInput
		switch e.Type {
		case event.TargetsDecidedEventType, event.OrderPlannedEventType, event.OrderReleasedEventType,
			event.OrderRefusedEventType, event.EngineStateEventType:
			kind = journal.KindDecision
		}
		if kind == journal.KindInput {
			groups = append(groups, nil)
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], journal.Entry{Kind: kind, Envelope: e})
	}
	var buf syncBuffer
	log, err := journal.CreateLog(&buf, journal.NewLogHeader("h", "v"))
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if err := log.Append(g); err != nil {
			t.Fatal(err)
		}
	}
	out := buf.Bytes()
	if torn {
		trimmed := bytes.TrimRight(out, "\n")
		out = trimmed[:bytes.LastIndexByte(trimmed, '\n')+1]
	}
	return out
}

func TestLoadReadsAnEngineDurableLog(t *testing.T) {
	envelopes := buildDay(t)
	_, got, err := report.Load(logBytes(t, envelopes, false))
	if err != nil {
		t.Fatalf("a live engine writes only the durable log, and the reporter must read it: %v", err)
	}
	if len(got) != len(envelopes) || got[3].ID != envelopes[3].ID {
		t.Fatalf("got %d envelopes, want %d", len(got), len(envelopes))
	}
}

func TestLoadReportsOnlyTheCommittedGroupsOfADurableLog(t *testing.T) {
	envelopes := buildDay(t)
	_, got, err := report.Load(logBytes(t, envelopes, true))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || len(got) >= len(envelopes) {
		t.Fatalf("a torn tail is uncommitted and must not be reported: got %d of %d", len(got), len(envelopes))
	}
}

func TestLoadRefusesADamagedDurableLog(t *testing.T) {
	raw := logBytes(t, buildDay(t), false)
	damaged := bytes.Replace(raw, []byte(`"quantity":39`), []byte(`"quantity":40`), 1)
	if bytes.Equal(raw, damaged) {
		t.Fatal("the damage did not apply")
	}
	if _, _, err := report.Load(damaged); err == nil {
		t.Fatal("a damaged durable log must be refused")
	}
}

func TestLoadRefusesADurableLogWhoseRecordsNameAnotherRun(t *testing.T) {
	for name, edit := range map[string]func(*event.Envelope){
		"configuration":    func(e *event.Envelope) { e.ConfigurationHash = "another" },
		"strategy version": func(e *event.Envelope) { e.StrategyVersion = "another" },
	} {
		t.Run(name, func(t *testing.T) {
			envelopes := buildDay(t)
			edit(&envelopes[3])
			if _, _, err := report.Load(logBytes(t, envelopes, false)); err == nil || !strings.Contains(err.Error(), "header names the run") {
				t.Fatalf("a log that mixes runs must be refused as a journal is, got %v", err)
			}
		})
	}
}
