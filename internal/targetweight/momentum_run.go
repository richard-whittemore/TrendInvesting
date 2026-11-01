package targetweight

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
	"github.com/richard-whittemore/TrendInvesting/internal/reconcile"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/glide"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// momentumRun decides the momentum combination for one IRA (plan.md section
// 2, "Momentum, combination"; ADR 0026, points 1 to 3).
//
// # Inputs
//
// After the configuration, in any order between sessions:
//   - market.session.calendar: the expected sessions. Every session closed
//     must be on it, and it must name the session after it, so the handler
//     knows whether that session is the first of a month;
//   - per session: each instrument's market.bar.completed version 2 (the
//     trend instrument, the risk-off instrument and holdings, and universe
//     members unless their close histories come instead; each instrument's
//     bars on one adjusted base), then
//     market.session.closed naming exactly those instruments;
//   - market.universe.snapshot, from the configured source, for the session
//     it ranks;
//   - market.close.history, after a decision session's universe snapshot and
//     before its close: one per member the run ranks from that snapshot, in
//     place of those members' daily bars (the compact universe input,
//     applyHistory), or, for a session already closed that a retraction
//     made a decision session, after that retraction;
//   - market.session.retracted: a session the exchange cancelled, removed
//     from the calendar (applyRetraction);
//   - account.snapshot version 4 and market.mark.recorded per instrument;
//   - execution.order.lifecycle version 2 and execution.fill version 5 for the
//     orders this run released;
//   - operator.kill-switch, which refuses every order while engaged;
//   - operator.run.approved, straight after the configuration of a fresh
//     run after an earlier one: the owner's approval stating the rebalance
//     in progress when that run ended, or none (applyApproval);
//   - operator.rebalance.cleared, the owner's clearance of the rebalance in
//     progress (applyCleared); and
//   - account.broker.snapshot, whose holdings net of working sells size
//     every sell the gate checks after it (ADR 0026, points 2.5, 3 and 4).
//     An IRA gets no sale back to 98% and no margin alarm.
//
// # Decision time
//
// A session whose next calendar session is in another month is the last
// before the rebalance: at its close the run decides, from closes to that
// session and the universe ranked on it (plan.md lines 44-45; D8 timing). A
// calendar session that never closed is a gap in every instrument's closes,
// which the rule treats as missing bars (ADR 0026, point 5); when it was a
// decision session, the next close journals its decision withheld
// (missing-bars), so no decision time passes without one (point 1).
//
// # Glide path
//
// Each decision counts the owner's age in whole months on its rebalance
// session, the next calendar session (glide_path.py:242 computes b on the
// month's first session), and sets b from the configured schedule. The
// momentum book, with its own risk-off IEF, is scaled to 1 - b; the bond
// sleeve holds 98% of b split over the bond funds; a fund both sleeves hold
// is one target, the sum, with each sleeve's share recorded (plan.md section
// 2, "Bond sleeve and glide path"; glide_path.py:80-88,177). The planner's
// band applies to the summed targets. Every decision, withheld or not,
// records the age and b.
//
// # Fail-closed rules
//
// A decided rebalance is withheld instead when the account snapshot is older
// than the session (stale-account-snapshot), when a held or targeted
// instrument has no bar in the session (missing-bars, which covers the
// final bars of the risk-off instrument and of the bond funds, which the
// bond sleeve targets whenever b is above 0; ADR 0026, point 5), or when an
// order of an earlier rebalance is not yet terminal (rebalance-in-progress).
//
// # Execution
//
// No order is working when a rebalance is planned, since an earlier
// rebalance's order still working withholds it, so the planner and the gate
// start with no working sells. The sells are planned at the decision; the buys only once every released
// sell is terminal, as a second set of orders caused by the last terminal
// report, or at once when no sell was released (ADR 0026, point 2, "IRA
// execution"). The buys are budgeted from cash under the D5 rule and scaled
// to fit; the budget is carried through each release and fill, less any cash
// a newer snapshot shows gone that the fills do not explain.
//
// # Dry-run
//
// A dry-run releases every order with send false, so no venue ever reports
// on it. The run records a synthetic terminal report for each as it is
// released (endUnsent), so the buy phase is planned at the same close and the
// next rebalance is not withheld as rebalance-in-progress. Nothing executes:
// no fill, proceeds or commission is counted, so the buys are budgeted from
// the snapshot's cash alone. Paper and live runs never record one.
type momentumRun struct {
	h       *Handler
	rule    momentum.Config
	cfg     event.MomentumConfiguration
	exec    event.ExecutionConfiguration
	window  int // calendar sessions of closes kept: the longest lookback
	anchors []glide.Anchor
	funds   []glide.Weight

	calendar []string
	last     string    // last session closed, YYYY-MM-DD
	lastEnd  time.Time // its period end

	adjusted    map[string]map[string]float64 // instrument -> session -> adjusted close
	raw         map[string]float64            // instrument -> raw close in the last session
	barred      map[string]bool               // instruments with their own bar in the last session
	bars        map[string]event.CompletedBarAdjustedPayload
	bases       map[string]string // instrument -> its adjusted view's base
	universe    *event.UniverseSnapshotPayload
	snapshot    *event.AccountCashSnapshotPayload
	marks       map[string]event.MarkRecordedPayload
	markAt      map[string]uint32 // each mark's schema version (bandMark)
	unavailable map[string]event.MarkUnavailablePayload

	// orders reads the venue's reports on the orders released, by the rule
	// the reporter reads the log by (event.OrderReports).
	orders    *event.OrderReports
	rebalance *rebalance

	// histories holds the ranked members' close histories until their
	// session closes; retracted, every session withdrawn from the calendar.
	// compact is the configuration's universe input: true for the compact
	// input, whose decision sessions' ranked members have no bars and need
	// their histories from the first decision session on; false for bars,
	// which reads no history. pending is the decision a retraction left
	// awaiting its histories (applyRetraction).
	histories map[string]event.CloseHistoryPayload
	retracted map[string]bool
	compact   bool
	pending   *pendingDecision

	// carried is the rebalance an earlier run left in progress, as the
	// owner's approval states it (applyApproval): its orders are that run's,
	// which this run never released, so it sends nothing for that decision
	// time and withholds every decision as rebalance-in-progress until the
	// owner clears it (ADR 0026, point 5); nil when none is.
	carried *event.RebalanceInProgress
}

// rebalance is the decision time in progress: what its buy phase needs.
type rebalance struct {
	decisionID   string
	decision     event.TargetsDecidedPayload // immutable original targets and decision session
	session      time.Time
	snapshot     event.AccountCashSnapshotPayload
	prices       map[string]float64
	buys         []event.OrderPlannedPayload
	withheld     map[string]bool // once withheld, stays cash through the buy phase
	sells        []string        // client order IDs of released sells
	sold         map[string]int64
	sellsPlanned int
	budget       *orderplan.Budget
	// sellRetired is set once a sell of the rebalance is retired as never
	// sent (applyRetired): the rebalance then ends with its buy phase never
	// started, since that phase would buy from the decision's targets,
	// prices and cash after a session that sold nothing it planned (ADR
	// 0026, point 5, amended 2026-10-22).
	sellRetired bool
}

