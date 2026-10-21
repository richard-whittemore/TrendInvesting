package restart

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	// engine state and, Degraded, the instruments it freezes, strictly
	// ascending (CheckIncidentInForce); all empty for a log that ended
	// Normal.
	IncidentID string
	State      string
	Scope      []string
	// ScopeCash is, Degraded, the cash components the incident freezes (a
	// reconciliation's unexplained cash, ADR 0019, amended 2026-09-24),
	// strictly ascending; empty otherwise.
	ScopeCash []string
	// SaleAttempted is whether that incident's one sale back to 98% was
	// attempted, and SaleClientOrderID the attempt's order when it was sent
	// (ADR 0019, amended 2026-10-15; saleAttemptAtEnd).
	SaleAttempted     bool
	SaleClientOrderID string
	// KillSwitchEngaged is whether the log's last operator.kill-switch the
	// gate accepted, one for the log's own account that validates and states
	// no time before the last accepted, engaged it; a kill switch for another
	// account, or backdated before one accepted, was refused and changed
	// nothing (risklimit.Gate.SetKillSwitch). A fresh run starts with it released,
	// so the operator engages it again.
	KillSwitchEngaged bool
	// PendingCash is every cash difference the log reported as a pending
	// cash read and never decided, in the order reported (pendingCashAtEnd).
	// A fresh run starts with none pending, so its approval must state the
	// starting book the owner checked in IBKR, which supersedes them (ADR
	// 0019, amended 2026-10-21).
	PendingCash []PendingCashRead
}

// PendingCashRead is one undecided pending cash difference: the
// reconciliation input that found it, its reconciliation, and the cash
// residual, in whole cents, in Currency.
type PendingCashRead struct {
	InputID          string
	ReconciliationID string
	Residual         int64
	Currency         string
}

