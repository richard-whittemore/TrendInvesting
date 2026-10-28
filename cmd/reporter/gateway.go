package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
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
type gatewaySchedule struct {
	nightly, cold int // minutes after midnight
	loc           *time.Location
}

// loadNewYork loads the zone the restart times are in; a test replaces it. A
// host without the zone data cannot place the windows, so the firing fails
// rather than guess another zone.
var loadNewYork = func() (*time.Location, error) { return time.LoadLocation("America/New_York") }

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
	loc, err := loadNewYork()
	if err != nil {
		return gatewaySchedule{}, fmt.Errorf("reporter: the Gateway's restart windows are in New York time and the host has no time zone data for it (install tzdata): %w", err)
	}
	return gatewaySchedule{nightly: nightly.Hour()*60 + nightly.Minute(), cold: cold.Hour()*60 + cold.Minute(), loc: loc}, nil
}

type gatewayWindow struct{ start, end time.Time }

// windows returns every restart window that touches [from, to]. The dates
// run from the day before from to the day after to, so a window that opens the
// previous evening (a restart just after midnight) is found.
func (s gatewaySchedule) windows(from, to time.Time) []gatewayWindow {
	from, to = from.In(s.loc), to.In(s.loc)
	var out []gatewayWindow
	add := func(w gatewayWindow) {
		if w.end.After(from) && !w.start.After(to) {
			out = append(out, w)
		}
	}
	for d := time.Date(from.Year(), from.Month(), from.Day()-1, 0, 0, 0, 0, s.loc); !d.After(to.AddDate(0, 0, 1)); d = d.AddDate(0, 0, 1) {
		at := func(mins int) time.Time {
			return time.Date(d.Year(), d.Month(), d.Day(), mins/60, mins%60, 0, 0, s.loc)
		}
		n := at(s.nightly)
		add(gatewayWindow{n.Add(-gatewayWindowLead), n.Add(gatewayNightlyWindowTail)})
		if d.Weekday() == time.Sunday {
			c := at(s.cold)
			add(gatewayWindow{c.Add(-gatewayWindowLead), c.Add(gatewayColdWindowTail)})
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

// hostGateway is the host's record of the Gateway: the channels that took its
// page and since when it has been seen logged out.
type hostGateway struct {
	delivered
	Since *time.Time `json:"logged_out_since,omitempty"`
	// Resolving is set when the Gateway has recovered but a channel has not yet
	// taken the resolution: the next firing retries it and does not read the
	// receipts that remain as an outage still open.
	Resolving bool `json:"resolving,omitempty"`
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
func gatewayStep(path string, check gatewayChecker, host string, sched gatewaySchedule, senders []sender, setAside []channelFault, clock func() time.Time) error {
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
	now := clock()
	ev, err := check(now)
	if err != nil {
		return err
	}
	out, said := ev.out, ev.said
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
		raw, err := json.Marshal(hostGateway{delivered: deliveredOf(seen, sent, repeats, nil), Since: h.Since, Resolving: h.Resolving})
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
	// An open outage is closed only by a completed login or a restored link
	// NEWER than its start. Silence in the journals, or events older than the
	// outage that remain once the evidence of it has rotated away, prove
	// nothing; none is opened from them either.
	open := (h.Since != nil || deliveredAnywhere(seen, key)) && !h.Resolving
	// An equal time counts as newer: the journal's times are to the
	// microsecond, and a login in the same instant as the firing is a login.
	if !out && open && (h.Since == nil || ev.recoveredAt.Before(*h.Since)) {
		out = true
		said = "no completed login or restored link newer than the outage's start is in the journals (the evidence may have rotated away); still treated as logged out. " + said
		if h.Since == nil {
			t := now
			h.Since = &t
		}
	}
	if !out {
		h.Since = nil
		if !deliveredAnywhere(seen, key) {
			h.Resolving = false
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
		h.Resolving = deliveredAnywhere(seen, key)
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
	if h.Resolving {
		// The new outage is due its page while an earlier recovery's resolution
		// is still pending: the channel that never took that resolution holds a
		// receipt for the old incident and would stay silent, so the receipts go
		// now. Until here a recovery still retries the pending resolution.
		forgetAlert(key, map[string]bool{}, seen, sent, repeats)
		h.Resolving = false
	}
	return deliver(senders, report.GatewayLoggedOutAlert(host, *h.Since, now, said), seen, sent, repeats, now, gatewayRemindEvery, record)
}
