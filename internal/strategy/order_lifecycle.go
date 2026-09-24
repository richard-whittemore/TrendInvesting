package strategy

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// applyOrderLifecycle handles event.OrderLifecycleEventType: a venue's
// report that one of this system's orders was acknowledged, amended,
// cancelled or refused, without executing. The rule is record, no decision,
// as applyAdapterRunStopped's is: positions, protective stops and pyramid
// state change only from a recorded fill (docs/architecture.md), and a
// lifecycle change is not one. The report is journalled so reconciliation
// can derive the broker's expected open orders from it (ADR 0019), not so
// the strategy can act on it.
//
// It is still read in full and refused if it cannot be understood, like
// every other input (docs/development.md principle 4: fail closed on
// unknown schemas): a report recorded under the wrong meaning would mislead
// the reconciliation that later reads it.
func (r *transition) applyOrderLifecycle(envelope event.Envelope) ([]event.Envelope, error) {
	if !r.configured {
		return nil, errors.New("strategy: received an order lifecycle report before a configuration event; failing closed")
	}
	if envelope.SchemaVersion != event.OrderLifecycleSchemaVersion {
		return nil, fmt.Errorf("strategy: order lifecycle payload schema version %d does not match the version %d this build requires; an older or newer schema is rejected, never silently upgraded, until an explicit upcaster exists (ADR 0015)", envelope.SchemaVersion, event.OrderLifecycleSchemaVersion)
	}
	var payload event.OrderLifecyclePayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return nil, fmt.Errorf("strategy: decode order lifecycle payload: %w", err)
	}
	if err := payload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: %w", err)
	}
	return nil, nil
}
