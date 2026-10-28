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
// The count is a file of epoch-second lines, one per exec of IBC since the
// last accepted login: each is written, file and directory synced, BEFORE IBC
// is launched, so a crash between the login and the parse of its refusal still
// counts. A start where no line of IBC's is ever read (xvfb-run without
// ibcstart.sh) gives its attempt back. A completed login, or the prompt for the
// second factor (IBKR has accepted the credentials), resets the count to ONE
// reserved attempt, on disk, for a relogin IBC may make by itself (the nightly
// or Sunday restart): its refusal is already counted if this program dies
// before reading it. IBC's output goes to the journal one line at a time,
// synchronously, after the count is settled; a journal that takes nothing for
// SinkTimeout fails the run closed (77) rather than queueing or dropping
// anything. A count that cannot be read, trusted or written fails closed: the
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
	"sync/atomic"
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
	Pattern       *regexp.Regexp // IBC's refused-login dialog
	Accepted      *regexp.Regexp // a completed login, or the second-factor prompt
	StopDeadline  time.Duration  // SIGTERM to SIGKILL, and SIGKILL to giving up
	DrainDeadline time.Duration  // how long output is read after the child ends
	SinkTimeout   time.Duration  // how long one write to the journal may take before the run fails closed
	Now           func() time.Time
}

const (
	defaultMax     = 3
	defaultWindow  = 24 * time.Hour
	defaultStop    = 15 * time.Second
	defaultDrain   = 5 * time.Second
	defaultSink    = 30 * time.Second
	defaultPattern = `Unrecognized Username or Password`
	// defaultAccepted matches IBC's own lines for an accepted login, anchored
	// so that no other text containing the words clears the count. "IBC: Login
	// has completed" is the line in IBC 3.24.2's log (it ends the Gateway's
	// logins in the journal). The second-factor prompt line follows the form of
	// the refusal's ("detected dialog entitled: ...; event=Opened") but its
	// wording is NOT verified against IBC 3.24.2; if it is wrong the prompt
	// clears nothing and the start stays counted, which fails closed (a few
	// unapproved prompts then stop the unit), never open.
	defaultAccepted = `IBC: Login has completed\s*$|detected dialog entitled: Second Factor Authentication; event=Opened\s*$`
	maxLine         = 64 << 10
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
		SinkTimeout:   time.Duration(intIn("TI_GATEWAY_SINK_TIMEOUT", 1, 300, int(defaultSink/time.Second))) * time.Second,
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
	c.Accepted = regexp.MustCompile(defaultAccepted)
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

// verdictState holds the one outcome of a run: a stop (77) always replaces a
// retry (75), whichever is seen first, and nothing replaces a stop.
type verdictState struct {
	mu sync.Mutex
	v  *verdict
}

func (s *verdictState) set(v verdict) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.v == nil || v.code > s.v.code {
		s.v = &v
	}
}

func (s *verdictState) get() *verdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.v
}

