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
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// This file holds the tests of ADR 0020's reservation at proposal, as
// amended 2026-09-24 (CONTEXT.md: "Hold"): a proposed entry or Add reserves
// its worst-case cost and one Unit of cap headroom until it fills, expires
// or is cancelled, and every later proposal is checked against what that
// leaves. The fixtures use unit_caps_test.go's compact breakout: every
// instrument's bars have the same shape, so every instrument's opening Unit
// sizes to the same quantity, level and N, and so places the same hold.

// probeCompactProposal runs one instrument's compact breakout under cfg and
// returns the proposal it makes: its quantity, entry level and N are every
// instrument's in this file's fixtures.
func probeCompactProposal(t *testing.T, cfg event.ConfigurationPayload) event.TradeProposalPayload {
	t.Helper()
	proposals := envelopesOfType(newStream(t, cfg).bars(compactEntryBars("PROBE", 0)).mustRun(), event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("probe: got %d trade proposal(s), want exactly 1", len(proposals))
	}
	return decodeTradeProposal(t, proposals[0])
}

// holdCashConfig is the compact fixture's configuration with every cap
// generous, so only cash can decline a Unit.
func holdCashConfig() event.ConfigurationPayload {
	return compactChannelConfig(1_000_000, 1_000_000, 1_000_000, 1_000_000)
}

// compactHold is the hold every compact-fixture entry places under cfg.
func compactHold(t *testing.T, cfg event.ConfigurationPayload) (proposal event.TradeProposalPayload, hold float64) {
	t.Helper()
	proposal = probeCompactProposal(t, cfg)
	return proposal, wantHold(cfg, proposal.Quantity, proposal.EntryLevel, proposal.N)
}

// quietCompactBar is a bar far below the compact fixture's Entry Channel: it
// signals nothing and, for an instrument with an outstanding proposal,
// expires it (ADR 0011).
func quietCompactBar(instrumentID string, periodEnd time.Time) event.CompletedBarPayload {
	return syntheticBar(instrumentID, periodEnd, 5)
}

// runTwoEntriesInOneSession breaks MMM and NNN out in one Session, with
// cash stated as available.
func runTwoEntriesInOneSession(t *testing.T, cfg event.ConfigurationPayload, available float64) []event.Envelope {
	t.Helper()
	return newStream(t, cfg).
		snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), available)).
		lockstep(compactEntryBars("MMM", 0), compactEntryBars("NNN", 0)).
		mustRun()
}

// TestTheSecondEntryOfASessionCloseIsDeclinedForTheFirstsHold is the
// headline case: two entries decided in one session-close pass, each of
// which the cash funds alone. The first, in rankSignals' order, places its
// hold, and the second is checked against the cash that hold leaves and
// declined, carrying exactly the figures compared.
func TestTheSecondEntryOfASessionCloseIsDeclinedForTheFirstsHold(t *testing.T) {
	t.Parallel()

	cfg := holdCashConfig()
	_, hold := compactHold(t, cfg)
	available := hold + hold/2
	emitted := runTwoEntriesInOneSession(t, cfg, available)

	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 || decodeTradeProposal(t, proposals[0]).InstrumentID != "MMM" {
		t.Fatalf("got %d trade proposal(s), want exactly 1, for MMM: the cash funds one Unit's hold, not two", len(proposals))
	}
	decline := onlyInsufficientCashDecline(t, emitted)
	if decline.InstrumentID != "NNN" {
		t.Fatalf("declined %q, want NNN", decline.InstrumentID)
	}
	if decline.RequiredCash != hold {
		t.Errorf("RequiredCash = %v, want NNN's hold %v", decline.RequiredCash, hold)
	}
	if want := available - hold; decline.AvailableCash != want {
		t.Errorf("AvailableCash = %v, want %v: the snapshot less MMM's standing hold", decline.AvailableCash, want)
	}
}

// TestTwoEntriesTheCashFundsTogetherAreBothProposed is the control the test
// above needs: with cash for both holds, both are proposed, so the decline
// above is the first hold's doing and not an unaffordable Unit.
func TestTwoEntriesTheCashFundsTogetherAreBothProposed(t *testing.T) {
	t.Parallel()

	cfg := holdCashConfig()
	_, hold := compactHold(t, cfg)
	emitted := runTwoEntriesInOneSession(t, cfg, 2*hold)
	if proposals := envelopesOfType(emitted, event.TradeProposalEventType); len(proposals) != 2 {
		t.Fatalf("got %d trade proposal(s), want 2: the cash funds both holds exactly", len(proposals))
	}
	if declines := envelopesOfType(emitted, event.ProposalDeclinedEventType); len(declines) != 0 {
		t.Fatalf("got %d decline(s), want 0", len(declines))
	}
}

