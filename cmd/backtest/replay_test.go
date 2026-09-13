package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// replayJournalFile is replay equivalence asked of a file, which is how every
// test here asks it: the property is about what a journal on disk can still
// prove, not about an in-memory value a test just built.
func replayJournalFile(t *testing.T, path string) (*replay.Divergence, error) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return replayEquivalence(bytes.NewReader(raw))
}

// rewriteJournal reads the journal at path, lets mutate change the header and
// the entries, and writes the result to a new file with the chain recomputed
// over the altered history — an editor with every incentive and no
// constraint. The chain is therefore always intact in what this returns, so a
// refusal from replay is replay's own and never chain verification leaking in.
func rewriteJournal(t *testing.T, path string, mutate func(header *journal.Header, entries []journal.Entry) []journal.Entry) string {
	t.Helper()

	header, records := readJournalFile(t, path)
	entries := make([]journal.Entry, 0, len(records))
	for _, record := range records {
		entries = append(entries, journal.Entry{Kind: record.Kind, Envelope: record.Envelope})
	}
	entries = mutate(&header, entries)

	rewritten := filepath.Join(t.TempDir(), "rewritten.jsonl")
	file, err := os.Create(rewritten)
	if err != nil {
		t.Fatalf("create the rewritten journal: %v", err)
	}
	if err := journal.Write(file, header, entries); err != nil {
		t.Fatalf("journal.Write() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close the rewritten journal: %v", err)
	}
	return rewritten
}

// TestTheCommittedGoldenJournalReplaysByteIdentically is this ticket's
// headline, asked of the one artifact in this repository that has to survive
// it: the committed slice-1 journal, read from disk rather than regenerated,
// replays to decisions byte-identical to the ones recorded in it.
//
// Reading the committed file rather than re-running the fixture is the whole
// point. A test that ran the backtest and replayed its output would still
// pass on a build whose rules had silently changed, because both halves would
// have changed together; only the committed bytes hold this build to what the
// platform decided when the golden was accepted.
func TestTheCommittedGoldenJournalReplaysByteIdentically(t *testing.T) {
	divergence, err := replayJournalFile(t, goldenJournal)
	if err != nil {
		t.Fatalf("replayEquivalence(%s) error = %v", goldenJournal, err)
	}
	if divergence != nil {
		t.Fatalf("the committed golden journal no longer replays: %+v", divergence)
	}

	// A journal with no decisions in it would satisfy the above vacuously.
	_, records := readJournalFile(t, goldenJournal)
	_, decisions := splitJournal(t, records)
	if len(decisions) == 0 {
		t.Fatal("the golden journal records no decisions; this test would pass vacuously")
	}
}

// TestTheGoldenJournalReplaysIdenticallyWhenItsInputsArriveAtDifferentTimes
// is arrival-time independence asked of the whole slice-1 fixture rather than
// of a fixture built to be small.
//
// internal/replay's unit fixture for this property never delivers a fill, so
// it exercises Setup evaluation, the Signal, the proposal and its expiry, and
// nothing else. The golden journal opens a Campaign, adds Units to it, sets
// and re-sets protective stops, and exits — the paths where a wall-clock read
// would do the most damage and would otherwise go unexamined by this
// property. Every input's RecordedAt is shifted by a constant; every decision
// must come back identical except its own RecordedAt, which the reducer
// deliberately copies through from the input that caused it.
func TestTheGoldenJournalReplaysIdenticallyWhenItsInputsArriveAtDifferentTimes(t *testing.T) {
	const shift = 37 * time.Minute

	header, records := readJournalFile(t, goldenJournal)
	inputs, recorded := splitJournal(t, records)

	shifted := make([]event.Envelope, len(inputs))
	for i, input := range inputs {
		input.RecordedAt = input.RecordedAt.Add(shift)
		shifted[i] = input
	}

	emitted := replayInputs(t, header, shifted)
	if len(emitted) != len(recorded) {
		t.Fatalf("the shifted inputs produced %d decision(s), want the %d recorded", len(emitted), len(recorded))
	}
	if len(emitted) == 0 {
		t.Fatal("the golden journal records no decisions; this test would pass vacuously")
	}

	for i := range recorded {
		want, got := recorded[i], emitted[i]

		// RecordedAt must move with the shift, or clearing it below would be
		// comparing two values that were never going to differ and the test
		// would not catch a reducer that ignored arrival time entirely.
		if !got.RecordedAt.Equal(want.RecordedAt.Add(shift)) {
			t.Fatalf("decision %d (%s) RecordedAt = %s, want the recorded %s shifted by %s", i, got.ID, got.RecordedAt, want.RecordedAt, shift)
		}

		want.RecordedAt, got.RecordedAt = time.Time{}, time.Time{}
		if d := replay.Equivalent([]event.Envelope{want}, []event.Envelope{got}); d != nil {
			t.Fatalf("decision %d (%s) diverged on something other than RecordedAt:\n want %+v\n got  %+v", i, got.ID, d.Want, d.Got)
		}
	}
}

