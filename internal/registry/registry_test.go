package registry_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/registry"
)

// The fixture configuration, and why every one of its values can fail.
//
// Three defects have survived in this repository because a fixture's
// constants never exercised the difference, so these are chosen against the
// encoder rather than for readability:
//
//   - TierBDistanceInN is 0.5, and the sensitivity table moves it by ONE unit
//     in the last place. Any encoder that rendered a float at fixed precision,
//     or through float32, or by rounding "for tidiness", would collapse the two
//     values onto the same hash. Whole numbers would not notice.
//   - Commission.PerShare is 0.005 and moves in its tenth significant digit,
//     inside a NESTED struct: it catches both a lossy float rendering and an
//     encoder that walks only top-level fields.
//   - NotionalAccount.RebasingDay is a nested INT, so the nested-field case is
//     covered for a type whose formatting is exact — a failure there can only
//     be a traversal bug, never a precision one.
//   - SlippageN is 0.05 (ADR 0013), which is what makes the zero-slippage
//     tests a change from a legitimate value rather than from nothing.
func baselineConfiguration() event.ConfigurationPayload {
	return event.ConfigurationPayload{
		StrategyID:             "turtle-baseline",
		SizingMode:             event.SizingModeVolatilityNormalised,
		UnitVolatilityFraction: 0.005,
		StopMultiple:           2,
		EntryChannelLength:     55,
		ExitChannelLength:      20,
		MaxUnits:               4,
		SlippageN:              0.05,
		TierBDistanceInN:       0.5,
		DollarsPerPoint:        1,
		NotionalAccount: event.NotionalAccountConfig{
			StartingEquity: 1_000_000,
			RebasingMonth:  1,
			RebasingDay:    1,
		},
		Commission: event.CommissionConfig{
			PerShare:                    0.005,
			MinimumPerOrder:             1,
			MaximumFractionOfTradeValue: 0.01,
		},
	}
}

var (
	spanStart = time.Date(1998, time.January, 2, 0, 0, 0, 0, time.UTC)
	spanEnd   = time.Date(2025, time.December, 31, 0, 0, 0, 0, time.UTC)
)

// completedRun is a run that finished and left a journal behind.
func completedRun(runID string) registry.Run {
	return registry.Run{
		RunID:           runID,
		Variant:         registry.Baseline,
		Status:          registry.StatusCompleted,
		StrategyVersion: event.ComposeStrategyVersion("turtle-baseline", "1.0.0", "test"),
		SpanStart:       spanStart,
		SpanEnd:         spanEnd,
		Configuration:   baselineConfiguration(),
		Artefacts: registry.Artefacts{
			JournalPath:     "journals/" + runID + ".jsonl",
			RecordCount:     412,
			FinalRecordHash: "sha256:" + strings.Repeat("ab", 32),
		},
	}
}

func mustEntry(t *testing.T, run registry.Run) registry.Entry {
	t.Helper()
	entry, err := registry.NewEntry(run)
	if err != nil {
		t.Fatalf("registry.NewEntry(%+v) error = %v", run, err)
	}
	return entry
}

// mustPath is the file an entry occupies, relative to a registry root.
func mustPath(t *testing.T, entry registry.Entry) string {
	t.Helper()
	path, err := entry.Path()
	if err != nil {
		t.Fatalf("Entry.Path() error = %v", err)
	}
	return path
}

// files is a registry as it sits on disk, keyed by slash-separated path.
type files map[string][]byte

func (f files) ReadDir(dir string) ([]string, error) {
	var names []string
	var exists bool
	for name := range f {
		rest, ok := strings.CutPrefix(name, dir+"/")
		if !ok {
			continue
		}
		exists = true
		// Only what is directly in dir; a nested path contributes the name
		// of the directory holding it, exactly as a real listing would.
		entry, _, _ := strings.Cut(rest, "/")
		if !slices.Contains(names, entry) {
			names = append(names, entry)
		}
	}
	if !exists {
		return nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrNotExist}
	}
	// Deliberately reverse-sorted: nothing may depend on a store's order.
	slices.Sort(names)
	slices.Reverse(names)
	return names, nil
}

func (f files) ReadFile(name string) ([]byte, error) {
	raw, ok := f[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return raw, nil
}

// encoded is entry as the file a caller would have installed for it.
func encoded(t *testing.T, entry registry.Entry) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := registry.Encode(&buf, entry); err != nil {
		t.Fatalf("registry.Encode() error = %v", err)
	}
	return buf.Bytes()
}

// recordedIn is a registry holding exactly the given entries.
func recordedIn(t *testing.T, entries ...registry.Entry) files {
	t.Helper()

	store := files{}
	for _, entry := range entries {
		store[mustPath(t, entry)] = encoded(t, entry)
	}
	return store
}

// TestTheSameConfigurationRegistersTheSameHashTwice is ADR 0012's first
// requirement of the registry: two runs of one configuration are recognisably
// runs of one configuration.
//
// The two payloads are built by DIFFERENT routes — a Go literal and a JSON
// round trip — because the identical-literal version of this test would pass
// against an encoder that lost precision, and the whole point of the fixture's
// 0.005 and 0.5 is that a lossy route is detectable.
func TestTheSameConfigurationRegistersTheSameHashTwice(t *testing.T) {
	t.Parallel()

	direct := baselineConfiguration()

	encoded, err := json.Marshal(direct)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var roundTripped event.ConfigurationPayload
	if err := json.Unmarshal(encoded, &roundTripped); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	first := mustEntry(t, completedRun("run-one"))
	second := completedRun("run-two")
	second.Configuration = roundTripped
	secondEntry := mustEntry(t, second)

	if first.ConfigurationHash != secondEntry.ConfigurationHash {
		t.Fatalf("two runs of one configuration registered %q and %q", first.ConfigurationHash, secondEntry.ConfigurationHash)
	}
	if want := event.ConfigurationHash(direct); first.ConfigurationHash != want {
		t.Fatalf("registered hash = %q, want the hash event.ConfigurationHash derives, %q", first.ConfigurationHash, want)
	}

	firstDir, secondDir := mustPath(t, first), mustPath(t, secondEntry)
	if dirOf(firstDir) != dirOf(secondDir) {
		t.Fatalf("two runs of one configuration were filed under %q and %q", dirOf(firstDir), dirOf(secondDir))
	}
	if firstDir == secondDir {
		t.Fatalf("two runs of one configuration occupy the same file %q; one would overwrite the other", firstDir)
	}
}

