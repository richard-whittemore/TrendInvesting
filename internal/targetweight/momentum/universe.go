package momentum

import (
	"fmt"
	"sort"
)

// Row is one listed stock of a universe snapshot: its listing type, one
// session's unadjusted close and volume, and its sector label with the
// label's source (ADR 0026 point 1, market.universe.snapshot). An empty
// Sector means neither source labelled it.
type Row struct {
	InstrumentID string
	Type         string
	Close        float64
	Volume       float64
	Sector       string
	SectorSource string
}

// Member is a Row that joined the universe, with its dollar-volume rank
// (1 is the largest) and the dollar volume it was ranked by.
type Member struct {
	Row
	Rank         int
	DollarVolume float64
}

// UniverseCounts records how many rows each universe filter kept.
type UniverseCounts struct {
	Rows           int
	CommonStock    int
	AtOrAboveFloor int
	Members        int
}

// RankUniverse keeps the rows of cfg.ListingType whose unadjusted close is
// at least cfg.MinPrice, ranks them by close × volume, largest first, and
// returns the first cfg.UniverseSize (defensive_v2.py:174-176; plan.md
// section 4, D8 "rank").
//
// Ties in dollar volume are broken by ascending instrument ID, so the
// result never depends on the order of rows. The backtest sorted stably in
// the order LEAN's coarse data arrived, which this cannot reproduce
// (ADR 0026 point 8).
//
// The backtest's HasFundamentalData test has no free equivalent and is not
// applied (plan.md section 4, D8 "known gaps"). A configuration that fails
// Validate is an error.
func RankUniverse(cfg Config, rows []Row) ([]Member, UniverseCounts, error) {
	if err := cfg.Validate(); err != nil {
		return nil, UniverseCounts{}, err
	}
	counts := UniverseCounts{Rows: len(rows)}
	seen := make(map[string]bool, len(rows))
	var kept []Member
	for _, r := range rows {
		if err := r.validate(); err != nil {
			return nil, UniverseCounts{}, err
		}
		if seen[r.InstrumentID] {
			return nil, UniverseCounts{}, fmt.Errorf("momentum: universe row %s repeated", r.InstrumentID)
		}
		seen[r.InstrumentID] = true
		if r.Type != cfg.ListingType {
			continue
		}
		counts.CommonStock++
		if r.Close < cfg.MinPrice {
			continue
		}
		counts.AtOrAboveFloor++
		kept = append(kept, Member{Row: r, DollarVolume: r.Close * r.Volume})
	}
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].DollarVolume != kept[j].DollarVolume {
			return kept[i].DollarVolume > kept[j].DollarVolume
		}
		return kept[i].InstrumentID < kept[j].InstrumentID
	})
	if len(kept) > cfg.UniverseSize {
		kept = kept[:cfg.UniverseSize]
	}
	for i := range kept {
		kept[i].Rank = i + 1
	}
	counts.Members = len(kept)
	return kept, counts, nil
}

func (r Row) validate() error {
	switch {
	case r.InstrumentID == "":
		return fmt.Errorf("momentum: universe row with empty instrument ID")
	case !finite(r.Close) || r.Close < 0:
		return fmt.Errorf("momentum: universe row %s close %v", r.InstrumentID, r.Close)
	case !finite(r.Volume) || r.Volume < 0:
		return fmt.Errorf("momentum: universe row %s volume %v", r.InstrumentID, r.Volume)
	}
	return nil
}
