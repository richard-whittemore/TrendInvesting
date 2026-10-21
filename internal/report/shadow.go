package report

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/nysecalendar"
)

// The verdict's three tones: the account ahead of the benchmark's shadow
// account, behind it, or level with it within levelBand.
const (
	VerdictAhead  = "ahead"
	VerdictBehind = "behind"
	VerdictLevel  = "level"
)

// levelBand is the gap between the two time-weighted returns, in percentage
// points, below which the verdict calls them level.
const levelBand = 0.1

// benchBar is the benchmark's close on one session: the split-adjusted close,
// which is the price as quoted, and, from a version 2 bar, the split- and
// dividend-adjusted close and the session it is based at.
type benchBar struct {
	split    float64
	adjusted float64
	base     string
	v2       bool
}

// growthLink is the benchmark's growth from the session before to this one:
// the ratio of the adjusted closes when both bars are version 2 with one
// base (dividends reinvested: the ratio method; CompletedBarAdjustedPayload
// says levels compare only within one base), else of the split-adjusted
// closes, a price return.
func growthLink(prev, next benchBar) (ratio float64, reinvested bool) {
	if prev.v2 && next.v2 && prev.base == next.base && prev.adjusted > 0 && next.adjusted > 0 {
		return next.adjusted / prev.adjusted, true
	}
	return next.split / prev.split, false
}

// equityOf builds the comparison over the paired points, oldest first.
//
// The benchmark side is a shadow account: it holds the first paired point's
// net liquidation in the benchmark at that session's close, and each
// recorded deposit or withdrawal buys or sells the benchmark for the same
// amount at the close of its New York day, or of the next session with a
// close when that day has none. A transfer on or before the first paired
// day is in the starting balance.
//
// Both returns are time-weighted: each interval between paired points grows
// by (net at its end less the transfers mirrored in it) over the net at its
// start, chained, so a transfer is never a gain or a loss. The benchmark's
// is its growth index's, which is the shadow's time-weighted return. The
// day's figures are the last interval's.
func equityOf(points []equityPoint, bars map[string]benchBar, ledger CashLedger, benchmark, date string) Equity {
	if len(points) < 2 {
		return Equity{}
	}
	first, last := points[0], points[len(points)-1]
	// The benchmark's growth index over its sessions in the comparison.
	var sessions []string
	for d := range bars {
		if d >= first.date && d <= last.date {
			sessions = append(sessions, d)
		}
	}
	sort.Strings(sessions)
	index := map[string]float64{sessions[0]: 1}
	priceOnlyUntil, reinvestedAny := "", false
	for i := 1; i < len(sessions); i++ {
		ratio, reinvested := growthLink(bars[sessions[i-1]], bars[sessions[i]])
		index[sessions[i]] = index[sessions[i-1]] * ratio
		if reinvested {
			reinvestedAny = true
		} else {
			priceOnlyUntil = sessions[i]
		}
	}
	// Each recorded transfer, at the session whose close mirrors it.
	flows := map[string]float64{}
	e := Equity{Available: true, StartDate: first.date, EndDate: last.date}
	for _, m := range ledger.Movements {
		day := m.AsOf.In(nysecalendar.NewYork).Format(time.DateOnly)
		at := sort.SearchStrings(sessions, day)
		if at == len(sessions) || sessions[at] <= first.date {
			continue
		}
		flows[sessions[at]] += m.Amount
		e.TransferCount++
		if m.Amount > 0 {
			e.Deposits += m.Amount
		} else {
			e.Withdrawals -= m.Amount
		}
	}
	units := first.net // the shadow's holding, in units of the index
	growth := 1.0
	next := 0
	for k, p := range points {
		var moved float64
		for ; next < len(sessions) && sessions[next] <= p.date; next++ {
			if f := flows[sessions[next]]; f != 0 {
				moved += f
				units += f / index[sessions[next]]
			}
		}
		if k > 0 {
			growth *= (p.net - moved) / points[k-1].net
		}
		e.Points = append(e.Points, EquityPoint{Date: p.date, Net: p.net, Benchmark: p.benchmark,
			Shadow: units * index[p.date], Transfers: moved,
			AccountGrowth: 100 * growth, BenchmarkGrowth: 100 * index[p.date]})
	}
	lastPoint, prevPoint := e.Points[len(e.Points)-1], e.Points[len(e.Points)-2]
	e.AccountReturn = growth - 1
	e.BenchmarkReturn = index[last.date] - 1
	e.Excess = e.AccountReturn - e.BenchmarkReturn
	e.ShadowValue = lastPoint.Shadow
	e.Gap = lastPoint.Net - lastPoint.Shadow
	if last.date == date {
		e.DayAvailable, e.DayStartDate = true, prevPoint.Date
		e.DayAccountReturn = (lastPoint.Net-lastPoint.Transfers)/prevPoint.Net - 1
		e.DayBenchmarkReturn = index[last.date]/index[prevPoint.Date] - 1
	}
	e.Verdict, e.VerdictTone = verdict(e.Excess, benchmark, first.date)
	e.Assumption = assumption(benchmark, reinvestedAny, priceOnlyUntil, ledger, e)
	return e
}

