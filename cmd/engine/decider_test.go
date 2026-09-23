package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// stubEnvelope is a minimal, individually-valid event.Envelope for a test
// handler to hand back as one of "the decisions" — its exact content is
// never inspected by newDecider, only carried through.
func stubEnvelope(id string) event.Envelope {
	payload := []byte(`{}`)
	return event.Envelope{
		ID:                id,
		Type:              "stub.decision",
		SchemaVersion:     1,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		RecordedAt:        time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Sequence:          1,
		Source:            "stub",
		StrategyVersion:   "stub/1.0.0+test",
		ConfigurationHash: "sha256:stub",
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

func stubInput(t *testing.T, id string) event.Envelope {
	t.Helper()
	payload := []byte(`{}`)
	return event.Envelope{
		ID:                id,
		Type:              "stub.input",
		SchemaVersion:     1,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		RecordedAt:        time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Sequence:          1,
		Source:            "test-client",
		StrategyVersion:   "stub/1.0.0+test",
		ConfigurationHash: "sha256:stub",
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// stubInputAt is stubInput with an explicit Sequence, for tests that must
// control contiguity (replay.Engine.Apply's own check) directly.
func stubInputAt(t *testing.T, id string, sequence uint64) event.Envelope {
	t.Helper()
	input := stubInput(t, id)
	input.Sequence = sequence
	return input
}

// newTestWiring composes handler exactly as run composes the real reducer —
// journal.Recorder, then the one *replay.Engine that persists its
// contiguity cursor across every call — plus a fresh *wireGuard, so a test
// can drive newDecider without repeating that composition inline.
func newTestWiring(t *testing.T, handler replay.Handler) (*wireGuard, *replay.Engine, *journal.Recorder) {
	t.Helper()
	recorder := journal.NewRecorder(handler)
	engine, err := replay.New(recorder)
	if err != nil {
		t.Fatalf("replay.New: %v", err)
	}
	return &wireGuard{}, engine, recorder
}

// TestNewDeciderBundlesEveryDecisionIntoOneEnvelope is decision 1's own
// test: a handler emitting several decisions for one input must have every
// one of them reach the wire, in order, inside the single envelope
// transport.Decider's contract allows — none of them picked as "the"
// decision, none summarised, none dropped.
func TestNewDeciderBundlesEveryDecisionIntoOneEnvelope(t *testing.T) {
	want := []event.Envelope{stubEnvelope("a"), stubEnvelope("b"), stubEnvelope("c")}
	handler := replay.HandlerFunc(func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
		return want, nil
	})
	guard, engine, _ := newTestWiring(t, handler)
	decide := newDecider(guard, engine, "stub/1.0.0+test", "sha256:stub")

	input := stubInput(t, "input-1")
	reply, err := decide(context.Background(), input)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if reply.Type != decisionsEventType {
		t.Fatalf("reply type = %q, want %q", reply.Type, decisionsEventType)
	}
	if reply.CausationID != input.ID {
		t.Fatalf("reply causation id = %q, want %q", reply.CausationID, input.ID)
	}

	var payload decisionsPayload
	if err := json.Unmarshal(reply.Payload, &payload); err != nil {
		t.Fatalf("decode reply payload: %v", err)
	}
	if len(payload.Decisions) != len(want) {
		t.Fatalf("got %d decisions, want %d", len(payload.Decisions), len(want))
	}
	for i, decision := range payload.Decisions {
		if decision.ID != want[i].ID {
			t.Errorf("decision %d id = %q, want %q", i, decision.ID, want[i].ID)
		}
		// Stamped by replay.Engine.Apply now that decision 1 routes through
		// it (not the raw, unstamped values journal.Recorder.Apply alone
		// would have returned): CausationID is the wire's own proof of this,
		// checked exactly the way transport.Client.exchange checks a reply.
		if decision.CausationID != input.ID {
			t.Errorf("decision %d causation id = %q, want %q", i, decision.CausationID, input.ID)
		}
		if decision.Sequence == 0 {
			t.Errorf("decision %d sequence = 0, want it stamped", i)
		}
	}
}

// TestNewDeciderWritesAnEmptyArrayNeverNullForZeroDecisions is decision 1's
// wire-format half: json.Marshal of a nil Go slice writes `null`, and
// decisionsPayload's own doc comment states why that must never reach the
// wire — a Python consumer treats `null` and `[]` as different values, even
// though both decode to a zero-length Go slice, which is why this asserts
// the literal wire bytes rather than a decoded length.
func TestNewDeciderWritesAnEmptyArrayNeverNullForZeroDecisions(t *testing.T) {
	handler := replay.HandlerFunc(func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
		return nil, nil
	})
	guard, engine, _ := newTestWiring(t, handler)
	decide := newDecider(guard, engine, "stub/1.0.0+test", "sha256:stub")

	reply, err := decide(context.Background(), stubInput(t, "input-1"))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !strings.Contains(string(reply.Payload), `"decisions":[]`) {
		t.Fatalf("payload = %s, want the literal bytes %q, not null", reply.Payload, `"decisions":[]`)
	}

	var payload decisionsPayload
	if err := json.Unmarshal(reply.Payload, &payload); err != nil {
		t.Fatalf("decode reply payload: %v", err)
	}
	if len(payload.Decisions) != 0 {
		t.Fatalf("got %d decisions, want 0", len(payload.Decisions))
	}
}

// TestNewDeciderFailsClosedOnAReducerError is "an envelope the reducer
// rejects returns a protocol error, never a fabricated decision", exercised
// directly against the Decider this command builds: a handler error must
// reach the caller as an error, with the returned envelope left at its zero
// value, never populated with a guessed or partial decision.
func TestNewDeciderFailsClosedOnAReducerError(t *testing.T) {
	wantErr := errors.New("risk controller is not ready")
	handler := replay.HandlerFunc(func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
		return nil, wantErr
	})
	guard, engine, _ := newTestWiring(t, handler)
	decide := newDecider(guard, engine, "stub/1.0.0+test", "sha256:stub")

	reply, err := decide(context.Background(), stubInput(t, "input-1"))
	if !errors.Is(err, wantErr) {
		t.Fatalf("decide error = %v, want it to wrap %v", err, wantErr)
	}
	if reply.ID != "" || reply.Type != "" || reply.Payload != nil {
		t.Fatalf("decide returned a non-empty envelope alongside an error: %+v", reply)
	}
}

// TestNewDeciderRejectsADuplicatedInput and
// TestNewDeciderRejectsAGappedInput are the routing this composition added
// specifically because journal.Recorder.Apply performs no contiguity check
// of its own (decision 1's "continued" section, decider.go): calling it
// directly, as an earlier version of this command did, let a duplicated or
// gapped Sequence reach the reducer and be journalled as though the input
// stream were well-formed. Routing through replay.Engine (engine, below) is
// what closes it, and both tests confirm the journal is still replayable
// afterwards: the refused input was never recorded at all, so there is
// nothing in it for a later replay to disagree with.
func TestNewDeciderRejectsADuplicatedInput(t *testing.T) {
	handler := replay.HandlerFunc(func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil })
	guard, engine, recorder := newTestWiring(t, handler)
	decide := newDecider(guard, engine, "stub/1.0.0+test", "sha256:stub")

	if _, err := decide(context.Background(), stubInputAt(t, "bar-1", 1)); err != nil {
		t.Fatalf("first bar: decide: %v", err)
	}
	_, err := decide(context.Background(), stubInputAt(t, "bar-1-again", 1))
	if err == nil || !strings.Contains(err.Error(), "non-contiguous sequence") {
		t.Fatalf("duplicated sequence: decide error = %v, want non-contiguous sequence", err)
	}

	assertRecorderReplayable(t, recorder, "sha256:stub", "stub/1.0.0+test")
	if len(recorder.Entries()) != 1 {
		t.Fatalf("recorder holds %d entries, want exactly the first bar's own input", len(recorder.Entries()))
	}
}

