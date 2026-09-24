package strategy_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// --- #100: a decision corpus, so a changed PREDICATE fails by name --------
//
// strategy.RuleSurfaceFingerprints (rules_version.go) hashes the rule
// CONSTANTS this package declares and catches a renamed or re-valued one. It
// cannot see a changed PREDICATE: validation logic whose behaviour changes
// with no Rule*, ADR*, or numeric rule constant touched. That gap let
// strategy.RulesVersion move 1.1.0 -> 1.2.0 and 1.2.0 -> 1.3.0 with the
// fingerprint unchanged both times, and nothing but a reviewer noticed.
//
// The decision corpus below closes that gap the same way the fingerprint
// closes its own: not by understanding what a rule change IS (that needs a
// person), but by recording what this package's reducer DECIDES for every
// scenario stream.run() (campaign_test.go) drives it through, and failing
// loudly the day a decision changes under an unchanged RulesVersion.
//
// Its limit, honestly: it catches a rule change only when some recorded
// scenario actually EXERCISES it. A predicate change with no scenario that
// reaches the state it guards is exactly as invisible here as it was to
// RuleSurfaceFingerprints — see acceptance test (b) in issue #100 for a
// worked example of checking whether that is the case for a given change.

// decisionCorpusDir holds one file per strategy.RulesVersion this package
// has ever declared, pinning {scenario name: hash}. Like
// strategy.RuleSurfaceFingerprints, it is append-only history: a file for a
// version already released is never rewritten, only ever added to for a new
// one (see decideDecisionCorpus).
const decisionCorpusDir = "testdata/decision-corpus"

// updateDecisionCorpus adds entries the current run recorded but the pinned
// file for strategy.RulesVersion does not yet have — a new test — and, on a
// full (unfiltered) run, drops entries the file has but this run did not
// record — a renamed or deleted test. It never overwrites a CHANGED hash:
// TestMain's comparison refuses that outright, flag or no flag, because an
// update path that rewrote a changed hash would make the whole guard
// decorative (see decideDecisionCorpus).
var updateDecisionCorpus = flag.Bool("update-decision-corpus", false,
	"add/drop internal/strategy decision-corpus entries for the current RulesVersion (never overwrites a changed hash)")

// decisionCorpusMu guards decisionCorpus and decisionCorpusCalls: stream.run
// is called from ordinary tests that may run under t.Parallel(), so every
// write to the shared corpus below must be synchronized.
var decisionCorpusMu sync.Mutex

// decisionCorpus accumulates, across every test in this package's run,
// scenario name -> decisionHash for each call to stream.run() (recordDecision
// below). It is compared against the pinned file for the current
// strategy.RulesVersion once every test has finished (TestMain).
var decisionCorpus = map[string]string{}

// decisionCorpusCalls counts how many times each test (by t.Name()) has
// driven the harness, so a test that calls stream.run() more than once —
// add_test.go's TestAChainedFillCannotReverseUnitOrder retries a corrected
// input after a rejected one, for example — records each call under its own
// key instead of the last one silently overwriting the first.
var decisionCorpusCalls = map[string]int{}

// recordDecision is stream.run()'s own hook (campaign_test.go): every call
// to the harness, successful or not, is recorded here before its result is
// returned to the caller, so a test's own assertions about what it got back
// never determine whether that run entered the corpus.
func recordDecision(t *testing.T, decisions []event.Envelope, runErr error) {
	t.Helper()
	hash := decisionHash(decisions, runErr)

	decisionCorpusMu.Lock()
	defer decisionCorpusMu.Unlock()

	name := t.Name()
	decisionCorpusCalls[name]++
	if n := decisionCorpusCalls[name]; n > 1 {
		name = fmt.Sprintf("%s#%d", name, n)
	}
	decisionCorpus[name] = hash
}