// TestTheCheckAndTheHoldUseTheWorstCasePrice pins what a Unit must be able
// to fund under the Baseline (ADR 0005 and ADR 0020, as amended
// 2026-09-24): its quantity at its price cap, level + 1N, plus ADR 0013's
// slippage and commission at that price. Cash that covers the Unit at its
// level but not at that worst case declines it, and the declared Variant
// "uncapped", which checks the Unit at its level as the reducer did before,
// proposes it from the same cash, as a stop-market order with no cap.
func TestTheCheckAndTheHoldUseTheWorstCasePrice(t *testing.T) {
	t.Parallel()

	cfg := holdCashConfig()
	p, hold := compactHold(t, cfg)
	atLevel := float64(p.Quantity) * p.EntryLevel * cfg.DollarsPerPoint
	if hold <= atLevel+1 {
		t.Fatalf("fixture is wrong: the hold %v does not exceed the Unit's cost at its level %v", hold, atLevel)
	}
	available := atLevel + 1
	run := func(cfg event.ConfigurationPayload) []event.Envelope {
		return newStream(t, cfg).
			snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), available)).
			bars(compactEntryBars("MMM", 0)).
			mustRun()
	}

	decline := onlyInsufficientCashDecline(t, run(cfg))
	if decline.RequiredCash != hold || decline.AvailableCash != available {
		t.Errorf("RequiredCash/AvailableCash = %v/%v, want the worst-case hold %v against %v", decline.RequiredCash, decline.AvailableCash, hold, available)
	}

	uncapped := holdCashConfig()
	uncapped.BuyOrderType, uncapped.GapBufferN = event.OrderTypeStopMarket, 0
	proposals := envelopesOfType(run(uncapped), event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("uncapped: got %d trade proposal(s), want 1: the Variant checks the Unit at its level", len(proposals))
	}
	if q := decodeTradeProposal(t, proposals[0]); q.OrderType != event.OrderTypeStopMarket || q.PriceCap != 0 || q.GapBufferN != 0 {
		t.Errorf("uncapped proposal order = %q, cap %v, buffer %v; want a stop-market order with no cap", q.OrderType, q.PriceCap, q.GapBufferN)
	}
}

// TestAProposalCarriesItsPriceCap pins the Baseline proposal's order: a
// stop-limit at its level, capped at level + 1N, for an entry measured in
// the decision N and for an Add measured in the Campaign's frozen N.
func TestAProposalCarriesItsPriceCap(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	rung2, _, _ := addRungs(t, cfg)
	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(addOpportunityBar("AAPL", day(57), rung2+5)).
		mustRun()
	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("got %d trade proposal(s), want 1", len(proposals))
	}
	entry := decodeTradeProposal(t, proposals[0])
	if entry.OrderType != event.OrderTypeStopLimit || entry.GapBufferN != 1 || entry.PriceCap != wantPriceCap(cfg, entry.EntryLevel, entry.N) {
		t.Errorf("entry order = %q, buffer %v, cap %v; want a stop-limit capped at %v", entry.OrderType, entry.GapBufferN, entry.PriceCap, wantPriceCap(cfg, entry.EntryLevel, entry.N))
	}
	adds := envelopesOfType(emitted, event.AddProposalEventType)
	if len(adds) == 0 {
		t.Fatal("fixture is wrong: the bar after the opening fill reaches no Add rung")
	}
	add := decodeAddProposal(t, adds[0])
	if add.OrderType != event.OrderTypeStopLimit || add.PriceCap != wantPriceCap(cfg, add.Level, add.CampaignN) {
		t.Errorf("add order = %q, cap %v; want a stop-limit capped at %v", add.OrderType, add.PriceCap, wantPriceCap(cfg, add.Level, add.CampaignN))
	}
}