func TestNewDeciderRejectsAGappedInput(t *testing.T) {
	handler := replay.HandlerFunc(func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil })
	guard, engine, recorder := newTestWiring(t, handler)
	decide := newDecider(guard, engine, "stub/1.0.0+test", "sha256:stub")

	if _, err := decide(context.Background(), stubInputAt(t, "bar-1", 1)); err != nil {
		t.Fatalf("first bar: decide: %v", err)
	}
	// Sequence 3 skips 2: a gap, not a duplicate.
	_, err := decide(context.Background(), stubInputAt(t, "bar-3", 3))
	if err == nil || !strings.Contains(err.Error(), "non-contiguous sequence") {
		t.Fatalf("gapped sequence: decide error = %v, want non-contiguous sequence", err)
	}

	assertRecorderReplayable(t, recorder, "sha256:stub", "stub/1.0.0+test")
	if len(recorder.Entries()) != 1 {
		t.Fatalf("recorder holds %d entries, want exactly the first bar's own input", len(recorder.Entries()))
	}

	// The engine's own cursor is unmoved by the refusal: sequence 2, the one
	// that was actually missing, is still accepted next.
	if _, err := decide(context.Background(), stubInputAt(t, "bar-2", 2)); err != nil {
		t.Fatalf("the correctly-numbered next bar was refused: %v", err)
	}
}

