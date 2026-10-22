package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
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
	done()
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
