package strategy

// This file is deliberately `package strategy`, not `strategy_test` — the
// same "test-only path" shape invariant_test.go documents for
// checkCampaignHasAProtectiveStop: it reaches ADR 0008's industry and sector
// caps by constructing campaignState directly through this package's own
// unexported fields, never through any exported production API.
//
// Why a white-box test is the honest way to cover this: classificationOf
// (unit_caps.go) is #36's seam for a point-in-time industry/sector label,
// and no classification input event exists yet (#37/#41 bring the
// point-in-time universe and its labels) — so today it always reports
// Unclassified, and CapIndustry/CapSector can never actually be REACHED
// through the event seam (unit_caps_test.go's tests exercise CapInstrument,
// CapUnclassifiedGroup and CapTotalLong, which are reachable today). The
// industry/sector grouping arithmetic itself — capExceeded, industryUnits,
// sectorUnits — is nonetheless real, already-committed code that must not
// silently break once #37/#41 supply real labels, so it is tested here
// directly, exactly as classificationOf's own doc comment says it will be
// used the day a real label exists.

import "testing"

// TestCapExceededGroupsByIndustryThenSector proves the industry and sector
// groupings are independent of one another and of the Unclassified Group:
// two classified Campaigns sharing an industry (but not a sector) bind the
// industry cap; two sharing a sector (but not an industry) bind the sector
// cap; and neither counts toward, or is counted by, an Unclassified
// instrument's own group exposure.
func TestCapExceededGroupsByIndustryThenSector(t *testing.T) {
	t.Parallel()

	classified := func(industry, sector string, units int) *instrumentState {
		return &instrumentState{campaign: &campaignState{
			classification: classification{Industry: industry, Sector: sector},
			units:          make([]unitState, units),
		}}
	}

	t.Run("industry cap binds before the generous sector cap", func(t *testing.T) {
		t.Parallel()
		r := newConfiguredReducerForInvariantTest(t)
		r.maxUnits = 1_000_000
		r.maxUnitsPerIndustry = 2
		r.maxUnitsPerSector = 1_000_000
		r.maxUnitsTotalLong = 1_000_000
		r.instruments = map[string]*instrumentState{
			"AAA": classified("Tech", "Software", 1),
			"BBB": classified("Tech", "Hardware", 1), // same industry, different sector
		}
		tx := r.begin()
		capName, limit, exposure, exceeded := tx.capExceeded("CCC", classification{Industry: "Tech", Sector: "Networking"})
		if !exceeded {
			t.Fatal("capExceeded() ok = false, want true")
		}
		if capName != "industry" {
			t.Errorf("cap = %q, want %q", capName, "industry")
		}
		if limit != 2 {
			t.Errorf("limit = %d, want 2", limit)
		}
		if exposure != 3 {
			t.Errorf("exposure = %d, want 3 (AAA and BBB's Units plus CCC's proposed one)", exposure)
		}
	})

	t.Run("sector cap binds when industries differ but the sector is shared", func(t *testing.T) {
		t.Parallel()
		r := newConfiguredReducerForInvariantTest(t)
		r.maxUnits = 1_000_000
		r.maxUnitsPerIndustry = 1_000_000
		r.maxUnitsPerSector = 2
		r.maxUnitsTotalLong = 1_000_000
		r.instruments = map[string]*instrumentState{
			"AAA": classified("Tech", "Consumer Discretionary", 1),
			"BBB": classified("Retail", "Consumer Discretionary", 1), // same sector, different industry
		}
		tx := r.begin()
		capName, limit, exposure, exceeded := tx.capExceeded("CCC", classification{Industry: "Media", Sector: "Consumer Discretionary"})
		if !exceeded {
			t.Fatal("capExceeded() ok = false, want true")
		}
		if capName != "sector" {
			t.Errorf("cap = %q, want %q", capName, "sector")
		}
		if limit != 2 {
			t.Errorf("limit = %d, want 2", limit)
		}
		if exposure != 3 {
			t.Errorf("exposure = %d, want 3", exposure)
		}
	})

	t.Run("a classified instrument's Units do not join the Unclassified Group", func(t *testing.T) {
		t.Parallel()
		r := newConfiguredReducerForInvariantTest(t)
		r.maxUnits = 1_000_000
		r.maxUnitsPerIndustry = 1_000_000
		r.maxUnitsPerSector = 1 // the Unclassified Group's own cap
		r.maxUnitsTotalLong = 1_000_000
		r.instruments = map[string]*instrumentState{
			"AAA": classified("Tech", "Software", 1),
		}
		tx := r.begin()
		// DDD is genuinely Unclassified (classificationOf's only answer
		// today), so its own entry is checked against the Unclassified
		// Group alone. AAA's classified Unit must not count toward it.
		if got := tx.unclassifiedGroupUnits(); got != 0 {
			t.Fatalf("unclassifiedGroupUnits() = %d, want 0: AAA is classified, not Unclassified", got)
		}
		_, _, _, exceeded := tx.capExceeded("DDD", tx.classificationOf("DDD"))
		if exceeded {
			t.Fatal("capExceeded(DDD) ok = true, want false: the Unclassified Group is still empty")
		}
	})
}

