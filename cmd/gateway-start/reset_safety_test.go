package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Synthetic refusals exercise the persisted reset policy in docs/manual/server.md.
func TestResetSafetyConfiguredBudget(t *testing.T) {
	for _, value := range []string{"4", "6", "9"} {
		c, warnings := configFromEnv(func(k string) string {
			if k == "TI_GATEWAY_LOGIN_MAX_FAILURES" {
				return value
			}
			return ""
		})
		if c.Max != 3 || len(warnings) != 1 {
			t.Errorf("setting %s: Max=%d warnings=%v; want 3 plus warning", value, c.Max, warnings)
		}
	}
}

func TestResetSafetyFifthRefusalIsCountedAcrossStarts(t *testing.T) {
	r := newRig(t)
	c := r.cfg(`echo '` + refusal + `'`)
	waits := 0
	resetClock(r, &c, "2026-11-01T03:40:00Z", func(time.Duration) { waits++ })
	for i, want := range []int{75, 75, 77} {
		code, output := r.run(c)
		if code != want || waits != 4 || r.launches() != 5+i {
			t.Fatalf("start %d: code=%d waits=%d launches=%d; want %d, 4, %d: %s", i, code, waits, r.launches(), want, 5+i, output)
		}
	}
}

func TestResetSafetyRestartResumesWait(t *testing.T) {
	r := newRig(t)
	c := r.cfg(`echo '` + refusal + `'`)
	resetClock(r, &c, "2026-11-01T04:08:00Z", nil)
	c.Accepted = regexp.MustCompile(defaultAccepted)
	var messages bytes.Buffer
	out := &journal{w: writerFunc(func(p []byte) (int, error) {
		raw, _ := os.ReadFile(r.state)
		if !strings.Contains(string(raw), "reset ") || len(r.counter()) != 1 {
			t.Errorf("before forwarding refusal, wait must be durable and attempt retained: %q", raw)
		}
		return len(p), nil
	}), timeout: time.Second}
	if code := runIBC(c, out, &journal{w: &messages, timeout: time.Second}, make(chan os.Signal)); code != resetRetry {
		t.Fatalf("first outcome=%d: %s", code, messages.String())
	}
	// A new supervisor five minutes later must sleep the remaining ten minutes
	// before its first exec, even though the previous supervisor already discounted it.
	c = r.cfg(`echo 'IBC: Login has completed'`)
	rendered := filepath.Join(r.dir, "rendered")
	c.Render = []string{"sh", "-c", "echo rendered > " + rendered}
	waits := 0
	resetClock(r, &c, "2026-11-01T04:13:00Z", func(d time.Duration) {
		waits++
		if _, err := os.Stat(rendered); !os.IsNotExist(err) {
			t.Errorf("renderer ran before resuming wait: %v", err)
		}
		if d != 10*time.Minute || r.launches() != 1 {
			t.Errorf("resumed delay=%s launches=%d; want 10m and only the old launch", d, r.launches())
		}
	})
	if code, output := r.run(c); code != 0 || waits != 1 {
		t.Fatalf("restart code=%d waits=%d: %s", code, waits, output)
	}
}

func TestResetSafetyJournalFailureStillWaits(t *testing.T) {
	for _, which := range []string{"stdout", "stderr"} {
		t.Run(which, func(t *testing.T) {
			r := newRig(t)
			c := r.cfg(`if [ "$(wc -l < ` + filepath.Join(r.dir, "launches") + `)" -eq 1 ]; then echo '` + refusal + `'; fi`)
			waits := 0
			resetClock(r, &c, "2026-11-01T04:58:00Z", func(d time.Duration) {
				waits++
				if d != 2*time.Minute {
					t.Errorf("delay=%s", d)
				}
			})
			var out, errw = io.Discard, io.Discard
			if which == "stdout" {
				out = failingWriter{}
			} else {
				errw = failingWriter{}
			}
			if code := run(c, out, errw); code != 0 || waits != 1 || r.launches() != 2 {
				t.Fatalf("code=%d waits=%d launches=%d; journal failure skipped reset wait", code, waits, r.launches())
			}
		})
	}
}

