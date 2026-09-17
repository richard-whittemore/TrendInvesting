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
		// A strategy id that carried a "/" composes to a string that reads
		// as a different id and rules version. Refused, not guessed at.
		{"a strategy id that carried a slash", "desk/turtle/1.1.0+abc1234"},
		{"a strategy id that carried two slashes", "a/b/c/1.1.0+abc1234"},
		{"strategy id with invalid characters", "turtle baseline/1.1.0+abc1234"},
		{"strategy id too long", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/1.1.0+abc1234"},
		{"rules version with invalid characters", "turtle-baseline/1.1.0\n+abc1234"},
		{"rules version too long", "turtle-baseline/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa+abc1234"},
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

// TestComposeDecomposeRoundTripsOrRefusesDelimiterBearingParts is the
// property that matters for a strategy version used as evidence: whatever
// goes in either comes back out exactly, or is refused. There is no third
// outcome in which Decompose returns parts that differ from the ones
// Compose was given.
//
// The case that made this necessary: a StrategyID of "desk/turtle" was legal
// (configuration validation required only that it be non-empty), and
// decomposing its composed form returned the id "desk" and the rules version
// "turtle/1.1.0" with no error at all. Replay would then refuse the journal
// for a rules-version mismatch that never happened, blaming the journal for a
// change in the engine — a wrong cause reported confidently, which is worse
// than no detection.
func TestComposeDecomposeRoundTripsOrRefusesDelimiterBearingParts(t *testing.T) {
	t.Parallel()

	legal := []struct {
		name                            string
		strategyID, rulesVersion, build string
	}{
		{"ordinary parts", "turtle-baseline", "1.1.0", "abc1234"},
		{"build carrying a plus", "turtle-baseline", "1.1.0", "abc1234+dirty"},
		{"build carrying a slash", "turtle-baseline", "1.1.0", "feature/x"},
		{"build carrying both", "turtle-baseline", "1.1.0", "feature/x+dirty"},
		{"boundary legal id", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "1.1.0", "abc1234"},
	}
	for _, tt := range legal {
		t.Run(tt.name, func(t *testing.T) {
			composed := event.ComposeStrategyVersion(tt.strategyID, tt.rulesVersion, tt.build)
			gotID, gotRules, gotBuild, err := event.DecomposeStrategyVersion(composed)
			if err != nil {
				t.Fatalf("DecomposeStrategyVersion(%q) error = %v", composed, err)
			}
			if gotID != tt.strategyID || gotRules != tt.rulesVersion || gotBuild != tt.build {
				t.Fatalf("round trip of (%q, %q, %q) returned (%q, %q, %q)",
					tt.strategyID, tt.rulesVersion, tt.build, gotID, gotRules, gotBuild)
			}
		})
	}

	// A delimiter-bearing id is refused where it enters the system. This is
	// the guarantee; Decompose's own check is only a second line, and
	// deliberately not asserted here because it cannot catch every case —
	// see the case below.
	for _, id := range []string{"desk/turtle", "desk+turtle", "desk/turtle+eu"} {
		t.Run("configuration refuses "+id, func(t *testing.T) {
			cfg := validConfiguration()
			cfg.StrategyID = id
			if err := cfg.Validate(); err == nil {
				t.Fatalf("ConfigurationPayload.Validate() accepted strategy id %q; a run must never be able to compose a strategy version from it", id)
			}
		})
	}
}

// TestAnAmbiguousStrategyVersionIsUnreachableRatherThanDetectable records why
// configuration validation, and not DecomposeStrategyVersion, is where the
// ambiguity is actually stopped — so that a later reader does not "simplify"
// this by deleting the configuration check and trusting the parse.
//
// A strategy id of "desk/turtle+eu" composes to a string that decomposes,
// with no error and no ambiguity visible in it, to a DIFFERENT and entirely
// legal triple: the id "desk", the rules version "turtle", and the build
// "eu/1.1.0+abc1234". Only the build may carry delimiters, so that reading is
// well-formed. Two different triples produce one string, and nothing in the
// string tells them apart.
func TestAnAmbiguousStrategyVersionIsUnreachableRatherThanDetectable(t *testing.T) {
	t.Parallel()

	const ambiguousID = "desk/turtle+eu"
	composed := event.ComposeStrategyVersion(ambiguousID, "1.1.0", "abc1234")

	gotID, gotRules, gotBuild, err := event.DecomposeStrategyVersion(composed)
	if err != nil {
		t.Fatalf("DecomposeStrategyVersion(%q) error = %v; this test exists because it does NOT error, so the reasoning behind the configuration-time check needs re-reading", composed, err)
	}
	if gotID == ambiguousID {
		t.Fatalf("DecomposeStrategyVersion(%q) recovered the original id %q; if the parse can now do this, the configuration-time check may be reconsidered", composed, gotID)
	}
	t.Logf("%q decomposes to a different legal triple: (%q, %q, %q)", composed, gotID, gotRules, gotBuild)

	cfg := validConfiguration()
	cfg.StrategyID = ambiguousID
	if err := cfg.Validate(); err == nil {
		t.Fatal("ConfigurationPayload.Validate() accepted the ambiguous strategy id; this is the only check standing between it and a journal nobody can decompose correctly")
	}
}
