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
	// The restart rule's state (advanceRestartRule): the unit's restart count at
	// the last good sample, the quiet baseline, the run of quiet firings, and
	// whether a failing start is open and opened the outage alone.
	Restarts     *int `json:"restarts,omitempty"`      // NRestarts at the last good sample
	Baseline     *int `json:"baseline,omitempty"`      // NRestarts when the unit was last seen quiet twice in a row
	Quiet        int  `json:"quiet,omitempty"`         // consecutive good quiet firings
	StartFailing bool `json:"start_failing,omitempty"` // a failing start is open, until it closes
	RestartOnly  bool `json:"restart_only,omitempty"`  // the outage was opened by the restart rule alone
	// Held is the end of the maintenance window that held this outage's page:
	// a record only, that the outage was seen and not paged inside it
	// (maintenance.go).
	Held *time.Time `json:"held_in_maintenance_until,omitempty"`
	// Closing is the close summary's line about this outage, owed after the
	// window that held it closed, and removed once it has been delivered.
	Closing string `json:"closing_summary,omitempty"`
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
func gatewayStep(path string, check gatewayChecker, sample func() (gatewayUnitSample, error), host string, sched gatewaySchedule, window func() time.Time, senders []sender, setAside []channelFault, clock func() time.Time) (retErr error) {
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
	// A systemctl that fails does not suppress what the journal evidences: the
	// page is decided and delivered below on the journal alone, and the failure
	// is then the firing's error.
	unit, sampleErr := sample()
	defer func() { retErr = errors.Join(retErr, sampleErr) }()
	out, said := ev.out, ev.said
	restartOut, restartClosed := false, false
	if sampleErr == nil {
		var detail string
		restartOut, restartClosed, detail = h.advanceRestartRule(unit)
		said += detail
	} else {
		// A firing whose sample failed leaves the baseline alone and counts no
		// quiet firing (it breaks the streak); while a failing start is open the
		// outage stays open whatever the journal shows.
		h.Quiet = 0
		if h.StartFailing {
			restartOut = true
			said += " ti-gateway.service keeps failing to start; systemd could not be asked at this firing."
		}
	}
	out = out || restartOut
	// Only an outage the restart rule opened alone is closed by the unit being
	// quiet: the journal's evidence is closed only by a completed login or a
	// restoration newer than the outage's start. With a silent journal the
	// reporter can only tell that the start loop has ended.
	if ev.out {
		h.RestartOnly = false
	}
	if restartClosed && !ev.out && h.RestartOnly {
		ev.recoveredAt = now
	}
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
		raw, err := json.Marshal(hostGateway{delivered: deliveredOf(seen, sent, repeats, nil), Since: h.Since, Resolving: h.Resolving, Restarts: h.Restarts, Baseline: h.Baseline, Quiet: h.Quiet, StartFailing: h.StartFailing, RestartOnly: h.RestartOnly, Held: h.Held, Closing: h.Closing})
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
	// What a closed window held is told once, by the routine channel, and is
	// owed until it has been delivered (the same rule as the unit failures').
	defer func() {
		retErr = errors.Join(retErr, flushGatewayClosing(&h, senders, seen, sent, repeats, host, now, record))
	}()
	key := report.GatewayKey(host)
	// An open outage is closed only by a completed login or a restored link
	// NEWER than its start. Silence in the journals, or events older than the
	// outage that remain once the evidence of it has rotated away, prove
	// nothing; none is opened from them either.
	// A pending resolution of an earlier incident is not an open outage, but a
	// Since is: a new outage needs fresh evidence of recovery even while an old
	// resolution is pending.
	open := h.Since != nil || (deliveredAnywhere(seen, key) && !h.Resolving)
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
		if h.Held != nil {
			h.Closing = "IB Gateway recovered"
		}
		h.Since, h.RestartOnly, h.Held = nil, false, nil
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
		h.RestartOnly = !ev.out && restartOut
	}
	if err := record(); err != nil {
		return err
	}
	// A maintenance window records the outage and pages nothing, reminders
	// included. It does not move the outage's start, so the ten minutes still
	// count from it: an outage already that old pages at the first firing after
	// the window closes, and a shorter one waits out the rest (restart windows
	// still count).
	if hold := holdFor(report.KindGatewayLoggedOut, window); !hold.IsZero() {
		h.Held = &hold
		return record()
	}
	wasHeld := h.Held != nil
	h.Held = nil
	if sched.inWindow(now) || now.Sub(sched.graceFrom(*h.Since, now)) < gatewayPagesAfter {
		if wasHeld {
			h.Closing = "IB Gateway still logged out (not paged yet: its 10 minutes have not passed)"
		}
		return record()
	}
	if h.Resolving {
		// The new outage is due its page while an earlier recovery's resolution
		// is still pending: the channel that never took that resolution holds a
		// receipt for the old incident and would stay silent, so the receipts go
		// now. Until here a recovery still retries the pending resolution.
		forgetAlert(key, map[string]bool{}, seen, sent, repeats)
		h.Resolving = false
	}
	err = deliver(senders, report.GatewayLoggedOutAlert(host, *h.Since, now, said), seen, sent, repeats, now, gatewayRemindEvery, record)
	if wasHeld {
		h.Closing = "IB Gateway still logged out (paged)"
		if err != nil {
			h.Closing = "IB Gateway still logged out (page failed, retrying)"
		}
	}
	return errors.Join(err, record())
}

