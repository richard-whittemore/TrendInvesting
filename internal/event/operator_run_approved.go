package event

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// OperatorRunApprovedEventType identifies the owner's approval to start a
// fresh run of an account whose engine has run before. A crash resumes from
// the durable log (ADR 0027); every other restart (leaving Halted, a build
// upgrade, a cleared rebalance) is a fresh run, and a fresh run decides
// nothing until this approval is its input, straight after the
// configuration (owner decision 2026-10-16). It names the previous run by its
// durable log and that log's final record hash, so the approval is bound to
// the evidence the owner reviewed, and carries the state in force that the
// fresh run starts from.
//
// Namespaced "operator.", as ADR 0026's operator.kill-switch is: a person's
// recorded instruction, not a market fact, an account observation or this
// system's own decision.
const OperatorRunApprovedEventType = "operator.run.approved"

// OperatorRunApprovedSchemaVersion is the current schema version of
// OperatorRunApprovedPayload. Version 2 adds the incident in force's cash
// scope (IncidentInForce.ScopeCashComponents), which version 1 could not
// state; version 3 adds the starting book (StartingBook), and with it a
// first run's approval, which names no previous log; version 4 adds the
// incident in force's retired sale attempts
// (IncidentInForce.SaleRetiredAttempts), omitted when none; version 5 adds
// the adapter's settlements of the incident's never-sent sales
// (IncidentInForce.SaleSettlements), omitted when none; version 6 adds an
// IRA's momentum state in force (Momentum), the rebalance in progress or
// none, stated by an IRA approval in place of Boost's latch and omitted by a
// Boost approval; DecodeRecordedOperatorRunApproved reads each (ADR 0015: a
// payload's shape is versioned).
const OperatorRunApprovedSchemaVersion uint32 = 6

// recordHashPattern is a journal record hash as the chain writes it (ADR
// 0017): "sha256:" and 64 lower-case hex digits.
var recordHashPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// OperatorRunApprovedPayload is one approval to start a fresh run.
type OperatorRunApprovedPayload struct {
	// AccountID is the account the fresh run decides; it must be the
	// configuration's.
	AccountID string `json:"account_id"`
	// PreviousLog is the file name, without a directory, of the previous
	// run's durable log in the same directory as the fresh run's log.
	PreviousLog string `json:"previous_log"`
	// PreviousLogFinalHash is the record hash of that log's last committed
	// record (journal.Recovery.FinalRecordHash).
	PreviousLogFinalHash string `json:"previous_log_final_hash"`
	// IncidentID names the incident the approval clears (ADR 0019's
	// recovery procedure), or is empty when there was none.
	IncidentID string    `json:"incident_id"`
	ApprovedBy string    `json:"approved_by"`
	ApprovedAt time.Time `json:"approved_at"`
	// Boost is a Boost run's state in force the fresh run starts from; null
	// in an IRA approval, which states Momentum instead.
	Boost BoostStateInForce `json:"boost"`
	// Momentum is an IRA run's state in force (schema version 6): the
	// rebalance in progress the approved log ends with, or none. It is left
	// out of a Boost approval, so a Boost approval's file is the one an
	// earlier build wrote; an approval states exactly one of the two.
	Momentum *MomentumStateInForce `json:"momentum,omitempty"`
	// IncidentInForce is the incident the previous run ended in and the
	// fresh run continues, with whether its one sale back to 98% was
	// attempted; nil (JSON null) when there is none. It is never the
	// incident the approval clears.
	IncidentInForce *IncidentInForce `json:"incident_in_force"`
	StartingBook    *StartingBook    `json:"starting_book"`
}

// StartingBook is the account's holdings and total cash the owner confirms a
// run starts from (schema version 3; ADR 0019 and ADR 0027, amended
// 2026-10-18). The IBKR adapter compares the run's first reading after a
// close with the latest of the earlier runs' last account snapshot and this
// book, adjusted by the fills since, and never adopts IBKR's book unchecked.
// An account's first run must state one, since no earlier run's snapshot
// exists; a fresh run may, to state the book after a change no fill explains
// (a transfer, a deposit), which the owner has checked in IBKR.
type StartingBook struct {
	// Positions are the holdings in raw shares, strictly ascending by
	// instrument, none zero; [] when the account holds none.
	Positions []AccountPosition `json:"positions"`
	// TotalCash is IBKR's TotalCashValue the owner confirms, in Currency.
	TotalCash float64 `json:"total_cash"`
	Currency  string  `json:"currency"`
}

