package event

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
//   - Version 1 holds Boost's strategy fields, the margin account's limits and
//     the account's identity only. Momentum, the glide path, execution (D5,
//     D7) and the Excess Liquidity thresholds join in later versions, each a
//     new hash (ADR 0016).
const TargetWeightConfigurationSchemaVersion uint32 = 1

// TargetWeightStrategyBoost is the only strategy this schema configures:
// Boost on SPY for the joint margin account (docs/live-trading/plan.md,
// section 2, "Boost").
const TargetWeightStrategyBoost = "boost"

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
	// Boost holds the rule's parameters (research/edge-search/boost/template.py:33-37).
	Boost BoostConfiguration `json:"boost"`
	// Limits holds the margin account's hard limits (ADR 0026, point 3).
	Limits MarginLimits `json:"limits"`
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

// MarginLimits is the joint margin account's hard limits (ADR 0026, point 3).
// Each is set so every order in the parity backtests passes; a value that
// refuses a backtested order is a configuration error.
type MarginLimits struct {
	// MaxExposure is the pre-trade exposure limit, 1.5 for the joint account
	// (plan.md section 5). It applies only to orders that increase exposure.
	MaxExposure          float64 `json:"max_exposure"`
	MaxOrdersPerDecision int     `json:"max_orders_per_decision"`
	// MaxOrderFraction bounds |quantity x price| / net liquidation per order.
	MaxOrderFraction float64 `json:"max_order_fraction"`
	// PriceBand is the fraction either side of the recorded mark within which
	// the reference price must lie, and the distance of each limit price from
	// the reference price (ADR 0026, point 2, "Order type and price").
	PriceBand float64 `json:"price_band"`
}

// WarmUpDate returns WarmUpStart parsed as a UTC date.
func (c BoostConfiguration) WarmUpDate() (time.Time, error) {
	return time.Parse(warmUpLayout, c.WarmUpStart)
}

// Validate checks every field against its closed set or range.
func (p TargetWeightConfigurationPayload) Validate() error {
	var errs []error
	if p.StrategyID != TargetWeightStrategyBoost {
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
	errs = append(errs, p.Boost.validate()...)
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
	return errs
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
