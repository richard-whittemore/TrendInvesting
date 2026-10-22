package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The gate runs code the session wrote: the red check's go test and the
// green check's make check. It holds no credential the job holds
// (docs/manual/alerts-and-dashboard.md, "What the session and the gate can
// and cannot do"): each command runs as the gate's own macOS user (never the
// job's, never root), through sudo with a fixed rule
// (docs/runbooks/mac-mini.md, "2. The alert triage job"), inside
// sandbox-exec with a profile that denies the network (loopback aside, but
// not any port another program listens on when the command starts, nor any
// Unix socket but its run's own), every write outside its run folder and
// the shared temporary folders, and every read under /Users but the
// folders it needs, with an environment the job builds whole: an empty
// home, the module cache read-only and offline, Go's vulnerability
// database from a file, and no token of any kind. Each command runs over a
// fresh copy of one commit (a check's) or of the worktree as the session
// left it (a session's command, serveTools), which the gate user itself
// unpacks and makes a repository, so git, run as the gate user, owns what
// it reads; the gate user never touches the job's clone or worktree, and
// the job never enters a copy or runs anything from it.

// gateBox is the gate's user, sandbox and folder.
type gateBox struct {
	// user is the gate's macOS user; root is the gate's folder, which only
	// the job can change (Available), holding its runs, the module cache and
	// the vulnerability database.
	user, root string
	// sudo and sandboxExec are the programs (tests: stand-ins).
	sudo, sandboxExec string
	// path is the commands' PATH; golden their TI_GOLDEN_DIR, if any;
	// toolchain the Go installation's folder, which the profile lets them
	// read wherever it is.
	path, golden, toolchain string
	// denyRead are the folders the profile denies reading again after the
	// folders it allows: the job's home and its token folders.
	denyRead []string
	// gocache, when set, is the build cache of every run (tests only; a run
	// otherwise builds in its own, so no run's code can feed another's).
	gocache string
	run     runner
	// sameUser lets the gate's user be the job's own (tests only: the
	// stand-in gate runs every command as the test's user).
	sameUser bool
	// listening is every port a program on this Mac listens on, which the
	// profile denies a command, read just before each command
	// (netstatListening; tests: nil, none, or a stand-in).
	listening func(ctx context.Context) ([]int, error)
	// endGateProcesses ends every process the gate user still runs, and
	// fails unless none is left (pkillGateUser; tests: a fake). Without it,
	// or when it fails, the gate is unavailable: no command runs that could
	// not be ended, and none succeeds with a process left behind.
	endGateProcesses func(ctx context.Context) error
	// stepTimeout is the time limit of each gate-side step (0: gateStepTimeout).
	stepTimeout time.Duration
	// sockets is the folder each run's own socket folder is made in: the
	// real path of /tmp ("" is it), where a socket's path is short enough.
	sockets string
}

func (g *gateBox) modCache() string { return filepath.Join(g.root, "modcache") }
func (g *gateBox) vulnDB() string   { return filepath.Join(g.root, "vulndb") }
func (g *gateBox) runs() string     { return filepath.Join(g.root, "runs") }

// socketDir is runDir's own socket folder, the only place outside runDir
// its commands may bind or reach a Unix socket: short (tig- and the run's
// name, under /tmp), for the tests whose socket paths must fit the
// platform's bound, and made by the gate user at its run's first command.
func (g *gateBox) socketDir(runDir string) string {
	root := g.sockets
	if root == "" {
		root = "/tmp"
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
	}
	return filepath.Join(root, "tig-"+filepath.Base(runDir))
}

// errGateUnavailable is a gate that cannot run: its user, its sandbox or its
// folder is missing or wrong, or a check could not be started in it.
var errGateUnavailable = errors.New("gate unavailable")

// gateStepTimeout is the time limit of each gate-side step (the object
// reads and the copy, the ports in use): safely under Claude Code's default
// two-minute command timeout, so a stalled step ends as the gate's failure
// before the wrapper gives up on the session's command. A step inside
// another (the ports read of the copy) ends no later than the enclosing
// step. The sudo that starts the session's own command is part of that
// command's own budget, not a step of its own.
const gateStepTimeout = time.Minute

// errGateStalled is a gate-side step that ran past its own time limit.
var errGateStalled = errors.New("a gate step ran past its time limit")

// errGateStep is the failure of a gate-side step: the object reads, the
// copy, the ports in use, the launcher (sudo, sandbox-exec, env), the end of
// the gate user's processes. It is the gate's outage whatever the session's
// context did meanwhile; only a cancel while the session's own command runs
// is an interruption.
type errGateStep struct{ err error }

