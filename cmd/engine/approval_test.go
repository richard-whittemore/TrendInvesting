package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// writeApproval writes an approval naming previous with finalHash and
// returns its path.
func writeApproval(t *testing.T, dir, previous, finalHash string, latched bool) string {
	t.Helper()
	raw, err := json.Marshal(event.OperatorRunApprovedPayload{
		AccountID: "joint", PreviousLog: previous, PreviousLogFinalHash: finalHash,
		ApprovedBy: "owner", ApprovedAt: time.Date(2026, time.October, 17, 13, 0, 0, 0, time.UTC),
		Boost: event.BoostStateInForce{Latched: new(latched)},
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
	firstLog := recoverFile(t, first.logPath)
	finalHash := firstLog.FinalRecordHash
	latch, decided := lastLatch(t, firstLog.Entries)
	if !decided {
		t.Fatal("the first run decided nothing")
	}

	fresh := boostOptions(t, dir)
	fresh.logPath, fresh.outPath = filepath.Join(dir, "run-2.log"), filepath.Join(dir, "run-2.journal")
	refuse(t, "no approval", fresh, "operator.run.approved")

	wrong := fresh
	wrong.approvalPath = writeApproval(t, dir, "run-1.log", "sha256:"+strings.Repeat("0", 64), true)
	refuse(t, "a wrong final hash", wrong, "final record hash")

	unlatched := fresh
	unlatched.approvalPath = writeApproval(t, dir, "run-1.log", finalHash, !latch)
	refuse(t, "a latch the previous run's last decision does not hold", unlatched, fmt.Sprintf("latched_after %t", latch))

	missing := fresh
	missing.approvalPath = writeApproval(t, dir, "run-0.log", finalHash, latch)
	refuse(t, "an unknown previous log", missing, "run-0.log")

	resumed := fresh
	resumed.resumePath, resumed.approvalPath = first.logPath, writeApproval(t, dir, "run-1.log", finalHash, latch)
	refuse(t, "an approval with -resume", resumed, "-approval and -resume")

	approved := fresh
	approved.approvalPath = writeApproval(t, dir, "run-1.log", finalHash, latch)
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
		if p.Boost.LatchedBefore != latch {
			t.Errorf("%s: the fresh run did not start from the approved latch", e.Envelope.ID)
		}
		break
	}
}

// lastLatch returns the latched_after of the last strategy.targets.decided
// in entries, and whether there is one.
func lastLatch(t *testing.T, entries []journal.Entry) (latched, decided bool) {
	t.Helper()
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Envelope.Type != event.TargetsDecidedEventType {
			continue
		}
		var p event.TargetsDecidedPayload
		if err := json.Unmarshal(entries[i].Envelope.Payload, &p); err != nil {
			t.Fatal(err)
		}
		return p.Boost.LatchedAfter, true
	}
	return false, false
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
			// A new start: run-1 holds nothing past its configuration, and a
			// fresh run never repeats that start.
			opts.asOf = "2024-01-03T21:00:00Z"
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

	// run-2 ended before deciding, so its latch in force is its approval's.
	latched := boostOptions(t, dir)
	latched.logPath, latched.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	latched.approvalPath = writeApproval(t, dir, "run-2.log", hashes["run-2.log"], true)
	refuse(t, "a latch the previous run's approval does not hold", latched, "latched false")

	resumed := boostOptions(t, dir)
	resumed.logPath, resumed.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	resumed.resumePath = filepath.Join(dir, "run-1.log")
	refuse(t, "a resume of an older run", resumed, "run-2.log")

	empty := boostOptions(t, t.TempDir())
	empty.approvalPath = writeApproval(t, dir, "run-1.log", hashes["run-1.log"], false)
	refuse(t, "an approval with no earlier run", empty, "no earlier run")
}

