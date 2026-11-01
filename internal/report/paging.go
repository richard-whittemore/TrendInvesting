package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/nysecalendar"
)

// Class separates capital safety from operations (alerts-and-dashboard.md,
// "Paging invariant"). Unknown kinds retain the capital-safety route.
type Class string

const (
	CapitalSafety Class = "capital-safety"
	Operational   Class = "operational"
)

// Class classifies every alert independently of its severity. Trading-unit
// failures become capital-safety near a session (alerts-and-dashboard.md).
func (a Alert) Class(near bool) Class {
	switch a.Kind {
	case KindUnitFailed:
		if near && (strings.HasPrefix(a.Key, "unit-failed:ti-engine@") || strings.HasPrefix(a.Key, "unit-failed:ti-adapter") || strings.HasPrefix(a.Key, "unit-failed:ti-gateway.")) {
			return CapitalSafety
		}
		return Operational
	case KindGatewayLoggedOut, KindGatewayResetWait, KindHistoryUnavailable, KindRestoredLogNewer:
		return Operational
	default:
		return CapitalSafety
	}
}

// SessionNear implements the New York clock rule in alerts-and-dashboard.md,
// "Paging invariant". Calendar errors are returned, never treated as quiet.
func SessionNear(now time.Time) (bool, error) {
	t := now.In(nysecalendar.NewYork)
	mins := t.Hour()*60 + t.Minute()
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return false, nil
	}
	if mins < 330 || mins >= 960 {
		return false, nil
	}
	return nysecalendar.IsTradingDay(t)
}

type ibkrWindow struct {
	days       map[time.Weekday]bool
	start, end int
	loc        *time.Location
}

// IBKRSchedule holds the adapter's recurring windows (ADR 0025, scheduled
// interruptions). It has no clock or filesystem dependency.
type IBKRSchedule struct{ windows []ibkrWindow }
type ibkrInterval struct{ start, end time.Time }

// ParseIBKRSchedule accepts exactly the maintenance.py schema, including
// overnight windows and 24:00 ends. A schedule must leave a weekly gap.
func ParseIBKRSchedule(raw []byte) (IBKRSchedule, error) {
	var out IBKRSchedule
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := uniqueJSON(dec, 0); err != nil {
		return out, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return out, errors.New("maintenance schedule: trailing JSON")
	}
	var input struct {
		Windows *[]struct {
			Days  []string `json:"days"`
			Start string   `json:"start"`
			End   string   `json:"end"`
			Zone  string   `json:"zone"`
		} `json:"windows"`
	}
	dec = json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		return out, err
	}
	if input.Windows == nil {
		return out, errors.New("maintenance schedule requires a windows array")
	}
	names := map[string]time.Weekday{"Sun": time.Sunday, "Mon": time.Monday, "Tue": time.Tuesday, "Wed": time.Wednesday, "Thu": time.Thursday, "Fri": time.Friday, "Sat": time.Saturday}
	for _, row := range *input.Windows {
		w := ibkrWindow{days: map[time.Weekday]bool{}}
		if len(row.Days) == 1 && row.Days[0] == "daily" {
			for d := time.Sunday; d <= time.Saturday; d++ {
				w.days[d] = true
			}
		} else {
			for _, name := range row.Days {
				d, ok := names[name]
				if !ok || w.days[d] {
					return out, errors.New("maintenance days must be daily or distinct Mon through Sun")
				}
				w.days[d] = true
			}
		}
		if len(w.days) == 0 {
			return out, errors.New("maintenance days must not be empty")
		}
		var err error
		if w.start, err = clockMinutes(row.Start, false); err != nil {
			return out, err
		}
		if w.end, err = clockMinutes(row.End, true); err != nil {
			return out, err
		}
		if w.start == w.end {
			return out, errors.New("maintenance start and end must differ")
		}
		if row.Zone == "" || row.Zone == "Local" {
			return out, errors.New("maintenance zone must name an IANA zone")
		}
		if w.loc, err = time.LoadLocation(row.Zone); err != nil {
			return out, err
		}
		out.windows = append(out.windows, w)
	}
	// A representative leap year includes every weekday and both DST changes;
	// validation is deterministic and never depends on the host clock.
	for d := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC); d.Year() == 2024; d = d.AddDate(0, 0, 1) {
		for _, v := range out.intervals(d) {
			if v.end.Sub(v.start) > 8*24*time.Hour {
				return IBKRSchedule{}, errors.New("maintenance windows must leave a weekly gap and finite end")
			}
		}
	}
	return out, nil
}

