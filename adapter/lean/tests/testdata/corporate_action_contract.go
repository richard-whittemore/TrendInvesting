// Command corporate_action_contract checks adapter output against the Go
// market.corporate-action contract (ADRs 0015, 0023, 0024): the envelope, its
// schema version, and the payload read strictly through the payload's own
// upcaster, so a field Go does not define fails. Python tests feed the exact
// wire JSON on stdin.
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
	if envelope.Type != event.MarketCorporateActionEventType || envelope.SchemaVersion != event.MarketCorporateActionSchemaVersion {
		must(fmt.Errorf("unexpected corporate action type/schema: %s/%d", envelope.Type, envelope.SchemaVersion))
	}
	payload, err := event.UpcastCorporateActionPayload(envelope.SchemaVersion, envelope.Payload)
	must(err)
	if !payload.EffectiveAt.Equal(envelope.EventTime) {
		must(fmt.Errorf("event time %s is not the action's effective time %s", envelope.EventTime, payload.EffectiveAt))
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
