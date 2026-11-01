package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// shortSocket is a socket path short enough for every platform's bound.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// toolRecorder is a fake tool run: it records each name and answers out
// with code, or waits for its context to end when block is set.
type toolRecorder struct {
	mu       sync.Mutex
	names    []string
	out      string
	code     int
	block    bool
	canceled chan struct{}
}

func (f *toolRecorder) run(ctx context.Context, name string) (out string, code int) {
	f.mu.Lock()
	f.names = append(f.names, name)
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		close(f.canceled)
		return "", 1
	}
	return f.out, f.code
}

func (f *toolRecorder) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.names)
}

// TestTheWrapperSendsOnlyAToolNameAndRunsNothingItself: the session's
// wrapper takes one name from the fixed list and hands only that name to
// the job's broker, over a socket only this user can open, and prints what
// the broker answers with its exit status; any other input is refused
// before anything is sent, and a raw request the broker does not know is
// refused there too.
func TestTheWrapperSendsOnlyAToolNameAndRunsNothingItself(t *testing.T) {
	socket := shortSocket(t)
	f := &toolRecorder{out: "--- FAIL: TestX\n", code: 1}
	stop, err := serveTools(context.Background(), socket, f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	info, err := os.Stat(socket)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the socket's mode is %v (%v), want 0600", info.Mode().Perm(), err)
	}
	env := []string{toolsSocketEnv + "=" + socket}
	var out, errs bytes.Buffer
	if code := runTool([]string{"go-test"}, env, &out, &errs); code != 1 || out.String() != "--- FAIL: TestX\n" {
		t.Fatalf("exit %d, output %q, errors %q; want the broker's answer", code, out.String(), errs.String())
	}
	for _, bad := range [][]string{{"go", "test"}, {"go-test", "-exec=/evil"}, {"sh"}, {}, {"go-test\nsh"}} {
		if code := runTool(bad, env, &out, &errs); code == 0 {
			t.Errorf("the wrapper took %q", bad)
		}
	}
	c, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(c, "go test -exec /evil ./...\n")
	answer, _ := io.ReadAll(c)
	_ = c.Close()
	if !strings.HasPrefix(string(answer), "exit 2\n") {
		t.Errorf("the broker answered a request of its own making %q", answer)
	}
	if got := f.called(); !slices.Equal(got, []string{"go-test"}) {
		t.Fatalf("the broker ran %q, want only go-test", got)
	}
	if code := runTool([]string{"go-test"}, []string{toolsSocketEnv + "=relative/s"}, &out, &errs); code != toolExitUnavailable {
		t.Errorf("a socket that is not an absolute path: exit %d", code)
	}
}

