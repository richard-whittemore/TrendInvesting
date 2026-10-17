package risklimit

import (
	"flag"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// pendingSeeds is how many seeds the ground-truth simulation runs: 500 in
// the suite; a local run passes -pending-seeds=20000.
var pendingSeeds = flag.Int("pending-seeds", 500, "seeds the ground-truth pending simulation runs")

// simSell is a sell in the simulated broker: one of ours, released by the
// gate (client set), or a hand sell the gate never released, which IBKR
// lists by its broker order ID alone and reports no fill or end of.
type simSell struct {
	instrument, client, brokerID string
	size, filled                 int64
	ended                        bool
}

func (s *simSell) live() bool { return !s.ended && s.filled < s.size }

// simFact is one fact the simulated broker produces, at its place seq in the
// true order: a fill or lifecycle report of ours, or a snapshot (stream
// "account" or "broker"). A snapshot's held is the holding it read, nil when
// it could not read the positions.
type simFact struct {
	seq    int
	stream string
	held   map[string]int64
	record func(g *Gate) (view, error)
}

// The pending rule is never below the truth (fail closed): for every view,
// the shares of an instrument already being sold that the view cannot show
// are never fewer than the shares truly sold after it was taken and still
// to be sold. A simulated broker holds 100 of each of two instruments, with
// up to two resting hand sells each that IBKR lists, and executes and
// cancels two sells of ours of 30 on each, second by second: partial fills,
// cancel-pending and cancel reports before and after the snapshots that show
// their fills, hand sells that fill or are withdrawn with no report, several
// account snapshots a second and one broker snapshot a second at most (as
// the gate requires), some of which cannot read the open orders or the
// positions. IBKR stamps each fact in whole seconds, so facts in one second
// keep only their true order. The facts are recorded in random order,
// account and broker snapshots each in their own order. After each, for
// every snapshot recorded that read the holding and for now, the gate's
// figure is checked against the truth: the view's holding less what will be
// left once every sell still working has filled, every fact of the run
// known. PendingSells is that figure for the latest account snapshot, and
// the broker-holding check's working figure that for the latest broker
// snapshot, when it read its positions and open orders.
func TestPendingIsNeverBelowTheTruth(t *testing.T) {
	base := time.Date(2025, time.March, 5, 16, 0, 0, 0, time.UTC)
	const tag = "targets-decided:joint:2025-03-04"
	const release = 30
	instruments := []string{"QQQ", "SPY"}
	limits := event.MarginLimits{
		MaxExposure: 1.5, MaxOrdersPerDecision: 1, MaxOrderFraction: 0.5, PriceBand: 0.05,
		ExcessLiquidityWarning: 0.4, ExcessLiquidityUrgent: 0.2, ExcessLiquidityStaleMinutes: 30,
	}
	stamp := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }
	statuses := []string{event.OrderLifecycleStatusCancelPending, event.OrderLifecycleStatusCanceled, event.OrderLifecycleStatusInvalid}
	for seed := int64(1); seed <= int64(*pendingSeeds); seed++ {
		rng := rand.New(rand.NewSource(seed))
		g, err := NewGate("joint", limits, event.TradingModeDryRun, 30*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		g.RecordDecision(tag)

		// The broker's true state, as it moves.
		var ours, hand []*simSell
		for _, instrument := range instruments {
			var rest int64 = 20
			for n := rng.Intn(3); n > 0 && rest > 0; n-- {
				size := 1 + rng.Int63n(rest)
				rest -= size
				hand = append(hand, &simSell{instrument: instrument, brokerID: fmt.Sprintf("hand-%s-%d", instrument, len(hand)), size: size})
			}
			for _, c := range []string{"a", "b"} {
				ours = append(ours, &simSell{instrument: instrument, client: "tw-" + strings.ToLower(instrument) + "-" + c, brokerID: "ib-" + instrument + "-" + c, size: release})
			}
		}
		holding := func() map[string]int64 {
			held := map[string]int64{}
			for _, instrument := range instruments {
				held[instrument] = 100
			}
			for _, s := range append(slices.Clone(ours), hand...) {
				held[s.instrument] -= s.filled
			}
			return held
		}
		positions := func(held map[string]int64) []event.BrokerPosition {
			var out []event.BrokerPosition
			for _, instrument := range instruments {
				if held[instrument] != 0 {
					out = append(out, event.BrokerPosition{InstrumentID: instrument, Quantity: held[instrument], Mark: 1, MarkSet: true})
				}
			}
			return out
		}
		accountAt := func(sec int, held map[string]int64) event.AccountPositionsSnapshotPayload {
			p := event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: stamp(sec), NetLiquidation: 10000, Currency: "USD"}
			for _, position := range positions(held) {
				p.Positions = append(p.Positions, event.AccountPosition{InstrumentID: position.InstrumentID, Quantity: position.Quantity})
			}
			return p
		}
		// brokerAt is a broker snapshot of the truth at sec: every live hand
		// sell, and each live sell of ours with probability listOurs.
		brokerAt := func(sec int, held map[string]int64, listOurs int) event.BrokerSnapshotPayload {
			s := event.BrokerSnapshotPayload{
				AccountID: "joint", AsOf: stamp(sec), MarketOpen: true, NetLiquidation: 10000, ExcessLiquidity: 6000,
				Positions: positions(held), OpenOrders: []event.BrokerOrder{},
			}
			for _, h := range hand {
				if h.live() {
					s.OpenOrders = append(s.OpenOrders, event.BrokerOrder{BrokerOrderID: h.brokerID, InstrumentID: h.instrument, Side: event.OrderSideSell, Status: event.BrokerOrderWorking, RemainingQuantity: h.size - h.filled})
				}
			}
			for _, o := range ours {
				if o.live() && rng.Intn(3) < listOurs {
					s.OpenOrders = append(s.OpenOrders, event.BrokerOrder{BrokerOrderID: o.brokerID, ClientOrderID: o.client, InstrumentID: o.instrument, Side: event.OrderSideSell, Status: event.BrokerOrderWorking, RemainingQuantity: o.size - o.filled})
				}
			}
			slices.SortFunc(s.OpenOrders, func(a, b event.BrokerOrder) int { return strings.Compare(a.BrokerOrderID, b.BrokerOrderID) })
			return s
		}

		// A view and the holding it read.
		type seen struct {
			v    view
			held map[string]int64
		}
		var views []seen
		setup := holding()
		if _, err := g.ObserveBroker("broker:0", brokerAt(0, setup, 0)); err != nil {
			t.Fatal(err)
		}
		views = append(views, seen{g.brokerView, setup})
		g.ObserveAccount(accountAt(0, setup))
		views = append(views, seen{g.account, setup})
		accountHeld, brokerHeld := setup, setup
		brokerReadable := true
		for _, o := range ours {
			plan := Order{
				PlanID: "order-planned:" + o.client,
				Planned: event.OrderPlannedPayload{
					AccountID: "joint", Tag: tag, ClientOrderID: o.client, InstrumentID: o.instrument, Side: event.OrderSideSell, Phase: event.OrderSideSell,
					OrderType: event.OrderTypeLimit, Quantity: release, LimitPrice: 1, TargetWeight: 1,
					NetLiquidation: 10000, HeldQuantity: 100, ReferencePrice: 1, PriceBand: 0.05,
				},
				Mark: 1, MarkSet: true,
			}
			outcomes, err := g.Check(Book{Holdings: map[string]int64{o.instrument: 100}, Prices: map[string]float64{o.instrument: 1}, At: stamp(0)}, []Order{plan})
			if err != nil || outcomes[0].Released == nil {
				t.Fatalf("seed %d: setup release of %s: %v %+v", seed, o.client, err, outcomes)
			}
		}

		// The truth, second by second.
		var facts []simFact
		seq := 0
		brokerSeconds := map[int]bool{}
		for sec := 1; sec <= 8; sec++ {
			for n := rng.Intn(5); n > 0; n-- {
				seq++
				o := ours[rng.Intn(len(ours))]
				switch rng.Intn(6) {
				case 0, 1:
					if !o.live() {
						continue
					}
					q := 1 + rng.Int63n(o.size-o.filled)
					o.filled += q
					p := event.TargetWeightFillPayload{
						AccountID: "joint", ClientOrderID: o.client, FillID: fmt.Sprintf("f%d", seq), InstrumentID: o.instrument,
						Side: event.OrderSideSell, Quantity: q, Price: 1, FilledAt: stamp(sec),
					}
					facts = append(facts, simFact{seq: seq, record: func(g *Gate) (view, error) { return view{}, g.ObserveFill(p) }})
				case 2:
					if o.ended {
						continue
					}
					p := event.LimitOrderLifecyclePayload{
						AccountID: "joint", ClientOrderID: o.client, OrderID: o.brokerID, InstrumentID: o.instrument, Side: event.OrderSideSell,
						Status: statuses[rng.Intn(len(statuses))], Quantity: release, LimitPrice: 1, OccurredAt: stamp(sec),
					}
					o.ended = p.Terminal()
					facts = append(facts, simFact{seq: seq, record: func(g *Gate) (view, error) { return view{}, g.ObserveLifecycle(p) }})
				case 3:
					if len(hand) == 0 {
						continue
					}
					h := hand[rng.Intn(len(hand))]
					switch {
					case !h.live():
					case rng.Intn(4) == 0:
						h.ended = true
					default:
						h.filled += 1 + rng.Int63n(h.size-h.filled)
					}
				default:
					held := holding()
					if rng.Intn(2) == 0 {
						p := accountAt(sec, held)
						facts = append(facts, simFact{seq: seq, stream: "account", held: held, record: func(g *Gate) (view, error) {
							g.ObserveAccount(p)
							return g.account, nil
						}})
						continue
					}
					if brokerSeconds[sec] {
						continue
					}
					brokerSeconds[sec] = true
					s := brokerAt(sec, held, 2)
					if rng.Intn(4) == 0 {
						s.Unreadable, s.OpenOrders = []string{event.BrokerComponentOpenOrders}, nil
					}
					if rng.Intn(6) == 0 {
						s.Unreadable, s.Positions, held = append(s.Unreadable, event.BrokerComponentPositions), nil, nil
					}
					id := fmt.Sprintf("broker:%d", seq)
					facts = append(facts, simFact{seq: seq, stream: "broker", held: held, record: func(g *Gate) (view, error) {
						_, err := g.ObserveBroker(id, s)
						return g.brokerView, err
					}})
				}
			}
		}

		// What will be left of each instrument once every sell still working
		// has filled, and what is still working: the truth every figure is
		// measured against.
		left, working := holding(), map[string]int64{}
		for _, s := range append(slices.Clone(ours), hand...) {
			if s.live() {
				left[s.instrument] -= s.size - s.filled
				working[s.instrument] += s.size - s.filled
			}
		}

		// The facts in random order, each snapshot stream in its own.
		order := rng.Perm(len(facts))
		recorded := make([]simFact, len(facts))
		for i, j := range order {
			recorded[i] = facts[j]
		}
		for _, stream := range []string{"account", "broker"} {
			var at []int
			var in []simFact
			for i, f := range recorded {
				if f.stream == stream {
					at, in = append(at, i), append(in, f)
				}
			}
			slices.SortFunc(in, func(a, b simFact) int { return a.seq - b.seq })
			for k, i := range at {
				recorded[i] = in[k]
			}
		}

		for step, f := range recorded {
			v, err := f.record(g)
			if err != nil {
				t.Fatalf("seed %d step %d: %v", seed, step, err)
			}
			switch f.stream {
			case "account":
				accountHeld = f.held
			case "broker":
				brokerHeld, brokerReadable = f.held, !slices.Contains(g.broker.Unreadable, event.BrokerComponentOpenOrders)
			}
			if f.held != nil {
				views = append(views, seen{v, f.held})
			}
			for _, instrument := range instruments {
				// remains stops at the latest reference at which nothing
				// released before it can still sell; that changes nothing.
				var every int64
				for _, p := range g.ledger.refs {
					if toGo, _, ok := g.ledger.toGo(instrument, p.view); ok {
						every = max(every, p.held[instrument]-toGo)
					}
				}
				if got := g.ledger.remains(instrument); got != every {
					t.Fatalf("seed %d step %d: %s: remains %d, but %d over every reference", seed, step, instrument, got, every)
				}
				for _, s := range views {
					if got, truth := g.ledger.unshown(instrument, s.v), s.held[instrument]-left[instrument]; got < truth {
						t.Fatalf("seed %d step %d: %s: the snapshot %d of %s is netted %d being sold, below the %d truly sold since or still to sell", seed, step, instrument, s.v.by, s.v.at.Format(time.TimeOnly), got, truth)
					}
				}
				if got := g.ledger.unshown(instrument, now); got < working[instrument] {
					t.Fatalf("seed %d step %d: %s: %d counted still to sell, below the %d truly working", seed, step, instrument, got, working[instrument])
				}
				if got, truth := g.PendingSells(instrument), accountHeld[instrument]-left[instrument]; got < truth {
					t.Fatalf("seed %d step %d: %s: pending sells %d, below the %d truly being sold that the account snapshot cannot show", seed, step, instrument, got, truth)
				}
				h, w, ok := g.brokerHolding(instrument)
				if ok != (brokerHeld != nil && brokerReadable) {
					t.Fatalf("seed %d step %d: broker holding read %v", seed, step, ok)
				}
				if ok && (h != brokerHeld[instrument] || w < brokerHeld[instrument]-left[instrument]) {
					t.Fatalf("seed %d step %d: %s: broker holding %d net of %d working, below the %d truly being sold that the broker snapshot cannot show", seed, step, instrument, h, w, brokerHeld[instrument]-left[instrument])
				}
			}
		}
	}
}