// ReadEndState reads the state the approved log's entries, recorded for
// accountID, end in. A record that is evidence of it and cannot be decoded
// refuses, naming it in logName, rather than being read as nothing.
func ReadEndState(entries []journal.Entry, accountID, logName string) (EndState, error) {
	var end EndState
	var err error
	if end.Latched, _, _, err = latchAtEnd(entries, logName); err != nil {
		return EndState{}, err
	}
	if end.IncidentID, end.State, end.Scope, end.ScopeCash, err = incidentAtEnd(entries, logName); err != nil {
		return EndState{}, err
	}
	end.Scope = slices.Sorted(slices.Values(end.Scope))
	end.ScopeCash = slices.Sorted(slices.Values(end.ScopeCash))
	if end.SaleAttempted, end.SaleClientOrderID, err = saleAttemptAtEnd(entries, end.IncidentID, logName); err != nil {
		return EndState{}, err
	}
	if end.PendingCash, err = pendingCashAtEnd(entries, logName); err != nil {
		return EndState{}, err
	}
	var lastKillAt time.Time
	for _, entry := range entries {
		env := entry.Envelope
		if entry.Kind != journal.KindInput || env.Type != event.KillSwitchEventType {
			continue
		}
		var p event.KillSwitchPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return EndState{}, fmt.Errorf("engine: decode %s in %s: %w", env.ID, logName, err)
		}
		// The gate refuses one stating a time before the last it applied
		// (risklimit.Gate.SetKillSwitch).
		if p.AccountID == accountID && p.Validate() == nil && !p.AsOf.Before(lastKillAt) {
			end.KillSwitchEngaged, lastKillAt = p.Engaged, p.AsOf
		}
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
			// Decoded already by incidentAtEnd, at its recorded version.
			inherited, _ := event.DecodeRecordedOperatorRunApproved(env.Payload, env.SchemaVersion)
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
// who approves and when, to clear the incident the log ended in under
// ADR 0019's recovery procedure, that incident's ID, or, for a sale made by
// hand that the log does not record, SaleAttempted.
type ApprovalRequest struct {
	AccountID            string
	PreviousLog          string
	PreviousLogFinalHash string
	ApprovedBy           string
	ApprovedAt           time.Time
	ClearIncident        string
	// SaleAttempted states the incident held in force as having had its one
	// sale back to 98% attempted, for a sale the owner made by hand that the
	// log does not record.
	SaleAttempted bool
	// StartingBook is whether the approval states the starting book the
	// owner checked in IBKR (-positions, -cash, -currency), which supersedes
	// a pending cash difference the log leaves undecided.
	StartingBook bool
}

// ProposeApproval builds the approval of a fresh run after a log ending in
// end: Boost's latch in force, and either the incident end holds carried in
// force with its state, scope (instruments and cash components) and sale
// attempt, or, when the owner clears it, that incident cleared by its ID and
// none in force (ADR 0027, amended 2026-10-17; ADR 0019, amended
// 2026-09-24, "The state survives a restart"). Clearing an incident other
// than the one end holds is refused. req.SaleAttempted states an unrecorded
// hand sale as the incident's one attempt, carried attempted with the
// recorded one; it is refused with ClearIncident, on a log ending Normal,
// and where end already records the attempt (ADR 0019, amended 2026-10-15).
// A log that leaves a pending cash difference undecided is refused unless
// req states the starting book (ADR 0019, amended 2026-10-21).
func ProposeApproval(end EndState, req ApprovalRequest) (event.OperatorRunApprovedPayload, error) {
	p := event.OperatorRunApprovedPayload{
		AccountID: req.AccountID, PreviousLog: req.PreviousLog, PreviousLogFinalHash: req.PreviousLogFinalHash,
		ApprovedBy: req.ApprovedBy, ApprovedAt: req.ApprovedAt, Boost: event.BoostStateInForce{Latched: new(end.Latched)},
	}
	switch {
	case req.SaleAttempted && req.ClearIncident != "":
		return event.OperatorRunApprovedPayload{}, errors.New("restart: -sale-attempted states the sale of an incident left in force, and -clear-incident leaves none in force: use one or the other")
	case req.SaleAttempted && end.IncidentID == "":
		return event.OperatorRunApprovedPayload{}, fmt.Errorf("restart: %s records no incident, so there is no sale to state as attempted: it ended normal", req.PreviousLog)
	case req.SaleAttempted && end.SaleAttempted:
		return event.OperatorRunApprovedPayload{}, fmt.Errorf("restart: %s already records an attempt of incident %s's one sale back to 98%%, so the approval carries it attempted without -sale-attempted", req.PreviousLog, end.IncidentID)
	case len(end.PendingCash) > 0 && !req.StartingBook:
		return event.OperatorRunApprovedPayload{}, pendingRefusal(req.PreviousLog, end.PendingCash)
	case req.ClearIncident != "" && end.IncidentID == "":
		return event.OperatorRunApprovedPayload{}, fmt.Errorf("restart: %s records no incident, so there is none to clear: it ended normal", req.PreviousLog)
	case req.ClearIncident != "" && req.ClearIncident != end.IncidentID:
		return event.OperatorRunApprovedPayload{}, fmt.Errorf("restart: %s ended %s in incident %s, so that is the incident to clear, not %s", req.PreviousLog, end.State, end.IncidentID, req.ClearIncident)
	case req.ClearIncident != "":
		p.IncidentID = req.ClearIncident
	case end.IncidentID != "":
		p.IncidentInForce = &event.IncidentInForce{
			IncidentID: end.IncidentID, State: end.State, ScopeInstrumentIDs: end.Scope, ScopeCashComponents: end.ScopeCash,
			SaleAttempted: new(end.SaleAttempted || req.SaleAttempted), SaleClientOrderID: end.SaleClientOrderID,
		}
	}
	if err := p.Validate(); err != nil {
		return event.OperatorRunApprovedPayload{}, errors.Join(errors.New("restart: the approval is refused"), err)
	}
	return p, nil
}

// pendingCashAtEnd returns every cash difference the approved log reported
// as a pending cash read (a risk.reconciliation.result classified
// pending-cash-read) with no later result deciding it: one for the same
// reconciliation input, explained or unexplained (risklimit's settlement).
// A result that cannot be decoded refuses, naming it in logName.
func pendingCashAtEnd(entries []journal.Entry, logName string) ([]PendingCashRead, error) {
	var pending []PendingCashRead
	for _, entry := range entries {
		env := entry.Envelope
		if entry.Kind != journal.KindDecision || env.Type != event.ReconciliationResultEventType {
			continue
		}
		var p event.ReconciliationResultPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil, fmt.Errorf("engine: decode %s in %s: %w", env.ID, logName, err)
		}
		if p.Classification != event.ReconciliationPendingCashRead {
			pending = slices.DeleteFunc(pending, func(pc PendingCashRead) bool { return pc.InputID == p.InputID })
			continue
		}
		pc := PendingCashRead{InputID: p.InputID, ReconciliationID: p.ReconciliationID, Currency: p.Currency}
		for _, c := range p.Components {
			if c.Component == event.ReconciliationComponentCash {
				pc.Residual = c.Residual
			}
		}
		pending = append(pending, pc)
	}
	return pending, nil
}

// pendingRefusal states why a fresh run's approval without a starting book
// is refused while pending names an undecided cash difference (ADR 0019,
// amended 2026-10-21).
func pendingRefusal(logName string, pending []PendingCashRead) error {
	named := make([]string, 0, len(pending))
	for _, pc := range pending {
		sign, c := "", pc.Residual
		if c < 0 {
			sign, c = "-", -c
		}
		named = append(named, fmt.Sprintf("reconciliation %s (input %s) found %s%d.%02d %s", pc.ReconciliationID, pc.InputID, sign, c/100, c%100, pc.Currency))
	}
	return fmt.Errorf("restart: %s ends with a cash difference pending a reading of IBKR's statement and not yet decided: %s; a fresh run starts with none pending, so it would be forgotten: wait until the Flex reader's records explain it or it is decided, or state the book you checked in IBKR after the transfer with -positions, -cash and -currency, which supersedes it (ADR 0019, amended 2026-10-21)",
		logName, strings.Join(named, "; "))
}

// CheckPendingCash refuses an approval that states no starting book when the
// approved log leaves a pending cash difference undecided: a fresh run starts
// with none pending, so the difference would be forgotten; the starting book
// the owner checked in IBKR supersedes it (ADR 0019, amended 2026-10-21).
// logName names the approved log in a refusal.
func CheckPendingCash(entries []journal.Entry, approval event.OperatorRunApprovedPayload, logName string) error {
	pending, err := pendingCashAtEnd(entries, logName)
	if err != nil || len(pending) == 0 || approval.StartingBook != nil {
		return err
	}
	return pendingRefusal(logName, pending)
}
