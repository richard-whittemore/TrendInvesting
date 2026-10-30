package report_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// logRun builds a synthetic durable log's records in recording order: each
// input followed by the decisions it caused.
type logRun struct {
	t   *testing.T
	seq uint64
	out []journal.Entry
}

func (r *logRun) add(kind, eventType string, version uint32, id string, at time.Time, payload any) {
	r.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		r.t.Fatal(err)
	}
	r.seq++
	r.out = append(r.out, journal.Entry{Kind: kind, Envelope: event.Envelope{
		ID: id, Type: eventType, EnvelopeVersion: event.CurrentEnvelopeVersion, SchemaVersion: version,
		EventTime: at, RecordedAt: at, Sequence: r.seq, Source: "test", StrategyVersion: "v", ConfigurationHash: "h",
		PayloadHash: event.HashPayload(raw), Payload: raw,
	}})
}

func (r *logRun) session(n int) {
	r.add(journal.KindInput, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "session:"+day(n).Format(time.DateOnly), day(n),
		event.SessionClosedPayload{PeriodEnd: day(n), InstrumentIDs: []string{"SPY"}})
}

func releasedOrder(clientID, mode string) event.OrderReleasedPayload {
	return event.OrderReleasedPayload{AccountID: "joint", PlanID: "plan:" + clientID, Tag: "targets", ClientOrderID: clientID,
		InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 9, LimitPrice: 101, Mode: mode, Send: mode != event.TradingModeDryRun, Figures: figures}
}

func (r *logRun) release(n int, o event.OrderReleasedPayload) {
	r.add(journal.KindDecision, event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, "release:"+o.ClientOrderID, day(n), o)
}

func (r *logRun) acknowledge(o event.OrderReleasedPayload, at time.Time) {
	r.add(journal.KindInput, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "ack:"+o.ClientOrderID, at,
		event.LimitOrderLifecyclePayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, OrderID: "7", InstrumentID: o.InstrumentID,
			Side: o.Side, Status: event.OrderLifecycleStatusSubmitted, Quantity: o.Quantity, LimitPrice: o.LimitPrice, OccurredAt: at})
}

func stateOf(t *testing.T, orders []report.Order, clientID string) report.Order {
	t.Helper()
	for _, o := range orders {
		if o.ClientOrderID == clientID {
			return o
		}
	}
	t.Fatalf("no order %s in %+v", clientID, orders)
	return report.Order{}
}

// TestAReleasedOrderIsCommittedThenReleasedThenAcknowledged: an order's
// lifecycle state is read from the log alone. Its release, durable in a
// committed group, makes it committed (ADR 0027, section 4: possibly
// sent). The adapter sends its next input only once it holds the reply to
// the last, so a later input makes it released: the adapter has it. The
// venue's report about it (ADR 0022) makes it acknowledged.
func TestAReleasedOrderIsCommittedThenReleasedThenAcknowledged(t *testing.T) {
	run := &logRun{t: t}
	order := releasedOrder("tw-1", event.TradingModePaper)
	run.session(0)
	run.release(0, order)

	orders, _, err := report.OrderLifecycles(run.out, event.TargetWeightStrategyBoost)
	if err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, orders, "tw-1"); got.State != report.LifecycleCommitted || !got.CommittedAt.Equal(day(0)) || !got.Send {
		t.Fatalf("after its release: %+v, want committed at %s, to be sent", got, day(0))
	}

	run.session(1)
	orders, _, _ = report.OrderLifecycles(run.out, event.TargetWeightStrategyBoost)
	if got := stateOf(t, orders, "tw-1").State; got != report.LifecycleReleased {
		t.Fatalf("after a later input: %s, want %s", got, report.LifecycleReleased)
	}

	acked := day(1).Add(time.Minute)
	run.acknowledge(order, acked)
	orders, _, _ = report.OrderLifecycles(run.out, event.TargetWeightStrategyBoost)
	if got := stateOf(t, orders, "tw-1"); got.State != report.LifecycleAcknowledged || !got.AcknowledgedAt.Equal(acked) {
		t.Fatalf("after the venue's report: %+v, want acknowledged at %s", got, acked)
	}
}

// TestAnOrderNotAcknowledgedInTimeRaisesOneAlertThatItsAcknowledgementClears:
// an order to be sent that the venue has not acknowledged within the
// configured time is an urgent incident, one per order however often the
// reporter runs. Its acknowledgement clears it: the alert is no longer
// raised, and an owner already told is told it cleared. A dry-run release is
// never sent, so it never alerts (ADR 0026, point 3, "Mode"; ADR 0019,
// "Alerting").
func TestAnOrderNotAcknowledgedInTimeRaisesOneAlertThatItsAcknowledgementClears(t *testing.T) {
	const within = 10 * time.Minute
	run := &logRun{t: t}
	paper, dry := releasedOrder("tw-paper", event.TradingModePaper), releasedOrder("tw-dry", event.TradingModeDryRun)
	run.session(0)
	run.release(0, paper)
	run.release(0, dry)
	run.session(1)
	orders, _, err := report.OrderLifecycles(run.out, event.TargetWeightStrategyBoost)
	if err != nil {
		t.Fatal(err)
	}
	none := func(string) bool { return false }

	if got := report.UnacknowledgedAlerts(orders, day(0).Add(within-time.Second), within, none); len(got) != 0 {
		t.Fatalf("within the bound: %+v, want no alert", got)
	}
	first := report.UnacknowledgedAlerts(orders, day(0).Add(within), within, none)
	later := report.UnacknowledgedAlerts(orders, day(0).Add(3*within), within, none)
	if len(first) != 1 || first[0].Kind != report.KindUnacknowledged || first[0].Severity != report.Urgent || first[0].AccountID != "joint" {
		t.Fatalf("at the bound: %+v, want one urgent unacknowledged-order alert", first)
	}
	if len(later) != 1 || later[0].Key != first[0].Key {
		t.Fatalf("a later run: %+v, want the same incident, keyed %q", later, first[0].Key)
	}

	run.acknowledge(paper, day(1).Add(time.Minute))
	orders, _, _ = report.OrderLifecycles(run.out, event.TargetWeightStrategyBoost)
	if got := report.UnacknowledgedAlerts(orders, day(2), within, none); len(got) != 0 {
		t.Fatalf("acknowledged, never alerted: %+v, want nothing", got)
	}
	delivered := func(key string) bool { return key == first[0].Key }
	cleared := report.UnacknowledgedAlerts(orders, day(2), within, delivered)
	if len(cleared) != 1 || cleared[0].Kind != report.KindAcknowledged || cleared[0].Severity != report.Routine || cleared[0].Key == first[0].Key {
		t.Fatalf("acknowledged after its alert was delivered: %+v, want one routine notice that it cleared", cleared)
	}
}

