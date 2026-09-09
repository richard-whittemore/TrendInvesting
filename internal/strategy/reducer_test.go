package strategy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/indicator"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

const (
	testStrategyVersion   = "test-strategy-1.0.0"
	testConfigurationHash = "cfg-test"
)

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%v) error = %v", v, err)
	}
	return b
}

// validConfigurationPayload mirrors event package's own fixture (a Baseline
// configuration, ADRs 0002/0003/0005/0007/0013) so this package's tests do
// not have to reach into event_test's unexported helper.
func validConfigurationPayload() event.ConfigurationPayload {
	return event.ConfigurationPayload{
		StrategyID:             "turtle-baseline",
		SizingMode:             event.SizingModeVolatilityNormalised,
		UnitVolatilityFraction: 0.005,
		StopMultiple:           2.0,
		EntryChannelLength:     55,
		ExitChannelLength:      20,
		MaxUnits:               4,
		SlippageN:              0.05,
		// #9: 1.0 is a Baseline-declared adaptation (ADR 0012's provenance
		// taxonomy), not a Faith number — used here only as a test fixture
		// default. Whoever owns the Baseline configuration (#50) must pick
		// this deliberately.
		TierBDistanceInN: 1.0,
		// #10: the Baseline trades US equities, where one point of price is
		// exactly one dollar per share. Faith's futures examples need the
		// contract multiplier instead (42,000 for Heating Oil, The Turtle
		// Rules p.15), which is why this is a parameter rather than a
		// hard-coded 1.
		DollarsPerPoint: 1,
		// #10: Risk at Stop is derived, never configured, under the Baseline's
		// volatility-normalised Sizing Mode (ADR 0003) — so this field must
		// be zero here, and ConfigurationPayload.Validate rejects any other
		// value while SizingMode is volatility-normalised.
		RiskAtStopFraction: 0,
		NotionalAccount: event.NotionalAccountConfig{
			StartingEquity: 1_000_000,
			RebasingMonth:  1,
			RebasingDay:    1,
		},
	}
}

func configEnvelope(t *testing.T, sequence uint64, at time.Time) event.Envelope {
	t.Helper()
	payload := mustMarshal(t, validConfigurationPayload())
	return event.Envelope{
		ID:                fmt.Sprintf("cfg-%d", sequence),
		Type:              event.ConfigurationEventType,
		SchemaVersion:     event.ConfigurationSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         at,
		RecordedAt:        at,
		Sequence:          sequence,
		Source:            "fixture",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// configEnvelopeWithConfig behaves like configEnvelope but lets the caller
// supply a full ConfigurationPayload, for tests that vary EntryChannelLength
// or TierBDistanceInN away from the Baseline fixture.
func configEnvelopeWithConfig(t *testing.T, sequence uint64, at time.Time, payload event.ConfigurationPayload) event.Envelope {
	t.Helper()
	encoded := mustMarshal(t, payload)
	return event.Envelope{
		ID:                fmt.Sprintf("cfg-%d", sequence),
		Type:              event.ConfigurationEventType,
		SchemaVersion:     event.ConfigurationSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         at,
		RecordedAt:        at,
		Sequence:          sequence,
		Source:            "fixture",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(encoded),
		Payload:           encoded,
	}
}

// priceView builds a PriceView with Open=Close so the cross-field
// consistency checks in bar.go (high>=open, low<=open, etc.) reduce to
// "close is between low and high", which every test in this file arranges
// directly.
func priceView(view string, high, low, closeAt float64) event.PriceView {
	return event.PriceView{
		View:   view,
		Open:   closeAt,
		High:   high,
		Low:    low,
		Close:  closeAt,
		Volume: 1_000_000,
	}
}

// completedBar builds a bar whose split-adjusted and raw views are
// identical, for tests that are not specifically about ADR 0004.
func completedBar(instrumentID string, periodEnd time.Time, high, low, closeAt float64) event.CompletedBarPayload {
	return event.CompletedBarPayload{
		InstrumentID:  instrumentID,
		PeriodEnd:     periodEnd,
		SplitAdjusted: priceView(event.ViewSplitAdjusted, high, low, closeAt),
		Raw:           priceView(event.ViewRaw, high, low, closeAt),
	}
}

func barEnvelope(t *testing.T, sequence uint64, bar event.CompletedBarPayload, recordedAt time.Time) event.Envelope {
	t.Helper()
	payload := mustMarshal(t, bar)
	return event.Envelope{
		ID:                fmt.Sprintf("bar-%d", sequence),
		Type:              event.CompletedBarEventType,
		SchemaVersion:     event.CompletedBarSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         bar.PeriodEnd,
		RecordedAt:        recordedAt,
		Sequence:          sequence,
		Source:            "fixture",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

func decodeSetupEvaluated(t *testing.T, envelope event.Envelope) event.SetupEvaluatedPayload {
	t.Helper()
	if envelope.Type != event.SetupEvaluatedEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.SetupEvaluatedEventType)
	}
	var payload event.SetupEvaluatedPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload
}

func decodeSignal(t *testing.T, envelope event.Envelope) event.SignalPayload {
	t.Helper()
	if envelope.Type != event.SignalEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.SignalEventType)
	}
	var payload event.SignalPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload
}

