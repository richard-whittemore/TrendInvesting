package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The clock-check seam: the reporter runs the check command on every firing
// and pages once per failure (ADR 0019, "Alerting": the clock drifting behind
// IBKR's withholds every sell).

const clockFailure = "clock: skew of -7s against https://www.cloudflare.com (this clock minus the reference), more than the 2s tolerance."

// standInCheck is a clock-check command whose answer the test sets: it prints
// the message and exits 1 when the failure is set, 0 otherwise.
type standInCheck struct{ dir string }

func newStandInCheck(t *testing.T) standInCheck {
	t.Helper()
	c := standInCheck{dir: t.TempDir()}
	script := "#!/bin/sh\ncat '" + c.dir + "/message'\nif test -e '" + c.dir + "/fail'; then exit 1; fi\n"
	if err := os.WriteFile(filepath.Join(c.dir, "check.sh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	c.pass()
	return c
}

func (c standInCheck) path() string { return filepath.Join(c.dir, "check.sh") }

func (c standInCheck) pass() {
	_ = os.Remove(filepath.Join(c.dir, "fail"))
	_ = os.WriteFile(filepath.Join(c.dir, "message"), []byte("clock: ok, skew 1s against https://www.apple.com, within 2s\n"), 0o600)
}

func (c standInCheck) fail(message string) {
	_ = os.WriteFile(filepath.Join(c.dir, "fail"), nil, 0o600)
	_ = os.WriteFile(filepath.Join(c.dir, "message"), []byte(message+"\n"), 0o600)
}

func clockRun(t *testing.T, r *relay, dir, check string, at time.Time, extra ...string) error {
	t.Helper()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty", "-repeat-interval", "30m"}
	if check != "" {
		args = append(args, "-clock-check", check)
	}
	args = append(args, extra...)
	return run(args, twoChannels(), func() time.Time { return at }, &bytes.Buffer{}, &http.Client{Transport: r})
}

func clockMessages(r *relay) []string {
	var out []string
	for _, host := range []string{"ntfy.private.test", "events.pagerduty.test"} {
		for _, m := range r.to(host) {
			if strings.Contains(strings.ToLower(m), "clock") && strings.Contains(m, "-7s") {
				out = append(out, m)
			}
		}
	}
	return out
}

func TestAFailingClockCheckPagesOnBothChannelsWithTheSkewAndTheRemedy(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	check := newStandInCheck(t)
	check.fail(clockFailure)
	dir := t.TempDir()
	if err := clockRun(t, r, dir, check.path(), t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"ntfy.private.test", "events.pagerduty.test"} {
		n := 0
		for _, m := range r.to(host) {
			if strings.Contains(m, "-7s") && strings.Contains(m, "automatic date and time") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s took %d clock alerts, want 1 with the skew and the remedy: %v", host, n, r.to(host))
		}
	}
}

func TestAClockFailureIsToldOncePerFailureNotEveryFiveMinutesOrEveryRepeatInterval(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	check := newStandInCheck(t)
	check.fail(clockFailure)
	dir := t.TempDir()
	// Every five minutes for two hours, well past the 30-minute repeat
	// interval a Halted alert would repeat at.
	for i := 0; i < 24; i++ {
		if err := clockRun(t, r, dir, check.path(), t0.Add(time.Hour+time.Duration(i)*5*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(clockMessages(r)); got != 2 {
		t.Fatalf("%d clock messages, want 1 per channel: %v", got, clockMessages(r))
	}
}

func TestAClockThatRecoversAndDriftsAgainIsANewFailure(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	check := newStandInCheck(t)
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	steps := []struct {
		fails bool
		total int
	}{{false, 0}, {true, 2}, {true, 2}, {false, 2}, {true, 4}}
	for i, s := range steps {
		if s.fails {
			check.fail(clockFailure)
		} else {
			check.pass()
		}
		if err := clockRun(t, r, dir, check.path(), at.Add(time.Duration(i)*5*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if got := len(clockMessages(r)); got != s.total {
			t.Fatalf("step %d: %d clock messages, want %d", i, got, s.total)
		}
	}
}

func TestAChannelNamedLaterTakesTheClockAlertOnceAndTheFirstIsNotToldAgain(t *testing.T) {
	check := newStandInCheck(t)
	check.fail(clockFailure)
	dir := t.TempDir()
	down := &relay{sent: map[string][]string{}}
	args := func(r *relay, channel string) error {
		a := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", channel, "-clock-check", check.path()}
		return run(a, twoChannels(), func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, &http.Client{Transport: r})
	}
	if err := args(down, "ntfy"); err != nil {
		t.Fatal(err)
	}
	if err := args(down, "ntfy,pagerduty"); err != nil {
		t.Fatal(err)
	}
	if len(clockMessages(down)) != 2 || len(down.to("ntfy.private.test")) == 0 {
		t.Fatalf("messages: %v", clockMessages(down))
	}
}

func TestACheckThatCannotRunIsAFailureNotASilence(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	if err := clockRun(t, r, dir, filepath.Join(dir, "missing.sh"), t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got := r.to("ntfy.private.test")
	n := 0
	for _, m := range got {
		if strings.Contains(m, "No skew was measured") && strings.Contains(m, "could not be run") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want one alert saying the check could not run: %v", got)
	}
}

func TestAPassingClockCheckPagesNothing(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	check := newStandInCheck(t)
	if err := clockRun(t, r, t.TempDir(), check.path(), t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, m := range append(r.to("ntfy.private.test"), r.to("events.pagerduty.test")...) {
		if strings.Contains(strings.ToLower(m), "clock") {
			t.Errorf("a passing check sent %q", m)
		}
	}
}

func TestWithoutAClockCheckTheReporterBehavesAsBefore(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	if err := clockRun(t, r, t.TempDir(), "", t0.Add(time.Hour), "-summary"); err != nil {
		t.Fatal(err)
	}
	for _, m := range append(r.to("events.pagerduty.test"), r.to("ntfy.private.test")...) {
		if strings.Contains(m, "Clock check") {
			t.Errorf("no check was named, yet: %q", m)
		}
	}
}

func TestTheDailySummaryShowsTheLastClockCheckTimeAndResult(t *testing.T) {
	check := newStandInCheck(t)
	for name, c := range map[string]struct {
		message string
		fails   bool
		want    []string
	}{
		"ok":     {"clock: ok, skew 1s against https://www.apple.com, within 2s", false, []string{"Clock check: ok", "skew 1s"}},
		"failed": {clockFailure, true, []string{"Clock check: FAILED", "-7s"}},
	} {
		t.Run(name, func(t *testing.T) {
			if c.fails {
				check.fail(c.message)
			} else {
				check.pass()
			}
			r := &relay{sent: map[string][]string{}}
			at := t0.Add(time.Hour)
			if err := clockRun(t, r, t.TempDir(), check.path(), at, "-summary"); err != nil {
				t.Fatal(err)
			}
			var summary string
			for _, m := range r.to("events.pagerduty.test") {
				if strings.Contains(m, "daily summary") && strings.Contains(m, "Clock check") {
					summary = m
				}
			}
			if summary == "" {
				t.Fatalf("no summary with a clock line: %v", r.to("events.pagerduty.test"))
			}
			for _, w := range append(c.want, at.UTC().Format("2006-01-02 15:04Z")) {
				if !strings.Contains(summary, w) {
					t.Errorf("summary lacks %q:\n%s", w, summary)
				}
			}
		})
	}
}
