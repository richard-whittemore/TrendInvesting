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

func TestPrepareResearchUsesFullInputSpan(t *testing.T) {
	p, err := registry.ResearchProtocol()
	if err != nil {
		t.Fatal(err)
	}
	cfg := fixtureConfiguration(t)
	for _, tc := range []struct {
		name    string
		opts    options
		bars    []event.CompletedBarPayload
		actions []event.CorporateActionPayload
		bad     bool
	}{
		{name: "fit in sample", opts: options{fit: true}, bars: []event.CompletedBarPayload{{PeriodEnd: p.Split.Add(-time.Second)}}},
		{name: "fit crossing", opts: options{fit: true}, bars: []event.CompletedBarPayload{{PeriodEnd: p.Split.Add(-time.Second)}, {PeriodEnd: p.Split}}, bad: true},
		{name: "late action", opts: options{fit: true}, bars: []event.CompletedBarPayload{{PeriodEnd: p.Split.Add(-time.Second)}}, actions: []event.CorporateActionPayload{{EffectiveAt: p.Split}}, bad: true},
		{name: "unregistered Variant", opts: options{variant: "v"}, bars: []event.CompletedBarPayload{{PeriodEnd: p.Split}}, bad: true},
		{name: "empty fit", opts: options{fit: true}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := prepareResearch(tc.opts, cfg, tc.bars, tc.actions)
			if (err != nil) != tc.bad {
				t.Fatalf("error %v", err)
			}
		})
	}
}
