package fills_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
)

// A symbol change (ADR 0024) reaches the book and the simulated account only
// through the reducer's own strategy.instrument.symbol-changed emission,
// mirroring split_test.go and dividend_test.go: the resting-order book and
// the account's holding and latest close all move from the old instrument id
// to the new one, and the corporate action input itself changes nothing.

func symbolChangeIn(t *testing.T, id, oldID, newID string, when time.Time) event.Envelope {
	t.Helper()
	return envelope(t, id, event.MarketCorporateActionEventType, event.MarketCorporateActionSchemaVersion, when,
		event.CorporateActionPayload{
			InstrumentID: oldID, Kind: event.CorporateActionKindSymbolChange, EffectiveAt: when,
			NewInstrumentID: newID,
		})
}

// TestASymbolChangeMovesTheBookAndTheAccountFromTheEmissions is the headline
// fill-simulator case: every resting order and the whole holding move to the
// new instrument id, byte for byte, and the old id is left with nothing.
func TestASymbolChangeMovesTheBookAndTheAccountFromTheEmissions(t *testing.T) {
	t.Parallel()

	simulator, recorder, _ := fourUnits(t)
	ctx := context.Background()
	before := stopsResting(simulator)
	account := simulator.Account()

	result, err := fills.Deliver(ctx, simulator, recorder, symbolChangeIn(t, "rename-1", testInstrument, "AAPL2", day(59).Add(12*time.Hour)))
	if err != nil {
		t.Fatalf("Deliver(symbol change) error = %v", err)
	}
	if got := len(envelopesOfType(result.Decisions, event.InstrumentSymbolChangedEventType)); got != 1 {
		t.Fatalf("got %d symbol-changed decision(s), want 1", got)
	}

	if resting := simulator.Resting(testInstrument); len(resting) != 0 {
		t.Fatalf("still resting %d order(s) under the old instrument id %q, want none", len(resting), testInstrument)
	}
	after := stopsResting2(simulator, "AAPL2")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("Exit Orders under the new instrument id = %v, want the same %v the old id held", after, before)
	}

	gotAccount := simulator.Account()
	if held, ok := gotAccount.Holdings[testInstrument]; ok && held != 0 {
		t.Fatalf("the account still holds %d under the old instrument id %q", held, testInstrument)
	}
	if gotAccount.Holdings["AAPL2"] != account.Holdings[testInstrument] {
		t.Fatalf("the account holds %d under the new instrument id, want the %d the old id held", gotAccount.Holdings["AAPL2"], account.Holdings[testInstrument])
	}
	if gotAccount.Cash != account.Cash {
		t.Fatalf("account cash changed from %v to %v: a symbol change credits no cash", account.Cash, gotAccount.Cash)
	}
}

// TestASymbolChangeInputChangesNoRestingOrder: the corporate action itself is
// never interpreted by the book (ADR 0024; fills.observe), mirroring
// TestASplitInputChangesNoRestingOrder and TestADividendInputChangesNoRestingOrder.
func TestASymbolChangeInputChangesNoRestingOrder(t *testing.T) {
	t.Parallel()

	simulator, _, _ := fourUnits(t)
	before := simulator.Resting(testInstrument)
	if err := simulator.Observe(symbolChangeIn(t, "rename-1", testInstrument, "AAPL2", day(59).Add(12*time.Hour))); err != nil {
		t.Fatalf("Observe(symbol change) error = %v", err)
	}
	if !reflect.DeepEqual(before, simulator.Resting(testInstrument)) {
		t.Fatal("a symbol change input changed the resting-order book before the reducer's emissions")
	}
}

// TestASymbolChangeDecisionTheSimulatorCannotApplyFailsClosed covers
// observeSymbolChanged's own decode and Validate failures, mirroring
// TestACashInLieuTheBookCannotApplyFailsClosed's undecodable/invalid cases.
func TestASymbolChangeDecisionTheSimulatorCannotApplyFailsClosed(t *testing.T) {
	t.Parallel()

	simulator, _, _ := fourUnits(t)
	decision := func(mutate func(*event.InstrumentSymbolChangedPayload)) event.Envelope {
		payload := event.InstrumentSymbolChangedPayload{
			InstrumentID: testInstrument, NewInstrumentID: "AAPL2",
			CorporateActionID: "rename-1", EffectiveAt: day(59).Add(12 * time.Hour),
			CampaignID: "campaign:AAPL:corrupted",
			Rule:       event.RuleSymbolChangeCarriesInstrumentState, ADR: event.ADRSymbolChangeCarriesInstrumentState,
		}
		mutate(&payload)
		return envelope(t, "symbol-change-decision", event.InstrumentSymbolChangedEventType, event.InstrumentSymbolChangedSchemaVersion, payload.EffectiveAt, payload)
	}

	invalid := decision(func(p *event.InstrumentSymbolChangedPayload) { p.Rule = "" })
	if err := simulator.Observe(invalid); err == nil || !strings.Contains(err.Error(), "rule") {
		t.Fatalf("Observe(invalid symbol change) error = %v, want it to name the missing rule", err)
	}

	undecodable := decision(func(*event.InstrumentSymbolChangedPayload) {})
	undecodable.Payload = []byte(`42`)
	if err := simulator.Observe(undecodable); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("Observe(undecodable symbol change) error = %v, want a decode failure", err)
	}
}

// stopsResting2 is stopsResting for an arbitrary instrument id, mirroring
// split_test.go's stopsResting, which is fixed to testInstrument.
func stopsResting2(sim *fills.Simulator, instrumentID string) map[int]int64 {
	out := make(map[int]int64)
	for _, o := range sim.Resting(instrumentID) {
		if o.Side == fills.SideSell {
			for _, index := range o.UnitIndexes {
				out[index] = o.Quantity
			}
		}
	}
	return out
}
