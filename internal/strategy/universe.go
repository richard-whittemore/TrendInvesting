package strategy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/universe"
)

// This file holds ADR 0009's Baseline universe: a declared classification
// fact, applied here (applyInstrumentClassification), and the monthly,
// point-in-time evaluation that combines it with what this reducer already
// tracks from completed bars (evaluateUniverse, called from session.go's
// applySessionClosed). The rule that matters most, CONTEXT.md's "Eligible"
// and ADR 0009's own words: losing eligibility never closes an open
// Campaign. Nothing in this file ever reads or clears a campaign field;
// universeIneligible's one caller is sizeUnit's NEW-entry path, never
// evaluateCampaign, evaluateAdd, or any exit path.

// pendingClassification is one declared classification fact not yet in
// force: ADR 0009 is point-in-time, so a fact effective in the future must
// not be read by any evaluation before its own EffectiveAt arrives (a
// Greptile review finding on this ticket's PR, valid: storing a
// future-effective declaration immediately let it decide an earlier
// Session's eligibility). classificationRecord.pending holds these,
// ascending by EffectiveAt, until evaluateUniverse promotes each one whose
// EffectiveAt has arrived.
type pendingClassification struct {
	classification universe.Classification
	effectiveAt    time.Time
}

// classificationRecord is one instrument's classification history: the
// declaration currently in force (active, as of activeEffectiveAt — the
// zero time.Time before any declaration has ever taken effect), and any
// later declarations still waiting for their own EffectiveAt (pending, in
// ascending order).
//
// Every declaration is admitted here in the order it is received
// (applyInstrumentClassification's own chronology check), but only takes
// effect — becomes active, and is read by evaluateUniverse or
// universeIneligible — at the first Session close at or after its own
// EffectiveAt (evaluateUniverse's own doc comment). Two or more pending
// declarations can be promoted at once if a gap in Sessions jumps past
// several of their effective dates together; only the LAST one promoted at
// any such close becomes active, since none of the ones it supersedes was
// ever active for any Session's own evaluation.
//
// pendingEvaluation is ADR 0009's amendment of 2026-09-25 (the owner's
// decision): true from the moment a declaration is PROMOTED to active —
// never merely accepted while still pending — until evaluateUniverse next
// evaluates it, so "an instrument classified since its last evaluation is
// evaluated at the next Session close" holds without waiting for the next
// calendar month. false once evaluated; the monthly cadence
// (universe.FirstOfMonth) governs every later re-evaluation, until another
// declaration is promoted and sets it again.
type classificationRecord struct {
	active            universe.Classification
	activeEffectiveAt time.Time
	pending           []pendingClassification
	pendingEvaluation bool
}

// lastDeclaredEffectiveAt is the EffectiveAt of the most recently accepted
// declaration for this instrument, active or still pending — what a new
// declaration's own chronology is checked against
// (applyInstrumentClassification), so a classification cannot be restated
// out of order regardless of whether the one it would regress has taken
// effect yet.
func (r classificationRecord) lastDeclaredEffectiveAt() time.Time {
	if n := len(r.pending); n > 0 {
		return r.pending[n-1].effectiveAt
	}
	return r.activeEffectiveAt
}

// universeState is one instrument's most recently recorded ADR 0009
// eligibility verdict (instrumentState's own doc comment).
type universeState struct {
	evaluated bool
	eligible  bool
}

// securityTypeFor maps the wire contract's SecurityType string onto
// internal/universe's own enumeration, mirroring reducer.go's
// sizingModeFor: internal/universe stays free of the wire contract, so
// exactly one place bridges the two, and it fails closed on a value
// InstrumentClassificationPayload.Validate has already required to be
// recognised but that this reducer's own switch does not (yet) know —
// unreachable in practice, guarded anyway, matching this package's
// fail-closed style.
func securityTypeFor(securityType string) (universe.SecurityType, error) {
	switch securityType {
	case event.SecurityTypeCommonStock:
		return universe.SecurityTypeCommonStock, nil
	case event.SecurityTypeETF:
		return universe.SecurityTypeETF, nil
	case event.SecurityTypeADR:
		return universe.SecurityTypeADR, nil
	case event.SecurityTypeSPAC:
		return universe.SecurityTypeSPAC, nil
	default:
		return "", fmt.Errorf("strategy: security type %q is not a recognised security type; failing closed rather than classifying it as common stock", securityType)
	}
}