func (e *errGateStep) Error() string { return e.err.Error() }
func (e *errGateStep) Unwrap() error { return e.err }

// gateStep marks err as a gate-side step's failure (errGateUnavailable).
func gateStep(err error) error {
	var step *errGateStep
	if errors.As(err, &step) {
		return err
	}
	if !errors.Is(err, errGateUnavailable) {
		err = fmt.Errorf("%w: %w", errGateUnavailable, err)
	}
	return &errGateStep{err: err}
}

// stepFailure is err of the gate-side step whose context is step, marked
// as a stall when the step ran past its limit.
func stepFailure(step context.Context, err error) error {
	if errors.Is(step.Err(), context.DeadlineExceeded) && !errors.Is(err, errGateStalled) {
		err = fmt.Errorf("%w: %w", err, errGateStalled)
	}
	return gateStep(err)
}

// stepDeadline is the context key of the enclosing gate-side step's end.
type stepDeadline struct{}

// stepContext is the context of one gate-side step: the session's values,
// never its cancellation or deadline, with the step's own time limit, cut
// to the end of any enclosing step.
func (g *gateBox) stepContext(ctx context.Context) (context.Context, context.CancelFunc) {
	d := g.stepTimeout
	if d <= 0 {
		d = gateStepTimeout
	}
	end := time.Now().Add(d)
	if outer, ok := ctx.Value(stepDeadline{}).(time.Time); ok && outer.Before(end) {
		end = outer
	}
	return context.WithDeadline(context.WithValue(context.WithoutCancel(ctx), stepDeadline{}, end), end)
}

// errProcessesLeft is a gate command after which the gate user's processes
// could not all be ended: a failure of the gate whatever cancelled the command.
var errProcessesLeft = errors.New("the gate user's processes could not all be ended")

// Available checks the gate before any session: the gate user exists and is
// neither the job's own nor root, its processes can be ended, sandbox-exec
// is there, the gate's folder is the job's and no one else can write to it,
// and a probe runs through sudo and the sandbox.
func (g *gateBox) Available(ctx context.Context) error {
	u, err := user.Lookup(g.user)
	if err != nil {
		return fmt.Errorf("%w: no macOS user %s", errGateUnavailable, g.user)
	}
	if u.Uid == "0" || u.Uid == strconv.Itoa(os.Getuid()) && !g.sameUser {
		return fmt.Errorf("%w: the gate's user %s must be a user of its own, not this job's user and not root", errGateUnavailable, g.user)
	}
	if g.endGateProcesses == nil {
		return fmt.Errorf("%w: the gate user's processes cannot be ended", errGateUnavailable)
	}
	if info, err := os.Stat(g.sandboxExec); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%w: no %s", errGateUnavailable, g.sandboxExec)
	}
	info, err := os.Stat(g.root)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: no gate folder (-gate-dir)", errGateUnavailable)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: the gate folder must be this user's and writable by no one else (mode 750)", errGateUnavailable)
	}
	runDir, done, err := g.newRun(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", errGateUnavailable, err)
	}
	defer done()
	if _, err := g.Run(ctx, runDir, "", []string{"/usr/bin/true"}); err != nil {
		return fmt.Errorf("%w: the probe through sudo and sandbox-exec, and the end of the gate user's processes after it, failed", errGateUnavailable)
	}
	return nil
}

// runsFolder makes the runs folder: the gate user's to write in, never to
// rename or remove what the job made there (the sticky bit).
func (g *gateBox) runsFolder() error {
	//nolint:gosec // G301/G302: the gate user, in the gate's group, must write here; no one else can reach the gate's folder (mode 750).
	if err := os.MkdirAll(g.runs(), 0o770); err != nil {
		return errors.New("the runs folder could not be made")
	}
	if err := os.Chmod(g.runs(), 0o770|os.ModeSticky); err != nil {
		return errors.New("the runs folder's mode could not be set")
	}
	return nil
}

