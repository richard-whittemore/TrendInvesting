package journal_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

func TestVerifyRejectsInvalidEnvelopesWithAnIntactChain(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		mutate func(*event.Envelope)
		field  string
	}{
		{"missing strategy version", func(e *event.Envelope) { e.StrategyVersion = "" }, "strategy version"},
		{"mismatched payload hash", func(e *event.Envelope) { e.PayloadHash = "sha256:wrong" }, "payload hash"},
		{"unsupported envelope version", func(e *event.Envelope) { e.EnvelopeVersion++ }, "envelope version"},
	} {
		for index := range 3 {
			t.Run(fmt.Sprintf("%s/record_%d", tt.name, index+1), func(t *testing.T) {
				t.Parallel()
				header, records, err := journal.Read(bytes.NewReader(writeJournal(t)))
				if err != nil {
					t.Fatal(err)
				}
				tt.mutate(&records[index].Envelope)
				// A second invalid envelope must not hide the first invalid
				// record, whether that record is an input or a decision.
				if index+1 < len(records) {
					records[index+1].Envelope.Source = ""
				}
				forged := rechainJournal(t, header, records)
				_, err = journal.Verify(bytes.NewReader(forged))
				assertValidationError(t, err, fmt.Sprintf("record %d: invalid event envelope", index+1), tt.field)
				if strings.Contains(err.Error(), "event source") {
					t.Fatalf("Verify reported a later invalid record: %v", err)
				}
				entries := make([]journal.Entry, len(records))
				for i, record := range records {
					entries[i] = journal.Entry{Kind: record.Kind, Envelope: record.Envelope}
				}
				var output bytes.Buffer
				writeErr := journal.Write(&output, header, entries)
				if writeErr == nil || err.Error() != writeErr.Error() {
					t.Fatalf("Verify error = %v, Write error = %v; want the same validation", err, writeErr)
				}
			})
		}
	}
}

func TestVerifyRejectsInvalidHeadersWithAnIntactChain(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		mutate func(*journal.Header)
		field  string
	}{
		{"missing configuration hash", func(h *journal.Header) { h.ConfigurationHash = "" }, "configuration hash"},
		{"missing strategy version", func(h *journal.Header) { h.StrategyVersion = "" }, "strategy version"},
		{"missing span start", func(h *journal.Header) { h.SpanStart = time.Time{} }, "span"},
		{"missing span end", func(h *journal.Header) { h.SpanEnd = time.Time{} }, "span"},
		{"backwards span", func(h *journal.Header) { h.SpanEnd = h.SpanStart.Add(-time.Hour) }, "span"},
		{"unsupported chain algorithm", func(h *journal.Header) { h.ChainAlgorithm = "unknown" }, "chain algorithm"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			header, records, err := journal.Read(bytes.NewReader(writeJournal(t)))
			if err != nil {
				t.Fatal(err)
			}
			tt.mutate(&header)
			forged := rechainJournal(t, header, records)
			_, err = journal.Verify(bytes.NewReader(forged))
			assertValidationError(t, err, "invalid header", tt.field)
			var output bytes.Buffer
			writeErr := journal.Write(&output, header, testEntries(3))
			if writeErr == nil || err.Error() != writeErr.Error() {
				t.Fatalf("Verify error = %v, Write error = %v; want the same validation", err, writeErr)
			}
		})
	}
}

func assertValidationError(t *testing.T, err error, location, field string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Verify accepted an intact chain over invalid content; want %s naming %s", location, field)
	}
	var broken *journal.ChainBrokenError
	if errors.As(err, &broken) {
		t.Fatalf("Verify error = %v; validation failure must not be a ChainBrokenError", err)
	}
	if !strings.Contains(err.Error(), location) || !strings.Contains(err.Error(), field) {
		t.Fatalf("Verify error = %v; want %s naming %s", err, location, field)
	}
}

// rechainJournal repairs every hash after an edit using ADR 0017's formula,
// independently of Verify, so rejection must be about content validation.
func rechainJournal(t *testing.T, header journal.Header, records []journal.Record) []byte {
	t.Helper()
	previous := headerSeed(header)
	for i := range records {
		previous = chainHash(previous, records[i].Kind, records[i].Envelope)
		records[i].RecordHash = hashString(previous)
	}
	return writeVerbatim(t, header, records)
}
