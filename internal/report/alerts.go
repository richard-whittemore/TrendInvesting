package report

import (
	"fmt"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/nysecalendar"
)

// Severity is how loudly an alert is delivered.
type Severity string

// The two severities of docs/live-trading/plan.md section 3: an urgent alert
// needs a person now, a routine one can wait for the daily read.
const (
	Urgent  Severity = "urgent"
	Routine Severity = "routine"
)

// Kind is what an alert reports.
type Kind string

// The incident types of ADR 0019's alerting: every refusal, Degraded and
// Halted, withheld decisions, missing data and heartbeat loss.
const (
	KindRefusal     Kind = "refusal"
	KindDegraded    Kind = "degraded"
	KindHalted      Kind = "halted"
	KindWithheld    Kind = "withheld"
	KindMissingData Kind = "missing-data"
	KindHeartbeat   Kind = "heartbeat-lost"
	// KindJournalRefused is the reporter's own failure to read the journal:
	// every alert the journal holds is withheld until it reads.
	KindJournalRefused Kind = "journal-refused"
	// KindHeartbeatUnchecked is the reporter's own failure to check the
	// heartbeat, such as a span the calendar does not cover.
	KindHeartbeatUnchecked Kind = "heartbeat-unchecked"
	// KindUnacknowledged is an order to be sent that the venue has not
	// acknowledged within the configured time, and KindAcknowledged the
	// notice that one already alerted has since been acknowledged.
	KindUnacknowledged Kind = "order-unacknowledged"
	KindAcknowledged   Kind = "order-acknowledged"
)

// Alert is one incident to tell the owner about. Key names the incident:
// two alerts with the same Key are the same incident and are delivered once.
type Alert struct {
	Key       string
	Kind      Kind
	Severity  Severity
	AccountID string
	At        time.Time
	Title     string
	Body      string
}

// Alerts returns the journal's alerts in recording order, each incident once.
//
//   - Every risk.order.refused is its own incident.
//   - A strategy.engine.state record is an incident when its state, reason and
//     scope differ from the previous one: a repeat of the same state is the
//     same incident, and Degraded to Halted is a new one (ADR 0019). The
//     incident is keyed by its start, so a state that returns after another
//     is a new incident too.
//   - A run of consecutive withheld decisions with one reason is one
//     incident; a decided outcome ends it. Reasons that name absent inputs
//     (missing bars, stale snapshot, missing universe) and a refusal for a
//     missing mark are missing data; every other withheld reason is a
//     withheld decision.
func Alerts(envelopes []event.Envelope) ([]Alert, error) {
	account, _, err := AccountOf(envelopes)
	if err != nil {
		return nil, err
	}
	var out []Alert
	var lastState, runReason string
	var runStart time.Time
	for _, e := range envelopes {
		switch e.Type {
		case event.OrderRefusedEventType:
			var p event.OrderRefusedPayload
			if err := read(e, refusedVersions, &p); err != nil {
				return nil, err
			}
			out = append(out, refusalAlerts(account, e, p)...)
		case event.EngineStateEventType:
			var p event.EngineStatePayload
			if err := read(e, engineStateVersions, &p); err != nil {
				return nil, err
			}
			key := engineStateKey(p)
			if key == lastState {
				continue
			}
			lastState = key
			out = append(out, engineStateAlert(account, e, p))
		case event.TargetsDecidedEventType:
			var p event.TargetsDecidedPayload
			if err := read(e, decidedVersions, &p); err != nil {
				return nil, err
			}
			if p.Outcome != event.TargetsOutcomeWithheld {
				runReason = ""
				continue
			}
			if p.WithheldReason == runReason {
				continue
			}
			runReason, runStart = p.WithheldReason, e.EventTime
			out = append(out, withheldAlert(account, e, p, runStart))
		}
	}
	return out, nil
}

