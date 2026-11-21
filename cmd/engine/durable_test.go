package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
	"github.com/richard-whittemore/TrendInvesting/transport"
)

const durableTestInstrument = "SPY"

// sessionInputs is day's bar and its Session's close, at the sequences
// engine.go's wire contract gives them.
func sessionInputs(t *testing.T, day int, strategyVersion, configurationHash string) []event.Envelope {
	t.Helper()
	bar := flatBar(durableTestInstrument, day)
	sequence := configurationSequence + 1 + 2*uint64(day)
	return []event.Envelope{
		boostBarEnvelope(t, day, sequence, strategyVersion, configurationHash),
		sessionClosedEnvelope(t, bar, sequence+1, strategyVersion, configurationHash),
	}
}

func testIdentity(t *testing.T) (strategyVersion, configurationHash string) {
	t.Helper()
	cfg := boostConfiguration(t, boostConfigPath)
	return event.ComposeStrategyVersion(cfg.StrategyID, targetweight.RulesVersion, "test-build"), event.TargetWeightConfigurationHash(cfg)
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
	stop := startEngine(t, options{mode: modeDryRun, socketPath: socketPath, configPath: boostConfigPath, asOf: testAsOf, outPath: out, logPath: filepath.Join(dir, "run.log"), build: "test-build"})
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
	stop := startEngine(t, options{mode: modeDryRun, socketPath: socketPath, configPath: boostConfigPath, asOf: testAsOf, outPath: filepath.Join(dir, "first.jsonl"), logPath: firstLog, build: "test-build"})
	driveSessions(t, socketPath, firstLog, 0, 4)
	if err := stop(); err != nil {
		t.Fatalf("the first run: %v", err)
	}

	secondLog := filepath.Join(dir, "second.log")
	secondOut := filepath.Join(dir, "second.jsonl")
	report := resumeEngine(t, options{mode: modeDryRun, socketPath: socketPath, configPath: boostConfigPath, asOf: testAsOf, outPath: secondOut, logPath: secondLog, resumePath: firstLog, build: "test-build"})
	driveSessions(t, socketPath, secondLog, 4, 8)
	if err := report.stop(); err != nil {
		t.Fatalf("the resumed run: %v", err)
	}
	if !strings.Contains(report.out.String(), "resumed 13 record(s)") {
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

// tornLogAtInput interrupts the named input's record before its commit, so
// recovery excludes its entire group, including decisions (ADR 0027 §5).
func tornLogAtInput(t *testing.T, whole []byte, inputID string) []byte {
	t.Helper()
	lines := bytes.SplitAfter(whole, []byte("\n"))
	offset := len(lines[0])
	for _, line := range lines[1:] {
		if len(line) == 0 {
			continue
		}
		var decoded struct {
			Record *journal.Record `json:"record"`
		}
		if err := json.Unmarshal(line, &decoded); err != nil {
			t.Fatalf("decode the durable log line: %v", err)
		}
		if decoded.Record != nil && decoded.Record.Kind == journal.KindInput && decoded.Record.Envelope.ID == inputID {
			return whole[:offset+len(line)/2]
		}
		offset += len(line)
	}
	t.Fatalf("the durable log has no input %s", inputID)
	return nil
}

// TestARestartAfterACrashMidWriteResumesFromTheLastCommit: a crash part way
// through an Append leaves a torn tail. The restart reports it, recovers
// every committed group, and the adapter sends the torn input again, because
// none of its decisions was ever released (ADR 0027's truncated-tail rule).
func TestARestartAfterACrashMidWriteResumesFromTheLastCommit(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(shortSocketDir(t), "engine.sock")
	firstLog := filepath.Join(t.TempDir(), "first.log")
	stop := startEngine(t, options{mode: modeDryRun, socketPath: socketPath, configPath: boostConfigPath, asOf: testAsOf, outPath: filepath.Join(dir, "first.jsonl"), logPath: firstLog, build: "test-build"})
	driveSessions(t, socketPath, firstLog, 0, 3)
	if err := stop(); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	// ADR 0027 §6: the simulated crash directory holds only the crashed
	// run, never a complete run beside a strict prefix of its records.
	// Day 2's close was being written when the process died.
	whole := readFile(t, firstLog)
	strategyVersion, configurationHash := testIdentity(t)
	tornInput := sessionInputs(t, 2, strategyVersion, configurationHash)[1]
	tornBytes := tornLogAtInput(t, whole, tornInput.ID)
	torn := filepath.Join(dir, "torn.log")
	if err := os.WriteFile(torn, tornBytes, 0o600); err != nil {
		t.Fatalf("write the torn log: %v", err)
	}

	recovered := recoverFile(t, torn)
	full := recoverFile(t, firstLog)
	if recovered.Torn == nil || len(recovered.Entries) != len(full.Entries)-2 {
		t.Fatalf("the crash recovered %d of %d records with tail %+v, want the final close and its decision uncommitted", len(recovered.Entries), len(full.Entries), recovered.Torn)
	}
	last := recovered.Entries[len(recovered.Entries)-1]
	if last.Kind != journal.KindInput || last.Envelope.Type != event.CompletedBarEventType || last.Envelope.Sequence != configurationSequence+5 {
		t.Fatalf("the last committed record = %+v, want day 2's bar before its torn close", last)
	}

	secondOut := filepath.Join(dir, "second.jsonl")
	secondLog := filepath.Join(dir, "second.log")
	report := resumeEngine(t, options{mode: modeDryRun, socketPath: socketPath, configPath: boostConfigPath, asOf: testAsOf, outPath: secondOut, logPath: secondLog, resumePath: torn, build: "test-build"})
	client, err := transport.Dial(socketPath)
	if err != nil {
		t.Fatalf("dial the engine: %v", err)
	}
	if _, err := client.Decide(context.Background(), tornInput); err != nil {
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
	if !bytes.Equal(readFile(t, torn), tornBytes) {
		t.Fatal("resuming modified the log it resumed from")
	}
}

func TestRunRefusesAResumeItCannotHonour(t *testing.T) {
	comp, err := compose(boostConfigPath, "test-build", mustTestAsOf(t))
	if err != nil {
		t.Fatal(err)
	}
	buffer := &memoryLog{}
	log, err := journal.CreateLog(buffer, logHeader(comp))
	if err != nil {
		t.Fatal(err)
	}
	recorder := journal.NewDurableRecorder(comp.handler, log, journal.DefaultMaxRecords)
	engine, err := replay.New(recorder)
	if err != nil {
		t.Fatal(err)
	}
	inputs := append([]event.Envelope{comp.configEnvelope}, sessionInputs(t, 0, comp.strategyVersion, comp.configurationHash)...)
	for _, input := range inputs {
		if _, err := engine.Apply(context.Background(), input); err != nil {
			t.Fatalf("record the Boost resume fixture: %v", err)
		}
	}
	for _, name := range []string{"resume without a log", "a log that exists", "a missing log", "a damaged log", "another start", "another build", "a log read fails after history validation"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			firstLog := filepath.Join(dir, "first.log")
			if err := os.WriteFile(firstLog, buffer.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			opts := options{mode: modeDryRun, socketPath: filepath.Join(shortSocketDir(t), "engine.sock"), configPath: boostConfigPath, asOf: testAsOf, build: "test-build",
				outPath: filepath.Join(dir, "out.jsonl"), logPath: filepath.Join(dir, "next.log"), resumePath: firstLog}
			var want string
			switch name {
			case "resume without a log":
				opts.logPath, want = "", "-resume requires -log"
			case "a log that exists":
				// ADR 0018: even a valid resume cannot overwrite its source.
				opts.logPath, want = firstLog, "already exists"
			case "a missing log":
				opts.resumePath, want = filepath.Join(dir, "missing.log"), "missing.log"
			case "a damaged log":
				if err := os.WriteFile(firstLog, append(append([]byte(nil), buffer.Bytes()...), []byte("garbage\n")...), 0o600); err != nil {
					t.Fatal(err)
				}
				want = "damaged"
			case "another start":
				// ADR 0027: change the composed configuration input's time,
				// preserving the logged input and its hash chain.
				opts.asOf, want = "2026-01-01T00:00:01Z", "configuration input"
			case "another build":
				opts.build, want = "other-build", "does not resume across builds"
			case "a log read fails after history validation":
				// ADR 0027: successful history and approval reads cannot
				// excuse a later read fault when rebuilding the run.
				opened := openDurableLog
				t.Cleanup(func() { openDurableLog = opened })
				reads := 0
				openDurableLog = func(path string) (io.ReadCloser, error) {
					if path == firstLog {
						reads++
						if reads > 2 {
							return nil, &fs.PathError{Op: "open", Path: path, Err: errDiskFault}
						}
					}
					return opened(path)
				}
				want = "open the durable log to resume from"
			}
			before := readFile(t, firstLog)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := run(ctx, opts, io.Discard)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("run() = %v, want an error saying %q", err, want)
			}
			if name == "a log read fails after history validation" && !errors.Is(err, errDiskFault) {
				t.Fatalf("run() = %v, want the log read fault", err)
			}
			if !bytes.Equal(readFile(t, firstLog), before) {
				t.Fatal("the refused start modified its source log")
			}
			if opts.logPath != "" && opts.logPath != firstLog {
				if _, err := os.Stat(opts.logPath); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("the refused start left a new log behind: %v", err)
				}
			}
		})
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
		opts := options{mode: modeDryRun, socketPath: socketPath, configPath: boostConfigPath, asOf: testAsOf, outPath: out, logPath: logPath, build: "test-build"}
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

// instanceLockShared tries a shared lock on the instance lock in dir, as the
// IBKR adapter's settle-ended-run takes it.
func instanceLockShared(t *testing.T, dir string) error {
	t.Helper()
	file, err := os.Open(filepath.Join(dir, "engine.lock")) // #nosec G304 -- the test's own folder.
	if err != nil {
		t.Fatalf("open the instance lock: %v", err)
	}
	defer func() { _ = file.Close() }()
	return syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
}

// TestTheEngineHoldsItsInstanceLockWhileItRunsWhateverLogItWrites pins the
// proof the IBKR adapter's settle-ended-run reads that a run has ended (ADR
// 0019, amended 2026-10-22): the engine holds an exclusive lock on one file
// in its state folder, the folder of -log, for as long as it runs, so a
// resume that continues the run in a new log holds the same lock as the
// process it continues.
func TestTheEngineHoldsItsInstanceLockWhileItRunsWhateverLogItWrites(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(shortSocketDir(t), "engine.sock")
	firstLog := filepath.Join(dir, "first.log")
	stop := startEngine(t, options{mode: modeDryRun, socketPath: socketPath, configPath: boostConfigPath, asOf: testAsOf, outPath: filepath.Join(dir, "first.jsonl"), logPath: firstLog, build: "test-build"})
	if err := instanceLockShared(t, dir); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("a shared lock on the running engine's instance lock = %v, want EWOULDBLOCK: the engine holds it", err)
	}
	driveSessions(t, socketPath, firstLog, 0, 2)
	if err := stop(); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	if err := instanceLockShared(t, dir); err != nil {
		t.Fatalf("a shared lock on the stopped engine's instance lock = %v, want it granted", err)
	}
	report := resumeEngine(t, options{mode: modeDryRun, socketPath: socketPath, configPath: boostConfigPath, asOf: testAsOf, outPath: filepath.Join(dir, "second.jsonl"), logPath: filepath.Join(dir, "second.log"), resumePath: firstLog, build: "test-build"})
	if err := instanceLockShared(t, dir); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("a shared lock on the resumed engine's instance lock = %v, want EWOULDBLOCK: a resume continues the run, so it holds the lock", err)
	}
	if err := report.stop(); err != nil {
		t.Fatalf("the resumed run: %v", err)
	}
	if err := instanceLockShared(t, dir); err != nil {
		t.Fatalf("a shared lock on the stopped resumed engine's instance lock = %v, want it granted", err)
	}
}

