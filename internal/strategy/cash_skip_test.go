package strategy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// This file holds the cash-skip rule's tests (ADR 0010). When the next
// Unit would cost more than the cash available at the previous close, it is
// skipped — no partial Unit, no borrowing, no deferred queue — and the
// rejection is journalled with reason event.DeclineReasonInsufficientCash,
// carrying both the required and the available figure. A cost that leaves
// the float64 range is the same skip under
// event.DeclineReasonUnitCostNotRepresentable, which carries neither figure
// because the cost is the one that cannot be stated.
//
// Every fixture below builds on reducer_test.go's breakout fixture (entry
// level 155, quantity 133, so cost 133 x 155 = 20,615 exactly) and, for the
// Add Ladder cases, campaign_test.go's/add_test.go's Campaign fixture
// (campaignFillPrice 201.25, campaign N breakoutFixtureN(t, cfg), frozen
// Unit quantity 133). Cash figures are chosen to sit just either side of a
// Unit's cost, not comfortably clear of it: this project has twice been
// bitten by a fixture whose constants never exercised the boundary a rule
// actually depends on (docs/development.md's fusion-defect history is the
// same lesson applied to arithmetic; this is the sizing-outcome analogue).

// cashSkipEntryQuantity and cashSkipEntryLevel are the breakout fixture's own
// numbers (TestReducerEmitsTradeProposalOnSignal): 133 shares, entered at
// the Entry Channel high of 155 — the level a resting buy-stop actually
// sits at (ADR 0005), which is what a Unit's cost is computed from, not the
// breakout bar's own high of 200 and not the price that eventually fills.
const (
	cashSkipEntryQuantity int64   = 133
	cashSkipEntryLevel    float64 = 155
)

// cashSkipEntryCost mirrors sizeUnit's own arithmetic exactly (reducer.go):
// quantity x entry level x dollars per point, a bare product feeding only a
// comparison, never an addition or subtraction.
func cashSkipEntryCost(cfg event.ConfigurationPayload) float64 {
	return float64(cashSkipEntryQuantity) * cashSkipEntryLevel * cfg.DollarsPerPoint
}

// cashSnapshot builds an account.snapshot payload with Equity held at cfg's
// own starting figure (a no-op against the Notional Account, so nothing in
// this file's assertions is about ADR 0007) and the given AvailableCash —
// ADR 0010's cash basis.
func cashSnapshot(cfg event.ConfigurationPayload, asOf time.Time, availableCash float64) event.AccountSnapshotPayload {
	return event.AccountSnapshotPayload{
		AsOf:          asOf,
		Equity:        cfg.NotionalAccount.StartingEquity,
		AvailableCash: availableCash,
		Currency:      "USD",
	}
}

// --- The entry-side cases -------------------------------------------------

// TestAffordableUnitProposesNormally is the control: comfortable headroom
// above the Unit's cost proposes exactly as every other breakout fixture in
// this package does, and declines nothing.
func TestAffordableUnitProposesNormally(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	cost := cashSkipEntryCost(cfg)
	emitted := newStream(t, cfg).
		snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), cost+10_000)).
		bars(breakoutBars("AAPL")).
		mustRun()

	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("got %d trade proposal(s), want exactly 1", len(proposals))
	}
	if declines := envelopesOfType(emitted, event.ProposalDeclinedEventType); len(declines) != 0 {
		t.Fatalf("got %d decline(s), want 0: an affordable unit must not be declined", len(declines))
	}
	if quantity := decodeTradeProposal(t, proposals[0]).Quantity; quantity != cashSkipEntryQuantity {
		t.Errorf("Quantity = %d, want %d (the frozen unit size)", quantity, cashSkipEntryQuantity)
	}
}

