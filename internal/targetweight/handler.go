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
	"slices"
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
//   - 1.7.0 was reserved for a rules change landing in parallel and never
//     released; no journal carries it.
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
//     released as 1.12.0 after 1.11.0 landed first.
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
//   - 1.13.0 ranks and picks a momentum name with no sector label, uncapped
//     and counted toward no sector's cap, as the backtest did
//     (defensive_v2.py:69-74), where 1.12.0 left it out as no-sector: the
//     owner's decision of 2026-10-18 (ADR 0026 point 8). A journal holding
//     such a name within the book's reach decides differently, so one
//     written under 1.12.0 does not replay. It withholds a month as
//     labels-missing when more than the configuration's MaxUnlabelledShare
//     of the ranked members has no label (configuration version 7, so a
//     version 6 configuration is refused).
//   - 1.14.0 was reserved for the Boost re-plan and never released; no
//     journal carries it.
//   - 1.15.0 scores a held momentum name whose window has a gap across it,
//     from the last ScoreLookback+1 closes it has in the last 260 sessions,
//     as the backtest did (defensive_v2.py:271,286-291), where 1.13.0 left
//     it out as short-history and sold it: the owner's decision of
//     2026-10-18 (ADR 0026 point 5, amended). A candidate not held still
//     needs a complete window. The run keeps 260 sessions of closes, a
//     name left out for a gap reads gap, and every gap's sessions are
//     recorded, so a journal written under 1.13.0 does not replay. Only a
//     run reading the compact universe input scores a held name so; a bars
//     run keeps the complete-window rule for every name, held or not.
//   - 1.16.0 has Boost re-plan, at the next decided session, the difference
//     between the account's position and the target in force after a
//     refusal that leaves the engine Normal (the kill switch, a stale broker
//     snapshot), where earlier versions ordered only when the target changed
//     (owner's decision of 2026-10-18; ADR 0026, point 2, "Boost", amended
//     2026-10-18, after ADR 0019 reading 10). A refusal under Degraded or
//     Halted follows that state's own rules, as before. Each decision
//     records whether a re-plan is pending after it, and a run recovered
//     from the decision restores it; the gate flags only the first
//     kill-switch refusal of an engagement for the alert (ADR 0026, point 3,
//     kill switch: "alert once"). A journal holding such a refusal decides
//     differently under 1.15.0.
//   - 1.17.0 keeps a held momentum pick whose one share at its previous raw
//     close exceeds its target value: it keeps its place, sector slot and
//     weight, is recorded held-one-lot-short with its one-share price,
//     target value and the holding it keeps (shares, value and share of net
//     liquidation), and gets no order, where 1.16.0 skipped it under D7-L
//     and sold it in full. A pick not held is skipped as before, and a book
//     with no affordable pick is still withheld as all-unaffordable. The
//     owner's decision of 2026-10-18 (option 2), adopted on the measurement
//     in docs/research/d7l-held-name-2026-10.md (ADR 0026, point 2, "D7-L",
//     amended 2026-10-18). A journal holding such a name decides
//     differently under 1.16.0.
//   - 1.18.0 has Boost re-plan, at the next decided session, the remaining
//     difference to the target in force after the last order a decided
//     session released ends with shares unfilled, as after a refusal, where
//     1.17.0 left the account short until the target changed: canceled
//     (a DAY order's expiry included) part-filled or with nothing filled, or
//     rejected by the venue (invalid), which is re-planned once; a gate
//     refusal of that one re-plan does not reset the bound. Two of the
//     account's orders rejected in a row, whatever their targets, are
//     withheld and paged once, whichever order's rejection made it two, and
//     no session buys again until the owner starts a fresh run, while a
//     sell, which reduces exposure, still goes, each later rejection paged
//     as one while buys are stopped; the count stays at two. Each order's
//     verdict counts when the venue's first word on it is read, however
//     late: a rejection one more in a row, an acceptance, though a later
//     order replaced it as the watched one, back to none before the stop.
//     The order is watched until it fills in full or a
//     decided session plans after its end; only a later decided session's
//     released order replaces it, and each decision records it with its
//     executions, its end and its venue order ID, so a restart watches it
//     still, counts an execution sent again once, and nets a sell as any
//     working sell, matched to IBKR's listing of it by that ID when IBKR
//     echoes no client order ID. Each decision records the rejections in a
//     row, the orders they name (at the stop, the two that stopped buys) and
//     every released order the venue has not yet ruled on, so a restart
//     counts as the full run does. An acceptance ends the run only when the
//     order accepted was released after the latest rejection counted, so a
//     late acceptance of an older order does not give a re-planned order a
//     second re-plan; the decision records how many undecided orders are
//     older than that rejection. An order leaves the undecided orders at the
//     venue's first word on it, or when the owner settles it as never sent
//     (operator.rebalance.cleared, which Boost reads for that alone); a
//     dry-run lists none, since no venue receives its orders, and each of
//     its decisions records why (no_venue). The
//     cause of the re-plan it planned or carries (refused,
//     part-filled, unfilled or rejected, with the order and its filled and
//     released shares; targets.decided schema version 6). The part-fill
//     rule is the owner's decision of 2026-10-18; its extension to an order
//     with nothing filled, the bound on a rejection's re-plan, and the bound
//     of two rejections in a row were confirmed by the owner on 2026-10-18.
//     A session with no bar is withheld and skipped:
//     the indicators continue at the next bar over the closes received, and
//     that decision names the gap (gap_sessions), where 1.17.0 withheld every
//     later session. That a missing bar must not stop the run is the owner's
//     decision of 2026-10-18; skipping the session, rather than filling the
//     close forward as LEAN does, is the orchestrator's reading, put to the
//     owner for confirmation (ADR 0026, point 2, "Boost", and point 5,
//     amended 2026-10-18). A journal holding such an order or gap decides
//     differently under 1.17.0.
//   - 1.19.0 reads account.broker.snapshot version 2 (the read time beside
//     an as-of that is the session's close, or the reason the reading could
//     not stand for it) and market.mark.recorded version 2 (IBKR's last
//     price beside the mark, as evidence), which the IBKR adapter writes
//     under the owner's decision of 2026-10-18 that one reading after the
//     close, with nothing working at IBKR and no execution since, is the
//     account as of that close (ADR 0019, ADR 0025 point 5, ADR 0026 point
//     1, amended 2026-10-18). The mark is the delivered bar's official
//     close; the price band compares a checked order's reference price with
//     IBKR's last price beside it when IBKR gave one, else with that close,
//     and records which (RiskFigures.MarkSource). A broker snapshot as of
//     the close values its net liquidation at the official close, and the
//     margin alarm divides Excess Liquidity by IBKR's own net liquidation at
//     the read beside it (one price basis per rule). A journal holding either
//     version 2 input does not replay under 1.18.0, which refuses it.
//   - 1.20.0 checks every account snapshot after a run's first against the
//     book in force and the fills, dividends and splits journalled since it
//     before it becomes the book (risklimit.Gate.CheckBook,
//     reconcile.Holdings): a holding they do not explain, such as a trade
//     made during the session that the engine did not place, a fill it was
//     never given or a journalled fill IBKR does not show, is journalled as
//     a risk.reconciliation.result (reconciliation ID
//     close-snapshot:<snapshot ID>) and degrades the account on the
//     instrument, where 1.19.0 took the snapshot as the book silently (ADR
//     0019, amended 2026-10-21). A snapshot whose holdings a reconciliation
//     already classified against the same book and the same changes is not
//     classified again, nor is a difference already reported with the same
//     residual, nor is an unverifiable snapshot whose every reason a
//     journalled Halt already gave, and whose unexplained holdings were
//     already reported, while the account stays Halted. A journal holding such a snapshot decides differently under
//     1.19.0.
//   - 1.21.0 reads the Flex reader's journalled deposits and withdrawals
//     (account.cash-movement version 2) and its status (account.flex-status),
//     which the engine delivers from the instance's ledger, and its gate's
//     cash rule changes (ADR 0019, amended 2026-10-21): a reconciliation adds
//     each journalled deposit and subtracts each withdrawal dated after the
//     book to the expected cash, listing them beside the fills; a difference
//     in cash alone on a New York day the reader's status does not cover is
//     a pending cash read (risk.reconciliation.result classified
//     pending-cash-read, action alert), which moves no state; a pending
//     decrease withholds every buy on the account (risk.order.refused,
//     pending-cash-decrease, the first refusal of each pending difference
//     flagged as its one alert), and a pending increase withholds none but
//     is left out of the buy budget until the reader's records explain it;
//     a later reconciliation that finds the same difference against the
//     same book is that difference. It is decided at a later
//     reconciliation: explained once the transfers recorded since account
//     for it, else Degraded on cash once the reader has covered the day or
//     more than two sessions have closed. A journalled withdrawal larger
//     than the cash before it (a transfer dated by day alone counted after
//     that day's fills), or a run with no reader status, degrades as
//     before. The cash-drift bound, 1% of net
//     liquidation, is unchanged. A journal holding a cash difference with a
//     reader's status, or a transfer, decides differently under 1.20.0,
//     which refuses both inputs.
//   - 1.22.0 records, in a dry-run Boost run, the synthetic terminal report
//     a dry-run momentum run has recorded since 1.11.0, for each order it
//     releases, the sale back to 98% included, and ends the order in the
//     gate at its release, so a later exit, the broker-holding check and the
//     sale back to 98% are sized from the account with no unsent sell
//     netted, as paper sizes them (ADR 0026, point 3, "Mode", amended
//     2026-10-22). No dry-run order is watched, since none can execute, so
//     its end re-plans nothing, and a venue's fill or lifecycle report on
//     one is refused, since no venue received it. Where 1.21.0 counted every
//     unsent sell as still being sold and read such reports, a dry-run Boost
//     journal decides differently, so one written under 1.21.0 does not
//     replay. Paper and live decisions are unchanged.
//   - 1.23.0 reads execution.order.retired, the IBKR adapter's retirement
//     of a released order it never sent because its session was over, or
//     the owner's retirement of one the adapter can never send (the owner's
//     decision of 2026-10-22; ADR 0026, point 2, "Boost", amended
//     2026-10-22). The order must be one the run released, as released,
//     that no venue has reported on; it ends with nothing filled, in every
//     strategy. In Boost one awaiting the venue's word leaves those orders,
//     and any other, the sale back to 98% included, is retired by its own
//     rule; a retired sell is no longer counted as being sold, the watched
//     order so retired is re-planned at the next decided session as
//     unfilled, and a retired sale back to 98% spent no attempt, so it is
//     attempted again under its own rule, under a client order ID of its
//     own. Each Boost decision records the retired orders (decision schema
//     7), so a restart refuses a venue report on one as the full run does.
//     A momentum rebalance with a retired sell never starts its buy phase,
//     and once its orders are all terminal it completes. A venue report on
//     a retired order is refused. A dry-run refuses every retirement.
//     1.22.0 refused the input, so a journal holding one decides
//     differently.
const RulesVersion = "1.23.0"

