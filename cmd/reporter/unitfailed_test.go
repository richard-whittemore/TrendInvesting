package main

import (
	"bytes"
	"errors"
	"fmt"
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
// and for a query with neither, a line from an earlier run. Like the real
// tool, the journalctl notes "-- No entries --" for a query that matched
// nothing unless it is given -q, and a noisy one notes it even then. Both
// record how they were called.
type standInSystemd struct{ dir string }

// firstRun is the stand-in's invocation until a test starts another.
const firstRun = "0123456789abcdef0123456789abcdef"

func newStandInSystemd(t *testing.T) standInSystemd {
	t.Helper()
	s := standInSystemd{dir: t.TempDir()}
	d := "'" + s.dir + "'"
	systemctl := "#!/bin/sh\necho \"$@\" >>" + d + "/systemctl-calls\nfor a; do unit=$a; done\n" +
		"case $1 in show) ;; *) exit 2 ;; esac\n" +
		"if grep -qx \"$unit\" " + d + "/broken 2>/dev/null; then exit 1; fi\n" +
		"if grep -qx \"$unit\" " + d + "/failed; then echo ActiveState=failed; elif grep -q \"^$unit=\" " + d + "/states; then grep \"^$unit=\" " + d + "/states | sed 's/^[^=]*=/ActiveState=/'; else echo ActiveState=active; fi\n" +
		"echo InvocationID=$(cat " + d + "/invocation)\n"
	journalctl := "#!/bin/sh\necho \"$@\" >>" + d + "/journalctl-calls\nf=unit-wide\nq=\n" +
		"for a; do case $a in -q) q=1 ;; _SYSTEMD_INVOCATION_ID=*) f=own-${a#*=} ;; INVOCATION_ID=*) f=systemd-${a#*=} ;; esac; done\n" +
		"if test -e " + d + "/$f; then cat " + d + "/$f\nelif test -z \"$q\" || test -e " + d + "/noisy; then echo '-- No entries --'; fi\nexit 0\n"
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

// noisy makes the journalctl note "-- No entries --" even when given -q.
func (s standInSystemd) noisy() { _ = os.WriteFile(filepath.Join(s.dir, "noisy"), nil, 0o600) }

func (s standInSystemd) fail(units ...string) {
	_ = os.WriteFile(filepath.Join(s.dir, "failed"), []byte(strings.Join(units, "\n")+"\n"), 0o600)
}

// state sets unit's ActiveState when it is not failed (activating while a run is in flight, inactive once it ended well).
func (s standInSystemd) state(unit, state string) {
	_ = os.WriteFile(filepath.Join(s.dir, "states"), []byte(unit+"="+state+"\n"), 0o600)
}

// breaks makes `systemctl show` fail for unit, as a timeout would.
func (s standInSystemd) breaks(unit string) {
	_ = os.WriteFile(filepath.Join(s.dir, "broken"), []byte(unit+"\n"), 0o600)
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
	return notifyAt(t, transport, dir, systemd, getenv, t0.Add(time.Hour), unit, extra...)
}

func notifyAt(t *testing.T, transport http.RoundTripper, dir string, systemd standInSystemd, getenv func(string) string, at time.Time, unit string, extra ...string) error {
	t.Helper()
	args := append([]string{"notify-unit-failed", "-state", filepath.Join(dir, "unit-failures.json"), "-channel", "ntfy,pagerduty", "-journalctl", systemd.journalctl(), "-systemctl", systemd.systemctl()}, extra...)
	args = append(args, unit)
	return run(args, getenv, func() time.Time { return at }, &bytes.Buffer{}, &http.Client{Transport: transport})
}

// monitored is the hook's environment as systemd gives it for the failed run
// invocation of unit (MONITOR_UNIT, MONITOR_INVOCATION_ID).
func monitored(unit, invocation string) func(string) string {
	return func(k string) string {
		switch k {
		case "MONITOR_UNIT":
			return unit
		case "MONITOR_INVOCATION_ID":
			return invocation
		}
		return twoChannels()(k)
	}
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

func TestTheHookPagesAFailedDataJobOnceWithItsManualRow(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		if err := notify(t, r, dir, systemd, twoChannels(), "ti-data.service"); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, "ti-data.service"); n != 1 {
			t.Errorf("%s took %d pages for the data job, want 1", host, n)
		}
		if got := strings.Join(r.to(host), "\n"); !strings.Contains(got, "The data job fails") {
			t.Errorf("%s: the page does not give the data job's manual row:\n%s", host, got)
		}
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
	failBody string // when set, only a request whose body holds it is refused
	healed   bool
}

func (f *flakyRelay) setFailing(on bool) { f.mu.Lock(); f.healed = !on; f.mu.Unlock() }

func (f *flakyRelay) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	down := req.URL.Host == f.failHost && !f.healed
	f.mu.Unlock()
	if down && f.failBody != "" {
		raw, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(raw))
		down = strings.Contains(string(raw), f.failBody)
	}
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

// TestAUnitThatFailsAgainWithinOneEpisodeIsNotPagedAgain: a unit restarted
// that fails again before any reporter firing saw it well is the same failure
// episode, so it is not paged again and its incident is not resolved and
// triggered again (ADR 0019, "Alerting": each incident once).
func TestAUnitThatFailsAgainWithinOneEpisodeIsNotPagedAgain(t *testing.T) {
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
			if n := pagesFor(r, host, unit); n != 1 {
				t.Errorf("%s took %d pages for one episode, want 1", host, n)
			}
		}
		if got := pdEvents(r); strings.Join(got, "\n") != "trigger "+report.UnitFailedKey(unit, firstRun) {
			t.Errorf("PagerDuty events %v, want the one trigger of the episode's first run", got)
		}
	})
	t.Run("the hook takes the failed run systemd hands it", func(t *testing.T) {
		r := &relay{sent: map[string][]string{}}
		systemd := newStandInSystemd(t)
		systemd.invoke(secondRun) // already restarted when the hook reads systemctl
		if err := notify(t, r, t.TempDir(), systemd, monitored("ti-engine@boost.service", firstRun), "ti-engine@boost.service"); err != nil {
			t.Fatal(err)
		}
		if got := pdEvents(r); len(got) != 1 || got[0] != "trigger "+report.UnitFailedKey("ti-engine@boost.service", firstRun) {
			t.Errorf("PagerDuty events %v, want one trigger keyed by the run systemd named", got)
		}
	})
	t.Run("a firing that finds a new failed run of a paged unit", func(t *testing.T) {
		// A start refused while the unit is already failed does not start
		// the OnFailure= hook (systemd 255), so the unit stays failed under a
		// new invocation: still the same episode.
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
			if n := pagesFor(r, host, "ti-adapter-boost.service"); n != 1 {
				t.Errorf("%s took %d pages for one episode, want 1", host, n)
			}
		}
		if n := count(pdEvents(r), "resolve"); n != 0 {
			t.Errorf("the episode's incident was resolved within it: %v", pdEvents(r))
		}
	})
}

