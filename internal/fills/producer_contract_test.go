package fills_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// This file drives the simulator from a stand-in producer rather than from
// the reducer, because that is what the package's own contract says it is:
// the resting-order book is learned entirely from a handler's emissions, and
// nothing here validates them (internal/replay owns that, on the journalled
// stream, not on the seam between a handler and this package). Every refusal
// below is therefore reachable by any producer, and is a reconciliation
// failure rather than a defect in the reducer that happens to be paired with
// it today.
//
// The fixtures deliberately emit payloads the reducer never would. That is
// the point: a guard against a producer that has gone wrong cannot be
// exercised by a producer that has not.

// emitting returns a handler that answers the FIRST envelope it is given with
// decisions, and every later one with nothing.
func emitting(decisions ...event.Envelope) replay.Handler {
	emitted := false
	return replay.HandlerFunc(func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
		if emitted {
			return nil, nil
		}
		emitted = true
		return decisions, nil
	})
}

// emittingOnBar returns a handler that answers only the completed bar with
// decisions, so an order comes to rest inside the bar it will be priced
// against in step 3 rather than before step 1.
func emittingOnBar(decisions ...event.Envelope) replay.Handler {
	return replay.HandlerFunc(func(_ context.Context, in event.Envelope) ([]event.Envelope, error) {
		if in.Type != event.CompletedBarEventType {
			return nil, nil
		}
		return decisions, nil
	})
}

// resolvingProposals is the minimum a producer must do for an order to leave
// the book: say so. This package never decides that a proposal has been
// executed — the reducer answers an entry fill with a Campaign-opened event
// and an Add fill with a unit-added one — so a handler that answers a fill
// with nothing leaves the order resting and the fixpoint fills it again.
func resolvingProposals(t *testing.T) replay.Handler {
	t.Helper()
	return replay.HandlerFunc(func(_ context.Context, in event.Envelope) ([]event.Envelope, error) {
		if in.Type != event.FillEventType {
			return nil, nil
		}
		var fill event.FillPayload
		decodeInto(t, in, &fill)
		return []event.Envelope{envelope(t, "expired:"+fill.ProposalID, event.ProposalExpiredEventType, event.ProposalExpiredSchemaVersion, fill.FilledAt, event.ProposalExpiredPayload{
			InstrumentID:   fill.InstrumentID,
			Kind:           event.ProposalKindEntry,
			ProposalID:     fill.ProposalID,
			SignalID:       "signal:AAPL:day-56",
			PeriodEnd:      day(56),
			ExpiredAt:      fill.FilledAt,
			EarliestFillAt: day(55),
			Rule:           event.RuleSignalExpiresWithItsBar,
			ADR:            event.ADRSignalExpiry,
			Reason:         event.ExpiryReasonSupersededByNextBar,
			Quantity:       fill.Quantity,
			Level:          fill.Level,
		})}, nil
	})
}

func newSimulator(t *testing.T) *fills.Simulator {
	t.Helper()
	simulator, err := fills.New(baselineConfig(), testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("fills.New() error = %v", err)
	}
	return simulator
}

// observeDecisions delivers one configuration envelope answered by decisions,
// and returns whatever the simulator made of them.
func observeDecisions(t *testing.T, simulator *fills.Simulator, decisions ...event.Envelope) error {
	t.Helper()
	_, err := fills.Deliver(context.Background(), simulator, emitting(decisions...), configurationEnvelope(t, baselineConfig()))
	return err
}

// malformed is an envelope of the given type whose payload is a JSON array.
// It is well-formed JSON — so nothing rejects it before the decode — and
// cannot decode into any of this package's payload structs.
func malformed(t *testing.T, eventType string, schemaVersion uint32) event.Envelope {
	t.Helper()
	return envelope(t, "malformed:"+eventType, eventType, schemaVersion, day(56), []any{})
}

const strayCampaign = "campaign:AAPL:never-opened"

