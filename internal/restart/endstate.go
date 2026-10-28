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
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// EndState is the state an approved log ends in that a fresh run cannot
// recompute from bars, read from the log's own records: what the owner's
// approval states, and what the operator is told before approving.
type EndState struct {
	// Latched is Boost's latch in force (CheckApprovedLatch; ADR 0026,
	// point 5), false for a log that never decided and for a momentum log.
	Latched bool
	// Momentum is whether the log is a momentum run's (IsMomentumLog), and
	// Rebalance the rebalance it ends with in progress, nil when none
	// (RebalanceAtEnd; ADR 0026, point 5).
	Momentum  bool
	Rebalance *event.RebalanceInProgress
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
	// SaleRetiredAttempts is how many of that incident's sales were retired
	// as never sent, by a retirement the engine applied, counting those an
	// earlier approval carried (saleAttemptAtEnd): each had a client order
	// ID of its own, which the fresh run's next attempt must not reuse.
	SaleRetiredAttempts int
	// SaleSettlements are the adapter's settlements of the incident's sales
	// the log released and the adapter never sent that the engine's
	// retirement rule applies (saleAttemptAtEnd), each counted in
	// SaleRetiredAttempts: the evidence the approval carries.
	SaleSettlements []event.NeverSentSettlement
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
// accountID, end in, with the adapter's settlements of the orders that log
// released and the adapter never sent (ReadSettlements): those of another
// log are left out, and each of logName's is applied after the log's last
// record, since its run had ended (saleAttemptAtEnd). A record that is
// evidence of it and cannot be decoded, and a settlement the log does not
// bear out, refuses, naming it in logName, rather than being read as
// nothing.
func ReadEndState(entries []journal.Entry, accountID, logName string, settlements ...event.NeverSentSettlement) (EndState, error) {
	var end EndState
	var err error
	if end.Momentum = IsMomentumLog(entries); end.Momentum {
		if end.Rebalance, err = RebalanceAtEnd(entries, accountID, logName); err != nil {
			return EndState{}, err
		}
	} else if end.Latched, _, _, err = latchAtEnd(entries, logName); err != nil {
		return EndState{}, err
	}
	if end.IncidentID, end.State, end.Scope, end.ScopeCash, err = incidentAtEnd(entries, logName); err != nil {
		return EndState{}, err
	}
	end.Scope = slices.Sorted(slices.Values(end.Scope))
	end.ScopeCash = slices.Sorted(slices.Values(end.ScopeCash))
	var own []event.NeverSentSettlement
	for _, s := range settlements {
		if s.Log == logName {
			own = append(own, s)
		}
	}
	sale, err := saleAttemptAtEnd(entries, end.IncidentID, accountID, logName, own)
	if err != nil {
		return EndState{}, err
	}
	end.SaleAttempted, end.SaleClientOrderID, end.SaleRetiredAttempts, end.SaleSettlements = sale.attempted, sale.clientOrderID, sale.retired, sale.settled
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

// saleAttempt is what saleAttemptAtEnd reads of an incident's one sale back
// to 98%: whether it was attempted, the attempt's client order ID when it
// was sent, how many of the incident's sales were retired, and the
// settlements of never-sent sales among those retirements.
type saleAttempt struct {
	attempted     bool
	clientOrderID string
	retired       int
	settled       []event.NeverSentSettlement
}

// saleAttemptAtEnd reports whether incident's one sale back to 98% was
// attempted in the approved log, recorded for accountID: planned under the
// incident's tag, withheld under it for a person
// (event.SaleWithheldPayload.Attempted), or carried in force as attempted by
// the log's own approval (ADR 0019, amended 2026-10-15: one attempt per
// incident, never retried automatically). A sale retired as never sent
// after it was planned is no attempt, since nothing was sent (ADR 0019,
// amended 2026-10-17, reading 4, as the owner's decision of 2026-10-22
// reads it), but only by a retirement the engine applies (retirements):
// one it refuses leaves the attempt spent (fail closed, AGENTS.md). So is
// a sale settled, after the log's run ended, by the adapter's settlement
// that it never sent it (event.NeverSentSettlement), which the run's engine
// could never be told: each of settlements is applied after the log's last
// record by the same rule, and must name the input whose committed reply
// released its order; any it does not bear out refuses, naming it, since a
// settlement is the owner's statement, never silently dropped (fail
// closed). It returns the attempt's client order ID when the sale was sent,
// and how many of the incident's sales were retired, counting those the
// log's own approval carried, so the next attempt's client order ID is never
// a retired order's (ADR 0026, point 2, "Order identity"), with the
// settlements that retired the incident's sales. A log that ended Normal has
// no incident and no attempt; its settlements are still checked.
func saleAttemptAtEnd(entries []journal.Entry, incident, accountID, logName string, settlements []event.NeverSentSettlement) (saleAttempt, error) {
	if incident == "" && len(settlements) == 0 {
		return saleAttempt{}, nil
	}
	decode := func(env event.Envelope, v any) error {
		if err := json.Unmarshal(env.Payload, v); err != nil {
			return fmt.Errorf("engine: decode %s in %s: %w", env.ID, logName, err)
		}
		return nil
	}
	var a saleAttempt
	sales := map[string]bool{} // the incident's planned sales, by client order ID
	input := ""                // the input whose decisions the entry is among
	r := newRetirements(accountID)
	for _, entry := range entries {
		env := entry.Envelope
		if entry.Kind == journal.KindInput {
			input = env.ID
		}
		switch {
		case entry.Kind == journal.KindInput && env.Type == event.OperatorRunApprovedEventType:
			// Decoded already by incidentAtEnd, at its recorded version.
			inherited, _ := event.DecodeRecordedOperatorRunApproved(env.Payload, env.SchemaVersion)
			if in := inherited.IncidentInForce; in != nil && incident != "" && in.IncidentID == incident {
				a.attempted, a.clientOrderID, a.retired = *in.SaleAttempted, in.SaleClientOrderID, in.SaleRetiredAttempts
			}
		case entry.Kind == journal.KindDecision && env.Type == event.OrderPlannedEventType:
			var p event.OrderPlannedPayload
			if err := decode(env, &p); err != nil {
				return saleAttempt{}, err
			}
			if p.Sale != (event.SaleFigures{}) && incident != "" && p.Tag == incident {
				a.attempted, a.clientOrderID = true, p.ClientOrderID
				sales[p.ClientOrderID] = true
			}
		case entry.Kind == journal.KindDecision && env.Type == event.SaleWithheldEventType:
			var p event.SaleWithheldPayload
			if err := decode(env, &p); err != nil {
				return saleAttempt{}, err
			}
			if incident != "" && p.Tag == incident && p.Attempted() {
				a.attempted = true
			}
		case entry.Kind == journal.KindInput && env.Type == event.OrderRetiredEventType:
			var p event.OrderRetiredPayload
			if err := decode(env, &p); err != nil {
				return saleAttempt{}, err
			}
			if r.retire(env.ID, p) != nil || !sales[p.ClientOrderID] {
				continue
			}
			a.retireSale(p.ClientOrderID)
		default:
			if err := r.observe(entry, input, decode); err != nil {
				return saleAttempt{}, err
			}
		}
	}
	for _, s := range settlements {
		id := s.Retirement.ClientOrderID
		if err := s.Validate(); err != nil {
			return saleAttempt{}, fmt.Errorf("restart: the adapter's settlement for %s is refused: %w", logName, err)
		}
		if err := r.retire("never-sent:"+s.InputID+":"+id, s.Retirement); err != nil {
			return saleAttempt{}, fmt.Errorf("restart: the adapter's settlement of order %s as never sent is refused: %s does not bear it out: %w", id, logName, err)
		}
		if r.releasedBy[id] != s.InputID {
			return saleAttempt{}, fmt.Errorf("restart: the adapter's settlement of order %s as never sent names input %s, but in %s input %s released it: the settlement is not of that release", id, s.InputID, logName, r.releasedBy[id])
		}
		if sales[id] {
			a.retireSale(id)
			a.settled = append(a.settled, s)
		}
	}
	return a, nil
}

// retireSale counts the incident's sale id retired as never sent: it spent
// nothing, so it was not an attempt (ADR 0019, amended 2026-10-17, reading
// 4, as the owner's decision of 2026-10-22 reads it).
func (a *saleAttempt) retireSale(id string) {
	a.retired++
	if a.attempted && id == a.clientOrderID {
		a.attempted, a.clientOrderID = false, ""
	}
}

// retirements reads a log's retirements of the incident's sales back to 98%
// by the engine's own rule (targetweight.RetirementVerdict), against what
// the log records of each sale as the engine reads it: its first release,
// the retirements applied, and the venue's reports, executions and broker
// listings naming it that the engine reads (ADR 0026, point 2, "Boost",
// amended 2026-10-22, and point 5: recovered, never recomputed). No
// clearance the engine applies names a sale: it clears only an order its
// Boost count holds as awaiting a decided session's verdict
// (targetweight.BoostVerdicts.ClearNeverSent), which a sale never is, so
// neither a clearance nor the count is read.
type retirements struct {
	account  string
	released map[string]event.OrderReleasedPayload
	// releasedBy names the input among whose decisions each order's first
	// release is recorded.
	releasedBy map[string]string
	retired    map[string]bool // retirements applied
	named      map[string]bool // named by a venue as the engine reads it: it was sent
}

func newRetirements(account string) *retirements {
	return &retirements{account: account, released: map[string]event.OrderReleasedPayload{}, releasedBy: map[string]string{}, retired: map[string]bool{}, named: map[string]bool{}}
}

// observe records what entry says of an order a retirement may name, as the
// engine reads it: a lifecycle report of its account, at the version the
// engine reads, describing the order as released (event's Describes); an
// execution of its account; and a broker snapshot of its account listing
// the order under its instrument. A report or execution on an order retired
// already contradicts the retirement and is refused, naming nothing. A
// record that cannot be decoded refuses, through decode. input is the
// input whose decisions entry is among, recorded with an order's first
// release.
func (r *retirements) observe(entry journal.Entry, input string, decode func(event.Envelope, any) error) error {
	env := entry.Envelope
	switch {
	case entry.Kind == journal.KindDecision && env.Type == event.OrderReleasedEventType:
		var p event.OrderReleasedPayload
		if err := decode(env, &p); err != nil {
			return err
		}
		if _, ok := r.released[p.ClientOrderID]; !ok {
			r.released[p.ClientOrderID], r.releasedBy[p.ClientOrderID] = p, input
		}
	case entry.Kind != journal.KindInput:
	case env.Type == event.OrderLifecycleEventType:
		var p event.LimitOrderLifecyclePayload
		if err := decode(env, &p); err != nil {
			return err
		}
		if o, ok := r.released[p.ClientOrderID]; ok && env.SchemaVersion == event.OrderLifecycleLimitVersion && p.AccountID == r.account && !r.retired[p.ClientOrderID] && p.Describes(o) {
			r.named[p.ClientOrderID] = true
		}
	case env.Type == event.FillEventType:
		var p event.TargetWeightFillPayload
		if err := decode(env, &p); err != nil {
			return err
		}
		if _, ok := r.released[p.ClientOrderID]; ok && p.AccountID == r.account && !r.retired[p.ClientOrderID] {
			r.named[p.ClientOrderID] = true
		}
	case env.Type == event.BrokerSnapshotEventType:
		var p struct {
			AccountID  string `json:"account_id"`
			OpenOrders []struct {
				ClientOrderID string `json:"client_order_id"`
				InstrumentID  string `json:"instrument_id"`
			} `json:"open_orders"`
		}
		if err := decode(env, &p); err != nil {
			return err
		}
		for _, l := range p.OpenOrders {
			if o, ok := r.released[l.ClientOrderID]; ok && p.AccountID == r.account && l.ClientOrderID != "" && o.InstrumentID == l.InstrumentID {
				r.named[l.ClientOrderID] = true
			}
		}
	}
	return nil
}

// retire applies retirement p, journalled as input inputID, when the engine
// applies it by its own rule (targetweight.RetirementVerdict), recording it,
// and otherwise returns the engine's refusal. A retirement the engine may
// have refused is never taken as applied (fail closed, AGENTS.md).
func (r *retirements) retire(inputID string, p event.OrderRetiredPayload) error {
	_, err := targetweight.RetirementVerdict(targetweight.RetirementState{
		AccountID: r.account,
		Released:  func(id string) (event.OrderReleasedPayload, bool) { o, ok := r.released[id]; return o, ok },
		Retired:   func(id string) bool { return r.retired[id] },
		Named:     func(id string) bool { return r.named[id] },
	}, inputID, p)
	if err != nil {
		return err
	}
	r.retired[p.ClientOrderID] = true
	return nil
}

// CheckSaleAttempt refuses an approval that carries the approved log's
// incident in force with its one sale back to 98% stated not attempted when
// the log records an attempt: a fresh run learns of an earlier attempt only
// from the approval, and would otherwise attempt the sale twice (ADR 0019,
// amended 2026-10-15), and one stating fewer of the incident's sales
// retired as never sent than the log records, since the next attempt would
// reuse a retired order's client order ID (ADR 0026, point 2, "Order
// identity"). The adapter's settlements of never-sent sales the approval
// carries are read as ReadEndState reads them, by the engine's own rule:
// one the log does not bear out, or that is of no sale of the incident,
// refuses the approval (fail closed, AGENTS.md). Stating an attempt is
// always accepted, since the owner may have sold by hand. logName names the
// approved log in a refusal.
func CheckSaleAttempt(entries []journal.Entry, approval event.OperatorRunApprovedPayload, logName string) error {
	in := approval.IncidentInForce
	if in == nil || *in.SaleAttempted && len(in.SaleSettlements) == 0 {
		return nil
	}
	sale, err := saleAttemptAtEnd(entries, in.IncidentID, approval.AccountID, logName, in.SaleSettlements)
	if err != nil {
		return err
	}
	if len(sale.settled) != len(in.SaleSettlements) {
		var named []string
		for _, s := range in.SaleSettlements {
			if !slices.ContainsFunc(sale.settled, func(t event.NeverSentSettlement) bool {
				return t.Retirement.ClientOrderID == s.Retirement.ClientOrderID
			}) {
				named = append(named, s.Retirement.ClientOrderID)
			}
		}
		return fmt.Errorf("engine: the approval carries the settlement of order(s) %s as never sent, which %s does not record as a sale of incident %s: an approval carries only the incident's own sales (ADR 0019, amended 2026-10-15)",
			strings.Join(named, ", "), logName, in.IncidentID)
	}
	switch {
	case *in.SaleAttempted:
		return nil
	case !sale.attempted && in.SaleRetiredAttempts < sale.retired:
		return fmt.Errorf("engine: %s records %d of incident %s's sales back to 98%% retired as never sent, but the approval states %d: each had a client order ID of its own, which the next attempt must not reuse (ADR 0026, point 2, \"Order identity\")",
			logName, sale.retired, in.IncidentID, in.SaleRetiredAttempts)
	case !sale.attempted:
		return nil
	}
	return fmt.Errorf("engine: %s records an attempt of incident %s's one sale back to 98%% (sale client order %q), but the approval states it not attempted: it is attempted once per incident and never retried automatically (ADR 0019, amended 2026-10-15)",
		logName, in.IncidentID, sale.clientOrderID)
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
// end: Boost's latch in force, or for a momentum log the rebalance in
// progress or none, and either the incident end holds carried in
// force with its state, scope (instruments and cash components) and sale
// attempt, with the adapter's settlements of its never-sent sales as their
// evidence, or, when the owner clears it, that incident cleared by its ID and
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
	if end.Momentum {
		// An IRA's state in force is its rebalance in progress, or none
		// (ADR 0026, point 5); it has no latch.
		p.Boost, p.Momentum = event.BoostStateInForce{}, &event.MomentumStateInForce{RebalanceInProgress: end.Rebalance}
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
			SaleRetiredAttempts: end.SaleRetiredAttempts, SaleSettlements: end.SaleSettlements,
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
