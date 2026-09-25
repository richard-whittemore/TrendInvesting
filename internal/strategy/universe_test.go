package strategy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// This file holds ADR 0009's tests: a declared classification fact
// (market.instrument-classification), the monthly, point-in-time
// eligibility evaluation it feeds (strategy.universe.eligibility), and the
// one rule that matters most — losing eligibility never closes an open
// Campaign, and never touches an Add either; it only stops a NEW Campaign
// from being proposed (event.DeclineReasonIneligible).
//
// day(i) (reducer_test.go) is 2026-01-02 + i days, so day(29) is
// 2026-01-31 and day(30) is 2026-02-01: the first trading day of February
// falls naturally inside this package's existing fixtures without any
// special-cased calendar arithmetic.

// universeOn sets cfg's three universe thresholds to a small, positive
// triple, turning ADR 0009's gate on (as amended 2026-09-25, the owner's
// decision: all three must switch together, never a partial mix) with a
// floor low enough for this package's compact fixtures to clear on their own
// price and dollar volume.
func universeOn(cfg *event.ConfigurationPayload) {
	cfg.UniverseMinPrice = 1
	cfg.UniverseMinDollarVolume = 1
	cfg.UniverseMinHistoryBars = 1
}

func classificationEnvelope(t *testing.T, sequence uint64, payload event.InstrumentClassificationPayload) event.Envelope {
	t.Helper()
	marshalled := mustMarshal(t, payload)
	return event.Envelope{
		ID:                fmt.Sprintf("classification-%d", sequence),
		Type:              event.MarketInstrumentClassificationEventType,
		SchemaVersion:     event.MarketInstrumentClassificationSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         payload.EffectiveAt,
		RecordedAt:        payload.EffectiveAt,
		Sequence:          sequence,
		Source:            "fixture",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(marshalled),
		Payload:           marshalled,
	}
}

// classify appends a market.instrument-classification input, mirroring
// stream's own fill/bar methods.
func (s *stream) classify(payload event.InstrumentClassificationPayload) *stream {
	s.seq++
	s.envelopes = append(s.envelopes, classificationEnvelope(s.t, s.seq, payload))
	return s
}

func eligibleClassification(instrumentID string, effectiveAt time.Time) event.InstrumentClassificationPayload {
	return event.InstrumentClassificationPayload{
		InstrumentID:      instrumentID,
		SecurityType:      event.SecurityTypeCommonStock,
		USPrimaryExchange: true,
		EffectiveAt:       effectiveAt,
	}
}

// etfClassification declares instrumentID an ETF: it fails ADR 0009's
// classification criterion regardless of price, dollar volume or history.
func etfClassification(instrumentID string, effectiveAt time.Time) event.InstrumentClassificationPayload {
	return event.InstrumentClassificationPayload{
		InstrumentID:      instrumentID,
		SecurityType:      event.SecurityTypeETF,
		USPrimaryExchange: true,
		EffectiveAt:       effectiveAt,
	}
}

func decodeUniverseEligibility(t *testing.T, envelope event.Envelope) event.UniverseEligibilityPayload {
	t.Helper()
	if envelope.Type != event.UniverseEligibilityEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.UniverseEligibilityEventType)
	}
	var payload event.UniverseEligibilityPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload
}

// universeEligibilityFor returns instrumentID's eligibility decision(s), in
// emission order.
func universeEligibilityFor(t *testing.T, envelopes []event.Envelope, instrumentID string) []event.UniverseEligibilityPayload {
	t.Helper()
	var out []event.UniverseEligibilityPayload
	for _, e := range envelopesOfType(envelopes, event.UniverseEligibilityEventType) {
		p := decodeUniverseEligibility(t, e)
		if p.InstrumentID == instrumentID {
			out = append(out, p)
		}
	}
	return out
}

