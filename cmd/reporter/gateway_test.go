package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The Gateway health check: the reporter runs the check command
// (it reads the ti-gateway and adapter journals itself) and pages
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

const (
	ibcRefused   = "host ibc[1]: detected dialog entitled: Unrecognized Username or Password; event=Opened"
	ibcCompleted = "host ibc[1]: Login has completed"
	ibcStopped   = "host gateway-start[1]: gateway-start: login retries stopped: 3 of 3 refused logins within the window"
	linkLost     = "host adapter[2]: ibkr link state=connectivity-lost reason=gateway-lost-ibkr client_id=11 attempt=None code=2110"
	linkUp       = "host adapter[2]: ibkr link state=connected reason=restored client_id=11 attempt=None code=1102"
	// The API handshake the adapter logs at every reconnect, restored or not.
	linkHandshake  = "host adapter[2]: ibkr link state=connected reason= client_id=11 attempt=None code=None"
	linkReconciled = "host adapter[2]: ibkr reconciled client_id=11 positions=0 open_orders=0 executions=0"
	ibcStarting    = "host gateway-start[1]: gateway-start: starting IBC"
)

// stamp is a journal line's time, as journalctl -o short-iso writes it.
func stamp(t time.Time, rest string) string { return t.Format("2006-01-02T15:04:05-0700") + " " + rest }

// standInGateway is a journalctl whose journals the test sets: files named by
// unit, filtered by -g as the real one does (exit 1 and "-- No entries --"
// when nothing matches), a failure, and the hint journalctl prints, with a
// zero status, when the user cannot see the journal.
type standInGateway struct{ dir string }

