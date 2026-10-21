package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// shadowFixture is a synthetic account that receives a deposit of 500 on its
// third paired day: its net liquidation jumps, its time-weighted growth does
// not, and the shadow account buys SPY for the same amount.
func shadowFixture(tone string) account {
	a := pairedFixture(0)
	eq := &a.Summary.Equity
	eq.Points = []report.EquityPoint{
		{Date: "2024-01-02", Net: 1000, Benchmark: 100, Shadow: 1000, AccountGrowth: 100, BenchmarkGrowth: 100},
		{Date: "2024-01-03", Net: 1010, Benchmark: 101, Shadow: 1010, AccountGrowth: 101, BenchmarkGrowth: 101},
		{Date: "2024-01-04", Net: 1530, Benchmark: 102, Shadow: 1520, Transfers: 500, AccountGrowth: 102.5, BenchmarkGrowth: 102},
	}
	eq.Available, eq.StartDate, eq.EndDate = true, "2024-01-02", "2024-01-04"
	eq.AccountReturn, eq.BenchmarkReturn, eq.Excess = 0.025, 0.02, 0.005
	eq.DayAvailable, eq.DayStartDate, eq.DayAccountReturn, eq.DayBenchmarkReturn = true, "2024-01-03", 0.0149, 0.0099
	eq.ShadowValue, eq.Gap = 1520, 10
	eq.Verdict, eq.VerdictTone = "Beating SPY by 0.5 points since 2024-01-02", tone
	eq.TransferCount, eq.Deposits = 1, 500
	eq.Assumption = "Counted: SPY with dividends reinvested; your deposits and withdrawals mirrored as recorded by the Flex reader (1 in this period: 500.00 in, 0.00 out). Both returns are time-weighted; fees and interest stay in the account's own result."
	a.Summary.Date = "2024-01-04"
	return a
}

func TestTheEquityCardOpensWithTheVerdictInWordsAndColour(t *testing.T) {
	for tone, class := range map[string]string{report.VerdictAhead: "verdict pos", report.VerdictBehind: "verdict neg", report.VerdictLevel: "verdict level"} {
		body := execThemed(t, "", []account{shadowFixture(tone)})
		verdict := strings.Index(body, `<p class="`+class+`">`)
		card := strings.Index(body, "<h3>Equity against SPY</h3>")
		if verdict < 0 || verdict > card {
			t.Fatalf("%s: the verdict must open the equity card with class %q:\n%s", tone, class, body)
		}
		line, _, _ := strings.Cut(body[verdict:], "</p>")
		if !strings.Contains(line, "Beating SPY by 0.5 points since 2024-01-02") || !strings.Contains(line, "dollar gap &#43;10.00") {
			t.Errorf("%s: verdict line %q lacks the words or the dollar gap", tone, line)
		}
	}
}

func TestTheEquityCardShowsTheShadowAccountInDollarsAndWhatIsCounted(t *testing.T) {
	body := execThemed(t, "", []account{shadowFixture(report.VerdictAhead)})
	for _, want := range []string{
		"Account 1530.00 &middot; SPY shadow account 1520.00 &middot; gap &#43;10.00 on 2024-01-04",
		"your deposits and withdrawals mirrored as recorded by the Flex reader",
		"time-weighted",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("equity card lacks %q", want)
		}
	}
	if strings.Contains(body, "price return only, no deposits or withdrawals") {
		t.Error("the old assumption is still stated")
	}
}

func TestTheChartDrawsGrowthSoADepositDoesNotBendIt(t *testing.T) {
	v := equityChart(shadowFixture(report.VerdictAhead).Summary)
	if v.AccountEnd != "102.50" || v.BenchmarkEnd != "102.00" {
		t.Fatalf("ends %s and %s: the chart must draw time-weighted growth, not net liquidation", v.AccountEnd, v.BenchmarkEnd)
	}
	if !strings.Contains(v.Label, "time-weighted") {
		t.Errorf("label %q must say the lines are time-weighted", v.Label)
	}
	strip := dailyChangeStrip(shadowFixture(report.VerdictAhead).Summary)
	if len(strip.Bars) != 2 || strip.TopLabel != "+1.49%" {
		t.Fatalf("strip %+v: the change after the deposit is its time-weighted growth, not the jump", strip)
	}
}

