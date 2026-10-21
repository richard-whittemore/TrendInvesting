package report_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// The benchmark side is a shadow account: it holds the first paired day's
// net liquidation in SPY at that day's close, mirrors every recorded deposit
// and withdrawal at the close of its New York day (the next session's close
// when that day has none), and reinvests SPY's dividends through the version
// 2 bar's split- and dividend-adjusted view. Both sides' percentages are
// time-weighted, so a transfer moves neither. Every figure below is
// synthetic.

const base = "2024-01-02"

// adjustedBar records a version 2 bar whose split-adjusted close is split and
// whose split- and dividend-adjusted close is adjusted, based at base.
func (s *stream) adjustedBar(n int, split, adjusted float64) {
	view := func(label string, c float64) event.PriceView {
		return event.PriceView{View: label, Open: c, High: c, Low: c, Close: c, Volume: 1}
	}
	s.add(event.CompletedBarEventType, event.CompletedBarAdjustedVersion, fmt.Sprintf("bar:%d", n), day(n),
		event.CompletedBarAdjustedPayload{
			CompletedBarPayload: event.CompletedBarPayload{InstrumentID: "SPY", PeriodEnd: day(n), SplitAdjusted: view(event.ViewSplitAdjusted, split), Raw: view(event.ViewRaw, split)},
			Adjusted:            view(event.ViewAdjusted, adjusted), AdjustedBase: base,
		})
}

// transfer is one ledger line as the Flex reader writes it.
type transfer struct {
	id     string
	asOf   time.Time
	amount float64
}

