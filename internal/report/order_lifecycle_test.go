package report_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
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
// Describes); one that does not is unread. A Boost run reads no fill, so a
// fill acknowledges nothing there (ADR 0022; ADR 0019, "Alerting").
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
		r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:1", at,
			event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "tw-1", FillID: "f1", InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 9, Price: 100, FilledAt: at})
	})
	if got.State == report.LifecycleAcknowledged || len(unread) != 0 {
		t.Errorf("a fill in a Boost run, which reads none: %+v, unread %+v; want nothing acknowledged", got, unread)
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
