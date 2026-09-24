package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// replayJournalFile checks replay equivalence from a file (ADR 0017); the
// evidence under test must be read from disk rather than supplied in memory.
func replayJournalFile(t *testing.T, path string) (*replay.Divergence, error) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return replayEquivalence(context.Background(), bytes.NewReader(raw))
}

// rewriteJournal writes an altered header and history to a new file with
// recomputed chain hashes (ADR 0017). The chain must remain valid so replay
// refusals cannot be attributed to chain verification.
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

// TestTheCommittedGoldenJournalReplaysByteIdentically checks ADR 0017's
// replay equivalence against the committed slice-1 journal read from disk.
// Regenerating the fixture here would let changed rules affect both sides
// and conceal a divergence from the previously accepted decisions.
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
// property.
//
// The arrivals are RESCHEDULED, not translated: each input's RecordedAt moves
// by its own cumulative offset, so the GAPS between consecutive arrivals all
// change too. A single constant offset preserves every gap, and so cannot
// catch a reducer reading elapsed time between events — the leak a uniform
// shift is structurally blind to. The offsets only ever increase, so the
// stream stays monotonic and remains one a real clock could have produced.
//
// Every decision must come back identical except its own RecordedAt, which
// the reducer deliberately copies through from the input that caused it.
func TestTheGoldenJournalReplaysIdenticallyWhenItsInputsArriveAtDifferentTimes(t *testing.T) {
	header, records := readJournalFile(t, goldenJournal)
	inputs, recorded := splitJournal(t, records)

	shifted := make([]event.Envelope, len(inputs))
	arrivalByInputID := make(map[string]time.Time, len(inputs))
	offset := 37 * time.Minute
	for i, input := range inputs {
		offset += time.Duration(i%7+1) * time.Minute
		input.RecordedAt = input.RecordedAt.Add(offset)
		shifted[i] = input
		arrivalByInputID[input.ID] = input.RecordedAt
	}

	// The reschedule is only a reschedule if the gaps actually moved, and the
	// stream is only realistic if it never runs backwards.
	gapsChanged := 0
	for i := 1; i < len(inputs); i++ {
		was := inputs[i].RecordedAt.Sub(inputs[i-1].RecordedAt)
		now := shifted[i].RecordedAt.Sub(shifted[i-1].RecordedAt)
		if now != was {
			gapsChanged++
		}
		if now < 0 {
			t.Fatalf("arrival %d runs backwards in the rescheduled stream (%s after %s)", i, shifted[i].RecordedAt, shifted[i-1].RecordedAt)
		}
	}
	if gapsChanged == 0 {
		t.Fatal("no gap between consecutive arrivals changed; this is a translation, not a reschedule, and an elapsed-time leak would pass")
	}

	emitted := replayInputs(t, header, shifted)
	if len(emitted) != len(recorded) {
		t.Fatalf("the rescheduled inputs produced %d decision(s), want the %d recorded", len(emitted), len(recorded))
	}
	if len(emitted) == 0 {
		t.Fatal("the golden journal records no decisions; this test would pass vacuously")
	}

	for i := range recorded {
		want, got := recorded[i], emitted[i]

		// RecordedAt must track the rescheduled arrival of the input that
		// caused this decision, or clearing it below would be comparing two
		// values that were never going to differ and the test would not catch
		// a reducer that ignored arrival time entirely.
		wantArrival, ok := arrivalByInputID[got.CausationID]
		if !ok {
			t.Fatalf("decision %d (%s) names causation %q, which is not one of the journal's inputs", i, got.ID, got.CausationID)
		}
		if !got.RecordedAt.Equal(wantArrival) {
			t.Fatalf("decision %d (%s) RecordedAt = %s, want its causing input's rescheduled arrival %s", i, got.ID, got.RecordedAt, wantArrival)
		}
		if got.RecordedAt.Equal(want.RecordedAt) {
			t.Fatalf("decision %d (%s) RecordedAt did not move with the reschedule", i, got.ID)
		}

		want.RecordedAt, got.RecordedAt = time.Time{}, time.Time{}
		if d := replay.Equivalent([]event.Envelope{want}, []event.Envelope{got}); d != nil {
			t.Fatalf("decision %d (%s) diverged on something other than RecordedAt:\n want %+v\n got  %+v", i, got.ID, d.Want, d.Got)
		}
	}
}