// ledgerOf is the ledger file holding the transfers, one record a line.
func ledgerOf(t *testing.T, transfers ...transfer) []byte {
	t.Helper()
	var b strings.Builder
	for _, tr := range transfers {
		stamp := tr.asOf.UTC().Format(time.RFC3339)
		line, err := json.Marshal(map[string]any{
			"id": "ibkr-flex:" + tr.id, "type": event.CashMovementEventType, "schema_version": event.CashMovementTransferVersion,
			"event_time": stamp, "source": "ibkr-flex",
			"payload": map[string]any{"as_of": stamp, "amount": tr.amount, "equity_before": 50000, "currency": "USD", "transaction_id": tr.id},
		})
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// startOfNewYorkDay is how the Flex reader dates a transfer IBKR gives by its
// day alone: 05:00 UTC, midnight in New York in January.
func startOfNewYorkDay(n int) time.Time {
	return time.Date(2024, time.January, 2+n, 5, 0, 0, 0, time.UTC)
}

func summarizeWith(t *testing.T, s *stream, ledger report.CashLedger, date string) report.Summary {
	t.Helper()
	got, err := report.SummarizeWithCash(report.History{}, ledger, s.out, date, "SPY")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func read(t *testing.T, raw []byte) report.CashLedger {
	t.Helper()
	ledger := report.ParseCashLedger(raw)
	if ledger.Unreadable != "" {
		t.Fatalf("ledger unreadable: %s", ledger.Unreadable)
	}
	return ledger
}

// depositSeries: SPY 100, 110, 110, 121 with no dividend; the account moves
// with it, and a deposit of 5000 lands on day 2.
func depositSeries(t *testing.T) *stream {
	s := newStream(t)
	s.adjustedBar(0, 100, 100)
	s.snapshot(0, 10000, 0)
	s.adjustedBar(1, 110, 110)
	s.snapshot(1, 11000, 0)
	s.adjustedBar(2, 110, 110)
	s.snapshot(2, 16000, 0)
	s.adjustedBar(3, 121, 121)
	s.snapshot(3, 17600, 0)
	return s
}

func TestADepositMidSeriesIsNotOutperformance(t *testing.T) {
	s := depositSeries(t)
	got := summarizeWith(t, s, read(t, ledgerOf(t, transfer{"d1", startOfNewYorkDay(2), 5000})), "2024-01-05").Equity
	// Account: 1.1 x (16000-5000)/11000 x 17600/16000 = 1.21; SPY 121/100.
	if !near(got.AccountReturn, 0.21) || !near(got.BenchmarkReturn, 0.21) || !near(got.Excess, 0) {
		t.Fatalf("returns account %v SPY %v excess %v: a deposit must not read as a gain", got.AccountReturn, got.BenchmarkReturn, got.Excess)
	}
	// The shadow held 100 shares (10000 at 100) and bought 5000/110 more on day 2.
	if p := got.Points[2]; !near(p.Shadow, 11000+5000) || !near(p.Transfers, 5000) {
		t.Fatalf("day 2 point %+v: the shadow buys the deposit's SPY shares at that day's close", p)
	}
	if !near(got.ShadowValue, (100+5000.0/110)*121) || !near(got.Gap, 17600-got.ShadowValue) {
		t.Fatalf("shadow %v gap %v", got.ShadowValue, got.Gap)
	}
	if got.Verdict != "Level with SPY since 2024-01-02" || got.VerdictTone != report.VerdictLevel {
		t.Fatalf("verdict %q tone %q", got.Verdict, got.VerdictTone)
	}
	if !near(got.Deposits, 5000) || got.Withdrawals != 0 || got.TransferCount != 1 {
		t.Fatalf("transfers in %v out %v count %d", got.Deposits, got.Withdrawals, got.TransferCount)
	}
	// Without the ledger the same deposit reads as the account's own gain:
	// the page says so rather than hiding it.
	bare := summarizeWith(t, s, report.CashLedger{}, "2024-01-05").Equity
	if !near(bare.AccountReturn, 0.76) || !strings.Contains(bare.Assumption, "deposits and withdrawals not yet recorded: the comparison assumes none") {
		t.Fatalf("without a ledger: return %v, assumption %q", bare.AccountReturn, bare.Assumption)
	}
}

func TestAWithdrawalMidSeriesIsNotUnderperformance(t *testing.T) {
	s := newStream(t)
	s.adjustedBar(0, 100, 100)
	s.snapshot(0, 10000, 0)
	s.adjustedBar(1, 110, 110)
	s.snapshot(1, 9000, 0) // 11000 less a withdrawal of 2000
	s.adjustedBar(2, 99, 99)
	s.snapshot(2, 8100, 0)
	got := summarizeWith(t, s, read(t, ledgerOf(t, transfer{"w1", day(1).Add(-3 * time.Hour), -2000})), "2024-01-04").Equity
	if !near(got.AccountReturn, -0.01) || !near(got.BenchmarkReturn, -0.01) || got.VerdictTone != report.VerdictLevel {
		t.Fatalf("account %v SPY %v tone %q: a withdrawal must not read as a loss", got.AccountReturn, got.BenchmarkReturn, got.VerdictTone)
	}
	if p := got.Points[1]; !near(p.Shadow, 9000) || !near(p.Transfers, -2000) {
		t.Fatalf("day 1 point %+v: the shadow sells the withdrawal's SPY shares", p)
	}
	if !near(got.ShadowValue, (100-2000.0/110)*99) || !near(got.Withdrawals, 2000) {
		t.Fatalf("shadow %v withdrawals %v", got.ShadowValue, got.Withdrawals)
	}
	if !got.DayAvailable || !near(got.DayAccountReturn, -0.1) {
		t.Fatalf("day %+v", got)
	}
	if !near(got.Points[1].AccountGrowth, 110) {
		t.Fatalf("growth %v: the chart line must not bend at the withdrawal", got.Points[1].AccountGrowth)
	}
}

func TestADividendExDateLiftsTheBenchmarkSide(t *testing.T) {
	s := newStream(t)
	s.adjustedBar(0, 100, 100)
	s.snapshot(0, 10000, 0)
	// Ex-date: the price falls by the 1.00 paid; reinvested, SPY's holder is level.
	s.adjustedBar(1, 99, 100)
	s.snapshot(1, 10000, 0)
	got := summarizeWith(t, s, read(t, nil), "2024-01-03").Equity
	if !near(got.BenchmarkReturn, 0) || !near(got.DayBenchmarkReturn, 0) || !near(got.ShadowValue, 10000) {
		t.Fatalf("SPY %v day %v shadow %v: the dividend must be reinvested", got.BenchmarkReturn, got.DayBenchmarkReturn, got.ShadowValue)
	}
	if got.Points[1].Benchmark != 99 {
		t.Fatalf("the quoted close stays the split-adjusted price: %v", got.Points[1].Benchmark)
	}
	if !strings.Contains(got.Assumption, "SPY with dividends reinvested") || strings.Contains(got.Assumption, "not counted") {
		t.Fatalf("assumption %q", got.Assumption)
	}
}

func TestTheVerdictSaysWhetherTheAccountIsBeatingSPY(t *testing.T) {
	for name, c := range map[string]struct {
		last          float64
		verdict, tone string
	}{
		"ahead":  {10250, "Beating SPY by 1.3 points since 2024-01-02", report.VerdictAhead},
		"behind": {10040, "Behind SPY by 0.8 points since 2024-01-02", report.VerdictBehind},
		"level":  {10125, "Level with SPY since 2024-01-02", report.VerdictLevel},
		"edge":   {10129, "Level with SPY since 2024-01-02", report.VerdictLevel},
		"beyond": {10132, "Beating SPY by 0.1 points since 2024-01-02", report.VerdictAhead},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStream(t)
			s.adjustedBar(0, 100, 100)
			s.snapshot(0, 10000, 0)
			s.adjustedBar(1, 101.2, 101.2)
			s.snapshot(1, c.last, 0)
			got := summarizeWith(t, s, read(t, nil), "2024-01-03").Equity
			if got.Verdict != c.verdict || got.VerdictTone != c.tone {
				t.Fatalf("verdict %q tone %q, want %q %q (excess %v)", got.Verdict, got.VerdictTone, c.verdict, c.tone, got.Excess)
			}
			if !near(got.Gap, c.last-10120) {
				t.Fatalf("gap %v", got.Gap)
			}
		})
	}
}

func TestTheAssumptionNamesALedgerThatIsAbsentOrUnreadable(t *testing.T) {
	missing := report.LoadCashLedger("/flex/boost/cash-movements.jsonl", func(string) ([]byte, error) { return nil, fs.ErrNotExist })
	if missing.Read || missing.Unreadable != "" {
		t.Fatalf("an absent ledger is not read and not a fault: %+v", missing)
	}
	if got := report.LoadCashLedger("/flex/boost/cash-movements.jsonl", func(string) ([]byte, error) {
		return ledgerOf(t, transfer{"d1", startOfNewYorkDay(2), 5000}), nil
	}); !got.Read || len(got.Movements) != 1 {
		t.Fatalf("a ledger read whole: %+v", got)
	}
	if report.VerdictLine(report.Equity{}, "SPY") != "" {
		t.Fatal("no verdict before two paired points")
	}
	if got := report.LoadCashLedger("", nil); got.Read {
		t.Fatalf("no ledger named: %+v", got)
	}
	broken := report.LoadCashLedger("/flex/boost/cash-movements.jsonl", func(string) ([]byte, error) { return nil, errors.New("permission denied") })
	if broken.Unreadable == "" || strings.Contains(broken.Unreadable, "/flex/") {
		t.Fatalf("an unreadable ledger says why, without the server's path: %+v", broken)
	}
	s := depositSeries(t)
	for name, c := range map[string]struct {
		ledger report.CashLedger
		want   string
	}{
		"absent":     {missing, "deposits and withdrawals not yet recorded: the comparison assumes none"},
		"unreadable": {report.ParseCashLedger([]byte("{\n")), "deposits and withdrawals not readable (ledger line 1 is not a cash movement as the Flex reader writes it): the comparison assumes none"},
		"read":       {read(t, ledgerOf(t, transfer{"d1", startOfNewYorkDay(2), 5000})), "your deposits and withdrawals mirrored as recorded by the Flex reader"},
	} {
		got := summarizeWith(t, s, c.ledger, "2024-01-05").Equity
		if !strings.Contains(got.Assumption, c.want) {
			t.Errorf("%s: assumption %q lacks %q", name, got.Assumption, c.want)
		}
	}
}

func TestTheLedgerReadsOnlyTheFlexReadersCashMovements(t *testing.T) {
	good := ledgerOf(t, transfer{"d1", startOfNewYorkDay(2), 5000})
	cases := map[string][]byte{
		"not json":         []byte("nope\n"),
		"no final newline": good[:len(good)-1],
		"version 1":        []byte(strings.Replace(string(good), `"schema_version":2`, `"schema_version":1`, 1)),
		"another type":     []byte(strings.Replace(string(good), event.CashMovementEventType, "account.snapshot", 1)),
		"no transaction":   []byte(strings.Replace(string(good), `"transaction_id":"d1"`, `"transaction_id":""`, 1)),
		"twice":            append(append([]byte{}, good...), good...),
	}
	for name, raw := range cases {
		if got := report.ParseCashLedger(raw); got.Unreadable == "" || len(got.Movements) != 0 {
			t.Errorf("%s: %+v, want the ledger refused whole", name, got)
		}
	}
	if got := report.ParseCashLedger(good); !got.Read || len(got.Movements) != 1 || got.Movements[0].Amount != 5000 {
		t.Fatalf("good ledger: %+v", got)
	}
	if got := report.ParseCashLedger(nil); !got.Read || len(got.Movements) != 0 {
		t.Fatalf("an empty ledger is read and holds nothing: %+v", got)
	}
}

func TestAVersion1SeriesSaysDividendsAreNotCounted(t *testing.T) {
	s := newStream(t)
	s.bar(0, "SPY", 100)
	s.snapshot(0, 10000, 0)
	s.bar(1, "SPY", 99)
	s.snapshot(1, 10000, 0)
	got := summarizeWith(t, s, read(t, nil), "2024-01-03").Equity
	if !near(got.BenchmarkReturn, -0.01) || !strings.Contains(got.Assumption, "SPY price return only: dividends not counted") {
		t.Fatalf("SPY %v assumption %q", got.BenchmarkReturn, got.Assumption)
	}
	mixed := newStream(t)
	mixed.bar(0, "SPY", 100)
	mixed.snapshot(0, 10000, 0)
	mixed.bar(1, "SPY", 99)
	mixed.snapshot(1, 10000, 0)
	mixed.adjustedBar(2, 99, 99)
	mixed.snapshot(2, 10000, 0)
	mixed.adjustedBar(3, 98, 99)
	mixed.snapshot(3, 10000, 0)
	got = summarizeWith(t, mixed, read(t, nil), "2024-01-05").Equity
	if !near(got.BenchmarkReturn, -0.01) {
		t.Fatalf("mixed SPY %v: the dividend on day 3 is reinvested, the v1 days are price only", got.BenchmarkReturn)
	}
	if !strings.Contains(got.Assumption, "SPY with dividends reinvested from 2024-01-04; price return only before 2024-01-04") {
		t.Fatalf("mixed assumption %q", got.Assumption)
	}
}

func TestATransferIsMirroredAtTheCloseOfItsNewYorkDayOrTheNextSession(t *testing.T) {
	s := newStream(t)
	s.adjustedBar(0, 100, 100)
	s.snapshot(0, 10000, 0)
	s.adjustedBar(1, 110, 110)
	s.snapshot(1, 12000, 0) // 11000 and the evening deposit
	// No session on day 2 or 3: no bar, no snapshot.
	s.adjustedBar(4, 120, 120)
	s.snapshot(4, 12000*120.0/110+2000, 0) // the account holds SPY too
	got := summarizeWith(t, s, read(t, ledgerOf(t,
		// 20:00 New York on day 1 is 01:00 UTC on day 2: still day 1's close.
		transfer{"evening", time.Date(2024, time.January, 4, 1, 0, 0, 0, time.UTC), 1000},
		// Day 3 has no session: the next session's close, day 4.
		transfer{"weekend", startOfNewYorkDay(3), 2000},
	)), "2024-01-06").Equity
	if !near(got.Points[1].Transfers, 1000) || !near(got.Points[2].Transfers, 2000) {
		t.Fatalf("points %+v", got.Points)
	}
	want := (100+1000.0/110)*120 + 2000
	if !near(got.ShadowValue, want) || !near(got.AccountReturn, 0.2) || got.VerdictTone != report.VerdictLevel {
		t.Fatalf("shadow %v (want %v) account %v tone %q", got.ShadowValue, want, got.AccountReturn, got.VerdictTone)
	}
	// A transfer on or before the first paired day is in the starting balance.
	early := summarizeWith(t, s, read(t, ledgerOf(t, transfer{"early", startOfNewYorkDay(0), 700})), "2024-01-06").Equity
	if early.TransferCount != 0 || !near(early.Points[0].Shadow, 10000) {
		t.Fatalf("early transfer counted: %+v", early)
	}
}

func TestTheDailySummaryCarriesTheVerdictAndWhatIsCounted(t *testing.T) {
	s := depositSeries(t)
	got := summarizeWith(t, s, read(t, ledgerOf(t, transfer{"d1", startOfNewYorkDay(2), 5000})), "2024-01-05")
	text := report.RenderText(got)
	for _, want := range []string{
		"Level with SPY since 2024-01-02; dollar gap +0.00 (account 17600.00, SPY shadow 17600.00)",
		"time-weighted", "your deposits and withdrawals mirrored as recorded by the Flex reader",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("summary lacks %q:\n%s", want, text)
		}
	}
}

