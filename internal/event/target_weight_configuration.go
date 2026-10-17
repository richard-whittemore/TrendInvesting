package event

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"
)

// TargetWeightConfigurationEventType identifies the configuration that opens
// every target-weight run (ADR 0026, point 6). It is a separate type from
// strategy.configuration, which is the Turtle reducer's: the two strategies
// share no rule and no state (AGENTS.md rule 4).
const TargetWeightConfigurationEventType = "strategy.target-weight.configuration"

// TargetWeightConfigurationSchemaVersion is the current schema version of
// TargetWeightConfigurationPayload.
//
//   - Version 1 held Boost's strategy fields, the margin account's limits and
//     the account's identity only.
//   - Version 2 added the momentum strategy for an IRA: Momentum (its rule's
//     parameters, the universe data source and the sector-label sources) and
//     Execution (the D5 cash rule, the share quantity rule, the fee
//     allowance). StrategyID chooses which strategy section is set; the other
//     is zero.
//   - Version 3 adds the margin-cushion alarm's Excess Liquidity thresholds
//     to the limits (ADR 0026, point 6): required for Boost's margin account,
//     absent for an IRA, which has no margin cushion. The glide path joins in
//     a later version, a new hash (ADR 0016).
const TargetWeightConfigurationSchemaVersion uint32 = 3

// The strategies this schema configures: Boost on SPY for the joint margin
// account, and the momentum combination for an IRA (docs/live-trading/plan.md,
// section 2, "Boost" and "Momentum, combination").
const (
	TargetWeightStrategyBoost    = "boost"
	TargetWeightStrategyMomentum = "momentum"
)

// MomentumRuleTrend names the momentum variant configured: mom_trend, the
// variant F_COMBO selects (research/edge-search/build_variants.py:152-153).
const MomentumRuleTrend = "mom_trend"

// The D5 cash rules: which cash an IRA's buys are funded from (ADR 0026,
// point 2, "Budget"; plan.md section 5, "IRA execution"). Whether a buy may
// use unsettled sale proceeds is IBKR's answer to D5, so it is configuration.
const (
	// CashRuleSettled funds buys from settled cash only; the proceeds of the
	// rebalance's own sells do not count until they settle.
	CashRuleSettled = "settled-cash"
	// CashRuleTotal funds buys from total cash, settled and unsettled, plus
	// the proceeds the rebalance's sells realise.
	CashRuleTotal = "total-cash"
)

// ShareQuantityWhole is the only share quantity rule built: whole shares,
// rounded toward zero (ADR 0026, point 2, D7-L's rounding). Fractional
// shares (D7-F) need decimal quantities in the order and fill payloads.
const ShareQuantityWhole = "whole-shares"

// The trading modes, set per account (docs/live-trading/plan.md, section 5,
// "Modes"). A dry-run releases orders with send false and never sends them
// (ADR 0026, point 3).
const (
	TradingModeDryRun = "dry-run"
	TradingModePaper  = "paper"
	TradingModeLive   = "live"
)

// AdapterIBKR names the live broker adapter (ADR 0025, point 3).
const AdapterIBKR = "ibkr"

// warmUpLayout is the date layout of BoostConfiguration.WarmUpStart.
const warmUpLayout = "2006-01-02"

// TargetWeightConfigurationPayload is one account's target-weight
// configuration (ADR 0026, point 6). Any change is a new hash and a new run.
type TargetWeightConfigurationPayload struct {
	StrategyID string `json:"strategy_id"`
	AccountID  string `json:"account_id"`
	Mode       string `json:"mode"`
	Adapter    string `json:"adapter"`
	// Boost holds the rule's parameters (research/edge-search/boost/template.py:33-37),
	// set only for StrategyID boost.
	Boost BoostConfiguration `json:"boost,omitzero"`
	// Momentum holds the momentum rule's parameters, set only for StrategyID
	// momentum.
	Momentum MomentumConfiguration `json:"momentum,omitzero"`
	// Execution holds the IRA execution rules, set only for StrategyID
	// momentum.
	Execution ExecutionConfiguration `json:"execution,omitzero"`
	// Limits holds the account's hard limits (ADR 0026, point 3).
	Limits MarginLimits `json:"limits"`
}

