package event

import (
	"errors"
	"fmt"
	"time"
)

// CampaignDividendEventType identifies the dividend decision payload for the
// Envelope's Type field: a cash dividend was credited to an open Campaign's
// account (CONTEXT.md: "Cash in lieu" states the sibling rule for a split;
// ADR 0004, ADR 0024).
const CampaignDividendEventType = "strategy.campaign.dividend"

// CampaignDividendSchemaVersion is the current schema version of
// CampaignDividendPayload, for the Envelope's SchemaVersion field (ADR 0015).
const CampaignDividendSchemaVersion uint32 = 1

// RuleDividendCreditedAsCash names the rule for CampaignDividendPayload.Rule:
// a dividend is credited to the account as cash and changes no channel
// level, N, or any price the rules see (ADR 0004, ADR 0024).
const RuleDividendCreditedAsCash = "campaign.dividend.credited-as-cash"

// ADRDividendCreditedAsCash is the ADR CampaignDividendPayload.ADR cites: ADR
// 0024.
const ADRDividendCreditedAsCash = "0024"

// CampaignDividendPayload records a dividend credited to an open Campaign
// (ADR 0024): the corporate action's own terms, restated, and the Campaign
// they were credited to.
//
// Unlike a split's cash in lieu, a dividend changes no Unit's quantity and
// re-rests no Exit Order: the cash is the whole effect. It reaches spendable
// cash only through the next previous-close snapshot, exactly like cash in
// lieu (ADR 0020) — this decision records the credit; it does not apply it to
// spendable cash itself.
type CampaignDividendPayload struct {
	CampaignID   string `json:"campaign_id"`
	InstrumentID string `json:"instrument_id"`
	// CorporateActionID is the id of the market.corporate-action envelope
	// this dividend restates.
	CorporateActionID string    `json:"corporate_action_id"`
	EffectiveAt       time.Time `json:"effective_at"`
	// CashAmount and Currency restate the corporate action's dividend terms
	// (CorporateActionPayload).
	CashAmount float64 `json:"cash_amount"`
	Currency   string  `json:"currency"`
	Rule       string  `json:"rule"`
	ADR        string  `json:"adr"`
}

// Validate checks every field is present and CashAmount is positive and
// finite.
func (p CampaignDividendPayload) Validate() error {
	var errs []error
	if p.CampaignID == "" {
		errs = append(errs, errors.New("campaign id is required"))
	}
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if p.CorporateActionID == "" {
		errs = append(errs, errors.New("corporate action id is required"))
	}
	switch {
	case p.EffectiveAt.IsZero():
		errs = append(errs, errors.New("effective at is required"))
	case !writableTime(p.EffectiveAt):
		errs = append(errs, errors.New("effective at cannot be written as RFC 3339"))
	}
	if !isFinite(p.CashAmount) || p.CashAmount <= 0 {
		errs = append(errs, fmt.Errorf("cash amount must be positive and finite, got %v: a dividend that paid nothing records no decision", p.CashAmount))
	}
	if p.Currency == "" {
		errs = append(errs, errors.New("currency is required"))
	}
	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid campaign dividend payload: %w", err)
	}
	return nil
}
