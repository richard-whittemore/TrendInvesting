package event

import (
	"errors"
	"fmt"
	"time"
)

// WatchlistPublishedEventType identifies ADR 0011's Watchlist decision for
// the Envelope's Type field: the ranked set of every Setup a Session
// evaluated to Tier A or Tier B (CONTEXT.md: "Watchlist") — the pre-image of
// tomorrow's Signals and the artefact a reviewer reconciles a Signal's
// outcome against.
const WatchlistPublishedEventType = "strategy.watchlist.published"

// WatchlistPublishedSchemaVersion is the current schema version of
// WatchlistPublishedPayload, for the Envelope's SchemaVersion field.
const WatchlistPublishedSchemaVersion uint32 = 1

// RuleWatchlistRankedByStrength names the rule for
// WatchlistPublishedPayload.Rule: every Tier A and Tier B Setup a Session
// evaluated, ranked by Strength (CONTEXT.md: "Watchlist"; ADR 0011, decision
// 2).
const RuleWatchlistRankedByStrength = "watchlist.ranked-by-strength"

// ADRWatchlistRankedByStrength is the ADR WatchlistPublishedPayload.ADR
// cites: ADR 0011, which declares the Watchlist a first-class observable in
// every configuration, ranked by Strength, and states that in the Baseline
// it never gates a Tier A Setup's entry — only a Unit cap or the cash rule
// does that.
const ADRWatchlistRankedByStrength = "0011"

// WatchlistEntry is one Setup's own line on the Watchlist: its Tier
// (CONTEXT.md: "Tier" — TierA or TierB, never TierNone, which has no place
// on the Watchlist at all), its DistanceToEntryInN (SetupEvaluatedPayload's
// own field, carried again here so a reader never has to join back to that
// instrument's own Setup-evaluated event), and its Strength (ADR 0010, as
// amended by the owner's decision of 2026-09-25) — the key
// WatchlistPublishedPayload.Entries is ranked by, descending.
type WatchlistEntry struct {
	InstrumentID       string  `json:"instrument_id"`
	Tier               string  `json:"tier"`
	DistanceToEntryInN float64 `json:"distance_to_entry_in_n"`
	Strength           float64 `json:"strength"`
}

// WatchlistPublishedPayload carries ADR 0011's Watchlist for one Session:
// every Setup that Session evaluated to Tier A or Tier B, ranked by Strength
// descending — the same ranking rankSignals shares (internal/strategy,
// session.go). An instrument this Session could not rank at all
// (insufficient history: ADR 0010, as amended 2026-09-25) is left off the
// Watchlist entirely rather than ranked last, for the identical reason its
// Signal, if it had one, would be declined rather than ranked last (ADR
// 0011's own amendment, "Watchlist emission timing and unrankable Setups").
//
// This decision is OBSERVATIONAL ONLY in the Baseline (ADR 0011, decision
// 3): nothing in the reducer ever reads a Watchlist entry to decide whether
// a Tier A Setup becomes a Campaign — only a Unit cap or the cash rule does
// that (strategy.proposal.declined names the one that bound). A Session with
// no rankable Tier A or Tier B Setup publishes no Watchlist at all, rather
// than one with an empty Entries list — see Validate.
type WatchlistPublishedPayload struct {
	PeriodEnd time.Time        `json:"period_end"`
	Rule      string           `json:"rule"`
	ADR       string           `json:"adr"`
	Entries   []WatchlistEntry `json:"entries"`
}

// Validate checks that the payload identifies a period, names its rule and
// ADR, carries at least one entry (an empty Watchlist is never published,
// only omitted — see the type's own doc comment), that every entry
// identifies an instrument, with no instrument named twice, that Tier is
// TierA or TierB (never TierNone), that Tier and DistanceToEntryInN agree
// exactly as SetupEvaluatedPayload's own doc comment states (TierA strictly
// negative, TierB non-negative), that Strength is finite, and that Entries
// is sorted by Strength, descending. The total order's remaining tie-breaks
// (20-day median dollar volume, instrument ID; ADR 0010) are not carried on
// the wire, so only the primary key's own ordering is checked here.
func (p WatchlistPublishedPayload) Validate() error {
	var errs []error
	switch {
	case p.PeriodEnd.IsZero():
		errs = append(errs, errors.New("period end is required"))
	case !writableTime(p.PeriodEnd):
		errs = append(errs, errors.New("period end cannot be written as RFC 3339"))
	}
	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}
	if len(p.Entries) == 0 {
		errs = append(errs, errors.New("entries must not be empty: an empty watchlist is never published, only omitted"))
	}

	seen := make(map[string]bool, len(p.Entries))
	var previousStrength float64
	for i, e := range p.Entries {
		switch {
		case e.InstrumentID == "":
			errs = append(errs, fmt.Errorf("entries[%d]: instrument id is required", i))
		case seen[e.InstrumentID]:
			errs = append(errs, fmt.Errorf("entries[%d]: instrument %q appears more than once", i, e.InstrumentID))
		}
		seen[e.InstrumentID] = true

		distanceFinite := isFinite(e.DistanceToEntryInN)
		if !distanceFinite {
			errs = append(errs, fmt.Errorf("entries[%d]: distance to entry in n must be finite", i))
		}
		switch e.Tier {
		case TierA:
			if distanceFinite && !(e.DistanceToEntryInN < 0) {
				errs = append(errs, fmt.Errorf("entries[%d]: tier a requires a negative distance to entry in n (a breakout strictly exceeds the entry channel)", i))
			}
		case TierB:
			if distanceFinite && !(e.DistanceToEntryInN >= 0) {
				errs = append(errs, fmt.Errorf("entries[%d]: tier b requires a non-negative distance to entry in n", i))
			}
		default:
			errs = append(errs, fmt.Errorf("entries[%d]: tier %q is not tier a or tier b (tier none has no place on the watchlist)", i, e.Tier))
		}

		if !isFinite(e.Strength) {
			errs = append(errs, fmt.Errorf("entries[%d]: strength must be finite", i))
		} else if i > 0 && e.Strength > previousStrength {
			errs = append(errs, fmt.Errorf("entries[%d]: strength %v exceeds entries[%d]'s %v; entries must be sorted by strength, descending", i, e.Strength, i-1, previousStrength))
		}
		previousStrength = e.Strength
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid watchlist published payload: %w", err)
	}
	return nil
}
