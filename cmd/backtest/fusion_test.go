package main

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// A golden journal only guards against fused multiply-add if its own numbers
// are sensitive to it. Go permits fusing `x + a*b` into one operation and
// arm64 does while amd64 does not, so an accumulator over Units — the
// weighted entry price, the weighted exit price, the aggregate open risk —
// can hold a different value on the two machines. The fixture's quantities
// and prices are chosen so that it does: four Units, whose products need
// more than 53 bits.
//
// These tests hold that sensitivity in place. Without them a later edit to
// the fixture could quietly make every product exact, the golden would pass
// on both architectures whatever the arithmetic did, and the next fusion
// defect would be invisible again.

// fusedSum accumulates quantity x price the way an arm64 build would if the
// product were not rounded first: one operation, one rounding, the
// full-precision product kept.
func fusedSum(quantity float64, prices []float64) float64 {
	var sum float64
	for _, price := range prices {
		sum = math.FMA(quantity, price, sum)
	}
	return sum
}

// roundedSum is the same accumulation with each product rounded to float64
// before it is added, which is what sizing.Product guarantees.
func roundedSum(quantity float64, prices []float64) float64 {
	var sum float64
	for _, price := range prices {
		sum += float64(quantity * price)
	}
	return sum
}

// goldenFills returns the quantity every Unit of the fixture's Campaign was
// filled in, the prices it entered at, and the prices it was closed at.
func goldenFills(t *testing.T) (quantity float64, entries, exits []float64) {
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
		var fill event.FillPayload
		if err := json.Unmarshal(record.Envelope.Payload, &fill); err != nil {
			t.Fatalf("decode fill %s: %v", record.Envelope.ID, err)
		}
		if quantity == 0 {
			quantity = float64(fill.Quantity)
		}
		if float64(fill.Quantity) != quantity {
			t.Fatalf("fill %s is for %d shares, the first was for %v: these tests assume one Unit size", record.Envelope.ID, fill.Quantity, quantity)
		}
		switch fill.Kind {
		case event.FillKindEntry, event.FillKindAdd:
			entries = append(entries, fill.Price)
		default:
			exits = append(exits, fill.Price)
		}
	}
	return quantity, entries, exits
}

// TestTheGoldenFixtureIsSensitiveToFusedMultiplyAdd: on both sides of the
// Campaign, accumulating the fixture's own fills with the product fused
// gives a different answer from accumulating it with the product rounded.
// That difference is what the golden journal detects.
func TestTheGoldenFixtureIsSensitiveToFusedMultiplyAdd(t *testing.T) {
	quantity, entries, exits := goldenFills(t)

	if len(entries) < 2 || len(exits) < 2 {
		t.Fatalf("the fixture fills %d Unit(s) in and %d out; an accumulator needs at least two terms to diverge", len(entries), len(exits))
	}

	for _, side := range []struct {
		name   string
		prices []float64
	}{
		{"the weighted entry price", entries},
		{"the weighted exit price", exits},
	} {
		rounded, fused := roundedSum(quantity, side.prices), fusedSum(quantity, side.prices)
		if rounded == fused {
			t.Errorf("%s accumulates to %v either way: the fixture's prices no longer tell a fused build from an unfused one, and the golden journal no longer guards against one",
				side.name, rounded)
		}
	}
}

// TestTheRecordedCampaignUsesTheRoundedAccumulation is the other half: the
// fixture is sensitive, and what the journal actually recorded is the
// rounded answer rather than the fused one.
func TestTheRecordedCampaignUsesTheRoundedAccumulation(t *testing.T) {
	quantity, entries, exits := goldenFills(t)

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

	total := quantity * float64(len(entries))
	for _, side := range []struct {
		name   string
		prices []float64
		got    float64
	}{
		{"entry price", entries, exited.EntryPrice},
		{"exit price", exits, exited.ExitPrice},
	} {
		want := roundedSum(quantity, side.prices) / total
		fused := fusedSum(quantity, side.prices) / total
		if side.got != want {
			t.Errorf("recorded %s = %v, want %v (the rounded accumulation; the fused one gives %v)", side.name, side.got, want, fused)
		}
	}
}
