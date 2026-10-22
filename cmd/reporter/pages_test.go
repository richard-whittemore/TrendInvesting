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

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// summaryOnlyFails is a client whose own ntfy server refuses the daily summary
// and takes everything else, and whose PagerDuty takes every event.
type summaryOnlyFails struct {
	mu   sync.Mutex
	sent map[string][]string
}

func (s *summaryOnlyFails) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	s.mu.Lock()
	s.sent[req.URL.Host] = append(s.sent[req.URL.Host], req.Header.Get("Title")+"\n"+string(body))
	s.mu.Unlock()
	status, answer := http.StatusOK, ""
	switch {
	case strings.Contains(req.URL.Host, "pagerduty"):
		status, answer = http.StatusAccepted, `{"status":"success"}`
	case strings.Contains(req.Header.Get("Title"), "daily summary"):
		status = http.StatusServiceUnavailable
	}
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(answer)), Header: http.Header{}, Request: req}, nil
}

// TestTheDailySummaryGoesToTheOwnNtfyServerAndNeverOpensAnIncident: the
// summary is a routine message with balances, so it goes to the owner's own
// ntfy server and PagerDuty is sent incidents only (ADR 0019, "Alerting").
func TestTheDailySummaryGoesToTheOwnNtfyServerAndNeverOpensAnIncident(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty", "-summary"}
	var out bytes.Buffer
	if err := run(args, twoChannels(), func() time.Time { return t0.Add(time.Hour) }, &out, &http.Client{Transport: r}); err != nil {
		t.Fatal(err)
	}
	own := strings.Join(r.to("ntfy.private.test"), "\n")
	if !strings.Contains(own, "daily summary 2024-01-02") || !strings.Contains(own, "engine halted") {
		t.Errorf("the own ntfy server must take the summary and the incident alerts:\n%s", own)
	}
	pager := strings.Join(r.to("events.pagerduty.test"), "\n")
	if strings.Contains(pager, "daily summary") {
		t.Errorf("the daily summary opened a PagerDuty incident:\n%s", pager)
	}
	if !strings.Contains(pager, "engine halted") || !strings.Contains(pager, "order refused") {
		t.Errorf("PagerDuty must still take the incidents:\n%s", pager)
	}
	if strings.Contains(out.String(), "the daily summary is not sent") {
		t.Errorf("the summary has a route, yet the reporter said it is not sent:\n%s", out.String())
	}
}

// TestTheDailySummaryWithOnlyPagerDutyIsNotSentAndSaidSoOnce: with no ntfy
// server of the owner's own, PagerDuty does not take the summary; the owner,
// whose only eyes are the channels, is told so once, as a routine message
// through the incident channels (ADR 0019, "Alerting").
func TestTheDailySummaryWithOnlyPagerDutyIsNotSentAndSaidSoOnce(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "pagerduty", "-summary"}
	now := func() time.Time { return t0.Add(time.Hour) }
	for run1, want := range []int{1, 0} {
		var out bytes.Buffer
		if err := run(args, twoChannels(), now, &out, &http.Client{Transport: r}); err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(out.String(), "the daily summary is not sent"); got != want {
			t.Errorf("run %d said so %d times, want %d:\n%s", run1+1, got, want, out.String())
		}
	}
	pager := r.to("events.pagerduty.test")
	if count(pager, "daily summary 2024") != 0 || count(pager, "engine halted") == 0 {
		t.Errorf("PagerDuty must take incidents and no summary:\n%s", strings.Join(pager, "\n"))
	}
	if n := count(pager, "the daily summary has no channel: name your own ntfy server"); n != 1 {
		t.Errorf("PagerDuty took the notice %d times over two runs, want once:\n%s", n, strings.Join(pager, "\n"))
	}
	for _, m := range pager {
		if strings.Contains(m, "has no channel") && !strings.Contains(m, `"severity":"info"`) {
			t.Errorf("the notice is routine, not an urgent page:\n%s", m)
		}
	}
}

