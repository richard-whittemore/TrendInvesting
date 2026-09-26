package strategy_test

import (
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// This file holds ADR 0010's own previous-close basis, extended by the
// owner's decision recorded there to Unit-cap headroom as well as cash: an
// entry may not spend either the cash OR the cap headroom an exit frees on
// the SAME bar (Session). ADR 0010's text is explicit that both are "known
// at the previous close", and ADR 0020's cash-only amendment leaves that
// sentence for Unit-cap headroom untouched.
//
// cash_skip_test.go already proves the cash half: a sell's proceeds are
// never credited within the bar they happen in, only through a LATER
// account.snapshot (ADR 0020's "Filled"/"available" ledger). This file
// proves the cap-headroom half at the identical seam, and pins the case ADR
// 0021's own "Open" section named directly: a Campaign
// closed by a fill delivered BEFORE its Session's market.session.closed —
// exactly what internal/fills.RunSession's open-instant pass delivers for a
// stop gapping through at a Session's own open (RunBar's own doc comment,
// step 1) — must not free its cap headroom for that SAME Session's other
// entries and Adds. unit_caps.go's instrumentUnits/groupUnits read the
// CURRENTLY open Campaign's Units, so without instrumentState's own
// unitsFreedThisSession bookkeeping a Campaign already closed by the time
// the session-close pass runs would silently drop out of every cap it
// counted towards, one full Session early.

// oldMaxUnitsTotalLong is the only cap this file's fixture lets bind:
// instrument, industry and sector are all configured far out of reach, so a
// decline can only ever name the total-long cap.
const oldMaxUnitsTotalLong = 1

// headroomCapConfig is compactChannelConfig with every cap but total-long
// generous, and total-long itself pinned at oldMaxUnitsTotalLong: one
// Campaign anywhere in the run already spends the whole budget.
func headroomCapConfig() event.ConfigurationPayload {
	return compactChannelConfig(1_000_000, 1_000_000, 1_000_000, oldMaxUnitsTotalLong)
}

// buildOldCampaignAndSharedSessionBreakout opens OLD's one-Unit Campaign on
// its own timeline (consuming the total-long cap's only Unit), then warms up
// NEW on a second, later, non-overlapping timeline (ADR 0021: Sessions
// follow one another strictly), delivering every one of NEW's own bars
// except its last two as ordinary, quiet Sessions. The caller places those
// last two bars — newBars[len-2] and newBars[len-1], the latter NEW's actual
// breakout — and decides what happens to OLD's Campaign, and in what order,
// around them.
func buildOldCampaignAndSharedSessionBreakout(t *testing.T, cfg event.ConfigurationPayload) (s *stream, oldCampaignID, oldOpenFillID string, oldQuantity int64, newBars []event.CompletedBarPayload) {
	t.Helper()

	quantity, entryLevel, oldPeriodEnd := probeCompactEntry(t, cfg)
	oldOpenFillID = "old-fill-open"
	oldFill := compactEntryFill("OLD", oldPeriodEnd, oldOpenFillID, quantity, entryLevel)
	oldCampaignID = testDecisionID("campaign", "OLD", oldPeriodEnd)

	s = newStream(t, cfg).bars(compactEntryBars("OLD", 0)).fill(oldFill)

	newBars = compactEntryBars("NEW", compactEntryStride)
	for _, b := range newBars[:len(newBars)-2] {
		s.bar(b)
	}
	return s, oldCampaignID, oldOpenFillID, quantity, newBars
}

// oldStopFill is the fill that closes OLD's Campaign, gapping through its
// stop at filledAt: applyStopFill never checks the level against a bar's own
// range (campaign.go's own doc comment, mirroring applyExitFill's), so this
// fixture states the closing fact directly at the event seam
// (docs/development.md).
func oldStopFill(campaignID, unitID string, quantity int64, filledAt event.CompletedBarPayload) event.FillPayload {
	return event.FillPayload{
		InstrumentID: "OLD",
		Kind:         event.FillKindStop,
		CampaignID:   campaignID,
		FillID:       "old-fill-stop",
		UnitIDs:      []string{unitID},
		Direction:    event.DirectionLong,
		Quantity:     quantity,
		Price:        1, // Any positive price: never compared against a bar range (see doc comment above).
		FilledAt:     filledAt.PeriodEnd,
	}
}

// tradeProposalsFor filters emitted's trade proposals to instrumentID: the
// fixtures in this file replay their WHOLE stream, including OLD's own
// original entry, so a bare count of every proposal in the run says nothing
// about NEW's own outcome on its own.
func tradeProposalsFor(t *testing.T, emitted []event.Envelope, instrumentID string) []event.TradeProposalPayload {
	t.Helper()
	var out []event.TradeProposalPayload
	for _, e := range envelopesOfType(emitted, event.TradeProposalEventType) {
		p := decodeTradeProposal(t, e)
		if p.InstrumentID == instrumentID {
			out = append(out, p)
		}
	}
	return out
}

// proposalDeclinedFor is tradeProposalsFor for strategy.proposal.declined.
func proposalDeclinedFor(t *testing.T, emitted []event.Envelope, instrumentID string) []event.ProposalDeclinedPayload {
	t.Helper()
	var out []event.ProposalDeclinedPayload
	for _, e := range envelopesOfType(emitted, event.ProposalDeclinedEventType) {
		p := decodeProposalDeclined(t, e)
		if p.InstrumentID == instrumentID {
			out = append(out, p)
		}
	}
	return out
}

// TestEntryCannotUseCapHeadroomFreedByAnExitDeliveredBeforeTheSessionCloses
// is this ticket's negative test for cap headroom: OLD's Campaign, alone,
// already holds the total-long cap's only Unit. Its stop gaps through and
// closes it on the SAME Session that NEW's own breakout is decided —
// delivered, as internal/fills.RunSession's own open-instant pass would
// deliver it, BEFORE that Session's market.session.closed. NEW's entry must
// still be declined: the Unit OLD's exit frees is not available until the
// NEXT Session (ADR 0010), whatever order the fill and the bar arrive in.
//
// An implementation that lets a same-Session exit's fill (delivered ahead of
// the close) shrink what unit_caps.go counts as committed passes this test
// only if it is broken back to reading live Campaign state alone — which is
// exactly the defect this test is written to catch.
func TestEntryCannotUseCapHeadroomFreedByAnExitDeliveredBeforeTheSessionCloses(t *testing.T) {
	t.Parallel()

	cfg := headroomCapConfig()
	s, campaignID, unitID, quantity, newBars := buildOldCampaignAndSharedSessionBreakout(t, cfg)
	priorBar := newBars[len(newBars)-2]
	breakoutBar := newBars[len(newBars)-1]

	// A quiet Session for NEW, still one short of its own breakout, while
	// OLD's Campaign remains open.
	s.bar(priorBar)

	stop := oldStopFill(campaignID, unitID, quantity, breakoutBar)
	emitted := s.fill(stop).barOnly(breakoutBar).closeSession(breakoutBar.PeriodEnd, "NEW").mustRun()

	if exited := envelopesOfType(emitted, event.CampaignExitedEventType); len(exited) != 1 {
		t.Fatalf("fixture error: got %d campaign-exited event(s) for OLD, want exactly 1", len(exited))
	}

	if proposals := tradeProposalsFor(t, emitted, "NEW"); len(proposals) != 0 {
		t.Fatalf("got %d trade proposal(s) for NEW, want 0: OLD's Unit, freed by an exit in THIS Session, is not available until the next one (ADR 0010)", len(proposals))
	}
	declines := proposalDeclinedFor(t, emitted, "NEW")
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1, for NEW", len(declines))
	}
	decline := declines[0]
	if decline.Reason != event.DeclineReasonUnitCapExceeded {
		t.Errorf("Reason = %q, want %q", decline.Reason, event.DeclineReasonUnitCapExceeded)
	}
	if decline.Cap != event.CapTotalLong {
		t.Errorf("Cap = %q, want %q", decline.Cap, event.CapTotalLong)
	}
	if decline.CapLimit != oldMaxUnitsTotalLong {
		t.Errorf("CapLimit = %d, want %d", decline.CapLimit, oldMaxUnitsTotalLong)
	}
	// OLD's Unit, still committed for THIS Session, plus NEW's own proposed
	// one: 2, one past the limit of 1.
	if decline.PostTradeExposure != oldMaxUnitsTotalLong+1 {
		t.Errorf("PostTradeExposure = %d, want %d (OLD's freed-this-Session Unit plus NEW's own)", decline.PostTradeExposure, oldMaxUnitsTotalLong+1)
	}
	if err := decline.Validate(); err != nil {
		t.Errorf("decline fails its own Validate(): %v", err)
	}
}