// newRun makes a fresh run folder the gate user can write in: home (also
// HOME, TMPDIR and the build cache), where the copy of a commit (tree) is
// made. done removes it, as the gate user, whose files the job cannot
// remove.
func (g *gateBox) newRun(ctx context.Context) (runDir string, done func(), err error) {
	var id [4]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", nil, errors.New("no randomness for the run folder")
	}
	if err := g.runsFolder(); err != nil {
		return "", nil, err
	}
	// The sandbox matches real paths, so the run folder is named by its own.
	runs, err := filepath.EvalSymlinks(g.runs())
	if err != nil {
		return "", nil, errors.New("the runs folder could not be read")
	}
	runDir = filepath.Join(runs, hex.EncodeToString(id[:]))
	for _, d := range []string{runDir, filepath.Join(runDir, "home"), filepath.Join(runDir, "home", "tmp")} {
		//nolint:gosec // G301/G302: the gate user, in the gate's group, must write here; no one else can reach the gate's folder (mode 750).
		if err := os.Mkdir(d, 0o770); err != nil {
			return "", nil, errors.New("the run folder could not be made")
		}
		mode := os.FileMode(0o770)
		if d == runDir {
			// Sticky: the gate user never renames or removes what the job
			// makes in it (exportTree's folders).
			mode |= os.ModeSticky
		}
		//nolint:gosec // G301/G302: the gate user, in the gate's group, must write here; no one else can reach the gate's folder (mode 750).
		if err := os.Chmod(d, mode); err != nil {
			return "", nil, errors.New("the run folder could not be made")
		}
	}
	return runDir, func() { g.removeRun(ctx, runDir) }, nil
}

// removeRun empties a run folder and removes its socket folder as the gate
// user, then removes what is left (the job's own folders) as the job.
func (g *gateBox) removeRun(ctx context.Context, runDir string) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	_, _ = g.Run(cleanup, runDir, "", []string{"/bin/sh", "-c", `chmod -R u+rwX "$0" "$1" 2>/dev/null; rm -rf "$0"/* "$0"/.[!.]* "$1"`, runDir, g.socketDir(runDir)})
	_ = os.RemoveAll(runDir)
}

// staleRun is how old a run folder is before a sweep takes it as one a
// killed job left: no gate command runs for a day.
const staleRun = 24 * time.Hour

// Sweep removes the run folders older than staleRun, which a job killed
// during a check left behind, through the same sudo rule as every command.
func (g *gateBox) Sweep(ctx context.Context) error {
	entries, err := os.ReadDir(g.runs())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("the gate's runs folder could not be read")
	}
	runs, err := filepath.EvalSymlinks(g.runs())
	if err != nil {
		return errors.New("the gate's runs folder could not be read")
	}
	var failed []string
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !e.IsDir() || time.Since(info.ModTime()) < staleRun {
			continue
		}
		dir := filepath.Join(runs, e.Name())
		g.removeRun(ctx, dir)
		if _, err := os.Stat(dir); err == nil {
			failed = append(failed, e.Name())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("the gate's stale runs %s could not be removed", strings.Join(failed, ", "))
	}
	return nil
}

// Run runs argv as the gate user in the sandbox, in runDir's folder wd
// ("" for runDir itself), and afterwards ends every process the gate user
// still runs. The job starts sudo in /, never in a folder of the gate
// user's (its copy is the gate user's alone, mode 700): env, as the gate
// user inside the sandbox, enters the folder. The ports in use are read
// just before the command starts. A command the gate could not start
// (sudo, sandbox-exec, env or the program missing or refusing: the
// launcher never ran it) and one after which the gate user's processes
// could not all be ended are the gate's failure (errGateStep,
// errGateUnavailable), never the command's, whatever ctx did meanwhile.
func (g *gateBox) Run(ctx context.Context, runDir, wd string, argv []string) (string, error) {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", gateStep(errors.New("no randomness for the command's launch mark"))
	}
	marker := filepath.Join(runDir, "launched-"+hex.EncodeToString(id[:]))
	var ports []int
	if g.listening != nil {
		step, cancel := g.stepContext(ctx)
		var err error
		ports, err = g.listening(step)
		if err != nil {
			err = stepFailure(step, errors.New("the ports in use could not be read"))
		}
		cancel()
		if err != nil {
			return "", err
		}
	}
	name, args := g.command(runDir, wd, marker, argv, ports)
	if args == nil {
		return "", gateStep(errors.New("the run folder's name cannot be put in the sandbox's profile"))
	}
	if err := ctx.Err(); err != nil {
		return "", err // given up on before it started: nothing of the gate's failed
	}
	out, err := withStop(g.run, func() { _ = g.stopAll() }).Run(ctx, "/", nil, name, args...)
	_, unlaunched := os.Stat(marker)
	_ = os.Remove(marker)
	if serr := g.stopAll(); serr != nil {
		return out, gateStep(fmt.Errorf("%w (%w)", errProcessesLeft, serr))
	}
	var unstarted *errNotStarted
	if errors.As(err, &unstarted) && ctx.Err() != nil {
		// The launcher process was never started and the session was
		// already stopped: an interruption, nothing of the gate's failed.
		return out, ctx.Err()
	}
	if unlaunched != nil {
		return out, gateStep(fmt.Errorf("sudo, sandbox-exec or env could not start %s", filepath.Base(argv[0])))
	}
	return out, err
}

