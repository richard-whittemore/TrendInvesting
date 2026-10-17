package risklimit

import (
	"fmt"
	"math/rand"
	"slices"
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

// trueFill is an execution in the simulated broker: the shares, the second
// IBKR stamps it with, and its place in the broker's true order.
type trueFill struct {
	client   string
	quantity int64
	sec      int
	seq      int
}

// fact is one recorded fact of the simulated broker, at its place seq in the
// true order: a fill report (fill), a cancellation (ends names the sell), or
// a snapshot (stream is "account" or "broker", taken at second asOf; shows
// when the gate measures its holding's fall). record applies it to the gate.
type fact struct {
	seq     int
	stream  string
	shows   bool
	fill    *trueFill
	ends    string
	asOf    int
	account bool
	record  func(g *Gate) error
}

// PendingSells is never below what is truly being sold that the latest
// account snapshot cannot show (fail closed: never sized past the holding).
// A simulated broker executes and cancels two sells of 30 against a holding
// of 100, second by second, and takes account and broker snapshots of the
// truth, at most one a second; IBKR stamps each fact in whole seconds, so
// facts in one second keep only their true order. The facts are recorded in
// random order, account and broker snapshots each in their own order. The
// truth is the shares of each sell executed after the latest recorded
// account snapshot, and every sell's shares still working. A sell whose
// cancellation is recorded counts only the executions the gate can know of:
// those reported, and those before a recorded snapshot whose holding the
// gate measures (an account snapshot, or a broker snapshot that read the
// open orders).
func TestPendingSellsIsNeverBelowWhatIsTrulyBeingSold(t *testing.T) {
	base := time.Date(2025, time.March, 5, 16, 0, 0, 0, time.UTC)
	const tag = "targets-decided:joint:2025-03-04"
	const release = 30
	limits := event.MarginLimits{
		MaxExposure: 1.5, MaxOrdersPerDecision: 1, MaxOrderFraction: 0.5, PriceBand: 0.05,
		ExcessLiquidityWarning: 0.4, ExcessLiquidityUrgent: 0.2, ExcessLiquidityStaleMinutes: 30,
	}
	clients := []string{"tw-a", "tw-b"}
	stamp := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }
	for seed := int64(1); seed <= 500; seed++ {
		rng := rand.New(rand.NewSource(seed))
		g, err := NewGate("joint", limits, event.TradingModeDryRun, 30*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		g.RecordDecision(tag)
		snapshot := func(sec int, held int64) event.BrokerSnapshotPayload {
			return event.BrokerSnapshotPayload{
				AccountID: "joint", AsOf: stamp(sec), MarketOpen: true, NetLiquidation: 10000, ExcessLiquidity: 6000,
				Positions: []event.BrokerPosition{{InstrumentID: "SPY", Quantity: held, Mark: 1, MarkSet: true}}, OpenOrders: []event.BrokerOrder{},
			}
		}
		accountAt := func(sec int, held int64) event.AccountPositionsSnapshotPayload {
			return event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: stamp(sec), NetLiquidation: 10000, Currency: "USD", Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: held}}}
		}
		if _, err := g.ObserveBroker("broker:0", snapshot(0, 100)); err != nil {
			t.Fatal(err)
		}
		g.ObserveAccount(accountAt(0, 100))
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
			outcomes, err := g.Check(Book{Holdings: map[string]int64{"SPY": 100}, Prices: map[string]float64{"SPY": 1}, At: stamp(0)}, []Order{o})
			if err != nil || outcomes[0].Released == nil {
				t.Fatalf("seed %d: setup release of %s: %v %+v", seed, c, err, outcomes)
			}
		}

		// The truth, second by second.
		filled := map[string]int64{}
		cancelled := map[string]bool{}
		var fills []*trueFill
		var facts []fact
		seq := 0
		for sec := 1; sec <= 8; sec++ {
			for n := rng.Intn(4); n > 0; n-- {
				seq++
				c := clients[rng.Intn(len(clients))]
				switch rng.Intn(5) {
				case 0, 1:
					if cancelled[c] || filled[c] == release {
						continue
					}
					f := &trueFill{client: c, quantity: 1 + rng.Int63n(release-filled[c]), sec: sec, seq: seq}
					filled[c] += f.quantity
					fills = append(fills, f)
					id := fmt.Sprintf("f%d", seq)
					facts = append(facts, fact{seq: seq, fill: f, record: func(g *Gate) error {
						return g.ObserveFill(event.TargetWeightFillPayload{
							AccountID: "joint", ClientOrderID: f.client, FillID: id, InstrumentID: "SPY",
							Side: event.OrderSideSell, Quantity: f.quantity, Price: 1, FilledAt: stamp(f.sec),
						})
					}})
				case 2:
					status := event.OrderLifecycleStatusCancelPending
					if !cancelled[c] && rng.Intn(2) == 0 {
						cancelled[c], status = true, event.OrderLifecycleStatusCanceled
					} else if cancelled[c] {
						continue
					}
					ends, s := "", sec
					if status == event.OrderLifecycleStatusCanceled {
						ends = c
					}
					facts = append(facts, fact{seq: seq, ends: ends, record: func(g *Gate) error {
						return g.ObserveLifecycle(event.LimitOrderLifecyclePayload{
							AccountID: "joint", ClientOrderID: c, OrderID: "ib-" + c, InstrumentID: "SPY", Side: event.OrderSideSell,
							Status: status, Quantity: release, LimitPrice: 1, OccurredAt: stamp(s),
						})
					}})
				default:
					held := 100 - filled["tw-a"] - filled["tw-b"]
					stream := "account"
					if rng.Intn(2) == 0 {
						stream = "broker"
					}
					taken := false
					for _, f := range facts {
						if f.stream != "" && f.asOf == sec {
							taken = true
						}
					}
					if taken {
						continue
					}
					if stream == "account" {
						p := accountAt(sec, held)
						facts = append(facts, fact{seq: seq, stream: stream, shows: true, asOf: sec, account: true, record: func(g *Gate) error {
							g.ObserveAccount(p)
							return nil
						}})
						continue
					}
					s := snapshot(sec, held)
					for _, l := range clients {
						if !cancelled[l] && filled[l] < release && rng.Intn(3) > 0 {
							s.OpenOrders = append(s.OpenOrders, event.BrokerOrder{BrokerOrderID: "ib-" + l, ClientOrderID: l, InstrumentID: "SPY", Side: event.OrderSideSell, Status: event.BrokerOrderWorking, RemainingQuantity: release - filled[l]})
						}
					}
					shows := true
					if rng.Intn(4) == 0 {
						s.Unreadable, s.OpenOrders, shows = []string{event.BrokerComponentOpenOrders}, nil, false
					}
					id := fmt.Sprintf("broker:%d", seq)
					facts = append(facts, fact{seq: seq, stream: stream, shows: shows, asOf: sec, record: func(g *Gate) error {
						_, err := g.ObserveBroker(id, s)
						return err
					}})
				}
			}
		}

		// The facts in random order, each snapshot stream in its own.
		order := rng.Perm(len(facts))
		recorded := make([]fact, len(facts))
		for i, j := range order {
			recorded[i] = facts[j]
		}
		for _, stream := range []string{"account", "broker"} {
			var at []int
			var in []fact
			for i, f := range recorded {
				if f.stream == stream {
					at, in = append(at, i), append(in, f)
				}
			}
			slices.SortFunc(in, func(a, b fact) int { return a.seq - b.seq })
			for k, i := range at {
				recorded[i] = in[k]
			}
		}

		accountSeq, shownSeq := 0, 0
		reported := map[*trueFill]bool{}
		ended := map[string]bool{}
		for step, f := range recorded {
			if err := f.record(g); err != nil {
				t.Fatalf("seed %d step %d: %v", seed, step, err)
			}
			switch {
			case f.fill != nil:
				reported[f.fill] = true
			case f.ends != "":
				ended[f.ends] = true
			case f.shows:
				shownSeq = max(shownSeq, f.seq)
			}
			if f.account {
				accountSeq = f.seq
			}
			var truth int64
			for _, c := range clients {
				if !cancelled[c] {
					truth += release - filled[c]
				}
			}
			for _, x := range fills {
				if x.seq > accountSeq && (!ended[x.client] || reported[x] || x.seq < shownSeq) {
					truth += x.quantity
				}
			}
			if got := g.PendingSells("SPY"); got < truth {
				t.Fatalf("seed %d step %d: pending sells %d, below the %d truly being sold that the account snapshot cannot show", seed, step, got, truth)
			}
		}
	}
}