// TestAnApprovalFileStatesEveryKey: the approval is written outside the
// engine, so one that leaves out Boost's latch or the incident in force is
// refused before anything is created, never read as "not latched" or "no
// incident" (ADR 0026, point 5; ADR 0027, the restart model).
func TestAnApprovalFileStatesEveryKey(t *testing.T) {
	dir := t.TempDir()
	first := boostOptions(t, dir)
	first.logPath, first.outPath = filepath.Join(dir, "run-1.log"), filepath.Join(dir, "run-1.journal")
	if err := startEngine(t, first)(); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	finalHash := recoverFile(t, first.logPath).FinalRecordHash
	base := `"account_id":"joint","previous_log":"run-1.log","previous_log_final_hash":"` + finalHash + `","incident_id":"","approved_by":"owner","approved_at":"2026-10-17T13:00:00Z"`
	for name, c := range map[string]struct{ raw, want string }{
		"no boost":    {`{` + base + `,"incident_in_force":null}`, "boost"},
		"no latch":    {`{` + base + `,"boost":{},"incident_in_force":null}`, "latched"},
		"null latch":  {`{` + base + `,"boost":{"latched":null},"incident_in_force":null}`, "latched"},
		"no incident": {`{` + base + `,"boost":{"latched":false}}`, "incident_in_force"},
	} {
		fresh := boostOptions(t, dir)
		fresh.logPath, fresh.outPath = filepath.Join(dir, "run-2.log"), filepath.Join(dir, "run-2.journal")
		fresh.approvalPath = filepath.Join(t.TempDir(), "approval.json")
		if err := os.WriteFile(fresh.approvalPath, []byte(c.raw), 0o600); err != nil {
			t.Fatal(err)
		}
		refuse(t, name, fresh, c.want)
	}
}

