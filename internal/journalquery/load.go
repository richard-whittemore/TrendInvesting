// Package journalquery answers questions about what a run did, from the
// journals it left behind: the orders planned, released and refused, the
// fills with their quantities, prices and commissions, the positions those
// fills build, and the refusals and engine-state changes along the way.
//
// It is read-only by construction: every function takes bytes already read
// and returns a table, except the directory listing (ListLogs), which only
// reads a directory's entries, so nothing here can write to a journal or
// reach an engine. A journal is trusted only after journal.Verify accepts it; a file
// whose hash chain does not verify is refused whole, naming the record where
// it breaks, and no figure from it is reported (ADR 0017). Any index a caller
// builds from these tables is a disposable copy, never a second source of
// truth.
package journalquery

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// Journal is one verified journal: its name (shown beside every row it
// contributes), its header, and its records in sequence order. A durable log
// (ADR 0027) has no journal header, so Header stays zero and LogHeader holds
// the run identity its header names; for an end-of-run journal LogHeader is
// zero.
type Journal struct {
	Name      string
	Header    journal.Header
	LogHeader journal.LogHeader
	Records   []journal.Record
	// Recovery is a durable log's recovery, nil for an end-of-run journal,
	// so a reader that needs the log's final record hash does not recover
	// the log a second time.
	Recovery *journal.Recovery
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

// distinct refuses journals that hold one run's history twice, which a
// resumed run's log does (it begins with every committed record of the run it
// resumes) and so does a second copy of one journal. Counting both would
// report every fill twice, and the tool will not guess which copy to keep.
//
// The rule is about run history, not about records that merely look alike.
// Two journals are the same history when, over the sequence numbers both
// hold, every record is identical (same sequence, kind, envelope id, type,
// payload hash, event and recorded times, strategy version and configuration
// hash) and at least one such record is not a configuration record.
// Configuration records are left out of the test because independent runs of
// one configuration all begin with the same one, so sharing it says nothing
// about history. Requiring the whole overlap to match, not one record, keeps
// two independent runs apart when a record or two happens to coincide, and
// the times keep apart records that repeat an id and payload at another
// moment. Records one journal holds past the other's end are the new part of
// a resumed run and are not compared.
func distinct(journals []Journal) error {
	for i, a := range journals {
		for _, b := range journals[i+1:] {
			if sameHistory(a, b) {
				return fmt.Errorf("%s and %s overlap: over the sequences both hold they record the same history, as a resumed run's log and the run it resumed do, or two copies of one journal; name one file per run so no record is counted twice", a.Name, b.Name)
			}
		}
	}
	return nil
}

// identity is what must agree for two records to be the same record.
type identity struct {
	kind, id, typ, hash, strategy, configuration string
	eventTime, recordedAt                        int64
}

func identify(r journal.Record) identity {
	e := r.Envelope
	return identity{r.Kind, e.ID, e.Type, e.PayloadHash, e.StrategyVersion, e.ConfigurationHash, e.EventTime.UnixNano(), e.RecordedAt.UnixNano()}
}

func isConfiguration(r journal.Record) bool {
	return r.Envelope.Type == event.ConfigurationEventType || r.Envelope.Type == event.TargetWeightConfigurationEventType
}

// sameHistory reports whether a and b agree on every sequence both hold and
// share at least one record that is not configuration.
func sameHistory(a, b Journal) bool {
	held := map[uint64]journal.Record{}
	for _, r := range b.Records {
		held[r.Sequence] = r
	}
	shared := false
	for _, r := range a.Records {
		other, ok := held[r.Sequence]
		if !ok {
			continue
		}
		if isConfiguration(r) && isConfiguration(other) {
			continue
		}
		if identify(r) != identify(other) {
			return false
		}
		shared = true
	}
	return shared
}