// source names this handler on every envelope it emits.
const source = "target-weight"

// idTime is the fixed timestamp layout of the decision IDs this handler builds.
const idTime = "2006-01-02T15:04:05Z07:00"

// maxVenueRejections is how many of the account's orders the venue may
// reject (invalid) in a row, whatever their targets, before Boost buys
// nothing more until the owner starts a fresh run: a target that changes at
// each session would otherwise give every rejected order its own re-plan.
// A sell, which reduces exposure, is never stopped (ADR 0026, points 2,
// "Boost", amended 2026-10-18, and 4; confirmed by the owner on
// 2026-10-18).
const maxVenueRejections = event.BoostVenueRejectionLimit

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
//     boost, the target in force, any pending re-plan and the watched order
//     from it, never by recomputing (ADR 0026, point 5); sessions up to its session then only feed the indicators,
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
//     lifecycle report is recorded and decides nothing but the re-plan of
//     the watched order (ADR 0022; ADR 0026, point 2, "Boost", amended
//     2026-10-18); one about an order neither this run released nor the
//     recovered decision watches, or that does not describe it as released,
//     is refused: it is reconciliation's to explain (ADR 0019);
//   - after an order's release: the owner's operator.rebalance.cleared
//     naming the decision that released it and that order, which settles an
//     order the adapter never sent: no venue will rule on it, so it is no
//     longer awaited (applyClearance);
//   - after an order's release: the adapter's execution.order.retired, for
//     an order it never sent because its session was over, or the owner's,
//     for one it can never send, which ends the order with nothing filled,
//     so the watched order is re-planned as unfilled (applyRetired);
//   - at any time after the configuration: account.cash-movement version 2
//     and account.flex-status, the Flex reader's journalled transfers and its
//     status, which a reconciliation's cash is bridged by and judged against
//     (ADR 0019, amended 2026-10-21); market.corporate-action, which a
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
// (missing-bars) and is skipped: the indicators continue at the next bar
// over the closes received, and the decision that reads it names the gap
// (ADR 0026, point 5, amended 2026-10-18). A session with no snapshot as of its
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
	// replan is set when the last planned order was refused and the engine
	// stayed Normal, or when the last released order ended with shares
	// unfilled (settleWatch), so the next decided session plans the
	// difference to the target in force again (orderplan.BoostInput.Replan;
	// ADR 0026, point 2, "Boost", amended 2026-10-18). Every decision records
	// it as it stands after that decision (BoostFigures.ReplanPending), so a
	// restart from the decision recovers it.
	replan bool
	// replanCause is why replan is set: a refusal, or the watched order's
	// end with shares unfilled (BoostFigures.Replan).
	replanCause event.BoostReplan
	// watch is the order the latest decided session released, watched until
	// it has filled in full, or has ended and a decided session has planned
	// after its end; nil when there is none. Only a later decided session's
	// released order replaces it, since that order was sized from the
	// account as it stood (settleWatch). A dry-run's orders, which nothing
	// can execute, are never watched (applySession's mode guard); their
	// synthetic report is endUnsent's.
	watch *watched
	// verdicts counts the venue's rejections of the orders decided sessions
	// released, in a row, and holds the orders it has not yet ruled on
	// (BoostVerdicts; BoostFigures.VenueRejections, RejectedInARow,
	// UndecidedOrders, UndecidedBeforeRejection and RejectionsWhileStopped).
	verdicts BoostVerdicts
	// cleared are the orders the owner settled as never sent, in the order
	// cleared (BoostFigures.ClearedOrders): off undecided, but released in
	// the gate still, so a decision records them for a restart to restore.
	cleared []string
	// retired are the orders the adapter or the owner retired as never sent
	// (applyRetired), in the order retired, each with its release and when
	// (BoostFigures.RetiredOrders): ended with nothing filled, so a venue
	// report on one is refused, and a decision records them for a restart to
	// restore.
	retired []event.BoostRetiredOrder
	// named are the orders a venue has named, by client order ID: a lifecycle
	// report, an execution or a broker snapshot's open-order listing, which
	// show the order reached IBKR, so it is never retired.
	named map[string]bool
	// working are the orders the venue accepted that were not fully filled
	// or ended when last read, in the order the venue accepted them, whichever order is watched
	// now (BoostFigures.WorkingOrders): a later order replaces them as the
	// watched one, but the venue's fills and cancel of them are still read.
	working []undecidedOrder
	// withheld is the rejection that brought the count to
	// maxVenueRejections, whichever order it was, held until the next
	// decision, decided or withheld, records it (BoostFigures.ReplanWithheld):
	// a session that only feeds the indicators, as a restarted run's warm-up
	// does, records nothing.
	withheld event.BoostReplan
	// stoppedRejections are the rejections read after that one, held until
	// the next decision records them (BoostFigures.RejectedWhileStopped).
	stoppedRejections []event.BoostReplan
	// gap lists the sessions, YYYY-MM-DD, that closed with no bar since the
	// last bar the indicators read; the next decision that reads one
	// records it (BoostFigures.GapSessions).
	gap        []string
	lastPeriod time.Time // last session closed
	bar        *event.CompletedBarAdjustedPayload
	base       string // the adjusted view's base, fixed by the first bar
	snapshot   *event.AccountPositionsSnapshotPayload
	mark       *event.MarkRecordedPayload
	markAt     uint32 // the schema version the mark arrived at (bandMark)
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
		return h.applySnapshot(in)
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
	case event.CashMovementEventType:
		return nil, h.applyCashMovement(in)
	case event.FlexStatusEventType:
		return nil, h.applyFlexStatus(in)
	case event.AlertAcknowledgedEventType:
		return nil, h.applyAcknowledgement(in)
	case event.RebalanceClearedEventType:
		return nil, h.applyClearance(in)
	case event.OrderRetiredEventType:
		return nil, h.applyRetired(in)
	case event.RunCompletedEventType:
		return nil, nil
	}
	return nil, fmt.Errorf("targetweight: input type %q is not one this handler reads", in.Type)
}