// TestAFillAcknowledgesAnOrder: an execution is the venue's report on an
// order too, so in a momentum run, which reads fills, a fill naming its
// client order ID acknowledges it, and a report naming no released order
// changes nothing.
func TestAFillAcknowledgesAnOrder(t *testing.T) {
	run := &logRun{t: t}
	order := releasedOrder("tw-1", event.TradingModePaper)
	run.session(0)
	run.release(0, order)
	filled := day(0).Add(time.Hour)
	run.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:unknown", filled,
		event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "tw-other", FillID: "f0", InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 9, Price: 100, FilledAt: filled})
	run.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:1", filled,
		event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "tw-1", FillID: "f1", InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 9, Price: 100, FilledAt: filled})
	orders, unread, err := report.OrderLifecycles(run.out, event.TargetWeightStrategyMomentum)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 || len(unread) != 0 {
		t.Fatalf("orders %+v, unread %+v, want the one released and nothing unread", orders, unread)
	}
	if got := orders[0]; got.State != report.LifecycleAcknowledged || got.Acknowledgement != "fill" || !got.AcknowledgedAt.Equal(filled) {
		t.Fatalf("after its fill: %+v, want acknowledged by the fill at %s", got, filled)
	}
}

// TestOnlyAReportTheEngineReadsAcknowledgesAnOrder: the log holds an input
// the engine refused as well as one it read, and carries no mark of which,
// so the reporter reads a venue's report as the engine does. A lifecycle
// report acknowledges an order only when it describes the order as
// released, its limit within half a cent (event.LimitOrderLifecyclePayload's
// Describes); one that does not is unread. A Boost run reads a fill by the
// risk gate's check, so a fill the gate refuses acknowledges nothing there
// (ADR 0022; ADR 0019, "Alerting").
func TestOnlyAReportTheEngineReadsAcknowledgesAnOrder(t *testing.T) {
	order := releasedOrder("tw-1", event.TradingModePaper)
	at := day(0).Add(time.Minute)
	lifecycle := func(mutate func(*event.LimitOrderLifecyclePayload)) event.LimitOrderLifecyclePayload {
		p := event.LimitOrderLifecyclePayload{AccountID: order.AccountID, ClientOrderID: order.ClientOrderID, OrderID: "7", InstrumentID: order.InstrumentID,
			Side: order.Side, Status: event.OrderLifecycleStatusSubmitted, Quantity: order.Quantity, LimitPrice: order.LimitPrice, OccurredAt: at}
		mutate(&p)
		return p
	}
	stateAfter := func(strategy string, add func(*logRun)) (report.Order, []report.UnreadReport) {
		t.Helper()
		run := &logRun{t: t}
		run.session(0)
		run.release(0, order)
		add(run)
		orders, unread, err := report.OrderLifecycles(run.out, strategy)
		if err != nil {
			t.Fatal(err)
		}
		return stateOf(t, orders, "tw-1"), unread
	}

	got, unread := stateAfter(event.TargetWeightStrategyBoost, func(r *logRun) {
		r.add(journal.KindInput, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "ack:other", at,
			lifecycle(func(p *event.LimitOrderLifecyclePayload) { p.Quantity++ }))
	})
	if got.State == report.LifecycleAcknowledged {
		t.Errorf("a report with other figures, which the engine refuses, acknowledged the order: %+v", got)
	}
	if len(unread) != 1 || unread[0].InputID != "ack:other" || unread[0].ClientOrderID != "tw-1" || !strings.Contains(unread[0].Reason, "as released") {
		t.Errorf("unread %+v, want the refused report, saying it does not describe the order as released", unread)
	}

	got, unread = stateAfter(event.TargetWeightStrategyBoost, func(r *logRun) {
		r.add(journal.KindInput, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "ack:unknown", at,
			lifecycle(func(p *event.LimitOrderLifecyclePayload) { p.ClientOrderID = "tw-other" }))
	})
	if got.State == report.LifecycleAcknowledged || len(unread) != 0 {
		t.Errorf("a report naming no released order: %+v, unread %+v; want nothing changed, which is reconciliation's", got, unread)
	}

	got, unread = stateAfter(event.TargetWeightStrategyBoost, func(r *logRun) {
		r.add(journal.KindInput, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "ack:echo", at,
			lifecycle(func(p *event.LimitOrderLifecyclePayload) { p.LimitPrice = order.LimitPrice + 1e-9 }))
	})
	if got.State != report.LifecycleAcknowledged || len(unread) != 0 {
		t.Errorf("a report whose limit differs by a rounding error: %+v, unread %+v; want acknowledged", got, unread)
	}

	got, unread = stateAfter(event.TargetWeightStrategyBoost, func(r *logRun) {
		r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:big", at,
			event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "tw-1", FillID: "f1", InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 10, Price: 100, FilledAt: at})
	})
	if got.State == report.LifecycleAcknowledged || len(unread) != 0 {
		t.Errorf("a fill larger than the order in a Boost run, which the gate refuses: %+v, unread %+v; want nothing acknowledged", got, unread)
	}

	got, _ = stateAfter(event.TargetWeightStrategyMomentum, func(r *logRun) {
		r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:big", at,
			event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "tw-1", FillID: "f1", InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 10, Price: 100, FilledAt: at})
	})
	if got.State == report.LifecycleAcknowledged {
		t.Errorf("a fill larger than the order, which the engine refuses, acknowledged it: %+v", got)
	}
}

