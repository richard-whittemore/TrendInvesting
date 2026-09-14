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
	configPath   string
	barsPath     string
	outPath      string
	registryPath string
	runID        string
	variant      string
	build        string
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
func backtest(opts options, out io.Writer) error {
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
	bars, err := readBars(opts.barsPath)
	if err != nil {
		return err
	}

	// Both derived from the configuration actually being run, never supplied
	// as a string (ADR 0016); only the build identifier comes from outside.
	configurationHash := event.ConfigurationHash(cfg)
	strategyVersion := event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, opts.build)

	reducer, err := strategy.NewReducer(strategyVersion, cfg)
	if err != nil {
		return fmt.Errorf("backtest: %w", err)
	}
	simulator, err := fills.New(cfg, strategyVersion, configurationHash)
	if err != nil {
		return fmt.Errorf("backtest: %w", err)
	}
	recorder := journal.NewRecorder(reducer)

	runErr := drive(context.Background(), simulator, recorder, cfg, strategyVersion, bars)

	// The journal is written whether or not the run completed: a handler
	// that failed closed may have emitted a final event explaining why, and
	// that event is exactly the one a reviewer needs (replay.Handler's
	// contract). A run that refused before its first input has nothing to
	// record and is reported on its own.
	header, headerErr := recorder.Header(configurationHash, strategyVersion)
	if headerErr != nil {
		return errors.Join(runErr, headerErr)
	}
	journalErr := writeJournal(opts.outPath, header, recorder.Entries())

	// Recorded whatever became of the run, and before the journal failure is
	// returned: the graveyard of failed runs is the point of the registry
	// (ADR 0012), and a run that is only registered when it went well is a
	// curated record.
	if err := registerRun(opts, cfg, strategyVersion, header, runErr, journalErr); err != nil {
		return errors.Join(runErr, journalErr, err)
	}
	if journalErr != nil {
		return errors.Join(runErr, journalErr)
	}

	report := fmt.Sprintf("wrote %s: %d record(s) over %s to %s\n",
		opts.outPath, len(recorder.Entries()),
		header.SpanStart.UTC().Format(time.RFC3339), header.SpanEnd.UTC().Format(time.RFC3339))
	if _, err := io.WriteString(out, report); err != nil {
		return errors.Join(runErr, fmt.Errorf("backtest: report the run: %w", err))
	}
	return runErr
}

// registerRun records the run in the registry opts names, if it names one.
//
// The status follows from whether anything went wrong, and a run that went
// wrong is recorded rather than dropped: ADR 0012's graveyard of failed and
// abandoned runs is what stops a surviving Variant looking more special than
// it is, and AGENTS.md rule 6 forbids curating it afterwards.
//
// # The journal is only this run's evidence if this run installed it
//
// journalErr is taken separately from runErr, and not merged into one
// failure, because it answers a question nothing else can: whether the
// journal now sitting at opts.outPath was written BY THIS PROCESS.
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
// So artefacts are attached only when journalErr is nil. Where they are
// attached, the chain head is read back from the file that actually landed
// rather than from what this process intended to write, so the value anchored
// in git attests the evidence rather than the intention (ADR 0017) — and a
// journal that this run installed and cannot then read back fails the
// command, whatever became of the run itself.
func registerRun(opts options, cfg event.ConfigurationPayload, strategyVersion string, header journal.Header, runErr, journalErr error) error {
	if opts.registryPath == "" {
		return nil
	}

	run := registry.Run{
		RunID:           opts.runID,
		Variant:         opts.variant,
		Status:          registry.StatusCompleted,
		StrategyVersion: strategyVersion,
		SpanStart:       header.SpanStart,
		SpanEnd:         header.SpanEnd,
		Configuration:   cfg,
	}
	if failure := errors.Join(runErr, journalErr); failure != nil {
		run.Status = registry.StatusFailed
		run.Detail = failure.Error()
	}

	if journalErr == nil {
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
	// Innermost first: the link, then the configuration-hash directory's own
	// entry in the root that MkdirAll may have just created it in.
	for _, synced := range []string{directory, root} {
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
// is written, so the never-overwrite rule (AGENTS.md rule 6) is untouched,
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
		return fmt.Errorf("backtest: run %q is already recorded under this configuration, and what is recorded is a different run: a recorded run is evidence and is never overwritten (AGENTS.md rule 6); record this one under another id", runID)
	}
	return nil
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
func drive(ctx context.Context, simulator *fills.Simulator, recorder *journal.Recorder, cfg event.ConfigurationPayload, strategyVersion string, bars []event.CompletedBarPayload) error {
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

	for _, bar := range bars {
		envelope, err := inputEnvelope("bar:"+bar.InstrumentID+":"+bar.PeriodEnd.UTC().Format(time.RFC3339Nano),
			event.CompletedBarEventType, event.CompletedBarSchemaVersion, bar.PeriodEnd, bar, cfg, strategyVersion)
		if err != nil {
			return err
		}
		if _, err := fills.RunBar(ctx, simulator, recorder, envelope); err != nil {
			return fmt.Errorf("backtest: %w", err)
		}
	}

	// The last bar's period end is where the input stream ran to, so it is
	// what every proposal still outstanding expires at (#68's rule, in
	// internal/strategy).
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

// readConfiguration loads the configuration and refuses the one run that is
// invalid by construction before any bar is processed.
func readConfiguration(path string) (event.ConfigurationPayload, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return event.ConfigurationPayload{}, fmt.Errorf("backtest: read the configuration: %w", err)
	}
	var cfg event.ConfigurationPayload
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return event.ConfigurationPayload{}, fmt.Errorf("backtest: decode the configuration in %s: %w", path, err)
	}
	// ADR 0013: a backtest run with zero slippage is invalid by
	// construction. The simulator refuses one too, but only once it is
	// built; a run is refused here, before a single bar is read, so the
	// operator is told what is wrong with their configuration rather than
	// what went wrong during their run.
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
	return fmt.Errorf("backtest: %s already exists: a journal is recorded evidence and is never overwritten (AGENTS.md rule 6); move it aside or choose another path", path)
}

// writeJournal writes the run's journal to path, refusing to disturb
// anything already there and leaving nothing behind if it fails.
//
// A journal is recorded evidence, and AGENTS.md rule 6 forbids rewriting or
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
func writeJournal(path string, header journal.Header, entries []journal.Entry) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".journal-*.partial")
	if err != nil {
		return fmt.Errorf("backtest: create the journal: %w", err)
	}
	temporary := file.Name()
	// Every failure from here on removes the partial file, so the only way
	// anything lands at path is the rename below.
	defer func() {
		_ = file.Close()
		_ = os.Remove(temporary)
	}()

	if err := journal.Write(file, header, entries); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("backtest: flush the journal to disk: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("backtest: close the journal: %w", err)
	}
	// A HARD LINK, not a rename, and this must not be "simplified" back:
	// rename(2) replaces an existing destination silently, so a journal
	// that appeared while this run was in progress — a concurrent run, a
	// restored backup — would be destroyed by it. link(2) fails with EEXIST
	// atomically instead, which is what makes the refusal a guarantee
	// rather than a check that something can race past. The content is
	// complete before the name exists either way, so there is still no
	// partial journal at the destination.
	if err := os.Link(temporary, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return journalExistsError(path)
		}
		return fmt.Errorf("backtest: install the journal at %s: %w", path, err)
	}
	return nil
}
