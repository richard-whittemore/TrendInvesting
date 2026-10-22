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
	// Verification is what journal.Verify reported about an end-of-run
	// journal (stop, completeness, final record hash); nil for a durable
	// log, whose Recovery holds the equivalent.
	Verification *journal.Verification
}

// Load verifies data as a journal and returns it. The chain is checked before
// anything else is trusted, so a tampered or truncated-in-the-middle file is
// refused with the record at which it breaks.
func Load(name string, data []byte) (Journal, error) {
	if isDurableLog(data) {
		return loadDurableLog(name, data)
	}
	verification, err := journal.Verify(bytes.NewReader(data))
	if err != nil {
		var broken *journal.ChainBrokenError
		if errors.As(err, &broken) {
			return Journal{}, fmt.Errorf("%s: refused, the hash chain does not verify: %w", name, err)
		}
		return Journal{}, fmt.Errorf("%s: refused, the journal does not verify: %w", name, err)
	}
	// Verify has just parsed these same bytes, so Read cannot fail on them.
	header, records, _ := journal.Read(bytes.NewReader(data))
	return Journal{Name: name, Header: header, Records: records, Verification: &verification}, nil
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

// FinalRecordHash is the hash of the journal's last record, or empty when the
// journal holds no record. Only an end-of-run journal's final hash is what a
// run registry anchors (ADR 0017); a durable log's chain is its own (ADR
// 0027).
func (j Journal) FinalRecordHash() string {
	switch {
	case j.Verification != nil:
		return j.Verification.FinalRecordHash
	case j.Recovery != nil:
		return j.Recovery.FinalRecordHash
	}
	return ""
}

// CheckFinalHash compares the journal's final record hash with the one a run
// registry recorded. A journal that has lost records from its end still
// verifies by chain alone, so this comparison is what detects it (ADR 0017).
func (j Journal) CheckFinalHash(want string) error {
	if want == "" {
		return fmt.Errorf("%s: no anchor given: an absent registry entry is not agreement with a final record hash", j.Name)
	}
	if got := j.FinalRecordHash(); got != want {
		return fmt.Errorf("%s: the final record hash is %q but %q was recorded, so the file is not the run's complete history", j.Name, got, want)
	}
	return nil
}

// Incomplete says in words why the journal cannot stand as the whole of a
// run, or returns "" when nothing shows it cut short: a journal or log that
// holds no committed record, a durable log whose uncommitted tail a crash
// or a read in mid-write left behind (ADR 0027), or input recorded after the
// adapter stopped the run (ADR 0012).
func (j Journal) Incomplete() string {
	if len(j.Records) == 0 {
		return fmt.Sprintf("%s holds no committed record", j.Name)
	}
	if j.Recovery != nil && j.Recovery.Torn != nil {
		return fmt.Sprintf("%s ends after record %d (byte %d), followed by %d bytes not committed (yet): if the engine is still writing, read it again once it has finished; otherwise the log was cut short", j.Name, len(j.Records), j.Recovery.Torn.Offset, j.Recovery.Torn.Length)
	}
	if after := j.inputAfterStop(); after != "" {
		return fmt.Sprintf("%s records input (%s) after the run was stopped, so it is not one run's complete history", j.Name, after)
	}
	return ""
}

// inputAfterStop names the first input recorded after the adapter stopped the
// run, other than the one run-completed record the reducer allows (ADR 0012),
// or returns "". An end-of-run journal's verification states it; a durable
// log has no verification, so its records are read the same way.
func (j Journal) inputAfterStop() string {
	if j.Verification != nil {
		return j.Verification.InputAfterStop
	}
	stopped, completed := false, false
	for _, r := range j.Records {
		if r.Kind != journal.KindInput {
			continue
		}
		switch {
		case stopped && r.Envelope.Type == event.RunCompletedEventType && !completed:
			completed = true
		case stopped:
			return r.Envelope.Type
		case r.Envelope.Type == event.AdapterRunStoppedEventType:
			stopped = true
		}
	}
	return ""
}
