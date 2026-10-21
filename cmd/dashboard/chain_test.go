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
func writeRun(t *testing.T, dir, name string, approval event.OperatorRunApprovedPayload, first int, modified time.Time, extra ...journal.Entry) string {
	t.Helper()
	path := filepath.Join(dir, name)
	var file memoryFile
	log, err := journal.CreateLog(&file, journal.NewLogHeader("h", "v"))
	if err != nil {
		t.Fatal(err)
	}
	entries := []journal.Entry{
		configurationOf(t, name, first),
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
	for _, e := range extra {
		e.Envelope.Sequence = uint64(len(entries) + 1)
		entries = append(entries, e)
	}
	// Each input is its own group, with the decisions recorded after it.
	var group []journal.Entry
	for i, e := range entries {
		group = append(group, e)
		if i+1 < len(entries) && entries[i+1].Kind == journal.KindDecision {
			continue
		}
		if err := log.Append(group); err != nil {
			t.Fatal(err)
		}
		group = nil
	}
	if err := os.WriteFile(path, file.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	return log.FinalRecordHash()
}

// configurationOf is the run's configuration input, its first record.
func configurationOf(t *testing.T, name string, first int) journal.Entry {
	t.Helper()
	cfg := event.TargetWeightConfigurationPayload{StrategyID: event.TargetWeightStrategyBoost, AccountID: "joint", Mode: event.TradingModeDryRun, Adapter: event.AdapterIBKR, Boost: event.BoostConfiguration{InstrumentID: "SPY", InvestedFraction: 0.98, BoostSize: 1.5, RSIPeriod: 2, RSIEntry: 10, TrendDays: 200, ExitSMA: 5, WarmUpStart: "2024-01-02"},
		Limits:    event.MarginLimits{MaxExposure: 1.5, MaxOrdersPerDecision: 1, MaxOrderFraction: 1, PriceBand: 0.05, ExcessLiquidityWarning: 0.4, ExcessLiquidityUrgent: 0.2, ExcessLiquidityStaleMinutes: 30},
		Execution: event.ExecutionConfiguration{MaxSnapshotAge: 30}}
	return input(t, 1, at(first), event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "cfg:"+name, cfg)
}

// memoryFile is a log file held in memory and written out whole: a test's
// logs need no flush to the disk.
type memoryFile struct{ bytes.Buffer }

func (*memoryFile) Sync() error { return nil }

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

// halted is an engine state decision holding the engine Halted: an alert.
func halted(t *testing.T, n int) journal.Entry {
	t.Helper()
	e := input(t, 0, at(n), event.EngineStateEventType, event.EngineStateScopedVersion, "state:halted",
		event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "figures"})
	e.Kind = journal.KindDecision
	return e
}

// TestDashboardShowsTheCurrentRunWhenTheChainIsBroken: only the equity
// history reads the earlier runs, so a broken chain never blanks the page:
// the current run's holdings, state, alerts and day's figures are shown, and
// the equity card names both logs of the broken link and where the intact
// runs start.
func TestDashboardShowsTheCurrentRunWhenTheChainIsBroken(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	writeRun(t, dir, "run1.log", approval("", ""), 0, base)
	writeRun(t, dir, "run2.log", approval("run1.log", "sha256:"+strings.Repeat("0", 64)), 2, base.Add(time.Hour), halted(t, 3))
	h := newHandlerFor(nil, []string{dir}, "SPY")
	got := summaryOf(t, h)
	s := got.Summary
	if s.EngineState != "halted" || !s.HaveSnapshot || len(s.Holdings) != 1 || len(got.Alerts) == 0 || s.Date != at(3).Format(time.DateOnly) {
		t.Fatalf("the current run is not shown: %+v, alerts %v", s, got.Alerts)
	}
	const message = "history unavailable: the chain of runs is broken between run2.log and run1.log"
	if s.Equity.HistoryUnavailable != message || s.Equity.HistoryStartsAt != "run2.log" || len(s.Equity.Points) != 2 {
		t.Fatalf("equity = %+v, want the broken link named and the intact run's two points", s.Equity)
	}
	rec := get(h, http.MethodGet, "/")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), message) || !strings.Contains(rec.Body.String(), "engine halted") {
		t.Fatalf("status %d body %q", rec.Code, rec.Body)
	}
}

