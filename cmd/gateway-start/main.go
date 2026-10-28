// Command gateway-start starts IB Gateway under IBC and bounds the retry of a
// refused login. It renders IBC's config.ini (deploy/libexec/render-ibc-config.sh),
// starts xvfb-run and IBC as its own child, and reads the child's output line
// by line, passing it to the journal.
//
// IBC 3.24.2 has no setting for a refused login ("Unrecognized Username or
// Password"): it leaves the dialog open and never exits, so the Gateway sat
// logged out for hours with the unit active. On the first such dialog of a run
// this program counts it, durably, and then stops its own child (SIGTERM, then
// SIGKILL after a deadline) so systemd starts the unit again. It signals
// nothing but that child's own process handle: no group, no pid 0 or -1, no
// killall or pkill (docs/development.md, "Ending processes"). What is left of
// IBC's process tree (java, Xvfb) is ended by systemd's KillMode=control-group
// when this program exits.
//
// IBKR's documentation says "After ten failed login attempts in 24 hours, your
// Portal log in will be disabled" (IBKR Guides, "Log In and Log Out",
// https://www.ibkrguides.com/clientportal/gettingstarted/login.htm; the FAQ
// "Troubleshooting client portal login failures", article 1132, is cited from
// search only, its page being a script). That is the Client Portal login; IBKR
// publishes no separate figure for the Gateway's API login, so the same is
// assumed. The owner's own failed web attempts count against it too, so the
// bound is 3 refused logins within 24 hours.
//
// The count is a file of epoch-second lines, written with the file and its
// directory synced BEFORE the child is stopped, and emptied by a completed
// login. A count that cannot be read, trusted or written fails closed: the
// program exits 77 without launching IBC, since an unknown history cannot
// authorise another try. Exit codes: 75 = a refused login was seen and the
// bound is not reached (the unit restarts); 77 = retries stopped, always saying
// "login retries stopped" for the health check to recognise (the unit does not
// restart); the renderer's own codes (64, 65, 78) are passed on; otherwise
// IBC's own status.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	exitRetry    = 75
	exitStopped  = 77
	exitInternal = 70
	exitConfig   = 78
	// stopPhrase is what the health check (cmd/reporter) keys the stop on.
	stopPhrase = "login retries stopped"
)

type config struct {
	Render        []string // renders IBC's config; empty skips it
	Command       []string // IBC's command
	StateFile     string
	Max           int
	Window        time.Duration
	Pattern       *regexp.Regexp
	StopDeadline  time.Duration // SIGTERM to SIGKILL, and SIGKILL to giving up
	DrainDeadline time.Duration // how long output is read after the child ends
	Now           func() time.Time
}

const (
	defaultMax      = 3
	defaultWindow   = 24 * time.Hour
	defaultStop     = 15 * time.Second
	defaultDrain    = 5 * time.Second
	defaultPattern  = `Unrecognized Username or Password`
	defaultRender   = "/usr/local/libexec/trend-investing/render-ibc-config.sh"
	defaultState    = "/var/lib/ti-gateway/login-failures"
	maxStopDeadline = 300
)

// configFromEnv reads the settings, falling back to the default (never to
// "unbounded") for any that is invalid; the second result says which.
func configFromEnv(getenv func(string) string) (c config, warn []string) {
	intIn := func(name string, lo, hi, def int) int {
		v := getenv(name)
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			warn = append(warn, fmt.Sprintf("%s=%q is not a whole number from %d to %d; using %d", name, v, lo, hi, def))
			return def
		}
		return n
	}
	pick := func(name, def string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return def
	}
	c = config{
		Render:        []string{pick("TI_GATEWAY_RENDER", defaultRender)},
		StateFile:     pick("TI_GATEWAY_STATE_FILE", defaultState),
		Max:           intIn("TI_GATEWAY_LOGIN_MAX_FAILURES", 1, 9, defaultMax),
		StopDeadline:  time.Duration(intIn("TI_GATEWAY_STOP_DEADLINE", 1, maxStopDeadline, int(defaultStop/time.Second))) * time.Second,
		DrainDeadline: defaultDrain,
		Now:           time.Now,
	}
	// A window shorter than IBKR's 24 hours would forget refusals it still counts.
	c.Window = time.Duration(intIn("TI_GATEWAY_LOGIN_WINDOW", 86400, 7*86400, 86400)) * time.Second
	pattern, err := regexp.Compile(pick("TI_GATEWAY_LOGIN_FAILURE_PATTERN", defaultPattern))
	if err != nil {
		warn = append(warn, "TI_GATEWAY_LOGIN_FAILURE_PATTERN is not a valid expression; using the default")
		pattern = regexp.MustCompile(defaultPattern)
	}
	c.Pattern = pattern
	if ibc := getenv("TI_GATEWAY_IBC"); ibc != "" {
		c.Command = []string{ibc}
	} else if major, runtime := getenv("GATEWAY_MAJOR"), getenv("RUNTIME_DIRECTORY"); major != "" && runtime != "" {
		home := "/var/lib/ti-gateway"
		c.Command = []string{"xvfb-run", "-a", "--server-args=-screen 0 1024x768x24",
			"/opt/ibc/scripts/ibcstart.sh", major, "--gateway",
			"--tws-path=" + home + "/Jts", "--tws-settings-path=" + home + "/Jts",
			"--ibc-path=/opt/ibc", "--ibc-ini=" + filepath.Join(runtime, "config.ini"),
			"--on2fatimeout=restart"}
	}
	return c, warn
}

