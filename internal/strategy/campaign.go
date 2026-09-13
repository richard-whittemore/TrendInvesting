package strategy

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// This file holds the reducer's Campaign machinery: the two pieces of
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

// unitState is one Unit of an open Campaign (CONTEXT.md: "Unit"): the record
// of one accepted entry or Add fill and the Protective Stop that fill set.
//
// campaignState holds a slice of these — one per accepted entry/Add fill —
// so that the Stop Ladder can move an individual Unit's own protectiveStop
// and close a SUBSET of units (a
// gapped Unit has its own stop level, so a stop fill may close some Units
// and not others): every field below belongs to exactly one fill, and
// nothing here is shared or aggregated across Units — aggregation (total
// quantity, weighted-average entry, the Campaign's reported Protective
// Stop) is computed FROM this slice by campaignState's own methods, never
// stored redundantly on it.
type unitState struct {
	// index is this Unit's position in the Campaign: 1 for the opening fill,
	// 2 through the Campaign's frozen maxUnits for each Add.
	index int
	// openingFillID is the fill that brought this Unit into being — the
	// entry fill for Unit 1, an Add fill for every later Unit.
	openingFillID string
	fillPrice     float64
	// quantity is what actually executed for this Unit — at most the
	// Campaign's frozen unitQuantity; a partial fill is accepted for the
	// filled quantity, mirroring the same rule for Unit 1.
	quantity int64
	// protectiveStop is THIS Unit's own stop, fillPrice - stopMultiple x
	// campaignN, set once when the Unit's fill was accepted and never moved
	// here (the Stop Ladder is what raises earlier Units' stops as later
	// ones are added).
	protectiveStop float64
	filledAt       time.Time
}

// campaignState is one instrument's open Campaign (CONTEXT.md: "Campaign" —
// the complete life of a position in one instrument, from the first Unit's
// entry to the exit of the last).
//
// **campaignN, unitQuantity and each Unit's own fillPrice are the frozen
// numbers ADR 0006 requires, and the whole Add Ladder and Stop Ladder are
// derivable from them.** Under the Baseline a Unit is added every half a
// campaign N above the PREVIOUS Unit's actual fill (The Turtle Rules
// p.19-20) and each Protective Stop sits stopMultiple campaign N below its
// OWN Unit's fill (p.22): every rung of both ladders is arithmetic over
// those numbers and the frozen stopMultiple, with no recomputation of N
// while the Campaign is open. That is what makes replay reconstruct the
// same ladders rather than re-deriving different ones from the bars — the
// reason ADR 0006 requires the frozen values to be journaled, which
// event.CampaignOpenedPayload and event.CampaignUnitAddedPayload do.
//
// maxUnits is frozen from the Reducer's configured ConfigurationPayload.MaxUnits
// at the moment the Campaign opens (ADR 0008: 4 Units per instrument in the
// Baseline), exactly as campaignN and unitQuantity are: a later
// reconfiguration (which this system does not support mid-run in any case,
// see applyConfiguration) must never change how many Units an ALREADY OPEN
// Campaign may hold.
type campaignState struct {
	campaignID   string
	instrumentID string
	proposalID   string
	signalID     string
	direction    string
	campaignN    float64
	unitQuantity int64
	stopMultiple float64
	maxUnits     int
	openedAt     time.Time
	// units holds every CURRENTLY OPEN Unit, in ascending index order.
	// Non-empty for as long as the Campaign itself exists in state.campaign:
	// openCampaign always appends Unit 1 before returning, and the per-Unit
	// stop fill clears state.campaign entirely (not merely to an empty units
	// slice) the instant the last Unit closes — see applyStopFill. A
	// PARTIAL stop fill (the gap case) removes only the Units it named, so
	// this slice can shrink below maxUnits while the
	// Campaign is still open.
	units []unitState
	// unitsOpened is the TOTAL number of Units this Campaign has EVER held,
	// incremented once in openCampaign (to 1) and once per accepted Add —
	// never decremented when a partial stop fill removes a Unit from
	// units above. CampaignExitedPayload.Units reports this, not
	// len(units), because a per-Unit stop fill can close a Campaign's
	// Units across more than one fill (the gap case): by the time the LAST
	// Unit closes, units may already have shrunk from an earlier partial
	// close, but the exited record must still say how many Units the
	// Campaign held over its WHOLE life.
	unitsOpened int
	// partiallyStopped is true from the moment any stop fill closes SOME
	// but not all of this Campaign's Units, and never reset while the
	// Campaign remains open. The Turtle Rules p.23-24 describes a Whipsaw
	// variant in which Faith re-enters after such a partial stop-out; that
	// is explicitly out of scope for the Baseline (ADR 0012 — a declared
	// Variant, not assumed), so evaluateAdd refuses to propose a further
	// Unit for as long as this is true: the Campaign continues with
	// whatever Units remain until its own exit or full stop, never adding
	// to a position that has already started coming off.
	partiallyStopped bool
	// closedQuantity, closedEntryWeightedSum and closedExitWeightedSum
	// accumulate, across every stop fill that has already closed PART of
	// this Campaign, the facts a LATER closing fill (a further stop fill
	// that finally empties the Campaign, or an Exit-Channel fill closing
	// whatever Units remain) needs to report the Campaign's WHOLE-LIFE
	// aggregate result — see CampaignExitedPayload's own doc comment,
	// "Accumulating partial stop-outs", for the algebra this generalises
	// and why it reduces EXACTLY to the original single-fill formula
	// whenever no partial close has ever happened (closedQuantity stays 0
	// for the whole Campaign life when no partial stop has occurred).
	// Populated by applyStopFill on every stop fill, whether or not it happens to
	// empty the Campaign.
	closedQuantity         int64
	closedEntryWeightedSum float64
	closedExitWeightedSum  float64
	// lastCloseFillAt is the FilledAt of the most recently ACCEPTED closing
	// fill for this Campaign — a stop fill (partial or final) or the final
	// exit fill alike — and the zero time.Time before any closing fill has
	// ever been accepted (every fill's FilledAt is required non-zero, so
	// the zero value is unambiguous as "never"). Two successive partial
	// stop fills carry no other ordering guarantee
	// against EACH OTHER — campaign.openedAt only bounds the FIRST closing
	// fill, not a second, third, and so on — so without this, a
	// later-delivered fill timestamped BEFORE an already-accepted partial
	// close would be accepted, leaving the Campaign's own closing history
	// non-chronological (a Unit shown as closing before an earlier Unit
	// that, by the timestamps, closed after it). Checked in both
	// applyStopFill and applyExitFill against every closing fill, equal
	// timestamps allowed (The Turtle Rules p.19: several Units can execute
	// "in one day", the same allowance a same-bar Add chain gets).
	lastCloseFillAt time.Time
}

// lastUnit returns the most recently accepted Unit — the one the NEXT Add's
// rung is measured from (The Turtle Rules p.19: "measured from the actual
// fill of the PREVIOUS Unit").
func (c *campaignState) lastUnit() unitState {
	return c.units[len(c.units)-1]
}

// filledQuantity is the Campaign's whole position: the sum of every held
// Unit's own quantity. An exit fill closes the whole Campaign (a stop fill
// can close a per-Unit subset instead), so this is what a closing fill's own
// Quantity must match, and what an exit proposal is raised for.
func (c *campaignState) filledQuantity() int64 {
	var total int64
	for _, u := range c.units {
		total += u.quantity
	}
	return total
}

// entryPrice is the Campaign's quantity-weighted average fill price across
// every held Unit — see event.CampaignExitedPayload's doc comment ("Multi-Unit
// aggregation") for why this is the aggregate that keeps RealisedResult's
// derivation identical whether the Campaign held one Unit or four. For a
// single-Unit Campaign this is exactly that Unit's own fillPrice, so a
// Campaign that only ever opened one Unit is unaffected.
func (c *campaignState) entryPrice() float64 {
	var weighted float64
	var quantity int64
	for _, u := range c.units {
		weighted += float64(u.quantity) * u.fillPrice
		quantity += u.quantity
	}
	return weighted / float64(quantity)
}

// lifeAggregate combines whatever earlier partial closes this Campaign has
// already accumulated (c.closedQuantity/closedEntryWeightedSum/closedExitWeightedSum)
// with THIS closing fill's own contribution — thisQuantity Units, whose
// combined entry-weighted sum is thisEntryWeightedSum, all closing at price
// — into the Campaign's WHOLE-LIFE Quantity, EntryPrice and ExitPrice. See
// event.CampaignExitedPayload's own doc comment, "Accumulating partial
// stop-outs", for the algebra and why the result is EXACTLY the original
// single-fill formula (entryPrice = c.entryPrice(), exitPrice = price)
// whenever c.closedQuantity is still 0 — every Campaign that has never had a
// partial stop, byte for byte. Shared by applyStopFill's own final close
// and applyExitFill: an exit-channel fill closing whatever Units survived
// an earlier partial stop must aggregate the WHOLE life exactly as a final
// stop fill does, not merely the Units it happens to close itself.
//
// thisQuantity/thisEntryWeightedSum describe the Units THIS fill closes —
// campaign.units in full for an exit fill (which always closes the whole
// remaining position), or only the named subset for a stop fill (the
// per-Unit closing) — computed by the caller, which already has the
// distinction to make.
func (c *campaignState) lifeAggregate(thisQuantity int64, thisEntryWeightedSum, price float64) (quantity int64, entryPrice, exitPrice float64) {
	if c.closedQuantity == 0 {
		// Whenever no partial close has ever happened, thisQuantity and
		// thisEntryWeightedSum always describe EVERY Unit c.units still
		// holds (both callers close the whole remaining position in that
		// case), so c.entryPrice()'s own tested computation is exactly this
		// branch's answer — reused directly rather than re-derived from the
		// caller's own aggregates.
		return thisQuantity, c.entryPrice(), price
	}
	quantity = c.closedQuantity + thisQuantity
	entryPrice = (c.closedEntryWeightedSum + thisEntryWeightedSum) / float64(quantity)
	// The product is rounded before the sum: see docs/development.md,
	// floating-point determinism. This expression is the one that fused on
	// arm64 and made a journal architecture-dependent.
	exitPrice = (c.closedExitWeightedSum + float64(float64(thisQuantity)*price)) / float64(quantity)
	return quantity, entryPrice, exitPrice
}

