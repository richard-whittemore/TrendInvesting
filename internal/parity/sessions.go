package parity

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/nysecalendar"
)

// Sessions lists the trading sessions after one date and through another
// (YYYY-MM-DD), oldest first, or fails with ErrNoSessions when it does not
// cover them. The IRA sequence counts the sessions the buys waited by it,
// never by what the export says it waited.
type Sessions func(after, through string) ([]string, error)

// ErrNoSessions is a span no session calendar covers.
var ErrNoSessions = errors.New("parity: no session calendar covers the span")

func day(s string) (time.Time, error) {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parity: %q is not a YYYY-MM-DD date", s)
	}
	return d, nil
}

// span parses a calendar's arguments; empty is a span of no session.
func span(after, through string) (a, t time.Time, empty bool, err error) {
	if a, err = day(after); err != nil {
		return
	}
	if t, err = day(through); err != nil {
		return
	}
	return a, t, !t.After(a), nil
}

// SessionList is the calendar of dates: every session from the first date
// to the last, strictly increasing. It covers any span after the day
// before the first date and through the last. Where internal/nysecalendar covers a
// year the list must agree with it day for day, so that a list drawn from
// a backtest's own history is checked against the exchange's rules
// wherever they are written down.
func SessionList(dates []string) (Sessions, error) {
	if len(dates) == 0 {
		return nil, errors.New("parity: an empty session list")
	}
	listed := make(map[string]bool, len(dates))
	for i, s := range dates {
		if _, err := day(s); err != nil {
			return nil, err
		}
		if i > 0 && s <= dates[i-1] {
			return nil, fmt.Errorf("parity: session %s does not follow %s", s, dates[i-1])
		}
		listed[s] = true
	}
	first, _ := day(dates[0])
	last, _ := day(dates[len(dates)-1])
	// A span may start on the day before the first date: no day of it then
	// lies outside the list, so the first date can itself be counted.
	before := first.AddDate(0, 0, -1).Format(time.DateOnly)
	for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
		trading, err := nysecalendar.IsTradingDay(d)
		if err == nil && trading != listed[d.Format(time.DateOnly)] {
			return nil, fmt.Errorf("parity: the session list and the NYSE calendar disagree on %s", d.Format(time.DateOnly))
		}
	}
	return func(after, through string) ([]string, error) {
		_, _, empty, err := span(after, through)
		switch {
		case err != nil:
			return nil, err
		case empty:
			return nil, nil
		case after < before || through > dates[len(dates)-1]:
			return nil, fmt.Errorf("%w: (%s, %s] beyond the list's %s to %s", ErrNoSessions, after, through, dates[0], dates[len(dates)-1])
		}
		i := sort.SearchStrings(dates, after)
		if i < len(dates) && dates[i] == after {
			i++
		}
		j := sort.SearchStrings(dates, through)
		if j < len(dates) && dates[j] == through {
			j++
		}
		return append([]string(nil), dates[i:j]...), nil
	}, nil
}

// NYSESessions is internal/nysecalendar's calendar, in the years it covers.
func NYSESessions(after, through string) ([]string, error) {
	a, t, empty, err := span(after, through)
	if err != nil || empty {
		return nil, err
	}
	var out []string
	for d := a.AddDate(0, 0, 1); !d.After(t); d = d.AddDate(0, 0, 1) {
		trading, err := nysecalendar.IsTradingDay(d)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrNoSessions, err)
		}
		if trading {
			out = append(out, d.Format(time.DateOnly))
		}
	}
	return out, nil
}

// Either is the first of cals that covers a span.
func Either(cals ...Sessions) Sessions {
	return func(after, through string) ([]string, error) {
		for _, c := range cals {
			out, err := c(after, through)
			if !errors.Is(err, ErrNoSessions) {
				return out, err
			}
		}
		return nil, fmt.Errorf("%w: (%s, %s]", ErrNoSessions, after, through)
	}
}
