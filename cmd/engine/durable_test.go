package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
	"github.com/richard-whittemore/TrendInvesting/transport"
)

const durableTestInstrument = "TEST"

// sessionInputs is day's bar and its Session's close, at the sequences
// engine.go's wire contract gives them.
func sessionInputs(t *testing.T, day int, strategyVersion, configurationHash string) []event.Envelope {
	t.Helper()
	bar := flatBar(durableTestInstrument, day)
	sequence := configurationSequence + 1 + 2*uint64(day)
	return []event.Envelope{
		barEnvelope(t, bar, sequence, strategyVersion, configurationHash),
		sessionClosedEnvelope(t, bar, sequence+1, strategyVersion, configurationHash),
	}
}

func testIdentity(t *testing.T) (strategyVersion, configurationHash string) {
	t.Helper()
	cfg := testConfiguration(t)
	return event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, "test-build"), event.ConfigurationHash(cfg)
}

// driveSessions sends days [from, to) over one connection. When logPath is
// set, every decision the engine returns is checked to be in the durable log
// on disk already: journal-before-send, observed from the adapter's side.
func driveSessions(t *testing.T, socketPath, logPath string, from, to int) {
	t.Helper()
	client, err := transport.Dial(socketPath)
	if err != nil {
		t.Fatalf("dial the engine: %v", err)
	}
	defer func() { _ = client.Close() }()
	driveSessionsOn(t, client, logPath, from, to)
}

// driveSessionsOn drives sessions over a connection the caller already holds.
// The engine admits one connection at a time, and frees the slot only once it
// notices the previous one closed, so a test that closes a connection and
// dials another at once can be refused (an EOF on its first call).
func driveSessionsOn(t *testing.T, client *transport.Client, logPath string, from, to int) {
	t.Helper()
	strategyVersion, configurationHash := testIdentity(t)
	for day := from; day < to; day++ {
		for _, input := range sessionInputs(t, day, strategyVersion, configurationHash) {
			reply, err := client.Decide(context.Background(), input)
			if err != nil {
				t.Fatalf("day %d: decide %s: %v", day, input.ID, err)
			}
			if logPath == "" {
				continue
			}
			var payload decisionsPayload
			if err := json.Unmarshal(reply.Payload, &payload); err != nil {
				t.Fatalf("day %d: decode decisions: %v", day, err)
			}
			durable := recoverFile(t, logPath)
			for _, decision := range payload.Decisions {
				if !logHolds(durable, decision.ID) {
					t.Fatalf("day %d: the engine returned %s before it was in the durable log", day, decision.ID)
				}
			}
		}
	}
}

func recoverFile(t *testing.T, path string) journal.Recovery {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the durable log: %v", err)
	}
	defer func() { _ = file.Close() }()
	recovery, err := journal.RecoverLog(file)
	if err != nil {
		t.Fatalf("recover the durable log: %v", err)
	}
	return recovery
}