// TestMonthlyEvaluationEmitsOneEligibilityDecisionPerInstrumentPerMonth
// covers the ticket's "a month boundary" case directly: an instrument
// classified once is evaluated at the very first Session (no earlier one
// exists that month), not again for every quiet bar in the same month, and
// again exactly once at the next month's first trading day.
func TestMonthlyEvaluationEmitsOneEligibilityDecisionPerInstrumentPerMonth(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	universeOn(&cfg)

	quiet := func(d int) event.CompletedBarPayload { return completedBar("QQQQ", day(d), 150, 150, 150) }

	s := newStream(t, cfg).classify(eligibleClassification("QQQQ", day(0)))
	for d := 1; d <= 30; d++ { // day(1)..day(29) is January; day(30) is 2026-02-01.
		s.bar(quiet(d))
	}
	emitted := s.mustRun()

	decisions := universeEligibilityFor(t, emitted, "QQQQ")
	if len(decisions) != 2 {
		t.Fatalf("got %d universe.eligibility decision(s) for QQQQ across one month boundary, want exactly 2 (one per month)", len(decisions))
	}
	if !decisions[0].PeriodEnd.Equal(day(1)) {
		t.Errorf("first evaluation PeriodEnd = %s, want day(1) %s (the run's very first Session)", decisions[0].PeriodEnd, day(1))
	}
	if !decisions[1].PeriodEnd.Equal(day(30)) {
		t.Errorf("second evaluation PeriodEnd = %s, want day(30) %s (February's first trading day)", decisions[1].PeriodEnd, day(30))
	}
	// January's evaluation runs on QQQQ's very first bar: only one raw
	// close/volume pair exists, short of indicator.DollarVolumeWindow (20),
	// so the dollar-volume window is not yet ready and this decision is
	// ineligible on that criterion alone, whatever UniverseMinHistoryBars
	// says.
	if decisions[0].Eligible {
		t.Error("January's decision: Eligible = true, want false (the 20-bar dollar-volume window cannot be ready on the very first bar)")
	}
	if decisions[0].DollarVolumeEligible {
		t.Error("January's decision: DollarVolumeEligible = true, want false")
	}
	// By February's evaluation, 30 completed bars have fed the window, so
	// it is ready and comfortably above the Baseline's $5,000,000 floor.
	if !decisions[1].Eligible {
		t.Errorf("February's decision: Eligible = false, want true (%+v)", decisions[1])
	}
}

// TestWithTheUniverseGateOffAnUnclassifiedInstrumentIsNeverGated proves the
// OFF half of ADR 0009's amendment of 2026-09-25 (the owner's decision):
// with every threshold at zero (validConfigurationPayload's own default),
// an instrument this run's universe port has never classified is left
// completely ungated (never evaluated, never declined for ineligibility) —
// what every pre-existing fixture, golden journal and decision-corpus
// scenario in this whole repository runs under, since none of them turns
// the gate on or classifies anything at all.
func TestWithTheUniverseGateOffAnUnclassifiedInstrumentIsNeverGated(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		mustRun()

	if decisions := universeEligibilityFor(t, emitted, "AAPL"); len(decisions) != 0 {
		t.Fatalf("got %d universe.eligibility decision(s) with the gate off, want 0", len(decisions))
	}
	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("got %d trade proposal(s) for an unclassified instrument's breakout with the gate off, want exactly 1", len(proposals))
	}
	declined := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(declined) != 0 {
		t.Fatalf("got %d decline(s) for an unclassified instrument with the gate off, want 0", len(declined))
	}
}

// TestWithTheUniverseGateOnAnUnclassifiedInstrumentIsDeclined proves the ON
// half: turning the gate on (universeOn) makes an unclassified instrument's
// entry Signal declined ineligible, with a detail naming why, rather than
// merely ungated (ADR 0009, as amended 2026-09-25, the owner's decision:
// "an instrument that is unclassified ... is declined as ineligible").
func TestWithTheUniverseGateOnAnUnclassifiedInstrumentIsDeclined(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	universeOn(&cfg)

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		mustRun()

	if proposals := envelopesOfType(emitted, event.TradeProposalEventType); len(proposals) != 0 {
		t.Fatalf("got %d trade proposal(s) for an unclassified instrument with the gate on, want 0", len(proposals))
	}
	declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1", len(declines))
	}
	decline := decodeProposalDeclined(t, declines[0])
	if decline.Reason != event.DeclineReasonIneligible {
		t.Errorf("decline.Reason = %q, want %q", decline.Reason, event.DeclineReasonIneligible)
	}
	if !strings.Contains(decline.Detail, "never classified") {
		t.Errorf("decline.Detail = %q, want it to name that the instrument was never classified", decline.Detail)
	}
}

