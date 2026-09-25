package event_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestConfigurationHashStability pins the Baseline fixture's hash (ADR
// 0016). Changing this pin is a deliberate act: either ConfigurationSchemaVersion
// bumped, or the canonical encoding changed, and either should be visible as
// a diff to this literal, not silently absorbed. ADR 0012 requires "the same
// configuration always produces the same hash" — this is what makes that
// checkable.
func TestConfigurationHashStability(t *testing.T) {
	t.Parallel()

	got := event.ConfigurationHash(validConfiguration())
	// This pin includes ConfigurationSchemaVersion 7 and its three ADR 0009
	// universe thresholds (UniverseMinPrice, UniverseMinDollarVolume,
	// UniverseMinHistoryBars), all zero: the universe gate off, as ADR
	// 0009's amendment of 2026-09-25 (the owner's decision) makes every
	// existing fixture. The pin immediately before this one, with the same
	// schema version but all three thresholds at the Baseline's declared
	// values (5, 5,000,000, 250) rather than the gate-off zero every
	// existing fixture actually runs under, was
	// sha256:c3b6828fd5c55d289d657a0d7360e094b4f262b2409abbfb65d95d54a155560c.
	// The pin before that, for schema version 6, was
	// sha256:1bab80d294ae64f7cb84ae95168b74e3970023f9eb06209a14aecee731b6ac3d.
	//
	// That pin included ConfigurationSchemaVersion 6 and its BuyOrderType
	// and GapBufferN (ADR 0005, as amended 2026-09-24). The pin before that,
	// for schema version 5, was
	// sha256:bfbf4c6abc13336089e13fb46870052b82048eb7a372678390397f3401e7b272.
	//
	// That pin included ConfigurationSchemaVersion 5 and #55's three new
	// Unit-cap fields (MaxUnitsPerIndustry, MaxUnitsPerSector,
	// MaxUnitsTotalLong), alongside the Commission fields
	// (PerShare, MinimumPerOrder, MaximumFractionOfTradeValue) schema
	// version 4 added. ADR 0016 requires both the version and every field to
	// feed the hash: the schema version is inside the hashed prefix, and the
	// fields are inside the canonical bytes. This prevents a schema or
	// field change from being silently omitted from configuration identity.
	// The previous pin, for schema version 4 with no
	// MaxUnitsPerIndustry/MaxUnitsPerSector/MaxUnitsTotalLong, was
	// sha256:acdf9cc9f6b45ea4658373abff96306af35539d68ad2a1e0eedee4a014fbb386.
	// Schema 8 adds RecomputeNAtAdd=false to the frozen Baseline (ADR 0006).
	const want = "sha256:badfd0c1bf083e4d9f988a767267b3a71f6451cb06361500245cec447b97fe53"
	if got != want {
		t.Fatalf("ConfigurationHash(baseline) = %q, want the pinned hash %q", got, want)
	}
}

// TestConfigurationHashFormat confirms the exported string shape: prefixed
// "sha256:" so a reader can tell the algorithm from the string, followed by
// 64 lowercase hex characters (a SHA-256 digest).
func TestConfigurationHashFormat(t *testing.T) {
	t.Parallel()

	got := event.ConfigurationHash(validConfiguration())
	if !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("ConfigurationHash() = %q, want it prefixed %q", got, "sha256:")
	}
	digest := strings.TrimPrefix(got, "sha256:")
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) {
		t.Fatalf("ConfigurationHash() digest = %q, want 64 lowercase hex characters", digest)
	}
}

// TestConfigurationHashIsDeterministic confirms hashing the same payload
// twice yields byte-identical results — the baseline property ADR 0012's
// "same configuration, same hash" depends on.
func TestConfigurationHashIsDeterministic(t *testing.T) {
	t.Parallel()

	payload := validConfiguration()
	first := event.ConfigurationHash(payload)
	second := event.ConfigurationHash(payload)
	if first != second {
		t.Fatalf("ConfigurationHash() is not deterministic: %q != %q", first, second)
	}
}

