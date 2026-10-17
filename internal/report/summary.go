package report

import (
	"fmt"
	"sort"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// Engine states a summary reports.
const (
	StateNormal   = "normal"
	StateDegraded = event.EngineStateDegraded
	StateHalted   = event.EngineStateHalted
)

// Order statuses: planned, then released or refused by the gate.
const (
	OrderPlanned  = "planned"
	OrderReleased = "released"
	OrderRefused  = "refused"
)

// DecisionLine is one strategy.targets.decided record.
type DecisionLine struct {
	At             time.Time
	Outcome        string
	WithheldReason string
	Targets        []event.TargetWeight
}

// OrderLine is one order of the day and what the gate did with it.
type OrderLine struct {
	ClientOrderID string
	InstrumentID  string
	Side          string
	Quantity      int64
	LimitPrice    float64
	Status        string
	Reason        string
	Send          bool
}

// StatusText is the order's status as a person reads it: a released order
// says whether it was sent to the broker, since a dry-run releases orders
// with send false and never sends them (ADR 0026, point 3).
func (o OrderLine) StatusText() string {
	if o.Status != OrderReleased {
		return o.Status
	}
	if o.Send {
		return "released (sent)"
	}
	return "released (not sent)"
}

// FillLine is one execution.fill record.
type FillLine struct {
	At           time.Time
	InstrumentID string
	Quantity     int64
	Price        float64
}

// Equity compares the account's net liquidation with the benchmark's close
// over the points where both are recorded on one date. Returns are simple
// price returns: the benchmark's dividends are not included.
//
// The comparison runs from StartDate to EndDate, the latest paired point,
// which can be earlier than the summary's date when that day has no bar or
// snapshot. The day's figures compare EndDate with the paired point before
// it, DayStartDate, which is more than one session back when points are
// missing between them; they are available only when EndDate is the
// summary's date, so an earlier day's change is never shown as today's.
type Equity struct {
	Available          bool
	StartDate          string
	EndDate            string
	AccountReturn      float64
	BenchmarkReturn    float64
	Excess             float64
	DayAvailable       bool
	DayStartDate       string
	DayAccountReturn   float64
	DayBenchmarkReturn float64
}

// Summary is one account's day.
type Summary struct {
	AccountID      string
	StrategyID     string
	Date           string
	Benchmark      string
	HaveSnapshot   bool
	HoldingsAsOf   time.Time
	NetLiquidation float64
	Holdings       []event.AccountPosition
	Decisions      []DecisionLine
	Orders         []OrderLine
	Refusals       int
	Fills          []FillLine
	EngineState    string
	Equity         Equity
	// RejectedInputs counts the inputs the summary would have read (bars,
	// snapshots, the day's fills) that are at a schema version this build
	// does not read or that fail validation. None of them moves a figure.
	RejectedInputs int
}

// Summarize builds the account's summary for the UTC date from the journal's
// envelopes, in recording order. Holdings, engine state and equity are as of
// the end of that date; decisions, orders and fills are those of the date.
// Events after the date are ignored, so a past day's summary does not change
// when the journal grows.
//
// A journal keeps the inputs the engine refused, so an input that does not
// read or validate is skipped and counted in RejectedInputs: refusing the
// whole report for it would silence every later alert. A decision the
// engine recorded (targets, orders, engine state) that does not read or
// validate refuses the summary, as journalquery does.
func Summarize(envelopes []event.Envelope, date, benchmark string) (Summary, error) {
	end, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return Summary{}, fmt.Errorf("report: date %q is not YYYY-MM-DD: %w", date, err)
	}
	account, strategy, err := AccountOf(envelopes)
	if err != nil {
		return Summary{}, err
	}
	end = end.AddDate(0, 0, 1)
	s := Summary{AccountID: account, StrategyID: strategy, Date: date, Benchmark: benchmark, EngineState: StateNormal}
	orders := map[string]int{}
	closes := map[string]float64{}
	var points []equityPoint
	netBy := map[string]float64{}
	for _, e := range envelopes {
		if !e.EventTime.Before(end) {
			continue
		}
		today := dateOf(e.EventTime) == date
		switch e.Type {
		case event.AccountSnapshotEventType:
			if e.SchemaVersion == event.AccountSnapshotSchemaVersion {
				continue // equity and cash only: no positions to show
			}
			var p event.AccountPositionsSnapshotPayload
			if read(e, snapshotVersions, &p) != nil || !s.snapshotFollows(p) {
				s.RejectedInputs++
				continue
			}
			s.HaveSnapshot, s.HoldingsAsOf, s.NetLiquidation, s.Holdings = true, p.AsOf, p.NetLiquidation, p.Positions
			netBy[dateOf(p.AsOf)] = p.NetLiquidation
		case event.CompletedBarEventType:
			var p event.CompletedBarPayload
			if read(e, barVersions, &p) != nil {
				s.RejectedInputs++
				continue
			}
			if p.InstrumentID == benchmark {
				closes[dateOf(p.PeriodEnd)] = p.SplitAdjusted.Close
			}
		case event.EngineStateEventType:
			var p event.EngineStatePayload
			if err := read(e, engineStateVersions, &p); err != nil {
				return Summary{}, err
			}
			s.EngineState = p.State
		case event.TargetsDecidedEventType:
			if !today {
				continue
			}
			var p event.TargetsDecidedPayload
			if err := read(e, decidedVersions, &p); err != nil {
				return Summary{}, err
			}
			s.Decisions = append(s.Decisions, DecisionLine{At: e.EventTime, Outcome: p.Outcome, WithheldReason: p.WithheldReason, Targets: p.Targets})
		case event.OrderPlannedEventType:
			if !today {
				continue
			}
			var p event.OrderPlannedPayload
			if err := read(e, plannedVersions, &p); err != nil {
				return Summary{}, err
			}
			orders[p.ClientOrderID] = len(s.Orders)
			s.Orders = append(s.Orders, OrderLine{ClientOrderID: p.ClientOrderID, InstrumentID: p.InstrumentID, Side: p.Side,
				Quantity: p.Quantity, LimitPrice: p.LimitPrice, Status: OrderPlanned})
		case event.OrderReleasedEventType:
			if !today {
				continue
			}
			var p event.OrderReleasedPayload
			if err := read(e, releasedVersions, &p); err != nil {
				return Summary{}, err
			}
			line := s.order(orders, p.ClientOrderID, p.InstrumentID, p.Side, p.Quantity, p.LimitPrice)
			line.Status, line.Send = OrderReleased, p.Send
		case event.OrderRefusedEventType:
			if !today {
				continue
			}
			var p event.OrderRefusedPayload
			if err := read(e, refusedVersions, &p); err != nil {
				return Summary{}, err
			}
			line := s.order(orders, p.ClientOrderID, p.InstrumentID, p.Side, p.Quantity, p.LimitPrice)
			line.Status, line.Reason = OrderRefused, p.Reason
			s.Refusals++
		case event.FillEventType:
			if !today {
				continue
			}
			var p event.FillPayload
			if read(e, fillVersions, &p) != nil {
				s.RejectedInputs++
				continue
			}
			s.Fills = append(s.Fills, FillLine{At: p.FilledAt, InstrumentID: p.InstrumentID, Quantity: p.Quantity, Price: p.Price})
		}
	}
	for d, net := range netBy {
		if c, ok := closes[d]; ok && c > 0 && net > 0 {
			points = append(points, equityPoint{date: d, net: net, benchmark: c})
		}
	}
	sort.Slice(points, func(i, j int) bool { return points[i].date < points[j].date })
	s.Equity = equityOf(points, date)
	return s, nil
}

