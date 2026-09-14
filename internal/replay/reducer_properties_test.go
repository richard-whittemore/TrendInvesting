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

// propertyAvailableCash is the available-cash figure this file's fixture
// supplies: comfortably clear of any Unit its prices and starting equity can
// size, since these are replay-equivalence properties rather than
// affordability tests (the cash-skip rule's own tests live in
// internal/strategy).
const propertyAvailableCash = 1_000_000_000.0

// propertySnapshotEnvelope is ADR 0010's cash basis, delivered once before
// any bar: the reducer refuses to size a Unit until an account.snapshot has
// supplied an available-cash figure, and its AsOf is at or before every
// decision bar's previous close.
func propertySnapshotEnvelope(t *testing.T, sequence uint64, cfg event.ConfigurationPayload, at, recordedAt time.Time) event.Envelope {
	t.Helper()
	payload := mustMarshalT(t, event.AccountSnapshotPayload{
		AsOf:          at,
		Equity:        cfg.NotionalAccount.StartingEquity,
		AvailableCash: propertyAvailableCash,
		Currency:      "USD",
	})
	return event.Envelope{
		ID:                "account-snapshot-1",
		Type:              event.AccountSnapshotEventType,
		SchemaVersion:     event.AccountSnapshotSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         at,
		RecordedAt:        recordedAt,
		Sequence:          sequence,
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
func signalNeverSurvivesFixture(t *testing.T, recordedAt arrivalSchedule) (cfg event.ConfigurationPayload, envelopes []event.Envelope) {
	t.Helper()
	cfg = propertyFixtureConfiguration()
	envelopes = []event.Envelope{
		propertyConfigEnvelope(t, cfg, propertyDay(0), recordedAt(0, propertyDay(0))),
		propertySnapshotEnvelope(t, 2, cfg, propertyDay(0), recordedAt(0, propertyDay(0))),
	}

	seq := uint64(3)
	for i := 0; i < 20; i++ {
		high := 100 + float64(i+1) // 101..120: channel high after warm-up is 120
		bar := flatBar("AAPL", propertyDay(i+1), high, high-2)
		envelopes = append(envelopes, propertyBarEnvelope(t, seq, bar, cfg, recordedAt(i+1, bar.PeriodEnd)))
		seq++
	}

	// Bar 21: high 130 exceeds the warmed-up channel high of 120 — a
	// breakout, a Signal, and (this fixture's prices and starting equity
	// size at least one whole Unit) a trade proposal.
	breakout := flatBar("AAPL", propertyDay(21), 130, 128)
	envelopes = append(envelopes, propertyBarEnvelope(t, seq, breakout, cfg, recordedAt(21, breakout.PeriodEnd)))
	seq++

	// Bar 22: high 125 does not exceed the channel high, now 130 (bars 2-21)
	// since bar 21 entered the window — not a breakout, so the proposal bar
	// 21 raised is superseded rather than confirmed. No fill is ever
	// delivered, matching this ticket's scope: replay equivalence tests the
	// reducer's own decisions, not whether the fill simulator would have
	// filled this proposal.
	quiet := flatBar("AAPL", propertyDay(22), 125, 123)
	envelopes = append(envelopes, propertyBarEnvelope(t, seq, quiet, cfg, recordedAt(22, quiet.PeriodEnd)))

	return cfg, envelopes
}

// arrivalSchedule decides when the input at index i, carrying eventTime, was
// recorded as arriving.
type arrivalSchedule func(index int, eventTime time.Time) time.Time

// arrivedWhenItHappened is the baseline schedule: RecordedAt tracks EventTime.
func arrivedWhenItHappened(_ int, eventTime time.Time) time.Time { return eventTime }

// arrivedOnADifferentSchedule is the schedule that makes arrival-time
// independence a real test rather than a restatement.
//
// A single constant offset applied to every input preserves every interval
// between consecutive arrivals, so it cannot detect a reducer that reads
// ELAPSED time between events — which is at least as likely a wall-clock
// leak as one reading an absolute instant, and is the reading a uniform
// shift is structurally blind to. The per-index steps below vary, so every
// interval changes and no two change alike.
//
// The offsets are cumulative and strictly increasing, so a monotonic arrival
// stream stays monotonic: the shifted stream is one a real clock could have
// produced, and a failure therefore means the reducer is wrong rather than
// that the input was impossible. Every step is far under the fixture's
// one-day bar spacing, so ordering is never inverted.
func arrivedOnADifferentSchedule(index int, eventTime time.Time) time.Time {
	offset := 37 * time.Minute
	for k := 0; k <= index; k++ {
		offset += time.Duration(k%7+1) * time.Minute
	}
	return eventTime.Add(offset)
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

	cfg, inputs := signalNeverSurvivesFixture(t, arrivedWhenItHappened)

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
//
// The arrivals are RESCHEDULED, not translated. A single constant offset
// leaves every interval between consecutive arrivals intact and so cannot
// catch a reducer reading elapsed time between events; see
// arrivedOnADifferentSchedule and TestTheArrivalTimeComparisonCatchesAnIntervalLeak,
// which holds that sensitivity in place.
func TestArrivalTimeIndependence(t *testing.T) {
	t.Parallel()

	cfg, original := signalNeverSurvivesFixture(t, arrivedWhenItHappened)
	_, shifted := signalNeverSurvivesFixture(t, arrivedOnADifferentSchedule)

	assertArrivalsAreRescheduledNotTranslated(t, original, shifted)

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

// assertArrivalsAreRescheduledNotTranslated is the sensitivity guard on the
// arrival-time property: it confirms the two streams differ in the GAPS
// between consecutive arrivals and not merely in their absolute instants. If
// this ever passes vacuously the property test above silently weakens to the
// uniform-shift version, which no interval-reading leak can fail.
func assertArrivalsAreRescheduledNotTranslated(t *testing.T, original, shifted []event.Envelope) {
	t.Helper()

	if len(original) != len(shifted) || len(original) < 2 {
		t.Fatalf("got %d original and %d shifted inputs; need the same count and at least two to have an interval at all", len(original), len(shifted))
	}

	changed := 0
	for i := 1; i < len(original); i++ {
		was := original[i].RecordedAt.Sub(original[i-1].RecordedAt)
		now := shifted[i].RecordedAt.Sub(shifted[i-1].RecordedAt)
		if now != was {
			changed++
		}
		if now < 0 {
			t.Fatalf("arrival %d goes backwards in the shifted stream (%s after %s); a rescheduled stream must still be one a real clock could produce",
				i, shifted[i].RecordedAt, shifted[i-1].RecordedAt)
		}
	}
	if changed == 0 {
		t.Fatal("every interval between consecutive arrivals survived the shift unchanged; this is a translation, not a reschedule, and a reducer reading elapsed time between events would pass")
	}
}

// TestTheArrivalTimeComparisonCatchesAnIntervalLeak is the test for the test.
//
// It runs the arrival-time comparison against a handler that deliberately
// leaks the elapsed time between consecutive arrivals into what it emits, and
// requires the comparison to report a divergence. It then runs the SAME
// handler under a uniform shift and requires that to be missed — which is the
// evidence that a constant offset is structurally blind to this leak, and the
// reason the schedule above varies its steps.
//
// Without this, the property test could only show that a correct reducer
// passes, which is the weaker half of what is worth knowing.
func TestTheArrivalTimeComparisonCatchesAnIntervalLeak(t *testing.T) {
	t.Parallel()

	baseline := buildArrivals(t, arrivedWhenItHappened)
	rescheduled := buildArrivals(t, arrivedOnADifferentSchedule)
	translated := buildArrivals(t, func(_ int, eventTime time.Time) time.Time {
		return eventTime.Add(37 * time.Minute)
	})

	if diverged := intervalLeakDiverges(t, baseline, rescheduled); !diverged {
		t.Fatal("a reducer leaking the interval between arrivals was NOT caught by the rescheduled stream; the arrival-time property is not testing what it claims")
	}
	if diverged := intervalLeakDiverges(t, baseline, translated); diverged {
		t.Fatal("a uniform shift caught the interval leak, so it is not blind after all; the reasoning behind the varying schedule needs re-reading")
	}
}

// buildArrivals returns the fixture's input stream under one arrival schedule.
func buildArrivals(t *testing.T, schedule arrivalSchedule) []event.Envelope {
	t.Helper()
	_, envelopes := signalNeverSurvivesFixture(t, schedule)
	return envelopes
}

// intervalLeakDiverges runs an interval-leaking handler over both streams and
// reports whether comparing the emissions, modulo RecordedAt, finds a
// divergence — exactly the comparison TestArrivalTimeIndependence performs.
func intervalLeakDiverges(t *testing.T, original, shifted []event.Envelope) bool {
	t.Helper()

	fromOriginal := runLeakingHandler(t, original)
	fromShifted := runLeakingHandler(t, shifted)
	if len(fromOriginal) != len(fromShifted) || len(fromOriginal) == 0 {
		t.Fatalf("the leaking handler emitted %d and %d decisions; it must emit the same non-zero number for the comparison to mean anything", len(fromOriginal), len(fromShifted))
	}

	for i := range fromOriginal {
		want, got := fromOriginal[i], fromShifted[i]
		want.RecordedAt, got.RecordedAt = time.Time{}, time.Time{}
		if replay.Equivalent([]event.Envelope{want}, []event.Envelope{got}) != nil {
			return true
		}
	}
	return false
}

func runLeakingHandler(t *testing.T, inputs []event.Envelope) []event.Envelope {
	t.Helper()
	engine, err := replay.New(&intervalLeakingHandler{})
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}
	emitted, err := engine.Run(context.Background(), inputs)
	if err != nil {
		t.Fatalf("Engine.Run() error = %v", err)
	}
	return emitted
}

// intervalLeakingHandler is a deliberately wrong handler: it computes the
// elapsed time since the previous input ARRIVED and puts it in what it emits.
// A decision derived from how long the gap was, rather than from the ordered
// event stream, is exactly the defect arrival-time independence exists to
// catch.
type intervalLeakingHandler struct{ previousArrival time.Time }

func (h *intervalLeakingHandler) Apply(_ context.Context, in event.Envelope) ([]event.Envelope, error) {
	var elapsed time.Duration
	if !h.previousArrival.IsZero() {
		elapsed = in.RecordedAt.Sub(h.previousArrival)
	}
	h.previousArrival = in.RecordedAt

	payload := json.RawMessage(fmt.Sprintf(`{"elapsed_since_previous_arrival_ns":%d}`, elapsed))
	return []event.Envelope{{
		ID:                "leak-" + in.ID,
		Type:              "test.leaked",
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		SchemaVersion:     1,
		EventTime:         in.EventTime,
		RecordedAt:        in.RecordedAt,
		Source:            "leaking-fixture",
		StrategyVersion:   in.StrategyVersion,
		ConfigurationHash: in.ConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}}, nil
}

// TestASignalNeverSurvivesItsBar: bar 21's Signal and the proposal it caused
// belong to bar 21 alone. Bar 22 does not renew, repeat, or extend it — it
// only records that the proposal is now dead, dated to the bar it was
// raised on (PeriodEnd) even though the supersession is only observable once
// bar 22 arrives (ExpiredAt) — CONTEXT.md: "Signal", ADR 0011.
func TestASignalNeverSurvivesItsBar(t *testing.T) {
	t.Parallel()

	cfg, inputs := signalNeverSurvivesFixture(t, arrivedWhenItHappened)
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
