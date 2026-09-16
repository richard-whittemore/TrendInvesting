package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/buildinfo"
	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/registry"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// writeConfiguration writes cfg to a file the command can be pointed at.
func writeConfiguration(t *testing.T, dir string, cfg event.ConfigurationPayload) string {
	t.Helper()

	path := filepath.Join(dir, "configuration.json")
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write the configuration: %v", err)
	}
	return path
}

// verifyJournal is what the journal at path says about itself: the chain
// head and record count the registry anchors (ADR 0017).
func verifyJournal(t *testing.T, path string) journal.Verification {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the journal: %v", err)
	}
	defer func() { _ = file.Close() }()

	verification, err := journal.Verify(file)
	if err != nil {
		t.Fatalf("journal.Verify() error = %v", err)
	}
	return verification
}

// runsUnder is every run the registry at root holds for cfg.
func runsUnder(t *testing.T, root string, cfg event.ConfigurationPayload) []registry.Entry {
	t.Helper()

	found, err := registry.Runs(registryStore{root: root}, event.ConfigurationHash(cfg))
	if err != nil {
		t.Fatalf("registry.Runs() error = %v", err)
	}
	return found
}

// TestARunIsRecordedUnderItsConfigurationHash is #39's headline: the command
// that produces a result is the command that records it, so the record cannot
// be the part someone forgets.
func TestARunIsRecordedUnderItsConfigurationHash(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "runs")
	journalPath := filepath.Join(dir, "journal.jsonl")

	var log bytes.Buffer
	err := run(context.Background(), []string{
		"-config", configurationFixture,
		"-bars", barsFixture,
		"-out", journalPath,
		"-registry", root,
		"-run-id", "baseline-2026-09-13",
	}, &log)
	if err != nil {
		t.Fatalf("run() error = %v\n%s", err, log.String())
	}

	cfg := fixtureConfiguration(t)
	found := runsUnder(t, root, cfg)
	if len(found) != 1 {
		t.Fatalf("the registry holds %d runs, want the one just performed", len(found))
	}
	entry := found[0]

	if entry.RunID != "baseline-2026-09-13" {
		t.Errorf("run id = %q, want the one the invocation declared", entry.RunID)
	}
	if entry.Status != registry.StatusCompleted {
		t.Errorf("status = %q, want %q", entry.Status, registry.StatusCompleted)
	}
	if entry.Variant != registry.Baseline {
		t.Errorf("variant = %q, want %q by default", entry.Variant, registry.Baseline)
	}
	if want := "+" + buildinfo.Version; !strings.HasSuffix(entry.StrategyVersion, want) {
		t.Errorf("strategy version = %q, want it to end with %q", entry.StrategyVersion, want)
	}
	if entry.Configuration != cfg {
		t.Errorf("the recorded configuration differs from the one run:\n%+v\n%+v", entry.Configuration, cfg)
	}
	if entry.SpanStart.IsZero() || entry.SpanEnd.IsZero() {
		t.Errorf("span = %s..%s, want the span the run covered", entry.SpanStart, entry.SpanEnd)
	}

	// The journal's own chain head, anchored outside the journal (ADR 0017).
	verification := verifyJournal(t, journalPath)
	if entry.Artefacts.JournalPath == "" {
		t.Fatal("the entry records no journal")
	}
	if entry.Artefacts.FinalRecordHash != verification.FinalRecordHash {
		t.Errorf("recorded final record hash = %q, want the journal's own %q", entry.Artefacts.FinalRecordHash, verification.FinalRecordHash)
	}
	if entry.Artefacts.RecordCount != verification.RecordCount {
		t.Errorf("recorded record count = %d, want the journal's own %d", entry.Artefacts.RecordCount, verification.RecordCount)
	}
}

// TestARunLocatedByItsConfigurationHashReplays closes ADR 0012's loop: a
// configuration hash finds the run, and the run names the journal that
// `-replay` then reproduces byte for byte.
func TestARunLocatedByItsConfigurationHashReplays(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "runs")

	var log bytes.Buffer
	err := run(context.Background(), []string{
		"-config", configurationFixture,
		"-bars", barsFixture,
		"-out", filepath.Join(dir, "journal.jsonl"),
		"-registry", root,
		"-run-id", "replayable",
	}, &log)
	if err != nil {
		t.Fatalf("run() error = %v\n%s", err, log.String())
	}

	found := runsUnder(t, root, fixtureConfiguration(t))
	if len(found) != 1 {
		t.Fatalf("the registry holds %d runs, want one", len(found))
	}

	// The journal path is recorded relative to the registry root, so a caller
	// resolves it against the root it read the entry from.
	journalPath := filepath.Join(root, filepath.FromSlash(found[0].Artefacts.JournalPath))
	if filepath.IsAbs(found[0].Artefacts.JournalPath) {
		t.Fatalf("the entry records journal %q, an absolute path that means nothing on another machine", found[0].Artefacts.JournalPath)
	}

	var replayLog bytes.Buffer
	if err := run(context.Background(), []string{"-replay", journalPath}, &replayLog); err != nil {
		t.Fatalf("replaying the located run: %v\n%s", err, replayLog.String())
	}
	if !strings.Contains(replayLog.String(), "replays byte-identically") {
		t.Fatalf("replay reported %q", replayLog.String())
	}
}

