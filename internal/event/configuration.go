package event

import (
	"errors"
	"fmt"
	"time"
)

// ConfigurationEventType identifies the configuration payload for the
// Envelope's Type field.
const ConfigurationEventType = "strategy.configuration"

// ConfigurationSchemaVersion is the current schema version of
// ConfigurationPayload, for the Envelope's SchemaVersion field.
//
// Bumped to 2 for #9: TierBDistanceInN was added. Bumped to 3 for #10:
// DollarsPerPoint and RiskAtStopFraction were added. A schema change is
// explicit in this project (docs/development.md), never a silent field
// addition — and here it must be, because both #10 fields decode as the
// float64 zero from an older record: a zero DollarsPerPoint divides by zero
// in sizing, and a zero RiskAtStopFraction would make a fixed-risk-at-stop
// run size every Unit from a risk budget of nothing.
//
// Bumped 3 -> 4 for #18: Commission was added (ADR 0013's
// Interactive-Brokers-style per-share model, the second half of the cost
// model whose first half — SlippageN — this payload has carried since #8). A
// schema-3 record decodes the whole block as zeros, and a
// MaximumFractionOfTradeValue of zero is not a legitimate cap: it would
// charge nothing on every order however large the per-share rate, which is
// the commission-side twin of the zero-slippage run ADR 0013 declares invalid
// by construction. So a schema-3 record is rejected outright rather than
// silently run free of costs (ADR 0015's rule, the same discipline every
// earlier bump in this package applied).
const ConfigurationSchemaVersion uint32 = 4

// SizingMode selects which quantity position size is keyed to (ADR 0003).
type SizingMode string

// The two declared Sizing Modes. Volatility-normalised is the Baseline;
// fixed-risk-at-stop is the Sublime Variant. They are only equivalent at one
// particular Stop Multiple, so switching between them is a declared
// experiment, never an implicit side effect.
const (
	SizingModeVolatilityNormalised SizingMode = "volatility-normalised"
	SizingModeFixedRiskAtStop      SizingMode = "fixed-risk-at-stop"
)

// CommissionConfig carries ADR 0013's commission model: the
// Interactive-Brokers-style per-share schedule the Baseline charges on every
// fill (#18).
//
// Three parameters, not one number, because that is the shape of the
// published schedule the Baseline adopts: a rate per share, a floor per
// order, and a ceiling expressed as a fraction of the order's own trade
// value. The floor is what makes a small order expensive in percentage terms
// (the reason Faith's own "small accounts lose diversification" warning has a
// cost-side twin), and the ceiling is what stops the floor from swallowing a
// tiny order whole.
//
// They are configuration, not constants, for the same reason every other
// number in this payload is: a Variant that runs a different broker's
// schedule is a declared experiment (ADR 0012), never an edit to the code.
// The Baseline's own values are declared by whoever owns the Baseline
// configuration (#50); see internal/fills for the arithmetic that applies
// them, which is where the ordering of floor and ceiling is decided and
// tested.
type CommissionConfig struct {
	// PerShare is the rate charged per share or contract executed. May be
	// zero: a commission-free venue is a legitimate Variant, and unlike
	// slippage (ADR 0013: never zero) nothing in the methodology requires a
	// commission to exist.
	PerShare float64 `json:"per_share"`
	// MinimumPerOrder is the floor charged on any order that executes. May
	// be zero, for the same reason PerShare may.
	MinimumPerOrder float64 `json:"minimum_per_order"`
	// MaximumFractionOfTradeValue caps the charge at a fraction of the
	// order's own trade value (quantity x price x DollarsPerPoint). It must
	// be positive and at most 1: a cap of zero would charge nothing at all
	// however large the per-share rate — which is also exactly what a
	// record written before this block existed decodes to, so this is the
	// field that makes an older configuration fail closed (see
	// ConfigurationSchemaVersion).
	MaximumFractionOfTradeValue float64 `json:"maximum_fraction_of_trade_value"`
}

// NotionalAccountConfig carries the Notional Account settings (ADR 0007): a
// configured starting equity, re-based to actual equity every year on the
// configured month and day.
type NotionalAccountConfig struct {
	StartingEquity float64 `json:"starting_equity"`
	RebasingMonth  int     `json:"rebasing_month"`
	RebasingDay    int     `json:"rebasing_day"`
}