// TestEveryReportTheEngineRefusesIsUnread: a venue report the engine would
// refuse surfaces for its own alert, read by the rule the momentum run
// applies (event.OrderReports): a lifecycle report at a schema version
// neither target-weight run reads, a fill that takes the order past its
// quantity in total, a fill id reused for another execution, and any report
// on an order already terminal. The same execution delivered again is read
// and changes nothing.
func TestEveryReportTheEngineRefusesIsUnread(t *testing.T) {
	order := releasedOrder("tw-1", event.TradingModePaper)
	at := day(0).Add(time.Minute)
	fill := func(id string, quantity int64) event.TargetWeightFillPayload {
		return event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "tw-1", FillID: id, InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: quantity, Price: 100, FilledAt: at}
	}
	lifecycle := event.LimitOrderLifecyclePayload{AccountID: order.AccountID, ClientOrderID: order.ClientOrderID, OrderID: "7", InstrumentID: order.InstrumentID,
		Side: order.Side, Status: event.OrderLifecycleStatusCanceled, Quantity: order.Quantity, LimitPrice: order.LimitPrice, OccurredAt: at}
	unreadAfter := func(strategy string, add func(*logRun)) []string {
		t.Helper()
		run := &logRun{t: t}
		run.session(0)
		run.release(0, order)
		add(run)
		_, unread, err := report.OrderLifecycles(run.out, strategy)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, u := range unread {
			ids = append(ids, u.InputID)
		}
		return ids
	}
	input := func(r *logRun, eventType string, version uint32, id string, p any) {
		r.add(journal.KindInput, eventType, version, id, at, p)
	}
	for _, strategy := range []string{event.TargetWeightStrategyBoost, event.TargetWeightStrategyMomentum} {
		if got := unreadAfter(strategy, func(r *logRun) {
			input(r, event.OrderLifecycleEventType, event.OrderLifecycleSchemaVersion, "ack:v1", lifecycle)
		}); !slices.Equal(got, []string{"ack:v1"}) {
			t.Errorf("%s: a lifecycle report at a version neither run reads: unread %v, want it", strategy, got)
		}
	}
	for name, c := range map[string]struct {
		add  func(*logRun)
		want []string
	}{
		"fills past the order's quantity in total": {func(r *logRun) {
			input(r, event.FillEventType, event.FillTargetWeightVersion, "fill:1", fill("f1", 5))
			input(r, event.FillEventType, event.FillTargetWeightVersion, "fill:2", fill("f2", 5))
		}, []string{"fill:2"}},
		"a fill id reused for another execution": {func(r *logRun) {
			input(r, event.FillEventType, event.FillTargetWeightVersion, "fill:1", fill("f1", 5))
			input(r, event.FillEventType, event.FillTargetWeightVersion, "fill:again", fill("f1", 5))
			input(r, event.FillEventType, event.FillTargetWeightVersion, "fill:reused", fill("f1", 1))
		}, []string{"fill:reused"}},
		"a report on an order already terminal": {func(r *logRun) {
			input(r, event.FillEventType, event.FillTargetWeightVersion, "fill:all", fill("f1", 9))
			input(r, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "ack:late", lifecycle)
			input(r, event.FillEventType, event.FillTargetWeightVersion, "fill:late", fill("f2", 1))
		}, []string{"ack:late", "fill:late"}},
	} {
		if got := unreadAfter(event.TargetWeightStrategyMomentum, c.add); !slices.Equal(got, c.want) {
			t.Errorf("momentum, %s: unread %v, want %v", name, got, c.want)
		}
	}
	// A Boost run tracks no terminal state: it reads every lifecycle report
	// that describes the order, however many.
	if got := unreadAfter(event.TargetWeightStrategyBoost, func(r *logRun) {
		input(r, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "ack:1", lifecycle)
		input(r, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "ack:2", lifecycle)
	}); len(got) != 0 {
		t.Errorf("boost, two lifecycle reports on a cancelled order: unread %v, want none", got)
	}
}

// TestAnUnreadVenueReportRaisesItsOwnAlertOnce: a venue's report the engine
// did not read is one urgent incident per input, however often the reporter
// runs (ADR 0019, "Alerting").
func TestAnUnreadVenueReportRaisesItsOwnAlertOnce(t *testing.T) {
	unread := []report.UnreadReport{{InputID: "ack:1", Type: event.OrderLifecycleEventType, ClientOrderID: "tw-1", Reason: "it does not validate", At: day(0)}}
	alerts := report.UnreadReportAlerts("joint", unread)
	if len(alerts) != 1 || alerts[0].Kind != report.KindVenueReportUnread || alerts[0].Severity != report.Urgent || alerts[0].AccountID != "joint" {
		t.Fatalf("alerts %+v, want one urgent unread-report alert for joint", alerts)
	}
	if !strings.Contains(alerts[0].Body, "ack:1") || !strings.Contains(alerts[0].Body, "tw-1") || !strings.Contains(alerts[0].Body, "it does not validate") {
		t.Errorf("body %q, want the input, the order and the reason", alerts[0].Body)
	}
	if again := report.UnreadReportAlerts("joint", unread); again[0].Key != alerts[0].Key {
		t.Errorf("keys %q and %q, want one incident", alerts[0].Key, again[0].Key)
	}
}

