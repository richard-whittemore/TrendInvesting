package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

// writeJSON writes v as JSON at a new path in a temporary folder.
func writeJSON(t *testing.T, name string, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// paperIRA writes the momentum fixture as a paper IRA's configuration, and
// returns its path and payload.
func paperIRA(t *testing.T) (string, event.TargetWeightConfigurationPayload) {
	t.Helper()
	cfg := boostConfiguration(t, momentumConfigPath)
	cfg.Mode, cfg.AccountID = modePaper, "DUIRA"
	return writeJSON(t, "paper-ira.json", cfg), cfg
}

// runPaperMonthMidRebalance runs a paper IRA's first run over the synthetic
// month with no venue reporting on its orders, so it ends with the month's
// buys working, and returns the log directory and the log.
func runPaperMonthMidRebalance(t *testing.T) (dir, log string, cfg event.TargetWeightConfigurationPayload, path string) {
	t.Helper()
	dir = t.TempDir()
	path, cfg = paperIRA(t)
	opts := momentumOptions(t, dir)
	opts.mode, opts.configPath = modePaper, path
	opts.approvalPath = writeJSON(t, "first-run.json", event.OperatorRunApprovedPayload{
		AccountID: "DUIRA", ApprovedBy: "owner", ApprovedAt: time.Date(2025, time.January, 2, 13, 0, 0, 0, time.UTC),
		Momentum:     &event.MomentumStateInForce{},
		StartingBook: &event.StartingBook{Positions: []event.AccountPosition{}, TotalCash: 10000, Currency: "USD"},
	})
	stop := startEngine(t, opts)
	feed := newMomentumFeed(t, opts.socketPath, cfg, "test-build", approvalSequence+1)
	for _, in := range readMonth(t, momentumMonthPath) {
		feed.send(in)
	}
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("stop the first run: %v", err)
	}
	return dir, opts.logPath, cfg, path
}

// TestAFreshIRARunStartsOnlyFromTheRebalanceItsPreviousLogEndsWith: a fresh
// run cannot see the earlier run's working orders, so a restart
// mid-rebalance would decide as if nothing were working (ADR 0026, point 5:
// "A restart mid-rebalance sends nothing for that decision time until the
// owner clears it"). The engine checks the approval's rebalance in progress
// against the previous log before it starts: one stating none where the log
// ends mid-rebalance, one naming a rebalance the log does not hold, and a
// Boost-shaped one are refused, leaving nothing behind; the approval
// `operator approve` writes from the log starts the run, which records it.
func TestAFreshIRARunStartsOnlyFromTheRebalanceItsPreviousLogEndsWith(t *testing.T) {
	dir, log, cfg, path := runPaperMonthMidRebalance(t)
	recovery := recoverFile(t, log)
	end, err := restart.ReadEndState(recovery.Entries, cfg.AccountID, "run.log")
	if err != nil {
		t.Fatal(err)
	}
	if end.Rebalance == nil || end.Rebalance.Phase != event.RebalancePhaseBuysWorking {
		t.Fatalf("setup: the first run ends with %+v in progress, want the month's buys working", end.Rebalance)
	}
	proposed, err := restart.ProposeApproval(end, restart.ApprovalRequest{
		AccountID: cfg.AccountID, PreviousLog: "run.log", PreviousLogFinalHash: recovery.FinalRecordHash,
		ApprovedBy: "owner", ApprovedAt: time.Date(2025, time.February, 4, 13, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	fresh := func(name string, approval event.OperatorRunApprovedPayload) (options, error) {
		opts := momentumOptions(t, dir)
		opts.mode, opts.configPath, opts.asOf = modePaper, path, "2025-02-03T21:00:00Z"
		opts.outPath, opts.logPath = filepath.Join(dir, name+".journal"), filepath.Join(dir, name+".log")
		opts.approvalPath = writeJSON(t, name+"-approval.json", approval)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return opts, run(ctx, opts, io.Discard)
	}
	none := proposed
	none.Momentum = &event.MomentumStateInForce{}
	other := proposed
	r := *proposed.Momentum.RebalanceInProgress
	r.ClientOrderIDs = []string{"DUIRA-not-an-order"}
	other.Momentum = &event.MomentumStateInForce{RebalanceInProgress: &r}
	boost := proposed
	boost.Momentum, boost.Boost = nil, event.BoostStateInForce{Latched: new(false)}
	for name, c := range map[string]struct {
		approval event.OperatorRunApprovedPayload
		want     string
	}{
		"none": {none, "ends mid-rebalance"}, "other": {other, "cannot find"}, "boost": {boost, "Boost-shaped"},
	} {
		opts, err := fresh(name, c.approval)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: run() = %v, want a refusal saying %q", name, err, c.want)
		}
		if _, statErr := os.Stat(opts.logPath); !os.IsNotExist(statErr) {
			t.Errorf("%s: the refused run left a durable log behind", name)
		}
	}

	opts := momentumOptions(t, dir)
	opts.mode, opts.configPath, opts.asOf = modePaper, path, "2025-02-03T21:00:00Z"
	opts.outPath, opts.logPath = filepath.Join(dir, "fresh.journal"), filepath.Join(dir, "fresh.log")
	opts.approvalPath = writeJSON(t, "fresh-approval.json", proposed)
	if err := startEngine(t, opts)(); err != nil {
		t.Fatalf("the approval operator approve proposes: %v", err)
	}
	second := recoverFile(t, opts.logPath).Entries[1]
	if second.Kind != journal.KindInput || second.Envelope.Type != event.OperatorRunApprovedEventType || second.Envelope.SchemaVersion != event.OperatorRunApprovedSchemaVersion {
		t.Fatalf("the fresh run's second record is %s %s v%d, want the approval", second.Kind, second.Envelope.Type, second.Envelope.SchemaVersion)
	}
}

// TestAMomentumFirstRunRefusesABoostShapedApproval: a first run's approval
// states its strategy's state too, so a Boost-shaped one is refused for a
// momentum configuration, and the reverse.
func TestAMomentumFirstRunRefusesABoostShapedApproval(t *testing.T) {
	path, _ := paperIRA(t)
	opts := momentumOptions(t, t.TempDir())
	opts.mode, opts.configPath = modePaper, path
	opts.approvalPath = writeFirstRunApproval(t, "DUIRA")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := run(ctx, opts, io.Discard)
	cancel()
	if err == nil || !strings.Contains(err.Error(), "Boost-shaped") {
		t.Fatalf("a momentum first run with a Boost-shaped approval: %v", err)
	}

	dir := t.TempDir()
	boost := boostOptions(t, dir)
	boost.mode, boost.configPath = modePaper, writeBoostConfiguration(t, dir, modePaper, "DU1234567")
	boost.approvalPath = writeJSON(t, "ira-first-run.json", event.OperatorRunApprovedPayload{
		AccountID: "DU1234567", ApprovedBy: "owner", ApprovedAt: time.Date(2025, time.January, 2, 13, 0, 0, 0, time.UTC),
		Momentum:     &event.MomentumStateInForce{},
		StartingBook: &event.StartingBook{Positions: []event.AccountPosition{}, TotalCash: 4000, Currency: "USD"},
	})
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	err = run(ctx, boost, io.Discard)
	cancel()
	if err == nil || !strings.Contains(err.Error(), "momentum") {
		t.Fatalf("a Boost first run with an IRA approval: %v", err)
	}
}