// TestAFilledProposalsHoldBecomesItsDebitAndIsNotCountedTwice: MMM's entry
// fills below its hold, and NNN, deciding in the next Session, is checked
// against the snapshot less MMM's fill's actual cost only. The hold is
// released as the fill is debited, so the reservation and the spend it
// became are never deducted together.
func TestAFilledProposalsHoldBecomesItsDebitAndIsNotCountedTwice(t *testing.T) {
	t.Parallel()

	cfg := holdCashConfig()
	p, hold := compactHold(t, cfg)
	mmm, nnn := compactEntryBars("MMM", 0), compactEntryBars("NNN", 1)
	fill := compactEntryFill("MMM", mmm[len(mmm)-1].PeriodEnd, "sim-fill-mmm", p.Quantity, p.EntryLevel)
	fill.Commission = 1
	spent := fillCost(cfg, fill)
	if spent >= hold {
		t.Fatalf("fixture is wrong: the fill's cost %v is not below its hold %v", spent, hold)
	}
	available := spent + hold - 0.01

	emitted := newStream(t, cfg).
		snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), available)).
		bar(mmm[0]).
		lockstep(mmm[1:], nnn[:len(nnn)-1]).
		fill(fill).
		bar(nnn[len(nnn)-1]).
		mustRun()

	if opened := envelopesOfType(emitted, event.CampaignOpenedEventType); len(opened) != 1 {
		t.Fatalf("got %d campaign(s) opened, want MMM's", len(opened))
	}
	decline := onlyInsufficientCashDecline(t, emitted)
	if decline.InstrumentID != "NNN" || decline.RequiredCash != hold {
		t.Fatalf("decline = %+v, want NNN's hold %v declined", decline, hold)
	}
	if want := available - spent; decline.AvailableCash != want {
		t.Errorf("AvailableCash = %v, want %v: the snapshot less MMM's fill's actual cost, with its hold released", decline.AvailableCash, want)
	}
}

// TestAnExpiredProposalReleasesItsHold: MMM's entry is proposed and never
// fills, and MMM's next bar expires it (ADR 0011). NNN breaks out in that
// same Session with cash for exactly one hold, and is proposed, because
// MMM's expiry released the cash its hold reserved.
func TestAnExpiredProposalReleasesItsHold(t *testing.T) {
	t.Parallel()

	cfg := holdCashConfig()
	_, hold := compactHold(t, cfg)
	mmm := append(compactEntryBars("MMM", 0), quietCompactBar("MMM", day(22)))
	nnn := compactEntryBars("NNN", 1)

	emitted := newStream(t, cfg).
		snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), hold)).
		bar(mmm[0]).
		lockstep(mmm[1:], nnn).
		mustRun()

	if expired := envelopesOfType(emitted, event.ProposalExpiredEventType); len(expired) != 1 {
		t.Fatalf("got %d expiries, want MMM's", len(expired))
	}
	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 2 {
		t.Fatalf("got %d trade proposal(s), want 2: MMM's, and then NNN's once MMM's expiry released its hold", len(proposals))
	}
	if declines := envelopesOfType(emitted, event.ProposalDeclinedEventType); len(declines) != 0 {
		t.Fatalf("got %d decline(s), want 0", len(declines))
	}
}

// TestASnapshotDoesNotReleaseAHold: MMM's entry is proposed and stays
// outstanding (MMM has no bar in the next Session), and a snapshot as of
// the previous close restates the cash before NNN is decided. The snapshot
// states the account's cash, which a resting order has not reduced, so
// MMM's hold still stands against it, and NNN is declined against the
// snapshot less that hold (ADR 0020, as amended 2026-09-24).
func TestASnapshotDoesNotReleaseAHold(t *testing.T) {
	t.Parallel()

	cfg := holdCashConfig()
	_, hold := compactHold(t, cfg)
	mmm, nnn := compactEntryBars("MMM", 0), compactEntryBars("NNN", 1)
	restated := hold + hold/2

	emitted := newStream(t, cfg).
		snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), 2*hold)).
		bar(mmm[0]).
		lockstep(mmm[1:], nnn[:len(nnn)-1]).
		snapshot(cashSnapshot(cfg, day(21), restated)).
		bar(nnn[len(nnn)-1]).
		mustRun()

	decline := onlyInsufficientCashDecline(t, emitted)
	if decline.InstrumentID != "NNN" {
		t.Fatalf("declined %q, want NNN", decline.InstrumentID)
	}
	if want := restated - hold; decline.AvailableCash != want {
		t.Errorf("AvailableCash = %v, want %v: the restated cash less MMM's hold, which the snapshot does not release", decline.AvailableCash, want)
	}
}

// TestReplayingAHoldFixtureTwiceYieldsByteIdenticalEmissions extends the
// replay-equivalence property to a run whose decisions depend on a hold:
// the reservation order is the session-close pass's own, so two runs agree
// byte for byte (ADR 0020's "Why the ledger is deterministic").
func TestReplayingAHoldFixtureTwiceYieldsByteIdenticalEmissions(t *testing.T) {
	t.Parallel()

	cfg := holdCashConfig()
	_, hold := compactHold(t, cfg)
	first := runTwoEntriesInOneSession(t, cfg, hold+hold/2)
	second := runTwoEntriesInOneSession(t, cfg, hold+hold/2)
	if len(envelopesOfType(first, event.ProposalDeclinedEventType)) != 1 {
		t.Fatal("fixture must decline a Unit for a hold for this property to mean anything")
	}
	a, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("two runs of one hold fixture emitted different bytes")
	}
}