func tradeProposal(t *testing.T, id string, level float64, quantity int64, n float64) event.Envelope {
	t.Helper()
	return envelope(t, id, event.TradeProposalEventType, event.TradeProposalSchemaVersion, day(56), event.TradeProposalPayload{
		InstrumentID:           testInstrument,
		PeriodEnd:              day(56),
		SignalID:               "signal:AAPL:day-56",
		Rule:                   event.RuleUnitSizingVolatilityNormalised,
		ADR:                    event.ADRUnitSizing,
		Direction:              event.DirectionLong,
		EntryLevel:             level,
		Quantity:               quantity,
		N:                      n,
		SizingMode:             event.SizingModeVolatilityNormalised,
		UnitVolatilityFraction: 0.005,
		StopMultiple:           2,
		DollarsPerPoint:        1,
		NotionalAccount:        1_000_000,
		ProtectiveStopIntent:   level - 2*fixtureN,
	})
}

func addProposal(t *testing.T, id, campaignID string, level float64, quantity int64, n float64) event.Envelope {
	t.Helper()
	return envelope(t, id, event.AddProposalEventType, event.AddProposalSchemaVersion, day(56), event.AddProposalPayload{
		CampaignID:       campaignID,
		InstrumentID:     testInstrument,
		PeriodEnd:        day(56),
		UnitIndex:        2,
		Level:            level,
		Quantity:         quantity,
		PreviousUnitFill: level - 0.5*fixtureN,
		CampaignN:        n,
		Rule:             event.RuleAddLadderHalfN,
		ADR:              event.ADRCampaignFrozenAtEntry,
	})
}

// campaignOpened is a Campaign as this package learns of one. campaignN is a
// parameter because the whole point of several tests below is a producer
// that reports an N this package cannot price slippage in.
func campaignOpened(t *testing.T, campaignID string, entryPrice, campaignN float64) event.Envelope {
	t.Helper()
	return envelope(t, campaignID, event.CampaignOpenedEventType, event.CampaignOpenedSchemaVersion, day(56), event.CampaignOpenedPayload{
		CampaignID:     campaignID,
		InstrumentID:   testInstrument,
		ProposalID:     "proposal:AAPL:day-56",
		SignalID:       "signal:AAPL:day-56",
		FillID:         "sim-fill-0001",
		Rule:           event.RuleCampaignOpenedFromFill,
		ADR:            event.ADRCampaignFrozenAtEntry,
		Direction:      event.DirectionLong,
		CampaignN:      campaignN,
		UnitQuantity:   fixtureUnitQuantity,
		FilledQuantity: fixtureUnitQuantity,
		EntryPrice:     entryPrice,
		StopMultiple:   2,
		ProtectiveStop: entryPrice - 2*fixtureN,
		Units:          1,
		OpenedAt:       day(56),
	})
}

func protectiveStopSet(t *testing.T, campaignID string, unitIndex int, level float64) event.Envelope {
	t.Helper()
	return envelope(t, "stop-set:AAPL:unit-"+strconv.Itoa(unitIndex), event.ProtectiveStopSetEventType, event.ProtectiveStopSetSchemaVersion, day(56), event.ProtectiveStopSetPayload{
		CampaignID:   campaignID,
		InstrumentID: testInstrument,
		UnitIndex:    unitIndex,
		Reason:       event.ProtectiveStopReasonInitial,
		AsOf:         day(56),
		Level:        level,
		EntryPrice:   level + 2*fixtureN,
		CampaignN:    fixtureN,
		StopMultiple: 2,
		Rule:         event.RuleProtectiveStopSetFromFill,
		ADR:          event.ADRCampaignFrozenAtEntry,
	})
}

// TestAnUndecodablePayloadFromAProducerFailsClosed covers every event type
// the book is learned from. The payload is a JSON array in each case: a
// producer whose encoder changed shape, or a journal line that was truncated
// and re-assembled. Decoding it into the payload struct silently would leave
// an order at level zero for quantity zero.
func TestAnUndecodablePayloadFromAProducerFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		eventType     string
		schemaVersion uint32
	}{
		{"trade proposal", event.TradeProposalEventType, event.TradeProposalSchemaVersion},
		{"add proposal", event.AddProposalEventType, event.AddProposalSchemaVersion},
		{"exit proposal", event.ExitProposalEventType, event.ExitProposalSchemaVersion},
		{"campaign opened", event.CampaignOpenedEventType, event.CampaignOpenedSchemaVersion},
		{"unit added", event.CampaignUnitAddedEventType, event.CampaignUnitAddedSchemaVersion},
		{"protective stop set", event.ProtectiveStopSetEventType, event.ProtectiveStopSetSchemaVersion},
		{"units stopped", event.CampaignUnitsStoppedEventType, event.CampaignUnitsStoppedSchemaVersion},
		{"campaign exited", event.CampaignExitedEventType, event.CampaignExitedSchemaVersion},
		{"proposal expired", event.ProposalExpiredEventType, event.ProposalExpiredSchemaVersion},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := observeDecisions(t, newSimulator(t), malformed(t, tt.eventType, tt.schemaVersion))
			if err == nil {
				t.Fatalf("observing a malformed %s returned nil, want a decode failure", tt.eventType)
			}
			if !strings.Contains(err.Error(), "decode "+tt.eventType+" payload") {
				t.Errorf("error = %v, want it to name the payload it could not decode", err)
			}
		})
	}
}

