package strategy

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds everything #11 adds to the reducer: the two pieces of
// per-instrument position state, and the handling of the one new input event
// that may change them.
//
// The invariant it exists to enforce, stated once here because every function
// below is a consequence of it: **position state changes only from recorded
// fill events, never at order-submission time and never from a quoted price.**
// docs/architecture.md — "Go never assumes an intended order was filled" — and
// .greptile/rules.md, which names "request-driven state" as one of the failure
// modes that already occurred in the predecessor prototype. Emitting a trade
// proposal therefore moves nothing except a note that the proposal is
// outstanding; only applyFill can bring a Campaign into being.

// campaignState is one instrument's open Campaign (CONTEXT.md: "Campaign" —
// the complete life of a position in one instrument, from the first Unit's
// entry to the exit of the last).
//
// **campaignN, unitQuantity and entryPrice are the frozen numbers ADR 0006
// requires, and the whole Add Ladder and Stop Ladder are derivable from them.**
// Under the Baseline a Unit is added every half a campaign N above the entry
// and each Protective Stop sits stopMultiple campaign N below its Unit's entry
// (The Turtle Rules p.19-20, p.22): every rung of both ladders is arithmetic
// over those three numbers and the frozen stopMultiple, with no recomputation
// of N while the Campaign is open. That is what makes replay reconstruct the
// same ladders rather than re-deriving different ones from the bars — the
// reason ADR 0006 requires the frozen values to be journaled, which
// event.CampaignOpenedPayload does.
//
// entryPrice is the price that ACTUALLY filled, including the producer's
// slippage (ADR 0013 measures the Add Ladder from the slipped fill, as Faith
// specifies [T p.19]). It is deliberately not the trade proposal's entry
// level, which was the level the Signal fired at.
type campaignState struct {
	campaignID     string
	proposalID     string
	signalID       string
	openingFillID  string
	direction      string
	campaignN      float64
	unitQuantity   int64
	filledQuantity int64
	entryPrice     float64
	stopMultiple   float64
	protectiveStop float64
	openedAt       time.Time
	units          int
}

// pendingProposalState is a trade proposal that has been emitted and not yet
// resolved. It is NOT position state: nothing about the instrument's position
// depends on it, and a Campaign is never derived from it alone.
//
// It exists for exactly two reasons. First, a fill has to be checkable against
// something: a fill naming a proposal this reducer never made is a material
// reconciliation difference (docs/architecture.md's safety invariants), and
// without this record every fill would have to be taken on trust. Second, it
// carries the frozen figures — N, the Unit quantity, the Stop Multiple — from
// the bar that produced the proposal to the fill that executes it, so ADR
// 0006's freeze is the proposal's N and not a recomputation at fill time.
//
// It lives for one bar. ADR 0011 gives a Signal the lifetime of its bar, and
// the proposal a Signal produced inherits it: the next completed bar for the
// instrument supersedes the proposal (see Reducer.expireProposal).
type pendingProposalState struct {
	proposalID string
	signalID   string
	periodEnd  time.Time
	// earliestFillAt is the period end of the bar BEFORE the decision bar —
	// which is to say the moment the decision bar opened, and so the earliest
	// instant at which an order for this proposal could have executed. See
	// applyFill for why the bound is this rather than periodEnd.
	//
	// It is the zero time if the decision bar was the instrument's first,
	// leaving such a proposal bounded above only. That cannot arise from this
	// reducer — a proposal requires a Signal, which requires both a warm N
	// (twenty completed bars) and a warm Entry Channel, so at least twenty
	// bars always precede a decision bar — and the zero value degrades
	// correctly rather than needing a special case: every real timestamp is
	// after it, so the lower bound simply does not bind.
	earliestFillAt time.Time
	direction      string
	quantity       int64
	n              float64
	stopMultiple   float64
	entryLevel     float64
}

