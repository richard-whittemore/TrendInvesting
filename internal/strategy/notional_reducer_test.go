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
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// This file is #16's event seam: account.snapshot events in,
// strategy.drawdown-step.applied events out, and #10's sizing reading the
// stepped Notional Account. Kept separate from reducer_test.go so the two
// tickets working internal/strategy in parallel (#11 and this one) do not
// collide on the same test file.

func accountSnapshotPayload(asOf time.Time, equity float64) event.AccountSnapshotPayload {
	return event.AccountSnapshotPayload{
		AsOf:     asOf,
		Equity:   equity,
		Currency: "USD",
	}
}

func accountSnapshotEnvelope(t *testing.T, sequence uint64, payload event.AccountSnapshotPayload, recordedAt time.Time) event.Envelope {
	t.Helper()
	encoded := mustMarshal(t, payload)
	return event.Envelope{
		ID:                fmt.Sprintf("snapshot-%d", sequence),
		Type:              event.AccountSnapshotEventType,
		SchemaVersion:     event.AccountSnapshotSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         payload.AsOf,
		RecordedAt:        recordedAt,
		Sequence:          sequence,
		Source:            "fixture",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(encoded),
		Payload:           encoded,
	}
}

func decodeDrawdownStepApplied(t *testing.T, envelope event.Envelope) event.DrawdownStepAppliedPayload {
	t.Helper()
	if envelope.Type != event.DrawdownStepAppliedEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.DrawdownStepAppliedEventType)
	}
	var payload event.DrawdownStepAppliedPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload
}

// runReducerWithAccountSnapshotsThenHighs replays a configuration event,
// then one account.snapshot event per entry in snapshots, then one completed
// bar per entry in highs (see runReducerOverHighs in reducer_test.go, which
// this mirrors for the bar-only case), and returns every envelope the engine
// emitted.
func runReducerWithAccountSnapshotsThenHighs(t *testing.T, instrumentID string, snapshots []event.AccountSnapshotPayload, highs []float64, cfg event.ConfigurationPayload) []event.Envelope {
	t.Helper()
	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	envelopes := []event.Envelope{configEnvelopeWithConfig(t, 1, day(0), cfg)}
	seq := uint64(2)
	for _, snap := range snapshots {
		envelopes = append(envelopes, accountSnapshotEnvelope(t, seq, snap, snap.AsOf))
		seq++
	}
	for i, high := range highs {
		periodEnd := day(i + 1)
		bar := syntheticBar(instrumentID, periodEnd, high-100)
		envelopes = append(envelopes, barEnvelope(t, seq, bar, periodEnd))
		seq++
	}

	emitted, err := engine.Run(context.Background(), envelopes)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	return emitted
}

// snapshotBefore is the AsOf every account.snapshot fixture in this file
// uses: strictly before day(1), the first bar's period end, and strictly
// after day(0), the configuration's event time.
func snapshotBefore(bars int) time.Time {
	return day(0).Add(time.Duration(bars) * time.Hour)
}

