// Command order_decisions_contract checks the adapter's decision fixtures
// against the Go decision payloads the adapter may receive (ADR 0015): each
// fixture must carry its type's current schema version and name only fields
// that type defines. Python tests feed a JSON array of decision envelopes on
// stdin. Covers both the decision types orders.py's SCHEMA_VERSIONS acts on
// and the ones it deliberately ignores (bookkeeping and diagnostic decisions
// that name no order of the reducer's own — ADR 0022's discipline applied to
// the adapter's own boundary, tests/test_orders.py's IgnoredDecisionTests).
// tests/testdata/wire_coverage.go keeps this switch in step with every
// decision type internal/event defines.
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
	// The decisions below carry no order of their own; orders.py's
	// OrderDesk.act ignores them (they are not in SCHEMA_VERSIONS). They are
	// still checked here so a fixture exists for every decision type the
	// adapter may receive (issue #31), not only the ones it acts on.
	case event.CampaignEvaluatedEventType:
		return event.CampaignEvaluatedSchemaVersion, &event.CampaignEvaluatedPayload{}, true
	case event.DrawdownStepAppliedEventType:
		return event.DrawdownStepAppliedSchemaVersion, &event.DrawdownStepAppliedPayload{}, true
	case event.NotionalAccountCashAdjustedEventType:
		return event.NotionalAccountCashAdjustedSchemaVersion, &event.NotionalAccountCashAdjustedPayload{}, true
	case event.NotionalAccountRebasedEventType:
		return event.NotionalAccountRebasedSchemaVersion, &event.NotionalAccountRebasedPayload{}, true
	case event.NotionalAccountRecoveredEventType:
		return event.NotionalAccountRecoveredSchemaVersion, &event.NotionalAccountRecoveredPayload{}, true
	case event.ProposalDeclinedEventType:
		return event.ProposalDeclinedSchemaVersion, &event.ProposalDeclinedPayload{}, true
	case event.EngineStateEventType:
		return event.EngineStateSchemaVersion, &event.EngineStatePayload{}, true
	case event.SetupEvaluatedEventType:
		return event.SetupEvaluatedSchemaVersion, &event.SetupEvaluatedPayload{}, true
	case event.SignalEventType:
		return event.SignalSchemaVersion, &event.SignalPayload{}, true
	case event.ProtectiveStopSetEventType:
		return event.ProtectiveStopSetSchemaVersion, &event.ProtectiveStopSetPayload{}, true
	}
	return 0, nil, false
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