// TestUnaffordableUnitIsDeclinedWithBothCashFigures pins the ticket's
// headline case: cash short by exactly one cent of the Unit's cost — not
// comfortably short — is not proposed, and the decline carries both figures
// exactly.
func TestUnaffordableUnitIsDeclinedWithBothCashFigures(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	cost := cashSkipEntryCost(cfg)
	available := cost - 0.01
	emitted := newStream(t, cfg).
		snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), available)).
		bars(breakoutBars("AAPL")).
		mustRun()

	if proposals := envelopesOfType(emitted, event.TradeProposalEventType); len(proposals) != 0 {
		t.Fatalf("got %d trade proposal(s), want 0: an unaffordable unit must never be proposed, partial or otherwise", len(proposals))
	}
	declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1", len(declines))
	}
	decline := decodeProposalDeclined(t, declines[0])
	if decline.Reason != event.DeclineReasonInsufficientCash {
		t.Errorf("Reason = %q, want %q", decline.Reason, event.DeclineReasonInsufficientCash)
	}
	if decline.Kind != event.ProposalDeclinedKindEntry {
		t.Errorf("Kind = %q, want %q", decline.Kind, event.ProposalDeclinedKindEntry)
	}
	if decline.CampaignID != "" {
		t.Errorf("CampaignID = %q, want empty: no campaign exists yet", decline.CampaignID)
	}
	if decline.RequiredCash != cost {
		t.Errorf("RequiredCash = %v, want exactly %v", decline.RequiredCash, cost)
	}
	if decline.AvailableCash != available {
		t.Errorf("AvailableCash = %v, want exactly %v", decline.AvailableCash, available)
	}
	if err := decline.Validate(); err != nil {
		t.Errorf("emitted decline fails its own Validate(): %v", err)
	}
}

// TestCostExactlyEqualToAvailableCashIsAffordable pins the boundary reading
// this ticket settles explicitly: you can spend exactly what you have. A
// fixture whose cash sits comfortably clear of the Unit's cost could not
// fail under the opposite reading (declining when cost >= available); this
// one is built to fail under it.
func TestCostExactlyEqualToAvailableCashIsAffordable(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	cost := cashSkipEntryCost(cfg)
	emitted := newStream(t, cfg).
		snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), cost)).
		bars(breakoutBars("AAPL")).
		mustRun()

	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("got %d trade proposal(s), want exactly 1: cost exactly equal to available cash is affordable", len(proposals))
	}
	if declines := envelopesOfType(emitted, event.ProposalDeclinedEventType); len(declines) != 0 {
		t.Fatalf("got %d decline(s), want 0", len(declines))
	}
}

// TestNoAccountSnapshotEverSuppliedFailsClosedOnEntry is the failure this
// ticket exists to prevent: a run that never delivers an account.snapshot
// must not size a Unit as though cash were infinite. Built without
// newStream, which always injects one.
func TestNoAccountSnapshotEverSuppliedFailsClosedOnEntry(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	reducer, err := strategy.NewReducer(testStrategyVersion, cfg)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	envelopes := []event.Envelope{configEnvelopeWithConfig(t, 1, day(0), cfg)}
	seq := uint64(2)
	for _, bar := range breakoutBars("AAPL") {
		envelopes = append(envelopes, barEnvelope(t, seq, bar, bar.PeriodEnd))
		seq++
	}

	_, err = engine.Run(context.Background(), envelopes)
	if err == nil {
		t.Fatal("Run() error = nil, want a fail-closed error: no account.snapshot was ever delivered")
	}
	for _, want := range []string{"account.snapshot", "available-cash", "0010"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Run() error = %v, want substring %q", err, want)
		}
	}
}

// --- The Add Ladder cases --------------------------------------------------

// cashSkipCampaignUnitQuantity is the campaign fixture's own frozen Unit
// size (campaign_test.go's campaignFillPrice fixture; add_test.go's fixtures
// all use the same 133).
const cashSkipCampaignUnitQuantity int64 = 133

