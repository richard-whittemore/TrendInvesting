package journalquery

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// Filter narrows a query. Empty fields match everything.
type Filter struct {
	Instrument string
	// Account keeps records of one account. A record that names no account
	// (a fill) belongs to its journal's account when the journal has exactly
	// one, and is dropped by an account filter otherwise.
	Account string
	// from is the first instant kept and until the first instant dropped; a
	// zero value leaves that end open.
	from, until time.Time
	// AllowTrailingFills lets reconcile pass over fills recorded after an
	// account's last snapshot, which no snapshot has checked yet.
	AllowTrailingFills bool
}

// NewFilter builds a Filter. from and to are UTC dates (YYYY-MM-DD), both
// inclusive of their whole day, either may be empty.
func NewFilter(account, instrument, from, to string) (Filter, error) {
	f := Filter{Account: account, Instrument: instrument}
	var err error
	if from != "" {
		if f.from, err = time.Parse(time.DateOnly, from); err != nil {
			return Filter{}, fmt.Errorf("from %q is not a YYYY-MM-DD date", from)
		}
	}
	if to != "" {
		end, err := time.Parse(time.DateOnly, to)
		if err != nil {
			return Filter{}, fmt.Errorf("to %q is not a YYYY-MM-DD date", to)
		}
		f.until = end.AddDate(0, 0, 1)
	}
	if !f.from.IsZero() && !f.until.IsZero() && !f.from.Before(f.until) {
		return Filter{}, fmt.Errorf("the range runs backwards: from %s is after to %s", from, to)
	}
	return f, nil
}

func (f Filter) keepInstrument(id string) bool { return f.Instrument == "" || f.Instrument == id }

func (f Filter) keepTime(at time.Time) bool {
	return (f.from.IsZero() || !at.Before(f.from)) && (f.until.IsZero() || at.Before(f.until))
}

func (f Filter) keepAccount(it item) bool { return f.Account == "" || f.Account == it.account }

func (f Filter) keep(it item, instrument string) bool {
	return f.keepAccount(it) && f.keepInstrument(instrument) && f.keepTime(it.at)
}

func (f Filter) keepFill(it item) bool {
	return it.fill != nil && f.keep(it, it.fill.InstrumentID)
}

// keepQuantity selects unit declarations for totals (ADR 0026 D7-F).
// Snapshots declare account-wide units, including when no instrument is held.
func (f Filter) keepQuantity(it item) bool {
	switch {
	case it.fill != nil:
		return f.keepFill(it)
	case it.snapshot != nil:
		return f.keepAccount(it) && f.keepTime(it.at)
	case it.planned != nil:
		return f.keep(it, it.planned.InstrumentID)
	case it.released != nil:
		return f.keep(it, it.released.InstrumentID)
	case it.refused != nil:
		return f.keep(it, it.refused.InstrumentID)
	case it.lifecycle != nil:
		return f.keep(it, it.lifecycle.InstrumentID)
	case it.skipped != nil:
		return f.keep(it, it.skipped.InstrumentID)
	default:
		return false
	}
}

// signedQuantity is a fill's effect on the position: entries, adds and buys
// buy, stops, exits and sells sell (both fill versions record the executed
// size unsigned).
func signedQuantity(kind string, quantity int64) int64 {
	if kind == "stop" || kind == "exit" || kind == event.OrderSideSell {
		return -quantity
	}
	return quantity
}

// money prints v with at least two decimals and no padding beyond what the
// figure needs, so 1.5 reads 1.50 and a sub-cent commission is not rounded.
func money(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if i := strings.IndexByte(s, '.'); i < 0 {
		return s + ".00"
	} else if len(s)-i-1 < 2 {
		return s + "0"
	}
	return s
}

// Fills lists every fill that passes f, in journal order.
func Fills(journals []Journal, f Filter) (Table, error) {
	if err := distinct(journals); err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"journal", "seq", "time", "instrument", "kind", "quantity", "price", "commission"}}
	for _, j := range journals {
		items, err := decode(j)
		if err != nil {
			return Table{}, err
		}
		for _, it := range items {
			if !f.keepFill(it) {
				continue
			}
			t.Rows = append(t.Rows, []string{
				it.journal, strconv.FormatUint(it.seq, 10), it.at.UTC().Format("2006-01-02"), it.fill.InstrumentID, it.fill.Kind,
				event.FormatQuantity(signedQuantity(it.fill.Kind, it.fill.Quantity), it.fill.QuantityScale), money(it.fill.Price), money(it.fill.Commission),
			})
		}
	}
	return t, nil
}

