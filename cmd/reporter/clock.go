package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// The clock check's bounds. The check (deploy/libexec/clock-check.sh) asks
// timedatectl, then tries three references, each request bounded at 10
// seconds by the script, so its own worst case is about 30 seconds;
// clockCheckTimeout leaves room above that. At the timeout the check's whole
// process group is killed, and clockCheckWaitDelay bounds the wait for any
// child that left the group yet still holds the output, so one check takes at
// most 45 seconds. It runs only after every incident alert of the firing has
// been sent, so it never holds one back (ADR 0019, "Alerting"); after it come
// at most the clock alert or its resolve and the daily summary, two channels
// at -send-timeout (30 s) each: the tail of the run is 45 + 60 + 60 = 165
// seconds. The incident alerts before it are bounded only per send, so with
// several on slow channels the run can pass the unit's TimeoutStartSec of 4
// minutes (deploy/systemd/ti-reporter@.service) inside this step; systemd then
// stops it and the next firing makes the period's check again. Tests shorten
// them.
var (
	clockCheckTimeout   = 40 * time.Second
	clockCheckWaitDelay = 5 * time.Second
)

// clockCheckFresh is how long one check of the host's clock stands for every
// reporter on the host: half the timer's 5-minute period
// (deploy/systemd/ti-reporter@.timer), so the instances that fire in the same
// period share one check, and each period makes a new one.
const clockCheckFresh = 150 * time.Second

// clockPagesAfter is how many checks in a row must fail before the failure
// pages: two, one timer period apart, so within 10 minutes of the clock going
// wrong, and a single unreachable reference never pages. One passing check
// clears the failure (docs/manual/alerts-and-dashboard.md).
const clockPagesAfter = 2

// maxClockOutput is the most of the check's output kept for an alert.
const maxClockOutput = 1000

// runClockCheck runs the clock-check command (deploy/libexec/clock-check.sh:
// exit 0 is a good clock, anything else is not) and returns what it said. A
// command that is missing, not executable, silent or stopped at the timeout is
// a failed check that says which: an unchecked clock is not a checked one.
func runClockCheck(command string, now time.Time) report.ClockCheck {
	ctx, cancel := context.WithTimeout(context.Background(), clockCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command)
	// Its own process group, killed whole at the timeout, so a child such as
	// curl cannot keep it running.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = clockCheckWaitDelay
	out, err := cmd.CombinedOutput()
	said := strings.TrimSpace(string(out))
	if len(said) > maxClockOutput {
		said = said[:maxClockOutput]
	}
	c := report.ClockCheck{At: now, Passed: err == nil, Output: said}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		c.Output = fmt.Sprintf("the check timed out after %s", clockCheckTimeout)
		if said != "" {
			c.Output += "; it had said: " + said
		}
	case errors.Is(err, fs.ErrNotExist) && !errors.As(err, &exit) && !exists(command):
		c.Output = "the check could not be run: " + command + " is missing"
	case errors.Is(err, fs.ErrPermission):
		c.Output = "the check could not be run: " + command + " is not executable by this user"
	case errors.As(err, &exit) && said == "":
		c.Output = fmt.Sprintf("the check failed and said nothing (exit status %d)", exit.ExitCode())
	case said == "":
		c.Output = "the check could not be run: " + err.Error()
	}
	return c
}

// hostClock is the host-level clock state, shared by every reporter instance
// on the host: the channels that took the clock alert, the last check, and how
// many checks in a row have failed.
type hostClock struct {
	delivered
	Last     *report.ClockCheck `json:"last,omitempty"`
	Failures int                `json:"consecutive_failures"`
}

func loadHostClock(path string) (hostClock, error) {
	var h hostClock
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err == nil {
		err = json.Unmarshal(raw, &h)
	}
	if err != nil {
		return h, fmt.Errorf("reporter: the host clock state %s is unreadable and is not reset, since a reset forgets a page already sent: %w", path, err)
	}
	return h, nil
}

