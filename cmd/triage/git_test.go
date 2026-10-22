package main

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// recordingRunner runs each command for real and records its argv and
// environment.
type recordingRunner struct {
	inner execRunner
	argv  [][]string
	envs  [][]string
}

func (r *recordingRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
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

// script writes an executable shell script.
func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// standInGate is a gate whose sudo and sandbox-exec are stand-ins that run
// the command as this user, unconfined: the command line the job builds is
// the real one, checked by TestTheGateCommandRunsAsItsOwnUserInASandbox;
// the sandbox itself is checked on macOS by TestTheSandboxDeniesTheNetwork.
func standInGate(t *testing.T) *gateBox {
	t.Helper()
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	root := t.TempDir()
	cache := t.TempDir()
	return &gateBox{
		user: me.Username, root: root, path: os.Getenv("PATH"), gocache: cache,
		sudo:        script(t, bin, "sudo", "[ \"$1\" = -n ] && [ \"$2\" = -u ] || exit 98\nshift 3\nexec \"$@\"\n"),
		sandboxExec: script(t, bin, "sandbox-exec", "[ \"$1\" = -p ] || exit 97\nshift 2\nexec \"$@\"\n"),
		run:         execRunner{},
	}
}

// gitWorld is an origin repository holding a small Go module on main, the
// job's own clone of it, and a workspace over them whose gate is the
// stand-in; its check is `go test ./...`, which passes only once Fixed
// returns true.
func gitWorld(t *testing.T) (*gitWorkspace, *recordingRunner, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not installed")
	}
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	origin := filepath.Join(root, "origin.git")
	clone := filepath.Join(root, "clone")
	write(t, seed, "go.mod", "module example.com/x\n\ngo 1.24\n")
	write(t, seed, "internal/x/x.go", "package x\n\n// Fixed reports the defect fixed.\nfunc Fixed() bool { return false }\n")
	mustGit(t, seed, "init", "--quiet", "-b", "main")
	mustGit(t, seed, "add", ".")
	mustGit(t, seed, "commit", "--quiet", "-m", "seed")
	mustGit(t, root, "clone", "--quiet", "--bare", seed, origin)
	mustGit(t, root, "clone", "--quiet", origin, clone)
	r := &recordingRunner{}
	home := t.TempDir()
	w := &gitWorkspace{
		repoDir: clone, workRoot: filepath.Join(root, "work"), run: r,
		env:      gitEnv([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GOCACHE=" + filepath.Join(home, "gocache")}),
		gate:     standInGate(t),
		checkCmd: []string{"go", "test", "./..."}, pushURL: origin, token: "ghp_secrettokensecrettokensecret00",
		fetchVulnDB: func(context.Context, string) error { return nil },
	}
	w.gate.run = r
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

const (
	fixedTest = "package x\n\nimport \"testing\"\n\nfunc TestFixed(t *testing.T) {\n\tif !Fixed() {\n\t\tt.Fatal(\"broken\")\n\t}\n}\n"
	fixedCode = "package x\n\n// Fixed reports the defect fixed.\nfunc Fixed() bool { return true }\n"
)

// TestTheGateCommitsTheTestFirstRedThenTheFixGreenAndPushesTheBranch: the
// change reaches the remote as two commits, the first holding only the
// test, which the gate saw fail by name before the fix, and the token is
// handed to git in its environment only, never in its arguments, and never
// to a gate command.
func TestTheGateCommitsTheTestFirstRedThenTheFixGreenAndPushesTheBranch(t *testing.T) {
	w, r, origin := gitWorld(t)
	ctx := context.Background()
	dir, err := w.Prepare(ctx, "triage/alert-41")
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "internal/x/x.go", fixedCode)
	write(t, dir, "internal/x/x_test.go", fixedTest)
	changed, err := w.Changed(ctx, dir)
	if err != nil || !slices.Equal(changed, []string{"internal/x/x.go", "internal/x/x_test.go"}) {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	if err := w.Gate(ctx, dir, []string{"internal/x/x_test.go"}, []string{"internal/x/x.go"}, "Fix the code"); err != nil {
		t.Fatal(err)
	}
	added, err := w.Added(ctx, dir)
	if err != nil || !strings.Contains(added, "return true") || !strings.Contains(added, "Fix the code") || strings.Contains(added, "return false") {
		t.Fatalf("added %q, err %v", added, err)
	}
	if err := w.Push(ctx, dir, "triage/alert-41"); err != nil {
		t.Fatal(err)
	}
	first := mustGit(t, origin, "show", "--name-only", "--format=", "triage/alert-41~1")
	head := mustGit(t, origin, "show", "--name-only", "--format=", "triage/alert-41")
	if first != "internal/x/x_test.go" || head != "internal/x/x.go" {
		t.Fatalf("first commit %q, head %q; want the test alone first", first, head)
	}
	var red bool
	for i, argv := range r.argv {
		line := strings.Join(argv, " ")
		if strings.Contains(line, w.token) {
			t.Errorf("the token is in argv %v", argv)
		}
		if strings.Contains(line, "-run ^(TestFixed)$") {
			red = true
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
	if !red {
		t.Errorf("the red check did not run the new test by name: %q", r.argv)
	}
	w.Remove(ctx, dir)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("worktree left: %v", err)
	}
	if left, _ := os.ReadDir(filepath.Join(w.gate.root, "runs")); len(left) != 0 {
		t.Errorf("gate runs left behind: %v", left)
	}
}

// TestTheRedCheckMustShowTheNewTestFailingByName: the tests' commit is red
// only when `go test -run` over the new tests reports one of them failing,
// by name: a new test that passes, a test file that does not compile, a
// change adding no test function, or a failure of another test is not red
// (AGENTS.md rule 1: the failing test shows the defect).
func TestTheRedCheckMustShowTheNewTestFailingByName(t *testing.T) {
	for name, c := range map[string]struct {
		test, code string
		want       error
	}{
		"fails by name":   {fixedTest, fixedCode, nil},
		"passes":          {strings.Replace(fixedTest, "!Fixed()", "false", 1), fixedCode, errNotRed},
		"does not build":  {strings.Replace(fixedTest, "!Fixed()", "!NotYetWritten()", 1), fixedCode + "\nfunc NotYetWritten() bool { return true }\n", errNotRed},
		"no new function": {"package x\n\nvar _ = 1\n", fixedCode, errNoNewTest},
	} {
		t.Run(name, func(t *testing.T) {
			w, _, _ := gitWorld(t)
			ctx := context.Background()
			dir, err := w.Prepare(ctx, "triage/alert-42")
			if err != nil {
				t.Fatal(err)
			}
			defer w.Remove(ctx, dir)
			write(t, dir, "internal/x/x_test.go", c.test)
			write(t, dir, "internal/x/x.go", c.code)
			err = w.Gate(ctx, dir, []string{"internal/x/x_test.go"}, []string{"internal/x/x.go"}, "t")
			if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestRedShownNeedsATestFailureOfANewTest(t *testing.T) {
	exitOne := exec.Command("sh", "-c", "exit 1").Run()
	exitTwo := exec.Command("sh", "-c", "exit 2").Run()
	names := []string{"TestFixed"}
	for name, c := range map[string]struct {
		out  string
		err  error
		want bool
	}{
		"its failure":     {"--- FAIL: TestFixed (0.00s)\nFAIL\nFAIL\texample.com/x\t0.1s\n", exitOne, true},
		"a subtest":       {"    --- FAIL: TestFixed/case (0.00s)\n--- FAIL: TestFixed (0.00s)\n", exitOne, true},
		"another test":    {"--- FAIL: TestOther (0.00s)\nFAIL\n", exitOne, false},
		"a build failure": {"--- FAIL: TestFixed (0.00s)\nFAIL\texample.com/y [build failed]\n", exitOne, false},
		"a timeout":       {"panic: test timed out after 10m0s\n\trunning tests:\n\t\tTestFixed (10m0s)\n--- FAIL: TestFixed\n", exitOne, false},
		"passed":          {"ok\texample.com/x\n", nil, false},
		"not a test exit": {"--- FAIL: TestFixed (0.00s)\n", exitTwo, false},
		"no launch":       {"--- FAIL: TestFixed (0.00s)\n", errors.New("exec: not found"), false},
	} {
		if got := redShown(c.out, c.err, names); got != c.want {
			t.Errorf("%s: redShown = %v, want %v", name, got, c.want)
		}
	}
}

// TestTheGateCommandRunsAsItsOwnUserInASandbox: every gate command runs as
// the gate's own macOS user, through sudo with no password, inside
// sandbox-exec with a profile denying the network (loopback aside), writes
// outside its run folder and /tmp, and reads of the job's own folders, and
// with an environment the job builds whole: an empty home, the module cache
// read-only and offline, and no credential.
func TestTheGateCommandRunsAsItsOwnUserInASandbox(t *testing.T) {
	g := &gateBox{user: "ti-triage-gate", root: "/Users/Shared/ti-triage-gate", path: "/opt/homebrew/bin:/usr/bin:/bin",
		sudo: "/usr/bin/sudo", sandboxExec: "/usr/bin/sandbox-exec", denyRead: []string{"/Users/ti-triage"}, golden: "/Users/Shared/golden"}
	name, args := g.command("/Users/Shared/ti-triage-gate/runs/r1", []string{"make", "check"})
	argv := append([]string{name}, args...)
	if !slices.Equal(argv[:6], []string{"/usr/bin/sudo", "-n", "-u", "ti-triage-gate", "/usr/bin/sandbox-exec", "-p"}) {
		t.Fatalf("argv %q", argv)
	}
	profile := argv[6]
	for _, want := range []string{"(deny network*)", `(remote ip "localhost:*")`, "(deny file-write*)",
		`(subpath "/Users/Shared/ti-triage-gate/runs/r1")`, `(deny file-read* (subpath "/Users/ti-triage"))`} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile lacks %s:\n%s", want, profile)
		}
	}
	if !slices.Equal(argv[7:9], []string{"/usr/bin/env", "-i"}) || !slices.Equal(argv[len(argv)-2:], []string{"make", "check"}) {
		t.Fatalf("argv %q", argv)
	}
	env := argv[9 : len(argv)-2]
	for _, want := range []string{"HOME=/Users/Shared/ti-triage-gate/runs/r1/home", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off",
		"GOMODCACHE=/Users/Shared/ti-triage-gate/modcache", "GOVULNDB=file:///Users/Shared/ti-triage-gate/vulndb", "TI_GOLDEN_DIR=/Users/Shared/golden", "PATH=/opt/homebrew/bin:/usr/bin:/bin"} {
		if !slices.Contains(env, want) {
			t.Errorf("env lacks %s: %q", want, env)
		}
	}
	for _, e := range env {
		if strings.Contains(e, "TOKEN") || strings.Contains(e, "SSH_AUTH") || strings.HasPrefix(e, "ANTHROPIC") {
			t.Errorf("env holds %s", e)
		}
	}
	if _, args := g.command(`/tmp/a"b`, []string{"true"}); args != nil {
		t.Errorf("a folder whose name would break the profile was accepted: %q", args)
	}
}

// TestTheSandboxDeniesTheNetwork runs the real sandbox-exec, on macOS, with
// the gate's profile: a command inside it reaches no host and writes only in
// its run folder.
func TestTheSandboxDeniesTheNetwork(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is macOS's")
	}
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("no sandbox-exec")
	}
	g := standInGate(t)
	g.sandboxExec = "/usr/bin/sandbox-exec"
	runDir, done, err := g.newRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	// The package's own folder: outside the run folder and the temporary
	// folders the profile lets a command write in.
	outside, err := filepath.Abs("sandbox-write-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })
	if _, err := g.Run(context.Background(), runDir, []string{"/bin/sh", "-c", "echo x > " + outside}); err == nil {
		t.Error("the sandbox let a command write outside its run folder")
	}
	if _, err := g.Run(context.Background(), runDir, []string{"/bin/sh", "-c", "echo x > \"$HOME/inside\""}); err != nil {
		t.Errorf("the sandbox refused a write in its run folder: %v", err)
	}
	if out, err := g.Run(context.Background(), runDir, []string{"/usr/bin/curl", "-sS", "-m", "5", "-o", "/dev/null", "https://example.com"}); err == nil {
		t.Errorf("the sandbox let a command reach the network: %s", out)
	}
}

// TestAnUnavailableGateRefuses: without the gate's user, its sandbox, or a
// folder only the job can change, the gate is unavailable and nothing is
// run.
func TestAnUnavailableGateRefuses(t *testing.T) {
	ctx := context.Background()
	if err := standInGate(t).Available(ctx); err != nil {
		t.Fatalf("the stand-in gate is unavailable: %v", err)
	}
	noUser := standInGate(t)
	noUser.user = "ti-triage-gate-that-does-not-exist"
	noSandbox := standInGate(t)
	noSandbox.sandboxExec = filepath.Join(t.TempDir(), "absent")
	openRoot := standInGate(t)
	if err := os.Chmod(openRoot.root, 0o777); err != nil {
		t.Fatal(err)
	}
	failing := standInGate(t)
	failing.sudo = script(t, t.TempDir(), "sudo", "exit 1\n")
	for name, g := range map[string]*gateBox{"no user": noUser, "no sandbox": noSandbox, "open root": openRoot, "sudo refuses": failing} {
		if err := g.Available(ctx); err == nil {
			t.Errorf("%s: the gate was available", name)
		}
	}
}

// TestASessionThatRepointsTheWorktreesGitFileIsRefused: the worktree's .git
// file names the job's own repository; a session that changed it (to a
// folder whose configuration would run a program at the job's next git
// command) has its changes refused before git reads them.
func TestASessionThatRepointsTheWorktreesGitFileIsRefused(t *testing.T) {
	w, _, _ := gitWorld(t)
	ctx := context.Background()
	dir, err := w.Prepare(ctx, "triage/alert-47")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Remove(ctx, dir)
	write(t, dir, ".git", "gitdir: "+filepath.Join(dir, "evil")+"\n")
	if _, err := w.Changed(ctx, dir); err == nil || !strings.Contains(err.Error(), ".git") {
		t.Fatalf("err = %v, want the changed .git file named", err)
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

func TestIsTestFileNamesTheRepositorysGoTests(t *testing.T) {
	for name, want := range map[string]bool{
		"internal/x/a_test.go": true, "internal/x/testdata/a.json": true, "cmd/y/testdata/b": true,
		"adapter/ibkr/tests/test_loop.py": false, "deploy/rehearsal/test-clock.sh": false, "testdata/b": false,
		"internal/x/a.go": false, "docs/testing.md": false,
	} {
		if isTestFile(name) != want {
			t.Errorf("isTestFile(%q) != %v", name, want)
		}
	}
}

// TestThePrePushCheckReadsFileNamesAndRefusesBinaries: what is pushed is
// checked whole: every path the commits add, rename or remove is read with
// their messages and added lines, so an account id in a file's name is
// caught; and a binary file, which no line check can read, refuses the push.
func TestThePrePushCheckReadsFileNamesAndRefusesBinaries(t *testing.T) {
	w, _, _ := gitWorld(t)
	ctx := context.Background()
	dir, err := w.Prepare(ctx, "triage/alert-45")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Remove(ctx, dir)
	write(t, dir, "internal/x/x.go", fixedCode)
	write(t, dir, "internal/x/x_test.go", fixedTest)
	write(t, dir, "internal/x/testdata/DU1234567.json", "{}\n")
	if err := w.Gate(ctx, dir, []string{"internal/x/x_test.go", "internal/x/testdata/DU1234567.json"}, []string{"internal/x/x.go"}, "t"); err != nil {
		t.Fatal(err)
	}
	added, err := w.Added(ctx, dir)
	if err != nil || !strings.Contains(added, "DU1234567") {
		t.Fatalf("added %q, err %v: the file's name was not read", added, err)
	}

	dir2, err := w.Prepare(ctx, "triage/alert-46")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Remove(ctx, dir2)
	write(t, dir2, "internal/x/x.go", fixedCode)
	write(t, dir2, "internal/x/x_test.go", fixedTest)
	write(t, dir2, "internal/x/testdata/blob.bin", "U1234567\x00\x01")
	if err := w.Gate(ctx, dir2, []string{"internal/x/x_test.go", "internal/x/testdata/blob.bin"}, []string{"internal/x/x.go"}, "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Added(ctx, dir2); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("err = %v, want a binary file refused", err)
	}
}

// TestAGateCommandsChildrenAreKilledWithIt: a command past its time is
// killed with every process it started, and its output read only to a
// bound, so a child holding the output open never hangs the job.
func TestAGateCommandsChildrenAreKilledWithIt(t *testing.T) {
	r := timedRunner{execRunner{}, 200 * time.Millisecond}
	start := time.Now()
	_, err := r.Run(context.Background(), t.TempDir(), []string{"PATH=" + os.Getenv("PATH")}, "sh", "-c", "sleep 30 & sleep 30")
	if err == nil {
		t.Fatal("no error past the time")
	}
	if time.Since(start) > 15*time.Second {
		t.Fatalf("the command's child held the job for %s", time.Since(start))
	}
}

// TestACommandsOutputIsKeptToItsLastBytes: only the last maxOutput bytes of
// a command's output are kept, so a command that prints without end never
// fills the job's memory, and the end, where go test reports, is kept.
func TestACommandsOutputIsKeptToItsLastBytes(t *testing.T) {
	out, err := execRunner{}.Run(context.Background(), t.TempDir(), []string{"PATH=" + os.Getenv("PATH")},
		"sh", "-c", "yes 0123456789 | head -c 1000000; echo; echo the-end")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > maxOutput || !strings.HasSuffix(out, "the-end\n") {
		t.Fatalf("kept %d bytes ending %q", len(out), out[max(0, len(out)-20):])
	}
}
