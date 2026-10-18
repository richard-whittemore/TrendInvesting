package parity_test

import (
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/parity"
)

// The comparison never puts a mismatch down to a cause. It recomputes the
// book under the backtest's own handling of each listed case and compares
// it with QuantConnect's export exactly; the live rule's departures from
// that book are listed apart, each classified by cause.

// inBookTie gives the second and third picks of the second month one
// score, in the order Go's live rule would not choose (it breaks ties by
// ascending ID). Both are picked on both sides.
func inBookTie(t *testing.T, ms []parity.MomentumMonth) {
	t.Helper()
	m := &ms[1]
	a, b := m.Ranked[1], m.Ranked[2]
	if a.Mark != parity.MarkPicked || b.Mark != parity.MarkPicked {
		t.Fatal("fixture: ranks 2 and 3 must be picks")
	}
	tieNames(m, &a, &b)
	if a.Ticker < b.Ticker {
		a, b = b, a
	}
	m.Ranked[1], m.Ranked[2] = a, b
	markAndWeigh(m)
}

// lastPick and firstBeyond return the index of the second month's last
// pick and of its first name beyond the book.
func lastPick(m *parity.MomentumMonth) int {
	last := -1
	for i, e := range m.Ranked {
		if e.Mark == parity.MarkPicked {
			last = i
		}
	}
	return last
}

func firstBeyond(m *parity.MomentumMonth) int {
	for i, e := range m.Ranked {
		if e.Mark == parity.MarkBeyond {
			return i
		}
	}
	return -1
}

func TestACapDefectAfterANoSectorPickFailsCapMarks(t *testing.T) {
	ms := months()
	m := &ms[1]
	m.Ranked[10].Sector = "" // a pick in QuantConnect's book, uncapped
	markAndWeigh(m)
	p := lastPick(m)
	if m.Ranked[10].Mark != parity.MarkPicked || p <= 10 {
		t.Fatal("fixture: rank 11 must be a pick with picks after it")
	}
	// A name QuantConnect picked after the no-sector pick is marked capped,
	// as a Go cap defect would mark it.
	m.Ranked[p].Mark = parity.MarkCapped
	delete(m.Weights, m.Ranked[p].Ticker)
	r := compare(ms)
	if c := checkNamed(r, parity.CheckCapMarks); c.Pass() {
		t.Errorf("a mark after a no-sector pick is compared, so the changed one must fail: %+v", c)
	}
	if c := checkNamed(r, parity.CheckWeights); c.Pass() {
		t.Errorf("the picked sets differ, so the weights must fail: %+v", c)
	}
}

func TestAnUnrelatedPickInAMonthWithAnInBookTieFailsWeights(t *testing.T) {
	ms := months()
	inBookTie(t, ms)
	m := &ms[1]
	p, q := lastPick(m), firstBeyond(m)
	// The last pick is swapped for the first name beyond the book: nothing
	// the tie can explain.
	w := m.Weights[m.Ranked[p].Ticker]
	delete(m.Weights, m.Ranked[p].Ticker)
	m.Ranked[p].Mark, m.Ranked[q].Mark = parity.MarkBeyond, parity.MarkPicked
	m.Weights[m.Ranked[q].Ticker] = w
	r := compare(ms)
	if c := checkNamed(r, parity.CheckWeights); c.Pass() {
		t.Errorf("an unrelated pick must fail the weights though the month holds an in-book tie: %+v", c)
	}
	if c := checkNamed(r, parity.CheckCapMarks); c.Pass() {
		t.Errorf("and its marks: %+v", c)
	}
}

func TestAZeroVolatilityNameQuantConnectCappedExcusesNothing(t *testing.T) {
	ms := months()
	m := &ms[1]
	if m.Ranked[3].Mark != parity.MarkCapped || m.Ranked[5].Mark != parity.MarkCapped {
		t.Fatal("fixture: ranks 4 and 6 must be capped")
	}
	zeroVolatility(m, &m.Ranked[3])
	m.Ranked[5].Mark = parity.MarkBeyond // Go caps it
	r := compare(ms)
	if c := checkNamed(r, parity.CheckCapMarks); c.Pass() {
		t.Errorf("a capped zero volatility changes nothing after it, so the changed mark must fail: %+v", c)
	}
}

// classified asserts that the report's differences are all of kind, that
// one names where, and that there are at least n.
func classified(t *testing.T, r parity.Report, kind parity.Kind, where string, n int) {
	t.Helper()
	found := false
	for _, d := range r.Differences {
		if d.Kind != kind || d.Size == "" {
			t.Errorf("difference %+v, want kind %s", d, kind)
		}
		found = found || strings.Contains(d.Where, where)
	}
	if !found || len(r.Differences) < n {
		t.Errorf("want at least %d %s differences, one at %q: %+v", n, kind, where, r.Differences)
	}
}

// everyItemCompared asserts that the book checks compared every name and
// every target of both months and passed.
func everyItemCompared(t *testing.T, r parity.Report) {
	t.Helper()
	for name, n := range map[string]int{parity.CheckRankedOrder: 80, parity.CheckCapMarks: 80, parity.CheckWeights: 51} {
		if c := checkNamed(r, name); c.Compared != n || !c.Pass() {
			t.Errorf("%s: compared %d, want %d, and pass: %+v", name, c.Compared, n, c)
		}
	}
}

func TestANoSectorPickIsComparedAsTheBacktestHandledItAndItsLiveDeparturesListed(t *testing.T) {
	ms := months()
	m := &ms[1]
	m.Ranked[10].Sector = ""
	markAndWeigh(m)
	ticker := m.Ranked[10].Ticker
	r := compare(ms)
	everyItemCompared(t, r)
	failsOnly(t, r, parity.CheckDifferences)
	// The name itself, the name that takes its place, and the weights.
	classified(t, r, parity.KindNoSector, ticker, 3)
	for _, d := range r.Differences {
		if d.Listed() {
			t.Errorf("no-sector is not on ADR 0026 point 8's list: %+v", d)
		}
	}
}

func TestAZeroVolatilityPickIsComparedAsTheBacktestWeightedItAndItsLiveDeparturesListed(t *testing.T) {
	ms := months()
	m := &ms[1]
	zeroVolatility(m, &m.Ranked[0])
	markAndWeigh(m)
	r := compare(ms)
	everyItemCompared(t, r)
	if !r.Pass() {
		t.Errorf("a zero volatility is on the list: %+v", r.Checks)
	}
	classified(t, r, parity.KindZeroVolatility, m.Ranked[0].Ticker, 3)
}

func TestAnInBookTieIsComparedInTheBacktestsOrderAndItsLiveDeparturesListed(t *testing.T) {
	ms := months()
	inBookTie(t, ms)
	r := compare(ms)
	everyItemCompared(t, r)
	if !r.Pass() {
		t.Errorf("a tie is on the list: %+v", r.Checks)
	}
	classified(t, r, parity.KindTie, "rank 2", 2)
}