func TestResetSafetyChattyDescendantCannotOverrideWait(t *testing.T) {
	r := newRig(t)
	c := r.cfg(`if [ "$(wc -l < ` + filepath.Join(r.dir, "launches") + `)" -eq 1 ]; then
 (trap '' TERM; i=0; while [ $i -lt 100 ]; do echo descendant; sleep 0.02; i=$((i+1)); done) &
 echo '` + refusal + `'; wait
 fi`)
	c.DrainDeadline, c.SinkTimeout = 40*time.Millisecond, 40*time.Millisecond
	waits := 0
	resetClock(r, &c, "2026-11-01T04:58:00Z", func(time.Duration) { waits++ })
	if code, output := r.run(c); code != 0 || waits != 1 {
		t.Fatalf("code=%d waits=%d; descendant must not replace reset outcome: %s", code, waits, output)
	}
}

func TestResetSafetyPendingStopPreventsLaunch(t *testing.T) {
	for _, when := range []string{"entry", "before exec", "during refusal", "during reset message"} {
		t.Run(when, func(t *testing.T) {
			r := newRig(t)
			c := r.cfg(`echo '` + refusal + `'`)
			resetClock(r, &c, "2026-11-01T04:59:59Z", nil)
			c.Accepted = regexp.MustCompile(defaultAccepted)
			sigs := make(chan os.Signal, 1)
			if when == "entry" {
				sigs <- syscall.SIGTERM
			}
			var messages bytes.Buffer
			errw := writerFunc(func(p []byte) (int, error) {
				if when == "before exec" && strings.Contains(string(p), "starting IBC") {
					sigs <- syscall.SIGTERM
				}
				if when == "during reset message" && strings.Contains(string(p), "reset wait: login refused") {
					sigs <- syscall.SIGTERM
				}
				return messages.Write(p)
			})
			out := writerFunc(func(p []byte) (int, error) {
				if when == "during refusal" {
					sigs <- syscall.SIGTERM
				}
				return len(p), nil
			})
			code := runIBC(c, &journal{w: out, timeout: time.Second}, &journal{w: errw, timeout: time.Second}, sigs)
			wantLaunches := 0
			if when == "during refusal" || when == "during reset message" {
				wantLaunches = 1
			}
			if code != 143 || r.launches() != wantLaunches {
				t.Fatalf("code=%d launches=%d; want 143, %d: %s", code, r.launches(), wantLaunches, messages.String())
			}
			if when == "during refusal" || when == "during reset message" {
				assertInterruptedResetCounted(t, r)
			}
			if (when == "during refusal" || when == "during reset message") && !strings.Contains(messages.String(), "gateway-start: reset wait: interrupted by stop") {
				t.Errorf("missing interruption prefix: %s", messages.String())
			}
		})
	}
}

func TestResetSafetyDecisionLogsActualDelayBeforeWait(t *testing.T) {
	r := newRig(t)
	c := r.cfg(`if [ "$(wc -l < ` + filepath.Join(r.dir, "launches") + `)" -eq 1 ]; then echo '` + refusal + `'; fi`)
	log := &recordingJournal{}
	want := "gateway-start: reset wait: login refused at 00:58 ET inside IBKR's nightly reset hour; waiting 2m until 01:00 ET (consumed reset allowances 1 of 4 in 24h)"
	resetClock(r, &c, "2026-11-01T04:58:00Z", func(time.Duration) {
		if !strings.Contains(log.text(), want) {
			t.Errorf("decision missing before wait: %s", log.text())
		}
	})
	if code := run(c, io.Discard, log); code != 0 {
		t.Fatalf("code=%d: %s", code, log.text())
	}
}

