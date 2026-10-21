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
	earlier, err := report.EarlierRuns(path, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	var bars []string
	for _, e := range earlier {
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
	earlier, err := report.EarlierRuns(filepath.Join(dir, "run1.log"), os.ReadFile)
	if err != nil || len(earlier) != 0 {
		t.Fatalf("earlier = %d, err = %v", len(earlier), err)
	}
}

func TestEarlierRunsOfAnEndOfRunJournalAreNone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "joint.journal")
	if err := os.WriteFile(path, []byte(`{"journal_version":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if earlier, err := report.EarlierRuns(path, os.ReadFile); err != nil || len(earlier) != 0 {
		t.Fatalf("earlier = %d, err = %v", len(earlier), err)
	}
}

func TestEarlierRunsRefuseABrokenChainNamingTheLink(t *testing.T) {
	t.Run("a log that is not the one approved", func(t *testing.T) {
		dir := t.TempDir()
		path := threeRuns(t, dir)
		// A later record in run 2 changes its final hash, so run 3's approval no longer names it.
		writeRun(t, dir, "run2.log", approvalOf("run1.log", "sha256:"+strings.Repeat("0", 64)), runStream(t, 5))
		_, err := report.EarlierRuns(path, os.ReadFile)
		var broken *restart.ChainError
		if !errors.As(err, &broken) || broken.Run != "run3.log" || broken.Previous != "run2.log" {
			t.Fatalf("err = %v, want a chain error between run3.log and run2.log", err)
		}
	})
	t.Run("a missing log", func(t *testing.T) {
		dir := t.TempDir()
		path := threeRuns(t, dir)
		if err := os.Remove(filepath.Join(dir, "run1.log")); err != nil {
			t.Fatal(err)
		}
		_, err := report.EarlierRuns(path, os.ReadFile)
		if err == nil || !strings.Contains(err.Error(), "run2.log") || !strings.Contains(err.Error(), "run1.log") {
			t.Fatalf("err = %v, want the link between run2.log and run1.log", err)
		}
	})
	t.Run("a log that is damaged", func(t *testing.T) {
		dir := t.TempDir()
		path := threeRuns(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "run1.log"), []byte("not a log\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := report.EarlierRuns(path, os.ReadFile); err == nil || !strings.Contains(err.Error(), "run1.log") {
			t.Fatalf("err = %v, want run1.log named", err)
		}
	})
}

func TestEarlierRunsRefuseACurrentLogThatIsMissingOrDamaged(t *testing.T) {
	dir := t.TempDir()
	if _, err := report.EarlierRuns(filepath.Join(dir, "none.log"), os.ReadFile); err == nil {
		t.Fatal("a missing current log must be refused")
	}
	path := filepath.Join(dir, "bad.log")
	if err := os.WriteFile(path, []byte(`{"log_version":1,"damaged":`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := report.EarlierRuns(path, os.ReadFile); err == nil {
		t.Fatal("a damaged current log must be refused")
	}
}
