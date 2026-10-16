package event_test

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// synthGlide is the glide-path section with a synthetic birth value, never
// anyone's real one: the real value lives only in server configuration
// (plan.md section 2; ADR 0026, point 6).
func synthGlide() event.GlideConfiguration {
	return event.GlideConfiguration{
		Anchors: []event.GlideAnchor{
			{AgeYears: 38, Share: 0}, {AgeYears: 53, Share: 0.308}, {AgeYears: 65, Share: 0.5}, {AgeYears: 72, Share: 0.7},
		},
		RetirementAgeYears: 65,
		BondFunds:          []event.BondFund{{InstrumentID: "IEF", Fraction: 0.5}, {InstrumentID: "SHY", Fraction: 0.5}},
		Birth:              "1980-01",
	}
}

func TestGlideConfigurationValidate(t *testing.T) {
	v := validMomentumConfiguration()
	type P = event.TargetWeightConfigurationPayload
	anchors := func(mutate func([]event.GlideAnchor) []event.GlideAnchor) func(*P) {
		return func(p *P) { p.Glide.Anchors = mutate(slices.Clone(p.Glide.Anchors)) }
	}
	funds := func(mutate func([]event.BondFund) []event.BondFund) func(*P) {
		return func(p *P) { p.Glide.BondFunds = mutate(slices.Clone(p.Glide.BondFunds)) }
	}
	expectInvalid(t, "momentum without glide", v, func(p *P) { p.Glide = event.GlideConfiguration{} }, "glide path is required")
	expectInvalid(t, "one anchor", v, anchors(func(a []event.GlideAnchor) []event.GlideAnchor { return a[:1] }), "two anchors")
	expectInvalid(t, "ages not increasing", v, anchors(func(a []event.GlideAnchor) []event.GlideAnchor { a[2].AgeYears = 53; return a }), "increase")
	expectInvalid(t, "share above one", v, anchors(func(a []event.GlideAnchor) []event.GlideAnchor { a[3].Share = 1.01; return a }), "[0, 1]")
	expectInvalid(t, "share nan", v, anchors(func(a []event.GlideAnchor) []event.GlideAnchor { a[1].Share = math.NaN(); return a }), "[0, 1]")
	expectInvalid(t, "first share not zero", v, anchors(func(a []event.GlideAnchor) []event.GlideAnchor { a[0].Share = 0.1; return a }), "first anchor")
	expectInvalid(t, "first age negative", v, anchors(func(a []event.GlideAnchor) []event.GlideAnchor { a[0].AgeYears = -1; return a }), "age")
	expectInvalid(t, "retirement not an anchor", v, func(p *P) { p.Glide.RetirementAgeYears = 66 }, "retirement")
	expectInvalid(t, "no bond funds", v, funds(func([]event.BondFund) []event.BondFund { return nil }), "bond fund")
	expectInvalid(t, "fund repeated", v, funds(func(f []event.BondFund) []event.BondFund { f[1].InstrumentID = "IEF"; return f }), "repeated")
	expectInvalid(t, "fund unnamed", v, funds(func(f []event.BondFund) []event.BondFund { f[0].InstrumentID = ""; return f }), "empty")
	expectInvalid(t, "fund zero", v, funds(func(f []event.BondFund) []event.BondFund { f[0].Fraction, f[1].Fraction = 0, 1; return f }), "positive")
	expectInvalid(t, "funds not summing to one", v, funds(func(f []event.BondFund) []event.BondFund { f[1].Fraction = 0.4; return f }), "sum to 1")
	for _, birth := range []string{"", "1980", "1980-1", "1980-01-15", "1980-13", "1980-00", "80-01-", "+980-01", "1980/01", "198a-01", "1980--1"} {
		expectInvalid(t, "birth "+birth, v, func(p *P) { p.Glide.Birth = birth }, "birth")
	}

	b := validTargetWeightConfiguration()
	expectInvalid(t, "boost with glide", b, func(p *P) { p.Glide = synthGlide() }, "no glide")

	// One fund taking the whole sleeve is a valid split.
	one := validMomentumConfiguration()
	one.Glide.BondFunds = []event.BondFund{{InstrumentID: "IEF", Fraction: 1}}
	if err := one.Validate(); err != nil {
		t.Errorf("a single bond fund is valid: %v", err)
	}
}

