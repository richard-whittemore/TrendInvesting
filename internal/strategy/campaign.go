package strategy

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
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

// closedStopFillState remembers the fill that most recently closed a
// Campaign in this instrument, purely so a re-delivery of that EXACT fill is
// still recognised as an idempotent no-op after the Campaign it closed is
// gone from state (docs/architecture.md: "duplicate decision and order
// identifiers must be idempotent"). Without it, a re-delivered stop fill
// arriving after instrumentState.campaign has already been cleared would
// look identical to a stop fill for an unknown Campaign — see applyStopFill.
//
// It holds the same fields applyFillToOpenCampaign already compares for an
// entry fill's duplicate check, for the same reason: idempotency means "the
// same fact delivered twice", not "any fact carrying a fill id already
// seen" (see applyFillToOpenCampaign's doc comment). A producer that reuses
// a fill id for a different execution is a reconciliation failure, not a
// duplicate.
type closedStopFillState struct {
	campaignID string
	fillID     string
	quantity   int64
	price      float64
	direction  string
	filledAt   time.Time
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

// checkBarConfirmsCampaignOpening enforces the upper end of the window
// described on applyFill, at the earliest point the reducer is able to.
//
// A fill timestamped after a bar that had not yet completed cannot have
// happened: the execution claims a moment the stream has not reached. That
// contradiction is invisible when the fill arrives — the next bar does not
// exist yet, and no bar length is configured, so there is nothing to compare
// against — and it becomes visible the instant the next completed bar for the
// instrument does arrive. So it is checked here rather than in applyFill, and
// the run fails rather than continuing with a Campaign whose id, OpenedAt and
// EventTime sit in the stream's future.
//
// Equality is allowed: a fill at a bar's close happened within that bar.
//
// Only the first bar after the Campaign opened can fail this, since the run
// stops when it does and every later bar ends after the one that did not.
func checkBarConfirmsCampaignOpening(state *instrumentState, bar event.CompletedBarPayload) error {
	if state.campaign == nil || !bar.PeriodEnd.Before(state.campaign.openedAt) {
		return nil
	}
	return fmt.Errorf("strategy: instrument %q: bar period end %s predates campaign %q, which was opened by fill %q timestamped %s; an execution cannot have happened after a bar that had not yet completed, so the fill's timestamp is inconsistent with the bar stream",
		bar.InstrumentID, bar.PeriodEnd.Format(time.RFC3339),
		state.campaign.campaignID, state.campaign.openingFillID,
		state.campaign.openedAt.Format(time.RFC3339))
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
// # The window a fill's timestamp must lie in
//
// It is **(the period end of the bar before the decision bar, the period end
// of the next bar for that instrument]**, and it is deliberately not anchored
// on the proposal's own period end at either end. ADR 0005 makes the entry a
// resting order that fills *inside* the breakout bar, so a fill timestamped
// before the decision bar's close is the ordinary backtest case; live, the
// order rests into the following session and fills after that close. A bound
// at the proposal's period end would therefore be wrong in both directions.
//
// The two ends are enforced in different places, because they become knowable
// at different times:
//
//   - **Lower bound, here.** A fill from before the decision bar even opened
//     is impossible: no order for that proposal existed then, and accepting
//     one would journal a Campaign whose identity, OpenedAt and EventTime
//     predate the bar whose data produced the decision authorising it. The
//     bound is pendingProposalState.earliestFillAt, known the moment the
//     proposal was made, so it is checked as the fill is applied.
//   - **Upper bound, at the next completed bar** — see
//     checkBarConfirmsCampaignOpening. A fill claiming a moment the stream has
//     not reached is equally impossible, but it cannot be detected here: the
//     next bar does not exist yet, and no bar length is configured, so there
//     is nothing to compare against. It is checked at the first point the
//     reducer can know it, which is when that bar arrives.
//
// Both bounds were PR #69 review findings, each rebounded from what the
// finding literally proposed. Note that the upper bound is not the same rule
// as ADR 0011's expiry, which handles a fill *arriving* after the next bar
// (the proposal is gone, so there is nothing pending to match); this one
// handles a fill arriving in time but *claiming* a time after it.
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
		if fill.Kind == event.FillKindStop {
			return nil, fmt.Errorf("strategy: stop fill %q names campaign %q for instrument %q, which this reducer has never evaluated; a fill for a campaign this strategy has no history for is a reconciliation failure, not something to absorb (docs/architecture.md)",
				fill.FillID, fill.CampaignID, fill.InstrumentID)
		}
		return nil, fmt.Errorf("strategy: fill %q names proposal %q for instrument %q, which this reducer has never evaluated; a fill for an order this strategy never proposed is a reconciliation failure, not something to absorb (docs/architecture.md)",
			fill.FillID, fill.ProposalID, fill.InstrumentID)
	}

	// #12: a stop fill takes a completely different path from an entry
	// fill — it closes a Campaign rather than opening one — so it is
	// dispatched before any of the entry-fill logic below runs.
	// fill.Validate() has already rejected any Kind other than
	// event.FillKindEntry or event.FillKindStop, so the fall-through below
	// is reached only for an entry fill.
	if fill.Kind == event.FillKindStop {
		return r.applyStopFill(state, fill, envelope)
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
	// The lower bound of the window described above; the upper bound is
	// checked at the next completed bar, by checkBarConfirmsCampaignOpening.
	// Strict: a fill stamped exactly at the previous bar's period end is at
	// the instant the decision bar opened, before which no order for this
	// proposal existed. The zero bound (documented on
	// pendingProposalState.earliestFillAt) needs no special case: every real
	// timestamp is after it.
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
// A fill that would produce an unreachable Protective Stop (at or below
// zero) is refused by sizing.ProtectiveStopLevel before any payload is even
// built, and the run stops. That cannot arise from a producer honouring ADR
// 0005, which fills a long entry at or above the proposed level, on a
// proposal whose stop intent was already required to be positive — so
// reaching it means the position could not be protected, which is a
// condition to fail on rather than to journal.
//
// #12: every open Campaign has a Protective Stop from the moment it exists —
// this function is the one place a Campaign is constructed (campaignState
// has no "open without stop" zero value that would pass
// checkCampaignHasAProtectiveStop), and it emits the Protective-Stop-set
// decision (event.ProtectiveStopSetEventType) immediately after
// Campaign-opened, in the same Apply return, so the two are never observed
// apart in the journal.
func (r *Reducer) openCampaign(state *instrumentState, pending *pendingProposalState, fill event.FillPayload, input event.Envelope) ([]event.Envelope, error) {
	// The Campaign's identity is the instrument plus the moment it came into
	// being, which is the opening fill's timestamp. Deterministic, so replay
	// reconstructs the same identity with no randomness or wall-clock read;
	// the Campaign-opened envelope carries the same value as its own ID.
	campaignID := decisionID("campaign", fill.InstrumentID, fill.FilledAt)

	// The Turtle Rules p.22's 2N stop in the Baseline, measured from the
	// ACTUAL fill (ADR 0013) and the campaign's FROZEN N (ADR 0006).
	// sizing.ProtectiveStopLevel computes entryPrice - stopMultiple x
	// campaignN in exactly the expression order both
	// event.CampaignOpenedPayload.Validate and
	// event.ProtectiveStopSetPayload.Validate re-derive it in below, so all
	// three agree bit for bit.
	protectiveStop, err := sizing.ProtectiveStopLevel(fill.Price, pending.n, pending.stopMultiple, sizing.DirectionLong)
	if err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: fill %q cannot compute a protective stop: %w", fill.InstrumentID, fill.FillID, err)
	}

	openedPayload := event.CampaignOpenedPayload{
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
	if err := openedPayload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: fill %q would open an invalid campaign: %w", fill.InstrumentID, fill.FillID, err)
	}
	openedPayloadBytes, err := json.Marshal(openedPayload)
	if err != nil {
		// Unreachable: every field is a string, an int, an int64, a float64 or
		// a time.Time, none of which can fail to marshal. Failing closed
		// rather than panicking, in case the payload ever grows a field that
		// can.
		return nil, fmt.Errorf("strategy: marshal campaign opened payload: %w", err)
	}

	// #12: the Protective-Stop-set decision, built and validated before any
	// state moves, for the same reason the Campaign-opened payload is —
	// see this function's doc comment.
	stopSetPayload := event.ProtectiveStopSetPayload{
		CampaignID:    campaignID,
		InstrumentID:  fill.InstrumentID,
		AsOf:          fill.FilledAt,
		Level:         protectiveStop,
		PreviousLevel: 0,
		EntryPrice:    fill.Price,
		CampaignN:     pending.n,
		StopMultiple:  pending.stopMultiple,
		Rule:          event.RuleProtectiveStopSetFromFill,
		ADR:           event.ADRCampaignFrozenAtEntry,
	}
	if err := stopSetPayload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: fill %q would set an invalid protective stop: %w", fill.InstrumentID, fill.FillID, err)
	}
	stopSetPayloadBytes, err := json.Marshal(stopSetPayload)
	if err != nil {
		// Unreachable, for the same reason as openedPayloadBytes above.
		return nil, fmt.Errorf("strategy: marshal protective stop set payload: %w", err)
	}

	// The state moves only now, after BOTH payloads it will be journalled as
	// have been validated: a Campaign that could not be recorded, complete
	// with its stop, must not exist in memory either. This is what makes
	// "an open Campaign without a Protective Stop" unrepresentable by
	// construction: there is no assignment to state.campaign anywhere else
	// in this package, and this one never runs without a validated,
	// positive, below-entry stop already in hand.
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

	// EventTime is the fill's timestamp on both: the Campaign, and its stop,
	// came into being when the fill did, not when the Signal fired. Order is
	// Campaign-opened then Protective-Stop-set, per the ticket.
	return []event.Envelope{
		r.stamp(campaignID, event.CampaignOpenedEventType, event.CampaignOpenedSchemaVersion, fill.FilledAt, input, openedPayloadBytes),
		r.stamp(decisionID("protective-stop-set", fill.InstrumentID, fill.FilledAt), event.ProtectiveStopSetEventType, event.ProtectiveStopSetSchemaVersion, fill.FilledAt, input, stopSetPayloadBytes),
	}, nil
}

