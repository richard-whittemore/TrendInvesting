package targetweight_test

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// febDecision is the ID of the synthetic run's February decision, whose
// rebalance sells AAA and CCC and then buys BBB and IEF.
const febDecision = "targets-decided:ira:2025-02-28T21:00:00Z"

func febSell(instrument string) string {
	return orderplan.ClientOrderID(febDecision, instrument, event.OrderSideSell)
}

// clearing returns an inject hook feeding the owner's clearing of the
// February rebalance straight after the input afterID.
func clearing(afterID, action string) func(*testing.T, string, *stream) []event.Envelope {
	return func(t *testing.T, id string, s *stream) []event.Envelope {
		if id != afterID {
			return nil
		}
		at := closeOf("2025-03-03").Add(-5 * time.Hour)
		return []event.Envelope{s.envelope(t, event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, "cleared:"+action, at,
			event.RebalanceClearedPayload{AccountID: iraAccount, DecisionID: febDecision, Action: action, ClearedAt: at, Note: "checked against the broker"})}
	}
}

// orderOutcomes returns the planned, released and refused payloads and
// engine states among decisions, in order, for comparison across runs.
func orderOutcomes(es []event.Envelope) []string {
	var out []string
	for _, e := range es {
		switch e.Type {
		case event.OrderPlannedEventType, event.OrderReleasedEventType, event.OrderRefusedEventType, event.EngineStateEventType:
			out = append(out, e.Type+" "+string(e.Payload))
		}
	}
	return out
}

// afterDecision returns the order outcomes of es from February's decision on.
func afterDecision(t *testing.T, es []event.Envelope) []string {
	t.Helper()
	for i, e := range es {
		if e.ID == febDecision {
			return orderOutcomes(es[i+1:])
		}
	}
	return orderOutcomes(es)
}

// buyPhase returns the February buy phase's outcomes, which follow its four
// sell outcomes, and fails when there is none.
func buyPhase(t *testing.T, es []event.Envelope) []string {
	t.Helper()
	all := afterDecision(t, es)
	if len(all) <= 4 {
		t.Fatalf("no buy phase among %v", all)
	}
	return all[4:]
}

