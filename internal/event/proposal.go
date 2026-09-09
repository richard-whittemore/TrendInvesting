package event

import (
	"errors"
	"fmt"
	"time"
)

// TradeProposalEventType identifies the trade-proposal decision payload for
// the Envelope's Type field. docs/architecture.md names "trade proposals
// returned to LEAN" as the decision boundary's output: this is the Go side of
// that boundary, and it is a proposal, not an order — nothing here assumes a
// fill (ADR 0005 and #18 own the fill model).
const TradeProposalEventType = "strategy.trade.proposed"

// TradeProposalSchemaVersion is the current schema version of
// TradeProposalPayload, for the Envelope's SchemaVersion field.
const TradeProposalSchemaVersion uint32 = 1

// ProposalDeclinedEventType identifies the payload recorded when a Signal
// fired but produced no position. It exists so that "the strategy recognised
// its entry condition and deliberately took nothing" is a journalled fact
// rather than an absence a reader has to infer.
const ProposalDeclinedEventType = "strategy.proposal.declined"

// ProposalDeclinedSchemaVersion is the current schema version of
// ProposalDeclinedPayload.
const ProposalDeclinedSchemaVersion uint32 = 1

// The rule names for TradeProposalPayload.Rule, one per Sizing Mode.
//
// Each names what the rule computes, not which vendor's system it resembles
// and not the parameter values it happened to run with — #9's finding
// applied to sizing. A Variant that changes the Unit Volatility Fraction or
// the Stop Multiple is still volatility-normalised sizing, and a rule name
// carrying either number would misdescribe it; the numbers live in their own
// fields on the payload. Which run this is — Baseline or a declared Variant —
// is established by the envelope's ConfigurationHash and StrategyVersion (ADR
// 0012), never by these names.
const (
	RuleUnitSizingVolatilityNormalised = "unit.sizing.volatility-normalised"
	RuleUnitSizingFixedRiskAtStop      = "unit.sizing.fixed-risk-at-stop"
)

// ADRUnitSizing is the ADR TradeProposalPayload.ADR cites: ADR 0003, the
// decision that makes sizing volatility-normalised, makes the Sizing Mode
// explicit, and makes Risk at Stop derived rather than configured. It governs
// both modes — the ADR is what declares that there are two and that the
// choice between them is a declared experiment — so it is the defining ADR of
// a fixed-risk-at-stop proposal just as much as of a Baseline one.
const ADRUnitSizing = "0003"

// The enumerated reasons a proposal can be declined. A closed set, not free
// text: a journal that can be asked "how often did a Signal produce nothing
// because the account was too small" needs values it can group by, and
// ProposalDeclinedPayload.Detail carries the specifics of the individual case.
const (
	// DeclineReasonNNotReady means N was not a usable volatility reading
	// when the Signal fired. A Tier A Setup already requires a ready N
	// (SetupEvaluatedPayload), so this should be unreachable from the
	// reducer; it exists so that a producer which ever reaches it declines
	// visibly instead of sizing from a zero.
	DeclineReasonNNotReady = "n-not-ready"
	// DeclineReasonQuantityBelowOneUnit means the sizing arithmetic
	// truncated to zero: the Notional Account cannot fund a single share or
	// contract at this N. The Turtle Rules p.15 names this directly — small
	// accounts lose diversification because truncation is coarse.
	DeclineReasonQuantityBelowOneUnit = "quantity-below-one-unit"
	// DeclineReasonStopIntentNotPositive means the Protective Stop intent
	// (entry less Stop Multiple x N) is at or below zero. A long equity
	// cannot trade below zero, so such a stop is unreachable and the Unit
	// would in fact risk the whole position rather than the derived
	// fraction.
	DeclineReasonStopIntentNotPositive = "stop-intent-not-positive"
)