// TestAnEventForACampaignThisSimulatorDoesNotHoldFailsClosed is the
// reconciliation rule on the book's side: an event naming a Campaign no
// Campaign-opened event ever introduced cannot be applied to the right
// orders, and guessing which Campaign was meant is the failure this refuses.
//
// The exit proposal case is the one with a consequence beyond bookkeeping:
// an exit's slippage is measured in the Campaign's own frozen N (ADR 0006),
// so without the Campaign there is no N to price it in at all.
func TestAnEventForACampaignThisSimulatorDoesNotHoldFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		decision func(*testing.T) event.Envelope
		wantErr  string
	}{
		{
			name: "exit proposal",
			decision: func(t *testing.T) event.Envelope {
				t.Helper()
				return envelope(t, "exit-proposal:AAPL:day-56", event.ExitProposalEventType, event.ExitProposalSchemaVersion, day(56), event.ExitProposalPayload{
					CampaignID:   strayCampaign,
					InstrumentID: testInstrument,
					PeriodEnd:    day(56),
					Reason:       event.ExitReasonExitChannel,
					Level:        150,
					Quantity:     fixtureUnitQuantity,
					Rule:         event.RuleExitChannelBreach,
					ADR:          event.ADRExitChannelBreach,
				})
			},
			wantErr: "exit proposal",
		},
		{
			name: "unit added",
			decision: func(t *testing.T) event.Envelope {
				t.Helper()
				return envelope(t, "unit-added:AAPL:2", event.CampaignUnitAddedEventType, event.CampaignUnitAddedSchemaVersion, day(56), event.CampaignUnitAddedPayload{
					CampaignID:     strayCampaign,
					InstrumentID:   testInstrument,
					UnitIndex:      2,
					FillID:         "sim-fill-0002",
					FillPrice:      160,
					Quantity:       fixtureUnitQuantity,
					CampaignN:      fixtureN,
					StopMultiple:   2,
					ProtectiveStop: 157,
					Units:          2,
					AddedAt:        day(56),
					Rule:           event.RuleAddLadderHalfN,
					ADR:            event.ADRCampaignFrozenAtEntry,
				})
			},
			wantErr: "unit-added",
		},
		{
			name: "protective stop set",
			decision: func(t *testing.T) event.Envelope {
				t.Helper()
				return protectiveStopSet(t, strayCampaign, 1, 150)
			},
			wantErr: "protective-stop-set",
		},
		{
			name: "units stopped",
			decision: func(t *testing.T) event.Envelope {
				t.Helper()
				return envelope(t, "units-stopped:AAPL:day-56", event.CampaignUnitsStoppedEventType, event.CampaignUnitsStoppedSchemaVersion, day(56), event.CampaignUnitsStoppedPayload{
					CampaignID:      strayCampaign,
					InstrumentID:    testInstrument,
					FillID:          "sim-fill-0003",
					UnitIndexes:     []int{1},
					FillPrice:       150,
					QuantityClosed:  fixtureUnitQuantity,
					EntryPrice:      156,
					CampaignN:       fixtureN,
					DollarsPerPoint: 1,
					StoppedAt:       day(56),
					Rule:            event.RuleProtectiveStopSetFromFill,
					ADR:             event.ADRCampaignExitRecordsTheFill,
				})
			},
			wantErr: "units-stopped",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := observeDecisions(t, newSimulator(t), tt.decision(t))
			if err == nil {
				t.Fatal("observing an event for an unheld campaign returned nil, want it to fail closed")
			}
			if !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), strayCampaign) {
				t.Errorf("error = %v, want it to name %q and the campaign %q", err, tt.wantErr, strayCampaign)
			}
		})
	}
}