// TestReducerEmitsDrawdownStepEventsForAccountSnapshots is the primary
// event seam test: two account.snapshot events, each crossing exactly one
// threshold, in -> two strategy.drawdown-step.applied events out, with
// correct before/after figures and 1-based step numbers, in order. A third
// snapshot recovers most of the way back up and must apply no further step
// (ADR 0007: not a high-water mark; recovery is #17).
func TestReducerEmitsDrawdownStepEventsForAccountSnapshots(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	cfg := validConfigurationPayload() // starting equity is 1,000,000
	snap1 := accountSnapshotPayload(snapshotBefore(1), 900_000)
	snap2 := accountSnapshotPayload(snapshotBefore(2), 820_000)
	snap3 := accountSnapshotPayload(snapshotBefore(3), 950_000) // partial recovery: no step

	envelopes := []event.Envelope{
		configEnvelopeWithConfig(t, 1, day(0), cfg),
		accountSnapshotEnvelope(t, 2, snap1, snap1.AsOf),
		accountSnapshotEnvelope(t, 3, snap2, snap2.AsOf),
		accountSnapshotEnvelope(t, 4, snap3, snap3.AsOf),
	}

	emitted, err := engine.Run(context.Background(), envelopes)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(emitted) != 2 {
		t.Fatalf("len(emitted) = %d, want 2 (one Drawdown Step per triggering snapshot, none for the recovery)", len(emitted))
	}

	first := decodeDrawdownStepApplied(t, emitted[0])
	if first.NotionalBefore != 1_000_000 || first.NotionalAfter != 800_000 || first.Threshold != 900_000 || first.StepNumber != 1 {
		t.Errorf("first step = %+v, want before 1,000,000 after 800,000 threshold 900,000 step 1", first)
	}
	if !first.AsOf.Equal(snap1.AsOf) || first.Equity != 900_000 {
		t.Errorf("first step AsOf/Equity = %v/%v, want %v/900000", first.AsOf, first.Equity, snap1.AsOf)
	}
	if first.Rule != event.RuleNotionalAccountDrawdownStep || first.ADR != event.ADRNotionalAccountDrawdownStep {
		t.Errorf("first step Rule/ADR = %q/%q, want %q/%q", first.Rule, first.ADR, event.RuleNotionalAccountDrawdownStep, event.ADRNotionalAccountDrawdownStep)
	}
	if err := first.Validate(); err != nil {
		t.Errorf("first step fails its own Validate(): %v", err)
	}

	second := decodeDrawdownStepApplied(t, emitted[1])
	if second.NotionalBefore != 800_000 || second.NotionalAfter != 640_000 || second.Threshold != 820_000 || second.StepNumber != 2 {
		t.Errorf("second step = %+v, want before 800,000 after 640,000 threshold 820,000 step 2", second)
	}
	if err := second.Validate(); err != nil {
		t.Errorf("second step fails its own Validate(): %v", err)
	}

	// Envelope-level stamping, matching every other emission from this
	// reducer (docs/architecture.md).
	if !emitted[0].EventTime.Equal(snap1.AsOf) {
		t.Errorf("EventTime = %v, want the snapshot's AsOf %v", emitted[0].EventTime, snap1.AsOf)
	}
	if !emitted[0].RecordedAt.Equal(snap1.AsOf) {
		t.Errorf("RecordedAt = %v, want the input envelope's RecordedAt %v", emitted[0].RecordedAt, snap1.AsOf)
	}
	if emitted[0].Source != "reducer" {
		t.Errorf("Source = %q, want %q", emitted[0].Source, "reducer")
	}
	wantID := fmt.Sprintf("drawdown-step:%s:1", snap1.AsOf.UTC().Format("2006-01-02T15:04:05.000000000Z"))
	if emitted[0].ID != wantID {
		t.Errorf("ID = %q, want %q (deterministic and reproducible on replay)", emitted[0].ID, wantID)
	}
}

// TestReducerAppliesSeveralDrawdownStepsFromOneSnapshot: a single large drop
// in one account.snapshot must apply several Drawdown Steps, journalled
// separately, in order (NotionalAccount.Observe's own contract; see
// notional_test.go's TestNotionalAccountSingleObservationAppliesSeveralStepsInOrder
// for why 750,000, not 700,000, is the correct three-step fixture).
func TestReducerAppliesSeveralDrawdownStepsFromOneSnapshot(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	cfg := validConfigurationPayload()
	snap := accountSnapshotPayload(snapshotBefore(1), 750_000)
	envelopes := []event.Envelope{
		configEnvelopeWithConfig(t, 1, day(0), cfg),
		accountSnapshotEnvelope(t, 2, snap, snap.AsOf),
	}

	emitted, err := engine.Run(context.Background(), envelopes)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(emitted) != 3 {
		t.Fatalf("len(emitted) = %d, want 3 (900,000 / 820,000 / 756,000 crossed in one snapshot)", len(emitted))
	}

	wantThresholds := []float64{900_000, 820_000, 756_000}
	wantBefore := []float64{1_000_000, 800_000, 640_000}
	wantAfter := []float64{800_000, 640_000, 512_000}
	for i, e := range emitted {
		step := decodeDrawdownStepApplied(t, e)
		if step.Threshold != wantThresholds[i] {
			t.Errorf("step %d Threshold = %v, want %v", i+1, step.Threshold, wantThresholds[i])
		}
		if step.NotionalBefore != wantBefore[i] {
			t.Errorf("step %d NotionalBefore = %v, want %v", i+1, step.NotionalBefore, wantBefore[i])
		}
		if step.NotionalAfter != wantAfter[i] {
			t.Errorf("step %d NotionalAfter = %v, want %v", i+1, step.NotionalAfter, wantAfter[i])
		}
		if step.StepNumber != i+1 {
			t.Errorf("step %d StepNumber = %d, want %d", i+1, step.StepNumber, i+1)
		}
		if !step.AsOf.Equal(snap.AsOf) || step.Equity != 750_000 {
			t.Errorf("step %d AsOf/Equity = %v/%v, want %v/750000", i+1, step.AsOf, step.Equity, snap.AsOf)
		}
	}
}

