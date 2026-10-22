// Command triage analyses each alert issue once, with a headless Claude Code
// session on the owner's Mac mini, and writes the analysis as one comment
// (docs/manual/alerts-and-dashboard.md, "Alerts analysed by Claude"). launchd
// runs it every few minutes; each run is one poll, and a poll that finds no
// alert issue without its comment makes no model call.
//
// It holds the triage token (the repository's issues and pulls) and nothing
// that reaches the server, the broker or the secrets: the journal slice comes
// from the dashboard's read-only route over the tailnet, with no credential.
// The session's tools are a fixed list; it never performs an owner action,
// only states it; a code defect's fix goes through the repository's normal
// flow, tests first, as a draft PR the job never merges. Every rule lives
// where the comments name it; this is composition.
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
	"strings"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

func main() {
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
	repo := flags.String("repo", "", "the repository the alert issues are in, owner/name")
	repoDir := flags.String("repo-dir", "", "the job's own clone of the repository, outside any synced folder")
	tokenFile := flags.String("token-file", "", "file holding the triage token (fine-grained: Issues, Pull requests and Contents, read and write, on the one repository); readable by you only")
	claudeTokenFile := flags.String("claude-token-file", "", "file holding the token `claude setup-token` printed, readable by you only; without it the session uses this user's own Claude Code login")
	sliceURL := flags.String("slice-url", "", "the dashboard's journal slice on the tailnet: https://<server>.<tailnet>.ts.net/api/journal-slice")
	dailyCap := flags.Int("daily-cap", 10, "the most model calls a New York day; past it each new issue gets one \"triage skipped: daily limit\" comment")
	author := flags.String("alert-author", "", "the GitHub login that opens the alert issues (the reporter's token's user); default the triage token's own user")
	claude := flags.String("claude", "claude", "the Claude Code program")
	model := flags.String("model", "", "the session's model; default Claude Code's own")
	claudeTimeout := flags.Duration("claude-timeout", 20*time.Minute, "the longest one session may run")
	maxTurns := flags.Int("max-turns", 60, "the most turns one session may take")
	testCmd := flags.String("test-cmd", "go test ./...", "the gate's red check: it must fail at the tests' commit")
	checkCmd := flags.String("check-cmd", "make check", "the gate's green check: it must pass at the fix's commit")
	checkTimeout := flags.Duration("check-timeout", 45*time.Minute, "the longest one gate command may run")
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
	case *repo == "":
		return errors.New("triage: -repo is required (owner/name)")
	case *repoDir == "":
		return errors.New("triage: -repo-dir is required (the job's own clone)")
	case *sliceURL == "":
		return errors.New("triage: -slice-url is required")
	case *dailyCap < 0:
		return errors.New("triage: -daily-cap must be 0 or more")
	case *maxTurns <= 0:
		return errors.New("triage: -max-turns must be positive")
	case len(strings.Fields(*testCmd)) == 0 || len(strings.Fields(*checkCmd)) == 0:
		return errors.New("triage: -test-cmd and -check-cmd must each name a command")
	}
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
	claudeToken := ""
	if *claudeTokenFile != "" {
		if claudeToken, err = readSecret(*claudeTokenFile, "-claude-token-file"); err != nil {
			return err
		}
	}
	gh, err := newGitHubTracker(getenv("TI_GITHUB_API_URL"), getenv("TI_GITHUB_API_ALLOW_HOST"), *repo, token, client)
	if err != nil {
		return err
	}
	slices, err := newHTTPSlicer(*sliceURL, getenv("TI_TRIAGE_SLICE_ALLOW_HOST"), client)
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
	j := &job{
		tracker: gh,
		slicer:  slices,
		analyst: claudeCLI{path: *claude, model: *model, timeout: *claudeTimeout, maxTurns: *maxTurns, env: sessionEnv(environ, claudeToken)},
		fixer: &gitWorkspace{
			repoDir: *repoDir, workRoot: filepath.Join(*stateDir, "work"), run: timedRunner{execRunner{}, *checkTimeout},
			env: gitEnv(kept(environ, keptNames)), checkEnv: checkEnv(environ),
			testCmd: strings.Fields(*testCmd), checkCmd: strings.Fields(*checkCmd),
			pushURL: "https://github.com/" + *repo + ".git", token: token,
		},
		redactor:  report.NewRedactor([]string{token, claudeToken, sliceHost, host, short}),
		statePath: filepath.Join(*stateDir, "state.json"), offFile: offFile,
		dailyCap: *dailyCap, author: *author, location: ny, now: time.Now,
		testCmd: *testCmd, checkCmd: *checkCmd, log: log,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return j.poll(ctx)
}

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

func (t timedRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	return t.inner.Run(ctx, dir, env, name, args...)
}
