package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// widenedSpan rewrites a journal's header to claim a day it did not cover and
// recomputes every record hash from it — what someone holding the file and the
// published algorithm can do. The chain is the header's own seed, so a
// repaired chain verifies and the false claim survives it.
func widenedSpan(t *testing.T, path string) string {
	t.Helper()

	header, records := readJournalFile(t, path)
	header.SpanEnd = header.SpanEnd.Add(24 * time.Hour)

	chain := journal.NewChain(header)
	lines := []any{header}
	for _, record := range records {
		record.RecordHash = chain.Next(record.Kind, record.Envelope)
		lines = append(lines, record)
	}

	var written []byte
	for _, line := range lines {
		encoded, err := json.Marshal(line)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		written = append(written, append(encoded, '\n')...)
	}

	widened := filepath.Join(t.TempDir(), "widened.jsonl")
	if err := os.WriteFile(widened, written, 0o600); err != nil {
		t.Fatalf("write the widened journal: %v", err)
	}
	return widened
}

// TestAJournalClaimingASpanItsRecordsDenyVerifiesAndIsRefusedAsARecordOfARun
// is the gap the chain does not close. Tamper-evidence forces an editor to
// rewrite everything downstream; it does not stop one who does. Comparing the
// header's claim with the records it holds is what catches this.
func TestAJournalClaimingASpanItsRecordsDenyVerifiesAndIsRefusedAsARecordOfARun(t *testing.T) {
	_, path := runBacktestTo(t)
	widened := widenedSpan(t, path)

	if _, err := os.Stat(widened); err != nil {
		t.Fatalf("stat the widened journal: %v", err)
	}
	verifyJournal(t, widened)

	_, err := replayJournalFile(t, widened)
	if err == nil {
		t.Fatal("replayEquivalence() error = nil, want a journal claiming a span its records deny refused")
	}
	if !strings.Contains(err.Error(), "did not cover") {
		t.Errorf("error = %v, want it to say the header claims a span the run did not cover", err)
	}
}
