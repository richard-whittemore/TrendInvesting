package strategy

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
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
	for _, id := range rankSignals(signalled) {
		state, _ := r.instrument(id)
		signal := state.pendingSignal
		state.pendingSignal = nil
		sized, err := r.sizeUnit(id, signal.periodEnd, envelope, signal.signalID, signal.entryLevel, signal.n, signal.nReady, signal.earliestFillAt)
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

// rankSignals orders a Session's Signals for sizing. ADR 0010 ranks
// simultaneous Signals by Strength, (close − close 63 bars ago) / N, highest
// first [T p.29], with ties broken by 20-day median dollar volume and then
// by symbol. Neither Strength nor median dollar volume is computed yet, and
// their definitions are unsettled (ADR 0021, "Open"), so this applies only
// the last tie-break: ascending instrument ID. That is total and independent
// of arrival order. Every proposal reserves its cash and cap headroom as it
// is made (ADR 0020, as amended 2026-09-24), so this order also decides
// which Signals are funded when the Session's Signals together exceed the
// cash or a cap: until Strength is computed, the lowest instrument ID first.
func rankSignals(instrumentIDs []string) []string {
	return slices.Sorted(slices.Values(instrumentIDs))
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