func dirOf(p string) string {
	cut := strings.LastIndex(p, "/")
	if cut < 0 {
		return ""
	}
	return p[:cut]
}

// TestAnyParameterChangeChangesTheHash is ADR 0012's second requirement. Each
// row changes exactly one field; see baselineConfiguration for why these
// particular values would catch a lossy or shallow encoder rather than
// agreeing with one.
func TestAnyParameterChangeChangesTheHash(t *testing.T) {
	t.Parallel()

	baseline := mustEntry(t, completedRun("baseline"))

	tests := []struct {
		name   string
		change func(*event.ConfigurationPayload)
	}{
		{
			// One unit in the last place above 0.5. The two values are
			// distinct float64s and render distinctly under shortest
			// round-trip formatting, and under nothing else.
			name:   "tier b distance by one unit in the last place",
			change: func(c *event.ConfigurationPayload) { c.TierBDistanceInN = 0.5000000000000001 },
		},
		{
			name:   "a nested float in its tenth significant digit",
			change: func(c *event.ConfigurationPayload) { c.Commission.PerShare = 0.0050000001 },
		},
		{
			name:   "a nested integer",
			change: func(c *event.ConfigurationPayload) { c.NotionalAccount.RebasingDay = 2 },
		},
		{
			name:   "the stop multiple",
			change: func(c *event.ConfigurationPayload) { c.StopMultiple = 2.5 },
		},
		{
			name:   "an entry channel length",
			change: func(c *event.ConfigurationPayload) { c.EntryChannelLength = 54 },
		},
	}

	seen := map[string]string{baseline.ConfigurationHash: "baseline"}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := completedRun("changed")
			test.change(&run.Configuration)
			changed := mustEntry(t, run)

			if changed.ConfigurationHash == baseline.ConfigurationHash {
				t.Fatalf("changing %s left the configuration hash at %q", test.name, changed.ConfigurationHash)
			}
			if previous, ok := seen[changed.ConfigurationHash]; ok {
				t.Fatalf("changing %s produced the same hash as changing %s", test.name, previous)
			}
			seen[changed.ConfigurationHash] = test.name
		})
	}
}

// TestAZeroSlippageRunIsRefused is ADR 0013: a backtest run with zero
// slippage is invalid by construction and must be rejected by the run
// registry. Retention (AGENTS.md rule 6) is not a licence to record it —
// nothing was produced that is evidence of anything — so every status is
// refused, not just the successful one.
func TestAZeroSlippageRunIsRefused(t *testing.T) {
	t.Parallel()

	for _, status := range []registry.Status{registry.StatusCompleted, registry.StatusFailed, registry.StatusAbandoned} {
		t.Run(string(status), func(t *testing.T) {
			run := completedRun("zero-slippage")
			run.Status = status
			run.Detail = "recorded for the test"
			run.Configuration.SlippageN = 0

			_, err := registry.NewEntry(run)
			if err == nil {
				t.Fatal("registry.NewEntry() error = nil, want a zero-slippage run to be refused")
			}
			if !strings.Contains(err.Error(), "ADR 0013") {
				t.Errorf("registry.NewEntry() error = %v, want it to cite the rule it enforces", err)
			}
		})
	}
}

// TestANegativeSlippageRunIsRefused: "never zero" is a floor, not an equality
// check, and a negative slippage would pay the trader to trade.
func TestANegativeSlippageRunIsRefused(t *testing.T) {
	t.Parallel()

	run := completedRun("negative-slippage")
	run.Configuration.SlippageN = -0.05

	if _, err := registry.NewEntry(run); err == nil {
		t.Fatal("registry.NewEntry() error = nil, want a negative-slippage run to be refused")
	}
}

// TestAFailedRunIsRetainedAndFindable is the point of the registry: the
// graveyard survives, so a surviving Variant cannot look more special than it
// is (ADR 0012).
func TestAFailedRunIsRetainedAndFindable(t *testing.T) {
	t.Parallel()

	failed := completedRun("run-that-failed")
	failed.Status = registry.StatusFailed
	failed.Detail = "the run halted: a unit sized beyond the exactly representable range"

	abandoned := completedRun("run-that-was-abandoned")
	abandoned.Status = registry.StatusAbandoned
	abandoned.Detail = "superseded by a corrected fixture before the span completed"
	abandoned.Artefacts = registry.Artefacts{}
	abandoned.SpanStart, abandoned.SpanEnd = time.Time{}, time.Time{}

	entries := []registry.Entry{
		mustEntry(t, completedRun("run-that-completed")),
		mustEntry(t, failed),
		mustEntry(t, abandoned),
	}
	fsys := recordedIn(t, entries...)

	found, err := registry.Runs(fsys, entries[0].ConfigurationHash)
	if err != nil {
		t.Fatalf("registry.Runs() error = %v", err)
	}
	if len(found) != len(entries) {
		t.Fatalf("registry.Runs() returned %d runs, want all %d recorded under the configuration", len(found), len(entries))
	}

	byStatus := map[registry.Status]registry.Entry{}
	for _, entry := range found {
		byStatus[entry.Status] = entry
	}
	for _, want := range []registry.Status{registry.StatusCompleted, registry.StatusFailed, registry.StatusAbandoned} {
		if _, ok := byStatus[want]; !ok {
			t.Errorf("registry.Runs() returned no %s run; a failed or abandoned run is retained, never dropped", want)
		}
	}
	if detail := byStatus[registry.StatusFailed].Detail; !strings.Contains(detail, "halted") {
		t.Errorf("the failed run's detail = %q, want the reason it failed to survive with it", detail)
	}
	if journalPath := byStatus[registry.StatusFailed].Artefacts.JournalPath; journalPath == "" {
		t.Error("the failed run records no journal; the partial evidence a failed run left is exactly what a reviewer needs")
	}
}

