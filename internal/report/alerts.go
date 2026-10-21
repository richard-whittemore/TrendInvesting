package report

import (
	"fmt"
	"slices"
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
	// KindReconciliation is a reconciliation that did not match: the
	// account's book and the broker disagree, or the broker's evidence could
	// not be compared (ADR 0019).
	KindReconciliation Kind = "reconciliation"
	// KindUnacknowledged is an order to be sent that the venue has not
	// acknowledged within the configured time, and KindAcknowledged the
	// notice that one already alerted has since been acknowledged.
	KindUnacknowledged Kind = "order-unacknowledged"
	KindAcknowledged   Kind = "order-acknowledged"
	// KindVenueReportUnread is a venue's report the log holds that the
	// engine did not read: it does not validate, or does not describe the
	// order it names as released. It acknowledges nothing.
	KindVenueReportUnread Kind = "venue-report-unread"
	// KindMarginAlarm is a change of the joint account's margin-cushion
	// alarm level (plan.md section 5, "A margin-cushion alarm").
	KindMarginAlarm Kind = "margin-alarm"
	// KindSaleSent, KindSaleFilled and KindSaleFailed are the steps of a sale
	// back to 98% the owner is told of (plan.md, decision D6): released to be
	// sent, each execution, and the venue ending it unfilled.
	KindSaleSent   Kind = "sale-sent"
	KindSaleFilled Kind = "sale-filled"
	KindSaleFailed Kind = "sale-failed"
	// KindSaleByHand is a sale back to 98% the owner must make by hand:
	// withheld for a reason that spends the attempt, withheld for values
	// that are not positive, or refused at the gate as broker-holding (ADR
	// 0019, amended 2026-10-15, "No order when it cannot be checked";
	// docs/manual/states-and-kill-switch.md, "Selling by hand safely").
	KindSaleByHand Kind = "sale-by-hand"
	// KindChannelRefused is the reporter's own failure to use one delivery
	// channel, an empty or malformed secret: that channel is set aside and
	// the others still deliver.
	KindChannelRefused Kind = "channel-refused"
)

// Alert is one incident to tell the owner about. Key names the incident:
// two alerts with the same Key are the same incident and are delivered once,
// unless Repeat is set: then the alert is sent again at the owner's interval
// until a recorded operator.alert.acknowledged names its key, which sets
// Acknowledged (ADR 0019, amended 2026-09-24, "Alerting"). PublicBody, when
// set, is the text for a public relay: it names what is in doubt and carries
// no balance.
type Alert struct {
	Key          string
	Kind         Kind
	Severity     Severity
	AccountID    string
	At           time.Time
	Title        string
	Body         string
	PublicBody   string
	Repeat       bool
	Acknowledged bool
}

// Due reports whether a is to be sent to a channel now: never delivered to
// it, or delivered last at last and, repeating and unacknowledged, at least
// every since then.
func Due(a Alert, delivered bool, last, now time.Time, every time.Duration) bool {
	return !delivered || a.Repeat && !a.Acknowledged && now.Sub(last) >= every
}

