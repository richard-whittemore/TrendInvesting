package parity_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/parity"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/glide"
)

// glideShare restates glide_path.py's glide_share, the schedule the
// QuantConnect runs read the bond share from.
func glideShare(age float64) float64 {
	switch {
	case age <= 38:
		return 0
	case age <= 53:
		return 0.308 * (age - 38) / 15
	case age <= 65:
		return 0.308 + float64(0.016*(age-53))
	case age <= 72:
		return 0.50 + 0.20*(age-65)/7
	}
	return 0.70
}

// withBondSleeve restates glide_path.py's with_bond_sleeve.
func withBondSleeve(book map[string]float64, b float64) map[string]float64 {
	out := map[string]float64{}
	if b == 0 {
		for k, v := range book {
			out[k] = v
		}
		return out
	}
	for k, v := range book {
		out[k] = float64(v * (1 - b))
	}
	for _, f := range []struct {
		sym  string
		frac float64
	}{{"IEF", 0.5}, {"SHY", 0.5}} {
		out[f.sym] += float64(float64(0.98*b) * f.frac)
	}
	return out
}

// glideMonths is a path run started at 53 on 2002-08, logged in 2010-01 to
// 2010-03; 2010-02 is a risk-off month whose momentum book holds IEF, so
// both sleeves hold it.
func glideMonths() []parity.GlideMonth {
	var out []parity.GlideMonth
	for i, month := range []string{"2010-01", "2010-02", "2010-03"} {
		age := 53 + float64(89+i)/12
		b := glideShare(age)
		book := map[string]float64{"AAA": 0.5, "BBB": 0.48}
		if i == 1 {
			book = map[string]float64{"AAA": 0.25, "BBB": 0.24, "IEF": 0.49}
		}
		out = append(out, parity.GlideMonth{Month: month, Date: month + "-01", Glide: "path", StartAge: 53,
			MeasureFrom: "2002-08", Age: age, B: b, Book: book, Targets: withBondSleeve(book, b)})
	}
	return out
}

var glidePlan = []string{"2010-01", "2010-02", "2010-03"}

func compareGlide(ms []parity.GlideMonth) parity.Report {
	return parity.CompareGlide(glide.DefaultAnchors(), "G", glidePlan, ms)
}

func TestAMatchingGlideExportPassesEveryCheck(t *testing.T) {
	r := compareGlide(glideMonths())
	if !r.Pass() {
		t.Fatalf("matching glide export failed:\n%s", r.Markdown())
	}
	want := map[string]int{
		parity.CheckGlideMonths: 3, parity.CheckGlideExport: 3, parity.CheckGlideAge: 3, parity.CheckGlideShare: 3,
		parity.CheckGlideWeights: 4 + 4 + 4, parity.CheckGlideSleeves: 1,
	}
	if len(r.Checks) != len(want) {
		t.Fatalf("checks %v", r.Checks)
	}
	for name, n := range want {
		if c := checkNamed(r, name); c.Compared != n {
			t.Errorf("%s compared %d, want %d", name, c.Compared, n)
		}
	}
}

func glideFailsOnly(t *testing.T, ms []parity.GlideMonth, names ...string) {
	t.Helper()
	r := compareGlide(ms)
	failing := map[string]bool{}
	for _, n := range names {
		failing[n] = true
	}
	for _, c := range r.Checks {
		if c.Pass() == failing[c.Name] {
			t.Errorf("%s: pass=%v, want %v (%s)", c.Name, c.Pass(), !failing[c.Name], c.First)
		}
	}
}

func TestAShareOffByMoreThanTheToleranceFails(t *testing.T) {
	ms := glideMonths()
	ms[2].B += 1e-9
	ms[2].Targets = withBondSleeve(ms[2].Book, ms[2].B)
	glideFailsOnly(t, ms, parity.CheckGlideShare)
}

func TestAnAgeAMonthOffFails(t *testing.T) {
	ms := glideMonths()
	ms[0].Age += 1.0 / 12
	ms[0].B = glideShare(ms[0].Age)
	ms[0].Targets = withBondSleeve(ms[0].Book, ms[0].B)
	glideFailsOnly(t, ms, parity.CheckGlideAge, parity.CheckGlideShare)
}

func TestACombinedWeightOffOrMissingFailsWeights(t *testing.T) {
	ms := glideMonths()
	ms[0].Targets["AAA"] += 1e-11
	glideFailsOnly(t, ms, parity.CheckGlideWeights)
	ms = glideMonths()
	delete(ms[2].Targets, "SHY")
	glideFailsOnly(t, ms, parity.CheckGlideWeights)
	ms = glideMonths()
	ms[2].Targets["TLT"] = 0.01
	glideFailsOnly(t, ms, parity.CheckGlideWeights)
}

func TestIEFNotSummedAcrossTheSleevesFails(t *testing.T) {
	// QuantConnect's IEF holding only the momentum sleeve's share.
	ms := glideMonths()
	ms[1].Targets["IEF"] = float64(0.49 * (1 - ms[1].B))
	glideFailsOnly(t, ms, parity.CheckGlideWeights, parity.CheckGlideSleeves)
}

func TestARunWithNoInstrumentInBothSleevesFailsSleeves(t *testing.T) {
	ms := glideMonths()
	ms = append(ms[:1], ms[2:]...)
	r := parity.CompareGlide(glide.DefaultAnchors(), "G", []string{"2010-01", "2010-03"}, ms)
	if c := checkNamed(r, parity.CheckGlideSleeves); c.Pass() || c.Compared != 0 {
		t.Errorf("sleeves %+v", c)
	}
}