// MomentumConfiguration is the momentum combination's parameters as
// backtested: research/edge-search/momentum/defensive_v2.py as F_COMBO
// configures it (ADR 0026, point 6, "Strategy"; plan.md section 2), and the
// identity of the universe's inputs (ADR 0026, point 6, "Identity").
type MomentumConfiguration struct {
	// Rule is MomentumRuleTrend.
	Rule string `json:"rule"`
	// ListingType is the listing type a universe row must carry, "CS"
	// (plan.md section 4, D8 "As built").
	ListingType string `json:"listing_type"`
	// UniverseSize is how many rows by dollar volume join the universe (500).
	UniverseSize int `json:"universe_size"`
	// MinPrice is the unadjusted close a row needs, inclusive ($5).
	MinPrice float64 `json:"min_price"`
	// ScoreLookback (252) and ScoreSkip (21) place the 12-1 score's two
	// closes, in sessions back from the last (defensive_v2.py:287-289).
	ScoreLookback int `json:"score_lookback"`
	ScoreSkip     int `json:"score_skip"`
	// VolatilityReturns is how many daily returns the volatility measures (252).
	VolatilityReturns int `json:"volatility_returns"`
	TopN              int `json:"top_n"`
	SectorCap         int `json:"sector_cap"`
	// InverseVolatility is F_COMBO's INV_VOL; only true is built.
	InverseVolatility bool    `json:"inverse_volatility"`
	Invested          float64 `json:"invested"`
	TrendInstrument   string  `json:"trend_instrument"`
	// TrendCloses is TREND_DAYS: the closes the trend mean covers (200).
	TrendCloses       int     `json:"trend_closes"`
	RiskOffInstrument string  `json:"risk_off_instrument"`
	RiskOffFraction   float64 `json:"risk_off_fraction"`
	// RebalanceBand is REBALANCE_BAND: a held name is traded only when its
	// weight is more than RebalanceBand x its target away from it (0.25).
	RebalanceBand float64 `json:"rebalance_band"`
	// UniverseSource names the producer of market.universe.snapshot; each
	// snapshot must name it (ADR 0026, point 8).
	UniverseSource string `json:"universe_source"`
	// SectorSources are the sector-label sources a universe row may name, in
	// order of preference (plan.md section 4, "Sectors", "Chosen": GICS from
	// IWV, then Nasdaq's screener).
	SectorSources []string `json:"sector_sources"`
}

// ExecutionConfiguration is an IRA's execution rules (ADR 0026, points 2 and 6).
type ExecutionConfiguration struct {
	// CashRule is one of the CashRule constants (D5).
	CashRule string `json:"cash_rule"`
	// ShareQuantity is ShareQuantityWhole (D7).
	ShareQuantity string `json:"share_quantity"`
	// MaxFeesPerOrder is the most commission and fees one order can cost,
	// added to every buy's worst-case cost (ADR 0026, point 2, "Worst-case
	// cost").
	MaxFeesPerOrder float64 `json:"max_fees_per_order"`
}

// BoostConfiguration is Boost's parameters as backtested
// (research/edge-search/boost/template.py:33-37; plan.md section 2: SPY,
// BOOST_SIZE 1.5, RSI_ENTRY 10, TREND_DAYS 200, EXIT_SMA 5, 98% invested).
type BoostConfiguration struct {
	InstrumentID     string  `json:"instrument_id"`
	InvestedFraction float64 `json:"invested_fraction"`
	BoostSize        float64 `json:"boost_size"`
	// RSIPeriod is the RSI's length in closes; the research uses RSI(2) with
	// Wilder smoothing (template.py:54).
	RSIPeriod int     `json:"rsi_period"`
	RSIEntry  float64 `json:"rsi_entry"`
	TrendDays int     `json:"trend_days"`
	ExitSMA   int     `json:"exit_sma"`
	// WarmUpStart is the first session whose close feeds the indicators,
	// as YYYY-MM-DD. It is a string, not a time.Time, so the hashed bytes are
	// the date a reader sees (canonicalJSON cannot traverse time.Time). RSI(2)
	// depends on its seed (ADR 0026, point 8), so the start is configuration.
	WarmUpStart string `json:"warm_up_start"`
}

