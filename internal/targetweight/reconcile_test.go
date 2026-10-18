package targetweight_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// The reconciliation seam at the handler: a recorded account.reconciliation
// envelope in, the risk.reconciliation.result and any strategy.engine.state
// envelopes out (ADR 0019, amended 2026-09-24).

// observation is IBKR's morning view of account: SPY held, and cash.
func observation(account string, spy int64, cash float64) event.ReconciliationPayload {
	p := event.ReconciliationPayload{
		AccountID: account, ReconciliationID: "morning:1", Trigger: event.ReconciliationTriggerMorning,
		AsOf: sessionEnd(1).Add(-8 * time.Hour), Unreadable: []string{}, Positions: []event.AccountPosition{}, TotalCash: cash, Currency: "USD",
	}
	if spy != 0 {
		p.Positions = []event.AccountPosition{{InstrumentID: "SPY", Quantity: spy}}
	}
	return p
}

// bookedBoost is a configured Boost handler whose book is session 0's
// account snapshot: 147 SPY.
func bookedBoost(t *testing.T) (*targetweight.Handler, *stream) {
	t.Helper()
	h, s := configured(t)
	mustApply(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "snapshot:0", sessionEnd(0),
		event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: sessionEnd(0), NetLiquidation: 8530, Currency: "USD",
			Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: 147}}}))
	return h, s
}

func reconciliationInput(t *testing.T, s *stream, id string, p event.ReconciliationPayload) event.Envelope {
	return s.envelope(t, event.ReconciliationEventType, event.ReconciliationSchemaVersion, id, p.AsOf, p)
}

func TestAMatchingReconciliationRecordsItsResultAndNoState(t *testing.T) {
	h, s := bookedBoost(t)
	out := mustApply(t, h, reconciliationInput(t, s, "reconciliation:1", observation("joint", 147, -4700)))
	if len(out) != 1 || out[0].Type != event.ReconciliationResultEventType || out[0].ID != "reconciliation-result:joint:reconciliation:1" {
		t.Fatalf("want the result alone, got %+v", out)
	}
	var r event.ReconciliationResultPayload
	decode(t, out[0], &r)
	if r.Classification != event.ReconciliationMatched || r.AnchorID != "snapshot:0" || r.CashCompared {
		t.Errorf("result %+v", r)
	}
}

func TestAMismatchDegradesTheAccountOnItsInstrument(t *testing.T) {
	h, s := bookedBoost(t)
	out := mustApply(t, h, reconciliationInput(t, s, "reconciliation:1", observation("joint", 140, -4700)))
	if len(out) != 2 || out[1].Type != event.EngineStateEventType || out[1].SchemaVersion != event.EngineStateScopedVersion {
		t.Fatalf("want the result and the engine state, got %+v", out)
	}
	var state event.EngineStatePayload
	decode(t, out[1], &state)
	if state.State != event.EngineStateDegraded || state.Reason != event.EngineStateReasonReconciliationUnexplained || !slices.Equal(state.ScopeInstrumentIDs, []string{"SPY"}) {
		t.Errorf("state %+v", state)
	}
	if out[1].ID != "engine-state:joint:reconciliation:1" {
		t.Errorf("the record is named for its input: %s", out[1].ID)
	}
	if again := mustApply(t, h, reconciliationInput(t, s, "reconciliation:2", observation("joint", 140, -4700))); len(again) != 1 {
		t.Errorf("the same difference again records its result only: %+v", again)
	}
}