// TestTheEngineRefusesToStartWhileItsInstanceLockIsHeld: a second engine of
// the same instance never runs beside the first, and its refusal leaves no
// log behind (ADR 0019, amended 2026-10-22; fail closed, AGENTS.md).
func TestTheEngineRefusesToStartWhileItsInstanceLockIsHeld(t *testing.T) {
	dir := t.TempDir()
	held, err := os.OpenFile(filepath.Join(dir, "engine.lock"), os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- the test's own folder.
	if err != nil {
		t.Fatalf("create the instance lock: %v", err)
	}
	defer func() { _ = held.Close() }()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("hold the instance lock: %v", err)
	}
	logPath := filepath.Join(dir, "run.log")
	// Bounded, so a start that is not refused stops rather than serving.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = run(ctx, options{mode: modeDryRun, socketPath: filepath.Join(shortSocketDir(t), "engine.sock"), configPath: boostConfigPath, asOf: testAsOf, outPath: filepath.Join(dir, "run.jsonl"), logPath: logPath, build: "test-build"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "engine.lock") || !strings.Contains(err.Error(), "another engine") {
		t.Fatalf("run = %v, want a refusal naming the instance lock another engine holds", err)
	}
	if _, err := os.Stat(logPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the refused start left %s behind (%v): a refused start leaves nothing new", logPath, err)
	}
}