// runReducerOverHighs replays a configuration event (payload cfg) followed by
// one bar per entry in highs for instrumentID, and returns every envelope the
// engine emitted. Each bar's split-adjusted High is exactly highs[i], with
// Low and Close fixed at 100 (see syntheticBar's doc comment): True Range
// resolves to exactly highs[i]-100, and highs[i] is exactly what feeds the
// Entry Channel.
func runReducerOverHighs(t *testing.T, instrumentID string, highs []float64, cfg event.ConfigurationPayload) []event.Envelope {
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

// referenceStep is one bar's expected Setup-evaluated outcome, as computed
// by referenceSetupSteps.
type referenceStep struct {
	n                 float64
	nReady            bool
	entryChannelHigh  float64
	entryChannelReady bool
	tier              string
	distance          float64
	breakout          bool
}

// referenceSetupSteps is a second, independently written computation of what
// the reducer under test should produce for a series of bars whose
// split-adjusted High is highs[i] (Low and Close fixed at 100, so True Range
// is highs[i]-100 — see runReducerOverHighs). It uses
// internal/indicator.SMASeed and internal/indicator.WilderNext (already
// covered directly by internal/indicator's own tests) for N, and a plain
// slice scan — deliberately not internal/indicator.EntryChannel — for the
// channel high, so this oracle does not share a bug with either the
// production reducer or the type it wires together. It is the ground truth
// several event-seam tests below compare the reducer's actual output
// against, in place of hand-deriving many bars' worth of Tier/distance
// arithmetic by hand.
//
// Both inputs are read BEFORE the bar is folded into them (PR #64 review):
// the decision N and the channel high are the values standing after the
// PRECEDING bars, and this bar's True Range and high are added afterwards,
// for the next bar to see. This mirrors the reducer's own evaluate-then-add
// ordering, so the oracle and the reducer agree about which bars each
// decision may see; the property it is checking — that the decision bar is
// never an input to its own decision (CONTEXT.md: "Completed bar") — is
// asserted independently by
// TestReducerSizesFromNThroughThePrecedingBarNotTheSignalBar and
// TestAddThenEvaluateNWouldShrinkTheWideBarsOwnUnit, which do not use this
// helper at all.
func referenceSetupSteps(highs []float64, period, channelLength int, tierBDistance float64) []referenceStep {
	steps := make([]referenceStep, len(highs))
	var nValue float64
	var seed []float64
	var window []float64 // completed-bar highs, in order, added AFTER each step

	for i, high := range highs {
		tr := high - 100

		// Evaluate: N as it stands after the preceding bars only.
		decisionN := nValue
		nReady := i >= period && decisionN > 0

		var channelHigh float64
		channelReady := len(window) >= channelLength
		if channelReady {
			start := len(window) - channelLength
			channelHigh = window[start]
			for _, v := range window[start:] {
				if v > channelHigh {
					channelHigh = v
				}
			}
		}
		breakout := channelReady && high > channelHigh

		ready := nReady && channelReady
		var tier string
		var distance float64
		if ready {
			distance = (channelHigh - high) / decisionN
			switch {
			case breakout:
				tier = event.TierA
			case distance >= 0 && distance <= tierBDistance:
				// A tie (distance exactly 0) is at least Tier B, never Tier
				// none: it is the closest possible approach without a
				// breakout, and TierBDistanceInN is always non-negative.
				tier = event.TierB
			default:
				tier = event.TierNone
			}
		}

		reportedN := decisionN
		if !nReady {
			reportedN = 0
		}
		steps[i] = referenceStep{
			n:                 reportedN,
			nReady:            nReady,
			entryChannelHigh:  channelHigh,
			entryChannelReady: channelReady,
			tier:              tier,
			distance:          distance,
			breakout:          breakout,
		}

		// Add: fold this bar in, for the next bar's decision.
		switch {
		case len(seed) < period-1:
			seed = append(seed, tr)
		case len(seed) == period-1:
			seed = append(seed, tr)
			nValue = indicator.SMASeed(seed)
		default:
			nValue = indicator.WilderNext(nValue, tr, period)
		}
		window = append(window, high)
	}
	return steps
}

// syntheticTrueRanges reproduces internal/indicator's hand-computable 25-value
// True Range series (first 20 values 1..20, then five 5s) so this seam's
// expected N values are traceable to the same hand-worked arithmetic as
// TestWilderAverageSyntheticSeriesSeedAndFirstSteps, not just "whatever the
// code currently outputs".
func syntheticTrueRanges() []float64 {
	trs := make([]float64, 0, 25)
	for i := 1; i <= 20; i++ {
		trs = append(trs, float64(i))
	}
	for i := 0; i < 5; i++ {
		trs = append(trs, 5)
	}
	return trs
}

// syntheticBar builds a bar whose realised True Range is exactly tr,
// regardless of the previous bar: holding Low and Close fixed at 100 makes
// every gap term equal to either tr (high-low or high-previousClose, since
// previousClose is always 100) or 0 (previousClose-low, 100-100), so
// TrueRange always resolves to exactly high-low = tr. This isolates "does
// the reducer wire True Range and the Wilder average through correctly" from
// gap arithmetic, which internal/indicator already tests directly.
func syntheticBar(instrumentID string, periodEnd time.Time, tr float64) event.CompletedBarPayload {
	return completedBar(instrumentID, periodEnd, 100+tr, 100, 100)
}

func day(i int) time.Time {
	return time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
}

const epsilon = 1e-9

func TestReducerFailsClosedOnBarBeforeConfiguration(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	bar := syntheticBar("AAPL", day(0), 1.0)
	_, err = engine.Run(context.Background(), []event.Envelope{barEnvelope(t, 1, bar, day(0))})
	if err == nil {
		t.Fatal("Run() error = nil, want an error for a bar before any configuration")
	}
	if !strings.Contains(err.Error(), "configuration") {
		t.Fatalf("Run() error = %v, want it to name the missing configuration", err)
	}
}

// TestReducerEmitsOneSetupEvaluatedPerBarWithExpectedNAndReadiness is the
// primary event-seam test: config then 25 bars in, one Setup-evaluated event
// per bar out, carrying N and readiness matching the hand-worked values in
// internal/indicator's synthetic-series test.
func TestReducerEmitsOneSetupEvaluatedPerBarWithExpectedNAndReadiness(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	trs := syntheticTrueRanges()
	envelopes := []event.Envelope{configEnvelope(t, 1, day(0))}
	seq := uint64(2)
	for i, tr := range trs {
		bar := syntheticBar("AAPL", day(i+1), tr)
		envelopes = append(envelopes, barEnvelope(t, seq, bar, day(i+1)))
		seq++
	}

	emitted, err := engine.Run(context.Background(), envelopes)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(emitted) != len(trs) {
		t.Fatalf("len(emitted) = %d, want %d (one per bar, none for the configuration event)", len(emitted), len(trs))
	}

	// Evaluate-then-add (PR #64 review): the N a bar is decided against is
	// the N standing after the bars BEFORE it, so the 20-bar seed first
	// appears on bar 21's decision, not bar 20's. Bar 20 is the bar that
	// completes the seed; it does not get to use it. The values themselves
	// are unchanged — they are the same hand-worked series as
	// internal/indicator's TestWilderAverageSyntheticSeriesSeedAndFirstSteps
	// — each simply lands one bar later.
	wantReadyFrom := 21 // 1-indexed bar count at which N becomes ready
	wantNAfterSeed := []float64{
		10.5,        // bar 21 decides on the seed of bars 1..20
		10.225,      // bar 22
		9.96375,     // bar 23
		9.7155625,   // bar 24
		9.479784375, // bar 25
	}

	for i, decision := range emitted {
		barNumber := i + 1

		if decision.Source != "reducer" {
			t.Errorf("bar %d: Source = %q, want %q", barNumber, decision.Source, "reducer")
		}
		if decision.StrategyVersion != testStrategyVersion {
			t.Errorf("bar %d: StrategyVersion = %q, want %q", barNumber, decision.StrategyVersion, testStrategyVersion)
		}
		if decision.ConfigurationHash != testConfigurationHash {
			t.Errorf("bar %d: ConfigurationHash = %q, want %q", barNumber, decision.ConfigurationHash, testConfigurationHash)
		}
		if decision.EnvelopeVersion != event.CurrentEnvelopeVersion {
			t.Errorf("bar %d: EnvelopeVersion = %d, want %d", barNumber, decision.EnvelopeVersion, event.CurrentEnvelopeVersion)
		}
		wantEventTime := day(barNumber)
		if !decision.EventTime.Equal(wantEventTime) {
			t.Errorf("bar %d: EventTime = %v, want %v (the bar's period end)", barNumber, decision.EventTime, wantEventTime)
		}
		if !decision.RecordedAt.Equal(wantEventTime) {
			t.Errorf("bar %d: RecordedAt = %v, want %v (the input bar's RecordedAt, never time.Now())", barNumber, decision.RecordedAt, wantEventTime)
		}

		payload := decodeSetupEvaluated(t, decision)
		if payload.InstrumentID != "AAPL" {
			t.Errorf("bar %d: InstrumentID = %q, want AAPL", barNumber, payload.InstrumentID)
		}
		if !payload.PeriodEnd.Equal(wantEventTime) {
			t.Errorf("bar %d: PeriodEnd = %v, want %v", barNumber, payload.PeriodEnd, wantEventTime)
		}

		wantReady := barNumber >= wantReadyFrom
		if payload.NReady != wantReady {
			t.Errorf("bar %d: NReady = %v, want %v", barNumber, payload.NReady, wantReady)
		}

		switch {
		case barNumber < wantReadyFrom:
			if payload.N != 0 {
				t.Errorf("bar %d: N = %v, want 0 (not ready)", barNumber, payload.N)
			}
		default:
			want := wantNAfterSeed[barNumber-wantReadyFrom]
			if diff := math.Abs(payload.N - want); diff > epsilon {
				t.Errorf("bar %d: N = %v, want %v (diff %v)", barNumber, payload.N, want, diff)
			}
		}
	}
}

// TestReducerUsesSplitAdjustedViewOnly is the event-seam test for ADR 0004:
// the raw view here carries drastically different prices, and if the
// reducer ever mixed the two, N after warm-up would reflect the raw
// numbers rather than the split-adjusted ones.
func TestReducerUsesSplitAdjustedViewOnly(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	envelopes := []event.Envelope{configEnvelope(t, 1, day(0))}
	seq := uint64(2)
	// Twenty-one bars, not twenty: evaluate-then-add means the twentieth bar
	// completes the seed and the twenty-first is the first bar decided
	// against it (PR #64 review).
	const bars = 21
	for i := 0; i < bars; i++ {
		periodEnd := day(i + 1)
		bar := event.CompletedBarPayload{
			InstrumentID: "AAPL",
			PeriodEnd:    periodEnd,
			// True Range = 1 every bar: seed SMA = 1.0.
			SplitAdjusted: priceView(event.ViewSplitAdjusted, 101, 100, 100),
			// True Range = 1000 every bar: if this view leaked in, seed
			// SMA would be 1000.0 instead.
			Raw: priceView(event.ViewRaw, 1050, 50, 50),
		}
		envelopes = append(envelopes, barEnvelope(t, seq, bar, periodEnd))
		seq++
	}

	emitted, err := engine.Run(context.Background(), envelopes)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(emitted) != bars {
		t.Fatalf("len(emitted) = %d, want %d", len(emitted), bars)
	}

	last := decodeSetupEvaluated(t, emitted[len(emitted)-1])
	if !last.NReady {
		t.Fatalf("NReady = false on bar %d, want true", bars)
	}
	if diff := math.Abs(last.N - 1.0); diff > epsilon {
		t.Fatalf("N = %v, want ~1.0 (split-adjusted True Range); got a value near the raw view's True Range would indicate ADR 0004 is violated", last.N)
	}
}

// TestReducerKeepsSeparateStatePerInstrument warms AAPL up to Ready over 21
// bars (twenty complete the seed, the twenty-first is the first bar decided
// against it — see the evaluate-then-add note on the reducer), then feeds a
// single MSFT bar. If the two instruments shared one True Range/N tracker,
// that single MSFT bar would be decided against AAPL's warmed-up N and read
// as Ready; kept separate, it must be MSFT's first bar and read as not
// ready.
func TestReducerKeepsSeparateStatePerInstrument(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	envelopes := []event.Envelope{configEnvelope(t, 1, day(0))}
	seq := uint64(2)
	const aaplBars = 21
	for i := 0; i < aaplBars; i++ {
		bar := syntheticBar("AAPL", day(i+1), 1.0)
		envelopes = append(envelopes, barEnvelope(t, seq, bar, day(i+1)))
		seq++
	}
	msftBar := syntheticBar("MSFT", day(aaplBars+1), 1.0)
	envelopes = append(envelopes, barEnvelope(t, seq, msftBar, day(aaplBars+1)))

	emitted, err := engine.Run(context.Background(), envelopes)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(emitted) != aaplBars+1 {
		t.Fatalf("len(emitted) = %d, want %d", len(emitted), aaplBars+1)
	}

	aaplLast := decodeSetupEvaluated(t, emitted[aaplBars-1])
	if aaplLast.InstrumentID != "AAPL" || !aaplLast.NReady {
		t.Fatalf("AAPL on bar %d: InstrumentID=%q NReady=%v, want AAPL, true", aaplBars, aaplLast.InstrumentID, aaplLast.NReady)
	}

	msftFirst := decodeSetupEvaluated(t, emitted[aaplBars])
	if msftFirst.InstrumentID != "MSFT" {
		t.Fatalf("InstrumentID = %q, want MSFT", msftFirst.InstrumentID)
	}
	if msftFirst.NReady {
		t.Fatal("MSFT NReady = true on its first bar, want false: instrument state must not be shared with AAPL")
	}
	if msftFirst.N != 0 {
		t.Fatalf("MSFT N = %v on its first bar, want 0", msftFirst.N)
	}
}

// TestReducerBarCountWarmupIgnoresCalendarSpacing feeds 15 bars spread over
// 43 calendar days (far more than 20) and asserts N is still not ready:
// warm-up counts completed bars, never calendar days (CONTEXT.md: "Completed
// bar"). A calendar-day warm-up bug would report ready here.
func TestReducerBarCountWarmupIgnoresCalendarSpacing(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	envelopes := []event.Envelope{configEnvelope(t, 1, day(0))}
	seq := uint64(2)
	const bars = 15
	for i := 0; i < bars; i++ {
		// Three calendar days per bar: 15 bars span 43 calendar days
		// (day(1) .. day(43)), well past 20, while only 15 bars have been
		// completed.
		periodEnd := day(1 + i*3)
		bar := syntheticBar("AAPL", periodEnd, 1.0)
		envelopes = append(envelopes, barEnvelope(t, seq, bar, periodEnd))
		seq++
	}

	emitted, err := engine.Run(context.Background(), envelopes)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(emitted) != bars {
		t.Fatalf("len(emitted) = %d, want %d", len(emitted), bars)
	}

	last := decodeSetupEvaluated(t, emitted[len(emitted)-1])
	if last.NReady {
		t.Fatalf("NReady = true after %d completed bars spanning %d calendar days, want false (warm-up must count bars, not calendar days)", bars, 1+(bars-1)*3)
	}
}

// TestReducerRejectsUnrecognizedEventType documents the fail-closed choice
// made in Apply: an event type this reducer does not recognise is an error,
// not a silent no-op (docs/development.md principle 4).
func TestReducerRejectsUnrecognizedEventType(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	payload := json.RawMessage(`{}`)
	unknown := event.Envelope{
		ID:                "evt-1",
		Type:              "some.other.event",
		SchemaVersion:     1,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         day(0),
		RecordedAt:        day(0),
		Sequence:          1,
		Source:            "fixture",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}

	if _, err := engine.Run(context.Background(), []event.Envelope{unknown}); err == nil {
		t.Fatal("Run() error = nil, want error for an unrecognized event type")
	}
}

func TestNewReducerRequiresStrategyVersionAndConfigurationHash(t *testing.T) {
	t.Parallel()

	if _, err := strategy.NewReducer("", testConfigurationHash); err == nil {
		t.Fatal("NewReducer(\"\", ...) error = nil, want error")
	}
	if _, err := strategy.NewReducer(testStrategyVersion, ""); err == nil {
		t.Fatal("NewReducer(..., \"\") error = nil, want error")
	}
}

// TestReplayingSameFixtureTwiceYieldsByteIdenticalEmissions is the property
// test: two independent runs of the same input stream through two fresh
// Reducer instances must produce byte-identical emitted envelopes, proving
// there is no hidden non-determinism (no time.Now(), no randomness, no
// map-iteration-order dependence).
func TestReplayingSameFixtureTwiceYieldsByteIdenticalEmissions(t *testing.T) {
	t.Parallel()

	buildFixture := func(t *testing.T) []event.Envelope {
		t.Helper()
		envelopes := []event.Envelope{configEnvelope(t, 1, day(0))}
		seq := uint64(2)
		for i, tr := range syntheticTrueRanges() {
			bar := syntheticBar("AAPL", day(i+1), tr)
			envelopes = append(envelopes, barEnvelope(t, seq, bar, day(i+1)))
			seq++
		}
		msftBar := syntheticBar("MSFT", day(1), 3.0)
		envelopes = append(envelopes, barEnvelope(t, seq, msftBar, day(1)))
		return envelopes
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

// TestReducerRejectsInvalidConfigurationPayload confirms the reducer
// delegates to ConfigurationPayload.Validate() rather than trusting a
// well-formed-JSON-but-invalid payload (an empty object decodes cleanly but
// fails every required-field check).
func TestReducerRejectsInvalidConfigurationPayload(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	payload := json.RawMessage(`{}`)
	invalid := event.Envelope{
		ID:                "cfg-1",
		Type:              event.ConfigurationEventType,
		SchemaVersion:     event.ConfigurationSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         day(0),
		RecordedAt:        day(0),
		Sequence:          1,
		Source:            "fixture",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}

	_, err = engine.Run(context.Background(), []event.Envelope{invalid})
	if err == nil || !strings.Contains(err.Error(), "invalid configuration payload") {
		t.Fatalf("Run() error = %v, want it to name an invalid configuration payload", err)
	}
}

// TestReducerRejectsUndecodableConfigurationPayload covers the JSON-decode
// error path directly: the payload is valid JSON (so it passes
// Envelope.Validate()) but is not a JSON object, so it cannot decode into
// ConfigurationPayload.
func TestReducerRejectsUndecodableConfigurationPayload(t *testing.T) {
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
		ID:                "cfg-1",
		Type:              event.ConfigurationEventType,
		SchemaVersion:     event.ConfigurationSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         day(0),
		RecordedAt:        day(0),
		Sequence:          1,
		Source:            "fixture",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}

	_, err = engine.Run(context.Background(), []event.Envelope{undecodable})
	if err == nil || !strings.Contains(err.Error(), "decode configuration payload") {
		t.Fatalf("Run() error = %v, want it to name a decode failure", err)
	}
}

// TestReducerRejectsInvalidCompletedBarPayload confirms the reducer
// delegates to CompletedBarPayload.Validate() for a well-formed-JSON but
// invalid bar (missing instrument id).
func TestReducerRejectsInvalidCompletedBarPayload(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	bar := completedBar("", day(1), 101, 100, 100) // missing instrument id
	payload := mustMarshal(t, bar)
	invalid := event.Envelope{
		ID:                "bar-1",
		Type:              event.CompletedBarEventType,
		SchemaVersion:     event.CompletedBarSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         day(1),
		RecordedAt:        day(1),
		Sequence:          1,
		Source:            "fixture",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}

	envelopes := []event.Envelope{configEnvelope(t, 1, day(0))}
	invalid.Sequence = 2
	envelopes = append(envelopes, invalid)

	_, err = engine.Run(context.Background(), envelopes)
	if err == nil || !strings.Contains(err.Error(), "invalid completed bar payload") {
		t.Fatalf("Run() error = %v, want it to name an invalid completed bar payload", err)
	}
}

// TestReducerRejectsUndecodableCompletedBarPayload covers the JSON-decode
// error path for a bar: valid JSON, not a JSON object.
func TestReducerRejectsUndecodableCompletedBarPayload(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	payload := json.RawMessage(`42`)
	undecodable := event.Envelope{
		ID:                "bar-1",
		Type:              event.CompletedBarEventType,
		SchemaVersion:     event.CompletedBarSchemaVersion,
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

	envelopes := []event.Envelope{configEnvelope(t, 1, day(0)), undecodable}

	_, err = engine.Run(context.Background(), envelopes)
	if err == nil || !strings.Contains(err.Error(), "decode completed bar payload") {
		t.Fatalf("Run() error = %v, want it to name a decode failure", err)
	}
}

// TestReducerRejectsConfigurationWithWrongSchemaVersion is a Greptile PR #62
// finding (P1): a configuration envelope's SchemaVersion must equal
// event.ConfigurationSchemaVersion before the payload is even decoded. A
// schema-1 configuration (recorded before #9 added TierBDistanceInN) would
// otherwise decode cleanly with TierBDistanceInN defaulting to the float64
// zero value, which ConfigurationPayload.Validate accepts as legitimately
// "no Tier B window" — silently changing what Tier B means for that run
// instead of the run being rejected as incompatible. This mirrors ADR
// 0015's envelope-level rule at the payload level: an older or newer schema
// is rejected, never silently upgraded, until an explicit upcaster exists.
func TestReducerRejectsConfigurationWithWrongSchemaVersion(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	wrongVersion := configEnvelope(t, 1, day(0))
	wrongVersion.SchemaVersion = 1 // the original schema, two bumps behind

	_, err = engine.Run(context.Background(), []event.Envelope{wrongVersion})
	if err == nil {
		t.Fatal("Run() error = nil, want error for a configuration payload at the wrong schema version")
	}
	for _, want := range []string{"schema version", "1", fmt.Sprintf("%d", event.ConfigurationSchemaVersion)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Run() error = %v, want substring %q", err, want)
		}
	}
}

// TestReducerRejectsCompletedBarWithWrongSchemaVersion is the completed-bar
// counterpart of TestReducerRejectsConfigurationWithWrongSchemaVersion: a
// bar envelope's SchemaVersion must equal event.CompletedBarSchemaVersion
// before the payload is decoded.
func TestReducerRejectsCompletedBarWithWrongSchemaVersion(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	// event.CompletedBarSchemaVersion is 1; SchemaVersion 0 would also be
	// rejected, but at Envelope.Validate() ("schema version must be
	// positive") rather than by the check under test here, so a distinct
	// positive-but-wrong value (2) is used to exercise the reducer's own
	// schema check specifically.
	bar := syntheticBar("AAPL", day(1), 1.0)
	wrongVersion := barEnvelope(t, 2, bar, day(1))
	wrongVersion.SchemaVersion = 2

	envelopes := []event.Envelope{configEnvelope(t, 1, day(0)), wrongVersion}

	_, err = engine.Run(context.Background(), envelopes)
	if err == nil {
		t.Fatal("Run() error = nil, want error for a completed bar payload at the wrong schema version")
	}
	for _, want := range []string{"schema version", "2", "1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Run() error = %v, want substring %q", err, want)
		}
	}
}