// applyStopFill handles a fill.Kind == event.FillKindStop delivery: the only
// way a Campaign closes in this ticket. #13's Exit-Channel exit and #24's
// delisting exit are later tickets and will each produce their own kind of
// terminal fact, sharing event.CampaignExitedPayload with their own Reason
// rather than a new event type.
//
// **No decision about WHETHER the stop was hit is made here.** That is
// #18's fill simulator, comparing a bar's low against the Protective Stop
// level under ADR 0005. This function only ever reacts to a fill event that
// already says the stop was hit — it never reads bar data, and nothing in
// this package compares a price to campaignState.protectiveStop except the
// capital-safety invariant check (checkCampaignHasAProtectiveStop), which
// checks the stop's OWN shape, never a bar's price against it. A reviewer
// checking for look-ahead should find none: this function's only inputs are
// the fill and the Campaign state a fill already opened.
func (r *Reducer) applyStopFill(state *instrumentState, fill event.FillPayload, input event.Envelope) ([]event.Envelope, error) {
	campaign := state.campaign
	if campaign == nil {
		// Either the Campaign never existed, or it was already closed. A
		// re-delivery of the EXACT fill that closed it is idempotent
		// (docs/architecture.md); anything else — including this same fill
		// id with different contents — is a reconciliation failure.
		if closed := state.closedStopFill; closed != nil && closed.fillID == fill.FillID {
			if closed.campaignID != fill.CampaignID || closed.quantity != fill.Quantity ||
				closed.price != fill.Price || closed.direction != fill.Direction || !closed.filledAt.Equal(fill.FilledAt) {
				return nil, fmt.Errorf("strategy: instrument %q: stop fill %q was already recorded as closing campaign %q, but this delivery's campaign id, quantity, price, direction or timestamp differ from it; a reused fill identifier carrying different contents is a reconciliation failure, not a duplicate delivery",
					fill.InstrumentID, fill.FillID, closed.campaignID)
			}
			return nil, nil
		}
		return nil, fmt.Errorf("strategy: instrument %q: stop fill %q names campaign %q, but there is no open campaign for it; a stop fill for an unknown or already-closed campaign is a reconciliation failure (docs/architecture.md)",
			fill.InstrumentID, fill.FillID, fill.CampaignID)
	}
	if campaign.campaignID != fill.CampaignID {
		return nil, fmt.Errorf("strategy: instrument %q: stop fill %q names campaign %q, but the open campaign is %q; a fill for a campaign this strategy does not hold is a reconciliation failure (docs/architecture.md)",
			fill.InstrumentID, fill.FillID, fill.CampaignID, campaign.campaignID)
	}
	if fill.Direction != campaign.direction {
		return nil, fmt.Errorf("strategy: instrument %q: stop fill %q is %s but campaign %q is %s; the closing fill must be in the campaign's own direction (FillPayload.Direction is the position's direction, not the order's buy/sell side)",
			fill.InstrumentID, fill.FillID, fill.Direction, campaign.campaignID, campaign.direction)
	}
	if fill.Quantity != campaign.filledQuantity {
		return nil, fmt.Errorf("strategy: instrument %q: stop fill %q executed %d but campaign %q holds %d; a partial stop fill is rejected — accumulating a partial close into one campaign is deferred to its own issue, the same limitation #67 already records for a partial entry",
			fill.InstrumentID, fill.FillID, fill.Quantity, campaign.campaignID, campaign.filledQuantity)
	}
	if fill.FilledAt.Before(campaign.openedAt) {
		return nil, fmt.Errorf("strategy: instrument %q: stop fill %q is timestamped %s, which predates campaign %q's own opening fill at %s; a campaign cannot be closed before it opened",
			fill.InstrumentID, fill.FillID, fill.FilledAt.Format(time.RFC3339), campaign.campaignID, campaign.openedAt.Format(time.RFC3339))
	}

	// The realised result, in the exact expression order
	// event.CampaignExitedPayload.Validate re-derives it in, so the two
	// agree bit for bit. ExitPrice is fill.Price — what actually filled,
	// which under ADR 0005's gap rule may sit below the Protective Stop
	// level — never campaign.protectiveStop itself.
	realisedResult := float64(campaign.filledQuantity) * (fill.Price - campaign.entryPrice) * r.dollarsPerPoint
	realisedResultInN := (fill.Price - campaign.entryPrice) / campaign.campaignN

	exitedPayload := event.CampaignExitedPayload{
		CampaignID:          campaign.campaignID,
		InstrumentID:        fill.InstrumentID,
		FillID:              fill.FillID,
		ExitedAt:            fill.FilledAt,
		Reason:              event.ExitReasonStop,
		EntryPrice:          campaign.entryPrice,
		ExitPrice:           fill.Price,
		Quantity:            campaign.filledQuantity,
		CampaignN:           campaign.campaignN,
		DollarsPerPoint:     r.dollarsPerPoint,
		ProtectiveStopLevel: campaign.protectiveStop,
		RealisedResult:      realisedResult,
		RealisedResultInN:   realisedResultInN,
		Rule:                event.RuleCampaignExitedByStop,
		ADR:                 event.ADRCampaignExitRecordsTheFill,
	}
	if err := exitedPayload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: stop fill %q would close campaign %q with an invalid exit: %w", fill.InstrumentID, fill.FillID, campaign.campaignID, err)
	}
	exitedPayloadBytes, err := json.Marshal(exitedPayload)
	if err != nil {
		// Unreachable, for the same reason as openCampaign's marshal guards.
		return nil, fmt.Errorf("strategy: marshal campaign exited payload: %w", err)
	}

	exitID := decisionID("campaign-exited", fill.InstrumentID, fill.FilledAt)
	exitEnvelope := r.stamp(exitID, event.CampaignExitedEventType, event.CampaignExitedSchemaVersion, fill.FilledAt, input, exitedPayloadBytes)

	// The state moves only now, after the payload it will be journalled as
	// has been validated. Remembering closedStopFill before clearing
	// campaign is what lets a re-delivery of this exact fill be recognised
	// as a duplicate afterwards (see this function's top).
	state.closedStopFill = &closedStopFillState{
		campaignID: campaign.campaignID,
		fillID:     fill.FillID,
		quantity:   fill.Quantity,
		price:      fill.Price,
		direction:  fill.Direction,
		filledAt:   fill.FilledAt,
	}
	// #12: the instrument is a Setup again — CONTEXT.md defines a Setup as
	// an Eligible instrument not in a Campaign, and clearing this is the
	// only thing that gate (applyCompletedBar's "no new entry while a
	// Campaign is open") reads. The very next completed bar therefore
	// evaluates this instrument normally and may Signal, with no further
	// change needed anywhere else.
	state.campaign = nil

	return []event.Envelope{exitEnvelope}, nil
}

