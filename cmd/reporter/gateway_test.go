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

// The Gateway health check: the reporter runs the check command
// (deploy/libexec/gateway-check.sh: exit 0 logged in, 1 logged out) and pages
// once when the Gateway has been logged out past the threshold outside its
// documented restart windows (ADR 0019, "Alerting": nothing can reach IBKR
// while it is logged out, and no unit has failed). One instance owns the check.

var newYork = func() *time.Location {
	l, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	return l
}()

// Tuesday 2024-01-02 in New York, outside every restart window.
func tuesday(h, m int) time.Time { return time.Date(2024, time.January, 2, h, m, 0, 0, newYork) }

const gatewayOut = "gateway: logged out: IBC saw the dialog 'Unrecognized Username or Password' and no 'Login has completed' since"

// standInGateway is a gateway-check command whose answer the test sets.
type standInGateway struct{ dir string }

func newStandInGateway(t *testing.T) standInGateway {
	t.Helper()
	g := standInGateway{dir: t.TempDir()}
	script := "#!/bin/sh\ncat '" + g.dir + "/message'\nif test -e '" + g.dir + "/out'; then exit 1; fi\nif test -e '" + g.dir + "/broken'; then exit 3; fi\nif test -e '" + g.dir + "/silent'; then exit 4; fi\n"
	if err := os.WriteFile(filepath.Join(g.dir, "check.sh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	g.login()
	return g
}

func (g standInGateway) path() string { return filepath.Join(g.dir, "check.sh") }
func (g standInGateway) login() {
	_ = os.Remove(filepath.Join(g.dir, "out"))
	_ = os.WriteFile(filepath.Join(g.dir, "message"), []byte("gateway: logged in\n"), 0o600)
}
func (g standInGateway) logout() {
	_ = os.WriteFile(filepath.Join(g.dir, "out"), nil, 0o600)
	_ = os.WriteFile(filepath.Join(g.dir, "message"), []byte(gatewayOut+"\n"), 0o600)
}

func gatewayRun(t *testing.T, r http.RoundTripper, dir, check, account string, at time.Time, extra ...string) error {
	t.Helper()
	return gatewayRunEnv(t, r, dir, check, account, at, nil, extra...)
}

// gatewayRunEnv is gatewayRun with more of the unit's environment (the
// Gateway's configured restart times, from /etc/trend-investing/gateway.env).
func gatewayRunEnv(t *testing.T, r http.RoundTripper, dir, check, account string, at time.Time, more map[string]string, extra ...string) error {
	t.Helper()
	environment := map[string]string{
		"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": "https://ntfy.private.test",
		"TI_PAGERDUTY_ROUTING_KEY": "k", "TI_PAGERDUTY_URL": "https://events.pagerduty.test/v2/enqueue",
	}
	for k, v := range more {
		environment[k] = v
	}
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty", "-repeat-interval", "30m",
		"-account", account, "-gateway-check", check, "-gateway-owner", "boost"}
	args = append(args, extra...)
	return run(args, env(environment), func() time.Time { return at }, &bytes.Buffer{}, &http.Client{Transport: r})
}

func gatewayPages(r *relay) int {
	n := 0
	for _, host := range []string{"ntfy.private.test", "events.pagerduty.test"} {
		for _, m := range r.to(host) {
			if strings.Contains(m, "IB Gateway is logged out") && !strings.Contains(m, `"event_action":"resolve"`) {
				n++
			}
		}
	}
	return n
}

func TestAGatewayLoggedOutPastTheThresholdPagesOnBothChannelsOnce(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.logout()
	dir := t.TempDir()
	for i, min := range []int{0, 5, 10, 15} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatalf("firing %d: %v", i, err)
		}
		if i < 2 && gatewayPages(r) != 0 {
			t.Fatalf("paged at minute %d, before the 10-minute threshold: %v", min, r.sent)
		}
	}
	if got := gatewayPages(r); got != 2 {
		t.Fatalf("%d gateway pages, want 1 per channel: %v", got, r.sent)
	}
	for _, host := range []string{"ntfy.private.test", "events.pagerduty.test"} {
		joined := strings.Join(r.to(host), "\n")
		if !strings.Contains(joined, "Unrecognized Username or Password") || !strings.Contains(joined, "paper login") {
			t.Errorf("%s: the page lacks what the check said or the remedy: %v", host, r.to(host))
		}
	}
}