// TestASecondEngineRefusesAtTheInstanceLockBeforeItReadsTheApprovalOrTheRuns:
// the instance lock is taken before the approval is read or validated and
// before the folder's run history is read, so the approval an engine
// validates is checked against a run history no other engine of the
// instance can change while it runs (ADR 0019, amended 2026-10-22; ADR
// 0027; fail closed, AGENTS.md). Here the lock is held: the start is
// refused for the lock, never for the missing approval or the unreadable
// earlier log, which it never reaches.
func TestASecondEngineRefusesAtTheInstanceLockBeforeItReadsTheApprovalOrTheRuns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, dir string)
	}{
		{"an approval file that does not exist", func(*testing.T, string) {}},
		{"an earlier log that cannot be recovered", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "run-0.log"), []byte("not a durable log\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.prepare(t, dir)
			held, err := os.OpenFile(filepath.Join(dir, "engine.lock"), os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- the test's own folder.
			if err != nil {
				t.Fatalf("create the instance lock: %v", err)
			}
			defer func() { _ = held.Close() }()
			if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
				t.Fatalf("hold the instance lock: %v", err)
			}
			opts := boostOptions(t, dir)
			opts.approvalPath = filepath.Join(t.TempDir(), "approval.json")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err = run(ctx, opts, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "another engine") {
				t.Fatalf("run = %v, want the refusal for the instance lock another engine holds", err)
			}
			if strings.Contains(err.Error(), "read the approval") || strings.Contains(err.Error(), "run-0.log") {
				t.Fatalf("run = %v: the refused start read the approval or the run history before the instance lock", err)
			}
			if _, err := os.Stat(opts.logPath); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the refused start left %s behind (%v)", opts.logPath, err)
			}
		})
	}
}