// checkCampaignHasAProtectiveStop enforces, at the start of every completed
// bar, the ticket's capital-safety invariant: every open Campaign has a
// Protective Stop, positive and strictly below its entry price, at all
// times (CONTEXT.md: "Protective Stop" — "Every open Campaign has one at
// all times").
//
// This is deliberately a *runtime* check on top of a representation that
// already makes the violation unreachable in practice: openCampaign is the
// only place state.campaign is ever assigned, and it never runs without a
// protectiveStop that has already passed sizing.ProtectiveStopLevel's and
// event.CampaignOpenedPayload.Validate's checks — there is no
// "open-without-stop" zero value of campaignState that would satisfy the
// type system. So reaching a violation here can only mean memory was
// corrupted after the fact (a defect in this process, not a bad input
// event), which is why the response is a HALT rather than an error naming
// "the fill" or "the bar": there is no upstream input event to blame, and
// continuing to trade an instrument whose stop this reducer can no longer
// vouch for is exactly the state docs/architecture.md's safety invariants
// exist to prevent ("material reconciliation differences force safe mode").
//
// Returns the halt envelope alongside the error (rather than only the
// error) so a caller that does not discard emissions on error — the
// test-only path in invariant_test.go calls Reducer.Apply directly rather
// than through replay.Engine.Run, which DOES discard a handler's emissions
// whenever it returns an error — can still see what was about to be
// journalled.
func (r *Reducer) checkCampaignHasAProtectiveStop(state *instrumentState, bar event.CompletedBarPayload, input event.Envelope) (event.Envelope, error) {
	campaign := state.campaign
	if campaign == nil {
		return event.Envelope{}, nil
	}
	if campaign.protectiveStop > 0 && campaign.protectiveStop < campaign.entryPrice {
		return event.Envelope{}, nil
	}

	detail := fmt.Sprintf(
		"campaign %q for instrument %q has protective stop %v against entry price %v: every open campaign must have a protective stop, positive and below its entry price, at all times",
		campaign.campaignID, bar.InstrumentID, campaign.protectiveStop, campaign.entryPrice)

	payload := event.EngineStatePayload{
		State:  event.EngineStateHalted,
		Reason: event.EngineStateReasonCampaignWithoutProtectiveStop,
		Detail: detail,
	}
	if err := payload.Validate(); err != nil {
		// Unreachable: State and Reason are this package's own constants and
		// Detail is built from a non-empty format string above. Guarded
		// anyway, matching this project's fail-closed style: a payload that
		// fails its own contract must never be journalled.
		return event.Envelope{}, fmt.Errorf("strategy: built invalid engine state payload: %w", err)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		// Unreachable, for the same reason as openCampaign's marshal guards.
		return event.Envelope{}, fmt.Errorf("strategy: marshal engine state payload: %w", err)
	}
	haltEnvelope := r.stamp(
		decisionID("engine-state", bar.InstrumentID, bar.PeriodEnd),
		event.EngineStateEventType, event.EngineStateSchemaVersion,
		bar.PeriodEnd, input, payloadBytes,
	)
	return haltEnvelope, fmt.Errorf("strategy: capital-safety invariant violated: %s", detail)
}
