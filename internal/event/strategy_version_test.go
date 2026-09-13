package event_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestComposeStrategyVersion pins the exact format ADR 0016 specifies:
// "<strategy-id>/<rules-version>+<build>".
func TestComposeStrategyVersion(t *testing.T) {
	t.Parallel()

	got := event.ComposeStrategyVersion("turtle-baseline", "1.0.0", "abc1234")
	const want = "turtle-baseline/1.0.0+abc1234"
	if got != want {
		t.Fatalf("ComposeStrategyVersion() = %q, want %q", got, want)
	}
}

// TestComposeStrategyVersionDistinguishesEveryInput confirms all three parts
// matter: two builds, two rules versions, or two strategy ids never compose
// to the same StrategyVersion, which is what lets a reviewer trust the
// string as provenance rather than as a label that happens to collide.
func TestComposeStrategyVersionDistinguishesEveryInput(t *testing.T) {
	t.Parallel()

	base := event.ComposeStrategyVersion("turtle-baseline", "1.0.0", "abc1234")

	tests := []struct {
		name string
		got  string
	}{
		{"strategy id", event.ComposeStrategyVersion("sublime-variant", "1.0.0", "abc1234")},
		{"rules version", event.ComposeStrategyVersion("turtle-baseline", "1.1.0", "abc1234")},
		{"build", event.ComposeStrategyVersion("turtle-baseline", "1.0.0", "def5678")},
	}
	for _, tt := range tests {
		if tt.got == base {
			t.Errorf("changing %s did not change the composed strategy version (got %q both times)", tt.name, tt.got)
		}
	}
}

// TestDecomposeStrategyVersionInvertsCompose confirms
// DecomposeStrategyVersion recovers exactly the three parts
// ComposeStrategyVersion was given, including a build identifier that
// itself contains "+" (a "-dirty" git describe suffix never does, but a
// build string is an opaque caller-supplied value, and Decompose must not
// assume it looks like anything in particular beyond the first "+").
func TestDecomposeStrategyVersionInvertsCompose(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                            string
		strategyID, rulesVersion, build string
	}{
		{"ordinary build", "turtle-baseline", "1.1.0", "abc1234"},
		{"build containing a plus", "sublime-variant", "2.0.0", "abc1234+dirty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			composed := event.ComposeStrategyVersion(tt.strategyID, tt.rulesVersion, tt.build)

			gotID, gotRules, gotBuild, err := event.DecomposeStrategyVersion(composed)
			if err != nil {
				t.Fatalf("DecomposeStrategyVersion(%q) error = %v", composed, err)
			}
			if gotID != tt.strategyID || gotRules != tt.rulesVersion || gotBuild != tt.build {
				t.Fatalf("DecomposeStrategyVersion(%q) = (%q, %q, %q), want (%q, %q, %q)",
					composed, gotID, gotRules, gotBuild, tt.strategyID, tt.rulesVersion, tt.build)
			}
		})
	}
}

// TestDecomposeStrategyVersionRejectsMalformedInput confirms every way a
// string can fail to be a ComposeStrategyVersion output is refused rather
// than partially decomposed.
func TestDecomposeStrategyVersionRejectsMalformedInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{"no slash at all", "turtle-baseline-1.1.0+abc1234"},
		{"no plus after the slash", "turtle-baseline/1.1.0-abc1234"},
		{"empty strategy id", "/1.1.0+abc1234"},
		{"empty rules version", "turtle-baseline/+abc1234"},
		{"empty build", "turtle-baseline/1.1.0+"},
		{"empty string", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := event.DecomposeStrategyVersion(tt.input)
			if err == nil {
				t.Fatalf("DecomposeStrategyVersion(%q) error = nil, want a refusal", tt.input)
			}
		})
	}
}
