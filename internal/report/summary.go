package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
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

// DecisionLine is the latest decision evidence for one account session
// (ADR 0026, amendment of 2026-10-29).
type DecisionLine struct {
	At             time.Time
	Outcome        string
	WithheldReason string
	Targets        []event.TargetWeight
	WithheldPicks  []event.WithheldPick
}

// OrderLine is one order of the day and what the gate did with it.
type OrderLine struct {
	QuantityScale int64 `json:",omitempty"`
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
	QuantityScale int64 `json:",omitempty"`
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
// planner sized but could not order, the shares the rule asked for
// and why none is ordered (ADR 0026, point 8).
type SkippedLine struct {
	At            time.Time
	ClientOrderID string
	InstrumentID  string
	Side          string
	Reason        string
	Shares        float64
}

// Equity compares the account with a shadow account holding the benchmark
// (equityOf has the rule) over the points where the account's net
// liquidation and the benchmark's close are both recorded on one date. Both
// returns are time-weighted, so a recorded deposit or withdrawal moves
// neither; the benchmark's dividends are reinvested where its bars carry the
// split- and dividend-adjusted view, and Assumption says in words what is
// counted.
//
// The comparison runs from StartDate to EndDate, the latest paired point,
// which can be earlier than the summary's date when that day has no bar or
// snapshot. The day's figures compare EndDate with the paired point before
// it, DayStartDate, which is more than one session back when points are
// missing between them; they are available only when EndDate is the
// summary's date, so an earlier day's change is never shown as today's.
//
// Points are the paired points themselves, oldest first, so a chart is drawn
// from the same figures the returns are computed from.
type Equity struct {
	// UnknownAccountPoints names omitted sessions whose recorded snapshot or
	// decision could not be read (ADR 0015).
	UnknownAccountPoints []string
	Available            bool
	StartDate            string
	EndDate              string
	AccountReturn        float64
	BenchmarkReturn      float64
	Excess               float64
	DayAvailable         bool
	DayStartDate         string
	DayAccountReturn     float64
	DayBenchmarkReturn   float64
	Points               []EquityPoint
	// ShadowValue is the shadow account's value on EndDate, and Gap the
	// account's net liquidation less it.
	ShadowValue float64
	Gap         float64
	// DollarNote says when the shadow account stopped at zero, and, once a
	// deposit restarted it, the day its dollar figures restart from;
	// DollarsWithheld is set while it is stopped, and ShadowValue and Gap
	// then mean nothing.
	DollarNote      string
	DollarsWithheld bool
	// Verdict says whether the account is beating the benchmark since
	// StartDate, from the two time-weighted returns; VerdictTone is
	// VerdictAhead, VerdictBehind or VerdictLevel.
	Verdict     string
	VerdictTone string
	// TransferCount, Deposits and Withdrawals are the recorded transfers
	// the shadow account mirrored, Withdrawals as a positive sum.
	TransferCount int
	Deposits      float64
	Withdrawals   float64
	// Assumption states what the comparison counts: the benchmark's
	// dividends, and the owner's transfers or that none is recorded.
	Assumption string
	// HistoryUnavailable is set when the chain of runs is broken
	// (HistoryUnavailable, the function): the comparison then runs from
	// HistoryStartsAt, the log of the first intact run it includes, not
	// from the account's first run.
	HistoryUnavailable string
	HistoryStartsAt    string
	// HistorySince is set when earlier runs of another mode come before
	// the current mode's (HistorySince, the function): the comparison then
	// runs from the first run of the current mode.
	HistorySince string
	// HistoryStarting is set when the current run is still starting
	// (HistoryStarting, the function): it has no history yet.
	HistoryStarting string
}

// EquityPoint is one date on which the account's net liquidation and the
// benchmark's close are both recorded. Benchmark is the split-adjusted
// close, the price as quoted; Shadow the shadow account's value at it;
// Transfers the recorded transfers mirrored since the point before; and
// AccountGrowth and BenchmarkGrowth each side's time-weighted growth since
// the first point, rebased to 100 there.
type EquityPoint struct {
	Date            string
	Net             float64
	Benchmark       float64
	Shadow          float64
	Transfers       float64
	AccountGrowth   float64
	BenchmarkGrowth float64
}

// Summary is one account's day.
type Summary struct {
	// QuantityScale applies to holdings; each order and fill carries its own (ADR 0026 D7-F).
	QuantityScale  int64 `json:",omitempty"`
	AccountID      string
	StrategyID     string
	Date           string
	Benchmark      string
	HaveSnapshot   bool
	HoldingsAsOf   time.Time
	NetLiquidation float64
	// HaveCash identifies a version-4 or version-6 IRA cash snapshot (ADR 0026 point 2).
	HaveCash      bool
	SettledCash   float64
	TotalCash     float64
	Holdings      []event.AccountPosition
	Decisions     []DecisionLine
	Orders        []OrderLine
	Refusals      int
	Fills         []FillLine
	SalesWithheld []SaleWithheldLine
	Skipped       []SkippedLine
	EngineState   string
	Equity        Equity
	// Costs is what the account has paid in commissions, fees and broker
	// interest (Costs).
	Costs Costs
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
// trades) that does not read or validate refuses the summary, as
// journalquery does. Past target decisions instead mark their account
// points unknown and omit them (manual, "Reading the figures").
func Summarize(envelopes []event.Envelope, date, benchmark string) (Summary, error) {
	return SummarizeWithCash(History{}, CashLedger{}, envelopes, date, benchmark)
}

// SummarizeWithHistory is Summarize for a run that follows others: history
// holds the envelopes of the runs before it, oldest first (EarlierRuns is how
// a caller finds them). Only the equity comparison reads them: its
// paired points, chart and returns run from the first run's first close, so a
// restart or an upgrade does not restart the history. Everything else is the
// current run's alone: a fresh run starts in the state its own approval
// states, not the state an earlier run ended in, and its inputs are counted
// as its own. A broken history is named in the comparison, which then runs
// over the intact runs alone.
func SummarizeWithHistory(history History, envelopes []event.Envelope, date, benchmark string) (Summary, error) {
	return SummarizeWithCash(history, CashLedger{}, envelopes, date, benchmark)
}

// SummarizeWithCash is SummarizeWithHistory with the owner's recorded
// deposits and withdrawals, ledger, which only the equity comparison reads:
// its shadow account mirrors them and its returns leave them out
// (equityOf). The ledger is read from the Flex reader's file, not from the
// journal, so a transfer is counted once whether or not the engine also
// journalled it.
func SummarizeWithCash(history History, ledger CashLedger, envelopes []event.Envelope, date, benchmark string) (Summary, error) {
	return SummarizeWithCosts(history, ledger, CostLedger{}, envelopes, date, benchmark)
}

// SummarizeWithCosts is SummarizeWithCash with the Flex reader's costs ledger,
// which only the costs block reads: the account's commissions on fills,
// from this run and the earlier ones, and the ledger's commissions, fees and
// broker interest, totalled by Costs.
func SummarizeWithCosts(history History, ledger CashLedger, costs CostLedger, envelopes []event.Envelope, date, benchmark string) (Summary, error) {
	// A fractional book is read only for a D7-F configuration. The versioned
	// payload validators still reject an unknown or mismatched scale (ADR 0015;
	// ADR 0026 D7-F).
	fractional := false
	for _, e := range envelopes {
		if e.Type == event.TargetWeightConfigurationEventType {
			var cfg event.TargetWeightConfigurationPayload
			if err := read(e, configurationVersions, &cfg); err != nil {
				return Summary{}, err
			}
			fractional = cfg.Execution.ShareQuantity == event.ShareQuantityFractional
		}
	}
	for _, e := range envelopes {
		var units struct {
			QuantityScale int64 `json:"quantity_scale"`
		}
		if json.Unmarshal(e.Payload, &units) == nil && units.QuantityScale != 0 && !fractional &&
			e.Type != event.OrderPlannedEventType && e.Type != event.OrderReleasedEventType && e.Type != event.OrderRefusedEventType {
			return Summary{}, fmt.Errorf("report: fractional quantity record in a whole-share account")
		}
	}
	earlier := history.Earlier
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
	type decisionSession struct {
		account string
		end     time.Time
	}
	decisions := map[decisionSession]int{}
	orders := map[string]int{}
	closes := map[string]benchBar{}
	var points []equityPoint
	netBy := map[string]float64{}
	// Close timestamps must match, not merely their dates: a reading stamped
	// at read time cannot value the session (ADR 0026 point 1; ADR 0019,
	// amendment of 2026-10-18). Only a stale-account-snapshot decision
	// disproves a standing close; a withheld rebalance keeps it (manual,
	// "Reading the figures"). netBy remains the dated cost denominator.
	closeNet := map[time.Time]float64{}
	withheld := map[time.Time]bool{}
	unknown := map[string]bool{}
	pairEarlier(earlier, end, account, strategy, benchmark, netBy, closeNet, closes, withheld, unknown)
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
			p, cash, err := readSnapshot(e, strategy)
			if err != nil || !s.snapshotFollows(p.AccountPositionsSnapshotPayload) {
				if err != nil && accountPointDate(e) < date {
					unknown[accountPointDate(e)] = true
				}
				s.RejectedInputs++
				continue
			}
			s.HaveSnapshot, s.HoldingsAsOf, s.NetLiquidation, s.Holdings = true, p.AsOf, p.NetLiquidation, p.Positions
			s.QuantityScale = p.QuantityScale
			s.HaveCash, s.SettledCash, s.TotalCash = cash, p.SettledCash, p.TotalCash
			netBy[dateOf(p.AsOf)] = p.NetLiquidation
			closeNet[p.AsOf.UTC()] = p.NetLiquidation
		case event.BrokerSnapshotEventType:
			var p event.BrokerSnapshotPayload
			if read(e, brokerSnapshotVersions, &p) != nil {
				s.RejectedInputs++
				continue
			}
			readAt := e.SchemaVersion == event.BrokerSnapshotReadAtVersion || e.SchemaVersion == event.BrokerSnapshotFractionalVersion
			s.CashChecked, s.CashUnchecked = readAt && p.AsOfClose, readAt && !p.AsOfClose
			s.CashDrift, s.UnexplainedCash = p.CashDrift, p.UnexplainedCash
		case event.CompletedBarEventType:
			p, bar, err := readBar(e)
			if err != nil {
				s.RejectedInputs++
				continue
			}
			if p.InstrumentID == benchmark {
				closes[dateOf(p.PeriodEnd)] = bar
			}
		case event.EngineStateEventType:
			var p event.EngineStatePayload
			if err := read(e, engineStateVersions, &p); err != nil {
				return Summary{}, err
			}
			s.EngineState = p.State
		case event.TargetsDecidedEventType:
			var p event.TargetsDecidedPayload
			if err := read(e, decidedVersions, &p); err != nil {
				if today {
					return Summary{}, err
				}
				unknown[dateOf(e.EventTime)] = true
				unknown[accountPointDate(e)] = true
				continue
			}
			if p.AccountID == account {
				withheld[p.SessionPeriodEnd.UTC()] = p.WithheldReason == event.WithheldStaleAccountSnapshot
			}
			if !today {
				continue
			}
			key := decisionSession{account: p.AccountID, end: p.SessionPeriodEnd.UTC()}
			line := DecisionLine{At: e.EventTime, Outcome: p.Outcome, WithheldReason: p.WithheldReason, Targets: p.Targets, WithheldPicks: p.WithheldPicks}
			if i, ok := decisions[key]; ok {
				s.Decisions[i] = line
			} else {
				decisions[key] = len(s.Decisions)
				s.Decisions = append(s.Decisions, line)
			}
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
				Quantity: p.Quantity, QuantityScale: p.QuantityScale, LimitPrice: p.LimitPrice, Status: OrderPlanned})
		case event.OrderReleasedEventType:
			if !today {
				continue
			}
			var p event.OrderReleasedPayload
			if err := read(e, releasedVersions, &p); err != nil {
				return Summary{}, err
			}
			line := s.order(orders, p.ClientOrderID, p.InstrumentID, p.Side, p.Quantity, p.QuantityScale, p.LimitPrice)
			line.Status, line.Send = OrderReleased, p.Send
		case event.OrderRefusedEventType:
			if !today {
				continue
			}
			var p event.OrderRefusedPayload
			if err := read(e, refusedVersions, &p); err != nil {
				return Summary{}, err
			}
			line := s.order(orders, p.ClientOrderID, p.InstrumentID, p.Side, p.Quantity, p.QuantityScale, p.LimitPrice)
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
	for d, c := range closes {
		if net, ok := closeNet[c.at.UTC()]; ok && !unknown[d] && !withheld[c.at.UTC()] && c.split > 0 && net > 0 {
			points = append(points, equityPoint{date: d, net: net, benchmark: c.split})
		}
	}
	sort.Slice(points, func(i, j int) bool { return points[i].date < points[j].date })
	s.Equity = equityOf(points, closes, ledger, benchmark, date)
	for d := range unknown {
		s.Equity.UnknownAccountPoints = append(s.Equity.UnknownAccountPoints, d)
	}
	sort.Strings(s.Equity.UnknownAccountPoints)
	s.Costs = costsOf(earlier, envelopes, end, date, costs, netBy)
	if history.Broken != nil {
		s.Equity.HistoryUnavailable, s.Equity.HistoryStartsAt = HistoryUnavailable(history.Broken), historyStartsAt(history)
	}
	s.Equity.HistorySince = HistorySince(history)
	s.Equity.HistoryStarting = HistoryStarting(history)
	return s, nil
}

