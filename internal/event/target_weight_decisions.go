package event

import (
	"errors"
	"fmt"
	"time"
)

// TargetsDecidedEventType identifies a target-weight strategy's decision for
// one account at one decision time (ADR 0026, point 1). It decides targets
// only; it never names an order.
const TargetsDecidedEventType = "strategy.targets.decided"

// TargetsDecidedSchemaVersion is the current schema version of TargetsDecidedPayload.
const TargetsDecidedSchemaVersion uint32 = 1

// The two outcomes of a target decision (ADR 0026, point 1).
const (
	TargetsOutcomeDecided  = "decided"
	TargetsOutcomeWithheld = "withheld"
)

// The closed set of reasons a decision is withheld (ADR 0026, points 1 and 5).
const (
	// WithheldMissingBars: a session closed without the instrument's bar.
	// Boost never computes RSI across a gap (plan.md section 2, "Boost").
	WithheldMissingBars = "missing-bars"
	// WithheldStaleAccountSnapshot: no account snapshot as of the session's
	// close, so nothing can be valued at the previous close (ADR 0020).
	WithheldStaleAccountSnapshot = "stale-account-snapshot"
	// WithheldMissingUniverse: no market.universe.snapshot for the decision
	// time, so momentum cannot rank (ADR 0026, point 1, "Universe").
	WithheldMissingUniverse = "missing-universe"
	// WithheldInsufficientTrendHistory: fewer than 200 SPY closes, which the
	// backtest treats as risk-on and live withholds (ADR 0026, point 5).
	WithheldInsufficientTrendHistory = "insufficient-trend-history"
	// WithheldNoEligibleNames: the ranking left no name to hold.
	WithheldNoEligibleNames = "no-eligible-names"
)

// TargetWeight is one instrument's target as a fraction of net liquidation.
type TargetWeight struct {
	InstrumentID string  `json:"instrument_id"`
	Weight       float64 `json:"weight"`
}

// BoostFigures are the figures behind a Boost decision, so the journal alone
// can check it (ADR 0026, point 1): the adjusted close, RSI(2), both means,
// the latched boost state before and after (template.py:89-94), and the
// target in force before this decision (template.py's last_want, unset before
// the first decision, template.py:97-100). An indicator not ready carries 0.
type BoostFigures struct {
	InstrumentID     string  `json:"instrument_id"`
	Close            float64 `json:"close"`
	RSI              float64 `json:"rsi"`
	RSIReady         bool    `json:"rsi_ready"`
	TrendMean        float64 `json:"trend_mean"`
	TrendMeanReady   bool    `json:"trend_mean_ready"`
	ExitMean         float64 `json:"exit_mean"`
	ExitMeanReady    bool    `json:"exit_mean_ready"`
	LatchedBefore    bool    `json:"latched_before"`
	LatchedAfter     bool    `json:"latched_after"`
	TargetInForce    float64 `json:"target_in_force"`
	TargetInForceSet bool    `json:"target_in_force_set"`
}

// TargetsDecidedPayload is one account's target decision.
//
// A withheld decision carries no targets, but it still carries the Boost
// figures: the latched state advances with every bar (template.py:89-94), and
// a restart recovers it from the last decision of either outcome (ADR 0026,
// point 5).
type TargetsDecidedPayload struct {
	AccountID  string `json:"account_id"`
	StrategyID string `json:"strategy_id"`
	// DecisionTime is when the decision was taken; SessionPeriodEnd is the
	// session whose closes it used. A decision never precedes its session.
	DecisionTime     time.Time `json:"decision_time"`
	SessionPeriodEnd time.Time `json:"session_period_end"`
	Outcome          string    `json:"outcome"`
	WithheldReason   string    `json:"withheld_reason,omitempty"`
	Detail           string    `json:"detail,omitempty"`
	// Targets is sorted strictly ascending by instrument.
	Targets []TargetWeight `json:"targets"`
	Boost   BoostFigures   `json:"boost"`
}