func TestAMomentumCashDifferenceDegradesCashAlone(t *testing.T) {
	h, s := momentumHandler(t)
	at := closeOf("2025-01-02")
	mustApply(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotCashVersion, "snapshot:0", at, event.AccountCashSnapshotPayload{
		AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{AccountID: iraAccount, AsOf: at, NetLiquidation: 30000, Currency: "USD",
			Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: 10}}},
		SettledCash: 1000, TotalCash: 1000,
	}))
	dividend := event.CorporateActionPayload{InstrumentID: "SPY", Kind: event.CorporateActionKindDividend, EffectiveAt: at.Add(time.Hour), CashAmount: 17.25, Currency: "USD"}
	mustApply(t, h, s.envelope(t, event.MarketCorporateActionEventType, event.MarketCorporateActionSchemaVersion, "dividend:1", at.Add(time.Hour), dividend))
	obs := observation(iraAccount, 10, 1017.25)
	obs.AsOf = at.Add(12 * time.Hour)
	if out := mustApply(t, h, reconciliationInput(t, s, "reconciliation:1", obs)); len(out) != 1 {
		t.Fatalf("the dividend explains the cash: %+v", out)
	}
	obs.TotalCash, obs.ReconciliationID = 1017.24, "morning:2"
	out := mustApply(t, h, reconciliationInput(t, s, "reconciliation:2", obs))
	if len(out) != 2 {
		t.Fatalf("one cent short degrades: %+v", out)
	}
	var state event.EngineStatePayload
	decode(t, out[1], &state)
	if len(state.ScopeInstrumentIDs) != 0 || !slices.Equal(state.ScopeCashComponents, []string{event.ReconciliationComponentCash}) {
		t.Errorf("cash alone: %+v", state)
	}
}

// TestAReconciliationReplaysByteForByte: the same inputs through a fresh
// handler give the same records (ADR 0019: replay reproduces the result and
// the state change byte for byte).
func TestAReconciliationReplaysByteForByte(t *testing.T) {
	run := func() []event.Envelope {
		h, s := bookedBoost(t)
		return mustApply(t, h, reconciliationInput(t, s, "reconciliation:1", observation("joint", 140, -4700)))
	}
	if first, second := run(), run(); !reflect.DeepEqual(first, second) {
		t.Fatalf("replay differs:\n%+v\n%+v", first, second)
	}
}

func TestReconciliationInputsAreChecked(t *testing.T) {
	h, s := bookedBoost(t)
	mustRefuse(t, h, s.envelope(t, event.ReconciliationEventType, 9, "r", sessionEnd(0), observation("joint", 147, 0)), "schema version")
	mustRefuse(t, h, reconciliationInput(t, s, "r", observation("ira", 147, 0)), "account")
	mustRefuse(t, h, s.envelope(t, event.MarketCorporateActionEventType, 9, "ca", sessionEnd(0), event.CorporateActionPayload{}), "corporate")
	mustRefuse(t, h, s.envelope(t, event.MarketCorporateActionEventType, event.MarketCorporateActionSchemaVersion, "ca", sessionEnd(0), event.CorporateActionPayload{InstrumentID: "SPY"}), "corporate")
	delisting := event.CorporateActionPayload{InstrumentID: "SPY", Kind: event.CorporateActionKindDelisting, EffectiveAt: sessionEnd(0)}
	mustRefuse(t, h, s.envelope(t, event.MarketCorporateActionEventType, event.MarketCorporateActionSchemaVersion, "", sessionEnd(0), delisting), "envelope ID")
}

func TestAnAcknowledgementIsARecordedInput(t *testing.T) {
	ack := event.AlertAcknowledgedPayload{AccountID: "joint", AlertKey: "k", AcknowledgedBy: "owner", AcknowledgedAt: sessionEnd(1)}
	h, s := configured(t)
	if out := mustApply(t, h, s.envelope(t, event.AlertAcknowledgedEventType, event.AlertAcknowledgedSchemaVersion, "ack:1", sessionEnd(1), ack)); out != nil {
		t.Errorf("an acknowledgement decides nothing: %+v", out)
	}
	ack.AccountID = "ira"
	mustRefuse(t, h, s.envelope(t, event.AlertAcknowledgedEventType, event.AlertAcknowledgedSchemaVersion, "ack:2", sessionEnd(1), ack), "account")
	mustRefuse(t, h, s.envelope(t, event.AlertAcknowledgedEventType, event.AlertAcknowledgedSchemaVersion, "ack:3", sessionEnd(1), event.AlertAcknowledgedPayload{}), "invalid")

	m, ms := momentumHandler(t)
	ack.AccountID = iraAccount
	if out := mustApply(t, m, ms.envelope(t, event.AlertAcknowledgedEventType, event.AlertAcknowledgedSchemaVersion, "ack:1", sessionEnd(1), ack)); out != nil {
		t.Errorf("a momentum run reads an acknowledgement too: %+v", out)
	}
}
