package event

import (
	"errors"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// DrawdownStepAppliedEventType identifies the Drawdown-Step-applied decision
// payload for the Envelope's Type field: ADR 0007's Notional Account was
// reduced by one Drawdown Step (CONTEXT.md: "Drawdown Step").
const DrawdownStepAppliedEventType = "strategy.drawdown-step.applied"

// DrawdownStepAppliedSchemaVersion is the current schema version of
// DrawdownStepAppliedPayload, for the Envelope's SchemaVersion field.
const DrawdownStepAppliedSchemaVersion uint32 = 1

// RuleNotionalAccountDrawdownStep names the rule for
// DrawdownStepAppliedPayload.Rule: a 20% reduction of the Notional Account
// each time actual equity falls 10% of the CURRENT Notional Account below
// the figure it was last measured from (The Turtle Rules p.17; ADR 0007).
const RuleNotionalAccountDrawdownStep = "notional-account.drawdown-step"

// ADRNotionalAccountDrawdownStep is the ADR DrawdownStepAppliedPayload.ADR
// cites: ADR 0007, which defines the Notional Account and its Drawdown Step
// ladder (a 10% fall of the current account triggers a 20% reduction,
// measured against the figure last measured from — not a high-water mark).
const ADRNotionalAccountDrawdownStep = "0007"

// DrawdownStepAppliedPayload records one Drawdown Step: the Notional Account
// fell from NotionalBefore to NotionalAfter because Equity, read from an
// account snapshot as of AsOf, crossed Threshold (CONTEXT.md: "Drawdown
// Step"; The Turtle Rules p.17, ADR 0007).
type DrawdownStepAppliedPayload struct {
	// AsOf is the account snapshot's as-of time that caused this step — the
	// same value as the AccountSnapshotPayload.AsOf it was derived from,
	// carried here so a step is traceable to its snapshot without joining on
	// causation alone.
	AsOf time.Time `json:"as_of"`
	// Equity is the actual account equity read from that snapshot.
	Equity float64 `json:"equity"`
	// Threshold is the equity level this step crossed: the measurement base
	// then in force, less 10% of the Notional Account before this step (The
	// Turtle Rules p.17: "down 10%"). Equity at or below it steps the
	// account; equity above it does not — the boundary is inclusive.
	Threshold float64 `json:"threshold"`
	// NotionalBefore and NotionalAfter are the Notional Account immediately
	// before and after this one step. NotionalAfter must equal
	// sizing.DrawdownSteppedNotional(NotionalBefore) EXACTLY — no tolerance —
	// and must always be strictly less than NotionalBefore.
	//
	// Exact float64 equality is deliberate, not merely convenient. Each step
	// event carries its own NotionalBefore/NotionalAfter pair, and both the
	// reducer (internal/strategy.NotionalAccount.Observe) and Validate call
	// the identical exported sizing.DrawdownSteppedNotional on the identical
	// NotionalBefore: nothing is chained or independently re-derived across
	// the check, so a tolerance would only let a differently-derived number
	// through — the defect the check exists to catch (#10's
	// TradeProposalPayload precedent; #65 tracks this "one shared function,
	// exact equality" discipline generally, including the risk that two
	// textually identical expressions can be fused differently across
	// architectures — see sizing.DrawdownSteppedNotional's doc comment for
	// why that risk does not apply to this particular derivation).
	NotionalBefore float64 `json:"notional_before"`
	NotionalAfter  float64 `json:"notional_after"`
	// StepNumber is this step's 1-based position among every Drawdown Step
	// applied so far in the run (yearly re-basing, which would reset this
	// count, is #17). A single account snapshot with a large enough drop can
	// apply several steps, each with its own StepNumber, in the order
	// applied.
	StepNumber int `json:"step_number"`
	// Rule and ADR name the strategy rule that produced this step
	// (docs/development.md principle 3).
	Rule string `json:"rule"`
	ADR  string `json:"adr"`
}

// Validate checks that the payload identifies when it applies, that every
// figure is finite and positive, that NotionalAfter is strictly below
// NotionalBefore and matches sizing.DrawdownSteppedNotional(NotionalBefore)
// EXACTLY (no tolerance — see the field's own doc comment for why), that
// Equity is at or below Threshold (the inclusive boundary The Turtle Rules
// p.17 describes), and that StepNumber, Rule, and ADR are present.
func (p DrawdownStepAppliedPayload) Validate() error {
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

	thresholdFinite := isFinite(p.Threshold)
	switch {
	case !thresholdFinite:
		errs = append(errs, errors.New("threshold must be finite"))
	case p.Threshold <= 0:
		errs = append(errs, errors.New("threshold must be positive"))
	}

	beforeFinite := isFinite(p.NotionalBefore)
	switch {
	case !beforeFinite:
		errs = append(errs, errors.New("notional before must be finite"))
	case p.NotionalBefore <= 0:
		errs = append(errs, errors.New("notional before must be positive"))
	}

	afterFinite := isFinite(p.NotionalAfter)
	switch {
	case !afterFinite:
		errs = append(errs, errors.New("notional after must be finite"))
	case p.NotionalAfter <= 0:
		errs = append(errs, errors.New("notional after must be positive"))
	}

	if beforeFinite && afterFinite {
		if p.NotionalAfter >= p.NotionalBefore {
			errs = append(errs, fmt.Errorf(
				"notional after %v must be strictly below notional before %v: a drawdown step only ever reduces the account",
				p.NotionalAfter, p.NotionalBefore))
		}
		if derived := sizing.DrawdownSteppedNotional(p.NotionalBefore); p.NotionalAfter != derived {
			errs = append(errs, fmt.Errorf(
				"notional after %v does not match the derivation %v (sizing.DrawdownSteppedNotional of notional before %v)",
				p.NotionalAfter, derived, p.NotionalBefore))
		}
	}

	if equityFinite && thresholdFinite && p.Equity > p.Threshold {
		errs = append(errs, fmt.Errorf(
			"equity %v exceeds threshold %v: a drawdown step only applies at or below the threshold it crossed (The Turtle Rules p.17: \"down 10%%\")",
			p.Equity, p.Threshold))
	}

	if p.StepNumber < 1 {
		errs = append(errs, fmt.Errorf("step number must be a positive, 1-based count, got %d", p.StepNumber))
	}
	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid drawdown step applied payload: %w", err)
	}
	return nil
}
