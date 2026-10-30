package risklimit

import (
	"flag"
	"fmt"
	"math"
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
// lists by its broker order ID alone and reports no fill or end of. A hand
// sell is seen once a broker snapshot that read the open orders has listed
// it since it was placed or last raised, and filledSince is set by a fill
// after its latest listing.
type simSell struct {
	instrument, client, brokerID string
	size, filled                 int64
	ended, seen, filledSince     bool
	// hidden are the shares IBKR has not yet listed: its size when placed,
	// or what a raise added.
	hidden []*simHidden
	// fills are its fills, each at its true place.
	fills []simFill
}

// simFill is shares of a sell filled at the true place seq.
type simFill struct {
	seq      int
	quantity int64
}

// soldAfter is the shares of the sell h sold after the true place seq or
// still to sell: its part of the truth for a view taken there.
func (h *simSell) soldAfter(seq int) int64 {
	var n int64
	for _, f := range h.fills {
		if f.seq > seq {
			n += f.quantity
		}
	}
	if h.live() {
		n += h.size - h.filled
	}
	return n
}

// simHidden is shares of a hand sell no broker snapshot that read the open
// orders has yet listed: the gate cannot count them until one it records
// does (the documented limitation). by is the true place of the snapshot
// that first lists them, 0 while none has; quantity is 0 once the hand sell
// is withdrawn unseen, having sold none of them.
type simHidden struct {
	instrument string
	quantity   int64
	by         int
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
// to be sold, less, of each hand sell's part of that truth, no more than
// its shares no broker snapshot recorded has yet listed, which the gate
// cannot know (the documented limitation). The program's own sells are also
// checked alone, against their own part of the truth with no allowance, so
// no shortfall of them hides behind an unlisted hand sell. A
// simulated broker holds 100 of each of two instruments, with up to two
// resting hand sells each that IBKR lists, and executes and cancels two
// sells of ours of 30 on each, second by second: partial fills,
// cancel-pending and cancel reports before and after the snapshots that show
// their fills; hand sells placed at random seconds, which only later
// snapshots list, raised in size, filled or withdrawn with no report, never
// filled before IBKR lists them and never raised after a fill IBKR has not
// yet listed (so a listing shows each raise); several account snapshots a
// second and one broker snapshot a second at most (as the gate requires),
// some of which cannot read the open orders or the positions. One more hand
// sell is placed after the last snapshot, and in some runs fills before any
// snapshot lists it. IBKR stamps each fact in whole seconds, so facts in one second
// keep only their true order. The facts are recorded in random order,
// account and broker snapshots each in their own order. After each, for
// every snapshot recorded that read the holding and for now, the gate's
// figure is checked against the truth: the view's holding less what will be
// left once every sell still working has filled, every fact of the run
// known. PendingSells is that figure for the latest account snapshot, and
// the broker-holding check's working figure that for the latest broker
// snapshot, when it read its positions and open orders. Once every fact is
// recorded, a last broker snapshot lists every sell still live: it shows
// the late hand sell when it is still working, and every figure is then
// checked against the whole truth; the late hand sell that filled before
// any listing is the documented limitation: every figure is checked to be
// short of the truth by no more than its size, and what is counted still to
// sell, now and at each reference taken after that fill, not at all.
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
	limitation := 0
	for seed := int64(1); seed <= int64(*pendingSeeds); seed++ {
		rng := rand.New(rand.NewSource(seed))
		g, err := NewGate("joint", limits, event.TradingModeDryRun, 30*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		g.RecordDecision(tag)

		// The broker's true state, as it moves.
		// Each instrument's 100 cover our 60 and, at most, 40 sold by hand:
		// 30 resting, placed or raised before the last snapshot, and 10 kept
		// for the hand sell placed after it.
		var ours, hand []*simSell
		var hidden []*simHidden
		room := map[string]int64{}
		for _, instrument := range instruments {
			var rest int64 = 20
			for n := rng.Intn(3); n > 0 && rest > 0; n-- {
				size := 1 + rng.Int63n(rest)
				rest -= size
				hand = append(hand, &simSell{instrument: instrument, brokerID: fmt.Sprintf("hand-%s-%d", instrument, len(hand)), size: size, seen: true})
			}
			room[instrument] = 10 + rest
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
		if err := g.ObserveAccount(accountAt(0, setup)); err != nil {
			t.Fatal(err)
		}
		views = append(views, seen{g.account, setup})
		// placeOf is each snapshot's true place, by its identity in the gate.
		placeOf := map[int]int{g.brokerView.by: 0, g.account.by: 0}
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
				instrument := instruments[rng.Intn(len(instruments))]
				switch rng.Intn(8) {
				case 0, 1:
					if !o.live() {
						continue
					}
					q := 1 + rng.Int63n(o.size-o.filled)
					o.filled += q
					o.fills = append(o.fills, simFill{seq, q})
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
						for _, x := range h.hidden {
							if x.by == 0 {
								x.quantity = 0
							}
						}
					case h.seen:
						q := 1 + rng.Int63n(h.size-h.filled)
						h.filled += q
						h.fills = append(h.fills, simFill{seq, q})
						h.filledSince = true
					}
				case 4:
					// The owner places a hand sell, which only later
					// snapshots list.
					if room[instrument] == 0 {
						continue
					}
					size := 1 + rng.Int63n(min(room[instrument], 15))
					room[instrument] -= size
					h := &simSell{instrument: instrument, brokerID: fmt.Sprintf("hand-%s-%d", instrument, len(hand)), size: size}
					h.hidden = append(h.hidden, &simHidden{instrument: instrument, quantity: size})
					hidden = append(hidden, h.hidden[0])
					hand = append(hand, h)
				case 5:
					// The owner raises a live hand sell's size in TWS; it
					// keeps its order ID.
					if len(hand) == 0 {
						continue
					}
					h := hand[rng.Intn(len(hand))]
					if !h.live() || !h.seen || h.filledSince || room[h.instrument] == 0 {
						continue
					}
					raise := 1 + rng.Int63n(min(room[h.instrument], 10))
					room[h.instrument] -= raise
					h.size += raise
					h.seen = false
					x := &simHidden{instrument: h.instrument, quantity: raise}
					h.hidden = append(h.hidden, x)
					hidden = append(hidden, x)
				default:
					held := holding()
					if rng.Intn(2) == 0 {
						p := accountAt(sec, held)
						facts = append(facts, simFact{seq: seq, stream: "account", held: held, record: func(g *Gate) (view, error) {
							if err := g.ObserveAccount(p); err != nil {
								t.Fatal(err)
							}
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
					if s.OpenOrders != nil {
						seeHand(hand, seq)
					}
					facts = append(facts, brokerFact(seq, held, s))
				}
			}
		}

		// The hand sell placed after the last snapshot, which fills before
		// any lists it in about half the runs, an account snapshot showing
		// the fall.
		late := &simSell{instrument: instruments[rng.Intn(len(instruments))], brokerID: "hand-late", size: 1 + rng.Int63n(10)}
		late.hidden = append(late.hidden, &simHidden{instrument: late.instrument, quantity: late.size})
		hidden = append(hidden, late.hidden[0])
		hand = append(hand, late)
		if rng.Intn(2) == 0 {
			seq++
			late.filled = late.size
			late.fills = append(late.fills, simFill{seq, late.size})
			held, p := holding(), accountAt(9, holding())
			facts = append(facts, simFact{seq: seq, stream: "account", held: held, record: func(g *Gate) (view, error) {
				if err := g.ObserveAccount(p); err != nil {
					t.Fatal(err)
				}
				return g.account, nil
			}})
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

		// unlisted is, by instrument, the hand-sold shares no broker snapshot
		// recorded has listed.
		recordedBroker := map[int]bool{}
		unlisted := func() map[string]int64 {
			out := map[string]int64{}
			for _, x := range hidden {
				if x.by == 0 || !recordedBroker[x.by] {
					out[x.instrument] += x.quantity
				}
			}
			return out
		}
		// slack is, for a view taken at the true place seq, the most what
		// it counts still to sell of instrument may fall short of the
		// truth: of each hand sell's part of that truth, no more than its
		// shares no broker snapshot recorded has listed. Our own sells and
		// listed hand-sold shares give none.
		slack := func(instrument string, seq int) int64 {
			var out int64
			for _, h := range hand {
				if h.instrument != instrument {
					continue
				}
				var unseen int64
				for _, x := range h.hidden {
					if x.by == 0 || !recordedBroker[x.by] {
						unseen += x.quantity
					}
				}
				out += min(unseen, h.soldAfter(seq))
			}
			return out
		}
		short := false
		checkAll := func(step int) {
			for _, instrument := range instruments {
				// Each reference's toGo meets its truth less its own slack
				// alone, and the part of it for our own sells, with no unknown
				// sell recorded, meets our sells' part of the truth exactly,
				// so an under-count of our own sells there is caught. A
				// snapshot's figure nets the most over the references, so it
				// may fall short by the most any reference's slack is.
				ourOwn := func(seq int) int64 {
					var n int64
					for _, o := range ours {
						if o.instrument == instrument {
							n += o.soldAfter(seq)
						}
					}
					return n
				}
				bare := g.ledger
				bare.unknown = nil
				if got, _, _ := bare.toGo(instrument, now); got < ourOwn(math.MaxInt) {
					t.Fatalf("seed %d step %d: %s: %d of our own sells counted still to sell, below the %d truly working", seed, step, instrument, got, ourOwn(math.MaxInt))
				}
				var every, less int64
				for _, p := range g.ledger.refs {
					if got, _, _ := bare.toGo(instrument, p.view); got < ourOwn(placeOf[p.by]) {
						t.Fatalf("seed %d step %d: %s: the reference %d of %s counts %d of our own sells still to sell, below the %d they truly sold since or still sell", seed, step, instrument, p.by, p.at.Format(time.TimeOnly), got, ourOwn(placeOf[p.by]))
					}
					toGo, _, ok := g.ledger.toGo(instrument, p.view)
					if !ok {
						continue
					}
					every = max(every, p.held[instrument]-toGo)
					truth, own := p.held[instrument]-left[instrument], slack(instrument, placeOf[p.by])
					if toGo < truth-own {
						t.Fatalf("seed %d step %d: %s: the reference %d of %s counts %d still to sell, below the %d truly sold since or still to sell less %d unlisted", seed, step, instrument, p.by, p.at.Format(time.TimeOnly), toGo, truth, own)
					}
					less = max(less, own)
				}
				// remains stops at the latest reference at which nothing
				// released before it can still sell: that changes nothing
				// while every hand-sold share is listed, and only lowers
				// remains while some are not.
				if got, unseen := g.ledger.remains(instrument), unlisted()[instrument]; got > every || got != every && unseen == 0 {
					t.Fatalf("seed %d step %d: %s: remains %d, but %d over every reference, %d unlisted", seed, step, instrument, got, every, unseen)
				}
				for _, s := range views {
					got, truth := g.ledger.unshown(instrument, s.v), s.held[instrument]-left[instrument]
					if got < truth-less {
						t.Fatalf("seed %d step %d: %s: the snapshot %d of %s is netted %d being sold, below the %d truly sold since or still to sell less %d unlisted", seed, step, instrument, s.v.by, s.v.at.Format(time.TimeOnly), got, truth, less)
					}
					short = short || got < truth
				}
				if got, less := g.ledger.unshown(instrument, now), slack(instrument, math.MaxInt); got < working[instrument]-less {
					t.Fatalf("seed %d step %d: %s: %d counted still to sell, below the %d truly working less %d unlisted", seed, step, instrument, got, working[instrument], less)
				}
				if got, truth := g.PendingSells(instrument), accountHeld[instrument]-left[instrument]; got < truth-less {
					t.Fatalf("seed %d step %d: %s: pending sells %d, below the %d truly being sold that the account snapshot cannot show less %d unlisted", seed, step, instrument, got, truth, less)
				}
				h, w, ok := g.brokerHolding(instrument)
				if ok != (brokerHeld != nil && brokerReadable) {
					t.Fatalf("seed %d step %d: broker holding read %v", seed, step, ok)
				}
				if ok && (h != brokerHeld[instrument] || w < brokerHeld[instrument]-left[instrument]-less) {
					t.Fatalf("seed %d step %d: %s: broker holding %d net of %d working, below the %d truly being sold that the broker snapshot cannot show less %d unlisted", seed, step, instrument, h, w, brokerHeld[instrument]-left[instrument], less)
				}
			}
		}
		record := func(step int, f simFact) {
			v, err := f.record(g)
			if err != nil {
				t.Fatalf("seed %d step %d: %v", seed, step, err)
			}
			switch f.stream {
			case "account":
				accountHeld = f.held
			case "broker":
				recordedBroker[f.seq] = true
				brokerHeld, brokerReadable = f.held, !slices.Contains(g.broker.Unreadable, event.BrokerComponentOpenOrders)
			}
			if f.held != nil {
				views = append(views, seen{v, f.held})
				placeOf[v.by] = f.seq
			}
		}
		for step, f := range recorded {
			record(step, f)
			checkAll(step)
		}

		// The documented limitation, checked alone: a last broker snapshot
		// lists every live sell. A late hand sell still working is then
		// counted, and every figure meets the whole truth; one that filled
		// before any listing never is, and every figure falls short of the
		// truth by no more than its size; what is counted still to sell, now
		// and at each reference taken after that fill, not at all.
		seq++
		s := brokerAt(10, holding(), 3)
		seeHand(hand, seq)
		record(len(recorded), brokerFact(seq, holding(), s))
		short = false
		checkAll(len(recorded))
		if !late.live() && short {
			limitation++
		}
	}
	t.Logf("a late hand sell that filled before any listing left a figure below the truth in %d of %d runs", limitation, *pendingSeeds)
}

// seeHand marks every live hand sell seen by the broker snapshot at true
// place seq, which read the open orders, and the shares it first lists.
func seeHand(hand []*simSell, seq int) {
	for _, h := range hand {
		if !h.live() {
			continue
		}
		h.seen, h.filledSince = true, false
		for _, x := range h.hidden {
			if x.by == 0 {
				x.by = seq
			}
		}
	}
}

// brokerFact is the broker snapshot s at true place seq, which read held.
func brokerFact(seq int, held map[string]int64, s event.BrokerSnapshotPayload) simFact {
	id := fmt.Sprintf("broker:%d", seq)
	return simFact{seq: seq, stream: "broker", held: held, record: func(g *Gate) (view, error) {
		_, err := g.ObserveBroker(id, s)
		return g.brokerView, err
	}}
}
