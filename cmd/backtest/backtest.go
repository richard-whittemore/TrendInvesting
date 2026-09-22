package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/registry"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// sourceFixture is stamped on every input this command produces. The bars
// and the configuration come from a file, not from the strategy or the
// simulator, and Source is what a journal reader uses to tell them apart
// (docs/architecture.md).
const sourceFixture = "fixture"

// options is one invocation of the backtest.
//
// build identifies the running build and is the only part of a run's
// identity that comes from outside the configuration: the strategy version
// is composed from the configuration's own StrategyID, the rules version
// declared in code, and it (ADR 0016). main fills it from buildinfo.Version.
// It is a parameter rather than a package read so that a test asserting a
// journal byte for byte can fix it — a golden keyed to the build identifier
// would assert which machine produced the journal rather than what the
// platform decided, and would fail on every release build and every new
// machine.
//
// registryPath, runID and variant describe how the run is recorded (ADR
// 0012). An empty registryPath records nothing: pointing at a registry is the
// operator's decision, which is why the zero-slippage refusal does not live
// in the registry alone — see readConfiguration.
type options struct {
	configPath string
	barsPath   string
	// corporateActionsPath is a JSON array of event.CorporateActionPayload,
	// the fixture-driven input path for a corporate action (CONTEXT.md:
	// "Delisting Exit"). Empty names no fixture, and a run given none
	// behaves exactly as one with no corporate-action input in its stream at
	// all: this field, unset, is the zero value every existing caller of
	// options already passes.
	corporateActionsPath string
	outPath              string
	registryPath         string
	runID                string
	variant              string
	build                string
	// maxRecords bounds the records this run holds in memory before its
	// journal is written. Zero is an invocation that named no bound.
	maxRecords int
}

// recordBound bounds the records this run holds in memory before its journal
// is written (ADR 0017), from the invocation or the default when it named
// none. The bound is checked between inputs, so a run exceeds it by the
// decisions of the input that reached it, and by no more than
// journal.MaxEmissionsPerInput.
func (o options) recordBound() int {
	if o.maxRecords < 1 {
		return journal.DefaultMaxRecords
	}
	return o.maxRecords
}

// backtest runs the configuration at opts.configPath over the bars at
// opts.barsPath and writes the journal to opts.outPath.
//
// The composition, in order: the configuration event, then each completed
// bar through the per-bar protocol, then the end-of-stream event that
// resolves whatever is still outstanding. Every input goes through the fill
// simulator so that one component numbers the composed stream — a
// configuration event applied around RunBar rather than through it would
// leave a gap replay.Engine.Run refuses.
func backtest(ctx context.Context, opts options, out io.Writer) error {
	if opts.build == "" {
		return errors.New("backtest: the running build must be identified; it is part of every envelope's strategy version (ADR 0016)")
	}
	// Checked before the run rather than after it, so an operator is told
	// the path is taken in a moment rather than at the end of a backtest.
	// This is a courtesy, not the guarantee: writeJournal creates the
	// destination exclusively, because a journal can appear while a run is
	// in progress.
	if err := checkJournalPathFree(opts.outPath); err != nil {
		return err
	}
	cfg, err := readConfiguration(opts.configPath)
	if err != nil {
		return err
	}

	// Both derived from the configuration actually being run, never supplied
	// as a string (ADR 0016); only the build identifier comes from outside.
	// Composed here rather than after the bars are read, so that a run which
	// fails before its first bar still has the strategy version every entry
	// states.
	configurationHash := event.ConfigurationHash(cfg)
	strategyVersion := event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, opts.build)

	result := perform(ctx, opts, cfg, configurationHash, strategyVersion)

	// Recorded whatever became of the run, and before the failure is
	// returned: the graveyard of failed and abandoned runs is the point of
	// the registry (ADR 0012), and a run that is only registered when it went
	// well is a curated record.
	if err := registerRun(opts, cfg, strategyVersion, result); err != nil {
		return errors.Join(result.failure(), err)
	}
	if !result.installed {
		return result.failure()
	}

	report := fmt.Sprintf("wrote %s: %d record(s) over %s to %s\n",
		opts.outPath, result.records,
		result.header.SpanStart.UTC().Format(time.RFC3339), result.header.SpanEnd.UTC().Format(time.RFC3339))
	if _, err := io.WriteString(out, report); err != nil {
		return errors.Join(result.failure(), fmt.Errorf("backtest: report the run: %w", err))
	}
	return result.failure()
}