// decodeInput checks an input's schema version and decodes and validates its payload.
func decodeInput[T interface{ Validate() error }](in event.Envelope, version uint32) (T, error) {
	if in.SchemaVersion != version {
		var p T
		return p, fmt.Errorf("targetweight: %s schema version %d is not the version %d this build reads (ADR 0015)", in.Type, in.SchemaVersion, version)
	}
	return decodeAt[T](in)
}

// decodeVersions is decodeInput for an input read at any of versions: the
// broker snapshot and the mark, which the IBKR adapter writes at version 2
// (the owner's decision of 2026-10-18) and earlier journals hold at 1.
func decodeVersions[T interface{ Validate() error }](in event.Envelope, versions []uint32) (T, error) {
	if !slices.Contains(versions, in.SchemaVersion) {
		var p T
		return p, fmt.Errorf("targetweight: %s schema version %d is not one of the versions %v this build reads (ADR 0015)", in.Type, in.SchemaVersion, versions)
	}
	return decodeAt[T](in)
}

// decodeAt decodes and validates an input's payload at the version it arrived at.
func decodeAt[T interface{ Validate() error }](in event.Envelope) (T, error) {
	var p T
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
	// A refusal that left the engine Normal is re-planned at the next decided
	// session, by this run as by the earlier one (ADR 0026, point 2, "Boost",
	// amended 2026-10-18).
	h.replan = p.Boost.ReplanPending
	// The rejections in a row and the orders the venue has not yet ruled on
	// are the decision's, so the restarted run counts as the full run does
	// (ADR 0026, point 5: recovered, never recomputed).
	h.verdicts.Recover(p.Boost)
	// A withheld decision carries the pending re-plan's cause forward. A
	// decided one left a re-plan pending only by its own order's refusal:
	// refused, unless the order refused was a rejection's one re-plan, which
	// stays that re-plan until an order is released, but only while the
	// target is unchanged: an order to a new target is a new order, read by
	// the rule applySession reads it by (ADR 0026, point 2, "Boost", and
	// point 5, amended 2026-10-18). The decision records the target in force
	// before it and the target it decided.
	sameTarget := p.Outcome == event.TargetsOutcomeDecided && p.Boost.TargetInForceSet && p.Boost.TargetInForce == p.Targets[0].Weight
	switch {
	case !h.replan:
	case p.Outcome == event.TargetsOutcomeWithheld && p.Boost.Replan.Cause != "",
		p.Boost.Replan.Cause == event.ReplanCauseRejected && sameTarget:
		h.replanCause = p.Boost.Replan
	default:
		h.replanCause = event.BoostReplan{Cause: event.ReplanCauseRefused}
	}
	// The order the earlier run watched is watched still, from the
	// executions reported before the restart and its end, and the venue's
	// reports on it are read as on an order this run released: an execution
	// the decision holds, sent again, counts once, and a sell is netted as
	// any working sell (ADR 0026, points 2 and 5, amended 2026-10-18; ADR
	// 0019, amended 2026-10-15). It joins the orders this run released, which
	// keep any a broker snapshot read before the recovery released.
	if w := p.Boost.Watched; !w.IsZero() {
		o := event.OrderReleasedPayload{
			AccountID: h.cfg.AccountID, ClientOrderID: w.ClientOrderID, InstrumentID: h.cfg.Boost.InstrumentID,
			Side: w.Side, Quantity: w.Released, LimitPrice: w.LimitPrice,
		}
		fills := make([]event.TargetWeightFillPayload, len(w.Fills))
		for i, f := range w.Fills {
			fills[i] = w.Fill(h.cfg.AccountID, h.cfg.Boost.InstrumentID, f)
		}
		if err := h.gate.RestoreReleased(o, fills, w.EndedAt, w.VenueOrderID); err != nil {
			return fmt.Errorf("targetweight: recovered decision %s: %w", in.ID, err)
		}
		h.release(o)
		h.watch = &watched{order: o, status: w.Status, endedAt: w.EndedAt, afterRejection: w.AfterRejection, venueID: w.VenueOrderID}
	}
	// An undecided order but the watched one is restored as released,
	// latest first, so the gate keeps release order: the venue's reports on
	// it are read, and a sell is netted, as on an order this run released.
	for i := len(p.Boost.UndecidedOrders) - 1; i >= 0; i-- {
		r := p.Boost.UndecidedOrders[i]
		if r.ClientOrderID == p.Boost.Watched.ClientOrderID {
			continue
		}
		o := event.OrderReleasedPayload{
			AccountID: h.cfg.AccountID, ClientOrderID: r.ClientOrderID, InstrumentID: h.cfg.Boost.InstrumentID,
			Side: r.Side, Quantity: r.Released, LimitPrice: r.LimitPrice,
		}
		if err := h.gate.RestoreReleased(o, nil, time.Time{}, r.VenueOrderID); err != nil {
			return fmt.Errorf("targetweight: recovered decision %s: %w", in.ID, err)
		}
		h.release(o)
	}
	// A cleared order stays released, the oldest the gate knows of, so a
	// sell never sent is still counted as sold. The watched one is restored
	// above.
	for _, r := range p.Boost.ClearedOrders {
		if r.ClientOrderID != p.Boost.Watched.ClientOrderID {
			o := event.OrderReleasedPayload{
				AccountID: h.cfg.AccountID, ClientOrderID: r.ClientOrderID, InstrumentID: h.cfg.Boost.InstrumentID,
				Side: r.Side, Quantity: r.Released, LimitPrice: r.LimitPrice,
			}
			if err := h.gate.RestoreReleased(o, nil, time.Time{}, ""); err != nil {
				return fmt.Errorf("targetweight: recovered decision %s: %w", in.ID, err)
			}
			h.release(o)
		}
		h.cleared = append(h.cleared, r.ClientOrderID)
	}
	// A retired order is restored as released and ended at its retirement,
	// so a venue report on it is refused after a restart as in the full run
	// (ADR 0026, point 5: recovered, never recomputed). The watched one is
	// restored above, ended.
	for _, r := range p.Boost.RetiredOrders {
		if r.ClientOrderID != p.Boost.Watched.ClientOrderID {
			o := event.OrderReleasedPayload{
				AccountID: h.cfg.AccountID, ClientOrderID: r.ClientOrderID, InstrumentID: h.cfg.Boost.InstrumentID,
				Side: r.Side, Quantity: r.Released, LimitPrice: r.LimitPrice,
			}
			if err := h.gate.RestoreReleased(o, nil, r.RetiredAt, ""); err != nil {
				return fmt.Errorf("targetweight: recovered decision %s: %w", in.ID, err)
			}
			h.release(o)
		}
		// A retired sale back to 98% counts among its tag's retired
		// attempts, so the next has an ID of its own; one whose ID is not
		// the attempt's it records is refused (ADR 0026, point 2, "Order
		// identity"; fail closed, AGENTS.md).
		if r.SaleTag != "" {
			if want := orderplan.SaleAttemptClientOrderID(r.SaleTag, h.cfg.Boost.InstrumentID, r.SaleAttempt-1); want != r.ClientOrderID {
				return fmt.Errorf("targetweight: recovered decision %s: retired order %s is recorded as attempt %d of %s's sale, whose client order ID is %s", in.ID, r.ClientOrderID, r.SaleAttempt, r.SaleTag, want)
			}
			if err := h.gate.RestoreRetiredSale(r.SaleTag, h.cfg.Boost.InstrumentID, r.SaleAttempt); err != nil {
				return fmt.Errorf("targetweight: recovered decision %s: %w", in.ID, err)
			}
		}
		h.retired = append(h.retired, r)
	}
	// An order a venue named before the decision reached IBKR, so the
	// restarted run refuses its retirement as the full run does (ADR 0026,
	// point 2, "Boost", and point 5, amended 2026-10-22).
	for _, id := range p.Boost.NamedOrders {
		h.name(id)
	}
	// An accepted order still working beside the watched one is restored
	// with its executions, latest first so the gate keeps release order,
	// and the venue's later reports on it are read as on an order this run
	// released.
	for i := len(p.Boost.WorkingOrders) - 1; i >= 0; i-- {
		w := p.Boost.WorkingOrders[i]
		o := event.OrderReleasedPayload{
			AccountID: h.cfg.AccountID, ClientOrderID: w.ClientOrderID, InstrumentID: h.cfg.Boost.InstrumentID,
			Side: w.Side, Quantity: w.Released, LimitPrice: w.LimitPrice,
		}
		fills := make([]event.TargetWeightFillPayload, len(w.Fills))
		for j, f := range w.Fills {
			fills[j] = w.Fill(h.cfg.AccountID, h.cfg.Boost.InstrumentID, f)
		}
		if err := h.gate.RestoreReleased(o, fills, time.Time{}, w.VenueOrderID); err != nil {
			return fmt.Errorf("targetweight: recovered decision %s: %w", in.ID, err)
		}
		h.release(o)
	}
	for _, w := range p.Boost.WorkingOrders {
		h.working = append(h.working, undecidedOrder{id: w.ClientOrderID, venueID: w.VenueOrderID})
	}
	// A watched order the venue accepted and has not ended, neither awaiting
	// its verdict nor cleared, works as it does in the full run, where the
	// acceptance put it among the working orders: it is recorded among them
	// once a later release replaces it as the watched order (ADR 0026, point
	// 5: recovered, never recomputed). No venue receives a dry-run's order.
	if w := p.Boost.Watched; !w.IsZero() && w.Status == "" && p.Boost.NoVenue == "" &&
		!slices.ContainsFunc(p.Boost.UndecidedOrders, func(r event.BoostUndecidedOrder) bool { return r.ClientOrderID == w.ClientOrderID }) &&
		!slices.ContainsFunc(p.Boost.ClearedOrders, func(r event.BoostUndecidedOrder) bool { return r.ClientOrderID == w.ClientOrderID }) {
		h.working = append(h.working, undecidedOrder{id: w.ClientOrderID, venueID: w.VenueOrderID})
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
	// Version 3 adds the starting book, which the IBKR adapter reads (ADR
	// 0027, amended 2026-10-18), and version 4 the incident's retired sale
	// attempts; a version 2 or 3 approval still replays.
	p, err := decodeVersions[event.OperatorRunApprovedPayload](in, []uint32{2, 3, event.OperatorRunApprovedSchemaVersion})
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
		if err := h.gate.RestoreSaleAttempt(incident.IncidentID, *incident.SaleAttempted, incident.SaleRetiredAttempts); err != nil {
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

func (h *Handler) applySnapshot(in event.Envelope) ([]event.Envelope, error) {
	p, err := decodeInput[event.AccountPositionsSnapshotPayload](in, event.AccountSnapshotPositionsVersion)
	if err != nil {
		return nil, err
	}
	if p.AccountID != h.cfg.AccountID {
		return nil, fmt.Errorf("targetweight: snapshot for account %q; this run decides %q", p.AccountID, h.cfg.AccountID)
	}
	if h.snapshot != nil && !p.AsOf.After(h.snapshot.AsOf) {
		return nil, fmt.Errorf("targetweight: snapshot as of %s does not follow the last, %s", p.AsOf, h.snapshot.AsOf)
	}
	h.snapshot = &p
	h.gate.ObserveAccount(p)
	return h.checkBook(in, reconcile.PositionsBook(in.ID, p), p.AsOf)
}

// checkBook checks book, the account snapshot in, against the book in force
// and the fills journalled since it before it becomes the book
// (risklimit.Gate.CheckBook), and journals the check's result, then the
// engine state when it moved, timed at the snapshot, when there is one to
// journal: a holding no fill explains is a difference (ADR 0019, amended
// 2026-10-21).
func (h *Handler) checkBook(in event.Envelope, book reconcile.Book, at time.Time) ([]event.Envelope, error) {
	r, journal := h.gate.CheckBook(book)
	if !journal {
		return nil, nil
	}
	b := &emissions{h: h, in: in, at: at}
	b.add(r.ResultID, event.ReconciliationResultEventType, event.ReconciliationResultSchemaVersion, r.Result)
	if r.State != nil {
		b.add(r.StateID, event.EngineStateEventType, r.StateSchemaVersion, *r.State)
	}
	return b.result()
}

func (h *Handler) applyMark(in event.Envelope) error {
	p, err := decodeVersions[event.MarkRecordedPayload](in, event.MarkRecordedVersions())
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
	h.markAt = in.SchemaVersion
	return nil
}

// bandMark is the price the gate's price band compares a checked order's
// reference price with, from a mark recorded at schema version, and the
// source the order's figures name: IBKR's last price when a version-2 mark
// carries one, so the band checks the reference price against a price of
// its own, else the mark, the official close; a version-1 mark names no
// source, so its figures keep their bytes. One rule for Boost's and
// momentum's orders alike (ADR 0026 point 3, amended 2026-10-18).
func bandMark(p event.MarkRecordedPayload, version uint32) (price float64, source string) {
	switch {
	case version != event.MarkRecordedEvidenceVersion:
		return p.Price, ""
	case p.BrokerLastSet:
		return p.BrokerLast, event.MarkSourceBrokerLast
	}
	return p.Price, event.MarkSourceOfficialClose
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
	h.settleWatch()

	quiet := p.PeriodEnd.Before(h.decideFrom) || !p.PeriodEnd.After(h.quietUntil)
	b := &emissions{h: h, in: in, at: p.PeriodEnd}
	// A session with no bar is skipped: the indicators and the latch do not
	// move, nothing is traded, and the next bar continues the series (ADR
	// 0026, point 5, amended 2026-10-18, the owner's choice for a data
	// failure). No close is filled in for it, as LEAN's fill-forward would.
	if bar == nil {
		h.gap = append(h.gap, p.PeriodEnd.UTC().Format(time.DateOnly))
		if quiet {
			return nil, nil
		}
		figures := h.figures(boost.Readings{}, h.latched)
		h.recordRejections(&figures)
		b.targets(figures, event.WithheldMissingBars,
			fmt.Sprintf("no %s bar in the session closing %s; Boost skips the session and trades nothing, and decides again at the next session with a bar, its indicators continuing over the closes received", h.cfg.Boost.InstrumentID, p.PeriodEnd.Format(idTime)))
		return b.result()
	}

	// RSI(2) and the means read the split- and dividend-adjusted close, as
	// the backtest's adjusted series does (ADR 0026, point 1).
	readings := h.indicators.Add(bar.Adjusted.Close)
	gap := h.gap
	h.gap = nil
	if quiet {
		return nil, nil
	}
	before := h.latched
	h.latched = h.params.Next(before, readings)
	figures := h.figures(readings, before)
	figures.GapSessions = gap
	h.recordRejections(&figures)
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
	at := len(b.out) - 1
	sameTarget := h.inForceSet && h.inForce == decided.Targets[0].Weight
	h.inForce, h.inForceSet = decided.Targets[0].Weight, true
	h.gate.RecordDecision(decisionID)

	// The planner and the gate cannot refuse what this handler has validated
	// (the decision just built, a validated snapshot, the bar's raw close and
	// the configured band and limits), so an error from either is a defect.
	order, ok, err := orderplan.PlanBoost(orderplan.BoostInput{
		Tag: decisionID, Decision: decided, Snapshot: *snapshot,
		ReferencePrice: bar.Raw.Close, PriceBand: h.cfg.Limits.PriceBand,
		WorkingSells: h.gate.PendingSells(h.cfg.Boost.InstrumentID),
		Replan:       h.replan,
	})
	b.fail(err)
	// Two of the account's orders rejected in a row, whatever their targets,
	// stop Boost's buys until a fresh run, started from the owner's approval,
	// which carries no count (ADR 0026, point 2, "Boost", amended 2026-10-18;
	// ADR 0027). A sell reduces exposure, so it still goes: Boost's exit, and
	// a re-plan of one (ADR 0026, point 4's exemption). The decision records
	// the count, so a restart from it stops too.
	if ok && h.verdicts.Stopped() && order.Side != event.OrderSideSell {
		ok = false
	}
	// A rejection's one re-plan is marked on the order it releases. The mark
	// holds only while the target is unchanged: an order to a new target is
	// a new order, not the rejection's re-plan, though a rejection of it is
	// still the second in a row (settleWatch; ADR 0026, point 2, "Boost",
	// amended 2026-10-18).
	afterRejection := h.replan && h.replanCause.Cause == event.ReplanCauseRejected && sameTarget
	rejection := h.replanCause
	h.replan, h.replanCause = false, event.BoostReplan{}
	// A watched order that has ended is settled by this decision, which
	// planned from the account as it now stands, its re-plan included; one
	// still working stays watched until this session's own order, if any,
	// replaces it.
	if h.watch != nil && h.watch.status != "" {
		h.watch = nil
	}
	if !ok || order.Side != event.OrderSideSell {
		// The decision no longer plans the sell that fired a withheld sale.
		if l := h.gate.LapseExitSale(); l != nil {
			b.lapsed(*l)
		}
	}
	if !ok {
		b.settleReplan(at, decided)
		return b.result()
	}
	gateOrder := risklimit.Order{Planned: order}
	if h.mark != nil && !h.mark.AsOf.Before(p.PeriodEnd) {
		gateOrder.MarkSet = true
		gateOrder.Mark, gateOrder.MarkSource = bandMark(*h.mark, h.markAt)
	}
	book := risklimit.Book{Holdings: holdings(snapshot.Positions), Prices: map[string]float64{h.cfg.Boost.InstrumentID: bar.Raw.Close}, At: b.at}
	outcome := b.order(gateOrder, book)
	// A refusal that leaves the engine Normal is re-planned at the next
	// decision (ADR 0026, point 2, "Boost", amended 2026-10-18); one under
	// Degraded or Halted follows that state's own rules.
	h.replan = outcome.Refused != nil && h.gate.State() == risklimit.StateNormal
	switch {
	case !h.replan:
	case afterRejection:
		// A rejection's one re-plan the gate refused is still that re-plan
		// until an order is released, so a rejection of the order released
		// later is its second (ADR 0026, point 2, "Boost", amended
		// 2026-10-18).
		h.replanCause = rejection
	default:
		h.replanCause = event.BoostReplan{Cause: event.ReplanCauseRefused}
	}
	// No venue receives a dry-run's order, so none will ever rule on it or
	// execute it: its synthetic report ended it at its release (endUnsent),
	// it is never watched, so its end re-plans nothing, and never listed as
	// awaiting a verdict, and the decision records why (BoostFigures.NoVenue;
	// ADR 0026, point 3, "Mode").
	if outcome.Released != nil && h.cfg.Mode != event.TradingModeDryRun {
		h.unwatch()
		h.watch = &watched{order: *outcome.Released, afterRejection: afterRejection}
		h.verdicts.Release(outcome.Released.ClientOrderID)
	}
	if order.Side == event.OrderSideSell && outcome.Refused != nil {
		b.sale(event.SaleTriggerExit)
	}
	b.settleReplan(at, decided)
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
	if err := h.refuseDryRunReport(in, p.ClientOrderID); err != nil {
		return err
	}
	if slices.Contains(h.cleared, p.ClientOrderID) {
		return h.contradictsClearance(in, p.ClientOrderID)
	}
	if h.isRetired(p.ClientOrderID) {
		return h.contradictsRetirement(in, p.ClientOrderID)
	}
	if err := h.gate.ObserveFill(p); err != nil {
		return fmt.Errorf("targetweight: %s %s: %w", in.Type, in.ID, err)
	}
	h.name(p.ClientOrderID)
	// An execution shows the venue accepted the order.
	h.observeOutcome(p.ClientOrderID, false)
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
	// A pending cash difference this reconciliation decided is journalled
	// before its own result, each with the state it moved (ADR 0019,
	// amended 2026-10-21).
	for _, settled := range append(r.Settled, r) {
		b.add(settled.ResultID, event.ReconciliationResultEventType, event.ReconciliationResultSchemaVersion, settled.Result)
		if settled.State != nil {
			b.add(settled.StateID, event.EngineStateEventType, settled.StateSchemaVersion, *settled.State)
		}
	}
	return b.result()
}

// applyCashMovement records a deposit or withdrawal the Flex reader
// journalled, account.cash-movement version 2, as a change a
// reconciliation's cash is bridged by (risklimit.Gate.ObserveCashMovement).
// It changes no target or order: the next decision sizes from the account
// snapshot, which holds the cash IBKR shows (ADR 0026, point 2).
func (h *Handler) applyCashMovement(in event.Envelope) error {
	p, err := decodeInput[event.CashMovementPayload](in, event.CashMovementTransferVersion)
	if err != nil {
		return err
	}
	if err := h.gate.ObserveCashMovement(in.ID, p); err != nil {
		return fmt.Errorf("targetweight: %s %s: %w", in.Type, in.ID, err)
	}
	return nil
}

// applyFlexStatus records the Flex reader's status, which tells a
// reconciliation whether a cash difference's day has been read
// (risklimit.Gate.ObserveFlexStatus). It decides nothing.
func (h *Handler) applyFlexStatus(in event.Envelope) error {
	p, err := decodeInput[event.FlexStatusPayload](in, event.FlexStatusSchemaVersion)
	if err != nil {
		return err
	}
	if err := h.gate.ObserveFlexStatus(p); err != nil {
		return fmt.Errorf("targetweight: %s %s: %w", in.Type, in.ID, err)
	}
	return nil
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
	p, err := decodeVersions[event.BrokerSnapshotPayload](in, event.BrokerSnapshotVersions())
	if err != nil {
		return nil, err
	}
	if err := h.refuseSharedVenue(in, p.OpenOrders); err != nil {
		return nil, err
	}
	obs, err := h.gate.ObserveBroker(in.ID, p)
	if err != nil {
		return nil, fmt.Errorf("targetweight: %s %s: %w", in.Type, in.ID, err)
	}
	h.learnListings(p.OpenOrders)
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

// learnListings reads the open orders IBKR lists: one listed under the
// client order ID of an order this run released reached IBKR, as a
// lifecycle report naming it would show, so its broker order ID is its
// venue order ID (learnVenueID), and the owner's clearance of it as never
// sent is refused (applyClearance).
func (h *Handler) learnListings(listed []event.BrokerOrder) {
	for _, l := range listed {
		o, ok := h.released[l.ClientOrderID]
		if !ok || l.ClientOrderID == "" || o.InstrumentID != l.InstrumentID {
			continue
		}
		h.name(l.ClientOrderID)
		h.learnVenueID(l.ClientOrderID, l.BrokerOrderID)
	}
}

// learnVenueID records venueID as the venue order ID of order id, once a
// lifecycle report or a broker snapshot's open-order listing names it, on
// every list the order is on that does not know one yet: the watched
// order, the orders awaiting the venue's verdict and the working orders.
// An order accepted before any report named it, by an execution, so
// records the ID once it is known, and a run restarted from the decision
// matches IBKR's listing of it with no client order ID echoed by that ID
// (ADR 0026, point 5, amended 2026-10-18).
func (h *Handler) learnVenueID(id, venueID string) {
	if w := h.watch; w != nil && w.order.ClientOrderID == id && w.venueID == "" {
		w.venueID = venueID
	}
	for _, list := range [][]undecidedOrder{h.verdicts.undecided, h.working} {
		for i := range list {
			if r := &list[i]; r.id == id && r.venueID == "" {
				r.venueID = venueID
			}
		}
	}
}

// refuseVenue refuses record in when it would give order id the venue order
// ID venueID that another order the run lists (the watched order, those
// awaiting the venue's verdict and the working ones) already carries: a
// venue order ID names one order, and the failure is the input's, refused
// when it arrives rather than found when the decision is written (ADR 0026,
// point 5, amended 2026-10-18; fail closed, AGENTS.md). venueID is never
// empty and one input never gives two orders the same ID: event validation
// requires a venue report's order ID and each listing's broker order ID,
// and lists the latter strictly ascending.
func (h *Handler) refuseVenue(in event.Envelope, id, venueID string) error {
	carried := func(other, otherVenue string) error {
		if otherVenue == venueID && other != id {
			return fmt.Errorf("targetweight: %s %s gives order %s venue order %s, which order %s already carries; a venue order ID names one order", in.Type, in.ID, id, venueID, other)
		}
		return nil
	}
	if w := h.watch; w != nil {
		if err := carried(w.order.ClientOrderID, w.venueID); err != nil {
			return err
		}
	}
	for _, list := range [][]undecidedOrder{h.verdicts.undecided, h.working} {
		for _, r := range list {
			if err := carried(r.id, r.venueID); err != nil {
				return err
			}
		}
	}
	return nil
}

// refuseSharedVenue refuses a broker snapshot that lists an order this run
// released, under its client order ID, with a broker order ID another order
// carries as its venue order ID (refuseVenue).
func (h *Handler) refuseSharedVenue(in event.Envelope, listed []event.BrokerOrder) error {
	for _, l := range listed {
		if o, ok := h.released[l.ClientOrderID]; ok && l.ClientOrderID != "" && o.InstrumentID == l.InstrumentID {
			if err := h.refuseVenue(in, l.ClientOrderID, l.BrokerOrderID); err != nil {
				return err
			}
		}
	}
	return nil
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
	if err := h.refuseDryRunReport(in, p.ClientOrderID); err != nil {
		return err
	}
	o, ok := h.released[p.ClientOrderID]
	if !ok {
		return fmt.Errorf("targetweight: lifecycle %s reports order %s, which this run did not release", in.ID, p.ClientOrderID)
	}
	if slices.Contains(h.cleared, p.ClientOrderID) {
		return h.contradictsClearance(in, p.ClientOrderID)
	}
	if h.isRetired(p.ClientOrderID) {
		return h.contradictsRetirement(in, p.ClientOrderID)
	}
	if !p.Describes(o) {
		return fmt.Errorf("targetweight: lifecycle %s reports %s %s %d %s at %v; order %s was released as %s %s %d %s at %v",
			in.ID, p.AccountID, p.Side, p.Quantity, p.InstrumentID, p.LimitPrice, p.ClientOrderID, o.AccountID, o.Side, o.Quantity, o.InstrumentID, o.LimitPrice)
	}
	if err := h.refuseVenue(in, p.ClientOrderID, p.OrderID); err != nil {
		return err
	}
	if err := h.gate.ObserveLifecycle(p); err != nil {
		return fmt.Errorf("targetweight: %s %s: %w", in.Type, in.ID, err)
	}
	h.name(p.ClientOrderID)
	h.learnVenueID(p.ClientOrderID, p.OrderID)
	if w := h.watch; w != nil && w.order.ClientOrderID == p.ClientOrderID && p.Terminal() && w.status == "" {
		w.status, w.endedAt = p.Status, p.OccurredAt.UTC()
	}
	// A rejection is counted among the account's rejections in a row. Any
	// report but submitted shows the venue accepted the order, whether or
	// not it is still the watched one: submitted is not enough, as IBKR's
	// PreSubmitted maps to it and can precede a rejection.
	if rejected, decides := BoostLifecycleWord(p.Status); decides {
		h.observeOutcome(p.ClientOrderID, rejected)
	}
	// An order the venue ended is no longer working.
	if p.Terminal() {
		h.working = slices.DeleteFunc(h.working, func(o undecidedOrder) bool { return o.id == p.ClientOrderID })
	}
	return nil
}

// refuseDryRunReport refuses, in a dry-run, a venue's fill or lifecycle
// report on order id, before anything is read from it. A dry-run sends no
// order and ends each it releases with its own synthetic cancel
// (endUnsent), so no venue can report on one: the report is a defect, and
// reading it would count an execution no account holds (ADR 0026, point 3,
// "Mode", amended 2026-10-22; fail closed, AGENTS.md).
func (h *Handler) refuseDryRunReport(in event.Envelope, id string) error {
	if h.cfg.Mode != event.TradingModeDryRun {
		return nil
	}
	if _, ok := h.released[id]; ok {
		return fmt.Errorf("targetweight: %s %s reports order %s, which this dry-run released with send false and ended with its synthetic cancel; no venue received it, so a venue report on it is a defect: find what fed it before going on", in.Type, in.ID, id)
	}
	return fmt.Errorf("targetweight: %s %s reports order %s in a dry-run, which sends no order, so no venue can report on one: find what fed it before going on", in.Type, in.ID, id)
}

// contradictsClearance refuses a venue report on an order the owner
// cleared as never sent: a report proves the order reached IBKR, so the
// clearance was wrong, and the run stops for a person to reconcile before
// it goes on (ADR 0026, point 2, "Boost", amended 2026-10-18; fail closed,
// AGENTS.md).
func (h *Handler) contradictsClearance(in event.Envelope, id string) error {
	return fmt.Errorf("targetweight: %s %s reports order %s, which the owner cleared as never sent; the report shows it reached IBKR, so the clearance was wrong: reconcile the account with IBKR and start a fresh run", in.Type, in.ID, id)
}

// contradictsRetirement refuses a venue report on an order retired as never
// sent: a report proves the order reached IBKR, so the retirement was wrong,
// and the run stops for a person to reconcile before it goes on (the owner's
// decision of 2026-10-22; ADR 0026, point 2, "Boost", amended 2026-10-22;
// fail closed, AGENTS.md).
func (h *Handler) contradictsRetirement(in event.Envelope, id string) error {
	return fmt.Errorf("targetweight: %s %s reports order %s, which was retired as never sent; the report shows it reached IBKR, so the retirement was wrong: reconcile the account with IBKR and start a fresh run", in.Type, in.ID, id)
}

// undecidedOrder is an order a decided session released that the venue has
// not yet ruled on, or one the venue accepted that still works, by client
// order ID, and its venue order ID once a lifecycle report or a broker
// snapshot's open-order listing named it (learnVenueID).
type undecidedOrder struct {
	id, venueID string
}

// observeOutcome reads the venue's first word on order id by Boost's count
// (BoostVerdicts.Observe; ADR 0026, point 2, "Boost", amended 2026-10-18).
// An order accepted works until it is filled or ended. The rejection that
// brings the count to maxVenueRejections is held for the next decision to
// record and the reporter to page, whichever order it was and whether or
// not it is still watched; once buys are stopped each rejection read is
// held so too.
func (h *Handler) observeOutcome(id string, rejected bool) {
	o, verdict := h.verdicts.observe(id, rejected)
	if verdict == VerdictNone {
		return
	}
	if verdict == VerdictAccepted {
		// Accepted, it works until it is filled or ended, whether or not it
		// is the watched order when a later one replaces it.
		h.working = append(h.working, o)
		return
	}
	record := event.BoostReplan{Cause: event.ReplanCauseRejected, ClientOrderID: id, Filled: h.gate.Filled(id), Released: h.released[id].Quantity}
	switch {
	case verdict == VerdictRejectedWhileStopped:
		h.stoppedRejections = append(h.stoppedRejections, record)
	case h.verdicts.Stopped():
		h.withheld = record
	}
}

// applyClearance reads the owner's settlement of a Boost order the adapter
// never sent (operator.rebalance.cleared, recorded after relay.py resolve
// --never-sent): no venue received it, so none will ever rule on it, and it
// leaves the orders awaiting the venue's word, counted neither accepted nor
// rejected (ADR 0026, point 2, "Boost", amended 2026-10-18). It must name
// the decided session's decision and its one order released, still
// undecided and never named by the venue, else it is refused, so a
// clearance never means more than the owner said (fail closed): an order
// the venue has named, even only as submitted, was sent, and its verdict is
// still to come. Nothing else changes: the order stays released in the
// gate, so a never-sent sell still counts as being sold and a never-sent buy
// is not planned again, until the fresh run the owner starts after
// reconciling; the decision records it among the cleared orders so a restart
// restores it.
func (h *Handler) applyClearance(in event.Envelope) error {
	p, err := decodeInput[event.RebalanceClearedPayload](in, event.RebalanceClearedSchemaVersion)
	if err != nil {
		return err
	}
	if p.AccountID != h.cfg.AccountID {
		return fmt.Errorf("targetweight: clearance %s is for account %q; this run decides %q", in.ID, p.AccountID, h.cfg.AccountID)
	}
	sideOf := func(id string) string { return h.released[id].Side }
	if err := h.verdicts.ClearNeverSent(in.ID, p, h.cfg.Boost.InstrumentID, sideOf); err != nil {
		return err
	}
	h.cleared = append(h.cleared, p.ClientOrderIDs...)
	return nil
}

// applyRetired reads the retirement of a Boost order the adapter never sent
// (execution.order.retired): its session ended before the adapter could
// send it, or the owner retired an order the adapter can never send (the
// owner's decision of 2026-10-22; ADR 0026, point 2, "Boost", amended
// 2026-10-22). No venue received it, so none will ever rule on it, and it
// ends with nothing filled, so the gate no longer counts a retired sell as
// being sold (ADR 0019, amended 2026-10-15, "net of every working sell").
// It must name an order of this account that this run released, as
// released (event's Describes), neither cleared nor retired before. An
// order among those awaiting the venue's word leaves them by that count's
// rule (BoostVerdicts.Retire), counted neither accepted nor rejected; any
// other, the sale back to 98% and any order released outside a decided
// session among them, by its own: no venue may have named it or reported an
// execution of it. A retired sale back to 98% spent no attempt, so it is
// attempted again under its own rule at the next opportunity
// (risklimit.Gate.RetireSale). The watched order so retired ends as
// canceled at its retirement, so the session's close re-plans it as
// unfilled (settleWatch), as an order that expired unfilled is. Anything
// else is refused, so a retirement never ends an order that may be working
// (fail closed). A dry-run sends nothing, so it refuses every retirement.
func (h *Handler) applyRetired(in event.Envelope) error {
	p, err := decodeInput[event.OrderRetiredPayload](in, event.OrderRetiredSchemaVersion)
	if err != nil {
		return err
	}
	id := p.ClientOrderID
	if h.cfg.Mode == event.TradingModeDryRun {
		return fmt.Errorf("targetweight: retirement %s names order %s in a dry-run, which sends no order and ends each it releases with its own synthetic cancel, so it has nothing to retire: find what fed it before going on", in.ID, id)
	}
	if p.AccountID != h.cfg.AccountID {
		return fmt.Errorf("targetweight: retirement %s is for account %q; this run decides %q", in.ID, p.AccountID, h.cfg.AccountID)
	}
	o, ok := h.released[id]
	switch {
	case !ok:
		return fmt.Errorf("targetweight: retirement %s names order %s, which this run did not release", in.ID, id)
	case slices.Contains(h.cleared, id):
		return fmt.Errorf("targetweight: retirement %s names order %s, which the owner cleared as never sent already", in.ID, id)
	case h.isRetired(id):
		return fmt.Errorf("targetweight: retirement %s names order %s, which is retired already", in.ID, id)
	case !p.Describes(o):
		return fmt.Errorf("targetweight: retirement %s retires %s %s %d %s at %v; order %s was released as %s %s %d %s at %v",
			in.ID, p.AccountID, p.Side, p.Quantity, p.InstrumentID, p.LimitPrice, id, o.AccountID, o.Side, o.Quantity, o.InstrumentID, o.LimitPrice)
	}
	held := h.verdicts.Holds(id)
	switch {
	case held:
		if err := h.verdicts.Retire(in.ID, id); err != nil {
			return err
		}
	case h.named[id]:
		return fmt.Errorf("targetweight: retirement %s names order %s, which a venue has named or reported executed; it was sent, and only an order no venue received is retired", in.ID, id)
	}
	// The gate ends the order as a cancellation with nothing filled would.
	// The report is the engine's own and is never journalled: it is marked
	// synthetic and names no venue order, so the gate cannot take it for a
	// venue's (LimitOrderLifecyclePayload.Synthetic).
	end := event.LimitOrderLifecyclePayload{
		AccountID: o.AccountID, ClientOrderID: id, InstrumentID: o.InstrumentID, Side: o.Side,
		Status: event.OrderLifecycleStatusCanceled, Quantity: o.Quantity, LimitPrice: o.LimitPrice,
		OccurredAt: p.RetiredAt, Synthetic: true, Message: "retired unsent: " + p.Reason,
	}
	if err := h.gate.ObserveLifecycle(end); err != nil {
		return fmt.Errorf("targetweight: %s %s: %w", in.Type, in.ID, err)
	}
	r := event.BoostRetiredOrder{ClientOrderID: id, Side: o.Side, Released: o.Quantity, LimitPrice: o.LimitPrice, RetiredAt: p.RetiredAt.UTC()}
	if !held {
		// A retired sale back to 98% is recorded with its tag and the
		// attempt retired, so a run restarted from the decision gives the
		// next attempt an ID of its own (ADR 0026, point 2, "Order
		// identity", and point 5).
		if tag, attempt, sale := h.gate.RetireSale(id); sale {
			r.SaleTag, r.SaleAttempt = tag, attempt
		}
	}
	h.retired = append(h.retired, r)
	if w := h.watch; w != nil && w.order.ClientOrderID == id && w.status == "" {
		w.status, w.endedAt = event.OrderLifecycleStatusCanceled, p.RetiredAt.UTC()
	}
	return nil
}

// isRetired reports whether order id was retired as never sent.
func (h *Handler) isRetired(id string) bool {
	return slices.ContainsFunc(h.retired, func(r event.BoostRetiredOrder) bool { return r.ClientOrderID == id })
}

// name records that a venue named order id.
func (h *Handler) name(id string) {
	if h.named == nil {
		h.named = map[string]bool{}
	}
	h.named[id] = true
}

// recordRejections moves the rejections held for the next decision into
// figures: the one that stopped buys and those read after it.
func (h *Handler) recordRejections(figures *event.BoostFigures) {
	figures.ReplanWithheld, h.withheld = h.withheld, event.BoostReplan{}
	figures.RejectedWhileStopped, h.stoppedRejections = h.stoppedRejections, nil
}

// unwatch moves the watched order, replaced by a later release, to the end
// of the working orders when the venue accepted it and it still works, so
// the working orders a decision records are in the same order whether the
// run accepted it itself or restored it as accepted from a decision (ADR
// 0026, point 5: recovered, never recomputed).
func (h *Handler) unwatch() {
	if h.watch == nil {
		return
	}
	i := slices.IndexFunc(h.working, func(o undecidedOrder) bool { return o.id == h.watch.order.ClientOrderID })
	if i < 0 {
		return
	}
	o := h.working[i]
	h.working = append(slices.Delete(h.working, i, i+1), o)
}

// watched is the order the latest decided session released, and its
// status once the venue ended it, canceled (a DAY order's expiry included)
// or invalid, and when that end occurred; empty while it may still execute.
// afterRejection marks the one re-plan of an order the venue rejected;
// venueID is the venue's own ID for it, from the first lifecycle report or
// broker snapshot's open-order listing that names it (learnVenueID).
type watched struct {
	order          event.OrderReleasedPayload
	status         string
	endedAt        time.Time
	afterRejection bool
	venueID        string
}

// settleWatch reads the watched order as a session closes (ADR 0026, point
// 2, "Boost", amended 2026-10-18). Filled in full, it is no longer watched
// and a re-plan it set is cleared: re-planning would trade drift toward an
// unchanged target. Ended with fewer shares filled than released, it sets
// the re-plan the next decided session plans, part-filled or unfilled, with
// its shares filled as they now stand, so a fill the venue reports after
// the cancellation still counts at every close until that session; a
// refusal's re-plan already pending plans the whole difference, this
// order's remainder included, and keeps its cause.
//
// An order the venue rejected (invalid) may have been refused for a reason
// that lasts, so it is re-planned once, cause rejected, as a refusal is.
// When the venue has rejected maxVenueRejections of the account's orders
// in a row, whatever their targets (that re-planned order, or an order to a
// new target), a rejected buy is not planned again, and no session buys
// until the owner starts a fresh run; observeOutcome holds the rejection for
// the next decision to record and the reporter to page. A refusal's re-plan
// already pending is dropped. A rejected sell reduces exposure, so it is
// re-planned at every decided session while it is due (ADR 0026, point 4's
// exemption).
func (h *Handler) settleWatch() {
	w := h.watch
	if w == nil {
		return
	}
	id, released := w.order.ClientOrderID, w.order.Quantity
	filled := h.gate.Filled(id)
	own := h.replanCause.ClientOrderID == id
	switch {
	case filled >= released:
		if own {
			h.replan, h.replanCause = false, event.BoostReplan{}
		}
		h.watch = nil
	case w.status == event.OrderLifecycleStatusInvalid && h.verdicts.Stopped():
		// A rejection's one re-plan is always the second rejection in a row,
		// so this bound holds it too.
		h.watch = nil
		h.replan, h.replanCause = false, event.BoostReplan{}
		if w.order.Side == event.OrderSideSell {
			h.replan, h.replanCause = true, event.BoostReplan{Cause: event.ReplanCauseRejected, ClientOrderID: id, Filled: filled, Released: released}
		}
	case w.status == "", h.replan && !own:
	default:
		cause := event.ReplanCausePartFilled
		switch {
		case w.status == event.OrderLifecycleStatusInvalid:
			cause = event.ReplanCauseRejected
		case filled == 0:
			cause = event.ReplanCauseUnfilled
		}
		h.replan = true
		h.replanCause = event.BoostReplan{Cause: cause, ClientOrderID: id, Filled: filled, Released: released}
	}
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
	b := event.BoostFigures{
		InstrumentID: h.cfg.Boost.InstrumentID, Close: r.Close,
		RSI: r.RSI, RSIReady: r.RSIReady, TrendMean: r.TrendMean, TrendMeanReady: r.TrendMeanReady,
		ExitMean: r.ExitMean, ExitMeanReady: r.ExitMeanReady,
		LatchedBefore: latchedBefore, LatchedAfter: h.latched,
		TargetInForce: h.inForce, TargetInForceSet: h.inForceSet,
		ReplanPending: h.replan, Replan: h.replanCause, Watched: h.watchedFigures(), WorkingOrders: h.workingFigures(),
		VenueRejections: h.verdicts.rejections, RejectedInARow: slices.Clone(h.verdicts.inARow), UndecidedOrders: h.undecidedFigures(), ClearedOrders: h.clearedFigures(), RetiredOrders: slices.Clone(h.retired),
		UndecidedBeforeRejection: h.verdicts.olderThanRejection, RejectionsWhileStopped: h.verdicts.whileStopped, NoVenue: h.noVenue(),
	}
	b.NamedOrders = h.namedFigures(b)
	return b
}

// namedFigures are the orders b restores as released, the watched, the
// undecided and the working ones, that a venue has named, ascending
// (BoostFigures.NamedOrders), so a run restarted from the decision refuses
// their retirement as the full run does (ADR 0026, point 5).
func (h *Handler) namedFigures(b event.BoostFigures) []string {
	var ids []string
	add := func(id string) {
		if id != "" && h.named[id] && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	add(b.Watched.ClientOrderID)
	for _, o := range b.UndecidedOrders {
		add(o.ClientOrderID)
	}
	for _, o := range b.WorkingOrders {
		add(o.ClientOrderID)
	}
	slices.Sort(ids)
	return ids
}

// noVenue is the run's mode when no venue receives its orders, dry-run, so
// none is listed as awaiting a verdict; empty otherwise.
func (h *Handler) noVenue() string {
	if h.cfg.Mode == event.TradingModeDryRun {
		return h.cfg.Mode
	}
	return ""
}

// undecidedFigures are the orders the venue has not yet ruled on as a
// decision records them, so a run restarted from the decision counts a late
// verdict on any of them as this one does (ADR 0026, point 5): each but the
// watched order with its release and its venue order ID.
func (h *Handler) undecidedFigures() []event.BoostUndecidedOrder {
	var out []event.BoostUndecidedOrder
	for _, r := range h.verdicts.undecided {
		o := event.BoostUndecidedOrder{ClientOrderID: r.id}
		if h.watch == nil || h.watch.order.ClientOrderID != r.id {
			released := h.released[r.id]
			o.Side, o.Released, o.LimitPrice, o.VenueOrderID = released.Side, released.Quantity, released.LimitPrice, r.venueID
		}
		out = append(out, o)
	}
	return out
}

// clearedFigures are the orders the owner settled as never sent as a
// decision records them, each with its release (ADR 0026, point 5).
func (h *Handler) clearedFigures() []event.BoostUndecidedOrder {
	var out []event.BoostUndecidedOrder
	for _, id := range h.cleared {
		o := h.released[id]
		out = append(out, event.BoostUndecidedOrder{ClientOrderID: id, Side: o.Side, Released: o.Quantity, LimitPrice: o.LimitPrice})
	}
	return out
}

// workingFigures are the orders the venue accepted that still work besides
// the watched one, as a decision records them, so a run restarted from the
// decision reads the venue's later reports on them (ADR 0026, point 5). An
// order filled in full leaves the list for good.
func (h *Handler) workingFigures() []event.BoostWatchedOrder {
	h.working = slices.DeleteFunc(h.working, func(o undecidedOrder) bool { return h.gate.Filled(o.id) >= h.released[o.id].Quantity })
	var out []event.BoostWatchedOrder
	for _, o := range h.working {
		if h.watch != nil && h.watch.order.ClientOrderID == o.id {
			continue
		}
		out = append(out, h.orderFigures(h.released[o.id], o.venueID, "", time.Time{}, false))
	}
	return out
}

// watchedFigures is the watched order as a decision records it, so a run
// restarted from the decision watches it still (ADR 0026, point 5): its
// identity as released, its shares filled so far and each execution, and
// its end once reported.
func (h *Handler) watchedFigures() event.BoostWatchedOrder {
	w := h.watch
	if w == nil {
		return event.BoostWatchedOrder{}
	}
	return h.orderFigures(w.order, w.venueID, w.status, w.endedAt, w.afterRejection)
}

// orderFigures records the released order o with the executions the gate
// holds of it, as a decision states a watched or working order.
func (h *Handler) orderFigures(o event.OrderReleasedPayload, venueID, status string, endedAt time.Time, afterRejection bool) event.BoostWatchedOrder {
	var fills []event.BoostWatchedFill
	for _, f := range h.gate.Fills(o.ClientOrderID) {
		fills = append(fills, event.BoostWatchedFill{FillID: f.FillID, Quantity: f.Quantity, Price: f.Price, Commission: f.Commission, FilledAt: f.FilledAt.UTC()})
	}
	return event.BoostWatchedOrder{
		ClientOrderID: o.ClientOrderID, Side: o.Side, Released: o.Quantity, LimitPrice: o.LimitPrice,
		Filled: h.gate.Filled(o.ClientOrderID), Fills: fills, Status: status, EndedAt: endedAt, AfterRejection: afterRejection,
		VenueOrderID: venueID,
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

// settleReplan rewrites the decided session's decision, emitted at index i,
// with the re-plan and the watched order as the session's order left them:
// a withheld session leaves them as they were, which figures already
// records, but a decided one plans, clearing the re-plan or, on a refusal
// that leaves the engine Normal, setting it, and watches the order it
// released.
func (b *emissions) settleReplan(i int, decided event.TargetsDecidedPayload) {
	watched, undecided, working := b.h.watchedFigures(), b.h.undecidedFigures(), b.h.workingFigures()
	if decided.Boost.ReplanPending == b.h.replan && decided.Boost.Watched.Equal(watched) && slices.Equal(decided.Boost.UndecidedOrders, undecided) &&
		slices.EqualFunc(decided.Boost.WorkingOrders, working, event.BoostWatchedOrder.Equal) {
		return
	}
	decided.Boost.ReplanPending, decided.Boost.Watched, decided.Boost.UndecidedOrders = b.h.replan, watched, undecided
	decided.Boost.WorkingOrders = working
	decided.Boost.NamedOrders = b.h.namedFigures(decided.Boost)
	e, err := b.h.stamp(b.out[i].ID, event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, b.at, b.in, decided)
	b.fail(err)
	b.out[i] = e
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
	check := payload.Validate
	if w, ok := payload.(interface{ ValidateWritten() error }); ok {
		check = w.ValidateWritten
	}
	if err := check(); err != nil {
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
		b.h.release(*out.Released)
		b.add("risk-order-released:"+o.Planned.ClientOrderID, event.OrderReleasedEventType, out.Released.SchemaVersion(), *out.Released)
		b.endUnsent(*out.Released)
	} else if out.Refused != nil {
		b.add("risk-order-refused:"+o.Planned.ClientOrderID, event.OrderRefusedEventType, out.Refused.SchemaVersion(), *out.Refused)
	}
	if out.State != nil {
		b.add(out.StateID, event.EngineStateEventType, out.StateSchemaVersion, *out.State)
	}
	return out
}

// endUnsent records, in a dry-run only, a synthetic terminal report on the
// Boost order o just released, which no venue receives, and ends it as a
// venue's cancellation would: the gate no longer counts a sell as pending,
// so a later exit, the broker-holding check and the sale back to 98% are
// sized from the account with no sell of the run's working, as paper sizes
// them once its earlier orders have ended (ADR 0026, point 3, "Mode",
// amended 2026-10-22; ADR 0019, amended 2026-10-15, "net of every working
// sell"). The report is marked synthetic and names no venue order, so no
// reader can take it for a venue's report or an execution
// (LimitOrderLifecyclePayload.Synthetic); it fills nothing.
func (b *emissions) endUnsent(o event.OrderReleasedPayload) {
	if b.h.cfg.Mode != event.TradingModeDryRun {
		return
	}
	p := event.LimitOrderLifecyclePayload{
		AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, InstrumentID: o.InstrumentID, Side: o.Side,
		Status: event.OrderLifecycleStatusCanceled, Quantity: o.Quantity, LimitPrice: o.LimitPrice,
		OccurredAt: b.at, Synthetic: true,
		Message: "dry-run: journalled with send false and never sent; synthetic terminal report, nothing executed",
	}
	b.add("synthetic-lifecycle:"+o.ClientOrderID, event.OrderLifecycleEventType, p.SchemaVersion(), p)
	// The gate released this order from these very figures, so it cannot
	// refuse the report: an error is a defect.
	b.fail(b.h.gate.ObserveLifecycle(p))
}

// release adds o to the orders this run released, by client order ID.
func (h *Handler) release(o event.OrderReleasedPayload) {
	if h.released == nil {
		h.released = map[string]event.OrderReleasedPayload{}
	}
	h.released[o.ClientOrderID] = o
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
