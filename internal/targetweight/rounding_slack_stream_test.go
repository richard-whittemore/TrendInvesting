package targetweight_test

import (
	"encoding/json"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
)

func TestRoundingSlackFlowsFromConfigurationToDecisions(t *testing.T) {
	opts := dryRun(event.TradingModeDryRun)
	opts.mutate = func(c *event.TargetWeightConfigurationPayload) {
		c.Mode = event.TradingModeDryRun
		c.Execution.ShareQuantity = event.ShareQuantityFractional
		if err := json.Unmarshal([]byte(`{"rounding_slack_per_order":0.37}`), &c.Execution); err != nil {
			t.Fatal(err)
		}
	}
	r := simulateMomentum(t, opts)
	plans := map[string]event.OrderPlannedPayload{}
	for _, p := range planned(t, r.decisions) {
		plans[p.ClientOrderID] = p
	}
	buys := 0
	for _, p := range released(t, r.decisions) {
		if p.Side != event.OrderSideBuy {
			continue
		}
		buys++
		for _, payload := range []any{plans[p.ClientOrderID], p.Figures} {
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["rounding_slack"]) != "0.37" {
				t.Fatalf("configured slack missing from decision: %s", raw)
			}
		}
		if p.Figures.BuyCost != orderplan.BuyCost(plans[p.ClientOrderID], 1) || p.Figures.BuyCost > p.Figures.CashAvailable {
			t.Fatalf("planner/gate disagree: %+v", p)
		}
	}
	if buys == 0 {
		t.Fatal("no fractional buys exercised")
	}
}