// TestOrderLifecyclesReadsOnlyWhatItCanRead: a release at a version this
// build does not read, or that does not validate, is refused, never guessed
// at (ADR 0015). A venue's report that does not validate was refused by the
// engine and committed all the same, and the log is append-only, so it is
// unread rather than refusing every later reading of the log: it
// acknowledges nothing and is returned for its own alert. Other decisions
// change nothing, and an order released again under its client order ID
// keeps its first release.
func TestOrderLifecyclesReadsOnlyWhatItCanRead(t *testing.T) {
	order := releasedOrder("tw-1", event.TradingModePaper)
	run := &logRun{t: t}
	run.session(0)
	run.add(journal.KindDecision, event.OrderReleasedEventType, event.OrderReleasedSafetyVersion+1, "release", day(0), order)
	if _, _, err := report.OrderLifecycles(run.out, event.TargetWeightStrategyBoost); err == nil {
		t.Error("a release at another version: read without error")
	}
	for name, add := range map[string]func(*logRun){
		"a lifecycle report that does not validate": func(r *logRun) {
			r.add(journal.KindInput, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "unread", day(0), event.LimitOrderLifecyclePayload{ClientOrderID: "tw-1"})
		},
		"a fill that does not validate": func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "unread", day(0), event.TargetWeightFillPayload{ClientOrderID: "tw-1"})
		},
	} {
		run := &logRun{t: t}
		run.session(0)
		run.release(0, order)
		add(run)
		orders, unread, err := report.OrderLifecycles(run.out, event.TargetWeightStrategyMomentum)
		if err != nil {
			t.Errorf("%s: %v, want it unread, not a refused log", name, err)
			continue
		}
		if got := stateOf(t, orders, "tw-1"); got.State == report.LifecycleAcknowledged {
			t.Errorf("%s acknowledged the order: %+v", name, got)
		}
		if len(unread) != 1 || unread[0].InputID != "unread" || unread[0].ClientOrderID != "tw-1" {
			t.Errorf("%s: unread %+v, want it", name, unread)
		}
	}

	run = &logRun{t: t}
	run.session(0)
	run.add(journal.KindDecision, event.OrderPlannedEventType, event.OrderPlannedSchemaVersion, "plan", day(0), struct{}{})
	run.release(0, order)
	run.session(1)
	again := order
	again.Quantity = 5
	run.add(journal.KindDecision, event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, "release-again", day(1), again)
	orders, _, err := report.OrderLifecycles(run.out, event.TargetWeightStrategyBoost)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 || orders[0].Quantity != 9 || orders[0].State != report.LifecycleReleased {
		t.Fatalf("orders %+v, want the first release only, released", orders)
	}
}

// TestAnOrderStillCommittedSaysTheAdapterMayNotHaveIt: an alert for an order
// no later input follows says the adapter may never have received it and
// will ask for its reply again (ADR 0027, amendment "re-delivery on
// request").
func TestAnOrderStillCommittedSaysTheAdapterMayNotHaveIt(t *testing.T) {
	run := &logRun{t: t}
	run.session(0)
	run.release(0, releasedOrder("tw-1", event.TradingModePaper))
	orders, _, err := report.OrderLifecycles(run.out, event.TargetWeightStrategyBoost)
	if err != nil {
		t.Fatal(err)
	}
	alerts := report.UnacknowledgedAlerts(orders, day(1), time.Minute, func(string) bool { return false })
	if len(alerts) != 1 || !strings.Contains(alerts[0].Body, "may never have received it") {
		t.Fatalf("alerts %+v, want one saying the adapter may never have received the order", alerts)
	}
}

// TestAVenueReportAfterTheOwnersClearanceIsUnread: the reporter reads the
// owner's clearance by the rule the engine applies (event.OrderReports), so
// a venue report on an order the owner cleared, which the engine refuses as
// on a terminal order, is unread here too and raises its alert; a clearance
// the engine refused clears nothing, and the report then acknowledges the
// order (ADR 0026, point 5; ADR 0019, "Alerting").
func TestAVenueReportAfterTheOwnersClearanceIsUnread(t *testing.T) {
	order := releasedOrder("tw-1", event.TradingModePaper)
	at := day(0).Add(time.Minute)
	cleared := func(r *logRun, id string, orders ...string) {
		r.add(journal.KindInput, event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, id, at,
			event.RebalanceClearedPayload{AccountID: order.AccountID, AsOf: at, DecisionID: order.Tag, ClientOrderIDs: orders, Operator: "owner", Detail: "cancelled at IBKR"})
	}
	run := func(clearing func(*logRun)) ([]report.Order, []report.UnreadReport) {
		t.Helper()
		r := &logRun{t: t}
		r.session(0)
		r.release(0, order)
		clearing(r)
		r.acknowledge(order, at.Add(time.Minute))
		orders, unread, err := report.OrderLifecycles(r.out, event.TargetWeightStrategyMomentum)
		if err != nil {
			t.Fatal(err)
		}
		return orders, unread
	}
	orders, unread := run(func(r *logRun) { cleared(r, "cleared", "tw-1") })
	if len(unread) != 1 || unread[0].InputID != "ack:tw-1" || !strings.Contains(unread[0].Reason, "already terminal") {
		t.Errorf("unread %+v, want the report after the clearance unread as on a terminal order", unread)
	}
	if o := stateOf(t, orders, "tw-1"); o.State == report.LifecycleAcknowledged {
		t.Errorf("order %+v acknowledged by a report the engine refuses", o)
	}
	orders, unread = run(func(r *logRun) { cleared(r, "refused", "tw-1", "tw-2") })
	if len(unread) != 0 || stateOf(t, orders, "tw-1").State != report.LifecycleAcknowledged {
		t.Errorf("after a refused clearance: unread %+v, order %+v; want the report read", unread, stateOf(t, orders, "tw-1"))
	}
}

