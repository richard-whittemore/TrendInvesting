package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const sessionDialog = "host ibc[1]: 2026-11-20 13:05:45:805 IBC: detected dialog entitled: Existing session detected; event=Opened"

func sessionPages(r *relay) int {
	n := 0
	for _, m := range r.to("events.pagerduty.test") {
		if strings.Contains(m, `"event_action":"trigger"`) && strings.Contains(m, "another login") {
			n++
		}
	}
	return n
}

func TestGatewaySessionNotice(t *testing.T) {
	for _, instance := range []string{"", "trad-ira"} {
		t.Run(instance, func(t *testing.T) {
			g := newStandInGateway(t)
			r := &relay{sent: map[string][]string{}}
			dir := t.TempDir()
			target := gatewayTarget{instance: instance}
			account := "boost"
			var extra []string
			if instance != "" {
				account = instance
				envPath := filepath.Join(dir, "gateway.env")
				if err := os.WriteFile(envPath, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				extra = []string{"-gateway-instance-env", envPath}
			}
			lines := []string{stamp(tuesday(9, 0), ibcCompleted), stamp(tuesday(9, 59), sessionDialog)}
			runAt := func(at time.Time, want int) {
				t.Helper()
				g.write(target.unit(), lines...)
				if err := gatewayRun(t, r, dir, g.path(), account, at, extra...); err != nil {
					t.Fatal(err)
				}
				if got := sessionPages(r); got != want {
					t.Fatalf("session pages=%d want=%d: %v", got, want, r.sent)
				}
			}
			runAt(tuesday(10, 0), 1)
			runAt(tuesday(10, 5), 1)
			lines = append(lines, stamp(tuesday(10, 6), sessionDialog))
			runAt(tuesday(10, 10), 2)
			runAt(tuesday(10, 15), 2)
			if got := count(r.to("events.pagerduty.test"), `"event_action":"resolve"`); got != 2 {
				t.Errorf("resolutions=%d want=2", got)
			}
			joined := strings.Join(r.to("ntfy.private.test"), "\n")
			for _, phrase := range []string{"IBKR app or website", "take the session back", "no restart", "Stop using", "logged-out page", "docs/manual/server.md", target.name()} {
				if !strings.Contains(joined, phrase) {
					t.Errorf("missing %q in %s", phrase, joined)
				}
			}
		})
	}
}

func TestGatewaySessionOldFirstRunRecorded(t *testing.T) {
	g := newStandInGateway(t)
	g.ibc(stamp(tuesday(9, 0), ibcCompleted), stamp(tuesday(9, 44), sessionDialog))
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(10, 0)); err != nil {
		t.Fatal(err)
	}
	if sessionPages(r) != 0 {
		t.Fatal("paged historical session")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "host-gateway.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "session_notified_through") {
		t.Fatalf("historical event not recorded: %s", raw)
	}
	// Moving the firing clock back cannot make the recorded historical line new.
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(9, 45)); err != nil {
		t.Fatal(err)
	}
	if sessionPages(r) != 0 {
		t.Fatal("re-paged historical session")
	}
}

func TestGatewaySessionDoesNotChangeLogoutEvidence(t *testing.T) {
	for _, state := range []string{ibcCompleted, ibcRefused} {
		g := newStandInGateway(t)
		g.ibc(stamp(tuesday(9, 0), state))
		check := gatewayJournalChecker(g.path(), nil)
		before, err := check(tuesday(10, 0))
		if err != nil {
			t.Fatal(err)
		}
		g.ibc(stamp(tuesday(9, 0), state), stamp(tuesday(9, 59), sessionDialog))
		after, err := check(tuesday(10, 0))
		if err != nil {
			t.Fatal(err)
		}
		// Compare only outage evidence; notices carry separate journal timestamps.
		if before.out != after.out || before.silent != after.silent || before.said != after.said || !reflect.DeepEqual(before.recoveredAt, after.recoveredAt) {
			t.Fatalf("logout changed: before=%+v after=%+v", before, after)
		}
	}
}

