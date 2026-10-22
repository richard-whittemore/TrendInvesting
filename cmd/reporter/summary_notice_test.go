package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// resolvesFail is a relay whose PagerDuty refuses every resolve.
type resolvesFail struct{ relay }

func (r *resolvesFail) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	if strings.Contains(string(body), `"event_action":"resolve"`) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Status: "503", Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: req}, nil
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	return r.relay.RoundTrip(req)
}

// TestRestoringTheSummaryRouteResolvesTheNoticesIncident: PagerDuty folds a
// trigger into an incident under the same key, and one acknowledged on the
// phone takes no further triggers, so the notice is resolved on each
// PagerDuty channel that took it before it is forgotten; a channel whose
// resolve fails keeps its record and is tried again (ADR 0019, "Alerting":
// once per incident; clock.go, resolveAll).
func TestRestoringTheSummaryRouteResolvesTheNoticesIncident(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	journalPath := writeJournal(t, dir)
	state := filepath.Join(dir, "s.json")
	now := func() time.Time { return t0.Add(time.Hour) }
	runWith := func(channels string, client http.RoundTripper) error {
		t.Helper()
		return run([]string{"-journal", journalPath, "-state", state, "-channel", channels, "-summary"}, twoChannels(), now, &bytes.Buffer{}, &http.Client{Transport: client})
	}
	if err := runWith("pagerduty", r); err != nil {
		t.Fatal(err)
	}
	failing := &resolvesFail{relay: relay{sent: map[string][]string{}}}
	if err := runWith("pagerduty,ntfy", failing); err == nil || !strings.Contains(err.Error(), "resolve") {
		t.Fatalf("a failed resolve of the notice must fail the run: %v", err)
	}
	if err := runWith("pagerduty,ntfy", r); err != nil {
		t.Fatal(err)
	}
	resolves := 0
	for _, m := range r.to("events.pagerduty.test") {
		if strings.Contains(m, `"event_action":"resolve"`) && strings.Contains(m, `"dedup_key":"summary-no-channel:`) {
			resolves++
		}
	}
	if resolves != 1 {
		t.Errorf("the notice's incident was resolved %d time(s) once the route was restored, want once after the failed try", resolves)
	}
	if err := runWith("pagerduty,ntfy", r); err != nil {
		t.Fatal(err)
	}
	if n := count(r.to("events.pagerduty.test"), `"event_action":"resolve"`); n != 1 {
		t.Errorf("the notice was resolved %d times; once resolved it is forgotten", n)
	}
}

// TestTheNoChannelNoticeIsKeyedByTheAccountAlone: the notice is the account's
// and is said once, so a run whose journal is refused, which cannot name the
// account, does not say it under the file's name, and the readable run that
// follows says it once (ADR 0019, "Alerting": once per incident).
func TestTheNoChannelNoticeIsKeyedByTheAccountAlone(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	journalPath := filepath.Join(dir, "joint.journal")
	if err := os.WriteFile(journalPath, []byte("not a journal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "s.json")
	now := func() time.Time { return t0.Add(time.Hour) }
	args := []string{"-journal", journalPath, "-state", state, "-channel", "pagerduty", "-summary"}
	_ = run(args, twoChannels(), now, &bytes.Buffer{}, &http.Client{Transport: r})
	writeJournal(t, dir)
	if err := run(args, twoChannels(), now, &bytes.Buffer{}, &http.Client{Transport: r}); err != nil {
		t.Fatal(err)
	}
	if n := count(r.to("events.pagerduty.test"), "the daily summary has no channel"); n != 1 {
		t.Errorf("the notice was sent %d times across a refused and a readable run, want once:\n%s", n, strings.Join(r.to("events.pagerduty.test"), "\n"))
	}
	if n := count(r.to("events.pagerduty.test"), "joint.journal: the daily summary has no channel"); n != 0 {
		t.Error("the notice was titled by the journal file's name, not the account")
	}
}