// TestEntryMayUseCapHeadroomFreedByAnExitOnTheFollowingSession is criterion
// (c): capital OLD's exit frees on Session t is available on Session t+1,
// once that Session's own bar and market.session.closed have made the
// Session current — exactly the SAME fixture as the negative test above,
// with OLD's exit moved one whole Session earlier: it closes, and that
// Session closes in full, BEFORE NEW's own breakout Session even opens.
func TestEntryMayUseCapHeadroomFreedByAnExitOnTheFollowingSession(t *testing.T) {
	t.Parallel()

	cfg := headroomCapConfig()
	s, campaignID, unitID, quantity, newBars := buildOldCampaignAndSharedSessionBreakout(t, cfg)
	priorBar := newBars[len(newBars)-2]
	breakoutBar := newBars[len(newBars)-1]

	// OLD's exit lands on the Session immediately before NEW's own breakout
	// bar, and that Session closes in full (bar, then market.session.closed)
	// before NEW's own breakout Session opens — a full Session's gap, not
	// merely a fill/bar ordering within one.
	stop := oldStopFill(campaignID, unitID, quantity, priorBar)
	s.fill(stop).bar(priorBar)

	emitted := s.bar(breakoutBar).mustRun()

	if exited := envelopesOfType(emitted, event.CampaignExitedEventType); len(exited) != 1 {
		t.Fatalf("fixture error: got %d campaign-exited event(s) for OLD, want exactly 1", len(exited))
	}
	proposals := tradeProposalsFor(t, emitted, "NEW")
	if len(proposals) != 1 {
		t.Fatalf("got %d trade proposal(s) for NEW, want exactly 1: OLD's Unit, freed a full Session ago, funds NEW's entry (ADR 0010)", len(proposals))
	}
	if declines := proposalDeclinedFor(t, emitted, "NEW"); len(declines) != 0 {
		t.Fatalf("got %d decline(s) for NEW, want 0", len(declines))
	}
}

