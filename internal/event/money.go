package event

import (
	"errors"
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
// Whole-share orders preserve their original float expression and ignore
// fractional slack (ADR 0026). Invalid inputs or non-finite debits return an error.
func BuyReservation(quantity, scale int64, price, fees, slack float64) (*big.Rat, error) {
	if !isFinite(slack) || slack < 0 {
		return nil, errors.New("buy reservation: rounding slack must be finite and nonnegative")
	}
	cost, err := BuyCost(quantity, scale, price, fees)
	if err != nil {
		return nil, err
	}
	if scale != 0 {
		cost.Add(cost, decimalMoney(slack))
	}
	return finiteBuyMoney(cost)
}

// BuyCost returns the exact worst-case debit: quantity times the decimal
// limit rounded up to a cent, plus fees (ADR 0026, point 2, "Worst-case cost").
// Whole-share orders retain QuantityValue + fees in its original float
// order (ADR 0026). Invalid inputs and debits outside the finite monetary
// range return an error; callers must refuse rather than commit cash.
func BuyCost(quantity, scale int64, price, fees float64) (*big.Rat, error) {
	if err := quantityScaleError(scale); err != nil {
		return nil, err
	}
	if quantity < 0 || !isFinite(price) || price < 0 || !isFinite(fees) || fees < 0 {
		return nil, errors.New("buy cost: quantity, price and fees must be finite and nonnegative")
	}
	if scale == 0 {
		cost := new(big.Rat).SetFloat64(QuantityValue(quantity, 0, price) + fees)
		if cost == nil {
			return nil, errors.New("buy cost: debit exceeds the finite monetary range")
		}
		return cost, nil
	}
	return finiteBuyMoney(new(big.Rat).Add(quantityCents(quantity, scale, price, true), decimalMoney(fees)))
}

// finiteBuyMoney keeps ADR 0026's exact cent arithmetic within the account's
// finite float monetary domain, including fees and the execution reserve.
func finiteBuyMoney(cost *big.Rat) (*big.Rat, error) {
	value, _ := cost.Float64()
	if !isFinite(value) {
		return nil, errors.New("buy cost: debit exceeds the finite monetary range")
	}
	return cost, nil
}

// SaleProceeds credits quantity times price rounded down to cents before
// commission (ADR 0026, point 2, D5): an IRA never funds buys from a rounded-up
// sale. Whole-share fills preserve their unrounded float proceeds (ADR 0026).
// Inputs must be validated nonnegative quantities, prices and commission.
func SaleProceeds(quantity, scale int64, price, commission float64) float64 {
	if scale == 0 {
		return QuantityValue(quantity, 0, price) - commission
	}
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