func newMomentum(strategyVersion string, cfg event.TargetWeightConfigurationPayload, opts Options) (*Handler, error) {
	m := cfg.Momentum
	rule := momentumRule(m)
	// The bond sleeve holds glide.Invested of b, so the momentum book, scaled
	// to 1 - b, must be invested at most that for the two to fit.
	if m.Invested > glide.Invested {
		return nil, fmt.Errorf("targetweight: momentum invested %v is above the bond sleeve's %v", m.Invested, glide.Invested)
	}
	anchors := make([]glide.Anchor, len(cfg.Glide.Anchors))
	for i, a := range cfg.Glide.Anchors {
		anchors[i] = glide.Anchor{AgeYears: a.AgeYears, Share: a.Share}
	}
	funds := make([]glide.Weight, len(cfg.Glide.BondFunds))
	for i, f := range cfg.Glide.BondFunds {
		funds[i] = glide.Weight{Symbol: f.InstrumentID, Weight: f.Fraction}
	}
	// A birth value implying an implausible age on the first session decided
	// is refused here, as a likely typo, before any input is read.
	if _, err := glide.AgeMonths(cfg.Glide.Birth, opts.DecideFrom); err != nil {
		return nil, fmt.Errorf("targetweight: %w", err)
	}
	gate, err := risklimit.NewIRAGate(cfg.AccountID, cfg.Limits, cfg.Mode, cfg.Execution.MaxFeesPerOrder, time.Duration(cfg.Execution.MaxSnapshotAge)*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("targetweight: %w", err)
	}
	if err := gate.SetQuantityScale(cfg.Execution.QuantityScale()); err != nil {
		return nil, err
	}
	if cfg.Execution.ShareQuantity == event.ShareQuantityFractional {
		slack := cfg.Execution.RoundingSlack()
		// Own the optional value: a caller cannot mutate a frozen run's
		// cash rule after its configuration hash is derived (ADR 0016).
		cfg.Execution.RoundingSlackPerOrder = &slack
		if err := gate.SetRoundingSlackPerOrder(slack); err != nil {
			return nil, err
		}
	}
	if cfg.Execution.CashBufferFraction != nil {
		// Freeze the configured reserve before deriving identity (ADR 0016).
		fraction := cfg.Execution.CashBuffer()
		cfg.Execution.CashBufferFraction = &fraction
	}
	h := &Handler{
		strategyVersion: strategyVersion,
		hash:            event.TargetWeightConfigurationHash(cfg),
		cfg:             cfg,
		decideFrom:      opts.DecideFrom,
		gate:            gate,
	}
	h.momentum = &momentumRun{
		h: h, rule: rule, cfg: m, exec: cfg.Execution,
		window:  momentumWindow(m),
		anchors: anchors, funds: funds,
		adjusted:    map[string]map[string]float64{},
		bars:        map[string]event.CompletedBarAdjustedPayload{},
		bases:       map[string]string{},
		marks:       map[string]event.MarkRecordedPayload{},
		markAt:      map[string]uint32{},
		unavailable: map[string]event.MarkUnavailablePayload{},
		orders:      event.NewOrderReports(),

		histories: map[string]event.CloseHistoryPayload{},
		retracted: map[string]bool{},
		compact:   m.UniverseInput == event.UniverseInputCompact,
	}
	return h, nil
}

func (r *momentumRun) apply(in event.Envelope) ([]event.Envelope, error) {
	switch in.Type {
	case event.SessionCalendarEventType:
		return nil, r.applyCalendar(in)
	case event.EngineStateEventType:
		// An earlier run's state and scope, before the first bar or session
		// (ADR 0019, amended 2026-09-24; ADR 0026, point 5).
		return nil, r.h.applyRecordedState(in)
	case event.CompletedBarEventType:
		r.h.started = true
		return nil, r.applyBar(in)
	case event.UniverseSnapshotEventType:
		return nil, r.applyUniverse(in)
	case event.CloseHistoryEventType:
		return r.applyHistory(in)
	case event.SessionRetractedEventType:
		return r.applyRetraction(in)
	case event.AccountSnapshotEventType:
		return r.applySnapshot(in)
	case event.MarkRecordedEventType:
		return nil, r.applyMark(in)
	case event.MarkUnavailableEventType:
		return nil, r.applyMarkUnavailable(in)
	case event.SessionClosedEventType:
		r.h.started = true
		return r.applySession(in)
	case event.KillSwitchEventType:
		// The kill switch refuses every order of an IRA, which gets no sale
		// back to 98% (ADR 0026, points 3 and 4).
		return nil, r.h.applyKillSwitch(in)
	case event.OperatorRunApprovedEventType:
		return r.applyApproval(in)
	case event.RebalanceClearedEventType:
		return nil, r.applyCleared(in)
	case event.BrokerSnapshotEventType:
		// The latest broker holdings and open orders size every sell (ADR
		// 0026, point 2.5); an IRA has no margin alarm, so this emits nothing.
		return r.h.applyBrokerSnapshot(in)
	case event.OrderLifecycleEventType:
		return r.applyLifecycle(in)
	case event.FillEventType:
		return r.applyFill(in)
	case event.OrderRetiredEventType:
		return r.applyRetired(in)
	case event.ReconciliationEventType:
		return r.h.applyReconciliation(in)
	case event.MarketCorporateActionEventType:
		return nil, r.h.applyCorporateAction(in)
	case event.CashMovementEventType:
		return nil, r.h.applyCashMovement(in)
	case event.FlexStatusEventType:
		return nil, r.h.applyFlexStatus(in)
	case event.AlertAcknowledgedEventType:
		return nil, r.h.applyAcknowledgement(in)
	case event.RunCompletedEventType:
		return nil, nil
	}
	return nil, fmt.Errorf("targetweight: input type %q is not one a momentum run reads", in.Type)
}

// applyCalendar merges a calendar into the one held. The two must share a
// session, and over the dates both cover they must list the same sessions: a
// calendar that drops or adds a session the run already knows is a
// contradiction, never a correction. A retracted session is no session, so a
// calendar that still lists one is read without it.
func (r *momentumRun) applyCalendar(in event.Envelope) error {
	p, err := decodeInput[event.SessionCalendarPayload](in, event.SessionCalendarSchemaVersion)
	if err != nil {
		return err
	}
	p.Sessions = slices.DeleteFunc(p.Sessions, func(s string) bool { return r.retracted[s] })
	if len(p.Sessions) == 0 {
		return nil
	}
	if len(r.calendar) > 0 {
		lo, hi := max(p.Sessions[0], r.calendar[0]), min(p.Sessions[len(p.Sessions)-1], r.calendar[len(r.calendar)-1])
		if lo > hi {
			// Merged, the two would leave a hole whose dates read as no
			// session at all (SessionCalendarPayload).
			return fmt.Errorf("targetweight: calendar %s does not overlap the calendar held (%s to %s), so the sessions between them are unknown", in.ID, r.calendar[0], r.calendar[len(r.calendar)-1])
		}
		if !slices.Equal(within(r.calendar, lo, hi), within(p.Sessions, lo, hi)) {
			return fmt.Errorf("targetweight: calendar %s disagrees with the calendar held between %s and %s", in.ID, lo, hi)
		}
	}
	merged := append(slices.Clone(r.calendar), p.Sessions...)
	slices.Sort(merged)
	r.calendar = slices.Compact(merged)
	return nil
}

// within returns the sessions of sorted that lie in [lo, hi].
func within(sorted []string, lo, hi string) []string {
	var out []string
	for _, s := range sorted {
		if s >= lo && s <= hi {
			out = append(out, s)
		}
	}
	return out
}

func (r *momentumRun) applyBar(in event.Envelope) error {
	p, err := decodeInput[event.CompletedBarAdjustedPayload](in, event.CompletedBarAdjustedVersion)
	if err != nil {
		return err
	}
	switch _, seen := r.bars[p.InstrumentID]; {
	case r.bases[p.InstrumentID] != "" && r.bases[p.InstrumentID] != p.AdjustedBase:
		return fmt.Errorf("targetweight: bar %s is based at %s; %s's bars are based at %s, and levels on two bases cannot be compared", in.ID, p.AdjustedBase, p.InstrumentID, r.bases[p.InstrumentID])
	case seen:
		return fmt.Errorf("targetweight: a second bar for %s before its session closed", p.InstrumentID)
	case !p.PeriodEnd.After(r.lastEnd):
		return fmt.Errorf("targetweight: bar %s does not follow the last session %s", p.PeriodEnd, r.lastEnd)
	}
	r.bases[p.InstrumentID] = p.AdjustedBase
	r.bars[p.InstrumentID] = p
	return nil
}

func (r *momentumRun) applyUniverse(in event.Envelope) error {
	p, err := decodeInput[event.UniverseSnapshotPayload](in, event.UniverseSnapshotSchemaVersion)
	if err != nil {
		return err
	}
	if err := checkUniverse(r.cfg, in.ID, p); err != nil {
		return err
	}
	switch {
	case r.universe != nil && p.Session <= r.universe.Session:
		return fmt.Errorf("targetweight: universe for %s does not follow the last, %s", p.Session, r.universe.Session)
	case len(r.histories) > 0:
		// The histories held are checked against the snapshot they follow.
		return fmt.Errorf("targetweight: universe for %s arrived while the close histories of %s still wait for that session to close", p.Session, r.universe.Session)
	}
	r.universe = &p
	return nil
}