// TestReducerSizesFromTheSteppedNotionalAccount is the seam that closes the
// loop to #10's sizing: one account.snapshot drops the Notional Account by
// one step (to 800,000), and the breakout fixture's proposal is then sized
// from that stepped figure, not the configured starting equity.
func TestReducerSizesFromTheSteppedNotionalAccount(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	highs := breakoutFixtureHighs()
	snap := accountSnapshotPayload(snapshotBefore(1), 900_000) // one step: 1,000,000 -> 800,000

	emitted := runReducerWithAccountSnapshotsThenHighs(t, "AAPL", []event.AccountSnapshotPayload{snap}, highs, cfg)

	drawdownSteps := envelopesOfType(emitted, event.DrawdownStepAppliedEventType)
	if len(drawdownSteps) != 1 {
		t.Fatalf("len(drawdownSteps) = %d, want 1", len(drawdownSteps))
	}
	step := decodeDrawdownStepApplied(t, drawdownSteps[0])
	if step.NotionalAfter != 800_000 {
		t.Fatalf("step.NotionalAfter = %v, want 800,000", step.NotionalAfter)
	}

	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("len(proposals) = %d, want 1", len(proposals))
	}
	proposal := decodeTradeProposal(t, proposals[0])

	if proposal.NotionalAccount != 800_000 {
		t.Fatalf("proposal.NotionalAccount = %v, want the stepped 800,000, not the configured starting equity %v", proposal.NotionalAccount, cfg.NotionalAccount.StartingEquity)
	}

	wantN := breakoutFixtureN(t, cfg)
	// sizing.UnitQuantity is the oracle here, called directly with the
	// stepped 800,000 rather than the configured 1,000,000: this test's
	// question is "did the stepped figure reach sizing", not "is the sizing
	// formula correct" (already covered exhaustively in internal/sizing's
	// own tests and in reducer_test.go's breakout fixture).
	wantQuantity, err := sizing.UnitQuantity(800_000, cfg.UnitVolatilityFraction, wantN, cfg.DollarsPerPoint)
	if err != nil {
		t.Fatalf("sizing.UnitQuantity() error = %v", err)
	}
	if proposal.Quantity != wantQuantity {
		t.Fatalf("proposal.Quantity = %d, want %d (floor(800,000 x 0.005 / N))", proposal.Quantity, wantQuantity)
	}
	if err := proposal.Validate(); err != nil {
		t.Fatalf("proposal fails its own Validate(): %v", err)
	}
}

// TestReducerWithoutASnapshotSizesFromTheConfiguredStartingEquity documents
// #16's stated invariant directly: before any account.snapshot arrives, the
// Notional Account equals the configured StartingEquity, so #10's existing
// fixtures produce the same quantities unchanged. (reducer_test.go's
// TestReducerEmitsTradeProposalOnSignal already exercises this implicitly,
// since it sends no account.snapshot event at all and compares against
// cfg.NotionalAccount.StartingEquity; this test names the invariant
// explicitly as its own case.)
func TestReducerWithoutASnapshotSizesFromTheConfiguredStartingEquity(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	emitted := runReducerOverHighs(t, "AAPL", breakoutFixtureHighs(), cfg)

	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("len(proposals) = %d, want 1", len(proposals))
	}
	proposal := decodeTradeProposal(t, proposals[0])
	if proposal.NotionalAccount != cfg.NotionalAccount.StartingEquity {
		t.Fatalf("proposal.NotionalAccount = %v, want the configured starting equity %v unchanged (no snapshot arrived)", proposal.NotionalAccount, cfg.NotionalAccount.StartingEquity)
	}
}

