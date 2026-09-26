package strategy

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/indicator"
)

// pendingSignalState is a Signal whose sizing waits for its Session to close
// (ADR 0021). A bar emits its Signal at once, but ADR 0010 decides a day's
// entries only after its exits and Adds, and ranks simultaneous Signals
// against each other, which cannot happen until every bar of the Session
// has arrived. It holds exactly what applyCompletedBar read from the bars
// BEFORE the Signal's own (CONTEXT.md: "Completed bar"), so sizing at the
// close decides from the same inputs sizing at the bar would have.
//
// It is not a proposal and reserves nothing. It lives from its bar to its
// Session's close, which consumes it.
type pendingSignalState struct {
	signalID  string
	periodEnd time.Time
	// entryLevel is the Entry Channel high the breakout exceeded: the level
	// the resting buy-stop sits at (ADR 0005).
	entryLevel float64
	n          float64
	nReady     bool
	// earliestFillAt is the period end of the bar before the Signal's own:
	// the previous close ADR 0010's cash basis is measured at.
	earliestFillAt time.Time
}

// admitToSession places a completed bar in the open Session, opening one if
// none is open (ADR 0021). A Session is every bar sharing one period end, and
// Sessions follow one another strictly: a bar for any other period end while
// a Session is open, or for a period end not after the last closed Session,
// fails closed. Either would let a day be decided before all of its bars had
// arrived, or reopen a day already decided.
func (r *transition) admitToSession(bar event.CompletedBarPayload) error {
	if r.sessionOpen {
		if !bar.PeriodEnd.Equal(r.sessionPeriodEnd) {
			return fmt.Errorf("strategy: instrument %q: bar period end %s arrived while the Session ending %s is still open; a Session must close before the next begins (ADR 0021)",
				bar.InstrumentID, bar.PeriodEnd.Format(time.RFC3339), r.sessionPeriodEnd.Format(time.RFC3339))
		}
		return nil
	}
	if r.hasClosedSession && !bar.PeriodEnd.After(r.lastClosedSession) {
		return fmt.Errorf("strategy: instrument %q: bar period end %s is not after the last closed Session %s; a closed Session is never reopened (ADR 0021)",
			bar.InstrumentID, bar.PeriodEnd.Format(time.RFC3339), r.lastClosedSession.Format(time.RFC3339))
	}
	r.sessionOpen = true
	r.sessionPeriodEnd = bar.PeriodEnd
	// Every Session that ever opens gets the next generation, once, here —
	// the one place unit_caps.go's protectedSessionGeneration and
	// freedThisSessionUnits both read (Reducer.sessionGeneration's own doc
	// comment).
	r.sessionGeneration++
	return nil
}

// admitDelistedBar records a bar for a delisted instrument as received in the
// open Session. It decides nothing (ADR 0009), but the producer sent it, so
// the Session's close names it. A second one in one Session fails closed,
// just as a second live bar does through bar chronology.
func (r *transition) admitDelistedBar(bar event.CompletedBarPayload) error {
	if slices.Contains(r.sessionDelistedBars, bar.InstrumentID) {
		return fmt.Errorf("strategy: instrument %q already has a bar in the Session ending %s; an instrument is evaluated once per Session (ADR 0011, ADR 0021)",
			bar.InstrumentID, r.sessionPeriodEnd.Format(time.RFC3339))
	}
	r.sessionDelistedBars = append(r.sessionDelistedBars, bar.InstrumentID)
	return nil
}

// barReceivedInOpenSession reports whether the open Session already holds a
// bar for instrumentID.
func (r *transition) barReceivedInOpenSession(instrumentID string) bool {
	if !r.sessionOpen {
		return false
	}
	state := r.peekInstrument(instrumentID)
	return state != nil && state.lastPeriodEnd.Equal(r.sessionPeriodEnd)
}