// assertRecorderReplayable writes recorder's entries as a journal and
// checks journal.Verify accepts it — the brief's own ask, "with the journal
// still replayable afterwards", for a recorder that has just refused an
// input.
func assertRecorderReplayable(t *testing.T, recorder *journal.Recorder, configurationHash, strategyVersion string) {
	t.Helper()
	header, err := recorder.Header(configurationHash, strategyVersion)
	if err != nil {
		t.Fatalf("recorder.Header: %v", err)
	}
	var buf strings.Builder
	if err := journal.Write(&buf, header, recorder.Entries()); err != nil {
		t.Fatalf("journal.Write: %v", err)
	}
	if _, err := journal.Verify(strings.NewReader(buf.String())); err != nil {
		t.Fatalf("journal.Verify: %v", err)
	}
}

// TestWireGuardSerialisesAnAbandonedDecisionAgainstTheNextOne is finding 3's
// test: transport.Server.decideWithTimeout gives up waiting on a Decider
// call the moment its ctx is done, in a goroutine transport's own
// Server.wg never tracks — so that call can still be running, inside
// strategy.Reducer.Apply (which discards its own ctx and so never notices),
// when a LATER call starts. ADR 0014's own documented behaviour has the
// adapter carry on over the SAME connection after a rejected timeout ("the
// session survives"), so a later call arriving while the first is still
// live is the real shape of the race, not a contrived one.
//
// This drives that shape directly against wireGuard with an explicitly
// cancelled context standing in for decideWithTimeout's own ctx.Done()
// firing, so the ordering it proves is exact rather than a hope about
// wall-clock timing against a real transport.Server.
func TestWireGuardSerialisesAnAbandonedDecisionAgainstTheNextOne(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	handler := replay.HandlerFunc(func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
		close(entered)
		<-release
		close(finished)
		return nil, nil
	})
	guard, engine, recorder := newTestWiring(t, handler)

	abandonedCtx, cancel := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() {
		_, err := guard.decide(abandonedCtx, engine, stubInputAt(t, "bar-1", 1))
		firstErr <- err
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first call to enter the handler")
	}

	// The wire gives up on this decision without waiting for the goroutine
	// running it — exactly what Server.decideWithTimeout's own ctx.Done()
	// firing does, modelled here by cancelling this call's own context
	// while it is still inside the handler.
	cancel()

	// A second call for the next bar on the same connection arrives before
	// the first has actually finished — it can only be blocked acquiring
	// guard's lock, never running its own handler call concurrently with
	// the first's.
	secondDone := make(chan struct{})
	var secondErr error
	go func() {
		_, secondErr = guard.decide(context.Background(), engine, stubInputAt(t, "bar-2", 2))
		close(secondDone)
	}()

	// The first call is still deliberately blocked on release at this
	// point, so the second call has nothing to acquire the lock from yet;
	// a bounded wait confirms it has not somehow completed anyway (which
	// could only happen if the two calls ran concurrently rather than
	// serialised).
	select {
	case <-secondDone:
		t.Fatal("the second call completed while the first was still blocked; it must be serialised behind it, not run concurrently with it")
	case <-time.After(100 * time.Millisecond):
	}

	// Only now does the first call's own handler body actually finish —
	// still holding guard's lock (decide's mu.Unlock is deferred past this
	// point) — so the second call could not have raced it even once
	// release closes.
	close(release)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the abandoned first call's handler body to finish")
	}

	if err := <-firstErr; err == nil {
		t.Fatal("the abandoned call returned no error; want it to report its own context having been cancelled")
	}
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the second call to finish")
	}
	if secondErr == nil {
		t.Fatal("the second call succeeded; want it poisoned by the first call's abandonment")
	}

	// awaitIdle must return promptly now that both calls are done — proving
	// it is not itself what was blocking, only what waits for a call
	// already in flight.
	idleDone := make(chan struct{})
	go func() { guard.awaitIdle(); close(idleDone) }()
	select {
	case <-idleDone:
	case <-time.After(5 * time.Second):
		t.Fatal("awaitIdle did not return once every call had finished")
	}

	// The abandoned call's own input still reached the recorder: no data
	// was lost by the wire having given up on it, because it was allowed to
	// run to completion under the same lock rather than raced or discarded.
	found := false
	for _, entry := range recorder.Entries() {
		if entry.Envelope.ID == "bar-1" {
			found = true
		}
	}
	if !found {
		t.Fatal("the abandoned decision's own input never reached the recorder")
	}
}