// TestFilledExposureNeverExceedsAnyCapUnderSameSessionCompetition extends
// TestASingleProposalNeverTakesExposurePastAnyCap to the case holds exist
// for: several instruments break out in the SAME Session, and every
// proposal the reducer makes is filled at once. Filled exposure — per
// instrument, in the Unclassified Group and in total — never exceeds its
// cap, because each proposal reserves its Unit before the next is checked.
// Without the reservation, a Session with more Signals than headroom would
// propose, and fill, every one of them. The generator is seeded, so the
// sequence is the same on every run.
func TestFilledExposureNeverExceedsAnyCapUnderSameSessionCompetition(t *testing.T) {
	t.Parallel()

	const (
		maxUnits          = 1
		maxUnitsPerSector = 5
		maxUnitsTotalLong = 7
	)
	cfg := compactChannelConfig(maxUnits, 1_000_000, maxUnitsPerSector, maxUnitsTotalLong)
	p := probeCompactProposal(t, cfg)

	reducer, err := strategy.NewReducer(testStrategyVersion, cfg)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	var seq uint64
	nextSeq := func() uint64 { seq++; return seq }
	apply := func(envelope event.Envelope) []event.Envelope {
		t.Helper()
		out, err := reducer.Apply(context.Background(), envelope)
		if err != nil {
			t.Fatalf("Apply(%s) error = %v", envelope.Type, err)
		}
		return out
	}
	apply(configEnvelopeFor(t, cfg, nextSeq()))
	snapshot := defaultAccountSnapshot(cfg)
	apply(accountSnapshotEnvelope(t, nextSeq(), snapshot, snapshot.AsOf))

	rng := rand.New(rand.NewSource(20260924))
	filled, competingSessions, next, dayOffset := 0, 0, 0, 0
	for round := 0; round < 6; round++ {
		// Between two and four fresh instruments warm up together and break
		// out in one Session.
		var ids []string
		var series [][]event.CompletedBarPayload
		for range 2 + rng.Intn(3) {
			id := fmt.Sprintf("S%02d", next)
			next++
			ids = append(ids, id)
			series = append(series, compactEntryBars(id, dayOffset))
		}
		var closeEmitted []event.Envelope
		for i := range series[0] {
			for _, bars := range series {
				apply(barEnvelope(t, nextSeq(), bars[i], bars[i].PeriodEnd))
			}
			closeEmitted = apply(sessionClosedEnvelope(t, nextSeq(), series[0][i].PeriodEnd, ids))
		}
		periodEnd := series[0][len(series[0])-1].PeriodEnd
		dayOffset += len(series[0]) + 1

		capDeclines := 0
		for _, e := range envelopesOfType(closeEmitted, event.ProposalDeclinedEventType) {
			if decodeProposalDeclined(t, e).Reason == event.DeclineReasonUnitCapExceeded {
				capDeclines++
			}
		}
		proposed := envelopesOfType(closeEmitted, event.TradeProposalEventType)
		if len(proposed) > 0 && capDeclines > 0 {
			// A Session that both proposed and declined for a cap: the
			// competition this test exists for.
			competingSessions++
		}
		for _, e := range proposed {
			id := decodeTradeProposal(t, e).InstrumentID
			for _, out := range apply(fillEnvelope(t, nextSeq(), compactEntryFill(id, periodEnd, "fill-"+id, p.Quantity, p.EntryLevel))) {
				if out.Type == event.CampaignOpenedEventType {
					filled++
				}
			}
		}
		// Every instrument holds at most one Unit (maxUnits 1, no Adds
		// here), so the Unclassified Group and total long both hold one per
		// opened Campaign.
		if filled > maxUnitsPerSector || filled > maxUnitsTotalLong {
			t.Fatalf("after round %d: %d Units filled, exceeding the Unclassified Group cap %d or the total-long cap %d", round, filled, maxUnitsPerSector, maxUnitsTotalLong)
		}
	}
	if filled != maxUnitsPerSector {
		t.Fatalf("filled %d Units, want the Unclassified Group cap %d reached exactly: the fixture must fill up to the cap", filled, maxUnitsPerSector)
	}
	if competingSessions == 0 {
		t.Fatal("fixture error: no Session both proposed and declined for a cap, so same-Session competition was never exercised")
	}
}
