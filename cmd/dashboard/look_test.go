package main

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// execPage renders the page for accounts with no theme chosen.
func execPage(w io.Writer, accounts []account) error {
	return page.Execute(w, pageData{Accounts: accounts})
}

func execThemed(t *testing.T, theme string, accounts []account) string {
	t.Helper()
	var sb strings.Builder
	if err := page.Execute(&sb, pageData{Accounts: accounts, Theme: theme}); err != nil {
		t.Fatal(err)
	}
	return sb.String()
}

// pairedFixture is a synthetic summary with n paired points: the account
// grows 1% a day from 1000 and the benchmark 0.5% a day from 100.
func pairedFixture(n int) account {
	eq := report.Equity{Available: n >= 2}
	net, bench := 1000.0, 100.0
	for i := 0; i < n; i++ {
		eq.Points = append(eq.Points, report.EquityPoint{Date: fmt.Sprintf("2024-01-%02d", i+2), Net: net, Benchmark: bench})
		net, bench = net*1.01, bench*1.005
	}
	if n > 0 {
		eq.StartDate, eq.EndDate = eq.Points[0].Date, eq.Points[n-1].Date
	}
	if n >= 2 {
		eq.AccountReturn, eq.BenchmarkReturn = eq.Points[n-1].Net/1000-1, eq.Points[n-1].Benchmark/100-1
		eq.Excess = eq.AccountReturn - eq.BenchmarkReturn
		eq.DayAvailable, eq.DayStartDate = true, eq.Points[n-2].Date
		eq.DayAccountReturn, eq.DayBenchmarkReturn = 0.01, 0.005
	}
	return account{Summary: report.Summary{AccountID: "joint", StrategyID: "boost", Date: eq.EndDate, Benchmark: "SPY", EngineState: "Normal", Equity: eq}}
}

var pathOf = func(class string) *regexp.Regexp {
	return regexp.MustCompile(`<path class="` + class + `" d="([^"]*)"`)
}

// pointsIn counts the vertices of an SVG path drawn as one M and L commands.
func pointsIn(d string) int { return strings.Count(d, "M") + strings.Count(d, "L") }

func TestEquityChartDrawsOnePathPerSeriesWithOnePointPerPairedDay(t *testing.T) {
	for _, theme := range []string{"", "dark", "light"} {
		body := execThemed(t, theme, []account{pairedFixture(6)})
		for _, class := range []string{"line-account", "line-benchmark"} {
			found := pathOf(class).FindAllStringSubmatch(body, -1)
			if len(found) != 1 {
				t.Fatalf("theme %q: want one %s path, got %d", theme, class, len(found))
			}
			if got := pointsIn(found[0][1]); got != 6 {
				t.Errorf("theme %q: %s has %d points, want 6 (the paired days)", theme, class, got)
			}
		}
	}
}

func TestEquityChartNeedsTwoPairedPointsNotABrokenChart(t *testing.T) {
	for _, n := range []int{0, 1} {
		body := execThemed(t, "", []account{pairedFixture(n)})
		if !strings.Contains(body, "needs two paired points") {
			t.Errorf("%d points: the page must say the chart needs two paired points", n)
		}
		if strings.Contains(body, `class="line-account"`) || strings.Contains(body, "NaN") || strings.Contains(body, "Inf") {
			t.Errorf("%d points: no chart may be drawn", n)
		}
	}
}

func TestDailyChangeStripHasABarPerSeriesPerDay(t *testing.T) {
	body := execThemed(t, "", []account{pairedFixture(6)})
	for _, class := range []string{"bar-account", "bar-benchmark"} {
		if got := strings.Count(body, `<rect class="`+class); got != 5 {
			t.Errorf("%s bars = %d, want 5 (one per day between 6 paired points)", class, got)
		}
	}
	if strings.Contains(execThemed(t, "", []account{pairedFixture(1)}), `<rect class="bar-`) {
		t.Error("a single point has no daily change to draw")
	}
}

func TestEquityCardKeepsTheCountDatesAndStatesTheAssumption(t *testing.T) {
	body := execThemed(t, "", []account{pairedFixture(6)})
	for _, want := range []string{"6 paired points", "2024-01-02", "2024-01-07", "price return only, no deposits or withdrawals", "Equity against SPY"} {
		if !strings.Contains(body, want) {
			t.Errorf("equity card lacks %q", want)
		}
	}
}

func TestChartsLabelAxesInWordsAndDates(t *testing.T) {
	body := execThemed(t, "", []account{pairedFixture(6)})
	for _, want := range []string{`role="img"`, "aria-label=", "Account, solid line", "SPY, dashed line", "<text"} {
		if !strings.Contains(body, want) {
			t.Errorf("charts lack %q", want)
		}
	}
}

func TestThemeQueryIsHonouredAndLinksFlipIt(t *testing.T) {
	h := newHandler([]string{writeJournal(t, t.TempDir())}, "SPY")
	cases := []struct {
		query, root string
		absent      bool
	}{{"/?theme=dark", `<html lang="en" data-theme="dark">`, false}, {"/?theme=light", `<html lang="en" data-theme="light">`, false}, {"/", `<html lang="en">`, false}, {"/?theme=neon", `<html lang="en">`, false}}
	for _, c := range cases {
		rec := get(h, http.MethodGet, c.query)
		body := rec.Body.String()
		if !strings.Contains(body, c.root) {
			t.Errorf("%s: root element is not %s", c.query, c.root)
		}
		if !strings.Contains(body, `href="?theme=light"`) || !strings.Contains(body, `href="?theme=dark"`) {
			t.Errorf("%s: the header must link to both themes", c.query)
		}
	}
	dark := get(h, http.MethodGet, "/?theme=dark").Body.String()
	if !strings.Contains(dark, `href="?theme=dark" aria-current="true"`) || strings.Contains(dark, `href="?theme=light" aria-current`) {
		t.Error("the chosen theme's link is marked current, and only it")
	}
}

func TestPageCarriesBothThemesAndNoOutsideResources(t *testing.T) {
	body := execThemed(t, "", []account{pairedFixture(3)})
	for _, want := range []string{"prefers-color-scheme:dark", `:root[data-theme="dark"]`, `:root[data-theme="light"]`, "system-ui"} {
		if !strings.Contains(body, want) {
			t.Errorf("styles lack %q", want)
		}
	}
	for _, banned := range []string{"http://", "https://", "<script", "<link", "@import", "url(", "src="} {
		if strings.Contains(body, banned) {
			t.Errorf("the page reaches outside itself with %q", banned)
		}
	}
}

func TestEngineStateIsGroupedByHowMuchAttentionItNeeds(t *testing.T) {
	for state, want := range map[string]string{"normal": "ok", "Normal": "ok", "degraded": "warn", "halted": "bad"} {
		if got := stateClass(state); got != want {
			t.Errorf("stateClass(%q) = %q, want %q", state, got, want)
		}
	}
}