// TestAnEpisodeSeenWellIsOverEvenWhenItsResolveFailed: a unit seen well ends
// its episode though PagerDuty could not be told, so the next failed run pages
// afresh and the old incident is resolved again (ADR 0019, "Alerting").
func TestAnEpisodeSeenWellIsOverEvenWhenItsResolveFailed(t *testing.T) {
	flaky := &flakyRelay{relay: &relay{sent: map[string][]string{}}, failHost: pdHost, healed: true}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	const unit = "ti-engine@boost.service"
	start := t0.Add(time.Hour)
	if err := notifyAt(t, flaky, dir, systemd, monitored(unit, firstRun), start, unit); err != nil {
		t.Fatal(err)
	}
	flaky.setFailing(true)
	systemd.fail()
	if err := firing(t, flaky, dir, systemd, start.Add(5*time.Minute), unit); err == nil {
		t.Fatal("a resolve PagerDuty refused must fail the firing")
	}
	flaky.setFailing(false)
	systemd.invoke(secondRun)
	systemd.fail(unit)
	if err := notifyAt(t, flaky, dir, systemd, monitored(unit, secondRun), start.Add(10*time.Minute), unit); err != nil {
		t.Fatal(err)
	}
	if n := pagesFor(flaky.relay, ntfyHost, unit); n != 2 {
		t.Errorf("ntfy took %d pages across two episodes, want 2", n)
	}
	if n := count(pdEvents(flaky.relay), "trigger "+report.UnitFailedKey(unit, secondRun)); n != 1 {
		t.Errorf("PagerDuty was not paged for the new episode: %v", pdEvents(flaky.relay))
	}
	if n := count(pdEvents(flaky.relay), "resolve "+report.UnitFailedKey(unit, firstRun)); n != 1 {
		t.Errorf("the old incident was not resolved once it could be: %v", pdEvents(flaky.relay))
	}
}

// TestAReporterThatFailsEveryFiringPagesOnceAndRemindsHourly: every timer
// firing is a new systemd run, so a reporter that fails on every firing (an
// engine not yet started, a channel set aside, a channel down) is one
// failure episode: one page, then at most one reminder an hour naming when
// it began and how many runs failed, never a page per firing (ADR 0019,
// "Alerting": each incident once).
func TestAReporterThatFailsEveryFiringPagesOnceAndRemindsHourly(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	systemd.fail()
	dir := t.TempDir()
	const self = "ti-reporter@boost.service"
	start := t0.Add(time.Hour)
	first := fmt.Sprintf("%032x", 1)
	for i := 0; i < 14; i++ { // 65 minutes of firings, each one failing
		if err := notifyAt(t, r, dir, systemd, monitored(self, fmt.Sprintf("%032x", i+1)), start.Add(time.Duration(i)*5*time.Minute), self); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, self); n != 1 {
			t.Errorf("%s took %d pages for one episode, want 1", host, n)
		}
		reminders := 0
		for _, m := range r.to(host) {
			if strings.Contains(m, self+" still failing") {
				reminders++
				for _, want := range []string{"since " + start.UTC().Format(time.RFC3339), "13 failed runs"} {
					if !strings.Contains(m, want) {
						t.Errorf("%s: the reminder lacks %q:\n%s", host, want, m)
					}
				}
			}
		}
		if reminders != 1 {
			t.Errorf("%s took %d reminders in 65 minutes, want 1", host, reminders)
		}
	}
	for _, e := range pdEvents(r) {
		if e != "trigger "+report.UnitFailedKey(self, first) {
			t.Errorf("PagerDuty event %q: the episode's incident must be the only one, never resolved within it (%v)", e, pdEvents(r))
		}
	}
}

