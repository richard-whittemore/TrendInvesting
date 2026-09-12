package event

import (
	"errors"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// NotionalAccountCashAdjustedEventType identifies the
// Notional-Account-cash-adjusted decision payload for the Envelope's Type
// field: a deposit or withdrawal scaled the yearly starting figure and the
// Notional Account so that it can neither trigger nor mask a Drawdown Step
// (ADR 0007).
const NotionalAccountCashAdjustedEventType = "strategy.notional-account.cash-adjusted"

// NotionalAccountCashAdjustedSchemaVersion is the current schema version of
// NotionalAccountCashAdjustedPayload, for the Envelope's SchemaVersion
// field.
const NotionalAccountCashAdjustedSchemaVersion uint32 = 1

// RuleNotionalAccountCashAdjustment names the rule for
// NotionalAccountCashAdjustedPayload.Rule: a cash movement scales the
// yearly starting figure, the measurement base, and the Notional Account by
// the identical factor (ADR 0007).
const RuleNotionalAccountCashAdjustment = "notional-account.cash-adjustment"

// ADRNotionalAccountCashAdjustment is the ADR
// NotionalAccountCashAdjustedPayload.ADR cites: ADR 0007.
const ADRNotionalAccountCashAdjustment = "0007"

// NotionalAccountCashAdjustedPayload records one cash movement's effect on
// the Notional Account: a movement of Amount took equity from EquityBefore
// to EquityAfter, and scaled the yearly starting figure from
// StartingFigureBefore to StartingFigureAfter and the Notional Account from
// NotionalBefore to NotionalAfter by the identical factor (ADR 0007; The
// Turtle Rules p.17's ladder is unaffected by a movement exactly because
// every figure it is measured against moves together).
type NotionalAccountCashAdjustedPayload struct {
	// AsOf is the cash movement's own as-of time — the same value as the
	// CashMovementPayload.AsOf it was derived from.
	AsOf time.Time `json:"as_of"`
	// Amount is the movement: positive for a deposit, negative for a
	// withdrawal.
	Amount float64 `json:"amount"`
	// EquityBefore and EquityAfter are actual equity immediately before and
	// after this movement. EquityAfter must equal EquityBefore+Amount
	// EXACTLY (see Validate).
	EquityBefore float64 `json:"equity_before"`
	EquityAfter  float64 `json:"equity_after"`
	// StartingFigureBefore and StartingFigureAfter are the yearly starting
	// figure immediately before and after this movement.
	// StartingFigureAfter must equal
	// sizing.CashMovementScaledFigure(StartingFigureBefore, EquityBefore,
	// EquityAfter) EXACTLY — no tolerance, the same exact-derivation
	// discipline DrawdownStepAppliedPayload.Validate uses (see that field's
	// own doc comment): the reducer
	// (internal/strategy.NotionalAccount.ApplyCashMovement) and this
	// Validate both call the identical exported
	// sizing.CashMovementScaledFigure, so nothing is chained or
	// independently re-derived across the check.
	StartingFigureBefore float64 `json:"starting_figure_before"`
	StartingFigureAfter  float64 `json:"starting_figure_after"`
	// NotionalBefore and NotionalAfter are the Notional Account immediately
	// before and after this movement, scaled by the identical factor as
	// StartingFigureBefore/After (see Validate).
	NotionalBefore float64 `json:"notional_before"`
	NotionalAfter  float64 `json:"notional_after"`
	// Rule and ADR name the strategy rule that produced this adjustment
	// (docs/development.md principle 3).
	Rule string `json:"rule"`
	ADR  string `json:"adr"`
}

// Validate checks that the payload identifies when it applies, that Amount
// is finite and non-zero, that EquityBefore/EquityAfter and
// StartingFigureBefore/NotionalBefore are finite and positive, that
// EquityAfter matches EquityBefore+Amount EXACTLY, and that
// StartingFigureAfter and NotionalAfter each match
// sizing.CashMovementScaledFigure's derivation from their own Before figure
// EXACTLY (no tolerance — see the payload's own doc comment for why), and
// that Rule and ADR are present.
func (p NotionalAccountCashAdjustedPayload) Validate() error {
	var errs []error
	if p.AsOf.IsZero() {
		errs = append(errs, errors.New("as of is required"))
	}

	amountFinite := isFinite(p.Amount)
	switch {
	case !amountFinite:
		errs = append(errs, errors.New("amount must be finite"))
	case p.Amount == 0:
		errs = append(errs, errors.New("amount must be non-zero"))
	}

	equityBeforeFinite := isFinite(p.EquityBefore)
	switch {
	case !equityBeforeFinite:
		errs = append(errs, errors.New("equity before must be finite"))
	case p.EquityBefore <= 0:
		errs = append(errs, errors.New("equity before must be positive"))
	}

	equityAfterFinite := isFinite(p.EquityAfter)
	switch {
	case !equityAfterFinite:
		errs = append(errs, errors.New("equity after must be finite"))
	case p.EquityAfter <= 0:
		errs = append(errs, errors.New("equity after must be positive"))
	}

	if amountFinite && equityBeforeFinite && equityAfterFinite {
		if want := p.EquityBefore + p.Amount; p.EquityAfter != want {
			errs = append(errs, fmt.Errorf(
				"equity after %v does not match equity before %v plus amount %v (%v)",
				p.EquityAfter, p.EquityBefore, p.Amount, want))
		}
	}

	startingBeforeFinite := isFinite(p.StartingFigureBefore)
	switch {
	case !startingBeforeFinite:
		errs = append(errs, errors.New("starting figure before must be finite"))
	case p.StartingFigureBefore <= 0:
		errs = append(errs, errors.New("starting figure before must be positive"))
	}

	startingAfterFinite := isFinite(p.StartingFigureAfter)
	switch {
	case !startingAfterFinite:
		errs = append(errs, errors.New("starting figure after must be finite"))
	case p.StartingFigureAfter <= 0:
		errs = append(errs, errors.New("starting figure after must be positive"))
	}

	if startingBeforeFinite && startingAfterFinite && equityBeforeFinite && equityAfterFinite {
		if derived := sizing.CashMovementScaledFigure(p.StartingFigureBefore, p.EquityBefore, p.EquityAfter); p.StartingFigureAfter != derived {
			errs = append(errs, fmt.Errorf(
				"starting figure after %v does not match the derivation %v (sizing.CashMovementScaledFigure of starting figure before %v)",
				p.StartingFigureAfter, derived, p.StartingFigureBefore))
		}
	}

	notionalBeforeFinite := isFinite(p.NotionalBefore)
	switch {
	case !notionalBeforeFinite:
		errs = append(errs, errors.New("notional before must be finite"))
	case p.NotionalBefore <= 0:
		errs = append(errs, errors.New("notional before must be positive"))
	}

	notionalAfterFinite := isFinite(p.NotionalAfter)
	switch {
	case !notionalAfterFinite:
		errs = append(errs, errors.New("notional after must be finite"))
	case p.NotionalAfter <= 0:
		errs = append(errs, errors.New("notional after must be positive"))
	}

	if notionalBeforeFinite && notionalAfterFinite && equityBeforeFinite && equityAfterFinite {
		if derived := sizing.CashMovementScaledFigure(p.NotionalBefore, p.EquityBefore, p.EquityAfter); p.NotionalAfter != derived {
			errs = append(errs, fmt.Errorf(
				"notional after %v does not match the derivation %v (sizing.CashMovementScaledFigure of notional before %v)",
				p.NotionalAfter, derived, p.NotionalBefore))
		}
	}

	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid notional account cash adjusted payload: %w", err)
	}
	return nil
}
