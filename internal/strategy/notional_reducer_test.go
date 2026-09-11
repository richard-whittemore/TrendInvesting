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

// --- #17: yearly re-basing, deposits, and withdrawals — event seam ---
//
// jan(day, year) is defined in notional_test.go (same package): a UTC
// midnight time.Time. validConfigurationPayload's NotionalAccount re-bases
// on 1 January (RebasingMonth/RebasingDay 1/1).

func cashMovementPayload(asOf time.Time, amount, equityBefore float64) event.CashMovementPayload {
	return event.CashMovementPayload{
		AsOf:         asOf,
		Amount:       amount,
		EquityBefore: equityBefore,
		Currency:     "USD",
	}
}

func cashMovementEnvelope(t *testing.T, sequence uint64, payload event.CashMovementPayload, recordedAt time.Time) event.Envelope {
	t.Helper()
	encoded := mustMarshal(t, payload)
	return event.Envelope{
		ID:                fmt.Sprintf("cash-movement-%d", sequence),
		Type:              event.CashMovementEventType,
		SchemaVersion:     event.CashMovementSchemaVersion,
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

func decodeNotionalAccountRebased(t *testing.T, envelope event.Envelope) event.NotionalAccountRebasedPayload {
	t.Helper()
	if envelope.Type != event.NotionalAccountRebasedEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.NotionalAccountRebasedEventType)
	}
	var payload event.NotionalAccountRebasedPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload
}

func decodeNotionalAccountRecovered(t *testing.T, envelope event.Envelope) event.NotionalAccountRecoveredPayload {
	t.Helper()
	if envelope.Type != event.NotionalAccountRecoveredEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.NotionalAccountRecoveredEventType)
	}
	var payload event.NotionalAccountRecoveredPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload
}

func decodeNotionalAccountCashAdjusted(t *testing.T, envelope event.Envelope) event.NotionalAccountCashAdjustedPayload {
	t.Helper()
	if envelope.Type != event.NotionalAccountCashAdjustedEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.NotionalAccountCashAdjustedEventType)
	}
	var payload event.NotionalAccountCashAdjustedPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload
}

// TestReducerRebasesAcrossAYearBoundaryThenStepsAgainstTheNewFigure is the
// ticket's headline event-seam case: a snapshot before the account's first
// re-basing date establishes its period without re-basing, the first
// snapshot on or after 1 January re-bases it to actual equity (emitting
// strategy.notional-account.rebased), and a further snapshot's Drawdown
// Step is measured against the NEW figure, not the one re-basing replaced.
func TestReducerRebasesAcrossAYearBoundaryThenStepsAgainstTheNewFigure(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	cfg := validConfigurationPayload() // starting equity 1,000,000, rebasing 1 January
	snap1 := accountSnapshotPayload(jan(2, 2026).Add(time.Hour), 970_000)
	snap2 := accountSnapshotPayload(jan(1, 2027), 900_000) // rebases to 900,000
	snap3 := accountSnapshotPayload(jan(2, 2027), 810_000) // steps against the NEW 900,000 figure

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
		t.Fatalf("len(emitted) = %d, want 2 (a rebased event for snap2, a drawdown step for snap3; snap1 emits nothing)", len(emitted))
	}

	rebased := decodeNotionalAccountRebased(t, emitted[0])
	if rebased.PreviousStartingFigure != 1_000_000 || rebased.NewStartingFigure != 900_000 || rebased.Equity != 900_000 {
		t.Errorf("rebased = %+v, want previous 1,000,000 new 900,000 equity 900,000", rebased)
	}
	if !rebased.AsOf.Equal(snap2.AsOf) {
		t.Errorf("rebased.AsOf = %v, want %v", rebased.AsOf, snap2.AsOf)
	}
	if rebased.Rule != event.RuleNotionalAccountRebase || rebased.ADR != event.ADRNotionalAccountRebase {
		t.Errorf("rebased Rule/ADR = %q/%q, want %q/%q", rebased.Rule, rebased.ADR, event.RuleNotionalAccountRebase, event.ADRNotionalAccountRebase)
	}
	if err := rebased.Validate(); err != nil {
		t.Errorf("rebased fails its own Validate(): %v", err)
	}
	wantRebasedID := fmt.Sprintf("notional-account-rebased:%s", snap2.AsOf.UTC().Format("2006-01-02T15:04:05.000000000Z"))
	if emitted[0].ID != wantRebasedID {
		t.Errorf("ID = %q, want %q", emitted[0].ID, wantRebasedID)
	}

	step := decodeDrawdownStepApplied(t, emitted[1])
	if step.NotionalBefore != 900_000 || step.NotionalAfter != 720_000 || step.Threshold != 810_000 {
		t.Errorf("step = %+v, want before 900,000 after 720,000 threshold 810,000 (measured against the re-based figure)", step)
	}
	if step.StepNumber != 1 {
		t.Errorf("step.StepNumber = %d, want 1: re-basing resets the step count", step.StepNumber)
	}
	if err := step.Validate(); err != nil {
		t.Errorf("step fails its own Validate(): %v", err)
	}
}