func (r *momentumRun) applySnapshot(in event.Envelope) ([]event.Envelope, error) {
	p, err := decodeVersions[event.AccountCashSnapshotPayload](in, []uint32{event.AccountSnapshotCashVersion, event.AccountSnapshotFractionalCashVersion})
	if err != nil {
		return nil, err
	}
	if err := r.h.normalizePositions(&p.AccountPositionsSnapshotPayload); err != nil {
		return nil, err
	}
	if p.AccountID != r.h.cfg.AccountID {
		return nil, fmt.Errorf("targetweight: snapshot for account %q; this run decides %q", p.AccountID, r.h.cfg.AccountID)
	}
	if r.snapshot != nil && !p.AsOf.After(r.snapshot.AsOf) {
		return nil, fmt.Errorf("targetweight: snapshot as of %s does not follow the last, %s", p.AsOf, r.snapshot.AsOf)
	}
	r.snapshot = &p
	return r.h.checkBook(in, reconcile.CashBook(in.ID, p), p.AsOf)
}

func (r *momentumRun) applyMark(in event.Envelope) error {
	p, err := decodeVersions[event.MarkRecordedPayload](in, event.MarkRecordedVersions())
	if err != nil {
		return err
	}
	if last, ok := r.marks[p.InstrumentID]; ok && !p.AsOf.After(last.AsOf) {
		return fmt.Errorf("targetweight: mark for %s as of %s does not follow the last, %s", p.InstrumentID, p.AsOf, last.AsOf)
	}
	if last, ok := r.unavailable[p.InstrumentID]; ok && !p.AsOf.After(last.AsOf) {
		return fmt.Errorf("targetweight: mark for %s does not follow its last unavailable observation", p.InstrumentID)
	}
	r.marks[p.InstrumentID] = p
	r.markAt[p.InstrumentID] = in.SchemaVersion
	return nil
}

// applyMarkUnavailable keeps the adapter's observation for a withheld pick's
// evidence (ADR 0026, amendment of 2026-10-29). Observations advance per name;
// a later successful reading can resolve it before the decision.
func (r *momentumRun) applyMarkUnavailable(in event.Envelope) error {
	p, err := decodeInput[event.MarkUnavailablePayload](in, event.MarkUnavailableSchemaVersion)
	if err != nil {
		return err
	}
	if last, ok := r.unavailable[p.InstrumentID]; ok && !p.AsOf.After(last.AsOf) {
		return fmt.Errorf("targetweight: unavailable mark for %s does not follow its last observation", p.InstrumentID)
	}
	if last, ok := r.marks[p.InstrumentID]; ok && !p.AsOf.After(last.AsOf) {
		return fmt.Errorf("targetweight: unavailable mark for %s does not follow its last mark", p.InstrumentID)
	}
	r.unavailable[p.InstrumentID] = p
	return nil
}

// missingMark returns the evidence for a missing mark at this decision,
// never relabelling absent evidence as an IBKR refusal or timeout (ADR 0026,
// amendment of 2026-10-29).
func (r *momentumRun) missingMark(id string, session time.Time) *event.WithheldPick {
	mark, marked := r.marks[id]
	failure, failed := r.unavailable[id]
	if marked && !mark.AsOf.Before(session) && (!failed || mark.AsOf.After(failure.AsOf)) {
		return nil
	}
	pick := &event.WithheldPick{InstrumentID: id, Reason: event.RefusedMissingMark,
		Detail: fmt.Sprintf("no mark recorded for %s at or after the session's close; no adapter failure detail recorded", id), Failure: event.MarkFailureUnknown}
	if failed && !failure.AsOf.Before(session) {
		pick.Detail, pick.Failure = failure.Detail, failure.Failure
	}
	return pick
}

func (r *momentumRun) applySession(in event.Envelope) ([]event.Envelope, error) {
	p, err := decodeInput[event.SessionClosedPayload](in, event.SessionClosedSchemaVersion)
	if err != nil {
		return nil, err
	}
	date := p.PeriodEnd.UTC().Format(time.DateOnly)
	at, onCalendar := slices.BinarySearch(r.calendar, date)
	switch {
	case !p.PeriodEnd.After(r.lastEnd):
		return nil, fmt.Errorf("targetweight: session %s does not follow the last, %s", p.PeriodEnd, r.lastEnd)
	case !onCalendar:
		return nil, fmt.Errorf("targetweight: session %s is not on the calendar", date)
	case at+1 == len(r.calendar):
		return nil, fmt.Errorf("targetweight: the calendar ends at %s, so whether it is the last session before a rebalance cannot be known", date)
	case !slices.Equal(p.InstrumentIDs, slices.Sorted(maps.Keys(r.bars))):
		return nil, fmt.Errorf("targetweight: session %s names %v, but bars arrived for %v", date, p.InstrumentIDs, slices.Sorted(maps.Keys(r.bars)))
	}
	b := &emissions{h: r.h, in: in, at: p.PeriodEnd}
	if pd := r.pending; pd != nil {
		// The session after the one awaiting its histories closed first.
		r.pending = nil
		prev, _ := slices.BinarySearch(r.calendar, pd.session) // the last session closed is on the calendar
		detail := fmt.Sprintf("a retraction made %s the last session before the rebalance, but %d of the close histories of the %d members ranked from its universe snapshot had arrived when the session closing %s did",
			pd.session, len(pd.histories), len(pd.ranked), p.PeriodEnd.Format(idTime))
		if err := r.withheldAt(b, decisionID(r.h.cfg.AccountID, pd.end), pd.session, pd.end, prev, event.WithheldHistoriesMissing, detail); err != nil {
			return nil, err
		}
	}
	r.raw = make(map[string]float64, len(r.bars))
	r.barred = make(map[string]bool, len(r.bars))
	for id, bar := range r.bars {
		if !bar.PeriodEnd.Equal(p.PeriodEnd) {
			return nil, fmt.Errorf("targetweight: bar %s for %s is not the session %s being closed", bar.PeriodEnd, id, p.PeriodEnd)
		}
		if r.adjusted[id] == nil {
			r.adjusted[id] = map[string]float64{}
		}
		// Scores, volatility and the trend read the split- and
		// dividend-adjusted close (ADR 0026, point 1).
		r.adjusted[id][date] = bar.Adjusted.Close
		r.raw[id] = bar.Raw.Close
		r.barred[id] = true
	}
	r.bars = map[string]event.CompletedBarAdjustedPayload{}
	reason, missing, err := r.mergeHistories(date, at)
	if err != nil {
		return nil, err
	}
	if err := r.missed(b, at); err != nil {
		return nil, err
	}
	r.last, r.lastEnd = date, p.PeriodEnd
	r.trim(at)

	decide, _ := momentum.IsDecisionSession(date, r.calendar[at+1]) // both on the validated calendar
	switch {
	case !decide || p.PeriodEnd.Before(r.h.decideFrom):
		return b.result()
	case missing != "":
		if err := r.withheldAt(b, decisionID(r.h.cfg.AccountID, p.PeriodEnd), date, p.PeriodEnd, at, reason, missing); err != nil {
			return nil, err
		}
		return b.result()
	}
	return r.decide(b, p.PeriodEnd, at, r.universe)
}