// TestAFailedRunIsRecordedWithItsPartialEvidence: the run halts part way
// through, and both the failure and the journal it left behind are retained.
// A registry that only recorded runs that finished would be the curated
// record ADR 0012 exists to prevent.
func TestAFailedRunIsRecordedWithItsPartialEvidence(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "runs")
	journalPath := filepath.Join(dir, "journal.jsonl")

	// A Notional Account large enough that the first Unit sized against it
	// exceeds the exactly representable range, which halts the run at the
	// first Breakout rather than at the first bar.
	cfg := fixtureConfiguration(t)
	cfg.NotionalAccount.StartingEquity = 1e21
	configPath := writeConfiguration(t, dir, cfg)

	var log bytes.Buffer
	err := run(context.Background(), []string{
		"-config", configPath,
		"-bars", barsFixture,
		"-out", journalPath,
		"-registry", root,
		"-run-id", "halted",
	}, &log)
	if err == nil {
		t.Fatal("run() error = nil, want the run to have failed")
	}

	found := runsUnder(t, root, cfg)
	if len(found) != 1 {
		t.Fatalf("the registry holds %d runs, want the failed one retained", len(found))
	}
	entry := found[0]

	if entry.Status != registry.StatusFailed {
		t.Errorf("status = %q, want %q", entry.Status, registry.StatusFailed)
	}
	if entry.Detail == "" {
		t.Error("the failed run records no reason; a status with no reason is not evidence of anything")
	}
	if entry.Artefacts.JournalPath == "" {
		t.Error("the failed run records no journal; the partial journal is exactly what a reviewer reads")
	}
	if _, statErr := os.Stat(journalPath); statErr != nil {
		t.Errorf("os.Stat(%s) error = %v, want the partial journal to have been written", journalPath, statErr)
	}
}

// TestARunWhoseJournalCouldNotBeWrittenIsStillRecorded: the run itself
// finished and its evidence did not land. Recording nothing would leave the
// registry claiming the run never happened, so it is recorded as failed, with
// no artefacts and the reason its journal is missing.
func TestARunWhoseJournalCouldNotBeWrittenIsStillRecorded(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "runs")

	var log bytes.Buffer
	err := backtest(context.Background(), options{
		configPath:   configurationFixture,
		barsPath:     barsFixture,
		outPath:      filepath.Join(dir, "no-such-directory", "journal.jsonl"),
		registryPath: root,
		runID:        "journal-lost",
		variant:      registry.Baseline,
		build:        testBuild,
	}, &log)
	if err == nil {
		t.Fatal("backtest() error = nil, want the journal it could not write to be reported")
	}

	found := runsUnder(t, root, fixtureConfiguration(t))
	if len(found) != 1 {
		t.Fatalf("the registry holds %d runs, want the run recorded regardless", len(found))
	}
	if found[0].Status != registry.StatusFailed {
		t.Errorf("status = %q, want %q", found[0].Status, registry.StatusFailed)
	}
	if found[0].Artefacts.JournalPath != "" {
		t.Errorf("the entry names journal %q, which was never written", found[0].Artefacts.JournalPath)
	}
	if !strings.Contains(found[0].Detail, "create the journal") {
		t.Errorf("detail = %q, want it to say why no journal survives the run", found[0].Detail)
	}
}

// TestARunThatLostTheJournalRaceClaimsNoEvidence is the failure mode that
// corrupts the audit trail rather than merely losing from it.
//
// The run finished, and its journal lost the hard-link race to a concurrent
// run of the same configuration. A complete, valid, VERIFIABLE journal is
// therefore sitting at this run's own destination — and it belongs to the
// other run. Recording it here would produce an entry asserting that this run
// produced evidence it did not produce, anchored to another run's chain head.
// The header cannot tell them apart: two runs of one configuration have
// identical headers, and a header carries no run id. So the only safe answer
// is to record no artefacts at all.
func TestARunThatLostTheJournalRaceClaimsNoEvidence(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "runs")

	// The winner's journal: a real one, written by a run of the same
	// configuration, so it verifies perfectly and names the same header.
	winner := filepath.Join(dir, "winner.jsonl")
	var log bytes.Buffer
	if err := backtest(context.Background(), options{configPath: configurationFixture, barsPath: barsFixture, outPath: winner, build: testBuild}, &log); err != nil {
		t.Fatalf("write the winning journal: %v", err)
	}
	winning := verifyJournal(t, winner)

	cfg := fixtureConfiguration(t)
	header := journal.NewHeader(event.ConfigurationHash(cfg), winning.Header.StrategyVersion, winning.Header.SpanStart, winning.Header.SpanEnd)

	// The loser: the same run, whose own link to that destination failed.
	err := registerRun(options{
		outPath:      winner,
		registryPath: root,
		runID:        "lost-the-race",
		variant:      registry.Baseline,
		build:        testBuild,
	}, cfg, winning.Header.StrategyVersion, outcome{
		header:     header,
		journalErr: journalExistsError(winner),
	})
	if err != nil {
		t.Fatalf("registerRun() error = %v, want the losing run recorded", err)
	}

	found := runsUnder(t, root, cfg)
	if len(found) != 1 {
		t.Fatalf("the registry holds %d runs, want the losing run recorded", len(found))
	}
	entry := found[0]

	if entry.Status != registry.StatusFailed {
		t.Errorf("status = %q, want %q", entry.Status, registry.StatusFailed)
	}
	if entry.Artefacts != (registry.Artefacts{}) {
		t.Fatalf("the losing run claims artefacts %+v; the journal at its destination is another run's", entry.Artefacts)
	}
	if entry.Artefacts.FinalRecordHash == winning.FinalRecordHash {
		t.Fatalf("the losing run anchored the winner's chain head %q as its own", winning.FinalRecordHash)
	}
}

