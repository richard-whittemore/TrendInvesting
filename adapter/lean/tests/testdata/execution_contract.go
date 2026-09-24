// Command execution_contract checks the inputs the adapter builds from LEAN's
// order reports against Go's own contracts (ADR 0015): each envelope must
// validate, carry its type's current schema version, name only the fields
// its payload defines, and pass that payload's Validate. Python tests feed a
// JSON array of the exact wire envelopes on stdin.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

type validated interface{ Validate() error }

func main() {
	var envelopes []event.Envelope
	must(json.NewDecoder(os.Stdin).Decode(&envelopes))
	for _, envelope := range envelopes {
		must(envelope.Validate())
		var version uint32
		var payload validated
		switch envelope.Type {
		case event.FillEventType:
			version, payload = event.FillSchemaVersion, &event.FillPayload{}
		case event.OrderLifecycleEventType:
			version, payload = event.OrderLifecycleSchemaVersion, &event.OrderLifecyclePayload{}
		default:
			must(fmt.Errorf("%s: %q is not an input the adapter builds from an order report", envelope.ID, envelope.Type))
		}
		if envelope.SchemaVersion != version {
			must(fmt.Errorf("%s: %s schema version %d, Go's is %d", envelope.ID, envelope.Type, envelope.SchemaVersion, version))
		}
		decoder := json.NewDecoder(bytes.NewReader(envelope.Payload))
		decoder.DisallowUnknownFields()
		must(decoder.Decode(payload))
		must(payload.Validate())
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
