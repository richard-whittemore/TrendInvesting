package strategy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// --- #11 fixtures and helpers -------------------------------------------
//
// Every test in this file drives the reducer through replay.Engine.Run — the
// event seam — because that is where the invariant this ticket encodes is
// observable: a Campaign exists only if a Campaign-opened event was emitted,
// and that only ever happens on a recorded fill.

// campaignFillPrice is what the fixtures below actually fill at. It is
// deliberately different from the breakout fixture's entry level of 200 (see
// breakoutFixtureHighs), so a Campaign that recorded the intended level rather
// than the executed price would be visible rather than indistinguishable. It
// is above the level, which is the direction ADR 0013's slippage always pushes
// a long entry.
const campaignFillPrice = 201.25

// breakoutBars is breakoutFixtureHighs as bars for one instrument: 55 warm-up
// bars followed by the breakout bar at day(56), whose high is 200.
func breakoutBars(instrumentID string) []event.CompletedBarPayload {
	highs := breakoutFixtureHighs()
	bars := make([]event.CompletedBarPayload, 0, len(highs))
	for i, high := range highs {
		bars = append(bars, syntheticBar(instrumentID, day(i+1), high-100))
	}
	return bars
}

// nextBreakoutBar is the bar after the fixture's breakout bar, with a high of
// 201. The Entry Channel over the preceding 55 bars (2..56) tops out at 200,
// so this bar is a fresh breakout in its own right — which is what makes it a
// usable probe for "does an open Campaign suppress a new entry".
func nextBreakoutBar(instrumentID string) event.CompletedBarPayload {
	return syntheticBar(instrumentID, day(57), 101)
}

// quietBar is the same slot in the stream with a high of 150 instead: well
// under the 200 Entry Channel and further than the configured Tier B distance
// of 1N away from it, so it produces a Setup-evaluated event and nothing else.
// It is what a test needs when the point is that the previous bar's proposal
// expired leaving nothing outstanding, rather than being replaced.
func quietBar(instrumentID string) event.CompletedBarPayload {
	return syntheticBar(instrumentID, day(57), 50)
}

// testDecisionID mirrors the reducer's unexported decisionID so a fixture can
// name the proposal a fill executes before the run that produces it. It is not
// taken on trust: TestFillOpensACampaignWithNAndUnitSizeFrozen asserts the
// emitted proposal envelope's ID equals this, so the two cannot drift apart
// silently.
func testDecisionID(kind, instrumentID string, periodEnd time.Time) string {
	return fmt.Sprintf("%s:%s:%s", kind, instrumentID, periodEnd.UTC().Format("2006-01-02T15:04:05.000000000Z"))
}

// openingFill is the fill that executes the breakout bar's proposal for
// instrumentID: the full 133-share Unit at campaignFillPrice, timestamped at
// the bar period end (ADR 0005 models the entry as a resting order filled
// inside that bar, so in a backtest the fill belongs to the bar it happened
// in).
func openingFill(instrumentID string) event.FillPayload {
	return event.FillPayload{
		InstrumentID: instrumentID,
		Kind:         event.FillKindEntry,
		ProposalID:   testDecisionID("proposal", instrumentID, day(56)),
		FillID:       "sim-fill-0001",
		Direction:    event.DirectionLong,
		Quantity:     133,
		Price:        campaignFillPrice,
		FilledAt:     day(56),
	}
}

// campaignStopGap is how far BELOW the Campaign's Protective Stop level the
// stop fixtures below actually fill — proving that the recorded exit price
// is what filled, not the level (ADR 0005's gap rule).
const campaignStopGap = 0.37

// closingStopFill is the fill that closes the Campaign openingFill opened,
// at its Protective Stop, gapped through by campaignStopGap. campaignID must
// be the Campaign-opened envelope's own ID (deterministic — see
// testDecisionID — but callers pass it explicitly rather than
// re-deriving it, so a fixture that got the Campaign's identity wrong fails
// loudly instead of silently matching).
func closingStopFill(instrumentID, campaignID string, campaignN float64, filledAt time.Time) event.FillPayload {
	stopLevel := campaignFillPrice - 2*campaignN
	return event.FillPayload{
		InstrumentID: instrumentID,
		Kind:         event.FillKindStop,
		CampaignID:   campaignID,
		FillID:       "sim-fill-0002",
		Direction:    event.DirectionLong,
		Quantity:     133,
		Price:        stopLevel - campaignStopGap,
		FilledAt:     filledAt,
	}
}

func fillEnvelope(t *testing.T, sequence uint64, fill event.FillPayload) event.Envelope {
	t.Helper()
	payload := mustMarshal(t, fill)
	return event.Envelope{
		ID:              fmt.Sprintf("fill-%d", sequence),
		Type:            event.FillEventType,
		SchemaVersion:   event.FillSchemaVersion,
		EnvelopeVersion: event.CurrentEnvelopeVersion,
		EventTime:       fill.FilledAt,
		RecordedAt:      fill.FilledAt,
		Sequence:        sequence,
		// A fill is an external fact this system did not produce: in slice 1 it
		// comes from a fixture and then #18's simulator, in slice 2 from the
		// LEAN adapter (#30). The reducer must never be the source of one.
		Source:            "fill-simulator",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// stream accumulates a contiguous input stream: one configuration event
// followed by bars and fills in the order a producer would deliver them.
type stream struct {
	t         *testing.T
	envelopes []event.Envelope
	seq       uint64
}

func newStream(t *testing.T, cfg event.ConfigurationPayload) *stream {
	t.Helper()
	return &stream{
		t:         t,
		envelopes: []event.Envelope{configEnvelopeWithConfig(t, 1, day(0), cfg)},
		seq:       1,
	}
}

func (s *stream) bar(bar event.CompletedBarPayload) *stream {
	s.seq++
	s.envelopes = append(s.envelopes, barEnvelope(s.t, s.seq, bar, bar.PeriodEnd))
	return s
}

func (s *stream) bars(bars []event.CompletedBarPayload) *stream {
	for _, b := range bars {
		s.bar(b)
	}
	return s
}

func (s *stream) fill(fill event.FillPayload) *stream {
	s.seq++
	s.envelopes = append(s.envelopes, fillEnvelope(s.t, s.seq, fill))
	return s
}

// fillAtSchema delivers a fill stamped with a schema version other than the
// one this build was written against.
func (s *stream) fillAtSchema(fill event.FillPayload, schemaVersion uint32) *stream {
	s.seq++
	envelope := fillEnvelope(s.t, s.seq, fill)
	envelope.SchemaVersion = schemaVersion
	s.envelopes = append(s.envelopes, envelope)
	return s
}

func (s *stream) run() ([]event.Envelope, error) {
	s.t.Helper()
	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		s.t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		s.t.Fatalf("replay.New() error = %v", err)
	}
	return engine.Run(context.Background(), s.envelopes)
}

func (s *stream) mustRun() []event.Envelope {
	s.t.Helper()
	emitted, err := s.run()
	if err != nil {
		s.t.Fatalf("Run() error = %v", err)
	}
	return emitted
}

// wantRunError asserts the run fails closed, naming each expected substring.
func (s *stream) wantRunError(want ...string) {
	s.t.Helper()
	_, err := s.run()
	if err == nil {
		s.t.Fatal("Run() error = nil, want an error")
	}
	for _, substring := range want {
		if !strings.Contains(err.Error(), substring) {
			s.t.Errorf("Run() error = %v, want substring %q", err, substring)
		}
	}
}

func decodeCampaignOpened(t *testing.T, envelope event.Envelope) event.CampaignOpenedPayload {
	t.Helper()
	if envelope.Type != event.CampaignOpenedEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.CampaignOpenedEventType)
	}
	var payload event.CampaignOpenedPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload
}

func decodeProposalExpired(t *testing.T, envelope event.Envelope) event.ProposalExpiredPayload {
	t.Helper()
	if envelope.Type != event.ProposalExpiredEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.ProposalExpiredEventType)
	}
	var payload event.ProposalExpiredPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload
}

func decodeProtectiveStopSet(t *testing.T, envelope event.Envelope) event.ProtectiveStopSetPayload {
	t.Helper()
	if envelope.Type != event.ProtectiveStopSetEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.ProtectiveStopSetEventType)
	}
	var payload event.ProtectiveStopSetPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload
}

func decodeCampaignExited(t *testing.T, envelope event.Envelope) event.CampaignExitedPayload {
	t.Helper()
	if envelope.Type != event.CampaignExitedEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.CampaignExitedEventType)
	}
	var payload event.CampaignExitedPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload
}

// instrumentOf reads the instrument an emission is about. Every decision
// payload in this system carries instrument_id, so one decoder serves them all
// and the counting helpers below do not need a switch over payload types.
func instrumentOf(t *testing.T, envelope event.Envelope) string {
	t.Helper()
	var payload struct {
		InstrumentID string `json:"instrument_id"`
	}
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload.InstrumentID
}