// TestASyntheticReportAcknowledgesNothing: a dry-run momentum run records its
// own synthetic terminal report on each order it releases (ADR 0026, point 3,
// "Mode", amended 2026-10-17). It is a decision, not the venue's report, so
// it acknowledges nothing and counts no fill; but it ends the order as the
// engine reads it, so a venue's report on that order afterwards is one the
// engine refuses, and is unread. A synthetic report the engine could not
// have recorded refuses the log.
func TestASyntheticReportAcknowledgesNothing(t *testing.T) {
	order := releasedOrder("tw-1", event.TradingModeDryRun)
	synthetic := event.LimitOrderLifecyclePayload{AccountID: order.AccountID, ClientOrderID: order.ClientOrderID, InstrumentID: order.InstrumentID,
		Side: order.Side, Status: event.OrderLifecycleStatusCanceled, Quantity: order.Quantity, LimitPrice: order.LimitPrice, OccurredAt: day(0),
		Message: "dry-run: never sent", Synthetic: true}
	run := &logRun{t: t}
	run.session(0)
	run.release(0, order)
	run.add(journal.KindDecision, event.OrderLifecycleEventType, event.OrderLifecycleSyntheticVersion, "synthetic-lifecycle:tw-1", day(0), synthetic)
	run.session(1)
	orders, unread, err := report.OrderLifecycles(run.out, event.TargetWeightStrategyMomentum)
	if err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, orders, "tw-1"); got.State != report.LifecycleReleased || len(unread) != 0 {
		t.Fatalf("after its synthetic report: %+v, unread %+v, want released, unacknowledged, nothing unread", got, unread)
	}

	filled := day(1).Add(time.Hour)
	run.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:1", filled,
		event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "tw-1", FillID: "f1", InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 9, Price: 100, FilledAt: filled})
	orders, unread, err = report.OrderLifecycles(run.out, event.TargetWeightStrategyMomentum)
	if err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, orders, "tw-1").State; got != report.LifecycleReleased || len(unread) != 1 || !strings.Contains(unread[0].Reason, "already terminal") {
		t.Fatalf("a fill after it: %s, unread %+v, want released and the fill unread as the engine refuses it", got, unread)
	}

	for name, c := range map[string]struct {
		version uint32
		p       event.LimitOrderLifecyclePayload
		want    string
	}{
		"at the venue's version": {event.OrderLifecycleLimitVersion, synthetic, "does not read"},
		"on no released order":   {event.OrderLifecycleSyntheticVersion, func() event.LimitOrderLifecyclePayload { p := synthetic; p.ClientOrderID = "tw-9"; return p }(), "never released"},
	} {
		bad := &logRun{t: t}
		bad.session(0)
		bad.release(0, order)
		bad.add(journal.KindDecision, event.OrderLifecycleEventType, c.version, "synthetic-lifecycle", day(0), c.p)
		if _, _, err := report.OrderLifecycles(bad.out, event.TargetWeightStrategyMomentum); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a refusal naming %q", name, err, c.want)
		}
	}
}

// TestARetiredOrderIsARoutineNoticeNeverAPage: the adapter retired an order
// it never sent because its session was over (execution.order.retired), so
// no venue will ever acknowledge it. It is retired, not awaiting the venue:
// the reporter tells the owner once, as a routine notice naming the order
// and why, and never pages it as unacknowledged; an unacknowledged-order
// page already delivered for it is answered by the same notice. A
// retirement the engine refuses (one naming no released order, or one that
// does not describe it) retires nothing. The retirement ends the order as
// the engine reads it, so a venue's report on it after is unread (the
// owner's decision of 2026-10-22; ADR 0019, "Alerting").
func TestARetiredOrderIsARoutineNoticeNeverAPage(t *testing.T) {
	const within = 10 * time.Minute
	order := releasedOrder("tw-1", event.TradingModePaper)
	at := day(1).Add(time.Minute)
	retire := func(r *logRun, id string, p event.OrderRetiredPayload) {
		r.add(journal.KindInput, event.OrderRetiredEventType, event.OrderRetiredSchemaVersion, id, at, p)
	}
	payload := event.OrderRetiredPayload{AccountID: order.AccountID, ClientOrderID: order.ClientOrderID, InstrumentID: order.InstrumentID,
		Side: order.Side, Quantity: order.Quantity, LimitPrice: order.LimitPrice, Reason: event.OrderRetiredSessionOver,
		Detail: "released for the session of 2026-10-16, not today's", RetiredBy: event.OrderRetiredByAdapter, RetiredAt: at}
	none := func(string) bool { return false }
	for _, strategy := range []string{event.TargetWeightStrategyBoost, event.TargetWeightStrategyMomentum} {
		t.Run(strategy, func(t *testing.T) {
			r := &logRun{t: t}
			if strategy == event.TargetWeightStrategyBoost {
				// A Boost order is released by a Boost decision of the
				// account's run, so the count holds it as awaiting the
				// venue's word, which a retirement the engine applies needs.
				boostDecisionOf(r, order.Tag)
			}
			r.session(0)
			r.release(0, order)
			r.session(1)
			moved := payload
			moved.Quantity++
			retire(r, "retired:moved", moved)
			unknown := payload
			unknown.ClientOrderID = "tw-9"
			retire(r, "retired:unknown", unknown)
			r.add(journal.KindInput, event.OrderRetiredEventType, 2, "retired:other-version", at, payload)
			orders, _, err := report.OrderLifecycles(r.out, strategy)
			if err != nil {
				t.Fatal(err)
			}
			if got := report.UnacknowledgedAlerts(orders, day(2), within, none); len(got) != 1 || got[0].Kind != report.KindUnacknowledged {
				t.Fatalf("after retirements the engine refuses: %+v, want the order still paged as unacknowledged", got)
			}

			retire(r, "retired:tw-1", payload)
			r.acknowledge(order, at.Add(time.Minute))
			orders, unread, err := report.OrderLifecycles(r.out, strategy)
			if err != nil {
				t.Fatal(err)
			}
			o := stateOf(t, orders, "tw-1")
			if o.State != report.LifecycleRetired || o.Retirement != event.OrderRetiredSessionOver || !o.RetiredAt.Equal(at) {
				t.Fatalf("order %+v, want it retired for %q at %s", o, event.OrderRetiredSessionOver, at)
			}
			for _, delivered := range []func(string) bool{none, func(key string) bool { return key == "order-unacknowledged:tw-1@joint" }} {
				got := report.UnacknowledgedAlerts(orders, day(2), within, delivered)
				if len(got) != 1 || got[0].Kind != report.KindRetired || got[0].Severity != report.Routine {
					t.Fatalf("a retired order: %+v, want one routine retirement notice and no page", got)
				}
				if got[0].Title != "joint: order tw-1 retired (session over)" || !strings.Contains(got[0].Body, payload.Detail) ||
					got[0].Key != "order-retired:tw-1@joint" {
					t.Errorf("notice %+v, want it to name the order, the reason and the adapter's detail", got[0])
				}
			}
			if len(unread) != 1 || unread[0].InputID != "ack:tw-1" || !strings.Contains(unread[0].Reason, "terminal") {
				t.Errorf("unread %+v, want the venue's report after the retirement unread, as the engine refuses it", unread)
			}
		})
	}
}

