package main

import (
	"os"
	"path/filepath"
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

// dropHeld forgets every held record whose alert key begins with prefix (all
// of them for ""), and says whether it forgot any.
func dropHeld(sent map[string]time.Time, prefix string) bool {
	dropped := false
	for k := range sent {
		ch, alert, ok := strings.Cut(k, "|")
		if ok && strings.HasPrefix(ch, heldPrefix) && strings.HasPrefix(alert, prefix) {
			delete(sent, k)
			dropped = true
		}
	}
	return dropped
}

// maintenancePath is the marker's path: -maintenance, else maintenance.json
// beside the state file, in the reports directory the reporter and the hook
// already read and write (deploy/maintenance.sh).
func maintenancePath(flagValue, statePath string) string {
	if flagValue != "" {
		return flagValue
	}
	return filepath.Join(filepath.Dir(statePath), "maintenance.json")
}

// windowEnd is when the window open at at ends, or the zero time when none is
// open. An expired marker is removed by the read.
func windowEnd(path string, at time.Time) time.Time {
	raw, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	m, open, expired := maintenance.Read(raw, at)
	if expired {
		_ = os.Remove(path)
	}
	if open {
		return m.Until
	}
	return time.Time{}
}

// splitHeld separates the alerts a window holds, recording each, from those
// delivered as always.
func splitHeld(alerts []report.Alert, sent map[string]time.Time, until, at time.Time) (deliverable []report.Alert) {
	for _, a := range alerts {
		if report.HeldDuringMaintenance(a) {
			holdAlert(sent, until, at, a.Key)
			continue
		}
		deliverable = append(deliverable, a)
	}
	return deliverable
}