func newStandInGateway(t *testing.T) standInGateway {
	t.Helper()
	g := standInGateway{dir: t.TempDir()}
	script := `#!/bin/sh
echo "$*" >>'` + g.dir + `/argv'
unit=""; pattern=""
while [ $# -gt 0 ]; do case $1 in -u) unit=$2; shift;; -g) pattern=$2; shift;; esac; shift; done
if [ -e '` + g.dir + `/fail' ]; then echo "Failed to open journal files" >&2; exit 1; fi
if [ -e '` + g.dir + `/noaccess' ]; then echo "Hint: You are currently not seeing messages from other users and the system."; echo "-- No entries --"; exit 0; fi
out=$(cat '` + g.dir + `'/"$unit" 2>/dev/null)
out=$(printf '%s\n' "$out" | grep -E -- "$pattern")
if [ -z "$out" ]; then echo "-- No entries --"; exit 1; fi
printf '%s\n' "$out"
`
	if err := os.WriteFile(filepath.Join(g.dir, "journalctl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	// systemctl show -p NRestarts -p ActiveState -p SubState ti-gateway.service
	systemctl := `#!/bin/sh
d='` + g.dir + `'
if [ -e "$d/systemctl-fail" ]; then echo "Failed to connect to bus" >&2; exit 1; fi
echo "$@" >>"$d/systemctl-argv"
if [ -e "$d/systemctl-raw" ]; then cat "$d/systemctl-raw"; exit 0; fi
echo "NRestarts=$(cat "$d/nrestarts" 2>/dev/null || echo 0)"
echo "ActiveState=$(cat "$d/activestate" 2>/dev/null || echo active)"
echo "SubState=$(cat "$d/substate" 2>/dev/null || echo running)"
`
	if err := os.WriteFile(filepath.Join(g.dir, "systemctl"), []byte(systemctl), 0o700); err != nil {
		t.Fatal(err)
	}
	g.login()
	return g
}

func (g standInGateway) unit(restarts int, active, sub string) {
	_ = os.WriteFile(filepath.Join(g.dir, "nrestarts"), []byte(fmt.Sprint(restarts)), 0o600)
	_ = os.WriteFile(filepath.Join(g.dir, "activestate"), []byte(active), 0o600)
	_ = os.WriteFile(filepath.Join(g.dir, "substate"), []byte(sub), 0o600)
}

func (g standInGateway) journalctl() string { return filepath.Join(g.dir, "journalctl") }
func (g standInGateway) path() string       { return g.journalctl() }
func (g standInGateway) write(unit string, lines ...string) {
	_ = os.WriteFile(filepath.Join(g.dir, unit), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}
func (g standInGateway) ibc(lines ...string) { g.write("ti-gateway.service", lines...) }
func (g standInGateway) adapter(lines ...string) {
	g.write("ti-adapter-boost.service", lines...)
}
func (g standInGateway) flag(name string, on bool) {
	if on {
		_ = os.WriteFile(filepath.Join(g.dir, name), nil, 0o600)
	} else {
		_ = os.Remove(filepath.Join(g.dir, name))
	}
}

// login and logout stamp far in the future, so they count as newer than any
// outage a test has opened; tests of older evidence write their own stamps.
func (g standInGateway) login()  { g.ibc("2099-01-01T00:00:00-0500 " + ibcCompleted) }
func (g standInGateway) logout() { g.ibc("2099-01-01T00:00:00-0500 " + ibcRefused) }

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
		"-account", account, "-journalctl", check, "-systemctl", filepath.Join(filepath.Dir(check), "systemctl"), "-gateway-adapters", "ti-adapter-boost.service", "-gateway-owner", "boost"}
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

func TestAJournalThatCannotBeReadFailsTheFiringAndPagesNoLogout(t *testing.T) {
	for name, set := range map[string]func(g standInGateway){
		"journalctl fails":                func(g standInGateway) { g.flag("fail", true) },
		"journalctl hints at lost access": func(g standInGateway) { g.flag("noaccess", true) },
	} {
		t.Run(name, func(t *testing.T) {
			r := &relay{sent: map[string][]string{}}
			g := newStandInGateway(t)
			set(g)
			err := gatewayRun(t, r, t.TempDir(), g.path(), "boost", tuesday(14, 0))
			if err == nil || !strings.Contains(err.Error(), "gateway check") {
				t.Fatalf("got %v, want an error naming the gateway check", err)
			}
			if gatewayPages(r) != 0 {
				t.Fatal("a journal that could not be read paged a logout")
			}
		})
	}
}

func TestAMissingJournalctlFailsTheFiring(t *testing.T) {
	err := gatewayRun(t, &relay{sent: map[string][]string{}}, t.TempDir(), filepath.Join(t.TempDir(), "none"), "boost", tuesday(14, 0))
	if err == nil || !strings.Contains(err.Error(), "gateway check") {
		t.Fatalf("got %v", err)
	}
}

func TestTheRetryStopIsLoggedOutEvenAfterAnEarlierCompletedLogin(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.ibc(stamp(tuesday(1, 0), ibcCompleted), stamp(tuesday(9, 0), ibcStopped))
	dir := t.TempDir()
	for _, min := range []int{0, 5, 10} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	if gatewayPages(r) != 2 {
		t.Fatalf("%d pages for a stopped Gateway, want 1 per channel", gatewayPages(r))
	}
}

// Silence in IBC's journal still lets the adapter's evidence page.
func TestAdapterEvidenceIsExaminedWhenIBCsJournalHasNoLoginEvent(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.ibc("2024-01-01T00:00:00-0500 host ibc[1]: some unrelated line")
	g.adapter(stamp(tuesday(9, 0), linkLost))
	dir := t.TempDir()
	for _, min := range []int{0, 5, 10} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	if gatewayPages(r) != 2 {
		t.Fatalf("%d pages, want 1 per channel for a link lost for 5 hours", gatewayPages(r))
	}
}

// The link must have been lost without a connection since, for 3 hours: lost
// at 06:00, restored at 08:00 and lost again at 09:55 is a 5-minute loss.
func TestAdapterLossCountsOnlyAsOneContinuousLossOfThreeHours(t *testing.T) {
	cases := map[string]struct {
		lines []string
		want  int
	}{
		"lost, restored, lost again": {[]string{stamp(tuesday(6, 0), linkLost), stamp(tuesday(8, 0), linkUp), stamp(tuesday(9, 55), linkLost)}, 0},
		"lost for under 3h":          {[]string{stamp(tuesday(8, 31), linkLost)}, 0},
		"lost 3h and still lost":     {[]string{stamp(tuesday(6, 0), linkLost), stamp(tuesday(7, 0), linkLost)}, 2},
		"restored since":             {[]string{stamp(tuesday(5, 0), linkLost), stamp(tuesday(9, 59), linkUp)}, 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := &relay{sent: map[string][]string{}}
			g := newStandInGateway(t)
			g.adapter(c.lines...)
			dir := t.TempDir()
			for _, min := range []int{0, 5, 10, 15} {
				if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(11, min)); err != nil {
					t.Fatal(err)
				}
			}
			if got := gatewayPages(r); got != c.want {
				t.Fatalf("%d pages, want %d: %v", got, c.want, r.sent)
			}
		})
	}
}

