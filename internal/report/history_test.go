package report_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

// writeRun writes a durable log of the envelopes, each its own committed
// group, and returns its final record hash. approval, when set, is recorded
// after the configuration, as the engine does.
func writeRun(t *testing.T, dir, name string, approval *event.OperatorRunApprovedPayload, envelopes []event.Envelope) string {
	t.Helper()
	file, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	log, err := journal.CreateLog(file, journal.NewLogHeader("h", "v"))
	if err != nil {
		t.Fatal(err)
	}
	append1 := func(e event.Envelope) {
		t.Helper()
		if err := log.Append([]journal.Entry{{Kind: journal.KindInput, Envelope: e}}); err != nil {
			t.Fatal(err)
		}
	}
	append1(envelopes[0])
	if approval != nil {
		raw, err := json.Marshal(approval)
		if err != nil {
			t.Fatal(err)
		}
		append1(event.Envelope{ID: "approved:" + name, Type: event.OperatorRunApprovedEventType, EnvelopeVersion: event.CurrentEnvelopeVersion,
			SchemaVersion: event.OperatorRunApprovedSchemaVersion, EventTime: envelopes[0].EventTime, RecordedAt: envelopes[0].EventTime,
			Sequence: 2, Source: "test", StrategyVersion: "v", ConfigurationHash: "h", PayloadHash: event.HashPayload(raw), Payload: raw})
	}
	for _, e := range envelopes[1:] {
		append1(e)
	}
	return log.FinalRecordHash()
}

func approvalOf(previous, hash string) *event.OperatorRunApprovedPayload {
	latched := false
	p := &event.OperatorRunApprovedPayload{AccountID: "joint", ApprovedBy: "owner", ApprovedAt: day(0), Boost: event.BoostStateInForce{Latched: &latched}}
	if previous == "" {
		p.StartingBook = &event.StartingBook{Positions: []event.AccountPosition{}, TotalCash: 1000, Currency: "USD"}
		return p
	}
	p.PreviousLog, p.PreviousLogFinalHash = previous, hash
	return p
}

func runStream(t *testing.T, from int) []event.Envelope {
	s := newStream(t)
	s.bar(from, "SPY", 100+float64(from))
	s.snapshot(from, 4000+float64(10*from), 39)
	return s.out
}

// threeRuns writes a chain of three logs in dir and returns the newest's path.
func threeRuns(t *testing.T, dir string) string {
	t.Helper()
	h1 := writeRun(t, dir, "run1.log", approvalOf("", ""), runStream(t, 0))
	h2 := writeRun(t, dir, "run2.log", approvalOf("run1.log", h1), runStream(t, 1))
	writeRun(t, dir, "run3.log", approvalOf("run2.log", h2), runStream(t, 2))
	return filepath.Join(dir, "run3.log")
}

func TestEarlierRunsAreTheEnvelopesOfEveryRunBeforeTheCurrent(t *testing.T) {
	path := threeRuns(t, t.TempDir())
	h := report.EarlierRuns(path, os.ReadFile)
	if h.Broken != nil {
		t.Fatal(h.Broken)
	}
	var bars []string
	for _, e := range h.Earlier {
		if e.Type == event.CompletedBarEventType {
			bars = append(bars, e.EventTime.Format("2006-01-02"))
		}
	}
	if strings.Join(bars, ",") != day(0).Format("2006-01-02")+","+day(1).Format("2006-01-02") {
		t.Fatalf("earlier bars = %v, want run 1's then run 2's, and not the current run's", bars)
	}
}

func TestEarlierRunsOfAFirstRunAreNone(t *testing.T) {
	dir := t.TempDir()
	writeRun(t, dir, "run1.log", approvalOf("", ""), runStream(t, 0))
	h := report.EarlierRuns(filepath.Join(dir, "run1.log"), os.ReadFile)
	if h.Broken != nil || len(h.Earlier) != 0 {
		t.Fatalf("earlier = %d, broken = %v", len(h.Earlier), h.Broken)
	}
}

