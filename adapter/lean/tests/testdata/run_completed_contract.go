// Command run_completed_contract checks adapter output against the Go
// replay.run.completed contract (ADR 0012, ADR 0015); Python tests feed the
// exact wire JSON on stdin.
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
	if envelope.Type != event.RunCompletedEventType || envelope.SchemaVersion != event.RunCompletedSchemaVersion {
		must(fmt.Errorf("unexpected run completed type/schema: %s/%d", envelope.Type, envelope.SchemaVersion))
	}
	var payload event.RunCompletedPayload
	must(json.Unmarshal(envelope.Payload, &payload))
	must(payload.Validate())
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