// Alerts returns the journal's alerts in recording order, each incident once.
//
//   - Every risk.order.refused is its own incident, except that a kill-switch
//     engagement and a stale episode alert once: the gate flags the first
//     kill-switch or stale-broker-snapshot refusal of its engagement or
//     episode (ADR 0026, point 3, kill switch: "alert once"; ADR 0019,
//     amendment of 2026-10-17, reading 10), and an unflagged one sends
//     nothing once an earlier record of the same reason in the journal
//     carried the flag. Without that proof the journal may predate the flag,
//     so an unflagged refusal alerts as any other (ADR 0019, "Alerting": an
//     alert is a safety requirement).
//   - A risk.sale.withheld the gate flagged opens the account's stale
//     episode and is that episode's one urgent alert, as missing data
//     (ADR 0019, amendment of 2026-10-17, reading 10, "Stale episode"); it
//     proves the gate flags stale episodes, as a flagged stale refusal does.
//   - A sale back to 98% the owner must make by hand pages with the hand-sale
//     steps (docs/manual/states-and-kill-switch.md, "Selling by hand
//     safely"): withheld for a reason that spends the attempt, once per
//     record; withheld for values that are not positive, once per sale; and
//     refused at the gate as broker-holding, beside its refusal alert.
//   - A strategy.engine.state record is an incident when its state, reason and
//     scope differ from the previous one: a repeat of the same state is the
//     same incident, and Degraded to Halted is a new one (ADR 0019). The
//     incident is keyed by its start, so a state that returns after another
//     is a new incident too.
//   - A Boost decision recording a re-plan withheld after the venue rejected
//     the re-planned order again is one urgent refusal incident, keyed by
//     the order (ADR 0026, point 2, "Boost", amended 2026-10-18).
//   - A run of consecutive withheld decisions with one reason is one
//     incident; a decided outcome ends it. A stale-account-snapshot withhold
//     whose latest account.broker.snapshot before it is a version-2 reading
//     stamped at its read time names that snapshot and the reason it states:
//     why the IBKR adapter's reading after the close could not stand for the
//     close (the owner's decision of 2026-10-18; ADR 0019, amended). A broker
//     snapshot that does not read is an input passed over, as a venue's
//     report is. Reasons that name absent inputs
//     (missing bars, stale snapshot, missing universe, missing close
//     histories) and a refusal for a missing mark are missing data; every
//     other withheld reason is a withheld decision, routine except
//     all-unaffordable, which may rest on a bad valuation, and
//     labels-missing, which may rest on a broken sector-label source; both
//     are urgent.
//   - A risk.reconciliation.result that degrades or halts is one incident,
//     however many components it names; the engine-state record the same
//     reconciliation caused is that incident too, not another alert. A
//     result that continues sends nothing. The alert states the account's
//     state after the reconciliation, as the gate holds it: the record the
//     reconciliation caused, or, when it caused none (the state did not
//     move), the state already in force, from the latest engine-state record
//     or the incident a fresh run's approval carried in force.
//
// Engine-state and reconciliation alerts repeat until acknowledged; an
// operator.alert.acknowledged anywhere in the journal acknowledges the alert
// whose key it names, provided it is for this account: the journal records an
// acknowledgement for another account with no effect. The engine journals only
// acknowledgements it could read, so one this build cannot read is a build
// mismatch and a read error.
func Alerts(envelopes []event.Envelope) ([]Alert, error) {
	account, _, err := AccountOf(envelopes)
	if err != nil {
		return nil, err
	}
	var out []Alert
	var lastState, runReason string
	var runStart time.Time
	reconciled := map[string]bool{}
	acknowledged := map[string]bool{}
	caused, err := causedStates(envelopes)
	if err != nil {
		return nil, err
	}
	var inForce *gateState
	// flagged holds each refusal reason the gate is proven to flag: a record
	// of that reason earlier in the journal carried the flag.
	flagged := map[string]bool{}
	pending := sales{}
	// paged holds the by-hand pages already raised: a sale withheld again
	// for values that are not positive is the same incident.
	paged := map[string]bool{}
	// notAtClose is the latest broker snapshot stamped at its read time, if
	// the latest one is: its ID and why it was not as of the close.
	var notAtClose [2]string
	for _, e := range envelopes {
		switch e.Type {
		case event.BrokerSnapshotEventType:
			var p event.BrokerSnapshotPayload
			if read(e, brokerSnapshotVersions, &p) != nil {
				continue
			}
			notAtClose = [2]string{}
			if e.SchemaVersion == event.BrokerSnapshotReadAtVersion && !p.AsOfClose {
				notAtClose = [2]string{e.ID, p.NotAsOfClose}
			}
		case event.OperatorRunApprovedEventType:
			var p event.OperatorRunApprovedPayload
			if err := read(e, approvalVersions, &p); err != nil {
				return nil, err
			}
			// An approval with no incident in force starts the run clear.
			inForce = nil
			if in := p.IncidentInForce; in != nil {
				inForce = &gateState{in.State, in.ScopeInstrumentIDs, in.ScopeCashComponents}
			}
		case event.ReconciliationResultEventType:
			var p event.ReconciliationResultPayload
			if err := read(e, reconciliationVersions, &p); err != nil {
				return nil, err
			}
			if p.Action != event.ReconciliationActionContinue {
				reconciled[e.CausationID] = true
				after, ok := caused[e.CausationID]
				if !ok {
					after = inForce.with(p)
				}
				out = append(out, reconciliationAlert(account, e, p, after))
			}
		case event.AlertAcknowledgedEventType:
			// Only an acknowledgement for this account stops the repeats
			// (ADR 0019); an unreadable one is an error, never skipped.
			var p event.AlertAcknowledgedPayload
			if err := read(e, acknowledgedVersions, &p); err != nil {
				return nil, err
			}
			if p.AccountID == account {
				acknowledged[p.AlertKey] = true
			}
		case event.OrderRefusedEventType:
			var p event.OrderRefusedPayload
			if err := read(e, refusedVersions, &p); err != nil {
				return nil, err
			}
			out = append(out, refusalAlerts(account, e, p, flagged[p.Reason])...)
			if a, ok := pending.refused(account, e, p); ok {
				out = append(out, a)
			}
			if p.Alert {
				flagged[p.Reason] = true
			}
		case event.SaleWithheldEventType:
			var p event.SaleWithheldPayload
			if err := read(e, saleWithheldVersions, &p); err != nil {
				return nil, err
			}
			if p.Alert {
				flagged[event.RefusedStaleBrokerSnapshot] = true
				out = append(out, staleSaleAlert(account, e, p))
			}
			if a, ok := byHandAlert(account, e, p); ok && !paged[a.Key] {
				paged[a.Key] = true
				out = append(out, a)
			}
		case event.MarginAlarmEventType:
			var p event.MarginAlarmPayload
			if err := read(e, marginAlarmVersions, &p); err != nil {
				return nil, err
			}
			out = append(out, marginAlarmAlert(account, e, p))
		case event.OrderPlannedEventType:
			// A plan is a decision, and the one that names a sale back to
			// 98%: one that does not read would silence that sale's sent,
			// filled and failed alerts, so it is an error, never skipped,
			// and the reporter pages that it cannot read the journal (ADR
			// 0019, "Alerting").
			var p event.OrderPlannedPayload
			if err := read(e, plannedVersions, &p); err != nil {
				return nil, err
			}
			pending.plan(p)
		case event.OrderReleasedEventType:
			var p event.OrderReleasedPayload
			if err := read(e, releasedVersions, &p); err != nil {
				return nil, err
			}
			if a, ok := pending.release(account, e, p); ok {
				out = append(out, a)
			}
		case event.FillEventType:
			// A venue's report is an input the log keeps even when the
			// engine refused it, so one that does not read raises no sale
			// alert; the order's other alerts are unaffected.
			var p event.TargetWeightFillPayload
			if e.SchemaVersion != event.FillTargetWeightVersion || read(e, targetWeightFillVersions, &p) != nil {
				continue
			}
			if a, ok := pending.fill(account, e, p); ok {
				out = append(out, a)
			}
		case event.OrderLifecycleEventType:
			var p event.LimitOrderLifecyclePayload
			if e.SchemaVersion != event.OrderLifecycleLimitVersion || read(e, lifecycleVersions, &p) != nil {
				continue
			}
			if a, ok := pending.lifecycle(account, e, p); ok {
				out = append(out, a)
			}
		case event.EngineStateEventType:
			var p event.EngineStatePayload
			if err := read(e, engineStateVersions, &p); err != nil {
				return nil, err
			}
			inForce = &gateState{p.State, p.ScopeInstrumentIDs, p.ScopeCashComponents}
			key := engineStateKey(p)
			if key == lastState {
				continue
			}
			lastState = key
			if e.CausationID != "" && reconciled[e.CausationID] {
				continue
			}
			out = append(out, engineStateAlert(account, e, p))
		case event.TargetsDecidedEventType:
			var p event.TargetsDecidedPayload
			if err := read(e, decidedVersions, &p); err != nil {
				return nil, err
			}
			if w := p.Boost.ReplanWithheld; w.Cause != "" {
				out = append(out, rejectionAlert(account, e, w, p.Boost.RejectedInARow))
			}
			for i, r := range p.Boost.RejectedWhileStopped {
				nth := p.Boost.RejectionsWhileStopped - len(p.Boost.RejectedWhileStopped) + i + 1
				out = append(out, stoppedRejectionAlert(account, e, r, nth, p.Boost.RejectedInARow))
			}
			if p.Outcome != event.TargetsOutcomeWithheld {
				runReason = ""
				continue
			}
			if p.WithheldReason == runReason {
				continue
			}
			runReason, runStart = p.WithheldReason, e.EventTime
			a := withheldAlert(account, e, p, runStart)
			if p.WithheldReason == event.WithheldStaleAccountSnapshot && notAtClose[0] != "" {
				a.Body += fmt.Sprintf(" The adapter's reading after the close could not stand for the close (broker snapshot %s): %s. It was stamped at its read time, so the session was withheld and nothing was ordered: docs/runbooks/first-paper-order.md, \"A session withheld after the close\".", notAtClose[0], notAtClose[1])
			}
			out = append(out, a)
		}
	}
	for i := range out {
		out[i].Acknowledged = acknowledged[out[i].Key]
	}
	return out, nil
}

