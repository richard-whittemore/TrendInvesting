package event

import (
	"errors"
	"fmt"
	"time"
)

// CampaignUnitsStoppedEventType identifies the per-fill decision payload for
// the Envelope's Type field: a stop fill closed one or more (but not
// necessarily all) of an open Campaign's Units (#15, the gap case).
//
// #12 made a stop fill close a Campaign's WHOLE filled quantity in one
// decision. #15's Stop Ladder means a gapped Unit can carry a stop level
// genuinely different from its Campaign-mates (The Turtle Rules p.23), so a
// stop fill must be able to name and close a SUBSET of Units — Unit 4 alone,
// in the gap fixture, while Units 1-3's own (lower) stops have not yet been
// reached. This event records exactly that: which Units this ONE fill
// closed, what it realised, and how many Units (and how much open risk)
// remain. It is emitted for EVERY stop fill, whether or not it happens to
// empty the Campaign; when it does, strategy.campaign.exited (Reason
// ExitReasonStop) follows in the same Apply return, aggregating the whole
// Campaign's life rather than only this fill's own share (see
// CampaignExitedPayload's own doc comment, "Multi-Unit aggregation" and
// "Accumulating partial stop-outs").
const CampaignUnitsStoppedEventType = "strategy.campaign.units-stopped"

// CampaignUnitsStoppedSchemaVersion is the current schema version of
// CampaignUnitsStoppedPayload, for the Envelope's SchemaVersion field.
const CampaignUnitsStoppedSchemaVersion uint32 = 1

// RuleCampaignUnitsStoppedByStop names the rule for
// CampaignUnitsStoppedPayload.Rule: a fill said one or more Units' own
// Protective Stop was hit.
const RuleCampaignUnitsStoppedByStop = "campaign.units-stopped.by-stop"

// CampaignUnitsStoppedPayload records ONE stop fill closing one or more
// Units of an open Campaign: which Units, at what price, what it realised
// for THEM alone, and what remains.
//
// EntryPrice, CampaignN, DollarsPerPoint and UnitQuantity are restated here
// for the identical reason CampaignExitedPayload restates them (see that
// type's own doc comment): Validate re-derives RealisedResult exactly, and a
// payload cannot re-derive a value from a field it does not have.
// EntryPrice is the quantity-weighted average fill price of ONLY the Units
// THIS fill closes — not the whole Campaign's own entryPrice() once earlier
// Units remain open or have already closed at a different price.
//
// # AggregateOpenRiskAfter is restated, not independently re-derivable here
//
// AggregateOpenRiskAfter is the Campaign's aggregate open risk (see
// internal/sizing.AggregateOpenRisk) computed over whatever Units remain
// OPEN after this fill — 0 when RemainingUnits is 0. This payload
// deliberately does NOT carry every remaining Unit's own entry and stop (the
// per-Unit detail needed to re-derive it): that detail already lives on
// every completed bar's strategy.campaign.evaluated event
// (CampaignEvaluatedPayload.Units), which Validate DOES re-derive it against
// exactly. Repeating the whole remaining-Units list here, on every stop
// fill, for a figure the very next bar's own event re-derives in full, would
// duplicate a payload's worth of per-Unit state rather than referencing it —
// so Validate here checks only the SHAPE of the number it was handed
// (finite, non-negative, and exactly zero when RemainingUnits is 0), the
// same restraint CampaignEvaluatedPayload.ProtectiveStop's own doc comment
// states applied to it before #15 added the Units list this event still does
// not carry.
type CampaignUnitsStoppedPayload struct {
	CampaignID   string `json:"campaign_id"`
	InstrumentID string `json:"instrument_id"`
	// FillID is the stop fill's producer-assigned id, recorded so this
	// closure can be reconciled against the execution record and a
	// re-delivered fill recognised as a duplicate rather than a further
	// closure.
	FillID string `json:"fill_id"`
	// UnitIndexes are the indexes of the Units THIS fill closed, in
	// ascending order (the order FillPayload.UnitIDs resolved to).
	UnitIndexes []int   `json:"unit_indexes"`
	FillPrice   float64 `json:"fill_price"`
	// QuantityClosed is the sum of the closed Units' own quantities — what
	// FillPayload.Quantity was checked to equal.
	QuantityClosed int64 `json:"quantity_closed"`
	// EntryPrice is the quantity-weighted average fill price of ONLY the
	// Units this fill closes (see the type's doc comment).
	EntryPrice      float64 `json:"entry_price"`
	CampaignN       float64 `json:"campaign_n"`
	DollarsPerPoint float64 `json:"dollars_per_point"`
	// RealisedResult is THIS fill's own share of the Campaign's result:
	// QuantityClosed x (FillPrice - EntryPrice) x DollarsPerPoint. Negative
	// for a loss, as a stop-out ordinarily is. This is not the Campaign's
	// whole-life result when earlier or later fills also closed part of it
	// — see CampaignExitedPayload.RealisedResult for that aggregate.
	RealisedResult float64   `json:"realised_result"`
	StoppedAt      time.Time `json:"stopped_at"`
	// RemainingUnits is how many Units are still open after this fill: 0
	// means this fill emptied the Campaign, and a strategy.campaign.exited
	// event (Reason ExitReasonStop) follows in the same Apply return.
	RemainingUnits int `json:"remaining_units"`
	// AggregateOpenRiskAfter is the aggregate open risk over whatever Units
	// remain open after this fill, 0 when RemainingUnits is 0 (see the
	// type's own doc comment, "AggregateOpenRiskAfter is restated").
	AggregateOpenRiskAfter float64 `json:"aggregate_open_risk_after"`
	// Rule and ADR name the rule that produced this decision
	// (docs/development.md principle 3).
	Rule string `json:"rule"`
	ADR  string `json:"adr"`
}

