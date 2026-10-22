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

	"github.com/richard-whittemore/TrendInvesting/internal/report"
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

// standInSystemd is a systemctl whose failed units and current invocation
// the test sets (`systemctl show -p ActiveState -p InvocationID <unit>`), and
// a journalctl that prints, for an invocation, the lines the unit wrote
// (_SYSTEMD_INVOCATION_ID=) or systemd's own lines about it (INVOCATION_ID=),
// and for a query with neither, a line from an earlier run. Both record how
// they were called.
type standInSystemd struct{ dir string }

// firstRun is the stand-in's invocation until a test starts another.
const firstRun = "0123456789abcdef0123456789abcdef"

func newStandInSystemd(t *testing.T) standInSystemd {
	t.Helper()
	s := standInSystemd{dir: t.TempDir()}
	d := "'" + s.dir + "'"
	systemctl := "#!/bin/sh\necho \"$@\" >>" + d + "/systemctl-calls\nfor a; do unit=$a; done\n" +
		"case $1 in show) ;; *) exit 2 ;; esac\n" +
		"if grep -qx \"$unit\" " + d + "/failed; then echo ActiveState=failed; else echo ActiveState=active; fi\n" +
		"echo InvocationID=$(cat " + d + "/invocation)\n"
	journalctl := "#!/bin/sh\necho \"$@\" >>" + d + "/journalctl-calls\nf=unit-wide\n" +
		"for a; do case $a in _SYSTEMD_INVOCATION_ID=*) f=own-${a#*=} ;; INVOCATION_ID=*) f=systemd-${a#*=} ;; esac; done\n" +
		"cat " + d + "/$f 2>/dev/null\nexit 0\n"
	for name, script := range map[string]string{"systemctl": systemctl, "journalctl": journalctl} {
		if err := os.WriteFile(filepath.Join(s.dir, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	s.invoke(firstRun)
	s.fail()
	s.say("adapter stopping: ReconcileIncomplete: IBKR's open-order stream never paused")
	return s
}

func (s standInSystemd) systemctl() string  { return filepath.Join(s.dir, "systemctl") }
func (s standInSystemd) journalctl() string { return filepath.Join(s.dir, "journalctl") }

// invoke makes id the units' current invocation, as a start does.
func (s standInSystemd) invoke(id string) {
	_ = os.WriteFile(filepath.Join(s.dir, "invocation"), []byte(id+"\n"), 0o600)
}
func (s standInSystemd) current() string {
	raw, _ := os.ReadFile(filepath.Join(s.dir, "invocation"))
	return strings.TrimSpace(string(raw))
}

// say is a line the current invocation wrote itself.
func (s standInSystemd) say(line string) {
	_ = os.WriteFile(filepath.Join(s.dir, "own-"+s.current()), []byte(line+"\n"), 0o600)
}

// systemdSays are systemd's own lines about the current invocation.
func (s standInSystemd) systemdSays(lines ...string) {
	_ = os.WriteFile(filepath.Join(s.dir, "systemd-"+s.current()), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

// earlierRunSaid is a line the unit wrote in an earlier run: what a query by
// the unit alone, with no invocation, would answer.
func (s standInSystemd) earlierRunSaid(line string) {
	_ = os.WriteFile(filepath.Join(s.dir, "unit-wide"), []byte(line+"\n"), 0o600)
}
func (s standInSystemd) fail(units ...string) {
	_ = os.WriteFile(filepath.Join(s.dir, "failed"), []byte(strings.Join(units, "\n")+"\n"), 0o600)
}
func (s standInSystemd) calls() string {
	raw, _ := os.ReadFile(filepath.Join(s.dir, "journalctl-calls"))
	return string(raw)
}
func (s standInSystemd) systemctlCalls() string {
	raw, _ := os.ReadFile(filepath.Join(s.dir, "systemctl-calls"))
	return string(raw)
}

// pagesFor counts the messages on host that are the page for unit.
func pagesFor(r *relay, host, unit string) int { return count(r.to(host), unit+" failed") }

func notify(t *testing.T, transport http.RoundTripper, dir string, systemd standInSystemd, getenv func(string) string, unit string, extra ...string) error {
	t.Helper()
	args := append([]string{"notify-unit-failed", "-state", filepath.Join(dir, "unit-failures.json"), "-channel", "ntfy,pagerduty", "-journalctl", systemd.journalctl(), "-systemctl", systemd.systemctl()}, extra...)
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
	if !strings.Contains(systemd.calls(), "_SYSTEMD_INVOCATION_ID="+firstRun) {
		t.Errorf("the reason must be the failed run's own line: journalctl %s", systemd.calls())
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
	for _, unit := range []string{"", "--help", "-n1", "a b.service", "../etc/passwd", "ti-engine@boost.service;rm", "sshd.service", "ti-unit-failed@ti-engine@boost.service.service", "ti-engine@Boost.service", "ti-engine@boost"} {
		r := &relay{sent: map[string][]string{}}
		systemd := newStandInSystemd(t)
		if err := notify(t, r, t.TempDir(), systemd, twoChannels(), unit); err == nil {
			t.Errorf("%q: accepted", unit)
		}
		if len(r.to(ntfyHost))+len(r.to(pdHost)) != 0 || systemd.calls() != "" || systemd.systemctlCalls() != "" {
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

const secondRun = "fedcba9876543210fedcba9876543210"

// pdEvents are PagerDuty's events in order, each its action and dedup key.
func pdEvents(r *relay) []string {
	var out []string
	for _, m := range r.to(pdHost) {
		action, key := "", ""
		if _, rest, ok := strings.Cut(m, `"event_action":"`); ok {
			action, _, _ = strings.Cut(rest, `"`)
		}
		if _, rest, ok := strings.Cut(m, `"dedup_key":"`); ok {
			key, _, _ = strings.Cut(rest, `"`)
		}
		out = append(out, action+" "+key)
	}
	return out
}

// TestAUnitThatFailsAgainBeforeAnyFiringIsPagedAgain: the page is keyed by
// the failed run (systemd's invocation), not the unit alone, so a unit
// restarted and failed again before a reporter firing saw it running pages
// again, and the earlier incident is resolved first (ADR 0019, "Alerting":
// each incident reaches a person).
func TestAUnitThatFailsAgainBeforeAnyFiringIsPagedAgain(t *testing.T) {
	t.Run("the hook reads the failed run from systemctl", func(t *testing.T) {
		r := &relay{sent: map[string][]string{}}
		systemd := newStandInSystemd(t)
		dir := t.TempDir()
		const unit = "ti-adapter-boost.service"
		if err := notify(t, r, dir, systemd, twoChannels(), unit); err != nil {
			t.Fatal(err)
		}
		systemd.invoke(secondRun)
		if err := notify(t, r, dir, systemd, twoChannels(), unit); err != nil {
			t.Fatal(err)
		}
		for _, host := range []string{ntfyHost, pdHost} {
			if n := pagesFor(r, host, unit); n != 2 {
				t.Errorf("%s took %d pages for two failed runs, want 2", host, n)
			}
		}
		events := pdEvents(r)
		want := []string{"trigger " + report.UnitFailedKey(unit, firstRun), "resolve " + report.UnitFailedKey(unit, firstRun), "trigger " + report.UnitFailedKey(unit, secondRun)}
		if strings.Join(events, "\n") != strings.Join(want, "\n") {
			t.Errorf("PagerDuty events:\n%s\nwant:\n%s", strings.Join(events, "\n"), strings.Join(want, "\n"))
		}
	})
	t.Run("the hook takes the failed run systemd hands it", func(t *testing.T) {
		r := &relay{sent: map[string][]string{}}
		systemd := newStandInSystemd(t)
		systemd.invoke(secondRun) // already restarted when the hook reads systemctl
		monitor := func(k string) string {
			switch k {
			case "MONITOR_UNIT":
				return "ti-engine@boost.service"
			case "MONITOR_INVOCATION_ID":
				return firstRun
			}
			return twoChannels()(k)
		}
		if err := notify(t, r, t.TempDir(), systemd, monitor, "ti-engine@boost.service"); err != nil {
			t.Fatal(err)
		}
		if got := pdEvents(r); len(got) != 1 || got[0] != "trigger "+report.UnitFailedKey("ti-engine@boost.service", firstRun) {
			t.Errorf("PagerDuty events %v, want one trigger keyed by the run systemd named", got)
		}
	})
	t.Run("a firing pages a failed run the hook never saw", func(t *testing.T) {
		// A start refused while the unit is already failed does not start
		// the OnFailure= hook (systemd 255), so the unit stays failed under a
		// new invocation and only the firing can tell.
		r := &relay{sent: map[string][]string{}}
		systemd := newStandInSystemd(t)
		systemd.fail("ti-adapter-boost.service")
		dir := t.TempDir()
		if err := notify(t, r, dir, systemd, twoChannels(), "ti-adapter-boost.service"); err != nil {
			t.Fatal(err)
		}
		systemd.invoke(secondRun)
		if err := firing(t, r, dir, systemd, t0.Add(time.Hour+5*time.Minute), "ti-adapter-boost.service"); err != nil {
			t.Fatal(err)
		}
		for _, host := range []string{ntfyHost, pdHost} {
			if n := pagesFor(r, host, "ti-adapter-boost.service"); n != 2 {
				t.Errorf("%s took %d pages for two failed runs, want 2", host, n)
			}
		}
		if n := count(pdEvents(r), "resolve "+report.UnitFailedKey("ti-adapter-boost.service", firstRun)); n != 1 {
			t.Errorf("the first run's incident was resolved %d times, want 1: %v", n, pdEvents(r))
		}
	})
}

// midFiring runs once, before the first request of a firing reaches the
// channels: a unit that fails while the firing delivers.
type midFiring struct {
	*relay
	once sync.Once
	then func()
}

func (m *midFiring) RoundTrip(req *http.Request) (*http.Response, error) {
	m.once.Do(m.then)
	return m.relay.RoundTrip(req)
}

// TestAPageSentDuringAFiringIsNotResolvedByIt: the firing samples systemd
// once, at its start; a unit that fails (and is paged by the hook) while the
// firing delivers is newer than that sample, so the firing leaves its page
// open (ADR 0019, "Alerting").
func TestAPageSentDuringAFiringIsNotResolvedByIt(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	systemd.fail()
	dir := t.TempDir()
	const unit = "ti-engine@boost.service"
	m := &midFiring{relay: r, then: func() {
		systemd.invoke(secondRun)
		systemd.fail(unit)
		if err := notify(t, r, dir, systemd, twoChannels(), unit); err != nil {
			t.Error(err)
		}
	}}
	if err := firing(t, m, dir, systemd, t0.Add(time.Hour), unit); err != nil {
		t.Fatal(err)
	}
	if n := pagesFor(r, pdHost, unit); n != 1 {
		t.Fatalf("the hook's page did not go: %d", n)
	}
	if n := count(r.to(pdHost), `"event_action":"resolve"`); n != 0 {
		t.Errorf("the firing resolved a page newer than its sample: %v", pdEvents(r))
	}
	if err := firing(t, r, dir, systemd, t0.Add(time.Hour+5*time.Minute), unit); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, unit); n != 1 {
			t.Errorf("%s took %d pages for one failed run, want 1: the record was forgotten", host, n)
		}
	}
}

// TestTheReasonIsTheFailedRunsNeverAnEarlierOnes: the page's reason is the
// failed invocation's own last line; a run that wrote nothing (a start
// refused for a missing run.env) is explained by systemd's own line about that
// run, never by a line from an earlier run.
func TestTheReasonIsTheFailedRunsNeverAnEarlierOnes(t *testing.T) {
	const unit = "ti-engine@boost.service"
	const stale = "engine stopping cleanly at the session close"
	reasonOf := func(t *testing.T, systemd standInSystemd) string {
		t.Helper()
		r := &relay{sent: map[string][]string{}}
		if err := notify(t, r, t.TempDir(), systemd, twoChannels(), unit); err != nil {
			t.Fatal(err)
		}
		return strings.Join(r.to(ntfyHost), "\n")
	}
	t.Run("the run wrote its own line", func(t *testing.T) {
		systemd := newStandInSystemd(t)
		systemd.earlierRunSaid(stale)
		systemd.invoke(secondRun)
		systemd.say("engine stopping: the durable log's tail is torn")
		got := reasonOf(t, systemd)
		if !strings.Contains(got, "the durable log's tail is torn") || strings.Contains(got, stale) {
			t.Errorf("the page's reason is not the failed run's own line:\n%s", got)
		}
	})
	t.Run("the run wrote nothing: systemd's line about it", func(t *testing.T) {
		systemd := newStandInSystemd(t)
		systemd.earlierRunSaid(stale)
		systemd.invoke(secondRun)
		systemd.systemdSays(
			"ti-engine@boost.service: Failed to load environment files: No such file or directory",
			"ti-engine@boost.service: Failed to spawn 'start' task: No such file or directory",
			"ti-engine@boost.service: Failed with result 'resources'.",
			"Failed to start ti-engine@boost.service.",
		)
		got := reasonOf(t, systemd)
		if !strings.Contains(got, "Failed to load environment files") || strings.Contains(got, stale) {
			t.Errorf("the page's reason is not systemd's line about the failed run:\n%s", got)
		}
	})
	t.Run("nothing at all about the run", func(t *testing.T) {
		systemd := newStandInSystemd(t)
		systemd.earlierRunSaid(stale)
		systemd.invoke(secondRun)
		got := reasonOf(t, systemd)
		if strings.Contains(got, stale) || !strings.Contains(got, "wrote no journal line") {
			t.Errorf("a run that left no line is paged with an earlier run's:\n%s", got)
		}
	})
}

// TestEveryUnitNameIsCheckedBeforeSystemctlOrJournalctlRuns: -units and
// -self-unit are refused unless every name is a trading unit's, so no flag
// value reaches systemctl or journalctl as an option or another unit.
func TestEveryUnitNameIsCheckedBeforeSystemctlOrJournalctlRuns(t *testing.T) {
	for _, tc := range []struct{ units, self string }{
		{"--help", ""},
		{"ti-engine@boost.service,-H evil", ""},
		{"sshd.service", ""},
		{"ti-engine@boost.service", "--now"},
		{"ti-engine@boost.service", "ti-gateway.service"},
	} {
		r := &relay{sent: map[string][]string{}}
		systemd := newStandInSystemd(t)
		var extra []string
		if tc.self != "" {
			extra = []string{"-self-unit", tc.self}
		}
		err := firing(t, r, t.TempDir(), systemd, t0.Add(time.Hour), tc.units, extra...)
		if err == nil || !strings.Contains(err.Error(), "unit") {
			t.Errorf("-units %q -self-unit %q: err = %v, want a refusal", tc.units, tc.self, err)
		}
		if systemd.systemctlCalls() != "" || systemd.calls() != "" {
			t.Errorf("-units %q -self-unit %q: systemctl or journalctl ran: %q %q", tc.units, tc.self, systemd.systemctlCalls(), systemd.calls())
		}
	}
}
