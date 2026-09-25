package strategy

import (
	"encoding/json"
	"fmt"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds ADR 0011's Watchlist: the ranked set of every Setup a
// Session evaluated to Tier A or Tier B, ranked by Strength — the same
// ranking rankSignals shares (session.go) — and observational only in the
// Baseline (ADR 0011, decision 3): nothing here ever gates a Tier A Setup's
// entry, which only a Unit cap or the cash rule may do (session.go,
// sizeUnit).

// watchlistCandidate pairs one instrument's ranking figures (signalRanking,
// session.go) with the Tier and DistanceToEntryInN its own Setup-evaluated
// decision carried this Session (instrumentState.lastSetupTier/
// lastSetupDistanceToEntryInN).
type watchlistCandidate struct {
	ranking  signalRanking
	tier     string
	distance float64
}

// emitWatchlist builds and stamps ADR 0011's Watchlist decision for the
// Session that is closing: every instrument in live whose bar THIS Session
// evaluated to Tier A or Tier B, ranked by Strength via rankSignal/
// rankSignals — the identical ranking seam ADR 0010's Signal ranking shares,
// so the Watchlist and tomorrow's entries are never ranked two different
// ways.
//
// "This Session" is checked by matching instrumentState.lastSetupPeriodEnd
// against r.sessionPeriodEnd: Tier B is memoryless (ADR 0011; CONTEXT.md:
// "Tier"), so an instrument whose most recent Setup-evaluated decision
// belongs to an earlier Session — because a Campaign opened for it this
// Session and no Setup-evaluated event fired at all — is silently excluded,
// never carried forward.
//
// An instrument rankSignal cannot rank at all (insufficient split-adjusted
// close or raw dollar-volume history) has no place in the Watchlist's total
// order, for the identical reason it is declined rather than ranked last as
// a Signal (ADR 0010, as amended 2026-09-25) — see ADR 0011's own amendment,
// "Watchlist emission timing and unrankable Setups".
//
// It returns no emission at all, rather than one with an empty Entries list,
// when no live instrument qualifies: nothing else this reducer emits states
// an empty per-Session fact (there is no "zero Adds this Session" event
// either), and WatchlistPublishedPayload.Validate refuses an empty Entries
// list for the same reason.
//
// This is read-only: it neither sets nor clears anything evaluateAdd,
// sizeUnit or a cap check reads, so nothing about the Adds and entries
// decided immediately after it can depend on what it found (ADR 0011,
// decision 3).
func (r *transition) emitWatchlist(live []string, input event.Envelope) ([]event.Envelope, error) {
	var candidates []watchlistCandidate
	for _, id := range live {
		state := r.peekInstrument(id)
		if state == nil || state.lastSetupTier == event.TierNone {
			continue
		}
		if !state.lastSetupPeriodEnd.Equal(r.sessionPeriodEnd) {
			continue
		}
		ranking, _, ranked := r.rankSignal(id)
		if !ranked {
			continue
		}
		candidates = append(candidates, watchlistCandidate{
			ranking:  ranking,
			tier:     state.lastSetupTier,
			distance: state.lastSetupDistanceToEntryInN,
		})
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	rankings := make([]signalRanking, len(candidates))
	byID := make(map[string]watchlistCandidate, len(candidates))
	for i, c := range candidates {
		rankings[i] = c.ranking
		byID[c.ranking.instrumentID] = c
	}

	entries := make([]event.WatchlistEntry, 0, len(candidates))
	for _, id := range rankSignals(rankings) {
		c := byID[id]
		entries = append(entries, event.WatchlistEntry{
			InstrumentID:       id,
			Tier:               c.tier,
			DistanceToEntryInN: c.distance,
			Strength:           c.ranking.strength,
		})
	}

	payload := event.WatchlistPublishedPayload{
		PeriodEnd: r.sessionPeriodEnd,
		Rule:      event.RuleWatchlistRankedByStrength,
		ADR:       event.ADRWatchlistRankedByStrength,
		Entries:   entries,
	}
	if err := payload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: built invalid watchlist published payload: %w", err)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("strategy: marshal watchlist published payload: %w", err)
	}

	// "session", never an instrument id: this decision is scoped to the
	// whole Session, exactly one per Session, unlike every other decisionID
	// call site in this package.
	envelope := r.stamp(
		decisionID("watchlist", "session", r.sessionPeriodEnd),
		event.WatchlistPublishedEventType, event.WatchlistPublishedSchemaVersion,
		r.sessionPeriodEnd, input, payloadBytes,
	)
	return []event.Envelope{envelope}, nil
}