// TestReplayComparesOnTheRulesVersionAndNotTheBuild accepts a journal from
// a different build with the same rules (ADR 0016). Comparing the entire
// strategy version would reject cross-machine reproduction and make replay
// equivalence unfalsifiable in CI.
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

// TestReplayRefusesAJournalWhoseRulesVersionIsNotThisBuilds requires refusal
// before decision comparison on a rules-version mismatch (ADR 0016). A changed
// engine does not establish that the journal is wrong.
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

// TestReplayRefusesAHeaderThatClaimsAConfigurationTheJournalDoesNotRecord
// checks the header's run identity (ADR 0012). Replaying only the input
// configuration would falsely accept a journal whose header claims another run.
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

// TestReplayRefusesAJournalThatRecordsNoConfiguration checks that missing
// recorded configuration fails closed (ADR 0017). A default would replay a
// different run and misreport the missing input as a decision divergence.
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
	if err := run(context.Background(), []string{"-replay", path}, &out); err != nil {
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
	err := run(context.Background(), []string{"-replay", rewritten}, &out)
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
	err := run(context.Background(), []string{"-replay", rewritten}, &out)
	if err == nil {
		t.Fatal("run(-replay) error = nil, want the extra decision reported")
	}
	if !strings.Contains(err.Error(), "this build now produces nothing") {
		t.Fatalf("run(-replay) error = %v, want it to report that the replay produced nothing there", err)
	}
}

// TestAnInvocationNamingTwoOperationsIsRefused protects ADR 0017's separate
// verification and replay checks. Branch order previously allowed
// `-verify a -replay b` to exit successfully without reading b; conflicting
// operations must fail instead of silently selecting one.
func TestAnInvocationNamingTwoOperationsIsRefused(t *testing.T) {
	_, path := runBacktestTo(t)

	tests := []struct {
		name    string
		args    []string
		wantsIn string
	}{
		{
			name:    "two journal-reading modes",
			args:    []string{"-verify", path, "-replay", path},
			wantsIn: "-verify and -replay",
		},
		{
			name:    "a mode alongside the flags describing a run",
			args:    []string{"-replay", path, "-config", configurationFixture, "-bars", barsFixture},
			wantsIn: "-config, -bars",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(context.Background(), tt.args, &out)
			if err == nil {
				t.Fatalf("run(%v) error = nil, want a refusal; it performed one operation and discarded the other", tt.args)
			}
			if !strings.Contains(err.Error(), tt.wantsIn) {
				t.Fatalf("run(%v) error = %v, want it to name %q", tt.args, err, tt.wantsIn)
			}
			if out.Len() > 0 {
				t.Fatalf("run(%v) wrote a report before refusing:\n%s", tt.args, out.String())
			}
		})
	}
}

func TestEachOperationOnItsOwnIsStillAccepted(t *testing.T) {
	_, path := runBacktestTo(t)

	for _, args := range [][]string{{"-verify", path}, {"-replay", path}} {
		var out bytes.Buffer
		if err := run(context.Background(), args, &out); err != nil {
			t.Fatalf("run(%v) error = %v", args, err)
		}
		if out.Len() == 0 {
			t.Fatalf("run(%v) reported nothing", args)
		}
	}
}

func TestTheCommandRefusesToReplayAMissingFile(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"-replay", filepath.Join(t.TempDir(), "absent.jsonl")}, &out)
	if err == nil {
		t.Fatal("run(-replay) error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "absent.jsonl") {
		t.Fatalf("run(-replay) error = %v, want it to name the file", err)
	}
}

// TestReplayStopsWhenItsInvocationIsCancelled pins that -replay honours the
// command's own interrupt handling: main cancels the invocation context on
// SIGINT or SIGTERM, and a replay must stop on it rather than absorb the
// signal and keep running the reducer. A cancelled replay is reported as the
// cancellation, never as a divergence of a journal that is in fact valid.
func TestReplayStopsWhenItsInvocationIsCancelled(t *testing.T) {
	raw, err := os.ReadFile(goldenJournal)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	divergence, err := replayEquivalence(ctx, bytes.NewReader(raw))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("replayEquivalence(cancelled) error = %v, want it to wrap context.Canceled", err)
	}
	if divergence != nil {
		t.Fatalf("replayEquivalence(cancelled) divergence = %+v, want none reported for a valid journal", divergence)
	}
}

// cancellingReader cancels its context after delivering its first chunk, so a
// test can cancel an invocation part-way through reading a journal.
type cancellingReader struct {
	r      io.Reader
	cancel context.CancelFunc
	read   bool
}

