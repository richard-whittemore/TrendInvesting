package fills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// Result is what one turn of the backtest loop produced.
//
// Inputs is the input stream the loop actually applied, in order: the bar
// itself plus every fill the simulator interleaved around it, each stamped
// with its own contiguous sequence. It is what the backtest loop journals,
// and replaying it through replay.Engine.Run with a fresh reducer reproduces
// Decisions exactly
// — which is the property that makes the journal evidence rather than a
// summary.
//
// Decisions is every envelope the reducer emitted, in emission order, exactly
// as it returned them. Sequence, CausationID and CorrelationID are NOT set:
// replay.Engine owns those, and a handler may not claim causation it did not
// have (docs/architecture.md). A caller that wants the stamped output stream
// replays Inputs.
type Result struct {
	Inputs    []event.Envelope
	Decisions []event.Envelope
}

func (r *Result) add(input event.Envelope, decisions []event.Envelope) {
	r.Inputs = append(r.Inputs, input)
	r.Decisions = append(r.Decisions, decisions...)
}

// Deliver applies one input envelope that is not a completed bar — a
// configuration event, an account snapshot, a cash movement — to handler,
// numbering it into the composed input stream and folding whatever the
// handler emits into the simulator's resting-order book.
//
// It exists so that the backtest driver has exactly one way to number the
// stream: a configuration event applied around RunBar rather than through it
// would leave a gap replay.Engine.Run refuses.
func Deliver(ctx context.Context, sim *Simulator, handler replay.Handler, envelope event.Envelope) (Result, error) {
	if sim == nil || handler == nil {
		return Result{}, fmt.Errorf("fills: a simulator and a handler are both required")
	}
	if envelope.Type == event.CompletedBarEventType || envelope.Type == event.SessionClosedEventType {
		return Result{}, fmt.Errorf("fills: a %s envelope belongs to RunSession, which applies the per-bar protocol to it", envelope.Type)
	}
	var result Result
	if err := sim.deliver(ctx, handler, envelope, reference{}, &result); err != nil {
		return result, err
	}
	return result, nil
}