func TestResetSafetyDSTCutoffUsesFirstOneAM(t *testing.T) {
	for _, tc := range []struct {
		at   string
		want time.Duration
	}{
		{"2026-11-01T03:40:00Z", 15 * time.Minute},
		{"2026-11-01T04:58:00Z", 2 * time.Minute},
		{"2026-11-01T05:00:00Z", 0},
		{"2026-11-01T05:59:59Z", 0},
		{"2026-11-01T06:00:00Z", 0},
		{"2026-11-01T06:59:59Z", 0},
	} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		if got := nightlyResetDelay(at); got != tc.want {
			t.Errorf("%s: delay=%s want=%s", tc.at, got, tc.want)
		}
	}
}

func TestResetSafetyGroupGoneBeforeWait(t *testing.T) {
	// The existing descendant fixture checks liveness; this case also requires
	// an unreaped leader to be gone before the internal retry (docs/development.md).
	r := newRig(t)
	c := r.cfg(`echo '` + refusal + `'; while :; do :; done`)
	c.Accepted = regexp.MustCompile(defaultAccepted)
	resetClock(r, &c, "2026-11-01T04:58:00Z", nil)
	var messages bytes.Buffer
	if code := runIBC(c, &journal{w: io.Discard, timeout: time.Second}, &journal{w: &messages, timeout: time.Second}, make(chan os.Signal)); code != resetRetry {
		t.Fatalf("outcome=%d: %s", code, messages.String())
	}
}

func TestResetSafetyCleanupPastCutoffReportsZeroWait(t *testing.T) {
	r := newRig(t)
	c := r.cfg(`echo '` + refusal + `'`)
	resetClock(r, &c, "2026-11-01T04:59:59Z", nil)
	c.Accepted = regexp.MustCompile(defaultAccepted)
	var mu sync.Mutex
	now := c.Now()
	c.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	var messages bytes.Buffer
	out := writerFunc(func(p []byte) (int, error) { mu.Lock(); now = now.Add(time.Minute); mu.Unlock(); return len(p), nil })
	code := runIBC(c, &journal{w: out, timeout: time.Second}, &journal{w: &messages, timeout: time.Second}, make(chan os.Signal))
	if code != resetRetry || !strings.Contains(messages.String(), "waiting 0s until 01:00 ET (consumed reset allowances 1 of 4 in 24h)") {
		t.Fatalf("outcome=%d, missing actual zero wait at cutoff: %s", code, messages.String())
	}
}

func TestResetSafetyStopWinsWhenTimerIsReady(t *testing.T) {
	r := newRig(t)
	now, _ := time.Parse(time.RFC3339, "2026-11-01T04:58:00Z")
	w := resetWait{now, now.Add(2 * time.Minute), []time.Time{now}}
	if err := os.WriteFile(r.state, w.withAttempts(nil), 0o600); err != nil {
		t.Fatal(err)
	}
	c := r.cfg(`exit 0`)
	c.Now = func() time.Time { return now }
	sigs := make(chan os.Signal, 1)
	c.After = func(time.Duration) <-chan time.Time {
		sigs <- syscall.SIGTERM
		timer := make(chan time.Time, 1)
		timer <- w.until
		return timer
	}
	var messages bytes.Buffer
	if code := awaitReset(c, &journal{w: &messages, timeout: time.Second}, sigs); code != 143 || !strings.Contains(messages.String(), "gateway-start: reset wait: interrupted by stop") {
		t.Fatalf("code=%d: %s", code, messages.String())
	}
}

