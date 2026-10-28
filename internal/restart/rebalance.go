package restart

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// IsMomentumLog reports whether entries were recorded by a momentum run:
// its configuration input names strategy_id "momentum" (ADR 0026, point 6).
// A log with no target-weight configuration, a Boost or a Turtle one, is
// not.
func IsMomentumLog(entries []journal.Entry) bool {
	for _, entry := range entries {
		env := entry.Envelope
		if entry.Kind != journal.KindInput || env.Type != event.TargetWeightConfigurationEventType {
			continue
		}
		var kind struct {
			StrategyID string `json:"strategy_id"`
		}
		return json.Unmarshal(env.Payload, &kind) == nil && kind.StrategyID == event.TargetWeightStrategyMomentum
	}
	return false
}

// logAccount returns the account a log's configuration input names,
// refusing a log with none or one whose account cannot be read back. Only
// the account is read, so the reading does not depend on the running build
// accepting the rest of the configuration.
func logAccount(entries []journal.Entry, logName string) (string, error) {
	for _, entry := range entries {
		env := entry.Envelope
		if entry.Kind != journal.KindInput || env.Type != event.TargetWeightConfigurationEventType {
			continue
		}
		var cfg struct {
			AccountID string `json:"account_id"`
		}
		if err := json.Unmarshal(env.Payload, &cfg); err != nil {
			return "", cannotReadBack(logName, fmt.Errorf("decode its configuration input %s: %w", env.ID, err))
		}
		if cfg.AccountID == "" {
			return "", cannotReadBack(logName, fmt.Errorf("its configuration input %s names no account", env.ID))
		}
		return cfg.AccountID, nil
	}
	return "", cannotReadBack(logName, errors.New("it holds no target-weight configuration input"))
}

// readRecord decodes a decision record recovery reads, refusing one at a
// schema version this build does not read, or one that does not decode or
// validate at its version (ADR 0015), so a null or empty record is never
// read as one that names nothing.
func readRecord[T interface{ ValidateSchema(uint32) error }](env event.Envelope, versions []uint32, logName string) (T, error) {
	var p T
	if !slices.Contains(versions, env.SchemaVersion) {
		return p, cannotReadBack(logName, fmt.Errorf("%s %s is at schema version %d, not one of %v this build reads", env.Type, env.ID, env.SchemaVersion, versions))
	}
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return p, cannotReadBack(logName, fmt.Errorf("decode %s: %w", env.ID, err))
	}
	if err := p.ValidateSchema(env.SchemaVersion); err != nil {
		return p, cannotReadBack(logName, fmt.Errorf("%s: %w", env.ID, err))
	}
	return p, nil
}

// cannotReadBack is RebalanceAtEnd's refusal of a log it cannot read: fail
// closed, never no rebalance in progress (AGENTS.md).
func cannotReadBack(logName string, err error) error {
	return fmt.Errorf("engine: the rebalance in progress of %s cannot be read back: %w", logName, err)
}

// RebalanceAtEnd returns the rebalance a momentum log ends with in progress,
// for the account its configuration input names, or nil when none is (ADR
// 0026, point 5: "Every planned, released and sent order, and the IRA phase
// (sells sent, buys pending)"). It reads only what the log records, so it
// is the same under any build (ADR 0027 refuses to recompute a run under
// another build's rules):
//
//   - the rebalance the run carried in is the one its last
//     strategy.run.approval-accepted decision states: the run records one
//     only for the approval it accepted, so an approval input it refused,
//     wherever it stands, counts for nothing (the log commits a refused
//     input with no mark of the refusal);
//   - the orders are the run's own risk.order.released decisions and its
//     dry-run synthetic reports, and the venue's lifecycle reports and
//     fills, the retirements and the owner's clearances it was sent, read by
//     the rule the momentum run applies and the reporter reads the log by
//     (event.OrderReports), so a report or clearance the run refused is not
//     taken as read here either;
//   - a carried rebalance, whose orders the run never released, stays in
//     progress until a clearance names it and exactly its orders, as the run
//     reads one (momentumRun.applyCleared).
//
// A record it must read and cannot decode or validate refuses, naming
// logName: "cannot be read back", never nil.
func RebalanceAtEnd(entries []journal.Entry, logName string) (*event.RebalanceInProgress, error) {
	r, _, err := rebalanceOfAccount(entries, logName)
	return r, err
}

