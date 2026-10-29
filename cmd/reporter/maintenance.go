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

// gatewayName names the Gateway among the held conditions, and
// gatewayHeldPrefix begins its held record's alert key (gatewayHeldKey): the
// Gateway is held by the same record as a unit, in the unit-failure record.
const (
	gatewayName       = "IB Gateway"
	gatewayHeldPrefix = "gateway-out:"
)

func gatewayHeldKey(host string) string { return gatewayHeldPrefix + host }

// A held condition ends in one of two ways at the first firing after the
// window closes. If it is still wrong it is paged by the ordinary path (the
// page is never gated by any of this bookkeeping). If it recovered it is
// marked handled and recorded here as held and recovered: the firing says so
// in one output line, and the next daily summary lists it, read from these
// records and removed once that summary has been delivered.
const recoveredHeldPrefix = "recovered-held:"

// maxRecoveredHeld bounds the records kept for a daily summary that is never
// sent.
const maxRecoveredHeld = 50

// recordRecoveredHeld records that the window ending at until held name and
// that it recovered, and returns the output line that says so.
func recordRecoveredHeld(u *unitState, name string, until, at time.Time) string {
	w := until.UTC().Format(time.RFC3339)
	u.sent[channelKey(recoveredHeldPrefix+w, name)] = at
	if len(recoveredHeldIn(u.sent)) > maxRecoveredHeld {
		all := recoveredHeldIn(u.sent)
		sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
		for _, r := range all[:len(all)-maxRecoveredHeld] {
			delete(u.sent, r.key)
		}
	}
	return "held during maintenance and recovered: " + name + " (" + w + ")"
}

type recoveredHeld struct {
	key, name, window string
	at                time.Time
}

func recoveredHeldIn(sent map[string]time.Time) (found []recoveredHeld) {
	for k, at := range sent {
		ch, name, ok := strings.Cut(k, "|")
		w, isRecovered := strings.CutPrefix(ch, recoveredHeldPrefix)
		if ok && isRecovered {
			found = append(found, recoveredHeld{key: k, name: name, window: w, at: at})
		}
	}
	return found
}

// recoveredHeldLine is the daily summary's line listing what was held and
// recovered since the previous daily summary, and the records it lists; empty
// when there are none or the record cannot be read (a summary never waits for
// it).
func recoveredHeldLine(path string) (line string, keys []string) {
	if path == "" {
		return "", nil
	}
	_, sent, _, _, err := loadState(path)
	if err != nil {
		return "", nil
	}
	found := recoveredHeldIn(sent)
	sort.Slice(found, func(i, j int) bool { return found[i].window+found[i].name < found[j].window+found[j].name })
	var parts []string
	for _, r := range found {
		parts = append(parts, r.name+" ("+r.window+")")
		keys = append(keys, r.key)
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "Held during maintenance and recovered since the last daily summary: " + strings.Join(parts, ", ") + "\n", keys
}

// forgetRecoveredHeld removes the records a delivered daily summary listed.
func forgetRecoveredHeld(path string, keys []string, at time.Time) error {
	if path == "" || len(keys) == 0 {
		return nil
	}
	return withUnitState(path, at, func(u *unitState) error {
		for _, k := range keys {
			delete(u.sent, k)
		}
		return u.record()
	})
}
