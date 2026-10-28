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
// bound is 3 attempts within 24 hours.
//
// The count is a file of epoch-second lines, one per attempt in the last 24
// hours. Every exec of IBC is one attempt, written to disk (file and directory
// synced) BEFORE IBC starts, whether or not it got as far as a login; a
// completed login, or the prompt for the second factor (IBKR has accepted the
// credentials), clears them. A refusal after such a login in the same run (the
// nightly or Sunday restart, a second-factor relogin) is a new attempt, written
// when it is seen; if this program dies between seeing it and writing it the
// next start counts anyway, so the bound is loose by at most one. At 3 attempts
// the program exits 77. A count that cannot be read, trusted or written fails
// closed the same way, without launching IBC.
//
// Every write to the journal is bounded. A journal that errs, writes short or
// takes nothing for SinkTimeout stops IBC and exits 75, so the unit starts
// again: the bound above does not depend on the journal, so this heals.
//
// Exit codes: 75 = stop IBC and start again (a refusal below the bound, or a
// journal that failed); 77 = retries stopped, always saying "login retries
// stopped" for the health check to recognise (the unit does not restart); 70 =
// IBC could not be started (restarted); the renderer's own codes (64, 65, 78)
// are passed on, each with a "cannot start IBC" line; otherwise IBC's own
// status.
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
	MarkerFile    string   // written when IBC cannot be started, for the health check, even when the journal takes nothing
	Warnings      []string // settings that were invalid, reported through the same bounded journal writes
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
		MarkerFile:    pick("TI_GATEWAY_MARKER_FILE", filepath.Join(filepath.Dir(pick("TI_GATEWAY_STATE_FILE", defaultState)), "last-start-failure")),
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
func writeDurable(path string, data []byte) error { return writeDurableMode(path, data, 0o600) }

// writeDurableMode is writeDurable with the file's mode: the marker the health
// check reads is world-readable, the count is not.
func writeDurableMode(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
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

// drainReader reads the pipe; once IBC has ended each read waits at most d, set
// before the read, so a read is not cut off by time spent forwarding earlier lines.
type drainReader struct {
	f        *os.File
	d        time.Duration
	draining *atomic.Bool
}

func (r drainReader) Read(p []byte) (int, error) {
	if r.draining.Load() {
		_ = r.f.SetReadDeadline(time.Now().Add(r.d))
	}
	return r.f.Read(p)
}

// journal is where this program and IBC's output are written: every write has
// a bound, so a journal that takes nothing cannot hold the starter, and a write
// that errs, or is short, or misses the bound is reported to the caller.
type journal struct {
	mu      sync.Mutex // one write at a time to the underlying writer
	w       io.Writer
	timeout time.Duration
	dead    atomic.Bool
}

// write returns an error when the journal failed this line; once it has missed
// the bound it is not tried again (a blocked write would still be in flight).
func (j *journal) write(text string) error {
	if j.dead.Load() {
		return errors.New("the journal is not taking output")
	}
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		j.mu.Lock()
		defer j.mu.Unlock()
		n, err := io.WriteString(j.w, text)
		done <- result{n, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return r.err
		}
		if r.n < len(text) {
			return io.ErrShortWrite
		}
		return nil
	case <-time.After(j.timeout):
		j.dead.Store(true)
		return fmt.Errorf("the journal took nothing for %s", j.timeout)
	}
}

// boundedWriter lets a child's output (the renderer's) go through a journal and
// remembers that a write failed, so the caller can stop before starting IBC.
type boundedWriter struct {
	j      *journal
	failed *atomic.Bool
}