func countFor(t *testing.T, emitted []event.Envelope, eventType, instrumentID string) int {
	t.Helper()
	count := 0
	for _, e := range emitted {
		if e.Type == eventType && instrumentOf(t, e) == instrumentID {
			count++
		}
	}
	return count
}

func onlyEnvelopeOfType(t *testing.T, emitted []event.Envelope, eventType string) event.Envelope {
	t.Helper()
	matched := envelopesOfType(emitted, eventType)
	if len(matched) != 1 {
		t.Fatalf("got %d %s event(s), want exactly 1", len(matched), eventType)
	}
	return matched[0]
}

// --- The headline behaviour ---------------------------------------------

// TestFillOpensACampaignWithNAndUnitSizeFrozen is #11's primary event-seam
// test: #10's breakout fixture produces a trade proposal, a fill for that
// proposal arrives, and exactly one Campaign comes into being — carrying the
// frozen campaign N and Unit share count that ADR 0006 requires to be
// journaled, and the price that ACTUALLY filled rather than the level the
// Signal fired at.
//
// The fill price (201.25) is deliberately not the entry level (200), so the
// Protective Stop the Campaign records (201.25 - 2N = 126.09...) differs from
// the proposal's stop intent (200 - 2N = 124.84...). ADR 0013 puts slippage in
// the fill producer and measures the Add Ladder from the slipped fill, so a
// Campaign that recorded the intended level would put every later rung of both
// ladders in the wrong place.
func TestFillOpensACampaignWithNAndUnitSizeFrozen(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		mustRun()

	// 55 warm-up bars emit one Setup-evaluated each; the breakout bar emits
	// three (Setup-evaluated, Signal, Proposal); the fill emits two
	// (Campaign-opened, then Protective-Stop-set).
	if len(emitted) != 60 {
		t.Fatalf("len(emitted) = %d, want 60 (55 x 1, the breakout bar's 3, and the fill's 2)", len(emitted))
	}

	proposalEnvelope := onlyEnvelopeOfType(t, emitted, event.TradeProposalEventType)
	signalEnvelope := onlyEnvelopeOfType(t, emitted, event.SignalEventType)
	proposal := decodeTradeProposal(t, proposalEnvelope)

	// The fixture names the proposal it fills before the run produces it, so
	// this pins testDecisionID against the reducer's own construction.
	fill := openingFill("AAPL")
	if proposalEnvelope.ID != fill.ProposalID {
		t.Fatalf("proposal envelope ID = %q, but the fixture's fill executes %q", proposalEnvelope.ID, fill.ProposalID)
	}

	campaignEnvelope := onlyEnvelopeOfType(t, emitted, event.CampaignOpenedEventType)
	// Second-to-last: the Protective-Stop-set emission follows it, per the
	// ticket's required emission order.
	if campaignEnvelope.Sequence != uint64(len(emitted)-1) {
		t.Errorf("Campaign opened Sequence = %d, want %d (second-to-last: Protective-Stop-set follows it)", campaignEnvelope.Sequence, len(emitted)-1)
	}

	stopSetEnvelope := onlyEnvelopeOfType(t, emitted, event.ProtectiveStopSetEventType)
	if stopSetEnvelope.Sequence != uint64(len(emitted)) {
		t.Errorf("Protective-Stop-set Sequence = %d, want %d (it is the last emission)", stopSetEnvelope.Sequence, len(emitted))
	}
	if stopSetEnvelope.CausationID != campaignEnvelope.CausationID {
		t.Errorf("Protective-Stop-set CausationID = %q, want the same fill %q that caused the Campaign", stopSetEnvelope.CausationID, campaignEnvelope.CausationID)
	}
	if !stopSetEnvelope.EventTime.Equal(day(56)) {
		t.Errorf("Protective-Stop-set EventTime = %v, want the fill's %v", stopSetEnvelope.EventTime, day(56))
	}

	wantID := "campaign:AAPL:2026-02-27T00:00:00.000000000Z"
	if campaignEnvelope.ID != wantID {
		t.Errorf("Campaign opened ID = %q, want %q (deterministic and reproducible on replay)", campaignEnvelope.ID, wantID)
	}
	if campaignEnvelope.SchemaVersion != event.CampaignOpenedSchemaVersion {
		t.Errorf("Campaign opened SchemaVersion = %d, want %d", campaignEnvelope.SchemaVersion, event.CampaignOpenedSchemaVersion)
	}
	if campaignEnvelope.EnvelopeVersion != event.CurrentEnvelopeVersion {
		t.Errorf("Campaign opened EnvelopeVersion = %d, want %d", campaignEnvelope.EnvelopeVersion, event.CurrentEnvelopeVersion)
	}
	// EventTime is the fill's own timestamp, not the bar's and never a wall
	// clock read: the Campaign came into being when the fill did.
	if !campaignEnvelope.EventTime.Equal(day(56)) {
		t.Errorf("Campaign opened EventTime = %v, want the fill's %v", campaignEnvelope.EventTime, day(56))
	}
	if !campaignEnvelope.RecordedAt.Equal(day(56)) {
		t.Errorf("Campaign opened RecordedAt = %v, want the input fill's %v", campaignEnvelope.RecordedAt, day(56))
	}
	if campaignEnvelope.Source != "reducer" {
		t.Errorf("Campaign opened Source = %q, want %q", campaignEnvelope.Source, "reducer")
	}
	if campaignEnvelope.StrategyVersion != testStrategyVersion {
		t.Errorf("Campaign opened StrategyVersion = %q, want %q", campaignEnvelope.StrategyVersion, testStrategyVersion)
	}
	if campaignEnvelope.ConfigurationHash != testConfigurationHash {
		t.Errorf("Campaign opened ConfigurationHash = %q, want %q", campaignEnvelope.ConfigurationHash, testConfigurationHash)
	}
	if campaignEnvelope.PayloadHash != event.HashPayload(campaignEnvelope.Payload) {
		t.Error("Campaign opened PayloadHash does not attest its own payload")
	}
	// The engine stamps causation from the input that produced the emission:
	// the fill, not the bar.
	if campaignEnvelope.CausationID != fmt.Sprintf("fill-%d", len(breakoutBars("AAPL"))+2) {
		t.Errorf("Campaign opened CausationID = %q, want the fill envelope's ID", campaignEnvelope.CausationID)
	}

	campaign := decodeCampaignOpened(t, campaignEnvelope)
	if campaign.CampaignID != wantID {
		t.Errorf("CampaignID = %q, want %q", campaign.CampaignID, wantID)
	}
	if campaign.InstrumentID != "AAPL" {
		t.Errorf("InstrumentID = %q, want AAPL", campaign.InstrumentID)
	}
	if campaign.ProposalID != proposalEnvelope.ID {
		t.Errorf("ProposalID = %q, want the proposal envelope's ID %q", campaign.ProposalID, proposalEnvelope.ID)
	}
	if campaign.SignalID != signalEnvelope.ID {
		t.Errorf("SignalID = %q, want the Signal envelope's ID %q", campaign.SignalID, signalEnvelope.ID)
	}
	if campaign.FillID != fill.FillID {
		t.Errorf("FillID = %q, want %q", campaign.FillID, fill.FillID)
	}
	if campaign.Rule != event.RuleCampaignOpenedFromFill {
		t.Errorf("Rule = %q, want %q", campaign.Rule, event.RuleCampaignOpenedFromFill)
	}
	// ADR 0006 is the acceptance criterion the ticket names explicitly.
	if campaign.ADR != "0006" {
		t.Errorf("ADR = %q, want %q (ADR 0006 freezes campaign N and the Unit size at first entry)", campaign.ADR, "0006")
	}
	if campaign.Direction != event.DirectionLong {
		t.Errorf("Direction = %q, want %q", campaign.Direction, event.DirectionLong)
	}

	// Frozen: exactly the proposal's N and quantity, carried across the fill
	// unchanged. Exact equality, not a tolerance — a Campaign whose N differs
	// in the last bit from the proposal's would put every ladder rung
	// fractionally out for the whole of the Campaign's life.
	wantN := breakoutFixtureN(t, cfg)
	if campaign.CampaignN != wantN {
		t.Errorf("CampaignN = %v, want exactly the proposal's %v", campaign.CampaignN, wantN)
	}
	if campaign.CampaignN != proposal.N {
		t.Errorf("CampaignN = %v, want the proposal's N %v", campaign.CampaignN, proposal.N)
	}
	if campaign.UnitQuantity != 133 {
		t.Errorf("UnitQuantity = %d, want the proposal's frozen 133", campaign.UnitQuantity)
	}
	if campaign.UnitQuantity != proposal.Quantity {
		t.Errorf("UnitQuantity = %d, want the proposal's quantity %d", campaign.UnitQuantity, proposal.Quantity)
	}
	if campaign.FilledQuantity != 133 {
		t.Errorf("FilledQuantity = %d, want 133 (the whole Unit filled)", campaign.FilledQuantity)
	}
	if campaign.Units != 1 {
		t.Errorf("Units = %d, want 1 (a Campaign opens holding its first Unit)", campaign.Units)
	}

	// The actual fill price, not the level the Signal fired at.
	if campaign.EntryPrice != campaignFillPrice {
		t.Errorf("EntryPrice = %v, want the fill price %v", campaign.EntryPrice, campaignFillPrice)
	}
	if campaign.EntryPrice == proposal.EntryLevel {
		t.Fatal("the fixture no longer distinguishes the fill price from the proposed entry level")
	}
	if campaign.StopMultiple != cfg.StopMultiple {
		t.Errorf("StopMultiple = %v, want %v", campaign.StopMultiple, cfg.StopMultiple)
	}
	// The stop is measured from what actually filled (#12 restates this; ADR
	// 0013 requires it for the ladders). Exact equality, in the expression
	// order CampaignOpenedPayload.Validate re-derives it in.
	wantStop := campaignFillPrice - cfg.StopMultiple*wantN
	if campaign.ProtectiveStop != wantStop {
		t.Errorf("ProtectiveStop = %v, want exactly %v (fill price - 2N)", campaign.ProtectiveStop, wantStop)
	}
	if campaign.ProtectiveStop == proposal.ProtectiveStopIntent {
		t.Error("ProtectiveStop equals the proposal's stop intent; the Campaign's stop must come from the actual fill, not the intended level")
	}

	// The Protective-Stop-set decision emitted alongside it must carry the
	// SAME Level, and the frozen numbers it was derived from.
	stopSet := decodeProtectiveStopSet(t, stopSetEnvelope)
	if stopSet.CampaignID != wantID {
		t.Errorf("Protective-Stop-set CampaignID = %q, want %q", stopSet.CampaignID, wantID)
	}
	if stopSet.InstrumentID != "AAPL" {
		t.Errorf("Protective-Stop-set InstrumentID = %q, want AAPL", stopSet.InstrumentID)
	}
	if stopSet.PreviousLevel != 0 {
		t.Errorf("Protective-Stop-set PreviousLevel = %v, want 0 (a first set)", stopSet.PreviousLevel)
	}
	if stopSet.EntryPrice != campaignFillPrice {
		t.Errorf("Protective-Stop-set EntryPrice = %v, want the fill price %v", stopSet.EntryPrice, campaignFillPrice)
	}
	if stopSet.Level != wantStop {
		t.Errorf("Protective-Stop-set Level = %v, want exactly %v (fill price - 2N, the same value the Campaign carries)", stopSet.Level, wantStop)
	}
	if stopSet.Rule != event.RuleProtectiveStopSetFromFill {
		t.Errorf("Protective-Stop-set Rule = %q, want %q", stopSet.Rule, event.RuleProtectiveStopSetFromFill)
	}
	if stopSet.ADR != "0006" {
		t.Errorf("Protective-Stop-set ADR = %q, want %q", stopSet.ADR, "0006")
	}
	if err := stopSet.Validate(); err != nil {
		t.Errorf("emitted Protective-Stop-set payload fails its own Validate(): %v", err)
	}
	if !campaign.OpenedAt.Equal(day(56)) {
		t.Errorf("OpenedAt = %v, want the fill's timestamp %v", campaign.OpenedAt, day(56))
	}
	if err := campaign.Validate(); err != nil {
		t.Errorf("emitted Campaign-opened payload fails its own Validate(): %v", err)
	}
}