// runCashSkipLadderFixture opens a Campaign, adds Unit 2 normally, then
// reaches Unit 3's rung with cash short by one cent of its cost — declined
// with reason insufficient-cash — and shows the skip does not poison the
// ladder: once cash recovers, the SAME rung is taken on its own merits on a
// later bar (ADR 0010's own wording), becoming Unit 3, and a later, higher
// rung is taken too, becoming Unit 4. Every rung is computed via
// sizing.NextAddLevel rather than hand-typed, so the fixture cannot silently
// drift from the arithmetic it exercises (ADR 0006).
func runCashSkipLadderFixture(t *testing.T) []event.Envelope {
	t.Helper()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)

	// Every rung measured from the PREVIOUS unit's actual fill, and every
	// fill in this fixture lands exactly on its own rung (no slippage),
	// which keeps the arithmetic in this file's assertions exact.
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(rung 2) error = %v", err)
	}
	rung3, err := sizing.NextAddLevel(rung2, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(rung 3) error = %v", err)
	}
	rung4, err := sizing.NextAddLevel(rung3, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(rung 4) error = %v", err)
	}

	// The exact cost of Unit 3's rung, mirroring evaluateAdd's own
	// arithmetic (campaign.go): frozen quantity x rung x dollars per point.
	cost3 := float64(cashSkipCampaignUnitQuantity) * rung3 * cfg.DollarsPerPoint
	cost4 := float64(cashSkipCampaignUnitQuantity) * rung4 * cfg.DollarsPerPoint

	// Short by exactly one cent of Unit 3's cost — comfortably above the
	// entry's own cost (20,615) and Unit 2's, so only Unit 3's rung is ever
	// declined.
	initialCash := cost3 - 0.01
	// Recovers to comfortably clear of Unit 4's own cost, so nothing later
	// in this fixture is gated by cash again.
	recoveredCash := cost4 + 10_000

	bar57 := addOpportunityBar("AAPL", day(57), rung2+5)
	fill2 := addFill("AAPL", campaignID, 2, day(57), "sim-fill-add-2", rung2, cashSkipCampaignUnitQuantity, day(57))
	bar58 := addOpportunityBar("AAPL", day(58), rung3+5) // reaches rung 3: declined
	bar59 := addOpportunityBar("AAPL", day(59), rung3+5) // the SAME rung, re-evaluated on its own merits
	fill3 := addFill("AAPL", campaignID, 3, day(59), "sim-fill-add-3", rung3, cashSkipCampaignUnitQuantity, day(59))
	bar60 := addOpportunityBar("AAPL", day(60), rung4+5) // a genuinely later rung
	fill4 := addFill("AAPL", campaignID, 4, day(60), "sim-fill-add-4", rung4, cashSkipCampaignUnitQuantity, day(60))

	return newStream(t, cfg).
		snapshot(cashSnapshot(cfg, day(0).Add(time.Hour), initialCash)).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(bar57).
		fill(fill2).
		bar(bar58).
		snapshot(cashSnapshot(cfg, day(58), recoveredCash)).
		bar(bar59).
		fill(fill3).
		bar(bar60).
		fill(fill4).
		mustRun()
}

