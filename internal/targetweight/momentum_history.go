package targetweight

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// applyHistory holds one ranked member's close history until its session
// closes. The compact universe input (owner decision of 2026-10-16 on the
// momentum input shape) is one universe snapshot per session and, at a
// decision session, one history per member ranked from it, in place of a bar
// for every listed stock every session. A history must follow its session's
// universe snapshot, its session must still be to close, and every close it
// carries must be on a calendar session: a close on a date the calendar does
// not list as a session is a malformed input, never a skipped one. The one
// history for a session already closed that is read is one for the decision
// a retraction left awaiting its histories (applyRetraction), which decides
// once the last of them arrives.
func (r *momentumRun) applyHistory(in event.Envelope) ([]event.Envelope, error) {
	p, err := decodeInput[event.CloseHistoryPayload](in, event.CloseHistorySchemaVersion)
	if err != nil {
		return nil, err
	}
	if !r.compact {
		return nil, fmt.Errorf("targetweight: history %s: this run reads bars for its universe members (universe input %q), never a close history", in.ID, event.UniverseInputBars)
	}
	if r.pending != nil && p.Session == r.pending.session {
		return r.applyPendingHistory(in, p)
	}
	switch _, seen := r.histories[p.InstrumentID]; {
	case p.Session <= r.last:
		return nil, fmt.Errorf("targetweight: history %s ends at %s, on or before the last session closed, %s, and no decision there awaits its histories: that session has already closed", in.ID, p.Session, r.last)
	case r.universe == nil || r.universe.Session != p.Session:
		return nil, fmt.Errorf("targetweight: history %s ends at %s, which is not the session of the universe snapshot held", in.ID, p.Session)
	case seen:
		return nil, fmt.Errorf("targetweight: a second close history for %s before its session closed", p.InstrumentID)
	}
	if err := r.onCalendar(in, p); err != nil {
		return nil, err
	}
	r.histories[p.InstrumentID] = p
	return nil, nil
}

// onCalendar refuses a history with a close on a date the calendar does not
// list as a session.
func (r *momentumRun) onCalendar(in event.Envelope, p event.CloseHistoryPayload) error {
	for _, c := range p.Closes {
		if _, ok := slices.BinarySearch(r.calendar, c.Session); !ok {
			return fmt.Errorf("targetweight: history %s has a close on %s, which is not on the calendar", in.ID, c.Session)
		}
	}
	return nil
}

// pendingDecision is a decision a retraction made due at a session already
// closed, in a run reading the compact universe input: the members ranked
// from that session's universe snapshot had no histories, since it was no
// decision session when it closed. The decision waits for them.
type pendingDecision struct {
	session   string
	end       time.Time
	universe  event.UniverseSnapshotPayload
	ranked    []string // sorted
	closes    map[string]float64
	histories map[string]event.CloseHistoryPayload
}

// applyPendingHistory holds one history of the decision awaiting them and,
// with the last of the members ranked, merges them and decides, as that
// session's close would have had its calendar never listed the session
// retracted.
func (r *momentumRun) applyPendingHistory(in event.Envelope, p event.CloseHistoryPayload) ([]event.Envelope, error) {
	pd := r.pending
	if _, seen := pd.histories[p.InstrumentID]; seen {
		return nil, fmt.Errorf("targetweight: a second close history for %s awaiting the decision at %s", p.InstrumentID, pd.session)
	}
	if !slices.Contains(pd.ranked, p.InstrumentID) {
		return nil, fmt.Errorf("targetweight: history %s names %s, but the universe of %s ranks %v", in.ID, p.InstrumentID, pd.session, pd.ranked)
	}
	if err := r.onCalendar(in, p); err != nil {
		return nil, err
	}
	pd.histories[p.InstrumentID] = p
	if len(pd.histories) < len(pd.ranked) {
		return nil, nil
	}
	r.pending = nil
	if err := r.merge(pd.ranked, pd.closes, pd.histories); err != nil {
		return nil, err
	}
	at, _ := slices.BinarySearch(r.calendar, pd.session) // the last session closed is on the calendar
	return r.decide(&emissions{h: r.h, in: in, at: pd.end}, pd.end, at, &pd.universe)
}

