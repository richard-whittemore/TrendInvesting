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
// Only a session after the last one closed can be retracted, and only while
// nothing of it has arrived: its bars, its universe snapshot or its close
// histories say it traded. A retraction of a session already retracted is a
// redelivery and changes nothing; a date the calendar does not list is
// refused.
//
// When the session retracted followed the last one closed and was its
// month's last, the session closed becomes the last before the rebalance,
// and its decision is due. A run configured for bars holds every close it
// needs and decides at once, or withholds as missing-bars when a member
// ranked from that session's snapshot had no bar in it (barsMissing). A run
// configured for the compact universe input has no histories for the
// members ranked from that session's snapshot, since it was no decision
// session when it closed: its decision waits for them
// (applyPendingHistory), and is withheld as histories-missing if the next
// session closes first.
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
		return nil, fmt.Errorf("targetweight: retraction %s names %s, on or before the last session closed, %s, so it can no longer be retracted", in.ID, p.Session, r.last)
	case r.universe != nil && r.universe.Session == p.Session:
		// The histories held follow the snapshot held, so this covers them.
		return nil, fmt.Errorf("targetweight: retraction %s names %s, but its universe snapshot has arrived, so it traded", in.ID, p.Session)
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
	// rebalance, and its decision is due.
	decide, _ := momentum.IsDecisionSession(r.last, r.calendar[prev+1])
	if !decide || r.lastEnd.Before(r.h.decideFrom) {
		return nil, nil
	}
	var u *event.UniverseSnapshotPayload
	if r.universe != nil && r.universe.Session == r.last {
		u = r.universe
	}
	b := &emissions{h: r.h, in: in, at: r.lastEnd}
	switch {
	case u != nil && r.compact:
		ranked, closes := rankedMembers(r.rule, *u)
		if len(ranked) > 0 {
			slices.Sort(ranked)
			r.pending = &pendingDecision{session: r.last, end: r.lastEnd, universe: *u, ranked: ranked, closes: closes, histories: map[string]event.CloseHistoryPayload{}}
			return nil, nil
		}
	case u != nil:
		if detail := r.barsMissing(*u); detail != "" {
			if err := r.withheldAt(b, decisionID(r.h.cfg.AccountID, r.lastEnd), r.last, r.lastEnd, prev, event.WithheldMissingBars, detail); err != nil {
				return nil, err
			}
			return b.result()
		}
	}
	return r.decide(b, r.lastEnd, prev, u)
}