func TestAMissingPlannedGlideMonthFailsAndAnUnplannedOneIsCompared(t *testing.T) {
	ms := glideMonths()
	glideFailsOnly(t, ms[1:], parity.CheckGlideMonths)
	r := parity.CompareGlide(glide.DefaultAnchors(), "G", glidePlan[1:], ms)
	if !r.Pass() || checkNamed(r, parity.CheckGlideAge).Compared != 3 {
		t.Errorf("an unplanned month the run logged is compared and passes:\n%s", r.Markdown())
	}
}

func TestAMalformedGlideMonthFailsTheExport(t *testing.T) {
	for name, alter := range map[string]func(*parity.GlideMonth){
		"date in another month": func(m *parity.GlideMonth) { m.Date = "2010-02-01" },
		"bad date":              func(m *parity.GlideMonth) { m.Date = "2010-01-xx" },
		"unknown mode":          func(m *parity.GlideMonth) { m.Glide = "none" },
		"fractional start age":  func(m *parity.GlideMonth) { m.StartAge = 53.5 },
		"bad measure month":     func(m *parity.GlideMonth) { m.MeasureFrom = "2002-8" },
		"share above one":       func(m *parity.GlideMonth) { m.B = 1.5 },
		"negative weight":       func(m *parity.GlideMonth) { m.Book["AAA"] = -0.1 },
		"no book":               func(m *parity.GlideMonth) { m.Book = nil },
		"implausible birth":     func(m *parity.GlideMonth) { m.StartAge = 5 },
		"book above 98%":        func(m *parity.GlideMonth) { m.Book["BBB"] = 0.49 },
	} {
		t.Run(name, func(t *testing.T) {
			ms := glideMonths()
			alter(&ms[0])
			r := compareGlide(ms)
			if c := checkNamed(r, parity.CheckGlideExport); c.Pass() {
				t.Errorf("export passed: %+v", c)
			}
		})
	}
	ms := glideMonths()
	ms = append(ms, ms[0])
	if c := checkNamed(compareGlide(ms), parity.CheckGlideMonths); c.Pass() {
		t.Error("a month exported twice passed")
	}
	ms = glideMonths()
	ms = append(ms, ms[2])
	if c := checkNamed(parity.CompareGlide(glide.DefaultAnchors(), "G", glidePlan[:2], ms), parity.CheckGlideMonths); c.Pass() {
		t.Error("an unplanned month exported twice passed")
	}
}

func TestAScheduleGoCannotReadFailsTheExport(t *testing.T) {
	r := parity.CompareGlide([]glide.Anchor{{AgeYears: 38}}, "G", glidePlan, glideMonths())
	if c := checkNamed(r, parity.CheckGlideExport); c.Pass() || !strings.Contains(c.First, "anchors") {
		t.Errorf("export %+v", c)
	}
}

func TestAFixedMixIsComparedOnItsWeightsOnly(t *testing.T) {
	ms := glideMonths()
	for i := range ms {
		ms[i].Glide, ms[i].B = "fixed", 0.3
		ms[i].Targets = withBondSleeve(ms[i].Book, 0.3)
	}
	r := compareGlide(ms)
	for _, name := range []string{parity.CheckGlideMonths, parity.CheckGlideExport, parity.CheckGlideWeights, parity.CheckGlideSleeves} {
		if c := checkNamed(r, name); !c.Pass() {
			t.Errorf("%s failed in a fixed run: %s", name, c.First)
		}
	}
	for _, name := range []string{parity.CheckGlideAge, parity.CheckGlideShare} {
		if c := checkNamed(r, name); c.Compared != 0 {
			t.Errorf("%s compared %d in a fixed run", name, c.Compared)
		}
	}
}

func TestAGlidePayloadDecodesExactly(t *testing.T) {
	raw := `{"month":"2010-02","date":"2010-02-01","glide":"path","start_age":53.0,"measure_from":"2002-08",` +
		`"age":60.5,"b":0.42000000000000004,"book":{"AAA":0.49,"IEF":0.49},"targets":{"AAA":0.2842,"IEF":0.4900000000000001}}`
	m, err := parity.DecodeGlide([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprintf("%s %s %s %v %s %v %v %v %v", m.Month, m.Date, m.Glide, m.StartAge, m.MeasureFrom, m.Age, m.B,
		m.Book["IEF"], m.Targets["IEF"])
	if got != "2010-02 2010-02-01 path 53 2002-08 60.5 0.42000000000000004 0.49 0.4900000000000001" {
		t.Errorf("decoded %s", got)
	}
}

func TestAMalformedGlidePayloadIsRefused(t *testing.T) {
	good := `{"month":"2010-02","date":"2010-02-01","glide":"path","start_age":53.0,"measure_from":"2002-08",` +
		`"age":60.5,"b":0.4,"book":{"AAA":0.98},"targets":{"AAA":0.98}}`
	for name, raw := range map[string]string{
		"unknown field": strings.Replace(good, `"b":`, `"x":1,"b":`, 1),
		"missing age":   strings.Replace(good, `"age":60.5,`, ``, 1),
		"null share":    strings.Replace(good, `"b":0.4`, `"b":null`, 1),
		"null weight":   strings.Replace(good, `"targets":{"AAA":0.98}`, `"targets":{"AAA":null}`, 1),
		"null book":     strings.Replace(good, `"book":{"AAA":0.98}`, `"book":{"AAA":null}`, 1),
		"no targets":    strings.Replace(good, `,"targets":{"AAA":0.98}`, ``, 1),
		"no month":      strings.Replace(good, `"month":"2010-02",`, ``, 1),
		"trailing":      good + `{}`,
	} {
		if _, err := parity.DecodeGlide([]byte(raw)); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}
