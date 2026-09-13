package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/buildinfo"
	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// updateGolden rewrites the committed golden journal instead of asserting
// against it. Regenerating is a deliberate act: the golden journal is the
// recorded behaviour of the whole platform over a fixed fixture, so a diff
// in it is a change in what this system decides, to be read rather than
// accepted.
var updateGolden = flag.Bool("update", false, "rewrite the golden journal from this run")

const (
	configurationFixture = "testdata/configuration.json"
	barsFixture          = "testdata/bars.json"
	goldenJournal        = "testdata/journal.golden.jsonl"
)

// runBacktestTo runs the fixture and returns the journal it wrote.
func runBacktestTo(t *testing.T, args ...string) ([]byte, string) {
	t.Helper()

	out := filepath.Join(t.TempDir(), "journal.jsonl")
	full := append([]string{"-config", configurationFixture, "-bars", barsFixture, "-out", out}, args...)

	var log bytes.Buffer
	if err := run(full, &log); err != nil {
		t.Fatalf("run(%v) error = %v\n%s", full, err, log.String())
	}
	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read the journal the command wrote: %v", err)
	}
	return written, out
}

func fixtureConfiguration(t *testing.T) event.ConfigurationPayload {
	t.Helper()
	raw, err := os.ReadFile(configurationFixture)
	if err != nil {
		t.Fatalf("read %s: %v", configurationFixture, err)
	}
	var cfg event.ConfigurationPayload
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode %s: %v", configurationFixture, err)
	}
	return cfg
}

// TestTheCommandTurnsABarFixtureIntoTheGoldenJournal is #19's headline: one
// documented command runs a fixture end to end and writes a journal.
func TestTheCommandTurnsABarFixtureIntoTheGoldenJournal(t *testing.T) {
	written, _ := runBacktestTo(t)

	if *updateGolden {
		if err := os.WriteFile(goldenJournal, written, 0o644); err != nil {
			t.Fatalf("rewrite the golden journal: %v", err)
		}
		t.Log("golden journal rewritten")
		return
	}

	want, err := os.ReadFile(goldenJournal)
	if err != nil {
		t.Fatalf("read the golden journal: %v", err)
	}
	if !bytes.Equal(written, want) {
		t.Fatalf("the journal differs from the golden one; run `go test ./cmd/backtest -update` to see the change as a diff and read it before accepting it")
	}
}

// TestRunningTheSameFixtureTwiceProducesIdenticalJournals: a journal is
// evidence, and evidence that varied between two runs of the same inputs
// would be worthless.
func TestRunningTheSameFixtureTwiceProducesIdenticalJournals(t *testing.T) {
	first, _ := runBacktestTo(t)
	second, _ := runBacktestTo(t)

	if !bytes.Equal(first, second) {
		t.Fatal("two runs of the same fixture produced different journals")
	}
}

// TestAZeroSlippageConfigurationIsRefused: ADR 0013 makes a zero-slippage
// run invalid by construction, and the command says so before it processes
// a single bar rather than leaving the first fill to notice.
func TestAZeroSlippageConfigurationIsRefused(t *testing.T) {
	cfg := fixtureConfiguration(t)
	cfg.SlippageN = 0

	dir := t.TempDir()
	configPath := filepath.Join(dir, "zero-slippage.json")
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := os.WriteFile(configPath, encoded, 0o600); err != nil {
		t.Fatalf("write the configuration: %v", err)
	}
	journalPath := filepath.Join(dir, "journal.jsonl")

	var log bytes.Buffer
	err = run([]string{"-config", configPath, "-bars", barsFixture, "-out", journalPath}, &log)
	if err == nil {
		t.Fatal("run() error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "ADR 0013") {
		t.Fatalf("run() error = %v, want one naming ADR 0013", err)
	}
	if _, statErr := os.Stat(journalPath); statErr == nil {
		t.Fatal("the command wrote a journal for a run it refused")
	}
}

// TestTheHeaderRecordsTheDerivedConfigurationHashAndStrategyVersion: both
// are derived from the configuration the run actually used, never passed in
// as a string (ADR 0016).
func TestTheHeaderRecordsTheDerivedConfigurationHashAndStrategyVersion(t *testing.T) {
	written, _ := runBacktestTo(t)

	header, _, err := journal.Read(bytes.NewReader(written))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}

	cfg := fixtureConfiguration(t)
	if want := event.ConfigurationHash(cfg); header.ConfigurationHash != want {
		t.Errorf("header configuration hash = %q, want %q", header.ConfigurationHash, want)
	}
	want := event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, buildinfo.Version)
	if header.StrategyVersion != want {
		t.Errorf("header strategy version = %q, want %q", header.StrategyVersion, want)
	}
	if header.JournalVersion != journal.FormatVersion {
		t.Errorf("header journal version = %d, want %d", header.JournalVersion, journal.FormatVersion)
	}
}