func (c *cancellingReader) Read(p []byte) (int, error) {
	if c.read {
		c.cancel()
	}
	c.read = true
	if len(p) > 64 {
		p = p[:64]
	}
	return c.r.Read(p)
}

// TestReplayAndRerunStopWhenCancelledPartWayThroughReadingTheJournal pins
// that the journal scan itself honours cancellation, for both operations.
// The invocation is cancelled after the first 64 bytes of a valid journal;
// neither may report success or a divergence.
func TestReplayAndRerunStopWhenCancelledPartWayThroughReadingTheJournal(t *testing.T) {
	raw, err := os.ReadFile(goldenJournal)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	divergence, err := replayEquivalence(ctx, &cancellingReader{r: bytes.NewReader(raw), cancel: cancel})
	// "journal: read" names the phase: the scan itself stopped, rather than
	// reading the whole file and leaving a later phase to notice.
	if !errors.Is(err, context.Canceled) || divergence != nil || !strings.Contains(err.Error(), "journal: read") {
		t.Fatalf("replayEquivalence = %+v, %v; want no divergence and the journal scan stopped with context.Canceled", divergence, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	err = pipelineEquivalence(ctx, &cancellingReader{r: bytes.NewReader(raw), cancel: cancel})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "journal: read") {
		t.Fatalf("pipelineEquivalence error = %v, want the journal scan stopped with context.Canceled", err)
	}
}

// TestStoppedByReportsACancellationThatArrivesBetweenPhases pins the check
// that closes the gap after the last input is applied or during the
// comparison: a phase can finish its own work successfully while the
// invocation has since been cancelled, and that must not read as success.
func TestStoppedByReportsACancellationThatArrivesBetweenPhases(t *testing.T) {
	if err := stoppedBy(context.Background()); err != nil {
		t.Fatalf("stoppedBy(live) = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := stoppedBy(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("stoppedBy(cancelled) = %v, want it to wrap context.Canceled", err)
	}
}

// TestReplayCancelledAfterTheReducerFinishesIsNotReportedAsSuccess pins the
// final check at the caller: a valid journal whose invocation is cancelled
// after the replay has run, and before the result is returned, must not come
// back as a clean (nil, nil). replay.Equivalent has no context hook, so this
// cancels at the last point the context is consulted rather than inside the
// comparison.
//
// The threshold is calibrated rather than hard-coded: a first, uncancelled
// run counts how often the context is consulted, and the second run cancels
// on only the last of those — the final check itself.
func TestReplayCancelledAfterTheReducerFinishesIsNotReportedAsSuccess(t *testing.T) {
	raw, err := os.ReadFile(goldenJournal)
	if err != nil {
		t.Fatal(err)
	}
	var total int
	if divergence, err := replayEquivalence(stopAfter{Context: context.Background(), consulted: &total, n: 1 << 30}, bytes.NewReader(raw)); err != nil || divergence != nil {
		t.Fatalf("calibration replay = %+v, %v; want a clean replay of the golden journal", divergence, err)
	}
	var consulted int
	divergence, err := replayEquivalence(stopAfter{Context: context.Background(), consulted: &consulted, n: total - 1}, bytes.NewReader(raw))
	// "stopped before completing" is stoppedBy's own wording: the
	// cancellation must be caught by the final check, not by an earlier phase
	// that a shorter calibration would otherwise land on.
	if divergence != nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "stopped before completing") {
		t.Fatalf("replayEquivalence = %+v, %v; want no divergence and the final check reporting context.Canceled", divergence, err)
	}
}

// TestSettleReplayReportsAnEstablishedDivergenceEvenWhenCancelled pins that an
// interrupt never hides a finding: once the comparison has found a
// divergence, a cancellation must not replace it. A clean comparison under a
// cancelled invocation, by contrast, reports the cancellation.
func TestSettleReplayReportsAnEstablishedDivergenceEvenWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	found := &replay.Divergence{}
	if got, err := settleReplay(ctx, found); got != found || err != nil {
		t.Fatalf("settleReplay(cancelled, divergence) = %+v, %v; want the divergence and no error", got, err)
	}
	if got, err := settleReplay(ctx, nil); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("settleReplay(cancelled, nil) = %+v, %v; want no divergence and an error wrapping context.Canceled", got, err)
	}
	if got, err := settleReplay(context.Background(), nil); got != nil || err != nil {
		t.Fatalf("settleReplay(live, nil) = %+v, %v; want a clean result", got, err)
	}
}
