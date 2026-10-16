package event

import (
	"errors"
	"fmt"
	"time"
)

// UniverseSnapshotEventType identifies one session's universe input for the
// momentum strategy (ADR 0026, point 1, "Universe"): every listed stock's
// listing type, that session's unadjusted close and volume, and its sector
// label with the label's source. The module ranks and filters; the producer
// only records (plan.md section 4, D8). It is a different input from the
// Turtle's market.instrument-classification (ADR 0009).
const UniverseSnapshotEventType = "market.universe.snapshot"

// UniverseSnapshotSchemaVersion is the current schema version of UniverseSnapshotPayload.
const UniverseSnapshotSchemaVersion uint32 = 1

// UniverseSnapshotPayload is one session's universe rows, as Source recorded
// them.
type UniverseSnapshotPayload struct {
	// Session is the session whose close and volume the rows carry, as
	// YYYY-MM-DD: the last session before the rebalance (plan.md section 4,
	// D8 "rank").
	Session string `json:"session"`
	// Source names the universe data source, which the configuration names.
	Source string `json:"source"`
	// Rows is sorted strictly ascending by instrument.
	Rows []UniverseRow `json:"rows"`
}

// UniverseRow is one listed stock. Close and Volume are the unadjusted trade
// close and volume (plan.md section 4, D8: an adjusted close can move a stock
// across $5 or in and out of the top 500). Sector and SectorSource are both
// empty when no source labels the stock.
type UniverseRow struct {
	InstrumentID string  `json:"instrument_id"`
	Type         string  `json:"type"`
	Close        float64 `json:"close"`
	Volume       float64 `json:"volume"`
	Sector       string  `json:"sector"`
	SectorSource string  `json:"sector_source"`
}

// Validate checks the session, source and every row.
func (p UniverseSnapshotPayload) Validate() error {
	var errs []error
	if _, err := time.Parse(time.DateOnly, p.Session); err != nil {
		errs = append(errs, fmt.Errorf("session %q is not a YYYY-MM-DD date", p.Session))
	}
	if p.Source == "" {
		errs = append(errs, errors.New("source is required"))
	}
	ids := make([]string, len(p.Rows))
	for i, r := range p.Rows {
		ids[i] = r.InstrumentID
		if r.Type == "" {
			errs = append(errs, fmt.Errorf("row %d: type is required", i+1))
		}
		if !isFinite(r.Close) || r.Close < 0 || !isFinite(r.Volume) || r.Volume < 0 {
			errs = append(errs, fmt.Errorf("row %d: close and volume must be finite and not negative", i+1))
		}
		if (r.Sector == "") != (r.SectorSource == "") {
			errs = append(errs, fmt.Errorf("row %d: a sector label and its source come together", i+1))
		}
	}
	errs = append(errs, ascendingIDs("row instrument id", ids)...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid universe snapshot payload: %w", err)
	}
	return nil
}

// SessionCalendarEventType identifies a recorded trading calendar: every
// session expected from its first to its last (ADR 0026, point 1, "the
// session calendar"). Decision times come from it, never a clock.
const SessionCalendarEventType = "market.session.calendar"

// SessionCalendarSchemaVersion is the current schema version of SessionCalendarPayload.
const SessionCalendarSchemaVersion uint32 = 1

// SessionCalendarPayload lists the expected sessions as YYYY-MM-DD, strictly
// ascending; a date between two listed sessions is not a session.
type SessionCalendarPayload struct {
	Source   string   `json:"source"`
	Sessions []string `json:"sessions"`
}

