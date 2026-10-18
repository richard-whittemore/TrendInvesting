package restart

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// EndState is the state an approved log ends in that a fresh run cannot
// recompute from bars, read from the log's own records: what the owner's
// approval states, and what the operator is told before approving.
type EndState struct {
	// Latched is Boost's latch in force (CheckApprovedLatch; ADR 0026,
	// point 5), false for a log that never decided.
	Latched bool
	// IncidentID, State and Scope are the incident the log ends in, its
	// engine state and, Degraded, its scope, strictly ascending
	// (CheckIncidentInForce); all empty for a log that ended Normal.
	IncidentID string
	State      string
	Scope      []string
	// SaleAttempted is whether that incident's one sale back to 98% was
	// attempted, and SaleClientOrderID the attempt's order when it was sent
	// (ADR 0019, amended 2026-10-15; saleAttemptAtEnd).
	SaleAttempted     bool
	SaleClientOrderID string
	// KillSwitchEngaged is whether the log's last operator.kill-switch
	// engaged it. A fresh run starts with it released, so the operator
	// engages it again.
	KillSwitchEngaged bool
}

// ReadEndState reads the state the approved log's entries end in. A record
// that is evidence of it and cannot be decoded refuses, naming it in
// logName, rather than being read as nothing.
func ReadEndState(entries []journal.Entry, logName string) (EndState, error) {
	var end EndState
	var err error
	if end.Latched, _, _, err = latchAtEnd(entries, logName); err != nil {
		return EndState{}, err
	}
	if end.IncidentID, end.State, end.Scope, err = incidentAtEnd(entries, logName); err != nil {
		return EndState{}, err
	}
	end.Scope = slices.Sorted(slices.Values(end.Scope))
	if end.SaleAttempted, end.SaleClientOrderID, err = saleAttemptAtEnd(entries, end.IncidentID, logName); err != nil {
		return EndState{}, err
	}
	for _, entry := range entries {
		env := entry.Envelope
		if entry.Kind != journal.KindInput || env.Type != event.KillSwitchEventType {
			continue
		}
		var p event.KillSwitchPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return EndState{}, fmt.Errorf("engine: decode %s in %s: %w", env.ID, logName, err)
		}
		end.KillSwitchEngaged = p.Engaged
	}
	return end, nil
}

// saleAttemptAtEnd reports whether incident's one sale back to 98% was
// attempted in the approved log: planned under the incident's tag, withheld
// under it for a person (event.SaleWithheldPayload.Attempted), or carried in
// force as attempted by the log's own approval (ADR 0019, amended
// 2026-10-15: one attempt per incident, never retried automatically). It
// returns the attempt's client order ID when the sale was sent. A log that
// ended Normal has no incident and no attempt.
func saleAttemptAtEnd(entries []journal.Entry, incident, logName string) (attempted bool, clientOrderID string, err error) {
	if incident == "" {
		return false, "", nil
	}
	for _, entry := range entries {
		env := entry.Envelope
		switch {
		case entry.Kind == journal.KindInput && env.Type == event.OperatorRunApprovedEventType:
			// Decoded already by incidentAtEnd.
			inherited, _ := event.DecodeOperatorRunApproved(env.Payload)
			if in := inherited.IncidentInForce; in != nil && in.IncidentID == incident && *in.SaleAttempted {
				attempted, clientOrderID = true, in.SaleClientOrderID
			}
		case entry.Kind == journal.KindDecision && env.Type == event.OrderPlannedEventType:
			var p event.OrderPlannedPayload
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				return false, "", fmt.Errorf("engine: decode %s in %s: %w", env.ID, logName, err)
			}
			if p.Sale != (event.SaleFigures{}) && p.Tag == incident {
				attempted, clientOrderID = true, p.ClientOrderID
			}
		case entry.Kind == journal.KindDecision && env.Type == event.SaleWithheldEventType:
			var p event.SaleWithheldPayload
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				return false, "", fmt.Errorf("engine: decode %s in %s: %w", env.ID, logName, err)
			}
			if p.Tag == incident && p.Attempted() {
				attempted = true
			}
		}
	}
	return attempted, clientOrderID, nil
}

// CheckSaleAttempt refuses an approval that carries the approved log's
// incident in force with its one sale back to 98% stated not attempted when
// the log records an attempt: a fresh run learns of an earlier attempt only
// from the approval, and would otherwise attempt the sale twice (ADR 0019,
// amended 2026-10-15). Stating an attempt is always accepted, since the
// owner may have sold by hand. logName names the approved log in a refusal.
func CheckSaleAttempt(entries []journal.Entry, approval event.OperatorRunApprovedPayload, logName string) error {
	in := approval.IncidentInForce
	if in == nil || *in.SaleAttempted {
		return nil
	}
	attempted, clientOrderID, err := saleAttemptAtEnd(entries, in.IncidentID, logName)
	if err != nil || !attempted {
		return err
	}
	return fmt.Errorf("engine: %s records an attempt of incident %s's one sale back to 98%% (sale client order %q), but the approval states it not attempted: it is attempted once per incident and never retried automatically (ADR 0019, amended 2026-10-15)",
		logName, in.IncidentID, clientOrderID)
}

// ApprovalRequest is what the owner supplies for an approval: which log,
// who approves and when, and, to clear the incident the log ended in under
// ADR 0019's recovery procedure, that incident's ID.
type ApprovalRequest struct {
	AccountID            string
	PreviousLog          string
	PreviousLogFinalHash string
	ApprovedBy           string
	ApprovedAt           time.Time
	ClearIncident        string
}

// ProposeApproval builds the approval of a fresh run after a log ending in
// end: Boost's latch in force, and either the incident end holds carried in
// force with its state, scope and sale attempt, or, when the owner clears
// it, that incident cleared by its ID and none in force (ADR 0027, amended
// 2026-10-17; ADR 0019, amended 2026-09-24, "The state survives a
// restart"). Clearing an incident other than the one end holds is refused.
func ProposeApproval(end EndState, req ApprovalRequest) (event.OperatorRunApprovedPayload, error) {
	p := event.OperatorRunApprovedPayload{
		AccountID: req.AccountID, PreviousLog: req.PreviousLog, PreviousLogFinalHash: req.PreviousLogFinalHash,
		ApprovedBy: req.ApprovedBy, ApprovedAt: req.ApprovedAt, Boost: event.BoostStateInForce{Latched: new(end.Latched)},
	}
	switch {
	case req.ClearIncident != "" && end.IncidentID == "":
		return event.OperatorRunApprovedPayload{}, fmt.Errorf("restart: %s records no incident, so there is none to clear: it ended normal", req.PreviousLog)
	case req.ClearIncident != "" && req.ClearIncident != end.IncidentID:
		return event.OperatorRunApprovedPayload{}, fmt.Errorf("restart: %s ended %s in incident %s, so that is the incident to clear, not %s", req.PreviousLog, end.State, end.IncidentID, req.ClearIncident)
	case req.ClearIncident != "":
		p.IncidentID = req.ClearIncident
	case end.IncidentID != "":
		p.IncidentInForce = &event.IncidentInForce{
			IncidentID: end.IncidentID, State: end.State, ScopeInstrumentIDs: end.Scope,
			SaleAttempted: new(end.SaleAttempted), SaleClientOrderID: end.SaleClientOrderID,
		}
	}
	if err := p.Validate(); err != nil {
		return event.OperatorRunApprovedPayload{}, errors.Join(errors.New("restart: the approval is refused"), err)
	}
	return p, nil
}