// systemd 255 and later write -o short-iso offsets with a colon.
func TestAdapterLinesWithAColonInTheOffsetAreRead(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.adapter(tuesday(8, 0).Format("2006-01-02T15:04:05-07:00") + " " + linkLost)
	dir := t.TempDir()
	for _, min := range []int{0, 5, 10} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	if gatewayPages(r) != 2 {
		t.Fatalf("%d pages, want 1 per channel", gatewayPages(r))
	}
}

func TestAnAdapterJournalLineWithoutATimeFailsTheFiring(t *testing.T) {
	g := newStandInGateway(t)
	g.adapter("not-a-time " + linkLost)
	if err := gatewayRun(t, &relay{sent: map[string][]string{}}, t.TempDir(), g.path(), "boost", tuesday(14, 0)); err == nil {
		t.Fatal("an adapter line with no time was accepted")
	}
}

// A window that opens the previous evening: a restart configured just after
// midnight has its window from 23:55.
func TestARestartJustAfterMidnightHasItsWindowFromThePreviousEvening(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.logout()
	dir := t.TempDir()
	cfg := map[string]string{"TI_GATEWAY_RESTART_TIME": "12:00 AM"}
	for _, at := range []time.Time{tuesday(23, 40), tuesday(23, 57)} {
		if err := gatewayRunEnv(t, r, dir, g.path(), "boost", at, cfg); err != nil {
			t.Fatal(err)
		}
	}
	if got := gatewayPages(r); got != 0 {
		t.Fatalf("paged %d times at 23:57, inside the window that opens at 23:55: %v", got, r.sent)
	}
}

func TestAMissingTimeZoneFailsTheFiringInsteadOfGuessing(t *testing.T) {
	old := loadNewYork
	loadNewYork = func() (*time.Location, error) { return nil, errors.New("unknown time zone America/New_York") }
	defer func() { loadNewYork = old }()
	g := newStandInGateway(t)
	err := gatewayRun(t, &relay{sent: map[string][]string{}}, t.TempDir(), g.path(), "boost", tuesday(14, 0))
	if err == nil || !strings.Contains(err.Error(), "time zone") {
		t.Fatalf("got %v, want an error about the time zone data", err)
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
	g.ibc("2024-01-01T00:00:00-0500 host ibc[1]: some unrelated line")
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(9, 0)); err != nil {
		t.Fatal(err)
	}
	if gatewayPages(r) != 0 {
		t.Fatal("no evidence and no open outage paged")
	}
	g.logout()
	for _, min := range []int{0, 5, 10} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	g.ibc("2024-01-01T00:00:00-0500 host ibc[1]: some unrelated line")
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

// The adapter reconnects every restore_timeout while the Gateway has no IBKR,
// and logs the API handshake each time; only IBKR's restoration (or a
// reconciliation, which needs IBKR) ends a loss. A silent drop of 4 hours is
// lost, handshake, lost, handshake ... and pages.
func TestARepeatedReconnectWhileIBKRIsGoneIsOneLongLossAndPages(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	var lines []string
	for at := tuesday(6, 0); at.Before(tuesday(10, 0)); at = at.Add(5 * time.Minute) {
		lines = append(lines, stamp(at, linkLost), stamp(at.Add(time.Minute), linkHandshake))
	}
	g.adapter(lines...)
	dir := t.TempDir()
	for _, min := range []int{0, 5, 10} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(11, min)); err != nil {
			t.Fatal(err)
		}
	}
	if gatewayPages(r) != 2 {
		t.Fatalf("%d pages for a 5-hour silent drop with a reconnect every 5 minutes, want 1 per channel", gatewayPages(r))
	}
}

