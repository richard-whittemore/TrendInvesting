package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

const (
	barsDelistingFixture                      = "testdata/bars_delisting.json"
	barsGroupedInstrumentsFixture             = "testdata/bars_grouped_instruments.json"
	corporateActionsDelistingFixture          = "testdata/corporate_actions_delisting.json"
	corporateActionsUntradedFixture           = "testdata/corporate_actions_untraded.json"
	corporateActionsGroupedInstrumentsFixture = "testdata/corporate_actions_grouped_instruments.json"
	corporateActionsUnorderedFixture          = "testdata/corporate_actions_unordered.json"
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

// TestFixtureOrderDoesNotChangeWhereAnActionLands pins the property that
// makes an ordering guard unnecessary. deliverActionsDueFor rescans every
// action before each bar and takes any whose effective time that bar has
// reached, so an action lands by its own effective time and its own
// instrument's bars — never by where it sits in the file.
//
// The fixture lists a January 20th action before a January 8th one. Running
// it, and running the same two sorted, produce byte-identical journals. A
// guard demanding sorted input would have rejected the first to prevent
// nothing.
//
// The fixture includes the case that makes this more than sorting: its last
// two actions fall between the SAME pair of bars. The reducer treats the
// first delisting it accepts as terminal and ignores a later notice for the
// same instrument, so whichever of the two arrives first decides the
// effective time the exit records. Delivering them in the order the file
// happened to list them would make that a fact about the file.
func TestFixtureOrderDoesNotChangeWhereAnActionLands(t *testing.T) {
	unordered, err := readCorporateActions(corporateActionsUnorderedFixture)
	if err != nil {
		t.Fatalf("readCorporateActions() error = %v, want nil: fixture order is not a constraint", err)
	}
	if len(unordered) != 3 || !unordered[2].EffectiveAt.Before(unordered[1].EffectiveAt) {
		t.Fatalf("the fixture no longer lists a later effective time first (%d actions), "+
			"so this test no longer exercises what it claims", len(unordered))
	}
	if !unordered[1].EffectiveAt.Truncate(24 * time.Hour).Equal(unordered[2].EffectiveAt.Truncate(24 * time.Hour)) {
		t.Fatal("the fixture's last two actions no longer fall between the same pair of daily bars, " +
			"so the same-boundary case this test exists for is no longer covered")
	}

	sortedPath := filepath.Join(t.TempDir(), "sorted.json")
	encoded, err := json.Marshal([]event.CorporateActionPayload{unordered[2], unordered[1], unordered[0]})
	if err != nil {
		t.Fatalf("encode the sorted fixture: %v", err)
	}
	if err := os.WriteFile(sortedPath, encoded, 0o600); err != nil {
		t.Fatalf("write the sorted fixture: %v", err)
	}

	if got, want := journalFor(t, corporateActionsUnorderedFixture), journalFor(t, sortedPath); !bytes.Equal(got, want) {
		t.Error("the journal differs when the same actions are listed in a different order; " +
			"placement must depend on effective time and the instrument's own bars, not on file position")
	}
}

// journalFor runs the delisting bar fixture with the corporate actions at
// path and returns the journal written.
func journalFor(t *testing.T, actionsPath string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "journal.jsonl")
	opts := options{
		configPath:           configurationFixture,
		barsPath:             barsDelistingFixture,
		corporateActionsPath: actionsPath,
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
	return written
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
