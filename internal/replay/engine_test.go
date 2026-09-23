package replay_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// envelope builds a valid input envelope at the given sequence, so each test
// states only what it is actually about. Each envelope's ID is derived from
// its sequence so tests can tell which input caused which emission.
func envelope(sequence uint64) event.Envelope {
	now := time.Date(2026, time.August, 29, 20, 0, 0, 0, time.UTC)
	payload := json.RawMessage(`{}`)
	return event.Envelope{
		ID:                fmt.Sprintf("evt-%d", sequence),
		Type:              "test.event",
		SchemaVersion:     1,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         now,
		RecordedAt:        now,
		Sequence:          sequence,
		Source:            "fixture",
		StrategyVersion:   "test-strategy-1.0.0",
		ConfigurationHash: "cfg-test",
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// decision builds a valid envelope a handler can emit, identified by id. It
// deliberately leaves Sequence, CausationID, and CorrelationID at their zero
// values (or, when asked, set to deliberately wrong values) so tests can
// assert the engine — not the handler — is what sets them.
func decision(id string) event.Envelope {
	now := time.Date(2026, time.August, 29, 20, 5, 0, 0, time.UTC)
	payload := json.RawMessage(`{"id":"` + id + `"}`)
	return event.Envelope{
		ID:                id,
		Type:              "test.decision",
		SchemaVersion:     1,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         now,
		RecordedAt:        now,
		Source:            "handler",
		StrategyVersion:   "test-strategy-1.0.0",
		ConfigurationHash: "cfg-test",
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

func TestEngineRunAppliesEventsInOrder(t *testing.T) {
	t.Parallel()

	var got []uint64
	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) ([]event.Envelope, error) {
		got = append(got, item.Sequence)
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := engine.Run(context.Background(), []event.Envelope{envelope(4), envelope(5), envelope(6)}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(got) != 3 || got[0] != 4 || got[1] != 5 || got[2] != 6 {
		t.Fatalf("applied sequences = %v, want [4 5 6]", got)
	}
}

func TestEngineRunRejectsSequenceGap(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, nil }))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = engine.Run(context.Background(), []event.Envelope{envelope(1), envelope(3)})
	if err == nil || !strings.Contains(err.Error(), "non-contiguous sequence") {
		t.Fatalf("Run() error = %v, want non-contiguous sequence", err)
	}
}

// TestEngineApplyEnforcesContiguityAcrossSeparateCalls is Apply's whole
// reason for existing separately from Run: a caller that never has a batch
// in hand — one event arriving at a time from a socket, not read from a
// fixture — gets the identical contiguity guarantee across as many separate
// calls as it makes, not only within one slice handed to Run in one call.
func TestEngineApplyEnforcesContiguityAcrossSeparateCalls(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, nil }))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := engine.Apply(context.Background(), envelope(1)); err != nil {
		t.Fatalf("Apply(1) error = %v", err)
	}
	// A gap between two SEPARATE calls, not two elements of one slice.
	_, err = engine.Apply(context.Background(), envelope(3))
	if err == nil || !strings.Contains(err.Error(), "non-contiguous sequence") {
		t.Fatalf("Apply(3) after Apply(1) error = %v, want non-contiguous sequence", err)
	}
}

// TestEngineApplyAcceptsAnyStartingSequence mirrors Run's own index==0
// exemption: the first call this Engine ever receives fixes where its
// contiguity check starts counting from, whatever sequence it names.
func TestEngineApplyAcceptsAnyStartingSequence(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, nil }))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := engine.Apply(context.Background(), envelope(41)); err != nil {
		t.Fatalf("Apply(41) error = %v", err)
	}
	if _, err := engine.Apply(context.Background(), envelope(42)); err != nil {
		t.Fatalf("Apply(42) error = %v", err)
	}
}

// TestEngineApplySharesTheOutputSequenceCounterAcrossCalls is the emitted
// stream's own contiguity, checked the same way as the input stream's: two
// separate calls to Apply on one Engine still number their combined
// emissions 1, 2, 3..., never restarting at 1 on the second call the way two
// separate Run calls (or, before this method existed, two separate Engines)
// would.
func TestEngineApplySharesTheOutputSequenceCounterAcrossCalls(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) ([]event.Envelope, error) {
		return []event.Envelope{decision(fmt.Sprintf("d%d", item.Sequence))}, nil
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	first, err := engine.Apply(context.Background(), envelope(1))
	if err != nil {
		t.Fatalf("Apply(1) error = %v", err)
	}
	second, err := engine.Apply(context.Background(), envelope(2))
	if err != nil {
		t.Fatalf("Apply(2) error = %v", err)
	}
	if len(first) != 1 || first[0].Sequence != 1 {
		t.Fatalf("first call emitted %+v, want one envelope at output sequence 1", first)
	}
	if len(second) != 1 || second[0].Sequence != 2 {
		t.Fatalf("second call emitted %+v, want one envelope at output sequence 2", second)
	}
}

