package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A run's journal is the only durable evidence it leaves, so the two places
// the command can fail AFTER the run itself has finished both have to be
// reported rather than absorbed. Neither is composition: the first decides
// whether the evidence lands at all, and the second decides whether an
// operator is told where it landed.

// failingWriter is the operator's own output stream, gone.
type failingWriter struct{}

var errReport = errors.New("the report stream failed")

func (failingWriter) Write([]byte) (int, error) { return 0, errReport }

// TestAJournalThatCannotBeWrittenIsReported: the destination directory does
// not exist, so the exclusive create that makes the journal cannot even
// begin. The run itself succeeded, and saying nothing would leave an
// operator with a completed backtest and no evidence of it.
func TestAJournalThatCannotBeWrittenIsReported(t *testing.T) {
	out := filepath.Join(t.TempDir(), "no-such-directory", "journal.jsonl")

	err := backtest(options{configPath: configurationFixture, barsPath: barsFixture, outPath: out, build: testBuild}, failingWriter{})
	if err == nil {
		t.Fatal("backtest() error = nil, want the journal it could not write to be reported")
	}
	if !strings.Contains(err.Error(), "create the journal") {
		t.Errorf("backtest() error = %v, want it to name what it could not do", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Errorf("os.Stat(%s) = %v, want the journal not to exist", out, statErr)
	}
}

// TestAReportThatCannotBeWrittenIsReported: the journal landed and the
// operator's own output stream did not. The journal is left exactly where it
// is — it is the evidence, and the report is only the note saying where to
// find it — but the command still fails, because a run that cannot tell
// anyone what it did has not finished.
func TestAReportThatCannotBeWrittenIsReported(t *testing.T) {
	out := filepath.Join(t.TempDir(), "journal.jsonl")

	err := backtest(options{configPath: configurationFixture, barsPath: barsFixture, outPath: out, build: testBuild}, failingWriter{})
	if !errors.Is(err, errReport) {
		t.Fatalf("backtest() error = %v, want the report stream's own failure", err)
	}
	if !strings.Contains(err.Error(), "report the run") {
		t.Errorf("backtest() error = %v, want it to name what it could not do", err)
	}
	if _, statErr := os.Stat(out); statErr != nil {
		t.Errorf("os.Stat(%s) error = %v, want the journal to have been written regardless", out, statErr)
	}
}
