package journalquery

import (
	"bytes"
	"fmt"

	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// isDurableLog reports whether data begins with a durable-log header, which
// names a log_version where a journal header names a journal_version.
func isDurableLog(data []byte) bool {
	first, _, _ := bytes.Cut(data, []byte("\n"))
	return bytes.Contains(first, []byte(`"log_version"`))
}

// loadDurableLog reads the live engine's durable log (ADR 0027) with the
// journal package's own reader, journal.RecoverLog, which verifies the chain
// and every commit. Only committed groups are reported: a torn tail is the
// uncommitted end of a crashed run and holds no figure to trust. The result
// is the Journal the same entries would make, so every report treats the two
// forms alike. A log that is damaged (not merely torn) is refused whole.
func loadDurableLog(name string, data []byte) (Journal, error) {
	recovery, err := journal.RecoverLog(bytes.NewReader(data))
	if err != nil {
		return Journal{}, fmt.Errorf("%s: refused, the durable log does not verify: %w", name, err)
	}
	// A log's header states no span (ADR 0027), so Header stays zero.
	loaded := Journal{Name: name, LogHeader: recovery.Header}
	// A log's sequence is contiguous from 1 and RecoverLog has checked it.
	for i, entry := range recovery.Entries {
		loaded.Records = append(loaded.Records, journal.Record{Sequence: uint64(i + 1), Kind: entry.Kind, Envelope: entry.Envelope})
	}
	return loaded, nil
}