// TestEngineApplyAdvancesContiguityEvenWhenTheHandlerErrors proves the
// distinction Apply's own doc comment draws: contiguity is a property of the
// INPUT STREAM's numbering, not of whether the handler went on to accept
// what it was given. A handler error for sequence 2 must not make sequence
// 3 look like a gap on the very next call — that would reject an input the
// producer numbered perfectly correctly, for a reason that has nothing to do
// with its own numbering.
func TestEngineApplyAdvancesContiguityEvenWhenTheHandlerErrors(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("rejected on business grounds, not a stream defect")
	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) ([]event.Envelope, error) {
		if item.Sequence == 2 {
			return nil, wantErr
		}
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := engine.Apply(context.Background(), envelope(1)); err != nil {
		t.Fatalf("Apply(1) error = %v", err)
	}
	if _, err := engine.Apply(context.Background(), envelope(2)); !errors.Is(err, wantErr) {
		t.Fatalf("Apply(2) error = %v, want it to wrap %v", err, wantErr)
	}
	if _, err := engine.Apply(context.Background(), envelope(3)); err != nil {
		t.Fatalf("Apply(3) error = %v, want sequence 3 accepted as contiguous after a REJECTED (not skipped) sequence 2", err)
	}
}

func TestEngineApplyHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) {
		t.Fatal("handler must not be called once the context is cancelled")
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := engine.Apply(ctx, envelope(1)); err == nil {
		t.Fatal("Apply() error = nil, want context error")
	}
}

func TestEngineApplyRejectsInvalidInputEnvelope(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, nil }))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	invalid := envelope(1)
	invalid.Source = "" // missing a required provenance field

	if _, err := engine.Apply(context.Background(), invalid); err == nil {
		t.Fatal("Apply() error = nil, want error")
	}
}

func TestEngineApplyFailsClosedOnInvalidEmission(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) ([]event.Envelope, error) {
		valid := decision("b")
		invalid := decision("c")
		invalid.Source = "" // missing a required provenance field
		return []event.Envelope{valid, invalid}, nil
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = engine.Apply(context.Background(), envelope(5))
	if err == nil {
		t.Fatal("Apply() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "sequence 5") {
		t.Fatalf("Apply() error = %v, want it to name the input sequence (5)", err)
	}
	if !strings.Contains(err.Error(), "emission 1") {
		t.Fatalf("Apply() error = %v, want it to name the emission index (1)", err)
	}
}

// TestEngineApplyNamesBothAnInvalidFinalEmissionAndTheHandlerError is Apply's
// own version of
// TestEngineRunNamesBothAnInvalidFinalEmissionAndTheHandlerError: a handler
// that fails closed AND returns an invalid explanatory emission must have
// both failures named, the original handler error first in the chain.
func TestEngineApplyNamesBothAnInvalidFinalEmissionAndTheHandlerError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("handler failed closed")
	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) {
		invalid := decision("x")
		invalid.Source = "" // missing a required provenance field
		return []event.Envelope{invalid}, wantErr
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = engine.Apply(context.Background(), envelope(1))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Apply() error = %v, want it to wrap the original handler error %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "sequence 1") {
		t.Errorf("Apply() error = %v, want it to name the input sequence (1)", err)
	}
	if !strings.Contains(err.Error(), "emission 0") {
		t.Errorf("Apply() error = %v, want it to name the emission index (0)", err)
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Errorf("Apply() error = %v, want it to name the emission as invalid", err)
	}
}

