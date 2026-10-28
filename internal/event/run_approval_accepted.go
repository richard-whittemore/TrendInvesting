package event

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// RunApprovalAcceptedEventType identifies a momentum run's record that it
// accepted the owner's approval (operator.run.approved) and the rebalance in
// progress that approval carries in (ADR 0026, point 5; ADR 0027, note of
// 2026-10-28). It is a decision, recorded once, when the run reads the
// approval; an approval the run refuses records none. A later restart reads
// the rebalance carried in from this record, never from the approval input
// itself (which the log commits whether or not the run accepted it) and
// never by recomputing the run under another build's rules.
const RunApprovalAcceptedEventType = "strategy.run.approval-accepted"

// RunApprovalAcceptedSchemaVersion is the current schema version of
// RunApprovalAcceptedPayload.
const RunApprovalAcceptedSchemaVersion uint32 = 1

// RunApprovalAcceptedPayload is one accepted approval.
type RunApprovalAcceptedPayload struct {
	// AccountID is the account the run decides, the approval's.
	AccountID string `json:"account_id"`
	// ApprovalID and ApprovalSequence name the operator.run.approved input
	// the run accepted.
	ApprovalID       string `json:"approval_id"`
	ApprovalSequence uint64 `json:"approval_sequence"`
	// RebalanceInProgress is the rebalance the approval carries in, which
	// the run sends nothing for until the owner clears it; null when none.
	RebalanceInProgress *RebalanceInProgress `json:"rebalance_in_progress"`
}

// Validate checks the account, the approval named and the rebalance.
func (p RunApprovalAcceptedPayload) Validate() error {
	var errs []error
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	if p.ApprovalID == "" {
		errs = append(errs, errors.New("approval id is required: the record names the approval accepted"))
	}
	if p.ApprovalSequence == 0 {
		errs = append(errs, errors.New("approval sequence is required"))
	}
	if r := p.RebalanceInProgress; r != nil {
		if err := r.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid run approval accepted payload: %w", err)
	}
	return nil
}

// DecodeRunApprovalAccepted decodes and validates a recorded acceptance at
// schemaVersion: the rebalance key is required, null when none, an unknown
// key is refused, and any other version is refused (ADR 0015).
func DecodeRunApprovalAccepted(raw []byte, schemaVersion uint32) (RunApprovalAcceptedPayload, error) {
	if schemaVersion != RunApprovalAcceptedSchemaVersion {
		return RunApprovalAcceptedPayload{}, fmt.Errorf("the run approval accepted record's schema version %d is not one this build reads (%d; ADR 0015)", schemaVersion, RunApprovalAcceptedSchemaVersion)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return RunApprovalAcceptedPayload{}, fmt.Errorf("decode the run approval accepted record: %w", err)
	}
	if _, ok := keys[rebalanceKey]; !ok {
		return RunApprovalAcceptedPayload{}, fmt.Errorf("the run approval accepted record states no %s; it is required, null when none", rebalanceKey)
	}
	var p RunApprovalAcceptedPayload
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return RunApprovalAcceptedPayload{}, fmt.Errorf("decode the run approval accepted record: %w", err)
	}
	if err := p.Validate(); err != nil {
		return RunApprovalAcceptedPayload{}, err
	}
	return p, nil
}