// TestARunIsLocatedByItsConfigurationHash: retrieval is by hash alone, and a
// run recorded under a different configuration is not returned for it.
func TestARunIsLocatedByItsConfigurationHash(t *testing.T) {
	t.Parallel()

	wanted := mustEntry(t, completedRun("wanted"))

	other := completedRun("other")
	other.Configuration.StopMultiple = 3
	otherEntry := mustEntry(t, other)

	fsys := recordedIn(t, wanted, otherEntry)

	found, err := registry.Runs(fsys, wanted.ConfigurationHash)
	if err != nil {
		t.Fatalf("registry.Runs() error = %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("registry.Runs() returned %d runs, want only the one recorded under that configuration", len(found))
	}
	if found[0].RunID != "wanted" {
		t.Fatalf("registry.Runs() returned run %q, want %q", found[0].RunID, "wanted")
	}
	if found[0].Artefacts.JournalPath != wanted.Artefacts.JournalPath {
		t.Fatalf("the located run names journal %q, want %q: a run is located so its journal can be replayed", found[0].Artefacts.JournalPath, wanted.Artefacts.JournalPath)
	}
}

// TestAConfigurationWithNoRecordedRunsHasNone: an unrecorded configuration is
// an empty answer, not a failure — asking whether a configuration has ever
// been run is a legitimate question with "no" as an answer.
func TestAConfigurationWithNoRecordedRunsHasNone(t *testing.T) {
	t.Parallel()

	fsys := recordedIn(t, mustEntry(t, completedRun("recorded")))

	unrun := baselineConfiguration()
	unrun.MaxUnits = 6

	found, err := registry.Runs(fsys, event.ConfigurationHash(unrun))
	if err != nil {
		t.Fatalf("registry.Runs() error = %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("registry.Runs() returned %d runs for a configuration never run", len(found))
	}
}

// TestAnUnrecognisedStatusIsRefused: the status vocabulary is closed in both
// directions. Storing an unrecognised one as-is would let a run describe
// itself however it liked, and a later count of what failed would be wrong.
func TestAnUnrecognisedStatusIsRefused(t *testing.T) {
	t.Parallel()

	t.Run("when a run is recorded", func(t *testing.T) {
		run := completedRun("invented-status")
		run.Status = "succeeded"

		_, err := registry.NewEntry(run)
		if err == nil {
			t.Fatal("registry.NewEntry() error = nil, want an unrecognised status to be refused")
		}
		if !strings.Contains(err.Error(), "succeeded") {
			t.Errorf("registry.NewEntry() error = %v, want it to name the status it refused", err)
		}
	})

	t.Run("when a recorded run is read back", func(t *testing.T) {
		entry := mustEntry(t, completedRun("invented-status"))

		var encoded bytes.Buffer
		if err := registry.Encode(&encoded, entry); err != nil {
			t.Fatalf("registry.Encode() error = %v", err)
		}
		tampered := bytes.Replace(encoded.Bytes(), []byte(`"completed"`), []byte(`"succeeded"`), 1)
		if bytes.Equal(tampered, encoded.Bytes()) {
			t.Fatal("the encoded entry does not state its status as a JSON string; the tamper did nothing")
		}

		if _, err := registry.Decode(bytes.NewReader(tampered)); err == nil {
			t.Fatal("registry.Decode() error = nil, want an unrecognised status to fail closed on the way in")
		}
	})
}

// TestARunThatStatesNoStatusIsRefused: the zero value of a string type is not
// a status, and it is what a record written without one decodes to.
func TestARunThatStatesNoStatusIsRefused(t *testing.T) {
	t.Parallel()

	run := completedRun("no-status")
	run.Status = ""

	if _, err := registry.NewEntry(run); err == nil {
		t.Fatal("registry.NewEntry() error = nil, want a run stating no status to be refused")
	}
}

// TestAnEntryWhoseHashDisagreesWithItsOwnConfigurationIsRefused: the hash in
// the file is a claim, and the configuration beside it is the evidence for
// that claim. Re-deriving it on the way in is what stops a run being quietly
// re-attributed to a different Variant.
func TestAnEntryWhoseHashDisagreesWithItsOwnConfigurationIsRefused(t *testing.T) {
	t.Parallel()

	entry := mustEntry(t, completedRun("re-attributed"))

	var encoded bytes.Buffer
	if err := registry.Encode(&encoded, entry); err != nil {
		t.Fatalf("registry.Encode() error = %v", err)
	}

	elsewhere := baselineConfiguration()
	elsewhere.MaxUnits = 6
	tampered := bytes.Replace(encoded.Bytes(), []byte(entry.ConfigurationHash), []byte(event.ConfigurationHash(elsewhere)), 1)
	if bytes.Equal(tampered, encoded.Bytes()) {
		t.Fatal("the encoded entry does not state its configuration hash; the tamper did nothing")
	}

	_, err := registry.Decode(bytes.NewReader(tampered))
	if err == nil {
		t.Fatal("registry.Decode() error = nil, want an entry whose hash does not follow from its own configuration to be refused")
	}
	if !strings.Contains(err.Error(), "configuration") {
		t.Errorf("registry.Decode() error = %v, want it to say what disagrees", err)
	}
}

// TestARunFiledUnderTheWrongConfigurationIsRefused: the directory name is the
// index, so a file moved into another configuration's directory would
// otherwise be returned as a run of that configuration.
func TestARunFiledUnderTheWrongConfigurationIsRefused(t *testing.T) {
	t.Parallel()

	misfiled := mustEntry(t, completedRun("misfiled"))

	elsewhere := baselineConfiguration()
	elsewhere.MaxUnits = 6
	wrongHash := event.ConfigurationHash(elsewhere)
	wrongDir, err := registry.Dir(wrongHash)
	if err != nil {
		t.Fatalf("registry.Dir() error = %v", err)
	}

	store := files{wrongDir + "/misfiled.json": encoded(t, misfiled)}

	if _, err := registry.Runs(store, wrongHash); err == nil {
		t.Fatal("registry.Runs() error = nil, want a run filed under another configuration to be refused")
	}
}

// TestARunWhoseFileNameIsNotItsRunIDIsRefused: the file name is the run's
// identity on disk, and it is what makes a second run of the same
// configuration land beside the first rather than on top of it. A file whose
// name and content disagree has lost that guarantee.
func TestARunWhoseFileNameIsNotItsRunIDIsRefused(t *testing.T) {
	t.Parallel()

	entry := mustEntry(t, completedRun("declared"))
	dir, err := registry.Dir(entry.ConfigurationHash)
	if err != nil {
		t.Fatalf("registry.Dir() error = %v", err)
	}

	store := files{dir + "/renamed.json": encoded(t, entry)}

	if _, err := registry.Runs(store, entry.ConfigurationHash); err == nil {
		t.Fatal("registry.Runs() error = nil, want a run whose file name is not its run id to be refused")
	}
}

// TestARunIDThatWouldNotSurviveTheFilesystemIsRefused. The run id becomes a
// file name, so anything that could escape the registry, collide on a
// case-insensitive filesystem, or name a directory rather than a run is
// refused before it is ever written.
func TestARunIDThatWouldNotSurviveTheFilesystemIsRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		runID string
	}{
		{"empty", ""},
		{"a path separator", "2026/baseline"},
		{"a parent directory", ".."},
		{"a leading dot", ".hidden"},
		{"an upper-case letter", "Baseline-01"},
		{"a space", "baseline 01"},
		{"a trailing hyphen", "baseline-"},
		{"a backslash", `2026\baseline`},
		{"longer than a file name holds", strings.Repeat("a", 200)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := completedRun("placeholder")
			run.RunID = test.runID

			if _, err := registry.NewEntry(run); err == nil {
				t.Fatalf("registry.NewEntry() with run id %q error = nil, want it refused", test.runID)
			}
		})
	}
}

