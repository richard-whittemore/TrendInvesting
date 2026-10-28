package restart_test

import (
	"context"
	"encoding/json"
	"os"
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
	return newHandlerRunIn(t, event.TradingModePaper)
}

// newHandlerRunIn is newHandlerRun for a run in mode.
func newHandlerRunIn(t *testing.T, mode string) (*handlerRun, event.TargetWeightConfigurationPayload) {
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
	asOf := time.Date(2025, time.January, 2, 21, 0, 0, 0, time.UTC)
	h, err := targetweight.New("momentum/test", cfg, targetweight.Options{DecideFrom: asOf})
	if err != nil {
		t.Fatal(err)
	}
	r := &handlerRun{t: t, recorder: journal.NewRecorder(h), hash: h.ConfigurationHash(), errs: map[string]error{}}
	r.apply("configuration:x", event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, r.hash, asOf, cfg)
	return r, cfg
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
