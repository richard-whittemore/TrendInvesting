package journal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// shortDisk accepts every write but reports one byte fewer than it was
// given, with no error: io.Writer's contract makes that a failure.
type shortDisk struct{ disk }

func (d *shortDisk) Write(p []byte) (int, error) {
	n, err := d.disk.Write(p)
	return n - 1, err
}

func TestCreateLogReportsAFailureToWriteOrFlushTheHeader(t *testing.T) {
	t.Parallel()

	if _, err := journal.CreateLog(&disk{failWriteAt: 3}, testLogHeader()); !errors.Is(err, errWrite) {
		t.Fatalf("CreateLog() on a failing write = %v, want the write failure", err)
	}
	if _, err := journal.CreateLog(&disk{failSync: true}, testLogHeader()); !errors.Is(err, errSync) {
		t.Fatalf("CreateLog() on a failing flush = %v, want the flush failure", err)
	}
	header := testLogHeader()
	header.LogVersion = 2
	if _, err := journal.CreateLog(&disk{}, header); err == nil || !strings.Contains(err.Error(), "log version 2") {
		t.Fatalf("CreateLog() with another version = %v, want it named", err)
	}
}

func TestAShortWriteFailsTheLog(t *testing.T) {
	t.Parallel()

	if _, err := journal.CreateLog(&shortDisk{}, testLogHeader()); err == nil {
		t.Fatal("CreateLog() accepted a short write")
	}
}

func TestAppendReturnsTimestampEncodingErrorsWithoutWriting(t *testing.T) {
	t.Parallel()

	d := &disk{}
	log := createLog(t, d)
	before := d.written.Len()
	g := group(1, 2)
	g[1].Envelope.RecordedAt = time.Date(10000, 1, 2, 0, 0, 0, 0, time.UTC)
	err := log.Append(g)
	var marshalErr *json.MarshalerError
	if !errors.As(err, &marshalErr) {
		t.Fatalf("Append() error = %v, want the timestamp encoding error", err)
	}
	if d.written.Len() != before {
		t.Fatal("Append() wrote part of a group it could not encode")
	}
	appendGroups(t, log, group(1, 2))
}

func TestDurabilityErrorsSayWhatTheyMean(t *testing.T) {
	t.Parallel()

	failing := &disk{}
	recorder := durableRecorder(t, emittingHandler(), failing)
	failing.failSync = true
	_, err := recorder.Apply(context.Background(), testEnvelope(1))
	for _, want := range []string{"evt-1", "not durable", "none of its decisions is released", "accepts nothing further", errSync.Error()} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("durability error %q does not say %q", err, want)
		}
	}
	var failed *journal.LogFailedError
	if !errors.As(err, &failed) || !errors.Is(err, errSync) {
		t.Fatalf("durability error %v does not unwrap to the log's failure", err)
	}

	damage := (&journal.LogDamageError{Line: 4, Offset: 99, Reason: "why"}).Error()
	for _, want := range []string{"line 4", "byte 99", "why", "not the torn tail"} {
		if !strings.Contains(damage, want) {
			t.Fatalf("damage error %q does not say %q", damage, want)
		}
	}
	divergence := (&journal.ResumeDivergenceError{Input: "evt-3", Reason: "why"}).Error()
	for _, want := range []string{"evt-3", "why", "cannot continue"} {
		if !strings.Contains(divergence, want) {
			t.Fatalf("divergence error %q does not say %q", divergence, want)
		}
	}
}

func TestRecoverLogReportsAReadFailure(t *testing.T) {
	t.Parallel()

	whole := logBytes(t, group(1, 2))
	header := bytes.SplitAfter(whole, []byte("\n"))[0]
	for name, reader := range map[string]*errReader{
		"header":  {},
		"records": {prefix: append([]byte(nil), header...)},
	} {
		_, err := journal.RecoverLog(reader)
		if !errors.Is(err, errWrite) {
			t.Fatalf("%s: RecoverLog() error = %v, want the read failure", name, err)
		}
	}
}