// protectiveStop is the MINIMUM of every held Unit's own protectiveStop —
// the level at which the Campaign's protection is FIRST breached, and (since
// every fill stays at or above its own rung, ADR 0005 rule 1: "a long fills
// at max(level, open)") always Unit 1's own stop, because fills are
// strictly increasing while every Unit's distance-to-stop (stopMultiple x
// campaignN) stays the same. Reported on every strategy.campaign.evaluated
// event and checked by checkCampaignHasAProtectiveStop's invariant; the
// Stop Ladder moves this by raising earlier Units' stops rather than
// leaving them at their own original level.
func (c *campaignState) protectiveStop() float64 {
	stop := c.units[0].protectiveStop
	for _, u := range c.units[1:] {
		if u.protectiveStop < stop {
			stop = u.protectiveStop
		}
	}
	return stop
}

// openRiskUnits maps every currently-held Unit onto sizing.UnitOpenRisk, in
// the SAME ascending index order they are stored in — the shape
// sizing.AggregateOpenRisk needs (.greptile/rules.md's "risk multiplication
// when pyramiding" failure mode). Callers pass the result straight to
// sizing.AggregateOpenRisk and, separately, build
// event.CampaignEvaluatedPayload.Units from the SAME campaign.units slice —
// the identical order both places read it in is what makes the reducer's
// own computation and event.CampaignEvaluatedPayload.Validate's
// re-derivation agree bit for bit.
func (c *campaignState) openRiskUnits() []sizing.UnitOpenRisk {
	units := make([]sizing.UnitOpenRisk, len(c.units))
	for i, u := range c.units {
		units[i] = sizing.UnitOpenRisk{EntryPrice: u.fillPrice, ProtectiveStop: u.protectiveStop, Quantity: u.quantity}
	}
	return units
}

// aggregateOpenRisk returns the Campaign's aggregate open risk, computed by
// the one shared function event.CampaignEvaluatedPayload.Validate also
// calls (sizing.AggregateOpenRisk) — see openRiskUnits' own doc comment.
func (c *campaignState) aggregateOpenRisk(dollarsPerPoint float64) (float64, error) {
	return sizing.AggregateOpenRisk(c.openRiskUnits(), dollarsPerPoint)
}

// resolveUnits looks up every id in ids (a stop fill's UnitIDs) against
// this Campaign's CURRENTLY held Units, by openingFillID. It returns the
// matched Units in ASCENDING index order — regardless of the order ids
// itself named them in, so event.CampaignUnitsStoppedPayload.UnitIndexes,
// which requires strictly ascending indexes, never depends on a producer's
// own ordering — and every id from ids that did not resolve, so the caller
// can fail closed naming exactly which ones: an id naming a Unit this
// Campaign never held, or one an EARLIER stop fill already closed, is a
// reconciliation failure either way (docs/architecture.md), and both read
// identically from here — "not currently in c.units" — which is exactly
// right: this function has no way to tell "never existed" from "already
// closed" apart, and does not need to, since the caller's error message
// covers both.
func (c *campaignState) resolveUnits(ids []string) (found []unitState, missing []string) {
	byFillID := make(map[string]unitState, len(c.units))
	for _, u := range c.units {
		byFillID[u.openingFillID] = u
	}
	for _, id := range ids {
		u, ok := byFillID[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		found = append(found, u)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].index < found[j].index })
	return found, missing
}

// removeUnits deletes every Unit named in toRemove (by openingFillID) from
// c.units, preserving the ascending order of whatever remains. Called only
// after every payload a stop fill will be journalled as has already been
// validated (openCampaign's own discipline, applied here): a Unit that
// could not be recorded as closed must not disappear from state either.
func (c *campaignState) removeUnits(toRemove []unitState) {
	remove := make(map[string]bool, len(toRemove))
	for _, u := range toRemove {
		remove[u.openingFillID] = true
	}
	remaining := make([]unitState, 0, len(c.units)-len(toRemove))
	for _, u := range c.units {
		if !remove[u.openingFillID] {
			remaining = append(remaining, u)
		}
	}
	c.units = remaining
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
	// unitIDs is a stop fill's own FillPayload.UnitIDs,
	// compared alongside every other field so a re-delivered stop fill that
	// reuses a FillID but names a DIFFERENT set of Units is caught as a
	// reconciliation failure (a reused id with different contents), not
	// silently accepted as the identical fact. Always nil for every other
	// Kind, matching FillPayload.Validate's own closed-shape rule.
	unitIDs []string
	// level, slippageApplied and commission are compared
	// alongside every other field for exactly the reason unitIDs is: a
	// re-delivered fill that reuses a FillID but states a different level, a
	// different slippage or a different commission is not the identical
	// fact, and a reused identifier carrying different contents is a
	// reconciliation failure, not a duplicate delivery. The reducer makes no
	// other use of them — it never re-derives a fill's price from its level,
	// and the fill model and cost model both belong to the producer (ADR
	// 0005/0013) — but it must not silently absorb a producer that changed
	// them.
	level           float64
	slippageApplied float64
	commission      float64
}