// TestOnlyAFillOpensACampaignNotTheProposalThatPrecededIt is the ticket's
// named negative test: **position state changes only from recorded fill
// events, never at order-submission time**
// (docs/architecture.md's safety invariants; .greptile/rules.md,
// "request-driven state").
//
// The fixture is built so that the two implementations diverge visibly. Both
// instruments break out and are proposed on their bar 56; only MSFT's proposal
// is filled. A reducer that opened a Campaign when it emitted a proposal would
// have AAPL in a Campaign too, and AAPL's next breakout bar would then be
// suppressed by the "no new entry while a Campaign is open" gate. The correct
// reducer leaves AAPL with no Campaign at all, so its next breakout is
// proposed exactly as if the MSFT fill had never happened.
func TestOnlyAFillOpensACampaignNotTheProposalThatPrecededIt(t *testing.T) {
	t.Parallel()

	emitted := newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		bars(breakoutBars("MSFT")).
		fill(openingFill("MSFT")).
		bar(nextBreakoutBar("AAPL")).
		mustRun()

	campaigns := envelopesOfType(emitted, event.CampaignOpenedEventType)
	if len(campaigns) != 1 {
		t.Fatalf("got %d Campaign(s), want exactly 1: only the filled proposal may open one", len(campaigns))
	}
	if got := instrumentOf(t, campaigns[0]); got != "MSFT" {
		t.Fatalf("the Campaign opened for %q, want MSFT (the only instrument whose proposal filled)", got)
	}

	if got := countFor(t, emitted, event.CampaignOpenedEventType, "AAPL"); got != 0 {
		t.Errorf("AAPL has %d Campaign(s), want 0: a proposal that was never filled leaves no Campaign behind", got)
	}
	// AAPL's bar 57 must be evaluated and proposed exactly as usual: one
	// Signal and one proposal on bar 56, and another pair on bar 57.
	if got := countFor(t, emitted, event.SignalEventType, "AAPL"); got != 2 {
		t.Errorf("AAPL emitted %d Signal(s), want 2 (bars 56 and 57); an unfilled proposal must not suppress a later entry", got)
	}
	if got := countFor(t, emitted, event.TradeProposalEventType, "AAPL"); got != 2 {
		t.Errorf("AAPL emitted %d proposal(s), want 2 (bars 56 and 57)", got)
	}
	// And the proposal that was never filled is journalled as expired rather
	// than vanishing.
	if got := countFor(t, emitted, event.ProposalExpiredEventType, "AAPL"); got != 1 {
		t.Errorf("AAPL emitted %d proposal-expired event(s), want 1 (bar 56's unfilled proposal, superseded by bar 57)", got)
	}
}

// --- Proposal expiry ----------------------------------------------------

// TestProposalWithNoFillOpensNoCampaignAndExpiresWithItsBar covers the
// ticket's "proposal then no fill" case. ADR 0011 makes a Signal expire with
// its bar; the proposal a Signal produced inherits that lifetime, and the
// expiry is journalled rather than dropped so that the journal can explain why
// a fill arriving afterwards is rejected (see
// TestFillArrivingAfterItsProposalExpiredIsRejected).
func TestProposalWithNoFillOpensNoCampaignAndExpiresWithItsBar(t *testing.T) {
	t.Parallel()

	emitted := newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		bar(nextBreakoutBar("AAPL")).
		mustRun()

	if got := len(envelopesOfType(emitted, event.CampaignOpenedEventType)); got != 0 {
		t.Fatalf("got %d Campaign(s), want 0: no fill arrived", got)
	}

	// 55 warm-up bars, the breakout bar's 3 events, and bar 57's 4 (the
	// expiry of bar 56's proposal, then Setup-evaluated, Signal, Proposal).
	if len(emitted) != 62 {
		t.Fatalf("len(emitted) = %d, want 62", len(emitted))
	}

	expiredEnvelope := onlyEnvelopeOfType(t, emitted, event.ProposalExpiredEventType)
	// The expiry closes the previous bar's business before the new bar is
	// evaluated, mirroring ADR 0010's exits-before-entries ordering.
	tail := emitted[len(emitted)-4:]
	wantOrder := []string{
		event.ProposalExpiredEventType,
		event.SetupEvaluatedEventType,
		event.SignalEventType,
		event.TradeProposalEventType,
	}
	for i, want := range wantOrder {
		if tail[i].Type != want {
			t.Fatalf("emission %d of the final bar = %q, want %q", i, tail[i].Type, want)
		}
	}

	if expiredEnvelope.ID != "proposal-expired:AAPL:2026-02-28T00:00:00.000000000Z" {
		t.Errorf("expiry ID = %q, want it keyed to the bar that superseded the proposal", expiredEnvelope.ID)
	}
	if !expiredEnvelope.EventTime.Equal(day(57)) {
		t.Errorf("expiry EventTime = %v, want the superseding bar's %v", expiredEnvelope.EventTime, day(57))
	}

	expired := decodeProposalExpired(t, expiredEnvelope)
	if expired.InstrumentID != "AAPL" {
		t.Errorf("expiry InstrumentID = %q, want AAPL", expired.InstrumentID)
	}
	if expired.ProposalID != testDecisionID("proposal", "AAPL", day(56)) {
		t.Errorf("expiry ProposalID = %q, want bar 56's proposal", expired.ProposalID)
	}
	if expired.SignalID != testDecisionID("signal", "AAPL", day(56)) {
		t.Errorf("expiry SignalID = %q, want bar 56's Signal", expired.SignalID)
	}
	if !expired.PeriodEnd.Equal(day(56)) {
		t.Errorf("expiry PeriodEnd = %v, want the expiring proposal's bar %v", expired.PeriodEnd, day(56))
	}
	if !expired.ExpiredAt.Equal(day(57)) {
		t.Errorf("expiry ExpiredAt = %v, want %v", expired.ExpiredAt, day(57))
	}
	if expired.Reason != event.ExpiryReasonSupersededByNextBar {
		t.Errorf("expiry Reason = %q, want %q", expired.Reason, event.ExpiryReasonSupersededByNextBar)
	}
	if expired.ADR != "0011" {
		t.Errorf("expiry ADR = %q, want %q", expired.ADR, "0011")
	}
	if expired.Quantity != 133 || expired.EntryLevel != 200 {
		t.Errorf("expiry Quantity/EntryLevel = %d/%v, want 133/200 (what was proposed and not taken)", expired.Quantity, expired.EntryLevel)
	}
	if err := expired.Validate(); err != nil {
		t.Errorf("emitted proposal-expired payload fails its own Validate(): %v", err)
	}
}

