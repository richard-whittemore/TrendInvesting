// Command engine is the Go decision engine ADR 0014 puts behind a
// Unix-domain socket: it composes strategy.NewReducer, journal.Recorder and
// transport.Listen exactly as cmd/backtest composes the reducer, the fill
// simulator and journal.Recorder over a bar fixture (backtest.go's own doc
// comment: "The composition, in order..."), wired to a socket instead of a
// file. It is composition only and contains no rules
// (cmd-is-composition-only, .golangci.yml; AGENTS.md rule 7).
//
//	engine -socket <path> -config <configuration.json> -out <journal.jsonl>
//
// #27 requires that "the reducer's decisions are received and written to a
// journal"; this command is that wiring. The four decisions this ticket had
// to make, and why, are recorded beside the code that makes them: decision 1
// (a Decider returns one envelope, a reducer returns many) in decider.go's
// newDecider; decision 2 (where the configuration comes from) and decision 3
// (when the journal is written, and what a mid-stream disconnect means) in
// run, below; decision 4 (only one active executor) also in decider.go's
// newDecider.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
	"github.com/richard-whittemore/TrendInvesting/transport"
)

// sourceEngine is stamped on the configuration input this command
// manufactures for itself at startup, and on the outer envelope newDecider
// returns over the wire (event.Envelope.Source's own doc comment: "Source
// names the component that emitted the event"). It never appears on a
// decision: every decision this command journals or returns is the
// reducer's own emission, and carries the reducer's own Source ("reducer",
// internal/strategy/reducer.go's sourceReducer), unchanged.
const sourceEngine = "engine"

// options is one invocation of the engine.
//
// build is a parameter rather than a read of internal/buildinfo, for the
// identical reason cmd/backtest's own options.build is: it is the only part
// of a run's identity that comes from outside the configuration, and a test
// asserting a journal byte for byte needs to fix it (backtest.go's own
// comment on this field).
type options struct {
	socketPath      string
	configPath      string
	outPath         string
	build           string
	maxFrameBytes   int
	decisionTimeout time.Duration
}

