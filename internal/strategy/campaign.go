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

// acceptedFillState remembers ONE fill this reducer has already accepted —
// either the entry that opened a Campaign or the stop that closed one —
// keyed by FillID on Reducer.acceptedFills, for the WHOLE run rather than
// per instrument (see applyFill).
//
// docs/architecture.md requires duplicate decision and order identifiers to
// be idempotent WITHOUT qualification, and a fill's effect can be long gone
// from the rest of instrumentState by the time a re-delivery of the same
// fill id arrives: the Campaign it opened may already have closed, or a
// SECOND Campaign in the same instrument may itself have opened and closed
// since. Remembering every accepted fill's identity for the life of the
// run — rather than only the most recent one, as an earlier design did — is
// what makes a re-delivery recognisable regardless of how much has happened
// to the instrument since.
//
// It is keyed on the Reducer, not on instrumentState, and carries its own
// instrumentID, because a fill id is a producer-assigned identifier with no
// guarantee of being scoped to one instrument: a producer that reused a fill
// id across two different instruments' executions must be rejected as a
// reconciliation failure — the same "same id, different contents" rule as
// any other reused id — rather than silently accepted as two independent
// new fills because each instrument kept its own, separate history.
//
// Memory grows by one of these per fill this reducer actually accepts in a
// run, which is bounded by the number of fills — the same order of
// magnitude as the number of Campaigns and Adds a run produces, not a
// concern at this system's scale.
type acceptedFillState struct {
	instrumentID string
	kind         string
	proposalID   string
	campaignID   string
	quantity     int64
	price        float64
	direction    string
	filledAt     time.Time
}

// acceptedFillFromPayload builds the record applyFill stores for fill once
// it has been accepted (whether it opened or closed a Campaign).
func acceptedFillFromPayload(fill event.FillPayload) acceptedFillState {
	return acceptedFillState{
		instrumentID: fill.InstrumentID,
		kind:         fill.Kind,
		proposalID:   fill.ProposalID,
		campaignID:   fill.CampaignID,
		quantity:     fill.Quantity,
		price:        fill.Price,
		direction:    fill.Direction,
		filledAt:     fill.FilledAt,
	}
}