// TestAFailureAfterTheUnitWasSeenWellPagesAgain: an episode ends when a
// firing sees the unit well (for the reporter, a firing that finishes without
// a fault), so the next failure is a new incident and pages again.
func TestAFailureAfterTheUnitWasSeenWellPagesAgain(t *testing.T) {
	start := t0.Add(time.Hour)
	t.Run("the engine", func(t *testing.T) {
		r := &relay{sent: map[string][]string{}}
		systemd := newStandInSystemd(t)
		dir := t.TempDir()
		const unit = "ti-engine@boost.service"
		if err := notifyAt(t, r, dir, systemd, monitored(unit, firstRun), start, unit); err != nil {
			t.Fatal(err)
		}
		systemd.fail()
		if err := firing(t, r, dir, systemd, start.Add(5*time.Minute), unit); err != nil {
			t.Fatal(err)
		}
		systemd.invoke(secondRun)
		systemd.fail(unit)
		if err := notifyAt(t, r, dir, systemd, monitored(unit, secondRun), start.Add(10*time.Minute), unit); err != nil {
			t.Fatal(err)
		}
		want := []string{"trigger " + report.UnitFailedKey(unit, firstRun), "resolve " + report.UnitFailedKey(unit, firstRun), "trigger " + report.UnitFailedKey(unit, secondRun)}
		var got []string
		for _, e := range pdEvents(r) {
			if strings.Contains(e, report.UnitFailedKeyPrefix(unit)) {
				got = append(got, e)
			}
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("PagerDuty events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if n := pagesFor(r, ntfyHost, unit); n != 2 {
			t.Errorf("ntfy took %d pages across two episodes, want 2", n)
		}
	})
	t.Run("the reporter", func(t *testing.T) {
		r := &relay{sent: map[string][]string{}}
		systemd := newStandInSystemd(t)
		systemd.fail()
		dir := t.TempDir()
		const self = "ti-reporter@boost.service"
		if err := notifyAt(t, r, dir, systemd, monitored(self, firstRun), start, self); err != nil {
			t.Fatal(err)
		}
		if err := firing(t, r, dir, systemd, start.Add(5*time.Minute), "ti-engine@boost.service", "-self-unit", self); err != nil {
			t.Fatal(err)
		}
		if err := notifyAt(t, r, dir, systemd, monitored(self, secondRun), start.Add(10*time.Minute), self); err != nil {
			t.Fatal(err)
		}
		for _, host := range []string{ntfyHost, pdHost} {
			if n := pagesFor(r, host, self); n != 2 {
				t.Errorf("%s took %d reporter pages across two episodes, want 2", host, n)
			}
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
	t.Run("a journalctl that notes it found no entries", func(t *testing.T) {
		systemd := newStandInSystemd(t)
		systemd.noisy()
		systemd.invoke(secondRun)
		systemd.systemdSays("ti-engine@boost.service: Failed with result 'exit-code'.")
		got := reasonOf(t, systemd)
		if strings.Contains(got, "No entries") || !strings.Contains(got, "Failed with result 'exit-code'") {
			t.Errorf("journalctl's own notice was taken for the run's line:\n%s", got)
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
		{"ti-engine@boost.service", "ti-ntfy.service"},
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

// TestAFailedRunThatJoinsAnEpisodeDuringAFiringIsNotResolvedByIt: the firing
// samples the unit well, then, before it reaches the unit-failure record, the
// unit fails again within the open episode and the hook records that run. The
// firing ends no episode with a failed run recorded at or after its sample,
// the same rule as for a page, so the incident stays open while the unit is
// failed (ADR 0019, "Alerting").
func TestAFailedRunThatJoinsAnEpisodeDuringAFiringIsNotResolvedByIt(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	const unit = "ti-engine@boost.service"
	start := t0.Add(time.Hour)
	if err := notifyAt(t, r, dir, systemd, monitored(unit, firstRun), start, unit); err != nil {
		t.Fatal(err)
	}
	systemd.fail() // the owner restarted it: the firing samples it well
	m := &midFiring{relay: r, then: func() {
		systemd.invoke(secondRun)
		systemd.fail(unit)
		if err := notifyAt(t, r, dir, systemd, monitored(unit, secondRun), start.Add(5*time.Minute), unit); err != nil {
			t.Error(err)
		}
	}}
	if err := firing(t, m, dir, systemd, start.Add(5*time.Minute), unit); err != nil {
		t.Fatal(err)
	}
	if n := count(pdEvents(r), "resolve"); n != 0 {
		t.Errorf("the firing resolved an episode a run joined after its sample: %v", pdEvents(r))
	}
	if err := firing(t, r, dir, systemd, start.Add(10*time.Minute), unit); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, unit); n != 1 {
			t.Errorf("%s took %d pages for one episode, want 1", host, n)
		}
	}
	if n := count(pdEvents(r), "resolve"); n != 0 {
		t.Errorf("the episode's incident was resolved within it: %v", pdEvents(r))
	}
}

// TestAnUnnamedFailedRunIsOneEpisode: a failed unit systemd names no
// invocation for, found failed by every firing, is one episode: one page,
// then a reminder an hour, never resolved and paged again, even after an
// earlier episode whose resolve failed (ADR 0019, "Alerting": each incident
// once).
func TestAnUnnamedFailedRunIsOneEpisode(t *testing.T) {
	flaky := &flakyRelay{relay: &relay{sent: map[string][]string{}}, failHost: pdHost, healed: true}
	systemd := newStandInSystemd(t)
	systemd.invoke("")
	const unit = "ti-engine@boost.service"
	dir := t.TempDir()
	start := t0.Add(time.Hour)
	systemd.fail(unit)
	if err := firing(t, flaky, dir, systemd, start, unit); err != nil {
		t.Fatal(err)
	}
	flaky.setFailing(true)
	systemd.fail()
	_ = firing(t, flaky, dir, systemd, start.Add(5*time.Minute), unit) // the resolve fails
	flaky.setFailing(false)
	systemd.fail(unit)
	second := start.Add(10 * time.Minute)
	for i := 0; i < 14; i++ { // 65 minutes of firings that find it failed
		_ = firing(t, flaky, dir, systemd, second.Add(time.Duration(i)*5*time.Minute), unit)
	}
	if n := pagesFor(flaky.relay, ntfyHost, unit); n != 2 {
		t.Errorf("ntfy took %d pages across two episodes, want 2", n)
	}
	var events []string
	for _, e := range pdEvents(flaky.relay) {
		if strings.Contains(e, report.UnitFailedKeyPrefix(unit)) {
			events = append(events, e)
		}
	}
	if count(events, "trigger") != 3 || count(events, "resolve") != 1 { // two pages and one reminder
		t.Errorf("PagerDuty events %v: want the old incident resolved once and the new one never", events)
	}
	for _, host := range []string{ntfyHost, pdHost} {
		reminders := 0
		for _, m := range flaky.to(host) {
			if strings.Contains(m, unit+" still failing") {
				reminders++
				if !strings.Contains(m, "since "+second.UTC().Format(time.RFC3339)) {
					t.Errorf("%s: the reminder does not name the episode's start:\n%s", host, m)
				}
			}
		}
		if reminders != 1 {
			t.Errorf("%s took %d reminders in 65 minutes, want 1", host, reminders)
		}
	}
}

// TestAnEpisodesBookkeepingIsBoundedAndGoneWhenItEnds: an episode of many
// failed runs keeps a bounded record, and once it ends (even through a
// resolve that failed first) nothing of it is left in the record.
func TestAnEpisodesBookkeepingIsBoundedAndGoneWhenItEnds(t *testing.T) {
	flaky := &flakyRelay{relay: &relay{sent: map[string][]string{}}, failHost: pdHost, healed: true}
	systemd := newStandInSystemd(t)
	systemd.fail()
	dir := t.TempDir()
	const self = "ti-reporter@boost.service"
	start := t0.Add(time.Hour)
	record := func() int {
		t.Helper()
		seen, sent, repeats, _, err := loadState(filepath.Join(dir, "unit-failures.json"))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for k := range seen {
			n += strings.Count(k, report.UnitFailedKeyPrefix(self))
		}
		for k := range sent {
			n += strings.Count(k, report.UnitFailedKeyPrefix(self))
		}
		for k := range repeats {
			n += strings.Count(k, report.UnitFailedKeyPrefix(self))
		}
		return n
	}
	for i := 0; i < 15; i++ { // past the first reminder

		_ = notifyAt(t, flaky, dir, systemd, monitored(self, fmt.Sprintf("%032x", i+1)), start.Add(time.Duration(i)*5*time.Minute), self)
	}
	before := record()
	for i := 15; i < 50; i++ {
		_ = notifyAt(t, flaky, dir, systemd, monitored(self, fmt.Sprintf("%032x", i+1)), start.Add(time.Duration(i)*5*time.Minute), self)
	}
	if n := record(); n > before {
		t.Errorf("the record grew from %d to %d entries over 35 more failed runs of one episode", before, n)
	}
	flaky.setFailing(true)
	_ = firing(t, flaky, dir, systemd, start.Add(5*time.Hour), "ti-engine@boost.service", "-self-unit", self)
	flaky.setFailing(false)
	_ = firing(t, flaky, dir, systemd, start.Add(5*time.Hour+5*time.Minute), "ti-engine@boost.service", "-self-unit", self)
	if n := record(); n != 0 {
		t.Errorf("the ended episode left %d entries in the record", n)
	}
}

// TestAStaleSampleOfAFailedRunIsNotCountedTwice: a firing samples a unit failed
// as run R2, whose own hook counted it; a later run R3 is recorded (the hook
// keeps only the latest later run) before the firing's unit step, so the
// firing's sample of R2 is older than R3 and must not count R2 again (ADR
// 0019, "Alerting": each incident once).
func TestAStaleSampleOfAFailedRunIsNotCountedTwice(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	const unit = "ti-engine@boost.service"
	path := filepath.Join(dir, "unit-failures.json")
	base := t0.Add(time.Hour)
	thirdRun := fmt.Sprintf("%032x", 3)
	if err := notifyAt(t, r, dir, systemd, monitored(unit, firstRun), base, unit); err != nil {
		t.Fatal(err)
	}
	if err := notifyAt(t, r, dir, systemd, monitored(unit, secondRun), base.Add(10*time.Minute), unit); err != nil {
		t.Fatal(err)
	}
	if err := notifyAt(t, r, dir, systemd, monitored(unit, thirdRun), base.Add(61*time.Minute), unit); err != nil {
		t.Fatal(err)
	}
	senders, setAside, err := buildSenders("ntfy,pagerduty", twoChannels(), &http.Client{Transport: r}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	sampled := map[string]unitSample{unit: {failed: true, invocation: secondRun}}
	if _, err := unitStep(path, []string{unit}, nil, sampled, base.Add(60*time.Minute), systemd.journalctl(), nil, senders, setAside, base.Add(60*time.Minute), time.Hour, nil); err != nil {
		t.Fatal(err)
	}
	_, _, repeats, _, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := repeats[runsKey(report.UnitFailedKey(unit, firstRun))], 2; got != want {
		t.Errorf("the episode counted %d later failed runs, want %d (R2 and R3 once each)", got, want)
	}
}

// TestAUnitThatFailsOnceAndStaysFailedIsRemindedHourly: a unit that fails one
// run and is never restarted is sampled under the run that paged, at every
// firing, and is still reminded once an hour, with no run count since only one
// run is known (ADR 0019, "Alerting": each incident once, reminded while the
// fault lasts).
func TestAUnitThatFailsOnceAndStaysFailedIsRemindedHourly(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	const unit = "ti-engine@boost.service"
	dir := t.TempDir()
	start := t0.Add(time.Hour)
	if err := notifyAt(t, r, dir, systemd, monitored(unit, firstRun), start, unit); err != nil {
		t.Fatal(err)
	}
	systemd.fail(unit)
	for i := 1; i <= 36; i++ { // three hours of firings that find it still failed
		if err := firing(t, r, dir, systemd, start.Add(time.Duration(i)*5*time.Minute), unit); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, unit); n != 1 {
			t.Errorf("%s took %d pages for one episode, want 1", host, n)
		}
		reminders := 0
		for _, m := range r.to(host) {
			if strings.Contains(m, unit+" still failing") {
				reminders++
				if !strings.Contains(m, "still failing since "+start.UTC().Format(time.RFC3339)+",") || strings.Contains(m, "failed runs") {
					t.Errorf("%s: the reminder must name the start and no count:\n%s", host, m)
				}
			}
		}
		if reminders != 3 {
			t.Errorf("%s took %d reminders in 3 hours, want 3", host, reminders)
		}
	}
}

// The data job is paged once by the OnFailure hook and is not a unit the
// reporter holds the heartbeat or the reminders for; the reporter samples it
// only to end its episode once a run has ended well (ADR 0019, "Alerting").
const dataJob = "ti-data.service"

func TestAFailedDataJobIsNeitherRemindedNorPagedAgainByItsRetriesOrTheReporter(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	systemd.invoke(firstRun)
	systemd.fail(dataJob)
	if err := notifyAt(t, r, dir, systemd, monitored(dataJob, firstRun), t0.Add(time.Hour), dataJob); err != nil {
		t.Fatal(err)
	}
	// The retry starts: activating, not well; the reporter fires meanwhile.
	systemd.fail()
	systemd.state(dataJob, "activating")
	if err := firing(t, r, dir, systemd, t0.Add(2*time.Hour), "ti-adapter-boost.service", "-resolve-units", dataJob); err != nil {
		t.Fatal(err)
	}
	// The retry fails too, two hours later.
	second := "fedcba9876543210fedcba9876543210"
	systemd.invoke(second)
	systemd.fail(dataJob)
	if err := notifyAt(t, r, dir, systemd, monitored(dataJob, second), t0.Add(3*time.Hour), dataJob); err != nil {
		t.Fatal(err)
	}
	if err := firing(t, r, dir, systemd, t0.Add(4*time.Hour), "ti-adapter-boost.service", "-resolve-units", dataJob); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, dataJob); n != 1 {
			t.Errorf("%s took %d pages for the data job across a failed retry, want 1", host, n)
		}
		if got := strings.Join(r.to(host), "\n"); strings.Contains(got, "still failing") {
			t.Errorf("%s was reminded of the data job:\n%s", host, got)
		}
	}
	if n := count(r.to(pdHost), `"event_action":"resolve"`); n != 0 {
		t.Errorf("an episode ended while the data job was failed or retrying: %d resolves", n)
	}
}

func TestADataJobRunThatEndedWellEndsItsEpisodeAndTheNextFailureIsTold(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	systemd.invoke(firstRun)
	systemd.fail(dataJob)
	if err := notifyAt(t, r, dir, systemd, monitored(dataJob, firstRun), t0.Add(time.Hour), dataJob); err != nil {
		t.Fatal(err)
	}
	systemd.fail()
	systemd.state(dataJob, "inactive")
	if err := firing(t, r, dir, systemd, t0.Add(2*time.Hour), "ti-adapter-boost.service", "-resolve-units", dataJob); err != nil {
		t.Fatal(err)
	}
	if n := count(r.to(pdHost), `"event_action":"resolve"`); n != 1 {
		t.Errorf("PagerDuty resolves = %d, want 1 once the data job ended well", n)
	}
	next := "fedcba9876543210fedcba9876543210"
	systemd.invoke(next)
	systemd.fail(dataJob)
	if err := notifyAt(t, r, dir, systemd, monitored(dataJob, next), t0.Add(30*time.Hour), dataJob); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, dataJob); n != 2 {
			t.Errorf("%s took %d pages across two episodes, want 2", host, n)
		}
	}
}

func TestAFailedDataJobSampledByTheReporterIsNeverNamedAndLeavesTheHeartbeatAlone(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	systemd.fail(dataJob)
	dir := t.TempDir()
	const hb = "hc.private.test"
	getenv := func(k string) string {
		if k == "TI_HEARTBEAT_URL" {
			return "https://" + hb + "/ping/uuid"
		}
		return twoChannels()(k)
	}
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty",
		"-units", "ti-adapter-boost.service", "-resolve-units", dataJob, "-systemctl", systemd.systemctl(), "-journalctl", systemd.journalctl()}
	if err := run(args, getenv, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, &http.Client{Transport: r}); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := count(r.to(host), dataJob); n != 1 {
			t.Errorf("%s: %d messages name the data job, want only its one page (no state-at-start line, no reminder)", host, n)
		}
	}
	if n := len(r.to(hb)); n != 1 {
		t.Errorf("the heartbeat was pinged %d times with only the data job failed, want 1", n)
	}
}