// TestTheRunsFirstSessionCloseEvaluatesAClassifiedInstrumentAtOnce covers
// the ticket's own "no month-long wait" case for a run's very first
// Session: an instrument classified before the run's first bar is evaluated
// at that very first Session close, not held for a month boundary that, for
// a run starting mid-month, could be weeks away.
func TestTheRunsFirstSessionCloseEvaluatesAClassifiedInstrumentAtOnce(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	universeOn(&cfg)
	// day(15) is 2026-01-17, deliberately mid-month: a purely monthly
	// cadence would not evaluate anything until day(30) (2026-02-01), more
	// than two weeks later.
	start := 15
	emitted := newStream(t, cfg).
		classify(eligibleClassification("AAPL", day(0))).
		bar(completedBar("AAPL", day(start), 150, 150, 150)).
		mustRun()

	decisions := universeEligibilityFor(t, emitted, "AAPL")
	if len(decisions) != 1 {
		t.Fatalf("got %d decision(s), want exactly 1", len(decisions))
	}
	if !decisions[0].PeriodEnd.Equal(day(start)) {
		t.Errorf("decision PeriodEnd = %s, want the run's very first Session %s, not a later month boundary", decisions[0].PeriodEnd, day(start))
	}
}

// TestAMidMonthClassificationIsEvaluatedAtTheNextSessionClose covers the
// ticket's own "mid-month classification evaluated at next close" case: an
// instrument classified after a run is already under way, off any calendar
// month boundary, is evaluated at the very next Session close rather than
// waiting for the next first-of-month.
func TestAMidMonthClassificationIsEvaluatedAtTheNextSessionClose(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	universeOn(&cfg)

	// day(1) opens the run (no classification yet, so nothing to evaluate);
	// day(10) and day(11) are both comfortably inside January, nowhere near
	// day(30)'s February boundary.
	emitted := newStream(t, cfg).
		bar(completedBar("AAPL", day(1), 150, 150, 150)).
		classify(eligibleClassification("AAPL", day(1).Add(time.Hour))).
		bar(completedBar("AAPL", day(10), 150, 150, 150)).
		bar(completedBar("AAPL", day(11), 150, 150, 150)).
		mustRun()

	decisions := universeEligibilityFor(t, emitted, "AAPL")
	if len(decisions) != 1 {
		t.Fatalf("got %d decision(s), want exactly 1", len(decisions))
	}
	if !decisions[0].PeriodEnd.Equal(day(10)) {
		t.Errorf("decision PeriodEnd = %s, want day(10) %s: the Session immediately after the classification arrived, not day(11) or a month boundary", decisions[0].PeriodEnd, day(10))
	}
}

// TestNewReducerRefusesAPartialUniverseConfiguration is the reducer-seam
// counterpart of internal/event's own ConfigurationPayload.Validate tests:
// NewReducer validates the payload it is given, so a partial universe
// configuration is refused before any input is ever applied.
func TestNewReducerRefusesAPartialUniverseConfiguration(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	cfg.UniverseMinPrice = 5 // dollar volume and history bars left at zero

	_, err := strategy.NewReducer(testStrategyVersion, cfg)
	if err == nil || !strings.Contains(err.Error(), "universe thresholds must be either all zero") {
		t.Fatalf("NewReducer() error = %v, want it to name the partial universe configuration", err)
	}
}

