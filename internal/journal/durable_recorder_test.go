package journal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// runningTotal is a stateful handler: each decision states the sum of every
// input sequence it has seen, so a handler restored to the wrong state
// decides differently from the one that recorded the log.
type runningTotal struct{ total uint64 }

func (h *runningTotal) Apply(_ context.Context, input event.Envelope) ([]event.Envelope, error) {
	h.total += input.Sequence
	payload := json.RawMessage(fmt.Sprintf(`{"total":%d}`, h.total))
	return []event.Envelope{{
		ID:                fmt.Sprintf("total-after-%d", input.Sequence),
		Type:              "test.total",
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		SchemaVersion:     1,
		EventTime:         input.EventTime,
		RecordedAt:        input.RecordedAt,
		Source:            "reducer",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}}, nil
}

func durableRecorder(t *testing.T, handler replay.Handler, d *disk) *journal.Recorder {
	t.Helper()
	return journal.NewDurableRecorder(handler, createLog(t, d), journal.DefaultMaxRecords)
}

// TestADurableRecorderMakesDecisionsDurableBeforeReturningThem is
// journal-before-send (ADR 0019, amended 2026-09-24; ADR 0026 §3): Apply's
// return value is the only path a decision has to the adapter, so a crash at
// any instant after Apply returns finds every decision it returned on disk.
func TestADurableRecorderMakesDecisionsDurableBeforeReturningThem(t *testing.T) {
	t.Parallel()

	d := &disk{}
	recorder := durableRecorder(t, emittingHandler(), d)
	for _, input := range testInputs(3) {
		decisions, err := recorder.Apply(context.Background(), input)
		if err != nil {
			t.Fatalf("Apply(%s) error = %v", input.ID, err)
		}
		// The crash: nothing but what was synced survives.
		recovered := recoverLog(t, d.durable)
		for _, decision := range decisions {
			if !containsDecision(recovered.Entries, decision.ID) {
				t.Fatalf("Apply(%s) returned %s before it was durable", input.ID, decision.ID)
			}
		}
	}
	assertEntries(t, recoverLog(t, d.durable).Entries, recorder.Entries())
}

func containsDecision(entries []journal.Entry, id string) bool {
	for _, entry := range entries {
		if entry.Kind == journal.KindDecision && entry.Envelope.ID == id {
			return true
		}
	}
	return false
}

// TestADurableRecorderReleasesNothingItCouldNotMakeDurable: the crash
// between write and send seen from the other side. When the write or the
// flush fails, Apply returns no decision, so nothing exists to send, and the
// log refuses every later input rather than letting the reducer run ahead of
// its record.
func TestADurableRecorderReleasesNothingItCouldNotMakeDurable(t *testing.T) {
	t.Parallel()

	for name, fail := range map[string]func(*disk){
		"sync":  func(d *disk) { d.failSync = true },
		"write": func(d *disk) { d.failWriteAt = d.written.Len() + 5 },
	} {
		d := &disk{}
		recorder := durableRecorder(t, emittingHandler(), d)
		inputs := testInputs(3)
		if _, err := recorder.Apply(context.Background(), inputs[0]); err != nil {
			t.Fatalf("%s: Apply(first) error = %v", name, err)
		}
		fail(d)
		decisions, err := recorder.Apply(context.Background(), inputs[1])
		var durability *journal.DurabilityError
		if !errors.As(err, &durability) {
			t.Fatalf("%s: Apply() error = %v, want *DurabilityError", name, err)
		}
		if decisions != nil {
			t.Fatalf("%s: Apply() released %d decisions it could not make durable", name, len(decisions))
		}
		d.failSync, d.failWriteAt = false, 0
		decisions, err = recorder.Apply(context.Background(), inputs[2])
		if decisions != nil || !errors.As(err, &durability) {
			t.Fatalf("%s: Apply() after a durability failure = %d decisions, %v; want nothing and the failure", name, len(decisions), err)
		}
		// What a restart is guaranteed to find: the first input and its
		// decisions. Bytes written before a failed flush may also survive;
		// a group that does is durable but was never released, the same
		// state as a crash between write and send.
		assertEntries(t, recoverLog(t, d.durable).Entries, recorder.Entries()[:3])
	}
}