func TestAFailedDataJobTheHookCouldNotDeliverIsPagedOnceByTheReporterAndNeverReminded(t *testing.T) {
	flaky := &flakyRelay{relay: &relay{sent: map[string][]string{}}, failHost: ntfyHost}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	systemd.invoke(firstRun)
	systemd.fail(dataJob)
	if err := notifyAt(t, flaky, dir, systemd, monitored(dataJob, firstRun), t0.Add(time.Hour), dataJob); err == nil {
		t.Fatal("the hook should fail while ntfy is down")
	}
	flaky.setFailing(false)
	for i := 1; i <= 3; i++ {
		if err := firing(t, flaky, dir, systemd, t0.Add(time.Hour+time.Duration(i)*70*time.Minute), "ti-adapter-boost.service", "-resolve-units", dataJob); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(flaky.relay, host, dataJob); n != 1 {
			t.Errorf("%s took %d pages, want the one the reporter's retry delivered", host, n)
		}
		if strings.Contains(strings.Join(flaky.to(host), "\n"), "still failing") {
			t.Errorf("%s was reminded of the data job", host)
		}
	}
}

func TestResolveUnitsAcceptsOnlyTheDataJobAndNoUnitAlsoInUnits(t *testing.T) {
	for name, extra := range map[string][]string{
		"the engine":     {"-units", "ti-adapter-boost.service", "-resolve-units", "ti-engine@boost.service"},
		"the adapter":    {"-resolve-units", "ti-adapter-boost.service"},
		"the reporter":   {"-resolve-units", "ti-reporter@boost.service"},
		"a unit in both": {"-units", dataJob, "-resolve-units", dataJob},
	} {
		t.Run(name, func(t *testing.T) {
			r := &relay{sent: map[string][]string{}}
			systemd := newStandInSystemd(t)
			dir := t.TempDir()
			args := append([]string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy",
				"-systemctl", systemd.systemctl(), "-journalctl", systemd.journalctl()}, extra...)
			err := run(args, twoChannels(), func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, &http.Client{Transport: r})
			if err == nil || !strings.Contains(err.Error(), "-resolve-units") {
				t.Errorf("err = %v, want a startup refusal naming -resolve-units", err)
			}
			if got := systemd.systemctlCalls(); got != "" {
				t.Errorf("systemctl ran before the refusal: %s", got)
			}
		})
	}
}