// --- Greptile PR #60 findings ---

// TestReducerRejectsConfigurationWithMismatchedHash is Greptile finding 1
// (P1, applyConfiguration): a configuration envelope whose ConfigurationHash
// differs from the hash passed to NewReducer must be rejected, naming both
// hashes, rather than silently accepted while every later decision is
// stamped with the constructor's hash (an audit-attribution defect).
func TestReducerRejectsConfigurationWithMismatchedHash(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	const otherHash = "cfg-other"
	mismatched := configEnvelope(t, 1, day(0))
	mismatched.ConfigurationHash = otherHash // ConfigurationHash is envelope
	// provenance, independent of the payload bytes/hash, so this alone makes
	// the envelope carry a different configuration hash than the reducer's.

	_, err = engine.Run(context.Background(), []event.Envelope{mismatched})
	if err == nil {
		t.Fatal("Run() error = nil, want error for a configuration hash mismatch")
	}
	if !strings.Contains(err.Error(), testConfigurationHash) || !strings.Contains(err.Error(), otherHash) {
		t.Fatalf("Run() error = %v, want it to name both configuration hashes (%q and %q)", err, testConfigurationHash, otherHash)
	}
}

// TestReducerAcceptsConfigurationWithMatchingHash confirms the mismatch
// check above does not also reject a legitimately matching hash.
func TestReducerAcceptsConfigurationWithMatchingHash(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	// configEnvelope stamps ConfigurationHash = testConfigurationHash, matching
	// NewReducer's argument above.
	if _, err := engine.Run(context.Background(), []event.Envelope{configEnvelope(t, 1, day(0))}); err != nil {
		t.Fatalf("Run() error = %v, want a matching configuration hash to be accepted", err)
	}
}

// TestReducerRejectsSecondConfigurationEvent is the other half of finding 1:
// the reducer is configured once per run. ADR 0006 freezes a Campaign's
// configuration at entry; a mid-stream reconfiguration is not something this
// reducer supports, so a second configuration event — even one with a
// matching hash — is rejected rather than silently re-applied.
func TestReducerRejectsSecondConfigurationEvent(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	first := configEnvelope(t, 1, day(0))
	second := configEnvelope(t, 2, day(1))

	_, err = engine.Run(context.Background(), []event.Envelope{first, second})
	if err == nil {
		t.Fatal("Run() error = nil, want error for a second configuration event")
	}
	if !strings.Contains(err.Error(), "already configured") {
		t.Fatalf("Run() error = %v, want it to say the reducer is already configured", err)
	}
}

// TestReducerRejectsDuplicateBarPeriodEnd is Greptile finding 2 (P1,
// applyCompletedBar): an exact duplicate bar (same instrument, same
// PeriodEnd) must be rejected, naming the instrument, the last recorded
// period end, and the offending one, rather than silently advancing the
// accumulator and overwriting previousClose a second time for the same bar.
func TestReducerRejectsDuplicateBarPeriodEnd(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	first := syntheticBar("AAPL", day(1), 1.0)
	duplicate := syntheticBar("AAPL", day(1), 2.0) // same PeriodEnd as first

	envelopes := []event.Envelope{
		configEnvelope(t, 1, day(0)),
		barEnvelope(t, 2, first, day(1)),
		barEnvelope(t, 3, duplicate, day(1)),
	}

	_, err = engine.Run(context.Background(), envelopes)
	if err == nil {
		t.Fatal("Run() error = nil, want error for a duplicate bar period end")
	}
	for _, want := range []string{"AAPL", day(1).Format(time.RFC3339)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Run() error = %v, want substring %q", err, want)
		}
	}
}

// TestReducerRejectsOutOfOrderBarPeriodEnd is the other half of finding 2: a
// bar whose PeriodEnd is earlier than the last one recorded for the same
// instrument must be rejected the same way a duplicate is.
func TestReducerRejectsOutOfOrderBarPeriodEnd(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	later := syntheticBar("AAPL", day(5), 1.0)
	earlier := syntheticBar("AAPL", day(2), 1.0) // before day(5)

	envelopes := []event.Envelope{
		configEnvelope(t, 1, day(0)),
		barEnvelope(t, 2, later, day(5)),
		barEnvelope(t, 3, earlier, day(5)), // envelope's own RecordedAt is irrelevant here
	}

	_, err = engine.Run(context.Background(), envelopes)
	if err == nil {
		t.Fatal("Run() error = nil, want error for an out-of-order bar period end")
	}
	for _, want := range []string{"AAPL", day(5).Format(time.RFC3339), day(2).Format(time.RFC3339)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Run() error = %v, want substring %q", err, want)
		}
	}
}

// TestReducerAcceptsEarlierPeriodEndForDifferentInstrument confirms
// chronology is tracked per instrument, not globally: the replay.Engine's
// input Sequence — not PeriodEnd — is what orders the stream across
// different instruments, so a second instrument's earlier-dated bar must
// still be accepted.
func TestReducerAcceptsEarlierPeriodEndForDifferentInstrument(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	aapl := syntheticBar("AAPL", day(5), 1.0)
	msft := syntheticBar("MSFT", day(1), 1.0) // earlier than AAPL's, different instrument

	envelopes := []event.Envelope{
		configEnvelope(t, 1, day(0)),
		barEnvelope(t, 2, aapl, day(5)),
		barEnvelope(t, 3, msft, day(1)),
	}

	emitted, err := engine.Run(context.Background(), envelopes)
	if err != nil {
		t.Fatalf("Run() error = %v, want a different instrument's earlier bar to be accepted", err)
	}
	if len(emitted) != 2 {
		t.Fatalf("len(emitted) = %d, want 2", len(emitted))
	}
}