// Validate checks the source and that the sessions are dates in strictly
// ascending order.
func (p SessionCalendarPayload) Validate() error {
	var errs []error
	if p.Source == "" {
		errs = append(errs, errors.New("source is required"))
	}
	if len(p.Sessions) == 0 {
		errs = append(errs, errors.New("a calendar lists at least one session"))
	}
	for i, s := range p.Sessions {
		if _, err := time.Parse(time.DateOnly, s); err != nil {
			errs = append(errs, fmt.Errorf("session %d %q is not a YYYY-MM-DD date", i+1, s))
		} else if i > 0 && s <= p.Sessions[i-1] {
			errs = append(errs, fmt.Errorf("sessions must be strictly ascending; %s follows %s", s, p.Sessions[i-1]))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid session calendar payload: %w", err)
	}
	return nil
}

// AccountSnapshotCashVersion is version 4 of account.snapshot
// (AccountSnapshotEventType): version 3's positions and net liquidation, and
// the account's cash, which an IRA's buys are budgeted from (ADR 0026,
// point 2, "Budget").
//
//   - Version 4 adds SettledCash and TotalCash to version 3. Version 3 stays
//     what Boost reads and version 2 what the Turtle reducer reads; each
//     consumer refuses the others' versions (ADR 0015).
const AccountSnapshotCashVersion uint32 = 4

// AccountCashSnapshotPayload is account.snapshot version 4. TotalCash
// includes unsettled sale proceeds; SettledCash does not. Either may be
// negative when the broker reports a debit, which in an IRA is a margin
// loan the risk gate refuses to add to.
type AccountCashSnapshotPayload struct {
	AccountPositionsSnapshotPayload
	SettledCash float64 `json:"settled_cash"`
	TotalCash   float64 `json:"total_cash"`
}

// Validate checks version 3's fields and that both cash figures are finite.
func (p AccountCashSnapshotPayload) Validate() error {
	var errs []error
	if err := p.AccountPositionsSnapshotPayload.Validate(); err != nil {
		errs = append(errs, err)
	}
	if !isFinite(p.SettledCash) || !isFinite(p.TotalCash) {
		errs = append(errs, errors.New("settled and total cash must be finite"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid account cash snapshot payload: %w", err)
	}
	return nil
}

// OrderLifecycleLimitVersion is version 2 of execution.order.lifecycle
// (OrderLifecycleEventType): a state change of one of a target-weight
// account's limit orders, joined to it by client order ID (ADR 0026,
// Consequences: StopPrice does not apply to a limit order).
//
//   - Version 2 is LimitOrderLifecyclePayload. Version 1 stays what the
//     Turtle reducer reads (OrderLifecycleSchemaVersion); each consumer
//     refuses the other's version.
const OrderLifecycleLimitVersion uint32 = 2

// LimitOrderLifecyclePayload records one state change of one released limit
// order, as the venue reported it. Status is one of the OrderLifecycleStatus
// constants except updated: a planned order is never amended. An execution is
// a TargetWeightFillPayload, never a lifecycle change.
type LimitOrderLifecyclePayload struct {
	AccountID     string `json:"account_id"`
	ClientOrderID string `json:"client_order_id"`
	// OrderID is the venue's own identifier, which arrives only in lifecycle
	// reports and is never derived (ADR 0026, point 2, "Order identity").
	OrderID      string    `json:"order_id"`
	InstrumentID string    `json:"instrument_id"`
	Side         string    `json:"side"`
	Status       string    `json:"status"`
	Quantity     int64     `json:"quantity"`
	LimitPrice   float64   `json:"limit_price"`
	OccurredAt   time.Time `json:"occurred_at"`
	Message      string    `json:"message"`
}

// Terminal reports whether the order can no longer execute: canceled or invalid.
func (p LimitOrderLifecyclePayload) Terminal() bool {
	return p.Status == OrderLifecycleStatusCanceled || p.Status == OrderLifecycleStatusInvalid
}

// Validate checks identity, side, status, quantity, limit and time.
func (p LimitOrderLifecyclePayload) Validate() error {
	errs := orderIdentity(p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side, p.Quantity, p.LimitPrice)
	if p.OrderID == "" {
		errs = append(errs, errors.New("order id is required"))
	}
	switch p.Status {
	case OrderLifecycleStatusSubmitted, OrderLifecycleStatusCancelPending, OrderLifecycleStatusCanceled, OrderLifecycleStatusInvalid:
	default:
		errs = append(errs, fmt.Errorf("status %q is not submitted, cancel-pending, canceled or invalid", p.Status))
	}
	errs = append(errs, requiredTime("occurred at", p.OccurredAt)...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid limit order lifecycle payload: %w", err)
	}
	return nil
}

// FillTargetWeightVersion is version 5 of execution.fill (FillEventType): one
// execution of a target-weight account's limit order, joined to it by client
// order ID. It has no Campaign, Unit or proposal (ADR 0026, Consequences).
//
//   - Version 5 is TargetWeightFillPayload. Version 4 stays what the Turtle
//     reducer reads (FillSchemaVersion); each consumer refuses the other's
//     version.
const FillTargetWeightVersion uint32 = 5

// TargetWeightFillPayload records one execution: Quantity whole shares at
// Price, with the Commission charged for it. FillID is unique per execution.
type TargetWeightFillPayload struct {
	AccountID     string    `json:"account_id"`
	ClientOrderID string    `json:"client_order_id"`
	FillID        string    `json:"fill_id"`
	InstrumentID  string    `json:"instrument_id"`
	Side          string    `json:"side"`
	Quantity      int64     `json:"quantity"`
	Price         float64   `json:"price"`
	Commission    float64   `json:"commission"`
	FilledAt      time.Time `json:"filled_at"`
}

// Validate checks identity, side, quantity, price, commission and time.
func (p TargetWeightFillPayload) Validate() error {
	errs := orderIdentity(p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side, p.Quantity, 1)
	if !isFinite(p.Price) || p.Price <= 0 {
		errs = append(errs, errors.New("price must be finite and positive"))
	}
	if p.FillID == "" {
		errs = append(errs, errors.New("fill id is required"))
	}
	if !isFinite(p.Commission) || p.Commission < 0 {
		errs = append(errs, errors.New("commission must be finite and not negative"))
	}
	errs = append(errs, requiredTime("filled at", p.FilledAt)...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid target-weight fill payload: %w", err)
	}
	return nil
}