// clockStep checks the host's clock, under an exclusive lock on the shared
// clock state so that the host's reporters page and clear once between them.
// An instance that finds the lock held skips the check, since another is
// making it now, and reports the last one recorded. A check lying within
// clockCheckFresh of now, before or after it (a sibling's clock reading a
// moment later), is this period's and is reused rather than made again or
// counted again. A check dated further ahead than that means the clock went
// back, which is itself a failed check: it is counted, a new check is made
// for the evidence, and the two are one failure. A failure pages once
// clockPagesAfter checks in a row have failed; a passing check resolves the
// incident on each working PagerDuty channel that took it and forgets it for
// every channel, so the next failure pages anew; the one exception is a
// PagerDuty channel set aside this run or whose resolve failed, whose record
// is kept until a run in which it works. It returns the check the summary shows, if any.
func clockStep(path, command, host string, senders []sender, setAside []channelFault, clock func() time.Time, every time.Duration) (*report.ClockCheck, error) {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("reporter: the host clock state's lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			h, err := loadHostClock(path)
			return h.Last, err
		}
		return nil, fmt.Errorf("reporter: the host clock state's lock: %w", err)
	}
	h, err := loadHostClock(path)
	if err != nil {
		return nil, err
	}
	// The time is taken now, under the lock, not carried from the start of
	// the firing: the alerts delivered before this step can take seconds, and
	// a sibling may have recorded its check meanwhile.
	now := clock()
	seen, sent, repeats := map[string]bool{}, map[string]time.Time{}, map[string]int{}
	for _, k := range h.Keys {
		seen[k] = true
	}
	for k, at := range h.Sent {
		sent[k] = at
	}
	for k, n := range h.Repeats {
		repeats[k] = n
	}
	record := func() error {
		d := deliveredOf(seen, sent, repeats)
		raw, err := json.Marshal(hostClock{delivered: d, Last: h.Last, Failures: h.Failures})
		if err != nil {
			return err
		}
		return writeAtomic(path, raw)
	}
	age := time.Duration(0)
	if h.Last != nil {
		age = now.Sub(h.Last.At)
	}
	if h.Last == nil || age >= clockCheckFresh || age <= -clockCheckFresh {
		c := runClockCheck(command, now)
		switch {
		case h.Last != nil && age <= -clockCheckFresh:
			// The clock went back: that is itself the fault, a failed check
			// counted in its own right whatever the new check says.
			c = steppedBack(c, -age)
			h.Failures++
		case c.Passed:
			h.Failures = 0
		default:
			h.Failures++
		}
		h.Last = &c
		if err := record(); err != nil {
			return h.Last, err
		}
	}
	key := report.ClockDriftKey(host)
	switch {
	case h.Last.Passed && deliveredAnywhere(seen, key):
		failed, resolveErr := resolveAll(senders, key, seen)
		// Each channel's record is cleared on its own (ADR 0019, "Alerting"):
		// a working channel, or one no longer configured, forgets the
		// incident at once so a new fault pages it again. Two kinds of record
		// are kept: a set-aside PagerDuty, which can close the incident and
		// sends its resolve in the first run in which it works, and any
		// channel whose resolve failed, which keeps its record to retry. The
		// rest forget at once and are not held back by them.
		keep := map[string]bool{}
		for _, f := range setAside {
			keep[f.channel] = f.channel == "pagerduty"
		}
		for _, name := range failed {
			keep[name] = true
		}
		forgetAlert(key, keep, seen, sent, repeats)
		if err := record(); err != nil {
			return h.Last, errors.Join(resolveErr, err)
		}
		return h.Last, resolveErr
	case !h.Last.Passed && h.Failures >= clockPagesAfter:
		return h.Last, deliver(senders, report.ClockDriftAlert(host, *h.Last, h.Failures), seen, sent, repeats, now, every, record)
	}
	return h.Last, nil
}

// steppedBack is the failed check for a clock found set back by about the
// given duration: the new check's evidence under the words for the step. A
// passing new check says nothing about the skew, since the clock now agrees
// with the reference only after the step.
func steppedBack(c report.ClockCheck, by time.Duration) report.ClockCheck {
	said := fmt.Sprintf("the clock stepped back by about %s: the last check was recorded that far ahead of now.", by.Round(time.Minute))
	if !c.Passed {
		said += " The check also fails now: " + c.Output
	}
	return report.ClockCheck{At: c.At, Passed: false, Output: said}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// resolver is a channel that can close an incident it opened: PagerDuty.
// ntfy posts are messages, with nothing to close.
type resolver interface {
	resolve(key string) error
}

// resolveAll closes the incident keyed key on every channel that took it and
// can close it. It returns the names of the channels whose resolve failed and
// the joined error; the caller keeps only those channels' records so the next
// firing tries again.
func resolveAll(senders []sender, key string, seen map[string]bool) ([]string, error) {
	var failures []error
	var names []string
	for _, s := range senders {
		r, ok := s.(resolver)
		if !ok || !seen[channelKey(s.name(), key)] {
			continue
		}
		if err := r.resolve(key); err != nil {
			names = append(names, s.name())
			failures = append(failures, fmt.Errorf("%s: resolve %s: %w", s.name(), key, err))
		}
	}
	return names, errors.Join(failures...)
}

// hostName names this server in the clock alert; the clock is the host's.
func hostName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "this server"
	}
	return h
}

// forgetAlert removes every channel's record of the alert keyed key, except
// for the channels named in keep, so the incident is told afresh to the rest
// the next time it arises.
func forgetAlert(key string, keep, seen map[string]bool, sent map[string]time.Time, repeats map[string]int) {
	suffix := channelKey("", key)
	drop := func(k string) bool {
		ch, _, _ := strings.Cut(k, "|")
		return strings.HasSuffix(k, suffix) && !keep[ch]
	}
	for k := range seen {
		if drop(k) {
			delete(seen, k)
		}
	}
	for k := range sent {
		if drop(k) {
			delete(sent, k)
		}
	}
	for k := range repeats {
		if drop(k) {
			delete(repeats, k)
		}
	}
}
