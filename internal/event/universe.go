package event

import (
	"errors"
	"fmt"
	"time"
)

// MarketInstrumentClassificationEventType identifies a fact about an
// instrument's own listing that ADR 0009's eligibility test reads: whether
// it is common stock, and whether its primary listing is a US exchange
// (CONTEXT.md: "Universe"; ADR 0009). Namespaced "market.", matching
// MarketCorporateActionEventType: a fact about the market this system did
// not produce and cannot re-derive from bars — internal/universe's Port is
// the seam that supplies it, and this is how a Port's answer becomes a
// journalled, replayable fact rather than a live dependency a replay would
// have to re-invoke identically (ADR 0009's own Consequences: "membership
// changes only through declared criteria on declared dates, so it can be
// reproduced exactly on replay").
const MarketInstrumentClassificationEventType = "market.instrument-classification"

// MarketInstrumentClassificationSchemaVersion is the current schema version
// of InstrumentClassificationPayload, for the Envelope's SchemaVersion
// field.
const MarketInstrumentClassificationSchemaVersion uint32 = 1

// The four SecurityType values InstrumentClassificationPayload.SecurityType
// accepts, mirroring internal/universe.SecurityType's own four values. The
// two packages declare their own enumerations rather than one importing the
// other's (internal/universe stays free of the wire contract, matching
// internal/sizing's own independence from internal/event — see
// sizingModeFor in internal/strategy for the identical reason), so exactly
// one place bridges them: internal/strategy's securityTypeFor.
const (
	SecurityTypeCommonStock = "common_stock"
	SecurityTypeETF         = "etf"
	SecurityTypeADR         = "adr"
	SecurityTypeSPAC        = "spac"
)

// InstrumentClassificationPayload records a fact about an instrument's own
// listing, external to any decision this system made: the same kind of
// fact CorporateActionPayload's own doc comment describes, but declared
// once rather than dated to a moment a listing changed. Unlike a corporate
// action, a later declaration for the same instrument does not need to
// precede any particular bar: it only affects a monthly eligibility
// evaluation that has not yet run, never a decision already made from an
// earlier declaration.
type InstrumentClassificationPayload struct {
	InstrumentID string `json:"instrument_id"`
	// SecurityType is one of the SecurityType* constants: a closed set, so a
	// future instrument class (a REIT, a unit trust) is a new recognised
	// value added deliberately, never a string a producer could send today
	// and have silently misread as common stock.
	SecurityType string `json:"security_type"`
	// USPrimaryExchange is whether the instrument's primary listing is a US
	// exchange (ADR 0009: "no ETFs, ADRs, or SPACs" trade on a foreign
	// primary exchange either, so this is checked independently of
	// SecurityType, not implied by it).
	USPrimaryExchange bool `json:"us_primary_exchange"`
	// EffectiveAt is when this classification was declared true, for the
	// audit trail: a reader can see when a fact was known, even though
	// nothing here orders it against any particular bar (see the type's own
	// doc comment).
	EffectiveAt time.Time `json:"effective_at"`
}