// TestACrashBeforeTheApprovalIsRecordedResumesWithIt: a fresh run records
// its configuration, then the owner's approval; a crash between the two
// leaves a log holding nothing past the configuration. Resuming it alone
// would start from no latch and no incident, losing the approved state in
// force (ADR 0026, point 5; ADR 0027, the restart model), so it is refused;
// it resumes with the same approval, checked against the run before it, and
// records it as the resumed log's second input.
func TestACrashBeforeTheApprovalIsRecordedResumesWithIt(t *testing.T) {
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
	firstLog := recoverFile(t, first.logPath)
	latch, _ := lastLatch(t, firstLog.Entries)

	// The crashed fresh run: its log holds the configuration only.
	scratch := t.TempDir()
	crashed := boostOptions(t, scratch)
	if err := startEngine(t, crashed)(); err != nil {
		t.Fatalf("the configuration-only run: %v", err)
	}
	if n := len(recoverFile(t, crashed.logPath).Entries); n != 1 {
		t.Fatalf("the configuration-only log holds %d records, want 1", n)
	}
	raw, err := os.ReadFile(crashed.logPath)
	if err != nil {
		t.Fatal(err)
	}
	crashedLog := filepath.Join(dir, "run-2.log")
	if err := os.WriteFile(crashedLog, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(crashedLog, later, later); err != nil {
		t.Fatal(err)
	}

	resumed := boostOptions(t, dir)
	resumed.logPath, resumed.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	resumed.resumePath = crashedLog
	refuse(t, "a resume without the approval", resumed, "-approval")

	wrong := resumed
	wrong.approvalPath = writeApproval(t, dir, "run-2.log", recoverFile(t, crashedLog).FinalRecordHash, latch)
	refuse(t, "an approval of the crashed run itself", wrong, "run-1.log")

	unlatched := resumed
	unlatched.approvalPath = writeApproval(t, dir, "run-1.log", firstLog.FinalRecordHash, !latch)
	refuse(t, "a latch the run before does not hold", unlatched, fmt.Sprintf("latched_after %t", latch))

	approved := resumed
	approved.approvalPath = writeApproval(t, dir, "run-1.log", firstLog.FinalRecordHash, latch)
	stop = startEngine(t, approved)
	feed = newBoostFeed(t, approved.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 3)
	feed.sessions(3, 4)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("the resumed run: %v", err)
	}
	entries := recoverFile(t, approved.logPath).Entries
	if entries[1].Envelope.Type != event.OperatorRunApprovedEventType || entries[1].Envelope.Sequence != 2 {
		t.Fatalf("the resumed log's second record is %s at sequence %d, want the approval at 2", entries[1].Envelope.Type, entries[1].Envelope.Sequence)
	}
	for _, e := range entries {
		if e.Envelope.Type != event.TargetsDecidedEventType {
			continue
		}
		var p event.TargetsDecidedPayload
		if err := json.Unmarshal(e.Envelope.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Boost.LatchedBefore != latch {
			t.Errorf("%s: the resumed run did not start from the approved latch", e.Envelope.ID)
		}
		break
	}
}

// engineStateEntry is a recorded strategy.engine.state decision.
func engineStateEntry(t *testing.T, p event.EngineStatePayload) journal.Entry {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return journal.Entry{Kind: journal.KindDecision, Envelope: event.Envelope{
		ID: "engine-state:joint:1", Type: event.EngineStateEventType, SchemaVersion: event.EngineStateScopedVersion, Payload: raw,
	}}
}

// TestAnApprovalNeverLoosensThePreviousRunsState: the previous log's
// strategy.engine.state records are the evidence of the state it ended in,
// and a fresh run resumes in that state and scope; leaving Degraded or
// Halted is never automatic (ADR 0019, amended 2026-09-24, "The state
// survives a restart"). So an approval of a run that ended Degraded or
// Halted either clears the incident by its ID (ADR 0019's recovery
// procedure) or carries it in force at least as tight as it ended.
func TestAnApprovalNeverLoosensThePreviousRunsState(t *testing.T) {
	degraded := func(ids ...string) journal.Entry {
		return engineStateEntry(t, event.EngineStatePayload{State: event.EngineStateDegraded, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "x", ScopeInstrumentIDs: ids})
	}
	halted := engineStateEntry(t, event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "x"})
	inForce := func(state string, ids ...string) *event.IncidentInForce {
		return &event.IncidentInForce{IncidentID: "engine-state:joint:1", State: state, ScopeInstrumentIDs: ids, SaleAttempted: new(false)}
	}
	for _, c := range []struct {
		name    string
		entries []journal.Entry
		clears  string
		in      *event.IncidentInForce
		refusal string
	}{
		{"normal, none in force", nil, "", nil, ""},
		{"degraded, cleared", []journal.Entry{degraded("SPY")}, "engine-state:joint:1", nil, ""},
		{"degraded, carried", []journal.Entry{degraded("SPY")}, "", inForce(event.EngineStateDegraded, "SPY"), ""},
		{"degraded, carried halted", []journal.Entry{degraded("SPY")}, "", inForce(event.EngineStateHalted), ""},
		{"degraded, scopes merged", []journal.Entry{degraded("SPY"), degraded("TLT")}, "", inForce(event.EngineStateDegraded, "SPY", "TLT"), ""},
		{"halted after degraded, carried", []journal.Entry{degraded("SPY"), halted}, "", inForce(event.EngineStateHalted), ""},
		{"degraded, dropped", []journal.Entry{degraded("SPY")}, "", nil, "ended degraded"},
		{"halted, dropped", []journal.Entry{halted}, "", nil, "ended halted"},
		{"halted, carried degraded", []journal.Entry{halted}, "", inForce(event.EngineStateDegraded, "SPY"), "ended halted"},
		{"degraded, scope narrowed", []journal.Entry{degraded("SPY"), degraded("TLT")}, "", inForce(event.EngineStateDegraded, "SPY"), "TLT"},
		{"degraded, another incident cleared", []journal.Entry{degraded("SPY")}, "incident-9", nil, "engine-state:joint:1"},
		{"halted, another incident cleared", []journal.Entry{halted}, "incident-9", nil, "engine-state:joint:1"},
		{"halted, carried under another id", []journal.Entry{halted}, "", otherIncident(event.EngineStateHalted), "engine-state:joint:1"},
		{"normal, an incident cleared", nil, "incident-9", nil, "no incident"},
	} {
		approval := event.OperatorRunApprovedPayload{IncidentID: c.clears, IncidentInForce: c.in}
		err := checkIncidentInForce(c.entries, approval, "run-1.log")
		switch {
		case c.refusal == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.refusal != "" && (err == nil || !strings.Contains(err.Error(), c.refusal)):
			t.Errorf("%s: checkIncidentInForce = %v, want a refusal saying %q", c.name, err, c.refusal)
		}
	}
}

// otherIncident is an incident in force under an ID the logs in these
// tests never record.
func otherIncident(state string, ids ...string) *event.IncidentInForce {
	return &event.IncidentInForce{IncidentID: "engine-state:joint:9", State: state, ScopeInstrumentIDs: ids, SaleAttempted: new(false)}
}

