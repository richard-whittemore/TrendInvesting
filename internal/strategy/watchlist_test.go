package strategy_test

import (
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds #35's tests: ADR 0011's Watchlist, the ranked set of every
// Setup a Session evaluates to Tier A or Tier B, sharing its ranking with
// rankSignals (session.go) and never gating a Baseline entry.

// watchlistTierBBar is a Setup within TierBDistanceInN of the Entry Channel
// high (155, breakoutFixtureHighs' own warm-up peak), built so inserting it
// changes nothing about N: its own True Range is exactly n
// (stableRampWilderValue()), the same Wilder fixed point breakoutBars' own
// preamble bars hold N at (WilderNext(n, n, period) == n exactly), so the
// eventual breakout bar's decision N — and everything sizeUnit derives from
// it — is unaffected by whether this bar preceded it.
//
// High = 100+x and Low = High-n (Close is 100, matching every other bar in
// this fixture family): True Range is max(High-Low, High-100, |Low-100|) =
// max(n, x, n-x), which equals n for any 0 <= x <= n. x = 30 sits inside
// both windows this bar must land in at once: [0, n] for that identity, and
// [55-n, 55] for Tier B's own distance test (0 <= (155-High)/n <= 1), given
// n approx 37.58 for x = 30: (155-130)/37.58 approx 0.665, safely inside
// [0, 1].
func watchlistTierBBar(instrumentID string, periodEnd time.Time) event.CompletedBarPayload {
	n := stableRampWilderValue()
	const x = 30.0
	high := 100 + x
	low := high - n
	return completedBar(instrumentID, periodEnd, high, low, 100)
}

// watchlistFixtureBars is breakoutBars with one Tier B Session inserted
// immediately before the breakout: the same 55-bar ramp and
// breakoutHistoryPreamble Sessions breakoutBars' own doc comment describes
// (so Strength and the dollar-volume tie-break are already computable, ADR
// 0010 as amended 2026-09-25), then watchlistTierBBar, then the unchanged
// breakout bar — timestamped one hour after the last preamble bar, still
// within day(55), so day(56)'s breakout is untouched.
func watchlistFixtureBars(instrumentID string) []event.CompletedBarPayload {
	bars := breakoutBars(instrumentID)
	tierBAt := day(55).Add(time.Duration(breakoutHistoryPreamble+1) * time.Hour)
	tierB := watchlistTierBBar(instrumentID, tierBAt)

	out := make([]event.CompletedBarPayload, 0, len(bars)+1)
	out = append(out, bars[:len(bars)-1]...)
	out = append(out, tierB, bars[len(bars)-1])
	return out
}

// TestWatchlistMovesASetupFromTierBToTierA is the ticket's first named case:
// a Setup moving B -> A. AAPL's Watchlist Session shows it at Tier B, and
// the very next Session — the breakout — shows it at Tier A, with nothing
// carried over: Tier B is memoryless (ADR 0011; CONTEXT.md: "Tier"), so each
// Watchlist is built solely from that Session's own Setup-evaluated
// decision.
func TestWatchlistMovesASetupFromTierBToTierA(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	bars := watchlistFixtureBars("AAPL")
	emitted := newStream(t, cfg).bars(bars).mustRun()

	watchlists := envelopesOfType(emitted, event.WatchlistPublishedEventType)
	if len(watchlists) != 2 {
		t.Fatalf("got %d Watchlist(s), want exactly 2: the Tier B Session and the breakout's Tier A Session (every earlier warm-up bar is Tier None and publishes none at all)", len(watchlists))
	}

	tierBWatchlist := decodeWatchlistPublished(t, watchlists[0])
	if len(tierBWatchlist.Entries) != 1 {
		t.Fatalf("Tier B Watchlist has %d entries, want 1", len(tierBWatchlist.Entries))
	}
	tierBEntry := tierBWatchlist.Entries[0]
	if tierBEntry.InstrumentID != "AAPL" || tierBEntry.Tier != event.TierB {
		t.Fatalf("Tier B Watchlist entry = %+v, want AAPL at Tier B", tierBEntry)
	}
	if tierBEntry.DistanceToEntryInN < 0 || tierBEntry.DistanceToEntryInN > cfg.TierBDistanceInN {
		t.Fatalf("Tier B Watchlist DistanceToEntryInN = %v, want within [0, %v]", tierBEntry.DistanceToEntryInN, cfg.TierBDistanceInN)
	}

	tierAWatchlist := decodeWatchlistPublished(t, watchlists[1])
	if len(tierAWatchlist.Entries) != 1 {
		t.Fatalf("Tier A Watchlist has %d entries, want 1", len(tierAWatchlist.Entries))
	}
	tierAEntry := tierAWatchlist.Entries[0]
	if tierAEntry.InstrumentID != "AAPL" || tierAEntry.Tier != event.TierA {
		t.Fatalf("Tier A Watchlist entry = %+v, want AAPL at Tier A", tierAEntry)
	}
	if tierAEntry.DistanceToEntryInN >= 0 {
		t.Fatalf("Tier A Watchlist DistanceToEntryInN = %v, want negative (a breakout)", tierAEntry.DistanceToEntryInN)
	}

	// The Tier B Session leaves no residue: the breakout still proposes
	// exactly one entry, sized identically to the same instrument's own
	// straight-to-Tier-A fixture (TestWatchlistEntryForASetupEnteringTierADirectly),
	// because watchlistTierBBar was built to leave N, and therefore
	// everything sizeUnit derives from it, unmoved.
	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("got %d trade proposal(s) after the Tier B Session, want exactly 1", len(proposals))
	}
	direct := decodeTradeProposal(t, onlyEnvelopeOfType(t, newStream(t, cfg).bars(breakoutBars("AAPL")).mustRun(), event.TradeProposalEventType))
	viaTierB := decodeTradeProposal(t, proposals[0])
	if direct != viaTierB {
		t.Fatalf("the entry proposal differs depending on whether AAPL passed through Tier B first:\n direct:     %+v\n via Tier B: %+v", direct, viaTierB)
	}
}

