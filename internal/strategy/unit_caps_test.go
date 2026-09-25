package strategy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/indicator"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// This file holds #36's event-seam tests: ADR 0008's four Unit caps —
// instrument, industry, sector, total long — plus CONTEXT.md's Unclassified
// Group, checked against POST-TRADE exposure for both entries and Adds, with
// every rejection naming the cap that bound and the exposure that would have
// resulted.
//
// Industry and sector are unreachable through this event seam today: no
// classification input exists yet (#37/#41), so classificationOf
// (unit_caps.go) always reports Unclassified, and every Unclassified
// instrument shares CONTEXT.md's single Unclassified Group. The industry and
// sector arithmetic itself is covered white-box, bypassing that seam
// directly, in unit_caps_internal_test.go.

// capsConfig is validConfigurationPayload with all four Unit caps replaced
// by small, deliberately different numbers so each can bind on its own —
// never the ADR 0008 Baseline defaults (4/6/10/12), which #55's own tests
// already cover.
func capsConfig(maxUnits, maxUnitsPerIndustry, maxUnitsPerSector, maxUnitsTotalLong int) event.ConfigurationPayload {
	cfg := validConfigurationPayload()
	cfg.MaxUnits = maxUnits
	cfg.MaxUnitsPerIndustry = maxUnitsPerIndustry
	cfg.MaxUnitsPerSector = maxUnitsPerSector
	cfg.MaxUnitsTotalLong = maxUnitsTotalLong
	return cfg
}

// compactChannelConfig is capsConfig with the Entry and Exit Channel lengths
// shortened to match compactFixtureHighs' own, shorter warm-up
// (indicator.DefaultPeriod bars rather than validConfigurationPayload's 55):
// every multi-instrument fixture in this file uses compactEntryBars, so its
// configuration must agree with the fixture's own warm-up length or the
// Entry Channel is simply never ready.
func compactChannelConfig(maxUnits, maxUnitsPerIndustry, maxUnitsPerSector, maxUnitsTotalLong int) event.ConfigurationPayload {
	cfg := capsConfig(maxUnits, maxUnitsPerIndustry, maxUnitsPerSector, maxUnitsTotalLong)
	cfg.EntryChannelLength = indicator.DefaultPeriod
	cfg.ExitChannelLength = indicator.DefaultPeriod / 2
	return cfg
}

// compactFixtureHighs is breakoutFixtureHighs scaled down to
// indicator.DefaultPeriod (20) warm-up bars instead of 55: N and the Entry
// Channel both warm at bar 20, so a fixture built from this is ready one bar
// sooner. Used only by this file's multi-instrument fixtures, where several
// instruments each need their own warm-up and a 55-bar one would make the
// fixtures unwieldy without testing anything the shorter one does not.
func compactFixtureHighs() []float64 {
	highs := make([]float64, 0, indicator.DefaultPeriod+1)
	for i := 1; i <= indicator.DefaultPeriod; i++ {
		highs = append(highs, 100+float64(i))
	}
	// Just above the warmed-up channel high (100+20=120), not far above it:
	// a Signal only needs to clear the channel, and clearing it by a wide
	// margin would also reach the SAME bar's first Add rung (campaign.go's
	// same-bar chain, "openCampaign ... the bar that opens a Campaign can
	// also cover Unit 2's rung"), which is not what a plain entry fixture
	// is testing here.
	return append(highs, 121)
}

// compactEntryBars is compactFixtureHighs as bars for instrumentID, starting
// at day(startDay+1) and ending at day(startDay+21) — the same shape
// breakoutBars gives at startDay 0 (CONTEXT.md's evaluate-then-advance
// discipline needs no more), scaled down and made relocatable so several
// instruments can each warm up in their own, non-overlapping run of
// Sessions (ADR 0021: Sessions follow one another strictly).
func compactEntryBars(instrumentID string, startDay int) []event.CompletedBarPayload {
	highs := compactFixtureHighs()
	bars := make([]event.CompletedBarPayload, 0, len(highs))
	for i, high := range highs {
		bars = append(bars, syntheticBar(instrumentID, day(startDay+i+1), high-100))
	}
	return bars
}