// TradeProposalPayload is the first event in this system that states a number
// of shares. It carries what a reviewer needs to see that the number is
// justified: the level the entry is proposed at, the quantity, where the
// Protective Stop would sit, what that would risk, and every parameter that
// produced those figures.
//
// Risk at Stop is stated as a *derived* number (ADR 0003), so a reader sees
// what a Unit actually risks rather than what a parameter claims. Validate
// re-derives it and refuses a payload whose stated value the payload's own
// fields do not support — an assertion that costs nothing and catches the one
// class of defect that would otherwise be invisible in a journal.
//
// Deliberately out of scope, so nothing here should be read as having
// considered them:
//
//   - Caps. This is one Unit, and no cap is checked (#55 records that
//     ConfigurationPayload.MaxUnits models one of ADR 0008's four levels;
//     the cap check itself belongs to a later ticket). A proposal is not a
//     permission to trade.
//   - Fills. EntryLevel is the level the Signal fired at, not a fill price;
//     slippage is a fill concern (ADR 0013) owned by #18.
//   - Drawdown. NotionalAccount is the configured starting equity; Drawdown
//     Steps and yearly re-basing (ADR 0007) are #16/#17.
//   - Campaign state. Freezing N and the Unit size at first entry (ADR 0006)
//     is #11; this payload carries what that freeze will need.
type TradeProposalPayload struct {
	InstrumentID string    `json:"instrument_id"`
	PeriodEnd    time.Time `json:"period_end"`
	// SignalID is the ID of the Signal envelope this proposal answers. It is
	// the audit join between the decision and the position it would create;
	// the envelope's own CausationID names the bar, not the Signal.
	SignalID string `json:"signal_id"`
	// Rule and ADR name the strategy rule that produced this proposal
	// (docs/development.md principle 3).
	Rule      string `json:"rule"`
	ADR       string `json:"adr"`
	Direction string `json:"direction"`
	// EntryLevel is the level the Signal fired at — the breakout high. What
	// actually fills there is not decided here.
	EntryLevel float64 `json:"entry_level"`
	// Quantity is a whole number of shares or contracts, truncated toward
	// zero (The Turtle Rules p.14-15). It is always positive: a proposal for
	// nothing is a decline (ProposalDeclinedPayload), not a proposal for
	// zero.
	Quantity int64 `json:"quantity"`
	// N is the volatility reading the Unit was sized from (CONTEXT.md: "N").
	N float64 `json:"n"`
	// SizingMode records which principle sized this Unit, so replay can show
	// it without re-deriving it from the configuration (ADR 0003).
	SizingMode SizingMode `json:"sizing_mode"`
	// UnitVolatilityFraction and StopMultiple are the configuration in force.
	// Under SizingModeFixedRiskAtStop the Unit Volatility Fraction did not
	// size this Unit and does not determine its Risk at Stop; it is recorded
	// because it was the configuration in effect, and SizingMode is what says
	// which of the two applied.
	UnitVolatilityFraction float64 `json:"unit_volatility_fraction"`
	StopMultiple           float64 `json:"stop_multiple"`
	// RiskAtStop is the fraction of the Notional Account this Unit loses if
	// its Protective Stop is hit — derived under volatility-normalised
	// sizing, the configured input under fixed-risk-at-stop (CONTEXT.md:
	// "Risk at Stop"; ADR 0003).
	RiskAtStop float64 `json:"risk_at_stop"`
	// DollarsPerPoint is the instrument's contract multiplier: 1 for shares,
	// 42,000 for Faith's Heating Oil contract (The Turtle Rules p.15).
	DollarsPerPoint float64 `json:"dollars_per_point"`
	// NotionalAccount is the equity figure this Unit was sized against
	// (CONTEXT.md: "Notional Account"), recorded on the proposal so the size
	// can be re-derived from the payload alone.
	NotionalAccount float64 `json:"notional_account"`
	// ProtectiveStopIntent is where the Protective Stop would sit: the entry
	// level less StopMultiple x N (CONTEXT.md: "Protective Stop"; The Turtle
	// Rules p.22's 2N stop in the Baseline). It is an intent, not an order:
	// no stop exists until a fill does.
	ProtectiveStopIntent float64 `json:"protective_stop_intent"`
}

