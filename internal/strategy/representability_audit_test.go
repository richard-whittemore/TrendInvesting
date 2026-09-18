package strategy_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// The payload guards remain necessary even for finite inputs: rounding can
// erase a stop distance or raise (CONTEXT.md: "Protective Stop", "Stop Ladder").
func TestFiniteOpeningFillCanProduceAnInvalidCampaign(t *testing.T) {
	t.Parallel()
	fill := openingFill("AAPL")
	fill.Price = 1e308
	newStream(t, validConfigurationPayload()).bars(breakoutBars("AAPL")).fill(fill).
		wantRunError("would open an invalid campaign", "must be below the entry price")
}

func TestFiniteAddFillCanProduceAnInvalidUnit(t *testing.T) {
	t.Parallel()
	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	fill := addFill("AAPL", campaignID, 2, day(57), "large-add", 1e308, 133, day(57))
	newStream(t, cfg).bars(breakoutBars("AAPL")).fill(openingFill("AAPL")).
		bar(addOpportunityBar("AAPL", day(57), 250)).fill(fill).
		wantRunError("would add an invalid unit", "must be below")
}

func TestFiniteStopRaiseCanRoundBackToItsPreviousLevel(t *testing.T) {
	t.Parallel()
	cfg := validConfigurationPayload()
	fill := openingFill("AAPL")
	fill.Price = math.Ldexp(1, 60)
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	newStream(t, cfg).
		snapshot(cashSnapshot(cfg, day(1), 1e30)).
		bars(breakoutBars("AAPL")).fill(fill).
		bar(addOpportunityBar("AAPL", day(57), fill.Price)).
		fill(addFill("AAPL", campaignID, 2, day(57), "rounded-raise", fill.Price, 133, day(57))).
		wantRunError("would raise unit 1 to an invalid protective stop", "previous level")
}

// ADR 0007 permits a new year's positive equity to rebase the account;
// finite dollar risk divided by that finite account need not remain finite.
func TestCampaignRiskFractionCanOverflowAfterARebase(t *testing.T) {
	t.Parallel()
	cfg := validConfigurationPayload()
	at := day(57).AddDate(1, 0, 0)
	newStream(t, cfg).bars(breakoutBars("AAPL")).fill(openingFill("AAPL")).
		snapshot(event.AccountSnapshotPayload{AsOf: at, Equity: 1e-308, AvailableCash: 1e9, Currency: "USD"}).
		bar(postEntryBar("AAPL", at.AddDate(0, 0, 1), 150)).
		wantRunError("built invalid campaign evaluated payload", "aggregate open risk fraction must be finite")
}