// applyInstrumentClassification handles
// event.MarketInstrumentClassificationEventType: a declared fact about an
// instrument's own listing (ADR 0009), the classification half of ADR
// 0009's eligibility test — the price/volume/history half is derived from
// completed bars this reducer already holds (evaluateUniverse).
//
// Unlike a corporate action, a classification carries no chronology
// constraint against the open Session's bars: it does not retroactively
// change any decision already made. It is admitted here the moment it
// arrives, in r.classifications' pending queue, but does not take effect —
// is not read by any evaluation — until the first Session close at or
// after its own EffectiveAt (evaluateUniverse; ADR 0009 is point-in-time, so
// a fact effective in the future must never decide an earlier Session's
// eligibility). EffectiveAt is checked only against the instrument's own
// most recently accepted declaration (classificationRecord.
// lastDeclaredEffectiveAt), active or still pending, so the record itself
// cannot regress — a producer restating an older fact after a newer one
// would corrupt the audit trail this field exists to keep honest.
func (r *transition) applyInstrumentClassification(envelope event.Envelope) ([]event.Envelope, error) {
	if !r.configured {
		return nil, errors.New("strategy: received an instrument classification before a configuration event; failing closed")
	}
	if envelope.SchemaVersion != event.MarketInstrumentClassificationSchemaVersion {
		return nil, fmt.Errorf("strategy: instrument classification payload schema version %d does not match the version %d this build requires; an older or newer schema is rejected, never silently upgraded, until an explicit upcaster exists (ADR 0015)", envelope.SchemaVersion, event.MarketInstrumentClassificationSchemaVersion)
	}
	var payload event.InstrumentClassificationPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return nil, fmt.Errorf("strategy: decode instrument classification payload: %w", err)
	}
	if err := payload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: invalid instrument classification payload: %w", err)
	}
	securityType, err := securityTypeFor(payload.SecurityType)
	if err != nil {
		return nil, err
	}
	record := r.classifications[payload.InstrumentID]
	if lastDeclared := record.lastDeclaredEffectiveAt(); !lastDeclared.IsZero() && payload.EffectiveAt.Before(lastDeclared) {
		return nil, fmt.Errorf("strategy: instrument %q: classification effective at %s predates its own most recent declaration at %s; a classification cannot be restated out of order",
			payload.InstrumentID, payload.EffectiveAt.Format(time.RFC3339), lastDeclared.Format(time.RFC3339))
	}
	record.pending = append(record.pending, pendingClassification{
		classification: universe.Classification{
			SecurityType:      securityType,
			USPrimaryExchange: payload.USPrimaryExchange,
		},
		effectiveAt: payload.EffectiveAt,
	})
	r.classifications[payload.InstrumentID] = record
	return nil, nil
}

// universeGateOn reports whether ADR 0009's universe gate is on for this
// run, as amended 2026-09-25 (the owner's decision): the three thresholds
// switch together, so reading one field's sign is sufficient —
// ConfigurationPayload.Validate has already refused any other combination.
// Off (every existing fixture, golden journal and decision-corpus scenario)
// gates nothing at all: evaluateUniverse and universeIneligible are both
// unconditional no-ops.
func (r *transition) universeGateOn() bool {
	return r.universeCriteria.MinPrice > 0
}

