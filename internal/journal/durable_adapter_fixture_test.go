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
