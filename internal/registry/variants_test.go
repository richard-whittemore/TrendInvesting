package registry_test

import (
	"github.com/richard-whittemore/TrendInvesting/internal/registry"
	"testing"
)

func TestWiderTotalLongCapChangesOnlyDeclaredDimension(t *testing.T) {
	baseline := completedRun("baseline").Configuration
	baseline.MaxUnitsTotalLong = 12
	for _, tc := range []struct {
		id  string
		cap int
	}{{"total-long-cap-24", 24}, {"total-long-cap-36", 36}} {
		got, err := registry.WiderTotalLongCap(baseline, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		want := baseline
		want.StrategyID = tc.id
		want.MaxUnitsTotalLong = tc.cap
		if got != want {
			t.Fatalf("Variant changed another dimension: %+v", got)
		}
	}
	if baseline.MaxUnitsTotalLong != 12 {
		t.Fatal("Baseline mutated")
	}
	if _, err := registry.WiderTotalLongCap(baseline, "total-long-cap-25"); err == nil {
		t.Fatal("undeclared cap accepted")
	}
}
