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
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
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

	wantReadyFrom := 20 // 1-indexed bar count at which N becomes ready
	wantNAfterSeed := []float64{
		10.5,          // bar 20
		10.225,        // bar 21
		9.96375,       // bar 22
		9.7155625,     // bar 23
		9.479784375,   // bar 24
		9.25579515625, // bar 25
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
	for i := 0; i < 20; i++ {
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
	if len(emitted) != 20 {
		t.Fatalf("len(emitted) = %d, want 20", len(emitted))
	}

	last := decodeSetupEvaluated(t, emitted[len(emitted)-1])
	if !last.NReady {
		t.Fatal("NReady = false after 20 bars, want true")
	}
	if diff := math.Abs(last.N - 1.0); diff > epsilon {
		t.Fatalf("N = %v, want ~1.0 (split-adjusted True Range); got a value near the raw view's True Range would indicate ADR 0004 is violated", last.N)
	}
}

// TestReducerKeepsSeparateStatePerInstrument warms AAPL up to Ready over 20
// bars, then feeds a single MSFT bar. If the two instruments shared one
// True Range/N tracker, that single MSFT bar would land as the tracker's
// 21st input and read as Ready; kept separate, it must be MSFT's first bar
// and read as not ready.
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
	for i := 0; i < 20; i++ {
		bar := syntheticBar("AAPL", day(i+1), 1.0)
		envelopes = append(envelopes, barEnvelope(t, seq, bar, day(i+1)))
		seq++
	}
	msftBar := syntheticBar("MSFT", day(21), 1.0)
	envelopes = append(envelopes, barEnvelope(t, seq, msftBar, day(21)))

	emitted, err := engine.Run(context.Background(), envelopes)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(emitted) != 21 {
		t.Fatalf("len(emitted) = %d, want 21", len(emitted))
	}

	aaplLast := decodeSetupEvaluated(t, emitted[19])
	if aaplLast.InstrumentID != "AAPL" || !aaplLast.NReady {
		t.Fatalf("AAPL after 20 bars: InstrumentID=%q NReady=%v, want AAPL, true", aaplLast.InstrumentID, aaplLast.NReady)
	}

	msftFirst := decodeSetupEvaluated(t, emitted[20])
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
