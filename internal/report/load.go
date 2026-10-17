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
	loaded, err := journalquery.Load("journal", raw)
	if err != nil {
		return journal.Header{}, nil, fmt.Errorf("report: %w", err)
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
		return journal.Header{}, nil, err
	}
	envelopes := make([]event.Envelope, len(loaded.Records))
	for i, r := range loaded.Records {
		envelopes[i] = r.Envelope
	}
	return loaded.Header, envelopes, nil
}
