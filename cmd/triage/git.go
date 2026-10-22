package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/procgroup"
)

// runner runs one program in dir with exactly env, and returns the last
// maxOutput bytes of what it printed.
type runner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) (string, error)
}

// maxOutput is the most of a command's output kept: its end, where go test
// reports.
const maxOutput = 64 << 10

// waitDelay is how long a command's output is still read after the command
// was killed, before the job stops waiting for it.
const waitDelay = 5 * time.Second

// execRunner runs programs for real, each in its own process group, killed
// whole when its context ends; output past maxOutput is dropped from the
// front. What a command printed never goes into an error: it may hold what
// the journal slice held.
type execRunner struct {
	// stop, when set, ends the processes of the command that another macOS
	// user owns, which this user cannot signal (gateBox).
	stop func()
}

func (r execRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	//nolint:gosec // G204: every program is git, go, sudo or the operator's -check-cmd, run on purpose; no argument comes from GitHub, and the session's only ones are the names of its new tests, which match a fixed pattern.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if r.stop != nil {
			r.stop()
		}
		return procgroup.Kill(cmd.Process.Pid)
	}
	cmd.WaitDelay = waitDelay
	out := &tailBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	return out.String(), err
}

// disqualifiers are what go test prints for a package that failed to build
// or set up, or a test run out of time: each fails a run whatever the tests
// said (redShown).
var disqualifiers = []string{"[build failed]", "[setup failed]", "panic: test timed out"}

// tailBuffer keeps the last maxOutput bytes written to it, and notes every
// disqualifier anywhere in the whole output, even one long cut from the
// tail, which String then names at the end.
type tailBuffer struct {
	buf   []byte
	carry []byte
	seen  []string
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	scan := append(slices.Clone(t.carry), p...)
	longest := 0
	for _, d := range disqualifiers {
		longest = max(longest, len(d))
		if !slices.Contains(t.seen, d) && bytes.Contains(scan, []byte(d)) {
			t.seen = append(t.seen, d)
		}
	}
	t.carry = append([]byte(nil), scan[max(0, len(scan)-longest+1):]...)
	n := len(p)
	if len(p) >= maxOutput {
		t.buf = append(t.buf[:0], p[len(p)-maxOutput:]...)
		return n, nil
	}
	if over := len(t.buf) + len(p) - maxOutput; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) String() string {
	s := string(t.buf)
	for _, d := range t.seen {
		if !strings.Contains(s, d) {
			s += "\n" + d + "\n"
		}
	}
	return s
}

// gitWorkspace is the job's own clone of the repository, a fresh worktree of
// main for each analysis, and the gate a fix goes through before it is
// pushed: the normal flow of the repository (AGENTS.md rule 1, tests first;
// rule 2, the check passes), made mechanical. The gate's commands run code
// the session wrote, so they run as the gate's own user in a sandbox
// (gateBox), each over a copy of one commit, never in this worktree. Only
// the push is handed the token, in its environment, never on its command
// line; nothing is ever forced or pushed to main.
type gitWorkspace struct {
	repoDir, workRoot string
	run               runner
	// env is every git command's environment (gitEnv).
	env []string
	// gate runs the red and green checks.
	gate *gateBox
	// checkCmd is the green check.
	checkCmd []string
	// pushURL is where a branch is pushed: the code repository on github.com.
	pushURL, token string
	// fetchVulnDB writes Go's vulnerability database into a folder, for the
	// green check's govulncheck, which has no network in the sandbox.
	fetchVulnDB func(ctx context.Context, dir string) error
	// prepared is each worktree Prepare made: its .git file and its branch.
	prepared map[string]preparedTree
}

// preparedTree is one worktree as Prepare made it.
type preparedTree struct{ gitFile, branch string }

// gitAuthor is the identity of the job's commits.
var gitAuthor = []string{
	"GIT_AUTHOR_NAME=TrendInvesting triage", "GIT_AUTHOR_EMAIL=triage@trend-investing.invalid",
	"GIT_COMMITTER_NAME=TrendInvesting triage", "GIT_COMMITTER_EMAIL=triage@trend-investing.invalid",
}