// TestEngineApplyLeavesNoGapInTheOutputStreamAfterADiscardedEmission pins
// the output counter's side of the same rule the cursor tests above pin for
// the input side: a call whose own emission is invalid, with no
// accompanying handler error, keeps NOTHING (Apply's own doc comment: this
// is the one case that discards the whole call), so it must not have
// consumed any of the shared output counter either — the next kept
// decision, from whatever input is accepted next (a genuine retry of this
// same envelope, or a different one this Engine goes on to accept), picks
// up immediately after the last one this Engine actually returned, with no
// number skipped in between.
func TestEngineApplyLeavesNoGapInTheOutputStreamAfterADiscardedEmission(t *testing.T) {
	t.Parallel()

	// attemptsAtTwo counts how many times sequence 2 has been delivered:
	// invalid the first two times, valid the third — modelling an operator
	// fixing whatever produced the invalid emission and redelivering the
	// identical, unchanged Sequence, which is the only kind of "retry" a
	// discarded call permits (Apply's own doc comment).
	var attemptsAtTwo int
	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) ([]event.Envelope, error) {
		switch item.Sequence {
		case 1:
			return []event.Envelope{decision("a")}, nil
		case 2:
			attemptsAtTwo++
			if attemptsAtTwo < 3 {
				invalid := decision("bad")
				invalid.Source = "" // missing a required provenance field
				return []event.Envelope{invalid}, nil
			}
			return []event.Envelope{decision("b")}, nil
		default:
			t.Fatalf("unexpected input sequence %d", item.Sequence)
			return nil, nil
		}
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	first, err := engine.Apply(context.Background(), envelope(1))
	if err != nil {
		t.Fatalf("Apply(1) error = %v", err)
	}
	if len(first) != 1 || first[0].Sequence != 1 {
		t.Fatalf("Apply(1) = %+v, want one envelope at output sequence 1", first)
	}

	if _, err := engine.Apply(context.Background(), envelope(2)); err == nil {
		t.Fatal("Apply(2) (first attempt) succeeded against an invalid emission; want an error")
	}

	// A genuine retry: sequence 2 again, unchanged, because nothing from the
	// failed call above was kept anywhere — this is checked as contiguous
	// precisely because the input cursor never moved.
	if _, err := engine.Apply(context.Background(), envelope(2)); err == nil {
		t.Fatal("Apply(2) (second attempt) succeeded; want it still invalid")
	}

	// Third delivery of the identical Sequence, now valid: its output must
	// start immediately after sequence 1's own output 1 — at 2, not 4 —
	// because neither discarded attempt consumed an output position.
	third, err := engine.Apply(context.Background(), envelope(2))
	if err != nil {
		t.Fatalf("Apply(2) (third attempt) error = %v", err)
	}
	if len(third) != 1 || third[0].Sequence != 2 {
		t.Fatalf("Apply(2) (third attempt) = %+v, want one envelope at output sequence 2, with no gap left by the two discarded attempts", third)
	}
}

// TestEngineApplyAdvancesBothCursorsWhenAPartialEmissionIsKept is the
// counterpart: when a handler error's own valid emissions are kept
// alongside an invalid final one (Handler's "may emit a final event
// explaining why" contract), something real was returned, so — unlike the
// fully-discarded case above — this envelope's Sequence is spent and the
// next call is checked against it, not retried at the same value.
func TestEngineApplyAdvancesBothCursorsWhenAPartialEmissionIsKept(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("handler failed closed")
	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) ([]event.Envelope, error) {
		switch item.Sequence {
		case 1:
			valid := decision("kept")
			invalid := decision("dropped")
			invalid.Source = "" // missing a required provenance field
			return []event.Envelope{valid, invalid}, wantErr
		case 2:
			return []event.Envelope{decision("next")}, nil
		default:
			t.Fatalf("unexpected input sequence %d", item.Sequence)
			return nil, nil
		}
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	first, err := engine.Apply(context.Background(), envelope(1))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Apply(1) error = %v, want it to wrap %v", err, wantErr)
	}
	if len(first) != 1 || first[0].ID != "kept" || first[0].Sequence != 1 {
		t.Fatalf("Apply(1) = %+v, want exactly the one valid emission kept, at output sequence 1", first)
	}

	// Sequence 1 is spent: retrying it is now a non-contiguous duplicate,
	// not a retry.
	if _, err := engine.Apply(context.Background(), envelope(1)); err == nil || !strings.Contains(err.Error(), "non-contiguous sequence") {
		t.Fatalf("Apply(1) again = %v, want non-contiguous sequence: this input's Sequence was already spent by the partial keep above", err)
	}

	second, err := engine.Apply(context.Background(), envelope(2))
	if err != nil {
		t.Fatalf("Apply(2) error = %v", err)
	}
	if len(second) != 1 || second[0].Sequence != 2 {
		t.Fatalf("Apply(2) = %+v, want one envelope at output sequence 2 (immediately after sequence 1's own kept output 1)", second)
	}
}

