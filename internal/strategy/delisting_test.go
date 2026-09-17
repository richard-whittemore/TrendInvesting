package strategy_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// This file holds the tests for the Delisting Exit (CONTEXT.md: "Delisting
// Exit"; ADR 0009), the third and last of the three ways a Campaign can end.
// Every fixture below builds on the existing
// breakoutBars/openingFill/postEntryBar/addOpportunityBar/buildGapCampaign
// fixtures (campaign_test.go, exit_test.go, add_test.go, stop_ladder_test.go),
// which open a 133-share AAPL Campaign at day(56) with campaignN ==
// breakoutFixtureN(t, cfg) and campaignFillPrice == 201.25.
//
// # Why these fixture numbers can fail
//
// The closing price a delisting exit records — state.previousClose, the last
// completed bar's own split-adjusted close (see delisting.go's own doc
// comment) — is deliberately made to differ from every OTHER number a wrong
// implementation could reach for instead: the campaignFillPrice entry
// (201.25), the campaign's Protective Stop (entryPrice - 2*campaignN), the
// warmed-up Exit Channel low (100, see exit_test.go's header), and the
// closing bar's own High and Low (never its Close). This project has shipped
// more than one fixture whose constants happened to coincide, so the
// assertion never exercised the difference it was written for; every bar
// below is built with an explicit, distinct Close for exactly that reason —
// completedBar/postEntryBar/addOpportunityBar never default Close to Low or
// High.

// corporateActionEnvelope builds a market.corporate-action envelope for
// payload, mirroring barEnvelope/fillEnvelope's own shape.
func corporateActionEnvelope(t *testing.T, sequence uint64, payload event.CorporateActionPayload) event.Envelope {
	t.Helper()
	encoded := mustMarshal(t, payload)
	return event.Envelope{
		ID:              fmt.Sprintf("corp-action-%d", sequence),
		Type:            event.MarketCorporateActionEventType,
		SchemaVersion:   event.MarketCorporateActionSchemaVersion,
		EnvelopeVersion: event.CurrentEnvelopeVersion,
		EventTime:       payload.EffectiveAt,
		RecordedAt:      payload.EffectiveAt,
		Sequence:        sequence,
		// A corporate action is a fact about the market this system did not
		// produce, mirroring a bar's own "fixture" source in these tests
		// (event.MarketCorporateActionEventType's own doc comment).
		Source:            "fixture",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(encoded),
		Payload:           encoded,
	}
}

func (s *stream) corporateAction(payload event.CorporateActionPayload) *stream {
	s.t.Helper()
	s.seq++
	s.envelopes = append(s.envelopes, corporateActionEnvelope(s.t, s.seq, payload))
	return s
}

// corporateActionAtSchema delivers a corporate action stamped with a schema
// version other than the one this build was written against, mirroring
// stream.fillAtSchema.
func (s *stream) corporateActionAtSchema(payload event.CorporateActionPayload, schemaVersion uint32) *stream {
	s.t.Helper()
	s.seq++
	envelope := corporateActionEnvelope(s.t, s.seq, payload)
	envelope.SchemaVersion = schemaVersion
	s.envelopes = append(s.envelopes, envelope)
	return s
}

func delistingAction(instrumentID string, effectiveAt time.Time) event.CorporateActionPayload {
	return event.CorporateActionPayload{
		InstrumentID: instrumentID,
		Kind:         event.CorporateActionKindDelisting,
		EffectiveAt:  effectiveAt,
	}
}

// --- The headline behaviour --------------------------------------------