// run performs one invocation: read and validate the configuration, compose
// the reducer and its journal, deliver the configuration as the run's
// required first input, listen on the socket until ctx is cancelled, and
// write the journal. It is separate from main so the command is testable as
// a function rather than as a process (mirrors cmd/backtest's run/backtest
// split).
func run(ctx context.Context, opts options, out io.Writer) error {
	var missing []error
	if opts.socketPath == "" {
		missing = append(missing, errors.New("-socket is required: the Unix-domain socket to listen on (ADR 0014)"))
	}
	if opts.configPath == "" {
		missing = append(missing, errors.New("-config is required: the configuration this run decides under"))
	}
	if opts.outPath == "" {
		missing = append(missing, errors.New("-out is required: where to write the run's journal when this process stops"))
	}
	if opts.build == "" {
		missing = append(missing, errors.New("the running build must be identified; it is part of every envelope's strategy version (ADR 0016)"))
	}
	if err := errors.Join(missing...); err != nil {
		return fmt.Errorf("engine: %w", err)
	}

	// Checked before the configuration is even read, so an operator is told
	// the path is taken in a moment rather than after the socket has already
	// bound (mirrors cmd/backtest.backtest's own ordering and its own
	// comment: this is a courtesy, not the guarantee — writeJournal installs
	// exclusively regardless, at the end of this run).
	if err := checkJournalPathFree(opts.outPath); err != nil {
		return err
	}

	cfg, err := readConfiguration(opts.configPath)
	if err != nil {
		return err
	}
	configurationHash := event.ConfigurationHash(cfg)
	strategyVersion := event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, opts.build)

	reducer, err := strategy.NewReducer(strategyVersion, cfg)
	if err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	recorder := journal.NewRecorder(reducer)

	// # Decision 2: where the configuration comes from
	//
	// The reducer needs a configuration before it can accept anything
	// (internal/strategy.Reducer.Apply fails closed on a bar that arrives
	// first: "received a completed bar before a configuration event"), and
	// strategy.NewReducer itself already needs a full configuration payload
	// before it can even be constructed. A backtest can read its
	// configuration event off the very stream it is about to replay
	// (cmd/backtest/backtest.go's drive builds one from -config and delivers
	// it as the run's first input); a live engine has no stream until this
	// command decides what will be on it, so the same file has to be read
	// before the socket exists at all. -config names that file, exactly as
	// cmd/backtest's own -config does.
	//
	// The SAME payload is then delivered to the reducer as its required
	// first input — never merely held as Go state — so that the
	// configuration this run actually decided under is itself the journal's
	// first record, exactly as cmd/backtest's own first input is
	// (backtest.go's drive, and ADR 0016's requirement that a configuration
	// hash and strategy version are always derived from a configuration
	// event that was actually applied, not asserted from outside it).
	//
	// This is also decision 4's fail-closed half from the brief: "an
	// unconfigured engine refuses inputs rather than sizing against
	// nothing". By the time transport.Listen is called below, the reducer is
	// already configured, so no bar arriving over the socket can ever reach
	// it unconfigured — and a -config that cannot be read or does not
	// validate stops this command here, before any socket binds, rather
	// than accepting connections it cannot safely answer.
	configEnvelope, err := configurationEnvelope(cfg, strategyVersion, time.Now().UTC())
	if err != nil {
		return err
	}
	if _, err := recorder.Apply(ctx, configEnvelope); err != nil {
		// Unreachable in practice: cfg has already passed Validate() above,
		// and strategy.NewReducer computed configurationHash from the same
		// cfg configEnvelope carries, so applyConfiguration's own checks
		// (schema version, matching configuration hash, "reducer is already
		// configured") cannot fail here. Guarded anyway, matching this
		// project's fail-closed style (see internal/strategy/reducer.go's
		// own such guards).
		return fmt.Errorf("engine: apply the configuration input: %w", err)
	}

	server, err := transport.Listen(opts.socketPath, newDecider(recorder, strategyVersion, configurationHash), transport.ServerConfig{
		MaxFrameBytes:   opts.maxFrameBytes,
		DecisionTimeout: opts.decisionTimeout,
	})
	if err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	if _, err := fmt.Fprintf(out, "listening on %s\n", server.Path()); err != nil {
		return fmt.Errorf("engine: report readiness: %w", err)
	}

	// # Decision 3: when the journal is written, and what a disconnect means
	//
	// ADR 0017: a journal is composed in memory and written once, because
	// its header states the span of input event times the run covered, and
	// that is not known until the last input has arrived. cmd/backtest knows
	// that moment exactly — its own last bar. A server has no such moment:
	// ServeContext blocks for as long as this process is asked to keep
	// deciding, and nothing in ADR 0014 ends a TRADING DAY merely because
	// one CONNECTION did. Quite the opposite: ADR 0014's own decision
	// section requires the adapter to abandon and never reuse a connection
	// the moment one exchange fails to complete ("A timed-out connection is
	// abandoned, not reused... The client... marks the connection dead
	// whenever an exchange does not complete"), and its measured failure
	// table reports "the session survives" as the adapter-facing outcome
	// for two of its eight enumerated failure cases — an abandoned
	// connection is an ordinary, planned-for event in this design, not a
	// sign that trading itself has to stop.
	//
	// So treating every dropped CONNECTION as the run's end — starting a
	// fresh journal, and with it a fresh reducer with no open Campaign for
	// any instrument and no Notional Account history (ADR 0006, ADR 0007)
	// — is not something ADR 0014 asks for, and this ticket has no
	// separate mandate to decide that a new socket connection means a new
	// trading day. This is this ticket's own judgment call, made because
	// the alternative is worse on the evidence this journal exists to be:
	// fragmenting one trading day's decisions across several partial
	// journals, none of which states the whole span a reviewer needs.
	//
	// So the run this journal records is this PROCESS's own lifetime: the
	// journal accumulates across however many connections arrive — at most
	// one at a time, decision 4 — and is written once, when this process is
	// asked to stop (SIGINT/SIGTERM, wired in main.go; a cancelled ctx in a
	// test). ServeContext returns only once every in-flight decision has
	// finished or been abandoned (transport.Server.Close waits on its
	// internal WaitGroup before returning), so by the time control reaches
	// the code below, no goroutine can still be holding newDecider's mutex,
	// and recorder.Entries()/Header() below cannot race a live Apply call.
	//
	// A connection that stops mid-stream — the adapter's socket read fails,
	// or it closes cleanly — costs this run nothing on its own:
	// transport.Server.handle simply returns, the connection is untracked,
	// and this process keeps running, ready for the next connection (or the
	// operator's shutdown) exactly as before. Nothing about the journal
	// changes until this process itself is asked to stop.
	//
	// This has a real, stated cost, said here plainly rather than papered
	// over: a SIGKILL, a panic, or a power loss between the last accepted
	// input and this process's own shutdown loses every decision this run
	// made, because ADR 0017's in-memory buffer is not durable until Write
	// runs. cmd/backtest carries the identical exposure for the identical
	// reason — ADR 0017's own consequence list: "A journal is composed in
	// memory and written in one pass... the format reopens when the bar
	// source is streamed, not before" — and this composition does not close
	// that gap, only inherits it at the scope of a whole server process
	// instead of one backtest run, where the window it is open for is far
	// longer and has no natural end. A durable, incremental journal (a
	// write-ahead log, one record at a time, rather than one write at the
	// end) would close it, and is future work, not attempted here.
	serveErr := server.ServeContext(ctx)

	header, headerErr := recorder.Header(configurationHash, strategyVersion)
	if headerErr != nil {
		// Unreachable in practice: configEnvelope was applied above, so
		// recorder always holds at least one input record by the time
		// ServeContext returns, and Header only fails when it holds none.
		// Guarded anyway, matching this project's fail-closed style.
		return errors.Join(serveErr, fmt.Errorf("engine: %w", headerErr))
	}
	entries := recorder.Entries()
	if writeErr := writeJournal(opts.outPath, header, entries); writeErr != nil {
		// Fail closed (docs/development.md principle 4: "Fail closed on
		// unknown schemas, missing sequences, stale data, or uncertain
		// brokerage state"; applied here to a journal write, the same
		// principle a stale or unrecognised event fails on): a journal that
		// fails to write is reported as a failure of this command, never
		// folded into a nil error — this exit code is the only signal an
		// operator has that the run's decisions are not on disk.
		return errors.Join(serveErr, fmt.Errorf("engine: the journal was not written; this run's decisions are not on disk: %w", writeErr))
	}
	if _, err := fmt.Fprintf(out, "wrote %s: %d record(s) over %s to %s\n",
		opts.outPath, len(entries),
		header.SpanStart.UTC().Format(time.RFC3339), header.SpanEnd.UTC().Format(time.RFC3339)); err != nil {
		return errors.Join(serveErr, fmt.Errorf("engine: report the run: %w", err))
	}
	return serveErr
}