// The three tests below pin the cases where a nil decisions slice does NOT
// mean the input was rejected — the reading a caller would otherwise take
// from the nil alone. Each one returns a nil slice and still spends this
// input's Sequence, so a retry of the identical envelope is a duplicate,
// not a retry.
//
// Only the INPUT cursor moves in these three. Apply commits
// outputSequence as "where the kept decisions' own stamps end (unchanged if
// there were none)" (Apply's own doc comment), and none are kept here, so
// outputSequence is exactly where it was. The output side of that same rule
// has its own test:
// TestEngineApplyLeavesNoGapInTheOutputStreamAfterADiscardedEmission.
//
// The input cursor's move is proven not by inspecting it directly (it is
// unexported) but by the one externally observable consequence: the NEXT
// call is checked against the moved cursor, so a deliberately gapped
// Sequence after it is refused. Had the cursor NOT moved, that gapped call
// would instead be this Engine's first-ever, which accepts any starting
// Sequence, and would wrongly succeed.

// TestEngineApplyAdvancesTheInputCursorWhenTheHandlerDecidesNothing is the
// first case: a handler that legitimately decides nothing for this input.
func TestEngineApplyAdvancesTheInputCursorWhenTheHandlerDecidesNothing(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, nil }))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	emitted, err := engine.Apply(context.Background(), envelope(1))
	if err != nil {
		t.Fatalf("Apply(1) error = %v", err)
	}
	if emitted != nil {
		t.Fatalf("Apply(1) = %v, want nil", emitted)
	}

	if _, err := engine.Apply(context.Background(), envelope(3)); err == nil || !strings.Contains(err.Error(), "non-contiguous sequence") {
		t.Fatalf("Apply(3) after a decide-nothing Apply(1) = %v, want non-contiguous sequence: the cursor must have moved to 1 despite the nil, nil return", err)
	}
}

// TestEngineApplyConsumesNoOutputPositionWhenNothingIsKept pins the other
// half of what the three tests around it claim: a call that keeps nothing
// moves the input cursor but leaves outputSequence exactly where it was, so
// the next kept decision is output 1, not output 2. Without this, "only the
// input cursor moves" would be prose with no test behind it, and an
// implementation that bumped outputSequence per input rather than per kept
// decision would pass every other test in this group.
func TestEngineApplyConsumesNoOutputPositionWhenNothingIsKept(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) ([]event.Envelope, error) {
		if item.Sequence == 1 {
			return nil, nil
		}
		return []event.Envelope{decision("first kept")}, nil
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := engine.Apply(context.Background(), envelope(1)); err != nil {
		t.Fatalf("Apply(1) error = %v", err)
	}

	emitted, err := engine.Apply(context.Background(), envelope(2))
	if err != nil {
		t.Fatalf("Apply(2) error = %v", err)
	}
	if len(emitted) != 1 || emitted[0].Sequence != 1 {
		t.Fatalf("Apply(2) = %+v, want one envelope at output sequence 1: Apply(1) kept nothing, so it consumed no output position", emitted)
	}
}

// TestEngineApplyAdvancesTheInputCursorWhenTheHandlerErrorsWithNoEmissions is
// the second case: a plain handler error with no emissions at all.
func TestEngineApplyAdvancesTheInputCursorWhenTheHandlerErrorsWithNoEmissions(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("rejected on business grounds")
	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) ([]event.Envelope, error) {
		if item.Sequence == 1 {
			return nil, wantErr
		}
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	emitted, err := engine.Apply(context.Background(), envelope(1))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Apply(1) error = %v, want it to wrap %v", err, wantErr)
	}
	if emitted != nil {
		t.Fatalf("Apply(1) = %v, want nil", emitted)
	}

	if _, err := engine.Apply(context.Background(), envelope(3)); err == nil || !strings.Contains(err.Error(), "non-contiguous sequence") {
		t.Fatalf("Apply(3) after Apply(1)'s plain handler error = %v, want non-contiguous sequence: the cursor must have moved to 1 despite the nil, err return", err)
	}
}

