package targetweight_test

import (
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// killSwitchAt is an operator.kill-switch input for the IRA at session's
// close, engaged or released.
func killSwitchAt(t *testing.T, s *stream, id, session string, engaged bool) event.Envelope {
	t.Helper()
	return s.envelope(t, event.KillSwitchEventType, event.KillSwitchSchemaVersion, id, closeOf(session),
		event.KillSwitchPayload{AccountID: iraAccount, AsOf: closeOf(session), Engaged: engaged, Operator: "owner", Detail: "test"})
}

// TestAMomentumRunReadsTheKillSwitch: the kill switch refuses every momentum
// order and changes no engine state, and an IRA gets no sale back to 98%
// (ADR 0026, points 3 and 4; plan.md section 5, "Kill switch"). Each refusal
// is journalled at the version that holds its reason. Released, the switch
// lets the decision's orders go again.
func TestAMomentumRunReadsTheKillSwitch(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{stopAfter: "2025-01-31", beforeClose: func(s *stream, session string) []event.Envelope {
		if session != "2025-01-31" {
			return nil
		}
		return []event.Envelope{killSwitchAt(t, s, "kill:1", session, true)}
	}})
	refused := ofType(r.decisions, event.OrderRefusedEventType)
	if len(refused) != 2 || len(released(t, r.decisions)) != 0 {
		t.Fatalf("%d refused, %d released; want both January buys refused", len(refused), len(released(t, r.decisions)))
	}
	for _, e := range refused {
		var p event.OrderRefusedPayload
		decode(t, e, &p)
		if p.Reason != event.RefusedKillSwitch || e.SchemaVersion != event.OrderRefusedSafetyVersion {
			t.Errorf("refusal %s at version %d: %+v", e.ID, e.SchemaVersion, p)
		}
	}
	if n := len(ofType(r.decisions, event.EngineStateEventType)); n != 0 {
		t.Errorf("the kill switch changed the engine state %d times", n)
	}
	if n := len(sales(t, r.decisions)) + len(ofType(r.decisions, event.SaleWithheldEventType)); n != 0 {
		t.Errorf("an IRA got %d sales back to 98%%", n)
	}

	r = simulateMomentum(t, momentumOptions{stopAfter: "2025-01-31", beforeClose: func(s *stream, session string) []event.Envelope {
		if session != "2025-01-31" {
			return nil
		}
		return []event.Envelope{killSwitchAt(t, s, "kill:1", session, true), killSwitchAt(t, s, "kill:2", session, false)}
	}})
	if n := len(released(t, r.decisions)); n != 2 {
		t.Errorf("%d released once the switch is off, want 2", n)
	}
}

// TestAMomentumRunSizesSellsFromTheBrokerSnapshot: a broker snapshot reaches
// the IRA's gate, so a sell beyond the broker's holding is refused as a
// breach and degrades the account on that instrument, never resized (ADR
// 0019, amendment of 2026-10-17; ADR 0026, point 2.5). The figures of every order
// checked after it carry the broker snapshot, at the version that holds them,
// and an IRA's snapshot raises no margin alarm.
func TestAMomentumRunSizesSellsFromTheBrokerSnapshot(t *testing.T) {
	base := simulateMomentum(t, momentumOptions{})
	var sells []event.OrderPlannedPayload
	for _, o := range planned(t, base.decisions) {
		if o.Side == event.OrderSideSell {
			sells = append(sells, o)
		}
	}
	if len(sells) == 0 {
		t.Fatal("setup: the February rebalance sells nothing")
	}
	short := sells[0]
	var session string
	for _, e := range ofType(base.decisions, event.TargetsDecidedEventType) {
		if e.ID == short.Tag {
			var d event.TargetsDecidedPayload
			decode(t, e, &d)
			session = d.SessionPeriodEnd.Format(time.DateOnly)
		}
	}
	var account event.AccountCashSnapshotPayload
	for _, in := range base.inputs {
		if in.ID == "snapshot:"+session {
			decode(t, in, &account)
		}
	}
	if len(account.Positions) == 0 {
		t.Fatalf("setup: no account snapshot holding for %s", session)
	}
	r := simulateMomentum(t, momentumOptions{beforeClose: func(s *stream, day string) []event.Envelope {
		if day != session {
			return nil
		}
		snap := event.BrokerSnapshotPayload{AccountID: iraAccount, AsOf: closeOf(day), MarketOpen: false,
			NetLiquidation: account.NetLiquidation, ExcessLiquidity: account.NetLiquidation}
		for _, p := range account.Positions {
			q := p.Quantity
			if p.InstrumentID == short.InstrumentID {
				q = short.Quantity - 1
			}
			snap.Positions = append(snap.Positions, event.BrokerPosition{InstrumentID: p.InstrumentID, Quantity: q, Mark: 1, MarkSet: true})
		}
		return []event.Envelope{s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "broker:"+day, closeOf(day), snap)}
	}})
	if n := len(ofType(r.decisions, event.MarginAlarmEventType)); n != 0 {
		t.Errorf("an IRA's broker snapshot raised %d margin alarms", n)
	}
	var found bool
	for _, e := range ofType(r.decisions, event.OrderRefusedEventType) {
		var p event.OrderRefusedPayload
		decode(t, e, &p)
		if p.ClientOrderID != short.ClientOrderID {
			continue
		}
		found = true
		if p.Reason != event.RefusedBrokerHolding || !p.Figures.BrokerSet || p.Figures.BrokerHolding != short.Quantity-1 || e.SchemaVersion != event.OrderRefusedSafetyVersion {
			t.Errorf("refusal at version %d: %+v", e.SchemaVersion, p)
		}
	}
	if !found {
		t.Fatalf("the sell of %s beyond the broker's holding was not refused", short.InstrumentID)
	}
	states := ofType(r.decisions, event.EngineStateEventType)
	if len(states) != 1 {
		t.Fatalf("%d engine states, want one Degraded", len(states))
	}
	var state event.EngineStatePayload
	decode(t, states[0], &state)
	if state.State != event.EngineStateDegraded || len(state.ScopeInstrumentIDs) != 1 || state.ScopeInstrumentIDs[0] != short.InstrumentID ||
		states[0].ID != "engine-state:"+iraAccount+":"+short.ClientOrderID {
		t.Errorf("state %s: %+v", states[0].ID, state)
	}
	for _, e := range ofType(r.decisions, event.OrderReleasedEventType) {
		var p event.OrderReleasedPayload
		decode(t, e, &p)
		if p.Figures.BrokerSet && e.SchemaVersion != event.OrderReleasedSafetyVersion {
			t.Errorf("release %s with broker figures at version %d", e.ID, e.SchemaVersion)
		}
	}
}

func TestAMomentumRunRefusesBadSafetyInputs(t *testing.T) {
	h, s := momentumHandler(t)
	at := closeOf("2025-01-02")
	mustRefuse(t, h, s.envelope(t, event.KillSwitchEventType, event.KillSwitchSchemaVersion, "k", at,
		event.KillSwitchPayload{AccountID: "joint", AsOf: at, Engaged: true, Operator: "o", Detail: "d"}), "kill switch for account")
	mustRefuse(t, h, s.envelope(t, event.KillSwitchEventType, event.KillSwitchSchemaVersion+1, "k", at, event.KillSwitchPayload{}), "schema version")
	mustRefuse(t, h, s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "b", at,
		event.BrokerSnapshotPayload{AccountID: "joint", AsOf: at, NetLiquidation: 1, ExcessLiquidity: 1}), "broker snapshot for account")
	mustRefuse(t, h, s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion+1, "b", at, event.BrokerSnapshotPayload{}), "schema version")
}
