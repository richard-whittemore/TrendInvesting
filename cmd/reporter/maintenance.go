package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/maintenance"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// A maintenance window (internal/maintenance) holds back the alerts that
// report.HeldDuringMaintenance names: the reporter and the OnFailure hook
// RECORD each in their state files with the window noted, deliver nothing for
// it, and judge it again at the first firing after the window closes, so
// whatever is still wrong pages then through the normal paths and whatever
// recovered resolves quietly (ADR 0019, "Alerting"; the heartbeat and the
// daily summary are untouched).

// heldPrefix begins the channel part of a held record's key in a state
// file's sent times: held:<window's end>|<alert key>, valued at when it was
// held. The window's end is the note of which window held it.
const heldPrefix = "held:"

func heldKey(until time.Time, alertKey string) string {
	return channelKey(heldPrefix+until.UTC().Format(time.RFC3339), alertKey)
}

// holdAlert records that the window ending at until held the alert keyed
// alertKey at at.
func holdAlert(sent map[string]time.Time, until, at time.Time, alertKey string) {
	sent[heldKey(until, alertKey)] = at
}

// maintenanceFileDefault is where ti-maintenance writes the marker
// (deploy/maintenance.sh): a folder only root can write, which the reporter
// and the hook only read. A test replaces it.
var maintenanceFileDefault = "/run/ti-maintenance/maintenance.json"

// maintenancePath is the marker's path: -maintenance, else the default.
func maintenancePath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return maintenanceFileDefault
}

// windowRead is when the window open at at ends (the zero time when none is
// open), and, when the marker is there but is not a window the reader can
// trust (unreadable, or not the one layout), what is wrong with it and a
// fingerprint of that content, so that it is said once. Nothing is removed:
// an expired marker already reads closed, and a start that replaces it must
// never be undone by a reader that read the old one.
func windowRead(path string, at time.Time) (until time.Time, problem, fingerprint string) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, "", ""
	}
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		sum := sha256.Sum256([]byte("unreadable:" + err.Error()))
		return time.Time{}, "cannot be read (" + err.Error() + ")", hex.EncodeToString(sum[:6])
	}
	m, verdict := maintenance.Judge(raw, at)
	switch verdict {
	case maintenance.Open:
		return m.Until, "", ""
	case maintenance.Invalid:
		sum := sha256.Sum256(raw)
		return time.Time{}, "is not a window ti-maintenance wrote (wrong layout, times that disagree, or longer than 30 minutes)", hex.EncodeToString(sum[:6])
	}
	return time.Time{}, "", ""
}

// windowEnd is when the window open at at ends, or the zero time.
func windowEnd(path string, at time.Time) time.Time {
	until, _, _ := windowRead(path, at)
	return until
}

// holdFor is the end of the window that holds an alert of kind, asked of
// window at this moment, or the zero time when the alert is not held. It is
// the one place a hold is decided: only the kinds report.HeldDuringMaintenance
// names are held, never a capital-safety kind, and only while the window is
// open now.
func holdFor(kind report.Kind, window func() time.Time) time.Time {
	if window == nil || !report.HeldDuringMaintenance(report.Alert{Kind: kind}) {
		return time.Time{}
	}
	return window()
}

// heldDuring reports whether the window holds alert a now.
func heldDuring(a report.Alert, window func() time.Time) bool {
	return !holdFor(a.Kind, window).IsZero()
}

// held is one held record: the alert key, when it was held and the end of the
// window that held it.
type held struct {
	alertKey  string
	at, until time.Time
}

// takeHeld forgets the held records whose alert key begins with prefix and
// that were recorded before cutoff (all of them for the zero time), and
// returns them. A record made after cutoff is newer than whatever the caller
// knows and stays.
func takeHeld(sent map[string]time.Time, prefix string, cutoff time.Time) (taken []held) {
	for k, at := range sent {
		ch, alert, ok := strings.Cut(k, "|")
		if !ok || !strings.HasPrefix(ch, heldPrefix) || !strings.HasPrefix(alert, prefix) {
			continue
		}
		if !cutoff.IsZero() && !at.Before(cutoff) {
			continue
		}
		until, _ := time.Parse(time.RFC3339, strings.TrimPrefix(ch, heldPrefix))
		taken = append(taken, held{alertKey: alert, at: at, until: until})
		delete(sent, k)
	}
	return taken
}

// heldOf lists, without forgetting, the held records whose alert key begins
// with prefix.
func heldOf(sent map[string]time.Time, prefix string) (found []held) {
	for k, at := range sent {
		if ch, alert, ok := strings.Cut(k, "|"); ok && strings.HasPrefix(ch, heldPrefix) && strings.HasPrefix(alert, prefix) {
			found = append(found, held{alertKey: alert, at: at})
		}
	}
	return found
}

