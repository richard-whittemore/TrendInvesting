package restart_test

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// ADR 0027: recorded fills use the live acceptance rule. An overflowing
// buy cannot end the last working order or suppress its unread-report alert.
func TestOverflowingFillAgreesLiveRecoveryAndReplay(t *testing.T) {
	for _, mode := range []string{event.ShareQuantityMinimumLot, event.ShareQuantityFractional} {
		for _, cause := range []string{"product", "commission"} {
			t.Run(mode+"/"+cause, func(t *testing.T) {
				_, cfg := newHandlerRun(t)
				cfg.Execution.ShareQuantity = mode
				run := newConfiguredHandlerRun(t, cfg, false)
				orders := run.month()
				o := orders[0]
				if o.Side != event.OrderSideBuy || o.Quantity <= event.QuantityUnits(o.QuantityScale) {
					t.Fatalf("fixture must release a buy larger than one share: %+v", o)
				}
				for _, other := range orders[1:] {
					p := event.LimitOrderLifecyclePayload{AccountID: other.AccountID, ClientOrderID: other.ClientOrderID, OrderID: "ib-" + other.ClientOrderID, InstrumentID: other.InstrumentID, Side: other.Side, Status: event.OrderLifecycleStatusCanceled, Quantity: other.Quantity, QuantityScale: other.QuantityScale, LimitPrice: other.LimitPrice, OccurredAt: afterMonth}
					id := "cancel:" + other.ClientOrderID
					run.apply(id, event.OrderLifecycleEventType, p.SchemaVersion(), run.hash, afterMonth, p)
					if err := run.errs[id]; err != nil {
						t.Fatal(err)
					}
				}
				p := event.TargetWeightFillPayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, FillID: "overflow", InstrumentID: o.InstrumentID, Side: o.Side, Quantity: o.Quantity, QuantityScale: o.QuantityScale, Price: math.MaxFloat64, FilledAt: afterMonth}
				if cause == "commission" {
					p.Price = math.MaxFloat64 / event.QuantityValue(p.Quantity, p.QuantityScale, 1) / 2
					p.Commission = math.MaxFloat64
				}
				if err := p.ValidateSchema(p.SchemaVersion()); err != nil {
					t.Fatal(err)
				}
				for _, id := range []string{"overflow", "overflow-again"} {
					run.apply(id, event.FillEventType, p.SchemaVersion(), run.hash, afterMonth, p)
					if err := run.errs[id]; err == nil || !strings.Contains(err.Error(), "finite monetary range") {
						t.Fatalf("live must refuse overflowing fill %s: %v", id, err)
					}
				}
				entries := run.recorder.Entries()
				got, err := restart.RebalanceAtEnd(entries, "overflow.log")
				if err != nil || got == nil || got.Phase != event.RebalancePhaseBuysWorking || !reflect.DeepEqual(got.ClientOrderIDs, []string{o.ClientOrderID}) {
					t.Errorf("recovery must retain last working buy: %s, %v", describeRebalance(got), err)
				}
				approval := event.OperatorRunApprovedPayload{AccountID: cfg.AccountID, Momentum: &event.MomentumStateInForce{}}
				if err := restart.CheckRebalanceInProgress(entries, approval, "overflow.log"); err == nil || !strings.Contains(err.Error(), "mid-rebalance") {
					t.Errorf("approval omitting unresolved order must refuse: %v", err)
				}
				states, unread, err := report.OrderLifecycles(entries, cfg.StrategyID)
				if err != nil {
					t.Fatal(err)
				}
				if len(unread) != 2 {
					t.Errorf("reporter must leave both overflow attempts unread: %+v", unread)
				} else {
					for i, id := range []string{"overflow", "overflow-again"} {
						if unread[i].InputID != id || unread[i].ClientOrderID != o.ClientOrderID || !strings.Contains(unread[i].Reason, "finite monetary range") {
							t.Errorf("unread report: %+v", unread[i])
						}
					}
				}
				for _, state := range states {
					if state.ClientOrderID == o.ClientOrderID && state.State != report.LifecycleReleased {
						t.Errorf("overflow acknowledged the order: %+v", state)
					}
				}
				// A clearance naming exactly this order proves the live handler
				// still holds it working; it refuses a completed order.
				run.clear(cfg.AccountID, o.Tag, []string{o.ClientOrderID})
				if err := run.errs["cleared:"+o.Tag+":"+cfg.AccountID]; err != nil {
					t.Fatalf("live order was not left working: %v", err)
				}
				entries = run.recorder.Entries()
				h, err := targetweight.New("momentum/test", cfg, targetweight.Options{DecideFrom: entries[0].Envelope.EventTime})
				if err != nil {
					t.Fatal(err)
				}
				replayed := journal.NewRecorder(h)
				for _, entry := range entries {
					if entry.Kind != journal.KindInput {
						continue
					}
					_, err := replayed.Apply(context.Background(), entry.Envelope)
					if fmt.Sprint(err) != fmt.Sprint(run.errs[entry.Envelope.ID]) {
						t.Fatalf("replay refusal differs for %s: %v, live %v", entry.Envelope.ID, err, run.errs[entry.Envelope.ID])
					}
				}
				if !reflect.DeepEqual(entries, replayed.Entries()) {
					t.Fatal("month journal replay differs, including recorded overflow refusals and clearance")
				}
			})
		}
	}
}
