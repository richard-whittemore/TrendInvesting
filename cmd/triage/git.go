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
	"slices"
	"strings"
)

// runner runs one program in dir with exactly env.
type runner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) error
}

// execRunner runs programs for real. Their output is discarded, never put in
// an error: it may hold what the journal slice held.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) error {
	//nolint:gosec // G204: every program is git or the operator's own -test-cmd and -check-cmd, run on purpose; no argument comes from GitHub or the session.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	return cmd.Run()
}

// gitWorkspace is the job's own clone of the repository, a fresh worktree of
// main for each analysis, and the gate a fix goes through before it is
// pushed: the normal flow of the repository (AGENTS.md rule 1, tests first;
// rule 2, the check passes), made mechanical. Only the push is handed the
// token, in its environment, never on its command line; nothing is ever
// forced or pushed to main.
type gitWorkspace struct {
	repoDir, workRoot string
	run               runner
	// env is every git command's environment (gitEnv); checkEnv is the test
	// and check commands', which run the session's code and hold no token.
	env, checkEnv     []string
	testCmd, checkCmd []string
	// pushURL is where a branch is pushed: the repository on github.com.
	pushURL, token string
}

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

func (w *gitWorkspace) git(ctx context.Context, dir string, args ...string) error {
	return w.run.Run(ctx, dir, w.env, "git", args...)
}

// gitOutput runs git and returns what it printed; it is used for git's own
// listings only.
func (w *gitWorkspace) gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	//nolint:gosec // G204: git's own fixed listings; the only variable argument is a folder the job made.
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir, cmd.Env = dir, w.env
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s failed", args[0])
	}
	return string(out), nil
}

// Prepare fetches main and makes a fresh worktree of it on branch, replacing
// any worktree or branch an earlier run left.
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
	return dir, nil
}

// Changed is every path the session changed, added or removed.
func (w *gitWorkspace) Changed(ctx context.Context, dir string) ([]string, error) {
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

// Gate commits the tests alone, then the fix, and passes only if the test
// command fails at the tests' commit (red) and the check passes at the
// fix's (green).
func (w *gitWorkspace) Gate(ctx context.Context, dir string, tests, fix []string, title string) error {
	branch, err := w.gitOutput(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	branch = strings.TrimSpace(branch)
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
	if err := w.git(ctx, dir, "checkout", "--quiet", "--detach", "HEAD~1"); err != nil {
		return errors.New("git checkout of the tests' commit failed")
	}
	red := w.run.Run(ctx, dir, w.checkEnv, w.testCmd[0], w.testCmd[1:]...)
	if err := w.git(ctx, dir, "checkout", "--quiet", branch); err != nil {
		return errors.New("git checkout of the fix failed")
	}
	if red == nil {
		return errNotRed
	}
	if err := w.run.Run(ctx, dir, w.checkEnv, w.checkCmd[0], w.checkCmd[1:]...); err != nil {
		return fmt.Errorf("`%s` failed at the fix's commit", strings.Join(w.checkCmd, " "))
	}
	return nil
}

// Added is the commits' messages and every line they add, for the check that
// no identifier is pushed.
func (w *gitWorkspace) Added(ctx context.Context, dir string) (string, error) {
	messages, err := w.gitOutput(ctx, dir, "log", "--format=%B", "origin/main..HEAD")
	if err != nil {
		return "", err
	}
	diff, err := w.gitOutput(ctx, dir, "diff", "origin/main..HEAD")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(messages)
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			b.WriteString(line + "\n")
		}
	}
	return b.String(), nil
}

// Push pushes the worktree's head to branch on the repository, never forced.
// The token reaches git as an HTTP header in its environment
// (GIT_CONFIG_COUNT), so it is never on a command line another process can
// read.
func (w *gitWorkspace) Push(ctx context.Context, dir, branch string) error {
	header := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+w.token))
	env := append(slices.Clone(w.env), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader", "GIT_CONFIG_VALUE_0="+header)
	if err := w.run.Run(ctx, dir, env, "git", "-c", "credential.helper=", "push", "--quiet", w.pushURL, "HEAD:refs/heads/"+branch); err != nil {
		return errors.New("git push failed (a branch of that name may already be on GitHub)")
	}
	return nil
}

// Remove removes the worktree and its local branch; a pushed branch stays on
// GitHub.
func (w *gitWorkspace) Remove(ctx context.Context, dir string) {
	branch, _ := w.gitOutput(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	_ = w.git(ctx, w.repoDir, "worktree", "remove", "--force", dir)
	if b := strings.TrimSpace(branch); b != "" && b != "HEAD" {
		_ = w.git(ctx, w.repoDir, "branch", "-D", b)
	}
}