// compactEntryFill is openingFill generalised to any instrument, period end,
// quantity, fill id and price — what every instrument in this file's
// multi-instrument fixtures fills its opening Unit at.
func compactEntryFill(instrumentID string, periodEnd time.Time, fillID string, quantity int64, price float64) event.FillPayload {
	return event.FillPayload{
		InstrumentID: instrumentID,
		Kind:         event.FillKindEntry,
		ProposalID:   testDecisionID("proposal", instrumentID, periodEnd),
		FillID:       fillID,
		Direction:    event.DirectionLong,
		Quantity:     quantity,
		Price:        price,
		FilledAt:     periodEnd,
	}
}

// probeCompactEntry runs a single-instrument compact-entry fixture under cfg
// and returns the Quantity and EntryLevel every instrument's opening Unit
// will size to: every instrument in this file's fixtures runs the identical
// bars, so the reducer's own sizing arithmetic (Notional Account, N, dollars
// per point) produces the identical Quantity and EntryLevel for each of
// them. Probing once, rather than hand-deriving the arithmetic a second
// time, is what keeps these fixtures anchored to the production sizing
// rather than to a second, driftable copy of it.
func probeCompactEntry(t *testing.T, cfg event.ConfigurationPayload) (quantity int64, entryLevel float64, periodEnd time.Time) {
	t.Helper()
	bars := compactEntryBars("PROBE", 0)
	emitted := newStream(t, cfg).bars(bars).mustRun()
	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("probe: got %d trade proposal(s), want exactly 1", len(proposals))
	}
	proposal := decodeTradeProposal(t, proposals[0])
	return proposal.Quantity, proposal.EntryLevel, bars[len(bars)-1].PeriodEnd
}

// --- Each cap binding in turn -------------------------------------------

// TestFifthAddIsDeclinedForTheInstrumentCap is ADR 0008's per-instrument cap
// (4 Units), now enforced with a named decline rather than the reducer's
// former silent "propose nothing": a Campaign already holding its
// configured maximum reaches its next rung, and the Add is declined naming
// the instrument cap and the exposure (5) that would have resulted.
func TestFifthAddIsDeclinedForTheInstrumentCap(t *testing.T) {
	t.Parallel()

	cfg := capsConfig(4, 1_000_000, 1_000_000, 1_000_000)
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)

	ladder, err := sizing.AddLadder(campaignFillPrice, campaignN, cfg.MaxUnits, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("AddLadder() error = %v", err)
	}
	bigBar := addOpportunityBar("AAPL", day(57), ladder[3]+1)
	fill2 := addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", ladder[1], 133, day(57))
	fill3 := addFill("AAPL", campaignID, 3, day(57), "sim-fill-add-3", ladder[2], 133, day(57))
	fill4 := addFill("AAPL", campaignID, 4, day(57), "sim-fill-add-4", ladder[3], 133, day(57))
	// Clears every remaining rung by a huge margin: the Campaign is already
	// at its configured maximum of 4 Units, so the fifth Add opportunity is
	// declined for the instrument cap rather than silently skipped.
	fifthBar := addOpportunityBar("AAPL", day(58), ladder[3]+100_000)

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(bigBar).
		fill(fill2).
		fill(fill3).
		fill(fill4).
		bar(fifthBar).
		mustRun()

	added := envelopesOfType(emitted, event.CampaignUnitAddedEventType)
	if len(added) != 3 {
		t.Fatalf("got %d unit-added event(s), want exactly 3 (units 2, 3 and 4)", len(added))
	}
	declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1 (the fifth Unit)", len(declines))
	}
	decline := decodeProposalDeclined(t, declines[0])
	if decline.Kind != event.ProposalDeclinedKindAdd {
		t.Errorf("Kind = %q, want %q", decline.Kind, event.ProposalDeclinedKindAdd)
	}
	if decline.CampaignID != campaignID {
		t.Errorf("CampaignID = %q, want %q", decline.CampaignID, campaignID)
	}
	if decline.Reason != event.DeclineReasonUnitCapExceeded {
		t.Fatalf("Reason = %q, want %q", decline.Reason, event.DeclineReasonUnitCapExceeded)
	}
	if decline.Cap != event.CapInstrument {
		t.Errorf("Cap = %q, want %q", decline.Cap, event.CapInstrument)
	}
	if decline.CapLimit != 4 {
		t.Errorf("CapLimit = %d, want 4", decline.CapLimit)
	}
	if decline.PostTradeExposure != 5 {
		t.Errorf("PostTradeExposure = %d, want 5 (4 held plus the proposed Unit)", decline.PostTradeExposure)
	}
	if decline.Detail == "" {
		t.Error("Detail is empty; a decline must record the figures that produced it")
	}
	if err := decline.Validate(); err != nil {
		t.Errorf("decline fails its own Validate(): %v", err)
	}
}

