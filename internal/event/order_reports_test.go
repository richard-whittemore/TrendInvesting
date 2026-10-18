package event_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestOrderReportsReadsAReportAsTheMomentumRunDoes: the one rule by which a
// run that reads fills reads the venue's reports on its released orders. A
// report naming no released order is ErrNotReleased; one that does not
// describe the order, takes it past its quantity in total, reuses a fill id
// for another execution, or arrives once the order is terminal is refused;
// the same execution again is a redelivery that changes nothing.
func TestOrderReportsReadsAReportAsTheMomentumRunDoes(t *testing.T) {
	at := time.Date(2025, 2, 3, 21, 0, 0, 0, time.UTC)
	released := event.OrderReleasedPayload{AccountID: "ira", ClientOrderID: "tw-1", InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: 9, LimitPrice: 10}
	fill := func(id string, quantity int64) event.TargetWeightFillPayload {
		return event.TargetWeightFillPayload{AccountID: "ira", ClientOrderID: "tw-1", FillID: id, InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: quantity, Price: 10, FilledAt: at}
	}
	lifecycle := event.LimitOrderLifecyclePayload{AccountID: "ira", ClientOrderID: "tw-1", OrderID: "7", InstrumentID: "AAA", Side: event.OrderSideBuy,
		Status: event.OrderLifecycleStatusSubmitted, Quantity: 9, LimitPrice: 10 + 1e-9, OccurredAt: at}
	refused := func(err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v, want one mentioning %q", err, want)
		}
	}

	r := event.NewOrderReports()
	r.Release(released)
	again := released
	again.Quantity = 5
	r.Release(again)
	if o, ok := r.Order("tw-1"); !ok || o.Released.Quantity != 9 {
		t.Fatalf("order %+v, want its first release kept", o)
	}
	if _, err := r.Lifecycle(lifecycle); err != nil {
		t.Fatalf("a lifecycle report that describes the order: %v", err)
	}
	other := lifecycle
	other.ClientOrderID = "tw-other"
	if _, err := r.Lifecycle(other); !errors.Is(err, event.ErrNotReleased) {
		t.Errorf("a report naming no released order: %v, want ErrNotReleased", err)
	}
	moved := lifecycle
	moved.Quantity++
	_, err := r.Lifecycle(moved)
	refused(err, "as released")
	sold := fill("f0", 1)
	sold.Side = event.OrderSideSell
	_, _, err = r.Fill(sold)
	refused(err, "describes")
	_, _, err = r.Fill(fill("f0", 10))
	refused(err, "past its")

	if o, redelivery, err := r.Fill(fill("f1", 5)); err != nil || redelivery || o.Filled != 5 || o.Terminal {
		t.Fatalf("first fill: %+v %v %v", o, redelivery, err)
	}
	if _, redelivery, err := r.Fill(fill("f1", 5)); err != nil || !redelivery {
		t.Errorf("the same execution again: redelivery %v, %v; want a redelivery", redelivery, err)
	}
	_, _, err = r.Fill(fill("f1", 1))
	refused(err, "reuses fill id")
	_, _, err = r.Fill(fill("f2", 5))
	refused(err, "past its")
	o, _, err := r.Fill(fill("f3", 4))
	if err != nil || !o.Terminal || len(o.Fills) != 2 {
		t.Fatalf("the fill that completes it: %+v %v, want it terminal with two fills", o, err)
	}
	_, err = r.Lifecycle(lifecycle)
	refused(err, "already terminal")
	_, _, err = r.Fill(fill("f4", 1))
	refused(err, "already terminal")

	r.Release(event.OrderReleasedPayload{AccountID: "ira", ClientOrderID: "tw-2", InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: 9, LimitPrice: 10})
	r.Release(event.OrderReleasedPayload{AccountID: "ira", ClientOrderID: "tw-0", InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: 1, LimitPrice: 10})
	if got := r.Working(); !slices.Equal(got, []string{"tw-0", "tw-2"}) {
		t.Errorf("working %v, want the orders not terminal, sorted", got)
	}
	cancelled := lifecycle
	cancelled.ClientOrderID, cancelled.Status, cancelled.LimitPrice = "tw-2", event.OrderLifecycleStatusCanceled, 10
	if o, err := r.Lifecycle(cancelled); err != nil || !o.Terminal {
		t.Errorf("a cancellation: %+v %v, want the order terminal", o, err)
	}
}