// TestAConfigurationHashThatIsNotOneIsRefused: Dir turns a hash into a
// directory name, and it is reached from an operator's command line, so a
// value that is not a hash must not become a path.
func TestAConfigurationHashThatIsNotOneIsRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		hash string
	}{
		{"empty", ""},
		{"no algorithm", strings.Repeat("ab", 32)},
		{"an empty algorithm", ":" + strings.Repeat("ab", 32)},
		{"an empty digest", "sha256:"},
		{"a path separator in the digest", "sha256:../../etc"},
		{"a parent directory", ".."},
		{"upper case", "SHA256:" + strings.Repeat("AB", 32)},
		// A digest of the wrong length or alphabet is the shape that reads
		// as evidence of absence: it becomes a directory that happens not to
		// exist, and the answer comes back "no run is recorded".
		{"a truncated digest", "sha256:0"},
		{"a digest one character short", "sha256:" + strings.Repeat("a", 63)},
		{"a digest one character long", "sha256:" + strings.Repeat("a", 65)},
		{"a non-hexadecimal digest", "sha256:g" + strings.Repeat("a", 63)},
		{"an algorithm this build never derives", "md5:" + strings.Repeat("ab", 16)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := registry.Dir(test.hash); err == nil {
				t.Fatalf("registry.Dir(%q) error = nil, want it refused", test.hash)
			}
			if _, err := registry.Runs(files{}, test.hash); err == nil {
				t.Fatalf("registry.Runs(%q) error = nil, want it refused", test.hash)
			}
		})
	}
}

// TestTheDirectoryIsTheConfigurationHash: the layout is the index. There is
// no index file to rewrite, which is what lets two runs recorded on two
// branches merge without a conflict.
func TestTheDirectoryIsTheConfigurationHash(t *testing.T) {
	t.Parallel()

	entry := mustEntry(t, completedRun("run-one"))
	path := mustPath(t, entry)

	digest := strings.TrimPrefix(entry.ConfigurationHash, "sha256:")
	if want := "sha256-" + digest + "/run-one.json"; path != want {
		t.Fatalf("Entry.Path() = %q, want %q", path, want)
	}
	if strings.Contains(path, ":") {
		t.Errorf("Entry.Path() = %q, want no %q: it is not a legal file name on every filesystem the registry is cloned onto", path, ":")
	}
}

// TestAnEntryThatMayNotBeRecordedHasNoPath: a path is where a run will be
// written, so an entry that may not be written has nowhere to go.
func TestAnEntryThatMayNotBeRecordedHasNoPath(t *testing.T) {
	t.Parallel()

	path, err := registry.Entry{}.Path()
	if err == nil {
		t.Fatal("Entry.Path() error = nil, want an entry that may not be recorded to have no path")
	}
	if path != "" {
		t.Fatalf("Entry.Path() = %q, want no path alongside the refusal", path)
	}
}

