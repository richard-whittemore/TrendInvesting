package targetweight

import (
	"fmt"
	"maps"
	"slices"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// applyHistory holds one ranked member's close history until its session
// closes. The compact universe input (owner decision of 2026-10-16 on the
// momentum input shape) is one universe snapshot per session and, at a
// decision session, one history per member ranked from it, in place of a bar
// for every listed stock every session. A history must follow its session's
// universe snapshot, and every close it carries must be on a calendar
// session: a close on a date the calendar does not list as a session is a
// malformed input, never a skipped one.
func (r *momentumRun) applyHistory(in event.Envelope) error {
	p, err := decodeInput[event.CloseHistoryPayload](in, event.CloseHistorySchemaVersion)
	if err != nil {
		return err
	}
	switch _, seen := r.histories[p.InstrumentID]; {
	case r.universe == nil || r.universe.Session != p.Session:
		return fmt.Errorf("targetweight: history %s ends at %s, which is not the session of the universe snapshot held", in.ID, p.Session)
	case seen:
		return fmt.Errorf("targetweight: a second close history for %s before its session closed", p.InstrumentID)
	}
	for _, c := range p.Closes {
		if _, ok := slices.BinarySearch(r.calendar, c.Session); !ok {
			return fmt.Errorf("targetweight: history %s has a close on %s, which is not on the calendar", in.ID, c.Session)
		}
	}
	r.histories[p.InstrumentID] = p
	return nil
}

// mergeHistories adds the close histories held to the run's adjusted closes
// at the close of calendar session at (date), after that session's bars.
//
// Histories arrive only for a decision session, and they must name exactly
// the members this run ranks from that session's universe snapshot
// (RankUniverse, ADR 0026, point 1: ranking is a Go rule, so a producer's
// list is checked against it, never trusted). A close that an earlier bar or
// history already gave for the same session must agree with it, on the same
// adjusted base. A member's raw reference price for the session is its
// universe row's close, the same unadjusted trade close its bar would carry,
// unless a bar already gave one.
func (r *momentumRun) mergeHistories(date string, at int) error {
	if len(r.histories) == 0 {
		return nil
	}
	histories := r.histories
	r.histories = map[string]event.CloseHistoryPayload{}
	decide, _ := momentum.IsDecisionSession(date, r.calendar[at+1]) // both on the validated calendar
	if !decide || r.universe.Session != date {
		return fmt.Errorf("targetweight: close histories arrived for the session %s, which is not a decision session with its universe snapshot", date)
	}
	// The configuration and the snapshot's rows are validated, so ranking
	// cannot refuse them; were it to, no member matches and the run stops.
	members, _, _ := momentum.RankUniverse(r.rule, rows(r.universe.Rows))
	ranked := make([]string, len(members))
	closes := make(map[string]float64, len(members))
	for i, m := range members {
		ranked[i] = m.InstrumentID
		closes[m.InstrumentID] = m.Close
	}
	slices.Sort(ranked)
	if got := slices.Sorted(maps.Keys(histories)); !slices.Equal(got, ranked) {
		return fmt.Errorf("targetweight: close histories for the session %s name %v, but the universe ranks %v", date, got, ranked)
	}
	for _, id := range ranked {
		h := histories[id]
		if base := r.bases[id]; base != "" && base != h.AdjustedBase {
			return fmt.Errorf("targetweight: the close history for %s is based at %s; its earlier closes are based at %s, and levels on two bases cannot be compared", id, h.AdjustedBase, base)
		}
		if r.adjusted[id] == nil {
			r.adjusted[id] = map[string]float64{}
		}
		for _, c := range h.Closes {
			if held, ok := r.adjusted[id][c.Session]; ok && held != c.Close {
				return fmt.Errorf("targetweight: the close history for %s gives %v on %s, but the run holds %v", id, c.Close, c.Session, held)
			}
			r.adjusted[id][c.Session] = c.Close
		}
		r.bases[id] = h.AdjustedBase
		if _, ok := r.raw[id]; !ok {
			r.raw[id] = closes[id]
		}
	}
	return nil
}
