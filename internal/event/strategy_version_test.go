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
