package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// dirRecorder records the folder each command is started in.
type dirRecorder struct {
	inner runner
	dirs  []string
	argv  [][]string
}

func (r *dirRecorder) Run(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	r.dirs = append(r.dirs, dir)
	r.argv = append(r.argv, append([]string{name}, args...))
	return r.inner.Run(ctx, dir, env, name, args...)
}

// TestEveryGateCommandStartsWhereTheGateSaysNotWhereTheJobIs: the job never
// enters the gate user's private copy (mode 700, the gate user's own): it
// starts sudo in /, and env, run as the gate user inside the sandbox, enters
// the run folder or its copy (env -C).
func TestEveryGateCommandStartsWhereTheGateSaysNotWhereTheJobIs(t *testing.T) {
	w, _, _ := gitWorld(t)
	r := &dirRecorder{inner: execRunner{}}
	w.gate.run = r
	ctx := context.Background()
	dir, err := w.Prepare(ctx, "triage/alert-50")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Remove(ctx, dir)
	write(t, dir, "internal/x/x.go", fixedCode)
	write(t, dir, "internal/x/x_test.go", fixedTest)
	if err := w.Gate(ctx, dir, []string{"internal/x/x_test.go"}, []string{"internal/x/x.go"}, "t"); err != nil {
		t.Fatal(err)
	}
	var inTree int
	for i, argv := range r.argv {
		if r.dirs[i] != "/" {
			t.Errorf("%q was started in %s by the job, not in / with the folder set by the gate side", argv, r.dirs[i])
		}
		at := slices.Index(argv, "-C")
		if at < 1 || argv[at-1] != "-i" || argv[at-2] != "/usr/bin/env" || !strings.HasPrefix(argv[at+1], filepath.Join(w.gate.root, "runs")) && !strings.Contains(argv[at+1], "/runs/") {
			t.Errorf("%q does not enter its folder through env -C", argv)
			continue
		}
		if filepath.Base(argv[at+1]) == "tree" {
			inTree++
		}
	}
	if inTree != 2 {
		t.Errorf("%d commands ran in the copy, want the red and the green check: %q", inTree, r.argv)
	}
}

// TestEndingTheGateUsersProcessesFailingMakesTheGateUnavailable: a command
// after which the gate user's processes could not be ended is never a
// success: the gate is unavailable for that run, and before any session.
func TestEndingTheGateUsersProcessesFailingMakesTheGateUnavailable(t *testing.T) {
	g := standInGate(t)
	g.endGateProcesses = func(context.Context) error { return errors.New("the sudo rule does not allow it") }
	ctx := context.Background()
	runDir, done, err := g.newRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if _, err := g.Run(ctx, runDir, "", []string{"/usr/bin/true"}); !errors.Is(err, errGateUnavailable) {
		t.Fatalf("err = %v, want gate unavailable", err)
	}
	if err := g.Available(ctx); !errors.Is(err, errGateUnavailable) {
		t.Fatalf("Available = %v, want gate unavailable", err)
	}
}

// TestAGateThatCannotStartTheCheckIsUnavailableNotTheChanges: sudo,
// sandbox-exec or env failing to start the check (here env cannot find go
// on the gate's PATH, after the copy was made) is the gate's failure, never
// the change's: "gate unavailable", not "not red".
func TestAGateThatCannotStartTheCheckIsUnavailableNotTheChanges(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	w, _, _ := gitWorld(t)
	ctx := context.Background()
	dir, err := w.Prepare(ctx, "triage/alert-51")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Remove(ctx, dir)
	write(t, dir, "internal/x/x.go", fixedCode)
	write(t, dir, "internal/x/x_test.go", fixedTest)
	onlyGit := t.TempDir()
	if err := os.Symlink(gitPath, filepath.Join(onlyGit, "git")); err != nil {
		t.Fatal(err)
	}
	w.gate.path = onlyGit + ":/usr/bin:/bin"
	if _, err := exec.LookPath("/usr/bin/go"); err == nil {
		t.Skip("go is in /usr/bin, so the gate's PATH cannot leave it out")
	}
	err = w.Gate(ctx, dir, []string{"internal/x/x_test.go"}, []string{"internal/x/x.go"}, "t")
	if !errors.Is(err, errGateUnavailable) || errors.Is(err, errNotRed) {
		t.Fatalf("err = %v, want gate unavailable", err)
	}
	sandboxFails := standInGate(t)
	sandboxFails.sandboxExec = script(t, t.TempDir(), "sandbox-exec", refuseSignals+"echo 'sandbox-exec: profile invalid' >&2\nexit 65\n")
	runDir, done, err := sandboxFails.newRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if _, err := sandboxFails.Run(ctx, runDir, "", []string{"/usr/bin/true"}); !errors.Is(err, errGateUnavailable) {
		t.Fatalf("a sandbox-exec that failed: err = %v, want gate unavailable", err)
	}
	// A command that started and failed is its own failure.
	runDir2, done2, err := standInGate(t).newRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer done2()
	g := standInGate(t)
	g.root = filepath.Dir(filepath.Dir(runDir2))
	var exit *exec.ExitError
	if _, err := g.Run(ctx, runDir2, "", []string{"/bin/sh", "-c", "exit 3"}); errors.Is(err, errGateUnavailable) || !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("a command that ran and failed: err = %v, want its own exit 3", err)
	}
}

