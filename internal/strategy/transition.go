package strategy

import (
	"maps"
	"slices"
	"sort"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// transition owns the candidate state for one input (docs/development.md:
// reducer transactions). All mutation handlers live on this private type;
// only Reducer.Apply can publish their state and decisions via transact.
//
// The candidate is copy-on-write. Scalars, the Notional Account and the
// delisted map are copied eagerly by begin. Instrument state and the
// accepted-fill history grow with the universe and the length of the run, so
// neither is copied per input: the embedded Reducer's instruments and
// acceptedFills are nil for the whole transaction, and every access goes
// through the accessors below. An instrument is deep-copied into touched on
// its first access; a newly accepted fill is buffered in newFills. commit
// writes both overlays into the published maps; a rejection simply drops
// them, so nothing a rejected handler did can reach published state.
type transition struct {
	Reducer
	// baseInstruments and baseFills are the published maps. Handlers never
	// write them; only commit does, after the builder has succeeded.
	baseInstruments map[string]*instrumentState
	baseFills       map[string]acceptedFillState
	// touched holds this transaction's private copy of every instrument it
	// has accessed, and every instrument it has created.
	touched map[string]*instrumentState
	// newFills holds every fill this transaction has accepted.
	newFills map[string]acceptedFillState
	// removedInstrumentIDs holds every instrument id a symbol change has
	// moved away from in this transaction (ADR 0024): commit deletes each one
	// from the published instruments map, after touched has been copied in,
	// so the old id leaves no stale entry behind. No other transition ever
	// removes an instrument.
	removedInstrumentIDs []string
	// A capital-safety halt diagnoses pre-existing corruption and must survive
	// rejection (docs/architecture.md: safety invariants). Ordinary decisions
	// returned alongside an error are discarded with the candidate state.
	failureEmissions []event.Envelope
}

// transact publishes one complete transition, or discards it on any error.
// Its builder must validate every payload before returning success; later
// payloads may depend on earlier mutations of the candidate (ADR 0006/0007).
func (r *Reducer) transact(build func(*transition) ([]event.Envelope, error)) ([]event.Envelope, error) {
	tx := r.begin()
	emissions, err := build(tx)
	if err != nil {
		return tx.failureEmissions, err
	}
	tx.commit(r)
	return emissions, nil
}

// begin opens a transaction over r. Everything reachable from r that is not
// behind the instrument or accepted-fill accessors is copied here; account
// chronology, currency pinning, cash with its fill debits and holds, the delisted and renamed maps and the open
// Session's delisted bars are small and commit together with the instruments and fills (ADR 0007/0009, ADR 0024).
// time.Time locations are immutable and may be shared.
func (r *Reducer) begin() *transition {
	tx := &transition{
		Reducer:         *r,
		baseInstruments: r.instruments,
		baseFills:       r.acceptedFills,
	}
	tx.notionalAccount = copyValue(r.notionalAccount)
	tx.delisted = maps.Clone(r.delisted)
	tx.renamed = maps.Clone(r.renamed)
	// maps.Clone is shallow: classificationRecord.pending is a slice, so
	// each record's own pending queue is cloned too, or a rejected
	// transaction's promotion (evaluateUniverse) could mutate the backing
	// array published state still shares.
	tx.classifications = maps.Clone(r.classifications)
	for id, record := range tx.classifications {
		record.pending = slices.Clone(record.pending)
		tx.classifications[id] = record
	}
	tx.fillDebits = slices.Clone(r.fillDebits)
	tx.holds = slices.Clone(r.holds)
	tx.sessionDelistedBars = slices.Clone(r.sessionDelistedBars)
	tx.instruments = nil
	tx.acceptedFills = nil
	return tx
}

// commit publishes the candidate into r: both overlays are written into the
// published maps, every instrument a symbol change moved away from is
// deleted from the result (ADR 0024), then every eagerly copied field is
// swapped in. NewReducer, the only constructor, allocates both published
// maps.
//
// Deletion runs after the copy, never before: renameInstrument's caller
// still holds the moved state under touched[oldID] until it reassigns it to
// the new id in the same call, so deleting oldID first and copying second
// would risk resurrecting it from a stale touched entry if a future handler
// ever touched the old id again earlier in the same transaction. Running
// last also means a transaction that renames the same id more than once (a
// chain A->B->C) still ends with exactly the final id published.
func (tx *transition) commit(r *Reducer) {
	maps.Copy(tx.baseInstruments, tx.touched)
	maps.Copy(tx.baseFills, tx.newFills)
	for _, id := range tx.removedInstrumentIDs {
		delete(tx.baseInstruments, id)
	}
	*r = tx.Reducer
	r.instruments = tx.baseInstruments
	r.acceptedFills = tx.baseFills
}

// instrument returns this transaction's own copy of an instrument's state,
// deep-copying the published state on first access. Every access that may
// mutate instrument state must come through here, never through a direct
// index of the instruments map (docs/development.md: reducer transactions).
func (tx *transition) instrument(id string) (*instrumentState, bool) {
	if state, ok := tx.touched[id]; ok {
		return state, true
	}
	published, ok := tx.baseInstruments[id]
	if !ok {
		return nil, false
	}
	state := published.clone()
	tx.addInstrument(id, state)
	return state, true
}

// peekInstrument is instrument without the copy, for a read that never
// mutates what it is given: the result may be published state. It is nil for
// an instrument the transaction cannot see.
func (tx *transition) peekInstrument(id string) *instrumentState {
	if state, ok := tx.touched[id]; ok {
		return state
	}
	return tx.baseInstruments[id]
}

// addInstrument records state created by this transaction.
func (tx *transition) addInstrument(id string, state *instrumentState) {
	if tx.touched == nil {
		tx.touched = make(map[string]*instrumentState)
	}
	tx.touched[id] = state
}

// renameInstrument moves state's identity from oldID to newID (ADR 0024): a
// symbol change, not a delisting and a fresh instrument. It records oldID
// for deletion at commit and republishes the same *instrumentState under
// newID — the Campaign, indicator history, universe classification and every
// pending proposal or hold it carries are the SAME value, not a copy, so
// nothing in it needs its own migration.
//
// The caller must already hold its own copy of state from tx.instrument(oldID)
// (never peekInstrument's shared, unowned value), since this transaction is
// about to mutate what oldID maps to.
func (tx *transition) renameInstrument(oldID, newID string, state *instrumentState) {
	delete(tx.touched, oldID)
	tx.addInstrument(newID, state)
	tx.removedInstrumentIDs = append(tx.removedInstrumentIDs, oldID)
}

// instrumentIDs returns every instrument this transaction can see, published
// or created by it, in ascending order, excluding any id this same
// transaction has renamed away (ADR 0024) — commit has not yet deleted it
// from baseInstruments, but it is no longer this transaction's to see. The
// state is held in maps, and a journal's decision order must not depend on
// Go's map iteration order (.greptile/rules.md: determinism).
func (tx *transition) instrumentIDs() []string {
	ids := make([]string, 0, len(tx.baseInstruments)+len(tx.touched))
	for id := range tx.baseInstruments {
		ids = append(ids, id)
	}
	for id := range tx.touched {
		if _, published := tx.baseInstruments[id]; !published {
			ids = append(ids, id)
		}
	}
	if len(tx.removedInstrumentIDs) > 0 {
		removed := make(map[string]bool, len(tx.removedInstrumentIDs))
		for _, id := range tx.removedInstrumentIDs {
			removed[id] = true
		}
		ids = slices.DeleteFunc(ids, func(id string) bool { return removed[id] })
	}
	sort.Strings(ids)
	return ids
}

// acceptedFill looks fillID up in this transaction's accepted fills first,
// then in the whole run's published history (campaign.go: acceptedFillState).
func (tx *transition) acceptedFill(fillID string) (acceptedFillState, bool) {
	if fill, ok := tx.newFills[fillID]; ok {
		return fill, true
	}
	fill, ok := tx.baseFills[fillID]
	return fill, ok
}

// recordAcceptedFill buffers an accepted fill until commit. A recorded
// acceptedFillState, including its unitIDs, is never mutated afterwards,
// which is why the published history can be shared rather than copied.
func (tx *transition) recordAcceptedFill(fillID string, fill acceptedFillState) {
	if tx.newFills == nil {
		tx.newFills = make(map[string]acceptedFillState)
	}
	tx.newFills[fillID] = fill
}

// clone deep-copies one instrument's state: indicator ring and seed buffers,
// the Signal awaiting its Session, every pending proposal, and the Campaign
// with its Units.
func (s *instrumentState) clone() *instrumentState {
	cloned := *s
	cloned.n = s.n.Clone()
	cloned.entryChannel = s.entryChannel.Clone()
	cloned.exitChannel = s.exitChannel.Clone()
	cloned.splitAdjustedCloses = s.splitAdjustedCloses.Clone()
	cloned.rawCloses = s.rawCloses.Clone()
	cloned.rawVolumes = s.rawVolumes.Clone()
	cloned.pendingSignal = copyValue(s.pendingSignal)
	cloned.pendingProposal = copyValue(s.pendingProposal)
	cloned.pendingAddProposal = copyValue(s.pendingAddProposal)
	cloned.pendingExitProposal = copyValue(s.pendingExitProposal)
	cloned.campaign = copyValue(s.campaign)
	if cloned.campaign != nil {
		cloned.campaign.units = slices.Clone(s.campaign.units)
	}
	return &cloned
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
