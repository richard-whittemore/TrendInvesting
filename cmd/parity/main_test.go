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
	for _, want := range []string{"- BX_SYN: 300 sessions from 2 tagged orders of 3", "| BX_SYN | boost-state | latched state and target exact | 90 | 0 | 0 | PASS |",
		"| BX_SYN | boost-rsi | rel <= 1e-09, readiness exact | 300 | 0 | 0 | PASS |", "**Overall: PASS**"} {
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

// fullOrder is a QuantConnect order object as the orders listing gives it.
type fullOrder struct {
	ID           int               `json:"id"`
	Symbol       map[string]string `json:"symbol"`
	Quantity     float64           `json:"quantity"`
	Status       int               `json:"status"`
	Time         string            `json:"time"`
	LastFillTime *string           `json:"lastFillTime"`
	Tag          string            `json:"tag"`
}

func writeFullOrders(t *testing.T, orders []fullOrder) string {
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

const glideTag = glidePrefix + `{"month":"2002-08","date":"2002-08-01","glide":"path","start_age":38.0,` +
	`"measure_from":"2002-08","age":38.0,"b":0.0,"book":{"AAA":0.49,"IEF":0.49},"targets":{"AAA":0.49,"IEF":0.49}}`

const lotTag = lotPrefix + `{"month":"2002-08","mode":"held","nlv":1000.0,"risk_off":false,` +
	`"ranked":[["AAA",0.5,0.02,311]],"prices":{"AAA":10.0},"held":{},"weights":{"AAA":0.98},"skipped":[],"kept":[]}`

// iraTag plans 98 shares of AAA at $10 from $1,000 of cash, bought at once.
const iraTag = iraPrefix + `{"month":"2002-08","decision":"2002-08-01","nlv":1000.0,"targets":{"AAA":0.98},` +
	`"held":{},"prices":{"AAA":10.0},"working":{},"unpriced":[],"sells":[],"buys":[["AAA",98,10.5]],"skipped":[],` +
	`"waited":0,"buy_session":"2002-08-01","delay":1,"band":0.25,"price_band":0.05,"max_fees":2.0,"budget":1100.0,` +
	`"scale":1.0,"kept":[["AAA",98]],"scaled_skipped":[],"for_sells":0,"for_price":0,"unplaced":[],"placed":[["AAA","buy",1]]}`

func TestGlideLotAndIRAExportsAreComparedAgainstTheirPlannedMonths(t *testing.T) {
	fill := "2002-08-01T20:00:00Z"
	orders := writeFullOrders(t, []fullOrder{
		{ID: 1, Symbol: map[string]string{"value": "AAA", "id": "AAA X"}, Quantity: 98, Status: 3,
			Time: "2002-08-01T13:00:00Z", LastFillTime: &fill, Tag: "IRAORD1 2002-08 buy"},
		{ID: 2, Symbol: map[string]string{"value": "SPY", "id": "SPY X"}, Quantity: 1, Status: 5,
			Time: "2002-08-01T13:00:00Z", Tag: glideTag},
		{ID: 3, Symbol: map[string]string{"value": "SPY", "id": "SPY X"}, Quantity: 1, Status: 5,
			Time: "2002-08-01T13:00:00Z", Tag: lotTag},
		{ID: 4, Symbol: map[string]string{"value": "SPY", "id": "SPY X"}, Quantity: 1, Status: 5,
			Time: "2002-08-01T13:00:00Z", Tag: iraTag},
	})
	var out, errs bytes.Buffer
	pass, err := run([]string{"-glide", "G=" + orders, "-glide-months", "G=2002-08..2002-09",
		"-lot", "L=" + orders, "-lot-months", "L=2002-08", "-ira", "I=" + orders, "-ira-months", "I=2002-08"}, &out, &errs)
	if err != nil {
		t.Fatal(err)
	}
	if pass {
		t.Error("a planned month missing from the glide export must fail")
	}
	for _, want := range []string{
		"- G: 1 logged months from 1 tagged orders of 4 (planned 2)",
		"- L: 1 logged months from 1 tagged orders of 4 (planned 1)",
		"- I: 1 logged months from 1 tagged orders of 4 (planned 1)",
		"| G | glide-months | every planned month exported once | 2 | 1 |", "planned month 2002-09 exported 0 times",
		"| L | d7l-picks | exact, withheld included | 2 | 0 |", "| I | ira-orders | exact | 1 | 0 |",
		"| I | ira-sequence |",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}

func TestMonthRangesExpandInclusivelyAcrossYears(t *testing.T) {
	got, err := expandMonths("2015-11..2016-02,2016-05")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "2015-11,2015-12,2016-01,2016-02,2016-05" {
		t.Errorf("expanded %v", got)
	}
	for _, bad := range []string{"2016-02..2015-11", "2015-13..2016-01", "2015-11..x", "2015-1", "", "2015-11,,2015-12"} {
		if _, err := expandMonths(bad); err == nil {
			t.Errorf("%q expanded", bad)
		}
	}
}

func TestTheCommandRefusesWhatItCannotCompareOfTheNewKinds(t *testing.T) {
	good := writeOrders(t, []order{{ID: 1, Tag: glideTag}})
	bad := writeOrders(t, []order{{ID: 1, Tag: glidePrefix + "{"}, {ID: 2, Tag: lotPrefix + "{"}, {ID: 3, Tag: iraPrefix + "{"}})
	notOrders := writeOrders(t, []order{{ID: 1, Tag: iraTag}})
	if err := os.WriteFile(notOrders, []byte(`[{"id":1,"quantity":"x","tag":"`+strings.ReplaceAll(iraTag, `"`, `\"`)+`"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"glide months for no run": {[]string{"-glide", "A=" + good, "-glide-months", "A=2002-08", "-glide-months", "B=2002-08"}, "B has months but no export"},
		"lot months for no run":   {[]string{"-lot", "A=" + good, "-lot-months", "A=2002-08", "-lot-months", "B=2002-08"}, "B has months but no export"},
		"ira months for no run":   {[]string{"-ira", "A=" + good, "-ira-months", "A=2002-08", "-ira-months", "B=2002-08"}, "B has months but no export"},
		"a glide run sans months": {[]string{"-glide", "A=" + good}, "A: has no planned months"},
		"a bad month range":       {[]string{"-glide", "A=" + good, "-glide-months", "A=2002-09..2002-08"}, "A: months"},
		"a bad momentum month":    {[]string{"-momentum", "A=" + good, "-months", "A=2002-8"}, "A: months"},
		"a malformed glide month": {[]string{"-glide", "A=" + bad, "-glide-months", "A=2002-08"}, "order 1"},
		"a malformed lot month":   {[]string{"-lot", "A=" + bad, "-lot-months", "A=2002-08"}, "order 2"},
		"a malformed ira month":   {[]string{"-ira", "A=" + bad, "-ira-months", "A=2002-08"}, "order 3"},
		"no tagged lot month":     {[]string{"-lot", "A=" + good, "-lot-months", "A=2002-08"}, "no tagged order"},
		"ira orders unreadable":   {[]string{"-ira", "A=" + notOrders, "-ira-months", "A=2002-08"}, "A: orders"},
		"a bad glide flag":        {[]string{"-glide", "nope"}, "NAME=VALUE"},
		"a missing ira file":      {[]string{"-ira", "A=/nonexistent/o.json", "-ira-months", "A=2002-08"}, "no such file"},
		"a missing calendar":      {[]string{"-ira", "A=" + good, "-ira-months", "A=2002-08", "-ira-calendar", "/nonexistent/c.json"}, "ira calendar"},
		"a calendar not a list":   {[]string{"-ira", "A=" + good, "-ira-months", "A=2002-08", "-ira-calendar", calendar(t, `{}`)}, "ira calendar"},
		"a calendar out of order": {[]string{"-ira", "A=" + good, "-ira-months", "A=2002-08", "-ira-calendar", calendar(t, `["2002-08-02","2002-08-01"]`)}, "does not follow"},
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

func calendar(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheIRASequenceCountsSessionsByTheCalendarFile(t *testing.T) {
	// Bought a session after the rebalance, waiting for a price.
	tag := strings.NewReplacer(`"waited":0,"buy_session":"2002-08-01"`, `"waited":1,"buy_session":"2002-08-02"`,
		`"for_price":0`, `"for_price":1`).Replace(iraTag)
	fill := "2002-08-02T20:00:00Z"
	orders := writeFullOrders(t, []fullOrder{
		{ID: 1, Symbol: map[string]string{"value": "AAA", "id": "AAA X"}, Quantity: 98, Status: 3,
			Time: "2002-08-02T12:00:00Z", LastFillTime: &fill, Tag: "IRAORD1 2002-08 buy"},
		{ID: 2, Symbol: map[string]string{"value": "SPY", "id": "SPY X"}, Quantity: 1, Status: 5,
			Time: "2002-08-02T12:00:00Z", Tag: tag},
	})
	args := []string{"-ira", "I=" + orders, "-ira-months", "I=2002-08"}
	for _, c := range []struct {
		extra []string
		want  string
	}{
		{nil, "- I ira-sequence: 2002-08: parity: no session calendar covers the span"},
		{[]string{"-ira-calendar", calendar(t, `["2002-07-31","2002-08-01","2002-08-02"]`)}, "| I | ira-sequence |"},
	} {
		var out, errs bytes.Buffer
		if _, err := run(append(args, c.extra...), &out, &errs); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), c.want) {
			t.Errorf("report lacks %q:\n%s", c.want, out.String())
		}
		if c.extra != nil && strings.Contains(out.String(), "- I ira-sequence:") {
			t.Errorf("with a calendar the sequence failed:\n%s", out.String())
		}
	}
}