// TestAToolWhoseWrapperGoesAwayIsStopped: the session's own command timeout
// ends its wrapper; the broker then stops the tool it was running, so no
// gate command outlives the command that asked for it.
func TestAToolWhoseWrapperGoesAwayIsStopped(t *testing.T) {
	socket := shortSocket(t)
	f := &toolRecorder{block: true, canceled: make(chan struct{})}
	stop, err := serveTools(context.Background(), socket, f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(c, "go-test\n")
	deadline := time.Now().Add(5 * time.Second)
	for len(f.called()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_ = c.Close()
	select {
	case <-f.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("the tool ran on after its wrapper went away")
	}
}

// TestASessionsToolsRunAsTheGateUserOverACopyOfItsWorktree: go test, go vet
// and gofmt run through sudo and sandbox-exec as the gate user, over a copy
// of the worktree as the session left it (its new, uncommitted files too),
// never in the worktree and never as the job; git's read-only commands run
// as the job, in the worktree, with git's system and global configuration
// cut off and no hook, so nothing the session wrote runs as the user that
// holds the Claude token.
func TestASessionsToolsRunAsTheGateUserOverACopyOfItsWorktree(t *testing.T) {
	w, r, _ := gitWorld(t)
	ctx := context.Background()
	dir, err := w.Prepare(ctx, "triage/alert-52")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Remove(ctx, dir)
	write(t, dir, "internal/x/x_test.go", fixedTest) // new and uncommitted: the copy must hold it
	run, done, err := w.OpenTools(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	before := len(r.argv)
	out, code := run(ctx, "go-test")
	if code != 1 || !strings.Contains(out, "--- FAIL: TestFixed") {
		t.Fatalf("go-test: exit %d, output %q; want the session's new test run and failing", code, out)
	}
	var viaGate bool
	for _, argv := range r.argv[before:] {
		if argv[0] == w.gate.sudo && slices.Contains(argv, "go") && slices.Contains(argv, "test") {
			viaGate = true
			if i := slices.Index(argv, "-C"); i < 0 || filepath.Base(argv[i+1]) != "tree" {
				t.Errorf("go test did not run in the gate's copy: %q", argv)
			}
		}
		if argv[0] == "go" {
			t.Errorf("the job ran %q itself", argv)
		}
	}
	if !viaGate {
		t.Fatalf("go test did not run through the gate: %q", r.argv[before:])
	}
	if out, code := run(ctx, "gofmt"); code != 0 {
		t.Errorf("gofmt: exit %d, %q", code, out)
	}
	out, code = run(ctx, "git-status")
	if code != 0 || !strings.Contains(out, "x_test.go") {
		t.Fatalf("git-status: exit %d, %q", code, out)
	}
	if out, code := run(ctx, "not-a-tool"); code == 0 {
		t.Errorf("an unknown tool ran: %q", out)
	}
	_ = done()
	if left, _ := os.ReadDir(filepath.Join(w.gate.root, "runs")); len(left) != 0 {
		t.Errorf("the session's gate run was left behind: %v", left)
	}
	var gitRan bool
	for i, argv := range r.argv[before:] {
		if argv[0] != "git" || !slices.Contains(argv, "status") {
			continue
		}
		gitRan = true
		env := r.envs[before+i]
		for _, e := range []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"} {
			if !slices.Contains(env, e) {
				t.Errorf("git-status ran without %s: %q", e, env)
			}
		}
		if !slices.Contains(argv, "core.hooksPath=/dev/null") || !slices.Contains(argv, "core.fsmonitor=false") {
			t.Errorf("git-status ran with hooks or a file-system monitor: %q", argv)
		}
	}
	if !gitRan {
		t.Errorf("git-status did not run as the job: %q", r.argv[before:])
	}
}

// TestALinkTheGatePlantsNeverRedirectsTheJobsWrites: a session's commands
// share one run folder, which the gate user can write in. A link its code
// planted there (in place of the archive an earlier command used) is never
// followed by the job's next write: the job writes each archive into a
// fresh folder of its own that the gate user can read but not change, and
// the run folder keeps the gate user from renaming or removing what the
// job made (the sticky bit).
func TestALinkTheGatePlantsNeverRedirectsTheJobsWrites(t *testing.T) {
	w, _, _ := gitWorld(t)
	ctx := context.Background()
	dir, err := w.Prepare(ctx, "triage/alert-53")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Remove(ctx, dir)
	run, done, err := w.OpenTools(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = done() }()
	if out, code := run(ctx, "gofmt"); code != 0 {
		t.Fatalf("gofmt: exit %d, %q", code, out)
	}
	runs, err := os.ReadDir(filepath.Join(w.gate.root, "runs"))
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs %v, %v; want the session's one", runs, err)
	}
	runDir := filepath.Join(w.gate.root, "runs", runs[0].Name())
	if info, err := os.Lstat(runDir); err != nil || info.Mode()&os.ModeSticky == 0 {
		t.Errorf("the run folder is not sticky (%v): the gate user could rename what the job made in it", err)
	}
	victim := filepath.Join(t.TempDir(), "job-state")
	if err := os.WriteFile(victim, []byte("the job's own\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// What the gate's code could do between two commands: put links where
	// the job writes, to a file only the job may change.
	for _, name := range []string{"commit.tar", "in", "stage"} {
		p := filepath.Join(runDir, name)
		_ = os.RemoveAll(p)
		if err := os.Symlink(victim, p); err != nil {
			t.Fatal(err)
		}
	}
	if out, code := run(ctx, "gofmt"); code != 0 {
		t.Fatalf("gofmt after the links: exit %d, %q", code, out)
	}
	raw, err := os.ReadFile(victim)
	info, serr := os.Stat(victim)
	if err != nil || string(raw) != "the job's own\n" || serr != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the job wrote through a link the gate planted: %d bytes, %v", len(raw), info.Mode())
	}
}

// TestTheGatesCopyIgnoresTheSessionsExportRules: the copy the gate checks
// is exactly the tree the session left: a .gitattributes the session wrote
// can neither leave a file out of it (export-ignore: a failing test the
// gate would never run, then pushed) nor rewrite one (export-subst).
func TestTheGatesCopyIgnoresTheSessionsExportRules(t *testing.T) {
	w, _, _ := gitWorld(t)
	ctx := context.Background()
	dir, err := w.Prepare(ctx, "triage/alert-54")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Remove(ctx, dir)
	write(t, dir, ".gitattributes", "internal/x/x_test.go export-ignore\ninternal/x/subst.txt export-subst\n")
	write(t, dir, "internal/x/x_test.go", fixedTest)
	write(t, dir, "internal/x/subst.txt", "$Format:%H$\n")
	run, done, err := w.OpenTools(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = done() }()
	out, code := run(ctx, "go-test")
	if code != 1 || !strings.Contains(out, "--- FAIL: TestFixed") {
		t.Errorf("go-test: exit %d; the session's failing test was left out of the gate's copy:\n%s", code, out)
	}
	runs, err := os.ReadDir(filepath.Join(w.gate.root, "runs"))
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs %v, %v; want the session's one", runs, err)
	}
	raw, err := os.ReadFile(filepath.Join(w.gate.root, "runs", runs[0].Name(), "tree", "internal", "x", "subst.txt"))
	if err != nil || string(raw) != "$Format:%H$\n" {
		t.Errorf("the gate's copy of subst.txt is %q (%v), not the session's", raw, err)
	}
}

// TestAGateFailureInASessionsCommandIsKept: a session's command that met a
// gate failure answers the wrapper with the gate's status, and the end of
// the session's commands returns that failure (errGateUnavailable).
func TestAGateFailureInASessionsCommandIsKept(t *testing.T) {
	w, _, _ := gitWorld(t)
	ctx := context.Background()
	dir, err := w.Prepare(ctx, "triage/alert-55")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Remove(ctx, dir)
	run, done, err := w.OpenTools(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if out, code := run(ctx, "gofmt"); code != 0 {
		t.Fatalf("gofmt: exit %d, %q", code, out)
	}
	w.gate.endGateProcesses = func(context.Context) error { return errors.New("the sudo rule does not allow it") }
	if out, code := run(ctx, "gofmt"); code != toolExitUnavailable {
		t.Errorf("gofmt with the gate failing: exit %d, %q; want %d", code, out, toolExitUnavailable)
	}
	w.gate.endGateProcesses = func(context.Context) error { return nil }
	if out, code := run(ctx, "gofmt"); code != 0 {
		t.Fatalf("gofmt with the gate back: exit %d, %q", code, out)
	}
	if err := done(); !errors.Is(err, errGateUnavailable) {
		t.Fatalf("done = %v, want the gate's failure kept", err)
	}
}

// stalledCatFile puts a git ahead of the real one on PATH whose cat-file
// never answers (it only sleeps, so killing it ends it); every other git
// command runs the real git. It signals nothing.
func stalledCatFile(t *testing.T) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	bin := t.TempDir()
	script(t, bin, "git", "case \" $* \" in *\" cat-file \"*) exec sleep 30 ;; esac\nexec '"+realGit+"' \"$@\"\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestAnArchiveThatStallsEndsAtTheCommandTimeout: the archive of a commit is
// written under the same time bound as every other gate command, whatever
// context the caller holds, so a stalled object read never holds the job.
func TestAnArchiveThatStallsEndsAtTheCommandTimeout(t *testing.T) {
	w, _, _ := gitWorld(t)
	stalledCatFile(t)
	w.run = timedRunner{execRunner{}, 300 * time.Millisecond}
	start := time.Now()
	err := w.treeTar(context.Background(), w.repoDir, "HEAD", io.Discard)
	if err == nil {
		t.Fatal("a stalled cat-file was taken for a written archive")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the archive held the job for %s past a 300ms command timeout", took)
	}
}

// TestACanceledSessionCommandIsNotAGateOutage: a command the session's
// wrapper gave up on (its context canceled) before it starts or while the
// session's own command runs is interrupted, not the gate's failure, so a later command that
// succeeds leaves the session's answer valid.
func TestACanceledSessionCommandIsNotAGateOutage(t *testing.T) {
	w, _, _ := gitWorld(t)
	ctx := context.Background()
	dir, err := w.Prepare(ctx, "triage/alert-56")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Remove(ctx, dir)
	run, done, err := w.OpenTools(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if out, code := run(canceled, "gofmt"); code != toolExitUnavailable {
		t.Errorf("canceled before the snapshot: exit %d, %q; want %d", code, out, toolExitUnavailable)
	}
	w.gate.run = stallingCommandRunner{}
	mid, cancelMid := context.WithCancel(ctx)
	go func() { time.Sleep(300 * time.Millisecond); cancelMid() }()
	start := time.Now()
	if out, code := run(mid, "gofmt"); code != toolExitUnavailable || time.Since(start) > 10*time.Second {
		t.Errorf("canceled while the command ran: exit %d after %s, %q", code, time.Since(start), out)
	}
	if err := done(); err != nil {
		t.Fatalf("done = %v: a canceled command was kept as the gate's failure", err)
	}
}

// stallingCommandRunner runs every gate command for real but the session's
// gofmt, which it marks launched (the launcher ran it) and holds until its
// context ends.
type stallingCommandRunner struct{}

func (stallingCommandRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	if slices.Contains(args, "gofmt") {
		if err := os.WriteFile(launchedMark(args), nil, 0o600); err != nil {
			return "", err
		}
		<-ctx.Done()
		return "", ctx.Err()
	}
	return execRunner{}.Run(ctx, dir, env, name, args...)
}

// TestAStalledGateStepIsAGateOutageUnderASessionDeadline: a gate-side step
// (the object read, the ports read of the export's copy) that stalls is the gate's failure even
// when the session's own deadline ends first: each such step has a time
// limit of its own, well under the wrapper's, and does not follow the
// session's context.
func TestAStalledGateStepIsAGateOutageUnderASessionDeadline(t *testing.T) {
	for _, step := range []string{"object read", "ports in use"} {
		t.Run(step, func(t *testing.T) {
			w, _, _ := gitWorld(t)
			ctx := context.Background()
			dir, err := w.Prepare(ctx, "triage/alert-57")
			if err != nil {
				t.Fatal(err)
			}
			defer w.Remove(ctx, dir)
			w.gate.stepTimeout = 600 * time.Millisecond
			if step == "object read" {
				stalledCatFile(t)
			} else {
				w.gate.listening = func(ctx context.Context) ([]int, error) { <-ctx.Done(); return nil, ctx.Err() }
			}
			run, done, err := w.OpenTools(ctx, dir)
			if err != nil {
				t.Fatal(err)
			}
			session, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
			defer cancel()
			start := time.Now()
			if out, code := run(session, "gofmt"); code != toolExitUnavailable || time.Since(start) > 10*time.Second {
				t.Errorf("stalled %s: exit %d after %s, %q", step, code, time.Since(start), out)
			}
			if err := done(); !errors.Is(err, errGateUnavailable) {
				t.Fatalf("done = %v: a stalled gate step was taken for an interrupted command", err)
			}
		})
	}
}

// launchedMark is the launch mark of a gate command line (the launcher's
// $0), or "" for a line that is not one.
func launchedMark(args []string) string {
	if i := slices.Index(args, launcher); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// stallingLauncherRunner runs every gate command for real but the
// session's gofmt, whose launcher it holds, never making its launch mark,
// until its context ends (the session gives up meanwhile: cancel): sudo or
// sandbox-exec stalled before the command started.
type stallingLauncherRunner struct{ cancel context.CancelFunc }

func (r stallingLauncherRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	if slices.Contains(args, "gofmt") {
		r.cancel()
		<-ctx.Done()
		return "", ctx.Err()
	}
	return execRunner{}.Run(ctx, dir, env, name, args...)
}

// TestAGateSideFailureIsAGateOutageWhateverTheSessionDid: a gate-side step
// (the launcher of the session's own command, the ports read before it)
// that fails or stalls is the gate's failure even when the session's
// context ends in the same moment; only a cancel while the session's own
// command runs is an interruption.
func TestAGateSideFailureIsAGateOutageWhateverTheSessionDid(t *testing.T) {
	cases := map[string]func(w *gitWorkspace, cancel context.CancelFunc){
		"stalled launcher": func(w *gitWorkspace, cancel context.CancelFunc) {
			w.gate.run = stallingLauncherRunner{cancel: cancel}
		},
		"ports read failing as the session gives up": func(w *gitWorkspace, cancel context.CancelFunc) {
			var calls int
			w.gate.listening = func(context.Context) ([]int, error) {
				calls++
				if calls == 1 { // the copy's
					return nil, nil
				}
				cancel()
				return nil, errors.New("netstat failed")
			}
		},
	}
	for name, setUp := range cases {
		t.Run(name, func(t *testing.T) {
			w, _, _ := gitWorld(t)
			ctx := context.Background()
			dir, err := w.Prepare(ctx, "triage/alert-58")
			if err != nil {
				t.Fatal(err)
			}
			defer w.Remove(ctx, dir)
			run, done, err := w.OpenTools(ctx, dir)
			if err != nil {
				t.Fatal(err)
			}
			session, cancel := context.WithCancel(ctx)
			defer cancel()
			setUp(w, cancel)
			if out, code := run(session, "gofmt"); code != toolExitUnavailable {
				t.Errorf("exit %d, %q; want %d", code, out, toolExitUnavailable)
			}
			err = done()
			var step *errGateStep
			if !errors.Is(err, errGateUnavailable) || !errors.As(err, &step) {
				t.Fatalf("done = %v: a gate-side failure was taken for an interrupted command", err)
			}
		})
	}
}

// TestAStalledPortsReadBeforeTheSessionsCommandIsAGateOutage: the ports
// read just before the session's own command (not the copy's) stalls past
// its limit while the session's deadline ends first: the gate's failure,
// named as a stall.
func TestAStalledPortsReadBeforeTheSessionsCommandIsAGateOutage(t *testing.T) {
	w, _, _ := gitWorld(t)
	ctx := context.Background()
	dir, err := w.Prepare(ctx, "triage/alert-59")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Remove(ctx, dir)
	w.gate.stepTimeout = 600 * time.Millisecond
	session, expireSession := controlledDeadline(ctx, time.Now().Add(150*time.Millisecond))
	defer expireSession()
	var expireStep context.CancelFunc
	w.gate.withDeadline = func(ctx context.Context, end time.Time) (context.Context, context.CancelFunc) {
		step, expire := controlledDeadline(ctx, end)
		expireStep = expire
		return step, expire
	}
	var calls int
	w.gate.listening = func(ctx context.Context) ([]int, error) {
		calls++
		if calls == 1 { // the copy's
			return nil, nil
		}
		// The gate-side limit is independent of the session's deadline
		// (docs/manual/alerts-and-dashboard.md, "What the session and the
		// gate can and cannot do"). Expire the session first, only once
		// the command's ports read has actually started.
		expireSession()
		if ctx.Err() != nil {
			t.Fatalf("the session's deadline ended the ports read: %v", ctx.Err())
		}
		expireStep()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	run, done, err := w.OpenTools(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if out, code := run(session, "gofmt"); code != toolExitUnavailable {
		t.Errorf("exit %d, %q; want %d", code, out, toolExitUnavailable)
	}
	if calls != 2 {
		t.Fatalf("the ports were read %d times, want the copy's and the command's", calls)
	}
	if !errors.Is(session.Err(), context.DeadlineExceeded) {
		t.Fatalf("session = %v, want its deadline exceeded first", session.Err())
	}
	err = done()
	var step *errGateStep
	if !errors.Is(err, errGateStalled) || !errors.Is(err, errGateUnavailable) || !errors.As(err, &step) {
		t.Fatalf("done = %v, want the stalled ports read kept as the gate's failure", err)
	}
}

// controlledDeadline supplies a deadline that expires explicitly, so the
// gate/session ordering does not depend on scheduling or subprocess latency.
func controlledDeadline(parent context.Context, end time.Time) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	return controlledDeadlineContext{Context: ctx, end: end}, func() { cancel(context.DeadlineExceeded) }
}

type controlledDeadlineContext struct {
	context.Context
	end time.Time
}

func (c controlledDeadlineContext) Deadline() (time.Time, bool) { return c.end, true }
func (c controlledDeadlineContext) Err() error {
	if c.Context.Err() != nil {
		return context.Cause(c.Context)
	}
	return nil
}

// TestAStepInsideAStepEndsWithIt: a gate-side step started inside another
// (the ports read of the export's copy) ends no later than the enclosing
// step, never a fresh limit of its own; a step started from the session's
// context keeps its own limit whatever the session's deadline; and the
// limit is safely under Claude Code's default two-minute command timeout.
func TestAStepInsideAStepEndsWithIt(t *testing.T) {
	if gateStepTimeout > time.Minute {
		t.Errorf("gateStepTimeout = %s, want at most a minute", gateStepTimeout)
	}
	g := &gateBox{stepTimeout: 500 * time.Millisecond}
	outer, cancel := g.stepContext(context.Background())
	defer cancel()
	time.Sleep(300 * time.Millisecond)
	inner, cancelInner := g.stepContext(outer)
	defer cancelInner()
	od, _ := outer.Deadline()
	id, _ := inner.Deadline()
	if id.After(od) {
		t.Errorf("the inner step ends %s after its enclosing step", id.Sub(od))
	}
	session, cancelSession := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelSession()
	step, cancelStep := g.stepContext(session)
	defer cancelStep()
	if sd, _ := step.Deadline(); time.Until(sd) < 400*time.Millisecond {
		t.Errorf("a step from the session's context has %s left, want its own limit", time.Until(sd))
	}
}
