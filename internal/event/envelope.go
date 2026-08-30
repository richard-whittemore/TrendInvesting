// Package event defines the transport-neutral contract shared by the Go
// application, the LEAN adapter, persistence, and deterministic replay.
package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Envelope contains the immutable metadata required to order, correlate, and
// replay a domain input or output.
type Envelope struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	SchemaVersion uint32          `json:"schema_version"`
	EventTime     time.Time       `json:"event_time"`
	RecordedAt    time.Time       `json:"recorded_at"`
	Sequence      uint64          `json:"sequence"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	CausationID   string          `json:"causation_id,omitempty"`
	Payload       json.RawMessage `json:"payload"`
}

// Validate checks only transport-level invariants. Strategy-specific payload
// validation belongs to the handler for the declared event type and schema.
func (e Envelope) Validate() error {
	var errs []error
	if e.ID == "" {
		errs = append(errs, errors.New("event id is required"))
	}
	if e.Type == "" {
		errs = append(errs, errors.New("event type is required"))
	}
	if e.SchemaVersion == 0 {
		errs = append(errs, errors.New("schema version must be positive"))
	}
	if e.EventTime.IsZero() {
		errs = append(errs, errors.New("event time is required"))
	}
	if e.RecordedAt.IsZero() {
		errs = append(errs, errors.New("recorded time is required"))
	}
	if e.Sequence == 0 {
		errs = append(errs, errors.New("sequence must be positive"))
	}
	if len(e.Payload) == 0 || !json.Valid(e.Payload) {
		errs = append(errs, errors.New("payload must contain valid JSON"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid event envelope: %w", err)
	}
	return nil
}
