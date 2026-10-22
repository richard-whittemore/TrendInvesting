package targetweight_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

func TestBoostRunDecidesEnterAndExit(t *testing.T) {
	r := simulate(t, boostDryRunOptions())
	decided := targets(t, r.decisions)
	if len(decided) != len(syntheticCloses()) {
		t.Fatalf("%d decisions for %d sessions", len(decided), len(syntheticCloses()))
	}
	for i, d := range decided {
		if d.Outcome != event.TargetsOutcomeDecided {
			t.Fatalf("session %d withheld: %s", i, d.Detail)
		}
		wantLatched := i >= 16 && i < 20
		if d.Boost.LatchedAfter != wantLatched {
			t.Errorf("session %d: latched %v, want %v (%+v)", i, d.Boost.LatchedAfter, wantLatched, d.Boost)
		}
	}
	first := decided[0]
	if first.Targets[0].Weight != 0.98 || first.Boost.TargetInForceSet || first.Boost.RSIReady {
		t.Errorf("first decision: %+v", first)
	}
	if decided[18].Boost.Close >= decided[18].Boost.TrendMean {
		t.Errorf("session 18 should close below the trend mean while latched: %+v", decided[18].Boost)
	}
	orders := planned(t, r.decisions)
	if len(orders) != 3 {
		t.Fatalf("want three orders (first sale to 98%%, entry, exit), got %d: %+v", len(orders), orders)
	}
	// The account holds 58; 0.98 x 4000 / 100 = 39.2, so it sells 18.
	if orders[0].Side != event.OrderSideSell || orders[0].Quantity != 18 {
		t.Errorf("first order: %+v", orders[0])
	}
	if orders[1].Side != event.OrderSideBuy || orders[2].Side != event.OrderSideSell {
		t.Errorf("entry then exit: %+v", orders[1:])
	}
	if got := len(ofType(r.decisions, event.OrderReleasedEventType)); got != 3 {
		t.Errorf("%d released, want 3", got)
	}
	for _, e := range ofType(r.decisions, event.OrderReleasedEventType) {
		var p event.OrderReleasedPayload
		decode(t, e, &p)
		if p.Send {
			t.Error("a dry-run release is never sent")
		}
	}
	// Every order is tagged with the decision envelope it serves.
	ids := map[string]bool{}
	for _, e := range ofType(r.decisions, event.TargetsDecidedEventType) {
		ids[e.ID] = true
	}
	for _, o := range orders {
		if !ids[o.Tag] {
			t.Errorf("order tag %q names no decision", o.Tag)
		}
	}
}