// acknowledgeLine is the line a repeating alert ends with: the key the owner's
// acknowledgement names, and that it repeats until then.
func acknowledgeLine(key string) string {
	return " This alert repeats until you acknowledge it; acknowledge with alert key: " + key
}

// gateState is the account's engine state as the gate holds it: Degraded or
// Halted, and a Degraded state's whole scope.
type gateState struct {
	state       string
	instruments []string
	cash        []string
}

// with is the gate's state after result p when p moved nothing (ADR 0019,
// amended 2026-09-24: leaving a state is never automatic, a Degraded scope
// widens to the union and Halted overrides): g as it was, widened by p's
// scope; p's own view when no state was in force.
func (g *gateState) with(p event.ReconciliationResultPayload) gateState {
	switch {
	case p.Action == event.ReconciliationActionHalt || g != nil && g.state == event.EngineStateHalted:
		return gateState{state: event.EngineStateHalted}
	case g == nil:
		return gateState{event.EngineStateDegraded, p.ScopeInstrumentIDs, p.ScopeCashComponents}
	}
	return gateState{event.EngineStateDegraded, sortedUnion(g.instruments, p.ScopeInstrumentIDs), sortedUnion(g.cash, p.ScopeCashComponents)}
}

func sortedUnion(a, b []string) []string {
	out := slicesConcat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}

