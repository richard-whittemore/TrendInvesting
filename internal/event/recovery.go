package event

import (
	"errors"
	"fmt"
	"time"
)

// NotionalAccountRecoveredEventType identifies the
// Notional-Account-recovered decision payload for the Envelope's Type
// field: ADR 0007's full recovery, which restores the Notional Account only
// when actual equity regains the yearly starting figure — never on a new
// high-water mark (CONTEXT.md: "Notional Account").
const NotionalAccountRecoveredEventType = "strategy.notional-account.recovered"

// NotionalAccountRecoveredSchemaVersion is the current schema version of
// NotionalAccountRecoveredPayload, for the Envelope's SchemaVersion field.
const NotionalAccountRecoveredSchemaVersion uint32 = 1

// RuleNotionalAccountRecovery names the rule for
// NotionalAccountRecoveredPayload.Rule: the Notional Account and its
// measurement base are restored to the yearly starting figure once actual
// equity regains it (ADR 0007).
const RuleNotionalAccountRecovery = "notional-account.recovery"

// ADRNotionalAccountRecovery is the ADR NotionalAccountRecoveredPayload.ADR
// cites: ADR 0007.
const ADRNotionalAccountRecovery = "0007"

// NotionalAccountRecoveredPayload records one recovery: the Notional
// Account, which stood at NotionalBefore after StepsCleared Drawdown Steps,
// was restored to StartingFigure because Equity, read from an account
// snapshot as of AsOf, regained it (ADR 0007).
type NotionalAccountRecoveredPayload struct {
	// AsOf is the account snapshot's as-of time that caused this recovery.
	AsOf time.Time `json:"as_of"`
	// Equity is the actual account equity read from that snapshot. Must be
	// at or above StartingFigure (see Validate): a recovery only fires once
	// equity has regained it.
	Equity float64 `json:"equity"`
	// StartingFigure is the yearly starting figure regained: what the
	// Notional Account and its measurement base are restored to.
	StartingFigure float64 `json:"starting_figure"`
	// NotionalBefore is the Notional Account immediately before this
	// recovery. Must be strictly below StartingFigure (see Validate): a
	// recovery only fires after one or more Drawdown Steps.
	NotionalBefore float64 `json:"notional_before"`
	// StepsCleared is how many Drawdown Steps had been applied, since the
	// ladder was last reset by a re-basing or a prior recovery, that this
	// recovery clears. Always a positive count: a recovery only fires after
	// one or more steps.
	StepsCleared int `json:"steps_cleared"`
	// Rule and ADR name the strategy rule that produced this recovery
	// (docs/development.md principle 3).
	Rule string `json:"rule"`
	ADR  string `json:"adr"`
}

// Validate checks that the payload identifies when it applies, that every
// figure is finite and positive, that NotionalBefore is strictly below
// StartingFigure (a recovery only fires from an active drawdown), that
// Equity is at or above StartingFigure (the recovery condition itself),
// that StepsCleared is a positive count, and that Rule and ADR are present.
func (p NotionalAccountRecoveredPayload) Validate() error {
	var errs []error
	if p.AsOf.IsZero() {
		errs = append(errs, errors.New("as of is required"))
	}

	equityFinite := isFinite(p.Equity)
	switch {
	case !equityFinite:
		errs = append(errs, errors.New("equity must be finite"))
	case p.Equity <= 0:
		errs = append(errs, errors.New("equity must be positive"))
	}

	startingFigureFinite := isFinite(p.StartingFigure)
	switch {
	case !startingFigureFinite:
		errs = append(errs, errors.New("starting figure must be finite"))
	case p.StartingFigure <= 0:
		errs = append(errs, errors.New("starting figure must be positive"))
	}

	beforeFinite := isFinite(p.NotionalBefore)
	switch {
	case !beforeFinite:
		errs = append(errs, errors.New("notional before must be finite"))
	case p.NotionalBefore <= 0:
		errs = append(errs, errors.New("notional before must be positive"))
	}

	if beforeFinite && startingFigureFinite && p.NotionalBefore >= p.StartingFigure {
		errs = append(errs, fmt.Errorf(
			"notional before %v must be strictly below starting figure %v: a recovery only follows one or more Drawdown Steps",
			p.NotionalBefore, p.StartingFigure))
	}

	if equityFinite && startingFigureFinite && p.Equity < p.StartingFigure {
		errs = append(errs, fmt.Errorf(
			"equity %v must be at or above starting figure %v: a recovery only fires once equity regains the yearly starting figure (ADR 0007), never on a new high-water mark",
			p.Equity, p.StartingFigure))
	}

	if p.StepsCleared < 1 {
		errs = append(errs, fmt.Errorf("steps cleared must be a positive count, got %d", p.StepsCleared))
	}

	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid notional account recovered payload: %w", err)
	}
	return nil
}
