package targetweight_test

import (
	"bytes"
	"context"
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/glide"
)

// glideBirth is a synthetic birth month, nobody's real one: the owner is
// exactly 53 years (636 months) old on February's rebalance, 2025-02-03, so
// b is the anchor 0.308 there and one month further up the path on March's.
const glideBirth = "1972-02"

func withBirth(birth string) func(*event.TargetWeightConfigurationPayload) {
	return func(c *event.TargetWeightConfigurationPayload) { c.Glide.Birth = birth }
}

func shareOf(tw event.TargetWeight, sleeve string) float64 {
	for _, s := range tw.Shares {
		if s.Sleeve == sleeve {
			return s.Weight
		}
	}
	return 0
}

func targetOf(p event.TargetsDecidedPayload, id string) (event.TargetWeight, bool) {
	for _, t := range p.Targets {
		if t.InstrumentID == id {
			return t, true
		}
	}
	return event.TargetWeight{}, false
}

// TestGlideBondShareStepsAcrossTheMonthBoundary checks that each decision
// counts the owner's age on its rebalance session, the first session of the
// next month (glide_path.py:242 computes b on the MonthStart rebalance day),
// and records it with b.
func TestGlideBondShareStepsAcrossTheMonthBoundary(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{mutate: withBirth(glideBirth)})
	decisions := targets(t, r.decisions)
	if len(decisions) != 2 {
		t.Fatalf("%d decisions", len(decisions))
	}
	jan, feb := decisions[0], decisions[1]
	b2, err := glide.BondShare(glide.DefaultAnchors(), 53*12+1)
	if err != nil {
		t.Fatal(err)
	}
	want := []event.GlideFigures{
		{RebalanceSession: "2025-02-03", AgeMonths: 53 * 12, BondShare: 0.308},
		{RebalanceSession: "2025-03-03", AgeMonths: 53*12 + 1, BondShare: b2},
	}
	for i, d := range decisions {
		if d.Glide != want[i] {
			t.Errorf("decision %d glide %+v, want %+v", i, d.Glide, want[i])
		}
	}
	if !(feb.Glide.BondShare > jan.Glide.BondShare) {
		t.Errorf("b did not step up across the month: %v then %v", jan.Glide.BondShare, feb.Glide.BondShare)
	}
	// Before the path starts, the age is still recorded and b is 0.
	young := targets(t, simulateMomentum(t, momentumOptions{}).decisions)[0]
	if young.Glide.AgeMonths != (2025-1995)*12+2-6 || young.Glide.BondShare != 0 {
		t.Errorf("before 38: %+v", young.Glide)
	}
}

// TestGlideSleevesAreSummedIntoOneTarget checks the combination (plan.md
// section 2; glide_path.py:80-88,177): the momentum book scaled to 1 - b,
// with its own risk-off IEF; the bond sleeve 0.98 b split IEF 0.5 / SHY 0.5;
// IEF held by both one target, the sum, with each sleeve's share recorded.
func TestGlideSleevesAreSummedIntoOneTarget(t *testing.T) {
	plain := targets(t, simulateMomentum(t, momentumOptions{}).decisions)
	r := simulateMomentum(t, momentumOptions{mutate: withBirth(glideBirth)})
	for i, d := range targets(t, r.decisions) {
		b := d.Glide.BondShare
		// The rule's own figures are the book before scaling.
		if len(d.Momentum.Names) != len(plain[i].Momentum.Names) {
			t.Fatalf("decision %d ranks differently with the glide path", i)
		}
		for k, n := range d.Momentum.Names {
			if n != plain[i].Momentum.Names[k] {
				t.Errorf("decision %d name %s: %+v, without the glide %+v", i, n.InstrumentID, n, plain[i].Momentum.Names[k])
			}
		}
		book := map[string]float64{}
		for _, tw := range plain[i].Targets {
			book[tw.InstrumentID] = tw.Weight
		}
		sum := 0.0
		for _, tw := range d.Targets {
			mom := float64(book[tw.InstrumentID] * (1 - b))
			bond := 0.0
			if tw.InstrumentID == "IEF" || tw.InstrumentID == "SHY" {
				bond = float64(float64(0.98*b) * 0.5)
			}
			if shareOf(tw, event.SleeveMomentum) != mom || shareOf(tw, event.SleeveBond) != bond {
				t.Errorf("decision %d %s shares %+v, want momentum %v bond %v", i, tw.InstrumentID, tw.Shares, mom, bond)
			}
			sum += tw.Weight
		}
		if math.Abs(sum-0.98) > 1e-12 {
			t.Errorf("decision %d weights sum to %v, want 0.98", i, sum)
		}
	}
	feb := targets(t, r.decisions)[1]
	ief, ok := targetOf(feb, "IEF")
	if !ok || len(ief.Shares) != 2 || ief.Shares[0].Sleeve != event.SleeveMomentum || ief.Shares[1].Sleeve != event.SleeveBond {
		t.Fatalf("February's risk-off IEF is one target with both sleeves' shares: %+v", ief)
	}
	if ief.Weight != ief.Shares[0].Weight+ief.Shares[1].Weight {
		t.Errorf("IEF %v is not the sum of %+v", ief.Weight, ief.Shares)
	}
	// The planner sized February's IEF from the summed target.
	for _, o := range planned(t, r.decisions) {
		if o.InstrumentID == "IEF" && o.Tag == "targets-decided:ira:2025-02-28T21:00:00Z" && o.TargetWeight != ief.Weight {
			t.Errorf("IEF planned at target %v, want the sum %v", o.TargetWeight, ief.Weight)
		}
	}
}

