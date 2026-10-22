package main

import (
	"archive/tar"
	"archive/zip"
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
	"slices"
	"strings"
	"syscall"
	"time"
)

// The gate runs code the session wrote: the red check's go test and the
// green check's make check. It holds no credential the job holds
// (docs/manual/alerts-and-dashboard.md, "What the session and the gate can
// and cannot do"): each command runs as the gate's own macOS user, which
// cannot read the job's home or its token folder, through sudo with a fixed
// rule (docs/runbooks/mac-mini.md, "2. The alert triage job"), inside
// sandbox-exec with a profile that denies the network (loopback aside) and
// every write outside its run folder and the shared temporary folders, with
// an environment the job builds whole: an empty home, the module cache
// read-only and offline, Go's vulnerability database from a file, and no
// token of any kind. Each command runs over a fresh copy of one commit; the
// gate user never touches the job's clone or worktree, and the job never
// runs anything from a copy once the gate user has had it.

// gateBox is the gate's user, sandbox and folder.
type gateBox struct {
	// user is the gate's macOS user; root is the gate's folder, which only
	// the job can change (Available), holding its runs, the module cache and
	// the vulnerability database.
	user, root string
	// sudo and sandboxExec are the programs (tests: stand-ins).
	sudo, sandboxExec string
	// path is the commands' PATH; golden their TI_GOLDEN_DIR, if any.
	path, golden string
	// denyRead are the folders the profile denies reading besides what the
	// gate user's own rights deny: the job's home and its token folder.
	denyRead []string
	// gocache, when set, is the build cache of every run (tests only; a run
	// otherwise builds in its own, so no run's code can feed another's).
	gocache string
	run     runner
}

func (g *gateBox) modCache() string { return filepath.Join(g.root, "modcache") }
func (g *gateBox) vulnDB() string   { return filepath.Join(g.root, "vulndb") }
func (g *gateBox) runs() string     { return filepath.Join(g.root, "runs") }

// errGateUnavailable is a gate that cannot run: its user, its sandbox or its
// folder is missing or wrong.
var errGateUnavailable = errors.New("gate unavailable")

// Available checks the gate before any session: the gate user exists and is
// not the job's own, sandbox-exec is there, the gate's folder is the job's
// and no one else can write to it, and a probe runs through sudo and the
// sandbox.
func (g *gateBox) Available(ctx context.Context) error {
	if _, err := user.Lookup(g.user); err != nil {
		return fmt.Errorf("%w: no macOS user %s", errGateUnavailable, g.user)
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
	//nolint:gosec // G301/G302: the gate user, in the gate's group, must write here; no one else can reach the gate's folder (mode 750).
	if err := os.MkdirAll(g.runs(), 0o770); err != nil {
		return fmt.Errorf("%w: the runs folder could not be made", errGateUnavailable)
	}
	// The runs folder is the gate user's to write in, never to rename or
	// remove what the job made there (the sticky bit).
	if err := os.Chmod(g.runs(), 0o770|os.ModeSticky); err != nil {
		return fmt.Errorf("%w: the runs folder's mode could not be set", errGateUnavailable)
	}
	runDir, done, err := g.newRun(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", errGateUnavailable, err)
	}
	defer done()
	if _, err := g.Run(ctx, runDir, []string{"/usr/bin/true"}); err != nil {
		return fmt.Errorf("%w: the probe through sudo and sandbox-exec failed", errGateUnavailable)
	}
	return nil
}

// newRun makes a fresh run folder the gate user can write in: home (also
// HOME, TMPDIR and the build cache) and tree (the copy of a commit). done
// removes it, as the gate user, whose files the job cannot remove.
func (g *gateBox) newRun(ctx context.Context) (runDir string, done func(), err error) {
	var id [4]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", nil, errors.New("no randomness for the run folder")
	}
	//nolint:gosec // G301/G302: the gate user, in the gate's group, must write here; no one else can reach the gate's folder (mode 750).
	if err := os.MkdirAll(g.runs(), 0o770); err != nil {
		return "", nil, errors.New("the runs folder could not be made")
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
		//nolint:gosec // G301/G302: the gate user, in the gate's group, must write here; no one else can reach the gate's folder (mode 750).
		if err := os.Chmod(d, 0o770); err != nil {
			return "", nil, errors.New("the run folder could not be made")
		}
	}
	done = func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_, _ = g.Run(cleanup, runDir, []string{"/bin/sh", "-c", `chmod -R u+rwX "$0" 2>/dev/null; rm -rf "$0"/* "$0"/.[!.]*`, runDir})
		_ = os.RemoveAll(runDir)
	}
	return runDir, done, nil
}

