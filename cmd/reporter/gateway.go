package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/procgroup"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// The Gateway health check (deploy/libexec/gateway-check.sh) reads IBC's and
// the adapters' journals: exit 0 is logged in (or nothing to say), exit 1 is
// logged out, anything else is a check that could not tell. It is bounded as
// the clock check is (clock.go).
var (
	gatewayCheckTimeout   = 40 * time.Second
	gatewayCheckWaitDelay = 5 * time.Second
)

const (
	// gatewayPagesAfter is how long the Gateway must be logged out, counted
	// from the end of any restart window, before it pages: two timer periods
	// (5 minutes each) of margin over a login that is merely slow (ADR 0019,
	// "Alerting").
	gatewayPagesAfter = 10 * time.Minute
	// gatewayRemindEvery is the reminder interval while it stays logged out.
	gatewayRemindEvery = time.Hour
)

// gatewaySchedule is when the Gateway is documented to log in again: the
// nightly restart (TI_GATEWAY_RESTART_TIME, 12-hour clock, default 11:45 PM)
// and the Sunday cold restart (TI_GATEWAY_COLD_RESTART, 24-hour, default
// 09:00), both New York time, read from the same settings the IBC render uses
// (deploy/libexec/render-ibc-config.sh). Being logged out near them pages
// nothing; the grace counts from the window's end.
type gatewaySchedule struct{ nightly, cold int } // minutes after midnight

// The window around a restart: it opens gatewayWindowLead before it, and
// closes after the nightly restart's login (the Gateway restarts, then logs in
// and may wait on the phone) or the Sunday cold restart's, which waits on the
// phone prompt.
const (
	gatewayWindowLead        = 5 * time.Minute
	gatewayNightlyWindowTail = 45 * time.Minute
	gatewayColdWindowTail    = 30 * time.Minute
)

func gatewayScheduleFrom(getenv func(string) string) (gatewaySchedule, error) {
	pick := func(name, fallback string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return fallback
	}
	nightly, err := time.Parse("3:04 PM", pick("TI_GATEWAY_RESTART_TIME", "11:45 PM"))
	if err != nil {
		return gatewaySchedule{}, fmt.Errorf("reporter: TI_GATEWAY_RESTART_TIME must be HH:MM AM/PM, as IBC's AutoRestartTime is: %w", err)
	}
	cold, err := time.Parse("15:04", pick("TI_GATEWAY_COLD_RESTART", "09:00"))
	if err != nil {
		return gatewaySchedule{}, fmt.Errorf("reporter: TI_GATEWAY_COLD_RESTART must be HH:MM on a 24-hour clock, as IBC's ColdRestartTime is: %w", err)
	}
	return gatewaySchedule{nightly: nightly.Hour()*60 + nightly.Minute(), cold: cold.Hour()*60 + cold.Minute()}, nil
}

type gatewayWindow struct{ start, end time.Time }

// windows returns every restart window that can touch [from, to].
func (s gatewaySchedule) windows(from, to time.Time) []gatewayWindow {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	from, to = from.In(loc), to.In(loc)
	var out []gatewayWindow
	for d := time.Date(from.Year(), from.Month(), from.Day()-1, 0, 0, 0, 0, loc); !d.After(to); d = d.AddDate(0, 0, 1) {
		at := func(mins int) time.Time { return time.Date(d.Year(), d.Month(), d.Day(), mins/60, mins%60, 0, 0, loc) }
		n := at(s.nightly)
		out = append(out, gatewayWindow{n.Add(-gatewayWindowLead), n.Add(gatewayNightlyWindowTail)})
		if d.Weekday() == time.Sunday {
			c := at(s.cold)
			out = append(out, gatewayWindow{c.Add(-gatewayWindowLead), c.Add(gatewayColdWindowTail)})
		}
	}
	return out
}

// inWindow reports whether t is inside a restart window.
func (s gatewaySchedule) inWindow(t time.Time) bool {
	for _, w := range s.windows(t, t) {
		if !t.Before(w.start) && t.Before(w.end) {
			return true
		}
	}
	return false
}

// graceFrom is when the page threshold starts counting for an outage seen
// since: that time, or the end of the latest restart window the outage
// overlaps, whichever is later.
func (s gatewaySchedule) graceFrom(since, now time.Time) time.Time {
	from := since
	for _, w := range s.windows(since, now) {
		if w.end.After(from) && w.start.Before(now) && w.end.After(since) {
			from = w.end
		}
	}
	return from
}

