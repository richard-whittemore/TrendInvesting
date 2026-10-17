package event

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// OperatorRunApprovedEventType identifies the owner's approval to start a
// fresh run of an account whose engine has run before. A crash resumes from
// the durable log (ADR 0027); every other restart (leaving Halted, a build
// upgrade, a cleared rebalance) is a fresh run, and a fresh run decides
// nothing until this approval is its input, straight after the
// configuration (owner decision 2026-10-16). It names the previous run by its
// durable log and that log's final record hash, so the approval is bound to
// the evidence the owner reviewed, and carries the state in force that the
// fresh run starts from.
//
// Namespaced "operator.", as ADR 0026's operator.kill-switch is: a person's
// recorded instruction, not a market fact, an account observation or this
// system's own decision.
const OperatorRunApprovedEventType = "operator.run.approved"

// OperatorRunApprovedSchemaVersion is the current schema version of
// OperatorRunApprovedPayload.
const OperatorRunApprovedSchemaVersion uint32 = 1

// recordHashPattern is a journal record hash as the chain writes it (ADR
// 0017): "sha256:" and 64 lower-case hex digits.
var recordHashPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// OperatorRunApprovedPayload is one approval to start a fresh run.
type OperatorRunApprovedPayload struct {
	// AccountID is the account the fresh run decides; it must be the
	// configuration's.
	AccountID string `json:"account_id"`
	// PreviousLog is the file name, without a directory, of the previous
	// run's durable log in the same directory as the fresh run's log.
	PreviousLog string `json:"previous_log"`
	// PreviousLogFinalHash is the record hash of that log's last committed
	// record (journal.Recovery.FinalRecordHash).
	PreviousLogFinalHash string `json:"previous_log_final_hash"`
	// IncidentID names the incident the approval clears (ADR 0019's
	// recovery procedure), or is empty when there was none.
	IncidentID string    `json:"incident_id"`
	ApprovedBy string    `json:"approved_by"`
	ApprovedAt time.Time `json:"approved_at"`
	// Boost is the state in force the fresh run starts from.
	Boost BoostStateInForce `json:"boost"`
}

// BoostStateInForce is Boost's state that a fresh run cannot recompute from
// bars: whether the boost is latched (template.py:89-94; ADR 0026, point 5).
type BoostStateInForce struct {
	Latched bool `json:"latched"`
}

// Validate checks the account, the previous log's name and hash, and who
// approved and when.
func (p OperatorRunApprovedPayload) Validate() error {
	var errs []error
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	switch {
	case p.PreviousLog == "":
		errs = append(errs, errors.New("previous log is required"))
	case strings.ContainsAny(p.PreviousLog, `/\`) || p.PreviousLog == "." || p.PreviousLog == "..":
		errs = append(errs, fmt.Errorf("previous log %q must be a file name in the fresh run's log directory, not a path", p.PreviousLog))
	}
	if !recordHashPattern.MatchString(p.PreviousLogFinalHash) {
		errs = append(errs, fmt.Errorf("previous log final hash %q is not a record hash (sha256: and 64 lower-case hex digits)", p.PreviousLogFinalHash))
	}
	if p.ApprovedBy == "" {
		errs = append(errs, errors.New("approved by is required"))
	}
	errs = append(errs, requiredTime("approved at", p.ApprovedAt)...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid operator run approved payload: %w", err)
	}
	return nil
}
