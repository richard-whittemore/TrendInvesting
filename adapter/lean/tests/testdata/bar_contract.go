// Command bar_contract checks adapter output against the Go completed-bar
// contract (ADR 0015, ADR 0004); Python tests feed the exact wire JSON on
// stdin.
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
	if envelope.Type != event.CompletedBarEventType || envelope.SchemaVersion != event.CompletedBarSchemaVersion {
		must(fmt.Errorf("unexpected completed bar type/schema: %s/%d", envelope.Type, envelope.SchemaVersion))
	}
	var payload event.CompletedBarPayload
	must(json.Unmarshal(envelope.Payload, &payload))
	must(payload.Validate())
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
