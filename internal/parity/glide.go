package parity

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/glide"
)

// The glide-path checks (docs/research/parity-2026-10.md, "Remaining scope,
// plan, 2026-10-19").
const (
	CheckGlideMonths  = "glide-months"
	CheckGlideExport  = "glide-export"
	CheckGlideAge     = "glide-age"
	CheckGlideShare   = "glide-share"
	CheckGlideWeights = "glide-weights"
	CheckGlideSleeves = "glide-sleeves"
)

// The glide modes of research/edge-search/momentum/glide_path.py: the
// schedule from a starting age, or a fixed share.
const (
	GlidePath  = "path"
	GlideFixed = "fixed"
)

// GlideMonth is one logged rebalance of a QuantConnect glide-path run
// (research/edge-search/momentum/glide_parity_log.py): the owner's age the
// bond share was read at, in years, the share, the momentum book and the
// combined targets traded. A path run's age is StartAge on the first day
// of MeasureFrom (YYYY-MM), plus the whole months since.
type GlideMonth struct {
	Month       string
	Date        string
	Glide       string
	StartAge    float64
	MeasureFrom string
	Age         float64
	B           float64
	Book        map[string]float64
	Targets     map[string]float64
}

// CompareGlide checks each logged rebalance of a glide-path run against the
// Go glide module given the run's own inputs: the owner's age in months
// (glide.AgeMonths, from the birth month the run implies) against the age
// the run read, and the bond share glide.BondShare gives at it on anchors
// (the runs used glide.DefaultAnchors) against the
// share the run used, both within WeightTolerance; and glide.Combine of the
// run's momentum book at the run's share against the run's combined
// targets, every instrument, within WeightTolerance (docs/research/
// parity-2026-10.md, "What Go recomputes, and the thresholds"; ADR 0026
// point 7, item 2). An
// instrument both sleeves hold is counted again under glide-sleeves, Go's
// two shares summed, so the report shows that the summing was exercised. A
// fixed-mix month has no schedule to check and is compared on its weights.
//
// Every planned month must be exported once; a month the run logged beyond
// them is compared too.
func CompareGlide(anchors []glide.Anchor, run string, planned []string, months []GlideMonth) Report {
	checks := map[string]*Check{}
	var order []*Check
	for _, c := range []struct{ name, threshold string }{
		{CheckGlideMonths, "every planned month exported once"},
		{CheckGlideExport, "well-formed"},
		{CheckGlideAge, fmt.Sprintf("months / 12, abs <= %g", WeightTolerance)},
		{CheckGlideShare, fmt.Sprintf("abs <= %g", WeightTolerance)},
		{CheckGlideWeights, fmt.Sprintf("instruments exact; abs <= %g", WeightTolerance)},
		{CheckGlideSleeves, fmt.Sprintf("abs <= %g", WeightTolerance)},
	} {
		checks[c.name] = &Check{Run: run, Name: c.name, Threshold: c.threshold}
		order = append(order, checks[c.name])
	}
	plannedMonths(checks[CheckGlideMonths], planned, monthsOf(months, func(m GlideMonth) string { return m.Month }))
	for i := range months {
		glideMonth(anchors, checks, &months[i])
	}
	var out Report
	for _, c := range order {
		out.Checks = append(out.Checks, *c)
	}
	return out
}

// plannedMonths checks that every planned month was exported exactly once
// and that no month was exported twice. A month exported once but not
// planned is a month the run logged, and is not a failure.
func plannedMonths(c *Check, planned []string, got map[string]int) {
	want := map[string]bool{}
	for _, p := range planned {
		want[p] = true
		c.compare(got[p] == 1, 0, fmt.Sprintf("planned month %s exported %d times", p, got[p]))
	}
	extra := make([]string, 0, len(got))
	for m, n := range got {
		if !want[m] && n > 1 {
			extra = append(extra, m)
		}
	}
	sort.Strings(extra)
	for _, m := range extra {
		c.compare(false, 0, fmt.Sprintf("month %s exported %d times", m, got[m]))
	}
}

func monthsOf[T any](ms []T, month func(T) string) map[string]int {
	got := map[string]int{}
	for _, m := range ms {
		got[month(m)]++
	}
	return got
}