// outcome is what became of a run whose configuration this command accepted.
//
// installed answers a question nothing else can: whether the journal now
// sitting at opts.outPath was written BY THIS PROCESS. It is not "journalErr
// is nil" — the hard link that installs a journal is its commit point, and
// the flush after it is a second fact, so a run can hold a journal it
// installed and an error describing what happened next.
type outcome struct {
	header     journal.Header
	records    int
	runErr     error
	journalErr error
	installed  bool
}

// failure is everything that went wrong, as one error.
func (o outcome) failure() error { return errors.Join(o.runErr, o.journalErr) }

// status is what the registry records this run as (ADR 0012's vocabulary).
//
// A cancelled context is the operator's interrupt: a run deliberately not
// carried through is abandoned, not failed. Otherwise the status describes
// the RUN and not the bookkeeping around it — a run that reached the end of
// its input stream and installed its journal is completed even when a later
// step failed, because "failed" states that the run stopped before the end of
// its input stream and it did not. Every failure is recorded in Detail
// regardless, and still fails the command.
func (o outcome) status() registry.Status {
	switch {
	case errors.Is(o.runErr, context.Canceled):
		return registry.StatusAbandoned
	case o.runErr != nil, !o.installed:
		return registry.StatusFailed
	default:
		return registry.StatusCompleted
	}
}

// perform runs the configuration this command has already accepted, and
// reports what became of it rather than returning at the first failure.
//
// Every path out of here is a run that HAPPENED: the configuration was read
// and accepted, so the operator asked for work and something was attempted,
// even if the bar fixture turned out to be unreadable. Returning early from
// any of them is what used to leave a valid configuration with an unreadable
// bars file recorded nowhere at all.
//
// The line sits at the configuration, not earlier. A run refused before its
// configuration was accepted produced nothing — the zero-slippage refusal
// (ADR 0013), an unreadable or invalid configuration, an occupied journal
// path — and an entry for it would assert that a run was performed.
func perform(ctx context.Context, opts options, cfg event.ConfigurationPayload, configurationHash, strategyVersion string) outcome {
	bars, err := readBars(opts.barsPath)
	if err != nil {
		return outcome{runErr: err}
	}
	corporateActions, err := readCorporateActions(opts.corporateActionsPath)
	if err != nil {
		return outcome{runErr: err}
	}
	reducer, err := strategy.NewReducer(strategyVersion, cfg)
	if err != nil {
		return outcome{runErr: fmt.Errorf("backtest: %w", err)}
	}
	simulator, err := fills.New(cfg, strategyVersion, configurationHash)
	if err != nil {
		return outcome{runErr: fmt.Errorf("backtest: %w", err)}
	}
	recorder := journal.NewBoundedRecorder(reducer, opts.recordBound())

	result := outcome{runErr: namingTheBoundFlag(drive(ctx, simulator, recorder, cfg, strategyVersion, bars, corporateActions))}

	// The journal is written whether or not the run completed: a handler
	// that failed closed may have emitted a final event explaining why, and
	// that event is exactly the one a reviewer needs (replay.Handler's
	// contract). A run that stopped before its first input has nothing to
	// write, and is recorded with no artefacts rather than not at all.
	header, err := recorder.Header(configurationHash, strategyVersion)
	if err != nil {
		result.journalErr = err
		return result
	}
	result.header = header
	// Taken once: at the bound a second defensive copy would double the peak
	// footprint at the moment it is tightest.
	entries := recorder.Entries()
	result.records = len(entries)
	result.installed, result.journalErr = writeJournal(opts.outPath, header, entries)
	return result
}

// namingTheBoundFlag adds the flag that raises the record bound, which
// internal/journal cannot name: the bound belongs to the recorder and the
// flag to this command.
func namingTheBoundFlag(err error) error {
	var limit *journal.RecordLimitError
	if !errors.As(err, &limit) {
		return err
	}
	return fmt.Errorf("%w; -max-records raises it for this command", err)
}

