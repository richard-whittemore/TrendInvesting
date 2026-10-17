package report

import (
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// OrderState is where a released order stands, as the log records it.
type OrderState string

// The three lifecycle states of a released order, in the order it passes
// through them (ADR 0027, section 4; ADR 0022).
const (
	// LifecycleCommitted: its risk.order.released is in a committed group, so it
	// is durable and possibly sent, and no later input shows the adapter
	// has it.
	LifecycleCommitted OrderState = "committed"
	// LifecycleReleased: a later input is committed. The adapter sends its next
	// input only once it holds the reply to its last, so it holds this
	// order.
	LifecycleReleased OrderState = "released"
	// LifecycleAcknowledged: the venue has reported on it, by a lifecycle report
	// or a fill naming its client order ID that the engine reads (see
	// OrderLifecycles).
	LifecycleAcknowledged OrderState = "acknowledged"
)

// Order is one released order and its lifecycle state.
type Order struct {
	ClientOrderID string
	AccountID     string
	InstrumentID  string
	Side          string
	Quantity      int64
	LimitPrice    float64
	Mode          string
	// Send is false for a dry-run release, which is never sent and so is
	// never acknowledged (ADR 0026, point 3, "Mode").
	Send  bool
	State OrderState
	// CommittedAt is the release decision's recorded time, which is its
	// input's.
	CommittedAt time.Time
	// AcknowledgedAt is when the venue's first report about it occurred, and
	// Acknowledgement that report's status ("fill" for an execution).
	AcknowledgedAt  time.Time
	Acknowledgement string

	committedIn string // the input whose group holds its release
}

// OrderLifecycles returns every released order in the log, in the order
// released, with its lifecycle state, and the venue's reports the engine did
// not read. entries are the log's committed records in recording order,
// inputs and decisions marked as such; strategyID is the run's
// configuration's (AccountOf). A release at a schema version this build does
// not read is refused (ADR 0015).
//
// The log commits an input the engine refused as well as one it read, with
// no mark of which (journal.Recorder.Apply), so a venue's report is read as
// the engine reads it, and only one the engine reads acknowledges an order:
// a lifecycle report that describes the order as released
// (event.LimitOrderLifecyclePayload's Describes), or, in a momentum run, the
// one run that reads fills, a fill of no more than the order's quantity on
// its account, instrument and side. A report that does not validate, or
// does not describe the released order it names, is returned as unread for
// its own alert rather than refusing the log, since the log is append-only
// and would be refused on every later reading. A report naming no released
// order changes nothing: explaining it is reconciliation's (ADR 0019).
func OrderLifecycles(entries []journal.Entry, strategyID string) ([]Order, []UnreadReport, error) {
	var orders []*Order
	var unread []UnreadReport
	byID := map[string]*Order{}
	var group string // the input whose group the current record belongs to
	for _, entry := range entries {
		e := entry.Envelope
		if entry.Kind == journal.KindInput {
			group = e.ID
			for _, o := range orders {
				if o.State == LifecycleCommitted && o.committedIn != group {
					o.State = LifecycleReleased
				}
			}
			if u := acknowledge(e, strategyID, byID); u != nil {
				unread = append(unread, *u)
			}
			continue
		}
		if e.Type != event.OrderReleasedEventType {
			continue
		}
		var p event.OrderReleasedPayload
		if err := read(e, releasedVersions, &p); err != nil {
			return nil, nil, err
		}
		if _, seen := byID[p.ClientOrderID]; seen {
			continue
		}
		o := &Order{
			ClientOrderID: p.ClientOrderID, AccountID: p.AccountID, InstrumentID: p.InstrumentID, Side: p.Side,
			Quantity: p.Quantity, LimitPrice: p.LimitPrice, Mode: p.Mode, Send: p.Send,
			State: LifecycleCommitted, CommittedAt: e.RecordedAt, committedIn: group,
		}
		orders = append(orders, o)
		byID[p.ClientOrderID] = o
	}
	out := make([]Order, len(orders))
	for i, o := range orders {
		out[i] = *o
	}
	return out, unread, nil
}

// UnreadReport is a venue's report the log holds that the engine did not
// read: InputID is the input, ClientOrderID the order it names (empty when
// its payload does not decode), Reason why it was not read, and At when it
// was recorded.
type UnreadReport struct {
	InputID, Type, ClientOrderID, Reason string
	At                                   time.Time
}

// UnreadReportAlerts returns an urgent alert for each venue report the
// engine did not read, one incident per input however often the reporter
// runs (ADR 0019, "Alerting"). Such a report acknowledges nothing, so an
// order it names stays unacknowledged and alerts in its own right.
func UnreadReportAlerts(account string, unread []UnreadReport) []Alert {
	out := make([]Alert, 0, len(unread))
	for _, u := range unread {
		out = append(out, Alert{
			Key: "venue-report-unread:" + u.InputID + "@" + account, Kind: KindVenueReportUnread, Severity: Urgent, AccountID: account, At: u.At,
			Title: fmt.Sprintf("%s: venue report %s not read", account, u.InputID),
			Body: fmt.Sprintf("The log holds the venue's report %s (%s, client order ID %q), recorded at %s, which the engine did not read: %s. It acknowledges nothing. Check IBKR for the order it names before acting on it; the reporter's other alerts are unaffected.",
				u.InputID, u.Type, u.ClientOrderID, u.At.UTC().Format(time.RFC3339), u.Reason),
		})
	}
	return out
}

// acknowledge marks the order a venue's report names as acknowledged, the
// first report only, when the engine reads the report (see OrderLifecycles),
// and returns the report as unread when the engine does not.
func acknowledge(e event.Envelope, strategyID string, byID map[string]*Order) *UnreadReport {
	unread := func(clientID, reason string) *UnreadReport {
		return &UnreadReport{InputID: e.ID, Type: e.Type, ClientOrderID: clientID, Reason: reason, At: e.RecordedAt}
	}
	var o *Order
	var status string
	var at time.Time
	switch {
	case e.Type == event.OrderLifecycleEventType && e.SchemaVersion == event.OrderLifecycleLimitVersion:
		var p event.LimitOrderLifecyclePayload
		if err := read(e, lifecycleVersions, &p); err != nil {
			return unread(p.ClientOrderID, err.Error())
		}
		var ok bool
		if o, ok = byID[p.ClientOrderID]; !ok {
			return nil
		}
		if !p.Describes(o.released()) {
			return unread(p.ClientOrderID, fmt.Sprintf("it reports %s %s %d %s at %v, which does not describe order %s as released (%s %s %d %s at %.2f)",
				p.AccountID, p.Side, p.Quantity, p.InstrumentID, p.LimitPrice, o.ClientOrderID, o.AccountID, o.Side, o.Quantity, o.InstrumentID, o.LimitPrice))
		}
		status, at = p.Status, p.OccurredAt
	case e.Type == event.FillEventType && e.SchemaVersion == event.FillTargetWeightVersion:
		if strategyID != event.TargetWeightStrategyMomentum {
			return nil // a Boost run reads no fill
		}
		var p event.TargetWeightFillPayload
		if err := read(e, targetWeightFillVersions, &p); err != nil {
			return unread(p.ClientOrderID, err.Error())
		}
		var ok bool
		if o, ok = byID[p.ClientOrderID]; !ok {
			return nil
		}
		if p.AccountID != o.AccountID || p.InstrumentID != o.InstrumentID || p.Side != o.Side || p.Quantity > o.Quantity {
			return unread(p.ClientOrderID, fmt.Sprintf("it fills %s %s %d %s, which does not describe order %s as released (%s %s %d %s)",
				p.AccountID, p.Side, p.Quantity, p.InstrumentID, o.ClientOrderID, o.AccountID, o.Side, o.Quantity, o.InstrumentID))
		}
		status, at = "fill", p.FilledAt
	default:
		return nil
	}
	if o.State != LifecycleAcknowledged {
		o.State, o.AcknowledgedAt, o.Acknowledgement = LifecycleAcknowledged, at, status
	}
	return nil
}

// released is the order's identity as its release recorded it, for
// event.LimitOrderLifecyclePayload's Describes.
func (o *Order) released() event.OrderReleasedPayload {
	return event.OrderReleasedPayload{ClientOrderID: o.ClientOrderID, AccountID: o.AccountID, InstrumentID: o.InstrumentID,
		Side: o.Side, Quantity: o.Quantity, LimitPrice: o.LimitPrice}
}

// UnacknowledgedAlerts returns an urgent alert for every order to be sent
// that the venue has not acknowledged within the allowed time of its
// commit, by now, the caller's clock. It is one incident per order, keyed by
// its client order ID, however often the reporter runs. An acknowledged
// order raises nothing; if its alert was already delivered (delivered
// reports a key as sent), a routine notice says it cleared. A release with
// Send false is never sent and never alerts (ADR 0026, point 3, "Mode"; ADR
// 0019, "Alerting").
func UnacknowledgedAlerts(orders []Order, now time.Time, within time.Duration, delivered func(key string) bool) []Alert {
	var out []Alert
	for _, o := range orders {
		if !o.Send {
			continue
		}
		key := "order-unacknowledged:" + o.ClientOrderID + "@" + o.AccountID
		order := fmt.Sprintf("%s %d %s, limit %.2f, client order ID %s", o.Side, o.Quantity, o.InstrumentID, o.LimitPrice, o.ClientOrderID)
		switch {
		case o.State == LifecycleAcknowledged && delivered(key):
			out = append(out, Alert{
				Key: "order-acknowledged:" + o.ClientOrderID + "@" + o.AccountID, Kind: KindAcknowledged, Severity: Routine, AccountID: o.AccountID, At: o.AcknowledgedAt,
				Title: fmt.Sprintf("%s: order %s acknowledged", o.AccountID, o.ClientOrderID),
				Body:  fmt.Sprintf("The venue reported %s (%s) at %s. The earlier unacknowledged-order alert is cleared.", order, o.Acknowledgement, o.AcknowledgedAt.UTC().Format(time.RFC3339)),
			})
		case o.State != LifecycleAcknowledged && now.Sub(o.CommittedAt) >= within:
			out = append(out, Alert{
				Key: key, Kind: KindUnacknowledged, Severity: Urgent, AccountID: o.AccountID, At: now,
				Title: fmt.Sprintf("%s: order %s not acknowledged", o.AccountID, o.ClientOrderID),
				Body:  fmt.Sprintf("Order %s was committed at %s and the venue has not acknowledged it within %s. %s Do not place it by hand: check IBKR for an order whose orderRef is %s first, since a second order with the same tag is unknown to reconciliation (ADR 0019).", order, o.CommittedAt.UTC().Format(time.RFC3339), within, stateAdvice(o.State), o.ClientOrderID),
			})
		}
	}
	return out
}

// stateAdvice says what an unacknowledged order's state means.
func stateAdvice(state OrderState) string {
	if state == LifecycleCommitted {
		return "The log shows no input after it, so the adapter may never have received it: it asks the engine for that input's committed reply again, and the order is served unchanged."
	}
	return "The adapter has received it, so it was sent, lost, or is still waiting at the adapter."
}