// Validate checks that the payload identifies the Campaign and the closing
// fill, that UnitIndexes is non-empty with strictly ascending positive
// indexes (no duplicates, no unordered list masking one), that every frozen
// number is usable, that RealisedResult matches its derivation from
// QuantityClosed, FillPrice, EntryPrice and DollarsPerPoint EXACTLY (the
// same exact-equality discipline every derived result in this package
// uses), that RemainingUnits is not negative, and that AggregateOpenRiskAfter
// is finite, non-negative, and exactly zero when RemainingUnits is 0 (see
// the type's own doc comment for why it is not independently re-derived
// here).
func (p CampaignUnitsStoppedPayload) Validate() error {
	var errs []error
	if p.CampaignID == "" {
		errs = append(errs, errors.New("campaign id is required"))
	}
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if p.FillID == "" {
		errs = append(errs, errors.New("fill id is required: a units-stopped decision must name the fill that closed them"))
	}

	if len(p.UnitIndexes) == 0 {
		errs = append(errs, errors.New("unit indexes is required: at least one unit must be named"))
	} else {
		for i, idx := range p.UnitIndexes {
			if idx < 1 {
				errs = append(errs, fmt.Errorf("unit indexes[%d] = %d, must be at least 1", i, idx))
			}
			if i > 0 && p.UnitIndexes[i-1] >= idx {
				errs = append(errs, fmt.Errorf("unit indexes must be strictly ascending with no duplicates, got %d at position %d after %d", idx, i, p.UnitIndexes[i-1]))
			}
		}
	}

	fillPriceFinite := isFinite(p.FillPrice)
	switch {
	case !fillPriceFinite:
		errs = append(errs, errors.New("fill price must be finite"))
	case p.FillPrice <= 0:
		errs = append(errs, errors.New("fill price must be positive"))
	}

	if p.QuantityClosed <= 0 {
		errs = append(errs, fmt.Errorf("quantity closed must be a positive whole number, got %d", p.QuantityClosed))
	}

	entryPriceFinite := isFinite(p.EntryPrice)
	switch {
	case !entryPriceFinite:
		errs = append(errs, errors.New("entry price must be finite"))
	case p.EntryPrice <= 0:
		errs = append(errs, errors.New("entry price must be positive"))
	}

	campaignNFinite := isFinite(p.CampaignN)
	switch {
	case !campaignNFinite:
		errs = append(errs, errors.New("campaign n must be finite"))
	case p.CampaignN <= 0:
		errs = append(errs, errors.New("campaign n must be positive"))
	}

	dollarsPerPointFinite := isFinite(p.DollarsPerPoint)
	switch {
	case !dollarsPerPointFinite:
		errs = append(errs, errors.New("dollars per point must be finite"))
	case p.DollarsPerPoint <= 0:
		errs = append(errs, errors.New("dollars per point must be positive"))
	}

	realisedResultFinite := isFinite(p.RealisedResult)
	if !realisedResultFinite {
		errs = append(errs, errors.New("realised result must be finite"))
	}

	if p.QuantityClosed > 0 && fillPriceFinite && entryPriceFinite && dollarsPerPointFinite && realisedResultFinite {
		if derived := float64(p.QuantityClosed) * (p.FillPrice - p.EntryPrice) * p.DollarsPerPoint; p.RealisedResult != derived {
			errs = append(errs, fmt.Errorf(
				"stated realised result %v does not match the derivation %v (quantity closed %d x (fill price %v - entry price %v) x dollars per point %v)",
				p.RealisedResult, derived, p.QuantityClosed, p.FillPrice, p.EntryPrice, p.DollarsPerPoint))
		}
	}

	if p.StoppedAt.IsZero() {
		errs = append(errs, errors.New("stopped at is required"))
	}

	if p.RemainingUnits < 0 {
		errs = append(errs, fmt.Errorf("remaining units must not be negative, got %d", p.RemainingUnits))
	}

	aggregateFinite := isFinite(p.AggregateOpenRiskAfter)
	switch {
	case !aggregateFinite:
		errs = append(errs, errors.New("aggregate open risk after must be finite"))
	case p.AggregateOpenRiskAfter < 0:
		errs = append(errs, errors.New("aggregate open risk after must not be negative"))
	case p.RemainingUnits == 0 && p.AggregateOpenRiskAfter != 0:
		errs = append(errs, fmt.Errorf("aggregate open risk after must be zero when remaining units is 0, got %v", p.AggregateOpenRiskAfter))
	}

	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid campaign units stopped payload: %w", err)
	}
	return nil
}