// evaluateUniverse runs ADR 0009's point-in-time eligibility evaluation,
// for every instrument this run's universe port has ever classified
// (Reducer.classifications) — called from session.go's applySessionClosed,
// once per Session, before its Adds and entries are decided. A no-op,
// unconditionally, while the universe gate is off (universeGateOn false):
// no evaluation, no decision, nothing recorded.
//
// # Promotion: a declaration takes effect only once its own date arrives
//
// Before anything else, every instrument's pending queue is checked against
// THIS Session's own period end: every pending declaration whose EffectiveAt
// is at or before it is promoted to active, in ascending order, oldest
// first. This is what keeps eligibility point-in-time (ADR 0009): a
// declaration effective in the future is inert until a Session close
// actually reaches its date, however long before that it was received. An
// instrument with no active declaration at all yet (activeEffectiveAt still
// the zero time.Time after promotion) has nothing to evaluate and is
// skipped entirely — CONTEXT.md: "Eligible" reads a declared fact, not one
// that has not arrived yet.
//
// # Which Sessions evaluate which instruments (ADR 0009, as amended
// 2026-09-25, the owner's decision)
//
// Per instrument, not per Session: an instrument with an active declaration
// evaluates at this Session's close when EITHER
//
//   - a declaration was just promoted to active this Session close
//     (classificationRecord.pendingEvaluation), which is necessarily true
//     the first time any declaration of its ever takes effect — so the
//     first Session close at or after that, and the next one after any
//     later declaration takes effect in turn, both evaluate it without
//     waiting for a month boundary — OR
//   - universe.FirstOfMonth reports that the CLOSING Session — the one
//     applySessionClosed is deciding, r.sessionPeriodEnd — is the first
//     trading day of its calendar month, purely from the sequence of
//     Sessions this reducer has closed (ADR 0021: Sessions follow one
//     another strictly, so comparing consecutive Sessions is sufficient).
//
// An instrument neither newly promoted nor due for its month is left
// exactly as its last evaluation recorded it: no new decision, and its
// existing state.universe verdict still governs universeIneligible.
//
// # Which instruments evaluate, and which day's figures
//
// Every instrument in r.classifications with an active declaration is
// evaluated, whether or not it has a bar in THIS Session — ADR 0009's
// universe is a fact about the whole declared set, not only today's
// traders. r.stateFor is used rather than peekInstrument/instrument so that
// an instrument classified before its first bar still evaluates, against
// zero history: stateFor returns a freshly initialised, empty
// instrumentState for one this reducer has never seen a bar for, which
// fails ADR 0009's history criterion honestly rather than needing a
// separate "not yet evaluated" case.
//
// Price is the instrument's most recent raw close (ADR 0004, as amended:
// absolute price floors in universe eligibility read the raw view), and the
// dollar-volume window is its raw closes/volumes — both already fed,
// unconditionally, by applyCompletedBar's advance block, the same state ADR
// 0010's ranking tie-break reads (indicator.MedianDollarVolume; this
// reducer keeps exactly one set of these windows, reused here rather than
// duplicated). When the instrument's most recent bar predates this Session
// (it did not trade today), these are simply its last known figures — ADR
// 0009 evaluates from information available at the time, and no later
// figure is used.
//
// # What is recorded
//
// Every instrument evaluated gets exactly one
// event.UniverseEligibilityEventType decision, in ascending instrument
// order (determinism: CONTEXT.md/.greptile/rules.md), and its verdict
// replaces state.universe — read by universeIneligible to gate a NEW entry,
// and by nothing else: an open Campaign is never affected (this file's own
// doc comment).
func (r *transition) evaluateUniverse(envelope event.Envelope) ([]event.Envelope, error) {
	if !r.universeGateOn() {
		return nil, nil
	}
	firstOfMonth := universe.FirstOfMonth(r.sessionPeriodEnd, r.hasClosedSession, r.lastClosedSession)
	ids := make([]string, 0, len(r.classifications))
	for id := range r.classifications {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var emissions []event.Envelope
	for _, id := range ids {
		record := r.classifications[id]

		// Promote every pending declaration due by this Session's own
		// period end, oldest first. Only the last one promoted becomes
		// active: none of the ones it supersedes here was ever active for
		// any Session's own evaluation, so there is nothing of theirs to
		// preserve.
		for len(record.pending) > 0 && !record.pending[0].effectiveAt.After(r.sessionPeriodEnd) {
			record.active = record.pending[0].classification
			record.activeEffectiveAt = record.pending[0].effectiveAt
			record.pending = record.pending[1:]
			record.pendingEvaluation = true
		}
		r.classifications[id] = record

		if record.activeEffectiveAt.IsZero() {
			continue // no declaration has taken effect yet
		}
		if !record.pendingEvaluation && !firstOfMonth {
			continue
		}
		state, err := r.stateFor(id)
		if err != nil {
			return nil, err
		}
		rawCloses := state.rawCloses.Values()
		price := 0.0
		if len(rawCloses) > 0 {
			price = rawCloses[len(rawCloses)-1]
		}
		result := universe.Evaluate(universe.Input{
			Classification:      record.active,
			Price:               price,
			DollarVolumeCloses:  rawCloses,
			DollarVolumeVolumes: state.rawVolumes.Values(),
			CompletedBars:       state.completedBars,
		}, r.universeCriteria)

		state.universe = universeState{evaluated: true, eligible: result.Eligible}
		record.pendingEvaluation = false
		r.classifications[id] = record

		payload := event.UniverseEligibilityPayload{
			InstrumentID:           id,
			PeriodEnd:              r.sessionPeriodEnd,
			Eligible:               result.Eligible,
			ClassificationEligible: result.ClassificationEligible,
			Price:                  price,
			PriceEligible:          result.PriceEligible,
			DollarVolume:           result.DollarVolume,
			DollarVolumeEligible:   result.DollarVolumeEligible,
			CompletedBars:          state.completedBars,
			HistoryEligible:        result.HistoryEligible,
			Rule:                   event.RuleUniverseEligibility,
			ADR:                    event.ADRUniverseEligibility,
		}
		if err := payload.Validate(); err != nil {
			return nil, fmt.Errorf("strategy: built invalid universe eligibility payload: %w", err)
		}
		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("strategy: marshal universe eligibility payload: %w", err)
		}
		emissions = append(emissions, r.stamp(
			decisionID("universe-eligibility", id, r.sessionPeriodEnd),
			event.UniverseEligibilityEventType, event.UniverseEligibilitySchemaVersion,
			r.sessionPeriodEnd, envelope, payloadBytes,
		))
	}
	return emissions, nil
}

