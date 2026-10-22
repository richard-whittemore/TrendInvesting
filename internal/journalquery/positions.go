package journalquery

import (
	"fmt"
	"sort"
	"strconv"
)

// step is the position in one instrument after one fill.
type step struct {
	it         item
	instrument string
	change     int64
	position   int64
}

// check compares what the fills since the previous snapshot imply with what
// the next snapshot reports, for one instrument.
type check struct {
	it         item
	instrument string
	fills      int64
	snapshot   int64
}

// addQuantity adds two position quantities and reports false rather than
// wrapping when the sum does not fit an int64, so a figure that cannot be
// represented is refused instead of reported wrong.
func addQuantity(a, b int64) (int64, bool) {
	sum := a + b
	if (b > 0 && sum < a) || (b < 0 && sum > a) {
		return 0, false
	}
	return sum, true
}

// rebuild replays a journal's fills against its account snapshots, one book
// per account.
//
// An account's first snapshot is its baseline, taking the account's holdings
// as the broker reported them, unless a fill came first, in which case the
// book started empty and that snapshot is compared like any other. Every
// later snapshot is compared with the book the fills built and then becomes
// the book, so one disagreement is reported once rather than carried into
// every later comparison.
//
// A Turtle fill (version 4) names no account. It belongs to its journal's
// single account; a journal naming several accounts has no way to say which
// one such a fill hit, so rebuild refuses it rather than guess. A
// target-weight fill (version 5) names its own account.
func rebuild(j Journal) (steps []step, checks []check, trailing []step, err error) {
	items, err := decode(j)
	if err != nil {
		return nil, nil, nil, err
	}
	accounts := namedAccounts(items)
	books := map[string]map[string]int64{}
	started := map[string]bool{}
	pending := map[string][]step{}
	for _, it := range items {
		switch {
		case it.fill != nil:
			if it.account == "" && len(accounts) > 1 {
				return nil, nil, nil, fmt.Errorf("%s: record %d: the fill cannot be attributed to an account: the journal names %d accounts and a fill names none", j.Name, it.seq, len(accounts))
			}
			book := books[it.account]
			if book == nil {
				book = map[string]int64{}
				books[it.account] = book
			}
			started[it.account] = true
			change := signedQuantity(it.fill.Kind, it.fill.Quantity)
			sum, ok := addQuantity(book[it.fill.InstrumentID], change)
			if !ok {
				return nil, nil, nil, fmt.Errorf("%s: record %d: the fill takes %s past the largest position that can be reported (int64 overflow)", j.Name, it.seq, it.fill.InstrumentID)
			}
			book[it.fill.InstrumentID] = sum
			s := step{it, it.fill.InstrumentID, change, book[it.fill.InstrumentID]}
			steps = append(steps, s)
			pending[it.account] = append(pending[it.account], s)
		case it.renamed != nil:
			// The holding continues under the new id without a fill (ADR
			// 0024), in every account that holds it.
			for _, book := range books {
				if q, ok := book[it.renamed.InstrumentID]; ok {
					sum, ok := addQuantity(book[it.renamed.NewInstrumentID], q)
					if !ok {
						return nil, nil, nil, fmt.Errorf("%s: record %d: renaming %s to %s overflows the position quantity (int64 overflow)", j.Name, it.seq, it.renamed.InstrumentID, it.renamed.NewInstrumentID)
					}
					book[it.renamed.NewInstrumentID] = sum
					delete(book, it.renamed.InstrumentID)
				}
			}
		case it.snapshot != nil:
			reported := map[string]int64{}
			for _, p := range it.snapshot.Positions {
				reported[p.InstrumentID] = p.Quantity
			}
			if started[it.account] {
				ids := map[string]bool{}
				for id := range books[it.account] {
					ids[id] = true
				}
				for id := range reported {
					ids[id] = true
				}
				sorted := make([]string, 0, len(ids))
				for id := range ids {
					sorted = append(sorted, id)
				}
				sort.Strings(sorted)
				for _, id := range sorted {
					checks = append(checks, check{it, id, books[it.account][id], reported[id]})
				}
				if len(sorted) == 0 {
					// No holdings against no holdings is still a comparison:
					// an all-cash account's later snapshot checked something.
					checks = append(checks, check{it: it, instrument: noHoldings})
				}
			}
			delete(pending, it.account)
			started[it.account] = true
			books[it.account] = reported
		}
	}
	for _, fills := range pending {
		trailing = append(trailing, fills...)
	}
	sort.Slice(trailing, func(a, b int) bool { return trailing[a].it.seq < trailing[b].it.seq })
	return steps, checks, trailing, nil
}