// RunBar is the per-bar protocol: ADR 0005's fill model and ADR 0010's
// ordering, applied to one completed bar of one instrument, as a Session of
// its own (see RunSession, which the backtest loop calls; the protocol is
// tested here once rather than re-derived there).
//
// # The protocol
//
// For each completed bar B of an instrument, in order:
//
//  1. **The open-instant pass.** Every order already resting before B whose
//     level lay beyond B's open executed AT the open — the first instant of
//     the bar — so those fills are delivered to the reducer BEFORE B is. A
//     Campaign that gapped through its stop was already closed for the whole
//     of that session, and journalling a Campaign-evaluated event for it, or
//     adding a Unit to it later in the bar, would record something that
//     cannot have happened.
//  2. **The bar, then its Session's close.** B is delivered to the reducer.
//     It expires yesterday's proposals (ADR 0011) and evaluates an open
//     Campaign — the stop and Exit Channel levels in force, and on a breach
//     the exit proposal — or, with no Campaign open, evaluates the Setup and
//     signals a breakout. Then market.session.closed ends B's Session, and
//     the reducer decides the Add, or sizes the Signal into a trade proposal
//     (ADR 0021).
//  3. **The intrabar fixpoint.** Every order now resting is evaluated against
//     B, repeatedly, until nothing more fills: every covered BUY first, then,
//     when no buy is covered, the next covered SELL — a Unit's Exit Order at
//     its own stop, worst price first, and once none is left, the Exit
//     Orders at the Exit Channel as one exit. Each fill
//     is delivered to the reducer as it is decided, and whatever that
//     delivery causes — a Unit, its own Protective Stop, the next rung, a
//     cancelled Add, a closed Campaign — is folded back into the book before
//     the next pass. This is what lets all four Units be added inside one bar
//     (The Turtle Rules p.19), each rung measured from the fill before it.
//  4. Anything still unfilled keeps resting. The reducer expires it at B+1.
//
// # Rule 3: why buys before sells, and the cases enumerated
//
// ADR 0005 rule 3 says same-bar ambiguity resolves pessimistically, and names
// one case: a bar covering both an entry and its Protective Stop entered and
// THEN was stopped. Generalised, the rule is "emit fills in the order that is
// worst for the trader among the orderings the bar's range permits", and for
// a long-only Baseline that is: buys first, then sells.
//
// Every sell is a Unit's one Exit Order (CONTEXT.md: "Exit Order"), resting
// at the higher of its Protective Stop and any proposed Exit-Channel exit's
// level (ADR 0005, as amended). A stop and an exit therefore never rest as
// two orders for the same shares, and no two sells compete: each fills at
// its own level on ADR 0005's own terms whichever is delivered first.
//
// ADR 0010's "exits before Adds before entries" is not in tension with this.
// That ordering governs DECISIONS within a day — which is why step 2 above
// delivers the bar to the reducer, whose evaluateCampaign runs before
// evaluateAdd, unchanged. Step 3 orders EXECUTIONS within a bar, which is a
// different question that daily bars cannot answer, and pessimism is the
// declared tie-break.
//
// The cases, enumerated over what can rest simultaneously for one instrument:
//
//	entry (buy) + its own new stop (sell)
//	    Entered, then stopped: a realised loss. The favourable ordering would
//	    have the stop taken out first, so the entry never happens and the loss
//	    never appears. ADR 0005 rule 3's own named case.
//	Add (buy) + existing Exit Orders (sell)
//	    Unit added, then the whole Campaign stopped: a bigger position taken
//	    off at the stop. Stopping first would cancel the Add and lose less.
//	Add (buy) + Exit Orders at an Exit-Channel exit (sell)
//	    Cannot arise from one bar — the reducer only evaluates an Add when
//	    that same bar did not propose an exit (ADR 0010) — but the ordering
//	    holds if it ever does: add, then exit everything.
//	entry (buy) + Exit Order of an EARLIER Campaign (sell)
//	    Cannot arise: an entry is proposed only when no Campaign is open.
//	Exit Orders at different Units' own stops (sell + sell)
//	    The gap case (The Turtle Rules p.22-23). Each Unit's order fills on
//	    its own terms, one fill per Unit, delivered worst price first.
//	Exit Orders at a stop + Exit Orders at the Exit Channel (sell + sell)
//	    The exit level lies between Units' stops: the Units whose stop is
//	    higher rest there, the rest at the exit level. Any bar reaching the
//	    exit level has reached those higher stops too, so every Unit sells at
//	    its own order's level: the stops first, as stop fills, then one exit
//	    fill for whatever the Campaign still holds — the only exit fill the
//	    reducer accepts (every Unit exits together; CONTEXT.md: "Campaign").
//
// The one thing that overrides the ordering is knowledge. An order that
// gapped through its level executed at the open, and the open precedes
// everything else in the bar — that is not an ambiguity to resolve
// pessimistically but a fact the bar states, which is why step 1 exists and
// why an order created by a fill inside the bar is referenced to that fill's
// price rather than to the open (see Range.Reference).
//
// # Timestamps
//
// Every fill is stamped at the bar's own period end. The reducer polices a
// fill's timestamp on both sides — strictly after the previous bar's period
// end, not after the next bar's, and never regressing across a Campaign's
// closing fills — and the bar's period end satisfies all of that with the
// equalities the reducer explicitly allows (several Units executing "in one
// day", The Turtle Rules p.19). The ORDER of fills within a bar is carried by
// the order they are delivered in, not by their timestamps, so no artificial
// intrabar instants are invented: a daily bar cannot say what time of day
// anything happened, and a timestamp that implied otherwise would be a claim
// the data does not support.
func RunBar(ctx context.Context, sim *Simulator, handler replay.Handler, barEnvelope event.Envelope) (Result, error) {
	return RunSession(ctx, sim, handler, []event.Envelope{barEnvelope})
}

