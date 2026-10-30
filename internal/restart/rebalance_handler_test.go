package restart_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// handlerRun records inputs through the real momentum handler, as the
// engine's recorder does: an input the handler refuses is committed all
// the same, with no mark of the refusal.
type handlerRun struct {
	t        *testing.T
	recorder *journal.Recorder
	hash     string
	sequence uint64
	errs     map[string]error
}

func newHandlerRun(t *testing.T) (*handlerRun, event.TargetWeightConfigurationPayload) {
	t.Helper()
	return newHandlerRunWith(t, event.TradingModePaper, false)
}

// newHandlerRunIn is newHandlerRun for a run in mode.
func newHandlerRunIn(t *testing.T, mode string) (*handlerRun, event.TargetWeightConfigurationPayload) {
	t.Helper()
	return newHandlerRunWith(t, mode, false)
}

// newHandlerRunAwaiting is newHandlerRun for a fresh run the engine told to
// await its approval (cmd/engine's armApproval).
func newHandlerRunAwaiting(t *testing.T) (*handlerRun, event.TargetWeightConfigurationPayload) {
	t.Helper()
	return newHandlerRunWith(t, event.TradingModePaper, true)
}

func newHandlerRunWith(t *testing.T, mode string, awaiting bool) (*handlerRun, event.TargetWeightConfigurationPayload) {
	t.Helper()
	raw, err := os.ReadFile("../../cmd/engine/testdata/momentum-dry-run.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg event.TargetWeightConfigurationPayload
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Mode, cfg.AccountID = mode, iraLogAccount
	return newConfiguredHandlerRun(t, cfg, awaiting), cfg
}

func newConfiguredHandlerRun(t *testing.T, cfg event.TargetWeightConfigurationPayload, awaiting bool) *handlerRun {
	t.Helper()
	asOf := time.Date(2025, time.January, 2, 21, 0, 0, 0, time.UTC)
	h, err := targetweight.New("momentum/test", cfg, targetweight.Options{DecideFrom: asOf})
	if err != nil {
		t.Fatal(err)
	}
	if awaiting {
		h.AwaitApproval()
	}
	r := &handlerRun{t: t, recorder: journal.NewRecorder(h), hash: h.ConfigurationHash(), errs: map[string]error{}}
	r.apply("configuration:x", event.TargetWeightConfigurationEventType, cfg.SchemaVersion(), r.hash, asOf, cfg)
	if err := r.errs["configuration:x"]; err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *handlerRun) apply(id, eventType string, version uint32, hash string, at time.Time, p any) {
	r.t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		r.t.Fatal(err)
	}
	r.sequence++
	_, r.errs[id] = r.recorder.Apply(context.Background(), event.Envelope{ID: id, Type: eventType, SchemaVersion: version, EnvelopeVersion: event.CurrentEnvelopeVersion,
		EventTime: at, RecordedAt: at, Sequence: r.sequence, Source: "test", StrategyVersion: "momentum/test", ConfigurationHash: hash,
		PayloadHash: event.HashPayload(raw), Payload: raw})
}

// TestRecoveryReadsTheApprovalTheRunAccepted: settled through the real
// handler, not by position. An approval the handler refuses (here, one for
// another configuration) is committed to the log but leaves the run
// awaiting an approval, so a corrected one at the next input is accepted;
// recovery must read the rebalance that accepted approval carries, and a
// refused approval after it changes nothing (ADR 0026, point 5).
func TestRecoveryReadsTheApprovalTheRunAccepted(t *testing.T) {
	run, _ := newHandlerRun(t)
	carried := december()
	run.approve("approval:other-configuration", "sha256:other", nil)
	run.approve("approval:accepted", run.hash, carried)
	run.approve("approval:again", run.hash, nil)
	if run.errs["approval:other-configuration"] == nil || run.errs["approval:accepted"] != nil || run.errs["approval:again"] == nil {
		t.Fatalf("setup: the handler's verdicts were %v; want the first refused, the second accepted, the third refused", run.errs)
	}
	got, err := restart.RebalanceAtEnd(run.recorder.Entries(), "run-1.log")
	if err != nil || describeRebalance(got) != describeRebalance(carried) {
		t.Fatalf("recovery read %s, %v; want the accepted approval's %s", describeRebalance(got), err, describeRebalance(carried))
	}
}