// MarginLimits is an account's hard limits (ADR 0026, point 3): the joint
// margin account's, or an IRA's, whose MaxExposure is at most 1 because an IRA
// never uses margin. Each is set so every order in the parity backtests
// passes; a value that refuses a backtested order is a configuration error.
type MarginLimits struct {
	// MaxExposure is the pre-trade exposure limit, 1.5 for the joint account
	// and 1.0 for an IRA (plan.md section 5). It applies only to orders that
	// increase exposure.
	MaxExposure          float64 `json:"max_exposure"`
	MaxOrdersPerDecision int     `json:"max_orders_per_decision"`
	// MaxOrderFraction bounds |quantity x price| / net liquidation per order.
	MaxOrderFraction float64 `json:"max_order_fraction"`
	// PriceBand is the fraction either side of the recorded mark within which
	// the reference price must lie, and the distance of each limit price from
	// the reference price (ADR 0026, point 2, "Order type and price").
	PriceBand float64 `json:"price_band"`
	// The margin-cushion alarm's thresholds (plan.md section 5; ADR 0026,
	// point 6): Excess Liquidity as a share of net liquidation, a warning
	// below ExcessLiquidityWarning (0.40), urgent below ExcessLiquidityUrgent
	// (0.20), and no reading for ExcessLiquidityStaleMinutes (30) while the
	// market is open counting as urgent. All three are set for a margin
	// account and all three are zero, and omitted, for an IRA.
	ExcessLiquidityWarning      float64 `json:"excess_liquidity_warning,omitzero"`
	ExcessLiquidityUrgent       float64 `json:"excess_liquidity_urgent,omitzero"`
	ExcessLiquidityStaleMinutes int     `json:"excess_liquidity_stale_minutes,omitzero"`
}

// WarmUpDate returns WarmUpStart parsed as a UTC date.
func (c BoostConfiguration) WarmUpDate() (time.Time, error) {
	return time.Parse(warmUpLayout, c.WarmUpStart)
}

// Validate checks every field against its closed set or range.
func (p TargetWeightConfigurationPayload) Validate() error {
	var errs []error
	switch p.StrategyID {
	case TargetWeightStrategyBoost:
		errs = append(errs, p.Boost.validate()...)
		if !p.Limits.HasCushion() {
			errs = append(errs, errors.New("a margin account's limits set the excess liquidity thresholds"))
		}
		if !p.Momentum.isZero() || p.Execution != (ExecutionConfiguration{}) {
			errs = append(errs, errors.New("a boost configuration sets no momentum or execution section"))
		}
	case TargetWeightStrategyMomentum:
		errs = append(errs, p.Momentum.validate()...)
		errs = append(errs, p.Execution.validate()...)
		if p.Boost != (BoostConfiguration{}) {
			errs = append(errs, errors.New("a momentum configuration sets no boost section"))
		}
		if p.Limits.MaxExposure > 1 {
			errs = append(errs, errors.New("an IRA's max exposure is at most 1: it never uses margin"))
		}
		if p.Limits.HasCushion() {
			errs = append(errs, errors.New("an IRA's limits set no excess liquidity thresholds: it has no margin cushion"))
		}
	default:
		errs = append(errs, fmt.Errorf("strategy id %q is not a target-weight strategy this schema configures", p.StrategyID))
	}
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	if !validTradingMode(p.Mode) {
		errs = append(errs, fmt.Errorf("mode %q is not dry-run, paper or live", p.Mode))
	}
	if p.Adapter != AdapterIBKR {
		errs = append(errs, fmt.Errorf("adapter %q is not %q (ADR 0025)", p.Adapter, AdapterIBKR))
	}
	errs = append(errs, p.Limits.validate()...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid target-weight configuration payload: %w", err)
	}
	return nil
}