// RunSession is RunBar's protocol applied to a whole Session (CONTEXT.md:
// "Session"): every completed bar sharing one period end, across the
// universe. ADR 0021 moves a day's Adds and entries from each bar to the
// Session's close, so the steps interleave around it:
//
//  1. For each bar, in the order given: its open-instant pass, then the bar
//     itself. Each bar's exits are decided here.
//  2. market.session.closed, naming every bar of the Session. The Session's
//     Adds and entries are decided here, across instruments.
//  3. For each instrument, in ascending instrument order: its intrabar
//     fixpoint. The Adds and entries just proposed still rest and fill
//     inside their own bar (ADR 0005), after they were proposed.
//
// Step 3's order never depends on the order the bars were given in, so
// neither do the Session's fills. Fills never cross instruments, so the order
// matters only to where each lands in the journal.
//
// The close is built here from the bars themselves, so it always names
// exactly what was delivered, with their provenance: the first bar's source,
// strategy version, configuration hash and recorded-at instant.
func RunSession(ctx context.Context, sim *Simulator, handler replay.Handler, barEnvelopes []event.Envelope) (Result, error) {
	var result Result
	if sim == nil || handler == nil {
		return result, fmt.Errorf("fills: a simulator and a handler are both required")
	}
	session, err := sessionBars(barEnvelopes)
	if err != nil {
		return result, err
	}

	for _, b := range session {
		// Every fill this call produces is a fact about ITS bar, whichever
		// side of the bar it is delivered on, so the bar's provenance is
		// fixed before a single order is priced — not inferred from whatever
		// was delivered last. The open-instant pass runs BEFORE the bar
		// itself reaches the reducer, so a stamp taken from delivery order
		// would date a gap fill, and every decision it causes, to the
		// PREVIOUS bar: an execution recorded before the session that
		// produced it.
		sim.recordedAt = b.envelope.RecordedAt

		// Step 1: the open-instant pass. ADR 0004: every level this
		// simulator fills against was computed by the reducer on the
		// split-adjusted view, so the fill must be decided on it too.
		if err := sim.fillPass(ctx, handler, b.bar, b.bar.SplitAdjusted, true, &b.fills, &result); err != nil {
			return result, err
		}
		// Step 2: the bar itself.
		if err := sim.deliver(ctx, handler, b.envelope, reference{}, &result); err != nil {
			return result, err
		}
	}

	closed, err := sessionClosedFor(session)
	if err != nil {
		return result, err
	}
	if err := sim.deliver(ctx, handler, closed, reference{}, &result); err != nil {
		return result, err
	}

	// Step 3: the intrabar fixpoint, per instrument in ascending order.
	byInstrument := slices.Clone(session)
	sort.Slice(byInstrument, func(a, b int) bool { return byInstrument[a].bar.InstrumentID < byInstrument[b].bar.InstrumentID })
	for _, b := range byInstrument {
		sim.recordedAt = b.envelope.RecordedAt
		if err := sim.fillPass(ctx, handler, b.bar, b.bar.SplitAdjusted, false, &b.fills, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

// sessionBar is one bar of a Session: its envelope, its decoded payload, and
// how many fills it has produced so far (maxFillsPerBar bounds it).
type sessionBar struct {
	envelope event.Envelope
	bar      event.CompletedBarPayload
	fills    int
}

// sessionBars decodes and validates a Session's bars: at least one, every
// one a valid completed bar, all sharing one period end, no instrument
// twice.
func sessionBars(envelopes []event.Envelope) ([]*sessionBar, error) {
	if len(envelopes) == 0 {
		return nil, errors.New("fills: a Session holds at least one bar")
	}
	session := make([]*sessionBar, 0, len(envelopes))
	seen := make(map[string]bool, len(envelopes))
	for _, envelope := range envelopes {
		if envelope.Type != event.CompletedBarEventType {
			return nil, fmt.Errorf("fills: a Session requires %s envelopes, got %q", event.CompletedBarEventType, envelope.Type)
		}
		b := &sessionBar{envelope: envelope}
		if err := json.Unmarshal(envelope.Payload, &b.bar); err != nil {
			return nil, fmt.Errorf("fills: decode completed bar payload: %w", err)
		}
		if err := b.bar.Validate(); err != nil {
			return nil, fmt.Errorf("fills: %w", err)
		}
		if len(session) > 0 && !b.bar.PeriodEnd.Equal(session[0].bar.PeriodEnd) {
			return nil, fmt.Errorf("fills: a Session's bars share one period end; %s's %s is not %s",
				b.bar.InstrumentID, b.bar.PeriodEnd.Format(time.RFC3339), session[0].bar.PeriodEnd.Format(time.RFC3339))
		}
		if seen[b.bar.InstrumentID] {
			return nil, fmt.Errorf("fills: instrument %q appears twice in one Session", b.bar.InstrumentID)
		}
		seen[b.bar.InstrumentID] = true
		session = append(session, b)
	}
	return session, nil
}

// sessionClosedFor builds the market.session.closed that ends session. Its
// provenance never depends on the order the bars arrived in (ADR 0021:
// order-independence), because the reducer stamps it onto every proposal
// the close makes. The close is known only once every bar has been
// recorded, so RecordedAt is the latest bar's. The other provenance is the
// lowest instrument ID's bar's.
func sessionClosedFor(session []*sessionBar) (event.Envelope, error) {
	ids := make([]string, 0, len(session))
	first, lowest := session[0].envelope, session[0].bar.InstrumentID
	recordedAt := first.RecordedAt
	for _, b := range session {
		ids = append(ids, b.bar.InstrumentID)
		if b.bar.InstrumentID < lowest {
			first, lowest = b.envelope, b.bar.InstrumentID
		}
		if b.envelope.RecordedAt.After(recordedAt) {
			recordedAt = b.envelope.RecordedAt
		}
	}
	sort.Strings(ids)
	periodEnd := session[0].bar.PeriodEnd
	payload := event.SessionClosedPayload{PeriodEnd: periodEnd, InstrumentIDs: ids}
	if err := payload.Validate(); err != nil {
		return event.Envelope{}, fmt.Errorf("fills: built an invalid session closed payload: %w", err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// validated-payload-json (docs/development.md).
		return event.Envelope{}, fmt.Errorf("fills: marshal session closed payload: %w", err)
	}
	return event.Envelope{
		ID:                "session-closed:" + periodEnd.UTC().Format(idTimeLayout),
		Type:              event.SessionClosedEventType,
		SchemaVersion:     event.SessionClosedSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         periodEnd,
		RecordedAt:        recordedAt,
		Source:            first.Source,
		StrategyVersion:   first.StrategyVersion,
		ConfigurationHash: first.ConfigurationHash,
		PayloadHash:       event.HashPayload(encoded),
		Payload:           encoded,
	}, nil
}

// fillPass repeatedly fills the first candidate the bar covers, delivering
// each fill and re-evaluating afterwards, until nothing more fills.
//
// onlyGapped restricts it to orders that executed at the reference price —
// step 1's open-instant pass. Re-evaluating after every single fill, rather
// than filling a batch, is what makes the Add chain work: the next rung and
// the new Unit's own stop do not exist until the fill before them has been
// accepted by the reducer.
func (s *Simulator) fillPass(ctx context.Context, handler replay.Handler, bar event.CompletedBarPayload, view event.PriceView, onlyGapped bool, fillsThisBar *int, result *Result) error {
	for {
		candidates, err := s.covered(bar.InstrumentID, bar.PeriodEnd, view)
		if err != nil {
			return err
		}
		next, ok := pick(candidates, onlyGapped)
		if !ok {
			return nil
		}
		*fillsThisBar++
		if *fillsThisBar > maxFillsPerBar {
			return fmt.Errorf("fills: instrument %q: bar ending %s produced more than %d fills; the reducer's own maximum units (ADR 0008) bounds this, so exceeding it means an order was refilled rather than resolved",
				bar.InstrumentID, bar.PeriodEnd.Format(time.RFC3339), maxFillsPerBar)
		}
		envelope, err := s.fillEnvelopeFor(bar, next, *fillsThisBar)
		if err != nil {
			return err
		}
		if err := s.deliver(ctx, handler, envelope, reference{bar: bar.PeriodEnd, price: next.price}, result); err != nil {
			return err
		}
	}
}

// pick takes the first candidate covered returned, honouring the gapped
// filter. covered has already ordered them: buys first, then sells worst
// price first.
func pick(candidates []candidate, onlyGapped bool) (candidate, bool) {
	for _, c := range candidates {
		if onlyGapped && !c.atReference {
			continue
		}
		return c, true
	}
	return candidate{}, false
}

// fillEnvelopeFor builds one execution.fill envelope.
//
// Its ID is deterministic and reproducible on replay — no randomness, no wall
// clock (.golangci.yml forbids both in internal/) — and is also the
// FillPayload's own FillID, so a journal reader joining an envelope to the
// Unit it opened has one identifier to follow rather than two. One instrument
// has exactly one bar per period end and the fills within a bar are numbered
// in the order they were decided, so the triple is unique for the whole run.
//
// EventTime is the bar's period end and RecordedAt is that same bar
// envelope's own recording moment — both of them the bar this fill was
// decided from, never whatever the simulator delivered last. The simulator
// observes the same recording moment as the data that produced it and has no
// clock of its own to offer (.golangci.yml forbids one in internal/), and a
// fill produced by the open-instant pass is a fact about the bar whose open
// produced it even though it is delivered before that bar. See RunBar, where
// the provenance is fixed for the whole call.
//
// Note that RecordedAt and FilledAt answer different questions and are not
// interchangeable: RecordedAt is when the system learned of the bar, FilledAt
// is when the execution happened. Both being the bar's own is a property of
// backtest fixtures, not a rule.
func (s *Simulator) fillEnvelopeFor(bar event.CompletedBarPayload, c candidate, n int) (event.Envelope, error) {
	commission, err := s.chargeFor(c)
	if err != nil {
		return event.Envelope{}, err
	}
	id := fmt.Sprintf("fill:%s:%s:%d", bar.InstrumentID, bar.PeriodEnd.UTC().Format(idTimeLayout), n)
	payload := event.FillPayload{
		InstrumentID: bar.InstrumentID,
		Kind:         c.kind,
		ProposalID:   c.proposalID,
		CampaignID:   c.campaignID,
		FillID:       id,
		UnitIDs:      c.unitIDs,
		// The POSITION's direction, held constant across every fill of a
		// Campaign's life, not the order's buy/sell side — see
		// event.FillPayload.Direction. The Baseline is long-only (ADR 0002).
		Direction:       event.DirectionLong,
		Quantity:        c.quantity,
		Price:           c.price,
		FilledAt:        bar.PeriodEnd,
		Level:           c.level,
		SlippageApplied: s.slippageN * c.n,
		Commission:      commission,
	}
	if err := payload.Validate(); err != nil {
		return event.Envelope{}, fmt.Errorf("fills: built an invalid fill payload: %w", err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// validated-payload-json (docs/development.md): Validate above
		// accepted every field; event.TestValidatedPayloadsMarshal pins it.
		return event.Envelope{}, fmt.Errorf("fills: marshal fill payload: %w", err)
	}
	return event.Envelope{
		ID:                id,
		Type:              event.FillEventType,
		SchemaVersion:     event.FillSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         bar.PeriodEnd,
		RecordedAt:        s.recordedAt,
		Source:            Source,
		StrategyVersion:   s.strategyVersion,
		ConfigurationHash: s.configurationHash,
		PayloadHash:       event.HashPayload(encoded),
		Payload:           encoded,
	}, nil
}

// deliver numbers one input envelope into the composed stream, validates it,
// applies it to the handler, and folds every emission into the resting-order
// book.
//
// The Sequence the producer set is overwritten. That is not a liberty: the
// simulator interleaves fills into the stream, so only the loop can know the
// running order, and replay.Engine.Run refuses a stream with a gap. It is the
// same thing replay.Engine already does to a handler's emissions, for the
// same reason — one component owns the numbering of one stream. A driver that
// also needs to check the producer's own numbering for gaps must do so before
// calling here.
//
// A handler error is returned with the emissions it arrived alongside already
// recorded (replay.Handler's contract: a handler that fails closed may emit
// a final event explaining why, and that event must still reach the
// journal).
func (s *Simulator) deliver(ctx context.Context, handler replay.Handler, envelope event.Envelope, ref reference, result *Result) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.sequence++
	envelope.Sequence = s.sequence
	if err := envelope.Validate(); err != nil {
		return fmt.Errorf("fills: input %d: %w", s.sequence, err)
	}

	decisions, applyErr := handler.Apply(ctx, envelope)
	result.add(envelope, decisions)
	if applyErr != nil {
		return fmt.Errorf("fills: apply event %s at sequence %d: %w", envelope.ID, envelope.Sequence, applyErr)
	}
	for i, decision := range decisions {
		if err := s.observe(decision, ref); err != nil {
			return fmt.Errorf("fills: emission %d of event %s: %w", i, envelope.ID, err)
		}
	}
	return nil
}

// chargeFor is the commission on candidate c: ADR 0013's charge on each
// broker order the fill journals, summed in the orders' recorded order.
func (s *Simulator) chargeFor(c candidate) (float64, error) {
	if len(c.orderQuantities) == 0 {
		return s.commission.Charge(c.quantity, c.price, s.dollarsPerPoint)
	}
	var total float64
	for _, q := range c.orderQuantities {
		charge, err := s.commission.Charge(q, c.price, s.dollarsPerPoint)
		if err != nil {
			return 0, err
		}
		total += charge
	}
	return total, nil
}