// ConfigurationPayload carries every Baseline parameter named in ADRs 0002,
// 0003, 0005, 0007, and 0013: the strategy identifier, Sizing Mode, Unit
// Volatility Fraction, Stop Multiple, Entry and Exit Channel lengths, the
// maximum number of Units, slippage in N, and the Notional Account settings.
//
// The numeric policy for these fields (float64 precision, rounding,
// eventual fixed-point representation) is deliberately unresolved here; that
// is ADR territory, out of scope for this contract. ConfigurationHash (#50,
// ADR 0016) derives from exactly this payload plus its schema version — see
// ConfigurationHash.
type ConfigurationPayload struct {
	StrategyID             string     `json:"strategy_id"`
	SizingMode             SizingMode `json:"sizing_mode"`
	UnitVolatilityFraction float64    `json:"unit_volatility_fraction"`
	StopMultiple           float64    `json:"stop_multiple"`
	EntryChannelLength     int        `json:"entry_channel_length"`
	ExitChannelLength      int        `json:"exit_channel_length"`
	MaxUnits               int        `json:"max_units"`
	SlippageN              float64    `json:"slippage_n"`
	// TierBDistanceInN is how close (in N) a Setup's high may sit below the
	// Entry Channel and still be reported as Tier B (CONTEXT.md: "Tier";
	// #9). Unlike the channel lengths above, this is not a Faith number:
	// System 2's own printed rules have no concept of an "approaching"
	// state. It is a Baseline-declared adaptation (ADR 0012's provenance
	// taxonomy) that whoever owns the Baseline configuration (#50) must
	// choose deliberately, not a value transcribed from a source.
	TierBDistanceInN float64 `json:"tier_b_distance_in_n"`
	// DollarsPerPoint is the instrument's contract multiplier: what one
	// point of price movement is worth per share or contract. For US
	// equities it is exactly 1 — one point of price is one dollar per share
	// — and for Faith's Heating Oil example it is 42,000 (The Turtle Rules
	// p.15). It is a parameter rather than a constant so that example
	// reproduces exactly rather than being approximated.
	//
	// It lives on the strategy configuration for slice 1, which trades a
	// single equity symbol where the multiplier is 1 for every instrument.
	// That is a deliberate simplification, not a claim about the domain:
	// the contract multiplier is genuinely a property of the instrument, not
	// of the strategy, and a universe that ever contains a futures contract
	// (or an instrument quoted in a different unit) will need it to arrive
	// as per-instrument reference data instead. See #10's Concerns.
	DollarsPerPoint float64 `json:"dollars_per_point"`
	// RiskAtStopFraction is the fraction of the Notional Account one Unit
	// loses at its Protective Stop — and it is a configurable input in
	// exactly one Sizing Mode.
	//
	// Under SizingModeFixedRiskAtStop it is the sizing input [M p.56] and
	// must be present and positive. Under SizingModeVolatilityNormalised it
	// must be **zero**: ADR 0003's central decision is that Risk at Stop
	// there is derived (Unit Volatility Fraction x Stop Multiple), never
	// configured, and Validate rejects a configuration that states it rather
	// than quietly ignoring the stated value. Quietly ignoring it is how a
	// run comes to believe it risks a figure that nothing in the arithmetic
	// honours — the exact confusion ADR 0003 was written to prevent.
	RiskAtStopFraction float64               `json:"risk_at_stop_fraction"`
	NotionalAccount    NotionalAccountConfig `json:"notional_account"`
	// Commission is ADR 0013's commission model (#18), the cost-model
	// companion to SlippageN above.
	Commission CommissionConfig `json:"commission"`
}

