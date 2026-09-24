// Command run_stopped_contract checks adapter output against the Go
// adapter.run.stopped contract (ADR 0012, ADR 0015); Python tests feed the
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
	if envelope.Type != event.AdapterRunStoppedEventType || envelope.SchemaVersion != event.AdapterRunStoppedSchemaVersion {
		must(fmt.Errorf("unexpected adapter run stopped type/schema: %s/%d", envelope.Type, envelope.SchemaVersion))
	}
	var payload event.AdapterRunStoppedPayload
	must(json.Unmarshal(envelope.Payload, &payload))
	must(payload.Validate())
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