// gitEnv is base with git's own configuration cut off: no system or global
// configuration (so no credential helper, such as the macOS keychain's, is
// asked), no prompt, and the job's identity.
func gitEnv(base []string) []string {
	return append(slices.Clone(base), append([]string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}, gitAuthor...)...)
}

// gitSafe are the settings every git command of the job runs with: no hook
// and no file-system monitor, the two ways a repository's own files could
// make git run a program.
var gitSafe = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}

func (w *gitWorkspace) git(ctx context.Context, dir string, args ...string) error {
	_, err := w.run.Run(ctx, dir, w.env, "git", append(slices.Clone(gitSafe), args...)...)
	return err
}

// gitOutput runs git and returns what it printed; it is used for git's own
// listings only.
func (w *gitWorkspace) gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	//nolint:gosec // G204: git's own fixed listings; the only variable argument is a folder the job made.
	cmd := exec.CommandContext(ctx, "git", append(slices.Clone(gitSafe), args...)...)
	cmd.Dir, cmd.Env = dir, w.env
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s failed", args[0])
	}
	return string(out), nil
}

// goEnv is the environment of the job's own go commands, which fill the
// module cache the session and the gate read: online, writing the cache, no
// workspace file and no toolchain download.
func (w *gitWorkspace) goEnv() []string {
	return append(slices.Clone(w.env), "GOMODCACHE="+w.gate.modCache(), "GOFLAGS=-mod=mod", "GOWORK=off", "GOTOOLCHAIN=local")
}

// Sweep removes the gate's run folders a killed job left behind.
func (w *gitWorkspace) Sweep(ctx context.Context) error { return w.gate.Sweep(ctx) }

// GateAvailable reports whether the gate can run at all.
func (w *gitWorkspace) GateAvailable(ctx context.Context) error {
	return w.gate.Available(ctx)
}

// Prepare fetches main and makes a fresh worktree of it on branch, replacing
// any worktree or branch an earlier run left, and fills the module cache for
// main's requirements (the session and the gate are offline).
func (w *gitWorkspace) Prepare(ctx context.Context, branch string) (string, error) {
	if err := w.git(ctx, w.repoDir, "fetch", "--quiet", "origin", "main"); err != nil {
		return "", errors.New("git fetch of main failed")
	}
	dir := filepath.Join(w.workRoot, strings.ReplaceAll(branch, "/", "-"))
	_ = w.git(ctx, w.repoDir, "worktree", "remove", "--force", dir)
	_ = os.RemoveAll(dir)
	_ = w.git(ctx, w.repoDir, "worktree", "prune")
	_ = w.git(ctx, w.repoDir, "branch", "-D", branch)
	if err := os.MkdirAll(w.workRoot, 0o700); err != nil {
		return "", errors.New("the work folder could not be made")
	}
	if err := w.git(ctx, w.repoDir, "worktree", "add", "--quiet", "-b", branch, dir, "origin/main"); err != nil {
		return "", errors.New("git worktree add failed")
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		return "", errors.New("the worktree's .git file could not be read")
	}
	if w.prepared == nil {
		w.prepared = map[string]preparedTree{}
	}
	w.prepared[dir] = preparedTree{gitFile: string(raw), branch: branch}
	if _, err := w.run.Run(ctx, dir, w.goEnv(), "go", "mod", "download"); err != nil {
		return "", errors.New("go mod download failed, so the module cache could not be filled")
	}
	// tidy -diff reads what make check's own tidy -diff will, and changes
	// nothing; its answer about main is not the job's to judge.
	_, _ = w.run.Run(ctx, dir, w.goEnv(), "go", "mod", "tidy", "-diff")
	return dir, nil
}

