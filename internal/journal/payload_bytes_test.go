package journal_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// TestWritePreservesCompactPayloadBytes checks ADR 0017's chain and exact
// payload preservation across combinations of JSON string representations.
func TestWritePreservesCompactPayloadBytes(t *testing.T) {
	t.Parallel()
	fragments := []string{`AT&T`, `<x>`, `é`, `€`, `\"`, `\\`, `\u0026`, `\u00e9`, `a b`}
	var entries []journal.Entry
	for _, left := range fragments {
		for _, right := range fragments {
			for _, shape := range []string{`"%s"`, `{"id":"%s"}`, `{"nested":[{"id":"%s"}],"n":1e+02}`} {
				envelope := testEnvelope(1)
				envelope.Sequence = uint64(len(entries) + 1)
				envelope.ID = fmt.Sprintf("evt-%d", envelope.Sequence)
				envelope.Payload = json.RawMessage(fmt.Sprintf(shape, left+right))
				envelope.PayloadHash = event.HashPayload(envelope.Payload)
				kind := journal.KindInput
				if len(entries)%2 == 1 {
					kind = journal.KindDecision
				}
				entries = append(entries, journal.Entry{Kind: kind, Envelope: envelope})
			}
		}
	}
	header := journal.NewHeader(testConfigurationHash, testStrategyVersion, at(1), at(1))
	var written bytes.Buffer
	if err := journal.Write(&written, header, entries); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := bytes.Count(written.Bytes(), []byte{'\n'}); got != len(entries)+1 {
		t.Fatalf("journal has %d newlines, want %d", got, len(entries)+1)
	}
	if _, err := journal.Verify(bytes.NewReader(written.Bytes())); err != nil {
		t.Errorf("Verify untouched journal: %v", err)
	}
	_, records, err := journal.Read(bytes.NewReader(written.Bytes()))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != len(entries) {
		t.Fatalf("Read returned %d records, want %d", len(records), len(entries))
	}
	for i, record := range records {
		want := entries[i].Envelope
		if !bytes.Equal(record.Envelope.Payload, want.Payload) {
			t.Fatalf("record %d payload = %s, want byte-identical %s", i+1, record.Envelope.Payload, want.Payload)
		}
		if record.Envelope.PayloadHash != want.PayloadHash {
			t.Fatalf("record %d payload hash changed", i+1)
		}
		if err := record.Envelope.Validate(); err != nil {
			t.Fatalf("record %d no longer validates: %v", i+1, err)
		}
	}
	t.Logf("verified %d compact payloads with byte-identical round trips", len(entries))
}
