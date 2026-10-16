package boost_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/boost"
)

// The Boost golden cases are QuantConnect logs of research/edge-search/boost/
// golden_log.py (docs/research/golden-cases-2026-10.md): one line per logged
// session, "BGOLD <date> close=... rsi2=... sma200=... sma5=... sig=0|1
// want=...|", six decimals, "-" for an indicator not ready. They hold market
// data, so they live outside the repository and these tests skip without them.
//
// The logs hold the first decision and whole months, not every close from the
// warm-up start, so the indicators cannot be rebuilt from 1998 here; that
// comparison is the parity slice's. What the logs do pin: the rule's state and
// target on every logged session given its logged readings, the 5-day mean
// from the logged closes, and the Wilder RSI(2) recursion.

type goldenDay struct {
	date    string
	r       boost.Readings
	latched bool
	want    float64
}

func goldenDays(t *testing.T, run string) []goldenDay {
	t.Helper()
	dir := os.Getenv("TI_GOLDEN_DIR")
	if dir == "" {
		t.Skip("TI_GOLDEN_DIR is not set; the QuantConnect golden logs are kept outside the repository (docs/research/golden-cases-2026-10.md)")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "golden_"+run+".json"))
	if os.IsNotExist(err) {
		t.Skipf("golden_%s.json is absent from TI_GOLDEN_DIR", run)
	}
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	if err := json.Unmarshal(raw, &lines); err != nil {
		t.Fatal(err)
	}
	var days []goldenDay
	for _, line := range lines {
		_, body, ok := strings.Cut(line, "BGOLD ")
		if !ok {
			continue
		}
		if !strings.HasSuffix(body, "|") {
			t.Fatalf("line cut short: %q", line)
		}
		fields := strings.Fields(strings.TrimSuffix(body, "|"))
		if len(fields) != 7 {
			t.Fatalf("unexpected line %q", line)
		}
		value := func(i int, key string) (float64, bool) {
			text, found := strings.CutPrefix(fields[i], key+"=")
			if !found {
				t.Fatalf("field %d of %q is not %s", i, line, key)
			}
			if text == "-" {
				return 0, false
			}
			v, err := strconv.ParseFloat(text, 64)
			if err != nil {
				t.Fatal(err)
			}
			return v, true
		}
		d := goldenDay{date: fields[0]}
		d.r.Close, _ = value(1, "close")
		d.r.RSI, d.r.RSIReady = value(2, "rsi2")
		d.r.TrendMean, d.r.TrendMeanReady = value(3, "sma200")
		d.r.ExitMean, d.r.ExitMeanReady = value(4, "sma5")
		sig, _ := value(5, "sig")
		d.latched = sig == 1
		d.want, _ = value(6, "want")
		days = append(days, d)
	}
	if len(days) < 2 {
		t.Fatalf("golden_%s.json holds %d BGOLD lines", run, len(days))
	}
	return days
}

// months splits the logged sessions after the first decision into runs of
// consecutive sessions, one per logged month.
func months(days []goldenDay) [][]goldenDay {
	var out [][]goldenDay
	for _, d := range days[1:] {
		if n := len(out); n > 0 && out[n-1][0].date[:7] == d.date[:7] {
			out[n-1] = append(out[n-1], d)
			continue
		}
		out = append(out, []goldenDay{d})
	}
	return out
}