// TestARecordedJournalPathIsRelativeToTheRegistryRoot: the registry is
// committed to git and read wherever it is cloned, so an absolute path in it
// names a location that exists on exactly one machine.
func TestARecordedJournalPathIsRelativeToTheRegistryRoot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "runs")
	journalPath := filepath.Join(dir, "journal.jsonl")

	var log bytes.Buffer
	err := run(context.Background(), []string{
		"-config", configurationFixture,
		"-bars", barsFixture,
		"-out", journalPath,
		"-registry", root,
		"-run-id", "portable",
	}, &log)
	if err != nil {
		t.Fatalf("run() error = %v\n%s", err, log.String())
	}

	found := runsUnder(t, root, fixtureConfiguration(t))
	if len(found) != 1 {
		t.Fatalf("the registry holds %d runs, want one", len(found))
	}
	recorded := found[0].Artefacts.JournalPath

	if filepath.IsAbs(recorded) || strings.Contains(recorded, dir) {
		t.Fatalf("recorded journal path = %q, want one relative to the registry root", recorded)
	}
	if resolved := filepath.Join(root, filepath.FromSlash(recorded)); resolved != journalPath {
		t.Fatalf("the recorded path resolves to %q, want %q", resolved, journalPath)
	}
}

// TestAJournalThatCannotBeRecordedRelativeToTheRegistryIsRefused: one
// absolute and one relative path cannot be expressed relative to one another
// at all, and recording the absolute one anyway is what this refusal exists
// to prevent.
func TestAJournalThatCannotBeRecordedRelativeToTheRegistryIsRefused(t *testing.T) {
	dir := t.TempDir()

	var log bytes.Buffer
	err := backtest(context.Background(), options{
		configPath:   configurationFixture,
		barsPath:     barsFixture,
		outPath:      filepath.Join(dir, "journal.jsonl"),
		registryPath: "runs",
		runID:        "unexpressible",
		variant:      registry.Baseline,
		build:        testBuild,
	}, &log)
	if err == nil {
		t.Fatal("backtest() error = nil, want the path that cannot be recorded portably to be refused")
	}
	if !strings.Contains(err.Error(), "relative") {
		t.Errorf("backtest() error = %v, want it to say what it could not express", err)
	}
}

// TestAZeroSlippageRunIsRefusedAndNothingIsRegistered: ADR 0013 makes the run
// invalid by construction, so it is refused before it happens — not recorded
// as a failure, which would imply it was performed.
func TestAZeroSlippageRunIsRefusedAndNothingIsRegistered(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "runs")

	cfg := fixtureConfiguration(t)
	cfg.SlippageN = 0
	configPath := writeConfiguration(t, dir, cfg)

	var log bytes.Buffer
	err := run(context.Background(), []string{
		"-config", configPath,
		"-bars", barsFixture,
		"-out", filepath.Join(dir, "journal.jsonl"),
		"-registry", root,
		"-run-id", "zero-slippage",
	}, &log)
	if err == nil {
		t.Fatal("run() error = nil, want a zero-slippage run to be refused")
	}
	if !strings.Contains(err.Error(), "ADR 0013") {
		t.Errorf("run() error = %v, want it to cite the rule", err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Errorf("os.Stat(%s) = %v, want nothing to have been registered at all", root, statErr)
	}
}

// TestConcurrentRunsAreEachRecorded is the concurrency question the registry
// answers by its layout: one file per run under a directory named for the
// configuration. Two runs never write the same path, so neither clobbers the
// other and neither waits for the other.
func TestConcurrentRunsAreEachRecorded(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "runs")

	const runs = 8
	var wait sync.WaitGroup
	errs := make([]error, runs)
	for i := range runs {
		wait.Add(1)
		go func() {
			defer wait.Done()
			var log bytes.Buffer
			errs[i] = backtest(context.Background(), options{
				configPath:   configurationFixture,
				barsPath:     barsFixture,
				outPath:      filepath.Join(dir, fmt.Sprintf("journal-%d.jsonl", i)),
				registryPath: root,
				runID:        fmt.Sprintf("concurrent-%d", i),
				variant:      registry.Baseline,
				build:        testBuild,
			}, &log)
		}()
	}
	wait.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent run %d: %v", i, err)
		}
	}

	found := runsUnder(t, root, fixtureConfiguration(t))
	if len(found) != runs {
		t.Fatalf("the registry holds %d runs, want all %d recorded", len(found), runs)
	}
	seen := map[string]bool{}
	for _, entry := range found {
		if seen[entry.RunID] {
			t.Errorf("run %q was recorded twice", entry.RunID)
		}
		seen[entry.RunID] = true
	}
}