// TestUnaffordableThirdRungFollowedByAnAffordableFourth is the ticket's
// ladder case: Unit 3's rung is declined for insufficient cash, and a LATER
// rung (Unit 4's) is still taken — proving a skip does not end the ladder
// (ADR 0010: "the next rung is evaluated on its own merits when price
// reaches it").
func TestUnaffordableThirdRungFollowedByAnAffordableFourth(t *testing.T) {
	t.Parallel()

	emitted := runCashSkipLadderFixture(t)

	declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1 (unit 3's first attempt)", len(declines))
	}
	decline := decodeProposalDeclined(t, declines[0])
	if decline.Reason != event.DeclineReasonInsufficientCash {
		t.Errorf("Reason = %q, want %q", decline.Reason, event.DeclineReasonInsufficientCash)
	}
	if decline.Kind != event.ProposalDeclinedKindAdd {
		t.Errorf("Kind = %q, want %q", decline.Kind, event.ProposalDeclinedKindAdd)
	}
	if decline.SignalID != "" {
		t.Errorf("SignalID = %q, want empty: an add proposal answers no signal", decline.SignalID)
	}
	wantCampaignID := testDecisionID("campaign", "AAPL", day(56))
	if decline.CampaignID != wantCampaignID {
		t.Errorf("CampaignID = %q, want %q", decline.CampaignID, wantCampaignID)
	}
	if decline.RequiredCash <= decline.AvailableCash {
		t.Errorf("RequiredCash %v does not exceed AvailableCash %v", decline.RequiredCash, decline.AvailableCash)
	}

	// Units 2, 3 and 4 all eventually join the campaign: the skip cost the
	// ladder nothing but the one declined attempt.
	added := envelopesOfType(emitted, event.CampaignUnitAddedEventType)
	if len(added) != 3 {
		t.Fatalf("got %d unit-added event(s), want exactly 3 (units 2, 3 and 4)", len(added))
	}
	var unitIndexes []int
	for _, a := range added {
		unitIndexes = append(unitIndexes, decodeCampaignUnitAdded(t, a).UnitIndex)
	}
	for i, want := range []int{2, 3, 4} {
		if unitIndexes[i] != want {
			t.Errorf("unit-added[%d].UnitIndex = %d, want %d", i, unitIndexes[i], want)
		}
	}

	// Exactly two Add proposals succeeded (units 2 and 4's first attempt,
	// and unit 3's SECOND attempt) plus the one that was declined: three Add
	// opportunities were evaluated for unit 3's rung and unit 4's rung and
	// unit 2's rung combined, only one of which was ever declined.
	proposed := envelopesOfType(emitted, event.AddProposalEventType)
	if len(proposed) != 3 {
		t.Fatalf("got %d add proposal(s), want exactly 3 (units 2, 3 and 4's successful proposals)", len(proposed))
	}
}

// TestNoPartialUnitIsEverEmittedUnderAnyCashSkipFixture scans every
// proposal-shaped envelope this file's fixtures produce and confirms each
// one's Quantity is the frozen Unit size exactly, never a fraction of it:
// ADR 0010 forbids a partial Unit under every outcome, not only the
// affordable one.
func TestNoPartialUnitIsEverEmittedUnderAnyCashSkipFixture(t *testing.T) {
	t.Parallel()

	emitted := runCashSkipLadderFixture(t)

	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("got %d trade proposal(s), want exactly 1", len(proposals))
	}
	if quantity := decodeTradeProposal(t, proposals[0]).Quantity; quantity != cashSkipCampaignUnitQuantity {
		t.Errorf("entry Quantity = %d, want %d (the frozen unit size)", quantity, cashSkipCampaignUnitQuantity)
	}

	for _, e := range envelopesOfType(emitted, event.AddProposalEventType) {
		if quantity := decodeAddProposal(t, e).Quantity; quantity != cashSkipCampaignUnitQuantity {
			t.Errorf("add proposal %s Quantity = %d, want %d (the frozen unit size — an Add is never resized)", e.ID, quantity, cashSkipCampaignUnitQuantity)
		}
	}
}