func TestAGatewayLoggedOutInsideARestartWindowPagesNothingUntilItEndsPlusTheThreshold(t *testing.T) {
	cases := map[string][]time.Time{
		"nightly 23:45 restart": {
			time.Date(2024, 1, 2, 23, 42, 0, 0, newYork), time.Date(2024, 1, 2, 23, 57, 0, 0, newYork),
			time.Date(2024, 1, 3, 0, 12, 0, 0, newYork), time.Date(2024, 1, 3, 0, 27, 0, 0, newYork)},
		"Sunday 09:00 cold restart": {
			time.Date(2024, 1, 7, 8, 56, 0, 0, newYork), time.Date(2024, 1, 7, 9, 11, 0, 0, newYork),
			time.Date(2024, 1, 7, 9, 26, 0, 0, newYork)},
	}
	for name, times := range cases {
		t.Run(name, func(t *testing.T) {
			r := &relay{sent: map[string][]string{}}
			g := newStandInGateway(t)
			g.logout()
			dir := t.TempDir()
			for _, at := range times {
				if err := gatewayRun(t, r, dir, g.path(), "boost", at); err != nil {
					t.Fatal(err)
				}
			}
			if got := gatewayPages(r); got != 0 {
				t.Fatalf("paged %d times inside the restart window: %v", got, r.sent)
			}
			// Still out once the window and the threshold have passed: it pages.
			end := times[len(times)-1]
			for _, add := range []time.Duration{20 * time.Minute, 35 * time.Minute, 50 * time.Minute} {
				if err := gatewayRun(t, r, dir, g.path(), "boost", end.Add(add)); err != nil {
					t.Fatal(err)
				}
			}
			if got := gatewayPages(r); got != 2 {
				t.Fatalf("%d pages after the window, want 1 per channel: %v", got, r.sent)
			}
		})
	}
}

func TestAGatewayThatLogsInAgainResolvesAndAFreshOutagePagesAnew(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.logout()
	dir := t.TempDir()
	for _, min := range []int{0, 5, 10, 15} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	g.login()
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 20)); err != nil {
		t.Fatal(err)
	}
	var resolved []string
	for _, m := range r.to("events.pagerduty.test") {
		if strings.Contains(m, `"event_action":"resolve"`) && strings.Contains(m, "gateway-logged-out:") {
			resolved = append(resolved, m)
		}
	}
	if len(resolved) != 1 {
		t.Fatalf("resolves %v, want one for the gateway incident", resolved)
	}
	g.logout()
	for _, min := range []int{25, 30, 40} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	if got := gatewayPages(r); got != 4 {
		t.Fatalf("%d pages, want 2 per channel (two outages)", got)
	}
}

func TestAStillLoggedOutGatewayIsRemindedHourlyNotEveryFiring(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.logout()
	dir := t.TempDir()
	for m := 0; m <= 130; m += 5 {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(10, 0).Add(time.Duration(m)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	// Paged at 10 min, reminded at 70 and 130 minutes: three per channel.
	if got := gatewayPages(r); got != 6 {
		t.Fatalf("%d gateway messages in 130 minutes, want 3 per channel", got)
	}
}

func TestOnlyTheOwningInstanceRunsTheGatewayCheck(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.logout()
	dir := t.TempDir()
	for _, min := range []int{0, 5, 10, 15} {
		if err := gatewayRun(t, r, dir, g.path(), "trad-ira", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	if got := gatewayPages(r); got != 0 {
		t.Fatalf("an instance that does not own the check paged %d times", got)
	}
}

func TestAGatewayCheckThatCannotRunFailsTheFiringAndPagesNoLogout(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	_ = os.WriteFile(filepath.Join(g.dir, "broken"), nil, 0o600)
	err := gatewayRun(t, r, t.TempDir(), g.path(), "boost", tuesday(14, 0))
	if err == nil || !strings.Contains(err.Error(), "gateway") {
		t.Fatalf("a check that exits 3 gave %v, want an error naming the gateway check", err)
	}
	if gatewayPages(r) != 0 {
		t.Fatal("a check that could not run paged a logout")
	}
}

// The grace counts from the window's end, not from the last check inside it
// (ADR 0019, "Alerting"): the nightly window ends at 00:30.
func TestTheGraceCountsFromTheWindowEndNotTheLastCheckInsideIt(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.logout()
	dir := t.TempDir()
	at := func(h, m int) time.Time { return time.Date(2024, 1, 3, h, m, 0, 0, newYork) }
	for _, c := range []time.Time{at(0, 25), at(0, 30), at(0, 35)} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", c); err != nil {
			t.Fatal(err)
		}
	}
	if got := gatewayPages(r); got != 0 {
		t.Fatalf("paged %d times at the window's end plus 5 minutes, want none until plus 10: %v", got, r.sent)
	}
	if err := gatewayRun(t, r, dir, g.path(), "boost", at(0, 40)); err != nil {
		t.Fatal(err)
	}
	if got := gatewayPages(r); got != 2 {
		t.Fatalf("%d pages at the window's end plus 10 minutes, want 1 per channel", got)
	}
}

// An outage that began before the window and runs through it is not paged
// inside the window, and is paged 10 minutes after it ends.
func TestAnOutageRunningIntoAWindowPagesTenMinutesAfterItEnds(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.logout()
	dir := t.TempDir()
	for _, c := range []time.Time{time.Date(2024, 1, 2, 23, 30, 0, 0, newYork), time.Date(2024, 1, 2, 23, 50, 0, 0, newYork), time.Date(2024, 1, 3, 0, 35, 0, 0, newYork)} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", c); err != nil {
			t.Fatal(err)
		}
	}
	if got := gatewayPages(r); got != 0 {
		t.Fatalf("paged %d times before the window's end plus 10 minutes", got)
	}
	if err := gatewayRun(t, r, dir, g.path(), "boost", time.Date(2024, 1, 3, 0, 40, 0, 0, newYork)); err != nil {
		t.Fatal(err)
	}
	if got := gatewayPages(r); got != 2 {
		t.Fatalf("%d pages, want 1 per channel", got)
	}
}

