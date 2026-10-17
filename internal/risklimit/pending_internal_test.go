package risklimit

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// No snapshot is credited with more of an instrument's sells filled than the
// gate counts filled now, so the sells a snapshot cannot show are never
// fewer than zero (the invariant unshownSells' fail-closed branch rests on).
// Fill reports, lifecycle reports and account and broker snapshots arrive in
// any order and at times that collide within a second, across two sells.
func TestNoSnapshotIsCreditedWithMoreThanIsFilledNow(t *testing.T) {
	base := time.Date(2025, time.March, 5, 16, 0, 0, 0, time.UTC)
	const tag = "targets-decided:joint:2025-03-04"
	statuses := []string{event.OrderLifecycleStatusSubmitted, event.OrderLifecycleStatusCancelPending, event.OrderLifecycleStatusCanceled, event.OrderLifecycleStatusInvalid}
	limits := event.MarginLimits{
		MaxExposure: 1.5, MaxOrdersPerDecision: 1, MaxOrderFraction: 0.5, PriceBand: 0.05,
		ExcessLiquidityWarning: 0.4, ExcessLiquidityUrgent: 0.2, ExcessLiquidityStaleMinutes: 30,
	}
	for seed := int64(1); seed <= 500; seed++ {
		rng := rand.New(rand.NewSource(seed))
		g, err := NewGate("joint", limits, event.TradingModeDryRun, 30*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		g.RecordDecision(tag)
		snapshot := func(asOf time.Time, held int64) event.BrokerSnapshotPayload {
			return event.BrokerSnapshotPayload{
				AccountID: "joint", AsOf: asOf, MarketOpen: true, NetLiquidation: 10000, ExcessLiquidity: 6000,
				Positions: []event.BrokerPosition{{InstrumentID: "SPY", Quantity: held, Mark: 1, MarkSet: true}}, OpenOrders: []event.BrokerOrder{},
			}
		}
		if _, err := g.ObserveBroker("broker:0", snapshot(base, 100)); err != nil {
			t.Fatal(err)
		}
		g.ObserveAccount(event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: base, NetLiquidation: 10000, Currency: "USD", Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: 100}}})
		const release = 30
		clients := []string{"tw-a", "tw-b"}
		reported := map[string]int64{}
		for _, c := range clients {
			o := Order{
				PlanID: "order-planned:" + c,
				Planned: event.OrderPlannedPayload{
					AccountID: "joint", Tag: tag, ClientOrderID: c, InstrumentID: "SPY", Side: event.OrderSideSell, Phase: event.OrderSideSell,
					OrderType: event.OrderTypeLimit, Quantity: release, LimitPrice: 1, TargetWeight: 1,
					NetLiquidation: 10000, HeldQuantity: 100, ReferencePrice: 1, PriceBand: 0.05,
				},
				Mark: 1, MarkSet: true,
			}
			outcomes, err := g.Check(Book{Holdings: map[string]int64{"SPY": 100}, Prices: map[string]float64{"SPY": 1}, At: base}, []Order{o})
			if err != nil || outcomes[0].Released == nil {
				t.Fatalf("seed %d: setup release of %s: %v %+v", seed, c, err, outcomes)
			}
		}
		lastBroker := base
		probes := []time.Time{base, endOfTime}
		for step := 1; step <= 16; step++ {
			when := base.Add(time.Duration(rng.Intn(8)) * time.Second)
			probes = append(probes, when)
			c := clients[rng.Intn(len(clients))]
			switch rng.Intn(4) {
			case 0:
				if reported[c] < release {
					q := 1 + rng.Int63n(release-reported[c])
					reported[c] += q
					if err := g.ObserveFill(event.TargetWeightFillPayload{
						AccountID: "joint", ClientOrderID: c, FillID: fmt.Sprintf("f%d", step), InstrumentID: "SPY",
						Side: event.OrderSideSell, Quantity: q, Price: 1, FilledAt: when,
					}); err != nil {
						t.Fatal(err)
					}
				}
			case 1:
				if err := g.ObserveLifecycle(event.LimitOrderLifecyclePayload{
					AccountID: "joint", ClientOrderID: c, OrderID: "ib-" + c, InstrumentID: "SPY", Side: event.OrderSideSell,
					Status: statuses[rng.Intn(len(statuses))], Quantity: release, LimitPrice: 1, OccurredAt: when,
				}); err != nil {
					t.Fatal(err)
				}
			case 2:
				// Broker snapshots follow one another; every other fact
				// may be recorded out of order against them.
				if !when.After(lastBroker) {
					when = lastBroker.Add(time.Second)
					probes = append(probes, when)
				}
				lastBroker = when
				s := snapshot(when, 40+rng.Int63n(61))
				if rng.Intn(2) == 0 {
					s.OpenOrders = []event.BrokerOrder{{BrokerOrderID: "ib-" + c, ClientOrderID: c, InstrumentID: "SPY", Side: event.OrderSideSell, Status: event.BrokerOrderWorking, RemainingQuantity: 1 + rng.Int63n(release)}}
				}
				if rng.Intn(4) == 0 {
					s.Unreadable, s.OpenOrders = []string{event.BrokerComponentOpenOrders}, nil
				}
				if _, err := g.ObserveBroker(fmt.Sprintf("broker:%d", step), s); err != nil {
					t.Fatal(err)
				}
			default:
				g.ObserveAccount(event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: when, NetLiquidation: 10000, Currency: "USD", Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: 40 + rng.Int63n(61)}}})
			}
			filled, _ := g.shares("SPY")
			for _, p := range probes {
				if by := g.filledBy("SPY", p); by > filled {
					t.Fatalf("seed %d step %d: a snapshot at %s is credited with %d filled; %d are filled now", seed, step, p.Format(time.TimeOnly), by, filled)
				}
			}
		}
	}
}

// Were a snapshot ever credited with more filled than the gate counts filled
// now, what it shows of the sells' fills could not be read: it is credited
// with none, so every share the sells filled or still pend counts as
// working, never a count below zero. A listing stamped beyond endOfTime stands in for the
// broken invariant: no recorded fact can be, so the state is set directly.
func TestASnapshotCreditedBeyondTheFillsNowIsCreditedWithNone(t *testing.T) {
	g, err := NewGate("joint", event.MarginLimits{
		MaxExposure: 1.5, MaxOrdersPerDecision: 1, MaxOrderFraction: 0.5, PriceBand: 0.05,
		ExcessLiquidityWarning: 0.4, ExcessLiquidityUrgent: 0.2, ExcessLiquidityStaleMinutes: 30,
	}, event.TradingModeDryRun, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	beyond := endOfTime.Add(time.Second)
	g.sells["tw-a"] = &trackedSell{instrumentID: "SPY", released: 60, listed: []shown{{at: beyond, filled: 30}}}
	g.sellOrder = append(g.sellOrder, "tw-a")
	if filled, _ := g.shares("SPY"); g.filledBy("SPY", beyond) <= filled {
		t.Fatalf("setup: the invariant holds")
	}
	if got := g.unshownSells("SPY", beyond); got != 60 {
		t.Errorf("unshown sells %d, want all 60 released", got)
	}
}
