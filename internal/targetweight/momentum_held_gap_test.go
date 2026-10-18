package targetweight_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// february returns the February decision and its record of id.
func february(t *testing.T, r momentumResult, id string) (event.TargetsDecidedPayload, event.MomentumName) {
	t.Helper()
	for _, d := range targets(t, r.decisions) {
		if d.Momentum.CloseSession != "2025-02-28" {
			continue
		}
		for _, n := range d.Momentum.Names {
			if n.InstrumentID == id {
				return d, n
			}
		}
		t.Fatalf("February records no %s: %+v", id, d.Momentum.Names)
	}
	t.Fatal("no February decision")
	return event.TargetsDecidedPayload{}, event.MomentumName{}
}

// TestAHeldStockWithOneMissingSessionIsDecidedAcrossTheGap encodes the
// owner's decision of 2026-10-18 (ADR 0026 point 5, amended): CCC, bought on
// January's decision and named to the market-data step, has no bar on
// 2025-02-20, inside February's window, in the compact stream. It is scored
// across the gap as the backtest scores it, so it stays in the book on its
// merits, and the decision names the missing session. Before, it read
// short-history and was sold whole.
func TestAHeldStockWithOneMissingSessionIsDecidedAcrossTheGap(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{noBar: map[string]string{"2025-02-20": "CCC"}, compact: true})
	feb, ccc := february(t, r, "CCC")
	if feb.Outcome != event.TargetsOutcomeDecided {
		t.Fatalf("February %s %s", feb.Outcome, feb.Detail)
	}
	if ccc.Status != event.MomentumStatusPicked || !slices.Equal(ccc.GapSessions, []string{"2025-02-20"}) {
		t.Errorf("CCC %+v, want picked across its gap on 2025-02-20", ccc)
	}
	if !slices.Contains(targetIDs(feb), "CCC") {
		t.Errorf("CCC is not targeted: %v", targetIDs(feb))
	}
	for _, o := range planned(t, r.decisions) {
		if o.InstrumentID == "CCC" && o.Side == event.OrderSideSell && o.Quantity == 115 {
			t.Errorf("the whole holding of CCC is sold: %+v", o)
		}
	}
}

// TestABarsRunDoesNotScoreAHoldingAcrossAGap: in a stream of every member's
// bars a held member has bars whether or not it was named to the
// market-data step, and that step leaves an unnamed member out of the
// ex-date session of a split or dividend it could not adjust, so the run
// cannot tell a session the stock did not trade from an unadjusted level
// jump. CCC, held, with no bar on 2025-02-20 (such an ex-date) is not scored
// across it: a bars run keeps the complete-window rule for every name, and
// CCC reads gap and is not targeted (ADR 0026 point 5, amended 2026-10-18).
func TestABarsRunDoesNotScoreAHoldingAcrossAGap(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{noBar: map[string]string{"2025-02-20": "CCC"}, unnamed: map[string]bool{"CCC": true}})
	feb, ccc := february(t, r, "CCC")
	if ccc.Status != event.MomentumStatusGap || !slices.Equal(ccc.GapSessions, []string{"2025-02-20"}) {
		t.Errorf("CCC %+v, want gap on 2025-02-20", ccc)
	}
	if slices.Contains(targetIDs(feb), "CCC") {
		t.Errorf("CCC is targeted: %v", targetIDs(feb))
	}
}

// TestACandidateNotHeldWithAGapIsStillNotRanked: the ranking rule is
// unchanged for a stock the account does not hold (ADR 0026 point 5): BBB,
// never bought, with no bar on 2025-02-20, is left out of February's
// ranking with its own status, gap, naming the session.
func TestACandidateNotHeldWithAGapIsStillNotRanked(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{noBar: map[string]string{"2025-02-20": "BBB"}})
	feb, bbb := february(t, r, "BBB")
	if bbb.Status != event.MomentumStatusGap || bbb.ScoreRank != 0 || !slices.Equal(bbb.GapSessions, []string{"2025-02-20"}) {
		t.Errorf("BBB %+v, want gap on 2025-02-20 and unranked", bbb)
	}
	if slices.Contains(targetIDs(feb), "BBB") {
		t.Errorf("BBB is targeted: %v", targetIDs(feb))
	}
}