// gatewayNoEvidenceExit is the check's exit status for a readable journal that
// holds no login event (rotated, or aged out): neither logged in nor out.
const gatewayNoEvidenceExit = 4

// runGatewayCheck runs the check: out is true for exit 1, silent for exit 4
// (no evidence). Any other failure, a missing command or a timeout is an
// error, not a logout.
func runGatewayCheck(command string) (out, silent bool, said string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), gatewayCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return procgroup.Kill(cmd.Process.Pid) }
	cmd.WaitDelay = gatewayCheckWaitDelay
	raw, runErr := cmd.CombinedOutput()
	said = strings.TrimSpace(string(raw))
	if len(said) > maxClockOutput {
		said = said[:maxClockOutput]
	}
	var exit *exec.ExitError
	switch {
	case runErr == nil:
		return false, false, said, nil
	case ctx.Err() != nil:
		return false, false, said, fmt.Errorf("reporter: the gateway check timed out after %s", gatewayCheckTimeout)
	case errors.As(runErr, &exit) && exit.ExitCode() == 1:
		return true, false, said, nil
	case errors.As(runErr, &exit) && exit.ExitCode() == gatewayNoEvidenceExit:
		return false, true, said, nil
	default:
		return false, false, said, fmt.Errorf("reporter: the gateway check could not tell: %w (it said: %s)", runErr, said)
	}
}

// hostGateway is the host's record of the Gateway: the channels that took its
// page and since when it has been seen logged out.
type hostGateway struct {
	delivered
	Since *time.Time `json:"logged_out_since,omitempty"`
}

func loadHostGateway(path string) (hostGateway, error) {
	var h hostGateway
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err == nil {
		err = json.Unmarshal(raw, &h)
	}
	if err != nil {
		return h, fmt.Errorf("reporter: the gateway state %s is unreadable and is not reset, since a reset forgets a page already sent: %w", path, err)
	}
	return h, nil
}

// gatewayStep checks the Gateway under an exclusive lock on the host's
// gateway state, so a check is made by one reporter at a time (the caller
// runs it only for the owning instance). A logged-out Gateway is timed from
// the first firing that saw it, or from the end of a restart window it was
// seen in; it pages once the time passes gatewayPagesAfter outside a window,
// is reminded every gatewayRemindEvery, and a firing that finds it logged in
// resolves the page and forgets it, so the next outage pages anew.
func gatewayStep(path, command, host string, sched gatewaySchedule, senders []sender, setAside []channelFault, clock func() time.Time) error {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("reporter: the gateway state's lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil
		}
		return fmt.Errorf("reporter: the gateway state's lock: %w", err)
	}
	h, err := loadHostGateway(path)
	if err != nil {
		return err
	}
	out, silent, said, err := runGatewayCheck(command)
	if err != nil {
		return err
	}
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
		raw, err := json.Marshal(hostGateway{delivered: deliveredOf(seen, sent, repeats, nil), Since: h.Since})
		if err != nil {
			return err
		}
		return writeAtomic(path, raw)
	}
	if pruneReceipts(seen, sent, repeats, now) {
		if err := record(); err != nil {
			return err
		}
	}
	key := report.GatewayKey(host)
	// A journal with no login event proves nothing about a login: while an
	// outage is open it stays open, and none is opened from silence.
	if silent && (h.Since != nil || deliveredAnywhere(seen, key)) {
		out = true
		said = "no login event is left in the journal to show a recovery (it rotated, or the last event aged out); still treated as logged out since the earlier check. " + said
		if h.Since == nil {
			t := now
			h.Since = &t
		}
	}
	if !out {
		h.Since = nil
		if !deliveredAnywhere(seen, key) {
			return record()
		}
		failed, resolveErr := resolveAll(senders, key, seen, now)
		keep := map[string]bool{}
		for _, f := range setAside {
			keep[f.channel] = f.channel == "pagerduty"
		}
		for _, name := range failed {
			keep[name] = true
		}
		forgetAlert(key, keep, seen, sent, repeats)
		return errors.Join(resolveErr, record())
	}
	if h.Since == nil {
		t := now
		h.Since = &t
	}
	if err := record(); err != nil {
		return err
	}
	if sched.inWindow(now) || now.Sub(sched.graceFrom(*h.Since, now)) < gatewayPagesAfter {
		return nil
	}
	return deliver(senders, report.GatewayLoggedOutAlert(host, *h.Since, now, said), seen, sent, repeats, now, gatewayRemindEvery, record)
}
