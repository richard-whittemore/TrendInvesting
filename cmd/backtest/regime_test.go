package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/registry"
)

func TestRegimeReportsAndRepeatedEvaluation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	cfg := fixtureConfiguration(t)
	for i, id := range []string{"first", "second"} {
		var out bytes.Buffer
		if err := backtest(context.Background(), options{configPath: configurationFixture, barsPath: barsFixture, outPath: filepath.Join(filepath.Dir(root), id+".jsonl"), registryPath: root, runID: id, variant: "test-variant", build: testBuild}, &out); err != nil {
			t.Fatal(err)
		}
		dir, err := registry.Dir(event.ConfigurationHash(cfg))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(root, dir, id+".report"))
		if err != nil {
			t.Fatal(err)
		}
		var report researchResult
		if err := json.Unmarshal(raw, &report); err != nil {
			t.Fatal(err)
		}
		if report.Report.Designation != "out-of-sample" || len(report.Report.Windows) != 7 || report.Report.Full.Samples < 2 || report.Opening == nil || report.Opening.Repeat != (i == 1) {
			t.Fatalf("report %+v", report)
		}
		if !strings.Contains(out.String(), "1998–2000 late bull") || (i == 1 && !strings.Contains(out.String(), "new hypothesis")) {
			t.Fatalf("output %s", out.String())
		}
	}
}

func TestFitRejectsOutOfSampleBeforeExecution(t *testing.T) {
	dir := t.TempDir()
	err := run(context.Background(), []string{"-config", configurationFixture, "-bars", barsFixture, "-out", filepath.Join(dir, "journal"), "-fit"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "entirely in-sample") {
		t.Fatalf("fit err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "journal")); !os.IsNotExist(err) {
		t.Fatalf("fit wrote journal: %v", err)
	}
}

// TestFitRejectionDoesNotConsumeAnOpening: a -fit invocation refused before
// execution never touched the held-out data an opening protects, so it must
// not count as a prior exposure when the same Variant is genuinely evaluated
// out-of-sample for the first time (ADR 0012). The Baseline's conservative
// treatment of a legacy entry recorded with no report is not this case: this
// run reports, and its report states it reserved no opening.
func TestFitRejectionDoesNotConsumeAnOpening(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	rejected := options{configPath: configurationFixture, barsPath: barsFixture, outPath: filepath.Join(t.TempDir(), "rejected.jsonl"), registryPath: root, runID: "rejected", variant: "test-variant", build: testBuild, fit: true}
	if err := backtest(context.Background(), rejected, &bytes.Buffer{}); err == nil {
		t.Fatal("expected the fit to be refused")
	}
	var out bytes.Buffer
	first := options{configPath: configurationFixture, barsPath: barsFixture, outPath: filepath.Join(t.TempDir(), "first.jsonl"), registryPath: root, runID: "first", variant: "test-variant", build: testBuild}
	if err := backtest(context.Background(), first, &out); err != nil {
		t.Fatal(err)
	}
	dir, err := registry.Dir(event.ConfigurationHash(fixtureConfiguration(t)))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, dir, "first.report"))
	if err != nil {
		t.Fatal(err)
	}
	var report researchResult
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.Opening == nil || report.Opening.Repeat || len(report.Opening.Prior) != 0 {
		t.Fatalf("the run refused by -fit must not count as a prior opening: %+v", report.Opening)
	}
}

// flatBar is a minimal, valid, non-triggering bar: identical open, high, low
// and close, so a run over any number of them proposes nothing.
func flatBar(instrumentID string, at time.Time) event.CompletedBarPayload {
	view := event.PriceView{Open: 100, High: 100, Low: 100, Close: 100, Volume: 1000}
	splitAdjusted, raw := view, view
	splitAdjusted.View, raw.View = event.ViewSplitAdjusted, event.ViewRaw
	return event.CompletedBarPayload{InstrumentID: instrumentID, PeriodEnd: at, SplitAdjusted: splitAdjusted, Raw: raw}
}

