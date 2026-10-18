package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/parity"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/boost"
)

// Synthetic exports only; no market data.

type order struct {
	ID  int    `json:"id"`
	Tag string `json:"tag"`
}

func writeOrders(t *testing.T, orders []order) string {
	t.Helper()
	raw, err := json.Marshal(orders)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "orders.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// boostRows logs n synthetic sessions as the Boost template does, the
// first warmup of them in warm-up.
func boostRows(t *testing.T, n, warmup int) [][]any {
	t.Helper()
	p := parity.BacktestBoost()
	ind, err := boost.NewIndicators(p)
	if err != nil {
		t.Fatal(err)
	}
	var rows [][]any
	day := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	price, latched := 100.0, false
	opt := func(v float64, ok bool) any {
		if !ok {
			return nil
		}
		return v
	}
	for i := 0; i < n; i++ {
		price = float64(price * 1.001)
		if i%9 == 7 {
			price = float64(price * 0.97)
		}
		r := ind.Add(price)
		for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			day = day.AddDate(0, 0, 1)
		}
		row := []any{day.Format(time.DateOnly), price,
			opt(r.RSI, r.RSIReady), opt(r.TrendMean, r.TrendMeanReady), opt(r.ExitMean, r.ExitMeanReady), nil, nil}
		if i >= warmup {
			latched = p.Next(latched, r)
			state := 0
			if latched {
				state = 1
			}
			row[5], row[6] = state, p.Target(latched)
		}
		rows = append(rows, row)
		day = day.AddDate(0, 0, 1)
	}
	return rows
}