// TestAnEligibleInstrumentsSignalProposesNormally proves the positive path
// end to end: classified common stock on a US primary exchange, evaluated
// eligible at the first trading day of its month, and its Tier A Signal is
// sized into an ordinary trade proposal — not merely "ungated" as the
// unclassified case above is, but actually evaluated and found eligible.
func TestAnEligibleInstrumentsSignalProposesNormally(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	universeOn(&cfg)

	emitted := newStream(t, cfg).
		classify(eligibleClassification("AAPL", day(0))).
		bars(breakoutBars("AAPL")).
		mustRun()

	decisions := universeEligibilityFor(t, emitted, "AAPL")
	if len(decisions) == 0 {
		t.Fatal("got 0 universe.eligibility decisions for a classified instrument, want at least 1")
	}
	// The LAST evaluation before the breakout is what gates it (ADR 0009 is
	// re-evaluated monthly, not on every bar): breakoutBars crosses one
	// month boundary (day(30), 2026-02-01) before its day(56) breakout, by
	// which point 30 completed bars have filled the 20-bar dollar-volume
	// window comfortably above the Baseline's floor.
	last := decisions[len(decisions)-1]
	if !last.Eligible {
		t.Errorf("last evaluation before the breakout (at %s): Eligible = false, want true (%+v)", last.PeriodEnd, last)
	}
	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("got %d trade proposal(s), want exactly 1 (an eligible instrument's breakout must still be sized)", len(proposals))
	}
}

// TestAnIneligibleInstrumentsSignalIsDeclinedInsteadOfProposed covers the
// ticket's own "each criterion excluding an instrument" list at the
// reducer seam: BBB is declared an ETF before its own first evaluation, so
// ADR 0009's classification criterion excludes it regardless of its price,
// dollar volume or history all otherwise qualifying — its Tier A Signal is
// declined (event.DeclineReasonIneligible) rather than sized, with Strength
// exactly zero (ranking never ran for it, the identical discipline
// DeclineReasonInsufficientHistory already follows).
func TestAnIneligibleInstrumentsSignalIsDeclinedInsteadOfProposed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	universeOn(&cfg)

	emitted := newStream(t, cfg).
		classify(etfClassification("BBB", day(0))).
		bars(breakoutBars("BBB")).
		mustRun()

	decisions := universeEligibilityFor(t, emitted, "BBB")
	if len(decisions) == 0 {
		t.Fatal("got 0 universe.eligibility decisions for BBB, want at least 1")
	}
	for _, d := range decisions {
		if d.Eligible {
			t.Errorf("decision at %s: Eligible = true, want false (BBB is an ETF)", d.PeriodEnd)
		}
		if d.ClassificationEligible {
			t.Errorf("decision at %s: ClassificationEligible = true, want false", d.PeriodEnd)
		}
		// Price and dollar volume, unaffected by the classification failure,
		// are still reported eligible: only one criterion excludes BBB.
		if !d.PriceEligible {
			t.Errorf("decision at %s: PriceEligible = false, want true (only classification should exclude BBB)", d.PeriodEnd)
		}
	}

	if proposals := envelopesOfType(emitted, event.TradeProposalEventType); len(proposals) != 0 {
		t.Fatalf("got %d trade proposal(s) for an ineligible instrument, want 0", len(proposals))
	}
	if opened := envelopesOfType(emitted, event.CampaignOpenedEventType); len(opened) != 0 {
		t.Fatalf("got %d campaign(s) opened for an ineligible instrument, want 0", len(opened))
	}
	declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1", len(declines))
	}
	decline := decodeProposalDeclined(t, declines[0])
	if decline.Reason != event.DeclineReasonIneligible {
		t.Errorf("decline.Reason = %q, want %q", decline.Reason, event.DeclineReasonIneligible)
	}
	if decline.Strength != 0 {
		t.Errorf("decline.Strength = %v, want exactly 0 (ranking never ran for an instrument excluded before rankSignals)", decline.Strength)
	}
	if decline.Kind != event.ProposalDeclinedKindEntry {
		t.Errorf("decline.Kind = %q, want %q", decline.Kind, event.ProposalDeclinedKindEntry)
	}
}

