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

const (
	narrowStopVariant = "profit-protecting-stop"
	// uncappedVariant keeps Faith's stop-market entry, with no price cap,
	// for the head-to-head comparison with the Baseline's 1N cap (ADR 0005
	// and ADR 0012, as amended 2026-09-24).
	uncappedVariant = "uncapped"
	// gapAboveCapBarsFixture is the golden bars with the breakout bar
	// gapping above the Baseline's price cap (variants/uncapped/README.md).
	gapAboveCapBarsFixture = "testdata/bars_gap_above_cap.json"
)

// declaredVariant is one declared Variant (ADR 0012) with a pinned golden:
// its fixture directory under testdata/variants, the bars it runs on, the one
// dimension on which its configuration may differ from the fixture's, and
// the scenario its journal must reach for the golden to mean anything.
type declaredVariant struct {
	name     string
	barsPath string
	// declare applies the Variant's one declared difference to the fixture
	// configuration; anything else differing fails the test.
	declare func(*event.ConfigurationPayload)
	// scenario fails unless the journal reaches the Variant's declared
	// scenario; a successful run alone cannot establish it.
	scenario func(t *testing.T, records []journal.Record)
}

// declaredVariants is every Variant with a pinned golden.
func declaredVariants() []declaredVariant {
	return []declaredVariant{
		{
			name:     "recompute-n-at-add",
			barsPath: "testdata/variants/recompute-n-at-add/bars.json",
			declare:  func(c *event.ConfigurationPayload) { c.RecomputeNAtAdd = true },
			scenario: assertRecomputedAdds,
		},
		{
			name: narrowStopVariant,
			// The scenario needs the Add Ladder's stop raises, so it runs on
			// the golden bars lowered until the default account, 1,000,000
			// of cash, funds all four Units (README.md; ADR 0020).
			barsPath: fourUnitBarsFixture,
			declare:  func(c *event.ConfigurationPayload) { c.StopMultiple = 0.1 },
			scenario: assertRiskFreeStopExit,
		},
		{
			name: uncappedVariant,
			// The golden bars with the breakout bar gapping above the
			// Baseline's 1N price cap (README.md).
			barsPath: gapAboveCapBarsFixture,
			declare: func(c *event.ConfigurationPayload) {
				c.BuyOrderType = event.OrderTypeStopMarket
				c.GapBufferN = 0
			},
			scenario: assertEntryFilledAboveTheCap,
		},
	}
}

// TestDeclaredVariantGolden pins every declared Variant's decisions (ADR
// 0012) and detects rule changes without fingerprinting source (ADR 0016).
func TestDeclaredVariantGolden(t *testing.T) {
	for _, v := range declaredVariants() {
		t.Run(v.name, func(t *testing.T) { runDeclaredVariantGolden(t, v) })
	}
}