// TestFillArrivingAfterItsProposalExpiredIsRejected is the reason the expiry
// event exists. Once the next bar has superseded a proposal there is nothing
// left to fill: acting on the fill would open a Campaign at a price and a
// volatility reading the strategy no longer stands behind. The run fails
// closed, and the journal already contains the expiry that explains it.
//
// Both shapes of "afterwards" are covered, because they take different
// branches: the next bar may itself be a breakout, replacing the expired
// proposal with a new one that the late fill does not name, or it may be
// quiet, leaving nothing outstanding at all.
func TestFillArrivingAfterItsProposalExpiredIsRejected(t *testing.T) {
	t.Parallel()

	t.Run("superseded by a new proposal", func(t *testing.T) {
		t.Parallel()

		newStream(t, validConfigurationPayload()).
			bars(breakoutBars("AAPL")).
			bar(nextBreakoutBar("AAPL")).
			fill(openingFill("AAPL")).
			wantRunError("AAPL", "2026-02-27", "the pending trade proposal is", "2026-02-28")
	})

	t.Run("nothing outstanding at all", func(t *testing.T) {
		t.Parallel()

		newStream(t, validConfigurationPayload()).
			bars(breakoutBars("AAPL")).
			bar(quietBar("AAPL")).
			fill(openingFill("AAPL")).
			wantRunError("AAPL", "no pending trade proposal")
	})
}

// --- Partial fills ------------------------------------------------------

// TestPartialFillOpensACampaignSizedToTheFilledQuantity covers the ticket's
// "proposal then partial fill" case: the Campaign exists for what actually
// executed, while the Unit share count stays frozen at the full proposed size
// because ADR 0006 computes the whole Add Ladder and Stop Ladder from it.
func TestPartialFillOpensACampaignSizedToTheFilledQuantity(t *testing.T) {
	t.Parallel()

	partial := openingFill("AAPL")
	partial.Quantity = 100

	emitted := newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(partial).
		mustRun()

	campaign := decodeCampaignOpened(t, onlyEnvelopeOfType(t, emitted, event.CampaignOpenedEventType))
	if campaign.FilledQuantity != 100 {
		t.Errorf("FilledQuantity = %d, want 100 (what actually executed)", campaign.FilledQuantity)
	}
	if campaign.UnitQuantity != 133 {
		t.Errorf("UnitQuantity = %d, want the frozen 133: a partial fill does not resize the Unit the ladders are computed from (ADR 0006)", campaign.UnitQuantity)
	}
	if campaign.Units != 1 {
		t.Errorf("Units = %d, want 1: a Unit is indivisible as a risk measure (ADR 0010), and FilledQuantity records how much of it filled", campaign.Units)
	}
	if err := campaign.Validate(); err != nil {
		t.Errorf("emitted Campaign-opened payload fails its own Validate(): %v", err)
	}
}

// TestSecondPartialFillWithADifferentFillIDIsRejected pins the deliberate
// limitation this ticket ships with: accumulating successive partial fills
// into one Campaign is deferred to its own issue, and until it lands a second
// partial fails the run rather than silently opening a second Campaign for the
// same instrument (which would double the position and halve nothing about the
// risk).
func TestSecondPartialFillWithADifferentFillIDIsRejected(t *testing.T) {
	t.Parallel()

	first := openingFill("AAPL")
	first.Quantity = 100
	second := openingFill("AAPL")
	second.Quantity = 33
	second.FillID = "sim-fill-0002"

	newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(first).
		fill(second).
		wantRunError("AAPL", "sim-fill-0002", "already opened")
}

// --- Idempotency --------------------------------------------------------

// TestDuplicateFillIsAnIdempotentNoOp covers docs/architecture.md's
// "duplicate decision and order identifiers must be idempotent". Duplicate
// delivery is expected from any transport, so the same fill arriving twice
// must neither open a second Campaign nor fail the run.
func TestDuplicateFillIsAnIdempotentNoOp(t *testing.T) {
	t.Parallel()

	emitted := newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		fill(openingFill("AAPL")).
		mustRun()

	if got := len(envelopesOfType(emitted, event.CampaignOpenedEventType)); got != 1 {
		t.Fatalf("got %d Campaign(s), want exactly 1 despite the duplicate delivery", got)
	}
	// The second delivery must emit nothing at all, not merely nothing new:
	// the emission count is identical to the single-delivery run (60: the
	// warm-up and breakout bars' 58, plus the opening fill's Campaign-opened
	// and Protective-Stop-set).
	if len(emitted) != 60 {
		t.Errorf("len(emitted) = %d, want 60 (the duplicate fill emits nothing)", len(emitted))
	}
}

// TestFillReusingAFillIDWithDifferentContentsIsRejected draws the line around
// idempotency: it means "the same fact delivered twice", not "any fact
// carrying an identifier we have seen". A producer that reuses a fill id for a
// different execution is a reconciliation failure
// (docs/architecture.md), and treating it as a duplicate would silently
// discard a real execution.
func TestFillReusingAFillIDWithDifferentContentsIsRejected(t *testing.T) {
	t.Parallel()

	repriced := openingFill("AAPL")
	repriced.Price = campaignFillPrice + 1

	newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		fill(repriced).
		wantRunError("sim-fill-0001", "differ")
}

// --- Fail-closed rules --------------------------------------------------

// TestFillForAnUnknownProposalIsRejected: a fill for an order this system
// never proposed is a reconciliation failure, not something to absorb
// (docs/architecture.md's safety invariants — material reconciliation
// differences force safe mode).
func TestFillForAnUnknownProposalIsRejected(t *testing.T) {
	t.Parallel()

	unknown := openingFill("AAPL")
	unknown.ProposalID = "proposal:AAPL:1999-01-01T00:00:00.000000000Z"

	newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(unknown).
		wantRunError("AAPL", "1999-01-01")
}

// TestFillNamingADifferentProposalWhileACampaignIsOpenIsRejected is the same
// rule again once the instrument is committed: a fill for some other order it
// never had outstanding must not be quietly folded into the Campaign that does
// exist.
func TestFillNamingADifferentProposalWhileACampaignIsOpenIsRejected(t *testing.T) {
	t.Parallel()

	stray := openingFill("AAPL")
	stray.ProposalID = "proposal:AAPL:1999-01-01T00:00:00.000000000Z"
	stray.FillID = "sim-fill-0002"

	newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		fill(stray).
		wantRunError("AAPL", "1999-01-01", "already open from proposal")
}

// TestFillThatWouldLeaveThePositionUnprotectedIsRejected covers the one way a
// fill can be individually valid and still produce a Campaign that must not
// exist: a price low enough that the Protective Stop, measured from the actual
// fill, lands at or below zero. A long equity cannot be stopped out there, so
// the Unit would in fact risk the whole position while the journal recorded a
// stop.
//
// It cannot arise from a producer honouring ADR 0005 — a long entry fills at
// max(level, open), and the proposal's stop intent was already required to be
// positive — so this is a misbehaving-producer check, and failing the run is
// the only answer that does not leave an unprotected position behind.
//
// #12 moved where this is caught: openCampaign now calls
// sizing.ProtectiveStopLevel BEFORE building event.CampaignOpenedPayload at
// all (rather than building an invalid payload and letting its own Validate
// reject it), so the error surfaces from the arithmetic seam's own
// fail-closed guard.
func TestFillThatWouldLeaveThePositionUnprotectedIsRejected(t *testing.T) {
	t.Parallel()

	underwater := openingFill("AAPL")
	underwater.Price = 1

	newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(underwater).
		wantRunError("cannot compute a protective stop", "is not positive")
}

