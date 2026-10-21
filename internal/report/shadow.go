package report

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/nysecalendar"
)

// The verdict's tones: the account ahead of the benchmark's shadow account,
// behind it, or level with it within levelBand.
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
	// A close that is not positive is no link of the index: it is left out,
	// as the pairing filter leaves it out of the points, and said.
	var leftOut []string
	for d, bar := range bars {
		if d < first.date || d > last.date {
			continue
		}
		if !(bar.split > 0) {
			leftOut = append(leftOut, d)
			continue
		}
		sessions = append(sessions, d)
	}
	sort.Strings(leftOut)
	sort.Strings(sessions)
	index := map[string]float64{sessions[0]: 1}
	// The links that are a price return, as spans of sessions: a run of
	// consecutive price-only links is one span, from the session before its
	// first link to its last.
	var priceOnly [][2]string
	reinvestedAny, extend := false, false
	for i := 1; i < len(sessions); i++ {
		ratio, reinvested := growthLink(bars[sessions[i-1]], bars[sessions[i]])
		index[sessions[i]] = index[sessions[i-1]] * ratio
		switch {
		case reinvested:
			reinvestedAny, extend = true, false
		case extend:
			priceOnly[len(priceOnly)-1][1] = sessions[i]
		default:
			priceOnly, extend = append(priceOnly, [2]string{sessions[i-1], sessions[i]}), true
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
	// The shadow's holding, in units of the index. It cannot sell what it
	// does not hold: a withdrawal that takes its value to zero (within half
	// a cent) or below leaves it at zero, stopped, and its dollar figures
	// are withheld until the next recorded deposit buys the benchmark at
	// that session's close and restarts it. The time-weighted verdict never
	// depends on the units, so it is kept.
	units := first.net
	stopped, stoppedOn, restartedOn := false, "", ""
	growth, prevNet := 1.0, first.net
	next := 0
	for _, p := range points {
		var moved float64
		for ; next < len(sessions) && sessions[next] <= p.date; next++ {
			f := flows[sessions[next]]
			if f == 0 {
				continue
			}
			moved += f
			at := index[sessions[next]]
			switch {
			case stopped && f > 0:
				units, stopped, restartedOn = f/at, false, sessions[next]
			case stopped:
			case f < 0 && float64(units*at)+f < 0.005:
				units, stopped, stoppedOn, restartedOn = 0, true, sessions[next], ""
			default:
				units += f / at
			}
		}
		if p.date != first.date {
			growth *= (p.net - moved) / prevNet
		}
		prevNet = p.net
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
	if stoppedOn != "" {
		e.DollarNote = fmt.Sprintf("the recorded withdrawals exceeded the %s shadow's value on %s; ", benchmark, stoppedOn)
		if stopped {
			e.DollarsWithheld = true
			e.DollarNote += "dollar figures are not meaningful after that day"
		} else {
			e.DollarNote += "dollar figures restart from " + restartedOn
		}
	}
	e.Assumption = assumption(benchmark, reinvestedAny, priceOnly, leftOut, ledger, date, e)
	return e
}

// verdict says in words whether the account is beating the benchmark since
// the first paired day, by the gap between the two time-weighted returns in
// percentage points, to one decimal ("1.0 point", else "points"); less than
// levelBand apart is level.
func verdict(excess float64, benchmark, since string) (text, tone string) {
	points := excess * 100
	switch {
	case math.Abs(points) < levelBand:
		return fmt.Sprintf("Level with %s since %s", benchmark, since), VerdictLevel
	case points > 0:
		return fmt.Sprintf("Beating %s by %s since %s", benchmark, inPoints(points), since), VerdictAhead
	default:
		return fmt.Sprintf("Behind %s by %s since %s", benchmark, inPoints(-points), since), VerdictBehind
	}
}

func inPoints(v float64) string {
	text := fmt.Sprintf("%.1f", v)
	if text == "1.0" {
		return text + " point"
	}
	return text + " points"
}

// assumption states exactly what the comparison counts: whether the
// benchmark's dividends are reinvested over the whole comparison, on every
// link but the spans named, or not at all, and any session left out; and
// whether the owner's transfers are mirrored, through which day the Flex
// reader has recorded them, and whether the summary's day, date, is past it.
func assumption(benchmark string, reinvestedAny bool, priceOnly [][2]string, leftOut []string, ledger CashLedger, date string, e Equity) string {
	var parts []string
	switch {
	case !reinvestedAny:
		parts = append(parts, benchmark+" price return only: dividends not counted")
	case len(priceOnly) > 0:
		spans := make([]string, len(priceOnly))
		for i, span := range priceOnly {
			spans[i] = span[0] + "–" + span[1]
		}
		parts = append(parts, fmt.Sprintf("%s price return only for %s; dividends reinvested for the rest", benchmark, inWords(spans)))
	default:
		parts = append(parts, benchmark+" with dividends reinvested")
	}
	for _, d := range leftOut {
		parts = append(parts, fmt.Sprintf("%s close on %s is not positive: that session is left out", benchmark, d))
	}
	assumesNone := "the comparison assumes none, so money you add or take out counts as the account's own gain or loss"
	switch {
	case ledger.NotAccepted != "":
		parts = append(parts, fmt.Sprintf("ledger not accepted: %s; %s", ledger.NotAccepted, assumesNone))
	case ledger.Read:
		parts = append(parts, fmt.Sprintf("your deposits and withdrawals mirrored as recorded by the Flex reader (%s)", transfersInWords(e)))
		switch {
		case ledger.CoveredTo == "":
			parts = append(parts, "how far the Flex reader has recorded is not known; "+lastTwoDays)
		case date > ledger.CoveredTo:
			parts = append(parts, "transfers recorded through "+ledger.CoveredTo, lastTwoDays+"; a transfer appears in the summary two evenings after it posts")
		default:
			parts = append(parts, "transfers recorded through "+ledger.CoveredTo)
		}
	case ledger.Unreadable != "":
		parts = append(parts, fmt.Sprintf("deposits and withdrawals not readable (%s): %s", ledger.Unreadable, assumesNone))
	default:
		parts = append(parts, "deposits and withdrawals not yet recorded: "+assumesNone)
	}
	return "Counted: " + strings.Join(parts, "; ") + ". Both returns are time-weighted; fees and interest stay in the account's own result."
}

// lastTwoDays is the reader's lag: a transfer posted on day D is first in
// the statement read on the evening of the next session, so the summary two
// sessions after D is the first to include it (reconciliation chapter,
// "When"; a Friday transfer is in Tuesday's summary).
const lastTwoDays = "deposits or withdrawals from the last two days may not be recorded yet"

// inWords joins items as a list in words: "a", "a and b", "a, b and c".
func inWords(items []string) string {
	if len(items) == 1 {
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
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
// account's value on the latest paired day, then the note on a stopped or
// restarted shadow; the verdict and the note alone while the dollar figures
// are withheld, since the gap then measures nothing; "" before the
// comparison is available.
func VerdictLine(e Equity, benchmark string) string {
	if !e.Available {
		return ""
	}
	if e.DollarsWithheld {
		return e.Verdict + "; " + e.DollarNote
	}
	line := fmt.Sprintf("%s; dollar gap %s (account %.2f, %s shadow %.2f)", e.Verdict, SignedMoney(e.Gap), e.Points[len(e.Points)-1].Net, benchmark, e.ShadowValue)
	if e.DollarNote != "" {
		line += "; " + e.DollarNote
	}
	return line
}

// SignedMoney is a dollar figure to the cent with its sign; a figure that
// rounds to zero cents is +0.00, never -0.00.
func SignedMoney(v float64) string {
	if math.Abs(v) < 0.005 {
		v = 0
	}
	return fmt.Sprintf("%+.2f", v)
}
