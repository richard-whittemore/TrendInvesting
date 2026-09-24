package strategy_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// This file holds #169's tests: an adapter's own record of a deliberate stop
// (event.AdapterRunStoppedEventType), distinguishing a run the adapter chose
// to end from one that simply ran out of bars (ADR 0012).

// validRunStoppedPayload is the fixture stop: LEAN's untrusted DELISTED,
// naming AAPL (adapter/lean/algorithm.go's handle_delisting).
func validRunStoppedPayload() event.AdapterRunStoppedPayload {
	return event.AdapterRunStoppedPayload{
		Reason:       event.AdapterRunStoppedReasonDelisted,
		InstrumentID: "AAPL",
		Detail:       "LEAN reports AAPL DELISTED at 2026-02-27T00:00:00Z",
	}
}

// adapterRunStoppedEnvelope is the deliberate-stop fact, as an input
// envelope, mirroring end_of_stream_test.go's runCompletedEnvelope.
func adapterRunStoppedEnvelope(t *testing.T, sequence uint64, at time.Time, payload event.AdapterRunStoppedPayload) event.Envelope {
	t.Helper()
	encoded := mustMarshal(t, payload)
	return event.Envelope{
		ID:                "run-stopped",
		Type:              event.AdapterRunStoppedEventType,
		SchemaVersion:     event.AdapterRunStoppedSchemaVersion,
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

func (s *stream) stop(payload event.AdapterRunStoppedPayload, at time.Time) *stream {
	s.seq++
	s.envelopes = append(s.envelopes, adapterRunStoppedEnvelope(s.t, s.seq, at, payload))
	return s
}

// stopMutated appends a stop envelope after mutate has had a chance to
// change it, mirroring end_of_stream_test.go's endOfStreamMutated.
func (s *stream) stopMutated(at time.Time, mutate func(*event.Envelope)) *stream {
	s.seq++
	envelope := adapterRunStoppedEnvelope(s.t, s.seq, at, validRunStoppedPayload())
	mutate(&envelope)
	envelope.PayloadHash = event.HashPayload(envelope.Payload)
	s.envelopes = append(s.envelopes, envelope)
	return s
}

// TestAdapterRunStoppedIsRecordedWithNoDecision is #169's central claim:
// "record, no decision". A run that stops and then completes emits exactly
// what the identical run would have emitted with no stop at all — the stop
// itself contributes nothing to the decision stream.
func TestAdapterRunStoppedIsRecordedWithNoDecision(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()

	withoutStop := newStream(t, cfg).bars(breakoutBars("AAPL")).endOfStream(day(56)).mustRun()
	withStop := newStream(t, cfg).bars(breakoutBars("AAPL")).
		stop(validRunStoppedPayload(), day(56)).
		endOfStream(day(56)).
		mustRun()

	if d := replay.Equivalent(withoutStop, withStop); d != nil {
		t.Fatalf("a recorded stop changed the decision stream: %+v", d)
	}
}

// TestAdapterRunStoppedThenRunCompletedReplaysByteIdentically drives the
// full ADR 0017 evidence chain for a stopped run: the stop is written to a
// journal as an input, the chain verifies, and a fresh reducer replaying the
// journal's own inputs reproduces the same (empty) decisions.
func TestAdapterRunStoppedThenRunCompletedReplaysByteIdentically(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	s := newStream(t, cfg).bars(breakoutBars("AAPL")).
		stop(validRunStoppedPayload(), day(56)).
		endOfStream(day(56))
	emitted := s.mustRun()

	verifyMovementJournal(t, s, emitted)
}

// TestNothingButRunCompletedMayFollowAStop is the ordering rule #169 asks
// for: a stop that some further input then contradicted cannot be recorded,
// because it would leave a run claiming to have deliberately ended while
// still receiving input.
func TestNothingButRunCompletedMayFollowAStop(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()

	t.Run("a bar", func(t *testing.T) {
		t.Parallel()
		newStream(t, cfg).bars(breakoutBars("AAPL")).
			stop(validRunStoppedPayload(), day(56)).
			bar(quietBar("AAPL")).
			wantRunError("the run was deliberately stopped")
	})

	t.Run("a second stop event", func(t *testing.T) {
		t.Parallel()
		newStream(t, cfg).bars(breakoutBars("AAPL")).
			stop(validRunStoppedPayload(), day(56)).
			stop(validRunStoppedPayload(), day(56)).
			wantRunError("the run was deliberately stopped")
	})
}

// TestARunCanCompleteWithNoStopAtAll: a stop is never required, only
// permitted — the ordinary path (no adapter.run.stopped input at all) is
// unaffected by this reducer accepting the new event type.
func TestARunCanCompleteWithNoStopAtAll(t *testing.T) {
	t.Parallel()

	newStream(t, validConfigurationPayload()).bars(breakoutBars("AAPL")).endOfStream(day(56)).mustRun()
}

// TestAnAdapterRunStoppedEventBeforeAConfigurationFailsClosed mirrors every
// other input's own requirement (e.g.
// TestAnEndOfStreamEventBeforeAConfigurationFailsClosed): a stream consisting
// of a stop with no configuration would record a run that never ran.
func TestAnAdapterRunStoppedEventBeforeAConfigurationFailsClosed(t *testing.T) {
	t.Parallel()

	reducer, err := strategy.NewReducer(testStrategyVersion, validConfigurationPayload())
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	_, err = engine.Run(context.Background(), []event.Envelope{
		adapterRunStoppedEnvelope(t, 1, day(56), validRunStoppedPayload()),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "before a configuration event") {
		t.Fatalf("Run() error = %v, want the same wording every other input uses", err)
	}
}

// TestAnAdapterRunStoppedEventAtTheWrongSchemaVersionFailsClosed mirrors
// TestAnEndOfStreamEventAtTheWrongSchemaVersionFailsClosed: an older or
// newer schema is rejected, never silently upgraded, until an explicit
// upcaster exists (ADR 0015).
func TestAnAdapterRunStoppedEventAtTheWrongSchemaVersionFailsClosed(t *testing.T) {
	t.Parallel()

	newStream(t, validConfigurationPayload()).
		stopMutated(day(56), func(e *event.Envelope) {
			e.SchemaVersion = event.AdapterRunStoppedSchemaVersion + 1
		}).
		wantRunError("adapter run stopped payload schema version", "ADR 0015")
}

func TestAnUndecodableAdapterRunStoppedPayloadFailsClosed(t *testing.T) {
	t.Parallel()

	newStream(t, validConfigurationPayload()).
		stopMutated(day(56), func(e *event.Envelope) {
			e.Payload = []byte(`[]`)
		}).
		wantRunError("decode adapter run stopped payload")
}

// TestAnInvalidAdapterRunStoppedPayloadFailsClosed: the reducer surfaces
// AdapterRunStoppedPayload.Validate's own refusals rather than absorbing
// them — an unknown reason here, event-level validation's own table
// (internal/event/run_stopped_test.go) covers the rest of the payload.
func TestAnInvalidAdapterRunStoppedPayloadFailsClosed(t *testing.T) {
	t.Parallel()

	newStream(t, validConfigurationPayload()).
		stopMutated(day(56), func(e *event.Envelope) {
			e.Payload = mustMarshal(t, event.AdapterRunStoppedPayload{
				Reason: "invalid-startup", InstrumentID: "AAPL", Detail: "not a recognised reason",
			})
		}).
		wantRunError("not a recognised run stop reason")
}