// TestTheManualsWorkedExample is the alerts and dashboard chapter's worked
// example of one deposit and one dividend, figure for figure (its dates
// there are other synthetic ones).
func TestTheManualsWorkedExample(t *testing.T) {
	s := newStream(t)
	s.adjustedBar(0, 100, 100)
	s.snapshot(0, 10000, 0)
	s.adjustedBar(1, 110, 110)
	s.snapshot(1, 11000, 0)
	s.adjustedBar(2, 110, 110)
	s.snapshot(2, 16000, 0)    // the deposit of 5000
	s.adjustedBar(3, 109, 110) // ex-dividend: 1.00 paid, reinvested
	s.snapshot(3, 16000, 0)
	got := summarizeWith(t, s, read(t, ledgerOf(t, transfer{"d1", startOfNewYorkDay(2), 5000})), "2024-01-05").Equity
	if !near(got.AccountReturn, 0.10) || !near(got.BenchmarkReturn, 0.10) || got.Verdict != "Level with SPY since 2024-01-02" {
		t.Fatalf("account %v SPY %v verdict %q", got.AccountReturn, got.BenchmarkReturn, got.Verdict)
	}
	if !near(got.Points[2].Shadow, 16000) || !near(got.ShadowValue, 16000) || report.SignedMoney(got.Gap) != "+0.00" {
		t.Fatalf("shadow %v then %v, gap %v", got.Points[2].Shadow, got.ShadowValue, got.Gap)
	}
	priceOnly := newStream(t)
	priceOnly.bar(0, "SPY", 100)
	priceOnly.snapshot(0, 10000, 0)
	priceOnly.bar(1, "SPY", 110)
	priceOnly.snapshot(1, 11000, 0)
	priceOnly.bar(2, "SPY", 110)
	priceOnly.snapshot(2, 16000, 0)
	priceOnly.bar(3, "SPY", 109)
	priceOnly.snapshot(3, 16000, 0)
	if v := summarizeWith(t, priceOnly, read(t, ledgerOf(t, transfer{"d1", startOfNewYorkDay(2), 5000})), "2024-01-05").Equity.Verdict; v != "Beating SPY by 1.0 points since 2024-01-02" {
		t.Fatalf("price only: %q", v)
	}
	if v := summarizeWith(t, priceOnly, report.CashLedger{}, "2024-01-04").Equity.Verdict; v != "Beating SPY by 50.0 points since 2024-01-02" {
		t.Fatalf("deposit counted as a gain: %q", v)
	}
}