// Validate checks that a proposal is internally consistent, not merely
// well-formed. Beyond the required fields and ranges it enforces three
// invariants that a journal reader would otherwise have to take on trust:
//
//  1. **Risk at Stop matches its derivation.** Under
//     SizingModeVolatilityNormalised it must be exactly
//     UnitVolatilityFraction x StopMultiple (ADR 0003: derived, never
//     configured). Exact float64 equality is deliberate and is not a price
//     comparison: the stated value must be the identical value the
//     derivation produces, so any tolerance would let a differently-derived
//     number through — which is the defect the check exists to catch. Under
//     SizingModeFixedRiskAtStop, Risk at Stop is the sizing input and is
//     deliberately unrelated to the Unit Volatility Fraction, so only the
//     range check applies.
//
//  2. **The Protective Stop intent matches its derivation.** It must be
//     exactly EntryLevel - StopMultiple x N, must be strictly below the
//     entry level, and must be positive: a long equity cannot trade below
//     zero, so a stop at or under zero is unreachable and the Unit would
//     risk the whole position rather than the stated fraction. Exact again,
//     with the same reasoning as invariant 1 — which means a producer must
//     compute the level in this expression order, in float64: Go evaluates
//     untyped *constant* arithmetic in arbitrary precision and rounds once
//     at the end, so a level folded from constants can differ in the last
//     bit from the same expression evaluated at run time.
//
//  3. **Truncation never risks more than the budget.** Under
//     volatility-normalised sizing, Quantity x N x DollarsPerPoint must not
//     exceed NotionalAccount x UnitVolatilityFraction — the 1N budget The
//     Turtle Rules p.14 actually states, and the exact expression
//     sizing.UnitQuantity guarantees, so this is exact rather than
//     approximate. (The at-the-stop form follows from it, since Risk at Stop
//     is that fraction times the Stop Multiple.) Under fixed-risk-at-stop
//     the budget is stated at the stop instead: Quantity x StopMultiple x N
//     x DollarsPerPoint must not exceed NotionalAccount x RiskAtStop.
func (p TradeProposalPayload) Validate() error {
	var errs []error
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if p.PeriodEnd.IsZero() {
		errs = append(errs, errors.New("period end is required"))
	}
	if p.SignalID == "" {
		errs = append(errs, errors.New("signal id is required: a proposal must name the signal it answers"))
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

	modeRecognised := false
	switch p.SizingMode {
	case SizingModeVolatilityNormalised, SizingModeFixedRiskAtStop:
		modeRecognised = true
	default:
		errs = append(errs, fmt.Errorf("sizing mode %q is not a recognised sizing mode", p.SizingMode))
	}

	if p.Quantity <= 0 {
		errs = append(errs, fmt.Errorf("quantity must be a positive whole number, got %d: a proposal for nothing is a decline, not a proposal", p.Quantity))
	}

	entryLevelFinite := isFinite(p.EntryLevel)
	switch {
	case !entryLevelFinite:
		errs = append(errs, errors.New("entry level must be finite"))
	case p.EntryLevel <= 0:
		errs = append(errs, errors.New("entry level must be positive"))
	}

	nFinite := isFinite(p.N)
	switch {
	case !nFinite:
		errs = append(errs, errors.New("n must be finite"))
	case p.N <= 0:
		errs = append(errs, errors.New("n must be positive"))
	}

	fractionFinite := isFinite(p.UnitVolatilityFraction)
	switch {
	case !fractionFinite:
		errs = append(errs, errors.New("unit volatility fraction must be finite"))
	case p.UnitVolatilityFraction <= 0 || p.UnitVolatilityFraction > 1:
		errs = append(errs, errors.New("unit volatility fraction must be greater than zero and at most one"))
	}

	stopMultipleFinite := isFinite(p.StopMultiple)
	switch {
	case !stopMultipleFinite:
		errs = append(errs, errors.New("stop multiple must be finite"))
	case p.StopMultiple <= 0:
		errs = append(errs, errors.New("stop multiple must be positive"))
	}

	riskAtStopFinite := isFinite(p.RiskAtStop)
	switch {
	case !riskAtStopFinite:
		errs = append(errs, errors.New("risk at stop must be finite"))
	case p.RiskAtStop <= 0 || p.RiskAtStop > 1:
		errs = append(errs, errors.New("risk at stop must be greater than zero and at most one"))
	}

	dollarsPerPointFinite := isFinite(p.DollarsPerPoint)
	switch {
	case !dollarsPerPointFinite:
		errs = append(errs, errors.New("dollars per point must be finite"))
	case p.DollarsPerPoint <= 0:
		errs = append(errs, errors.New("dollars per point must be positive"))
	}

	notionalAccountFinite := isFinite(p.NotionalAccount)
	switch {
	case !notionalAccountFinite:
		errs = append(errs, errors.New("notional account must be finite"))
	case p.NotionalAccount <= 0:
		errs = append(errs, errors.New("notional account must be positive"))
	}

	stopIntentFinite := isFinite(p.ProtectiveStopIntent)
	switch {
	case !stopIntentFinite:
		errs = append(errs, errors.New("protective stop intent must be finite"))
	case p.ProtectiveStopIntent <= 0:
		errs = append(errs, errors.New("protective stop intent must be positive: a long position cannot be stopped out at or below zero"))
	case entryLevelFinite && p.ProtectiveStopIntent >= p.EntryLevel:
		errs = append(errs, fmt.Errorf("protective stop intent %v must be below the entry level %v for a long position", p.ProtectiveStopIntent, p.EntryLevel))
	}

	// Invariant 1: Risk at Stop matches its derivation.
	if modeRecognised && p.SizingMode == SizingModeVolatilityNormalised && fractionFinite && stopMultipleFinite && riskAtStopFinite {
		if derived := p.UnitVolatilityFraction * p.StopMultiple; p.RiskAtStop != derived {
			errs = append(errs, fmt.Errorf(
				"stated risk at stop %v does not match the derivation %v (unit volatility fraction %v x stop multiple %v): under volatility-normalised sizing risk at stop is derived, never configured (ADR 0003)",
				p.RiskAtStop, derived, p.UnitVolatilityFraction, p.StopMultiple))
		}
	}

	// Invariant 2: the Protective Stop intent matches its derivation.
	if entryLevelFinite && stopMultipleFinite && nFinite && stopIntentFinite {
		if derived := p.EntryLevel - p.StopMultiple*p.N; p.ProtectiveStopIntent != derived {
			errs = append(errs, fmt.Errorf(
				"stated protective stop intent %v does not match the derivation %v (entry level %v - stop multiple %v x n %v)",
				p.ProtectiveStopIntent, derived, p.EntryLevel, p.StopMultiple, p.N))
		}
	}

	// Invariant 3: truncation never risks more than the budget.
	if modeRecognised && p.Quantity > 0 && nFinite && dollarsPerPointFinite && notionalAccountFinite && fractionFinite && stopMultipleFinite && riskAtStopFinite {
		switch p.SizingMode {
		case SizingModeVolatilityNormalised:
			// The same expression order sizing.UnitQuantity uses, so the two
			// agree bit for bit.
			risked := float64(p.Quantity) * (p.N * p.DollarsPerPoint)
			budget := p.NotionalAccount * p.UnitVolatilityFraction
			if risked > budget {
				errs = append(errs, fmt.Errorf(
					"quantity %d exceeds the 1N budget: %v exceeds the permitted %v (notional account %v x unit volatility fraction %v); truncation may only ever risk less",
					p.Quantity, risked, budget, p.NotionalAccount, p.UnitVolatilityFraction))
			}
		case SizingModeFixedRiskAtStop:
			// The same expression order sizing.FixedRiskAtStopQuantity uses.
			risked := float64(p.Quantity) * (p.StopMultiple * p.N * p.DollarsPerPoint)
			budget := p.NotionalAccount * p.RiskAtStop
			if risked > budget {
				errs = append(errs, fmt.Errorf(
					"quantity %d exceeds the risk-at-stop budget: %v exceeds the permitted %v (notional account %v x risk at stop %v); truncation may only ever risk less",
					p.Quantity, risked, budget, p.NotionalAccount, p.RiskAtStop))
			}
		}
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid trade proposal payload: %w", err)
	}
	return nil
}

// ProposalDeclinedPayload records a Signal that fired and deliberately
// produced no position.
//
// It exists because the alternative — emitting nothing — makes "no Signal
// today" and "a Signal whose sizing produced nothing" identical in the
// journal, and the second is a fact about the strategy's capacity that a
// reviewer needs to see. docs/development.md principle 3: record enough
// immutable evidence to explain every accepted *and rejected* decision.
//
// A decline is not an error. The run continues; the Setup is simply not
// traded on this bar, and a Signal is not carried forward (ADR 0011).
type ProposalDeclinedPayload struct {
	InstrumentID string    `json:"instrument_id"`
	PeriodEnd    time.Time `json:"period_end"`
	// SignalID is the ID of the Signal envelope that was declined, so the
	// decline can be joined to the decision it answers.
	SignalID string `json:"signal_id"`
	// Reason is one of the enumerated DeclineReason constants — a closed set
	// so a journal can be grouped by it.
	Reason string `json:"reason"`
	// Detail carries the numbers behind this particular decline, in prose.
	// It is required: a reason without its figures cannot be checked.
	Detail string `json:"detail"`
}

// Validate checks the identifying fields, that Reason is one of the
// enumerated constants rather than free text, and that Detail is present.
func (p ProposalDeclinedPayload) Validate() error {
	var errs []error
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if p.PeriodEnd.IsZero() {
		errs = append(errs, errors.New("period end is required"))
	}
	if p.SignalID == "" {
		errs = append(errs, errors.New("signal id is required: a decline must name the signal it answers"))
	}
	switch p.Reason {
	case DeclineReasonNNotReady, DeclineReasonQuantityBelowOneUnit, DeclineReasonStopIntentNotPositive:
		// recognised
	default:
		errs = append(errs, fmt.Errorf("reason %q is not a recognised decline reason", p.Reason))
	}
	if p.Detail == "" {
		errs = append(errs, errors.New("detail is required: a decline must record the figures that produced it"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid proposal declined payload: %w", err)
	}
	return nil
}
