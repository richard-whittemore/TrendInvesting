package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/buildinfo"
	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// updateGolden regenerates the fixed fixture's recorded platform behaviour
// (ADR 0017). Review the resulting decision changes before accepting them.
var updateGolden = flag.Bool("update", false, "rewrite the golden journal from this run")

const (
	configurationFixture = "testdata/configuration.json"
	barsFixture          = "testdata/bars.json"
	goldenJournal        = "testdata/journal.golden.jsonl"

	// testBuild fixes golden-test provenance across developer, CI and release
	// builds so byte comparisons measure decisions (ADR 0017).
	testBuild = "test"
)

// runBacktestTo runs the fixture under a fixed build identifier and returns
// the journal it wrote, with the path it was written to.
func runBacktestTo(t *testing.T) (written []byte, path string) {
	t.Helper()
	return runBacktestAs(t, testBuild)
}

func runBacktestAs(t *testing.T, build string) (written []byte, path string) {
	t.Helper()

	out := filepath.Join(t.TempDir(), "journal.jsonl")
	opts := options{configPath: configurationFixture, barsPath: barsFixture, outPath: out, build: build}

	var log bytes.Buffer
	if err := backtest(context.Background(), opts, &log); err != nil {
		t.Fatalf("backtest(%+v) error = %v\n%s", opts, err, log.String())
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

// TestTheCommandTurnsABarFixtureIntoTheGoldenJournal checks the documented
// end-to-end command against committed decision evidence (ADR 0017).
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

// TestRunningTheSameFixtureTwiceProducesIdenticalJournals checks ADR 0017's
// byte-identical evidence requirement for repeated inputs.
func TestRunningTheSameFixtureTwiceProducesIdenticalJournals(t *testing.T) {
	first, _ := runBacktestTo(t)
	second, _ := runBacktestTo(t)

	if !bytes.Equal(first, second) {
		t.Fatal("two runs of the same fixture produced different journals")
	}
}

// TestTheBuildIdentifierChangesNothingButTheStrategyVersion checks that
// build provenance is recorded but cannot change decisions (ADR 0016).
func TestTheBuildIdentifierChangesNothingButTheStrategyVersion(t *testing.T) {
	alphaRaw, _ := runBacktestAs(t, "alpha")
	betaRaw, _ := runBacktestAs(t, "beta")

	alphaHeader, alphaRecords, err := journal.Read(bytes.NewReader(alphaRaw))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}
	betaHeader, betaRecords, err := journal.Read(bytes.NewReader(betaRaw))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}

	if !strings.HasSuffix(alphaHeader.StrategyVersion, "+alpha") || !strings.HasSuffix(betaHeader.StrategyVersion, "+beta") {
		t.Fatalf("the build identifier did not reach the header: %q and %q", alphaHeader.StrategyVersion, betaHeader.StrategyVersion)
	}

	alphaHeader.StrategyVersion, betaHeader.StrategyVersion = "", ""
	if alphaHeader != betaHeader {
		t.Fatalf("two builds produced different headers:\n %+v\n %+v", alphaHeader, betaHeader)
	}

	if len(alphaRecords) != len(betaRecords) {
		t.Fatalf("two builds recorded %d and %d records", len(alphaRecords), len(betaRecords))
	}
	for i := range alphaRecords {
		alpha, beta := alphaRecords[i], betaRecords[i]
		if alpha.Sequence != beta.Sequence || alpha.Kind != beta.Kind {
			t.Fatalf("record %d differs in sequence or kind: %d %s vs %d %s", i+1, alpha.Sequence, alpha.Kind, beta.Sequence, beta.Kind)
		}
		if !strings.HasSuffix(alpha.Envelope.StrategyVersion, "+alpha") || !strings.HasSuffix(beta.Envelope.StrategyVersion, "+beta") {
			t.Fatalf("record %d does not carry its own build: %q and %q", i+1, alpha.Envelope.StrategyVersion, beta.Envelope.StrategyVersion)
		}
		alpha.Envelope.StrategyVersion, beta.Envelope.StrategyVersion = "", ""
		if !reflect.DeepEqual(alpha.Envelope, beta.Envelope) {
			t.Fatalf("record %d (%s) differs between two builds in something other than the strategy version", i+1, alpha.Envelope.Type)
		}
	}
}

