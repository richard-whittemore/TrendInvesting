package risklimit_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
)

func TestSetRoundingSlackPerOrderRejectsInvalidInput(t *testing.T) {
	const want = "risklimit: IRA rounding slack must be in [0.01, 1.00] dollars"
	for _, slack := range []float64{0, -0.01, 0.0099, 1.0001, math.NaN(), math.Inf(1), math.Inf(-1)} {
		g := fractionalGate(t, 0)
		if err := g.SetRoundingSlackPerOrder(slack); err == nil || err.Error() != want {
			t.Errorf("slack %v: error = %v; want %q", slack, err, want)
		}
		out := checkBook(t, g, risklimit.Book{QuantityScale: 10000, CashAvailable: 1.10}, fractionalOrder("AAA", event.OrderSideBuy, 10000))[0]
		released(t, out)
		if out.Released.Figures.RoundingSlack != event.DefaultRoundingSlack || out.Released.Figures.BuyCost != 1.10 {
			t.Fatalf("invalid setter changed the reserve: %+v", out.Released.Figures)
		}
	}
	g := gate(t, limits(), event.TradingModeDryRun)
	if err := g.SetRoundingSlackPerOrder(0.10); err == nil || err.Error() != want {
		t.Fatalf("margin gate setter error = %v; want %q", err, want)
	}
}

// ADR 0026: the configured per-order allowance is used for both cash
// admission and journalled evidence, including the inclusive dollar bounds.
func TestSetRoundingSlackPerOrderChangesCashAdmission(t *testing.T) {
	for _, slack := range []float64{0.01, 0.25, 1} {
		for _, cash := range []float64{1 + slack - 0.01, 1 + slack} {
			g := fractionalGate(t, 0)
			if err := g.SetRoundingSlackPerOrder(slack); err != nil {
				t.Fatalf("slack %v: %v", slack, err)
			}
			out := checkBook(t, g, risklimit.Book{QuantityScale: 10000, CashAvailable: cash}, fractionalOrder("AAA", event.OrderSideBuy, 10000))[0]
			var figures event.RiskFigures
			if cash < 1+slack {
				refusedFor(t, out, event.RefusedInsufficientCash)
				figures = out.Refused.Figures
			} else {
				released(t, out)
				figures = out.Released.Figures
			}
			if figures.RoundingSlack != slack || figures.BuyCost != 1+slack {
				t.Fatalf("slack %v, cash %v: figures %+v", slack, cash, figures)
			}
		}
	}
}

func TestRoundingSlackGateChecksAndJournalsDefault(t *testing.T) {
	for _, cash := range []float64{73.23, 73.3299, 73.33} {
		o := fractionalOrder("AAA", event.OrderSideBuy, 72500)
		o.Planned.LimitPrice, o.Planned.ReferencePrice, o.Mark = 10.10, 10.10, 10.10
		out := checkBook(t, fractionalGate(t, 0), risklimit.Book{QuantityScale: 10000, CashAvailable: cash}, o)[0]
		var figures event.RiskFigures
		if cash < 73.33 {
			refusedFor(t, out, event.RefusedInsufficientCash)
			figures = out.Refused.Figures
		} else {
			released(t, out)
			figures = out.Released.Figures
		}
		raw, err := json.Marshal(figures)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]float64
		// Only the two monetary fields matter; other figure fields have other types.
		var values map[string]json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			t.Fatal(err)
		}
		fields = make(map[string]float64)
		for _, key := range []string{"buy_cost", "rounding_slack"} {
			var value float64
			if err := json.Unmarshal(values[key], &value); err != nil {
				t.Fatalf("missing %s in %s", key, raw)
			}
			fields[key] = value
		}
		if fields["buy_cost"] != 73.33 || fields["rounding_slack"] != 0.10 {
			t.Fatalf("cash %v: journalled figures %s", cash, raw)
		}
	}
}
