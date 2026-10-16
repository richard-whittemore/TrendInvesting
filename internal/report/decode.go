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
// configuration event. A journal with none cannot be reported.
func AccountOf(envelopes []event.Envelope) (accountID, strategyID string, err error) {
	for _, e := range envelopes {
		if e.Type != event.TargetWeightConfigurationEventType {
			continue
		}
		var p event.TargetWeightConfigurationPayload
		if err := decode(e, &p); err != nil {
			return "", "", err
		}
		return p.AccountID, p.StrategyID, nil
	}
	return "", "", errors.New("report: the journal records no target-weight configuration, so its account is unknown")
}

func decode(e event.Envelope, into any) error {
	if err := json.Unmarshal(e.Payload, into); err != nil {
		return fmt.Errorf("report: decode %s %q: %w", e.Type, e.ID, err)
	}
	return nil
}

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