func TestResetSafetyMalformedWaitFailsClosed(t *testing.T) {
	for _, raw := range []string{
		"reset 1793509080\n", "reset invalid 1793509200 1\n",
		"reset 1793509080 1793509200 0\n", "reset 1793509080 1793509200 5\n",
		"reset 1793509080 1793509200 1\n", "reset 1793509080 1793509200 2 1793509080\n",
		"reset 1793509080 1793509200 1\nreset 1 2 3\n",
	} {
		t.Run(raw, func(t *testing.T) {
			r := newRig(t)
			if err := os.WriteFile(r.state, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if code, output := r.run(r.cfg(`exit 0`)); code != exitStopped || r.launches() != 0 {
				t.Fatalf("malformed wait allowed startup: code=%d launches=%d %s", code, r.launches(), output)
			}
		})
	}
}

func TestResetSafetyAcceptedLoginKeepsRollingAllowance(t *testing.T) {
	r := newRig(t)
	at, _ := time.Parse(time.RFC3339, "2026-11-01T04:40:00Z")
	w := resetWait{at.Add(-15 * time.Minute), at, []time.Time{at.Add(-60 * time.Minute), at.Add(-45 * time.Minute), at.Add(-30 * time.Minute), at.Add(-15 * time.Minute)}}
	if err := os.WriteFile(r.state, w.withAttempts(nil), 0o600); err != nil {
		t.Fatal(err)
	}
	c := r.cfg(`echo 'IBC: Login has completed'; echo '` + refusal + `'`)
	resetClock(r, &c, at.Format(time.RFC3339), func(time.Duration) { t.Error("accepted login replenished reset allowance") })
	if code, output := r.run(c); code != exitRetry || len(r.counter()) != 1 {
		t.Fatalf("fifth reset refusal after acceptance: code=%d count=%v: %s", code, r.counter(), output)
	}
}

// Synthetic nights exercise the rolling budget in docs/manual/server.md,
// including an idle supervisor over the spring clock change.
func TestResetAllowanceSurvivesIdleAcrossNights(t *testing.T) {
	for _, start := range []string{"2027-03-14T05:00:00Z", "2026-10-30T04:00:00Z", "2026-11-01T04:00:00Z"} {
		t.Run(start, func(t *testing.T) {
			r := newRig(t)
			first, _ := time.Parse(time.RFC3339, start)
			c := r.cfg(`echo '` + refusal + `'`)
			c.Accepted = regexp.MustCompile(defaultAccepted)
			var messages bytes.Buffer
			for i := 0; i < 4; i++ {
				at := first.Add(time.Duration(i) * 15 * time.Minute)
				c.Now = func() time.Time { return at }
				if code := runIBC(c, &journal{w: io.Discard, timeout: time.Second}, &journal{w: &messages, timeout: time.Second}, make(chan os.Signal)); code != resetRetry {
					t.Fatalf("first night refusal %d: %d: %s", i, code, messages.String())
				}
			}
			// Spring and ordinary nights retain all four; after the autumn change
			// all four are older than 24h and may expire.
			next := first.Add(23*time.Hour + 40*time.Minute)
			if start == "2026-11-01T04:00:00Z" {
				next = first.Add(25*time.Hour + 40*time.Minute)
			}
			c.Now = func() time.Time { return next }
			if start == "2026-11-01T04:00:00Z" {
				// 00:40 EST on the following night, after the 25-hour day.
				if code := runIBC(c, &journal{w: io.Discard, timeout: time.Second}, &journal{w: &messages, timeout: time.Second}, make(chan os.Signal)); code != resetRetry {
					t.Fatalf("expired allowances did not renew: %d: %s", code, messages.String())
				}
				return
			}
			// On spring's shortened day, use 23:40 EDT rather than 00:40 EDT.
			if start == "2027-03-14T05:00:00Z" {
				next = first.Add(22*time.Hour + 40*time.Minute)
			}
			for i, want := range []int{exitRetry, exitRetry, exitStopped} {
				if code := runIBC(c, &journal{w: io.Discard, timeout: time.Second}, &journal{w: &messages, timeout: time.Second}, make(chan os.Signal)); code != want {
					t.Fatalf("idle restart %d: code=%d want=%d; previous night's allowances must remain spent: %s", i, code, want, messages.String())
				}
				next = next.Add(5 * time.Minute)
			}
			if r.launches() != 7 || len(r.counter()) != 3 {
				t.Fatalf("launches=%d counted=%v; want seven and three", r.launches(), r.counter())
			}
		})
	}
}

func TestResetInconsistentWaitStopsWithoutLaunching(t *testing.T) {
	// A persisted wait whose refusal or deadline is not what the rule would have
	// written is untrusted state: nothing launches on it (AGENTS.md: fail closed).
	// A refusal ahead of the clock is TestResetWaitSurvivesABackwardClockStep.
	now, _ := time.Parse(time.RFC3339, "2026-11-01T04:58:00Z")
	for name, pair := range map[string][2]time.Time{
		"outside night":          {now, now.Add(24 * time.Hour)},
		"wrong cooldown":         {now, now.Add(time.Second)},
		"refusal outside window": {now.Add(-12 * time.Hour), now.Add(-12*time.Hour + 15*time.Minute)},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			raw := fmt.Sprintf("reset %d %d 1 %d\n", pair[0].Unix(), pair[1].Unix(), pair[0].Unix())
			if err := os.WriteFile(r.state, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			c := r.cfg(`exit 0`)
			c.Now = func() time.Time { return now }
			c.After = func(d time.Duration) <-chan time.Time {
				t.Errorf("inconsistent wait slept for %s", d)
				ch := make(chan time.Time, 1)
				ch <- now
				return ch
			}
			code, output := r.run(c)
			if code != exitStopped || r.launches() != 0 || !strings.Contains(output, "inconsistent") {
				t.Fatalf("code=%d launches=%d: %s", code, r.launches(), output)
			}
			if raw2, _ := os.ReadFile(r.state); string(raw2) != raw {
				t.Fatalf("the untrusted state was changed: %q", raw2)
			}
		})
	}
}

