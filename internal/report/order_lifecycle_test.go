package report_test

import (
	"encoding/json"
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

	orders, err := report.OrderLifecycles(run.out)
	if err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, orders, "tw-1"); got.State != report.LifecycleCommitted || !got.CommittedAt.Equal(day(0)) || !got.Send {
		t.Fatalf("after its release: %+v, want committed at %s, to be sent", got, day(0))
	}

	run.session(1)
	orders, _ = report.OrderLifecycles(run.out)
	if got := stateOf(t, orders, "tw-1").State; got != report.LifecycleReleased {
		t.Fatalf("after a later input: %s, want %s", got, report.LifecycleReleased)
	}

	acked := day(1).Add(time.Minute)
	run.acknowledge(order, acked)
	orders, _ = report.OrderLifecycles(run.out)
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
	orders, err := report.OrderLifecycles(run.out)
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
	orders, _ = report.OrderLifecycles(run.out)
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
// order too, so a fill naming its client order ID acknowledges it, and a
// report naming no released order changes nothing.
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
	orders, err := report.OrderLifecycles(run.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 {
		t.Fatalf("orders %+v, want the one released", orders)
	}
	if got := orders[0]; got.State != report.LifecycleAcknowledged || got.Acknowledgement != "fill" || !got.AcknowledgedAt.Equal(filled) {
		t.Fatalf("after its fill: %+v, want acknowledged by the fill at %s", got, filled)
	}
}

// TestOrderLifecyclesReadsOnlyWhatItCanRead: a release, lifecycle report or
// fill at a version this build does not read, or that does not validate,
// is refused, never guessed at (ADR 0015). Other decisions change nothing,
// and an order released again under its client order ID keeps its first
// release.
func TestOrderLifecyclesReadsOnlyWhatItCanRead(t *testing.T) {
	order := releasedOrder("tw-1", event.TradingModePaper)
	for name, add := range map[string]func(*logRun){
		"a release at another version": func(r *logRun) {
			r.add(journal.KindDecision, event.OrderReleasedEventType, event.OrderReleasedSchemaVersion+1, "release", day(0), order)
		},
		"a lifecycle report that does not validate": func(r *logRun) {
			r.add(journal.KindInput, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "ack", day(0), event.LimitOrderLifecyclePayload{ClientOrderID: "tw-1"})
		},
		"a fill that does not validate": func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill", day(0), event.TargetWeightFillPayload{ClientOrderID: "tw-1"})
		},
	} {
		run := &logRun{t: t}
		run.session(0)
		add(run)
		if _, err := report.OrderLifecycles(run.out); err == nil {
			t.Errorf("%s: read without error", name)
		}
	}

	run := &logRun{t: t}
	run.session(0)
	run.add(journal.KindDecision, event.OrderPlannedEventType, event.OrderPlannedSchemaVersion, "plan", day(0), struct{}{})
	run.release(0, order)
	run.session(1)
	again := order
	again.Quantity = 5
	run.add(journal.KindDecision, event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, "release-again", day(1), again)
	orders, err := report.OrderLifecycles(run.out)
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
	orders, err := report.OrderLifecycles(run.out)
	if err != nil {
		t.Fatal(err)
	}
	alerts := report.UnacknowledgedAlerts(orders, day(1), time.Minute, func(string) bool { return false })
	if len(alerts) != 1 || !strings.Contains(alerts[0].Body, "may never have received it") {
		t.Fatalf("alerts %+v, want one saying the adapter may never have received the order", alerts)
	}
}
