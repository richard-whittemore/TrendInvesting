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
	// or a fill naming its client order ID.
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
// released, with its lifecycle state. entries are the log's committed
// records in recording order, inputs and decisions marked as such. A record
// at a schema version this build does not read is refused (ADR 0015). A
// venue's report naming no released order changes nothing: explaining it is
// reconciliation's (ADR 0019).
func OrderLifecycles(entries []journal.Entry) ([]Order, error) {
	var orders []*Order
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
			if err := acknowledge(e, byID); err != nil {
				return nil, err
			}
			continue
		}
		if e.Type != event.OrderReleasedEventType {
			continue
		}
		var p event.OrderReleasedPayload
		if err := read(e, releasedVersions, &p); err != nil {
			return nil, err
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
	return out, nil
}

// acknowledge marks the order a venue's report names as acknowledged, the
// first report only.
func acknowledge(e event.Envelope, byID map[string]*Order) error {
	var clientID, status string
	var at time.Time
	switch {
	case e.Type == event.OrderLifecycleEventType && e.SchemaVersion == event.OrderLifecycleLimitVersion:
		var p event.LimitOrderLifecyclePayload
		if err := read(e, lifecycleVersions, &p); err != nil {
			return err
		}
		clientID, status, at = p.ClientOrderID, p.Status, p.OccurredAt
	case e.Type == event.FillEventType && e.SchemaVersion == event.FillTargetWeightVersion:
		var p event.TargetWeightFillPayload
		if err := read(e, targetWeightFillVersions, &p); err != nil {
			return err
		}
		clientID, status, at = p.ClientOrderID, "fill", p.FilledAt
	default:
		return nil
	}
	o, ok := byID[clientID]
	if !ok || o.State == LifecycleAcknowledged {
		return nil
	}
	o.State, o.AcknowledgedAt, o.Acknowledgement = LifecycleAcknowledged, at, status
	return nil
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
