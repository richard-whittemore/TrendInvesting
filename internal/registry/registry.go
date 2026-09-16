// Package registry records every backtest run under the configuration hash
// that identifies it, so that the graveyard of failed and abandoned runs
// survives beside the successful ones and a Variant that is still standing
// cannot look more special than it is (ADR 0012).
//
// # One file per run, and no index
//
// A run is recorded as a single file named for its run id, in a directory
// named for its configuration hash. The layout IS the index: there is no
// manifest, no catalogue and no aggregate file, because every one of those
// would be a single mutable blob that two runs recorded on two branches would
// have to merge by hand. Two runs never write the same path, so two branches
// that each recorded one merge as two additions and a run recorded on either
// cannot clobber the other.
//
// Nothing here rewrites or deletes an existing entry (ADR 0018). The
// exclusive create that enforces that is the caller's, because this package
// performs no I/O of its own; see cmd/backtest.
//
// # The hash is derived, never stated
//
// A caller supplies the configuration and never its hash: NewEntry derives it
// with event.ConfigurationHash (ADR 0016), and Validate re-derives it on the
// way back in, so an entry cannot be re-attributed to a different Variant by
// editing one string.
package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// A Store is a run registry as it sits on disk: the names recorded in one
// configuration's directory, and the bytes of one recorded run. It is the
// repository interface the domain defines and something outside it
// implements, which is what keeps this package free of I/O
// (docs/development.md's package boundaries); cmd/backtest implements it over
// a directory.
//
// Deliberately narrower than fs.FS, which would be the obvious choice: the
// registry needs file NAMES and file BYTES and nothing else, while fs.ReadDir
// hands back fs.DirEntry values carrying file metadata this package never
// looks at. Names and bytes are the whole contract.
type Store interface {
	// ReadDir returns the names of the files recorded directly in dir, in
	// any order. A dir that does not exist must report an error satisfying
	// errors.Is(err, fs.ErrNotExist), because a configuration nothing has
	// been run under is an empty answer rather than a failure.
	ReadDir(dir string) ([]string, error)
	// ReadFile returns the bytes of the recorded run at name.
	ReadFile(name string) ([]byte, error)
}

// FormatVersion is the entry format this build writes and can read. Decode
// rejects any other value in both directions, fail closed, because no
// upcaster is implemented — the same rule ADR 0015 applies to the envelope.
const FormatVersion uint32 = 1

// entrySuffix is the extension every recorded run carries, so that a note or
// an editor's backup file sitting beside the runs is not read as one.
const entrySuffix = ".json"

// maxRunIDLength bounds a run id so that it, plus the suffix, is a legal file
// name on every filesystem the registry is cloned onto.
const maxRunIDLength = 128

// Baseline is the Variant identifier of a run of the Baseline itself
// (CONTEXT.md: "Baseline", "Variant"). Every run states one or the other;
// there is no unattributed run.
const Baseline = "baseline"

// Status is what became of a run. The vocabulary is closed and describes the
// RUN, not the verdict on it: whether a Variant is adopted or rejected is a
// judgement made over many runs against ADR 0012's five criteria, and
// recording it here would let a run's own record assert its own conclusion.
type Status string

// The three outcomes a run can have. All three are retained: ADR 0012's
// "no Variant wins" is a legitimate outcome, and it is only legible if the
// runs that produced it are still there.
const (
	// StatusCompleted: the run reached the end of its input stream and wrote
	// a journal.
	StatusCompleted Status = "completed"
	// StatusFailed: the run stopped before the end of its input stream.
	// Whatever partial evidence it left is retained with it.
	StatusFailed Status = "failed"
	// StatusAbandoned: the run was declared and deliberately not carried
	// through, or its output discarded.
	StatusAbandoned Status = "abandoned"
)

func (s Status) recognised() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusAbandoned:
		return true
	default:
		return false
	}
}

// Artefacts is where a run's evidence lives.
//
// FinalRecordHash and RecordCount are the journal's own chain head, anchored
// here so the git history attests it from outside the journal — which is what
// turns ADR 0017's tamper-evidence from "detectable if you kept the original"
// into "detectable, full stop", and closes its end-truncation gap. A journal
// recorded without them is an unanchored artefact and is refused.
type Artefacts struct {
	// JournalPath is where the run's journal lives, RELATIVE to the registry
	// root and slash-separated. The registry is committed to git and read
	// wherever it is cloned, so an absolute path here would name a location
	// that exists on exactly one machine; one is refused rather than stored.
	JournalPath     string `json:"journal_path"`
	RecordCount     uint64 `json:"record_count"`
	FinalRecordHash string `json:"final_record_hash"`
}

