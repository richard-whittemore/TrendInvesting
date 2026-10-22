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
// server of the owner's own, PagerDuty does not take the summary.
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
	if got := strings.Join(r.to("events.pagerduty.test"), "\n"); strings.Contains(got, "daily summary") || !strings.Contains(got, "engine halted") {
		t.Errorf("PagerDuty must take incidents and no summary:\n%s", got)
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

// TestANewStateFileReplaysNoHistory: a state file that is new or lost leaves
// each channel with no record, so it starts at the run's own time: the log's
// earlier alerts are not sent to it, and an event after that time is.
func TestANewStateFileReplaysNoHistory(t *testing.T) {
	withBaseline(t)
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	journalPath := writeJournal(t, dir)
	state := filepath.Join(dir, "s.json")
	args := []string{"-journal", journalPath, "-state", state, "-channel", "ntfy,pagerduty"}

	var out bytes.Buffer
	if err := run(args, twoChannels(), func() time.Time { return t0.Add(time.Hour) }, &out, &http.Client{Transport: r}); err != nil {
		t.Fatal(err)
	}
	if len(r.to("ntfy.private.test"))+len(r.to("events.pagerduty.test")) != 0 || out.Len() != 0 {
		t.Fatalf("the log's history was sent to a channel with no record: ntfy %v, pagerduty %v, echo %q", r.to("ntfy.private.test"), r.to("events.pagerduty.test"), out.String())
	}
	raw, _ := os.ReadFile(state)
	if !strings.Contains(string(raw), `"baselines"`) {
		t.Fatalf("the state file does not record where each channel starts: %s", raw)
	}

	// Run again as if the log's events were after the baseline: a state file
	// begun an hour before them takes them.
	later := t.TempDir()
	fresh := filepath.Join(later, "s.json")
	if err := run([]string{"-journal", journalPath, "-state", fresh, "-channel", "ntfy,pagerduty"}, twoChannels(),
		func() time.Time { return t0.Add(-time.Hour) }, &bytes.Buffer{}, &http.Client{Transport: r}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(r.to("ntfy.private.test"), "\n"), "engine halted") || !strings.Contains(strings.Join(r.to("events.pagerduty.test"), "\n"), "engine halted") {
		t.Fatal("an event after the channel's baseline was not sent")
	}
}

// TestAChannelAddedToAnExistingStateFileReplaysNoHistory: each channel has
// its own record, so a channel named later starts from then, and the channel
// that already took the history is not sent it again.
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
	if got := r.to("events.pagerduty.test"); len(got) != 0 {
		t.Errorf("a channel added later was sent the history: %v", got)
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