// TestByteIdenticalReplayOfAFixtureContainingASkip extends this project's
// replay-equivalence property (e.g.
// TestReplayingProposalFixtureTwiceYieldsByteIdenticalEmissions) to a
// fixture that contains a cash-skip decline: two independent runs must
// produce byte-identical envelopes, proving the cash check introduces no
// wall-clock read, no randomness and no map-iteration-order dependence.
func TestByteIdenticalReplayOfAFixtureContainingASkip(t *testing.T) {
	t.Parallel()

	first := runCashSkipLadderFixture(t)
	second := runCashSkipLadderFixture(t)

	if len(envelopesOfType(first, event.ProposalDeclinedEventType)) == 0 {
		t.Fatal("fixture must contain at least one decline for this property to mean anything")
	}
	if len(first) != len(second) {
		t.Fatalf("len(first) = %d, len(second) = %d", len(first), len(second))
	}
	for i := range first {
		a, err := json.Marshal(first[i])
		if err != nil {
			t.Fatalf("Marshal(first[%d]) error = %v", i, err)
		}
		b, err := json.Marshal(second[i])
		if err != nil {
			t.Fatalf("Marshal(second[%d]) error = %v", i, err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("emission %d differs between runs:\n  first:  %s\n  second: %s", i, a, b)
		}
	}
}

// --- The cash basis is the PREVIOUS close's, and only that ----------------

// cashSkipGenerousCash is comfortably clear of every cost in this file's
// fixtures, used where the subject is WHICH snapshot may be spent rather than
// how much it holds.
const cashSkipGenerousCash = 1_000_000_000.0

// TestSnapshotDatedAfterTheDecisionBarIsRefusedOnEntry is ADR 0010's cash
// basis at the seam it is actually decided: the cash a bar may spend is the
// cash known at the PREVIOUS close. A snapshot stamped after the decision bar
// itself reports cash that did not exist when the bar opened — the exact
// look-ahead ADR 0010 forbids — so the run fails closed rather than sizing a
// Unit against it.
func TestSnapshotDatedAfterTheDecisionBarIsRefusedOnEntry(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	bars := breakoutBars("AAPL")
	newStream(t, cfg).
		bars(bars[:len(bars)-1]).
		snapshot(cashSnapshot(cfg, day(56).Add(time.Hour), cashSkipGenerousCash)).
		bar(bars[len(bars)-1]).
		wantRunError("AAPL", "previous close", "0010")
}

// TestSnapshotDatedAtThePreviousCloseIsSpendable is the control the test above
// needs: the same stream, with the same figure stamped AT the previous close
// instead of after the decision bar, proposes normally. Without it, a reducer
// that refused every mid-stream snapshot would satisfy the test above.
func TestSnapshotDatedAtThePreviousCloseIsSpendable(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	bars := breakoutBars("AAPL")
	emitted := newStream(t, cfg).
		bars(bars[:len(bars)-1]).
		snapshot(cashSnapshot(cfg, day(55), cashSkipGenerousCash)).
		bar(bars[len(bars)-1]).
		mustRun()

	if proposals := envelopesOfType(emitted, event.TradeProposalEventType); len(proposals) != 1 {
		t.Fatalf("got %d trade proposal(s), want exactly 1: cash known at the previous close is spendable", len(proposals))
	}
	if declines := envelopesOfType(emitted, event.ProposalDeclinedEventType); len(declines) != 0 {
		t.Fatalf("got %d decline(s), want 0", len(declines))
	}
}

// TestSnapshotDatedAfterTheDecisionBarIsRefusedOnAnAdd is the same rule on the
// Add Ladder: an open Campaign's rung is sized against the cash known at the
// previous close too, so a snapshot stamped after the bar the rung is reached
// on fails the run closed rather than funding the Add.
func TestSnapshotDatedAfterTheDecisionBarIsRefusedOnAnAdd(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignN := breakoutFixtureN(t, cfg)
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(rung 2) error = %v", err)
	}

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		snapshot(cashSnapshot(cfg, day(57).Add(time.Hour), cashSkipGenerousCash)).
		bar(addOpportunityBar("AAPL", day(57), rung2+5)).
		wantRunError("AAPL", "previous close", "0010")
}

// --- A cost that leaves the float64 range is still a skip -----------------

// The fixture below is deliberately extreme, and every figure in it is
// admissible: no rule caps the Notional Account, a contract multiplier is a
// configured parameter (The Turtle Rules p.15 needs 42,000 for Heating Oil),
// and a bar's prices are required to be finite and positive and nothing more.
// Three individually finite operands can still multiply past the float64
// range, and the product is then +Inf — a figure no journal can record, since
// JSON cannot even encode it. The Unit costs more than any cash that can
// exist, so ADR 0010 skips it; what must not happen is the run halting on a
// payload it built itself.
const (
	// overflowChannelHigh and overflowTrueRange give an Entry Channel high
	// 100,000 times N: wide enough that quantity x level x dollars per point
	// overflows while the Protective Stop intent (level - 2N) stays a
	// reachable price.
	overflowChannelHigh = 1e11
	overflowTrueRange   = 1e6
)

