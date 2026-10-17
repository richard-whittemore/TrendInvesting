package momentum_test

import (
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

func TestRankUniverseKeepsCommonStockAtOrAboveTheFloorByDollarVolume(t *testing.T) {
	cfg := momentum.FCombo()
	cfg.UniverseSize = 3
	rows := []momentum.Row{
		{InstrumentID: "EEE", Type: "CS", Close: 20, Volume: 50},       // 1000, ties AAA
		{InstrumentID: "BBB", Type: "ETF", Close: 100, Volume: 1000},   // not common stock
		{InstrumentID: "CCC", Type: "CS", Close: 5, Volume: 300},       // exactly $5: kept, 1500
		{InstrumentID: "DDD", Type: "CS", Close: 4.99, Volume: 100000}, // under the floor
		{InstrumentID: "AAA", Type: "CS", Close: 10, Volume: 100},      // 1000
		{InstrumentID: "FFF", Type: "CS", Close: 1000, Volume: 0.5},    // 500, fourth: cut
	}
	members, counts, err := momentum.RankUniverse(cfg, rows)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"CCC", "AAA", "EEE"}
	if len(members) != len(want) {
		t.Fatalf("members = %v, want %v", members, want)
	}
	for i, m := range members {
		mustEqual(t, "member", m.InstrumentID, want[i])
		mustEqual(t, "rank", m.Rank, i+1)
	}
	mustEqual(t, "dollar volume", members[0].DollarVolume, 1500.0)
	mustEqual(t, "counts", counts, momentum.UniverseCounts{Rows: 6, CommonStock: 5, AtOrAboveFloor: 4, Members: 3})
}

func TestRankUniverseRefusesMalformedRows(t *testing.T) {
	cfg := momentum.FCombo()
	good := momentum.Row{InstrumentID: "AAA", Type: "CS", Close: 10, Volume: 100}
	cases := map[string]func(*momentum.Row){
		"empty id":        func(r *momentum.Row) { r.InstrumentID = "" },
		"NaN close":       func(r *momentum.Row) { r.Close = math.NaN() },
		"negative close":  func(r *momentum.Row) { r.Close = -1 },
		"infinite volume": func(r *momentum.Row) { r.Volume = math.Inf(1) },
		"negative volume": func(r *momentum.Row) { r.Volume = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			row := good
			mutate(&row)
			if _, _, err := momentum.RankUniverse(cfg, []momentum.Row{row}); err == nil {
				t.Fatal("want an error")
			}
		})
	}
	t.Run("duplicate id", func(t *testing.T) {
		if _, _, err := momentum.RankUniverse(cfg, []momentum.Row{good, good}); err == nil {
			t.Fatal("want an error")
		}
	})
}
