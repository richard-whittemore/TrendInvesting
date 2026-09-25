package event

import (
	"errors"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// TradeProposalEventType identifies the trade-proposal decision payload for
// the Envelope's Type field. docs/architecture.md names "trade proposals
// returned to LEAN" as the decision boundary's output: this is the Go side of
// that boundary, and it is a proposal, not an order — nothing here assumes a
// fill (ADR 0005 owns the fill model).
const TradeProposalEventType = "strategy.trade.proposed"

// TradeProposalSchemaVersion is the current schema version of
// TradeProposalPayload, for the Envelope's SchemaVersion field.
//
//   - Version 2 added OrderType, GapBufferN and PriceCap (ADR 0005, as
//     amended 2026-09-24): the order the entry rests as and its price cap. A
//     version-1 record decodes OrderType as the empty string, which is not a
//     recognised order type, so it is rejected rather than read as an
//     uncapped order (ADR 0015).
//   - Version 3 added Strength (ADR 0010, as amended by the owner's decision
//     of 2026-09-25): the ranking measure that placed this Signal ahead of
//     the Session's other Signals. A version-2 record decodes it as zero,
//     which this schema's Validate accepts as any other finite Strength
//     would, so an older record is not rejected outright — but its zero is
//     not a claim that Strength was actually computed as zero, since no
//     earlier build ever computed it at all.
const TradeProposalSchemaVersion uint32 = 3

// ProposalDeclinedEventType identifies the payload recorded when a Signal
// fired but produced no position. It exists so that "the strategy recognised
// its entry condition and deliberately took nothing" is a journalled fact
// rather than an absence a reader has to infer.
const ProposalDeclinedEventType = "strategy.proposal.declined"

// ProposalDeclinedSchemaVersion is the current schema version of
// ProposalDeclinedPayload.
//
//   - Version 2 added Kind (ProposalKindEntry|ProposalKindAdd), required, and
//     the CampaignID/RequiredCash/AvailableCash fields that go with it. A
//     version-1 record decodes Kind as the empty string, which is not a
//     recognised value, so it is rejected outright — the same discipline
//     ProposalExpiredSchemaVersion's own version-2 bump follows.
//   - Version 3 makes AvailableCash the spendable cash at the attempt after
//     accepted withdrawal debits (ADR 0020's cash-movement amendment).
//     RequiredCash remains the Unit cost compared against it. Schema-2
//     decisions must not silently acquire this meaning (ADR 0015).
//   - Version 4 makes AvailableCash the spendable cash at the attempt after
//     withdrawal debits AND after the actual cost of every entry and Add fill
//     the snapshot does not yet reflect (ADR 0020: "available = basis -
//     every actual fill cost"). It may therefore be negative: ADR 0020 floors
//     the basis, never the fills taken from it. Schema-3 decisions must not
//     silently acquire this meaning (ADR 0015).
//   - Version 5 added Cap, CapLimit and PostTradeExposure, required exactly
//     when Reason is DeclineReasonUnitCapExceeded (ADR 0008's four Unit caps
//     and the Unclassified Group). A version-4 record decodes Cap as the
//     empty string, which is not a recognised cap name, so a schema-5 record
//     asserting that reason against an older schema is rejected outright —
//     the same discipline every previous field addition follows.
//   - Version 6 changes three meanings, adding no field (ADR 0020, as
//     amended 2026-09-24). RequiredCash is the hold the Unit would have
//     placed, its worst-case cost at its price cap with slippage and
//     commission (under the declared Variant "uncapped", its cost at the
//     level, as before). AvailableCash is spendable cash after fill debits
//     AND every standing hold. PostTradeExposure counts the Units reserved by
//     standing holds as well as the Units committed by fills. Schema-5
//     decisions must not silently acquire these meanings (ADR 0015).
//   - Version 7 added Strength and DeclineReasonInsufficientHistory (ADR
//     0010, as amended by the owner's decision of 2026-09-25). Strength is
//     required and finite for Kind ProposalDeclinedKindEntry whenever Reason
//     is not DeclineReasonInsufficientHistory — the ranking measure this
//     Signal was compared by before it was declined — and must be exactly
//     zero for ProposalDeclinedKindAdd (an Add answers no Signal) and for
//     DeclineReasonInsufficientHistory (no Strength could be computed for an
//     instrument this reason declines to rank at all). Reason
//     DeclineReasonInsufficientHistory is recognised only from this schema
//     version on, mirroring DeclineReasonUnitCapExceeded's own schema-5
//     addition. A version-6 record decodes Strength as zero, which this
//     version's Validate accepts for any reason it also recognises on that
//     record's own terms; it is not a claim that Strength was computed.
const ProposalDeclinedSchemaVersion uint32 = 7

// The rule names for TradeProposalPayload.Rule, one per Sizing Mode.
//
// Each names what the rule computes, not which vendor's system it resembles
// and not the parameter values it happened to run with. A Variant that
// changes the Unit Volatility Fraction or
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
	// DeclineReasonInsufficientCash means the hold the Unit would place —
	// its worst-case cost: quantity x (price cap + slippage) x dollars per
	// point plus commission, or quantity x level x dollars per point under
	// the declared Variant "uncapped" — exceeds snapshot-backed spendable
	// cash after accepted withdrawal debits, the fills the snapshot does not
	// yet reflect, and every hold still standing (ADR 0010 and ADR 0020, as
	// amended). There is no partial Unit and no borrowing: the whole Unit is
	// skipped, and RequiredCash/AvailableCash carry the two figures the
	// comparison was made from.
	DeclineReasonInsufficientCash = "insufficient-cash"
	// DeclineReasonUnitCostNotRepresentable means the Unit's cost is a
	// finite number only in exact arithmetic: quantity x the order's resting
	// level x dollars per point, each of them finite, multiplies past the
	// float64 range. Such a Unit costs more than any cash that can be held,
	// so it is skipped exactly as an unaffordable one is (ADR 0010) rather
	// than stopping the run. RequiredCash and AvailableCash are both zero:
	// the cost is the one figure that cannot be stated — JSON cannot encode
	// an infinity at all — and Detail carries the operands it was formed
	// from instead.
	DeclineReasonUnitCostNotRepresentable = "unit-cost-not-representable"
	// DeclineReasonUnitCapExceeded means the Unit that would result from
	// this proposal was checked against post-trade exposure — the Units
	// that would be held after this Unit joined its instrument, its
	// correlation group and the account's total long exposure — and one of
	// ADR 0008's four caps (or the Unclassified Group's own, at the
	// loosely-correlated level) is at or below that exposure already before
	// this Unit, so admitting it would exceed the cap [T p.16]. Cap,
	// CapLimit and PostTradeExposure name which cap bound and the exposure
	// that would have resulted.
	DeclineReasonUnitCapExceeded = "unit-cap-exceeded"
	// DeclineReasonInsufficientHistory means a Tier A Signal could not be
	// ranked at all: fewer than the split-adjusted closes Strength needs, or
	// fewer than the raw closes/volumes the 20-day median dollar volume
	// tie-break needs, or an N that is not a usable volatility reading at the
	// Session's own close (ADR 0010, as amended by the owner's decision of
	// 2026-09-25). The Signal is declined rather than ranked last: an
	// incomparable instrument has no place in a total order. Strength is
	// zero for this reason, since none was computed.
	DeclineReasonInsufficientHistory = "insufficient-history"
)