func TestEarlierRunsOfAnEndOfRunJournalAreNone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "joint.journal")
	if err := os.WriteFile(path, []byte(`{"journal_version":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if h := report.EarlierRuns(path, os.ReadFile); h.Broken != nil || len(h.Earlier) != 0 {
		t.Fatalf("earlier = %d, broken = %v", len(h.Earlier), h.Broken)
	}
}

// TestEarlierRunsOfABrokenChainNameTheLinkAndKeepTheIntactRuns: a chain
// that cannot be followed is never an error that refuses the journal: the
// history names the broken link and holds only the runs newer than it.
func TestEarlierRunsOfABrokenChainNameTheLinkAndKeepTheIntactRuns(t *testing.T) {
	t.Run("a log that is not the one approved", func(t *testing.T) {
		dir := t.TempDir()
		path := threeRuns(t, dir)
		// A later record in run 2 changes its final hash, so run 3's approval no longer names it.
		writeRun(t, dir, "run2.log", approvalOf("run1.log", "sha256:"+strings.Repeat("0", 64)), runStream(t, 5))
		h := report.EarlierRuns(path, os.ReadFile)
		if h.Broken == nil || h.Broken.Run != "run3.log" || h.Broken.Previous != "run2.log" || len(h.Earlier) != 0 {
			t.Fatalf("broken = %v, earlier = %d; want the link between run3.log and run2.log and no earlier run", h.Broken, len(h.Earlier))
		}
	})
	t.Run("a missing log", func(t *testing.T) {
		dir := t.TempDir()
		path := threeRuns(t, dir)
		if err := os.Remove(filepath.Join(dir, "run1.log")); err != nil {
			t.Fatal(err)
		}
		h := report.EarlierRuns(path, os.ReadFile)
		if h.Broken == nil || !strings.Contains(h.Broken.Error(), "run2.log") || !strings.Contains(h.Broken.Error(), "run1.log") {
			t.Fatalf("broken = %v, want the link between run2.log and run1.log", h.Broken)
		}
		var bars []string
		for _, e := range h.Earlier {
			if e.Type == event.CompletedBarEventType {
				bars = append(bars, e.EventTime.Format("2006-01-02"))
			}
		}
		if strings.Join(bars, ",") != day(1).Format("2006-01-02") {
			t.Fatalf("earlier bars = %v, want run 2's alone, the intact run before the current", bars)
		}
	})
	t.Run("a log that is damaged", func(t *testing.T) {
		dir := t.TempDir()
		path := threeRuns(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "run1.log"), []byte("not a log\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if h := report.EarlierRuns(path, os.ReadFile); h.Broken == nil || !strings.Contains(h.Broken.Error(), "run1.log") {
			t.Fatalf("broken = %v, want run1.log named", h.Broken)
		}
	})
}

func TestEarlierRunsOfACurrentLogThatIsMissingOrDamagedAreBroken(t *testing.T) {
	dir := t.TempDir()
	if h := report.EarlierRuns(filepath.Join(dir, "none.log"), os.ReadFile); h.Broken == nil || h.Broken.Run != "none.log" {
		t.Fatalf("a missing current log: broken = %v", h.Broken)
	}
	path := filepath.Join(dir, "bad.log")
	if err := os.WriteFile(path, []byte(`{"log_version":1,"damaged":`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if h := report.EarlierRuns(path, os.ReadFile); h.Broken == nil || h.Broken.Run != "bad.log" {
		t.Fatalf("a damaged current log: broken = %v", h.Broken)
	}
}

// TestEarlierRunsOfAnotherAccountsLogAreBroken: a log spliced into the
// chain from another account never feeds the history.
func TestEarlierRunsOfAnotherAccountsLogAreBroken(t *testing.T) {
	dir := t.TempDir()
	first := newStreamFor(t, "ira")
	first.bar(0, "SPY", 100)
	first.snapshot(0, 4000, 39)
	h1 := writeRun(t, dir, "run1.log", approvalOf("", ""), first.out)
	current := newStreamFor(t, "joint")
	current.bar(1, "SPY", 101)
	current.snapshot(1, 4010, 39)
	writeRun(t, dir, "run2.log", approvalOf("run1.log", h1), current.out)
	h := report.EarlierRuns(filepath.Join(dir, "run2.log"), os.ReadFile)
	if h.Broken == nil || h.Broken.Previous != "run1.log" || h.Broken.Cause != restart.CauseIdentity || len(h.Earlier) != 0 {
		t.Fatalf("broken = %v, earlier = %d; want run1.log refused and no earlier run", h.Broken, len(h.Earlier))
	}
}

// modeRun is a run of account joint in mode, started on session from (its
// configuration's time) with a close on that session.
func modeRun(t *testing.T, mode string, from int) []event.Envelope {
	return modeRunOf(t, "joint", mode, from)
}

// modeRunOf is modeRun for the named account.
func modeRunOf(t *testing.T, account, mode string, from int) []event.Envelope {
	s := newStreamFor(t, account)
	setMode(t, s, mode)
	s.out[0].EventTime, s.out[0].RecordedAt = day(from).Add(-time.Hour), day(from).Add(-time.Hour)
	s.bar(from, "SPY", 100+float64(from))
	s.snapshot(from, 4000+float64(10*from), 39)
	return s.out
}

// TestEarlierRunsKeepOnlyTheCurrentModesRuns: the engine accepts a paper run
// approved against a dry-run log and a live run against a paper log, so the
// chain is whole; the equity history holds only the runs of the current
// log's mode, from the earliest contiguous one, and says where it starts.
func TestEarlierRunsKeepOnlyTheCurrentModesRuns(t *testing.T) {
	t.Run("paper after a dry run", func(t *testing.T) {
		dir := t.TempDir()
		h1 := writeRun(t, dir, "run1.log", approvalOf("", ""), modeRun(t, event.TradingModeDryRun, 0))
		h2 := writeRun(t, dir, "run2.log", approvalOf("run1.log", h1), modeRun(t, event.TradingModePaper, 1))
		writeRun(t, dir, "run3.log", approvalOf("run2.log", h2), modeRun(t, event.TradingModePaper, 2))
		h := report.EarlierRuns(filepath.Join(dir, "run3.log"), os.ReadFile)
		if h.Broken != nil {
			t.Fatalf("a mode change the engine accepts is no break: %v", h.Broken)
		}
		var bars []string
		for _, e := range h.Earlier {
			if e.Type == event.CompletedBarEventType {
				bars = append(bars, e.EventTime.Format("2006-01-02"))
			}
		}
		if strings.Join(bars, ",") != day(1).Format("2006-01-02") {
			t.Fatalf("earlier bars = %v, want run 2's alone, the first paper run", bars)
		}
		if want := "history since " + day(1).Format("2006-01-02") + ", the first paper run"; report.HistorySince(h) != want {
			t.Fatalf("since = %q, want %q", report.HistorySince(h), want)
		}
	})
	t.Run("live after paper", func(t *testing.T) {
		dir := t.TempDir()
		h1 := writeRun(t, dir, "run1.log", approvalOf("", ""), modeRun(t, event.TradingModePaper, 0))
		writeRun(t, dir, "run2.log", approvalOf("run1.log", h1), modeRun(t, event.TradingModeLive, 1))
		h := report.EarlierRuns(filepath.Join(dir, "run2.log"), os.ReadFile)
		if h.Broken != nil || len(h.Earlier) != 0 {
			t.Fatalf("broken = %v, earlier = %d; want a whole chain and no earlier live run", h.Broken, len(h.Earlier))
		}
		if want := "history since " + day(1).Format("2006-01-02") + ", the first live run"; report.HistorySince(h) != want {
			t.Fatalf("since = %q, want %q", report.HistorySince(h), want)
		}
	})
	t.Run("one mode throughout", func(t *testing.T) {
		if h := report.EarlierRuns(threeRuns(t, t.TempDir()), os.ReadFile); report.HistorySince(h) != "" {
			t.Fatalf("since = %q, want none when every run is of one mode", report.HistorySince(h))
		}
	})
}

// TestEarlierRunsOfRecoveryUseTheCallersRecovery: a caller that has
// recovered the current log passes that recovery, so the log is not read or
// recovered again.
func TestEarlierRunsOfRecoveryUseTheCallersRecovery(t *testing.T) {
	dir := t.TempDir()
	path := threeRuns(t, dir)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	current, err := journal.RecoverLog(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	h := report.EarlierRunsOfRecovery(path, current, func(earlier string) (journal.Recovery, error) {
		if earlier == path {
			t.Fatal("the current log was recovered again")
		}
		raw, err := os.ReadFile(earlier)
		if err != nil {
			return journal.Recovery{}, err
		}
		return journal.RecoverLog(bytes.NewReader(raw))
	})
	if h.Broken != nil || len(h.Earlier) == 0 {
		t.Fatalf("broken = %v, earlier = %d", h.Broken, len(h.Earlier))
	}
}

// setMode rewrites the stream's configuration to mode.
func setMode(t *testing.T, s *stream, mode string) {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(s.out[0].Payload, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["mode"] = mode
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.out[0].Payload, s.out[0].PayloadHash = raw, event.HashPayload(raw)
}

// TestEarlierRunsNameAnEarlierLogThatCannotBeRead: the reason a file cannot
// be read is kept, without the server's path.
func TestEarlierRunsNameAnEarlierLogThatCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	path := threeRuns(t, dir)
	readFile := func(name string) ([]byte, error) {
		if filepath.Base(name) == "run1.log" {
			return nil, errors.New("disk fault")
		}
		return os.ReadFile(name)
	}
	h := report.EarlierRuns(path, readFile)
	if h.Broken == nil || h.Broken.Previous != "run1.log" || !strings.Contains(h.Broken.Error(), "disk fault") {
		t.Fatalf("broken = %v, want run1.log named with its reason", h.Broken)
	}
}

// TestLoadLogRecoversADurableLogOnce: the envelopes are Load's, and a
// durable log's recovery comes with them, nil for an end-of-run journal.
func TestLoadLogRecoversADurableLogOnce(t *testing.T) {
	path := threeRuns(t, t.TempDir())
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	envelopes, recovery, err := report.LoadLog(raw)
	_, want, _ := report.Load(raw)
	if err != nil || recovery == nil || len(envelopes) != len(want) || len(recovery.Entries) != len(want) || recovery.FinalRecordHash == "" {
		t.Fatalf("envelopes = %d, recovery = %v, err = %v", len(envelopes), recovery, err)
	}
	if _, _, err := report.LoadLog([]byte("not a journal\n")); err == nil {
		t.Fatal("a file that does not verify must refuse")
	}
}

// TestForHistoryKeepsWhatTheChainAndTheHistoryRead: of an earlier run's
// recovery, its first two records, its approvals and configurations, its
// snapshots, target decisions and the benchmark's bars are kept, nothing else, and the
// history it feeds is the whole recovery's.
func TestForHistoryKeepsWhatTheChainAndTheHistoryRead(t *testing.T) {
	s := newStream(t)
	s.bar(0, "SPY", 100)
	s.bar(0, "QQQ", 300)
	s.snapshot(0, 4000, 39)
	s.decided(0, 0.98)
	s.bar(1, "SPY", 101)
	s.snapshot(1, 4010, 39)
	var entries []journal.Entry
	for _, e := range s.out {
		entries = append(entries, journal.Entry{Kind: journal.KindInput, Envelope: e})
	}
	entries = append(entries, journal.Entry{Kind: journal.KindDecision, Envelope: s.out[1]})
	whole := journal.Recovery{Entries: entries, FinalRecordHash: "sha256:" + strings.Repeat("1", 64)}
	kept := report.ForHistory(whole, "SPY")
	var ids []string
	for _, e := range kept.Entries {
		ids = append(ids, e.Envelope.ID)
	}
	if strings.Join(ids, ",") != "configuration,bar:0,snapshot:0,targets:0,bar:1,snapshot:1" || kept.FinalRecordHash != whole.FinalRecordHash {
		t.Fatalf("kept %v", ids)
	}
	var envelopes []event.Envelope
	for _, e := range kept.Entries {
		envelopes = append(envelopes, e.Envelope)
	}
	current := newStream(t)
	current.bar(2, "SPY", 102)
	current.snapshot(2, 4020, 39)
	got, err := report.SummarizeWithHistory(report.History{Earlier: envelopes}, current.out, day(2).Format("2006-01-02"), "SPY")
	if err != nil || len(got.Equity.Points) != 3 {
		t.Fatalf("points = %d, err = %v", len(got.Equity.Points), err)
	}
}

// TestEarlierRunsOfACurrentLogWithNoApprovalHoldNoRun: a paper log that
// holds no approval is broken at itself, with no intact run.
func TestEarlierRunsOfACurrentLogWithNoApprovalHoldNoRun(t *testing.T) {
	dir := t.TempDir()
	s := newStream(t)
	setMode(t, s, event.TradingModePaper)
	s.bar(0, "SPY", 100)
	writeRun(t, dir, "run1.log", nil, s.out)
	h := report.EarlierRuns(filepath.Join(dir, "run1.log"), os.ReadFile)
	if h.Broken == nil || h.Broken.Run != "run1.log" || h.Broken.Cause != restart.CauseUnapproved || len(h.Earlier) != 0 || h.Since != nil {
		t.Fatalf("history = %+v", h)
	}
}

// TestEarlierRunsOfACurrentLogHoldingOnlyItsConfigurationAreAStart: the
// current log holding only its configuration, in any mode, is a start in
// progress, not a break: no history yet, and nothing to restore.
func TestEarlierRunsOfACurrentLogHoldingOnlyItsConfigurationAreAStart(t *testing.T) {
	for _, mode := range []string{event.TradingModeDryRun, event.TradingModePaper} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			writeRun(t, dir, "run1.log", approvalOf("", ""), runStream(t, 0))
			s := newStream(t)
			setMode(t, s, mode)
			writeRun(t, dir, "run2.log", nil, s.out)
			h := report.EarlierRuns(filepath.Join(dir, "run2.log"), os.ReadFile)
			if h.Broken != nil || !h.Starting || len(h.Earlier) != 0 || h.Since != nil {
				t.Fatalf("history = %+v, want a start in progress", h)
			}
			want := "the run is starting; no history yet"
			if mode != event.TradingModeDryRun {
				want = "the run started but recorded no approval; if the engine is not running, resume it with its approval (see the manual's \"Restarting an engine\" chapter, docs/manual/restart.md)"
			}
			if got := report.HistoryStarting(h); got != want {
				t.Fatalf("line = %q, want %q", got, want)
			}
		})
	}
	if got := report.HistoryStarting(report.EarlierRuns(threeRuns(t, t.TempDir()), os.ReadFile)); got != "" {
		t.Fatalf("a run past its start reads %q", got)
	}
}

// TestABrokenChainAcrossAModeChangeStartsAtTheFirstRunItIncludes: the
// comparison over the intact runs holds only the current mode's, so where it
// starts is the first of those, never an intact run of another mode it
// leaves out.
func TestABrokenChainAcrossAModeChangeStartsAtTheFirstRunItIncludes(t *testing.T) {
	dir := t.TempDir()
	const account = "DU1234567" // an IBKR paper account
	h2 := writeRun(t, dir, "dry2.log", approvalOf("dry1.log", "sha256:"+strings.Repeat("0", 64)), modeRunOf(t, account, event.TradingModeDryRun, 1))
	h3 := writeRun(t, dir, "paper1.log", approvalOf("dry2.log", h2), modeRunOf(t, account, event.TradingModePaper, 2))
	current := modeRunOf(t, account, event.TradingModePaper, 3)
	writeRun(t, dir, "paper2.log", approvalOf("paper1.log", h3), current)
	h := report.EarlierRuns(filepath.Join(dir, "paper2.log"), os.ReadFile)
	if h.Broken == nil || h.Since == nil || h.Since.Log != "paper1.log" {
		t.Fatalf("history = %+v, want a break and a history of paper runs", h)
	}
	got, err := report.SummarizeWithHistory(h, current, day(3).Format("2006-01-02"), "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.Equity.HistoryStartsAt != "paper1.log" {
		t.Fatalf("starts at %q, want paper1.log, the first run the comparison includes", got.Equity.HistoryStartsAt)
	}
	if a := report.HistoryUnavailableAlert(account, h, day(3)); !strings.Contains(a.Body, "runs from paper1.log") {
		t.Fatalf("alert body: %s", a.Body)
	}
}

// TestAHistoryAfterTheModeReturnsSaysTheEarlierRunsOfThatModeAreLeftOut:
// paper, then live, then paper again: the history starts at the latest
// change of mode, and the line says the paper runs before it are left out.
func TestAHistoryAfterTheModeReturnsSaysTheEarlierRunsOfThatModeAreLeftOut(t *testing.T) {
	dir := t.TempDir()
	h1 := writeRun(t, dir, "run1.log", approvalOf("", ""), modeRun(t, event.TradingModePaper, 0))
	h2 := writeRun(t, dir, "run2.log", approvalOf("run1.log", h1), modeRun(t, event.TradingModeLive, 1))
	writeRun(t, dir, "run3.log", approvalOf("run2.log", h2), modeRun(t, event.TradingModePaper, 2))
	h := report.EarlierRuns(filepath.Join(dir, "run3.log"), os.ReadFile)
	want := "history since " + day(2).Format("2006-01-02") + ", the first paper run since the mode last changed; the paper runs before that change are not included"
	if h.Broken != nil || report.HistorySince(h) != want {
		t.Fatalf("broken = %v, since = %q, want %q", h.Broken, report.HistorySince(h), want)
	}
}
