// Package nysecalendar is the NYSE trading calendar: which days are sessions
// and when each closes. It is the Go reading of
// adapter/marketdata/nyse_calendar.py, which dates the sessions the engine
// records, and follows the same published rules (NYSE "Holidays & Trading
// Hours"); calendar_test.go checks the same days that file's tests do, so the
// two cannot drift apart unnoticed.
//
// Full-day closures: New Year's Day, Martin Luther King Jr. Day (3rd Monday
// of January), Washington's Birthday (3rd Monday of February), Good Friday,
// Memorial Day (last Monday of May), Juneteenth (June 19, from 2022),
// Independence Day, Labor Day (1st Monday of September), Thanksgiving (4th
// Thursday of November), Christmas. A holiday on a Saturday is observed the
// Friday before and on a Sunday the Monday after, except that New Year's Day
// on a Saturday is not observed on the Friday. Sessions close at 4 p.m. New
// York time; early closes are at 1 p.m.: the day after Thanksgiving, and
// July 3 and December 24 when they fall Monday to Thursday. Special closures
// are listed in special. Years outside firstYear..lastYear are refused with
// ErrOutOfRange, never guessed at; extend both calendars together.
package nysecalendar

import (
	"errors"
	"fmt"
	"time"
	// The zone database is built in, so a host without one cannot stop the
	// calendar loading.
	_ "time/tzdata"
)

const firstYear, lastYear = 2022, 2030

// special lists one-off closures: 2025-01-09, the National Day of Mourning
// for President Carter.
var special = map[string]bool{"2025-01-09": true}

// NewYork is the exchange's time zone, in which sessions are dated.
var NewYork = mustLoad("America/New_York")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(fmt.Sprintf("nysecalendar: load %s: %v", name, err))
	}
	return loc
}

// ErrOutOfRange is returned for a day outside the years the calendar covers.
var ErrOutOfRange = errors.New("nysecalendar: no NYSE calendar for that year")

func civil(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func nthWeekday(year int, month time.Month, weekday time.Weekday, n int) time.Time {
	first := civil(year, month, 1)
	offset := (int(weekday) - int(first.Weekday()) + 7) % 7
	return first.AddDate(0, 0, offset+7*(n-1))
}

func lastMonday(year int, month time.Month) time.Time {
	last := civil(year, month+1, 0)
	return last.AddDate(0, 0, -((int(last.Weekday()) + 6) % 7))
}

// easter is the Gregorian Easter Sunday (the anonymous algorithm, as the
// Python calendar computes it).
func easter(year int) time.Time {
	a, b, c := year%19, year/100, year%100
	d, e := b/4, b%4
	g := (8*b + 13) / 25
	h := (19*a + b - d - g + 15) % 30
	j, k := c/4, c%4
	m := (a + 11*h) / 319
	r := (2*e + 2*j - k - h + m + 32) % 7
	month := (h - m + r + 90) / 25
	return civil(year, time.Month(month), (h-m+r+month+19)%32)
}

func observed(day time.Time) time.Time {
	switch day.Weekday() {
	case time.Saturday:
		return day.AddDate(0, 0, -1)
	case time.Sunday:
		return day.AddDate(0, 0, 1)
	}
	return day
}

func key(day time.Time) string { return day.Format(time.DateOnly) }

func holidays(year int) map[string]bool {
	out := map[string]bool{}
	for _, d := range []time.Time{
		nthWeekday(year, time.January, time.Monday, 3), nthWeekday(year, time.February, time.Monday, 3),
		easter(year).AddDate(0, 0, -2), lastMonday(year, time.May),
		nthWeekday(year, time.September, time.Monday, 1), nthWeekday(year, time.November, time.Thursday, 4),
		observed(civil(year, time.July, 4)), observed(civil(year, time.December, 25)), observed(civil(year, time.June, 19)),
	} {
		out[key(d)] = true
	}
	if civil(year, time.January, 1).Weekday() != time.Saturday {
		out[key(observed(civil(year, time.January, 1)))] = true
	}
	for d := range special {
		if d[:4] == fmt.Sprint(year) {
			out[d] = true
		}
	}
	return out
}

func earlyClose(day time.Time) bool {
	if key(day) == key(nthWeekday(day.Year(), time.November, time.Thursday, 4).AddDate(0, 0, 1)) {
		return true
	}
	for _, md := range []struct {
		m time.Month
		d int
	}{{time.July, 3}, {time.December, 24}} {
		d := civil(day.Year(), md.m, md.d)
		if key(d) == key(day) && d.Weekday() >= time.Monday && d.Weekday() <= time.Thursday {
			return true
		}
	}
	return false
}

// dateOf is day's calendar date, read in its own location.
func dateOf(day time.Time) time.Time { return civil(day.Year(), day.Month(), day.Day()) }

// IsTradingDay reports whether the calendar date of day is an NYSE session.
func IsTradingDay(day time.Time) (bool, error) {
	d := dateOf(day)
	if d.Year() < firstYear || d.Year() > lastYear {
		return false, fmt.Errorf("%w: %d (it covers %d to %d)", ErrOutOfRange, d.Year(), firstYear, lastYear)
	}
	return d.Weekday() != time.Saturday && d.Weekday() != time.Sunday && !holidays(d.Year())[key(d)], nil
}

// SessionClose returns the close of the session on day's calendar date: 4
// p.m. New York time, 1 p.m. on an early close. A day that is not a session
// has no close and is an error.
func SessionClose(day time.Time) (time.Time, error) {
	trading, err := IsTradingDay(day)
	if err != nil {
		return time.Time{}, err
	}
	if !trading {
		return time.Time{}, fmt.Errorf("nysecalendar: %s is not an NYSE trading day", key(dateOf(day)))
	}
	return closeOf(dateOf(day)), nil
}

// closeOf is the close of trading day d.
func closeOf(d time.Time) time.Time {
	hour := 16
	if earlyClose(d) {
		hour = 13
	}
	return time.Date(d.Year(), d.Month(), d.Day(), hour, 0, 0, 0, NewYork)
}

// Closes counts the sessions that closed after after and no later than
// through, and returns the latest of them. Weekends and holidays close
// nothing. A span reaching a year the calendar does not cover is an error.
func Closes(after, through time.Time) (int, time.Time, error) {
	if !through.After(after) {
		return 0, time.Time{}, nil
	}
	n := 0
	var latest time.Time
	last := dateOf(through.In(NewYork))
	for d := dateOf(after.In(NewYork)); !d.After(last); d = d.AddDate(0, 0, 1) {
		trading, err := IsTradingDay(d)
		if err != nil {
			return 0, time.Time{}, err
		}
		if c := closeOf(d); trading && c.After(after) && !c.After(through) {
			n++
			latest = c
		}
	}
	return n, latest, nil
}