// TestOrderReportsRefusesAFillPastTheOrderWhateverItsSize: a fill quantity
// so large that the order's filled total would wrap past int64 is still
// past the order, never read as a fill within it.
func TestOrderReportsRefusesAFillPastTheOrderWhateverItsSize(t *testing.T) {
	at := time.Date(2025, 2, 3, 21, 0, 0, 0, time.UTC)
	r := event.NewOrderReports()
	r.Release(event.OrderReleasedPayload{AccountID: "ira", ClientOrderID: "tw-1", InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: 9, LimitPrice: 10})
	fill := func(id string, quantity int64) event.TargetWeightFillPayload {
		return event.TargetWeightFillPayload{AccountID: "ira", ClientOrderID: "tw-1", FillID: id, InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: quantity, Price: 10, FilledAt: at}
	}
	if _, _, err := r.Fill(fill("f1", 5)); err != nil {
		t.Fatalf("first fill: %v", err)
	}
	if _, _, err := r.Fill(fill("f2", math.MaxInt64)); err == nil || !strings.Contains(err.Error(), "past its") {
		t.Fatalf("a fill of MaxInt64 shares on a part-filled order: %v, want it refused as past the order", err)
	}
	if o, _ := r.Order("tw-1"); o.Filled != 5 || o.Terminal {
		t.Errorf("order %+v, want it left at 5 filled and working", o)
	}
}

// adapterRecord is one input as the IBKR adapter's paper sender journals it
// (adapter/ibkr/reports.py): its event type, schema version and payload.
type adapterRecord struct {
	EventType     string          `json:"event_type"`
	SchemaVersion uint32          `json:"schema_version"`
	Payload       json.RawMessage `json:"payload"`
}

// readAdapterFile decodes one of the IBKR adapter's test files, refusing a
// field the engine does not know.
func readAdapterFile(t *testing.T, name string, into any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "adapter", "ibkr", "tests", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	strict(t, name, raw, into)
}

func strict(t *testing.T, name string, raw []byte, into any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// TestTheIBKRAdaptersPaperReportsAreReadByOrderReports is the contract with
// the IBKR adapter's paper sender: its test sends the released order in
// paper_released.json to the fake gateway and journals exactly the records
// in each reports file, and the engine reads every one of them by the rule
// it reads a venue's reports by (OrderReports), at the versions it reads a
// venue's reports at (lifecycle version 2, fill version 5), to the order's
// end: filled in two executions, or refused by IBKR.
func TestTheIBKRAdaptersPaperReportsAreReadByOrderReports(t *testing.T) {
	var released event.OrderReleasedPayload
	readAdapterFile(t, "paper_released.json", &released)
	if err := released.ValidateSchema(released.SchemaVersion()); err != nil {
		t.Fatal(err)
	}
	if released.Mode != event.TradingModePaper || !released.Send || !strings.HasPrefix(released.AccountID, event.PaperAccountPrefix) {
		t.Fatalf("released %+v, want a paper release to send to a paper account", released)
	}
	for _, tc := range []struct {
		file             string
		lifecycles       int
		fills            int
		filled           int64
		finalStatusWants string
	}{
		{"paper_reports.json", 1, 2, released.Quantity, event.OrderLifecycleStatusSubmitted},
		{"paper_rejected.json", 1, 0, 0, event.OrderLifecycleStatusInvalid},
	} {
		t.Run(tc.file, func(t *testing.T) {
			var records []adapterRecord
			readAdapterFile(t, tc.file, &records)
			r := event.NewOrderReports()
			r.Release(released)
			var lifecycles, fills int
			var last string
			for i, rec := range records {
				switch rec.EventType {
				case event.OrderLifecycleEventType:
					var p event.LimitOrderLifecyclePayload
					strict(t, tc.file, rec.Payload, &p)
					if rec.SchemaVersion != event.OrderLifecycleLimitVersion || p.SchemaVersion() != rec.SchemaVersion {
						t.Fatalf("record %d: lifecycle at version %d, want a venue's report at %d", i, rec.SchemaVersion, event.OrderLifecycleLimitVersion)
					}
					if err := p.ValidateSchema(rec.SchemaVersion); err != nil {
						t.Fatalf("record %d: %v", i, err)
					}
					if _, err := r.Lifecycle(p); err != nil {
						t.Fatalf("record %d: OrderReports refused the lifecycle report: %v", i, err)
					}
					lifecycles++
					last = p.Status
				case event.FillEventType:
					var p event.TargetWeightFillPayload
					strict(t, tc.file, rec.Payload, &p)
					if rec.SchemaVersion != event.FillTargetWeightVersion {
						t.Fatalf("record %d: fill at version %d, want %d", i, rec.SchemaVersion, event.FillTargetWeightVersion)
					}
					if err := p.Validate(); err != nil {
						t.Fatalf("record %d: %v", i, err)
					}
					if _, redelivery, err := r.Fill(p); err != nil || redelivery {
						t.Fatalf("record %d: OrderReports read the fill as redelivery %v, err %v", i, redelivery, err)
					}
					fills++
				default:
					t.Fatalf("record %d: event type %q is neither a lifecycle report nor a fill", i, rec.EventType)
				}
			}
			o, _ := r.Order(released.ClientOrderID)
			if lifecycles != tc.lifecycles || fills != tc.fills || o.Filled != tc.filled || !o.Terminal || last != tc.finalStatusWants {
				t.Errorf("%d lifecycle reports (last %q), %d fills, %d filled, terminal %v; want %d (last %q), %d, %d, terminal",
					lifecycles, last, fills, o.Filled, o.Terminal, tc.lifecycles, tc.finalStatusWants, tc.fills, tc.filled)
			}
		})
	}
}
