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
	barsDelistingFixture                      = "testdata/bars_delisting.json"
	barsGroupedInstrumentsFixture             = "testdata/bars_grouped_instruments.json"
	corporateActionsDelistingFixture          = "testdata/corporate_actions_delisting.json"
	corporateActionsUntradedFixture           = "testdata/corporate_actions_untraded.json"
	corporateActionsGroupedInstrumentsFixture = "testdata/corporate_actions_grouped_instruments.json"
	corporateActionsOutOfOrderFixture         = "testdata/corporate_actions_out_of_order.json"
	corporateActionsCrossInstrumentFixture    = "testdata/corporate_actions_cross_instrument_order.json"
	corporateActionsNullFixture               = "testdata/corporate_actions_null.json"
	corporateActionsEmptyFixture              = "testdata/corporate_actions_empty.json"
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

// noDecisionFollowsCorporateActionInputs checks that every recorded
// market.corporate-action input is immediately followed by no decision:
// the shape applyDelisting's own doc comment describes for a no-op
// (an already-delisted repeat, or a genuinely unknown instrument), and the
// shape every corporate-action input in these fixtures is expected to take,
// since none of them opens or closes a Campaign. It returns how many such
// inputs it found.
func noDecisionFollowsCorporateActionInputs(t *testing.T, written []byte) int {
	t.Helper()
	_, records, err := journal.Read(bytes.NewReader(written))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}
	count := 0
	for i, record := range records {
		if record.Kind != journal.KindInput || record.Envelope.Type != event.MarketCorporateActionEventType {
			continue
		}
		count++
		if i+1 < len(records) && records[i+1].Kind == journal.KindDecision {
			t.Fatalf("the corporate-action input at record %d was followed by a decision, which this fixture's no-op case must never produce", i+1)
		}
	}
	return count
}

// TestPerInstrumentInterleaveHandlesAnInstrumentGroupedBarFixture checks a
// fixture shape readBars' own contract permits: it promises only "the order
// the run delivers them", not global chronology, so a fixture that lists
// every bar of one instrument before any bar of the next is well-formed.
// Interleaving an action against a DIFFERENT instrument's bar would place it
// at the wrong point in its own instrument's history; positioning it per
// instrument (backtest.go's deliverActionsDueFor) must accept this exact
// fixture shape without error.
func TestPerInstrumentInterleaveHandlesAnInstrumentGroupedBarFixture(t *testing.T) {
	out := filepath.Join(t.TempDir(), "journal.jsonl")
	opts := options{
		configPath:           configurationFixture,
		barsPath:             barsGroupedInstrumentsFixture,
		corporateActionsPath: corporateActionsGroupedInstrumentsFixture,
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

	// Neither instrument ever opens a Campaign in this fixture, so both
	// actions — one for the instrument whose own bars come first in the
	// file, one for the instrument whose bars come second — are legitimate
	// no-ops, not errors.
	if got := noDecisionFollowsCorporateActionInputs(t, written); got != 2 {
		t.Fatalf("got %d corporate-action inputs recorded, want 2", got)
	}
}

// TestAnOutOfOrderCorporateActionsFixtureIsRefused checks readCorporateActions'
// own ordering guard: a fixture naming an earlier-effective action after a
// later one (AAPL effective after MSFT despite appearing first) is refused
// before interleaving ever sees it, naming both entries, rather than being
// delivered in file order and left for the reducer to judge.
func TestAnOutOfOrderCorporateActionsFixtureIsRefused(t *testing.T) {
	_, err := readCorporateActions(corporateActionsOutOfOrderFixture)
	if err == nil {
		t.Fatal("readCorporateActions() error = nil, want the ordering refusal")
	}
	for _, want := range []string{"AAPL", "2026-01-03", "2026-01-02"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("readCorporateActions() error = %v, want it to name %q", err, want)
		}
	}
}

// TestActionsForDifferentInstrumentsNeedNoOrderBetweenThem pins the other
// half of the ordering rule. Each action is placed against its OWN
// instrument's bars, so two naming different instruments have no order
// relative to one another, and a fixture listing a later effective time
// first is well formed. Refusing it would reject a run that would have been
// correct — the fixture below is exactly that shape.
func TestActionsForDifferentInstrumentsNeedNoOrderBetweenThem(t *testing.T) {
	actions, err := readCorporateActions(corporateActionsCrossInstrumentFixture)
	if err != nil {
		t.Fatalf("readCorporateActions() error = %v, want nil: actions naming different instruments "+
			"are each placed against their own instrument's bars and so need no order between them", err)
	}
	if len(actions) != 2 {
		t.Fatalf("readCorporateActions() returned %d actions, want 2", len(actions))
	}
	if !actions[1].EffectiveAt.Before(actions[0].EffectiveAt) {
		t.Fatal("the fixture no longer lists a later effective time before an earlier one, " +
			"so this test no longer exercises what it claims")
	}
}

// TestANullCorporateActionsFixtureIsRefused checks that a file holding JSON
// null — a nil slice once decoded — is refused rather than silently treated
// as a run declaring no corporate action, unlike an empty array.
func TestANullCorporateActionsFixtureIsRefused(t *testing.T) {
	_, err := readCorporateActions(corporateActionsNullFixture)
	if err == nil {
		t.Fatal("readCorporateActions() error = nil, want the null-fixture refusal")
	}
}

// TestAnEmptyCorporateActionsFixtureIsAcceptedAndChangesNothing checks that
// "[]" — a deliberate declaration of zero corporate actions — is accepted,
// and that a run given it is byte-for-byte the golden journal: the same
// property a run given no fixture at all has.
func TestAnEmptyCorporateActionsFixtureIsAcceptedAndChangesNothing(t *testing.T) {
	actions, err := readCorporateActions(corporateActionsEmptyFixture)
	if err != nil {
		t.Fatalf("readCorporateActions() error = %v, want [] accepted", err)
	}
	if len(actions) != 0 {
		t.Fatalf("readCorporateActions() = %v, want zero actions", actions)
	}

	out := filepath.Join(t.TempDir(), "journal.jsonl")
	opts := options{
		configPath:           configurationFixture,
		barsPath:             barsFixture,
		corporateActionsPath: corporateActionsEmptyFixture,
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
	want, err := os.ReadFile(goldenJournal)
	if err != nil {
		t.Fatalf("read the golden journal: %v", err)
	}
	if !bytes.Equal(written, want) {
		t.Fatal("a run given an empty corporate-action fixture differs from the golden journal, which was run with no fixture at all")
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
	if got := noDecisionFollowsCorporateActionInputs(t, written); got != 1 {
		t.Fatalf("got %d corporate-action inputs recorded, want 1", got)
	}
}
