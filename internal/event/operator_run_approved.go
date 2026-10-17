package event

import (
	"bytes"
	"encoding/json"
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
	// IncidentInForce is the incident the previous run ended in and the
	// fresh run continues, with whether its one sale back to 98% was
	// attempted; nil (JSON null) when there is none. It is never the
	// incident the approval clears.
	IncidentInForce *IncidentInForce `json:"incident_in_force"`
}

// BoostStateInForce is Boost's state that a fresh run cannot recompute from
// bars: whether the boost is latched (template.py:89-94; ADR 0026, point 5).
// Latched is required: a missing or null latch is refused, never read as
// "not latched", which would turn the boost off unasked.
type BoostStateInForce struct {
	Latched *bool `json:"latched"`
}

// IncidentInForce is one incident's state that a fresh run cannot recompute
// from bars: whether ADR 0019's one sale back to 98% (amended 2026-10-15:
// attempted once per incident, never retried automatically) was attempted.
// A fresh run approved with the sale attempted never attempts it again.
type IncidentInForce struct {
	// IncidentID names the incident (ADR 0019's first engine-state record
	// out of Normal).
	IncidentID string `json:"incident_id"`
	// SaleAttempted is required: whether the incident's sale was attempted,
	// sent or withheld (an unreadable broker value or an unknown working
	// sell is an attempt; nothing to sell is not).
	SaleAttempted *bool `json:"sale_attempted"`
	// SaleClientOrderID is the attempt's client order ID when it was sent,
	// and empty otherwise.
	SaleClientOrderID string `json:"sale_client_order_id,omitempty"`
}

// Validate checks the account, the previous log's name and hash, who
// approved and when, and that the state in force is stated: Boost's latch,
// and for an incident in force, whether its sale was attempted.
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
	if p.Boost.Latched == nil {
		errs = append(errs, errors.New("boost latched is required: the latch in force is stated, never assumed (ADR 0026, point 5)"))
	}
	if in := p.IncidentInForce; in != nil {
		switch in.IncidentID {
		case "":
			errs = append(errs, errors.New("incident in force needs its incident id"))
		case p.IncidentID:
			errs = append(errs, fmt.Errorf("incident %q cannot be both the one the approval clears and the one in force", in.IncidentID))
		}
		switch {
		case in.SaleAttempted == nil:
			errs = append(errs, errors.New("incident in force: sale attempted is required (ADR 0019, amended 2026-10-15: one attempt per incident)"))
		case !*in.SaleAttempted && in.SaleClientOrderID != "":
			errs = append(errs, fmt.Errorf("incident in force: sale client order %q names an attempt, but the sale is stated as not attempted", in.SaleClientOrderID))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid operator run approved payload: %w", err)
	}
	return nil
}

// approvalKeys are the keys an approval file must state, null included: a
// key left out is refused rather than read as its zero value.
var approvalKeys = []string{"account_id", "previous_log", "previous_log_final_hash", "incident_id", "approved_by", "approved_at", "boost", "incident_in_force"}

// DecodeOperatorRunApproved decodes and validates an approval written
// outside the engine. Every key must be present, an unknown key is refused,
// and the input is one JSON value, so a state the owner did not state is
// never a default (ADR 0026, point 5; ADR 0019, amended 2026-10-15).
func DecodeOperatorRunApproved(raw []byte) (OperatorRunApprovedPayload, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return OperatorRunApprovedPayload{}, fmt.Errorf("decode the approval: it must be one JSON value, an object: %w", err)
	}
	var missing []string
	for _, key := range approvalKeys {
		if _, ok := keys[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return OperatorRunApprovedPayload{}, fmt.Errorf("the approval states no %s; every key is required, null where there is none", strings.Join(missing, ", "))
	}
	var p OperatorRunApprovedPayload
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return OperatorRunApprovedPayload{}, fmt.Errorf("decode the approval: %w", err)
	}
	if err := p.Validate(); err != nil {
		return OperatorRunApprovedPayload{}, err
	}
	return p, nil
}
