package universe_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/universe"
)

func TestFixtureUniverseClassifyReportsADeclaredInstrument(t *testing.T) {
	t.Parallel()
	fixture := universe.NewFixtureUniverse(map[string]universe.Classification{
		"AAPL": {SecurityType: universe.SecurityTypeCommonStock, USPrimaryExchange: true},
	})
	got, ok := fixture.Classify("AAPL")
	if !ok {
		t.Fatal("Classify(\"AAPL\") ok = false, want true")
	}
	want := universe.Classification{SecurityType: universe.SecurityTypeCommonStock, USPrimaryExchange: true}
	if got != want {
		t.Errorf("Classify(\"AAPL\") = %+v, want %+v", got, want)
	}
}

func TestFixtureUniverseClassifyReportsNotOKForAnUndeclaredInstrument(t *testing.T) {
	t.Parallel()
	fixture := universe.NewFixtureUniverse(map[string]universe.Classification{
		"AAPL": {SecurityType: universe.SecurityTypeCommonStock, USPrimaryExchange: true},
	})
	if _, ok := fixture.Classify("SPY"); ok {
		t.Fatal("Classify(\"SPY\") ok = true, want false: SPY was never declared")
	}
}

// TestFixtureUniverseCopiesItsInputTable ensures a caller mutating the map
// it constructed the fixture from cannot change what the fixture reports —
// NewFixtureUniverse's own doc comment states this.
func TestFixtureUniverseCopiesItsInputTable(t *testing.T) {
	t.Parallel()
	table := map[string]universe.Classification{
		"AAPL": {SecurityType: universe.SecurityTypeCommonStock, USPrimaryExchange: true},
	}
	fixture := universe.NewFixtureUniverse(table)
	table["AAPL"] = universe.Classification{SecurityType: universe.SecurityTypeETF, USPrimaryExchange: true}
	table["SPY"] = universe.Classification{SecurityType: universe.SecurityTypeETF, USPrimaryExchange: true}

	got, ok := fixture.Classify("AAPL")
	if !ok || got.SecurityType != universe.SecurityTypeCommonStock {
		t.Fatalf("Classify(\"AAPL\") = %+v, ok = %v; mutating the caller's map changed the fixture's own table", got, ok)
	}
	if _, ok := fixture.Classify("SPY"); ok {
		t.Fatal("Classify(\"SPY\") ok = true, want false: SPY was added to the caller's map after construction")
	}
}

func TestClassificationCommonStockOnUSPrimaryExchange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		c    universe.Classification
		want bool
	}{
		{"common stock, US primary exchange", universe.Classification{SecurityType: universe.SecurityTypeCommonStock, USPrimaryExchange: true}, true},
		{"common stock, not a US primary exchange", universe.Classification{SecurityType: universe.SecurityTypeCommonStock, USPrimaryExchange: false}, false},
		{"etf on a US primary exchange", universe.Classification{SecurityType: universe.SecurityTypeETF, USPrimaryExchange: true}, false},
		{"adr on a US primary exchange", universe.Classification{SecurityType: universe.SecurityTypeADR, USPrimaryExchange: true}, false},
		{"spac on a US primary exchange", universe.Classification{SecurityType: universe.SecurityTypeSPAC, USPrimaryExchange: true}, false},
		{"zero value", universe.Classification{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.c.CommonStockOnUSPrimaryExchange(); got != tc.want {
				t.Errorf("%+v.CommonStockOnUSPrimaryExchange() = %v, want %v", tc.c, got, tc.want)
			}
		})
	}
}
