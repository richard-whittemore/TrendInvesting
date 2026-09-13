package replay_test

// These tests exercise replay equivalence's three underlying properties
// against a real internal/strategy.Reducer, the concrete Handler it is built
// to protect: replay-twice identity, arrival-time independence, and that a
// Signal never survives its own bar. They live here rather than in
// internal/strategy because they are about what replay.Engine plus a
// Handler guarantee, not about any one strategy rule, and internal/strategy
// has no reason to import internal/replay's test helpers back. Feeding
// events straight to a fresh Reducer, with no fill simulator in the loop,
// is deliberate: a journal's inputs include fills the simulator produced,
// so this is the reducer's own determinism, which is what replay
// equivalence actually tests (the simulator's determinism is a separate,
// stronger property, exercised end to end in cmd/backtest against the
// committed golden journal).

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

const propertyFixtureStrategyVersion = "replay-fixture/1.1.0+test"

// propertyFixtureConfiguration is a small, valid Baseline-shaped
// configuration: a 20-bar Entry Channel, matching indicator.DefaultPeriod so
// N and the channel warm up together in the fewest bars.
func propertyFixtureConfiguration() event.ConfigurationPayload {
	return event.ConfigurationPayload{
		StrategyID:             "replay-fixture",
		SizingMode:             event.SizingModeVolatilityNormalised,
		UnitVolatilityFraction: 0.005,
		StopMultiple:           2,
		EntryChannelLength:     20,
		ExitChannelLength:      10,
		MaxUnits:               4,
		SlippageN:              0.05,
		TierBDistanceInN:       1,
		DollarsPerPoint:        1,
		NotionalAccount: event.NotionalAccountConfig{
			StartingEquity: 1_000_000,
			RebasingMonth:  1,
			RebasingDay:    1,
		},
		Commission: event.CommissionConfig{
			PerShare:                    0.005,
			MinimumPerOrder:             1,
			MaximumFractionOfTradeValue: 0.01,
		},
	}
}

func propertyDay(n int) time.Time {
	return time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n)
}

func mustMarshalT(t *testing.T, v any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%v) error = %v", v, err)
	}
	return encoded
}

