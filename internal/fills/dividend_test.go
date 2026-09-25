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

// A dividend (ADR 0004, ADR 0024) reaches the simulated account only through
// the reducer's own strategy.campaign.dividend emission, exactly as a
// split's cash in lieu does (split_test.go). The corporate action itself,
// and the decision it produces, change nothing about the resting-order book.

func dividendIn(t *testing.T, id string, when time.Time, cash float64) event.Envelope {
	t.Helper()
	return envelope(t, id, event.MarketCorporateActionEventType, event.MarketCorporateActionSchemaVersion, when,
		event.CorporateActionPayload{
			InstrumentID: testInstrument, Kind: event.CorporateActionKindDividend, EffectiveAt: when,
			CashAmount: cash, Currency: "USD",
		})
}

// TestADividendCreditsTheSimulatedAccountFromTheEmissions is the headline
// fill-simulator case: the account's cash rises by exactly the dividend, and
// every resting order is untouched.
func TestADividendCreditsTheSimulatedAccountFromTheEmissions(t *testing.T) {
	t.Parallel()

	simulator, recorder, _ := fourUnits(t)
	ctx := context.Background()
	before := stopsResting(simulator)
	account := simulator.Account()

	const cash = 184.0
	result, err := fills.Deliver(ctx, simulator, recorder, dividendIn(t, "dividend-1", day(59).Add(12*time.Hour), cash))
	if err != nil {
		t.Fatalf("Deliver(dividend) error = %v", err)
	}
	if got := len(envelopesOfType(result.Decisions, event.CampaignDividendEventType)); got != 1 {
		t.Fatalf("got %d dividend decision(s), want 1", got)
	}

	after := simulator.Account()
	if after.Cash != account.Cash+cash {
		t.Fatalf("account cash after the dividend = %v, want %v", after.Cash, account.Cash+cash)
	}
	if after.Holdings[testInstrument] != account.Holdings[testInstrument] {
		t.Fatalf("account holding after the dividend = %d, want the unchanged %d: a dividend never changes a holding", after.Holdings[testInstrument], account.Holdings[testInstrument])
	}
	if !reflect.DeepEqual(before, stopsResting(simulator)) {
		t.Fatal("a dividend changed a resting Exit Order; it must change nothing about the book (ADR 0024)")
	}
}

// TestADividendInputChangesNoRestingOrder: the corporate action itself is
// never interpreted by the book (ADR 0024; fills.observe), mirroring
// TestASplitInputChangesNoRestingOrder.
func TestADividendInputChangesNoRestingOrder(t *testing.T) {
	t.Parallel()

	simulator, _, _ := fourUnits(t)
	before := simulator.Resting(testInstrument)
	if err := simulator.Observe(dividendIn(t, "dividend-1", day(59).Add(12*time.Hour), 184.0)); err != nil {
		t.Fatalf("Observe(dividend) error = %v", err)
	}
	if !reflect.DeepEqual(before, simulator.Resting(testInstrument)) {
		t.Fatal("a dividend input changed the resting-order book before the reducer's emissions")
	}
}

// TestADividendDecisionTheSimulatorCannotApplyFailsClosed covers
// observeDividend's own decode and Validate failures, mirroring
// TestACashInLieuTheBookCannotApplyFailsClosed's undecodable/invalid cases.
func TestADividendDecisionTheSimulatorCannotApplyFailsClosed(t *testing.T) {
	t.Parallel()

	simulator, _, _ := fourUnits(t)
	decision := func(mutate func(*event.CampaignDividendPayload)) event.Envelope {
		payload := event.CampaignDividendPayload{
			CampaignID: "campaign:AAPL:corrupted", InstrumentID: testInstrument,
			CorporateActionID: "dividend-1", EffectiveAt: day(59).Add(12 * time.Hour),
			CashAmount: 184.0, Currency: "USD",
			Rule: event.RuleDividendCreditedAsCash, ADR: event.ADRDividendCreditedAsCash,
		}
		mutate(&payload)
		return envelope(t, "dividend-decision", event.CampaignDividendEventType, event.CampaignDividendSchemaVersion, payload.EffectiveAt, payload)
	}

	invalid := decision(func(p *event.CampaignDividendPayload) { p.Rule = "" })
	if err := simulator.Observe(invalid); err == nil || !strings.Contains(err.Error(), "rule") {
		t.Fatalf("Observe(invalid dividend) error = %v, want it to name the missing rule", err)
	}

	undecodable := decision(func(*event.CampaignDividendPayload) {})
	undecodable.Payload = []byte(`42`)
	if err := simulator.Observe(undecodable); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("Observe(undecodable dividend) error = %v, want a decode failure", err)
	}
}