func run(cfg config, stdout, stderr io.Writer) int {
	errw := &lockedWriter{w: stderr}
	// say reports to the journal too, with the same bound: a journal that takes
	// nothing must not hold the exit, and after one refusal it is not tried again.
	var sayDead atomic.Bool
	say := func(format string, a ...any) {
		if sayDead.Load() {
			return
		}
		msg := fmt.Sprintf("gateway-start: "+format+"\n", a...)
		done := make(chan struct{})
		go func() {
			_, _ = io.WriteString(errw, msg)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(cfg.SinkTimeout):
			sayDead.Store(true)
		}
	}
	stopped := func(format string, a ...any) int {
		say(stopPhrase+": "+format, a...)
		return exitStopped
	}
	if cfg.Accepted == nil {
		cfg.Accepted = regexp.MustCompile(defaultAccepted)
	}
	if cfg.SinkTimeout <= 0 {
		cfg.SinkTimeout = defaultSink
	}

	if len(cfg.Render) > 0 {
		//nolint:gosec // G204: the renderer is the unit's own installed script (TI_GATEWAY_RENDER only in the rehearsal), with no argument from outside.
		r := exec.Command(cfg.Render[0], cfg.Render[1:]...)
		r.Stdout, r.Stderr = stdout, errw
		if err := r.Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) && ee.ExitCode() > 0 {
				// The renderer's own configuration exit, which the unit does not
				// restart on: the line tells the health check IBC is not running.
				say("cannot start IBC: the config renderer exited %d (see its message above)", ee.ExitCode())
				return ee.ExitCode()
			}
			// Not a configuration exit: 78 would not be restarted.
			say("cannot start IBC: the config renderer did not run: %v", err)
			return exitInternal
		}
	}
	if len(cfg.Command) == 0 {
		say("cannot start IBC: no command for it: GATEWAY_MAJOR and RUNTIME_DIRECTORY are not set by the unit")
		return exitConfig
	}

	n, raw, err := loadCount(cfg.StateFile, cfg.Now(), cfg.Window)
	if err != nil {
		return stopped("%s is unreadable or malformed (%v), so the number of login attempts is unknown; not starting IBC so the IBKR login cannot be locked. Check the paper login on the IBKR website, then delete %s and start the unit", cfg.StateFile, err, cfg.StateFile)
	}
	if n >= cfg.Max {
		return stopped("%d login attempts within the last %d hours without an accepted login; not starting IBC so the IBKR login cannot be locked. Check the paper login on the IBKR website, then delete %s and start the unit", n, int(cfg.Window/time.Hour), cfg.StateFile)
	}
	// The attempt is recorded before IBC is launched. A count that cannot be
	// written cannot bound anything.
	if err := writeDurable(cfg.StateFile, append(raw, []byte(strconv.FormatInt(cfg.Now().Unix(), 10)+"\n")...)); err != nil {
		return stopped("%s cannot be written (%v), so login attempts could not be counted; not starting IBC so the IBKR login cannot be locked", cfg.StateFile, err)
	}

	say("starting IBC (login attempt %d of %d within the window)", n+1, cfg.Max)
	// Only an exec of IBC that actually ran is an attempt: a start that fails
	// before it gives its count back.
	giveBack := func() int {
		if err := writeDurable(cfg.StateFile, raw); err != nil {
			return stopped("the attempt of a start that never ran IBC could not be given back (%v), so the count may be one too high; not starting IBC", err)
		}
		return exitInternal
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		say("cannot start IBC: no pipe for its output: %v", err)
		return giveBack()
	}
	//nolint:gosec // G204: IBC's command is built from the unit's environment (GATEWAY_MAJOR, RUNTIME_DIRECTORY) or TI_GATEWAY_IBC in the rehearsal, never from IBC's output.
	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		say("cannot start IBC: %v", err)
		return giveBack()
	}
	_ = pw.Close()

	var (
		final  verdictState
		stopCh = make(chan struct{})
		once   sync.Once
	)
	stop := func(v verdict) {
		final.set(v)
		once.Do(func() { close(stopCh) })
	}
	// IBC has run once any of its lines has been read; a child that ends before
	// that (xvfb-run without IBC) gives its attempt back.
	var proved atomic.Bool
	// toJournal writes one line to the journal, synchronously, and fails the
	// run closed when the journal stops taking output: nothing is queued, so
	// nothing is dropped or grows, and the count is already on disk.
	sinkDead := false
	toJournal := func(text string) {
		if sinkDead {
			return
		}
		done := make(chan struct{})
		go func() {
			_, _ = io.WriteString(stdout, text)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(cfg.SinkTimeout):
			sinkDead = true
			stop(verdict{exitStopped, stopPhrase + ": the journal has not taken IBC's output for " + cfg.SinkTimeout.String() + ", so what IBC reports cannot be recorded; stopping IBC and not retrying"})
		}
	}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		br := bufio.NewReaderSize(pr, maxLine)
		seen := false
		for {
			line, err := br.ReadSlice('\n')
			if errors.Is(err, bufio.ErrBufferFull) {
				// A line this long is no log line; nothing can be trusted to
				// watch for the refusal in it.
				stop(verdict{exitStopped, stopPhrase + ": IBC wrote a line longer than 64 KiB; stopping IBC and not retrying"})
				err = nil
			}
			if len(line) > 0 {
				text := string(line)
				trimmed := strings.TrimRight(text, "\r\n")
				accepted, refused := cfg.Accepted.MatchString(trimmed), cfg.Pattern.MatchString(trimmed)
				if accepted || refused || strings.Contains(trimmed, "IBC: ") {
					proved.Store(true)
				}
				// The count is settled on disk BEFORE the line is passed on or
				// any decision is taken.
				if accepted {
					// IBKR accepted the login. One attempt stays reserved, on disk,
					// for a relogin IBC may make by itself (the nightly or Sunday
					// restart, a second-factor relogin): its refusal is counted
					// even if this process dies before reading it.
					if err := writeDurable(cfg.StateFile, []byte(strconv.FormatInt(cfg.Now().Unix(), 10)+"\n")); err != nil {
						say("the login attempts could not be confirmed reset after the accepted login (%v); the count may be the old one or the reserved one", err)
					}
					seen = false
				}
				if !seen && refused {
					seen = true
					count, _, cerr := loadCount(cfg.StateFile, cfg.Now(), cfg.Window)
					switch {
					case cerr != nil:
						stop(verdict{exitStopped, fmt.Sprintf(stopPhrase+": the login count cannot be read (%v); stopping IBC and not retrying", cerr)})
					case count >= cfg.Max:
						stop(verdict{exitStopped, fmt.Sprintf(stopPhrase+": %d of %d login attempts within the window were refused; stopping IBC and not retrying", count, cfg.Max)})
					default:
						stop(verdict{exitRetry, fmt.Sprintf("IBC reports a refused login (attempt %d of %d within the window); stopping IBC so the unit starts it again", count, cfg.Max)})
					}
				}
				toJournal(text)
				if !strings.HasSuffix(text, "\n") {
					toJournal("\n")
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
	childEnded := false
	stopChild := func() {
		// Only this program's own child, through its own handle.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case waitErr = <-exited:
			childEnded = true
		case <-time.After(cfg.StopDeadline):
			_ = cmd.Process.Kill()
			select {
			case waitErr = <-exited:
				childEnded = true
			case <-time.After(cfg.StopDeadline):
				say("IBC did not end after SIGKILL; leaving it to systemd's control-group stop")
			}
		}
	}
	select {
	case waitErr = <-exited:
		childEnded = true
	case <-stopCh:
		stopChild()
	case <-sigs:
		stopChild()
	case <-readerDone:
		// The output ended. If IBC ends with it, that is all; if IBC still runs,
		// nothing watches for its refusal.
		select {
		case waitErr = <-exited:
			childEnded = true
		case <-time.After(time.Second):
			stop(verdict{exitStopped, stopPhrase + ": IBC's output ended while IBC still runs, so nothing can watch for a refused login; stopping IBC and not retrying"})
			stopChild()
		}
	}

	// Everything IBC wrote is read: the pipe's buffered data first, then an end
	// at EOF, or at the deadline when an orphan of IBC still holds the pipe
	// (the attempt was counted before launch, so what an orphan writes later
	// cannot lift the bound).
	_ = pr.SetReadDeadline(time.Now().Add(cfg.DrainDeadline))
	select {
	case <-readerDone:
	case <-time.After(cfg.DrainDeadline + 2*time.Second):
		stop(verdict{exitStopped, stopPhrase + ": the output reader did not finish; the refusal may be unseen, so not retrying"})
	}
	_ = pr.Close()

	if v := final.get(); v != nil {
		say("%s", v.msg)
		return v.code
	}
	if !proved.Load() {
		// The child ended without one line of IBC's: IBC never ran (xvfb-run
		// without ibcstart.sh), so this start was no login attempt.
		if code := giveBack(); code != exitInternal {
			return code
		}
		say("cannot start IBC: the child ended without IBC having run; the attempt was given back")
	}
	var ee *exec.ExitError
	switch {
	case !childEnded:
		return exitInternal
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