// cashSkipOverflowConfiguration is the Baseline fixture with an account and a
// contract multiplier large enough that a whole Unit's cost leaves the
// float64 range, while the quantity itself stays far under sizing's own
// exactly-representable limit.
func cashSkipOverflowConfiguration() event.ConfigurationPayload {
	cfg := validConfigurationPayload()
	cfg.NotionalAccount.StartingEquity = 3.6e307
	cfg.DollarsPerPoint = 1e290
	return cfg
}

// cashSkipOverflowBars warms the Entry Channel to overflowChannelHigh with a
// True Range of exactly overflowTrueRange on every bar — so N is exactly
// overflowTrueRange — and then breaks out above it.
func cashSkipOverflowBars(instrumentID string) []event.CompletedBarPayload {
	low := overflowChannelHigh - overflowTrueRange
	bars := make([]event.CompletedBarPayload, 0, 56)
	for i := 1; i <= 55; i++ {
		bars = append(bars, completedBar(instrumentID, day(i), overflowChannelHigh, low, low))
	}
	return append(bars, completedBar(instrumentID, day(56), 2*overflowChannelHigh, low, low))
}

// TestEntryCostBeyondTheRepresentableRangeIsSkippedNotHalted pins the
// promised decline behaviour for inputs the reducer accepted: a Unit whose
// cost overflows float64 is skipped and journalled, never recorded as +Inf
// and never allowed to stop the run.
func TestEntryCostBeyondTheRepresentableRangeIsSkippedNotHalted(t *testing.T) {
	t.Parallel()

	cfg := cashSkipOverflowConfiguration()
	envelopes := []event.Envelope{accountSnapshotEnvelopeFor(t, cfg, 2, cashSnapshot(cfg, day(0), cashSkipGenerousCash))}
	for i, bar := range cashSkipOverflowBars("AAPL") {
		envelopes = append(envelopes, barEnvelopeFor(t, cfg, uint64(i+3), bar))
	}

	emitted, err := runReducerOverAccountEvents(t, cfg, envelopes)
	if err != nil {
		t.Fatalf("Run() error = %v, want the unaffordable unit journalled as a skip", err)
	}
	if proposals := envelopesOfType(emitted, event.TradeProposalEventType); len(proposals) != 0 {
		t.Fatalf("got %d trade proposal(s), want 0", len(proposals))
	}
	declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1", len(declines))
	}
	decline := decodeProposalDeclined(t, declines[0])
	if decline.Reason != event.DeclineReasonUnitCostNotRepresentable {
		t.Errorf("Reason = %q, want %q", decline.Reason, event.DeclineReasonUnitCostNotRepresentable)
	}
	if decline.Kind != event.ProposalDeclinedKindEntry {
		t.Errorf("Kind = %q, want %q", decline.Kind, event.ProposalDeclinedKindEntry)
	}
	if decline.RequiredCash != 0 || decline.AvailableCash != 0 {
		t.Errorf("RequiredCash/AvailableCash = %v/%v, want 0/0: the cost is the one figure that cannot be stated", decline.RequiredCash, decline.AvailableCash)
	}
	if err := decline.Validate(); err != nil {
		t.Errorf("emitted decline fails its own Validate(): %v", err)
	}
}