// rememberPendingProposal records the trade proposal just emitted for this
// instrument as outstanding, and does nothing at all for any other emission.
//
// The single most important thing about this function is what it does not do:
// no Campaign, no position, no Protective Stop and no Unit count comes into
// being here. A proposal that is never filled leaves nothing behind but the
// expiry event, and TestOnlyAFillOpensACampaignNotTheProposalThatPrecededIt is
// the fixture that fails if that ever stops being true.
//
// It reads the figures back out of the emitted payload rather than taking them
// from the sizing step's locals, so what the reducer remembers as pending is
// literally what it journalled; the two cannot drift apart.
//
// earliestFillAt must be the period end of the bar preceding the decision bar,
// read before applyCompletedBar's advance block overwrites it.
func (r *Reducer) rememberPendingProposal(state *instrumentState, emitted event.Envelope, earliestFillAt time.Time) error {
	if emitted.Type != event.TradeProposalEventType {
		// A decline (#10) proposes nothing, so there is nothing to fill and
		// nothing to expire.
		return nil
	}
	var proposal event.TradeProposalPayload
	if err := json.Unmarshal(emitted.Payload, &proposal); err != nil {
		// Unreachable: the payload was marshalled from a validated
		// TradeProposalPayload a few lines earlier in the same Apply call.
		// Failing closed anyway rather than storing a half-decoded proposal
		// that a later fill would be checked against.
		return fmt.Errorf("strategy: decode the trade proposal just emitted: %w", err)
	}
	state.pendingProposal = &pendingProposalState{
		proposalID:     emitted.ID,
		signalID:       proposal.SignalID,
		periodEnd:      proposal.PeriodEnd,
		earliestFillAt: earliestFillAt,
		direction:      proposal.Direction,
		quantity:       proposal.Quantity,
		n:              proposal.N,
		stopMultiple:   proposal.StopMultiple,
		entryLevel:     proposal.EntryLevel,
	}
	return nil
}

// expireProposal ends an outstanding proposal that the next completed bar has
// superseded, and returns the event that records it.
//
// ADR 0011: a Signal belongs to one bar and expires with it — "never enter a
// Tier A stock without first checking that it still belongs in Tier A" — and
// the Baseline holds no pending-Signal memory. The proposal a Signal produced
// inherits that lifetime, so a bar arriving with the previous bar's proposal
// still outstanding ends it.
//
// Emitting the expiry rather than dropping it silently is a deliberate choice.
// It makes the journal able to explain applyFill's rejection of a fill that
// arrives after its proposal has gone: without the record, "this proposal
// expired" and "this proposal never existed" are the same absence, and the
// error would name a proposal the journal appears never to have made. It also
// completes the lifecycle #10 began — a Signal is never followed by silence,
// and now neither is a proposal: every one reaches a Campaign or an expiry.
func (r *Reducer) expireProposal(state *instrumentState, bar event.CompletedBarPayload, input event.Envelope) (event.Envelope, error) {
	pending := state.pendingProposal
	state.pendingProposal = nil

	payload := event.ProposalExpiredPayload{
		InstrumentID: bar.InstrumentID,
		ProposalID:   pending.proposalID,
		SignalID:     pending.signalID,
		PeriodEnd:    pending.periodEnd,
		ExpiredAt:    bar.PeriodEnd,
		Rule:         event.RuleSignalExpiresWithItsBar,
		ADR:          event.ADRSignalExpiry,
		Reason:       event.ExpiryReasonSupersededByNextBar,
		Quantity:     pending.quantity,
		EntryLevel:   pending.entryLevel,
	}
	if err := payload.Validate(); err != nil {
		return event.Envelope{}, fmt.Errorf("strategy: built invalid proposal expired payload: %w", err)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		// Unreachable: every field is a string, an int64, a float64 or a
		// time.Time, none of which can fail to marshal. Failing closed rather
		// than panicking, in case the payload ever grows a field that can.
		return event.Envelope{}, fmt.Errorf("strategy: marshal proposal expired payload: %w", err)
	}
	// Keyed to the bar that superseded the proposal, not to the proposal's own
	// bar: at most one expiry can happen per (instrument, completed bar), so
	// that pair identifies it uniquely — the same reasoning as decisionID's.
	return r.stamp(
		decisionID("proposal-expired", bar.InstrumentID, bar.PeriodEnd),
		event.ProposalExpiredEventType, event.ProposalExpiredSchemaVersion,
		bar.PeriodEnd, input, payloadBytes,
	), nil
}