// pairEarlier adds to netBy and closes the net liquidation and benchmark
// closes the earlier runs recorded before end, by the rules the current run
// reads its own by: a snapshot names this account and is later than the last
// taken. Unreadable snapshots or decisions leave their account point unknown
// (ADR 0015; manual, "Reading the figures"). The current run's records then replace a date both hold. Close timestamps
// and withheld decisions are kept too, for the same pairing as this run's
// (ADR 0026 point 1; Boost lessons L11).
func pairEarlier(earlier []event.Envelope, end time.Time, account, strategy, benchmark string, netBy map[string]float64, closeNet map[time.Time]float64, closes map[string]benchBar, withheld map[time.Time]bool, unknown map[string]bool) {
	var last time.Time
	taken := false
	for _, e := range earlier {
		if !e.EventTime.Before(end) {
			continue
		}
		switch e.Type {
		case event.AccountSnapshotEventType:
			if e.SchemaVersion == event.AccountSnapshotSchemaVersion {
				continue
			}
			p, _, err := readSnapshot(e, strategy)
			if err != nil || p.AccountID != account || (taken && !p.AsOf.After(last)) {
				if err != nil {
					unknown[accountPointDate(e)] = true
				}
				continue
			}
			taken, last = true, p.AsOf
			netBy[dateOf(p.AsOf)] = p.NetLiquidation
			closeNet[p.AsOf.UTC()] = p.NetLiquidation
		case event.TargetsDecidedEventType:
			var p event.TargetsDecidedPayload
			if err := read(e, decidedVersions, &p); err != nil {
				unknown[dateOf(e.EventTime)] = true
				unknown[accountPointDate(e)] = true
			} else if p.AccountID == account {
				withheld[p.SessionPeriodEnd.UTC()] = p.WithheldReason == event.WithheldStaleAccountSnapshot
			}
		case event.CompletedBarEventType:
			if p, bar, err := readBar(e); err == nil && p.InstrumentID == benchmark {
				closes[dateOf(p.PeriodEnd)] = bar
			}
		}
	}
}