// Validate checks the identity, the outcome and its reason, the targets and
// the Boost figures.
func (p TargetsDecidedPayload) Validate() error {
	var errs []error
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	if p.StrategyID != TargetWeightStrategyBoost {
		errs = append(errs, fmt.Errorf("strategy id %q is not a target-weight strategy this schema records", p.StrategyID))
	}
	decisionErrs := requiredTime("decision time", p.DecisionTime)
	sessionErrs := requiredTime("session period end", p.SessionPeriodEnd)
	errs = append(errs, decisionErrs...)
	errs = append(errs, sessionErrs...)
	if len(decisionErrs) == 0 && len(sessionErrs) == 0 && p.DecisionTime.Before(p.SessionPeriodEnd) {
		errs = append(errs, errors.New("decision time is before the session whose closes it used"))
	}
	switch p.Outcome {
	case TargetsOutcomeDecided:
		if p.WithheldReason != "" {
			errs = append(errs, errors.New("a decided outcome carries no withheld reason"))
		}
		if len(p.Targets) == 0 {
			errs = append(errs, errors.New("a decided outcome carries at least one target"))
		}
		if !(p.Boost.Close > 0) {
			errs = append(errs, errors.New("a decided Boost outcome carries a positive close"))
		}
	case TargetsOutcomeWithheld:
		switch p.WithheldReason {
		case WithheldMissingBars, WithheldStaleAccountSnapshot, WithheldMissingUniverse,
			WithheldInsufficientTrendHistory, WithheldNoEligibleNames:
		default:
			errs = append(errs, fmt.Errorf("withheld reason %q is not recognised", p.WithheldReason))
		}
		if p.Detail == "" {
			errs = append(errs, errors.New("a withheld outcome needs its detail"))
		}
		if len(p.Targets) != 0 {
			errs = append(errs, errors.New("a withheld outcome carries no targets"))
		}
	default:
		errs = append(errs, fmt.Errorf("outcome %q is not decided or withheld", p.Outcome))
	}
	for i, target := range p.Targets {
		if target.InstrumentID == "" {
			errs = append(errs, fmt.Errorf("target %d: instrument id is required", i+1))
		} else if i > 0 && target.InstrumentID <= p.Targets[i-1].InstrumentID {
			errs = append(errs, fmt.Errorf("targets must be strictly ascending by instrument; %q follows %q", target.InstrumentID, p.Targets[i-1].InstrumentID))
		}
		if !isFinite(target.Weight) || target.Weight < 0 {
			errs = append(errs, fmt.Errorf("target %d: weight must be finite and not negative", i+1))
		}
	}
	errs = append(errs, p.Boost.validate()...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid targets decided payload: %w", err)
	}
	return nil
}

func (b BoostFigures) validate() []error {
	var errs []error
	if b.InstrumentID == "" {
		errs = append(errs, errors.New("boost instrument id is required"))
	}
	if !isFinite(b.Close) || b.Close < 0 {
		errs = append(errs, errors.New("boost close must be finite and not negative"))
	}
	for _, f := range []struct {
		name  string
		value float64
		ready bool
	}{{"rsi", b.RSI, b.RSIReady}, {"trend mean", b.TrendMean, b.TrendMeanReady}, {"exit mean", b.ExitMean, b.ExitMeanReady}} {
		switch {
		case !isFinite(f.value) || f.value < 0:
			errs = append(errs, fmt.Errorf("boost %s must be finite and not negative", f.name))
		case !f.ready && f.value != 0:
			errs = append(errs, fmt.Errorf("boost %s carries a value but is not ready", f.name))
		}
	}
	switch {
	case !isFinite(b.TargetInForce) || b.TargetInForce < 0:
		errs = append(errs, errors.New("boost target in force must be finite and not negative"))
	case !b.TargetInForceSet && b.TargetInForce != 0:
		errs = append(errs, errors.New("boost target in force carries a value but none is set"))
	}
	return errs
}

