package strategy_test

import (
	"context"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// TestCampaignUnrepresentableResultHaltsAtTheFill preserves the fill as input
// and refuses a successful close with invented figures (ADR 0017; development
// principle 4). A failed Apply emits nothing and leaves the Campaign retryable.
func TestCampaignUnrepresentableResultHaltsAtTheFill(t *testing.T) {
	for _, kind := range []string{event.FillKindStop, event.FillKindExit} {
		for _, figure := range []string{"average move in n", "realised result in unit n"} {
			t.Run(kind+"/"+figure, func(t *testing.T) {
				const scale = 1e-200
				cfg := validConfigurationPayload()
				cfg.NotionalAccount.StartingEquity *= scale
				bars := breakoutBars("AAPL")
				scaleBar := func(b event.CompletedBarPayload) event.CompletedBarPayload {
					b.Raw.Open *= scale
					b.Raw.High *= scale
					b.Raw.Low *= scale
					b.Raw.Close *= scale
					b.SplitAdjusted.Open *= scale
					b.SplitAdjusted.High *= scale
					b.SplitAdjusted.Low *= scale
					b.SplitAdjusted.Close *= scale
					return b
				}
				for i := range bars {
					bars[i] = scaleBar(bars[i])
				}
				opening := openingFill("AAPL")
				opening.Price *= scale
				s := newStream(t, cfg).bars(bars).fill(opening)
				opened := decodeCampaignOpened(t, onlyEnvelopeOfType(t, s.mustRun(), event.CampaignOpenedEventType))
				unitIDs := []string{opening.FillID}
				quantity := opening.Quantity
				if figure == "realised result in unit n" {
					rung := opening.Price + float64(.5*opened.CampaignN)
					s.bar(scaleBar(addOpportunityBar("AAPL", day(57), 300)))
					s.fill(addFill("AAPL", opened.CampaignID, 2, day(57), "second", rung, opening.Quantity, day(57)))
					s.mustRun()
					unitIDs = append(unitIDs, "second")
					quantity *= 2
				}
				var fill event.FillPayload
				if kind == event.FillKindStop {
					fill = closingStopFill("AAPL", opened.CampaignID, opened.CampaignN, day(59))
					fill.UnitIDs = unitIDs
				} else {
					s.bar(scaleBar(postEntryBar("AAPL", day(58), 99)))
					fill = closingExitFill("AAPL", opened.CampaignID, 100, day(58), day(59))
				}
				fill.Quantity = quantity
				fill.Price = opened.CampaignN * 1e308
				if figure == "average move in n" {
					fill.Price = 1e200
				}
				r, err := strategy.NewReducer(testStrategyVersion, validConfigurationPayload())
				if err != nil {
					t.Fatal(err)
				}
				for _, input := range s.envelopes {
					if _, err := r.Apply(context.Background(), input); err != nil {
						t.Fatal(err)
					}
				}
				bad := fillEnvelope(t, s.seq+1, fill)
				out, err := r.Apply(context.Background(), bad)
				if err == nil || !strings.Contains(err.Error(), "cannot compute the "+figure) || !strings.Contains(err.Error(), "not representable") {
					t.Fatalf("Apply error = %v, want %s not representable", err, figure)
				}
				if len(out) != 0 {
					t.Fatalf("failed close emitted %d decisions", len(out))
				}
				fill.Price = opening.Price
				fill.Level = fill.Price
				out, err = r.Apply(context.Background(), fillEnvelope(t, s.seq+1, fill))
				if err != nil {
					t.Fatalf("failed close mutated Campaign: corrected retry: %v", err)
				}
				if len(envelopesOfType(out, event.CampaignExitedEventType)) != 1 {
					t.Fatal("corrected fill did not close Campaign")
				}
			})
		}
	}
}
