package report

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
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
// the engine reads it, and only one the engine reads acknowledges an order.
// A Boost run reads a lifecycle report (version 2) that describes the order
// as released (event.LimitOrderLifecyclePayload's Describes). A momentum
// run reads lifecycle reports and fills
// (version 5) by event.OrderReports, the rule the engine applies: one that
// does not describe the order, takes it past its quantity in total, reuses
// a fill id for another execution, or arrives once the order is terminal is
// refused. A venue report the run would read (a lifecycle report or a fill,
// in either run) that cannot be read at all, at a schema
// version the run does not read or not validating, is returned as unread
// whatever order it names, and its client order ID may be unknown (empty):
// it is refused before the order it names is known. A report that is read
// and refused by the rule above is returned as unread when it names a
// released order. Each unread report gets its own alert rather than refusing
// the log, since the log is append-only and would be refused on every later
// reading. A report that can be read and names no released order changes
// nothing and raises no alert: it is reconciliation's to explain (ADR 0019).
// In a momentum run the owner's clearance (operator.rebalance.cleared) is
// read by event.OrderReports too, as the engine reads it, so a report on an
// order it cleared is refused as on a terminal order. In a Boost run the
// clearance is judged by the Boost count's rule (boostRejections), and one
// the engine refuses, whether the reporter cannot read it or its rule refuses
// it as a whole, is returned as unread too, with its order IDs, and alerts as
// an operator clearance, not a venue report. Any other strategy reads no
// clearance (isBoost, isMomentum).
// A dry-run momentum run's synthetic report on an order it never sent, a
// decision, ends the order by the same rule and acknowledges nothing
// (endSynthetic). A fill in a Boost run is read as the Boost engine reads it,
// by the risk gate's fill rule (risklimit.CheckFill, which
// risklimit.Gate.ObserveFill applies), not by event.OrderReports: the rule
// accepts a fill its order's release describes whatever the lifecycle
// reports before it said, a late fill after a cancellation included, so such
// a fill acknowledges the order (ADR 0022). A fill that cannot be read is
// unread whatever order it names, as above. A fill the rule refuses
// acknowledges nothing: on an order not yet acknowledged the order alerts as
// unacknowledged, and on one already acknowledged, where nothing else would
// alert, it is a report the engine did not read, as in a momentum run.
func OrderLifecycles(entries []journal.Entry, strategyID string) ([]Order, []UnreadReport, error) {
	var orders []*Order
	var unread []UnreadReport
	byID := map[string]*Order{}
	reports := event.NewOrderReports()
	fills := newBoostFills()
	var group string           // the input whose group the current record belongs to
	var boost *boostRejections // the Boost engine's count, to judge a clearance as it does
	if isBoost(strategyID) {
		var envelopes []event.Envelope
		for _, entry := range entries {
			envelopes = append(envelopes, entry.Envelope)
		}
		account, _, _ := AccountOf(envelopes) // unknown: no clearance is judged by account
		boost = newBoostRejections(account, strategyID)
	}
	for _, entry := range entries {
		e := entry.Envelope
		if boost != nil {
			boost.observe(e, entry.Kind)
		}
		if entry.Kind == journal.KindInput {
			group = e.ID
			for _, o := range orders {
				if o.State == LifecycleCommitted && o.committedIn != group {
					o.State = LifecycleReleased
				}
			}
			if e.Type == event.RebalanceClearedEventType {
				switch {
				case boost != nil:
					// The Boost engine refuses a clearance it cannot read
					// and one its rule refuses as a whole.
					if u := boost.judgeClearance(e); u != nil {
						unread = append(unread, *u)
					}
				case isMomentum(strategyID):
					applyClearance(e, reports)
				}
				continue
			}
			if u := acknowledge(e, strategyID, byID, reports, fills); u != nil {
				unread = append(unread, *u)
			}
			continue
		}
		if e.Type == event.OrderLifecycleEventType {
			if err := endSynthetic(e, reports); err != nil {
				return nil, nil, err
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
		reports.Release(p)
		fills.release(p)
		if boost != nil {
			boost.release(p)
		}
	}
	out := make([]Order, len(orders))
	for i, o := range orders {
		out[i] = *o
	}
	return out, unread, nil
}

// endSynthetic reads a dry-run engine's synthetic report on an order it
// released and never sent (event.LimitOrderLifecyclePayload's Synthetic) by
// the rule the engine applied, so the order ends here as it ended there. It
// acknowledges nothing and counts no fill: no venue made it. A synthetic
// report the engine could not have recorded refuses the log, as a release
// that does not read does.
func endSynthetic(e event.Envelope, reports *event.OrderReports) error {
	var p event.LimitOrderLifecyclePayload
	if err := read(e, syntheticVersions, &p); err != nil {
		return err
	}
	if _, err := reports.Lifecycle(p); err != nil {
		return fmt.Errorf("report: %s %q %w", e.Type, e.ID, err)
	}
	return nil
}

// UnreadReport is a venue's report, or the owner's clearance
// (operator.rebalance.cleared), the log holds that the engine did not read:
// InputID is the input, ClientOrderID the order a report names (empty when
// its payload is at a version not read or does not decode), Reason why it
// was not read, and At when it was recorded. For a clearance, OrderIDs are
// the orders it names, as far as its payload can be decoded, and Refused is
// true when it was read and the engine's rule refuses it as a whole, false
// when it could not be read at all.
type UnreadReport struct {
	InputID, Type, ClientOrderID, Reason string
	At                                   time.Time
	OrderIDs                             []string
	Refused                              bool
}

// clearanceTitle is the title of a clearance's alert.
func clearanceTitle(account string, u UnreadReport) string {
	if u.Refused {
		return fmt.Sprintf("%s: operator clearance %s refused", account, u.InputID)
	}
	return fmt.Sprintf("%s: operator clearance %s not read", account, u.InputID)
}

// orderList names a clearance's orders, or says they are unknown.
func orderList(ids []string) string {
	if len(ids) == 0 {
		return "not known"
	}
	return strings.Join(ids, ", ")
}

// UnreadReportAlerts returns an urgent alert for each venue report, and each
// operator clearance, the engine did not read, one incident per input however often the reporter
// runs (ADR 0019, "Alerting"). Such a report acknowledges nothing, so an
// order it names stays unacknowledged and alerts in its own right.
func UnreadReportAlerts(account string, unread []UnreadReport) []Alert {
	out := make([]Alert, 0, len(unread))
	for _, u := range unread {
		if u.Type == event.RebalanceClearedEventType {
			verdict := "not read"
			if u.Refused {
				verdict = "refused"
			}
			out = append(out, Alert{
				Key: "venue-report-unread:" + u.InputID + "@" + account, Kind: KindVenueReportUnread, Severity: Urgent, AccountID: account, At: u.At,
				Title: clearanceTitle(account, u),
				Body: fmt.Sprintf("The log holds your clearance %s (orders %s), recorded at %s, which the engine did not apply (%s): %s. It clears nothing, so each order it names still counts as awaiting the venue's word. Check which Boost orders were never sent before clearing them again; the reporter's other alerts are unaffected.",
					u.InputID, orderList(u.OrderIDs), u.At.UTC().Format(time.RFC3339), verdict, u.Reason),
			})
			continue
		}
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
// and returns the report as unread when the engine refuses it. reports
// holds the run's releases for the momentum run's rule.
func acknowledge(e event.Envelope, strategyID string, byID map[string]*Order, reports *event.OrderReports, fills *boostFills) *UnreadReport {
	unread := func(clientID, reason string) *UnreadReport {
		return &UnreadReport{InputID: e.ID, Type: e.Type, ClientOrderID: clientID, Reason: reason, At: e.RecordedAt}
	}
	momentum := isMomentum(strategyID)
	var client, status string
	var at time.Time
	var refused error
	switch {
	case e.Type == event.OrderLifecycleEventType:
		var p event.LimitOrderLifecyclePayload
		if err := read(e, lifecycleVersions, &p); err != nil {
			return unread(p.ClientOrderID, err.Error())
		}
		client, status, at = p.ClientOrderID, p.Status, p.OccurredAt
		if momentum {
			_, refused = reports.Lifecycle(p)
		} else if o, ok := byID[client]; ok && !p.Describes(o.released()) {
			refused = fmt.Errorf("it reports %s %s %d %s at %v, which does not describe order %s as released (%s %s %d %s at %.2f)",
				p.AccountID, p.Side, p.Quantity, p.InstrumentID, p.LimitPrice, o.ClientOrderID, o.AccountID, o.Side, o.Quantity, o.InstrumentID, o.LimitPrice)
		}
	case e.Type == event.FillEventType && momentum:
		var p event.TargetWeightFillPayload
		if err := read(e, targetWeightFillVersions, &p); err != nil {
			return unread(p.ClientOrderID, err.Error())
		}
		client, status, at = p.ClientOrderID, "fill", p.FilledAt
		_, _, refused = reports.Fill(p)
	case e.Type == event.FillEventType:
		var p event.TargetWeightFillPayload
		if err := read(e, targetWeightFillVersions, &p); err != nil {
			// One that cannot be read is unread whatever order it names, as
			// in a momentum run: its client order ID may be unknown, and
			// its alert is keyed by its input's ID.
			return unread(p.ClientOrderID, err.Error())
		}
		if err := fills.accept(p); err != nil {
			// A fill the Boost engine's rule refuses acknowledges nothing.
			if o, ok := byID[p.ClientOrderID]; ok && o.State == LifecycleAcknowledged {
				return unread(p.ClientOrderID, err.Error())
			}
			return nil
		}
		client, status, at = p.ClientOrderID, "fill", p.FilledAt
	default:
		return nil // not a venue report
	}
	o, ok := byID[client]
	switch {
	case !ok || errors.Is(refused, event.ErrNotReleased):
		return nil // reconciliation's to explain (ADR 0019)
	case refused != nil:
		return unread(client, refused.Error())
	}
	if o.State != LifecycleAcknowledged {
		o.State, o.AcknowledgedAt, o.Acknowledgement = LifecycleAcknowledged, at, status
	}
	return nil
}

// boostFills reads a Boost run's fills by the rule the Boost engine's gate
// applies (risklimit.CheckFill), so the reporter and the engine cannot
// disagree about which fill counts. The rule needs no limits, so no gate is
// built: it holds each released order as the rule reads it and the fills
// applied, by account and fill ID, as the gate does.
type boostFills struct {
	orders  map[string]*risklimit.FillOrder
	applied map[string]map[string]event.TargetWeightFillPayload
}

func newBoostFills() *boostFills {
	return &boostFills{orders: map[string]*risklimit.FillOrder{}, applied: map[string]map[string]event.TargetWeightFillPayload{}}
}

// release records a released order, as the engine's gate does. OrderLifecycles
// passes each client order ID's first release only, read and validated (a
// named order of positive shares), so every release is one the rule holds.
func (b *boostFills) release(p event.OrderReleasedPayload) {
	b.orders[p.ClientOrderID] = &risklimit.FillOrder{AccountID: p.AccountID, ClientOrderID: p.ClientOrderID,
		InstrumentID: p.InstrumentID, Side: p.Side, Quantity: p.Quantity}
}

// accept applies fill p when the rule accepts it, or is a redelivery of an
// execution already applied, and returns the rule's refusal otherwise.
func (b *boostFills) accept(p event.TargetWeightFillPayload) error {
	o := b.orders[p.ClientOrderID]
	account := p.AccountID
	if o != nil {
		account = o.AccountID
	}
	redelivery, err := risklimit.CheckFill(p, account, o, b.applied[account])
	if err != nil || redelivery {
		return err
	}
	o.Reported += p.Quantity
	if b.applied[account] == nil {
		b.applied[account] = map[string]event.TargetWeightFillPayload{}
	}
	b.applied[account][p.FillID] = p
	return nil
}

// applyClearance applies the owner's clearance as the momentum engine
// does, by event.OrderReports' Clear, so the orders it made terminal there
// are terminal here. A clearance the engine refuses, one it cannot read
// included, clears nothing and raises no alert: it is not a venue report.
func applyClearance(e event.Envelope, reports *event.OrderReports) {
	var p event.RebalanceClearedPayload
	if read(e, clearedVersions, &p) == nil {
		_ = reports.Clear(p)
	}
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
