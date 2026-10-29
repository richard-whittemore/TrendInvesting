package report

import (
	"fmt"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/journalquery"
)

// Load verifies a journal's chain, content and identity (ADR 0017) and
// returns its envelopes in recording order, inputs and decisions together. It
// reads both forms a run leaves: the end-of-run journal, and the durable log
// a live engine streams (ADR 0027), of which only committed groups are
// reported. Reading is journalquery.Load's, so the reporter and the query
// tool never disagree about what a file holds. A file that does not verify is
// refused, never partly reported: a summary of tampered evidence would read as
// true.
func Load(raw []byte) (journal.Header, []event.Envelope, error) {
	header, entries, err := LoadEntries(raw)
	if err != nil {
		return journal.Header{}, nil, err
	}
	envelopes := make([]event.Envelope, len(entries))
	for i, e := range entries {
		envelopes[i] = e.Envelope
	}
	return header, envelopes, nil
}

// LoadEntries is Load keeping what each record was in the run, an input or
// a decision, which an order's lifecycle state is read from
// (OrderLifecycles).
func LoadEntries(raw []byte) (journal.Header, []journal.Entry, error) {
	header, entries, _, err := loadEntries(raw)
	return header, entries, err
}

// LoadLog is Load that also returns a durable log's recovery, nil for an
// end-of-run journal, so a caller that follows the chain of runs back from
// the log (EarlierRunsOfRecovery) reads and recovers it once.
func LoadLog(raw []byte) ([]event.Envelope, *journal.Recovery, error) {
	_, entries, recovery, err := loadEntries(raw)
	if err != nil {
		return nil, nil, err
	}
	envelopes := make([]event.Envelope, len(entries))
	for i, e := range entries {
		envelopes[i] = e.Envelope
	}
	return envelopes, recovery, nil
}

// loadEntries is LoadEntries returning the durable log's recovery too.
func loadEntries(raw []byte) (journal.Header, []journal.Entry, *journal.Recovery, error) {
	loaded, err := journalquery.Load("journal", raw)
	if err != nil {
		return journal.Header{}, nil, nil, fmt.Errorf("report: %w", err)
	}
	// Every record must name the run its file's header names (ADR 0017's
	// identity rule). Recovering a durable log checks its chain and commits
	// but not this, so a log is checked against the identity its header
	// names, exactly as a journal is.
	identity := loaded.Header
	if identity.JournalVersion == 0 {
		identity = journal.Header{ConfigurationHash: loaded.LogHeader.ConfigurationHash, StrategyVersion: loaded.LogHeader.StrategyVersion}
	}
	if err := journal.CheckIdentity(identity, loaded.Records); err != nil {
		return journal.Header{}, nil, nil, err
	}
	entries := make([]journal.Entry, len(loaded.Records))
	for i, r := range loaded.Records {
		entries[i] = journal.Entry{Kind: r.Kind, Envelope: r.Envelope}
	}
	return loaded.Header, entries, loaded.Recovery, nil
}

// LoadEntriesWithRecovery keeps committed record kinds and the durable recovery
// from the same verified read (ADR 0017; ADR 0027), for lifecycle-aware views.
func LoadEntriesWithRecovery(raw []byte) (journal.Header, []journal.Entry, *journal.Recovery, error) {
	return loadEntries(raw)
}
