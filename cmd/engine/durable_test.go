package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
	stop := startEngine(t, options{mode: modeBacktest, socketPath: socketPath, configPath: testConfigPath, asOf: testAsOf, outPath: out, build: "test-build"})
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
	stop := startEngine(t, options{mode: modeBacktest, socketPath: socketPath, configPath: testConfigPath, asOf: testAsOf, outPath: filepath.Join(dir, "first.jsonl"), logPath: firstLog, build: "test-build"})
	driveSessions(t, socketPath, firstLog, 0, 4)
	if err := stop(); err != nil {
		t.Fatalf("the first run: %v", err)
	}

	secondLog := filepath.Join(dir, "second.log")
	secondOut := filepath.Join(dir, "second.jsonl")
	report := resumeEngine(t, options{mode: modeBacktest, socketPath: socketPath, configPath: testConfigPath, asOf: testAsOf, outPath: secondOut, logPath: secondLog, resumePath: firstLog, build: "test-build"})
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
	stop := startEngine(t, options{mode: modeBacktest, socketPath: socketPath, configPath: testConfigPath, asOf: testAsOf, outPath: filepath.Join(dir, "first.jsonl"), logPath: firstLog, build: "test-build"})
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
	report := resumeEngine(t, options{mode: modeBacktest, socketPath: socketPath, configPath: testConfigPath, asOf: testAsOf, outPath: secondOut, logPath: secondLog, resumePath: torn, build: "test-build"})
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
	stop := startEngine(t, options{mode: modeBacktest, socketPath: socketPath, configPath: testConfigPath, asOf: testAsOf, outPath: filepath.Join(dir, "first.jsonl"), logPath: firstLog, build: "test-build"})
	driveSessions(t, socketPath, firstLog, 0, 1)
	if err := stop(); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	damaged := filepath.Join(dir, "damaged.log")
	if err := os.WriteFile(damaged, append(readFile(t, firstLog), []byte("garbage\n")...), 0o600); err != nil {
		t.Fatalf("write the damaged log: %v", err)
	}

	base := options{mode: modeBacktest, socketPath: socketPath, configPath: testConfigPath, asOf: testAsOf, build: "test-build"}
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
		}, "does not resume across builds"},
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

// TestRunRefusesALogAtTheJournalPath: a log created where the journal is to
// be installed would take that path for the whole run, and the operator
// would learn it only when the journal could not be written at shutdown. It
// is refused at startup, however the same path is spelled, and nothing is
// created.
func TestRunRefusesALogAtTheJournalPath(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(shortSocketDir(t), "engine.sock")
	out := filepath.Join(dir, "run.jsonl")
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(dir, linked); err != nil {
		t.Fatalf("link the journal's directory: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatalf("make a subdirectory: %v", err)
	}
	for name, logPath := range map[string]string{
		"through a link":     filepath.Join(linked, "run.jsonl"),
		"the same path":      out,
		"the same path, dot": dir + string(filepath.Separator) + "." + string(filepath.Separator) + "run.jsonl",
		"the same path, up":  filepath.Join(dir, "sub") + string(filepath.Separator) + ".." + string(filepath.Separator) + "run.jsonl",
	} {
		opts := options{mode: modeBacktest, socketPath: socketPath, configPath: testConfigPath, asOf: testAsOf, outPath: out, logPath: logPath, build: "test-build"}
		// Bounded, so a run that wrongly starts serving fails the test
		// rather than hanging it.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := run(ctx, opts, io.Discard)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "-log and -out") {
			t.Fatalf("%s: run() = %v, want an error saying -log and -out name one file", name, err)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatalf("%s: %s exists after the refusal (stat error %v), want nothing created", name, out, err)
		}
	}
}

// TestSamePathResolvesEachDirectoryAsTheFileSystemDoes: a ".." after a
// symbolic link climbs from the link's target, not from where the link
// sits, so cleaning the path before resolving it would both refuse two
// distinct files and miss one file spelled two ways.
func TestSamePathResolvesEachDirectoryAsTheFileSystemDoes(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "other", "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatalf("make the link's target: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(nested, link); err != nil {
		t.Fatalf("link: %v", err)
	}
	sep := string(filepath.Separator)
	// link/../run.jsonl is root/other/run.jsonl on disk, never root/run.jsonl.
	throughLinkUp := link + sep + ".." + sep + "run.jsonl"
	for _, tc := range []struct {
		name string
		a, b string
		want bool
	}{
		{"one spelling", filepath.Join(root, "run.jsonl"), filepath.Join(root, "run.jsonl"), true},
		{"a dot segment", root + sep + "." + sep + "run.jsonl", filepath.Join(root, "run.jsonl"), true},
		{"link then up is not the link's parent", throughLinkUp, filepath.Join(root, "run.jsonl"), false},
		{"link then up is the target's parent", throughLinkUp, filepath.Join(root, "other", "run.jsonl"), true},
		{"names differing only in case", filepath.Join(root, "run.JSONL"), filepath.Join(root, "run.jsonl"), true},
		{"another name", filepath.Join(root, "run.log"), filepath.Join(root, "run.jsonl"), false},
		{"another directory", filepath.Join(nested, "run.jsonl"), filepath.Join(root, "run.jsonl"), false},
	} {
		if got := samePath(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: samePath(%s, %s) = %v, want %v", tc.name, tc.a, tc.b, got, tc.want)
		}
	}
}