// TestEntryCannotUseCapHeadroomFreedByAnExitTimestampedBeforeThePeriodEnd is
// the live-trading shape of the same rule: a stop or exit fill's own FilledAt
// is its real execution instant, not the Session's own close (ADR 0021 §6's
// amendment: a live adapter reports "this Session's fills" ahead of "this
// Session's bar, and its market.session.closed"). Comparing a fill's FilledAt
// against the Session's own period end — rather than against which Session
// is actually open — would miss this shape entirely: the fill is delivered
// WHILE the Session is already open (its own bar has arrived), but its own
// timestamp is hours earlier than the Session's close. OLD's Unit must still
// be committed for NEW's own entry, decided at that SAME Session's close.
func TestEntryCannotUseCapHeadroomFreedByAnExitTimestampedBeforeThePeriodEnd(t *testing.T) {
	t.Parallel()

	cfg := headroomCapConfig()
	s, campaignID, unitID, quantity, newBars := buildOldCampaignAndSharedSessionBreakout(t, cfg)
	priorBar := newBars[len(newBars)-2]
	breakoutBar := newBars[len(newBars)-1]
	s.bar(priorBar)

	// The Session is opened by NEW's own bar FIRST, so it is already open —
	// sessionOpen is true — when OLD's stop fill arrives. That fill's own
	// FilledAt is hours before the Session's own period end, exactly what a
	// live venue reports for an intraday execution, and unlike every other
	// fixture in this file it is NOT equal to breakoutBar.PeriodEnd.
	stop := event.FillPayload{
		InstrumentID: "OLD",
		Kind:         event.FillKindStop,
		CampaignID:   campaignID,
		FillID:       "old-fill-stop-intraday",
		UnitIDs:      []string{unitID},
		Direction:    event.DirectionLong,
		Quantity:     quantity,
		Price:        1,
		FilledAt:     breakoutBar.PeriodEnd.Add(-4 * time.Hour),
	}

	emitted := s.barOnly(breakoutBar).fill(stop).closeSession(breakoutBar.PeriodEnd, "NEW").mustRun()

	if exited := envelopesOfType(emitted, event.CampaignExitedEventType); len(exited) != 1 {
		t.Fatalf("fixture error: got %d campaign-exited event(s) for OLD, want exactly 1", len(exited))
	}
	if proposals := tradeProposalsFor(t, emitted, "NEW"); len(proposals) != 0 {
		t.Fatalf("got %d trade proposal(s) for NEW, want 0: OLD's Unit is still committed for the Session that was already open when its exit fill arrived, whatever that fill's own timestamp says (ADR 0010)", len(proposals))
	}
	declines := proposalDeclinedFor(t, emitted, "NEW")
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1, for NEW", len(declines))
	}
	if declines[0].Cap != event.CapTotalLong || declines[0].PostTradeExposure != oldMaxUnitsTotalLong+1 {
		t.Errorf("decline = %+v, want the total-long cap exceeded by OLD's still-committed Unit plus NEW's own", declines[0])
	}
}