// checkGitFile refuses a worktree whose .git file is not the one Prepare
// made: a session that pointed it elsewhere could make the job's next git
// command read a configuration it wrote.
func (w *gitWorkspace) checkGitFile(dir string) error {
	info, err := os.Lstat(filepath.Join(dir, ".git"))
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("the worktree's .git file was changed")
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil || string(raw) != w.prepared[dir].gitFile {
		return errors.New("the worktree's .git file was changed")
	}
	return nil
}

// Changed is every path the session changed, added or removed.
func (w *gitWorkspace) Changed(ctx context.Context, dir string) ([]string, error) {
	if err := w.checkGitFile(dir); err != nil {
		return nil, err
	}
	out, err := w.gitOutput(ctx, dir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	return parsePorcelain(out), nil
}

// parsePorcelain reads git status --porcelain=v1 -z: each entry is two status
// letters, a space and a path; a rename or copy is followed by its old path.
func parsePorcelain(out string) []string {
	var paths []string
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		paths = append(paths, f[3:])
		if f[0] == 'R' || f[0] == 'C' {
			if i+1 < len(fields) && fields[i+1] != "" {
				paths = append(paths, fields[i+1])
			}
			i++
		}
	}
	return paths
}

// errNoNewTest is a tests' commit that adds no test function, so no test
// can be shown failing by name.
var errNoNewTest = errors.New("the change adds no new test function")

// newTestFunc is a Go test function a diff adds.
var newTestFunc = regexp.MustCompile(`(?m)^\+func (Test[A-Za-z0-9_]*)\(\w+ \*testing\.T\)`)

// Gate commits the tests alone, then the fix, and passes only if, at the
// tests' commit, `go test -run` over the new tests shows one of them failing
// by name (red), and the check passes at the fix's (green). Each check runs
// in the gate (gateBox) over a copy of its commit.
func (w *gitWorkspace) Gate(ctx context.Context, dir string, tests, fix []string, title string) error {
	if err := w.checkGitFile(dir); err != nil {
		return err
	}
	for _, c := range []struct {
		files   []string
		message string
	}{
		{tests, "Test first: " + title},
		{fix, "Fix: " + title},
	} {
		if err := w.git(ctx, dir, append([]string{"add", "-A", "--"}, c.files...)...); err != nil {
			return errors.New("git add failed")
		}
		if err := w.git(ctx, dir, "-c", "commit.gpgsign=false", "commit", "--quiet", "--no-verify", "-m", c.message+"\n\nCo-Authored-By: Claude <noreply@anthropic.com>"); err != nil {
			return errors.New("git commit failed")
		}
	}
	diff, err := w.gitOutput(ctx, dir, "diff", "--unified=0", "HEAD~2", "HEAD~1", "--", "*_test.go")
	if err != nil {
		return err
	}
	var names []string
	for _, m := range newTestFunc.FindAllStringSubmatch(diff, -1) {
		if !slices.Contains(names, m[1]) {
			names = append(names, m[1])
		}
	}
	if len(names) == 0 {
		return errNoNewTest
	}
	out, err := w.inGate(ctx, dir, "HEAD~1", []string{"go", "test", "-count=1", "-run", "^(" + strings.Join(names, "|") + ")$", "./..."})
	if errors.Is(err, errGateUnavailable) {
		return err
	}
	if ctx.Err() != nil || !redShown(out, err, names) {
		return errNotRed
	}
	if err := w.fetchVulnDB(ctx, w.gate.vulnDB()); err != nil {
		return fmt.Errorf("%w: the vulnerability database could not be fetched for the check", errGateUnavailable)
	}
	if _, err := w.inGate(ctx, dir, "HEAD", w.checkCmd); err != nil {
		if errors.Is(err, errGateUnavailable) {
			return err
		}
		return fmt.Errorf("`%s` failed at the fix's commit", strings.Join(w.checkCmd, " "))
	}
	return nil
}