// Run runs argv as the gate user in the sandbox, in runDir's tree when it
// has one, and afterwards ends every process the gate user still runs.
func (g *gateBox) Run(ctx context.Context, runDir string, argv []string) (string, error) {
	name, args := g.command(runDir, argv)
	if args == nil {
		return "", errors.New("the run folder's name cannot be put in the sandbox's profile")
	}
	dir := filepath.Join(runDir, "tree")
	if _, err := os.Stat(dir); err != nil {
		dir = runDir
	}
	out, err := withStop(g.run, g.stopAll).Run(ctx, dir, nil, name, args...)
	g.stopAll()
	return out, err
}

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
// gate, so a background process a test started never outlives its check.
// It never runs as the job's own user, which it would end too.
func (g *gateBox) stopAll() {
	me, err := user.Current()
	if err != nil || me.Username == g.user {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	//nolint:gosec // G204: the operator's -gate-user, through the fixed sudo rule.
	_ = exec.CommandContext(ctx, g.sudo, "-n", "-u", g.user, "/bin/kill", "-KILL", "-1").Run()
}

// command is the gate's command line for argv in runDir: sudo to the gate
// user, sandbox-exec with the profile, env with the whole environment, then
// argv. A folder whose name would break the profile gives no command.
func (g *gateBox) command(runDir string, argv []string) (name string, args []string) {
	profile, ok := sandboxProfile(runDir, g.denyRead)
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
	}
	if g.golden != "" {
		env = append(env, "TI_GOLDEN_DIR="+g.golden)
	}
	args = append([]string{"-n", "-u", g.user, g.sandboxExec, "-p", profile, "/usr/bin/env", "-i"}, env...)
	return g.sudo, append(args, argv...)
}

// sandboxProfile is the gate's sandbox-exec profile: everything a build and
// a test need, except the network beyond loopback (DNS included: the
// resolver's socket is not loopback), writes outside the run folder and the
// shared temporary folders, and reads of denyRead. ps is allowed out of the
// sandbox only because the setuid program cannot run in it, and it reaches
// nothing. A path holding a quote, a backslash or a line break is refused.
func sandboxProfile(runDir string, denyRead []string) (string, bool) {
	paths := append([]string{runDir}, denyRead...)
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
	b.WriteString(`(allow network* (remote unix-socket (subpath "/private/tmp")) (remote unix-socket (subpath ` + quote(runDir) + `)))` + "\n")
	b.WriteString(`(allow network-bind (local unix-socket (subpath "/private/tmp")) (local unix-socket (subpath ` + quote(runDir) + `)))` + "\n")
	b.WriteString("(deny file-write*)\n")
	b.WriteString(`(allow file-write* (subpath ` + quote(runDir) + `) (subpath "/private/tmp") (subpath "/private/var/folders")` +
		` (literal "/dev/null") (literal "/dev/zero") (literal "/dev/dtracehelper") (regex #"^/dev/tty") (regex #"^/dev/fd/"))` + "\n")
	for _, d := range denyRead {
		b.WriteString(`(deny file-read* (subpath ` + quote(d) + `))` + "\n")
	}
	b.WriteString(`(allow process-exec (literal "/bin/ps") (with no-sandbox))` + "\n")
	return b.String(), true
}

// exportTree writes rev of the repository at dir into dest as a fresh git
// repository of one commit (the deployment tests clone their checkout), all
// of it writable by the gate's group, so the gate user can build and test in
// it. The job runs no git command there once the gate user has it.
func exportTree(ctx context.Context, dir string, env []string, rev, dest string) error {
	//nolint:gosec // G204: git archive of a commit the job made.
	cmd := exec.CommandContext(ctx, "git", append(slices.Clone(gitSafe), "archive", "--format=tar", rev)...)
	cmd.Dir, cmd.Env = dir, env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return errors.New("git archive could not start")
	}
	if err := cmd.Start(); err != nil {
		return errors.New("git archive could not start")
	}
	extractErr := extractTar(out, dest)
	_, _ = io.Copy(io.Discard, out)
	if err := cmd.Wait(); err != nil || extractErr != nil {
		return errors.New("the commit could not be copied for the gate")
	}
	for _, args := range [][]string{
		{"init", "--quiet", "-b", "main"},
		{"add", "-A"},
		{"-c", "commit.gpgsign=false", "commit", "--quiet", "--no-verify", "-m", "the commit under the gate"},
	} {
		//nolint:gosec // G204: fixed git commands in a folder the job just made.
		c := exec.CommandContext(ctx, "git", append(slices.Clone(gitSafe), args...)...)
		c.Dir, c.Env = dest, env
		if err := c.Run(); err != nil {
			return errors.New("the copy for the gate could not be made a repository")
		}
	}
	return groupWritable(dest)
}

// extractTar writes a git archive's entries under dest: folders, regular
// files and symbolic links, each path inside dest, owner-only until
// groupWritable opens them to the gate's group.
func extractTar(r io.Reader, dest string) error {
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := filepath.Clean(h.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("an archive path leaves the tree")
		}
		target := filepath.Join(dest, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			mode := os.FileMode(0o600)
			if h.Mode&0o111 != 0 {
				mode = 0o700
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			//nolint:gosec // G110: the archive is of the job's own commit, bounded by the repository.
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("an archive entry of an unknown type")
		}
	}
}

// groupWritable gives the gate's group what the owner has under dir, so the
// gate user can change the copy.
func groupWritable(dir string) error {
	return filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		perm := info.Mode().Perm()
		//nolint:gosec // G122/G302: the job made every path here moments ago and the gate user has not had the copy yet; Walk does not follow links, and links are skipped.
		return os.Chmod(p, perm|(perm&0o700)>>3)
	})
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
		name := filepath.Clean(f.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return errors.New("a vulnerability database path leaves its folder")
		}
		target := filepath.Join(dest, name)
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