// applyFill handles event.FillEventType: the only input in this system that
// may change position state.
//
// A fill is an external fact the reducer did not produce and cannot re-derive
// — from a fixture, from #18's simulator, or from the LEAN adapter (#30) — so
// every check here is about whether the fact is reconcilable with what this
// strategy actually proposed, never about whether it "should" have happened.
// Each failure stops the run rather than being absorbed: docs/architecture.md
// requires material reconciliation differences to force safe mode, and a
// reducer that quietly ignored an unmatched fill would be holding a position
// the journal does not know about.
//
// The rules, in the order they are applied:
//
//  1. Schema and payload validity, like every other input (ADR 0015).
//  2. An instrument this reducer has never evaluated can have no proposal
//     outstanding, so any fill for it is unmatched.
//  3. If a Campaign is already open, the fill is either a duplicate delivery
//     of the one that opened it — an idempotent no-op, since duplicate
//     identifiers must be idempotent (docs/architecture.md) — or something
//     this ticket deliberately refuses (see applyFillToOpenCampaign).
//  4. Otherwise the fill must match the outstanding proposal: the same
//     proposal, the same direction, no more than the quantity that was sized,
//     and a timestamp inside the window in which an order for it could have
//     executed.
//
// **That window is (the previous bar's period end, the next bar's arrival),
// and it is deliberately not anchored on the proposal's own period end.** ADR
// 0005 makes the entry a resting order that fills *inside* the breakout bar,
// so a fill timestamped before the decision bar's close is the ordinary
// backtest case rather than an anomaly; live, the order rests into the
// following session and fills after it. What cannot happen is a fill from
// before the decision bar even opened — no order for that proposal could have
// existed then, and accepting one would journal a Campaign whose identity,
// OpenedAt and EventTime predate the bar whose data produced the decision
// authorising it. That is the lower bound checked below (a PR #69 review
// finding, rebounded: the finding proposed the proposal's period end, which
// would have rejected every legitimate intrabar fill). The upper bound needs
// no check because it is structural — the next completed bar for the
// instrument expires the proposal, so a fill arriving after it finds nothing
// pending and is rejected by that path.
//
// What a fill deliberately is NOT checked against: the proposal's entry level.
// ADR 0005 makes a long entry fill at max(level, open) and ADR 0013 pushes it
// further against the trader, so a fill below the level would indeed indicate
// a producer that broke the fill model — but a live venue may legitimately
// improve on a level, and this reducer's job is to record the fact it was
// handed, not to police the producer's model. The recorded price is what every
// later ladder is measured from either way.
func (r *Reducer) applyFill(envelope event.Envelope) ([]event.Envelope, error) {
	if !r.configured {
		return nil, errors.New("strategy: received a fill before a configuration event; failing closed")
	}
	if envelope.SchemaVersion != event.FillSchemaVersion {
		return nil, fmt.Errorf("strategy: fill payload schema version %d does not match the version %d this build requires; an older or newer schema is rejected, never silently upgraded, until an explicit upcaster exists (ADR 0015)", envelope.SchemaVersion, event.FillSchemaVersion)
	}

	var fill event.FillPayload
	if err := json.Unmarshal(envelope.Payload, &fill); err != nil {
		return nil, fmt.Errorf("strategy: decode fill payload: %w", err)
	}
	if err := fill.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: invalid fill payload: %w", err)
	}

	// Deliberately a plain lookup rather than stateFor: an instrument the
	// reducer has never seen a bar for cannot have been proposed for, and
	// creating state here would make the reducer look as though it had.
	state, known := r.instruments[fill.InstrumentID]
	if !known {
		return nil, fmt.Errorf("strategy: fill %q names proposal %q for instrument %q, which this reducer has never evaluated; a fill for an order this strategy never proposed is a reconciliation failure, not something to absorb (docs/architecture.md)",
			fill.FillID, fill.ProposalID, fill.InstrumentID)
	}

	if state.campaign != nil {
		return applyFillToOpenCampaign(state.campaign, fill)
	}

	pending := state.pendingProposal
	if pending == nil {
		return nil, fmt.Errorf("strategy: instrument %q: fill %q names proposal %q, but there is no pending trade proposal for it; a proposal expires with its bar (ADR 0011, see the strategy.proposal.expired event in the journal) and a fill for an order this strategy is no longer offering is a reconciliation failure (docs/architecture.md)",
			fill.InstrumentID, fill.FillID, fill.ProposalID)
	}
	if pending.proposalID != fill.ProposalID {
		return nil, fmt.Errorf("strategy: instrument %q: fill %q names proposal %q, but the pending trade proposal is %q; a fill for an order this strategy never proposed is a reconciliation failure (docs/architecture.md)",
			fill.InstrumentID, fill.FillID, fill.ProposalID, pending.proposalID)
	}
	if fill.Direction != pending.direction {
		// Currently unreachable from a valid payload: the Baseline is
		// long-only, so event.DirectionLong is the only direction
		// FillPayload.Validate and TradeProposalPayload.Validate accept, and a
		// mismatch is rejected before this point. Kept because the rule is
		// about reconciliation rather than about the payload contract — a
		// short fill against a long proposal is a different position, not a
		// variant of the same one — and because it must already be in place on
		// the day shorts are added rather than being remembered then.
		return nil, fmt.Errorf("strategy: instrument %q: fill %q is %s but proposal %q is %s; a fill in the opposite direction is a different position, not the proposed one",
			fill.InstrumentID, fill.FillID, fill.Direction, fill.ProposalID, pending.direction)
	}
	if fill.Quantity > pending.quantity {
		return nil, fmt.Errorf("strategy: instrument %q: fill %q executed %d against proposal %q, which sized %d; an over-execution risks more than the unit that was sized (ADR 0003) and is never absorbed",
			fill.InstrumentID, fill.FillID, fill.Quantity, fill.ProposalID, pending.quantity)
	}
	// The lower bound on the window described above. Strict: a fill stamped
	// exactly at the previous bar's period end is at the instant the decision
	// bar opened, before which no order for this proposal existed. The zero
	// bound (documented on pendingProposalState.earliestFillAt) needs no
	// special case: every real timestamp is after it.
	if !fill.FilledAt.After(pending.earliestFillAt) {
		return nil, fmt.Errorf("strategy: instrument %q: fill %q is timestamped %s, which predates the bar in which an order for proposal %q could have executed (that bar opened at %s and ended at %s); a campaign may not be opened by an execution older than the decision that authorised it",
			fill.InstrumentID, fill.FillID, fill.FilledAt.Format(time.RFC3339),
			fill.ProposalID, pending.earliestFillAt.Format(time.RFC3339), pending.periodEnd.Format(time.RFC3339))
	}

	return r.openCampaign(state, pending, fill, envelope)
}

