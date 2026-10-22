package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The unit-failure seam: systemd never restarts the engine or the adapter, so
// a stopped one waits for the owner. The OnFailure hook runs `reporter
// notify-unit-failed <unit>`, which sends one urgent page through the
// reporter's channels naming the unit, its last journal line and the manual
// row, once per failure; the reporter's own firings page a failed trading
// unit the hook missed and resolve the page when the unit is no longer failed
// (ADR 0019, "Alerting"). The channels, systemctl and journalctl are
// stand-ins.

const (
	ntfyHost = "ntfy.private.test"
	pdHost   = "events.pagerduty.test"
)

// standInSystemd is a systemctl whose failed units the test sets, and a
// journalctl that prints a canned last line for any unit and records how it
// was called.
type standInSystemd struct{ dir string }

func newStandInSystemd(t *testing.T) standInSystemd {
	t.Helper()
	s := standInSystemd{dir: t.TempDir()}
	systemctl := "#!/bin/sh\n# is-failed <unit>\nif grep -qx \"$2\" '" + s.dir + "/failed'; then echo failed; exit 0; fi\necho active; exit 1\n"
	journalctl := "#!/bin/sh\necho \"$@\" >>'" + s.dir + "/journalctl-calls'\ncat '" + s.dir + "/last-line'\n"
	for name, script := range map[string]string{"systemctl": systemctl, "journalctl": journalctl} {
		if err := os.WriteFile(filepath.Join(s.dir, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	s.fail()
	s.say("adapter stopping: ReconcileIncomplete: IBKR's open-order stream never paused")
	return s
}

func (s standInSystemd) systemctl() string  { return filepath.Join(s.dir, "systemctl") }
func (s standInSystemd) journalctl() string { return filepath.Join(s.dir, "journalctl") }
func (s standInSystemd) say(line string) {
	_ = os.WriteFile(filepath.Join(s.dir, "last-line"), []byte(line+"\n"), 0o600)
}
func (s standInSystemd) fail(units ...string) {
	_ = os.WriteFile(filepath.Join(s.dir, "failed"), []byte(strings.Join(units, "\n")+"\n"), 0o600)
}
func (s standInSystemd) calls() string {
	raw, _ := os.ReadFile(filepath.Join(s.dir, "journalctl-calls"))
	return string(raw)
}

// pagesFor counts the messages on host that are the page for unit.
func pagesFor(r *relay, host, unit string) int { return count(r.to(host), unit+" failed") }

func notify(t *testing.T, transport http.RoundTripper, dir string, systemd standInSystemd, getenv func(string) string, unit string, extra ...string) error {
	t.Helper()
	args := append([]string{"notify-unit-failed", "-state", filepath.Join(dir, "unit-failures.json"), "-channel", "ntfy,pagerduty", "-journalctl", systemd.journalctl()}, extra...)
	args = append(args, unit)
	return run(args, getenv, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, &http.Client{Transport: transport})
}

func TestTheHookPagesBothChannelsOnceWithTheUnitTheReasonAndTheManualRow(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	if err := notify(t, r, dir, systemd, twoChannels(), "ti-adapter-boost.service"); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{ntfyHost, pdHost} {
		got := strings.Join(r.to(host), "\n")
		if pagesFor(r, host, "ti-adapter-boost.service") < 1 {
			t.Fatalf("%s took no page for the unit:\n%s", host, got)
		}
		for _, want := range []string{"ReconcileIncomplete", "docs/manual/ibkr-adapter.md", "The adapter's unit failed"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: the page lacks %q:\n%s", host, want, got)
			}
		}
	}
	if !strings.Contains(r.to(pdHost)[0], "unit-failed:ti-adapter-boost.service") {
		t.Errorf("PagerDuty's incident is not keyed by the unit: %s", r.to(pdHost)[0])
	}
	if !strings.Contains(systemd.calls(), "_SYSTEMD_UNIT=ti-adapter-boost.service") || !strings.Contains(systemd.calls(), "-n 1") {
		t.Errorf("the reason must be the unit's own last line, not systemd's: journalctl %s", systemd.calls())
	}
}

func TestTheHookPagesOncePerUnitUntilItIsResolved(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := notify(t, r, dir, systemd, twoChannels(), "ti-adapter-boost.service"); err != nil {
			t.Fatal(err)
		}
	}
	if err := notify(t, r, dir, systemd, twoChannels(), "ti-engine@boost.service"); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, "ti-adapter-boost.service"); n != 1 {
			t.Errorf("%s took %d pages for the adapter, want 1", host, n)
		}
		if n := pagesFor(r, host, "ti-engine@boost.service"); n != 1 {
			t.Errorf("%s took %d pages for the engine, want its own 1", host, n)
		}
	}
}

