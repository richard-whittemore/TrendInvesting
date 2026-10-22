package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The session's commands run nowhere near the Claude process
// (docs/manual/alerts-and-dashboard.md, "What the session and the gate can
// and cannot do"): the wrapper the session may run (runTool) executes
// nothing itself. It hands one name from sessionTools to the job over a
// Unix socket only the job's user can open (a folder of mode 700, the
// socket 600), and prints the answer. The job, outside Claude Code's
// sandbox, runs that name's fixed command line: go test, go vet and gofmt
// as the gate user, through the gate's sudo rule and sandbox, over a copy
// of the worktree as the session left it; git's read-only commands as the
// job, in the worktree, with no configuration but the repository's own,
// no hook and no file-system monitor, so git runs nothing the session
// wrote. No code the session wrote runs as the user that holds the Claude
// token.

// toolsSocketEnv names the job's socket in the session's environment.
const toolsSocketEnv = "TI_TRIAGE_TOOLS"

// toolExitUnavailable is the wrapper's exit status when the job could not
// run the command (the gate unavailable, the socket unreachable).
const toolExitUnavailable = 125

// toolRun runs one of sessionTools by name and returns its output and exit
// status.
type toolRun func(ctx context.Context, name string) (out string, code int)

// toolHost opens a session's commands over its worktree: run runs each,
// done ends them and returns the gate's failure (errGateUnavailable) if
// any command met one (gitWorkspace.OpenTools; tests: a fake).
type toolHost interface {
	OpenTools(ctx context.Context, dir string) (run toolRun, done func() error, err error)
}

// maxToolRequest bounds a request: a tool's name and its line break.
const maxToolRequest = 64

// serveTools serves the session's commands at path until stop: one
// connection at a time, so no two gate commands run at once (each ends
// every process of the gate user's after it), each one line naming a tool,
// answered "exit <status>" and a line break, then its output. A name not in
// sessionTools is refused without running anything. A wrapper that goes
// away (the session's own time for a command) ends the command it asked
// for.
func serveTools(ctx context.Context, path string, run toolRun) (stop func(), err error) {
	if len(path) >= 104 {
		return nil, errors.New("the session's socket path is longer than a Unix socket's bound")
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, errors.New("the session's socket could not be made")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, errors.New("the session's socket could not be made this user's alone")
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			serveTool(ctx, c, run)
		}
	}()
	return func() {
		cancel()
		_ = l.Close()
		<-done
		_ = os.Remove(path)
	}, nil
}

// serveTool answers one connection.
func serveTool(ctx context.Context, c net.Conn, run toolRun) {
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(c, maxToolRequest)).ReadString('\n')
	name := strings.TrimSuffix(line, "\n")
	if err != nil || sessionTools[name] == nil {
		_, _ = fmt.Fprintf(c, "exit 2\ntriage tool: name one of %s\n", strings.Join(sessionToolNames(), ", "))
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, c) // returns when the wrapper goes away
		cancel()
	}()
	out, code := run(ctx, name)
	_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, _ = fmt.Fprintf(c, "exit %d\n%s", code, out)
}

// runTool is the wrapper every command of the session runs through
// (sessionTools, allowedTools): it takes exactly one name from the fixed
// list, sends that name alone to the job's socket (toolsSocketEnv), prints
// what the job answers and exits with its status. It runs no program and
// reads no other variable: whatever the session asks, the command line is
// the job's.
func runTool(args, environ []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || sessionTools[args[0]] == nil {
		_, _ = fmt.Fprintf(stderr, "triage tool: name one of %s\n", strings.Join(sessionToolNames(), ", "))
		return 2
	}
	var path string
	for _, e := range environ {
		if v, ok := strings.CutPrefix(e, toolsSocketEnv+"="); ok {
			path = v
		}
	}
	if !filepath.IsAbs(path) {
		_, _ = fmt.Fprintf(stderr, "triage tool: %s names no socket of the job's\n", toolsSocketEnv)
		return toolExitUnavailable
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "triage tool: the job's socket cannot be reached")
		return toolExitUnavailable
	}
	defer func() { _ = c.Close() }()
	if _, err := fmt.Fprintf(c, "%s\n", args[0]); err != nil {
		_, _ = fmt.Fprintln(stderr, "triage tool: the job's socket cannot be written")
		return toolExitUnavailable
	}
	r := bufio.NewReader(c)
	head, err := r.ReadString('\n')
	status, ok := strings.CutPrefix(strings.TrimSuffix(head, "\n"), "exit ")
	code, cerr := strconv.Atoi(status)
	if err != nil || !ok || cerr != nil || code < 0 || code > 255 {
		_, _ = fmt.Fprintln(stderr, "triage tool: the job's answer was not understood")
		return toolExitUnavailable
	}
	_, _ = io.Copy(stdout, r)
	return code
}

// gitToolArgs is a git tool's command line as the job runs it: git's own
// safe settings (gitSafe: no hook, no file-system monitor), then the tool's
// fixed arguments.
func gitToolArgs(name string) []string {
	return append(slices.Clone(gitSafe), sessionTools[name][1:]...)
}