// TestReducerFlatInstrumentStaysNotReadyUntilNonZeroTrueRange is Greptile
// finding 3 (P2, event.SetupEvaluatedPayload.Validate): twenty flat bars
// (high == low == close) legitimately warm up in bar count but produce
// N == 0, which is not a usable volatility reading. The reducer must report
// NReady = false for those (not error out), and readiness must return the
// moment True Range is non-zero again.
func TestReducerFlatInstrumentStaysNotReadyUntilNonZeroTrueRange(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	envelopes := []event.Envelope{configEnvelope(t, 1, day(0))}
	seq := uint64(2)
	for i := 0; i < 20; i++ {
		bar := syntheticBar("AAPL", day(i+1), 0.0) // flat: True Range = 0
		envelopes = append(envelopes, barEnvelope(t, seq, bar, day(i+1)))
		seq++
	}
	// Two ranged bars, not one: under evaluate-then-add (PR #64 review) bar
	// 21 is still decided against the all-zero seed, and bar 22 is the first
	// bar that can see bar 21's non-zero True Range.
	for i := 21; i <= 22; i++ {
		rangedBar := syntheticBar("AAPL", day(i), 1.0) // True Range = 1
		envelopes = append(envelopes, barEnvelope(t, seq, rangedBar, day(i)))
		seq++
	}

	emitted, err := engine.Run(context.Background(), envelopes)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(emitted) != 22 {
		t.Fatalf("len(emitted) = %d, want 22", len(emitted))
	}

	for i := 0; i < 21; i++ {
		payload := decodeSetupEvaluated(t, emitted[i])
		if payload.NReady {
			t.Fatalf("bar %d: NReady = true for a flat instrument, want false (warm-up complete but N is not a usable reading)", i+1)
		}
		if payload.N != 0 {
			t.Fatalf("bar %d: N = %v, want 0", i+1, payload.N)
		}
	}

	last := decodeSetupEvaluated(t, emitted[21])
	if !last.NReady {
		t.Fatal("bar 22: NReady = false, want true: True Range was non-zero on bar 21")
	}
	// previousN is 0 (the 20-bar seed of an all-zero series); WilderNext(0, 1,
	// 20) = (19*0+1)/20 = 1/20, contributed by bar 21 and first visible to
	// bar 22's decision.
	want := 1.0 / 20.0
	if diff := math.Abs(last.N - want); diff > epsilon {
		t.Fatalf("bar 22: N = %v, want %v (diff %v)", last.N, want, diff)
	}
}

// --- #9: Entry Channel and Signal emission ---

// breakoutFixtureHighs returns the fifty-five warm-up highs (channel warm-up,
// ADR 0002: 55-bar Entry Channel) plus one breakout high, all hand-derivable:
// bars 1..55 have High = 100+i (i=1..55, so highs step 101..155 and the
// Entry Channel's high after bar 55 is exactly 155), and bar 56's High is
// 200 — comfortably the new maximum across all 56 bars. That last property
// is exactly the shape TestLookAheadEntryChannelWouldMissTheBreakout depends
// on: a look-ahead implementation that includes the decision bar in its own
// channel computes channelHigh=200 for bar 56 too, sees "200 is not > 200",
// and emits nothing.
func breakoutFixtureHighs() []float64 {
	highs := make([]float64, 0, 56)
	for i := 1; i <= 55; i++ {
		highs = append(highs, 100+float64(i))
	}
	return append(highs, 200)
}

// TestReducerEmitsExactlyOneSignalOnBreakoutBar is the primary event-seam
// test: a fixture with 55 warm-up bars (channel and N both ready well before
// the end) plus one breakout bar emits exactly one Signal, on the expected
// bar, naming the expected rule, ADR, direction, channel high, breakout
// high, and N.
func TestReducerEmitsExactlyOneSignalOnBreakoutBar(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	highs := breakoutFixtureHighs()
	emitted := runReducerOverHighs(t, "AAPL", highs, cfg)

	// 55 warm-up bars each emit one Setup-evaluated event (no breakout: the
	// channel is not ready until bar 55 has been added, i.e. when
	// evaluating bar 56); bar 56 emits a Setup-evaluated event, a Signal,
	// and — since #10 — the trade proposal the Signal is sized into.
	if len(emitted) != len(highs)+2 {
		t.Fatalf("len(emitted) = %d, want %d (55 bars x 1 event, plus bar 56's 3 events)", len(emitted), len(highs)+2)
	}

	var signals []event.Envelope
	for _, e := range emitted {
		if e.Type == event.SignalEventType {
			signals = append(signals, e)
		}
	}
	if len(signals) != 1 {
		t.Fatalf("got %d Signal events, want exactly 1", len(signals))
	}

	wantEventTime := day(56)
	signalEnvelope := signals[0]
	if !signalEnvelope.EventTime.Equal(wantEventTime) {
		t.Errorf("Signal EventTime = %v, want %v", signalEnvelope.EventTime, wantEventTime)
	}
	if signalEnvelope.Source != "reducer" {
		t.Errorf("Signal Source = %q, want %q", signalEnvelope.Source, "reducer")
	}
	if signalEnvelope.StrategyVersion != testStrategyVersion {
		t.Errorf("Signal StrategyVersion = %q, want %q", signalEnvelope.StrategyVersion, testStrategyVersion)
	}
	if signalEnvelope.ConfigurationHash != testConfigurationHash {
		t.Errorf("Signal ConfigurationHash = %q, want %q", signalEnvelope.ConfigurationHash, testConfigurationHash)
	}

	signal := decodeSignal(t, signalEnvelope)
	if signal.InstrumentID != "AAPL" {
		t.Errorf("Signal InstrumentID = %q, want AAPL", signal.InstrumentID)
	}
	if !signal.PeriodEnd.Equal(wantEventTime) {
		t.Errorf("Signal PeriodEnd = %v, want %v", signal.PeriodEnd, wantEventTime)
	}
	if signal.Rule != event.RuleEntryChannelBreakout {
		t.Errorf("Signal Rule = %q, want %q", signal.Rule, event.RuleEntryChannelBreakout)
	}
	if signal.ADR != event.ADREntryChannelBreakout {
		t.Errorf("Signal ADR = %q, want %q", signal.ADR, event.ADREntryChannelBreakout)
	}
	if signal.Direction != event.DirectionLong {
		t.Errorf("Signal Direction = %q, want %q", signal.Direction, event.DirectionLong)
	}
	if signal.EntryChannelLength != cfg.EntryChannelLength {
		t.Errorf("Signal EntryChannelLength = %d, want %d (the length actually configured, not a hard-coded 55)", signal.EntryChannelLength, cfg.EntryChannelLength)
	}
	if signal.EntryChannelHigh != 155 {
		t.Errorf("Signal EntryChannelHigh = %v, want 155 (the highest high of bars 1..55)", signal.EntryChannelHigh)
	}
	if signal.BreakoutHigh != 200 {
		t.Errorf("Signal BreakoutHigh = %v, want 200 (bar 56's own high)", signal.BreakoutHigh)
	}

	steps := referenceSetupSteps(highs, indicator.DefaultPeriod, cfg.EntryChannelLength, cfg.TierBDistanceInN)
	wantN := steps[len(steps)-1].n
	if diff := math.Abs(signal.N - wantN); diff > epsilon {
		t.Errorf("Signal N = %v, want %v (diff %v)", signal.N, wantN, diff)
	}

	// The Setup-evaluated event on the same bar must report Tier A and the
	// same channel high, and the Signal must be emitted strictly after it
	// in the returned slice (Apply returns [setup-evaluated, signal,
	// proposal] since #10; TestReducerEmitsTradeProposalOnSignal asserts the
	// third).
	setupIndex := len(emitted) - 3
	signalIndex := len(emitted) - 2
	if emitted[setupIndex].Type != event.SetupEvaluatedEventType {
		t.Fatalf("emitted[%d].Type = %q, want %q", setupIndex, emitted[setupIndex].Type, event.SetupEvaluatedEventType)
	}
	if emitted[signalIndex].Type != event.SignalEventType {
		t.Fatalf("emitted[%d].Type = %q, want %q", signalIndex, emitted[signalIndex].Type, event.SignalEventType)
	}
	setup := decodeSetupEvaluated(t, emitted[setupIndex])
	if setup.Tier != event.TierA {
		t.Errorf("Setup-evaluated Tier = %q, want %q", setup.Tier, event.TierA)
	}
	if setup.EntryChannelHigh != 155 {
		t.Errorf("Setup-evaluated EntryChannelHigh = %v, want 155", setup.EntryChannelHigh)
	}
	if !setup.EntryChannelReady {
		t.Error("Setup-evaluated EntryChannelReady = false, want true")
	}
	if setup.DistanceToEntryInN >= 0 {
		t.Errorf("Setup-evaluated DistanceToEntryInN = %v, want negative (a breakout)", setup.DistanceToEntryInN)
	}
}

// TestLookAheadEntryChannelWouldMissTheBreakout is the ticket's headline
// negative test: the predecessor prototype's Donchian read included the
// current bar in its own channel before comparing
// (docs/methodology/Plan_and_Ticket_Review.md: "look-ahead in the Donchian
// read", in its bug taxonomy). On the exact fixture that the correctly
// ordered production reducer turns into exactly one Signal
// (TestReducerEmitsExactlyOneSignalOnBreakoutBar), a look-ahead channel
// computes the channel high AS the bar's own high whenever that bar is the
// new maximum, so "high exceeds channel high" reduces to "high exceeds
// itself" — never true — and produces zero breakouts.
//
// The look-ahead variant is written entirely locally here: it does not call
// anything in internal/indicator or internal/strategy, so this test cannot
// pass by accidentally exercising the production code twice.
func TestLookAheadEntryChannelWouldMissTheBreakout(t *testing.T) {
	t.Parallel()

	highs := breakoutFixtureHighs()
	const channelLength = 55

	// lookAheadBreakouts reproduces the bug: for each bar it adds the
	// CURRENT bar's high to the window BEFORE computing the channel high,
	// so the channel always includes the bar it is supposedly deciding.
	lookAheadBreakouts := func(highs []float64, length int) int {
		var window []float64
		breakouts := 0
		for _, h := range highs {
			window = append(window, h) // the bug: added before the read
			if len(window) < length {
				continue
			}
			start := len(window) - length
			channelHigh := window[start]
			for _, v := range window[start:] {
				if v > channelHigh {
					channelHigh = v
				}
			}
			if h > channelHigh {
				breakouts++
			}
		}
		return breakouts
	}

	if got := lookAheadBreakouts(highs, channelLength); got != 0 {
		t.Fatalf("look-ahead channel computed %d breakouts on the fixture, want 0 (a channel that already contains the bar's own high can never be exceeded by it)", got)
	}

	// The production reducer, on the identical fixture, must find exactly
	// one: this confirms the two are not just individually plausible but
	// genuinely opposite outcomes on the same input, and pins the defense
	// (evaluate-then-add ordering, internal/indicator.EntryChannel) against
	// regressing back to the look-ahead shape above.
	cfg := validConfigurationPayload()
	emitted := runReducerOverHighs(t, "AAPL", highs, cfg)

	signalCount := 0
	for _, e := range emitted {
		if e.Type == event.SignalEventType {
			signalCount++
		}
	}
	if signalCount != 1 {
		t.Fatalf("production reducer emitted %d Signal(s) on the same fixture, want exactly 1", signalCount)
	}
}