// The loss is timed from the FIRST loss after the newest restoration, not
// from the newest loss line.
func TestTheLossIsTimedFromTheFirstLossAfterTheNewestRestorationNotTheNewestLoss(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.adapter(stamp(tuesday(6, 0), linkLost), stamp(tuesday(10, 30), linkLost))
	dir := t.TempDir()
	for _, min := range []int{0, 5, 10} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(11, min)); err != nil {
			t.Fatal(err)
		}
	}
	if gatewayPages(r) != 2 {
		t.Fatalf("%d pages, want 1 per channel", gatewayPages(r))
	}
}

func TestAReconciliationEndsALoss(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.adapter(stamp(tuesday(6, 0), linkLost), stamp(tuesday(8, 0), linkReconciled), stamp(tuesday(10, 55), linkLost))
	dir := t.TempDir()
	for _, min := range []int{0, 5, 10, 15} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(11, min)); err != nil {
			t.Fatal(err)
		}
	}
	if gatewayPages(r) != 0 {
		t.Fatalf("paged %d times for a loss that began at 10:55", gatewayPages(r))
	}
}

// Evidence that rotates away must not resolve an open outage through an older
// completed login: only a login or restoration newer than the outage's start
// does.
func TestAnOutageIsNotResolvedByAnOlderLoginOnceItsEvidenceRotatesAway(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.ibc("2024-01-01T00:00:00-0500 " + ibcCompleted)
	g.adapter(stamp(tuesday(6, 0), linkLost))
	dir := t.TempDir()
	for _, min := range []int{0, 5, 10} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(11, min)); err != nil {
			t.Fatal(err)
		}
	}
	if gatewayPages(r) != 2 {
		t.Fatalf("%d pages, want 1 per channel", gatewayPages(r))
	}
	g.adapter() // the adapter's loss lines rotate away
	for _, min := range []int{15, 20} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(11, min)); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range r.to("events.pagerduty.test") {
		if strings.Contains(m, `"event_action":"resolve"`) {
			t.Fatal("an outage was resolved by a login older than its start")
		}
	}
	g.adapter(stamp(tuesday(11, 22), linkReconciled))
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(11, 25)); err != nil {
		t.Fatal(err)
	}
	resolved := 0
	for _, m := range r.to("events.pagerduty.test") {
		if strings.Contains(m, `"event_action":"resolve"`) {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("%d resolves after a reconciliation newer than the outage, want 1", resolved)
	}
}

// A restart of the unit (cmd/gateway-start logs "starting IBC") resets the
// login state until a new "Login has completed", even if the newest IBC login
// event before it was a completed one.
func TestAnIBCRestartWithNoLoginYetIsLoggedOut(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.ibc(stamp(tuesday(1, 0), ibcCompleted), stamp(tuesday(9, 0), ibcStarting))
	dir := t.TempDir()
	for _, min := range []int{0, 5, 10} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	if gatewayPages(r) != 2 {
		t.Fatalf("%d pages for an IBC that restarted and never logged in, want 1 per channel", gatewayPages(r))
	}
}

func TestTheJournalIsAskedForShortISOTimesWithoutAPagerFourteenDaysBack(t *testing.T) {
	g := newStandInGateway(t)
	g.adapter(stamp(tuesday(8, 0), linkUp))
	if err := gatewayRun(t, &relay{sent: map[string][]string{}}, t.TempDir(), g.path(), "boost", tuesday(14, 0)); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(g.dir, "argv"))
	calls := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(calls) != 2 {
		t.Fatalf("%d journal queries, want 2 (IBC and the adapter): %q", len(calls), calls)
	}
	for _, c := range calls {
		for _, want := range []string{"-o short-iso-precise", "--no-pager", "--since=-14d", "-g "} {
			if !strings.Contains(c, want) {
				t.Errorf("journalctl %q lacks %q", c, want)
			}
		}
	}
}

