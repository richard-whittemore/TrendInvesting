package strategy

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// applyAdapterRunStopped handles event.AdapterRunStoppedEventType: the
// adapter deliberately stopped this run (ADR 0012), distinct from reaching
// the natural end of its input stream. The reducer's rule is record, no
// decision — the same rule applyConfiguration follows for the event that
// opens a run — because this event states a fact about how the run ended,
// and nothing about strategy state changes because of it: whatever caused
// the stop (a delisting, today) is a fact the adapter observed itself and
// is not re-derived here.
//
// Nothing may follow a stop except event.RunCompletedEventType (see Apply's
// own runStopped check, above): a stop that some further bar or fill then
// contradicted would leave a run claiming to have deliberately ended while
// still receiving market data, which is not a fact this journal can state
// truthfully.
func (r *Reducer) applyAdapterRunStopped(envelope event.Envelope) ([]event.Envelope, error) {
	if !r.configured {
		return nil, errors.New("strategy: received a run stop before a configuration event; failing closed")
	}
	if envelope.SchemaVersion != event.AdapterRunStoppedSchemaVersion {
		return nil, fmt.Errorf("strategy: adapter run stopped payload schema version %d does not match the version %d this build requires; an older or newer schema is rejected, never silently upgraded, until an explicit upcaster exists (ADR 0015)", envelope.SchemaVersion, event.AdapterRunStoppedSchemaVersion)
	}
	var payload event.AdapterRunStoppedPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return nil, fmt.Errorf("strategy: decode adapter run stopped payload: %w", err)
	}
	if err := payload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: %w", err)
	}
	r.runStopped = true
	return nil, nil
}