// TestFillForAnInstrumentWithNoProposalIsRejected is the same rule for an
// instrument the reducer has never seen a bar for: there is nothing it could
// possibly have proposed.
func TestFillForAnInstrumentWithNoProposalIsRejected(t *testing.T) {
	t.Parallel()

	stray := openingFill("TSLA")

	newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(stray).
		wantRunError("TSLA")
}

// TestFillExceedingTheProposalQuantityIsRejected: an over-execution puts more
// capital at risk than the sizing arithmetic budgeted for, which is the one
// direction the truncation rules never permit (TradeProposalPayload's
// invariant 4). Recording it as a Campaign would launder the excess into the
// journal as if it had been sized.
func TestFillExceedingTheProposalQuantityIsRejected(t *testing.T) {
	t.Parallel()

	oversized := openingFill("AAPL")
	oversized.Quantity = 134

	newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(oversized).
		wantRunError("134", "133")
}

// TestFillWithAMismatchedDirectionIsRejected: a short fill against a long
// proposal is not a smaller version of the same trade, it is a different
// position.
func TestFillWithAMismatchedDirectionIsRejected(t *testing.T) {
	t.Parallel()

	// A direction the payload itself accepts would be needed for the reducer's
	// own check to be the one that fires; the Baseline is long-only, so the
	// payload rejects "short" first. Both fail closed, which is the point: the
	// test asserts the run stops, and names the direction in the message.
	mismatched := openingFill("AAPL")
	mismatched.Direction = "short"

	newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(mismatched).
		wantRunError("direction")
}

// TestReducerRejectsFillWithWrongSchemaVersion applies ADR 0015's rule at the
// payload level, as the configuration and bar inputs already do: a schema this
// build was not written against is rejected, never silently interpreted as the
// current one.
func TestReducerRejectsFillWithWrongSchemaVersion(t *testing.T) {
	t.Parallel()

	// event.FillSchemaVersion is 2 (#12 bumped it for Kind/CampaignID);
	// SchemaVersion 0 would also be rejected, but at Envelope.Validate()
	// ("schema version must be positive") rather than by the check under
	// test, so schema 1 — the version this build no longer accepts — is used
	// here instead: a distinct positive-but-wrong value that also documents
	// what actually changed.
	newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fillAtSchema(openingFill("AAPL"), 1).
		wantRunError("schema version", "1", "2")
}

// TestReducerRejectsFillBeforeConfiguration: the reducer cannot know what a
// fill means before it knows the configuration the proposal was sized under.
func TestReducerRejectsFillBeforeConfiguration(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	_, err = engine.Run(context.Background(), []event.Envelope{fillEnvelope(t, 1, openingFill("AAPL"))})
	if err == nil {
		t.Fatal("Run() error = nil, want an error for a fill before any configuration event")
	}
	if !strings.Contains(err.Error(), "configuration") {
		t.Errorf("Run() error = %v, want it to name the missing configuration", err)
	}
}

// TestReducerRejectsInvalidFillPayload: a payload that fails its own contract
// never reaches the Campaign logic.
func TestReducerRejectsInvalidFillPayload(t *testing.T) {
	t.Parallel()

	invalid := openingFill("AAPL")
	invalid.Quantity = 0

	newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(invalid).
		wantRunError("invalid fill payload", "quantity")
}

// TestReducerRejectsUndecodableFillPayload covers the decode step itself.
func TestReducerRejectsUndecodableFillPayload(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	broken := fillEnvelope(t, 2, openingFill("AAPL"))
	broken.Payload = json.RawMessage(`{"quantity":"one hundred"}`)
	broken.PayloadHash = event.HashPayload(broken.Payload)

	_, err = engine.Run(context.Background(), []event.Envelope{configEnvelope(t, 1, day(0)), broken})
	if err == nil {
		t.Fatal("Run() error = nil, want a decode error")
	}
	if !strings.Contains(err.Error(), "decode fill payload") {
		t.Errorf("Run() error = %v, want substring %q", err, "decode fill payload")
	}
}

// --- When a fill could have executed (PR #69 review) --------------------

// TestFillInsideTheDecisionBarOpensTheCampaignAtThatIntrabarTime is ADR 0005's
// case, and the reason this review finding could not be fixed the way it was
// literally written.
//
// The entry is a **resting order that fills inside the breakout bar** — that
// is the whole fill model — so a fill timestamped earlier than the decision
// bar's period end is the normal backtest case, not an anomaly. Rejecting
// `FilledAt < PeriodEnd` would reject every legitimate fill #18's simulator
// will ever produce.
//
// The Campaign therefore carries the intrabar time as its own: its `OpenedAt`,
// its envelope `EventTime` and its deterministic id are all the moment the
// position came into being, which is when the order executed and not when the
// bar it executed inside happened to close.
func TestFillInsideTheDecisionBarOpensTheCampaignAtThatIntrabarTime(t *testing.T) {
	t.Parallel()

	// Six hours before the decision bar's period end, and so eighteen hours
	// after the previous bar's: strictly inside the bar the resting order was
	// live in.
	intrabar := day(56).Add(-6 * time.Hour)
	fill := openingFill("AAPL")
	fill.FilledAt = intrabar

	emitted := newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(fill).
		mustRun()

	campaignEnvelope := onlyEnvelopeOfType(t, emitted, event.CampaignOpenedEventType)
	wantID := "campaign:AAPL:2026-02-26T18:00:00.000000000Z"
	if campaignEnvelope.ID != wantID {
		t.Errorf("Campaign opened ID = %q, want %q (keyed to when the position came into being)", campaignEnvelope.ID, wantID)
	}
	if !campaignEnvelope.EventTime.Equal(intrabar) {
		t.Errorf("Campaign opened EventTime = %v, want the intrabar fill time %v", campaignEnvelope.EventTime, intrabar)
	}

	campaign := decodeCampaignOpened(t, campaignEnvelope)
	if !campaign.OpenedAt.Equal(intrabar) {
		t.Errorf("OpenedAt = %v, want the intrabar fill time %v", campaign.OpenedAt, intrabar)
	}
	if err := campaign.Validate(); err != nil {
		t.Errorf("emitted Campaign-opened payload fails its own Validate(): %v", err)
	}
}

// TestFillAtTheDecisionBarsPeriodEndIsAccepted pins the upper end of the
// window, which is where a daily-bar backtest timestamps its fills by
// convention and where a live fill's timestamp would sit at the earliest.
func TestFillAtTheDecisionBarsPeriodEndIsAccepted(t *testing.T) {
	t.Parallel()

	fill := openingFill("AAPL")
	if !fill.FilledAt.Equal(day(56)) {
		t.Fatalf("fixture fill is timestamped %v, want the decision bar's period end %v", fill.FilledAt, day(56))
	}

	emitted := newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(fill).
		mustRun()

	if got := len(envelopesOfType(emitted, event.CampaignOpenedEventType)); got != 1 {
		t.Fatalf("got %d Campaign(s), want 1", got)
	}
}

// TestFillPredatingTheBarTheOrderCouldHaveExecutedInIsRejected is the half of
// the review finding that was real, correctly bounded.
//
// A fill cannot have executed before the bar it executed *inside* began. The
// bound is therefore the period end of the bar **preceding** the decision bar
// — the moment the decision bar opened — and not the decision bar's own period
// end, which would reject ADR 0005's ordinary intrabar fill (see
// TestFillInsideTheDecisionBarOpensTheCampaignAtThatIntrabarTime). Accepting a
// fill from before then would journal a Campaign whose identity, OpenedAt and
// EventTime predate the bar whose data produced the decision authorising it.
func TestFillPredatingTheBarTheOrderCouldHaveExecutedInIsRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		filledAt time.Time
	}{
		{
			// Exactly the previous bar's period end: the decision bar had not
			// opened yet, so the boundary is exclusive.
			name:     "at the previous bar's period end",
			filledAt: day(55),
		},
		{
			name:     "inside the previous bar",
			filledAt: day(55).Add(-1 * time.Hour),
		},
		{
			name:     "long before the proposal existed",
			filledAt: day(1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fill := openingFill("AAPL")
			fill.FilledAt = tt.filledAt

			newStream(t, validConfigurationPayload()).
				bars(breakoutBars("AAPL")).
				fill(fill).
				wantRunError("AAPL", "sim-fill-0001", "predates")
		})
	}
}