// TestACompletedRunWithoutAJournalIsRefused: a completed run whose evidence
// is missing is a claim about a result, not a result. A failed or abandoned
// run may legitimately have none.
func TestACompletedRunWithoutAJournalIsRefused(t *testing.T) {
	t.Parallel()

	run := completedRun("no-journal")
	run.Artefacts = registry.Artefacts{}

	if _, err := registry.NewEntry(run); err == nil {
		t.Fatal("registry.NewEntry() error = nil, want a completed run with no journal to be refused")
	}

	abandoned := completedRun("abandoned-without-a-journal")
	abandoned.Status = registry.StatusAbandoned
	abandoned.Detail = "declared and never carried through"
	abandoned.Artefacts = registry.Artefacts{}
	if _, err := registry.NewEntry(abandoned); err != nil {
		t.Fatalf("registry.NewEntry() error = %v, want an abandoned run with no journal to be recorded", err)
	}
}

// TestAJournalRecordedWithoutItsChainHeadIsRefused: ADR 0017 anchors a
// journal's final record hash in the git-committed registry, which is what
// closes the chain's end-truncation gap. A journal path recorded without it
// is an unanchored artefact and would silently lose that.
func TestAJournalRecordedWithoutItsChainHeadIsRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		artefacts registry.Artefacts
	}{
		{"no final record hash", registry.Artefacts{JournalPath: "journals/a.jsonl", RecordCount: 4}},
		{"no record count", registry.Artefacts{JournalPath: "journals/a.jsonl", FinalRecordHash: "sha256:" + strings.Repeat("ab", 32)}},
		{"a chain head with no journal", registry.Artefacts{FinalRecordHash: "sha256:" + strings.Repeat("ab", 32), RecordCount: 4}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := completedRun("unanchored")
			run.Artefacts = test.artefacts

			if _, err := registry.NewEntry(run); err == nil {
				t.Fatal("registry.NewEntry() error = nil, want the incomplete artefact record to be refused")
			}
		})
	}
}

// TestAJournalPathThatOnlyMeansSomethingOnOneMachineIsRefused. The registry
// is committed to git and read wherever it is cloned, so a journal is
// recorded relative to the registry root: an absolute path, a volume name or
// a backslash all name a location on exactly one machine, and an entry
// pointing at one has stopped being evidence for anybody else.
func TestAJournalPathThatOnlyMeansSomethingOnOneMachineIsRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		journalPath string
	}{
		{"an absolute path", "/Users/someone/work/journals/a.jsonl"},
		{"a temporary directory", "/tmp/backtest-1234/journal.jsonl"},
		{"a windows volume", `C:/runs/journals/a.jsonl`},
		{"a backslash", `journals\a.jsonl`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := completedRun("machine-specific")
			run.Artefacts.JournalPath = test.journalPath

			_, err := registry.NewEntry(run)
			if err == nil {
				t.Fatalf("registry.NewEntry() with journal path %q error = nil, want it refused", test.journalPath)
			}
			if !strings.Contains(err.Error(), "registry root") {
				t.Errorf("registry.NewEntry() error = %v, want it to say what a journal path is relative to", err)
			}
		})
	}

	// A path that climbs out of the registry root is still portable — it
	// means the same thing wherever the registry is cloned — so it stands.
	run := completedRun("beside-the-registry")
	run.Artefacts.JournalPath = "../journals/a.jsonl"
	if _, err := registry.NewEntry(run); err != nil {
		t.Fatalf("registry.NewEntry() error = %v, want a journal recorded beside the registry to be accepted", err)
	}
}

// TestAFailedRunStatesWhyItFailed: a status of "failed" with no reason is a
// record that something went wrong and nothing about what, which is the shape
// a curated record takes. A completed run needs no explanation.
func TestAFailedRunStatesWhyItFailed(t *testing.T) {
	t.Parallel()

	for _, status := range []registry.Status{registry.StatusFailed, registry.StatusAbandoned} {
		t.Run(string(status), func(t *testing.T) {
			run := completedRun("silent")
			run.Status = status

			if _, err := registry.NewEntry(run); err == nil {
				t.Fatalf("registry.NewEntry() error = nil, want a %s run with no stated reason to be refused", status)
			}
		})
	}

	if _, err := registry.NewEntry(completedRun("silent")); err != nil {
		t.Fatalf("registry.NewEntry() error = %v, want a completed run to need no explanation", err)
	}
}

// TestARunThatStatesNoVariantIsRefused: a run is a run of the Baseline or of
// a named Variant, and "neither stated" is how a Variant's results come to be
// read as the Baseline's.
func TestARunThatStatesNoVariantIsRefused(t *testing.T) {
	t.Parallel()

	run := completedRun("unattributed")
	run.Variant = ""

	if _, err := registry.NewEntry(run); err == nil {
		t.Fatal("registry.NewEntry() error = nil, want a run stating no Variant to be refused")
	}
}

// TestAStrategyVersionThatThisProjectNeverComposedIsRefused: the recorded
// strategy version must decompose to a strategy, a rules version and a build
// (ADR 0016), or a run cannot be traced back to the code that produced it.
func TestAStrategyVersionThatThisProjectNeverComposedIsRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		version string
	}{
		{"empty", ""},
		{"no build", "turtle-baseline/1.0.0"},
		{"no rules version", "turtle-baseline"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := completedRun("untraceable")
			run.StrategyVersion = test.version

			if _, err := registry.NewEntry(run); err == nil {
				t.Fatalf("registry.NewEntry() with strategy version %q error = nil, want it refused", test.version)
			}
		})
	}
}

