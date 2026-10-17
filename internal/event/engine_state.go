package event

import (
	"errors"
	"fmt"
)

// EngineStateEventType identifies the engine-state decision payload for the
// Envelope's Type field: the reducer detected a capital-safety invariant
// violation it cannot continue past, and is declaring itself unable to keep
// deciding.
//
// Named "strategy.*", not "execution.*": this is the reducer's own decision
// about itself, not an external fact. No existing name was found to reuse —
// docs/architecture.md's "Safety invariants" section states the *rule*
// ("material reconciliation differences force safe mode") but names no
// event. Later work needing an engine-state event should reuse THIS type and
// EngineStatePayload — adding a Reason value, the same way
// CampaignExitedPayload's Reason grows — rather than minting a second one.
const EngineStateEventType = "strategy.engine.state"

// EngineStateSchemaVersion is schema version 1 of EngineStatePayload: the
// state halted only, with no scope, meaning the whole engine. It is what the
// Turtle reducer emits.
const EngineStateSchemaVersion uint32 = 1

// EngineStateScopedVersion is schema version 2 of EngineStatePayload
// (ADR 0019, amended 2026-09-24, "The schema change"): it adds the state
// degraded and the scope naming the affected instruments, orders and cash
// components. Version-1 records stay valid under the version-1 rules. A
// degraded record with no scope is invalid: scope is never inferred from
// absence.
const EngineStateScopedVersion uint32 = 2

// EngineStateHalted means the run must stop rather than continue past the
// violation named in Reason; a halted record names no scope, since it covers
// the whole engine.
const EngineStateHalted = "halted"

// EngineStateDegraded means the engine continues within the limits ADR 0019
// (amended 2026-09-24) and ADR 0026 (point 4) set: no new risk, and the
// instruments, orders and cash components its scope names are frozen.
const EngineStateDegraded = "degraded"

// The enumerated reasons the engine can halt. A closed set, not free text,
// for the same reason every other Reason field in this package is: a
// journal must be groupable by it.
const (
	// EngineStateReasonCampaignWithoutProtectiveStop means an open Campaign
	// was found, at the start of a completed bar, without a Protective
	// Stop it could use (CONTEXT.md: "Every open Campaign has one at all
	// times"). What this reason requires is a FINITE, POSITIVE stop, and
	// nothing about its relation to the entry price — not because a stop
	// may sit anywhere, but because this invariant reads each Unit's
	// current stop and cannot tell a first level from one the Stop Ladder
	// has raised. It therefore enforces only what holds of either kind.
	// The kind-dependent relation lives at the seams that DO know which
	// one a level is: strictly below entry for an initial stop, no
	// relation for a raised one (sizing.ValidStopLevel states both, and
	// StopKindRaised is the kind this invariant uses).
	// This can only happen if memory was
	// corrupted after a Campaign opened — see
	// internal/strategy/campaign.go's checkCampaignHasAProtectiveStop for
	// why the Campaign struct cannot be built without a valid stop in the
	// first place.
	EngineStateReasonCampaignWithoutProtectiveStop = "campaign-without-protective-stop"
	// EngineStateReasonRiskLimitBreached means the target-weight risk gate
	// refused an order for breaching a hard limit (ADR 0026, point 3): an
	// order with no journalled decision halts, any other breach degrades.
	// It is recorded at schema version 2 only.
	EngineStateReasonRiskLimitBreached = "risk-limit-breached"
)

// EngineStatePayload records the engine halting because a capital-safety
// invariant it depends on no longer holds.
//
// Deliberately minimal — State, Reason and Detail only, no InstrumentID or
// PeriodEnd of its own: Detail carries whatever figures the specific Reason
// needs (see checkCampaignHasAProtectiveStop's Detail message), and the
// envelope this payload travels in already carries EventTime and, via
// CausationID, the input that triggered the halt.
type EngineStatePayload struct {
	State string `json:"state"`
	// Reason is one of the enumerated EngineStateReason constants.
	Reason string `json:"reason"`
	// Detail carries the figures behind this particular halt, in prose. It
	// is required: a reason without its figures cannot be checked (the same
	// rule ProposalDeclinedPayload.Detail and DrawdownStepAppliedPayload's
	// figures follow).
	Detail string `json:"detail"`
	// The scope (schema version 2 only): what a degraded engine has frozen,
	// each list strictly ascending. All three are omitted when empty, so a
	// halted record encodes exactly as it did at version 1.
	ScopeInstrumentIDs  []string `json:"scope_instrument_ids,omitempty"`
	ScopeOrderIDs       []string `json:"scope_order_ids,omitempty"`
	ScopeCashComponents []string `json:"scope_cash_components,omitempty"`
}

// ValidateSchema checks p under the rules of schemaVersion: version 1 knows
// only halted with no scope, for campaign-without-protective-stop, the one
// reason version-1 readers know; version 2 is Validate's rules, and carries
// every later reason. Any other version is refused (ADR 0015).
func (p EngineStatePayload) ValidateSchema(schemaVersion uint32) error {
	switch schemaVersion {
	case EngineStateSchemaVersion:
		if p.State != EngineStateHalted || p.scoped() {
			return fmt.Errorf("invalid engine state payload: schema version 1 records only %q with no scope", EngineStateHalted)
		}
		if p.Reason != EngineStateReasonCampaignWithoutProtectiveStop {
			return fmt.Errorf("invalid engine state payload: schema version 1 records only the reason %q", EngineStateReasonCampaignWithoutProtectiveStop)
		}
	case EngineStateScopedVersion:
	default:
		return fmt.Errorf("invalid engine state payload: schema version %d is not recognised", schemaVersion)
	}
	return p.Validate()
}

func (p EngineStatePayload) scoped() bool {
	return len(p.ScopeInstrumentIDs)+len(p.ScopeOrderIDs)+len(p.ScopeCashComponents) > 0
}

// Validate checks that State and Reason are each one of their enumerated
// constants, that Detail is present, and that a degraded state names a scope
// and a halted one does not.
func (p EngineStatePayload) Validate() error {
	var errs []error
	switch p.State {
	case EngineStateHalted:
		if p.scoped() {
			errs = append(errs, errors.New("a halted state covers the whole engine and names no scope"))
		}
	case EngineStateDegraded:
		if !p.scoped() {
			errs = append(errs, errors.New("a degraded state names its scope: scope is never inferred from absence"))
		}
	default:
		errs = append(errs, fmt.Errorf("state %q is not a recognised engine state", p.State))
	}
	errs = append(errs, ascendingIDs("scope instrument id", p.ScopeInstrumentIDs)...)
	errs = append(errs, ascendingIDs("scope order id", p.ScopeOrderIDs)...)
	errs = append(errs, ascendingIDs("scope cash component", p.ScopeCashComponents)...)
	switch p.Reason {
	case EngineStateReasonCampaignWithoutProtectiveStop, EngineStateReasonRiskLimitBreached:
		// recognised
	default:
		errs = append(errs, fmt.Errorf("reason %q is not a recognised engine state reason", p.Reason))
	}
	if p.Detail == "" {
		errs = append(errs, errors.New("detail is required: a halt without its figures cannot be checked"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid engine state payload: %w", err)
	}
	return nil
}
