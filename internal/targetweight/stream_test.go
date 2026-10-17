package targetweight_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

const strategyVersion = "boost/1.0.0+test"

// testConfiguration is Boost with short indicators and RSI_ENTRY 30, so a
// synthetic series of a few dozen sessions enters and exits. It is a test
// configuration, never the backtested one.
func testConfiguration() event.TargetWeightConfigurationPayload {
	return event.TargetWeightConfigurationPayload{
		StrategyID: event.TargetWeightStrategyBoost, AccountID: "joint", Mode: event.TradingModeDryRun, Adapter: event.AdapterIBKR,
		Boost: event.BoostConfiguration{
			InstrumentID: "SPY", InvestedFraction: 0.98, BoostSize: 1.5, RSIPeriod: 2, RSIEntry: 30,
			TrendDays: 10, ExitSMA: 3, WarmUpStart: "2025-01-02",
		},
		Limits: event.MarginLimits{
			MaxExposure: 1.5, MaxOrdersPerDecision: 1, MaxOrderFraction: 1, PriceBand: 0.05,
			ExcessLiquidityWarning: 0.4, ExcessLiquidityUrgent: 0.2, ExcessLiquidityStaleMinutes: 30,
		},
		Execution: event.ExecutionConfiguration{MaxSnapshotAge: 30},
	}
}

// syntheticCloses is an invented series: a steady rise, a pullback that
// enters at session 16 (RSI 28.6 above the 10-session mean), a close below
// the trend mean while latched (18), an exit at 20, and a later dip that the
// trend mean blocks (23, 24).
func syntheticCloses() []float64 {
	closes := make([]float64, 0, 28)
	for i := range 15 {
		closes = append(closes, float64(100+4*i))
	}
	return append(closes, 150, 148, 147, 146.5, 146.4, 150, 152, 151, 148, 147, 146.8, 152, 155)
}

// sessionEnd returns the n-th weekday session from 2025-01-02, closing 21:00 UTC.
func sessionEnd(n int) time.Time {
	day := time.Date(2025, time.January, 2, 21, 0, 0, 0, time.UTC)
	for i := 0; i < n; {
		day = day.AddDate(0, 0, 1)
		if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday {
			i++
		}
	}
	return day
}

// stream builds input envelopes stamped with the run's identity.
type stream struct {
	hash     string
	sequence uint64
}

func (s *stream) envelope(t *testing.T, eventType string, version uint32, id string, at time.Time, payload any) event.Envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	s.sequence++
	return event.Envelope{
		ID: id, Type: eventType, EnvelopeVersion: event.CurrentEnvelopeVersion, SchemaVersion: version,
		EventTime: at, RecordedAt: at, Sequence: s.sequence, Source: "fixture",
		StrategyVersion: strategyVersion, ConfigurationHash: s.hash, PayloadHash: event.HashPayload(raw), Payload: raw,
	}
}

// bar is SPY's version 2 bar at price in the adjusted and raw views. Its
// split-adjusted view is a flat decoy, so a handler that read it instead of
// the adjusted view would see no signal at all.
func bar(price float64, at time.Time) event.CompletedBarAdjustedPayload {
	return adjustedBar("SPY", price, 1e6, at)
}

// adjustedBar is a version 2 bar based at the first synthetic session.
func adjustedBar(id string, price, volume float64, at time.Time) event.CompletedBarAdjustedPayload {
	view := func(label string, p float64) event.PriceView {
		return event.PriceView{View: label, Open: p, High: p, Low: p, Close: p, Volume: volume}
	}
	return event.CompletedBarAdjustedPayload{
		CompletedBarPayload: event.CompletedBarPayload{InstrumentID: id, PeriodEnd: at, SplitAdjusted: view(event.ViewSplitAdjusted, 77), Raw: view(event.ViewRaw, price)},
		Adjusted:            view(event.ViewAdjusted, price),
		AdjustedBase:        "2024-12-31",
	}
}

// account simulates the joint account: a released order is acknowledged and
// fills at the next session's close and the venue reports the execution, cash pays for it, and net liquidation is cash plus the
// holding at the close.
type account struct {
	cash    float64
	held    int64
	pending []event.OrderReleasedPayload
}

// take queues every order released among out to fill at the next close.
func (a *account) take(t *testing.T, out []event.Envelope) {
	t.Helper()
	for _, d := range out {
		if d.Type == event.OrderReleasedEventType {
			var p event.OrderReleasedPayload
			decode(t, d, &p)
			a.pending = append(a.pending, p)
		}
	}
}

