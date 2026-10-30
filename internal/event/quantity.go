package event

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// FractionalQuantityScale counts ten-thousandths of one raw share (ADR 0026,
// D7-F). A zero scale denotes legacy whole shares; no other scale is valid.
const FractionalQuantityScale int64 = 10000

// QuantityUnits returns integer units per raw share (ADR 0026, D7-F).
// Callers validate scale before arithmetic.
func QuantityUnits(scale int64) int64 {
	if scale == 0 {
		return 1
	}
	return scale
}

// ParseQuantity converts a nonnegative decimal with at most four places to
// exact ten-thousandths of a share (ADR 0026, D7-F). It never rounds input.
func ParseQuantity(s string) (int64, error) {
	parts := strings.Split(s, ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, fmt.Errorf("quantity %q is not a decimal", s)
	}
	for _, part := range parts {
		if part == "" {
			return 0, fmt.Errorf("quantity %q has an empty decimal part", s)
		}
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return 0, fmt.Errorf("quantity %q must be a nonnegative decimal", s)
			}
		}
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	if len(fraction) > 4 {
		return 0, fmt.Errorf("quantity %q exceeds four decimal places", s)
	}
	n, err := strconv.ParseInt(parts[0]+fraction+strings.Repeat("0", 4-len(fraction)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("quantity %q overflows fixed-point shares: %w", s, err)
	}
	return n, nil
}

// FractionalIncrementTicks validates D7-F's configured increment: at most one
// share, dividing one share exactly; its default is 0.0001 (ADR 0026, D7-F).
func FractionalIncrementTicks(s string) (int64, error) {
	if s == "" {
		s = "0.0001"
	}
	n, err := ParseQuantity(s)
	if err != nil {
		return 0, err
	}
	if n <= 0 || n > FractionalQuantityScale || FractionalQuantityScale%n != 0 {
		return 0, errors.New("fractional increment must be positive, at most 1, and divide 1 exactly")
	}
	return n, nil
}

// QuantityValue values an exact share count at a decimal price (ADR 0026,
// D7-F). Only the monetary result becomes floating point. Legacy arithmetic
// is preserved byte-for-byte for whole-share configurations (ADR 0016).
func QuantityValue(quantity, scale int64, price float64) float64 {
	if scale == 0 {
		return float64(float64(quantity) * price)
	}
	if math.IsNaN(price) || math.IsInf(price, 0) {
		return math.NaN()
	}
	p, _ := new(big.Rat).SetString(strconv.FormatFloat(price, 'g', -1, 64))
	p.Mul(p, new(big.Rat).SetFrac64(quantity, QuantityUnits(scale)))
	value, _ := p.Float64()
	return value
}

func quantityScaleError(scale int64) error {
	if scale != 0 && scale != FractionalQuantityScale {
		return fmt.Errorf("quantity scale %d is not legacy whole shares (0) or four-place fixed point (10000)", scale)
	}
	return nil
}

func quantitySchema(scale int64, version, fractional uint32) error {
	if err := quantityScaleError(scale); err != nil {
		return err
	}
	if (version == fractional) != (scale == FractionalQuantityScale) {
		return fmt.Errorf("schema version %d and quantity scale %d disagree: fractional quantities require version %d", version, scale, fractional)
	}
	return nil
}

// FormatQuantity displays exact share units without binary floating point
// (ADR 0026, D7-F); callers validate the scale first.
func FormatQuantity(q, scale int64) string {
	if scale == 0 {
		return strconv.FormatInt(q, 10)
	}
	return strings.TrimRight(strings.TrimRight(new(big.Rat).SetFrac64(q, QuantityUnits(scale)).FloatString(4), "0"), ".")
}