// TestReplayComparesOnTheRulesVersionAndNotTheBuild is the accepting half of
// the rules-version rule: a journal written by a different build of the same
// rules replays, because the build suffix is traceability only (ADR 0016).
//
// This is the direction that is easy to get wrong in the safe-looking
// direction. A replay that compared whole strategy versions would refuse every
// journal not written by the running binary, which would make replay
// equivalence unfalsifiable in CI and useless for the cross-machine
// reproduction it exists to provide.
func TestReplayComparesOnTheRulesVersionAndNotTheBuild(t *testing.T) {
	_, path := runBacktestAs(t, "a-different-build")

	header, _ := readJournalFile(t, path)
	if !strings.HasSuffix(header.StrategyVersion, "+a-different-build") {
		t.Fatalf("header strategy version = %q, want it written by the other build", header.StrategyVersion)
	}
	_, rulesVersion, _, err := event.DecomposeStrategyVersion(header.StrategyVersion)
	if err != nil {
		t.Fatalf("DecomposeStrategyVersion(%q) error = %v", header.StrategyVersion, err)
	}
	if rulesVersion != strategy.RulesVersion {
		t.Fatalf("the fixture's rules version %q is not this build's %q; this test cannot show what it claims", rulesVersion, strategy.RulesVersion)
	}

	divergence, err := replayJournalFile(t, path)
	if err != nil {
		t.Fatalf("replayEquivalence() error = %v, want a journal of the same rules to be accepted", err)
	}
	if divergence != nil {
		t.Fatalf("a journal written by another build of the same rules diverged: %+v", divergence)
	}
}

// TestReplayRefusesAJournalWhoseRulesVersionIsNotThisBuilds is the refusing
// half. A rules-version bump is the declaration that two builds no longer
// replay each other's journals (ADR 0016), so the refusal has to come before
// any comparison: "the engine changed" and "the journal is wrong" are
// different findings, and a divergence report would assert the second.
func TestReplayRefusesAJournalWhoseRulesVersionIsNotThisBuilds(t *testing.T) {
	_, path := runBacktestTo(t)

	const otherRules = "9.9.9"
	if otherRules == strategy.RulesVersion {
		t.Fatalf("this test's stand-in rules version is now the real one (%q)", strategy.RulesVersion)
	}
	rewritten := rewriteJournal(t, path, func(header *journal.Header, entries []journal.Entry) []journal.Entry {
		strategyID, _, build, err := event.DecomposeStrategyVersion(header.StrategyVersion)
		if err != nil {
			t.Fatalf("DecomposeStrategyVersion(%q) error = %v", header.StrategyVersion, err)
		}
		header.StrategyVersion = event.ComposeStrategyVersion(strategyID, otherRules, build)
		return entries
	})

	// The chain is intact, so this is replay's own refusal.
	raw, err := os.ReadFile(rewritten)
	if err != nil {
		t.Fatalf("read the rewritten journal: %v", err)
	}
	if _, err := journal.Verify(bytes.NewReader(raw)); err != nil {
		t.Fatalf("journal.Verify() error = %v; the rewritten journal must verify or this test proves nothing about replay", err)
	}

	_, err = replayJournalFile(t, rewritten)
	if err == nil {
		t.Fatal("replayEquivalence() error = nil, want a refusal naming the rules-version mismatch")
	}
	if !strings.Contains(err.Error(), otherRules) || !strings.Contains(err.Error(), strategy.RulesVersion) {
		t.Fatalf("replayEquivalence() error = %v, want it to name both %q and %q", err, otherRules, strategy.RulesVersion)
	}
}

