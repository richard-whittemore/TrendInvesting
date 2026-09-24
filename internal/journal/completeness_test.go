package journal_test

import (
	"bytes"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// TestVerificationCompleteUsesFinalInput pins event.RunCompletedEventType's
// end-of-input meaning, allowing the terminal decisions required by ADR 0011.
func TestVerificationCompleteUsesFinalInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kinds []string
		types []string
		want  bool
	}{
		{"no marker", []string{journal.KindInput}, []string{"test.event"}, false},
		{"final input marker", []string{journal.KindInput}, []string{event.RunCompletedEventType}, true},
		{"terminal decision after marker", []string{journal.KindInput, journal.KindDecision}, []string{event.RunCompletedEventType, "test.event"}, true},
		{"later input after marker", []string{journal.KindInput, journal.KindInput}, []string{event.RunCompletedEventType, "test.event"}, false},
		{"decision is not a marker", []string{journal.KindInput, journal.KindDecision}, []string{"test.event", event.RunCompletedEventType}, false},
		{"no input", []string{journal.KindDecision}, []string{event.RunCompletedEventType}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := journal.NewHeader(testConfigurationHash, testStrategyVersion, at(1), at(1))
			chain := journal.NewChain(header)
			var records []journal.Record
			for i, kind := range tc.kinds {
				envelope := testEnvelope(uint64(i + 1))
				envelope.Type = tc.types[i]
				if envelope.Type == event.RunCompletedEventType {
					envelope.Payload = []byte(`{}`)
					envelope.PayloadHash = event.HashPayload(envelope.Payload)
				}
				records = append(records, journal.Record{
					Sequence: uint64(i + 1), Kind: kind, Envelope: envelope, RecordHash: chain.Next(kind, envelope),
				})
			}
			got, err := journal.Verify(bytes.NewReader(writeVerbatim(t, header, records)))
			if err != nil {
				t.Fatal(err)
			}
			if got.Complete != tc.want {
				t.Fatalf("Complete = %v, want %v", got.Complete, tc.want)
			}
		})
	}
}
