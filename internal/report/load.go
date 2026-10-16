package report

import (
	"bytes"
	"fmt"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// Load verifies a journal's chain, content and identity (ADR 0017) and
// returns its envelopes in recording order, inputs and decisions together. A
// journal that does not verify is refused, never partly reported: a summary
// of tampered evidence would read as true.
func Load(raw []byte) (journal.Header, []event.Envelope, error) {
	header, records, err := journal.Read(bytes.NewReader(raw))
	if err != nil {
		return journal.Header{}, nil, fmt.Errorf("report: the journal does not verify: %w", err)
	}
	if _, err := journal.Verify(bytes.NewReader(raw)); err != nil {
		return journal.Header{}, nil, fmt.Errorf("report: the journal does not verify: %w", err)
	}
	if err := journal.CheckIdentity(header, records); err != nil {
		return journal.Header{}, nil, err
	}
	envelopes := make([]event.Envelope, len(records))
	for i, r := range records {
		envelopes[i] = r.Envelope
	}
	return header, envelopes, nil
}
