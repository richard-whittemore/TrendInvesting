package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// AccountOf returns the account a journal belongs to: one engine and journal
// per account (docs/live-trading/plan.md section 3), named by its
// configuration event. A journal with none cannot be reported. Only the
// names are read from it; the engine validated the configuration before it
// ran.
func AccountOf(envelopes []event.Envelope) (accountID, strategyID string, err error) {
	for _, e := range envelopes {
		if e.Type != event.TargetWeightConfigurationEventType {
			continue
		}
		var p event.TargetWeightConfigurationPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return "", "", fmt.Errorf("report: decode %s %q: %w", e.Type, e.ID, err)
		}
		return p.AccountID, p.StrategyID, nil
	}
	return "", "", errors.New("report: the journal records no target-weight configuration, so its account is unknown")
}

// read decodes a record at one of the schema versions this build reads and
// validates it, as journalquery does: chain verification does not check
// payload values, so a record is validated before it can move a figure or
// raise an alert. A version outside versions is refused, never guessed at
// (ADR 0015).
func read(e event.Envelope, versions []uint32, into interface{ Validate() error }) error {
	known := false
	for _, v := range versions {
		known = known || e.SchemaVersion == v
	}
	if !known {
		return fmt.Errorf("report: %s %q is at schema version %d, which this build does not read (it reads %v)", e.Type, e.ID, e.SchemaVersion, versions)
	}
	if err := json.Unmarshal(e.Payload, into); err != nil {
		return fmt.Errorf("report: decode %s %q: %w", e.Type, e.ID, err)
	}
	err := into.Validate()
	if state, ok := into.(*event.EngineStatePayload); ok {
		err = state.ValidateSchema(e.SchemaVersion)
	}
	if err != nil {
		return fmt.Errorf("report: %s %q: %w", e.Type, e.ID, err)
	}
	return nil
}

// Schema versions this build reads, per record type.
var (
	barVersions         = []uint32{event.CompletedBarSchemaVersion}
	snapshotVersions    = []uint32{event.AccountSnapshotPositionsVersion}
	fillVersions        = []uint32{event.FillSchemaVersion}
	engineStateVersions = []uint32{event.EngineStateSchemaVersion, event.EngineStateScopedVersion}
	decidedVersions     = []uint32{event.TargetsDecidedSchemaVersion}
	plannedVersions     = []uint32{event.OrderPlannedSchemaVersion}
	releasedVersions    = []uint32{event.OrderReleasedSchemaVersion}
	refusedVersions     = []uint32{event.OrderRefusedSchemaVersion}
)

func dateOf(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// LastDate returns the UTC date of the journal's latest event: the day a
// summary run without a stated date reports.
func LastDate(envelopes []event.Envelope) (string, error) {
	var latest time.Time
	for _, e := range envelopes {
		if e.EventTime.After(latest) {
			latest = e.EventTime
		}
	}
	if latest.IsZero() {
		return "", errors.New("report: the journal records no events")
	}
	return dateOf(latest), nil
}
