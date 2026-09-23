package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
	"github.com/richard-whittemore/TrendInvesting/transport"
)

// readySignal is an io.Writer that closes ready the first time anything is
// written to it — run's first write is "listening on %s\n", once
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
	if !r.once {
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := newReadySignal()
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(ctx, options{
			socketPath: socketPath,
			configPath: testConfigPath,
			outPath:    outPath,
			build:      "test-build",
		}, out)
	}()

	select {
	case <-out.ready:
	case err := <-runErr:
		t.Fatalf("run returned before it ever reported readiness: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the engine to report it is listening")
	}

	client, err := transport.Dial(socketPath)
	if err != nil {
		t.Fatalf("dial the engine: %v", err)
	}

	const instrument = "TEST"
	const barCount = 25
	for day := range barCount {
		bar := flatBar(instrument, day)
		envelope := barEnvelope(t, bar, uint64(day+1), strategyVersion, configurationHash)

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
	}

	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	cancel()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run returned an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for run to stop after the context was cancelled")
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
	// One configuration input plus one bar input per bar sent.
	if want := 1 + barCount; len(inputs) != want {
		t.Errorf("journal holds %d input(s), want %d (1 configuration + %d bars)", len(inputs), want, barCount)
	}
	if inputs[0].Type != event.ConfigurationEventType {
		t.Errorf("journal's first input is %q, want %q", inputs[0].Type, event.ConfigurationEventType)
	}
	// One setup-evaluated decision per bar; the configuration input itself
	// produces none (internal/strategy/reducer.go's applyConfiguration
	// returns (nil, nil)).
	if len(decisions) != barCount {
		t.Errorf("journal holds %d decision(s), want %d (one per bar)", len(decisions), barCount)
	}
	for i, decision := range decisions {
		if decision.Type != event.SetupEvaluatedEventType {
			t.Errorf("decision %d type = %q, want %q", i, decision.Type, event.SetupEvaluatedEventType)
		}
	}
}

// TestRunRefusesToStartWithoutAValidConfiguration proves the "unconfigured
// engine refuses inputs rather than sizing against nothing" fail-closed rule
// (this ticket's brief) at its strongest point: a -config the reducer cannot
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

// TestRunReportsAJournalWriteFailureRatherThanExitingClean proves "if the
// journal cannot be written, that is not a silent success" (this ticket's
// brief): the run completes an ordinary exchange, then the destination
// directory is made unwritable before the engine is asked to stop, so the
// pre-flight "does a journal already exist here" check (which only reads,
// never writes) passes but the actual install at shutdown cannot.
func TestRunReportsAJournalWriteFailureRatherThanExitingClean(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(shortSocketDir(t), "engine.sock")
	journalDir := filepath.Join(dir, "journals")
	if err := os.Mkdir(journalDir, 0o700); err != nil {
		t.Fatalf("create the journal directory: %v", err)
	}
	outPath := filepath.Join(journalDir, "journal.jsonl")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := newReadySignal()
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(ctx, options{
			socketPath: socketPath,
			configPath: testConfigPath,
			outPath:    outPath,
			build:      "test-build",
		}, out)
	}()

	select {
	case <-out.ready:
	case err := <-runErr:
		t.Fatalf("run returned before it ever reported readiness: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the engine to report it is listening")
	}

	// Deny write+execute on the journal's own directory, so os.CreateTemp
	// inside it fails at shutdown exactly as a full or permission-denied
	// destination would in production. Restored in cleanup so t.TempDir's
	// own removal can still walk it.
	if err := os.Chmod(journalDir, 0o500); err != nil {
		t.Fatalf("make the journal directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(journalDir, 0o700) })

	cancel()

	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("run reported success despite the journal directory being unwritable; a failed journal write must never exit clean")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for run to stop after the context was cancelled")
	}

	if _, err := os.Stat(outPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a journal file exists at %s despite the write being refused", outPath)
	}
}
