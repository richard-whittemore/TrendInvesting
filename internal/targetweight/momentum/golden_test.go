package momentum_test

import (
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// goldenWeightTolerance bounds |Go weight - QuantConnect weight|: zero.
// Both sum the inverse volatilities in pick order and divide the same way,
// so the weights agree to the bit; a difference is a changed rule.
const goldenWeightTolerance = 0.0

// goldenMonths lists the momentum golden runs (docs/research/
// golden-cases-2026-10.md, "Runs"): each month's run, and its second part
// where the 10 KB log cap cut the first.
var goldenMonths = [][]string{
	{"IS_GOLD_2000_03", "IS_GOLD_2000_03_B"},
	{"IS_GOLD_2008_11"},
	{"IS_GOLD_2009_06"},
	{"IS_GOLD_2013_05"},
	{"OOS_GOLD_2016_02"},
	{"OOS_GOLD_2020_04"},
	{"OOS_GOLD_2021_03", "OOS_GOLD_2021_03_B"},
	{"OOS_GOLD_2022_06"},
	{"OOS_GOLD_2024_03"},
}

// goldenItem is one GOLDR entry: a ranked name as QuantConnect logged it.
type goldenItem struct {
	rank              int
	ticker, sector    string
	score, volatility float64
	mark              string // P picked, C sector-capped, - beyond the top 25
}

// goldenMonth is one month's GOLD lines, both parts joined.
type goldenMonth struct {
	header  string
	riskOff bool
	spy     float64
	sma     float64
	members int
	scored  int
	items   map[int]goldenItem
	weights map[string]float64
	short   map[string]int
}

// TestGoldenMonths replays QuantConnect's logged rebalances through
// SelectBook: from each month's logged scores, volatilities and sectors,
// the ranked order, the cap marks and the risk-off flag must match
// exactly, and the weights within goldenWeightTolerance (ADR 0026 point 7,
// items 1 and 2). The logs hold no closes, so Score is checked here only
// on the left-out names' close counts; the parity test checks it on
// QuantConnect's own closes.
//
// The logs are market data and stay outside the repository; the test
// reads them from TI_GOLDEN_DIR and skips without it, as in CI.
func TestGoldenMonths(t *testing.T) {
	dir := os.Getenv("TI_GOLDEN_DIR")
	if dir == "" {
		t.Skip("TI_GOLDEN_DIR is not set: the golden runs are kept outside the repository (docs/research/golden-cases-2026-10.md)")
	}
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		t.Skipf("TI_GOLDEN_DIR %s does not exist", dir)
	}
	cfg := momentum.FCombo()
	for _, runs := range goldenMonths {
		t.Run(runs[0], func(t *testing.T) {
			m := loadGoldenMonth(t, dir, runs)
			checkGoldenMonth(t, cfg, m)
		})
	}
}

func checkGoldenMonth(t *testing.T, cfg momentum.Config, m goldenMonth) {
	t.Helper()
	mustEqual(t, "risk-off flag", m.spy < m.sma, m.riskOff)

	cands := make([]momentum.Candidate, 0, len(m.items))
	for _, it := range m.items {
		cands = append(cands, momentum.Candidate{InstrumentID: it.ticker, Sector: it.sector, Score: it.score, Volatility: it.volatility})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].InstrumentID < cands[j].InstrumentID })
	book, err := momentum.SelectBook(cfg, cands, m.riskOff)
	if err != nil {
		t.Fatal(err)
	}
	marks := map[string]momentum.Status{"P": momentum.StatusPicked, "C": momentum.StatusSectorCapped, "-": momentum.StatusBeyondTopN}
	matched, picks, capped := 0, 0, 0
	for i, r := range book.Ranked {
		want := m.items[i+1]
		if r.InstrumentID != want.ticker || r.Status != marks[want.mark] || r.BacktestWouldPick != (want.mark == "P") {
			t.Errorf("rank %d: got %s %s, want %s %s", i+1, r.InstrumentID, r.Status, want.ticker, want.mark)
			continue
		}
		matched++
		switch want.mark {
		case "P":
			picks++
		case "C":
			capped++
		}
	}

	if len(book.Targets) != len(m.weights) {
		t.Errorf("targets = %d, logged %d", len(book.Targets), len(m.weights))
	}
	exact, maxDiff := 0, 0.0
	for _, tg := range book.Targets {
		want, ok := m.weights[tg.InstrumentID]
		if !ok {
			t.Errorf("target %s not logged", tg.InstrumentID)
			continue
		}
		diff := math.Abs(tg.Weight - want)
		if diff == 0 {
			exact++
		}
		maxDiff = math.Max(maxDiff, diff)
		if diff > goldenWeightTolerance {
			t.Errorf("%s weight %v, logged %v", tg.InstrumentID, tg.Weight, want)
		}
	}

	// The left-out names: as many as the scored count implies, each under
	// 253 closes, and Score leaves out a contiguous history that short.
	mustEqual(t, "left out", len(m.short), m.members-m.scored)
	cal := calendar(cfg.ScoreLookback + 1)
	for name, n := range m.short {
		h, err := momentum.Score(cfg, cal, series(cal, constant(n, 10)))
		if err != nil {
			t.Fatal(err)
		}
		if h.Status != momentum.StatusShortHistory || h.Closes != n {
			t.Errorf("%s with %d closes: %s", name, n, h.Status)
		}
	}
	t.Logf("risk_off=%v ranked %d/%d match (%d picked, %d capped); weights %d/%d exact, max |diff| %g; %d left out short",
		m.riskOff, matched, len(m.items), picks, capped, exact, len(m.weights), maxDiff, len(m.short))
}

