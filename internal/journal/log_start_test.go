package journal_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// TestRecoverLogStartReadsOnlyTheFirstGroups: a reader that needs only a
// log's first records (its configuration and approval) recovers those
// groups, checked as RecoverLog checks them, and reads no further than a
// buffer past them, however long the log is.
func TestRecoverLogStartReadsOnlyTheFirstGroups(t *testing.T) {
	d := &disk{}
	log := createLog(t, d)
	groups := [][]journal.Entry{group(1), group(2, 3)}
	for n := uint64(4); n < 2000; n++ {
		groups = append(groups, group(n))
	}
	appendGroups(t, log, groups...)
	whole := recoverLog(t, d.durable)

	counted := &countingReader{r: bytes.NewReader(d.durable)}
	start, err := journal.RecoverLogStart(counted, 2)
	if err != nil {
		t.Fatal(err)
	}
	assertEntries(t, start.Entries, joined(groups[0], groups[1]))
	if start.Header != whole.Header || start.Torn != nil {
		t.Fatalf("start = %+v", start)
	}
	if counted.n >= len(d.durable)/10 {
		t.Fatalf("read %d of %d bytes for the first two groups", counted.n, len(d.durable))
	}

	short, err := journal.RecoverLogStart(bytes.NewReader(d.durable), len(groups)+5)
	if err != nil || len(short.Entries) != len(whole.Entries) || short.FinalRecordHash != whole.FinalRecordHash {
		t.Fatalf("a log with fewer groups: %d entries, err = %v; want the whole log", len(short.Entries), err)
	}
	if _, err := journal.RecoverLogStart(bytes.NewReader([]byte("not a log\n")), 2); err == nil {
		t.Fatal("a file that is not a log must refuse")
	}
}
