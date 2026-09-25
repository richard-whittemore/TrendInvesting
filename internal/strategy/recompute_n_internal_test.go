package strategy

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// TestRecomputedAddSizingGuards exercises ADR 0006's Variant through the
// transaction seam: impossible sizing fails closed and valid but unusable
// Units are declined without acquiring a hold (ADR 0010/0020).
func TestRecomputedAddSizingGuards(t *testing.T) {
	for _, tt := range []struct {
		name              string
		account, n        float64
		reason, errorText string
	}{
		{"fractional share", 1, 2, event.DeclineReasonQuantityBelowOneUnit, ""},
		{"unreachable stop", 1000000, 100, event.DeclineReasonStopIntentNotPositive, ""},
		{"overflow", math.MaxFloat64, 0.0001, "", "recompute Add size"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := transitionFixture(t, "add")
			r.recomputeNAtAdd = true
			// newConfiguredReducerForInvariantTest builds a Reducer without
			// running applyConfiguration (transition_test.go), so the fields
			// only that path sets are given fixture values here, the same
			// way hold_internal_test.go's fixtures do.
			r.configuredSizingMode, r.sizingMode = event.SizingModeVolatilityNormalised, sizing.ModeVolatilityNormalised
			r.unitVolatilityFrac = 0.005
			r.holds = nil
			s := r.instruments["AAPL"]
			s.pendingAddProposal = nil
			s.campaign.sizingAccount = tt.account
			s.lastBarN = tt.n
			s.lastBarHigh = 300
			out, err := r.transact(func(tx *transition) ([]event.Envelope, error) {
				state, _ := tx.instrument("AAPL")
				return tx.evaluateAdd(state, event.Envelope{Type: event.SessionClosedEventType})
			})
			if tt.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errorText) {
					t.Fatalf("error=%v", err)
				}
			} else {
				if err != nil || len(out) != 1 {
					t.Fatalf("out=%v err=%v", out, err)
				}
				var p event.ProposalDeclinedPayload
				if err := json.Unmarshal(out[0].Payload, &p); err != nil {
					t.Fatal(err)
				}
				if p.Reason != tt.reason {
					t.Fatalf("reason=%s want %s", p.Reason, tt.reason)
				}
			}
			if r.instruments["AAPL"].pendingAddProposal != nil || len(r.holds) != 0 {
				t.Fatal("failed Add acquired a hold or proposal")
			}
		})
	}
}