// registerRun records the run in the registry opts names, if it names one.
//
// A run that went wrong is recorded rather than dropped: ADR 0012 retains
// every result, adopted, rejected and failed, under its configuration hash,
// and that graveyard is what stops a surviving Variant looking more special
// than it is. Curating it afterwards is what ADR 0018 forbids.
//
// # The journal is only this run's evidence if this run installed it
//
// Artefacts are attached only when THIS PROCESS installed the journal at
// opts.outPath (outcome.installed), never merely because no error was
// returned.
//
// A journal is installed by hard link, so a run can lose that link to a
// concurrent run and find a complete, valid, verifiable journal at its own
// destination — another run's. Anchoring that journal's path, record count
// and chain head to this run would produce a registry entry asserting that
// this run produced evidence it did not produce. That is a corrupted audit
// trail in the one artefact whose whole purpose is a trustworthy audit
// trail, and it is strictly worse than recording no artefacts at all. The
// journal header cannot settle it either: two runs of one configuration have
// identical headers, and a header carries no run id.
//
// Where they are attached, the chain head is read back from the file that
// actually landed rather than from what this process intended to write, so
// the value anchored in git attests the evidence rather than the intention
// (ADR 0017) — and a journal that this run installed and cannot then read
// back fails the command, whatever became of the run itself.
func registerRun(opts options, cfg event.ConfigurationPayload, strategyVersion string, result outcome) error {
	if opts.registryPath == "" {
		return nil
	}

	run := registry.Run{
		RunID:           opts.runID,
		Variant:         opts.variant,
		Status:          result.status(),
		StrategyVersion: strategyVersion,
		SpanStart:       result.header.SpanStart,
		SpanEnd:         result.header.SpanEnd,
		Configuration:   cfg,
	}
	if failure := result.failure(); failure != nil {
		run.Detail = failure.Error()
	}

	if result.installed {
		artefacts, err := anchorJournal(opts.registryPath, opts.outPath)
		if err != nil {
			return err
		}
		run.Artefacts = artefacts
	}

	entry, err := registry.NewEntry(run)
	if err != nil {
		return fmt.Errorf("backtest: %w", err)
	}
	return installEntry(opts.registryPath, entry)
}

// anchorJournal is what the registry records about the journal this run just
// installed: where it is, how many records it holds, and the chain head that
// anchors it from outside itself (ADR 0017).
//
// The path is recorded RELATIVE to the registry root, and slash-separated. An
// absolute path written into a git-committed registry names a location that
// exists on exactly one machine, so the entry would stop meaning anything the
// moment the repository was cloned. A journal and a registry root that cannot
// be expressed relative to one another — one absolute and one relative, or
// two volumes — are refused rather than recorded as an absolute path.
func anchorJournal(root, journalPath string) (registry.Artefacts, error) {
	relative, err := filepath.Rel(root, journalPath)
	if err != nil {
		return registry.Artefacts{}, fmt.Errorf("backtest: journal %s cannot be recorded relative to registry %s, so it would only mean anything on this machine; give both as absolute paths or both as paths relative to the same directory: %w", journalPath, root, err)
	}

	verification, err := verifyWritten(journalPath)
	if err != nil {
		return registry.Artefacts{}, fmt.Errorf("backtest: the journal this run wrote cannot be anchored in the registry: %w", err)
	}
	return registry.Artefacts{
		JournalPath:     filepath.ToSlash(relative),
		RecordCount:     verification.RecordCount,
		FinalRecordHash: verification.FinalRecordHash,
	}, nil
}

// verifyWritten reports what the journal at path says about itself.
func verifyWritten(path string) (journal.Verification, error) {
	file, err := os.Open(path)
	if err != nil {
		return journal.Verification{}, err
	}
	defer func() { _ = file.Close() }()
	return journal.Verify(file)
}