func TestPermanentOutputStopKeepsResetAttemptCounted(t *testing.T) {
	r := newRig(t)
	// One oversized synthetic line contains a refusal in the same read buffer.
	c := r.cfg(`printf '` + refusal + `'; head -c 65536 /dev/zero | tr '\000' x; printf '\n'`)
	c.Accepted = regexp.MustCompile(defaultAccepted)
	resetClock(r, &c, "2026-11-01T04:58:00Z", nil)
	var messages bytes.Buffer
	code := runIBC(c, &journal{w: io.Discard, timeout: time.Second}, &journal{w: &messages, timeout: time.Second}, make(chan os.Signal))
	if code != exitStopped || len(r.counter()) != 1 || !strings.Contains(messages.String(), "longer than 64 KiB") {
		t.Fatalf("code=%d count=%v; permanent stop must survive reset: %s", code, r.counter(), messages.String())
	}
}

func TestResetMessageUsesWholeUnitsAndConsumedAllowances(t *testing.T) {
	at, _ := time.Parse(time.RFC3339, "2026-11-01T04:08:00Z")
	w := resetWait{at, at.Add(15 * time.Minute), []time.Time{at}}
	for _, tc := range []struct {
		elapsed time.Duration
		want    string
	}{
		{1230 * time.Millisecond, "waiting 15m"},
		{14*time.Minute + 1230*time.Millisecond, "waiting 59s"},
		{15 * time.Minute, "waiting 0s"},
	} {
		got := w.message(at.Add(tc.elapsed))
		if !strings.Contains(got, tc.want) || !strings.Contains(got, "consumed reset allowances 1 of 4 in 24h") || strings.Contains(got, "uncounted") {
			t.Errorf("want %q and consumed allowances, got %s", tc.want, got)
		}
	}
}

func assertInterruptedResetCounted(t *testing.T, r *rig) {
	t.Helper()
	raw, err := os.ReadFile(r.state)
	if err != nil || len(r.counter()) != 1 || !strings.HasPrefix(string(raw), "reset ") {
		t.Fatalf("interrupted reset lost provisional count or header: %q, %v", raw, err)
	}
}

