package event

import (
	"math"
	"math/big"
	"strconv"
)

// DefaultRoundingSlack covers up to ten partial executions at one cent each
// (ADR 0026, "Per-execution rounding reserve"). It is an execution allowance,
// separate from the structural cash buffer awaiting the owner's decision.
const DefaultRoundingSlack = 0.10

// ValidRoundingSlack enforces ADR 0026's inclusive dollar bounds.
func ValidRoundingSlack(slack float64) bool {
	return !math.IsNaN(slack) && slack >= 0.01 && slack <= 1
}

// RoundingSlack resolves the D7-F execution allowance (ADR 0026). Earlier
// whole-share configurations retain their original cash rule and encoding.
func (c ExecutionConfiguration) RoundingSlack() float64 {
	if c.ShareQuantity != ShareQuantityFractional {
		return 0
	}
	if c.RoundingSlackPerOrder == nil {
		return DefaultRoundingSlack
	}
	return *c.RoundingSlackPerOrder
}

// BuyReservation adds the partial-execution allowance to the order's cent
// ceiling and fees (ADR 0026, "Per-execution rounding reserve"). Fill debits
// use BuyCost instead: slack is reserved once per order, never per fill.
// All inputs must be validated; slack may be zero for legacy orders.
func BuyReservation(quantity, scale int64, price, fees, slack float64) *big.Rat {
	return new(big.Rat).Add(BuyCost(quantity, scale, price, fees), decimalMoney(slack))
}

// BuyCost returns the exact worst-case debit: quantity times the decimal
// limit rounded up to a cent, plus fees (ADR 0026, point 2, "Worst-case cost").
// Callers supply validated nonnegative quantities, prices and fees; no
// intermediate float may discard a fraction of a cent before rounding.
func BuyCost(quantity, scale int64, price, fees float64) *big.Rat {
	return new(big.Rat).Add(quantityCents(quantity, scale, price, true), decimalMoney(fees))
}

// SaleProceeds credits quantity times price rounded down to cents before
// commission (ADR 0026, point 2, D5): an IRA never funds buys from a rounded-up
// sale. Inputs must be validated nonnegative quantities, prices and commission.
func SaleProceeds(quantity, scale int64, price, commission float64) float64 {
	proceeds := new(big.Rat).Sub(quantityCents(quantity, scale, price, false), decimalMoney(commission))
	value, _ := proceeds.Float64()
	return value
}

// quantityCents rounds an exact decimal execution value in the conservative
// direction required by ADR 0026's no-borrowing invariant, not reconciliation's
// nearest-cent convention (ADR 0019). Inputs must be validated and nonnegative.
func quantityCents(quantity, scale int64, price float64, up bool) *big.Rat {
	value := new(big.Rat).Mul(decimalMoney(price), new(big.Rat).SetFrac64(quantity, QuantityUnits(scale)))
	value.Mul(value, big.NewRat(100, 1))
	cents, remainder := new(big.Int), new(big.Int)
	cents.QuoRem(value.Num(), value.Denom(), remainder)
	if up && remainder.Sign() > 0 {
		cents.Add(cents, big.NewInt(1))
	}
	return new(big.Rat).SetFrac(cents, big.NewInt(100))
}

// decimalMoney preserves a validated monetary input's decimal meaning for
// ADR 0026 capital arithmetic; it must not receive NaN or infinity.
func decimalMoney(value float64) *big.Rat {
	result, _ := new(big.Rat).SetString(strconv.FormatFloat(value, 'g', -1, 64))
	return result
}