// What a held unit came to, as the close summary will say it.
const (
	closingRecovered = "recovered"
	closingPending   = "pending"
	closingPaged     = "paged"
	closingRetrying  = "retrying"
)

// closingPrefix begins the channel part of a closing record's key in the unit
// state's sent times: closing:<status>:<window's end>|<unit>. A closing record
// is a close summary that is owed: it is written in the same save as the
// removal of the held record it replaces and is removed only once the summary
// has been delivered, so a summary that could not be sent is sent at a later
// firing (ADR 0019, "Alerting": delivery state is durable).
const closingPrefix = "closing:"

func closingKey(status string, until time.Time, unit string) string {
	return channelKey(closingPrefix+status+":"+until.UTC().Format(time.RFC3339), unit)
}

// setClosing records that the window ending at until held unit and that it
// came to status, replacing any earlier status for the same window.
func setClosing(u *unitState, unit, status string, until, at time.Time) {
	for _, c := range closingOf(u.sent) {
		if c.unit == unit && c.until.Equal(until) {
			delete(u.sent, closingKey(c.status, c.until, c.unit))
		}
	}
	u.sent[closingKey(status, until, unit)] = at
}

type closing struct {
	unit, status string
	until        time.Time
}

// closingOf lists the closing records.
func closingOf(sent map[string]time.Time) (found []closing) {
	for k := range sent {
		ch, unit, ok := strings.Cut(k, "|")
		rest, isClosing := strings.CutPrefix(ch, closingPrefix)
		if !ok || !isClosing {
			continue
		}
		status, untilText, _ := strings.Cut(rest, ":")
		until, err := time.Parse(time.RFC3339, untilText)
		if err != nil {
			continue
		}
		found = append(found, closing{unit: unit, status: status, until: until})
	}
	return found
}

// closingText is how the summary says what a held unit came to.
func closingText(c closing) string {
	switch c.status {
	case closingRecovered:
		return c.unit + " recovered"
	case closingPaged:
		return c.unit + " still failed (paged)"
	case closingRetrying:
		return c.unit + " still failed (page failed, retrying)"
	}
	return c.unit + " still failed (page pending)"
}

// summaryTakerAmong reports whether any sender takes the routine summary.
func summaryTakerAmong(senders []sender) bool {
	for _, s := range senders {
		if t, ok := s.(summaryTaker); ok && t.takesSummary() {
			return true
		}
	}
	return false
}

// flushClosing sends, for each window that has closed, the one routine message
// that says what it held: "maintenance window closed; held: <unit> recovered,
// <unit> still failed (paged)". It goes only where the daily summary goes (the
// owner's own ntfy server: deliver restricts kindSummary to such a channel),
// never to an urgent channel or the issue tracker. A message that could not be
// delivered leaves its closing records, and is sent at the next firing; one
// that has been delivered, or has nowhere to go, leaves nothing behind. A page
// is called "paged" only when it was delivered.
func flushClosing(u *unitState, senders []sender, at time.Time, every time.Duration) error {
	byWindow := map[string][]closing{}
	for _, c := range closingOf(u.sent) {
		w := c.until.UTC().Format(time.RFC3339)
		byWindow[w] = append(byWindow[w], c)
	}
	windows := make([]string, 0, len(byWindow))
	for w := range byWindow {
		windows = append(windows, w)
	}
	sort.Strings(windows)
	var errs []error
	for _, w := range windows {
		recs := byWindow[w]
		sort.Slice(recs, func(i, j int) bool { return recs[i].unit < recs[j].unit })
		var parts []string
		for _, c := range recs {
			parts = append(parts, closingText(c))
		}
		body := "maintenance window closed; held: " + strings.Join(parts, ", ")
		key := "maintenance-closed:" + w + ":" + shortHash(body)
		if summaryTakerAmong(senders) {
			a := report.Alert{Key: key, Kind: kindSummary, Severity: report.Routine, AccountID: hostName(), At: at, Title: hostName() + ": maintenance window closed", Body: body, PublicBody: body}
			if err := deliver(senders, a, u.seen, u.sent, u.repeats, at, every, u.record); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		for _, c := range recs {
			delete(u.sent, closingKey(c.status, c.until, c.unit))
		}
		forgetAlert(key, nil, u.seen, u.sent, u.repeats)
		errs = append(errs, u.record())
	}
	return errors.Join(errs...)
}

// shortHash names a summary's content in its key, so a changed outcome is a
// new message.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}
