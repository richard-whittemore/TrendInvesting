package main

import (
	"time"

	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds the command-seam test for a review finding on
// deliverActionsDueFor (ADR 0024): a chain of symbol changes that all take
// effect before the destination's own first bar, with no instrument in the
// middle ever receiving a bar of its own.

// aapl3BarAfterEntry is the golden fixture's own 2026-01-24 bar (the day
// after outstandingExitBar), relabelled to AAPL3: real, already-valid price
// data, reused rather than invented, for the chain's destination instrument.
func aapl3BarAfterEntry(t *testing.T) event.CompletedBarPayload {
	t.Helper()
	all, err := readBars(barsFixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, bar := range all {
		if bar.PeriodEnd.Equal(outstandingExitBar.AddDate(0, 0, 1)) {
			bar.InstrumentID = "AAPL3"
			return bar
		}
	}
	t.Fatalf("the golden bar fixture holds no bar the day after %s", outstandingExitBar.Format(time.DateOnly))
	return event.CompletedBarPayload{}
}

// TestASymbolChangeChainIsFullyDeliveredBeforeTheDestinationsBar pins the
// review finding: AAPL is renamed to AAPL2, and AAPL2 is renamed to AAPL3,
// both effective in the gap after AAPL's own last bar and before AAPL3's
// first bar — AAPL2 itself never receives a bar of its own. Without
// symbolChangeChain, deliverActionsDueFor matches only the LAST hop
// (AAPL2 -> AAPL3) against AAPL3's own boundary, since only that action's
// own NewInstrumentID is "AAPL3"; the first hop (AAPL -> AAPL2) is never
// matched against AAPL3's bar at all. The reducer then receives
// "AAPL2 -> AAPL3" for an instrument it has never heard of (AAPL2 was never
// actually reached), silently records the id as retired with nothing to
// carry, and AAPL3's own bar is evaluated as a brand new instrument with no
// Campaign — the ORIGINAL Campaign's whole history silently lost, with no
// error at all. The fix delivers the full backward chain before the
// destination's bar, so the Campaign opened under AAPL is still open, under
// its own unchanged CampaignID, when AAPL3's bar is evaluated.
func TestASymbolChangeChainIsFullyDeliveredBeforeTheDestinationsBar(t *testing.T) {
	t.Parallel()

	chainAt1 := outstandingExitBar.Add(6 * time.Hour)
	chainAt2 := outstandingExitBar.Add(12 * time.Hour)

	bars := barsThroughEntry(t)
	bars = append(bars, aapl3BarAfterEntry(t))

	records := runJournal(t, options{
		configPath: configurationFixture,
		barsPath:   writeBars(t, bars),
		corporateActionsPath: writeActions(t, []event.CorporateActionPayload{
			{
				InstrumentID:    "AAPL",
				Kind:            event.CorporateActionKindSymbolChange,
				EffectiveAt:     chainAt1,
				NewInstrumentID: "AAPL2",
			},
			{
				InstrumentID:    "AAPL2",
				Kind:            event.CorporateActionKindSymbolChange,
				EffectiveAt:     chainAt2,
				NewInstrumentID: "AAPL3",
			},
		}),
	})

	var campaignID string
	var symbolChanges int
	var aapl3Evaluated bool
	var aapl3Setup bool
	for _, r := range records {
		switch r.Envelope.Type {
		case event.CampaignOpenedEventType:
			var opened event.CampaignOpenedPayload
			decodeRecord(t, r, &opened)
			if opened.InstrumentID == "AAPL" {
				campaignID = opened.CampaignID
			}
		case event.InstrumentSymbolChangedEventType:
			symbolChanges++
		case event.CampaignEvaluatedEventType:
			var evaluated event.CampaignEvaluatedPayload
			decodeRecord(t, r, &evaluated)
			if evaluated.InstrumentID == "AAPL3" {
				aapl3Evaluated = true
				if evaluated.CampaignID != campaignID {
					t.Errorf("AAPL3's campaign evaluated decision names campaign %q, want the SAME campaign %q the chain carried across", evaluated.CampaignID, campaignID)
				}
			}
		case event.SetupEvaluatedEventType:
			var setup struct {
				InstrumentID string `json:"instrument_id"`
			}
			decodeRecord(t, r, &setup)
			if setup.InstrumentID == "AAPL3" {
				aapl3Setup = true
			}
		}
	}

	if campaignID == "" {
		t.Fatal("no strategy.campaign.opened decision for AAPL: the fixture's own premise did not hold")
	}
	if symbolChanges != 2 {
		t.Fatalf("got %d symbol-changed decision(s), want 2: both hops of the chain must be delivered", symbolChanges)
	}
	if aapl3Setup {
		t.Fatal("AAPL3's bar produced a strategy.setup.evaluated decision: it was evaluated as a fresh instrument with no Campaign, meaning the chain's first hop (AAPL -> AAPL2) never reached the reducer before AAPL3's bar")
	}
	if !aapl3Evaluated {
		t.Fatal("AAPL3's bar produced no strategy.campaign.evaluated decision: the Campaign the chain should have carried across was not open under AAPL3")
	}
}