// Validate checks that the payload identifies an instrument, that
// SecurityType is one of the recognised values, and that EffectiveAt is
// present and writable.
func (p InstrumentClassificationPayload) Validate() error {
	var errs []error
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	switch p.SecurityType {
	case SecurityTypeCommonStock, SecurityTypeETF, SecurityTypeADR, SecurityTypeSPAC:
		// recognised
	default:
		errs = append(errs, fmt.Errorf("security type %q is not a recognised security type", p.SecurityType))
	}
	switch {
	case p.EffectiveAt.IsZero():
		errs = append(errs, errors.New("effective at is required"))
	case !writableTime(p.EffectiveAt):
		errs = append(errs, errors.New("effective at cannot be written as RFC 3339"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid instrument classification payload: %w", err)
	}
	return nil
}

// UniverseEligibilityEventType identifies ADR 0009's monthly, point-in-time
// eligibility decision for the Envelope's Type field: the outcome of
// applying the Baseline universe's criteria to one instrument on the first
// trading day of a calendar month (CONTEXT.md: "Universe", "Eligible").
const UniverseEligibilityEventType = "strategy.universe.eligibility"

// UniverseEligibilitySchemaVersion is the current schema version of
// UniverseEligibilityPayload, for the Envelope's SchemaVersion field.
const UniverseEligibilitySchemaVersion uint32 = 1

// RuleUniverseEligibility names the rule for UniverseEligibilityPayload.Rule:
// ADR 0009's four-criterion Baseline universe test (CONTEXT.md: "Universe",
// "Eligible").
const RuleUniverseEligibility = "universe.eligibility"

// ADRUniverseEligibility is the ADR UniverseEligibilityPayload.ADR cites:
// ADR 0009, which defines the Baseline universe's criteria and the rule
// that losing eligibility never closes an open Campaign.
const ADRUniverseEligibility = "0009"

// UniverseEligibilityPayload carries ADR 0009's eligibility decision for one
// instrument, evaluated point-in-time on the first trading day of a
// calendar month: the overall Eligible outcome, and which of the four
// criteria it was decided from, so a reviewer can see exactly which
// criterion excluded an instrument rather than only the outcome.
//
// Losing eligibility never closes an open Campaign (ADR 0009): this event
// records the evaluation only, and carries no consequence for a Campaign
// already open in the instrument. A strategy layer reading this event
// decides, from Eligible alone, whether the instrument remains a candidate
// for a NEW Campaign — it never reads this event as a reason to close an
// existing one.
type UniverseEligibilityPayload struct {
	InstrumentID string    `json:"instrument_id"`
	PeriodEnd    time.Time `json:"period_end"`
	Eligible     bool      `json:"eligible"`

	// ClassificationEligible is ADR 0009's first criterion: common stock on
	// a US primary exchange, no ETFs, ADRs, or SPACs.
	ClassificationEligible bool `json:"classification_eligible"`

	// Price is the raw close (ADR 0004, as amended) the price criterion was
	// measured from, and PriceEligible is whether it meets the configured
	// floor (at least $5 in the Baseline).
	Price         float64 `json:"price"`
	PriceEligible bool    `json:"price_eligible"`

	// DollarVolume is the 20-day median dollar volume
	// (indicator.MedianDollarVolume, the raw view — the same definition ADR
	// 0010's ranking tie-break reads), and DollarVolumeEligible is whether it
	// meets the configured floor (at least $5,000,000 in the Baseline). Zero
	// legitimately means either a genuinely zero median or a window not yet
	// full; DollarVolumeEligible is false in both cases.
	DollarVolume         float64 `json:"dollar_volume"`
	DollarVolumeEligible bool    `json:"dollar_volume_eligible"`

	// CompletedBars is the total completed bars this run has accepted for
	// the instrument, and HistoryEligible is whether it meets the configured
	// floor (at least 250 in the Baseline).
	CompletedBars   int  `json:"completed_bars"`
	HistoryEligible bool `json:"history_eligible"`

	// Rule and ADR name the strategy rule that produced this decision
	// (docs/development.md principle 3).
	Rule string `json:"rule"`
	ADR  string `json:"adr"`
}

// Validate checks that the payload identifies an instrument and period,
// that Price, DollarVolume and CompletedBars are non-negative and finite
// where applicable, and that Eligible is internally consistent with the
// four criteria: true if and only if every one of them is.
func (p UniverseEligibilityPayload) Validate() error {
	var errs []error
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	switch {
	case p.PeriodEnd.IsZero():
		errs = append(errs, errors.New("period end is required"))
	case !writableTime(p.PeriodEnd):
		errs = append(errs, errors.New("period end cannot be written as RFC 3339"))
	}
	switch {
	case !isFinite(p.Price):
		errs = append(errs, errors.New("price must be finite"))
	case p.Price < 0:
		errs = append(errs, errors.New("price must not be negative"))
	}
	switch {
	case !isFinite(p.DollarVolume):
		errs = append(errs, errors.New("dollar volume must be finite"))
	case p.DollarVolume < 0:
		errs = append(errs, errors.New("dollar volume must not be negative"))
	}
	if p.CompletedBars < 0 {
		errs = append(errs, errors.New("completed bars must not be negative"))
	}
	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}
	wantEligible := p.ClassificationEligible && p.PriceEligible && p.DollarVolumeEligible && p.HistoryEligible
	if p.Eligible != wantEligible {
		errs = append(errs, fmt.Errorf("eligible %v does not match its own four criteria (classification %v, price %v, dollar volume %v, history %v): eligible must be exactly their conjunction",
			p.Eligible, p.ClassificationEligible, p.PriceEligible, p.DollarVolumeEligible, p.HistoryEligible))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid universe eligibility payload: %w", err)
	}
	return nil
}