func TestAChannelThatFailedIsRetriedAndTheOneThatTookItIsNot(t *testing.T) {
	flaky := &flakyRelay{relay: &relay{sent: map[string][]string{}}, failHost: ntfyHost}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	if err := notify(t, flaky, dir, systemd, twoChannels(), "ti-engine@boost.service"); err == nil || !strings.Contains(err.Error(), "ntfy") {
		t.Fatalf("err = %v, want the failed channel named so the hook unit shows failed", err)
	}
	flaky.setFailing(false)
	if err := notify(t, flaky, dir, systemd, twoChannels(), "ti-engine@boost.service"); err != nil {
		t.Fatal(err)
	}
	if n := pagesFor(flaky.relay, pdHost, "ti-engine@boost.service"); n != 1 {
		t.Errorf("PagerDuty took %d pages, want 1", n)
	}
	if n := pagesFor(flaky.relay, ntfyHost, "ti-engine@boost.service"); n != 1 {
		t.Errorf("ntfy took %d pages after its retry, want 1", n)
	}
}

type flakyRelay struct {
	*relay
	mu       sync.Mutex
	failHost string
	healed   bool
}

func (f *flakyRelay) setFailing(on bool) { f.mu.Lock(); f.healed = !on; f.mu.Unlock() }

func (f *flakyRelay) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	down := req.URL.Host == f.failHost && !f.healed
	f.mu.Unlock()
	if down {
		_, _ = io.Copy(io.Discard, req.Body)
		return &http.Response{StatusCode: http.StatusBadGateway, Status: "502 Bad Gateway", Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: req}, nil
	}
	return f.relay.RoundTrip(req)
}

// TestNoPageCarriesASecretAnAccountOrAnAddress: the journal's last line may
// echo anything, so the page is cleaned before it leaves the server.
func TestNoPageCarriesASecretAnAccountOrAnAddress(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	systemd.say("login DU7654321 failed using topic zebra-topic-77 and key pdkey-5521 at https://hc-ping.com/uuid-8842")
	environment := env(map[string]string{
		"TI_NTFY_TOPIC": "zebra-topic-77", "TI_NTFY_SERVER": "https://ntfy.private.test",
		"TI_PAGERDUTY_ROUTING_KEY": "pdkey-5521", "TI_PAGERDUTY_URL": "https://events.pagerduty.test/v2/enqueue",
		"TI_HEARTBEAT_URL": "https://hc-ping.com/uuid-8842",
	})
	if err := notify(t, r, t.TempDir(), systemd, environment, "ti-adapter-boost.service"); err != nil {
		t.Fatal(err)
	}
	// PagerDuty's routing key is the credential of the request itself, so it
	// is looked for in what the page says, not in the request's envelope.
	all := strings.ReplaceAll(strings.Join(append(r.to(ntfyHost), r.to(pdHost)...), "\n"), `"routing_key":"pdkey-5521"`, "")
	for _, leak := range []string{"DU7654321", "pdkey-5521", "hc-ping.com", "uuid-8842"} {
		if strings.Contains(all, leak) {
			t.Errorf("a page carries %q:\n%s", leak, all)
		}
	}
	// The topic is the ntfy path, so it may be in the request URL but never
	// in a message.
	if bodies := strings.Join(r.to(ntfyHost), "\n"); strings.Contains(bodies, "zebra-topic-77") {
		t.Errorf("an ntfy message carries the topic:\n%s", bodies)
	}
}

func TestTheHookPagesEvenWhenTheJournalCannotBeRead(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	_ = os.Remove(systemd.journalctl())
	if err := notify(t, r, t.TempDir(), systemd, twoChannels(), "ti-engine@boost.service"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.to(pdHost), "\n"); !strings.Contains(got, "ti-engine@boost.service failed") || !strings.Contains(got, "could not be read") {
		t.Errorf("the page must go, saying the reason could not be read:\n%s", got)
	}
}

