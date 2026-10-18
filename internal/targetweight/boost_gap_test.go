package targetweight_test

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/boost"
)

// The rule these tests pin (ADR 0026, point 5, amended 2026-10-18, the
// owner's choice for a data failure): a session that closes without SPY's bar
// is withheld (missing-bars) and trades nothing; the indicators skip it, so
// the next bar continues RSI(2) and both means over the closes received, and
// the first decision that reads a bar after the gap names the missing
// sessions (gap_sessions). Later sessions decide as before. This is not a
// copy of the backtest: QuantConnect fills a missing daily bar forward with
// the previous close and decides on it.

// expectedReadings replays closes through Boost's indicators with the
// sessions in skip left out, as the rule says the handler must, and returns
// each session's readings (zero for a skipped session).
func expectedReadings(t *testing.T, closes []float64, skip map[int]bool) []boost.Readings {
	t.Helper()
	cfg := testConfiguration().Boost
	ind, err := boost.NewIndicators(boost.Parameters{RSIPeriod: cfg.RSIPeriod, TrendDays: cfg.TrendDays, ExitSMA: cfg.ExitSMA})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]boost.Readings, len(closes))
	for i, c := range closes {
		if !skip[i] {
			out[i] = ind.Add(c)
		}
	}
	return out
}

func sessionDate(n int) string { return sessionEnd(n).Format("2006-01-02") }

func TestAMissingBarIsSkippedAndBoostDecidesAcrossTheGap(t *testing.T) {
	for name, skip := range map[string]map[int]bool{
		"one session":  {12: true},
		"two sessions": {12: true, 13: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := simulate(t, options{skip: skip})
			decided := targets(t, r.decisions)
			if len(decided) != len(syntheticCloses()) {
				t.Fatalf("%d decisions for %d sessions", len(decided), len(syntheticCloses()))
			}
			want := expectedReadings(t, syntheticCloses(), skip)
			var gap []string
			for i, d := range decided {
				if skip[i] {
					gap = append(gap, sessionDate(i))
					if d.Outcome != event.TargetsOutcomeWithheld || d.WithheldReason != event.WithheldMissingBars {
						t.Errorf("session %d: %s %s, want withheld missing-bars", i, d.Outcome, d.WithheldReason)
					}
					if !strings.Contains(d.Detail, sessionDate(i)) || !strings.Contains(d.Detail, "skip") {
						t.Errorf("session %d detail should name the missing session and say it is skipped: %q", i, d.Detail)
					}
					if d.Boost.LatchedBefore != d.Boost.LatchedAfter || len(d.Boost.GapSessions) != 0 {
						t.Errorf("session %d: the latch moves or the gap is recorded on the withheld decision: %+v", i, d.Boost)
					}
					continue
				}
				if d.Outcome != event.TargetsOutcomeDecided {
					t.Errorf("session %d: %s %s, want decided", i, d.Outcome, d.Detail)
				}
				w := want[i]
				if d.Boost.RSI != w.RSI || d.Boost.TrendMean != w.TrendMean || d.Boost.ExitMean != w.ExitMean {
					t.Errorf("session %d: readings %+v, want those over the closes received %+v", i, d.Boost, w)
				}
				if !slices.Equal(d.Boost.GapSessions, gap) {
					t.Errorf("session %d: gap sessions %v, want %v", i, d.Boost.GapSessions, gap)
				}
				gap = nil
			}
			if n := len(planned(t, r.decisions)); n != 3 {
				t.Errorf("%d orders planned, want the first purchase, the entry and the exit", n)
			}
		})
	}
}

// TestAGapWhileBoostedNeverBlocksTheExit: the boost is latched from session
// 16; with session 18's bar missing, the exit is still planned when a close
// rises above the exit mean of the closes received.
func TestAGapWhileBoostedNeverBlocksTheExit(t *testing.T) {
	r := simulate(t, options{skip: map[int]bool{18: true}})
	orders := planned(t, r.decisions)
	if len(orders) != 3 || orders[2].Side != event.OrderSideSell {
		t.Fatalf("orders %+v, want the first purchase, the entry and the exit", orders)
	}
	if orders[2].Tag != decisionTag(20) {
		t.Errorf("exit planned by %s, want %s", orders[2].Tag, decisionTag(20))
	}
}

// TestSyntheticGoldenBoostGap is a synthetic golden case, hand-computed,
// since QuantConnect's logged runs hold no missing session: SPY's closes rise
// 4 a session from 100, and session 12 (148) is missing. At session 13 (152)
// the exit mean of the last three closes received is (140 + 144 + 152) / 3
// and the trend mean of the last ten is (112 + 116 + ... + 144 + 152) / 10 =
// 130.4. A fill-forward of 144 into session 12, which LEAN would do, would
// give 146.67 and 133.6 instead.
func TestSyntheticGoldenBoostGap(t *testing.T) {
	closes := make([]float64, 15)
	for i := range closes {
		closes[i] = float64(100 + 4*i)
	}
	r := simulate(t, options{closes: closes, skip: map[int]bool{12: true}})
	d := targets(t, r.decisions)[13]
	if math.Abs(d.Boost.ExitMean-436.0/3) > 1e-9 || math.Abs(d.Boost.TrendMean-130.4) > 1e-9 || d.Boost.RSI != 100 {
		t.Errorf("session 13: exit mean %v, trend mean %v, RSI %v; want 145.33, 130.4 and 100", d.Boost.ExitMean, d.Boost.TrendMean, d.Boost.RSI)
	}
	if !slices.Equal(d.Boost.GapSessions, []string{sessionDate(12)}) {
		t.Errorf("gap sessions %v", d.Boost.GapSessions)
	}
}

// TestAGapRunIsByteIdenticalReplaysAndRestarts: the first decided session
// after a gap is the same on a rerun, on replay, and in a run restarted from
// the withheld gap decision, whose quiet replay of the journal's bars skips
// the same session.
func TestAGapRunIsByteIdenticalReplaysAndRestarts(t *testing.T) {
	opts := options{skip: map[int]bool{12: true, 13: true}}
	assertByteIdenticalAndReplays(t, opts)
	full := simulate(t, opts)
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[12]
	r := simulate(t, options{skip: opts.skip, recovery: &recovery, snapshots: full.inputs})
	restarted := targets(t, r.decisions)
	original := targets(t, full.decisions)[13:]
	if len(restarted) != len(original) {
		t.Fatalf("restarted run decides %d sessions, want %d", len(restarted), len(original))
	}
	for i := range original {
		if restarted[i].Outcome != original[i].Outcome || restarted[i].Boost.RSI != original[i].Boost.RSI ||
			!slices.Equal(restarted[i].Boost.GapSessions, original[i].Boost.GapSessions) || restarted[i].Boost.LatchedAfter != original[i].Boost.LatchedAfter {
			t.Errorf("session %d: restarted %+v, original %+v", 13+i, restarted[i].Boost, original[i].Boost)
		}
	}
}