// A recovery whose notification fails is retried, never reverted to an
// outage that starts after the recovery.
func TestAFailedResolveIsRetriedAndNeverReopensTheOutage(t *testing.T) {
	f := &refusingResolve{relay: &relay{sent: map[string][]string{}}}
	g := newStandInGateway(t)
	g.logout()
	dir := t.TempDir()
	for _, min := range []int{0, 5, 10} {
		if err := gatewayRun(t, f, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	g.ibc(stamp(tuesday(14, 12), ibcCompleted))
	if err := gatewayRun(t, f, dir, g.path(), "boost", tuesday(14, 15)); err == nil {
		t.Error("a failed resolve was not reported as the run's error")
	}
	f.mu.Lock()
	f.allow = true
	f.mu.Unlock()
	for _, min := range []int{20, 25, 30} {
		if err := gatewayRun(t, f, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	resolved := 0
	for _, m := range f.to("events.pagerduty.test") {
		if strings.Contains(m, `"event_action":"resolve"`) {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("%d resolves, want 1 on the retry", resolved)
	}
	if got := gatewayPages(f.relay); got != 2 {
		t.Fatalf("%d outage pages, want the original one per channel only (the recovery was reopened)", got)
	}
}

// A login in the same second as the firing that saw the outage is newer: the
// journal's times are to the microsecond, and an equal time counts.
func TestALoginInTheSameSecondAsTheFiringThatSawTheOutageResolvesIt(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.logout()
	dir := t.TempDir()
	first := tuesday(14, 0).Add(400 * time.Millisecond)
	if err := gatewayRun(t, r, dir, g.path(), "boost", first); err != nil {
		t.Fatal(err)
	}
	for _, min := range []int{5, 11} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	// -o short-iso-precise: the login at 14:00:00.8 is after the outage seen at .4.
	g.ibc(tuesday(14, 0).Add(800*time.Millisecond).Format("2006-01-02T15:04:05.000000-0700") + " " + ibcCompleted)
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 15)); err != nil {
		t.Fatal(err)
	}
	resolved := 0
	for _, m := range r.to("events.pagerduty.test") {
		if strings.Contains(m, `"event_action":"resolve"`) {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("%d resolves for a login in the second after the outage was seen, want 1", resolved)
	}
}

func TestTheJournalIsAskedForPreciseTimes(t *testing.T) {
	g := newStandInGateway(t)
	if err := gatewayRun(t, &relay{sent: map[string][]string{}}, t.TempDir(), g.path(), "boost", tuesday(14, 0)); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(g.dir, "argv"))
	if !strings.Contains(string(raw), "-o short-iso-precise") {
		t.Fatalf("journalctl was asked %q, want -o short-iso-precise", raw)
	}
}

func TestTheGatewayUnitIsATradingUnitTheReporterCanSample(t *testing.T) {
	if err := checkTradingUnits("-units", []string{"ti-gateway.service"}); err != nil {
		t.Fatal(err)
	}
}

// A new outage while an earlier recovery's resolution has not been taken by
// every channel pages both channels at once.
func TestANewOutageWhileAResolveIsPendingPagesEveryChannelAtOnce(t *testing.T) {
	f := &refusingResolve{relay: &relay{sent: map[string][]string{}}}
	g := newStandInGateway(t)
	g.logout()
	dir := t.TempDir()
	for _, min := range []int{0, 5, 10} {
		if err := gatewayRun(t, f, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	g.ibc(stamp(tuesday(14, 12), ibcCompleted))
	_ = gatewayRun(t, f, dir, g.path(), "boost", tuesday(14, 15)) // the resolve fails
	g.ibc(stamp(tuesday(14, 16), ibcCompleted), stamp(tuesday(14, 17), ibcRefused))
	for _, min := range []int{20, 25, 31} {
		if err := gatewayRun(t, f, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{"ntfy.private.test", "events.pagerduty.test"} {
		n := 0
		for _, m := range f.to(host) {
			if strings.Contains(m, "IB Gateway is logged out") && !strings.Contains(m, `"event_action":"resolve"`) {
				n++
			}
		}
		if n != 2 {
			t.Errorf("%s took %d outage pages across two outages, want 2", host, n)
		}
	}
}

// A brief second outage that recovers inside its grace period does not discard
// the first outage's pending resolution: it is retried at the recovery.
func TestABriefSecondOutageDoesNotDiscardAPendingResolution(t *testing.T) {
	f := &refusingResolve{relay: &relay{sent: map[string][]string{}}}
	g := newStandInGateway(t)
	g.logout()
	dir := t.TempDir()
	for _, min := range []int{0, 5, 10} {
		if err := gatewayRun(t, f, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	g.ibc(stamp(tuesday(14, 12), ibcCompleted))
	_ = gatewayRun(t, f, dir, g.path(), "boost", tuesday(14, 15)) // the resolve fails
	g.ibc(stamp(tuesday(14, 16), ibcCompleted), stamp(tuesday(14, 17), ibcRefused))
	if err := gatewayRun(t, f, dir, g.path(), "boost", tuesday(14, 20)); err != nil { // out again, in its grace
		t.Fatal(err)
	}
	f.mu.Lock()
	f.allow = true
	f.mu.Unlock()
	g.ibc(stamp(tuesday(14, 21), ibcCompleted))
	if err := gatewayRun(t, f, dir, g.path(), "boost", tuesday(14, 25)); err != nil {
		t.Fatal(err)
	}
	resolved := 0
	for _, m := range f.to("events.pagerduty.test") {
		if strings.Contains(m, `"event_action":"resolve"`) {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("%d resolves, want the pending one delivered at the recovery", resolved)
	}
}

// While an earlier recovery's resolution is pending, a NEW outage is still an
// open outage that only a newer login can close: if its own evidence rotates
// away it is not erased.
func TestANewOutageDuringAPendingResolutionIsNotErasedWhenItsEvidenceRotatesAway(t *testing.T) {
	f := &refusingResolve{relay: &relay{sent: map[string][]string{}}}
	g := newStandInGateway(t)
	g.logout()
	dir := t.TempDir()
	for _, min := range []int{0, 5, 10} {
		if err := gatewayRun(t, f, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	g.ibc(stamp(tuesday(14, 12), ibcCompleted))
	_ = gatewayRun(t, f, dir, g.path(), "boost", tuesday(14, 15)) // the resolve fails: pending
	g.ibc(stamp(tuesday(14, 16), ibcCompleted), stamp(tuesday(14, 17), ibcRefused))
	if err := gatewayRun(t, f, dir, g.path(), "boost", tuesday(14, 20)); err != nil { // a new outage, in its grace
		t.Fatal(err)
	}
	g.ibc(stamp(tuesday(14, 12), ibcCompleted)) // its evidence rotates away; only the old login is left
	for _, min := range []int{25, 31} {
		if err := gatewayRun(t, f, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	if got := gatewayPages(f.relay); got != 4 {
		t.Fatalf("%d outage pages, want 2 per channel: the new outage was erased by missing evidence", got)
	}
}

// A start that keeps failing is seen through systemd's restart count, even when
// the journal has nothing newer than an old login.
func TestARisingRestartCountPagesWhenTheJournalShowsNothingNew(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.ibc(stamp(tuesday(1, 0), ibcCompleted))
	dir := t.TempDir()
	g.unit(0, "active", "running")
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 0)); err != nil {
		t.Fatal(err)
	}
	for i, min := range []int{5, 10, 16} {
		g.unit(10*(i+1), "activating", "start")
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	if gatewayPages(r) != 2 {
		t.Fatalf("%d pages for a unit restarting ten times between firings, want 1 per channel", gatewayPages(r))
	}
	for _, host := range []string{"ntfy.private.test", "events.pagerduty.test"} {
		if !strings.Contains(strings.Join(r.to(host), "\n"), "restart count") {
			t.Errorf("%s: the page does not say the unit keeps failing to start", host)
		}
	}
}

func TestASingleRestartPagesNothing(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	dir := t.TempDir()
	g.unit(0, "active", "running")
	_ = gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 0))
	for _, min := range []int{5, 10, 16, 25} {
		g.unit(1, "active", "running")
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	if gatewayPages(r) != 0 {
		t.Fatalf("%d pages for one restart", gatewayPages(r))
	}
}

func TestAUnitInAutoRestartAtTwoFiringsInARowPages(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	dir := t.TempDir()
	g.unit(2, "activating", "auto-restart")
	for _, min := range []int{0, 5, 10, 16} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	if gatewayPages(r) != 2 {
		t.Fatalf("%d pages for a unit in auto-restart at consecutive firings, want 1 per channel", gatewayPages(r))
	}
}

func TestAUnitSeenInAutoRestartAtOneFiringOnlyPagesNothing(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	dir := t.TempDir()
	g.unit(1, "activating", "auto-restart")
	_ = gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 0))
	g.unit(1, "active", "running")
	for _, min := range []int{5, 10, 16} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	if gatewayPages(r) != 0 {
		t.Fatalf("%d pages", gatewayPages(r))
	}
}

func TestARestartLoopInsideTheRestartWindowPagesAfterTheWindowEndsPlusGrace(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	dir := t.TempDir()
	at := func(h, m int) time.Time { return time.Date(2024, 1, 3, h, m, 0, 0, newYork) }
	g.unit(0, "active", "running")
	_ = gatewayRun(t, r, dir, g.path(), "boost", time.Date(2024, 1, 2, 23, 40, 0, 0, newYork))
	times := []time.Time{time.Date(2024, 1, 2, 23, 50, 0, 0, newYork), at(0, 10), at(0, 25), at(0, 35)}
	for i, c := range times {
		g.unit(10*(i+1), "activating", "start")
		if err := gatewayRun(t, r, dir, g.path(), "boost", c); err != nil {
			t.Fatal(err)
		}
	}
	if gatewayPages(r) != 0 {
		t.Fatalf("paged %d times inside the window or within its grace", gatewayPages(r))
	}
	g.unit(50, "activating", "start")
	if err := gatewayRun(t, r, dir, g.path(), "boost", at(0, 41)); err != nil {
		t.Fatal(err)
	}
	if gatewayPages(r) != 2 {
		t.Fatalf("%d pages after the window's end plus the grace, want 1 per channel", gatewayPages(r))
	}
}

func TestTheRestartLoopClearsWhenTheUnitIsActiveWithNoNewRestarts(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.ibc(stamp(tuesday(1, 0), ibcCompleted))
	dir := t.TempDir()
	g.unit(0, "active", "running")
	_ = gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 0))
	for i, min := range []int{5, 10, 16} {
		g.unit(10*(i+1), "activating", "start")
		_ = gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min))
	}
	g.unit(30, "active", "running")
	_ = gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 20)) // the last restart's count, no new ones
	_ = gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 25))
	resolved := 0
	for _, m := range r.to("events.pagerduty.test") {
		if strings.Contains(m, `"event_action":"resolve"`) {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("%d resolves after the unit stayed active with no new restarts, want 1", resolved)
	}
}

func TestASystemctlThatFailsFailsTheFiring(t *testing.T) {
	g := newStandInGateway(t)
	_ = os.WriteFile(filepath.Join(g.dir, "systemctl-fail"), nil, 0o600)
	err := gatewayRun(t, &relay{sent: map[string][]string{}}, t.TempDir(), g.path(), "boost", tuesday(14, 0))
	if err == nil || !strings.Contains(err.Error(), "gateway check") {
		t.Fatalf("got %v, want an error naming the gateway check", err)
	}
}

// The loop ends partway through an interval: the firing that sees one more
// restart does not strand the outage, and two quiet firings close it.
func TestALoopThatEndsPartwayStillClosesAfterTwoQuietFirings(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.ibc(stamp(tuesday(1, 0), ibcCompleted))
	dir := t.TempDir()
	g.unit(0, "active", "running")
	_ = gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 0))
	for i, min := range []int{5, 10, 16} {
		g.unit(10*(i+1), "activating", "start")
		_ = gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min))
	}
	resolves := func() int {
		n := 0
		for _, m := range r.to("events.pagerduty.test") {
			if strings.Contains(m, `"event_action":"resolve"`) {
				n++
			}
		}
		return n
	}
	g.unit(31, "active", "running") // one more restart before it settled
	_ = gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 20))
	g.unit(31, "active", "running")
	_ = gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 25))
	if resolves() != 0 {
		t.Fatal("the outage closed after one quiet firing")
	}
	_ = gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 30))
	if resolves() != 1 {
		t.Fatalf("%d resolves after two quiet firings, want 1", resolves())
	}
}