// A narrow declared variant's Stop Ladder can protect profit above the
// Campaign's average entry (CONTEXT.md: "risk-free"). This test was written
// to demonstrate that the exit validators' strict below-entry requirement
// was a REACHABLE refusal, not a theoretical one — a Variant with a narrow
// enough Stop Multiple raises a stop to or above entry and the Campaign
// then fails its own exit on a level the Stop Ladder was right to set.
//
// #134 removed that refusal: an exit records the stop that closed the
// Campaign without saying whether it was that Unit's first level or one the
// Ladder had raised, so it cannot hold it to the initial-stop shape. The
// scenario is unchanged and the assertion is inverted — the same narrow
// Variant that used to be refused now exits cleanly, through both the stop
// and exit-channel paths.
func TestProfitProtectingCampaignStopExitsCleanly(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{event.FillKindStop, event.FillKindExit} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			cfg := validConfigurationPayload()
			cfg.StopMultiple = 0.1
			n := breakoutFixtureN(t, cfg)
			campaignID := testDecisionID("campaign", "AAPL", day(56))
			addPrice := campaignFillPrice + float64(0.5*n)
			s := newStream(t, cfg).bars(breakoutBars("AAPL")).fill(openingFill("AAPL")).
				bar(addOpportunityBar("AAPL", day(57), addPrice)).
				fill(addFill("AAPL", campaignID, 2, day(57), "profit-protecting-add", addPrice, 133, day(57)))
			var fill event.FillPayload
			if kind == event.FillKindStop {
				fill = stopFillForUnits("AAPL", campaignID, "profit-protecting-stop", []string{"sim-fill-0001", "profit-protecting-add"}, 200, 266, day(58))
			} else {
				s.bar(postEntryBar("AAPL", day(58), 99))
				fill = closingExitFill("AAPL", campaignID, 100, day(58), day(58))
				fill.Quantity = 266
			}
			decisions, err := s.fill(fill).run()
			if err != nil {
				t.Fatalf("run() = %v, want nil: a Campaign whose Stop Ladder raised a stop to or above "+
					"entry is risk-free, and exiting at that stop is not a validation failure", err)
			}

			// Assert the scenario actually produced the state it is about,
			// so a future change that stops raising the stop above entry
			// cannot leave this passing for the wrong reason.
			var exited event.CampaignExitedPayload
			for _, envelope := range decisions {
				if envelope.Type != event.CampaignExitedEventType {
					continue
				}
				if err := json.Unmarshal(envelope.Payload, &exited); err != nil {
					t.Fatalf("unmarshalling the campaign-exited payload: %v", err)
				}
			}
			if exited.CampaignID == "" {
				t.Fatal("no campaign-exited decision was emitted; the scenario did not reach an exit")
			}
			if exited.ProtectiveStopLevel < exited.EntryPrice {
				t.Errorf("protective stop level %v is below the entry price %v; this test is only meaningful "+
					"while the Stop Ladder raises the stop to or above entry, which is the state #134 is about",
					exited.ProtectiveStopLevel, exited.EntryPrice)
			}
		})
	}
}

// A recorded stop fill is accepted on its own facts (ADR 0005); its finite
// price does not guarantee the Campaign's aggregate dollar result fits.
func TestFiniteStopFillCanProduceAnInvalidUnitsStoppedResult(t *testing.T) {
	t.Parallel()
	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	fill := closingStopFill("AAPL", campaignID, breakoutFixtureN(t, cfg), day(57))
	fill.Price = 1e308
	newStream(t, cfg).bars(breakoutBars("AAPL")).fill(openingFill("AAPL")).fill(fill).
		wantRunError("with an invalid units-stopped decision", "realised result must be finite")
}

// Setup's distance divides a current price difference by the preceding
// completed bars' N (CONTEXT.md: "Completed bar"), so valid bars can exceed
// the reporting range even before sizing is attempted.
func TestFiniteBarsCanProduceAnInvalidSetupDistance(t *testing.T) {
	t.Parallel()
	bars := breakoutBars("AAPL")
	for i := range bars[:len(bars)-1] {
		view := bars[i].SplitAdjusted
		bars[i] = completedBar("AAPL", bars[i].PeriodEnd, view.High*1e-300, view.Low*1e-300, view.Close*1e-300)
	}
	bars[len(bars)-1] = completedBar("AAPL", day(56), math.MaxFloat64, 1e-298, 1e-298)
	newStream(t, validConfigurationPayload()).bars(bars).
		wantRunError("built invalid setup evaluated payload", "distance to entry in n must be finite")
}

func TestCampaignAddRungOverflowPropagatesFromBothFillKinds(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{event.FillKindEntry, event.FillKindAdd} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			cfg := validConfigurationPayload()
			cfg.NotionalAccount.StartingEquity = 1000
			cfg.DollarsPerPoint = 1e-305
			s := newStream(t, cfg).bars(affineAuditBars(1e304, 0))
			fill := openingFill("AAPL")
			fill.Quantity = 1
			fill.Price = math.MaxFloat64
			if kind == event.FillKindEntry {
				s.fill(fill)
			} else {
				fill.Price = 3e306
				s.fill(fill).bar(completedBar("AAPL", day(57), 4e306, 2e306, 3e306)).
					fill(addFill("AAPL", testDecisionID("campaign", "AAPL", day(56)), 2, day(57), "overflowing-add-rung", math.MaxFloat64, 1, day(57)))
			}
			s.wantRunError("cannot compute the next add rung", "not representable")
		})
	}
}