func refusalAlerts(account string, e event.Envelope, p event.OrderRefusedPayload) []Alert {
	out := []Alert{{
		Key: "refusal:" + e.ID, Kind: KindRefusal, Severity: Urgent, AccountID: account, At: e.EventTime,
		Title: fmt.Sprintf("%s: order refused (%s)", account, p.Reason),
		Body: fmt.Sprintf("Risk gate refused %s %d %s, limit %.2f, order %s: %s. %s",
			p.Side, p.Quantity, p.InstrumentID, p.LimitPrice, p.ClientOrderID, p.Reason, p.Detail),
	}}
	if p.Reason == event.RefusedMissingMark {
		out = append(out, Alert{
			Key: "missing-data:mark:" + e.ID, Kind: KindMissingData, Severity: Urgent, AccountID: account, At: e.EventTime,
			Title: fmt.Sprintf("%s: no recorded mark for %s", account, p.InstrumentID),
			Body:  fmt.Sprintf("The gate had no market.mark.recorded for %s, so order %s was refused.", p.InstrumentID, p.ClientOrderID),
		})
	}
	return out
}

func engineStateKey(p event.EngineStatePayload) string {
	scope := strings.Join(p.ScopeInstrumentIDs, ",") + "|" + strings.Join(p.ScopeOrderIDs, ",") + "|" + strings.Join(p.ScopeCashComponents, ",")
	return "engine-state:" + p.State + ":" + p.Reason + ":" + scope
}

// engineStateAlert states what the system is still and no longer doing, per
// ADR 0019's "Every alert states" list, as far as the record itself knows.
//
// Its key is the state, reason and scope with the incident's start, so a
// state that returns after another is a new incident, never one already
// delivered.
func engineStateAlert(account string, e event.Envelope, p event.EngineStatePayload) Alert {
	kind := KindHalted
	body := fmt.Sprintf("Engine halted since %s: %s. %s. Whole engine. It places no new orders; the one sale back toward 98%% for a target-weight account (ADR 0019 amendment) is the only exception. Recovery is a person's: docs/runbooks/reconciliation.md.",
		e.EventTime.UTC().Format(time.RFC3339), p.Reason, p.Detail)
	if p.State == event.EngineStateDegraded {
		kind = KindDegraded
		body = fmt.Sprintf("Engine degraded since %s: %s. %s. Scope: instruments [%s], orders [%s], cash [%s]. It opens no new risk and refuses buys. The instruments in the scope are frozen: no order for them except restoring a missing Protective Stop, or, for a target-weight account, the one sale back toward 98%% (ADR 0019 amendment, 2026-10-16). Risk-reducing orders continue only for instruments outside the scope. Check the protection of the named positions at the broker. Recovery is a person's: docs/runbooks/reconciliation.md.",
			e.EventTime.UTC().Format(time.RFC3339), p.Reason, p.Detail,
			strings.Join(p.ScopeInstrumentIDs, ","), strings.Join(p.ScopeOrderIDs, ","), strings.Join(p.ScopeCashComponents, ","))
	}
	return Alert{Key: engineStateKey(p) + ":" + e.EventTime.UTC().Format(time.RFC3339Nano) + "@" + account, Kind: kind, Severity: Urgent, AccountID: account, At: e.EventTime,
		Title: fmt.Sprintf("%s: engine %s (%s)", account, p.State, p.Reason), Body: body}
}

func withheldAlert(account string, e event.Envelope, p event.TargetsDecidedPayload, start time.Time) Alert {
	kind, severity := KindWithheld, Routine
	switch p.WithheldReason {
	case event.WithheldMissingBars, event.WithheldStaleAccountSnapshot, event.WithheldMissingUniverse:
		kind, severity = KindMissingData, Urgent
	}
	return Alert{
		Key:  fmt.Sprintf("%s:%s:%s", kind, p.WithheldReason, start.UTC().Format(time.RFC3339)) + "@" + account,
		Kind: kind, Severity: severity, AccountID: account, At: e.EventTime,
		Title: fmt.Sprintf("%s: decision withheld (%s)", account, p.WithheldReason),
		Body:  fmt.Sprintf("No targets were decided for the session ending %s: %s. %s. Held positions and orders are unchanged.", p.SessionPeriodEnd.UTC().Format(time.DateOnly), p.WithheldReason, p.Detail),
	}
}