func TestReducerRejectsAccountSnapshotBeforeConfiguration(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	snap := accountSnapshotPayload(day(0), 900_000)
	_, err = engine.Run(context.Background(), []event.Envelope{accountSnapshotEnvelope(t, 1, snap, day(0))})
	if err == nil {
		t.Fatal("Run() error = nil, want an error for a snapshot before any configuration")
	}
	if !strings.Contains(err.Error(), "configuration") {
		t.Fatalf("Run() error = %v, want it to name the missing configuration", err)
	}
}

func TestReducerRejectsAccountSnapshotWithWrongSchemaVersion(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	snap := accountSnapshotPayload(snapshotBefore(1), 900_000)
	wrongVersion := accountSnapshotEnvelope(t, 2, snap, snap.AsOf)
	wrongVersion.SchemaVersion = 2

	envelopes := []event.Envelope{configEnvelope(t, 1, day(0)), wrongVersion}
	_, err = engine.Run(context.Background(), envelopes)
	if err == nil {
		t.Fatal("Run() error = nil, want error for an account snapshot payload at the wrong schema version")
	}
	for _, want := range []string{"schema version", "2", "1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Run() error = %v, want substring %q", err, want)
		}
	}
}

func TestReducerRejectsInvalidAccountSnapshotPayload(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	snap := accountSnapshotPayload(snapshotBefore(1), -1) // invalid: negative equity
	invalid := accountSnapshotEnvelope(t, 2, snap, snap.AsOf)

	envelopes := []event.Envelope{configEnvelope(t, 1, day(0)), invalid}
	_, err = engine.Run(context.Background(), envelopes)
	if err == nil || !strings.Contains(err.Error(), "invalid account snapshot payload") {
		t.Fatalf("Run() error = %v, want it to name an invalid account snapshot payload", err)
	}
}

func TestReducerRejectsUndecodableAccountSnapshotPayload(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	payload := json.RawMessage(`"not an object"`)
	undecodable := event.Envelope{
		ID:                "snapshot-1",
		Type:              event.AccountSnapshotEventType,
		SchemaVersion:     event.AccountSnapshotSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         snapshotBefore(1),
		RecordedAt:        snapshotBefore(1),
		Sequence:          2,
		Source:            "fixture",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}

	envelopes := []event.Envelope{configEnvelope(t, 1, day(0)), undecodable}
	_, err = engine.Run(context.Background(), envelopes)
	if err == nil || !strings.Contains(err.Error(), "decode account snapshot payload") {
		t.Fatalf("Run() error = %v, want it to name a decode failure", err)
	}
}

// TestReducerRejectsDuplicateAccountSnapshotAsOf mirrors
// TestReducerRejectsDuplicateBarPeriodEnd: an exact duplicate AsOf must be
// rejected, not silently re-applied.
func TestReducerRejectsDuplicateAccountSnapshotAsOf(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	asOf := snapshotBefore(1)
	first := accountSnapshotPayload(asOf, 900_000)
	duplicate := accountSnapshotPayload(asOf, 800_000) // same AsOf as first

	envelopes := []event.Envelope{
		configEnvelope(t, 1, day(0)),
		accountSnapshotEnvelope(t, 2, first, first.AsOf),
		accountSnapshotEnvelope(t, 3, duplicate, duplicate.AsOf),
	}

	_, err = engine.Run(context.Background(), envelopes)
	if err == nil {
		t.Fatal("Run() error = nil, want error for a duplicate account snapshot as-of")
	}
	if !strings.Contains(err.Error(), "duplicate or out-of-order snapshot") {
		t.Fatalf("Run() error = %v, want it to name a duplicate or out-of-order snapshot", err)
	}
}

