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
	script := "#!/bin/sh\ncat '" + g.dir + "/message'\nif test -e '" + g.dir + "/out'; then exit 1; fi\nif test -e '" + g.dir + "/broken'; then exit 3; fi\n"
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
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty", "-repeat-interval", "30m",
		"-account", account, "-gateway-check", check, "-gateway-owner", "boost"}
	args = append(args, extra...)
	return run(args, twoChannels(), func() time.Time { return at }, &bytes.Buffer{}, &http.Client{Transport: r})
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