func glideMonth(anchors []glide.Anchor, checks map[string]*Check, m *GlideMonth) {
	ageMonths, err := wellFormedGlide(m)
	checks[CheckGlideExport].compare(err == nil, 0, fmt.Sprintf("%s: %v", m.Month, err))
	if err != nil {
		return
	}
	if m.Glide == GlidePath {
		d := math.Abs(float64(ageMonths)/12 - m.Age)
		checks[CheckGlideAge].compare(d <= WeightTolerance, d,
			fmt.Sprintf("%s: Go %d months, QuantConnect %v years", m.Month, ageMonths, m.Age))
		b, err := glide.BondShare(anchors, ageMonths)
		if err != nil {
			checks[CheckGlideExport].fail(fmt.Sprintf("%s: %v", m.Month, err))
			return
		}
		d = math.Abs(b - m.B)
		checks[CheckGlideShare].compare(d <= WeightTolerance, d, fmt.Sprintf("%s: Go %v, QuantConnect %v", m.Month, b, m.B))
	}
	book := make([]glide.Weight, 0, len(m.Book))
	for _, s := range sortedKeys(m.Book) {
		book = append(book, glide.Weight{Symbol: s, Weight: m.Book[s]})
	}
	combined, err := glide.Combine(book, m.B, glide.DefaultBondFunds())
	if err != nil {
		checks[CheckGlideExport].fail(fmt.Sprintf("%s: %v", m.Month, err))
		return
	}
	goW := map[string]float64{}
	for _, t := range combined {
		goW[t.Symbol] = t.Weight
		if len(t.Shares) == 2 {
			q := m.Targets[t.Symbol]
			d := math.Abs(t.Shares[0].Weight + t.Shares[1].Weight - q)
			checks[CheckGlideSleeves].compare(d <= WeightTolerance, d,
				fmt.Sprintf("%s %s: Go %v + %v, QuantConnect %v", m.Month, t.Symbol, t.Shares[0].Weight, t.Shares[1].Weight, q))
		}
	}
	c := checks[CheckGlideWeights]
	for _, n := range union(goW, m.Targets) {
		g, inGo := goW[n]
		q, inQC := m.Targets[n]
		d := math.Abs(g - q)
		c.compare(inGo && inQC && d <= WeightTolerance, d,
			fmt.Sprintf("%s %s: Go %v (set %v), QuantConnect %v (set %v)", m.Month, n, g, inGo, q, inQC))
	}
}

// wellFormedGlide checks the month's fields and, for a path run, returns
// the owner's age in months on its date from the birth month the run
// implies: MeasureFrom less StartAge years.
func wellFormedGlide(m *GlideMonth) (int, error) {
	day, err := time.Parse(time.DateOnly, m.Date)
	if err != nil {
		return 0, fmt.Errorf("date %q", m.Date)
	}
	if day.Format("2006-01") != m.Month {
		return 0, fmt.Errorf("date %s is not in month %s", m.Date, m.Month)
	}
	if !finiteIn(m.B, 0, 1) {
		return 0, fmt.Errorf("bond share %v outside [0, 1]", m.B)
	}
	if len(m.Book) == 0 || len(m.Targets) == 0 {
		return 0, errors.New("no book or no targets")
	}
	for s, w := range m.Book {
		if !finiteIn(w, 0, 1) {
			return 0, fmt.Errorf("book weight %s %v", s, w)
		}
	}
	switch m.Glide {
	case GlideFixed:
		return 0, nil
	case GlidePath:
	default:
		return 0, fmt.Errorf("glide mode %q", m.Glide)
	}
	start, err := time.Parse("2006-01", m.MeasureFrom)
	if err != nil || start.Format("2006-01") != m.MeasureFrom {
		return 0, fmt.Errorf("measure-from month %q", m.MeasureFrom)
	}
	if m.StartAge != math.Trunc(m.StartAge) || m.StartAge < 0 || m.StartAge > 200 {
		return 0, fmt.Errorf("start age %v is not whole years", m.StartAge)
	}
	birth := start.AddDate(-int(m.StartAge), 0, 0).Format("2006-01")
	return glide.AgeMonths(birth, day)
}

func finiteIn(x, lo, hi float64) bool {
	return !math.IsNaN(x) && x >= lo && x <= hi
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type glidePayload struct {
	Month       *string             `json:"month"`
	Date        *string             `json:"date"`
	Glide       *string             `json:"glide"`
	StartAge    *float64            `json:"start_age"`
	MeasureFrom *string             `json:"measure_from"`
	Age         *float64            `json:"age"`
	B           *float64            `json:"b"`
	Book        map[string]*float64 `json:"book"`
	Targets     map[string]*float64 `json:"targets"`
}

// DecodeGlide decodes one GLIDE1 payload, refusing an unknown field, a
// required field missing or null, and a null weight.
func DecodeGlide(raw []byte) (GlideMonth, error) {
	var p glidePayload
	if err := strictDecode(raw, &p); err != nil {
		return GlideMonth{}, err
	}
	if p.Month == nil || *p.Month == "" {
		return GlideMonth{}, errors.New("parity: glide payload has no month")
	}
	for _, f := range []struct {
		name    string
		present bool
	}{
		{"date", p.Date != nil}, {"glide", p.Glide != nil}, {"start_age", p.StartAge != nil},
		{"measure_from", p.MeasureFrom != nil}, {"age", p.Age != nil}, {"b", p.B != nil},
		{"book", p.Book != nil}, {"targets", p.Targets != nil},
	} {
		if !f.present {
			return GlideMonth{}, fmt.Errorf("parity: %s: %s missing or null", *p.Month, f.name)
		}
	}
	book, err := required(*p.Month, "book", p.Book)
	if err != nil {
		return GlideMonth{}, err
	}
	targets, err := required(*p.Month, "targets", p.Targets)
	if err != nil {
		return GlideMonth{}, err
	}
	return GlideMonth{Month: *p.Month, Date: *p.Date, Glide: *p.Glide, StartAge: *p.StartAge,
		MeasureFrom: *p.MeasureFrom, Age: *p.Age, B: *p.B, Book: book, Targets: targets}, nil
}
