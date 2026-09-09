package event

import (
	"errors"
	"fmt"
	"time"
)

// CampaignOpenedEventType identifies the Campaign-opened decision payload for
// the Envelope's Type field. CONTEXT.md: "Campaign" — the complete life of a
// position in one instrument, from the first Unit's entry to the exit of the
// last.
//
// This is a *decision* event even though a fill caused it: the fill is the
// external fact, and this is the strategy stating what that fact means for its
// own position state — which N and Unit share count are now frozen, and where
// the Protective Stop sits.
const CampaignOpenedEventType = "strategy.campaign.opened"

// CampaignOpenedSchemaVersion is the current schema version of
// CampaignOpenedPayload, for the Envelope's SchemaVersion field.
const CampaignOpenedSchemaVersion uint32 = 1

// RuleCampaignOpenedFromFill names the rule for CampaignOpenedPayload.Rule.
//
// The name states the mechanism the rule exists to enforce — a Campaign comes
// into being from a recorded fill — rather than the strategy that proposed the
// entry. It is the same rule under the Baseline and under every Variant,
// because it is not a strategy choice: docs/architecture.md makes it an
// architectural invariant.
const RuleCampaignOpenedFromFill = "campaign.opened.from-fill"

// ADRCampaignFrozenAtEntry is the ADR CampaignOpenedPayload.ADR cites: ADR
// 0006, which freezes a Campaign's N and Unit share count at first entry and
// requires them to be journaled because replay depends on them rather than on
// recomputation.
const ADRCampaignFrozenAtEntry = "0006"

// CampaignOpenedPayload records a Campaign coming into being, and the two
// numbers ADR 0006 freezes at that moment.
//
// **Everything the Add Ladder and the Stop Ladder will ever need is derivable
// from CampaignN, UnitQuantity and EntryPrice**, and that is the point of the
// payload. Under the Baseline a Unit is added every half a campaign N above
// the entry and each Protective Stop sits StopMultiple campaign N below its
// Unit's entry (The Turtle Rules p.19-20, p.22), so the whole of both ladders
// is arithmetic over those three numbers plus the frozen StopMultiple. ADR
// 0006: the Campaign's N is not recomputed while it is open, the frozen values
// are part of Campaign state, and they must be journaled — replay depends on
// them, not on recomputation. A consumer that recomputed N from the bars would
// silently produce a different ladder for the same Campaign.
//
// EntryPrice is the price that ACTUALLY filled, including the producer's
// slippage. ADR 0013 measures the Add Ladder from the slipped fill, as Faith
// specifies [T p.19], so a Campaign recording the level the Signal fired at
// would put every later rung of both ladders in the wrong place. The
// corresponding TradeProposalPayload.ProtectiveStopIntent was measured from
// the intended level and is deliberately a different number; ProtectiveStop
// here is the real one.
//
// Deliberately out of scope, so nothing here should be read as having been
// handled:
//
//   - Adds. Units is always 1 on this event — it is the Campaign-*opened*
//     record. Adding a Unit is #13 and emits its own event.
//   - Stop movement. The stop as the Campaign's ladder advances is #12; this
//     payload states where it sits at entry.
//   - Accumulating partial fills. A partial fill opens a Campaign for the
//     filled quantity (FilledQuantity below); combining a later partial into
//     the same Campaign is deferred to its own issue.
//   - Caps and the cash rule (ADR 0008, ADR 0010). Those constrain whether an
//     order is placed; by the time a fill exists the position is a fact.
type CampaignOpenedPayload struct {
	// CampaignID identifies this Campaign for the whole of its life, so every
	// later Add, stop movement and exit can be joined to it. It is
	// deterministic — derived from the instrument and the opening fill's
	// timestamp — so replay reconstructs the same identity without any
	// randomness or wall-clock read.
	CampaignID   string `json:"campaign_id"`
	InstrumentID string `json:"instrument_id"`
	// ProposalID and SignalID are the audit chain back to the decision that
	// produced this position: which proposal was executed, and which Signal
	// that proposal answered.
	ProposalID string `json:"proposal_id"`
	SignalID   string `json:"signal_id"`
	// FillID is the opening fill's producer-assigned id, recorded so the
	// Campaign can be reconciled against the execution record and so a
	// re-delivered fill is recognisable as a duplicate rather than a second
	// entry.
	FillID string `json:"fill_id"`
	// Rule and ADR name the rule that produced this decision
	// (docs/development.md principle 3).
	Rule      string `json:"rule"`
	ADR       string `json:"adr"`
	Direction string `json:"direction"`
	// CampaignN is the frozen campaign N: the volatility reading the proposal
	// was sized from, carried across the fill unchanged and never recomputed
	// while the Campaign is open (ADR 0006).
	CampaignN float64 `json:"campaign_n"`
	// UnitQuantity is the frozen Unit share count — the whole size the
	// proposal asked for, which is the size of every rung of the Add Ladder.
	// A partial fill does NOT reduce it: the Unit is the risk measure the caps
	// are counted in (ADR 0010), and resizing it would change what "one Unit"
	// means partway through a Campaign.
	UnitQuantity int64 `json:"unit_quantity"`
	// FilledQuantity is how much of the Unit actually executed, at most
	// UnitQuantity. It is the position the Campaign really holds.
	FilledQuantity int64 `json:"filled_quantity"`
	// EntryPrice is the actual fill price (see the type's doc comment).
	EntryPrice float64 `json:"entry_price"`
	// StopMultiple is the configured number of N between a Unit's entry and
	// its Protective Stop (CONTEXT.md: "Stop Multiple"; 2 in the Baseline, The
	// Turtle Rules p.22), frozen here alongside N so the Stop Ladder is
	// computable from this record alone.
	StopMultiple float64 `json:"stop_multiple"`
	// ProtectiveStop is where the Campaign's stop sits: EntryPrice less
	// StopMultiple campaign N, measured from the actual fill. Validate
	// re-derives it exactly.
	ProtectiveStop float64 `json:"protective_stop"`
	// Units is the number of Units the Campaign holds, always 1 here. A
	// partial fill does not make it a fraction: a Unit is indivisible as a
	// risk measure (ADR 0010), and FilledQuantity is what records how much of
	// it filled.
	Units int `json:"units"`
	// OpenedAt is the opening fill's timestamp — when the Campaign came into
	// being, which is when the fill did and not when the Signal fired.
	OpenedAt time.Time `json:"opened_at"`
}