// acceptedFillFromPayload builds the record applyFill stores for fill once
// it has been accepted (whether it opened or closed a Campaign, in whole or
// in part).
func acceptedFillFromPayload(fill event.FillPayload) acceptedFillState {
	return acceptedFillState{
		instrumentID:    fill.InstrumentID,
		kind:            fill.Kind,
		proposalID:      fill.ProposalID,
		campaignID:      fill.CampaignID,
		quantity:        fill.Quantity,
		price:           fill.Price,
		direction:       fill.Direction,
		filledAt:        fill.FilledAt,
		unitIDs:         fill.UnitIDs,
		level:           fill.Level,
		slippageApplied: fill.SlippageApplied,
		commission:      fill.Commission,
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
		a.filledAt.Equal(fill.FilledAt) &&
		slices.Equal(a.unitIDs, fill.UnitIDs) &&
		// Bit-for-bit, deliberately. This is not a price comparison
		// deciding whether a level was reached (.greptile/rules.md's rule);
		// it is an identity comparison asking whether two deliveries state
		// the identical recorded fact, and a tolerance there would let a
		// genuinely different execution pass as a duplicate.
		a.level == fill.Level &&
		a.slippageApplied == fill.SlippageApplied &&
		a.commission == fill.Commission
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
		// A decline proposes nothing, so there is nothing to fill and
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
// completes the lifecycle a Signal starts — a Signal is never followed by
// silence, and now neither is a proposal: every one reaches a Campaign or
// an expiry.
//
// This exact payload and event type is reused for an outstanding EXIT
// proposal too (expireExitProposal, below), naming the difference with
// Kind rather than minting a second event.
func (r *Reducer) expireEntryProposal(state *instrumentState, bar event.CompletedBarPayload, input event.Envelope) (event.Envelope, error) {
	pending := state.pendingProposal
	state.pendingProposal = nil

	payload := event.ProposalExpiredPayload{
		InstrumentID:   bar.InstrumentID,
		Kind:           event.ProposalKindEntry,
		ProposalID:     pending.proposalID,
		SignalID:       pending.signalID,
		PeriodEnd:      pending.periodEnd,
		ExpiredAt:      bar.PeriodEnd,
		EarliestFillAt: pending.earliestFillAt,
		Rule:           event.RuleSignalExpiresWithItsBar,
		ADR:            event.ADRSignalExpiry,
		Reason:         event.ExpiryReasonSupersededByNextBar,
		Quantity:       pending.quantity,
		Level:          pending.entryLevel,
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

// pendingExitProposalState is the exit-side mirror of pendingProposalState:
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
//
// earliestFillAt is the exit-side twin of pendingProposalState's own field of
// the same name (see Reducer.applyFill's doc comment, "The window a fill's
// timestamp must lie in"): the period end of the bar BEFORE the breach bar —
// the moment the breach bar opened, and so the earliest instant at which an
// order for this exit proposal could have executed. It degrades correctly at
// its zero value for the same reason pendingProposalState's does: a breach
// can only happen once a Campaign is open, which itself requires a Signal,
// which requires a warm N and Entry Channel — so a real bar always precedes
// a breach bar in practice, but the zero time needs no special case either
// way, since every real timestamp is after it.
type pendingExitProposalState struct {
	proposalID     string
	periodEnd      time.Time
	quantity       int64
	level          float64
	earliestFillAt time.Time
}

// expireExitProposal ends an outstanding exit proposal that the next
// completed bar has superseded, and returns the event that records it —
// the exit-side mirror of expireEntryProposal, reusing the identical
// event.ProposalExpiredPayload with Kind ProposalKindExit rather than a
// second event type (see that payload's own doc comment). Unlike an
// entry-kind expiry, SignalID is left empty: an exit proposal is not sized
// from a Signal at all (event.ExitProposalPayload's doc comment).
func (r *Reducer) expireExitProposal(state *instrumentState, bar event.CompletedBarPayload, input event.Envelope) (event.Envelope, error) {
	pending := state.pendingExitProposal
	state.pendingExitProposal = nil

	payload := event.ProposalExpiredPayload{
		InstrumentID:   bar.InstrumentID,
		Kind:           event.ProposalKindExit,
		ProposalID:     pending.proposalID,
		SignalID:       "",
		PeriodEnd:      pending.periodEnd,
		ExpiredAt:      bar.PeriodEnd,
		EarliestFillAt: pending.earliestFillAt,
		Rule:           event.RuleExitProposalExpiresWithItsBar,
		ADR:            event.ADRSignalExpiry,
		Reason:         event.ExpiryReasonSupersededByNextBar,
		Quantity:       pending.quantity,
		Level:          pending.level,
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

// pendingAddProposalState is the Add-side mirror of pendingExitProposalState:
// an Add proposal (strategy.add.proposed) that has been emitted and not yet
// resolved. It is NOT position state, for the identical reason
// pendingExitProposalState is not: nothing about the Campaign's own life
// depends on it, and a further Unit joins the Campaign only from a recorded
// Add fill — never from this proposal alone (see evaluateAdd and
// applyAddFill).
//
// It lives for one bar, the identical lifetime ADR 0011 gives every
// proposal in this system: the next completed bar for the instrument
// supersedes it (see Reducer.expireAddProposal) — including a proposal
// raised by the SAME-BAR Add chain (see applyAddFill), since that proposal
// is still attributed to the bar that produced the opportunity for it, not
// to the fill that happened to trigger the next rung's evaluation.
//
// earliestFillAt is the Add-side twin of pendingExitProposalState's own
// field of the same name: the earliest instant at which an order for THIS
// proposal could have executed. For the proposal evaluateCampaign's own bar
// evaluation raises, that is the period end of the bar BEFORE it (the
// moment it opened, matching every other proposal kind's own rule). For a
// proposal raised by the same-bar chain (from applyAddFill), it is the SAME
// bound — the bar that produced the opportunity for the whole chain is one
// and the same bar throughout, so every rung proposed within it shares its
// one opening moment as the earliest an execution could exist (see
// instrumentState.lastBarEarliestFillAt).
type pendingAddProposalState struct {
	proposalID       string
	periodEnd        time.Time
	unitIndex        int
	quantity         int64
	level            float64
	previousUnitFill float64
	earliestFillAt   time.Time
}

// expireAddProposal ends an outstanding Add proposal that the next completed
// bar has superseded, and returns the event that records it — the Add-side
// mirror of expireExitProposal, reusing the identical
// event.ProposalExpiredPayload with Kind ProposalKindAdd rather than a third
// event type. Like an exit-kind expiry, SignalID is left empty: an Add
// proposal is not sized from a Signal (event.AddProposalPayload's doc
// comment).
func (r *Reducer) expireAddProposal(state *instrumentState, bar event.CompletedBarPayload, input event.Envelope) (event.Envelope, error) {
	pending := state.pendingAddProposal
	state.pendingAddProposal = nil

	payload := event.ProposalExpiredPayload{
		InstrumentID:   bar.InstrumentID,
		Kind:           event.ProposalKindAdd,
		ProposalID:     pending.proposalID,
		SignalID:       "",
		PeriodEnd:      pending.periodEnd,
		ExpiredAt:      bar.PeriodEnd,
		EarliestFillAt: pending.earliestFillAt,
		Rule:           event.RuleAddProposalExpiresWithItsBar,
		ADR:            event.ADRSignalExpiry,
		Reason:         event.ExpiryReasonSupersededByNextBar,
		Quantity:       pending.quantity,
		Level:          pending.level,
	}
	if err := payload.Validate(); err != nil {
		return event.Envelope{}, fmt.Errorf("strategy: built invalid proposal expired payload: %w", err)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		// Unreachable, for the same reason expireExitProposal's marshal
		// guard is.
		return event.Envelope{}, fmt.Errorf("strategy: marshal proposal expired payload: %w", err)
	}
	// A distinct decisionID kind from the other two expiry paths' own, even
	// though at most one of the three can ever be outstanding for one
	// instrument at a time in practice — kept distinct for the same reason
	// expireExitProposal's own id is.
	return r.stamp(
		decisionID("add-proposal-expired", bar.InstrumentID, bar.PeriodEnd),
		event.ProposalExpiredEventType, event.ProposalExpiredSchemaVersion,
		bar.PeriodEnd, input, payloadBytes,
	), nil
}

// expireAddProposalForStop cancels an outstanding Add proposal the instant a
// stop fill closes PART of the same Campaign — the SECOND way an Add
// proposal can end, next to expireAddProposal's ordinary next-bar expiry
// (ADR 0011). Reused payload and event type (event.ProposalExpiredPayload /
// ProposalExpiredEventType), discriminated by
// event.ExpiryReasonSupersededByStop rather than a second event type — the
// same "one event type, a Reason discriminator" choice already made for the
// Stop Ladder's own raise (event.ProtectiveStopSetPayload).
//
// Unlike expireAddProposal, ExpiredAt is the CLOSING FILL's own timestamp,
// not a bar's PeriodEnd: the proposal was superseded by an execution, not by
// a later bar closing, and — since a resting stop can fill INSIDE the same
// bar that proposed the Add (ADR 0005) — ExpiredAt can legitimately equal or
// precede PeriodEnd here, unlike expireAddProposal's own next-bar case; see
// event.ProposalExpiredPayload's own doc comment for the reason-dependent
// rule this requires. applyAddFill's own partiallyStopped guard is a
// further, independent line of defence, belt and braces alongside
// cancelling the proposal outright here.
//
// **Deliberately does NOT mutate state.pendingAddProposal.** Building and
// validating this payload can fail — genuinely, not merely defensively —
// when the closing fill's own timestamp predates the proposal's own
// EarliestFillAt bound, and openCampaign's own discipline applies here
// exactly as everywhere else in this file: nothing may be committed to
// memory before every payload for the transition has validated. The caller
// (applyStopFill) clears state.pendingAddProposal itself, only after this
// envelope — and every other payload the same stop fill produces — has
// validated successfully.
func (r *Reducer) expireAddProposalForStop(state *instrumentState, fill event.FillPayload, input event.Envelope) (event.Envelope, error) {
	pending := state.pendingAddProposal

	payload := event.ProposalExpiredPayload{
		InstrumentID:   fill.InstrumentID,
		Kind:           event.ProposalKindAdd,
		ProposalID:     pending.proposalID,
		SignalID:       "",
		PeriodEnd:      pending.periodEnd,
		ExpiredAt:      fill.FilledAt,
		EarliestFillAt: pending.earliestFillAt,
		Rule:           event.RuleAddProposalSupersededByStop,
		ADR:            event.ADRSignalExpiry,
		Reason:         event.ExpiryReasonSupersededByStop,
		Quantity:       pending.quantity,
		Level:          pending.level,
	}
	if err := payload.Validate(); err != nil {
		return event.Envelope{}, fmt.Errorf("strategy: built invalid proposal expired payload: %w", err)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		// Unreachable, for the same reason expireAddProposal's marshal
		// guard is.
		return event.Envelope{}, fmt.Errorf("strategy: marshal proposal expired payload: %w", err)
	}
	// Keyed to the stop fill that superseded the proposal, not to the bar
	// that raised it: a distinct decisionID kind from expireAddProposal's
	// own, and further disambiguated by the fill's own id in case more than
	// one closing fill for this instrument's history ever shares a
	// timestamp.
	return r.stamp(
		decisionID(fmt.Sprintf("add-proposal-expired-by-stop-%s", fill.FillID), fill.InstrumentID, fill.FilledAt),
		event.ProposalExpiredEventType, event.ProposalExpiredSchemaVersion,
		fill.FilledAt, input, payloadBytes,
	), nil
}

// evaluateCampaign runs an open Campaign's per-bar decision: the
// Protective Stop and Exit Channel levels in force on this bar, journaled as
// a CampaignEvaluatedPayload regardless of whether either one triggers
// anything — this is the event that fills the hole that would otherwise
// leave a bar for an instrument in a Campaign emitting nothing at all.
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
//
// previousPeriodEnd is the period end of the bar BEFORE this one — the same
// value applyCompletedBar captures for the entry-proposal path, passed
// through here so a freshly raised exit proposal can record it as
// pendingExitProposalState.earliestFillAt (see that field's doc comment and
// applyExitFill's window check).
func (r *Reducer) evaluateCampaign(state *instrumentState, bar event.CompletedBarPayload, exitChannelLow float64, exitChannelReady bool, previousPeriodEnd time.Time, input event.Envelope) ([]event.Envelope, error) {
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

	// Every held Unit's own facts, in the SAME ascending order
	// campaign.units and campaign.openRiskUnits() share — the order both
	// this producer and event.CampaignEvaluatedPayload.Validate's
	// re-derivation read Units in, so the two agree bit for bit.
	units := make([]event.CampaignEvaluatedUnit, len(campaign.units))
	for i, u := range campaign.units {
		units[i] = event.CampaignEvaluatedUnit{UnitIndex: u.index, EntryPrice: u.fillPrice, Quantity: u.quantity, ProtectiveStop: u.protectiveStop}
	}
	aggregateOpenRisk, err := campaign.aggregateOpenRisk(r.dollarsPerPoint)
	if err != nil {
		// Unreachable from a Campaign this package itself built: every
		// Unit's own fillPrice/protectiveStop/quantity has already passed
		// sizing.ProtectiveStopLevel's and its own payload's Validate, and
		// r.dollarsPerPoint is a configured, already-validated positive
		// figure (event.ConfigurationPayload.Validate). Failing closed
		// anyway rather than panicking, matching this package's own style.
		return nil, fmt.Errorf("strategy: instrument %q: campaign %q cannot compute aggregate open risk: %w", bar.InstrumentID, campaign.campaignID, err)
	}
	notionalAccount := r.notionalAccount.Current()

	evaluatedPayload := event.CampaignEvaluatedPayload{
		CampaignID:                campaign.campaignID,
		InstrumentID:              bar.InstrumentID,
		PeriodEnd:                 bar.PeriodEnd,
		ProtectiveStop:            campaign.protectiveStop(),
		Units:                     units,
		ExitChannelLow:            reportedExitChannelLow,
		ExitChannelReady:          exitChannelReady,
		ExitConditionMet:          exitConditionMet,
		DollarsPerPoint:           r.dollarsPerPoint,
		AggregateOpenRisk:         aggregateOpenRisk,
		NotionalAccount:           notionalAccount,
		AggregateOpenRiskFraction: aggregateOpenRisk / notionalAccount,
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
	// 0011's mechanism rather than inventing a second one.
	proposalPayload := event.ExitProposalPayload{
		CampaignID:   campaign.campaignID,
		InstrumentID: bar.InstrumentID,
		PeriodEnd:    bar.PeriodEnd,
		Reason:       event.ExitReasonExitChannel,
		Level:        exitChannelLow,
		Quantity:     campaign.filledQuantity(),
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
		proposalID:     proposalEnvelope.ID,
		periodEnd:      bar.PeriodEnd,
		quantity:       campaign.filledQuantity(),
		level:          exitChannelLow,
		earliestFillAt: previousPeriodEnd,
	}

	return emissions, nil
}

// evaluateAdd is the Add Ladder evaluation: it computes the next rung from
// the Campaign's LAST Unit's ACTUAL fill (The Turtle Rules p.19-20: "add 1
// Unit every 1/2N measured from the actual fill of the previous Unit") and,
// if the relevant bar's high reached it, proposes adding the next Unit — a
// resting buy-stop at the rung fills the moment price reaches it (ADR 0005:
// "filled in the bar whose range first covers it"), so the comparison is
// >=, not the strict > an Entry Channel breakout uses.
//
// # Three call sites, one function (every Unit addable within one bar)
//
// This is called from three places:
//
//  1. applyCompletedBar, once per completed bar, AFTER the exit evaluation
//     (ADR 0010: exits before Adds) and only when this SAME bar did not
//     itself propose an exit (exit takes precedence, ADR 0010's other
//     ordering rule). Only reachable for a Campaign already open when
//     the bar arrives.
//  2. openCampaign, immediately after a new Campaign's first Unit fills:
//     if the entry proposal named the breakout bar's own high as the entry
//     level, Unit 1's rung would be structurally always above that bar's
//     own high and this call site could never fire; naming the Entry
//     Channel high instead (as EntryLevel does) removes that accidental
//     guarantee — the bar that opens a Campaign can also cover Unit 2's
//     rung, and without this call site that opportunity would be silently
//     skipped, since applyCompletedBar already processed this bar as a
//     Setup (no Campaign existed yet) and will never see it again.
//  3. applyAddFill, immediately after a new Unit's fill is accepted: the
//     THIRD and FOURTH rungs each depend on the PREVIOUS Unit's actual
//     fill, which is unknown until that fill arrives, so the opportunity
//     for the NEXT Unit can only be evaluated once it has. If the bar that
//     raised the JUST-EXECUTED proposal also covers the next rung, this
//     re-evaluation proposes it immediately — still attributed to that same
//     bar — which is how all four Units can be added within one bar from
//     resting orders, each rung faithfully measured from the fill before it
//     (The Turtle Rules p.19-20: "all four could be added in one day").
//
// All three call sites therefore need the SAME bar's high, period end and
// earliest-fill-at bound, even though the second and third run from a
// fill's own Apply call — a later call than the bar event that produced the
// opportunity. That is why all three read state.lastBarHigh/lastBarPeriodEnd/
// lastBarEarliestFillAt (set once per completed bar, in applyCompletedBar)
// rather than bar-local values only the first call site would have.
//
// A campaign already at its configured maximum Units proposes nothing —
// silently, since a fully-Loaded Campaign is an ordinary state, not an
// error.
func (r *Reducer) evaluateAdd(state *instrumentState, input event.Envelope) ([]event.Envelope, error) {
	campaign := state.campaign
	if campaign.partiallyStopped {
		// The Turtle Rules p.23-24 describes a Whipsaw variant in
		// which Faith re-enters after a partial stop-out; that is a
		// declared Variant, not the Baseline (ADR 0012), so once any Unit
		// has been stopped out, this Campaign continues with whatever
		// Units remain until its own exit or full stop — never a further
		// Add. Checked here, the one place both call sites (this
		// function's own two: applyCompletedBar's per-bar evaluation, and
		// the same-bar chain from applyAddFill) funnel through, rather
		// than at each call site separately.
		return nil, nil
	}
	if len(campaign.units) >= campaign.maxUnits {
		return nil, nil
	}

	last := campaign.lastUnit()
	rung, err := sizing.NextAddLevel(last.fillPrice, campaign.campaignN, sizing.DirectionLong)
	if err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: campaign %q cannot compute the next add rung: %w", campaign.instrumentID, campaign.campaignID, err)
	}
	if state.lastBarHigh < rung {
		return nil, nil
	}

	unitIndex := len(campaign.units) + 1
	payload := event.AddProposalPayload{
		CampaignID:       campaign.campaignID,
		InstrumentID:     campaign.instrumentID,
		PeriodEnd:        state.lastBarPeriodEnd,
		UnitIndex:        unitIndex,
		Level:            rung,
		Quantity:         campaign.unitQuantity,
		PreviousUnitFill: last.fillPrice,
		CampaignN:        campaign.campaignN,
		Rule:             event.RuleAddLadderHalfN,
		ADR:              event.ADRCampaignFrozenAtEntry,
	}
	if err := payload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: built invalid add proposal payload: %w", campaign.instrumentID, err)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		// Unreachable: every field is a string, an int, an int64, a float64
		// or a time.Time, none of which can fail to marshal. Failing closed
		// rather than panicking, in case the payload ever grows a field that
		// can.
		return nil, fmt.Errorf("strategy: marshal add proposal payload: %w", err)
	}

	// The decisionID kind carries the unit index because, unlike an entry or
	// exit proposal, more than one Add proposal CAN belong to the same bar
	// (the same-bar chain above) — without it, the second proposal in a
	// chain would collide with the first's id instead of getting its own.
	proposalEnvelope := r.stamp(
		decisionID(fmt.Sprintf("add-proposal-unit-%d", unitIndex), campaign.instrumentID, state.lastBarPeriodEnd),
		event.AddProposalEventType, event.AddProposalSchemaVersion,
		state.lastBarPeriodEnd, input, payloadBytes,
	)

	// Remembered so a fill executing it can be checked against it, and so a
	// later bar with no fill knows to expire it — the identical reason
	// rememberPendingProposal and evaluateCampaign's own remembering exist.
	// No Campaign state moves here: applyAddFill is this reducer's only path
	// to actually adding a Unit.
	state.pendingAddProposal = &pendingAddProposalState{
		proposalID:       proposalEnvelope.ID,
		periodEnd:        state.lastBarPeriodEnd,
		unitIndex:        unitIndex,
		quantity:         campaign.unitQuantity,
		level:            rung,
		previousUnitFill: last.fillPrice,
		earliestFillAt:   state.lastBarEarliestFillAt,
	}

	return []event.Envelope{proposalEnvelope}, nil
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
// The comparison is against campaign.lastUnit().filledAt, not
// campaign.openedAt (Unit 1's own fill): an Add fill can bring a LATER Unit
// into being after the Campaign already opened, including several within
// the same bar (the same-bar Add chain, see applyAddFill), so the bound
// this check enforces must track whichever Unit was accepted most recently,
// not only the first. Before any Add exists, lastUnit() is Unit 1 and this
// is exactly the single-Unit behaviour unchanged.
//
// Only the first bar after a Unit's fill was accepted can fail this, since
// the run stops when it does and every later bar ends after the one that
// did not.
func checkBarConfirmsCampaignOpening(state *instrumentState, bar event.CompletedBarPayload) error {
	if state.campaign == nil {
		return nil
	}
	last := state.campaign.lastUnit()
	if !bar.PeriodEnd.Before(last.filledAt) {
		return nil
	}
	return fmt.Errorf("strategy: instrument %q: bar period end %s predates campaign %q's most recently accepted unit fill %q timestamped %s; an execution cannot have happened after a bar that had not yet completed, so the fill's timestamp is inconsistent with the bar stream",
		bar.InstrumentID, bar.PeriodEnd.Format(time.RFC3339),
		state.campaign.campaignID, last.openingFillID,
		last.filledAt.Format(time.RFC3339))
}

// checkBarConfirmsCampaignClosing is checkBarConfirmsCampaignOpening's mirror
// for the CLOSING fill: the identical contradiction,
// checked at the identical point, for a stop or exit fill instead of an
// opening one. A closing fill's FilledAt cannot be validated against "the
// next bar" the moment the fill is applied (applyStopFill/applyExitFill), for
// the same reason applyFill cannot validate an opening fill's upper bound —
// the next bar does not exist yet, and no bar length is configured — so it
// is checked here instead, the instant the next completed bar for the
// instrument arrives.
//
// Unlike checkBarConfirmsCampaignOpening, this is NOT gated on
// state.campaign being non-nil: state.lastClosingFillAt is set precisely
// WHEN a Campaign closes, so the check that matters happens with
// state.campaign already nil. It is also never reset afterwards, and that is
// safe rather than a bug: applyCompletedBar's own bar-chronology check
// (above, in the caller) already enforces that a given instrument's bar
// PeriodEnd strictly increases across the WHOLE run, so once one bar has
// passed this check, every later bar's PeriodEnd is later still and can
// never fail it again — including every bar of any LATER Campaign the same
// instrument goes on to open. Only the first bar after a Campaign's close
// can therefore ever fail this, the same property
// checkBarConfirmsCampaignOpening's own doc comment states for the opening
// side.
func checkBarConfirmsCampaignClosing(state *instrumentState, bar event.CompletedBarPayload) error {
	if state.lastClosingFillAt.IsZero() || !bar.PeriodEnd.Before(state.lastClosingFillAt) {
		return nil
	}
	return fmt.Errorf("strategy: instrument %q: bar period end %s predates the fill timestamped %s that most recently closed a campaign for this instrument; an execution cannot have happened after a bar that had not yet completed, so the fill's timestamp is inconsistent with the bar stream",
		bar.InstrumentID, bar.PeriodEnd.Format(time.RFC3339),
		state.lastClosingFillAt.Format(time.RFC3339))
}

// applyFill handles event.FillEventType: the only input in this system that
// may change position state.
//
// A fill is an external fact the reducer did not produce and cannot re-derive
// — from a fixture, from the fill simulator, or from the LEAN adapter — so
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
//     opened a Campaign, closed one, or any later kind, for whichever
//     instrument it named — is an idempotent no-op if the
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
//     id is deliberately refused (see applyFillToOpenCampaign) — rule 2
//     has already resolved every
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
// Note that the upper bound is not the same rule as ADR 0011's expiry, which
// handles a fill *arriving* after the next bar (the proposal is gone, so
// there is nothing pending to match); this one handles a fill arriving in
// time but *claiming* a time after it.
//
// An exit fill (applyExitFill) is bound by the IDENTICAL window, against
// the breach bar that raised the outstanding exit proposal rather than the
// entry's decision bar: the lower bound is
// pendingExitProposalState.earliestFillAt, checked in applyExitFill exactly
// as pendingProposalState.earliestFillAt is checked here; the upper bound is
// checkBarConfirmsCampaignClosing, checkBarConfirmsCampaignOpening's own
// mirror for a CLOSING fill (stop or exit), called from applyCompletedBar
// alongside it. A stop fill is not bound this way at all: it closes a
// Campaign directly from the Protective Stop, with no proposal of its own to
// bound against (see applyStopFill's own doc comment) — only its lower bound
// (not before the Campaign opened) is checked, the same as it always has
// been.
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
		if fill.Kind == event.FillKindStop || fill.Kind == event.FillKindExit || fill.Kind == event.FillKindAdd {
			return nil, fmt.Errorf("strategy: %s fill %q names campaign %q for instrument %q, which this reducer has never evaluated; a fill for a campaign this strategy has no history for is a reconciliation failure, not something to absorb (docs/architecture.md)",
				fill.Kind, fill.FillID, fill.CampaignID, fill.InstrumentID)
		}
		return nil, fmt.Errorf("strategy: fill %q names proposal %q for instrument %q, which this reducer has never evaluated; a fill for an order this strategy never proposed is a reconciliation failure, not something to absorb (docs/architecture.md)",
			fill.FillID, fill.ProposalID, fill.InstrumentID)
	}

	// A stop fill, an exit fill or an Add fill each take a
	// completely different path from an entry fill — they act on an ALREADY
	// open Campaign rather than opening one — so all three are dispatched
	// before any of the entry-fill logic below runs. fill.Validate() has
	// already rejected any Kind other than event.FillKindEntry,
	// event.FillKindStop, event.FillKindExit or event.FillKindAdd, so the
	// fall-through below is reached only for an entry fill.
	if fill.Kind == event.FillKindStop {
		return r.applyStopFill(state, fill, envelope)
	}
	if fill.Kind == event.FillKindExit {
		return r.applyExitFill(state, fill, envelope)
	}
	if fill.Kind == event.FillKindAdd {
		return r.applyAddFill(state, fill, envelope)
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
//     to a future change; until it lands, rejecting is the only safe
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
		fill.InstrumentID, campaign.campaignID, campaign.units[0].openingFillID, fill.FillID)
}

// openCampaign brings a Campaign into being from the fill that executed its
// proposal, and returns the Campaign-opened and Protective-Stop-set events
// that record it — plus, if the SAME bar that produced this fill also covers
// Unit 2's own rung, the same-bar Add chain evaluateAdd continues from
// applyAddFill (see evaluateAdd's own doc comment, the second of its three
// call sites).
//
// The campaign N and the Unit share count are taken from the proposal, not
// recomputed: ADR 0006 freezes them at first entry, and recomputing at fill
// time would make the frozen values depend on when the fill arrived. The
// entry price and the filled quantity are taken from the fill, not from the
// proposal: what actually executed is the only thing the position consists of.
//
// A partial fill opens a Campaign for the filled quantity, while the Unit
// share count stays frozen at the full proposed size — the Unit is the risk
// measure the caps are counted in (ADR
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
// Every open Campaign has a Protective Stop from the moment it exists —
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

	// The Protective-Stop-set decision, built and validated before any
	// state moves, for the same reason the Campaign-opened payload is —
	// see this function's doc comment.
	stopSetPayload := event.ProtectiveStopSetPayload{
		CampaignID:    campaignID,
		InstrumentID:  fill.InstrumentID,
		UnitIndex:     1,
		Reason:        event.ProtectiveStopReasonInitial,
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
		campaignID:   campaignID,
		instrumentID: fill.InstrumentID,
		proposalID:   pending.proposalID,
		signalID:     pending.signalID,
		direction:    fill.Direction,
		campaignN:    pending.n,
		unitQuantity: pending.quantity,
		stopMultiple: pending.stopMultiple,
		maxUnits:     r.maxUnits,
		openedAt:     fill.FilledAt,
		units: []unitState{{
			index:          1,
			openingFillID:  fill.FillID,
			fillPrice:      fill.Price,
			quantity:       fill.Quantity,
			protectiveStop: protectiveStop,
			filledAt:       fill.FilledAt,
		}},
		unitsOpened: 1,
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
	// Campaign-opened then Protective-Stop-set.
	emissions := []event.Envelope{
		r.stamp(campaignID, event.CampaignOpenedEventType, event.CampaignOpenedSchemaVersion, fill.FilledAt, input, openedPayloadBytes),
		r.stamp(decisionID("protective-stop-set", fill.InstrumentID, fill.FilledAt), event.ProtectiveStopSetEventType, event.ProtectiveStopSetSchemaVersion, fill.FilledAt, input, stopSetPayloadBytes),
	}

	// If the entry filled at the breakout bar's own high, Unit 1's rung
	// (fill + 1/2N) would always sit ABOVE that bar's own high and could
	// never be covered by it — evaluateAdd's same-bar chain (see its own doc
	// comment) would only ever need to start from applyAddFill, because the
	// entry's own bar would be structurally never a candidate. Naming the
	// Entry Channel high as the entry level instead removes that accidental
	// guarantee: the SAME bar that fills the entry can also cover Unit 2's
	// rung (The Turtle Rules p.19-20's "all four could be added in one
	// day"). This is the third call site the chain needs, using the identical path
	// applyAddFill uses rather than a second one: state.lastBarHigh/
	// lastBarPeriodEnd/lastBarEarliestFillAt already name the breakout bar
	// (set when it was processed as a bar, before this fill arrived), exactly
	// what evaluateAdd reads.
	chainEmissions, err := r.evaluateAdd(state, input)
	if err != nil {
		return nil, err
	}
	emissions = append(emissions, chainEmissions...)

	return emissions, nil
}

// applyStopFill handles a fill.Kind == event.FillKindStop delivery: the way
// a Campaign's Units close on their own Protective Stop. An Exit-Channel
// exit and a delisting exit each produce their own kind of terminal
// fact, sharing event.CampaignExitedPayload with their own Reason rather
// than a new event type.
//
// **No decision about WHETHER a stop was hit is made here.** That is the
// fill simulator's job, comparing a bar's low against a Unit's own Protective
// Stop level under ADR 0005. This function only ever reacts to a fill event
// that already says one or more Units' own stop was hit — it never reads
// bar data, and nothing in this package compares a price to a Unit's
// protectiveStop except the capital-safety invariant check
// (checkCampaignHasAProtectiveStop), which checks the stop's OWN shape,
// never a bar's price against it. Likewise, WHETHER a named Unit's stop
// level was actually reachable by this fill's price (the gap rule) is not
// checked here either: ADR 0005 makes the simulator the sole authority on
// fill legitimacy, the same restraint FillPayload.UnitIDs's own doc comment
// states.
//
// # Per-Unit closing (the gap case)
//
// A stop fill names, via FillIDs.UnitIDs, exactly which Units it closes —
// possibly not all of them: The Turtle Rules p.23's gap case leaves a later
// Unit's stop at a genuinely different level from earlier ones, so one
// Unit's stop can be hit while the others' have not been. Every named id is
// resolved against the Campaign's CURRENTLY held Units (campaignState.resolveUnits);
// an id that does not resolve — naming a Unit this Campaign never held, or
// one an earlier stop fill already closed — fails closed as a
// reconciliation failure, never silently ignored.
//
// strategy.campaign.units-stopped (CampaignUnitsStoppedPayload) is emitted
// for every accepted stop fill, whether or not it happens to close the
// Campaign's last remaining Unit. When it does, strategy.campaign.exited
// (Reason ExitReasonStop) follows immediately after, in the SAME Apply
// return, aggregating the Campaign's WHOLE life — see
// CampaignExitedPayload's own doc comment, "Accumulating partial
// stop-outs", for how a stop-out spread across more than one fill
// accumulates into that final record, and why the algebra reduces EXACTLY
// to the original single-fill formula whenever (as in a fixture that has
// never had a partial close) it never was.
//
// Once any stop fill closes PART of a Campaign while Units remain,
// campaign.partiallyStopped is set: evaluateAdd refuses to propose any
// further Unit for the rest of this Campaign's life (The Turtle Rules
// p.23-24's Whipsaw variant — Faith re-entering after a partial stop-out —
// is out of scope for the Baseline, ADR 0012), and the Campaign simply
// continues with whatever Units remain until its own exit or full stop.
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
	if fill.FilledAt.Before(campaign.openedAt) {
		return nil, fmt.Errorf("strategy: instrument %q: stop fill %q is timestamped %s, which predates campaign %q's own opening fill at %s; a campaign cannot be closed before it opened",
			fill.InstrumentID, fill.FillID, fill.FilledAt.Format(time.RFC3339), campaign.campaignID, campaign.openedAt.Format(time.RFC3339))
	}
	// campaign.openedAt only bounds the FIRST closing fill this Campaign
	// ever sees; it says
	// nothing about ordering between a SECOND (or third) partial stop and
	// the one(s) before it. Without this, a later-delivered fill
	// timestamped before an already-accepted closing fill would be
	// accepted, leaving the Campaign's own closing history
	// non-chronological. Equal timestamps are allowed — several Units can
	// close at the same instant (The Turtle Rules p.19's "all four could be
	// added in one day" allowance, mirrored here for closing).
	if !campaign.lastCloseFillAt.IsZero() && fill.FilledAt.Before(campaign.lastCloseFillAt) {
		return nil, fmt.Errorf("strategy: instrument %q: stop fill %q is timestamped %s, which predates campaign %q's most recently accepted closing fill at %s; a later closing fill cannot have executed before an earlier one",
			fill.InstrumentID, fill.FillID, fill.FilledAt.Format(time.RFC3339), campaign.campaignID, campaign.lastCloseFillAt.Format(time.RFC3339))
	}

	// Resolve every named Unit against the Campaign's CURRENTLY held
	// Units. fill.Validate() has already required UnitIDs to be non-empty
	// and free of duplicates for a stop fill.
	closingUnits, missing := campaign.resolveUnits(fill.UnitIDs)
	if len(missing) > 0 {
		return nil, fmt.Errorf("strategy: instrument %q: stop fill %q names unit(s) %v for campaign %q, which are unknown or already closed; a stop fill naming an unknown or already-closed unit is a reconciliation failure (docs/architecture.md)",
			fill.InstrumentID, fill.FillID, missing, campaign.campaignID)
	}
	var closingQuantity int64
	for _, u := range closingUnits {
		closingQuantity += u.quantity
	}
	if fill.Quantity != closingQuantity {
		return nil, fmt.Errorf("strategy: instrument %q: stop fill %q executed %d but the named unit(s) hold %d; a partial fill against the named units is rejected — accumulating a partial fill into one unit's own close is not supported, the same limitation a partial entry fill is held to",
			fill.InstrumentID, fill.FillID, fill.Quantity, closingQuantity)
	}

	// The level that was in force, for THESE Units, at the moment this fill
	// closed them: the minimum across the Units it actually names (never
	// across the whole Campaign, which may hold other Units at other
	// levels once the Stop Ladder has diverged — the gap case).
	stopLevel := closingUnits[0].protectiveStop
	for _, u := range closingUnits[1:] {
		if u.protectiveStop < stopLevel {
			stopLevel = u.protectiveStop
		}
	}

	// This fill's OWN share: the quantity-weighted average entry of ONLY
	// the Units it closes, and its realised result against that average —
	// event.CampaignUnitsStoppedPayload's own figures, independent of
	// whether any other fill has ever closed part of this Campaign before.
	var thisEntryWeightedSum float64
	unitIndexes := make([]int, len(closingUnits))
	for i, u := range closingUnits {
		thisEntryWeightedSum += float64(u.quantity) * u.fillPrice
		unitIndexes[i] = u.index
	}
	thisEntryPrice := thisEntryWeightedSum / float64(closingQuantity)
	thisRealisedResult := float64(closingQuantity) * (fill.Price - thisEntryPrice) * r.dollarsPerPoint

	remainingAfter := len(campaign.units) - len(closingUnits)

	// AggregateOpenRiskAfter (event.CampaignUnitsStoppedPayload's own
	// required shape): 0 when nothing remains, else computed over the
	// Units THIS fill does NOT close, exactly as they stand right now
	// (their own stops are unaffected by a stop fill — only an Add ever
	// moves a stop, via the Stop Ladder). Computed before removal.
	var aggregateOpenRiskAfter float64
	if remainingAfter > 0 {
		remainingUnits := make([]sizing.UnitOpenRisk, 0, remainingAfter)
		closing := make(map[string]bool, len(closingUnits))
		for _, u := range closingUnits {
			closing[u.openingFillID] = true
		}
		for _, u := range campaign.units {
			if !closing[u.openingFillID] {
				remainingUnits = append(remainingUnits, sizing.UnitOpenRisk{EntryPrice: u.fillPrice, ProtectiveStop: u.protectiveStop, Quantity: u.quantity})
			}
		}
		var err error
		aggregateOpenRiskAfter, err = sizing.AggregateOpenRisk(remainingUnits, r.dollarsPerPoint)
		if err != nil {
			// Unreachable: every remaining Unit's own figures have already
			// passed sizing.ProtectiveStopLevel's and its own payload's
			// Validate. Failing closed anyway, matching this package's style.
			return nil, fmt.Errorf("strategy: instrument %q: stop fill %q cannot compute the remaining aggregate open risk: %w", fill.InstrumentID, fill.FillID, err)
		}
	}

	unitsStoppedPayload := event.CampaignUnitsStoppedPayload{
		CampaignID:             campaign.campaignID,
		InstrumentID:           fill.InstrumentID,
		FillID:                 fill.FillID,
		UnitIndexes:            unitIndexes,
		FillPrice:              fill.Price,
		QuantityClosed:         closingQuantity,
		EntryPrice:             thisEntryPrice,
		CampaignN:              campaign.campaignN,
		DollarsPerPoint:        r.dollarsPerPoint,
		RealisedResult:         thisRealisedResult,
		StoppedAt:              fill.FilledAt,
		RemainingUnits:         remainingAfter,
		AggregateOpenRiskAfter: aggregateOpenRiskAfter,
		Rule:                   event.RuleCampaignUnitsStoppedByStop,
		ADR:                    event.ADRCampaignExitRecordsTheFill,
	}
	if err := unitsStoppedPayload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: stop fill %q would close unit(s) with an invalid units-stopped decision: %w", fill.InstrumentID, fill.FillID, err)
	}
	unitsStoppedBytes, err := json.Marshal(unitsStoppedPayload)
	if err != nil {
		// Unreachable, for the same reason as openCampaign's marshal guards.
		return nil, fmt.Errorf("strategy: marshal campaign units stopped payload: %w", err)
	}

	// If this fill empties the Campaign, build the WHOLE-LIFE Campaign-exited
	// record too — see CampaignExitedPayload's own doc comment,
	// "Accumulating partial stop-outs", for the algebra and why it reduces
	// exactly to the ORIGINAL single-fill formula (entryPrice =
	// campaign.entryPrice(), exitPrice = fill.Price) whenever
	// campaign.closedQuantity is still 0 — every fixture that has never had
	// a partial close, byte for byte.
	var exitedEnvelope *event.Envelope
	if remainingAfter == 0 {
		lifeQuantity, lifeEntryPrice, lifeExitPrice := campaign.lifeAggregate(closingQuantity, thisEntryWeightedSum, fill.Price)
		lifeRealisedResult := float64(lifeQuantity) * (lifeExitPrice - lifeEntryPrice) * r.dollarsPerPoint
		averageMoveInN, err := sizing.AverageMoveInN(lifeExitPrice, lifeEntryPrice, campaign.campaignN)
		if err != nil {
			return nil, fmt.Errorf("strategy: instrument %q: stop fill %q cannot compute the average move in n: %w", fill.InstrumentID, fill.FillID, err)
		}
		realisedResultInUnitN, err := sizing.RealisedResultInUnitN(lifeRealisedResult, campaign.unitQuantity, campaign.campaignN, r.dollarsPerPoint)
		if err != nil {
			return nil, fmt.Errorf("strategy: instrument %q: stop fill %q cannot compute the realised result in unit n: %w", fill.InstrumentID, fill.FillID, err)
		}

		exitedPayload := event.CampaignExitedPayload{
			CampaignID:            campaign.campaignID,
			InstrumentID:          fill.InstrumentID,
			FillID:                fill.FillID,
			ExitedAt:              fill.FilledAt,
			Reason:                event.ExitReasonStop,
			EntryPrice:            lifeEntryPrice,
			ExitPrice:             lifeExitPrice,
			Quantity:              lifeQuantity,
			CampaignN:             campaign.campaignN,
			DollarsPerPoint:       r.dollarsPerPoint,
			UnitQuantity:          campaign.unitQuantity,
			ProtectiveStopLevel:   stopLevel,
			RealisedResult:        lifeRealisedResult,
			AverageMoveInN:        averageMoveInN,
			RealisedResultInUnitN: realisedResultInUnitN,
			Units:                 campaign.unitsOpened,
			Rule:                  event.RuleCampaignExitedByStop,
			ADR:                   event.ADRCampaignExitRecordsTheFill,
		}
		if err := exitedPayload.Validate(); err != nil {
			return nil, fmt.Errorf("strategy: instrument %q: stop fill %q would close campaign %q with an invalid exit: %w", fill.InstrumentID, fill.FillID, campaign.campaignID, err)
		}
		exitedPayloadBytes, err := json.Marshal(exitedPayload)
		if err != nil {
			// Unreachable, for the same reason as openCampaign's marshal guards.
			return nil, fmt.Errorf("strategy: marshal campaign exited payload: %w", err)
		}
		envelope := r.stamp(decisionID("campaign-exited", fill.InstrumentID, fill.FilledAt), event.CampaignExitedEventType, event.CampaignExitedSchemaVersion, fill.FilledAt, input, exitedPayloadBytes)
		exitedEnvelope = &envelope
	}

	// Built and validated here too, BEFORE any state mutation below — this is
	// genuinely reachable, not merely defensive: a stop-superseded expiry's
	// ExpiredAt/EarliestFillAt chronology (see event.ProposalExpiredPayload's
	// own doc comment) depends on the PENDING PROPOSAL's own bar, which this
	// closing fill's own timestamp has no guaranteed relationship to. Only
	// relevant for a PARTIAL close: a full close discards the whole Campaign
	// (and so any pending Add proposal for it) regardless.
	var addExpiryEnvelope *event.Envelope
	if remainingAfter > 0 && state.pendingAddProposal != nil {
		envelope, err := r.expireAddProposalForStop(state, fill, input)
		if err != nil {
			return nil, err
		}
		addExpiryEnvelope = &envelope
	}

	// The state moves only now, after every payload it will be journalled as
	// has been validated — identical discipline to openCampaign's own.
	campaign.closedQuantity += closingQuantity
	campaign.closedEntryWeightedSum += thisEntryWeightedSum
	campaign.closedExitWeightedSum += float64(closingQuantity) * fill.Price
	campaign.lastCloseFillAt = fill.FilledAt
	campaign.removeUnits(closingUnits)
	r.acceptedFills[fill.FillID] = acceptedFillFromPayload(fill)

	emissions := []event.Envelope{r.stamp(
		decisionID(fmt.Sprintf("units-stopped-%s", fill.FillID), fill.InstrumentID, fill.FilledAt),
		event.CampaignUnitsStoppedEventType, event.CampaignUnitsStoppedSchemaVersion, fill.FilledAt, input, unitsStoppedBytes,
	)}

	if exitedEnvelope != nil {
		// The instrument is a Setup again — CONTEXT.md defines a Setup
		// as an Eligible instrument not in a Campaign, and clearing this is
		// the only thing that gate (applyCompletedBar's "no new entry while
		// a Campaign is open") reads. The very next completed bar therefore
		// evaluates this instrument normally and may Signal, with no
		// further change needed anywhere else.
		state.campaign = nil
		// Recorded so checkBarConfirmsCampaignClosing
		// can catch a bar arriving that predates this closing fill — the
		// identical upper-bound check checkBarConfirmsCampaignOpening
		// already applies to an opening fill, mirrored here for a closing
		// one.
		state.lastClosingFillAt = fill.FilledAt
		emissions = append(emissions, *exitedEnvelope)
	} else {
		// A PARTIAL close leaves the Campaign open with whatever
		// Units remain, and no further Add is ever proposed for it again
		// (see evaluateAdd's own doc comment and this function's own,
		// "Per-Unit closing").
		campaign.partiallyStopped = true

		// partiallyStopped stops evaluateAdd from proposing a FURTHER Add, but an Add
		// proposal already outstanding from a bar BEFORE this partial stop
		// is untouched by that flag alone — cancel it here, explicitly,
		// rather than letting it wait to expire with the next bar (ADR
		// 0011's ordinary lifecycle), since a fill for it could otherwise
		// arrive and be accepted before that next bar ever does (see
		// applyAddFill's own new partiallyStopped guard for the second half
		// of this fix). The envelope was already built and validated above,
		// before any state moved; only now, once we know the WHOLE
		// transition validated successfully, is state.pendingAddProposal
		// actually cleared.
		if addExpiryEnvelope != nil {
			state.pendingAddProposal = nil
			emissions = append(emissions, *addExpiryEnvelope)
		}
	}

	return emissions, nil
}

// applyExitFill handles a fill.Kind == event.FillKindExit delivery: the
// other way of closing a Campaign, alongside a stop fill. Unlike a stop fill,
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
// where a stop fill already closed this same Campaign, and this exit fill
// is a second, later closing fill for it. applyFill's own
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
	if fill.Quantity != campaign.filledQuantity() {
		return nil, fmt.Errorf("strategy: instrument %q: exit fill %q executed %d but campaign %q holds %d; a partial exit fill is rejected — every Unit exits together (CONTEXT.md: 'Campaign')",
			fill.InstrumentID, fill.FillID, fill.Quantity, campaign.campaignID, campaign.filledQuantity())
	}
	if fill.FilledAt.Before(campaign.openedAt) {
		return nil, fmt.Errorf("strategy: instrument %q: exit fill %q is timestamped %s, which predates campaign %q's own opening fill at %s; a campaign cannot be closed before it opened",
			fill.InstrumentID, fill.FillID, fill.FilledAt.Format(time.RFC3339), campaign.campaignID, campaign.openedAt.Format(time.RFC3339))
	}
	// The identical check applyStopFill applies to its own kind — an exit fill closing whatever
	// Units survived an earlier partial stop must not claim a timestamp
	// before that earlier closing fill's own (see campaignState.lastCloseFillAt's
	// doc comment).
	if !campaign.lastCloseFillAt.IsZero() && fill.FilledAt.Before(campaign.lastCloseFillAt) {
		return nil, fmt.Errorf("strategy: instrument %q: exit fill %q is timestamped %s, which predates campaign %q's most recently accepted closing fill at %s; a later closing fill cannot have executed before an earlier one",
			fill.InstrumentID, fill.FillID, fill.FilledAt.Format(time.RFC3339), campaign.campaignID, campaign.lastCloseFillAt.Format(time.RFC3339))
	}
	// The lower bound of the exit fill's own execution window (see
	// pendingExitProposalState.earliestFillAt's doc comment and
	// applyFill's own "The window a fill's timestamp must lie in" for the
	// identical reasoning applied to an entry fill). Strict: a fill stamped
	// exactly at the bar before the breach is at the instant the breach bar
	// opened, before which no order for this exit proposal existed. This is
	// independent of, and strictly tighter than, the campaign-opening check
	// above whenever the breach happens on a later bar than the opening —
	// both are kept, since neither implies the other in every fixture (a
	// breach on the SAME bar the campaign opened, were that ever possible,
	// would make them coincide).
	if !fill.FilledAt.After(pending.earliestFillAt) {
		return nil, fmt.Errorf("strategy: instrument %q: exit fill %q is timestamped %s, which predates the bar in which an order for the exit proposal could have executed (that bar opened at %s); a campaign may not be closed by an execution older than the decision that authorised it",
			fill.InstrumentID, fill.FillID, fill.FilledAt.Format(time.RFC3339), pending.earliestFillAt.Format(time.RFC3339))
	}

	// An exit fill always closes every Unit the Campaign STILL holds, but an EARLIER partial
	// stop (the gap case) may already have closed some of them — and the
	// exited record must represent the Campaign's WHOLE life, not merely
	// the Units this fill happens to close itself. thisEntryWeightedSum is
	// therefore computed over campaign.units (the still-open Units, exactly
	// what this fill closes), and combined with whatever campaign.closedQuantity/
	// closedEntryWeightedSum/closedExitWeightedSum an earlier stop already
	// accumulated via the SAME campaignState.lifeAggregate helper
	// applyStopFill's own final-close branch uses — see that method's own
	// doc comment for the algebra, and CampaignExitedPayload's own doc
	// comment, "Accumulating partial stop-outs", for why this reduces
	// EXACTLY to the ORIGINAL single-fill formula (entryPrice =
	// campaign.entryPrice(), exitPrice = fill.Price) whenever no partial
	// stop ever preceded this exit — every such fixture, byte for byte.
	var thisEntryWeightedSum float64
	for _, u := range campaign.units {
		thisEntryWeightedSum += float64(u.quantity) * u.fillPrice
	}
	thisQuantity := campaign.filledQuantity()
	quantity, entryPrice, exitPrice := campaign.lifeAggregate(thisQuantity, thisEntryWeightedSum, fill.Price)
	realisedResult := float64(quantity) * (exitPrice - entryPrice) * r.dollarsPerPoint
	// See applyStopFill's identical
	// comment.
	averageMoveInN, err := sizing.AverageMoveInN(exitPrice, entryPrice, campaign.campaignN)
	if err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: exit fill %q cannot compute the average move in n: %w", fill.InstrumentID, fill.FillID, err)
	}
	realisedResultInUnitN, err := sizing.RealisedResultInUnitN(realisedResult, campaign.unitQuantity, campaign.campaignN, r.dollarsPerPoint)
	if err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: exit fill %q cannot compute the realised result in unit n: %w", fill.InstrumentID, fill.FillID, err)
	}

	exitedPayload := event.CampaignExitedPayload{
		CampaignID:            campaign.campaignID,
		InstrumentID:          fill.InstrumentID,
		FillID:                fill.FillID,
		ExitedAt:              fill.FilledAt,
		Reason:                event.ExitReasonExitChannel,
		EntryPrice:            entryPrice,
		ExitPrice:             exitPrice,
		Quantity:              quantity,
		CampaignN:             campaign.campaignN,
		DollarsPerPoint:       r.dollarsPerPoint,
		UnitQuantity:          campaign.unitQuantity,
		ProtectiveStopLevel:   campaign.protectiveStop(),
		RealisedResult:        realisedResult,
		AverageMoveInN:        averageMoveInN,
		RealisedResultInUnitN: realisedResultInUnitN,
		Units:                 campaign.unitsOpened,
		Rule:                  event.RuleCampaignExitedByExitChannel,
		ADR:                   event.ADRCampaignExitRecordsTheFill,
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
	// Identical to applyStopFill's own recording, so
	// checkBarConfirmsCampaignClosing catches a bar arriving that predates
	// THIS closing fill regardless of which kind closed the campaign.
	state.lastClosingFillAt = fill.FilledAt

	return []event.Envelope{exitEnvelope}, nil
}

