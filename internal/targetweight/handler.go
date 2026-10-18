// Package targetweight is the target-weight strategy module (ADR 0026,
// point 1): a replay.Handler that decides each account's target weights from
// completed bars, the account snapshot and its configuration, and hands each
// decision to the strategy-independent order planner (internal/orderplan) and
// the hard-limit gate (internal/risklimit). It shares no state with the
// Turtle reducer in internal/strategy (AGENTS.md rule 4). It decides Boost for
// the joint margin account and the momentum combination for an IRA, one
// configuration per account.
package targetweight

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
	"github.com/richard-whittemore/TrendInvesting/internal/reconcile"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/boost"
)

// RulesVersion is the target-weight module's rules version (ADR 0016),
// bumped only when a rule changes.
//
//   - 1.1.0 reads configuration schema version 2 and refuses version 1
//     (ADR 0015), so a journal written under 1.0.0 does not replay.
//   - 1.2.0 adds the glide path to momentum decisions (configuration
//     schema version 3, targets.decided schema version 3; ADR 0026).
//   - 1.3.0 reads market.bar.completed version 2 and refuses version 1, and
//     computes its indicators from the split- and dividend-adjusted close
//     (ADR 0026), so a journal written under 1.2.0 does not replay.
//   - 1.4.0 adds D7-L's minimum-lot substitution of an unaffordable pick and
//     records skipped trades (configuration schema version 4,
//     targets.decided schema version 4), so a journal written under 1.3.0
//     does not replay.
//   - 1.5.0 reads configuration schema version 5 (the margin-cushion
//     thresholds and MaxSnapshotAge) and refuses earlier versions, and its
//     gate reads the kill switch, broker snapshots and the venue's reports on
//     released orders, and issues ADR 0019's sale back to 98%, so a journal
//     written under 1.4.0 does not replay.
//   - 1.6.0 reads the universe input shape the configuration declares
//     (configuration schema version 6) and refuses version 5, and withholds
//     a decision session whose ranked members sent neither their bars nor
//     their histories, so a journal written under 1.5.0 does not replay.
//   - 1.7.0 is reserved by a rules change landing in parallel, so it is not
//     defined here.
//   - 1.8.0 has the gate read a venue's lifecycle report on a released order
//     whose limit is within half a cent of the order's (event.SameLimitPrice,
//     the cent a limit is rounded to) where 1.6.0 refused any limit not
//     bit-equal to it, so a journal holding such a report decides differently
//     under 1.6.0.
//   - 1.9.0 reads account.reconciliation, market.corporate-action and
//     operator.alert.acknowledged; a reconciliation's difference degrades or
//     halts the account (ADR 0019, amended 2026-09-24), and a Degraded scope
//     can name cash components, and it reads operator.run.approved schema
//     version 2 (the incident in force states its cash scope) and refuses
//     version 1, so a journal written under 1.8.0, or holding a version 1
//     approval, does not replay.
//   - 1.10.0 was reserved for the operator inputs below, but they were
//     released as 1.12.0 after 1.11.0 landed first; 1.7.0 was never released.
//   - 1.11.0 records, in a dry-run momentum run only, a synthetic terminal
//     report (execution.order.lifecycle version 3) for each order it
//     releases and plans the buy phase at the same close (ADR 0026, point 3,
//     "Mode", amended 2026-10-17), so a dry-run journal written under an
//     earlier rules version, which waited on its unsent sells and withheld
//     later months as rebalance-in-progress, does not replay. Paper and live
//     decisions are unchanged.
//   - 1.12.0 reads the owner's operator.rebalance.cleared in a momentum run,
//     which ends a rebalance in progress (ADR 0026, point 5), and its gate
//     refuses a kill switch stating a time earlier than the last one it
//     applied (risklimit.Gate.SetKillSwitch; ADR 0019, amendment of
//     2026-10-17), so a journal written under an earlier version does not
//     replay.
const RulesVersion = "1.12.0"

// source names this handler on every envelope it emits.
const source = "target-weight"

// idTime is the fixed timestamp layout of the decision IDs this handler builds.
const idTime = "2006-01-02T15:04:05Z07:00"

// Options are run-level facts, recorded beside the configuration hash rather
// than inside it (ADR 0016).
type Options struct {
	// DecideFrom is the first session decided. Sessions before it only feed
	// the indicators, as QuantConnect's warm-up does (template.py:55-67).
	DecideFrom time.Time
}

