package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// writeChainedLog writes haltedEntries as a durable log with a run approval
// recorded straight after the configuration, and returns its final hash.
func writeChainedLog(t *testing.T, path string, approval event.OperatorRunApprovedPayload) string {
	t.Helper()
	entries := haltedEntries(t)
	approved := journal.Entry{Kind: journal.KindInput, Envelope: envelope(t, 2, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "approved:"+filepath.Base(path), t0, approval)}
	entries = append(entries[:1], append([]journal.Entry{approved}, entries[1:]...)...)
	var buf syncBuffer
	log, err := journal.CreateLog(&buf, journal.NewLogHeader("h", "v"))
	if err != nil {
		t.Fatal(err)
	}
	var group []journal.Entry
	flush := func() {
		if len(group) > 0 {
			if err := log.Append(group); err != nil {
				t.Fatal(err)
			}
			group = nil
		}
	}
	for _, e := range entries {
		if e.Kind == journal.KindInput {
			flush()
		}
		group = append(group, e)
	}
	flush()
	if err := os.WriteFile(path, bytes.Clone(buf.Bytes()), 0o600); err != nil {
		t.Fatal(err)
	}
	return log.FinalRecordHash()
}

func approvalNaming(previous, hash string) event.OperatorRunApprovedPayload {
	latched := false
	p := event.OperatorRunApprovedPayload{AccountID: "joint", ApprovedBy: "owner", ApprovedAt: t0.Add(-time.Hour), Boost: event.BoostStateInForce{Latched: &latched}}
	if previous == "" {
		p.StartingBook = &event.StartingBook{Positions: []event.AccountPosition{}, TotalCash: 1000, Currency: "USD"}
		return p
	}
	p.PreviousLog, p.PreviousLogFinalHash = previous, hash
	return p
}

// TestReadJournalFollowsTheChainOfRunsForTheSummary: the reporter's daily
// summary reads the runs before the current one, so a restart does not
// restart the equity comparison.
func TestReadJournalFollowsTheChainOfRunsForTheSummary(t *testing.T) {
	dir := t.TempDir()
	first := writeChainedLog(t, filepath.Join(dir, "run1.log"), approvalNaming("", ""))
	current := filepath.Join(dir, "run2.log")
	writeChainedLog(t, current, approvalNaming("run1.log", first))
	earlier, envelopes, _, _, err := readJournal(current, t0, time.Minute, map[string]bool{})
	if err != nil || len(envelopes) == 0 || len(earlier) == 0 {
		t.Fatalf("earlier = %d, envelopes = %d, err = %v", len(earlier), len(envelopes), err)
	}
}

func TestReadJournalRefusesABrokenChainNamingTheLink(t *testing.T) {
	dir := t.TempDir()
	writeChainedLog(t, filepath.Join(dir, "run1.log"), approvalNaming("", ""))
	current := filepath.Join(dir, "run2.log")
	writeChainedLog(t, current, approvalNaming("run1.log", "sha256:"+strings.Repeat("0", 64)))
	_, _, _, _, err := readJournal(current, t0, time.Minute, map[string]bool{})
	if err == nil || !strings.Contains(err.Error(), "run2.log") || !strings.Contains(err.Error(), "run1.log") {
		t.Fatalf("err = %v, want the link between run2.log and run1.log", err)
	}
}
