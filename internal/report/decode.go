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
// configuration event. A journal with none cannot be reported. The
// configuration is read like every other record: only at a schema version
// this build reads, and only if it validates (ADR 0015), since an unread
// layout would name no account and mis-key every alert.
func AccountOf(envelopes []event.Envelope) (accountID, strategyID string, err error) {
	for _, e := range envelopes {
		if e.Type != event.TargetWeightConfigurationEventType {
			continue
		}
		var p event.TargetWeightConfigurationPayload
		if err := read(e, configurationVersions, &p); err != nil {
			return "", "", err
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
	// A payload whose fields depend on its version is checked against the
	// version it was recorded at, so a record carrying a later version's
	// field is refused (ADR 0015).
	validate := into.Validate
	if v, ok := into.(interface{ ValidateSchema(uint32) error }); ok {
		validate = func() error { return v.ValidateSchema(e.SchemaVersion) }
	}
	if err := validate(); err != nil {
		return fmt.Errorf("report: %s %q: %w", e.Type, e.ID, err)
	}
	return nil
}

// Schema versions this build reads, per record type.
var (
	configurationVersions = []uint32{event.TargetWeightConfigurationSchemaVersion}
	sessionVersions       = []uint32{event.SessionClosedSchemaVersion}
	// A bar is read at version 1, which the Turtle engine writes, and at
	// version 2, which the target-weight engines write (ADR 0026).
	barVersions         = []uint32{event.CompletedBarSchemaVersion, event.CompletedBarAdjustedVersion}
	snapshotVersions    = []uint32{event.AccountSnapshotPositionsVersion}
	fillVersions        = []uint32{event.FillSchemaVersion}
	engineStateVersions = []uint32{event.EngineStateSchemaVersion, event.EngineStateScopedVersion}
	decidedVersions     = []uint32{event.TargetsDecidedSchemaVersion}
	// The order records are read at every version the event package holds,
	// by the list every reader of the log shares.
	plannedVersions  = event.OrderPlannedVersions()
	releasedVersions = event.OrderReleasedVersions()
	refusedVersions  = event.OrderRefusedVersions()
	// A target-weight account's orders are limit orders, reported at
	// lifecycle version 2 and fill version 5 (ADR 0026, Consequences).
	lifecycleVersions = []uint32{event.OrderLifecycleLimitVersion}
	// syntheticVersions: a dry-run engine's own report, a decision, never
	// the venue's input (event.LimitOrderLifecycleVersions).
	syntheticVersions        = []uint32{event.OrderLifecycleSyntheticVersion}
	targetWeightFillVersions = []uint32{event.FillTargetWeightVersion}
	// The owner's clearance of a momentum run's rebalance in progress (ADR
	// 0026, point 5).
	clearedVersions = []uint32{event.RebalanceClearedSchemaVersion}
	// The reconciliation's result and the owner's acknowledgement (ADR 0019).
	reconciliationVersions = []uint32{event.ReconciliationResultSchemaVersion}
	acknowledgedVersions   = []uint32{event.AlertAcknowledgedSchemaVersion}
	// A fresh run's approval, for the incident it carries in force: version
	// 1, which could not state a cash scope, and the current version.
	approvalVersions = []uint32{1, event.OperatorRunApprovedSchemaVersion}
)

func dateOf(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// LastDate returns the UTC date of the journal's latest event: the day the
// dashboard shows. The reporter's daily summary waits for a closed session
// instead (ClosedSessionDate), since it is sent once per day.
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

// ClosedSessionDate returns the UTC date of the latest session the log
// records as closed. A day's decision, orders and gate records are written in
// the group its market.session.closed input opens (ADR 0021; ADR 0027,
// section 2), so a day with that record is complete and one without it is
// not yet. A session record this build does not read closes no day.
func ClosedSessionDate(envelopes []event.Envelope) (string, bool) {
	var latest time.Time
	for _, e := range envelopes {
		if closesSession(e) && e.EventTime.After(latest) {
			latest = e.EventTime
		}
	}
	if latest.IsZero() {
		return "", false
	}
	return dateOf(latest), true
}

// SessionClosedOn reports whether the log records a session closed on the
// UTC date.
func SessionClosedOn(envelopes []event.Envelope, date string) bool {
	for _, e := range envelopes {
		if closesSession(e) && dateOf(e.EventTime) == date {
			return true
		}
	}
	return false
}

func closesSession(e event.Envelope) bool {
	if e.Type != event.SessionClosedEventType {
		return false
	}
	var p event.SessionClosedPayload
	return read(e, sessionVersions, &p) == nil
}