// TestReducerFirstSnapshotAfterRebasingDateSizesFromTheConfiguredFigure
// confirms #17's documented decision: the very first snapshot of a run,
// even one well after the year's re-basing date, does not re-base, and
// sizing continues to use the configured StartingEquity.
func TestReducerFirstSnapshotAfterRebasingDateSizesFromTheConfiguredFigure(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	highs := breakoutFixtureHighs()
	// June 2026 is well after the 1 January re-basing date, and this is the
	// account's very first snapshot.
	snap := accountSnapshotPayload(jan(2, 2026).AddDate(0, 5, 0), 950_000)

	emitted := runReducerWithAccountSnapshotsThenHighs(t, "AAPL", []event.AccountSnapshotPayload{snap}, highs, cfg)

	rebasedEvents := envelopesOfType(emitted, event.NotionalAccountRebasedEventType)
	if len(rebasedEvents) != 0 {
		t.Fatalf("len(rebasedEvents) = %d, want 0: the first snapshot of a run never re-bases", len(rebasedEvents))
	}
	drawdownSteps := envelopesOfType(emitted, event.DrawdownStepAppliedEventType)
	if len(drawdownSteps) != 0 {
		t.Fatalf("len(drawdownSteps) = %d, want 0: 950,000 is above the configured account's first threshold (900,000)", len(drawdownSteps))
	}

	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("len(proposals) = %d, want 1", len(proposals))
	}
	proposal := decodeTradeProposal(t, proposals[0])
	if proposal.NotionalAccount != cfg.NotionalAccount.StartingEquity {
		t.Fatalf("proposal.NotionalAccount = %v, want the configured starting equity %v unchanged (no re-basing on the first snapshot)", proposal.NotionalAccount, cfg.NotionalAccount.StartingEquity)
	}
}

// TestReducerBreakoutAfterRebasingSizesFromTheRebasedFigure closes the loop
// to #10's sizing after a re-basing: the account's first snapshot
// establishes its period (no re-basing), a later snapshot on the re-basing
// date re-bases it to 950,000, and the subsequent breakout is sized from
// that re-based figure, not the configured 1,000,000.
func TestReducerBreakoutAfterRebasingSizesFromTheRebasedFigure(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	highs := breakoutFixtureHighs()
	snap1 := accountSnapshotPayload(jan(2, 2026).Add(time.Hour), 970_000) // establishes the period, no rebase
	snap2 := accountSnapshotPayload(jan(1, 2027), 950_000)                // rebases to 950,000

	emitted := runReducerWithAccountSnapshotsThenHighs(t, "AAPL", []event.AccountSnapshotPayload{snap1, snap2}, highs, cfg)

	rebasedEvents := envelopesOfType(emitted, event.NotionalAccountRebasedEventType)
	if len(rebasedEvents) != 1 {
		t.Fatalf("len(rebasedEvents) = %d, want 1", len(rebasedEvents))
	}
	rebased := decodeNotionalAccountRebased(t, rebasedEvents[0])
	if rebased.NewStartingFigure != 950_000 {
		t.Fatalf("rebased.NewStartingFigure = %v, want 950,000", rebased.NewStartingFigure)
	}

	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("len(proposals) = %d, want 1", len(proposals))
	}
	proposal := decodeTradeProposal(t, proposals[0])
	if proposal.NotionalAccount != 950_000 {
		t.Fatalf("proposal.NotionalAccount = %v, want the re-based 950,000, not the configured starting equity %v", proposal.NotionalAccount, cfg.NotionalAccount.StartingEquity)
	}

	wantN := breakoutFixtureN(t, cfg)
	wantQuantity, err := sizing.UnitQuantity(950_000, cfg.UnitVolatilityFraction, wantN, cfg.DollarsPerPoint)
	if err != nil {
		t.Fatalf("sizing.UnitQuantity() error = %v", err)
	}
	if proposal.Quantity != wantQuantity {
		t.Fatalf("proposal.Quantity = %d, want %d (floor(950,000 x 0.005 / N))", proposal.Quantity, wantQuantity)
	}
}