// Validate checks the holdings and the cash.
func (b StartingBook) Validate() error {
	var errs []error
	if b.Positions == nil {
		errs = append(errs, errors.New("starting book positions are stated, [] when the account holds none"))
	}
	for i, position := range b.Positions {
		switch {
		case position.InstrumentID == "":
			errs = append(errs, fmt.Errorf("starting book position %d names no instrument", i+1))
		case position.Quantity == 0:
			errs = append(errs, fmt.Errorf("starting book position %s states a zero quantity; an instrument not held is left out", position.InstrumentID))
		}
		if i > 0 && position.InstrumentID <= b.Positions[i-1].InstrumentID {
			errs = append(errs, fmt.Errorf("starting book positions must be strictly ascending by instrument; %q follows %q", position.InstrumentID, b.Positions[i-1].InstrumentID))
		}
	}
	if !isFinite(b.TotalCash) {
		errs = append(errs, errors.New("starting book total cash must be finite"))
	}
	if b.Currency == "" {
		errs = append(errs, errors.New("starting book currency is required"))
	}
	return errors.Join(errs...)
}

// FirstRun reports whether p approves an account's first run: it names no
// previous log, and states the starting book instead.
func (p OperatorRunApprovedPayload) FirstRun() bool {
	return p.PreviousLog == "" && p.StartingBook != nil
}

// BoostStateInForce is Boost's state that a fresh run cannot recompute from
// bars: whether the boost is latched (template.py:89-94; ADR 0026, point 5).
// Latched is required: a missing or null latch is refused, never read as
// "not latched", which would turn the boost off unasked.
type BoostStateInForce struct {
	Latched *bool `json:"latched"`
}

// MarshalJSON encodes a Boost state stating no latch, an IRA approval's, as
// null rather than as a latch of null, which the decoder would read as a
// Boost approval that forgot its latch.
func (b BoostStateInForce) MarshalJSON() ([]byte, error) {
	if b.Latched == nil {
		return []byte("null"), nil
	}
	return json.Marshal(struct {
		Latched *bool `json:"latched"`
	}{b.Latched})
}

// MomentumStateInForce is an IRA's state that a fresh run cannot recompute
// from bars: the rebalance in progress when the approved log ended (ADR
// 0026, point 5: "Every planned, released and sent order, and the IRA phase
// (sells sent, buys pending)"). RebalanceInProgress is required, null when
// none: no rebalance is taken as finished by its absence.
type MomentumStateInForce struct {
	RebalanceInProgress *RebalanceInProgress `json:"rebalance_in_progress"`
}

// The IRA phases a rebalance in progress can end a run in (ADR 0026, point
// 2, "IRA execution": the buys are planned only once every sell is
// terminal): its sells working and its buys pending, never planned; or every
// sell terminal and its buys working.
const (
	RebalancePhaseSellsWorking = "sells-working"
	RebalancePhaseBuysWorking  = "buys-working"
)

// RebalanceInProgress is one rebalance whose orders were not all terminal
// when the approved log ended. A fresh run sends nothing for it, and decides
// no later rebalance, until the owner clears exactly these orders
// (operator.rebalance.cleared; ADR 0026, point 5: "A restart mid-rebalance
// sends nothing for that decision time until the owner clears it").
type RebalanceInProgress struct {
	// DecisionID is the rebalance's strategy.targets.decided envelope ID,
	// the tag its orders carry; DecisionTime that decision's time.
	DecisionID   string    `json:"decision_id"`
	DecisionTime time.Time `json:"decision_time"`
	// Phase is RebalancePhaseSellsWorking or RebalancePhaseBuysWorking.
	Phase string `json:"phase"`
	// ClientOrderIDs are, strictly ascending, every order of the rebalance
	// not terminal when the log ended.
	ClientOrderIDs []string `json:"client_order_ids"`
}

