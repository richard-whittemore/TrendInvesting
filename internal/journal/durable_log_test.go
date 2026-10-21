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

var errSync = errors.New("the disk refused to flush")

// disk is a SyncWriter that remembers what a crash would leave behind: only
// the bytes written before the last successful Sync are durable. Every call
// is recorded in order, so a test can assert that a sync followed a write.
type disk struct {
	written  bytes.Buffer
	durable  []byte
	calls    []string
	failSync bool
	// failWriteAt, when positive, makes the write that would take the file
	// past this many bytes persist only the bytes up to it and then fail: a
	// write torn by the disk filling or the process dying mid-line.
	failWriteAt int
}

func (d *disk) Write(p []byte) (int, error) {
	d.calls = append(d.calls, "write")
	if d.failWriteAt > 0 && d.written.Len()+len(p) > d.failWriteAt {
		keep := d.failWriteAt - d.written.Len()
		d.written.Write(p[:keep])
		return keep, errWrite
	}
	return d.written.Write(p)
}

func (d *disk) Sync() error {
	d.calls = append(d.calls, "sync")
	if d.failSync {
		return errSync
	}
	d.durable = append([]byte(nil), d.written.Bytes()...)
	return nil
}

func testLogHeader() journal.LogHeader {
	return journal.NewLogHeader(testConfigurationHash, testStrategyVersion)
}

// group is one input followed by the decisions it caused, the unit the
// durable log commits.
func group(input uint64, decisions ...uint64) []journal.Entry {
	out := []journal.Entry{{Kind: journal.KindInput, Envelope: testEnvelope(input)}}
	for _, d := range decisions {
		out = append(out, journal.Entry{Kind: journal.KindDecision, Envelope: testEnvelope(d)})
	}
	return out
}

func createLog(t *testing.T, d *disk) *journal.LogWriter {
	t.Helper()
	log, err := journal.CreateLog(d, testLogHeader())
	if err != nil {
		t.Fatalf("CreateLog() error = %v", err)
	}
	return log
}

func appendGroups(t *testing.T, log *journal.LogWriter, groups ...[]journal.Entry) {
	t.Helper()
	for _, g := range groups {
		if err := log.Append(g); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
}

func recoverLog(t *testing.T, data []byte) journal.Recovery {
	t.Helper()
	recovery, err := journal.RecoverLog(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("RecoverLog() error = %v", err)
	}
	return recovery
}

func assertEntries(t *testing.T, got, want []journal.Entry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("recovered %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Kind != want[i].Kind || !bytes.Equal(event.CanonicalEnvelopeBytes(got[i].Envelope), event.CanonicalEnvelopeBytes(want[i].Envelope)) {
			t.Fatalf("entry %d = %s %s, want %s %s", i, got[i].Kind, got[i].Envelope.ID, want[i].Kind, want[i].Envelope.ID)
		}
	}
}