func clockMinutes(s string, end bool) (int, error) {
	if end && s == "24:00" {
		return 1440, nil
	}
	t, err := time.Parse("15:04", s)
	if err != nil || len(s) != 5 {
		return 0, errors.New("maintenance time must be HH:MM; only end allows 24:00")
	}
	return t.Hour()*60 + t.Minute(), nil
}

// uniqueJSON refuses duplicate and non-schema field names instead of accepting a different
// schedule from the adapter (ADR 0025, scheduled interruptions).
func uniqueJSON(d *json.Decoder, depth int) error {
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	fields := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || fields[name] {
				return errors.New("maintenance schedule: duplicate field")
			}
			knownField := depth == 0 && name == "windows" || depth == 2 && (name == "days" || name == "start" || name == "end" || name == "zone")
			if !knownField {
				return fmt.Errorf("maintenance schedule: unknown field %q", name)
			}
			fields[name] = true
		}
		if err := uniqueJSON(d, depth+1); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

// localBoundary matches maintenance.py: earliest repeated start, latest
// repeated end, and a nonexistent boundary rolls forward over the DST gap.
func localBoundary(day time.Time, minutes int, ending bool, loc *time.Location) time.Time {
	civil := time.Date(day.Year(), day.Month(), day.Day(), minutes/60, minutes%60, 0, 0, time.UTC)
	offsets := map[int]bool{}
	for _, h := range []int{-36, 0, 36} {
		_, off := civil.Add(time.Duration(h) * time.Hour).In(loc).Zone()
		offsets[off] = true
	}
	var valid []time.Time
	var latest time.Time
	for off := range offsets {
		v := civil.Add(-time.Duration(off) * time.Second)
		if v.After(latest) {
			latest = v
		}
		local := v.In(loc)
		if local.Format("2006-01-02 15:04") == civil.Format("2006-01-02 15:04") {
			valid = append(valid, v)
		}
	}
	if len(valid) == 0 {
		return latest
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].Before(valid[j]) })
	if ending {
		return valid[len(valid)-1]
	}
	return valid[0]
}
func (s IBKRSchedule) intervals(now time.Time) []ibkrInterval {
	var all []ibkrInterval
	for _, w := range s.windows {
		local := now.In(w.loc)
		day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
		for offset := -8; offset <= 8; offset++ {
			d := day.AddDate(0, 0, offset)
			if !w.days[d.Weekday()] {
				continue
			}
			endDay := d
			end := w.end
			if end <= w.start {
				endDay = endDay.AddDate(0, 0, 1)
			}
			all = append(all, ibkrInterval{localBoundary(d, w.start, false, w.loc), localBoundary(endDay, end, true, w.loc)})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].start.Before(all[j].start) })
	var merged []ibkrInterval
	for _, v := range all {
		if len(merged) > 0 && !v.start.After(merged[len(merged)-1].end) {
			last := &merged[len(merged)-1]
			if v.end.After(last.end) {
				last.end = v.end
			}
		} else {
			merged = append(merged, v)
		}
	}
	return merged
}

// WindowEnd returns the exclusive end of the union containing now, or zero.
func (s IBKRSchedule) WindowEnd(now time.Time) time.Time {
	for _, v := range s.intervals(now) {
		if !now.Before(v.start) && now.Before(v.end) {
			return v.end
		}
	}
	return time.Time{}
}

// KindGatewayResetWait distinguishes a reset wait from a logout (ADR 0019, "Alerting").
const KindGatewayResetWait Kind = "gateway-reset-wait"

// GatewayResetWaitAlert reports a current reset wait (ADR 0019, "Alerting";
// server.md, "The Gateway is logged out"). Callers must establish current
// journal evidence on the wait's own night; it shares GatewayKey(host).
func GatewayResetWaitAlert(host string, now time.Time, said string) Alert {
	return Alert{Key: GatewayKey(host), Kind: KindGatewayResetWait, Severity: Urgent, AccountID: host, At: now, Repeat: true,
		Title: fmt.Sprintf("%s: IB Gateway reset wait", host),
		Body:  "The Gateway start program is waiting out IBKR's reset hour after a refused login; a refusal after 01:00 ET pages as a logout. " + said}
}

// ClosedSince returns the latest scheduled end after from and no later than
// now. Even missed firings restart escalation at that end, per
// alerts-and-dashboard.md's Paging invariant. Recurrence is weekly, so the
// surrounding intervals contain the latest end however old the incident is.
func (s IBKRSchedule) ClosedSince(from, now time.Time) time.Time {
	var latest time.Time
	for _, interval := range s.intervals(now) {
		if interval.end.After(from) && !interval.end.After(now) && interval.end.After(latest) {
			latest = interval.end
		}
	}
	return latest
}