// TestARetirementAfterTheOwnersClearanceRetiresNothing: in a momentum run
// the owner's clearance ends the orders it names as the engine reads it,
// so a retirement of one of them afterwards is one the engine refuses, and
// the reporter does not count the order retired.
func TestARetirementAfterTheOwnersClearanceRetiresNothing(t *testing.T) {
	order := releasedOrder("tw-1", event.TradingModePaper)
	at := day(1).Add(time.Minute)
	r := &logRun{t: t}
	r.session(0)
	r.release(0, order)
	r.add(journal.KindInput, event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, "cleared", at,
		event.RebalanceClearedPayload{AccountID: order.AccountID, AsOf: at, DecisionID: order.Tag, ClientOrderIDs: []string{"tw-1"}, Operator: "owner", Detail: "cancelled at IBKR"})
	r.add(journal.KindInput, event.OrderRetiredEventType, event.OrderRetiredSchemaVersion, "retired", at,
		event.OrderRetiredPayload{AccountID: order.AccountID, ClientOrderID: "tw-1", InstrumentID: order.InstrumentID, Side: order.Side,
			Quantity: order.Quantity, LimitPrice: order.LimitPrice, Reason: event.OrderRetiredSessionOver, Detail: "over",
			RetiredBy: event.OrderRetiredByAdapter, RetiredAt: at})
	orders, _, err := report.OrderLifecycles(r.out, event.TargetWeightStrategyMomentum)
	if err != nil {
		t.Fatal(err)
	}
	if o := stateOf(t, orders, "tw-1"); o.State == report.LifecycleRetired {
		t.Errorf("order %+v retired after the clearance ended it", o)
	}
}

// TestAnUnreadableBoostClearanceIsAnUnreadRecordTheReporterPages: the Boost
// engine refuses a clearance it cannot read, and the reporter pages it with
// the unread records, one incident per input, as it pages an unread venue
// report; a momentum account's clearance is not read this way (ADR 0019,
// "Alerting"; ADR 0026, point 2, "Boost", amended 2026-10-18).
func TestAnUnreadableBoostClearanceIsAnUnreadRecordTheReporterPages(t *testing.T) {
	for strategy, want := range map[string]int{event.TargetWeightStrategyBoost: 1, event.TargetWeightStrategyMomentum: 0} {
		run := &logRun{t: t}
		run.session(0)
		run.add(journal.KindInput, event.RebalanceClearedEventType, 99, "cleared:bad", day(1), map[string]string{})
		_, unread, err := report.OrderLifecycles(run.out, strategy)
		if err != nil {
			t.Fatal(err)
		}
		if len(unread) != want || (want == 1 && (unread[0].InputID != "cleared:bad" || unread[0].Type != event.RebalanceClearedEventType)) {
			t.Errorf("%s: unread %+v, want %d", strategy, unread, want)
		}
	}
}

// entriesOf is a stream as the durable log holds it: the owner's, the
// venue's and the run's inputs as inputs, the engine's decisions and
// releases as decisions.
func entriesOf(s *stream) []journal.Entry {
	out := make([]journal.Entry, 0, len(s.out))
	for _, e := range s.out {
		kind := journal.KindDecision
		switch e.Type {
		case event.RebalanceClearedEventType, event.OrderRetiredEventType, event.OrderLifecycleEventType, event.FillEventType, event.OperatorRunApprovedEventType, event.TargetWeightConfigurationEventType, event.BrokerSnapshotEventType:
			kind = journal.KindInput
		}
		out = append(out, journal.Entry{Kind: kind, Envelope: e})
	}
	return out
}

