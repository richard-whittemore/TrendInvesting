package fills_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
)

// TestObserveTerminalInputsLeavesOrdersUnchanged pins the input/emission
// boundary: ADR 0009 delistings and ADR 0011 completion cancel orders through
// reducer emissions, never by interpreting the input in the order book.
func TestObserveTerminalInputsLeavesOrdersUnchanged(t *testing.T) {
	t.Parallel()
	for _, input := range []event.Envelope{
		delistingEnvelope(t, day(56)),
		envelope(t, "completed", event.RunCompletedEventType, event.RunCompletedSchemaVersion, day(56), event.RunCompletedPayload{}),
	} {
		t.Run(input.Type, func(t *testing.T) {
			sim := observeRun(t, append(warmUpBars(), breakoutBar()))
			other := restingEntryProposal(t, 157)
			var proposal event.TradeProposalPayload
			decodeInto(t, other, &proposal)
			proposal.InstrumentID = "OTHER"
			if err := sim.Observe(envelope(t, "other-entry", other.Type, other.SchemaVersion, day(56), proposal)); err != nil {
				t.Fatal(err)
			}
			before, unrelated := sim.Resting(testInstrument), sim.Resting("OTHER")
			if len(before) == 0 || len(unrelated) == 0 {
				t.Fatal("fixture must hold both a Protective Stop and an unrelated entry")
			}
			if err := sim.Observe(input); err != nil {
				t.Fatalf("Observe(%s): %v", input.Type, err)
			}
			if !reflect.DeepEqual(before, sim.Resting(testInstrument)) || !reflect.DeepEqual(unrelated, sim.Resting("OTHER")) {
				t.Fatal("an input changed resting orders before the reducer's emissions")
			}
		})
	}
}

// TestDelistingEmissionsRemoveEveryRestingOrder exercises ADR 0009's
// cancellation of each proposal kind and the Campaign's Protective Stops.
// Bars are applied without automatic fills so real reducer proposals remain
// outstanding when the action arrives; no decision payload is fabricated.
func TestDelistingEmissionsRemoveEveryRestingOrder(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{event.ProposalKindEntry, event.ProposalKindAdd, event.ProposalKindExit, "protective-stop"} {
		t.Run(kind, func(t *testing.T) {
			cfg := baselineConfig()
			sim, reducer := newComposed(t, cfg)
			ctx := context.Background()
			var sequence uint64
			apply := func(input event.Envelope) []event.Envelope {
				t.Helper()
				sequence++
				input.Sequence = sequence
				decisions, err := reducer.Apply(ctx, input)
				if err != nil {
					t.Fatalf("Apply(%s): %v", input.ID, err)
				}
				for _, decision := range decisions {
					if err := sim.Observe(decision); err != nil {
						t.Fatalf("Observe(%s): %v", decision.Type, err)
					}
				}
				return decisions
			}
			applyBar := func(b event.CompletedBarPayload) []event.Envelope {
				t.Helper()
				decisions := apply(barEnvelope(t, b))
				return append(decisions, apply(envelope(t, "session:"+b.PeriodEnd.Format(time.RFC3339),
					event.SessionClosedEventType, event.SessionClosedSchemaVersion, b.PeriodEnd,
					event.SessionClosedPayload{PeriodEnd: b.PeriodEnd, InstrumentIDs: []string{testInstrument}}))...)
			}
			apply(configurationEnvelope(t, cfg))
			apply(accountSnapshotEnvelope(t, cfg))
			for _, b := range warmUpBars() {
				applyBar(b)
			}
			proposed := onlyOfType(t, applyBar(breakoutBar()), event.TradeProposalEventType)
			at := day(56)
			pending := proposed.ID
			if kind != event.ProposalKindEntry {
				var proposal event.TradeProposalPayload
				decodeInto(t, proposed, &proposal)
				apply(envelope(t, "entry-fill", event.FillEventType, event.FillSchemaVersion, at, event.FillPayload{
					InstrumentID: testInstrument, Kind: event.FillKindEntry, ProposalID: proposed.ID,
					FillID: "entry-fill", Direction: event.DirectionLong, Quantity: proposal.Quantity,
					Price: 155.575, FilledAt: at, Level: proposal.EntryLevel, SlippageApplied: fixtureSlippage,
				}))
				pending = ""
			}
			switch kind {
			case event.ProposalKindAdd:
				at = day(57)
				pending = onlyOfType(t, applyBar(bar(at, 156, 157, 155.5, 156.5)), event.AddProposalEventType).ID
			case event.ProposalKindExit:
				// Lift the Exit Channel above the stop before breaching it
				// (ADR 0005: one Exit Order per Unit, at the higher level).
				for i := 57; i <= 76; i++ {
					applyBar(bar(day(i), 155.8, 156, 155.5, 155.8))
				}
				at = day(77)
				pending = onlyOfType(t, applyBar(bar(at, 155.8, 156, 155, 155.2)), event.ExitProposalEventType).ID
			}
			resting := sim.Resting(testInstrument)
			wantOrders := 1
			if kind == event.ProposalKindAdd {
				wantOrders = 2
			}
			if len(resting) != wantOrders {
				t.Fatalf("before delisting: %v, want %d orders", resting, wantOrders)
			}
			if pending != "" && resting[0].ProposalID != pending {
				t.Fatalf("resting order does not name the outstanding %s proposal: %v", kind, resting)
			}
			result, err := fills.Deliver(ctx, sim, reducer, delistingEnvelope(t, at))
			if err != nil {
				t.Fatal(err)
			}
			if pending != "" {
				var expired event.ProposalExpiredPayload
				decodeInto(t, onlyOfType(t, result.Decisions, event.ProposalExpiredEventType), &expired)
				if expired.ProposalID != pending || expired.Kind != kind || expired.Reason != event.ExpiryReasonSupersededByDelisting {
					t.Fatalf("wrong delisting cancellation: %+v", expired)
				}
			}
			if kind != event.ProposalKindEntry {
				var exited event.CampaignExitedPayload
				decodeInto(t, onlyOfType(t, result.Decisions, event.CampaignExitedEventType), &exited)
				if exited.Reason != event.ExitReasonDelisting {
					t.Fatalf("exit reason = %s", exited.Reason)
				}
			} else if len(envelopesOfType(result.Decisions, event.CampaignExitedEventType)) != 0 {
				t.Fatal("an unfilled entry must not exit a Campaign")
			}
			if got := sim.Resting(testInstrument); len(got) != 0 {
				t.Fatalf("after delisting: %v, want no resting order", got)
			}
			later, err := fills.RunBar(ctx, sim, reducer, barEnvelope(t, bar(at.AddDate(0, 0, 1), 160, 170, 100, 160)))
			if err != nil || len(envelopesOfType(later.Inputs, event.FillEventType)) != 0 {
				t.Fatalf("bar after delisting produced fills or failed: %+v, %v", later, err)
			}
		})
	}
}

func delistingEnvelope(t *testing.T, at time.Time) event.Envelope {
	t.Helper()
	return envelope(t, "delisting", event.MarketCorporateActionEventType, event.MarketCorporateActionSchemaVersion, at,
		event.CorporateActionPayload{InstrumentID: testInstrument, Kind: event.CorporateActionKindDelisting, EffectiveAt: at})
}