// The five cap identities ProposalDeclinedPayload.Cap names, one per level
// of Faith's correlation grouping (ADR 0008, The Turtle Rules p.16):
// CapInstrument and CapTotalLong are checked for every instrument;
// CapUnclassifiedGroup applies in place of BOTH CapIndustry and CapSector
// for an instrument with no point-in-time industry/sector label, since
// every such instrument shares CONTEXT.md's single Unclassified Group
// rather than two separate ones.
const (
	CapInstrument        = "instrument"
	CapIndustry          = "industry"
	CapSector            = "sector"
	CapUnclassifiedGroup = "unclassified-group"
	CapTotalLong         = "total-long"
)

// The two Kind values ProposalDeclinedPayload accepts, mirroring
// ProposalExpiredPayload's own Kind: which sizing path produced no position.
// There is no ProposalKindExit here — an open Campaign's exit is a mandatory
// closure, never a sized proposal that cash can decline.
const (
	// ProposalDeclinedKindEntry is a Signal that fired and was sized (or
	// failed to size) into a new Campaign's opening Unit. SignalID is
	// required; CampaignID must be empty, since no Campaign exists yet.
	ProposalDeclinedKindEntry = ProposalKindEntry
	// ProposalDeclinedKindAdd is an open Campaign's Add Ladder rung being
	// reached and sized (or failing to size) into a further Unit. CampaignID
	// is required; SignalID must be empty, since an Add answers no Signal
	// (AddProposalPayload's own doc comment).
	ProposalDeclinedKindAdd = ProposalKindAdd
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
//   - Caps. This is one Unit, and no cap is checked (ConfigurationPayload.
//     MaxUnits models one of ADR 0008's four levels; the cap check itself is
//     applied elsewhere). A proposal is not a permission to trade.
//   - Fills. EntryLevel is the level a resting order sits at, not a fill
//     price; slippage is a fill concern owned by the fill model (ADR 0013).
//   - Drawdown. NotionalAccount is the configured starting equity; Drawdown
//     Steps and yearly re-basing (ADR 0007) are applied elsewhere.
//   - Campaign state. Freezing N and the Unit size at first entry (ADR 0006)
//     happens elsewhere; this payload carries what that freeze will need.
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
	// EntryLevel is the Entry Channel high the breakout exceeded (SignalPayload.
	// EntryChannelHigh) — the level a resting buy-stop actually sits at under
	// ADR 0005, not the breakout bar's own high (SignalPayload.BreakoutHigh).
	// Faith's wording is "exceeded by a single tick" [T p.19]; the Baseline's
	// tick increment is zero, with the Signal's own strict exceedance doing
	// that work (a baseline-declared adaptation, ADR 0012). What actually
	// fills there is not decided here.
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
	// RiskAtStop is the **declared budget**: the fraction of the Notional
	// Account the Sizing Mode is keyed to — derived as UnitVolatilityFraction
	// x StopMultiple under volatility-normalised sizing, the configured input
	// under fixed-risk-at-stop (CONTEXT.md: "Risk at Stop"; ADR 0003). It is
	// deliberately not adjusted for truncation.
	RiskAtStop float64 `json:"risk_at_stop"`
	// RealisedRiskAtStop is what the whole-share Quantity above actually
	// risks: Quantity x StopMultiple x N x DollarsPerPoint / NotionalAccount.
	//
	// The gap between it and RiskAtStop is the truncation from a fractional
	// Unit to a whole one, and it always points the same way — a truncated
	// position risks less than the budget, never more. Both are recorded
	// because neither can stand for the other: RiskAtStop is what the
	// strategy declared it would risk and is the figure the Sizing Mode's
	// arithmetic is keyed to, while RealisedRiskAtStop is what this
	// particular position stands to lose, and a journal carrying only the
	// first overstates that (Faith's Heating Oil Unit declares 2 % and
	// realises 1.895 %, The Turtle Rules p.15).
	RealisedRiskAtStop float64 `json:"realised_risk_at_stop"`
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
	// OrderType, GapBufferN and PriceCap state the order the entry rests as
	// (ADR 0005, as amended 2026-09-24): a stop-limit at EntryLevel capped at
	// PriceCap = EntryLevel + GapBufferN x N, or, in the declared Variant
	// "uncapped", a stop-market order with GapBufferN and PriceCap both zero
	// (CONTEXT.md: "Price cap"). Validate re-derives the cap exactly.
	OrderType  OrderType `json:"order_type"`
	GapBufferN float64   `json:"gap_buffer_n"`
	PriceCap   float64   `json:"price_cap"`
	// Strength is Faith's ranking measure that placed this Signal ahead of
	// the Session's other Signals: (close(d) - close(d-63)) / N(d)
	// (CONTEXT.md: "Strength"; The Turtle Rules p.29, ADR 0010, as amended by
	// the owner's decision of 2026-09-25). It can be negative — an
	// instrument can still make a fresh 55-bar high while its longer-term
	// price change is negative — so only finiteness is checked.
	Strength float64 `json:"strength"`
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
//  3. **The realised risk matches its derivation and does not exceed the
//     budget.** RealisedRiskAtStop must be exactly
//     sizing.RealisedRiskAtStop of this payload's own fields — the same
//     function the producer used, called rather than reimplemented, so the
//     exact comparison cannot fail on a last-bit difference between two
//     algebraically identical expressions — and it must not exceed
//     RiskAtStop, in either mode. Truncation may only ever risk less than
//     the declared budget.
//
//  4. **Truncation never risks more than the budget.** Under
//     volatility-normalised sizing, Quantity x N x DollarsPerPoint must not
//     exceed NotionalAccount x UnitVolatilityFraction — the 1N budget The
//     Turtle Rules p.14 actually states, and the exact expression
//     sizing.UnitQuantity guarantees, so this is exact rather than
//     approximate. (The at-the-stop form follows from it, since Risk at Stop
//     is that fraction times the Stop Multiple.) Under fixed-risk-at-stop
//     the budget is stated at the stop instead: Quantity x StopMultiple x N
//     x DollarsPerPoint must not exceed NotionalAccount x RiskAtStop.
//
//  5. **The price cap matches its derivation.** A stop-limit's PriceCap
//     must be exactly EntryLevel + GapBufferN x N (sizing.PriceCap), with
//     GapBufferN finite and at least zero; a stop-market order carries
//     neither (ADR 0005, as amended 2026-09-24).
func (p TradeProposalPayload) Validate() error {
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

	realisedFinite := isFinite(p.RealisedRiskAtStop)
	switch {
	case !realisedFinite:
		errs = append(errs, errors.New("realised risk at stop must be finite"))
	case p.RealisedRiskAtStop <= 0 || p.RealisedRiskAtStop > 1:
		errs = append(errs, errors.New("realised risk at stop must be greater than zero and at most one"))
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

	if !isFinite(p.Strength) {
		errs = append(errs, errors.New("strength must be finite"))
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
		if derived := p.EntryLevel - sizing.Product(p.StopMultiple, p.N); p.ProtectiveStopIntent != derived {
			errs = append(errs, fmt.Errorf(
				"stated protective stop intent %v does not match the derivation %v (entry level %v - stop multiple %v x n %v)",
				p.ProtectiveStopIntent, derived, p.EntryLevel, p.StopMultiple, p.N))
		}
	}

	// Invariant 5: the price cap matches its derivation (ADR 0005, as
	// amended 2026-09-24).
	errs = append(errs, validatePriceCap(p.OrderType, p.GapBufferN, p.PriceCap, p.EntryLevel, p.N, entryLevelFinite && nFinite)...)

	// Invariant 3: the realised risk matches its derivation and does not
	// exceed the declared budget.
	if p.Quantity > 0 && stopMultipleFinite && nFinite && dollarsPerPointFinite && notionalAccountFinite && realisedFinite {
		if derived, ok := sizing.RealisedRiskAtStop(p.Quantity, p.StopMultiple, p.N, p.DollarsPerPoint, p.NotionalAccount); !ok || p.RealisedRiskAtStop != derived {
			errs = append(errs, fmt.Errorf(
				"stated realised risk at stop %v does not match the derivation %v (quantity %d x stop multiple %v x n %v x dollars per point %v / notional account %v)",
				p.RealisedRiskAtStop, derived, p.Quantity, p.StopMultiple, p.N, p.DollarsPerPoint, p.NotionalAccount))
		}
	}
	if realisedFinite && riskAtStopFinite && p.RealisedRiskAtStop > p.RiskAtStop {
		errs = append(errs, fmt.Errorf(
			"realised risk at stop %v exceeds the declared risk at stop %v: truncation may only ever risk less than the budget, never more",
			p.RealisedRiskAtStop, p.RiskAtStop))
	}

	// Invariant 4: truncation never risks more than the budget.
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
// A decline is not an error. The run continues; the Setup (or the open
// Campaign, for an Add) is simply not traded on this bar, and neither a
// Signal nor an Add opportunity is carried forward (ADR 0011).
type ProposalDeclinedPayload struct {
	InstrumentID string    `json:"instrument_id"`
	PeriodEnd    time.Time `json:"period_end"`
	// Kind discriminates which sizing path produced no position:
	// ProposalDeclinedKindEntry or ProposalDeclinedKindAdd. Required and
	// closed, mirroring FillPayload.Kind's and ProposalExpiredPayload.Kind's
	// own discipline: an empty or unrecognised value is rejected rather than
	// defaulted.
	Kind string `json:"kind"`
	// SignalID is the ID of the Signal envelope that was declined, so the
	// decline can be joined to the decision it answers. Required for Kind
	// ProposalDeclinedKindEntry; must be empty for ProposalDeclinedKindAdd,
	// which answers no Signal (AddProposalPayload's own doc comment).
	SignalID string `json:"signal_id"`
	// CampaignID identifies the open Campaign whose Add Ladder rung was
	// declined. Required for Kind ProposalDeclinedKindAdd; must be empty for
	// ProposalDeclinedKindEntry, since no Campaign exists yet.
	CampaignID string `json:"campaign_id"`
	// Reason is one of the enumerated DeclineReason constants — a closed set
	// so a journal can be grouped by it.
	Reason string `json:"reason"`
	// Detail carries the numbers behind this particular decline, in prose.
	// It is required: a reason without its figures cannot be checked.
	Detail string `json:"detail"`
	// RequiredCash and AvailableCash are the two figures
	// DeclineReasonInsufficientCash was compared from: the Unit's cost, and
	// snapshot-backed spendable cash at the attempt after accepted withdrawal
	// debits and the fills the snapshot does not yet reflect (ADR 0010 and
	// ADR 0020). Required and finite when Reason is
	// DeclineReasonInsufficientCash, with RequiredCash not negative and
	// strictly greater than AvailableCash — exactly the comparison that makes
	// the Unit unaffordable. AvailableCash may be negative: a fill the basis
	// could not fund is recorded at its actual cost, and ADR 0020 forbids
	// flooring the remainder. Both must be exactly zero for every other
	// reason, so a field that means nothing for that reason cannot carry a
	// stray number.
	RequiredCash  float64 `json:"required_cash"`
	AvailableCash float64 `json:"available_cash"`
	// Cap, CapLimit and PostTradeExposure are the three figures
	// DeclineReasonUnitCapExceeded was decided from: which of ADR 0008's
	// caps bound (one of the CapInstrument/CapIndustry/CapSector/
	// CapUnclassifiedGroup/CapTotalLong constants), the configured limit for
	// that cap, and the Unit count that would have resulted — across every
	// open Campaign sharing that cap's grouping — had this Unit been taken.
	// Required and consistent (PostTradeExposure strictly greater than
	// CapLimit, exactly the comparison that makes the Unit excessive) when
	// Reason is DeclineReasonUnitCapExceeded; Cap must be empty and both
	// counts zero for every other reason, mirroring RequiredCash/
	// AvailableCash's own discipline for DeclineReasonInsufficientCash.
	Cap               string `json:"cap"`
	CapLimit          int    `json:"cap_limit"`
	PostTradeExposure int    `json:"post_trade_exposure"`
	// Strength is the ranking measure (ADR 0010, as amended by the owner's
	// decision of 2026-09-25) that placed this Signal in the session-close
	// pass. Required and finite for Kind ProposalDeclinedKindEntry whenever
	// Reason is not DeclineReasonInsufficientHistory — every other entry-kind
	// reason is reached only after ranking has already computed it. Zero for
	// ProposalDeclinedKindAdd (an Add answers no Signal) and for
	// DeclineReasonInsufficientHistory (no Strength could be computed at
	// all), mirroring RequiredCash/AvailableCash's own reason-keyed
	// discipline above.
	Strength float64 `json:"strength"`
}

// Validate checks the identifying fields, that Kind is one of the recognised
// values and that SignalID/CampaignID are present or absent exactly as that
// Kind requires, that Reason is one of the enumerated constants rather than
// free text, that Detail is present, and — only for Reason
// DeclineReasonInsufficientCash — that RequiredCash and AvailableCash are
// finite and consistent with an actual shortfall under the current schema;
// RequiredCash must not be negative. For every other reason both must be
// zero. Only for Reason DeclineReasonUnitCapExceeded — a schema-5 addition —
// Cap must be one of the five recognised cap identities and CapLimit/
// PostTradeExposure must be consistent with an actual excess; for every
// other reason all three must be empty/zero.
func (p ProposalDeclinedPayload) Validate() error {
	return p.ValidateSchema(ProposalDeclinedSchemaVersion)
}

// ValidateSchema checks a decline under its declared schema (ADR 0015).
// Schemas 2 and 3 require nonnegative AvailableCash; schema 4 permits the
// negative ledger remainder introduced by ADR 0020. Schema 5 additionally
// recognises DeclineReasonUnitCapExceeded and its Cap/CapLimit/
// PostTradeExposure fields; a record declared under an earlier schema cannot
// assert that reason. Schema 6 changes the meaning of the cash and exposure
// figures without changing their shape (ProposalDeclinedSchemaVersion), so
// it is validated exactly as schema 5 is. Schema 1 lacks the required Kind and is unsupported,
// as are unknown schemas. Validating an older payload does not upgrade its
// cash fields to the current meaning.
func (p ProposalDeclinedPayload) ValidateSchema(version uint32) error {
	switch version {
	case 2, 3, 4, 5, 6, ProposalDeclinedSchemaVersion:
	default:
		return fmt.Errorf("proposal declined payload schema version %d is not supported", version)
	}
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
	switch p.Kind {
	case ProposalDeclinedKindEntry:
		if p.SignalID == "" {
			errs = append(errs, errors.New("signal id is required: an entry-kind decline must name the signal it answers"))
		}
		if p.CampaignID != "" {
			errs = append(errs, fmt.Errorf("campaign id must be empty for an entry-kind decline (got %q): no campaign exists yet", p.CampaignID))
		}
	case ProposalDeclinedKindAdd:
		if p.SignalID != "" {
			errs = append(errs, fmt.Errorf("signal id must be empty for an add-kind decline (got %q): an add proposal answers no signal", p.SignalID))
		}
		if p.CampaignID == "" {
			errs = append(errs, errors.New("campaign id is required: an add-kind decline must name the campaign whose rung it answers"))
		}
	default:
		errs = append(errs, fmt.Errorf("kind %q is not a recognised proposal declined kind", p.Kind))
	}
	switch p.Reason {
	case DeclineReasonNNotReady, DeclineReasonQuantityBelowOneUnit, DeclineReasonStopIntentNotPositive,
		DeclineReasonInsufficientCash, DeclineReasonUnitCostNotRepresentable:
		// recognised
	case DeclineReasonUnitCapExceeded:
		if version < 5 {
			errs = append(errs, fmt.Errorf("reason %q is not recognised before proposal declined schema 5 (ADR 0015): a version-%d record cannot have asserted it", DeclineReasonUnitCapExceeded, version))
		}
	case DeclineReasonInsufficientHistory:
		if version < 7 {
			errs = append(errs, fmt.Errorf("reason %q is not recognised before proposal declined schema 7 (ADR 0015): a version-%d record cannot have asserted it", DeclineReasonInsufficientHistory, version))
		}
	default:
		errs = append(errs, fmt.Errorf("reason %q is not a recognised decline reason", p.Reason))
	}
	if p.Detail == "" {
		errs = append(errs, errors.New("detail is required: a decline must record the figures that produced it"))
	}

	requiredCashFinite := isFinite(p.RequiredCash)
	availableCashFinite := isFinite(p.AvailableCash)
	if p.Reason == DeclineReasonInsufficientCash {
		switch {
		case !requiredCashFinite:
			errs = append(errs, errors.New("required cash must be finite"))
		case p.RequiredCash < 0:
			errs = append(errs, errors.New("required cash must not be negative"))
		}
		if !availableCashFinite {
			errs = append(errs, errors.New("available cash must be finite"))
		}
		if version <= 3 && p.AvailableCash < 0 {
			errs = append(errs, errors.New("available cash must not be negative in proposal declined schema 3 or below"))
		}
		if requiredCashFinite && availableCashFinite && p.RequiredCash <= p.AvailableCash {
			errs = append(errs, fmt.Errorf(
				"required cash %v does not exceed available cash %v: a decline reasoned insufficient-cash must record an actual shortfall",
				p.RequiredCash, p.AvailableCash))
		}
	} else {
		// Compared with zero directly, never "finite and non-zero": NaN is
		// not equal to zero and neither is an infinity, so both are caught
		// by the same rule that keeps a field meaningless for this reason
		// from carrying any number at all.
		if p.RequiredCash != 0 {
			errs = append(errs, fmt.Errorf("required cash must be zero for reason %q (got %v): it is only meaningful for %q", p.Reason, p.RequiredCash, DeclineReasonInsufficientCash))
		}
		if p.AvailableCash != 0 {
			errs = append(errs, fmt.Errorf("available cash must be zero for reason %q (got %v): it is only meaningful for %q", p.Reason, p.AvailableCash, DeclineReasonInsufficientCash))
		}
	}

	if p.Reason == DeclineReasonUnitCapExceeded {
		switch p.Cap {
		case CapInstrument, CapIndustry, CapSector, CapUnclassifiedGroup, CapTotalLong:
			// recognised
		default:
			errs = append(errs, fmt.Errorf("cap %q is not a recognised cap identity", p.Cap))
		}
		if p.CapLimit <= 0 {
			errs = append(errs, errors.New("cap limit must be a positive integer"))
		}
		if p.PostTradeExposure <= p.CapLimit {
			errs = append(errs, fmt.Errorf(
				"post-trade exposure %d does not exceed the cap limit %d: a decline reasoned unit-cap-exceeded must record an actual excess",
				p.PostTradeExposure, p.CapLimit))
		}
	} else {
		if p.Cap != "" {
			errs = append(errs, fmt.Errorf("cap must be empty for reason %q (got %q): it is only meaningful for %q", p.Reason, p.Cap, DeclineReasonUnitCapExceeded))
		}
		if p.CapLimit != 0 {
			errs = append(errs, fmt.Errorf("cap limit must be zero for reason %q (got %d): it is only meaningful for %q", p.Reason, p.CapLimit, DeclineReasonUnitCapExceeded))
		}
		if p.PostTradeExposure != 0 {
			errs = append(errs, fmt.Errorf("post-trade exposure must be zero for reason %q (got %d): it is only meaningful for %q", p.Reason, p.PostTradeExposure, DeclineReasonUnitCapExceeded))
		}
	}

	// Strength is meaningful only once ranking has actually computed it: an
	// Add answers no Signal at all, and DeclineReasonInsufficientHistory is
	// the reason ranking never ran (mirroring RequiredCash/AvailableCash's
	// own zero-for-every-other-reason discipline above).
	if p.Kind == ProposalDeclinedKindAdd || p.Reason == DeclineReasonInsufficientHistory {
		if p.Strength != 0 {
			errs = append(errs, fmt.Errorf("strength must be zero for kind %q reason %q (got %v): no Strength was computed for it", p.Kind, p.Reason, p.Strength))
		}
	} else if !isFinite(p.Strength) {
		errs = append(errs, errors.New("strength must be finite"))
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid proposal declined payload: %w", err)
	}
	return nil
}

// ProposalExpiredEventType identifies the payload recorded when a trade
// proposal was superseded without ever being filled.
//
// It completes the lifecycle a Signal starts: a Signal is never followed by
// silence (a proposal or a decline always follows it), and now neither is a
// proposal — every proposal reaches exactly one terminal event, a Campaign
// (CampaignOpenedEventType) or this. Without it, "this proposal expired" and
// "this proposal never existed" are the same absence in the journal, and the
// error a consumer raises for a fill arriving too late has nothing behind it
// that a reviewer can check.
const ProposalExpiredEventType = "strategy.proposal.expired"

// ProposalExpiredSchemaVersion is the current schema version of
// ProposalExpiredPayload.
//
// A field that is REQUIRED always carries its own version bump (ADR 0015),
// following the same discipline as FillSchemaVersion.
//
//   - Version 2 added Kind (ProposalKindEntry|ProposalKindExit), required, so
//     this one expiry mechanism is reused for an outstanding exit proposal
//     (strategy.exit.proposed) rather than minting a second event type. An
//     older version-1 record decodes Kind as the empty string, which is not
//     a recognised value, so it is rejected outright. ProposalKindAdd was
//     later a further recognised value of the same, already-required field,
//     so it did not need its own bump: no version-2 record ever wrote "add",
//     so there is no existing record the new value could be mistaken for.
//   - Version 3 added EarliestFillAt, needed because ExpiredAt's required
//     relationship to PeriodEnd differs by Reason (see EarliestFillAt's own
//     doc comment and Validate). A version-2 record decodes EarliestFillAt
//     as the zero time, which Validate would read as "no lower bound at
//     all" — silently weakening the chronology check for an old record
//     rather than rejecting it — so version 2 is rejected.
const ProposalExpiredSchemaVersion uint32 = 3

// The three Kind values ProposalExpiredPayload accepts. An entry-kind expiry
// is a trade proposal (strategy.trade.proposed) that a Signal produced and
// the next bar superseded without a fill; an exit-kind expiry is an exit
// proposal (strategy.exit.proposed, ExitProposalPayload) that an open
// Campaign's Exit Channel breach produced and the next bar superseded
// without an exit fill; an add-kind expiry is an Add proposal
// (strategy.add.proposed, AddProposalPayload) that an open Campaign's rung
// being reached produced and the next bar superseded without an Add fill.
// All three share the same lifecycle rule (ADR 0011: no persistent proposal
// memory in the Baseline), which is why one payload serves all of them
// rather than a separate one per kind.
const (
	ProposalKindEntry = "entry"
	ProposalKindExit  = "exit"
	ProposalKindAdd   = "add"
)

// RuleExitProposalExpiresWithItsBar names the rule for
// ProposalExpiredPayload.Rule when Kind is ProposalKindExit: an exit
// proposal belongs to one bar and expires with it, the same lifecycle
// RuleSignalExpiresWithItsBar states for an entry-kind proposal — kept as a
// separate constant (rather than reusing that one) because an exit proposal
// answers no Signal at all, so a rule named "signal.expires..." would
// misdescribe it.
const RuleExitProposalExpiresWithItsBar = "exit-proposal.expires.with-its-bar"

// RuleAddProposalExpiresWithItsBar identifies an Add's bar-based expiry
// under ADR 0011: the next instrument bar for an ordinary Add, the second
// subsequent bar for a fill-chained Add (amended 2026-09-24). The original
// identifier is retained; AddProposalPayload.ValidForSessions states the
// window. An Add answers no Signal, so it has its own rule identity.
const RuleAddProposalExpiresWithItsBar = "add-proposal.expires.with-its-bar"

// RuleAddProposalSupersededByStop names the rule for
// ProposalExpiredPayload.Rule when Reason is ExpiryReasonSupersededByStop: an
// outstanding Add proposal is cancelled by a stop fill closing part or all of
// the same Campaign, not by the next bar (ADR 0011, as amended 2026-09-24).
const RuleAddProposalSupersededByStop = "add-proposal.superseded-by-stop"

// RuleExitProposalSupersededByDelisting names the rule for
// ProposalExpiredPayload.Rule when Reason is
// ExpiryReasonSupersededByDelisting and Kind is ProposalKindExit: an
// outstanding exit proposal is cancelled the instant a delisting forces the
// same Campaign closed (ADR 0009), rather than waiting for ADR 0011's
// ordinary next-bar expiry — a delisted instrument produces no further bar
// for that expiry to ever run on.
const RuleExitProposalSupersededByDelisting = "exit-proposal.superseded-by-delisting"

// RuleAddProposalSupersededByDelisting is RuleExitProposalSupersededByDelisting's
// counterpart for Kind ProposalKindAdd.
const RuleAddProposalSupersededByDelisting = "add-proposal.superseded-by-delisting"

// RuleEntryProposalSupersededByDelisting is
// RuleExitProposalSupersededByDelisting's counterpart for Kind
// ProposalKindEntry: an outstanding entry proposal is cancelled the instant a
// delisting arrives for the instrument. The invariant this serves is ADR
// 0009's — a delisted instrument cannot be traded again in the run — and
// cancelling is what upholds it: a proposal left outstanding could still be
// filled, opening a Campaign in an instrument that has stopped trading.
const RuleEntryProposalSupersededByDelisting = "entry-proposal.superseded-by-delisting"

// RuleSignalExpiresWithItsBar names the rule for ProposalExpiredPayload.Rule:
// a Signal belongs to one bar and expires with it, so the proposal that Signal
// produced inherits the same lifetime. A trending instrument re-qualifies by
// making a new high and is proposed again on its own (ADR 0011).
const RuleSignalExpiresWithItsBar = "signal.expires.with-its-bar"

// ADRSignalExpiry is the ADR ProposalExpiredPayload.ADR cites: ADR 0011, which
// decides that a Signal never outlives its bar and that persistent Signals are
// a declared Variant rather than the Baseline.
const ADRSignalExpiry = "0011"

// ExpiryReasonSupersededByNextBar is the ORDINARY expiry reason: the
// proposal's expiry bar for the instrument arrived and no fill for the
// proposal ever did — the next bar, or for a fill-chained Add the one after
// (ADR 0011, as amended 2026-09-24). It is an enumerated value rather than
// free text for the same reason the decline reasons are — a journal must be
// groupable by it.
const ExpiryReasonSupersededByNextBar = "superseded-by-next-bar"

// ExpiryReasonSupersededByStop is the SECOND expiry reason: an outstanding
// Add proposal (ProposalKindAdd only — an entry or exit proposal has no analogous
// interaction with a stop fill) is cancelled the instant a stop fill closes
// PART or ALL of the same Campaign, rather than waiting for ADR 0011's
// ordinary next-bar expiry (the full close since ADR 0011's 2026-09-24
// amendment). Without this, a fill for that stale proposal could still
// arrive before the next bar's own expiry ever ran, bringing a further Unit
// into a Campaign that has already started coming off, or one already
// closed. ExpiredAt for this reason is the CLOSING FILL's own timestamp, not a
// bar's PeriodEnd.
const ExpiryReasonSupersededByStop = "superseded-by-stop"

// ExpiryReasonInputStreamEnded is the THIRD expiry reason: the run's input
// stream ended (RunCompletedEventType) while the proposal was still
// outstanding, so the next bar that would have superseded it under ADR 0011
// never arrived. It is valid for every Kind, since an entry, an Add and an
// exit proposal can all be outstanding when a run ends.
//
// ExpiredAt for this reason is the instant the stream ended, which is at the
// earliest the proposal's own bar: unlike ExpiryReasonSupersededByNextBar it
// is therefore NOT required to fall strictly after PeriodEnd, because there
// is no later bar — that absence is the whole reason the event exists.
//
// A new value of an already-required field needs no schema bump (see
// ProposalExpiredSchemaVersion): no earlier record ever wrote it, so there is
// nothing an older reader could mistake for it.
const ExpiryReasonInputStreamEnded = "input-stream-ended"

// ExpiryReasonSupersededByDelisting is the FOURTH expiry reason: whatever
// proposal is outstanding for an instrument is cancelled the instant a
// delisting arrives for it (ADR 0009), rather than waiting for ADR 0011's
// ordinary next-bar expiry. A delisted instrument produces no further
// completed bar, so without this the ordinary mechanism would never run and
// the proposal would simply be forgotten in memory rather than reaching a
// journalled terminal event — "every proposal reaches a Campaign or an
// expiry" (see this payload's own doc comment).
//
// Valid for every Kind, and an ENTRY proposal is not the marginal case but
// the dangerous one: it is outstanding precisely when no Campaign is open,
// which is the state a delisting would otherwise pass over as a no-op, and a
// proposal left live there can still be filled — opening a Campaign in an
// instrument that has stopped trading.
//
// ExpiredAt for this reason is the corporate action's own EffectiveAt
// (CorporateActionPayload), mirroring ExpiryReasonSupersededByStop's use of
// the closing fill's own timestamp rather than a bar's PeriodEnd, and for the
// identical reason: it can legitimately fall inside the SAME bar that raised
// the proposal.
//
// A new value of an already-required field needs no schema bump, for the
// identical reason ExpiryReasonInputStreamEnded's own bump-free addition
// states above.
const ExpiryReasonSupersededByDelisting = "superseded-by-delisting"

// ProposalExpiredPayload records a trade proposal that was never filled and
// has now been superseded.
//
// An expiry is not an error and not a failure of the strategy: under ADR 0011
// the Baseline holds no pending-Signal memory, so a proposal that the market
// did not fill inside its own bar simply ceases to exist. What the event adds
// is that the cessation is visible — including as the count a reviewer needs
// to ask what fraction of proposals actually became Campaigns.
type ProposalExpiredPayload struct {
	InstrumentID string `json:"instrument_id"`
	// Kind discriminates which proposal this is: ProposalKindEntry or
	// ProposalKindExit. Required and closed, mirroring
	// FillPayload.Kind's own discipline: an empty or unrecognised value is
	// rejected rather than defaulted.
	Kind string `json:"kind"`
	// ProposalID and SignalID name the decision chain that has now ended.
	// SignalID is required for ProposalKindEntry (a trade proposal always
	// answers a Signal) and must be empty for ProposalKindExit (an exit
	// proposal is raised directly from an open Campaign's per-bar
	// evaluation, never from a Signal — see ExitProposalPayload's doc
	// comment).
	ProposalID string `json:"proposal_id"`
	SignalID   string `json:"signal_id"`
	// PeriodEnd is the completed bar the expired proposal belonged to.
	// ExpiredAt is when the proposal actually ended: for Reason
	// ExpiryReasonSupersededByNextBar, the period end of the bar that
	// superseded it (always strictly LATER than PeriodEnd — a proposal is
	// superseded by a later bar); for Reason ExpiryReasonSupersededByStop,
	// the closing fill's own timestamp, which can legitimately fall AT OR
	// BEFORE PeriodEnd (ADR 0005 makes a stop a resting order that can fill
	// inside the SAME bar that proposed the Add it cancels, not only on a
	// later one). See Validate for the reason-dependent rule this asymmetry
	// requires.
	PeriodEnd time.Time `json:"period_end"`
	ExpiredAt time.Time `json:"expired_at"`
	// EarliestFillAt is the earliest instant at which an order for THIS
	// proposal could have executed — the period end of the bar BEFORE the
	// proposal's own decision bar (the identical figure
	// pendingProposalState.earliestFillAt/pendingAddProposalState.earliestFillAt
	// already carry internally), restated here so ExpiredAt's chronology is
	// checkable from the event alone. Required to hold for EVERY Reason —
	// unlike the PeriodEnd relationship above, ExpiredAt must always be
	// strictly after this, since no execution or supersession can predate
	// the earliest moment an order for the proposal could have existed. The
	// zero time here means "no lower bound" (a proposal raised on an
	// instrument's very first decision bar — see
	// pendingProposalState.earliestFillAt's own doc comment for why that
	// degrades correctly rather than needing a special case): every real
	// ExpiredAt is after it.
	EarliestFillAt time.Time `json:"earliest_fill_at"`
	// Rule and ADR name the rule that produced this decision.
	Rule string `json:"rule"`
	ADR  string `json:"adr"`
	// Reason is one of the enumerated expiry reasons.
	Reason string `json:"reason"`
	// Quantity and Level restate what was proposed and not taken, so the
	// expiry is readable without joining back to the proposal — the entry
	// level for an entry-kind expiry, the Exit Channel level for an
	// exit-kind expiry.
	Quantity int64   `json:"quantity"`
	Level    float64 `json:"level"`
}

// Validate checks the identifying fields, that Kind is one of the recognised
// values and that SignalID is present or absent exactly as that Kind
// requires, that Reason is one of the enumerated constants, that the
// restated proposal figures are usable, and ExpiredAt's chronology:
// strictly after EarliestFillAt for EVERY Reason (no execution or
// supersession can predate the earliest moment an order for the proposal
// could have existed), and — ADDITIONALLY, for Reason
// ExpiryReasonSupersededByNextBar only — strictly after PeriodEnd too (a
// proposal is superseded by a LATER bar in that case; ExpiryReasonSupersededByStop's
// own closing fill can legitimately land inside the SAME bar, see
// ExpiredAt's own doc comment), and — for Reason ExpiryReasonInputStreamEnded
// only — at or after PeriodEnd, non-strictly (ExpiryReasonInputStreamEnded's
// own doc comment: "at the earliest the proposal's own bar").
func (p ProposalExpiredPayload) Validate() error {
	var errs []error
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if p.ProposalID == "" {
		errs = append(errs, errors.New("proposal id is required: an expiry must name the proposal that ended"))
	}
	switch p.Kind {
	case ProposalKindEntry:
		if p.SignalID == "" {
			errs = append(errs, errors.New("signal id is required: an expiry must name the signal behind the proposal"))
		}
	case ProposalKindExit:
		if p.SignalID != "" {
			errs = append(errs, fmt.Errorf("signal id must be empty for an exit-kind expiry (got %q): an exit proposal is not sized from a signal", p.SignalID))
		}
	case ProposalKindAdd:
		if p.SignalID != "" {
			errs = append(errs, fmt.Errorf("signal id must be empty for an add-kind expiry (got %q): an add proposal is not sized from a signal", p.SignalID))
		}
	default:
		errs = append(errs, fmt.Errorf("kind %q is not a recognised proposal kind", p.Kind))
	}
	periodEndPresent := !p.PeriodEnd.IsZero()
	switch {
	case !periodEndPresent:
		errs = append(errs, errors.New("period end is required"))
	case !writableTime(p.PeriodEnd):
		errs = append(errs, errors.New("period end cannot be written as RFC 3339"))
	}
	if !writableTime(p.EarliestFillAt) {
		errs = append(errs, errors.New("earliest fill at cannot be written as RFC 3339"))
	}
	switch {
	case p.ExpiredAt.IsZero():
		errs = append(errs, errors.New("expired at is required"))
	case !writableTime(p.ExpiredAt):
		errs = append(errs, errors.New("expired at cannot be written as RFC 3339"))
	default:
		// Holds for EVERY Reason: see EarliestFillAt's own doc comment for
		// why the zero value needs no special case.
		if !p.ExpiredAt.After(p.EarliestFillAt) {
			errs = append(errs, fmt.Errorf("expired at %s must be after the earliest instant an execution for this proposal could exist (%s)",
				p.ExpiredAt.Format(time.RFC3339), p.EarliestFillAt.Format(time.RFC3339)))
		}
		// The stricter, next-bar-only rule: a stop-superseded expiry's
		// ExpiredAt is the closing fill's own timestamp, which can
		// legitimately fall inside the SAME bar that raised the proposal
		// (ADR 0005) — see ExpiredAt's own doc comment.
		if p.Reason == ExpiryReasonSupersededByNextBar && periodEndPresent && !p.ExpiredAt.After(p.PeriodEnd) {
			errs = append(errs, fmt.Errorf("expired at %s must be after the proposal's period end %s: a proposal is superseded by a later bar",
				p.ExpiredAt.Format(time.RFC3339), p.PeriodEnd.Format(time.RFC3339)))
		}
		// The weaker, non-strict rule for ExpiryReasonInputStreamEnded: its
		// own doc comment states ExpiredAt is "at the earliest the
		// proposal's own bar" — equal to PeriodEnd is fine (there is no
		// later bar to wait for), but before it states a stream ending
		// before a bar it already delivered.
		if p.Reason == ExpiryReasonInputStreamEnded && periodEndPresent && p.ExpiredAt.Before(p.PeriodEnd) {
			errs = append(errs, fmt.Errorf("expired at %s precedes the proposal's period end %s: a stream cannot end before a bar it already delivered",
				p.ExpiredAt.Format(time.RFC3339), p.PeriodEnd.Format(time.RFC3339)))
		}
	}
	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}
	switch p.Reason {
	case ExpiryReasonSupersededByNextBar:
		// recognised
	case ExpiryReasonInputStreamEnded:
		// Valid for every Kind, subject to the universal EarliestFillAt
		// rule above and to the non-strict PeriodEnd lower bound checked
		// with it: see the constant's own doc comment.
	case ExpiryReasonSupersededByStop:
		if p.Kind != ProposalKindAdd {
			errs = append(errs, fmt.Errorf("reason %q is only valid for kind %q (got %q): only an add proposal is cancelled by a stop fill", ExpiryReasonSupersededByStop, ProposalKindAdd, p.Kind))
		}
	case ExpiryReasonSupersededByDelisting:
		// Valid for every Kind, and subject only to the universal
		// EarliestFillAt rule above: see the constant's own doc comment.
	default:
		errs = append(errs, fmt.Errorf("reason %q is not a recognised expiry reason", p.Reason))
	}
	if p.Quantity <= 0 {
		errs = append(errs, fmt.Errorf("quantity must be a positive whole number, got %d", p.Quantity))
	}
	switch {
	case !isFinite(p.Level):
		errs = append(errs, errors.New("level must be finite"))
	case p.Level <= 0:
		errs = append(errs, errors.New("level must be positive"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid proposal expired payload: %w", err)
	}
	return nil
}
