package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// uncappedConfigurationFixture is the declared Variant "uncapped" (ADR 0005
// and ADR 0012, as amended 2026-09-24).
var uncappedConfigurationFixture = filepath.Join("testdata", "variants", uncappedVariant, "configuration.json")

// entryFills returns every entry fill a journal records, in order.
func entryFills(t *testing.T, records []journal.Record) []event.FillPayload {
	t.Helper()
	var out []event.FillPayload
	for _, record := range records {
		if record.Envelope.Type != event.FillEventType {
			continue
		}
		var fill event.FillPayload
		decodeRecord(t, record, &fill)
		if fill.Kind == event.FillKindEntry {
			out = append(out, fill)
		}
	}
	return out
}

// TestTheBaselineSkipsTheGapTheUncappedVariantFills is the head-to-head
// comparison ADR 0012 requires, on bars whose breakout bar opens at 128.50,
// above the Baseline's cap of level 127.01 + 1N (N is 1.0) = 128.01, and
// never trades back down to it. The Baseline's stop-limit does not fill that
// bar; its proposal expires, and the next bar's Signal enters at its own
// level. The uncapped Variant's stop-market order fills the gap at the open,
// as Faith's entry would [T p.18].
func TestTheBaselineSkipsTheGapTheUncappedVariantFills(t *testing.T) {
	t.Parallel()

	gapBar := time.Date(2026, time.January, 22, 0, 0, 0, 0, time.UTC)
	baseline := entryFills(t, runJournal(t, options{configPath: configurationFixture, barsPath: gapAboveCapBarsFixture}))
	uncapped := entryFills(t, runJournal(t, options{configPath: uncappedConfigurationFixture, barsPath: gapAboveCapBarsFixture}))

	if len(baseline) == 0 || !baseline[0].FilledAt.After(gapBar) {
		t.Fatalf("Baseline entries %+v: the first must fill after the gap bar %s, which opened above its cap", baseline, gapBar)
	}
	if len(uncapped) == 0 || !uncapped[0].FilledAt.Equal(gapBar) {
		t.Fatalf("uncapped entries %+v: the first must fill in the gap bar %s", uncapped, gapBar)
	}
	if price := uncapped[0].Price - uncapped[0].SlippageApplied; price != 128.5 {
		t.Errorf("uncapped entry executed at %v before slippage, want the gap bar's open 128.5", price)
	}
}

// TestTheUncappedVariantChecksAUnitAtItsLevel is the declared Variant
// reproducing the affordability check the reducer made before the cap: on
// the golden bars, every Add it cannot fund is declined for its quantity at
// its level, where the Baseline declines it for its hold at its cap, with
// slippage and commission.
func TestTheUncappedVariantChecksAUnitAtItsLevel(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name       string
		configPath string
		atLevel    bool
	}{
		{"uncapped", uncappedConfigurationFixture, true},
		{"Baseline", configurationFixture, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			records := runJournal(t, options{configPath: tt.configPath, barsPath: barsFixture})
			var quantity int64
			declines := 0
			for _, record := range records {
				switch record.Envelope.Type {
				case event.CampaignOpenedEventType:
					var opened event.CampaignOpenedPayload
					decodeRecord(t, record, &opened)
					quantity = opened.UnitQuantity
				case event.ProposalDeclinedEventType:
					var decline event.ProposalDeclinedPayload
					decodeRecord(t, record, &decline)
					if decline.Reason != event.DeclineReasonInsufficientCash {
						continue
					}
					declines++
					levelCost := declinedAddLevelCost(t, records, decline, quantity)
					if (decline.RequiredCash == levelCost) != tt.atLevel {
						t.Errorf("decline %s: RequiredCash %v, cost at the level %v; want them equal %v", record.Envelope.ID, decline.RequiredCash, levelCost, tt.atLevel)
					}
				}
			}
			if declines == 0 {
				t.Fatal("fixture error: no Add was declined for cash")
			}
		})
	}
}

// declinedAddLevelCost is the declined Add's quantity at its rung: the
// Campaign's last Unit's fill plus half its frozen N (The Turtle Rules p.19).
// Only Unit 2 is ever declined on the golden bars, so the last fill is the
// entry's.
func declinedAddLevelCost(t *testing.T, records []journal.Record, decline event.ProposalDeclinedPayload, quantity int64) float64 {
	t.Helper()
	for _, record := range records {
		if record.Envelope.Type != event.CampaignOpenedEventType {
			continue
		}
		var opened event.CampaignOpenedPayload
		decodeRecord(t, record, &opened)
		if opened.CampaignID == decline.CampaignID {
			rung := opened.EntryPrice + float64(0.5*opened.CampaignN)
			return float64(quantity) * rung
		}
	}
	t.Fatalf("decline names campaign %q, which the journal never opened", decline.CampaignID)
	return 0
}