// A slow loop (one restart per firing, at two firings in a row) is a failing
// start; a single restart is not.
func TestASlowLoopOfOneRestartPerFiringPages(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.ibc(stamp(tuesday(1, 0), ibcCompleted))
	dir := t.TempDir()
	g.unit(0, "active", "running")
	_ = gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, 0))
	for i, min := range []int{5, 10, 16, 21} {
		g.unit(i+1, "activating", "start")
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min)); err != nil {
			t.Fatal(err)
		}
	}
	if gatewayPages(r) != 2 {
		t.Fatalf("%d pages for a restart at every firing, want 1 per channel", gatewayPages(r))
	}
}

// An active unit with no new restarts closes only an outage the restart rule
// opened; it never resolves one the journal evidenced, even after that
// evidence has rotated away.
func TestAnActiveUnitDoesNotResolveAnOutageTheJournalEvidenced(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.logout() // a refusal: the journal evidences the outage
	dir := t.TempDir()
	g.unit(0, "active", "running")
	for _, min := range []int{0, 5, 10, 16} {
		_ = gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min))
	}
	if gatewayPages(r) != 2 {
		t.Fatalf("%d pages, want 1 per channel", gatewayPages(r))
	}
	// A restart loop appears and ends, while the journal's evidence rotates to an old login.
	g.ibc(stamp(tuesday(1, 0), ibcCompleted))
	for i, min := range []int{20, 25} {
		g.unit(10*(i+1), "activating", "start")
		_ = gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min))
	}
	g.unit(20, "active", "running")
	for _, min := range []int{30, 35, 40, 45} {
		_ = gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min))
	}
	for _, m := range r.to("events.pagerduty.test") {
		if strings.Contains(m, `"event_action":"resolve"`) {
			t.Fatal("an outage the journal had evidenced was resolved by an active unit with no new restarts")
		}
	}
}