// TestTheHeaderSpansTheRunsFirstAndLastInputEventTime.
func TestTheHeaderSpansTheRunsFirstAndLastInputEventTime(t *testing.T) {
	written, _ := runBacktestTo(t)

	header, records, err := journal.Read(bytes.NewReader(written))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}

	first, last := records[0].Envelope.EventTime, records[0].Envelope.EventTime
	for _, record := range records {
		at := record.Envelope.EventTime
		if at.Before(first) {
			first = at
		}
		if at.After(last) {
			last = at
		}
	}
	if !header.SpanStart.Equal(first) {
		t.Errorf("header span starts at %s, the earliest event is at %s", header.SpanStart, first)
	}
	if !header.SpanEnd.Equal(last) {
		t.Errorf("header span ends at %s, the latest event is at %s", header.SpanEnd, last)
	}
}

// TestTheJournalHoldsEveryInputAndDecisionInOneContiguousSequence: #19's
// second acceptance criterion. Journal sequence numbers are contiguous from
// 1, and both kinds of event are present.
func TestTheJournalHoldsEveryInputAndDecisionInOneContiguousSequence(t *testing.T) {
	written, _ := runBacktestTo(t)

	_, records, err := journal.Read(bytes.NewReader(written))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}

	var inputs, decisions int
	for i, record := range records {
		if record.Sequence != uint64(i+1) {
			t.Fatalf("record %d states sequence %d", i+1, record.Sequence)
		}
		if err := record.Envelope.Validate(); err != nil {
			t.Fatalf("record %d holds an invalid envelope: %v", record.Sequence, err)
		}
		if record.Envelope.Source == strategy.Source {
			decisions++
		} else {
			inputs++
		}
	}
	if inputs == 0 || decisions == 0 {
		t.Fatalf("the journal holds %d input(s) and %d decision(s); it must hold both", inputs, decisions)
	}
}

// TestTheJournalTheCommandWritesVerifies: the chain the writer computed is
// the chain the verifier recomputes.
func TestTheJournalTheCommandWritesVerifies(t *testing.T) {
	_, path := runBacktestTo(t)

	var out bytes.Buffer
	if err := run([]string{"-verify", path}, &out); err != nil {
		t.Fatalf("run(-verify) error = %v", err)
	}
	if !strings.Contains(out.String(), "verified") {
		t.Fatalf("verify reported:\n%s", out.String())
	}
}

// TestVerifyRefusesAnEditedJournal: the documented way to check a journal
// catches an edit to it.
func TestVerifyRefusesAnEditedJournal(t *testing.T) {
	written, path := runBacktestTo(t)

	edited := bytes.Replace(written, []byte(`"source":"fixture"`), []byte(`"source":"forged"`), 1)
	if bytes.Equal(edited, written) {
		t.Fatal("the fixture no longer contains the text this test edits")
	}
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatalf("write the edited journal: %v", err)
	}

	var out bytes.Buffer
	err := run([]string{"-verify", path}, &out)
	if err == nil {
		t.Fatal("run(-verify) error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "chain is broken") {
		t.Fatalf("run(-verify) error = %v, want one naming the broken chain", err)
	}
}

// TestTheRunEndsByExpiringTheProposalItWasStillHolding: #68, end to end.
// The fixture's last bar breaks out without the entry ever filling, so the
// journal's final decision is that proposal's expiry rather than silence.
func TestTheRunEndsByExpiringTheProposalItWasStillHolding(t *testing.T) {
	written, _ := runBacktestTo(t)

	_, records, err := journal.Read(bytes.NewReader(written))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}

	final := records[len(records)-1].Envelope
	if final.Type != event.ProposalExpiredEventType {
		t.Fatalf("the journal's final record is %s, want %s", final.Type, event.ProposalExpiredEventType)
	}
	var expiry event.ProposalExpiredPayload
	if err := json.Unmarshal(final.Payload, &expiry); err != nil {
		t.Fatalf("decode the final expiry: %v", err)
	}
	if expiry.Reason != event.ExpiryReasonInputStreamEnded {
		t.Fatalf("the final expiry's reason is %q, want %q", expiry.Reason, event.ExpiryReasonInputStreamEnded)
	}

	// And the end-of-stream event that caused it is itself in the journal,
	// which is what lets a replay reproduce the expiry.
	var sawRunCompleted bool
	for _, record := range records {
		if record.Envelope.Type == event.RunCompletedEventType {
			sawRunCompleted = true
		}
	}
	if !sawRunCompleted {
		t.Fatal("the journal records no end-of-stream event, so a replay could not reproduce the expiry it caused")
	}
}

func TestTheCommandRefusesAnIncompleteInvocation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "no configuration", args: []string{"-bars", barsFixture, "-out", "j.jsonl"}, want: "-config"},
		{name: "no bars", args: []string{"-config", configurationFixture, "-out", "j.jsonl"}, want: "-bars"},
		{name: "no output", args: []string{"-config", configurationFixture, "-bars", barsFixture}, want: "-out"},
		{name: "missing bar file", args: []string{"-config", configurationFixture, "-bars", "testdata/absent.json", "-out", "j.jsonl"}, want: "absent.json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(tt.args, &out)
			if err == nil {
				t.Fatalf("run(%v) error = nil, want one naming %q", tt.args, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("run(%v) error = %v, want one naming %q", tt.args, err, tt.want)
			}
		})
	}
}