// TestWatchlistEntryForASetupEnteringTierADirectly is the ticket's second
// named case: a Setup can enter Tier A directly, without ever having been
// Tier B.
func TestWatchlistEntryForASetupEnteringTierADirectly(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	bars := breakoutBars("MSFT")
	emitted := newStream(t, cfg).bars(bars).mustRun()

	watchlists := envelopesOfType(emitted, event.WatchlistPublishedEventType)
	if len(watchlists) != 1 {
		t.Fatalf("got %d Watchlist(s), want exactly 1 (the breakout Session): MSFT never touched Tier B", len(watchlists))
	}
	watchlist := decodeWatchlistPublished(t, watchlists[0])
	if len(watchlist.Entries) != 1 {
		t.Fatalf("Watchlist has %d entries, want 1", len(watchlist.Entries))
	}
	entry := watchlist.Entries[0]
	if entry.InstrumentID != "MSFT" || entry.Tier != event.TierA {
		t.Fatalf("Watchlist entry = %+v, want MSFT at Tier A", entry)
	}
	if entry.DistanceToEntryInN >= 0 {
		t.Fatalf("DistanceToEntryInN = %v, want negative (a breakout)", entry.DistanceToEntryInN)
	}
	// Every bar in this fixture family closes flat at 100 (syntheticBar's own
	// doc comment), so Strength — (close(d)-close(d-63))/N(d) — is exactly
	// zero: a legitimate, deterministic value, not a sign that ranking
	// silently skipped this entry.
	if entry.Strength != 0 {
		t.Fatalf("Strength = %v, want exactly 0 (every close in this fixture is flat at 100)", entry.Strength)
	}
	if watchlist.Rule != event.RuleWatchlistRankedByStrength || watchlist.ADR != event.ADRWatchlistRankedByStrength {
		t.Fatalf("Rule/ADR = %q/%q, want %q/%q", watchlist.Rule, watchlist.ADR, event.RuleWatchlistRankedByStrength, event.ADRWatchlistRankedByStrength)
	}
}

