package strategy_test

import "github.com/richard-whittemore/TrendInvesting/internal/event"

// uncappedConfigurationPayload is validConfigurationPayload as the declared
// Variant "uncapped" states it: Faith's stop-market entry, with no price cap,
// whose hold is the Unit's cost at its level (ADR 0005 and ADR 0020, as
// amended 2026-09-24).
func uncappedConfigurationPayload() event.ConfigurationPayload {
	cfg := validConfigurationPayload()
	cfg.BuyOrderType = event.OrderTypeStopMarket
	cfg.GapBufferN = 0
	return cfg
}

// wantHold is the hold a buy of quantity at level places under cfg,
// derived here independently of the reducer (ADR 0020, as amended
// 2026-09-24): under a stop-limit, quantity x (level + GapBufferN x n +
// SlippageN x n) x dollars per point plus ADR 0013's commission at that
// price (rate, then floor, then ceiling); under a stop-market, quantity x
// level x dollars per point. Each product is rounded before it is added, in
// the order the reducer adds them, so the two agree bit for bit.
func wantHold(cfg event.ConfigurationPayload, quantity int64, level, n float64) float64 {
	q := float64(quantity)
	if cfg.BuyOrderType == event.OrderTypeStopMarket {
		return q * level * cfg.DollarsPerPoint
	}
	priceCap := level + float64(cfg.GapBufferN*n)
	price := priceCap + float64(cfg.SlippageN*n)
	commission := q * cfg.Commission.PerShare
	if commission < cfg.Commission.MinimumPerOrder {
		commission = cfg.Commission.MinimumPerOrder
	}
	if ceiling := q * price * cfg.DollarsPerPoint * cfg.Commission.MaximumFractionOfTradeValue; commission > ceiling {
		commission = ceiling
	}
	return float64(q*price*cfg.DollarsPerPoint) + commission
}

// wantPriceCap is a stop-limit proposal's price cap under cfg: level +
// GapBufferN x n (ADR 0005, as amended 2026-09-24), or zero for a
// stop-market order, which has none.
func wantPriceCap(cfg event.ConfigurationPayload, level, n float64) float64 {
	if cfg.BuyOrderType == event.OrderTypeStopMarket {
		return 0
	}
	return level + float64(cfg.GapBufferN*n)
}