func recoveredRecord(t *testing.T, es []event.Envelope) event.RestartRecoveredPayload {
	t.Helper()
	found := ofType(es, event.RestartRecoveredEventType)
	if len(found) != 1 {
		t.Fatalf("%d restart records, want 1", len(found))
	}
	var p event.RestartRecoveredPayload
	decode(t, found[0], &p)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestARestartAfterTheSellsHoldsTheBuysUntilTheOwnerClears restarts the run
// after February's decision released its sells. The restart recovers the
// book, b and each sleeve's share, the released sells and the phase from the
// journal, and holds the rebalance: when the sells fill, nothing is planned.
// Once the owner clears it, the buy phase is exactly the one the run without
// a restart planned (ADR 0026, point 5; plan.md section 5).
func TestARestartAfterTheSellsHoldsTheBuysUntilTheOwnerClears(t *testing.T) {
	full := simulateMomentum(t, momentumOptions{})
	held := simulateMomentum(t, momentumOptions{restartAfter: []string{"session:2025-02-28"}})
	restarted := entryDecisions(held.entries)

	got := recoveredRecord(t, restarted)
	feb := targets(t, full.decisions)[1]
	if got.BookDecisionID != febDecision || got.LastDecisionID != febDecision || !got.QuietUntil.Equal(closeOf("2025-02-28")) {
		t.Errorf("recovered decisions: %+v", got)
	}
	if len(got.Book) != len(feb.Targets) || got.Glide != feb.Glide {
		t.Errorf("recovered book %+v glide %+v, want %+v %+v", got.Book, got.Glide, feb.Targets, feb.Glide)
	}
	for i := range got.Book {
		if got.Book[i].InstrumentID != feb.Targets[i].InstrumentID || got.Book[i].Weight != feb.Targets[i].Weight || !slices.Equal(got.Book[i].Shares, feb.Targets[i].Shares) {
			t.Errorf("book %d: %+v, want %+v", i, got.Book[i], feb.Targets[i])
		}
	}
	want := event.RecoveredRebalance{DecisionID: febDecision, Phase: event.RebalanceSellsWorking, Held: true, Working: []string{febSell("AAA"), febSell("CCC")}}
	slices.Sort(want.Working)
	if got.Rebalance.DecisionID != want.DecisionID || got.Rebalance.Phase != want.Phase || !got.Rebalance.Held || !slices.Equal(got.Rebalance.Working, want.Working) {
		t.Errorf("recovered rebalance %+v, want %+v", got.Rebalance, want)
	}
	if got.EngineState != event.EngineStateNormal {
		t.Errorf("engine state %s", got.EngineState)
	}
	if n := len(ofType(restarted, event.OrderPlannedEventType)); n != 0 {
		t.Fatalf("the held rebalance planned %d orders after its sells filled", n)
	}

	cleared := simulateMomentum(t, momentumOptions{
		restartAfter: []string{"session:2025-02-28"},
		inject:       clearing("fill:"+febSell("CCC"), event.RebalanceResume),
	})
	if !slices.Equal(afterDecision(t, entryDecisions(cleared.entries)), buyPhase(t, full.decisions)) {
		t.Errorf("buy phase after the clearing:\n%v\nwant\n%v", afterDecision(t, entryDecisions(cleared.entries)), buyPhase(t, full.decisions))
	}
}

// TestARestartAfterAPartialFillRecoversTheFillAndTheBudget restarts after
// AAA's sell filled 10 shares and before the rest is cancelled, under each
// cash rule. The fill and the proceeds it adds under total-cash are read back
// from the journal, so the buys after the clearing are the ones the
// uninterrupted run planned.
func TestARestartAfterAPartialFillRecoversTheFillAndTheBudget(t *testing.T) {
	for _, rule := range []string{event.CashRuleSettled, event.CashRuleTotal} {
		t.Run(rule, func(t *testing.T) {
			mutate := func(c *event.TargetWeightConfigurationPayload) { c.Execution.CashRule = rule }
			partial := map[string]int64{"AAA": 10}
			full := simulateMomentum(t, momentumOptions{mutate: mutate, partial: partial})
			r := simulateMomentum(t, momentumOptions{
				mutate: mutate, partial: partial,
				restartAfter: []string{"fill:" + febSell("AAA")},
				inject:       clearing("fill:"+febSell("CCC"), event.RebalanceResume),
			})
			got := recoveredRecord(t, entryDecisions(r.entries))
			if got.Rebalance.Phase != event.RebalanceSellsWorking || !slices.Equal(got.Rebalance.Working, sortedIDs(febSell("AAA"), febSell("CCC"))) {
				t.Errorf("recovered %+v", got.Rebalance)
			}
			if want := buyPhase(t, full.decisions); !slices.Equal(afterDecision(t, entryDecisions(r.entries)), want) {
				t.Errorf("after the clearing:\n%v\nwant\n%v", afterDecision(t, entryDecisions(r.entries)), want)
			}
		})
	}
}

func sortedIDs(ids ...string) []string {
	slices.Sort(ids)
	return ids
}

// TestASecondRestartStaysHeldUntilCleared restarts twice inside the same
// rebalance: the second restart reads the first's records, including its
// hold, and holds again; the clearing then plans the buys.
func TestASecondRestartStaysHeldUntilCleared(t *testing.T) {
	full := simulateMomentum(t, momentumOptions{})
	r := simulateMomentum(t, momentumOptions{
		restartAfter: []string{"session:2025-02-28", "fill:" + febSell("AAA")},
		inject:       clearing("fill:"+febSell("CCC"), event.RebalanceResume),
	})
	if len(r.earlier) != 2 {
		t.Fatalf("%d restarts", len(r.earlier))
	}
	got := recoveredRecord(t, entryDecisions(r.entries))
	if !got.Rebalance.Held || !slices.Equal(got.Rebalance.Working, []string{febSell("CCC")}) {
		t.Errorf("second restart recovered %+v", got.Rebalance)
	}
	if !slices.Equal(afterDecision(t, entryDecisions(r.entries)), buyPhase(t, full.decisions)) {
		t.Errorf("after the clearing:\n%v\nwant\n%v", afterDecision(t, entryDecisions(r.entries)), buyPhase(t, full.decisions))
	}
	// A restart after the clearing, once the buys are working, holds again:
	// it is still mid-rebalance.
	again := simulateMomentum(t, momentumOptions{
		restartAfter: []string{"session:2025-02-28", "ack:" + orderplan.ClientOrderID(febDecision, "BBB", event.OrderSideBuy)},
		inject:       clearing("fill:"+febSell("CCC"), event.RebalanceResume),
	})
	rec := recoveredRecord(t, entryDecisions(again.entries))
	if rec.Rebalance.Phase != event.RebalanceBuysWorking || !rec.Rebalance.Held {
		t.Errorf("restart with the buys working recovered %+v", rec.Rebalance)
	}
}

// TestARestartWithTheSellsFilledAndTheBuysPendingIsHeld restarts a held run
// again once its sells are terminal: the buy phase is pending, still held,
// and the clearing plans it at once, as the run without a restart did.
func TestARestartWithTheSellsFilledAndTheBuysPendingIsHeld(t *testing.T) {
	full := simulateMomentum(t, momentumOptions{})
	r := simulateMomentum(t, momentumOptions{
		restartAfter: []string{"session:2025-02-28", "fill:" + febSell("CCC")},
		inject:       clearing("bar:AAA:2025-03-03", event.RebalanceResume),
	})
	got := recoveredRecord(t, entryDecisions(r.entries))
	if got.Rebalance.Phase != event.RebalanceBuysPending || !got.Rebalance.Held || len(got.Rebalance.Working) != 0 {
		t.Errorf("recovered %+v", got.Rebalance)
	}
	if !slices.Equal(afterDecision(t, entryDecisions(r.entries)), buyPhase(t, full.decisions)) {
		t.Errorf("after the clearing:\n%v\nwant\n%v", afterDecision(t, entryDecisions(r.entries)), buyPhase(t, full.decisions))
	}
}

// TestAbandoningAHeldRebalancePlansNoBuys clears the held rebalance by
// abandoning it: the sells are tracked to their fills and nothing is bought.
func TestAbandoningAHeldRebalancePlansNoBuys(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{
		restartAfter: []string{"session:2025-02-28"},
		inject:       clearing("ack:"+febSell("AAA"), event.RebalanceAbandon),
	})
	if n := len(ofType(entryDecisions(r.entries), event.OrderPlannedEventType)); n != 0 {
		t.Errorf("an abandoned rebalance planned %d orders", n)
	}
}