// snapshotFollows applies the engine's own rule for a positions snapshot
// (internal/targetweight, applySnapshot): it names this journal's account and
// is later than the last one taken. The log keeps a snapshot the engine
// refused, and one that broke either rule must not replace the holdings the
// engine decided on.
func (s *Summary) snapshotFollows(p event.AccountPositionsSnapshotPayload) bool {
	return p.AccountID == s.AccountID && (!s.HaveSnapshot || p.AsOf.After(s.HoldingsAsOf))
}

// order returns the day's line for a client order ID, adding one when the
// gate recorded an order the day's plans do not show.
func (s *Summary) order(index map[string]int, id, instrument, side string, quantity int64, limit float64) *OrderLine {
	i, ok := index[id]
	if !ok {
		i = len(s.Orders)
		index[id] = i
		s.Orders = append(s.Orders, OrderLine{ClientOrderID: id, InstrumentID: instrument, Side: side, Quantity: quantity, LimitPrice: limit})
	}
	return &s.Orders[i]
}

type equityPoint struct {
	date           string
	net, benchmark float64
}

func equityOf(points []equityPoint, date string) Equity {
	if len(points) < 2 {
		return Equity{}
	}
	first, last, prev := points[0], points[len(points)-1], points[len(points)-2]
	e := Equity{
		Available: true, StartDate: first.date, EndDate: last.date,
		AccountReturn:   last.net/first.net - 1,
		BenchmarkReturn: last.benchmark/first.benchmark - 1,
	}
	e.Excess = e.AccountReturn - e.BenchmarkReturn
	if last.date == date {
		e.DayAvailable, e.DayStartDate = true, prev.date
		e.DayAccountReturn = last.net/prev.net - 1
		e.DayBenchmarkReturn = last.benchmark/prev.benchmark - 1
	}
	return e
}
