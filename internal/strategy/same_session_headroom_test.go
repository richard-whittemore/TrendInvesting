package strategy_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
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