// TestAddCostBeyondTheRepresentableRangeIsSkippedNotHalted is the same
// property on the Add Ladder: a Campaign opened at an affordable cost can
// still reach a rung whose own cost overflows, since the ladder is measured
// from the ACTUAL fill rather than from the level that was proposed.
func TestAddCostBeyondTheRepresentableRangeIsSkippedNotHalted(t *testing.T) {
	t.Parallel()

	// The entry is affordable at the Entry Channel high, but the ladder is
	// measured from the ACTUAL fill (The Turtle Rules p.19), and this Unit
	// fills ten orders of magnitude above the level it rested at — slippage
	// no rule caps. Rung 2 therefore lands where the same frozen quantity
	// costs more than float64 can state, while the entry's own cost was a
	// perfectly ordinary number.
	cfg := cashSkipOverflowConfiguration()
	cfg.NotionalAccount.StartingEquity = 2e297
	cfg.DollarsPerPoint = 1e280

	campaignID := testDecisionID("campaign", "AAPL", day(56))
	quantity, err := sizing.UnitQuantity(cfg.NotionalAccount.StartingEquity, cfg.UnitVolatilityFraction, overflowTrueRange, cfg.DollarsPerPoint)
	if err != nil {
		t.Fatalf("sizing.UnitQuantity() error = %v", err)
	}
	if entryCost := float64(quantity) * overflowChannelHigh * cfg.DollarsPerPoint; math.IsInf(entryCost, 0) {
		t.Fatalf("fixture is wrong: the ENTRY cost overflowed too, so this test would prove nothing about the Add path")
	}

	const fillPrice = 1e21
	rung2, err := sizing.NextAddLevel(fillPrice, overflowTrueRange, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(rung 2) error = %v", err)
	}

	opening := event.FillPayload{
		InstrumentID: "AAPL",
		Kind:         event.FillKindEntry,
		ProposalID:   testDecisionID("proposal", "AAPL", day(56)),
		FillID:       "sim-fill-0001",
		Direction:    event.DirectionLong,
		Quantity:     quantity,
		Price:        fillPrice,
		FilledAt:     day(56),
	}

	envelopes := []event.Envelope{accountSnapshotEnvelopeFor(t, cfg, 2, cashSnapshot(cfg, day(0), math.MaxFloat64))}
	seq := uint64(3)
	for _, bar := range cashSkipOverflowBars("AAPL") {
		envelopes = append(envelopes, barEnvelopeFor(t, cfg, seq, bar))
		seq++
	}
	envelopes = append(envelopes, fillEnvelopeFor(t, cfg, seq, opening))
	seq++
	envelopes = append(envelopes, barEnvelopeFor(t, cfg, seq, completedBar("AAPL", day(57), rung2, fillPrice-overflowTrueRange, fillPrice-overflowTrueRange)))

	emitted, err := runReducerOverAccountEvents(t, cfg, envelopes)
	if err != nil {
		t.Fatalf("Run() error = %v, want the unaffordable rung journalled as a skip", err)
	}

	if opened := envelopesOfType(emitted, event.CampaignOpenedEventType); len(opened) != 1 {
		t.Fatalf("got %d campaign-opened event(s), want exactly 1: the entry itself must be affordable", len(opened))
	}
	declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1", len(declines))
	}
	decline := decodeProposalDeclined(t, declines[0])
	if decline.Reason != event.DeclineReasonUnitCostNotRepresentable {
		t.Errorf("Reason = %q, want %q", decline.Reason, event.DeclineReasonUnitCostNotRepresentable)
	}
	if decline.Kind != event.ProposalDeclinedKindAdd {
		t.Errorf("Kind = %q, want %q", decline.Kind, event.ProposalDeclinedKindAdd)
	}
	if decline.CampaignID != campaignID {
		t.Errorf("CampaignID = %q, want %q", decline.CampaignID, campaignID)
	}
	if decline.RequiredCash != 0 || decline.AvailableCash != 0 {
		t.Errorf("RequiredCash/AvailableCash = %v/%v, want 0/0", decline.RequiredCash, decline.AvailableCash)
	}
	if proposals := envelopesOfType(emitted, event.AddProposalEventType); len(proposals) != 0 {
		t.Fatalf("got %d add proposal(s), want 0", len(proposals))
	}
	if err := decline.Validate(); err != nil {
		t.Errorf("emitted decline fails its own Validate(): %v", err)
	}
}
