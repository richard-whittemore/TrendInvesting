package journal

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// ResumeDivergenceError reports a restart whose handler, given a recorded
// input, did not reproduce the records the durable log holds for it. The
// run cannot continue from that log: an adapter may already have acted on
// what the log says was decided (ADR 0026 §5; ADR 0027).
type ResumeDivergenceError struct {
	Input  string
	Reason string
}

func (e *ResumeDivergenceError) Error() string {
	return fmt.Sprintf("journal: resuming from the durable log, event %s did not reproduce what the log records for it: %s; the run cannot continue from this log", e.Input, e.Reason)
}

// compareGroup reports the first difference between the group a durable log
// recorded for input and the group a resumed run produced, or nil. Records
// are compared by kind and canonical envelope bytes, the same definition of
// "the same envelope" the chain hashes over (ADR 0016; ADR 0017).
func compareGroup(input string, want, got []Entry) *ResumeDivergenceError {
	for i := 0; i < len(want) || i < len(got); i++ {
		switch {
		case i >= len(got):
			return &ResumeDivergenceError{Input: input, Reason: fmt.Sprintf("the log records %s %s at position %d of the group, and the resumed run decided nothing there", want[i].Kind, want[i].Envelope.ID, i+1)}
		case i >= len(want):
			return &ResumeDivergenceError{Input: input, Reason: fmt.Sprintf("the resumed run decided %s at position %d of the group, beyond what the log records", got[i].Envelope.ID, i+1)}
		case want[i].Kind != got[i].Kind || !bytes.Equal(event.CanonicalEnvelopeBytes(want[i].Envelope), event.CanonicalEnvelopeBytes(got[i].Envelope)):
			return &ResumeDivergenceError{Input: input, Reason: fmt.Sprintf("position %d of the group: the log records %s %s, the resumed run produced %s %s", i+1, want[i].Kind, want[i].Envelope.ID, got[i].Kind, got[i].Envelope.ID)}
		}
	}
	return nil
}

// Resume rebuilds a run from its durable log: it applies each recovered
// input through through — the path live inputs take, which ends at recorder
// — and requires recorder to reproduce, record for record, the group the
// log holds for it, writing each into recorder's own new log. State is read
// back from the log, never recomputed from anything else (ADR 0026 §5).
//
// recorder must be a fresh durable Recorder whose log was created under the
// recovered log's header, so a log recorded with the same handler resumes
// into a byte-identical log. The recovered log itself is never written: it
// stays the evidence it is, torn tail included.
//
// A handler error the recorded run also met is part of the run and does not
// stop the resume; an input that is not recorded again, or a group that
// differs, does. Decisions regenerated here are not released again: what a
// log holds may already have been sent, and matching it against the broker
// is reconciliation's job (ADR 0019).
func Resume(ctx context.Context, through replay.Handler, recorder *Recorder, recovery Recovery) error {
	if recorder.log == nil {
		return errors.New("journal: resume requires a durable recorder, so the resumed run keeps a durable log")
	}
	if len(recorder.entries) != 0 || recorder.failed != nil {
		return errors.New("journal: resume requires a recorder that has recorded nothing yet")
	}
	if recorder.log.Header() != recovery.Header {
		return fmt.Errorf("journal: the durable log was recorded by run %s / %s, not this run %s / %s; a log resumes only into the run that recorded it",
			recovery.Header.ConfigurationHash, recovery.Header.StrategyVersion, recorder.log.Header().ConfigurationHash, recorder.log.Header().StrategyVersion)
	}
	groups, err := groupsOf(recovery.Entries)
	if err != nil {
		return err
	}
	for _, group := range groups {
		input := group[0].Envelope
		recorder.expected = group
		_, applyErr := through.Apply(ctx, input)
		var divergence *ResumeDivergenceError
		var durability *DurabilityError
		switch {
		case errors.As(applyErr, &divergence), errors.As(applyErr, &durability):
			return applyErr
		case recorder.expected != nil:
			recorder.expected = nil
			recorder.failed = &ResumeDivergenceError{Input: input.ID, Reason: "the input was not recorded again"}
			return errors.Join(recorder.failed, applyErr)
		}
	}
	return nil
}

// groupsOf splits recovered entries into the groups the log committed them
// in: each input with the decisions that follow it.
func groupsOf(entries []Entry) ([][]Entry, error) {
	var groups [][]Entry
	for i, entry := range entries {
		switch {
		case entry.Kind == KindInput:
			groups = append(groups, []Entry{entry})
		case len(groups) == 0:
			return nil, fmt.Errorf("journal: recovered entry %d is a decision before any input", i+1)
		default:
			groups[len(groups)-1] = append(groups[len(groups)-1], entry)
		}
	}
	return groups, nil
}
