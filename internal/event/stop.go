package event

import (
	"errors"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// ProtectiveStopSetEventType identifies the Protective-Stop-set decision
// payload for the Envelope's Type field: a Campaign's Protective Stop
// (CONTEXT.md: "Protective Stop" — "Every open Campaign has one at all
// times") came into force at this level.
//
// Named "strategy.*", not "execution.*": like Campaign-opened, this is a
// *decision* the strategy records about what a fill means for its own
// position state, even though the fill is the external fact that caused it.
//
// Named "...set", not "...moved": a Campaign's first stop for a Unit is SET,
// once, when the Unit's fill is accepted; the Stop Ladder later RAISES an
// earlier Unit's stop as further Units are added, reusing this SAME event
// type and payload (see PreviousLevel below) rather than minting a second
// one — a consumer that groups a journal by event type then sees every stop
// movement of a Campaign's life in one place, first set and every later
// raise alike.
const ProtectiveStopSetEventType = "strategy.protective-stop.set"

// ProtectiveStopSetSchemaVersion is the current schema version of
// ProtectiveStopSetPayload, for the Envelope's SchemaVersion field.
//
// Version 2 added UnitIndex and Reason, both required. A version-1 record
// decodes UnitIndex as the int zero (not a legitimate Unit index — Validate
// requires it positive) and Reason as the empty string (not one of the
// enumerated ProtectiveStopReason* values), so a version-1 record is
// rejected outright rather than silently read as Unit 0 or an unrecognised
// reason (ADR 0015's rule, the same discipline CampaignExitedSchemaVersion's
// own bump applies to a new field with an ambiguous zero value).
const ProtectiveStopSetSchemaVersion uint32 = 2

// RuleProtectiveStopSetFromFill names the rule for
// ProtectiveStopSetPayload.Rule when Reason is ProtectiveStopReasonInitial:
// a Campaign's first Protective Stop for a Unit is set from its actual fill
// (the opening fill for Unit 1, an Add fill for a later Unit), not the
// intended entry level (ADR 0013 measures every ladder from the slipped
// fill).
const RuleProtectiveStopSetFromFill = "protective-stop.set.from-fill"

// RuleStopLadderRaisedByHalfN names the rule for ProtectiveStopSetPayload.Rule
// when Reason is ProtectiveStopReasonAddLadder: an EARLIER Unit's Protective
// Stop rose by half the campaign N because a further Unit was added (the
// Stop Ladder). The Turtle Rules p.22: "if additional units were added, the
// stops for earlier units were raised by 1/2 N."
const RuleStopLadderRaisedByHalfN = "stop-ladder.raised-by-half-n"

// The two reasons ProtectiveStopSetPayload.Reason carries today — a closed
// set, not free text, for the same "groupable journal" reasoning
// ProposalDeclinedPayload.Reason and ExitReasonStop/ExitReasonExitChannel
// already follow.
const (
	// ProtectiveStopReasonInitial means this is a Unit's OWN first stop,
	// set the moment its fill was accepted — Unit 1's at Campaign-open, or
	// a later Unit's own at the Add that brought it into being. PreviousLevel
	// is always 0 for this reason.
	ProtectiveStopReasonInitial = "initial"
	// ProtectiveStopReasonAddLadder means this is an EARLIER Unit's stop,
	// raised by half the campaign N because a further Unit was just added
	// (the Stop Ladder, The Turtle Rules p.22-23). PreviousLevel is always
	// positive for this reason: there is always a prior level to have
	// raised from.
	ProtectiveStopReasonAddLadder = "add-ladder"
)

// ProtectiveStopSetPayload records a Campaign's Protective Stop coming into
// force at Level.
//
// **Which ADR this cites, and why 0006 rather than 0003.** ADR 0003 is why
// the Stop Multiple is a named, configured parameter (Baseline 2) rather
// than a hard-coded number; ADR 0006 is why the distance is measured from
// the Campaign's FROZEN campaign N rather than one recomputed at the moment
// the stop is set. What this event exists to record is the second fact — a
// stop level tied to a number that will not drift for the life of the
// Campaign — so its ADR field cites ADR 0006, the same ADR
// CampaignOpenedPayload cites for freezing CampaignN and UnitQuantity in the
// first place. The Stop Multiple itself is carried on the payload (so the
// level is independently re-derivable) but does not need its own citation:
// ADR 0003 is not what this event is claiming.
//
// Level is measured from the Campaign's actual EntryPrice (the fill), never
// the proposal's intended level — see CampaignOpenedPayload's doc comment
// for why (ADR 0013).
//
// PreviousLevel is 0 on a Unit's first Protective-Stop-set (Reason
// ProtectiveStopReasonInitial: there is no previous level to report) and
// positive on a Stop Ladder raise (Reason ProtectiveStopReasonAddLadder). It
// was present on this payload before the Stop Ladder needed it, so the Stop
// Ladder could reuse this event without a schema migration for the field
// itself (only UnitIndex and Reason needed adding, see
// ProtectiveStopSetSchemaVersion's doc comment).
//
// # One event type, a Reason discriminator, not a second event type
//
// This event is named "...set", not "...moved", so the Stop Ladder's later
// raises could reuse it (see ProtectiveStopSetEventType's own doc comment):
// "a consumer that groups a journal by event type then sees every stop
// movement of a Campaign's life in one place, first set and every later
// raise alike." Reason is that reuse mechanism — the identical choice
// CampaignExitedPayload already made for its own Reason field (ExitReasonStop
// / ExitReasonExitChannel) rather than minting strategy.campaign.exited-by-stop
// and strategy.campaign.exited-by-exit-channel as two types. A second event
// type here would force every consumer that wants "every stop movement,
// first set and raise alike" to subscribe to two types and merge them.
type ProtectiveStopSetPayload struct {
	CampaignID   string `json:"campaign_id"`
	InstrumentID string `json:"instrument_id"`
	// UnitIndex is which Unit this stop belongs to: 1 through the Campaign's
	// configured maximum. Needed because the Stop Ladder moves an INDIVIDUAL
	// Unit's own stop rather than a single Campaign-level figure — the gap
	// case (The Turtle Rules p.23) leaves Units at genuinely different
	// levels, so an event that did not say which Unit it was about would be
	// unreadable the moment levels diverge.
	UnitIndex int `json:"unit_index"`
	// Reason is one of the two enumerated ProtectiveStopReason* constants
	// (see the type's doc comment, "One event type, a Reason discriminator").
	Reason string `json:"reason"`
	// AsOf is when this stop came into force: the fill's timestamp on an
	// initial set (matching CampaignOpenedPayload.OpenedAt for Unit 1, or
	// CampaignUnitAddedPayload.AddedAt for a later Unit's own), and the
	// TRIGGERING Add fill's timestamp on a Stop Ladder raise — the raise of
	// an EARLIER Unit's stop happens at the moment a LATER Unit's
	// fill is accepted, not at any timestamp of the earlier Unit's own.
	// Never a bar's PeriodEnd: like the Campaign itself, a stop movement is
	// caused by a fill, not by a bar closing.
	AsOf time.Time `json:"as_of"`
	// Level is where this Unit's Protective Stop now sits. Validate
	// re-derives it exactly — as EntryPrice - StopMultiple x CampaignN when
	// Reason is ProtectiveStopReasonInitial, or as PreviousLevel + 0.5 x
	// CampaignN (sizing.RaisedStop) when Reason is
	// ProtectiveStopReasonAddLadder.
	Level float64 `json:"level"`
	// PreviousLevel is the stop level this one replaces (see the type's doc
	// comment).
	PreviousLevel float64 `json:"previous_level"`
	// EntryPrice, CampaignN and StopMultiple are restated from the Campaign
	// (CampaignOpenedPayload carries the same three numbers) so an INITIAL
	// Level is independently re-derivable from this payload alone, without
	// joining back to the Campaign-opened event. EntryPrice is always THIS
	// Unit's own fill price (unchanged by a later raise): on an add-ladder
	// raise it is restated for identification and audit only — Level there
	// is derived from PreviousLevel, not from EntryPrice (see the type's own
	// doc comment on Level).
	EntryPrice   float64 `json:"entry_price"`
	CampaignN    float64 `json:"campaign_n"`
	StopMultiple float64 `json:"stop_multiple"`
	// Rule and ADR name the rule that produced this decision
	// (docs/development.md principle 3).
	Rule string `json:"rule"`
	ADR  string `json:"adr"`
}

// Validate checks that the payload identifies the Campaign, the Unit and the
// moment the stop came into force, that Reason is one of the enumerated
// constants, that every frozen number is usable, that Level is positive (a
// long position cannot be stopped out at or below zero) and — for Reason
// ProtectiveStopReasonInitial only — strictly below EntryPrice; a RAISED
// stop (ProtectiveStopReasonAddLadder) may sit at or above EntryPrice, a
// legitimate break-even or profit-protecting level under a narrow enough
// Stop Multiple (see sizing.AggregateOpenRisk's own doc comment for why the
// Baseline never reaches this). Validate also checks
// that PreviousLevel is legitimate for the stated Reason (zero
// for an initial set, positive and strictly below Level for an add-ladder
// raise — the Baseline's Stop Ladder only ever raises a stop), and that
// Level matches its derivation for the stated Reason EXACTLY — the same
// exact-equality discipline every derived level in this package uses, for
// the same reason: a tolerance would let a differently-derived stop
// through. The add-ladder derivation calls sizing.RaisedStop rather than
// re-typing "PreviousLevel + 0.5 x CampaignN" here: one shared function,
// called by both the producer and the validator, so the two cannot silently
// disagree about the Stop Ladder's own arithmetic.
func (p ProtectiveStopSetPayload) Validate() error {
	var errs []error
	if p.CampaignID == "" {
		errs = append(errs, errors.New("campaign id is required"))
	}
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if p.UnitIndex < 1 {
		errs = append(errs, fmt.Errorf("unit index must be at least 1, got %d", p.UnitIndex))
	}
	switch p.Reason {
	case ProtectiveStopReasonInitial, ProtectiveStopReasonAddLadder:
		// recognised
	default:
		errs = append(errs, fmt.Errorf("reason %q is not a recognised protective stop reason", p.Reason))
	}
	if p.AsOf.IsZero() {
		errs = append(errs, errors.New("as of is required"))
	}
	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}

	campaignNFinite := isFinite(p.CampaignN)
	switch {
	case !campaignNFinite:
		errs = append(errs, errors.New("campaign n must be finite"))
	case p.CampaignN <= 0:
		errs = append(errs, errors.New("campaign n must be positive: the stop is measured from it (ADR 0006)"))
	}

	stopMultipleFinite := isFinite(p.StopMultiple)
	switch {
	case !stopMultipleFinite:
		errs = append(errs, errors.New("stop multiple must be finite"))
	case p.StopMultiple <= 0:
		errs = append(errs, errors.New("stop multiple must be positive"))
	}

	entryPriceFinite := isFinite(p.EntryPrice)
	switch {
	case !entryPriceFinite:
		errs = append(errs, errors.New("entry price must be finite"))
	case p.EntryPrice <= 0:
		errs = append(errs, errors.New("entry price must be positive"))
	}

	// Deliberately NOT checked here: "Level below EntryPrice" — that shape
	// is required only for an INITIAL stop (see the Reason switch below). A
	// stop RAISED by the Stop Ladder (Reason ProtectiveStopReasonAddLadder)
	// may sit at or above its own Unit's entry once enough half-N raises
	// have accumulated under a narrow enough Stop Multiple — a legitimate
	// break-even or profit-protecting level (CONTEXT.md: "risk-free"), never
	// a corrupted one, since a stop only ever reaches entry by rising from
	// below it.
	levelFinite := isFinite(p.Level)
	switch {
	case !levelFinite:
		errs = append(errs, errors.New("level must be finite"))
	case p.Level <= 0:
		errs = append(errs, errors.New("level must be positive: a long position cannot be stopped out at or below zero"))
	}

	previousLevelFinite := isFinite(p.PreviousLevel)
	switch {
	case !previousLevelFinite:
		errs = append(errs, errors.New("previous level must be finite"))
	case p.PreviousLevel < 0:
		errs = append(errs, errors.New("previous level must not be negative: zero means this unit's first protective stop"))
	case p.PreviousLevel > 0 && levelFinite && p.PreviousLevel >= p.Level:
		errs = append(errs, fmt.Errorf(
			"previous level %v must be below the new level %v: the baseline's stop ladder only ever raises a unit's stop",
			p.PreviousLevel, p.Level))
	}

	switch p.Reason {
	case ProtectiveStopReasonInitial:
		if previousLevelFinite && p.PreviousLevel != 0 {
			errs = append(errs, fmt.Errorf("previous level must be zero for an initial set, got %v: there is no prior level to have raised from", p.PreviousLevel))
		}
		// Only an INITIAL stop is required strictly below entry — see this
		// function's own doc comment and the levelFinite switch above for
		// why a RAISED stop is not held to the same shape.
		if entryPriceFinite && levelFinite && p.Level >= p.EntryPrice {
			errs = append(errs, fmt.Errorf("level %v must be below the entry price %v for a long position's initial stop", p.Level, p.EntryPrice))
		}
		if entryPriceFinite && stopMultipleFinite && campaignNFinite && levelFinite {
			if derived := p.EntryPrice - float64(p.StopMultiple*p.CampaignN); p.Level != derived {
				errs = append(errs, fmt.Errorf(
					"stated level %v does not match the derivation %v (entry price %v - stop multiple %v x campaign n %v)",
					p.Level, derived, p.EntryPrice, p.StopMultiple, p.CampaignN))
			}
		}
	case ProtectiveStopReasonAddLadder:
		if previousLevelFinite && p.PreviousLevel <= 0 {
			errs = append(errs, fmt.Errorf("previous level must be positive for an add-ladder raise, got %v: there is always a prior level to have raised from", p.PreviousLevel))
		}
		if previousLevelFinite && p.PreviousLevel > 0 && campaignNFinite && levelFinite {
			if derived, err := sizing.RaisedStop(p.PreviousLevel, p.CampaignN); err == nil && p.Level != derived {
				errs = append(errs, fmt.Errorf(
					"stated level %v does not match the derivation %v (previous level %v + 0.5 x campaign n %v): the stop ladder raises an earlier unit's stop by half n (The Turtle Rules p.22)",
					p.Level, derived, p.PreviousLevel, p.CampaignN))
			}
		}
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid protective stop set payload: %w", err)
	}
	return nil
}