// TestTheCommandStampsTheRunningBuild checks main's buildinfo.Version
// wiring (ADR 0016), which the fixed-build golden tests bypass.
func TestTheCommandStampsTheRunningBuild(t *testing.T) {
	out := filepath.Join(t.TempDir(), "journal.jsonl")

	var log bytes.Buffer
	if err := run(context.Background(), []string{"-config", configurationFixture, "-bars", barsFixture, "-out", out}, &log); err != nil {
		t.Fatalf("run() error = %v\n%s", err, log.String())
	}
	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	header, _, err := journal.Read(bytes.NewReader(written))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}
	if want := "+" + buildinfo.Version; !strings.HasSuffix(header.StrategyVersion, want) {
		t.Fatalf("header strategy version = %q, want it to end with %q", header.StrategyVersion, want)
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
	err = run(context.Background(), []string{"-config", configPath, "-bars", barsFixture, "-out", journalPath}, &log)
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
	want := event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, testBuild)
	if header.StrategyVersion != want {
		t.Errorf("header strategy version = %q, want %q", header.StrategyVersion, want)
	}
	if header.JournalVersion != journal.FormatVersion {
		t.Errorf("header journal version = %d, want %d", header.JournalVersion, journal.FormatVersion)
	}
}

// TestTheHeaderSpansTheRunsFirstAndLastInputEventTime checks the input-time
// span required by ADR 0017; all recorded decisions must fall inside it.
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

// TestTheJournalHoldsEveryInputAndDecisionInOneContiguousSequence checks
// ADR 0017's journal order: both kinds are present, numbered from one.
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
		switch record.Kind {
		case journal.KindDecision:
			decisions++
		case journal.KindInput:
			inputs++
		default:
			t.Fatalf("record %d states kind %q", record.Sequence, record.Kind)
		}
	}
	if inputs == 0 || decisions == 0 {
		t.Fatalf("the journal holds %d input(s) and %d decision(s); it must hold both", inputs, decisions)
	}
}

// TestTheJournalTheCommandWritesVerifies checks the command's chain
// verification path against its own output (ADR 0017).
func TestTheJournalTheCommandWritesVerifies(t *testing.T) {
	_, path := runBacktestTo(t)

	var out bytes.Buffer
	if err := run(context.Background(), []string{"-verify", path}, &out); err != nil {
		t.Fatalf("run(-verify) error = %v", err)
	}
	if !strings.Contains(out.String(), "verified") {
		t.Fatalf("verify reported:\n%s", out.String())
	}
}

// TestVerifyRefusesAnEditedJournal checks that the documented verification
// command rejects an edit without hash repair (ADR 0017).
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
	err := run(context.Background(), []string{"-verify", path}, &out)
	if err == nil {
		t.Fatal("run(-verify) error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "chain is broken") {
		t.Fatalf("run(-verify) error = %v, want one naming the broken chain", err)
	}
}

// TestTheRunEndsByExpiringTheProposalItWasStillHolding checks the terminal
// event for ADR 0011's one-bar proposal lifetime when no next bar arrives.
// The fixture ends holding an exit proposal its stops pre-empted; its expiry
// and causing end-of-stream input must both be journalled for replay (ADR
// 0017).
func TestTheRunEndsByExpiringTheProposalItWasStillHolding(t *testing.T) {
	written, _ := runBacktestOnBars(t, writeBars(t, barsEndingWithAnOutstandingExit(t)))

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
	if expiry.Reason != event.ExpiryReasonInputStreamEnded || expiry.Kind != event.ProposalKindExit {
		t.Fatalf("the final expiry is a %q proposal for %q, want the outstanding %q proposal for %q", expiry.Kind, expiry.Reason, event.ProposalKindExit, event.ExpiryReasonInputStreamEnded)
	}

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

// TestTheCommandRefusesToOverwriteAnExistingJournal: a journal is recorded
// evidence, and ADR 0018 forbids rewriting or deleting it. A rerun
// that pointed at an existing journal would destroy the earlier run's
// evidence before it had even validated its own configuration.
func TestTheCommandRefusesToOverwriteAnExistingJournal(t *testing.T) {
	out := filepath.Join(t.TempDir(), "journal.jsonl")
	existing := []byte("the previous run's evidence\n")
	if err := os.WriteFile(out, existing, 0o600); err != nil {
		t.Fatalf("write the existing journal: %v", err)
	}

	var log bytes.Buffer
	err := backtest(context.Background(), options{configPath: configurationFixture, barsPath: barsFixture, outPath: out, build: testBuild}, &log)
	if err == nil {
		t.Fatal("backtest() error = nil, want a refusal to overwrite")
	}
	if !strings.Contains(err.Error(), out) {
		t.Errorf("backtest() error = %v, want one naming %s", err, out)
	}

	after, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatalf("read the journal back: %v", readErr)
	}
	if !bytes.Equal(after, existing) {
		t.Fatalf("the existing journal was modified:\n before %q\n after  %q", existing, after)
	}
}

