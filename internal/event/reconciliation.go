package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// ReconciliationEventType identifies one reconciliation of an account's book
// against the broker (ADR 0019, "Reconciliation evidence is an input"): IBKR's
// positions and cash for the account at one cutoff, recorded so that replay
// never asks the broker again. It is an input; the classification is the
// domain's decision (ReconciliationResultEventType). It carries the observed
// positions account.broker.snapshot also carries, and the cash that snapshot
// does not (ADR 0019, amendment of 2026-10-17, reading 11), so the one record
// is the comparison's whole broker evidence.
const ReconciliationEventType = "account.reconciliation"

// ReconciliationSchemaVersion is the current schema version of ReconciliationPayload.
const ReconciliationSchemaVersion uint32 = 1

// ReconciliationTriggerMorning is the reconciliation before the day's first
// decision (ADR 0019, "Reconcile at decision boundaries"; plan.md section 3:
// "every morning the adapter reports IBKR's positions and cash"). It is the
// one trigger this build reads; the after-fill, pre-order and scheduled
// checks are later work.
const ReconciliationTriggerMorning = "morning"

// The components of a reconciliation observation that can be unreadable, and
// the names a result's components and an engine state's cash scope use. An
// unreadable component is not zero (ADR 0019, "An unavailable balance is not
// zero"): it carries no value.
const (
	ReconciliationComponentCash      = "total-cash"
	ReconciliationComponentPositions = "positions"
	// ReconciliationComponentPosition is one instrument's holding in a
	// result's component list.
	ReconciliationComponentPosition = "position"
)

// ReconciliationPayload is IBKR's view of one account at AsOf, a whole second.
type ReconciliationPayload struct {
	AccountID        string    `json:"account_id"`
	ReconciliationID string    `json:"reconciliation_id"`
	Trigger          string    `json:"trigger"`
	AsOf             time.Time `json:"as_of"`
	// Unreadable lists, strictly ascending, the components IBKR could not
	// supply; it is stated, [] when every component was read.
	Unreadable []string `json:"unreadable"`
	// Positions is every holding in raw shares, strictly ascending by
	// instrument, with no zero rows; an instrument not listed is not held. A
	// readable list is stated, [] when none are held. A short is recorded as
	// observed, never dropped.
	Positions []AccountPosition `json:"positions"`
	// TotalCash is IBKR's TotalCashValue in Currency, negative for a debit.
	TotalCash float64 `json:"total_cash"`
	Currency  string  `json:"currency"`
}

// UnmarshalJSON decodes the observation and requires unreadable to be
// stated: an observation that omits it would read as complete.
func (p *ReconciliationPayload) UnmarshalJSON(raw []byte) error {
	type plain ReconciliationPayload
	var stated struct {
		plain
		Unreadable *[]string `json:"unreadable"`
	}
	if err := json.Unmarshal(raw, &stated); err != nil {
		return err
	}
	if stated.Unreadable == nil {
		return errors.New("reconciliation payload: unreadable must be stated, [] when every component was read")
	}
	*p = ReconciliationPayload(stated.plain)
	p.Unreadable = *stated.Unreadable
	return nil
}

// Readable reports whether component was read.
func (p ReconciliationPayload) Readable(component string) bool {
	return !slices.Contains(p.Unreadable, component)
}

