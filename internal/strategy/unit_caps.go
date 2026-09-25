package strategy

import (
	"fmt"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds ADR 0008's Unit caps: at most 4 Units in one instrument, 6
// in one industry, 10 in one sector, and 12 long in total [T p.16], checked
// against POST-TRADE exposure — the Units that would be held after the
// proposed entry or Add — for every proposed entry and Add alike. A
// rejection names the specific cap that bound and the exposure that would
// have resulted (event.DeclineReasonUnitCapExceeded).
//
// Post-trade exposure is read from every OPEN Campaign's current state at
// the moment of the check (reducer.go's sizeUnit for an entry, campaign.go's
// evaluateAdd for an Add): the Units it already holds, plus the one Unit
// this proposal would add. ADR 0010 states that Unit-cap headroom, like
// cash, is "known at the previous close" and ADR 0020 — which amended that
// sentence for cash only — leaves it "unchanged" for caps. Neither ADR
// describes a running ledger for caps the way ADR 0020 built one for cash.
// So a Unit proposed earlier in the SAME session-close pass is not yet
// reflected here unless it has already been folded into instrumentState by
// an accepted fill: this function reads committed Campaign state, never
// other pending proposals from the same pass. Reserving cap headroom at
// proposal time, ranked against other proposals in the same pass, would be
// the "aggregate check with a new priority order" ADR 0020 explicitly
// rejects for the analogous cash case — see ADR 0008's own implementation
// note for the full reasoning and the ADR text it is read from.

// classification is the correlation group ADR 0008's industry and sector
// Unit caps count a Campaign against. It is captured once when a Campaign
// opens (openCampaign, campaign.go) and stored on campaignState; nothing
// ever re-reads classificationOf for an already-open Campaign. That is the
// same freeze-at-entry discipline ADR 0006 applies to N and the Unit size,
// applied here so that a later change to an instrument's classification
// cannot retroactively alter an already-open Campaign's cap grouping.
//
// Unclassified is true when no point-in-time industry/sector label exists
// for the instrument. CONTEXT.md's "Unclassified Group" then applies: every
// Unclassified instrument shares the one group, capped at the
// loosely-correlated level (event.ConfigurationPayload.MaxUnitsPerSector —
// ADR 0008 ties the Unclassified Group's cap to the sector level rather than
// declaring it separately configurable), and Industry/Sector are meaningless
// and left empty.
type classification struct {
	Unclassified bool
	Industry     string
	Sector       string
}

// unclassifiedClassification is the one classification value classificationOf
// can produce today (see its own doc comment).
var unclassifiedClassification = classification{Unclassified: true}

// classificationOf resolves instrumentID's point-in-time industry and sector
// classification for ADR 0008's caps.
//
// This is the seam ADR 0008 needs in place of a classification input event:
// no such input exists yet — the point-in-time universe and its labels are
// separate, future work — so today every instrument is Unclassified, and
// every Unclassified instrument shares CONTEXT.md's single Unclassified
// Group. When a classification input arrives, this is the one place that
// resolves it; capExceeded and openCampaign's freeze are already written in
// terms of its result and need no further change. Called at most once per
// Campaign (openCampaign, at the moment it opens) and once per entry
// decline check (sizeUnit); an open Campaign's own frozen classification
// field is what every later Add check reads, never a fresh call to this
// function.
func (r *transition) classificationOf(instrumentID string) classification {
	return unclassifiedClassification
}

// instrumentUnits returns the Units instrumentID's own open Campaign
// currently holds, or 0 if it has none.
func (r *transition) instrumentUnits(instrumentID string) int {
	state := r.peekInstrument(instrumentID)
	if state == nil || state.campaign == nil {
		return 0
	}
	return len(state.campaign.units)
}

// groupUnits sums the Units held across every open Campaign, over every
// instrument this transaction can see, whose classification matches. It
// reads with peekInstrument (docs/development.md: reducer transactions), so
// checking a cap never copies an instrument this transaction is not already
// proposing for.
func (r *transition) groupUnits(matches func(classification) bool) int {
	total := 0
	for _, id := range r.instrumentIDs() {
		state := r.peekInstrument(id)
		if state == nil || state.campaign == nil {
			continue
		}
		if matches(state.campaign.classification) {
			total += len(state.campaign.units)
		}
	}
	return total
}

// industryUnits sums Units across every open Campaign classified (not
// Unclassified) in industry.
func (r *transition) industryUnits(industry string) int {
	return r.groupUnits(func(c classification) bool {
		return !c.Unclassified && c.Industry == industry
	})
}

// sectorUnits sums Units across every open Campaign classified (not
// Unclassified) in sector.
func (r *transition) sectorUnits(sector string) int {
	return r.groupUnits(func(c classification) bool {
		return !c.Unclassified && c.Sector == sector
	})
}

// unclassifiedGroupUnits sums Units across every open Campaign in
// CONTEXT.md's single Unclassified Group.
func (r *transition) unclassifiedGroupUnits() int {
	return r.groupUnits(func(c classification) bool { return c.Unclassified })
}

// totalLongUnits sums Units across every open Campaign, of any instrument,
// industry, sector or classification. Every Campaign in this system is long
// (event.DirectionLong; long_only_test.go), so this is ADR 0008's total-long
// cap's own exposure figure without a direction filter.
func (r *transition) totalLongUnits() int {
	return r.groupUnits(func(classification) bool { return true })
}

// capExceeded reports the first of ADR 0008's Unit caps that ONE further
// Unit at classification c for instrumentID would exceed, given every open
// Campaign's CURRENT state (see this file's own doc comment on post-trade
// exposure and same-pass headroom). Caps are checked tightest correlation
// first — instrument, then the Unclassified Group or industry and sector,
// then total long [T p.16] — and capExceeded returns as soon as one binds,
// since a single decline already names one cap and one resulting exposure;
// a Unit that would breach more than one cap at once is not distinguished
// further; only the tightest, and therefore most informative, is reported.
//
// ok is false when no cap binds, in which case cap, limit and exposure are
// meaningless and must not be read.
func (r *transition) capExceeded(instrumentID string, c classification) (capName string, limit, exposure int, ok bool) {
	if exposure := r.instrumentUnits(instrumentID) + 1; exposure > r.maxUnits {
		return event.CapInstrument, r.maxUnits, exposure, true
	}
	if c.Unclassified {
		if exposure := r.unclassifiedGroupUnits() + 1; exposure > r.maxUnitsPerSector {
			return event.CapUnclassifiedGroup, r.maxUnitsPerSector, exposure, true
		}
	} else {
		if exposure := r.industryUnits(c.Industry) + 1; exposure > r.maxUnitsPerIndustry {
			return event.CapIndustry, r.maxUnitsPerIndustry, exposure, true
		}
		if exposure := r.sectorUnits(c.Sector) + 1; exposure > r.maxUnitsPerSector {
			return event.CapSector, r.maxUnitsPerSector, exposure, true
		}
	}
	if exposure := r.totalLongUnits() + 1; exposure > r.maxUnitsTotalLong {
		return event.CapTotalLong, r.maxUnitsTotalLong, exposure, true
	}
	return "", 0, 0, false
}

// capDetail renders the prose Detail a cap decline carries — the same
// "state the operands" discipline every other decline reason's Detail
// follows (sizeUnit, evaluateAdd).
func capDetail(capName string, limit, exposure int) string {
	return fmt.Sprintf("cap %q post-trade exposure %d exceeds the configured limit %d (ADR 0008)", capName, exposure, limit)
}
