package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// TestVerifyReportsRunCompleteness distinguishes retained failed evidence
// (ADR 0012) from event.RunCompletedEventType's end of the input stream.
func TestVerifyReportsRunCompleteness(t *testing.T) {
	for _, complete := range []bool{true, false} {
		name := "complete"
		if !complete {
			name = "incomplete"
		}
		t.Run(name, func(t *testing.T) {
			original := runBar
			t.Cleanup(func() { runBar = original })
			failure := errors.New("simulator stopped on the second bar")
			calls := 0
			if !complete {
				runBar = func(ctx context.Context, sim *fills.Simulator, handler replay.Handler, bar event.Envelope) (fills.Result, error) {
					calls++
					if calls == 2 {
						return fills.Result{}, failure
					}
					return original(ctx, sim, handler, bar)
				}
			}
			path := filepath.Join(t.TempDir(), "journal.jsonl")
			cfg := fixtureConfiguration(t)
			result := perform(context.Background(), options{
				configPath: configurationFixture, barsPath: barsFixture, outPath: path, build: testBuild,
			}, cfg, event.ConfigurationHash(cfg), event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, testBuild))
			if !result.installed || result.journalErr != nil {
				t.Fatalf("installed = %v, journal error = %v", result.installed, result.journalErr)
			}
			if complete && result.runErr != nil {
				t.Fatalf("completed fixture: %v", result.runErr)
			}
			if !complete && (!errors.Is(result.runErr, failure) || calls != 2) {
				t.Fatalf("mid-stream failure = %v, bar calls = %d", result.runErr, calls)
			}
			verification := verifyJournal(t, path)
			if verification.Complete != complete {
				t.Errorf("Complete = %v, want %v", verification.Complete, complete)
			}
			var out bytes.Buffer
			if err := run(context.Background(), []string{"-verify", path}, &out); err != nil {
				t.Fatalf("run(-verify): %v", err)
			}
			want := "run                complete\n"
			if !complete {
				want = "run                INCOMPLETE — final input is not replay.run.completed; the run did not finish\n"
			}
			if !strings.Contains(out.String(), want) || !strings.Contains(out.String(), "chain              verified\n") {
				t.Errorf("verify report missing %q or verified chain:\n%s", want, out.String())
			}
			t.Logf("-verify output:\n%s", out.String())
		})
	}
}
