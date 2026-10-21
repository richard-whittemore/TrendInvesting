package targetweight_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/reconcile"
)

// The close-snapshot seam at the handler: an account.snapshot whose
// holdings the book and the fills journalled since it do not explain is
// journalled as a risk.reconciliation.result and degrades the account on the
// instrument, never adopted as the book silently (ADR 0019, amended
// 2026-10-21).

// boostSnapshot is the joint account's snapshot id at session n's close,
// holding spy SPY.
func boostSnapshot(t *testing.T, s *stream, id string, n int, spy int64) event.Envelope {
	t.Helper()
	return s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, id, sessionEnd(n),
		event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: sessionEnd(n), NetLiquidation: 8530, Currency: "USD",
			Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: spy}}})
}

func TestACloseSnapshotHoldingTheBookRecordsNothing(t *testing.T) {
	h, s := bookedBoost(t)
	if out := mustApply(t, h, boostSnapshot(t, s, "snapshot:1", 1, 147)); out != nil {
		t.Fatalf("the ordinary close records nothing: %+v", out)
	}
}

// TestATradeTheEngineDidNotPlaceDegradesTheAccount: three SPY bought by hand
// during session 1, which no fill explains, are in that session's close
// snapshot.
func TestATradeTheEngineDidNotPlaceDegradesTheAccount(t *testing.T) {
	h, s := bookedBoost(t)
	out := mustApply(t, h, boostSnapshot(t, s, "snapshot:1", 1, 150))
	if len(out) != 2 || out[0].Type != event.ReconciliationResultEventType || out[1].Type != event.EngineStateEventType {
		t.Fatalf("want the check's result and the engine state, got %+v", out)
	}
	if out[0].ID != "reconciliation-result:joint:snapshot:1" || !out[0].EventTime.Equal(sessionEnd(1)) {
		t.Errorf("the result is the snapshot's, timed at it: %+v", out[0])
	}
	var r event.ReconciliationResultPayload
	decode(t, out[0], &r)
	if r.Classification != event.ReconciliationUnexplained || r.ReconciliationID != reconcile.CloseSnapshotPrefix+"snapshot:1" || r.AnchorID != "snapshot:0" {
		t.Errorf("result %+v", r)
	}
	var state event.EngineStatePayload
	decode(t, out[1], &state)
	if state.State != event.EngineStateDegraded || state.Reason != event.EngineStateReasonReconciliationUnexplained || !slices.Equal(state.ScopeInstrumentIDs, []string{"SPY"}) {
		t.Errorf("state %+v", state)
	}
	if out[1].ID != "engine-state:joint:snapshot:1" {
		t.Errorf("the record is named for the snapshot: %+v", out[1])
	}
	// The snapshot is the book from here: the next close holding it matches.
	if again := mustApply(t, h, boostSnapshot(t, s, "snapshot:2", 2, 150)); again != nil {
		t.Errorf("the next close against the new book: %+v", again)
	}
}

func TestAMomentumCloseSnapshotIsCheckedOnItsHoldingsAlone(t *testing.T) {
	h, s := momentumHandler(t)
	snapshot := func(id, session string, spy int64, cash float64) event.Envelope {
		at := closeOf(session)
		return s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotCashVersion, id, at, event.AccountCashSnapshotPayload{
			AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{AccountID: iraAccount, AsOf: at, NetLiquidation: 30000, Currency: "USD",
				Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: spy}}},
			SettledCash: cash, TotalCash: cash,
		})
	}
	mustApply(t, h, snapshot("snapshot:0", "2025-01-02", 10, 1000))
	if out := mustApply(t, h, snapshot("snapshot:1", "2025-01-03", 10, 1001.17)); out != nil {
		t.Fatalf("a posting in the cash is not this check's: %+v", out)
	}
	out := mustApply(t, h, snapshot("snapshot:2", "2025-01-06", 9, 1001.17))
	if len(out) != 2 {
		t.Fatalf("one share short degrades: %+v", out)
	}
	var state event.EngineStatePayload
	decode(t, out[1], &state)
	if !slices.Equal(state.ScopeInstrumentIDs, []string{"SPY"}) || len(state.ScopeCashComponents) != 0 {
		t.Errorf("SPY alone: %+v", state)
	}
}

// TestASnapshotAfterItsReadingsReconciliationIsNotReportedTwice: the IBKR
// adapter sends a reading's reconciliation ahead of its snapshot; the
// reconciliation reports the difference, and the snapshot adds nothing.
func TestASnapshotAfterItsReadingsReconciliationIsNotReportedTwice(t *testing.T) {
	h, s := bookedBoost(t)
	obs := observation("joint", 150, -4700)
	obs.AsOf = sessionEnd(1)
	if out := mustApply(t, h, reconciliationInput(t, s, "reconciliation:1", obs)); len(out) != 2 {
		t.Fatalf("the reconciliation degrades: %+v", out)
	}
	if out := mustApply(t, h, boostSnapshot(t, s, "snapshot:1", 1, 150)); out != nil {
		t.Errorf("one difference, one report: %+v", out)
	}
}

func TestACloseSnapshotCheckReplaysByteForByte(t *testing.T) {
	run := func() []event.Envelope {
		h, s := bookedBoost(t)
		return mustApply(t, h, boostSnapshot(t, s, "snapshot:1", 1, 150))
	}
	if first, second := run(), run(); !reflect.DeepEqual(first, second) {
		t.Fatalf("replay differs:\n%+v\n%+v", first, second)
	}
}
