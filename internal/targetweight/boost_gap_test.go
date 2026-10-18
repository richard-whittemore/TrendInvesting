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
// since QuantConnect's logged runs hold no missing session. It is built so
// that skipping the missing session and LEAN's fill-forward of it give a
// different RSI(2) and different means. SPY's closes rise 4 a session from
// 100 to 140 (sessions 0 to 10), fall to 136 (session 11), session 12 is
// missing, then 139 (13) and 143 (14). The test configuration's RSI period
// is 2, its trend mean 10 sessions and its exit mean 3.
//
// RSI(2), Wilder's averages with k = 1/2: every rise of 4 holds the gain
// average at 4 and the loss average at 0. Session 11's fall of 4 gives gain
// 0/2 + 4/2 = 2 and loss 4/2 + 0/2 = 2.
//
//   - Skipped, session 13's change is 139 - 136 = +3: gain 3/2 + 2/2 = 2.5,
//     loss 0/2 + 2/2 = 1, RSI = 100 - 100 / (1 + 2.5) = 500/7 = 71.428571...
//     Exit mean (140 + 136 + 139) / 3 = 415/3 = 138.333...; trend mean of the
//     last ten closes received, 112, 116, ..., 140, 136, 139, is 1283 / 10 =
//     128.3.
//   - Filled forward (136 again at session 12): session 12's change of 0
//     gives gain 1 and loss 1, then +3 gives gain 3/2 + 1/2 = 2 and loss 1/2,
//     RSI = 100 - 100 / (1 + 4) = 80. Exit mean (136 + 136 + 139) / 3 = 137;
//     trend mean (116 + ... + 140 + 136 + 136 + 139) / 10 = 1307 / 10 = 130.7.
func TestSyntheticGoldenBoostGap(t *testing.T) {
	closes := make([]float64, 0, 15)
	for i := range 11 {
		closes = append(closes, float64(100+4*i))
	}
	closes = append(closes, 136, 136, 139, 143) // session 12's 136 is never fed
	r := simulate(t, options{closes: closes, skip: map[int]bool{12: true}})
	d := targets(t, r.decisions)[13]
	const skipRSI, skipExit, skipTrend = 500.0 / 7, 415.0 / 3, 128.3
	if math.Abs(d.Boost.RSI-skipRSI) > 1e-9 || math.Abs(d.Boost.ExitMean-skipExit) > 1e-9 || math.Abs(d.Boost.TrendMean-skipTrend) > 1e-9 {
		t.Errorf("session 13: RSI %v, exit mean %v, trend mean %v; want %v, %v and %v", d.Boost.RSI, d.Boost.ExitMean, d.Boost.TrendMean, skipRSI, skipExit, skipTrend)
	}
	if !slices.Equal(d.Boost.GapSessions, []string{sessionDate(12)}) {
		t.Errorf("gap sessions %v", d.Boost.GapSessions)
	}
	// The fill-forward reading, computed by hand above, is what the same
	// indicators give with 136 fed at session 12, and differs in all three.
	filled := expectedReadings(t, closes, nil)[13]
	const fillRSI, fillExit, fillTrend = 80.0, 137.0, 130.7
	if math.Abs(filled.RSI-fillRSI) > 1e-9 || math.Abs(filled.ExitMean-fillExit) > 1e-9 || math.Abs(filled.TrendMean-fillTrend) > 1e-9 {
		t.Fatalf("fill-forward readings %+v, want RSI %v, exit mean %v, trend mean %v", filled, fillRSI, fillExit, fillTrend)
	}
	if math.Abs(skipRSI-fillRSI) < 1 || math.Abs(skipExit-fillExit) < 1 || math.Abs(skipTrend-fillTrend) < 1 {
		t.Fatal("the case does not tell skipping from filling forward")
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
