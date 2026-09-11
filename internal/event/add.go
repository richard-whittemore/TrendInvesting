package event

import (
	"errors"
	"fmt"
	"time"
)

// AddProposalEventType identifies the Add-proposal decision payload for the
// Envelope's Type field: an open Campaign's next rung has been reached on
// this completed bar (CONTEXT.md: "Add", "Add Ladder"), and the reducer is
// proposing to add another Unit.
//
// Named "strategy.*", not "execution.*", mirroring ExitProposalEventType and
// TradeProposalEventType: this is a decision the strategy made, not an
// external fact — nothing here assumes a fill. ADR 0005 makes the Add a
// resting order at the rung, filled in the bar whose range first covers it;
// #18's fill simulator decides whether and at what price it actually filled.
//
// A separate payload from both TradeProposalPayload and ExitProposalPayload,
// for the same "closed set, not free text" reasoning ExitProposalPayload's
// own doc comment states: an Add proposal carries neither a fresh Signal's
// sizing inputs (it reuses the Campaign's own frozen campaignN and Unit
// quantity, ADR 0006) nor an exit's "close everything" shape — it names the
// Campaign it would extend, which Unit it would become, and the rung that
// was reached.
const AddProposalEventType = "strategy.add.proposed"

// AddProposalSchemaVersion is the current schema version of
// AddProposalPayload, for the Envelope's SchemaVersion field.
const AddProposalSchemaVersion uint32 = 1

// RuleAddLadderHalfN names the rule for AddProposalPayload.Rule and
// CampaignUnitAddedPayload.Rule: the next Unit is added half a campaign N
// above the PREVIOUS Unit's actual fill (The Turtle Rules p.19-20; ADR
// 0006 freezes the campaign N this rung is measured from).
const RuleAddLadderHalfN = "add.ladder.half-n"

// AddProposalPayload records the reducer proposing to add another Unit to an
// open Campaign: the whole of a Turtle Add Ladder rung (The Turtle Rules
// p.19-20), before any fill exists.
//
// Level is PreviousUnitFill + 0.5 x CampaignN (sizing.NextAddLevel; Validate
// re-derives it exactly, the same discipline every derived level in this
// package uses) — measured from the ACTUAL fill of the Unit immediately
// before the one this proposal would open, never from an intended or
// proposed level, so that slippage on one fill pushes every later rung out
// accordingly [T p.19]. What actually fills there is not decided here: ADR
// 0005 makes it a resting order, and #18's fill simulator decides the
// executed price.
//
// Quantity is always the Campaign's frozen UnitQuantity (ADR 0006): an Add
// is never resized from the current Notional Account — the whole Add Ladder
// is computable at entry from the frozen campaign N and Unit share count, so
// a later Add sized from a drifted account figure would silently break that
// invariant and risk more or less than the ladder promised.
//
// Deliberately out of scope, so nothing here should be read as having
// considered them:
//
//   - Whether the fill actually happens, and at what price. ADR 0005 and #18
//     own that; a proposal is not a fill.
//   - Caps (ADR 0008). A proposal is not a permission to trade.
//   - Exit precedence (ADR 0010). This proposal exists only when the SAME
//     bar did not already propose an exit — enforced by where
//     internal/strategy.Reducer evaluates an Add relative to an exit, not by
//     anything on this payload.
type AddProposalPayload struct {
	// CampaignID identifies the Campaign this proposal would extend.
	CampaignID   string    `json:"campaign_id"`
	InstrumentID string    `json:"instrument_id"`
	PeriodEnd    time.Time `json:"period_end"`
	// UnitIndex is the Unit this proposal would become if filled: 2 through
	// the Campaign's configured maximum (Unit 1 is the Campaign's own
	// opening fill, never proposed here).
	UnitIndex int `json:"unit_index"`
	// Level is the rung that was reached (see the type's doc comment).
	Level float64 `json:"level"`
	// Quantity is the Campaign's frozen UnitQuantity (see the type's doc
	// comment) — never resized from the current Notional Account.
	Quantity int64 `json:"quantity"`
	// PreviousUnitFill is the actual fill price Level was measured from.
	PreviousUnitFill float64 `json:"previous_unit_fill"`
	// CampaignN is the Campaign's frozen campaign N (ADR 0006), restated so
	// Level is independently re-derivable from this payload alone.
	CampaignN float64 `json:"campaign_n"`
	// Rule and ADR name the rule that produced this decision
	// (docs/development.md principle 3).
	Rule string `json:"rule"`
	ADR  string `json:"adr"`
}

