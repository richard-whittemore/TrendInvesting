package event

import (
	"errors"
	"fmt"
	"time"
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
// Named "...set", not "...moved": this ticket (#12) only ever SETS a fresh
// stop, once, when a Campaign opens. #15's Stop Ladder will RAISE the stop
// as Units are added, and is designed to reuse this SAME event type and
// payload (see PreviousLevel below) rather than mint a second one — a
// consumer that groups a journal by event type then sees every stop
// movement of a Campaign's life in one place, first set and every later
// raise alike.
const ProtectiveStopSetEventType = "strategy.protective-stop.set"

// ProtectiveStopSetSchemaVersion is the current schema version of
// ProtectiveStopSetPayload, for the Envelope's SchemaVersion field.
const ProtectiveStopSetSchemaVersion uint32 = 1

// RuleProtectiveStopSetFromFill names the rule for
// ProtectiveStopSetPayload.Rule: a Campaign's first Protective Stop is set
// from its actual opening fill, not the intended entry level (ADR 0013
// measures every ladder from the slipped fill).
const RuleProtectiveStopSetFromFill = "protective-stop.set.from-fill"

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
// PreviousLevel is 0 on a Campaign's first Protective-Stop-set (there is no
// previous level to report) and is deliberately part of the payload from
// this ticket onward, ahead of #15's need for it: adding a field to a
// schema that already exists is a version bump every existing journal must
// upcast through, while a field present and validated from day one, even
// though every value produced today is 0, costs nothing and leaves #15
// nothing to migrate.
type ProtectiveStopSetPayload struct {
	CampaignID   string `json:"campaign_id"`
	InstrumentID string `json:"instrument_id"`
	// AsOf is when this stop came into force: the fill's timestamp on the
	// first set (matching CampaignOpenedPayload.OpenedAt), and — from #15
	// onward — the Add's fill timestamp on a later raise. Never a bar's
	// PeriodEnd: like the Campaign itself, a stop movement is caused by a
	// fill, not by a bar closing.
	AsOf time.Time `json:"as_of"`
	// Level is where the Protective Stop now sits. Validate re-derives it
	// exactly as EntryPrice - StopMultiple x CampaignN.
	Level float64 `json:"level"`
	// PreviousLevel is the stop level this one replaces, or 0 on a first set
	// (see the type's doc comment).
	PreviousLevel float64 `json:"previous_level"`
	// EntryPrice, CampaignN and StopMultiple are restated from the Campaign
	// (CampaignOpenedPayload carries the same three numbers) so Level is
	// independently re-derivable from this payload alone, without joining
	// back to the Campaign-opened event.
	EntryPrice   float64 `json:"entry_price"`
	CampaignN    float64 `json:"campaign_n"`
	StopMultiple float64 `json:"stop_multiple"`
	// Rule and ADR name the rule that produced this decision
	// (docs/development.md principle 3).
	Rule string `json:"rule"`
	ADR  string `json:"adr"`
}

// Validate checks that the payload identifies the Campaign and the moment
// the stop came into force, that every frozen number is usable, that Level
// is exactly EntryPrice - StopMultiple x CampaignN (the same exact-equality
// discipline CampaignOpenedPayload.Validate applies to ProtectiveStop, for
// the same reason: a tolerance would let a differently-derived stop
// through), that Level is positive and strictly below EntryPrice (a long
// position cannot be stopped out at or below zero), and that PreviousLevel
// is a legitimate value: 0 (a first set) or a positive level strictly below
// the new Level (the Baseline's Stop Ladder only ever raises a stop; #15 is
// not implemented by this ticket, so PreviousLevel is always 0 today, but
// the check is stated now rather than left to be added when a producer
// first needs it).
func (p ProtectiveStopSetPayload) Validate() error {
	var errs []error
	if p.CampaignID == "" {
		errs = append(errs, errors.New("campaign id is required"))
	}
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
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

	levelFinite := isFinite(p.Level)
	switch {
	case !levelFinite:
		errs = append(errs, errors.New("level must be finite"))
	case p.Level <= 0:
		errs = append(errs, errors.New("level must be positive: a long position cannot be stopped out at or below zero"))
	case entryPriceFinite && p.Level >= p.EntryPrice:
		errs = append(errs, fmt.Errorf("level %v must be below the entry price %v for a long position", p.Level, p.EntryPrice))
	}

	if entryPriceFinite && stopMultipleFinite && campaignNFinite && levelFinite {
		if derived := p.EntryPrice - p.StopMultiple*p.CampaignN; p.Level != derived {
			errs = append(errs, fmt.Errorf(
				"stated level %v does not match the derivation %v (entry price %v - stop multiple %v x campaign n %v)",
				p.Level, derived, p.EntryPrice, p.StopMultiple, p.CampaignN))
		}
	}

	previousLevelFinite := isFinite(p.PreviousLevel)
	switch {
	case !previousLevelFinite:
		errs = append(errs, errors.New("previous level must be finite"))
	case p.PreviousLevel < 0:
		errs = append(errs, errors.New("previous level must not be negative: zero means this is the campaign's first protective stop"))
	case p.PreviousLevel > 0 && levelFinite && p.PreviousLevel >= p.Level:
		errs = append(errs, fmt.Errorf(
			"previous level %v must be below the new level %v: the baseline's stop ladder only ever raises a campaign's stop",
			p.PreviousLevel, p.Level))
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid protective stop set payload: %w", err)
	}
	return nil
}
