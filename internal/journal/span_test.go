package journal_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// The header's span is a claim about the run: the first and last event time
// of the inputs it applied. Recorder derives it, but Write accepts a header
// from any caller and compared it to nothing, so a journal could chain
// perfectly, pass CheckIdentity and still state a span its own input records
// deny. These tests construct that disagreement and require it refused.

// readBack parses a written journal into the header and records a verifier
// would see.
func readBack(t *testing.T, written []byte) (journal.Header, []journal.Record) {
	t.Helper()
	header, records, err := journal.Read(bytes.NewReader(written))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}
	return header, records
}

func TestWriteRefusesAHeaderWhoseSpanTheInputRecordsDeny(t *testing.T) {
	t.Parallel()

	// testEntries(3) records inputs on day 1 and day 3, so day 1 to day 3 is
	// the only span this journal can truthfully state.
	for _, tt := range []struct {
		name       string
		start, end time.Time
	}{
		{"starting before the earliest input", at(0), at(3)},
		{"starting after the earliest input", at(2), at(3)},
		{"ending before the latest input", at(1), at(2)},
		{"ending after the latest input", at(1), at(4)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			header := journal.NewHeader(testConfigurationHash, testStrategyVersion, tt.start, tt.end)
			var buf bytes.Buffer

			err := journal.Write(&buf, header, testEntries(3))
			if err == nil {
				t.Fatal("journal.Write() error = nil, want a header stating a span the records deny to be refused")
			}
			if !strings.Contains(err.Error(), "did not cover") {
				t.Errorf("error = %v, want it to say a header cannot state a span the run did not cover", err)
			}
			if buf.Len() != 0 {
				t.Errorf("the refused write emitted %d byte(s); a journal refused before it is written is not written at all", buf.Len())
			}
		})
	}
}

// TestWriteRefusesAJournalThatRecordsNoInput: the span is the first and last
// input event time, so a file holding only decisions has no span it could
// state truthfully — and is not evidence of anything that was applied.
func TestWriteRefusesAJournalThatRecordsNoInput(t *testing.T) {
	t.Parallel()

	decisions := []journal.Entry{{Kind: journal.KindDecision, Envelope: testEnvelope(1)}}

	err := journal.Write(&bytes.Buffer{}, testHeader(), decisions)
	if err == nil {
		t.Fatal("journal.Write() error = nil, want a journal of decisions alone to be refused")
	}
	if !strings.Contains(err.Error(), "no record is an input") {
		t.Errorf("error = %v, want it to say the journal records no input", err)
	}
}

func TestCheckSpanAcceptsTheSpanTheRecordsDerive(t *testing.T) {
	t.Parallel()

	header, records := readBack(t, writeJournal(t))

	if err := journal.CheckSpan(header, records); err != nil {
		t.Fatalf("journal.CheckSpan() error = %v, want a written journal to agree with its own header", err)
	}
}

// TestCheckSpanReportsAHeaderTheRecordsDeny is the reader's half of the same
// rule: a span widened after the fact — by a hand that also repaired the
// chain — claims the run covered a period it never reached.
func TestCheckSpanReportsAHeaderTheRecordsDeny(t *testing.T) {
	t.Parallel()

	header, records := readBack(t, writeJournal(t))
	header.SpanEnd = at(9)

	err := journal.CheckSpan(header, records)
	if err == nil {
		t.Fatal("journal.CheckSpan() error = nil, want a widened span refused")
	}
	if !strings.Contains(err.Error(), "did not cover") {
		t.Errorf("error = %v, want it to say a header cannot state a span the run did not cover", err)
	}
}

// TestCheckSpanReadsTheInputStreamAlone pins which stream the span is a
// claim about. A decision is attributed to the input that caused it, and the
// span states the period the run was given, so a decision's own event time is
// not what the header is asserting.
func TestCheckSpanReadsTheInputStreamAlone(t *testing.T) {
	t.Parallel()

	entries := testEntries(3)
	entries[1].Envelope.EventTime = at(9)

	var buf bytes.Buffer
	if err := journal.Write(&buf, testHeader(), entries); err != nil {
		t.Fatalf("journal.Write() error = %v, want a decision outside the span to be no business of the span's", err)
	}

	header, records := readBack(t, buf.Bytes())
	if err := journal.CheckSpan(header, records); err != nil {
		t.Fatalf("journal.CheckSpan() error = %v, want the input stream alone to settle the span", err)
	}
}

// TestCheckSpanRefusesRecordsWithNoInputAmongThem: a file a reader was handed
// has to fail closed on the same rule the writer refuses.
func TestCheckSpanRefusesRecordsWithNoInputAmongThem(t *testing.T) {
	t.Parallel()

	header, records := readBack(t, writeJournal(t))
	for i := range records {
		records[i].Kind = journal.KindDecision
	}

	err := journal.CheckSpan(header, records)
	if err == nil {
		t.Fatal("journal.CheckSpan() error = nil, want records with no input among them refused")
	}
	if !strings.Contains(err.Error(), "no record is an input") {
		t.Errorf("error = %v, want it to say the journal records no input", err)
	}
}
