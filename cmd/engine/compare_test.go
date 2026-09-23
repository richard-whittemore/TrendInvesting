package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
	"github.com/richard-whittemore/TrendInvesting/transport"
)

// comparableDecision is the part of a Setup-evaluated decision that is a
// fact about what the reducer decided for a given bar, independent of
// which INPUT STREAM carried that bar to it. Sequence, CausationID and
// CorrelationID are deliberately excluded: those are stamped from the
// input that caused the decision (internal/replay.Stamp), and
// cmd/backtest's own input stream is not this command's — a backtest also
// delivers a starting account.snapshot and a closing replay.run.completed
// (backtest.go's drive), neither of which this command's -config/-socket
// composition sends at all (decision 2's own comment in engine.go). Two
// engines agreeing on every field below, for the same bar, is exactly what
// "same reducer, so they must [match]" (this ticket's brief) asserts;
// disagreeing on Sequence would not be a finding about the reducer, only
// about how many OTHER inputs surrounded the bar in each run.
type comparableDecision struct {
	ID                string
	Type              string
	SchemaVersion     uint32
	EventTime         time.Time
	RecordedAt        time.Time
	StrategyVersion   string
	ConfigurationHash string
	PayloadHash       string
}

func comparableDecisions(t *testing.T, path string) []comparableDecision {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = file.Close() }()
	_, records, err := journal.Read(file)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	_, decisions, err := journal.Split(records)
	if err != nil {
		t.Fatalf("split %s: %v", path, err)
	}
	var out []comparableDecision
	for _, decision := range decisions {
		if decision.Type != event.SetupEvaluatedEventType {
			continue
		}
		out = append(out, comparableDecision{
			ID:                decision.ID,
			Type:              decision.Type,
			SchemaVersion:     decision.SchemaVersion,
			EventTime:         decision.EventTime,
			RecordedAt:        decision.RecordedAt,
			StrategyVersion:   decision.StrategyVersion,
			ConfigurationHash: decision.ConfigurationHash,
			PayloadHash:       decision.PayloadHash,
		})
	}
	return out
}

// TestServerDecisionsMatchBacktestForTheSameBars is the brief's third
// verification: "The decisions the server returns match what cmd/backtest
// produces for the same inputs — same reducer, so they must." It runs the
// real cmd/backtest binary (via `go run`, so there is no reimplementation of
// its composition to drift from it) over a bars fixture, runs this
// command's own engine over the identical bars, and compares the
// Setup-evaluated decisions each one's journal recorded.
func TestServerDecisionsMatchBacktestForTheSameBars(t *testing.T) {
	if testing.Short() {
		t.Skip("shells out to `go run ./cmd/backtest`; skipped in -short")
	}

	dir := t.TempDir()
	const instrument = "TEST"
	const barCount = 25
	bars := make([]event.CompletedBarPayload, 0, barCount)
	for day := range barCount {
		bars = append(bars, flatBar(instrument, day))
	}
	barsPath := writeBarsFixture(t, dir, bars)
	backtestJournal := filepath.Join(dir, "backtest.journal.jsonl")

	cmd := exec.Command("go", "run", "../backtest",
		"-config", testConfigPath,
		"-bars", barsPath,
		"-out", backtestJournal,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run cmd/backtest: %v\n%s", err, output)
	}

	cfg := testConfiguration(t)
	strategyVersion := event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, "dev")
	configurationHash := event.ConfigurationHash(cfg)

	socketPath := filepath.Join(shortSocketDir(t), "engine.sock")
	engineJournal := filepath.Join(dir, "engine.journal.jsonl")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := newReadySignal()
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(ctx, options{
			socketPath: socketPath,
			configPath: testConfigPath,
			outPath:    engineJournal,
			// "dev" matches buildinfo.Version's own default (cmd/backtest was
			// just invoked with no -ldflags override), so the two runs
			// compose an identical StrategyVersion.
			build: "dev",
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
	for day, bar := range bars {
		// The engine's own configuration input occupies Sequence
		// configurationSequence; the adapter's numbering continues from
		// there (engine.go's package doc comment).
		envelope := barEnvelope(t, bar, configurationSequence+1+uint64(day), strategyVersion, configurationHash)
		if _, err := client.Decide(context.Background(), envelope); err != nil {
			t.Fatalf("bar %d: decide: %v", day, err)
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

	want := comparableDecisions(t, backtestJournal)
	got := comparableDecisions(t, engineJournal)
	if len(want) != barCount {
		t.Fatalf("cmd/backtest recorded %d setup-evaluated decision(s), want %d — the fixture no longer holds what this test assumes", len(want), barCount)
	}
	if len(got) != len(want) {
		t.Fatalf("this engine recorded %d setup-evaluated decision(s), cmd/backtest recorded %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bar %d diverges between the engine and cmd/backtest:\n engine:   %+v\n backtest: %+v", i, got[i], want[i])
		}
	}
}