func (c BoostConfiguration) validate() []error {
	var errs []error
	if c.InstrumentID == "" {
		errs = append(errs, errors.New("boost instrument id is required"))
	}
	if !isFinite(c.InvestedFraction) || c.InvestedFraction <= 0 || c.InvestedFraction > 1 {
		errs = append(errs, errors.New("boost invested fraction must be finite, above 0 and at most 1"))
	}
	if !isFinite(c.BoostSize) || c.BoostSize < 1 {
		errs = append(errs, errors.New("boost size must be finite and at least 1"))
	}
	if c.RSIPeriod < 1 {
		errs = append(errs, errors.New("boost rsi period must be at least 1"))
	}
	if !isFinite(c.RSIEntry) || c.RSIEntry <= 0 || c.RSIEntry >= 100 {
		errs = append(errs, errors.New("boost rsi entry must lie strictly between 0 and 100"))
	}
	if c.TrendDays < 1 {
		errs = append(errs, errors.New("boost trend days must be at least 1"))
	}
	if c.ExitSMA < 1 {
		errs = append(errs, errors.New("boost exit sma must be at least 1"))
	}
	if _, err := c.WarmUpDate(); err != nil {
		errs = append(errs, fmt.Errorf("boost warm-up start %q is not a YYYY-MM-DD date", c.WarmUpStart))
	}
	return errs
}

func (c MomentumConfiguration) isZero() bool {
	return c.Rule == "" && c.ListingType == "" && c.UniverseSize == 0 && c.MinPrice == 0 &&
		c.ScoreLookback == 0 && c.ScoreSkip == 0 && c.VolatilityReturns == 0 && c.TopN == 0 &&
		c.SectorCap == 0 && !c.InverseVolatility && c.Invested == 0 && c.TrendInstrument == "" &&
		c.TrendCloses == 0 && c.RiskOffInstrument == "" && c.RiskOffFraction == 0 &&
		c.RebalanceBand == 0 && c.UniverseSource == "" && len(c.SectorSources) == 0
}

func (c MomentumConfiguration) validate() []error {
	var errs []error
	if c.Rule != MomentumRuleTrend {
		errs = append(errs, fmt.Errorf("momentum rule %q is not %q", c.Rule, MomentumRuleTrend))
	}
	if c.ListingType == "" {
		errs = append(errs, errors.New("momentum listing type is required"))
	}
	for _, f := range []struct {
		name  string
		value int
	}{{"universe size", c.UniverseSize}, {"score lookback", c.ScoreLookback}, {"top n", c.TopN}, {"sector cap", c.SectorCap}, {"trend closes", c.TrendCloses}} {
		if f.value < 1 {
			errs = append(errs, fmt.Errorf("momentum %s must be at least 1", f.name))
		}
	}
	if c.ScoreSkip < 0 || c.ScoreSkip >= c.ScoreLookback {
		errs = append(errs, errors.New("momentum score skip must lie in [0, score lookback)"))
	}
	if c.VolatilityReturns < 2 || c.VolatilityReturns > c.ScoreLookback {
		errs = append(errs, errors.New("momentum volatility returns must lie in [2, score lookback]"))
	}
	if !isFinite(c.MinPrice) || c.MinPrice < 0 {
		errs = append(errs, errors.New("momentum min price must be finite and not negative"))
	}
	if !c.InverseVolatility {
		errs = append(errs, errors.New("momentum inverse volatility must be true: equal weights are not built"))
	}
	if !isFinite(c.Invested) || c.Invested <= 0 || c.Invested > 1 {
		errs = append(errs, errors.New("momentum invested must be finite, above 0 and at most 1"))
	}
	if c.TrendInstrument == "" || c.RiskOffInstrument == "" || c.TrendInstrument == c.RiskOffInstrument {
		errs = append(errs, errors.New("momentum trend and risk-off instruments are required and distinct"))
	}
	if !isFinite(c.RiskOffFraction) || c.RiskOffFraction < 0 || c.RiskOffFraction > 1 {
		errs = append(errs, errors.New("momentum risk-off fraction must lie in [0, 1]"))
	}
	if !isFinite(c.RebalanceBand) || c.RebalanceBand <= 0 || c.RebalanceBand >= 1 {
		errs = append(errs, errors.New("momentum rebalance band must lie strictly between 0 and 1"))
	}
	if c.UniverseSource == "" {
		errs = append(errs, errors.New("momentum universe source is required"))
	}
	if len(c.SectorSources) == 0 {
		errs = append(errs, errors.New("momentum sector sources are required"))
	}
	for i, s := range c.SectorSources {
		if s == "" || slices.Contains(c.SectorSources[:i], s) {
			errs = append(errs, fmt.Errorf("momentum sector source %d is empty or repeated", i+1))
		}
	}
	return errs
}