// Validate checks the decision, its time, the phase and the orders.
func (r RebalanceInProgress) Validate() error {
	var errs []error
	if r.DecisionID == "" {
		errs = append(errs, errors.New("rebalance in progress: decision id is required"))
	}
	errs = append(errs, requiredTime("rebalance in progress: decision time", r.DecisionTime)...)
	if r.Phase != RebalancePhaseSellsWorking && r.Phase != RebalancePhaseBuysWorking {
		errs = append(errs, fmt.Errorf("rebalance in progress: phase %q is not %q or %q", r.Phase, RebalancePhaseSellsWorking, RebalancePhaseBuysWorking))
	}
	if len(r.ClientOrderIDs) == 0 {
		errs = append(errs, errors.New("rebalance in progress: client order ids are required: every order not terminal is named"))
	}
	errs = append(errs, ascendingIDs("rebalance in progress: client order id", r.ClientOrderIDs)...)
	return errors.Join(errs...)
}

// StrategyID names the target-weight strategy whose state in force the
// approval states: momentum for an IRA approval, else Boost.
func (p OperatorRunApprovedPayload) StrategyID() string {
	if p.Momentum != nil {
		return TargetWeightStrategyMomentum
	}
	return TargetWeightStrategyBoost
}

// IncidentInForce is one incident's state that a fresh run cannot recompute
// from bars: the engine state and scope it holds the account in, and whether
// ADR 0019's one sale back to 98% (amended 2026-10-15:
// attempted once per incident, never retried automatically) was attempted.
// A fresh run approved with the sale attempted never attempts it again.
type IncidentInForce struct {
	// IncidentID names the incident (ADR 0019's first engine-state record
	// out of Normal).
	IncidentID string `json:"incident_id"`
	// State is the engine state the incident holds the account in,
	// EngineStateDegraded or EngineStateHalted, ScopeInstrumentIDs the
	// instruments a Degraded state freezes and ScopeCashComponents the cash
	// components it freezes (a reconciliation's unexplained cash, which
	// blocks buys), each strictly ascending. Degraded names at least one of
	// either; Halted covers the whole account and names none. All three are
	// required (ScopeCashComponents from schema version 2): the fresh run
	// resumes in that state and scope before any market input, and leaving
	// either is never automatic (ADR 0019, amended 2026-09-24, "The state
	// survives a restart").
	State               string   `json:"state"`
	ScopeInstrumentIDs  []string `json:"scope_instrument_ids"`
	ScopeCashComponents []string `json:"scope_cash_components"`
	// SaleAttempted is required: whether the incident's sale was attempted,
	// sent or withheld (an unreadable broker value or an unknown working
	// sell is an attempt; nothing to sell is not).
	SaleAttempted *bool `json:"sale_attempted"`
	// SaleClientOrderID is the attempt's client order ID when it was sent,
	// and empty otherwise.
	SaleClientOrderID string `json:"sale_client_order_id,omitempty"`
	// SaleRetiredAttempts is how many of the incident's sales were retired
	// as never sent (execution.order.retired), each under a client order ID
	// of its own, so the fresh run's next attempt has the next one and never
	// a retired order's (orderplan.SaleAttemptClientOrderID; ADR 0026, point
	// 2, "Order identity"). Schema version 4; omitted when none.
	SaleRetiredAttempts int `json:"sale_retired_attempts,omitempty"`
	// SaleSettlements are the adapter's settlements of the incident's sales
	// the previous run released and the adapter never sent, written after
	// that run ended (NeverSentSettlement): the evidence on which the
	// approval counts each among SaleRetiredAttempts and states the sale not
	// attempted, which the engine checks against the previous log by its own
	// retirement rule before the fresh run starts (ADR 0019, amended
	// 2026-10-17, reading 4, as the owner's decision of 2026-10-22 reads
	// it). Schema version 5; omitted when none.
	SaleSettlements []NeverSentSettlement `json:"sale_settlements,omitempty"`
}

