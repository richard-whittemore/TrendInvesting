package main

import (
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

// writeCosts writes a Flex costs ledger beside a cash ledger in dir: a fee
// of 10 and interest paid of 3.21 on the page's day, and a fee of 99 the
// day after it.
func writeCosts(t *testing.T, dir string) {
	t.Helper()
	var b strings.Builder
	for _, c := range []struct {
		id, kind string
		asOf     time.Time
		amount   float64
	}{
		{"c1", event.CostFee, at(1), -10},
		{"c2", event.CostInterestPaid, at(1), -3.21},
		{"c3", event.CostFee, at(2), -99},
	} {
		stamp := c.asOf.UTC().Format(time.RFC3339)
		line, err := json.Marshal(map[string]any{"id": "ibkr-flex:" + c.id, "type": event.CostEventType, "schema_version": event.CostSchemaVersion,
			"event_time": stamp, "source": "ibkr-flex",
			"payload": map[string]any{"kind": c.kind, "amount": c.amount, "currency": "USD", "as_of": stamp, "transaction_id": c.id}})
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "costs.jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func costsFixture() account {
	a := shadowFixture(report.VerdictAhead)
	a.Summary.Costs = report.Costs{
		MonthStart: "2024-01-01", YearStart: "2024-01-01", StartDate: "2024-01-02",
		Month:                 report.CostTotals{Commissions: 1, Fees: 10, InterestPaid: 3.21, InterestReceived: 0.42},
		Year:                  report.CostTotals{Commissions: 1, Fees: 10, InterestPaid: 3.21, InterestReceived: 0.42},
		Since:                 report.CostTotals{Commissions: 4, Fees: 10, InterestPaid: 3.21, InterestReceived: 0.42},
		AverageNetLiquidation: 1500, Share: 0.0112, ShareAvailable: true,
		Note: "Counted: commissions on fills as the journal holds them.",
	}
	return a
}

func TestTheCostsCardSitsUnderTheEquityComparisonWithEveryPeriodAndKind(t *testing.T) {
	body := execThemed(t, "", []account{costsFixture()})
	equity, costs, change := strings.Index(body, "<h3>Equity against SPY</h3>"), strings.Index(body, "<h3>Costs</h3>"), strings.Index(body, "<h3>Change between paired points</h3>")
	if equity < 0 || costs < equity || (change >= 0 && change < costs) {
		t.Fatalf("the costs card must follow the equity comparison: equity %d costs %d change %d", equity, costs, change)
	}
	card, _, _ := strings.Cut(body[costs:], "</div>")
	for _, want := range []string{"Month to date", "Year to date", "Since start", "Commissions", "Fees", "Interest paid", "Interest received", "Net cost",
		"1.00", "10.00", "3.21", "0.42", "13.79", "16.79", // net: 1+10+3.21-0.42 and 4+10+3.21-0.42
		"1.12% of average net liquidation", "Counted: commissions on fills"} {
		if !strings.Contains(card, want) {
			t.Errorf("costs card lacks %q:\n%s", want, card)
		}
	}
}

func TestTheCostsCardWithoutAnAverageNetLiquidationGivesNoShare(t *testing.T) {
	a := costsFixture()
	a.Summary.Costs.ShareAvailable = false
	body := execThemed(t, "", []account{a})
	if strings.Contains(body, "of average net liquidation") {
		t.Error("a share was shown with no average to take it of")
	}
}

func TestTheDashboardTotalsTheCostsLedgerBesideEachAccountsCashLedger(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "engines", "boost")
	flex := filepath.Join(root, "flex", "boost")
	for _, d := range []string{dir, flex} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeRun(t, dir, "run1.log", approval("", ""), 0, time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC))
	ledger := writeLedger(t, flex, at(1).Add(-2*time.Hour), 10)
	writeCosts(t, flex)
	h := newHandlerWith(source{dirs: []string{dir}, ledgers: map[string]string{dir: ledger}}, "SPY", os.ReadFile)
	got := summaryOf(t, h).Summary.Costs
	if got.Month.Fees != 10 || !near(got.Month.InterestPaid, 3.21) || got.Since.Fees != 10 {
		t.Fatalf("costs %+v: the fee dated after the page's day is not counted", got)
	}
	raw := get(h, http.MethodGet, "/api/summary.json").Body.String()
	for _, key := range []string{`"Costs":{`, `"Month":{"Commissions":0,"Fees":10`, `"Since":{`, `"Share":`, `"AverageNetLiquidation":`} {
		if !strings.Contains(raw, key) {
			t.Errorf("summary.json lacks %s:\n%s", key, raw)
		}
	}
	// Another account's folder: the costs are not read.
	other := filepath.Join(root, "flex", "roth-ira")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCosts(t, other)
	h = newHandlerWith(source{dirs: []string{dir}, ledgers: map[string]string{dir: filepath.Join(other, "cash-movements.jsonl")}}, "SPY", os.ReadFile)
	if got := summaryOf(t, h).Summary.Costs; got.Since.Fees != 0 || !strings.Contains(got.Note, "not accepted") {
		t.Errorf("another account's costs were read: %+v", got)
	}
}
