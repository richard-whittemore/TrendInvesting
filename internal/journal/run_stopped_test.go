package journal_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// stopPayloadJSON is a valid event.AdapterRunStoppedPayload, encoded for a
// hand-built record.
func stopPayloadJSON(t *testing.T) []byte {
	t.Helper()
	encoded, err := json.Marshal(event.AdapterRunStoppedPayload{
		Reason: event.AdapterRunStoppedReasonDelisted, InstrumentID: "AAPL", Detail: "LEAN reports AAPL DELISTED",
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// verifyRecords writes kinds/types as a valid, chained journal (mirroring
// TestVerificationCompleteUsesFinalInput) and returns what Verify reports.
func verifyRecords(t *testing.T, kinds, types []string, payloads map[int][]byte) journal.Verification {
	t.Helper()
	header := journal.NewHeader(testConfigurationHash, testStrategyVersion, at(1), at(1))
	chain := journal.NewChain(header)
	var records []journal.Record
	for i, kind := range kinds {
		envelope := testEnvelope(uint64(i + 1))
		envelope.Type = types[i]
		if raw, ok := payloads[i]; ok {
			envelope.Payload = raw
			envelope.PayloadHash = event.HashPayload(raw)
		} else if envelope.Type == event.RunCompletedEventType {
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
	return got
}

// TestVerificationStoppedNamesTheReasonAndInstrument is the claim on
// journal.Verify: a run an adapter deliberately stopped is reported as such,
// with the reason and instrument its own payload named, not merely as
// "complete" like a run that reached its last bar.
func TestVerificationStoppedNamesTheReasonAndInstrument(t *testing.T) {
	t.Parallel()

	got := verifyRecords(t,
		[]string{journal.KindInput, journal.KindInput},
		[]string{event.AdapterRunStoppedEventType, event.RunCompletedEventType},
		map[int][]byte{0: stopPayloadJSON(t)})

	if !got.Stopped {
		t.Fatal("Stopped = false, want true")
	}
	if got.StopReason != event.AdapterRunStoppedReasonDelisted {
		t.Errorf("StopReason = %q, want %q", got.StopReason, event.AdapterRunStoppedReasonDelisted)
	}
	if got.StopInstrumentID != "AAPL" {
		t.Errorf("StopInstrumentID = %q, want %q", got.StopInstrumentID, "AAPL")
	}
	if !got.Complete {
		t.Error("Complete = false, want true: the stop is followed by replay.run.completed")
	}
}

// TestVerificationNotStoppedWithNoAdapterRunStoppedInput: the ordinary path
// — a run that simply reached its last bar — reports Stopped false and
// leaves the reason/instrument fields empty.
func TestVerificationNotStoppedWithNoAdapterRunStoppedInput(t *testing.T) {
	t.Parallel()

	got := verifyRecords(t,
		[]string{journal.KindInput},
		[]string{event.RunCompletedEventType},
		nil)

	if got.Stopped {
		t.Fatal("Stopped = true, want false")
	}
	if got.StopReason != "" || got.StopInstrumentID != "" {
		t.Errorf("StopReason/StopInstrumentID = %q/%q, want both empty", got.StopReason, got.StopInstrumentID)
	}
	if !got.Complete {
		t.Error("Complete = false, want true")
	}
}

// TestVerifyRejectsAnUndecodableAdapterRunStoppedPayload: Verify decodes the
// stop's own payload to report StopReason/StopInstrumentID, and fails closed
// rather than silently reporting a stop with no figures when that payload
// cannot be decoded.
func TestVerifyRejectsAnUndecodableAdapterRunStoppedPayload(t *testing.T) {
	t.Parallel()

	header := journal.NewHeader(testConfigurationHash, testStrategyVersion, at(1), at(1))
	chain := journal.NewChain(header)
	envelope := testEnvelope(1)
	envelope.Type = event.AdapterRunStoppedEventType
	envelope.Payload = []byte(`[]`)
	envelope.PayloadHash = event.HashPayload(envelope.Payload)
	records := []journal.Record{
		{Sequence: 1, Kind: journal.KindInput, Envelope: envelope, RecordHash: chain.Next(journal.KindInput, envelope)},
	}

	_, err := journal.Verify(bytes.NewReader(writeVerbatim(t, header, records)))
	if err == nil {
		t.Fatal("Verify() error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "decode adapter run stopped payload") {
		t.Fatalf("Verify() error = %v, want it to name the decode failure", err)
	}
}

// TestVerifyRejectsAStopTheDomainContractRefuses: a well-chained record is
// not evidence of a stop unless its payload satisfies the stop's own
// contract (event.AdapterRunStoppedPayload.Validate) at the schema version
// this build reads (ADR 0015). Otherwise -verify would report a "verified"
// stop with no reason or instrument, one the reducer itself rejects.
func TestVerifyRejectsAStopTheDomainContractRefuses(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		payload       []byte
		schemaVersion uint32
		want          string
	}{
		{"empty payload", []byte(`{}`), event.AdapterRunStoppedSchemaVersion, "adapter run stopped"},
		{"unknown reason", []byte(`{"reason":"bored","instrument_id":"AAPL","detail":"x"}`), event.AdapterRunStoppedSchemaVersion, "adapter run stopped"},
		{"newer schema", stopPayloadJSON(t), event.AdapterRunStoppedSchemaVersion + 1, "schema version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			header := journal.NewHeader(testConfigurationHash, testStrategyVersion, at(1), at(1))
			chain := journal.NewChain(header)
			envelope := testEnvelope(1)
			envelope.Type = event.AdapterRunStoppedEventType
			envelope.SchemaVersion = tc.schemaVersion
			envelope.Payload = tc.payload
			envelope.PayloadHash = event.HashPayload(envelope.Payload)
			records := []journal.Record{
				{Sequence: 1, Kind: journal.KindInput, Envelope: envelope, RecordHash: chain.Next(journal.KindInput, envelope)},
			}
			_, err := journal.Verify(bytes.NewReader(writeVerbatim(t, header, records)))
			if err == nil {
				t.Fatal("Verify() error = nil, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Verify() error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
