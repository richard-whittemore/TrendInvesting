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
// Bumped to 2 for #9: TierBDistanceInN was added. A schema change is
// explicit in this project (docs/development.md), never a silent field
// addition.
const ConfigurationSchemaVersion uint32 = 2

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
// is ADR territory, out of scope for this contract. ConfigurationHash
// derivation (#50) is also out of scope: this payload is its obvious input.
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
	TierBDistanceInN float64               `json:"tier_b_distance_in_n"`
	NotionalAccount  NotionalAccountConfig `json:"notional_account"`
}

// Validate checks that every Baseline parameter is present and in range. A
// zero slippage value is rejected (ADR 0013: "A backtest run with zero
// slippage is invalid by construction"), the Sizing Mode must be one of the
// two declared values, channel lengths and maximum Units must be positive
// integers, and every float64 parameter must be finite (isFinite, defined
// alongside PriceView in bar.go): NaN and +/-Inf are rejected explicitly,
// before the range check that follows, rather than silently passing an
// ordered comparison that is always false against NaN.
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
	case !isFinite(c.NotionalAccount.StartingEquity):
		errs = append(errs, errors.New("notional account starting equity must be finite"))
	case c.NotionalAccount.StartingEquity <= 0:
		errs = append(errs, errors.New("notional account starting equity must be positive"))
	}
	if !validRebasingDate(c.NotionalAccount.RebasingMonth, c.NotionalAccount.RebasingDay) {
		errs = append(errs, errors.New("notional account rebasing date must be a valid month and day"))
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