// TestReducerTieAtChannelHighIsTierBNotABreakout locks in Faith's word
// choice (The Turtle Rules p.19: "exceeds"): a bar whose high exactly equals
// the channel high is not a Breakout (no Signal), and therefore not Tier A.
// It IS Tier B: a tie's distance is exactly 0, the closest possible approach
// without a breakout, and TierBDistanceInN is always non-negative
// (ConfigurationPayload.Validate), so 0 always falls within [0,
// TierBDistanceInN] — ADR 0011's Watchlist exists to surface exactly this
// case.
func TestReducerTieAtChannelHighIsTierBNotABreakout(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	warmupHighs := make([]float64, 55)
	for i := range warmupHighs {
		warmupHighs[i] = 100 + float64(i+1) // channel high after warm-up is 155
	}
	highs := append(append([]float64{}, warmupHighs...), 155) // tie: bar 56's high == channel high

	emitted := runReducerOverHighs(t, "AAPL", highs, cfg)
	if len(emitted) != len(highs) {
		t.Fatalf("len(emitted) = %d, want %d (a tie is not a breakout, so no Signal)", len(emitted), len(highs))
	}
	for _, e := range emitted {
		if e.Type == event.SignalEventType {
			t.Fatal("unexpected Signal emitted for a tie at the channel high")
		}
	}

	last := decodeSetupEvaluated(t, emitted[len(emitted)-1])
	if last.EntryChannelHigh != 155 {
		t.Fatalf("EntryChannelHigh = %v, want 155", last.EntryChannelHigh)
	}
	if last.DistanceToEntryInN != 0 {
		t.Fatalf("DistanceToEntryInN = %v, want 0 (a tie)", last.DistanceToEntryInN)
	}
	if last.Tier != event.TierB {
		t.Fatalf("Tier = %q, want %q (a tie is the closest possible approach without a breakout)", last.Tier, event.TierB)
	}
}

// TestReducerTierBAndTierNoneBasedOnConfiguredDistance builds two fixtures
// that are identical except for the final bar's high, one intended to land
// within TierBDistanceInN of the channel and one intended to land beyond it.
// referenceSetupSteps (an independent computation) is used first to confirm
// each fixture actually lands in its intended regime — this is checked
// explicitly rather than assumed, so a bad fixture choice fails loudly and
// separately from any defect in the reducer itself — and then the
// reducer's actual output is compared against that same independent
// computation.
func TestReducerTierBAndTierNoneBasedOnConfiguredDistance(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload() // TierBDistanceInN: 1.0
	warmupHighs := make([]float64, 55)
	for i := range warmupHighs {
		warmupHighs[i] = 100 + float64(i+1) // channel high after warm-up is 155
	}

	tests := []struct {
		name     string
		high     float64
		wantTier string
	}{
		{"within tier b distance", 150, event.TierB},
		{"beyond tier b distance", 105, event.TierNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			highs := append(append([]float64{}, warmupHighs...), tt.high)

			steps := referenceSetupSteps(highs, indicator.DefaultPeriod, cfg.EntryChannelLength, cfg.TierBDistanceInN)
			last := steps[len(steps)-1]
			if last.tier != tt.wantTier {
				t.Fatalf("fixture does not exercise tier %q: independent computation gives tier %q, distance %v; adjust the final bar's high (%v)", tt.wantTier, last.tier, last.distance, tt.high)
			}
			if last.breakout {
				t.Fatalf("fixture unexpectedly breaks out (high %v); this test is only about Tier B vs Tier none", tt.high)
			}

			emitted := runReducerOverHighs(t, "AAPL", highs, cfg)
			if len(emitted) != len(highs) {
				t.Fatalf("len(emitted) = %d, want %d (no Signal expected for this bar)", len(emitted), len(highs))
			}
			for _, e := range emitted {
				if e.Type == event.SignalEventType {
					t.Fatal("unexpected Signal emitted for a non-breakout bar")
				}
			}

			got := decodeSetupEvaluated(t, emitted[len(emitted)-1])
			if got.Tier != last.tier {
				t.Fatalf("Tier = %q, want %q", got.Tier, last.tier)
			}
			if diff := math.Abs(got.DistanceToEntryInN - last.distance); diff > epsilon {
				t.Fatalf("DistanceToEntryInN = %v, want %v (diff %v)", got.DistanceToEntryInN, last.distance, diff)
			}
			if diff := math.Abs(got.N - last.n); diff > epsilon {
				t.Fatalf("N = %v, want %v (diff %v)", got.N, last.n, diff)
			}
			if got.EntryChannelHigh != last.entryChannelHigh {
				t.Fatalf("EntryChannelHigh = %v, want %v", got.EntryChannelHigh, last.entryChannelHigh)
			}
		})
	}
}

// TestReducerNoSignalWhileNNotReady constructs a configuration whose
// EntryChannelLength (5) is shorter than N's fixed 20-bar warm-up
// (indicator.DefaultPeriod), so the channel becomes ready first: bar 6 meets
// the breakout condition against the channel, but N is not yet ready, and
// must not produce a Signal or a Tier.
func TestReducerNoSignalWhileNNotReady(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	cfg.EntryChannelLength = 5

	highs := []float64{101, 102, 103, 104, 105, 200} // channel ready at bar 6 (5 warm-up + this one); N needs 20
	emitted := runReducerOverHighs(t, "AAPL", highs, cfg)
	if len(emitted) != len(highs) {
		t.Fatalf("len(emitted) = %d, want %d (no Signal: N is not ready)", len(emitted), len(highs))
	}
	for _, e := range emitted {
		if e.Type == event.SignalEventType {
			t.Fatal("unexpected Signal emitted while N is not ready")
		}
	}

	last := decodeSetupEvaluated(t, emitted[len(emitted)-1])
	if last.NReady {
		t.Fatal("NReady = true after 6 bars, want false (N needs 20)")
	}
	if !last.EntryChannelReady {
		t.Fatal("EntryChannelReady = false after 6 bars with EntryChannelLength=5, want true")
	}
	if last.EntryChannelHigh != 105 {
		t.Fatalf("EntryChannelHigh = %v, want 105 (the highest high of bars 1..5)", last.EntryChannelHigh)
	}
	if last.Tier != event.TierNone {
		t.Fatalf("Tier = %q, want %q (N is not ready, even though the channel condition is met)", last.Tier, event.TierNone)
	}
	if last.DistanceToEntryInN != 0 {
		t.Fatalf("DistanceToEntryInN = %v, want 0 (not meaningful while N is not ready)", last.DistanceToEntryInN)
	}
}

// TestReducerSignalCarriesTheConfiguredEntryChannelLength is a Greptile PR
// #62 finding: a Signal's Rule must never hard-code a channel length, or a
// Faith system label, that a Variant could configure differently — System 1
// is a 20-day channel and System 2 a 55-day one (ADR 0002), so naming the
// rule after either system would misdescribe a Variant configured with the
// other's length. A Variant configured with EntryChannelLength: 5 (neither
// Faith system) must produce a Signal whose EntryChannelLength is 5 and
// whose Rule is the mechanism-named, length-independent
// event.RuleEntryChannelBreakout.
func TestReducerSignalCarriesTheConfiguredEntryChannelLength(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	cfg.EntryChannelLength = 5

	// 25 warm-up bars, all high=101 (channel ready after 5, N ready after
	// 20; a constant series never breaks out — each bar ties its own
	// 5-bar channel), then one breakout well above it.
	highs := make([]float64, 25)
	for i := range highs {
		highs[i] = 101
	}
	highs = append(highs, 200)

	emitted := runReducerOverHighs(t, "AAPL", highs, cfg)

	var signals []event.Envelope
	for _, e := range emitted {
		if e.Type == event.SignalEventType {
			signals = append(signals, e)
		}
	}
	if len(signals) != 1 {
		t.Fatalf("got %d Signal(s), want exactly 1", len(signals))
	}

	signal := decodeSignal(t, signals[0])
	if signal.EntryChannelLength != 5 {
		t.Fatalf("Signal EntryChannelLength = %d, want 5 (the configured length, not the Baseline's 55)", signal.EntryChannelLength)
	}
	if signal.Rule != event.RuleEntryChannelBreakout {
		t.Fatalf("Signal Rule = %q, want %q (length-independent; the length lives in its own field)", signal.Rule, event.RuleEntryChannelBreakout)
	}
}

// TestReducerNoSignalWhileEntryChannelNotReady is the mirror case: N becomes
// ready at bar 20 (the Baseline's EntryChannelLength, 55, is far from full),
// so a Setup can be NReady with the channel still not ready, and must not
// produce a Signal or a Tier either.
func TestReducerNoSignalWhileEntryChannelNotReady(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload() // EntryChannelLength: 55
	highs := make([]float64, 25)
	for i := range highs {
		highs[i] = 100 + float64(i+1)
	}

	emitted := runReducerOverHighs(t, "AAPL", highs, cfg)
	if len(emitted) != len(highs) {
		t.Fatalf("len(emitted) = %d, want %d", len(emitted), len(highs))
	}
	for _, e := range emitted {
		if e.Type == event.SignalEventType {
			t.Fatal("unexpected Signal emitted while the Entry Channel is not ready")
		}
	}

	last := decodeSetupEvaluated(t, emitted[len(emitted)-1])
	if !last.NReady {
		t.Fatal("NReady = false after 25 bars, want true")
	}
	if last.EntryChannelReady {
		t.Fatal("EntryChannelReady = true after 25 bars with EntryChannelLength=55, want false")
	}
	if last.EntryChannelHigh != 0 {
		t.Fatalf("EntryChannelHigh = %v, want 0 (not ready)", last.EntryChannelHigh)
	}
	if last.Tier != event.TierNone {
		t.Fatalf("Tier = %q, want %q (the Entry Channel is not ready)", last.Tier, event.TierNone)
	}
	if last.DistanceToEntryInN != 0 {
		t.Fatalf("DistanceToEntryInN = %v, want 0 (not meaningful while the Entry Channel is not ready)", last.DistanceToEntryInN)
	}
}

// TestReducerSignalsExpirePerBar is the event-seam expression of ADR 0011: a
// Signal belongs to one bar. Two consecutive breakout bars each get their
// own Signal (proving no "already signaled" memory suppresses the second
// one), and a bar that follows a breakout but does not exceed the *updated*
// channel (which now includes the breakout bar) emits none.
func TestReducerSignalsExpirePerBar(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	warmupHighs := make([]float64, 55)
	for i := range warmupHighs {
		warmupHighs[i] = 100 + float64(i+1) // channel high after warm-up is 155
	}
	highs := append(append([]float64{}, warmupHighs...),
		200, // bar 56: breakout #1 (channel high 155)
		250, // bar 57: breakout #2 (channel now includes 200, so its high is 200)
		210, // bar 58: does not exceed the now-updated channel high (250)
	)

	emitted := runReducerOverHighs(t, "AAPL", highs, cfg)

	var signalBars []time.Time
	for _, e := range emitted {
		if e.Type == event.SignalEventType {
			signalBars = append(signalBars, e.EventTime)
		}
	}
	want := []time.Time{day(56), day(57)}
	if len(signalBars) != len(want) {
		t.Fatalf("got %d Signal(s) at %v, want %d at %v", len(signalBars), signalBars, len(want), want)
	}
	for i, w := range want {
		if !signalBars[i].Equal(w) {
			t.Errorf("Signal %d at %v, want %v", i, signalBars[i], w)
		}
	}
}

// TestReducerKeepsSeparateEntryChannelPerInstrument confirms a second
// instrument does not inherit the first instrument's Entry Channel state: an
// MSFT bar fed after AAPL's full breakout fixture must be evaluated against
// its own empty channel, not AAPL's warmed-up one, even though its high
// would otherwise satisfy AAPL's breakout condition.
func TestReducerKeepsSeparateEntryChannelPerInstrument(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	envelopes := []event.Envelope{configEnvelope(t, 1, day(0))}
	seq := uint64(2)
	for i, high := range breakoutFixtureHighs() {
		periodEnd := day(i + 1)
		bar := syntheticBar("AAPL", periodEnd, high-100)
		envelopes = append(envelopes, barEnvelope(t, seq, bar, periodEnd))
		seq++
	}
	// A high that would exceed AAPL's warmed-up channel (155) many times
	// over, fed as MSFT's very first bar.
	msftBar := syntheticBar("MSFT", day(57), 900)
	envelopes = append(envelopes, barEnvelope(t, seq, msftBar, day(57)))

	emitted, err := engine.Run(context.Background(), envelopes)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	msftDecision := decodeSetupEvaluated(t, emitted[len(emitted)-1])
	if msftDecision.InstrumentID != "MSFT" {
		t.Fatalf("InstrumentID = %q, want MSFT", msftDecision.InstrumentID)
	}
	if msftDecision.EntryChannelReady {
		t.Fatal("MSFT EntryChannelReady = true on its first bar, want false: channel state must not be shared with AAPL")
	}
	if msftDecision.Tier != event.TierNone {
		t.Fatalf("MSFT Tier = %q on its first bar, want %q", msftDecision.Tier, event.TierNone)
	}
	if emitted[len(emitted)-1].Type == event.SignalEventType {
		t.Fatal("unexpected Signal emitted for MSFT's first bar")
	}
}