// Delay delivery to the final handoff: a reset verdict must not escape through
// the generic return while its cleanup decision is still pending
// (docs/development.md, "Ending processes").
func TestFinalResetVerdictWaitsForCleanupDecision(t *testing.T) {
	decisions := make(chan resetDecision)
	done := make(chan *resetDecision, 1)
	go func() { done <- receiveReset(&verdict{code: resetRetry}, decisions) }()
	select {
	case got := <-done:
		t.Fatalf("reset verdict returned before its cleanup decision: %v", got)
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case decisions <- resetDecision{discounted: []byte("settled")}:
	case <-time.After(time.Second):
		t.Fatal("reset decision has no receiver")
	}
	select {
	case got := <-done:
		if got == nil || string(got.discounted) != "settled" {
			t.Fatalf("lost cleanup decision: %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("reset decision was not delivered")
	}
}

func TestResetHistoryMustBeConsistentWithTheRecordedRefusal(t *testing.T) {
	// A header whose history does not account for its refusal could replenish the
	// allowance; it fails closed instead (AGENTS.md: fail closed).
	// The refusal (00:58 ET on the DST night) and its 2-minute-to-cutoff deadline
	// are a consistent pair, so only the history can be what fails each case.
	const pair = "reset 1793509080 1793509200 "
	if w, _, err := splitReset([]byte(pair + "1 1793509080\n")); err != nil || len(w.refusals) != 1 {
		t.Fatalf("the consistent pair itself must parse: %v", err)
	}
	for _, raw := range []string{
		pair + "4 0 0 0 0\n",
		pair + "2 1793508000 1793507000\n",
		pair + "1 1793509999\n",
		pair + "2 1793508000 1793508500\n",
		pair + "4 1 2 3 1793509080\n",
	} {
		t.Run(raw, func(t *testing.T) {
			r := newRig(t)
			if err := os.WriteFile(r.state, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			// The clock sits just after the refusal, inside its wait, so neither the
			// skew check nor an expired deadline can be what stops the start.
			c := r.cfg(`exit 0`)
			resetClock(r, &c, "2026-11-01T04:59:00Z", func(d time.Duration) { t.Errorf("an inconsistent history was waited on for %s", d) })
			code, output := r.run(c)
			if code != exitStopped || r.launches() != 0 || !strings.Contains(output, "reset history") {
				t.Fatalf("inconsistent history allowed startup: code=%d launches=%d %s", code, r.launches(), output)
			}
		})
	}
}

func TestResetWaitSurvivesABackwardClockStep(t *testing.T) {
	// A refusal recorded at 00:08 ET with its deadline at 00:23; the clock steps back.
	at := time.Date(2026, time.November, 1, 4, 8, 0, 0, time.UTC)
	w := resetWait{at, at.Add(15 * time.Minute), []time.Time{at}}
	t.Run("within skew the wait is kept to its deadline", func(t *testing.T) {
		r := newRig(t)
		if err := os.WriteFile(r.state, w.withAttempts(nil), 0o600); err != nil {
			t.Fatal(err)
		}
		c := r.cfg(`echo 'IBC: Login has completed'`)
		var delays []time.Duration
		resetClock(r, &c, "2026-11-01T04:07:00Z", func(d time.Duration) { delays = append(delays, d) })
		if code, output := r.run(c); code != 0 || len(delays) != 1 || delays[0] != 16*time.Minute {
			t.Fatalf("code=%d delays=%v: %s", code, delays, output)
		}
	})
	t.Run("beyond skew the start stops rather than launch", func(t *testing.T) {
		r := newRig(t)
		if err := os.WriteFile(r.state, w.withAttempts(nil), 0o600); err != nil {
			t.Fatal(err)
		}
		c := r.cfg(`echo 'IBC: Login has completed'`)
		resetClock(r, &c, "2026-11-01T04:00:00Z", nil)
		if code, output := r.run(c); code != exitStopped || r.launches() != 0 || !strings.Contains(output, "ahead of the clock") {
			t.Fatalf("code=%d launches=%d: %s", code, r.launches(), output)
		}
	})
}
