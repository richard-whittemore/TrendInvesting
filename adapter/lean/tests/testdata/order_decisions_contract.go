// Command order_decisions_contract checks the adapter's order-test fixtures
// against the Go decision payloads the adapter reads (ADR 0015): each fixture
// must carry its type's current schema version and name only fields that
// type defines. Python tests feed a JSON array of decision envelopes on stdin.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

type fixture struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	SchemaVersion uint32          `json:"schema_version"`
	Payload       json.RawMessage `json:"payload"`
}

func main() {
	var fixtures []fixture
	must(json.NewDecoder(os.Stdin).Decode(&fixtures))
	for _, f := range fixtures {
		version, payload, ok := contract(f.Type)
		if !ok {
			must(fmt.Errorf("%s: %q is not a decision type the adapter acts on", f.ID, f.Type))
		}
		if f.SchemaVersion != version {
			must(fmt.Errorf("%s: %s schema version %d, Go's is %d", f.ID, f.Type, f.SchemaVersion, version))
		}
		decoder := json.NewDecoder(bytes.NewReader(f.Payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(payload); err != nil {
			must(fmt.Errorf("%s: %s: %w", f.ID, f.Type, err))
		}
	}
}

func contract(eventType string) (uint32, any, bool) {
	switch eventType {
	case event.TradeProposalEventType:
		return event.TradeProposalSchemaVersion, &event.TradeProposalPayload{}, true
	case event.AddProposalEventType:
		return event.AddProposalSchemaVersion, &event.AddProposalPayload{}, true
	case event.ProposalExpiredEventType:
		return event.ProposalExpiredSchemaVersion, &event.ProposalExpiredPayload{}, true
	case event.CampaignOpenedEventType:
		return event.CampaignOpenedSchemaVersion, &event.CampaignOpenedPayload{}, true
	case event.ExitOrderSetEventType:
		return event.ExitOrderSetSchemaVersion, &event.ExitOrderSetPayload{}, true
	case event.ExitProposalEventType:
		return event.ExitProposalSchemaVersion, &event.ExitProposalPayload{}, true
	case event.CampaignUnitAddedEventType:
		return event.CampaignUnitAddedSchemaVersion, &event.CampaignUnitAddedPayload{}, true
	case event.CampaignUnitsStoppedEventType:
		return event.CampaignUnitsStoppedSchemaVersion, &event.CampaignUnitsStoppedPayload{}, true
	case event.CampaignExitedEventType:
		return event.CampaignExitedSchemaVersion, &event.CampaignExitedPayload{}, true
	}
	return 0, nil, false
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
