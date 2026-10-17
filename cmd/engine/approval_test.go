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
)

// writeApproval writes an approval naming previous with finalHash and
// returns its path.
func writeApproval(t *testing.T, dir, previous, finalHash string, latched bool) string {
	t.Helper()
	raw, err := json.Marshal(event.OperatorRunApprovedPayload{
		AccountID: "joint", PreviousLog: previous, PreviousLogFinalHash: finalHash, IncidentID: "incident-1",
		ApprovedBy: "owner", ApprovedAt: time.Date(2026, time.October, 17, 13, 0, 0, 0, time.UTC),
		Boost: event.BoostStateInForce{Latched: latched},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// refuse runs opts, bounded, and requires a refusal saying want that leaves
// no durable log behind.
func refuse(t *testing.T, name string, opts options, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := run(ctx, opts, io.Discard)
	cancel()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: run() = %v, want a refusal saying %q", name, err, want)
	}
	if _, statErr := os.Stat(opts.logPath); !os.IsNotExist(statErr) {
		t.Errorf("%s: the refused run left a durable log behind", name)
	}
}

// TestAFreshRunAfterAPriorRunNeedsTheOwnersApproval: a restart that is not a
// crash is a fresh run, and it decides nothing until the owner's recorded
// approval names the previous run's log and that log's final hash (owner
// decision 2026-10-16). The approval is the fresh log's second record, and
// the adapter's inputs follow it.
func TestAFreshRunAfterAPriorRunNeedsTheOwnersApproval(t *testing.T) {
	dir := t.TempDir()
	first := boostOptions(t, dir)
	first.logPath, first.outPath = filepath.Join(dir, "run-1.log"), filepath.Join(dir, "run-1.journal")
	stop := startEngine(t, first)
	feed := newBoostFeed(t, first.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 3)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	finalHash := recoverFile(t, first.logPath).FinalRecordHash

	fresh := boostOptions(t, dir)
	fresh.logPath, fresh.outPath = filepath.Join(dir, "run-2.log"), filepath.Join(dir, "run-2.journal")
	refuse(t, "no approval", fresh, "operator.run.approved")

	wrong := fresh
	wrong.approvalPath = writeApproval(t, dir, "run-1.log", "sha256:"+strings.Repeat("0", 64), true)
	refuse(t, "a wrong final hash", wrong, "final record hash")

	missing := fresh
	missing.approvalPath = writeApproval(t, dir, "run-0.log", finalHash, true)
	refuse(t, "an unknown previous log", missing, "run-0.log")

	resumed := fresh
	resumed.resumePath, resumed.approvalPath = first.logPath, writeApproval(t, dir, "run-1.log", finalHash, true)
	refuse(t, "an approval with -resume", resumed, "-approval and -resume")

	approved := fresh
	approved.approvalPath = writeApproval(t, dir, "run-1.log", finalHash, true)
	stop = startEngine(t, approved)
	feed = newBoostFeed(t, approved.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 3)
	feed.sessions(3, 4)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("the approved run: %v", err)
	}
	entries := recoverFile(t, approved.logPath).Entries
	if entries[1].Envelope.Type != event.OperatorRunApprovedEventType || entries[1].Envelope.Sequence != 2 {
		t.Fatalf("the fresh log's second record is %s at sequence %d, want the approval at 2", entries[1].Envelope.Type, entries[1].Envelope.Sequence)
	}
	for _, e := range entries {
		if e.Envelope.Type != event.TargetsDecidedEventType {
			continue
		}
		var p event.TargetsDecidedPayload
		if err := json.Unmarshal(e.Envelope.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if !p.Boost.LatchedBefore {
			t.Errorf("%s: the fresh run did not start from the approved latch", e.Envelope.ID)
		}
	}
}

// TestAnApprovalMustNameTheLatestRun: with several earlier logs in the
// directory, only the most recent can be approved, and an approval with no
// earlier run is refused rather than recorded as fact.
func TestAnApprovalMustNameTheLatestRun(t *testing.T) {
	dir := t.TempDir()
	hashes := map[string]string{}
	for i, name := range []string{"run-1", "run-2"} {
		opts := boostOptions(t, dir)
		opts.logPath, opts.outPath = filepath.Join(dir, name+".log"), filepath.Join(dir, name+".journal")
		if name == "run-2" {
			opts.approvalPath = writeApproval(t, dir, "run-1.log", hashes["run-1.log"], false)
		}
		if err := startEngine(t, opts)(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		hashes[name+".log"] = recoverFile(t, opts.logPath).FinalRecordHash
		at := time.Date(2026, time.October, 10+i, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(opts.logPath, at, at); err != nil {
			t.Fatal(err)
		}
	}
	older := boostOptions(t, dir)
	older.logPath, older.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	older.approvalPath = writeApproval(t, dir, "run-1.log", hashes["run-1.log"], false)
	refuse(t, "an approval of an older run", older, "run-2.log")

	empty := boostOptions(t, t.TempDir())
	empty.approvalPath = writeApproval(t, dir, "run-1.log", hashes["run-1.log"], false)
	refuse(t, "an approval with no earlier run", empty, "no earlier run")
}