// applyFillToOpenCampaign resolves a fill that arrives for an instrument
// already in a Campaign.
//
// Exactly one case is benign: the same fill delivered twice. Duplicate
// delivery is expected from any transport and docs/architecture.md requires
// duplicate identifiers to be idempotent, so a re-delivery emits nothing and
// errors nothing — the Campaign that already exists is the correct outcome.
//
// The other two cases fail closed:
//
//   - A second, different fill for the same proposal (a further partial fill).
//     Accumulating successive partials into one Campaign is deferred to its
//     own issue; until it lands, rejecting is the only safe answer, because
//     the alternative — opening a second Campaign for the same instrument —
//     would double the position while every cap and ladder still counted one.
//   - A fill naming a different proposal. The instrument is already committed;
//     a fill for some other order it never had outstanding is a reconciliation
//     failure.
//
// Idempotency means "the same fact delivered twice", not "any fact carrying an
// identifier already seen". A producer that reuses a fill id for a different
// execution is a defect, and treating it as a duplicate would silently discard
// a real execution — so the contents are compared, not just the id.
func applyFillToOpenCampaign(campaign *campaignState, fill event.FillPayload) ([]event.Envelope, error) {
	if campaign.proposalID != fill.ProposalID {
		return nil, fmt.Errorf("strategy: instrument %q: fill %q names proposal %q, but campaign %q is already open from proposal %q; a fill for an order this strategy never proposed is a reconciliation failure (docs/architecture.md)",
			fill.InstrumentID, fill.FillID, fill.ProposalID, campaign.campaignID, campaign.proposalID)
	}
	if campaign.openingFillID != fill.FillID {
		return nil, fmt.Errorf("strategy: instrument %q: campaign %q has already opened from fill %q, and fill %q is a second, different execution of the same proposal; accumulating successive partial fills into one campaign is deferred to its own issue, and opening a second campaign instead would double the position while every cap and ladder still counted one",
			fill.InstrumentID, campaign.campaignID, campaign.openingFillID, fill.FillID)
	}
	if fill.Quantity != campaign.filledQuantity || fill.Price != campaign.entryPrice ||
		fill.Direction != campaign.direction || !fill.FilledAt.Equal(campaign.openedAt) {
		return nil, fmt.Errorf("strategy: instrument %q: fill %q was already recorded as campaign %q's opening fill, but this delivery's quantity, price, direction or timestamp differ from it (%d at %v %s on %s, against the recorded %d at %v %s on %s); a reused fill identifier carrying different contents is a reconciliation failure, not a duplicate delivery",
			fill.InstrumentID, fill.FillID, campaign.campaignID,
			fill.Quantity, fill.Price, fill.Direction, fill.FilledAt.Format(time.RFC3339),
			campaign.filledQuantity, campaign.entryPrice, campaign.direction, campaign.openedAt.Format(time.RFC3339))
	}
	// The duplicate delivery of an execution already recorded: nothing to do,
	// and nothing to complain about.
	return nil, nil
}