func TestTheHookRefusesAnythingButAUnitName(t *testing.T) {
	for _, unit := range []string{"", "--help", "-n1", "a b.service", "../etc/passwd", "ti-engine@boost.service;rm"} {
		r := &relay{sent: map[string][]string{}}
		systemd := newStandInSystemd(t)
		if err := notify(t, r, t.TempDir(), systemd, twoChannels(), unit); err == nil {
			t.Errorf("%q: accepted", unit)
		}
		if len(r.to(ntfyHost))+len(r.to(pdHost)) != 0 || systemd.calls() != "" {
			t.Errorf("%q: something was sent or read", unit)
		}
	}
}

func TestTheHookWithNoWorkingChannelFailsLoudly(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	if err := notify(t, r, t.TempDir(), newStandInSystemd(t), env(nil), "ti-engine@boost.service"); err == nil {
		t.Fatal("a hook that reaches no one must fail, so the hook unit shows failed")
	}
}

func firing(t *testing.T, r http.RoundTripper, dir string, systemd standInSystemd, at time.Time, units string, extra ...string) error {
	t.Helper()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty",
		"-units", units, "-systemctl", systemd.systemctl(), "-journalctl", systemd.journalctl()}
	args = append(args, extra...)
	return run(args, twoChannels(), func() time.Time { return at }, &bytes.Buffer{}, &http.Client{Transport: r})
}

// TestTheReportersFiringPagesAFailedUnitTheHookMissedOnceAndNotWhenTheHookDid:
// both write one record, beside the reporter's state.
func TestTheReportersFiringPagesAFailedUnitTheHookMissedOnceAndNotWhenTheHookDid(t *testing.T) {
	t.Run("the hook missed it", func(t *testing.T) {
		r := &relay{sent: map[string][]string{}}
		systemd := newStandInSystemd(t)
		systemd.fail("ti-adapter-boost.service")
		dir := t.TempDir()
		for i := 0; i < 3; i++ {
			if err := firing(t, r, dir, systemd, t0.Add(time.Hour+time.Duration(i)*5*time.Minute), "ti-engine@boost.service,ti-adapter-boost.service"); err != nil {
				t.Fatal(err)
			}
		}
		for _, host := range []string{ntfyHost, pdHost} {
			if n := pagesFor(r, host, "ti-adapter-boost.service"); n != 1 {
				t.Errorf("%s took %d pages for the failed adapter, want 1", host, n)
			}
			if n := pagesFor(r, host, "ti-engine@boost.service"); n != 0 {
				t.Errorf("%s was paged for an engine that is not failed", host)
			}
		}
	})
	t.Run("the hook paged first", func(t *testing.T) {
		r := &relay{sent: map[string][]string{}}
		systemd := newStandInSystemd(t)
		systemd.fail("ti-adapter-boost.service")
		dir := t.TempDir()
		if err := notify(t, r, dir, systemd, twoChannels(), "ti-adapter-boost.service"); err != nil {
			t.Fatal(err)
		}
		if err := firing(t, r, dir, systemd, t0.Add(time.Hour), "ti-adapter-boost.service"); err != nil {
			t.Fatal(err)
		}
		for _, host := range []string{ntfyHost, pdHost} {
			if n := pagesFor(r, host, "ti-adapter-boost.service"); n != 1 {
				t.Errorf("%s took %d pages, want 1 between the hook and the firing", host, n)
			}
		}
	})
	t.Run("the unit-state file sits beside the reporter's", func(t *testing.T) {
		r := &relay{sent: map[string][]string{}}
		systemd := newStandInSystemd(t)
		systemd.fail("ti-adapter-boost.service")
		dir := t.TempDir()
		if err := firing(t, r, dir, systemd, t0.Add(time.Hour), "ti-adapter-boost.service"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "unit-failures.json")); err != nil {
			t.Errorf("no unit-failures.json beside the state file: %v", err)
		}
	})
}