// flushGatewayClosing sends the Gateway's line of the close summary, only
// where the daily summary goes, and clears it once delivered (or when there is
// nowhere to send it).
func flushGatewayClosing(h *hostGateway, senders []sender, seen map[string]bool, sent map[string]time.Time, repeats map[string]int, host string, now time.Time, record func() error) error {
	if h.Closing == "" {
		return nil
	}
	if summaryTakerAmong(senders) {
		body := "maintenance window closed; held: " + h.Closing
		a := report.Alert{Key: "maintenance-closed-gateway:" + shortHash(body), Kind: kindSummary, Severity: report.Routine, AccountID: host, At: now, Title: host + ": maintenance window closed", Body: body, PublicBody: body}
		if err := deliver(senders, a, seen, sent, repeats, now, gatewayRemindEvery, record); err != nil {
			return err
		}
		forgetAlert(a.Key, nil, seen, sent, repeats)
	}
	h.Closing = ""
	return record()
}

// advanceRestartRule applies the restart rule to a good sample of the Gateway
// unit and says whether a failing start is open, and whether it has just closed.
//
// The rule (docs/manual/server.md, "The Gateway is logged out"; ADR 0019,
// "Alerting": fail closed on a unit that does not stay up): the reporter keeps
// a quiet baseline, the unit's NRestarts when it was last seen quiet (active,
// with NRestarts unchanged from the previous good sample) at two firings in a
// row. A failing start is detected when NRestarts minus that baseline is at
// least gatewayRestartsPage, for any loop that restarts at least every ten
// minutes (gateway-start bounds a cycle under six minutes, whatever its
// settings): a 30 s loop makes about ten a firing,
// a ten-minute one a restart every other firing, which never lets the baseline
// move. A fall in NRestarts (a manual start, reset-failed) re-bases and counts
// as neither a rise nor a quiet firing. An open failing start closes after
// gatewayQuietToClose consecutive good quiet firings.
func (h *hostGateway) advanceRestartRule(unit gatewayUnitSample) (open, closed bool, detail string) {
	cur := unit.restarts
	if h.Restarts == nil || h.Baseline == nil {
		// The first sample, or a record written before the baseline existed: the
		// baseline starts here. An open failing start stays open (and closes
		// after two quiet firings from here).
		h.Restarts, h.Baseline, h.Quiet = &cur, &cur, 0
		return h.StartFailing, false, restartDetail(h.StartFailing, 0)
	}
	if *h.Baseline < 0 || *h.Restarts < 0 || *h.Baseline > *h.Restarts {
		// A saved baseline that cannot be right is no baseline.
		h.Restarts, h.Baseline, h.Quiet = &cur, &cur, 0
		return h.StartFailing, false, restartDetail(h.StartFailing, 0) + " (the saved restart baseline was not valid and was reset)"
	}
	prev := *h.Restarts
	if cur < prev {
		h.Restarts, h.Baseline, h.Quiet = &cur, &cur, 0
		return h.StartFailing, false, restartDetail(h.StartFailing, 0)
	}
	quiet := unit.active && cur == prev
	if quiet {
		h.Quiet++
	} else {
		h.Quiet = 0
	}
	h.Restarts = &cur
	if quiet && h.Quiet >= gatewayQuietToClose {
		h.Baseline = &cur
	}
	rise := cur - *h.Baseline
	switch {
	case rise >= gatewayRestartsPage:
		h.StartFailing = true
		return true, false, restartDetail(true, rise)
	case h.StartFailing && h.Quiet >= gatewayQuietToClose:
		h.StartFailing = false
		return false, true, ""
	case h.StartFailing:
		return true, false, restartDetail(true, 0)
	}
	return false, false, ""
}

func restartDetail(failing bool, rise int) string {
	switch {
	case !failing:
		return ""
	case rise > 0:
		return fmt.Sprintf(" ti-gateway.service keeps failing to start: its restart count is %d above where it last stayed quiet.", rise)
	}
	return " ti-gateway.service keeps failing to start: it has not yet been quiet for two firings in a row."
}