// TestDashboardKeepsBothLogNamesForADamagedEarlierLog: a damaged earlier
// log, whatever its recovery error reads, is shown as the broken link
// between the two logs, never relabelled as the current journal failing.
func TestDashboardKeepsBothLogNamesForADamagedEarlierLog(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	h1 := writeRun(t, dir, "run1.log", approval("", ""), 0, base)
	writeRun(t, dir, "run2.log", approval("run1.log", h1), 2, base.Add(time.Hour))
	path := filepath.Join(dir, "run1.log")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `"net_liquidation":4000`, `"net_liquidation":4001`, 1))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got := summaryOf(t, newHandlerFor([]string{filepath.Join(dir, "run2.log")}, nil, "SPY"))
	if got.Summary.Equity.HistoryUnavailable != "history unavailable: the chain of runs is broken between run2.log and run1.log" {
		t.Fatalf("equity = %+v", got.Summary.Equity)
	}
}

// TestDashboardRereadsOnlyTheCurrentLog: each earlier log's recovery is kept
// by its path, size and modification time, so a second request reads only
// the current log, and a log that changed is read again.
func TestDashboardRereadsOnlyTheCurrentLog(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	h1 := writeRun(t, dir, "run1.log", approval("", ""), 0, base)
	h2 := writeRun(t, dir, "run2.log", approval("run1.log", h1), 2, base.Add(time.Hour))
	writeRun(t, dir, "run3.log", approval("run2.log", h2), 4, base.Add(2*time.Hour))
	opens := map[string]int{}
	readFile := func(path string) ([]byte, error) {
		opens[filepath.Base(path)]++
		return os.ReadFile(path)
	}
	h := newHandlerWith(source{dirs: []string{dir}}, "SPY", readFile)
	if got := summaryOf(t, h); len(got.Summary.Equity.Points) != 6 {
		t.Fatalf("first request: %d points", len(got.Summary.Equity.Points))
	}
	clear(opens)
	if got := summaryOf(t, h); len(got.Summary.Equity.Points) != 6 {
		t.Fatalf("second request: %d points", len(got.Summary.Equity.Points))
	}
	if len(opens) != 1 || opens["run3.log"] != 1 {
		t.Fatalf("second request opened %v, want run3.log once", opens)
	}
	clear(opens)
	writeRun(t, dir, "run3.log", approval("run2.log", h2), 4, base.Add(3*time.Hour), halted(t, 5))
	if got := summaryOf(t, h); got.Summary.EngineState != "halted" {
		t.Fatalf("the changed current log was not read again: %+v", got.Summary)
	}
	if opens["run1.log"] != 0 || opens["run2.log"] != 0 {
		t.Fatalf("after the current log changed, opened %v", opens)
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

// TestDashboardOfAnEngineDirectoryWithNoRunSaysSo: a directory the engine has
// not started a run in is a first start, not a damaged log, so the page says
// there is no run yet, naming the account, rather than that the journal is
// unavailable.
func TestDashboardOfAnEngineDirectoryWithNoRunSaysSo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "trad-ira")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := get(newHandlerFor(nil, []string{dir}, "SPY"), http.MethodGet, "/")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "trad-ira: no run yet: the engine has not started a run") {
		t.Fatalf("status %d body %q, want the account's no-run message", rec.Code, rec.Body)
	}
}

// TestDashboardDegradesPerAccount: an engine directory with no run, or one
// that cannot be read, shows its own message and never blanks the other
// accounts' cards.
func TestDashboardDegradesPerAccount(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	good := filepath.Join(root, "boost")
	empty := filepath.Join(root, "trad-ira")
	for _, d := range []string{good, empty} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeRun(t, good, "run1.log", approval("", ""), 0, base, halted(t, 5))
	missing := filepath.Join(root, "gone")
	h := newHandlerFor(nil, []string{good, empty, missing}, "SPY")
	rec := get(h, http.MethodGet, "/")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, body)
	}
	for _, want := range []string{"trad-ira: no run yet", "gone: journal unavailable", "acct-joint", "halted"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, root) {
		t.Error("the page must not leak the server's paths")
	}
	rec = get(h, http.MethodGet, "/api/summary.json")
	var accounts []account
	if err := json.Unmarshal(rec.Body.Bytes(), &accounts); err != nil || len(accounts) != 3 || accounts[0].Summary.EngineState != "halted" || accounts[1].Message == "" {
		t.Fatalf("accounts = %+v, err = %v", accounts, err)
	}
}