// Handler decides one account's target weights: Boost, described here, or
// momentum (see momentumRun).
//
// # Boost's inputs, in stream order
//
//   - strategy.target-weight.configuration, first and once, matching the
//     configuration the handler was built with;
//   - optionally, straight after it, one recorded strategy.targets.decided:
//     the last decision of an earlier run. A restart recovers the latched
//     boost and the target in force from it, never by recomputing (ADR 0026,
//     point 5); sessions up to its session then only feed the indicators,
//     which are rebuilt from the journal's own bars from the warm-up start;
//   - optionally, straight after the configuration and before any other
//     input, the owner's operator.run.approved: a fresh run after an earlier
//     one starts from the approved latch and the incident in force. A handler
//     told to await it (AwaitApproval) refuses every other input until it is
//     recorded, since a fresh run decides nothing before the approval (ADR
//     0027, amended 2026-10-17);
//   - optionally, before the first bar or session, the earlier run's
//     strategy.engine.state records, in journal order. A restart while
//     Degraded or Halted resumes in that state and scope (ADR 0019, amended
//     2026-09-24, "The state survives a restart"; ADR 0026, point 5);
//   - per session: the Boost instrument's market.bar.completed version 2
//     (every bar on one adjusted base), account.snapshot version 3,
//     market.mark.recorded, then market.session.closed, which decides;
//   - at any time after the configuration: operator.kill-switch, and
//     account.broker.snapshot, which also evaluates the margin-cushion alarm;
//   - after an order's release: the venue's execution.fill (version 5) and
//     execution.order.lifecycle (version 2) reports on it, which resolve a
//     released sell's pending shares (risklimit.Gate.PendingSells). A
//     lifecycle report is recorded and otherwise decides nothing (ADR 0022);
//     one about an order this run did not release, or that does not describe
//     it as released, is refused: it is reconciliation's to explain (ADR
//     0019);
//   - at any time after the configuration: market.corporate-action, which a
//     reconciliation's book is bridged by; account.reconciliation, which
//     classifies IBKR's positions and cash against the latest account
//     snapshot and every fill and corporate action since it, and degrades or
//     halts the account on a difference (ADR 0019, amended 2026-09-24); and
//     operator.alert.acknowledged, the owner's acknowledgement, which the
//     reporter reads and which decides nothing here.
//
// # The sale back to 98%
//
// When a session's planned sell is refused (Boost's exit fired, but the
// account is Halted, Degraded on the instrument, or the kill switch is on),
// or a broker snapshot turns the margin alarm urgent, the gate decides
// whether the sale back to the unlevered weight goes (risklimit.Gate.Sale);
// the sale, or why it was withheld, is journalled, then checked like any
// other order.
//
// # Fail-closed rules
//
// A session that closes without the instrument's bar withholds
// (missing-bars), and so does every later session: the indicators would span
// the gap (plan.md section 2, "Boost"). A session with no snapshot as of its
// close, or only one taken after it, withholds (stale-account-snapshot):
// orders are valued at the previous close (ADR 0026, point 2). But the latch still advances, as
// the research's sig does on every bar (template.py:89-94); the target in
// force changes only on a decided outcome, so a change withheld one session
// is ordered the next.
type Handler struct {
	strategyVersion string
	hash            string
	cfg             event.TargetWeightConfigurationPayload
	params          boost.Parameters
	warmUp          time.Time
	decideFrom      time.Time

	configured bool
	started    bool // a bar or session has been applied
	recovered  bool
	approved   bool
	awaiting   bool      // a fresh run: nothing but the approval is read until it is recorded
	prelude    bool      // an input other than the configuration and an approval has been read
	quietUntil time.Time // sessions at or before it only feed the indicators

	indicators *boost.Indicators
	latched    bool
	inForce    float64
	inForceSet bool
	gapAt      time.Time
	lastPeriod time.Time // last session closed
	bar        *event.CompletedBarAdjustedPayload
	base       string // the adjusted view's base, fixed by the first bar
	snapshot   *event.AccountPositionsSnapshotPayload
	mark       *event.MarkRecordedPayload
	gate       *risklimit.Gate
	// released holds every order this run released, by client order ID,
	// so a venue's report about one can be checked against it.
	released map[string]event.OrderReleasedPayload

	momentum *momentumRun // set for a momentum configuration, nil for Boost
}

