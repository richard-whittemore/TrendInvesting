package journalquery

import (
	"bytes"
	"errors"
	"fmt"
)

// ErrDurableLogUnsupported is returned for a file in the durable-log form
// (ADR 0027). The log reader, journal.RecoverLog, is not on this branch's
// base; loadDurableLog is the one place to wire it in, returning a Journal
// built from the recovered committed groups. Until then the form is detected
// and refused by name rather than misread as a journal.
var ErrDurableLogUnsupported = errors.New("the durable log form (ADR 0027) is not readable yet")

// isDurableLog reports whether data begins with a durable-log header, which
// names a log_version where a journal header names a journal_version.
func isDurableLog(data []byte) bool {
	first, _, _ := bytes.Cut(data, []byte("\n"))
	return bytes.Contains(first, []byte(`"log_version"`))
}

// loadDurableLog is the seam for the durable log reader.
func loadDurableLog(name string, _ []byte) error {
	return fmt.Errorf("%s: %w", name, ErrDurableLogUnsupported)
}