// approvalEntry is a recorded operator.run.approved input.
func approvalEntry(t *testing.T, p event.OperatorRunApprovedPayload) journal.Entry {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return journal.Entry{Kind: journal.KindInput, Envelope: event.Envelope{
		ID: "operator-run-approved:x", Type: event.OperatorRunApprovedEventType, SchemaVersion: event.OperatorRunApprovedSchemaVersion, Payload: raw,
	}}
}

// TestAnInheritedIncidentSurvivesARunThatRecordsNoStateChange: a fresh run
// restores the incident its approval carries in force without recording a
// strategy.engine.state decision, and refusing orders while held records
// none either, so the approved log's own approval is evidence of the state
// it ended in. An approval of that log must clear the incident or carry it
// at least as tight; leaving Degraded or Halted is never automatic (ADR
// 0019, amended 2026-09-24, "The state survives a restart").
func TestAnInheritedIncidentSurvivesARunThatRecordsNoStateChange(t *testing.T) {
	inForce := func(state string, ids ...string) *event.IncidentInForce {
		return &event.IncidentInForce{IncidentID: "engine-state:joint:1", State: state, ScopeInstrumentIDs: ids, SaleAttempted: new(false)}
	}
	inherited := func(in *event.IncidentInForce) journal.Entry {
		return approvalEntry(t, event.OperatorRunApprovedPayload{
			AccountID: "joint", PreviousLog: "run-0.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("0", 64),
			ApprovedBy: "owner", ApprovedAt: time.Date(2026, time.October, 17, 13, 0, 0, 0, time.UTC),
			Boost: event.BoostStateInForce{Latched: new(false)}, IncidentInForce: in,
		})
	}
	degradedTLT := engineStateEntry(t, event.EngineStatePayload{State: event.EngineStateDegraded, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "x", ScopeInstrumentIDs: []string{"TLT"}})
	degradedTLT.Envelope.ID = "engine-state:joint:2"
	for _, c := range []struct {
		name    string
		entries []journal.Entry
		clears  string
		in      *event.IncidentInForce
		refusal string
	}{
		{"inherited none", []journal.Entry{inherited(nil)}, "", nil, ""},
		{"inherited halted, dropped", []journal.Entry{inherited(inForce(event.EngineStateHalted))}, "", nil, "ended halted"},
		{"inherited halted, carried degraded", []journal.Entry{inherited(inForce(event.EngineStateHalted))}, "", inForce(event.EngineStateDegraded, "SPY"), "ended halted"},
		{"inherited halted, carried", []journal.Entry{inherited(inForce(event.EngineStateHalted))}, "", inForce(event.EngineStateHalted), ""},
		{"inherited halted, cleared", []journal.Entry{inherited(inForce(event.EngineStateHalted))}, "engine-state:joint:1", nil, ""},
		{"inherited degraded, dropped", []journal.Entry{inherited(inForce(event.EngineStateDegraded, "SPY"))}, "", nil, "ended degraded"},
		{"inherited degraded, widened later", []journal.Entry{inherited(inForce(event.EngineStateDegraded, "SPY")), degradedTLT}, "", inForce(event.EngineStateDegraded, "TLT"), "SPY"},
		{"inherited degraded, widened later, carried", []journal.Entry{inherited(inForce(event.EngineStateDegraded, "SPY")), degradedTLT}, "", inForce(event.EngineStateDegraded, "SPY", "TLT"), ""},
		{"inherited halted, degraded later", []journal.Entry{inherited(inForce(event.EngineStateHalted)), degradedTLT}, "", inForce(event.EngineStateDegraded, "TLT"), "ended halted"},
		{"inherited halted, another incident cleared", []journal.Entry{inherited(inForce(event.EngineStateHalted))}, "engine-state:joint:9", nil, "engine-state:joint:1"},
		{"inherited degraded, widened later, the later record cleared", []journal.Entry{inherited(inForce(event.EngineStateDegraded, "SPY")), degradedTLT}, "engine-state:joint:2", nil, "engine-state:joint:1"},
	} {
		approval := event.OperatorRunApprovedPayload{IncidentID: c.clears, IncidentInForce: c.in}
		err := checkIncidentInForce(c.entries, approval, "run-1.log")
		switch {
		case c.refusal == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.refusal != "" && (err == nil || !strings.Contains(err.Error(), c.refusal)):
			t.Errorf("%s: checkIncidentInForce = %v, want a refusal saying %q", c.name, err, c.refusal)
		}
	}
}

