package event

import (
	"errors"
	"fmt"

	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// validatePriceCap checks a trade or Add proposal's order type, gap buffer
// and price cap together (ADR 0005, as amended 2026-09-24; CONTEXT.md: "Price
// cap"). A stop-limit's cap must be exactly level + gapBufferN x n, computed
// as sizing.PriceCap computes it, so the producer and this check agree bit
// for bit; a stop-market order carries no cap, so both figures are zero.
// derivable reports whether level and n were themselves finite, so the
// derivation is attempted only from figures already checked.
func validatePriceCap(orderType OrderType, gapBufferN, priceCap, level, n float64, derivable bool) []error {
	errs := validateGapBuffer(orderType, gapBufferN)
	switch orderType {
	case OrderTypeStopLimit:
		switch {
		case !isFinite(priceCap):
			return append(errs, errors.New("price cap must be finite"))
		case priceCap <= 0:
			// A stop-limit's cap is the most it may pay; zero is not a
			// marker for "uncapped", which only a stop-market order is.
			return append(errs, fmt.Errorf("price cap must be positive for a %s order, got %v", OrderTypeStopLimit, priceCap))
		case derivable && priceCap < level:
			return append(errs, fmt.Errorf("price cap %v is below the level %v: a %s order capped there could never fill", priceCap, level, OrderTypeStopLimit))
		}
		if len(errs) == 0 && derivable {
			if derived, ok := sizing.PriceCap(level, gapBufferN, n); !ok || priceCap != derived {
				errs = append(errs, fmt.Errorf(
					"stated price cap %v does not match the derivation %v (level %v + gap buffer %v x n %v; ADR 0005)",
					priceCap, derived, level, gapBufferN, n))
			}
		}
	case OrderTypeStopMarket:
		if priceCap != 0 {
			errs = append(errs, fmt.Errorf("price cap must be zero for a %s order, which has none (ADR 0005), got %v", OrderTypeStopMarket, priceCap))
		}
	}
	return errs
}