// rebalanceOfAccount is RebalanceAtEnd, with the account the log's
// configuration names.
func rebalanceOfAccount(entries []journal.Entry, logName string) (*event.RebalanceInProgress, string, error) {
	accountID, err := logAccount(entries, logName)
	if err != nil {
		return nil, "", err
	}
	reports := event.NewOrderReports()
	decided := map[string]event.TargetsDecidedPayload{}
	var carried *event.RebalanceInProgress
	// refused names an approval input the run was given while it had
	// accepted none, which therefore refused it; "" when there is none.
	refused, accepted := "", false
	// refusedPrevious is the previous log the refused approval names: "" for
	// a first run's, nil when it names none it can be read for.
	var refusedPrevious *string
	for _, entry := range entries {
		env := entry.Envelope
		if entry.Kind == journal.KindDecision {
			switch env.Type {
			case event.RunApprovalAcceptedEventType:
				p, err := event.DecodeRunApprovalAccepted(env.Payload, env.SchemaVersion)
				if err != nil {
					return nil, "", cannotReadBack(logName, fmt.Errorf("%s: %w", env.ID, err))
				}
				if p.AccountID != accountID {
					return nil, "", cannotReadBack(logName, fmt.Errorf("%s accepts an approval for account %q, but the log is %q's", env.ID, p.AccountID, accountID))
				}
				carried, accepted, refused = p.RebalanceInProgress, true, ""
			case event.TargetsDecidedEventType:
				p, err := readRecord[event.TargetsDecidedPayload](env, event.TargetsDecidedVersions(), logName)
				if err != nil {
					return nil, "", err
				}
				decided[env.ID] = p
			case event.OrderReleasedEventType:
				p, err := readRecord[event.OrderReleasedPayload](env, event.OrderReleasedVersions(), logName)
				if err != nil {
					return nil, "", err
				}
				if p.AccountID == accountID {
					reports.Release(p)
				}
			case event.OrderLifecycleEventType:
				// A dry-run's synthetic report on an order it never sent.
				p, err := readRecord[event.LimitOrderLifecyclePayload](env, []uint32{event.OrderLifecycleSyntheticVersion}, logName)
				if err != nil {
					return nil, "", err
				}
				_, _ = reports.Lifecycle(p)
			}
			continue
		}
		if env.Type == event.OperatorRunApprovedEventType && !accepted {
			// Its own group records no acceptance unless the next record is
			// one, which clears it (above). Whatever account it names, or
			// fails to name, and whether or not it decodes: the run had
			// accepted no approval, so its state is not known from the log.
			var p struct {
				PreviousLog *string `json:"previous_log"`
			}
			refused, refusedPrevious = env.ID, nil
			if json.Unmarshal(env.Payload, &p) == nil {
				refusedPrevious = p.PreviousLog
			}
		}
		// The venue's reports, retirements and clearances the run was sent;
		// one the run refused is refused by the same rule here and read as
		// nothing.
		switch env.Type {
		case event.OrderLifecycleEventType:
			var p event.LimitOrderLifecyclePayload
			if json.Unmarshal(env.Payload, &p) == nil && env.SchemaVersion == event.OrderLifecycleLimitVersion && p.ValidateSchema(env.SchemaVersion) == nil && p.AccountID == accountID {
				_, _ = reports.Lifecycle(p)
			}
		case event.FillEventType:
			var p event.TargetWeightFillPayload
			if json.Unmarshal(env.Payload, &p) == nil && env.SchemaVersion == event.FillTargetWeightVersion && p.Validate() == nil && p.AccountID == accountID {
				_, _, _ = reports.Fill(p)
			}
		case event.OrderRetiredEventType:
			var p event.OrderRetiredPayload
			if json.Unmarshal(env.Payload, &p) == nil && env.SchemaVersion == event.OrderRetiredSchemaVersion && p.Validate() == nil && p.AccountID == accountID {
				_, _ = reports.Retire(p)
			}
		case event.RebalanceClearedEventType:
			var p event.RebalanceClearedPayload
			if json.Unmarshal(env.Payload, &p) != nil || env.SchemaVersion != event.RebalanceClearedSchemaVersion || p.Validate() != nil || p.AccountID != accountID {
				continue
			}
			if carried != nil {
				if p.DecisionID == carried.DecisionID && slices.Equal(p.ClientOrderIDs, carried.ClientOrderIDs) {
					carried = nil
				}
				continue
			}
			_ = reports.Clear(p)
		}
	}
	if refused != "" {
		// The run refused the only approval it was given: the rebalance that
		// approval named is not known to be carried, nor to be ended, so the
		// state in force is the run's before it, or, for an account's first
		// run, none was ever carried (ADR 0026, point 5).
		next := "approve the run before it, or, if it was the account's first run, start again as a first run with `operator approve -first-run`"
		switch {
		case refusedPrevious != nil && *refusedPrevious == "":
			next = "it was the account's first run, so start again as a first run with `operator approve -first-run`"
		case refusedPrevious != nil:
			next = fmt.Sprintf("approve the run before it, %s", *refusedPrevious)
		}
		return nil, "", cannotReadBack(logName, fmt.Errorf("its run refused its approval %s and accepted none: move %s, unchanged, into the quarantine folder beside it, then %s (docs/runbooks/durable-log.md, \"An unrecoverable log\")", refused, logName, next))
	}
	if carried != nil {
		return carried, accountID, nil
	}
	working := reports.Working()
	if len(working) == 0 {
		return nil, accountID, nil
	}
	// Every order not terminal is the rebalance in progress's: a momentum
	// run plans a new rebalance only once every order is terminal.
	first, _ := reports.Order(working[0])
	tag := first.Released.Tag
	phase := event.RebalancePhaseBuysWorking
	for _, id := range working {
		o, _ := reports.Order(id)
		if o.Released.Tag != tag {
			return nil, "", cannotReadBack(logName, fmt.Errorf("it ends with orders of two rebalances not terminal, %s and %s, which a momentum run never leaves", tag, o.Released.Tag))
		}
		if o.Released.Side == event.OrderSideSell {
			phase = event.RebalancePhaseSellsWorking
		}
	}
	decision, ok := decided[tag]
	if !ok {
		return nil, "", cannotReadBack(logName, fmt.Errorf("it holds orders of %s not terminal but not that decision", tag))
	}
	return &event.RebalanceInProgress{DecisionID: tag, DecisionTime: decision.DecisionTime, Phase: phase, ClientOrderIDs: working}, accountID, nil
}