// TestARefusedBoostClearanceIsAnUnreadRecordTheReporterPages: a clearance the
// reporter can read and the Boost engine refuses as a whole (it names an
// order twice, or one this run never released) is paged on every channel as
// the unreadable one is, naming the clearance and its orders; a clearance the
// engine applies pages nothing (ADR 0019, "Alerting"; ADR 0026, point 2,
// "Boost", amended 2026-10-18).
func TestARefusedBoostClearanceIsAnUnreadRecordTheReporterPages(t *testing.T) {
	never := orderplan.ClientOrderID("targets:1", "SPY", event.OrderSideBuy)
	for name, c := range map[string]struct {
		orders  []string
		want    int
		verdict string
	}{
		"applied":    {[]string{never}, 0, ""},
		"twice":      {[]string{never, never}, 1, "not read"},
		"unreleased": {[]string{never, "zz-unreleased"}, 1, "refused"},
	} {
		l := newBoostLog(t)
		l.decide(0)
		l.decide(1, never)
		l.add(event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, "cleared:x", day(2), event.RebalanceClearedPayload{
			AccountID: "joint", AsOf: day(2), DecisionID: "targets:1", ClientOrderIDs: c.orders, Operator: "owner", Detail: "never sent"})
		_, unread, err := report.OrderLifecycles(entriesOf(l.stream), event.TargetWeightStrategyBoost)
		if err != nil {
			t.Fatal(err)
		}
		if len(unread) != c.want {
			t.Fatalf("%s: unread %+v, want %d", name, unread, c.want)
		}
		if c.want == 0 {
			continue
		}
		a := report.UnreadReportAlerts("joint", unread)[0]
		if a.Severity != report.Urgent || !strings.Contains(a.Title, "operator clearance cleared:x "+c.verdict) || strings.Contains(a.Title+a.Body, "venue report") || !strings.Contains(a.Body, never) {
			t.Errorf("%s: alert %+v must name the refused clearance and its orders", name, a)
		}
	}
}

// TestAnUnreadableClearanceAlertNamesTheClearanceNotAVenueReport: the alert
// for a clearance the engine cannot read says so, not that a venue report was
// not read, and names the orders when the payload can be partly read (ADR
// 0019, "Alerting").
func TestAnUnreadableClearanceAlertNamesTheClearanceNotAVenueReport(t *testing.T) {
	run := &logRun{t: t}
	run.add(journal.KindInput, event.RebalanceClearedEventType, 99, "cleared:bad", day(1), map[string]string{})
	_, unread, err := report.OrderLifecycles(run.out, event.TargetWeightStrategyBoost)
	if err != nil || len(unread) != 1 {
		t.Fatalf("unread %+v, %v", unread, err)
	}
	a := report.UnreadReportAlerts("joint", unread)[0]
	if !strings.Contains(a.Title, "operator clearance cleared:bad not read") || strings.Contains(a.Title+a.Body, "venue report") {
		t.Errorf("alert %+v must name the operator clearance", a)
	}
}

// TestAStrategyThatIsNeitherBoostNorMomentumReadsNoClearance: the two
// places that classify a strategy agree that anything not Boost is not
// Boost and anything not momentum is not momentum, so a clearance in a run of
// an unknown strategy is neither paged as a Boost one nor applied as a
// momentum one (fail closed for the Boost-only count).
func TestAStrategyThatIsNeitherBoostNorMomentumReadsNoClearance(t *testing.T) {
	run := &logRun{t: t}
	run.add(journal.KindInput, event.RebalanceClearedEventType, 99, "cleared:bad", day(1), map[string]string{})
	_, unread, err := report.OrderLifecycles(run.out, "mystery")
	if err != nil || len(unread) != 0 {
		t.Errorf("unread %+v, %v; want none", unread, err)
	}
}

// TestAClearanceIsJudgedAgainstTheCurrentRunsCountAndOwnAccount: a fresh
// run's approval ends the count, so a clearance of an order the last run
// released is refused after it, and another account's clearance is no
// business of this one's (ADR 0026, point 2, "Boost", amended 2026-10-18;
// ADR 0027).
func TestAClearanceIsJudgedAgainstTheCurrentRunsCountAndOwnAccount(t *testing.T) {
	never := orderplan.ClientOrderID("targets:1", "SPY", event.OrderSideBuy)
	clearOrder := func(l *boostLog, id, account string) {
		l.add(event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, id, day(3), event.RebalanceClearedPayload{
			AccountID: account, AsOf: day(3), DecisionID: "targets:1", ClientOrderIDs: []string{never}, Operator: "owner", Detail: "never sent"})
	}
	for name, c := range map[string]struct {
		approve bool
		account string
		want    int
		refused bool
	}{
		"same run":          {false, "joint", 0, false},
		"after a fresh run": {true, "joint", 1, true},
		"another account":   {true, "other", 1, true},
	} {
		l := newBoostLog(t)
		l.decide(0)
		l.decide(1, never)
		if c.approve {
			l.add(event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "approved:2", day(2), *approvalOf("", ""))
		}
		clearOrder(l, "cleared:x", c.account)
		_, unread, err := report.OrderLifecycles(entriesOf(l.stream), event.TargetWeightStrategyBoost)
		if err != nil || len(unread) != c.want {
			t.Errorf("%s: unread %+v, %v; want %d", name, unread, err, c.want)
		}
		if len(unread) == 1 && unread[0].Refused != c.refused {
			t.Errorf("%s: refused %v, want %v: the engine refuses it, so it is paged as refused", name, unread[0].Refused, c.refused)
		}
	}
}

// boostDecisionOf journals a Boost account's configuration and the Boost
// decision tag that releases its orders, as the engine journals them.
func boostDecisionOf(r *logRun, tag string) {
	for _, e := range newStream(r.t).out {
		r.seq++
		e.Sequence = r.seq
		r.out = append(r.out, journal.Entry{Kind: journal.KindInput, Envelope: e})
	}
	r.add(journal.KindDecision, event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, tag, day(0), event.TargetsDecidedPayload{
		AccountID: "joint", StrategyID: event.TargetWeightStrategyBoost, DecisionTime: day(0), SessionPeriodEnd: day(0),
		Outcome: event.TargetsOutcomeDecided, Targets: []event.TargetWeight{{InstrumentID: "SPY", Weight: 0.98}},
		Boost: event.BoostFigures{InstrumentID: "SPY", Close: 100}})
}

