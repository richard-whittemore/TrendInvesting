package event

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// CampaignCashInLieuEventType identifies the cash-in-lieu decision payload
// for the Envelope's Type field: a split's broker paid cash for fractional
// shares, and the reducer took the shares it could not deliver off the
// Campaign's most recent Units (CONTEXT.md: "Cash in lieu"; ADR 0023).
const CampaignCashInLieuEventType = "strategy.campaign.cash-in-lieu"

// CampaignCashInLieuSchemaVersion is the current schema version of
// CampaignCashInLieuPayload, for the Envelope's SchemaVersion field (ADR
// 0015).
const CampaignCashInLieuSchemaVersion uint32 = 1

// RuleCashInLieuMostRecentUnitsFirst names the rule for
// CampaignCashInLieuPayload.Rule: a split's shortfall takes one raw share
// off each of the Campaign's most recent Units, the most recent first, and
// never more than one per Unit (ADR 0023).
const RuleCashInLieuMostRecentUnitsFirst = "campaign.cash-in-lieu.most-recent-units-first"

// ADRSplitCashInLieu is the ADR CampaignCashInLieuPayload.ADR cites: ADR
// 0023.
const ADRSplitCashInLieu = "0023"

// UnitReduction is one Unit a split's cash in lieu reduced: by exactly one
// raw share, from QuantityBefore to QuantityAfter split-adjusted shares.
type UnitReduction struct {
	UnitIndex      int   `json:"unit_index"`
	QuantityBefore int64 `json:"quantity_before"`
	QuantityAfter  int64 `json:"quantity_after"`
}

// CampaignCashInLieuPayload records a split's cash in lieu applied to an open
// Campaign (ADR 0023): the split's terms as its corporate action stated them,
// every Unit reduced, most recent first, and the Campaign's holding before
// and after.
//
// Its figures are derived by exact integer arithmetic, so Validate checks
// them exactly: one reduction per raw share lost, each of exactly
// EngineSharesPerRawShare, in strictly descending Unit order, summing to
// EngineSharesLost, which is the holding's change. A split that paid cash
// without losing a share reduces no Unit and still records the cash. A split
// that paid nothing records no decision at all, so CashInLieu is positive.
type CampaignCashInLieuPayload struct {
	CampaignID   string `json:"campaign_id"`
	InstrumentID string `json:"instrument_id"`
	// CorporateActionID is the id of the market.corporate-action envelope
	// whose split this applies.
	CorporateActionID string    `json:"corporate_action_id"`
	EffectiveAt       time.Time `json:"effective_at"`
	// NewShares, OldShares, EngineSharesPerRawShare, RawSharesLost,
	// CashInLieu and Currency restate the corporate action's split terms
	// (CorporateActionPayload).
	NewShares               int64 `json:"new_shares"`
	OldShares               int64 `json:"old_shares"`
	EngineSharesPerRawShare int64 `json:"engine_shares_per_raw_share"`
	RawSharesLost           int64 `json:"raw_shares_lost"`
	// EngineSharesLost is RawSharesLost in the engine's split-adjusted
	// shares: what the Campaign's holding fell by.
	EngineSharesLost int64           `json:"engine_shares_lost"`
	CashInLieu       float64         `json:"cash_in_lieu"`
	Currency         string          `json:"currency"`
	Reductions       []UnitReduction `json:"reductions"`
	// QuantityBefore and QuantityAfter are the Campaign's whole holding in
	// split-adjusted shares either side of the reduction.
	QuantityBefore int64  `json:"quantity_before"`
	QuantityAfter  int64  `json:"quantity_after"`
	Rule           string `json:"rule"`
	ADR            string `json:"adr"`
}

// Validate checks every field and the exact arithmetic the payload's own doc
// comment states.
func (p CampaignCashInLieuPayload) Validate() error {
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
	errs = append(errs, splitRatioErrors(p.NewShares, p.OldShares, p.EngineSharesPerRawShare)...)
	if p.RawSharesLost < 0 {
		errs = append(errs, fmt.Errorf("raw shares lost must not be negative, got %d", p.RawSharesLost))
	}
	if !isFinite(p.CashInLieu) || p.CashInLieu <= 0 {
		errs = append(errs, fmt.Errorf("cash in lieu must be positive and finite, got %v: a split that paid nothing records no decision", p.CashInLieu))
	}
	if p.Currency == "" {
		errs = append(errs, errors.New("currency is required"))
	}
	errs = append(errs, p.reductionErrors()...)
	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid campaign cash in lieu payload: %w", err)
	}
	return nil
}

// reductionErrors checks the reductions and the holding against the shares
// lost, with every sum checked for int64 overflow, so a wrapped figure is
// refused rather than read as a small one.
func (p CampaignCashInLieuPayload) reductionErrors() []error {
	var errs []error
	if int64(len(p.Reductions)) != p.RawSharesLost {
		errs = append(errs, fmt.Errorf("%d reduction(s) for %d raw share(s) lost: one reduction per raw share lost, one raw share per Unit (ADR 0023)", len(p.Reductions), p.RawSharesLost))
	}
	var total int64
	overflow := false
	for i, r := range p.Reductions {
		if r.UnitIndex < 1 {
			errs = append(errs, fmt.Errorf("reduction %d: unit index must be at least 1, got %d", i+1, r.UnitIndex))
		}
		if i > 0 && r.UnitIndex >= p.Reductions[i-1].UnitIndex {
			errs = append(errs, fmt.Errorf("reduction %d: unit %d follows unit %d; Units are reduced most recent Unit first, each once (ADR 0023)", i+1, r.UnitIndex, p.Reductions[i-1].UnitIndex))
		}
		if r.QuantityAfter < 1 {
			errs = append(errs, fmt.Errorf("reduction %d: unit %d is left with %d shares; a reduced Unit keeps at least one share", i+1, r.UnitIndex, r.QuantityAfter))
			continue
		}
		if r.QuantityBefore <= r.QuantityAfter || r.QuantityBefore-r.QuantityAfter != p.EngineSharesPerRawShare {
			errs = append(errs, fmt.Errorf("reduction %d: unit %d goes from %d to %d shares, not by exactly one raw share of %d engine shares (ADR 0023)", i+1, r.UnitIndex, r.QuantityBefore, r.QuantityAfter, p.EngineSharesPerRawShare))
			continue
		}
		if total > math.MaxInt64-p.EngineSharesPerRawShare {
			overflow = true
			continue
		}
		total += p.EngineSharesPerRawShare
	}
	switch {
	case overflow:
		errs = append(errs, errors.New("engine shares lost overflows a whole number of shares"))
	case p.EngineSharesLost != total:
		errs = append(errs, fmt.Errorf("engine shares lost %d is not the %d the reductions sum to", p.EngineSharesLost, total))
	}
	switch {
	case p.QuantityAfter < 1:
		errs = append(errs, fmt.Errorf("quantity after must be positive, got %d: a reduction never closes a Campaign", p.QuantityAfter))
	case p.QuantityBefore < p.QuantityAfter || p.QuantityBefore-p.QuantityAfter != p.EngineSharesLost:
		errs = append(errs, fmt.Errorf("quantity after %d is not quantity before %d less engine shares lost %d", p.QuantityAfter, p.QuantityBefore, p.EngineSharesLost))
	}
	return errs
}