// TestTheGateProfileReachesOnlyItsOwnSockets: the gate's commands may bind
// and reach Unix sockets only in their run folder and their own socket
// folder (TI_TEST_SOCKET_DIR, short, for the tests that need a short path),
// never another under /private/tmp, where host services keep theirs.
func TestTheGateProfileReachesOnlyItsOwnSockets(t *testing.T) {
	g := &gateBox{user: "ti-triage-gate", root: "/Users/Shared/ti-triage-gate", path: "/usr/bin:/bin",
		sudo: "/usr/bin/sudo", sandboxExec: "/usr/bin/sandbox-exec", sockets: "/private/tmp"}
	runDir := "/Users/Shared/ti-triage-gate/runs/r1"
	name, args := g.command(runDir, "", "/x/launched", []string{"make", "check"}, nil)
	argv := append([]string{name}, args...)
	profile := argv[6]
	socketDir := g.socketDir(runDir)
	if socketDir != "/private/tmp/tig-r1" {
		t.Fatalf("socket folder %q", socketDir)
	}
	for _, line := range strings.Split(profile, "\n") {
		if strings.Contains(line, "unix-socket") && strings.Contains(line, `(subpath "/private/tmp")`) {
			t.Errorf("the profile lets the gate reach every socket under /private/tmp: %s", line)
		}
	}
	for _, want := range []string{`(remote unix-socket (subpath "` + socketDir + `"))`, `(local unix-socket (subpath "` + socketDir + `"))`, `(remote unix-socket (subpath "` + runDir + `"))`} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile lacks %s:\n%s", want, profile)
		}
	}
	if !slices.Contains(argv, "TI_TEST_SOCKET_DIR="+socketDir) {
		t.Errorf("the command is not told its socket folder: %q", argv)
	}
}

// TestThePortsInUseAreReadJustBeforeEveryCommand: the listening ports are
// read again for each gate command, immediately before it starts, so a
// program that began listening during an earlier command is denied to the
// next.
func TestThePortsInUseAreReadJustBeforeEveryCommand(t *testing.T) {
	g := standInGate(t)
	var reads int
	g.listening = func(context.Context) ([]int, error) { reads++; return []int{40000 + reads}, nil }
	r := &dirRecorder{inner: execRunner{}}
	g.run = r
	ctx := context.Background()
	runDir, done, err := g.newRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	for range 2 {
		if _, err := g.Run(ctx, runDir, "", []string{"/usr/bin/true"}); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 2 || !strings.Contains(r.argv[0][6], "localhost:40001") || !strings.Contains(r.argv[1][6], "localhost:40002") {
		t.Fatalf("%d reads; want one per command, each in its own profile", reads)
	}
}

// pkillStandIns are a sudo that records its arguments and runs nothing,
// answering as sudo would (status: listing then pkill, by line), and a ps
// that prints out.
func pkillStandIns(t *testing.T, listing, pkill int, ps string) (sudo, psPath, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "record")
	// This stand-in never execs: it only writes its arguments down.
	sudo = script(t, dir, "sudo", "printf '%s\\n' \"$*\" >> '"+record+"'\n"+
		"case \" $* \" in *\" -l \"*) exit "+strconv.Itoa(listing)+" ;; esac\nexit "+strconv.Itoa(pkill)+"\n")
	psPath = script(t, dir, "ps", "printf '%s' '"+ps+"'\n[ -n '"+ps+"' ] || exit 1\n")
	return sudo, psPath, record
}