// TestACampaignsClassificationIsFrozenAtOpenNotReReadLater is #36's own
// named acceptance criterion: "an instrument whose classification changes
// mid-Campaign does not retroactively invalidate the open Campaign."
//
// classificationOf always reports Unclassified today (no classification
// input event exists — #37/#41), so there is no real event that can make an
// instrument's classification "change" through the production event seam
// yet. What IS real today is the freeze-at-entry design itself
// (campaignState.classification, set once in openCampaign and never
// re-read): this test proves that design by mutating an already-open
// Campaign's frozen field directly — standing in for the classification
// input #37/#41 will one day deliver after the Campaign has already opened
// — and showing every cap computation keeps using what was frozen, never a
// fresh classificationOf call.
func TestACampaignsClassificationIsFrozenAtOpenNotReReadLater(t *testing.T) {
	t.Parallel()

	r := newConfiguredReducerForInvariantTest(t)
	r.maxUnits = 1_000_000
	r.maxUnitsPerIndustry = 1_000_000
	r.maxUnitsPerSector = 1_000_000
	r.maxUnitsTotalLong = 1_000_000
	r.instruments = map[string]*instrumentState{
		"AAA": {campaign: &campaignState{instrumentID: "AAA", classification: unclassifiedClassification, units: []unitState{{}}}},
	}

	// AAA opened Unclassified — classificationOf's only possible answer at
	// the moment its Campaign opened.
	if got := r.instruments["AAA"].campaign.classification; got != unclassifiedClassification {
		t.Fatalf("fixture error: AAA is not Unclassified at open, got %+v", got)
	}

	// A classification input for AAA arrives AFTER its Campaign is already
	// open (#37/#41's own future input). Simulated here by mutating the
	// Campaign's frozen field directly, since openCampaign is the only
	// production write site and nothing drives a second one today.
	r.instruments["AAA"].campaign.classification = classification{Industry: "Tech", Sector: "Software"}

	tx := r.begin()
	// AAA's own Campaign no longer belongs to the Unclassified Group — this
	// is NOT the "retroactive invalidation" #36 forbids: nothing has
	// reopened, resized, or exited AAA's Campaign, only what group a LATER
	// decision would count it under.
	if got := tx.unclassifiedGroupUnits(); got != 0 {
		t.Errorf("unclassifiedGroupUnits() = %d, want 0: AAA's frozen classification changed away from Unclassified", got)
	}
	if got := tx.industryUnits("Tech"); got != 1 {
		t.Errorf("industryUnits(Tech) = %d, want 1: AAA's mutated classification is what every cap computation reads", got)
	}
	// The per-instrument cap never depended on classification at all, and
	// is completely unaffected.
	if got := tx.instrumentUnits("AAA"); got != 1 {
		t.Errorf("instrumentUnits(AAA) = %d, want 1", got)
	}
	// classificationOf itself is unaffected by the mutation: it is a pure
	// lookup, consulted only once (openCampaign) for the whole life of a
	// Campaign. A hypothetical SECOND Campaign for AAA (impossible while
	// this one is open, but this is what the seam would report if asked
	// again) still reports Unclassified — proving the change visible above
	// came from the frozen field, never from classificationOf itself having
	// started reporting something new.
	if got := tx.classificationOf("AAA"); got != unclassifiedClassification {
		t.Errorf("classificationOf(AAA) = %+v, want Unclassified: the seam itself never changed, only the already-open Campaign's frozen field (simulating a later classification input)", got)
	}
}
