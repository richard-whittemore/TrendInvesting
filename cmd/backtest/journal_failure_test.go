package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failingWriter models an unavailable operator report stream.
type failingWriter struct{}

var errReport = errors.New("the report stream failed")

func (failingWriter) Write([]byte) (int, error) { return 0, errReport }

// TestAJournalThatCannotBeWrittenIsReported checks that a completed run
// reports failure to persist its only durable evidence (ADR 0017). A missing
// destination directory prevents temporary-file creation.
func TestAJournalThatCannotBeWrittenIsReported(t *testing.T) {
	out := filepath.Join(t.TempDir(), "no-such-directory", "journal.jsonl")

	err := backtest(context.Background(), options{configPath: configurationFixture, barsPath: barsFixture, outPath: out, build: testBuild}, failingWriter{})
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

// TestAReportThatCannotBeWrittenIsReported requires a command failure when
// the operator cannot be told where the completed run's evidence landed.
// The journal must survive the reporting failure (ADR 0017; AGENTS.md rule 6).
func TestAReportThatCannotBeWrittenIsReported(t *testing.T) {
	out := filepath.Join(t.TempDir(), "journal.jsonl")

	err := backtest(context.Background(), options{configPath: configurationFixture, barsPath: barsFixture, outPath: out, build: testBuild}, failingWriter{})
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
