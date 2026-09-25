package registry_test

import (
	"encoding/json"
	"github.com/richard-whittemore/TrendInvesting/internal/registry"
	"math"
	"reflect"
	"testing"
	"time"
)

func date(s string) time.Time {
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return v
}
func protocolForTest(t *testing.T) registry.Protocol {
	t.Helper()
	p, err := registry.ResearchProtocol()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRegimeBoundaries(t *testing.T) {
	p := protocolForTest(t)
	for _, tc := range []struct {
		at    string
		names []string
	}{
		{"1997-12-31T23:59:59Z", nil},
		{"1998-01-01T00:00:00Z", []string{"1998–2000 late bull"}},
		{"2000-01-01T00:00:00Z", []string{"1998–2000 late bull", "2000–02 bear"}},
		{"2000-12-31T23:59:59Z", []string{"1998–2000 late bull", "2000–02 bear"}},
		{"2001-01-01T00:00:00Z", []string{"2000–02 bear"}},
		{"2003-01-01T00:00:00Z", []string{"2003–07 bull"}},
		{"2008-01-01T00:00:00Z", []string{"2008–09 crash"}},
		{"2009-01-01T00:00:00Z", []string{"2008–09 crash", "2009–19 bull"}},
		{"2020-01-01T00:00:00Z", []string{"2020 COVID"}},
		{"2021-01-01T00:00:00Z", nil},
		{"2022-12-31T23:59:59Z", []string{"2022 correction"}},
		{"2023-01-01T00:00:00Z", nil},
	} {
		t.Run(tc.at, func(t *testing.T) {
			if got := p.WindowsAt(date(tc.at)); !reflect.DeepEqual(got, tc.names) {
				t.Fatalf("got %v want %v", got, tc.names)
			}
		})
	}
	for _, w := range p.Windows {
		if !w.Contains(w.Start) || w.Contains(w.End) || !w.Contains(w.End.Add(-time.Nanosecond)) {
			t.Fatalf("boundary: %+v", w)
		}
	}
	split := date("2016-01-01T00:00:00Z")
	for _, tc := range []struct {
		start, end time.Time
		want       string
	}{
		{split.Add(-time.Second), split.Add(-time.Nanosecond), "in-sample"},
		{split, split, "out-of-sample"},
		{split.Add(-time.Second), split, "mixed"},
		{time.Time{}, time.Time{}, "unknown"},
	} {
		if got := p.Designation(tc.start, tc.end); got != tc.want {
			t.Fatalf("designation %s want %s", got, tc.want)
		}
	}
}

func TestPrimaryMetricHandComputed(t *testing.T) {
	// Synthetic arithmetic example, not a source Golden Scenario. One Julian
	// year: 100 -> 120 -> 90 -> 110. Return 10%, peak drawdown 25%, ratio .4.
	start := date("2014-01-01T00:00:00Z")
	points := []registry.EquityPoint{{start, 100}, {start.Add(100 * 24 * time.Hour), 120}, {start.Add(200 * 24 * time.Hour), 90}, {start.Add(8766 * time.Hour), 110}}
	p := protocolForTest(t)
	r, err := p.Report(points, start, points[3].At)
	if err != nil {
		t.Fatal(err)
	}
	m := r.Full
	if math.Abs(m.TotalReturn-.1) > 1e-12 || math.Abs(m.MaxDrawdown-.25) > 1e-12 || m.Primary == nil || math.Abs(*m.Primary-.4) > 1e-12 {
		t.Fatalf("metrics %+v", m)
	}
	if len(r.Windows) != 7 || r.Designation != "in-sample" {
		t.Fatalf("report %+v", r)
	}
}

func TestSecondOpeningIsNewHypothesis(t *testing.T) {
	p := protocolForTest(t)
	run := registry.Run{RunID: "first", Variant: "wider-cap", Configuration: baselineConfiguration()}
	first, err := p.Opening(run, nil)
	if err != nil {
		t.Fatal(err)
	}
	run.RunID = "second"
	run.Configuration.MaxUnitsTotalLong++ // A new parameter value is still the same Variant.
	second, err := p.Opening(run, []registry.Opening{first})
	if err != nil {
		t.Fatal(err)
	}
	if first.Repeat || !second.Repeat || second.Hypothesis == first.Hypothesis || len(second.Prior) != 1 {
		t.Fatalf("first %+v second %+v", first, second)
	}
}

func TestProtocolValidation(t *testing.T) {
	p := protocolForTest(t)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.DecodeProtocol(raw); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{nil, []byte(`{}`), append(append([]byte{}, raw...), []byte(` {}`)...), []byte(`{"unknown":true}`)} {
		if _, err := registry.DecodeProtocol(bad); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	for name, mutate := range map[string]func(*registry.Protocol){
		"version":   func(p *registry.Protocol) { p.Version++ },
		"split":     func(p *registry.Protocol) { p.Split = time.Time{} },
		"nan":       func(p *registry.Protocol) { p.DaysPerYear = math.NaN() },
		"infinite":  func(p *registry.Protocol) { p.DaysPerYear = math.Inf(1) },
		"year":      func(p *registry.Protocol) { p.DaysPerYear = 0 },
		"count":     func(p *registry.Protocol) { p.Windows = nil },
		"name":      func(p *registry.Protocol) { p.Windows[0].Name = "" },
		"duplicate": func(p *registry.Protocol) { p.Windows[1].Name = p.Windows[0].Name },
		"end":       func(p *registry.Protocol) { p.Windows[0].End = p.Windows[0].Start },
	} {
		t.Run(name, func(t *testing.T) {
			p := protocolForTest(t)
			mutate(&p)
			if _, err := p.Report(nil, time.Time{}, time.Time{}); err == nil {
				t.Fatal("accepted invalid protocol")
			}
		})
	}
}

func TestMetricsEdgeCases(t *testing.T) {
	p := protocolForTest(t)
	start := date("2015-01-01T00:00:00Z")
	end := start.Add(8766 * time.Hour)
	for _, tc := range []struct {
		name   string
		points []registry.EquityPoint
		state  string
		bad    bool
	}{
		{"empty", nil, "no-data", false},
		{"one", []registry.EquityPoint{{At: start, Equity: 100}}, "insufficient-history", false},
		{"flat", []registry.EquityPoint{{At: start, Equity: 100}, {At: end, Equity: 100}}, "zero-drawdown", false},
		{"loss", []registry.EquityPoint{{At: start, Equity: 100}, {At: end, Equity: 50}}, "ok", false},
		{"bankrupt", []registry.EquityPoint{{At: start, Equity: 100}, {At: end, Equity: 0}}, "ok", false},
		{"ratio overflow", []registry.EquityPoint{{At: start, Equity: 1}, {At: start.Add(24 * time.Hour), Equity: math.Nextafter(1, 0)}, {At: end, Equity: 1e300}}, "non-finite-ratio", false},
		{"overflow", []registry.EquityPoint{{At: start, Equity: 1}, {At: start.Add(time.Second), Equity: 1e300}}, "non-finite-return", false},
		{"invalid time", []registry.EquityPoint{{Equity: 100}}, "", true},
		{"nan", []registry.EquityPoint{{At: start, Equity: math.NaN()}}, "", true},
		{"zero opening", []registry.EquityPoint{{At: start, Equity: 0}}, "", true},
		{"negative", []registry.EquityPoint{{At: start, Equity: -1}}, "", true},
		{"duplicate", []registry.EquityPoint{{At: start, Equity: 100}, {At: start, Equity: 90}}, "", true},
		{"backwards", []registry.EquityPoint{{At: end, Equity: 100}, {At: start, Equity: 90}}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := p.Report(tc.points, start, end)
			if (err != nil) != tc.bad {
				t.Fatalf("error %v", err)
			}
			if !tc.bad && r.Full.State != tc.state {
				t.Fatalf("metrics %+v", r.Full)
			}
		})
	}
	if _, err := p.Report(nil, end, start); err == nil {
		t.Fatal("accepted backwards span")
	}
	if err := p.PermitFit(start, p.Split.Add(-time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	for _, span := range [][2]time.Time{{start, p.Split}, {p.Split, p.Split}, {end, start}, {time.Time{}, time.Time{}}} {
		if err := p.PermitFit(span[0], span[1]); err == nil {
			t.Fatal("accepted fitting span")
		}
	}
}

func TestWindowKeepsFirstLossAndDoesNotInventAnEnd(t *testing.T) {
	p := protocolForTest(t)
	start := date("2015-12-31T00:00:00Z")
	end := date("2016-01-02T00:00:00Z")
	points := []registry.EquityPoint{{At: start, Equity: 100}, {At: p.Split, Equity: 80}, {At: end, Equity: 90}}
	r, err := p.Report(points, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if r.OutOfSample.MaxDrawdown != .2 || math.Abs(r.OutOfSample.TotalReturn+.1) > 1e-12 || r.OutOfSample.Start != p.Split || r.OutOfSample.End != end || r.Designation != "mixed" {
		t.Fatalf("report %+v", r)
	}
	// A zero boundary mark leaves subsequent window returns undefined.
	points[0].Equity = 100
	points[1].Equity = 0
	points[2].At = date("2020-01-02T00:00:00Z")
	r, err = p.Report(points, start, points[2].At)
	if err != nil {
		t.Fatal(err)
	}
	if r.Windows[5].Metrics.State != "insufficient-history" {
		t.Fatalf("COVID %+v", r.Windows[5])
	}
}

func TestOpeningValidationAndOtherVariants(t *testing.T) {
	p := protocolForTest(t)
	run := completedRun("first")
	run.Variant = "v"
	first, err := p.Opening(run, []registry.Opening{{Variant: "different", Hypothesis: "other"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.DecodeOpening(raw); err != nil {
		t.Fatal(err)
	}
	if first.Repeat {
		t.Fatal("other Variant consumed opening")
	}
	if _, err := p.Opening(run, []registry.Opening{first}); err == nil {
		t.Fatal("reused opening")
	}
	for _, bad := range [][]byte{nil, []byte(`{}`), append(append([]byte{}, raw...), []byte(` {}`)...)} {
		if _, err := registry.DecodeOpening(bad); err == nil {
			t.Fatal("bad opening accepted")
		}
	}
	for _, mutate := range []func(*registry.Opening){
		func(o *registry.Opening) { o.ConfigurationHash = "bad" }, func(o *registry.Opening) { o.ProtocolHash = "bad" }, func(o *registry.Opening) { o.Hypothesis = "other" }, func(o *registry.Opening) { o.Variant = "" }, func(o *registry.Opening) { o.Repeat = true },
	} {
		o := first
		mutate(&o)
		raw, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := registry.DecodeOpening(raw); err == nil {
			t.Fatal("accepted invalid opening")
		}
	}
	run.RunID = "../bad"
	if _, err := p.Opening(run, nil); err == nil {
		t.Fatal("bad id")
	}
	run.RunID = "good"
	run.Variant = registry.Baseline
	if _, err := p.Opening(run, nil); err == nil {
		t.Fatal("baseline opening")
	}
}
