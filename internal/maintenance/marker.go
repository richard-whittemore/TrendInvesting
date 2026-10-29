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
	"regexp"
	"strconv"
	"strings"
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
	// ID names this window, so that the program that opened it can end it
	// and no other (ti-maintenance end --id).
	ID         string
	Opened     time.Time
	Until      time.Time
	OpenedUnix int64
	UntilUnix  int64
}

// The one layout ti-maintenance start writes, byte for byte: lower-case keys
// in this order, no spaces, UTC times to the second ending in Z, the same
// times again as seconds since the epoch, one object and one newline. Both
// readers (this one and deploy/maintenance.sh) accept only this layout and
// read anything else as no window, so a hand-edited or damaged file can never
// be open to one and closed to the other.
var layout = regexp.MustCompile(`^\{"id":"([0-9a-f]{16})","opened":"([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})Z","until":"([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})Z","opened_unix":([0-9]{1,12}),"until_unix":([0-9]{1,12})\}$`)

// Verdict is what a marker file says at a moment.
type Verdict int

const (
	// Open: a window is open.
	Open Verdict = iota
	// Closed: no window is open: there is no file, or it is a well-formed
	// window that has ended or has not begun.
	Closed
	// Invalid: the file is there and is not a marker this layout allows, or
	// one that cannot be trusted (over the maximum, ends before it begins,
	// times that disagree). It reads as no window; the caller says so.
	Invalid
)

// Judge judges the marker file's content at now. Nothing is removed by any
// reader: an expired marker reads Closed whoever sees it, and the next start
// replaces it, so no reader ever deletes a file that a start may be replacing.
func Judge(raw []byte, now time.Time) (Marker, Verdict) {
	body := strings.TrimRight(string(raw), "\n")
	if len(raw) == 0 {
		return Marker{}, Closed
	}
	f := layout.FindStringSubmatch(body)
	if f == nil || strings.Count(string(raw), "\n") > 1 {
		return Marker{}, Invalid
	}
	opened, err1 := time.ParseInLocation("2006-01-02T15:04:05", f[2], time.UTC)
	until, err2 := time.ParseInLocation("2006-01-02T15:04:05", f[3], time.UTC)
	ou, err3 := strconv.ParseInt(f[4], 10, 64)
	uu, err4 := strconv.ParseInt(f[5], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || ou != opened.Unix() || uu != until.Unix() {
		return Marker{}, Invalid
	}
	if until.Before(opened) || until.Sub(opened) > Max {
		return Marker{}, Invalid
	}
	m := Marker{ID: f[1], Opened: opened, Until: until, OpenedUnix: ou, UntilUnix: uu}
	if now.Before(opened) || !now.Before(until) {
		return m, Closed
	}
	return m, Open
}

// Read reports whether a window is open at now, and which.
func Read(raw []byte, now time.Time) (Marker, bool) {
	m, v := Judge(raw, now)
	return m, v == Open
}