// TestConfigurationHashChangesWithEveryField is the sensitivity test #50
// requires: every field, mutated one at a time away from the Baseline
// fixture, must change the hash. "Any parameter change produces a different
// [hash]" (ADR 0012) is only as trustworthy as this table is complete.
func TestConfigurationHashChangesWithEveryField(t *testing.T) {
	t.Parallel()

	base := validConfiguration()
	baseHash := event.ConfigurationHash(base)

	tests := []struct {
		name   string
		mutate func(*event.ConfigurationPayload)
	}{
		{"strategy id", func(c *event.ConfigurationPayload) { c.StrategyID = "turtle-variant" }},
		{"sizing mode", func(c *event.ConfigurationPayload) { c.SizingMode = event.SizingModeFixedRiskAtStop }},
		{"unit volatility fraction", func(c *event.ConfigurationPayload) { c.UnitVolatilityFraction = 0.01 }},
		{"stop multiple", func(c *event.ConfigurationPayload) { c.StopMultiple = 3 }},
		{"entry channel length", func(c *event.ConfigurationPayload) { c.EntryChannelLength = 20 }},
		{"exit channel length", func(c *event.ConfigurationPayload) { c.ExitChannelLength = 10 }},
		{"max units", func(c *event.ConfigurationPayload) { c.MaxUnits = 6 }},
		{"max units per industry", func(c *event.ConfigurationPayload) { c.MaxUnitsPerIndustry = 7 }},
		{"max units per sector", func(c *event.ConfigurationPayload) { c.MaxUnitsPerSector = 11 }},
		{"max units total long", func(c *event.ConfigurationPayload) { c.MaxUnitsTotalLong = 13 }},
		{"slippage n", func(c *event.ConfigurationPayload) { c.SlippageN = 0.1 }},
		{"tier b distance in n", func(c *event.ConfigurationPayload) { c.TierBDistanceInN = 2.0 }},
		{"dollars per point", func(c *event.ConfigurationPayload) { c.DollarsPerPoint = 42000 }},
		{"risk at stop fraction", func(c *event.ConfigurationPayload) { c.RiskAtStopFraction = 0.02 }},
		{"notional account starting equity", func(c *event.ConfigurationPayload) { c.NotionalAccount.StartingEquity = 2_000_000 }},
		{"notional account rebasing month", func(c *event.ConfigurationPayload) { c.NotionalAccount.RebasingMonth = 6 }},
		{"notional account rebasing day", func(c *event.ConfigurationPayload) { c.NotionalAccount.RebasingDay = 15 }},
		// #18's Commission fields (schema version 4): no exception to "every
		// field, one at a time" just because they arrived after this test
		// was first written.
		{"commission per share", func(c *event.ConfigurationPayload) { c.Commission.PerShare = 0.01 }},
		{"commission minimum per order", func(c *event.ConfigurationPayload) { c.Commission.MinimumPerOrder = 2.00 }},
		{"commission maximum fraction of trade value", func(c *event.ConfigurationPayload) { c.Commission.MaximumFractionOfTradeValue = 0.02 }},
		// ADR 0009's three universe thresholds (schema version 7): no
		// exception to "every field, one at a time" just because they
		// arrived after this test was first written.
		{"universe min price", func(c *event.ConfigurationPayload) { c.UniverseMinPrice = 10 }},
		{"universe min dollar volume", func(c *event.ConfigurationPayload) { c.UniverseMinDollarVolume = 10_000_000 }},
		{"universe min history bars", func(c *event.ConfigurationPayload) { c.UniverseMinHistoryBars = 300 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mutated := validConfiguration()
			tt.mutate(&mutated)
			got := event.ConfigurationHash(mutated)
			if got == baseHash {
				t.Fatalf("ConfigurationHash() unchanged after mutating %s: got %q, same as the baseline hash", tt.name, got)
			}
		})
	}
}