// writeLedger writes a Flex ledger holding one deposit, as the reader writes it.
func writeLedger(t *testing.T, dir string, asOf time.Time, amount float64) string {
	t.Helper()
	stamp := asOf.UTC().Format(time.RFC3339)
	line, err := json.Marshal(map[string]any{"id": "ibkr-flex:t1", "type": event.CashMovementEventType, "schema_version": event.CashMovementTransferVersion,
		"event_time": stamp, "source": "ibkr-flex",
		"payload": map[string]any{"as_of": stamp, "amount": amount, "equity_before": 4000, "currency": "USD", "transaction_id": "t1"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cash-movements.jsonl")
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheDashboardReadsTheFlexLedgerForEachEngineDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "engines", "boost")
	flex := filepath.Join(root, "flex", "boost")
	for _, d := range []string{dir, flex} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeRun(t, dir, "run1.log", approval("", ""), 0, time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC))
	// Net 4000 then 4010 with SPY 100 then 101: a deposit of 10 on day 1
	// leaves the account level, one point behind SPY.
	ledger := writeLedger(t, flex, at(1).Add(-2*time.Hour), 10)
	// The reader's last reading covered the day before the page's day.
	if err := os.WriteFile(ledger+".status.json", []byte(`{"covered_to":"20240102","state":"read"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHandlerWith(source{dirs: []string{dir}, ledgers: map[string]string{dir: ledger}}, "SPY", os.ReadFile)
	got := summaryOf(t, h).Summary.Equity
	if got.Verdict != "Behind SPY by 1.0 point since "+at(0).Format(time.DateOnly) || got.TransferCount != 1 || !near(got.ShadowValue, 4040+10) {
		t.Fatalf("with the ledger: %+v", got)
	}
	// The JSON carries the shadow, the time-weighted returns and the verdict.
	raw := get(h, http.MethodGet, "/api/summary.json").Body.String()
	for _, key := range []string{`"ShadowValue":`, `"Verdict":"Behind SPY by 1.0 point since`, `"VerdictTone":"behind"`, `"AccountReturn":0`, `"Gap":`, `"Assumption":"Counted: `} {
		if !strings.Contains(raw, key) {
			t.Errorf("summary.json lacks %s:\n%s", key, raw)
		}
	}
	page := get(h, http.MethodGet, "/").Body.String()
	if !strings.Contains(page, "mirrored as recorded by the Flex reader (1 in this period: 10.00 in, 0.00 out)") {
		t.Errorf("the page does not state the transfer it mirrored")
	}
	if !strings.Contains(page, "transfers recorded through 2024-01-02; deposits or withdrawals from the last two days may not be recorded yet; a transfer appears in the summary two evenings after it posts") {
		t.Errorf("the page does not say how far the Flex reader has recorded:\n%s", page)
	}
	// No ledger yet: the page says the comparison assumes none.
	h = newHandlerWith(source{dirs: []string{dir}, ledgers: map[string]string{dir: filepath.Join(flex, "absent.jsonl")}}, "SPY", os.ReadFile)
	if body := get(h, http.MethodGet, "/").Body.String(); !strings.Contains(body, "deposits and withdrawals not yet recorded: the comparison assumes none") {
		t.Errorf("an absent ledger must be named on the page")
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func TestTheCashLedgerFlagNamesTheAccountItBelongsTo(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	got, err := ledgersOf([]string{"/flex/boost/cash-movements.jsonl"}, nil, []string{one})
	if err != nil || got[one] != "/flex/boost/cash-movements.jsonl" {
		t.Fatalf("one source and a bare path: %v, %v", got, err)
	}
	got, err = ledgersOf([]string{two + "=/flex/ira/cash-movements.jsonl"}, []string{"j.journal"}, []string{two})
	if err != nil || got[two] != "/flex/ira/cash-movements.jsonl" || got["j.journal"] != "" {
		t.Fatalf("named source: %v, %v", got, err)
	}
	got, err = ledgersOf([]string{"j.journal=/flex/ira/cash-movements.jsonl"}, []string{"j.journal"}, nil)
	if err != nil || got["j.journal"] != "/flex/ira/cash-movements.jsonl" {
		t.Fatalf("a journal named: %v, %v", got, err)
	}
	for name, c := range map[string]struct{ values, journals, dirs []string }{
		"bare path, two sources": {[]string{"/flex/a.jsonl"}, []string{"j.journal"}, []string{one}},
		"unknown source":         {[]string{"/elsewhere=/flex/a.jsonl"}, nil, []string{one}},
		"twice for one source":   {[]string{one + "=/a", one + "=/b"}, nil, []string{one}},
		"no path":                {[]string{one + "="}, nil, []string{one}},
		"bare path, one journal": {[]string{"/flex/a/cash-movements.jsonl"}, []string{"j.journal"}, nil},
	} {
		if _, err := ledgersOf(c.values, c.journals, c.dirs); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if err := run([]string{"-journal", "a", "-journal", "b", "-cash-ledger", "/flex/x.jsonl"}, &bytes.Buffer{}); err == nil {
		t.Error("run must refuse a ledger it cannot place")
	}
}

// A ledger is accepted only from its account's own Flex folder: flex/<instance>
// for the engine directory engines/<instance>, or for a journal in it.
func TestALedgerFromAnotherAccountsFolderIsNotMirrored(t *testing.T) {
	root := t.TempDir()
	trad := filepath.Join(root, "engines", "trad-ira")
	roth := filepath.Join(root, "flex", "roth-ira")
	for _, d := range []string{trad, roth} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeRun(t, trad, "run1.log", approval("", ""), 0, time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC))
	journal := filepath.Join(trad, "run1.log")
	ledger := writeLedger(t, roth, at(1).Add(-2*time.Hour), 10)
	for name, src := range map[string]source{
		"engine directory": {dirs: []string{trad}, ledgers: map[string]string{trad: ledger}},
		"journal":          {journals: []string{journal}, ledgers: map[string]string{journal: ledger}},
	} {
		h := newHandlerWith(src, "SPY", os.ReadFile)
		got := summaryOf(t, h).Summary.Equity
		if got.TransferCount != 0 || !strings.Contains(got.Assumption, "ledger not accepted: it belongs to roth-ira, not trad-ira") {
			t.Errorf("%s: transfers %d, assumption %q", name, got.TransferCount, got.Assumption)
		}
		if page := get(h, http.MethodGet, "/").Body.String(); !strings.Contains(page, "ledger not accepted: it belongs to roth-ira, not trad-ira") {
			t.Errorf("%s: the page does not say the ledger was not accepted", name)
		}
	}
}

func TestWithheldDollarFiguresKeepTheVerdictAndShowTheReason(t *testing.T) {
	a := shadowFixture(report.VerdictAhead)
	a.Summary.Equity.DollarsWithheld = true
	a.Summary.Equity.DollarNote = "the recorded withdrawals exceeded the SPY shadow's value on 2024-01-03; dollar figures are not meaningful after that day"
	a.Summary.Equity.ShadowValue, a.Summary.Equity.Gap = 0, 1530
	body := execThemed(t, "", []account{a})
	if !strings.Contains(body, "Beating SPY by 0.5 points") || !strings.Contains(body, "exceeded the SPY shadow&#39;s value on 2024-01-03; dollar figures are not meaningful after that day") {
		t.Fatalf("the verdict and the reason are both shown:\n%s", body)
	}
	if strings.Contains(body, "dollar gap") || strings.Contains(body, "&middot; gap") {
		t.Error("withheld dollar figures show no gap")
	}
}
