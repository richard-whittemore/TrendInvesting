package main

import (
	"bytes"
	"strings"
	"testing"
)

const config = "testdata/momentum.json"

// universeLine is one request: the snapshot of session, ranked with next as
// the session after it. AAA and BBB rank; CCC is under the price floor and
// ETF is not common stock.
func universeLine(session, next string) string {
	return `{"next_session":"` + next + `","universe":{"session":"` + session + `","source":"massive","rows":[` +
		`{"instrument_id":"AAA","type":"CS","close":20,"volume":1000,"sector":"Technology","sector_source":"ishares-iwv-gics"},` +
		`{"instrument_id":"BBB","type":"CS","close":50,"volume":1000,"sector":"","sector_source":""},` +
		`{"instrument_id":"CCC","type":"CS","close":4,"volume":1e9,"sector":"","sector_source":""},` +
		`{"instrument_id":"ETF","type":"ETF","close":30,"volume":1e9,"sector":"","sector_source":""}]}}`
}

func TestRankAnswersEachSnapshotInOrder(t *testing.T) {
	in := universeLine("2026-11-27", "2026-11-30") + "\n" + universeLine("2026-11-30", "2026-12-01") + "\n"
	var out, errs bytes.Buffer
	if err := run([]string{"-config", config}, strings.NewReader(in), &out, &errs); err != nil {
		t.Fatalf("%v: %s", err, errs.String())
	}
	want := `{"session":"2026-11-27","decision_session":false,"window":4,"members":[]}` + "\n" +
		`{"session":"2026-11-30","decision_session":true,"window":4,"members":["BBB","AAA"]}` + "\n"
	if out.String() != want {
		t.Errorf("got\n%swant\n%s", out.String(), want)
	}
}

func TestRankRefusesWhatTheRunWouldRefuse(t *testing.T) {
	for name, c := range map[string]struct {
		args []string
		in   string
		want string
	}{
		"no configuration":      {nil, "", "-config"},
		"missing configuration": {[]string{"-config", "testdata/none.json"}, "", "no such file"},
		"not JSON":              {[]string{"-config", config}, "{", "line 1"},
		"unknown field":         {[]string{"-config", config}, `{"universe":{},"next":"2026-12-01"}` + "\n", "unknown field"},
		"an invalid snapshot":   {[]string{"-config", config}, `{"universe":{},"next_session":"2026-12-01"}` + "\n", "invalid universe snapshot"},
		"another source":        {[]string{"-config", config}, strings.Replace(universeLine("2026-11-30", "2026-12-01"), `"massive"`, `"other"`, 1), "this run reads"},
		"a bad flag":            {[]string{"-nope"}, "", "flag provided but not defined"},
		"a bars configuration":  {[]string{"-config", "testdata/momentum-bars.json"}, universeLine("2026-11-30", "2026-12-01"), "reads bars"},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errs bytes.Buffer
			err := run(c.args, strings.NewReader(c.in), &out, &errs)
			if err == nil || !strings.Contains(err.Error()+errs.String(), c.want) {
				t.Fatalf("error %v (%s), want one mentioning %q", err, errs.String(), c.want)
			}
		})
	}
}