// TestDelistingClosesAnOpenCampaignAtTheLastAvailablePrice is the primary
// event-seam test for a Delisting Exit: a delisting for an instrument with an
// open Campaign closes it at the last available price (the closing bar's own
// Close — see this file's header for why 155 is deliberately unlike every
// other number in the fixture), reason delisting, with no fill involved
// (FillID names the corporate-action envelope instead).
func TestDelistingClosesAnOpenCampaignAtTheLastAvailablePrice(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	lastBar := completedBar("AAPL", day(57), 160, 150, 155)
	effectiveAt := day(57)

	stream := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(lastBar)

	// Neither the Exit Channel nor the Add Ladder fires on this bar: its low
	// (150) stays well above the warmed-up channel (100), and its high (160)
	// stays well below the second Unit's own rung — pinned so a wrong
	// implementation that mistook one of THOSE proposals for a delisting
	// close would show up as an extra emission below.
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}
	if lastBar.SplitAdjusted.High >= rung2 {
		t.Fatalf("fixture bug: bar high %v reaches the add rung %v; this test needs a bar that proposes nothing of its own", lastBar.SplitAdjusted.High, rung2)
	}

	stream = stream.corporateAction(delistingAction("AAPL", effectiveAt))
	// The corporate action envelope's own id, read back from the stream
	// rather than hand-computed from its sequence number: what matters is
	// that FillID below names WHICHEVER envelope forced the closure, not a
	// particular numbering scheme.
	corporateActionEnvelopeID := stream.envelopes[len(stream.envelopes)-1].ID
	emitted := stream.mustRun()

	if got := len(envelopesOfType(emitted, event.ExitProposalEventType)); got != 0 {
		t.Fatalf("got %d exit proposal(s), want 0: this bar's low never breached the exit channel", got)
	}
	if got := len(envelopesOfType(emitted, event.AddProposalEventType)); got != 0 {
		t.Fatalf("got %d add proposal(s), want 0: this bar's high never reached the add rung", got)
	}

	exitEnvelope := onlyEnvelopeOfType(t, emitted, event.CampaignExitedEventType)
	if !exitEnvelope.EventTime.Equal(effectiveAt) {
		t.Errorf("Campaign exited EventTime = %v, want the corporate action's own %v", exitEnvelope.EventTime, effectiveAt)
	}

	exited := decodeCampaignExited(t, exitEnvelope)
	if exited.CampaignID != campaignID {
		t.Errorf("CampaignID = %q, want %q", exited.CampaignID, campaignID)
	}
	if exited.FillID != corporateActionEnvelopeID {
		t.Errorf("FillID = %q, want the corporate-action envelope's own id %q: a delisting has no execution to name (event.CampaignExitedPayload.FillID's own doc comment)", exited.FillID, corporateActionEnvelopeID)
	}
	if exited.ExitedAt != effectiveAt {
		t.Errorf("ExitedAt = %v, want the corporate action's own EffectiveAt %v", exited.ExitedAt, effectiveAt)
	}
	if exited.Reason != event.ExitReasonDelisting {
		t.Errorf("Reason = %q, want %q", exited.Reason, event.ExitReasonDelisting)
	}
	if exited.Rule != event.RuleCampaignExitedByDelisting {
		t.Errorf("Rule = %q, want %q", exited.Rule, event.RuleCampaignExitedByDelisting)
	}
	if exited.ADR != event.ADRDelistingForcesExit {
		t.Errorf("ADR = %q, want %q", exited.ADR, event.ADRDelistingForcesExit)
	}
	if exited.EntryPrice != campaignFillPrice {
		t.Errorf("EntryPrice = %v, want the opening fill's %v", exited.EntryPrice, campaignFillPrice)
	}
	// The headline assertion: the exit price is the closing bar's own Close
	// (155) — not its Low (150), not its High (160), not the warmed-up Exit
	// Channel level (100), and not the campaign's own Protective Stop
	// (campaignFillPrice - 2*campaignN). Any of those wrong choices would
	// fail this assertion outright, which is why they were chosen apart.
	wantExitPrice := 155.0
	if exited.ExitPrice != wantExitPrice {
		t.Errorf("ExitPrice = %v, want %v (the closing bar's own Close, the last available price)", exited.ExitPrice, wantExitPrice)
	}
	stopLevel := campaignFillPrice - float64(cfg.StopMultiple*campaignN)
	if wantExitPrice == 100 || wantExitPrice == stopLevel || wantExitPrice == lastBar.SplitAdjusted.Low || wantExitPrice == lastBar.SplitAdjusted.High {
		t.Fatalf("fixture bug: the chosen exit price %v coincides with a level a wrong implementation might reach for instead; the assertion above would be vacuous", wantExitPrice)
	}
	if exited.Quantity != 133 {
		t.Errorf("Quantity = %d, want the whole 133-share campaign", exited.Quantity)
	}
	if exited.CampaignN != campaignN {
		t.Errorf("CampaignN = %v, want the campaign's frozen %v", exited.CampaignN, campaignN)
	}
	if exited.ProtectiveStopLevel != stopLevel {
		t.Errorf("ProtectiveStopLevel = %v, want the campaign's own %v (unaffected by the delisting)", exited.ProtectiveStopLevel, stopLevel)
	}

	wantRealisedResult := float64(133) * (wantExitPrice - campaignFillPrice) * cfg.DollarsPerPoint
	if exited.RealisedResult != wantRealisedResult {
		t.Errorf("RealisedResult = %v, want exactly %v", exited.RealisedResult, wantRealisedResult)
	}
	if err := exited.Validate(); err != nil {
		t.Errorf("emitted Campaign-exited payload fails its own Validate(): %v", err)
	}
}

