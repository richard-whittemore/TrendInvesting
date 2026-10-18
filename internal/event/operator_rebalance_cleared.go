package event

import (
	"errors"
	"fmt"
	"time"
)

// RebalanceClearedEventType identifies the owner's decision to clear a
// rebalance in progress: an IRA's decision whose orders are not all
// terminal withholds every later decision (rebalance-in-progress), and a
// rebalance in progress sends nothing more until the owner clears it (ADR
// 0026, point 5 and "A new rebalance is withheld while an earlier one's
// orders are working"; plan.md section 3). It is a recorded input, never a
// live read of a file or command, namespaced "operator." as
// operator.kill-switch is.
const RebalanceClearedEventType = "operator.rebalance.cleared"

// RebalanceClearedSchemaVersion is the current schema version of
// RebalanceClearedPayload.
const RebalanceClearedSchemaVersion uint32 = 1

// RebalanceClearedPayload clears one rebalance in progress. The owner has
// dealt with each named order at the broker (cancelled it, or confirmed it
// is no longer working); the run takes each as terminal and plans no more of
// that rebalance, so the next decision time decides afresh.
type RebalanceClearedPayload struct {
	AccountID string    `json:"account_id"`
	AsOf      time.Time `json:"as_of"`
	// DecisionID is the strategy.targets.decided envelope ID of the
	// rebalance in progress.
	DecisionID string `json:"decision_id"`
	// ClientOrderIDs are, strictly ascending, every order of that rebalance
	// that is not terminal: the clearance names them all, so none is taken
	// as cleared by its absence.
	ClientOrderIDs []string `json:"client_order_ids"`
	// Operator names who cleared it; Detail says what was done at the
	// broker and why.
	Operator string `json:"operator"`
	Detail   string `json:"detail"`
}

// Validate checks the account, time, decision, orders, operator and detail.
func (p RebalanceClearedPayload) Validate() error {
	var errs []error
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	errs = append(errs, requiredTime("as of", p.AsOf)...)
	if p.DecisionID == "" {
		errs = append(errs, errors.New("decision id is required: the clearance names the rebalance in progress"))
	}
	if len(p.ClientOrderIDs) == 0 {
		errs = append(errs, errors.New("client order ids are required: the clearance names every order of the rebalance that is not terminal"))
	}
	errs = append(errs, ascendingIDs("client order id", p.ClientOrderIDs)...)
	if p.Operator == "" {
		errs = append(errs, errors.New("operator is required"))
	}
	if p.Detail == "" {
		errs = append(errs, errors.New("detail is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid rebalance cleared payload: %w", err)
	}
	return nil
}