// Validate checks the account, the previous log's name and hash, who
// approved and when, and that the state in force is stated: Boost's latch,
// and for an incident in force, whether its sale was attempted.
func (p OperatorRunApprovedPayload) Validate() error {
	var errs []error
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	switch {
	case p.FirstRun():
		errs = append(errs, p.firstRunErrors()...)
	case p.PreviousLog == "":
		errs = append(errs, errors.New("previous log is required: only a first run's approval names none, and it states the starting book"))
	case strings.ContainsAny(p.PreviousLog, `/\`) || p.PreviousLog == "." || p.PreviousLog == "..":
		errs = append(errs, fmt.Errorf("previous log %q must be a file name in the fresh run's log directory, not a path", p.PreviousLog))
	}
	if !p.FirstRun() && !recordHashPattern.MatchString(p.PreviousLogFinalHash) {
		errs = append(errs, fmt.Errorf("previous log final hash %q is not a record hash (sha256: and 64 lower-case hex digits)", p.PreviousLogFinalHash))
	}
	if p.StartingBook != nil {
		if err := p.StartingBook.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if p.ApprovedBy == "" {
		errs = append(errs, errors.New("approved by is required"))
	}
	errs = append(errs, requiredTime("approved at", p.ApprovedAt)...)
	switch {
	case p.Momentum != nil && p.Boost.Latched != nil:
		errs = append(errs, errors.New("the approval states Boost's latch and an IRA's momentum state: it states the state in force of one strategy only"))
	case p.Momentum != nil:
		errs = append(errs, p.momentumErrors()...)
	case p.Boost.Latched == nil:
		errs = append(errs, errors.New("boost latched is required: the latch in force is stated, never assumed (ADR 0026, point 5)"))
	}
	if in := p.IncidentInForce; in != nil {
		switch in.IncidentID {
		case "":
			errs = append(errs, errors.New("incident in force needs its incident id"))
		case p.IncidentID:
			errs = append(errs, fmt.Errorf("incident %q cannot be both the one the approval clears and the one in force", in.IncidentID))
		}
		scoped := len(in.ScopeInstrumentIDs)+len(in.ScopeCashComponents) > 0
		switch in.State {
		case EngineStateHalted:
			if scoped {
				errs = append(errs, errors.New("incident in force: a halted state covers the whole account and names no scope"))
			}
		case EngineStateDegraded:
			if !scoped {
				errs = append(errs, errors.New("incident in force: a degraded state names its scope instruments or cash components: scope is never inferred from absence"))
			}
		default:
			errs = append(errs, fmt.Errorf("incident in force: state %q is not %q or %q; the state in force is stated, never assumed (ADR 0019, amended 2026-09-24)", in.State, EngineStateDegraded, EngineStateHalted))
		}
		errs = append(errs, ascendingIDs("incident in force: scope instrument id", in.ScopeInstrumentIDs)...)
		errs = append(errs, ascendingIDs("incident in force: scope cash component", in.ScopeCashComponents)...)
		switch {
		case in.SaleAttempted == nil:
			errs = append(errs, errors.New("incident in force: sale attempted is required (ADR 0019, amended 2026-10-15: one attempt per incident)"))
		case !*in.SaleAttempted && in.SaleClientOrderID != "":
			errs = append(errs, fmt.Errorf("incident in force: sale client order %q names an attempt, but the sale is stated as not attempted", in.SaleClientOrderID))
		}
		if in.SaleRetiredAttempts < 0 {
			errs = append(errs, fmt.Errorf("incident in force: sale retired attempts %d is fewer than none", in.SaleRetiredAttempts))
		}
		errs = append(errs, p.settlementErrors()...)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid operator run approved payload: %w", err)
	}
	return nil
}

// momentumErrors checks an IRA approval's momentum state: a rebalance in
// progress it states is valid, a first run has none, and an incident in
// force states no sale back to 98%, which only Boost's margin account has
// (ADR 0019, amended 2026-10-15; ADR 0026, point 4).
func (p OperatorRunApprovedPayload) momentumErrors() []error {
	var errs []error
	if r := p.Momentum.RebalanceInProgress; r != nil {
		if err := r.Validate(); err != nil {
			errs = append(errs, err)
		}
		if p.FirstRun() {
			errs = append(errs, errors.New("a first run's approval states no rebalance in progress: no earlier run released an order"))
		}
	}
	if in := p.IncidentInForce; in != nil && (in.SaleAttempted != nil && *in.SaleAttempted || in.SaleClientOrderID != "" || in.SaleRetiredAttempts != 0 || len(in.SaleSettlements) > 0) {
		errs = append(errs, errors.New("incident in force: an IRA has no sale back to 98%, so its approval states the sale not attempted, with no order, retirement or settlement"))
	}
	return errs
}

// settlementErrors checks the incident in force's settlements of never-sent
// sales: each valid, of the approved log and the approval's account, naming
// its order once, and every one counted among the retired attempts.
func (p OperatorRunApprovedPayload) settlementErrors() []error {
	in := p.IncidentInForce
	var errs []error
	seen := map[string]bool{}
	for _, s := range in.SaleSettlements {
		id := s.Retirement.ClientOrderID
		if err := s.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("incident in force: %w", err))
		}
		if s.Log != p.PreviousLog {
			errs = append(errs, fmt.Errorf("incident in force: the settlement of order %q names log %q, not the approved previous log %q", id, s.Log, p.PreviousLog))
		}
		if s.Retirement.AccountID != p.AccountID {
			errs = append(errs, fmt.Errorf("incident in force: the settlement of order %q is for account %q; the approval is for %q", id, s.Retirement.AccountID, p.AccountID))
		}
		if seen[id] {
			errs = append(errs, fmt.Errorf("incident in force: order %q is settled twice; a settlement names each order once", id))
		}
		seen[id] = true
	}
	if len(in.SaleSettlements) > in.SaleRetiredAttempts {
		errs = append(errs, fmt.Errorf("incident in force: %d sales settled as never sent, but sale retired attempts is %d: each settled sale is a retired attempt", len(in.SaleSettlements), in.SaleRetiredAttempts))
	}
	return errs
}

// firstRunErrors checks a first run's approval: no earlier run, so no final
// hash, no latch and no incident to clear or continue.
func (p OperatorRunApprovedPayload) firstRunErrors() []error {
	var errs []error
	if p.PreviousLogFinalHash != "" {
		errs = append(errs, errors.New("a first run's approval names no previous log, so no previous log final hash"))
	}
	if p.Boost.Latched != nil && *p.Boost.Latched {
		errs = append(errs, errors.New("a first run's approval states the boost not latched: no earlier run latched it"))
	}
	if p.IncidentID != "" || p.IncidentInForce != nil {
		errs = append(errs, errors.New("a first run's approval clears and continues no incident: no earlier run had one"))
	}
	return errs
}

// ValidateSchema checks p under the schema version it was recorded at (ADR
// 0015), then validates it. Each version holds every field of the ones
// before it: version 1 could not state an incident's cash scope, so a
// payload naming one is refused under it; version 2 holds it (that the key
// was stated, null included, is checked where the raw JSON is read, in
// DecodeRecordedOperatorRunApproved); version 3 adds a starting book (ADR
// 0027, amended 2026-10-18), version 4 an incident's retired sale attempts,
// version 5 its settlements of never-sent sales, and version 6 an IRA's
// momentum state (ADR 0027, note of 2026-10-28), each refused under an
// earlier version; any other version is refused.
func (p OperatorRunApprovedPayload) ValidateSchema(version uint32) error {
	if version < 1 || version > OperatorRunApprovedSchemaVersion {
		return fmt.Errorf("the approval's schema version %d is not one this build reads (1 to %d; ADR 0015)", version, OperatorRunApprovedSchemaVersion)
	}
	in := p.IncidentInForce
	switch {
	case version == 1 && in != nil && len(in.ScopeCashComponents) > 0:
		return fmt.Errorf("a schema version 1 approval cannot state incident_in_force.%s", incidentCashKey)
	case version < startingBookVersion && p.StartingBook != nil:
		return fmt.Errorf("a schema version %d approval cannot state a starting book; version %d states it", version, startingBookVersion)
	case version < retiredAttemptsVersion && in != nil && in.SaleRetiredAttempts != 0:
		return fmt.Errorf("a schema version %d approval cannot state incident_in_force.sale_retired_attempts; version %d states it", version, retiredAttemptsVersion)
	case version < settlementsVersion && in != nil && len(in.SaleSettlements) > 0:
		return fmt.Errorf("a schema version %d approval cannot state incident_in_force.sale_settlements; version %d states them", version, settlementsVersion)
	case version < momentumVersion && p.Momentum != nil:
		return fmt.Errorf("a schema version %d approval cannot state a %s state; version %d states it", version, momentumKey, momentumVersion)
	}
	return p.Validate()
}

// startingBookVersion, retiredAttemptsVersion and settlementsVersion are
// the approval schema versions that add the starting book, an incident's
// retired sale attempts and its settlements of never-sent sales.
const (
	startingBookVersion    uint32 = 3
	retiredAttemptsVersion uint32 = 4
	settlementsVersion     uint32 = 5
	momentumVersion        uint32 = 6
)

// momentumKey is an IRA approval's momentum state, a key from schema
// version 6, left out of a Boost approval; rebalanceKey is the key it must
// state, null when no rebalance is in progress, and rebalanceKeys the keys a
// stated rebalance must state.
const (
	momentumKey  = "momentum"
	rebalanceKey = "rebalance_in_progress"
)

var rebalanceKeys = []string{"decision_id", "decision_time", "phase", "client_order_ids"}

// approvalKeys are the keys an approval file must state, null included: a
// key left out is refused rather than read as its zero value.
var approvalKeys = []string{"account_id", "previous_log", "previous_log_final_hash", "incident_id", "approved_by", "approved_at", "boost", "incident_in_force"}

// startingBookKey is the starting book, a key from schema version 3, null
// when the approval states none.
const startingBookKey = "starting_book"

// startingBookKeys are the keys a stated starting book must state, none of
// them null: a cash left out is never read as an approved zero balance (ADR
// 0019: an unavailable balance is not zero; ADR 0027, amended 2026-10-18).
var startingBookKeys = []string{"positions", "total_cash", "currency"}

// incidentKeys are the keys an incident in force must state at schema
// version 1; the sale's client order ID alone may be left out, when no sale
// was sent. Version 2 adds incidentCashKey.
var incidentKeys = []string{"incident_id", "state", "scope_instrument_ids", "sale_attempted"}

// incidentCashKey is the incident in force's cash scope, a key from schema
// version 2.
const incidentCashKey = "scope_cash_components"

// checkMomentumKeys checks an approval's stated momentum key: only from
// schema version 6, an object, stating its rebalance in progress, null when
// none, and every key of a rebalance it states.
func checkMomentumKeys(raw json.RawMessage, schemaVersion uint32) error {
	if schemaVersion < momentumVersion {
		return fmt.Errorf("a schema version %d approval cannot state %s; version %d states it", schemaVersion, momentumKey, momentumVersion)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(raw, &state); err != nil || state == nil {
		return fmt.Errorf("decode the approval's %s: it must be an object stating %s, null when no rebalance is in progress", momentumKey, rebalanceKey)
	}
	value, ok := state[rebalanceKey]
	if !ok {
		return fmt.Errorf("the approval states no %s.%s; it is required, null when no rebalance is in progress (ADR 0026, point 5)", momentumKey, rebalanceKey)
	}
	var rebalance map[string]json.RawMessage
	if err := json.Unmarshal(value, &rebalance); err != nil {
		return fmt.Errorf("decode the approval's %s.%s: it must be an object or null: %w", momentumKey, rebalanceKey, err)
	}
	var missing []string
	if rebalance != nil {
		for _, key := range rebalanceKeys {
			if _, ok := rebalance[key]; !ok {
				missing = append(missing, momentumKey+"."+rebalanceKey+"."+key)
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the approval states no %s; a rebalance in progress states its decision, time, phase and orders", strings.Join(missing, ", "))
	}
	return nil
}

// DecodeOperatorRunApproved decodes and validates an approval written
// outside the engine, at the current schema version. Every key must be
// present, an unknown key is refused, and the input is one JSON value, so a
// state the owner did not state is never a default (ADR 0026, point 5; ADR
// 0019, amended 2026-10-15).
func DecodeOperatorRunApproved(raw []byte) (OperatorRunApprovedPayload, error) {
	return DecodeRecordedOperatorRunApproved(raw, OperatorRunApprovedSchemaVersion)
}

// DecodeRecordedOperatorRunApproved decodes and validates an approval
// recorded at schemaVersion, as DecodeOperatorRunApproved does: version 1
// could not state an incident's cash scope, so it is read with none and may
// not name one; versions 2 to 6 must state it; versions 3 to 6 must also
// state the starting_book key, null when the approval states no book, and a
// book it states must state its positions, total_cash and currency, none
// null, while versions 1 and 2 may not state the key; versions 4 to 6 may
// state an incident's sale_retired_attempts, versions 5 and 6 its
// sale_settlements, each of which may be left out as none; only version 6
// may state the momentum key, an IRA approval's, which must state its
// rebalance_in_progress, null when none, and every key of a rebalance it
// states (checkMomentumKeys; ValidateSchema); any other version is refused
// (ADR 0015).
func DecodeRecordedOperatorRunApproved(raw []byte, schemaVersion uint32) (OperatorRunApprovedPayload, error) {
	if schemaVersion < 1 || schemaVersion > OperatorRunApprovedSchemaVersion {
		return OperatorRunApprovedPayload{}, fmt.Errorf("the approval's schema version %d is not one this build reads (1 to %d; ADR 0015)", schemaVersion, OperatorRunApprovedSchemaVersion)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return OperatorRunApprovedPayload{}, fmt.Errorf("decode the approval: it must be one JSON value, an object: %w", err)
	}
	var missing []string
	for _, key := range approvalKeys {
		if _, ok := keys[key]; !ok {
			missing = append(missing, key)
		}
	}
	_, statesBook := keys[startingBookKey]
	switch {
	case schemaVersion < startingBookVersion && statesBook:
		return OperatorRunApprovedPayload{}, fmt.Errorf("a schema version %d approval cannot state %s", schemaVersion, startingBookKey)
	case schemaVersion >= startingBookVersion && !statesBook:
		missing = append(missing, startingBookKey)
	}
	if len(missing) > 0 {
		return OperatorRunApprovedPayload{}, fmt.Errorf("the approval states no %s; every key is required, null where there is none", strings.Join(missing, ", "))
	}
	if raw, statesMomentum := keys[momentumKey]; statesMomentum {
		if err := checkMomentumKeys(raw, schemaVersion); err != nil {
			return OperatorRunApprovedPayload{}, err
		}
	}
	if statesBook {
		var book map[string]json.RawMessage
		if err := json.Unmarshal(keys[startingBookKey], &book); err != nil {
			return OperatorRunApprovedPayload{}, fmt.Errorf("decode the approval's %s: it must be an object or null: %w", startingBookKey, err)
		}
		if book != nil {
			for _, key := range startingBookKeys {
				if value, ok := book[key]; !ok || string(bytes.TrimSpace(value)) == "null" {
					missing = append(missing, startingBookKey+"."+key)
				}
			}
		}
		if len(missing) > 0 {
			return OperatorRunApprovedPayload{}, fmt.Errorf("the approval states no %s; a starting book states its holdings, total cash and currency, none of them null", strings.Join(missing, ", "))
		}
	}
	var incident map[string]json.RawMessage
	if err := json.Unmarshal(keys["incident_in_force"], &incident); err != nil {
		return OperatorRunApprovedPayload{}, fmt.Errorf("decode the approval's incident_in_force: it must be an object or null: %w", err)
	}
	if incident != nil {
		required := incidentKeys
		_, statesCash := incident[incidentCashKey]
		switch {
		case schemaVersion == 1 && statesCash:
			return OperatorRunApprovedPayload{}, fmt.Errorf("a schema version 1 approval cannot state incident_in_force.%s", incidentCashKey)
		case schemaVersion != 1:
			required = append(slices.Clone(incidentKeys), incidentCashKey)
		}
		for _, key := range required {
			if _, ok := incident[key]; !ok {
				missing = append(missing, "incident_in_force."+key)
			}
		}
		if len(missing) > 0 {
			return OperatorRunApprovedPayload{}, fmt.Errorf("the approval states no %s; every key of an incident in force is required (ADR 0019, amended 2026-09-24)", strings.Join(missing, ", "))
		}
	}
	var p OperatorRunApprovedPayload
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return OperatorRunApprovedPayload{}, fmt.Errorf("decode the approval: %w", err)
	}
	if err := p.ValidateSchema(schemaVersion); err != nil {
		return OperatorRunApprovedPayload{}, err
	}
	return p, nil
}