// TestTheWriteItselfRefusesADestinationThatAppearedLate checks ADR 0017's
// atomic refusal at installation. The early path check only saves run time;
// a concurrent run or restored backup may occupy the destination afterward.
// A rename would silently destroy that evidence.
func TestTheWriteItselfRefusesADestinationThatAppearedLate(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "journal.jsonl")
	existing := []byte("a journal that appeared after the run started\n")
	if err := os.WriteFile(out, existing, 0o600); err != nil {
		t.Fatalf("write the existing journal: %v", err)
	}

	// The span is the one input's own event time: a header stating any other
	// is refused before the destination is reached.
	header := journal.NewHeader("sha256:abc", "turtle-baseline/1.1.0+test", time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC())
	entries := []journal.Entry{{Kind: journal.KindInput, Envelope: validEnvelopeForWrite()}}

	_, err := writeJournal(out, header, entries)
	if err == nil {
		t.Fatal("writeJournal() error = nil, want a refusal to replace the destination")
	}
	if !strings.Contains(err.Error(), out) {
		t.Errorf("writeJournal() error = %v, want one naming %s", err, out)
	}

	after, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatalf("read the journal back: %v", readErr)
	}
	if !bytes.Equal(after, existing) {
		t.Fatalf("the destination was replaced:\n before %q\n after  %q", existing, after)
	}

	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	if len(left) != 1 {
		t.Fatalf("the refused write left %d files behind, want only the original", len(left))
	}
}

// TestConcurrentWritesLeaveExactlyOneJournal exercises ADR 0017's atomic
// installation with eight concurrent writes to an initially absent path.
// Exactly one complete run must survive any scheduler interleaving; every
// loser must report an occupied path and remove its temporary file.
func TestConcurrentWritesLeaveExactlyOneJournal(t *testing.T) {
	const writers = 8

	dir := t.TempDir()
	out := filepath.Join(dir, "journal.jsonl")

	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A different configuration hash per writer, so the survivor
			// names which run actually installed it.
			header := journal.NewHeader(fmt.Sprintf("sha256:run-%d", i), "turtle-baseline/1.1.0+test", time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC())
			_, errs[i] = writeJournal(out, header, []journal.Entry{{Kind: journal.KindInput, Envelope: validEnvelopeForWrite()}})
		}(i)
	}
	wg.Wait()

	var won int
	for i, err := range errs {
		if err == nil {
			won++
			continue
		}
		if !strings.Contains(err.Error(), "already exists") {
			t.Errorf("writer %d failed for the wrong reason: %v", i, err)
		}
	}
	if won != 1 {
		t.Fatalf("%d of %d concurrent writes succeeded, want exactly 1", won, writers)
	}

	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read the surviving journal: %v", err)
	}
	verification, err := journal.Verify(bytes.NewReader(written))
	if err != nil {
		t.Fatalf("the surviving journal does not verify: %v", err)
	}
	if !strings.HasPrefix(verification.Header.ConfigurationHash, "sha256:run-") {
		t.Fatalf("the surviving journal's header is %q, want one writer's own", verification.Header.ConfigurationHash)
	}

	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	if len(left) != 1 {
		t.Fatalf("the losers left %d files behind, want only the journal", len(left))
	}
}

// validEnvelopeForWrite is the smallest envelope journal.Write accepts, so
// the tests above fail on the destination rather than on their fixture.
func validEnvelopeForWrite() event.Envelope {
	payload := json.RawMessage(`{}`)
	at := time.Unix(0, 0).UTC()
	return event.Envelope{
		ID:                "evt-1",
		Type:              "test.event",
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		SchemaVersion:     1,
		EventTime:         at,
		RecordedAt:        at,
		Sequence:          1,
		Source:            "fixture",
		StrategyVersion:   "turtle-baseline/1.1.0+test",
		ConfigurationHash: "sha256:abc",
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// TestAFailedWriteLeavesNothingAtTheDestination checks ADR 0017's complete
// journal installation: a failed temporary-file write must leave neither
// a partial destination nor a temporary file.
func TestAFailedWriteLeavesNothingAtTheDestination(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "journal.jsonl")

	// Invalid header validation fails after temporary-file creation,
	// exercising cleanup before installation.
	broken := journal.NewHeader("", "", time.Time{}, time.Time{})
	entries := []journal.Entry{{Kind: journal.KindInput, Envelope: event.Envelope{}}}

	if _, err := writeJournal(out, broken, entries); err == nil {
		t.Fatal("writeJournal() error = nil, want the invalid header refused")
	}

	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(%s) = %v, want the destination untouched", out, err)
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	if len(left) != 0 {
		names := make([]string, 0, len(left))
		for _, entry := range left {
			names = append(names, entry.Name())
		}
		t.Fatalf("a failed write left %v behind", names)
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
			err := run(context.Background(), tt.args, &out)
			if err == nil {
				t.Fatalf("run(%v) error = nil, want one naming %q", tt.args, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("run(%v) error = %v, want one naming %q", tt.args, err, tt.want)
			}
		})
	}
}