// installEntry writes entry into the registry rooted at root.
//
// The same exclusive install the journal uses: the entry is written to a
// temporary file in its own directory, flushed to disk, and HARD-LINKED into
// place. link(2) fails with EEXIST atomically, so a run id the registry
// already holds is refused rather than replaced, and two runs recorded at the
// same instant can neither interleave nor overwrite one another. rename(2)
// replaces a destination silently and must not be substituted for it.
//
// Two runs of different ids never contend at all: each writes a file of its
// own, which is also what lets two branches that each recorded a run merge
// without a conflict.
//
// The entry's CONTENT and its DIRECTORY ENTRY are both flushed. Syncing the
// file alone leaves the link itself — and the configuration-hash directory
// created to hold it — in the page cache, so a power loss just after a
// command reported success could come back with the run reported as recorded
// and nothing on disk to show for it. The registry is the durable record of
// what was run, so "reported as recorded" and "recorded" have to be the same
// thing.
//
// # The link commits, and the flush after it is a second fact
//
// That flush happens after the link, so a flush that fails leaves the entry
// installed. Both facts are therefore reported, never merged into one
// failure: the error says the entry IS recorded and where, because an
// operator told only that registration failed would record the same run again
// under another id, and two entries for one run is the corrupted audit trail
// the registry exists to avoid.
func installEntry(root string, entry registry.Entry) error {
	relative, err := entry.Path()
	if err != nil {
		return fmt.Errorf("backtest: %w", err)
	}
	destination := filepath.Join(root, filepath.FromSlash(relative))
	directory := filepath.Dir(destination)
	// Noted before the directories exist: a name is durable once the
	// directory HOLDING it is flushed, so the install has to know which
	// names it is about to create.
	creating := missingDirs(directory)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("backtest: create the registry directory: %w", err)
	}

	// Encoded before it is written, so the bytes this install would put on
	// disk are in hand to compare against whatever is already there.
	var encoded bytes.Buffer
	if err := registry.Encode(&encoded, entry); err != nil {
		return err
	}

	file, err := os.CreateTemp(directory, ".run-*.partial")
	if err != nil {
		return fmt.Errorf("backtest: create the registry entry: %w", err)
	}
	temporary := file.Name()
	// Every failure from here on removes the partial file, so the only way
	// anything lands at the destination is the link below.
	defer func() {
		_ = file.Close()
		_ = os.Remove(temporary)
	}()

	if _, err := file.Write(encoded.Bytes()); err != nil {
		return fmt.Errorf("backtest: write the registry entry: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("backtest: flush the registry entry to disk: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("backtest: close the registry entry: %w", err)
	}
	switch err := os.Link(temporary, destination); {
	case err == nil:
	case errors.Is(err, os.ErrExist):
		if err := alreadyRecorded(destination, encoded.Bytes(), entry.RunID); err != nil {
			return err
		}
	default:
		return fmt.Errorf("backtest: install the registry entry at %s: %w", destination, err)
	}
	for _, synced := range syncedAfter(directory, root, creating) {
		if err := syncDir(synced); err != nil {
			return fmt.Errorf("backtest: run %q is recorded at %s and can be read there now, but flushing the registry directory %s to disk failed, so the entry may not survive a power loss: the run is recorded and must not be recorded again under another id, and committing the registry to git is what makes the record durable (ADR 0017): %w", entry.RunID, destination, synced, err)
		}
	}
	return nil
}

// alreadyRecorded says what an entry already occupying destination means for
// the one being installed.
//
// Byte-identical content is the same record rather than a collision: nothing
// is written, so the never-overwrite rule (ADR 0018) is untouched,
// and an install that committed its link and then failed at a later step can
// be repeated instead of being refused. Anything else is a different run
// claiming a recorded id, and is refused.
//
// The comparison is on the bytes, never on a decode of each side: two entries
// that decode alike can differ on disk, and the registry is committed to git
// and read as a diff, so what is on disk is what "identical" has to mean.
func alreadyRecorded(destination string, installing []byte, runID string) error {
	recorded, err := os.ReadFile(destination)
	if err != nil {
		return fmt.Errorf("backtest: run %q is already recorded under this configuration, and the entry recorded at %s cannot be read to say whether it is this one: %w", runID, destination, err)
	}
	if !bytes.Equal(recorded, installing) {
		return fmt.Errorf("backtest: run %q is already recorded under this configuration, and what is recorded is a different run: a recorded run is evidence and is never overwritten (ADR 0018); record this one under another id", runID)
	}
	return nil
}

// missingDirs lists the directories MkdirAll must create to reach dir,
// innermost first: dir itself and every ancestor above it that is not there
// yet. It is called before the creation, so what it reports is what this
// command is about to become responsible for.
func missingDirs(dir string) []string {
	var missing []string
	for {
		if _, err := os.Stat(dir); err == nil {
			return missing
		}
		missing = append(missing, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			return missing
		}
		dir = parent
	}
}

// syncedAfter is every directory whose OWN ENTRIES an install changed,
// innermost first: the one the entry was linked into, the parent of every
// directory that had to be created to reach it, and the registry root.
//
// A directory entry is what a name IS, so flushing the entry's content and
// the directory holding it is not enough when this command also created that
// directory: the new directory's own name lives one level up, and an
// unflushed name there loses everything beneath it. MkdirAll creates as many
// levels as the operator's -registry asks for, so the chain runs up to the
// first ancestor that was already there and stops.
//
// The root is flushed whether or not this command created it, which is where
// the walk stops by policy rather than by necessity: above it the directories
// are the operator's and git's, not this command's.
func syncedAfter(directory, root string, created []string) []string {
	chain := make([]string, 0, len(created)+2)
	chain = append(chain, directory)
	for _, dir := range created {
		chain = append(chain, filepath.Dir(dir))
	}
	chain = append(chain, root)

	seen := make(map[string]bool, len(chain))
	ordered := make([]string, 0, len(chain))
	for _, dir := range chain {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		ordered = append(ordered, dir)
	}
	return ordered
}