// TestReducerRejectsConfigurationWithNonPositiveEntryChannelLength confirms
// the reducer surfaces ConfigurationPayload.Validate's rejection of a
// non-positive EntryChannelLength, naming it.
func TestReducerRejectsConfigurationWithNonPositiveEntryChannelLength(t *testing.T) {
	t.Parallel()

	for _, length := range []int{0, -55} {
		reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
		if err != nil {
			t.Fatalf("NewReducer() error = %v", err)
		}
		engine, err := replay.New(reducer)
		if err != nil {
			t.Fatalf("replay.New() error = %v", err)
		}

		cfg := validConfigurationPayload()
		cfg.EntryChannelLength = length

		_, err = engine.Run(context.Background(), []event.Envelope{configEnvelopeWithConfig(t, 1, day(0), cfg)})
		if err == nil || !strings.Contains(err.Error(), "entry channel length") {
			t.Fatalf("Run() error = %v, want it to name an invalid entry channel length (%d)", err, length)
		}
	}
}

// TestReducerRejectsConfigurationWithNegativeTierBDistanceInN confirms the
// reducer surfaces ConfigurationPayload.Validate's rejection of a negative
// TierBDistanceInN, naming it. (A non-finite TierBDistanceInN cannot reach
// the reducer through a JSON envelope at all — encoding.json.Marshal itself
// refuses NaN/Inf — so that case is covered directly at the payload seam,
// event.TestConfigurationPayloadValidateRejectsNonFiniteFields.)
func TestReducerRejectsConfigurationWithNegativeTierBDistanceInN(t *testing.T) {
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
	cfg.TierBDistanceInN = -1.0

	_, err = engine.Run(context.Background(), []event.Envelope{configEnvelopeWithConfig(t, 1, day(0), cfg)})
	if err == nil || !strings.Contains(err.Error(), "tier b distance in n") {
		t.Fatalf("Run() error = %v, want it to name an invalid tier b distance in n", err)
	}
}

// TestReplayingBreakoutFixtureTwiceYieldsByteIdenticalEmissions is
// TestReplayingSameFixtureTwiceYieldsByteIdenticalEmissions's counterpart for
// #9: two independent runs of the breakout fixture (which also emits a
// Signal, not just Setup-evaluated events) through two fresh Reducer
// instances must produce byte-identical emitted envelopes.
func TestReplayingBreakoutFixtureTwiceYieldsByteIdenticalEmissions(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	highs := breakoutFixtureHighs()

	runOnce := func(t *testing.T) []event.Envelope {
		t.Helper()
		return runReducerOverHighs(t, "AAPL", highs, cfg)
	}

	first := runOnce(t)
	second := runOnce(t)

	firstBytes := mustMarshal(t, first)
	secondBytes := mustMarshal(t, second)

	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatalf("replay is not deterministic:\n  first:  %s\n  second: %s", firstBytes, secondBytes)
	}
}

// --- #10: Unit sizing and the trade proposal ---

func decodeTradeProposal(t *testing.T, envelope event.Envelope) event.TradeProposalPayload {
	t.Helper()
	if envelope.Type != event.TradeProposalEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.TradeProposalEventType)
	}
	var payload event.TradeProposalPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload
}

func decodeProposalDeclined(t *testing.T, envelope event.Envelope) event.ProposalDeclinedPayload {
	t.Helper()
	if envelope.Type != event.ProposalDeclinedEventType {
		t.Fatalf("envelope.Type = %q, want %q", envelope.Type, event.ProposalDeclinedEventType)
	}
	var payload event.ProposalDeclinedPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	return payload
}

// breakoutFixtureN is N on the breakout bar of breakoutFixtureHighs, computed
// independently by referenceSetupSteps rather than read back from the
// reducer, so the expected quantities below are anchored to the same
// hand-checkable arithmetic as #8's and #9's expectations.
func breakoutFixtureN(t *testing.T, cfg event.ConfigurationPayload) float64 {
	t.Helper()
	steps := referenceSetupSteps(breakoutFixtureHighs(), indicator.DefaultPeriod, cfg.EntryChannelLength, cfg.TierBDistanceInN)
	return steps[len(steps)-1].n
}

// TestReducerEmitsTradeProposalOnSignal is #10's primary event-seam test:
// #9's breakout fixture in, and now a trade proposal out alongside the
// Signal, with the emission order Setup-evaluated -> Signal -> Proposal in
// one Apply return.
//
// Everything asserted here is hand-derivable from the fixture. The breakout
// bar's high is 200 (breakoutFixtureHighs), so the entry level is 200 — the
// level the Signal fired at; what actually fills there is #18's decision, not
// this ticket's. The N the bar is decided against is 37.57779214788228: the
// Wilder average of the fifty-five True Ranges BEFORE it, excluding the
// breakout bar's own (see
// TestReducerSizesFromNThroughThePrecedingBarNotTheSignalBar). The Baseline
// configuration is a $1,000,000 Notional Account at a 0.5 % Unit Volatility
// Fraction, so:
//
//	quantity = floor(1,000,000 x 0.005 / (37.5777... x 1))
//	         = floor(5,000 / 37.5777...) = floor(133.07...) = 133 shares
//
// Risk at Stop is derived, never configured (ADR 0003): 0.005 x 2 = 0.01, the
// declared budget. What those 133 whole shares actually risk is the realised
// figure, 133 x (2 x 37.5777...) / 1,000,000 = 0.009995..., a little under
// the budget — the gap is the truncation from 133.07 to 133. The Protective
// Stop intent is 200 - 2 x 37.5777... = 124.84...
func TestReducerEmitsTradeProposalOnSignal(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	highs := breakoutFixtureHighs()
	emitted := runReducerOverHighs(t, "AAPL", highs, cfg)

	// 55 warm-up bars emit one Setup-evaluated event each; the breakout bar
	// emits three (Setup-evaluated, Signal, Proposal).
	if len(emitted) != len(highs)+2 {
		t.Fatalf("len(emitted) = %d, want %d (55 x 1, plus the breakout bar's 3)", len(emitted), len(highs)+2)
	}

	setupIndex, signalIndex, proposalIndex := len(emitted)-3, len(emitted)-2, len(emitted)-1
	wantOrder := []string{event.SetupEvaluatedEventType, event.SignalEventType, event.TradeProposalEventType}
	for offset, want := range wantOrder {
		index := setupIndex + offset
		if emitted[index].Type != want {
			t.Fatalf("emitted[%d].Type = %q, want %q (emission order must be Setup-evaluated, Signal, Proposal)", index, emitted[index].Type, want)
		}
	}

	proposalEnvelope := emitted[proposalIndex]
	wantPeriodEnd := day(56)
	wantID := "proposal:AAPL:2026-02-27T00:00:00.000000000Z"
	if proposalEnvelope.ID != wantID {
		t.Errorf("Proposal ID = %q, want %q (deterministic and reproducible on replay)", proposalEnvelope.ID, wantID)
	}
	if proposalEnvelope.SchemaVersion != event.TradeProposalSchemaVersion {
		t.Errorf("Proposal SchemaVersion = %d, want %d", proposalEnvelope.SchemaVersion, event.TradeProposalSchemaVersion)
	}
	if proposalEnvelope.EnvelopeVersion != event.CurrentEnvelopeVersion {
		t.Errorf("Proposal EnvelopeVersion = %d, want %d", proposalEnvelope.EnvelopeVersion, event.CurrentEnvelopeVersion)
	}
	if !proposalEnvelope.EventTime.Equal(wantPeriodEnd) {
		t.Errorf("Proposal EventTime = %v, want %v (the period end, never a wall clock read)", proposalEnvelope.EventTime, wantPeriodEnd)
	}
	if !proposalEnvelope.RecordedAt.Equal(emitted[signalIndex].RecordedAt) {
		t.Errorf("Proposal RecordedAt = %v, want the input's %v", proposalEnvelope.RecordedAt, emitted[signalIndex].RecordedAt)
	}
	if proposalEnvelope.Source != "reducer" {
		t.Errorf("Proposal Source = %q, want %q", proposalEnvelope.Source, "reducer")
	}
	if proposalEnvelope.StrategyVersion != testStrategyVersion {
		t.Errorf("Proposal StrategyVersion = %q, want %q", proposalEnvelope.StrategyVersion, testStrategyVersion)
	}
	if proposalEnvelope.ConfigurationHash != testConfigurationHash {
		t.Errorf("Proposal ConfigurationHash = %q, want %q", proposalEnvelope.ConfigurationHash, testConfigurationHash)
	}

	proposal := decodeTradeProposal(t, proposalEnvelope)
	if proposal.InstrumentID != "AAPL" {
		t.Errorf("Proposal InstrumentID = %q, want AAPL", proposal.InstrumentID)
	}
	if !proposal.PeriodEnd.Equal(wantPeriodEnd) {
		t.Errorf("Proposal PeriodEnd = %v, want %v", proposal.PeriodEnd, wantPeriodEnd)
	}
	// The proposal names the Signal envelope that caused it, so a journal
	// reader can join the two without inferring anything from timing.
	if proposal.SignalID != emitted[signalIndex].ID {
		t.Errorf("Proposal SignalID = %q, want the Signal envelope's ID %q", proposal.SignalID, emitted[signalIndex].ID)
	}
	if proposal.Rule != event.RuleUnitSizingVolatilityNormalised {
		t.Errorf("Proposal Rule = %q, want %q", proposal.Rule, event.RuleUnitSizingVolatilityNormalised)
	}
	if proposal.ADR != event.ADRUnitSizing {
		t.Errorf("Proposal ADR = %q, want %q", proposal.ADR, event.ADRUnitSizing)
	}
	if proposal.Direction != event.DirectionLong {
		t.Errorf("Proposal Direction = %q, want %q", proposal.Direction, event.DirectionLong)
	}
	if proposal.SizingMode != event.SizingModeVolatilityNormalised {
		t.Errorf("Proposal SizingMode = %q, want %q", proposal.SizingMode, event.SizingModeVolatilityNormalised)
	}
	// The entry level is the breakout high — the level the Signal fired at.
	// What actually fills there is #18's concern (ADR 0013 puts slippage in
	// the fill model, not in sizing), so no slippage is applied here.
	if proposal.EntryLevel != 200 {
		t.Errorf("Proposal EntryLevel = %v, want 200 (the breakout high)", proposal.EntryLevel)
	}
	if proposal.Quantity != 133 {
		t.Errorf("Proposal Quantity = %d, want 133 (floor(5,000 / 37.5777...))", proposal.Quantity)
	}
	wantN := breakoutFixtureN(t, cfg)
	if proposal.N != wantN {
		t.Errorf("Proposal N = %v, want %v", proposal.N, wantN)
	}
	if proposal.UnitVolatilityFraction != cfg.UnitVolatilityFraction {
		t.Errorf("Proposal UnitVolatilityFraction = %v, want %v", proposal.UnitVolatilityFraction, cfg.UnitVolatilityFraction)
	}
	if proposal.StopMultiple != cfg.StopMultiple {
		t.Errorf("Proposal StopMultiple = %v, want %v", proposal.StopMultiple, cfg.StopMultiple)
	}
	if proposal.DollarsPerPoint != cfg.DollarsPerPoint {
		t.Errorf("Proposal DollarsPerPoint = %v, want %v", proposal.DollarsPerPoint, cfg.DollarsPerPoint)
	}
	// The Notional Account used this ticket is the configured starting
	// equity. Drawdown Steps and yearly re-basing (ADR 0007) are #16/#17.
	if proposal.NotionalAccount != cfg.NotionalAccount.StartingEquity {
		t.Errorf("Proposal NotionalAccount = %v, want the configured starting equity %v", proposal.NotionalAccount, cfg.NotionalAccount.StartingEquity)
	}
	// Exact equality is deliberate: the journal's Risk at Stop must be the
	// identical float64 the derivation produces (ADR 0003).
	if proposal.RiskAtStop != cfg.UnitVolatilityFraction*cfg.StopMultiple {
		t.Errorf("Proposal RiskAtStop = %v, want exactly %v (0.005 x 2)", proposal.RiskAtStop, cfg.UnitVolatilityFraction*cfg.StopMultiple)
	}
	// The declared budget above is what the strategy set out to risk; the
	// realised figure below is what the whole-share quantity actually risks,
	// and the gap between them is the truncation (PR #64 review).
	wantRealised := 133 * (cfg.StopMultiple * wantN * cfg.DollarsPerPoint) / cfg.NotionalAccount.StartingEquity
	if proposal.RealisedRiskAtStop != wantRealised {
		t.Errorf("Proposal RealisedRiskAtStop = %v, want exactly %v", proposal.RealisedRiskAtStop, wantRealised)
	}
	if !(proposal.RealisedRiskAtStop < proposal.RiskAtStop) {
		t.Errorf("Proposal RealisedRiskAtStop %v is not below the declared RiskAtStop %v; 133.07 shares truncated to 133 must leave a gap",
			proposal.RealisedRiskAtStop, proposal.RiskAtStop)
	}
	if proposal.ProtectiveStopIntent != 200-cfg.StopMultiple*wantN {
		t.Errorf("Proposal ProtectiveStopIntent = %v, want exactly %v (entry - 2N)", proposal.ProtectiveStopIntent, 200-cfg.StopMultiple*wantN)
	}
	if err := proposal.Validate(); err != nil {
		t.Errorf("emitted proposal fails its own Validate(): %v", err)
	}
}