// TestFillAfterTheDecisionBarButBeforeTheNextIsAccepted is the live
// resting-order case, and the reason the upper end of the window cannot be
// checked when the fill arrives.
//
// Live, the order proposed on the decision bar's close rests into the
// following session and fills there — after the proposal's `PeriodEnd`, and
// before the next completed bar exists. At the moment the fill is applied the
// reducer has no way to know when that bar will end: there is no bar-length
// configuration, and the next bar has not arrived. So the fill is accepted on
// its own terms, and the bar that follows confirms it.
func TestFillAfterTheDecisionBarButBeforeTheNextIsAccepted(t *testing.T) {
	t.Parallel()

	// Twelve hours after the decision bar closed, twelve before the next bar
	// does: inside the session the resting order was live in.
	nextSession := day(56).Add(12 * time.Hour)
	fill := openingFill("AAPL")
	fill.FilledAt = nextSession

	emitted := newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(fill).
		bar(nextBreakoutBar("AAPL")).
		mustRun()

	campaign := decodeCampaignOpened(t, onlyEnvelopeOfType(t, emitted, event.CampaignOpenedEventType))
	if !campaign.OpenedAt.Equal(nextSession) {
		t.Errorf("OpenedAt = %v, want the next session's fill time %v", campaign.OpenedAt, nextSession)
	}
	// The bar that follows is applied without error — it simply emits nothing,
	// because the instrument is now in a Campaign.
	if len(emitted) != 60 {
		t.Errorf("len(emitted) = %d, want 60 (the bar after the fill is processed and emits nothing)", len(emitted))
	}
}

// TestFillAtTheNextBarsPeriodEndIsAccepted pins the inclusive upper boundary: a
// fill at the very close of the bar that confirms it is still a fill that
// happened within that bar.
func TestFillAtTheNextBarsPeriodEndIsAccepted(t *testing.T) {
	t.Parallel()

	fill := openingFill("AAPL")
	fill.FilledAt = day(57)

	emitted := newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(fill).
		bar(nextBreakoutBar("AAPL")).
		mustRun()

	if got := len(envelopesOfType(emitted, event.CampaignOpenedEventType)); got != 1 {
		t.Fatalf("got %d Campaign(s), want 1", got)
	}
}

// TestBarPredatingTheCampaignsOpeningFillFailsClosed is the upper bound of the
// window, enforced at the earliest point the reducer can know it (a second PR
// #69 review finding).
//
// A fill timestamped after a bar that has not yet completed cannot have
// happened: the execution claims a moment the stream has not reached. But the
// reducer cannot see that when the fill arrives — the next bar does not exist
// yet and no bar length is configured — so the fill is accepted then, and the
// contradiction is caught by the very next bar for that instrument, which
// fails the run rather than continuing with a Campaign whose id, OpenedAt and
// EventTime sit in the stream's future.
//
// Both halves are asserted, because the point is the split: the same fill is
// accepted on its own and rejected once the bar that contradicts it arrives.
func TestBarPredatingTheCampaignsOpeningFillFailsClosed(t *testing.T) {
	t.Parallel()

	// A day beyond the next bar's period end: unknowable at fill time,
	// contradicted the moment that bar arrives.
	fill := openingFill("AAPL")
	fill.FilledAt = day(58)

	// Accepted on its own terms, because nothing yet contradicts it.
	emitted := newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(fill).
		mustRun()
	if got := len(envelopesOfType(emitted, event.CampaignOpenedEventType)); got != 1 {
		t.Fatalf("got %d Campaign(s) before the contradicting bar, want 1: the fill is unknowable at the time it arrives", got)
	}

	// And rejected as soon as a bar shows the timestamp was impossible.
	newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(fill).
		bar(nextBreakoutBar("AAPL")).
		wantRunError("AAPL", "campaign:AAPL:2026-03-01T00:00:00.000000000Z", "predates")
}

// --- No new entry while a Campaign is open ------------------------------

// TestNoSignalOrProposalWhileACampaignIsOpen covers the ticket's rule that an
// instrument already in a Campaign takes no new entry. CONTEXT.md defines a
// Setup as an Eligible instrument *not* in a Campaign, so no Setup-evaluated
// event is emitted for it either: the reducer keeps tracking N and the Entry
// Channel (later tickets need them) but says nothing about the bar until #12
// adds the Campaign's own stop evaluation.
func TestNoSignalOrProposalWhileACampaignIsOpen(t *testing.T) {
	t.Parallel()

	withCampaign := newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(nextBreakoutBar("AAPL")).
		mustRun()

	// The comparison run is the identical fixture without the fill, where bar
	// 57 is a breakout in its own right and is proposed as usual. The
	// difference between the two is exactly what the Campaign suppressed.
	withoutCampaign := newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		bar(nextBreakoutBar("AAPL")).
		mustRun()

	if got := countFor(t, withoutCampaign, event.SignalEventType, "AAPL"); got != 2 {
		t.Fatalf("the comparison run emitted %d Signal(s), want 2: the fixture must make bar 57 a genuine breakout", got)
	}

	if got := countFor(t, withCampaign, event.SignalEventType, "AAPL"); got != 1 {
		t.Errorf("AAPL emitted %d Signal(s) with a Campaign open, want 1 (only the entry bar's)", got)
	}
	if got := countFor(t, withCampaign, event.TradeProposalEventType, "AAPL"); got != 1 {
		t.Errorf("AAPL emitted %d proposal(s) with a Campaign open, want 1", got)
	}
	if got := countFor(t, withCampaign, event.SetupEvaluatedEventType, "AAPL"); got != 56 {
		t.Errorf("AAPL emitted %d Setup-evaluated event(s), want 56 (bars 1..56 only): an instrument in a Campaign is not a Setup", got)
	}
	// Bar 57 therefore emits nothing at all: the run's last emission is still
	// the Protective-Stop-set that followed the Campaign the fill opened.
	if last := withCampaign[len(withCampaign)-1]; last.Type != event.ProtectiveStopSetEventType {
		t.Errorf("last emission is %q, want the Protective-Stop-set: the bar after it must emit nothing", last.Type)
	}
	if len(withCampaign) != 60 {
		t.Errorf("len(emitted) = %d, want 60: the bar arriving during a Campaign adds no emission", len(withCampaign))
	}
}

// TestASecondInstrumentIsUnaffectedByAnothersCampaign: Campaign state is per
// instrument. An open Campaign in AAPL must not gate MSFT's own entry.
func TestASecondInstrumentIsUnaffectedByAnothersCampaign(t *testing.T) {
	t.Parallel()

	emitted := newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bars(breakoutBars("MSFT")).
		bar(nextBreakoutBar("AAPL")).
		mustRun()

	if got := countFor(t, emitted, event.CampaignOpenedEventType, "AAPL"); got != 1 {
		t.Errorf("AAPL has %d Campaign(s), want 1", got)
	}
	if got := countFor(t, emitted, event.SignalEventType, "MSFT"); got != 1 {
		t.Errorf("MSFT emitted %d Signal(s), want 1: AAPL's Campaign must not gate another instrument", got)
	}
	if got := countFor(t, emitted, event.TradeProposalEventType, "MSFT"); got != 1 {
		t.Errorf("MSFT emitted %d proposal(s), want 1", got)
	}
	if got := countFor(t, emitted, event.SetupEvaluatedEventType, "MSFT"); got != 56 {
		t.Errorf("MSFT emitted %d Setup-evaluated event(s), want 56", got)
	}
	// AAPL's bar 57 is still suppressed by its own Campaign.
	if got := countFor(t, emitted, event.SignalEventType, "AAPL"); got != 1 {
		t.Errorf("AAPL emitted %d Signal(s), want 1", got)
	}
	// And MSFT, which has no Campaign, keeps a pending proposal that nothing
	// has expired yet — so no Campaign was invented for it.
	if got := countFor(t, emitted, event.CampaignOpenedEventType, "MSFT"); got != 0 {
		t.Errorf("MSFT has %d Campaign(s), want 0", got)
	}
}

// --- Replay equivalence -------------------------------------------------

