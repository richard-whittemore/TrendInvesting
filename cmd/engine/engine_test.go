package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
	"github.com/richard-whittemore/TrendInvesting/transport"
)

// readySignal is an io.Writer that closes ready when socket readiness is
// reported — run's first write is "listening on %s\n", once
// transport.Listen has already bound the socket, so a test waiting on ready
// never dials a socket that has not been created yet, and never polls or
// sleeps to find out.
type readySignal struct {
	mu    sync.Mutex
	ready chan struct{}
	once  bool
	buf   bytes.Buffer
}

func newReadySignal() *readySignal {
	return &readySignal{ready: make(chan struct{})}
}

func (r *readySignal) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, err := r.buf.Write(p)
	if !r.once && strings.Contains(r.buf.String(), "listening on ") {
		r.once = true
		close(r.ready)
	}
	return n, err
}

// TestRunEndToEndOverASocket drives the engine exactly the way the brief's
// verification asks: "configuration in, bars in, decisions out, journal on
// disk", over a real Unix-domain socket, with no LEAN or mock in the way —
// the client is transport.Client, the server is a real transport.Server
// bound to a real socket file, and the decider behind it is a real
// strategy.Reducer built from this command's own composition (run).
func TestRunEndToEndOverASocket(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(shortSocketDir(t), "engine.sock")
	outPath := filepath.Join(dir, "journal.jsonl")
	cfg := testConfiguration(t)
	strategyVersion := event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, "test-build")
	configurationHash := event.ConfigurationHash(cfg)

	stop := startEngine(t, options{
		socketPath: socketPath,
		configPath: testConfigPath,
		asOf:       testAsOf,
		outPath:    outPath,
		build:      "test-build",
	})

	client, err := transport.Dial(socketPath)
	if err != nil {
		t.Fatalf("dial the engine: %v", err)
	}
	const instrument = "TEST"
	const barCount = 25
	for day := range barCount {
		bar := flatBar(instrument, day)
		// This run's own configuration input occupies Sequence
		// configurationSequence (see engine.go's package doc comment): the
		// adapter's own numbering continues that stream rather than
		// starting one of its own, so its first bar is
		// configurationSequence+1.
		// Each bar is followed by the close of its Session (ADR 0021),
		// below, so every day occupies two sequence numbers.
		sequence := configurationSequence + 1 + 2*uint64(day)
		envelope := barEnvelope(t, bar, sequence, strategyVersion, configurationHash)

		decision, err := client.Decide(context.Background(), envelope)
		if err != nil {
			t.Fatalf("bar %d: decide: %v", day, err)
		}
		if decision.CausationID != envelope.ID {
			t.Fatalf("bar %d: decision cites causation %q, want %q", day, decision.CausationID, envelope.ID)
		}
		if decision.Type != decisionsEventType {
			t.Fatalf("bar %d: decision type = %q, want %q", day, decision.Type, decisionsEventType)
		}
		var payload decisionsPayload
		if err := json.Unmarshal(decision.Payload, &payload); err != nil {
			t.Fatalf("bar %d: decode decisions payload: %v", day, err)
		}
		if len(payload.Decisions) != 1 {
			t.Fatalf("bar %d: got %d decisions, want exactly 1 (a flat bar never breaks out): %+v", day, len(payload.Decisions), payload.Decisions)
		}
		if payload.Decisions[0].Type != event.SetupEvaluatedEventType {
			t.Fatalf("bar %d: decision[0] type = %q, want %q", day, payload.Decisions[0].Type, event.SetupEvaluatedEventType)
		}

		closed := sessionClosedEnvelope(t, bar, sequence+1, strategyVersion, configurationHash)
		reply, err := client.Decide(context.Background(), closed)
		if err != nil {
			t.Fatalf("bar %d: session close: %v", day, err)
		}
		var closeDecisions decisionsPayload
		if err := json.Unmarshal(reply.Payload, &closeDecisions); err != nil {
			t.Fatalf("bar %d: decode session-close decisions: %v", day, err)
		}
		if reply.CausationID != closed.ID || len(closeDecisions.Decisions) != 1 {
			t.Fatalf("bar %d: session close answered %q with %d decisions, want its own causation and one empty Watchlist", day, reply.CausationID, len(closeDecisions.Decisions))
		}
		assertEmptyWatchlist(t, closeDecisions.Decisions[0], bar.PeriodEnd)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if err := stop(); err != nil {
		t.Fatalf("run returned an error: %v", err)
	}

	// journal.Verify accepts the journal the server wrote (brief's
	// verification, bullet 2).
	file, err := os.Open(outPath)
	if err != nil {
		t.Fatalf("open the journal: %v", err)
	}
	defer func() { _ = file.Close() }()
	verification, err := journal.Verify(file)
	if err != nil {
		t.Fatalf("journal.Verify: %v", err)
	}
	if verification.Header.ConfigurationHash != configurationHash {
		t.Errorf("journal configuration hash = %q, want %q", verification.Header.ConfigurationHash, configurationHash)
	}
	if verification.Header.StrategyVersion != strategyVersion {
		t.Errorf("journal strategy version = %q, want %q", verification.Header.StrategyVersion, strategyVersion)
	}

	if _, err := file.Seek(0, 0); err != nil {
		t.Fatalf("rewind the journal: %v", err)
	}
	header, records, err := journal.Read(file)
	if err != nil {
		t.Fatalf("journal.Read: %v", err)
	}
	if err := journal.CheckIdentity(header, records); err != nil {
		t.Errorf("journal.CheckIdentity: %v", err)
	}
	if err := journal.CheckSpan(header, records); err != nil {
		t.Errorf("journal.CheckSpan: %v", err)
	}
	inputs, decisions, err := journal.Split(records)
	if err != nil {
		t.Fatalf("journal.Split: %v", err)
	}
	// One configuration input plus, per bar sent, the bar and its Session's
	// close (ADR 0021).
	if want := 1 + 2*barCount; len(inputs) != want {
		t.Errorf("journal holds %d input(s), want %d (1 configuration + %d bars and their session closes)", len(inputs), want, barCount)
	}
	if inputs[0].Type != event.ConfigurationEventType {
		t.Errorf("journal's first input is %q, want %q", inputs[0].Type, event.ConfigurationEventType)
	}
	// One Setup evaluation per bar and one Watchlist per Session close
	// (ADR 0011); the configuration input itself produces none.
	if len(decisions) != 2*barCount {
		t.Fatalf("journal holds %d decision(s), want %d (Setup and Watchlist per Session)", len(decisions), 2*barCount)
	}
	for day := range barCount {
		if decision := decisions[2*day]; decision.Type != event.SetupEvaluatedEventType {
			t.Errorf("decision %d type = %q, want %q", 2*day, decision.Type, event.SetupEvaluatedEventType)
		}
		assertEmptyWatchlist(t, decisions[2*day+1], flatBar(instrument, day).PeriodEnd)
	}

	// The check this whole ticket is about: the journal must replay, not
	// merely verify structurally. See assertJournalReplays's own doc
	// comment for why journal.Verify/CheckIdentity/CheckSpan/Split, above,
	// are not sufficient on their own.
	assertJournalReplays(t, header, inputs, decisions)
}

