// Package maintenance reads the host's maintenance window: the bounded quiet
// period in which an install's own noise (a unit that fails while its program
// is replaced) pages no one, and in which everything is still recorded, and
// pages at the first firing after the window closes (ADR 0019, "Alerting": a
// fault that persists is not left silent).
//
// The marker is written by deploy/maintenance.sh (ti-maintenance), read here
// by the reporter and the OnFailure hook. A window is at most Max long and
// carries its own expiry, so it can never outlive it: a marker past its
// expiry reads as closed whether or not anyone removed it. A marker that
// cannot be trusted reads as closed, so a fault in the file never silences
// an alert.
package maintenance

import (
	"encoding/json"
	"time"
)

// Max is the longest a window may be, and Default the length of one opened
// without a duration.
const (
	Max     = 30 * time.Minute
	Default = 15 * time.Minute
)

// Marker is the file's content.
type Marker struct {
	Opened time.Time `json:"opened"`
	Until  time.Time `json:"until"`
}

// Read judges the marker file's content at now. The window is open when the
// content parses, spans no more than Max, and now is within it. A marker with
// no times, one that spans more than Max or ends before it opens, and one
// opened in the future read closed, and so does an empty or missing file
// (nil content). expired is true for a marker at or past its expiry, which
// the caller removes (best effort: its being there changes nothing).
func Read(raw []byte, now time.Time) (m Marker, open, expired bool) {
	if err := json.Unmarshal(raw, &m); err != nil || m.Opened.IsZero() || m.Until.IsZero() {
		return Marker{}, false, false
	}
	if !now.Before(m.Until) {
		return Marker{}, false, true
	}
	if m.Until.Before(m.Opened) || m.Until.Sub(m.Opened) > Max {
		return Marker{}, false, false
	}
	if now.Before(m.Opened) {
		return Marker{}, false, false
	}
	return m, true, false
}