// fill fills every queued order at price and returns the venue's report of
// each execution, filled at at.
func (a *account) fill(price float64, at time.Time) []event.TargetWeightFillPayload {
	var reports []event.TargetWeightFillPayload
	for _, o := range a.pending {
		quantity := o.Quantity
		if o.Side == event.OrderSideSell {
			quantity = -quantity
		}
		a.held += quantity
		a.cash -= float64(float64(quantity) * price)
		reports = append(reports, event.TargetWeightFillPayload{
			AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, FillID: "fill:" + o.ClientOrderID, InstrumentID: o.InstrumentID,
			Side: o.Side, Quantity: o.Quantity, Price: price, FilledAt: at,
		})
	}
	a.pending = nil
	return reports
}

// broker is IBKR's view of the simulated account at the close: the holding
// at its mark, net liquidation, and Excess Liquidity under a synthetic 25%
// maintenance requirement, with the market open.
func (a *account) broker(at time.Time, price float64) event.BrokerSnapshotPayload {
	value := float64(float64(a.held) * price)
	netLiq := a.cash + value
	s := event.BrokerSnapshotPayload{
		AccountID: "joint", AsOf: at, MarketOpen: true,
		NetLiquidation: netLiq, ExcessLiquidity: netLiq - float64(0.25*value),
		Positions:  []event.BrokerPosition{},
		OpenOrders: []event.BrokerOrder{},
	}
	if a.held != 0 {
		s.Positions = []event.BrokerPosition{{InstrumentID: "SPY", Quantity: a.held, Mark: price, MarkSet: true}}
	}
	return s
}

func (a *account) snapshot(at time.Time, price float64) event.AccountPositionsSnapshotPayload {
	s := event.AccountPositionsSnapshotPayload{
		AccountID: "joint", AsOf: at, Currency: "USD",
		NetLiquidation: a.cash + float64(float64(a.held)*price),
	}
	if a.held != 0 {
		s.Positions = []event.AccountPosition{{InstrumentID: "SPY", Quantity: a.held}}
	}
	return s
}

// options shape a run of the synthetic series.
type options struct {
	closes   []float64
	skip     map[int]bool // sessions whose bar is missing
	noSnap   map[int]bool // sessions with no account snapshot
	noMark   map[int]bool // sessions with no mark
	recovery *event.Envelope
	// recorded are an earlier run's journalled records, fed after the
	// configuration and before the recovered decision.
	recorded []event.Envelope
	// snapshots, when set, supplies each session's account snapshot from an
	// earlier run's inputs, so a restarted run sees the account the original
	// run's orders left.
	snapshots []event.Envelope
	from      int // first session fed
	mutate    func(*event.TargetWeightConfigurationPayload)
	// prelude is an earlier run's recorded strategy.engine.state records, fed
	// after the configuration (and the recovered decision) and before any bar.
	prelude []event.Envelope
	// brokers feeds an account.broker.snapshot each session, after the
	// account snapshot; brokerMutate changes session i's. With snapshots set,
	// the earlier run's broker snapshots are fed instead.
	brokers      bool
	brokerMutate map[int]func(*event.BrokerSnapshotPayload)
	// kill engages (true) or releases (false) the kill switch at the start
	// of session i.
	kill map[int]bool
}

type run struct {
	inputs    []event.Envelope
	decisions []event.Envelope
	entries   []journal.Entry
	recorder  *journal.Recorder
	hash      string
}

