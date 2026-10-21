package report_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
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

// TestEarlierRunsOfAnotherAccountsOrADryRunsLogAreBroken: a log spliced into
// the chain from another account, or a dry run's, never feeds the history.
func TestEarlierRunsOfAnotherAccountsOrADryRunsLogAreBroken(t *testing.T) {
	for name, first := range map[string]*stream{"another account": newStreamFor(t, "ira"), "a dry run": newStreamFor(t, "joint")} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			first.bar(0, "SPY", 100)
			first.snapshot(0, 4000, 39)
			h1 := writeRun(t, dir, "run1.log", approvalOf("", ""), first.out)
			current := newStreamFor(t, "joint")
			if name == "a dry run" {
				setMode(t, current, event.TradingModePaper)
			}
			current.bar(1, "SPY", 101)
			current.snapshot(1, 4010, 39)
			writeRun(t, dir, "run2.log", approvalOf("run1.log", h1), current.out)
			h := report.EarlierRuns(filepath.Join(dir, "run2.log"), os.ReadFile)
			if h.Broken == nil || h.Broken.Previous != "run1.log" || len(h.Earlier) != 0 {
				t.Fatalf("broken = %v, earlier = %d; want run1.log refused and no earlier run", h.Broken, len(h.Earlier))
			}
		})
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
