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

// CampaignExitedEventType identifies the Campaign-exited decision payload
// for the Envelope's Type field: a Campaign's life ended (CONTEXT.md:
// "Campaign" — "from the first Unit's entry to the exit of the last").
//
// Like Campaign-opened, this is a *decision* event even though a fill caused
// it: the fill is the external fact, and this is the strategy stating what
// that fact means for its own position state.
const CampaignExitedEventType = "strategy.campaign.exited"

// CampaignExitedSchemaVersion is the current schema version of
// CampaignExitedPayload, for the Envelope's SchemaVersion field.
//
// Bumped 1 -> 2 for #14: Units was added. A schema-1 record decodes Units as
// the int zero, which is not a legitimate Unit count (Validate requires it
// positive), so a schema-1 record is rejected outright rather than silently
// read as a zero-Unit exit — ADR 0015's rule, the same discipline #12 and
// #13 each applied to their own additive fields.
const CampaignExitedSchemaVersion uint32 = 2

// RuleCampaignExitedByStop names the rule for CampaignExitedPayload.Rule
// when Reason is ExitReasonStop: a Campaign closes because a fill said its
// Protective Stop was hit.
const RuleCampaignExitedByStop = "campaign.exited.by-stop"

// ADRCampaignExitRecordsTheFill is the ADR CampaignExitedPayload.ADR cites:
// ADR 0005, whose resting-order fill model is why ExitPrice is the fill's
// own price and may sit below ProtectiveStopLevel on a gap — "record what
// was filled, never the level" (this payload's own doc comment) is a direct
// restatement of ADR 0005's rule 1, "gaps fill at the open".
const ADRCampaignExitRecordsTheFill = "0005"

// The enumerated reasons a Campaign can exit. A closed set, not free text,
// for the same reason ProposalDeclinedPayload.Reason and
// ProposalExpiredPayload.Reason are: a journal must be groupable by it.
const (
	// ExitReasonStop means a fill said the Protective Stop was hit (#12).
	ExitReasonStop = "stop"
	// ExitReasonExitChannel is declared in exit_proposal.go, next to
	// ExitProposalPayload, which names the identical string: a fill said
	// price fell below the Exit Channel low (#13, The Turtle Rules p.26,
	// ADR 0002). #24 will add a delisting reason; adding it is a new
	// enumerated value on this already-existing payload and event type, not
	// a new one, since every exit is the same underlying fact — a
	// Campaign's life ended, and why.
)

// RuleCampaignExitedByExitChannel names the rule for
// CampaignExitedPayload.Rule when Reason is ExitReasonExitChannel: a
// Campaign closes because a fill said price fell below the Exit Channel low
// (#13).
const RuleCampaignExitedByExitChannel = "campaign.exited.by-exit-channel"