// TestTwoRunsClaimingOneRunIDLeaveExactlyOneEntry: same configuration, same
// run id, at the same moment. Nothing in the registry is ever overwritten
// (AGENTS.md rule 6), so the second is refused rather than replacing the
// first — and the refusal is the exclusive create itself, not a check
// something can race past.
func TestTwoRunsClaimingOneRunIDLeaveExactlyOneEntry(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "runs")

	const attempts = 6
	var wait sync.WaitGroup
	errs := make([]error, attempts)
	for i := range attempts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			var log bytes.Buffer
			errs[i] = backtest(context.Background(), options{
				configPath:   configurationFixture,
				barsPath:     barsFixture,
				outPath:      filepath.Join(dir, fmt.Sprintf("journal-%d.jsonl", i)),
				registryPath: root,
				runID:        "contested",
				variant:      registry.Baseline,
				build:        testBuild,
			}, &log)
		}()
	}
	wait.Wait()

	var succeeded int
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case !strings.Contains(err.Error(), "contested"):
			t.Errorf("attempt %d failed without naming the run it lost: %v", i, err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d of %d attempts recorded the run, want exactly one", succeeded, attempts)
	}

	found := runsUnder(t, root, fixtureConfiguration(t))
	if len(found) != 1 {
		t.Fatalf("the registry holds %d runs, want exactly one", len(found))
	}
}

// TestARecordedRunIsNeverOverwritten, even long after the fact: re-running
// under an id the registry already holds is refused, so a result cannot be
// quietly replaced by a later, better-looking one.
func TestARecordedRunIsNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "runs")

	record := func(journal string) error {
		var log bytes.Buffer
		return backtest(context.Background(), options{
			configPath:   configurationFixture,
			barsPath:     barsFixture,
			outPath:      filepath.Join(dir, journal),
			registryPath: root,
			runID:        "once-only",
			variant:      registry.Baseline,
			build:        testBuild,
		}, &log)
	}

	if err := record("first.jsonl"); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	err := record("second.jsonl")
	if err == nil {
		t.Fatal("the second run error = nil, want the recorded run to be left alone")
	}
	if !strings.Contains(err.Error(), "once-only") {
		t.Errorf("the second run error = %v, want it to name the run already recorded", err)
	}

	if found := runsUnder(t, root, fixtureConfiguration(t)); len(found) != 1 {
		t.Fatalf("the registry holds %d runs, want the original one only", len(found))
	}
}

// TestTheCommandListsTheRunsRecordedUnderAConfigurationHash: an operator with
// a hash gets the journals to replay, without reading the layout by hand.
func TestTheCommandListsTheRunsRecordedUnderAConfigurationHash(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "runs")
	journalPath := filepath.Join(dir, "journal.jsonl")

	var log bytes.Buffer
	err := run(context.Background(), []string{
		"-config", configurationFixture,
		"-bars", barsFixture,
		"-out", journalPath,
		"-registry", root,
		"-run-id", "listed",
	}, &log)
	if err != nil {
		t.Fatalf("run() error = %v\n%s", err, log.String())
	}

	var listing bytes.Buffer
	hash := event.ConfigurationHash(fixtureConfiguration(t))
	if err := run(context.Background(), []string{"-registry", root, "-runs", hash}, &listing); err != nil {
		t.Fatalf("run(-runs) error = %v\n%s", err, listing.String())
	}
	for _, want := range []string{"listed", string(registry.StatusCompleted), journalPath} {
		if !strings.Contains(listing.String(), want) {
			t.Errorf("the listing does not mention %q:\n%s", want, listing.String())
		}
	}
}

