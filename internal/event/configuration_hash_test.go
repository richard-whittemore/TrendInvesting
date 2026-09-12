package event_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestConfigurationHashStability pins the Baseline fixture's hash (#50, ADR
// 0016). Changing this pin is a deliberate act: either ConfigurationSchemaVersion
// bumped, or the canonical encoding changed, and either should be visible as
// a diff to this literal, not silently absorbed. ADR 0012 requires "the same
// configuration always produces the same hash" — this is what makes that
// checkable.
func TestConfigurationHashStability(t *testing.T) {
	t.Parallel()

	got := event.ConfigurationHash(validConfiguration())
	const want = "sha256:REPLACE_WITH_PINNED_HASH"
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
		{"slippage n", func(c *event.ConfigurationPayload) { c.SlippageN = 0.1 }},
		{"tier b distance in n", func(c *event.ConfigurationPayload) { c.TierBDistanceInN = 2.0 }},
		{"dollars per point", func(c *event.ConfigurationPayload) { c.DollarsPerPoint = 42000 }},
		{"risk at stop fraction", func(c *event.ConfigurationPayload) { c.RiskAtStopFraction = 0.02 }},
		{"notional account starting equity", func(c *event.ConfigurationPayload) { c.NotionalAccount.StartingEquity = 2_000_000 }},
		{"notional account rebasing month", func(c *event.ConfigurationPayload) { c.NotionalAccount.RebasingMonth = 6 }},
		{"notional account rebasing day", func(c *event.ConfigurationPayload) { c.NotionalAccount.RebasingDay = 15 }},
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