// applyAddFill handles a fill.Kind == event.FillKindAdd delivery: the way
// of bringing a further Unit into an open Campaign. Like an exit fill, an
// Add fill always executes a specific outstanding Add proposal
// (evaluateAdd's AddProposalPayload) — ADR 0005 makes it a resting order, so
// it is always proposed before it can be filled — and fill.Validate() has
// already required both CampaignID and ProposalID to be present for this
// Kind.
//
// **No decision about WHETHER the rung was reached is made here**, mirroring
// applyStopFill's and applyExitFill's own doc comments: this function only
// ever reacts to a fill event that already says the Add executed.
//
// A partial Add is accepted for the quantity that actually filled — at most
// the proposal's Quantity (the Campaign's frozen unitQuantity) — mirroring
// the same rule for the opening fill; a second partial fill for the SAME
// Add proposal is deferred to a future change, the identical limitation
// applyFill already enforces for a second partial entry.
//
// Reaching this function with state.campaign == nil, or with the Campaign
// already at its frozen maxUnits, means there is nothing valid for this fill
// to have executed: applyFill's own idempotency check has already resolved
// any re-delivery of a fill this reducer previously accepted before dispatch
// reaches here (see acceptedFillState's doc comment), so what remains is
// genuinely unmatched — an Add fill arriving anyway fails closed for a
// Campaign that is never proposed a fifth Unit.
func (r *Reducer) applyAddFill(state *instrumentState, fill event.FillPayload, input event.Envelope) ([]event.Envelope, error) {
	campaign := state.campaign
	if campaign == nil {
		return nil, fmt.Errorf("strategy: instrument %q: add fill %q names campaign %q, but there is no open campaign for it; a fill for an unknown or already-closed campaign is a reconciliation failure (docs/architecture.md)",
			fill.InstrumentID, fill.FillID, fill.CampaignID)
	}
	if campaign.campaignID != fill.CampaignID {
		return nil, fmt.Errorf("strategy: instrument %q: add fill %q names campaign %q, but the open campaign is %q; a fill for a campaign this strategy does not hold is a reconciliation failure (docs/architecture.md)",
			fill.InstrumentID, fill.FillID, fill.CampaignID, campaign.campaignID)
	}
	// Belt and braces
	// alongside expireAddProposalForStop, which already clears
	// state.pendingAddProposal the instant a stop fill partially closes
	// this Campaign (so the "no outstanding add proposal" check below would
	// already catch a stale fill on its own) — checked explicitly, and
	// first, so the error names the actual reason rather than a generic
	// "no proposal" one, matching the maxUnits check's own redundancy just
	// below.
	if campaign.partiallyStopped {
		return nil, fmt.Errorf("strategy: instrument %q: add fill %q names campaign %q, which has already been partially stopped; no further unit is ever accepted once a campaign has started coming off (The Turtle Rules p.23-24's Whipsaw variant is out of scope for the Baseline, ADR 0012)",
			fill.InstrumentID, fill.FillID, campaign.campaignID)
	}
	if len(campaign.units) >= campaign.maxUnits {
		return nil, fmt.Errorf("strategy: instrument %q: add fill %q would bring campaign %q to more than its configured maximum of %d units; a fifth unit (or beyond the configured maximum) is never proposed and never accepted (ADR 0008)",
			fill.InstrumentID, fill.FillID, campaign.campaignID, campaign.maxUnits)
	}
	pending := state.pendingAddProposal
	if pending == nil {
		return nil, fmt.Errorf("strategy: instrument %q: add fill %q names proposal %q, but there is no outstanding add proposal for campaign %q; an add proposal expires with its bar (ADR 0011, see the strategy.proposal.expired event in the journal) and a fill for a proposal this strategy is no longer offering is a reconciliation failure (docs/architecture.md)",
			fill.InstrumentID, fill.FillID, fill.ProposalID, campaign.campaignID)
	}
	if pending.proposalID != fill.ProposalID {
		return nil, fmt.Errorf("strategy: instrument %q: add fill %q names proposal %q, but the outstanding add proposal is %q; a fill for a proposal this strategy never made is a reconciliation failure (docs/architecture.md)",
			fill.InstrumentID, fill.FillID, fill.ProposalID, pending.proposalID)
	}
	if fill.Direction != campaign.direction {
		return nil, fmt.Errorf("strategy: instrument %q: add fill %q is %s but campaign %q is %s; the add must be in the campaign's own direction (FillPayload.Direction is the position's direction, not the order's buy/sell side)",
			fill.InstrumentID, fill.FillID, fill.Direction, campaign.campaignID, campaign.direction)
	}
	if fill.Quantity > pending.quantity {
		return nil, fmt.Errorf("strategy: instrument %q: add fill %q executed %d against proposal %q, which sized %d; an over-execution risks more than the unit that was sized (ADR 0003) and is never absorbed",
			fill.InstrumentID, fill.FillID, fill.Quantity, fill.ProposalID, pending.quantity)
	}
	// The lower bound of the add fill's own execution window — the same
	// window check applyFill applies to an entry fill and applyExitFill
	// applies to an exit fill, bound here against the bar that raised THIS
	// add proposal (see pendingAddProposalState.earliestFillAt).
	if !fill.FilledAt.After(pending.earliestFillAt) {
		return nil, fmt.Errorf("strategy: instrument %q: add fill %q is timestamped %s, which predates the bar in which an order for the add proposal could have executed (that bar opened at %s); a unit may not be added by an execution older than the decision that authorised it",
			fill.InstrumentID, fill.FillID, fill.FilledAt.Format(time.RFC3339), pending.earliestFillAt.Format(time.RFC3339))
	}
	// A same-bar Add
	// proposal (evaluateAdd's own chain, see applyAddFill's package doc
	// comment) is raised only AFTER the preceding Unit's fill was accepted,
	// so a later Unit's fill claiming a timestamp EARLIER than the Unit
	// immediately before it records causally impossible ordering — the
	// execution history would show Unit 3 filling before Unit 2 did. Equal
	// timestamps are allowed: a gap that opens above every remaining rung
	// fills every Unit at the same instant (The Turtle Rules p.19: "all
	// four could be added in one day"). campaign.lastUnit() is exactly the
	// Unit immediately before this one in the chain — Unit 1 itself when
	// this is the second Unit, which is why this check also supersedes (and
	// replaces) a standalone "before the campaign's own opening fill" check:
	// lastUnit().filledAt equals campaign.openedAt in exactly that case.
	if last := campaign.lastUnit(); fill.FilledAt.Before(last.filledAt) {
		return nil, fmt.Errorf("strategy: instrument %q: add fill %q is timestamped %s, which is before unit %d's own fill at %s; a later unit cannot have executed before the unit immediately before it in the campaign",
			fill.InstrumentID, fill.FillID, fill.FilledAt.Format(time.RFC3339), last.index, last.filledAt.Format(time.RFC3339))
	}

	// The Turtle Rules p.22's 2N stop, measured from THIS Unit's own actual
	// fill and the campaign's frozen N (ADR 0006) — identical arithmetic to
	// openCampaign's own, applied to a later Unit.
	protectiveStop, err := sizing.ProtectiveStopLevel(fill.Price, campaign.campaignN, campaign.stopMultiple, sizing.DirectionLong)
	if err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: add fill %q cannot compute a protective stop: %w", fill.InstrumentID, fill.FillID, err)
	}

	unitIndex := pending.unitIndex
	unitsAfter := len(campaign.units) + 1

	unitAddedPayload := event.CampaignUnitAddedPayload{
		CampaignID:     campaign.campaignID,
		InstrumentID:   fill.InstrumentID,
		UnitIndex:      unitIndex,
		FillID:         fill.FillID,
		FillPrice:      fill.Price,
		Quantity:       fill.Quantity,
		CampaignN:      campaign.campaignN,
		StopMultiple:   campaign.stopMultiple,
		ProtectiveStop: protectiveStop,
		Units:          unitsAfter,
		AddedAt:        fill.FilledAt,
		Rule:           event.RuleAddLadderHalfN,
		ADR:            event.ADRCampaignFrozenAtEntry,
	}
	if err := unitAddedPayload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: add fill %q would add an invalid unit: %w", fill.InstrumentID, fill.FillID, err)
	}
	unitAddedBytes, err := json.Marshal(unitAddedPayload)
	if err != nil {
		// Unreachable, for the same reason as openCampaign's marshal guards.
		return nil, fmt.Errorf("strategy: marshal campaign unit added payload: %w", err)
	}

	// A fresh Protective-Stop-set decision for THIS Unit alone,
	// Reason ProtectiveStopReasonInitial. PreviousLevel is 0: this is a
	// brand-new stop for a Unit that never had one before, not a raise of
	// an earlier Unit's stop (the Stop Ladder, built below).
	newUnitStopSetPayload := event.ProtectiveStopSetPayload{
		CampaignID:    campaign.campaignID,
		InstrumentID:  fill.InstrumentID,
		UnitIndex:     unitIndex,
		Reason:        event.ProtectiveStopReasonInitial,
		AsOf:          fill.FilledAt,
		Level:         protectiveStop,
		PreviousLevel: 0,
		EntryPrice:    fill.Price,
		CampaignN:     campaign.campaignN,
		StopMultiple:  campaign.stopMultiple,
		Rule:          event.RuleProtectiveStopSetFromFill,
		ADR:           event.ADRCampaignFrozenAtEntry,
	}
	if err := newUnitStopSetPayload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: add fill %q would set an invalid protective stop: %w", fill.InstrumentID, fill.FillID, err)
	}
	newUnitStopSetBytes, err := json.Marshal(newUnitStopSetPayload)
	if err != nil {
		// Unreachable, for the same reason as openCampaign's marshal guards.
		return nil, fmt.Errorf("strategy: marshal protective stop set payload: %w", err)
	}

	// The Stop Ladder (The Turtle Rules p.22-23): "if additional units
	// were added, the stops for earlier units were raised by 1/2 N." Every
	// EARLIER Unit's own stop — never the newly-added Unit's own, set
	// above from ITS OWN fill — rises by exactly RaisedStop(previous,
	// campaignN), regardless of how far the new Unit's own fill landed
	// from its rung: this is what keeps the earlier Units at the standard
	// raise even in the gap case (a later Unit filling well past its rung),
	// implementing the source's literal first sentence rather than the
	// paraphrase "set every stop to 2N below the newest fill" — see
	// sizing.RaisedStop's own doc comment for why the two readings diverge
	// there. Every raised payload is built and validated here, alongside
	// the two above, before ANY of them mutate campaign state (openCampaign's
	// own discipline: a Campaign, or a Unit's stop, that could not be
	// recorded must not exist in memory either).
	type raise struct {
		unitIndex int
		newStop   float64
		payload   event.ProtectiveStopSetPayload
		bytes     []byte
	}
	raises := make([]raise, 0, len(campaign.units))
	for _, earlier := range campaign.units {
		newStop, err := sizing.RaisedStop(earlier.protectiveStop, campaign.campaignN)
		if err != nil {
			return nil, fmt.Errorf("strategy: instrument %q: add fill %q cannot raise unit %d's protective stop: %w", fill.InstrumentID, fill.FillID, earlier.index, err)
		}
		raisedPayload := event.ProtectiveStopSetPayload{
			CampaignID:    campaign.campaignID,
			InstrumentID:  fill.InstrumentID,
			UnitIndex:     earlier.index,
			Reason:        event.ProtectiveStopReasonAddLadder,
			AsOf:          fill.FilledAt,
			Level:         newStop,
			PreviousLevel: earlier.protectiveStop,
			EntryPrice:    earlier.fillPrice,
			CampaignN:     campaign.campaignN,
			StopMultiple:  campaign.stopMultiple,
			Rule:          event.RuleStopLadderRaisedByHalfN,
			ADR:           event.ADRCampaignFrozenAtEntry,
		}
		if err := raisedPayload.Validate(); err != nil {
			return nil, fmt.Errorf("strategy: instrument %q: add fill %q would raise unit %d to an invalid protective stop: %w", fill.InstrumentID, fill.FillID, earlier.index, err)
		}
		raisedBytes, err := json.Marshal(raisedPayload)
		if err != nil {
			// Unreachable, for the same reason as openCampaign's marshal guards.
			return nil, fmt.Errorf("strategy: marshal protective stop set payload: %w", err)
		}
		raises = append(raises, raise{unitIndex: earlier.index, newStop: newStop, payload: raisedPayload, bytes: raisedBytes})
	}

	// The state moves only now, after every payload it will be journalled as
	// has been validated — identical discipline to openCampaign's own.
	// Earlier Units' stops are raised in place, in ascending index order,
	// before the new Unit is appended, so the same-bar chain re-evaluation
	// below reads the fully-updated Campaign.
	for i := range campaign.units {
		campaign.units[i].protectiveStop = raises[i].newStop
	}
	campaign.units = append(campaign.units, unitState{
		index:          unitIndex,
		openingFillID:  fill.FillID,
		fillPrice:      fill.Price,
		quantity:       fill.Quantity,
		protectiveStop: protectiveStop,
		filledAt:       fill.FilledAt,
	})
	campaign.unitsOpened++
	state.pendingAddProposal = nil
	r.acceptedFills[fill.FillID] = acceptedFillFromPayload(fill)

	emissions := []event.Envelope{
		r.stamp(decisionID(fmt.Sprintf("unit-added-%d", unitIndex), fill.InstrumentID, fill.FilledAt), event.CampaignUnitAddedEventType, event.CampaignUnitAddedSchemaVersion, fill.FilledAt, input, unitAddedBytes),
		r.stamp(decisionID(fmt.Sprintf("protective-stop-set-unit-%d", unitIndex), fill.InstrumentID, fill.FilledAt), event.ProtectiveStopSetEventType, event.ProtectiveStopSetSchemaVersion, fill.FilledAt, input, newUnitStopSetBytes),
	}
	for _, r2 := range raises {
		emissions = append(emissions, r.stamp(
			decisionID(fmt.Sprintf("protective-stop-raised-unit-%d-for-add-%d", r2.unitIndex, unitIndex), fill.InstrumentID, fill.FilledAt),
			event.ProtectiveStopSetEventType, event.ProtectiveStopSetSchemaVersion, fill.FilledAt, input, r2.bytes,
		))
	}

	// The same-bar Add chain: re-evaluate immediately for the NEXT rung,
	// still measured against the bar that produced the opportunity for this
	// whole chain (state.lastBarHigh/lastBarPeriodEnd/lastBarEarliestFillAt —
	// see evaluateAdd's own doc comment). This is what lets all four Units
	// be added within a single bar, each rung faithfully measured from the
	// fill immediately before it.
	chainEmissions, err := r.evaluateAdd(state, input)
	if err != nil {
		return nil, err
	}
	emissions = append(emissions, chainEmissions...)

	return emissions, nil
}

