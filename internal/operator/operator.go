// Package operator defines the records the operator tool writes into an
// engine's inbox: the owner's kill switch (operator.kill-switch, ADR 0026,
// point 3) and the clearance of a rebalance in progress
// (operator.rebalance.cleared, ADR 0026, point 5), each a recorded input,
// never a live read of a file or command. A record is validated when it is
// written and again, byte for byte, when the engine reads it, which delivers
// it as an input at the next position of the run's one input sequence; the
// durable log, not the inbox, is the evidence (ADR 0027). It performs no I/O.
package operator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// Source is stamped on every operator input the engine delivers: a person's
// recorded instruction, not a market fact, an account observation or this
// system's own decision.
const Source = "operator"

// fileTime is the layout of a record file name's time, which sorts by time.
const fileTime = "20060102T150405Z"

// Record is one operator input as the tool records it.
type Record struct {
	// ID is the input's envelope ID: its type, account and time, so no two
	// inputs of one type and account share a second.
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	SchemaVersion uint32          `json:"schema_version"`
	At            time.Time       `json:"at"`
	AccountID     string          `json:"account_id"`
	Payload       json.RawMessage `json:"payload"`
}

// KillSwitch records the owner's engagement or release of the kill switch.
func KillSwitch(p event.KillSwitchPayload) (Record, error) {
	return record(event.KillSwitchEventType, event.KillSwitchSchemaVersion, p.AccountID, p.AsOf, p)
}

// RebalanceCleared records the owner's clearance of a rebalance in progress.
func RebalanceCleared(p event.RebalanceClearedPayload) (Record, error) {
	return record(event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, p.AccountID, p.AsOf, p)
}

func record(eventType string, version uint32, account string, at time.Time, p interface{ Validate() error }) (Record, error) {
	if err := p.Validate(); err != nil {
		return Record{}, fmt.Errorf("operator: %w", err)
	}
	// A validated payload marshals (validated-payload-json).
	raw, _ := json.Marshal(p)
	at = at.UTC()
	return Record{
		ID: fmt.Sprintf("%s:%s:%s", eventType, account, at.Format(time.RFC3339)), Type: eventType, SchemaVersion: version,
		At: at, AccountID: account, Payload: raw,
	}, nil
}

// FileName is the record's name in the inbox: its stated time, then its
// type, so the inbox sorts by the time each record states. That is the
// time the operator gave with -at, which may be earlier than when the
// record was written, so file-name order is the order of the stated times,
// not of writing.
func (r Record) FileName() string { return r.At.Format(fileTime) + "-" + r.Type + ".json" }

// Encode returns the record as the tool writes it, one line of JSON and a
// newline. The payload keeps its exact bytes, which Decode compares. A
// record KillSwitch, RebalanceCleared or Decode built marshals: its payload
// is the JSON its constructor encoded.
func (r Record) Encode() []byte {
	raw, _ := json.Marshal(r)
	return append(raw, '\n')
}

// Decode reads a record back and refuses one the tool would not write: an
// unknown key, anything but white space after the one record, an unknown
// type or version, a payload its type refuses, or any field that differs
// from the record rebuilt from the payload, so a hand-edited or damaged
// file is never read as another instruction.
func Decode(raw []byte) (Record, error) {
	var r Record
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&r); err != nil {
		return Record{}, fmt.Errorf("operator: decode the operator record: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Record{}, fmt.Errorf("operator: record %s has more after the record: a record file holds exactly one record", r.ID)
	}
	var rebuilt Record
	var err error
	switch r.Type {
	case event.KillSwitchEventType:
		var p event.KillSwitchPayload
		if err = json.Unmarshal(r.Payload, &p); err == nil {
			rebuilt, err = KillSwitch(p)
		}
	case event.RebalanceClearedEventType:
		var p event.RebalanceClearedPayload
		if err = json.Unmarshal(r.Payload, &p); err == nil {
			rebuilt, err = RebalanceCleared(p)
		}
	default:
		return Record{}, fmt.Errorf("operator: record %s has type %q, which is not an operator input this build reads", r.ID, r.Type)
	}
	switch {
	case err != nil:
		return Record{}, fmt.Errorf("operator: record %s: %w", r.ID, err)
	case r.SchemaVersion != rebuilt.SchemaVersion:
		return Record{}, fmt.Errorf("operator: record %s is %s schema version %d, not the version %d this build reads (ADR 0015)", r.ID, r.Type, r.SchemaVersion, rebuilt.SchemaVersion)
	case r.ID != rebuilt.ID || !r.At.Equal(rebuilt.At) || r.AccountID != rebuilt.AccountID || !bytes.Equal(r.Payload, rebuilt.Payload):
		return Record{}, fmt.Errorf("operator: record %s is not the record its payload makes, %s: it was edited or damaged after the operator tool wrote it", r.ID, rebuilt.ID)
	}
	return rebuilt, nil
}

// Envelope delivers the record as an input at sequence, stamped with the
// run's strategy version and configuration hash. Its times are the
// operator's, so a journal holding it replays identically (ADR 0017).
func (r Record) Envelope(sequence uint64, strategyVersion, configurationHash string) event.Envelope {
	return event.Envelope{
		ID: r.ID, Type: r.Type, SchemaVersion: r.SchemaVersion, EnvelopeVersion: event.CurrentEnvelopeVersion,
		EventTime: r.At, RecordedAt: r.At, Sequence: sequence, Source: Source,
		StrategyVersion: strategyVersion, ConfigurationHash: configurationHash,
		PayloadHash: event.HashPayload(r.Payload), Payload: r.Payload,
	}
}