func TestADurableRecorderWithholdsDecisionsItRefusesToJournal(t *testing.T) {
	t.Parallel()

	// The emission ceiling and an unstampable decision each refuse to
	// journal what the handler returned; a durable recorder must not hand
	// those decisions on either.
	d := &disk{}
	recorder := journal.NewDurableRecorder(burstHandler(journal.MaxEmissionsPerInput+1, nil), createLog(t, d), journal.DefaultMaxRecords)
	decisions, err := recorder.Apply(context.Background(), testEnvelope(1))
	if err == nil || decisions != nil {
		t.Fatalf("Apply() over the ceiling = %d decisions, %v; want nothing and an error", len(decisions), err)
	}

	unstampable := replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) {
		bad := decisionFor(1, 0)
		bad.Source = ""
		return []event.Envelope{decisionFor(1, 1), bad}, nil
	})
	d = &disk{}
	recorder = journal.NewDurableRecorder(unstampable, createLog(t, d), journal.DefaultMaxRecords)
	decisions, err = recorder.Apply(context.Background(), testEnvelope(1))
	if err == nil || decisions != nil {
		t.Fatalf("Apply() with an unstampable decision = %d decisions, %v; want nothing and an error", len(decisions), err)
	}
	// What the in-memory journal holds, the log holds too.
	assertEntries(t, recoverLog(t, d.durable).Entries, recorder.Entries())
}

func TestADurableRecorderJournalsAFailingHandlersFinalDecision(t *testing.T) {
	t.Parallel()

	halt := errors.New("halted")
	d := &disk{}
	recorder := durableRecorder(t, replay.HandlerFunc(func(_ context.Context, input event.Envelope) ([]event.Envelope, error) {
		return []event.Envelope{decisionFor(input.Sequence, 0)}, halt
	}), d)
	decisions, err := recorder.Apply(context.Background(), testEnvelope(1))
	if !errors.Is(err, halt) || len(decisions) != 1 {
		t.Fatalf("Apply() = %d decisions, %v; want the halt and its explanation", len(decisions), err)
	}
	if !containsDecision(recoverLog(t, d.durable).Entries, decisions[0].ID) {
		t.Fatal("the halt's explanation was returned before it was durable")
	}
}

// interruptedRun records inputs[:crashAfter] through a durable recorder over
// a running total, then "crashes": only the synced bytes survive.
func interruptedRun(t *testing.T, inputs []event.Envelope, crashAfter int) []byte {
	t.Helper()
	d := &disk{}
	recorder := durableRecorder(t, &runningTotal{}, d)
	driveRecorder(t, recorder, inputs[:crashAfter])
	return d.durable
}

// TestARestartReproducesTheRunByteForByte: a restart reads the durable log
// (ADR 0026 §5), never recomputes from elsewhere. The recovered log's inputs
// are replayed into a fresh handler, every regenerated record is checked
// against the log, and the new log is the old one byte for byte; the run then
// continues, and its journal is the uninterrupted run's journal exactly.
func TestARestartReproducesTheRunByteForByte(t *testing.T) {
	t.Parallel()

	inputs := testInputs(8)
	old := interruptedRun(t, inputs, 5)
	recovery := recoverLog(t, old)

	d := &disk{}
	resumed := durableRecorder(t, &runningTotal{}, d)
	if err := journal.Resume(context.Background(), resumed, resumed, recovery); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if !bytes.Equal(d.durable, old) {
		t.Fatalf("the resumed log differs from the one it recovered:\n%s\nwant:\n%s", d.durable, old)
	}
	driveRecorder(t, resumed, inputs[5:])

	uninterrupted := journal.NewRecorder(&runningTotal{})
	driveRecorder(t, uninterrupted, inputs)
	if got, want := writeRecorder(t, resumed), writeRecorder(t, uninterrupted); !bytes.Equal(got, want) {
		t.Fatalf("the resumed run's journal differs from the uninterrupted run's:\n%s\nwant:\n%s", got, want)
	}
	if _, err := journal.Verify(bytes.NewReader(writeRecorder(t, resumed))); err != nil {
		t.Fatalf("Verify(resumed journal) error = %v", err)
	}
}

func TestARestartAfterATornTailResumesFromTheLastCommit(t *testing.T) {
	t.Parallel()

	inputs := testInputs(4)
	committed := interruptedRun(t, inputs, 2)
	whole := interruptedRun(t, inputs, 3)
	torn := whole[:len(committed)+(len(whole)-len(committed))/2]
	recovery := recoverLog(t, torn)
	if recovery.Torn == nil {
		t.Fatal("no torn tail reported")
	}

	d := &disk{}
	resumed := durableRecorder(t, &runningTotal{}, d)
	if err := journal.Resume(context.Background(), resumed, resumed, recovery); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if !bytes.Equal(d.durable, committed) {
		t.Fatal("the resumed log is not the committed prefix of the torn one")
	}
	// The torn input was never released, so the adapter sends it again.
	driveRecorder(t, resumed, inputs[2:])
	uninterrupted := journal.NewRecorder(&runningTotal{})
	driveRecorder(t, uninterrupted, inputs)
	if !bytes.Equal(writeRecorder(t, resumed), writeRecorder(t, uninterrupted)) {
		t.Fatal("the resumed run's journal differs from the uninterrupted run's")
	}
}

