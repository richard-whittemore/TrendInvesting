package strategy_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds the ticket's own headline negative test (ADR 0010, as
// amended by ADR 0020): a large exit and a large entry on the same bar. The
// entry must be declined for insufficient cash regardless of the exit's own
// size, because a sell's proceeds are never credited within the bar they
// happen in — only a LATER account.snapshot states them (ADR 0020's "Filled"
// case: "Sells are never credited by a fill: their proceeds return only
// through a later snapshot").
//
// cash_skip_test.go already pins the single-instrument shape of this rule
// (a Unit costing one cent more than available cash is declined). This file
// adds the two-instrument, same-Session shape the ticket names explicitly:
// an open Campaign's stop gaps through — a large exit, credited nowhere this
// bar — on the identical Session a second instrument's own entry is decided,
// and that entry is declined exactly as if the exit had never happened.

// probeCompactEntryFull is probeCompactEntry (unit_caps_test.go) but returns
// the whole decoded proposal, including N — cash_skip_test.go's wantHold
// needs N as well as Quantity and EntryLevel, which probeCompactEntry alone
// does not expose.
func probeCompactEntryFull(t *testing.T, cfg event.ConfigurationPayload) (proposal event.TradeProposalPayload, periodEnd time.Time) {
	t.Helper()
	bars := compactEntryBars("CASHPROBE", 0)
	emitted := newStream(t, cfg).bars(bars).mustRun()
	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("probe: got %d trade proposal(s), want exactly 1", len(proposals))
	}
	return decodeTradeProposal(t, proposals[0]), bars[len(bars)-1].PeriodEnd
}

// TestEntryCannotUseCashFreedByALargeExitOnTheSameSession is the ticket's own
// required negative fixture: a large exit and a large entry on one bar. OLD
// opens a Campaign identically to NEW's own would-be entry (same cfg, same
// compact fixture shape, so probeCompactEntryFull's one figure applies to
// both — unit_caps_test.go's probeCompactEntry's own doc comment). Cash is
// then pinned, by a snapshot dated before the shared Session, one cent short
// of NEW's own entry cost. OLD's stop then gaps through for a LARGE exit —
// several times that cost — delivered, as internal/fills.RunSession's
// open-instant pass would deliver it, before the shared Session's
// market.session.closed. NEW's entry is still declined: whatever OLD's exit
// was worth, it credits this bar's cash with nothing.
func TestEntryCannotUseCashFreedByALargeExitOnTheSameSession(t *testing.T) {
	t.Parallel()

	cfg := compactChannelConfig(1_000_000, 1_000_000, 1_000_000, 1_000_000) // every cap generous: only cash binds
	proposal, oldPeriodEnd := probeCompactEntryFull(t, cfg)
	cost := wantHold(cfg, proposal.Quantity, proposal.EntryLevel, proposal.N)

	oldOpenFillID := "old-cash-fill-open"
	oldFill := compactEntryFill("OLD", oldPeriodEnd, oldOpenFillID, proposal.Quantity, proposal.EntryLevel)
	oldCampaignID := testDecisionID("campaign", "OLD", oldPeriodEnd)

	s := newStream(t, cfg).bars(compactEntryBars("OLD", 0)).fill(oldFill)

	newBars := compactEntryBars("NEW", compactEntryStride)
	for _, b := range newBars[:len(newBars)-2] {
		s.bar(b)
	}
	priorBar := newBars[len(newBars)-2]
	breakoutBar := newBars[len(newBars)-1]

	// Cash pinned one cent short of NEW's own entry cost, as of the Session
	// immediately before the shared one — comfortably eligible under ADR
	// 0010's previous-close basis, and short by exactly the boundary
	// cash_skip_test.go's own fixtures are built to exercise, not merely
	// clear of it.
	tightCash := cost - 0.01
	s.bar(priorBar).snapshot(cashSnapshot(cfg, priorBar.PeriodEnd, tightCash))

	// A LARGE exit: several times NEW's own entry cost, so crediting it would
	// obviously fund the entry if this rule did not hold. Quantity and price
	// are OLD's own, gapped well above its entry — the exit's realised
	// proceeds are not what this fixture is about, only that they are large.
	largeExitPrice := proposal.EntryLevel * 5
	stop := event.FillPayload{
		InstrumentID: "OLD",
		Kind:         event.FillKindStop,
		CampaignID:   oldCampaignID,
		FillID:       "old-cash-fill-stop",
		UnitIDs:      []string{oldOpenFillID},
		Direction:    event.DirectionLong,
		Quantity:     proposal.Quantity,
		Price:        largeExitPrice,
		FilledAt:     breakoutBar.PeriodEnd,
	}

	emitted := s.fill(stop).barOnly(breakoutBar).closeSession(breakoutBar.PeriodEnd, "NEW").mustRun()

	if exited := envelopesOfType(emitted, event.CampaignExitedEventType); len(exited) != 1 {
		t.Fatalf("fixture error: got %d campaign-exited event(s) for OLD, want exactly 1", len(exited))
	}
	// The exit really was large: its notional value dwarfs NEW's own entry
	// cost, so a fixture that accidentally made it small would prove nothing.
	if exitNotional := float64(proposal.Quantity) * largeExitPrice; exitNotional <= cost*3 {
		t.Fatalf("fixture error: OLD's exit notional %v is not comfortably larger than NEW's own cost %v", exitNotional, cost)
	}

	if proposals := tradeProposalsFor(t, emitted, "NEW"); len(proposals) != 0 {
		t.Fatalf("got %d trade proposal(s) for NEW, want 0: a same-Session exit, however large, credits no cash today (ADR 0010, as amended by ADR 0020)", len(proposals))
	}
	declines := proposalDeclinedFor(t, emitted, "NEW")
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1, for NEW", len(declines))
	}
	decline := declines[0]
	if decline.Reason != event.DeclineReasonInsufficientCash {
		t.Errorf("Reason = %q, want %q", decline.Reason, event.DeclineReasonInsufficientCash)
	}
	if decline.RequiredCash != cost {
		t.Errorf("RequiredCash = %v, want exactly %v", decline.RequiredCash, cost)
	}
	if decline.AvailableCash != tightCash {
		t.Errorf("AvailableCash = %v, want exactly %v: OLD's same-Session exit must not have moved it", decline.AvailableCash, tightCash)
	}
	if err := decline.Validate(); err != nil {
		t.Errorf("decline fails its own Validate(): %v", err)
	}
}