// TestReplayingTheCampaignFixtureTwiceYieldsByteIdenticalEmissions is the
// ticket's "replaying the same fixture reconstructs identical Campaign state"
// criterion, asserted the strongest way available: the two runs' emitted
// envelopes must be byte-identical, which covers the deterministic Campaign id
// and every frozen number in the payload.
func TestReplayingTheCampaignFixtureTwiceYieldsByteIdenticalEmissions(t *testing.T) {
	t.Parallel()

	build := func() []event.Envelope {
		return newStream(t, validConfigurationPayload()).
			bars(breakoutBars("AAPL")).
			fill(openingFill("AAPL")).
			bar(nextBreakoutBar("AAPL")).
			mustRun()
	}

	first, second := build(), build()
	if len(first) != len(second) {
		t.Fatalf("emission counts differ: %d and %d", len(first), len(second))
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

// --- Stop fills close a Campaign (#12) -----------------------------------
//
// Every test below drives the reducer through replay.Engine.Run, the same
// event seam #11's fixtures use. None of them ever supplies a bar whose low
// crosses the stop level and expects a fill to appear on its own —
// TestABarsLowThroughTheStopWithNoStopFillLeavesTheCampaignOpen is the
// fixture that would fail if the reducer ever started deciding fills from
// bar data itself (ADR 0005; #18 owns that decision).

// TestStopFillClosesTheCampaignWithReasonStopAndRealisedResult is #12's
// primary event-seam test: entry, then a stop fill that gaps THROUGH the
// Protective Stop level (proving the exit records what was filled, not the
// level — ADR 0005's gap rule), producing a Campaign-exited event with
// Reason "stop" and a hand-derivable realised result.
func TestStopFillClosesTheCampaignWithReasonStopAndRealisedResult(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	stopFilledAt := day(57)
	stop := closingStopFill("AAPL", campaignID, campaignN, stopFilledAt)

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		fill(stop).
		mustRun()

	// The breakout bar's 3, the opening fill's 2 (Campaign-opened,
	// Protective-Stop-set), and the stop fill's 1 (Campaign-exited).
	exitEnvelope := onlyEnvelopeOfType(t, emitted, event.CampaignExitedEventType)
	if exitEnvelope.Sequence != uint64(len(emitted)) {
		t.Errorf("Campaign exited Sequence = %d, want %d (it is the last emission)", exitEnvelope.Sequence, len(emitted))
	}
	wantExitID := "campaign-exited:AAPL:2026-02-28T00:00:00.000000000Z"
	if exitEnvelope.ID != wantExitID {
		t.Errorf("Campaign exited ID = %q, want %q (deterministic, keyed to the closing fill)", exitEnvelope.ID, wantExitID)
	}
	if !exitEnvelope.EventTime.Equal(stopFilledAt) {
		t.Errorf("Campaign exited EventTime = %v, want the stop fill's %v", exitEnvelope.EventTime, stopFilledAt)
	}
	if exitEnvelope.CausationID != fmt.Sprintf("fill-%d", len(breakoutBars("AAPL"))+3) {
		t.Errorf("Campaign exited CausationID = %q, want the stop fill envelope's ID", exitEnvelope.CausationID)
	}

	exited := decodeCampaignExited(t, exitEnvelope)
	if exited.CampaignID != campaignID {
		t.Errorf("CampaignID = %q, want %q", exited.CampaignID, campaignID)
	}
	if exited.InstrumentID != "AAPL" {
		t.Errorf("InstrumentID = %q, want AAPL", exited.InstrumentID)
	}
	if exited.FillID != stop.FillID {
		t.Errorf("FillID = %q, want the stop fill's %q", exited.FillID, stop.FillID)
	}
	if exited.Reason != event.ExitReasonStop {
		t.Errorf("Reason = %q, want %q", exited.Reason, event.ExitReasonStop)
	}
	if exited.Rule != event.RuleCampaignExitedByStop {
		t.Errorf("Rule = %q, want %q", exited.Rule, event.RuleCampaignExitedByStop)
	}
	if exited.ADR != "0005" {
		t.Errorf("ADR = %q, want %q (the fill model: gaps fill at the open)", exited.ADR, "0005")
	}
	if exited.EntryPrice != campaignFillPrice {
		t.Errorf("EntryPrice = %v, want the opening fill's %v", exited.EntryPrice, campaignFillPrice)
	}
	// The headline assertion: the exit price is what actually filled, which
	// gapped THROUGH the Protective Stop level, so it is strictly below it.
	if exited.ExitPrice != stop.Price {
		t.Errorf("ExitPrice = %v, want the stop fill's own price %v", exited.ExitPrice, stop.Price)
	}
	wantStopLevel := campaignFillPrice - cfg.StopMultiple*campaignN
	if exited.ProtectiveStopLevel != wantStopLevel {
		t.Errorf("ProtectiveStopLevel = %v, want exactly %v (the level that was in force)", exited.ProtectiveStopLevel, wantStopLevel)
	}
	if !(exited.ExitPrice < exited.ProtectiveStopLevel) {
		t.Fatalf("the fixture no longer gaps the exit price through the protective stop level (%v against %v); the headline assertion above is vacuous without it", exited.ExitPrice, exited.ProtectiveStopLevel)
	}
	if exited.Quantity != 133 {
		t.Errorf("Quantity = %d, want the whole 133-share campaign", exited.Quantity)
	}
	if exited.CampaignN != campaignN {
		t.Errorf("CampaignN = %v, want the campaign's frozen %v", exited.CampaignN, campaignN)
	}
	if exited.DollarsPerPoint != cfg.DollarsPerPoint {
		t.Errorf("DollarsPerPoint = %v, want %v", exited.DollarsPerPoint, cfg.DollarsPerPoint)
	}

	// Hand-derivable from the fixture's own numbers, exactly the expression
	// order the reducer and event.CampaignExitedPayload.Validate share.
	wantRealisedResult := float64(133) * (stop.Price - campaignFillPrice) * cfg.DollarsPerPoint
	if exited.RealisedResult != wantRealisedResult {
		t.Errorf("RealisedResult = %v, want exactly %v", exited.RealisedResult, wantRealisedResult)
	}
	if !(exited.RealisedResult < 0) {
		t.Errorf("RealisedResult = %v, want negative (the fixture stops out at a loss)", exited.RealisedResult)
	}
	wantRealisedResultInN := (stop.Price - campaignFillPrice) / campaignN
	if exited.RealisedResultInN != wantRealisedResultInN {
		t.Errorf("RealisedResultInN = %v, want exactly %v", exited.RealisedResultInN, wantRealisedResultInN)
	}
	if err := exited.Validate(); err != nil {
		t.Errorf("emitted Campaign-exited payload fails its own Validate(): %v", err)
	}
}

// TestAfterAStopExitTheInstrumentSignalsAgain covers the ticket's "the
// instrument is a Setup again" requirement: once the Campaign exits, the
// very next breakout bar produces a fresh Signal and proposal, exactly as if
// no Campaign had ever existed.
func TestAfterAStopExitTheInstrumentSignalsAgain(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	stop := closingStopFill("AAPL", campaignID, campaignN, day(57))

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		fill(stop).
		bar(nextBreakoutBar("AAPL")).
		mustRun()

	if got := countFor(t, emitted, event.CampaignExitedEventType, "AAPL"); got != 1 {
		t.Fatalf("got %d Campaign-exited event(s), want exactly 1", got)
	}
	// Both the entry fill (day 56) and the closing stop fill (day 57) land
	// before the fixture's next bar (also day 57, see nextBreakoutBar's
	// doc comment) is even processed, so no bar in this fixture is ever
	// evaluated WHILE the Campaign is open — every one of the 56 warm-up
	// and breakout bars, plus the bar after the exit, gets an ordinary
	// Setup-evaluated event. The point this test exists to prove is what
	// happens AFTER the exit, not the suppression while open (that is
	// TestNoSignalOrProposalWhileACampaignIsOpen's job): the bar after the
	// exit is itself a fresh breakout (nextBreakoutBar's doc comment), so it
	// produces a second Signal and proposal exactly as if no Campaign had
	// ever existed.
	if got := countFor(t, emitted, event.SetupEvaluatedEventType, "AAPL"); got != 57 {
		t.Errorf("AAPL emitted %d Setup-evaluated event(s), want 57 (every bar, since none falls while the campaign is open)", got)
	}
	if got := countFor(t, emitted, event.SignalEventType, "AAPL"); got != 2 {
		t.Errorf("AAPL emitted %d Signal(s), want 2 (the original entry, and the fresh one after the exit)", got)
	}
	if got := countFor(t, emitted, event.TradeProposalEventType, "AAPL"); got != 2 {
		t.Errorf("AAPL emitted %d proposal(s), want 2", got)
	}
}

// TestDuplicateStopFillIsAnIdempotentNoOp mirrors
// TestDuplicateFillIsAnIdempotentNoOp for a stop fill closing rather than
// opening a Campaign (docs/architecture.md: duplicate identifiers must be
// idempotent).
func TestDuplicateStopFillIsAnIdempotentNoOp(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	stop := closingStopFill("AAPL", campaignID, campaignN, day(57))

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		fill(stop).
		fill(stop).
		mustRun()

	if got := len(envelopesOfType(emitted, event.CampaignExitedEventType)); got != 1 {
		t.Fatalf("got %d Campaign-exited event(s), want exactly 1 despite the duplicate delivery", got)
	}
	// 55 x 1, the breakout bar's 3, the opening fill's 2, the stop fill's 1:
	// the duplicate delivery emits nothing at all, not merely nothing new.
	if len(emitted) != 61 {
		t.Errorf("len(emitted) = %d, want 61 (the duplicate stop fill emits nothing)", len(emitted))
	}
}

// TestStopFillReusingAFillIDWithDifferentContentsIsRejected mirrors
// TestFillReusingAFillIDWithDifferentContentsIsRejected for a stop fill: a
// reused fill id carrying different contents is a reconciliation failure,
// not a duplicate.
func TestStopFillReusingAFillIDWithDifferentContentsIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	stop := closingStopFill("AAPL", campaignID, campaignN, day(57))
	repriced := stop
	repriced.Price = stop.Price + 1

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		fill(stop).
		fill(repriced).
		wantRunError(stop.FillID, "differ")
}

// TestStopFillForAnUnknownCampaignIsRejected covers the ticket's "stop fill
// for an unknown or already-closed Campaign errors" case, the "unknown"
// half: no Campaign of ANY id has ever existed for AAPL (no entry fill was
// ever sent), so state.campaign is nil for the reason "never opened", not
// "closed" — the other half, TestStopFillForAnAlreadyClosedCampaignIsRejected,
// covers that distinct path through the same guard.
func TestStopFillForAnUnknownCampaignIsRejected(t *testing.T) {
	t.Parallel()

	stray := event.FillPayload{
		InstrumentID: "AAPL",
		Kind:         event.FillKindStop,
		CampaignID:   "campaign:AAPL:1999-01-01T00:00:00.000000000Z",
		FillID:       "sim-fill-9999",
		Direction:    event.DirectionLong,
		Quantity:     133,
		Price:        100,
		FilledAt:     day(57),
	}

	newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(stray).
		wantRunError("AAPL", "1999-01-01", "no open campaign")
}

// TestStopFillForAnAlreadyClosedCampaignIsRejected covers the "already
// closed" half: a second, different stop fill arriving after the Campaign
// has already exited.
func TestStopFillForAnAlreadyClosedCampaignIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	stop := closingStopFill("AAPL", campaignID, campaignN, day(57))
	second := stop
	second.FillID = "sim-fill-0003"
	second.FilledAt = day(58)

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		fill(stop).
		fill(second).
		wantRunError("AAPL", "sim-fill-0003", "no open campaign")
}

