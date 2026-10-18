package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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
		row := []any{"2001-" + strconv.Itoa(1+i/28) + "-" + strconv.Itoa(1+i%28), price,
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
	}
	// Two-digit months and days keep the dates in order as text.
	for _, row := range rows {
		var y, m, d int
		parts := strings.Split(row[0].(string), "-")
		y, _ = strconv.Atoi(parts[0])
		m, _ = strconv.Atoi(parts[1])
		d, _ = strconv.Atoi(parts[2])
		row[0] = strconv.Itoa(y) + "-" + pad(m) + "-" + pad(d)
	}
	return rows
}

func pad(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
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
	pass, err := run([]string{"-boost", "BX_SYN=" + path}, &out, &errs)
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
	pass, err := run([]string{"-boost", "BX_SYN=" + path}, &out, &errs)
	if err != nil || pass {
		t.Fatalf("pass=%v err=%v: sessions out of order must fail the comparison", pass, err)
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
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no input":                {nil, "nothing to compare"},
		"a bad flag":              {[]string{"-nope"}, "flag provided but not defined"},
		"a run without a name":    {[]string{"-boost", good}, "NAME=VALUE"},
		"a run named twice":       {[]string{"-boost", "A=" + good, "-boost", "A=" + good}, "named twice"},
		"months for no run":       {[]string{"-momentum", "A=" + good, "-months", "A=2009-06", "-months", "B=2009-06"}, "B has months but no export"},
		"a run without months":    {[]string{"-momentum", "A=" + good}, "A has no planned months"},
		"a missing file":          {[]string{"-boost", "A=/nonexistent/orders.json"}, "no such file"},
		"a file that is not JSON": {[]string{"-boost", "A=" + notJSON}, "unexpected end"},
		"a malformed month":       {[]string{"-momentum", "A=" + bad, "-months", "A=2009-06"}, "order 1"},
		"a malformed chunk":       {[]string{"-boost", "A=" + badBoost}, "order 1"},
		"no tagged order":         {[]string{"-boost", "A=" + none}, "no tagged order"},
		"no tagged month":         {[]string{"-momentum", "A=" + none, "-months", "A=2009-06"}, "no tagged order"},
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
