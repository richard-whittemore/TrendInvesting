package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
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
	Since  *time.Time             `json:"logged_out_since,omitempty"`
	Notice report.Kind            `json:"notice_kind,omitempty"`
	Links  map[string]adapterLoss `json:"adapter_losses,omitempty"`
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
}

// adapterLoss owns a connectivity incident's first observation and process
// generation (ADR 0019, "Alerting"). Other units cannot supply its recovery.
type adapterLoss struct {
	Since   time.Time `json:"since"`
	Started time.Time `json:"started"`
}

// updateAdapterLosses applies each unit's evidence only to its own loss (ADR
// 0019, "Alerting"; alerts-and-dashboard.md, "Paging invariant"). Silence never
// resolves a loss; stopping or replacing its process invalidates its evidence.
func (h *hostGateway) updateAdapterLosses(ev *gatewayEvidence, now time.Time) bool {
	if ev.adapters == nil {
		return false
	}
	if h.Links == nil {
		h.Links = map[string]adapterLoss{}
	}
	// Previous-build state has no kind or ownership. A completed IBC login
	// identifies an adapter-sourced outage, except one opened by the restart rule.
	if h.Since != nil && len(h.Links) == 0 && ev.loggedIn && !h.RestartOnly && (h.Notice == "" || h.Notice == report.KindIBKRConnectivity) {
		h.Notice = report.KindIBKRConnectivity
		for unit, evidence := range ev.adapters {
			if !evidence.previousLoss.IsZero() {
				h.Links[unit] = adapterLoss{Since: *h.Since}
			}
		}
		// When the old loss lines rotated away, ownership is unknown. Retain each
		// configured candidate until its own recovery or process change is known.
		if len(h.Links) == 0 {
			for unit := range ev.adapters {
				h.Links[unit] = adapterLoss{Since: *h.Since}
			}
		}
	}
	hadLoss := len(h.Links) > 0
	for unit, evidence := range ev.adapters {
		loss, exists := h.Links[unit]
		if exists {
			obsolete := evidence.sampleErr == nil && (!evidence.active || evidence.started.After(loss.Since) || !loss.Started.IsZero() && evidence.started.After(loss.Started))
			if obsolete || !evidence.restored.Before(loss.Since) || !ev.recoveredAt.Before(loss.Since) {
				delete(h.Links, unit)
			}
		}
		if !evidence.lost.IsZero() {
			if _, exists := h.Links[unit]; !exists {
				h.Links[unit] = adapterLoss{Since: now, Started: evidence.started}
			}
		}
	}
	if len(h.Links) > 0 {
		ev.out = true
		ev.connectivity = ev.loggedIn || ev.silent && h.Notice == report.KindIBKRConnectivity
	}
	return hadLoss && len(h.Links) == 0 && !ev.silent
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
func gatewayStep(path string, check gatewayChecker, sample func() (gatewayUnitSample, error), host string, sched gatewaySchedule, window func() time.Time, unitState string, note func(string), senders []sender, setAside []channelFault, clock func() time.Time, policies ...*pagingPolicy) (retErr error) {
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
		raw, err := json.Marshal(hostGateway{delivered: deliveredOf(seen, sent, repeats, nil), Since: h.Since, Notice: h.Notice, Links: h.Links, Resolving: h.Resolving, Restarts: h.Restarts, Baseline: h.Baseline, Quiet: h.Quiet, StartFailing: h.StartFailing, RestartOnly: h.RestartOnly, Held: h.Held})
		if err != nil {
			return err
		}
		return writeAtomic(path, raw)
	}
	key := report.GatewayKey(host)
	observed, escalation := sent[channelKey("observed", key)], sent[channelKey("escalation", key)]
	if pruneReceipts(seen, sent, repeats, now) {
		// Expiring an earlier receipt cannot erase an open incident's clocks
		// (ADR 0019; alerts-and-dashboard.md, "Paging invariant").
		if h.Since != nil {
			if !observed.IsZero() {
				sent[channelKey("observed", key)] = observed
			}
			if !escalation.IsZero() {
				sent[channelKey("escalation", key)] = escalation
			}
		}
		if err := record(); err != nil {
			return err
		}
	}
	// An open outage is closed only by a completed login or a restored link
	// NEWER than its start, or by invalidating the owning adapter's process
	// evidence. Another adapter's events cannot recover its loss. Silence in the journals, or events older than the
	// outage that remain once the evidence of it has rotated away, prove
	// nothing; none is opened from them either.
	// A pending resolution of an earlier incident is not an open outage, but a
	// Since is: a new outage needs fresh evidence of recovery even while an old
	// resolution is pending.
	open := h.Since != nil || (deliveredAnywhere(seen, key) && !h.Resolving)
	// An equal time counts as newer: the journal's times are to the
	// microsecond, and a login in the same instant as the firing is a login.
	obsoleteLink := h.updateAdapterLosses(&ev, now)
	out = out || ev.out
	if !out && open && !obsoleteLink && (h.Since == nil || ev.recoveredAt.Before(*h.Since)) {
		out = true
		ev.connectivity = h.Notice == report.KindIBKRConnectivity
		said = "no completed login or restored link newer than the outage's start is in the journals (the evidence may have rotated away); still treated as unavailable. " + said
		if h.Since == nil {
			t := now
			h.Since = &t
		}
	}
	// Sampling faults have their own operational incident; they never prevent
	// the Gateway incident from routing (ADR 0019, "Alerting").
	units := make([]string, 0, len(ev.adapters))
	for unit := range ev.adapters {
		units = append(units, unit)
	}
	slices.Sort(units)
	for _, unit := range units {
		evidence := ev.adapters[unit]
		a := report.AdapterSampleFault(host, unit, "", now)
		if evidence.sampleErr != nil {
			a.Body += evidence.sampleErr.Error()
			retErr = errors.Join(retErr, evidence.sampleErr, deliver(senders, a, seen, sent, repeats, now, gatewayRemindEvery, record, policies...))
		} else {
			if deliveredAnywhere(seen, a.Key) {
				failed, resolveErr := resolveAll(senders, a.Key, seen, now)
				keep := map[string]bool{}
				for _, f := range setAside {
					keep[f.channel] = f.channel == "pagerduty"
				}
				for _, ch := range failed {
					keep[ch] = true
				}
				forgetAlert(a.Key, keep, seen, sent, repeats)
				retErr = errors.Join(retErr, resolveErr)
			}
			delete(sent, channelKey("observed", a.Key))
			delete(sent, channelKey("escalation", a.Key))
		}
	}

	if !out {
		// A Gateway that logged in again while a window held its outage is
		// handled at the window's close, not before. The bookkeeping of that
		// never stands between an outage and its page: a fault in it is
		// reported with the firing's errors and nothing else waits on it.
		if holdFor(report.KindGatewayLoggedOut, window).IsZero() {
			retErr = errors.Join(retErr, closeGatewayHold(unitState, host, true, now, note))
			h.Held = nil
		}
		h.Since, h.RestartOnly, h.Notice, h.Links = nil, false, "", nil
		delete(sent, channelKey("escalation", key))
		delete(sent, channelKey("observed", key))
		if !deliveredAnywhere(seen, key) {
			h.Resolving = false
			return errors.Join(retErr, record())
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
		return errors.Join(retErr, resolveErr, record())
	}
	if ev.out {
		h.Notice = ""
		if ev.connectivity {
			h.Notice = report.KindIBKRConnectivity
		}
		if ev.resetWait {
			h.Notice = report.KindGatewayResetWait
		}
	}
	if h.Since == nil {
		t := now
		h.Since = &t
		h.RestartOnly = !ev.out && restartOut
	}
	if sent[channelKey("observed", key)].IsZero() {
		sent[channelKey("observed", key)] = *h.Since
	}
	if sent[channelKey("escalation", key)].IsZero() {
		sent[channelKey("escalation", key)] = *h.Since
	}
	if err := record(); err != nil {
		return errors.Join(retErr, err)
	}
	// A maintenance window records the outage and pages nothing, reminders
	// included. It does not move the outage's start, so the ten minutes still
	// count from it: an outage already that old pages at the first firing after
	// the window closes, and a shorter one waits out the rest (restart windows
	// still count).
	if hold := holdFor(report.KindGatewayLoggedOut, window); !hold.IsZero() {
		// The shared hold is written first, then this file's: a stop between
		// leaves the shared hold, which the close always looks for.
		retErr = errors.Join(retErr, holdGateway(unitState, host, hold, now))
		h.Held = &hold
		return errors.Join(retErr, record())
	}
	// Past a window an outage not yet ten minutes old keeps its hold: if it
	// logs in before it pages, the window's hold is handled and said then.
	a := report.GatewayLoggedOutAlert(host, *h.Since, now, said)
	if ev.resetWait || ev.connectivity {
		a = report.IBKRNotice(host, now, ev.resetWait, said)
	}
	if !ev.resetWait && (sched.inWindow(now) || now.Sub(sched.graceFrom(*h.Since, now)) < gatewayPagesAfter) {
		if p := policyOf(policies); p != nil {
			_, _, _, policyErr := p.routeAndCancel(senders, a, seen, sent, now, gatewayRemindEvery)
			retErr = errors.Join(retErr, policyErr)
		}
		return errors.Join(retErr, record())
	}
	// It pages. The hold is forgotten only now, immediately before the page,
	// and only if one exists; the bookkeeping is bounded in time and its faults
	// are reported with the firing's errors, but never delay or stop the page.
	retErr = errors.Join(retErr, closeGatewayHold(unitState, host, false, now, note))
	h.Held = nil
	if h.Resolving {
		// The new outage is due its page while an earlier recovery's resolution
		// is still pending: the channel that never took that resolution holds a
		// receipt for the old incident and would stay silent, so the receipts go
		// now. Until here a recovery still retries the pending resolution.
		forgetAlert(key, map[string]bool{"observed": true, "escalation": true}, seen, sent, repeats)
		h.Resolving = false
	}
	err = deliver(senders, a, seen, sent, repeats, now, gatewayRemindEvery, record, policies...)
	return errors.Join(retErr, err, record())
}

// gatewayBookkeepingWait bounds the Gateway's wait for the shared record's
// lock, so a stalled holder never delays its page.
const gatewayBookkeepingWait = 10 * time.Second

// holdGateway records, in the unit-failure record the units' held failures are
// in, that the window ending at until held the Gateway's outage, so the close
// handles it as it handles a unit: paged if it is still out, otherwise marked
// held and recovered with one output line.
func holdGateway(path, host string, until, at time.Time) error {
	if path == "" {
		return nil
	}
	return withUnitStateWait(path, at, gatewayBookkeepingWait, func(u *unitState) error {
		holdAlert(u.sent, until, at, gatewayHeldKey(host))
		return u.record()
	})
}

// gatewayHeld reports whether the shared record holds the Gateway's outage. It
// reads the record without its lock (writes replace the file whole), so a
// Gateway that was never held waits on nothing.
func gatewayHeld(path, host string) (bool, error) {
	if path == "" {
		return false, nil
	}
	_, sent, _, _, err := loadState(path)
	if err != nil {
		return false, err
	}
	return len(heldOf(sent, gatewayHeldKey(host))) > 0, nil
}

// closeGatewayHold forgets the Gateway's held record, if the shared record has
// one; when the Gateway recovered, the line saying it was held and recovered
// is given to note. With no hold nothing is locked or written.
func closeGatewayHold(path, host string, recovered bool, at time.Time, note func(string)) error {
	if held, err := gatewayHeld(path, host); err != nil || !held {
		return err
	}
	return withUnitStateWait(path, at, gatewayBookkeepingWait, func(u *unitState) error {
		taken := takeHeld(u.sent, gatewayHeldKey(host), time.Time{})
		if len(taken) == 0 {
			return nil
		}
		if recovered && note != nil {
			for _, h := range taken {
				note(recoveredLine(gatewayName, h.until))
			}
		}
		return u.record()
	})
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