// withheldAt journals a withheld decision for calendar session at (session,
// closing at end), found at b's time, with the glide figures on the session
// after it, as a decided one records them; an implausible age there stops
// the run. So no decision time passes without a decision (ADR 0026, point
// 1): a decision session that never closed (missing-bars), one whose
// ranked members sent no close history in the compact universe input
// (histories-missing), and one with a ranked member that sent no bar in the
// bars input (missing-bars), never decided from the bars of the names held
// alone (point 5).
func (r *momentumRun) withheldAt(b *emissions, id, session string, end time.Time, at int, reason, detail string) error {
	p := event.TargetsDecidedPayload{
		QuantityScale: r.exec.QuantityScale(), AccountID: r.h.cfg.AccountID, StrategyID: r.h.cfg.StrategyID,
		DecisionTime: b.at, SessionPeriodEnd: end,
		Outcome: event.TargetsOutcomeWithheld, WithheldReason: reason, Detail: detail,
		Momentum: event.MomentumFigures{CloseSession: session, TrendInstrument: r.rule.TrendInstrument},
	}
	figures, err := r.glideFigures(r.calendar[at+1])
	if err != nil {
		return fmt.Errorf("targetweight: decision for %s: %w", session, err)
	}
	r.applyGlide(&p, momentum.Decision{}, figures)
	b.add(id, event.TargetsDecidedEventType, p.SchemaVersion(), p)
	return nil
}

// missed journals a withheld missing-bars decision for each decision session
// between the last session closed and calendar session at that never closed,
// at this close (b's): ADR 0026 emits one decision per account at each
// decision time (point 1), and a missing final bar withholds it (point 5).
// Before the first session closed, and before the first session to decide,
// nothing is due. Each records the glide figures on its own rebalance
// session, as a decided one does, and an implausible age there stops the run.
func (r *momentumRun) missed(b *emissions, at int) error {
	if r.last == "" {
		return nil
	}
	end := b.at
	from := r.h.decideFrom.UTC().Format(time.DateOnly)
	prev, _ := slices.BinarySearch(r.calendar, r.last) // the last session closed is on the calendar
	for k := prev + 1; k < at; k++ {
		session := r.calendar[k]
		if decide, _ := momentum.IsDecisionSession(session, r.calendar[k+1]); !decide || session < from {
			continue
		}
		detail := fmt.Sprintf("the decision session %s never closed, so no instrument has its final bar; found at the session closing %s",
			session, end.Format(idTime))
		if err := r.withheldAt(b, fmt.Sprintf("targets-decided:%s:missed:%s", r.h.cfg.AccountID, session), session, end, k, event.WithheldMissingBars, detail); err != nil {
			return err
		}
	}
	return nil
}

// trim drops every close older than the window ending at calendar session at.
func (r *momentumRun) trim(at int) {
	if at+1 < r.window {
		return
	}
	cutoff := r.calendar[at+1-r.window]
	for id, closes := range r.adjusted {
		for s := range closes {
			if s < cutoff {
				delete(closes, s)
			}
		}
		if len(closes) == 0 {
			delete(r.adjusted, id)
		}
	}
}

// decide makes the decision at the close of calendar session at, adding to
// b, ranking the universe snapshot u when it is that session's.
func (r *momentumRun) decide(b *emissions, end time.Time, at int, u *event.UniverseSnapshotPayload) ([]event.Envelope, error) {
	h := r.h
	input := momentum.Input{Sessions: r.calendar[max(0, at+1-r.window) : at+1], Closes: r.adjusted, Held: r.held()}
	if u != nil && u.Session == r.last {
		input.Universe = momentum.Snapshot{Session: u.Session, Rows: rows(u.Rows)}
	}
	figures, err := r.glideFigures(r.calendar[at+1])
	if err != nil {
		return nil, fmt.Errorf("targetweight: decision at %s: %w", end.Format(idTime), err)
	}
	lot, lotSet := r.minimumLot(end, figures.BondShare)
	if lotSet {
		input.MinimumLot = &lot
	}
	// Decide cannot refuse what this run has validated: the configuration,
	// a calendar of distinct dates, closes on its sessions, validated rows,
	// and under D7-L a raw close for every scored member, whose window ends
	// with a bar in this session.
	d, err := momentum.Decide(r.rule, input)
	b.fail(err)
	p := r.payload(d, end)
	p.QuantityScale = r.exec.QuantityScale()
	if lotSet && (d.Outcome == momentum.OutcomeDecided || d.Reason == momentum.ReasonAllUnaffordable) {
		// The figures are recorded exactly when D7-L valued the picks: a
		// book the rule decided under it, or one it withheld as
		// all-unaffordable, beside the unaffordable names they value. They
		// stay if the run then withholds a decided book (missing bars, a
		// rebalance in progress). A decision the rule withheld for any other
		// reason, no-eligible-names among them, compared no pick and records
		// none.
		p.Momentum.MinimumLot = event.MinimumLotFigures{NetLiquidation: lot.NetLiquidation, MomentumScale: lot.Scale}
	}
	r.applyGlide(&p, d, figures)

	var plan orderplan.IRAPlan
	if p.Outcome == event.TargetsOutcomeDecided {
		reason, detail := r.withhold(p, end)
		if reason == "" {
			plan, err = orderplan.PlanIRA(orderplan.IRAInput{
				Tag: decisionID(h.cfg.AccountID, end), Decision: p, Snapshot: r.snapshot.AccountPositionsSnapshotPayload,
				QuantityScale: r.exec.QuantityScale(), FractionalIncrement: r.exec.FractionalIncrement,
				ReferencePrices: r.raw, PriceBand: h.cfg.Limits.PriceBand, RebalanceBand: r.cfg.RebalanceBand,
			})
			b.fail(err)
			// Keep the intended weights, including the withheld allocation,
			// so scaling never redistributes its cash (ADR 0026, 2026-10-29).
			for _, buy := range plan.Buys {
				if pick := r.missingMark(buy.InstrumentID, end); pick != nil {
					p.WithheldPicks = append(p.WithheldPicks, *pick)
				}
			}
		} else {
			p.Outcome, p.Targets, p.WithheldReason, p.Detail = event.TargetsOutcomeWithheld, nil, reason, detail
		}
	}
	if p.Outcome == event.TargetsOutcomeDecided {
		p.CashBuffer = orderplan.CashBuffer(r.snapshot.NetLiquidation, r.exec.CashBuffer())
	}
	id := decisionID(h.cfg.AccountID, end)
	b.add(id, event.TargetsDecidedEventType, p.SchemaVersion(), p)
	if p.Outcome != event.TargetsOutcomeDecided || b.err != nil {
		return b.result()
	}
	r.skipped(b, plan.Skipped)
	h.gate.RecordDecision(id)
	reb := &rebalance{
		decisionID: id, decision: p, session: end, snapshot: *r.snapshot, prices: maps.Clone(r.raw),
		buys: plan.Buys, sold: map[string]int64{}, sellsPlanned: len(plan.Sells),
		budget:   orderplan.NewBudget(r.cash(*r.snapshot)),
		withheld: map[string]bool{},
	}
	for _, pick := range p.WithheldPicks {
		reb.withheld[pick.InstrumentID] = true
	}
	r.rebalance = reb
	for _, o := range r.check(b, reb, plan.Sells, 0) {
		reb.sells = append(reb.sells, o.ClientOrderID)
	}
	r.endUnsent(b, reb.sells)
	if r.sellsTerminal(reb) {
		r.planBuys(b)
	}
	return b.result()
}

// held returns the instruments the latest account snapshot holds that sent
// their own bar in the last session of a compact run: a held member whose
// window has a gap is scored across it (momentum.ScoreHeld; ADR 0026 point
// 5, amended 2026-10-18). With no snapshot it holds none, and the decision
// is withheld as stale-account-snapshot anyway.
//
// Invariant: a score across a gap must never span a split or dividend the
// market-data step could not adjust. A holding named to that step refuses
// the stream on one, so a gap in its own bars is a session it did not trade.
// Only the compact stream's bars are the named holdings'; a holding seen
// only through its close history is not known to be named, so its gap
// leaves it out as for a stock not held. A bars stream carries every
// member's bars, named or not, and marks no gap as a plain missing session,
// so a bars run holds none here and keeps the complete-window rule for
// every name (fail closed).
func (r *momentumRun) held() map[string]bool {
	held := map[string]bool{}
	if r.snapshot != nil && r.compact {
		for _, p := range r.snapshot.Positions {
			if p.Quantity != 0 && r.barred[p.InstrumentID] {
				held[p.InstrumentID] = true
			}
		}
	}
	return held
}

