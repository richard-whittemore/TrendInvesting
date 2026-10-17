package targetweight

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
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
//     trend instrument, the risk-off instrument, universe members and
//     holdings; each instrument's bars on one adjusted base), then
//     market.session.closed naming exactly those instruments;
//   - market.universe.snapshot, from the configured source, for the session
//     it ranks;
//   - account.snapshot version 4 and market.mark.recorded per instrument;
//   - execution.order.lifecycle version 2 and execution.fill version 5 for the
//     orders this run released.
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

	adjusted map[string]map[string]float64 // instrument -> session -> adjusted close
	raw      map[string]float64            // instrument -> raw close in the last session
	bars     map[string]event.CompletedBarAdjustedPayload
	bases    map[string]string // instrument -> its adjusted view's base
	universe *event.UniverseSnapshotPayload
	snapshot *event.AccountCashSnapshotPayload
	marks    map[string]event.MarkRecordedPayload

	orders    map[string]*trackedOrder
	fills     map[string]event.TargetWeightFillPayload // every accepted fill, by fill id
	rebalance *rebalance
}

// trackedOrder is one released order and what the venue has reported of it.
type trackedOrder struct {
	planned  event.OrderPlannedPayload
	filled   int64
	terminal bool
	fills    map[string]event.TargetWeightFillPayload
}

// rebalance is the decision time in progress: what its buy phase needs.
type rebalance struct {
	decisionID   string
	session      time.Time
	snapshot     event.AccountCashSnapshotPayload
	prices       map[string]float64
	buys         []event.OrderPlannedPayload
	sells        []string // client order IDs of released sells
	sold         map[string]int64
	sellsPlanned int
	budget       *orderplan.Budget
}

func newMomentum(strategyVersion string, cfg event.TargetWeightConfigurationPayload, opts Options) (*Handler, error) {
	m := cfg.Momentum
	rule := momentum.Config{
		ListingType: m.ListingType, UniverseSize: m.UniverseSize, MinPrice: m.MinPrice,
		ScoreLookback: m.ScoreLookback, ScoreSkip: m.ScoreSkip, VolatilityReturns: m.VolatilityReturns,
		TopN: m.TopN, SectorCap: m.SectorCap, Invested: m.Invested,
		TrendInstrument: m.TrendInstrument, TrendCloses: m.TrendCloses,
		RiskOffInstrument: m.RiskOffInstrument, RiskOffFraction: m.RiskOffFraction,
	}
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
	gate, err := risklimit.NewIRAGate(cfg.AccountID, cfg.Limits, cfg.Mode, cfg.Execution.MaxFeesPerOrder)
	if err != nil {
		return nil, fmt.Errorf("targetweight: %w", err)
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
		window:  max(m.ScoreLookback+1, m.TrendCloses),
		anchors: anchors, funds: funds,
		adjusted: map[string]map[string]float64{},
		bars:     map[string]event.CompletedBarAdjustedPayload{},
		bases:    map[string]string{},
		marks:    map[string]event.MarkRecordedPayload{},
		orders:   map[string]*trackedOrder{},
		fills:    map[string]event.TargetWeightFillPayload{},
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
	case event.AccountSnapshotEventType:
		return nil, r.applySnapshot(in)
	case event.MarkRecordedEventType:
		return nil, r.applyMark(in)
	case event.SessionClosedEventType:
		r.h.started = true
		return r.applySession(in)
	case event.OrderLifecycleEventType:
		return r.applyLifecycle(in)
	case event.FillEventType:
		return r.applyFill(in)
	case event.RunCompletedEventType:
		return nil, nil
	}
	return nil, fmt.Errorf("targetweight: input type %q is not one a momentum run reads", in.Type)
}

