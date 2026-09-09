// Package event defines the transport-neutral contract shared by the Go
// application, the LEAN adapter, persistence, and deterministic replay.
package event

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// CurrentEnvelopeVersion is the envelope shape this build produces and
// expects to replay. It is distinct from Envelope.SchemaVersion, which
// versions a payload for a given event Type: CurrentEnvelopeVersion versions
// the envelope struct itself (see ADR 0015). Validate rejects any envelope
// whose EnvelopeVersion does not equal this constant, fail closed in both
// directions, because no upcaster is implemented yet.
const CurrentEnvelopeVersion uint32 = 1

// Envelope contains the immutable metadata required to order, correlate, and
// replay a domain input or output.
//
// The provenance fields (Source, StrategyVersion, ConfigurationHash) let a
// reviewer reading a journal tell which component, which build, and which
// configuration produced a decision. PayloadHash attests the payload bytes, so
// an alteration after recording is detectable rather than silent. All four are
// required: an event that cannot be traced to the code and configuration that
// produced it is not auditable evidence.
type Envelope struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// EnvelopeVersion is the shape version of this envelope struct itself,
	// distinct from SchemaVersion (which versions the payload for Type). It
	// must equal CurrentEnvelopeVersion: this build has no upcaster, so any
	// other value fails closed (ADR 0015).
	EnvelopeVersion uint32    `json:"envelope_version"`
	SchemaVersion   uint32    `json:"schema_version"`
	EventTime       time.Time `json:"event_time"`
	RecordedAt      time.Time `json:"recorded_at"`
	Sequence        uint64    `json:"sequence"`
	CorrelationID   string    `json:"correlation_id,omitempty"`
	CausationID     string    `json:"causation_id,omitempty"`
	// Source names the component that emitted the event, for example
	// "lean-adapter", "fill-simulator", "reducer", or "fixture".
	Source string `json:"source"`
	// StrategyVersion identifies the version of the strategy code in effect
	// when the event was produced.
	StrategyVersion string `json:"strategy_version"`
	// ConfigurationHash identifies the configuration the event was produced
	// under. Results are retained under their configuration hash (ADR 0012),
	// so this is what ties a journal back to a declared Baseline or Variant.
	ConfigurationHash string `json:"configuration_hash"`
	// PayloadHash is HashPayload(Payload): the SHA-256 of the payload bytes
	// exactly as stored, hex-encoded.
	PayloadHash string          `json:"payload_hash"`
	Payload     json.RawMessage `json:"payload"`
}

// HashPayload returns the hex-encoded SHA-256 of payload, hashing the bytes
// exactly as they are stored. It is deliberately not canonicalising JSON:
// producers in this system are deterministic, so the recorded bytes are the
// thing being attested, and a canonicalisation step would be one more piece of
// logic that could disagree between producer and verifier.
//
// Producers and validation share this one definition so that "the payload
// hash" cannot come to mean two different things.
func HashPayload(payload json.RawMessage) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
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
	if e.EnvelopeVersion == 0 {
		errs = append(errs, errors.New("envelope version is required"))
	}
	if e.EnvelopeVersion > CurrentEnvelopeVersion {
		errs = append(errs, fmt.Errorf("envelope version %d is greater than the current version %d: produced by a newer build", e.EnvelopeVersion, CurrentEnvelopeVersion))
	}
	// This also fires for the zero value above, since 0 < CurrentEnvelopeVersion:
	// an envelope recorded before this field existed decodes as EnvelopeVersion
	// 0, which is exactly the "no upcaster for an older shape" case this check
	// exists to catch, reported alongside the required-field complaint above.
	if e.EnvelopeVersion < CurrentEnvelopeVersion {
		errs = append(errs, fmt.Errorf("envelope version %d is less than the current version %d: no upcaster registered", e.EnvelopeVersion, CurrentEnvelopeVersion))
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
	if e.Source == "" {
		errs = append(errs, errors.New("event source is required"))
	}
	if e.StrategyVersion == "" {
		errs = append(errs, errors.New("strategy version is required"))
	}
	if e.ConfigurationHash == "" {
		errs = append(errs, errors.New("configuration hash is required"))
	}
	if e.PayloadHash == "" {
		errs = append(errs, errors.New("payload hash is required"))
	} else if HashPayload(e.Payload) != e.PayloadHash {
		// Only meaningful once a hash is present; an absent hash is already
		// reported above, and complaining twice about one gap obscures it.
		errs = append(errs, errors.New("payload hash does not match payload"))
	}
	if len(e.Payload) == 0 || !json.Valid(e.Payload) {
		errs = append(errs, errors.New("payload must contain valid JSON"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid event envelope: %w", err)
	}
	return nil
}