// TestReducerRejectsOutOfOrderAccountSnapshot mirrors
// TestReducerRejectsOutOfOrderBarPeriodEnd: a snapshot whose AsOf is earlier
// than the last one recorded must be rejected the same way a duplicate is.
func TestReducerRejectsOutOfOrderAccountSnapshot(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	later := accountSnapshotPayload(snapshotBefore(5), 900_000)
	earlier := accountSnapshotPayload(snapshotBefore(2), 800_000)

	envelopes := []event.Envelope{
		configEnvelope(t, 1, day(0)),
		accountSnapshotEnvelope(t, 2, later, later.AsOf),
		accountSnapshotEnvelope(t, 3, earlier, earlier.AsOf),
	}

	_, err = engine.Run(context.Background(), envelopes)
	if err == nil {
		t.Fatal("Run() error = nil, want error for an out-of-order account snapshot")
	}
	if !strings.Contains(err.Error(), "duplicate or out-of-order snapshot") {
		t.Fatalf("Run() error = %v, want it to name a duplicate or out-of-order snapshot", err)
	}
}

// TestReplayingAccountSnapshotFixtureTwiceYieldsByteIdenticalEmissions is the
// determinism property test for this seam, matching
// TestReplayingSameFixtureTwiceYieldsByteIdenticalEmissions in
// reducer_test.go.
func TestReplayingAccountSnapshotFixtureTwiceYieldsByteIdenticalEmissions(t *testing.T) {
	t.Parallel()

	buildFixture := func(t *testing.T) []event.Envelope {
		t.Helper()
		snap := accountSnapshotPayload(snapshotBefore(1), 750_000) // three steps in one snapshot
		return []event.Envelope{
			configEnvelope(t, 1, day(0)),
			accountSnapshotEnvelope(t, 2, snap, snap.AsOf),
		}
	}

	runOnce := func(t *testing.T) []event.Envelope {
		t.Helper()
		reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
		if err != nil {
			t.Fatalf("NewReducer() error = %v", err)
		}
		engine, err := replay.New(reducer)
		if err != nil {
			t.Fatalf("replay.New() error = %v", err)
		}
		emitted, err := engine.Run(context.Background(), buildFixture(t))
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		return emitted
	}

	first := runOnce(t)
	second := runOnce(t)

	firstBytes := mustMarshal(t, first)
	secondBytes := mustMarshal(t, second)
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatalf("replay is not deterministic:\n  first:  %s\n  second: %s", firstBytes, secondBytes)
	}
}

// TestReducerSurfacesTheNotionalAccountAsymptoteError is the event-seam
// counterpart of notional_test.go's
// TestNotionalAccountEquityAtTheAsymptoteErrors (Greptile PR #66 finding): an
// account.snapshot at or below the Drawdown Step ladder's 50%-drawdown
// asymptote makes NotionalAccount.Observe fail closed, and the reducer must
// not swallow that — replay.Engine.Run surfaces it as a run error, and since
// Run returns nil on any error, nothing is emitted for the whole run.
func TestReducerSurfacesTheNotionalAccountAsymptoteError(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	// 500,000 is exactly the asymptote for validConfigurationPayload's
	// 1,000,000 starting equity (base - 50%*current, both still 1,000,000
	// since no step has been applied yet).
	snap := accountSnapshotPayload(snapshotBefore(1), 500_000)
	envelopes := []event.Envelope{
		configEnvelope(t, 1, day(0)),
		accountSnapshotEnvelope(t, 2, snap, snap.AsOf),
	}

	emitted, err := engine.Run(context.Background(), envelopes)
	if err == nil {
		t.Fatal("Run() error = nil, want an error: equity at the Notional Account's 50% drawdown asymptote is undefined (ADR 0007)")
	}
	if !strings.Contains(err.Error(), "undefined") {
		t.Fatalf("Run() error = %v, want it to say the rule is undefined this deep", err)
	}
	if emitted != nil {
		t.Fatalf("emitted = %v, want nil: the engine returns nothing for a run that failed closed", emitted)
	}
}