var epochLine = regexp.MustCompile(`^\d{1,12}$`)

// loadCount returns the refusals recorded within the window and the file's
// exact content. A count that is not a regular file, cannot be read, or holds
// anything but whole epoch-second lines is an error, never "no history".
func loadCount(path string, now time.Time, window time.Duration) (n int, raw []byte, err error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, err
	}
	if !fi.Mode().IsRegular() {
		return 0, nil, errors.New("not a regular file")
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		return 0, nil, err
	}
	if len(raw) == 0 {
		return 0, raw, nil
	}
	if raw[len(raw)-1] != '\n' {
		return 0, nil, errors.New("the last line is not complete")
	}
	for _, l := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if !epochLine.MatchString(l) {
			return 0, nil, fmt.Errorf("line %q is not a time", l)
		}
		at, _ := strconv.ParseInt(l, 10, 64)
		if now.Sub(time.Unix(at, 0)) < window {
			n++
		}
	}
	return n, raw, nil
}

// syncFile flushes a file or directory to disk.
func syncFile(f *os.File) error { return f.Sync() }

// writeDurable replaces path with data: written to a file beside it, synced,
// renamed over it, and the directory synced, every error returned.
func writeDurable(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := syncFile(f); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return syncFile(dir)
}

// lockedWriter serialises writes from the output reader and the main flow.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

type verdict struct {
	code int
	msg  string
}

