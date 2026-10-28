package restart

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
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

// logConfiguration returns a log's target-weight configuration input and
// its payload, refusing a log with none or one that cannot be decoded.
func logConfiguration(entries []journal.Entry, logName string) (event.Envelope, event.TargetWeightConfigurationPayload, error) {
	for _, entry := range entries {
		env := entry.Envelope
		if entry.Kind != journal.KindInput || env.Type != event.TargetWeightConfigurationEventType {
			continue
		}
		var cfg event.TargetWeightConfigurationPayload
		if err := json.Unmarshal(env.Payload, &cfg); err != nil {
			return env, cfg, fmt.Errorf("engine: decode %s in %s: %w", env.ID, logName, err)
		}
		return env, cfg, nil
	}
	return event.Envelope{}, event.TargetWeightConfigurationPayload{}, fmt.Errorf("engine: %s holds no target-weight configuration input, so the account whose rebalance it records is not known", logName)
}

// RebalanceAtEnd returns the rebalance a momentum log ends with in
// progress, or nil when none is (ADR 0026, point 5: "Every planned,
// released and sent order, and the IRA phase (sells sent, buys pending)").
// It is read by the run's own rule, never restated here: every input the
// log records is applied, in order, to a fresh target-weight handler built
// from the log's configuration input, its first session decided at that
// input's time (cmd/engine's compose), and the handler then says what is
// in progress (targetweight.Handler.RebalanceInProgress). The log commits an
// input the run refused as well as one it read, with no mark of which, and
// a refusal changes no state, so an input the handler refuses here is one
// the run refused: an approval the run refused, wherever it stands, changes
// nothing, and the approval it accepted, wherever that stands, carries its
// rebalance until a clearance the run read ends it. Like a resume (ADR
// 0027, section 6), this reads the log under the running build's rules. A
// log whose configuration the handler refuses is refused, naming logName.
func RebalanceAtEnd(entries []journal.Entry, logName string) (*event.RebalanceInProgress, error) {
	r, _, err := rebalanceOfAccount(entries, logName)
	return r, err
}

// rebalanceOfAccount is RebalanceAtEnd, with the account the log's
// configuration names.
func rebalanceOfAccount(entries []journal.Entry, logName string) (*event.RebalanceInProgress, string, error) {
	env, cfg, err := logConfiguration(entries, logName)
	if err != nil {
		return nil, "", err
	}
	version := env.StrategyVersion
	if version == "" {
		version = logName
	}
	h, err := targetweight.New(version, cfg, targetweight.Options{DecideFrom: env.EventTime})
	if err != nil {
		return nil, "", fmt.Errorf("engine: the configuration of %s cannot be read back: %w", logName, err)
	}
	for _, entry := range entries {
		if entry.Kind == journal.KindInput {
			// A refused input is one the run refused too: it changed nothing.
			_, _ = h.Apply(context.Background(), entry.Envelope)
		}
	}
	return h.RebalanceInProgress(), cfg.AccountID, nil
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