// launcher is the last step of every gate command, run by the gate user
// inside the sandbox ($0 the command's launch mark, then the command): it
// makes the run's socket folder its own, then marks the command launched
// and replaces itself with it. A command line that never reached it
// (sudo, sandbox-exec or env failing), or whose program is not on the
// gate's PATH, leaves no mark; once the mark is made, the exit status is
// the command's own.
const launcher = `command -v "$1" >/dev/null 2>&1 || { printf 'gate: %s cannot be started\n' "$1" >&2; exit 127; }
if ! /bin/mkdir -m 700 "$TI_TEST_SOCKET_DIR" 2>/dev/null; then
  [ -d "$TI_TEST_SOCKET_DIR" ] && [ -O "$TI_TEST_SOCKET_DIR" ] || { echo "gate: the run's socket folder belongs to another user" >&2; exit 127; }
fi
: > "$0" || exit 127
exec "$@"
`

// withStop is r with stop run when a command is killed, through a time
// bound; any other runner (a test's) is r unchanged.
func withStop(r runner, stop func()) runner {
	switch t := r.(type) {
	case execRunner:
		t.stop = stop
		return t
	case timedRunner:
		t.inner = withStop(t.inner, stop)
		return t
	}
	return r
}

// stopAll ends every process of the gate user, which only ever runs the
// gate, so a background process a test started never outlives its check;
// it fails unless every one is ended. It goes through endGateProcesses
// only, which refuses any target but the gate user's own processes.
func (g *gateBox) stopAll() error {
	if g.endGateProcesses == nil {
		return errors.New("the gate user's processes cannot be ended")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return g.endGateProcesses(ctx)
}

// pkillGateUser is the gate's end of the gate user's processes, and the
// only code of the job that names a program sending signals
// (docs/development.md, "Ending processes"; TestNoKillOutsideTheGuardedFunctions):
// fail closed, it resolves the gate user's UID and refuses unless that UID
// exists, is not this process's own and is not root; asks sudo whether its
// fixed rule allows exactly pkill -KILL -U <that UID> (sudo -l, which runs
// nothing), so a missing rule is never taken for pkill finding no process
// (both exit 1); runs it; and then fails while ps still shows a process of
// that UID's that is not a zombie.
func pkillGateUser(sudo, gateUser, ps string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		//nolint:gosec // G204: the operator's -gate-user, read by id.
		out, err := exec.CommandContext(ctx, "/usr/bin/id", "-u", gateUser).Output()
		if err != nil {
			return fmt.Errorf("the gate user %s has no UID, so no process is ended", gateUser)
		}
		uid, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil || uid <= 0 || uid == os.Getuid() {
			return fmt.Errorf("the gate user's UID %q is root, this job's own or not a UID, so no process is ended", strings.TrimSpace(string(out)))
		}
		pkill := []string{"/usr/bin/pkill", "-KILL", "-U", strconv.Itoa(uid)}
		//nolint:gosec // G204: the fixed sudo rule, the gate user and its own UID.
		if err := exec.CommandContext(ctx, sudo, append([]string{"-n", "-u", gateUser, "-l"}, pkill...)...).Run(); err != nil {
			return errors.New("the sudo rule does not let this job end the gate user's processes (pkill -KILL -U <its UID>)")
		}
		signal := func() error {
			//nolint:gosec // G204: the fixed sudo rule, the gate user and its own UID.
			err := exec.CommandContext(ctx, sudo, append([]string{"-n", "-u", gateUser}, pkill...)...).Run()
			var exit *exec.ExitError
			if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) { // 1: pkill found no process
				return errors.New("pkill of the gate user's processes failed")
			}
			return nil
		}
		if err := signal(); err != nil {
			return err
		}
		for resignalled := false; ; {
			left, err := gateProcessesLeft(ctx, ps, uid)
			if err != nil {
				return err
			}
			if !left {
				return nil
			}
			// One forked while pkill read the process table: signalled once
			// more, through the same line.
			if !resignalled {
				resignalled = true
				if err := signal(); err != nil {
					return err
				}
				continue
			}
			select {
			case <-ctx.Done():
				return errors.New("the gate user still runs processes after pkill")
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}

// gateProcessesLeft reports whether ps lists a process of uid's that is not
// a zombie. Only ps's own "none matched" (exit 1, nothing printed on stdout or stderr) is no
// process left; a ps that could not run, failed otherwise or ran out of
// time leaves the state unknown, which is an error (docs/development.md,
// "Ending processes": fail closed).
func gateProcessesLeft(ctx context.Context, ps string, uid int) (bool, error) {
	//nolint:gosec // G204: the ps program and the gate user's own UID.
	listed, err := exec.CommandContext(ctx, ps, "-U", strconv.Itoa(uid), "-o", "stat=").Output()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return false, errors.New("the gate user's processes could not be listed in time")
	case err == nil:
		return runningIn(string(listed)), nil
	case errors.As(err, &exit) && exit.ExitCode() == 1 && strings.TrimSpace(string(listed)) == "" && strings.TrimSpace(string(exit.Stderr)) == "":
		return false, nil
	}
	return false, errors.New("the gate user's processes could not be listed")
}

// runningIn reports whether ps's stat column names a process that is not a
// zombie (ended, waiting for its parent to read it).
func runningIn(stats string) bool {
	for _, stat := range strings.Fields(stats) {
		if !strings.HasPrefix(stat, "Z") {
			return true
		}
	}
	return false
}

// ibkrPorts are the API ports of IB Gateway and TWS (live and paper), which
// the profile denies even when nothing listens on them yet: the gate's code
// must never reach the broker (docs/live-trading/plan.md section 7).
var ibkrPorts = []int{4001, 4002, 7496, 7497}

// netstatListening is every TCP port in LISTEN and every bound UDP port on
// this Mac, read from the netstat program at path. A listing that shows no Internet socket at
// all is one the system kept from this process (macOS can), never taken
// as "nothing listens": the gate is then unavailable.
func netstatListening(path string) func(ctx context.Context) ([]int, error) {
	return func(ctx context.Context) ([]int, error) {
		// One listing of every protocol: netstat's -p tcp prints nothing when
		// run from a Go program on macOS.
		out, err := exec.CommandContext(ctx, path, "-an").Output()
		if err != nil {
			return nil, errors.New("netstat failed")
		}
		if !regexp.MustCompile(`(?m)^(tcp|udp)[46]`).Match(out) {
			return nil, errors.New("netstat showed no Internet socket, so the ports in use cannot be told")
		}
		return listeningPorts(string(out)), nil
	}
}

// listeningPorts reads netstat -an: a tcp line in LISTEN, or any udp line,
// names its local address's port after the address's last dot.
func listeningPorts(out string) []int {
	var ports []int
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		if listens := strings.HasPrefix(f[0], "tcp") && slices.Contains(f, "LISTEN") || strings.HasPrefix(f[0], "udp"); !listens {
			continue
		}
		i := strings.LastIndex(f[3], ".")
		if i < 0 {
			continue
		}
		if p, err := strconv.Atoi(f[3][i+1:]); err == nil && p > 0 && !slices.Contains(ports, p) {
			ports = append(ports, p)
		}
	}
	slices.Sort(ports)
	return ports
}

