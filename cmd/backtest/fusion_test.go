package main

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// The golden fixture must remain sensitive to fused multiply-add to enforce
// byte-identical replay (ADR 0017; docs/development.md). The Go specification
// permits an implementation to fuse a multiply-add, and permits it not to; it
// guarantees nothing per architecture. What this project observed is narrower
// and is the reason the rule exists: with gc 1.24.4, linux/arm64 fused and
// linux/amd64 did not, and a committed golden that passed on one failed on the
// other. Treat that as a property of a toolchain and target, not of arm64, and
// re-establish it rather than assume it if either changes.
//
// These four Units use quantity-price products needing more than 53 bits, so
// the weighted entry price and aggregate open risk diverge when a product is
// left fusible; making all products exact would let the golden pass even with
// fusible arithmetic. The Campaign closes in one exit fill for all four Units
// (each Unit's Exit Order rests at the Exit Channel; ADR 0005, as amended),
// so its exit side is a single product with nothing to accumulate: the entry
// side is what this fixture's sensitivity rests on, and the exit side is
// checked only for recording the rounded product.

// fill is one execution's quantity and price.
type fill struct {
	quantity, price float64
}

// fusedSum accumulates quantity x price the way a fusing build would if the
// product were not rounded first: one operation, one rounding, the
// full-precision product kept. math.FMA states that shape explicitly rather
// than relying on any target to produce it.
func fusedSum(fills []fill) float64 {
	var sum float64
	for _, f := range fills {
		sum = math.FMA(f.quantity, f.price, sum)
	}
	return sum
}

// roundedSum is the same accumulation with each product rounded to float64
// before it is added, which is what sizing.Product guarantees.
func roundedSum(fills []fill) float64 {
	var sum float64
	for _, f := range fills {
		sum += float64(f.quantity * f.price)
	}
	return sum
}

// quantityOf is the shares fills executed.
func quantityOf(fills []fill) float64 {
	var total float64
	for _, f := range fills {
		total += f.quantity
	}
	return total
}

// goldenFills returns the fills the fixture's Campaign entered with and the
// fills it was closed with.
func goldenFills(t *testing.T) (entries, exits []fill) {
	t.Helper()

	written, _ := runBacktestTo(t)
	_, records, err := journal.Read(bytes.NewReader(written))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}

	for _, record := range records {
		if record.Envelope.Type != event.FillEventType {
			continue
		}
		var payload event.FillPayload
		if err := json.Unmarshal(record.Envelope.Payload, &payload); err != nil {
			t.Fatalf("decode fill %s: %v", record.Envelope.ID, err)
		}
		executed := fill{quantity: float64(payload.Quantity), price: payload.Price}
		switch payload.Kind {
		case event.FillKindEntry, event.FillKindAdd:
			entries = append(entries, executed)
		default:
			exits = append(exits, executed)
		}
	}
	return entries, exits
}

// TestTheGoldenFixtureIsSensitiveToFusedMultiplyAdd: accumulating the
// Campaign's own entry fills with the product fused gives a different answer
// from accumulating them with the product rounded. That difference is what
// the golden journal detects.
func TestTheGoldenFixtureIsSensitiveToFusedMultiplyAdd(t *testing.T) {
	entries, _ := goldenFills(t)

	if len(entries) < 2 {
		t.Fatalf("the fixture fills %d Unit(s) in; an accumulator needs at least two terms to diverge", len(entries))
	}
	if rounded, fused := roundedSum(entries), fusedSum(entries); rounded == fused {
		t.Errorf("the weighted entry price accumulates to %v either way: the fixture's prices no longer tell a fused build from an unfused one, and the golden journal no longer guards against one",
			rounded)
	}
}

// TestTheRecordedCampaignUsesTheRoundedAccumulation is the other half: the
// fixture is sensitive, and what the journal actually recorded is the
// rounded answer rather than the fused one.
func TestTheRecordedCampaignUsesTheRoundedAccumulation(t *testing.T) {
	entries, exits := goldenFills(t)

	written, _ := runBacktestTo(t)
	_, records, err := journal.Read(bytes.NewReader(written))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}

	var exited event.CampaignExitedPayload
	var found bool
	for _, record := range records {
		if record.Envelope.Type != event.CampaignExitedEventType {
			continue
		}
		if err := json.Unmarshal(record.Envelope.Payload, &exited); err != nil {
			t.Fatalf("decode the campaign exited record: %v", err)
		}
		found = true
	}
	if !found {
		t.Fatal("the fixture records no campaign exit to check the accumulation against")
	}

	for _, side := range []struct {
		name  string
		fills []fill
		got   float64
	}{
		{"entry price", entries, exited.EntryPrice},
		{"exit price", exits, exited.ExitPrice},
	} {
		total := quantityOf(side.fills)
		want := roundedSum(side.fills) / total
		fused := fusedSum(side.fills) / total
		if side.got != want {
			t.Errorf("recorded %s = %v, want %v (the rounded accumulation; the fused one gives %v)", side.name, side.got, want, fused)
		}
	}
}