// checkCampaignHasAProtectiveStop enforces, at the start of every completed
// bar, the capital-safety invariant: every open Campaign has a
// Protective Stop, positive, at all times (CONTEXT.md: "Protective Stop" —
// "Every open Campaign has one at all times").
//
// The check applies to every held Unit individually — each Unit's
// OWN protectiveStop must be positive — rather than a single Campaign-level
// figure, since a multi-Unit Campaign has no single "the entry price" any
// invariant could compare against. A single-Unit Campaign reduces to
// exactly the original one-figure check.
//
// This check does NOT require a Unit's stop to sit strictly below its own
// fill price: repeated half-N raises (the Stop Ladder) can legitimately
// lift an earlier Unit's stop to or above its own entry under a Variant
// with a narrow enough Stop Multiple (sizing.AggregateOpenRisk's own doc
// comment works the arithmetic — the Baseline's own 2N stop and four-Unit
// maximum never reach it, since the maximum raise is 1.5N). A stop at or
// above entry is a risk-free position (CONTEXT.md), not a corrupted one, so
// this invariant now checks only what a raise can never make false:
// positive and finite.
//
// This is deliberately a *runtime* check on top of a representation that
// already makes the violation unreachable in practice: openCampaign and
// applyAddFill are the only places a unitState is ever constructed, and
// neither runs without a protectiveStop that has already passed
// sizing.ProtectiveStopLevel's and its payload's own Validate checks —
// there is no "open-without-stop" zero value of unitState that would
// satisfy the type system. So reaching a violation here can only mean
// memory was corrupted after the fact (a defect in this process, not a bad
// input event), which is why the response is a HALT rather than an error
// naming "the fill" or "the bar": there is no upstream input event to
// blame, and continuing to trade an instrument whose stop this reducer can
// no longer vouch for is exactly the state docs/architecture.md's safety
// invariants exist to prevent ("material reconciliation differences force
// safe mode").
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
	var broken *unitState
	for i := range campaign.units {
		u := &campaign.units[i]
		if !(u.protectiveStop > 0) {
			broken = u
			break
		}
	}
	if broken == nil {
		return event.Envelope{}, nil
	}

	detail := fmt.Sprintf(
		"campaign %q for instrument %q: unit %d has protective stop %v (fill price %v): every unit of an open campaign must have a protective stop, positive and finite, at all times",
		campaign.campaignID, bar.InstrumentID, broken.index, broken.protectiveStop, broken.fillPrice)

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
