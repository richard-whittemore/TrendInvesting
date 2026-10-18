package parity_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/parity"
)

func TestASessionListCountsTheSessionsItCovers(t *testing.T) {
	s, err := parity.SessionList([]string{"2020-04-08", "2020-04-09", "2020-04-13", "2020-04-14"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		after, through string
		want           []string
	}{
		{"2020-04-08", "2020-04-13", []string{"2020-04-09", "2020-04-13"}},
		{"2020-04-09", "2020-04-13", []string{"2020-04-13"}},
		{"2020-04-10", "2020-04-14", []string{"2020-04-13", "2020-04-14"}},
		{"2020-04-13", "2020-04-13", nil},
	} {
		got, err := s(c.after, c.through)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("(%s, %s]: %v, %v; want %v", c.after, c.through, got, err, c.want)
		}
	}
	for _, span := range [][2]string{{"2020-04-07", "2020-04-09"}, {"2020-04-13", "2020-04-15"}} {
		if _, err := s(span[0], span[1]); !errors.Is(err, parity.ErrNoSessions) {
			t.Errorf("%v: %v", span, err)
		}
	}
	if _, err := s("2020-04-xx", "2020-04-13"); err == nil || errors.Is(err, parity.ErrNoSessions) {
		t.Errorf("a bad date: %v", err)
	}
}

func TestAMalformedSessionListIsRefused(t *testing.T) {
	for name, dates := range map[string][]string{
		"empty":     nil,
		"bad date":  {"2020-04-08", "2020-4-9"},
		"unsorted":  {"2020-04-09", "2020-04-08"},
		"duplicate": {"2020-04-08", "2020-04-08"},
		// internal/nysecalendar covers 2022: a list must agree with it.
		"a session missing": {"2022-01-03", "2022-01-05"},
		"a holiday listed":  {"2022-01-14", "2022-01-17", "2022-01-18"},
	} {
		if _, err := parity.SessionList(dates); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := parity.SessionList([]string{"2022-01-14", "2022-01-18"}); err != nil {
		t.Errorf("a list agreeing with the NYSE calendar: %v", err)
	}
}

func TestTheNYSECalendarCountsSessionsInTheYearsItCovers(t *testing.T) {
	got, err := parity.NYSESessions("2022-01-14", "2022-01-18")
	if err != nil || !reflect.DeepEqual(got, []string{"2022-01-18"}) {
		t.Errorf("across MLK Day: %v, %v", got, err)
	}
	if got, err := parity.NYSESessions("2022-01-18", "2022-01-18"); err != nil || got != nil {
		t.Errorf("an empty span: %v, %v", got, err)
	}
	if _, err := parity.NYSESessions("2021-12-30", "2022-01-04"); !errors.Is(err, parity.ErrNoSessions) {
		t.Errorf("2021: %v", err)
	}
	for _, span := range [][2]string{{"2022-01-xx", "2022-01-18"}, {"2022-01-14", "x"}} {
		if _, err := parity.NYSESessions(span[0], span[1]); err == nil || errors.Is(err, parity.ErrNoSessions) {
			t.Errorf("%v: %v", span, err)
		}
	}
}

func TestEitherIsTheFirstCalendarThatCoversASpan(t *testing.T) {
	list, err := parity.SessionList([]string{"2020-04-08", "2020-04-09"})
	if err != nil {
		t.Fatal(err)
	}
	both := parity.Either(list, parity.NYSESessions)
	if got, err := both("2020-04-08", "2020-04-09"); err != nil || len(got) != 1 {
		t.Errorf("the list's span: %v, %v", got, err)
	}
	if got, err := both("2022-01-14", "2022-01-18"); err != nil || len(got) != 1 {
		t.Errorf("the calendar's span: %v, %v", got, err)
	}
	if _, err := both("2021-04-08", "2021-04-09"); !errors.Is(err, parity.ErrNoSessions) {
		t.Errorf("neither: %v", err)
	}
	if _, err := both("2020-04-08", "x"); err == nil || errors.Is(err, parity.ErrNoSessions) {
		t.Errorf("a bad date is not a gap in coverage: %v", err)
	}
}