// noHoldings stands in the instrument column of a comparison in which both
// the fills and the snapshot hold nothing.
const noHoldings = "-"

// Positions lists each fill with the position it leaves in its instrument,
// rebuilt from the account's baseline snapshot. The running position never
// depends on f: a filter only chooses which rows to show.
func Positions(journals []Journal, f Filter) (Table, error) {
	if err := distinct(journals); err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"journal", "seq", "time", "account", "instrument", "kind", "change", "position"}}
	for _, j := range journals {
		steps, _, _, err := rebuild(j)
		if err != nil {
			return Table{}, err
		}
		for _, s := range steps {
			if !f.keep(s.it, s.instrument) {
				continue
			}
			t.Rows = append(t.Rows, []string{
				s.it.journal, strconv.FormatUint(s.it.seq, 10), s.it.at.UTC().Format("2006-01-02"), accountCell(s.it.account),
				s.instrument, s.it.fill.Kind, strconv.FormatInt(s.change, 10), strconv.FormatInt(s.position, 10),
			})
		}
	}
	return t, nil
}

// DisagreementError reports fills and snapshots that do not agree. Reconcile
// returns it together with the full table, so the rows are never hidden.
// Trailing counts the fills that no snapshot has covered yet, when they were
// not allowed.
type DisagreementError struct{ Count, Trailing int }

func (e *DisagreementError) Error() string {
	msg := fmt.Sprintf("%d disagreement(s) between the fills and the account snapshots", e.Count)
	if e.Trailing > 0 {
		msg += fmt.Sprintf("; %d fills not yet covered by a snapshot", e.Trailing)
	}
	return msg
}

// TrailingFillsError reports fills recorded after the last snapshot of their
// account. Reconcile returns it together with the full table: what was
// compared agrees, but a pass would cover fills no snapshot has checked.
type TrailingFillsError struct{ Count int }

func (e *TrailingFillsError) Error() string {
	return fmt.Sprintf("%d fills not yet covered by a snapshot: the comparisons above agree, but those fills have not been checked against the broker's positions (use -allow-trailing-fills to accept this)", e.Count)
}

// Reconcile compares, at every account snapshot after the account's first,
// the positions the fills imply with the positions the snapshot reports. It
// lists every comparison and, when any disagree, returns a *DisagreementError
// along with the table. A check that compared nothing is not a pass: a
// journal that is empty or cut short, or one with no snapshot after its
// baseline in the selection, is an error in words and no table (ADR 0017,
// ADR 0027). Fills recorded after an account's last snapshot are not yet
// checked, so they are reported as a *TrailingFillsError with the table unless
// f.AllowTrailingFills accepts them.
func Reconcile(journals []Journal, f Filter) (Table, error) {
	if err := distinct(journals); err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"journal", "seq", "time", "account", "instrument", "fills", "snapshot", "status"}}
	disagreements, trailing := 0, 0
	for _, j := range journals {
		if why := j.Incomplete(); why != "" {
			return Table{}, fmt.Errorf("reconcile refused: %s", why)
		}
	}
	for _, j := range journals {
		_, checks, tail, err := rebuild(j)
		if err != nil {
			return Table{}, err
		}
		compared := 0
		for _, c := range checks {
			if !f.keep(c.it, c.instrument) {
				continue
			}
			compared++
			status := "ok"
			if c.fills != c.snapshot {
				status = "DISAGREES"
				disagreements++
			}
			t.Rows = append(t.Rows, []string{
				c.it.journal, strconv.FormatUint(c.it.seq, 10), c.it.at.UTC().Format("2006-01-02"), accountCell(c.it.account),
				c.instrument, strconv.FormatInt(c.fills, 10), strconv.FormatInt(c.snapshot, 10), status,
			})
		}
		if compared == 0 {
			return Table{}, fmt.Errorf("reconcile compared nothing in %s: no account snapshot after the baseline met the selection, so no fills there were checked against the broker's positions", j.Name)
		}
		for _, s := range tail {
			if f.keep(s.it, s.instrument) {
				trailing++
			}
		}
	}
	if f.AllowTrailingFills {
		trailing = 0
	}
	if disagreements > 0 {
		return t, &DisagreementError{Count: disagreements, Trailing: trailing}
	}
	if trailing > 0 {
		return t, &TrailingFillsError{Count: trailing}
	}
	return t, nil
}