func TestADataJobSampleThatCannotBeTakenNeitherFailsTheFiringNorSkipsTheEnginesStep(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	systemd.fail("ti-adapter-boost.service")
	systemd.breaks(dataJob)
	dir := t.TempDir()
	var out bytes.Buffer
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty",
		"-units", "ti-adapter-boost.service", "-resolve-units", dataJob, "-systemctl", systemd.systemctl(), "-journalctl", systemd.journalctl()}
	if err := run(args, twoChannels(), func() time.Time { return t0.Add(time.Hour) }, &out, &http.Client{Transport: r}); err != nil {
		t.Fatalf("a data job sample that failed failed the firing: %v", err)
	}
	if n := pagesFor(r, ntfyHost, "ti-adapter-boost.service"); n != 1 {
		t.Errorf("the adapter's backup page went %d times, want 1", n)
	}
	for _, want := range []string{dataJob, "trading units were checked", "data job's resolve and backup page are skipped"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the log line lacks %q: %q", want, out.String())
		}
	}
}

func TestADataJobPageThatCannotBeDeliveredIsLoggedRetriedAndNeverFailsTheFiringOrHoldsTheHeartbeat(t *testing.T) {
	flaky := &flakyRelay{relay: &relay{sent: map[string][]string{}}, failHost: ntfyHost, failBody: dataJob}
	systemd := newStandInSystemd(t)
	systemd.fail(dataJob)
	dir := t.TempDir()
	const hb = "hc.private.test"
	getenv := func(k string) string {
		if k == "TI_HEARTBEAT_URL" {
			return "https://" + hb + "/ping/uuid"
		}
		return twoChannels()(k)
	}
	fire := func(at time.Time, out *bytes.Buffer) error {
		args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty",
			"-units", "ti-adapter-boost.service", "-resolve-units", dataJob, "-systemctl", systemd.systemctl(), "-journalctl", systemd.journalctl()}
		return run(args, getenv, func() time.Time { return at }, out, &http.Client{Transport: flaky})
	}
	var out bytes.Buffer
	if err := fire(t0.Add(time.Hour), &out); err != nil {
		t.Fatalf("an undeliverable data job page failed the firing: %v\n%s", err, out.String())
	}
	if n := len(flaky.to(hb)); n != 1 {
		t.Errorf("the heartbeat was pinged %d times while the data job's page could not be delivered, want 1", n)
	}
	if !strings.Contains(out.String(), "retried at the next firing") {
		t.Errorf("the failed delivery was not logged: %q", out.String())
	}
	flaky.setFailing(false)
	if err := fire(t0.Add(2*time.Hour), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if n := pagesFor(flaky.relay, ntfyHost, dataJob); n != 1 {
		t.Errorf("ntfy took %d pages after it healed, want the one retry", n)
	}
	if n := pagesFor(flaky.relay, pdHost, dataJob); n != 1 {
		t.Errorf("PagerDuty took %d pages, want 1", n)
	}
}