// TestLosingEligibilityNeverClosesAnOpenCampaignOrBlocksItsAdds is this
// ticket's central rule, tested through the full reducer seam: CCC opens a
// Campaign while eligible, ADR 0009 finds it ineligible at the next month's
// first trading day (2026-03-01, day(58) — see add_test.go's
// addOpportunityBar/breakoutBars fixtures, whose day(58) rung already falls
// there), and:
//
//   - the open Campaign is never closed by the eligibility decision alone
//     (no strategy.campaign.exited appears anywhere in the run);
//   - the very next Add rung, reached in the SAME Session the ineligible
//     verdict is recorded in, still proposes and still fills — an Add is not
//     a new Campaign, so ADR 0009 never gates it (CONTEXT.md: "Universe" —
//     the set of instruments the strategy may OPEN a Campaign in).
func TestLosingEligibilityNeverClosesAnOpenCampaignOrBlocksItsAdds(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	// breakoutBars gives 64 completed bars by day(56), short of the
	// Baseline's 250-bar floor; this fixture is about the eligibility gate
	// and the Add Ladder, not the history criterion (already covered
	// directly by internal/universe's own tests), so it turns the gate on
	// with a floor the fixture actually reaches.
	universeOn(&cfg)
	campaignID := testDecisionID("campaign", "CCC", day(56))
	campaignN := breakoutFixtureN(t, cfg)

	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(rung 2) error = %v", err)
	}
	fill2Price := rung2 + 0.06
	rung3, err := sizing.NextAddLevel(fill2Price, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel(rung 3) error = %v", err)
	}
	fill3Price := rung3 + 0.05

	bar57 := addOpportunityBar("CCC", day(57), rung2+5) // clears rung2; day(57) is still February.
	fill2 := addFill("CCC", campaignID, 2, day(57), "sim-fill-add-2", fill2Price, 133, day(57))
	bar58 := addOpportunityBar("CCC", day(58), rung3+5) // clears rung3; day(58) is 2026-03-01.
	fill3 := addFill("CCC", campaignID, 3, day(58), "sim-fill-add-3", fill3Price, 133, day(58))

	emitted := newStream(t, cfg).
		classify(eligibleClassification("CCC", day(0))).
		bars(breakoutBars("CCC")).
		fill(openingFillAs("CCC", "sim-fill-open")).
		bar(bar57).
		fill(fill2).
		classify(etfClassification("CCC", day(57).Add(time.Hour))). // declared ineligible before March's evaluation
		bar(bar58).
		fill(fill3).
		mustRun()

	marchDecision := universeEligibilityFor(t, emitted, "CCC")
	found := false
	for _, d := range marchDecision {
		if d.PeriodEnd.Equal(day(58)) {
			found = true
			if d.Eligible {
				t.Errorf("March's decision: Eligible = true, want false (CCC was declared an ETF before it)")
			}
		}
	}
	if !found {
		t.Fatalf("no universe.eligibility decision recorded at day(58) (2026-03-01); got %+v", marchDecision)
	}

	if exited := envelopesOfType(emitted, event.CampaignExitedEventType); len(exited) != 0 {
		t.Fatalf("got %d campaign-exited event(s), want 0: losing eligibility must never close an open Campaign (ADR 0009)", len(exited))
	}
	added := envelopesOfType(emitted, event.CampaignUnitAddedEventType)
	if len(added) != 2 {
		t.Fatalf("got %d unit-added event(s), want exactly 2 (units 2 and 3, including the one reached in the same Session CCC was just found ineligible in)", len(added))
	}
	declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(declines) != 0 {
		t.Fatalf("got %d decline(s), want 0: an Add is never checked against ADR 0009's eligibility gate", len(declines))
	}
}