// mergeHistories adds the close histories held to the run's adjusted closes
// at the close of calendar session at (date), after that session's bars.
//
// Histories arrive only for a decision session, and they must name exactly
// the members this run ranks from that session's universe snapshot
// (RankUniverse, ADR 0026, point 1: ranking is a Go rule, so a producer's
// list is checked against it, never trusted). The configuration declares
// the universe input, so the check runs at every decision session with its
// snapshot from the first: in the compact input, a decision session with no
// history for the members ranked returns why (missing), for the decision to
// be withheld as histories-missing (reason); in the bars input, one with no
// bar for any member ranked is withheld as missing-bars (barsMissing).
// Neither is decided from the bars of the names held alone (ADR 0026, point
// 5, "fail closed").
func (r *momentumRun) mergeHistories(date string, at int) (reason, missing string, err error) {
	histories := r.histories
	r.histories = map[string]event.CloseHistoryPayload{}
	decide, _ := momentum.IsDecisionSession(date, r.calendar[at+1]) // both on the validated calendar
	held := r.universe != nil && r.universe.Session == date
	if len(histories) > 0 && (!decide || !held) {
		return "", "", fmt.Errorf("targetweight: close histories arrived for the session %s, which is not a decision session with its universe snapshot", date)
	}
	if !decide || !held {
		return "", "", nil
	}
	if !r.compact {
		if detail := r.barsMissing(*r.universe); detail != "" {
			return event.WithheldMissingBars, detail, nil
		}
		return "", "", nil
	}
	ranked, closes := rankedMembers(r.rule, *r.universe)
	slices.Sort(ranked)
	if len(histories) == 0 {
		if len(ranked) > 0 {
			return event.WithheldHistoriesMissing, fmt.Sprintf("no market.close.history arrived for the %d members ranked from the universe snapshot of %s, so none can be scored", len(ranked), date), nil
		}
		return "", "", nil
	}
	if got := slices.Sorted(maps.Keys(histories)); !slices.Equal(got, ranked) {
		return "", "", fmt.Errorf("targetweight: close histories for the session %s name %v, but the universe ranks %v", date, got, ranked)
	}
	return "", "", r.merge(ranked, closes, histories)
}

// barsMissing says why a decision in the bars input cannot be made from the
// universe snapshot u of the last session closed: the snapshot ranks members
// and none had a bar in that session, as in a stream written without the
// members' bars. A member left out of some sessions for bad data leaves the
// others, so one ranked member's bar is enough.
func (r *momentumRun) barsMissing(u event.UniverseSnapshotPayload) string {
	ranked, _ := rankedMembers(r.rule, u)
	if len(ranked) == 0 || slices.ContainsFunc(ranked, func(id string) bool { _, ok := r.raw[id]; return ok }) {
		return ""
	}
	return fmt.Sprintf("none of the %d members ranked from the universe snapshot of %s has a bar in that session, so none can be scored; a run configured for bars needs the ranked members' bars", len(ranked), u.Session)
}

