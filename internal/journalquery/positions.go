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
// A fill names no account. It belongs to its journal's single account; a
// journal naming several accounts has no way to say which one a fill hit, so
// rebuild refuses it rather than guess.
func rebuild(j Journal) ([]step, []check, error) {
	items, err := decode(j)
	if err != nil {
		return nil, nil, err
	}
	accounts := map[string]bool{}
	for _, it := range items {
		if it.snapshot != nil || it.planned != nil || it.released != nil || it.refused != nil {
			accounts[it.account] = true
		}
	}
	books := map[string]map[string]int64{}
	started := map[string]bool{}
	var steps []step
	var checks []check
	for _, it := range items {
		switch {
		case it.fill != nil:
			if it.account == "" && len(accounts) > 1 {
				return nil, nil, fmt.Errorf("%s: record %d: the fill cannot be attributed to an account: the journal names %d accounts and a fill names none", j.Name, it.seq, len(accounts))
			}
			book := books[it.account]
			if book == nil {
				book = map[string]int64{}
				books[it.account] = book
			}
			started[it.account] = true
			change := signedQuantity(it.fill.Kind, it.fill.Quantity)
			book[it.fill.InstrumentID] += change
			steps = append(steps, step{it, it.fill.InstrumentID, change, book[it.fill.InstrumentID]})
		case it.renamed != nil:
			// The holding continues under the new id without a fill (ADR
			// 0024), in every account that holds it.
			for _, book := range books {
				if q, ok := book[it.renamed.InstrumentID]; ok {
					book[it.renamed.NewInstrumentID] += q
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
			}
			started[it.account] = true
			books[it.account] = reported
		}
	}
	return steps, checks, nil
}

// Positions lists each fill with the position it leaves in its instrument,
// rebuilt from the account's baseline snapshot. The running position never
// depends on f: a filter only chooses which rows to show.
func Positions(journals []Journal, f Filter) (Table, error) {
	if err := distinct(journals); err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"journal", "seq", "time", "account", "instrument", "kind", "change", "position"}}
	for _, j := range journals {
		steps, _, err := rebuild(j)
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
type DisagreementError struct{ Count int }

func (e *DisagreementError) Error() string {
	return fmt.Sprintf("%d disagreement(s) between the fills and the account snapshots", e.Count)
}

// Reconcile compares, at every account snapshot after the account's first,
// the positions the fills imply with the positions the snapshot reports. It
// lists every comparison and, when any disagree, returns a *DisagreementError
// along with the table.
func Reconcile(journals []Journal, f Filter) (Table, error) {
	if err := distinct(journals); err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"journal", "seq", "time", "account", "instrument", "fills", "snapshot", "status"}}
	disagreements := 0
	for _, j := range journals {
		_, checks, err := rebuild(j)
		if err != nil {
			return Table{}, err
		}
		for _, c := range checks {
			if !f.keep(c.it, c.instrument) {
				continue
			}
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
	}
	if disagreements > 0 {
		return t, &DisagreementError{Count: disagreements}
	}
	return t, nil
}