// currentIs is the data job's state asked of systemd under the lock.
func currentIs(sample unitSample) func(string) (unitSample, error) {
	return func(string) (unitSample, error) { return sample, nil }
}

func dataStep(t *testing.T, r http.RoundTripper, path string, systemd standInSystemd, at time.Time, now func(string) (unitSample, error), stale ...unitSample) (soft, hard error) {
	t.Helper()
	senders, setAside, err := buildSenders("ntfy,pagerduty", twoChannels(), &http.Client{Transport: r}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	samples := map[string]unitSample{}
	if len(stale) > 0 {
		samples[dataJob] = stale[0]
	}
	return unitStep(path, nil, []string{dataJob}, samples, time.Time{}, systemd.journalctl(), nil, senders, setAside, at, time.Hour, now)
}

func TestAStaleFailedDataJobSampleCannotPageOrResolveOnItsOwnWord(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "unit-failures.json")
	base := t0.Add(time.Hour)
	second := "fedcba9876543210fedcba9876543210"
	// No page was delivered by the hook; the sample taken at the start of the firing says failed, but the unit has since ended well, so asking systemd under the lock shows it well: nothing is paged.
	if _, hard := dataStep(t, r, path, systemd, base, currentIs(unitSample{state: "inactive", invocation: second}), unitSample{failed: true, state: "failed", invocation: firstRun}); hard != nil {
		t.Fatal(hard)
	}
	if n := pagesFor(r, ntfyHost, dataJob); n != 0 {
		t.Errorf("a recovered data job was paged: %d", n)
	}
	// An episode is open; a retry fails after a recovery was sampled, and the unit under the lock is failed: the incident stays.
	if err := notifyAt(t, r, dir, systemd, monitored(dataJob, firstRun), base.Add(time.Minute), dataJob); err != nil {
		t.Fatal(err)
	}
	if err := notifyAt(t, r, dir, systemd, monitored(dataJob, second), base.Add(2*time.Minute), dataJob); err != nil {
		t.Fatal(err)
	}
	if _, hard := dataStep(t, r, path, systemd, base.Add(3*time.Minute), currentIs(unitSample{failed: true, state: "failed", invocation: second}), unitSample{state: "inactive"}); hard != nil {
		t.Fatal(hard)
	}
	if n := count(r.to(pdHost), `"event_action":"resolve"`); n != 0 {
		t.Errorf("an incident of a still failed data job was resolved: %d", n)
	}
}