// TestReducerEmitsFixedRiskAtStopProposal runs the same fixture under the
// Sublime Variant's Sizing Mode (ADR 0003; [M p.56]). Risk at Stop is 2 % —
// the configured input, not a derivation — and the 3N stop makes the
// quantity 1,000,000 x 0.02 / (3 x 37.5777...) = 177.42... -> 177, a
// different number from the Baseline's 133 on the identical bar. That
// difference is the whole reason the Sizing Mode is explicit.
func TestReducerEmitsFixedRiskAtStopProposal(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	cfg.SizingMode = event.SizingModeFixedRiskAtStop
	cfg.StopMultiple = 3
	cfg.RiskAtStopFraction = 0.02

	emitted := runReducerOverHighs(t, "AAPL", breakoutFixtureHighs(), cfg)

	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("got %d proposal(s), want exactly 1", len(proposals))
	}

	proposal := decodeTradeProposal(t, proposals[0])
	if proposal.Quantity != 177 {
		t.Errorf("Proposal Quantity = %d, want 177 (floor(20,000 / (3 x 37.5777...)))", proposal.Quantity)
	}
	if proposal.SizingMode != event.SizingModeFixedRiskAtStop {
		t.Errorf("Proposal SizingMode = %q, want %q", proposal.SizingMode, event.SizingModeFixedRiskAtStop)
	}
	if proposal.Rule != event.RuleUnitSizingFixedRiskAtStop {
		t.Errorf("Proposal Rule = %q, want %q", proposal.Rule, event.RuleUnitSizingFixedRiskAtStop)
	}
	if proposal.RiskAtStop != 0.02 {
		t.Errorf("Proposal RiskAtStop = %v, want exactly the configured 0.02", proposal.RiskAtStop)
	}
	wantN := breakoutFixtureN(t, cfg)
	// The realised-below-declared invariant holds in this mode too, where
	// the declared figure is the configured input rather than a derivation.
	wantRealised := 177 * (3 * wantN * cfg.DollarsPerPoint) / cfg.NotionalAccount.StartingEquity
	if proposal.RealisedRiskAtStop != wantRealised {
		t.Errorf("Proposal RealisedRiskAtStop = %v, want exactly %v", proposal.RealisedRiskAtStop, wantRealised)
	}
	if !(proposal.RealisedRiskAtStop < proposal.RiskAtStop) {
		t.Errorf("Proposal RealisedRiskAtStop %v is not below the declared %v", proposal.RealisedRiskAtStop, proposal.RiskAtStop)
	}
	if proposal.ProtectiveStopIntent != 200-3*wantN {
		t.Errorf("Proposal ProtectiveStopIntent = %v, want exactly %v (entry - 3N)", proposal.ProtectiveStopIntent, 200-3*wantN)
	}
	if err := proposal.Validate(); err != nil {
		t.Errorf("emitted proposal fails its own Validate(): %v", err)
	}
}

// TestReducerDeclinesWhenTheAccountIsTooSmallForOneShare is the fail-closed
// case the ticket names: the Signal still fires (the strategy did recognise
// its entry condition), but 100 x 0.005 = $0.50 of 1N budget cannot buy one
// share at N = 40.6989..., so the quantity truncates to zero. No proposal is
// emitted, and a strategy.proposal.declined event records why — a silent drop
// would leave the journal unable to distinguish "no Signal" from "a Signal
// that produced nothing" (The Turtle Rules p.15 notes small accounts lose
// diversification exactly because truncation is coarse).
func TestReducerDeclinesWhenTheAccountIsTooSmallForOneShare(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	cfg.NotionalAccount.StartingEquity = 100

	highs := breakoutFixtureHighs()
	emitted := runReducerOverHighs(t, "AAPL", highs, cfg)

	if len(emitted) != len(highs)+2 {
		t.Fatalf("len(emitted) = %d, want %d (55 x 1, plus the breakout bar's Setup-evaluated, Signal and decline)", len(emitted), len(highs)+2)
	}
	if got := len(envelopesOfType(emitted, event.SignalEventType)); got != 1 {
		t.Fatalf("got %d Signal(s), want exactly 1: sizing declines the trade, it does not suppress the Signal", got)
	}
	if got := len(envelopesOfType(emitted, event.TradeProposalEventType)); got != 0 {
		t.Fatalf("got %d proposal(s), want 0", got)
	}

	declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1", len(declines))
	}
	declineEnvelope := declines[0]
	wantID := "proposal-declined:AAPL:2026-02-27T00:00:00.000000000Z"
	if declineEnvelope.ID != wantID {
		t.Errorf("Decline ID = %q, want %q", declineEnvelope.ID, wantID)
	}
	if declineEnvelope.SchemaVersion != event.ProposalDeclinedSchemaVersion {
		t.Errorf("Decline SchemaVersion = %d, want %d", declineEnvelope.SchemaVersion, event.ProposalDeclinedSchemaVersion)
	}
	if !declineEnvelope.EventTime.Equal(day(56)) {
		t.Errorf("Decline EventTime = %v, want %v", declineEnvelope.EventTime, day(56))
	}
	// The decline is the third emission of the bar, in the Signal's place in
	// the ordering: Setup-evaluated, Signal, then the sizing outcome.
	if emitted[len(emitted)-1].Type != event.ProposalDeclinedEventType {
		t.Errorf("last emission = %q, want the decline", emitted[len(emitted)-1].Type)
	}

	decline := decodeProposalDeclined(t, declineEnvelope)
	if decline.InstrumentID != "AAPL" {
		t.Errorf("Decline InstrumentID = %q, want AAPL", decline.InstrumentID)
	}
	if decline.SignalID != envelopesOfType(emitted, event.SignalEventType)[0].ID {
		t.Errorf("Decline SignalID = %q, want the Signal envelope's ID", decline.SignalID)
	}
	if decline.Reason != event.DeclineReasonQuantityBelowOneUnit {
		t.Errorf("Decline Reason = %q, want %q", decline.Reason, event.DeclineReasonQuantityBelowOneUnit)
	}
	if decline.Detail == "" {
		t.Error("Decline Detail is empty; the journal must record the numbers that produced the decline")
	}
	if err := decline.Validate(); err != nil {
		t.Errorf("emitted decline fails its own Validate(): %v", err)
	}
}

// TestReducerDeclinesWhenTheProtectiveStopIntentIsNotPositive covers the
// other fail-closed case: a Stop Multiple of 6 against N = 37.5777... puts
// the Protective Stop at 200 - 225.46... = -25.46, below zero. A long equity
// cannot fall below zero, so that stop is unreachable and the Unit would in
// fact risk the entire position rather than the derived fraction. The
// quantity is positive here (133 shares), so this is a genuinely separate
// decline from the too-small-account case, and it must be journaled rather
// than either proposed or silently dropped.
func TestReducerDeclinesWhenTheProtectiveStopIntentIsNotPositive(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	cfg.StopMultiple = 6

	emitted := runReducerOverHighs(t, "AAPL", breakoutFixtureHighs(), cfg)

	if got := len(envelopesOfType(emitted, event.TradeProposalEventType)); got != 0 {
		t.Fatalf("got %d proposal(s), want 0", got)
	}
	declines := envelopesOfType(emitted, event.ProposalDeclinedEventType)
	if len(declines) != 1 {
		t.Fatalf("got %d decline(s), want exactly 1", len(declines))
	}
	decline := decodeProposalDeclined(t, declines[0])
	if decline.Reason != event.DeclineReasonStopIntentNotPositive {
		t.Fatalf("Decline Reason = %q, want %q", decline.Reason, event.DeclineReasonStopIntentNotPositive)
	}
}

// TestReducerRejectsVolatilityNormalisedConfigurationThatConfiguresRiskAtStop
// is the ticket's named negative test, at the event seam: Risk at Stop must
// not be directly configurable in the Baseline mode (ADR 0003 — it is
// derived, never configured). A configuration that sets both
// SizingMode: volatility-normalised and a risk-at-stop fraction is rejected
// at configuration time, before any bar can be sized from it, rather than
// being quietly ignored — quietly ignoring it is exactly how a run comes to
// believe it is risking a figure that nothing in the arithmetic honours.
func TestReducerRejectsVolatilityNormalisedConfigurationThatConfiguresRiskAtStop(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	cfg.RiskAtStopFraction = 0.01 // even the "correct" 0.005 x 2 must be rejected

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	_, err = engine.Run(context.Background(), []event.Envelope{configEnvelopeWithConfig(t, 1, day(0), cfg)})
	if err == nil {
		t.Fatal("Run() error = nil, want a rejection of a volatility-normalised configuration that configures Risk at Stop")
	}
	if !strings.Contains(err.Error(), "risk at stop") {
		t.Fatalf("Run() error = %v, want it to name the risk at stop fraction", err)
	}
}

