package restart

import (
	"encoding/json"
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

// logConfiguration returns the account a log's configuration input names
// and the configuration hash it was recorded under, refusing a log with no
// target-weight configuration input.
func logConfiguration(entries []journal.Entry, logName string) (account, hash string, err error) {
	for _, entry := range entries {
		env := entry.Envelope
		if entry.Kind != journal.KindInput || env.Type != event.TargetWeightConfigurationEventType {
			continue
		}
		var cfg struct {
			AccountID string `json:"account_id"`
		}
		if err := json.Unmarshal(env.Payload, &cfg); err != nil {
			return "", "", fmt.Errorf("engine: decode %s in %s: %w", env.ID, logName, err)
		}
		return cfg.AccountID, env.ConfigurationHash, nil
	}
	return "", "", fmt.Errorf("engine: %s holds no target-weight configuration input, so the account whose rebalance it records is not known", logName)
}

// RebalanceAtEnd returns the rebalance a momentum log ends with in
// progress, for the account its own configuration input names, or nil when
// none is (ADR 0026, point 5: "Every planned, released and sent order, and
// the IRA phase (sells sent, buys pending)"). The orders are read by the
// rule the momentum run applies and the reporter reads the log by
// (event.OrderReports), so a report or clearance the run refused is not
// taken as read here either: the log commits a refused input as well as a
// read one. Released orders and a dry-run's synthetic reports are the run's
// own decisions; the venue's lifecycle reports and fills, retirements and
// the owner's clearances are its inputs. A rebalance the log's own approval
// carried in from an earlier run, which the run released none of, stays in
// progress until a clearance names it and exactly its orders, as the run
// reads one (momentumRun.applyCleared). The run reads an approval only as
// its second input, straight after the configuration, for its own
// configuration (Handler's decodeApproval), so any other approval in the
// log is one it refused and changes nothing here. A record that is evidence
// of an order and cannot be decoded refuses, naming it in logName.
func RebalanceAtEnd(entries []journal.Entry, logName string) (*event.RebalanceInProgress, error) {
	r, _, err := rebalanceOfAccount(entries, logName)
	return r, err
}

// rebalanceOfAccount is RebalanceAtEnd, with the account the log's
// configuration names.
func rebalanceOfAccount(entries []journal.Entry, logName string) (*event.RebalanceInProgress, string, error) {
	accountID, configHash, err := logConfiguration(entries, logName)
	if err != nil {
		return nil, "", err
	}
	r, err := rebalanceAtEnd(entries, accountID, configHash, logName)
	return r, accountID, err
}

// rebalanceAtEnd reads the rebalance in progress of accountID's log,
// recorded under configHash.
func rebalanceAtEnd(entries []journal.Entry, accountID, configHash, logName string) (*event.RebalanceInProgress, error) {
	inputs := 0
	reports := event.NewOrderReports()
	decided := map[string]event.TargetsDecidedPayload{}
	var carried *event.RebalanceInProgress
	decode := func(env event.Envelope, into any) error {
		if err := json.Unmarshal(env.Payload, into); err != nil {
			return fmt.Errorf("engine: decode %s in %s: %w", env.ID, logName, err)
		}
		return nil
	}
	for _, entry := range entries {
		env := entry.Envelope
		if entry.Kind == journal.KindDecision {
			switch env.Type {
			case event.TargetsDecidedEventType:
				var p event.TargetsDecidedPayload
				if err := decode(env, &p); err != nil {
					return nil, err
				}
				decided[env.ID] = p
			case event.OrderReleasedEventType:
				var p event.OrderReleasedPayload
				if err := decode(env, &p); err != nil {
					return nil, err
				}
				if p.AccountID == accountID {
					reports.Release(p)
				}
			case event.OrderLifecycleEventType:
				// A dry-run's synthetic report on an order it never sent.
				var p event.LimitOrderLifecyclePayload
				if err := decode(env, &p); err != nil {
					return nil, err
				}
				_, _ = reports.Lifecycle(p)
			}
			continue
		}
		inputs++
		switch env.Type {
		case event.OperatorRunApprovedEventType:
			if inputs != 2 || env.ConfigurationHash != configHash {
				continue // refused by the run: not straight after its configuration, or another's
			}
			p, err := event.DecodeRecordedOperatorRunApproved(env.Payload, env.SchemaVersion)
			if err != nil {
				return nil, fmt.Errorf("engine: %s in %s: %w", env.ID, logName, err)
			}
			if p.Momentum != nil && p.AccountID == accountID {
				carried = p.Momentum.RebalanceInProgress
			}
		case event.OrderLifecycleEventType:
			var p event.LimitOrderLifecyclePayload
			if decode(env, &p) == nil && env.SchemaVersion == event.OrderLifecycleLimitVersion && p.ValidateSchema(env.SchemaVersion) == nil && p.AccountID == accountID {
				_, _ = reports.Lifecycle(p)
			}
		case event.FillEventType:
			var p event.TargetWeightFillPayload
			if decode(env, &p) == nil && env.SchemaVersion == event.FillTargetWeightVersion && p.Validate() == nil && p.AccountID == accountID {
				_, _, _ = reports.Fill(p)
			}
		case event.OrderRetiredEventType:
			var p event.OrderRetiredPayload
			if decode(env, &p) == nil && env.SchemaVersion == event.OrderRetiredSchemaVersion && p.Validate() == nil && p.AccountID == accountID {
				_, _ = reports.Retire(p)
			}
		case event.RebalanceClearedEventType:
			var p event.RebalanceClearedPayload
			if decode(env, &p) != nil || env.SchemaVersion != event.RebalanceClearedSchemaVersion || p.Validate() != nil || p.AccountID != accountID {
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
	if carried != nil {
		return carried, nil
	}
	working := reports.Working()
	if len(working) == 0 {
		return nil, nil
	}
	// Every order not terminal is the rebalance in progress's: a momentum
	// run plans a new rebalance only once every order is terminal.
	first, _ := reports.Order(working[0])
	tag := first.Released.Tag
	phase := event.RebalancePhaseBuysWorking
	for _, id := range working {
		o, _ := reports.Order(id)
		if o.Released.Tag != tag {
			return nil, fmt.Errorf("engine: %s ends with orders of two rebalances not terminal, %s and %s, which a momentum run never leaves", logName, tag, o.Released.Tag)
		}
		if o.Released.Side == event.OrderSideSell {
			phase = event.RebalancePhaseSellsWorking
		}
	}
	decision, ok := decided[tag]
	if !ok {
		return nil, fmt.Errorf("engine: %s holds orders of %s not terminal but not that decision", logName, tag)
	}
	return &event.RebalanceInProgress{DecisionID: tag, DecisionTime: decision.DecisionTime, Phase: phase, ClientOrderIDs: working}, nil
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
