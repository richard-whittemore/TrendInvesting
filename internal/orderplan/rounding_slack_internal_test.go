package orderplan

import (
	"math/big"
	"testing"
)

// ADR 0026 point 2, Scaling: fees are reserved first, and surplus cash
// must never enlarge a requested basket beyond its original quantities.
func TestFractionalFeeScaleCapsSurplusCash(t *testing.T) {
	cash, fees, atLimit := big.NewRat(125, 1), big.NewRat(5, 1), big.NewRat(100, 1)
	got := fractionalFeeScale(cash, fees, atLimit)
	if got.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("fee scale = %s; want 1", got)
	}
	if cash.Cmp(big.NewRat(125, 1)) != 0 || fees.Cmp(big.NewRat(5, 1)) != 0 || atLimit.Cmp(big.NewRat(100, 1)) != 0 {
		t.Fatal("fee scaling mutated its monetary inputs")
	}
}