func orderList(t *testing.T, es []event.Envelope) []string {
	t.Helper()
	var out []string
	for _, o := range planned(t, es) {
		out = append(out, o.Side+" "+o.InstrumentID)
	}
	return out
}

// TestGlideBandAppliesToTheSummedTargets checks that the 25% band applies to
// each summed target (plan.md section 2): IEF, held by both sleeves, is
// resized only once its holding is past the band of the summed target.
func TestGlideBandAppliesToTheSummedTargets(t *testing.T) {
	// January risk-on: IEF and SHY are the bond sleeve's only, 0.98 b / 2
	// each. February risk-off: IEF's summed target is about three times
	// January's, past the band, so it is bought; SHY's moves by b's one-month
	// step, well inside the band, so it is not traded.
	r := simulateMomentum(t, momentumOptions{mutate: withBirth(glideBirth)})
	want := []string{"buy AAA", "buy CCC", "buy IEF", "buy SHY", "sell AAA", "sell CCC", "buy BBB", "buy IEF"}
	if got := orderList(t, r.decisions); !equalStrings(got, want) {
		t.Fatalf("orders %v, want %v", got, want)
	}

	// Risk-off in both months: IEF is held by both sleeves both times and
	// its summed target hardly moves, so its holding stays inside the band
	// and it is not traded in February.
	both := simulateMomentum(t, momentumOptions{mutate: withBirth(glideBirth), riskOffJanuary: true})
	decisions := targets(t, both.decisions)
	for _, d := range decisions {
		if ief, _ := targetOf(d, "IEF"); !d.Momentum.RiskOff || len(ief.Shares) != 2 {
			t.Fatalf("decision on %s: risk-off %v, IEF %+v", d.Momentum.CloseSession, d.Momentum.RiskOff, ief)
		}
	}
	for _, o := range planned(t, both.decisions) {
		if o.Tag == "targets-decided:ira:2025-02-28T21:00:00Z" && (o.InstrumentID == "IEF" || o.InstrumentID == "SHY") {
			t.Errorf("%s %s resized inside the band: %+v", o.Side, o.InstrumentID, o)
		}
	}
	if got := orderList(t, both.decisions); len(got) < 5 || got[2] != "buy IEF" {
		t.Errorf("orders %v: January buys IEF once for both sleeves", got)
	}
}