// TestReducerFullRecoveryClearsBothStepsAndEmitsRecoveredEvent: two Drawdown
// Steps in, then a snapshot regaining the yearly starting figure emits the
// step events (in order) followed by exactly one
// strategy.notional-account.recovered event reporting both steps cleared.
func TestReducerFullRecoveryClearsBothStepsAndEmitsRecoveredEvent(t *testing.T) {
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
	snap1 := accountSnapshotPayload(snapshotBefore(1), 900_000)   // step 1: -> 800,000
	snap2 := accountSnapshotPayload(snapshotBefore(2), 820_000)   // step 2: -> 640,000
	snap3 := accountSnapshotPayload(snapshotBefore(3), 1_050_000) // full recovery

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
	if len(emitted) != 3 {
		t.Fatalf("len(emitted) = %d, want 3 (two steps, one recovery)", len(emitted))
	}
	if emitted[0].Type != event.DrawdownStepAppliedEventType || emitted[1].Type != event.DrawdownStepAppliedEventType {
		t.Fatalf("emitted[0].Type/emitted[1].Type = %q/%q, want two drawdown steps first", emitted[0].Type, emitted[1].Type)
	}

	recovered := decodeNotionalAccountRecovered(t, emitted[2])
	want := event.NotionalAccountRecoveredPayload{
		AsOf:           snap3.AsOf,
		Equity:         1_050_000,
		StartingFigure: 1_000_000,
		NotionalBefore: 640_000,
		StepsCleared:   2,
		Rule:           event.RuleNotionalAccountRecovery,
		ADR:            event.ADRNotionalAccountRecovery,
	}
	if recovered != want {
		t.Errorf("recovered = %+v, want %+v", recovered, want)
	}
	if err := recovered.Validate(); err != nil {
		t.Errorf("recovered fails its own Validate(): %v", err)
	}

	// A Drawdown Step after this recovery starts a fresh episode: StepNumber
	// resets to 1, exactly as it does after a re-basing.
	snap4 := accountSnapshotPayload(snapshotBefore(4), 900_000)
	emitted, err = engine.Run(context.Background(), []event.Envelope{accountSnapshotEnvelope(t, 5, snap4, snap4.AsOf)})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(emitted) != 1 {
		t.Fatalf("len(emitted) = %d, want 1", len(emitted))
	}
	if step := decodeDrawdownStepApplied(t, emitted[0]); step.StepNumber != 1 {
		t.Fatalf("step.StepNumber = %d, want 1: a recovery resets the step count", step.StepNumber)
	}
}