// TestNamingTheOwnServerAfterwardsSendsTheSummaryAndSaysNothing: the notice is
// kept until a server of the owner's own is named, and is said again if that
// server is later left out.
func TestNamingTheOwnServerAfterwardsSendsTheSummaryAndTheNoticeReturnsIfItIsLeftOut(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	journalPath := writeJournal(t, dir)
	state := filepath.Join(dir, "s.json")
	now := func() time.Time { return t0.Add(time.Hour) }
	runWith := func(channels string) string {
		t.Helper()
		var out bytes.Buffer
		if err := run([]string{"-journal", journalPath, "-state", state, "-channel", channels, "-summary"}, twoChannels(), now, &out, &http.Client{Transport: r}); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if got := runWith("pagerduty"); !strings.Contains(got, "the daily summary is not sent") {
		t.Fatalf("first run did not say so:\n%s", got)
	}
	if got := runWith("pagerduty,ntfy"); strings.Contains(got, "the daily summary is not sent") {
		t.Fatalf("the own server is named, yet:\n%s", got)
	}
	if !strings.Contains(strings.Join(r.to("ntfy.private.test"), "\n"), "daily summary") {
		t.Fatal("the summary did not reach the own server once it was named")
	}
	if got := runWith("pagerduty"); !strings.Contains(got, "the daily summary is not sent") {
		t.Fatalf("the own server was left out again, and the reporter did not say so:\n%s", got)
	}
}

// TestAFailedSummaryNeverBlocksAnIncidentPage: the summary's channel failing
// fails the run with that channel named, and every incident still reaches
// both channels (ADR 0019, "Alerting": one channel's fault must not silence
// another).
func TestAFailedSummaryNeverBlocksAnIncidentPage(t *testing.T) {
	r := &summaryOnlyFails{sent: map[string][]string{}}
	dir := t.TempDir()
	state := filepath.Join(dir, "s.json")
	args := []string{"-journal", writeJournal(t, dir), "-state", state, "-channel", "ntfy,pagerduty", "-summary"}
	err := run(args, twoChannels(), func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, &http.Client{Transport: r})
	if err == nil || !strings.Contains(err.Error(), "ntfy") {
		t.Fatalf("err = %v, want the failed summary named", err)
	}
	for _, host := range []string{"ntfy.private.test", "events.pagerduty.test"} {
		got := strings.Join(r.sent[host], "\n")
		if !strings.Contains(got, "engine halted") || !strings.Contains(got, "order refused") {
			t.Errorf("%s did not take the incidents:\n%s", host, got)
		}
	}
	if strings.Contains(strings.Join(r.sent["events.pagerduty.test"], "\n"), "daily summary") {
		t.Error("the failed summary was sent to PagerDuty instead")
	}
	raw, _ := os.ReadFile(state)
	if strings.Contains(string(raw), "summary:") {
		t.Errorf("a summary that failed was recorded as delivered: %s", raw)
	}
}

// TestANewStateFileReplaysNoHistoryButPagesWhatIsInForce: a state file
// that is new or lost leaves each channel with no record, so it starts at the
// run's own time: the log's earlier routine history is not sent to it, and an
// event after that time is. What is still in force is not history: the
// account's standing and each open alert reach every channel once, marked
// "state at reporter start", and the open alerts repeat as usual (ADR 0019,
// "Alerting").
func TestANewStateFileReplaysNoHistoryButPagesWhatIsInForce(t *testing.T) {
	withBaseline(t)
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	journalPath := writeJournal(t, dir)
	state := filepath.Join(dir, "s.json")
	args := []string{"-journal", journalPath, "-state", state, "-channel", "ntfy,pagerduty"}
	runAt := func(at time.Time) {
		t.Helper()
		if err := run(args, twoChannels(), func() time.Time { return at }, &bytes.Buffer{}, &http.Client{Transport: r}); err != nil {
			t.Fatal(err)
		}
	}

	runAt(t0.Add(time.Hour))
	for _, host := range []string{"ntfy.private.test", "events.pagerduty.test"} {
		got := r.to(host)
		if n := count(got, "order refused"); n != 0 {
			t.Errorf("%s was sent the log's routine history: %q", host, got)
		}
		if n := count(got, "state at reporter start: Halted"); n != 1 {
			t.Errorf("%s took the account's standing %d times, want once: %q", host, n, got)
		}
		if n := count(got, "state at reporter start: joint: engine halted"); n != 1 {
			t.Errorf("%s took the open halt %d times, want once, marked as the state at reporter start: %q", host, n, got)
		}
	}
	raw, _ := os.ReadFile(state)
	if !strings.Contains(string(raw), `"baselines"`) {
		t.Fatalf("the state file does not record where each channel starts: %s", raw)
	}

	// Within the repeat interval nothing more; after it the halt repeats.
	before := len(r.to("ntfy.private.test")) + len(r.to("events.pagerduty.test"))
	runAt(t0.Add(time.Hour + time.Minute))
	if got := len(r.to("ntfy.private.test")) + len(r.to("events.pagerduty.test")); got != before {
		t.Fatalf("a second run within the interval sent %d more", got-before)
	}
	runAt(t0.Add(time.Hour + defaultRepeatInterval + time.Minute))
	for _, host := range []string{"ntfy.private.test", "events.pagerduty.test"} {
		if n := count(r.to(host), "Engine halted since"); n != 2 {
			t.Errorf("%s took the halt %d times, want its first delivery and one repeat: %q", host, n, r.to(host))
		}
		if n := count(r.to(host), "state at reporter start: Halted"); n != 1 {
			t.Errorf("%s took the standing %d times, want once only", host, n)
		}
	}

	// Run again as if the log's events were after the baseline: a state file
	// begun an hour before them takes them, and is told no standing, since
	// nothing before it was held back from it.
	r2 := &relay{sent: map[string][]string{}}
	fresh := filepath.Join(t.TempDir(), "s.json")
	if err := run([]string{"-journal", journalPath, "-state", fresh, "-channel", "ntfy,pagerduty"}, twoChannels(),
		func() time.Time { return t0.Add(-2 * time.Hour) }, &bytes.Buffer{}, &http.Client{Transport: r2}); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"ntfy.private.test", "events.pagerduty.test"} {
		got := r2.to(host)
		if count(got, "Engine halted since") != 1 || count(got, "order refused") != 1 || count(got, "state at reporter start") != 0 {
			t.Errorf("%s: events after the channel's baseline are sent as they are: %q", host, got)
		}
	}
}

