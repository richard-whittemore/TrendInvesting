package report

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
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
	account, strategy, err := AccountOf(envelopes)
	if err != nil {
		return Alert{}, false, err
	}
	var state, stopped, unreadable, refused, unreadClearances, killed, pending, alarm []string // each the private line, then the public one
	headline := "Normal"
	rej := newBoostRejections(account, strategy)
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
			state, stopped, unreadable, headline = nil, nil, nil, "Normal"
			rej.freshRun()
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
				// closed, and still send everything else in force. A stop
				// already known stays named with its orders; a momentum
				// account has no Boost stop.
				line := fmt.Sprintf("The decision record %s cannot be read: treat the buy stop as possibly in force.", e.ID)
				if isMomentum(strategy) {
					line = fmt.Sprintf("The decision record %s cannot be read: check the account's latest decision before relying on it.", e.ID)
				}
				unreadable = []string{line, line}
				continue
			}
			if isBoost(p.StrategyID) {
				stopped, unreadable = nil, nil
				lastBoost, lastFigures = e, p.Boost
				rej.decided(e.ID, p.Boost)
				if rej.verdicts.Stopped() {
					line := boostStopped(e.EventTime, lastFigures, rej.verdicts)
					stopped = []string{line, line}
				}
			}
		case event.OrderReleasedEventType:
			var p event.OrderReleasedPayload
			if read(e, releasedVersions, &p) == nil {
				rej.release(p)
			}
		case event.OrderRetiredEventType:
			rej.retire(e)
		case event.OrderLifecycleEventType, event.FillEventType, event.RebalanceClearedEventType:
			if rej.venue(e) == targetweight.VerdictRejected && lastBoost.ID != "" && rej.verdicts.Stopped() {
				// The decision that records the stop comes with the next
				// session; the second rejection is in force from its report.
				line := boostStopped(e.EventTime, lastFigures, rej.verdicts)
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
	if headline == "Normal" {
		switch {
		case stopped != nil:
			headline = "Normal, Boost's buys stopped"
		case unreadable != nil && isMomentum(strategy):
			headline = "Normal, a decision record cannot be read"
		case unreadable != nil:
			headline = "Normal, Boost's buys possibly stopped"
		}
	}
	if len(rej.refused) > 0 {
		line := fmt.Sprintf("The clearance record(s) %s cannot be applied (the engine refuses them): check which Boost orders were never sent.", strings.Join(rej.refused, ", "))
		refused = []string{line, line}
	}
	if len(rej.unread) > 0 {
		line := fmt.Sprintf("The clearance record(s) %s cannot be read (the engine refuses a clearance it cannot read): check which Boost orders were never sent.", strings.Join(rej.unread, ", "))
		unreadClearances = []string{line, line}
	}
	for _, l := range [][]string{state, stopped, unreadable, refused, unreadClearances, killed, pending, alarm} {
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

// boostRejections counts the venue's rejections of Boost's orders by the
// engine's own rule (targetweight.BoostVerdicts; ADR 0026, point 2, "Boost",
// amended 2026-10-18): from each Boost decision's recorded count and the
// orders it lists as awaiting the venue's word (point 5: recovered, never
// recomputed), then the venue's words journalled after it, each read only
// when the engine reads it: a report that describes the order as released
// (ADR 0022), a fill the gate's fill rule accepts (risklimit.CheckFill) and
// a clearance or a retirement the engine applies.
type boostRejections struct {
	account    string
	strategy   string // the account's, from its configuration
	verdicts   targetweight.BoostVerdicts
	instrument string                                // the Boost decision's instrument, for a clearance
	boostTags  map[string]bool                       // IDs of the Boost decisions seen
	released   map[string]event.OrderReleasedPayload // each order's first release
	fills      *boostFills
	refused    []string // clearances the engine refuses, named on the page
	unread     []string // clearances the reporter cannot read or validate, named on the page (the reporter also pages each as an unread record)
}

func newBoostRejections(account, strategy string) *boostRejections {
	return &boostRejections{account: account, strategy: strategy, boostTags: map[string]bool{}, released: map[string]event.OrderReleasedPayload{}, fills: newBoostFills()}
}

// freshRun starts the count a fresh run starts with: none (ADR 0026, point
// 2, "Boost", amended 2026-10-18; ADR 0027).
func (r *boostRejections) freshRun() {
	r.verdicts, r.boostTags, r.refused, r.unread = targetweight.BoostVerdicts{}, map[string]bool{}, nil, nil
}

// decided takes a Boost decision's recorded count as the count in force.
func (r *boostRejections) decided(id string, b event.BoostFigures) {
	r.boostTags[id] = true
	r.instrument = b.InstrumentID
	r.verdicts.Recover(b)
}

// release records an order's first release, and an order a Boost decision
// released to a venue as awaiting the venue's word.
func (r *boostRejections) release(p event.OrderReleasedPayload) {
	if _, ok := r.released[p.ClientOrderID]; !ok {
		r.released[p.ClientOrderID] = p
		r.fills.release(p)
	}
	if r.boostTags[p.Tag] && p.Send {
		r.verdicts.Release(p.ClientOrderID)
	}
}

// venue reads a venue report, fill or clearance e as the engine reads it,
// and returns what it did to the count. A record this build cannot read,
// or another account's, or one the engine refuses, does nothing.
func (r *boostRejections) venue(e event.Envelope) targetweight.Verdict {
	switch e.Type {
	case event.OrderLifecycleEventType:
		var p event.LimitOrderLifecyclePayload
		if e.SchemaVersion != event.OrderLifecycleLimitVersion || read(e, lifecycleVersions, &p) != nil || p.AccountID != r.account {
			return targetweight.VerdictNone
		}
		if o, ok := r.released[p.ClientOrderID]; !ok || !p.Describes(o) {
			return targetweight.VerdictNone
		}
		r.verdicts.LearnVenueID(p.ClientOrderID, p.OrderID)
		if rejected, decides := targetweight.BoostLifecycleWord(p.Status); decides {
			return r.verdicts.Observe(p.ClientOrderID, rejected)
		}
	case event.FillEventType:
		var p event.TargetWeightFillPayload
		if read(e, targetWeightFillVersions, &p) != nil || p.AccountID != r.account || r.fills.accept(p) != nil {
			return targetweight.VerdictNone
		}
		// An execution shows the venue accepted the order.
		return r.verdicts.Observe(p.ClientOrderID, false)
	case event.RebalanceClearedEventType:
		// Only a Boost account's clearance runs through the Boost count,
		// whatever the run has decided so far: the momentum engine applies
		// it through its own order reports.
		if !isBoost(r.strategy) {
			return targetweight.VerdictNone
		}
		if u := r.judgeClearance(e); u != nil {
			if u.Refused {
				r.refused = append(r.refused, e.ID)
			} else {
				r.unread = append(r.unread, e.ID)
			}
		}
	}
	return targetweight.VerdictNone
}

// judgeClearance reads the owner's clearance e as the Boost engine does and
// applies it to the count, returning it as unread when the engine refuses it:
// Refused false when it cannot decode or validate it, true when its rule
// (targetweight.BoostVerdicts.ClearNeverSent) refuses it as a whole. A
// clearance naming another account is refused as the engine refuses it.
func (r *boostRejections) judgeClearance(e event.Envelope) *UnreadReport {
	var p event.RebalanceClearedPayload
	if err := read(e, clearedVersions, &p); err != nil {
		// Whatever the payload names, for the alert.
		var named event.RebalanceClearedPayload
		_ = json.Unmarshal(e.Payload, &named)
		return &UnreadReport{InputID: e.ID, Type: e.Type, Reason: err.Error(), At: e.RecordedAt, OrderIDs: named.ClientOrderIDs}
	}
	if p.AccountID != r.account {
		return &UnreadReport{InputID: e.ID, Type: e.Type, Reason: fmt.Sprintf("clearance %s is for account %q; this run decides %q", e.ID, p.AccountID, r.account), At: e.RecordedAt, OrderIDs: p.ClientOrderIDs, Refused: true}
	}
	sideOf := func(id string) string { return r.released[id].Side }
	if err := r.verdicts.ClearNeverSent(e.ID, p, r.instrument, sideOf); err != nil {
		return &UnreadReport{InputID: e.ID, Type: e.Type, Reason: err.Error(), At: e.RecordedAt, OrderIDs: p.ClientOrderIDs, Refused: true}
	}
	return nil
}

// retire reads a retirement e (execution.order.retired) of an order never
// sent as the Boost engine does, and reports whether the engine applies it:
// it must read, be this account's, describe an order released as released,
// and pass the count's rule (targetweight.BoostVerdicts.Retire), which takes
// the order off those awaiting the venue's word, so a later report on it,
// which the engine refuses, counts for nothing (ADR 0026, point 2, "Boost",
// amended 2026-10-22). Any other retirement changes nothing.
func (r *boostRejections) retire(e event.Envelope) bool {
	var p event.OrderRetiredPayload
	if read(e, retiredVersions, &p) != nil || p.AccountID != r.account {
		return false
	}
	if o, ok := r.released[p.ClientOrderID]; !ok || !p.Describes(o) {
		return false
	}
	return r.verdicts.Retire(e.ID, p.ClientOrderID) == nil
}

// observe keeps the count as the log's records move it, in recording order
// (kind is the record's journal kind): a fresh run's approval ends it, a Boost
// decision recovers it, a release adds to it elsewhere, and a venue's report
// or fill moves it. A clearance is judged by judgeClearance.
func (r *boostRejections) observe(e event.Envelope, kind string) {
	switch {
	case e.Type == event.OperatorRunApprovedEventType:
		r.freshRun()
	case e.Type == event.TargetsDecidedEventType:
		var p event.TargetsDecidedPayload
		if read(e, decidedVersions, &p) == nil && isBoost(p.StrategyID) {
			r.decided(e.ID, p.Boost)
		}
	case kind == journal.KindInput && (e.Type == event.OrderLifecycleEventType || e.Type == event.FillEventType):
		r.venue(e)
	}
}

// boostStopped names Boost's stop: the count the engine holds, the orders
// it names, and whether the boost is on, which a fresh run's approval
// states (ADR 0026, points 2, "Boost", amended 2026-10-18, and 5).
func boostStopped(at time.Time, b event.BoostFigures, v targetweight.BoostVerdicts) string {
	state := "off"
	if b.LatchedAfter {
		state = "on"
	}
	var ids []string
	for _, o := range v.InARow() {
		ids = append(ids, orderName(o))
	}
	return fmt.Sprintf("Boost's buys are stopped as of %s: the venue rejected %d orders in a row (%s), so no session buys until you start a fresh run; its sells still go. The boost is %s (a fresh run's approval states it).",
		at.UTC().Format(time.RFC3339), v.Rejections(), strings.Join(ids, ", "), state)
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