// TestReportDesignationUsesTheReservedSpanNotThePartialRecord: prepareResearch
// reserves a held-out opening from the FULL input given to the run, before
// execution, because a Variant's out-of-sample exposure is what it was
// GIVEN to run over, not merely what it got through before stopping. A run
// bounded to a single record here never applies its second (out-of-sample)
// bar at all, so the journal's own span is entirely in-sample -- but the
// report must not then claim "in-sample" for a run whose opening was
// reserved because the full declared span crossed the split: that would
// contradict the very evidence (the .opening sidecar) sitting beside it
// (ADR 0012, Proposed amendment: "the designation uses all input dates").
func TestReportDesignationUsesTheReservedSpanNotThePartialRecord(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	bars := []event.CompletedBarPayload{
		flatBar("AAPL", time.Date(2015, time.December, 31, 0, 0, 0, 0, time.UTC)),
		flatBar("AAPL", time.Date(2016, time.January, 1, 0, 0, 0, 0, time.UTC)),
	}
	opts := options{configPath: configurationFixture, barsPath: writeBars(t, bars), outPath: filepath.Join(t.TempDir(), "journal.jsonl"), registryPath: root, runID: "partial", variant: "test-variant", build: testBuild, maxRecords: 1}
	var out bytes.Buffer
	if err := backtest(context.Background(), opts, &out); err == nil {
		t.Fatal("expected the record bound to stop the run before its second bar")
	}
	dir, err := registry.Dir(event.ConfigurationHash(fixtureConfiguration(t)))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, dir, "partial.report"))
	if err != nil {
		t.Fatal(err)
	}
	var report researchResult
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.Opening == nil {
		t.Fatal("a Variant run over a span crossing the split reserves an opening")
	}
	if report.Report.Designation != "mixed" {
		t.Fatalf("designation %q contradicts the opening reserved beside it: %+v", report.Report.Designation, report)
	}
}

func TestOpeningSurvivesFailedRun(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "runs")
	for i, id := range []string{"failed", "retry"} {
		var out bytes.Buffer
		opts := options{configPath: configurationFixture, barsPath: barsFixture, outPath: filepath.Join(dir, id), registryPath: root, runID: id, variant: "test-variant", build: testBuild}
		if i == 0 {
			opts.maxRecords = 2
		}
		err := backtest(context.Background(), opts, &out)
		if (i == 0) != (err != nil) {
			t.Fatalf("run %s err %v", id, err)
		}
		if i == 1 && !strings.Contains(out.String(), "new hypothesis") {
			t.Fatalf("retry not flagged: %s", out.String())
		}
	}
}

func TestOpeningLockAndConcurrentReservations(t *testing.T) {
	root := filepath.Join(t.TempDir(), "registry")
	p, err := registry.ResearchProtocol()
	if err != nil {
		t.Fatal(err)
	}
	cfg := fixtureConfiguration(t)
	if err := os.MkdirAll(filepath.Join(root, ".research-lock"), 0o750); err != nil {
		t.Fatal(err)
	}
	opts := options{registryPath: root, runID: "first", variant: "v"}
	if _, err := reserveOpening(opts, cfg, p); err == nil {
		t.Fatal("opened while registry locked")
	}
	if err := os.Remove(filepath.Join(root, ".research-lock")); err != nil {
		t.Fatal(err)
	}
	type result struct {
		o   *registry.Opening
		err error
		id  string
	}
	results := make(chan result, 2)
	gate := make(chan struct{})
	for _, id := range []string{"one", "two"} {
		go func(id string) {
			<-gate
			local := opts
			local.runID = id
			o, err := reserveOpening(local, cfg, p)
			results <- result{o, err, id}
		}(id)
	}
	close(gate)
	var successful []*registry.Opening
	var retry []string
	for range 2 {
		r := <-results
		if r.err != nil {
			retry = append(retry, r.id)
		} else {
			successful = append(successful, r.o)
		}
	}
	for _, id := range retry {
		opts.runID = id
		o, err := reserveOpening(opts, cfg, p)
		if err != nil {
			t.Fatal(err)
		}
		successful = append(successful, o)
	}
	firsts := 0
	for _, o := range successful {
		if !o.Repeat {
			firsts++
		}
	}
	if len(successful) != 2 || firsts != 1 {
		t.Fatalf("concurrent reservations %+v", successful)
	}
	opts.runID = "one"
	if _, err := reserveOpening(opts, cfg, p); err == nil {
		t.Fatal("opening id reused")
	}
}