// moneySum prints a sum of commissions. Adding cents in floating point leaves
// noise in the last bits (0.1+0.2), so a sum is rounded to a millionth of a
// dollar first; the explicit conversion keeps the product from fusing into
// the rounding (docs/development.md, floating-point determinism).
func moneySum(v float64) string {
	scaled := float64(v * 1e6)
	return money(math.Round(scaled) / 1e6)
}

type tally struct {
	fills, bought, sold int64
	commission          float64
}

func (t *tally) add(f *fill) error {
	t.fills++
	quantity := &t.bought
	if signedQuantity(f.Kind, f.Quantity) < 0 {
		quantity = &t.sold
	}
	sum, ok := addQuantity(*quantity, f.Quantity)
	if !ok {
		return fmt.Errorf("quantity totals exceed int64 (overflow)")
	}
	*quantity = sum
	t.commission += f.Commission
	return nil
}

func (t tally) row(instrument, year string, scale int64) []string {
	return []string{instrument, year, strconv.FormatInt(t.fills, 10), event.FormatQuantity(t.bought, scale), event.FormatQuantity(t.sold, scale), moneySum(t.commission)}
}

// Totals counts fills, shares bought and sold, and commission paid for each
// instrument in each calendar year (UTC, by the fill's own time). Whole-share
// selections retain their combined TOTAL (ADR 0016). Selections containing
// fractional records show an account column and a TOTAL per account, so units
// at different scales are never added (ADR 0026 D7-F).
func Totals(journals []Journal, f Filter) (Table, error) {
	if err := distinct(journals); err != nil {
		return Table{}, err
	}
	decoded, err := decodeAggregations(journals)
	if err != nil {
		return Table{}, err
	}
	perAccount := false
	scales := map[string]int64{}
	for _, items := range decoded {
		for _, it := range items {
			if !f.keepQuantity(it) {
				continue
			}
			scale, _ := it.quantityScale()
			scales[it.account] = max(scales[it.account], scale)
			perAccount = perAccount || it.recordedScale != 0
		}
	}
	type key struct {
		account, instrument string
		year                int
	}
	groups := map[key]*tally{}
	totals := map[string]*tally{}
	if perAccount {
		for account := range scales {
			totals[account] = &tally{}
		}
	} else {
		totals[""] = &tally{}
	}
	for _, items := range decoded {
		for _, it := range items {
			if !f.keepFill(it) {
				continue
			}
			account := ""
			if perAccount {
				account = it.account
			}
			execution := *it.fill
			if !perAccount && execution.QuantityScale != 0 {
				// A whole-share selection retains its recorded units after replay's
				// checked legacy upcast (ADR 0026 D7-F; ADR 0016).
				execution.Quantity /= execution.QuantityScale
				execution.QuantityScale = 0
			}
			scales[account] = execution.QuantityScale
			k := key{account, it.fill.InstrumentID, it.at.UTC().Year()}
			if groups[k] == nil {
				groups[k] = &tally{}
			}
			if err := groups[k].add(&execution); err != nil {
				return Table{}, fmt.Errorf("%s: record %d: %w", it.journal, it.seq, err)
			}
			if err := totals[account].add(&execution); err != nil {
				return Table{}, fmt.Errorf("%s: record %d: %w", it.journal, it.seq, err)
			}
		}
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool {
		if keys[a].account != keys[b].account {
			return keys[a].account < keys[b].account
		}
		if keys[a].instrument != keys[b].instrument {
			return keys[a].instrument < keys[b].instrument
		}
		return keys[a].year < keys[b].year
	})
	t := Table{Columns: []string{"instrument", "year", "fills", "bought", "sold", "commission"}}
	if perAccount {
		t.Columns = append([]string{"account"}, t.Columns...)
	}
	for _, k := range keys {
		row := groups[k].row(k.instrument, strconv.Itoa(k.year), scales[k.account])
		if perAccount {
			row = append([]string{accountCell(k.account)}, row...)
		}
		t.Rows = append(t.Rows, row)
	}
	accounts := make([]string, 0, len(totals))
	for account := range totals {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)
	for _, account := range accounts {
		row := totals[account].row("TOTAL", "all", scales[account])
		if perAccount {
			row = append([]string{accountCell(account)}, row...)
		}
		t.Rows = append(t.Rows, row)
	}
	return t, nil
}
