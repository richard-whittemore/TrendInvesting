package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/registry"
)

// A run that outgrows memory used to die on an allocation having written
// nothing at all. Bounded, it stops at a stated record count, journals what it
// recorded and is registered as the failed run it is.

// TestARunThatReachesTheRecordBoundIsJournalledAndRecordedAsFailed
func TestARunThatReachesTheRecordBoundIsJournalledAndRecordedAsFailed(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "journal.jsonl")
	root := filepath.Join(dir, "runs")

	var log bytes.Buffer
	err := backtest(context.Background(), options{
		configPath:   configurationFixture,
		barsPath:     barsFixture,
		outPath:      out,
		build:        testBuild,
		maxRecords:   5,
		registryPath: root,
		runID:        "reached-the-bound",
		variant:      registry.Baseline,
	}, &log)

	var limit *journal.RecordLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("backtest() error = %v, want a *journal.RecordLimitError", err)
	}
	if !strings.Contains(err.Error(), "-max-records") {
		t.Errorf("backtest() error = %v, want it to name the flag that raises the bound", err)
	}

	verification := verifyJournal(t, out)
	if verification.RecordCount == 0 {
		t.Fatal("the run left no records; the bound exists so a run that outgrows memory still leaves what it took")
	}
	if verification.RecordCount > 8 {
		t.Errorf("RecordCount = %d, want the bound of 5 overshot by at most the final input's own decisions", verification.RecordCount)
	}

	entry := onlyRun(t, root)
	if entry.Status != registry.StatusFailed {
		t.Errorf("status = %q, want %q: a run that stopped before the end of its input stream failed", entry.Status, registry.StatusFailed)
	}
	if entry.Artefacts.RecordCount != verification.RecordCount {
		t.Errorf("the registry anchors %d records, want the %d the journal holds", entry.Artefacts.RecordCount, verification.RecordCount)
	}
}

// TestTheJournalOfARunStoppedAtTheBoundReplays: the truncated journal is a
// faithful record, not a broken one — every input it holds kept the decisions
// it caused, which is why the bound is checked at an input boundary.
func TestTheJournalOfARunStoppedAtTheBoundReplays(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "journal.jsonl")

	var log bytes.Buffer
	if err := backtest(context.Background(), options{
		configPath: configurationFixture,
		barsPath:   barsFixture,
		outPath:    out,
		build:      testBuild,
		maxRecords: 12,
	}, &log); err == nil {
		t.Fatal("backtest() error = nil, want the run stopped at its bound")
	}

	divergence, err := replayJournalFile(t, out)
	if err != nil {
		t.Fatalf("replayEquivalence() error = %v, want the truncated journal to replay", err)
	}
	if divergence != nil {
		t.Fatalf("replay diverged at %+v; a journal truncated at an input boundary holds every decision its inputs caused", divergence)
	}
}

func TestARecordBoundBelowOneIsRefused(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{
		"-config", configurationFixture,
		"-bars", barsFixture,
		"-out", filepath.Join(t.TempDir(), "journal.jsonl"),
		"-max-records", "0",
	}, &out)
	if err == nil {
		t.Fatal("run() error = nil, want a bound that records nothing refused")
	}
	if !strings.Contains(err.Error(), "-max-records") {
		t.Errorf("run() error = %v, want it to name the flag", err)
	}
}