// TestARestartWithNoRebalanceInProgressIsNotHeld restarts in mid-February,
// with January's rebalance complete: nothing is held, and February decides
// and trades exactly as the run without a restart.
func TestARestartWithNoRebalanceInProgressIsNotHeld(t *testing.T) {
	full := simulateMomentum(t, momentumOptions{})
	r := simulateMomentum(t, momentumOptions{restartAfter: []string{"session:2025-02-14"}})
	got := recoveredRecord(t, entryDecisions(r.entries))
	if got.Rebalance.Phase != event.RebalanceComplete || got.Rebalance.Held {
		t.Errorf("recovered %+v", got.Rebalance)
	}
	restarted := entryDecisions(r.entries)
	decided := ofType(restarted, event.TargetsDecidedEventType)
	if len(decided) != 1 || decided[0].ID != febDecision {
		t.Fatalf("restarted run decided %d times", len(decided))
	}
	if want := ofType(full.decisions, event.TargetsDecidedEventType)[1]; !bytes.Equal(decided[0].Payload, want.Payload) {
		t.Errorf("February after the restart:\n%s\nwant\n%s", decided[0].Payload, want.Payload)
	}
	if !slices.Equal(afterDecision(t, restarted), afterDecision(t, full.decisions)) {
		t.Errorf("orders after the restart differ")
	}
}

// TestARecoveredDegradedStateStillRefusesTheBuys loses AAA's mark at
// February's close, so its sell is refused and the account Degraded. The
// restart recovers the state and scope, and after the clearing the buys are
// refused exactly as without the restart.
func TestARecoveredDegradedStateStillRefusesTheBuys(t *testing.T) {
	noMark := map[string]string{"2025-02-28": "AAA"}
	full := simulateMomentum(t, momentumOptions{noMark: noMark})
	r := simulateMomentum(t, momentumOptions{
		noMark: noMark, restartAfter: []string{"session:2025-02-28"},
		inject: clearing("fill:"+febSell("CCC"), event.RebalanceResume),
	})
	got := recoveredRecord(t, entryDecisions(r.entries))
	if got.EngineState != event.EngineStateDegraded || !slices.Equal(got.Scope, []string{"AAA"}) {
		t.Errorf("recovered %s %v", got.EngineState, got.Scope)
	}
	// AAA's sell was refused, so the sell phase is two plans, a refusal, the
	// state change and CCC's release.
	want := afterDecision(t, full.decisions)
	if len(want) <= 5 || !slices.Equal(afterDecision(t, entryDecisions(r.entries)), want[5:]) {
		t.Errorf("after the clearing:\n%v\nwant\n%v", afterDecision(t, entryDecisions(r.entries)), want)
	}
	for _, refused := range ofType(entryDecisions(r.entries), event.OrderRefusedEventType) {
		if !strings.Contains(string(refused.Payload), event.RefusedEngineDegraded) {
			t.Errorf("buy refused for %s", refused.Payload)
		}
	}
}

// TestARestartedRunReplaysByteIdentically writes the restarted run's
// journal twice, then replays it on a handler built from the run options the
// journal itself records (the first session to decide).
func TestARestartedRunReplaysByteIdentically(t *testing.T) {
	opts := momentumOptions{
		restartAfter: []string{"session:2025-02-28", "fill:" + febSell("AAA")},
		inject:       clearing("fill:"+febSell("CCC"), event.RebalanceResume),
		partial:      map[string]int64{"AAA": 10},
	}
	write := func(r momentumResult) []byte {
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
	first := write(simulateMomentum(t, opts))
	if !bytes.Equal(first, write(simulateMomentum(t, opts))) {
		t.Fatal("rerunning a restarted run must write a byte-identical journal")
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
	recorded, err := targetweight.RecordedOptions(inputs)
	if err != nil {
		t.Fatal(err)
	}
	if !recorded.DecideFrom.Equal(closeOf("2025-01-02")) {
		t.Errorf("recorded first session to decide %s", recorded.DecideFrom)
	}
	handler, err := targetweight.New(strategyVersion, momentumConfiguration(), recorded)
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
