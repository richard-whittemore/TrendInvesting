package event

import "time"

// CanonicalEnvelopeBytes returns the canonical byte string that identifies an
// envelope's content, for a consumer that must hash an envelope rather than a
// payload — the journal's record chain is the one this exists for.
//
// Every field of the envelope is covered, so an alteration to any of them
// changes the bytes. Two properties a hasher depends on:
//
//   - A round trip through JSON does not change the result, so bytes hashed
//     when a record was written are the bytes recomputed when it is read back.
//     Timestamps are therefore rendered as RFC 3339 in UTC, which also makes
//     the same instant canonicalise identically whatever zone it is stated in.
//   - The payload appears as the bytes exactly as stored, which is what
//     PayloadHash attests (see HashPayload), never a re-encoding of them.
//
// It reuses canonicalJSON, the one canonical encoder in this project (ADR
// 0016), rather than adding a second: two encoders that disagreed would make
// a hash mean two different things. The map is built with the timestamps
// already rendered because canonicalJSON refuses a struct whose state is
// entirely unexported — time.Time is exactly that shape — for the reason its
// own doc comment gives.
func CanonicalEnvelopeBytes(e Envelope) []byte {
	return CanonicalBytes(map[string]any{
		"id":                 e.ID,
		"type":               e.Type,
		"envelope_version":   e.EnvelopeVersion,
		"schema_version":     e.SchemaVersion,
		"event_time":         e.EventTime.UTC().Format(time.RFC3339Nano),
		"recorded_at":        e.RecordedAt.UTC().Format(time.RFC3339Nano),
		"sequence":           e.Sequence,
		"correlation_id":     e.CorrelationID,
		"causation_id":       e.CausationID,
		"source":             e.Source,
		"strategy_version":   e.StrategyVersion,
		"configuration_hash": e.ConfigurationHash,
		"payload_hash":       e.PayloadHash,
		"payload":            string(e.Payload),
	})
}

// CanonicalBytes renders fields as canonical JSON: keys sorted, no
// insignificant whitespace, numbers in Go's shortest round-trip formatting
// (see canonicalJSON; ADR 0016).
//
// It exists so that a consumer hashing something other than an envelope —
// the journal's header is the one there is — uses this project's single
// canonical encoder rather than acquiring a second definition of "the
// canonical bytes of a value". Render timestamps as RFC 3339 in UTC before
// passing them: canonicalJSON refuses a value whose state is entirely
// unexported, which time.Time is.
func CanonicalBytes(fields map[string]any) []byte {
	return canonicalJSON(fields)
}