// TestAnInvocationThatCannotBeRecordedIsRefused. Each of these would
// otherwise perform a run and record it under a name nobody chose, or read a
// registry that was never named.
func TestAnInvocationThatCannotBeRecordedIsRefused(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name string
		args []string
	}{
		{
			name: "a registry with no run id",
			args: []string{"-config", configurationFixture, "-bars", barsFixture, "-out", filepath.Join(dir, "a.jsonl"), "-registry", filepath.Join(dir, "runs")},
		},
		{
			name: "a run id with no registry",
			args: []string{"-config", configurationFixture, "-bars", barsFixture, "-out", filepath.Join(dir, "b.jsonl"), "-run-id", "orphan"},
		},
		{
			name: "listing runs with no registry",
			args: []string{"-runs", "sha256:0"},
		},
		{
			name: "listing runs and performing one",
			args: []string{"-registry", filepath.Join(dir, "runs"), "-runs", "sha256:0", "-config", configurationFixture},
		},
		{
			name: "a registry alongside a journal to verify",
			args: []string{"-verify", goldenJournal, "-registry", filepath.Join(dir, "runs")},
		},
		{
			name: "a registry alongside a journal to replay",
			args: []string{"-replay", goldenJournal, "-registry", filepath.Join(dir, "runs")},
		},
		// A declared Variant that nothing records is the failure that matters
		// most for a registry whose point is that the graveyard of failed
		// Variants survives: the operator believes they declared one.
		{
			name: "a Variant alongside a journal to verify",
			args: []string{"-verify", goldenJournal, "-variant", "recompute-n-per-add"},
		},
		{
			name: "a Variant alongside a listing of recorded runs",
			args: []string{"-registry", filepath.Join(dir, "runs"), "-runs", "sha256:0", "-variant", "recompute-n-per-add"},
		},
		{
			name: "a Variant on a run with no registry",
			args: []string{"-config", configurationFixture, "-bars", barsFixture, "-out", filepath.Join(dir, "c.jsonl"), "-variant", "recompute-n-per-add"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var log bytes.Buffer
			if err := run(context.Background(), test.args, &log); err == nil {
				t.Fatalf("run(%v) error = nil, want the invocation refused", test.args)
			}
		})
	}
}

// TestARunUnderADeclaredVariantIsRecordedAsThatVariant: a Variant's results
// and the Baseline's are distinguishable in the record, which is the whole
// point of keeping the graveyard (ADR 0012).
func TestARunUnderADeclaredVariantIsRecordedAsThatVariant(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "runs")

	var log bytes.Buffer
	err := run(context.Background(), []string{
		"-config", configurationFixture,
		"-bars", barsFixture,
		"-out", filepath.Join(dir, "journal.jsonl"),
		"-registry", root,
		"-run-id", "variant-run",
		"-variant", "recompute-n-per-add",
	}, &log)
	if err != nil {
		t.Fatalf("run() error = %v\n%s", err, log.String())
	}

	found := runsUnder(t, root, fixtureConfiguration(t))
	if len(found) != 1 {
		t.Fatalf("the registry holds %d runs, want one", len(found))
	}
	if found[0].Variant != "recompute-n-per-add" {
		t.Fatalf("variant = %q, want the declared one", found[0].Variant)
	}
}

// entryFor is a valid entry for the fixture configuration, recorded under
// runID and pointing at journalPath. Two entries differing only in their
// journal path are two different runs claiming one id.
func entryFor(t *testing.T, runID, journalPath string) registry.Entry {
	t.Helper()

	cfg := fixtureConfiguration(t)
	span := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	entry, err := registry.NewEntry(registry.Run{
		RunID:           runID,
		Variant:         registry.Baseline,
		Status:          registry.StatusCompleted,
		StrategyVersion: event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, testBuild),
		SpanStart:       span,
		SpanEnd:         span,
		Configuration:   cfg,
		Artefacts: registry.Artefacts{
			JournalPath:     journalPath,
			RecordCount:     1,
			FinalRecordHash: "sha256:" + strings.Repeat("0", 64),
		},
	})
	if err != nil {
		t.Fatalf("registry.NewEntry() error = %v", err)
	}
	return entry
}

// entryPath is the file entry occupies in the registry rooted at root.
func entryPath(t *testing.T, root string, entry registry.Entry) string {
	t.Helper()

	relative, err := entry.Path()
	if err != nil {
		t.Fatalf("Entry.Path() error = %v", err)
	}
	return filepath.Join(root, filepath.FromSlash(relative))
}

// TestAnEntryThatCouldNotBeFlushedIsReportedAsRecorded. The hard link is the
// commit point and the flush after it is a second, separate fact, so a flush
// that fails leaves the entry installed. Reporting only "registration failed"
// would tell the operator the opposite of the truth about a record that is
// sitting there, and send them to record the same run under another id —
// which would put two entries in the registry for one run.
func TestAnEntryThatCouldNotBeFlushedIsReportedAsRecorded(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "runs")

	// Writable and traversable but not readable: the entry links into the
	// configuration-hash directory below it, and the flush of the root's own
	// handle cannot open it.
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("create the registry root: %v", err)
	}
	if err := os.Chmod(root, 0o300); err != nil {
		t.Fatalf("chmod the registry root: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o750) })
	if file, err := os.Open(root); err == nil {
		_ = file.Close()
		t.Skip("this user reads a directory it has no read permission on, so the flush cannot be made to fail here")
	}

	var log bytes.Buffer
	err := backtest(context.Background(), options{
		configPath:   configurationFixture,
		barsPath:     barsFixture,
		outPath:      filepath.Join(dir, "journal.jsonl"),
		registryPath: root,
		runID:        "unflushed",
		variant:      registry.Baseline,
		build:        testBuild,
	}, &log)
	if err == nil {
		t.Fatal("backtest() error = nil, want the flush that failed to be reported")
	}
	for _, want := range []string{"unflushed", "recorded", root} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("backtest() error = %v, want it to mention %q", err, want)
		}
	}

	// The state the message claims: the entry is there and readable.
	found := runsUnder(t, root, fixtureConfiguration(t))
	if len(found) != 1 {
		t.Fatalf("the registry holds %d runs, want the entry the link installed", len(found))
	}
	if found[0].RunID != "unflushed" {
		t.Errorf("run id = %q, want the one the invocation declared", found[0].RunID)
	}
}