// accountPointDate selects an unreadable record's account point for exclusion
// before returns are computed and for unknown-point diagnostics (ADR 0015).
// A decoded snapshot as_of or decision session_period_end supplies only an
// exclusion date, never a figure; otherwise the event date is used. Callers
// must also exclude a rejected decision's event date: its unvalidated session
// date cannot replace that conservative exclusion.
func accountPointDate(e event.Envelope) string {
	var p struct {
		AsOf             time.Time `json:"as_of"`
		SessionPeriodEnd time.Time `json:"session_period_end"`
	}
	if json.Unmarshal(e.Payload, &p) == nil {
		if e.Type == event.AccountSnapshotEventType && !p.AsOf.IsZero() {
			return dateOf(p.AsOf)
		}
		if e.Type == event.TargetsDecidedEventType && !p.SessionPeriodEnd.IsZero() {
			return dateOf(p.SessionPeriodEnd)
		}
	}
	return dateOf(e.EventTime)
}

// ForHistory returns what of an earlier run's recovery the chain of runs and
// the equity history read, for a caller that keeps earlier logs between
// reads: its header and final record hash, its first two records (its
// configuration and approval, ADR 0027), every configuration and approval
// input after them (FollowChain refuses a second approval), every account
// snapshot and target decision (the standing-close rule needs both), of
// every fill only its ID, commission, time and version, which
// the costs block counts (earlierFillOf), and every bar of benchmark
// (pairEarlier). A log holding nothing
// past its configuration keeps that one record, so restart.CutShort reads it
// as it reads the whole log.
func ForHistory(recovery journal.Recovery, benchmark string) journal.Recovery {
	kept := recovery
	kept.Entries = nil
	for i, e := range recovery.Entries {
		env := e.Envelope
		keep := i < 2
		switch {
		case env.Type == event.TargetsDecidedEventType:
			keep = true
		case e.Kind != journal.KindInput:
		case env.Type == event.TargetWeightConfigurationEventType, env.Type == event.OperatorRunApprovedEventType, env.Type == event.AccountSnapshotEventType:
			keep = true
		case env.Type == event.FillEventType:
			e.Envelope, keep = earlierFillOf(env), true
		case env.Type == event.CompletedBarEventType:
			if p, _, err := readBar(env); err == nil && p.InstrumentID == benchmark {
				keep = true
			}
		}
		if keep {
			kept.Entries = append(kept.Entries, e)
		}
	}
	return kept
}

