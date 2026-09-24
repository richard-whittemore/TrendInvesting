package main

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// A golden fixture must remain sensitive to fused multiply-add to enforce
// byte-identical replay (ADR 0017; docs/development.md). The Go specification
// permits an implementation to fuse a multiply-add, and permits it not to; it
// guarantees nothing per architecture. What this project observed is narrower
// and is the reason the rule exists: with gc 1.24.4, linux/arm64 fused and
// linux/amd64 did not, and a committed golden that passed on one failed on
// the other. Treat that as a property of a toolchain and target, not of
// arm64, and re-establish it rather than assume it if either changes.
//
// The golden bars, run with fourUnitCash, buy four Units whose
// quantity-price products need more than 53 bits, so the weighted entry
// price, the weighted exit price and the aggregate open risk diverge when a
// product is left fusible; making all products exact would let a journal
// pass even with fusible arithmetic. At the default cash the main golden
// holds one Unit (ADR 0020 debits Unit 1's fill before Unit 2 is checked),
// so these tests run the four-Unit Campaign themselves, and the declared
// Variant's golden, which opens with fourUnitCash, commits one.
//
// That Campaign closes all four Units in one exit fill: every Unit's Exit
// Order rests at the Exit Channel (ADR 0005, as amended), so its exit side
// is a single product with nothing to accumulate and cannot tell a fused
// build from an unfused one. exitSensitiveFills below derives, from the
// golden's own bars, a Campaign that instead closes in the four Units' own
// (distinct) stop fills, so the exit side is checked for accumulation
// sensitivity the same way the entry side is, not merely for recording
// whatever single product resulted.

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

// fillsFrom decodes every event.FillEventType record in written into the
// Campaign's entry fills (entry and add) and its exit fills (stop and
// exit).
func fillsFrom(t *testing.T, written []byte) (entries, exits []fill) {
	t.Helper()

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

// goldenFills returns the fills the golden bar fixture's Campaign entered
// with and the fill it was closed with, run with fourUnitCash so that all
// four Units are bought: at the default cash the Campaign holds one Unit and
// its entry side has nothing to accumulate.
func goldenFills(t *testing.T) (entries, exits []fill) {
	t.Helper()

	written, _ := runBacktestOnBars(t, barsFixture)
	return fillsFrom(t, written)
}

// exitSensitiveFills returns the fills of a Campaign that closes in SEVERAL
// fills at distinct prices, so its exit side has something to accumulate.
// It reuses barsEndingWithAnOutstandingExit (outstanding_proposal_test.go),
// which cuts the golden bars the day after the breakout and lowers that
// day's low: the proposed exit then sits below every Unit's own stop, so
// each Unit's Exit Order stays at its own (different) stop level and the
// four stops close the Campaign together
// (TestAnExitBelowEveryStopLeavesTheStopsToCloseTheCampaign pins that this
// fixture's bar fills four stops and no exit). That is the same shape the
// main golden's entry side already has -- four Units filled at four
// distinct prices -- applied to the exit instead.
func exitSensitiveFills(t *testing.T) (entries, exits []fill) {
	t.Helper()

	written, _ := runBacktestOnBars(t, writeBars(t, barsEndingWithAnOutstandingExit(t)))
	return fillsFrom(t, written)
}

// campaignExited decodes the one event.CampaignExitedEventType record in
// written.
func campaignExited(t *testing.T, written []byte) event.CampaignExitedPayload {
	t.Helper()

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
	return exited
}

// assertSensitive fails if fills' rounded and fused accumulations agree: an
// accumulator needs at least two terms to diverge, and prices chosen so
// that it does not are prices that no longer exercise this guard at all.
func assertSensitive(t *testing.T, side string, fills []fill) {
	t.Helper()

	if len(fills) < 2 {
		t.Fatalf("the fixture fills %d Unit(s) on the %s side; an accumulator needs at least two terms to diverge", len(fills), side)
	}
	if rounded, fused := roundedSum(fills), fusedSum(fills); rounded == fused {
		t.Errorf("the weighted %s price accumulates to %v either way: the fixture's prices no longer tell a fused build from an unfused one, and the golden journal no longer guards against one",
			side, rounded)
	}
}

// TestTheGoldenFixtureIsSensitiveToFusedMultiplyAdd: accumulating a
// Campaign's own fills with the product fused gives a different answer from
// accumulating them with the product rounded, on both sides of the
// Campaign -- goldenFills' entry side (the golden bars' four Units added
// at different prices) and exitSensitiveFills' exit side (four Units
// stopped out at different levels). That difference is what a golden
// journal detects.
func TestTheGoldenFixtureIsSensitiveToFusedMultiplyAdd(t *testing.T) {
	entries, _ := goldenFills(t)
	assertSensitive(t, "entry", entries)

	_, exits := exitSensitiveFills(t)
	assertSensitive(t, "exit", exits)
}

// TestTheRecordedCampaignUsesTheRoundedAccumulation is the other half: each
// fixture is sensitive, and what its journal actually recorded is the
// rounded answer rather than the fused one -- the four-Unit Campaign's
// EntryPrice, and exitSensitiveFills' own Campaign's ExitPrice.
func TestTheRecordedCampaignUsesTheRoundedAccumulation(t *testing.T) {
	entryWritten, _ := runBacktestOnBars(t, barsFixture)
	entries, _ := fillsFrom(t, entryWritten)
	assertRoundedAccumulation(t, "entry price", entries, campaignExited(t, entryWritten).EntryPrice)

	exitWritten, _ := runBacktestOnBars(t, writeBars(t, barsEndingWithAnOutstandingExit(t)))
	_, exits := fillsFrom(t, exitWritten)
	assertRoundedAccumulation(t, "exit price", exits, campaignExited(t, exitWritten).ExitPrice)
}

// assertRoundedAccumulation fails unless got is fills' rounded quantity-
// weighted accumulation rather than its fused one.
func assertRoundedAccumulation(t *testing.T, name string, fills []fill, got float64) {
	t.Helper()

	total := quantityOf(fills)
	want := roundedSum(fills) / total
	fused := fusedSum(fills) / total
	if got != want {
		t.Errorf("recorded %s = %v, want %v (the rounded accumulation; the fused one gives %v)", name, got, want, fused)
	}
}
