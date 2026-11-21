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
			for _, phrase := range []string{"IBKR app or website", "IBC 3.24.2 documents", "another TWS or Gateway session", "expected", "not yet confirmed by a real event", "setting did not hold", "remedy applies", "Stop using", "logged-out page", "docs/manual/server.md", target.name()} {
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

func TestGatewaySessionSetAsideResolvesHealthyChannels(t *testing.T) {
	target := gatewayTarget{}
	h := &hostGateway{}
	seen, sent, repeats := map[string]bool{}, map[string]time.Time{}, map[string]int{}
	healthy := &gatewayIntentSender{policyRecorder: policyRecorder{channel: "pagerduty"}}
	returning := &gatewayIntentSender{policyRecorder: policyRecorder{channel: "ntfy"}}
	record := func() error {
		if len(h.SessionNotices) > 0 && h.SessionNotices[0].Resolving && len(returning.alerts) == 0 {
			t.Error("persisted resolution-only state before set-aside channel took delivery")
		}
		return nil
	}
	at := tuesday(10, 0)
	if err := target.sessionNotices(h, []time.Time{at}, true, "host", []sender{healthy}, []channelFault{{channel: "ntfy"}}, seen, sent, repeats, at, record); err != nil {
		t.Errorf("set-aside channel added a notice error: %v", err)
	}
	if len(healthy.alerts) != 1 || len(healthy.resolutions) != 1 {
		t.Errorf("healthy channel: pages=%d resolutions=%d", len(healthy.alerts), len(healthy.resolutions))
	}
	if len(h.SessionNotices) != 1 {
		t.Fatal("notice lost before set-aside channel could take delivery")
	}
	for i := 1; i <= 2; i++ {
		if err := target.sessionNotices(h, nil, true, "host", []sender{healthy, returning}, nil, seen, sent, repeats, at.Add(time.Duration(i)*time.Minute), record); err != nil {
			t.Fatal(err)
		}
	}
	if len(healthy.alerts) != 1 || len(returning.alerts) != 1 || len(returning.resolutions) != 1 || len(h.SessionNotices) != 0 {
		t.Fatalf("retry: healthy pages=%d returned pages=%d resolutions=%d pending=%d", len(healthy.alerts), len(returning.alerts), len(returning.resolutions), len(h.SessionNotices))
	}
}

func TestGatewaySessionReadFaultAllowsLogoutRecovery(t *testing.T) {
	g := newStandInGateway(t)
	g.logout()
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	for _, at := range []time.Time{tuesday(10, 0), tuesday(10, 10)} {
		if err := gatewayRun(t, r, dir, g.path(), "boost", at); err != nil {
			t.Fatal(err)
		}
	}
	if gatewayPages(r) != 2 {
		t.Fatal("outage did not open")
	}
	g.login(tuesday(10, 11))
	g.flag("fail-session", true)
	if err := gatewayRun(t, r, dir, g.path(), "boost", tuesday(10, 15)); err == nil || !strings.Contains(err.Error(), "Session query failed") {
		t.Fatalf("session read fault lost: %v", err)
	}
	if count(r.to("events.pagerduty.test"), `"event_action":"resolve"`) != 1 {
		t.Fatal("session read fault blocked logout resolution")
	}
	h, err := loadHostGateway(filepath.Join(dir, "host-gateway.json"))
	if err != nil || h.Since != nil || h.Resolving {
		t.Fatalf("outage remains open: %+v, %v", h, err)
	}
}

func TestGatewaySessionSkippedFirstFiringDefersCursor(t *testing.T) {
	target := gatewayTarget{instance: "trad-ira"}
	path := filepath.Join(t.TempDir(), "gateway.json")
	sched, err := gatewayScheduleFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	at := tuesday(10, 0)
	skipped := true
	checks := 0
	p := &gatewayIntentSender{policyRecorder: policyRecorder{channel: "pagerduty"}}
	check := func(time.Time) (gatewayEvidence, error) {
		checks++
		return gatewayEvidence{sessions: []time.Time{tuesday(10, 1)}, recoveredAt: at}, nil
	}
	sample := func() (gatewayUnitSample, error) {
		if skipped {
			return gatewayUnitSample{skipped: "inactive and disabled"}, nil
		}
		return gatewayUnitSample{active: true}, nil
	}
	step := func() error {
		return target.step(path, check, sample, "host", sched, nil, "", nil, []sender{p}, nil, func() time.Time { return at })
	}
	if err := step(); err != nil {
		t.Fatal(err)
	}
	h, err := loadHostGateway(path)
	if err != nil || h.SessionNotifiedThrough != nil || checks != 0 {
		t.Errorf("skipped firing initialized cursor or read journal: %+v, checks=%d, err=%v", h, checks, err)
	}
	skipped = false
	at = tuesday(11, 0)
	if err := step(); err != nil {
		t.Fatal(err)
	}
	if checks != 1 || len(p.alerts) != 0 {
		t.Fatalf("first real read: checks=%d historical pages=%d", checks, len(p.alerts))
	}
}