// New returns a handler for cfg, deriving its configuration hash itself
// (ADR 0016) so no caller can pair a configuration with another's hash.
func New(strategyVersion string, cfg event.TargetWeightConfigurationPayload, opts Options) (*Handler, error) {
	if strategyVersion == "" {
		return nil, errors.New("targetweight: strategy version is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("targetweight: %w", err)
	}
	if opts.DecideFrom.IsZero() {
		return nil, errors.New("targetweight: the first session to decide is required")
	}
	if cfg.StrategyID == event.TargetWeightStrategyMomentum {
		return newMomentum(strategyVersion, cfg, opts)
	}
	warmUp, _ := cfg.Boost.WarmUpDate() // validated above
	params := boost.Parameters{
		InvestedFraction: cfg.Boost.InvestedFraction, BoostSize: cfg.Boost.BoostSize, RSIEntry: cfg.Boost.RSIEntry,
		RSIPeriod: cfg.Boost.RSIPeriod, TrendDays: cfg.Boost.TrendDays, ExitSMA: cfg.Boost.ExitSMA,
	}
	indicators, err := boost.NewIndicators(params)
	if err != nil {
		return nil, fmt.Errorf("targetweight: %w", err)
	}
	gate, err := risklimit.NewGate(cfg.AccountID, cfg.Limits, cfg.Mode, time.Duration(cfg.Execution.MaxSnapshotAge)*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("targetweight: %w", err)
	}
	return &Handler{
		strategyVersion: strategyVersion,
		hash:            event.TargetWeightConfigurationHash(cfg),
		cfg:             cfg,
		params:          params,
		warmUp:          warmUp,
		decideFrom:      opts.DecideFrom,
		indicators:      indicators,
		gate:            gate,
	}, nil
}

// AwaitApproval marks this run as a fresh run after an earlier one, which
// decides nothing until the owner's approval is recorded (ADR 0027, amended
// 2026-10-17): until then, every input but the configuration and the
// approval is refused. Call it before the first input. The momentum handler
// reads no approval and ignores it.
func (h *Handler) AwaitApproval() { h.awaiting = true }

// ConfigurationHash returns the hash every emission carries.
func (h *Handler) ConfigurationHash() string { return h.hash }

// Apply implements replay.Handler.
func (h *Handler) Apply(_ context.Context, in event.Envelope) ([]event.Envelope, error) {
	if !h.configured && in.Type != event.TargetWeightConfigurationEventType {
		return nil, fmt.Errorf("targetweight: %s %s arrived before the configuration", in.Type, in.ID)
	}
	if h.momentum != nil && in.Type != event.TargetWeightConfigurationEventType {
		return h.momentum.apply(in)
	}
	if in.Type != event.TargetWeightConfigurationEventType && in.Type != event.OperatorRunApprovedEventType {
		if h.awaiting && !h.approved {
			return nil, fmt.Errorf("targetweight: %s %s arrived before the owner's approval; a fresh run after an earlier one reads nothing but its configuration until the approval is recorded (ADR 0027, amended 2026-10-17)", in.Type, in.ID)
		}
		h.prelude = true
	}
	switch in.Type {
	case event.TargetWeightConfigurationEventType:
		return nil, h.applyConfiguration(in)
	case event.TargetsDecidedEventType:
		return nil, h.applyRecovery(in)
	case event.OperatorRunApprovedEventType:
		return nil, h.applyApproval(in)
	case event.EngineStateEventType:
		return nil, h.applyRecordedState(in)
	case event.KillSwitchEventType:
		return nil, h.applyKillSwitch(in)
	case event.BrokerSnapshotEventType:
		return h.applyBrokerSnapshot(in)
	case event.CompletedBarEventType:
		return nil, h.applyBar(in)
	case event.AccountSnapshotEventType:
		return nil, h.applySnapshot(in)
	case event.MarkRecordedEventType:
		return nil, h.applyMark(in)
	case event.SessionClosedEventType:
		return h.applySession(in)
	case event.FillEventType:
		return nil, h.applyFill(in)
	case event.OrderLifecycleEventType:
		return nil, h.applyLifecycle(in)
	case event.ReconciliationEventType:
		return h.applyReconciliation(in)
	case event.MarketCorporateActionEventType:
		return nil, h.applyCorporateAction(in)
	case event.AlertAcknowledgedEventType:
		return nil, h.applyAcknowledgement(in)
	case event.RunCompletedEventType:
		return nil, nil
	}
	return nil, fmt.Errorf("targetweight: input type %q is not one this handler reads", in.Type)
}

// decodeInput checks an input's schema version and decodes and validates its payload.
func decodeInput[T interface{ Validate() error }](in event.Envelope, version uint32) (T, error) {
	var p T
	if in.SchemaVersion != version {
		return p, fmt.Errorf("targetweight: %s schema version %d is not the version %d this build reads (ADR 0015)", in.Type, in.SchemaVersion, version)
	}
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return p, fmt.Errorf("targetweight: decode %s %s: %w", in.Type, in.ID, err)
	}
	// A payload whose fields depend on its version is checked against the
	// version it arrived at, so a synthetic lifecycle report, which only an
	// engine records (version 3), is never read as a venue's (ADR 0015).
	var err error
	if v, ok := any(p).(interface{ ValidateSchema(uint32) error }); ok {
		err = v.ValidateSchema(in.SchemaVersion)
	} else {
		err = p.Validate()
	}
	if err != nil {
		return p, fmt.Errorf("targetweight: %s %s: %w", in.Type, in.ID, err)
	}
	return p, nil
}

func (h *Handler) applyConfiguration(in event.Envelope) error {
	if h.configured {
		return errors.New("targetweight: a run is configured once")
	}
	p, err := decodeInput[event.TargetWeightConfigurationPayload](in, event.TargetWeightConfigurationSchemaVersion)
	if err != nil {
		return err
	}
	if got := event.TargetWeightConfigurationHash(p); got != h.hash || in.ConfigurationHash != h.hash {
		return fmt.Errorf("targetweight: configuration %s (envelope %s) is not the configuration %s this run was built with", got, in.ConfigurationHash, h.hash)
	}
	h.configured = true
	return nil
}