// TestAFutureEffectiveClassificationDoesNotChangeAnEarlierSessionsVerdict
// covers a Greptile review finding on this ticket's PR: a declaration
// effective AFTER a Session that must nonetheless evaluate (here,
// day(30)'s first-of-month close) must not be read by that evaluation just
// because it happened to arrive before the Session closed. ADR 0009 is
// point-in-time: a fact takes effect only at the first Session close at or
// after its own EffectiveAt, never earlier, however early it is declared.
func TestAFutureEffectiveClassificationDoesNotChangeAnEarlierSessionsVerdict(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	universeOn(&cfg)

	quiet := func(d int) event.CompletedBarPayload { return completedBar("EEEE", day(d), 150, 150, 150) }

	s := newStream(t, cfg).classify(eligibleClassification("EEEE", day(0)))
	for d := 1; d <= 29; d++ { // January.
		s.bar(quiet(d))
	}
	// Declared well before day(30)'s close, but not effective until
	// day(35) — after it.
	s.classify(etfClassification("EEEE", day(35)))
	s.bar(quiet(30)) // day(30) = 2026-02-01, February's first trading day.
	for d := 31; d <= 34; d++ {
		s.bar(quiet(d))
	}
	s.bar(quiet(35)) // the first close at or after day(35)'s own EffectiveAt.
	emitted := s.mustRun()

	decisions := universeEligibilityFor(t, emitted, "EEEE")
	byPeriodEnd := make(map[time.Time]event.UniverseEligibilityPayload, len(decisions))
	for _, d := range decisions {
		byPeriodEnd[d.PeriodEnd] = d
	}

	feb1, ok := byPeriodEnd[day(30)]
	if !ok {
		t.Fatalf("no decision recorded at day(30) (2026-02-01); got %+v", decisions)
	}
	if !feb1.ClassificationEligible || !feb1.Eligible {
		t.Errorf("day(30)'s decision = %+v, want still eligible: the ETF declaration is not effective until day(35), so it must not have been read yet", feb1)
	}

	promotion, ok := byPeriodEnd[day(35)]
	if !ok {
		t.Fatalf("no decision recorded at day(35), the first close at or after the ETF declaration's own EffectiveAt; got %+v", decisions)
	}
	if promotion.ClassificationEligible || promotion.Eligible {
		t.Errorf("day(35)'s decision = %+v, want ineligible: the ETF declaration has now taken effect", promotion)
	}

	// Nothing between day(30) and day(35) should have re-evaluated EEEE at
	// all: no declaration was due yet, and neither Session was a month
	// boundary.
	for d := 31; d <= 34; d++ {
		if _, evaluated := byPeriodEnd[day(d)]; evaluated {
			t.Errorf("an unexpected decision was recorded at day(%d); the ETF declaration is not due until day(35)", d)
		}
	}
}

// TestAnInstrumentWithOnlyAFutureDeclarationHasNoActiveClassificationYet
// covers the case evaluateUniverse's own promotion loop guards against:
// an instrument whose only declaration so far is not yet effective has
// nothing active at all, and so is skipped entirely — no decision, and no
// gating — even at a Session close (here, the run's very first) that would
// otherwise evaluate it.
func TestAnInstrumentWithOnlyAFutureDeclarationHasNoActiveClassificationYet(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	universeOn(&cfg)

	quiet := func(d int) event.CompletedBarPayload { return completedBar("FFFF", day(d), 150, 150, 150) }

	emitted := newStream(t, cfg).
		classify(eligibleClassification("FFFF", day(10))). // not effective until day(10)
		bar(quiet(1)).                                     // the run's first Session: nothing active yet
		bar(quiet(9)).
		bar(quiet(10)). // the first close at or after the declaration's own EffectiveAt
		mustRun()

	decisions := universeEligibilityFor(t, emitted, "FFFF")
	if len(decisions) != 1 {
		t.Fatalf("got %d decision(s), want exactly 1 (none before day(10), one at it)", len(decisions))
	}
	if !decisions[0].PeriodEnd.Equal(day(10)) {
		t.Errorf("decision PeriodEnd = %s, want day(10) %s", decisions[0].PeriodEnd, day(10))
	}

	// Before day(10), FFFF is not yet a candidate the gate has anything to
	// say about, but it also is not declined: sizeUnit only ever runs for
	// a bar that breaks out, and none of these quiet bars do.
	if declines := envelopesOfType(emitted, event.ProposalDeclinedEventType); len(declines) != 0 {
		t.Fatalf("got %d decline(s), want 0", len(declines))
	}
}