// verdict says in words whether the account is beating the benchmark since
// the first paired day, by the gap between the two time-weighted returns in
// percentage points, to one decimal; less than levelBand apart is level.
func verdict(excess float64, benchmark, since string) (text, tone string) {
	points := excess * 100
	switch {
	case math.Abs(points) < levelBand:
		return fmt.Sprintf("Level with %s since %s", benchmark, since), VerdictLevel
	case points > 0:
		return fmt.Sprintf("Beating %s by %.1f points since %s", benchmark, points, since), VerdictAhead
	default:
		return fmt.Sprintf("Behind %s by %.1f points since %s", benchmark, -points, since), VerdictBehind
	}
}

// assumption states exactly what the comparison counts: whether the
// benchmark's dividends are reinvested over the whole comparison, from a
// date, or not at all, and whether the owner's transfers are mirrored.
func assumption(benchmark string, reinvestedAny bool, priceOnlyUntil string, ledger CashLedger, e Equity) string {
	var parts []string
	switch {
	case !reinvestedAny:
		parts = append(parts, benchmark+" price return only: dividends not counted")
	case priceOnlyUntil != "":
		parts = append(parts, fmt.Sprintf("%s with dividends reinvested from %s; price return only before %s", benchmark, priceOnlyUntil, priceOnlyUntil))
	default:
		parts = append(parts, benchmark+" with dividends reinvested")
	}
	switch {
	case ledger.Read:
		parts = append(parts, fmt.Sprintf("your deposits and withdrawals mirrored as recorded by the Flex reader (%s)", transfersInWords(e)))
	case ledger.Unreadable != "":
		parts = append(parts, fmt.Sprintf("deposits and withdrawals not readable (%s): the comparison assumes none, so money you add or take out counts as the account's own gain or loss", ledger.Unreadable))
	default:
		parts = append(parts, "deposits and withdrawals not yet recorded: the comparison assumes none, so money you add or take out counts as the account's own gain or loss")
	}
	return "Counted: " + strings.Join(parts, "; ") + ". Both returns are time-weighted; fees and interest stay in the account's own result."
}

func transfersInWords(e Equity) string {
	switch e.TransferCount {
	case 0:
		return "none in this period"
	case 1:
		return fmt.Sprintf("1 in this period: %.2f in, %.2f out", e.Deposits, e.Withdrawals)
	}
	return fmt.Sprintf("%d in this period: %.2f in, %.2f out", e.TransferCount, e.Deposits, e.Withdrawals)
}

// VerdictLine is the verdict with the dollar gap beside it, as the daily
// summary sends it: the account's net liquidation against the shadow
// account's value on the latest paired day; "" before the comparison is
// available.
func VerdictLine(e Equity, benchmark string) string {
	if !e.Available {
		return ""
	}
	return fmt.Sprintf("%s; dollar gap %s (account %.2f, %s shadow %.2f)", e.Verdict, SignedMoney(e.Gap), e.Points[len(e.Points)-1].Net, benchmark, e.ShadowValue)
}

// SignedMoney is a dollar figure to the cent with its sign; a figure that
// rounds to zero cents is +0.00, never -0.00.
func SignedMoney(v float64) string {
	if math.Abs(v) < 0.005 {
		v = 0
	}
	return fmt.Sprintf("%+.2f", v)
}