// decisionHash is one harness run's fingerprint: every decision envelope it
// emitted, in emission order, plus the run's own error text if it failed —
// so a rules change registers whether it changes what is decided or only
// whether the run is accepted at all (ADR 0016's 1.1.0 -> 1.2.0 bump was
// exactly the latter: an emission that used to be refused started being
// accepted).
//
// Each envelope contributes its canonical bytes (event.CanonicalBytes; ADR
// 0016) MINUS its strategy_version field — canonicalDecisionFields, below —
// so the hash cannot change merely because the version string labelling the
// decision did, which is the one field every decision in this package
// carries that names strategy.RulesVersion at all.
func decisionHash(decisions []event.Envelope, runErr error) string {
	fields := make([]map[string]any, len(decisions))
	for i, d := range decisions {
		fields[i] = canonicalDecisionFields(d)
	}
	payload := map[string]any{
		// A JSON array preserves element order (writeCanonicalSlice), which
		// is what lets emission order, not just emission content, affect
		// the hash.
		"decisions": fields,
		"failed":    runErr != nil,
	}
	if runErr != nil {
		payload["error"] = runErr.Error()
	}
	sum := event.CanonicalBytes(payload)
	return event.HashPayload(sum)
}

// canonicalDecisionFields is event.CanonicalEnvelopeBytes' own field map
// (canonical_envelope.go) with "strategy_version" removed. It cannot import
// that unexported map directly, so it restates the same field set; the two
// covering different fields would be a silent way for a decision-corpus
// change to stop covering an envelope field CanonicalEnvelopeBytes covers,
// which is why both are exercised by
// TestCanonicalDecisionFieldsCoverEveryEnvelopeFieldButStrategyVersion.
func canonicalDecisionFields(e event.Envelope) map[string]any {
	return map[string]any{
		"id":                 e.ID,
		"type":               e.Type,
		"envelope_version":   e.EnvelopeVersion,
		"schema_version":     e.SchemaVersion,
		"event_time":         e.EventTime.UTC().Format(rfc3339Nano),
		"recorded_at":        e.RecordedAt.UTC().Format(rfc3339Nano),
		"sequence":           e.Sequence,
		"correlation_id":     e.CorrelationID,
		"causation_id":       e.CausationID,
		"source":             e.Source,
		"configuration_hash": e.ConfigurationHash,
		"payload_hash":       e.PayloadHash,
		"payload":            string(e.Payload),
		// strategy_version deliberately excluded (see decisionHash's doc
		// comment).
	}
}

const rfc3339Nano = "2006-01-02T15:04:05.999999999Z07:00"

// decisionCorpusPath is the pinned file for a RulesVersion.
func decisionCorpusPath(rulesVersion string) string {
	return filepath.Join(decisionCorpusDir, rulesVersion+".json")
}

// readDecisionCorpusFile reads path, reporting found=false rather than an
// error when it does not exist yet — the state a version has right after
// being bumped, before anyone has run -update-decision-corpus for it.
func readDecisionCorpusFile(path string) (data map[string]string, found bool, err error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var m map[string]string
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return nil, false, fmt.Errorf("decode %s: %w", path, err)
	}
	// One JSON object and nothing after it. A second value, or any trailing
	// bytes, means the file is not what -update-decision-corpus wrote, and a
	// guard that silently ignored the rest would be trusting evidence it had
	// not read.
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, false, fmt.Errorf("decode %s: trailing data after the corpus object", path)
	}
	return m, true, nil
}