// Validate checks that the payload identifies the Campaign, the instrument
// and the period, that UnitIndex is at least 2 (Unit 1 is never an Add),
// that Quantity is a positive whole number, that PreviousUnitFill and
// CampaignN are usable, that Rule and ADR are present, and that Level
// matches its derivation EXACTLY — the same exact-equality discipline every
// derived level in this package uses (CampaignOpenedPayload.ProtectiveStop,
// ProtectiveStopSetPayload.Level): a tolerance would let a
// differently-derived rung through, which is the defect the check exists to
// catch.
func (p AddProposalPayload) Validate() error {
	var errs []error
	if p.CampaignID == "" {
		errs = append(errs, errors.New("campaign id is required"))
	}
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if p.PeriodEnd.IsZero() {
		errs = append(errs, errors.New("period end is required"))
	}
	if p.UnitIndex < 2 {
		errs = append(errs, fmt.Errorf("unit index must be at least 2, got %d: unit 1 is the campaign's own opening fill, never an add proposal", p.UnitIndex))
	}
	if p.Quantity <= 0 {
		errs = append(errs, fmt.Errorf("quantity must be a positive whole number, got %d", p.Quantity))
	}

	previousFillFinite := isFinite(p.PreviousUnitFill)
	switch {
	case !previousFillFinite:
		errs = append(errs, errors.New("previous unit fill must be finite"))
	case p.PreviousUnitFill <= 0:
		errs = append(errs, errors.New("previous unit fill must be positive"))
	}

	campaignNFinite := isFinite(p.CampaignN)
	switch {
	case !campaignNFinite:
		errs = append(errs, errors.New("campaign n must be finite"))
	case p.CampaignN <= 0:
		errs = append(errs, errors.New("campaign n must be positive: the add ladder is computed from it (ADR 0006)"))
	}

	levelFinite := isFinite(p.Level)
	switch {
	case !levelFinite:
		errs = append(errs, errors.New("level must be finite"))
	case p.Level <= 0:
		errs = append(errs, errors.New("level must be positive"))
	}

	if previousFillFinite && campaignNFinite && levelFinite {
		if derived := p.PreviousUnitFill + 0.5*p.CampaignN; p.Level != derived {
			errs = append(errs, fmt.Errorf(
				"stated level %v does not match the derivation %v (previous unit fill %v + 0.5 x campaign n %v): the add ladder is measured from the actual fill (The Turtle Rules p.19)",
				p.Level, derived, p.PreviousUnitFill, p.CampaignN))
		}
	}

	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid add proposal payload: %w", err)
	}
	return nil
}

// CampaignUnitAddedEventType identifies the Unit-added decision payload for
// the Envelope's Type field: an Add fill executed, and a further Unit joined
// the Campaign (CONTEXT.md: "Add").
//
// Like Campaign-opened, this is a *decision* event even though a fill caused
// it: the fill is the external fact, and this is the strategy stating what
// that fact means for its own position state.
const CampaignUnitAddedEventType = "strategy.campaign.unit-added"

// CampaignUnitAddedSchemaVersion is the current schema version of
// CampaignUnitAddedPayload, for the Envelope's SchemaVersion field.
const CampaignUnitAddedSchemaVersion uint32 = 1

// CampaignUnitAddedPayload records a further Unit joining an open Campaign,
// and the fresh Protective Stop set for that Unit ALONE.
//
// ProtectiveStop is this Unit's OWN stop — FillPrice - StopMultiple x
// CampaignN, measured from THIS Unit's actual fill, exactly as
// CampaignOpenedPayload.ProtectiveStop is measured from Unit 1's. It is
// deliberately not a move of any earlier Unit's stop: #15's Stop Ladder is
// what raises earlier Units' stops as later ones are added; this ticket only
// ever sets a brand new stop for the Unit that was just added (see the
// accompanying strategy.protective-stop.set event, emitted for this Unit
// alone, in the same Apply return).
//
// Units is the Campaign's total Unit count AFTER this Add — always equal to
// UnitIndex, since Units are added strictly in order; Validate checks the
// two agree, so a producer that ever let them drift fails rather than
// journaling a silently inconsistent count.
type CampaignUnitAddedPayload struct {
	CampaignID   string `json:"campaign_id"`
	InstrumentID string `json:"instrument_id"`
	// UnitIndex is which Unit this is: 2 through the Campaign's configured
	// maximum.
	UnitIndex int `json:"unit_index"`
	// FillID is the Add fill's producer-assigned id, recorded so this Unit
	// can be reconciled against the execution record and a re-delivered fill
	// recognised as a duplicate rather than a further Unit.
	FillID    string  `json:"fill_id"`
	FillPrice float64 `json:"fill_price"`
	// Quantity is what actually executed for this Unit — at most the
	// Campaign's frozen UnitQuantity; a partial Add is accepted for the
	// filled quantity, mirroring the Campaign's own opening fill (#11).
	Quantity int64 `json:"quantity"`
	// CampaignN and StopMultiple are restated from the Campaign so
	// ProtectiveStop is independently re-derivable from this payload alone,
	// the same reasoning ProtectiveStopSetPayload's own restated fields
	// follow.
	CampaignN      float64 `json:"campaign_n"`
	StopMultiple   float64 `json:"stop_multiple"`
	ProtectiveStop float64 `json:"protective_stop"`
	// Units is the Campaign's total Unit count after this Add (see the
	// type's doc comment).
	Units   int       `json:"units"`
	AddedAt time.Time `json:"added_at"`
	// Rule and ADR name the rule that produced this decision
	// (docs/development.md principle 3).
	Rule string `json:"rule"`
	ADR  string `json:"adr"`
}