func TestGatewaySessionNoticeResolutionRetry(t *testing.T) {
	g := newStandInGateway(t)
	g.ibc(stamp(tuesday(9, 0), ibcCompleted), stamp(tuesday(9, 59), sessionDialog))
	dir := t.TempDir()
	failing := &resolvesFail{relay: relay{sent: map[string][]string{}}}
	if err := gatewayRun(t, failing, dir, g.path(), "boost", tuesday(10, 0)); err == nil || !strings.Contains(err.Error(), "resolve") {
		t.Fatalf("resolution failure lost: %v", err)
	}
	if sessionPages(&failing.relay) != 1 {
		t.Fatal("notice was not delivered")
	}
	r := &relay{sent: map[string][]string{}}
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(10, 5)); err != nil {
		t.Fatal(err)
	}
	if sessionPages(r) != 0 {
		t.Fatal("resolution retry re-paged notice")
	}
	if count(r.to("events.pagerduty.test"), `"event_action":"resolve"`) != 1 {
		t.Fatal("notice resolution not retried")
	}
}

func TestGatewaySessionAllowsLogoutResolution(t *testing.T) {
	g := newStandInGateway(t)
	g.logout()
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	for _, at := range []time.Time{tuesday(10, 0), tuesday(10, 10)} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", at); err != nil {
			t.Fatal(err)
		}
	}
	g.ibc(stamp(tuesday(10, 11), ibcCompleted), stamp(tuesday(10, 12), sessionDialog))
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(10, 15)); err != nil {
		t.Fatal(err)
	}
	if gatewayPages(r) != 2 || sessionPages(r) != 1 {
		t.Fatalf("outage and notice pages: %v", r.sent)
	}
	if count(r.to("events.pagerduty.test"), `"event_action":"resolve"`) != 2 {
		t.Fatal("notice prevented logout resolution")
	}
}

func TestGatewaySessionJournalFailureIsNotEmpty(t *testing.T) {
	g := newStandInGateway(t)
	g.ibc(stamp(tuesday(9, 0), ibcCompleted), "bad-time "+sessionDialog)
	dir := t.TempDir()
	r := &relay{sent: map[string][]string{}}
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(10, 0)); err == nil {
		t.Fatal("malformed notice timestamp treated as no event")
	}
	g.flag("noaccess", true)
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(10, 5)); err == nil {
		t.Fatal("unreadable journal treated as no event")
	}
}

func TestGatewaySessionPendingResolutionDoesNotBlockLaterNotice(t *testing.T) {
	g := newStandInGateway(t)
	first := stamp(tuesday(9, 59), sessionDialog)
	g.ibc(stamp(tuesday(9, 0), ibcCompleted), first)
	dir := t.TempDir()
	failing := &resolvesFail{relay: relay{sent: map[string][]string{}}}
	_ = gatewayRun(t, failing, dir, g.path(), "boost", tuesday(10, 0))
	g.ibc(stamp(tuesday(9, 0), ibcCompleted), first, stamp(tuesday(10, 1), sessionDialog))
	_ = gatewayRun(t, failing, dir, g.path(), "boost", tuesday(10, 5))
	if got := sessionPages(&failing.relay); got != 2 {
		t.Fatalf("pages=%d want=2 despite pending resolution", got)
	}
}

// sessionDeliveryFails refuses only the session page on ntfy; successful
// channel receipts must survive retry (server.md, "When another login takes the Gateway's session").
type sessionDeliveryFails struct{ relay }

func (r *sessionDeliveryFails) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "ntfy.private.test" && strings.Contains(req.Header.Get("Title"), "another login") {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Status: "503", Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: req}, nil
	}
	return r.relay.RoundTrip(req)
}

func TestGatewaySessionPartialDeliveryRetriesOnlyMissingChannel(t *testing.T) {
	g := newStandInGateway(t)
	g.ibc(stamp(tuesday(9, 0), ibcCompleted), stamp(tuesday(9, 59), sessionDialog))
	dir := t.TempDir()
	failing := &sessionDeliveryFails{relay: relay{sent: map[string][]string{}}}
	if err := gatewayRun(t, failing, dir, g.path(), "boost", tuesday(10, 0)); err == nil {
		t.Fatal("delivery failure lost")
	}
	if sessionPages(&failing.relay) != 1 {
		t.Fatal("healthy channel did not page")
	}
	r := &relay{sent: map[string][]string{}}
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(10, 5)); err != nil {
		t.Fatal(err)
	}
	if sessionPages(r) != 0 {
		t.Fatal("successful channel re-paged")
	}
	if count(r.to("ntfy.private.test"), "session used by another login") != 1 {
		t.Fatal("missing channel not retried once")
	}
}
