package event

import (
	"errors"
	"fmt"
	"time"
)

// SessionClosedEventType identifies the input that ends a Session
// (CONTEXT.md: "Session"): every completed bar sharing one period end has
// been sent, so the day's Adds and entries can be decided across the whole
// universe in ADR 0010's order (ADR 0021). Namespaced "market.", like
// CompletedBarEventType: it is a fact about the market data the producer
// delivered, not a decision.
const SessionClosedEventType = "market.session.closed"

// SessionClosedSchemaVersion is the current schema version of
// SessionClosedPayload.
const SessionClosedSchemaVersion uint32 = 1

// SessionClosedPayload names the Session it ends and every instrument whose
// completed bar for that Session precedes it in the stream (ADR 0021). The
// set, rather than a count, lets the reducer name the instrument that is
// missing or extra when it disagrees with what was received, and lets a
// journal reader see what the Session covered.
type SessionClosedPayload struct {
	PeriodEnd time.Time `json:"period_end"`
	// InstrumentIDs is sorted strictly ascending: one canonical encoding per
	// set, so two producers stating the same Session write the same bytes.
	InstrumentIDs []string `json:"instrument_ids"`
}

// Validate checks that the payload names a writable period end and a
// non-empty, strictly ascending set of non-empty instrument IDs.
func (p SessionClosedPayload) Validate() error {
	var errs []error
	switch {
	case p.PeriodEnd.IsZero():
		errs = append(errs, errors.New("period end is required"))
	case !writableTime(p.PeriodEnd):
		errs = append(errs, errors.New("period end cannot be written as RFC 3339"))
	}
	if len(p.InstrumentIDs) == 0 {
		errs = append(errs, errors.New("a session names at least one instrument: a session without a bar has nothing to close"))
	}
	for i, id := range p.InstrumentIDs {
		if id == "" {
			errs = append(errs, fmt.Errorf("instrument id %d is empty", i+1))
			continue
		}
		if i > 0 && id <= p.InstrumentIDs[i-1] {
			errs = append(errs, fmt.Errorf("instrument ids must be strictly ascending; %q follows %q", id, p.InstrumentIDs[i-1]))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid session closed payload: %w", err)
	}
	return nil
}