// writeDecisionCorpusFile writes m as sorted, stably formatted JSON.
// encoding/json already sorts a map[string]string's keys, which is what
// "sorted keys, stable formatting" (#100) needs without a second, bespoke
// encoder.
func writeDecisionCorpusFile(path string, m map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// decisionCorpusFullRun reports whether this test process ran the whole
// package's tests, unfiltered — the only condition under which a scenario
// pinned in the corpus but not recorded this run can be trusted to mean
// "renamed or deleted" rather than "not selected". Three flags select a
// subset: -run (test.run) and -skip (test.skip), each naming a pattern, and
// -short (test.short), which some tests in this package may honour by
// skipping themselves (none do today, but nothing stops one from starting
// to). Any one of them means this run cannot support "not recorded implies
// gone" — completeness is judged only on a plain, filterless `go test`.
// That is necessary, not sufficient: decideDecisionCorpus also requires the
// run to have passed, since a failing or -failfast-stopped test may never
// have reached stream.run.
//
// A pattern matching every test (e.g. -run '.*') is still treated as filtered:
// this reads the flag's presence, not what it happens to match, which is
// the honest side of a real trade-off — false positives here would fail a
// -run-scoped debugging session for scenarios it never touched; the cost is
// a genuinely-complete -run invocation not getting the completeness check
// it would have earned. `make check`'s own `go test ./...` passes none
// of these flags, so this never weakens what CI enforces.
func decisionCorpusFullRun() bool {
	for _, name := range []string{"test.run", "test.skip"} {
		if f := flag.Lookup(name); f != nil && f.Value.String() != "" {
			return false
		}
	}
	return !testing.Short()
}

// decisionCorpusCountIsOne reports whether each test runs exactly once in
// this process. With -count=N every test runs N times and stream.run's
// per-name call counter keeps climbing across iterations, so the second
// iteration's first call is recorded as "Name#2" — a key that means "the
// second call within one run" everywhere else. Those keys cannot be told
// apart from genuine repeat calls, so the corpus is not checked at all on
// such a run rather than checked wrongly.
func decisionCorpusCountIsOne() bool {
	f := flag.Lookup("test.count")
	return f == nil || f.Value.String() == "1"
}

// snapshotDecisionCorpus copies the corpus recorded so far, so the rest of
// the comparison need not hold decisionCorpusMu.
func snapshotDecisionCorpus() map[string]string {
	decisionCorpusMu.Lock()
	defer decisionCorpusMu.Unlock()
	out := make(map[string]string, len(decisionCorpus))
	for k, v := range decisionCorpus {
		out[k] = v
	}
	return out
}

// changedScenarios returns, sorted, every name recorded in BOTH existing and
// recorded whose hash disagrees: a scenario the pinned file already speaks
// for, decided differently now.
func changedScenarios(existing, recorded map[string]string) []string {
	var names []string
	for name, hash := range recorded {
		if want, ok := existing[name]; ok && want != hash {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// newScenarios returns, sorted, every name this run recorded that the
// pinned file does not have at all: a new test.
func newScenarios(existing, recorded map[string]string) []string {
	var names []string
	for name := range recorded {
		if _, ok := existing[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// droppedScenarios returns, sorted, every name the pinned file has that this
// run did not record at all: a renamed or deleted test, PROVIDED this run
// was complete (decisionCorpusFullRun) — the caller's responsibility to
// check, not this function's.
func droppedScenarios(existing, recorded map[string]string) []string {
	var names []string
	for name := range existing {
		if _, ok := recorded[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// decisionCorpusOutcome is decideDecisionCorpus's pure result: problems to
// report (a non-nil, non-empty slice fails the run) and, when update
// applies cleanly, the corpus file's new full contents (nil: leave the file
// alone).
type decisionCorpusOutcome struct {
	problems []string
	write    map[string]string
}

// decideDecisionCorpus is TestMain's comparison, factored out from all file
// I/O so it can be exercised directly against constructed inputs
// (decision_corpus_logic_test.go) rather than only through a real `go test`
// invocation.
//
// existing/found is the pinned file's own content for version, as
// readDecisionCorpusFile reports it (found=false: no file yet, the state
// right after a RulesVersion bump). recorded is what this run actually
// produced (snapshotDecisionCorpus). fullRun is decisionCorpusFullRun's
// answer; passed is whether every test in the run passed; update is
// *updateDecisionCorpus.
//
// Two rules keep the corpus append-only in the sense that matters — no
// scenario loses its pinned entry except by being genuinely renamed or
// deleted:
//
//   - Nothing is ever DROPPED unless the run was both unfiltered and
//     passing. A failing test, or one -failfast stopped before it ran, may
//     never have reached stream.run, and "not recorded" would then mean
//     "did not get that far", not "gone".
//   - Nothing is WRITTEN at all from a failing run. A corpus is evidence of
//     what the reducer decides when its tests pass; a failing run is not.
func decideDecisionCorpus(existing map[string]string, found bool, recorded map[string]string, fullRun, passed, update bool, version, path string) decisionCorpusOutcome {
	changed := changedScenarios(existing, recorded)
	if len(changed) > 0 {
		msg := fmt.Sprintf(
			"decision corpus %s: the reducer's decisions CHANGED for %d scenario(s) while strategy.RulesVersion "+
				"stayed %q. Per ADR 0016 this is a rule change, not a replay/journal divergence: give it a "+
				"RulesVersion bump, a new %s-style corpus file for the new version, and a new row in "+
				"strategy.RuleSurfaceFingerprints. Changed scenario(s): %s",
			path, len(changed), version, decisionCorpusDir, strings.Join(changed, ", "))
		if update {
			msg += fmt.Sprintf(" -update-decision-corpus refuses to overwrite a changed hash in %s: that refusal "+
				"is the tripwire, so bump RulesVersion first.", path)
		}
		return decisionCorpusOutcome{problems: []string{msg}}
	}

	if update && !passed {
		return decisionCorpusOutcome{problems: []string{fmt.Sprintf(
			"decision corpus: refusing to update %s from a run with failing tests — a corpus records what the "+
				"reducer decides when its tests pass; fix the failures and rerun", path)}}
	}

	if !found {
		if !update {
			return decisionCorpusOutcome{problems: []string{fmt.Sprintf(
				"decision corpus: no pinned file %s for strategy.RulesVersion %q — run the whole package with "+
					"-update-decision-corpus to generate it", path, version)}}
		}
		if !fullRun {
			return decisionCorpusOutcome{problems: []string{fmt.Sprintf(
				"decision corpus: refusing to generate %s from a filtered run (-run, -skip or -short excludes some "+
					"scenarios) — rerun the whole package, unfiltered, with -update-decision-corpus", path)}}
		}
		return decisionCorpusOutcome{write: recorded}
	}

	added := newScenarios(existing, recorded)
	complete := fullRun && passed
	var missing []string
	if complete {
		missing = droppedScenarios(existing, recorded)
	}

	var problems []string
	next := existing
	dirty := false
	if len(added) > 0 {
		if update {
			next = cloneStringMap(next)
			for _, name := range added {
				next[name] = recorded[name]
			}
			dirty = true
		} else {
			problems = append(problems, fmt.Sprintf(
				"decision corpus %s: %d scenario(s) were recorded but are not pinned (a new test) — rerun with "+
					"-update-decision-corpus to ADD them (it never rewrites an existing entry). Before you do: if any of "+
					"these scenarios pins behaviour the previous build did not have, it is a new rule arriving with its "+
					"own tests, which this corpus cannot tell from a new test of an old rule — that is a RulesVersion "+
					"bump (ADR 0016), not an add. New scenario(s): %s",
				path, len(added), strings.Join(added, ", ")))
		}
	}
	if complete && len(missing) > 0 {
		if update {
			next = cloneStringMap(next)
			for _, name := range missing {
				delete(next, name)
			}
			dirty = true
		} else {
			problems = append(problems, fmt.Sprintf(
				"decision corpus %s: %d pinned scenario(s) were not recorded by this full run (a renamed or "+
					"deleted test) — rerun with -update-decision-corpus to drop them. Missing scenario(s): %s",
				path, len(missing), strings.Join(missing, ", ")))
		}
	}

	outcome := decisionCorpusOutcome{problems: problems}
	if dirty {
		outcome.write = next
	}
	return outcome
}

func cloneStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// compareDecisionCorpus is decideDecisionCorpus wired to real file I/O, for
// TestMain to call after every test has run.
func compareDecisionCorpus(passed bool) []string {
	version := strategy.RulesVersion
	path := decisionCorpusPath(version)

	existing, found, err := readDecisionCorpusFile(path)
	if err != nil {
		return []string{fmt.Sprintf("decision corpus: %v", err)}
	}

	recorded := snapshotDecisionCorpus()
	fullRun := decisionCorpusFullRun()
	outcome := decideDecisionCorpus(existing, found, recorded, fullRun, passed, *updateDecisionCorpus, version, path)

	if outcome.write != nil {
		if err := writeDecisionCorpusFile(path, outcome.write); err != nil {
			outcome.problems = append(outcome.problems, fmt.Sprintf("decision corpus: write %s: %v", path, err))
		}
	}
	return outcome.problems
}

// TestMain runs every test in the package first, exactly as if it were
// absent, then compares what stream.run() recorded (decisionCorpus) against
// the pinned file for the CURRENT strategy.RulesVersion (compareDecisionCorpus)
// and fails the process if that comparison reports a problem — after every
// test has already reported its own, so a decision-corpus failure is never
// mistaken for, or hidden behind, an ordinary test failure.
func TestMain(m *testing.M) {
	code := m.Run()

	if !decisionCorpusCountIsOne() {
		fmt.Fprintln(os.Stderr, "decision corpus: not checked — -count is not 1, and repeated iterations cannot be told apart from repeated calls within one run")
		os.Exit(code)
	}

	if problems := compareDecisionCorpus(code == 0); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, p)
		}
		if code == 0 {
			code = 1
		}
	}

	os.Exit(code)
}