// TestPkillGateUserEndsOnlyTheGateUsersOwn: the gate's end of its user's
// processes, with stand-ins that run nothing, asks sudo whether its rule
// allows exactly pkill -KILL -U <the gate user's UID> before running it, so
// a missing rule is told apart from pkill finding nothing (both exit 1);
// it then fails while ps still shows a live process of the gate user's,
// or when ps cannot tell.
func TestPkillGateUserEndsOnlyTheGateUsersOwn(t *testing.T) {
	u, err := user.Lookup("daemon")
	if err != nil || u.Uid == "0" || u.Uid == strconv.Itoa(os.Getuid()) {
		t.Skip("no daemon user other than root and this test's own")
	}
	ctx := context.Background()
	sudo, ps, record := pkillStandIns(t, 0, 1, "")
	if err := pkillGateUser(sudo, "daemon", ps)(ctx); err != nil {
		t.Fatalf("pkill finding nothing: %v", err)
	}
	raw, _ := os.ReadFile(record)
	want := "-n -u daemon -l /usr/bin/pkill -KILL -U " + u.Uid + "\n-n -u daemon /usr/bin/pkill -KILL -U " + u.Uid + "\n"
	if string(raw) != want {
		t.Fatalf("sudo was asked %q, want %q", raw, want)
	}
	noRule, ps2, record2 := pkillStandIns(t, 1, 1, "")
	if err := pkillGateUser(noRule, "daemon", ps2)(ctx); err == nil {
		t.Fatal("a sudo rule that does not allow pkill was taken for pkill finding nothing")
	}
	if raw, _ := os.ReadFile(record2); strings.Count(string(raw), "\n") != 1 {
		t.Fatalf("pkill was run without the rule allowing it: %q", raw)
	}
	failing, ps3, _ := pkillStandIns(t, 0, 2, "")
	if err := pkillGateUser(failing, "daemon", ps3)(ctx); err == nil {
		t.Fatal("pkill failing was taken for success")
	}
	left, ps4, _ := pkillStandIns(t, 0, 0, "S\n")
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err := pkillGateUser(left, "daemon", ps4)(short); err == nil {
		t.Fatal("a gate process still running was taken for every one ended")
	}
	zombie, ps5, _ := pkillStandIns(t, 0, 0, "Z\n")
	if err := pkillGateUser(zombie, "daemon", ps5)(ctx); err != nil {
		t.Fatalf("an ended process not yet reaped was taken for one running: %v", err)
	}
	for _, refused := range []string{"root", "ti-triage-gate-that-does-not-exist"} {
		if err := pkillGateUser(sudo, refused, ps)(ctx); err == nil {
			t.Errorf("%s was not refused", refused)
		}
	}
	// A ps that cannot list the gate user's processes (it fails, is killed
	// or is absent) leaves their state unknown: the gate is unavailable,
	// never "every process ended". A process ps still lists after pkill
	// (forked while pkill read the table) is signalled once more, through
	// the same line, and checked again.
	sudo6, _, _ := pkillStandIns(t, 0, 0, "")
	dir := t.TempDir()
	for name, body := range map[string]string{
		"fails":     "exit 2\n",
		"is killed": "exit 137\n",
		"is absent": "",
	} {
		ps := filepath.Join(dir, "ps-missing")
		if body != "" {
			ps = script(t, dir, "ps-"+strings.ReplaceAll(name, " ", "-"), body)
		}
		if err := pkillGateUser(sudo6, "daemon", ps)(ctx); err == nil {
			t.Errorf("a ps that %s was taken for no process left", name)
		}
	}
	// A survivor the first time, none the second.
	count := filepath.Join(dir, "count")
	once := script(t, dir, "ps-once", "if [ -e '"+count+"' ]; then exit 1; fi\n: > '"+count+"'\nprintf 'S\\n'\n")
	survivor, _, record6 := pkillStandIns(t, 0, 0, "")
	if err := pkillGateUser(survivor, "daemon", once)(ctx); err != nil {
		t.Fatalf("a survivor ended by the second pkill: %v", err)
	}
	raw, _ = os.ReadFile(record6)
	line := "-n -u daemon /usr/bin/pkill -KILL -U " + u.Uid + "\n"
	if want := "-n -u daemon -l /usr/bin/pkill -KILL -U " + u.Uid + "\n" + line + line; string(raw) != want {
		t.Fatalf("sudo was asked %q, want the rule, pkill, and pkill once more %q", raw, want)
	}
}
