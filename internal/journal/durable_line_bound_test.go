package journal_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// maxLogLineBytes mirrors the bound RecoverLog reads a line under: a line's
// bytes, without its newline, may take at most this many.
const maxLogLineBytes = 8 << 20

// paddedInput is an input whose payload is a JSON string of n bytes, so a
// test can size a record line exactly.
func paddedInput(n int) journal.Entry {
	envelope := testEnvelope(1)
	envelope.Payload = json.RawMessage(`"` + strings.Repeat("a", n) + `"`)
	envelope.PayloadHash = event.HashPayload(envelope.Payload)
	return journal.Entry{Kind: journal.KindInput, Envelope: envelope}
}

// recordLineLength is how many bytes, without its newline, the first record
// line of a log holding only entry takes.
func recordLineLength(t *testing.T, entry journal.Entry) int {
	t.Helper()
	d := &disk{}
	log := createLog(t, d)
	appendGroups(t, log, []journal.Entry{entry})
	lines := bytes.SplitN(d.written.Bytes(), []byte{'\n'}, 3)
	return len(lines[1])
}

// inputWithRecordLine is an input whose record line takes exactly length
// bytes without its newline.
func inputWithRecordLine(t *testing.T, length int) journal.Entry {
	t.Helper()
	const probe = 16
	overhead := recordLineLength(t, paddedInput(probe)) - probe
	// The overhead does not depend on the payload's length, so the probe
	// sizes a line Append would refuse as exactly as one it takes.
	if longer := recordLineLength(t, paddedInput(2*probe)) - 2*probe; longer != overhead {
		t.Fatalf("record line overhead is %d bytes for one payload and %d for another", overhead, longer)
	}
	return paddedInput(length - overhead)
}

// TestALogLineAtTheReadBoundIsWrittenAndRecovered: whatever Append makes
// durable, RecoverLog must read back. A line exactly at the bound is both.
func TestALogLineAtTheReadBoundIsWrittenAndRecovered(t *testing.T) {
	t.Parallel()

	entry := inputWithRecordLine(t, maxLogLineBytes)
	if got := recordLineLength(t, entry); got != maxLogLineBytes {
		t.Fatalf("sized a record line at %d bytes, want %d", got, maxLogLineBytes)
	}
	d := &disk{}
	log := createLog(t, d)
	appendGroups(t, log, []journal.Entry{entry})

	recovery := recoverLog(t, d.durable)
	assertEntries(t, recovery.Entries, []journal.Entry{entry})
	if recovery.Torn != nil {
		t.Fatalf("Torn = %+v, want none", recovery.Torn)
	}

	// The journal composed from it at shutdown must verify too.
	header, err := recovery.JournalHeader()
	if err != nil {
		t.Fatalf("JournalHeader() error = %v", err)
	}
	var buf bytes.Buffer
	if err := journal.Write(&buf, header, recovery.Entries); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if _, err := journal.Verify(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
}

// TestAppendRefusesALineRecoveryWouldReject: a group with a line one byte
// past the bound would be committed, its decisions released, and the log
// then refused as damaged on restart. Append refuses it before writing, and
// the refusal is the caller's, so the log stays usable.
func TestAppendRefusesALineRecoveryWouldReject(t *testing.T) {
	t.Parallel()

	oversized := inputWithRecordLine(t, maxLogLineBytes+1)
	d := &disk{}
	log := createLog(t, d)
	before := d.written.Len()

	err := log.Append([]journal.Entry{oversized})
	if err == nil {
		t.Fatal("Append() accepted a line RecoverLog would refuse")
	}
	var failed *journal.LogFailedError
	if errors.As(err, &failed) {
		t.Fatalf("Append() error = %v, want a refusal that leaves the log usable", err)
	}
	if d.written.Len() != before {
		t.Fatal("Append() wrote before refusing")
	}
	appendGroups(t, log, group(1, 2))
	assertEntries(t, recoverLog(t, d.durable).Entries, group(1, 2))
}