// TestAProtectiveStopForAUnitThisSimulatorDoesNotHoldFailsClosed is the same
// rule one level down: the Campaign is held, the Unit is not. Applying the
// level to no Unit at all would leave that Unit's stop resting where it was,
// which is the shape of a stop that silently fails to move.
func TestAProtectiveStopForAUnitThisSimulatorDoesNotHoldFailsClosed(t *testing.T) {
	t.Parallel()

	const campaignID = "campaign:AAPL:day-56"
	err := observeDecisions(t, newSimulator(t),
		campaignOpened(t, campaignID, 156, fixtureN),
		protectiveStopSet(t, campaignID, 4, 150),
	)
	if err == nil {
		t.Fatal("observing a protective stop for an unheld unit returned nil, want it to fail closed")
	}
	if !strings.Contains(err.Error(), "unit 4") {
		t.Errorf("error = %v, want it to name the unit it does not hold", err)
	}
}

// TestRestingOmitsAUnitWhoseProtectiveStopHasNotArrivedYet pins the shape of
// the window between a Campaign opening and its first stop-set event. The
// reducer emits both in one Apply return, so the window does not exist in the
// composed system — but Resting is a read of whatever the book holds at the
// moment it is called, and reporting a stop order resting at level zero would
// be a claim that the position is protected at zero.
func TestRestingOmitsAUnitWhoseProtectiveStopHasNotArrivedYet(t *testing.T) {
	t.Parallel()

	simulator := newSimulator(t)
	if err := observeDecisions(t, simulator, campaignOpened(t, "campaign:AAPL:day-56", 156, fixtureN)); err != nil {
		t.Fatalf("observing a campaign-opened event: %v", err)
	}
	if resting := simulator.Resting(testInstrument); len(resting) != 0 {
		t.Fatalf("Resting() = %+v, want nothing: the unit has no protective stop yet", resting)
	}
}

// TestAnOrderTheBarNeverReachesDoesNotFill is the negative case the fixtures
// otherwise never produce, because every proposal the reducer raises is
// covered by the bar that raised it. An order resting above the bar's high is
// left alone and stays resting.
func TestAnOrderTheBarNeverReachesDoesNotFill(t *testing.T) {
	t.Parallel()

	simulator := newSimulator(t)
	if err := observeDecisions(t, simulator, tradeProposal(t, "proposal:AAPL:day-56", 500, fixtureUnitQuantity, fixtureN)); err != nil {
		t.Fatalf("observing a trade proposal: %v", err)
	}

	result, err := fills.RunBar(context.Background(), simulator, emitting(), barEnvelope(t, bar(day(57), 150, 160, 149, 155)))
	if err != nil {
		t.Fatalf("RunBar() error = %v", err)
	}
	if fills := envelopesOfType(result.Inputs, event.FillEventType); len(fills) != 0 {
		t.Errorf("got %d fill(s), want none: the bar's high of 160 never reached the order at 500", len(fills))
	}
	if resting := simulator.Resting(testInstrument); len(resting) != 1 {
		t.Errorf("Resting() = %+v, want the order still resting", resting)
	}
}

// TestTwoCoveredBuysFillInAscendingLevelOrder is the ordering ADR 0005 rule 3
// states for buys, asserted rather than assumed.
//
// The reducer cannot produce this state: it clears the entry proposal the
// instant the Campaign that fill opened is journalled, and an Add proposal
// exists only after that, so at most one buy rests at a time. The rule is
// nonetheless the one that makes the Add chain correct — rung n+1 is measured
// from rung n's ACTUAL fill, so filling the higher rung first would measure
// the next rung from the wrong price — and this package takes its book from
// whatever producer it is paired with, not from that reducer's timing.
func TestTwoCoveredBuysFillInAscendingLevelOrder(t *testing.T) {
	t.Parallel()

	const campaignID = "campaign:AAPL:day-56"
	simulator := newSimulator(t)
	// The higher rung is observed FIRST, so a sort that did nothing would
	// leave it first and fill it first.
	if err := observeDecisions(t, simulator,
		addProposal(t, "add-proposal:AAPL:day-56", campaignID, 157, fixtureUnitQuantity, fixtureN),
		tradeProposal(t, "proposal:AAPL:day-56", 156, fixtureUnitQuantity, fixtureN),
	); err != nil {
		t.Fatalf("observing two resting buys: %v", err)
	}

	result, err := fills.RunBar(context.Background(), simulator, resolvingProposals(t), barEnvelope(t, bar(day(57), 150, 160, 149, 158)))
	if err != nil {
		t.Fatalf("RunBar() error = %v", err)
	}

	filled := fillPayloads(t, envelopesOfType(result.Inputs, event.FillEventType))
	if len(filled) != 2 {
		t.Fatalf("got %d fill(s), want 2 (both buys covered by a bar reaching 160)", len(filled))
	}
	if filled[0].Kind != event.FillKindEntry || filled[1].Kind != event.FillKindAdd {
		t.Fatalf("fills arrived as %s then %s, want the lower level (%s at 156) before the higher (%s at 157)",
			filled[0].Kind, filled[1].Kind, event.FillKindEntry, event.FillKindAdd)
	}
	if filled[0].Level >= filled[1].Level {
		t.Errorf("fill levels are %v then %v, want ascending", filled[0].Level, filled[1].Level)
	}
}

