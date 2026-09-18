package journal_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// A journal is the only durable evidence a run leaves, so the two ways it can
// fail to be one — a write that did not land, and a file that cannot be read
// back — have to be reported rather than absorbed. A short write that
// returned nil would leave a truncated file that verifies as far as it goes,
// which is the worst possible shape: evidence that looks complete.

// errWrite is what every writer below fails with.
var errWrite = errors.New("the underlying writer failed")

// failingWriter fails every write. Write buffers its lines, so the failure
// surfaces at the first flush: on the header line when the header alone
// exceeds the buffer, and otherwise once enough records have accumulated.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

// errReader yields prefix and then fails, so a reader can be made to fail
// at a chosen line rather than only at the first.
type errReader struct {
	prefix []byte
	read   bool
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.read || len(r.prefix) == 0 {
		return 0, errWrite
	}
	n := copy(p, r.prefix)
	r.prefix = r.prefix[n:]
	if len(r.prefix) == 0 {
		r.read = true
	}
	return n, nil
}

func TestWriteReportsAFailureOnTheHeaderLine(t *testing.T) {
	t.Parallel()

	// A header larger than bufio's buffer is written straight through, so the
	// failure lands on the header line itself rather than on a later flush.
	header := journal.NewHeader(testConfigurationHash, strings.Repeat("v", 8<<10), at(1), at(3))

	err := journal.Write(failingWriter{}, header, testEntries(3))
	if !errors.Is(err, errWrite) {
		t.Fatalf("Write() error = %v, want the writer's own failure", err)
	}
	if !strings.Contains(err.Error(), "write line") {
		t.Errorf("error = %v, want it to say which operation failed", err)
	}
}

func TestWriteReportsAFailureOnARecordLine(t *testing.T) {
	t.Parallel()

	// Write returns an I/O failure rather than absorbing it, because a
	// truncated journal is a valid chain as far as it goes: ADR 0017 records
	// end-truncation as the gap tamper-evidence does not close. The header
	// fits in the buffer, so the first failure can only come from the
	// records: enough of them to force a flush. testEntries numbers its
	// inputs oddly, so 64 of them span day 1 to day 63.
	header := journal.NewHeader(testConfigurationHash, testStrategyVersion, at(1), at(63))
	err := journal.Write(failingWriter{}, header, testEntries(64))
	if !errors.Is(err, errWrite) {
		t.Fatalf("Write() error = %v, want the writer's own failure", err)
	}
}

func TestWriteRefusesAHeaderClaimingADifferentChainAlgorithm(t *testing.T) {
	t.Parallel()

	header := testHeader()
	header.ChainAlgorithm = "sha256(whatever)"

	err := journal.Write(&bytes.Buffer{}, header, testEntries(1))
	if err == nil {
		t.Fatal("Write() error = nil, want a header naming an algorithm this build does not compute to be refused")
	}
	if !strings.Contains(err.Error(), "chain algorithm") {
		t.Errorf("error = %v, want it to name the chain algorithm", err)
	}
}

func TestReadRefusesAnEmptyFile(t *testing.T) {
	t.Parallel()

	_, _, err := journal.Read(strings.NewReader(""))
	if err == nil {
		t.Fatal("Read() error = nil, want an empty file to be refused")
	}
	if !strings.Contains(err.Error(), "begins with a header line") {
		t.Errorf("error = %v, want it to say what a journal begins with", err)
	}
}

func TestReadReportsAFailureReadingTheHeaderLine(t *testing.T) {
	t.Parallel()

	_, _, err := journal.Read(&errReader{})
	if !errors.Is(err, errWrite) {
		t.Fatalf("Read() error = %v, want the reader's own failure", err)
	}
	if !strings.Contains(err.Error(), "read header") {
		t.Errorf("error = %v, want it to distinguish a read failure from an empty file", err)
	}
}

func TestReadReportsAFailurePartWayThroughTheRecords(t *testing.T) {
	t.Parallel()

	written := writeJournal(t)
	firstLine := bytes.IndexByte(written, '\n') + 1

	_, _, err := journal.Read(&errReader{prefix: written[:firstLine]})
	if !errors.Is(err, errWrite) {
		t.Fatalf("Read() error = %v, want the reader's own failure", err)
	}
	if strings.Contains(err.Error(), "read header") {
		t.Errorf("error = %v, want the header to have been read before the failure", err)
	}
}

func TestReadRefusesAHeaderLineThatIsNotJSON(t *testing.T) {
	t.Parallel()

	_, _, err := journal.Read(strings.NewReader("this is not a header\n"))
	if err == nil {
		t.Fatal("Read() error = nil, want an undecodable header to be refused")
	}
	if !strings.Contains(err.Error(), "decode header") {
		t.Errorf("error = %v, want it to name the header", err)
	}
}

// TestReadSkipsBlankLinesBetweenRecords keeps a trailing newline, or a line
// ending a tool introduced while moving the file, from being read as a record
// that fails to decode. A blank line carries nothing and chains nothing.
func TestReadSkipsBlankLinesBetweenRecords(t *testing.T) {
	t.Parallel()

	written := writeJournal(t)
	spaced := bytes.ReplaceAll(written, []byte("\n"), []byte("\n\n"))

	_, records, err := journal.Read(bytes.NewReader(spaced))
	if err != nil {
		t.Fatalf("Read() error = %v, want blank lines to be skipped", err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d record(s), want the 3 the journal holds", len(records))
	}
}

// TestVerifyRefusesAJournalWithNoRecords is the header-only file: a run that
// started, wrote its identity, and recorded nothing. Reporting it as a
// verified journal with a final hash of the header alone would make an empty
// run indistinguishable from a complete one.
func TestVerifyRefusesAJournalWithNoRecords(t *testing.T) {
	t.Parallel()

	written := writeJournal(t)
	headerOnly := written[:bytes.IndexByte(written, '\n')+1]

	_, err := journal.Verify(bytes.NewReader(headerOnly))
	if err == nil {
		t.Fatal("Verify() error = nil, want a journal with no records to be refused")
	}
	if !strings.Contains(err.Error(), "records no events at all") {
		t.Errorf("error = %v, want it to say the journal records nothing", err)
	}
}