// configurationOnlyLog records a Boost run that stops before any input,
// leaving a log holding its configuration only, and installs a copy of it
// as dir/name modified at at: the log a start cut short leaves, whether a
// first run, a fresh run before its approval, or a resume of either.
func configurationOnlyLog(t *testing.T, dir, name string, at time.Time) string {
	t.Helper()
	scratch := t.TempDir()
	opts := boostOptions(t, scratch)
	if err := startEngine(t, opts)(); err != nil {
		t.Fatalf("the configuration-only run: %v", err)
	}
	raw, err := os.ReadFile(opts.logPath)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestARepeatedCrashBeforeTheApprovalIsRecordedResumesWithTheSameApproval:
// a resume of a start cut short copies its configuration before recording
// the approval, so a second crash in that window leaves another log holding
// the configuration only. Each such log is an attempt at the same start, so
// the approval to record is still the one naming the run before the first
// attempt (ADR 0027, the restart model; ADR 0026, point 5: the state in
// force is restored, never lost to a restart).
func TestARepeatedCrashBeforeTheApprovalIsRecordedResumesWithTheSameApproval(t *testing.T) {
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
	firstLog := recoverFile(t, first.logPath)
	latch, _ := lastLatch(t, firstLog.Entries)
	now := time.Now()
	configurationOnlyLog(t, dir, "run-2.log", now.Add(time.Hour))
	attempt := configurationOnlyLog(t, dir, "run-3.log", now.Add(2*time.Hour))

	resumed := boostOptions(t, dir)
	resumed.logPath, resumed.outPath = filepath.Join(dir, "run-4.log"), filepath.Join(dir, "run-4.journal")
	resumed.resumePath = attempt
	refuse(t, "a resume without the approval", resumed, "-approval")

	approved := resumed
	approved.approvalPath = writeApproval(t, dir, "run-1.log", firstLog.FinalRecordHash, latch)
	stop = startEngine(t, approved)
	if err := stop(); err != nil {
		t.Fatalf("the resumed run: %v", err)
	}
	entries := recoverFile(t, approved.logPath).Entries
	if len(entries) < 2 || entries[1].Envelope.Type != event.OperatorRunApprovedEventType {
		t.Fatalf("the resumed log does not record the approval as its second input: %d record(s)", len(entries))
	}
}

// TestACutShortFirstRunResumesWithoutAnApprovalAfterRepeatedCrashes: a
// first run needs no approval, and neither does a resume of its start cut
// short, however many attempts the directory holds; an attempt at the same
// start is never an earlier run an approval must name (ADR 0027, the
// restart model).
func TestACutShortFirstRunResumesWithoutAnApprovalAfterRepeatedCrashes(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	configurationOnlyLog(t, dir, "run-1.log", now.Add(time.Hour))
	attempt := configurationOnlyLog(t, dir, "run-2.log", now.Add(2*time.Hour))
	resumed := boostOptions(t, dir)
	resumed.logPath, resumed.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	resumed.resumePath = attempt
	if err := startEngine(t, resumed)(); err != nil {
		t.Fatalf("the resumed first run: %v", err)
	}
}

// TestAFreshRunCannotRepeatAStartThatRecordedNothing: a fresh run whose
// configuration input is the very one a log holding nothing past it opens
// with would leave a log no resume could tell from an attempt at that same
// start, so a crash before its approval was recorded would lose the
// approval (ADR 0027, the restart model). It is refused: resume that log,
// or declare a new start with -as-of.
func TestAFreshRunCannotRepeatAStartThatRecordedNothing(t *testing.T) {
	dir := t.TempDir()
	previous := configurationOnlyLog(t, dir, "run-1.log", time.Now())
	fresh := boostOptions(t, dir)
	fresh.logPath, fresh.outPath = filepath.Join(dir, "run-2.log"), filepath.Join(dir, "run-2.journal")
	fresh.approvalPath = writeApproval(t, dir, "run-1.log", recoverFile(t, previous).FinalRecordHash, false)
	refuse(t, "a repeated start", fresh, "-as-of")
}

// TestAFreshRunAfterACutShortStartIsCheckedAgainstTheRunBeforeIt: a log
// holding nothing past its configuration decided nothing and records no
// state, so a fresh run approved after it, with a new -as-of, is checked
// against the latest earlier log that holds more: the latch and incident
// that run ended with are still in force (ADR 0026, point 5; ADR 0019,
// amended 2026-09-24, "The state survives a restart"; ADR 0027, the
// restart model). The approval still names the latest log and its hash.
func TestAFreshRunAfterACutShortStartIsCheckedAgainstTheRunBeforeIt(t *testing.T) {
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
	latch, _ := lastLatch(t, recoverFile(t, first.logPath).Entries)
	cut := configurationOnlyLog(t, dir, "run-2.log", time.Now().Add(time.Hour))
	cutHash := recoverFile(t, cut).FinalRecordHash

	fresh := boostOptions(t, dir)
	fresh.logPath, fresh.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	fresh.asOf = "2024-01-03T21:00:00Z"
	unlatched := fresh
	unlatched.approvalPath = writeApproval(t, dir, "run-2.log", cutHash, !latch)
	refuse(t, "a latch the run before the cut-short start does not hold", unlatched, fmt.Sprintf("latched_after %t", latch))

	approved := fresh
	approved.approvalPath = writeApproval(t, dir, "run-2.log", cutHash, latch)
	if err := startEngine(t, approved)(); err != nil {
		t.Fatalf("the approved fresh run: %v", err)
	}
}

// committedPrefix installs dir/name as the first groups committed groups of
// the log at source, modified at at: the log a resume of source leaves when
// it stops partway through copying source's committed groups (ADR 0027 §6:
// the new log begins with the committed part of the old, byte for byte).
func committedPrefix(t *testing.T, source, dir, name string, groups int, at time.Time) string {
	t.Helper()
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(raw), "\n")
	kept, commits := lines[:1], 0
	for _, line := range lines[1:] {
		if commits == groups {
			break
		}
		kept = append(kept, line)
		if strings.Contains(line, `"commit"`) {
			commits++
		}
	}
	if commits != groups {
		t.Fatalf("%s commits %d group(s), fewer than %d", source, commits, groups)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(kept, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAResumeAbandonedPartwayIsNeverTheLatestRun: a resume creates its new
// log, then copies the old log's committed groups into it one by one (ADR
// 0027 §6), so a resume refused for divergence, or a crash during the copy,
// leaves a newer log holding a strict prefix of the old one's committed
// groups. That log is an abandoned attempt, not a run: the state it ends in
// is a cut-down copy of the old run's, so it is never the latest log. The
// old log is still the one resumed or approved, and the attempt is named
// when it is.
func TestAResumeAbandonedPartwayIsNeverTheLatestRun(t *testing.T) {
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
	firstLog := recoverFile(t, first.logPath)
	latch, _ := lastLatch(t, firstLog.Entries)
	abandoned := committedPrefix(t, first.logPath, dir, "run-2.log", 3, time.Now().Add(time.Hour))
	if n, all := len(recoverFile(t, abandoned).Entries), len(firstLog.Entries); n < 2 || n >= all {
		t.Fatalf("the abandoned attempt holds %d of %d records, want a strict prefix past the configuration", n, all)
	}

	resumedAttempt := boostOptions(t, dir)
	resumedAttempt.logPath, resumedAttempt.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	resumedAttempt.resumePath = abandoned
	refuse(t, "a resume of the abandoned attempt", resumedAttempt, "run-1.log")

	approvedAttempt := boostOptions(t, dir)
	approvedAttempt.logPath, approvedAttempt.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	approvedAttempt.approvalPath = writeApproval(t, dir, "run-2.log", recoverFile(t, abandoned).FinalRecordHash, latch)
	refuse(t, "an approval of the abandoned attempt", approvedAttempt, "run-1.log")

	resumed := boostOptions(t, dir)
	resumed.logPath, resumed.outPath = filepath.Join(dir, "run-3.log"), filepath.Join(dir, "run-3.journal")
	resumed.resumePath = first.logPath
	if err := startEngine(t, resumed)(); err != nil {
		t.Fatalf("the resume of the run the attempt abandoned: %v", err)
	}
}