// TestAUnitNoLongerFailedResolvesItsPageAndTheNextFailureIsTold: PagerDuty
// folds a trigger into an open incident under its key, so the page is
// resolved when the unit recovers; otherwise the next failure would be silent.
func TestAUnitNoLongerFailedResolvesItsPageAndTheNextFailureIsTold(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	systemd.fail("ti-adapter-boost.service")
	dir := t.TempDir()
	at := func(i int) time.Time { return t0.Add(time.Hour + time.Duration(i)*5*time.Minute) }
	step := func(i int) {
		t.Helper()
		if err := firing(t, r, dir, systemd, at(i), "ti-adapter-boost.service"); err != nil {
			t.Fatal(err)
		}
	}
	step(0)
	systemd.fail()
	step(1)
	if resolves := count(r.to(pdHost), `"event_action":"resolve"`); resolves != 1 {
		t.Errorf("PagerDuty resolves = %d, want 1: %v", resolves, r.to(pdHost))
	}
	step(2)
	if resolves := count(r.to(pdHost), `"event_action":"resolve"`); resolves != 1 {
		t.Errorf("a unit that stays well resolved again: %d", resolves)
	}
	systemd.fail("ti-adapter-boost.service")
	step(3)
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, "ti-adapter-boost.service"); n != 2 {
			t.Errorf("%s took %d pages across two failures, want 2", host, n)
		}
	}
}

// TestTheStateAtStartPageNamesAFailedTradingUnit: a restarted reporter, or a
// channel just named, learns of a unit that failed before it began.
func TestTheStateAtStartPageNamesAFailedTradingUnit(t *testing.T) {
	withBaseline(t)
	systemd := newStandInSystemd(t)
	systemd.fail("ti-adapter-boost.service")
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	// A log with nothing in force: the standing would otherwise not be sent.
	path := writeRun(t, dir, haltedEntries(t)[:3])
	args := []string{"-journal", path, "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty",
		"-units", "ti-engine@boost.service,ti-adapter-boost.service", "-systemctl", systemd.systemctl(), "-journalctl", systemd.journalctl()}
	if err := run(args, twoChannels(), func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, &http.Client{Transport: r}); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{ntfyHost, pdHost} {
		var standing string
		for _, m := range r.to(host) {
			if strings.Contains(m, "state at reporter start") {
				standing = m
			}
		}
		if standing == "" || !strings.Contains(standing, "ti-adapter-boost.service") || strings.Contains(standing, "ti-engine@boost.service") {
			t.Errorf("%s: the state-at-start page must name the failed adapter and only it: %q", host, standing)
		}
	}

	// With no unit failed and nothing in force, no standing page is sent.
	quiet := newStandInSystemd(t)
	quiet.fail()
	r2 := &relay{sent: map[string][]string{}}
	dir2 := t.TempDir()
	args2 := []string{"-journal", writeRun(t, dir2, haltedEntries(t)[:3]), "-state", filepath.Join(dir2, "s.json"), "-channel", "ntfy,pagerduty",
		"-units", "ti-adapter-boost.service", "-systemctl", quiet.systemctl(), "-journalctl", quiet.journalctl()}
	if err := run(args2, twoChannels(), func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, &http.Client{Transport: r2}); err != nil {
		t.Fatal(err)
	}
	if n := count(r2.to(ntfyHost), "state at reporter start"); n != 0 {
		t.Errorf("a page was sent with nothing in force: %v", r2.to(ntfyHost))
	}
}

func TestAUnitCheckThatCannotRunIsAFailureNotASilence(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	_ = os.Remove(systemd.systemctl())
	if err := firing(t, r, t.TempDir(), systemd, t0.Add(time.Hour), "ti-adapter-boost.service"); err == nil || !strings.Contains(err.Error(), "systemctl") {
		t.Fatalf("err = %v, want one naming systemctl", err)
	}
}

// TestAReporterFailurePageIsResolvedByTheNextCleanFiring: the reporter cannot
// see its own unit as failed while it runs, so a firing that finishes without
// a fault is what resolves the page its failure sent.
func TestAReporterFailurePageIsResolvedByTheNextCleanFiring(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	systemd.fail()
	dir := t.TempDir()
	const self = "ti-reporter@boost.service"
	if err := notify(t, r, dir, systemd, twoChannels(), self); err != nil {
		t.Fatal(err)
	}
	if err := firing(t, r, dir, systemd, t0.Add(time.Hour), "ti-engine@boost.service", "-self-unit", self); err != nil {
		t.Fatal(err)
	}
	if resolves := count(r.to(pdHost), `"event_action":"resolve"`); resolves != 1 {
		t.Errorf("PagerDuty resolves = %d, want 1", resolves)
	}
	if err := notify(t, r, dir, systemd, twoChannels(), self); err != nil {
		t.Fatal(err)
	}
	if n := pagesFor(r, pdHost, self); n != 2 {
		t.Errorf("PagerDuty took %d reporter pages across two failures, want 2", n)
	}
}