// applySessionClosed handles event.SessionClosedEventType: every bar of the
// open Session has arrived, so its Adds and entries are decided now, in one
// pass and in ADR 0010's order (ADR 0021). The Session's exits were already
// decided at their bars, because an exit concerns its own Campaign alone.
//
//  1. Adds, for every held Campaign whose bar reached its next rung and did
//     not propose an exit, in ascending instrument order. An Add is not a
//     Signal, so it is not ranked.
//  2. Entries, for every Signal of the Session, in rankSignals' order.
//
// Each is checked against the caps and cash this reducer implements, in that
// order, and each proposal places its hold before the next is checked (ADR
// 0020, as amended 2026-09-24), so the budget they share is spent on Adds
// before entries, and on entries in rankSignals' order.
// The pass reads every instrument, but copies only those it proposes for.
func (r *transition) applySessionClosed(envelope event.Envelope) ([]event.Envelope, error) {
	if !r.configured {
		return nil, errors.New("strategy: received a session close before a configuration event; failing closed")
	}
	if envelope.SchemaVersion != event.SessionClosedSchemaVersion {
		return nil, fmt.Errorf("strategy: session closed payload schema version %d does not match the version %d this build requires; an older or newer schema is rejected, never silently upgraded, until an explicit upcaster exists (ADR 0015)", envelope.SchemaVersion, event.SessionClosedSchemaVersion)
	}
	var payload event.SessionClosedPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return nil, fmt.Errorf("strategy: decode session closed payload: %w", err)
	}
	if err := payload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: %w", err)
	}
	if !r.sessionOpen {
		return nil, fmt.Errorf("strategy: a session close for %s arrived, but no Session is open: no bar has arrived since the last one closed (ADR 0021)",
			payload.PeriodEnd.Format(time.RFC3339))
	}
	if !payload.PeriodEnd.Equal(r.sessionPeriodEnd) {
		return nil, fmt.Errorf("strategy: a session close for %s does not match the open Session ending %s (ADR 0021)",
			payload.PeriodEnd.Format(time.RFC3339), r.sessionPeriodEnd.Format(time.RFC3339))
	}

	live := r.sessionLiveInstruments()
	if err := checkSessionNamesItsBars(payload.InstrumentIDs, live, r.sessionDelistedBars); err != nil {
		return nil, err
	}

	var emissions []event.Envelope

	// ADR 0009's monthly, point-in-time universe evaluation: recorded before
	// this Session's Adds and entries are decided, so a Signal ranked or
	// declined below already reflects it. It never touches an open
	// Campaign — see universe.go's own doc comment.
	universeEmissions, err := r.evaluateUniverse(envelope)
	if err != nil {
		return nil, err
	}
	emissions = append(emissions, universeEmissions...)

	// ADR 0011's Watchlist: the ranked set of every Setup this Session
	// evaluated to Tier A or Tier B — the pre-image of the entries decided
	// below, and the first thing reviewed each day (CONTEXT.md:
	// "Watchlist"). Built and emitted before this Session's Adds and
	// entries, but it is pure observability: nothing decided below reads it,
	// and it reads nothing they decide (ADR 0011, decision 3).
	watchlistEmissions, err := r.emitWatchlist(live, envelope)
	if err != nil {
		return nil, err
	}
	emissions = append(emissions, watchlistEmissions...)

	for _, id := range live {
		if !r.peekInstrument(id).addDue {
			continue
		}
		state, _ := r.instrument(id)
		state.addDue = false
		// Re-read the Campaign: a fill between the bar and this close may
		// have closed it or raised an exit it must now yield to.
		if state.campaign == nil || state.pendingExitProposal != nil {
			continue
		}
		added, err := r.evaluateAdd(state, envelope)
		if err != nil {
			return nil, err
		}
		emissions = append(emissions, added...)
	}

	var signalled []string
	for _, id := range live {
		if r.peekInstrument(id).pendingSignal != nil {
			signalled = append(signalled, id)
		}
	}

	// ADR 0009: an instrument this run's most recent monthly evaluation
	// found ineligible is declined before ranking, and never enters
	// rankSignals' order at all — the identical shape as ADR 0010's own
	// insufficient-history exclusion just below, for an unrelated reason.
	// This never touches any open Campaign: a Signal fires only for a Setup
	// (CONTEXT.md defines a Setup as an instrument NOT in a Campaign), so an
	// instrument declined here has none to close.
	var candidates []string
	for _, id := range signalled {
		if ineligible, detail := r.universeIneligible(id); ineligible {
			state, _ := r.instrument(id)
			signal := state.pendingSignal
			state.pendingSignal = nil
			declined, err := r.decline(id, signal.periodEnd, envelope, signal.signalID,
				event.DeclineReasonIneligible, detail, 0, 0, 0)
			if err != nil {
				return nil, err
			}
			emissions = append(emissions, declined)
			continue
		}
		candidates = append(candidates, id)
	}

	// Rank first, so an instrument that cannot be ranked is declined instead
	// of entering rankSignals' order at all (ADR 0010, as amended by the
	// owner's decision of 2026-09-25: an incomparable instrument has no place
	// in a total order).
	var rankable []signalRanking
	for _, id := range candidates {
		ranking, detail, ranked := r.rankSignal(id)
		if ranked {
			rankable = append(rankable, ranking)
			continue
		}
		state, _ := r.instrument(id)
		signal := state.pendingSignal
		state.pendingSignal = nil
		declined, err := r.decline(id, signal.periodEnd, envelope, signal.signalID,
			event.DeclineReasonInsufficientHistory, detail, 0, 0, 0)
		if err != nil {
			return nil, err
		}
		emissions = append(emissions, declined)
	}

	strengthByID := make(map[string]float64, len(rankable))
	for _, ranking := range rankable {
		strengthByID[ranking.instrumentID] = ranking.strength
	}
	for _, id := range rankSignals(rankable) {
		state, _ := r.instrument(id)
		signal := state.pendingSignal
		state.pendingSignal = nil
		sized, err := r.sizeUnit(id, signal.periodEnd, envelope, signal.signalID, signal.entryLevel, signal.n, signal.nReady, signal.earliestFillAt, strengthByID[id])
		if err != nil {
			return nil, err
		}
		emissions = append(emissions, sized)
		// A proposal is remembered as outstanding so that a fill can be
		// checked against it; nothing about position state moves here.
		if err := r.rememberPendingProposal(state, sized, signal.earliestFillAt); err != nil {
			return nil, err
		}
	}

	r.sessionOpen = false
	r.hasClosedSession = true
	r.lastClosedSession = r.sessionPeriodEnd
	r.sessionPeriodEnd = time.Time{}
	r.sessionDelistedBars = nil
	return emissions, nil
}