// TestWireGuardAwaitIdleWaitsForAnInFlightCall is awaitIdle's own contract,
// isolated from poisoning: called while a decision is still running (not
// necessarily an abandoned one), it must not return until that call is
// done — the property run relies on before it ever reads recorder.Entries()
// or recorder.Header().
func TestWireGuardAwaitIdleWaitsForAnInFlightCall(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	handler := replay.HandlerFunc(func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
		close(entered)
		<-release
		return nil, nil
	})
	guard, engine, _ := newTestWiring(t, handler)

	go func() { _, _ = guard.decide(context.Background(), engine, stubInputAt(t, "bar-1", 1)) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the call to enter the handler")
	}

	idleDone := make(chan struct{})
	go func() { guard.awaitIdle(); close(idleDone) }()

	select {
	case <-idleDone:
		t.Fatal("awaitIdle returned while a call was still in flight")
	case <-time.After(50 * time.Millisecond):
		// expected: still blocked.
	}

	close(release)
	select {
	case <-idleDone:
	case <-time.After(5 * time.Second):
		t.Fatal("awaitIdle did not return once the in-flight call finished")
	}
}

// TestDeciderRefusesABarBeforeConfiguration exercises the fail-closed rule
// that an unconfigured engine refuses inputs rather than sizing against
// nothing, at the level internal/strategy.Reducer itself enforces it: a
// real Reducer that has never been given a configuration input rejects a
// bar outright, and this command's Decider must carry that refusal through
// as an error rather than absorb it. run (engine.go) makes this unreachable
// in ordinary operation by always applying the configuration input before
// transport.Listen is ever called (decision 2); this test pins the guard
// directly, independent of that ordering.
func TestDeciderRefusesABarBeforeConfiguration(t *testing.T) {
	cfg := testConfiguration(t)
	strategyVersion := event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, "test-build")
	reducer, err := strategy.NewReducer(strategyVersion, cfg)
	if err != nil {
		t.Fatalf("construct the reducer: %v", err)
	}
	guard, engine, _ := newTestWiring(t, reducer)
	decide := newDecider(guard, engine, strategyVersion, event.ConfigurationHash(cfg))

	bar := flatBar("TEST", 0)
	envelope := barEnvelope(t, bar, 1, strategyVersion, event.ConfigurationHash(cfg))

	reply, err := decide(context.Background(), envelope)
	if err == nil {
		t.Fatal("decide succeeded against an unconfigured reducer; want an error")
	}
	if !strings.Contains(err.Error(), "configuration event") {
		t.Errorf("error = %v, want it to name the missing configuration event", err)
	}
	if reply.ID != "" || reply.Type != "" || reply.Payload != nil {
		t.Fatalf("decide returned a non-empty envelope alongside an error: %+v", reply)
	}
}