// TestAStrategyVersionNamingAnotherStrategyIsRefused: the header of the
// journal and the configuration beside it must describe one run, the same
// check cmd/backtest's replay makes of a journal.
func TestAStrategyVersionNamingAnotherStrategyIsRefused(t *testing.T) {
	t.Parallel()

	run := completedRun("mismatched")
	run.StrategyVersion = event.ComposeStrategyVersion("sublime-control", "1.0.0", "test")

	_, err := registry.NewEntry(run)
	if err == nil {
		t.Fatal("registry.NewEntry() error = nil, want a strategy version naming another strategy to be refused")
	}
	if !strings.Contains(err.Error(), "sublime-control") {
		t.Errorf("registry.NewEntry() error = %v, want it to name the disagreement", err)
	}
}

// TestAnInvalidConfigurationIsNeverRecorded: the registry records the
// configuration itself so a run can be reproduced from it, so a configuration
// nothing could be run under must not be recorded as though it had been.
func TestAnInvalidConfigurationIsNeverRecorded(t *testing.T) {
	t.Parallel()

	run := completedRun("invalid-configuration")
	run.Configuration.EntryChannelLength = 0

	if _, err := registry.NewEntry(run); err == nil {
		t.Fatal("registry.NewEntry() error = nil, want a configuration that could not be run to be refused")
	}
}

// TestASpanThatRunsBackwardsIsRefused, and a completed run states one at both
// ends: a result whose span is unknown cannot be placed in a Regime Window
// (ADR 0012).
func TestASpanIsCoherent(t *testing.T) {
	t.Parallel()

	t.Run("a backwards span is refused", func(t *testing.T) {
		run := completedRun("backwards")
		run.SpanStart, run.SpanEnd = spanEnd, spanStart

		if _, err := registry.NewEntry(run); err == nil {
			t.Fatal("registry.NewEntry() error = nil, want a backwards span to be refused")
		}
	})

	t.Run("a completed run states its span at both ends", func(t *testing.T) {
		run := completedRun("no-span")
		run.SpanStart, run.SpanEnd = time.Time{}, time.Time{}

		if _, err := registry.NewEntry(run); err == nil {
			t.Fatal("registry.NewEntry() error = nil, want a completed run with no span to be refused")
		}
	})

	t.Run("a run that processed nothing has no span", func(t *testing.T) {
		run := completedRun("nothing-processed")
		run.Status = registry.StatusFailed
		run.Detail = "the bar fixture could not be read"
		run.Artefacts = registry.Artefacts{}
		run.SpanStart, run.SpanEnd = time.Time{}, time.Time{}

		if _, err := registry.NewEntry(run); err != nil {
			t.Fatalf("registry.NewEntry() error = %v, want a run that processed no input to be recorded without a span", err)
		}
	})

	t.Run("half a span is refused", func(t *testing.T) {
		run := completedRun("half-a-span")
		run.Status = registry.StatusFailed
		run.Detail = "halted"
		run.SpanEnd = time.Time{}

		if _, err := registry.NewEntry(run); err == nil {
			t.Fatal("registry.NewEntry() error = nil, want a span stated at one end only to be refused")
		}
	})
}

// TestARegistryVersionThisBuildDoesNotUnderstandFailsClosed in both
// directions, the rule ADR 0015 applies to the envelope and ADR 0017 to the
// journal: this build has no upcaster for an older entry and cannot know what
// a newer one means.
func TestARegistryVersionThisBuildDoesNotUnderstandFailsClosed(t *testing.T) {
	t.Parallel()

	entry := mustEntry(t, completedRun("versioned"))
	var encoded bytes.Buffer
	if err := registry.Encode(&encoded, entry); err != nil {
		t.Fatalf("registry.Encode() error = %v", err)
	}
	current := []byte(`"registry_version": ` + strconv.FormatUint(uint64(registry.FormatVersion), 10))
	if !bytes.Contains(encoded.Bytes(), current) {
		t.Fatalf("the encoded entry does not state %s", current)
	}

	for _, version := range []uint32{registry.FormatVersion - 1, registry.FormatVersion + 1} {
		stated := []byte(`"registry_version": ` + strconv.FormatUint(uint64(version), 10))
		other := bytes.Replace(encoded.Bytes(), current, stated, 1)
		if _, err := registry.Decode(bytes.NewReader(other)); err == nil {
			t.Errorf("registry.Decode() of a version %d entry error = nil, want it refused", version)
		}
	}
}

// TestAnEntryRoundTripsThroughItsFile: what is written is what is read, so a
// registry read back years later says what the run said at the time.
func TestAnEntryRoundTripsThroughItsFile(t *testing.T) {
	t.Parallel()

	entry := mustEntry(t, completedRun("round-trip"))

	var encoded bytes.Buffer
	if err := registry.Encode(&encoded, entry); err != nil {
		t.Fatalf("registry.Encode() error = %v", err)
	}
	decoded, err := registry.Decode(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatalf("registry.Decode() error = %v", err)
	}
	if !decoded.SpanStart.Equal(entry.SpanStart) || !decoded.SpanEnd.Equal(entry.SpanEnd) {
		t.Fatalf("the span round tripped as %s..%s, want %s..%s", decoded.SpanStart, decoded.SpanEnd, entry.SpanStart, entry.SpanEnd)
	}
	decoded.SpanStart, decoded.SpanEnd = entry.SpanStart, entry.SpanEnd
	if decoded != entry {
		t.Fatalf("the entry round tripped as\n%+v\nwant\n%+v", decoded, entry)
	}
}