// TestRunRefusesASecondConnectionEvenWhenItsCallsDoNotOverlapWithTheFirst is
// decision 4's own end-to-end proof, against the real composition run wires
// (transport.ServerConfig.MaxConnections: 1), not just against wireGuard in
// isolation: the first connection sends one bar and goes idle — no call of
// its own is in flight — before the second connection ever dials. A guard
// living only inside the Decider could never see this case at all, since
// nothing inside a Decider call overlaps with anything; the refusal has to
// come from the server refusing to admit the second connection in the first
// place.
func TestRunRefusesASecondConnectionEvenWhenItsCallsDoNotOverlapWithTheFirst(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(shortSocketDir(t), "engine.sock")
	outPath := filepath.Join(dir, "journal.jsonl")
	cfg := testConfiguration(t)
	strategyVersion := event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, "test-build")
	configurationHash := event.ConfigurationHash(cfg)

	stop := startEngine(t, options{
		socketPath: socketPath,
		configPath: testConfigPath,
		asOf:       testAsOf,
		outPath:    outPath,
		build:      "test-build",
	})

	// The engine's own configuration input occupies Sequence
	// configurationSequence, so the first bar any connection may legitimately
	// send is configurationSequence+1 — see engine.go's package doc comment.
	first, err := transport.Dial(socketPath)
	if err != nil {
		t.Fatalf("dial the first connection: %v", err)
	}
	firstBar := barEnvelope(t, flatBar("TEST", 0), configurationSequence+1, strategyVersion, configurationHash)
	if _, err := first.Decide(context.Background(), firstBar); err != nil {
		t.Fatalf("first connection: decide: %v", err)
	}
	// first is now idle: its exchange finished and it has sent nothing
	// since. The second connection below arrives into that idle window, not
	// into any overlapping call.

	second, err := transport.Dial(socketPath)
	if err != nil {
		t.Fatalf("dial the second connection: %v", err)
	}
	defer func() { _ = second.Close() }()
	// secondBar carries the Sequence wireEngine's cursor would actually
	// accept next (configurationSequence+2, immediately after firstBar's own
	// configurationSequence+1) — not a value contiguity would refuse on its
	// own — so that its refusal below can only be MaxConnections: if this
	// carried a Sequence the engine would reject anyway (a duplicate of
	// firstBar's, say), the assertion would still pass with MaxConnections
	// removed entirely, proving nothing about the guard it names.
	secondBar := barEnvelope(t, flatBar("OTHER", 0), configurationSequence+2, strategyVersion, configurationHash)
	if _, err := second.Decide(context.Background(), secondBar); err == nil {
		t.Fatal("second connection: decide succeeded; want it refused while the first connection is still open, even though the two never overlapped a call in time")
	}

	// The first connection is unaffected by the second's refusal: wireEngine's
	// cursor is still exactly where firstBar left it, since the refused
	// second connection never reached wireEngine at all, so the first
	// connection's own next input — the close of firstBar's Session (ADR
	// 0021) — carries the identical Sequence secondBar used above.
	nextBarForFirst := sessionClosedEnvelope(t, flatBar("TEST", 0), configurationSequence+2, strategyVersion, configurationHash)
	if _, err := first.Decide(context.Background(), nextBarForFirst); err != nil {
		t.Fatalf("first connection after the second was refused: decide: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close the first connection: %v", err)
	}
	if err := stop(); err != nil {
		t.Fatalf("run returned an error: %v", err)
	}
}

