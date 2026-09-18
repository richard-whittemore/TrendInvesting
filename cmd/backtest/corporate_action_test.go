package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

const (
	barsDelistingFixture             = "testdata/bars_delisting.json"
	barsStaleFixture                 = "testdata/bars_stale.json"
	corporateActionsDelistingFixture = "testdata/corporate_actions_delisting.json"
	corporateActionsStaleFixture     = "testdata/corporate_actions_stale.json"
	corporateActionsUntradedFixture  = "testdata/corporate_actions_untraded.json"
)

// decisionsOfType decodes every decision envelope of the given type from a
// written journal, in journal order.
func decisionsOfType(t *testing.T, written []byte, eventType string) []event.Envelope {
	t.Helper()
	_, records, err := journal.Read(bytes.NewReader(written))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}
	var out []event.Envelope
	for _, record := range records {
		if record.Kind == journal.KindDecision && record.Envelope.Type == eventType {
			out = append(out, record.Envelope)
		}
	}
	return out
}

// TestADelistingClosesACampaignInTheJournal is the command-seam test: a
// Delisting Exit (CONTEXT.md: "Delisting Exit"), correct and tested at the
// event seam already, must also be reachable through the one artefact this
// project treats as evidence — the journal a backtest run writes.
func TestADelistingClosesACampaignInTheJournal(t *testing.T) {
	out := filepath.Join(t.TempDir(), "journal.jsonl")
	opts := options{
		configPath:           configurationFixture,
		barsPath:             barsDelistingFixture,
		corporateActionsPath: corporateActionsDelistingFixture,
		outPath:              out,
		build:                testBuild,
	}

	var log bytes.Buffer
	if err := backtest(context.Background(), opts, &log); err != nil {
		t.Fatalf("backtest(%+v) error = %v\n%s", opts, err, log.String())
	}
	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}

	exits := decisionsOfType(t, written, event.CampaignExitedEventType)
	if len(exits) != 1 {
		t.Fatalf("got %d %s decisions, want 1", len(exits), event.CampaignExitedEventType)
	}
	var payload event.CampaignExitedPayload
	if err := json.Unmarshal(exits[0].Payload, &payload); err != nil {
		t.Fatalf("decode campaign exited payload: %v", err)
	}
	if payload.Reason != event.ExitReasonDelisting {
		t.Fatalf("campaign exited reason = %q, want %q", payload.Reason, event.ExitReasonDelisting)
	}
}

// TestAStaleDelistingNoticeIsRefused checks that the reducer's own
// chronology check (internal/strategy/delisting.go's applyDelisting) is what
// judges a stale notice, and that its refusal surfaces through the command
// rather than being swallowed or reimplemented here.
func TestAStaleDelistingNoticeIsRefused(t *testing.T) {
	out := filepath.Join(t.TempDir(), "journal.jsonl")
	opts := options{
		configPath:           configurationFixture,
		barsPath:             barsStaleFixture,
		corporateActionsPath: corporateActionsStaleFixture,
		outPath:              out,
		build:                testBuild,
	}

	var log bytes.Buffer
	err := backtest(context.Background(), opts, &log)
	if err == nil {
		t.Fatal("backtest() error = nil, want the reducer's stale-notice refusal")
	}
	if !strings.Contains(err.Error(), "predates the last completed bar") {
		t.Fatalf("backtest() error = %v, want it to surface the reducer's own chronology refusal", err)
	}
}

// TestADelistingForAnUntradedInstrumentDoesNotHaltTheRun checks the case
// applyDelisting's own doc comment is careful about: a notice naming an
// instrument the strategy never traded is recorded, not an error, and must
// not stop the rest of the run.
func TestADelistingForAnUntradedInstrumentDoesNotHaltTheRun(t *testing.T) {
	out := filepath.Join(t.TempDir(), "journal.jsonl")
	opts := options{
		configPath:           configurationFixture,
		barsPath:             barsDelistingFixture,
		corporateActionsPath: corporateActionsUntradedFixture,
		outPath:              out,
		build:                testBuild,
	}

	var log bytes.Buffer
	if err := backtest(context.Background(), opts, &log); err != nil {
		t.Fatalf("backtest(%+v) error = %v\n%s", opts, err, log.String())
	}
	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}

	// The run still reaches its own campaign, opened on AAPL's breakout bar:
	// the untraded instrument's notice did not halt anything before it.
	opened := decisionsOfType(t, written, event.CampaignOpenedEventType)
	if len(opened) != 1 {
		t.Fatalf("got %d %s decisions, want 1 (the untraded notice must not halt the run)", len(opened), event.CampaignOpenedEventType)
	}

	// The notice itself produced no decision: recorded, not journalled as an
	// event, per applyDelisting's own doc comment on the unknown-instrument
	// case.
	corporateActionInputs := 0
	_, records, err := journal.Read(bytes.NewReader(written))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}
	for i, record := range records {
		if record.Kind == journal.KindInput && record.Envelope.Type == event.MarketCorporateActionEventType {
			corporateActionInputs++
			if i+1 < len(records) && records[i+1].Kind == journal.KindDecision {
				// A decision immediately following the corporate-action
				// input would have to be caused by something else in this
				// fixture; nothing else runs between inputs, so this would
				// mean the untraded notice produced a decision, which it
				// must not.
				t.Fatalf("the untraded-instrument notice at record %d was followed by a decision, which applyDelisting's unknown-instrument case must never produce", i+1)
			}
		}
	}
	if corporateActionInputs != 1 {
		t.Fatalf("got %d corporate-action inputs recorded, want 1", corporateActionInputs)
	}
}
