package targetweight_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

func newHandler(t *testing.T) (*targetweight.Handler, *stream) {
	t.Helper()
	h, err := targetweight.New(strategyVersion, testConfiguration(), targetweight.Options{DecideFrom: sessionEnd(0)})
	if err != nil {
		t.Fatal(err)
	}
	if h.ConfigurationHash() != event.TargetWeightConfigurationHash(testConfiguration()) {
		t.Fatal("the handler derives its hash from its configuration")
	}
	return h, &stream{hash: h.ConfigurationHash()}
}

func configured(t *testing.T) (*targetweight.Handler, *stream) {
	t.Helper()
	h, s := newHandler(t)
	mustApply(t, h, s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", sessionEnd(0), testConfiguration()))
	return h, s
}

func mustApply(t *testing.T, h *targetweight.Handler, in event.Envelope) []event.Envelope {
	t.Helper()
	out, err := h.Apply(context.Background(), in)
	if err != nil {
		t.Fatalf("apply %s: %v", in.ID, err)
	}
	return out
}

func mustRefuse(t *testing.T, h *targetweight.Handler, in event.Envelope, want string) {
	t.Helper()
	if _, err := h.Apply(context.Background(), in); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("apply %s: error %v, want one mentioning %q", in.ID, err, want)
	}
}

func TestNewRefusesBadArguments(t *testing.T) {
	cfg := testConfiguration()
	if _, err := targetweight.New("", cfg, targetweight.Options{DecideFrom: sessionEnd(0)}); err == nil {
		t.Error("missing strategy version accepted")
	}
	if _, err := targetweight.New(strategyVersion, cfg, targetweight.Options{}); err == nil {
		t.Error("missing first session accepted")
	}
	cfg.Boost.TrendDays = 0
	if _, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: sessionEnd(0)}); err == nil {
		t.Error("invalid configuration accepted")
	}
}

func TestConfigurationInputRules(t *testing.T) {
	h, s := newHandler(t)
	at := sessionEnd(0)
	mustRefuse(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarSchemaVersion, "bar", at, bar(100, at)), "before the configuration")
	other := testConfiguration()
	other.Limits.PriceBand = 0.04
	mustRefuse(t, h, s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "c", at, other), "not the configuration")
	mustRefuse(t, h, s.envelope(t, event.TargetWeightConfigurationEventType, 9, "c", at, testConfiguration()), "schema version")
	bad := s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "c", at, testConfiguration())
	bad.Payload = json.RawMessage(`{"strategy_id":1}`)
	mustRefuse(t, h, bad, "decode")
	mustRefuse(t, h, s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "c", at, event.TargetWeightConfigurationPayload{}), "invalid")
	mustApply(t, h, s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "c", at, testConfiguration()))
	mustRefuse(t, h, s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "c", at, testConfiguration()), "configured once")
	mustRefuse(t, h, s.envelope(t, "strategy.configuration", 1, "x", at, testConfiguration()), "not one this handler reads")
	if out := mustApply(t, h, s.envelope(t, event.RunCompletedEventType, event.RunCompletedSchemaVersion, "done", at, event.RunCompletedPayload{})); out != nil {
		t.Error("run completion decides nothing")
	}
}

