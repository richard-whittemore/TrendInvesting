package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/registry"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

const narrowStopVariant = "profit-protecting-stop"

// TestDeclaredVariantGolden pins the declared narrow-stop Variant's decisions
// (ADR 0012) and detects rule changes without fingerprinting source (ADR 0016).
// The fixture must reach a Stop Ladder exit at or above the Campaign's entry
// (CONTEXT.md: "risk-free"); a successful run alone cannot establish that.
func TestDeclaredVariantGolden(t *testing.T) {
	fixture := filepath.Join("testdata", "variants", narrowStopVariant)
	cfg, err := readConfiguration(filepath.Join(fixture, "configuration.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantConfig := fixtureConfiguration(t)
	wantConfig.StrategyID = narrowStopVariant
	wantConfig.StopMultiple = 0.1
	if cfg != wantConfig {
		t.Fatal("Variant declaration changed: want only the named strategy identity and 0.1N Stop Multiple to differ from the existing fixture")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "journal.golden.jsonl")
	root := filepath.Join(dir, "registry")
	var log bytes.Buffer
	runErr := backtest(context.Background(), options{
		configPath: filepath.Join(fixture, "configuration.json"), barsPath: barsFixture,
		outPath: path, registryPath: root, runID: "golden", variant: narrowStopVariant, build: testBuild,
	}, &log)
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Variant run produced no readable journal: %v; run error: %v", err, runErr)
	}
	golden := filepath.Join(fixture, "journal.golden.jsonl")
	if !*updateGolden || runErr != nil {
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("read Variant golden: %v; run error: %v", err, runErr)
		}
		if runErr != nil || !bytes.Equal(written, want) {
			wantDecisions, err := decisionsFromJournal(golden)
			if err != nil {
				t.Fatal(err)
			}
			gotDecisions, err := decisionsFromJournal(path)
			if err != nil {
				t.Fatal(err)
			}
			report, err := replay.Diff(wantDecisions, gotDecisions)
			if err != nil {
				t.Fatal(err)
			}
			t.Fatalf("possible rules change in declared Variant %q (ADR 0016): journal bytes changed or run failed; review decisions and RulesVersion before accepting -update\nrun error: %v\ndecision diff: %s", narrowStopVariant, runErr, report)
		}
	}

	verification := verifyJournal(t, path)
	_, records, err := journal.Read(bytes.NewReader(written))
	if err != nil {
		t.Fatal(err)
	}
	var raised, exited bool
	for _, record := range records {
		switch record.Envelope.Type {
		case event.ProtectiveStopSetEventType:
			var stop event.ProtectiveStopSetPayload
			if err := json.Unmarshal(record.Envelope.Payload, &stop); err != nil {
				t.Fatal(err)
			}
			if stop.Reason == event.ProtectiveStopReasonAddLadder {
				raised = true
			}
		case event.CampaignExitedEventType:
			var exit event.CampaignExitedPayload
			if err := json.Unmarshal(record.Envelope.Payload, &exit); err != nil {
				t.Fatal(err)
			}
			if exit.Reason == event.ExitReasonStop && exit.ProtectiveStopLevel >= exit.EntryPrice {
				exited = true
			}
		}
	}
	if !raised || !exited {
		t.Fatalf("Variant no longer reaches its declared scenario: Stop Ladder raised=%v, stop exit at or above entry=%v", raised, exited)
	}
	entries := runsUnder(t, root, cfg)
	if len(entries) != 1 {
		t.Fatalf("Variant registered %d runs, want one", len(entries))
	}
	entry := entries[0]
	if entry.Variant != narrowStopVariant || entry.Status != registry.StatusCompleted || entry.Configuration != cfg ||
		entry.ConfigurationHash != verification.Header.ConfigurationHash || entry.StrategyVersion != verification.Header.StrategyVersion ||
		entry.SpanStart != verification.Header.SpanStart || entry.SpanEnd != verification.Header.SpanEnd ||
		entry.Artefacts.RecordCount != verification.RecordCount || entry.Artefacts.FinalRecordHash != verification.FinalRecordHash ||
		entry.Artefacts.JournalPath != "../journal.golden.jsonl" {
		t.Fatalf("Variant registry attribution or journal anchor differs: %+v", entry)
	}
	entryPath, err := entry.Path()
	if err != nil {
		t.Fatal(err)
	}
	entryBytes, err := os.ReadFile(filepath.Join(root, entryPath))
	if err != nil {
		t.Fatal(err)
	}
	registryGolden := filepath.Join(fixture, "registry", entryPath)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(registryGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, data := range map[string][]byte{golden: written, registryGolden: entryBytes} {
			if err := os.WriteFile(name, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Log("Variant golden journal and registry fixture rewritten; review both before accepting")
	} else {
		wantEntry, err := os.ReadFile(registryGolden)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(entryBytes, wantEntry) {
			t.Fatal("Variant registry fixture differs; review the declaration, outcome and journal anchor before accepting -update")
		}
		t.Log("Variant journal and registry reproduced byte-for-byte")
	}
	log.Reset()
	if err := doReplay(path, &log); err != nil {
		t.Fatalf("Variant replay: %v", err)
	}
}
