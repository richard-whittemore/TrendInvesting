package main

import (
	"bytes"
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
	for _, when := range []string{"entry", "before exec", "during refusal"} {
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
			if when == "during refusal" {
				wantLaunches = 1
			}
			if code != 143 || r.launches() != wantLaunches {
				t.Fatalf("code=%d launches=%d; want 143, %d: %s", code, r.launches(), wantLaunches, messages.String())
			}
			if when == "during refusal" && !strings.Contains(messages.String(), "gateway-start: reset wait: interrupted by stop") {
				t.Errorf("missing interruption prefix: %s", messages.String())
			}
		})
	}
}

func TestResetSafetyDecisionLogsActualDelayBeforeWait(t *testing.T) {
	r := newRig(t)
	c := r.cfg(`if [ "$(wc -l < ` + filepath.Join(r.dir, "launches") + `)" -eq 1 ]; then echo '` + refusal + `'; fi`)
	log := &recordingJournal{}
	want := "gateway-start: reset wait: login refused at 00:58 ET inside IBKR's nightly reset hour; waiting 2m until 01:00 ET (uncounted 1 of 4 tonight)"
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
	if code != resetRetry || !strings.Contains(messages.String(), "waiting 0m until 01:00 ET (uncounted 1 of 4 tonight)") {
		t.Fatalf("outcome=%d, missing actual zero wait at cutoff: %s", code, messages.String())
	}
}

func TestResetSafetyStopWinsWhenTimerIsReady(t *testing.T) {
	r := newRig(t)
	now, _ := time.Parse(time.RFC3339, "2026-11-01T04:58:00Z")
	w := resetWait{now, now.Add(2 * time.Minute), 1}
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
		"reset 1793509080 1793509081 1\n", "reset 1793509080 1793509200 1\nreset 1 2 3\n",
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

func TestResetSafetyAcceptedLoginKeepsNightlyAllowance(t *testing.T) {
	r := newRig(t)
	at, _ := time.Parse(time.RFC3339, "2026-11-01T04:40:00Z")
	w := resetWait{at.Add(-15 * time.Minute), at, 4}
	if err := os.WriteFile(r.state, w.withAttempts(nil), 0o600); err != nil {
		t.Fatal(err)
	}
	c := r.cfg(`echo 'IBC: Login has completed'; echo '` + refusal + `'`)
	resetClock(r, &c, at.Format(time.RFC3339), func(time.Duration) { t.Error("accepted login replenished reset allowance") })
	if code, output := r.run(c); code != exitRetry || len(r.counter()) != 1 {
		t.Fatalf("fifth reset refusal after acceptance: code=%d count=%v: %s", code, r.counter(), output)
	}
}