// TestAMidMonthReclassificationToAnETFIsDeclinedAtTheNextSignal covers a
// Greptile review finding on this ticket's PR: a stock found eligible and
// later reclassified to an ETF, off any month boundary, must not let a
// Signal slip through on its stale (or unevaluated) verdict before the next
// scheduled monthly evaluation. classificationRecord.pendingEvaluation
// already closes this gap by construction — evaluateUniverse always runs,
// and always catches this reclassification up, before any Signal for the
// same Session can be ranked or declined — and this test proves it end to
// end rather than merely by inspection.
func TestAMidMonthReclassificationToAnETFIsDeclinedAtTheNextSignal(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	universeOn(&cfg)

	bars := breakoutBars("DDD") // breakout at day(56); day(30) crosses into February.
	breakout := bars[len(bars)-1]
	warmup := bars[:len(bars)-1]

	emitted := newStream(t, cfg).
		classify(eligibleClassification("DDD", day(0))).
		bars(warmup).
		// Reclassified to an ETF between the last warm-up bar and the
		// breakout bar's own Session close, effective at the breakout bar's
		// own period end: this is "mid-month" (day(56) is 2026-02-27, no
		// month boundary of its own) and it is declared before, not after,
		// the Session it must gate.
		classify(etfClassification("DDD", breakout.PeriodEnd)).
		bar(breakout).
		mustRun()

	decisions := universeEligibilityFor(t, emitted, "DDD")
	if len(decisions) == 0 {
		t.Fatal("got 0 universe.eligibility decisions for DDD, want at least 1")
	}
	// Every evaluation before the reclassification found DDD eligible
	// (common stock); only the last one, at the breakout's own Session,
	// reflects the reclassification.
	last := decisions[len(decisions)-1]
	if !last.PeriodEnd.Equal(breakout.PeriodEnd) {
		t.Fatalf("last decision's PeriodEnd = %s, want the breakout's own %s", last.PeriodEnd, breakout.PeriodEnd)
	}
	if last.Eligible || last.ClassificationEligible {
		t.Errorf("last decision = %+v, want ineligible on the classification criterion", last)
	}

	if proposals := envelopesOfType(emitted, event.TradeProposalEventType); len(proposals) != 0 {
		t.Fatalf("got %d trade proposal(s), want 0: DDD was reclassified an ETF before its own breakout Session closed", len(proposals))
	}
	declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1", len(declines))
	}
	decline := decodeProposalDeclined(t, declines[0])
	if decline.Reason != event.DeclineReasonIneligible {
		t.Errorf("decline.Reason = %q, want %q", decline.Reason, event.DeclineReasonIneligible)
	}
}

// --- Chronology and payload validity: fail-closed everywhere else --------

// classificationAtSchema delivers a classification stamped with a schema
// version other than the one this build was written against, mirroring
// delisting_test.go's corporateActionAtSchema.
func (s *stream) classificationAtSchema(payload event.InstrumentClassificationPayload, schemaVersion uint32) *stream {
	s.t.Helper()
	s.seq++
	envelope := classificationEnvelope(s.t, s.seq, payload)
	envelope.SchemaVersion = schemaVersion
	s.envelopes = append(s.envelopes, envelope)
	return s
}

func TestInstrumentClassificationBeforeConfigurationFailsClosed(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, validConfigurationPayload())
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	envelope := classificationEnvelope(t, 1, eligibleClassification("AAPL", day(1)))
	_, err = engine.Run(context.Background(), []event.Envelope{envelope})
	if err == nil {
		t.Fatal("Run() error = nil, want an error for a classification before any configuration")
	}
	if !strings.Contains(err.Error(), "configuration") {
		t.Fatalf("Run() error = %v, want it to name the missing configuration", err)
	}
}

func TestInstrumentClassificationWithWrongSchemaVersionFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	stream := newStream(t, cfg).classificationAtSchema(eligibleClassification("AAPL", day(1)), 999)
	stream.wantRunError("instrument classification payload schema version")
}

func TestInstrumentClassificationRejectsUndecodablePayload(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, validConfigurationPayload())
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	payload := mustMarshal(t, 42)
	undecodable := event.Envelope{
		ID:                "classification-undecodable",
		Type:              event.MarketInstrumentClassificationEventType,
		SchemaVersion:     event.MarketInstrumentClassificationSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         day(1),
		RecordedAt:        day(1),
		Sequence:          2,
		Source:            "fixture",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
	cfg := configEnvelope(t, 1, day(0))
	_, err = engine.Run(context.Background(), []event.Envelope{cfg, undecodable})
	if err == nil || !strings.Contains(err.Error(), "decode instrument classification payload") {
		t.Fatalf("Run() error = %v, want it to name the decode failure", err)
	}
}