// TestTwoClosingFillsInOneSessionAtDifferentTimestampsBothStayCommitted pins
// the accumulation half of the same mechanism: a two-Unit Campaign closed by
// TWO separate stop fills within one Session — Unit 1 first, Unit 2 a few
// hours later — must keep BOTH Units committed for that Session's own
// decisions, not just the most recently recorded one. A reset keyed to
// timestamp EQUALITY (rather than to which Session's decisions the fill
// belongs to) would silently forget Unit 1's the moment Unit 2's
// differently-timestamped closing fill arrived.
func TestTwoClosingFillsInOneSessionAtDifferentTimestampsBothStayCommitted(t *testing.T) {
	t.Parallel()

	// The per-instrument cap must allow 2 Units so OLD can hold both before
	// either closes; total-long is what this test actually binds, at 2 —
	// OLD's own two Units alone spend it.
	cfg := compactChannelConfig(2, 1_000_000, 1_000_000, 2)

	proposal, oldPeriodEnd := probeCompactEntryFull(t, cfg)
	unit1ID := "old-two-fill-unit1"
	oldFill := compactEntryFill("OLD", oldPeriodEnd, unit1ID, proposal.Quantity, proposal.EntryLevel)
	oldCampaignID := testDecisionID("campaign", "OLD", oldPeriodEnd)

	// Unit 2, added the day after OLD opens, at the rung ADR 0006's Add
	// Ladder arithmetic actually produces from Unit 1's own fill — computed,
	// never hand-typed, so this fixture cannot silently drift from the
	// production arithmetic it depends on (add_test.go's own discipline).
	// addOpportunityBar's own fixed Low/Close (150) belongs to the unrelated
	// breakoutBars price scale (entries near 200); this fixture's own compact
	// scale (entries near 120) needs its own bar, with a Low comfortably
	// above the compact fixture's warmed-up Exit Channel (100) and below the
	// rung, so the bar reaches the Add opportunity without also breaching the
	// Exit Channel.
	rung2, err := sizing.NextAddLevel(proposal.EntryLevel, proposal.N, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}
	addBar := completedBar("OLD", oldPeriodEnd.AddDate(0, 0, 1), rung2+1, 115, 115)
	unit2ID := "old-two-fill-unit2"
	unit2Fill := addFill("OLD", oldCampaignID, 2, addBar.PeriodEnd, unit2ID, rung2, proposal.Quantity, addBar.PeriodEnd)

	s := newStream(t, cfg).bars(compactEntryBars("OLD", 0)).fill(oldFill).bar(addBar).fill(unit2Fill)

	newBars := compactEntryBars("NEW", compactEntryStride)
	for _, b := range newBars[:len(newBars)-2] {
		s.bar(b)
	}
	priorBar := newBars[len(newBars)-2]
	breakoutBar := newBars[len(newBars)-1]
	s.bar(priorBar)

	// Both of OLD's Units close in the Session shared with NEW's own
	// breakout, delivered — as internal/fills.RunSession's own open-instant
	// pass would deliver a gapped stop — before that Session's own bar and
	// close, each timestamped hours apart from the other.
	stop1 := stopFillForUnits("OLD", oldCampaignID, "old-two-fill-stop1", []string{unit1ID}, 1, proposal.Quantity, breakoutBar.PeriodEnd.Add(-3*time.Hour))
	stop2 := stopFillForUnits("OLD", oldCampaignID, "old-two-fill-stop2", []string{unit2ID}, 1, proposal.Quantity, breakoutBar.PeriodEnd.Add(-1*time.Hour))

	emitted := s.fill(stop1).fill(stop2).barOnly(breakoutBar).closeSession(breakoutBar.PeriodEnd, "NEW").mustRun()

	stopped := envelopesOfType(emitted, event.CampaignUnitsStoppedEventType)
	if len(stopped) != 2 {
		t.Fatalf("fixture error: got %d units-stopped event(s) for OLD, want exactly 2 (one per fill)", len(stopped))
	}
	if exited := envelopesOfType(emitted, event.CampaignExitedEventType); len(exited) != 1 {
		t.Fatalf("fixture error: got %d campaign-exited event(s) for OLD, want exactly 1 (the second, closing fill)", len(exited))
	}

	if proposals := tradeProposalsFor(t, emitted, "NEW"); len(proposals) != 0 {
		t.Fatalf("got %d trade proposal(s) for NEW, want 0: BOTH of OLD's Units, closed minutes apart in the SAME Session, are still committed for it", len(proposals))
	}
	declines := proposalDeclinedFor(t, emitted, "NEW")
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1, for NEW", len(declines))
	}
	// Both of OLD's Units, still committed, plus NEW's own proposed one: 3,
	// one past the cap of 2. A reset-on-timestamp-mismatch bug would instead
	// see only Unit 2 (the most recently recorded fill), giving 2 — which
	// does not exceed the cap and would wrongly propose NEW's entry.
	if declines[0].Cap != event.CapTotalLong || declines[0].PostTradeExposure != 3 {
		t.Errorf("decline = %+v, want the total-long cap exceeded with PostTradeExposure 3 (OLD's two still-committed Units plus NEW's own)", declines[0])
	}
}