// TestRunRefusesToStartWithoutAValidConfiguration pins that an engine which
// cannot build a reducer never accepts an input at all, rather than accepting
// one and sizing against nothing — docs/development.md's "fail closed on
// unknown schemas, missing sequences, stale data, or uncertain brokerage
// state", at its strongest point: a -config the reducer cannot
// be built from stops this command before transport.Listen is ever called,
// so no socket is created for a bar to reach at all — a stronger guarantee
// than merely rejecting the first bar it receives.
func TestRunRefusesToStartWithoutAValidConfiguration(t *testing.T) {
	dir := t.TempDir()
	badConfig := filepath.Join(dir, "bad-configuration.json")
	// Zero slippage: ConfigurationPayload.Validate rejects this outright
	// (ADR 0013).
	if err := os.WriteFile(badConfig, []byte(`{
		"strategy_id": "bad",
		"sizing_mode": "volatility-normalised",
		"unit_volatility_fraction": 0.005,
		"stop_multiple": 2,
		"entry_channel_length": 20,
		"exit_channel_length": 10,
		"max_units": 4,
		"slippage_n": 0,
		"dollars_per_point": 1,
		"notional_account": {"starting_equity": 1000000, "rebasing_month": 1, "rebasing_day": 1},
		"commission": {"per_share": 0, "minimum_per_order": 0, "maximum_fraction_of_trade_value": 0.01}
	}`), 0o600); err != nil {
		t.Fatalf("write the bad configuration fixture: %v", err)
	}

	socketPath := filepath.Join(shortSocketDir(t), "engine.sock")
	err := run(context.Background(), options{
		socketPath: socketPath,
		configPath: badConfig,
		asOf:       testAsOf,
		outPath:    filepath.Join(dir, "journal.jsonl"),
		build:      "test-build",
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("run succeeded with an invalid configuration; want an error")
	}
	if _, statErr := os.Stat(socketPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a socket was created at %s despite the configuration being invalid; the engine must refuse to bind rather than accept connections it cannot safely answer", socketPath)
	}
}

// TestRunReportsAJournalWriteFailureRatherThanExitingClean pins that a
// journal which cannot be written is reported, never swallowed — an engine
// exiting zero having recorded nothing would leave a run with decisions
// already sent across the boundary and no evidence of them, which is
// docs/development.md's fail-closed principle applied to the artefact that
// principle exists to protect. The run completes an ordinary exchange, then the destination
// directory is made unwritable before the engine is asked to stop, so the
// pre-flight "does a journal already exist here" check (which only reads,
// never writes) passes but the actual install at shutdown cannot.
func TestRunReportsAJournalWriteFailureRatherThanExitingClean(t *testing.T) {
	if os.Geteuid() == 0 {
		// A process running as root — routinely true inside a CI container
		// — ignores the permission bits os.Chmod below sets: the write this
		// test relies on being refused would instead succeed, and the test
		// would be asserting nothing.
		t.Skip("skipped when running as root: permission bits do not restrict root's own writes")
	}
	dir := t.TempDir()
	socketPath := filepath.Join(shortSocketDir(t), "engine.sock")
	journalDir := filepath.Join(dir, "journals")
	if err := os.Mkdir(journalDir, 0o700); err != nil {
		t.Fatalf("create the journal directory: %v", err)
	}
	outPath := filepath.Join(journalDir, "journal.jsonl")

	// Registered before startEngine, so LIFO order runs it AFTER
	// startEngine's own stop-the-engine cleanup: permissions are restored
	// only once run itself is no longer using the directory, and t.TempDir's
	// own removal (registered before this) can still walk it afterwards.
	t.Cleanup(func() { _ = os.Chmod(journalDir, 0o700) })

	stop := startEngine(t, options{
		socketPath: socketPath,
		configPath: testConfigPath,
		asOf:       testAsOf,
		outPath:    outPath,
		build:      "test-build",
	})

	// Deny write+execute on the journal's own directory, so os.CreateTemp
	// inside it fails at shutdown exactly as a full or permission-denied
	// destination would in production.
	if err := os.Chmod(journalDir, 0o500); err != nil {
		t.Fatalf("make the journal directory read-only: %v", err)
	}

	if err := stop(); err == nil {
		t.Fatal("run reported success despite the journal directory being unwritable; a failed journal write must never exit clean")
	}

	if _, err := os.Stat(outPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a journal file exists at %s despite the write being refused", outPath)
	}
}

// assertEmptyWatchlist checks ADR 0011's explicit record that a completed
// Session has no rankable Tier A or Tier B Setups.
func assertEmptyWatchlist(t *testing.T, envelope event.Envelope, periodEnd time.Time) {
	t.Helper()
	if envelope.Type != event.WatchlistPublishedEventType {
		t.Fatalf("decision type = %q, want %q", envelope.Type, event.WatchlistPublishedEventType)
	}
	var payload event.WatchlistPublishedPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("decode Watchlist: %v", err)
	}
	if err := payload.Validate(); err != nil {
		t.Fatalf("invalid Watchlist: %v", err)
	}
	if !payload.PeriodEnd.Equal(periodEnd) || len(payload.Entries) != 0 {
		t.Fatalf("Watchlist = %+v, want no entries at %v", payload, periodEnd)
	}
}