// TestWatchlistPersistsThroughACapDecline is the ticket's third named case: a
// Tier A Setup blocked by a cap still appears on the Watchlist — proving the
// Watchlist is observability, never a filter (ADR 0011, decision 3) — and
// the journal separately names the binding cap in the decline.
func TestWatchlistPersistsThroughACapDecline(t *testing.T) {
	t.Parallel()

	// Instrument and group caps generous; only the total-long cap, set to 1,
	// is under test — the identical fixture unit_caps_test.go's own
	// TestTwoEntriesInOneSessionCloseShareTheTotalLongCap uses.
	cfg := compactChannelConfig(1_000_000, 1_000_000, 1_000_000, 1)
	emitted := newStream(t, cfg).lockstep(compactEntryBars("MMM", 0), compactEntryBars("NNN", 0)).mustRun()

	watchlist := decodeWatchlistPublished(t, onlyEnvelopeOfType(t, emitted, event.WatchlistPublishedEventType))
	if len(watchlist.Entries) != 2 {
		t.Fatalf("Watchlist has %d entries, want 2: the cap decides which Signal is FUNDED, never which Setup is OBSERVED", len(watchlist.Entries))
	}
	byID := make(map[string]event.WatchlistEntry, 2)
	for _, e := range watchlist.Entries {
		byID[e.InstrumentID] = e
	}
	for _, id := range []string{"MMM", "NNN"} {
		entry, ok := byID[id]
		if !ok || entry.Tier != event.TierA {
			t.Fatalf("Watchlist entries = %+v, want both MMM and NNN listed at Tier A regardless of the cap", watchlist.Entries)
		}
	}

	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 || decodeTradeProposal(t, proposals[0]).InstrumentID != "MMM" {
		t.Fatalf("got %d trade proposal(s), want exactly 1, for MMM", len(proposals))
	}
	decline := decodeProposalDeclined(t, onlyEnvelopeOfType(t, emitted, event.ProposalDeclinedEventType))
	if decline.InstrumentID != "NNN" || decline.Reason != event.DeclineReasonUnitCapExceeded || decline.Cap != event.CapTotalLong {
		t.Fatalf("decline = %+v, want NNN declined for the %s cap, named in the journal", decline, event.CapTotalLong)
	}
}

// TestATierASetupNotEnteredDoesNotReappearOnTheWatchlistWithoutRequalifying
// is the ticket's fourth named case: a Tier A Setup that is not entered does
// not reappear as a live Signal — or on the Watchlist — the next bar unless
// it re-qualifies (ADR 0011, decision 4).
func TestATierASetupNotEnteredDoesNotReappearOnTheWatchlistWithoutRequalifying(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	bars := breakoutBars("AAPL")
	// quietBar's own doc comment: well under the Entry Channel and further
	// than TierBDistanceInN away from it, so it is neither Tier A nor Tier B
	// — no fill was ever supplied for the breakout's own proposal either.
	emitted := newStream(t, cfg).bars(bars).bar(quietBar("AAPL")).mustRun()

	if got := len(envelopesOfType(emitted, event.CampaignOpenedEventType)); got != 0 {
		t.Fatalf("got %d Campaign(s), want 0: no fill arrived", got)
	}
	watchlists := envelopesOfType(emitted, event.WatchlistPublishedEventType)
	if len(watchlists) != 1 {
		t.Fatalf("got %d Watchlist(s), want exactly 1 (only the breakout Session): AAPL does not requalify for either Tier the next Session, so it does not reappear", len(watchlists))
	}
	only := decodeWatchlistPublished(t, watchlists[0])
	if len(only.Entries) != 1 || only.Entries[0].InstrumentID != "AAPL" || only.Entries[0].Tier != event.TierA {
		t.Fatalf("the one Watchlist = %+v, want AAPL's breakout Session alone", only.Entries)
	}
}

