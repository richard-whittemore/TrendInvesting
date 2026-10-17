package glide

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Synthetic owner: born 1980-01 (not anyone's real birth month).
const synthBirth = "1980-01"

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 8, 0, 0, 0, time.UTC)
}

func mustShare(t *testing.T, months int) float64 {
	t.Helper()
	b, err := BondShare(DefaultAnchors(), months)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestAnchorsExactAndEitherSide(t *testing.T) {
	cases := []struct {
		name   string
		months int
		want   float64
	}{
		{"before 38", 30 * 12, 0},
		{"month before 38", 38*12 - 1, 0},
		{"38", 38 * 12, 0},
		{"month after 38", 38*12 + 1, 0.308 / 180},
		{"month before 53", 53*12 - 1, 0.308 - 0.308/180},
		{"53", 53 * 12, 0.308},
		{"month after 53", 53*12 + 1, 0.308 + 0.016/12},
		{"month before 65", 65*12 - 1, 0.5 - 0.016/12},
		{"65", 65 * 12, 0.5},
		{"month after 65", 65*12 + 1, 0.5 + 0.2/84},
		{"month before 72", 72*12 - 1, 0.7 - 0.2/84},
		{"72", 72 * 12, 0.7},
		{"month after 72", 72*12 + 1, 0.7},
		{"far after", 100 * 12, 0.7},
	}
	for _, c := range cases {
		got := mustShare(t, c.months)
		if !near(got, c.want) {
			t.Errorf("%s: b = %v, want %v", c.name, got, c.want)
		}
	}
	for _, m := range []int{38 * 12, 53 * 12, 65 * 12, 72 * 12} {
		want := map[int]float64{38 * 12: 0, 53 * 12: 0.308, 65 * 12: 0.5, 72 * 12: 0.7}[m]
		if got := mustShare(t, m); got != want {
			t.Errorf("anchor at %d months not exact: %v != %v", m, got, want)
		}
	}
}

func TestAgeMonthsChangesOnFirstOfMonth(t *testing.T) {
	// Born 1980-01: on 2018-01-31 the owner is 38y 0m; on 2018-02-01 38y 1m.
	a, err := AgeMonths(synthBirth, day(2018, time.January, 31))
	if err != nil || a != 38*12 {
		t.Fatalf("2018-01-31: %d, %v", a, err)
	}
	b, err := AgeMonths(synthBirth, day(2018, time.February, 1))
	if err != nil || b != 38*12+1 {
		t.Fatalf("2018-02-01: %d, %v", b, err)
	}
	// The day inside a month never matters.
	c, _ := AgeMonths(synthBirth, day(2018, time.February, 28))
	if c != b {
		t.Fatalf("day changed the age: %d vs %d", c, b)
	}
}

func TestBirthRefusals(t *testing.T) {
	now := day(2026, time.October, 1)
	for _, s := range []string{
		"", "1980", "1980-1", "1980-13", "1980-00", "1980-01-15", "198001", "1980/01",
		"abcd-01", "1980-ab", "-980-01", "1980--1", "+980-01", "1980-+1",
		"2030-01", // born in the future
		"2026-09", // an infant
		"1900-01", // over 110
		"2010-10", // under 18
	} {
		if _, err := AgeMonths(s, now); err == nil {
			t.Errorf("birth %q accepted", s)
		}
	}
	if _, err := AgeMonths("2008-10", now); err != nil {
		t.Errorf("exactly 18 refused: %v", err)
	}
}

func TestBadAnchorsFailClosed(t *testing.T) {
	bad := [][]Anchor{
		nil,
		{{38, 0}},
		{{38, 0}, {38, 0.1}},
		{{53, 0.3}, {38, 0}},
		{{38, -0.1}, {53, 0.3}},
		{{38, 0}, {53, 1.5}},
		{{38, 0}, {53, math.NaN()}},
		{{38, 0.3}, {53, 0.5}}, // a nonzero first share would be ignored at that age
	}
	for i, a := range bad {
		if _, err := BondShare(a, 40*12); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func sum(ts []Target) float64 {
	var s float64
	for _, t := range ts {
		s += t.Weight
	}
	return s
}

func TestCombineSumsIEFAndRecordsShares(t *testing.T) {
	mom := []Weight{{"AAA", 0.49}, {"IEF", 0.49}} // half risk-off in IEF
	got, err := Combine(mom, 0.3, DefaultBondFunds())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Symbol != "AAA" || got[1].Symbol != "IEF" || got[2].Symbol != "SHY" {
		t.Fatalf("targets = %+v", got)
	}
	ief := got[1]
	// Variables, not constants: constant expressions are evaluated exactly.
	b, w, half, invested := 0.3, 0.49, 0.5, 0.98
	wantMom := float64(w * (1 - b))
	wantBond := float64(float64(invested*b) * half)
	if len(ief.Shares) != 2 || ief.Shares[0] != (Share{SleeveMomentum, wantMom}) ||
		ief.Shares[1] != (Share{SleeveBond, wantBond}) {
		t.Fatalf("IEF shares = %+v", ief.Shares)
	}
	if ief.Weight != wantMom+wantBond {
		t.Fatalf("IEF weight %v != sum of shares %v", ief.Weight, wantMom+wantBond)
	}
	if len(got[0].Shares) != 1 || got[0].Shares[0].Sleeve != SleeveMomentum {
		t.Fatalf("AAA shares = %+v", got[0].Shares)
	}
	if len(got[2].Shares) != 1 || got[2].Shares[0].Sleeve != SleeveBond {
		t.Fatalf("SHY shares = %+v", got[2].Shares)
	}
}

func TestCombineWeightsSumTo98(t *testing.T) {
	mom := []Weight{{"A", 0.098}, {"B", 0.098}, {"C", 0.098}, {"D", 0.098}, {"E", 0.098},
		{"F", 0.098}, {"G", 0.098}, {"H", 0.098}, {"I", 0.098}, {"IEF", 0.098}}
	for _, b := range []float64{0, 0.05, 0.308, 0.5, 0.7, 1} {
		got, err := Combine(mom, b, DefaultBondFunds())
		if err != nil {
			t.Fatal(err)
		}
		if s := sum(got); math.Abs(s-0.98) > 1e-12 {
			t.Errorf("b=%v: weights sum to %v", b, s)
		}
	}
}

func TestCombineZeroShareLeavesMomentumUntouched(t *testing.T) {
	got, err := Combine([]Weight{{"A", 0.98}}, 0, DefaultBondFunds())
	if err != nil || len(got) != 1 || got[0].Weight != 0.98 || len(got[0].Shares) != 1 {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestCombineRefusals(t *testing.T) {
	f := DefaultBondFunds()
	m := []Weight{{"A", 0.5}}
	cases := map[string]func() error{
		"b negative":   func() error { _, e := Combine(m, -0.1, f); return e },
		"b above 1":    func() error { _, e := Combine(m, 1.1, f); return e },
		"b NaN":        func() error { _, e := Combine(m, math.NaN(), f); return e },
		"over 98":      func() error { _, e := Combine([]Weight{{"A", 0.99}}, 0.1, f); return e },
		"negative":     func() error { _, e := Combine([]Weight{{"A", -0.1}}, 0.1, f); return e },
		"dup":          func() error { _, e := Combine([]Weight{{"A", 0.1}, {"A", 0.1}}, 0.1, f); return e },
		"empty symbol": func() error { _, e := Combine([]Weight{{"", 0.1}}, 0.1, f); return e },
		"funds not 1":  func() error { _, e := Combine(m, 0.1, []Weight{{"IEF", 0.5}}); return e },
		"fund zero":    func() error { _, e := Combine(m, 0.1, []Weight{{"IEF", 1}, {"SHY", 0}}); return e },
		"momentum Inf": func() error { _, e := Combine([]Weight{{"A", math.Inf(1)}}, 0.1, f); return e },
	}
	for name, fn := range cases {
		if fn() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// ageOnReference transcribes glide_path.py:59-71 (glide_share) exactly.
func glideShareReference(age float64) float64 {
	switch {
	case age <= 38:
		return 0
	case age <= 53:
		return float64(0.308*(age-38)) / 15
	case age <= 65:
		return 0.308 + float64(0.016*(age-53))
	case age <= 72:
		return 0.50 + float64(0.20*(age-65))/7
	}
	return 0.70
}

func TestScheduleMatchesReferenceFormulaEveryMonth(t *testing.T) {
	for months := 0; months <= maxOwnerAgeYears*12; months++ {
		want := glideShareReference(float64(months) / 12)
		if got := mustShare(t, months); !near(got, want) {
			t.Fatalf("%d months: %v, reference %v", months, got, want)
		}
	}
}

// Values printed by running glide_path.py's glide_share at age 38 + k/12.
var reference = []struct {
	k int
	b float64
}{
	{0, 0.0},
	{6, 0.010266666666666667},
	{12, 0.020533333333333334},
	{18, 0.030799999999999998},
	{24, 0.04106666666666667},
	{30, 0.051333333333333335},
	{36, 0.061599999999999995},
	{42, 0.07186666666666668},
	{48, 0.08213333333333334},
	{54, 0.0924},
	{60, 0.10266666666666667},
	{66, 0.11293333333333333},
	{72, 0.12319999999999999},
	{78, 0.13346666666666665},
	{84, 0.14373333333333335},
	{90, 0.154},
	{96, 0.16426666666666667},
	{102, 0.17453333333333332},
	{108, 0.1848},
	{114, 0.19506666666666667},
	{120, 0.20533333333333334},
	{126, 0.21559999999999999},
	{132, 0.22586666666666666},
	{138, 0.23613333333333333},
	{144, 0.24639999999999998},
	{150, 0.25666666666666665},
	{156, 0.2669333333333333},
	{162, 0.2772},
	{168, 0.2874666666666667},
	{174, 0.29773333333333335},
	{180, 0.308},
	{186, 0.316},
	{192, 0.324},
	{198, 0.332},
	{204, 0.33999999999999997},
	{210, 0.348},
	{216, 0.356},
	{222, 0.364},
	{228, 0.372},
	{234, 0.38},
	{240, 0.388},
	{246, 0.396},
	{252, 0.404},
	{258, 0.41200000000000003},
	{264, 0.42},
	{270, 0.428},
	{276, 0.436},
	{282, 0.444},
	{288, 0.452},
	{294, 0.45999999999999996},
	{300, 0.46799999999999997},
	{306, 0.476},
	{312, 0.484},
	{318, 0.492},
	{324, 0.5},
	{330, 0.5142857142857142},
	{336, 0.5285714285714286},
	{342, 0.5428571428571428},
	{348, 0.5571428571428572},
	{354, 0.5714285714285714},
	{360, 0.5857142857142857},
	{366, 0.6},
	{372, 0.6142857142857143},
	{378, 0.6285714285714286},
	{384, 0.6428571428571428},
	{390, 0.6571428571428571},
	{396, 0.6714285714285715},
	{402, 0.6857142857142857},
	{408, 0.7},
	{414, 0.7},
	{420, 0.7},
	{426, 0.7},
	{432, 0.7},
}

func TestScheduleMatchesValuesComputedFromGlidePathPy(t *testing.T) {
	for _, r := range reference {
		if got := mustShare(t, 38*12+r.k); !near(got, r.b) {
			t.Errorf("k=%d: %v, want %v", r.k, got, r.b)
		}
	}
}

func TestEndToEndFromBirthValue(t *testing.T) {
	// Born 1980-01, rebalance 2033-01-15: 53y 0m -> 30.8%.
	m, err := AgeMonths(synthBirth, day(2033, time.January, 15))
	if err != nil {
		t.Fatal(err)
	}
	if b := mustShare(t, m); b != 0.308 {
		t.Fatalf("b = %v", b)
	}
	if _, err := AgeMonths("1980-01-15", day(2033, time.January, 15)); err == nil || !strings.Contains(err.Error(), "YYYY-MM") {
		t.Fatalf("day not refused: %v", err)
	}
}