// TestAKillSwitchEngagedBeforeTheBaselineIsPaged: the kill switch is in
// force until released, so a channel that begins after its engagement is
// told it, though its one refusal alert is history to that channel (ADR
// 0026, point 3).
func TestAKillSwitchEngagedBeforeTheBaselineIsPaged(t *testing.T) {
	withBaseline(t)
	entries := haltedEntries(t)[:3]
	entries = append(entries, journal.Entry{Kind: journal.KindInput, Envelope: envelope(t, 4, event.KillSwitchEventType, event.KillSwitchSchemaVersion, "kill", t0,
		event.KillSwitchPayload{AccountID: "joint", AsOf: t0, Engaged: true, Operator: "owner", Detail: "by hand"})})
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	args := []string{"-journal", writeRun(t, dir, entries), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty"}
	if err := run(args, twoChannels(), func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, &http.Client{Transport: r}); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"ntfy.private.test", "events.pagerduty.test"} {
		if got := r.to(host); count(got, "kill switch is engaged") != 1 {
			t.Errorf("%s was not told the kill switch is engaged: %q", host, got)
		}
	}
}

// TestTheBaselineIsOnDiskBeforeAnyDelivery: a run killed during its first
// send (systemd stops one at TimeoutStartSec) leaves the baseline it took, so
// the next run does not take a later one and drop what the log recorded
// between the two (ADR 0019, "Alerting": each incident once).
func TestTheBaselineIsOnDiskBeforeAnyDelivery(t *testing.T) {
	withBaseline(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "s.json")
	args := []string{"-journal", writeJournal(t, dir), "-state", state, "-channel", "ntfy"}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": "https://ntfy.example.test"})
	// The first run begins before the log's events, and is killed at its
	// first send: what it leaves is the state file as it stood then.
	killed := &killedAt{state: state, left: filepath.Join(dir, "left.json"), at: 1}
	if err := run(args, environment, func() time.Time { return t0.Add(-time.Minute) }, &bytes.Buffer{}, &http.Client{Transport: killed}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(killed.left)
	if err != nil || !strings.Contains(string(raw), `"baselines"`) {
		t.Fatalf("at the first send the state file held no baseline: %q, %v", raw, err)
	}
	if err := os.WriteFile(state, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	resumed := &killedAt{state: filepath.Join(dir, "unused.json"), left: filepath.Join(dir, "unused-left.json")}
	if err := run(args, environment, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, &http.Client{Transport: resumed}); err != nil {
		t.Fatal(err)
	}
	if count(resumed.sends, "order refused") != 1 {
		t.Fatalf("the refusal recorded after the killed run's baseline was dropped: %q", resumed.sends)
	}
}

// TestAChannelAddedToAnExistingStateFileReplaysNoHistory: each channel has
// its own record, so a channel named later starts from then: it is sent the
// account's standing and the open halt, not the routine history, and the
// channel that already took the history is not sent it again.
func TestAChannelAddedToAnExistingStateFileReplaysNoHistory(t *testing.T) {
	withBaseline(t)
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	journalPath := writeJournal(t, dir)
	state := filepath.Join(dir, "s.json")
	if err := run([]string{"-journal", journalPath, "-state", state, "-channel", "ntfy"}, twoChannels(),
		func() time.Time { return t0.Add(-time.Minute) }, &bytes.Buffer{}, &http.Client{Transport: r}); err != nil {
		t.Fatal(err)
	}
	took := len(r.to("ntfy.private.test"))
	if took == 0 {
		t.Fatal("the first channel took nothing")
	}
	if err := run([]string{"-journal", journalPath, "-state", state, "-channel", "ntfy,pagerduty"}, twoChannels(),
		func() time.Time { return t0.Add(time.Minute) }, &bytes.Buffer{}, &http.Client{Transport: r}); err != nil {
		t.Fatal(err)
	}
	if got := r.to("events.pagerduty.test"); count(got, "order refused") != 0 || count(got, "state at reporter start: Halted") != 1 || count(got, "Engine halted since") != 1 {
		t.Errorf("a channel added later must take the standing and the open halt, and no history: %v", got)
	}
	if got := len(r.to("ntfy.private.test")); got != took {
		t.Errorf("the channel that had the history was sent %d more: %q", got-took, r.to("ntfy.private.test"))
	}
}

// TestAStateFileFromBeforeBaselinesKeepsItsChannelsWhole: a channel that has
// taken alerts under a state file with no baselines is not new, so nothing is
// held back from it.
func TestAStateFileFromBeforeBaselinesKeepsItsChannelsWhole(t *testing.T) {
	withBaseline(t)
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	state := filepath.Join(dir, "s.json")
	if err := os.WriteFile(state, []byte(`{"keys":["ntfy|refusal:somebody-else"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-journal", writeJournal(t, dir), "-state", state, "-channel", "ntfy"}
	if err := run(args, twoChannels(), func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, &http.Client{Transport: r}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(r.to("ntfy.private.test"), "\n"), "engine halted") {
		t.Fatal("a channel with a record was held back from an alert it had not taken")
	}
}

// TestWhatAChannelMissedWhileSetAsideReachesItOnceItWorks: a channel whose
// secret is bad is baselined when it is first named, so the alerts raised
// after that reach it once it works, as they do for the others.
func TestWhatAChannelMissedWhileSetAsideReachesItOnceItWorks(t *testing.T) {
	withBaseline(t)
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	journalPath := writeJournal(t, dir)
	state := filepath.Join(dir, "s.json")
	args := []string{"-journal", journalPath, "-state", state, "-channel", "ntfy,pagerduty"}
	bad := env(map[string]string{"TI_NTFY_TOPIC": "", "TI_PAGERDUTY_ROUTING_KEY": "k", "TI_PAGERDUTY_URL": "https://events.pagerduty.test/v2/enqueue"})
	if err := run(args, bad, func() time.Time { return t0.Add(-time.Hour) }, &bytes.Buffer{}, &http.Client{Transport: r}); err == nil {
		t.Fatal("a channel with no topic must fail the run")
	}
	if err := run(args, twoChannels(), func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, &http.Client{Transport: r}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(r.to("ntfy.private.test"), "\n"), "engine halted") {
		t.Fatal("the alert raised while the channel was set aside never reached it")
	}
}