func runDeclaredVariantGolden(t *testing.T, v declaredVariant) {
	fixture := filepath.Join("testdata", "variants", v.name)
	cfg, err := readConfiguration(filepath.Join(fixture, "configuration.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantConfig := fixtureConfiguration(t)
	wantConfig.StrategyID = v.name
	v.declare(&wantConfig)
	if cfg != wantConfig {
		t.Fatalf("Variant %q declaration changed: want only the named strategy identity and its one declared dimension to differ from the existing fixture", v.name)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "journal.golden.jsonl")
	root := filepath.Join(dir, "registry")
	var log bytes.Buffer
	runErr := backtest(context.Background(), options{
		configPath: filepath.Join(fixture, "configuration.json"), barsPath: v.barsPath,
		outPath: path, registryPath: root, runID: "golden", variant: v.name, build: testBuild,
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
			t.Fatalf("possible rules change in declared Variant %q (ADR 0016): journal bytes changed or run failed; review decisions and RulesVersion before accepting -update\nrun error: %v\ndecision diff: %s", v.name, runErr, report)
		}
	}

	verification := verifyJournal(t, path)
	_, records, err := journal.Read(bytes.NewReader(written))
	if err != nil {
		t.Fatal(err)
	}
	v.scenario(t, records)
	entries := runsUnder(t, root, cfg)
	if len(entries) != 1 {
		t.Fatalf("Variant registered %d runs, want one", len(entries))
	}
	entry := entries[0]
	if entry.Variant != v.name || entry.Status != registry.StatusCompleted || entry.Configuration != cfg ||
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
	log.Reset()
	if err := doReplay(context.Background(), path, &log); err != nil {
		t.Fatalf("Variant replay: %v", err)
	}
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
}

// assertRiskFreeStopExit is the narrow-stop Variant's declared scenario: a
// Stop Ladder exit at or above the Campaign's entry (CONTEXT.md:
// "risk-free").
func assertRiskFreeStopExit(t *testing.T, records []journal.Record) {
	t.Helper()
	raised := make(map[string]bool)
	var exited bool
	for _, record := range records {
		switch record.Envelope.Type {
		case event.ProtectiveStopSetEventType:
			var stop event.ProtectiveStopSetPayload
			if err := json.Unmarshal(record.Envelope.Payload, &stop); err != nil {
				t.Fatal(err)
			}
			if stop.Reason == event.ProtectiveStopReasonAddLadder && stop.Level >= stop.EntryPrice {
				raised[stop.CampaignID] = true
			}
		case event.CampaignExitedEventType:
			var exit event.CampaignExitedPayload
			if err := json.Unmarshal(record.Envelope.Payload, &exit); err != nil {
				t.Fatal(err)
			}
			if raised[exit.CampaignID] && exit.Reason == event.ExitReasonStop && exit.ProtectiveStopLevel >= exit.EntryPrice {
				exited = true
			}
		}
	}
	if !exited {
		t.Fatal("Variant no longer reaches its declared scenario: a Campaign must raise a stop to or above Unit entry and then record a stop exit at or above average entry")
	}
}

// assertEntryFilledAboveTheCap is the uncapped Variant's declared scenario:
// its stop-market entry fills at a gap above the level + 1N cap the
// Baseline rests at, which the Baseline would skip (ADR 0005, as amended
// 2026-09-24), and every proposal it makes is a stop-market order with no
// cap.
func assertEntryFilledAboveTheCap(t *testing.T, records []journal.Record) {
	t.Helper()
	caps := make(map[string]float64)
	var filledAbove bool
	for _, record := range records {
		switch record.Envelope.Type {
		case event.TradeProposalEventType:
			var p event.TradeProposalPayload
			if err := json.Unmarshal(record.Envelope.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.OrderType != event.OrderTypeStopMarket || p.PriceCap != 0 {
				t.Fatalf("proposal %s rests as %q capped at %v; the Variant declares a stop-market order with no cap", record.Envelope.ID, p.OrderType, p.PriceCap)
			}
			caps[record.Envelope.ID] = p.EntryLevel + float64(1*p.N)
		case event.FillEventType:
			var f event.FillPayload
			if err := json.Unmarshal(record.Envelope.Payload, &f); err != nil {
				t.Fatal(err)
			}
			if baselineCap, ok := caps[f.ProposalID]; ok && f.Kind == event.FillKindEntry && f.Price-f.SlippageApplied > baselineCap {
				filledAbove = true
			}
		}
	}
	if !filledAbove {
		t.Fatal("Variant no longer reaches its declared scenario: an entry must fill at a gap above the level + 1N the Baseline caps it at")
	}
}

// assertRecomputedAdds requires the ADR 0006 Variant to resize an Add and
// journal its distinct N, instead of merely carrying a Variant label.
func assertRecomputedAdds(t *testing.T, records []journal.Record) {
	t.Helper()
	changed := false
	var opening event.CampaignOpenedPayload
	for _, r := range records {
		switch r.Envelope.Type {
		case event.CampaignOpenedEventType:
			if err := json.Unmarshal(r.Envelope.Payload, &opening); err != nil {
				t.Fatal(err)
			}
		case event.AddProposalEventType:
			var p event.AddProposalPayload
			if err := json.Unmarshal(r.Envelope.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.AddN <= 0 {
				t.Fatal("Variant Add missing add_n")
			}
			if p.AddN != opening.CampaignN && p.Quantity != opening.UnitQuantity {
				changed = true
			}
		}
	}
	if !changed {
		t.Fatal("Variant never resized an Add from changed N")
	}
}