// TestAnOrderCarryingAnUnusableNFailsClosed covers all three order kinds.
// Slippage is measured in N (ADR 0013), so an order whose N is zero cannot be
// priced at all; filling it at its level would silently produce the
// zero-slippage run ADR 0013 calls invalid by construction.
func TestAnOrderCarryingAnUnusableNFailsClosed(t *testing.T) {
	t.Parallel()

	const campaignID = "campaign:AAPL:day-56"

	tests := []struct {
		name      string
		decisions func(*testing.T) []event.Envelope
		wantKind  string
	}{
		{
			name: "entry order",
			decisions: func(t *testing.T) []event.Envelope {
				t.Helper()
				return []event.Envelope{tradeProposal(t, "proposal:AAPL:day-56", 156, fixtureUnitQuantity, 0)}
			},
			wantKind: event.FillKindEntry,
		},
		{
			name: "protective stop order",
			decisions: func(t *testing.T) []event.Envelope {
				t.Helper()
				return []event.Envelope{
					campaignOpened(t, campaignID, 156, 0),
					protectiveStopSet(t, campaignID, 1, 153),
				}
			},
			wantKind: event.FillKindStop,
		},
		{
			name: "exit order",
			decisions: func(t *testing.T) []event.Envelope {
				t.Helper()
				return []event.Envelope{
					campaignOpened(t, campaignID, 156, 0),
					envelope(t, "exit-proposal:AAPL:day-56", event.ExitProposalEventType, event.ExitProposalSchemaVersion, day(56), event.ExitProposalPayload{
						CampaignID:   campaignID,
						InstrumentID: testInstrument,
						PeriodEnd:    day(56),
						Reason:       event.ExitReasonExitChannel,
						Level:        150,
						Quantity:     fixtureUnitQuantity,
						Rule:         event.RuleExitChannelBreach,
						ADR:          event.ADRExitChannelBreach,
					}),
				}
			},
			wantKind: event.FillKindExit,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			simulator := newSimulator(t)
			if err := observeDecisions(t, simulator, tt.decisions(t)...); err != nil {
				t.Fatalf("observing the fixture's decisions: %v", err)
			}

			_, err := fills.RunBar(context.Background(), simulator, emitting(), barEnvelope(t, bar(day(57), 150, 160, 149, 155)))
			if err == nil {
				t.Fatal("RunBar() error = nil, want an order with an unusable n to fail closed")
			}
			if !strings.Contains(err.Error(), "slippage is measured in n") || !strings.Contains(err.Error(), tt.wantKind) {
				t.Errorf("error = %v, want it to name %q and the rule slippage is measured by", err, tt.wantKind)
			}
		})
	}
}

// TestAnOrderRestingAtANonPositiveLevelFailsClosed reaches Execute's own
// refusals through the simulator: no consumer can record a non-positive fill
// price (event.FillPayload.Validate), so a level at or below zero is an error
// rather than something to clamp.
func TestAnOrderRestingAtANonPositiveLevelFailsClosed(t *testing.T) {
	t.Parallel()

	simulator := newSimulator(t)
	if err := observeDecisions(t, simulator, tradeProposal(t, "proposal:AAPL:day-56", -5, fixtureUnitQuantity, fixtureN)); err != nil {
		t.Fatalf("observing a trade proposal: %v", err)
	}

	_, err := fills.RunBar(context.Background(), simulator, emitting(), barEnvelope(t, bar(day(57), 150, 160, 149, 155)))
	if err == nil {
		t.Fatal("RunBar() error = nil, want an order at a non-positive level to fail closed")
	}
	if !strings.Contains(err.Error(), "level must be positive") {
		t.Errorf("error = %v, want it to name the level", err)
	}
}