// applyRecovery restores Boost's latch and target in force from an earlier
// run's recorded decision (ADR 0026, point 5). It comes once, straight after
// the configuration, and never after an approval: the approval states the
// latch in force, and nothing after it changes that latch (ADR 0026, point
// 5; ADR 0027, amended 2026-10-17), just as applyApproval refuses to follow
// a recovered decision.
func (h *Handler) applyRecovery(in event.Envelope) error {
	if h.recovered || h.approved || h.started {
		return errors.New("targetweight: a recovered decision comes once, straight after the configuration, never after an approval or a bar")
	}
	p, err := decodeInput[event.TargetsDecidedPayload](in, event.TargetsDecidedSchemaVersion)
	if err != nil {
		return err
	}
	if in.ConfigurationHash != h.hash || p.AccountID != h.cfg.AccountID || p.Boost.InstrumentID != h.cfg.Boost.InstrumentID {
		return fmt.Errorf("targetweight: recovered decision %s belongs to another configuration or account", in.ID)
	}
	h.recovered = true
	h.latched = p.Boost.LatchedAfter
	h.inForce, h.inForceSet = p.Boost.TargetInForce, p.Boost.TargetInForceSet
	if p.Outcome == event.TargetsOutcomeDecided {
		h.inForce, h.inForceSet = p.Targets[0].Weight, true
	}
	h.quietUntil = p.SessionPeriodEnd
	return nil
}

// applyApproval starts a fresh run from the owner's approval: Boost's latch
// is the approved one, never recomputed from the bars (ADR 0026, point 5).
// An incident in force puts the gate in the incident's Degraded or Halted
// state and scope before any market input, since a restart while Degraded
// or Halted resumes in that state (ADR 0019, amended 2026-09-24, "The state
// survives a restart"), and carries whether its one sale back to 98% was
// attempted, so the fresh run never attempts it twice (ADR 0019, amended
// 2026-10-15). It comes once, straight after the configuration, before any
// other input, so the gate it restores into is the one New built: no
// recorded state, decision or sale of this run can precede it.
func (h *Handler) applyApproval(in event.Envelope) error {
	if h.approved || h.prelude {
		return errors.New("targetweight: an approval comes once, straight after the configuration and before any other input")
	}
	p, err := decodeInput[event.OperatorRunApprovedPayload](in, event.OperatorRunApprovedSchemaVersion)
	if err != nil {
		return err
	}
	if in.ConfigurationHash != h.hash {
		return fmt.Errorf("targetweight: approval %s belongs to another configuration", in.ID)
	}
	if p.AccountID != h.cfg.AccountID {
		return fmt.Errorf("targetweight: approval %s is for another account, %q; this run decides %q", in.ID, p.AccountID, h.cfg.AccountID)
	}
	if incident := p.IncidentInForce; incident != nil {
		if err := h.gate.RestoreSaleAttempt(incident.IncidentID, *incident.SaleAttempted); err != nil {
			return fmt.Errorf("targetweight: approval %s: %w", in.ID, err)
		}
		if err := h.gate.RestoreIncidentState(incident.IncidentID, incident.State, incident.ScopeInstrumentIDs, incident.ScopeCashComponents); err != nil {
			return fmt.Errorf("targetweight: approval %s: %w", in.ID, err)
		}
	}
	h.approved = true
	h.latched = *p.Boost.Latched
	return nil
}

// applyRecordedState restores an earlier run's engine state, scope and
// incident into the gate before any decision (ADR 0026, point 5).
func (h *Handler) applyRecordedState(in event.Envelope) error {
	if h.started {
		return fmt.Errorf("targetweight: recorded %s %s arrived after the run began; an earlier run's records come first", in.Type, in.ID)
	}
	if in.ConfigurationHash != h.hash {
		return fmt.Errorf("targetweight: recorded %s %s belongs to another configuration", in.Type, in.ID)
	}
	if in.SchemaVersion != event.EngineStateSchemaVersion && in.SchemaVersion != event.EngineStateScopedVersion {
		return fmt.Errorf("targetweight: %s schema version %d is not one this build reads (ADR 0015)", in.Type, in.SchemaVersion)
	}
	var p event.EngineStatePayload
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return fmt.Errorf("targetweight: decode %s %s: %w", in.Type, in.ID, err)
	}
	if err := h.gate.RestoreState(in.ID, p, in.SchemaVersion); err != nil {
		return fmt.Errorf("targetweight: %s %s: %w", in.Type, in.ID, err)
	}
	return nil
}