// CheckRebalanceInProgress refuses an IRA approval whose rebalance in
// progress is not the one the approved log ends with (RebalanceAtEnd):
// naming one the log does not hold in progress, or stating none where one
// is, since a fresh run sends nothing for a rebalance in progress until the
// owner clears it (ADR 0026, point 5). A Boost-shaped approval, which states
// no momentum state, is refused for a momentum log. logName names the
// approved log in a refusal.
func CheckRebalanceInProgress(entries []journal.Entry, approval event.OperatorRunApprovedPayload, logName string) error {
	if approval.Momentum == nil {
		return fmt.Errorf("engine: %s is a momentum run's, but the approval is Boost-shaped: it states Boost's latch and no rebalance in progress; approve it with `operator approve -log %s`, which states the rebalance in progress or none (ADR 0026, point 5)", logName, logName)
	}
	want, account, err := rebalanceOfAccount(entries, logName)
	switch {
	case err != nil:
		return err
	case approval.AccountID != account:
		return fmt.Errorf("engine: the approval is for account %q, but %s is the run of %q", approval.AccountID, logName, account)
	}
	stated := approval.Momentum.RebalanceInProgress
	switch {
	case want == nil && stated == nil:
		return nil
	case want == nil:
		return fmt.Errorf("engine: the approval names rebalance %s with orders %v in progress, but %s ends with no rebalance in progress, so a run approved with it would hold for a rebalance it cannot find (ADR 0026, point 5)", stated.DecisionID, stated.ClientOrderIDs, logName)
	case stated == nil:
		return fmt.Errorf("engine: %s ends mid-rebalance (%s, %s, orders %v not terminal), but the approval states none in progress: a fresh run sends nothing for that rebalance until the owner clears it (ADR 0026, point 5), so approve it as `operator approve` writes it", logName, want.DecisionID, want.Phase, want.ClientOrderIDs)
	case stated.DecisionID != want.DecisionID || !stated.DecisionTime.Equal(want.DecisionTime) || stated.Phase != want.Phase || !slices.Equal(stated.ClientOrderIDs, want.ClientOrderIDs):
		return fmt.Errorf("engine: the approval names rebalance %s (%s, orders %v), but %s ends with %s (%s, orders %v) in progress: the approval's is a rebalance it cannot find (ADR 0026, point 5)", stated.DecisionID, stated.Phase, stated.ClientOrderIDs, logName, want.DecisionID, want.Phase, want.ClientOrderIDs)
	}
	return nil
}
