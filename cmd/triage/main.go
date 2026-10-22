// Command triage analyses each alert issue once, with a headless Claude Code
// session on the owner's Mac mini, and writes the analysis as one comment
// (docs/manual/alerts-and-dashboard.md, "Alerts analysed by Claude"). launchd
// runs it every few minutes; each run is one poll, and a poll that finds no
// alert issue without its comment makes no model call.
//
// It holds the triage token (the alerts repository's issues; the code
// repository's contents and pulls) and the Claude token, both read into
// memory at its start, and nothing that reaches the server, the broker or
// the secrets: the journal slice comes from the dashboard's read-only route
// over the tailnet, with no credential and no figure. The session runs in
// Claude Code's sandbox, with exact commands, each through this program's
// own wrapper, which only asks the job to run it (runTool, serveTools); the
// code it writes runs only in the gate, as another macOS user in a sandbox
// with no network but loopback (gateBox);
// it never performs an owner action, only states it; a code defect's fix
// goes through the repository's normal flow, tests first, as a draft PR the
// job never merges. Every rule lives where the comments name it; this is
// composition.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == toolCommand {
		os.Exit(runTool(os.Args[2:], os.Environ(), os.Stdout, os.Stderr))
	}
	client := &http.Client{Timeout: 30 * time.Second}
	if err := run(os.Args[1:], os.Environ(), os.Stderr, client); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args, environ []string, log io.Writer, client *http.Client) error {
	flags := flag.NewFlagSet("triage", flag.ContinueOnError)
	flags.SetOutput(log)
	stateDir := flags.String("state-dir", "", "the job's folder: its state file, its lock, its worktrees and its off file (OFF)")
	alertsRepo := flags.String("alerts-repo", "", "the repository the alert issues are in, owner/name (it may be private); default -code-repo")
	codeRepo := flags.String("code-repo", "", "the repository the code is in, where a fix's branch is pushed and its draft PR opened, owner/name; default -alerts-repo")
	repoDir := flags.String("repo-dir", "", "the job's own clone of the repository, outside any synced folder")
	tokenFile := flags.String("token-file", "", "file holding the alerts repository's token (fine-grained: Issues, read and write, on the alerts repository only), and the code repository's too unless -code-token-file is given; readable by you only, in a folder the gate user cannot read")
	codeTokenFile := flags.String("code-token-file", "", "file holding the code repository's token (fine-grained: Contents and Pull requests, read and write, on the code repository only), when -token-file's covers the alerts repository only; readable by you only")
	claudeTokenFile := flags.String("claude-token-file", "", "file holding the token `claude setup-token` printed, readable by you only; without it the session uses this user's own Claude Code login")
	sliceURL := flags.String("slice-url", "", "the dashboard's journal slice on the tailnet: https://<server>.<tailnet>.ts.net/api/journal-slice")
	dailyCap := flags.Int("daily-cap", 10, "the most model calls a New York day; past it each new issue gets one \"triage skipped: daily limit\" comment")
	author := flags.String("alert-author", "", "the GitHub login that opens the alert issues (the reporter's token's user); default the triage token's own user")
	claude := flags.String("claude", "claude", "the Claude Code program")
	model := flags.String("model", "", "the session's model; default Claude Code's own")
	claudeTimeout := flags.Duration("claude-timeout", 20*time.Minute, "the longest one session may run")
	maxTurns := flags.Int("max-turns", 60, "the most turns one session may take")
	checkCmd := flags.String("check-cmd", "make check", "the gate's green check: it must pass at the fix's commit (the red check is always go test -run over the new tests)")
	checkTimeout := flags.Duration("check-timeout", 45*time.Minute, "the longest one gate command may run")
	gateDir := flags.String("gate-dir", "", "the gate's folder: yours, group the gate user's, mode 750 (runs, module cache, vulnerability database)")
	gateUser := flags.String("gate-user", "ti-triage-gate", "the macOS user the gate's commands run as, through sudo")
	sliceWindow := flags.Duration("slice-window", 72*time.Hour, "how far before and after the alert the journal slice reaches")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *stateDir == "" {
		return errors.New("triage: -state-dir is required")
	}
	offFile := filepath.Join(*stateDir, "OFF")
	if _, err := os.Stat(offFile); err == nil {
		_, _ = fmt.Fprintf(log, "triage: off (%s exists); nothing read, nothing written\n", offFile)
		return nil
	}
	switch {
	case *alertsRepo == "" && *codeRepo == "":
		return errors.New("triage: -alerts-repo or -code-repo is required (owner/name)")
	case *gateDir == "":
		return errors.New("triage: -gate-dir is required (the gate's folder)")
	case *repoDir == "":
		return errors.New("triage: -repo-dir is required (the job's own clone)")
	case *sliceURL == "":
		return errors.New("triage: -slice-url is required")
	case *dailyCap < 0:
		return errors.New("triage: -daily-cap must be 0 or more")
	case *maxTurns <= 0:
		return errors.New("triage: -max-turns must be positive")
	case len(strings.Fields(*checkCmd)) == 0:
		return errors.New("triage: -check-cmd must name a command")
	case *sliceWindow <= 0:
		return errors.New("triage: -slice-window must be positive")
	}
	alerts, code := repositories(*alertsRepo, *codeRepo)
	getenv := func(name string) string {
		for _, e := range environ {
			if k, v, ok := strings.Cut(e, "="); ok && k == name {
				return v
			}
		}
		return ""
	}
	token, err := readSecret(*tokenFile, "-token-file")
	if err != nil {
		return err
	}
	codeToken := token
	if *codeTokenFile != "" {
		if codeToken, err = readSecret(*codeTokenFile, "-code-token-file"); err != nil {
			return err
		}
	}
	claudeToken := ""
	if *claudeTokenFile != "" {
		if claudeToken, err = readSecret(*claudeTokenFile, "-claude-token-file"); err != nil {
			return err
		}
	}
	gh, err := newGitHubTracker(getenv("TI_GITHUB_API_URL"), getenv("TI_GITHUB_API_ALLOW_HOST"), alerts, code, token, codeToken, client)
	if err != nil {
		return err
	}
	journalSlice, err := newHTTPSlicer(*sliceURL, getenv("TI_TRIAGE_SLICE_ALLOW_HOST"), client)
	if err != nil {
		return err
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		return errors.New("triage: the America/New_York zone is not installed, so the day of the cap cannot be told")
	}
	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		return fmt.Errorf("triage: make -state-dir: %w", err)
	}
	release, err := lock(*stateDir)
	if errors.Is(err, errLocked) {
		_, _ = fmt.Fprintln(log, "triage: another poll is running; this one does nothing")
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	sliceHost := ""
	if u, err := url.Parse(*sliceURL); err == nil {
		sliceHost = u.Hostname()
	}
	host, _ := os.Hostname()
	short, _, _ := strings.Cut(host, ".")
	// The folders no code the session wrote may read: the token files'
	// folders and the job's home.
	var secretDirs []string
	for _, f := range []string{*tokenFile, *codeTokenFile, *claudeTokenFile} {
		if f != "" {
			if abs, err := filepath.Abs(filepath.Dir(f)); err == nil && !slices.Contains(secretDirs, abs) {
				secretDirs = append(secretDirs, abs)
			}
		}
	}
	denyGate := slices.Clone(secretDirs)
	if home := getenv("HOME"); home != "" {
		denyGate = append(denyGate, home)
	}
	gpath := gatePath(getenv("PATH"), getenv("HOME"))
	toolchain := goToolchain(gpath)
	sudo := orDefault(getenv("TI_TRIAGE_SUDO"), "/usr/bin/sudo")
	gate := &gateBox{
		user: *gateUser, root: *gateDir, path: gpath, golden: getenv("TI_GOLDEN_DIR"), toolchain: toolchain,
		sudo: sudo, sandboxExec: orDefault(getenv("TI_TRIAGE_SANDBOX_EXEC"), "/usr/bin/sandbox-exec"),
		denyRead: denyGate, run: timedRunner{execRunner{}, *checkTimeout},
		listening:        netstatListening(orDefault(getenv("TI_TRIAGE_NETSTAT"), "/usr/sbin/netstat")),
		endGateProcesses: gateEnder(sudo, *gateUser, "/bin/ps"),
	}
	// The session's commands reach the job over a socket in a folder only
	// this user can open (serveTools).
	brokerDir, err := os.MkdirTemp("", "ti-tools-")
	if err != nil {
		return errors.New("triage: the folder of the session's socket could not be made")
	}
	defer func() { _ = os.RemoveAll(brokerDir) }()
	if brokerDir, err = filepath.EvalSymlinks(brokerDir); err != nil {
		return errors.New("triage: the folder of the session's socket could not be read")
	}
	socket := filepath.Join(brokerDir, "s")
	env := sessionEnv(environ, claudeToken)
	workRoot := filepath.Join(*stateDir, "work")
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		return errors.New("triage: this program's own path cannot be told, so the session's commands cannot be pinned to it")
	}
	// What the session's commands may read under /Users: the worktrees and
	// this program, the wrapper they run through, which runs nothing itself.
	readable := []string{workRoot, self}
	fixer := &gitWorkspace{
		repoDir: *repoDir, workRoot: workRoot, run: timedRunner{execRunner{}, *checkTimeout},
		env: gitEnv(kept(environ, keptNames)), gate: gate, checkCmd: strings.Fields(*checkCmd),
		pushURL: "https://github.com/" + code + ".git", token: codeToken, fetchVulnDB: httpVulnDB(client),
	}
	j := &job{
		tracker: gh,
		slicer:  journalSlice,
		analyst: claudeCLI{path: *claude, model: *model, timeout: *claudeTimeout, maxTurns: *maxTurns, env: env, tool: self,
			settings: sessionSettings(secretDirs, readable, nil, []string{socket}), socket: socket, tools: fixer},
		fixer:     fixer,
		redactor:  report.NewRedactor([]string{token, codeToken, claudeToken, sliceHost, host, short}),
		statePath: filepath.Join(*stateDir, "state.json"), offFile: offFile,
		dailyCap: *dailyCap, author: *author, location: ny, now: time.Now, sliceWindow: *sliceWindow,
		checkCmd: *checkCmd, log: log,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return j.poll(ctx)
}