// causedStates are the journal's engine-state records that an input caused,
// by that input's ID: the state a reconciliation moved the account to.
func causedStates(envelopes []event.Envelope) (map[string]gateState, error) {
	out := map[string]gateState{}
	for _, e := range envelopes {
		if e.Type != event.EngineStateEventType || e.CausationID == "" {
			continue
		}
		var p event.EngineStatePayload
		if err := read(e, engineStateVersions, &p); err != nil {
			return nil, err
		}
		out[e.CausationID] = gateState{p.State, p.ScopeInstrumentIDs, p.ScopeCashComponents}
	}
	return out, nil
}

// reconciliationAlert states one reconciliation's differences, the state the
// account is in after it, as the gate holds it, and what that state still
// does (ADR 0019's "Every alert states" list). Its public text names the
// instruments and cash components in doubt and no figure; evidence that could
// not be compared is described there only as that, since its reasons can
// carry cents, dollars and share counts, which go to the private body alone
// (owner decision 2026-10-17, docs/live-trading/plan.md section 7).
func reconciliationAlert(account string, e event.Envelope, p event.ReconciliationResultPayload, after gateState) Alert {
	key := "reconciliation:" + e.ID + "@" + account
	when := e.EventTime.UTC().Format(time.RFC3339)
	effect := fmt.Sprintf("The account is Degraded on instruments [%s] and cash [%s]: it opens no new risk and refuses buys; the named instruments are frozen, except, for the joint account, the one sale back toward 98%% (ADR 0019 amendment, 2026-10-16); sells elsewhere continue.",
		strings.Join(after.instruments, ","), strings.Join(after.cash, ","))
	doubt := fmt.Sprintf("an unexplained difference in [%s]", strings.Join(slicesConcat(p.ScopeInstrumentIDs, p.ScopeCashComponents), ","))
	publicDoubt := doubt
	if after.state == event.EngineStateHalted {
		effect = "The account is Halted: no order is released, except, for the joint account, the one sale back toward 98% (ADR 0019 amendment, 2026-10-15)."
	}
	if p.Action == event.ReconciliationActionHalt {
		doubt = "evidence that could not be compared: " + strings.Join(p.Unverifiable, "; ")
		publicDoubt = "that the reconciliation could not be verified; see the private channel for what could not be compared"
	}
	recovery := " Leaving the state is never automatic. Recovery is a person's: docs/runbooks/reconciliation.md." + acknowledgeLine(key)
	book := "with no book recorded"
	if p.AnchorID != "" {
		book = "against the book " + p.AnchorID
	}
	return Alert{
		Key: key, Kind: KindReconciliation, Severity: Urgent, AccountID: account, At: e.EventTime, Repeat: true,
		Title: fmt.Sprintf("%s: reconciliation %s (%s)", account, p.Classification, p.ReconciliationID),
		Body: fmt.Sprintf("Reconciliation %s at %s %s found %s. %s. %s Check the named positions and cash at the broker.%s",
			p.ReconciliationID, when, book, doubt, p.Detail, effect, recovery),
		PublicBody: fmt.Sprintf("Reconciliation %s at %s found %s. %s Figures are in the journal (journal-query) and the dashboard.%s",
			p.ReconciliationID, when, publicDoubt, effect, recovery),
	}
}