// TestReducerRejectsConfigurationWithTheSupersededSchemaVersion pins #10's
// schema bump at the reducer: a configuration recorded under schema 2 (before
// DollarsPerPoint and RiskAtStopFraction existed) must be rejected outright,
// not decoded with both fields defaulting to the float64 zero. A zero
// DollarsPerPoint would divide by zero, and a zero RiskAtStopFraction would
// make a fixed-risk-at-stop run size every Unit from a risk budget of
// nothing — both silent, both capital-safety defects. ADR 0015's rule: an
// older schema is rejected, never silently upgraded, until an explicit
// upcaster exists.
func TestReducerRejectsConfigurationWithTheSupersededSchemaVersion(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	envelope := configEnvelope(t, 1, day(0))
	envelope.SchemaVersion = 2

	_, err = engine.Run(context.Background(), []event.Envelope{envelope})
	if err == nil {
		t.Fatal("Run() error = nil, want a rejection of a schema-2 configuration")
	}
	if !strings.Contains(err.Error(), "schema version") {
		t.Fatalf("Run() error = %v, want it to name the schema version", err)
	}
}

// TestReplayingProposalFixtureTwiceYieldsByteIdenticalEmissions extends #8's
// replay-equivalence property to the two new emissions: two independent runs
// of the proposal fixture through two fresh Reducer instances must produce
// byte-identical envelopes, proving the sizing step introduces no wall-clock
// read, no randomness and no map-iteration-order dependence.
func TestReplayingProposalFixtureTwiceYieldsByteIdenticalEmissions(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	first := runReducerOverHighs(t, "AAPL", breakoutFixtureHighs(), cfg)
	second := runReducerOverHighs(t, "AAPL", breakoutFixtureHighs(), cfg)

	if len(envelopesOfType(first, event.TradeProposalEventType)) != 1 {
		t.Fatal("fixture must emit exactly one proposal for this property to mean anything")
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

// TestSizingModeConstantsMatchTheEventContract pins the one seam where the
// two enumerations could drift. internal/sizing is a pure arithmetic package
// that deliberately imports nothing from internal/event (the same rule
// internal/indicator follows), so it declares its own Mode; the reducer maps
// event.SizingMode onto it with an explicit fail-closed switch. This test
// makes the string values equal by assertion, so a rename on either side is a
// test failure rather than a run that silently falls through to the
// unrecognised-mode branch.
func TestSizingModeConstantsMatchTheEventContract(t *testing.T) {
	t.Parallel()

	if string(event.SizingModeVolatilityNormalised) != string(sizing.ModeVolatilityNormalised) {
		t.Errorf("event.SizingModeVolatilityNormalised = %q, sizing.ModeVolatilityNormalised = %q", event.SizingModeVolatilityNormalised, sizing.ModeVolatilityNormalised)
	}
	if string(event.SizingModeFixedRiskAtStop) != string(sizing.ModeFixedRiskAtStop) {
		t.Errorf("event.SizingModeFixedRiskAtStop = %q, sizing.ModeFixedRiskAtStop = %q", event.SizingModeFixedRiskAtStop, sizing.ModeFixedRiskAtStop)
	}
}

// envelopesOfType filters emissions by event type, the shape several tests
// above repeat by hand.
func envelopesOfType(envelopes []event.Envelope, eventType string) []event.Envelope {
	var matched []event.Envelope
	for _, e := range envelopes {
		if e.Type == eventType {
			matched = append(matched, e)
		}
	}
	return matched
}

// --- PR #64 review: the decision bar is never an input to its own decision ---

// runReducerOverBars replays a configuration event followed by the given
// bars, in order, and returns every envelope the engine emitted. Unlike
// runReducerOverHighs it lets a caller set a bar's low and close
// independently of its high, which is what the two tests below need: they
// vary a bar's True Range while holding its high — and therefore the
// breakout level and the entry level — fixed.
func runReducerOverBars(t *testing.T, cfg event.ConfigurationPayload, bars []event.CompletedBarPayload) []event.Envelope {
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
	for _, bar := range bars {
		envelopes = append(envelopes, barEnvelope(t, seq, bar, bar.PeriodEnd))
		seq++
	}

	emitted, err := engine.Run(context.Background(), envelopes)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	return emitted
}

// breakoutBarsWithFinalRange builds #9's breakout fixture with the breakout
// bar's True Range under the caller's control while its HIGH stays fixed at
// 200.
//
// Bars 1..55 are the usual warm-up (high 100+i, low and close 100, so True
// Range is exactly i). Bar 56's high is 200 in every case, so the breakout,
// the Entry Channel it clears (155) and the entry level are identical no
// matter what is passed; only its low, and therefore its True Range, changes.
// The previous close is always 100, so the gap term (high - previous close)
// is 100 and the bar's True Range is max(200-low, 100, 100-low) — a low of
// 199 gives 100, a low of 1 gives 199.
func breakoutBarsWithFinalRange(instrumentID string, finalLow, finalClose float64) []event.CompletedBarPayload {
	bars := make([]event.CompletedBarPayload, 0, 56)
	for i := 1; i <= 55; i++ {
		bars = append(bars, completedBar(instrumentID, day(i), 100+float64(i), 100, 100))
	}
	return append(bars, completedBar(instrumentID, day(56), 200, finalLow, finalClose))
}

// TestReducerSizesFromNThroughThePrecedingBarNotTheSignalBar is the headline
// invariant this review round exists to pin (Greptile P1 on PR #64, and the
// same class of defect as the prototype's look-ahead Donchian read).
//
// Two fixtures differ in exactly one respect: the breakout bar's True Range.
// One is a narrow bar (True Range 100, driven entirely by the gap from the
// previous close), the other an extremely wide one (True Range 199). Their
// highs — and so the breakout, the Entry Channel level cleared, and the entry
// level — are identical.
//
// The Unit size and the Protective Stop intent must be IDENTICAL across the
// two. Under ADR 0005 the entry is a resting order that fills *inside* the
// breakout bar, so the quantity and the stop have to be computable before
// that bar opens, from N through the previous completed bar. A bar that
// changed its own N would shrink its own Unit and tighten its own stop by
// having been volatile — deciding with information that did not exist when
// the order was placed. CONTEXT.md's "Completed bar" states the rule
// generally: the decision bar is never an input to its own decision.
func TestReducerSizesFromNThroughThePrecedingBarNotTheSignalBar(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()

	narrow := decodeTradeProposal(t, onlyProposal(t, runReducerOverBars(t, cfg, breakoutBarsWithFinalRange("AAPL", 199, 199.5))))
	wide := decodeTradeProposal(t, onlyProposal(t, runReducerOverBars(t, cfg, breakoutBarsWithFinalRange("AAPL", 1, 150))))

	if narrow.EntryLevel != wide.EntryLevel {
		t.Fatalf("the two fixtures must differ only in the breakout bar's range: entry levels %v and %v", narrow.EntryLevel, wide.EntryLevel)
	}
	if narrow.N != wide.N {
		t.Errorf("N = %v (narrow breakout bar) and %v (wide breakout bar); the bar being decided on must not change the N it is decided against", narrow.N, wide.N)
	}
	if narrow.Quantity != wide.Quantity {
		t.Errorf("Quantity = %d (narrow breakout bar) and %d (wide breakout bar); a bar must not shrink its own Unit by having been volatile", narrow.Quantity, wide.Quantity)
	}
	if narrow.ProtectiveStopIntent != wide.ProtectiveStopIntent {
		t.Errorf("ProtectiveStopIntent = %v (narrow) and %v (wide); a bar must not tighten its own stop by having been volatile", narrow.ProtectiveStopIntent, wide.ProtectiveStopIntent)
	}

	// And both agree with the Wilder average of the fifty-five PRECEDING
	// True Ranges (1..55) — the same value breakoutFixtureN computes
	// independently — rather than with anything the breakout bar contributed.
	wantN := breakoutFixtureN(t, cfg)
	if narrow.N != wantN {
		t.Errorf("N = %v, want %v (the Wilder average of bars 1..55 only)", narrow.N, wantN)
	}
	if narrow.Quantity != 133 {
		t.Errorf("Quantity = %d, want 133 (floor(5,000 / 37.5777...))", narrow.Quantity)
	}
}

// TestAddThenEvaluateNWouldShrinkTheWideBarsOwnUnit is the negative half,
// written in the same style as #9's TestLookAheadEntryChannelWouldMissTheBreakout:
// it reproduces the rejected implementation locally, from scratch, and shows
// it produces a materially different answer on the same fixture. It calls
// nothing in internal/strategy or internal/indicator, so it cannot pass by
// accidentally exercising the production code twice.
//
// The bug is one line of ordering: fold the bar's True Range into N and then
// read N, rather than reading N and then folding. On the wide-bar fixture
// that inflates N from 37.57... to 45.65..., which cuts the Unit from 133
// shares to 109 and pulls the Protective Stop 16 points closer — all of it
// caused by the bar the order was already resting inside.
func TestAddThenEvaluateNWouldShrinkTheWideBarsOwnUnit(t *testing.T) {
	t.Parallel()

	const (
		period          = 20
		notionalAccount = 1_000_000.0
		fraction        = 0.005
		stopMultiple    = 2.0
		entryLevel      = 200.0
	)

	// True Ranges of the fixture: bars 1..55 are 1..55, and the wide
	// breakout bar is 199 (high 200, low 1, previous close 100).
	trueRanges := make([]float64, 0, 56)
	for i := 1; i <= 55; i++ {
		trueRanges = append(trueRanges, float64(i))
	}
	trueRanges = append(trueRanges, 199)

	// wilder replays the recursion over the first count True Ranges,
	// seeded with the simple average of the first period of them.
	wilder := func(count int) float64 {
		var sum float64
		for i := 0; i < period; i++ {
			sum += trueRanges[i]
		}
		n := sum / period
		for i := period; i < count; i++ {
			n = (float64(period-1)*n + trueRanges[i]) / period
		}
		return n
	}
	quantity := func(n float64) int64 {
		return int64(math.Floor(notionalAccount * fraction / n))
	}

	evaluateThenAdd := wilder(55) // N through bar 55: what the reducer must use
	addThenEvaluate := wilder(56) // N including the bar being decided: the bug

	if addThenEvaluate <= evaluateThenAdd {
		t.Fatalf("fixture no longer exercises the defect: add-then-evaluate N %v is not above evaluate-then-add N %v", addThenEvaluate, evaluateThenAdd)
	}
	correctQuantity, buggyQuantity := quantity(evaluateThenAdd), quantity(addThenEvaluate)
	if buggyQuantity >= correctQuantity {
		t.Fatalf("add-then-evaluate quantity %d is not below the correct %d; the fixture must make the two outcomes genuinely opposite", buggyQuantity, correctQuantity)
	}

	// The production reducer, on the identical fixture, must produce the
	// correct one — which is what makes this a regression guard rather than a
	// restatement of the test author's mental model.
	cfg := validConfigurationPayload()
	proposal := decodeTradeProposal(t, onlyProposal(t, runReducerOverBars(t, cfg, breakoutBarsWithFinalRange("AAPL", 1, 150))))
	if proposal.Quantity != correctQuantity {
		t.Fatalf("reducer Quantity = %d, want %d (the add-then-evaluate implementation would give %d)", proposal.Quantity, correctQuantity, buggyQuantity)
	}
	if diff := math.Abs(proposal.N - evaluateThenAdd); diff > epsilon {
		t.Fatalf("reducer N = %v, want %v (diff %v)", proposal.N, evaluateThenAdd, diff)
	}
}

// onlyProposal returns the single trade proposal among the emitted
// envelopes, failing the test if there is not exactly one.
func onlyProposal(t *testing.T, emitted []event.Envelope) event.Envelope {
	t.Helper()
	proposals := envelopesOfType(emitted, event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("got %d trade proposal(s), want exactly 1", len(proposals))
	}
	return proposals[0]
}
