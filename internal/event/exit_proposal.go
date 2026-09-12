package event

import (
	"errors"
	"fmt"
	"time"
)

// ExitProposalEventType identifies the exit-proposal decision payload for the
// Envelope's Type field: an open Campaign's Exit Channel breached on this
// completed bar (CONTEXT.md: "Exit Channel"), and the reducer is proposing to
// close the Campaign at the channel level.
//
// Named "strategy.*", not "execution.*": like a trade proposal
// (TradeProposalEventType), this is a decision the strategy made, not an
// external fact — nothing here assumes a fill. ADR 0005 makes the exit a
// resting order at the level, filled in the bar whose range first covers it
// (never at the close); the fill simulator decides whether and at what price
// it actually filled.
//
// A trade proposal and an exit proposal are deliberately two different event
// types, not one payload discriminated by a field: an entry proposal is sized
// from a Signal (N, Unit quantity, Stop Multiple, Notional Account — see
// TradeProposalPayload) and opens a brand new Campaign; an exit proposal
// carries none of that sizing machinery and instead names the Campaign it
// would close and the quantity already held. The two payloads share almost
// no fields, so folding them into one type would mean most fields are
// meaningless on one branch or the other — exactly the shape
// docs/development.md's "closed set, not free text" principle exists to
// avoid.
const ExitProposalEventType = "strategy.exit.proposed"

// ExitProposalSchemaVersion is the current schema version of
// ExitProposalPayload, for the Envelope's SchemaVersion field.
const ExitProposalSchemaVersion uint32 = 1

// RuleExitChannelBreach names the rule for ExitProposalPayload.Rule: the
// bar's low fell below the lowest low of the preceding ExitChannelLength
// completed bars (The Turtle Rules p.26; ADR 0002). Named by mechanism, not
// by Faith's system label, for the same reason RuleEntryChannelBreakout is:
// System 1 is a 10-day exit and System 2 a 20-day one (ADR 0002), so a name
// tied to "system2" would misdescribe a Variant configured with a different
// ExitChannelLength.
const RuleExitChannelBreach = "exit.channel.breach"

// ADRExitChannelBreach is the ADR ExitProposalPayload.ADR cites: ADR 0002,
// which selects the 20-bar Exit Channel for the Baseline (The Turtle Rules
// p.26: System 2 exits on a 20-day low/high, all Units). This field names
// the rule's defining ADR, the same reasoning ADREntryChannelBreakout
// documents for its own entry-side counterpart.
const ADRExitChannelBreach = "0002"

// ExitReasonExitChannel is CampaignExitedPayload.Reason's value for a
// Campaign closed by this proposal's fill. Declared here, next to the
// proposal that names it, rather than only on CampaignExitedPayload's own
// enumeration, since the two must always agree: an exit fill executing this
// proposal cites the identical string.
const ExitReasonExitChannel = "exit-channel"

// ExitProposalPayload records the reducer proposing to close an open
// Campaign at its Exit Channel level: the whole of a System 2 exit (The
// Turtle Rules p.26), before any fill exists.
//
// Level is the Exit Channel low the bar's low fell below — computed from the
// preceding ExitChannelLength completed bars only, excluding this one (see
// indicator.ExitChannel's doc comment for why that exclusion matters: the
// same look-ahead bug EntryChannel's doc comment names, mirrored). What
// actually fills there is not decided here: ADR 0005 makes it a resting
// order, and the fill simulator decides the executed price, which may sit
// below Level on a gap.
//
// Quantity is always the Campaign's full FilledQuantity: the Baseline has no
// partial exit (CONTEXT.md: "every Unit exits together"), so there is
// nothing here for a later ticket to add a partial-quantity concept to.
//
// Deliberately out of scope, so nothing here should be read as having
// considered them:
//
//   - Whether the fill actually happens, and at what price. ADR 0005 and the
//     fill model own that; a proposal is not a fill.
//   - Adds. This proposal exists only because a Campaign is already
//     open; it says nothing about whether the same bar would also have
//     produced an Add. ADR 0010's ordering — exits evaluated and journaled
//     before Adds — is enforced by where internal/strategy.Reducer emits
//     this event relative to where an Add would be evaluated, not by
//     anything on this payload.
type ExitProposalPayload struct {
	// CampaignID identifies the Campaign this proposal would close.
	CampaignID   string    `json:"campaign_id"`
	InstrumentID string    `json:"instrument_id"`
	PeriodEnd    time.Time `json:"period_end"`
	// Reason is ExitReasonExitChannel today; a Delisting Exit is a
	// different, non-proposed closing event (a Campaign is forced closed,
	// never proposed first), so this field is not expected to grow the way
	// CampaignExitedPayload.Reason does.
	Reason string `json:"reason"`
	// Level is the Exit Channel low that was breached — the price this
	// proposal is raised at (see the type's doc comment).
	Level float64 `json:"level"`
	// Quantity is the Campaign's full filled quantity: every Unit exits
	// together, never a partial amount (CONTEXT.md: "Campaign").
	Quantity int64 `json:"quantity"`
	// Rule and ADR name the rule that produced this decision
	// (docs/development.md principle 3).
	Rule string `json:"rule"`
	ADR  string `json:"adr"`
}

// Validate checks that the payload identifies the Campaign, the instrument
// and the period, that Reason is the one recognised value, that Level is
// finite and positive, that Quantity is a positive whole number, and that
// Rule and ADR are present.
func (p ExitProposalPayload) Validate() error {
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
	switch p.Reason {
	case ExitReasonExitChannel:
		// recognised
	default:
		errs = append(errs, fmt.Errorf("reason %q is not a recognised exit proposal reason", p.Reason))
	}

	switch {
	case !isFinite(p.Level):
		errs = append(errs, errors.New("level must be finite"))
	case p.Level <= 0:
		errs = append(errs, errors.New("level must be positive"))
	}

	if p.Quantity <= 0 {
		errs = append(errs, fmt.Errorf("quantity must be a positive whole number, got %d", p.Quantity))
	}
	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid exit proposal payload: %w", err)
	}
	return nil
}