func TestAResolvedDataJobRunNeverPagesAgainAndTheRecordIsBoundedAndSavedWithTheRemoval(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	systemd := newStandInSystemd(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "unit-failures.json")
	base := t0.Add(time.Hour)
	if err := notifyAt(t, r, dir, systemd, monitored(dataJob, firstRun), base, dataJob); err != nil {
		t.Fatal(err)
	}
	if _, hard := dataStep(t, r, path, systemd, base.Add(time.Hour), currentIs(unitSample{state: "inactive"})); hard != nil {
		t.Fatal(hard)
	}
	seen, sent, _, _, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := sent[handledKey(dataJob, firstRun)]; !kept {
		t.Errorf("the saved record holds no handled run after the removal: %v", sent)
	}
	for k := range seen {
		if strings.Contains(k, report.UnitFailedKey(dataJob, firstRun)) {
			t.Errorf("the saved record still holds the removed episode: %s", k)
		}
	}
	// The same failed run, sampled again after the resolve, is not paged or opened again.
	if _, hard := dataStep(t, r, path, systemd, base.Add(2*time.Hour), currentIs(unitSample{failed: true, state: "failed", invocation: firstRun})); hard != nil {
		t.Fatal(hard)
	}
	if err := notifyAt(t, r, dir, systemd, monitored(dataJob, firstRun), base.Add(3*time.Hour), dataJob); err != nil {
		t.Fatal(err)
	}
	if n := pagesFor(r, ntfyHost, dataJob); n != 1 {
		t.Errorf("a resolved failed run paged again: %d pages", n)
	}
	// A new run's failure after the recovery pages; the handled record stays bounded.
	for i := 0; i < maxHandledRuns+4; i++ {
		inv := fmt.Sprintf("%032x", 100+i)
		if err := notifyAt(t, r, dir, systemd, monitored(dataJob, inv), base.Add(time.Duration(10+i)*time.Hour), dataJob); err != nil {
			t.Fatal(err)
		}
		if _, hard := dataStep(t, r, path, systemd, base.Add(time.Duration(10+i)*time.Hour+time.Minute), currentIs(unitSample{state: "inactive"})); hard != nil {
			t.Fatal(hard)
		}
	}
	_, sent, _, _, _ = loadState(path)
	handled := 0
	for k := range sent {
		if strings.HasPrefix(k, "handled|") {
			handled++
		}
	}
	if handled != maxHandledRuns {
		t.Errorf("%d handled runs kept, want %d", handled, maxHandledRuns)
	}
	if n := pagesFor(r, ntfyHost, dataJob); n != 1+maxHandledRuns+4 {
		t.Errorf("%d pages, want one for the first run and one for each of the %d new runs after it", n, maxHandledRuns+4)
	}
	if _, kept := sent[handledKey(dataJob, fmt.Sprintf("%032x", 100+maxHandledRuns+3))]; !kept {
		t.Errorf("the newest handled run was evicted")
	}
	if _, kept := sent[handledKey(dataJob, fmt.Sprintf("%032x", 100))]; kept {
		t.Errorf("the oldest handled run was kept")
	}
	// The bound is not permanent: an evicted run is paged again if the hook is redelivered.
	if err := notifyAt(t, r, dir, systemd, monitored(dataJob, fmt.Sprintf("%032x", 100)), base.Add(100*time.Hour), dataJob); err != nil {
		t.Fatal(err)
	}
	if n := pagesFor(r, ntfyHost, dataJob); n != 2+maxHandledRuns+4 {
		t.Errorf("an evicted run did not page again: %d pages", n)
	}
}