// TestStopFillForAnInstrumentWithNoHistoryIsRejected is the same rule for an
// instrument the reducer has never evaluated at all.
func TestStopFillForAnInstrumentWithNoHistoryIsRejected(t *testing.T) {
	t.Parallel()

	stray := event.FillPayload{
		InstrumentID: "TSLA",
		Kind:         event.FillKindStop,
		CampaignID:   "campaign:TSLA:2026-02-27T00:00:00.000000000Z",
		FillID:       "sim-fill-9999",
		Direction:    event.DirectionLong,
		Quantity:     133,
		Price:        100,
		FilledAt:     day(57),
	}

	newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		fill(stray).
		wantRunError("TSLA")
}

// TestStopFillNamingADifferentCampaignIsRejected: an instrument already
// holding a Campaign, but a stop fill names some other campaign id.
func TestStopFillNamingADifferentCampaignIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignN := breakoutFixtureN(t, cfg)
	stray := closingStopFill("AAPL", "campaign:AAPL:1999-01-01T00:00:00.000000000Z", campaignN, day(57))

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		fill(stray).
		wantRunError("AAPL", "1999-01-01", "the open campaign is")
}

// TestStopFillWithWrongQuantityIsRejected covers the ticket's "stop fill
// with wrong quantity errors" case: a partial stop fill is out of scope and
// must fail the run rather than silently closing part of the position.
func TestStopFillWithWrongQuantityIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	partial := closingStopFill("AAPL", campaignID, campaignN, day(57))
	partial.Quantity = 100

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		fill(partial).
		wantRunError("AAPL", "100", "133", "partial stop fill is rejected")
}

// TestStopFillWithMismatchedDirectionIsRejected: the closing fill must be in
// the campaign's own direction (FillPayload.Direction records the position's
// direction, not the order's buy/sell side — see FillPayload's doc comment).
func TestStopFillWithMismatchedDirectionIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	mismatched := closingStopFill("AAPL", campaignID, campaignN, day(57))
	mismatched.Direction = "short"

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		fill(mismatched).
		wantRunError("direction")
}

// TestStopFillAtWrongSchemaIsRejected applies ADR 0015's rule to a stop fill,
// mirroring TestReducerRejectsFillWithWrongSchemaVersion.
func TestStopFillAtWrongSchemaIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	stop := closingStopFill("AAPL", campaignID, campaignN, day(57))

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		fillAtSchema(stop, 1).
		wantRunError("schema version", "1", "2")
}

// TestStopFillNamingAProposalIsRejected covers the ticket's "a stop fill
// referencing a proposal ID errors" case. This is caught at the payload
// level (FillPayload.Validate), before the reducer's own campaign logic ever
// runs, which is exactly what makes it safe: a producer that got the
// discriminator wrong cannot reach the campaign-closing logic at all.
func TestStopFillNamingAProposalIsRejected(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	confused := closingStopFill("AAPL", campaignID, campaignN, day(57))
	confused.ProposalID = testDecisionID("proposal", "AAPL", day(56))

	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		fill(confused).
		wantRunError("invalid fill payload", "proposal id")
}

// TestASecondInstrumentIsUnaffectedByAnothersStopExit: Campaign state,
// including its closure, is per instrument.
func TestASecondInstrumentIsUnaffectedByAnothersStopExit(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	stop := closingStopFill("AAPL", campaignID, campaignN, day(57))

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bars(breakoutBars("MSFT")).
		fill(stop).
		mustRun()

	if got := countFor(t, emitted, event.CampaignExitedEventType, "AAPL"); got != 1 {
		t.Errorf("AAPL has %d Campaign-exited event(s), want 1", got)
	}
	// MSFT is still mid-Campaign (its own opening fill was never sent): its
	// proposal is simply outstanding, untouched by AAPL's stop.
	if got := countFor(t, emitted, event.CampaignOpenedEventType, "MSFT"); got != 0 {
		t.Errorf("MSFT has %d Campaign(s), want 0: AAPL's stop must not affect it", got)
	}
	if got := countFor(t, emitted, event.CampaignExitedEventType, "MSFT"); got != 0 {
		t.Errorf("MSFT has %d Campaign-exited event(s), want 0", got)
	}
	if got := countFor(t, emitted, event.TradeProposalEventType, "MSFT"); got != 1 {
		t.Errorf("MSFT emitted %d proposal(s), want 1", got)
	}
}

// TestABarsLowThroughTheStopWithNoStopFillLeavesTheCampaignOpen is the
// ticket's required negative test: the reducer never decides a fill for
// itself from bar data (ADR 0005 leaves that to #18's fill simulator). A
// bar whose low trades straight through the Campaign's Protective Stop
// level, with NO stop fill event ever arriving, must leave the Campaign
// open — proving the "evaluate" half of applyCompletedBar never once reads
// campaignState.protectiveStop against a bar's price.
func TestABarsLowThroughTheStopWithNoStopFillLeavesTheCampaignOpen(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignN := breakoutFixtureN(t, cfg)
	stopLevel := campaignFillPrice - cfg.StopMultiple*campaignN

	// A bar whose entire range sits far below the Protective Stop level —
	// exactly the shape a fill simulator would read as "the stop was hit" —
	// with no corresponding stop fill in the stream at all.
	throughTheStop := completedBar("AAPL", day(57), stopLevel-1, stopLevel-50, stopLevel-25)

	emitted := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(throughTheStop).
		mustRun()

	if got := len(envelopesOfType(emitted, event.CampaignExitedEventType)); got != 0 {
		t.Fatalf("got %d Campaign-exited event(s), want 0: no stop fill arrived, so nothing may close the campaign", got)
	}
	// The bar itself emits nothing at all: an instrument in a Campaign is
	// not a Setup (CONTEXT.md), and #12 does not decide fills from bar data.
	if last := emitted[len(emitted)-1]; last.Type != event.ProtectiveStopSetEventType {
		t.Errorf("last emission is %q, want the Protective-Stop-set from the opening fill: the bar through the stop must add nothing", last.Type)
	}
}

// TestReplayingTheEntryThenStopFixtureTwiceYieldsByteIdenticalEmissions
// extends TestReplayingTheCampaignFixtureTwiceYieldsByteIdenticalEmissions
// through a full Campaign life — entry, Protective-Stop-set, and a stop
// exit — which covers the deterministic Campaign-exited id and every
// derived figure on it (RealisedResult, RealisedResultInN) alongside the
// Campaign-opened and Protective-Stop-set fields #11 already covered.
func TestReplayingTheEntryThenStopFixtureTwiceYieldsByteIdenticalEmissions(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	stop := closingStopFill("AAPL", campaignID, campaignN, day(57))

	build := func() []event.Envelope {
		return newStream(t, cfg).
			bars(breakoutBars("AAPL")).
			fill(openingFill("AAPL")).
			fill(stop).
			bar(nextBreakoutBar("AAPL")).
			mustRun()
	}

	first, second := build(), build()
	if len(first) != len(second) {
		t.Fatalf("emission counts differ: %d and %d", len(first), len(second))
	}
	if got := len(envelopesOfType(first, event.CampaignExitedEventType)); got != 1 {
		t.Fatalf("got %d Campaign-exited event(s) in the first run, want 1: the fixture must actually exercise a stop exit", got)
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