// syncDir flushes a directory's own entries to disk, so that a name created
// in it survives a power loss rather than only the named file's contents
// doing so.
//
// Windows cannot flush a directory handle at all, so the failure is tolerated
// there rather than failing a command whose work is already done; on every
// platform this project runs on it is a real flush.
func syncDir(dir string) error {
	file, err := os.Open(dir)
	if err == nil {
		err = file.Sync()
		_ = file.Close()
	}
	if err != nil && runtime.GOOS == "windows" {
		return nil
	}
	return err
}

// drive applies the run's inputs in order.
//
// actions is interleaved among bars PER INSTRUMENT (CONTEXT.md: "Delisting
// Exit"): before a bar's own decision runs, every not-yet-delivered action
// naming that SAME instrument, whose EffectiveAt precedes the bar's own
// PeriodEnd, is delivered first, so a delisting reaches the reducer ahead of
// the bar decision it forces closed. Positioning is per instrument rather
// than against the bar stream as a whole because readBars promises only
// "the order the run delivers them", never global chronology: an
// instrument-grouped fixture (every bar of one instrument, then every bar of
// the next) is well-formed, and measuring an action against a bar of a
// DIFFERENT instrument would place it at the wrong point in its own
// instrument's history — a defect a single stream-wide cursor cannot avoid,
// however it is positioned. Nothing here judges whether a given action's
// EffectiveAt is stale relative to what the reducer has already accepted for
// its instrument — that is internal/strategy/delisting.go's applyDelisting
// chronology check, on the reducer's own state, and this command does not
// reimplement it.
func drive(ctx context.Context, simulator *fills.Simulator, recorder *journal.Recorder, cfg event.ConfigurationPayload, strategyVersion string, bars []event.CompletedBarPayload, actions []event.CorporateActionPayload) error {
	// The configuration event's own time is the first bar's period end: the
	// run's configuration is in force from the moment the run starts, and
	// this command has no clock to consult (nor would a recorded time from
	// one be reproducible).
	configuration, err := inputEnvelope("configuration:"+event.ConfigurationHash(cfg),
		event.ConfigurationEventType, event.ConfigurationSchemaVersion, bars[0].PeriodEnd, cfg, cfg, strategyVersion)
	if err != nil {
		return err
	}
	if _, err := fills.Deliver(ctx, simulator, recorder, configuration); err != nil {
		return fmt.Errorf("backtest: %w", err)
	}

	// ADR 0010's cash basis: the reducer sizes no Unit until an
	// account.snapshot has supplied an available-cash figure. This command
	// has no brokerage or LEAN feed to read one from, so a fixture-driven
	// backtest starts the run fully in cash, at the configuration's own
	// starting equity — the same assumption the Notional Account itself
	// makes before any snapshot arrives (ADR 0007).
	startingCash, err := inputEnvelope("account-snapshot:starting",
		event.AccountSnapshotEventType, event.AccountSnapshotSchemaVersion, bars[0].PeriodEnd,
		event.AccountSnapshotPayload{
			AsOf:          bars[0].PeriodEnd,
			Equity:        cfg.NotionalAccount.StartingEquity,
			AvailableCash: cfg.NotionalAccount.StartingEquity,
			Currency:      "USD",
		}, cfg, strategyVersion)
	if err != nil {
		return err
	}
	if _, err := fills.Deliver(ctx, simulator, recorder, startingCash); err != nil {
		return fmt.Errorf("backtest: %w", err)
	}

	// delivered tracks which of actions has already been delivered, index
	// for index; a run given no fixture (actions is nil) leaves it empty, so
	// both helpers below iterate zero times and the bar loop that follows is
	// byte-for-byte what it was before this flag existed.
	delivered := make([]bool, len(actions))
	for _, bar := range bars {
		if err := deliverActionsDueFor(ctx, simulator, recorder, cfg, strategyVersion, actions, delivered, bar.InstrumentID, bar.PeriodEnd); err != nil {
			return err
		}
		envelope, err := inputEnvelope("bar:"+bar.InstrumentID+":"+bar.PeriodEnd.UTC().Format(time.RFC3339Nano),
			event.CompletedBarEventType, event.CompletedBarSchemaVersion, bar.PeriodEnd, bar, cfg, strategyVersion)
		if err != nil {
			return err
		}
		if _, err := fills.RunBar(ctx, simulator, recorder, envelope); err != nil {
			return fmt.Errorf("backtest: %w", err)
		}
	}
	// Whatever is left is not before any bar this run holds for its own
	// instrument: an action effective at or after that instrument's own last
	// bar, or naming an instrument this run holds no bar for at all.
	if err := deliverRemainingActions(ctx, simulator, recorder, cfg, strategyVersion, actions, delivered); err != nil {
		return err
	}

	// Outstanding proposals expire at the last input time when no next bar
	// can end their one-bar lifetime (ADR 0011; event.RunCompletedEventType).
	completedAt := bars[len(bars)-1].PeriodEnd
	completed, err := inputEnvelope("run-completed:"+completedAt.UTC().Format(time.RFC3339Nano),
		event.RunCompletedEventType, event.RunCompletedSchemaVersion, completedAt,
		event.RunCompletedPayload{}, cfg, strategyVersion)
	if err != nil {
		return err
	}
	if _, err := fills.Deliver(ctx, simulator, recorder, completed); err != nil {
		return fmt.Errorf("backtest: %w", err)
	}
	return nil
}