// TestReducerCashMovementMidDrawdownEmitsCashAdjustedAndNoStep is the
// ticket's named deposit fixture through the full event seam: one Drawdown
// Step, then a deposit at the SAME equity the step ladder is standing at
// (890,000, between the first and second thresholds), then a snapshot at
// the post-deposit equity that must neither step nor recover.
func TestReducerCashMovementMidDrawdownEmitsCashAdjustedAndNoStep(t *testing.T) {
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
	snap1 := accountSnapshotPayload(snapshotBefore(1), 900_000) // step: -> 800,000, base 900,000
	movement := cashMovementPayload(snapshotBefore(2), 200_000, 890_000)
	snap2 := accountSnapshotPayload(snapshotBefore(3), 1_090_000) // post-deposit equity: no step, no recovery

	envelopes := []event.Envelope{
		configEnvelopeWithConfig(t, 1, day(0), cfg),
		accountSnapshotEnvelope(t, 2, snap1, snap1.AsOf),
		cashMovementEnvelope(t, 3, movement, movement.AsOf),
		accountSnapshotEnvelope(t, 4, snap2, snap2.AsOf),
	}

	emitted, err := engine.Run(context.Background(), envelopes)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(emitted) != 2 {
		t.Fatalf("len(emitted) = %d, want 2 (one drawdown step, one cash adjustment; the final snapshot emits nothing)", len(emitted))
	}
	if emitted[0].Type != event.DrawdownStepAppliedEventType {
		t.Fatalf("emitted[0].Type = %q, want %q", emitted[0].Type, event.DrawdownStepAppliedEventType)
	}

	adjusted := decodeNotionalAccountCashAdjusted(t, emitted[1])
	want := event.NotionalAccountCashAdjustedPayload{
		AsOf:                 movement.AsOf,
		Amount:               200_000,
		EquityBefore:         890_000,
		EquityAfter:          1_090_000,
		StartingFigureBefore: 1_000_000,
		StartingFigureAfter:  1_224_719.1011235956,
		NotionalBefore:       800_000,
		NotionalAfter:        979_775.2808988765,
		Rule:                 event.RuleNotionalAccountCashAdjustment,
		ADR:                  event.ADRNotionalAccountCashAdjustment,
	}
	if adjusted != want {
		t.Errorf("adjusted = %+v, want %+v", adjusted, want)
	}
	if err := adjusted.Validate(); err != nil {
		t.Errorf("adjusted fails its own Validate(): %v", err)
	}
	wantID := fmt.Sprintf("notional-account-cash-adjusted:%s", movement.AsOf.UTC().Format("2006-01-02T15:04:05.000000000Z"))
	if emitted[1].ID != wantID {
		t.Errorf("ID = %q, want %q", emitted[1].ID, wantID)
	}
}

func TestReducerRejectsCashMovementBeforeConfiguration(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	movement := cashMovementPayload(day(0), 200_000, 1_000_000)
	_, err = engine.Run(context.Background(), []event.Envelope{cashMovementEnvelope(t, 1, movement, day(0))})
	if err == nil {
		t.Fatal("Run() error = nil, want an error for a cash movement before any configuration")
	}
	if !strings.Contains(err.Error(), "configuration") {
		t.Fatalf("Run() error = %v, want it to name the missing configuration", err)
	}
}

func TestReducerRejectsCashMovementWithWrongSchemaVersion(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	movement := cashMovementPayload(snapshotBefore(1), 200_000, 1_000_000)
	wrongVersion := cashMovementEnvelope(t, 2, movement, movement.AsOf)
	wrongVersion.SchemaVersion = 2

	envelopes := []event.Envelope{configEnvelope(t, 1, day(0)), wrongVersion}
	_, err = engine.Run(context.Background(), envelopes)
	if err == nil {
		t.Fatal("Run() error = nil, want error for a cash movement payload at the wrong schema version")
	}
	for _, want := range []string{"schema version", "2", "1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Run() error = %v, want substring %q", err, want)
		}
	}
}

func TestReducerRejectsInvalidCashMovementPayload(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	movement := cashMovementPayload(snapshotBefore(1), 0, 1_000_000) // invalid: zero amount
	invalid := cashMovementEnvelope(t, 2, movement, movement.AsOf)

	envelopes := []event.Envelope{configEnvelope(t, 1, day(0)), invalid}
	_, err = engine.Run(context.Background(), envelopes)
	if err == nil || !strings.Contains(err.Error(), "invalid cash movement payload") {
		t.Fatalf("Run() error = %v, want it to name an invalid cash movement payload", err)
	}
}

func TestReducerRejectsUndecodableCashMovementPayload(t *testing.T) {
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
		ID:                "cash-movement-1",
		Type:              event.CashMovementEventType,
		SchemaVersion:     event.CashMovementSchemaVersion,
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
	if err == nil || !strings.Contains(err.Error(), "decode cash movement payload") {
		t.Fatalf("Run() error = %v, want it to name a decode failure", err)
	}
}

