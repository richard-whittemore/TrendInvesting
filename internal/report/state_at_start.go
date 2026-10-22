package report

import (
	"fmt"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// KindStateAtStart is the account's standing told to a delivery channel that
// begins after the log's earlier events, which it is not sent as history.
const KindStateAtStart Kind = "state-at-start"

// StateAtStartTitle is how the title of a delivery to such a channel begins,
// for its standing and for each open alert raised before the channel began.
const StateAtStartTitle = "state at reporter start: "

// killSwitchVersions are the operator's kill-switch records this build reads.
var killSwitchVersions = []uint32{event.KillSwitchSchemaVersion}

// StateAtStartAlert is the account's standing at the end of its log, for a
// delivery channel that begins after the log's earlier events and so is sent
// none of them as history: an incident still in force must still reach the
// owner (ADR 0019, "Alerting": an alert is a safety requirement). It names
// the engine state in force when it is Degraded or Halted, with its scope
// (from the latest engine-state record, or the incident a fresh run's
// approval carried in force), Boost's buys when two venue rejections in a
// row stop them, with the latch it holds (from the latest Boost decision),
// the kill switch when engaged, a pending cash
// read the latest reconciliation left undecided, the margin-cushion alarm
// when not clear, and every open alert (repeating and not acknowledged),
// which follow it. A kill-switch record that does not read is named as
// possibly engaged: the gate refuses every order while it is (ADR 0026,
// point 3). ok is false when nothing is in force, since a page means an
// incident: the alert then carries only its key and account.
// Its public text carries no figure (owner decision 2026-10-17,
// docs/live-trading/plan.md section 7).
func StateAtStartAlert(envelopes []event.Envelope, alerts []Alert) (Alert, bool, error) {
	account, _, err := AccountOf(envelopes)
	if err != nil {
		return Alert{}, false, err
	}
	var state, stopped, killed, pending, alarm []string // each the private line, then the public one
	headline := "Normal"
	var rej boostRejections
	var lastBoost event.Envelope
	var lastFigures event.BoostFigures
	for _, e := range envelopes {
		switch e.Type {
		case event.OperatorRunApprovedEventType:
			var p event.OperatorRunApprovedPayload
			if err := read(e, approvalVersions, &p); err != nil {
				return Alert{}, false, err
			}
			// A fresh run ends Boost's stop (ADR 0026, point 2, "Boost",
			// amended 2026-10-18).
			state, stopped, headline = nil, nil, "Normal"
			rej = boostRejections{}
			if in := p.IncidentInForce; in != nil {
				headline = stateName(in.State)
				line := fmt.Sprintf("The engine is %s, carried in force by the run's approval (incident %s)%s.",
					stateName(in.State), in.IncidentID, scopeOf(in.ScopeInstrumentIDs, nil, in.ScopeCashComponents))
				state = []string{line, line}
			}
		case event.EngineStateEventType:
			var p event.EngineStatePayload
			if err := read(e, engineStateVersions, &p); err != nil {
				return Alert{}, false, err
			}
			line := fmt.Sprintf("The engine is %s since %s (%s)%s.", stateName(p.State), e.EventTime.UTC().Format(time.RFC3339), p.Reason,
				scopeOf(p.ScopeInstrumentIDs, p.ScopeOrderIDs, p.ScopeCashComponents))
			state, headline = []string{line + " " + p.Detail + ".", line}, stateName(p.State)
		case event.TargetsDecidedEventType:
			var p event.TargetsDecidedPayload
			if err := read(e, decidedVersions, &p); err != nil {
				// It may be the decision that stopped Boost's buys: fail
				// closed, and still send everything else in force.
				line := fmt.Sprintf("The decision record %s cannot be read: treat the buy stop as possibly in force.", e.ID)
				stopped = []string{line, line}
				continue
			}
			if p.StrategyID == event.TargetWeightStrategyBoost {
				stopped = nil
				lastBoost, lastFigures = e, p.Boost
				rej.decided(e.ID, p.Boost)
				if rej.count >= event.BoostVenueRejectionLimit {
					line := boostStopped(e.EventTime, lastFigures, rej.ids)
					stopped = []string{line, line}
				}
			}
		case event.OrderReleasedEventType:
			var p event.OrderReleasedPayload
			if read(e, releasedVersions, &p) == nil {
				rej.release(p)
			}
		case event.OrderLifecycleEventType:
			var p event.LimitOrderLifecyclePayload
			if e.SchemaVersion != event.OrderLifecycleLimitVersion || read(e, lifecycleVersions, &p) != nil || p.AccountID != account {
				continue
			}
			if rej.report(p) && lastBoost.ID != "" && rej.count >= event.BoostVenueRejectionLimit {
				// The decision that records the stop comes with the next
				// session; the second rejection is in force from its report.
				line := boostStopped(e.EventTime, lastFigures, rej.ids)
				stopped = []string{line, line}
			}
		case event.KillSwitchEventType:
			var p event.KillSwitchPayload
			if err := read(e, killSwitchVersions, &p); err != nil {
				line := fmt.Sprintf("The kill switch's latest record (%s) cannot be read, so the kill switch may be engaged.", e.ID)
				killed = []string{line, line}
				continue
			}
			if p.AccountID != account {
				continue
			}
			killed = nil
			if p.Engaged {
				line := fmt.Sprintf("The kill switch is engaged since %s, by %s: every order is refused but the one sale back to 98%%.", p.AsOf.UTC().Format(time.RFC3339), p.Operator)
				killed = []string{line + " " + p.Detail + ".", line}
			}
		case event.ReconciliationResultEventType:
			var p event.ReconciliationResultPayload
			if err := read(e, reconciliationVersions, &p); err != nil {
				return Alert{}, false, err
			}
			pending = nil
			if p.Action == event.ReconciliationActionAlert {
				line := fmt.Sprintf("A pending cash read: reconciliation %s found the cash different from the book on a day the Flex reader has not covered.", p.ReconciliationID)
				pending = []string{line, line}
			}
		case event.MarginAlarmEventType:
			var p event.MarginAlarmPayload
			if err := read(e, marginAlarmVersions, &p); err != nil {
				return Alert{}, false, err
			}
			alarm = nil
			if p.Level != event.MarginAlarmClear {
				line := fmt.Sprintf("The margin-cushion alarm is %s since %s.", p.Level, p.AsOf.UTC().Format(time.RFC3339))
				alarm = []string{line + " " + p.Detail + ".", line}
			}
		}
	}
	var open []string
	for _, a := range alerts {
		if a.Repeat && !a.Acknowledged {
			open = append(open, a.Title)
		}
	}
	var body, public []string
	if stopped != nil && headline == "Normal" {
		headline = "Normal, Boost's buys stopped or possibly stopped"
	}
	for _, l := range [][]string{state, stopped, killed, pending, alarm} {
		if l != nil {
			body, public = append(body, l[0]), append(public, l[1])
		}
	}
	key := "state-at-start:" + account
	if len(body) == 0 && len(open) == 0 {
		return Alert{Key: key, Kind: KindStateAtStart, AccountID: account}, false, nil
	}
	if len(open) > 0 {
		line := fmt.Sprintf("%d open alert(s) follow, each repeating until you acknowledge it: %s.", len(open), strings.Join(open, "; "))
		body, public = append(body, line), append(public, line)
	}
	end := " This is the account's standing when the reporter began delivering to this channel; the log's earlier events are not sent to it again. Check the account on the dashboard or with journal-query."
	return Alert{
		Key: key, Kind: KindStateAtStart, Severity: Urgent, AccountID: account,
		Title:      fmt.Sprintf("%s: state at reporter start: %s", account, headline),
		Body:       strings.Join(body, " ") + end,
		PublicBody: strings.Join(public, " ") + end,
	}, true, nil
}

// boostRejections follows the venue's rejections of Boost's orders as the
// engine counts them (ADR 0026, point 2, "Boost", amended 2026-10-18), from
// each Boost decision's count and the venue's reports journalled after it:
// the first word on an order a decided session released (a rejection counts
// one more; an acceptance of an order released after every rejection counted
// sets the count back; neither changes anything once buys are stopped).
type boostRejections struct {
	count      int
	ids        []string
	boostTags  map[string]bool // IDs of the Boost decisions seen
	released   map[string]int  // client order ID to its release's place, for orders a Boost decision released
	undecided  map[string]bool // released and not yet ruled on by the venue
	lastReject int             // place of the latest released rejection counted, -1 when none
	next       int
}

// decided takes a Boost decision's count as the count in force.
func (r *boostRejections) decided(id string, b event.BoostFigures) {
	if r.boostTags == nil {
		r.boostTags, r.released, r.undecided = map[string]bool{}, map[string]int{}, map[string]bool{}
	}
	r.boostTags[id] = true
	r.count, r.ids, r.lastReject = b.VenueRejections, nil, -1
	for _, o := range b.RejectedInARow {
		r.ids = append(r.ids, orderName(o))
		if at, ok := r.released[o.ClientOrderID]; ok {
			r.lastReject = max(r.lastReject, at)
		}
	}
}

// release holds an order a Boost decision released as one the venue has not
// yet ruled on.
func (r *boostRejections) release(p event.OrderReleasedPayload) {
	if !r.boostTags[p.Tag] || !p.Send {
		return
	}
	if _, ok := r.released[p.ClientOrderID]; ok {
		return
	}
	r.released[p.ClientOrderID] = r.next
	r.undecided[p.ClientOrderID] = true
	r.next++
}

// report reads the venue's report p and says whether it added a rejection.
func (r *boostRejections) report(p event.LimitOrderLifecyclePayload) bool {
	if !r.undecided[p.ClientOrderID] || p.Status == event.OrderLifecycleStatusSubmitted {
		return false
	}
	delete(r.undecided, p.ClientOrderID)
	if r.count >= event.BoostVenueRejectionLimit {
		return false
	}
	at := r.released[p.ClientOrderID]
	if p.Status != event.OrderLifecycleStatusInvalid {
		if at > r.lastReject {
			r.count, r.ids, r.lastReject = 0, nil, -1
		}
		return false
	}
	r.count++
	r.ids = append(r.ids, orderName(event.BoostRejectedOrder{ClientOrderID: p.ClientOrderID, VenueOrderID: p.OrderID}))
	r.lastReject = max(r.lastReject, at)
	return true
}

// boostStopped names Boost's stop: the orders the venue rejected in a row,
// and whether the boost is on, which a fresh run's approval states (ADR
// 0026, points 2, "Boost", amended 2026-10-18, and 5).
func boostStopped(at time.Time, b event.BoostFigures, ids []string) string {
	state := "off"
	if b.LatchedAfter {
		state = "on"
	}
	return fmt.Sprintf("Boost's buys are stopped as of %s: the venue rejected %d orders in a row (%s), so no session buys until you start a fresh run; its sells still go. The boost is %s (a fresh run's approval states it).",
		at.UTC().Format(time.RFC3339), len(ids), strings.Join(ids, ", "), state)
}

// stateName is an engine state as the owner's manual names it. A record
// that reads states Halted or Degraded and no other (event.EngineStatePayload
// and event.IncidentInForce validate it).
func stateName(state string) string {
	if state == event.EngineStateHalted {
		return "Halted"
	}
	return "Degraded"
}

// scopeOf names a Degraded state's scope, its non-empty parts only; empty
// for a Halted state, which covers the whole account.
func scopeOf(instruments, orders, cash []string) string {
	var parts []string
	for _, p := range []struct {
		name string
		ids  []string
	}{{"instruments", instruments}, {"orders", orders}, {"cash", cash}} {
		if len(p.ids) > 0 {
			parts = append(parts, p.name+" ["+strings.Join(p.ids, ",")+"]")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " on " + strings.Join(parts, " and ")
}