// HeartbeatAlert reports whether the account's engine has been silent for
// sessions NYSE sessions: that many sessions have closed since lastActivity,
// when the journal last changed, up to now, the caller's clock. Silence is
// counted in sessions, not hours, because the engine writes once per session
// input (ADR 0027, section 3) and nothing over a weekend or a holiday; the
// count is the owner's operational setting (ADR 0019, "Alerting"). The key
// names the silence by its start, so one outage is one incident however long
// it lasts, and a later outage is another. A span the calendar does not cover
// is an error, never a guess.
func HeartbeatAlert(account string, lastActivity, now time.Time, sessions int) (Alert, bool, error) {
	closed, latest, err := nysecalendar.Closes(lastActivity, now)
	if err != nil {
		return Alert{}, false, fmt.Errorf("report: heartbeat for %s: %w", account, err)
	}
	if closed < sessions {
		return Alert{}, false, nil
	}
	return Alert{
		Key: "heartbeat-lost:" + account + ":" + lastActivity.UTC().Format(time.RFC3339), Kind: KindHeartbeat, Severity: Urgent,
		AccountID: account, At: now,
		Title: fmt.Sprintf("%s: no engine heartbeat", account),
		Body: fmt.Sprintf("The journal last changed at %s, and %d NYSE sessions have closed since, the latest at %s. The engine may have crashed or wedged; stops and orders at the broker are unaffected.",
			lastActivity.UTC().Format(time.RFC3339), closed, latest.UTC().Format(time.RFC3339)),
	}, true, nil
}

// JournalRefusedAlert reports that the journal could not be read, so no
// alert it holds can be sent: a damaged or mismatched log is refused rather
// than guessed at, and goes to a person (ADR 0027, section 5). heartbeatUnchecked
// is the outcome of the heartbeat check: empty when it ran, otherwise the reason
// it did not, and the body then says plainly that liveness is unmonitored. The
// key is the account and the error, so one failure is one incident and a
// different failure is another.
func JournalRefusedAlert(account string, err error, heartbeatUnchecked string, at time.Time) Alert {
	heartbeat := " The heartbeat is still checked."
	if heartbeatUnchecked != "" {
		heartbeat = " The heartbeat is NOT checked: " + heartbeatUnchecked + "."
	}
	return Alert{
		Key: "journal-refused:" + account + ":" + err.Error(), Kind: KindJournalRefused, Severity: Urgent, AccountID: account, At: at,
		Title: fmt.Sprintf("%s: the reporter cannot read the journal", account),
		Body:  fmt.Sprintf("The reporter refused the journal: %s. No alert recorded in it (refusals, Degraded or Halted, withheld decisions) can be sent until it reads, and the daily summary is not sent.%s Stops and orders at the broker are unaffected. Check the file with journal-query; recovery is a person's.", err, heartbeat),
	}
}

// HeartbeatUncheckedAlert reports that the heartbeat could not be checked, so
// the reporter cannot tell a live engine from a dead one: a check that fails
// quietly on a timer run is the silence the heartbeat exists to catch (ADR
// 0019, "Alerting"; the heartbeat is counted in sessions, ADR 0027, section 3).
// The key is the account and the reason, so one failure is one incident.
func HeartbeatUncheckedAlert(account string, err error, at time.Time) Alert {
	return Alert{
		Key: "heartbeat-unchecked:" + account + ":" + err.Error(), Kind: KindHeartbeatUnchecked, Severity: Urgent, AccountID: account, At: at,
		Title: fmt.Sprintf("%s: the reporter cannot check the heartbeat", account),
		Body:  fmt.Sprintf("The reporter cannot check the heartbeat: %s. Until it can, a silent engine goes unreported. Stops and orders at the broker are unaffected. Check the NYSE calendar's years and the journal's modification time.", err),
	}
}

// Fresh returns, in order, the alerts whose keys are not in seen and not
// earlier in the list. The caller adds a key to seen only after delivering it,
// so a failed delivery is retried on the next run.
func Fresh(alerts []Alert, seen map[string]bool) []Alert {
	var out []Alert
	local := map[string]bool{}
	for _, a := range alerts {
		if seen[a.Key] || local[a.Key] {
			continue
		}
		local[a.Key] = true
		out = append(out, a)
	}
	return out
}