// Run is what a caller states about one backtest: enough to reproduce it
// exactly, and what became of it.
//
// Configuration is carried whole, not only as its hash, because a hash
// identifies a run and does not reproduce one. SpanStart and SpanEnd are the
// first and last input event time the run covered, and are absent only on a
// run that processed no input at all.
type Run struct {
	RunID           string                     `json:"run_id"`
	Variant         string                     `json:"variant"`
	Status          Status                     `json:"status"`
	StrategyVersion string                     `json:"strategy_version"`
	SpanStart       time.Time                  `json:"span_start"`
	SpanEnd         time.Time                  `json:"span_end"`
	Configuration   event.ConfigurationPayload `json:"configuration"`
	Artefacts       Artefacts                  `json:"artefacts"`
	// Detail says why a run ended as it did. Required of a failed or
	// abandoned run: a status with no reason records that something went
	// wrong and nothing about what, which is the shape a curated record
	// takes.
	Detail string `json:"detail"`
}

// Entry is one recorded run: a Run, plus the two facts the registry derives
// rather than accepts — the format version and the configuration hash.
type Entry struct {
	RegistryVersion   uint32 `json:"registry_version"`
	ConfigurationHash string `json:"configuration_hash"`
	Run
}

// NewEntry returns the registry entry for run, deriving its configuration
// hash from the configuration itself (ADR 0016), and refusing a run that may
// not be recorded at all.
func NewEntry(run Run) (Entry, error) {
	entry := Entry{
		RegistryVersion:   FormatVersion,
		ConfigurationHash: event.ConfigurationHash(run.Configuration),
		Run:               run,
	}
	if err := entry.Validate(); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

// Validate reports every way e may not be recorded, together.
//
// Two of the checks are the registry's own refusals rather than hygiene:
//
//   - A run stating zero (or negative, or non-finite) slippage is refused
//     whatever its status. ADR 0013 makes such a run invalid by construction
//     and names the run registry as what must reject it. Retention (ADR 0012)
//     is not a reason to record one: nothing was produced that is evidence of
//     anything. The check is stated here rather than left to
//     ConfigurationPayload.Validate below so that relaxing the payload
//     contract cannot quietly relax this.
//   - The configuration hash must be the one event.ConfigurationHash derives
//     from the configuration beside it. Re-deriving on the way in is what
//     stops a run being re-attributed to a different Variant by an edit to one
//     string.
func (e Entry) Validate() error {
	var errs []error

	switch {
	case e.RegistryVersion > FormatVersion:
		errs = append(errs, fmt.Errorf("registry version %d is newer than the version %d this build reads", e.RegistryVersion, FormatVersion))
	case e.RegistryVersion < FormatVersion:
		errs = append(errs, fmt.Errorf("registry version %d is older than the version %d this build reads, and no upcaster is registered", e.RegistryVersion, FormatVersion))
	}

	if err := checkRunID(e.RunID); err != nil {
		errs = append(errs, err)
	}
	if e.Variant == "" {
		errs = append(errs, fmt.Errorf("a run states the Variant it ran, or %q for the Baseline itself; unstated is how a Variant's results come to be read as the Baseline's", Baseline))
	}
	if !e.Status.recognised() {
		errs = append(errs, fmt.Errorf("status %q is not %q, %q or %q", e.Status, StatusCompleted, StatusFailed, StatusAbandoned))
	}

	if math.IsNaN(e.Configuration.SlippageN) || e.Configuration.SlippageN <= 0 {
		errs = append(errs, fmt.Errorf("the run states slippage %v: a run with zero slippage is invalid by construction and is refused whatever its status (ADR 0013)", e.Configuration.SlippageN))
	}
	if err := e.Configuration.Validate(); err != nil {
		errs = append(errs, err)
	}
	if derived := event.ConfigurationHash(e.Configuration); e.ConfigurationHash != derived {
		errs = append(errs, fmt.Errorf("the run is recorded under configuration %q, but the configuration recorded beside it hashes to %q", e.ConfigurationHash, derived))
	}

	strategyID, _, _, err := event.DecomposeStrategyVersion(e.StrategyVersion)
	switch {
	case err != nil:
		errs = append(errs, err)
	case strategyID != e.Configuration.StrategyID:
		errs = append(errs, fmt.Errorf("the run states strategy version %q, but the configuration it records declares strategy %q", e.StrategyVersion, e.Configuration.StrategyID))
	}

	errs = append(errs, e.checkSpan()...)
	errs = append(errs, e.checkArtefacts()...)

	if e.Detail == "" && (e.Status == StatusFailed || e.Status == StatusAbandoned) {
		errs = append(errs, fmt.Errorf("a %s run states why it ended; a status with no reason is not evidence of anything", e.Status))
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("registry: invalid run entry: %w", err)
	}
	return nil
}

// checkSpan: a completed run covered a span and states it, because a result
// that cannot be placed in a Regime Window cannot be evaluated (ADR 0012). A
// run that processed no input has no span to state, and states neither end.
//
// A span the entry cannot be WRITTEN DOWN with is refused here rather than at
// the install. Validate reports every way an entry may not be recorded, so an
// entry it accepts and Encode cannot write is a contract disagreeing with
// itself — and the disagreement surfaces as a JSON encoder message at the
// moment the run is being recorded, which is the worst moment for a registry
// whose point is that every run is recorded (ADR 0012).
func (e Entry) checkSpan() []error {
	switch {
	case e.SpanStart.IsZero() != e.SpanEnd.IsZero():
		return []error{errors.New("the span the run covered is stated at one end only")}
	case e.SpanEnd.Before(e.SpanStart):
		return []error{fmt.Errorf("the span runs backwards: %s to %s", e.SpanStart.Format(time.RFC3339), e.SpanEnd.Format(time.RFC3339))}
	case e.SpanStart.IsZero() && e.Status == StatusCompleted:
		return []error{errors.New("a completed run states the span of input event times it covered")}
	case !writableTime(e.SpanStart) || !writableTime(e.SpanEnd):
		return []error{fmt.Errorf("the span %s to %s cannot be written as RFC 3339, so the entry cannot be recorded", e.SpanStart.Format(time.RFC3339), e.SpanEnd.Format(time.RFC3339))}
	}
	return nil
}

// writableTime reports whether t survives being written as RFC 3339, which is
// how a recorded run states the span it covered and the only encoding of a
// time this format has.
//
// It asks the encoder rather than restating the encoder's rules. Restating
// them is how this check was wrong twice: first it tested nothing at all, then
// it tested only the year, and time.Time.MarshalJSON also refuses a timezone
// offset outside [0,23] hours. A validator that predicts a serialiser must be
// re-derived every time the serialiser changes; one that calls it cannot
// disagree with it.
func writableTime(t time.Time) bool {
	_, err := t.MarshalJSON()
	return err == nil
}

// checkArtefacts: a completed run points at the journal it wrote, because a
// result whose evidence is missing is a claim about a result rather than one.
// A failed or abandoned run may legitimately have left no journal.
func (e Entry) checkArtefacts() []error {
	var errs []error
	switch {
	case e.Artefacts.JournalPath == "":
		if e.Artefacts.FinalRecordHash != "" || e.Artefacts.RecordCount != 0 {
			errs = append(errs, errors.New("the run states a journal's chain head and record count but no journal for them to belong to"))
		}
		if e.Status == StatusCompleted {
			errs = append(errs, errors.New("a completed run records the journal it wrote; a result whose evidence is missing is a claim, not a result"))
		}
	case e.Artefacts.FinalRecordHash == "" || e.Artefacts.RecordCount == 0:
		errs = append(errs, errors.New("the run records a journal without the final record hash and record count that anchor its chain from outside it (ADR 0017)"))
	default:
		if err := checkJournalPath(e.Artefacts.JournalPath); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// checkJournalPath: a journal path is recorded relative to the registry root
// and slash-separated, so that it means the same thing wherever the registry
// is cloned. An absolute path, a volume name or a backslash all name a
// location on one machine, and the registry is committed to git precisely so
// that it can be read on another.
func checkJournalPath(journalPath string) error {
	switch {
	case strings.HasPrefix(journalPath, "/"):
		return fmt.Errorf("journal path %q is absolute; a journal is recorded relative to the registry root so the entry means the same thing wherever the registry is cloned", journalPath)
	case strings.ContainsAny(journalPath, `\:`):
		return fmt.Errorf("journal path %q names a volume or uses a backslash; a journal is recorded as a slash-separated path relative to the registry root", journalPath)
	}
	return nil
}

// deviceNames are the MS-DOS device names Win32 resolves ahead of a file of
// the same name, with or without an extension. A run id is checked against
// them on every platform: the registry is committed to git and cloned onto
// whatever machine reads it, so an id that cannot be a file name there is
// refused where it is chosen rather than where it is unusable.
var deviceNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com0": true, "com1": true, "com2": true, "com3": true, "com4": true,
	"com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt0": true, "lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true,
	"lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// checkRunID: the run id becomes a file name, so it is held to what survives
// being one. Upper case is refused because a case-insensitive filesystem
// would collide two ids that a case-sensitive one keeps apart, and a run
// recorded on one machine would then overwrite a different run on another.
func checkRunID(runID string) error {
	if runID == "" {
		return errors.New("a run states its own identifier; it is the name of the file the run is recorded in")
	}
	if len(runID) > maxRunIDLength {
		return fmt.Errorf("run id %q is %d bytes, longer than the %d a file name holds", runID, len(runID), maxRunIDLength)
	}
	for i := 0; i < len(runID); i++ {
		c := runID[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(runID)-1:
		default:
			return fmt.Errorf("run id %q holds %q: a run id may hold only lower-case letters, digits and interior hyphens, because it becomes a file name", runID, string(c))
		}
	}
	if deviceNames[runID] {
		return fmt.Errorf("run id %q names an MS-DOS device: Windows resolves it as one whatever extension follows, so %q could not be created there", runID, runID+entrySuffix)
	}
	return nil
}

// sha256Algorithm is the algorithm event.ConfigurationHash derives a hash
// with, and sha256DigestLength the hex length of what it produces (ADR 0016).
const (
	sha256Algorithm    = "sha256"
	sha256DigestLength = 64
)

// Dir is the directory, relative to a registry root, that every run of
// configurationHash is recorded in: the hash with its algorithm separator
// replaced, since ":" is not a legal file name character everywhere the
// registry is cloned.
//
// It refuses anything that is not a hash this build derives, digest and all.
// The argument reaches this from an operator's command line, and a directory
// name is the registry's whole index: a malformed selector that became a path
// would name a directory that happens not to exist, and Runs reports a
// missing directory as no runs. For an audit tool, answering "nothing here"
// to a malformed question is the wrong failure — a mistyped hash would read
// as evidence that a Variant was never run, which is the one thing the
// registry exists to make impossible (ADR 0012).
//
// An algorithm other than the one ADR 0016 derives is refused rather than
// carried opaquely: this build computes exactly one, so an unrecognised
// algorithm is a selector it cannot honour, and failing closed is the same
// rule ADR 0015 applies to a version no upcaster exists for.
func Dir(configurationHash string) (string, error) {
	algorithm, digest, ok := strings.Cut(configurationHash, ":")
	if !ok {
		return "", fmt.Errorf("registry: configuration hash %q does not name the algorithm that produced it, which ADR 0016 prefixes", configurationHash)
	}
	if algorithm != sha256Algorithm {
		return "", fmt.Errorf("registry: configuration hash %q names algorithm %q, and this build derives only %q (ADR 0016)", configurationHash, algorithm, sha256Algorithm)
	}
	if err := checkDigest(configurationHash, digest); err != nil {
		return "", err
	}
	return dir(algorithm, digest), nil
}

func dir(algorithm, digest string) string {
	return algorithm + "-" + digest
}

// checkDigest holds a sha256 digest to exactly what ADR 0016's hex encoding
// produces: 64 lower-case hexadecimal characters. Upper case is refused for
// the reason a run id is — a case-insensitive filesystem would map two
// selectors onto one directory.
func checkDigest(configurationHash, digest string) error {
	if len(digest) != sha256DigestLength {
		return fmt.Errorf("registry: configuration hash %q carries a %d-character digest; a %s digest is exactly %d lower-case hexadecimal characters (ADR 0016)", configurationHash, len(digest), sha256Algorithm, sha256DigestLength)
	}
	for i := 0; i < len(digest); i++ {
		c := digest[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return fmt.Errorf("registry: configuration hash %q holds %q in its digest; a %s digest is exactly %d lower-case hexadecimal characters (ADR 0016)", configurationHash, string(c), sha256Algorithm, sha256DigestLength)
	}
	return nil
}

// Path is the file, relative to a registry root, that e occupies. It is a
// slash-separated path: the registry is committed to git and read on whatever
// machine clones it.
func (e Entry) Path() (string, error) {
	if err := e.Validate(); err != nil {
		return "", err
	}
	// Validate has established that the hash is the one
	// event.ConfigurationHash derived from the configuration beside it, so
	// its shape needs no second check.
	algorithm, digest, _ := strings.Cut(e.ConfigurationHash, ":")
	return path.Join(dir(algorithm, digest), e.RunID+entrySuffix), nil
}

// Encode writes entry as the indented JSON one file holds, one field per
// line, because the registry is committed to git and read as a diff.
func Encode(w io.Writer, entry Entry) error {
	if err := entry.Validate(); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("registry: encode the run: %w", err)
	}
	if _, err := w.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("registry: write the run: %w", err)
	}
	return nil
}

// Decode reads one recorded run, failing closed on anything it does not
// understand: an unrecognised status, a field this build does not know, a
// second entry appended after the first, or a hash that does not follow from
// the configuration beside it.
//
// Failing closed on the way in is what makes the vocabulary closed in
// practice. A status stored as-is would let a run describe itself however it
// liked, and a later count of what failed would be wrong.
func Decode(r io.Reader) (Entry, error) {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()

	var entry Entry
	if err := decoder.Decode(&entry); err != nil {
		return Entry{}, fmt.Errorf("registry: decode the run: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Entry{}, errors.New("registry: the file holds more than one run; a run is recorded in a file of its own, which is what keeps two runs from writing the same path")
	}
	if err := entry.Validate(); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

// Runs returns every run recorded under configurationHash in store, in run-id
// order, whatever became of each: a failed or abandoned run is returned
// beside a completed one, never dropped.
//
// A configuration nothing has ever been run under has no runs, and that is an
// empty answer rather than a failure. A registry that cannot be READ is a
// failure, and is reported as one: "never run" and "could not be read" are
// different findings, and only one of them is evidence.
//
// An entry filed under a configuration other than its own, or in a file not
// named for its own run id, is refused rather than returned. The directory
// and the file name are the index, so an entry that disagrees with where it
// sits has lost the guarantee that makes the layout trustworthy.
func Runs(store Store, configurationHash string) ([]Entry, error) {
	directory, err := Dir(configurationHash)
	if err != nil {
		return nil, err
	}

	listed, err := store.ReadDir(directory)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("registry: read the runs recorded under %s: %w", configurationHash, err)
	}
	// Sorted here rather than trusted from the store, so the order two runs
	// come back in is the same wherever the registry is read.
	slices.Sort(listed)

	var entries []Entry
	for _, name := range listed {
		// Anything that is not a recorded run is left alone: a note beside
		// the runs, a subdirectory, and above all the temporary file an
		// install in progress is still writing.
		if !strings.HasSuffix(name, entrySuffix) {
			continue
		}
		entry, err := readEntry(store, path.Join(directory, name))
		if err != nil {
			return nil, err
		}
		if entry.ConfigurationHash != configurationHash {
			return nil, fmt.Errorf("registry: the run recorded in %s states configuration %s, but it is filed under %s", path.Join(directory, name), entry.ConfigurationHash, configurationHash)
		}
		if want := entry.RunID + entrySuffix; name != want {
			return nil, fmt.Errorf("registry: the run recorded in %s calls itself %q, so it belongs in %s", path.Join(directory, name), entry.RunID, want)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func readEntry(store Store, name string) (Entry, error) {
	raw, err := store.ReadFile(name)
	if err != nil {
		return Entry{}, fmt.Errorf("registry: read the run recorded in %s: %w", name, err)
	}
	entry, err := Decode(bytes.NewReader(raw))
	if err != nil {
		return Entry{}, fmt.Errorf("registry: %s: %w", name, err)
	}
	return entry, nil
}