// TestDelistingWithNoOpenCampaignIsANoOp pins the ticket's own deliberate
// exception to this project's fail-closed default: a delisting for an
// instrument this reducer has never even seen a bar for produces no event
// and no error.
func TestDelistingWithNoOpenCampaignIsANoOp(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	emitted := newStream(t, cfg).
		corporateAction(delistingAction("AAPL", day(1))).
		mustRun()

	if len(emitted) != 0 {
		t.Fatalf("got %d emission(s) for a delisting with no open campaign, want 0: %v", len(emitted), emitted)
	}
}

// TestDelistingAfterACampaignAlreadyClosedIsANoOp is
// TestDelistingWithNoOpenCampaignIsANoOp's other half: an instrument this
// reducer HAS seen, but whose Campaign has already exited on its own (via
// the Exit Channel here), is the identical no-op — not merely "never
// entered".
func TestDelistingAfterACampaignAlreadyClosedIsANoOp(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	breachAt := day(57)
	exitFill := closingExitFill("AAPL", campaignID, 100, breachAt, day(58))

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(postEntryBar("AAPL", breachAt, 99)).
		fill(exitFill).
		corporateAction(delistingAction("AAPL", day(59))).
		mustRun()

	if got := len(envelopesOfType(emitted, event.CampaignExitedEventType)); got != 1 {
		t.Fatalf("got %d campaign-exited event(s), want exactly 1 (the exit-channel close; the delisting notice that follows must add nothing)", got)
	}
	exited := decodeCampaignExited(t, onlyEnvelopeOfType(t, emitted, event.CampaignExitedEventType))
	if exited.Reason != event.ExitReasonExitChannel {
		t.Errorf("Reason = %q, want %q: the delisting notice arrived AFTER the campaign had already closed on its own", exited.Reason, event.ExitReasonExitChannel)
	}
}

// TestDelistingIsIdempotentOnceItHasAlreadyClosedTheCampaign covers a
// repeated delisting notice for the SAME campaign, closed by an EARLIER
// delisting of its own: the named invariant is that a delisted instrument
// cannot un-delist and cannot be traded again in this run, so the second
// notice states no new fact and is absorbed exactly like
// TestDelistingWithNoOpenCampaignIsANoOp.
func TestDelistingIsIdempotentOnceItHasAlreadyClosedTheCampaign(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(completedBar("AAPL", day(57), 160, 150, 155)).
		corporateAction(delistingAction("AAPL", day(57))).
		corporateAction(delistingAction("AAPL", day(58))).
		mustRun()

	if got := len(envelopesOfType(emitted, event.CampaignExitedEventType)); got != 1 {
		t.Fatalf("got %d campaign-exited event(s), want exactly 1: a repeated delisting notice must not close anything a second time", got)
	}
}

// --- Delisting wins ------------------------------------------------------

// TestDelistingOnTheSameBarAsAnExitChannelBreachDelistingWins is the
// ticket's own required case: a bar breaches the Exit Channel (raising an
// exit proposal, no fill for it ever arrives) and a delisting for the SAME
// bar follows. Exactly one exit results, reason delisting — never
// exit-channel — and the pre-empted exit proposal is cancelled with a
// journalled reason saying why, rather than left to dangle.
func TestDelistingOnTheSameBarAsAnExitChannelBreachDelistingWins(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	breachAt := day(57)
	breachBar := postEntryBar("AAPL", breachAt, 99) // low=99 (breach), close=124

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(breachBar).
		corporateAction(delistingAction("AAPL", breachAt)).
		mustRun()

	// The breach itself still produces its own proposal — evaluateCampaign
	// runs unconditionally on every bar an open Campaign sees — but it must
	// never be FILLED or otherwise turned into an exit-channel close.
	proposal := decodeExitProposal(t, onlyEnvelopeOfType(t, emitted, event.ExitProposalEventType))
	if proposal.Reason != event.ExitReasonExitChannel {
		t.Fatalf("fixture bug: exit proposal reason = %q, want %q (this test's premise is that a breach proposal exists to be pre-empted)", proposal.Reason, event.ExitReasonExitChannel)
	}

	exitEnvelope := onlyEnvelopeOfType(t, emitted, event.CampaignExitedEventType)
	exited := decodeCampaignExited(t, exitEnvelope)
	if exited.CampaignID != campaignID {
		t.Errorf("CampaignID = %q, want %q", exited.CampaignID, campaignID)
	}
	// The headline assertion: delisting wins. Reason is delisting, and the
	// exit price is the bar's own Close (124) — not the Exit Channel level
	// (100) an exit-channel close would have used.
	if exited.Reason != event.ExitReasonDelisting {
		t.Errorf("Reason = %q, want %q: delisting must win over the same-bar exit-channel breach", exited.Reason, event.ExitReasonDelisting)
	}
	wantExitPrice := 124.0
	if exited.ExitPrice != wantExitPrice {
		t.Errorf("ExitPrice = %v, want %v (the breach bar's own Close, not the channel level 100)", exited.ExitPrice, wantExitPrice)
	}

	// The pre-empted exit proposal is cancelled with its own journalled
	// reason, not left outstanding forever (a delisted instrument produces
	// no next bar for ADR 0011's ordinary expiry to ever run on).
	expired := decodeProposalExpired(t, onlyEnvelopeOfType(t, emitted, event.ProposalExpiredEventType))
	if expired.Kind != event.ProposalKindExit {
		t.Errorf("Kind = %q, want %q", expired.Kind, event.ProposalKindExit)
	}
	if expired.Reason != event.ExpiryReasonSupersededByDelisting {
		t.Errorf("Reason = %q, want %q", expired.Reason, event.ExpiryReasonSupersededByDelisting)
	}
	if expired.Rule != event.RuleExitProposalSupersededByDelisting {
		t.Errorf("Rule = %q, want %q", expired.Rule, event.RuleExitProposalSupersededByDelisting)
	}
	if expired.ProposalID != exitProposalID("AAPL", breachAt) {
		t.Errorf("ProposalID = %q, want the pre-empted exit proposal's own %q", expired.ProposalID, exitProposalID("AAPL", breachAt))
	}
	if !expired.ExpiredAt.Equal(breachAt) {
		t.Errorf("ExpiredAt = %v, want the delisting's own EffectiveAt %v", expired.ExpiredAt, breachAt)
	}
	if err := expired.Validate(); err != nil {
		t.Errorf("emitted proposal expired payload fails its own Validate(): %v", err)
	}
}