func (h *Handler) applyBar(in event.Envelope) error {
	p, err := decodeInput[event.CompletedBarAdjustedPayload](in, event.CompletedBarAdjustedVersion)
	if err != nil {
		return err
	}
	switch {
	case h.base != "" && p.AdjustedBase != h.base:
		return fmt.Errorf("targetweight: bar %s is based at %s; this run's bars are based at %s, and levels on two bases cannot be compared", in.ID, p.AdjustedBase, h.base)
	case p.InstrumentID != h.cfg.Boost.InstrumentID:
		return fmt.Errorf("targetweight: bar for %s; this account decides %s only", p.InstrumentID, h.cfg.Boost.InstrumentID)
	case h.bar != nil:
		return fmt.Errorf("targetweight: a second bar for %s before its session closed", p.InstrumentID)
	case !p.PeriodEnd.After(h.lastPeriod):
		return fmt.Errorf("targetweight: bar %s does not follow the last session %s", p.PeriodEnd, h.lastPeriod)
	case p.PeriodEnd.Before(h.warmUp):
		return fmt.Errorf("targetweight: bar %s precedes the warm-up start %s", p.PeriodEnd, h.cfg.Boost.WarmUpStart)
	}
	h.started = true
	h.base = p.AdjustedBase
	h.bar = &p
	return nil
}

func (h *Handler) applySnapshot(in event.Envelope) error {
	p, err := decodeInput[event.AccountPositionsSnapshotPayload](in, event.AccountSnapshotPositionsVersion)
	if err != nil {
		return err
	}
	if p.AccountID != h.cfg.AccountID {
		return fmt.Errorf("targetweight: snapshot for account %q; this run decides %q", p.AccountID, h.cfg.AccountID)
	}
	if h.snapshot != nil && !p.AsOf.After(h.snapshot.AsOf) {
		return fmt.Errorf("targetweight: snapshot as of %s does not follow the last, %s", p.AsOf, h.snapshot.AsOf)
	}
	h.snapshot = &p
	h.gate.ObserveAccount(p)
	h.gate.SetBook(reconcile.PositionsBook(in.ID, p))
	return nil
}

func (h *Handler) applyMark(in event.Envelope) error {
	p, err := decodeInput[event.MarkRecordedPayload](in, event.MarkRecordedSchemaVersion)
	if err != nil {
		return err
	}
	if p.InstrumentID != h.cfg.Boost.InstrumentID {
		return fmt.Errorf("targetweight: mark for %s; this account trades %s only", p.InstrumentID, h.cfg.Boost.InstrumentID)
	}
	if h.mark != nil && !p.AsOf.After(h.mark.AsOf) {
		return fmt.Errorf("targetweight: mark as of %s does not follow the last, %s", p.AsOf, h.mark.AsOf)
	}
	h.mark = &p
	return nil
}

func (h *Handler) applySession(in event.Envelope) ([]event.Envelope, error) {
	p, err := decodeInput[event.SessionClosedPayload](in, event.SessionClosedSchemaVersion)
	if err != nil {
		return nil, err
	}
	if !p.PeriodEnd.After(h.lastPeriod) {
		return nil, fmt.Errorf("targetweight: session %s does not follow the last, %s", p.PeriodEnd, h.lastPeriod)
	}
	named := false
	for _, id := range p.InstrumentIDs {
		named = named || id == h.cfg.Boost.InstrumentID
	}
	bar := h.bar
	if bar != nil && !bar.PeriodEnd.Equal(p.PeriodEnd) {
		return nil, fmt.Errorf("targetweight: bar %s is not the session %s being closed", bar.PeriodEnd, p.PeriodEnd)
	}
	if named != (bar != nil) {
		return nil, fmt.Errorf("targetweight: session %s and the bars received disagree about %s", p.PeriodEnd, h.cfg.Boost.InstrumentID)
	}
	h.started = true
	h.lastPeriod = p.PeriodEnd
	h.bar = nil

	quiet := p.PeriodEnd.Before(h.decideFrom) || !p.PeriodEnd.After(h.quietUntil)
	if bar == nil && h.gapAt.IsZero() {
		h.gapAt = p.PeriodEnd
	}
	b := &emissions{h: h, in: in, at: p.PeriodEnd}
	if !h.gapAt.IsZero() {
		if quiet {
			return nil, nil
		}
		figures := h.figures(boost.Readings{}, h.latched)
		b.targets(figures, event.WithheldMissingBars,
			fmt.Sprintf("no %s bar in the session closing %s; the indicators are not computed across the gap", h.cfg.Boost.InstrumentID, h.gapAt.Format(idTime)))
		return b.result()
	}

	// RSI(2) and the means read the split- and dividend-adjusted close, as
	// the backtest's adjusted series does (ADR 0026, point 1).
	readings := h.indicators.Add(bar.Adjusted.Close)
	if quiet {
		return nil, nil
	}
	before := h.latched
	h.latched = h.params.Next(before, readings)
	figures := h.figures(readings, before)
	snapshot := h.snapshot
	switch {
	case snapshot == nil || snapshot.AsOf.Before(p.PeriodEnd):
		b.targets(figures, event.WithheldStaleAccountSnapshot,
			fmt.Sprintf("no account snapshot for %s as of the session closing %s", h.cfg.AccountID, p.PeriodEnd.Format(idTime)))
		return b.result()
	case snapshot.AsOf.After(p.PeriodEnd):
		b.targets(figures, event.WithheldStaleAccountSnapshot,
			fmt.Sprintf("the account snapshot for %s is as of %s, after the session closing %s; orders are valued at that close",
				h.cfg.AccountID, snapshot.AsOf.Format(idTime), p.PeriodEnd.Format(idTime)))
		return b.result()
	}
	decisionID, decided := b.targets(figures, "", "")
	h.inForce, h.inForceSet = decided.Targets[0].Weight, true
	h.gate.RecordDecision(decisionID)

	// The planner and the gate cannot refuse what this handler has validated
	// (the decision just built, a validated snapshot, the bar's raw close and
	// the configured band and limits), so an error from either is a defect.
	order, ok, err := orderplan.PlanBoost(orderplan.BoostInput{
		Tag: decisionID, Decision: decided, Snapshot: *snapshot,
		ReferencePrice: bar.Raw.Close, PriceBand: h.cfg.Limits.PriceBand,
		WorkingSells: h.gate.PendingSells(h.cfg.Boost.InstrumentID),
	})
	b.fail(err)
	if !ok || order.Side != event.OrderSideSell {
		// The decision no longer plans the sell that fired a withheld sale.
		if l := h.gate.LapseExitSale(); l != nil {
			b.lapsed(*l)
		}
	}
	if !ok {
		return b.result()
	}
	gateOrder := risklimit.Order{Planned: order}
	if h.mark != nil && !h.mark.AsOf.Before(p.PeriodEnd) {
		gateOrder.Mark, gateOrder.MarkSet = h.mark.Price, true
	}
	book := risklimit.Book{Holdings: holdings(snapshot.Positions), Prices: map[string]float64{h.cfg.Boost.InstrumentID: bar.Raw.Close}, At: b.at}
	if outcome := b.order(gateOrder, book); order.Side == event.OrderSideSell && outcome.Refused != nil {
		b.sale(event.SaleTriggerExit)
	}
	return b.result()
}

