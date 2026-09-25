package strategy

import (
	"fmt"
	"math/rand"
	"testing"
)

// TestWiderCapEntryAndAddProperty uses synthetic frozen correlation groups
// because ADR 0008's current event seam has no classification input. The
// Baseline's 4/6/10 limits remain in force while total long is 12, 24 or 36.
func TestWiderCapEntryAndAddProperty(t *testing.T) {
	for _, limit := range []int{12, 24, 36} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			r := newConfiguredReducerForInvariantTest(t)
			r.maxUnits, r.maxUnitsPerIndustry, r.maxUnitsPerSector, r.maxUnitsTotalLong = 4, 6, 10, limit
			r.instruments = map[string]*instrumentState{}
			rng := rand.New(rand.NewSource(44))
			total, adds, declines := 0, 0, 0
			for range 1000 {
				index := rng.Intn(40)
				id := fmt.Sprintf("name-%d", index)
				group := classification{Industry: fmt.Sprintf("industry-%d", index/2), Sector: fmt.Sprintf("sector-%d", index/4)}
				tx := r.begin()
				capName, capLimit, exposure, exceeded := tx.capExceeded(id, group)
				if exceeded {
					if capName == "total-long" {
						declines++
						if capLimit != limit || exposure != total+1 {
							t.Fatalf("decline %d/%d, total %d", exposure, capLimit, total)
						}
					}
					continue
				}
				if r.instruments[id] == nil {
					r.instruments[id] = &instrumentState{campaign: &campaignState{classification: group}}
				} else {
					adds++
				}
				r.instruments[id].campaign.units = append(r.instruments[id].campaign.units, unitState{})
				total++
				if total > limit {
					t.Fatalf("post-trade exposure %d exceeds %d", total, limit)
				}
			}
			if total != limit || adds == 0 || declines == 0 {
				t.Fatalf("total=%d adds=%d declines=%d", total, adds, declines)
			}
		})
	}
}
