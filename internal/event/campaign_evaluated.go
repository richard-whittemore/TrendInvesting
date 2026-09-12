package event

import (
	"errors"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// CampaignEvaluatedEventType identifies the per-bar Campaign decision payload
// for the Envelope's Type field: while an instrument is in an open Campaign,
// this is the one decision event a completed bar produces for it.
//
// CONTEXT.md defines a Setup as an Eligible instrument NOT in a Campaign, so
// an instrument already in a Campaign gets no Setup-evaluated event — but
// that left a bar in a Campaign emitting nothing at all except the invariant
// check. This event fills that hole: the levels in force on this bar — the
// Protective Stop and the Exit Channel low — are journaled every bar a
// Campaign is open, whether or not either one triggers anything.
const CampaignEvaluatedEventType = "strategy.campaign.evaluated"

// CampaignEvaluatedSchemaVersion is the current schema version of
// CampaignEvaluatedPayload, for the Envelope's SchemaVersion field.
//
// Version 2 added Units, DollarsPerPoint, AggregateOpenRisk, NotionalAccount
// and AggregateOpenRiskFraction, all required. A version-1 record decodes
// Units as a nil (empty) slice, which is not a legitimate value under the
// new requirement — ProtectiveStop's own re-derivation depends on it being
// non-empty — so a version-1 record is rejected outright rather than
// silently read as a Campaign with no Units (ADR 0015's rule).
const CampaignEvaluatedSchemaVersion uint32 = 2

// CampaignEvaluatedUnit is one held Unit's own facts as reported on a
// per-bar CampaignEvaluatedPayload: its identity (UnitIndex), its own entry,
// its own quantity, and its own CURRENT Protective Stop — which, once the
// Stop Ladder (The Turtle Rules p.22-23) has raised some Units and not
// others (the gap case, p.23), may genuinely differ from every other Unit's
// own level. CampaignEvaluatedPayload also reports the Campaign-wide minimum
// (ProtectiveStop, still carried below); this list is what lets a journal
// reader see every level actually in force, not only the tightest one.
type CampaignEvaluatedUnit struct {
	UnitIndex      int     `json:"unit_index"`
	EntryPrice     float64 `json:"entry_price"`
	Quantity       int64   `json:"quantity"`
	ProtectiveStop float64 `json:"protective_stop"`
}

// CampaignEvaluatedPayload carries the outcome of evaluating one open
// Campaign on one completed bar: the Protective Stop currently in force, the
// Exit Channel low computed from the preceding ExitChannelLength completed
// bars (CONTEXT.md: "Exit Channel"; The Turtle Rules p.26, ADR 0002) and
// whether it is warmed up, and whether this bar's low breached it.
//
// ExitChannelLow is "the Exit Channel low in force for deciding this bar":
// computed from the completed bars PRECEDING it, never including this bar's
// own low — the same look-ahead discipline SetupEvaluatedPayload.N and
// EntryChannelHigh already document, mirrored on the exit side (see
// indicator.ExitChannel's doc comment).
//
// ExitChannelReady means at least ExitChannelLength completed bars have been
// added to the Exit Channel window. This can be false only if the Campaign
// opened very early in an instrument's history (fewer than ExitChannelLength
// completed bars existed before it did) — the window is fed every completed
// bar regardless of Campaign state, specifically so it is warm the moment a
// Campaign opens under ordinary circumstances. ExitChannelLow is 0 while not
// ready, the same zero convention EntryChannelHigh uses, so a consumer
// reading a level without checking readiness first sees an unambiguous "not
// evaluable" value rather than a stale one; Validate enforces both
// directions of that convention.
//
// ExitConditionMet is true only when ExitChannelReady is true AND this bar's
// low fell strictly below ExitChannelLow (The Turtle Rules p.26: price
// "falls below" the channel, mirroring p.19's "exceeds" for the Entry
// Channel — a tie is not a breach). Validate rejects ExitConditionMet==true
// while ExitChannelReady==false: an unready channel has no real level to
// have fallen below.
type CampaignEvaluatedPayload struct {
	CampaignID   string    `json:"campaign_id"`
	InstrumentID string    `json:"instrument_id"`
	PeriodEnd    time.Time `json:"period_end"`
	// ProtectiveStop is the Campaign's Protective Stop as it stands on this
	// bar (CONTEXT.md: "Every open Campaign has one at all times") — the
	// MINIMUM across every listed Unit's own ProtectiveStop (Validate checks
	// the two agree exactly): the first level that would trigger. Kept
	// alongside Units rather than replaced by it: a consumer that only cares
	// "is the Campaign still protected, and at what worst-case level" reads
	// this one field, without summarising Units itself.
	ProtectiveStop float64 `json:"protective_stop"`
	// Units lists every held Unit's own entry, quantity and CURRENT
	// Protective Stop. Before the Stop Ladder ever raises a stop unevenly
	// (the gap case, The Turtle Rules p.23), every Unit's own level
	// coincides and this list is redundant with ProtectiveStop; once levels
	// diverge, this is the only place a journal reader sees every level
	// actually in force. Always non-empty: an open Campaign always holds at
	// least Unit 1.
	Units []CampaignEvaluatedUnit `json:"units"`
	// ExitChannelLow and ExitChannelReady are the Exit Channel's own reading
	// (see the type's doc comment).
	ExitChannelLow   float64 `json:"exit_channel_low"`
	ExitChannelReady bool    `json:"exit_channel_ready"`
	// ExitConditionMet is whether this bar's low breached the Exit Channel
	// (see the type's doc comment). When true, the same bar also emits an
	// ExitProposalPayload naming the same CampaignID and Level.
	ExitConditionMet bool `json:"exit_condition_met"`
	// DollarsPerPoint is the instrument's contract multiplier (1 for
	// shares), restated so AggregateOpenRisk is independently re-derivable
	// from Units alone (see the type's doc comment on AggregateOpenRisk).
	DollarsPerPoint float64 `json:"dollars_per_point"`
	// AggregateOpenRisk is the Campaign's aggregate open risk
	// (.greptile/rules.md's "risk multiplication when pyramiding" failure
	// mode, fixed by construction): the sum, over every listed Unit, of
	// (EntryPrice - ProtectiveStop) x Quantity x DollarsPerPoint —
	// internal/sizing.AggregateOpenRisk, called identically by the producer
	// and by Validate below, so the two cannot silently disagree about which
	// figure is "the" aggregate. Computed
	// from EACH Unit's own entry and OWN current stop, never assumed
	// uniform across Units — the prototype's own risk-multiplication bug,
	// closed by this re-derivation: a payload that claims a small (correct,
	// "laddered") aggregate while its own Units carry the inflated
	// ("fresh stop per Unit") figures fails Validate.
	AggregateOpenRisk float64 `json:"aggregate_open_risk"`
	// NotionalAccount is the Notional Account figure (ADR 0007) in force
	// when this bar was evaluated — internal/strategy.NotionalAccount.Current(),
	// never actual account equity (see that type's own doc comment) —
	// restated so AggregateOpenRiskFraction is independently re-derivable.
	NotionalAccount float64 `json:"notional_account"`
	// AggregateOpenRiskFraction is AggregateOpenRisk divided by
	// NotionalAccount: the fraction of the account this Campaign has at
	// risk right now, across every held Unit.
	AggregateOpenRiskFraction float64 `json:"aggregate_open_risk_fraction"`
}

// Validate checks that the payload identifies the Campaign, instrument and
// period, that Units is non-empty with strictly ascending positive indexes
// and each Unit's own entry and stop are usable (entry positive, stop
// positive — a RAISED stop may sit at or above its own entry, a legitimate
// break-even or profit-protecting level under a narrow-enough Stop
// Multiple; see the loop's own comment below), that ProtectiveStop is
// EXACTLY the minimum across Units' own ProtectiveStop, that ExitChannelLow
// is never negative and matches the zero-while-not-ready convention
// EntryChannelHigh already uses, that ExitConditionMet is never true while
// the channel is not ready, that DollarsPerPoint and NotionalAccount are
// usable, and that AggregateOpenRisk and AggregateOpenRiskFraction each
// match their derivation from Units, DollarsPerPoint and NotionalAccount
// EXACTLY (the same exact-equality discipline every derived field in this
// package uses). AggregateOpenRisk is re-derived by calling
// internal/sizing.AggregateOpenRisk rather than re-typing the summation
// here — the same discipline CampaignExitedPayload.Validate already follows
// for sizing.AverageMoveInN/RealisedResultInUnitN.
func (p CampaignEvaluatedPayload) Validate() error {
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

	switch {
	case !isFinite(p.ProtectiveStop):
		errs = append(errs, errors.New("protective stop must be finite"))
	case p.ProtectiveStop <= 0:
		errs = append(errs, errors.New("protective stop must be positive"))
	}

	if len(p.Units) == 0 {
		errs = append(errs, errors.New("units is required: an open campaign always holds at least unit 1"))
	}
	unitsUsable := len(p.Units) > 0
	minStop := 0.0
	haveMinStop := false
	for i, u := range p.Units {
		if i > 0 && p.Units[i-1].UnitIndex >= u.UnitIndex {
			errs = append(errs, fmt.Errorf("units must have strictly ascending unit indexes with no duplicates, got %d at position %d after %d", u.UnitIndex, i, p.Units[i-1].UnitIndex))
			unitsUsable = false
		}
		if u.UnitIndex < 1 {
			errs = append(errs, fmt.Errorf("units[%d].unit_index = %d, must be at least 1", i, u.UnitIndex))
			unitsUsable = false
		}
		entryFinite := isFinite(u.EntryPrice)
		switch {
		case !entryFinite:
			errs = append(errs, fmt.Errorf("units[%d]: entry price must be finite", i))
			unitsUsable = false
		case u.EntryPrice <= 0:
			errs = append(errs, fmt.Errorf("units[%d]: entry price must be positive", i))
			unitsUsable = false
		}
		// A Unit's protective stop must be positive and finite, but is NOT
		// required to sit below its own entry price: repeated half-N raises
		// (the Stop Ladder) can lift an
		// earlier Unit's stop to or above its own entry under a Variant
		// with a narrower Stop Multiple (e.g. StopMultiple 1 with four
		// Units — the Baseline's 2N stop and four-Unit maximum never reach
		// this, since the maximum raise is 1.5N). A stop at or above entry
		// is a legitimate break-even or profit-protecting level (CONTEXT.md:
		// a Unit in that state is "risk-free"), not a corrupted or
		// mis-derived one, and its contribution to AggregateOpenRisk is
		// simply zero (sizing.AggregateOpenRisk's own doc comment) rather
		// than a validation failure. Only a Unit's INITIAL stop (Reason
		// ProtectiveStopReasonInitial on ProtectiveStopSetPayload) is still
		// required strictly below entry — a stop can only ever REACH entry
		// by rising from there.
		stopFinite := isFinite(u.ProtectiveStop)
		switch {
		case !stopFinite:
			errs = append(errs, fmt.Errorf("units[%d]: protective stop must be finite", i))
			unitsUsable = false
		case u.ProtectiveStop <= 0:
			errs = append(errs, fmt.Errorf("units[%d]: protective stop must be positive", i))
			unitsUsable = false
		}
		if u.Quantity <= 0 {
			errs = append(errs, fmt.Errorf("units[%d]: quantity must be a positive whole number, got %d", i, u.Quantity))
			unitsUsable = false
		}
		if stopFinite && u.ProtectiveStop > 0 && (!haveMinStop || u.ProtectiveStop < minStop) {
			minStop = u.ProtectiveStop
			haveMinStop = true
		}
	}
	if haveMinStop && isFinite(p.ProtectiveStop) && p.ProtectiveStop != minStop {
		errs = append(errs, fmt.Errorf("protective stop %v does not equal the minimum across units' own protective stops %v", p.ProtectiveStop, minStop))
	}

	switch {
	case !isFinite(p.ExitChannelLow):
		errs = append(errs, errors.New("exit channel low must be finite"))
	case p.ExitChannelLow < 0:
		errs = append(errs, errors.New("exit channel low must not be negative"))
	case p.ExitChannelReady && p.ExitChannelLow <= 0:
		errs = append(errs, errors.New("exit channel low must be positive when ready"))
	case !p.ExitChannelReady && p.ExitChannelLow != 0:
		errs = append(errs, errors.New("exit channel low must be zero while not ready"))
	}

	if !p.ExitChannelReady && p.ExitConditionMet {
		errs = append(errs, errors.New("exit condition met must be false while the exit channel is not ready"))
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

	aggregateFinite := isFinite(p.AggregateOpenRisk)
	if !aggregateFinite {
		errs = append(errs, errors.New("aggregate open risk must be finite"))
	}
	if unitsUsable && dollarsPerPointFinite && aggregateFinite {
		sizingUnits := make([]sizing.UnitOpenRisk, len(p.Units))
		for i, u := range p.Units {
			sizingUnits[i] = sizing.UnitOpenRisk{EntryPrice: u.EntryPrice, ProtectiveStop: u.ProtectiveStop, Quantity: u.Quantity}
		}
		if derived, err := sizing.AggregateOpenRisk(sizingUnits, p.DollarsPerPoint); err == nil && p.AggregateOpenRisk != derived {
			errs = append(errs, fmt.Errorf(
				"stated aggregate open risk %v does not match the derivation %v (sum over units of (entry price - protective stop) x quantity x dollars per point)",
				p.AggregateOpenRisk, derived))
		}
	}

	fractionFinite := isFinite(p.AggregateOpenRiskFraction)
	if !fractionFinite {
		errs = append(errs, errors.New("aggregate open risk fraction must be finite"))
	}
	if aggregateFinite && notionalAccountFinite && fractionFinite {
		if derived := p.AggregateOpenRisk / p.NotionalAccount; p.AggregateOpenRiskFraction != derived {
			errs = append(errs, fmt.Errorf(
				"stated aggregate open risk fraction %v does not match the derivation %v (aggregate open risk %v / notional account %v)",
				p.AggregateOpenRiskFraction, derived, p.AggregateOpenRisk, p.NotionalAccount))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid campaign evaluated payload: %w", err)
	}
	return nil
}
