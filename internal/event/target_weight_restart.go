package event

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// RunOptionsEventType identifies a target-weight run's run-level facts, the
// ones recorded beside the configuration hash rather than inside it (ADR
// 0016): the first session the run decides, and how many recorded records
// of an earlier run follow it, which a restart reads back instead of
// recomputing (ADR 0026, point 5). It is an input, fed straight after the
// configuration, so a replay of the journal recovers the first session to
// decide from the journal itself.
const RunOptionsEventType = "strategy.run.options"

// RunOptionsSchemaVersion is the current schema version of RunOptionsPayload.
const RunOptionsSchemaVersion uint32 = 1

// RunOptionsPayload is one run's options.
type RunOptionsPayload struct {
	// DecideFrom is the period end of the first session the run decides.
	// Sessions before it only feed the indicators.
	DecideFrom time.Time `json:"decide_from"`
	// RecoveredRecords is how many of the inputs straight after this one are
	// an earlier run's recorded records, its restart prelude: 0 for a run
	// that starts afresh.
	RecoveredRecords int `json:"recovered_records"`
}

// Validate checks the first session to decide and the prelude's length.
func (p RunOptionsPayload) Validate() error {
	errs := requiredTime("decide from", p.DecideFrom)
	if p.RecoveredRecords < 0 {
		errs = append(errs, errors.New("recovered records must not be negative"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid run options payload: %w", err)
	}
	return nil
}

// RestartRecoveredEventType identifies what a restarted target-weight run
// recovered from an earlier run's recorded records (ADR 0026, point 5),
// emitted once the restart prelude has been read: the book in force with b
// and each sleeve's share, Boost's latch, the engine state and its scope, and
// the rebalance in progress with its phase. When a rebalance was in progress
// it is held: nothing more is sent for its decision time until the owner
// clears it with operator.rebalance.cleared (plan.md section 5, "Fail
// closed": "a restarted gateway mid-rebalance").
const RestartRecoveredEventType = "strategy.restart.recovered"

// RestartRecoveredSchemaVersion is the current schema version of RestartRecoveredPayload.
const RestartRecoveredSchemaVersion uint32 = 1

// The closed set of a recovered rebalance's phases (ADR 0026, point 2, "IRA
// execution": sells first, buys once every sell is terminal).
const (
	// RebalanceSellsWorking: a released sell is not yet terminal.
	RebalanceSellsWorking = "sells-working"
	// RebalanceBuysPending: every sell is terminal and the buy phase has not
	// been planned.
	RebalanceBuysPending = "buys-pending"
	// RebalanceBuysWorking: the buy phase was planned and a released buy is
	// not yet terminal.
	RebalanceBuysWorking = "buys-working"
	// RebalanceComplete: every released order is terminal and the buy phase
	// was planned.
	RebalanceComplete = "complete"
	// RebalanceAbandoned: the owner cleared the rebalance by abandoning it;
	// its buy phase is never planned.
	RebalanceAbandoned = "abandoned"
)

// RecoveredRebalance is the last decided rebalance as the recorded records
// leave it: its decision, its phase, whether it is held, and the client order
// IDs of its released orders that are not yet terminal, ascending.
type RecoveredRebalance struct {
	DecisionID string   `json:"decision_id"`
	Phase      string   `json:"phase"`
	Held       bool     `json:"held"`
	Working    []string `json:"working,omitempty"`
}

// RestartRecoveredPayload is what one restart recovered for one account.
type RestartRecoveredPayload struct {
	AccountID        string `json:"account_id"`
	StrategyID       string `json:"strategy_id"`
	RecoveredRecords int    `json:"recovered_records"`
	// LastDecisionID is the last recovered strategy.targets.decided, of either
	// outcome, and QuietUntil its session: sessions up to it only feed the
	// indicators and are never decided again. Both are empty when no
	// decision was recovered.
	LastDecisionID string    `json:"last_decision_id,omitempty"`
	QuietUntil     time.Time `json:"quiet_until,omitzero"`
	// BookDecisionID is the decision whose targets are in force and Book
	// those targets, sorted by instrument, with each sleeve's share; Glide
	// is that decision's age and b. For Boost, Book is the target in force
	// (from the last decision of either outcome) and BoostLatched the latch.
	BookDecisionID string         `json:"book_decision_id,omitempty"`
	Book           []TargetWeight `json:"book,omitempty"`
	Glide          GlideFigures   `json:"glide,omitzero"`
	BoostLatched   bool           `json:"boost_latched,omitzero"`
	// EngineState is normal, degraded or halted, and Scope the instruments a
	// degraded state freezes.
	EngineState string             `json:"engine_state"`
	Scope       []string           `json:"scope,omitempty"`
	Rebalance   RecoveredRebalance `json:"rebalance,omitzero"`
}

// EngineStateNormal is the engine state with no breach in force. It is never
// recorded by strategy.engine.state, which records only a move out of it,
// but a restart records the state it recovered, Normal included.
const EngineStateNormal = "normal"

// Validate checks identity, the book, the engine state and the rebalance.
func (p RestartRecoveredPayload) Validate() error {
	var errs []error
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	if p.StrategyID != TargetWeightStrategyBoost && p.StrategyID != TargetWeightStrategyMomentum {
		errs = append(errs, fmt.Errorf("strategy id %q is not boost or momentum", p.StrategyID))
	}
	if p.RecoveredRecords < 1 {
		errs = append(errs, errors.New("recovered records must be positive: a restart reads at least one"))
	}
	switch {
	case p.LastDecisionID != "":
		errs = append(errs, requiredTime("quiet until", p.QuietUntil)...)
	case !p.QuietUntil.IsZero():
		errs = append(errs, errors.New("the last decision and its session come together"))
	}
	ids := make([]string, len(p.Book))
	for i, t := range p.Book {
		ids[i] = t.InstrumentID
		if !isFinite(t.Weight) || t.Weight < 0 {
			errs = append(errs, fmt.Errorf("book %s weight must be finite and not negative", t.InstrumentID))
		}
		for _, share := range t.Shares {
			if !isFinite(share.Weight) || share.Weight < 0 {
				errs = append(errs, fmt.Errorf("book %s sleeve share must be finite and not negative", t.InstrumentID))
			}
		}
	}
	if !isFinite(p.Glide.BondShare) || p.Glide.BondShare < 0 || p.Glide.BondShare > 1 || p.Glide.AgeMonths < 0 {
		errs = append(errs, errors.New("glide b must lie in [0, 1] and the age must not be negative"))
	}
	errs = append(errs, ascendingIDs("book instrument id", ids)...)
	if len(p.Book) > 0 && p.BookDecisionID == "" {
		errs = append(errs, errors.New("a book names the decision it comes from"))
	}
	if p.StrategyID != TargetWeightStrategyBoost && p.BoostLatched {
		errs = append(errs, errors.New("only a Boost restart recovers a latch"))
	}
	switch p.EngineState {
	case EngineStateNormal, EngineStateHalted:
		if len(p.Scope) > 0 {
			errs = append(errs, fmt.Errorf("a %s engine names no scope", p.EngineState))
		}
	case EngineStateDegraded:
		if len(p.Scope) == 0 {
			errs = append(errs, errors.New("a degraded engine names its scope"))
		}
	default:
		errs = append(errs, fmt.Errorf("engine state %q is not normal, degraded or halted", p.EngineState))
	}
	errs = append(errs, ascendingIDs("scope instrument id", p.Scope)...)
	errs = append(errs, p.Rebalance.validate()...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid restart recovered payload: %w", err)
	}
	return nil
}

func (r RecoveredRebalance) validate() []error {
	if r.DecisionID == "" && r.Phase == "" && !r.Held && len(r.Working) == 0 {
		return nil
	}
	var errs []error
	if r.DecisionID == "" {
		errs = append(errs, errors.New("rebalance decision id is required"))
	}
	working := r.Phase == RebalanceSellsWorking || r.Phase == RebalanceBuysWorking
	switch r.Phase {
	case RebalanceSellsWorking, RebalanceBuysWorking, RebalanceBuysPending:
	case RebalanceComplete, RebalanceAbandoned:
		if r.Held {
			errs = append(errs, fmt.Errorf("a %s rebalance is not held", r.Phase))
		}
	default:
		errs = append(errs, fmt.Errorf("rebalance phase %q is not recognised", r.Phase))
	}
	if working && len(r.Working) == 0 {
		errs = append(errs, fmt.Errorf("a %s rebalance names its working orders", r.Phase))
	}
	if (r.Phase == RebalanceBuysPending || r.Phase == RebalanceComplete) && len(r.Working) > 0 {
		errs = append(errs, fmt.Errorf("a %s rebalance has no working order", r.Phase))
	}
	errs = append(errs, ascendingIDs("rebalance working order", r.Working)...)
	return errs
}

// RebalanceClearedEventType identifies the owner clearing a rebalance a
// restart held (ADR 0026, point 5: "a restart mid-rebalance sends nothing
// for that decision time until the owner clears it"; plan.md section 5). It
// is a recorded operator input, never a clock or a broker read, so a replay
// clears at the same point.
const RebalanceClearedEventType = "operator.rebalance.cleared"

// RebalanceClearedSchemaVersion is the current schema version of RebalanceClearedPayload.
const RebalanceClearedSchemaVersion uint32 = 1

// The closed set of ways the owner clears a held rebalance.
const (
	// RebalanceResume: the owner has checked the account against the
	// journal; the decision time continues, and its buy phase is planned
	// once every sell is terminal, as it would have been without the
	// restart.
	RebalanceResume = "resume"
	// RebalanceAbandon: nothing more is sent for the decision time; its
	// working orders are still tracked to their end, and the next decision
	// time decides afresh once they are terminal.
	RebalanceAbandon = "abandon"
)

// RebalanceClearedPayload is one clearing: the account, the decision whose
// rebalance is held, the action, when the owner cleared it, and a note.
type RebalanceClearedPayload struct {
	AccountID  string    `json:"account_id"`
	DecisionID string    `json:"decision_id"`
	Action     string    `json:"action"`
	ClearedAt  time.Time `json:"cleared_at"`
	Note       string    `json:"note,omitempty"`
}

// Validate checks identity, the action and the time.
func (p RebalanceClearedPayload) Validate() error {
	var errs []error
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	if p.DecisionID == "" {
		errs = append(errs, errors.New("decision id is required: a clearing names the rebalance it clears"))
	}
	if !slices.Contains([]string{RebalanceResume, RebalanceAbandon}, p.Action) {
		errs = append(errs, fmt.Errorf("action %q is not resume or abandon", p.Action))
	}
	errs = append(errs, requiredTime("cleared at", p.ClearedAt)...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid rebalance cleared payload: %w", err)
	}
	return nil
}