// readable are the folders a command may read under /Users: its run folder,
// the module cache, the vulnerability database, the golden folder and the
// Go toolchain, when set.
func (g *gateBox) readable(runDir string) []string {
	out := []string{runDir, g.modCache(), g.vulnDB()}
	for _, d := range []string{g.golden, g.toolchain} {
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// command is the gate's command line for argv in runDir's folder wd: sudo
// to the gate user, sandbox-exec with the profile, env with the whole
// environment entering the folder (-C), then the launcher with marker and
// argv. A folder whose name would break the profile gives no command.
func (g *gateBox) command(runDir, wd, marker string, argv []string, ports []int) (name string, args []string) {
	sockets := g.socketDir(runDir)
	profile, ok := sandboxProfile(runDir, sockets, g.readable(runDir), g.denyRead, ports)
	if !ok {
		return "", nil
	}
	home := filepath.Join(runDir, "home")
	gocache := filepath.Join(home, "gocache")
	if g.gocache != "" {
		gocache = g.gocache
	}
	env := []string{
		"HOME=" + home, "TMPDIR=" + filepath.Join(home, "tmp"), "PATH=" + g.path, "LANG=en_US.UTF-8",
		"GOFLAGS=-mod=readonly", "GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off",
		"GOMODCACHE=" + g.modCache(), "GOCACHE=" + gocache, "GOVULNDB=file://" + g.vulnDB(),
		"TI_TEST_SOCKET_DIR=" + sockets,
	}
	if g.golden != "" {
		env = append(env, "TI_GOLDEN_DIR="+g.golden)
	}
	args = append([]string{"-n", "-u", g.user, g.sandboxExec, "-p", profile, "/usr/bin/env", "-i", "-C", filepath.Join(runDir, wd)}, env...)
	args = append(args, "/bin/sh", "-c", launcher, marker)
	return g.sudo, append(args, argv...)
}

// sandboxProfile is the gate's sandbox-exec profile: everything a build and
// a test need, except the network beyond loopback (DNS included: the
// resolver's socket is not loopback), any loopback port in ports or
// ibkrPorts (a program already listening there: a test's own servers listen
// on fresh ports), any Unix socket but those in the run folder and its own
// socket folder sockets (a host service's under /private/tmp is out of
// reach), writes outside the run folder and the shared temporary folders,
// and reads under /Users but of readable (and the folders above them, by
// name only), with denyRead denied again. ps is allowed out of the sandbox
// only because the setuid program cannot run in it, and it reaches nothing.
// A path holding a quote, a backslash or a line break is refused.
func sandboxProfile(runDir, sockets string, readable, denyRead []string, ports []int) (string, bool) {
	paths := append(append([]string{runDir, sockets}, readable...), denyRead...)
	for _, p := range paths {
		if strings.ContainsAny(p, "\"\\\n\r") || !filepath.IsAbs(p) {
			return "", false
		}
	}
	quote := func(p string) string { return `"` + p + `"` }
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")
	b.WriteString("(deny network*)\n")
	b.WriteString(`(allow network* (local ip "localhost:*") (remote ip "localhost:*"))` + "\n")
	b.WriteString(`(allow network* (remote unix-socket (subpath ` + quote(runDir) + `)) (remote unix-socket (subpath ` + quote(sockets) + `)))` + "\n")
	b.WriteString(`(allow network-bind (local unix-socket (subpath ` + quote(runDir) + `)) (local unix-socket (subpath ` + quote(sockets) + `)))` + "\n")
	denied := slices.Clone(ibkrPorts)
	for _, p := range ports {
		if !slices.Contains(denied, p) {
			denied = append(denied, p)
		}
	}
	for _, p := range denied {
		fmt.Fprintf(&b, "(deny network-outbound (remote ip \"localhost:%d\"))\n", p)
	}
	b.WriteString("(deny file-write*)\n")
	b.WriteString(`(allow file-write* (subpath ` + quote(runDir) + `) (subpath "/private/tmp") (subpath "/private/var/folders")` +
		` (literal "/dev/null") (literal "/dev/zero") (literal "/dev/dtracehelper") (regex #"^/dev/tty") (regex #"^/dev/fd/"))` + "\n")
	b.WriteString(`(deny file-read* (subpath "/Users"))` + "\n")
	var above []string
	for _, d := range readable {
		b.WriteString(`(allow file-read* (subpath ` + quote(d) + `))` + "\n")
		for p := filepath.Dir(d); p != "/" && p != "."; p = filepath.Dir(p) {
			if !slices.Contains(above, p) {
				above = append(above, p)
			}
		}
	}
	for _, p := range above {
		b.WriteString(`(allow file-read-metadata (literal ` + quote(p) + `))` + "\n")
	}
	for _, d := range denyRead {
		b.WriteString(`(deny file-read* (subpath ` + quote(d) + `))` + "\n")
	}
	b.WriteString(`(allow process-exec (literal "/bin/ps") (with no-sandbox))` + "\n")
	return b.String(), true
}

// copyScript is what the gate user runs in a fresh run folder to make the
// copy of a commit: the archive the job wrote ($0, in a folder of the job's
// the gate user can read but not change) unpacked into tree (bsdtar refuses
// a path that leaves it, or one through a link), then made a git repository
// of one commit (the deployment tests clone their checkout), with no hook,
// no file-system monitor and no configuration but its own. A copy an
// earlier command of the same run made (a session's) is replaced.
const copyScript = `set -eu
umask 077
if [ -e tree ]; then chmod -R u+rwX tree; rm -rf tree; fi
mkdir tree
/usr/bin/tar -xf "$0" -C tree
cd tree
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
safe="-c core.hooksPath=/dev/null -c core.fsmonitor=false"
git $safe init --quiet -b main
git $safe add -A
git $safe -c commit.gpgsign=false -c user.name=TrendInvesting-gate -c user.email=gate@trend-investing.invalid commit --quiet --no-verify -m "the commit under the gate"
`

// exportTree writes rev of the worktree at dir as an archive (the job's
// only step) and has the gate user make the copy from it (copyScript), so
// every file of the copy is the gate user's own. The run folder is the gate
// user's to write in and a session's commands share it, so the job never
// writes to a path there the gate user could have made: each archive goes
// into a folder the job makes afresh (stageFolder), which the gate user can
// read but neither change nor, the run folder being sticky, rename, and is
// created there, never followed through a link (O_EXCL, O_NOFOLLOW).
func (w *gitWorkspace) exportTree(ctx context.Context, dir, rev, runDir string) error {
	stage, err := stageFolder(runDir)
	if err != nil {
		return gateStep(err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	archive := filepath.Join(stage, "commit.tar")
	//nolint:gosec // G302/G304: a new file in the job's fresh folder; the gate user, in the gate's group, reads it.
	f, err := os.OpenFile(archive, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o640)
	if err != nil {
		return gateStep(errors.New("the commit's archive could not be made"))
	}
	werr := w.treeTar(ctx, dir, rev, f)
	if werr == nil {
		werr = f.Chmod(0o640)
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return gateStep(errors.New("the commit's archive could not be written"))
	}
	if _, err := w.gate.Run(ctx, runDir, "", []string{"/bin/sh", "-c", copyScript, archive}); err != nil {
		return gateStep(fmt.Errorf("the gate user could not make the copy of the commit: %w", err))
	}
	return nil
}

// stageFolder makes a fresh folder of the job's in runDir, which the gate's
// group may read and enter but not write in: made by mkdir, which never
// follows or reuses what is there, and checked to be a folder of this
// user's before its mode is set.
func stageFolder(runDir string) (string, error) {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", errors.New("no randomness for the archive's folder")
	}
	stage := filepath.Join(runDir, "in-"+hex.EncodeToString(id[:]))
	if err := os.Mkdir(stage, 0o700); err != nil {
		return "", errors.New("the archive's folder could not be made")
	}
	info, err := os.Lstat(stage)
	st, ok := sysStat(info)
	if err != nil || !info.IsDir() || !ok || int(st.Uid) != os.Getuid() {
		return "", errors.New("the archive's folder is not the job's own")
	}
	//nolint:gosec // G302: the gate user, in the gate's group, reads the archive here; no one else can reach the gate's folder (mode 750).
	if err := os.Chmod(stage, 0o750); err != nil {
		return "", errors.New("the archive's folder could not be opened to the gate user")
	}
	return stage, nil
}

// sysStat is info's system record, if it has one.
func sysStat(info os.FileInfo) (*syscall.Stat_t, bool) {
	if info == nil {
		return nil, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return st, ok
}

// maxLinkTarget bounds a link's target in the archive.
const maxLinkTarget = 4096

// treeTar writes rev's tree, in the repository at dir, to out as a tar
// archive, file by file from git's own objects (ls-tree, then cat-file),
// never through git archive: no attribute the tree holds (export-ignore,
// export-subst) changes what the gate checks, so its copy is exactly the
// tree that is pushed. A link stays a link; a submodule is an empty folder.
func (w *gitWorkspace) treeTar(ctx context.Context, dir, rev string, out io.Writer) error {
	// The same time bound as every other gate command, over the listing, the
	// object reads and the wait; the caller's own cancellation still ends it.
	if d := commandTimeout(w.run); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	listing, err := w.gitOutput(ctx, dir, "ls-tree", "-r", "-z", "--full-tree", rev)
	if err != nil {
		return err
	}
	//nolint:gosec // G204: git's own fixed command, in a folder the job made.
	cmd := exec.CommandContext(ctx, "git", append(slices.Clone(gitSafe), "cat-file", "--batch")...)
	cmd.Dir, cmd.Env = dir, w.env
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	objects, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return errors.New("git cat-file could not be started")
	}
	// A cat-file whose child holds its output open past the kill would block
	// the reads below; the deadline closes both pipes.
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			_ = in.Close()
			_ = objects.Close()
		case <-finished:
		}
	}()
	r := bufio.NewReader(objects)
	werr := writeTreeTar(tar.NewWriter(out), listing, in, r)
	_ = in.Close()
	_, _ = io.Copy(io.Discard, r)
	if err := cmd.Wait(); werr == nil && err != nil {
		werr = errors.New("git cat-file failed")
	}
	return werr
}

// commandTimeout is the time bound of r's commands, or 0 for a runner with none.
func commandTimeout(r runner) time.Duration {
	if t, ok := r.(timedRunner); ok {
		return t.timeout
	}
	return 0
}

// writeTreeTar writes each entry of an ls-tree -r -z listing to tw, asking
// cat-file --batch (its input in, its output r) for each blob in turn.
func writeTreeTar(tw *tar.Writer, listing string, in io.Writer, r *bufio.Reader) error {
	now := time.Now()
	for _, entry := range strings.Split(listing, "\x00") {
		if entry == "" {
			continue
		}
		meta, path, ok := strings.Cut(entry, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 || path == "" {
			return errors.New("git ls-tree's listing was not understood")
		}
		mode, kind, oid := f[0], f[1], f[2]
		if kind == "commit" {
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: path + "/", Mode: 0o755, ModTime: now}); err != nil {
				return err
			}
			continue
		}
		if kind != "blob" {
			return fmt.Errorf("git ls-tree listed a %s in the tree", kind)
		}
		if _, err := fmt.Fprintln(in, oid); err != nil {
			return errors.New("git cat-file could not be asked")
		}
		head, err := r.ReadString('\n')
		hf := strings.Fields(head)
		if err != nil || len(hf) != 3 || hf[0] != oid || hf[1] != "blob" {
			return errors.New("git cat-file's answer was not understood")
		}
		size, err := strconv.ParseInt(hf[2], 10, 64)
		if err != nil || size < 0 {
			return errors.New("git cat-file's answer was not understood")
		}
		h := &tar.Header{Typeflag: tar.TypeReg, Name: path, Mode: 0o644, Size: size, ModTime: now, Format: tar.FormatPAX}
		switch mode {
		case "100644":
		case "100755":
			h.Mode = 0o755
		case "120000":
			if size > maxLinkTarget {
				return errors.New("a link in the tree names a target longer than its bound")
			}
			target := make([]byte, size)
			if _, err := io.ReadFull(r, target); err != nil {
				return errors.New("git cat-file's answer was cut short")
			}
			h = &tar.Header{Typeflag: tar.TypeSymlink, Name: path, Linkname: string(target), Mode: 0o777, ModTime: now, Format: tar.FormatPAX}
		default:
			return fmt.Errorf("git ls-tree listed a file of mode %s", mode)
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := io.CopyN(tw, r, size); err != nil {
				return errors.New("git cat-file's answer was cut short")
			}
		}
		if b, err := r.ReadByte(); err != nil || b != '\n' {
			return errors.New("git cat-file's answer was not understood")
		}
	}
	return tw.Close()
}