// TestReRecordingTheIdenticalEntryIsNotAnOverwrite: a run id the registry
// already holds with byte-identical content is the same record, so recording
// it again writes nothing and is not refused. Nothing on disk changes, which
// is what keeps AGENTS.md rule 6 intact.
func TestReRecordingTheIdenticalEntryIsNotAnOverwrite(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	entry := entryFor(t, "recorded-twice", "journal.jsonl")

	if err := installEntry(root, entry); err != nil {
		t.Fatalf("installEntry() error = %v", err)
	}
	destination := entryPath(t, root, entry)
	before, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read the recorded entry: %v", err)
	}

	if err := installEntry(root, entry); err != nil {
		t.Fatalf("installEntry() error = %v, want the identical entry already recorded to be accepted", err)
	}

	after, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read the recorded entry: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the recorded entry was rewritten:\n%s\n%s", before, after)
	}
	if found := runsUnder(t, root, fixtureConfiguration(t)); len(found) != 1 {
		t.Fatalf("the registry holds %d runs, want the one record", len(found))
	}

	names, err := os.ReadDir(filepath.Dir(destination))
	if err != nil {
		t.Fatalf("os.ReadDir() error = %v", err)
	}
	if len(names) != 1 {
		t.Errorf("the configuration's directory holds %d files, want the entry alone", len(names))
	}
}

// TestARunIDHoldingADifferentRunIsRefused: identical content is the same
// record, and anything else is a genuine collision. The recorded run is
// evidence and is left exactly as it was (AGENTS.md rule 6).
func TestARunIDHoldingADifferentRunIsRefused(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	first := entryFor(t, "one-id-two-runs", "first.jsonl")
	second := entryFor(t, "one-id-two-runs", "second.jsonl")

	if err := installEntry(root, first); err != nil {
		t.Fatalf("installEntry() error = %v", err)
	}
	destination := entryPath(t, root, first)
	before, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read the recorded entry: %v", err)
	}

	err = installEntry(root, second)
	if err == nil {
		t.Fatal("installEntry() error = nil, want the recorded run left alone")
	}
	if !strings.Contains(err.Error(), "one-id-two-runs") {
		t.Errorf("installEntry() error = %v, want it to name the run already recorded", err)
	}

	after, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read the recorded entry: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the recorded entry was replaced by the second run:\n%s\n%s", before, after)
	}
	found := runsUnder(t, root, fixtureConfiguration(t))
	if len(found) != 1 {
		t.Fatalf("the registry holds %d runs, want the original one only", len(found))
	}
	if found[0].Artefacts.JournalPath != "first.jsonl" {
		t.Errorf("recorded journal = %q, want the first run's", found[0].Artefacts.JournalPath)
	}
}

// TestARunIsNotRecordedWhenNoRegistryIsNamed: the registry is where results
// are kept, and pointing at one is the operator's decision. The zero-slippage
// refusal therefore does not live here alone — see readConfiguration.
func TestARunIsNotRecordedWhenNoRegistryIsNamed(t *testing.T) {
	dir := t.TempDir()

	var log bytes.Buffer
	err := run(context.Background(), []string{
		"-config", configurationFixture,
		"-bars", barsFixture,
		"-out", filepath.Join(dir, "journal.jsonl"),
	}, &log)
	if err != nil {
		t.Fatalf("run() error = %v\n%s", err, log.String())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("os.ReadDir() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the run left %d files behind, want only its journal", len(entries))
	}
}

// TestARunThatFailedAfterItsConfigurationWasAcceptedIsRecorded. The registry's
// promise is that every run is recorded, successful or not (ADR 0012), and a
// failure between accepting the configuration and finishing the run used to
// return before anything was written — so an unreadable bar fixture under a
// perfectly valid configuration left no entry at all, which is the shape a
// curated record takes.
func TestARunThatFailedAfterItsConfigurationWasAcceptedIsRecorded(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "runs")

	var log bytes.Buffer
	err := run(context.Background(), []string{
		"-config", configurationFixture,
		"-bars", filepath.Join(dir, "no-such-bars.json"),
		"-out", filepath.Join(dir, "journal.jsonl"),
		"-registry", root,
		"-run-id", "bars-unreadable",
	}, &log)
	if err == nil {
		t.Fatal("run() error = nil, want the bars it could not read to be reported")
	}

	found := runsUnder(t, root, fixtureConfiguration(t))
	if len(found) != 1 {
		t.Fatalf("the registry holds %d runs, want the run that failed before its first bar", len(found))
	}
	entry := found[0]

	if entry.Status != registry.StatusFailed {
		t.Errorf("status = %q, want %q", entry.Status, registry.StatusFailed)
	}
	if !strings.Contains(entry.Detail, "read the bars") {
		t.Errorf("detail = %q, want it to say why the run never started", entry.Detail)
	}
	if entry.Artefacts != (registry.Artefacts{}) {
		t.Errorf("the run claims artefacts %+v; it processed no input at all", entry.Artefacts)
	}
	if !entry.SpanStart.IsZero() || !entry.SpanEnd.IsZero() {
		t.Errorf("span = %s..%s, want none: the run covered no input", entry.SpanStart, entry.SpanEnd)
	}
}

