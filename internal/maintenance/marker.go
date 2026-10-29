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

// Marker is the file's content: the window's two ends as times and as
// seconds since the epoch, which must agree, so that the shell script that
// writes it and the Go reader that judges it cannot read one file two ways.
type Marker struct {
	Opened     time.Time `json:"opened"`
	Until      time.Time `json:"until"`
	OpenedUnix int64     `json:"opened_unix"`
	UntilUnix  int64     `json:"until_unix"`
}

// Read judges the marker file's content at now. The window is open when the
// content parses, both ends are given in both forms and agree, it spans no
// more than Max, and now is from its opening to before its expiry. Anything
// else, including an empty or missing file (nil content), reads closed. An
// expired marker is left where it is: it reads closed for whoever reads it,
// and the next start replaces it, so no reader ever removes a file that a
// start may be replacing.
func Read(raw []byte, now time.Time) (Marker, bool) {
	var m Marker
	if err := json.Unmarshal(raw, &m); err != nil || m.Opened.IsZero() || m.Until.IsZero() {
		return Marker{}, false
	}
	if m.OpenedUnix != m.Opened.Unix() || m.UntilUnix != m.Until.Unix() {
		return Marker{}, false
	}
	if m.Until.Before(m.Opened) || m.Until.Sub(m.Opened) > Max || now.Before(m.Opened) || !now.Before(m.Until) {
		return Marker{}, false
	}
	return m, true
}
