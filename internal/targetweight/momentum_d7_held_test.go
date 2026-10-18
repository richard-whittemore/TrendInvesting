package targetweight_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func nameOf(t *testing.T, d event.TargetsDecidedPayload, id string) event.MomentumName {
	t.Helper()
	for _, n := range d.Momentum.Names {
		if n.InstrumentID == id {
			return n
		}
	}
	t.Fatalf("no name %s", id)
	return event.MomentumName{}
}

// TestMinimumLotKeepsAHeldNameOneLotShort holds 10 AAA, January's first
// pick, with its raw close about $14,000 a share against a target of a few
// thousand dollars. AAA is held, so D7-L does not skip it: it keeps its
// place, its sector slot (BBB stays capped behind it in Tech) and its
// weight, is recorded held-one-lot-short with its one-share price, target
// value and the holding it keeps (quantity, value at the raw close and
// share of net liquidation), and gets no order, neither a buy nor a sell
// (ADR 0026, point 2, "D7-L", amended 2026-10-18). HHH, held but outside
// the book, has a zero target and is still sold in full.
func TestMinimumLotKeepsAHeldNameOneLotShort(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{rawScale: dearAAA, held: map[string]int64{"AAA": 10, "HHH": 10}, stopAfter: "2025-02-03"})
	jan := targets(t, r.decisions)[0]
	if jan.Outcome != event.TargetsOutcomeDecided {
		t.Fatalf("January: %s %s", jan.Outcome, jan.WithheldReason)
	}
	if err := jan.ValidateWritten(); err != nil {
		t.Errorf("January's record does not validate: %v", err)
	}
	if got := targetIDs(jan); !equalStrings(got, []string{"AAA", "CCC"}) {
		t.Fatalf("January targets %v", got)
	}
	var bar event.CompletedBarPayload
	decode(t, inputOf(t, r, "bar:AAA:2025-01-31"), &bar)
	var snap event.AccountCashSnapshotPayload
	decode(t, inputOf(t, r, "snapshot:2025-01-31"), &snap)
	aaa := nameOf(t, jan, "AAA")
	value := float64(10 * bar.Raw.Close)
	if aaa.Status != event.MomentumStatusHeldOneLotShort || aaa.OneShare != bar.Raw.Close || !(aaa.Weight > 0) ||
		aaa.TargetValue != float64(float64(aaa.Weight*jan.Momentum.MinimumLot.MomentumScale)*snap.NetLiquidation) ||
		aaa.HeldQuantity != 10 || aaa.HeldValue != value || aaa.HeldShare != value/snap.NetLiquidation {
		t.Errorf("AAA recorded %+v", aaa)
	}
	if bbb := nameOf(t, jan, "BBB"); bbb.Status != event.MomentumStatusSectorCapped {
		t.Errorf("BBB recorded %+v", bbb)
	}
	for _, want := range []string{
		fmt.Sprintf("AAA holds 10 shares worth %.2f", value),
		fmt.Sprintf("of net liquidation %.2f", snap.NetLiquidation),
		fmt.Sprintf("one share %.2f", bar.Raw.Close),
		"no order",
	} {
		if !strings.Contains(jan.Detail, want) {
			t.Errorf("detail %q does not name %q", jan.Detail, want)
		}
	}
	var orders []string
	for _, o := range planned(t, r.decisions) {
		orders = append(orders, fmt.Sprintf("%s %s %d", o.Side, o.InstrumentID, o.Quantity))
		if o.InstrumentID == "AAA" {
			t.Errorf("AAA planned: %+v", o)
		}
	}
	if !containsPrefix(orders, "sell HHH 10") || !containsPrefix(orders, "buy CCC") {
		t.Errorf("orders %v, want HHH sold in full and CCC bought", orders)
	}
	for _, e := range ofType(r.decisions, event.OrderSkippedEventType) {
		var p event.OrderSkippedPayload
		decode(t, e, &p)
		if p.InstrumentID == "AAA" {
			t.Errorf("AAA recorded skipped: %+v", p)
		}
	}
}

func containsPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// TestMinimumLotWithholdsAnAllUnaffordableBookThoughOneIsHeld prices every
// candidate above its target with AAA held: AAA is kept one lot short, but
// no pick is affordable, so the decision is withheld as all-unaffordable and
// nothing is ordered; HHH, held outside the book, is kept too, since a bad
// valuation would look the same (ADR 0026, point 5, "fail closed").
func TestMinimumLotWithholdsAnAllUnaffordableBookThoughOneIsHeld(t *testing.T) {
	dear := map[string]float64{"AAA": 200, "BBB": 200, "CCC": 200, "FFF": 2000, "GGG": 2000, "HHH": 2000}
	r := simulateMomentum(t, momentumOptions{rawScale: dear, held: map[string]int64{"AAA": 10, "HHH": 10}, stopAfter: "2025-02-03"})
	jan := targets(t, r.decisions)[0]
	if jan.Outcome != event.TargetsOutcomeWithheld || jan.WithheldReason != event.WithheldAllUnaffordable {
		t.Fatalf("January: %s %s", jan.Outcome, jan.WithheldReason)
	}
	if err := jan.ValidateWritten(); err != nil {
		t.Errorf("January's record does not validate: %v", err)
	}
	if aaa := nameOf(t, jan, "AAA"); aaa.Status != event.MomentumStatusHeldOneLotShort {
		t.Errorf("AAA recorded %+v", aaa)
	}
	if orders := planned(t, r.decisions); len(orders) != 0 {
		t.Errorf("orders %+v, want none", orders)
	}
}