// TestDelistingCancelsAPendingAddProposal is
// TestDelistingOnTheSameBarAsAnExitChannelBreachDelistingWins's Add-side
// counterpart: a bar reaches the second Unit's own Add rung (raising an Add
// proposal), and a delisting for the same bar cancels it too.
func TestDelistingCancelsAPendingAddProposal(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignN := breakoutFixtureN(t, cfg)
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}
	addBar := addOpportunityBar("AAPL", day(57), rung2+5) // low=150, close=150

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(addBar).
		corporateAction(delistingAction("AAPL", day(57))).
		mustRun()

	addProposal := onlyEnvelopeOfType(t, emitted, event.AddProposalEventType)
	if addProposal.ID != addProposalID("AAPL", day(57), 2) {
		t.Fatalf("fixture bug: add proposal id = %q, want %q (this test's premise is that unit 2's rung was reached)", addProposal.ID, addProposalID("AAPL", day(57), 2))
	}

	exited := decodeCampaignExited(t, onlyEnvelopeOfType(t, emitted, event.CampaignExitedEventType))
	if exited.Reason != event.ExitReasonDelisting {
		t.Errorf("Reason = %q, want %q", exited.Reason, event.ExitReasonDelisting)
	}
	if exited.ExitPrice != 150 {
		t.Errorf("ExitPrice = %v, want 150 (the add-opportunity bar's own Close)", exited.ExitPrice)
	}
	// Still a single-Unit campaign: the Add proposal was cancelled, never
	// filled, so no second Unit ever joined it.
	if exited.Quantity != 133 {
		t.Errorf("Quantity = %d, want 133: the pending add must never have executed", exited.Quantity)
	}

	expired := decodeProposalExpired(t, onlyEnvelopeOfType(t, emitted, event.ProposalExpiredEventType))
	if expired.Kind != event.ProposalKindAdd {
		t.Errorf("Kind = %q, want %q", expired.Kind, event.ProposalKindAdd)
	}
	if expired.Reason != event.ExpiryReasonSupersededByDelisting {
		t.Errorf("Reason = %q, want %q", expired.Reason, event.ExpiryReasonSupersededByDelisting)
	}
	if expired.Rule != event.RuleAddProposalSupersededByDelisting {
		t.Errorf("Rule = %q, want %q", expired.Rule, event.RuleAddProposalSupersededByDelisting)
	}
	if expired.ProposalID != addProposalID("AAPL", day(57), 2) {
		t.Errorf("ProposalID = %q, want %q", expired.ProposalID, addProposalID("AAPL", day(57), 2))
	}
}

// --- Countable separately (the fourth acceptance criterion) ---------------