// Validate checks that every Baseline parameter is present and in range. A
// zero slippage value is rejected (ADR 0013: "A backtest run with zero
// slippage is invalid by construction"), the Sizing Mode must be one of the
// two declared values, channel lengths and maximum Units must be positive
// integers, and every float64 parameter must be finite (isFinite, defined
// alongside PriceView in bar.go): NaN and +/-Inf are rejected explicitly,
// before the range check that follows, rather than silently passing an
// ordered comparison that is always false against NaN.
//
// RiskAtStopFraction is the one field whose validity depends on another: it
// must be absent (zero) under volatility-normalised sizing and present and
// positive under fixed-risk-at-stop. See the field's own comment for why the
// first half is a rejection rather than an ignore.
func (c ConfigurationPayload) Validate() error {
	var errs []error
	if c.StrategyID == "" {
		errs = append(errs, errors.New("strategy id is required"))
	}
	switch c.SizingMode {
	case SizingModeVolatilityNormalised, SizingModeFixedRiskAtStop:
		// recognised
	default:
		errs = append(errs, fmt.Errorf("sizing mode %q is not a recognised sizing mode", c.SizingMode))
	}
	switch {
	case !isFinite(c.UnitVolatilityFraction):
		errs = append(errs, errors.New("unit volatility fraction must be finite"))
	case c.UnitVolatilityFraction <= 0 || c.UnitVolatilityFraction > 1:
		errs = append(errs, errors.New("unit volatility fraction must be greater than zero and at most one"))
	}
	switch {
	case !isFinite(c.StopMultiple):
		errs = append(errs, errors.New("stop multiple must be finite"))
	case c.StopMultiple <= 0:
		errs = append(errs, errors.New("stop multiple must be positive"))
	}
	if c.EntryChannelLength <= 0 {
		errs = append(errs, errors.New("entry channel length must be a positive integer"))
	}
	if c.ExitChannelLength <= 0 {
		errs = append(errs, errors.New("exit channel length must be a positive integer"))
	}
	if c.MaxUnits <= 0 {
		errs = append(errs, errors.New("maximum units must be a positive integer"))
	}
	switch {
	case !isFinite(c.SlippageN):
		errs = append(errs, errors.New("slippage must be finite"))
	case c.SlippageN <= 0:
		errs = append(errs, errors.New("slippage must be positive; zero slippage is invalid by construction (ADR 0013)"))
	}
	switch {
	case !isFinite(c.TierBDistanceInN):
		errs = append(errs, errors.New("tier b distance in n must be finite"))
	case c.TierBDistanceInN < 0:
		errs = append(errs, errors.New("tier b distance in n must not be negative"))
	}
	switch {
	case !isFinite(c.DollarsPerPoint):
		errs = append(errs, errors.New("dollars per point must be finite"))
	case c.DollarsPerPoint <= 0:
		errs = append(errs, errors.New("dollars per point must be positive; it is 1 for shares and the contract multiplier for a futures contract"))
	}
	// Risk at Stop is configurable in exactly one Sizing Mode. The
	// volatility-normalised branch is #10's named negative case, and it
	// rejects any non-zero value including the one the derivation would
	// itself produce: the rule is that the field is absent in this mode, not
	// that it must agree. Comparing against zero rather than range-checking
	// also catches NaN, which is a configured value, not an absent one.
	switch c.SizingMode {
	case SizingModeVolatilityNormalised:
		if c.RiskAtStopFraction != 0 {
			errs = append(errs, fmt.Errorf(
				"risk at stop must not be configured under volatility-normalised sizing (ADR 0003: it is derived as unit volatility fraction x stop multiple), got %v", c.RiskAtStopFraction))
		}
	case SizingModeFixedRiskAtStop:
		switch {
		case !isFinite(c.RiskAtStopFraction):
			errs = append(errs, errors.New("risk at stop fraction must be finite"))
		case c.RiskAtStopFraction <= 0 || c.RiskAtStopFraction > 1:
			errs = append(errs, errors.New("risk at stop fraction must be greater than zero and at most one under fixed-risk-at-stop sizing, where it is the sizing input"))
		}
	}
	switch {
	case !isFinite(c.NotionalAccount.StartingEquity):
		errs = append(errs, errors.New("notional account starting equity must be finite"))
	case c.NotionalAccount.StartingEquity <= 0:
		errs = append(errs, errors.New("notional account starting equity must be positive"))
	}
	if !validRebasingDate(c.NotionalAccount.RebasingMonth, c.NotionalAccount.RebasingDay) {
		errs = append(errs, errors.New("notional account rebasing date must be a valid month and day"))
	}
	// #18: the commission model. The rate and the floor may legitimately be
	// zero (a commission-free venue is a declared Variant); the cap may not,
	// both because a zero cap charges nothing at all and because it is what
	// an older record decodes to — see CommissionConfig's own field comments.
	switch {
	case !isFinite(c.Commission.PerShare):
		errs = append(errs, errors.New("commission per share must be finite"))
	case c.Commission.PerShare < 0:
		errs = append(errs, errors.New("commission per share must not be negative"))
	}
	switch {
	case !isFinite(c.Commission.MinimumPerOrder):
		errs = append(errs, errors.New("commission minimum per order must be finite"))
	case c.Commission.MinimumPerOrder < 0:
		errs = append(errs, errors.New("commission minimum per order must not be negative"))
	}
	switch {
	case !isFinite(c.Commission.MaximumFractionOfTradeValue):
		errs = append(errs, errors.New("commission maximum fraction of trade value must be finite"))
	case c.Commission.MaximumFractionOfTradeValue <= 0 || c.Commission.MaximumFractionOfTradeValue > 1:
		errs = append(errs, errors.New("commission maximum fraction of trade value must be greater than zero and at most one; a zero cap would charge nothing on every order, and is what a configuration recorded before the commission model existed decodes to"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid configuration payload: %w", err)
	}
	return nil
}

// validRebasingDate reports whether month/day form a date that recurs
// identically every year. 2027 is used as the reference year specifically
// because it is not a leap year: a 29 February re-basing date would not
// recur every year, so it must be rejected rather than silently rolled over
// to 1 March by time.Date.
func validRebasingDate(month, day int) bool {
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return false
	}
	t := time.Date(2027, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	return int(t.Month()) == month && t.Day() == day
}