// deliverActionsDueFor delivers every not-yet-delivered action in actions
// (per delivered, index-aligned with actions) that names instrumentID and
// whose EffectiveAt is strictly before boundary — that instrument's own next
// bar decision.
//
// It scans the whole list rather than following a single position in it,
// because actions may interleave several instruments in any order the
// fixture gives them: each instrument's own delivery point in the bar
// stream depends only on ITS OWN bars, never on where another instrument's
// bars or actions happen to sit in the file. readCorporateActions requires
// actions non-decreasing by EffectiveAt, so the entries this finds for one
// instrument are delivered here in ascending order too.
func deliverActionsDueFor(ctx context.Context, simulator *fills.Simulator, recorder *journal.Recorder, cfg event.ConfigurationPayload, strategyVersion string, actions []event.CorporateActionPayload, delivered []bool, instrumentID string, boundary time.Time) error {
	var due []int
	for i, action := range actions {
		if delivered[i] || action.InstrumentID != instrumentID || !action.EffectiveAt.Before(boundary) {
			continue
		}
		due = append(due, i)
	}
	return deliverInEffectiveOrder(ctx, simulator, recorder, cfg, strategyVersion, actions, delivered, due)
}

// deliverInEffectiveOrder delivers the given actions earliest first.
//
// Several actions for one instrument can fall before the same bar, and the
// reducer treats the first delisting it accepts as terminal: a later notice
// for the same instrument states no new fact and is ignored. Delivering in
// the order the fixture happened to list them would therefore let the file's
// order decide which effective time the Campaign's exit records, which is a
// fact about the file rather than about the instrument.
//
// Sorting here rather than demanding a sorted fixture keeps the command's
// output a function of what the actions say, not of how they were written
// down -- which only holds if the order is TOTAL. Effective time alone is
// not: two actions sharing an instant would fall back on their position in
// the file, and the journal would again describe the fixture rather than
// the instruments. Instrument and kind break the tie, and since those three
// fields are the whole of a CorporateActionPayload, two it cannot separate
// are the same value, whose order nothing can observe.
func deliverInEffectiveOrder(ctx context.Context, simulator *fills.Simulator, recorder *journal.Recorder, cfg event.ConfigurationPayload, strategyVersion string, actions []event.CorporateActionPayload, delivered []bool, due []int) error {
	sort.SliceStable(due, func(a, b int) bool {
		left, right := actions[due[a]], actions[due[b]]
		if !left.EffectiveAt.Equal(right.EffectiveAt) {
			return left.EffectiveAt.Before(right.EffectiveAt)
		}
		if left.InstrumentID != right.InstrumentID {
			return left.InstrumentID < right.InstrumentID
		}
		return left.Kind < right.Kind
	})
	for _, i := range due {
		if err := deliverCorporateAction(ctx, simulator, recorder, cfg, strategyVersion, actions[i]); err != nil {
			return err
		}
		delivered[i] = true
	}
	return nil
}

// deliverRemainingActions delivers whatever in actions is not yet delivered
// (per delivered), in the order actions gives them: called once, after the
// last bar, for an action effective at or after its own instrument's last
// bar, or naming an instrument this run holds no bar for at all — the
// unknown-instrument case internal/strategy/delisting.go's applyDelisting
// records without error.
func deliverRemainingActions(ctx context.Context, simulator *fills.Simulator, recorder *journal.Recorder, cfg event.ConfigurationPayload, strategyVersion string, actions []event.CorporateActionPayload, delivered []bool) error {
	var remaining []int
	for i := range actions {
		if !delivered[i] {
			remaining = append(remaining, i)
		}
	}
	return deliverInEffectiveOrder(ctx, simulator, recorder, cfg, strategyVersion, actions, delivered, remaining)
}