func joined(groups ...[]journal.Entry) []journal.Entry {
	var out []journal.Entry
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func TestCreateLogMakesTheHeaderDurableBeforeReturning(t *testing.T) {
	t.Parallel()

	d := &disk{}
	createLog(t, d)
	if strings.Join(d.calls, ",") != "write,sync" {
		t.Fatalf("calls = %v, want one write then one sync", d.calls)
	}
	recovery := recoverLog(t, d.durable)
	if recovery.Header != testLogHeader() {
		t.Fatalf("recovered header = %+v, want %+v", recovery.Header, testLogHeader())
	}
	if len(recovery.Entries) != 0 || recovery.Torn != nil {
		t.Fatalf("a log of a header alone recovered %d entries and torn tail %+v", len(recovery.Entries), recovery.Torn)
	}
}

func TestCreateLogRefusesAHeaderWithoutItsIdentity(t *testing.T) {
	t.Parallel()

	for name, header := range map[string]journal.LogHeader{
		"configuration": journal.NewLogHeader("", testStrategyVersion),
		"strategy":      journal.NewLogHeader(testConfigurationHash, ""),
	} {
		d := &disk{}
		if _, err := journal.CreateLog(d, header); err == nil {
			t.Fatalf("%s: CreateLog() accepted a header without its identity", name)
		}
		if d.written.Len() != 0 {
			t.Fatalf("%s: CreateLog() wrote %d bytes before refusing", name, d.written.Len())
		}
	}
}

// TestAppendSyncsOnceAfterTheWholeGroup: the fsync policy (ADR 0027) is one
// flush per input, after the input and every decision it caused are written.
func TestAppendSyncsOnceAfterTheWholeGroup(t *testing.T) {
	t.Parallel()

	d := &disk{}
	log := createLog(t, d)
	d.calls = nil
	appendGroups(t, log, group(1, 2, 3))
	if strings.Join(d.calls, ",") != "write,sync" {
		t.Fatalf("calls = %v, want the whole group in one write, then one sync", d.calls)
	}
	assertEntries(t, recoverLog(t, d.durable).Entries, group(1, 2, 3))
}

func TestRecoverLogReturnsEveryCommittedGroupInOrder(t *testing.T) {
	t.Parallel()

	d := &disk{}
	log := createLog(t, d)
	appendGroups(t, log, group(1, 2), group(3), group(4, 5, 6))
	recovery := recoverLog(t, d.durable)
	assertEntries(t, recovery.Entries, joined(group(1, 2), group(3), group(4, 5, 6)))
	if recovery.Torn != nil {
		t.Fatalf("a cleanly closed log reported a torn tail %+v", recovery.Torn)
	}
	if recovery.FinalRecordHash != log.FinalRecordHash() || recovery.FinalRecordHash == "" {
		t.Fatalf("recovered final hash %q, writer's %q", recovery.FinalRecordHash, log.FinalRecordHash())
	}
}

func TestAppendRefusesAGroupThatIsNotOneInputAndItsDecisions(t *testing.T) {
	t.Parallel()

	invalid := testEnvelope(2)
	invalid.ID = ""
	for name, g := range map[string][]journal.Entry{
		"empty":             nil,
		"decision first":    {{Kind: journal.KindDecision, Envelope: testEnvelope(1)}},
		"second input":      {{Kind: journal.KindInput, Envelope: testEnvelope(1)}, {Kind: journal.KindInput, Envelope: testEnvelope(2)}},
		"unknown kind":      {{Kind: journal.KindInput, Envelope: testEnvelope(1)}, {Kind: "guess", Envelope: testEnvelope(2)}},
		"invalid envelope":  {{Kind: journal.KindInput, Envelope: testEnvelope(1)}, {Kind: journal.KindDecision, Envelope: invalid}},
		"invalid input env": {{Kind: journal.KindInput, Envelope: invalid}},
	} {
		d := &disk{}
		log := createLog(t, d)
		before := d.written.Len()
		if err := log.Append(g); err == nil {
			t.Fatalf("%s: Append() accepted the group", name)
		}
		if d.written.Len() != before {
			t.Fatalf("%s: Append() wrote before refusing", name)
		}
		// A refused group is the caller's error, not the disk's: the log
		// stays usable.
		appendGroups(t, log, group(1, 2))
	}
}

func TestAFailedSyncRefusesEveryLaterAppend(t *testing.T) {
	t.Parallel()

	d := &disk{}
	log := createLog(t, d)
	appendGroups(t, log, group(1, 2))
	d.failSync = true
	err := log.Append(group(3, 4))
	if !errors.Is(err, errSync) {
		t.Fatalf("Append() error = %v, want the sync failure", err)
	}
	// A retried fsync can report success for data the kernel already
	// dropped, so the log never tries again.
	d.failSync = false
	d.calls = nil
	if err := log.Append(group(5)); err == nil {
		t.Fatal("Append() after a failed sync was accepted")
	}
	if len(d.calls) != 0 {
		t.Fatalf("Append() after a failed sync touched the disk: %v", d.calls)
	}
	assertEntries(t, recoverLog(t, d.durable).Entries, group(1, 2))
}

func TestAFailedWriteRefusesEveryLaterAppend(t *testing.T) {
	t.Parallel()

	d := &disk{}
	log := createLog(t, d)
	appendGroups(t, log, group(1, 2))
	d.failWriteAt = d.written.Len() + 10
	if err := log.Append(group(3, 4)); !errors.Is(err, errWrite) {
		t.Fatalf("Append() error = %v, want the write failure", err)
	}
	d.failWriteAt = 0
	if err := log.Append(group(5)); err == nil {
		t.Fatal("Append() after a failed write was accepted")
	}
	// What reached the disk is a committed group and a torn fragment.
	recovery := recoverLog(t, d.written.Bytes())
	assertEntries(t, recovery.Entries, group(1, 2))
	if recovery.Torn == nil || recovery.Torn.Length != 10 {
		t.Fatalf("torn tail = %+v, want the 10 bytes the failed write left", recovery.Torn)
	}
}

// logBytes is a log of the given groups, everything written and synced.
func logBytes(t *testing.T, groups ...[]journal.Entry) []byte {
	t.Helper()
	d := &disk{}
	appendGroups(t, createLog(t, d), groups...)
	return d.durable
}

// TestRecoverLogExcludesATornTail is the truncated-tail rule (ADR 0027): a
// crash part way through an Append leaves, after the last commit, some
// complete record lines of that one group and at most one final fragment
// with no newline. That tail was never committed, so none of its decisions
// was released; recovery keeps every committed group and reports the tail.
func TestRecoverLogExcludesATornTail(t *testing.T) {
	t.Parallel()

	committed := logBytes(t, group(1, 2))
	whole := logBytes(t, group(1, 2), group(3, 4, 5))
	tail := whole[len(committed):]
	lines := bytes.SplitAfter(tail, []byte("\n"))
	// lines: record 3, record 4, record 5, commit, "" (after the last newline).

	for name, cut := range map[string]int{
		"mid first record":         len(lines[0]) / 2,
		"after first record":       len(lines[0]),
		"mid second record":        len(lines[0]) + len(lines[1])/2,
		"after every record":       len(lines[0]) + len(lines[1]) + len(lines[2]),
		"mid commit":               len(lines[0]) + len(lines[1]) + len(lines[2]) + len(lines[3])/2,
		"commit without a newline": len(tail) - 1,
	} {
		torn := append(append([]byte(nil), committed...), tail[:cut]...)
		recovery := recoverLog(t, torn)
		assertEntries(t, recovery.Entries, group(1, 2))
		if recovery.Torn == nil {
			t.Fatalf("%s: no torn tail reported", name)
		}
		if recovery.Torn.Offset != int64(len(committed)) || recovery.Torn.Length != int64(cut) {
			t.Fatalf("%s: torn tail = %+v, want offset %d length %d", name, recovery.Torn, len(committed), cut)
		}
	}
}

func TestRecoverLogExcludesATailOfZeroBytes(t *testing.T) {
	t.Parallel()

	// A file extended before its data reached the disk reads back as NULs.
	committed := logBytes(t, group(1, 2))
	torn := append(append([]byte(nil), committed...), make([]byte, 4096)...)
	recovery := recoverLog(t, torn)
	assertEntries(t, recovery.Entries, group(1, 2))
	if recovery.Torn == nil || recovery.Torn.Length != 4096 {
		t.Fatalf("torn tail = %+v, want the 4096 NUL bytes", recovery.Torn)
	}
}

func assertDamage(t *testing.T, name string, data []byte) {
	t.Helper()
	_, err := journal.RecoverLog(bytes.NewReader(data))
	var damage *journal.LogDamageError
	if !errors.As(err, &damage) {
		t.Fatalf("%s: RecoverLog() error = %v, want *LogDamageError", name, err)
	}
}

// TestRecoverLogRefusesAnythingButATornTail: damage a crash cannot cause is
// refused, never trimmed away, because the bytes it would drop may be a
// committed group whose orders were already sent.
func TestRecoverLogRefusesAnythingButATornTail(t *testing.T) {
	t.Parallel()

	whole := logBytes(t, group(1, 2), group(3, 4), group(5))
	lines := bytes.SplitAfter(whole, []byte("\n"))
	// lines: header, r1, r2, c, r3, r4, c, r5, c, "".
	rejoin := func(ls [][]byte) []byte { return bytes.Join(ls, nil) }
	with := func(i int, line []byte) []byte {
		out := append([][]byte(nil), lines...)
		out[i] = line
		return rejoin(out)
	}
	without := func(i int) []byte {
		out := append([][]byte(nil), lines[:i]...)
		return rejoin(append(out, lines[i+1:]...))
	}
	edited := bytes.Replace(lines[4], []byte(`"n":3`), []byte(`"n":9`), 1)

	assertDamage(t, "an edited record before a later commit", with(4, edited))
	assertDamage(t, "an edited record in the last group", with(7, bytes.Replace(lines[7], []byte(`"n":5`), []byte(`"n":9`), 1)))
	assertDamage(t, "a missing record", without(5))
	assertDamage(t, "a missing commit", without(6))
	assertDamage(t, "a blank line", with(3, append([]byte("\n"), lines[3]...)))
	assertDamage(t, "a complete line that is not JSON at the end", append(append([]byte(nil), whole...), []byte("garbage\n")...))
	assertDamage(t, "NULs then a commit", append(append(append([]byte(nil), rejoin(lines[:7])...), make([]byte, 64)...), lines[8]...))
	assertDamage(t, "a commit naming the wrong record", with(6, bytes.Replace(lines[6], []byte(`"sequence":4`), []byte(`"sequence":3`), 1)))
	assertDamage(t, "an uncommitted second input", append(rejoin(lines[:8]), lines[7]...))
	assertDamage(t, "a decision first", rejoin(append([][]byte{lines[0]}, lines[2:]...)))
}

func TestRecoverLogRefusesALogWithoutACompleteHeader(t *testing.T) {
	t.Parallel()

	whole := logBytes(t)
	for name, data := range map[string][]byte{
		"empty":         nil,
		"torn header":   whole[:len(whole)/2],
		"not JSON":      []byte("nope\n"),
		"journal":       append(writeJournalBytes(t), '\n'),
		"newer version": bytes.Replace(whole, []byte(`"log_version":1`), []byte(`"log_version":2`), 1),
		"other chain":   bytes.Replace(whole, []byte(`canonical_log_header`), []byte(`canonical_other_header`), 1),
	} {
		if _, err := journal.RecoverLog(bytes.NewReader(data)); err == nil {
			t.Fatalf("%s: RecoverLog() accepted it", name)
		}
	}
}

func writeJournalBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := journal.Write(&buf, testHeader(), testEntries(3)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	return buf.Bytes()
}

func TestRecoverLogChainIsSeededByTheLogHeader(t *testing.T) {
	t.Parallel()

	// Re-attributing a log to another configuration breaks record 1, exactly
	// as ADR 0017's seed does for a journal.
	whole := logBytes(t, group(1, 2))
	var header map[string]any
	first := bytes.SplitN(whole, []byte("\n"), 2)
	if err := json.Unmarshal(first[0], &header); err != nil {
		t.Fatalf("decode the log header: %v", err)
	}
	header["configuration_hash"] = "sha256:another"
	encoded, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	assertDamage(t, "a re-attributed header", append(append(encoded, '\n'), first[1]...))
}

// TestARecoveredLogComposesAJournalThatVerifies: the log is not the journal,
// but a journal composed from it is an ordinary ADR 0017 journal.
func TestARecoveredLogComposesAJournalThatVerifies(t *testing.T) {
	t.Parallel()

	recovery := recoverLog(t, logBytes(t, group(1, 2), group(3, 4)))
	header, err := recovery.JournalHeader()
	if err != nil {
		t.Fatalf("JournalHeader() error = %v", err)
	}
	var buf bytes.Buffer
	if err := journal.Write(&buf, header, recovery.Entries); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	verification, err := journal.Verify(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if verification.RecordCount != 4 {
		t.Fatalf("RecordCount = %d, want 4", verification.RecordCount)
	}
	if !header.SpanStart.Equal(at(1)) || !header.SpanEnd.Equal(at(3)) {
		t.Fatalf("span = %s to %s, want the inputs' own", header.SpanStart, header.SpanEnd)
	}
}
