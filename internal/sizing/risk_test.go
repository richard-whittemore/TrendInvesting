package sizing_test

import (
	"math"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// --- #15: aggregate open risk, hand-derived on the Crude ladder ------------
//
// With Unit quantities equal (q contracts each) and dollarsPerPoint 1, the
// aggregate open risk at k Units, per contract, is:
//
//	k=1: 2.40  (2N)
//	k=2: 4.20  (3.5N)
//	k=3: 5.40  (4.5N)
//	k=4: 6.00  (5N)
//
// derived from the per-Unit risks the ticket itself states for k=4 — 0.60,
// 1.20, 1.80, 2.40 (1/2N, 1N, 3/2N, 2N; The Turtle Rules p.22, 28.30-27.70,
// 28.90-27.70, 29.50-27.70, 30.10-27.70) — and the analogous per-Unit risks
// at k=1..3 computed from crudeStop/RaisedStop the same way
// stop_test.go's TestRaisedStopReproducesFaithsCrudeTablesRowByRow does.

func crudeUnitOpenRisk(t *testing.T, entry, stop float64, quantity int64) sizing.UnitOpenRisk {
	t.Helper()
	return sizing.UnitOpenRisk{EntryPrice: entry, ProtectiveStop: stop, Quantity: quantity}
}

// TestAggregateOpenRiskCrudeLadderAtEachRungCount is the arithmetic-seam
// golden: AggregateOpenRisk applied to the Crude ladder's actual per-Unit
// entries and (correctly raised) stops, at one, two, three and four Units.
func TestAggregateOpenRiskCrudeLadderAtEachRungCount(t *testing.T) {
	t.Parallel()

	const quantity = int64(100)
	const dollarsPerPoint = 1.0

	unit1Stop := crudeStop(t, crudeFirstFill)
	unit1StopAtTwo, err := sizing.RaisedStop(unit1Stop, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit2Stop := crudeStop(t, crudeSecondFill)
	unit1StopAtThree, err := sizing.RaisedStop(unit1StopAtTwo, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit2StopAtThree, err := sizing.RaisedStop(unit2Stop, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit3Stop := crudeStop(t, crudeThirdFill)
	unit1StopAtFour, err := sizing.RaisedStop(unit1StopAtThree, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit2StopAtFour, err := sizing.RaisedStop(unit2StopAtThree, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit3StopAtFour, err := sizing.RaisedStop(unit3Stop, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit4Stop := crudeStop(t, crudeFourthFill)

	tests := []struct {
		name  string
		units []sizing.UnitOpenRisk
		want  float64
	}{
		{
			name:  "one unit",
			units: []sizing.UnitOpenRisk{crudeUnitOpenRisk(t, crudeFirstFill, unit1Stop, quantity)},
			want:  float64(quantity) * 2 * crudeN,
		},
		{
			name: "two units",
			units: []sizing.UnitOpenRisk{
				crudeUnitOpenRisk(t, crudeFirstFill, unit1StopAtTwo, quantity),
				crudeUnitOpenRisk(t, crudeSecondFill, unit2Stop, quantity),
			},
			want: float64(quantity) * 3.5 * crudeN,
		},
		{
			name: "three units",
			units: []sizing.UnitOpenRisk{
				crudeUnitOpenRisk(t, crudeFirstFill, unit1StopAtThree, quantity),
				crudeUnitOpenRisk(t, crudeSecondFill, unit2StopAtThree, quantity),
				crudeUnitOpenRisk(t, crudeThirdFill, unit3Stop, quantity),
			},
			want: float64(quantity) * 4.5 * crudeN,
		},
		{
			name: "four units",
			units: []sizing.UnitOpenRisk{
				crudeUnitOpenRisk(t, crudeFirstFill, unit1StopAtFour, quantity),
				crudeUnitOpenRisk(t, crudeSecondFill, unit2StopAtFour, quantity),
				crudeUnitOpenRisk(t, crudeThirdFill, unit3StopAtFour, quantity),
				crudeUnitOpenRisk(t, crudeFourthFill, unit4Stop, quantity),
			},
			want: float64(quantity) * 5 * crudeN,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := sizing.AggregateOpenRisk(tt.units, dollarsPerPoint)
			if err != nil {
				t.Fatalf("AggregateOpenRisk() error = %v, want nil", err)
			}
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("AggregateOpenRisk() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAggregateOpenRiskCrudeGapCase covers the ticket's gap fixture: the
// fourth Unit's own (larger) risk does not distort the earlier Units', and
// the aggregate is the plain sum of all four, computed from each Unit's own
// entry and OWN current (correctly raised, per-Unit) stop.
func TestAggregateOpenRiskCrudeGapCase(t *testing.T) {
	t.Parallel()

	const quantity = int64(100)

	unit1Stop := crudeStop(t, crudeFirstFill)
	unit1StopAtTwo, err := sizing.RaisedStop(unit1Stop, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit2Stop := crudeStop(t, crudeSecondFill)
	unit1StopAtThree, err := sizing.RaisedStop(unit1StopAtTwo, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit2StopAtThree, err := sizing.RaisedStop(unit2Stop, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit3Stop := crudeStop(t, crudeThirdFill)
	gapUnit1Stop, err := sizing.RaisedStop(unit1StopAtThree, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	gapUnit2Stop, err := sizing.RaisedStop(unit2StopAtThree, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	gapUnit3Stop, err := sizing.RaisedStop(unit3Stop, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	gapUnit4Stop := crudeStop(t, crudeGapFourthFill)

	units := []sizing.UnitOpenRisk{
		crudeUnitOpenRisk(t, crudeFirstFill, gapUnit1Stop, quantity),
		crudeUnitOpenRisk(t, crudeSecondFill, gapUnit2Stop, quantity),
		crudeUnitOpenRisk(t, crudeThirdFill, gapUnit3Stop, quantity),
		crudeUnitOpenRisk(t, crudeGapFourthFill, gapUnit4Stop, quantity),
	}

	got, err := sizing.AggregateOpenRisk(units, 1.0)
	if err != nil {
		t.Fatalf("AggregateOpenRisk() error = %v, want nil", err)
	}

	want := float64(quantity) * ((crudeFirstFill - gapUnit1Stop) + (crudeSecondFill - gapUnit2Stop) + (crudeThirdFill - gapUnit3Stop) + (crudeGapFourthFill - gapUnit4Stop))
	if got != want {
		t.Errorf("AggregateOpenRisk() = %v, want exactly %v", got, want)
	}
	// The earlier units' distance from the FOURTH unit's own (gapped) fill
	// is what grows in the gap case, even though each earlier unit's own
	// stop rose by only the standard 1/2N: unit 1's distance from the
	// fourth unit's fill is larger here than in the normal (non-gap) four-
	// unit case, proving the gap is reflected in the aggregate rather than
	// averaged away.
	gapUnit1DistanceFromFourthFill := crudeGapFourthFill - gapUnit1Stop
	normalUnit1DistanceFromFourthFill := crudeFourthFill - unit1StopAtThree // one raise short of "at four", for comparison against the SAME (pre-fourth-add) unit 1 stop
	if !(gapUnit1DistanceFromFourthFill > normalUnit1DistanceFromFourthFill) {
		t.Errorf("gap case unit 1's distance from the fourth fill %v is not larger than the normal case's %v", gapUnit1DistanceFromFourthFill, normalUnit1DistanceFromFourthFill)
	}
}

// TestAggregateOpenRiskRejectsFreshRiskPerUnit is the ticket's required
// negative: a fourth Unit given a FRESH full 2N stop (never raised, as if
// each Add reset the whole Campaign's risk budget — the prototype's
// risk-multiplication bug) must sum to 4 x a single Unit's own 2N risk, not
// the correctly-raised ladder's 5N. This is not an error case for
// AggregateOpenRisk itself (both are legitimate-shaped inputs); it is the
// PROOF that the function does not silently paper over the difference — the
// aggregate genuinely differs, and by exactly the amount the bug would
// inflate risk by.
func TestAggregateOpenRiskRejectsFreshRiskPerUnit(t *testing.T) {
	t.Parallel()

	const quantity = int64(100)
	entries := []float64{crudeFirstFill, crudeSecondFill, crudeThirdFill, crudeFourthFill}

	// The CORRECT ladder: every earlier unit's stop raised by the standard
	// amount, converging on 27.70 (see TestAggregateOpenRiskCrudeLadderAtEachRungCount).
	unit1Stop := crudeStop(t, crudeFirstFill)
	unit1StopAtTwo, err := sizing.RaisedStop(unit1Stop, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit2Stop := crudeStop(t, crudeSecondFill)
	unit1StopAtThree, err := sizing.RaisedStop(unit1StopAtTwo, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit2StopAtThree, err := sizing.RaisedStop(unit2Stop, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit3Stop := crudeStop(t, crudeThirdFill)
	unit1StopAtFour, err := sizing.RaisedStop(unit1StopAtThree, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit2StopAtFour, err := sizing.RaisedStop(unit2StopAtThree, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit3StopAtFour, err := sizing.RaisedStop(unit3Stop, crudeN)
	if err != nil {
		t.Fatalf("RaisedStop() error = %v", err)
	}
	unit4Stop := crudeStop(t, crudeFourthFill)

	correctlyLadderedStops := []float64{unit1StopAtFour, unit2StopAtFour, unit3StopAtFour, unit4Stop}
	correctUnits := make([]sizing.UnitOpenRisk, 4)
	for i := range entries {
		correctUnits[i] = crudeUnitOpenRisk(t, entries[i], correctlyLadderedStops[i], quantity)
	}
	correctAggregate, err := sizing.AggregateOpenRisk(correctUnits, 1.0)
	if err != nil {
		t.Fatalf("AggregateOpenRisk(correct) error = %v", err)
	}
	wantCorrect := float64(quantity) * 5 * crudeN
	if math.Abs(correctAggregate-wantCorrect) > 1e-9 {
		t.Fatalf("correct aggregate = %v, want %v (5N, the ladder ADR 0003 describes)", correctAggregate, wantCorrect)
	}

	// The BUG: every unit given a fresh, un-raised 2N stop of its OWN, as if
	// each Add reset the risk budget rather than the earlier Units' stops
	// ever rising.
	freshUnits := make([]sizing.UnitOpenRisk, 4)
	for i, entry := range entries {
		stop, err := sizing.ProtectiveStopLevel(entry, crudeN, crudeStopMultiple, sizing.DirectionLong)
		if err != nil {
			t.Fatalf("ProtectiveStopLevel(%v) error = %v", entry, err)
		}
		freshUnits[i] = crudeUnitOpenRisk(t, entry, stop, quantity)
	}
	freshAggregate, err := sizing.AggregateOpenRisk(freshUnits, 1.0)
	if err != nil {
		t.Fatalf("AggregateOpenRisk(fresh) error = %v", err)
	}
	wantFresh := float64(quantity) * 4 * crudeStopMultiple * crudeN
	if math.Abs(freshAggregate-wantFresh) > 1e-9 {
		t.Fatalf("fresh-risk aggregate = %v, want %v (4 x 2N, the prototype's bug)", freshAggregate, wantFresh)
	}

	if !(freshAggregate > correctAggregate) {
		t.Fatalf("fresh-risk aggregate %v is not greater than the correctly-laddered aggregate %v; this fixture must keep the bug's inflation visible", freshAggregate, correctAggregate)
	}
	// The inflation is exactly 3N-worth per unit-quantity: 8N - 5N = 3N.
	wantInflation := float64(quantity) * 3 * crudeN
	if math.Abs((freshAggregate-correctAggregate)-wantInflation) > 1e-9 {
		t.Errorf("aggregate inflation = %v, want %v (3N x quantity)", freshAggregate-correctAggregate, wantInflation)
	}
}

// TestAggregateOpenRiskTreatsStopAtOrAboveEntryAsZeroRisk is the #15
// review-round fixture ("Valid Stop Raises Fail"): repeated half-N raises
// under a narrow enough Stop Multiple can legitimately lift an earlier
// Unit's stop to or above its own entry (a break-even or
// profit-protecting level, CONTEXT.md's "risk-free"), and such a Unit must
// contribute exactly ZERO to the aggregate — never a negative figure, and
// never a validation error.
func TestAggregateOpenRiskTreatsStopAtOrAboveEntryAsZeroRisk(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		unit sizing.UnitOpenRisk
	}{
		{name: "stop equal to entry (break-even)", unit: sizing.UnitOpenRisk{EntryPrice: 100, ProtectiveStop: 100, Quantity: 10}},
		{name: "stop above entry (profit-protecting)", unit: sizing.UnitOpenRisk{EntryPrice: 100, ProtectiveStop: 110, Quantity: 10}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := sizing.AggregateOpenRisk([]sizing.UnitOpenRisk{tt.unit}, 1.0)
			if err != nil {
				t.Fatalf("AggregateOpenRisk() error = %v, want nil", err)
			}
			if got != 0 {
				t.Errorf("AggregateOpenRisk() = %v, want exactly 0", got)
			}
		})
	}

	// Mixed with a genuinely at-risk Unit: only that Unit's own distance
	// contributes.
	atRisk := sizing.UnitOpenRisk{EntryPrice: 200, ProtectiveStop: 190, Quantity: 5}
	riskFree := sizing.UnitOpenRisk{EntryPrice: 100, ProtectiveStop: 105, Quantity: 10}
	got, err := sizing.AggregateOpenRisk([]sizing.UnitOpenRisk{atRisk, riskFree}, 1.0)
	if err != nil {
		t.Fatalf("AggregateOpenRisk() error = %v, want nil", err)
	}
	want := (200.0 - 190.0) * 5.0
	if got != want {
		t.Errorf("AggregateOpenRisk() = %v, want exactly %v (only the at-risk unit's own distance)", got, want)
	}
}

// TestAggregateOpenRiskFailsClosed covers every non-finite/non-positive
// input and shape violation .greptile/rules.md requires to fail closed.
func TestAggregateOpenRiskFailsClosed(t *testing.T) {
	t.Parallel()

	validUnit := sizing.UnitOpenRisk{EntryPrice: 100, ProtectiveStop: 90, Quantity: 10}

	tests := []struct {
		name            string
		units           []sizing.UnitOpenRisk
		dollarsPerPoint float64
		wantErr         string
	}{
		{name: "no units", units: nil, dollarsPerPoint: 1, wantErr: "at least one unit is required"},
		{name: "zero dollars per point", units: []sizing.UnitOpenRisk{validUnit}, dollarsPerPoint: 0, wantErr: "dollars per point must be positive"},
		{name: "negative dollars per point", units: []sizing.UnitOpenRisk{validUnit}, dollarsPerPoint: -1, wantErr: "dollars per point must be positive"},
		{name: "non-finite dollars per point", units: []sizing.UnitOpenRisk{validUnit}, dollarsPerPoint: math.NaN(), wantErr: "dollars per point must be finite"},
		{
			name:            "zero entry price",
			units:           []sizing.UnitOpenRisk{{EntryPrice: 0, ProtectiveStop: 90, Quantity: 10}},
			dollarsPerPoint: 1,
			wantErr:         "entry price must be positive",
		},
		{
			name:            "non-finite entry price",
			units:           []sizing.UnitOpenRisk{{EntryPrice: math.Inf(1), ProtectiveStop: 90, Quantity: 10}},
			dollarsPerPoint: 1,
			wantErr:         "entry price must be finite",
		},
		{
			name:            "zero protective stop",
			units:           []sizing.UnitOpenRisk{{EntryPrice: 100, ProtectiveStop: 0, Quantity: 10}},
			dollarsPerPoint: 1,
			wantErr:         "protective stop must be positive",
		},
		{
			name:            "zero quantity",
			units:           []sizing.UnitOpenRisk{{EntryPrice: 100, ProtectiveStop: 90, Quantity: 0}},
			dollarsPerPoint: 1,
			wantErr:         "quantity must be a positive whole number",
		},
		{
			name:            "negative quantity",
			units:           []sizing.UnitOpenRisk{{EntryPrice: 100, ProtectiveStop: 90, Quantity: -5}},
			dollarsPerPoint: 1,
			wantErr:         "quantity must be a positive whole number",
		},
		{
			name: "second unit invalid",
			units: []sizing.UnitOpenRisk{
				validUnit,
				{EntryPrice: 100, ProtectiveStop: 0, Quantity: 10},
			},
			dollarsPerPoint: 1,
			wantErr:         "unit 1: protective stop must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := sizing.AggregateOpenRisk(tt.units, tt.dollarsPerPoint)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("AggregateOpenRisk() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}
