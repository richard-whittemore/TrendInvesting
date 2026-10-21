package momentum_test

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// labelledInput is a month with twenty common-stock members that all score,
// the first of them with no sector label, and SPY above its mean.
func labelledInput(unlabelled int) momentum.Input {
	return membersInput(20, unlabelled, func(i int) string { return fmt.Sprint(101 + i%11) })
}

// membersInput is a month with total common-stock members that all score and
// SPY above its mean. The first `unlabelled` members carry no sector label;
// the others take sectorOf(i).
func membersInput(total, unlabelled int, sectorOf func(i int) string) momentum.Input {
	cal := calendar(300)
	closes := map[string]map[string]float64{"SPY": series(cal, trending(300, 1.001))}
	var rows []momentum.Row
	for i, id := range ids(total, "N") {
		closes[id] = series(cal, wobbling(300, 1.001+float64(i)/10000, 0.01))
		row := momentum.Row{InstrumentID: id, Type: "CS", Close: 50, Volume: 1e6 + float64(i), Sector: sectorOf(i), SectorSource: "gics-iwv"}
		if i < unlabelled {
			row.Sector, row.SectorSource = "", ""
		}
		rows = append(rows, row)
	}
	return momentum.Input{Sessions: cal, Closes: closes, Universe: momentum.Snapshot{Session: cal[len(cal)-1], Rows: rows}}
}

// TestDecideWithholdsAUniverseWithTooManyUnlabelledNames: a name with no
// sector label is ranked uncapped, as the backtest did, but a snapshot in
// which more than the configured share of the ranked members has no label
// is a label outage (a renamed label column, a renamed vocabulary, a broken
// source), not a few names the sources do not cover. Deciding it would trade
// an uncapped book with the sector cap silently absent, so it is withheld as
// labels-missing and the last book kept (ADR 0026 point 5, "fail closed";
// point 8, the owner's decision of 2026-10-18).
func TestDecideWithholdsAUniverseWithTooManyUnlabelledNames(t *testing.T) {
	cfg := momentum.FCombo()
	mustEqual(t, "default ceiling", cfg.MaxUnlabelledShare, 0.20)
	for name, unlabelled := range map[string]int{"every name unlabelled": 20, "a quarter unlabelled": 5} {
		t.Run(name, func(t *testing.T) {
			d, err := momentum.Decide(cfg, labelledInput(unlabelled))
			if err != nil {
				t.Fatal(err)
			}
			mustEqual(t, "outcome", d.Outcome, momentum.OutcomeWithheld)
			mustEqual(t, "reason", d.Reason, momentum.ReasonLabelsMissing)
			mustEqual(t, "targets", len(d.Targets), 0)
			mustEqual(t, "names recorded", len(d.Names), 20)
			want := fmt.Sprintf("%d of 20 ranked members", unlabelled)
			if !strings.Contains(d.Detail, want) || !strings.Contains(d.Detail, "20%") {
				t.Errorf("detail = %q, want the count (%q) and the ceiling", d.Detail, want)
			}
		})
	}
}

// TestDecideDecidesAUniverseWithFewUnlabelledNames: one name in twenty (5%,
// the backtest's months had 3.5% to 5.3%) is under the ceiling, so the
// month is decided and the unlabelled name ranked uncapped: every other
// member shares one sector, so the sector cap leaves three of them picked,
// and the unlabelled name, the lowest ranked, is picked beside them.
func TestDecideDecidesAUniverseWithFewUnlabelledNames(t *testing.T) {
	in := membersInput(20, 1, func(int) string { return "101" })
	d, err := momentum.Decide(momentum.FCombo(), in)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "outcome", d.Outcome, momentum.OutcomeDecided)
	mustEqual(t, "reason", d.Reason, momentum.WithheldReason(""))
	unlabelled := in.Universe.Rows[0].InstrumentID
	picked, found := 0, false
	for _, n := range d.Names {
		if n.Status == momentum.StatusPicked {
			picked++
		}
		if n.InstrumentID == unlabelled {
			found = true
			mustEqual(t, "unlabelled name", n.Status, momentum.StatusPicked)
		}
	}
	if !found {
		t.Fatalf("unlabelled member %s missing from names", unlabelled)
	}
	mustEqual(t, "picked (three in the capped sector plus the unlabelled name)", picked, 4)
}

// TestDecideAtTheCeilingDecides: the ceiling is the most a decided month may
// have; only a share above it withholds.
func TestDecideAtTheCeilingDecides(t *testing.T) {
	cfg := momentum.FCombo()
	cfg.MaxUnlabelledShare = 0.25
	d, err := momentum.Decide(cfg, labelledInput(5))
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "outcome", d.Outcome, momentum.OutcomeDecided)
}

// TestDecideAtANonBinaryCeilingDecides: 29 of 100 is exactly the 0.29 ceiling,
// but 0.29*100 rounds below 29 in floating point, so the share is compared as
// a quotient: only a share above the ceiling withholds.
func TestDecideAtANonBinaryCeilingDecides(t *testing.T) {
	cfg := momentum.FCombo()
	cfg.MaxUnlabelledShare = 0.29
	in := membersInput(100, 29, func(i int) string { return fmt.Sprint(101 + i%11) })
	d, err := momentum.Decide(cfg, in)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "outcome", d.Outcome, momentum.OutcomeDecided)
}

// TestConfigRefusesAnUnlabelledCeilingOutsideTheUnitInterval: a ceiling of
// zero or less, one or more, or not finite is not a share.
func TestConfigRefusesAnUnlabelledCeilingOutsideTheUnitInterval(t *testing.T) {
	for _, v := range []float64{0, -0.1, 1, 1.5, math.NaN(), math.Inf(1)} {
		cfg := momentum.FCombo()
		cfg.MaxUnlabelledShare = v
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate with ceiling %v: want an error", v)
		}
	}
}