// openCampaign brings a Campaign into being from the fill that executed its
// proposal, and returns the one decision event that records it.
//
// The campaign N and the Unit share count are taken from the proposal, not
// recomputed: ADR 0006 freezes them at first entry, and recomputing at fill
// time would make the frozen values depend on when the fill arrived. The
// entry price and the filled quantity are taken from the fill, not from the
// proposal: what actually executed is the only thing the position consists of.
//
// A partial fill opens a Campaign for the filled quantity, exactly as the
// ticket requires, while the Unit share count stays frozen at the full
// proposed size — the Unit is the risk measure the caps are counted in (ADR
// 0010), so resizing it would change what "one Unit" means partway through a
// Campaign.
//
// A fill that produces an unreachable Protective Stop (at or below zero) fails
// event.CampaignOpenedPayload.Validate and stops the run. That cannot arise
// from a producer honouring ADR 0005, which fills a long entry at or above the
// proposed level, on a proposal whose stop intent was already required to be
// positive — so reaching it means the position could not be protected, which
// is a condition to fail on rather than to journal.
func (r *Reducer) openCampaign(state *instrumentState, pending *pendingProposalState, fill event.FillPayload, input event.Envelope) ([]event.Envelope, error) {
	// The Campaign's identity is the instrument plus the moment it came into
	// being, which is the opening fill's timestamp. Deterministic, so replay
	// reconstructs the same identity with no randomness or wall-clock read;
	// the Campaign-opened envelope carries the same value as its own ID.
	campaignID := decisionID("campaign", fill.InstrumentID, fill.FilledAt)

	// The expression order event.CampaignOpenedPayload.Validate re-derives the
	// stop in, so the two agree bit for bit (CONTEXT.md: "Protective Stop";
	// The Turtle Rules p.22's 2N stop in the Baseline, measured from the
	// actual fill).
	protectiveStop := fill.Price - pending.stopMultiple*pending.n

	payload := event.CampaignOpenedPayload{
		CampaignID:     campaignID,
		InstrumentID:   fill.InstrumentID,
		ProposalID:     pending.proposalID,
		SignalID:       pending.signalID,
		FillID:         fill.FillID,
		Rule:           event.RuleCampaignOpenedFromFill,
		ADR:            event.ADRCampaignFrozenAtEntry,
		Direction:      fill.Direction,
		CampaignN:      pending.n,
		UnitQuantity:   pending.quantity,
		FilledQuantity: fill.Quantity,
		EntryPrice:     fill.Price,
		StopMultiple:   pending.stopMultiple,
		ProtectiveStop: protectiveStop,
		Units:          1,
		OpenedAt:       fill.FilledAt,
	}
	if err := payload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: fill %q would open an invalid campaign: %w", fill.InstrumentID, fill.FillID, err)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		// Unreachable: every field is a string, an int, an int64, a float64 or
		// a time.Time, none of which can fail to marshal. Failing closed
		// rather than panicking, in case the payload ever grows a field that
		// can.
		return nil, fmt.Errorf("strategy: marshal campaign opened payload: %w", err)
	}

	// The state moves only now, after the payload it will be journalled as has
	// been validated: a Campaign that could not be recorded must not exist in
	// memory either.
	state.campaign = &campaignState{
		campaignID:     campaignID,
		proposalID:     pending.proposalID,
		signalID:       pending.signalID,
		openingFillID:  fill.FillID,
		direction:      fill.Direction,
		campaignN:      pending.n,
		unitQuantity:   pending.quantity,
		filledQuantity: fill.Quantity,
		entryPrice:     fill.Price,
		stopMultiple:   pending.stopMultiple,
		protectiveStop: protectiveStop,
		openedAt:       fill.FilledAt,
		units:          1,
	}
	// The proposal has been executed, so it is no longer outstanding and must
	// not later be expired as though it had never filled.
	state.pendingProposal = nil

	// EventTime is the fill's timestamp: the Campaign came into being when the
	// fill did, not when the Signal fired.
	return []event.Envelope{r.stamp(
		campaignID,
		event.CampaignOpenedEventType, event.CampaignOpenedSchemaVersion,
		fill.FilledAt, input, payloadBytes,
	)}, nil
}