// TestDashboardOfOnlyAbandonedStartsSaysTheEngineStarted: a directory holding
// only a log with nothing past its header is a run that started and crashed,
// not a first start.
func TestDashboardOfOnlyAbandonedStartsSaysTheEngineStarted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "boost")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var file memoryFile
	if _, err := journal.CreateLog(&file, journal.NewLogHeader("h", "v")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run1.log"), file.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	body := get(newHandlerFor(nil, []string{dir}, "SPY"), http.MethodGet, "/").Body.String()
	if !strings.Contains(body, "boost: the engine started a run but recorded nothing past its header") || strings.Contains(body, "has not started a run") {
		t.Fatalf("body %q", body)
	}
}

// twoEngineDirs writes a healthy boost directory and a trad-ira one holding a
// run, and returns them with the root.
func twoEngineDirs(t *testing.T) (boost, trad string) {
	t.Helper()
	root := t.TempDir()
	base := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	boost, trad = filepath.Join(root, "boost"), filepath.Join(root, "trad-ira")
	for _, d := range []string{boost, trad} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeRun(t, boost, "run1.log", approval("", ""), 0, base, halted(t, 5))
	writeRun(t, trad, "run1.log", approval("", ""), 0, base, halted(t, 5))
	return boost, trad
}

// TestADirectoryWhoseHeadLogDoesNotVerifyIsItsOwnCard: a head log that fails
// to verify, or cannot be read, after the directory's listing, shows that
// account's card and never blanks the others (ADR 0027 point 5).
func TestADirectoryWhoseHeadLogDoesNotVerifyIsItsOwnCard(t *testing.T) {
	boost, trad := twoEngineDirs(t)
	path := filepath.Join(boost, "run1.log")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-40] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := get(newHandlerFor(nil, []string{boost, trad}, "SPY"), http.MethodGet, "/")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "boost: journal does not verify") || !strings.Contains(body, "halted") {
		t.Fatalf("status %d body %q, want boost's card and trad-ira's figures", rec.Code, body)
	}
}

func TestADirectoryWhoseHeadLogCannotBeReadIsItsOwnCard(t *testing.T) {
	boost, trad := twoEngineDirs(t)
	fail := filepath.Join(boost, "run1.log")
	readFile := func(p string) ([]byte, error) {
		if p == fail {
			return nil, os.ErrPermission
		}
		return os.ReadFile(p)
	}
	rec := get(newHandlerWith(source{dirs: []string{boost, trad}}, "SPY", readFile), http.MethodGet, "/")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "boost: journal unavailable") || !strings.Contains(body, "halted") {
		t.Fatalf("status %d body %q", rec.Code, body)
	}
}

// TestAMessageCardCarriesNoFigures: in /api/summary.json a degraded account
// has its name and message and no summary or alerts, so it cannot be taken for
// an account with a zero balance.
func TestAMessageCardCarriesNoFigures(t *testing.T) {
	boost, _ := twoEngineDirs(t)
	empty := filepath.Join(t.TempDir(), "trad-ira")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := get(newHandlerFor(nil, []string{boost, empty}, "SPY"), http.MethodGet, "/api/summary.json")
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || len(raw) != 2 {
		t.Fatalf("body %q, err %v", rec.Body, err)
	}
	if _, ok := raw[0]["Summary"]; !ok {
		t.Errorf("a healthy account lacks its summary: %v", raw[0])
	}
	for _, key := range []string{"Summary", "Alerts"} {
		if _, ok := raw[1][key]; ok {
			t.Errorf("the message card carries %s: %s", key, rec.Body)
		}
	}
	if _, ok := raw[1]["Message"]; !ok {
		t.Errorf("the message card lacks its message: %s", rec.Body)
	}
}

// TestMessageCardAnchorsAreUnique: two directories of the same name, and a
// healthy account, each get their own anchor.
func TestMessageCardAnchorsAreUnique(t *testing.T) {
	var dirs []string
	for _, root := range []string{t.TempDir(), t.TempDir()} {
		d := filepath.Join(root, "boost")
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, d)
	}
	body := get(newHandlerFor(nil, dirs, "SPY"), http.MethodGet, "/").Body.String()
	for _, want := range []string{`id="dir-0"`, `id="dir-1"`, `href="#dir-1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	if strings.Contains(body, `id="acct-boost"`) {
		t.Error("a message card must not use an account's anchor")
	}
}