// CampaignExitedPayload records a Campaign's life ending: what was filled to
// close it, and the realised result.
//
// CampaignN, DollarsPerPoint and FillID are not named in the ticket's field
// list but are added here deliberately, for the same reason
// CampaignOpenedPayload carries CampaignN and StopMultiple rather than
// leaving a reader to join back to the configuration: **Validate re-derives
// RealisedResult and RealisedResultInN exactly, and a payload cannot
// re-derive a value from a field it does not have.** RealisedResultInN's
// formula divides by CampaignN; RealisedResult's multiplies by
// DollarsPerPoint; neither number lives anywhere else on this payload.
// FillID is added to close the same audit-chain gap CampaignOpenedPayload's
// own FillID closes for the opening fill — without it, a reviewer cannot
// join the exit decision back to the execution record that caused it, or
// recognise a re-delivered closing fill as the duplicate that produced this
// exact exit.
//
// ExitPrice is the actual fill price, which under ADR 0005's gap rule may
// sit strictly BELOW ProtectiveStopLevel: a stop fills at min(level, open)
// on a gap, so what closed the Campaign can be worse than the level that
// triggered it. This payload records what was filled, never the level —
// ProtectiveStopLevel is carried alongside it only as "the level that was in
// force", not as a substitute for ExitPrice.
//
// This is long-only, matching every other payload in this package today
// (DirectionLong is the only recognised Direction anywhere in this system):
// RealisedResult's sign falls out of ExitPrice - EntryPrice directly, with
// no direction-dependent flip, because a long Campaign's gain is exactly
// that difference. A short Campaign would need the opposite sign, which this
// payload does not implement.
//
// # Multi-Unit aggregation (#14)
//
// A Campaign that added Units closes ALL of them in one decision (this
// ticket keeps stop and exit fills whole-Campaign; #15 makes a stop
// per-Unit). Quantity is therefore the SUM of every held Unit's own
// quantity, and EntryPrice is their QUANTITY-WEIGHTED AVERAGE fill price —
// chosen deliberately over a per-Unit breakdown on this payload (the
// Campaign-opened and unit-added events already carry every individual
// fill; this payload's job is the Campaign-level result, not a restatement
// of the audit trail) and deliberately over reporting each Unit's own
// result separately, because the weighted average keeps RealisedResult's
// derivation IDENTICAL to the single-Unit formula: for quantities q_i and
// fills e_i summing to Quantity Q and weighted average E,
//
//	sum(q_i x (ExitPrice - e_i)) = Quantity x (ExitPrice - E)
//
// so Quantity x (ExitPrice - EntryPrice) x DollarsPerPoint is EXACTLY the
// sum of each Unit's own realised result, whether the Campaign held one Unit
// or four — Validate's derivation check below needs no per-Unit case.
// RealisedResultInN follows the same algebra with CampaignN in place of
// DollarsPerPoint, since campaignN is common to every Unit of one Campaign
// (ADR 0006 freezes it once, at first entry, for the Campaign's whole life).
type CampaignExitedPayload struct {
	CampaignID   string `json:"campaign_id"`
	InstrumentID string `json:"instrument_id"`
	// FillID is the closing fill's producer-assigned id (see the type's doc
	// comment).
	FillID string `json:"fill_id"`
	// ExitedAt is the closing fill's timestamp: the Campaign's life ended
	// when the fill did, matching CampaignOpenedPayload.OpenedAt's own
	// convention.
	ExitedAt time.Time `json:"exited_at"`
	// Reason is one of the enumerated Exit reason constants.
	Reason string `json:"reason"`
	// EntryPrice restates the Campaign's own entry price (CampaignOpenedPayload.EntryPrice),
	// so this payload is readable and re-derivable without joining back to
	// the Campaign-opened event.
	EntryPrice float64 `json:"entry_price"`
	// ExitPrice is the actual fill price that closed the Campaign — never
	// the Protective Stop level (see the type's doc comment).
	ExitPrice float64 `json:"exit_price"`
	// Quantity is the whole position closed: the Campaign's own
	// FilledQuantity. A partial stop close is out of scope for this ticket
	// (see internal/strategy/campaign.go's applyStopFill), so this is always
	// the Campaign's entire holding.
	Quantity int64 `json:"quantity"`
	// CampaignN is the Campaign's frozen campaign N (ADR 0006), restated so
	// RealisedResultInN is independently re-derivable (see the type's doc
	// comment).
	CampaignN float64 `json:"campaign_n"`
	// DollarsPerPoint is the instrument's contract multiplier (1 for
	// shares), restated so RealisedResult is independently re-derivable (see
	// the type's doc comment).
	DollarsPerPoint float64 `json:"dollars_per_point"`
	// ProtectiveStopLevel is the Protective Stop level that was in force
	// when this exit happened — the Campaign's ProtectiveStop as it stood at
	// close, not necessarily what was filled (see the type's doc comment).
	ProtectiveStopLevel float64 `json:"protective_stop_level"`
	// RealisedResult is the signed dollar result of the whole Campaign:
	// Quantity x (ExitPrice - EntryPrice) x DollarsPerPoint. Negative for a
	// loss, as a stop-out ordinarily is.
	RealisedResult float64 `json:"realised_result"`
	// RealisedResultInN is the same result expressed in campaign N:
	// (ExitPrice - EntryPrice) / CampaignN. CONTEXT.md's risk vocabulary is
	// stated in N throughout (Stop Multiple, Risk at Stop), so this is the
	// unit a reviewer compares a result against, e.g. "this Campaign lost
	// close to its full 2N risk".
	RealisedResultInN float64 `json:"realised_result_in_n"`
	// Units is the number of Units this Campaign held at close, 1 through
	// the configured maximum (#14; see the type's doc comment on multi-Unit
	// aggregation). Always 1 before #14's Add Ladder exists.
	Units int `json:"units"`
	// Rule and ADR name the rule that produced this decision
	// (docs/development.md principle 3).
	Rule string `json:"rule"`
	ADR  string `json:"adr"`
}

