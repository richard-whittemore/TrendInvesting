package targetweight_test

import (
	"context"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

const iraAccount = "ira"

// momentumConfiguration is the momentum rule with short lookbacks (a 6-session
// score skipping 1, 4 returns of volatility, a 5-close trend), a top 2 with
// one name per sector, and a universe of 5, so a few weeks of invented
// closes rebalance twice. It is a test configuration, never the backtested one.
func momentumConfiguration() event.TargetWeightConfigurationPayload {
	return event.TargetWeightConfigurationPayload{
		StrategyID: event.TargetWeightStrategyMomentum, AccountID: iraAccount, Mode: event.TradingModeDryRun, Adapter: event.AdapterIBKR,
		Momentum: event.MomentumConfiguration{
			Rule: event.MomentumRuleTrend, ListingType: "CS", UniverseSize: 5, MinPrice: 5,
			ScoreLookback: 6, ScoreSkip: 1, VolatilityReturns: 4, TopN: 2, SectorCap: 1,
			InverseVolatility: true, Invested: 0.98, TrendInstrument: "SPY", TrendCloses: 5,
			RiskOffInstrument: "IEF", RiskOffFraction: 0.5, RebalanceBand: 0.25,
			UniverseSource: "fixture-universe", SectorSources: []string{"gics", "screener"},
		},
		Execution: event.ExecutionConfiguration{CashRule: event.CashRuleSettled, ShareQuantity: event.ShareQuantityWhole, MaxFeesPerOrder: 1},
		Limits:    event.MarginLimits{MaxExposure: 1, MaxOrdersPerDecision: 10, MaxOrderFraction: 0.9, PriceBand: 0.05},
	}
}

// momentumCalendar is every weekday from 2025-01-02 to 2025-03-04: the
// sessions of the synthetic run, and one after it.
func momentumCalendar() []string {
	var out []string
	for day := time.Date(2025, time.January, 2, 0, 0, 0, 0, time.UTC); day.Before(time.Date(2025, time.March, 5, 0, 0, 0, 0, time.UTC)); day = day.AddDate(0, 0, 1) {
		if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday {
			out = append(out, day.Format(time.DateOnly))
		}
	}
	return out
}

func closeOf(session string) time.Time {
	d, err := time.Parse(time.DateOnly, session)
	if err != nil {
		panic(err)
	}
	return d.Add(21 * time.Hour)
}

// name is one invented instrument: its listing type, sector label, volume,
// the first session it trades (and the session it stops, when to is set), and
// its growth per session in January and in February. A small irregular factor
// keeps every volatility above zero.
type name struct {
	id, listing, sector, source string
	volume                      float64
	from                        int
	base, jan, feb              float64
	to                          int
}

func names() []name {
	return []name{
		{"AAA", "CS", "Tech", "gics", 3e6, 0, 50, 1.02, 0.98, 0},
		{"BBB", "CS", "Tech", "gics", 2e6, 0, 40, 1.018, 1.03, 0},
		{"CCC", "CS", "Health", "screener", 1.5e6, 0, 30, 1.015, 1.015, 0},
		{"DDD", "CS", "Energy", "gics", 9e6, 0, 4, 1, 1, 0}, // under the $5 floor
		{"EEE", "ETF", "", "", 9e6, 0, 100, 1.04, 1.04, 0},  // not common stock
		{"FFF", "CS", "Energy", "gics", 1e5, 0, 20, 0.99, 0.99, 0},
		{"GGG", "CS", "Utilities", "gics", 4e6, 38, 60, 1.05, 1.05, 0}, // lists late in February
		{"HHH", "CS", "", "", 5e6, 0, 25, 1.06, 1.001, 0},              // no sector label
		{"III", "CS", "Materials", "gics", 1e4, 0, 10, 1, 1, 5},        // delisted after five sessions
		{"SPY", "ETF", "", "", 1e8, 0, 500, 1.002, 1.002, 0},
		{"IEF", "ETF", "", "", 1e7, 0, 95, 1.0005, 1.0005, 0},
	}
}

// price is n's close on session i: base x growth^i, by month, times an
// irregular factor; SPY falls 3% a session over February's last six sessions,
// so February ends risk-off.
func (n name) price(cal []string, i int) float64 {
	p := n.base
	for k := 1; k <= i; k++ {
		g := n.jan
		if cal[k] >= "2025-02-01" {
			g = n.feb
		}
		if n.id == "SPY" && cal[k] >= "2025-02-21" {
			g = 0.97
		}
		p = float64(p * g)
	}
	return math.Round(float64(p*(1+float64(0.002*float64((i*7)%5))))*100) / 100
}

// ira simulates the IRA at the venue: each released order is acknowledged and
// fills at the next session's open, at that session's close capped by its
// limit, with a commission of 0.5. A sale's proceeds settle a session later.
type ira struct {
	settled, unsettled float64
	held               map[string]int64
	pending            []event.OrderReleasedPayload
}

func (a *ira) snapshot(at time.Time, prices map[string]float64) event.AccountCashSnapshotPayload {
	net := float64(a.settled + a.unsettled)
	var positions []event.AccountPosition
	for _, id := range slices.Sorted(mapsKeys(a.held)) {
		if a.held[id] != 0 {
			positions = append(positions, event.AccountPosition{InstrumentID: id, Quantity: a.held[id]})
			net += float64(float64(a.held[id]) * prices[id])
		}
	}
	return event.AccountCashSnapshotPayload{
		AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{
			AccountID: iraAccount, AsOf: at, NetLiquidation: net, Currency: "USD", Positions: positions,
		},
		SettledCash: a.settled, TotalCash: float64(a.settled + a.unsettled),
	}
}

func mapsKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

type momentumOptions struct {
	mutate      func(*event.TargetWeightConfigurationPayload)
	partial     map[string]int64 // instrument -> shares of its sell filled before the rest is cancelled
	noSnapshot  map[string]bool  // sessions without an account snapshot
	noBar       map[string]string
	noMark      map[string]string // session -> instrument without a mark
	noUniverse  bool
	silentVenue bool   // the venue never reports on a released order
	decideFrom  string // first session decided, when not the first session
	stopAfter   string // last session fed
	beforeClose func(session string) []event.Envelope
}

type momentumResult struct {
	run
	venue  []event.Envelope // the venue reports fed
	stream *stream
}

// simulateMomentum feeds the synthetic run through a journal Recorder
// wrapping a fresh handler, the venue filling released orders as it goes.
func simulateMomentum(t *testing.T, opts momentumOptions) momentumResult {
	t.Helper()
	cfg := momentumConfiguration()
	if opts.mutate != nil {
		opts.mutate(&cfg)
	}
	cal := momentumCalendar()
	from := cal[0]
	if opts.decideFrom != "" {
		from = opts.decideFrom
	}
	handler, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: closeOf(from)})
	if err != nil {
		t.Fatal(err)
	}
	rec := journal.NewRecorder(handler)
	s := &stream{hash: event.TargetWeightConfigurationHash(cfg)}
	res := momentumResult{run: run{recorder: rec, hash: s.hash}, stream: s}
	acct := &ira{settled: 10000, held: map[string]int64{}}
	apply := func(in event.Envelope) []event.Envelope {
		t.Helper()
		out, err := rec.Apply(context.Background(), in)
		if err != nil {
			t.Fatalf("apply %s: %v", in.ID, err)
		}
		res.inputs = append(res.inputs, in)
		res.decisions = append(res.decisions, out...)
		for _, d := range out {
			if d.Type == event.OrderReleasedEventType {
				var p event.OrderReleasedPayload
				decode(t, d, &p)
				acct.pending = append(acct.pending, p)
			}
		}
		return out
	}
	start := closeOf(cal[0]).Add(-12 * time.Hour)
	apply(s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", start, cfg))
	apply(s.envelope(t, event.SessionCalendarEventType, event.SessionCalendarSchemaVersion, "calendar", start,
		event.SessionCalendarPayload{Source: "fixture", Sessions: cal}))

	for i, session := range cal[:len(cal)-1] {
		end := closeOf(session)
		prices := map[string]float64{}
		for _, n := range names() {
			if i >= n.from && (n.to == 0 || i < n.to) {
				prices[n.id] = n.price(cal, i)
			}
		}
		// The venue: yesterday's proceeds settle, then each released order is
		// acknowledged and fills at the open; the fill that ends the sell
		// phase may release buys, which fill in turn.
		acct.settled, acct.unsettled = float64(acct.settled+acct.unsettled), 0
		minute := 0
		venue := func(p any, eventType string, version uint32, id string) {
			minute++
			at := end.Add(time.Duration(-390+minute) * time.Minute)
			e := s.envelope(t, eventType, version, id, at, p)
			res.venue = append(res.venue, e)
			apply(e)
		}
		for len(acct.pending) > 0 && !opts.silentVenue {
			o := acct.pending[0]
			acct.pending = acct.pending[1:]
			at := end.Add(time.Duration(-390+minute+1) * time.Minute)
			venue(event.LimitOrderLifecyclePayload{
				AccountID: iraAccount, ClientOrderID: o.ClientOrderID, OrderID: "ib-" + o.ClientOrderID, InstrumentID: o.InstrumentID,
				Side: o.Side, Status: event.OrderLifecycleStatusSubmitted, Quantity: o.Quantity, LimitPrice: o.LimitPrice, OccurredAt: at,
			}, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "ack:"+o.ClientOrderID)
			price := prices[o.InstrumentID]
			if o.Side == event.OrderSideBuy {
				price = math.Min(price, o.LimitPrice)
			} else {
				price = math.Max(price, o.LimitPrice)
			}
			quantity := o.Quantity
			partial, cancels := opts.partial[o.InstrumentID]
			if cancels && o.Side == event.OrderSideSell {
				quantity = partial
			}
			if quantity > 0 {
				at = end.Add(time.Duration(-390+minute+1) * time.Minute)
				signed := quantity
				if o.Side == event.OrderSideSell {
					signed = -quantity
					acct.unsettled = float64(acct.unsettled+float64(float64(quantity)*price)) - 0.5
				} else {
					acct.settled = float64(acct.settled-float64(float64(quantity)*price)) - 0.5
				}
				acct.held[o.InstrumentID] += signed
				venue(event.TargetWeightFillPayload{
					AccountID: iraAccount, ClientOrderID: o.ClientOrderID, FillID: "fill:" + o.ClientOrderID, InstrumentID: o.InstrumentID,
					Side: o.Side, Quantity: quantity, Price: price, Commission: 0.5, FilledAt: at,
				}, event.FillEventType, event.FillTargetWeightVersion, "fill:"+o.ClientOrderID)
			}
			if quantity < o.Quantity {
				at = end.Add(time.Duration(-390+minute+1) * time.Minute)
				venue(event.LimitOrderLifecyclePayload{
					AccountID: iraAccount, ClientOrderID: o.ClientOrderID, OrderID: "ib-" + o.ClientOrderID, InstrumentID: o.InstrumentID,
					Side: o.Side, Status: event.OrderLifecycleStatusCanceled, Quantity: o.Quantity, LimitPrice: o.LimitPrice, OccurredAt: at,
				}, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "cancel:"+o.ClientOrderID)
			}
		}

		var ids []string
		for _, n := range names() {
			if _, ok := prices[n.id]; !ok || opts.noBar[session] == n.id {
				continue
			}
			ids = append(ids, n.id)
			apply(s.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, fmt.Sprintf("bar:%s:%s", n.id, session), end,
				adjustedBar(n.id, prices[n.id], n.volume, end)))
		}
		if !opts.noUniverse && cal[i+1][:7] != session[:7] {
			snap := event.UniverseSnapshotPayload{Session: session, Source: "fixture-universe"}
			for _, n := range names() {
				if p, ok := prices[n.id]; ok {
					snap.Rows = append(snap.Rows, event.UniverseRow{InstrumentID: n.id, Type: n.listing, Close: p, Volume: n.volume, Sector: n.sector, SectorSource: n.source})
				}
			}
			slices.SortFunc(snap.Rows, func(a, b event.UniverseRow) int {
				if a.InstrumentID < b.InstrumentID {
					return -1
				}
				return 1
			})
			apply(s.envelope(t, event.UniverseSnapshotEventType, event.UniverseSnapshotSchemaVersion, "universe:"+session, end, snap))
		}
		if !opts.noSnapshot[session] {
			apply(s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotCashVersion, "snapshot:"+session, end, acct.snapshot(end, prices)))
		}
		for _, id := range ids {
			if opts.noMark[session] == id {
				continue
			}
			apply(s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, fmt.Sprintf("mark:%s:%s", id, session), end,
				event.MarkRecordedPayload{InstrumentID: id, AsOf: end, Price: prices[id], Source: event.AdapterIBKR}))
		}
		slices.Sort(ids)
		if opts.beforeClose != nil {
			for _, e := range opts.beforeClose(session) {
				apply(e)
			}
		}
		apply(s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "session:"+session, end,
			event.SessionClosedPayload{PeriodEnd: end, InstrumentIDs: ids}))
		if session == opts.stopAfter {
			break
		}
	}
	res.entries = rec.Entries()
	return res
}

func released(t *testing.T, es []event.Envelope) []event.OrderReleasedPayload {
	t.Helper()
	var out []event.OrderReleasedPayload
	for _, e := range ofType(es, event.OrderReleasedEventType) {
		var p event.OrderReleasedPayload
		decode(t, e, &p)
		out = append(out, p)
	}
	return out
}