// TestTheJournalAndTheDurableLogNeverNameTheInstanceLock: the instance lock
// is a fixed name in the folder of -log, so a -out or a -log naming it is
// refused before the lock is taken or anything is run, never accepted to
// fail when the journal cannot be installed at the end of the run (ADR
// 0018; ADR 0019, amended 2026-10-22; fail closed, AGENTS.md).
func TestTheJournalAndTheDurableLogNeverNameTheInstanceLock(t *testing.T) {
	for _, tc := range []struct {
		name     string
		log, out string
	}{
		{"-out names the instance lock", "run.log", "engine.lock"},
		{"-out names the instance lock in another case", "run.log", "Engine.Lock"},
		{"-log names the instance lock", "engine.lock", "run.jsonl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := options{mode: modeDryRun, socketPath: filepath.Join(shortSocketDir(t), "engine.sock"), configPath: boostConfigPath, asOf: testAsOf, outPath: filepath.Join(dir, tc.out), logPath: filepath.Join(dir, tc.log), build: "test-build"}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := run(ctx, opts, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "instance lock") {
				t.Fatalf("run = %v, want a refusal naming the instance lock", err)
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("the refused start left %v in %s: it is refused before the lock or the log is created", entries, dir)
			}
		})
	}
}

// TestAnInstanceLockTheFilesystemCannotTakeRefusesTheStartAndLeavesNoLog: on
// a filesystem without flock (ENOLCK, ENOTSUP) the engine cannot hold the
// proof that its run has not ended, so it does not start, says why, and
// leaves no empty log a later start would refuse as already existing (ADR
// 0018; ADR 0019, amended 2026-10-22; fail closed, AGENTS.md).
func TestAnInstanceLockTheFilesystemCannotTakeRefusesTheStartAndLeavesNoLog(t *testing.T) {
	for _, fault := range []error{syscall.ENOLCK, syscall.ENOTSUP} {
		t.Run(fault.Error(), func(t *testing.T) {
			saved := instanceFlock
			instanceFlock = func(int, int) error { return fault }
			t.Cleanup(func() { instanceFlock = saved })
			dir := t.TempDir()
			logPath := filepath.Join(dir, "run.log")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := run(ctx, options{mode: modeDryRun, socketPath: filepath.Join(shortSocketDir(t), "engine.sock"), configPath: boostConfigPath, asOf: testAsOf, outPath: filepath.Join(dir, "run.jsonl"), logPath: logPath, build: "test-build"}, io.Discard)
			if !errors.Is(err, fault) || !strings.Contains(err.Error(), "instance lock") {
				t.Fatalf("run = %v, want a refusal naming the instance lock and %v", err, fault)
			}
			if _, err := os.Stat(logPath); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the refused start left %s behind (%v): every later start would refuse it as already existing", logPath, err)
			}
		})
	}
}

