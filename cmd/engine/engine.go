// Command engine is the Go decision engine ADR 0014 puts behind a
// Unix-domain socket: it composes strategy.NewReducer, journal.Recorder and
// transport.Listen exactly as cmd/backtest composes the reducer, the fill
// simulator and journal.Recorder over a bar fixture (backtest.go's own doc
// comment: "The composition, in order..."), wired to a socket instead of a
// file. It is composition only and contains no rules
// (cmd-is-composition-only, .golangci.yml; AGENTS.md rule 7).
//
//	engine -socket <path> -config <configuration.json> -out <journal.jsonl> -as-of <RFC3339> [-mode backtest|dry-run|paper|live] [-log <durable.log> [-resume <previous.log>]] [-approval <approval.json>] [-operator <inbox>]
//
// # Wire contract: the adapter's first input continues the engine's own
//
// This run's own configuration input is always delivered first, at Sequence
// configurationSequence (1), before the socket opens. A fresh run after an
// earlier one then delivers the owner's approval as its second input, at
// Sequence approvalSequence (2; approval.go), also before the socket opens.
// The adapter's bars, session closes and account snapshots continue that
// SAME sequence — it does not start a numbering of its own — so its very
// first input must carry Sequence configurationSequence+1 (2), or
// approvalSequence+1 (3) when an approval was recorded, which the engine
// reports when it records one. Each slice's bars are one Session,
// ended by market.session.closed (ADR 0021); its account.snapshot follows
// the close's decisions, continuing the single input sequence: bar 2,
// session close 3, snapshot 4, bar 5, including warm-up. This
// is not a convenience: the journal this run writes is read back as ONE
// input stream (configuration included), and a stream in which two records
// both claim Sequence 1 fails its own replay.
//
// With -operator, the owner's recorded instructions (operator.go) join the
// same sequence: before the adapter's input, every instruction not yet
// recorded is applied at the sequence the adapter's input names and the
// ones after it, and the adapter's input follows them. The decisions
// returned carry, as their Sequence, the sequence the adapter's input was
// recorded at, so the adapter's next input always carries the returned
// Sequence + 1; without operator instructions that is the adapter's own
// Sequence + 1, as before. When operator instructions were recorded and the
// adapter's input is then refused, the refusal (transport.ProtocolError)
// states in its Sequence the last sequence recorded: the input's own, where
// its refusal was recorded, or, when the input was not recorded (an
// instruction could not be recorded, or the recorder refused the input of
// its own: its record bound or emission ceiling, ADR 0017), the last
// instruction's, and the run stops, so its durable log never misses a
// sequence and resumes. The adapter's next input carries that Sequence + 1.
// A resume reports the sequence the adapter's next input carries, once
// (with an approval, on the approval's line).
//
// An adapter's input sent again under its ID is the request below: it may
// carry the sequence it was recorded at or exactly the one it was sent
// with, before operator instructions moved it, and no instruction is
// delivered before it. After a restart, the log holding no sent sequence,
// it may carry any sequence from the first of the instructions recorded
// immediately before it.
//
// # Wire contract: an input sent again is a request for its committed reply
//
// An input whose ID names a group already committed is answered with the
// reply built from that group, byte for byte, and is not applied again, so
// the same input never yields a second order; a resumed run answers alike
// for every group it recovered (ADR 0027, amendment "re-delivery on
// request"). A different input under a committed ID is refused.
//
// The reducer's decisions are received over that socket and written to a
// journal; that is the whole of this command's job. Four design decisions
// this composition had to make, and why, are recorded beside the code that
// makes them: decision 1 (a Decider returns one envelope, a reducer returns
// many) in decider.go's newDecider; decision 2 (where the configuration
// comes from) and decision 3 (when the journal is written, and what a
// mid-stream disconnect means) in run, below; decision 4 (only one active
// executor) also in decider.go's newDecider.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/buildinfo"
	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/transport"
)