func loadGoldenMonth(t *testing.T, dir string, runs []string) goldenMonth {
	t.Helper()
	m := goldenMonth{items: map[int]goldenItem{}, weights: map[string]float64{}, short: map[string]int{}}
	for _, run := range runs {
		raw, err := os.ReadFile(filepath.Join(dir, "golden_"+run+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		if err := json.Unmarshal(raw, &lines); err != nil {
			t.Fatal(err)
		}
		for _, line := range lines {
			parseGoldenLine(t, &m, line)
		}
	}
	if m.header == "" {
		t.Fatal("no GOLDH line")
	}
	for rank := 1; rank <= len(m.items); rank++ {
		if _, ok := m.items[rank]; !ok {
			t.Fatalf("rank %d missing: the parts are not contiguous", rank)
		}
	}
	return m
}

func parseGoldenLine(t *testing.T, m *goldenMonth, line string) {
	t.Helper()
	fields := strings.SplitN(line, " ", 5)
	if len(fields) < 5 || !strings.HasPrefix(fields[2], "GOLD") {
		return
	}
	kind, body := fields[2], fields[4]
	if !strings.HasSuffix(body, "|") {
		t.Fatalf("line cut short: %.60s", line)
	}
	body = strings.TrimSuffix(body, "|")
	switch kind {
	case "GOLDH":
		if m.header != "" && m.header != body {
			t.Fatalf("parts disagree on the header: %q, %q", m.header, body)
		}
		m.header = body
		kv := map[string]string{}
		for _, f := range strings.Fields(body) {
			k, v, _ := strings.Cut(f, "=")
			kv[k] = v
		}
		m.riskOff = kv["risk_off"] == "1"
		m.spy = parseFloat(t, kv["spy"])
		m.sma = parseFloat(t, kv["sma"])
		m.members = parseInt(t, kv["members"])
		m.scored = parseInt(t, kv["scored"])
	case "GOLDR":
		for _, item := range strings.Split(body, ";") {
			p := strings.Split(item, ":")
			if len(p) != 6 {
				t.Fatalf("GOLDR item %q", item)
			}
			it := goldenItem{rank: parseInt(t, p[0]), ticker: p[1], score: parseFloat(t, p[2]),
				volatility: parseFloat(t, p[3]), sector: p[4], mark: p[5]}
			if it.sector == "-" {
				it.sector = ""
			}
			if prev, ok := m.items[it.rank]; ok && prev != it {
				t.Fatalf("parts disagree on rank %d", it.rank)
			}
			m.items[it.rank] = it
		}
	case "GOLDW":
		for _, item := range strings.Split(body, ";") {
			name, w, _ := strings.Cut(item, ":")
			m.weights[name] = parseFloat(t, w)
		}
	case "GOLDS":
		for _, item := range strings.Split(body, ";") {
			name, n, _ := strings.Cut(item, ":")
			m.short[name] = parseInt(t, n)
		}
	}
}

func parseFloat(t *testing.T, s string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func parseInt(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