func TestJournalIsByteIdenticalOnRerunAndReplays(t *testing.T) {
	write := func(r run) []byte {
		header, err := r.recorder.Header(r.hash, strategyVersion)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := journal.Write(&buf, header, r.entries); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	first := write(simulate(t, options{}))
	if !bytes.Equal(first, write(simulate(t, options{}))) {
		t.Fatal("rerunning the same inputs must write a byte-identical journal")
	}
	verification, err := journal.Verify(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("verify: %v (%+v)", err, verification)
	}
	header, records, err := journal.Read(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.CheckIdentity(header, records); err != nil {
		t.Fatal(err)
	}
	inputs, decisions, err := journal.Split(records)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := targetweight.New(strategyVersion, testConfiguration(), targetweight.Options{DecideFrom: sessionEnd(0)})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := replay.New(handler)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := engine.Run(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if d := replay.Equivalent(decisions, replayed); d != nil {
		t.Fatalf("replay diverged: %+v", d)
	}
}

func TestAStaleSnapshotWithholdsButTheLatchAdvances(t *testing.T) {
	// The entry session has no snapshot: withheld, but the latch still sets,
	// so the next session's decision differs from the target in force and
	// plans the entry.
	r := simulate(t, options{noSnap: map[int]bool{16: true}})
	decided := targets(t, r.decisions)
	if d := decided[16]; d.Outcome != event.TargetsOutcomeWithheld || d.WithheldReason != event.WithheldStaleAccountSnapshot || !d.Boost.LatchedAfter {
		t.Fatalf("session 16: %+v", d)
	}
	if d := decided[17]; d.Outcome != event.TargetsOutcomeDecided || d.Boost.TargetInForce != 0.98 || d.Targets[0].Weight != 1.47 {
		t.Fatalf("session 17: %+v", d)
	}
	orders := planned(t, r.decisions)
	if len(orders) != 3 || !strings.HasSuffix(orders[1].Tag, sessionEnd(17).Format("2006-01-02T15:04:05Z")) {
		t.Fatalf("the entry is planned the session after: %+v", orders)
	}
}

func TestAMissingMarkRefusesAndDegrades(t *testing.T) {
	r := simulate(t, options{noMark: map[int]bool{16: true}})
	refused := ofType(r.decisions, event.OrderRefusedEventType)
	if len(refused) != 2 {
		t.Fatalf("want the entry refused for its mark and the exit refused while Degraded, got %d", len(refused))
	}
	var first, second event.OrderRefusedPayload
	decode(t, refused[0], &first)
	decode(t, refused[1], &second)
	if first.Reason != event.RefusedMissingMark || second.Reason != event.RefusedEngineDegraded {
		t.Errorf("reasons %s, %s", first.Reason, second.Reason)
	}
	states := ofType(r.decisions, event.EngineStateEventType)
	if len(states) != 1 || states[0].SchemaVersion != event.EngineStateScopedVersion {
		t.Fatalf("one degraded state change at schema 2, got %+v", states)
	}
	var state event.EngineStatePayload
	decode(t, states[0], &state)
	if state.State != event.EngineStateDegraded || state.Reason != event.EngineStateReasonRiskLimitBreached {
		t.Errorf("state %+v", state)
	}
}

// TestARestartWhileBoostedNeitherEntersNorExits restarts the run from the
// decision of session 17, while boosted. The latch comes from that decision,
// the indicators from the journal's own bars, and the restarted run decides
// every later session exactly as the uninterrupted one did.
func TestARestartWhileBoostedNeitherEntersNorExits(t *testing.T) {
	full := simulate(t, options{})
	all := ofType(full.decisions, event.TargetsDecidedEventType)
	recovery := all[17]
	var recovered event.TargetsDecidedPayload
	decode(t, recovery, &recovered)
	if !recovered.Boost.LatchedAfter {
		t.Fatal("session 17 should be boosted")
	}
	restarted := simulate(t, options{recovery: &recovery, snapshots: full.inputs})
	after := targets(t, restarted.decisions)
	if len(after) != len(all)-18 {
		t.Fatalf("restarted run decided %d sessions, want %d", len(after), len(all)-18)
	}
	for i, e := range ofType(restarted.decisions, event.TargetsDecidedEventType) {
		want := all[18+i]
		if e.ID != want.ID || !bytes.Equal(e.Payload, want.Payload) {
			t.Errorf("session %d differs after restart:\n got %s\nwant %s", 18+i, e.Payload, want.Payload)
		}
	}
	if after[0].Boost.LatchedBefore != true || !after[0].Boost.LatchedAfter {
		t.Errorf("session 18 after restart: %+v", after[0].Boost)
	}
	// The first order after the restart is the exit the full run also made.
	orders := planned(t, restarted.decisions)
	if len(orders) != 1 || orders[0].Side != event.OrderSideSell {
		t.Fatalf("after restart: %+v", orders)
	}
}

// TestARecoveredLatchIsNeverRecomputed recovers a boost the bars alone would
// never have latched: it stays latched until a close above the exit mean, and
// no order is planned for a target already in force.
func TestARecoveredLatchIsNeverRecomputed(t *testing.T) {
	full := simulate(t, options{})
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[5]
	var p event.TargetsDecidedPayload
	decode(t, recovery, &p)
	p.Boost.LatchedAfter = true
	p.Targets = []event.TargetWeight{{InstrumentID: "SPY", Weight: 1.47}}
	recovery = reencode(t, recovery, p)
	// Closes after session 5 fall below the exit mean for two sessions, then rise.
	closes := syntheticCloses()
	closes[6], closes[7] = 110, 109
	r := simulate(t, options{recovery: &recovery, closes: closes})
	decided := targets(t, r.decisions)
	if !decided[0].Boost.LatchedAfter || !decided[1].Boost.LatchedAfter || decided[2].Boost.LatchedAfter {
		t.Fatalf("latched %v %v %v, want true true false", decided[0].Boost.LatchedAfter, decided[1].Boost.LatchedAfter, decided[2].Boost.LatchedAfter)
	}
	orders := planned(t, r.decisions)
	if len(orders) == 0 || !strings.HasSuffix(orders[0].Tag, sessionEnd(8).Format("2006-01-02T15:04:05Z")) {
		t.Fatalf("the first order after recovery is the exit at session 8: %+v", orders)
	}
}

func reencode(t *testing.T, e event.Envelope, p event.TargetsDecidedPayload) event.Envelope {
	t.Helper()
	s := &stream{hash: e.ConfigurationHash}
	out := s.envelope(t, e.Type, e.SchemaVersion, e.ID, e.EventTime, p)
	out.Source = e.Source
	return out
}