// TestGlideBondFundsNeedTheirFinalBar checks ADR 0026, point 5: a missing
// final bar for SHY at a decision time withholds it (missing-bars), as IEF's
// does, once the bond sleeve targets it; before the path starts SHY is not
// targeted and its bar is not needed.
func TestGlideBondFundsNeedTheirFinalBar(t *testing.T) {
	for name, c := range map[string]struct {
		opts momentumOptions
		want []string
	}{
		"SHY without its final bar": {momentumOptions{mutate: withBirth(glideBirth), noBar: map[string]string{"2025-01-31": "SHY"}},
			[]string{"withheld:missing-bars", "decided:"}},
		"IEF without its final bar, risk-on": {momentumOptions{mutate: withBirth(glideBirth), noBar: map[string]string{"2025-01-31": "IEF"}},
			[]string{"withheld:missing-bars", "decided:"}},
		"SHY without its bar before the path starts": {momentumOptions{noBar: map[string]string{"2025-01-31": "SHY"}},
			[]string{"decided:", "decided:"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := simulateMomentum(t, c.opts)
			if got := withheldReasons(t, r); !equalStrings(got, c.want) {
				t.Fatalf("decisions %v, want %v", got, c.want)
			}
			d := targets(t, r.decisions)[0]
			if d.Glide.AgeMonths == 0 {
				t.Errorf("a withheld decision still records the age and b: %+v", d.Glide)
			}
		})
	}
}

func TestGlideBirthValueIsRefused(t *testing.T) {
	from := targetweight.Options{DecideFrom: closeOf("2025-01-02")}
	for name, birth := range map[string]string{
		"malformed":       "1972-2",
		"a day":           "1972-02-01",
		"month 13":        "1972-13",
		"under 18":        "2007-02",
		"over 110":        "1914-12",
		"after the start": "2026-01",
	} {
		cfg := momentumConfiguration()
		cfg.Glide.Birth = birth
		if _, err := targetweight.New(strategyVersion, cfg, from); err == nil {
			t.Errorf("%s: birth %q accepted", name, birth)
		}
	}
	for name, birth := range map[string]string{"exactly 18": "2007-01", "exactly 110": "1915-01"} {
		cfg := momentumConfiguration()
		cfg.Glide.Birth = birth
		if _, err := targetweight.New(strategyVersion, cfg, from); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The bond sleeve holds 98% of b; a momentum book invested above that
	// could not be combined with it.
	cfg := momentumConfiguration()
	cfg.Momentum.Invested = 0.99
	if _, err := targetweight.New(strategyVersion, cfg, from); err == nil {
		t.Error("a momentum book invested above 98% accepted")
	}
}

// TestGlideAgeBeyondThePlausibleStopsTheRun checks that a run reaching an
// age outside the plausible range at a decision fails rather than deciding.
func TestGlideAgeBeyondThePlausibleStopsTheRun(t *testing.T) {
	cfg := momentumConfiguration()
	cfg.Glide.Birth = "1915-01" // 110 years at the start, one month more at February's rebalance
	h, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: closeOf("2025-01-31")})
	if err != nil {
		t.Fatal(err)
	}
	s := &stream{hash: h.ConfigurationHash()}
	mustApply(t, h, s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", closeOf("2025-01-31"), cfg))
	mustApply(t, h, calendarInput(t, s, "cal", "2025-01-31", "2025-02-03"))
	mustApply(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarSchemaVersion, "b", closeOf("2025-01-31"), spyBar("2025-01-31", 500)))
	_, err = h.Apply(context.Background(), s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "s", closeOf("2025-01-31"),
		event.SessionClosedPayload{PeriodEnd: closeOf("2025-01-31"), InstrumentIDs: []string{"SPY"}}))
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("age")) {
		t.Fatalf("err = %v, want the age refused", err)
	}
}

func TestGlideJournalIsByteIdenticalAndReplays(t *testing.T) {
	opts := momentumOptions{mutate: withBirth(glideBirth)}
	write := func(r momentumResult) []byte {
		header, err := r.recorder.Header(r.hash, strategyVersion)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := journal.Write(&buf, header, r.entries); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	first := write(simulateMomentum(t, opts))
	if !bytes.Equal(first, write(simulateMomentum(t, opts))) {
		t.Fatal("rerunning the same inputs must write a byte-identical journal")
	}
	header, records, err := journal.Read(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.CheckIdentity(header, records); err != nil {
		t.Fatal(err)
	}
	inputs, decisions, err := journal.Split(records)
	if err != nil {
		t.Fatal(err)
	}
	cfg := momentumConfiguration()
	opts.mutate(&cfg)
	handler, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: closeOf("2025-01-02")})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := replay.New(handler)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := engine.Run(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if d := replay.Equivalent(decisions, replayed); d != nil {
		t.Fatalf("replay diverged: %+v", d)
	}
}