// TestCreateLogFileFlushesTheDirectoryThatHoldsTheLog: a ".." after a
// symbolic link climbs from the link's target, so link/../run.log is created
// in the target's parent. The directory flushed must be that one, resolved
// as spelled, not the link's parent that cleaning the path would name. The
// link's parent is made unreadable, so flushing it fails while flushing the
// log's own directory succeeds.
func TestCreateLogFileFlushesTheDirectoryThatHoldsTheLog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions and symbolic links differ on Windows")
	}
	root := t.TempDir()
	nested := filepath.Join(root, "other", "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatalf("make the link's target: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(nested, link); err != nil {
		t.Fatalf("link: %v", err)
	}
	// Traversable but not readable: a path through root still resolves,
	// but root itself cannot be opened to be flushed.
	if err := os.Chmod(root, 0o300); err != nil {
		t.Fatalf("make the link's parent unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	sep := string(filepath.Separator)
	path := link + sep + ".." + sep + "run.log"

	file, err := createLogFile(path)
	if err != nil {
		t.Fatalf("createLogFile(%s) = %v, want the log created and its own directory flushed", path, err)
	}
	_ = file.Close()
	if _, err := os.Stat(filepath.Join(root, "other", "run.log")); err != nil {
		t.Fatalf("the log is not in the link target's parent: %v", err)
	}
}

// TestAResumeAcrossBuildsNamesTheUpgradeProcedure pins the owner's decision of
// 2026-10-16 (#337): a build upgrade ends the run at the end of a session and
// starts a fresh run on the new build, never a resume. The refusal must name
// both builds and the procedure, and must not be confused with a changed
// configuration.
func TestAResumeAcrossBuildsNamesTheUpgradeProcedure(t *testing.T) {
	cfg := testConfiguration(t)
	hash := event.ConfigurationHash(cfg)
	old := journal.NewLogHeader(hash, event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, "build-1"))
	next := journal.NewLogHeader(hash, event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, "build-2"))
	err := checkResumable(journal.Recovery{Header: old}, next, event.Envelope{})
	if err == nil {
		t.Fatal("checkResumable accepted a log from another build")
	}
	for _, want := range []string{"build-1", "build-2", "2026-10-16", "end of a session", "fresh run", "docs/runbooks/durable-log.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}

	// A different configuration is a different refusal: the upgrade
	// procedure would mislead an operator who changed -config.
	other := journal.NewLogHeader("sha256:other", next.StrategyVersion)
	err = checkResumable(journal.Recovery{Header: old}, other, event.Envelope{})
	if err == nil || strings.Contains(err.Error(), "docs/runbooks/durable-log.md") {
		t.Fatalf("a changed configuration got %v, want the generic refusal", err)
	}
}

// TestTheDurableLogIsRequiredOutsideABacktest pins #336 as the owner decided
// on 2026-10-16: an engine that serves a dry-run, paper or live session
// refuses to start without -log, so journal-before-send always holds; a
// backtest (the default) keeps -log optional.
func TestTheDurableLogIsRequiredOutsideABacktest(t *testing.T) {
	dir := t.TempDir()
	base := options{mode: modeBacktest, socketPath: filepath.Join(shortSocketDir(t), "e.sock"), configPath: testConfigPath, asOf: testAsOf, build: "test-build"}
	for _, mode := range []string{"dry-run", "paper", "live"} {
		opts := base
		opts.mode = mode
		opts.outPath = filepath.Join(dir, mode+".jsonl")
		err := run(context.Background(), opts, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "-log is required") || !strings.Contains(err.Error(), mode) {
			t.Errorf("mode %s without -log: run() = %v, want a refusal naming -log and the mode", mode, err)
		}
	}
	opts := base
	opts.mode, opts.outPath = "", filepath.Join(dir, "none.jsonl")
	if err := run(context.Background(), opts, io.Discard); err == nil || !strings.Contains(err.Error(), "-mode is required") {
		t.Errorf("no mode: run() = %v, want a refusal saying -mode is required", err)
	}
	opts = base
	opts.mode, opts.outPath = "sandbox", filepath.Join(dir, "sandbox.jsonl")
	if err := run(context.Background(), opts, io.Discard); err == nil || !strings.Contains(err.Error(), "-mode") {
		t.Errorf("an unknown mode: run() = %v, want a refusal naming -mode", err)
	}

	// With -log a mode that can reach a broker starts (a dry-run Boost
	// configuration; the Turtle reducer runs only as a backtest); a backtest
	// needs no log.
	for _, mode := range []string{"backtest", "dry-run"} {
		socketPath := filepath.Join(shortSocketDir(t), "engine.sock")
		opts := options{socketPath: socketPath, configPath: testConfigPath, asOf: testAsOf, build: "test-build", mode: mode,
			outPath: filepath.Join(dir, "ok-"+mode+".jsonl")}
		if mode == "dry-run" {
			opts.configPath, opts.asOf = boostConfigPath, boostAsOf
			opts.logPath = filepath.Join(dir, "ok-dry-run.log")
		}
		stop := startEngine(t, opts)
		if err := stop(); err != nil {
			t.Errorf("mode %q: %v", mode, err)
		}
	}
}