// vulnDBURL is Go's vulnerability database whole
// (https://go.dev/doc/security/vuln/database).
const vulnDBURL = "https://vuln.go.dev/vulndb.zip"

// maxVulnDB bounds the database's download and its unpacked size.
const maxVulnDB = 256 << 20

// httpVulnDB fetches the vulnerability database into dir, replacing what is
// there only once the new copy is whole; the gate user may read it, never
// change it.
func httpVulnDB(client *http.Client) func(ctx context.Context, dir string) error {
	return func(ctx context.Context, dir string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, vulnDBURL, http.NoBody)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return errors.New("the vulnerability database could not be reached")
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("the vulnerability database answered %s", resp.Status)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxVulnDB+1))
		if err != nil || len(raw) > maxVulnDB {
			return errors.New("the vulnerability database did not arrive whole")
		}
		next := dir + ".next"
		_ = os.RemoveAll(next)
		if err := unzipTo(raw, next); err != nil {
			_ = os.RemoveAll(next)
			return err
		}
		old := dir + ".old"
		_ = os.RemoveAll(old)
		_ = os.Rename(dir, old)
		if err := os.Rename(next, dir); err != nil {
			return err
		}
		return os.RemoveAll(old)
	}
}

// unzipTo writes the zip's regular files under dest, readable by the
// gate's group and writable by no one but the job.
func unzipTo(raw []byte, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return errors.New("the vulnerability database is not a zip")
	}
	var total uint64
	for _, f := range zr.File {
		if !filepath.IsLocal(f.Name) || strings.Contains(f.Name, "..") {
			return errors.New("a vulnerability database path leaves its folder")
		}
		target := filepath.Join(dest, filepath.Clean(f.Name))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			continue
		}
		if !f.Mode().IsRegular() {
			return errors.New("the vulnerability database holds something other than files")
		}
		total += f.UncompressedSize64
		if total > maxVulnDB {
			return errors.New("the vulnerability database is larger than its bound")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		//nolint:gosec // G302: the gate user, in the gate's group, reads the database; no one else can reach the gate's folder.
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
		if err != nil {
			_ = rc.Close()
			return err
		}
		_, err = io.Copy(out, io.LimitReader(rc, maxVulnDB))
		_ = rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}