func TestOnlyAFailureToSaveTheRecordIsHardForTheDataJob(t *testing.T) {
	persist, other := splitPersist(errors.Join(errors.New("ntfy: 502"), persistError{errors.New("disk full")}))
	if persist == nil || !strings.Contains(persist.Error(), "disk full") || strings.Contains(persist.Error(), "502") {
		t.Errorf("persist = %v", persist)
	}
	if other == nil || !strings.Contains(other.Error(), "502") || strings.Contains(other.Error(), "disk full") {
		t.Errorf("other = %v", other)
	}
}

func TestHandledRunEvictionBreaksTiesByInvocationAndPruneReceiptsLeavesTheRecordAlone(t *testing.T) {
	at := t0.Add(time.Hour)
	for round := 0; round < 20; round++ {
		u := &unitState{seen: map[string]bool{}, sent: map[string]time.Time{}, repeats: map[string]int{}}
		// 15 newer entries, then two older ones that share a time.
		for i := 0; i < maxHandledRuns-1; i++ {
			u.sent[handledKey(dataJob, fmt.Sprintf("%032x", 500+i))] = at.Add(time.Hour)
		}
		u.sent[handledKey(dataJob, fmt.Sprintf("%032x", 1))] = at
		key := report.UnitFailedKey(dataJob, fmt.Sprintf("%032x", 2))
		handleRuns(u, dataJob, key, fmt.Sprintf("%032x", 2), at)
		_, lowKept := u.sent[handledKey(dataJob, fmt.Sprintf("%032x", 1))]
		_, highKept := u.sent[handledKey(dataJob, fmt.Sprintf("%032x", 2))]
		if lowKept == highKept {
			t.Fatalf("round %d: exactly one of the tied entries should survive, kept low=%v high=%v", round, lowKept, highKept)
		}
		if !highKept {
			t.Fatalf("round %d: the tie is not broken toward the greater invocation", round)
		}
		if pruneReceipts(u.seen, u.sent, u.repeats, at.Add(1000*time.Hour)) {
			t.Fatal("pruneReceipts changed a record that holds no receipt")
		}
		if got := len(u.sent); got != maxHandledRuns {
			t.Fatalf("pruneReceipts removed handled entries: %d left, want %d", got, maxHandledRuns)
		}
	}
}