// inGate runs argv in a fresh run of the gate over a copy of rev. A run
// folder, a copy or a command that cannot be started (gateBox.Run) is the
// gate's failure (errGateUnavailable), never the change's.
func (w *gitWorkspace) inGate(ctx context.Context, dir, rev string, argv []string) (string, error) {
	runDir, done, err := w.gate.newRun(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errGateUnavailable, err)
	}
	defer done()
	if err := w.exportTree(ctx, dir, rev, runDir); err != nil {
		return "", err
	}
	out, err := w.gate.Run(ctx, runDir, "tree", argv)
	var exit *exec.ExitError
	if err != nil && !errors.Is(err, errGateUnavailable) && !errors.As(err, &exit) && ctx.Err() == nil {
		return out, fmt.Errorf("%w: the check could not be started", errGateUnavailable)
	}
	return out, err
}

// redShown reports whether a go test run shows one of the new tests failing:
// the test binary's own failure exit, a "--- FAIL:" line naming one of names
// (or a subtest of one), and no package that failed to build or set up and
// no timeout, which would fail whatever the test said.
func redShown(out string, err error, names []string) bool {
	var exit *exec.ExitError
	if err == nil || !errors.As(err, &exit) || exit.ExitCode() != 1 {
		return false
	}
	for _, bad := range disqualifiers {
		if strings.Contains(out, bad) {
			return false
		}
	}
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "--- FAIL: ")
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(rest, " ")
		name, _, _ = strings.Cut(name, "/")
		if slices.Contains(names, name) {
			return true
		}
	}
	return false
}

// errBinary is a commit holding a file no line check can read.
var errBinary = errors.New("the change holds a binary file, which cannot be checked for identifiers")

// Added is what the push publishes, for the check that no identifier is
// pushed: the commits' messages, every path they add, rename or remove, and
// every line they add. A binary file fails it.
func (w *gitWorkspace) Added(ctx context.Context, dir string) (string, error) {
	messages, err := w.gitOutput(ctx, dir, "log", "--format=%B", "origin/main..HEAD")
	if err != nil {
		return "", err
	}
	numstat, err := w.gitOutput(ctx, dir, "diff", "--numstat", "-z", "--no-renames", "origin/main..HEAD")
	if err != nil {
		return "", err
	}
	diff, err := w.gitOutput(ctx, dir, "diff", "--no-renames", "origin/main..HEAD")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(messages)
	for _, entry := range strings.Split(numstat, "\x00") {
		added, rest, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		removed, path, _ := strings.Cut(rest, "\t")
		if added == "-" || removed == "-" {
			return "", errBinary
		}
		b.WriteString(path + "\n")
	}
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			b.WriteString(line + "\n")
		}
	}
	return b.String(), nil
}

// Push pushes the worktree's head to branch on the code repository, never
// forced. The token reaches git as an HTTP header in its environment
// (GIT_CONFIG_COUNT), so it is never on a command line another process can
// read.
func (w *gitWorkspace) Push(ctx context.Context, dir, branch string) error {
	if err := w.checkGitFile(dir); err != nil {
		return err
	}
	header := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+w.token))
	env := append(slices.Clone(w.env), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader", "GIT_CONFIG_VALUE_0="+header)
	args := append(slices.Clone(gitSafe), "-c", "credential.helper=", "push", "--quiet", w.pushURL, "HEAD:refs/heads/"+branch)
	if _, err := w.run.Run(ctx, dir, env, "git", args...); err != nil {
		return errors.New("git push failed (a branch of that name may already be on GitHub)")
	}
	return nil
}

// Remove removes the worktree and its local branch; a pushed branch stays on
// GitHub. No git command runs inside the worktree: the session may have
// changed it.
func (w *gitWorkspace) Remove(ctx context.Context, dir string) {
	_ = w.git(ctx, w.repoDir, "worktree", "remove", "--force", dir)
	_ = os.RemoveAll(dir)
	_ = w.git(ctx, w.repoDir, "worktree", "prune")
	if b := w.prepared[dir].branch; b != "" {
		_ = w.git(ctx, w.repoDir, "branch", "-D", b)
	}
	delete(w.prepared, dir)
}
