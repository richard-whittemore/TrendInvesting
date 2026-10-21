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
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

var sessions = time.Date(2024, time.January, 2, 21, 0, 0, 0, time.UTC)

func at(n int) time.Time { return sessions.AddDate(0, 0, n) }

func input(t *testing.T, seq uint64, when time.Time, eventType string, version uint32, id string, payload any) journal.Entry {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return journal.Entry{Kind: journal.KindInput, Envelope: event.Envelope{ID: id, Type: eventType, EnvelopeVersion: event.CurrentEnvelopeVersion,
		SchemaVersion: version, EventTime: when, RecordedAt: when, Sequence: seq, Source: "test", StrategyVersion: "v",
		ConfigurationHash: "h", PayloadHash: event.HashPayload(raw), Payload: raw}}
}

// writeRun writes a durable log of one run: its configuration, its approval
// and a close on each session in sessionsOf with synthetic figures. It
// returns the log's final record hash.
func writeRun(t *testing.T, dir, name string, approval event.OperatorRunApprovedPayload, first int, modified time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	log, err := journal.CreateLog(file, journal.NewLogHeader("h", "v"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := event.TargetWeightConfigurationPayload{StrategyID: event.TargetWeightStrategyBoost, AccountID: "joint", Mode: event.TradingModeDryRun, Adapter: event.AdapterIBKR, Boost: event.BoostConfiguration{InstrumentID: "SPY", InvestedFraction: 0.98, BoostSize: 1.5, RSIPeriod: 2, RSIEntry: 10, TrendDays: 200, ExitSMA: 5, WarmUpStart: "2024-01-02"},
		Limits:    event.MarginLimits{MaxExposure: 1.5, MaxOrdersPerDecision: 1, MaxOrderFraction: 1, PriceBand: 0.05, ExcessLiquidityWarning: 0.4, ExcessLiquidityUrgent: 0.2, ExcessLiquidityStaleMinutes: 30},
		Execution: event.ExecutionConfiguration{MaxSnapshotAge: 30}}
	entries := []journal.Entry{
		input(t, 1, at(first), event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "cfg:"+name, cfg),
		input(t, 2, at(first), event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "approved:"+name, approval),
	}
	for n := first; n < first+2; n++ {
		price := 100 + float64(n)
		view := func(label string) event.PriceView {
			return event.PriceView{View: label, Open: price, High: price, Low: price, Close: price, Volume: 1}
		}
		entries = append(entries,
			input(t, uint64(len(entries)+1), at(n), event.CompletedBarEventType, event.CompletedBarSchemaVersion, "bar:"+name+string(rune('0'+n)),
				event.CompletedBarPayload{InstrumentID: "SPY", PeriodEnd: at(n), SplitAdjusted: view(event.ViewSplitAdjusted), Raw: view(event.ViewRaw)}),
			input(t, uint64(len(entries)+2), at(n), event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "snap:"+name+string(rune('0'+n)),
				event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: at(n), Currency: "USD", NetLiquidation: 4000 + float64(10*n), Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: 39}}}))
	}
	for _, e := range entries {
		if err := log.Append([]journal.Entry{e}); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	return log.FinalRecordHash()
}

func approval(previous, hash string) event.OperatorRunApprovedPayload {
	latched := false
	p := event.OperatorRunApprovedPayload{AccountID: "joint", ApprovedBy: "owner", ApprovedAt: at(0), Boost: event.BoostStateInForce{Latched: &latched}}
	if previous == "" {
		p.StartingBook = &event.StartingBook{Positions: []event.AccountPosition{}, TotalCash: 1000, Currency: "USD"}
		return p
	}
	p.PreviousLog, p.PreviousLogFinalHash = previous, hash
	return p
}

func summaryOf(t *testing.T, h http.Handler) account {
	t.Helper()
	rec := get(h, http.MethodGet, "/api/summary.json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var accounts []account
	if err := json.Unmarshal(rec.Body.Bytes(), &accounts); err != nil || len(accounts) != 1 {
		t.Fatalf("accounts = %v, err = %v", accounts, err)
	}
	return accounts[0]
}

// TestDashboardFollowsARestartAndShowsTheWholeHistory: the handler is
// started with the engine's directory, not a log. A second run appears after
// it started; the next request shows that run's day and the history from the
// first run's first close, with no restart of the dashboard.
func TestDashboardFollowsARestartAndShowsTheWholeHistory(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	h1 := writeRun(t, dir, "run1.log", approval("", ""), 0, base)
	h := newHandlerFor(nil, []string{dir}, "SPY")

	first := summaryOf(t, h)
	if first.Summary.Date != at(1).Format(time.DateOnly) || len(first.Summary.Equity.Points) != 2 {
		t.Fatalf("before the restart: date %s, %d points", first.Summary.Date, len(first.Summary.Equity.Points))
	}

	writeRun(t, dir, "run2.log", approval("run1.log", h1), 2, base.Add(time.Hour))
	second := summaryOf(t, h)
	e := second.Summary.Equity
	if second.Summary.Date != at(3).Format(time.DateOnly) {
		t.Fatalf("after the restart the page shows %s, want the new run's day", second.Summary.Date)
	}
	if len(e.Points) != 4 || e.StartDate != at(0).Format(time.DateOnly) || e.EndDate != at(3).Format(time.DateOnly) {
		t.Fatalf("history = %d points from %s to %s, want four from the first run's first close", len(e.Points), e.StartDate, e.EndDate)
	}
}

// TestDashboardRefusesABrokenChainNamingTheLink: the page does not show a
// history that silently starts late.
func TestDashboardRefusesABrokenChainNamingTheLink(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	writeRun(t, dir, "run1.log", approval("", ""), 0, base)
	writeRun(t, dir, "run2.log", approval("run1.log", "sha256:"+strings.Repeat("0", 64)), 2, base.Add(time.Hour))
	rec := get(newHandlerFor(nil, []string{dir}, "SPY"), http.MethodGet, "/")
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "run2.log") || !strings.Contains(rec.Body.String(), "run1.log") {
		t.Fatalf("status %d body %q, want a refusal naming run2.log and run1.log", rec.Code, rec.Body)
	}
}

// TestDashboardOfAFixedJournalStillFollowsTheChain: -journal names one log;
// its earlier runs are still followed.
func TestDashboardOfAFixedJournalStillFollowsTheChain(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	h1 := writeRun(t, dir, "run1.log", approval("", ""), 0, base)
	writeRun(t, dir, "run2.log", approval("run1.log", h1), 2, base.Add(time.Hour))
	got := summaryOf(t, newHandlerFor([]string{filepath.Join(dir, "run2.log")}, nil, "SPY"))
	if len(got.Summary.Equity.Points) != 4 {
		t.Fatalf("points = %d, want 4", len(got.Summary.Equity.Points))
	}
}
