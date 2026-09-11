package event

import (
	"errors"
	"fmt"
	"time"
)

// NotionalAccountRebasedEventType identifies the Notional-Account-rebased
// decision payload for the Envelope's Type field: ADR 0007's yearly
// re-basing to actual equity (CONTEXT.md: "Notional Account"; #17).
const NotionalAccountRebasedEventType = "strategy.notional-account.rebased"

// NotionalAccountRebasedSchemaVersion is the current schema version of
// NotionalAccountRebasedPayload, for the Envelope's SchemaVersion field.
const NotionalAccountRebasedSchemaVersion uint32 = 1

// RuleNotionalAccountRebase names the rule for
// NotionalAccountRebasedPayload.Rule: the Notional Account, its measurement
// base, and the yearly starting figure are all set to actual equity on the
// first snapshot on or after the configured re-basing date in a new year
// (ADR 0007).
const RuleNotionalAccountRebase = "notional-account.rebase"

// ADRNotionalAccountRebase is the ADR NotionalAccountRebasedPayload.ADR
// cites: ADR 0007.
const ADRNotionalAccountRebase = "0007"

// NotionalAccountRebasedPayload records one re-basing: the yearly starting
// figure moved from PreviousStartingFigure to NewStartingFigure — actual
// equity, as of AsOf (ADR 0007). Re-basing sets the Notional Account, its
// measurement base, AND the yearly starting figure all to actual equity in
// one motion, so NewStartingFigure always equals Equity exactly.
type NotionalAccountRebasedPayload struct {
	// AsOf is the account snapshot's as-of time that caused this re-basing.
	AsOf time.Time `json:"as_of"`
	// PreviousStartingFigure is the yearly starting figure immediately
	// before this re-basing: either the previous year's re-based figure, or
	// the configured StartingEquity if this is the account's first
	// re-basing.
	PreviousStartingFigure float64 `json:"previous_starting_figure"`
	// NewStartingFigure is the yearly starting figure after this re-basing:
	// actual equity. Must equal Equity exactly (see Validate).
	NewStartingFigure float64 `json:"new_starting_figure"`
	// Equity is the actual account equity read from the triggering
	// snapshot.
	Equity float64 `json:"equity"`
	// Rule and ADR name the strategy rule that produced this re-basing
	// (docs/development.md principle 3).
	Rule string `json:"rule"`
	ADR  string `json:"adr"`
}

// Validate checks that the payload identifies when it applies, that every
// figure is finite and positive, that NewStartingFigure matches Equity
// EXACTLY (re-basing sets the yearly starting figure to actual equity, not
// to something merely close to it), and that Rule and ADR are present.
func (p NotionalAccountRebasedPayload) Validate() error {
	var errs []error
	if p.AsOf.IsZero() {
		errs = append(errs, errors.New("as of is required"))
	}

	previousFinite := isFinite(p.PreviousStartingFigure)
	switch {
	case !previousFinite:
		errs = append(errs, errors.New("previous starting figure must be finite"))
	case p.PreviousStartingFigure <= 0:
		errs = append(errs, errors.New("previous starting figure must be positive"))
	}

	newFinite := isFinite(p.NewStartingFigure)
	switch {
	case !newFinite:
		errs = append(errs, errors.New("new starting figure must be finite"))
	case p.NewStartingFigure <= 0:
		errs = append(errs, errors.New("new starting figure must be positive"))
	}

	equityFinite := isFinite(p.Equity)
	switch {
	case !equityFinite:
		errs = append(errs, errors.New("equity must be finite"))
	case p.Equity <= 0:
		errs = append(errs, errors.New("equity must be positive"))
	}

	if newFinite && equityFinite && p.NewStartingFigure != p.Equity {
		errs = append(errs, fmt.Errorf(
			"new starting figure %v must equal equity %v exactly: re-basing sets the yearly starting figure to actual equity (ADR 0007)",
			p.NewStartingFigure, p.Equity))
	}

	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid notional account rebased payload: %w", err)
	}
	return nil
}