// OrderPlannedEventType identifies one order the strategy-independent planner
// derived from a target decision (ADR 0026, point 2).
const OrderPlannedEventType = "strategy.order.planned"

// OrderPlannedSchemaVersion is the current schema version of OrderPlannedPayload.
const OrderPlannedSchemaVersion uint32 = 1

// Order sides, which are also the planner's phases (ADR 0026, point 2,
// "Order identity").
const (
	OrderSideBuy  = "buy"
	OrderSideSell = "sell"
)

// OrderTypeLimit is the only order type planned: a limit at the price band's
// edge, marketable but never filled worse than the band (ADR 0026, point 2).
const OrderTypeLimit = "limit"

// OrderPlannedPayload is one planned order and the figures it was sized from:
// net liquidation V, the holding q and the raw reference close p, all as of
// the previous close (ADR 0026, point 2, "Valuation basis").
type OrderPlannedPayload struct {
	AccountID string `json:"account_id"`
	// Tag is the ID of the strategy.targets.decided envelope this order
	// serves; ClientOrderID derives from it, the instrument and the phase.
	Tag           string `json:"tag"`
	ClientOrderID string `json:"client_order_id"`
	InstrumentID  string `json:"instrument_id"`
	Side          string `json:"side"`
	Phase         string `json:"phase"`
	OrderType     string `json:"order_type"`
	// Quantity is in whole raw shares, always positive; Side gives the sign.
	Quantity       int64   `json:"quantity"`
	LimitPrice     float64 `json:"limit_price"`
	TargetWeight   float64 `json:"target_weight"`
	NetLiquidation float64 `json:"net_liquidation"`
	HeldQuantity   int64   `json:"held_quantity"`
	ReferencePrice float64 `json:"reference_price"`
	PriceBand      float64 `json:"price_band"`
}

