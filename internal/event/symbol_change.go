package event

import (
	"errors"
	"fmt"
	"time"
)

// InstrumentSymbolChangedEventType identifies the symbol-change decision
// payload for the Envelope's Type field: an instrument's open Campaign, its
// frozen values, its indicator history, its universe classification and its
// resting orders continue under a new instrument id (ADR 0024). It is not a
// delisting: nothing here closes, and CampaignID, when the instrument held
// one, is unchanged.
const InstrumentSymbolChangedEventType = "strategy.instrument.symbol-changed"

// InstrumentSymbolChangedSchemaVersion is the current schema version of
// InstrumentSymbolChangedPayload, for the Envelope's SchemaVersion field
// (ADR 0015).
const InstrumentSymbolChangedSchemaVersion uint32 = 1

// RuleSymbolChangeCarriesInstrumentState names the rule for
// InstrumentSymbolChangedPayload.Rule: a symbol change carries an
// instrument's Campaign, indicator history, universe classification and
// resting orders forward under a new instrument id, rather than closing the
// old identity and opening a new one (ADR 0024).
const RuleSymbolChangeCarriesInstrumentState = "instrument.symbol-change.carries-state"

// ADRSymbolChangeCarriesInstrumentState is the ADR
// InstrumentSymbolChangedPayload.ADR cites: ADR 0024.
const ADRSymbolChangeCarriesInstrumentState = "0024"

// InstrumentSymbolChangedPayload records a symbol change (ADR 0024): the
// instrument id an instrument's whole state continued under is
// NewInstrumentID; InstrumentID is the id it is leaving, which this reducer
// tracks no further business under. CampaignID is empty when the instrument
// held no open Campaign at the time — a Setup, not yet in a Campaign, is
// still carried across, but has no Campaign of its own to name.
type InstrumentSymbolChangedPayload struct {
	InstrumentID    string `json:"instrument_id"`
	NewInstrumentID string `json:"new_instrument_id"`
	// CorporateActionID is the id of the market.corporate-action envelope
	// this decision restates.
	CorporateActionID string    `json:"corporate_action_id"`
	EffectiveAt       time.Time `json:"effective_at"`
	// CampaignID names the open Campaign carried across, when there was one.
	CampaignID string `json:"campaign_id,omitempty"`
	Rule       string `json:"rule"`
	ADR        string `json:"adr"`
}

// Validate checks every required field is present and that NewInstrumentID
// differs from InstrumentID.
func (p InstrumentSymbolChangedPayload) Validate() error {
	var errs []error
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	switch p.NewInstrumentID {
	case "":
		errs = append(errs, errors.New("new instrument id is required"))
	case p.InstrumentID:
		errs = append(errs, fmt.Errorf("new instrument id %q is the same as instrument id: a symbol change must name a different instrument", p.NewInstrumentID))
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
	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid instrument symbol changed payload: %w", err)
	}
	return nil
}