func TestInstrumentClassificationRejectsAnInvalidPayload(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	invalid := eligibleClassification("AAPL", day(1))
	invalid.SecurityType = ""
	stream := newStream(t, cfg).classify(invalid)
	stream.wantRunError("invalid instrument classification payload", "not a recognised security type")
}

// TestInstrumentClassificationCannotBeRestatedOutOfOrder pins the one
// chronology rule a declared classification is held to: a later
// declaration's EffectiveAt must not precede an earlier one's own — the
// audit trail this field exists to keep honest (applyInstrumentClassification's
// own doc comment).
func TestInstrumentClassificationCannotBeRestatedOutOfOrder(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	stream := newStream(t, cfg).
		classify(eligibleClassification("AAPL", day(10))).
		classify(etfClassification("AAPL", day(5)))
	stream.wantRunError("predates its own most recent declaration")
}

// TestEveryDeclaredSecurityTypeBridgesToInternalUniverse exercises
// securityTypeFor's four recognised values directly (reducer.go's
// sizingModeFor is the identical bridge for event.SizingMode; this is ADR
// 0009's counterpart), not only the two universe.Evaluate itself needs to
// distinguish eligible from ineligible (internal/universe's own tests cover
// every SecurityType value's effect on Eligible thoroughly; this proves the
// wire-to-domain mapping for all four, including the two — ADR and SPAC —
// no other reducer-level fixture happens to classify an instrument as).
func TestEveryDeclaredSecurityTypeBridgesToInternalUniverse(t *testing.T) {
	t.Parallel()

	for _, securityType := range []string{
		event.SecurityTypeCommonStock, event.SecurityTypeETF, event.SecurityTypeADR, event.SecurityTypeSPAC,
	} {
		t.Run(securityType, func(t *testing.T) {
			t.Parallel()
			cfg := validConfigurationPayload()
			universeOn(&cfg)
			payload := eligibleClassification("AAPL", day(0))
			payload.SecurityType = securityType
			emitted := newStream(t, cfg).
				classify(payload).
				bar(completedBar("AAPL", day(1), 150, 150, 150)).
				mustRun()

			decisions := universeEligibilityFor(t, emitted, "AAPL")
			if len(decisions) != 1 {
				t.Fatalf("got %d decision(s), want exactly 1", len(decisions))
			}
			wantClassificationEligible := securityType == event.SecurityTypeCommonStock
			if decisions[0].ClassificationEligible != wantClassificationEligible {
				t.Errorf("ClassificationEligible = %v, want %v for security type %q", decisions[0].ClassificationEligible, wantClassificationEligible, securityType)
			}
		})
	}
}

// TestALaterClassificationReplacesAnEarlierOne is the positive counterpart:
// a classification declared with a LATER EffectiveAt is accepted and is
// what the next monthly evaluation reads — already exercised end to end by
// TestLosingEligibilityNeverClosesAnOpenCampaignOrBlocksItsAdds's second
// classify() call; this test isolates just the acceptance itself.
func TestALaterClassificationReplacesAnEarlierOne(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	universeOn(&cfg)
	emitted := newStream(t, cfg).
		classify(eligibleClassification("AAPL", day(0))).
		classify(etfClassification("AAPL", day(0).Add(time.Hour))).
		bar(completedBar("AAPL", day(1), 150, 150, 150)).
		mustRun()

	decisions := universeEligibilityFor(t, emitted, "AAPL")
	if len(decisions) != 1 {
		t.Fatalf("got %d decision(s), want exactly 1", len(decisions))
	}
	if decisions[0].Eligible || decisions[0].ClassificationEligible {
		t.Errorf("decision = %+v, want ineligible on the classification criterion (the later, ETF declaration must be what is read)", decisions[0])
	}
}
