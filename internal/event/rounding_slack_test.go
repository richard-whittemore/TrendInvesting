package event_test

import (
	"encoding/json"
	"math"
	"math/big"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// ADR 0026: legacy whole-share orders reserve no fractional execution slack.
func TestWholeShareRoundingSlackIsZero(t *testing.T) {
	for _, shareQuantity := range []string{"", event.ShareQuantityMinimumLot} {
		cfg := event.ExecutionConfiguration{ShareQuantity: shareQuantity}
		if got := cfg.RoundingSlack(); got != 0 {
			t.Errorf("share quantity %q: slack = %v; want 0", shareQuantity, got)
		}
	}
}

// ADR 0026: ceiling the order debit precedes adding fees and the once-per-order reserve.
func TestBuyReservationIncludesCentCeilingFeesAndSlack(t *testing.T) {
	for _, tc := range []struct {
		name  string
		slack float64
		want  *big.Rat
	}{
		{"fractional reserve", 0.10, big.NewRat(7458, 100)},
		{"legacy zero reserve", 0, big.NewRat(7448, 100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 7.25 * 10.10 = 73.225, ceiling 73.23, plus 1.25 fees.
			got, err := event.BuyReservation(72500, 10000, 10.10, 1.25, tc.slack)
			if err != nil {
				t.Fatal(err)
			}
			if got.Cmp(tc.want) != 0 {
				t.Fatalf("reservation = %s; want %s", got, tc.want)
			}
		})
	}
}

// ADR 0016: omitted and explicit defaults identify the same execution rule;
// changing the rounding reserve changes the configuration identity.
func TestRoundingSlackConfiguration(t *testing.T) {
	base := validMomentumConfiguration()
	base.Execution.ShareQuantity = event.ShareQuantityFractional
	implicit := event.TargetWeightConfigurationHash(base)
	for _, value := range []string{"0.01", "0.10", "1.00"} {
		p := base
		if err := json.Unmarshal([]byte(`{"rounding_slack_per_order":`+value+`}`), &p.Execution); err != nil {
			t.Fatal(err)
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("valid slack %s: %v", value, err)
		}
		same := event.TargetWeightConfigurationHash(p) == implicit
		if same != (value == "0.10") {
			t.Errorf("slack %s: default hash equality = %v", value, same)
		}
	}
	for _, value := range []string{"0", "-0.01", "0.0099", "1.0001"} {
		p := base
		if err := json.Unmarshal([]byte(`{"rounding_slack_per_order":`+value+`}`), &p.Execution); err != nil {
			t.Fatal(err)
		}
		if p.Validate() == nil {
			t.Errorf("accepted rounding slack %s outside [0.01, 1.00]", value)
		}
	}
}

func TestRoundingSlackRejectsNonfiniteAndLegacyConfiguration(t *testing.T) {
	for _, slack := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		p := validMomentumConfiguration()
		p.Execution.ShareQuantity = event.ShareQuantityFractional
		p.Execution.RoundingSlackPerOrder = &slack
		if p.Validate() == nil {
			t.Errorf("accepted slack %v", slack)
		}
	}
	p := validMomentumConfiguration()
	slack := 0.1
	p.Execution.RoundingSlackPerOrder = &slack
	if p.Validate() == nil {
		t.Fatal("whole-share configuration accepted fractional rounding slack")
	}
}

func TestRoundingSlackDecisionValidation(t *testing.T) {
	for _, slack := range []float64{0.01, 0.1, 1} {
		p := validOrderPlanned()
		p.QuantityScale, p.RoundingSlack = 10000, slack
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, slack := range []float64{-1, 0.0099, 1.001, math.NaN(), math.Inf(1)} {
		p := validOrderPlanned()
		p.QuantityScale, p.RoundingSlack = 10000, slack
		if p.Validate() == nil {
			t.Errorf("planned order accepted slack %v", slack)
		}
	}
	p := validOrderPlanned()
	p.RoundingSlack = 0.1
	if p.Validate() == nil {
		t.Error("legacy planned order accepted slack")
	}
	released := validOrderReleased()
	released.Figures.CashChecked, released.Figures.BuyCost, released.Figures.CashAvailable = true, 100, 100
	released.Figures.RoundingSlack = 0.1
	if released.Validate() == nil {
		t.Error("legacy release accepted slack")
	}
	refused := validIRARefused()
	refused.Figures.RoundingSlack = 0.1
	if refused.Validate() == nil {
		t.Error("legacy refusal accepted slack")
	}
	released.QuantityScale, refused.QuantityScale = 10000, 10000
	if err := released.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := refused.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*event.RiskFigures){
		func(f *event.RiskFigures) { f.RoundingSlack = math.NaN() },
		func(f *event.RiskFigures) { f.RoundingSlack = 1.01 },
		func(f *event.RiskFigures) { f.CashChecked = false },
		func(f *event.RiskFigures) { f.BuyCost = 0.01 },
	} {
		p := released
		mutate(&p.Figures)
		if p.Validate() == nil {
			t.Errorf("accepted invalid figures %+v", p.Figures)
		}
	}
}
