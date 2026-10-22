package main

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// recordingRunner runs each command for real and records its argv and
// environment.
type recordingRunner struct {
	inner execRunner
	argv  [][]string
	envs  [][]string
}

func (r *recordingRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) error {
	r.argv = append(r.argv, append([]string{name}, args...))
	r.envs = append(r.envs, env)
	return r.inner.Run(ctx, dir, env, name, args...)
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitWorld is an origin repository with one commit on main, the job's own
// clone of it, and a workspace over them whose check passes only once
// code.txt says "fixed".
func gitWorld(t *testing.T) (*gitWorkspace, *recordingRunner, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	origin := filepath.Join(root, "origin.git")
	clone := filepath.Join(root, "clone")
	if err := os.MkdirAll(seed, 0o700); err != nil {
		t.Fatal(err)
	}
	mustGit(t, seed, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "code.txt"), []byte("broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, seed, "add", ".")
	mustGit(t, seed, "commit", "--quiet", "-m", "seed")
	mustGit(t, root, "clone", "--quiet", "--bare", seed, origin)
	mustGit(t, root, "clone", "--quiet", origin, clone)
	r := &recordingRunner{}
	check := []string{"grep", "-q", "fixed", "code.txt"}
	w := &gitWorkspace{
		repoDir: clone, workRoot: filepath.Join(root, "work"), run: r,
		env: gitEnv([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + root}), checkEnv: []string{"PATH=" + os.Getenv("PATH")},
		testCmd: check, checkCmd: check, pushURL: origin, token: "ghp_secrettokensecrettokensecret00",
	}
	return w, r, origin
}

// write changes a file in the worktree as Claude's session would.
func write(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTheGateCommitsTheTestFirstRedThenTheFixGreenAndPushesTheBranch: the
// change reaches the remote as two commits, the first holding only the
// test, which the gate saw fail before the fix, and the token is handed to
// git in its environment only, never in its arguments.
func TestTheGateCommitsTheTestFirstRedThenTheFixGreenAndPushesTheBranch(t *testing.T) {
	w, r, origin := gitWorld(t)
	ctx := context.Background()
	dir, err := w.Prepare(ctx, "triage/alert-41")
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "code.txt", "fixed\n")
	write(t, dir, "pkg/code_test.go", "package pkg\n")
	changed, err := w.Changed(ctx, dir)
	if err != nil || !slices.Equal(changed, []string{"code.txt", "pkg/code_test.go"}) {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	if err := w.Gate(ctx, dir, []string{"pkg/code_test.go"}, []string{"code.txt"}, "Fix the code"); err != nil {
		t.Fatal(err)
	}
	added, err := w.Added(ctx, dir)
	if err != nil || !strings.Contains(added, "+fixed") || !strings.Contains(added, "Fix the code") || strings.Contains(added, "+broken") {
		t.Fatalf("added %q, err %v", added, err)
	}
	if err := w.Push(ctx, dir, "triage/alert-41"); err != nil {
		t.Fatal(err)
	}
	first := mustGit(t, origin, "show", "--name-only", "--format=", "triage/alert-41~1")
	head := mustGit(t, origin, "show", "--name-only", "--format=", "triage/alert-41")
	if first != "pkg/code_test.go" || head != "code.txt" {
		t.Fatalf("first commit %q, head %q; want the test alone first", first, head)
	}
	for i, argv := range r.argv {
		if strings.Contains(strings.Join(argv, " "), w.token) {
			t.Errorf("the token is in argv %v", argv)
		}
		if slices.Contains(argv, "push") {
			if !slices.ContainsFunc(r.envs[i], func(e string) bool {
				return strings.Contains(e, base64.StdEncoding.EncodeToString([]byte("x-access-token:"+w.token)))
			}) {
				t.Error("the push was not handed the token in its environment")
			}
			if slices.Contains(argv, "--force") || slices.ContainsFunc(argv, func(a string) bool { return strings.Contains(a, "main") }) {
				t.Errorf("push %v", argv)
			}
		} else if slices.ContainsFunc(r.envs[i], func(e string) bool { return strings.Contains(e, "x-access-token") }) {
			t.Errorf("%v was handed the token", argv)
		}
	}
	w.Remove(ctx, dir)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("worktree left: %v", err)
	}
}

// TestATestThatPassesBeforeTheFixIsNotRed: the gate refuses a change whose
// test passes without the fix, and one whose check fails with it.
func TestATestThatPassesBeforeTheFixIsNotRed(t *testing.T) {
	w, _, _ := gitWorld(t)
	ctx := context.Background()
	dir, err := w.Prepare(ctx, "triage/alert-42")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Remove(ctx, dir)
	write(t, dir, "code_test.go", "package x\n")
	write(t, dir, "other.txt", "x\n")
	w.testCmd = []string{"true"}
	if err := w.Gate(ctx, dir, []string{"code_test.go"}, []string{"other.txt"}, "t"); !errors.Is(err, errNotRed) {
		t.Fatalf("err = %v, want errNotRed", err)
	}

	dir2, err := w.Prepare(ctx, "triage/alert-43")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Remove(ctx, dir2)
	write(t, dir2, "code_test.go", "package x\n")
	write(t, dir2, "other.txt", "x\n")
	w.testCmd = []string{"false"}
	if err := w.Gate(ctx, dir2, []string{"code_test.go"}, []string{"other.txt"}, "t"); err == nil || !strings.Contains(err.Error(), "failed at the fix") {
		t.Fatalf("err = %v, want the check's failure", err)
	}
}

// TestPrepareStartsFreshEvenOverAnEarlierWorktree: a worktree or branch left
// by a run that died is replaced, never built on.
func TestPrepareStartsFreshEvenOverAnEarlierWorktree(t *testing.T) {
	w, _, _ := gitWorld(t)
	ctx := context.Background()
	dir, err := w.Prepare(ctx, "triage/alert-44")
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "left.txt", "x\n")
	dir, err = w.Prepare(ctx, "triage/alert-44")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Remove(ctx, dir)
	if changed, _ := w.Changed(ctx, dir); len(changed) != 0 {
		t.Fatalf("a fresh worktree holds %v", changed)
	}
}

func TestParsePorcelainReadsRenamesAndSpaces(t *testing.T) {
	got := parsePorcelain(" M a b.go\x00R  new.go\x00old.go\x00?? t_test.go\x00")
	if !slices.Equal(got, []string{"a b.go", "new.go", "old.go", "t_test.go"}) {
		t.Fatalf("got %q", got)
	}
}

func TestIsTestFileNamesTheRepositorysTests(t *testing.T) {
	for name, want := range map[string]bool{
		"internal/x/a_test.go": true, "adapter/ibkr/tests/test_loop.py": true, "deploy/rehearsal/test-clock.sh": true,
		"internal/x/testdata/a.json": true, "testdata/b": true, "internal/x/a.go": false, "docs/testing.md": false,
	} {
		if isTestFile(name) != want {
			t.Errorf("isTestFile(%q) != %v", name, want)
		}
	}
}