// TestUnclassifiedGroupCapBindsAcrossSeveralInstruments is #36's own named
// case: several UNLABELLED instruments — no classification input exists yet
// (#37/#41), so every one of them is Unclassified — share CONTEXT.md's
// single Unclassified Group, and the group's cap binds across them, not
// per instrument.
func TestUnclassifiedGroupCapBindsAcrossSeveralInstruments(t *testing.T) {
	t.Parallel()

	// Per-instrument cap generous (never the binding constraint here);
	// industry cap generous and unreachable in any case (every instrument is
	// Unclassified); the group cap is the tight one, at 2; total long
	// generous so it never competes with the group cap in this fixture.
	cfg := compactChannelConfig(1_000_000, 1_000_000, 2, 1_000_000)
	quantity, entryLevel, _ := probeCompactEntry(t, cfg)

	s := newStream(t, cfg)
	for i, id := range []string{"AAA", "BBB"} {
		bars := compactEntryBars(id, i*22)
		fill := compactEntryFill(id, bars[len(bars)-1].PeriodEnd, fmt.Sprintf("sim-fill-%s", id), quantity, entryLevel)
		s = s.bars(bars).fill(fill)
	}
	// A third Unclassified instrument: the group already holds 2 Units (its
	// own configured cap), so this entry's post-trade exposure of 3 exceeds
	// it, and the Signal is declined rather than sized into a Campaign.
	thirdBars := compactEntryBars("CCC", 2*22)
	emitted := s.bars(thirdBars).mustRun()

	opened := envelopesOfType(emitted, event.CampaignOpenedEventType)
	if len(opened) != 2 {
		t.Fatalf("got %d Campaign(s) opened, want exactly 2 (AAA and BBB)", len(opened))
	}
	declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1 (CCC's entry)", len(declines))
	}
	decline := decodeProposalDeclined(t, declines[0])
	if decline.InstrumentID != "CCC" {
		t.Errorf("InstrumentID = %q, want %q", decline.InstrumentID, "CCC")
	}
	if decline.Kind != event.ProposalDeclinedKindEntry {
		t.Errorf("Kind = %q, want %q", decline.Kind, event.ProposalDeclinedKindEntry)
	}
	if decline.Reason != event.DeclineReasonUnitCapExceeded {
		t.Fatalf("Reason = %q, want %q", decline.Reason, event.DeclineReasonUnitCapExceeded)
	}
	if decline.Cap != event.CapUnclassifiedGroup {
		t.Errorf("Cap = %q, want %q", decline.Cap, event.CapUnclassifiedGroup)
	}
	if decline.CapLimit != 2 {
		t.Errorf("CapLimit = %d, want 2", decline.CapLimit)
	}
	if decline.PostTradeExposure != 3 {
		t.Errorf("PostTradeExposure = %d, want 3 (AAA and BBB's Units plus CCC's proposed one)", decline.PostTradeExposure)
	}
}