// signalRanking pairs a rankable Signal's instrument ID with the two figures
// ADR 0010's order compares: Strength and 20-day median dollar volume.
// rankSignals breaks the remaining tie directly on instrumentID, so it is not
// carried here.
type signalRanking struct {
	instrumentID string
	strength     float64
	dollarVolume float64
}

// rankSignal computes id's ranking figures for the Session that is closing,
// or reports why id cannot be ranked at all (ADR 0010, as amended by the
// owner's decision of 2026-09-25): fewer than
// indicator.StrengthLookbackBars+1 split-adjusted closes, fewer than
// indicator.DollarVolumeWindow raw closes/volumes, or an N that is not a
// usable volatility reading at this Session's own close. Every one of these
// is insufficient history to rank, never a reason to rank last — an
// incomparable instrument has no place in a total order, so it is declined
// (event.DeclineReasonInsufficientHistory) instead of entering the order at
// all.
//
// n is read directly from the instrument's own indicator.WilderAverage,
// AFTER this Session's own bar has already advanced it (reducer.go's
// applyCompletedBar) — N(d), the Session's own N, deliberately not the
// pre-advance N pendingSignalState.n carries for the Signal's entry sizing:
// see indicator.Strength's own doc comment for why the two differ.
func (r *transition) rankSignal(id string) (ranking signalRanking, detail string, ranked bool) {
	state := r.peekInstrument(id)
	n := state.n.Value()
	if !state.n.Ready() || n <= 0 {
		return signalRanking{}, fmt.Sprintf(
			"n is not a usable volatility reading at the session's close (n %v): insufficient history to rank (ADR 0010, as amended 2026-09-25)", n), false
	}
	closes := state.splitAdjustedCloses.Values()
	strength, strengthReady := indicator.Strength(closes, n)
	rawCloses := state.rawCloses.Values()
	rawVolumes := state.rawVolumes.Values()
	dollarVolume, volumeReady := indicator.MedianDollarVolume(rawCloses, rawVolumes)
	if !strengthReady || !volumeReady {
		return signalRanking{}, fmt.Sprintf(
			"%d split-adjusted close(s) (need %d) and %d raw volume(s) (need %d): insufficient history to rank (ADR 0010, as amended 2026-09-25)",
			len(closes), indicator.StrengthLookbackBars+1, len(rawVolumes), indicator.DollarVolumeWindow), false
	}
	return signalRanking{instrumentID: id, strength: strength, dollarVolume: dollarVolume}, "", true
}