func TestGlideConfigurationEntersTheHash(t *testing.T) {
	base := event.TargetWeightConfigurationHash(validMomentumConfiguration())
	type P = event.TargetWeightConfigurationPayload
	for name, mutate := range map[string]func(*P){
		"birth":      func(p *P) { p.Glide.Birth = "1980-02" },
		"retirement": func(p *P) { p.Glide.RetirementAgeYears = 72 },
		"anchor age": func(p *P) { p.Glide.Anchors = slices.Clone(p.Glide.Anchors); p.Glide.Anchors[1].AgeYears = 54 },
		"anchor share": func(p *P) {
			p.Glide.Anchors = slices.Clone(p.Glide.Anchors)
			p.Glide.Anchors[1].Share = 0.3
		},
		"split": func(p *P) {
			p.Glide.BondFunds = []event.BondFund{{InstrumentID: "IEF", Fraction: 0.6}, {InstrumentID: "SHY", Fraction: 0.4}}
		},
	} {
		p := validMomentumConfiguration()
		mutate(&p)
		if event.TargetWeightConfigurationHash(p) == base {
			t.Errorf("changing the glide %s left the hash unchanged", name)
		}
	}
	raw, err := json.Marshal(validTargetWeightConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"glide"`) {
		t.Errorf("a Boost configuration encodes a glide section: %s", raw)
	}
}

// glideDecided is a decided momentum decision with the bond sleeve: b 0.308,
// the book risk-off, so IEF is held by both sleeves.
func glideDecided() event.TargetsDecidedPayload {
	p := validMomentumDecided()
	mom := 0.49 * (1 - 0.308)
	bond := 0.98 * 0.308 * 0.5
	p.Targets = []event.TargetWeight{
		{InstrumentID: "AAA", Weight: mom, Shares: []event.SleeveShare{{Sleeve: event.SleeveMomentum, Weight: mom}}},
		{InstrumentID: "IEF", Weight: mom + bond, Shares: []event.SleeveShare{{Sleeve: event.SleeveMomentum, Weight: mom}, {Sleeve: event.SleeveBond, Weight: bond}}},
		{InstrumentID: "SHY", Weight: bond, Shares: []event.SleeveShare{{Sleeve: event.SleeveBond, Weight: bond}}},
	}
	p.Glide = event.GlideFigures{RebalanceSession: "2025-03-05", AgeMonths: 53 * 12, BondShare: 0.308}
	return p
}

func TestGlideDecisionValidate(t *testing.T) {
	v := glideDecided()
	if err := v.Validate(); err != nil {
		t.Fatalf("the glide fixture is valid: %v", err)
	}
	type P = event.TargetsDecidedPayload
	target := func(i int, mutate func(*event.TargetWeight)) func(*P) {
		return func(p *P) {
			ts := slices.Clone(p.Targets)
			ts[i].Shares = slices.Clone(ts[i].Shares)
			mutate(&ts[i])
			p.Targets = ts
		}
	}
	expectInvalid(t, "no glide figures", v, func(p *P) { p.Glide = event.GlideFigures{} }, "glide")
	expectInvalid(t, "age zero", v, func(p *P) { p.Glide.AgeMonths = 0 }, "age")
	expectInvalid(t, "bond share above one", v, func(p *P) { p.Glide.BondShare = 1.5 }, "bond share")
	expectInvalid(t, "bond share nan", v, func(p *P) { p.Glide.BondShare = math.NaN() }, "bond share")
	expectInvalid(t, "rebalance session", v, func(p *P) { p.Glide.RebalanceSession = "March" }, "rebalance session")
	expectInvalid(t, "rebalance before close", v, func(p *P) { p.Glide.RebalanceSession = "2025-03-04" }, "after the close session")
	expectInvalid(t, "no shares", v, target(0, func(w *event.TargetWeight) { w.Shares = nil }), "sleeve share")
	expectInvalid(t, "shares not summing", v, target(1, func(w *event.TargetWeight) { w.Shares[1].Weight += 1e-9 }), "sum")
	expectInvalid(t, "unknown sleeve", v, target(2, func(w *event.TargetWeight) { w.Shares[0].Sleeve = "cash" }), "sleeve")
	expectInvalid(t, "sleeves out of order", v, target(1, func(w *event.TargetWeight) {
		w.Shares[0], w.Shares[1] = w.Shares[1], w.Shares[0]
	}), "momentum then bond")
	expectInvalid(t, "sleeve repeated", v, target(1, func(w *event.TargetWeight) { w.Shares[1].Sleeve = event.SleeveMomentum }), "momentum then bond")
	expectInvalid(t, "negative share", v, target(2, func(w *event.TargetWeight) { w.Shares[0].Weight, w.Weight = -0.1, -0.1 }), "share")

	b := validTargetsDecided()
	expectInvalid(t, "boost with glide figures", b, func(p *P) { p.Glide.AgeMonths = 600 }, "no glide")
	expectInvalid(t, "boost target with shares", b, func(p *P) {
		ts := slices.Clone(p.Targets)
		ts[0].Shares = []event.SleeveShare{{Sleeve: event.SleeveMomentum, Weight: ts[0].Weight}}
		p.Targets = ts
	}, "no sleeve shares")

	// A withheld momentum decision records the age and b, and no targets.
	withheld := v
	withheld.Outcome, withheld.Targets = event.TargetsOutcomeWithheld, nil
	withheld.WithheldReason, withheld.Detail = event.WithheldMissingBars, "no SHY bar"
	if err := withheld.Validate(); err != nil {
		t.Errorf("a withheld glide decision is valid: %v", err)
	}
	// A Boost decision's bytes carry no glide figure and no shares.
	raw, err := json.Marshal(validTargetsDecided())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"glide"`) || strings.Contains(string(raw), `"shares"`) {
		t.Errorf("a Boost decision encodes glide fields: %s", raw)
	}
}

// TestMomentumConfigurationHashStability pins the momentum fixture's hash,
// glide section included (ADR 0016).
func TestMomentumConfigurationHashStability(t *testing.T) {
	got := event.TargetWeightConfigurationHash(validMomentumConfiguration())
	const want = "sha256:2a8b82d34731e723aca06ba7ffab3e68c1e83d8c2ba601b1a8ca2b4412fe0939"
	if got != want {
		t.Fatalf("TargetWeightConfigurationHash = %s, want %s", got, want)
	}
}