// TestTotalLongCapBindsAcrossInstruments is ADR 0008's total-long cap (12 in
// the Baseline, here configured to 2): it sums every open Campaign's Units
// regardless of instrument, industry, sector or classification, so it binds
// once when the Unclassified Group's own cap is generous enough not to bind
// first.
func TestTotalLongCapBindsAcrossInstruments(t *testing.T) {
	t.Parallel()

	cfg := compactChannelConfig(1_000_000, 1_000_000, 1_000_000, 2)
	quantity, entryLevel, _ := probeCompactEntry(t, cfg)

	s := newStream(t, cfg)
	for i, id := range []string{"DDD", "EEE"} {
		bars := compactEntryBars(id, i*22)
		fill := compactEntryFill(id, bars[len(bars)-1].PeriodEnd, fmt.Sprintf("sim-fill-%s", id), quantity, entryLevel)
		s = s.bars(bars).fill(fill)
	}
	thirdBars := compactEntryBars("FFF", 2*22)
	emitted := s.bars(thirdBars).mustRun()

	opened := envelopesOfType(emitted, event.CampaignOpenedEventType)
	if len(opened) != 2 {
		t.Fatalf("got %d Campaign(s) opened, want exactly 2 (DDD and EEE)", len(opened))
	}
	declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1 (FFF's entry)", len(declines))
	}
	decline := decodeProposalDeclined(t, declines[0])
	if decline.Reason != event.DeclineReasonUnitCapExceeded {
		t.Fatalf("Reason = %q, want %q", decline.Reason, event.DeclineReasonUnitCapExceeded)
	}
	if decline.Cap != event.CapTotalLong {
		t.Errorf("Cap = %q, want %q", decline.Cap, event.CapTotalLong)
	}
	if decline.CapLimit != 2 {
		t.Errorf("CapLimit = %d, want 2", decline.CapLimit)
	}
	if decline.PostTradeExposure != 3 {
		t.Errorf("PostTradeExposure = %d, want 3", decline.PostTradeExposure)
	}
}

// TestAddBlockedByTheTotalLongCap is #36's own named case: "an Add blocked
// by a cap" — not the per-instrument one (TestFifthAddIsDeclinedForTheInstrumentCap
// already covers that), but a shared, cross-instrument cap. Two instruments
// each hold their single opening Unit, exactly filling the configured
// total-long cap of 2; a further Add on either one is then declined for the
// total-long cap, not the (generous) instrument cap.
func TestAddBlockedByTheTotalLongCap(t *testing.T) {
	t.Parallel()

	cfg := compactChannelConfig(1_000_000, 1_000_000, 1_000_000, 2)
	quantity, entryLevel, _ := probeCompactEntry(t, cfg)

	campaignID := testDecisionID("campaign", "GGG", day(21))
	ggg := compactEntryBars("GGG", 0)
	gggFill := compactEntryFill("GGG", ggg[len(ggg)-1].PeriodEnd, "sim-fill-GGG", quantity, entryLevel)
	hhh := compactEntryBars("HHH", 22)
	hhhFill := compactEntryFill("HHH", hhh[len(hhh)-1].PeriodEnd, "sim-fill-HHH", quantity, entryLevel)
	// Clears any conceivable Add Ladder rung: the point of this bar is the
	// cap, not the rung arithmetic.
	addBar := addOpportunityBar("GGG", day(45), entryLevel+1_000_000)

	emitted := newStream(t, cfg).
		bars(ggg).fill(gggFill).
		bars(hhh).fill(hhhFill).
		bar(addBar).
		mustRun()

	opened := envelopesOfType(emitted, event.CampaignOpenedEventType)
	if len(opened) != 2 {
		t.Fatalf("got %d Campaign(s) opened, want exactly 2 (GGG and HHH)", len(opened))
	}
	declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1 (GGG's Add)", len(declines))
	}
	decline := decodeProposalDeclined(t, declines[0])
	if decline.Kind != event.ProposalDeclinedKindAdd {
		t.Errorf("Kind = %q, want %q", decline.Kind, event.ProposalDeclinedKindAdd)
	}
	if decline.CampaignID != campaignID {
		t.Errorf("CampaignID = %q, want %q", decline.CampaignID, campaignID)
	}
	if decline.Reason != event.DeclineReasonUnitCapExceeded {
		t.Fatalf("Reason = %q, want %q", decline.Reason, event.DeclineReasonUnitCapExceeded)
	}
	if decline.Cap != event.CapTotalLong {
		t.Errorf("Cap = %q, want %q (the instrument cap is generous; the shared total-long cap is what binds)", decline.Cap, event.CapTotalLong)
	}
	if decline.CapLimit != 2 {
		t.Errorf("CapLimit = %d, want 2", decline.CapLimit)
	}
	if decline.PostTradeExposure != 3 {
		t.Errorf("PostTradeExposure = %d, want 3 (GGG and HHH's Units plus GGG's proposed Add)", decline.PostTradeExposure)
	}
}