// TestEngineApplyAdvancesTheInputCursorWhenTheOnlyEmissionIsInvalid is the
// third case: a handler error whose SOLE emission is itself invalid, with
// no valid siblings before it. It is easily mistaken for the
// invalid-with-valid-siblings case
// (TestEngineApplyAdvancesBothCursorsWhenAPartialEmissionIsKept, above),
// which does advance BOTH cursors because it keeps a prefix. Here nothing
// is kept, so only the input cursor moves — but move it does, and this
// input's Sequence is spent, which is what separates this case from the
// invalid-emission-with-no-handler-error case
// (TestEngineApplyFailsClosedOnInvalidEmission), where neither cursor
// moves and the identical envelope may be retried.
func TestEngineApplyAdvancesTheInputCursorWhenTheOnlyEmissionIsInvalid(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("handler failed closed")
	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) ([]event.Envelope, error) {
		if item.Sequence == 1 {
			invalid := decision("x")
			invalid.Source = "" // missing a required provenance field
			return []event.Envelope{invalid}, wantErr
		}
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	emitted, err := engine.Apply(context.Background(), envelope(1))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Apply(1) error = %v, want it to wrap %v", err, wantErr)
	}
	if emitted != nil {
		t.Fatalf("Apply(1) = %v, want nil (the sole emission was invalid, so nothing survives to be returned — but the cursor still moves)", emitted)
	}

	if _, err := engine.Apply(context.Background(), envelope(3)); err == nil || !strings.Contains(err.Error(), "non-contiguous sequence") {
		t.Fatalf("Apply(3) after Apply(1)'s sole-invalid-emission error = %v, want non-contiguous sequence: the cursor must have moved to 1", err)
	}
}

func TestNewRequiresHandler(t *testing.T) {
	t.Parallel()

	if _, err := replay.New(nil); err == nil {
		t.Fatal("New(nil) error = nil, want error")
	}
}

func TestEngineRunCollectsEmittedEnvelopesInOrder(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) ([]event.Envelope, error) {
		switch item.Sequence {
		case 4:
			return []event.Envelope{decision("a")}, nil
		case 5:
			return nil, nil
		case 6:
			return []event.Envelope{decision("b"), decision("c")}, nil
		default:
			t.Fatalf("unexpected input sequence %d", item.Sequence)
			return nil, nil
		}
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	emitted, err := engine.Run(context.Background(), []event.Envelope{envelope(4), envelope(5), envelope(6)})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var ids []string
	for _, item := range emitted {
		ids = append(ids, item.ID)
	}
	want := []string{"a", "b", "c"}
	if len(ids) != len(want) {
		t.Fatalf("emitted ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("emitted ids = %v, want %v", ids, want)
		}
	}
}

func TestEngineRunEmptyEmissionIsValid(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, nil }))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	emitted, err := engine.Run(context.Background(), []event.Envelope{envelope(1), envelope(2)})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(emitted) != 0 {
		t.Fatalf("emitted = %v, want empty", emitted)
	}
}

func TestEngineRunAssignsContiguousOutputSequenceRegardlessOfHandlerInput(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) ([]event.Envelope, error) {
		switch item.Sequence {
		case 4:
			wrong := decision("a")
			wrong.Sequence = 999
			return []event.Envelope{wrong}, nil
		case 5:
			first := decision("b")
			first.Sequence = 0
			second := decision("c")
			second.Sequence = 42
			return []event.Envelope{first, second}, nil
		default:
			t.Fatalf("unexpected input sequence %d", item.Sequence)
			return nil, nil
		}
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	emitted, err := engine.Run(context.Background(), []event.Envelope{envelope(4), envelope(5)})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(emitted) != 3 {
		t.Fatalf("len(emitted) = %d, want 3", len(emitted))
	}
	for i, item := range emitted {
		want := uint64(i + 1)
		if item.Sequence != want {
			t.Fatalf("emitted[%d].Sequence = %d, want %d", i, item.Sequence, want)
		}
	}
}

func TestEngineRunStampsCausationAndCorrelationFromInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		correlationID     string
		wantCorrelationID string
	}{
		{
			name:              "no correlation id set on input falls back to input id",
			correlationID:     "",
			wantCorrelationID: "evt-4",
		},
		{
			name:              "correlation id set on input is propagated",
			correlationID:     "corr-1",
			wantCorrelationID: "corr-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			input := envelope(4)
			input.CorrelationID = tt.correlationID

			engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) {
				return []event.Envelope{decision("a")}, nil
			}))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			emitted, err := engine.Run(context.Background(), []event.Envelope{input})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if len(emitted) != 1 {
				t.Fatalf("len(emitted) = %d, want 1", len(emitted))
			}
			if emitted[0].CausationID != "evt-4" {
				t.Fatalf("CausationID = %q, want %q", emitted[0].CausationID, "evt-4")
			}
			if emitted[0].CorrelationID != tt.wantCorrelationID {
				t.Fatalf("CorrelationID = %q, want %q", emitted[0].CorrelationID, tt.wantCorrelationID)
			}
		})
	}
}