func propertyConfigEnvelope(t *testing.T, cfg event.ConfigurationPayload, at, recordedAt time.Time) event.Envelope {
	t.Helper()
	payload := mustMarshalT(t, cfg)
	return event.Envelope{
		ID:                "cfg-1",
		Type:              event.ConfigurationEventType,
		SchemaVersion:     event.ConfigurationSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         at,
		RecordedAt:        recordedAt,
		Sequence:          1,
		Source:            "fixture",
		StrategyVersion:   propertyFixtureStrategyVersion,
		ConfigurationHash: event.ConfigurationHash(cfg),
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// flatBar builds a bar whose split-adjusted and raw views are identical,
// with Open and Close both at the midpoint of High and Low so the
// cross-field OHLC checks are satisfied trivially.
func flatBar(instrumentID string, periodEnd time.Time, high, low float64) event.CompletedBarPayload {
	mid := (high + low) / 2
	view := event.PriceView{Open: mid, High: high, Low: low, Close: mid, Volume: 1_000_000}
	split, raw := view, view
	split.View, raw.View = event.ViewSplitAdjusted, event.ViewRaw
	return event.CompletedBarPayload{InstrumentID: instrumentID, PeriodEnd: periodEnd, SplitAdjusted: split, Raw: raw}
}

func propertyBarEnvelope(t *testing.T, sequence uint64, bar event.CompletedBarPayload, cfg event.ConfigurationPayload, recordedAt time.Time) event.Envelope {
	t.Helper()
	payload := mustMarshalT(t, bar)
	return event.Envelope{
		ID:                fmt.Sprintf("bar-%d", sequence),
		Type:              event.CompletedBarEventType,
		SchemaVersion:     event.CompletedBarSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         bar.PeriodEnd,
		RecordedAt:        recordedAt,
		Sequence:          sequence,
		Source:            "fixture",
		StrategyVersion:   propertyFixtureStrategyVersion,
		ConfigurationHash: event.ConfigurationHash(cfg),
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// signalNeverSurvivesFixture builds 20 warm-up bars (a rising high each day,
// so True Range is non-zero and the Entry Channel is unambiguous), a bar 21
// breakout that raises a Signal and a trade proposal, and a bar 22 that does
// NOT confirm it — its high stays below the channel as bar 21 widened it —
// so the proposal is superseded rather than filled or renewed. recordedAt
// computes each envelope's RecordedAt from its EventTime, so a caller can
// shift it independently of EventTime to test arrival-time independence.
func signalNeverSurvivesFixture(t *testing.T, recordedAt func(eventTime time.Time) time.Time) (cfg event.ConfigurationPayload, envelopes []event.Envelope) {
	t.Helper()
	cfg = propertyFixtureConfiguration()
	envelopes = []event.Envelope{propertyConfigEnvelope(t, cfg, propertyDay(0), recordedAt(propertyDay(0)))}

	seq := uint64(2)
	for i := 0; i < 20; i++ {
		high := 100 + float64(i+1) // 101..120: channel high after warm-up is 120
		bar := flatBar("AAPL", propertyDay(i+1), high, high-2)
		envelopes = append(envelopes, propertyBarEnvelope(t, seq, bar, cfg, recordedAt(bar.PeriodEnd)))
		seq++
	}

	// Bar 21: high 130 exceeds the warmed-up channel high of 120 — a
	// breakout, a Signal, and (this fixture's prices and starting equity
	// size at least one whole Unit) a trade proposal.
	breakout := flatBar("AAPL", propertyDay(21), 130, 128)
	envelopes = append(envelopes, propertyBarEnvelope(t, seq, breakout, cfg, recordedAt(breakout.PeriodEnd)))
	seq++

	// Bar 22: high 125 does not exceed the channel high, now 130 (bars 2-21)
	// since bar 21 entered the window — not a breakout, so the proposal bar
	// 21 raised is superseded rather than confirmed. No fill is ever
	// delivered, matching this ticket's scope: replay equivalence tests the
	// reducer's own decisions, not whether the fill simulator would have
	// filled this proposal.
	quiet := flatBar("AAPL", propertyDay(22), 125, 123)
	envelopes = append(envelopes, propertyBarEnvelope(t, seq, quiet, cfg, recordedAt(quiet.PeriodEnd)))

	return cfg, envelopes
}

func runFixture(t *testing.T, cfg event.ConfigurationPayload, inputs []event.Envelope) []event.Envelope {
	t.Helper()
	reducer, err := strategy.NewReducer(propertyFixtureStrategyVersion, cfg)
	if err != nil {
		t.Fatalf("strategy.NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}
	emitted, err := engine.Run(context.Background(), inputs)
	if err != nil {
		t.Fatalf("Engine.Run() error = %v", err)
	}
	return emitted
}

// TestReplayTwiceIsByteIdentical: the headline property. Two freshly
// constructed reducers, given the same input stream, must emit decisions
// that are byte-identical in order — a journal's evidentiary value depends
// on there being no other possible outcome from the same inputs.
func TestReplayTwiceIsByteIdentical(t *testing.T) {
	t.Parallel()

	cfg, inputs := signalNeverSurvivesFixture(t, func(eventTime time.Time) time.Time { return eventTime })

	first := runFixture(t, cfg, inputs)
	second := runFixture(t, cfg, inputs)

	if d := replay.Equivalent(first, second); d != nil {
		t.Fatalf("two replays of the same inputs diverged at %d:\n want %+v\n got  %+v", d.Index, d.Want, d.Got)
	}
	if len(first) == 0 {
		t.Fatal("the fixture produced no decisions at all; this test would pass vacuously")
	}
}

// TestArrivalTimeIndependence is the test that catches a wall-clock leak: an
// input stream fed with a different, but still valid, RecordedAt on every
// envelope must produce decisions identical to the original run in every
// field EXCEPT RecordedAt itself — which the reducer deliberately copies
// through from the causing input (internal/strategy.Reducer.stamp) — so a
// leak would show up as a difference somewhere else: a Tier, a price, a
// quantity computed from wall-clock arrival rather than from the ordered
// event stream.
func TestArrivalTimeIndependence(t *testing.T) {
	t.Parallel()

	const shift = 37 * time.Minute

	cfg, original := signalNeverSurvivesFixture(t, func(eventTime time.Time) time.Time { return eventTime })
	_, shifted := signalNeverSurvivesFixture(t, func(eventTime time.Time) time.Time { return eventTime.Add(shift) })

	fromOriginal := runFixture(t, cfg, original)
	fromShifted := runFixture(t, cfg, shifted)

	if len(fromOriginal) != len(fromShifted) {
		t.Fatalf("got %d decision(s) from the original arrival times and %d from the shifted ones", len(fromOriginal), len(fromShifted))
	}
	if len(fromOriginal) == 0 {
		t.Fatal("the fixture produced no decisions at all; this test would pass vacuously")
	}

	shiftedByID := make(map[string]time.Time, len(shifted))
	for _, in := range shifted {
		shiftedByID[in.ID] = in.RecordedAt
	}

	for i := range fromOriginal {
		want, got := fromOriginal[i], fromShifted[i]

		// RecordedAt is expected to differ: it tracks the (now shifted)
		// causing input, never a wall clock of the reducer's own. Confirm it
		// really did track the shift, so clearing it below is not
		// vacuously comparing two zero values.
		wantRecordedAt, ok := shiftedByID[got.CausationID]
		if !ok {
			t.Fatalf("decision %d (%s) names causation %q, which is not one of the shifted inputs", i, got.ID, got.CausationID)
		}
		if !got.RecordedAt.Equal(wantRecordedAt) {
			t.Fatalf("decision %d (%s) RecordedAt = %s, want the shifted input's own %s", i, got.ID, got.RecordedAt, wantRecordedAt)
		}
		if got.RecordedAt.Equal(want.RecordedAt) {
			t.Fatalf("decision %d (%s) RecordedAt did not change with the shifted input; this test would not catch a leak that ignored RecordedAt entirely", i, got.ID)
		}

		// Every other field must be untouched by the shift.
		want.RecordedAt, got.RecordedAt = time.Time{}, time.Time{}
		if d := replay.Equivalent([]event.Envelope{want}, []event.Envelope{got}); d != nil {
			t.Fatalf("decision %d diverged on something other than RecordedAt:\n want %+v\n got  %+v", i, d.Want, d.Got)
		}
	}
}

// TestASignalNeverSurvivesItsBar: bar 21's Signal and the proposal it caused
// belong to bar 21 alone. Bar 22 does not renew, repeat, or extend it — it
// only records that the proposal is now dead, dated to the bar it was
// raised on (PeriodEnd) even though the supersession is only observable once
// bar 22 arrives (ExpiredAt) — CONTEXT.md: "Signal", ADR 0011.
func TestASignalNeverSurvivesItsBar(t *testing.T) {
	t.Parallel()

	cfg, inputs := signalNeverSurvivesFixture(t, func(eventTime time.Time) time.Time { return eventTime })
	emitted := runFixture(t, cfg, inputs)

	var signals []event.Envelope
	var expiries []event.ProposalExpiredPayload
	for _, e := range emitted {
		switch e.Type {
		case event.SignalEventType:
			signals = append(signals, e)
		case event.ProposalExpiredEventType:
			var payload event.ProposalExpiredPayload
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				t.Fatalf("decode %s: %v", e.ID, err)
			}
			expiries = append(expiries, payload)
		}
	}

	if len(signals) != 1 {
		t.Fatalf("got %d Signal(s), want exactly 1 (bar 22 must not repeat or renew bar 21's)", len(signals))
	}
	if !signals[0].EventTime.Equal(propertyDay(21)) {
		t.Fatalf("the Signal's EventTime is %s, want bar 21 (%s)", signals[0].EventTime, propertyDay(21))
	}

	if len(expiries) != 1 {
		t.Fatalf("got %d expiry(ies), want exactly 1 (bar 21's unfilled proposal, superseded by bar 22)", len(expiries))
	}
	expiry := expiries[0]
	if expiry.Reason != event.ExpiryReasonSupersededByNextBar {
		t.Fatalf("expiry reason = %q, want %q", expiry.Reason, event.ExpiryReasonSupersededByNextBar)
	}
	// PeriodEnd names the bar the expired proposal BELONGED to — bar 21,
	// where the Signal fired — never bar 22, which only supersedes it. A
	// Signal "surviving its bar" would look exactly like this field naming
	// the wrong bar.
	if !expiry.PeriodEnd.Equal(propertyDay(21)) {
		t.Fatalf("expiry PeriodEnd = %s, want bar 21 (%s): the proposal belongs to the bar that raised it, not the bar that superseded it", expiry.PeriodEnd, propertyDay(21))
	}
	if !expiry.ExpiredAt.Equal(propertyDay(22)) {
		t.Fatalf("expiry ExpiredAt = %s, want bar 22 (%s): supersession is only observable once the next bar arrives", expiry.ExpiredAt, propertyDay(22))
	}
}