// The windows come from the restart times the Gateway is configured with
// (the same TI_GATEWAY_RESTART_TIME and TI_GATEWAY_COLD_RESTART the IBC render
// uses), not from the defaults.
func TestTheRestartWindowsFollowTheConfiguredRestartTimes(t *testing.T) {
	configured := map[string]string{"TI_GATEWAY_RESTART_TIME": "10:00 PM", "TI_GATEWAY_COLD_RESTART": "14:00"}
	cases := []struct {
		name string
		at   time.Time
		want int
	}{
		{"inside the configured nightly window", time.Date(2024, 1, 2, 22, 10, 0, 0, newYork), 0},
		{"the default window is no longer one", time.Date(2024, 1, 2, 23, 50, 0, 0, newYork), 2},
		{"inside the configured Sunday window", time.Date(2024, 1, 7, 14, 10, 0, 0, newYork), 0},
		{"the default Sunday window is no longer one", time.Date(2024, 1, 7, 9, 10, 0, 0, newYork), 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &relay{sent: map[string][]string{}}
			g := newStandInGateway(t)
			g.logout()
			dir := t.TempDir()
			// Seen out 30 minutes earlier, so only the window can hold the page back.
			for _, at := range []time.Time{c.at.Add(-30 * time.Minute), c.at} {
				if err := gatewayRunEnv(t, r, dir, g.path(), "boost", at, configured); err != nil {
					t.Fatal(err)
				}
			}
			if got := gatewayPages(r); got != c.want {
				t.Fatalf("%d pages, want %d: %v", got, c.want, r.sent)
			}
		})
	}
}

func TestAMalformedConfiguredRestartTimeFailsTheFiring(t *testing.T) {
	g := newStandInGateway(t)
	err := gatewayRunEnv(t, &relay{sent: map[string][]string{}}, t.TempDir(), g.path(), "boost", tuesday(14, 0), map[string]string{"TI_GATEWAY_RESTART_TIME": "midnight"})
	if err == nil || !strings.Contains(err.Error(), "TI_GATEWAY_RESTART_TIME") {
		t.Fatalf("got %v, want an error naming TI_GATEWAY_RESTART_TIME", err)
	}
}

// A check that finds no evidence in the journal (rotated, or aged out after
// the retry stop) does not resolve an open outage: absence is not a login.
func TestNoJournalEvidenceKeepsAnOpenOutageOpenAndPagesNothingWhenNoneIsOpen(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(g.dir, "silent"), nil, 0o600)
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(9, 0)); err != nil {
		t.Fatal(err)
	}
	if gatewayPages(r) != 0 {
		t.Fatal("no evidence and no open outage paged")
	}
	_ = os.Remove(filepath.Join(g.dir, "silent"))
	g.logout()
	for _, min := range []int{0, 5, 10} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(g.dir, "silent"), nil, 0o600)
	for _, min := range []int{15, 20} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range r.to("events.pagerduty.test") {
		if strings.Contains(m, `"event_action":"resolve"`) {
			t.Fatalf("an outage was resolved on no evidence: %v", r.to("events.pagerduty.test"))
		}
	}
	if gatewayPages(r) != 2 {
		t.Fatalf("%d pages, want the one page only", gatewayPages(r))
	}
}
