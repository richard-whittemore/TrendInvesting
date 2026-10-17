// Package journalquery answers questions about what a run did, from the
// journals it left behind: the orders planned, released and refused, the
// fills with their quantities, prices and commissions, the positions those
// fills build, and the refusals and engine-state changes along the way.
//
// It is read-only by construction: every function takes bytes already read
// and returns a table, so nothing here can write to a journal or reach an
// engine. A journal is trusted only after journal.Verify accepts it; a file
// whose hash chain does not verify is refused whole, naming the record where
// it breaks, and no figure from it is reported (ADR 0017). Any index a caller
// builds from these tables is a disposable copy, never a second source of
// truth.
package journalquery

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// Journal is one verified journal: its name (shown beside every row it
// contributes), its header, and its records in sequence order.
type Journal struct {
	Name    string
	Header  journal.Header
	Records []journal.Record
}

// Load verifies data as a journal and returns it. The chain is checked before
// anything else is trusted, so a tampered or truncated-in-the-middle file is
// refused with the record at which it breaks.
func Load(name string, data []byte) (Journal, error) {
	if isDurableLog(data) {
		return loadDurableLog(name, data)
	}
	if _, err := journal.Verify(bytes.NewReader(data)); err != nil {
		var broken *journal.ChainBrokenError
		if errors.As(err, &broken) {
			return Journal{}, fmt.Errorf("%s: refused, the hash chain does not verify: %w", name, err)
		}
		return Journal{}, fmt.Errorf("%s: refused, the journal does not verify: %w", name, err)
	}
	// Verify has just parsed these same bytes, so Read cannot fail on them.
	header, records, _ := journal.Read(bytes.NewReader(data))
	return Journal{Name: name, Header: header, Records: records}, nil
}

// distinct refuses journals that share a record. A resumed run's log begins
// with every committed record of the run it resumes, so naming both would
// report that history twice; the tool will not guess which copy to keep, and
// asks for one file per run instead. Records are the same when their
// envelope id, type and payload hash are.
func distinct(journals []Journal) error {
	type id struct{ id, typ, hash string }
	seen := map[id]int{}
	for n, j := range journals {
		own := map[id]bool{}
		for _, r := range j.Records {
			k := id{r.Envelope.ID, r.Envelope.Type, r.Envelope.PayloadHash}
			if first, ok := seen[k]; ok && first != n {
				return fmt.Errorf("%s and %s overlap: both hold record %q (%s), as a resumed run's log and the run it resumed do; name one file per run so no record is counted twice", journals[first].Name, j.Name, k.id, k.typ)
			}
			own[k] = true
		}
		for k := range own {
			seen[k] = n
		}
	}
	return nil
}