// Validate checks identity, side, type, quantity and prices, and that the
// limit lies on the side of the reference price its order type requires: a
// buy's limit at or above it, a sell's at or below.
func (p OrderPlannedPayload) Validate() error {
	var errs []error
	errs = append(errs, orderIdentity(p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side, p.Quantity, p.LimitPrice)...)
	if p.Tag == "" {
		errs = append(errs, errors.New("tag is required: an order serves a journalled decision"))
	}
	if p.Phase != OrderSideBuy && p.Phase != OrderSideSell {
		errs = append(errs, fmt.Errorf("phase %q is not buy or sell", p.Phase))
	}
	if p.OrderType != OrderTypeLimit {
		errs = append(errs, fmt.Errorf("order type %q is not %q", p.OrderType, OrderTypeLimit))
	}
	if !isFinite(p.TargetWeight) || p.TargetWeight < 0 {
		errs = append(errs, errors.New("target weight must be finite and not negative"))
	}
	if !isFinite(p.NetLiquidation) || p.NetLiquidation <= 0 {
		errs = append(errs, errors.New("net liquidation must be finite and positive"))
	}
	if p.HeldQuantity < 0 {
		errs = append(errs, errors.New("held quantity must not be negative: the planner never plans against a short"))
	}
	referenceOK := isFinite(p.ReferencePrice) && p.ReferencePrice > 0
	if !referenceOK {
		errs = append(errs, errors.New("reference price must be finite and positive"))
	}
	if !isFinite(p.PriceBand) || p.PriceBand <= 0 || p.PriceBand >= 1 {
		errs = append(errs, errors.New("price band must lie strictly between 0 and 1"))
	}
	if referenceOK && isFinite(p.LimitPrice) {
		if p.Side == OrderSideBuy && p.LimitPrice < p.ReferencePrice {
			errs = append(errs, errors.New("a buy's limit must not be below its reference price"))
		}
		if p.Side == OrderSideSell && p.LimitPrice > p.ReferencePrice {
			errs = append(errs, errors.New("a sell's limit must not be above its reference price"))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid order planned payload: %w", err)
	}
	return nil
}

func orderIdentity(accountID, clientOrderID, instrumentID, side string, quantity int64, limit float64) []error {
	var errs []error
	if accountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	if clientOrderID == "" {
		errs = append(errs, errors.New("client order id is required"))
	}
	if instrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if side != OrderSideBuy && side != OrderSideSell {
		errs = append(errs, fmt.Errorf("side %q is not buy or sell", side))
	}
	if quantity <= 0 {
		errs = append(errs, errors.New("quantity must be positive"))
	}
	if !isFinite(limit) || limit <= 0 {
		errs = append(errs, errors.New("limit price must be finite and positive"))
	}
	return errs
}

// The risk gate's two outcomes for a planned order (ADR 0026, point 3). Only
// a released order, once durably journalled, may be sent.
const (
	OrderReleasedEventType = "risk.order.released"
	OrderRefusedEventType  = "risk.order.refused"
)

// OrderReleasedSchemaVersion and OrderRefusedSchemaVersion are the current
// schema versions of OrderReleasedPayload and OrderRefusedPayload.
const (
	OrderReleasedSchemaVersion uint32 = 1
	OrderRefusedSchemaVersion  uint32 = 1
)

// The closed set of reasons the risk gate refuses an order (ADR 0026, point 3).
const (
	// RefusedUndecidedOrder: the tag names no journalled decision. Halts.
	RefusedUndecidedOrder = "undecided-order"
	// RefusedMarginExposure: an exposure-increasing order would leave the
	// account above its pre-trade limit.
	RefusedMarginExposure = "margin-exposure"
	// RefusedOrderCount: the plan has more orders than the limit allows.
	RefusedOrderCount = "order-count"
	// RefusedOrderSize: the order is a larger share of net liquidation than allowed.
	RefusedOrderSize = "order-size"
	// RefusedPriceBand: the reference price lies outside the band around the mark.
	RefusedPriceBand = "price-band"
	// RefusedMissingMark: no mark recorded for the instrument at or after the
	// session's close, so the band cannot be checked.
	RefusedMissingMark = "missing-mark"
	// RefusedEngineDegraded: the account is Degraded, which allows no buy and
	// freezes the instruments its scope names (ADR 0026, point 4).
	RefusedEngineDegraded = "engine-degraded"
	// RefusedEngineHalted: the account is Halted, which allows no order.
	RefusedEngineHalted = "engine-halted"
)

// RiskFigures are the values the gate checked an order against, so the
// journal alone can check the outcome. Mark is 0 when MarkSet is false.
type RiskFigures struct {
	NetLiquidation   float64 `json:"net_liquidation"`
	ReferencePrice   float64 `json:"reference_price"`
	ExposureBefore   float64 `json:"exposure_before"`
	ExposureAfter    float64 `json:"exposure_after"`
	OrderFraction    float64 `json:"order_fraction"`
	Mark             float64 `json:"mark"`
	MarkSet          bool    `json:"mark_set"`
	MarkDeviation    float64 `json:"mark_deviation"`
	OrdersInDecision int     `json:"orders_in_decision"`
}

func (f RiskFigures) validate() []error {
	var errs []error
	if !isFinite(f.NetLiquidation) || f.NetLiquidation <= 0 {
		errs = append(errs, errors.New("figures net liquidation must be finite and positive"))
	}
	if !isFinite(f.ReferencePrice) || f.ReferencePrice <= 0 {
		errs = append(errs, errors.New("figures reference price must be finite and positive"))
	}
	for _, v := range []struct {
		name  string
		value float64
	}{{"exposure before", f.ExposureBefore}, {"exposure after", f.ExposureAfter}, {"order fraction", f.OrderFraction}, {"mark deviation", f.MarkDeviation}} {
		if !isFinite(v.value) || v.value < 0 {
			errs = append(errs, fmt.Errorf("figures %s must be finite and not negative", v.name))
		}
	}
	switch {
	case !isFinite(f.Mark) || f.Mark < 0:
		errs = append(errs, errors.New("figures mark must be finite and not negative"))
	case f.MarkSet && f.Mark == 0:
		errs = append(errs, errors.New("figures mark must be positive when set"))
	case !f.MarkSet && (f.Mark != 0 || f.MarkDeviation != 0):
		errs = append(errs, errors.New("figures mark carries a value but none is set"))
	}
	if f.OrdersInDecision < 0 {
		errs = append(errs, errors.New("figures orders in decision must not be negative"))
	}
	return errs
}

// OrderReleasedPayload is a planned order the gate passed. Send is false in
// dry-run: the order is journalled and never sent (ADR 0026, point 3, "Mode").
type OrderReleasedPayload struct {
	AccountID     string      `json:"account_id"`
	PlanID        string      `json:"plan_id"`
	Tag           string      `json:"tag"`
	ClientOrderID string      `json:"client_order_id"`
	InstrumentID  string      `json:"instrument_id"`
	Side          string      `json:"side"`
	Quantity      int64       `json:"quantity"`
	LimitPrice    float64     `json:"limit_price"`
	Mode          string      `json:"mode"`
	Send          bool        `json:"send"`
	Figures       RiskFigures `json:"figures"`
}

// Validate checks identity, mode, the dry-run rule and the figures.
func (p OrderReleasedPayload) Validate() error {
	var errs []error
	errs = append(errs, orderIdentity(p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side, p.Quantity, p.LimitPrice)...)
	if p.PlanID == "" {
		errs = append(errs, errors.New("plan id is required"))
	}
	if p.Tag == "" {
		errs = append(errs, errors.New("tag is required: no order is released without a journalled decision"))
	}
	if !validTradingMode(p.Mode) {
		errs = append(errs, fmt.Errorf("mode %q is not dry-run, paper or live", p.Mode))
	}
	if p.Mode == TradingModeDryRun && p.Send {
		errs = append(errs, errors.New("a dry-run release is never sent"))
	}
	errs = append(errs, p.Figures.validate()...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid order released payload: %w", err)
	}
	return nil
}

// OrderRefusedPayload is a planned order the gate refused, with its reason
// from the closed set and the figures behind it. Tag may be empty: an
// untagged order is exactly what RefusedUndecidedOrder refuses.
type OrderRefusedPayload struct {
	AccountID     string      `json:"account_id"`
	PlanID        string      `json:"plan_id"`
	Tag           string      `json:"tag"`
	ClientOrderID string      `json:"client_order_id"`
	InstrumentID  string      `json:"instrument_id"`
	Side          string      `json:"side"`
	Quantity      int64       `json:"quantity"`
	LimitPrice    float64     `json:"limit_price"`
	Reason        string      `json:"reason"`
	Detail        string      `json:"detail"`
	Figures       RiskFigures `json:"figures"`
}

// Validate checks identity, the reason, the detail and the figures.
func (p OrderRefusedPayload) Validate() error {
	var errs []error
	errs = append(errs, orderIdentity(p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side, p.Quantity, p.LimitPrice)...)
	if p.PlanID == "" {
		errs = append(errs, errors.New("plan id is required"))
	}
	switch p.Reason {
	case RefusedUndecidedOrder, RefusedMarginExposure, RefusedOrderCount, RefusedOrderSize,
		RefusedPriceBand, RefusedMissingMark, RefusedEngineDegraded, RefusedEngineHalted:
	default:
		errs = append(errs, fmt.Errorf("refusal reason %q is not recognised", p.Reason))
	}
	if p.Detail == "" {
		errs = append(errs, errors.New("detail is required: a refusal without its figures cannot be checked"))
	}
	errs = append(errs, p.Figures.validate()...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid order refused payload: %w", err)
	}
	return nil
}