// TestWatchlistOmitsAnInstrumentItCannotRank proves ADR 0011's own amendment
// ("Watchlist emission timing and unrankable Setups"): a Setup this Session
// cannot rank at all (fewer than indicator.StrengthLookbackBars+1
// split-adjusted closes) is left off the Watchlist entirely, and a Session
// whose only candidate is unrankable publishes no Watchlist at all — the
// identical fixture TestReducerEmitsExactlyOneSignalOnBreakoutBar already
// uses, which is unmodified by #35 for exactly this reason.
func TestWatchlistOmitsAnInstrumentItCannotRank(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	highs := breakoutFixtureHighs() // 56 closes: short of the 64 Strength needs
	emitted := runReducerOverHighs(t, "AAPL", highs, cfg)

	if got := len(envelopesOfType(emitted, event.SignalEventType)); got != 1 {
		t.Fatalf("got %d Signal(s), want exactly 1: the Tier A Signal still fires", got)
	}
	if got := len(envelopesOfType(emitted, event.WatchlistPublishedEventType)); got != 0 {
		t.Fatalf("got %d Watchlist(s), want 0: AAPL is the Session's only Tier A/B Setup and cannot be ranked, so nothing is left to publish", got)
	}
	if got := len(envelopesOfType(emitted, event.ProposalDeclinedEventType)); got != 1 {
		t.Fatalf("got %d decline(s), want exactly 1 (insufficient history)", got)
	}
}

// TestWatchlistEntriesAreRankedByStrengthDescending checks the Watchlist's
// own ranking, sharing rankSignals with the entries decided immediately
// after it (session.go). AAA and ZZZ are simultaneous Tier A breakouts;
// dollarVolumeA is built strictly higher than dollarVolumeZ's own median
// dollar volume so the tie-break, not Strength (both closes are flat at 100,
// so both Strengths are exactly zero), decides the order — the identical
// mechanism TestTotalLongCapBindsOnDollarVolumeWhenStrengthTies (unit_caps_test.go)
// already relies on for rankSignals itself.
func TestWatchlistEntriesAreRankedByStrengthDescending(t *testing.T) {
	t.Parallel()

	cfg := compactChannelConfig(1_000_000, 1_000_000, 1_000_000, 1_000_000)
	lowVolumeBars := compactEntryBars("AAA", 0)
	highVolumeBars := compactEntryBars("ZZZ", 0)
	for i := range highVolumeBars {
		highVolumeBars[i].Raw.Volume *= 10
	}
	emitted := newStream(t, cfg).lockstep(lowVolumeBars, highVolumeBars).mustRun()

	watchlist := decodeWatchlistPublished(t, onlyEnvelopeOfType(t, emitted, event.WatchlistPublishedEventType))
	if len(watchlist.Entries) != 2 {
		t.Fatalf("Watchlist has %d entries, want 2", len(watchlist.Entries))
	}
	if watchlist.Entries[0].InstrumentID != "ZZZ" || watchlist.Entries[1].InstrumentID != "AAA" {
		t.Fatalf("Watchlist order = [%s, %s], want ZZZ then AAA (higher median dollar volume, Strength tied at zero)", watchlist.Entries[0].InstrumentID, watchlist.Entries[1].InstrumentID)
	}
	if watchlist.Entries[0].Strength != watchlist.Entries[1].Strength {
		t.Fatalf("Strengths = %v, %v, want tied at zero (every close in this fixture is flat at 100)", watchlist.Entries[0].Strength, watchlist.Entries[1].Strength)
	}
}

// TestWatchlistPublishedPayloadFromReducerValidates is a belt-and-braces
// event-seam check that a real Watchlist decision decodes and Validates
// cleanly, matching every other decision's own such check in this package.
func TestWatchlistPublishedPayloadFromReducerValidates(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	emitted := newStream(t, cfg).bars(breakoutBars("AAPL")).mustRun()
	watchlist := decodeWatchlistPublished(t, onlyEnvelopeOfType(t, emitted, event.WatchlistPublishedEventType))
	if err := watchlist.Validate(); err != nil {
		t.Fatalf("Watchlist fails its own Validate(): %v", err)
	}
	if !watchlist.PeriodEnd.Equal(day(56)) {
		t.Fatalf("PeriodEnd = %v, want %v", watchlist.PeriodEnd, day(56))
	}
}
