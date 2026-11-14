package targetweight_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// ADR 0026, Budget: total-cash decisions do not depend on settled cash.
func TestUnavailableSettledCashPreservesTotalCashDecisions(t *testing.T) {
	opts := dryRun(event.TradingModeDryRun)
	opts.mutate = func(c *event.TargetWeightConfigurationPayload) {
		c.Mode = event.TradingModeDryRun
		c.Execution.ShareQuantity = event.ShareQuantityFractional
		c.Execution.CashRule = event.CashRuleTotal
	}
	original := simulateMomentum(t, opts)
	if len(released(t, original.decisions)) == 0 {
		t.Fatal("no buys exercised")
	}
	cfg := momentumConfiguration()
	opts.mutate(&cfg)
	for _, version := range []uint32{6, 7} {
		h, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: closeOf("2025-01-02")})
		if err != nil {
			t.Fatal(err)
		}
		var decisions []event.Envelope
		for _, in := range original.inputs {
			if in.Type == event.AccountSnapshotEventType {
				var p event.AccountCashSnapshotPayload
				decode(t, in, &p)
				p.QuantityScale = 10000
				for i := range p.Positions {
					p.Positions[i].Quantity *= 10000
				}
				if version == 7 {
					p.SettledCash = nil
				}
				in.SchemaVersion = version
				in.Payload, err = json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				in.PayloadHash = event.HashPayload(in.Payload)
			}
			out, err := h.Apply(context.Background(), in)
			if err != nil {
				t.Fatalf("v%d %s: %v", version, in.ID, err)
			}
			decisions = append(decisions, out...)
		}
		if !reflect.DeepEqual(decisions, original.decisions) {
			t.Fatalf("v%d changed total-cash decisions", version)
		}
	}
}

func TestBoostRefusesUnavailableSettledCashSnapshot(t *testing.T) {
	h, s := configured(t)
	mustRefuse(t, h, s.envelope(t, event.AccountSnapshotEventType, 7, "snapshot", sessionEnd(0), json.RawMessage(`{"quantity_scale":10000,"positions":[],"total_cash":1000}`)), "schema version")
}