// deliverCorporateAction wraps action as an input envelope and delivers it
// through the same path Deliver applies to any non-bar input.
func deliverCorporateAction(ctx context.Context, simulator *fills.Simulator, recorder *journal.Recorder, cfg event.ConfigurationPayload, strategyVersion string, action event.CorporateActionPayload) error {
	envelope, err := inputEnvelope("corporate-action:"+action.InstrumentID+":"+action.EffectiveAt.UTC().Format(time.RFC3339Nano),
		event.MarketCorporateActionEventType, event.MarketCorporateActionSchemaVersion, action.EffectiveAt, action, cfg, strategyVersion)
	if err != nil {
		return err
	}
	if _, err := fills.Deliver(ctx, simulator, recorder, envelope); err != nil {
		return fmt.Errorf("backtest: %w", err)
	}
	return nil
}

// inputEnvelope wraps one payload as an input envelope. Sequence is left
// unset: the fill simulator numbers the composed stream, since only it knows
// the running order once fills are interleaved.
//
// RecordedAt is the event's own time. That is a property of a backtest
// fixture — the system "learns of" a bar at the moment the bar ends — and
// not a rule; a live producer records when it actually received the data.
func inputEnvelope(id, eventType string, schemaVersion uint32, at time.Time, payload any, cfg event.ConfigurationPayload, strategyVersion string) (event.Envelope, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return event.Envelope{}, fmt.Errorf("backtest: encode the %s payload: %w", eventType, err)
	}
	return event.Envelope{
		ID:                id,
		Type:              eventType,
		SchemaVersion:     schemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         at,
		RecordedAt:        at,
		Source:            sourceFixture,
		StrategyVersion:   strategyVersion,
		ConfigurationHash: event.ConfigurationHash(cfg),
		PayloadHash:       event.HashPayload(encoded),
		Payload:           encoded,
	}, nil
}

// readConfiguration loads and validates the declared configuration.
// ADR 0013 requires positive slippage; reject nonpositive values before
// reading bars or constructing the simulator.
func readConfiguration(path string) (event.ConfigurationPayload, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return event.ConfigurationPayload{}, fmt.Errorf("backtest: read the configuration: %w", err)
	}
	var cfg event.ConfigurationPayload
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return event.ConfigurationPayload{}, fmt.Errorf("backtest: decode the configuration in %s: %w", path, err)
	}
	if math.IsNaN(cfg.SlippageN) || cfg.SlippageN <= 0 {
		return event.ConfigurationPayload{}, fmt.Errorf("backtest: the configuration in %s states slippage %v: a run with zero slippage is invalid by construction (ADR 0013)", path, cfg.SlippageN)
	}
	if err := cfg.Validate(); err != nil {
		return event.ConfigurationPayload{}, fmt.Errorf("backtest: the configuration in %s is invalid: %w", path, err)
	}
	return cfg, nil
}

// readBars loads the bar fixture: a JSON array of completed bars, in the
// order the run delivers them.
func readBars(path string) ([]event.CompletedBarPayload, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("backtest: read the bars: %w", err)
	}
	var bars []event.CompletedBarPayload
	if err := json.Unmarshal(raw, &bars); err != nil {
		return nil, fmt.Errorf("backtest: decode the bars in %s: %w", path, err)
	}
	if len(bars) == 0 {
		return nil, fmt.Errorf("backtest: %s holds no bars; there is nothing to run", path)
	}
	for i, bar := range bars {
		if err := bar.Validate(); err != nil {
			return nil, fmt.Errorf("backtest: bar %d in %s: %w", i+1, path, err)
		}
	}
	return bars, nil
}