// rfc3339DateTime is RFC 3339's date-time production (section 5.6):
// full-date "T" full-time ("T" and "Z" may be lower case, section 5.6), with optional "." fractional seconds and a "Z" or
// numeric offset whose hours are 00-23 and minutes 00-59.
var rfc3339DateTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]([01]\d|2[0-3]):[0-5]\d)$`)

// sourceEngine is stamped on the configuration input this command
// manufactures for itself at startup, on the owner's approval input it
// records after it (approvalEnvelope), and on the outer envelope newDecider
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
	socketPath string
	configPath string
	outPath    string
	logPath    string
	resumePath string
	// approvalPath names the owner's operator.run.approved file for a
	// fresh run after an earlier one (approval.go).
	approvalPath string
	// operatorDir names the operator inbox (operator.go), where the
	// operator tool records the owner's kill switch and rebalance
	// clearances for a run that can reach a broker.
	operatorDir string
	asOf        string
	// mode is required and is what the adapter on the socket does with the
	// decisions: modeBacktest (decisions are simulated), or a mode that can reach a
	// broker. A mode other than backtest requires logPath (ADR 0027).
	mode            string
	build           string
	maxFrameBytes   int
	decisionTimeout time.Duration
}

// The run modes. Only backtest may run without a durable log: every other
// mode serves an adapter whose orders can reach a broker, or rehearses
// doing so, and so must hold journal-before-send (ADR 0019 amended
// 2026-09-24; ADR 0026 §3; ADR 0027). There is no default: an empty mode is refused, so forgetting -mode
// can never run a live engine without the log.
const (
	modeBacktest = "backtest"
	modeDryRun   = "dry-run"
	modePaper    = "paper"
	modeLive     = "live"
)

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
	if opts.asOf == "" {
		missing = append(missing, errors.New("-as-of is required: the run's declared start as an RFC 3339 time (ADR 0012)"))
	}
	if opts.build == "" {
		missing = append(missing, errors.New("the running build must be identified; it is part of every envelope's strategy version (ADR 0016)"))
	}
	switch opts.mode {
	case modeBacktest:
	case "":
		missing = append(missing, fmt.Errorf("-mode is required: %s, %s, %s or %s; there is no default, so a live run cannot start without the durable log by forgetting it (owner decision 2026-10-16)", modeBacktest, modeDryRun, modePaper, modeLive))
	case modeDryRun, modePaper, modeLive:
		if opts.build == buildinfo.Unstamped {
			missing = append(missing, fmt.Errorf("-mode %s refuses an unstamped build (%q): a run's build is part of every envelope's strategy version (ADR 0016) and a run never resumes across builds (ADR 0027), so it must be identified; build the engine with `make engine`, which stamps it from `git describe --always --dirty`", opts.mode, opts.build))
		}
		if opts.logPath == "" {
			missing = append(missing, fmt.Errorf("-log is required in -mode %s: orders that can reach a broker are sent only after they are flushed to a durable log (ADR 0027; owner decision 2026-10-16)", opts.mode))
		}
	default:
		missing = append(missing, fmt.Errorf("-mode must be one of %s, %s, %s or %s, not %q", modeBacktest, modeDryRun, modePaper, modeLive, opts.mode))
	}
	if opts.operatorDir != "" && opts.mode == modeBacktest {
		missing = append(missing, errors.New("-operator applies to a run that can reach a broker; a backtest takes no operator input"))
	}
	if opts.resumePath != "" && opts.logPath == "" {
		missing = append(missing, errors.New("-resume requires -log: a resumed run streams its own durable log, which begins with the records it resumes (ADR 0027)"))
	}
	if opts.resumePath != "" && strings.HasSuffix(opts.build, "-dirty") {
		missing = append(missing, fmt.Errorf("-resume refuses build %q: a dirty build that does not identify its tree names every uncommitted change at its commit alike, and a run never resumes across builds (ADR 0027, owner decision 2026-10-16); `make engine` stamps a dirty tree with its own tree hash", opts.build))
	}
	if err := errors.Join(missing...); err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	// time.Parse's RFC 3339 layout is lenient (it accepts a comma before
	// fractional seconds), so the strict RFC 3339 date-time syntax is checked
	// first: a value the journal's own RFC 3339 writer would not produce is
	// refused at startup (ADR 0017).
	if !rfc3339DateTime.MatchString(opts.asOf) {
		return fmt.Errorf("engine: -as-of must be an RFC 3339 time: %q is not RFC 3339 date-time syntax", opts.asOf)
	}
	// time.Parse accepts only upper-case separators; RFC 3339 allows both.
	asOf, err := time.Parse(time.RFC3339, strings.ToUpper(opts.asOf))
	if err != nil {
		return fmt.Errorf("engine: -as-of must be an RFC 3339 time: %w", err)
	}
	if asOf.IsZero() {
		return errors.New("engine: -as-of must be nonzero: the journal requires a nonzero span (ADR 0017)")
	}

	// Checked before the configuration is even read, so an operator is told
	// the path is taken in a moment rather than after the socket has already
	// bound (mirrors cmd/backtest.backtest's own ordering and its own
	// comment: this is a courtesy, not the guarantee — writeJournal installs
	// exclusively regardless, at the end of this run).
	if err := checkJournalPathFree(opts.outPath); err != nil {
		return err
	}
	// A log created at the journal's own path would hold that path for the
	// whole run, and the journal could then not be installed at shutdown.
	if opts.logPath != "" && samePath(opts.logPath, opts.outPath) {
		return fmt.Errorf("engine: -log and -out both name %s: the durable log is streamed during the run and the journal is installed when it stops, so they must be two files", opts.outPath)
	}

	// The configuration selects the strategy: the target-weight handler
	// with its risk gate, or the Turtle reducer (compose.go).
	comp, err := compose(opts.configPath, opts.build, asOf.UTC())
	if err != nil {
		return err
	}
	if err := checkModeBinding(opts.mode, comp); err != nil {
		return err
	}
	approval, err := freshRunApproval(opts, comp)
	if err != nil {
		return err
	}
	if err := armApproval(comp.handler, approval); err != nil {
		return err
	}
	var inbox *operatorInbox
	if opts.operatorDir != "" {
		if inbox, err = openOperatorInbox(opts, comp, out); err != nil {
			return err
		}
	}
	configurationHash, strategyVersion, configEnvelope := comp.configurationHash, comp.strategyVersion, comp.configEnvelope
	reducer := comp.handler

	// # Durability: journal-before-send
	//
	// With -log, every input's records are streamed to a durable log and
	// flushed before any of its decisions is returned towards the adapter
	// (ADR 0019, amended 2026-09-24; ADR 0026 §3; ADR 0027), so a crash
	// loses at most the one input whose write it interrupted, none of whose
	// decisions was released. With -resume, this process first rebuilds its
	// state from a previous process's log (ADR 0026 §5). The end-of-run
	// journal below is written exactly as before either way.
	recorder := journal.NewRecorder(reducer)
	logHeader := journal.NewLogHeader(configurationHash, strategyVersion)
	var recovery journal.Recovery
	if opts.resumePath != "" {
		if recovery, err = recoverLogFile(opts.resumePath); err != nil {
			return err
		}
		if err := checkResumable(recovery, logHeader, configEnvelope); err != nil {
			return err
		}
	}
	if opts.logPath != "" {
		logFile, err := createLogFile(opts.logPath)
		if err != nil {
			return err
		}
		defer func() { _ = logFile.Close() }()
		log, err := journal.CreateLog(durableLogSink(logFile), logHeader)
		if err != nil {
			return fmt.Errorf("engine: %w", err)
		}
		recorder = journal.NewDurableRecorder(reducer, log, durableRecordLimit)
	}
	if inbox != nil {
		inbox.recorder = recorder
	}

	// wireEngine is the ONE replay.Engine this run ever constructs, wrapping
	// recorder, and it is what EVERY input this run ever applies — its own
	// configuration input below, and every bar arriving over the socket
	// after it (see newDecider) — is applied through, rather than recorder
	// directly: a *replay.Engine keeps its own contiguity cursor and
	// output-sequence counter on the instance itself, persisted across
	// every call it is asked to make, so constructing it once, here, and
	// reusing it for the whole run is what gives this run's WHOLE input
	// stream — not just the wire-driven part of it — the identical
	// missing/duplicated/reordered-input protection replay.Engine.Run
	// already gave a whole batch, across as many separate connections as
	// this run's socket ever serves. The journal this run writes is read
	// back as exactly that one stream (cmd/backtest's own replay feeds
	// every recorded input, configuration included, to a fresh
	// replay.Engine.Run), so nothing in it may be exempted from this
	// Engine's own numbering — see the configuration input's own delivery,
	// below, for the defect that exempting it caused. See decider.go's
	// newDecider for the wire-driven half of the reasoning.
	wireEngine, err := replay.New(recorder)
	if err != nil {
		// Unreachable: New only refuses a nil handler, and recorder is
		// always non-nil here. Guarded anyway, matching this project's
		// fail-closed style.
		return fmt.Errorf("engine: %w", err)
	}

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
	// This is also the other side of failing closed on an unconfigured
	// engine: rather than sizing against nothing, it refuses to accept
	// inputs at all until it has one. By the time transport.Listen is called
	// below, the reducer is already configured, so no bar arriving over the
	// socket can ever reach it unconfigured — and a -config that cannot be
	// read or does not validate stops this command here, before any socket
	// binds, rather than accepting connections it cannot safely answer.
	//
	// Delivered through wireEngine, not recorder directly. An earlier
	// version of this command delivered it through recorder alone,
	// reasoning that a self-composed envelope has nothing for a contiguity
	// check to protect against — but the JOURNAL this run writes is read
	// back, by cmd/backtest's own replay, as ONE input stream, configuration
	// included: replayJournalInputs feeds every input record straight to a
	// fresh replay.Engine.Run, which enforces contiguity across all of them.
	// Bypassing wireEngine here left the configuration and the adapter's own
	// first bar both claiming Sequence 1, and a journal recorded that way
	// fails its own replay with "non-contiguous sequence: got 1 after 1" —
	// the exact defect wireEngine exists to prevent, reintroduced by
	// exempting this one input from it. Routing it through wireEngine instead
	// seeds the persistent cursor at configurationSequence, so the adapter's
	// own first bar must carry configurationSequence+1 to be accepted — see
	// configurationSequence's own doc comment, and cmd/engine's package doc
	// comment, for where that is stated to an adapter author.
	if len(recovery.Entries) > 0 {
		// The recovered log opens with this same configuration input
		// (checkResumable), so resuming delivers it through wireEngine
		// exactly as the else branch does.
		if err := resumeRun(ctx, wireEngine, recorder, recovery); err != nil {
			return err
		}
		if err := reportResume(out, opts.resumePath, recovery, approval != nil); err != nil {
			return err
		}
	} else if _, err := wireEngine.Apply(ctx, configEnvelope); err != nil {
		// Unreachable in practice: cfg has already passed Validate() above,
		// and strategy.NewReducer computed configurationHash from the same
		// cfg configEnvelope carries, so applyConfiguration's own checks
		// (schema version, matching configuration hash, "reducer is already
		// configured") cannot fail here, wireEngine's own contiguity check
		// cannot fail on the very first call it ever receives (any starting
		// Sequence is accepted), and configurationEnvelope always builds a
		// valid envelope. Guarded anyway, matching this project's
		// fail-closed style (see internal/strategy/reducer.go's own such
		// guards).
		return fmt.Errorf("engine: apply the configuration input: %w", err)
	}
	if approval != nil {
		// A fresh run after an earlier one: the owner's approval is its
		// second input, before the socket opens (approval.go).
		envelope, err := approvalEnvelope(*approval, comp, asOf.UTC())
		if err != nil {
			return err
		}
		if _, err := wireEngine.Apply(ctx, envelope); err != nil {
			return fmt.Errorf("engine: apply the approval input: %w", err)
		}
		if _, err := fmt.Fprintf(out, "recorded the owner's approval of a fresh run after %s at sequence %d; the adapter's first input carries sequence %d\n",
			approval.PreviousLog, approvalSequence, approvalSequence+1); err != nil {
			return fmt.Errorf("engine: report the approval: %w", err)
		}
	}

	serveCtx, stopServing := context.WithCancelCause(ctx)
	defer stopServing(nil)
	guard := &wireGuard{stop: stopServing}
	server, err := transport.Listen(opts.socketPath, newDecider(guard, wireEngine, recorder, strategyVersion, configurationHash, inbox), transport.ServerConfig{
		MaxFrameBytes:   opts.maxFrameBytes,
		DecisionTimeout: opts.decisionTimeout,
		// Decision 4: at most one connection may be open at a time (see
		// decider.go's newDecider for why this belongs here and not inside
		// the Decider).
		MaxConnections: 1,
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
	// — is not something ADR 0014 asks for, and nothing else decides that a
	// new socket connection means a new trading day either. This choice is
	// made here, deliberately, because the alternative is worse on the
	// evidence this journal exists to be: fragmenting one trading day's
	// decisions across several partial journals, none of which states the
	// whole span a reviewer needs.
	//
	// So the run this journal records is this PROCESS's own lifetime: the
	// journal accumulates across however many connections arrive — at most
	// one at a time, decision 4 — and is written once, when this process is
	// asked to stop (SIGINT/SIGTERM, wired in main.go; a cancelled ctx in a
	// test). ServeContext waits for every accepted CONNECTION's own
	// goroutine (transport.Server.Close waits on its internal WaitGroup
	// before returning) — but NOT for a decision transport itself already
	// gave up on: Server.decideWithTimeout races a Decider call against its
	// own deadline in a goroutine Server.wg never tracks, so that goroutine
	// can still be running, inside the reducer, after ServeContext returns
	// control here. guard.awaitIdle(), immediately below, is this run's own
	// wait for that goroutine — see decider.go's wireGuard doc comment for
	// why this is necessary and what it costs — so recorder.Entries()/
	// Header() cannot race a decision still writing to the recorder.
	//
	// A connection that stops mid-stream — the adapter's socket read fails,
	// or it closes cleanly — costs this run nothing on its own:
	// transport.Server.handle simply returns, the connection is untracked,
	// and this process keeps running, ready for the next connection (or the
	// operator's shutdown) exactly as before. Nothing about the journal
	// changes until this process itself is asked to stop.
	//
	// Without -log, a SIGKILL, a panic, or a power loss between the last
	// accepted input and this process's own shutdown loses every decision
	// this run made, because ADR 0017's in-memory buffer is not durable
	// until Write runs. With -log, the durable log (ADR 0027) bounds that
	// loss to the one input whose write the crash interrupted, none of whose
	// decisions was released, and -resume rebuilds the run from it.
	serveErr := server.ServeContext(serveCtx)

	// ServeContext waits for every accepted CONNECTION's own goroutine, but
	// not for a decision transport itself gave up waiting on
	// (decider.go's wireGuard doc comment: Server.decideWithTimeout spawns
	// that goroutine without ever tracking it). awaitIdle is this run's own
	// wait for that goroutine, whichever call currently holds guard's lock
	// — so recorder.Header()/Entries() below can never read the recorder
	// while a decision it does not know has finished is still writing to
	// it.
	guard.awaitIdle()
	if failure := guard.failure(); failure != nil {
		serveErr = errors.Join(serveErr, failure)
	}

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

// configurationSequence is the Sequence this command always stamps its own
// configuration input with, and so the first Sequence value wireEngine's
// persistent cursor (see run) ever accepts for this run. It is a named
// constant, not a literal 1 inlined where it is used, because it is also
// half of this engine's wire contract: the adapter's OWN first bar must
// carry Sequence configurationSequence+1 (approvalSequence+1 after a
// recorded approval), continuing this run's one input
// stream rather than starting a second one alongside it at the same value
// — see run's own doc comment on why, and cmd/engine's package doc comment,
// which is where an adapter author is expected to read this.
const configurationSequence uint64 = 1

// configurationEnvelope wraps cfg as the event.ConfigurationEventType input
// this command delivers to the reducer before it opens its socket (decision
// 2, above), at Sequence configurationSequence.
//
// EventTime and RecordedAt equal the nonzero declared run start, preserving
// comparable evidence (ADR 0012) and a repeatable journal chain (ADR 0017).
// The reducer ignores configuration time; warm-up bars may precede it, and
// the journal span includes those earlier inputs under ADR 0017.
func configurationEnvelope(cfg event.ConfigurationPayload, strategyVersion string, asOf time.Time) (event.Envelope, error) {
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
		EventTime:         asOf,
		RecordedAt:        asOf,
		Sequence:          configurationSequence,
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

// samePath reports whether a and b name one file, however each is spelled:
// the same name in the same directory. The directories are compared by
// their identity on disk, each examined exactly as spelled, so the file
// system resolves every symbolic link and every ".." in order; cleaning the
// path first would resolve a ".." after a link lexically, from where the
// link sits rather than from its target. Names are compared ignoring case,
// since a case-insensitive file system (macOS's default) maps two spellings
// to one file; on a case-sensitive one this refuses a pair that would have
// worked, the cautious side. Neither file need exist yet. A directory that
// cannot be examined cannot hold the file either path names, so the paths
// are then treated as distinct and creating the file reports the error.
func samePath(a, b string) bool {
	dirA, nameA := splitAsSpelled(a)
	dirB, nameB := splitAsSpelled(b)
	if !strings.EqualFold(nameA, nameB) {
		return false
	}
	infoA, errA := os.Stat(dirA)
	infoB, errB := os.Stat(dirB)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

// splitAsSpelled splits path after its last separator. filepath.Split,
// unlike filepath.Dir, does not clean, so the directory keeps every
// component the file system must resolve.
func splitAsSpelled(path string) (dir, name string) {
	dir, name = filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	return dir, name
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

// armApproval tells a fresh run's handler to decide nothing until the
// owner's approval is recorded (ADR 0027, amended 2026-10-17): the handler
// refuses any other input before it, whatever order it arrives in. A handler
// that cannot be told to wait is refused, because running it would leave
// the guard silently off.
func armApproval(handler replay.Handler, approval *event.OperatorRunApprovedPayload) error {
	if approval == nil {
		return nil
	}
	awaiting, ok := handler.(interface{ AwaitApproval() })
	if !ok {
		return fmt.Errorf("engine: an approval is required but the %T handler cannot hold its inputs until it is recorded, so the run does not start", handler)
	}
	awaiting.AwaitApproval()
	return nil
}