// TestDelistedOutcomesAreCountableSeparatelyInAJournal runs two independent
// campaigns to two different terminal reasons within ONE journal (one
// engine.Run): AAPL exits via the Exit Channel, MSFT is delisted. Grouping
// the resulting campaign-exited events by Reason must separate the two
// cleanly — the acceptance criterion this ticket names directly.
func TestDelistedOutcomesAreCountableSeparatelyInAJournal(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	breachAt := day(57)

	msftOpeningFill := event.FillPayload{
		InstrumentID: "MSFT",
		Kind:         event.FillKindEntry,
		ProposalID:   testDecisionID("proposal", "MSFT", day(56)),
		FillID:       "sim-fill-0001-msft",
		Direction:    event.DirectionLong,
		Quantity:     133,
		Price:        campaignFillPrice,
		FilledAt:     day(56),
	}
	aaplExitFill := closingExitFill("AAPL", testDecisionID("campaign", "AAPL", day(56)), 100, breachAt, day(58))

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		bars(breakoutBars("MSFT")).
		fill(openingFill("AAPL")).
		fill(msftOpeningFill).
		bar(postEntryBar("AAPL", breachAt, 99)).            // AAPL breaches the exit channel
		bar(completedBar("MSFT", breachAt, 160, 150, 155)). // MSFT: an ordinary, non-breaching bar
		fill(aaplExitFill).
		corporateAction(delistingAction("MSFT", breachAt)).
		mustRun()

	exited := envelopesOfType(emitted, event.CampaignExitedEventType)
	if len(exited) != 2 {
		t.Fatalf("got %d campaign-exited event(s), want exactly 2 (one per instrument)", len(exited))
	}

	byReason := make(map[string]event.CampaignExitedPayload, 2)
	for _, e := range exited {
		payload := decodeCampaignExited(t, e)
		byReason[payload.Reason] = payload
	}
	if len(byReason) != 2 {
		t.Fatalf("got %d distinct reason(s) %v across 2 exits, want 2 (the two outcomes must be separately countable, not collapsed together)", len(byReason), byReason)
	}
	exitChannel, ok := byReason[event.ExitReasonExitChannel]
	if !ok {
		t.Fatalf("no campaign-exited event with reason %q", event.ExitReasonExitChannel)
	}
	if exitChannel.InstrumentID != "AAPL" {
		t.Errorf("exit-channel exit's InstrumentID = %q, want AAPL", exitChannel.InstrumentID)
	}
	delisted, ok := byReason[event.ExitReasonDelisting]
	if !ok {
		t.Fatalf("no campaign-exited event with reason %q", event.ExitReasonDelisting)
	}
	if delisted.InstrumentID != "MSFT" {
		t.Errorf("delisting exit's InstrumentID = %q, want MSFT", delisted.InstrumentID)
	}
	if delisted.ExitPrice != 155 {
		t.Errorf("delisting exit's ExitPrice = %v, want 155 (MSFT's own closing bar's Close)", delisted.ExitPrice)
	}
}

// --- Chronology and payload validity: fail-closed everywhere else --------

func TestDelistingBeforeConfigurationFailsClosed(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, validConfigurationPayload())
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	envelope := corporateActionEnvelope(t, 1, delistingAction("AAPL", day(1)))
	_, err = engine.Run(context.Background(), []event.Envelope{envelope})
	if err == nil {
		t.Fatal("Run() error = nil, want an error for a corporate action before any configuration")
	}
	if !strings.Contains(err.Error(), "configuration") {
		t.Fatalf("Run() error = %v, want it to name the missing configuration", err)
	}
}

func TestDelistingWithWrongSchemaVersionFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	stream := newStream(t, cfg).corporateActionAtSchema(delistingAction("AAPL", day(1)), 999)
	stream.wantRunError("corporate action payload schema version")
}

func TestDelistingRejectsUndecodablePayload(t *testing.T) {
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
		ID:                "corp-action-undecodable",
		Type:              event.MarketCorporateActionEventType,
		SchemaVersion:     event.MarketCorporateActionSchemaVersion,
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

	_, err = engine.Run(context.Background(), []event.Envelope{configEnvelope(t, 1, day(0)), undecodable})
	if err == nil || !strings.Contains(err.Error(), "decode corporate action payload") {
		t.Fatalf("Run() error = %v, want it to name a decode failure", err)
	}
}

func TestDelistingRejectsInvalidKind(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	stream := newStream(t, cfg).corporateAction(event.CorporateActionPayload{
		InstrumentID: "AAPL",
		Kind:         "merger",
		EffectiveAt:  day(1),
	})
	stream.wantRunError("invalid corporate action payload")
}