// TestABoostRetirementOfAnOrderTheCountDoesNotHoldIsReadByItsOwnRule: in a
// Boost run the engine reads the retirement of an order its count does not
// hold as awaiting the venue's word, such as the sale back to 98%, by the
// order's own rule: it is retired when no venue has named it, by a report,
// an execution or a broker snapshot's open-order listing (ADR 0026, point
// 2, "Boost", amended 2026-10-22; fail closed). The reporter reads it alike.
func TestABoostRetirementOfAnOrderTheCountDoesNotHoldIsReadByItsOwnRule(t *testing.T) {
	l := newBoostLog(t)
	l.decide(0)
	sale := func(id string) event.OrderReleasedPayload {
		return event.OrderReleasedPayload{AccountID: "joint", PlanID: "plan:" + id, Tag: "alarm:u", ClientOrderID: id,
			InstrumentID: "SPY", Side: event.OrderSideSell, Quantity: 10, LimitPrice: 95, Mode: event.TradingModePaper, Send: true, Figures: figures}
	}
	retired := func(id string) event.OrderRetiredPayload {
		return event.OrderRetiredPayload{AccountID: "joint", ClientOrderID: id, InstrumentID: "SPY", Side: event.OrderSideSell, Quantity: 10, LimitPrice: 95,
			Reason: event.OrderRetiredSessionOver, Detail: "session over", RetiredBy: event.OrderRetiredByAdapter, RetiredAt: day(1)}
	}
	l.add(event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, "release:alarm-1", day(1), sale("alarm-1"))
	l.add(event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, "release:alarm-2", day(1), sale("alarm-2"))
	l.add(event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "broker:1", day(1), event.BrokerSnapshotPayload{
		AccountID: "joint", AsOf: day(1), NetLiquidation: 10000, ExcessLiquidity: 10000, Positions: []event.BrokerPosition{},
		OpenOrders: []event.BrokerOrder{{BrokerOrderID: "7", ClientOrderID: "alarm-2", InstrumentID: "SPY", Side: event.OrderSideSell,
			Status: event.BrokerOrderWorking, RemainingQuantity: 10}}})
	// Another account's snapshot, and one that cannot be read, name nothing.
	l.add(event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "broker:ira", day(1), event.BrokerSnapshotPayload{
		AccountID: "ira", AsOf: day(1), NetLiquidation: 10000, ExcessLiquidity: 10000, Positions: []event.BrokerPosition{},
		OpenOrders: []event.BrokerOrder{{BrokerOrderID: "8", ClientOrderID: "alarm-1", InstrumentID: "SPY", Side: event.OrderSideSell,
			Status: event.BrokerOrderWorking, RemainingQuantity: 10}}})
	l.add(event.BrokerSnapshotEventType, 99, "broker:unread", day(1), map[string]string{})
	l.add(event.OrderRetiredEventType, event.OrderRetiredSchemaVersion, "retired:alarm-1", day(1), retired("alarm-1"))
	l.add(event.OrderRetiredEventType, event.OrderRetiredSchemaVersion, "retired:alarm-2", day(1), retired("alarm-2"))
	orders, _, err := report.OrderLifecycles(entriesOf(l.stream), event.TargetWeightStrategyBoost)
	if err != nil {
		t.Fatal(err)
	}
	if o := stateOf(t, orders, "alarm-1"); o.State != report.LifecycleRetired {
		t.Errorf("order %+v not retired, though no venue named it", o)
	}
	if o := stateOf(t, orders, "alarm-2"); o.State == report.LifecycleRetired {
		t.Errorf("order %+v retired, though IBKR listed it, so it was sent", o)
	}
}

func TestFractionalRetirementStopsPagingAndDisplaysShares(t *testing.T) {
	r := &logRun{t: t}
	o := releasedOrder("fractional", event.TradingModePaper)
	o.Quantity, o.QuantityScale = 72500, 10000
	r.session(0)
	r.add(journal.KindDecision, event.OrderReleasedEventType, o.SchemaVersion(), "release", day(0), o)
	orders, _, err := report.OrderLifecycles(r.out, event.TargetWeightStrategyMomentum)
	if err != nil {
		t.Fatal(err)
	}
	alerts := report.UnacknowledgedAlerts(orders, day(2), time.Minute, func(string) bool { return false })
	if len(alerts) != 1 || !strings.Contains(alerts[0].Body, "buy 7.25 SPY") {
		t.Errorf("unacknowledged alert %+v", alerts)
	}
	p := event.OrderRetiredPayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, InstrumentID: o.InstrumentID, Side: o.Side, Quantity: o.Quantity, QuantityScale: o.QuantityScale, LimitPrice: o.LimitPrice, Reason: event.OrderRetiredSessionOver, Detail: "session ended", RetiredBy: event.OrderRetiredByAdapter, RetiredAt: day(1)}
	r.add(journal.KindInput, event.OrderRetiredEventType, p.SchemaVersion(), "retired", day(1), p)
	orders, _, err = report.OrderLifecycles(r.out, event.TargetWeightStrategyMomentum)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 || orders[0].State != report.LifecycleRetired {
		t.Fatalf("retirement ignored: %+v", orders)
	}
	alerts = report.UnacknowledgedAlerts(orders, day(2), time.Minute, func(string) bool { return false })
	if len(alerts) != 1 || alerts[0].Severity != report.Routine || alerts[0].Kind != report.KindRetired || !strings.Contains(alerts[0].Body, "buy 7.25 SPY") {
		t.Fatalf("retirement alerts %+v", alerts)
	}
}