// simulate feeds the synthetic series session by session through a journal
// Recorder wrapping a fresh handler, filling released orders as it goes.
func simulate(t *testing.T, opts options) run {
	t.Helper()
	cfg := testConfiguration()
	if opts.mutate != nil {
		opts.mutate(&cfg)
	}
	if opts.closes == nil {
		opts.closes = syntheticCloses()
	}
	handler, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: sessionEnd(0)})
	if err != nil {
		t.Fatal(err)
	}
	rec := journal.NewRecorder(handler)
	s := &stream{hash: event.TargetWeightConfigurationHash(cfg)}
	r := run{recorder: rec, hash: s.hash}
	acct := &account{cash: 4000}
	apply := func(in event.Envelope) []event.Envelope {
		t.Helper()
		out, err := rec.Apply(context.Background(), in)
		if err != nil {
			t.Fatalf("apply %s: %v", in.ID, err)
		}
		r.inputs = append(r.inputs, in)
		r.decisions = append(r.decisions, out...)
		return out
	}
	start := sessionEnd(0).Add(-time.Hour)
	apply(s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", start, cfg))
	for _, e := range opts.recorded {
		s.sequence++
		e.Sequence = s.sequence
		e.CausationID, e.CorrelationID = "", ""
		apply(e)
	}
	if opts.recovery != nil {
		recovered := *opts.recovery
		s.sequence++
		recovered.Sequence = s.sequence
		recovered.CausationID, recovered.CorrelationID = "", ""
		apply(recovered)
	}
	for _, e := range opts.prelude {
		recorded := e
		s.sequence++
		recorded.Sequence = s.sequence
		recorded.CausationID, recorded.CorrelationID = "", ""
		apply(recorded)
	}
	for i := opts.from; i < len(opts.closes); i++ {
		at := sessionEnd(i)
		price := opts.closes[i]
		for _, o := range acct.pending {
			ack := event.LimitOrderLifecyclePayload{
				AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, OrderID: "ib-" + o.ClientOrderID, InstrumentID: o.InstrumentID,
				Side: o.Side, Status: event.OrderLifecycleStatusSubmitted, Quantity: o.Quantity, LimitPrice: o.LimitPrice, OccurredAt: at.Add(-3 * time.Hour),
			}
			apply(s.envelope(t, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "ack:"+o.ClientOrderID, ack.OccurredAt, ack))
		}
		for _, f := range acct.fill(price, at.Add(-2*time.Hour)) {
			apply(s.envelope(t, event.FillEventType, event.FillTargetWeightVersion, f.FillID, f.FilledAt, f))
		}
		if engaged, ok := opts.kill[i]; ok {
			apply(s.envelope(t, event.KillSwitchEventType, event.KillSwitchSchemaVersion, fmt.Sprintf("kill:%d", i), at.Add(-time.Hour),
				event.KillSwitchPayload{AccountID: "joint", AsOf: at.Add(-time.Hour), Engaged: engaged, Operator: "owner", Detail: "fixture"}))
		}
		ids := []string{"SPY"}
		if !opts.skip[i] {
			apply(s.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, fmt.Sprintf("bar:%d", i), at, bar(price, at)))
		} else {
			ids = nil
		}
		if opts.snapshots != nil {
			for _, in := range opts.snapshots {
				if in.ID == fmt.Sprintf("snapshot:%d", i) {
					var p event.AccountPositionsSnapshotPayload
					decode(t, in, &p)
					apply(s.envelope(t, in.Type, in.SchemaVersion, in.ID, at, p))
				}
			}
		} else if !opts.noSnap[i] {
			apply(s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, fmt.Sprintf("snapshot:%d", i), at, acct.snapshot(at, price)))
		}
		if opts.brokers {
			var b event.BrokerSnapshotPayload
			found := false
			if opts.snapshots != nil {
				for _, in := range opts.snapshots {
					if in.ID == fmt.Sprintf("broker:%d", i) {
						decode(t, in, &b)
						found = true
					}
				}
			}
			if !found {
				b = acct.broker(at, price)
			}
			if m := opts.brokerMutate[i]; m != nil {
				m(&b)
			}
			out := apply(s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, fmt.Sprintf("broker:%d", i), b.AsOf, b))
			acct.take(t, out)
		}
		if !opts.noMark[i] {
			apply(s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, fmt.Sprintf("mark:%d", i), at,
				event.MarkRecordedPayload{InstrumentID: "SPY", AsOf: at, Price: price, Source: event.AdapterIBKR}))
		}
		if ids == nil {
			// The producer's session names what it delivered; a session with
			// no SPY bar still closes, naming the instruments it did deliver.
			ids = []string{"QQQ"}
		}
		out := apply(s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, fmt.Sprintf("session:%d", i), at,
			event.SessionClosedPayload{PeriodEnd: at, InstrumentIDs: ids}))
		acct.take(t, out)
	}
	r.entries = rec.Entries()
	return r
}

func decode(t *testing.T, e event.Envelope, into any) {
	t.Helper()
	if err := json.Unmarshal(e.Payload, into); err != nil {
		t.Fatal(err)
	}
}

func ofType(es []event.Envelope, eventType string) []event.Envelope {
	var out []event.Envelope
	for _, e := range es {
		if e.Type == eventType {
			out = append(out, e)
		}
	}
	return out
}

func targets(t *testing.T, es []event.Envelope) []event.TargetsDecidedPayload {
	t.Helper()
	var out []event.TargetsDecidedPayload
	for _, e := range ofType(es, event.TargetsDecidedEventType) {
		var p event.TargetsDecidedPayload
		decode(t, e, &p)
		out = append(out, p)
	}
	return out
}

func planned(t *testing.T, es []event.Envelope) []event.OrderPlannedPayload {
	t.Helper()
	var out []event.OrderPlannedPayload
	for _, e := range ofType(es, event.OrderPlannedEventType) {
		var p event.OrderPlannedPayload
		decode(t, e, &p)
		out = append(out, p)
	}
	return out
}
