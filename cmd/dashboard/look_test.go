package main

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
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
	h := newHandlerFor([]string{writeJournal(t, t.TempDir())}, nil, "SPY")
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

// seriesFixture is a synthetic summary whose paired points carry exactly the
// given account and benchmark values, one per consecutive date.
func seriesFixture(nets, benches []float64) account {
	a := pairedFixture(0)
	eq := &a.Summary.Equity
	for i := range nets {
		eq.Points = append(eq.Points, report.EquityPoint{Date: fmt.Sprintf("d%05d", i), Net: nets[i], Benchmark: benches[i]})
	}
	eq.Available = len(nets) >= 2
	if n := len(nets); n >= 2 {
		first, last := eq.Points[0], eq.Points[n-1]
		eq.StartDate, eq.EndDate = first.Date, last.Date
		a.Summary.Date = last.Date
		eq.AccountReturn, eq.BenchmarkReturn = last.Net/first.Net-1, last.Benchmark/first.Benchmark-1
		eq.Excess = eq.AccountReturn - eq.BenchmarkReturn
		prev := eq.Points[n-2]
		eq.DayAvailable, eq.DayStartDate = true, prev.Date
		eq.DayAccountReturn, eq.DayBenchmarkReturn = last.Net/prev.Net-1, last.Benchmark/prev.Benchmark-1
	}
	return a
}

var numberIn = regexp.MustCompile(`-?\d+(?:\.\d+)?`)

func TestChartGeometryStaysInsidePlotForFlatFallingAndZeroChangeSeries(t *testing.T) {
	flat := []float64{100, 100, 100, 100}
	falling := []float64{100, 90, 70, 40}
	oneZeroDay := []float64{100, 110, 110, 90}
	for name, a := range map[string]account{
		"flat":        seriesFixture(flat, flat),
		"falling":     seriesFixture(falling, falling),
		"zero-change": seriesFixture(oneZeroDay, flat),
	} {
		body := execThemed(t, "", []account{a})
		if strings.Contains(body, "NaN") || strings.Contains(body, "Inf") {
			t.Errorf("%s: the page has NaN or Inf", name)
		}
		for _, d := range [][]string{{"line-account"}, {"line-benchmark"}} {
			for _, m := range pathOf(d[0]).FindAllStringSubmatch(body, -1) {
				nums := numberIn.FindAllString(m[1], -1)
				for i, s := range nums {
					v, _ := strconv.ParseFloat(s, 64)
					lo, hi := 56.0, 400.0
					if i%2 == 1 {
						lo, hi = 40, 160
					}
					if v < lo || v > hi {
						t.Errorf("%s: %s coordinate %v outside [%v,%v]", name, d[0], v, lo, hi)
					}
				}
			}
		}
		for _, m := range regexp.MustCompile(`<rect class="bar-[a-z]+" x="([^"]*)" y="([^"]*)" width="([^"]*)" height="([^"]*)"`).FindAllStringSubmatch(body, -1) {
			x, _ := strconv.ParseFloat(m[1], 64)
			y, _ := strconv.ParseFloat(m[2], 64)
			w, _ := strconv.ParseFloat(m[3], 64)
			h, _ := strconv.ParseFloat(m[4], 64)
			if x < 56 || x+w > 400.1 || y < 40 || y+h > 160.1 || h <= 0 {
				t.Errorf("%s: bar x=%v y=%v w=%v h=%v leaves the plot or has no height", name, x, y, w, h)
			}
		}
	}
}

func TestDailyChangeStripIsCappedToTheMostRecentPairedPointsAndKeepsVisibleBars(t *testing.T) {
	n := 5000
	nets, benches := make([]float64, n), make([]float64, n)
	for i := range nets {
		nets[i], benches[i] = 1000+float64(i%7), 100+float64(i%5)
	}
	body := execThemed(t, "", []account{seriesFixture(nets, benches)})
	bars := regexp.MustCompile(`<rect class="bar-account" x="([^"]*)" y="[^"]*" width="([^"]*)"`).FindAllStringSubmatch(body, -1)
	if len(bars) != stripCap {
		t.Fatalf("bars = %d, want the cap %d", len(bars), stripCap)
	}
	seen := map[string]bool{}
	for _, b := range bars {
		w, _ := strconv.ParseFloat(b[2], 64)
		if w < 1 {
			t.Fatalf("bar width %v is below the minimum of 1", w)
		}
		seen[b[1]] = true
	}
	if len(seen) != stripCap {
		t.Errorf("bars share x positions: %d distinct of %d", len(seen), stripCap)
	}
	if !strings.Contains(body, fmt.Sprintf("most recent %d", stripCap)) {
		t.Error("the card must state the cap")
	}
	if !strings.Contains(body, fmt.Sprintf("d%05d", n-1-stripCap)) {
		t.Error("the strip must start at the first paired point of the window")
	}
}

func TestDailyChangeStripPrintedStartDateEqualsItsScreenReaderLabel(t *testing.T) {
	body := execThemed(t, "", []account{pairedFixture(6)})
	m := regexp.MustCompile(`aria-label="[^"]*change between paired points from (\S+) to (\S+),`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("the strip label must say from <date> to <date>")
	}
	printed := regexp.MustCompile(`<text class="date" x="56.0" y="204">([^<]*)</text><text class="date" x="400.0" y="204" text-anchor="end">([^<]*)</text></svg>\n<p class="legend"><span><svg width="12"`).FindStringSubmatch(body)
	if len(printed) != 3 || printed[1] != m[1] || printed[2] != m[2] {
		t.Errorf("printed dates %v differ from the label's %v", printed, m[1:])
	}
}

func TestDailyChangeStripSaysItsBarsSpanPairedPointsNotDays(t *testing.T) {
	body := execThemed(t, "", []account{pairedFixture(6)})
	for _, want := range []string{"Change between paired points", "Account, left bar of each pair", "several sessions apart when a snapshot or a SPY close is missing"} {
		if !strings.Contains(body, want) {
			t.Errorf("strip lacks %q", want)
		}
	}
	if strings.Contains(body, "<h3>Daily change</h3>") || strings.Contains(body, "left bar of each day") {
		t.Error("the strip must not call a paired-point change a day")
	}
}