// TestAnOrderRestingForNoSharesFailsClosed reaches the commission model from
// the fill-building path: a charge for zero shares has no meaning, and a fill
// recorded without one would understate the cost of the run.
func TestAnOrderRestingForNoSharesFailsClosed(t *testing.T) {
	t.Parallel()

	simulator := newSimulator(t)
	if err := observeDecisions(t, simulator, tradeProposal(t, "proposal:AAPL:day-56", 156, 0, fixtureN)); err != nil {
		t.Fatalf("observing a trade proposal: %v", err)
	}

	_, err := fills.RunBar(context.Background(), simulator, emitting(), barEnvelope(t, bar(day(57), 150, 160, 149, 155)))
	if err == nil {
		t.Fatal("RunBar() error = nil, want an order for no shares to fail closed")
	}
	if !strings.Contains(err.Error(), "commission quantity") {
		t.Errorf("error = %v, want it to name the commission quantity", err)
	}
}

// TestAnOrderRaisedInsideTheBarIsPricedAndCanFailClosed separates step 3 from
// step 1: the order does not exist when the bar opens, so only the intrabar
// fixpoint can see it. Without this, a single test would leave one of the two
// passes unexercised on the failure path and neither would be visibly
// missing.
func TestAnOrderRaisedInsideTheBarIsPricedAndCanFailClosed(t *testing.T) {
	t.Parallel()

	simulator := newSimulator(t)
	handler := emittingOnBar(tradeProposal(t, "proposal:AAPL:day-57", 156, fixtureUnitQuantity, 0))

	_, err := fills.RunBar(context.Background(), simulator, handler, barEnvelope(t, bar(day(57), 150, 160, 149, 155)))
	if err == nil {
		t.Fatal("RunBar() error = nil, want the intrabar pass to price the order raised inside the bar")
	}
	if !strings.Contains(err.Error(), "slippage is measured in n") {
		t.Errorf("error = %v, want the intrabar pass to have priced the order", err)
	}
}

// TestABarThatKeepsRefillingTheSameOrderIsBounded is the runaway-fill guard,
// and it is the only thing bounding the intrabar fixpoint. The fixpoint
// re-evaluates after every fill because that is what makes the Add chain work
// — the next rung does not exist until the fill before it is accepted — so a
// producer that re-raises a resolved order rather than resolving it would
// otherwise loop for ever, emitting fills all the way.
//
// The reducer's own maximum Units (ADR 0008) bounds the legitimate count far
// below the cap, so exceeding it means an order was refilled rather than
// resolved.
func TestABarThatKeepsRefillingTheSameOrderIsBounded(t *testing.T) {
	t.Parallel()

	simulator := newSimulator(t)
	raised := 0
	// A producer that answers every fill by re-raising the same proposal:
	// the order returns to the book as fast as it leaves it.
	handler := replay.HandlerFunc(func(_ context.Context, in event.Envelope) ([]event.Envelope, error) {
		if in.Type != event.CompletedBarEventType && in.Type != event.FillEventType {
			return nil, nil
		}
		raised++
		return []event.Envelope{tradeProposal(t, "proposal:AAPL:refill", 156, fixtureUnitQuantity, fixtureN)}, nil
	})

	_, err := fills.RunBar(context.Background(), simulator, handler, barEnvelope(t, bar(day(57), 150, 160, 149, 155)))
	if err == nil {
		t.Fatal("RunBar() error = nil, want the runaway-fill guard to stop the bar")
	}
	if !strings.Contains(err.Error(), "produced more than") {
		t.Errorf("error = %v, want the runaway-fill guard's own message", err)
	}
	if raised < 2 {
		t.Errorf("the producer was asked %d time(s); the fixture is not exercising the fixpoint", raised)
	}
}

