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
// event; #26 (LEAN transport) had not yet landed a safe-mode/engine-state
// concept at the time this was written. A later ticket that needs an
// engine-state event should reuse THIS type and EngineStatePayload — adding
// a Reason value, the same way CampaignExitedPayload's Reason grows for #13
// and #24 — rather than minting a second one.
const EngineStateEventType = "strategy.engine.state"

// EngineStateSchemaVersion is the current schema version of
// EngineStatePayload, for the Envelope's SchemaVersion field.
const EngineStateSchemaVersion uint32 = 1

// EngineStateHalted is the only State value today: the run must stop rather
// than continue past the violation named in Reason.
const EngineStateHalted = "halted"

// The enumerated reasons the engine can halt. A closed set, not free text,
// for the same reason every other Reason field in this package is: a
// journal must be groupable by it.
const (
	// EngineStateReasonCampaignWithoutProtectiveStop means an open Campaign
	// was found, at the start of a completed bar, without a Protective Stop
	// that is positive and below its entry price (CONTEXT.md: "Every open
	// Campaign has one at all times"). This can only happen if memory was
	// corrupted after a Campaign opened — see
	// internal/strategy/campaign.go's checkCampaignHasAProtectiveStop for
	// why the Campaign struct cannot be built without a valid stop in the
	// first place.
	EngineStateReasonCampaignWithoutProtectiveStop = "campaign-without-protective-stop"
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
}

// Validate checks that State and Reason are each one of their enumerated
// constants, and that Detail is present.
func (p EngineStatePayload) Validate() error {
	var errs []error
	switch p.State {
	case EngineStateHalted:
		// recognised
	default:
		errs = append(errs, fmt.Errorf("state %q is not a recognised engine state", p.State))
	}
	switch p.Reason {
	case EngineStateReasonCampaignWithoutProtectiveStop:
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
