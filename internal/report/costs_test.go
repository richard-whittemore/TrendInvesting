package report_test

import (
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// The costs block totals what the account paid in commissions, fees and
// broker interest from the Flex reader's costs ledger and the commissions
// the journal holds on fills, without counting a commission twice. Every
// figure below is synthetic.

// cost is one costs-ledger line as the Flex reader writes it: amount is as
// IBKR states it, negative for money out.
type cost struct {
	id     string
	kind   string
	asOf   time.Time
	amount float64
}

func costLedgerOf(t *testing.T, costs ...cost) []byte {
	t.Helper()
	var b strings.Builder
	for _, c := range costs {
		stamp := c.asOf.UTC().Format(time.RFC3339)
		line, err := json.Marshal(map[string]any{
			"id": "ibkr-flex:" + c.id, "type": event.CostEventType, "schema_version": event.CostSchemaVersion,
			"event_time": stamp, "source": "ibkr-flex",
			"payload": map[string]any{"kind": c.kind, "amount": c.amount, "currency": "USD", "as_of": stamp, "transaction_id": c.id},
		})
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func readCosts(t *testing.T, costs ...cost) report.CostLedger {
	t.Helper()
	ledger := report.ParseCostLedger(costLedgerOf(t, costs...))
	if ledger.Unreadable != "" {
		t.Fatalf("costs ledger unreadable: %s", ledger.Unreadable)
	}
	return ledger
}

// fillWithCommission records a target-weight fill whose execution ID is
// execID and whose commission is commission, on day n.
func (s *stream) fillWithCommission(n int, execID string, commission float64) {
	s.add(event.FillEventType, event.FillTargetWeightVersion, "fill:"+execID, day(n),
		event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "tw-1", FillID: execID, InstrumentID: "SPY",
			Side: event.OrderSideBuy, Quantity: 10, Price: 100, Commission: commission, FilledAt: day(n)})
}

// fillAt records a target-weight fill whose execution ID is execID and whose
// commission is commission, filled and recorded at at.
func (s *stream) fillAt(at time.Time, execID string, commission float64) {
	s.add(event.FillEventType, event.FillTargetWeightVersion, "fill:"+execID, at,
		event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "tw-1", FillID: execID, InstrumentID: "SPY",
			Side: event.OrderSideBuy, Quantity: 10, Price: 100, Commission: commission, FilledAt: at})
}

func summarizeCosts(t *testing.T, s *stream, costs report.CostLedger, date string) report.Summary {
	t.Helper()
	got, err := report.SummarizeWithCosts(report.History{}, report.CashLedger{}, costs, s.out, date, "SPY")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// at is the given January 2024 day at noon New York time, as the Flex
// reader dates a posting with a time.
func at(dayOfMonth int) time.Time {
	return time.Date(2024, time.January, dayOfMonth, 17, 0, 0, 0, time.UTC)
}

func TestCostLedgerReadsEveryKindStrictly(t *testing.T) {
	ledger := readCosts(t,
		cost{"c1", event.CostCommission, at(3), -1},
		cost{"c2", event.CostFee, at(3), -10},
		cost{"c3", event.CostInterestPaid, at(3), -3.21},
		cost{"c4", event.CostInterestReceived, at(3), 0.42})
	if !ledger.Read || len(ledger.Postings) != 4 || ledger.Postings[2].Kind != event.CostInterestPaid || ledger.Postings[2].Amount != -3.21 {
		t.Fatalf("ledger %+v", ledger)
	}
	good := string(costLedgerOf(t, cost{"c1", event.CostFee, at(3), -10}))
	for name, raw := range map[string]string{
		"no whole last line": strings.TrimSuffix(good, "\n"),
		"unknown kind":       strings.Replace(good, `"fee"`, `"dividend"`, 1),
		"unknown field":      strings.Replace(good, `"currency"`, `"extra":1,"currency"`, 1),
		"a cash movement":    strings.Replace(good, event.CostEventType, event.CashMovementEventType, 1),
		"id not the txn":     strings.Replace(good, `"id":"ibkr-flex:c1"`, `"id":"ibkr-flex:zz"`, 1),
		"zero amount":        strings.Replace(good, `"amount":-10`, `"amount":0`, 1),
		"twice":              good + good,
		"other source":       strings.Replace(good, `"source":"ibkr-flex"`, `"source":"x"`, 1),
	} {
		if got := report.ParseCostLedger([]byte(raw)); got.Read || got.Unreadable == "" {
			t.Errorf("%s: ledger read: %+v", name, got)
		}
	}
	if got := report.ParseCostLedger(nil); !got.Read || len(got.Postings) != 0 {
		t.Errorf("an empty file is a ledger read and empty: %+v", got)
	}
}

func TestCostLedgerIsLoadedBesideTheCashLedgerFromTheAccountsOwnFolder(t *testing.T) {
	files := map[string][]byte{
		"/var/lib/flex/boost/costs.jsonl":             costLedgerOf(t, cost{"c1", event.CostFee, at(3), -10}),
		"/var/lib/flex/boost/costs.jsonl.status.json": []byte(`{"state":"read","covered_to":"20240104"}`),
	}
	readFile := func(path string) ([]byte, error) {
		if raw, ok := files[path]; ok {
			return raw, nil
		}
		return nil, fs.ErrNotExist
	}
	got := report.LoadCostLedgerFor("/var/lib/flex/boost/cash-movements.jsonl", "boost", readFile)
	if !got.Read || len(got.Postings) != 1 || got.CoveredTo != "2024-01-04" {
		t.Fatalf("ledger %+v", got)
	}
	if got := report.LoadCostLedgerFor("/var/lib/flex/boost/cash-movements.jsonl", "roth-ira", readFile); got.Read || got.NotAccepted == "" || len(got.Postings) != 0 {
		t.Errorf("another account's ledger must not be read: %+v", got)
	}
	if got := report.LoadCostLedgerFor("", "boost", readFile); got.Read || got.Unreadable != "" || got.NotAccepted != "" {
		t.Errorf("no ledger named: %+v", got)
	}
	if got := report.LoadCostLedgerFor("/var/lib/flex/trad-ira/cash-movements.jsonl", "trad-ira", readFile); got.Read {
		t.Errorf("no costs file yet is not a ledger: %+v", got)
	}
	files["/var/lib/flex/trad-ira/costs.jsonl.status.json"] = []byte(`{"state":"read","covered_to":"20240104"}`)
	if got := report.LoadCostLedgerFor("/var/lib/flex/trad-ira/cash-movements.jsonl", "trad-ira", readFile); !got.Read || got.CoveredTo != "2024-01-04" {
		t.Errorf("a reading that found no cost is a ledger read and empty: %+v", got)
	}
	failing := func(string) ([]byte, error) {
		return nil, &fs.PathError{Op: "read", Path: "/secret/path", Err: errors.New("disk on fire")}
	}
	if got := report.LoadCostLedgerFor("/var/lib/flex/boost/cash-movements.jsonl", "boost", failing); got.Unreadable == "" || strings.Contains(got.Unreadable, "/secret") {
		t.Errorf("unreadable ledger %+v", got)
	}
}

// costSeries: four sessions, 2024-01-02 to 2024-01-05, the account at 10000.
func costSeries(t *testing.T) *stream {
	s := newStream(t)
	for n, net := range []float64{10000, 10000, 10000, 10000} {
		s.bar(n, "SPY", 100)
		s.snapshot(n, net, 0)
	}
	return s
}

func TestTotalsPerKindForTheMonthTheYearAndSinceTheStart(t *testing.T) {
	s := costSeries(t)
	ledger := readCosts(t,
		cost{"old", event.CostFee, time.Date(2023, time.December, 29, 17, 0, 0, 0, time.UTC), -7},
		cost{"c1", event.CostCommission, at(3), -1},
		cost{"c2", event.CostFee, at(3), -10},
		cost{"c3", event.CostInterestPaid, at(4), -3.21},
		cost{"c4", event.CostInterestReceived, at(4), 0.42},
		cost{"later", event.CostFee, at(10), -99})
	got := summarizeCosts(t, s, ledger, "2024-01-05").Costs
	want := report.CostTotals{Commissions: 1, Fees: 10, InterestPaid: 3.21, InterestReceived: 0.42}
	if got.Month != want || got.Year != want {
		t.Errorf("month %+v year %+v, want %+v", got.Month, got.Year, want)
	}
	if !near(got.Since.Fees, 17) || got.Since.Commissions != 1 {
		t.Errorf("since the start %+v: the December fee belongs there and the later one nowhere", got.Since)
	}
	if !near(want.Net(), 1+10+3.21-0.42) {
		t.Errorf("net %v", want.Net())
	}
	if got.MonthStart != "2024-01-01" || got.YearStart != "2024-01-01" {
		t.Errorf("periods %q %q", got.MonthStart, got.YearStart)
	}
}

func TestAPastDaysCostsDoNotChangeWhenTheLedgerGrows(t *testing.T) {
	s := costSeries(t)
	ledger := readCosts(t, cost{"c1", event.CostFee, at(3), -10}, cost{"c2", event.CostFee, at(5), -5})
	if got := summarizeCosts(t, s, ledger, "2024-01-04").Costs; got.Since.Fees != 10 {
		t.Errorf("fees %v", got.Since.Fees)
	}
}

func TestYearToDateLeavesOutLastYearsCostsButMonthToDateLeavesOutLastMonths(t *testing.T) {
	s := newStream(t)
	s.bar(0, "SPY", 100)
	s.snapshot(0, 10000, 0)
	ledger := readCosts(t,
		cost{"dec", event.CostFee, time.Date(2023, time.December, 29, 17, 0, 0, 0, time.UTC), -7},
		cost{"jan", event.CostFee, at(2), -2})
	got := summarizeCosts(t, s, ledger, "2024-01-02").Costs
	if got.Month.Fees != 2 || got.Year.Fees != 2 || got.Since.Fees != 9 {
		t.Errorf("month %v year %v since %v", got.Month.Fees, got.Year.Fees, got.Since.Fees)
	}
}

func TestACommissionAlreadyOnAFillIsNotCountedTwice(t *testing.T) {
	s := costSeries(t)
	s.fillWithCommission(1, "0001f4e8.65a1b2c3.01.01", 1.25)
	ledger := readCosts(t,
		// The same execution, restated by IBKR as its commission posting.
		cost{"0001f4e8.65a1b2c3.01.01", event.CostCommission, at(3), -1.25},
		// A correction of the execution keeps its base.
		cost{"0001f4e8.65a1b2c3.01.02", event.CostCommission, at(3), -0.10},
		// A commission the journal holds no fill for.
		cost{"900000106", event.CostCommission, at(4), -2})
	got := summarizeCosts(t, s, ledger, "2024-01-05").Costs
	if !near(got.Since.Commissions, 1.25+2) {
		t.Errorf("commissions %v, want the fill's 1.25 and the unmatched 2 once", got.Since.Commissions)
	}
	if got.MatchedCommissions != 2 {
		t.Errorf("matched %d", got.MatchedCommissions)
	}
}

func TestCommissionsOnFillsCountWithNoLedgerAtAll(t *testing.T) {
	s := costSeries(t)
	s.fillWithCommission(2, "e1.01", 1.25)
	got := summarizeCosts(t, s, report.CostLedger{}, "2024-01-05").Costs
	if got.Since.Commissions != 1.25 || got.Month.Commissions != 1.25 {
		t.Fatalf("costs %+v", got)
	}
	if !strings.Contains(got.Note, "not yet recorded") {
		t.Errorf("note %q must say fees and interest are not yet recorded", got.Note)
	}
}

func TestEarlierRunsFillsCountSinceTheStart(t *testing.T) {
	earlier := costSeries(t)
	earlier.fillWithCommission(0, "e0.01", 3)
	current := newStream(t)
	current.bar(4, "SPY", 100)
	current.snapshot(4, 10000, 0)
	current.fillWithCommission(4, "e4.01", 1)
	got, err := report.SummarizeWithCosts(report.History{Earlier: earlier.out}, report.CashLedger{}, report.CostLedger{}, current.out, "2024-01-06", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.Costs.Since.Commissions != 4 {
		t.Errorf("commissions %v, want 3 from the earlier run and 1 from this one", got.Costs.Since.Commissions)
	}
}

func TestCostsAsAShareOfAverageNetLiquidation(t *testing.T) {
	s := newStream(t)
	for n, net := range []float64{10000, 20000, 30000} {
		s.bar(n, "SPY", 100)
		s.snapshot(n, net, 0)
	}
	ledger := readCosts(t, cost{"c1", event.CostFee, at(3), -100}, cost{"c2", event.CostInterestReceived, at(3), 20})
	got := summarizeCosts(t, s, ledger, "2024-01-04").Costs
	if !near(got.AverageNetLiquidation, 20000) || !got.ShareAvailable || !near(got.Share, 80.0/20000) {
		t.Errorf("average %v share %v (%v)", got.AverageNetLiquidation, got.Share, got.ShareAvailable)
	}
	if empty := summarizeCosts(t, newStream(t), ledger, "2024-01-04").Costs; empty.ShareAvailable {
		t.Errorf("no snapshot, no share: %+v", empty)
	}
}

func TestWhatTheLedgerCouldNotGiveIsSaid(t *testing.T) {
	s := costSeries(t)
	for name, tc := range map[string]struct {
		ledger report.CostLedger
		want   string
	}{
		"unreadable":    {report.CostLedger{Unreadable: "ledger line 2 is not a cost as the Flex reader writes it"}, "not readable"},
		"not accepted":  {report.CostLedger{NotAccepted: "it belongs to roth-ira, not boost"}, "not accepted"},
		"none":          {report.CostLedger{}, "not yet recorded"},
		"lagging":       {report.CostLedger{Read: true, CoveredTo: "2024-01-03"}, "2024-01-03"},
		"current":       {report.CostLedger{Read: true, CoveredTo: "2024-01-05"}, "costs recorded through 2024-01-05"},
		"unknown reach": {report.CostLedger{Read: true}, "how far the Flex reader has recorded is not known"},
	} {
		got := summarizeCosts(t, s, tc.ledger, "2024-01-05").Costs
		if !strings.Contains(got.Note, tc.want) {
			t.Errorf("%s: note %q lacks %q", name, got.Note, tc.want)
		}
	}
	empty := readCosts(t)
	empty.State, empty.CoveredTo = "read", "2024-01-05"
	if got := summarizeCosts(t, s, empty, "2024-01-05").Costs; strings.Contains(got.Note, "not yet recorded") || got.Incomplete != "" {
		t.Errorf("a ledger read and empty is costs of nothing: %+v", got)
	}
}

func TestAnotherCurrencyIsNotTotalledAndIsSaid(t *testing.T) {
	s := costSeries(t)
	raw := strings.Replace(string(costLedgerOf(t, cost{"c1", event.CostFee, at(3), -10})), `"currency":"USD"`, `"currency":"EUR"`, 1)
	ledger := report.ParseCostLedger([]byte(raw))
	got := summarizeCosts(t, s, ledger, "2024-01-05").Costs
	if got.Since.Fees != 0 || !strings.Contains(got.Note, "another currency") {
		t.Errorf("costs %+v", got)
	}
}

func TestTheCostsLineInTheDailySummary(t *testing.T) {
	s := costSeries(t)
	ledger := readCosts(t,
		cost{"c1", event.CostCommission, at(3), -1}, cost{"c2", event.CostFee, at(3), -10},
		cost{"c3", event.CostInterestPaid, at(4), -3.21}, cost{"c4", event.CostInterestReceived, at(4), 0.42})
	ledger.State, ledger.CoveredTo = "read", "2024-01-04"
	text := report.RenderText(summarizeCosts(t, s, ledger, "2024-01-05"))
	if !strings.Contains(text, "Costs this month: commissions 1.00, fees 10.00, interest 2.79\n") {
		t.Errorf("text lacks the costs line:\n%s", text)
	}
	if bare := report.RenderText(summarizeCosts(t, s, report.CostLedger{}, "2024-01-05")); !strings.Contains(bare, "Costs this month: commissions 0.00, fees 0.00, interest 0.00 (fees and interest not yet recorded") {
		t.Errorf("a day with no ledger says so:\n%s", bare)
	}
}

func TestAFillDeliveredTwiceIsOneExecutionsCommission(t *testing.T) {
	s := costSeries(t)
	s.fillWithCommission(1, "e1.01", 1.25)
	s.fillWithCommission(1, "e1.01", 1.25)
	if got := summarizeCosts(t, s, report.CostLedger{}, "2024-01-05").Costs; got.Since.Commissions != 1.25 {
		t.Errorf("commissions %v", got.Since.Commissions)
	}
}

func TestAFillThatDoesNotReadIsSaidAndItsCommissionIsNotCounted(t *testing.T) {
	s := costSeries(t)
	s.fillWithCommission(1, "e1.01", 1.25)
	s.out[len(s.out)-1].Payload = []byte(`{`)
	got := summarizeCosts(t, s, report.CostLedger{}, "2024-01-05").Costs
	if got.Since.Commissions != 0 || got.FillsNotRead != 1 || !strings.Contains(got.Note, "1 fill(s) that did not read") {
		t.Errorf("costs %+v", got)
	}
}

func TestCostsAreIncompleteWhenTheReaderIsSetAsideOrBehind(t *testing.T) {
	s := costSeries(t)
	for name, tc := range map[string]struct {
		ledger report.CostLedger
		want   string
	}{
		"set aside":     {report.CostLedger{Read: true, State: "set-aside", CheckedAt: "2024-01-05", CoveredTo: "2024-01-03"}, "costs incomplete: the Flex reader is set aside as of 2024-01-05"},
		"coverage gap":  {report.CostLedger{Read: true, State: "coverage-gap", CheckedAt: "2024-01-05"}, "costs incomplete: the Flex reader is set aside as of 2024-01-05"},
		"stopped":       {report.CostLedger{Read: true, State: "running", CheckedAt: "2024-01-05", CoveredTo: "2024-01-03"}, "costs incomplete: the Flex reader's reading of 2024-01-05 did not finish"},
		"behind":        {report.CostLedger{Read: true, State: "read", CoveredTo: "2024-01-01"}, "costs incomplete: the Flex reader is behind as of 2024-01-01"},
		"unknown reach": {report.CostLedger{Read: true, State: "read"}, "costs incomplete: how far the Flex reader has recorded costs is not known"},
		"no status":     {report.CostLedger{Read: true, CoveredTo: "2024-01-04"}, "costs incomplete: how far the Flex reader has recorded costs is not known"},
	} {
		got := summarizeCosts(t, s, tc.ledger, "2024-01-05")
		if got.Costs.Incomplete != tc.want {
			t.Errorf("%s: incomplete %q, want %q", name, got.Costs.Incomplete, tc.want)
		}
		if text := report.RenderText(got); !strings.Contains(text, "("+tc.want+")\n") {
			t.Errorf("%s: the summary line lacks the bracket:\n%s", name, text)
		}
	}
	// The reader's normal lag, two sessions, is said in the note but is not incomplete.
	if got := summarizeCosts(t, s, report.CostLedger{Read: true, State: "read", CoveredTo: "2024-01-03"}, "2024-01-05").Costs; got.Incomplete != "" {
		t.Errorf("within the lag: incomplete %q", got.Incomplete)
	}
}

func TestCostLedgerCarriesItsReadingsStateAndDay(t *testing.T) {
	files := map[string][]byte{
		"/var/lib/flex/boost/costs.jsonl":             costLedgerOf(t, cost{"c1", event.CostFee, at(3), -10}),
		"/var/lib/flex/boost/costs.jsonl.status.json": []byte(`{"state":"set-aside","checked_at":"2024-01-06T02:30:00Z","reason":"x"}`),
	}
	readFile := func(path string) ([]byte, error) {
		if raw, ok := files[path]; ok {
			return raw, nil
		}
		return nil, fs.ErrNotExist
	}
	got := report.LoadCostLedgerFor("/var/lib/flex/boost/cash-movements.jsonl", "boost", readFile)
	if !got.Read || got.State != "set-aside" || got.CheckedAt != "2024-01-05" {
		t.Fatalf("ledger %+v: the New York day of the reading", got)
	}
	delete(files, "/var/lib/flex/boost/costs.jsonl")
	if got := report.LoadCostLedgerFor("/var/lib/flex/boost/cash-movements.jsonl", "boost", readFile); !got.Read || got.State != "set-aside" {
		t.Errorf("a reading set aside before any cost was written is said: %+v", got)
	}
}

func TestACorrectedExecutionsCommissionIsCountedOnceTheLatest(t *testing.T) {
	s := costSeries(t)
	s.fillWithCommission(1, "0001f4e8.65a1b2c3.01.01", 1.25)
	s.fillWithCommission(2, "0001f4e8.65a1b2c3.01.02", 1.30)
	got := summarizeCosts(t, s, report.CostLedger{}, "2024-01-05").Costs
	if !near(got.Since.Commissions, 1.30) {
		t.Errorf("commissions %v, want the correction's 1.30 alone", got.Since.Commissions)
	}
}

func TestMonthsAndYearsAreCutAtNewYorkDays(t *testing.T) {
	s := costSeries(t)
	// 20:30 New York on 2024-01-31 is 01:30 UTC on 2024-02-01.
	evening := time.Date(2024, time.February, 1, 1, 30, 0, 0, time.UTC)
	ledger := readCosts(t, cost{"eve", event.CostFee, evening, -7})
	ledger.State, ledger.CoveredTo = "read", "2024-02-01"
	got := summarizeCosts(t, s, ledger, "2024-02-01").Costs
	if got.Month.Fees != 0 || got.Since.Fees != 7 {
		t.Errorf("month %v since %v: the fee is January's, as IBKR's New York day", got.Month.Fees, got.Since.Fees)
	}
	s.fillAt(evening, "eve.01", 2)
	if got := summarizeCosts(t, s, ledger, "2024-02-01").Costs; got.Month.Commissions != 0 || got.Since.Commissions != 2 {
		t.Errorf("month %v since %v: the fill's commission is January's, as its New York day", got.Month.Commissions, got.Since.Commissions)
	}
	if got := summarizeCosts(t, s, ledger, "2024-01-31").Costs; got.Month.Commissions != 2 || got.Month.Fees != 7 {
		t.Errorf("commissions %v fees %v: a fill and a fee late in the UTC evening count on their New York day", got.Month.Commissions, got.Month.Fees)
	}
	newYear := readCosts(t, cost{"nye", event.CostFee, time.Date(2025, time.January, 1, 2, 0, 0, 0, time.UTC), -3})
	if got := summarizeCosts(t, s, newYear, "2025-01-01").Costs; got.Year.Fees != 0 || got.Since.Fees != 3 {
		t.Errorf("year %v since %v: New Year's Eve in New York is last year's", got.Year.Fees, got.Since.Fees)
	}
}

func TestForHistoryKeepsOnlyWhatTheCostsReadOfAnEarlierFill(t *testing.T) {
	s := costSeries(t)
	s.fillWithCommission(1, "e1.01", 1.25)
	var entries []journal.Entry
	for _, e := range s.out {
		entries = append(entries, journal.Entry{Kind: journal.KindInput, Envelope: e})
	}
	whole := journal.Recovery{Entries: entries, FinalRecordHash: "sha256:" + strings.Repeat("1", 64)}
	kept := report.ForHistory(whole, "SPY")
	var fills []event.Envelope
	for _, e := range kept.Entries {
		if e.Envelope.Type != event.CompletedBarEventType && e.Envelope.Type != event.AccountSnapshotEventType && e.Envelope.Type != event.TargetWeightConfigurationEventType && e.Envelope.Type != event.OperatorRunApprovedEventType {
			fills = append(fills, e.Envelope)
		}
	}
	if len(fills) != 1 {
		t.Fatalf("kept %d fills", len(fills))
	}
	f := fills[0]
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(f.Payload, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 || fields["fill_id"] == nil || fields["commission"] == nil || fields["filled_at"] == nil || f.SchemaVersion != event.FillTargetWeightVersion {
		t.Errorf("kept %s at version %d: only the fill's ID, commission, time and version", f.Payload, f.SchemaVersion)
	}
	if f.ID != "" || f.Source != "" || f.CorrelationID != "" {
		t.Errorf("the envelope's other fields are not kept: %+v", f)
	}
	current := newStream(t)
	current.bar(4, "SPY", 100)
	current.snapshot(4, 10000, 0)
	var earlier []event.Envelope
	for _, e := range kept.Entries {
		earlier = append(earlier, e.Envelope)
	}
	got, err := report.SummarizeWithCosts(report.History{Earlier: earlier}, report.CashLedger{}, report.CostLedger{}, current.out, "2024-01-06", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.Costs.Since.Commissions != 1.25 || got.Costs.FillsNotRead != 0 {
		t.Errorf("costs %+v: the kept fill's commission still counts", got.Costs)
	}
}

func TestWhatCannotBeReadOfTheCostsStatusOrAKeptFillIsSaid(t *testing.T) {
	files := map[string][]byte{
		"/var/lib/flex/boost/costs.jsonl":             costLedgerOf(t, cost{"c1", event.CostFee, at(3), -10}),
		"/var/lib/flex/boost/costs.jsonl.status.json": []byte(`{`),
	}
	readFile := func(path string) ([]byte, error) {
		if raw, ok := files[path]; ok {
			return raw, nil
		}
		return nil, fs.ErrNotExist
	}
	s := costSeries(t)
	ledger := report.LoadCostLedgerFor("/var/lib/flex/boost/cash-movements.jsonl", "boost", readFile)
	if got := summarizeCosts(t, s, ledger, "2024-01-05").Costs; ledger.State != "" || got.Incomplete != "costs incomplete: how far the Flex reader has recorded costs is not known" {
		t.Errorf("a status file that does not read: ledger %+v, incomplete %q", ledger, got.Incomplete)
	}
	// A recorded day the calendar cannot place is behind: how far is not known.
	if got := summarizeCosts(t, s, report.CostLedger{Read: true, State: "read", CoveredTo: "not a day"}, "2024-01-05").Costs; got.Incomplete == "" {
		t.Error("a day not read is not taken as current")
	}
	earlier := costSeries(t)
	earlier.fillWithCommission(1, "e1.01", 1.25)
	earlier.out[len(earlier.out)-1].Payload = []byte(`{`)
	earlier.fillWithCommission(2, "", 1.25)
	var entries []journal.Entry
	for _, e := range earlier.out {
		entries = append(entries, journal.Entry{Kind: journal.KindInput, Envelope: e})
	}
	var kept []event.Envelope
	for _, e := range report.ForHistory(journal.Recovery{Entries: entries}, "SPY").Entries {
		kept = append(kept, e.Envelope)
	}
	current := newStream(t)
	current.bar(4, "SPY", 100)
	current.snapshot(4, 10000, 0)
	got, err := report.SummarizeWithCosts(report.History{Earlier: kept}, report.CashLedger{}, report.CostLedger{}, current.out, "2024-01-06", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.Costs.FillsNotRead != 2 || got.Costs.Since.Commissions != 0 {
		t.Errorf("costs %+v: an earlier fill that did not read is counted as not read", got.Costs)
	}
	forged := event.Envelope{Type: kept[len(kept)-1].Type, EventTime: day(2), Payload: []byte(`{"fill_id":"","commission":1,"filled_at":"2024-01-03T21:00:00Z"}`)}
	if got, _ := report.SummarizeWithCosts(report.History{Earlier: []event.Envelope{forged}}, report.CashLedger{}, report.CostLedger{}, current.out, "2024-01-06", "SPY"); got.Costs.FillsNotRead != 1 {
		t.Errorf("a kept fill without its ID: %+v", got.Costs)
	}
}

func TestTheNightlyReadersNormalReachIsNotBehind(t *testing.T) {
	s := costSeries(t)
	// The reading on the evening of the session before the summary's day ends
	// its statement a session earlier and covers to the day before that.
	for name, tc := range map[string]struct {
		coveredTo, date string
		behind          bool
	}{
		"a Wednesday after Tuesday evening's reading": {"2024-01-07", "2024-01-10", false},
		"a Monday after Friday evening's reading":     {"2024-01-03", "2024-01-08", false},
		"a Friday after Thursday evening's reading":   {"2024-01-02", "2024-01-05", false},
		"a Wednesday with Tuesday's reading missed":   {"2024-01-04", "2024-01-10", true},
	} {
		got := summarizeCosts(t, s, report.CostLedger{Read: true, State: "read", CoveredTo: tc.coveredTo}, tc.date).Costs
		if (got.Incomplete != "") != tc.behind {
			t.Errorf("%s: incomplete %q", name, got.Incomplete)
		}
	}
}

func TestASetAsideReadingWithNoReachStillSaysItWasSetAside(t *testing.T) {
	s := costSeries(t)
	for _, ledger := range []report.CostLedger{
		{Read: true, State: "set-aside", CheckedAt: "2024-01-05"},
		{Read: true, State: "coverage-gap", CheckedAt: "2024-01-05"},
		{Read: true, State: "running", CheckedAt: "2024-01-05"},
	} {
		got := summarizeCosts(t, s, ledger, "2024-01-05").Costs
		if got.Incomplete == "" || !strings.Contains(got.Note, got.Incomplete) {
			t.Errorf("%s: the note %q lacks %q", ledger.State, got.Note, got.Incomplete)
		}
	}
}

func TestARedeliveredExecutionNeverReplacesItsCorrection(t *testing.T) {
	s := costSeries(t)
	s.fillWithCommission(1, "0001f4e8.65a1b2c3.01.01", 1.25)
	s.fillWithCommission(2, "0001f4e8.65a1b2c3.01.02", 1.30)
	s.fillWithCommission(3, "0001f4e8.65a1b2c3.01.01", 1.25)
	s.fillWithCommission(1, "0001f4e8.65a1b2c4.01.9", 2.00)
	s.fillWithCommission(2, "0001f4e8.65a1b2c4.01.10", 2.10)
	s.fillWithCommission(3, "0001f4e8.65a1b2c4.01.9", 2.00)
	got := summarizeCosts(t, s, report.CostLedger{}, "2024-01-05").Costs
	if !near(got.Since.Commissions, 3.40) {
		t.Errorf("commissions %v, want the corrections' 1.30 and 2.10 alone", got.Since.Commissions)
	}
}

func TestARestatedCostCountsAtItsLatestValue(t *testing.T) {
	raw := string(costLedgerOf(t, cost{"c1", event.CostFee, at(3), -10}, cost{"c2", event.CostFee, at(3), -1}))
	restated := func(n, amount, note string) string {
		stamp := at(3).UTC().Format(time.RFC3339)
		return `{"id":"ibkr-flex:c1:restated:` + n + `","type":"account.cost","schema_version":1,"event_time":"` + stamp +
			`","source":"ibkr-flex","payload":{"kind":"fee","amount":` + amount + `,"currency":"USD","as_of":"` + stamp +
			`","transaction_id":"c1"},"note":"` + note + `"}` + "\n"
	}
	once := raw + restated("1", "-11", "restated: was fee -10.0 USD on x, now fee -11.0 USD on x")
	twice := once + restated("2", "-12", "restated: was fee -11.0 USD on x, now fee -12.0 USD on x")
	for name, tc := range map[string]struct {
		raw       string
		fees      float64
		restated  int
		wantInfix string
	}{
		"once":  {once, 12, 1, "1 restatement(s) by IBKR (latest 2024-01-03)"},
		"twice": {twice, 13, 2, "2 restatement(s) by IBKR (latest 2024-01-03)"},
	} {
		ledger := report.ParseCostLedger([]byte(tc.raw))
		if ledger.Unreadable != "" {
			t.Fatalf("%s: %s", name, ledger.Unreadable)
		}
		ledger.State, ledger.CoveredTo = "read", "2024-01-04"
		got := summarizeCosts(t, costSeries(t), ledger, "2024-01-05").Costs
		if !near(got.Since.Fees, tc.fees) || ledger.Restated != tc.restated || !strings.Contains(got.Note, tc.wantInfix) {
			t.Errorf("%s: fees %v, restated %d, note %q", name, got.Since.Fees, ledger.Restated, got.Note)
		}
	}
	for name, bad := range map[string]string{
		"numbered out of turn":   raw + restated("2", "-11", "restated: was x"),
		"without its note":       raw + restated("1", "-11", ""),
		"of a cost not recorded": strings.Replace(raw, `ibkr-flex:c1"`, `ibkr-flex:c9"`, 1),
	} {
		if name == "of a cost not recorded" {
			bad = string(costLedgerOf(t, cost{"c2", event.CostFee, at(3), -1})) + restated("1", "-11", "restated: was x")
		}
		if got := report.ParseCostLedger([]byte(bad)); got.Read {
			t.Errorf("%s: a restatement not as the reader writes it is read", name)
		}
	}
}

func TestAHexadecimalRevisionIsComparedAsHexadecimal(t *testing.T) {
	s := costSeries(t)
	// IBKR's execution IDs are hex-segmented: 0a follows 09, whatever the fill times say.
	s.fillWithCommission(1, "0001f4e8.65a1b2c7.01.0a", 1.10)
	s.fillWithCommission(2, "0001f4e8.65a1b2c7.01.09", 1.00)
	got := summarizeCosts(t, s, report.CostLedger{}, "2024-01-05").Costs
	if !near(got.Since.Commissions, 1.10) {
		t.Errorf("commissions %v, want revision 0a's 1.10 alone", got.Since.Commissions)
	}
}

func TestAFullTieKeepsTheLatestJournalledRevision(t *testing.T) {
	s := costSeries(t)
	// The same revision (0a and 0A) at the same time: the later journalled is the execution's.
	s.fillWithCommission(1, "0001f4e8.65a1b2c8.01.0a", 1.00)
	s.fillWithCommission(1, "0001f4e8.65a1b2c8.01.0A", 2.00)
	got := summarizeCosts(t, s, report.CostLedger{}, "2024-01-05").Costs
	if !near(got.Since.Commissions, 2.00) {
		t.Errorf("commissions %v, want the later journalled 2.00 alone", got.Since.Commissions)
	}
}

func TestAFillIsCutByItsFilledAtNotByTheEnvelopesTime(t *testing.T) {
	s := costSeries(t)
	// Filled 20:30 New York on 2024-01-31 (01:30 UTC on 2024-02-01), recorded after the 2nd's midnight.
	filled := time.Date(2024, time.February, 1, 1, 30, 0, 0, time.UTC)
	recorded := time.Date(2024, time.February, 2, 6, 0, 0, 0, time.UTC)
	s.add(event.FillEventType, event.FillTargetWeightVersion, "fill:late.01", recorded,
		event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "tw-1", FillID: "late.01", InstrumentID: "SPY",
			Side: event.OrderSideBuy, Quantity: 10, Price: 100, Commission: 2, FilledAt: filled})
	if got := summarizeCosts(t, s, report.CostLedger{}, "2024-01-31").Costs; got.Month.Commissions != 2 {
		t.Errorf("commissions %v: the fill was filled on the 31st in New York, so the 31st counts it", got.Month.Commissions)
	}
	s2 := costSeries(t)
	early := time.Date(2024, time.January, 30, 15, 0, 0, 0, time.UTC)
	s2.add(event.FillEventType, event.FillTargetWeightVersion, "fill:early.01", early,
		event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "tw-1", FillID: "early.01", InstrumentID: "SPY",
			Side: event.OrderSideBuy, Quantity: 10, Price: 100, Commission: 3, FilledAt: time.Date(2024, time.February, 2, 15, 0, 0, 0, time.UTC)})
	if got := summarizeCosts(t, s2, report.CostLedger{}, "2024-01-31").Costs; got.Since.Commissions != 0 {
		t.Errorf("commissions %v: a fill filled on the 2nd is not counted by the 31st whatever time it was recorded", got.Since.Commissions)
	}
}

func TestACorrectionsRevisionIsComparedNumericallyWhereBothAreNumbers(t *testing.T) {
	s := costSeries(t)
	// Both last segments are numbers: 10 follows 9 whatever their byte order or length, and the leading zero is no
	// later than the number it equals.
	s.fillWithCommission(1, "0001f4e8.65a1b2c5.01.09", 1.00)
	s.fillWithCommission(2, "0001f4e8.65a1b2c5.01.10", 1.10)
	// Not numbers: the later fill time is the later record, never byte order.
	s.fillWithCommission(1, "0001f4e8.65a1b2c6.01.z", 2.00)
	s.fillWithCommission(2, "0001f4e8.65a1b2c6.01.y", 2.20)
	got := summarizeCosts(t, s, report.CostLedger{}, "2024-01-05").Costs
	if !near(got.Since.Commissions, 3.30) {
		t.Errorf("commissions %v, want 1.10 and 2.20 alone", got.Since.Commissions)
	}
}

func TestACostRestatedToZeroReversesTheEarlierAmount(t *testing.T) {
	raw := string(costLedgerOf(t, cost{"c1", event.CostFee, at(3), -10}))
	stamp := at(3).UTC().Format(time.RFC3339)
	line := func(id, amount string) string {
		return `{"id":"ibkr-flex:` + id + `","type":"account.cost","schema_version":1,"event_time":"` + stamp +
			`","source":"ibkr-flex","payload":{"kind":"fee","amount":` + amount + `,"currency":"USD","as_of":"` + stamp +
			`","transaction_id":"c1"},"note":"restated: was fee -10.0 USD on x, now fee 0.0 USD on x"}` + "\n"
	}
	ledger := report.ParseCostLedger([]byte(raw + line("c1:restated:1", "0.0")))
	if ledger.Unreadable != "" || ledger.Restated != 1 || ledger.Reversed != 1 {
		t.Fatalf("a restatement to zero is a reversal: %+v", ledger)
	}
	ledger.State, ledger.CoveredTo = "read", "2024-01-04"
	got := summarizeCosts(t, costSeries(t), ledger, "2024-01-05").Costs
	if got.Since.Fees != 0 || !strings.Contains(got.Note, "1 reversing a cost to zero") {
		t.Errorf("fees %v, note %q", got.Since.Fees, got.Note)
	}
	// A zero is accepted only on a restated line, never on a new cost.
	if got := report.ParseCostLedger([]byte(strings.Replace(raw, "-10", "0", 1))); got.Read {
		t.Errorf("a new cost of nothing is read")
	}
}

func TestTheCostsStatusRestatedCountIsCarriedToTheNote(t *testing.T) {
	stamp := at(3).UTC().Format(time.RFC3339)
	files := map[string][]byte{
		"/var/lib/flex/boost/costs.jsonl": append(costLedgerOf(t, cost{"c1", event.CostFee, at(3), -10}),
			[]byte(`{"id":"ibkr-flex:c1:restated:1","type":"account.cost","schema_version":1,"event_time":"`+stamp+
				`","source":"ibkr-flex","payload":{"kind":"fee","amount":-11,"currency":"USD","as_of":"`+stamp+
				`","transaction_id":"c1"},"note":"restated: was x"}`+"\n")...),
		"/var/lib/flex/boost/costs.jsonl.status.json": []byte(`{"state":"read","covered_to":"20240104","checked_at":"2024-01-05T22:00:00Z","restated":3}`),
	}
	readFile := func(p string) ([]byte, error) {
		if b, ok := files[p]; ok {
			return b, nil
		}
		return nil, fs.ErrNotExist
	}
	ledger := report.LoadCostLedgerFor("/var/lib/flex/boost/cash-movements.jsonl", "boost", readFile)
	got := summarizeCosts(t, costSeries(t), ledger, "2024-01-05").Costs
	if ledger.RestatedLast != 3 || !strings.Contains(got.Note, "3 restated in the last reading") {
		t.Errorf("restated last %d, note %q", ledger.RestatedLast, got.Note)
	}
}
