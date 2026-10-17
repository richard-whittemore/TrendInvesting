package nysecalendar_test

import (
	"errors"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/nysecalendar"
)

func date(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return d
}

// The same days adapter/marketdata/tests/test_nyse_calendar.py checks, so the
// Go and Python calendars cannot drift apart unnoticed.
func TestNYSEFullDayHolidays2024To2027(t *testing.T) {
	for _, h := range []string{
		"2024-01-01", "2024-01-15", "2024-02-19", "2024-03-29", "2024-05-27", "2024-06-19",
		"2024-07-04", "2024-09-02", "2024-11-28", "2024-12-25",
		"2025-01-01", "2025-01-09", "2025-01-20", "2025-02-17", "2025-04-18", "2025-05-26",
		"2025-06-19", "2025-07-04", "2025-09-01", "2025-11-27", "2025-12-25",
		"2026-01-01", "2026-01-19", "2026-02-16", "2026-04-03", "2026-05-25", "2026-06-19",
		"2026-07-03", "2026-09-07", "2026-11-26", "2026-12-25",
		"2027-01-01", "2027-01-18", "2027-02-15", "2027-03-26", "2027-05-31", "2027-06-18",
		"2027-07-05", "2027-09-06", "2027-11-25", "2027-12-24",
	} {
		trading, err := nysecalendar.IsTradingDay(date(h))
		if err != nil || trading {
			t.Errorf("%s: trading %v, err %v; want a holiday", h, trading, err)
		}
	}
}

func TestOrdinaryWeekdaysAndEarlyClosesTrade(t *testing.T) {
	for _, d := range []string{"2026-03-27", "2026-07-02", "2025-07-03", "2025-11-28", "2025-12-24", "2026-12-24", "2027-12-31"} {
		trading, err := nysecalendar.IsTradingDay(date(d))
		if err != nil || !trading {
			t.Errorf("%s: trading %v, err %v", d, trading, err)
		}
	}
	for _, d := range []string{"2026-03-28", "2026-03-29"} {
		if trading, _ := nysecalendar.IsTradingDay(date(d)); trading {
			t.Errorf("%s is a weekend", d)
		}
	}
}

func TestOutsideTheTableFailsClosed(t *testing.T) {
	if _, err := nysecalendar.IsTradingDay(date("2035-01-03")); !errors.Is(err, nysecalendar.ErrOutOfRange) {
		t.Fatalf("err = %v", err)
	}
}

func TestEarlyClosesAreAtOneAndOtherSessionsAtFourNewYorkTime(t *testing.T) {
	for _, e := range []string{"2025-07-03", "2025-11-28", "2025-12-24", "2026-11-27", "2026-12-24", "2024-07-03"} {
		c, err := nysecalendar.SessionClose(date(e))
		if err != nil || c.In(nysecalendar.NewYork).Hour() != 13 {
			t.Errorf("%s: close %v, err %v", e, c, err)
		}
	}
	for _, f := range []string{"2026-03-27", "2026-07-02", "2027-12-23", "2022-07-01"} {
		c, err := nysecalendar.SessionClose(date(f))
		if err != nil || c.In(nysecalendar.NewYork).Hour() != 16 {
			t.Errorf("%s: close %v, err %v", f, c, err)
		}
	}
	if _, err := nysecalendar.SessionClose(date("2026-04-03")); err == nil {
		t.Fatal("a closed day has no close")
	}
	if _, err := nysecalendar.SessionClose(date("2035-01-03")); !errors.Is(err, nysecalendar.ErrOutOfRange) {
		t.Fatalf("a day outside the table has no known close, err = %v", err)
	}
}

func ny(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, nysecalendar.NewYork)
	if err != nil {
		panic(err)
	}
	return t
}

func TestClosesCountsOnlySessionClosesAfterTheStartAndByTheEnd(t *testing.T) {
	for name, c := range map[string]struct {
		after, through string
		want           int
		latest         string
	}{
		"a weekend closes nothing":          {"2026-10-16 17:30", "2026-10-19 09:00", 0, ""},
		"Good Friday and a weekend":         {"2026-04-02 18:00", "2026-04-06 15:59", 0, ""},
		"the next session's close":          {"2026-10-12 18:00", "2026-10-13 16:00", 1, "2026-10-13 16:00"},
		"two sessions":                      {"2026-10-12 18:00", "2026-10-14 16:30", 2, "2026-10-14 16:00"},
		"a close at the start is not after": {"2026-10-13 16:00", "2026-10-13 23:00", 0, ""},
		"Thanksgiving, then an early close": {"2026-11-25 18:00", "2026-11-27 13:30", 1, "2026-11-27 13:00"},
		"the end before the start is empty": {"2026-10-14 18:00", "2024-01-02 12:00", 0, ""},
	} {
		n, latest, err := nysecalendar.Closes(ny(c.after), ny(c.through))
		if err != nil || n != c.want {
			t.Errorf("%s: %d closes, err %v; want %d", name, n, err, c.want)
			continue
		}
		if c.latest != "" && !latest.Equal(ny(c.latest)) {
			t.Errorf("%s: latest close %v, want %s", name, latest, c.latest)
		}
	}
	if _, _, err := nysecalendar.Closes(ny("2030-12-30 18:00"), ny("2031-01-03 18:00")); !errors.Is(err, nysecalendar.ErrOutOfRange) {
		t.Fatalf("a span past the table must fail closed, err = %v", err)
	}
}