// TestReplayingACapDeclineFixtureTwiceYieldsByteIdenticalEmissions is this
// package's standard replay-equivalence shape (e.g.
// TestReplayingTheFourUnitAddFixtureTwiceYieldsByteIdenticalEmissions),
// applied to a fixture whose only interesting decision is a Unit-cap
// decline: two runs of one fixture must agree byte for byte, cap decline
// included.
func TestReplayingACapDeclineFixtureTwiceYieldsByteIdenticalEmissions(t *testing.T) {
	t.Parallel()

	cfg := compactChannelConfig(1_000_000, 1_000_000, 1, 1_000_000)

	build := func() []event.Envelope {
		quantity, entryLevel, _ := probeCompactEntry(t, cfg)
		s := newStream(t, cfg)
		first := compactEntryBars("III", 0)
		firstFill := compactEntryFill("III", first[len(first)-1].PeriodEnd, "sim-fill-III", quantity, entryLevel)
		s = s.bars(first).fill(firstFill)
		second := compactEntryBars("JJJ", 22)
		return s.bars(second).mustRun()
	}

	first, second := build(), build()
	if len(first) != len(second) {
		t.Fatalf("emission counts differ: %d and %d", len(first), len(second))
	}
	declines := envelopesOfType(first, event.ProposalDeclinedEventType)
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s) in the first run, want exactly 1: the fixture must actually exercise a cap decline", len(declines))
	}
	if decodeProposalDeclined(t, declines[0]).Reason != event.DeclineReasonUnitCapExceeded {
		t.Fatalf("decline reason = %q, want %q", decodeProposalDeclined(t, declines[0]).Reason, event.DeclineReasonUnitCapExceeded)
	}
	for i := range first {
		a, err := json.Marshal(first[i])
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		b, err := json.Marshal(second[i])
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("emission %d differs between replays:\n  first:  %s\n  second: %s", i, a, b)
		}
	}
}

// --- Property test: post-trade Unit exposure never exceeds any cap ------