func (h *Handler) applyKillSwitch(in event.Envelope) error {
	p, err := decodeInput[event.KillSwitchPayload](in, event.KillSwitchSchemaVersion)
	if err != nil {
		return err
	}
	if err := h.gate.SetKillSwitch(in.ID, p); err != nil {
		return fmt.Errorf("targetweight: %s %s: %w", in.Type, in.ID, err)
	}
	return nil
}

// applyFill records a venue's execution of a released order: a sell's
// filled shares are no longer pending (ADR 0026, point 2.5).
func (h *Handler) applyFill(in event.Envelope) error {
	p, err := decodeInput[event.TargetWeightFillPayload](in, event.FillTargetWeightVersion)
	if err != nil {
		return err
	}
	if err := h.gate.ObserveFill(p); err != nil {
		return fmt.Errorf("targetweight: %s %s: %w", in.Type, in.ID, err)
	}
	return nil
}

// applyReconciliation classifies a recorded reconciliation against the
// journal's book (risklimit.Gate.Reconcile) and journals the result, then
// the engine state when it moved. Its emissions are timed at the
// observation.
func (h *Handler) applyReconciliation(in event.Envelope) ([]event.Envelope, error) {
	p, err := decodeInput[event.ReconciliationPayload](in, event.ReconciliationSchemaVersion)
	if err != nil {
		return nil, err
	}
	r, err := h.gate.Reconcile(in.ID, p)
	if err != nil {
		return nil, fmt.Errorf("targetweight: %s %s: %w", in.Type, in.ID, err)
	}
	b := &emissions{h: h, in: in, at: p.AsOf}
	b.add(r.ResultID, event.ReconciliationResultEventType, event.ReconciliationResultSchemaVersion, r.Result)
	if r.State != nil {
		b.add(r.StateID, event.EngineStateEventType, r.StateSchemaVersion, *r.State)
	}
	return b.result()
}

// applyCorporateAction records a journalled corporate action, at any of the
// schema versions event.UpcastCorporateActionPayload reads, as a change a
// reconciliation's book is bridged by. It changes no target or order.
func (h *Handler) applyCorporateAction(in event.Envelope) error {
	p, err := event.UpcastCorporateActionPayload(in.SchemaVersion, in.Payload)
	if err != nil {
		return fmt.Errorf("targetweight: corporate action %s: %w", in.ID, err)
	}
	if err := h.gate.ObserveCorporateAction(in.ID, p); err != nil {
		return fmt.Errorf("targetweight: corporate action %s: %w", in.ID, err)
	}
	return nil
}