// TestAHeldStockWithTooFewClosesIsStillShortHistory: GGG, held from the
// start but trading only from late February, has fewer closes than the
// score needs; holding it scores nothing from too short a history.
func TestAHeldStockWithTooFewClosesIsStillShortHistory(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{held: map[string]int64{"GGG": 10}})
	_, ggg := february(t, r, "GGG")
	if ggg.Status != event.MomentumStatusShortHistory || len(ggg.GapSessions) != 0 {
		t.Errorf("GGG %+v, want short-history with no gap sessions", ggg)
	}
}

// TestAHeldStockWithoutItsOwnBarsIsNotScoredAcrossItsGap: a score across a
// gap must never span a split or dividend the market-data step could not
// adjust. A stock named to that step (every holding) refuses the stream on
// one, so only its own sessions' gaps reach the run; a holding the run sees
// only through its close history is not known to be one, so its gap leaves
// it out as for a stock not held (ADR 0026 point 5, amended 2026-10-18).
func TestAHeldStockWithoutItsOwnBarsIsNotScoredAcrossItsGap(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{noBar: map[string]string{"2025-02-20": "CCC"}, compact: true, unnamed: map[string]bool{"CCC": true}})
	feb, ccc := february(t, r, "CCC")
	if ccc.Status != event.MomentumStatusGap || !slices.Equal(ccc.GapSessions, []string{"2025-02-20"}) {
		t.Errorf("CCC %+v, want gap on 2025-02-20", ccc)
	}
	if slices.Contains(targetIDs(feb), "CCC") {
		t.Errorf("CCC is targeted: %v", targetIDs(feb))
	}
}

// TestTheRunKeepsTheBacktestsHistoryOfSessions: the run keeps the last
// BacktestHistorySessions sessions of closes, the history a held stock is
// scored across its gap from (defensive_v2.py:271; ADR 0026 point 5, amended
// 2026-10-18), and drops older ones, so a decision after 300 sessions counts
// SPY's closes in the last 260.
func TestTheRunKeepsTheBacktestsHistoryOfSessions(t *testing.T) {
	cfg := momentumConfiguration()
	var cal []string
	for day := time.Date(2025, time.January, 2, 0, 0, 0, 0, time.UTC); len(cal) < 301; day = day.AddDate(0, 0, 1) {
		if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday {
			cal = append(cal, day.Format(time.DateOnly))
		}
	}
	h, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: closeOf(cal[0])})
	if err != nil {
		t.Fatal(err)
	}
	s := &stream{hash: event.TargetWeightConfigurationHash(cfg)}
	start := closeOf(cal[0]).Add(-12 * time.Hour)
	inputs := []event.Envelope{
		s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", start, cfg),
		s.envelope(t, event.SessionCalendarEventType, event.SessionCalendarSchemaVersion, "calendar", start, event.SessionCalendarPayload{Source: "fixture", Sessions: cal}),
	}
	for i, session := range cal[:300] {
		end := closeOf(session)
		// OLD trades only the first five sessions, so every close of it is
		// dropped once the window has passed them.
		ids := []string{"IEF", "SHY", "SPY"}
		if i < 5 {
			ids = []string{"IEF", "OLD", "SHY", "SPY"}
		}
		for _, id := range ids {
			inputs = append(inputs, s.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, fmt.Sprintf("bar:%s:%s", id, session), end, adjustedBar(id, float64(100+i), 1e6, end)))
		}
		inputs = append(inputs, s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "session:"+session, end, event.SessionClosedPayload{PeriodEnd: end, InstrumentIDs: ids}))
	}
	var decided []event.Envelope
	for _, in := range inputs {
		out, err := h.Apply(context.Background(), in)
		if err != nil {
			t.Fatalf("apply %s: %v", in.ID, err)
		}
		decided = append(decided, out...)
	}
	ds := targets(t, decided)
	last := ds[len(ds)-1]
	if last.WithheldReason != event.WithheldMissingUniverse || last.Momentum.TrendCloses != momentum.BacktestHistorySessions {
		t.Errorf("last decision %s %s with %d SPY closes, want missing-universe with %d", last.Momentum.CloseSession, last.WithheldReason, last.Momentum.TrendCloses, momentum.BacktestHistorySessions)
	}
}