func TestDelistingEffectiveAtBeforeCampaignOpenedFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	// The opening fill claims day(57), a moment the stream has not yet reached
	// a bar for (legitimate in itself: ADR 0005 rests the order into the
	// following session, and the upper bound is only checked when that bar
	// arrives — see checkBarConfirmsCampaignOpening). That is what leaves room
	// for a delisting at day(56) to clear the last completed bar, which is
	// also day(56), while still predating the campaign's own opening.
	lateFill := openingFill("AAPL")
	lateFill.FilledAt = day(57)

	stream := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(lateFill).
		corporateAction(delistingAction("AAPL", day(56)))
	stream.wantRunError("predates campaign", "own opening fill")
}

// TestDelistingEffectiveAtBeforeTheLastCompletedBarFailsClosed pins the
// chronology this system already applies everywhere else (ADR 0004): the
// last available price is only as of the last completed bar this reducer
// has accepted, so a delisting cannot claim to take effect before that bar's
// own close exists.
func TestDelistingEffectiveAtBeforeTheLastCompletedBarFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	stream := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(completedBar("AAPL", day(57), 160, 150, 155)).
		// After campaign.openedAt (day56) but strictly before the last bar's
		// own period end (day57).
		corporateAction(delistingAction("AAPL", day(56).Add(12*time.Hour)))
	stream.wantRunError("predates the last completed bar")
}

// TestDelistingEffectiveAtBeforeTheLastCompletedBarFailsClosedForAnIdleKnownInstrument
// is the previous test's no-Campaign, no-proposal twin: the identical
// chronology check must hold for a known instrument with nothing outstanding
// to close, not only for one with an open Campaign. Before this test, that
// check ran only on the outstanding-business path, so a stale notice for a
// known but idle instrument fell into the no-op branch instead and recorded
// a tombstone that contradicted the SetupEvaluated events this reducer had
// already emitted for the bars after the one the notice actually predates —
// and, being terminal, could never be corrected by a later, accurate notice.
func TestDelistingEffectiveAtBeforeTheLastCompletedBarFailsClosedForAnIdleKnownInstrument(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	// The first 55 bars of the fixture only: warm-up, with no breakout (the
	// channel is not ready until bar 55 has been added — see
	// breakoutFixtureHighs' own caller), so this instrument is known to the
	// reducer but has no Campaign and no pending proposal of any kind.
	idleBars := breakoutBars("AAPL")[:55]
	stream := newStream(t, cfg).
		bars(idleBars).
		// Strictly before bar 55's own period end (day(55)).
		corporateAction(delistingAction("AAPL", day(54).Add(12*time.Hour)))
	stream.wantRunError("predates the last completed bar")
}

// TestDelistingAfterAPartialStopAggregatesTheWholeLife is the delisting
// counterpart of TestExitFillAfterAPartialStopAggregatesTheWholeLife
// (stop_ladder_test.go): a delisting closing whatever units survived an
// earlier partial stop must aggregate the campaign's WHOLE life, exactly as
// an exit-channel close does — never merely the units it happens to close
// itself.
func TestDelistingAfterAPartialStopAggregatesTheWholeLife(t *testing.T) {
	t.Parallel()

	stream, fixture := buildGapCampaign(t)

	stop4Price := fixture.unit4Stop - 0.37
	stop4 := stopFillForUnits("AAPL", fixture.campaignID, "sim-fill-stop-4", []string{fixture.unit4FillID}, stop4Price, 133, day(60))

	// bar59 (buildGapCampaign's own last bar) closed at 150 — the "last
	// available price" this delisting closes the surviving 3 units at, well
	// past the partial stop's own timestamp (day60).
	effectiveAt := day(61)

	emitted := stream.
		fill(stop4).
		corporateAction(delistingAction("AAPL", effectiveAt)).
		mustRun()

	exited := decodeCampaignExited(t, onlyEnvelopeOfType(t, emitted, event.CampaignExitedEventType))
	if exited.Reason != event.ExitReasonDelisting {
		t.Errorf("Reason = %q, want %q (even though an earlier stop closed part of this campaign)", exited.Reason, event.ExitReasonDelisting)
	}
	if exited.Units != 4 {
		t.Errorf("Units = %d, want 4 (the campaign's whole life)", exited.Units)
	}
	if exited.Quantity != 532 {
		t.Errorf("Quantity = %d, want 532 (133 x 4, across the earlier stop AND this delisting)", exited.Quantity)
	}

	const lastAvailablePrice = 150.0 // buildGapCampaign's own bar59 Close
	wantEntryPrice := (float64(133.0*fixture.unit4Fill) + float64(133.0*fixture.unit1Fill) + float64(133.0*fixture.unit2Fill) + float64(133.0*fixture.unit3Fill)) / 532.0
	wantExitPrice := (float64(133.0*stop4Price) + float64(399.0*lastAvailablePrice)) / 532.0
	if math.Abs(exited.EntryPrice-wantEntryPrice) > 1e-9 {
		t.Errorf("EntryPrice = %v, want ~%v", exited.EntryPrice, wantEntryPrice)
	}
	if math.Abs(exited.ExitPrice-wantExitPrice) > 1e-9 {
		t.Errorf("ExitPrice = %v, want ~%v", exited.ExitPrice, wantExitPrice)
	}
	wantRealisedResult := 532.0 * (wantExitPrice - wantEntryPrice) * fixture.cfg.DollarsPerPoint
	if math.Abs(exited.RealisedResult-wantRealisedResult) > 1e-6 {
		t.Errorf("RealisedResult = %v, want ~%v", exited.RealisedResult, wantRealisedResult)
	}
	if err := exited.Validate(); err != nil {
		t.Errorf("emitted campaign exited payload fails its own Validate(): %v", err)
	}
}