// TestACancelledContextStopsBeforeAnythingIsDelivered is the only guard that
// runs before an envelope is numbered. Delivering after cancellation would
// advance the composed stream's sequence for an event the run never applied,
// and replay.Engine.Run refuses a stream with a gap.
func TestACancelledContextStopsBeforeAnythingIsDelivered(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fills.Deliver(ctx, newSimulator(t), emitting(), configurationEnvelope(t, baselineConfig()))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Deliver() error = %v, want context.Canceled", err)
	}
}

// TestAnInvalidInputEnvelopeIsRefusedBeforeTheHandlerSeesIt keeps the
// composed input stream to the same contract as any other: what the loop
// journals must be replayable, and an envelope that fails its own Validate
// cannot be.
func TestAnInvalidInputEnvelopeIsRefusedBeforeTheHandlerSeesIt(t *testing.T) {
	t.Parallel()

	input := configurationEnvelope(t, baselineConfig())
	input.ID = ""

	applied := false
	handler := replay.HandlerFunc(func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
		applied = true
		return nil, nil
	})

	_, err := fills.Deliver(context.Background(), newSimulator(t), handler, input)
	if err == nil {
		t.Fatal("Deliver() error = nil, want an invalid input envelope to be refused")
	}
	if !strings.Contains(err.Error(), "input 1") {
		t.Errorf("error = %v, want it to name the sequence it refused", err)
	}
	if applied {
		t.Error("the handler was applied to an envelope that failed its own Validate")
	}
}

// TestAnUnrecognisedDecisionTypeIsRefusedInPlace names which emission of
// which event was refused, because a producer emitting several decisions for
// one input gives no other way to tell them apart.
func TestAnUnrecognisedDecisionTypeIsRefusedInPlace(t *testing.T) {
	t.Parallel()

	stray := envelope(t, "stray", "strategy.something.new", 1, day(56), map[string]any{"instrument_id": testInstrument})

	err := observeDecisions(t, newSimulator(t), stray)
	if err == nil {
		t.Fatal("observing an unrecognised decision type returned nil, want it to fail closed")
	}
	if !strings.Contains(err.Error(), "emission 0 of event") {
		t.Errorf("error = %v, want it to name which emission was refused", err)
	}
}

// TestRunBarRefusesAnUndecodableBarPayload is the bar's own decode, which
// happens before any order is priced: a bar that cannot be read is not a bar
// with no trading in it.
func TestRunBarRefusesAnUndecodableBarPayload(t *testing.T) {
	t.Parallel()

	_, err := fills.RunBar(context.Background(), newSimulator(t), emitting(),
		malformed(t, event.CompletedBarEventType, event.CompletedBarSchemaVersion))
	if err == nil {
		t.Fatal("RunBar() error = nil, want an undecodable bar payload to fail closed")
	}
	if !strings.Contains(err.Error(), "decode completed bar payload") {
		t.Errorf("error = %v, want it to name the payload it could not decode", err)
	}
}

// TestAProducerThatFailsOnAFillStopsTheBar is the other half of the fixpoint's
// error handling: the fill itself was priced and built correctly, and the
// producer refused it. The bar stops there rather than continuing to fill
// orders against a reducer that has already failed closed — and the fill is
// still recorded, because a handler that fails may emit a final event
// explaining why and that event must reach the journal.
func TestAProducerThatFailsOnAFillStopsTheBar(t *testing.T) {
	t.Parallel()

	simulator := newSimulator(t)
	if err := observeDecisions(t, simulator, tradeProposal(t, "proposal:AAPL:day-56", 156, fixtureUnitQuantity, fixtureN)); err != nil {
		t.Fatalf("observing a trade proposal: %v", err)
	}

	refused := errors.New("the producer refused this fill")
	handler := replay.HandlerFunc(func(_ context.Context, in event.Envelope) ([]event.Envelope, error) {
		if in.Type == event.FillEventType {
			return nil, refused
		}
		return nil, nil
	})

	result, err := fills.RunBar(context.Background(), simulator, handler, barEnvelope(t, bar(day(57), 150, 160, 149, 155)))
	if !errors.Is(err, refused) {
		t.Fatalf("RunBar() error = %v, want the producer's own error", err)
	}
	if filled := envelopesOfType(result.Inputs, event.FillEventType); len(filled) != 1 {
		t.Errorf("got %d fill(s) in the result, want the refused fill to still be recorded", len(filled))
	}
}