// writePlan writes the plan file of a run of the given range and count.
func writePlan(t *testing.T, first, last string, sessions int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.json")
	raw := `{"first":"` + first + `","last":"` + last + `","sessions":` + strconv.Itoa(sessions) + `}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// plan is the plan file of rows: first, last, count.
func plan(t *testing.T, rows [][]any) string {
	t.Helper()
	return writePlan(t, rows[0][0].(string), rows[len(rows)-1][0].(string), len(rows))
}

func boostTag(t *testing.T, rows [][]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"rows": rows})
	if err != nil {
		t.Fatal(err)
	}
	return boostPrefix + string(raw)
}

func TestABoostExportSplitAcrossOrdersIsJoinedInOrderIDOrderAndPasses(t *testing.T) {
	rows := boostRows(t, 300, 210)
	path := writeOrders(t, []order{
		{ID: 9, Tag: boostTag(t, rows[150:])},
		{ID: 3, Tag: "an ordinary order tag"},
		{ID: 2, Tag: boostTag(t, rows[:150])},
	})
	var out, errs bytes.Buffer
	pass, err := run([]string{"-boost", "BX_SYN=" + path, "-sessions", "BX_SYN=" + plan(t, rows)}, &out, &errs)
	if err != nil {
		t.Fatal(err)
	}
	if !pass {
		t.Fatalf("a matching export must pass:\n%s", out.String())
	}
	for _, want := range []string{"- BX_SYN: 300 sessions from 2 tagged orders of 3", "| BX_SYN | boost-state | latched state and target exact | 90 | 0 | 0 | PASS |", "**Overall: PASS**"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}

func TestChunksInTheWrongOrderFail(t *testing.T) {
	rows := boostRows(t, 300, 210)
	path := writeOrders(t, []order{{ID: 2, Tag: boostTag(t, rows[150:])}, {ID: 9, Tag: boostTag(t, rows[:150])}})
	var out, errs bytes.Buffer
	pass, err := run([]string{"-boost", "BX_SYN=" + path, "-sessions", "BX_SYN=" + plan(t, rows)}, &out, &errs)
	if err != nil || pass {
		t.Fatalf("pass=%v err=%v: sessions out of order must fail the comparison", pass, err)
	}
}

func TestABoostExportMissingItsFinalChunkFailsAgainstThePlan(t *testing.T) {
	rows := boostRows(t, 300, 210)
	path := writeOrders(t, []order{{ID: 2, Tag: boostTag(t, rows[:150])}})
	var out, errs bytes.Buffer
	pass, err := run([]string{"-boost", "BX_SYN=" + path, "-sessions", "BX_SYN=" + plan(t, rows)}, &out, &errs)
	if err != nil || pass {
		t.Fatalf("pass=%v err=%v: an export short of the planned sessions must fail", pass, err)
	}
	for _, want := range []string{"- BX_SYN: 150 sessions from 1 tagged orders of 1", "| BX_SYN | export |", "FAIL"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}

const monthTag = momentumPrefix + `{"month":"2009-06","risk_off":false,"spy_close":101.5,"spy_mean":99.25,"spy_n":3,` +
	`"members":1,"scored":0,"index":["2009-05-27","2009-05-28","2009-05-29"],"ranked":[],"weights":{},` +
	`"short":{"NEW":2},"closes":{"SPY":[100.0,101.0,101.5],"NEW":[null,10.5,11.0]}}`

func TestAMomentumExportIsComparedAgainstItsPlannedMonths(t *testing.T) {
	path := writeOrders(t, []order{{ID: 1, Tag: monthTag}})
	var out, errs bytes.Buffer
	pass, err := run([]string{"-momentum", "FU_SYN=" + path, "-months", "FU_SYN=2009-06,2009-07"}, &out, &errs)
	if err != nil {
		t.Fatal(err)
	}
	if pass {
		t.Error("a planned month missing from the export must fail")
	}
	for _, want := range []string{"- FU_SYN: 1 logged months from 1 tagged orders of 1", "planned month 2009-07 exported 0 times", "**Overall: FAIL**"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}

func TestTheCommandRefusesWhatItCannotCompare(t *testing.T) {
	good := writeOrders(t, []order{{ID: 1, Tag: monthTag}})
	bad := writeOrders(t, []order{{ID: 1, Tag: momentumPrefix + "{"}})
	badBoost := writeOrders(t, []order{{ID: 1, Tag: boostPrefix + "{"}})
	none := writeOrders(t, []order{{ID: 1, Tag: "plain"}})
	notJSON := filepath.Join(t.TempDir(), "x.json")
	if err := os.WriteFile(notJSON, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan2 := writePlan(t, "2001-01-01", "2001-01-02", 2)
	badPlans := map[string]string{"first after last": "2001-01-02,2001-01-01,2", "more than the weekdays": "2001-01-01,2001-01-02,9"}
	for name, spec := range badPlans {
		first, last, n := strings.Split(spec, ",")[0], strings.Split(spec, ",")[1], strings.Split(spec, ",")[2]
		cnt, _ := strconv.Atoi(n)
		path := writePlan(t, first, last, cnt)
		var out, errs bytes.Buffer
		if _, err := run([]string{"-boost", "A=" + good, "-sessions", "A=" + path}, &out, &errs); err == nil || !strings.Contains(err.Error(), "A: sessions plan") {
			t.Errorf("%s: error %v, want one naming the plan", name, err)
		}
	}
	notPlan := filepath.Join(t.TempDir(), "notplan.json")
	if err := os.WriteFile(notPlan, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no input":                     {nil, "nothing to compare"},
		"a bad flag":                   {[]string{"-nope"}, "flag provided but not defined"},
		"a run without a name":         {[]string{"-boost", good}, "NAME=VALUE"},
		"a run named twice":            {[]string{"-boost", "A=" + good, "-boost", "A=" + good}, "named twice"},
		"months for no run":            {[]string{"-momentum", "A=" + good, "-months", "A=2009-06", "-months", "B=2009-06"}, "B has months but no export"},
		"sessions for no run":          {[]string{"-boost", "A=" + good, "-sessions", "A=" + plan2, "-sessions", "B=" + plan2}, "B has sessions but no export"},
		"a boost run without sessions": {[]string{"-boost", "A=" + good}, "A has no planned sessions"},
		"a plan file that is missing":  {[]string{"-boost", "A=" + good, "-sessions", "A=/nonexistent/plan.json"}, "A: sessions plan"},
		"a plan file with no fields":   {[]string{"-boost", "A=" + good, "-sessions", "A=" + notPlan}, "A: sessions plan"},
		"a run without months":         {[]string{"-momentum", "A=" + good}, "A has no planned months"},
		"a missing file":               {[]string{"-boost", "A=/nonexistent/orders.json", "-sessions", "A=" + plan2}, "no such file"},
		"a file that is not JSON":      {[]string{"-boost", "A=" + notJSON, "-sessions", "A=" + plan2}, "unexpected end"},
		"a malformed month":            {[]string{"-momentum", "A=" + bad, "-months", "A=2009-06"}, "order 1"},
		"a malformed chunk":            {[]string{"-boost", "A=" + badBoost, "-sessions", "A=" + plan2}, "order 1"},
		"no tagged order":              {[]string{"-boost", "A=" + none, "-sessions", "A=" + plan2}, "no tagged order"},
		"no tagged month":              {[]string{"-momentum", "A=" + none, "-months", "A=2009-06"}, "no tagged order"},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errs bytes.Buffer
			_, err := run(c.args, &out, &errs)
			if err == nil || !strings.Contains(err.Error()+errs.String(), c.want) {
				t.Fatalf("error %v (%s), want one mentioning %q", err, errs.String(), c.want)
			}
		})
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

func TestAReportThatCannotBeWrittenIsAnError(t *testing.T) {
	rows := boostRows(t, 300, 210)
	path := writeOrders(t, []order{{ID: 1, Tag: boostTag(t, rows)}})
	var errs bytes.Buffer
	if _, err := run([]string{"-boost", "BX_SYN=" + path, "-sessions", "BX_SYN=" + plan(t, rows)}, brokenWriter{}, &errs); err == nil {
		t.Error("a failed write must be reported")
	}
}