func (b boundedWriter) Write(p []byte) (int, error) {
	if err := b.j.write(string(p)); err != nil {
		b.failed.Store(true)
	}
	return len(p), nil
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

// run starts IBC and supervises it. The rule in one sentence: every exec of
// IBC is one attempt, written to disk before it starts, and a completed login
// clears them; a refusal after a completed login in the same run is a new
// attempt, written when it is seen.
func run(cfg config, stdout, stderr io.Writer) int {
	if cfg.Accepted == nil {
		cfg.Accepted = regexp.MustCompile(defaultAccepted)
	}
	if cfg.SinkTimeout <= 0 {
		cfg.SinkTimeout = defaultSink
	}
	out := &journal{w: stdout, timeout: cfg.SinkTimeout}
	errj := &journal{w: stderr, timeout: cfg.SinkTimeout}
	say := func(format string, a ...any) { _ = errj.write(fmt.Sprintf("gateway-start: "+format+"\n", a...)) }
	stopped := func(format string, a ...any) int {
		say(stopPhrase+": "+format, a...)
		return exitStopped
	}
	// cannot reports that IBC cannot be started: to the journal, and to a marker
	// file beside the count that the health check reads, so the Gateway being
	// down is seen even when the journal takes nothing on either stream.
	cannot := func(format string, a ...any) {
		msg := fmt.Sprintf("cannot start IBC: "+format, a...)
		say("%s", msg)
		if cfg.MarkerFile != "" {
			_ = writeDurableMode(cfg.MarkerFile, []byte(fmt.Sprintf("%d %s\n", cfg.Now().Unix(), msg)), 0o644)
		}
	}
	for _, w := range cfg.Warnings {
		say("%s", w)
	}

	if len(cfg.Render) > 0 {
		//nolint:gosec // G204: the renderer is the unit's own installed script (TI_GATEWAY_RENDER only in the rehearsal), with no argument from outside.
		r := exec.Command(cfg.Render[0], cfg.Render[1:]...)
		var renderFailed atomic.Bool
		r.Stdout, r.Stderr = boundedWriter{out, &renderFailed}, boundedWriter{errj, &renderFailed}
		err := r.Run()
		if renderFailed.Load() {
			// What the renderer said cannot be journalled: stop before IBC starts.
			cannot("the journal is not taking the config renderer's output, so the unit starts again; no attempt was counted")
			return exitRetry
		}
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) && ee.ExitCode() > 0 {
				// The renderer's own configuration exit, which the unit does not
				// restart on: the line tells the health check IBC is not running.
				cannot("the config renderer exited %d (see its message above)", ee.ExitCode())
				return ee.ExitCode()
			}
			// Not a configuration exit: 78 would not be restarted.
			cannot("the config renderer did not run: %v", err)
			return exitInternal
		}
	}
	if len(cfg.Command) == 0 {
		cannot("no command for it: GATEWAY_MAJOR and RUNTIME_DIRECTORY are not set by the unit")
		return exitConfig
	}

	n, raw, err := loadCount(cfg.StateFile, cfg.Now(), cfg.Window)
	if err != nil {
		return stopped("%s is unreadable or malformed (%v), so the number of login attempts is unknown; not starting IBC so the IBKR login cannot be locked. Check the paper login on the IBKR website, then delete %s and start the unit", cfg.StateFile, err, cfg.StateFile)
	}
	if n >= cfg.Max {
		return stopped("%d login attempts within the last %d hours without an accepted login (a start that failed counts too); not starting IBC so the IBKR login cannot be locked. Check the paper login on the IBKR website, and the cause of any failed start, then delete %s and start the unit", n, int(cfg.Window/time.Hour), cfg.StateFile)
	}
	attempt := func(existing []byte) []byte {
		return append(existing, []byte(strconv.FormatInt(cfg.Now().Unix(), 10)+"\n")...)
	}
	// The attempt is on disk before IBC starts. A count that cannot be written
	// cannot bound anything.
	if err := writeDurable(cfg.StateFile, attempt(raw)); err != nil {
		return stopped("%s cannot be written (%v), so login attempts could not be counted; not starting IBC so the IBKR login cannot be locked", cfg.StateFile, err)
	}
	say("starting IBC (login attempt %d of %d within the window)", n+1, cfg.Max)

	pr, pw, err := os.Pipe()
	if err != nil {
		cannot("no pipe for its output: %v", err)
		return exitInternal
	}
	//nolint:gosec // G204: IBC's command is built from the unit's environment (GATEWAY_MAJOR, RUNTIME_DIRECTORY) or TI_GATEWAY_IBC in the rehearsal, never from IBC's output.
	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		cannot("%v", err)
		return exitInternal
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

	// progress is how long after origin the reader last finished a line (the
	// monotonic clock, so a correction of the wall clock changes nothing); the
	// drain below waits on it, so lines the reader already holds, written
	// through a slow (not stuck) journal, are delivered within the drain's bounds.
	origin := time.Now()
	var progress atomic.Int64
	var draining, abandoned atomic.Bool
	mark := func() { progress.Store(int64(time.Since(origin))) }
	mark()
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		br := bufio.NewReaderSize(drainReader{pr, cfg.DrainDeadline, &draining}, maxLine)
		seen, relogin := false, false
		for {
			if abandoned.Load() {
				return // the run is over and its verdict final: nothing more is acted on
			}
			line, err := br.ReadSlice('\n')
			mark()
			if errors.Is(err, bufio.ErrBufferFull) {
				// A line this long is no log line; nothing can be trusted to
				// watch for the refusal in it.
				stop(verdict{exitStopped, stopPhrase + ": IBC wrote a line longer than 64 KiB; stopping IBC and not retrying"})
				err = nil
			}
			if len(line) > 0 {
				text := string(line)
				trimmed := strings.TrimRight(text, "\r\n")
				// The count is settled on disk BEFORE the line goes to the journal
				// or any decision is taken.
				if cfg.Accepted.MatchString(trimmed) {
					if err := writeDurable(cfg.StateFile, nil); err != nil {
						say("the login attempts could not be confirmed cleared after the accepted login (%v); the count may still hold the old attempts", err)
					}
					if cfg.MarkerFile != "" {
						_ = os.Remove(cfg.MarkerFile)
					}
					relogin, seen = true, false
				}
				if !seen && cfg.Pattern.MatchString(trimmed) {
					seen = true
					count, existing, cerr := loadCount(cfg.StateFile, cfg.Now(), cfg.Window)
					if cerr == nil && relogin {
						// IBC logged in again by itself and was refused: its own attempt.
						cerr = writeDurable(cfg.StateFile, attempt(existing))
						count++
					}
					switch {
					case cerr != nil:
						stop(verdict{exitStopped, fmt.Sprintf(stopPhrase+": the login count cannot be kept (%v); stopping IBC and not retrying", cerr)})
					case count >= cfg.Max:
						stop(verdict{exitStopped, fmt.Sprintf(stopPhrase+": %d of %d login attempts within the window, the last refused (a start that failed counts too); stopping IBC and not retrying", count, cfg.Max)})
					default:
						stop(verdict{exitRetry, fmt.Sprintf("IBC reports a refused login (attempt %d of %d within the window); stopping IBC so the unit starts it again", count, cfg.Max)})
					}
				}
				werr := out.write(text)
				if werr == nil && !strings.HasSuffix(text, "\n") {
					werr = out.write("\n")
				}
				mark()
				if werr != nil {
					// The bound on attempts does not depend on the journal, so a
					// journal that fails is a restartable stop, which heals.
					stop(verdict{exitRetry, fmt.Sprintf("the journal is not taking IBC's output (%v), so what IBC reports cannot be recorded; stopping IBC so the unit starts it again", werr)})
				}
			}
			if err != nil {
				closedByUs := abandoned.Load() && errors.Is(err, os.ErrClosed)
				if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrDeadlineExceeded) && !closedByUs {
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

	// After IBC ends its output is read on, each read bounded by DrainDeadline of
	// waiting (the deadline is set before every read, so time spent writing
	// buffered lines to the journal does not use it up). What the reader gets
	// through within the bounds is delivered, each line bounded by SinkTimeout;
	// what is left when a bound ends the wait is NOT: output an orphan of IBC
	// writes later than DrainDeadline after the last read, and lines still
	// buffered or in the pipe. Two bounds end the wait with 75 (restartable:
	// every exec was counted before it started, so a reader that never
	// finished cannot lift the bound): no progress for DrainDeadline +
	// SinkTimeout + 2 s, and a total of 2 x SinkTimeout + DrainDeadline since
	// IBC ended (65 s by default).
	drainStart := time.Now()
	mark()
	draining.Store(true)
	_ = pr.SetReadDeadline(time.Now().Add(cfg.DrainDeadline))
	idle := cfg.DrainDeadline + cfg.SinkTimeout + 2*time.Second
	total := 2*cfg.SinkTimeout + cfg.DrainDeadline
drain:
	for {
		select {
		case <-readerDone:
			break drain
		case <-time.After(250 * time.Millisecond):
			switch {
			case time.Since(drainStart) > total:
				stop(verdict{exitRetry, "the output reader did not finish within the drain's total bound; the refusal may be unseen, so stopping and starting again (the attempt is already counted)"})
				break drain
			case time.Since(origin)-time.Duration(progress.Load()) > idle:
				stop(verdict{exitRetry, "the output reader made no progress after IBC ended; the refusal may be unseen, so stopping and starting again (the attempt is already counted)"})
				break drain
			}
		}
	}
	abandoned.Store(true) // before the close, so a read it wakes returns quietly
	_ = pr.Close()

	if v := final.get(); v != nil {
		say("%s", v.msg)
		return v.code
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
	cfg.Warnings = warn
	os.Exit(run(cfg, os.Stdout, os.Stderr))
}