// TestReplayRefusesAHeaderWhoseStrategyVersionIsNotComposed: the header's
// strategy version is validated as non-empty and nothing more, so a journal
// can reach replay carrying a string this project never composed. Refusing it
// is what stops the rules-version comparison from being skipped by a value it
// cannot parse.
func TestReplayRefusesAHeaderWhoseStrategyVersionIsNotComposed(t *testing.T) {
	_, path := runBacktestTo(t)

	rewritten := rewriteJournal(t, path, func(header *journal.Header, entries []journal.Entry) []journal.Entry {
		header.StrategyVersion = "not-a-composed-strategy-version"
		return entries
	})

	_, err := replayJournalFile(t, rewritten)
	if err == nil {
		t.Fatal("replayEquivalence() error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "not-a-composed-strategy-version") {
		t.Fatalf("replayEquivalence() error = %v, want it to name the unparseable strategy version", err)
	}
}

// TestReplayRefusesAHeaderThatClaimsAConfigurationTheJournalDoesNotRecord:
// the header's configuration hash is the run's identity (ADR 0012). A replay
// that took the configuration from the input stream while ignoring the
// header's claim about it would answer a question nobody asked — and would
// report equivalence for a journal whose own two halves disagree.
func TestReplayRefusesAHeaderThatClaimsAConfigurationTheJournalDoesNotRecord(t *testing.T) {
	_, path := runBacktestTo(t)

	const claimed = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	rewritten := rewriteJournal(t, path, func(header *journal.Header, entries []journal.Entry) []journal.Entry {
		if header.ConfigurationHash == claimed {
			t.Fatal("the fixture's configuration already hashes to this test's stand-in value")
		}
		header.ConfigurationHash = claimed
		return entries
	})

	_, err := replayJournalFile(t, rewritten)
	if err == nil {
		t.Fatal("replayEquivalence() error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), claimed) {
		t.Fatalf("replayEquivalence() error = %v, want it to name the hash the header claims", err)
	}
}

// TestReplayRefusesAHeaderThatNamesADifferentStrategyFromItsConfiguration:
// the strategy id is the other half of the header's strategy version, and
// the configuration hash pins only the payload. Left unchecked, a journal
// that verifies cleanly and is internally consistent could claim one strategy
// in its header while having run another, and still report byte-identical
// replay — because nothing would ever compare the two.
func TestReplayRefusesAHeaderThatNamesADifferentStrategyFromItsConfiguration(t *testing.T) {
	_, path := runBacktestTo(t)

	const impostor = "some-other-strategy"
	rewritten := rewriteJournal(t, path, func(header *journal.Header, entries []journal.Entry) []journal.Entry {
		_, rulesVersion, build, err := event.DecomposeStrategyVersion(header.StrategyVersion)
		if err != nil {
			t.Fatalf("DecomposeStrategyVersion(%q) error = %v", header.StrategyVersion, err)
		}
		renamed := event.ComposeStrategyVersion(impostor, rulesVersion, build)
		header.StrategyVersion = renamed
		// Every record must agree with the header, or the identity check
		// catches this first and the strategy-id check is never reached.
		for i := range entries {
			entries[i].Envelope.StrategyVersion = renamed
		}
		return entries
	})

	_, err := replayJournalFile(t, rewritten)
	if err == nil {
		t.Fatal("replayEquivalence() error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), impostor) {
		t.Fatalf("replayEquivalence() error = %v, want it to name the strategy the header claims", err)
	}
}

// The accepting direction: an unaltered journal names the same strategy in
// its header and its configuration, and replays.
func TestReplayAcceptsAHeaderThatNamesItsOwnConfigurationsStrategy(t *testing.T) {
	_, path := runBacktestTo(t)

	header, records := readJournalFile(t, path)
	inputs, _ := splitJournal(t, records)
	strategyID, _, _, err := event.DecomposeStrategyVersion(header.StrategyVersion)
	if err != nil {
		t.Fatalf("DecomposeStrategyVersion(%q) error = %v", header.StrategyVersion, err)
	}
	payload, err := configurationPayloadFrom(inputs)
	if err != nil {
		t.Fatalf("configurationPayloadFrom() error = %v", err)
	}
	if strategyID != payload.StrategyID {
		t.Fatalf("the fixture's header names %q while its configuration declares %q; this test cannot show what it claims", strategyID, payload.StrategyID)
	}

	divergence, err := replayJournalFile(t, path)
	if err != nil {
		t.Fatalf("replayEquivalence() error = %v", err)
	}
	if divergence != nil {
		t.Fatalf("an unaltered journal diverged: %+v", divergence)
	}
}

// TestReplayRefusesAJournalWhoseRecordsNameAnotherRun: an input's own
// identity fields are fed to the reducer and never compared to anything, so
// without this check a record from a different run could sit in the input
// stream of a journal that verifies cleanly. Recorded decisions are already
// pinned by the byte comparison; inputs are not.
func TestReplayRefusesAJournalWhoseRecordsNameAnotherRun(t *testing.T) {
	_, path := runBacktestTo(t)

	const foreign = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	var alteredSequence uint64
	rewritten := rewriteJournal(t, path, func(header *journal.Header, entries []journal.Entry) []journal.Entry {
		for i := range entries {
			if entries[i].Kind != journal.KindInput || entries[i].Envelope.Type != event.CompletedBarEventType {
				continue
			}
			entries[i].Envelope.ConfigurationHash = foreign
			alteredSequence = uint64(i + 1)
			return entries
		}
		t.Fatal("the fixture journal records no bar input to re-attribute")
		return entries
	})

	// The chain is recomputed, so the file verifies: this is a journal whose
	// records disagree with its header, not an edited one.
	raw, err := os.ReadFile(rewritten)
	if err != nil {
		t.Fatalf("read the rewritten journal: %v", err)
	}
	if _, err := journal.Verify(bytes.NewReader(raw)); err != nil {
		t.Fatalf("journal.Verify() error = %v; the rewritten journal must verify or this test proves nothing", err)
	}

	_, err = replayJournalFile(t, rewritten)
	if err == nil {
		t.Fatal("replayEquivalence() error = nil, want a refusal naming the record from another run")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("record %d", alteredSequence)) {
		t.Fatalf("replayEquivalence() error = %v, want it to name record %d", err, alteredSequence)
	}
}

// TestReplayRefusesAJournalThatRecordsNoConfiguration: the reducer is built
// from the journal's own configuration event, so a journal without one cannot
// be replayed at all. Failing closed says that, rather than replaying under
// some default and reporting a divergence that is really a missing input.
func TestReplayRefusesAJournalThatRecordsNoConfiguration(t *testing.T) {
	_, path := runBacktestTo(t)

	rewritten := rewriteJournal(t, path, func(header *journal.Header, entries []journal.Entry) []journal.Entry {
		kept := make([]journal.Entry, 0, len(entries))
		for _, entry := range entries {
			if entry.Envelope.Type == event.ConfigurationEventType {
				continue
			}
			kept = append(kept, entry)
		}
		if len(kept) == len(entries) {
			t.Fatal("the fixture journal records no configuration event to remove")
		}
		return kept
	})

	_, err := replayJournalFile(t, rewritten)
	if err == nil {
		t.Fatal("replayEquivalence() error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "no configuration event") {
		t.Fatalf("replayEquivalence() error = %v, want it to name the missing configuration", err)
	}
}

// TestReplayRefusesAnUndecodableConfigurationPayload: the configuration
// arrives as recorded bytes, and bytes on disk can be anything. The payload
// hash is repaired alongside the payload, so the envelope is internally
// consistent and the journal writes — what is being tested is replay's own
// decode, not the envelope validation that precedes it.
func TestReplayRefusesAnUndecodableConfigurationPayload(t *testing.T) {
	_, path := runBacktestTo(t)

	rewritten := rewriteJournal(t, path, func(header *journal.Header, entries []journal.Entry) []journal.Entry {
		for i := range entries {
			if entries[i].Envelope.Type != event.ConfigurationEventType {
				continue
			}
			payload := json.RawMessage(`"a string, not a configuration"`)
			entries[i].Envelope.Payload = payload
			entries[i].Envelope.PayloadHash = event.HashPayload(payload)
			return entries
		}
		t.Fatal("the fixture journal records no configuration event to corrupt")
		return entries
	})

	_, err := replayJournalFile(t, rewritten)
	if err == nil {
		t.Fatal("replayEquivalence() error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "configuration payload") {
		t.Fatalf("replayEquivalence() error = %v, want it to name the undecodable payload", err)
	}
}

// TestReplayRefusesAJournalItCannotRead: a file that is not a journal is a
// read failure, reported as one rather than as a divergence.
func TestReplayRefusesAJournalItCannotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-journal.jsonl")
	if err := os.WriteFile(path, []byte("{\n"), 0o600); err != nil {
		t.Fatalf("write the file: %v", err)
	}

	if _, err := replayJournalFile(t, path); err == nil {
		t.Fatal("replayEquivalence() error = nil, want a read failure")
	}
}

// TestTheCommandReportsReplayEquivalence: the documented way to ask a journal
// whether it replays.
func TestTheCommandReportsReplayEquivalence(t *testing.T) {
	_, path := runBacktestTo(t)

	var out bytes.Buffer
	if err := run([]string{"-replay", path}, &out); err != nil {
		t.Fatalf("run(-replay) error = %v", err)
	}
	if !strings.Contains(out.String(), "replays byte-identically") {
		t.Fatalf("replay reported:\n%s", out.String())
	}
}

// TestTheCommandReportsTheFirstDivergingDecision: a forged decision, with the
// chain recomputed so verification is satisfied, is reported by position and
// by what each side holds there.
func TestTheCommandReportsTheFirstDivergingDecision(t *testing.T) {
	_, path := runBacktestTo(t)

	var forgedType string
	rewritten := rewriteJournal(t, path, func(header *journal.Header, entries []journal.Entry) []journal.Entry {
		for i := len(entries) - 1; i >= 0; i-- {
			if entries[i].Kind != journal.KindDecision {
				continue
			}
			entries[i].Envelope = alterRecordedQuantity(t, entries[i].Envelope, true)
			forgedType = entries[i].Envelope.Type
			return entries
		}
		t.Fatal("the fixture journal records no decision to forge")
		return entries
	})

	var out bytes.Buffer
	err := run([]string{"-replay", rewritten}, &out)
	if err == nil {
		t.Fatal("run(-replay) error = nil, want the forgery reported")
	}
	if !strings.Contains(err.Error(), "replay diverges at decision") {
		t.Fatalf("run(-replay) error = %v, want it to name the diverging position", err)
	}
	if !strings.Contains(err.Error(), forgedType) {
		t.Fatalf("run(-replay) error = %v, want it to name the %s that diverged", err, forgedType)
	}
}

// TestTheCommandReportsADecisionTheReplayNeverProduced: the streams can
// differ in length as well as in content, and a recorded decision this build
// does not emit at all has no counterpart to name.
func TestTheCommandReportsADecisionTheReplayNeverProduced(t *testing.T) {
	_, path := runBacktestTo(t)

	rewritten := rewriteJournal(t, path, func(header *journal.Header, entries []journal.Entry) []journal.Entry {
		for i := len(entries) - 1; i >= 0; i-- {
			if entries[i].Kind == journal.KindDecision {
				return append(entries, entries[i])
			}
		}
		t.Fatal("the fixture journal records no decision to duplicate")
		return entries
	})

	var out bytes.Buffer
	err := run([]string{"-replay", rewritten}, &out)
	if err == nil {
		t.Fatal("run(-replay) error = nil, want the extra decision reported")
	}
	if !strings.Contains(err.Error(), "this build now produces nothing") {
		t.Fatalf("run(-replay) error = %v, want it to report that the replay produced nothing there", err)
	}
}

// A path that names no file is an operator error, reported as one.
func TestTheCommandRefusesToReplayAMissingFile(t *testing.T) {
	var out bytes.Buffer
	err := run([]string{"-replay", filepath.Join(t.TempDir(), "absent.jsonl")}, &out)
	if err == nil {
		t.Fatal("run(-replay) error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "absent.jsonl") {
		t.Fatalf("run(-replay) error = %v, want it to name the file", err)
	}
}
