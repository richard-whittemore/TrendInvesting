package targetweight

import (
	"fmt"
	"slices"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// applyRetraction removes a session the exchange cancelled from the calendar.
// A calendar session that never closes is a gap in every instrument's closes
// (ADR 0026, point 5), which would leave every window that reaches it short
// for a year; a retracted session is no session, so windows fill from the
// sessions before it and no decision time falls on it (point 1).
//
// Only a session still to come can be retracted: one on or before the last
// session closed has already been decided on or journalled as missed. A
// retraction of a session already retracted is a redelivery and changes
// nothing; a date the calendar does not list is refused.
func (r *momentumRun) applyRetraction(in event.Envelope) ([]event.Envelope, error) {
	p, err := decodeInput[event.SessionRetractedPayload](in, event.SessionRetractedSchemaVersion)
	if err != nil {
		return nil, err
	}
	if r.retracted[p.Session] {
		return nil, nil
	}
	at, listed := slices.BinarySearch(r.calendar, p.Session)
	switch {
	case !listed:
		return nil, fmt.Errorf("targetweight: retraction %s names %s, which is not on the calendar", in.ID, p.Session)
	case p.Session <= r.last:
		return nil, fmt.Errorf("targetweight: retraction %s names %s, on or before the last session closed, %s: it has already closed or been journalled missed", in.ID, p.Session, r.last)
	}
	for _, bar := range r.bars {
		if bar.PeriodEnd.UTC().Format(time.DateOnly) == p.Session {
			return nil, fmt.Errorf("targetweight: retraction %s names %s, but bars for it have arrived, so it traded", in.ID, p.Session)
		}
	}
	prev, _ := slices.BinarySearch(r.calendar, r.last) // the last session closed is on the calendar
	next := r.last != "" && at == prev+1
	if next && at+1 == len(r.calendar) {
		return nil, fmt.Errorf("targetweight: retraction %s would leave no session after the last closed, %s, so whether it is the last before a rebalance cannot be known", in.ID, r.last)
	}
	// Whether the last session closed was a decision session, while the
	// retracted one still follows it (both on the validated calendar).
	was, _ := momentum.IsDecisionSession(r.last, p.Session)
	r.calendar = slices.Delete(r.calendar, at, at+1)
	r.retracted[p.Session] = true
	if !next || was {
		return nil, nil
	}
	// The last session closed is now followed by the session after the one
	// retracted: if that is in another month, it was the last before the
	// rebalance, and its decision is due now.
	decide, _ := momentum.IsDecisionSession(r.last, r.calendar[prev+1])
	if !decide || r.lastEnd.Before(r.h.decideFrom) {
		return nil, nil
	}
	return r.decide(&emissions{h: r.h, in: in, at: r.lastEnd}, r.lastEnd, prev)
}