// TestPostTradeExposureNeverExceedsAnyCap is #36's property test: across a
// deterministically generated sequence of entries and Add attempts, spread
// over several Unclassified instruments, post-trade Unit exposure — per
// instrument, across the shared Unclassified Group, and in total — never
// exceeds its configured cap. The generator is seeded, so the sequence it
// drives the reducer through is identical on every run.
//
// The per-instrument cap is set to 1, so EVERY Add attempt in the generated
// sequence is a falsifiable instrument-cap case: an implementation that
// forgot to check post-trade exposure (or checked pre-trade instead) would
// let a second Unit through, and this test would then find a Campaign
// holding 2 Units against a configured cap of 1.
//
// Driven incrementally, one input at a time (Reducer.Apply, not
// stream.mustRun's upfront envelope list): whether an entry or an Add
// attempt actually opens or extends a Campaign is exactly what the caps
// under test decide, so the next action the generator can legally take
// (which instruments have a real, filled Campaign to Add to) depends on
// what the PREVIOUS one actually produced, not on an assumption pinned in
// advance.
func TestPostTradeExposureNeverExceedsAnyCap(t *testing.T) {
	t.Parallel()

	const (
		maxUnits            = 1
		maxUnitsPerIndustry = 1_000_000 // unreachable: every instrument is Unclassified
		maxUnitsPerSector   = 3
		maxUnitsTotalLong   = 5
	)
	cfg := compactChannelConfig(maxUnits, maxUnitsPerIndustry, maxUnitsPerSector, maxUnitsTotalLong)
	quantity, entryLevel, _ := probeCompactEntry(t, cfg)

	reducer, err := strategy.NewReducer(testStrategyVersion, cfg)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	var seq uint64
	nextSeq := func() uint64 { seq++; return seq }
	var allEmitted []event.Envelope
	apply := func(envelope event.Envelope) []event.Envelope {
		t.Helper()
		out, err := reducer.Apply(context.Background(), envelope)
		if err != nil {
			t.Fatalf("Apply(%s) error = %v", envelope.Type, err)
		}
		allEmitted = append(allEmitted, out...)
		return out
	}
	apply(configEnvelopeFor(t, cfg, nextSeq()))
	snapshot := defaultAccountSnapshot(cfg)
	apply(accountSnapshotEnvelope(t, nextSeq(), snapshot, snapshot.AsOf))

	// A small, fixed-seed generator: deterministic across runs, but not
	// hand-picked to visit exactly one scenario. rng.Intn(3) == 0 biases
	// toward "add" a little less than half the time, so both kinds of
	// action, and repeated attempts against an already-capped group, are
	// exercised. Only an instrument with a CONFIRMED open Campaign (its
	// entry actually filled, rather than being declined) is ever picked as
	// an Add target — what the previous step of THIS run actually decided,
	// not an assumption made in advance.
	rng := rand.New(rand.NewSource(42))
	pool := []string{"G0", "G1", "G2", "G3", "G4", "G5", "G6", "G7", "G8"}
	var openCampaigns []string
	dayOffset, nextPoolIndex := 0, 0

	entered := 0
	for step := 0; step < 14; step++ {
		tryAdd := len(openCampaigns) > 0 && rng.Intn(3) == 0
		if !tryAdd && nextPoolIndex >= len(pool) {
			tryAdd = len(openCampaigns) > 0
			if !tryAdd {
				break // pool exhausted and nothing open to Add to
			}
		}

		if tryAdd {
			id := openCampaigns[rng.Intn(len(openCampaigns))]
			// Clears any conceivable Add Ladder rung: the generator is
			// testing the cap, not the rung arithmetic (this file's other
			// tests already pin the rung itself, e.g.
			// TestFifthAddIsDeclinedForTheInstrumentCap). With maxUnits=1
			// every Add attempt exceeds the per-instrument cap by
			// construction, so none is ever expected to succeed here.
			bar := addOpportunityBar(id, day(dayOffset+1), entryLevel+1_000_000+float64(step))
			apply(barEnvelope(t, nextSeq(), bar, bar.PeriodEnd))
			sessionEmitted := apply(sessionClosedEnvelope(t, nextSeq(), bar.PeriodEnd, []string{id}))
			dayOffset++
			if len(envelopesOfType(sessionEmitted, event.AddProposalEventType)) != 0 {
				t.Fatalf("instrument %q's Add was proposed despite a per-instrument cap of %d already held by its own Campaign; the generator's premise (every Add attempt here exceeds it) no longer holds", id, maxUnits)
			}
			continue
		}

		id := pool[nextPoolIndex]
		nextPoolIndex++
		entered++
		bars := compactEntryBars(id, dayOffset)
		// Each of this instrument's warm-up bars is its own Session (ADR
		// 0021: no other instrument has a bar on any of these days), so
		// each must close before the next bar's Session can open — the
		// same shape stream.bar() gives a single-instrument fixture.
		var sessionEmitted []event.Envelope
		for _, bar := range bars {
			apply(barEnvelope(t, nextSeq(), bar, bar.PeriodEnd))
			sessionEmitted = apply(sessionClosedEnvelope(t, nextSeq(), bar.PeriodEnd, []string{id}))
		}
		periodEnd := bars[len(bars)-1].PeriodEnd
		dayOffset += len(bars) + 1
		if proposals := envelopesOfType(sessionEmitted, event.TradeProposalEventType); len(proposals) == 1 {
			fill := compactEntryFill(id, periodEnd, fmt.Sprintf("sim-fill-entry-%s-%d", id, entered), quantity, entryLevel)
			apply(fillEnvelope(t, nextSeq(), fill))
			openCampaigns = append(openCampaigns, id)
		}
	}
	emitted := allEmitted

	// The independent model this test checks the reducer's own arithmetic
	// against: tallied from the ACTUAL committed decisions (Campaign-opened,
	// unit-added), which is what the caps must actually have bounded.
	perInstrument := map[string]int{}
	group, total := 0, 0
	limitFor := func(capName string) int {
		switch capName {
		case event.CapInstrument:
			return maxUnits
		case event.CapUnclassifiedGroup:
			return maxUnitsPerSector
		case event.CapTotalLong:
			return maxUnitsTotalLong
		default:
			t.Fatalf("unexpected cap %q for an Unclassified instrument", capName)
			return 0
		}
	}

	for _, e := range emitted {
		switch e.Type {
		case event.CampaignOpenedEventType:
			p := decodeCampaignOpened(t, e)
			perInstrument[p.InstrumentID]++
			group++
			total++
		case event.CampaignUnitAddedEventType:
			var p event.CampaignUnitAddedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("json.Unmarshal(CampaignUnitAddedPayload) error = %v", err)
			}
			perInstrument[p.InstrumentID]++
			group++
			total++
		case event.ProposalDeclinedEventType:
			p := decodeProposalDeclined(t, e)
			if p.Reason != event.DeclineReasonUnitCapExceeded {
				continue
			}
			var wantExposure int
			switch p.Cap {
			case event.CapInstrument:
				wantExposure = perInstrument[p.InstrumentID] + 1
			case event.CapUnclassifiedGroup:
				wantExposure = group + 1
			case event.CapTotalLong:
				wantExposure = total + 1
			default:
				t.Fatalf("decline for %q names unexpected cap %q", p.InstrumentID, p.Cap)
			}
			if p.PostTradeExposure != wantExposure {
				t.Errorf("decline for %q: PostTradeExposure = %d, want %d (independently tallied)", p.InstrumentID, p.PostTradeExposure, wantExposure)
			}
			if p.CapLimit != limitFor(p.Cap) {
				t.Errorf("decline for %q: CapLimit = %d, want %d for cap %q", p.InstrumentID, p.CapLimit, limitFor(p.Cap), p.Cap)
			}
			if p.PostTradeExposure <= p.CapLimit {
				t.Errorf("decline for %q: post-trade exposure %d does not exceed cap %q's limit %d", p.InstrumentID, p.PostTradeExposure, p.Cap, p.CapLimit)
			}
		}

		// The property itself, checked after EVERY committed decision, not
		// just at the end: post-trade Unit exposure never exceeds any cap.
		for id, n := range perInstrument {
			if n > maxUnits {
				t.Fatalf("after %s: instrument %q holds %d Units, exceeding its cap %d", e.Type, id, n, maxUnits)
			}
		}
		if group > maxUnitsPerSector {
			t.Fatalf("after %s: the Unclassified Group holds %d Units, exceeding its cap %d", e.Type, group, maxUnitsPerSector)
		}
		if total > maxUnitsTotalLong {
			t.Fatalf("after %s: total long exposure is %d Units, exceeding its cap %d", e.Type, total, maxUnitsTotalLong)
		}
	}

	if len(envelopesOfType(emitted, event.CampaignOpenedEventType)) == 0 {
		t.Fatal("fixture error: the generated sequence opened no Campaign at all")
	}
	if len(envelopesOfType(emitted, event.ProposalDeclinedEventType)) == 0 {
		t.Fatal("fixture error: the generated sequence produced no decline at all; the caps were never exercised")
	}
}