func TestRecoverLogRefusesMalformedLines(t *testing.T) {
	t.Parallel()

	whole := logBytes(t, group(1, 2))
	lines := bytes.SplitAfter(whole, []byte("\n"))
	header := lines[0]
	commit := lines[3]
	long := append(append([]byte(`{"record":"`), bytes.Repeat([]byte("x"), 9<<20)...), []byte("\"}\n")...)

	assertDamage(t, "neither a record nor a commit", append(append([]byte(nil), header...), []byte("{}\n")...))
	assertDamage(t, "both a record and a commit", append(append([]byte(nil), header...), []byte(`{"record":{},"commit":{}}`+"\n")...))
	assertDamage(t, "a commit closing no group", append(append([]byte(nil), header...), commit...))
	twoValues := bytes.Join(append(append([][]byte(nil), lines[:3]...), bytes.TrimSuffix(commit, []byte("\n")), []byte(" {}\n")), nil)
	assertDamage(t, "two values on one line", twoValues)
	assertDamage(t, "a line longer than a record may take", append(append([]byte(nil), header...), long...))
	assertDamage(t, "an unknown field", append(append([]byte(nil), header...), []byte(`{"record":{"sequence":1,"surprise":true}}`+"\n")...))
}

func TestRecoverLogExcludesAnOverlongFragmentAsATornTail(t *testing.T) {
	t.Parallel()

	committed := logBytes(t, group(1, 2))
	torn := append(append([]byte(nil), committed...), bytes.Repeat([]byte("x"), 9<<20)...)
	recovery := recoverLog(t, torn)
	if recovery.Torn == nil || recovery.Torn.Length != 9<<20 {
		t.Fatalf("torn tail = %+v, want the whole fragment", recovery.Torn)
	}
}

func TestRecoverLogRefusesAnOlderVersion(t *testing.T) {
	t.Parallel()

	whole := logBytes(t)
	_, err := journal.RecoverLog(bytes.NewReader(bytes.Replace(whole, []byte(`"log_version":1`), []byte(`"log_version":0`), 1)))
	if err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("RecoverLog() of an older version = %v, want it refused as older", err)
	}
}

func TestJournalHeaderRefusesARecoveryItCannotStateASpanFor(t *testing.T) {
	t.Parallel()

	if _, err := (journal.Recovery{Header: testLogHeader()}).JournalHeader(); err == nil {
		t.Fatal("JournalHeader() of a recovery with no input stated a span")
	}
	recovery := journal.Recovery{Header: journal.LogHeader{}, Entries: group(1)}
	if _, err := recovery.JournalHeader(); err == nil {
		t.Fatal("JournalHeader() of a recovery without its identity stated a header")
	}
}

func TestResumeRefusesRecoveredEntriesThatAreNotGroups(t *testing.T) {
	t.Parallel()

	resumed := durableRecorder(t, emittingHandler(), &disk{})
	recovery := journal.Recovery{Header: testLogHeader(), Entries: []journal.Entry{{Kind: journal.KindDecision, Envelope: testEnvelope(1)}}}
	if err := journal.Resume(context.Background(), resumed, resumed, recovery); err == nil {
		t.Fatal("Resume() accepted a decision before any input")
	}
}

// TestResumeNamesAGroupOfTheWrongLength: a resumed handler deciding fewer or
// more than the log records is a divergence, not a partial match.
func TestResumeNamesAGroupOfTheWrongLength(t *testing.T) {
	t.Parallel()

	recovery := recoverLog(t, func() []byte {
		d := &disk{}
		driveRecorder(t, durableRecorder(t, emittingHandler(), d), testInputs(1))
		return d.durable
	}())
	at := map[string]string{"fewer": "position 3", "more": "position 4"}
	for name, handler := range map[string]replay.Handler{
		"fewer": replay.HandlerFunc(func(_ context.Context, input event.Envelope) ([]event.Envelope, error) {
			return []event.Envelope{decisionFor(input.Sequence, 0)}, nil
		}),
		"more": replay.HandlerFunc(func(_ context.Context, input event.Envelope) ([]event.Envelope, error) {
			return []event.Envelope{decisionFor(input.Sequence, 0), decisionFor(input.Sequence, 1), decisionFor(input.Sequence, 2)}, nil
		}),
	} {
		resumed := durableRecorder(t, handler, &disk{})
		err := journal.Resume(context.Background(), resumed, resumed, recovery)
		var divergence *journal.ResumeDivergenceError
		if !errors.As(err, &divergence) || !strings.Contains(divergence.Reason, at[name]) {
			t.Fatalf("%s: Resume() error = %v, want a divergence at %s", name, err, at[name])
		}
	}
}