func TestEngineRunOverridesHandlerSetCausationAndCorrelation(t *testing.T) {
	t.Parallel()

	input := envelope(4)
	input.CorrelationID = "corr-1"

	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) {
		tampered := decision("a")
		tampered.CausationID = "not-the-input"
		tampered.CorrelationID = "not-the-correlation"
		return []event.Envelope{tampered}, nil
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	emitted, err := engine.Run(context.Background(), []event.Envelope{input})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(emitted) != 1 {
		t.Fatalf("len(emitted) = %d, want 1", len(emitted))
	}
	if emitted[0].CausationID != "evt-4" {
		t.Fatalf("CausationID = %q, want %q (engine must override handler value)", emitted[0].CausationID, "evt-4")
	}
	if emitted[0].CorrelationID != "corr-1" {
		t.Fatalf("CorrelationID = %q, want %q (engine must override handler value)", emitted[0].CorrelationID, "corr-1")
	}
}

func TestEngineRunFailsClosedOnInvalidEmission(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) ([]event.Envelope, error) {
		switch item.Sequence {
		case 4:
			return []event.Envelope{decision("a")}, nil
		case 5:
			valid := decision("b")
			invalid := decision("c")
			invalid.Source = "" // missing a required provenance field
			return []event.Envelope{valid, invalid}, nil
		default:
			t.Fatalf("unexpected input sequence %d", item.Sequence)
			return nil, nil
		}
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = engine.Run(context.Background(), []event.Envelope{envelope(4), envelope(5)})
	if err == nil {
		t.Fatal("Run() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "sequence 5") {
		t.Fatalf("Run() error = %v, want it to name the input sequence (5)", err)
	}
	if !strings.Contains(err.Error(), "emission 1") {
		t.Fatalf("Run() error = %v, want it to name the emission index (1)", err)
	}
}

func TestEngineRunRejectsInvalidInputEnvelope(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, nil }))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	invalid := envelope(1)
	invalid.Source = "" // missing a required provenance field

	_, err = engine.Run(context.Background(), []event.Envelope{invalid})
	if err == nil {
		t.Fatal("Run() error = nil, want error")
	}
}

// A wrong envelope version is the same fail-closed seam as any other invalid
// input: the engine performs no version-specific handling of its own, it
// just calls Envelope.Validate() on every input before applying it (#7).
func TestEngineRunRejectsInputWithWrongEnvelopeVersion(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, nil }))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	invalid := envelope(1)
	invalid.EnvelopeVersion = event.CurrentEnvelopeVersion + 1 // produced by a newer build

	_, err = engine.Run(context.Background(), []event.Envelope{invalid})
	if err == nil || !strings.Contains(err.Error(), "newer build") {
		t.Fatalf("Run() error = %v, want it to reject the wrong envelope version", err)
	}
}

// A decision the handler emits is validated the same way, and an invalid
// emission must fail closed naming the input sequence and emission index
// (#7) even when the invalidity is a wrong envelope version.
func TestEngineRunFailsClosedOnEmissionWithWrongEnvelopeVersion(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) ([]event.Envelope, error) {
		switch item.Sequence {
		case 4:
			return []event.Envelope{decision("a")}, nil
		case 5:
			valid := decision("b")
			wrongVersion := decision("c")
			wrongVersion.EnvelopeVersion = event.CurrentEnvelopeVersion + 1 // produced by a newer build
			return []event.Envelope{valid, wrongVersion}, nil
		default:
			t.Fatalf("unexpected input sequence %d", item.Sequence)
			return nil, nil
		}
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = engine.Run(context.Background(), []event.Envelope{envelope(4), envelope(5)})
	if err == nil {
		t.Fatal("Run() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "sequence 5") {
		t.Fatalf("Run() error = %v, want it to name the input sequence (5)", err)
	}
	if !strings.Contains(err.Error(), "emission 1") {
		t.Fatalf("Run() error = %v, want it to name the emission index (1)", err)
	}
	if !strings.Contains(err.Error(), "newer build") {
		t.Fatalf("Run() error = %v, want it to name the wrong-envelope-version cause", err)
	}
}

func TestEngineRunPropagatesHandlerError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("handler failed")
	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) {
		return nil, wantErr
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = engine.Run(context.Background(), []event.Envelope{envelope(1)})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Run() error = %v, want it to wrap %v", err, wantErr)
	}
}

func TestEngineRunHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) {
		t.Fatal("handler must not be called once the context is cancelled")
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := engine.Run(ctx, []event.Envelope{envelope(1)}); err == nil {
		t.Fatal("Run() error = nil, want context error")
	}
}

// TestEngineRunJournalsPriorAndFinalEmissionsAlongsideHandlerError covers the
// engine contract change #12's review round required: a handler that fails
// closed may still emit a final event explaining why (Handler's doc
// comment), and the engine must stamp, validate, and return it — the prior
// emissions from earlier, successful Apply calls, PLUS the failing call's
// own emission — rather than discarding everything the moment Apply returns
// an error. Without this, a handler's own halt-and-explain event (e.g.
// internal/strategy's capital-safety halt) would never reach the journal:
// the run would fail closed silently at exactly the moment a reviewer most
// needs to see why.
func TestEngineRunJournalsPriorAndFinalEmissionsAlongsideHandlerError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("handler failed closed")
	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) ([]event.Envelope, error) {
		switch item.Sequence {
		case 4:
			return []event.Envelope{decision("a")}, nil
		case 5:
			return []event.Envelope{decision("b")}, wantErr
		default:
			t.Fatalf("unexpected input sequence %d", item.Sequence)
			return nil, nil
		}
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	emitted, err := engine.Run(context.Background(), []event.Envelope{envelope(4), envelope(5)})
	if err == nil {
		t.Fatal("Run() error = nil, want the handler's error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("Run() error = %v, want it to wrap %v", err, wantErr)
	}

	// Both emissions: sequence 4's "a" from before the failure, and sequence
	// 5's own "b" — the failing call's final emission — correctly sequenced
	// and stamped exactly as a successful emission would be.
	if len(emitted) != 2 {
		t.Fatalf("len(emitted) = %d, want 2 (the prior emission plus the failing call's own)", len(emitted))
	}
	if emitted[0].ID != "a" || emitted[0].Sequence != 1 {
		t.Errorf("emitted[0] = %+v, want ID a, Sequence 1", emitted[0])
	}
	if emitted[1].ID != "b" || emitted[1].Sequence != 2 {
		t.Errorf("emitted[1] = %+v, want ID b, Sequence 2", emitted[1])
	}
	if emitted[1].CausationID != "evt-5" {
		t.Errorf("emitted[1].CausationID = %q, want %q (the input that caused the failing Apply call)", emitted[1].CausationID, "evt-5")
	}
	if emitted[1].PayloadHash != event.HashPayload(emitted[1].Payload) {
		t.Error("emitted[1].PayloadHash does not attest its own payload")
	}
}

// TestEngineRunNamesBothAnInvalidFinalEmissionAndTheHandlerError covers the
// case where a handler fails closed AND the explanatory emission it returns
// is itself invalid: an emission that cannot be journalled is still an
// error, and it must not be allowed to mask the original handler error that
// caused it. Both are named, with the original error first in the chain
// (errors.Is finds it).
func TestEngineRunNamesBothAnInvalidFinalEmissionAndTheHandlerError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("handler failed closed")
	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) {
		invalid := decision("x")
		invalid.Source = "" // missing a required provenance field
		return []event.Envelope{invalid}, wantErr
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = engine.Run(context.Background(), []event.Envelope{envelope(1)})
	if err == nil {
		t.Fatal("Run() error = nil, want error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("Run() error = %v, want it to wrap the original handler error %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "handler failed closed") {
		t.Errorf("Run() error = %v, want it to name the original handler error", err)
	}
	if !strings.Contains(err.Error(), "sequence 1") {
		t.Errorf("Run() error = %v, want it to name the input sequence (1)", err)
	}
	if !strings.Contains(err.Error(), "emission 0") {
		t.Errorf("Run() error = %v, want it to name the emission index (0)", err)
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Errorf("Run() error = %v, want it to name the emission as invalid", err)
	}
}
