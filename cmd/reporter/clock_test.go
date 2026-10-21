package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// The clock-check seam: the reporter runs the check command on every firing
// and pages once per failure (ADR 0019, "Alerting": the clock drifting behind
// IBKR's withholds every sell). The clock is the host's, so every reporter
// instance on the host shares one record of it; a failure pages only after
// two checks in a row fail, and a passing check clears it.

const clockFailure = "clock: skew of -7s against https://www.cloudflare.com (this clock minus the reference), more than the 2s tolerance."

// standInCheck is a clock-check command whose answer the test sets: it prints
// the message and exits 1 when the failure is set, 0 otherwise, and counts
// how often it ran.
type standInCheck struct{ dir string }

func newStandInCheck(t *testing.T) standInCheck {
	t.Helper()
	c := standInCheck{dir: t.TempDir()}
	script := "#!/bin/sh\necho x >>'" + c.dir + "/runs'\ncat '" + c.dir + "/message'\nif test -e '" + c.dir + "/fail'; then exit 1; fi\n"
	if err := os.WriteFile(filepath.Join(c.dir, "check.sh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	c.pass()
	return c
}

func (c standInCheck) path() string { return filepath.Join(c.dir, "check.sh") }

func (c standInCheck) runs() int {
	raw, _ := os.ReadFile(filepath.Join(c.dir, "runs"))
	return strings.Count(string(raw), "x")
}

func (c standInCheck) pass() {
	_ = os.Remove(filepath.Join(c.dir, "fail"))
	_ = os.WriteFile(filepath.Join(c.dir, "message"), []byte("clock: ok, skew 1s against https://www.apple.com, within 2s\n"), 0o600)
}

func (c standInCheck) fail(message string) {
	_ = os.WriteFile(filepath.Join(c.dir, "fail"), nil, 0o600)
	_ = os.WriteFile(filepath.Join(c.dir, "message"), []byte(message+"\n"), 0o600)
}

func clockRun(t *testing.T, r http.RoundTripper, dir, check string, at time.Time, extra ...string) error {
	t.Helper()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty", "-repeat-interval", "30m"}
	if check != "" {
		args = append(args, "-clock-check", check)
	}
	args = append(args, extra...)
	return run(args, twoChannels(), func() time.Time { return at }, &bytes.Buffer{}, &http.Client{Transport: r})
}

// failingTwice runs the reporter on a failing check twice, one timer period
// apart, which is what it takes to page.
func failingTwice(t *testing.T, r http.RoundTripper, dir string, check standInCheck, at time.Time) {
	t.Helper()
	check.fail(clockFailure)
	for i := 0; i < 2; i++ {
		if err := clockRun(t, r, dir, check.path(), at.Add(time.Duration(i)*5*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
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
	failingTwice(t, r, t.TempDir(), check, t0.Add(time.Hour))
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

func TestOneFailingCheckPagesNothingAndTwoInARowPage(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	check := newStandInCheck(t)
	check.fail(clockFailure)
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	if err := clockRun(t, r, dir, check.path(), at); err != nil {
		t.Fatal(err)
	}
	if got := clockMessages(r); len(got) != 0 {
		t.Fatalf("one failing check paged: %v", got)
	}
	if err := clockRun(t, r, dir, check.path(), at.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := len(clockMessages(r)); got != 2 {
		t.Fatalf("%d clock messages after two failing checks, want 1 per channel", got)
	}
}

func TestAPassBetweenTwoFailuresPagesNothing(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	check := newStandInCheck(t)
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	for i, fails := range []bool{true, false, true, false, true} {
		if fails {
			check.fail(clockFailure)
		} else {
			check.pass()
		}
		if err := clockRun(t, r, dir, check.path(), at.Add(time.Duration(i)*5*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if got := clockMessages(r); len(got) != 0 {
		t.Fatalf("failures with a pass between them paged: %v", got)
	}
}

func TestChecksWithinOneTimerPeriodRunOnceAndCountOnce(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	check := newStandInCheck(t)
	check.fail(clockFailure)
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	for _, d := range []time.Duration{0, time.Minute, 2 * time.Minute} {
		if err := clockRun(t, r, dir, check.path(), at.Add(d)); err != nil {
			t.Fatal(err)
		}
	}
	if check.runs() != 1 {
		t.Errorf("the check ran %d times within one timer period, want 1", check.runs())
	}
	if got := clockMessages(r); len(got) != 0 {
		t.Fatalf("one failing check, reused, paged: %v", got)
	}
}

// clockPages counts the PagerDuty trigger events sent for the clock alert;
// resolves are not pages.
func clockPages(r *relay) int {
	n := 0
	for _, m := range r.to("events.pagerduty.test") {
		if strings.Contains(m, "clock-drift:") && strings.Contains(m, `"event_action":"trigger"`) {
			n++
		}
	}
	return n
}

// failuresRecorded is the consecutive-failure count in the host's clock state.
func failuresRecorded(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "host-clock.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"0", "1", "2", "3"} {
		if strings.Contains(string(raw), `"consecutive_failures":`+n+`,`) || strings.Contains(string(raw), `"consecutive_failures":`+n+`}`) {
			return n
		}
	}
	t.Fatalf("no failure count in %s", raw)
	return ""
}

func TestACheckRecordedAMomentInTheFutureIsThisPeriodsAndIsReusedNotCounted(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	check := newStandInCheck(t)
	check.fail(clockFailure)
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	for _, d := range []time.Duration{0, -time.Minute} {
		if err := clockRun(t, r, dir, check.path(), at.Add(d)); err != nil {
			t.Fatal(err)
		}
	}
	if check.runs() != 1 {
		t.Errorf("a check dated after this firing was made again: %d runs, want 1", check.runs())
	}
	if n := failuresRecorded(t, dir); n != "1" {
		t.Errorf("the reuse moved the failure count to %s, want 1", n)
	}
	if got := clockPages(r); got != 0 {
		t.Errorf("%d clock pages went out after one failed check, want 0", got)
	}
}

// A clock set back by hours is itself the fault (ADR 0019: a wrong clock makes
// the engine withhold sells): the check recorded while it ran ahead counts as
// a failed check, and a new one is made.
func TestACheckRecordedHoursAheadMeansTheClockWentBackAndCountsAsAFailedCheck(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	check := newStandInCheck(t)
	check.fail(clockFailure)
	dir := t.TempDir()
	at := t0.Add(5 * time.Hour)
	for _, d := range []time.Duration{0, -3 * time.Hour} {
		if err := clockRun(t, r, dir, check.path(), at.Add(d)); err != nil {
			t.Fatal(err)
		}
	}
	if check.runs() != 2 {
		t.Errorf("a check dated 3 hours ahead was reused: %d runs, want 2", check.runs())
	}
	if n := failuresRecorded(t, dir); n != "2" {
		t.Errorf("the step back was not counted: failure count %s, want 2", n)
	}
	if got := clockPages(r); got != 1 {
		t.Errorf("%d clock pages went out on a failed check and a step back, want 1", got)
	}
}

// A clock that keeps stepping back by hours, each check passing where it
// stands, pages on the second firing that finds the step.
func TestAClockThatKeepsSteppingBackPagesOnTheSecondFiringThatFindsTheStep(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	check := newStandInCheck(t)
	dir := t.TempDir()
	at := t0.Add(10 * time.Hour)
	for i, d := range []time.Duration{0, -3 * time.Hour, -6 * time.Hour} {
		if err := clockRun(t, r, dir, check.path(), at.Add(d)); err != nil {
			t.Fatal(err)
		}
		want := 0
		if i == 2 {
			want = 1
		}
		if got := clockPages(r); got != want {
			t.Fatalf("after firing %d: %d clock pages, want %d", i+1, got, want)
		}
	}
	body := strings.Join(r.to("events.pagerduty.test"), "\n")
	if !strings.Contains(body, "stepped back by about") {
		t.Errorf("the page does not say the clock stepped back: %s", body)
	}
}

// Three timers fire together; the sibling that took the clock time earlier
// reaches the clock step after another has recorded its check, and must not
// count a second failure in the same period.
func TestAnInstanceThatReachesTheClockStepAfterASiblingRecordedItsCheckDoesNotCountAgain(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	check := newStandInCheck(t)
	check.fail(clockFailure)
	reports := t.TempDir()
	clockState := filepath.Join(reports, "host-clock.json")
	fire := func(account string, now func() time.Time) {
		t.Helper()
		args := []string{"-journal", writeJournal(t, t.TempDir()), "-state", filepath.Join(reports, account+".reporter.json"),
			"-clock-state", clockState, "-account", account, "-channel", "ntfy,pagerduty", "-clock-check", check.path()}
		if err := run(args, twoChannels(), now, &bytes.Buffer{}, &http.Client{Transport: r}); err != nil {
			t.Fatal(err)
		}
	}
	p1 := t0.Add(time.Hour)
	p2 := p1.Add(5 * time.Minute)
	fire("first", func() time.Time { return p1 })
	// The sibling that is quick: its check is recorded a minute into the period.
	fire("quick", func() time.Time { return p2.Add(time.Minute) })
	// The slow one took its time at the period's start, then spent seconds
	// delivering alerts; every reading of its clock is earlier than the
	// sibling's record.
	var calls int
	fire("slow", func() time.Time { calls++; return p2.Add(time.Duration(calls) * time.Second) })
	if check.runs() != 2 {
		t.Errorf("the check ran %d times in two periods, want 2", check.runs())
	}
	raw, err := os.ReadFile(clockState)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"consecutive_failures":2`) {
		t.Errorf("the failure count moved within one period: %s", raw)
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
	}{{false, 0}, {true, 0}, {true, 2}, {true, 2}, {false, 2}, {true, 2}, {true, 4}}
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

func pagerDutyActions(r *relay, action string) []string {
	var out []string
	for _, m := range r.to("events.pagerduty.test") {
		if strings.Contains(m, `"event_action":"`+action+`"`) && strings.Contains(m, "clock-drift:") {
			out = append(out, m)
		}
	}
	return out
}

func TestAPassingCheckAfterAPageResolvesThePagerDutyIncident(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	check := newStandInCheck(t)
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	failingTwice(t, r, dir, check, at)
	check.pass()
	for i := 2; i < 4; i++ {
		if err := clockRun(t, r, dir, check.path(), at.Add(time.Duration(i)*5*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	resolved := pagerDutyActions(r, "resolve")
	if len(resolved) != 1 {
		t.Fatalf("%d PagerDuty resolves, want 1 after the first passing check: %v", len(resolved), r.to("events.pagerduty.test"))
	}
	if want := `"dedup_key":"clock-drift:` + hostName() + `"`; !strings.Contains(resolved[0], want) {
		t.Errorf("the resolve %s lacks %s", resolved[0], want)
	}
	// The next drift is a trigger PagerDuty opens a new incident for.
	failingTwice(t, r, dir, check, at.Add(time.Hour))
	if got := len(pagerDutyActions(r, "trigger")); got != 2 {
		t.Errorf("%d PagerDuty clock triggers, want 2 (one per failure)", got)
	}
}

// refusingResolve answers every PagerDuty resolve with a failure until it is
// allowed, and passes everything else to the relay.
type refusingResolve struct {
	*relay
	mu    sync.Mutex
	allow bool
}

func (f *refusingResolve) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	f.mu.Lock()
	allow := f.allow
	f.mu.Unlock()
	if !allow && strings.Contains(string(body), `"event_action":"resolve"`) {
		return &http.Response{StatusCode: http.StatusBadGateway, Status: "502 Bad Gateway", Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: req}, nil
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	return f.relay.RoundTrip(req)
}

func TestAResolveThatFailsIsTriedAgainAtTheNextFiring(t *testing.T) {
	f := &refusingResolve{relay: &relay{sent: map[string][]string{}}}
	check := newStandInCheck(t)
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	failingTwice(t, f, dir, check, at)
	check.pass()
	if err := clockRun(t, f, dir, check.path(), at.Add(10*time.Minute)); err == nil {
		t.Error("a failed resolve was not reported as the run's error")
	}
	f.mu.Lock()
	f.allow = true
	f.mu.Unlock()
	if err := clockRun(t, f, dir, check.path(), at.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := len(pagerDutyActions(f.relay, "resolve")); got != 1 {
		t.Fatalf("%d resolves delivered, want 1 on the retry", got)
	}
	// Once resolved, nothing more is sent for a passing clock.
	if err := clockRun(t, f, dir, check.path(), at.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := len(pagerDutyActions(f.relay, "resolve")); got != 1 {
		t.Fatalf("%d resolves, want still 1", got)
	}
}

func TestEveryAccountOnOneHostSharesOnePageForOneClockFault(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	check := newStandInCheck(t)
	check.fail(clockFailure)
	reports := t.TempDir()
	at := t0.Add(time.Hour)
	for i := 0; i < 2; i++ {
		for _, account := range []string{"boost", "trad-ira", "roth-ira"} {
			args := []string{"-journal", writeJournal(t, t.TempDir()), "-state", filepath.Join(reports, account+".reporter.json"),
				"-clock-state", filepath.Join(reports, "host-clock.json"), "-account", account,
				"-channel", "ntfy,pagerduty", "-clock-check", check.path()}
			// The three timers fire within the same minute.
			when := at.Add(time.Duration(i)*5*time.Minute + 10*time.Second)
			if err := run(args, twoChannels(), func() time.Time { return when }, &bytes.Buffer{}, &http.Client{Transport: r}); err != nil {
				t.Fatal(err)
			}
		}
	}
	got := clockMessages(r)
	if len(got) != 2 {
		t.Fatalf("%d clock messages for one host's fault, want 1 per channel: %v", len(got), got)
	}
	if check.runs() != 2 {
		t.Errorf("the check ran %d times in two timer periods, want 2", check.runs())
	}
	for _, m := range got {
		if !strings.Contains(m, hostName()) {
			t.Errorf("the page does not name the host %q: %s", hostName(), m)
		}
	}
}

func TestTheClockStateFileDefaultsToBesideTheReportersState(t *testing.T) {
	check := newStandInCheck(t)
	dir := t.TempDir()
	if err := clockRun(t, &relay{sent: map[string][]string{}}, dir, check.path(), t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "host-clock.json")); err != nil {
		t.Errorf("no host clock state beside the reporter's state: %v", err)
	}
}

func TestAnUnreadableHostClockStateIsAnErrorNotAReset(t *testing.T) {
	check := newStandInCheck(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "host-clock.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &relay{sent: map[string][]string{}}
	err := clockRun(t, r, dir, check.path(), t0.Add(time.Hour))
	if err == nil || !strings.Contains(err.Error(), "clock state") {
		t.Fatalf("got %v, want an error naming the clock state", err)
	}
	if len(r.to("events.pagerduty.test")) == 0 {
		t.Error("an unreadable clock state held back the incident alerts")
	}
}

// orderWatch records, for each message, whether the clock check had already
// run when it arrived.
type orderWatch struct {
	*relay
	marker string
	mu     sync.Mutex
	early  []string
}

func (o *orderWatch) RoundTrip(req *http.Request) (*http.Response, error) {
	title := req.Header.Get("Title")
	if _, err := os.Stat(o.marker); err == nil && !strings.Contains(strings.ToLower(title), "clock") && title != "" {
		o.mu.Lock()
		o.early = append(o.early, title)
		o.mu.Unlock()
	}
	return o.relay.RoundTrip(req)
}

func TestTheClockCheckRunsOnlyAfterTheIncidentAlertsAreDelivered(t *testing.T) {
	check := newStandInCheck(t)
	o := &orderWatch{relay: &relay{sent: map[string][]string{}}, marker: filepath.Join(check.dir, "runs")}
	if err := clockRun(t, o, t.TempDir(), check.path(), t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(o.to("ntfy.private.test")) == 0 {
		t.Fatal("no incident alert was sent")
	}
	if check.runs() != 1 {
		t.Fatalf("the check ran %d times, want 1", check.runs())
	}
	if len(o.early) != 0 {
		t.Errorf("incident alerts sent after the clock check, which could have held them back: %v", o.early)
	}
}

func withClockBounds(t *testing.T, timeout, waitDelay time.Duration) {
	t.Helper()
	oldTimeout, oldDelay := clockCheckTimeout, clockCheckWaitDelay
	clockCheckTimeout, clockCheckWaitDelay = timeout, waitDelay
	t.Cleanup(func() { clockCheckTimeout, clockCheckWaitDelay = oldTimeout, oldDelay })
}

func TestAHungClockCheckIsStoppedAtItsBoundEvenWhenItsChildrenHoldTheOutput(t *testing.T) {
	withClockBounds(t, 300*time.Millisecond, 300*time.Millisecond)
	dir := t.TempDir()
	for name, body := range map[string]string{
		// A child in the check's process group, holding the output open.
		"child": "echo started\nsleep 10 &\nsleep 30\n",
		// A child that left the group (its own session), still holding it.
		"escaped": "echo started\npython3 -c 'import os,time; os.setsid(); time.sleep(10)' &\nsleep 30\n",
	} {
		t.Run(name, func(t *testing.T) {
			script := filepath.Join(dir, name+".sh")
			if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			c := runClockCheck(script, t0)
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("the check held the run for %s, past its bound", took)
			}
			if c.Passed || !strings.Contains(c.Output, "timed out") || !strings.Contains(c.Output, "started") {
				t.Errorf("got %+v, want a failed check that says it timed out and what it had said", c)
			}
		})
	}
}

func TestAClockCheckThatCannotRunSaysWhy(t *testing.T) {
	withClockBounds(t, 300*time.Millisecond, 300*time.Millisecond)
	dir := t.TempDir()
	notExecutable := filepath.Join(dir, "not-executable.sh")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	silent := filepath.Join(dir, "silent.sh")
	if err := os.WriteFile(silent, []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	quietHang := filepath.Join(dir, "hang.sh")
	if err := os.WriteFile(quietHang, []byte("#!/bin/sh\nsleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for command, want := range map[string]string{
		filepath.Join(dir, "missing.sh"): "is missing",
		notExecutable:                    "not executable",
		silent:                           "said nothing (exit status 3)",
		quietHang:                        "timed out",
	} {
		c := runClockCheck(command, t0)
		if c.Passed || !strings.Contains(c.Output, want) {
			t.Errorf("%s: got %+v, want a failure saying %q", filepath.Base(command), c, want)
		}
	}
}

func TestACheckThatCannotRunIsAFailureNotASilence(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		if err := clockRun(t, r, dir, filepath.Join(dir, "missing.sh"), t0.Add(time.Hour+time.Duration(i)*5*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, m := range r.to("ntfy.private.test") {
		if strings.Contains(m, "No skew was measured") && strings.Contains(m, "is missing") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want one alert saying the check is missing: %v", r.to("ntfy.private.test"))
	}
}

func TestAChannelNamedLaterTakesTheClockAlertOnceAndTheFirstIsNotToldAgain(t *testing.T) {
	check := newStandInCheck(t)
	check.fail(clockFailure)
	dir := t.TempDir()
	down := &relay{sent: map[string][]string{}}
	at := t0.Add(time.Hour)
	args := func(channel string, i int) error {
		a := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", channel, "-clock-check", check.path()}
		when := at.Add(time.Duration(i) * 5 * time.Minute)
		return run(a, twoChannels(), func() time.Time { return when }, &bytes.Buffer{}, &http.Client{Transport: down})
	}
	for i, channel := range []string{"ntfy", "ntfy", "ntfy,pagerduty"} {
		if err := args(channel, i); err != nil {
			t.Fatal(err)
		}
	}
	if len(clockMessages(down)) != 2 || len(down.to("events.pagerduty.test")) == 0 {
		t.Fatalf("messages: %v", clockMessages(down))
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
	dir := t.TempDir()
	if err := clockRun(t, r, dir, "", t0.Add(time.Hour), "-summary"); err != nil {
		t.Fatal(err)
	}
	for _, m := range append(r.to("events.pagerduty.test"), r.to("ntfy.private.test")...) {
		if strings.Contains(m, "Clock check") {
			t.Errorf("no check was named, yet: %q", m)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "host-clock.json")); err == nil {
		t.Error("a host clock state was written with no clock check named")
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

// The real check's output, read by the reporter: deploy/libexec/clock-check.sh
// documents the lines it prints, and these are the ones the alert reads the
// skew from or says none was measured. timedatectl, systemd-detect-virt and
// curl are stand-ins on PATH, as in deploy/rehearsal/test-clock.sh.
func TestTheRealClockCheckSaysWhatTheReporterReads(t *testing.T) {
	stubs := t.TempDir()
	for name, body := range map[string]string{
		"timedatectl":         "#!/bin/sh\nexit 1\n",
		"systemd-detect-virt": "#!/bin/sh\ntest \"$FAKE_CONTAINER\" = 1\n",
		"curl": "#!/bin/sh\ntest \"$FAKE_OFFLINE\" != 1 || exit 6\n" +
			"python3 -I -c 'import email.utils,sys,time;print(\"HTTP/2 200\\r\\nDate: \"+email.utils.formatdate(int(time.time())+int(sys.argv[1]),usegmt=True)+\"\\r\\n\\r\")' \"$FAKE_OFFSET\"\n",
	} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "deploy", "libexec", "clock-check.sh"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TI_CLOCK_CONFIG", filepath.Join(stubs, "none.conf"))
	t.Setenv("TI_CLOCK_CONTAINER_MARKERS", "")
	t.Setenv("TI_CLOCK_REFERENCES", "https://reference.test")
	t.Setenv("TI_CLOCK_MAX_ROUNDTRIP_SECONDS", "10")
	for name, c := range map[string]struct {
		container, offline, offset string
		passes                     bool
		skew                       int
		measured                   bool
	}{
		"behind":         {"1", "0", "60", false, -60, true},
		"within":         {"1", "0", "0", true, 0, true},
		"unreachable":    {"1", "1", "0", false, 0, false},
		"unsynchronised": {"0", "0", "0", false, 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("FAKE_CONTAINER", c.container)
			t.Setenv("FAKE_OFFLINE", c.offline)
			t.Setenv("FAKE_OFFSET", c.offset)
			got := runClockCheck(script, t0)
			if got.Passed != c.passes {
				t.Fatalf("passed %v, want %v: %s", got.Passed, c.passes, got.Output)
			}
			skew, ok := report.ClockSkewSeconds(got.Output)
			if ok != c.measured || (ok && (skew < c.skew-2 || skew > c.skew+2)) {
				t.Errorf("read skew %d (%v) from %q, want about %d (%v)", skew, ok, got.Output, c.skew, c.measured)
			}
			if !c.passes {
				body := report.ClockDriftAlert("h", got, 2).Body
				if strings.Contains(body, "could be read") {
					t.Errorf("the real check's output was not understood: %s", body)
				}
			}
		})
	}
}

func TestAReporterThatFindsTheClockBeingCheckedSkipsTheCheckAndShowsTheLastOne(t *testing.T) {
	check := newStandInCheck(t)
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	if err := clockRun(t, &relay{sent: map[string][]string{}}, dir, check.path(), at); err != nil {
		t.Fatal(err)
	}
	// Another instance holds the host clock state's lock: it is checking now.
	lock, err := os.OpenFile(filepath.Join(dir, "host-clock.json.lock"), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	r := &relay{sent: map[string][]string{}}
	if err := clockRun(t, r, dir, check.path(), at.Add(10*time.Minute), "-summary"); err != nil {
		t.Fatal(err)
	}
	if check.runs() != 1 {
		t.Errorf("the check ran %d times while another instance held it, want 1", check.runs())
	}
	found := false
	for _, m := range r.to("events.pagerduty.test") {
		found = found || strings.Contains(m, "Clock check: ok at "+at.UTC().Format("2006-01-02 15:04Z"))
	}
	if !found {
		t.Errorf("the summary does not show the last recorded check: %v", r.to("events.pagerduty.test"))
	}
}

func TestAHostClockStateThatCannotBeLockedIsAnErrorAndHoldsNothingBack(t *testing.T) {
	check := newStandInCheck(t)
	dir := t.TempDir()
	r := &relay{sent: map[string][]string{}}
	err := clockRun(t, r, dir, check.path(), t0.Add(time.Hour), "-clock-state", filepath.Join(dir, "no-such-dir", "host-clock.json"))
	if err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("got %v, want an error naming the lock", err)
	}
	if len(r.to("events.pagerduty.test")) == 0 {
		t.Error("the incident alerts were held back")
	}
}

// A PagerDuty channel set aside for a bad secret cannot close its incident,
// so the incident is kept open on record and closed once the channel works
// again, not forgotten while still open there.
func TestAResolveWaitsForAPagerDutyChannelSetAside(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	check := newStandInCheck(t)
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	failingTwice(t, r, dir, check, at)
	check.pass()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty", "-repeat-interval", "30m", "-clock-check", check.path()}
	broken := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": "https://ntfy.private.test", "TI_PAGERDUTY_ROUTING_KEY": ""})
	err := run(args, broken, func() time.Time { return at.Add(10 * time.Minute) }, &bytes.Buffer{}, &http.Client{Transport: r})
	if err == nil {
		t.Error("the set-aside channel was not reported as the run's error")
	}
	if got := len(pagerDutyActions(r, "resolve")); got != 0 {
		t.Fatalf("%d resolves sent through a channel set aside", got)
	}
	if err := clockRun(t, r, dir, check.path(), at.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := len(pagerDutyActions(r, "resolve")); got != 1 {
		t.Fatalf("%d resolves once the channel works again, want 1", got)
	}
}

// runWith runs the reporter for the clock with the given channel list and
// environment, so a test can set one channel aside or leave one out.
func runWith(t *testing.T, r http.RoundTripper, dir string, check standInCheck, channels string, getenv func(string) string, at time.Time) error {
	t.Helper()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", channels, "-repeat-interval", "30m", "-clock-check", check.path()}
	return run(args, getenv, func() time.Time { return at }, &bytes.Buffer{}, &http.Client{Transport: r})
}

func ntfyClockPages(r *relay) int {
	n := 0
	for _, m := range r.to("ntfy.private.test") {
		if strings.Contains(m, "-7s") {
			n++
		}
	}
	return n
}

// A passing check clears the record channel by channel: a channel that works
// forgets the incident at once, so a new fault pages it again, while the set-
// aside PagerDuty channel keeps its record until it can send the resolve.
func TestAPassingCheckForgetsTheWorkingChannelsWhileOneIsSetAside(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	check := newStandInCheck(t)
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	failingTwice(t, r, dir, check, at)
	before := ntfyClockPages(r)
	if before == 0 {
		t.Fatal("ntfy was never paged")
	}
	broken := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": "https://ntfy.private.test", "TI_PAGERDUTY_ROUTING_KEY": ""})
	triggers, resolves := len(pagerDutyActions(r, "trigger")), len(pagerDutyActions(r, "resolve"))
	onlyPagerDuty := func(err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "pagerduty") || strings.Contains(err.Error(), "ntfy") {
			t.Errorf("got %v, want an error naming only the set-aside pagerduty channel", err)
		}
	}
	check.pass()
	onlyPagerDuty(runWith(t, r, dir, check, "ntfy,pagerduty", broken, at.Add(10*time.Minute)))
	check.fail(clockFailure)
	for i := 2; i <= 3; i++ {
		onlyPagerDuty(runWith(t, r, dir, check, "ntfy,pagerduty", broken, at.Add(time.Duration(10+5*i)*time.Minute)))
	}
	if got := ntfyClockPages(r); got != before+1 {
		t.Errorf("ntfy was paged %d times after a new fault, was %d before: want exactly one more", got, before)
	}
	if got := len(pagerDutyActions(r, "trigger")); got != triggers {
		t.Errorf("%d PagerDuty triggers, was %d: a set-aside channel was sent to", got, triggers)
	}
	if got := len(pagerDutyActions(r, "resolve")); got != resolves {
		t.Errorf("%d PagerDuty resolves, was %d: a set-aside channel was sent to", got, resolves)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "host-clock.json"))
	if err != nil || !strings.Contains(string(raw), "pagerduty|clock-drift:") {
		t.Errorf("the set-aside PagerDuty channel's record was dropped: %s (%v)", raw, err)
	}
}

// A PagerDuty resolve that fails keeps only PagerDuty's record, for the
// retry; ntfy, which has nothing to close, forgets the incident at once, so a
// new fault during the PagerDuty failure still pages ntfy.
func TestAFailedPagerDutyResolveDoesNotHoldNtfyBack(t *testing.T) {
	f := &refusingResolve{relay: &relay{sent: map[string][]string{}}}
	check := newStandInCheck(t)
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	failingTwice(t, f, dir, check, at)
	before := ntfyClockPages(f.relay)
	if before == 0 {
		t.Fatal("ntfy was never paged")
	}
	check.pass()
	if err := clockRun(t, f, dir, check.path(), at.Add(10*time.Minute)); err == nil {
		t.Error("a failed resolve was not reported as the run's error")
	}
	check.fail(clockFailure)
	for i := 2; i <= 3; i++ {
		_ = clockRun(t, f, dir, check.path(), at.Add(time.Duration(10+5*i)*time.Minute))
	}
	if got := ntfyClockPages(f.relay); got != before+1 {
		t.Errorf("ntfy was paged %d times, was %d before: a failed PagerDuty resolve held it back", got, before)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "host-clock.json"))
	if !strings.Contains(string(raw), "pagerduty|clock-drift:") {
		t.Errorf("PagerDuty's record was not kept for the resolve retry: %s", raw)
	}
}

// A channel taken out of -channel is neither sending nor set aside, so its
// record is dropped when a check passes: it cannot hold the rest back.
func TestAPassingCheckDropsTheRecordOfAChannelNoLongerConfigured(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	check := newStandInCheck(t)
	dir := t.TempDir()
	at := t0.Add(time.Hour)
	failingTwice(t, r, dir, check, at)
	check.pass()
	if err := runWith(t, r, dir, check, "ntfy", twoChannels(), at.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "host-clock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "pagerduty|") {
		t.Errorf("the record of a channel no longer configured was kept: %s", raw)
	}
}
