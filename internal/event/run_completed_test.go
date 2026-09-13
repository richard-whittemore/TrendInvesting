package event_test

import (
	"encoding/json"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestRunCompletedPayloadCarriesNoInstantOfItsOwn: the fact this event
// states is "the input stream ended", and WHEN it ended is the envelope's
// own EventTime. The payload is deliberately empty rather than restating it.
//
// Two instants that must always be equal are two instants that will
// eventually disagree — and a disagreement here would stamp every
// end-of-stream expiry at one of them while the journal's span was derived
// from the other, placing decisions outside the span the journal reports.
// An empty payload makes that unrepresentable rather than merely invalid.
func TestRunCompletedPayloadCarriesNoInstantOfItsOwn(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(event.RunCompletedPayload{})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if string(encoded) != "{}" {
		t.Fatalf("RunCompletedPayload marshals to %s, want {}: it must state no instant of its own", encoded)
	}
	if err := (event.RunCompletedPayload{}).Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestRunCompletedEventTypeAndSchemaVersion(t *testing.T) {
	t.Parallel()

	if event.RunCompletedEventType != "replay.run.completed" {
		t.Fatalf("RunCompletedEventType = %q", event.RunCompletedEventType)
	}
	if event.RunCompletedSchemaVersion != 1 {
		t.Fatalf("RunCompletedSchemaVersion = %d, want 1", event.RunCompletedSchemaVersion)
	}
}