// sellsTerminal reports whether every sell the rebalance released is
// terminal, as when it released none.
func (r *momentumRun) sellsTerminal(reb *rebalance) bool {
	for _, id := range reb.sells {
		if s, _ := r.orders.Order(id); !s.Terminal {
			return false
		}
	}
	return true
}

// endUnsent records, in a dry-run only, a synthetic terminal report for each
// of the orders just released, which the run never sent (ADR 0026, point 3,
// "Mode", amended 2026-10-17), and ends each as a venue's cancellation would:
// the gate no longer counts the sell as pending and a buy commits nothing
// more. The report is marked synthetic and names no venue order, so no reader
// can take it for a venue's report or an execution
// (LimitOrderLifecyclePayload.Synthetic).
func (r *momentumRun) endUnsent(b *emissions, released []string) {
	if r.h.cfg.Mode != event.TradingModeDryRun {
		return
	}
	for _, id := range released {
		o, _ := r.orders.Order(id)
		q := o.Released
		p := event.LimitOrderLifecyclePayload{
			AccountID: q.AccountID, ClientOrderID: id, InstrumentID: q.InstrumentID, Side: q.Side,
			Status: event.OrderLifecycleStatusCanceled, Quantity: q.Quantity, QuantityScale: q.QuantityScale, LimitPrice: q.LimitPrice,
			OccurredAt: b.at, Synthetic: true,
			Message: "dry-run: journalled with send false and never sent; synthetic terminal report, nothing executed",
		}
		b.add("synthetic-lifecycle:"+id, event.OrderLifecycleEventType, p.SchemaVersion(), p)
		// The run released this order from these very figures, so neither
		// the shared report rule nor the gate can refuse the report: an
		// error is a defect.
		_, err := r.orders.Lifecycle(p)
		b.fail(err)
		b.fail(r.h.gate.ObserveLifecycle(p))
		if q.Side == event.OrderSideBuy {
			r.rebalance.budget.Close(id)
		}
	}
}

// minimumLot returns what D7-L's substitution values the picks against at the
// close end, with b the decision's bond share (ADR 0026, point 2, "D7-L"):
// the snapshot's net liquidation and this session's raw closes, the basis the
// planner sizes orders from, the momentum sleeve's scale 1 - b, and the
// shares the snapshot holds, so a held pick one lot short is kept (ADR 0026,
// point 2, "D7-L", amended 2026-10-18). With no
// snapshot as of the close (staleSnapshot) the decision is withheld whatever
// the picks, and with b at 1 there is no momentum sleeve, so neither applies
// it.
func (r *momentumRun) minimumLot(end time.Time, b float64) (momentum.MinimumLot, bool) {
	if stale, _ := r.staleSnapshot(end); stale || r.exec.ShareQuantity != event.ShareQuantityMinimumLot {
		return momentum.MinimumLot{}, false
	}
	scale := 1 - b
	if scale <= 0 {
		return momentum.MinimumLot{}, false
	}
	held := map[string]int64{}
	for _, p := range r.snapshot.Positions {
		if p.Quantity > 0 {
			held[p.InstrumentID] = p.Quantity
		}
	}
	return momentum.MinimumLot{NetLiquidation: r.snapshot.NetLiquidation, Scale: scale, Prices: r.raw, Held: held}, true
}

// skipped records each trade the planner sized but could not order.
func (r *momentumRun) skipped(b *emissions, skipped []event.OrderSkippedPayload) {
	for _, s := range skipped {
		b.add("order-skipped:"+s.ClientOrderID, event.OrderSkippedEventType, s.SchemaVersion(), s)
	}
}

// glideFigures counts the owner's age in whole months on the rebalance
// session and sets b from the schedule. It fails only on an age outside the
// plausible range, which stops the run.
func (r *momentumRun) glideFigures(rebalance string) (event.GlideFigures, error) {
	day, _ := time.Parse(time.DateOnly, rebalance) // a validated calendar date
	age, err := glide.AgeMonths(r.h.cfg.Glide.Birth, day)
	if err != nil {
		return event.GlideFigures{}, err
	}
	// The anchors are validated by configuration, so this cannot refuse them.
	b, _ := glide.BondShare(r.anchors, age)
	return event.GlideFigures{RebalanceSession: rebalance, AgeMonths: age, BondShare: b}, nil
}

// applyGlide records the glide figures and, for a decided outcome, replaces
// the rule's targets with the sleeves combined.
func (r *momentumRun) applyGlide(p *event.TargetsDecidedPayload, d momentum.Decision, figures event.GlideFigures) {
	p.Glide = figures
	b := figures.BondShare
	if p.Outcome != event.TargetsOutcomeDecided {
		return
	}
	book := make([]glide.Weight, len(d.Targets))
	for i, t := range d.Targets {
		book[i] = glide.Weight{Symbol: t.InstrumentID, Weight: t.Weight}
	}
	combined, _ := glide.Combine(book, b, r.funds)
	p.Targets = make([]event.TargetWeight, len(combined))
	for i, t := range combined {
		shares := make([]event.SleeveShare, len(t.Shares))
		for k, s := range t.Shares {
			shares[k] = event.SleeveShare{Sleeve: s.Sleeve, Weight: s.Weight}
		}
		p.Targets[i] = event.TargetWeight{InstrumentID: t.Symbol, Weight: t.Weight, Shares: shares}
	}
}

func decisionID(accountID string, end time.Time) string {
	return fmt.Sprintf("targets-decided:%s:%s", accountID, end.UTC().Format(idTime))
}

// staleSnapshot reports whether the account snapshot is not as of the close
// end, and why: missing, earlier, or later. Orders and D7-L's target values
// are valued at that close (ADR 0026, point 2), so either needs a snapshot
// as of it exactly.
func (r *momentumRun) staleSnapshot(end time.Time) (stale bool, detail string) {
	id := r.h.cfg.AccountID
	switch {
	case r.snapshot == nil || r.snapshot.AsOf.Before(end):
		return true, fmt.Sprintf("no account snapshot for %s as of the session closing %s", id, end.Format(idTime))
	case r.snapshot.AsOf.After(end):
		return true, fmt.Sprintf("the account snapshot for %s is as of %s, after the session closing %s; orders are valued at that close",
			id, r.snapshot.AsOf.Format(idTime), end.Format(idTime))
	}
	return false, ""
}

// withhold returns why a decided rebalance cannot be planned, or "".
func (r *momentumRun) withhold(p event.TargetsDecidedPayload, end time.Time) (reason, detail string) {
	if stale, detail := r.staleSnapshot(end); stale {
		return event.WithheldStaleAccountSnapshot, detail
	}
	needed := make([]string, 0, len(p.Targets)+len(r.snapshot.Positions))
	for _, t := range p.Targets {
		needed = append(needed, t.InstrumentID)
	}
	for _, pos := range r.snapshot.Positions {
		needed = append(needed, pos.InstrumentID)
	}
	slices.Sort(needed)
	for _, id := range slices.Compact(needed) {
		if _, ok := r.raw[id]; !ok {
			return event.WithheldMissingBars, fmt.Sprintf("no %s bar in the session closing %s; it is held or targeted", id, end.Format(idTime))
		}
	}
	if c := r.carried; c != nil {
		return event.WithheldRebalanceInProgress, fmt.Sprintf("the rebalance %s, decided at %s, was in progress when the earlier run ended (%s), with orders %s not terminal; a fresh run sends nothing for it until the owner clears it (ADR 0026, point 5)",
			c.DecisionID, c.DecisionTime.Format(idTime), c.Phase, strings.Join(c.ClientOrderIDs, ", "))
	}
	if working := r.orders.Working(); len(working) > 0 {
		return event.WithheldRebalanceInProgress, fmt.Sprintf("order %s of an earlier rebalance is not terminal", working[0])
	}
	return "", ""
}