// Validate checks that the payload identifies the Campaign, the closing fill
// and the decision chain, that Reason is one of the enumerated constants,
// that every frozen number is usable, that ProtectiveStopLevel is positive
// and strictly below EntryPrice (the same long-only shape
// CampaignOpenedPayload.ProtectiveStop enforces), and that RealisedResult
// and RealisedResultInN each match their derivation from the payload's own
// fields EXACTLY — the same exact-equality discipline every derived field in
// this package uses, and for the same reason: a tolerance would let a
// differently-derived result through, which is the defect the check exists
// to catch.
func (p CampaignExitedPayload) Validate() error {
	var errs []error
	if p.CampaignID == "" {
		errs = append(errs, errors.New("campaign id is required"))
	}
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if p.FillID == "" {
		errs = append(errs, errors.New("fill id is required: an exit must name the fill that closed it"))
	}
	if p.ExitedAt.IsZero() {
		errs = append(errs, errors.New("exited at is required"))
	}
	switch p.Reason {
	case ExitReasonStop, ExitReasonExitChannel:
		// recognised
	default:
		errs = append(errs, fmt.Errorf("reason %q is not a recognised exit reason", p.Reason))
	}
	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}

	entryPriceFinite := isFinite(p.EntryPrice)
	switch {
	case !entryPriceFinite:
		errs = append(errs, errors.New("entry price must be finite"))
	case p.EntryPrice <= 0:
		errs = append(errs, errors.New("entry price must be positive"))
	}

	exitPriceFinite := isFinite(p.ExitPrice)
	switch {
	case !exitPriceFinite:
		errs = append(errs, errors.New("exit price must be finite"))
	case p.ExitPrice <= 0:
		errs = append(errs, errors.New("exit price must be positive"))
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

	dollarsPerPointFinite := isFinite(p.DollarsPerPoint)
	switch {
	case !dollarsPerPointFinite:
		errs = append(errs, errors.New("dollars per point must be finite"))
	case p.DollarsPerPoint <= 0:
		errs = append(errs, errors.New("dollars per point must be positive"))
	}

	stopLevelFinite := isFinite(p.ProtectiveStopLevel)
	switch {
	case !stopLevelFinite:
		errs = append(errs, errors.New("protective stop level must be finite"))
	case p.ProtectiveStopLevel <= 0:
		errs = append(errs, errors.New("protective stop level must be positive: a long position cannot be stopped out at or below zero"))
	case entryPriceFinite && p.ProtectiveStopLevel >= p.EntryPrice:
		errs = append(errs, fmt.Errorf("protective stop level %v must be below the entry price %v for a long position", p.ProtectiveStopLevel, p.EntryPrice))
	}

	realisedResultFinite := isFinite(p.RealisedResult)
	if !realisedResultFinite {
		errs = append(errs, errors.New("realised result must be finite"))
	}
	realisedResultInNFinite := isFinite(p.RealisedResultInN)
	if !realisedResultInNFinite {
		errs = append(errs, errors.New("realised result in n must be finite"))
	}

	if entryPriceFinite && exitPriceFinite && dollarsPerPointFinite && p.Quantity > 0 && realisedResultFinite {
		if derived := float64(p.Quantity) * (p.ExitPrice - p.EntryPrice) * p.DollarsPerPoint; p.RealisedResult != derived {
			errs = append(errs, fmt.Errorf(
				"stated realised result %v does not match the derivation %v (quantity %d x (exit price %v - entry price %v) x dollars per point %v)",
				p.RealisedResult, derived, p.Quantity, p.ExitPrice, p.EntryPrice, p.DollarsPerPoint))
		}
	}
	if entryPriceFinite && exitPriceFinite && campaignNFinite && realisedResultInNFinite {
		if derived := (p.ExitPrice - p.EntryPrice) / p.CampaignN; p.RealisedResultInN != derived {
			errs = append(errs, fmt.Errorf(
				"stated realised result in n %v does not match the derivation %v ((exit price %v - entry price %v) / campaign n %v)",
				p.RealisedResultInN, derived, p.ExitPrice, p.EntryPrice, p.CampaignN))
		}
	}

	if p.Units <= 0 {
		errs = append(errs, fmt.Errorf("units must be a positive whole number, got %d", p.Units))
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid campaign exited payload: %w", err)
	}
	return nil
}