// readCorporateActions loads the corporate-action fixture named by path: a
// JSON array of event.CorporateActionPayload (CONTEXT.md: "Delisting Exit"),
// in any order — the same
// event.MarketCorporateActionEventType a live producer would eventually
// deliver instead, so the reducer never learns which one sent it
// (event.MarketCorporateActionEventType's own doc comment).
//
// An empty path names no fixture, which is not an error: it is a run that
// declares no corporate action at all, and readBars' "there is nothing to
// run" refusal for a bar fixture does not apply here, since a run naming
// none is the ordinary case this flag did not exist to change. A path that
// is given but decodes to a nil slice (the file holds JSON null) IS refused,
// unlike an empty array: null names no fixture by accident — most likely a
// producer that failed to write one — where "[]" states, deliberately, that
// this run declares none.
//
// The order actions are listed in does not matter, and nothing here
// checks it. deliverActionsDueFor rescans the whole slice before every bar
// and takes any not-yet-delivered action for that instrument whose
// effective time the bar has reached, so where an action lands depends on
// its own effective time and its own instrument's bars, never on its
// position in the file. A fixture listing a later action first places
// exactly as the same actions sorted would.
//
// That is why no ordering guard exists: one would reject fixtures this
// command handles correctly, to catch a mistake that has no consequence.
//
// Whether an action is stale relative to what the reducer has already
// accepted for its instrument remains entirely
// internal/strategy/delisting.go's applyDelisting's own question.
func readCorporateActions(path string) ([]event.CorporateActionPayload, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("backtest: read the corporate actions: %w", err)
	}
	var actions []event.CorporateActionPayload
	if err := json.Unmarshal(raw, &actions); err != nil {
		return nil, fmt.Errorf("backtest: decode the corporate actions in %s: %w", path, err)
	}
	if actions == nil {
		return nil, fmt.Errorf("backtest: %s holds no corporate actions (JSON null); state an empty array to declare a run with none", path)
	}
	for i, action := range actions {
		if err := action.Validate(); err != nil {
			return nil, fmt.Errorf("backtest: corporate action %d in %s: %w", i+1, path, err)
		}
	}
	return actions, nil
}

// checkJournalPathFree reports whether anything already occupies path.
func checkJournalPathFree(path string) error {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return journalExistsError(path)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("backtest: check the journal path: %w", err)
	}
	return nil
}

func journalExistsError(path string) error {
	return fmt.Errorf("backtest: %s already exists: a journal is recorded evidence and is never overwritten (ADR 0018); move it aside or choose another path", path)
}

// writeJournal installs a completed journal without replacing existing
// evidence (ADR 0017; AGENTS.md rule 6). There is deliberately no overwrite
// option; moving prior evidence is an operator decision.
//
// A journal is recorded evidence, and ADR 0018 forbids rewriting or
// deleting it: a path that already exists is refused outright rather than
// truncated, so a rerun cannot destroy the previous run's evidence — least
// of all before it has validated its own configuration. There is
// deliberately no overwrite flag; moving the old journal aside is a
// decision a person should make, and one this command should not offer to
// make for them.
//
// The write goes to a temporary file in the destination's own directory, is
// flushed to disk, and is linked into place, so a run interrupted mid-write
// leaves no partial journal that reads like a complete one and a run that
// loses a race for the path loses it cleanly. The directory has to be the
// destination's own: a hard link cannot cross filesystems.
//
// It reports whether THIS CALL installed the journal at path, separately from
// what went wrong, because the two are different facts: the link is the
// commit point and the flush of the directory holding the new name is a step
// after it. A caller that read "installed" off a nil error would refuse to
// anchor a journal this run really did write.
//
// The directory's own entries are flushed after the link for the reason the
// registry's are (ADR 0018): the journal's CONTENT is durable once the
// temporary file is synced, and the NAME pointing at it is not until the
// directory holding it is. A crash in between would leave the registry entry
// anchoring a journal whose name did not survive — an anchor pointing at
// nothing, in the record that exists so a chain head can be trusted from
// outside the journal (ADR 0017).
func writeJournal(path string, header journal.Header, entries []journal.Entry) (bool, error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".journal-*.partial")
	if err != nil {
		return false, fmt.Errorf("backtest: create the journal: %w", err)
	}
	temporary := file.Name()
	// Cleanup removes the temporary name on success and failure; only the
	// hard link below can install a completed journal at path (ADR 0017).
	defer func() {
		_ = file.Close()
		_ = os.Remove(temporary)
	}()

	if err := journal.Write(file, header, entries); err != nil {
		return false, err
	}
	if err := file.Sync(); err != nil {
		return false, fmt.Errorf("backtest: flush the journal to disk: %w", err)
	}
	if err := file.Close(); err != nil {
		return false, fmt.Errorf("backtest: close the journal: %w", err)
	}
	// Keep link(2): it fails atomically with EEXIST if a concurrent run or
	// restored backup occupies path. rename(2) would silently replace that
	// evidence, violating ADR 0017 even after a successful early path check.
	if err := os.Link(temporary, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, journalExistsError(path)
		}
		return false, fmt.Errorf("backtest: install the journal at %s: %w", path, err)
	}
	directory := filepath.Dir(path)
	if err := syncDir(directory); err != nil {
		return true, fmt.Errorf("backtest: the journal is written at %s and can be read there now, but flushing the directory %s to disk failed, so its name may not survive a power loss; the run is recorded either way, and committing the journal and the registry to git is what makes the record durable (ADR 0017): %w", path, directory, err)
	}
	return true, nil
}