// universeIneligible reports whether id is currently excluded from NEW
// Campaigns by ADR 0009 — never from an already open one (this file's own
// doc comment) — and, when it is, a Detail string for the decline this
// gates (event.DeclineReasonIneligible).
//
// Always false while the universe gate is off (universeGateOn false): every
// existing fixture, golden journal and decision-corpus scenario runs this
// way, and nothing is ever gated for them.
//
// While the gate is on (ADR 0009, as amended 2026-09-25, the owner's
// decision), an instrument is not a candidate merely by default: one that
// has never been classified at all, or one that has been classified but
// whose own evaluation has not yet run (state.universe.evaluated false —
// see evaluateUniverse's own doc comment for why the reducer's design
// makes the second case vanishingly rare in practice, without making it
// impossible to state), is declined exactly as one a completed evaluation
// found ineligible is.
func (r *transition) universeIneligible(id string) (ineligible bool, detail string) {
	if !r.universeGateOn() {
		return false, ""
	}
	state := r.peekInstrument(id)
	if state == nil || !state.universe.evaluated {
		// A purely pending declaration (record.activeEffectiveAt still
		// zero) does not count as "classified" here: ADR 0009 is
		// point-in-time, and a fact not yet in effect is exactly as absent,
		// for gating purposes, as one that was never declared at all.
		if record, known := r.classifications[id]; known && !record.activeEffectiveAt.IsZero() {
			return true, fmt.Sprintf("instrument %q is classified but has not yet been evaluated by the Baseline universe (ADR 0009, as amended 2026-09-25): the universe gate is on, and an instrument is not a candidate for a new Campaign until its own evaluation has run", id)
		}
		return true, fmt.Sprintf("instrument %q was never classified for the Baseline universe (ADR 0009, as amended 2026-09-25): the universe gate is on, and an unclassified instrument is not a candidate for a new Campaign", id)
	}
	if state.universe.eligible {
		return false, ""
	}
	return true, fmt.Sprintf("instrument %q was found ineligible for the Baseline universe at its most recent evaluation (ADR 0009)", id)
}
