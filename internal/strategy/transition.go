package strategy

import (
	"maps"
	"slices"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// transition owns the candidate state for one input (docs/development.md:
// reducer transactions). All mutation handlers live on this private type;
// only Reducer.Apply can publish their state and decisions via transact.
type transition struct {
	Reducer
	// A capital-safety halt diagnoses pre-existing corruption and must survive
	// rejection (docs/architecture.md: safety invariants). Ordinary decisions
	// returned alongside an error are discarded with the candidate state.
	failureEmissions []event.Envelope
}

// transact publishes one complete transition, or discards it on any error.
// Its builder must validate every payload before returning success; later
// payloads may depend on earlier mutations of the candidate (ADR 0006/0007).
func (r *Reducer) transact(build func(*transition) ([]event.Envelope, error)) ([]event.Envelope, error) {
	tx := &transition{Reducer: r.clone()}
	emissions, err := build(tx)
	if err != nil {
		return tx.failureEmissions, err
	}
	*r = tx.Reducer
	return emissions, nil
}

// clone isolates every mutable object reachable from the reducer. Copying the
// entire input boundary also covers cross-instrument expiries (ADR 0011) and
// account chronology, currency pinning, cash and fill idempotency together.
// time.Time locations are immutable; all owned maps and slices are copied.
func (r *Reducer) clone() Reducer {
	cloned := *r
	cloned.notionalAccount = copyValue(r.notionalAccount)
	cloned.instruments = maps.Clone(r.instruments)
	for id, state := range r.instruments {
		s := *state
		s.n = state.n.Clone()
		s.entryChannel = state.entryChannel.Clone()
		s.exitChannel = state.exitChannel.Clone()
		s.pendingProposal = copyValue(state.pendingProposal)
		s.pendingAddProposal = copyValue(state.pendingAddProposal)
		s.pendingExitProposal = copyValue(state.pendingExitProposal)
		s.campaign = copyValue(state.campaign)
		if s.campaign != nil {
			s.campaign.units = slices.Clone(state.campaign.units)
		}
		cloned.instruments[id] = &s
	}
	cloned.delisted = maps.Clone(r.delisted)
	cloned.acceptedFills = maps.Clone(r.acceptedFills)
	for id, fill := range cloned.acceptedFills {
		fill.unitIDs = slices.Clone(fill.unitIDs)
		cloned.acceptedFills[id] = fill
	}
	return cloned
}

// copyValue copies an optional scalar-only record; its callers must separately
// clone any owned slices or maps (docs/development.md: reducer transactions).
func copyValue[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