// applyAcknowledgement reads the owner's acknowledgement of an alert, which
// stops the reporter repeating it (ADR 0019, amended 2026-09-24,
// "Alerting"). It decides nothing and changes no engine state: leaving
// Degraded or Halted is the recovery procedure's.
func (h *Handler) applyAcknowledgement(in event.Envelope) error {
	p, err := decodeInput[event.AlertAcknowledgedPayload](in, event.AlertAcknowledgedSchemaVersion)
	if err != nil {
		return err
	}
	if p.AccountID != h.cfg.AccountID {
		return fmt.Errorf("targetweight: acknowledgement %s is for account %q; this run decides %q", in.ID, p.AccountID, h.cfg.AccountID)
	}
	return nil
}

// applyBrokerSnapshot records the latest broker observation and journals the
// margin alarm when its level changes; an alarm turning urgent fires the sale
// back to 98%. Its emissions are timed at the snapshot.
func (h *Handler) applyBrokerSnapshot(in event.Envelope) ([]event.Envelope, error) {
	p, err := decodeInput[event.BrokerSnapshotPayload](in, event.BrokerSnapshotSchemaVersion)
	if err != nil {
		return nil, err
	}
	obs, err := h.gate.ObserveBroker(in.ID, p)
	if err != nil {
		return nil, fmt.Errorf("targetweight: %s %s: %w", in.Type, in.ID, err)
	}
	b := &emissions{h: h, in: in, at: p.AsOf}
	if obs.Alarm != nil {
		b.add(obs.AlarmID, event.MarginAlarmEventType, event.MarginAlarmSchemaVersion, *obs.Alarm)
	}
	for _, l := range obs.Lapsed {
		b.lapsed(l)
	}
	if obs.Urgent {
		b.sale(event.SaleTriggerMarginUrgent)
	}
	if obs.RetryExit {
		b.sale(event.SaleTriggerExit)
	}
	return b.result()
}

// applyLifecycle records the venue's report of a change to an order this
// run released, and decides nothing else from it (ADR 0022). The report must
// name the order as released: its account, instrument, side, quantity and
// limit, the limit within event.LimitPriceTolerance (event's Describes). A
// canceled or invalid sell's remainder is then no longer pending, a
// cancel-pending one still is (ADR 0026, point 2.5).
func (h *Handler) applyLifecycle(in event.Envelope) error {
	p, err := decodeInput[event.LimitOrderLifecyclePayload](in, event.OrderLifecycleLimitVersion)
	if err != nil {
		return err
	}
	o, ok := h.released[p.ClientOrderID]
	if !ok {
		return fmt.Errorf("targetweight: lifecycle %s reports order %s, which this run did not release", in.ID, p.ClientOrderID)
	}
	if !p.Describes(o) {
		return fmt.Errorf("targetweight: lifecycle %s reports %s %s %d %s at %v; order %s was released as %s %s %d %s at %v",
			in.ID, p.AccountID, p.Side, p.Quantity, p.InstrumentID, p.LimitPrice, p.ClientOrderID, o.AccountID, o.Side, o.Quantity, o.InstrumentID, o.LimitPrice)
	}
	if err := h.gate.ObserveLifecycle(p); err != nil {
		return fmt.Errorf("targetweight: %s %s: %w", in.Type, in.ID, err)
	}
	return nil
}

// holdings maps each position to its quantity.
func holdings(positions []event.AccountPosition) map[string]int64 {
	out := make(map[string]int64, len(positions))
	for _, p := range positions {
		out[p.InstrumentID] = p.Quantity
	}
	return out
}

// figures are the Boost figures for this session, with latchedBefore and the
// latch and target in force as they stand.
func (h *Handler) figures(r boost.Readings, latchedBefore bool) event.BoostFigures {
	return event.BoostFigures{
		InstrumentID: h.cfg.Boost.InstrumentID, Close: r.Close,
		RSI: r.RSI, RSIReady: r.RSIReady, TrendMean: r.TrendMean, TrendMeanReady: r.TrendMeanReady,
		ExitMean: r.ExitMean, ExitMeanReady: r.ExitMeanReady,
		LatchedBefore: latchedBefore, LatchedAfter: h.latched,
		TargetInForce: h.inForce, TargetInForceSet: h.inForceSet,
	}
}

// emissions collects one input's emissions in order, keeping the first error.
type emissions struct {
	h   *Handler
	in  event.Envelope
	at  time.Time
	out []event.Envelope
	err error
}

func (b *emissions) fail(err error) {
	if err != nil && b.err == nil {
		b.err = fmt.Errorf("targetweight: %w", err)
	}
}

func (b *emissions) add(id, eventType string, version uint32, payload interface{ Validate() error }) {
	e, err := b.h.stamp(id, eventType, version, b.at, b.in, payload)
	b.fail(err)
	b.out = append(b.out, e)
}

func (b *emissions) result() ([]event.Envelope, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.out, nil
}