// TestARunRefusedBeforeItsConfigurationWasAcceptedIsNotRecorded draws the
// other side of that line. Registration begins once the configuration has
// been read and accepted; a refusal before that produced nothing, and an
// entry for it would assert that a run was performed.
//
// Two refusals sit there: the zero-slippage one, which ADR 0013 makes invalid
// by construction (TestAZeroSlippageRunIsRefusedAndNothingIsRegistered), and
// an occupied journal path, which is refused before the configuration is even
// read.
func TestARunRefusedBeforeItsConfigurationWasAcceptedIsNotRecorded(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "runs")
	journalPath := filepath.Join(dir, "journal.jsonl")
	if err := os.WriteFile(journalPath, []byte("an earlier run's evidence\n"), 0o600); err != nil {
		t.Fatalf("write the journal already there: %v", err)
	}

	var log bytes.Buffer
	err := run(context.Background(), []string{
		"-config", configurationFixture,
		"-bars", barsFixture,
		"-out", journalPath,
		"-registry", root,
		"-run-id", "path-taken",
	}, &log)
	if err == nil {
		t.Fatal("run() error = nil, want the occupied journal path to be refused")
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Errorf("os.Stat(%s) = %v, want nothing to have been registered at all", root, statErr)
	}
}

// stopAfter is a context that reports itself cancelled once it has been
// consulted more than n times.
//
// A run consults its context once per input event (fills.Simulator checks it
// before each delivery), so this stops a run at a fixed point in its own
// stream rather than at a moment the scheduler chooses — which is what makes
// the partial journal an interrupted run leaves behind assertable at all.
type stopAfter struct {
	context.Context
	consulted *int
	n         int
}

func (s stopAfter) Err() error {
	*s.consulted++
	if *s.consulted > s.n {
		return context.Canceled
	}
	return nil
}

// TestAnInterruptedRunIsRecordedAsAbandoned. An operator who stops a run has
// deliberately not carried it through, which is what StatusAbandoned records,
// and ADR 0012 retains it beside the completed ones — the acceptance
// criterion is that failed AND abandoned runs survive. The interrupt cancels
// the run rather than ending the process, so whatever the run did produce is
// installed and anchored as its own.
func TestAnInterruptedRunIsRecordedAsAbandoned(t *testing.T) {
	t.Run("part way through its stream, keeping what it produced", func(t *testing.T) {
		dir := t.TempDir()
		root := filepath.Join(dir, "runs")
		journalPath := filepath.Join(dir, "journal.jsonl")

		consulted := 0
		ctx := stopAfter{Context: context.Background(), consulted: &consulted, n: 12}

		var log bytes.Buffer
		err := run(ctx, []string{
			"-config", configurationFixture,
			"-bars", barsFixture,
			"-out", journalPath,
			"-registry", root,
			"-run-id", "stopped-part-way",
		}, &log)
		if err == nil {
			t.Fatal("run() error = nil, want the interruption to be reported")
		}

		entry := onlyRun(t, root)
		if entry.Status != registry.StatusAbandoned {
			t.Fatalf("status = %q, want %q", entry.Status, registry.StatusAbandoned)
		}
		if entry.Detail == "" {
			t.Error("the abandoned run records no reason; a status with no reason is not evidence of anything")
		}

		// What it did produce, anchored as its own (ADR 0017).
		if entry.Artefacts.JournalPath == "" {
			t.Fatal("the abandoned run records no journal; the partial one is exactly what a reviewer reads")
		}
		verification := verifyJournal(t, journalPath)
		if entry.Artefacts.FinalRecordHash != verification.FinalRecordHash {
			t.Errorf("recorded final record hash = %q, want the partial journal's own %q", entry.Artefacts.FinalRecordHash, verification.FinalRecordHash)
		}
		if complete := verifyJournal(t, mustCompleteJournal(t)); verification.RecordCount >= complete.RecordCount {
			t.Errorf("the abandoned run recorded %d records, want fewer than the %d a complete run writes", verification.RecordCount, complete.RecordCount)
		}
	})

	t.Run("before its first input, with nothing to show for it", func(t *testing.T) {
		dir := t.TempDir()
		root := filepath.Join(dir, "runs")

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		var log bytes.Buffer
		err := run(ctx, []string{
			"-config", configurationFixture,
			"-bars", barsFixture,
			"-out", filepath.Join(dir, "journal.jsonl"),
			"-registry", root,
			"-run-id", "stopped-at-once",
		}, &log)
		if err == nil {
			t.Fatal("run() error = nil, want the interruption to be reported")
		}

		entry := onlyRun(t, root)
		if entry.Status != registry.StatusAbandoned {
			t.Fatalf("status = %q, want %q", entry.Status, registry.StatusAbandoned)
		}
		if entry.Artefacts != (registry.Artefacts{}) {
			t.Errorf("the run claims artefacts %+v; it produced none", entry.Artefacts)
		}
		if entry.Detail == "" {
			t.Error("the abandoned run records no reason")
		}
	})
}