// TestRecoveryReadsRecordsNotTheRunningBuildsRules: recovery reads what the
// log records, so a log written under other rules (here, a configuration
// input this build's handler would refuse, at a schema version it does not
// read, and another rules version) still reads back the rebalance its run
// recorded (ADR 0027: a run is never recomputed under another build).
func TestRecoveryReadsRecordsNotTheRunningBuildsRules(t *testing.T) {
	run, _ := newHandlerRun(t)
	run.approve("approval", run.hash, december())
	entries := run.recorder.Entries()
	entries[0].Envelope.SchemaVersion = 99
	entries[0].Envelope.StrategyVersion = "momentum/0.9.0+old"
	entries[0].Envelope.Payload = json.RawMessage(`{"strategy_id":"momentum","account_id":"` + iraLogAccount + `","a_field_this_build_does_not_know":1}`)
	got, err := restart.RebalanceAtEnd(entries, "run-1.log")
	if err != nil || describeRebalance(got) != describeRebalance(december()) {
		t.Fatalf("a log of other rules: %s, %v, want %s as recorded", describeRebalance(got), err, describeRebalance(december()))
	}
}

// TestAnAcceptedApprovalAfterARefusedInputIsRead: a fresh run awaiting its
// approval refuses an input before it, which the log commits, and then
// accepts the approval; recovery reads the acceptance the run recorded.
func TestAnAcceptedApprovalAfterARefusedInputIsRead(t *testing.T) {
	run, _ := newHandlerRunAwaiting(t)
	run.apply("calendar:early", event.SessionCalendarEventType, event.SessionCalendarSchemaVersion, run.hash, time.Date(2025, time.January, 2, 8, 0, 0, 0, time.UTC),
		event.SessionCalendarPayload{Source: "fixture", Sessions: []string{"2025-01-02", "2025-01-03"}})
	run.approve("approval", run.hash, december())
	if run.errs["calendar:early"] == nil || run.errs["approval"] != nil {
		t.Fatalf("setup: the handler's verdicts were %v", run.errs)
	}
	got, err := restart.RebalanceAtEnd(run.recorder.Entries(), "run-1.log")
	if err != nil || describeRebalance(got) != describeRebalance(december()) {
		t.Fatalf("recovery read %s, %v; want %s", describeRebalance(got), err, describeRebalance(december()))
	}
}

// TestAnAcceptanceThatCannotBeReadBackIsRefused: a recorded acceptance that
// does not decode, is at a version this build does not read, or names
// another account than the log's refuses, never reads as no rebalance.
func TestAnAcceptanceThatCannotBeReadBackIsRefused(t *testing.T) {
	for name, mutate := range map[string]func(e *event.Envelope){
		"undecodable":     func(e *event.Envelope) { e.Payload = json.RawMessage(`{"account_id":"DUIRA"}`) },
		"unknown version": func(e *event.Envelope) { e.SchemaVersion = 2 },
		"another account": func(e *event.Envelope) {
			e.Payload = json.RawMessage(`{"account_id":"DUOTHER","approval_id":"a","approval_sequence":2,"rebalance_in_progress":null}`)
		},
	} {
		run, _ := newHandlerRun(t)
		run.approve("approval", run.hash, december())
		entries := run.recorder.Entries()
		for i := range entries {
			if entries[i].Envelope.Type == event.RunApprovalAcceptedEventType {
				mutate(&entries[i].Envelope)
			}
		}
		if _, err := restart.RebalanceAtEnd(entries, "run-1.log"); err == nil || !strings.Contains(err.Error(), "cannot be read back") {
			t.Errorf("%s: %v, want a refusal saying it cannot be read back", name, err)
		}
	}
}