// targets adds the session's strategy.targets.decided, decided with the
// latch's target when reason is empty, else withheld with reason and detail,
// and returns its ID and payload.
func (b *emissions) targets(figures event.BoostFigures, reason, detail string) (string, event.TargetsDecidedPayload) {
	h := b.h
	p := event.TargetsDecidedPayload{
		AccountID: h.cfg.AccountID, StrategyID: h.cfg.StrategyID,
		DecisionTime: b.at, SessionPeriodEnd: b.at,
		Outcome: event.TargetsOutcomeDecided, Boost: figures,
	}
	if reason == "" {
		p.Targets = []event.TargetWeight{{InstrumentID: h.cfg.Boost.InstrumentID, Weight: h.params.Target(h.latched)}}
	} else {
		p.Outcome, p.WithheldReason, p.Detail = event.TargetsOutcomeWithheld, reason, detail
	}
	id := fmt.Sprintf("targets-decided:%s:%s", h.cfg.AccountID, b.at.UTC().Format(idTime))
	b.add(id, event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, p)
	return id, p
}

// stamp builds an emission: EventTime is the session's close and RecordedAt
// the input's, never the wall clock. Sequence, causation and correlation are
// the engine's to set (replay.Handler).
func (h *Handler) stamp(id, eventType string, version uint32, at time.Time, in event.Envelope, payload interface{ Validate() error }) (event.Envelope, error) {
	if err := payload.Validate(); err != nil {
		return event.Envelope{}, fmt.Errorf("targetweight: %s %s: %w", eventType, id, err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return event.Envelope{}, fmt.Errorf("targetweight: %w", err)
	}
	return event.Envelope{
		ID: id, Type: eventType, EnvelopeVersion: event.CurrentEnvelopeVersion, SchemaVersion: version,
		EventTime: at, RecordedAt: in.RecordedAt, Source: source,
		StrategyVersion: h.strategyVersion, ConfigurationHash: h.hash,
		PayloadHash: event.HashPayload(raw), Payload: raw,
	}, nil
}

// order journals a planned order, checks it with the gate against book and
// journals the gate's outcome and any state change, returning the outcome.
// Each is journalled at the schema version its SchemaVersion names (ADR
// 0015).
func (b *emissions) order(o risklimit.Order, book risklimit.Book) risklimit.Outcome {
	o.PlanID = "order-planned:" + o.Planned.ClientOrderID
	b.add(o.PlanID, event.OrderPlannedEventType, o.Planned.SchemaVersion(), o.Planned)
	// The gate cannot refuse what this handler has validated (a planned
	// order built from validated inputs), so an error is a defect and leaves
	// an empty outcome, which journals nothing.
	outcomes, err := b.h.gate.Check(book, []risklimit.Order{o})
	b.fail(err)
	var out risklimit.Outcome
	if len(outcomes) == 1 {
		out = outcomes[0]
	}
	if out.Released != nil {
		if b.h.released == nil {
			b.h.released = map[string]event.OrderReleasedPayload{}
		}
		b.h.released[out.Released.ClientOrderID] = *out.Released
		b.add("risk-order-released:"+o.Planned.ClientOrderID, event.OrderReleasedEventType, out.Released.SchemaVersion(), *out.Released)
	} else if out.Refused != nil {
		b.add("risk-order-refused:"+o.Planned.ClientOrderID, event.OrderRefusedEventType, out.Refused.SchemaVersion(), *out.Refused)
	}
	if out.State != nil {
		b.add(out.StateID, event.EngineStateEventType, out.StateSchemaVersion, *out.State)
	}
	return out
}

// lapsed journals the lapse of a withheld sale whose trigger the input ended,
// named for that input.
func (b *emissions) lapsed(l event.SaleWithheldPayload) {
	b.add("sale-lapsed:"+l.ClientOrderID+":"+b.in.ID, event.SaleWithheldEventType, event.SaleWithheldSchemaVersion, l)
}

// sale asks the gate for the sale back to the unlevered weight after
// trigger, and journals it and its outcome, or why it was withheld.
func (b *emissions) sale(trigger string) {
	h := b.h
	s, ok, err := h.gate.Sale(trigger, h.cfg.Boost.InstrumentID, b.at, h.cfg.Boost.InvestedFraction, h.cfg.Limits.PriceBand)
	b.fail(err)
	switch {
	case !ok:
	case s.Withheld != nil:
		// Nothing to sell is not an attempt, so the same sale may be withheld
		// again on a later trigger: the triggering input names each record.
		b.add("sale-withheld:"+s.Withheld.ClientOrderID+":"+b.in.ID, event.SaleWithheldEventType, event.SaleWithheldSchemaVersion, *s.Withheld)
	default:
		// The sale is sized from the broker snapshot alone (orderplan.PlanSale),
		// so it is valued at the broker's holding and mark: the joint margin
		// account holds the Boost instrument only.
		o := *s.Order
		book := risklimit.Book{Holdings: map[string]int64{o.InstrumentID: o.HeldQuantity}, Prices: map[string]float64{o.InstrumentID: o.ReferencePrice}, At: b.at}
		b.order(risklimit.Order{Planned: o}, book)
	}
}