// applyCleared applies the owner's clearance of the rebalance in progress:
// a rebalance whose orders are not all terminal sends nothing more and
// withholds every later decision until the owner clears it (ADR 0026, point
// 5 and "A new rebalance is withheld while an earlier one's orders are
// working"). The clearance must name that rebalance's decision and exactly
// the orders of it not terminal, which the owner has dealt with at the
// broker; each is then terminal and the rebalance plans nothing more, so its
// buy phase, if still pending, is never planned and the next decision time
// decides afresh. The orders are cleared by the rule the reporter reads the
// log by (event.OrderReports' Clear), so a later venue report on a cleared
// order is refused as on a terminal order by both. The gate's pending sells
// are left to the broker snapshots, which show what each cleared sell sold
// (ADR 0026, point 2.5).
func (r *momentumRun) applyCleared(in event.Envelope) error {
	p, err := decodeInput[event.RebalanceClearedPayload](in, event.RebalanceClearedSchemaVersion)
	if err != nil {
		return err
	}
	if p.AccountID != r.h.cfg.AccountID {
		return fmt.Errorf("targetweight: clearance %s is for account %q; this run decides %q", in.ID, p.AccountID, r.h.cfg.AccountID)
	}
	if c := r.carried; c != nil {
		// The earlier run's rebalance: this run released none of its orders,
		// and decided nothing since, so it is the only rebalance in progress.
		switch {
		case p.DecisionID != c.DecisionID:
			return fmt.Errorf("targetweight: clearance %s names %s, but the rebalance in progress is %s", in.ID, p.DecisionID, c.DecisionID)
		case !slices.Equal(p.ClientOrderIDs, c.ClientOrderIDs):
			return fmt.Errorf("targetweight: clearance %s names orders %v, but the orders not terminal are %v: name exactly those, each dealt with at the broker", in.ID, p.ClientOrderIDs, c.ClientOrderIDs)
		}
		r.carried = nil
		return nil
	}
	if err := r.orders.Clear(p); err != nil {
		return fmt.Errorf("targetweight: clearance %s: %w", in.ID, err)
	}
	for _, id := range p.ClientOrderIDs {
		if o, _ := r.orders.Order(id); o.Released.Side == event.OrderSideBuy {
			r.rebalance.budget.Close(id)
		}
	}
	return nil
}

// applyApproval starts a fresh IRA run from the owner's approval (ADR 0027,
// amended 2026-10-17): the incident in force puts the gate in its Degraded
// or Halted state and scope before any market input (ADR 0019, amended
// 2026-09-24, "The state survives a restart"), and a rebalance in progress
// when the earlier run ended is carried, so this run sends nothing for it
// until the owner clears it (ADR 0026, point 5). The approval must state the
// momentum state: one stating Boost's latch says nothing of a rebalance in
// progress, so it is refused, never read as none. An IRA has no sale back to
// 98%, so an incident's sale is never restored (event's momentumErrors
// refuses one stated attempted). Accepting it records
// strategy.run.approval-accepted, the record a restart reads the carried
// rebalance from.
func (r *momentumRun) applyApproval(in event.Envelope) ([]event.Envelope, error) {
	h := r.h
	p, err := h.decodeApproval(in)
	if err != nil {
		return nil, err
	}
	if p.Momentum == nil {
		return nil, fmt.Errorf("targetweight: approval %s is Boost-shaped (it states Boost's latch and no momentum state), so it says nothing of a rebalance in progress: an IRA run is approved with the rebalance its earlier run ended with in progress, or none (ADR 0026, point 5)", in.ID)
	}
	if incident := p.IncidentInForce; incident != nil {
		if err := h.gate.RestoreIncidentState(incident.IncidentID, incident.State, incident.ScopeInstrumentIDs, incident.ScopeCashComponents); err != nil {
			return nil, fmt.Errorf("targetweight: approval %s: %w", in.ID, err)
		}
	}
	h.approved = true
	r.carried = p.Momentum.RebalanceInProgress
	// The fact a restart reads back: which approval this run accepted and the
	// rebalance it carries (event.RunApprovalAcceptedEventType). Only an
	// accepted approval records one.
	b := &emissions{h: h, in: in, at: in.EventTime}
	b.add("run-approval-accepted:"+in.ID, event.RunApprovalAcceptedEventType, event.RunApprovalAcceptedSchemaVersion, event.RunApprovalAcceptedPayload{
		AccountID: p.AccountID, ApprovalID: in.ID, ApprovalSequence: in.Sequence, RebalanceInProgress: r.carried,
	})
	return b.result()
}

// cash is the cash the D5 rule funds buys from (ADR 0026, point 2, "Budget").
func (r *momentumRun) cash(s event.AccountCashSnapshotPayload) float64 {
	if r.exec.CashRule == event.CashRuleTotal {
		return s.TotalCash
	}
	return s.SettledCash
}

// planBuys plans the rebalance's buy phase: the buys scaled to the budget,
// then each through the gate, committing what each released buy may cost.
// First, cash that a snapshot newer than the decision's shows missing, beyond
// what the sells' recorded fills explain, leaves the budget (ADR 0026, point
// 2, "Budget"; an IRA never borrows), and so does a cash increase pending a
// reading of IBKR's statement that the decision's snapshot's cash could
// hold, one found by a reading at or before the moment that cash is held
// until, which the budget counts only once the Flex reader's records
// explain it (ADR 0019, points 2 and 3, amended 2026-10-21).
func (r *momentumRun) planBuys(b *emissions) {
	reb := r.rebalance
	if short := r.unexplainedCashDrop(reb); short > 0 {
		reb.budget.Remove(short)
	}
	// The book's anchor is the decision ID, not the snapshot's envelope id:
	// PendingCashIncrease reads only the book's AsOf (through HeldUntil), so
	// the anchor is informational here.
	if unexplained := r.h.gate.PendingCashIncrease(reconcile.CashBook(reb.decisionID, reb.snapshot)); unexplained > 0 {
		reb.budget.Remove(unexplained)
	}
	// ADR 0026, owner decision 2026-10-31: reserve only when buys start,
	// after proceeds and missing cash. Early clamping would lose the reserve
	// shortfall when starting cash is below it. Sells never wait on this rule.
	buffer := orderplan.CashBuffer(reb.snapshot.NetLiquidation, r.exec.CashBuffer())
	reb.budget.WithholdCashBuffer(buffer)
	buys := slices.Clone(reb.buys)
	for i := range buys {
		buys[i].RoundingSlack = r.exec.RoundingSlack()
		if _, err := event.BuyReservation(buys[i].Quantity, buys[i].QuantityScale, buys[i].LimitPrice, r.exec.MaxFeesPerOrder, buys[i].RoundingSlack); err != nil {
			b.err = fmt.Errorf("targetweight: buy %s refused: %w", buys[i].ClientOrderID, err)
			return
		}
	}
	buys, skipped, _ := orderplan.ScaleBuys(buys, reb.budget.Remaining(), r.exec.MaxFeesPerOrder)
	r.skipped(b, skipped)
	var released []string
	for _, o := range r.check(b, reb, buys, reb.sellsPlanned) {
		reb.budget.Release(o.ClientOrderID, orderplan.BuyCost(o, r.exec.MaxFeesPerOrder))
		released = append(released, o.ClientOrderID)
	}
	r.endUnsent(b, released)
}

// unexplainedCashDrop is how far the latest snapshot's cash under the D5 rule
// falls short of the cash the decision's snapshot and the sells' fills since
// explain, or 0 when no newer snapshot arrived. Under total-cash a fill at or
// before the newer snapshot adds its proceeds, net of commission; settled
// cash counts no proceeds, which settle later. A rise is never added: the
// budget counts only cash it can explain.
func (r *momentumRun) unexplainedCashDrop(reb *rebalance) float64 {
	latest := r.snapshot
	if !latest.AsOf.After(reb.snapshot.AsOf) {
		return 0
	}
	expected := r.cash(reb.snapshot)
	if r.exec.CashRule == event.CashRuleTotal {
		for _, id := range reb.sells {
			o, _ := r.orders.Order(id)
			for _, fillID := range slices.Sorted(maps.Keys(o.Fills)) {
				if f := o.Fills[fillID]; !f.FilledAt.After(latest.AsOf) {
					expected += event.SaleProceeds(f.Quantity, f.QuantityScale, f.Price, f.Commission)
				}
			}
		}
	}
	return max(0, expected-r.cash(*latest))
}