func TestGoldenBoost(t *testing.T) {
	for _, run := range []string{"BX_GOLD", "BX_GOLD_2"} {
		t.Run(run, func(t *testing.T) {
			days := goldenDays(t, run)
			p := params()

			first := days[0]
			if first.r.RSIReady || first.r.TrendMeanReady || first.r.ExitMeanReady || first.latched {
				t.Fatalf("first decision %s: %+v", first.date, first)
			}
			if p.Next(false, first.r) || p.Target(false) != first.want {
				t.Errorf("first decision %s: want 98%% with no boost, logged %v", first.date, first.want)
			}

			var transitions, sma5Checked, rsiChecked int
			for _, month := range months(days) {
				for i, d := range month {
					if math.Abs(p.Target(d.latched)-d.want) > 1e-6 {
						t.Errorf("%s: target for sig=%v is %v, logged %v", d.date, d.latched, p.Target(d.latched), d.want)
					}
					if i == 0 {
						continue
					}
					if got := p.Next(month[i-1].latched, d.r); got != d.latched {
						t.Errorf("%s: rule gives latched=%v after %v, logged %v (%+v)", d.date, got, month[i-1].latched, d.latched, d.r)
					}
					transitions++
					if i >= 4 {
						var sum float64
						for _, prior := range month[i-4 : i+1] {
							sum += prior.r.Close
						}
						if math.Abs(sum/5-d.r.ExitMean) > 1e-5 {
							t.Errorf("%s: 5-day mean of logged closes %v, logged %v", d.date, sum/5, d.r.ExitMean)
						}
						sma5Checked++
					}
					if i >= 2 && checkRSIStep(t, month[i-2], month[i-1], d) {
						rsiChecked++
					}
				}
			}
			t.Logf("%s: %d transitions, %d five-day means, %d RSI steps checked", run, transitions, sma5Checked, rsiChecked)
			if rsiChecked == 0 {
				t.Error("no RSI step was well conditioned enough to check")
			}
		})
	}
}

// checkRSIStep recovers the Wilder gain and loss averages behind b's RSI from
// a's and b's logged RSIs and the change between their closes, steps them by
// c's change with k = 1/2, and compares the result with c's logged RSI. It
// skips a step whose recovery is ill conditioned and reports whether it
// checked one.
func checkRSIStep(t *testing.T, a, b, c goldenDay) bool {
	t.Helper()
	if a.r.RSI >= 99.99 || b.r.RSI >= 99.99 {
		return false
	}
	rsA := a.r.RSI / (100 - a.r.RSI)
	rsB := b.r.RSI / (100 - b.r.RSI)
	if math.Abs(rsB-rsA) < 0.05 {
		return false
	}
	gain, loss := moves(b.r.Close - a.r.Close)
	lossA := (gain - float64(rsB*loss)) / (rsB - rsA)
	if lossA <= 0 {
		return false
	}
	gainA := rsA * lossA
	gainB := float64(gainA*0.5) + float64(gain*0.5)
	lossB := float64(lossA*0.5) + float64(loss*0.5)
	gain, loss = moves(c.r.Close - b.r.Close)
	gainC := float64(gainB*0.5) + float64(gain*0.5)
	lossC := float64(lossB*0.5) + float64(loss*0.5)
	if lossC <= 1e-9 {
		return false
	}
	got := 100 - 100/(1+gainC/lossC)
	if math.Abs(got-c.r.RSI) > 0.05 {
		t.Errorf("%s: Wilder RSI(2) stepped from %s and %s gives %v, logged %v", c.date, a.date, b.date, got, c.r.RSI)
	}
	return true
}

func moves(change float64) (gain, loss float64) {
	if change >= 0 {
		return change, 0
	}
	return 0, -change
}

// TestGoldenBoostExitThenNextDayReentry pins the one exit followed by a
// re-entry the next session in the logs (BX_GOLD_2, 2024-07-23 and 07-24).
func TestGoldenBoostExitThenNextDayReentry(t *testing.T) {
	days := goldenDays(t, "BX_GOLD_2")
	byDate := make(map[string]int)
	for i, d := range days {
		byDate[d.date] = i
	}
	p := params()
	for _, step := range []struct {
		date string
		want bool
	}{{"2024-07-23", false}, {"2024-07-24", true}} {
		i, ok := byDate[step.date]
		if !ok || i == 0 {
			t.Fatalf("%s is not logged after another session", step.date)
		}
		if days[i-1].latched == step.want || days[i].latched != step.want {
			t.Errorf("%s: logged %v then %v, want a change to %v", step.date, days[i-1].latched, days[i].latched, step.want)
		}
		if got := p.Next(days[i-1].latched, days[i].r); got != step.want {
			t.Errorf("%s: rule gives latched %v, want %v", step.date, got, step.want)
		}
	}
}