// rankSignals orders a Session's rankable Signals for sizing (ADR 0010, as
// amended by the owner's decision of 2026-09-25): Strength descending, then
// 20-day median dollar volume descending, then instrument ID ascending.
// Instrument IDs are unique, so this is a total order — two Signals tied on
// both Strength and dollar volume still resolve deterministically, and the
// same input always produces the same order, independent of arrival order or
// of Go's own map iteration. Every proposal reserves its cash and cap
// headroom as it is made (ADR 0020, as amended 2026-09-24), so this order
// also decides which Signals are funded when the Session's Signals together
// exceed the cash or a cap.
func rankSignals(rankings []signalRanking) []string {
	sorted := slices.Clone(rankings)
	slices.SortFunc(sorted, func(a, b signalRanking) int {
		if c := cmp.Compare(b.strength, a.strength); c != 0 {
			return c
		}
		if c := cmp.Compare(b.dollarVolume, a.dollarVolume); c != 0 {
			return c
		}
		return cmp.Compare(a.instrumentID, b.instrumentID)
	})
	ids := make([]string, len(sorted))
	for i, ranking := range sorted {
		ids[i] = ranking.instrumentID
	}
	return ids
}

// sessionLiveInstruments returns, in ascending order, every instrument whose
// bar the open Session holds and evaluated. Sessions follow one another
// strictly (admitToSession), so an instrument's last bar carries the open
// Session's period end exactly when it arrived in this Session.
func (r *transition) sessionLiveInstruments() []string {
	var live []string
	for _, id := range r.instrumentIDs() {
		if r.peekInstrument(id).lastPeriodEnd.Equal(r.sessionPeriodEnd) {
			live = append(live, id)
		}
	}
	return live
}

// checkSessionNamesItsBars fails closed unless named, the producer's
// statement of the Session, is exactly the bars received: those evaluated
// (live, ascending) and those absorbed for a delisted instrument.
func checkSessionNamesItsBars(named, live, delisted []string) error {
	received := slices.Sorted(slices.Values(slices.Concat(live, delisted)))
	var missing, extra []string
	for _, id := range named {
		if _, found := slices.BinarySearch(received, id); !found {
			missing = append(missing, id)
		}
	}
	for _, id := range received {
		if _, found := slices.BinarySearch(named, id); !found {
			extra = append(extra, id)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}
	return fmt.Errorf("strategy: the session close does not name exactly the bars received (ADR 0021): named but not received: %q; received but not named: %q", missing, extra)
}