// Validate checks that the payload identifies the Campaign and the decision
// chain behind it, that every frozen number is usable, and that the two
// invariants a journal reader would otherwise take on trust hold:
//
//  1. **The position never exceeds the Unit that was sized.** FilledQuantity
//     must be positive and at most UnitQuantity. An over-execution risks more
//     capital than the sizing arithmetic budgeted for, which is the one
//     direction TradeProposalPayload's truncation invariants never permit.
//
//  2. **The Protective Stop matches its derivation and is reachable.** It must
//     be exactly EntryPrice - StopMultiple x CampaignN, strictly below the
//     entry price, and positive — a long position cannot be stopped out at or
//     below zero, so such a Campaign would be unprotected in fact while
//     looking protected in the journal. Exact float64 equality is deliberate,
//     for the reason recorded on TradeProposalPayload.Validate: the stated
//     value must be the identical value the derivation produces, so any
//     tolerance would let a differently-derived stop through, which is the
//     defect the check exists to catch. A producer must therefore compute the
//     level in float64, in this expression order.
func (p CampaignOpenedPayload) Validate() error {
	var errs []error
	if p.CampaignID == "" {
		errs = append(errs, errors.New("campaign id is required"))
	}
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if p.ProposalID == "" {
		errs = append(errs, errors.New("proposal id is required: a campaign must name the proposal that was executed"))
	}
	if p.SignalID == "" {
		errs = append(errs, errors.New("signal id is required: a campaign must name the signal behind it"))
	}
	if p.FillID == "" {
		errs = append(errs, errors.New("fill id is required: a campaign must name the fill that opened it"))
	}
	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}
	switch p.Direction {
	case DirectionLong:
		// recognised
	default:
		errs = append(errs, fmt.Errorf("direction %q is not a recognised direction", p.Direction))
	}

	campaignNFinite := isFinite(p.CampaignN)
	switch {
	case !campaignNFinite:
		errs = append(errs, errors.New("campaign n must be finite"))
	case p.CampaignN <= 0:
		errs = append(errs, errors.New("campaign n must be positive: the add ladder and the stop ladder are computed from it (ADR 0006)"))
	}

	if p.UnitQuantity <= 0 {
		errs = append(errs, fmt.Errorf("unit quantity must be a positive whole number, got %d", p.UnitQuantity))
	}
	switch {
	case p.FilledQuantity <= 0:
		errs = append(errs, fmt.Errorf("filled quantity must be a positive whole number, got %d: a campaign exists only for what actually executed", p.FilledQuantity))
	case p.UnitQuantity > 0 && p.FilledQuantity > p.UnitQuantity:
		errs = append(errs, fmt.Errorf("filled quantity %d exceeds the unit quantity %d: a fill may never execute more than the unit that was sized", p.FilledQuantity, p.UnitQuantity))
	}

	entryPriceFinite := isFinite(p.EntryPrice)
	switch {
	case !entryPriceFinite:
		errs = append(errs, errors.New("entry price must be finite"))
	case p.EntryPrice <= 0:
		errs = append(errs, errors.New("entry price must be positive"))
	}

	stopMultipleFinite := isFinite(p.StopMultiple)
	switch {
	case !stopMultipleFinite:
		errs = append(errs, errors.New("stop multiple must be finite"))
	case p.StopMultiple <= 0:
		errs = append(errs, errors.New("stop multiple must be positive"))
	}

	protectiveStopFinite := isFinite(p.ProtectiveStop)
	switch {
	case !protectiveStopFinite:
		errs = append(errs, errors.New("protective stop must be finite"))
	case p.ProtectiveStop <= 0:
		errs = append(errs, errors.New("protective stop must be positive: a long position cannot be stopped out at or below zero"))
	case entryPriceFinite && p.ProtectiveStop >= p.EntryPrice:
		errs = append(errs, fmt.Errorf("protective stop %v must be below the entry price %v for a long position", p.ProtectiveStop, p.EntryPrice))
	}

	// Invariant 2's derivation half, checked whenever the operands are usable
	// so that a stop which is wrong AND out of range reports both facts.
	if entryPriceFinite && stopMultipleFinite && campaignNFinite && protectiveStopFinite {
		if derived := p.EntryPrice - p.StopMultiple*p.CampaignN; p.ProtectiveStop != derived {
			errs = append(errs, fmt.Errorf(
				"stated protective stop %v does not match the derivation %v (entry price %v - stop multiple %v x campaign n %v): the stop is measured from the actual fill (ADR 0013)",
				p.ProtectiveStop, derived, p.EntryPrice, p.StopMultiple, p.CampaignN))
		}
	}

	if p.Units != 1 {
		errs = append(errs, fmt.Errorf("units must be exactly 1, got %d: this is the campaign-opened record, and an add emits its own event", p.Units))
	}
	if p.OpenedAt.IsZero() {
		errs = append(errs, errors.New("opened at is required"))
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid campaign opened payload: %w", err)
	}
	return nil
}