// Validate checks identity, each component against its readability, and the
// positions.
func (p ReconciliationPayload) Validate() error {
	var errs []error
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	if p.ReconciliationID == "" {
		errs = append(errs, errors.New("reconciliation id is required"))
	}
	if p.Trigger != ReconciliationTriggerMorning {
		errs = append(errs, fmt.Errorf("trigger %q is not %s", p.Trigger, ReconciliationTriggerMorning))
	}
	errs = append(errs, wholeSecondTime("as of", p.AsOf)...)
	for i, c := range p.Unreadable {
		if c != ReconciliationComponentCash && c != ReconciliationComponentPositions {
			errs = append(errs, fmt.Errorf("unreadable component %q is not recognised", c))
		}
		if i > 0 && c <= p.Unreadable[i-1] {
			errs = append(errs, fmt.Errorf("unreadable components must be strictly ascending; %q follows %q", c, p.Unreadable[i-1]))
		}
	}
	switch {
	case !p.Readable(ReconciliationComponentPositions) && len(p.Positions) != 0:
		errs = append(errs, errors.New("unreadable positions carry no rows"))
	case p.Readable(ReconciliationComponentPositions) && p.Positions == nil:
		errs = append(errs, errors.New("readable positions must be stated, [] when none are held; an unavailable list is marked unreadable"))
	}
	for i, position := range p.Positions {
		if position.InstrumentID == "" {
			errs = append(errs, fmt.Errorf("position %d: instrument id is required", i+1))
			continue
		}
		if position.Quantity == 0 {
			errs = append(errs, fmt.Errorf("position %s: quantity must not be zero; an empty holding is stated by absence", position.InstrumentID))
		}
		if i > 0 && position.InstrumentID <= p.Positions[i-1].InstrumentID {
			errs = append(errs, fmt.Errorf("positions must be strictly ascending by instrument; %q follows %q", position.InstrumentID, p.Positions[i-1].InstrumentID))
		}
	}
	switch {
	case !p.Readable(ReconciliationComponentCash) && (p.TotalCash != 0 || p.Currency != ""):
		errs = append(errs, errors.New("an unreadable total cash carries no value"))
	case p.Readable(ReconciliationComponentCash) && !isFinite(p.TotalCash):
		errs = append(errs, errors.New("total cash must be finite"))
	case p.Readable(ReconciliationComponentCash) && p.Currency == "":
		errs = append(errs, errors.New("a readable total cash names its currency"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid reconciliation payload: %w", err)
	}
	return nil
}

// ReconciliationResultEventType identifies the domain's classification of
// one reconciliation input (ADR 0019, "Reconciliation evidence is an input;
// its consequence is a decision"): recomputed from the recorded observation
// and the journal's own book, never taken from the producer. It also records
// a close-snapshot check that did not continue: an account snapshot whose
// holdings the book and the fills since it do not explain (ADR 0019,
// amended 2026-10-21). An unexplained
// or unverifiable result is followed by a strategy.engine.state record when
// it moves the engine state.
const ReconciliationResultEventType = "risk.reconciliation.result"

// ReconciliationResultSchemaVersion is the current schema version of
// ReconciliationResultPayload.
const ReconciliationResultSchemaVersion uint32 = 1

// The classifications (ADR 0019, "Explain changes by causing events").
const (
	// ReconciliationMatched: nothing changed since the book.
	ReconciliationMatched = "matched"
	// ReconciliationExplained: journalled events account for every change exactly.
	ReconciliationExplained = "explained"
	// ReconciliationUnexplained: a residual the evidence bounds to named
	// instruments or cash components.
	ReconciliationUnexplained = "unexplained"
	// ReconciliationUnverifiable: incomplete or inconsistent evidence.
	ReconciliationUnverifiable = "unverifiable"
	// ReconciliationPendingCashRead: a difference in cash alone, on a day
	// the Flex reader has not yet read, which a deposit or withdrawal it has
	// not yet recorded may explain. It alerts, moves no state and is
	// checked again at the next reconciliation (ADR 0019, amended
	// 2026-10-21, "a cash difference the reader has not yet read").
	ReconciliationPendingCashRead = "pending-cash-read"
)

// PendingCashReadSessions is how many sessions may close after a pending
// cash read before it is decided whatever the Flex reader says: a reader set
// aside for longer never keeps a difference pending, and the reporter's
// alert names this cap (ADR 0019, amended 2026-10-21).
const PendingCashReadSessions = 2

// The actions (ADR 0019, amended 2026-09-24: "continue, degrade or halt";
// amended 2026-10-21: alert, for a pending cash read, which moves no state).
const (
	ReconciliationActionContinue = "continue"
	ReconciliationActionDegrade  = "degrade"
	ReconciliationActionHalt     = "halt"
	ReconciliationActionAlert    = "alert"
)

// ReconciliationComponent is one compared component: an instrument's
// holding in raw shares, or the account's total cash in whole cents. Book is
// the journal's book, Expected the book with every explanation applied,
// Observed IBKR's figure and Residual Observed - Expected. Every component is
// listed, unchanged ones too, so completeness can be checked (ADR 0019, "The
// input contract").
type ReconciliationComponent struct {
	Component    string `json:"component"`
	InstrumentID string `json:"instrument_id,omitempty"`
	Book         int64  `json:"book"`
	Expected     int64  `json:"expected"`
	Observed     int64  `json:"observed"`
	Residual     int64  `json:"residual"`
	// Explanations names the journalled events applied to it, in order: a
	// fill by its fill ID, a corporate action by its envelope ID.
	Explanations []string `json:"explanations"`
	// Explained is set when the residual is zero and nothing about the
	// component is in doubt.
	Explained bool   `json:"explained"`
	Detail    string `json:"detail,omitempty"`
}

// ReconciliationResultPayload is one reconciliation's classification.
type ReconciliationResultPayload struct {
	AccountID        string `json:"account_id"`
	ReconciliationID string `json:"reconciliation_id"`
	// InputID is the account.reconciliation envelope classified, or the
	// account.snapshot envelope of a close-snapshot check, whose
	// ReconciliationID is "close-snapshot:" and that ID (ADR 0019, amended
	// 2026-10-21); AnchorID the account.snapshot envelope the book starts
	// from, empty when there is none.
	InputID        string `json:"input_id"`
	AnchorID       string `json:"anchor_id"`
	Classification string `json:"classification"`
	Action         string `json:"action"`
	// CashCompared is set when the book states cash (account.snapshot
	// version 4), in Currency; Boost's book (version 3) states none.
	CashCompared bool                      `json:"cash_compared"`
	Currency     string                    `json:"currency"`
	Components   []ReconciliationComponent `json:"components"`
	// Unverifiable names why the evidence could not be compared.
	Unverifiable []string `json:"unverifiable,omitempty"`
	// The scope an unexplained result degrades, each strictly ascending:
	// exactly the components not explained.
	ScopeInstrumentIDs  []string `json:"scope_instrument_ids,omitempty"`
	ScopeCashComponents []string `json:"scope_cash_components,omitempty"`
	Detail              string   `json:"detail"`
	// RepeatOf is the reconciliation ID of the reading that first found
	// this pending cash difference, set on a later reading's result that
	// found the same difference and on that result's decision: the
	// difference has already paged, so the reporter does not page it again
	// (ADR 0019, amended 2026-10-21: one difference, one reconciliation
	// alert). Empty on every other result.
	RepeatOf string `json:"repeat_of,omitempty"`
}

// Validate checks identity, the classification against its action, reasons
// and scope, and each component.
func (p ReconciliationResultPayload) Validate() error {
	var errs []error
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	if p.ReconciliationID == "" {
		errs = append(errs, errors.New("reconciliation id is required"))
	}
	if p.InputID == "" {
		errs = append(errs, errors.New("input id is required"))
	}
	if p.Detail == "" {
		errs = append(errs, errors.New("detail is required"))
	}
	if p.CashCompared == (p.Currency == "") {
		errs = append(errs, errors.New("a cash comparison names its currency, and only a cash comparison does"))
	}
	var unexplainedIDs, unexplainedCash []string
	explanations := 0
	for i, c := range p.Components {
		switch c.Component {
		case ReconciliationComponentPosition:
			if c.InstrumentID == "" {
				errs = append(errs, fmt.Errorf("component %d: a position names its instrument", i+1))
			}
		case ReconciliationComponentCash:
			if !p.CashCompared || c.InstrumentID != "" {
				errs = append(errs, fmt.Errorf("component %d: a cash component is listed only when cash is compared, and names no instrument", i+1))
			}
		default:
			errs = append(errs, fmt.Errorf("component %d: component %q is not position or %s", i+1, c.Component, ReconciliationComponentCash))
		}
		if c.Residual != c.Observed-c.Expected {
			errs = append(errs, fmt.Errorf("component %d: residual must be observed less expected", i+1))
		}
		if c.Explained && c.Residual != 0 {
			errs = append(errs, fmt.Errorf("component %d: a component with a residual is not explained", i+1))
		}
		for _, id := range c.Explanations {
			if id == "" {
				errs = append(errs, fmt.Errorf("component %d: an explanation names its event", i+1))
			}
		}
		explanations += len(c.Explanations)
		switch {
		case c.Explained:
		case c.Component == ReconciliationComponentCash:
			unexplainedCash = append(unexplainedCash, c.Component)
		default:
			unexplainedIDs = append(unexplainedIDs, c.InstrumentID)
		}
	}
	errs = append(errs, ascendingIDs("scope instrument id", p.ScopeInstrumentIDs)...)
	unexplained := len(unexplainedIDs)+len(unexplainedCash) > 0
	scoped := len(p.ScopeInstrumentIDs)+len(p.ScopeCashComponents) > 0
	switch p.Classification {
	case ReconciliationMatched, ReconciliationExplained:
		if p.Action != ReconciliationActionContinue {
			errs = append(errs, fmt.Errorf("action %q does not follow %s, which continues", p.Action, p.Classification))
		}
		if unexplained {
			errs = append(errs, fmt.Errorf("a %s result has every component explained", p.Classification))
		}
		if p.Classification == ReconciliationMatched && explanations > 0 {
			errs = append(errs, errors.New("a matched result applies no explanation; one that does is explained"))
		}
		if scoped {
			errs = append(errs, errors.New("a result that continues names no scope"))
		}
	case ReconciliationUnexplained:
		if p.Action != ReconciliationActionDegrade {
			errs = append(errs, fmt.Errorf("action %q does not follow unexplained, which degrades", p.Action))
		}
		if !scoped || !slices.Equal(p.ScopeInstrumentIDs, unexplainedIDs) || !slices.Equal(p.ScopeCashComponents, unexplainedCash) {
			errs = append(errs, errors.New("an unexplained result's scope is exactly its unexplained components, and is never empty"))
		}
	case ReconciliationPendingCashRead:
		if p.Action != ReconciliationActionAlert {
			errs = append(errs, fmt.Errorf("action %q does not follow %s, which alerts", p.Action, p.Classification))
		}
		if !slices.Equal(unexplainedCash, []string{ReconciliationComponentCash}) || len(unexplainedIDs) > 0 {
			errs = append(errs, errors.New("a pending cash read is a difference in cash alone"))
		}
		if scoped {
			errs = append(errs, errors.New("a pending cash read moves no state and names no scope"))
		}
	case ReconciliationUnverifiable:
		if p.Action != ReconciliationActionHalt {
			errs = append(errs, fmt.Errorf("action %q does not follow unverifiable, which halts", p.Action))
		}
		if len(p.Unverifiable) == 0 {
			errs = append(errs, errors.New("an unverifiable result names its reasons"))
		}
		if scoped {
			errs = append(errs, errors.New("a halt covers the whole account and names no scope"))
		}
	default:
		errs = append(errs, fmt.Errorf("classification %q is not matched, explained, unexplained, unverifiable or pending-cash-read", p.Classification))
	}
	if p.RepeatOf != "" {
		if p.Classification == ReconciliationMatched || p.Classification == ReconciliationUnverifiable {
			errs = append(errs, fmt.Errorf("a %s result is no repeat of a pending cash difference", p.Classification))
		}
		if p.RepeatOf == p.InputID {
			errs = append(errs, errors.New("a repeat names an earlier reading, not its own input"))
		}
	}
	if p.Classification != ReconciliationUnverifiable && len(p.Unverifiable) > 0 {
		errs = append(errs, errors.New("only an unverifiable result names unverifiable reasons"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid reconciliation result payload: %w", err)
	}
	return nil
}

// AlertAcknowledgedEventType identifies the owner's acknowledgement of one
// alert (ADR 0019, amended 2026-09-24, "Alerting": the alert "repeats at a
// fixed interval until the owner acknowledges it. Acknowledgement is
// recorded"). It is a recorded input, never a live read; it stops the alert
// repeating and changes no engine state, since leaving Degraded or Halted is
// the recovery procedure's.
const AlertAcknowledgedEventType = "operator.alert.acknowledged"

// AlertAcknowledgedSchemaVersion is the current schema version of
// AlertAcknowledgedPayload.
const AlertAcknowledgedSchemaVersion uint32 = 1

// AlertAcknowledgedPayload acknowledges the alert whose key is AlertKey, the
// key the alert states.
type AlertAcknowledgedPayload struct {
	AccountID      string    `json:"account_id"`
	AlertKey       string    `json:"alert_key"`
	AcknowledgedBy string    `json:"acknowledged_by"`
	AcknowledgedAt time.Time `json:"acknowledged_at"`
	Detail         string    `json:"detail"`
}

// Validate checks the account, key, person and time.
func (p AlertAcknowledgedPayload) Validate() error {
	var errs []error
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	if p.AlertKey == "" {
		errs = append(errs, errors.New("alert key is required"))
	}
	if p.AcknowledgedBy == "" {
		errs = append(errs, errors.New("acknowledged by is required"))
	}
	errs = append(errs, requiredTime("acknowledged at", p.AcknowledgedAt)...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid alert acknowledged payload: %w", err)
	}
	return nil
}