// applyCalendar merges a calendar into the one held. The two must share a
// session, and over the dates both cover they must list the same sessions: a
// calendar that drops or adds a session the run already knows is a
// contradiction, never a correction.
func (r *momentumRun) applyCalendar(in event.Envelope) error {
	p, err := decodeInput[event.SessionCalendarPayload](in, event.SessionCalendarSchemaVersion)
	if err != nil {
		return err
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
	if p.Source != r.cfg.UniverseSource {
		return fmt.Errorf("targetweight: universe %s is from %q; this run reads %q", in.ID, p.Source, r.cfg.UniverseSource)
	}
	for _, row := range p.Rows {
		if row.SectorSource != "" && !slices.Contains(r.cfg.SectorSources, row.SectorSource) {
			return fmt.Errorf("targetweight: universe %s labels %s from %q, not a configured sector source", in.ID, row.InstrumentID, row.SectorSource)
		}
	}
	if r.universe != nil && p.Session <= r.universe.Session {
		return fmt.Errorf("targetweight: universe for %s does not follow the last, %s", p.Session, r.universe.Session)
	}
	r.universe = &p
	return nil
}

func (r *momentumRun) applySnapshot(in event.Envelope) error {
	p, err := decodeInput[event.AccountCashSnapshotPayload](in, event.AccountSnapshotCashVersion)
	if err != nil {
		return err
	}
	if p.AccountID != r.h.cfg.AccountID {
		return fmt.Errorf("targetweight: snapshot for account %q; this run decides %q", p.AccountID, r.h.cfg.AccountID)
	}
	if r.snapshot != nil && !p.AsOf.After(r.snapshot.AsOf) {
		return fmt.Errorf("targetweight: snapshot as of %s does not follow the last, %s", p.AsOf, r.snapshot.AsOf)
	}
	r.snapshot = &p
	return nil
}

func (r *momentumRun) applyMark(in event.Envelope) error {
	p, err := decodeInput[event.MarkRecordedPayload](in, event.MarkRecordedSchemaVersion)
	if err != nil {
		return err
	}
	if last, ok := r.marks[p.InstrumentID]; ok && !p.AsOf.After(last.AsOf) {
		return fmt.Errorf("targetweight: mark for %s as of %s does not follow the last, %s", p.InstrumentID, p.AsOf, last.AsOf)
	}
	r.marks[p.InstrumentID] = p
	return nil
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
	r.raw = make(map[string]float64, len(r.bars))
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
	}
	r.bars = map[string]event.CompletedBarAdjustedPayload{}
	b := &emissions{h: r.h, in: in, at: p.PeriodEnd}
	if err := r.missed(b, at); err != nil {
		return nil, err
	}
	r.last, r.lastEnd = date, p.PeriodEnd
	r.trim(at)

	decide, _ := momentum.IsDecisionSession(date, r.calendar[at+1]) // both on the validated calendar
	if !decide || p.PeriodEnd.Before(r.h.decideFrom) {
		return b.result()
	}
	return r.decide(b, p.PeriodEnd, at)
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
		p := event.TargetsDecidedPayload{
			AccountID: r.h.cfg.AccountID, StrategyID: r.h.cfg.StrategyID,
			DecisionTime: end, SessionPeriodEnd: end,
			Outcome: event.TargetsOutcomeWithheld, WithheldReason: event.WithheldMissingBars,
			Detail: fmt.Sprintf("the decision session %s never closed, so no instrument has its final bar; found at the session closing %s",
				session, end.Format(idTime)),
			Momentum: event.MomentumFigures{CloseSession: session, TrendInstrument: r.rule.TrendInstrument},
		}
		if err := r.applyGlide(&p, momentum.Decision{}, r.calendar[k+1]); err != nil {
			return fmt.Errorf("targetweight: decision for %s: %w", session, err)
		}
		b.add(fmt.Sprintf("targets-decided:%s:missed:%s", r.h.cfg.AccountID, session), event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, p)
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

// decide makes the decision at the close of calendar session at, adding to b.
func (r *momentumRun) decide(b *emissions, end time.Time, at int) ([]event.Envelope, error) {
	h := r.h
	input := momentum.Input{Sessions: r.calendar[max(0, at+1-r.window) : at+1], Closes: r.adjusted}
	if r.universe != nil && r.universe.Session == r.last {
		input.Universe = momentum.Snapshot{Session: r.universe.Session, Rows: rows(r.universe.Rows)}
	}
	// Decide cannot refuse what this run has validated: the configuration,
	// a calendar of distinct dates, closes on its sessions, validated rows.
	d, err := momentum.Decide(r.rule, input)
	b.fail(err)
	p := r.payload(d, end)
	if err := r.applyGlide(&p, d, r.calendar[at+1]); err != nil {
		return nil, fmt.Errorf("targetweight: decision at %s: %w", end.Format(idTime), err)
	}

	var plan orderplan.IRAPlan
	if p.Outcome == event.TargetsOutcomeDecided {
		reason, detail := r.withhold(p, end)
		if reason == "" {
			plan, err = orderplan.PlanIRA(orderplan.IRAInput{
				Tag: decisionID(h.cfg.AccountID, end), Decision: p, Snapshot: r.snapshot.AccountPositionsSnapshotPayload,
				ReferencePrices: r.raw, PriceBand: h.cfg.Limits.PriceBand, RebalanceBand: r.cfg.RebalanceBand,
			})
			b.fail(err)
		} else {
			p.Outcome, p.Targets, p.WithheldReason, p.Detail = event.TargetsOutcomeWithheld, nil, reason, detail
		}
	}
	id := decisionID(h.cfg.AccountID, end)
	b.add(id, event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, p)
	if p.Outcome != event.TargetsOutcomeDecided || b.err != nil {
		return b.result()
	}
	h.gate.RecordDecision(id)
	reb := &rebalance{
		decisionID: id, session: end, snapshot: *r.snapshot, prices: maps.Clone(r.raw),
		buys: plan.Buys, sold: map[string]int64{}, sellsPlanned: len(plan.Sells),
		budget: orderplan.NewBudget(r.cash(*r.snapshot)),
	}
	r.rebalance = reb
	for _, o := range r.check(b, reb, plan.Sells, 0) {
		reb.sells = append(reb.sells, o.ClientOrderID)
	}
	if len(reb.sells) == 0 {
		r.planBuys(b)
	}
	return b.result()
}

// applyGlide records the owner's age and b on the rebalance session and, for
// a decided outcome, replaces the rule's targets with the sleeves combined.
// It fails only on an age outside the plausible range, which stops the run.
func (r *momentumRun) applyGlide(p *event.TargetsDecidedPayload, d momentum.Decision, rebalance string) error {
	day, _ := time.Parse(time.DateOnly, rebalance) // a validated calendar date
	age, err := glide.AgeMonths(r.h.cfg.Glide.Birth, day)
	if err != nil {
		return err
	}
	// The anchors, the book and the funds are validated by configuration
	// and the rule, so neither call can refuse them.
	b, _ := glide.BondShare(r.anchors, age)
	p.Glide = event.GlideFigures{RebalanceSession: rebalance, AgeMonths: age, BondShare: b}
	if p.Outcome != event.TargetsOutcomeDecided {
		return nil
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
	return nil
}

func decisionID(accountID string, end time.Time) string {
	return fmt.Sprintf("targets-decided:%s:%s", accountID, end.UTC().Format(idTime))
}

// withhold returns why a decided rebalance cannot be planned, or "".
func (r *momentumRun) withhold(p event.TargetsDecidedPayload, end time.Time) (reason, detail string) {
	h := r.h
	switch {
	case r.snapshot == nil || r.snapshot.AsOf.Before(end):
		return event.WithheldStaleAccountSnapshot, fmt.Sprintf("no account snapshot for %s as of the session closing %s", h.cfg.AccountID, end.Format(idTime))
	case r.snapshot.AsOf.After(end):
		// Orders are valued at the session's close (ADR 0026, point 2).
		return event.WithheldStaleAccountSnapshot, fmt.Sprintf("the account snapshot for %s is as of %s, after the session closing %s; orders are valued at that close",
			h.cfg.AccountID, r.snapshot.AsOf.Format(idTime), end.Format(idTime))
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
	for _, id := range slices.Sorted(maps.Keys(r.orders)) {
		if !r.orders[id].terminal {
			return event.WithheldRebalanceInProgress, fmt.Sprintf("order %s of an earlier rebalance is not terminal", id)
		}
	}
	return "", ""
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
// 2, "Budget"; an IRA never borrows).
func (r *momentumRun) planBuys(b *emissions) {
	reb := r.rebalance
	if short := r.unexplainedCashDrop(reb); short > 0 {
		reb.budget.Remove(short)
	}
	buys, _ := orderplan.ScaleBuys(reb.buys, reb.budget.Remaining(), r.exec.MaxFeesPerOrder)
	for _, o := range r.check(b, reb, buys, reb.sellsPlanned) {
		reb.budget.Release(o.ClientOrderID, orderplan.BuyCost(o, r.exec.MaxFeesPerOrder))
	}
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
			o := r.orders[id]
			for _, fillID := range slices.Sorted(maps.Keys(o.fills)) {
				if f := o.fills[fillID]; !f.FilledAt.After(latest.AsOf) {
					expected += float64(float64(f.Quantity)*f.Price) - f.Commission
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
// PriorCounted stays zero.
func (r *momentumRun) check(b *emissions, reb *rebalance, orders []event.OrderPlannedPayload, prior int) []event.OrderPlannedPayload {
	h := r.h
	holdings := holdings(reb.snapshot.Positions)
	for id, sold := range reb.sold {
		holdings[id] -= sold
	}
	book := risklimit.Book{Holdings: holdings, Prices: reb.prices, PriorOrders: prior, CashAvailable: reb.budget.Remaining()}
	plan := make([]risklimit.Order, len(orders))
	for i, o := range orders {
		plan[i] = risklimit.Order{PlanID: "order-planned:" + o.ClientOrderID, Planned: o}
		if mark, ok := r.marks[o.InstrumentID]; ok && !mark.AsOf.Before(reb.session) {
			plan[i].Mark, plan[i].MarkSet = mark.Price, true
		}
		b.add(plan[i].PlanID, event.OrderPlannedEventType, event.OrderPlannedSchemaVersion, o)
	}
	// The gate cannot refuse orders the planner built from validated inputs
	// for this account, so an error is a defect.
	outcomes, err := h.gate.Check(book, plan)
	b.fail(err)
	var released []event.OrderPlannedPayload
	for i, out := range outcomes {
		o := orders[i]
		if out.Released != nil {
			b.add("risk-order-released:"+o.ClientOrderID, event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, *out.Released)
			r.orders[o.ClientOrderID] = &trackedOrder{planned: o, fills: map[string]event.TargetWeightFillPayload{}}
			released = append(released, o)
		} else {
			b.add("risk-order-refused:"+o.ClientOrderID, event.OrderRefusedEventType, event.OrderRefusedSchemaVersion, *out.Refused)
		}
		if out.State != nil {
			b.add("engine-state:"+o.ClientOrderID, event.EngineStateEventType, out.StateSchemaVersion, *out.State)
		}
	}
	return released
}

// tracked returns the released order a venue report names, checking that the
// report describes that order.
func (r *momentumRun) tracked(in event.Envelope, account, client, instrument, side string) (*trackedOrder, error) {
	o, ok := r.orders[client]
	switch {
	case account != r.h.cfg.AccountID:
		return nil, fmt.Errorf("targetweight: %s %s is for account %q; this run decides %q", in.Type, in.ID, account, r.h.cfg.AccountID)
	case !ok:
		return nil, fmt.Errorf("targetweight: %s %s names order %s, which this run never released", in.Type, in.ID, client)
	case o.planned.InstrumentID != instrument || o.planned.Side != side:
		return nil, fmt.Errorf("targetweight: %s %s describes %s %s, but order %s is %s %s", in.Type, in.ID, side, instrument, client, o.planned.Side, o.planned.InstrumentID)
	case o.terminal:
		return nil, fmt.Errorf("targetweight: %s %s reports on order %s, which is already terminal", in.Type, in.ID, client)
	}
	return o, nil
}

func (r *momentumRun) applyLifecycle(in event.Envelope) ([]event.Envelope, error) {
	p, err := decodeInput[event.LimitOrderLifecyclePayload](in, event.OrderLifecycleLimitVersion)
	if err != nil {
		return nil, err
	}
	o, err := r.tracked(in, p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side)
	if err != nil {
		return nil, err
	}
	if p.Quantity != o.planned.Quantity || p.LimitPrice != o.planned.LimitPrice {
		return nil, fmt.Errorf("targetweight: lifecycle %s reports %d at %v; order %s is %d at %v", in.ID, p.Quantity, p.LimitPrice, p.ClientOrderID, o.planned.Quantity, o.planned.LimitPrice)
	}
	if p.Terminal() {
		return r.terminal(in, o, p.OccurredAt)
	}
	return nil, nil
}

func (r *momentumRun) applyFill(in event.Envelope) ([]event.Envelope, error) {
	p, err := decodeInput[event.TargetWeightFillPayload](in, event.FillTargetWeightVersion)
	if err != nil {
		return nil, err
	}
	// A fill id names one execution across the account
	// (TargetWeightFillPayload), so it is checked across every order.
	if seen, dup := r.fills[p.FillID]; dup {
		if seen.SameExecution(p) {
			return nil, nil // a redelivery of the same execution
		}
		return nil, fmt.Errorf("targetweight: fill %s reuses fill id %s for a different execution", in.ID, p.FillID)
	}
	o, err := r.tracked(in, p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side)
	if err != nil {
		return nil, err
	}
	if o.filled+p.Quantity > o.planned.Quantity {
		return nil, fmt.Errorf("targetweight: fill %s takes order %s past its %d shares", in.ID, p.ClientOrderID, o.planned.Quantity)
	}
	r.fills[p.FillID] = p
	o.fills[p.FillID] = p
	o.filled += p.Quantity
	value, reb := float64(float64(p.Quantity)*p.Price), r.rebalance
	if p.Side == event.OrderSideSell {
		reb.sold[p.InstrumentID] += p.Quantity
		if r.exec.CashRule == event.CashRuleTotal {
			reb.budget.AddProceeds(value - p.Commission)
		}
	} else {
		reb.budget.Fill(p.ClientOrderID, value+p.Commission)
	}
	if o.filled == o.planned.Quantity {
		return r.terminal(in, o, p.FilledAt)
	}
	return nil, nil
}

// terminal marks o terminal and, when it was the last working sell of the
// rebalance in progress, plans the buy phase, caused by this report.
//
// Every order not yet terminal belongs to the rebalance in progress: a new
// rebalance is planned only once every order is terminal (withhold). And the
// buys are planned only once every sell is terminal, so no sell becomes
// terminal after them.
func (r *momentumRun) terminal(in event.Envelope, o *trackedOrder, at time.Time) ([]event.Envelope, error) {
	o.terminal = true
	reb := r.rebalance
	if o.planned.Side == event.OrderSideBuy {
		reb.budget.Close(o.planned.ClientOrderID)
		return nil, nil
	}
	for _, id := range reb.sells {
		if !r.orders[id].terminal {
			return nil, nil
		}
	}
	b := &emissions{h: r.h, in: in, at: at}
	r.planBuys(b)
	return b.result()
}

// payload maps a momentum decision to strategy.targets.decided.
func (r *momentumRun) payload(d momentum.Decision, end time.Time) event.TargetsDecidedPayload {
	p := event.TargetsDecidedPayload{
		AccountID: r.h.cfg.AccountID, StrategyID: r.h.cfg.StrategyID,
		DecisionTime: end, SessionPeriodEnd: end, Outcome: string(d.Outcome),
		Momentum: event.MomentumFigures{
			CloseSession: d.CloseSession, TrendInstrument: r.rule.TrendInstrument,
			TrendCloses: d.Trend.Closes, TrendClose: d.Trend.Close, TrendMean: d.Trend.Mean, RiskOff: d.Trend.RiskOff,
			UniverseRows: d.Universe.Rows, UniverseListingType: d.Universe.CommonStock,
			UniverseAtOrAboveFloor: d.Universe.AtOrAboveFloor, UniverseMembers: d.Universe.Members,
		},
	}
	if d.Outcome == momentum.OutcomeWithheld {
		p.WithheldReason = string(d.Reason)
		p.Detail = withheldDetail(d.Reason, r.rule, d.CloseSession)
	}
	for _, n := range d.Names {
		p.Momentum.Names = append(p.Momentum.Names, event.MomentumName{
			InstrumentID: n.InstrumentID, UniverseRank: n.UniverseRank, DollarVolume: n.DollarVolume,
			Sector: n.Sector, SectorSource: n.SectorSource, Status: string(n.Status), Closes: n.Closes,
			Score: n.Score, Volatility: n.Volatility, ScoreRank: n.ScoreRank, Weight: n.Weight,
			BacktestWouldPick: n.BacktestWouldPick,
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