// matches reports whether fill is the identical fact a was recorded from —
// same instrument, same kind, same proposal/campaign it names, same
// quantity, price, direction and timestamp — as opposed to merely reusing
// a's fill id for a different execution (possibly on a different
// instrument entirely).
func (a acceptedFillState) matches(fill event.FillPayload) bool {
	return a.instrumentID == fill.InstrumentID &&
		a.kind == fill.Kind &&
		a.proposalID == fill.ProposalID &&
		a.campaignID == fill.CampaignID &&
		a.quantity == fill.Quantity &&
		a.price == fill.Price &&
		a.direction == fill.Direction &&
		a.filledAt.Equal(fill.FilledAt)
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

// expireEntryProposal ends an outstanding entry (trade) proposal that the
// next completed bar has superseded, and returns the event that records it.
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
//
// #13 reuses this exact payload and event type for an outstanding EXIT
// proposal too (expireExitProposal, below), naming the difference with
// Kind rather than minting a second event.
func (r *Reducer) expireEntryProposal(state *instrumentState, bar event.CompletedBarPayload, input event.Envelope) (event.Envelope, error) {
	pending := state.pendingProposal
	state.pendingProposal = nil

	payload := event.ProposalExpiredPayload{
		InstrumentID: bar.InstrumentID,
		Kind:         event.ProposalKindEntry,
		ProposalID:   pending.proposalID,
		SignalID:     pending.signalID,
		PeriodEnd:    pending.periodEnd,
		ExpiredAt:    bar.PeriodEnd,
		Rule:         event.RuleSignalExpiresWithItsBar,
		ADR:          event.ADRSignalExpiry,
		Reason:       event.ExpiryReasonSupersededByNextBar,
		Quantity:     pending.quantity,
		Level:        pending.entryLevel,
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
	// bar: at most one entry-kind expiry can happen per (instrument, completed
	// bar), so that pair identifies it uniquely — the same reasoning as
	// decisionID's.
	return r.stamp(
		decisionID("proposal-expired", bar.InstrumentID, bar.PeriodEnd),
		event.ProposalExpiredEventType, event.ProposalExpiredSchemaVersion,
		bar.PeriodEnd, input, payloadBytes,
	), nil
}

// pendingExitProposalState is #13's exit-side mirror of pendingProposalState:
// an exit proposal (strategy.exit.proposed) that has been emitted and not
// yet resolved. It is NOT position state, for the identical reason
// pendingProposalState is not: nothing about the Campaign's own life
// depends on it, and the Campaign closes only from a recorded exit fill —
// never from this proposal alone (see evaluateCampaign and applyExitFill).
//
// It carries far less than pendingProposalState because an exit proposal
// carries no sizing of its own: it names the Campaign it would close, the
// level that was breached, and the quantity already held, nothing more.
//
// It lives for one bar, the identical lifetime ADR 0011 gives every proposal
// in this system: the next completed bar for the instrument supersedes it
// (see Reducer.expireExitProposal).
type pendingExitProposalState struct {
	proposalID string
	periodEnd  time.Time
	quantity   int64
	level      float64
}

// expireExitProposal ends an outstanding exit proposal that the next
// completed bar has superseded, and returns the event that records it —
// #13's exit-side mirror of expireEntryProposal, reusing the identical
// event.ProposalExpiredPayload with Kind ProposalKindExit rather than a
// second event type (see that payload's own doc comment). Unlike an
// entry-kind expiry, SignalID is left empty: an exit proposal is not sized
// from a Signal at all (event.ExitProposalPayload's doc comment).
func (r *Reducer) expireExitProposal(state *instrumentState, bar event.CompletedBarPayload, input event.Envelope) (event.Envelope, error) {
	pending := state.pendingExitProposal
	state.pendingExitProposal = nil

	payload := event.ProposalExpiredPayload{
		InstrumentID: bar.InstrumentID,
		Kind:         event.ProposalKindExit,
		ProposalID:   pending.proposalID,
		SignalID:     "",
		PeriodEnd:    pending.periodEnd,
		ExpiredAt:    bar.PeriodEnd,
		Rule:         event.RuleExitProposalExpiresWithItsBar,
		ADR:          event.ADRSignalExpiry,
		Reason:       event.ExpiryReasonSupersededByNextBar,
		Quantity:     pending.quantity,
		Level:        pending.level,
	}
	if err := payload.Validate(); err != nil {
		return event.Envelope{}, fmt.Errorf("strategy: built invalid proposal expired payload: %w", err)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		// Unreachable, for the same reason expireEntryProposal's marshal
		// guard is.
		return event.Envelope{}, fmt.Errorf("strategy: marshal proposal expired payload: %w", err)
	}
	// A distinct decisionID kind from the entry-kind expiry's own
	// ("exit-proposal-expired" vs "proposal-expired"), even though the two
	// can never coexist for one instrument on one bar in practice (a pending
	// entry proposal is always cleared before a Campaign, and so an exit
	// proposal, can exist) — kept distinct so the ids stay self-describing
	// rather than relying on that invariant to avoid a collision.
	return r.stamp(
		decisionID("exit-proposal-expired", bar.InstrumentID, bar.PeriodEnd),
		event.ProposalExpiredEventType, event.ProposalExpiredSchemaVersion,
		bar.PeriodEnd, input, payloadBytes,
	), nil
}

// evaluateCampaign runs an open Campaign's per-bar decision (#13): the
// Protective Stop and Exit Channel levels in force on this bar, journaled as
// a CampaignEvaluatedPayload regardless of whether either one triggers
// anything — this is the event that fills the hole #12's Concerns left open
// ("a bar for an instrument in a Campaign currently emits nothing at all").
//
// If the Exit Channel is ready and this bar's low fell strictly below it
// (The Turtle Rules p.26: price "falls below" the channel; a tie is not a
// breach, mirroring the Entry Channel's strict "exceeds"), it additionally
// proposes the exit: an ExitProposalPayload for the Campaign's WHOLE filled
// quantity, at the channel level — a proposal only. Nothing about the
// Campaign's own state moves here; only a recorded exit fill closes it (see
// applyExitFill and this file's package doc comment on the invariant this
// whole file exists to enforce).
//
// exitChannelLow/exitChannelReady are passed in rather than read from
// state.exitChannel again here: applyCompletedBar's evaluate block already
// read them, before the bar was folded into the channel for the NEXT bar to
// see, and passing the same values through keeps this function from being
// able to accidentally read a post-advance (and therefore look-ahead) value.
func (r *Reducer) evaluateCampaign(state *instrumentState, bar event.CompletedBarPayload, exitChannelLow float64, exitChannelReady bool, input event.Envelope) ([]event.Envelope, error) {
	campaign := state.campaign
	view := bar.SplitAdjusted

	// Matches N's and the Entry Channel's convention: a level computed from
	// fewer than ExitChannelLength bars is not a real Exit Channel level and
	// must never be read as one.
	reportedExitChannelLow := exitChannelLow
	if !exitChannelReady {
		reportedExitChannelLow = 0
	}
	exitConditionMet := exitChannelReady && view.Low < exitChannelLow

	evaluatedPayload := event.CampaignEvaluatedPayload{
		CampaignID:       campaign.campaignID,
		InstrumentID:     bar.InstrumentID,
		PeriodEnd:        bar.PeriodEnd,
		ProtectiveStop:   campaign.protectiveStop,
		ExitChannelLow:   reportedExitChannelLow,
		ExitChannelReady: exitChannelReady,
		ExitConditionMet: exitConditionMet,
	}
	if err := evaluatedPayload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: built invalid campaign evaluated payload: %w", bar.InstrumentID, err)
	}
	evaluatedBytes, err := json.Marshal(evaluatedPayload)
	if err != nil {
		// Unreachable: every field is a string, a float64, a bool or a
		// time.Time, none of which can fail to marshal. Failing closed
		// rather than panicking, in case the payload ever grows a field
		// that can.
		return nil, fmt.Errorf("strategy: marshal campaign evaluated payload: %w", err)
	}
	emissions := []event.Envelope{r.stamp(
		decisionID("campaign-evaluated", bar.InstrumentID, bar.PeriodEnd),
		event.CampaignEvaluatedEventType, event.CampaignEvaluatedSchemaVersion,
		bar.PeriodEnd, input, evaluatedBytes,
	)}

	if !exitConditionMet {
		return emissions, nil
	}

	// At most one exit proposal per bar (this function runs once per
	// instrument per completed bar), and it expires with its bar exactly
	// like an entry proposal does (see expireExitProposal) — reusing ADR
	// 0011's mechanism rather than inventing a second one, per the ticket.
	proposalPayload := event.ExitProposalPayload{
		CampaignID:   campaign.campaignID,
		InstrumentID: bar.InstrumentID,
		PeriodEnd:    bar.PeriodEnd,
		Reason:       event.ExitReasonExitChannel,
		Level:        exitChannelLow,
		Quantity:     campaign.filledQuantity,
		Rule:         event.RuleExitChannelBreach,
		ADR:          event.ADRExitChannelBreach,
	}
	if err := proposalPayload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: built invalid exit proposal payload: %w", bar.InstrumentID, err)
	}
	proposalBytes, err := json.Marshal(proposalPayload)
	if err != nil {
		// Unreachable, for the same reason evaluatedBytes' marshal guard is.
		return nil, fmt.Errorf("strategy: marshal exit proposal payload: %w", err)
	}
	proposalEnvelope := r.stamp(
		decisionID("exit-proposal", bar.InstrumentID, bar.PeriodEnd),
		event.ExitProposalEventType, event.ExitProposalSchemaVersion,
		bar.PeriodEnd, input, proposalBytes,
	)
	emissions = append(emissions, proposalEnvelope)

	// Remembered so a fill executing it can be checked against it, and so a
	// later bar with no fill knows to expire it — the identical two reasons
	// rememberPendingProposal exists for the entry side. No Campaign state
	// moves here: this reducer's only path to closing a Campaign is
	// applyExitFill, reading a recorded fill.
	state.pendingExitProposal = &pendingExitProposalState{
		proposalID: proposalEnvelope.ID,
		periodEnd:  bar.PeriodEnd,
		quantity:   campaign.filledQuantity,
		level:      exitChannelLow,
	}

	return emissions, nil
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
//  2. Idempotency, checked first — BEFORE even the instrument lookup below —
//     generically for every fill.Kind, against the WHOLE run's fill history
//     (Reducer.acceptedFills), not only one instrument's current position: a
//     re-delivery of a fill this reducer already accepted — whether it
//     opened a Campaign, closed one, or (after this ticket) any later kind,
//     for whichever instrument it named — is an idempotent no-op if the
//     contents match, and a reconciliation failure if they don't
//     (docs/architecture.md requires duplicate identifiers to be idempotent
//     without qualification; see acceptedFillState's doc comment for why
//     neither "current position" nor "per instrument" was enough — a fill id
//     is a producer-assigned identifier with no guaranteed scope). Checking
//     this before the instrument lookup means a fill id reused across two
//     different instruments is caught here, as a reconciliation failure,
//     rather than being accepted twice because each instrument kept its own
//     separate history.
//  3. An instrument this reducer has never evaluated can have no proposal
//     outstanding, so any fill for it is unmatched.
//  4. If a Campaign is already open, an entry-kind fill with a genuinely new
//     id is something this ticket deliberately refuses (see
//     applyFillToOpenCampaign) — rule 2 has already resolved every
//     re-delivery by this point, so what remains here is always a second,
//     different execution.
//  5. Otherwise the fill must match the outstanding proposal: the same
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

	// Idempotency, checked first — before even the instrument lookup below —
	// and generically against the WHOLE run's fill history, keyed only by
	// FillID: see acceptedFillState's doc comment for why neither "current
	// position" nor "per instrument" is enough. A fill id reused for a
	// different instrument entirely is therefore caught here, as a
	// reconciliation failure, rather than by an instrument-scoped check that
	// would never see it.
	if recorded, seen := r.acceptedFills[fill.FillID]; seen {
		if !recorded.matches(fill) {
			return nil, fmt.Errorf("strategy: fill %q was already recorded (instrument %q, kind %s, proposal %q, campaign %q, %d at %v %s on %s), but this delivery differs (instrument %q, kind %s, proposal %q, campaign %q, %d at %v %s on %s); a reused fill identifier carrying different contents is a reconciliation failure, not a duplicate delivery",
				fill.FillID,
				recorded.instrumentID, recorded.kind, recorded.proposalID, recorded.campaignID, recorded.quantity, recorded.price, recorded.direction, recorded.filledAt.Format(time.RFC3339),
				fill.InstrumentID, fill.Kind, fill.ProposalID, fill.CampaignID, fill.Quantity, fill.Price, fill.Direction, fill.FilledAt.Format(time.RFC3339))
		}
		// The duplicate delivery of an execution already recorded: nothing
		// to do, and nothing to complain about.
		return nil, nil
	}

	// Deliberately a plain lookup rather than stateFor: an instrument the
	// reducer has never seen a bar for cannot have been proposed for, and
	// creating state here would make the reducer look as though it had.
	state, known := r.instruments[fill.InstrumentID]
	if !known {
		if fill.Kind == event.FillKindStop || fill.Kind == event.FillKindExit {
			return nil, fmt.Errorf("strategy: %s fill %q names campaign %q for instrument %q, which this reducer has never evaluated; a fill for a campaign this strategy has no history for is a reconciliation failure, not something to absorb (docs/architecture.md)",
				fill.Kind, fill.FillID, fill.CampaignID, fill.InstrumentID)
		}
		return nil, fmt.Errorf("strategy: fill %q names proposal %q for instrument %q, which this reducer has never evaluated; a fill for an order this strategy never proposed is a reconciliation failure, not something to absorb (docs/architecture.md)",
			fill.FillID, fill.ProposalID, fill.InstrumentID)
	}

	// #12/#13: a stop fill or an exit fill each take a completely different
	// path from an entry fill — they close a Campaign rather than opening
	// one — so both are dispatched before any of the entry-fill logic below
	// runs. fill.Validate() has already rejected any Kind other than
	// event.FillKindEntry, event.FillKindStop or event.FillKindExit, so the
	// fall-through below is reached only for an entry fill.
	if fill.Kind == event.FillKindStop {
		return r.applyStopFill(state, fill, envelope)
	}
	if fill.Kind == event.FillKindExit {
		return r.applyExitFill(state, fill, envelope)
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

// applyFillToOpenCampaign resolves an entry-kind fill that arrives for an
// instrument already in a Campaign, once applyFill's own idempotency check
// (state.acceptedFills) has already established that fill.FillID is
// genuinely new — never seen before, by this reducer, from this instrument.
//
// That leaves exactly two possibilities, both of which fail closed:
//
//   - A fill naming a different proposal. The instrument is already
//     committed; a fill for some other order it never had outstanding is a
//     reconciliation failure.
//   - A second, different fill for the SAME proposal (a further partial
//     fill). Accumulating successive partials into one Campaign is deferred
//     to its own issue (#67); until it lands, rejecting is the only safe
//     answer, because the alternative — opening a second Campaign for the
//     same instrument — would double the position while every cap and
//     ladder still counted one.
//
// Duplicate delivery of the fill that actually opened this Campaign never
// reaches this function at all: applyFill's idempotency check resolves it
// first, whether the re-delivery is identical (a no-op) or reused with
// different contents (a reconciliation error) — see acceptedFillState's doc
// comment.
func applyFillToOpenCampaign(campaign *campaignState, fill event.FillPayload) ([]event.Envelope, error) {
	if campaign.proposalID != fill.ProposalID {
		return nil, fmt.Errorf("strategy: instrument %q: fill %q names proposal %q, but campaign %q is already open from proposal %q; a fill for an order this strategy never proposed is a reconciliation failure (docs/architecture.md)",
			fill.InstrumentID, fill.FillID, fill.ProposalID, campaign.campaignID, campaign.proposalID)
	}
	return nil, fmt.Errorf("strategy: instrument %q: campaign %q has already opened from fill %q, and fill %q is a second, different execution of the same proposal; accumulating successive partial fills into one campaign is deferred to its own issue, and opening a second campaign instead would double the position while every cap and ladder still counted one",
		fill.InstrumentID, campaign.campaignID, campaign.openingFillID, fill.FillID)
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
	// Recorded so a re-delivery of this exact fill is recognised as an
	// idempotent no-op for the rest of this run, however much later it
	// arrives and however much has happened to the instrument since (see
	// acceptedFillState's doc comment).
	r.acceptedFills[fill.FillID] = acceptedFillFromPayload(fill)

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
//
// Reaching this function with state.campaign == nil now means, unqualified,
// "there is no open campaign for this fill to close": applyFill's own
// idempotency check has already resolved a re-delivery of a fill this
// reducer previously accepted — whether it closed THIS instrument's most
// recent Campaign or an earlier one entirely — before dispatch ever reaches
// here (see acceptedFillState's doc comment). What is left is genuinely
// unknown.
func (r *Reducer) applyStopFill(state *instrumentState, fill event.FillPayload, input event.Envelope) ([]event.Envelope, error) {
	campaign := state.campaign
	if campaign == nil {
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
	// has been validated. Recorded into acceptedFills before campaign is
	// cleared, exactly as openCampaign does for the entry fill, so a
	// re-delivery of this exact fill stays an idempotent no-op for the rest
	// of the run regardless of what happens to this instrument afterwards
	// (see acceptedFillState's doc comment).
	r.acceptedFills[fill.FillID] = acceptedFillFromPayload(fill)
	// #12: the instrument is a Setup again — CONTEXT.md defines a Setup as
	// an Eligible instrument not in a Campaign, and clearing this is the
	// only thing that gate (applyCompletedBar's "no new entry while a
	// Campaign is open") reads. The very next completed bar therefore
	// evaluates this instrument normally and may Signal, with no further
	// change needed anywhere else.
	state.campaign = nil

	return []event.Envelope{exitEnvelope}, nil
}

// applyExitFill handles a fill.Kind == event.FillKindExit delivery: #13's
// way of closing a Campaign, alongside #12's stop fill. Unlike a stop fill,
// an exit fill always executes a specific outstanding exit proposal
// (evaluateCampaign's ExitProposalPayload) — ADR 0005 makes the exit a
// resting order, so it is always proposed before it can be filled — and
// fill.Validate() has already required both CampaignID and ProposalID to be
// present for this Kind.
//
// **No decision about WHETHER the channel was breached is made here**,
// mirroring applyStopFill's own doc comment: this function only ever reacts
// to a fill event that already says the exit was executed. It never reads
// bar data, and nothing in this package compares a bar's low to
// campaignState.protectiveStop or to an Exit Channel level except
// evaluateCampaign's own reading (which proposes, but never fills).
//
// Reaching this function with state.campaign == nil now means, unqualified,
// "there is no open campaign for this fill to close" — including the case
// #13 explicitly names: a stop fill already closed this same Campaign, and
// this exit fill is a second, later closing fill for it. applyFill's own
// idempotency check has already resolved a re-delivery of a fill this
// reducer previously accepted before dispatch ever reaches here (see
// acceptedFillState's doc comment), so what is left is genuinely a second,
// different execution racing the first — and it fails closed, exactly as two
// stop fills racing each other already would.
func (r *Reducer) applyExitFill(state *instrumentState, fill event.FillPayload, input event.Envelope) ([]event.Envelope, error) {
	campaign := state.campaign
	if campaign == nil {
		return nil, fmt.Errorf("strategy: instrument %q: exit fill %q names campaign %q, but there is no open campaign for it; a closing fill for an unknown or already-closed campaign is a reconciliation failure (docs/architecture.md)",
			fill.InstrumentID, fill.FillID, fill.CampaignID)
	}
	if campaign.campaignID != fill.CampaignID {
		return nil, fmt.Errorf("strategy: instrument %q: exit fill %q names campaign %q, but the open campaign is %q; a fill for a campaign this strategy does not hold is a reconciliation failure (docs/architecture.md)",
			fill.InstrumentID, fill.FillID, fill.CampaignID, campaign.campaignID)
	}
	pending := state.pendingExitProposal
	if pending == nil {
		return nil, fmt.Errorf("strategy: instrument %q: exit fill %q names proposal %q, but there is no outstanding exit proposal for campaign %q; an exit proposal expires with its bar (ADR 0011, see the strategy.proposal.expired event in the journal) and a fill for a proposal this strategy is no longer offering is a reconciliation failure (docs/architecture.md)",
			fill.InstrumentID, fill.FillID, fill.ProposalID, campaign.campaignID)
	}
	if pending.proposalID != fill.ProposalID {
		return nil, fmt.Errorf("strategy: instrument %q: exit fill %q names proposal %q, but the outstanding exit proposal is %q; a fill for a proposal this strategy never made is a reconciliation failure (docs/architecture.md)",
			fill.InstrumentID, fill.FillID, fill.ProposalID, pending.proposalID)
	}
	if fill.Direction != campaign.direction {
		return nil, fmt.Errorf("strategy: instrument %q: exit fill %q is %s but campaign %q is %s; the closing fill must be in the campaign's own direction (FillPayload.Direction is the position's direction, not the order's buy/sell side)",
			fill.InstrumentID, fill.FillID, fill.Direction, campaign.campaignID, campaign.direction)
	}
	if fill.Quantity != campaign.filledQuantity {
		return nil, fmt.Errorf("strategy: instrument %q: exit fill %q executed %d but campaign %q holds %d; a partial exit fill is rejected — every Unit exits together (CONTEXT.md: 'Campaign'), the same fail-closed answer #12 gives a partial stop fill",
			fill.InstrumentID, fill.FillID, fill.Quantity, campaign.campaignID, campaign.filledQuantity)
	}
	if fill.FilledAt.Before(campaign.openedAt) {
		return nil, fmt.Errorf("strategy: instrument %q: exit fill %q is timestamped %s, which predates campaign %q's own opening fill at %s; a campaign cannot be closed before it opened",
			fill.InstrumentID, fill.FillID, fill.FilledAt.Format(time.RFC3339), campaign.campaignID, campaign.openedAt.Format(time.RFC3339))
	}

	// The realised result, in the exact expression order
	// event.CampaignExitedPayload.Validate re-derives it in, so the two
	// agree bit for bit — identical to applyStopFill's own derivation.
	// ExitPrice is fill.Price — what actually filled, which under ADR 0005's
	// gap rule may sit below the Exit Channel level — never pending.level
	// itself.
	realisedResult := float64(campaign.filledQuantity) * (fill.Price - campaign.entryPrice) * r.dollarsPerPoint
	realisedResultInN := (fill.Price - campaign.entryPrice) / campaign.campaignN

	exitedPayload := event.CampaignExitedPayload{
		CampaignID:          campaign.campaignID,
		InstrumentID:        fill.InstrumentID,
		FillID:              fill.FillID,
		ExitedAt:            fill.FilledAt,
		Reason:              event.ExitReasonExitChannel,
		EntryPrice:          campaign.entryPrice,
		ExitPrice:           fill.Price,
		Quantity:            campaign.filledQuantity,
		CampaignN:           campaign.campaignN,
		DollarsPerPoint:     r.dollarsPerPoint,
		ProtectiveStopLevel: campaign.protectiveStop,
		RealisedResult:      realisedResult,
		RealisedResultInN:   realisedResultInN,
		Rule:                event.RuleCampaignExitedByExitChannel,
		ADR:                 event.ADRCampaignExitRecordsTheFill,
	}
	if err := exitedPayload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: exit fill %q would close campaign %q with an invalid exit: %w", fill.InstrumentID, fill.FillID, campaign.campaignID, err)
	}
	exitedPayloadBytes, err := json.Marshal(exitedPayload)
	if err != nil {
		// Unreachable, for the same reason as applyStopFill's marshal guard.
		return nil, fmt.Errorf("strategy: marshal campaign exited payload: %w", err)
	}

	exitID := decisionID("campaign-exited", fill.InstrumentID, fill.FilledAt)
	exitEnvelope := r.stamp(exitID, event.CampaignExitedEventType, event.CampaignExitedSchemaVersion, fill.FilledAt, input, exitedPayloadBytes)

	// The state moves only now, after the payload it will be journalled as
	// has been validated — identical discipline to applyStopFill's own.
	r.acceptedFills[fill.FillID] = acceptedFillFromPayload(fill)
	// The instrument is a Setup again (CONTEXT.md), the same consequence
	// applyStopFill's own closing has; and the exit proposal this fill
	// executed is resolved, so a later bar does not try to expire it again.
	state.campaign = nil
	state.pendingExitProposal = nil

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