func TestInputOrderRules(t *testing.T) {
	at, next := sessionEnd(0), sessionEnd(1)
	h, s := configured(t)
	qqq := bar(100, at)
	qqq.InstrumentID = "QQQ"
	mustRefuse(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarSchemaVersion, "b", at, qqq), "decides SPY only")
	early := bar(100, time.Date(2024, 12, 31, 21, 0, 0, 0, time.UTC))
	mustRefuse(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarSchemaVersion, "b", at, early), "warm-up")
	mustApply(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarSchemaVersion, "b", at, bar(100, at)))
	mustRefuse(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarSchemaVersion, "b2", next, bar(100, next)), "second bar")
	mustRefuse(t, h, s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "s", next, event.SessionClosedPayload{PeriodEnd: next, InstrumentIDs: []string{"SPY"}}), "not the session")
	mustRefuse(t, h, s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "s", at, event.SessionClosedPayload{PeriodEnd: at, InstrumentIDs: []string{"QQQ"}}), "disagree")
	mustRefuse(t, h, s.envelope(t, event.SessionClosedEventType, 7, "s", at, event.SessionClosedPayload{PeriodEnd: at, InstrumentIDs: []string{"SPY"}}), "schema")

	snap := (&account{cash: 4000}).snapshot(at, 100)
	wrong := snap
	wrong.AccountID = "ira"
	mustRefuse(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "a", at, wrong), "account")
	mustRefuse(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotSchemaVersion, "a", at, snap), "schema version")
	mustApply(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "a", at, snap))
	mustRefuse(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "a", at, snap), "does not follow")

	mark := event.MarkRecordedPayload{InstrumentID: "SPY", AsOf: at, Price: 100, Source: event.AdapterIBKR}
	otherMark := mark
	otherMark.InstrumentID = "QQQ"
	mustRefuse(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, "m", at, otherMark), "trades SPY only")
	mustRefuse(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, "m", at, event.MarkRecordedPayload{}), "invalid")
	mustApply(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, "m", at, mark))
	mustRefuse(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, "m", at, mark), "does not follow")

	out := mustApply(t, h, s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "s", at, event.SessionClosedPayload{PeriodEnd: at, InstrumentIDs: []string{"SPY"}}))
	if len(out) != 3 {
		t.Fatalf("first session: decision, plan and release, got %d", len(out))
	}
	mustRefuse(t, h, s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "s", at, event.SessionClosedPayload{PeriodEnd: at, InstrumentIDs: []string{"SPY"}}), "does not follow")
	mustRefuse(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarSchemaVersion, "b", at, bar(100, at)), "does not follow")
	mustRefuse(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarSchemaVersion, "b", next, event.CompletedBarPayload{}), "invalid")
	mustRefuse(t, h, s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "s", next, event.SessionClosedPayload{}), "invalid")
}

func TestRecoveryInputRules(t *testing.T) {
	full := simulate(t, options{})
	decisions := ofType(full.decisions, event.TargetsDecidedEventType)
	h, s := configured(t)
	at := sessionEnd(0)
	mustRefuse(t, h, s.envelope(t, event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, "r", at, event.TargetsDecidedPayload{}), "invalid")
	var p event.TargetsDecidedPayload
	decode(t, decisions[3], &p)
	p.AccountID = "ira"
	mustRefuse(t, h, s.envelope(t, event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, "r", at, p), "another configuration")
	mustApply(t, h, s.envelope(t, decisions[3].Type, decisions[3].SchemaVersion, decisions[3].ID, at, decisions[3].Payload))
	mustRefuse(t, h, s.envelope(t, decisions[3].Type, decisions[3].SchemaVersion, "again", at, decisions[3].Payload), "comes once")
}

func TestRecoveryFromAWithheldDecisionKeepsItsTargetInForce(t *testing.T) {
	r := simulate(t, options{noSnap: map[int]bool{16: true}})
	withheld := ofType(r.decisions, event.TargetsDecidedEventType)[16]
	restarted := simulate(t, options{recovery: &withheld, snapshots: r.inputs})
	first := targets(t, restarted.decisions)[0]
	if first.Boost.TargetInForce != 0.98 || !first.Boost.LatchedBefore || first.Targets[0].Weight != 1.47 {
		t.Fatalf("after a withheld recovery: %+v", first)
	}
	if len(planned(t, restarted.decisions)) == 0 {
		t.Fatal("the entry withheld before the restart is ordered after it")
	}
}

func TestDecisionsBeforeTheFirstSessionToDecideAreQuiet(t *testing.T) {
	cfg := testConfiguration()
	h, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: sessionEnd(3)})
	if err != nil {
		t.Fatal(err)
	}
	s := &stream{hash: h.ConfigurationHash()}
	mustApply(t, h, s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "c", sessionEnd(0), cfg))
	for i := range 5 {
		at := sessionEnd(i)
		ids := []string{"SPY"}
		if i == 1 {
			ids = []string{"QQQ"} // a gap during the warm-up still poisons the indicators
		} else {
			mustApply(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarSchemaVersion, "b", at, bar(100, at)))
		}
		out := mustApply(t, h, s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "s", at, event.SessionClosedPayload{PeriodEnd: at, InstrumentIDs: ids}))
		if i < 3 && out != nil {
			t.Fatalf("session %d precedes the first session to decide: %d emissions", i, len(out))
		}
		if i >= 3 {
			var p event.TargetsDecidedPayload
			decode(t, out[0], &p)
			if p.WithheldReason != event.WithheldMissingBars {
				t.Fatalf("session %d after a warm-up gap: %+v", i, p)
			}
		}
	}
}