// readFill decodes Turtle fills (version 4) and target-weight fills (versions
// 5 and 6), each validated at its own version (ADR 0015; ADR 0026 D7-F).
func readFill(e event.Envelope) (FillLine, error) {
	if e.SchemaVersion == event.FillTargetWeightVersion || e.SchemaVersion == event.FillTargetWeightFractionalVersion {
		var p event.TargetWeightFillPayload
		if err := read(e, targetWeightFillVersions, &p); err != nil {
			return FillLine{}, err
		}
		return FillLine{At: p.FilledAt, InstrumentID: p.InstrumentID, Side: p.Side, ClientOrderID: p.ClientOrderID, Quantity: p.Quantity, QuantityScale: p.QuantityScale, Price: p.Price}, nil
	}
	var p event.FillPayload
	if err := read(e, fillVersions, &p); err != nil {
		return FillLine{}, err
	}
	return FillLine{At: p.FilledAt, InstrumentID: p.InstrumentID, Quantity: p.Quantity, Price: p.Price}, nil
}

// readBar decodes a bar at either version this build reads and validates it
// in full at its own version, and returns its closes for the benchmark
// comparison. The split-adjusted close is the price as quoted, the view
// account money is kept in (ADR 0004, amendment). A version 2 bar's split-
// and dividend-adjusted close is read only for the benchmark's growth, as
// its ratios between sessions of one base are the benchmark held with its
// dividends reinvested (CompletedBarAdjustedPayload); no account figure is
// computed from it.
func readBar(e event.Envelope) (event.CompletedBarPayload, benchBar, error) {
	if e.SchemaVersion == event.CompletedBarAdjustedVersion {
		var p event.CompletedBarAdjustedPayload
		if err := read(e, barVersions, &p); err != nil {
			return event.CompletedBarPayload{}, benchBar{}, err
		}
		return p.CompletedBarPayload, benchBar{at: p.PeriodEnd, split: p.SplitAdjusted.Close, adjusted: p.Adjusted.Close, base: p.AdjustedBase, v2: true}, nil
	}
	var p event.CompletedBarPayload
	if err := read(e, barVersions, &p); err != nil {
		return event.CompletedBarPayload{}, benchBar{}, err
	}
	return p, benchBar{at: p.PeriodEnd, split: p.SplitAdjusted.Close}, nil
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
func (s *Summary) order(index map[string]int, id, instrument, side string, quantity, scale int64, limit float64) *OrderLine {
	i, ok := index[id]
	if !ok {
		i = len(s.Orders)
		index[id] = i
		s.Orders = append(s.Orders, OrderLine{ClientOrderID: id, InstrumentID: instrument, Side: side, Quantity: quantity, QuantityScale: scale, LimitPrice: limit})
	}
	return &s.Orders[i]
}

type equityPoint struct {
	date           string
	net, benchmark float64
}