// The sampler fails the firing on output it cannot trust, never writing a
// baseline of invented zeros.
func TestTheSamplerRejectsOutputItCannotTrust(t *testing.T) {
	for name, raw := range map[string]string{
		"no NRestarts":      "ActiveState=active\nSubState=running\n",
		"no SubState":       "NRestarts=0\nActiveState=active\n",
		"empty ActiveState": "NRestarts=0\nActiveState=\nSubState=running\n",
		"unknown state":     "NRestarts=0\nActiveState=confused\nSubState=running\n",
		"NRestarts a word":  "NRestarts=many\nActiveState=active\nSubState=running\n",
	} {
		t.Run(name, func(t *testing.T) {
			g := newStandInGateway(t)
			_ = os.WriteFile(filepath.Join(g.dir, "systemctl-raw"), []byte(raw), 0o600)
			err := gatewayRun(t, &relay{sent: map[string][]string{}}, t.TempDir(), g.path(), "boost", tuesday(14, 0))
			if err == nil || !strings.Contains(err.Error(), "gateway check") {
				t.Fatalf("got %v, want an error naming the gateway check", err)
			}
		})
	}
}

// A systemctl that fails does not suppress a page the journal evidences: the
// page goes out, and the failure is the firing's error.
func TestASystemctlFailureDoesNotSuppressAJournalEvidencedPage(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	g := newStandInGateway(t)
	g.logout()
	_ = os.WriteFile(filepath.Join(g.dir, "systemctl-fail"), nil, 0o600)
	dir := t.TempDir()
	for _, min := range []int{0, 5, 10, 16} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(14, min)); err == nil {
			t.Fatal("a failing systemctl was not the firing's error")
		}
	}
	if gatewayPages(r) != 2 {
		t.Fatalf("%d pages with systemctl failing, want 1 per channel from the journal's evidence", gatewayPages(r))
	}
}

// A unit file written for the previous build still passes -gateway-marker,
// which is accepted and ignored, so a binary installed alone still runs.
func TestAnOldUnitFilesMarkerFlagIsAcceptedAndIgnored(t *testing.T) {
	g := newStandInGateway(t)
	if err := gatewayRun(t, &relay{sent: map[string][]string{}}, t.TempDir(), g.path(), "boost", tuesday(14, 0), "-gateway-marker", "/var/lib/ti-gateway/last-start-failure"); err != nil {
		t.Fatalf("the old argv was refused: %v", err)
	}
}
