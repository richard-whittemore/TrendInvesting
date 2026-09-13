package strategy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// applyRunCompleted handles event.RunCompletedEventType: the run's input
// stream has ended, so every proposal still outstanding reaches its terminal
// event here.
//
// ADR 0011 gives a proposal the lifetime of one bar, ended by the next
// completed bar for its instrument. The last bar of a run has no next bar, so
// without this the proposal it raised would reach no terminal event at all
// and the journal would end mid-lifecycle — a reviewer counting how many
// proposals became Campaigns would be counting against a denominator the
// journal never closed.
//
// The expiries are the Kind-discriminated ones the reducer already emits,
// distinguished only by event.ExpiryReasonInputStreamEnded, and they are
// stamped at the instant the stream ended rather than at a bar's period end
// because no bar superseded them. That instant is the envelope's own
// EventTime, and the payload states none of its own: see
// event.RunCompletedPayload.
func (r *Reducer) applyRunCompleted(envelope event.Envelope) ([]event.Envelope, error) {
	if !r.configured {
		return nil, errors.New("strategy: received an end-of-stream event before a configuration event; failing closed")
	}
	if envelope.SchemaVersion != event.RunCompletedSchemaVersion {
		return nil, fmt.Errorf("strategy: run completed payload schema version %d does not match the version %d this build requires; an older or newer schema is rejected, never silently upgraded, until an explicit upcaster exists (ADR 0015)", envelope.SchemaVersion, event.RunCompletedSchemaVersion)
	}
	var payload event.RunCompletedPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return nil, fmt.Errorf("strategy: decode run completed payload: %w", err)
	}
	if err := payload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: %w", err)
	}
	completedAt := envelope.EventTime

	// Every expiry below is stamped with this instant, so a stream that
	// claims to have ended before data it already delivered would record an
	// expiry predating its own bar.
	for _, instrumentID := range r.instrumentIDs() {
		last := r.instruments[instrumentID].lastPeriodEnd
		if completedAt.Before(last) {
			return nil, fmt.Errorf("strategy: the input stream is declared to have ended at %s, which precedes the last completed bar for %s (%s)",
				completedAt.Format(time.RFC3339), instrumentID, last.Format(time.RFC3339))
		}
	}

	r.streamEnded = true

	var emitted []event.Envelope
	for _, instrumentID := range r.instrumentIDs() {
		expiries, err := r.expireOutstandingProposals(instrumentID, completedAt, envelope)
		if err != nil {
			return nil, err
		}
		emitted = append(emitted, expiries...)
	}
	return emitted, nil
}

// instrumentIDs returns the instruments this reducer holds state for, in
// ascending order. The state is held in a map, and a journal's decision order
// must not depend on Go's map iteration order (.greptile/rules.md:
// determinism).
func (r *Reducer) instrumentIDs() []string {
	ids := make([]string, 0, len(r.instruments))
	for id := range r.instruments {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// expireOutstandingProposals ends whatever one instrument still has pending,
// in ADR 0010's decision order: exits before Adds before entries. At most one
// of the three can be outstanding at a time in practice, so the order is a
// statement rather than a behaviour any test can observe on one instrument.
func (r *Reducer) expireOutstandingProposals(instrumentID string, completedAt time.Time, input event.Envelope) ([]event.Envelope, error) {
	state := r.instruments[instrumentID]
	var emitted []event.Envelope

	if pending := state.pendingExitProposal; pending != nil {
		state.pendingExitProposal = nil
		envelope, err := r.emitEndOfStreamExpiry(endOfStreamExpiry{
			idKind:         "exit-proposal-expired-at-end-of-stream",
			instrumentID:   instrumentID,
			kind:           event.ProposalKindExit,
			rule:           event.RuleExitProposalExpiresWithItsBar,
			proposalID:     pending.proposalID,
			periodEnd:      pending.periodEnd,
			earliestFillAt: pending.earliestFillAt,
			quantity:       pending.quantity,
			level:          pending.level,
		}, completedAt, input)
		if err != nil {
			return nil, err
		}
		emitted = append(emitted, envelope)
	}

	if pending := state.pendingAddProposal; pending != nil {
		state.pendingAddProposal = nil
		envelope, err := r.emitEndOfStreamExpiry(endOfStreamExpiry{
			idKind:         "add-proposal-expired-at-end-of-stream",
			instrumentID:   instrumentID,
			kind:           event.ProposalKindAdd,
			rule:           event.RuleAddProposalExpiresWithItsBar,
			proposalID:     pending.proposalID,
			periodEnd:      pending.periodEnd,
			earliestFillAt: pending.earliestFillAt,
			quantity:       pending.quantity,
			level:          pending.level,
		}, completedAt, input)
		if err != nil {
			return nil, err
		}
		emitted = append(emitted, envelope)
	}

	if pending := state.pendingProposal; pending != nil {
		state.pendingProposal = nil
		envelope, err := r.emitEndOfStreamExpiry(endOfStreamExpiry{
			idKind:         "proposal-expired-at-end-of-stream",
			instrumentID:   instrumentID,
			kind:           event.ProposalKindEntry,
			rule:           event.RuleSignalExpiresWithItsBar,
			proposalID:     pending.proposalID,
			signalID:       pending.signalID,
			periodEnd:      pending.periodEnd,
			earliestFillAt: pending.earliestFillAt,
			quantity:       pending.quantity,
			level:          pending.entryLevel,
		}, completedAt, input)
		if err != nil {
			return nil, err
		}
		emitted = append(emitted, envelope)
	}

	return emitted, nil
}

// endOfStreamExpiry is what the three pending-proposal kinds differ in. The
// rest of the expiry — the reason, the instant, the event type and schema —
// is identical across them, which is why one builder serves all three.
type endOfStreamExpiry struct {
	idKind         string
	instrumentID   string
	kind           string
	rule           string
	proposalID     string
	signalID       string
	periodEnd      time.Time
	earliestFillAt time.Time
	quantity       int64
	level          float64
}

func (r *Reducer) emitEndOfStreamExpiry(expiry endOfStreamExpiry, completedAt time.Time, input event.Envelope) (event.Envelope, error) {
	payload := event.ProposalExpiredPayload{
		InstrumentID:   expiry.instrumentID,
		Kind:           expiry.kind,
		ProposalID:     expiry.proposalID,
		SignalID:       expiry.signalID,
		PeriodEnd:      expiry.periodEnd,
		ExpiredAt:      completedAt,
		EarliestFillAt: expiry.earliestFillAt,
		Rule:           expiry.rule,
		ADR:            event.ADRSignalExpiry,
		Reason:         event.ExpiryReasonInputStreamEnded,
		Quantity:       expiry.quantity,
		Level:          expiry.level,
	}
	if err := payload.Validate(); err != nil {
		return event.Envelope{}, fmt.Errorf("strategy: built invalid proposal expired payload: %w", err)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		// Unreachable, for the same reason expireEntryProposal's marshal guard
		// is.
		return event.Envelope{}, fmt.Errorf("strategy: marshal proposal expired payload: %w", err)
	}
	// Keyed to the instant the stream ended rather than to a bar, and to its
	// own kind: a run ends once, so one instrument can produce at most one
	// end-of-stream expiry of each kind.
	return r.stamp(
		decisionID(expiry.idKind, expiry.instrumentID, completedAt),
		event.ProposalExpiredEventType, event.ProposalExpiredSchemaVersion,
		completedAt, input, payloadBytes,
	), nil
}