// TestAnEntryIsWrittenOneFieldPerLine: the registry is committed to git and
// read as a diff, and a single-line JSON blob would make every change to a run
// one unreadable line.
func TestAnEntryIsWrittenOneFieldPerLine(t *testing.T) {
	t.Parallel()

	var encoded bytes.Buffer
	if err := registry.Encode(&encoded, mustEntry(t, completedRun("readable"))); err != nil {
		t.Fatalf("registry.Encode() error = %v", err)
	}
	if lines := bytes.Count(encoded.Bytes(), []byte("\n")); lines < 20 {
		t.Fatalf("the encoded entry has %d lines; it is committed to git and read as a diff", lines)
	}
	if !bytes.HasSuffix(encoded.Bytes(), []byte("\n")) {
		t.Error("the encoded entry does not end in a newline")
	}
}

// TestAFileHoldingMoreThanOneEntryIsRefused: a second object appended after
// the first would be silently ignored, which is a run hidden in plain sight.
func TestAFileHoldingMoreThanOneEntryIsRefused(t *testing.T) {
	t.Parallel()

	var encoded bytes.Buffer
	if err := registry.Encode(&encoded, mustEntry(t, completedRun("first"))); err != nil {
		t.Fatalf("registry.Encode() error = %v", err)
	}
	if err := registry.Encode(&encoded, mustEntry(t, completedRun("second"))); err != nil {
		t.Fatalf("registry.Encode() error = %v", err)
	}

	if _, err := registry.Decode(bytes.NewReader(encoded.Bytes())); err == nil {
		t.Fatal("registry.Decode() error = nil, want a file holding two entries to be refused")
	}
}

// TestAnEntryCarryingAnUnknownFieldIsRefused: a field this build does not
// know is a field it would silently drop, and a dropped field is a fact about
// a run that the registry claims to have recorded and has not.
func TestAnEntryCarryingAnUnknownFieldIsRefused(t *testing.T) {
	t.Parallel()

	var encoded bytes.Buffer
	if err := registry.Encode(&encoded, mustEntry(t, completedRun("extra-field"))); err != nil {
		t.Fatalf("registry.Encode() error = %v", err)
	}
	with := bytes.Replace(encoded.Bytes(), []byte("{\n"), []byte("{\n  \"adopted\": true,\n"), 1)

	if _, err := registry.Decode(bytes.NewReader(with)); err == nil {
		t.Fatal("registry.Decode() error = nil, want an entry carrying an unknown field to be refused")
	}
}

// TestAnUndecodableEntryIsReported: a truncated or corrupt file names itself
// rather than being skipped, so a registry that has lost a run says so.
func TestAnUndecodableEntryIsReported(t *testing.T) {
	t.Parallel()

	if _, err := registry.Decode(bytes.NewReader([]byte("{not json"))); err == nil {
		t.Fatal("registry.Decode() error = nil, want a corrupt entry to be refused")
	}

	entry := mustEntry(t, completedRun("corrupt"))
	dir, err := registry.Dir(entry.ConfigurationHash)
	if err != nil {
		t.Fatalf("registry.Dir() error = %v", err)
	}
	store := files{dir + "/corrupt.json": []byte("{not json")}

	_, err = registry.Runs(store, entry.ConfigurationHash)
	if err == nil {
		t.Fatal("registry.Runs() error = nil, want a corrupt entry to be reported")
	}
	if !strings.Contains(err.Error(), "corrupt.json") {
		t.Errorf("registry.Runs() error = %v, want it to name the file it could not read", err)
	}
}

