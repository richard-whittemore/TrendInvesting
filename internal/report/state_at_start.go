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
				return Alert{}, false, err
			}
			if p.StrategyID == event.TargetWeightStrategyBoost {
				stopped = nil
				if p.Boost.VenueRejections >= event.BoostVenueRejectionLimit {
					line := boostStopped(e, p.Boost)
					stopped = []string{line, line}
				}
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
		headline = "Normal, Boost's buys stopped"
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

// boostStopped names Boost's stop from the decision that records it in
// force: the orders the venue rejected in a row, and the latch the boost
// holds, which a fresh run's approval states (ADR 0026, points 2, "Boost",
// amended 2026-10-18, and 5).
func boostStopped(e event.Envelope, b event.BoostFigures) string {
	ids := make([]string, 0, len(b.RejectedInARow))
	for _, r := range b.RejectedInARow {
		id := r.ClientOrderID
		if r.VenueOrderID != "" {
			id += " (IBKR " + r.VenueOrderID + ")"
		}
		ids = append(ids, id)
	}
	latch := "off"
	if b.LatchedAfter {
		latch = "on"
	}
	return fmt.Sprintf("Boost's buys are stopped as of %s: the venue rejected %d orders in a row (%s), so no session buys until you start a fresh run; its sells still go. The boost is %s (the latch a fresh run's approval states).",
		e.EventTime.UTC().Format(time.RFC3339), b.VenueRejections, strings.Join(ids, ", "), latch)
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