// readConfiguration loads and validates the declared configuration, exactly
// as cmd/backtest's own readConfiguration does — ConfigurationPayload.Validate
// already rejects a nonpositive SlippageN (ADR 0013), so nothing here
// duplicates that check.
func readConfiguration(path string) (event.ConfigurationPayload, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return event.ConfigurationPayload{}, fmt.Errorf("engine: read the configuration: %w", err)
	}
	var cfg event.ConfigurationPayload
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return event.ConfigurationPayload{}, fmt.Errorf("engine: decode the configuration in %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return event.ConfigurationPayload{}, fmt.Errorf("engine: the configuration in %s is invalid: %w", path, err)
	}
	return cfg, nil
}

// configurationEnvelope wraps cfg as the event.ConfigurationEventType input
// this command delivers to the reducer before it opens its socket (decision
// 2, above).
//
// EventTime and RecordedAt are the moment this process is composing the run,
// read from the wall clock — unlike cmd/backtest's fixture convention (the
// first bar's own PeriodEnd, since a fixture has no clock of its own to read;
// backtest.go's drive says so directly), this is a live process that does
// have one. now is a parameter rather than a call to time.Now() here so this
// function stays independently testable; .golangci.yml's forbidigo rule
// forbidding time.Now is disabled for cmd/ regardless (the composition root
// "legitimately touch[es] the outside world; determinism is enforced in the
// domain"), so the constraint here is testability, not the lint rule.
func configurationEnvelope(cfg event.ConfigurationPayload, strategyVersion string, now time.Time) (event.Envelope, error) {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return event.Envelope{}, fmt.Errorf("engine: encode the configuration payload: %w", err)
	}
	hash := event.ConfigurationHash(cfg)
	return event.Envelope{
		ID:                "configuration:" + hash,
		Type:              event.ConfigurationEventType,
		SchemaVersion:     event.ConfigurationSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         now,
		RecordedAt:        now,
		Sequence:          1,
		Source:            sourceEngine,
		StrategyVersion:   strategyVersion,
		ConfigurationHash: hash,
		PayloadHash:       event.HashPayload(encoded),
		Payload:           encoded,
	}, nil
}

// checkJournalPathFree reports whether anything already occupies path,
// mirroring cmd/backtest's own courtesy check.
func checkJournalPathFree(path string) error {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return journalExistsError(path)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("engine: check the journal path: %w", err)
	}
	return nil
}

func journalExistsError(path string) error {
	return fmt.Errorf("engine: %s already exists: a journal is recorded evidence and is never overwritten (ADR 0018); move it aside or choose another path", path)
}

// writeJournal installs the run's journal exactly as cmd/backtest's own
// writeJournal does (ADR 0017's chain, ADR 0018's exclusive install): a
// temporary file in the destination's own directory, flushed to disk, then
// hard-linked into place, so an interrupted write can never leave a partial
// file that reads like a complete journal and a concurrent writer can never
// replace one that already arrived. There is deliberately no overwrite path.
func writeJournal(path string, header journal.Header, entries []journal.Entry) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".journal-*.partial")
	if err != nil {
		return fmt.Errorf("engine: create the journal: %w", err)
	}
	temporary := file.Name()
	// Cleanup removes the temporary name on success and failure; only the
	// hard link below can install a completed journal at path (ADR 0017).
	defer func() {
		_ = file.Close()
		_ = os.Remove(temporary)
	}()

	if err := journal.Write(file, header, entries); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("engine: flush the journal to disk: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("engine: close the journal: %w", err)
	}
	if err := os.Link(temporary, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return journalExistsError(path)
		}
		return fmt.Errorf("engine: install the journal at %s: %w", path, err)
	}
	directory := filepath.Dir(path)
	if err := syncDir(directory); err != nil {
		return fmt.Errorf("engine: the journal is written at %s and can be read there now, but flushing the directory %s to disk failed, so its name may not survive a power loss; committing the journal to git is what makes the record durable (ADR 0017): %w", path, directory, err)
	}
	return nil
}

// syncDir flushes a directory's own entries to disk, matching
// cmd/backtest's own helper: a name created in a directory is durable only
// once the directory holding it is flushed too, not merely the file itself.
// Windows cannot flush a directory handle at all, so the failure is
// tolerated there rather than failing a command whose work is already done.
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
