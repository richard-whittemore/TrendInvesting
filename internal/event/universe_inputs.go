package event

import (
	"errors"
	"fmt"
	"time"
)

// CloseHistoryEventType identifies one ranked universe member's split- and
// dividend-adjusted closes over the momentum window ending at a decision
// session. With one market.universe.snapshot per session it is the momentum
// run's compact universe input: the run ranks the snapshot (ADR 0026, point
// 1, "Universe": ranking is a rule, so it lives in Go) and reads history for
// the members it ranked only, never a bar per listed stock per session.
const CloseHistoryEventType = "market.close.history"

// CloseHistorySchemaVersion is the current schema version of CloseHistoryPayload.
const CloseHistorySchemaVersion uint32 = 1

// CloseHistoryPayload is one instrument's adjusted closes up to and including
// Session. A session of the window with no usable close is absent, which the
// momentum rule treats as a missing bar (ADR 0026, point 5). The closes are
// adjusted forward from AdjustedBase exactly as CompletedBarAdjustedPayload's
// Adjusted view is, so they compare with that instrument's bars on the same
// base.
type CloseHistoryPayload struct {
	InstrumentID string `json:"instrument_id"`
	// Session is the decision session the history ends at, as YYYY-MM-DD:
	// the session of the universe snapshot it was ranked from.
	Session      string         `json:"session"`
	AdjustedBase string         `json:"adjusted_base"`
	Closes       []SessionClose `json:"closes"`
}

// SessionClose is one session's split- and dividend-adjusted close.
type SessionClose struct {
	Session string  `json:"session"`
	Close   float64 `json:"close"`
}

// Validate checks the instrument, the session and base, and that the closes
// are finite, positive and dated strictly ascending from the base to the
// session: a close after the session would be look-ahead.
func (p CloseHistoryPayload) Validate() error {
	var errs []error
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	_, sessionErr := time.Parse(time.DateOnly, p.Session)
	if sessionErr != nil {
		errs = append(errs, fmt.Errorf("session %q is not a YYYY-MM-DD date", p.Session))
	}
	_, baseErr := time.Parse(time.DateOnly, p.AdjustedBase)
	switch {
	case baseErr != nil:
		errs = append(errs, fmt.Errorf("adjusted base %q is not a YYYY-MM-DD date", p.AdjustedBase))
	case sessionErr == nil && p.AdjustedBase > p.Session:
		errs = append(errs, fmt.Errorf("adjusted base %s is after the session %s", p.AdjustedBase, p.Session))
	}
	for i, c := range p.Closes {
		if _, err := time.Parse(time.DateOnly, c.Session); err != nil {
			errs = append(errs, fmt.Errorf("close %d: session %q is not a YYYY-MM-DD date", i+1, c.Session))
			continue
		}
		switch {
		case i > 0 && c.Session <= p.Closes[i-1].Session:
			errs = append(errs, fmt.Errorf("closes must be strictly ascending; %s follows %s", c.Session, p.Closes[i-1].Session))
		case c.Session > p.Session:
			errs = append(errs, fmt.Errorf("close %d is dated %s, after the session %s: look-ahead", i+1, c.Session, p.Session))
		case c.Session < p.AdjustedBase:
			errs = append(errs, fmt.Errorf("close %d is dated %s, before the adjusted base %s", i+1, c.Session, p.AdjustedBase))
		}
		if !isFinite(c.Close) || c.Close <= 0 {
			errs = append(errs, fmt.Errorf("close %d must be finite and positive", i+1))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid close history payload: %w", err)
	}
	return nil
}

// SessionRetractedEventType identifies the removal of one session from the
// recorded calendar: the exchange cancelled it. A retracted session is not a
// session, so it is no gap in any instrument's closes and no decision time
// (ADR 0026, points 1 and 5), and a later calendar that still lists it does
// not contradict the one held.
const SessionRetractedEventType = "market.session.retracted"

// SessionRetractedSchemaVersion is the current schema version of SessionRetractedPayload.
const SessionRetractedSchemaVersion uint32 = 1

// SessionRetractedPayload names the session withdrawn, who recorded the
// withdrawal, and why.
type SessionRetractedPayload struct {
	Source  string `json:"source"`
	Session string `json:"session"`
	Reason  string `json:"reason"`
}

// Validate checks the source, the session date and the reason.
func (p SessionRetractedPayload) Validate() error {
	var errs []error
	if p.Source == "" {
		errs = append(errs, errors.New("source is required"))
	}
	if _, err := time.Parse(time.DateOnly, p.Session); err != nil {
		errs = append(errs, fmt.Errorf("session %q is not a YYYY-MM-DD date", p.Session))
	}
	if p.Reason == "" {
		errs = append(errs, errors.New("a reason is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid session retracted payload: %w", err)
	}
	return nil
}
