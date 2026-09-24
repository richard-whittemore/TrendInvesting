// Command session_contract checks adapter output against the Go session-closed
// contract (ADR 0015, ADR 0021); Python tests feed the exact wire JSON on stdin.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func main() {
	var envelope event.Envelope
	must(json.NewDecoder(os.Stdin).Decode(&envelope))
	must(envelope.Validate())
	if envelope.Type != event.SessionClosedEventType || envelope.SchemaVersion != event.SessionClosedSchemaVersion {
		must(fmt.Errorf("unexpected session closed type/schema: %s/%d", envelope.Type, envelope.SchemaVersion))
	}
	var payload event.SessionClosedPayload
	must(json.Unmarshal(envelope.Payload, &payload))
	must(payload.Validate())
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