func run(cfg config, stdout, stderr io.Writer) int {
	out, errw := &lockedWriter{w: stdout}, &lockedWriter{w: stderr}
	say := func(format string, a ...any) { _, _ = fmt.Fprintf(errw, "gateway-start: "+format+"\n", a...) }
	stopped := func(format string, a ...any) int {
		say(stopPhrase+": "+format, a...)
		return exitStopped
	}

	if len(cfg.Render) > 0 {
		//nolint:gosec // G204: the renderer is the unit's own installed script (TI_GATEWAY_RENDER only in the rehearsal), with no argument from outside.
		r := exec.Command(cfg.Render[0], cfg.Render[1:]...)
		r.Stdout, r.Stderr = out, errw
		if err := r.Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) && ee.ExitCode() > 0 {
				return ee.ExitCode()
			}
			say("the config renderer did not run: %v", err)
			return exitConfig
		}
	}
	if len(cfg.Command) == 0 {
		say("no command for IBC: GATEWAY_MAJOR and RUNTIME_DIRECTORY are not set by the unit")
		return exitConfig
	}

	n, raw, err := loadCount(cfg.StateFile, cfg.Now(), cfg.Window)
	if err != nil {
		return stopped("%s is unreadable or malformed (%v), so the number of refused logins is unknown; not starting IBC so the IBKR login cannot be locked. Check the paper login on the IBKR website, then delete %s and start the unit", cfg.StateFile, err, cfg.StateFile)
	}
	if n >= cfg.Max {
		return stopped("%d refused logins within the last %d hours; not starting IBC so the IBKR login cannot be locked. Check the paper login on the IBKR website, then delete %s and start the unit", n, int(cfg.Window/time.Hour), cfg.StateFile)
	}
	// A count that cannot be written cannot bound anything.
	if err := writeDurable(cfg.StateFile, raw); err != nil {
		return stopped("%s cannot be written (%v), so refused logins could not be counted; not starting IBC so the IBKR login cannot be locked", cfg.StateFile, err)
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		say("cannot make a pipe for IBC's output: %v", err)
		return exitInternal
	}
	//nolint:gosec // G204: IBC's command is built from the unit's environment (GATEWAY_MAJOR, RUNTIME_DIRECTORY) or TI_GATEWAY_IBC in the rehearsal, never from IBC's output.
	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		say("cannot start IBC: %v", err)
		return exitConfig
	}
	_ = pw.Close()

	var (
		mu     sync.Mutex
		final  *verdict
		stopCh = make(chan struct{})
		once   sync.Once
	)
	stop := func(v verdict) {
		mu.Lock()
		if final == nil {
			final = &v
		}
		mu.Unlock()
		once.Do(func() { close(stopCh) })
	}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		br := bufio.NewReader(pr)
		seen := false
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				_, _ = io.WriteString(out, line)
				if !strings.HasSuffix(line, "\n") {
					_, _ = io.WriteString(out, "\n")
				}
				text := strings.TrimRight(line, "\r\n")
				if strings.Contains(text, "Login has completed") {
					if err := writeDurable(cfg.StateFile, nil); err != nil {
						say("the refused-login count could not be emptied after the completed login (%v); it stays, which only makes the bound stricter", err)
					}
				}
				if !seen && cfg.Pattern.MatchString(text) {
					seen = true
					count, raw, err := loadCount(cfg.StateFile, cfg.Now(), cfg.Window)
					if err == nil {
						err = writeDurable(cfg.StateFile, append(raw, []byte(strconv.FormatInt(cfg.Now().Unix(), 10)+"\n")...))
					}
					switch {
					case err != nil:
						stop(verdict{exitStopped, fmt.Sprintf(stopPhrase+": the refused-login count cannot be saved (%v); stopping IBC and not retrying", err)})
					case count+1 >= cfg.Max:
						stop(verdict{exitStopped, fmt.Sprintf(stopPhrase+": %d of %d refused logins within the window; stopping IBC and not retrying", count+1, cfg.Max)})
					default:
						stop(verdict{exitRetry, fmt.Sprintf("IBC reports a refused login (%d of %d within the window); stopping IBC so the unit starts it again", count+1, cfg.Max)})
					}
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrDeadlineExceeded) {
					stop(verdict{exitStopped, fmt.Sprintf(stopPhrase+": the output reader failed (%v); stopping IBC and not retrying", err)})
				}
				return
			}
		}
	}()

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)

	var waitErr error
	stopChild := func() {
		// Only this program's own child, through its own handle.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case waitErr = <-exited:
		case <-time.After(cfg.StopDeadline):
			_ = cmd.Process.Kill()
			select {
			case waitErr = <-exited:
			case <-time.After(cfg.StopDeadline):
				say("IBC did not end after SIGKILL; leaving it to systemd's control-group stop")
			}
		}
	}
	select {
	case waitErr = <-exited:
	case <-stopCh:
		stopChild()
	case <-sigs:
		stopChild()
	}

	// Everything IBC wrote is read: the pipe's buffered data first, then an
	// end at EOF, or at the deadline when an orphan of IBC still holds the pipe.
	_ = pr.SetReadDeadline(time.Now().Add(cfg.DrainDeadline))
	select {
	case <-readerDone:
	case <-time.After(cfg.DrainDeadline + 2*time.Second):
		stop(verdict{exitStopped, stopPhrase + ": the output reader did not finish; the refused-login count may be incomplete, so not retrying"})
	}
	_ = pr.Close()

	mu.Lock()
	v := final
	mu.Unlock()
	if v != nil {
		say("%s", v.msg)
		return v.code
	}
	var ee *exec.ExitError
	switch {
	case waitErr == nil:
		return 0
	case errors.As(waitErr, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	default:
		say("waiting for IBC: %v", waitErr)
		return exitInternal
	}
}

func main() {
	cfg, warn := configFromEnv(os.Getenv)
	for _, w := range warn {
		_, _ = fmt.Fprintln(os.Stderr, "gateway-start:", w)
	}
	os.Exit(run(cfg, os.Stdout, os.Stderr))
}