func (c ExecutionConfiguration) validate() []error {
	var errs []error
	if c.CashRule != CashRuleSettled && c.CashRule != CashRuleTotal {
		errs = append(errs, fmt.Errorf("execution cash rule %q is not %q or %q", c.CashRule, CashRuleSettled, CashRuleTotal))
	}
	if c.ShareQuantity != ShareQuantityWhole {
		errs = append(errs, fmt.Errorf("execution share quantity %q is not %q, the only rule built", c.ShareQuantity, ShareQuantityWhole))
	}
	if !isFinite(c.MaxFeesPerOrder) || c.MaxFeesPerOrder < 0 {
		errs = append(errs, errors.New("execution max fees per order must be finite and not negative"))
	}
	return errs
}

func (l MarginLimits) validate() []error {
	var errs []error
	if !isFinite(l.MaxExposure) || l.MaxExposure <= 0 {
		errs = append(errs, errors.New("limits max exposure must be finite and positive"))
	}
	if l.MaxOrdersPerDecision < 1 {
		errs = append(errs, errors.New("limits max orders per decision must be at least 1"))
	}
	if !isFinite(l.MaxOrderFraction) || l.MaxOrderFraction <= 0 {
		errs = append(errs, errors.New("limits max order fraction must be finite and positive"))
	}
	if !isFinite(l.PriceBand) || l.PriceBand <= 0 || l.PriceBand >= 1 {
		errs = append(errs, errors.New("limits price band must lie strictly between 0 and 1"))
	}
	if !l.HasCushion() {
		return errs
	}
	if !isFinite(l.ExcessLiquidityWarning) || !isFinite(l.ExcessLiquidityUrgent) ||
		!(0 < l.ExcessLiquidityUrgent && l.ExcessLiquidityUrgent < l.ExcessLiquidityWarning && l.ExcessLiquidityWarning < 1) {
		errs = append(errs, errors.New("limits excess liquidity thresholds must satisfy 0 < urgent < warning < 1"))
	}
	if l.ExcessLiquidityStaleMinutes < 1 {
		errs = append(errs, errors.New("limits excess liquidity stale minutes must be at least 1"))
	}
	return errs
}

// HasCushion reports whether l sets any of the margin-cushion alarm's
// thresholds. Limits that set one must set all three validly; limits that set
// none have no alarm, as an IRA's (ADR 0026, point 6).
func (l MarginLimits) HasCushion() bool {
	return l.ExcessLiquidityWarning != 0 || l.ExcessLiquidityUrgent != 0 || l.ExcessLiquidityStaleMinutes != 0
}

// Validate checks every limit against its range.
func (l MarginLimits) Validate() error {
	if err := errors.Join(l.validate()...); err != nil {
		return fmt.Errorf("invalid margin limits: %w", err)
	}
	return nil
}

// ValidTradingMode reports whether mode is dry-run, paper or live.
func ValidTradingMode(mode string) bool { return validTradingMode(mode) }

func validTradingMode(mode string) bool {
	switch mode {
	case TradingModeDryRun, TradingModePaper, TradingModeLive:
		return true
	}
	return false
}

// TargetWeightConfigurationHash derives a target-weight run's configuration
// hash by ADR 0016's derivation: SHA-256 over "target-weight-configuration/v"
// + the schema version + "\n" + canonicalJSON(payload), hex encoded and
// prefixed "sha256:". The prefix differs from ConfigurationHash's, so a
// Turtle configuration and a target-weight one can never share a hash.
func TargetWeightConfigurationHash(payload TargetWeightConfigurationPayload) string {
	prefix := fmt.Sprintf("target-weight-configuration/v%d\n", TargetWeightConfigurationSchemaVersion)
	sum := sha256.Sum256(append([]byte(prefix), canonicalJSON(payload)...))
	return "sha256:" + hex.EncodeToString(sum[:])
}