// TestDelistingEffectiveAtBeforeAnEarlierPartialStopFailsClosed is
// TestDelistingAfterAPartialStopAggregatesTheWholeLife's negative twin: a
// delisting cannot claim to take effect before an earlier closing fill this
// same campaign has already accepted (campaignState.lastCloseFillAt's own
// doc comment).
func TestDelistingEffectiveAtBeforeAnEarlierPartialStopFailsClosed(t *testing.T) {
	t.Parallel()

	stream, fixture := buildGapCampaign(t)
	stop4Price := fixture.unit4Stop - 0.37
	stop4 := stopFillForUnits("AAPL", fixture.campaignID, "sim-fill-stop-4", []string{fixture.unit4FillID}, stop4Price, 133, day(60))

	stream = stream.
		fill(stop4).
		// day(59) is not before bar59's own period end (equal), but IS
		// before the partial stop's own day(60).
		corporateAction(delistingAction("AAPL", day(59)))
	stream.wantRunError("predates campaign", "most recently accepted closing fill")
}

// --- A delisted instrument cannot be traded again in this run --------------
//
// The three fixtures below pin the invariant applyDelisting's own doc comment
// names. They exist because the invariant was once stated and not enforced:
// the no-Campaign branch returned early, so a delisting arriving while an
// entry proposal was outstanding left that proposal live, and nothing
// recorded the instrument as delisted at all.

// TestDelistingTerminallyResolvesAnOutstandingEntryProposal covers a
// delisting that arrives with an ENTRY proposal outstanding — the case with no
// open Campaign at all, since a pending entry proposal and a Campaign can
// never coexist. The proposal reaches its terminal event, and the fill that
// would have executed it opens no Campaign.
func TestDelistingTerminallyResolvesAnOutstandingEntryProposal(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	proposalID := testDecisionID("proposal", "AAPL", day(56))
	signalID := testDecisionID("signal", "AAPL", day(56))

	stream := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		corporateAction(delistingAction("AAPL", day(56))).
		fill(openingFill("AAPL"))

	emitted, err := stream.run()
	if err == nil {
		t.Fatal("Run() error = nil, want an error: a fill for a delisted instrument is a reconciliation failure")
	}
	for _, want := range []string{"delisted", "sim-fill-0001"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Run() error = %v, want substring %q", err, want)
		}
	}

	// The capital-safety assertion: no Campaign came into being, in an
	// instrument that had stopped trading.
	if got := len(envelopesOfType(emitted, event.CampaignOpenedEventType)); got != 0 {
		t.Fatalf("got %d campaign-opened event(s), want 0: a delisted instrument cannot be entered", got)
	}

	proposal := decodeTradeProposal(t, onlyEnvelopeOfType(t, emitted, event.TradeProposalEventType))
	expired := decodeProposalExpired(t, onlyEnvelopeOfType(t, emitted, event.ProposalExpiredEventType))
	if expired.Kind != event.ProposalKindEntry {
		t.Errorf("Kind = %q, want %q", expired.Kind, event.ProposalKindEntry)
	}
	if expired.Reason != event.ExpiryReasonSupersededByDelisting {
		t.Errorf("Reason = %q, want %q", expired.Reason, event.ExpiryReasonSupersededByDelisting)
	}
	if expired.Rule != event.RuleEntryProposalSupersededByDelisting {
		t.Errorf("Rule = %q, want %q", expired.Rule, event.RuleEntryProposalSupersededByDelisting)
	}
	if expired.ProposalID != proposalID {
		t.Errorf("ProposalID = %q, want the outstanding entry proposal's own %q", expired.ProposalID, proposalID)
	}
	if expired.SignalID != signalID {
		t.Errorf("SignalID = %q, want %q: an entry-kind expiry names the signal behind the proposal", expired.SignalID, signalID)
	}
	if !expired.ExpiredAt.Equal(day(56)) {
		t.Errorf("ExpiredAt = %v, want the delisting's own EffectiveAt %v", expired.ExpiredAt, day(56))
	}
	// Read back from the proposal this same run emitted, so the expiry
	// restates what was actually proposed rather than a hand-copied constant.
	if expired.Quantity != proposal.Quantity {
		t.Errorf("Quantity = %d, want the proposal's own %d", expired.Quantity, proposal.Quantity)
	}
	if expired.Level != proposal.EntryLevel {
		t.Errorf("Level = %v, want the proposal's own entry level %v", expired.Level, proposal.EntryLevel)
	}
	if err := expired.Validate(); err != nil {
		t.Errorf("emitted proposal expired payload fails its own Validate(): %v", err)
	}
}