// merge adds the histories of the members ranked to the run's adjusted
// closes. A close that an earlier bar or history already gave for the same
// session must agree with it, on the same adjusted base. A member's raw
// reference price for the session is its universe row's close (closes), the
// same unadjusted trade close its bar would carry: a member with a bar too
// must have the two agree, since the planner and the gate value its orders
// at that price.
func (r *momentumRun) merge(ranked []string, closes map[string]float64, histories map[string]event.CloseHistoryPayload) error {
	for _, id := range ranked {
		h := histories[id]
		if base := r.bases[id]; base != "" && base != h.AdjustedBase {
			return fmt.Errorf("targetweight: the close history for %s is based at %s; its earlier closes are based at %s, and levels on two bases cannot be compared", id, h.AdjustedBase, base)
		}
		if raw, ok := r.raw[id]; ok && raw != closes[id] {
			return fmt.Errorf("targetweight: the universe row close %v for %s disagrees with its bar's raw close %v", closes[id], id, raw)
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
		r.raw[id] = closes[id]
	}
	return nil
}

// HistoryRequest is what the compact universe input needs from one session's
// universe snapshot: whether the session is a decision session and, for one,
// the members the momentum run will rank from the snapshot, in rank order,
// and how many calendar sessions of closes, ending at the session, each
// member's market.close.history must cover. The market-data step asks for it
// rather than rank in Python, since ranking is a rule (ADR 0026, point 1).
type HistoryRequest struct {
	Session         string   `json:"session"`
	DecisionSession bool     `json:"decision_session"`
	Window          int      `json:"window"`
	Members         []string `json:"members"`
}

// MomentumHistoryRequest answers HistoryRequest for the snapshot u, whose
// next calendar session is next, under the momentum configuration cfg, with
// the ranking and the window the run itself uses. A configuration or
// snapshot the run would refuse is an error, as is a next session that does
// not follow the snapshot's.
func MomentumHistoryRequest(cfg event.TargetWeightConfigurationPayload, u event.UniverseSnapshotPayload, next string) (HistoryRequest, error) {
	if cfg.StrategyID != event.TargetWeightStrategyMomentum {
		return HistoryRequest{}, fmt.Errorf("targetweight: a history request needs a momentum configuration, not %q", cfg.StrategyID)
	}
	if err := errors.Join(cfg.Validate(), u.Validate()); err != nil {
		return HistoryRequest{}, fmt.Errorf("targetweight: %w", err)
	}
	if cfg.Momentum.UniverseInput != event.UniverseInputCompact {
		return HistoryRequest{}, fmt.Errorf("targetweight: a history request needs a configuration reading the compact universe input; this one reads bars (universe input %q)", cfg.Momentum.UniverseInput)
	}
	if err := checkUniverse(cfg.Momentum, "for "+u.Session, u); err != nil {
		return HistoryRequest{}, err
	}
	decide, err := momentum.IsDecisionSession(u.Session, next)
	if err != nil {
		return HistoryRequest{}, fmt.Errorf("targetweight: %w", err)
	}
	req := HistoryRequest{Session: u.Session, DecisionSession: decide, Window: momentumWindow(cfg.Momentum), Members: []string{}}
	if decide {
		req.Members, _ = rankedMembers(momentumRule(cfg.Momentum), u)
	}
	return req, nil
}

// rankedMembers returns the members the rule ranks from u, in rank order,
// with each one's raw close. The configuration and the snapshot are
// validated before either caller ranks, so RankUniverse cannot refuse them;
// were it to, no member is ranked, and a history set then matches nothing.
func rankedMembers(rule momentum.Config, u event.UniverseSnapshotPayload) (ids []string, closes map[string]float64) {
	members, _, _ := momentum.RankUniverse(rule, rows(u.Rows))
	ids = make([]string, len(members))
	closes = make(map[string]float64, len(members))
	for i, m := range members {
		ids[i] = m.InstrumentID
		closes[m.InstrumentID] = m.Close
	}
	return ids, closes
}

// checkUniverse refuses a snapshot from a source the configuration does not
// read, or with a sector label from an unconfigured source.
func checkUniverse(m event.MomentumConfiguration, id string, p event.UniverseSnapshotPayload) error {
	if p.Source != m.UniverseSource {
		return fmt.Errorf("targetweight: universe %s is from %q; this run reads %q", id, p.Source, m.UniverseSource)
	}
	for _, row := range p.Rows {
		if row.SectorSource != "" && !slices.Contains(m.SectorSources, row.SectorSource) {
			return fmt.Errorf("targetweight: universe %s labels %s from %q, not a configured sector source", id, row.InstrumentID, row.SectorSource)
		}
	}
	return nil
}

// momentumRule is the momentum rule's configuration (ADR 0026, point 6).
func momentumRule(m event.MomentumConfiguration) momentum.Config {
	return momentum.Config{
		ListingType: m.ListingType, UniverseSize: m.UniverseSize, MinPrice: m.MinPrice,
		ScoreLookback: m.ScoreLookback, ScoreSkip: m.ScoreSkip, VolatilityReturns: m.VolatilityReturns,
		TopN: m.TopN, SectorCap: m.SectorCap, Invested: m.Invested,
		TrendInstrument: m.TrendInstrument, TrendCloses: m.TrendCloses,
		RiskOffInstrument: m.RiskOffInstrument, RiskOffFraction: m.RiskOffFraction,
	}
}

// momentumWindow is how many calendar sessions of closes the run keeps: the
// longest lookback, the score's ScoreLookback+1 closes or the trend's
// TrendCloses.
func momentumWindow(m event.MomentumConfiguration) int {
	return max(m.ScoreLookback+1, m.TrendCloses)
}
