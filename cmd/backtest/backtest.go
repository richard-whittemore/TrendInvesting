package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
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
type options struct {
	configPath string
	barsPath   string
	outPath    string
	build      string
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
	if err := writeJournal(opts.outPath, header, recorder.Entries()); err != nil {
		return errors.Join(runErr, err)
	}

	report := fmt.Sprintf("wrote %s: %d record(s) over %s to %s\n",
		opts.outPath, len(recorder.Entries()),
		header.SpanStart.UTC().Format(time.RFC3339), header.SpanEnd.UTC().Format(time.RFC3339))
	if _, err := io.WriteString(out, report); err != nil {
		return errors.Join(runErr, fmt.Errorf("backtest: report the run: %w", err))
	}
	return runErr
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