// TestABarAfterADelistingEvaluatesNoSetup covers the other half of the same
// invariant: once a delisting has closed a Campaign, a later bar for that
// instrument — however plainly it breaks out — produces no Setup evaluation,
// no Signal and no entry proposal, so no fresh Campaign can follow.
func TestABarAfterADelistingEvaluatesNoSetup(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	// A high of 500 clears the fixture's own Entry Channel (which tops out at
	// 200) by a wide margin, so this bar would be a Breakout — a Setup
	// evaluation, a Signal and a proposal — on any instrument still trading.
	staleBar := completedBar("AAPL", day(58), 500, 490, 495)

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(completedBar("AAPL", day(57), 160, 150, 155)).
		corporateAction(delistingAction("AAPL", day(57))).
		bar(staleBar).
		mustRun()

	exited := decodeCampaignExited(t, onlyEnvelopeOfType(t, emitted, event.CampaignExitedEventType))
	if exited.Reason != event.ExitReasonDelisting {
		t.Fatalf("fixture bug: Reason = %q, want %q (this test's premise is that the delisting closed the campaign)", exited.Reason, event.ExitReasonDelisting)
	}

	for _, envelope := range emitted {
		if envelope.EventTime.Equal(staleBar.PeriodEnd) {
			t.Errorf("emission %q of type %q is attributed to the bar after the delisting; a delisted instrument decides nothing", envelope.ID, envelope.Type)
		}
	}
	if got := len(envelopesOfType(emitted, event.CampaignOpenedEventType)); got != 1 {
		t.Errorf("got %d campaign-opened event(s), want exactly 1 (the original entry; the stale bar must open nothing)", got)
	}
}

// TestADelistingForAnInstrumentNeverTradedBarsItFromBeingEnteredAtAll is the
// third case: the ticket's own no-op criterion still holds — no event and no
// error for an instrument this reducer has never seen — but the fact is
// nonetheless recorded, so the bars that follow it are never evaluated.
func TestADelistingForAnInstrumentNeverTradedBarsItFromBeingEnteredAtAll(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	emitted := newStream(t, cfg).
		corporateAction(delistingAction("AAPL", day(1))).
		bars(breakoutBars("AAPL")).
		mustRun()

	if len(emitted) != 0 {
		t.Fatalf("got %d emission(s) after a delisting for an instrument never traded, want 0: %v", len(emitted), emitted)
	}
}

// TestDelistingForAnIdleKnownInstrumentStillRecordsTheTombstone is the
// previous test's known-but-idle twin: an instrument with completed bars
// accepted, but no Campaign and no pending proposal, still has its delisting
// recorded once the notice's own chronology clears the last completed bar —
// and the tombstone bars every later bar from being evaluated, exactly as it
// does for an instrument never traded at all.
func TestDelistingForAnIdleKnownInstrumentStillRecordsTheTombstone(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	// Warm-up only (see the idle fixture above): known to the reducer, but
	// idle — no breakout has ever raised a proposal.
	idleBars := breakoutBars("AAPL")[:55]
	// Clears the fixture's own Entry Channel (topping out at 155) by a wide
	// margin, so this bar would be a Breakout — a Setup evaluation, a Signal
	// and a proposal — on any instrument still trading.
	staleBar := completedBar("AAPL", day(56), 500, 490, 495)

	emitted := newStream(t, cfg).
		bars(idleBars).
		corporateAction(delistingAction("AAPL", day(55))).
		bar(staleBar).
		mustRun()

	for _, envelope := range emitted {
		if envelope.EventTime.Equal(staleBar.PeriodEnd) {
			t.Errorf("emission %q of type %q is attributed to the bar after the idle delisting notice; a delisted instrument decides nothing", envelope.ID, envelope.Type)
		}
	}
}