// Validate checks that the payload identifies the Campaign and the Add fill
// that caused it, that UnitIndex is at least 2 and equals Units, that every
// frozen number is usable, and that ProtectiveStop matches its derivation
// EXACTLY, is positive, and sits strictly below FillPrice — the same
// long-only shape CampaignOpenedPayload.ProtectiveStop enforces for Unit 1.
func (p CampaignUnitAddedPayload) Validate() error {
	var errs []error
	if p.CampaignID == "" {
		errs = append(errs, errors.New("campaign id is required"))
	}
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if p.UnitIndex < 2 {
		errs = append(errs, fmt.Errorf("unit index must be at least 2, got %d: unit 1 is the campaign's own opening fill, never an add", p.UnitIndex))
	}
	if p.FillID == "" {
		errs = append(errs, errors.New("fill id is required: an add must name the fill that added it"))
	}

	fillPriceFinite := isFinite(p.FillPrice)
	switch {
	case !fillPriceFinite:
		errs = append(errs, errors.New("fill price must be finite"))
	case p.FillPrice <= 0:
		errs = append(errs, errors.New("fill price must be positive"))
	}

	if p.Quantity <= 0 {
		errs = append(errs, fmt.Errorf("quantity must be a positive whole number, got %d", p.Quantity))
	}

	campaignNFinite := isFinite(p.CampaignN)
	switch {
	case !campaignNFinite:
		errs = append(errs, errors.New("campaign n must be finite"))
	case p.CampaignN <= 0:
		errs = append(errs, errors.New("campaign n must be positive"))
	}

	stopMultipleFinite := isFinite(p.StopMultiple)
	switch {
	case !stopMultipleFinite:
		errs = append(errs, errors.New("stop multiple must be finite"))
	case p.StopMultiple <= 0:
		errs = append(errs, errors.New("stop multiple must be positive"))
	}

	stopFinite := isFinite(p.ProtectiveStop)
	switch {
	case !stopFinite:
		errs = append(errs, errors.New("protective stop must be finite"))
	case p.ProtectiveStop <= 0:
		errs = append(errs, errors.New("protective stop must be positive: a long position cannot be stopped out at or below zero"))
	case fillPriceFinite && p.ProtectiveStop >= p.FillPrice:
		errs = append(errs, fmt.Errorf("protective stop %v must be below the fill price %v for a long position", p.ProtectiveStop, p.FillPrice))
	}

	if fillPriceFinite && stopMultipleFinite && campaignNFinite && stopFinite {
		if derived := p.FillPrice - p.StopMultiple*p.CampaignN; p.ProtectiveStop != derived {
			errs = append(errs, fmt.Errorf(
				"stated protective stop %v does not match the derivation %v (fill price %v - stop multiple %v x campaign n %v)",
				p.ProtectiveStop, derived, p.FillPrice, p.StopMultiple, p.CampaignN))
		}
	}

	if p.Units != p.UnitIndex {
		errs = append(errs, fmt.Errorf("units %d does not match unit index %d: units are added strictly in order, so the count after this add must equal the index of the unit just added", p.Units, p.UnitIndex))
	}
	if p.AddedAt.IsZero() {
		errs = append(errs, errors.New("added at is required"))
	}
	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid campaign unit added payload: %w", err)
	}
	return nil
}