// check emits each order and the gate's outcome for it, and returns the
// orders released, now tracked. The prior orders are the sell phase's, each
// sized within its holding (orderplan.PlanIRA), so every one is risk-reducing
// and none counts toward the order limit (ADR 0026, point 3): the book's
// PriorCounted stays zero. A mark must follow the latest unavailable
// observation; the original rebalance withholding remains in force
// (ADR 0026, amendment of 2026-10-29).
func (r *momentumRun) check(b *emissions, reb *rebalance, orders []event.OrderPlannedPayload, prior int) []event.OrderPlannedPayload {
	h := r.h
	// Preserve the original decision and order tags. Later missing marks
	// append evidence for that same session before any refusal (ADR 0026,
	// amendment of 2026-10-29); its unbought weights still stay in cash.
	p := reb.decision
	p.WithheldPicks = slices.Clone(p.WithheldPicks)
	for _, o := range orders {
		if o.Side == event.OrderSideBuy && !reb.withheld[o.InstrumentID] {
			if pick := r.missingMark(o.InstrumentID, reb.session); pick != nil {
				p.WithheldPicks = append(p.WithheldPicks, *pick)
				reb.withheld[o.InstrumentID] = true
			}
		}
	}
	if len(p.WithheldPicks) > len(reb.decision.WithheldPicks) {
		slices.SortFunc(p.WithheldPicks, func(a, b event.WithheldPick) int {
			return strings.Compare(a.InstrumentID, b.InstrumentID)
		})
		b.add(reb.decisionID+":withheld-picks", event.TargetsDecidedEventType, p.SchemaVersion(), p)
	}
	holdings := holdings(reb.snapshot.Positions)
	for id, sold := range reb.sold {
		holdings[id] -= sold
	}
	book := risklimit.Book{QuantityScale: r.exec.QuantityScale(), Holdings: holdings, Prices: reb.prices, PriorOrders: prior, CashAvailable: reb.budget.Remaining(), At: b.at}
	plan := make([]risklimit.Order, len(orders))
	for i, o := range orders {
		plan[i] = risklimit.Order{PlanID: "order-planned:" + o.ClientOrderID, Planned: o}
		failure, failed := r.unavailable[o.InstrumentID]
		if mark, ok := r.marks[o.InstrumentID]; ok && !mark.AsOf.Before(reb.session) &&
			(!failed || mark.AsOf.After(failure.AsOf)) && !reb.withheld[o.InstrumentID] {
			plan[i].MarkSet = true
			plan[i].Mark, plan[i].MarkSource = bandMark(mark, r.markAt[o.InstrumentID])
		}
		b.add(plan[i].PlanID, event.OrderPlannedEventType, o.SchemaVersion(), o)
	}
	// The gate cannot refuse orders the planner built from validated inputs
	// for this account, so an error is a defect.
	outcomes, err := h.gate.Check(book, plan)
	b.fail(err)
	var released []event.OrderPlannedPayload
	for i, out := range outcomes {
		o := orders[i]
		if out.Released != nil {
			b.add("risk-order-released:"+o.ClientOrderID, event.OrderReleasedEventType, out.Released.SchemaVersion(), *out.Released)
			r.orders.Release(*out.Released)
			released = append(released, o)
		} else {
			b.add("risk-order-refused:"+o.ClientOrderID, event.OrderRefusedEventType, out.Refused.SchemaVersion(), *out.Refused)
		}
		if out.State != nil {
			b.add(out.StateID, event.EngineStateEventType, out.StateSchemaVersion, *out.State)
		}
	}
	return released
}

// refusal is a venue report the rule refuses (event.OrderReports), naming
// the report.
func refusal(in event.Envelope, err error) error {
	return fmt.Errorf("targetweight: %s %s %w", in.Type, in.ID, err)
}

// forAccount refuses a venue report for another account than this run's.
func (r *momentumRun) forAccount(in event.Envelope, account string) error {
	if account != r.h.cfg.AccountID {
		return fmt.Errorf("targetweight: %s %s is for account %q; this run decides %q", in.Type, in.ID, account, r.h.cfg.AccountID)
	}
	return nil
}

func (r *momentumRun) applyLifecycle(in event.Envelope) ([]event.Envelope, error) {
	p, err := decodeVersions[event.LimitOrderLifecyclePayload](in, []uint32{event.OrderLifecycleLimitVersion, event.OrderLifecycleFractionalVersion})
	if err != nil {
		return nil, err
	}
	if err := r.forAccount(in, p.AccountID); err != nil {
		return nil, err
	}
	if p.QuantityScale != r.exec.QuantityScale() {
		return nil, fmt.Errorf("targetweight: report quantity scale %d does not match account scale %d", p.QuantityScale, r.exec.QuantityScale())
	}
	o, err := r.orders.Lifecycle(p)
	if err != nil {
		return nil, refusal(in, err)
	}
	// The gate nets every pending sell from later sizing (ADR 0026, point
	// 2.5); it reads a report the rule above has read.
	if err := r.h.gate.ObserveLifecycle(p); err != nil {
		return nil, fmt.Errorf("targetweight: lifecycle %s: %w", in.ID, err)
	}
	if o.Terminal {
		return r.terminal(in, o, p.OccurredAt)
	}
	return nil, nil
}

// applyRetired reads the retirement of an order the adapter never sent
// (execution.order.retired): its session ended before the adapter could
// send it, or the owner retired an order the adapter can never send (the
// owner's decision of 2026-10-22; ADR 0026, point 2, "Boost", amended
// 2026-10-22, read alike here). By the shared report rule
// (event.OrderReports' Retire) it must name an order this run released, as
// released, that no venue has reported on and that is not terminal; it then
// ends the order with nothing filled, as a venue's cancellation of an
// unfilled order would: the gate no longer counts a sell as pending, a buy
// commits nothing more, a retired sell means the rebalance's buy phase is
// never started, and once the rebalance's orders are all terminal the next
// decision decides afresh (ADR 0026, point 5, amended 2026-10-22). A dry-run sends
// nothing and ends each order itself, so it refuses every retirement.
func (r *momentumRun) applyRetired(in event.Envelope) ([]event.Envelope, error) {
	p, err := decodeVersions[event.OrderRetiredPayload](in, event.OrderRetiredVersions())
	if err != nil {
		return nil, err
	}
	if r.h.cfg.Mode == event.TradingModeDryRun {
		return nil, fmt.Errorf("targetweight: retirement %s names order %s in a dry-run, which sends no order and ends each it releases with its own synthetic cancel, so it has nothing to retire: find what fed it before going on", in.ID, p.ClientOrderID)
	}
	if err := r.forAccount(in, p.AccountID); err != nil {
		return nil, err
	}
	if p.QuantityScale != r.exec.QuantityScale() {
		return nil, fmt.Errorf("targetweight: report quantity scale %d does not match account scale %d", p.QuantityScale, r.exec.QuantityScale())
	}
	o, err := r.orders.Retire(p)
	if err != nil {
		return nil, refusal(in, err)
	}
	// The gate ends the order as a cancellation with nothing filled would,
	// by a report the engine never journals: marked synthetic and naming no
	// venue order, so the gate cannot take it for a venue's.
	q := o.Released
	end := event.LimitOrderLifecyclePayload{
		AccountID: q.AccountID, ClientOrderID: q.ClientOrderID, InstrumentID: q.InstrumentID, Side: q.Side,
		Status: event.OrderLifecycleStatusCanceled, Quantity: q.Quantity, QuantityScale: q.QuantityScale, LimitPrice: q.LimitPrice,
		OccurredAt: p.RetiredAt, Synthetic: true, Message: "retired unsent: " + p.Reason,
	}
	if err := r.h.gate.ObserveLifecycle(end); err != nil {
		return nil, fmt.Errorf("targetweight: retirement %s: %w", in.ID, err)
	}
	if q.Side == event.OrderSideSell && r.rebalance != nil {
		r.rebalance.sellRetired = true
	}
	return r.terminal(in, o, p.RetiredAt)
}