// TestAResumeAcrossBuildsNamesTheUpgradeProcedure pins ADR 0027's rule that a build
// upgrade ends the run at the end of a session and starts a fresh run on the
// new build, never a resume. The refusal must name both builds and the
// procedure, and must not be confused with a changed configuration.
func TestAResumeAcrossBuildsNamesTheUpgradeProcedure(t *testing.T) {
	cfg := boostConfiguration(t, momentumConfigPath)
	hash := event.TargetWeightConfigurationHash(cfg)
	old := journal.NewLogHeader(hash, event.ComposeStrategyVersion(cfg.StrategyID, targetweight.RulesVersion, "build-1"))
	next := journal.NewLogHeader(hash, event.ComposeStrategyVersion(cfg.StrategyID, targetweight.RulesVersion, "build-2"))
	err := checkResumable(journal.Recovery{Header: old}, next, event.Envelope{})
	if err == nil {
		t.Fatal("checkResumable accepted a log from another build")
	}
	for _, want := range []string{"build-1", "build-2", "end of a session", "fresh run", "docs/runbooks/durable-log.md"} {
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

// TestAChangedStartOutranksABuildChange: a resume that differs in both build
// and -as-of must send the operator to the configuration correction, not to
// the build-upgrade procedure, which would not fix the start time.
func TestAChangedStartOutranksABuildChange(t *testing.T) {
	cfg := boostConfiguration(t, momentumConfigPath)
	hash := event.TargetWeightConfigurationHash(cfg)
	old := journal.NewLogHeader(hash, event.ComposeStrategyVersion(cfg.StrategyID, targetweight.RulesVersion, "build-1"))
	next := journal.NewLogHeader(hash, event.ComposeStrategyVersion(cfg.StrategyID, targetweight.RulesVersion, "build-2"))
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	logged := event.Envelope{EventTime: start, RecordedAt: start}
	composed := event.Envelope{EventTime: start.Add(time.Second), RecordedAt: start.Add(time.Second)}
	err := checkResumable(journal.Recovery{Header: old, Entries: []journal.Entry{{Kind: journal.KindInput, Envelope: logged}}}, next, composed)
	if err == nil || !strings.Contains(err.Error(), "configuration input") || strings.Contains(err.Error(), "docs/runbooks/durable-log.md") {
		t.Fatalf("a changed start and build got %v, want the configuration-input refusal", err)
	}
}

// TestTheDurableLogIsRequiredOutsideABacktest pins ADR 0027's rule that an engine
// that serves a dry-run, paper or live session
// refuses to start without -log, so journal-before-send always holds; a
// backtest keeps -log optional. There is no default mode.
func TestTheDurableLogIsRequiredOutsideABacktest(t *testing.T) {
	dir := t.TempDir()
	base := options{mode: modeDryRun, socketPath: filepath.Join(shortSocketDir(t), "e.sock"), configPath: boostConfigPath, asOf: testAsOf, build: "test-build"}
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

	// Covers the retired Turtle reducer (ADR 0002): only its backtest
	// mode permits an omitted durable log.
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

// faultLogRead makes every read of the log at path go through a reader that
// returns its first n bytes and then fault, as a disk whose sector fails
// partway through the file does; n < 0 fails the open itself.
func faultLogRead(t *testing.T, path string, n int64, fault error) {
	t.Helper()
	opened := openDurableLog
	t.Cleanup(func() { openDurableLog = opened })
	openDurableLog = func(name string) (io.ReadCloser, error) {
		if name != path {
			return opened(name)
		}
		if n < 0 {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fault}
		}
		file, err := opened(name)
		if err != nil {
			return nil, err
		}
		return struct {
			io.Reader
			io.Closer
		}{io.MultiReader(io.LimitReader(file, n), iotest.ErrReader(fault)), file}, nil
	}
}

// errDiskFault stands for an I/O error a disk returns partway through a
// file.
var errDiskFault = errors.New("input/output error")

// TestAReadThatFailsPartwayIsAFaultNotDamage: a log that opens but whose
// read fails after its header and first committed group is a fault in
// reading it, never damage to what it holds, so it is never reported as an
// unrecoverable log (ADR 0027 §5); a log read in full whose content is
// damaged is.
func TestAReadThatFailsPartwayIsAFaultNotDamage(t *testing.T) {
	source := t.TempDir()
	full, _ := firstBoostRun(t, source)
	dir := t.TempDir()
	path := committedPrefix(t, full.logPath, dir, "run-1.log", 1, time.Now())
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	faultLogRead(t, path, int64(len(whole)), errDiskFault)
	_, err = recoverLogFile(path)
	var unrecoverable *unrecoverableLogError
	if err == nil || errors.As(err, &unrecoverable) || !errors.Is(err, errDiskFault) {
		t.Fatalf("recoverLogFile() = %v, want the read fault, not an unrecoverable log", err)
	}

	damaged := filepath.Join(dir, "run-2.log")
	if err := os.WriteFile(damaged, append(whole[:len(whole)/2:len(whole)/2], "not a record\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverLogFile(damaged); !errors.As(err, &unrecoverable) {
		t.Fatalf("recoverLogFile() of a damaged log = %v, want an unrecoverable log", err)
	}
}