// gateEnder makes the gate's end of its user's processes (pkillGateUser;
// the end-to-end tests, whose stand-in gate runs every command as the
// test's own user, swap it for a fake that ends nothing).
var gateEnder = pkillGateUser

// readSecret reads a token from a file only its owner can read; a file
// others can read is refused, since the token would not be the owner's alone.
func readSecret(path, name string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("triage: %s is required", name)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("triage: %s: the token file cannot be read", name)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("triage: %s must be a file only you can read (chmod 600)", name)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("triage: %s: the token file cannot be read", name)
	}
	return strings.TrimSpace(string(raw)), nil
}

// errLocked is another poll holding the lock.
var errLocked = errors.New("triage: another poll is running")

// lock takes the job's lock, so two polls never run at once and no issue is
// invoked twice. The operating system drops it when the process ends, so a
// poll that died never holds it.
func lock(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("triage: open the lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, fmt.Errorf("triage: take the lock: %w", err)
	}
	return func() { _ = f.Close() }, nil
}

// timedRunner bounds each command to timeout.
type timedRunner struct {
	inner   runner
	timeout time.Duration
}

func (t timedRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	return t.inner.Run(ctx, dir, env, name, args...)
}

// repositories is the alerts and the code repository: one given names both.
func repositories(alerts, code string) (alertsRepo, codeRepo string) {
	if alerts == "" {
		alerts = code
	}
	if code == "" {
		code = alerts
	}
	return alerts, code
}

// gatePath is the gate's PATH: the job's, less the folders under the job's
// home, which the gate user cannot read and the session could have written.
func gatePath(path, home string) string {
	var keep []string
	for _, p := range filepath.SplitList(path) {
		if p == "" || !filepath.IsAbs(p) || home != "" && (p == home || strings.HasPrefix(p, home+string(filepath.Separator))) {
			continue
		}
		keep = append(keep, p)
	}
	return strings.Join(keep, string(filepath.ListSeparator))
}

// goToolchain is the folder of the Go installation the gate's PATH finds
// (its bin folder's parent), which the sandboxes let commands read; "" when
// there is none.
func goToolchain(path string) string {
	for _, d := range filepath.SplitList(path) {
		p, err := filepath.EvalSymlinks(filepath.Join(d, "go"))
		if err == nil && filepath.IsAbs(p) {
			return filepath.Dir(filepath.Dir(p))
		}
	}
	return ""
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
