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

// StatusText is the order's status as a person reads it. A released order is
// only "send requested": the log is written before the send, so a durable
// release is possibly sent, never known to be (ADR 0027, section 4). A
// dry-run releases orders with send false and never sends them (ADR 0026,
// point 3).
func (o OrderLine) StatusText() string {
	if o.Status != OrderReleased {
		return o.Status
	}
	if o.Send {
		return "released (send requested)"
	}
	return "released (not sent)"
}

// FillLine is one execution.fill record.
type FillLine struct {
	At            time.Time
	InstrumentID  string
	Side          string
	ClientOrderID string
	Quantity      int64
	Price         float64
}

// SaleWithheldLine is one risk.sale.withheld record: a sale back to 98% of
// net liquidation that fired and was not planned, and why.
type SaleWithheldLine struct {
	At            time.Time
	ClientOrderID string
	InstrumentID  string
	Trigger       string
	Reason        string
	Detail        string
}

// SkippedLine is one strategy.order.skipped record: a trade the order
// planner sized but could not order, the unrounded shares the rule asked for
// and why none is ordered (ADR 0026, point 8).
type SkippedLine struct {
	At            time.Time
	ClientOrderID string
	InstrumentID  string
	Side          string
	Reason        string
	Shares        float64
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
	SalesWithheld  []SaleWithheldLine
	Skipped        []SkippedLine
	EngineState    string
	Equity         Equity
	// CashChecked is set when the latest broker snapshot is the IBKR
	// adapter's reading as of a close (version 2): CashDrift is then the
	// cash drift since the previous close it accepted, within its daily
	// bound, and UnexplainedCash that drift added up since the cash was
	// last explained: a reconciliation the engine explained, else the
	// owner's starting book, which is always the basis for Boost, whose
	// book carries no cash; a run whose chain holds neither (a dry run's
	// first run, which has no approval) takes the daily basis, so the
	// figure is the day's drift (ADR 0019, amended 2026-10-18).
	// CashUnchecked is set instead when that snapshot is a version-2 reading
	// stamped at its read time, which checked no drift.
	CashChecked     bool
	CashUnchecked   bool
	CashDrift       float64
	UnexplainedCash float64
	// RejectedInputs counts the inputs the summary would have read (bars,
	// snapshots, the day's fills) that are at a schema version this build
	// does not read or that fail validation. None of them moves a figure.
	RejectedInputs int
}

// Summarize builds the account's summary for the UTC date from the journal's
// envelopes, in recording order. Holdings, engine state and equity are as of
// the end of that date; decisions, orders, fills and withheld sales back to
// 98% (a lapse among them, which goes here rather than in an urgent alert:
// ADR 0019, amendment of 2026-10-17, reading 1) and skipped trades (ADR 0026,
// point 8) are those of the date.
// Events after the date are ignored, so a past day's summary does not change
// when the journal grows.
//
// A journal keeps the inputs the engine refused, so an input that does not
// read or validate is skipped and counted in RejectedInputs: refusing the
// whole report for it would silence every later alert. A decision the
// engine recorded (targets, orders, engine state, withheld sales, skipped
// trades) that does not read or
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
		case event.BrokerSnapshotEventType:
			var p event.BrokerSnapshotPayload
			if read(e, brokerSnapshotVersions, &p) != nil {
				s.RejectedInputs++
				continue
			}
			v2 := e.SchemaVersion == event.BrokerSnapshotReadAtVersion
			s.CashChecked, s.CashUnchecked = v2 && p.AsOfClose, v2 && !p.AsOfClose
			s.CashDrift, s.UnexplainedCash = p.CashDrift, p.UnexplainedCash
		case event.CompletedBarEventType:
			p, err := readBar(e)
			if err != nil {
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
			line, err := readFill(e)
			if err != nil {
				s.RejectedInputs++
				continue
			}
			s.Fills = append(s.Fills, line)
		case event.SaleWithheldEventType:
			if !today {
				continue
			}
			var p event.SaleWithheldPayload
			if err := read(e, saleWithheldVersions, &p); err != nil {
				return Summary{}, err
			}
			s.SalesWithheld = append(s.SalesWithheld, SaleWithheldLine{At: e.EventTime, ClientOrderID: p.ClientOrderID,
				InstrumentID: p.InstrumentID, Trigger: p.Trigger, Reason: p.Reason, Detail: p.Detail})
		case event.OrderSkippedEventType:
			if !today {
				continue
			}
			var p event.OrderSkippedPayload
			if err := read(e, skippedVersions, &p); err != nil {
				return Summary{}, err
			}
			s.Skipped = append(s.Skipped, SkippedLine{At: e.EventTime, ClientOrderID: p.ClientOrderID, InstrumentID: p.InstrumentID,
				Side: p.Side, Reason: p.Reason, Shares: p.Shares})
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

// readFill decodes a fill at either version this build reads: the Turtle
// engine's (version 4) and the target-weight engines' (version 5, ADR 0026,
// Consequences), each validated at its own version.
func readFill(e event.Envelope) (FillLine, error) {
	if e.SchemaVersion == event.FillTargetWeightVersion {
		var p event.TargetWeightFillPayload
		if err := read(e, targetWeightFillVersions, &p); err != nil {
			return FillLine{}, err
		}
		return FillLine{At: p.FilledAt, InstrumentID: p.InstrumentID, Side: p.Side, ClientOrderID: p.ClientOrderID, Quantity: p.Quantity, Price: p.Price}, nil
	}
	var p event.FillPayload
	if err := read(e, fillVersions, &p); err != nil {
		return FillLine{}, err
	}
	return FillLine{At: p.FilledAt, InstrumentID: p.InstrumentID, Quantity: p.Quantity, Price: p.Price}, nil
}

// readBar decodes a bar at either version this build reads and validates it
// in full at its own version. The benchmark comparison takes the
// split-adjusted close of either: account money is kept in that one view
// (ADR 0004, amendment), and a version 2 bar's split- and dividend-adjusted
// view is for the target-weight strategies' signals only (ADR 0026, point 1),
// never for an equity figure.
func readBar(e event.Envelope) (event.CompletedBarPayload, error) {
	if e.SchemaVersion == event.CompletedBarAdjustedVersion {
		var p event.CompletedBarAdjustedPayload
		if err := read(e, barVersions, &p); err != nil {
			return event.CompletedBarPayload{}, err
		}
		return p.CompletedBarPayload, nil
	}
	var p event.CompletedBarPayload
	if err := read(e, barVersions, &p); err != nil {
		return event.CompletedBarPayload{}, err
	}
	return p, nil
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
