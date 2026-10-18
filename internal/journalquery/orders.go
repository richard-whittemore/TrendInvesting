package journalquery

import (
	"fmt"
	"strconv"
	"strings"
)

// Orders lists the orders the run planned, released and refused, and the
// order lifecycle changes it recorded, for the records that pass f.
func Orders(journals []Journal, f Filter) (Table, error) {
	if err := distinct(journals); err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"journal", "seq", "time", "account", "instrument", "event", "side", "quantity", "price", "note"}}
	for _, j := range journals {
		items, err := decode(j)
		if err != nil {
			return Table{}, err
		}
		for _, it := range items {
			var instrument, event, side, note string
			var quantity int64
			var price float64
			switch {
			case it.planned != nil:
				p := it.planned
				instrument, event, side, quantity, price, note = p.InstrumentID, "planned", p.Side, p.Quantity, p.LimitPrice, p.Tag
			case it.released != nil:
				p := it.released
				instrument, event, side, quantity, price = p.InstrumentID, "released", p.Side, p.Quantity, p.LimitPrice
				note = fmt.Sprintf("mode=%s,send=%t", p.Mode, p.Send)
			case it.refused != nil:
				p := it.refused
				instrument, event, side, quantity, price, note = p.InstrumentID, "refused", p.Side, p.Quantity, p.LimitPrice, p.Reason
			case it.lifecycle != nil:
				p := it.lifecycle
				instrument, event, side, quantity, price, note = p.InstrumentID, "lifecycle-"+p.Status, p.Side, p.Quantity, p.Price, p.Note
				if p.Synthetic {
					// A dry-run engine's own report, never a venue's.
					event = "synthetic-" + event
				}
			default:
				continue
			}
			if !f.keep(it, instrument) {
				continue
			}
			t.Rows = append(t.Rows, []string{
				it.journal, strconv.FormatUint(it.seq, 10), it.at.UTC().Format("2006-01-02"), accountCell(it.account), instrument,
				event, dash(side), strconv.FormatInt(quantity, 10), money(price), note,
			})
		}
	}
	return t, nil
}

// Refusals lists the orders the risk gate refused, the sales back to 98% it
// withheld (a lapse included), and the engine-state changes (degraded,
// halted) for the records that pass f. An engine-state
// change with no instrument scope covers the whole engine, so an instrument
// filter keeps it.
func Refusals(journals []Journal, f Filter) (Table, error) {
	if err := distinct(journals); err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"journal", "seq", "time", "account", "instrument", "event", "reason", "detail"}}
	for _, j := range journals {
		items, err := decode(j)
		if err != nil {
			return Table{}, err
		}
		for _, it := range items {
			var row []string
			switch {
			case it.refused != nil:
				p := it.refused
				if !f.keep(it, p.InstrumentID) {
					continue
				}
				row = []string{p.InstrumentID, "refused", p.Reason, p.Detail}
			case it.withheld != nil:
				p := it.withheld
				if !f.keep(it, p.InstrumentID) {
					continue
				}
				row = []string{p.InstrumentID, "sale-withheld", p.Reason, p.Detail}
			case it.engine != nil:
				p := it.engine
				if !engineInScope(p.ScopeInstrumentIDs, f.Instrument) || !f.keepAccount(it) || !f.keepTime(it.at) {
					continue
				}
				row = []string{dash(strings.Join(p.ScopeInstrumentIDs, ";")), "engine-state", p.State + ":" + p.Reason, p.Detail}
			default:
				continue
			}
			t.Rows = append(t.Rows, append([]string{
				it.journal, strconv.FormatUint(it.seq, 10), it.at.UTC().Format("2006-01-02"), accountCell(it.account),
			}, row...))
		}
	}
	return t, nil
}

func engineInScope(scope []string, instrument string) bool {
	if instrument == "" || len(scope) == 0 {
		return true
	}
	for _, id := range scope {
		if id == instrument {
			return true
		}
	}
	return false
}

func accountCell(account string) string { return dash(account) }

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