// onlyRun is the single run the registry at root holds for the fixture
// configuration.
func onlyRun(t *testing.T, root string) registry.Entry {
	t.Helper()

	found := runsUnder(t, root, fixtureConfiguration(t))
	if len(found) != 1 {
		t.Fatalf("the registry holds %d runs, want exactly one", len(found))
	}
	return found[0]
}

// mustCompleteJournal is the journal an uninterrupted run of the fixture
// writes, for a test that needs to say what "the whole stream" looks like.
func mustCompleteJournal(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "complete.jsonl")
	var log bytes.Buffer
	if err := backtest(context.Background(), options{
		configPath: configurationFixture,
		barsPath:   barsFixture,
		outPath:    path,
		build:      testBuild,
	}, &log); err != nil {
		t.Fatalf("backtest() error = %v\n%s", err, log.String())
	}
	return path
}

// unreadableDir makes dir writable and traversable but not readable, so that
// a name can still be created in it and syncDir's own open of it fails. A
// user who can read such a directory anyway skips the test rather than
// letting it pass without exercising anything.
func unreadableDir(t *testing.T, dir string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatalf("chmod %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })
	if file, err := os.Open(dir); err == nil {
		_ = file.Close()
		t.Skip("this user reads a directory it has no read permission on, so the flush cannot be made to fail here")
	}
}

// TestAJournalWhoseDirectoryCouldNotBeFlushedIsStillTheRunsOwnEvidence. The
// journal is installed by hard link and its directory's own entry is flushed
// after it, exactly as the registry entry's is: a crash between the two would
// otherwise leave the registry anchoring a journal whose name did not survive
// (ADR 0017).
//
// The link is the commit point, so a flush that fails leaves the journal
// installed BY THIS RUN — which is what decides whether its chain head may be
// anchored here. The command reports the flush and the entry still carries
// the artefacts.
func TestAJournalWhoseDirectoryCouldNotBeFlushedIsStillTheRunsOwnEvidence(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "runs")
	journals := filepath.Join(dir, "journals")
	unreadableDir(t, journals)
	journalPath := filepath.Join(journals, "journal.jsonl")

	var log bytes.Buffer
	err := backtest(context.Background(), options{
		configPath:   configurationFixture,
		barsPath:     barsFixture,
		outPath:      journalPath,
		registryPath: root,
		runID:        "journal-unflushed",
		variant:      registry.Baseline,
		build:        testBuild,
	}, &log)
	if err == nil {
		t.Fatal("backtest() error = nil, want the flush that failed to be reported")
	}
	for _, want := range []string{journalPath, journals} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("backtest() error = %v, want it to mention %q", err, want)
		}
	}

	found := runsUnder(t, root, fixtureConfiguration(t))
	if len(found) != 1 {
		t.Fatalf("the registry holds %d runs, want the run recorded", len(found))
	}
	entry := found[0]
	if entry.Status != registry.StatusCompleted {
		t.Errorf("status = %q, want %q: the run reached the end of its stream and installed its journal", entry.Status, registry.StatusCompleted)
	}
	if entry.Detail == "" {
		t.Error("the entry records nothing about the flush that failed")
	}
	verification := verifyJournal(t, journalPath)
	if entry.Artefacts.FinalRecordHash != verification.FinalRecordHash {
		t.Errorf("recorded final record hash = %q, want the journal this run installed %q", entry.Artefacts.FinalRecordHash, verification.FinalRecordHash)
	}
}

// TestTheRegistryRootThisCommandCreatedIsFlushedWhereItsNameLives. MkdirAll
// creates the registry root when it is not there, and a name is only durable
// once the directory HOLDING it is flushed. Flushing the root and the
// configuration-hash directory below it leaves the root's own entry in the
// page cache, so a power loss could come back with the whole registry gone
// and the command having reported success.
func TestTheRegistryRootThisCommandCreatedIsFlushedWhereItsNameLives(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "evidence")
	unreadableDir(t, parent)
	root := filepath.Join(parent, "runs")

	var log bytes.Buffer
	err := backtest(context.Background(), options{
		configPath:   configurationFixture,
		barsPath:     barsFixture,
		outPath:      filepath.Join(dir, "journal.jsonl"),
		registryPath: root,
		runID:        "root-created",
		variant:      registry.Baseline,
		build:        testBuild,
	}, &log)
	if err == nil {
		t.Fatal("backtest() error = nil, want the flush of the directory holding the new root to be reported")
	}
	if !strings.Contains(err.Error(), parent) {
		t.Errorf("backtest() error = %v, want it to mention %q, where the root's own name lives", err, parent)
	}

	found := runsUnder(t, root, fixtureConfiguration(t))
	if len(found) != 1 {
		t.Fatalf("the registry holds %d runs, want the entry the link installed", len(found))
	}
}