func TestCampaignStopRaiseOverflowIsRefused(t *testing.T) {
	t.Parallel()
	cfg := validConfigurationPayload()
	cfg.NotionalAccount.StartingEquity = 1000
	cfg.DollarsPerPoint = 1e-305
	cfg.StopMultiple = 0.1
	n := breakoutFixtureN(t, validConfigurationPayload()) * 1e304
	fill := openingFill("AAPL")
	fill.Quantity = 1
	fill.Price = math.MaxFloat64 - float64(0.75*n)
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	newStream(t, cfg).bars(affineAuditBars(1e304, 0)).fill(fill).
		bar(completedBar("AAPL", day(57), math.MaxFloat64, 1e306, 2e306)).
		fill(addFill("AAPL", campaignID, 2, day(57), "lower-add-2", 1e306, 1, day(57))).
		fill(addFill("AAPL", campaignID, 3, day(57), "lower-add-3", 1.2e306, 1, day(57))).
		wantRunError("cannot raise unit 1's protective stop", "not representable")
}

func TestCampaignAggregateRiskOverflowIsRefused(t *testing.T) {
	t.Parallel()
	cfg := validConfigurationPayload()
	cfg.NotionalAccount.StartingEquity = 1.7e308
	cfg.UnitVolatilityFraction = 0.25
	cfg.StopMultiple = 4
	cfg.DollarsPerPoint = 1e306
	fill := openingFill("AAPL")
	fill.Quantity = 1
	fill.Price = 155.5
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	newStream(t, cfg).snapshot(cashSnapshot(cfg, day(1), math.MaxFloat64)).
		bars(breakoutBars("AAPL")).fill(fill).
		fill(addFill("AAPL", campaignID, 2, day(56), "large-risk-add", 175, 1, day(56))).
		bar(addOpportunityBar("AAPL", day(57), 200)).
		wantRunError("cannot compute aggregate open risk", "not representable")
}

func TestRemainingCampaignRiskOverflowIsRefusedAtAPartialStop(t *testing.T) {
	t.Parallel()
	cfg := validConfigurationPayload()
	cfg.NotionalAccount.StartingEquity = 1.7e308
	cfg.UnitVolatilityFraction = 0.02
	cfg.StopMultiple = 50
	cfg.DollarsPerPoint = 7e304
	fill := openingFill("AAPL")
	fill.Quantity = 1
	fill.Price = 2155.5
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	newStream(t, cfg).snapshot(cashSnapshot(cfg, day(1), math.MaxFloat64)).
		bars(affineAuditBars(1, 2000)).fill(fill).
		fill(addFill("AAPL", campaignID, 2, day(56), "large-risk-2", 2175, 1, day(56))).
		fill(addFill("AAPL", campaignID, 3, day(56), "large-risk-3", 2195, 1, day(56))).
		fill(stopFillForUnits("AAPL", campaignID, "partial-large-risk-stop", []string{fill.FillID}, 2100, 1, day(57))).
		wantRunError("cannot compute the remaining aggregate open risk", "not representable")
}

// Affine changes preserve the breakout fixture's price ordering; scaling
// changes its N by the same factor, and translation leaves N unchanged.
func affineAuditBars(scale, offset float64) []event.CompletedBarPayload {
	bars := breakoutBars("AAPL")
	for i := range bars {
		view := bars[i].SplitAdjusted
		bars[i] = completedBar("AAPL", bars[i].PeriodEnd, float64(view.High*scale)+offset, float64(view.Low*scale)+offset, float64(view.Close*scale)+offset)
	}
	return bars
}

func TestFiniteStopDistanceCanProduceAnInvalidTradeProposal(t *testing.T) {
	t.Parallel()
	cfg := validConfigurationPayload()
	cfg.StopMultiple = 1e-100
	newStream(t, cfg).bars(breakoutBars("AAPL")).
		wantRunError("built invalid trade proposal payload", "must be below")
}