func writeRecorder(t *testing.T, recorder *journal.Recorder) []byte {
	t.Helper()
	header, err := recorder.Header(testConfigurationHash, testStrategyVersion)
	if err != nil {
		t.Fatalf("Header() error = %v", err)
	}
	var buf bytes.Buffer
	if err := journal.Write(&buf, header, recorder.Entries()); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	return buf.Bytes()
}

// TestARestartRefusesAHandlerThatDecidesDifferently: a restart whose engine
// would not have made the recorded decisions must not continue from them —
// the adapter may already have acted on what the log says.
func TestARestartRefusesAHandlerThatDecidesDifferently(t *testing.T) {
	t.Parallel()

	inputs := testInputs(4)
	recovery := recoverLog(t, interruptedRun(t, inputs, 4))

	d := &disk{}
	resumed := durableRecorder(t, &runningTotal{total: 1}, d)
	err := journal.Resume(context.Background(), resumed, resumed, recovery)
	var divergence *journal.ResumeDivergenceError
	if !errors.As(err, &divergence) {
		t.Fatalf("Resume() error = %v, want *ResumeDivergenceError", err)
	}
	if divergence.Input != inputs[0].ID {
		t.Fatalf("divergence at %q, want the first input %q", divergence.Input, inputs[0].ID)
	}
	if decisions, err := resumed.Apply(context.Background(), testEnvelope(5)); decisions != nil || err == nil {
		t.Fatal("a recorder that diverged on resume accepted another input")
	}
	if len(recoverLog(t, d.durable).Entries) != 0 {
		t.Fatal("a diverging record reached the new log")
	}
}

func TestARestartRefusesALogFromAnotherRun(t *testing.T) {
	t.Parallel()

	recovery := recoverLog(t, interruptedRun(t, testInputs(2), 2))
	other, err := journal.CreateLog(&disk{}, journal.NewLogHeader("sha256:another", testStrategyVersion))
	if err != nil {
		t.Fatalf("CreateLog() error = %v", err)
	}
	resumed := journal.NewDurableRecorder(&runningTotal{}, other, journal.DefaultMaxRecords)
	if err := journal.Resume(context.Background(), resumed, resumed, recovery); err == nil {
		t.Fatal("Resume() accepted a log recorded under another configuration")
	}
}

func TestARestartRefusesARecorderThatIsNotFresh(t *testing.T) {
	t.Parallel()

	recovery := recoverLog(t, interruptedRun(t, testInputs(2), 2))
	inMemory := journal.NewRecorder(&runningTotal{})
	if err := journal.Resume(context.Background(), inMemory, inMemory, recovery); err == nil {
		t.Fatal("Resume() accepted a recorder with no durable log")
	}
	used := durableRecorder(t, &runningTotal{}, &disk{})
	driveRecorder(t, used, testInputs(1))
	if err := journal.Resume(context.Background(), used, used, recovery); err == nil {
		t.Fatal("Resume() accepted a recorder that had already recorded")
	}
}

// TestARestartRefusesAnInputItsPathRejects: resuming goes through the same
// path the live inputs take (the contiguity check included), and an input
// that path refuses is a recovered run that cannot be rebuilt.
func TestARestartRefusesAnInputItsPathRejects(t *testing.T) {
	t.Parallel()

	recovery := recoverLog(t, interruptedRun(t, testInputs(3), 3))
	resumed := durableRecorder(t, &runningTotal{}, &disk{})
	refusing := replay.HandlerFunc(func(ctx context.Context, input event.Envelope) ([]event.Envelope, error) {
		if input.Sequence == 2 {
			return nil, errors.New("refused before the recorder")
		}
		return resumed.Apply(ctx, input)
	})
	if err := journal.Resume(context.Background(), refusing, resumed, recovery); err == nil {
		t.Fatal("Resume() accepted a run one of whose inputs was never recorded again")
	}
}

// TestARestartReproducesAHandlersRecordedFailure: an input the reducer
// refused is recorded with its error, and refusing it again on resume is the
// same run, not a divergence.
func TestARestartReproducesAHandlersRecordedFailure(t *testing.T) {
	t.Parallel()

	refused := errors.New("refused by the reducer")
	handler := func() replay.Handler {
		return replay.HandlerFunc(func(_ context.Context, input event.Envelope) ([]event.Envelope, error) {
			if input.Sequence == 2 {
				return nil, refused
			}
			return []event.Envelope{decisionFor(input.Sequence, 0)}, nil
		})
	}
	d := &disk{}
	recorder := durableRecorder(t, handler(), d)
	for _, input := range testInputs(3) {
		_, _ = recorder.Apply(context.Background(), input)
	}
	recovery := recoverLog(t, d.durable)

	again := &disk{}
	resumed := durableRecorder(t, handler(), again)
	if err := journal.Resume(context.Background(), resumed, resumed, recovery); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if !bytes.Equal(again.durable, d.durable) {
		t.Fatal("the resumed log differs from the one it recovered")
	}
}
