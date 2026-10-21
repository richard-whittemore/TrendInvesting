package journal_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// TestTheIBKRAdaptersEngineLogFixtureIsADurableLog pins the file the IBKR
// adapter's acknowledge-correction command reads its fills from
// (adapter/ibkr/tests/testdata/engine_log.jsonl) to the durable log format
// this package writes: RecoverLog verifies its header, chain and commits,
// and finds one committed fill and a torn tail of one uncommitted fill
// record, which is not a fill the engine recorded (ADR 0027).
func TestTheIBKRAdaptersEngineLogFixtureIsADurableLog(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "adapter", "ibkr", "tests", "testdata", "engine_log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := journal.RecoverLog(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("RecoverLog: %v", err)
	}
	var fills []string
	for _, entry := range recovery.Entries {
		if entry.Envelope.Type != event.FillEventType {
			continue
		}
		if entry.Envelope.SchemaVersion != event.FillTargetWeightVersion {
			t.Fatalf("fill schema version %d, want %d", entry.Envelope.SchemaVersion, event.FillTargetWeightVersion)
		}
		var fill event.TargetWeightFillPayload
		if err := json.Unmarshal(entry.Envelope.Payload, &fill); err != nil {
			t.Fatal(err)
		}
		if err := fill.Validate(); err != nil {
			t.Fatal(err)
		}
		fills = append(fills, fill.FillID)
	}
	if len(fills) != 1 || fills[0] != "0001f4e8.paper.01.01" {
		t.Errorf("committed fills %v, want [0001f4e8.paper.01.01]", fills)
	}
	if recovery.Torn == nil || recovery.Torn.Records != 1 {
		t.Errorf("torn tail %+v, want one uncommitted record", recovery.Torn)
	}
}

// TestLogWriterWritesTheIBKRAdaptersEngineLogFixtureByteForByte pins the
// byte layout the IBKR adapter's reader depends on (adapter/ibkr/relay.py
// reads each payload's bytes between '"payload_hash":"<h>","payload":' and
// '},"record_hash":"<r>"}}' to check them against their hash): LogWriter,
// given the fixture's header and records, writes the fixture's bytes
// exactly, so a change of field order, separators or a field after the
// payload fails here before it makes every real log unreadable to the
// adapter's acknowledgement commands.
func TestLogWriterWritesTheIBKRAdaptersEngineLogFixtureByteForByte(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "adapter", "ibkr", "tests", "testdata", "engine_log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := journal.RecoverLog(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("RecoverLog: %v", err)
	}
	if recovery.Torn == nil {
		t.Fatal("the fixture has no torn tail to pin")
	}
	committed, tail := raw[:recovery.Torn.Offset], raw[recovery.Torn.Offset:]
	var torn struct {
		Record journal.Record `json:"record"`
	}
	if err := json.Unmarshal(tail, &torn); err != nil {
		t.Fatalf("the fixture's torn tail is not one record line: %v", err)
	}

	out := &disk{}
	w, err := journal.CreateLog(out, recovery.Header)
	if err != nil {
		t.Fatalf("CreateLog: %v", err)
	}
	// The fixture's groups are each one input.
	for _, entry := range recovery.Entries {
		if err := w.Append([]journal.Entry{entry}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if got := out.written.Bytes(); !bytes.Equal(got, committed) {
		t.Fatalf("LogWriter wrote\n%s\nwant the fixture's committed bytes\n%s", got, committed)
	}
	// The torn record, written as a group of its own, is the fixture's tail
	// line followed by the commit a crash cut off.
	if err := w.Append([]journal.Entry{{Kind: torn.Record.Kind, Envelope: torn.Record.Envelope}}); err != nil {
		t.Fatalf("Append the torn record: %v", err)
	}
	if got := out.written.Bytes()[len(committed):]; !bytes.HasPrefix(got, tail) {
		t.Fatalf("LogWriter wrote the torn record as\n%s\nwant the fixture's tail\n%s", got, tail)
	}
}