func logHolds(recovery journal.Recovery, id string) bool {
	for _, entry := range recovery.Entries {
		if entry.Kind == journal.KindDecision && entry.Envelope.ID == id {
			return true
		}
	}
	return false
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// uninterruptedJournal is the journal of one process deciding days [0, to).
func uninterruptedJournal(t *testing.T, to int) []byte {
	t.Helper()
	dir := t.TempDir()
	socketPath := filepath.Join(shortSocketDir(t), "engine.sock")
	out := filepath.Join(dir, "journal.jsonl")
	stop := startEngine(t, options{socketPath: socketPath, configPath: testConfigPath, asOf: testAsOf, outPath: out, build: "test-build"})
	driveSessions(t, socketPath, "", 0, to)
	if err := stop(); err != nil {
		t.Fatalf("the uninterrupted run: %v", err)
	}
	return readFile(t, out)
}

// TestARestartResumesFromTheDurableLog: a process streams its durable log,
// stops, and a second process resumes from it (ADR 0026 §5; ADR 0027). The
// second log begins with the first byte for byte, and the second process's
// journal is the journal one uninterrupted process would have written.
func TestARestartResumesFromTheDurableLog(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(shortSocketDir(t), "engine.sock")
	firstLog := filepath.Join(dir, "first.log")
	stop := startEngine(t, options{socketPath: socketPath, configPath: testConfigPath, asOf: testAsOf, outPath: filepath.Join(dir, "first.jsonl"), logPath: firstLog, build: "test-build"})
	driveSessions(t, socketPath, firstLog, 0, 4)
	if err := stop(); err != nil {
		t.Fatalf("the first run: %v", err)
	}

	secondLog := filepath.Join(dir, "second.log")
	secondOut := filepath.Join(dir, "second.jsonl")
	report := resumeEngine(t, options{socketPath: socketPath, configPath: testConfigPath, asOf: testAsOf, outPath: secondOut, logPath: secondLog, resumePath: firstLog, build: "test-build"})
	driveSessions(t, socketPath, secondLog, 4, 8)
	if err := report.stop(); err != nil {
		t.Fatalf("the resumed run: %v", err)
	}
	if !strings.Contains(report.out.String(), "resumed 17 record(s)") {
		t.Fatalf("the resumed run reported %q, want the records it resumed", report.out.String())
	}

	if first, second := readFile(t, firstLog), readFile(t, secondLog); !bytes.HasPrefix(second, first) {
		t.Fatal("the resumed run's durable log does not begin with the log it resumed from")
	}
	if got, want := readFile(t, secondOut), uninterruptedJournal(t, 8); !bytes.Equal(got, want) {
		t.Fatalf("the resumed run's journal differs from an uninterrupted run's:\n%s\nwant:\n%s", got, want)
	}
}

// resumed is a running engine and what it has reported; out is safe to read
// once stop has returned.
type resumed struct {
	stop func() error
	out  *bytes.Buffer
}

func resumeEngine(t *testing.T, opts options) resumed {
	t.Helper()
	out := &bytes.Buffer{}
	stop := startEngineInvocation(t, func(ctx context.Context, ready io.Writer) error {
		return run(ctx, opts, io.MultiWriter(ready, out))
	})
	return resumed{stop: stop, out: out}
}

// TestARestartAfterACrashMidWriteResumesFromTheLastCommit: a crash part way
// through an Append leaves a torn tail. The restart reports it, recovers
// every committed group, and the adapter sends the torn input again, because
// none of its decisions was ever released (ADR 0027's truncated-tail rule).
func TestARestartAfterACrashMidWriteResumesFromTheLastCommit(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(shortSocketDir(t), "engine.sock")
	firstLog := filepath.Join(dir, "first.log")
	stop := startEngine(t, options{socketPath: socketPath, configPath: testConfigPath, asOf: testAsOf, outPath: filepath.Join(dir, "first.jsonl"), logPath: firstLog, build: "test-build"})
	driveSessions(t, socketPath, firstLog, 0, 3)
	if err := stop(); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	// The crash: day 2's close was being written when the process died.
	whole := readFile(t, firstLog)
	lines := bytes.SplitAfter(whole, []byte("\n"))
	tornLength := len(lines[len(lines)-2]) + len(lines[len(lines)-3])/2
	torn := filepath.Join(dir, "torn.log")
	if err := os.WriteFile(torn, whole[:len(whole)-tornLength], 0o600); err != nil {
		t.Fatalf("write the torn log: %v", err)
	}

	secondOut := filepath.Join(dir, "second.jsonl")
	secondLog := filepath.Join(dir, "second.log")
	report := resumeEngine(t, options{socketPath: socketPath, configPath: testConfigPath, asOf: testAsOf, outPath: secondOut, logPath: secondLog, resumePath: torn, build: "test-build"})
	strategyVersion, configurationHash := testIdentity(t)
	client, err := transport.Dial(socketPath)
	if err != nil {
		t.Fatalf("dial the engine: %v", err)
	}
	if _, err := client.Decide(context.Background(), sessionInputs(t, 2, strategyVersion, configurationHash)[1]); err != nil {
		t.Fatalf("resend the torn input: %v", err)
	}
	// The same connection continues: the engine admits one at a time.
	driveSessionsOn(t, client, secondLog, 3, 5)
	_ = client.Close()
	if err := report.stop(); err != nil {
		t.Fatalf("the resumed run: %v", err)
	}
	if !strings.Contains(report.out.String(), "torn tail") {
		t.Fatalf("the resumed run reported %q, want the torn tail named", report.out.String())
	}
	if got, want := readFile(t, secondOut), uninterruptedJournal(t, 5); !bytes.Equal(got, want) {
		t.Fatal("the resumed run's journal differs from an uninterrupted run's")
	}
	if !bytes.Equal(readFile(t, torn), whole[:len(whole)-tornLength]) {
		t.Fatal("resuming modified the log it resumed from")
	}
}

func TestRunRefusesAResumeItCannotHonour(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(shortSocketDir(t), "engine.sock")
	firstLog := filepath.Join(dir, "first.log")
	stop := startEngine(t, options{socketPath: socketPath, configPath: testConfigPath, asOf: testAsOf, outPath: filepath.Join(dir, "first.jsonl"), logPath: firstLog, build: "test-build"})
	driveSessions(t, socketPath, firstLog, 0, 1)
	if err := stop(); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	damaged := filepath.Join(dir, "damaged.log")
	if err := os.WriteFile(damaged, append(readFile(t, firstLog), []byte("garbage\n")...), 0o600); err != nil {
		t.Fatalf("write the damaged log: %v", err)
	}

	base := options{socketPath: socketPath, configPath: testConfigPath, asOf: testAsOf, build: "test-build"}
	for name, tc := range map[string]struct {
		mutate func(*options)
		want   string
	}{
		"resume without a log": {func(o *options) { o.resumePath = firstLog }, "-resume requires -log"},
		"a log that exists":    {func(o *options) { o.logPath = firstLog }, "already exists"},
		"a missing log": {func(o *options) {
			o.logPath, o.resumePath = filepath.Join(dir, "m.log"), filepath.Join(dir, "missing.log")
		}, "missing.log"},
		"a damaged log": {func(o *options) { o.logPath, o.resumePath = filepath.Join(dir, "d.log"), damaged }, "damaged"},
		"another start": {func(o *options) {
			o.logPath, o.resumePath, o.asOf = filepath.Join(dir, "s.log"), firstLog, "2026-01-01T00:00:01Z"
		}, "configuration input"},
		"another build": {func(o *options) {
			o.logPath, o.resumePath, o.build = filepath.Join(dir, "b.log"), firstLog, "other-build"
		}, "not this run"},
	} {
		opts := base
		opts.outPath = filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".jsonl")
		tc.mutate(&opts)
		err := run(context.Background(), opts, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: run() = %v, want an error saying %q", name, err, tc.want)
		}
	}
}