// TestJournalRecordsTheCashBasisADeclineWasCheckedAgainst is criterion (d):
// the cash basis a day's decisions used is itself a journalled input
// (account.snapshot), not a number the reducer keeps only to itself. This
// reads the fixture's OWN input stream — never the reducer's live state —
// for the account.snapshot as of the decision bar's previous close, and
// confirms it is BYTE FOR BYTE the same figure the decline names: a reader
// auditing the journal alone reconstructs exactly what the reducer decided
// from, never a number they must simply trust.
func TestJournalRecordsTheCashBasisADeclineWasCheckedAgainst(t *testing.T) {
	t.Parallel()

	cfg := compactChannelConfig(1_000_000, 1_000_000, 1_000_000, 1_000_000)
	proposal, _ := probeCompactEntryFull(t, cfg)
	cost := wantHold(cfg, proposal.Quantity, proposal.EntryLevel, proposal.N)

	newBars := compactEntryBars("NEWJ", compactEntryStride)
	priorBar := newBars[len(newBars)-2]
	breakoutBar := newBars[len(newBars)-1]

	tightCash := cost - 0.01
	s := newStream(t, cfg)
	for _, b := range newBars[:len(newBars)-2] {
		s.bar(b)
	}
	s.bar(priorBar).snapshot(cashSnapshot(cfg, priorBar.PeriodEnd, tightCash))

	emitted := s.bar(breakoutBar).mustRun()

	declines := proposalDeclinedFor(t, emitted, "NEWJ")
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1", len(declines))
	}
	decline := declines[0]

	var journalledSnapshot event.AccountSnapshotPayload
	found := false
	for _, e := range s.envelopes {
		if e.Type != event.AccountSnapshotEventType {
			continue
		}
		var p event.AccountSnapshotPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode account.snapshot: %v", err)
		}
		if p.AsOf.Equal(priorBar.PeriodEnd) {
			journalledSnapshot, found = p, true
		}
	}
	if !found {
		t.Fatalf("fixture error: no account.snapshot in the journal as of %s", priorBar.PeriodEnd)
	}

	if decline.AvailableCash != journalledSnapshot.AvailableCash {
		t.Errorf("decline.AvailableCash = %v, want the journal's own account.snapshot figure %v", decline.AvailableCash, journalledSnapshot.AvailableCash)
	}
	if decline.RequiredCash != cost {
		t.Errorf("decline.RequiredCash = %v, want %v", decline.RequiredCash, cost)
	}
}

// TestJournalRecordsTheHeadroomBasisADeclineWasCheckedAgainst is criterion
// (d) for cap headroom: OLD's CampaignOpened decision (already in the
// journal from opening it) plus NEW's own decline together state the whole
// basis a headroom decline was checked against — CapLimit is the configured
// limit (also journalled, on strategy.configuration), and PostTradeExposure
// is OLD's one committed Unit plus NEW's own proposed one, exactly what the
// decline names, with nothing left for a reader to infer from live state
// they cannot see.
func TestJournalRecordsTheHeadroomBasisADeclineWasCheckedAgainst(t *testing.T) {
	t.Parallel()

	cfg := headroomCapConfig()
	s, campaignID, unitID, quantity, newBars := buildOldCampaignAndSharedSessionBreakout(t, cfg)
	priorBar := newBars[len(newBars)-2]
	breakoutBar := newBars[len(newBars)-1]
	s.bar(priorBar)

	stop := oldStopFill(campaignID, unitID, quantity, breakoutBar)
	emitted := s.fill(stop).barOnly(breakoutBar).closeSession(breakoutBar.PeriodEnd, "NEW").mustRun()

	opened := envelopesOfType(emitted, event.CampaignOpenedEventType)
	if len(opened) != 1 {
		t.Fatalf("fixture error: got %d campaign-opened event(s) for OLD, want exactly 1", len(opened))
	}
	var openedPayload event.CampaignOpenedPayload
	if err := json.Unmarshal(opened[0].Payload, &openedPayload); err != nil {
		t.Fatalf("decode campaign-opened: %v", err)
	}
	if openedPayload.CampaignID != campaignID {
		t.Fatalf("campaign-opened names %q, want %q", openedPayload.CampaignID, campaignID)
	}

	declines := proposalDeclinedFor(t, emitted, "NEW")
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1, for NEW", len(declines))
	}
	decline := declines[0]
	if decline.CapLimit != cfg.MaxUnitsTotalLong {
		t.Errorf("decline.CapLimit = %d, want the journalled configuration's own MaxUnitsTotalLong %d", decline.CapLimit, cfg.MaxUnitsTotalLong)
	}
	// OLD's one committed Unit (journalled on campaign-opened, above), still
	// counted this Session (unit_caps.go), plus NEW's own proposed one.
	if decline.PostTradeExposure != 2 {
		t.Errorf("decline.PostTradeExposure = %d, want 2 (OLD's own Unit plus NEW's)", decline.PostTradeExposure)
	}
}
