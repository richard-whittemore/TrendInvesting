package risklimit_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
)

// TestRestoreStateResumesTheRecordedStateAndScope reads back recorded
// engine states as a restart does (ADR 0019, amended 2026-09-24, "The state
// survives a restart"): Degraded with its scope, then Halted, which a later
// Degraded record cannot leave.
func TestRestoreStateResumesTheRecordedStateAndScope(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	degraded := event.EngineStatePayload{State: event.EngineStateDegraded, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "d", ScopeInstrumentIDs: []string{"SPY"}}
	if err := g.RestoreState("engine-state:1", degraded, event.EngineStateScopedVersion); err != nil {
		t.Fatal(err)
	}
	if g.State() != risklimit.StateDegraded || !slices.Equal(g.Scope(), []string{"SPY"}) {
		t.Fatalf("restored %s %v", g.State(), g.Scope())
	}
	out, err := g.Check(bookOf(order(event.OrderSideBuy, 1, 0)), []risklimit.Order{order(event.OrderSideBuy, 1, 0)})
	if err != nil || out[0].Refused == nil || out[0].Refused.Reason != event.RefusedEngineDegraded {
		t.Fatalf("a buy after a restored Degraded: %+v %v", out, err)
	}
	halted := event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "h"}
	if err := g.RestoreState("engine-state:2", halted, event.EngineStateSchemaVersion); err != nil {
		t.Fatal(err)
	}
	if g.State() != risklimit.StateHalted || len(g.Scope()) != 0 {
		t.Fatalf("restored %s %v", g.State(), g.Scope())
	}
	for _, c := range []struct {
		name    string
		id      string
		p       event.EngineStatePayload
		version uint32
		want    string
	}{
		{"degraded after halted", "engine-state:3", degraded, event.EngineStateScopedVersion, "follows Halted"},
		{"no envelope id", "", halted, event.EngineStateSchemaVersion, "envelope ID"},
		{"degraded at version 1", "engine-state:4", degraded, event.EngineStateSchemaVersion, "schema version 1"},
	} {
		if err := g.RestoreState(c.id, c.p, c.version); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}