func slicesConcat(a, b []string) []string { return append(append([]string{}, a...), b...) }

// refusalAlerts states one refusal. flags reports that the journal has
// proven the gate flags refusals of this reason; only then is an unflagged
// kill-switch or stale-broker-snapshot refusal a repeat that sends nothing.
func refusalAlerts(account string, e event.Envelope, p event.OrderRefusedPayload, flags bool) []Alert {
	if (p.Reason == event.RefusedKillSwitch || p.Reason == event.RefusedStaleBrokerSnapshot) && !p.Alert && flags {
		return nil
	}
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

// staleSaleAlert states the withheld sale that opened the account's stale
// episode: the sale was not placed, so the owner may need to act by hand.
func staleSaleAlert(account string, e event.Envelope, p event.SaleWithheldPayload) Alert {
	return Alert{
		Key: "missing-data:sale-withheld:" + e.ID, Kind: KindMissingData, Severity: Urgent, AccountID: account, At: e.EventTime,
		Title: fmt.Sprintf("%s: sale of %s withheld (%s)", account, p.InstrumentID, p.Reason),
		Body: fmt.Sprintf("The %s sale back to 98%% of %s (%s, order %s) was not placed: %s. %s. Later stale refusals of this episode send nothing more; check the IBKR snapshot and sell by hand if it is needed.",
			p.Kind, p.InstrumentID, p.Trigger, p.ClientOrderID, p.Reason, p.Detail),
	}
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
	key := engineStateKey(p) + ":" + e.EventTime.UTC().Format(time.RFC3339Nano) + "@" + account
	return Alert{Key: key, Kind: kind, Severity: Urgent, AccountID: account, At: e.EventTime, Repeat: true,
		Title: fmt.Sprintf("%s: engine %s (%s)", account, p.State, p.Reason), Body: body + acknowledgeLine(key)}
}

// handSaleSteps is the hand-sale procedure for the account while a program sale may be
// working (docs/manual/states-and-kill-switch.md, "Selling by hand safely"):
// the program plans Boost's exit again at each decided session while it is
// due, a hand sale placed after the last IBKR snapshot is not counted until a
// snapshot lists it, and the kill switch does not stop the sale back to 98%,
// so the owner stops the adapter, then the engine, before selling by hand (one command per unit: systemd does not order several in one). The
// fresh run's approval states the book after the sale: no fill of a hand
// sale is committed, so the previous close adjusted by the fills is not the
// book, and a first reading that is not the book halts the account as
// unverifiable (ADR 0019, amended 2026-10-18).
//
// The units are named by the engine instance, not by the account it
// decides, which the journal records: the steps come only from Boost's
// rejections and the sale back to 98%, which only the margin account makes
// (ADR 0019, amended 2026-10-15), and Boost runs as ti-engine@boost with ti-adapter-boost, the
// only adapter unit, wired to that engine's socket and log, whatever account
// its configuration decides (the shipped one decides "joint";
// deploy/systemd, docs/manual/server.md). The approval names the account.
func handSaleSteps(account string) string {
	return fmt.Sprintf("Before you sell anything by hand, stop the engine and the adapter, the adapter first: sudo systemctl stop ti-adapter-boost, then sudo systemctl stop ti-engine@boost (systemd does not order two units named in one command); if the first fails, stop there and read journalctl -u ti-adapter-boost before going on. The kill switch is not enough, because it does not stop the sale back to 98%%. Then cancel every open order on the instrument, the program's and any of your own, in IBKR's app, and confirm there that none is open, cancel-pending included; only then sell. Start the fresh run you approve only once your sale has filled or been cancelled; its approval states the book as it is after your sale (operator approve -log <previous run's log> -account %[1]s -by <you> -positions SYM=shares,... -cash <IBKR's TotalCashValue> -currency USD -out <approval>), typed right after reading the book in IBKR, or the fresh run's first reading is not the book it is compared with and the account halts as unverifiable. Then start the adapter only after the fresh run's engine is running; never resume the stopped run: a resumed engine hands the adapter an order it had released but never sent, and the adapter sends it beside your sale.", account)
}

// rejectionAlert states the rejection that brought the account's Boost
// orders rejected in a row to two, whatever their targets, after the other
// rejection the decision names (ADR 0026, point 2, "Boost", amended
// 2026-10-18): the program sends no more buys for
// the account until the owner finds the reason and approves a fresh run,
// while its sells, Boost's exit and the margin alarm's sale back to 98%,
// still go (ADR 0026, point 4), so a sale by hand follows the hand-sale
// procedure.
func rejectionAlert(account string, e event.Envelope, w event.BoostReplan, inARow []event.BoostRejectedOrder) Alert {
	after := ""
	for _, o := range inARow {
		if o.ClientOrderID != w.ClientOrderID {
			after = " after order " + orderName(o)
		}
	}
	return Alert{
		Key: "replan-withheld:" + w.ClientOrderID + "@" + account, Kind: KindRefusal, Severity: Urgent, AccountID: account, At: e.EventTime,
		Title: fmt.Sprintf("%s: order %s rejected, two orders in a row; Boost buys stopped", account, w.ClientOrderID),
		Body: fmt.Sprintf("IBKR rejected order %s (%d of %d shares filled)%s, and has now rejected two orders in a row for this account. The program sends no more buys for this account, whatever the target does, until you start a fresh run. Its sells still go: Boost's exit, tried again at each session while it is due, and the margin alarm's sale back to 98%%. %s Find why IBKR rejects the orders in IBKR's order log (journal-query orders lists the rejections), fix it, then approve a fresh run (operator approve, adding -clear-incident if the run also ended in an incident).",
			w.ClientOrderID, w.Filled, w.Released, after, handSaleSteps(account)),
	}
}

// stoppedRejectionAlert states an order the venue rejected after the stop,
// the nth since it: a sell sent while buys are stopped, which is planned
// again at each decided session while it is due, or an order released
// before the stop; it names the two rejections that stopped buys (ADR 0026,
// points 2, "Boost", amended 2026-10-18, and 4).
func stoppedRejectionAlert(account string, e event.Envelope, r event.BoostReplan, nth int, inARow []event.BoostRejectedOrder) Alert {
	stoppedBy := ""
	if len(inARow) > 0 {
		names := make([]string, len(inARow))
		for i, o := range inARow {
			names[i] = orderName(o)
		}
		stoppedBy = " Buys were stopped by the rejections of orders " + strings.Join(names, " and ") + "."
	}
	return Alert{
		Key: "rejected-while-stopped:" + r.ClientOrderID + "@" + account, Kind: KindRefusal, Severity: Urgent, AccountID: account, At: e.EventTime,
		Title: fmt.Sprintf("%s: order %s rejected: another rejection while Boost's buys are stopped (%s since the stop)", account, r.ClientOrderID, ordinal(nth)),
		Body: fmt.Sprintf("IBKR rejected order %s (%d of %d shares filled): another rejection while Boost's buys are stopped, the %s since the stop.%s A rejected sell, such as Boost's exit, is planned again at the next decided session while it is due; the program still sends no buy for this account until you start a fresh run. %s Find why IBKR rejects the orders in IBKR's order log (journal-query orders lists the rejections), fix it, then approve a fresh run (operator approve, adding -clear-incident if the run also ended in an incident).",
			r.ClientOrderID, r.Filled, r.Released, ordinal(nth), stoppedBy, handSaleSteps(account)),
	}
}

// orderName writes a Boost order rejected in a row by its client order ID
// and, when known, IBKR's own ID for it, by which IBKR's order log lists it.
func orderName(o event.BoostRejectedOrder) string {
	if o.VenueOrderID == "" {
		return o.ClientOrderID
	}
	return fmt.Sprintf("%s (IBKR order %s)", o.ClientOrderID, o.VenueOrderID)
}

// ordinal writes n as 1st, 2nd, 3rd, 4th, ..., 11th, 12th, 13th, 21st.
func ordinal(n int) string {
	suffix := "th"
	switch n % 10 {
	case 1:
		suffix = "st"
	case 2:
		suffix = "nd"
	case 3:
		suffix = "rd"
	}
	if n%100 >= 11 && n%100 <= 13 {
		suffix = "th"
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

func withheldAlert(account string, e event.Envelope, p event.TargetsDecidedPayload, start time.Time) Alert {
	kind, severity := KindWithheld, Routine
	switch p.WithheldReason {
	case event.WithheldMissingBars, event.WithheldStaleAccountSnapshot, event.WithheldMissingUniverse, event.WithheldHistoriesMissing:
		kind, severity = KindMissingData, Urgent
	case event.WithheldAllUnaffordable:
		// The valuation itself may be wrong (a bad net liquidation or bad
		// raw closes), which a person checks now (ADR 0026, point 2, "D7-L").
		severity = Urgent
	case event.WithheldLabelsMissing:
		// A sector-label source or its mapping may have failed, which a
		// person fixes before the next decision (ADR 0026, points 5 and 8).
		severity = Urgent
	}
	body := fmt.Sprintf("No targets were decided for the session ending %s: %s. %s. Held positions and orders are unchanged.", p.SessionPeriodEnd.UTC().Format(time.DateOnly), p.WithheldReason, p.Detail)
	if p.StrategyID == event.TargetWeightStrategyBoost && p.WithheldReason == event.WithheldMissingBars {
		// A Boost gap is skipped, not a stop: the run decides again at the
		// next bar (ADR 0026, point 5, amended 2026-10-18), so the one page
		// for the gap says so.
		body = fmt.Sprintf("%s had no bar for the session ending %s, the first of this gap. Boost skipped that session as a data failure and traded nothing; it decides again at the next session with a bar, computing RSI(2) and both means over the closes it has (the missing close is not filled in). Held positions and orders are unchanged. Check the daily data job.",
			p.Boost.InstrumentID, p.SessionPeriodEnd.UTC().Format(time.DateOnly))
	}
	return Alert{
		Key:  fmt.Sprintf("%s:%s:%s", kind, p.WithheldReason, start.UTC().Format(time.RFC3339)) + "@" + account,
		Kind: kind, Severity: severity, AccountID: account, At: e.EventTime,
		Title: fmt.Sprintf("%s: decision withheld (%s)", account, p.WithheldReason),
		Body:  body,
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

// ChannelRefusedAlert reports that the reporter set a delivery channel aside,
// for an empty or malformed secret or setting, and sends this run's alerts on
// the other channels: one channel's configuration must never silence the
// rest (ADR 0019, "Alerting": an alert is a safety requirement). err never
// carries a secret's value. The key is the account, the channel and the
// problem, so one problem is one incident and a different one is another.
func ChannelRefusedAlert(account, channel string, err error, at time.Time) Alert {
	return Alert{
		Key: "channel-refused:" + account + ":" + channel + ":" + err.Error(), Kind: KindChannelRefused, Severity: Urgent, AccountID: account, At: at,
		Title: fmt.Sprintf("%s: the reporter cannot deliver to %s", account, channel),
		Body:  fmt.Sprintf("The reporter set the %s channel aside: %s. Every alert still goes to the other channels, but none reaches %s until it is fixed. Check the secrets file (/etc/trend-investing/secrets.env) and journalctl -u ti-reporter@<instance>.", channel, err, channel),
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