func TestResearchSidecarsCannotBeOverwritten(t *testing.T) {
	root := t.TempDir()
	if err := installResearch(root, "run.report", map[string]int{"value": 1}); err != nil {
		t.Fatal(err)
	}
	if err := installResearch(root, "run.report", map[string]int{"value": 2}); err == nil {
		t.Fatal("overwrote report")
	}
	raw, err := os.ReadFile(filepath.Join(root, "run.report"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"value": 1`) {
		t.Fatalf("changed report %s", raw)
	}
}

// TestReportInstallFailureIsRecordedExplicitlyInTheEntry: an opening and a
// report are never deleted or overwritten (ADR 0018), so a run whose report
// fails to install cannot be silently retried under its own run id -- the
// remedy is that the failure is recorded, not hidden, in the entry
// registerRun already wrote for it, rather than an entry that looks exactly
// like a normal completed run beside a report that never arrived. This does
// not weaken exactly-once: a genuine second look still needs a new run id,
// which the existing repeat-flagging machinery then correctly reports as a
// new hypothesis.
func TestReportInstallFailureIsRecordedExplicitlyInTheEntry(t *testing.T) {
	root := t.TempDir()
	cfg := fixtureConfiguration(t)
	dir, err := registry.Dir(event.ConfigurationHash(cfg))
	if err != nil {
		t.Fatal(err)
	}
	// Occupies the report sidecar this run will try to install, so the
	// install's own exclusive link fails -- the same invariant
	// TestResearchSidecarsCannotBeOverwritten pins directly.
	if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, dir, "blocked.report"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := options{configPath: configurationFixture, barsPath: barsFixture, outPath: filepath.Join(t.TempDir(), "journal.jsonl"), registryPath: root, runID: "blocked", variant: registry.Baseline, build: testBuild}
	if err := backtest(context.Background(), opts, &bytes.Buffer{}); err == nil {
		t.Fatal("expected the occupied report sidecar to fail the command")
	}
	raw, err := os.ReadFile(filepath.Join(root, dir, "blocked.json"))
	if err != nil {
		t.Fatalf("the run's entry was not recorded: %v", err)
	}
	var entry registry.Entry
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Status != registry.StatusCompleted {
		t.Fatalf("status %q", entry.Status)
	}
	if entry.Detail == "" {
		t.Fatal("a run whose report failed to install must say so in its own Detail, not leave the entry looking complete")
	}
}

func TestPrepareResearchUsesFullInputSpan(t *testing.T) {
	p, err := registry.ResearchProtocol()
	if err != nil {
		t.Fatal(err)
	}
	cfg := fixtureConfiguration(t)
	for _, tc := range []struct {
		name       string
		opts       options
		bars       []event.CompletedBarPayload
		actions    []event.CorporateActionPayload
		bad        bool
		start, end time.Time
	}{
		{name: "fit in sample", opts: options{fit: true}, bars: []event.CompletedBarPayload{{PeriodEnd: p.Split.Add(-time.Second)}}, start: p.Split.Add(-time.Second), end: p.Split.Add(-time.Second)},
		{name: "fit crossing", opts: options{fit: true}, bars: []event.CompletedBarPayload{{PeriodEnd: p.Split.Add(-time.Second)}, {PeriodEnd: p.Split}}, bad: true, start: p.Split.Add(-time.Second), end: p.Split},
		{name: "late action", opts: options{fit: true}, bars: []event.CompletedBarPayload{{PeriodEnd: p.Split.Add(-time.Second)}}, actions: []event.CorporateActionPayload{{EffectiveAt: p.Split}}, bad: true, start: p.Split.Add(-time.Second), end: p.Split},
		{name: "unregistered Variant", opts: options{variant: "v"}, bars: []event.CompletedBarPayload{{PeriodEnd: p.Split}}, bad: true, start: p.Split, end: p.Split},
		{name: "empty fit", opts: options{fit: true}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, start, end, err := prepareResearch(tc.opts, cfg, tc.bars, tc.actions)
			if (err != nil) != tc.bad {
				t.Fatalf("error %v", err)
			}
			// The declared span is derived, and returned, even from a
			// refused attempt: finishResearch reports it regardless of
			// whether the attempt was permitted to run (ADR 0012, Proposed
			// amendment).
			if start != tc.start || end != tc.end {
				t.Fatalf("span %s..%s want %s..%s", start, end, tc.start, tc.end)
			}
		})
	}
}