// TestReducerRejectsOutOfOrderAccountEventsAcrossTypes: #17's decision that
// snapshots and cash movements share ONE strictly-increasing account
// timeline (ADR 0007 rule 4). A cash movement may not share an AsOf with a
// snapshot, in either direction.
func TestReducerRejectsOutOfOrderAccountEventsAcrossTypes(t *testing.T) {
	t.Parallel()

	t.Run("cash movement at the same AsOf as a prior snapshot", func(t *testing.T) {
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
		movement := cashMovementPayload(snap.AsOf, 200_000, 900_000) // same AsOf as snap

		envelopes := []event.Envelope{
			configEnvelope(t, 1, day(0)),
			accountSnapshotEnvelope(t, 2, snap, snap.AsOf),
			cashMovementEnvelope(t, 3, movement, movement.AsOf),
		}
		_, err = engine.Run(context.Background(), envelopes)
		if err == nil || !strings.Contains(err.Error(), "duplicate or out-of-order cash movement") {
			t.Fatalf("Run() error = %v, want it to name a duplicate or out-of-order cash movement", err)
		}
	})

	t.Run("snapshot at the same AsOf as a prior cash movement", func(t *testing.T) {
		t.Parallel()
		reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
		if err != nil {
			t.Fatalf("NewReducer() error = %v", err)
		}
		engine, err := replay.New(reducer)
		if err != nil {
			t.Fatalf("replay.New() error = %v", err)
		}

		movement := cashMovementPayload(snapshotBefore(1), 200_000, 1_000_000)
		snap := accountSnapshotPayload(movement.AsOf, 900_000) // same AsOf as movement

		envelopes := []event.Envelope{
			configEnvelope(t, 1, day(0)),
			cashMovementEnvelope(t, 2, movement, movement.AsOf),
			accountSnapshotEnvelope(t, 3, snap, snap.AsOf),
		}
		_, err = engine.Run(context.Background(), envelopes)
		if err == nil || !strings.Contains(err.Error(), "duplicate or out-of-order snapshot") {
			t.Fatalf("Run() error = %v, want it to name a duplicate or out-of-order snapshot", err)
		}
	})

	t.Run("snapshot before an earlier cash movement", func(t *testing.T) {
		t.Parallel()
		reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
		if err != nil {
			t.Fatalf("NewReducer() error = %v", err)
		}
		engine, err := replay.New(reducer)
		if err != nil {
			t.Fatalf("replay.New() error = %v", err)
		}

		movement := cashMovementPayload(snapshotBefore(5), 200_000, 1_000_000)
		snap := accountSnapshotPayload(snapshotBefore(2), 900_000) // earlier than movement

		envelopes := []event.Envelope{
			configEnvelope(t, 1, day(0)),
			cashMovementEnvelope(t, 2, movement, movement.AsOf),
			accountSnapshotEnvelope(t, 3, snap, snap.AsOf),
		}
		_, err = engine.Run(context.Background(), envelopes)
		if err == nil || !strings.Contains(err.Error(), "duplicate or out-of-order snapshot") {
			t.Fatalf("Run() error = %v, want it to name a duplicate or out-of-order snapshot", err)
		}
	})
}

// TestReplayingRebaseRecoveryCashMovementFixtureTwiceYieldsByteIdenticalEmissions
// is the determinism property test for #17's whole seam: a fixture
// combining a re-basing, a Drawdown Step measured against the new figure, a
// cash movement, and a full recovery, replayed twice, must produce
// byte-identical output.
func TestReplayingRebaseRecoveryCashMovementFixtureTwiceYieldsByteIdenticalEmissions(t *testing.T) {
	t.Parallel()

	buildFixture := func(t *testing.T) []event.Envelope {
		t.Helper()
		cfg := validConfigurationPayload()
		snap1 := accountSnapshotPayload(jan(2, 2026).Add(time.Hour), 970_000) // establishes the period
		snap2 := accountSnapshotPayload(jan(1, 2027), 900_000)                // rebases to 900,000
		snap3 := accountSnapshotPayload(jan(2, 2027), 810_000)                // step: -> 720,000, base 810,000
		movement := cashMovementPayload(jan(3, 2027), 50_000, 800_000)        // deposit: scales S to 956,250
		snap4 := accountSnapshotPayload(jan(4, 2027), 960_000)                // full recovery (>= the scaled 956,250)
		return []event.Envelope{
			configEnvelopeWithConfig(t, 1, day(0), cfg),
			accountSnapshotEnvelope(t, 2, snap1, snap1.AsOf),
			accountSnapshotEnvelope(t, 3, snap2, snap2.AsOf),
			accountSnapshotEnvelope(t, 4, snap3, snap3.AsOf),
			cashMovementEnvelope(t, 5, movement, movement.AsOf),
			accountSnapshotEnvelope(t, 6, snap4, snap4.AsOf),
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
