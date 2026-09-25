package sizing

// CommissionSchedule is ADR 0013's commission model: a rate per share, a
// floor per order and a ceiling expressed as a fraction of the order's own
// trade value. It mirrors event.CommissionConfig field for field; this
// package declares its own so the arithmetic stays free of the wire
// contract, as the Sizing Mode does.
type CommissionSchedule struct {
	PerShare                    float64
	MinimumPerOrder             float64
	MaximumFractionOfTradeValue float64
}

// Commission returns ADR 0013's charge on one order of quantity executed at
// price: the rate, then the floor, then the ceiling. The ceiling is a cap on
// the charge, so it overrides the floor. internal/fills charges every fill
// with this function and internal/strategy bounds a hold with it, so the
// charge a fill pays and the charge its hold reserved are one arithmetic
// (ADR 0013, as amended 2026-09-24).
//
// The charge never falls as price rises: the rate and floor do not depend on
// price, and the ceiling grows with it. That is what lets a hold charged at
// the worst-case price bound the charge on any fill below it (ADR 0020, as
// amended).
//
// Callers validate the schedule and operands. The boolean reports whether
// the charge is a finite figure a payload can carry.
func Commission(quantity int64, price, dollarsPerPoint float64, schedule CommissionSchedule) (float64, bool) {
	charge := float64(quantity) * schedule.PerShare
	if charge < schedule.MinimumPerOrder {
		charge = schedule.MinimumPerOrder
	}
	if ceiling := float64(quantity) * price * dollarsPerPoint * schedule.MaximumFractionOfTradeValue; charge > ceiling {
		charge = ceiling
	}
	return charge, isFinite(charge)
}

// PriceCap returns the price cap of a buy resting at level: level +
// gapBufferN x n (ADR 0005, as amended 2026-09-24; CONTEXT.md: "Price cap").
// The product is rounded before the addition (Product), so a producer and a
// validator computing it agree bit for bit on every architecture.
//
// The boolean reports whether the cap is finite.
func PriceCap(level, gapBufferN, n float64) (float64, bool) {
	price := level + Product(gapBufferN, n)
	return price, isFinite(price)
}

// WorstCaseBuyCost is the most a buy of quantity at priceCap can cost once
// it fills: quantity x (priceCap + slippageN x n) x dollarsPerPoint, plus
// the commission charged at that price (ADR 0020, as amended 2026-09-24;
// ADR 0013). ADR 0005's amended fill model never executes a capped buy
// above priceCap before slippage, and adds exactly slippageN x n to it, so
// no fill of the order can cost more than this.
//
// The slippage is rounded before it is added (Product), as internal/fills
// rounds the slippage it adds to a fill, so the per-share price here is the
// same float64 as the highest price a fill can record.
//
// The boolean reports whether the cost is a finite figure.
func WorstCaseBuyCost(quantity int64, priceCap, slippageN, n, dollarsPerPoint float64, schedule CommissionSchedule) (float64, bool) {
	price := priceCap + Product(slippageN, n)
	// Rounded before the addition, as internal/strategy rounds a fill's
	// trade value before adding its commission (ADR 0017).
	trade := float64(float64(quantity) * price * dollarsPerPoint)
	commission, ok := Commission(quantity, price, dollarsPerPoint, schedule)
	cost := trade + commission
	return cost, ok && isFinite(price) && isFinite(trade) && isFinite(cost)
}