func (r *momentumRun) applyFill(in event.Envelope) ([]event.Envelope, error) {
	p, err := decodeVersions[event.TargetWeightFillPayload](in, []uint32{event.FillTargetWeightVersion, event.FillTargetWeightFractionalVersion})
	if err != nil {
		return nil, err
	}
	if err := r.forAccount(in, p.AccountID); err != nil {
		return nil, err
	}
	if p.QuantityScale != r.exec.QuantityScale() {
		return nil, fmt.Errorf("targetweight: report quantity scale %d does not match account scale %d", p.QuantityScale, r.exec.QuantityScale())
	}
	o, redelivery, err := r.orders.Fill(p)
	if err != nil {
		return nil, refusal(in, err)
	}
	if redelivery {
		return nil, nil
	}
	// The gate nets every pending sell from later sizing (ADR 0026, point
	// 2.5); it reads a fill the rule above has read.
	if err := r.h.gate.ObserveFill(p); err != nil {
		return nil, fmt.Errorf("targetweight: fill %s: %w", in.ID, err)
	}
	reb := r.rebalance
	if p.Side == event.OrderSideSell {
		reb.sold[p.InstrumentID] += p.Quantity
		if r.exec.CashRule == event.CashRuleTotal {
			reb.budget.AddProceeds(event.SaleProceeds(p.Quantity, p.QuantityScale, p.Price, p.Commission))
		}
	} else {
		// OrderReports.Fill already accepted this debit under the shared
		// live and recovery rule (ADR 0027); BuyCost cannot fail here.
		cost, _ := event.BuyCost(p.Quantity, p.QuantityScale, p.Price, p.Commission)
		fillCost, _ := cost.Float64()
		reb.budget.Fill(p.ClientOrderID, fillCost)
	}
	if o.Terminal {
		return r.terminal(in, o, p.FilledAt)
	}
	return nil, nil
}

// terminal follows a report that made o terminal: when it was the last
// working sell of the rebalance in progress, it plans the buy phase, caused
// by this report, unless a sell of the rebalance was retired as never sent.
//
// Every order not yet terminal belongs to the rebalance in progress: a new
// rebalance is planned only once every order is terminal (withhold). And the
// buys are planned only once every sell is terminal, so no sell becomes
// terminal after them.
func (r *momentumRun) terminal(in event.Envelope, o *event.ReportedOrder, at time.Time) ([]event.Envelope, error) {
	reb := r.rebalance
	if o.Released.Side == event.OrderSideBuy {
		reb.budget.Close(o.Released.ClientOrderID)
		return nil, nil
	}
	if reb.sellRetired || !r.sellsTerminal(reb) {
		return nil, nil
	}
	b := &emissions{h: r.h, in: in, at: at}
	r.planBuys(b)
	return b.result()
}

// payload maps a momentum decision to strategy.targets.decided.
func (r *momentumRun) payload(d momentum.Decision, end time.Time) event.TargetsDecidedPayload {
	p := event.TargetsDecidedPayload{
		QuantityScale: r.exec.QuantityScale(), AccountID: r.h.cfg.AccountID, StrategyID: r.h.cfg.StrategyID,
		DecisionTime: end, SessionPeriodEnd: end, Outcome: string(d.Outcome),
		Momentum: event.MomentumFigures{
			CloseSession: d.CloseSession, TrendInstrument: r.rule.TrendInstrument,
			TrendCloses: d.Trend.Closes, TrendClose: d.Trend.Close, TrendMean: d.Trend.Mean, RiskOff: d.Trend.RiskOff,
			UniverseRows: d.Universe.Rows, UniverseListingType: d.Universe.CommonStock,
			UniverseAtOrAboveFloor: d.Universe.AtOrAboveFloor, UniverseMembers: d.Universe.Members,
		},
	}
	p.Detail = d.Detail
	if d.Outcome == momentum.OutcomeWithheld {
		p.WithheldReason = string(d.Reason)
		if p.Detail == "" {
			p.Detail = withheldDetail(d.Reason, r.rule, d.CloseSession)
		}
	}
	for _, n := range d.Names {
		p.Momentum.Names = append(p.Momentum.Names, event.MomentumName{
			InstrumentID: n.InstrumentID, UniverseRank: n.UniverseRank, DollarVolume: n.DollarVolume,
			Sector: n.Sector, SectorSource: n.SectorSource, Status: string(n.Status), Closes: n.Closes,
			Score: n.Score, Volatility: n.Volatility, ScoreRank: n.ScoreRank, Weight: n.Weight,
			BacktestWouldPick: n.BacktestWouldPick, OneShare: n.OneShare, TargetValue: n.TargetValue,
			HeldQuantity: n.HeldQuantity, HeldValue: n.HeldValue, HeldShare: n.HeldShare,
			GapSessions: n.GapSessions,
		})
	}
	for _, t := range d.Targets {
		p.Targets = append(p.Targets, event.TargetWeight{InstrumentID: t.InstrumentID, Weight: t.Weight})
	}
	sort.Slice(p.Targets, func(i, j int) bool { return p.Targets[i].InstrumentID < p.Targets[j].InstrumentID })
	return p
}

// withheldDetail explains each of the rule's own withheld reasons.
func withheldDetail(reason momentum.WithheldReason, rule momentum.Config, session string) string {
	switch reason {
	case momentum.ReasonInsufficientTrendHistory:
		return fmt.Sprintf("%s has fewer than %d closes to %s", rule.TrendInstrument, rule.TrendCloses, session)
	case momentum.ReasonMissingBars:
		return fmt.Sprintf("%s has no close on a session of its last %d to %s", rule.TrendInstrument, rule.TrendCloses, session)
	case momentum.ReasonMissingUniverse:
		return fmt.Sprintf("no universe snapshot with a member for the session %s", session)
	}
	return fmt.Sprintf("no universe member could be picked on %s", session)
}

// rows maps universe snapshot rows to the rule's rows.
func rows(in []event.UniverseRow) []momentum.Row {
	out := make([]momentum.Row, len(in))
	for i, r := range in {
		out[i] = momentum.Row{
			InstrumentID: r.InstrumentID, Type: r.Type, Close: r.Close, Volume: r.Volume,
			Sector: r.Sector, SectorSource: r.SectorSource,
		}
	}
	return out
}

// normalizeQuantities upcasts legacy whole-share inputs to D7-F's exact ticks
// (ADR 0015; ADR 0026 D7-F). A whole-share account refuses fractional schemas;
// overflow or a negative fractional holding refuses the entire conversion.
func (h *Handler) normalizeQuantities(scale *int64, quantities ...*int64) error {
	target := h.cfg.Execution.QuantityScale()
	if *scale == target {
		return nil
	}
	if target != event.FractionalQuantityScale || *scale != 0 {
		return fmt.Errorf("targetweight: quantity scale %d does not match account quantity scale %d", *scale, target)
	}
	for _, q := range quantities {
		if *q < 0 || *q > math.MaxInt64/event.FractionalQuantityScale {
			return fmt.Errorf("targetweight: whole-share quantity %d cannot be represented as nonnegative four-place shares", *q)
		}
	}
	for _, q := range quantities {
		*q *= event.FractionalQuantityScale
	}
	*scale = target
	return nil
}

func (h *Handler) normalizePositions(p *event.AccountPositionsSnapshotPayload) error {
	qs := make([]*int64, len(p.Positions))
	for i := range p.Positions {
		qs[i] = &p.Positions[i].Quantity
	}
	return h.normalizeQuantities(&p.QuantityScale, qs...)
}

func (h *Handler) normalizeBroker(p *event.BrokerSnapshotPayload) error {
	qs := make([]*int64, 0, len(p.Positions)+len(p.OpenOrders))
	for i := range p.Positions {
		qs = append(qs, &p.Positions[i].Quantity)
	}
	for i := range p.OpenOrders {
		qs = append(qs, &p.OpenOrders[i].RemainingQuantity)
	}
	return h.normalizeQuantities(&p.QuantityScale, qs...)
}

func (h *Handler) normalizeReconciliation(p *event.ReconciliationPayload) error {
	qs := make([]*int64, len(p.Positions))
	for i := range p.Positions {
		qs[i] = &p.Positions[i].Quantity
	}
	return h.normalizeQuantities(&p.QuantityScale, qs...)
}