// TestANonEntryInAConfigurationsDirectoryIsIgnored: a note beside the runs, or
// an editor's backup file, is not a run and must not be read as one — nor
// must it stop the runs beside it being found.
func TestANonEntryInAConfigurationsDirectoryIsIgnored(t *testing.T) {
	t.Parallel()

	entry := mustEntry(t, completedRun("recorded"))
	dir, err := registry.Dir(entry.ConfigurationHash)
	if err != nil {
		t.Fatalf("registry.Dir() error = %v", err)
	}

	store := files{
		dir + "/recorded.json":     encoded(t, entry),
		dir + "/NOTES.md":          []byte("why this configuration was run\n"),
		dir + "/.run-1234.partial": []byte("an install still in progress\n"),
		dir + "/nested/other":      []byte("not a run either\n"),
	}

	found, err := registry.Runs(store, entry.ConfigurationHash)
	if err != nil {
		t.Fatalf("registry.Runs() error = %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("registry.Runs() returned %d runs, want the one recorded run beside the notes", len(found))
	}
}

// TestAnUnreadableRegistryIsReported rather than read as an empty one: "this
// configuration has never been run" and "this registry could not be read" are
// different findings, and only one of them is evidence.
func TestAnUnreadableRegistryIsReported(t *testing.T) {
	t.Parallel()

	entry := mustEntry(t, completedRun("unreadable"))
	dir, err := registry.Dir(entry.ConfigurationHash)
	if err != nil {
		t.Fatalf("registry.Dir() error = %v", err)
	}

	_, err = registry.Runs(refusingStore{dir: dir}, entry.ConfigurationHash)
	if err == nil {
		t.Fatal("registry.Runs() error = nil, want the read failure to be reported")
	}
	if !errors.Is(err, errRefused) {
		t.Errorf("registry.Runs() error = %v, want it to carry the underlying failure", err)
	}
}

// TestARecordedRunThatCannotBeOpenedIsReported: the directory lists it and
// the file will not open. A registry that skipped it would report fewer runs
// than it holds, which is the one thing this registry exists to prevent.
func TestARecordedRunThatCannotBeOpenedIsReported(t *testing.T) {
	t.Parallel()

	entry := mustEntry(t, completedRun("unopenable"))
	dir, err := registry.Dir(entry.ConfigurationHash)
	if err != nil {
		t.Fatalf("registry.Dir() error = %v", err)
	}

	name := dir + "/unopenable.json"
	store := unreadableStore{files: files{name: encoded(t, entry)}, name: name}

	_, err = registry.Runs(store, entry.ConfigurationHash)
	if err == nil {
		t.Fatal("registry.Runs() error = nil, want the run it could not open to be reported")
	}
	if !errors.Is(err, errRefused) {
		t.Errorf("registry.Runs() error = %v, want it to carry the underlying failure", err)
	}
}

// TestAnEntryThatMayNotBeRecordedIsNeverWritten: Encode is the last point
// before an entry becomes a file, so it refuses one Validate would refuse
// rather than leaving the check to whoever reads it back.
func TestAnEntryThatMayNotBeRecordedIsNeverWritten(t *testing.T) {
	t.Parallel()

	var encoded bytes.Buffer
	if err := registry.Encode(&encoded, registry.Entry{}); err == nil {
		t.Fatal("registry.Encode() error = nil, want an entry that may not be recorded to be refused")
	}
	if encoded.Len() != 0 {
		t.Fatalf("registry.Encode() wrote %d bytes before refusing", encoded.Len())
	}
}

// TestAnEntryThatCannotBeWrittenIsReported: a full disk, a closed pipe. The
// caller installs the file, so it has to be told the content never arrived.
func TestAnEntryThatCannotBeWrittenIsReported(t *testing.T) {
	t.Parallel()

	err := registry.Encode(failingWriter{}, mustEntry(t, completedRun("unwritable")))
	if !errors.Is(err, errRefused) {
		t.Fatalf("registry.Encode() error = %v, want the write failure reported", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errRefused }

var errRefused = errors.New("the registry could not be read")

// unreadableStore lists a run and will not read it: a permission change, a
// failing disk, a half-mounted volume.
type unreadableStore struct {
	files
	name string
}

func (s unreadableStore) ReadFile(name string) ([]byte, error) {
	if name == s.name {
		return nil, &fs.PathError{Op: "open", Path: name, Err: errRefused}
	}
	return s.files.ReadFile(name)
}

// refusingStore is a registry whose directory exists and cannot be listed.
type refusingStore struct{ dir string }

func (s refusingStore) ReadDir(dir string) ([]string, error) {
	if dir == s.dir {
		return nil, &fs.PathError{Op: "open", Path: dir, Err: errRefused}
	}
	return nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrNotExist}
}

func (s refusingStore) ReadFile(name string) ([]byte, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// TestARunIDWindowsResolvesAsADeviceIsRefused. A run id becomes a file name,
// and Win32 resolves the MS-DOS device names ahead of a file of the same
// name, with or without an extension — so `<run-id>.json` for one of them is
// a name the registry cannot create there. The registry is committed to git
// and cloned onto whatever machine reads it, so the id is refused when it is
// chosen rather than on the machine that cannot honour it.
//
// This project's CI does not run on Windows, so the refusal is tested here
// and the Win32 behaviour behind it is not: what this test pins is that the
// id is refused, not that Windows would have refused it.
func TestARunIDWindowsResolvesAsADeviceIsRefused(t *testing.T) {
	t.Parallel()

	for _, runID := range []string{"con", "prn", "aux", "nul", "com1", "com9", "lpt1", "lpt9"} {
		t.Run(runID, func(t *testing.T) {
			run := completedRun("placeholder")
			run.RunID = runID

			if _, err := registry.NewEntry(run); err == nil {
				t.Fatalf("registry.NewEntry() with run id %q error = nil, want it refused", runID)
			}
		})
	}

	// A device name is reserved as a whole name, not as a prefix.
	for _, runID := range []string{"console", "com1-baseline", "nullable"} {
		t.Run(runID, func(t *testing.T) {
			run := completedRun("placeholder")
			run.RunID = runID

			if _, err := registry.NewEntry(run); err != nil {
				t.Fatalf("registry.NewEntry() with run id %q error = %v, want it accepted", runID, err)
			}
		})
	}
}

// TestASpanTheRegistryCannotWriteDownIsRefused. Validate reports every way an
// entry may not be recorded, so a span it accepts must be one Encode can
// write: an entry that passes validation and then fails to encode is a
// contract disagreeing with itself, and it fails at the install rather than
// at the point the span was chosen.
//
// RFC 3339 spans years 0 to 9999, which is what encoding/json holds a
// time.Time to.
func TestASpanTheRegistryCannotWriteDownIsRefused(t *testing.T) {
	t.Parallel()

	outOfRange := time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		start, end time.Time
	}{
		{"a span that ends outside the years RFC 3339 spans", spanStart, outOfRange},
		{"a span that starts outside them", outOfRange, outOfRange.AddDate(1, 0, 0)},
		{"a span before year zero", time.Date(-1, time.January, 1, 0, 0, 0, 0, time.UTC), spanEnd},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := completedRun("unwritable-span")
			run.SpanStart, run.SpanEnd = test.start, test.end

			if _, err := registry.NewEntry(run); err == nil {
				t.Fatalf("registry.NewEntry() error = nil, want a span the registry cannot write down to be refused")
			}
		})
	}
}

// TestAnEntryThatValidatesEncodes is the other half of the contract above: an
// entry Validate accepts can always be written down, so nothing that passes
// validation is lost at the install.
func TestAnEntryThatValidatesEncodes(t *testing.T) {
	t.Parallel()

	run := completedRun("at-the-edge")
	run.SpanStart = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)
	run.SpanEnd = time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)

	var encoded bytes.Buffer
	if err := registry.Encode(&encoded, mustEntry(t, run)); err != nil {
		t.Fatalf("registry.Encode() error = %v, want an entry Validate accepted to be writable", err)
	}
}