// OpenTools makes one gate run for a session's commands over its worktree
// dir; done removes it and returns the first gate failure a command met
// (errGateUnavailable), so the job's handling of a gate failure (the issue
// tried again later) applies whatever the session answered. Each command's
// build cache stays in that run, so a second go test is quick, and no other
// session's or check's cache is fed by it.
func (w *gitWorkspace) OpenTools(ctx context.Context, dir string) (run toolRun, done func() error, err error) {
	runDir, remove, err := w.gate.newRun(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errGateUnavailable, err)
	}
	var mu sync.Mutex
	var failed error
	run = func(ctx context.Context, name string) (string, int) {
		out, code, gateErr := w.sessionTool(ctx, dir, runDir, name)
		if gateErr != nil {
			mu.Lock()
			if failed == nil {
				failed = gateErr
			}
			mu.Unlock()
		}
		return out, code
	}
	return run, func() error {
		remove()
		mu.Lock()
		defer mu.Unlock()
		return failed
	}, nil
}

// sessionTool runs one of sessionTools for the session in dir: git's
// read-only commands as the job in the worktree, the others as the gate
// user over a copy of the worktree as it stands (snapshot), in runDir.
// gateErr is the gate's failure (errGateUnavailable), if the command met
// one.
func (w *gitWorkspace) sessionTool(ctx context.Context, dir, runDir, name string) (out string, code int, gateErr error) {
	argv := sessionTools[name]
	if argv == nil {
		return "triage tool: no such tool\n", 2, nil
	}
	if err := w.checkGitFile(dir); err != nil {
		// A worktree's git file the session changed is the session's doing,
		// never the gate's failure.
		return "triage tool: " + err.Error() + "\n", toolExitUnavailable, nil //nolint:nilerr // the session's answer, not the gate's error
	}
	if argv[0] == "git" {
		gitOut, err := w.run.Run(ctx, dir, w.env, "git", gitToolArgs(name)...)
		return gitOut, exitStatus(err), nil
	}
	if ctx.Err() != nil {
		return interrupted, toolExitUnavailable, nil //nolint:nilerr // the session gave up: not the gate's error
	}
	// Every gate-side step runs under a time limit of its own, never the
	// session's context: a step that stalls is the gate's failure whoever
	// gave up on the command meanwhile.
	step, cancel := w.gate.stepContext(ctx)
	tree, err := w.snapshot(step, dir)
	if err != nil {
		cancel()
		return w.gateStepFailure(step, "", fmt.Errorf("%w: %w", errGateUnavailable, err))
	}
	err = w.exportTree(step, dir, tree, runDir)
	cancel()
	if err != nil {
		return w.gateStepFailure(step, "", err)
	}
	out, err = w.gate.Run(ctx, runDir, "tree", argv)
	if errors.Is(err, errGateUnavailable) {
		return w.toolFailure(ctx, out+"\n", err)
	}
	if err != nil && ctx.Err() != nil {
		return out + interrupted, toolExitUnavailable, nil //nolint:nilerr // the session gave up: not the gate's error
	}
	return out, exitStatus(err), nil
}

// interrupted is the answer to a command its session gave up on.
const interrupted = "triage tool: the command was interrupted\n"

// gateStepFailure answers a command whose gate-side step failed (step is
// that step's context): the gate's failure, kept, naming a step that ran
// past its time limit.
func (w *gitWorkspace) gateStepFailure(step context.Context, out string, err error) (text string, code int, gateErr error) {
	if errors.Is(step.Err(), context.DeadlineExceeded) && !errors.Is(err, errGateStalled) {
		err = fmt.Errorf("%w: %w", err, errGateStalled)
	}
	return out + "triage tool: " + err.Error() + "\n", toolExitUnavailable, err
}

// toolFailure answers a session's command that met a gate failure err while
// the session's own command ran. A command whose context was canceled (the
// wrapper gave up on it) is interrupted, not the gate's failure, unless the
// gate user's processes could not be ended or a gate-side step ran past its
// own time limit: those are kept whatever cancelled the command.
func (w *gitWorkspace) toolFailure(ctx context.Context, out string, err error) (text string, code int, gateErr error) {
	if ctx.Err() != nil && !errors.Is(err, errProcessesLeft) && !errors.Is(err, errGateStalled) {
		return out + interrupted, toolExitUnavailable, nil //nolint:nilerr // the session gave up: not the gate's error
	}
	return out + "triage tool: " + err.Error() + "\n", toolExitUnavailable, err
}

// exitStatus is a command's exit status: 0, its own, or 1 for one that
// ended otherwise.
func exitStatus(err error) int {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit) && exit.ExitCode() > 0:
		return exit.ExitCode()
	}
	return 1
}

// snapshot is the tree of the worktree at dir as it stands, every file the
// session changed, added or removed included and .gitignore's left out,
// written through an index of its own beside the worktree (never the
// worktree's), so nothing of the worktree's state changes. Git stores a
// link as a link: no file outside the worktree is read into it.
func (w *gitWorkspace) snapshot(ctx context.Context, dir string) (string, error) {
	index := dir + ".snapshot-index"
	_ = os.Remove(index)
	defer func() { _ = os.Remove(index) }()
	env := append(slices.Clone(w.env), "GIT_INDEX_FILE="+index)
	for _, args := range [][]string{{"read-tree", "HEAD"}, {"add", "-A"}} {
		if _, err := w.run.Run(ctx, dir, env, "git", append(slices.Clone(gitSafe), args...)...); err != nil {
			return "", fmt.Errorf("git %s of the worktree failed", args[0])
		}
	}
	//nolint:gosec // G204: git's own fixed command, in a folder the job made.
	cmd := exec.CommandContext(ctx, "git", append(slices.Clone(gitSafe), "write-tree")...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.Output()
	if err != nil {
		return "", errors.New("git write-tree of the worktree failed")
	}
	return strings.TrimSpace(string(out)), nil
}
