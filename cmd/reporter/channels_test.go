package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ntfyRecorder is a fake ntfy server that keeps every title and body it is
// posted.
type ntfyRecorder struct {
	mu     sync.Mutex
	titles []string
	bodies []string
}

func (n *ntfyRecorder) handler(_ http.ResponseWriter, r *http.Request) {
	var b bytes.Buffer
	_, _ = b.ReadFrom(r.Body)
	n.mu.Lock()
	defer n.mu.Unlock()
	n.titles = append(n.titles, r.Header.Get("Title"))
	n.bodies = append(n.bodies, b.String())
}

func (n *ntfyRecorder) posted() (titles, bodies []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.titles...), append([]string(nil), n.bodies...)
}

// pagerDutySummaries are the summaries and details the fake Events API took.
func pagerDutySummaries(pd *pagerDuty) (summaries, details []string) {
	for _, m := range pd.received() {
		payload, _ := m["payload"].(map[string]any)
		summary, _ := payload["summary"].(string)
		custom, _ := payload["custom_details"].(map[string]any)
		detail, _ := custom["detail"].(string)
		summaries, details = append(summaries, summary), append(details, detail)
	}
	return summaries, details
}

func anyContains(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// TestOneChannelsBadSecretNeverSilencesTheOther: each delivery channel is
// validated on its own (ADR 0019, "Alerting": an alert is a safety
// requirement), so an empty or malformed secret sets that one channel aside:
// the run fails naming it, the other channel still delivers every alert, an
// urgent alert on that channel names the one set aside, and the daily summary
// says which channel is not delivering.
func TestOneChannelsBadSecretNeverSilencesTheOther(t *testing.T) {
	now := func() time.Time { return t0.Add(time.Hour) }
	t.Run("empty ntfy topic, PagerDuty delivers", func(t *testing.T) {
		pd := &pagerDuty{status: http.StatusAccepted, body: `{"status":"success"}`}
		server := httptest.NewServer(http.HandlerFunc(pd.handler))
		defer server.Close()
		dir := t.TempDir()
		args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty", "-summary"}
		environment := env(map[string]string{"TI_NTFY_TOPIC": "", "TI_PAGERDUTY_ROUTING_KEY": "k", "TI_PAGERDUTY_URL": server.URL})
		err := run(args, environment, now, &bytes.Buffer{}, server.Client())
		if err == nil || !strings.Contains(err.Error(), "ntfy") || !strings.Contains(err.Error(), "TI_NTFY_TOPIC") {
			t.Fatalf("err = %v, want one naming the ntfy channel and its secret", err)
		}
		summaries, details := pagerDutySummaries(pd)
		if !anyContains(summaries, "engine halted") || !anyContains(summaries, "order refused") {
			t.Errorf("PagerDuty did not take the journal's alerts: %v", summaries)
		}
		if !anyContains(summaries, "the reporter cannot deliver to ntfy") {
			t.Errorf("no alert names the channel set aside: %v", summaries)
		}
		summary := ""
		for i, s := range summaries {
			if strings.Contains(s, "daily summary") {
				summary = details[i]
			}
		}
		if !strings.Contains(summary, "Channels not delivering: ntfy") {
			t.Errorf("the daily summary does not name the channel set aside:\n%s", summary)
		}
	})
	for name, environment := range map[string]map[string]string{
		"empty routing key":  {"TI_PAGERDUTY_ROUTING_KEY": ""},
		"malformed endpoint": {"TI_PAGERDUTY_ROUTING_KEY": "routing-secret-9", "TI_PAGERDUTY_URL": "::not a url"},
	} {
		t.Run(name+", ntfy delivers", func(t *testing.T) {
			n := &ntfyRecorder{}
			server := httptest.NewServer(http.HandlerFunc(n.handler))
			defer server.Close()
			environment["TI_NTFY_TOPIC"], environment["TI_NTFY_SERVER"] = "t", server.URL
			dir := t.TempDir()
			args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "pagerduty,ntfy"}
			err := run(args, env(environment), now, &bytes.Buffer{}, server.Client())
			if err == nil || !strings.Contains(err.Error(), "pagerduty") || strings.Contains(err.Error(), "routing-secret-9") {
				t.Fatalf("err = %v, want one naming the pagerduty channel and never its key", err)
			}
			titles, _ := n.posted()
			if !anyContains(titles, "engine halted") || !anyContains(titles, "the reporter cannot deliver to pagerduty") {
				t.Errorf("ntfy took %v", titles)
			}
		})
	}
	t.Run("malformed ntfy server, PagerDuty delivers", func(t *testing.T) {
		pd := &pagerDuty{status: http.StatusAccepted, body: `{"status":"success"}`}
		server := httptest.NewServer(http.HandlerFunc(pd.handler))
		defer server.Close()
		dir := t.TempDir()
		args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty"}
		environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": "not a url", "TI_PAGERDUTY_ROUTING_KEY": "k", "TI_PAGERDUTY_URL": server.URL})
		if err := run(args, environment, now, &bytes.Buffer{}, server.Client()); err == nil || !strings.Contains(err.Error(), "TI_NTFY_SERVER") {
			t.Fatalf("err = %v", err)
		}
		if summaries, _ := pagerDutySummaries(pd); !anyContains(summaries, "engine halted") || !anyContains(summaries, "the reporter cannot deliver to ntfy") {
			t.Errorf("PagerDuty took %v", summaries)
		}
	})
	t.Run("unknown channel beside a good one", func(t *testing.T) {
		n := &ntfyRecorder{}
		server := httptest.NewServer(http.HandlerFunc(n.handler))
		defer server.Close()
		dir := t.TempDir()
		args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "pigeon,ntfy"}
		environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL})
		if err := run(args, environment, now, &bytes.Buffer{}, server.Client()); err == nil || !strings.Contains(err.Error(), "unknown channel") {
			t.Fatalf("err = %v", err)
		}
		if titles, _ := n.posted(); !anyContains(titles, "engine halted") || !anyContains(titles, "the reporter cannot deliver to pigeon") {
			t.Errorf("ntfy took %v", titles)
		}
	})
}

// TestTheChannelSetAsideIsAlertedOnceAndTheRestDeliveredOnce: the alert
// naming a channel set aside is keyed by the channel and its problem, so a
// later run with the same problem sends it no more, and the alerts the
// working channel took are not sent again either.
func TestTheChannelSetAsideIsAlertedOnceAndTheRestDeliveredOnce(t *testing.T) {
	n := &ntfyRecorder{}
	server := httptest.NewServer(http.HandlerFunc(n.handler))
	defer server.Close()
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty"}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL})
	now := func() time.Time { return t0.Add(time.Hour) }
	if err := run(args, environment, now, &bytes.Buffer{}, server.Client()); err == nil {
		t.Fatal("a channel set aside must fail the run")
	}
	first, _ := n.posted()
	if err := run(args, environment, now, &bytes.Buffer{}, server.Client()); err == nil {
		t.Fatal("a channel still set aside must fail the run again")
	}
	if again, _ := n.posted(); len(again) != len(first) {
		t.Errorf("the second run sent %v again", again[len(first):])
	}
	// The secret is fixed: the channel that was set aside takes everything it
	// missed, and the working channel takes nothing again.
	pd := &pagerDuty{status: http.StatusAccepted, body: `{"status":"success"}`}
	pdServer := httptest.NewServer(http.HandlerFunc(pd.handler))
	defer pdServer.Close()
	fixed := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL, "TI_PAGERDUTY_ROUTING_KEY": "k", "TI_PAGERDUTY_URL": pdServer.URL})
	if err := run(args, fixed, now, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatalf("the repaired run failed: %v", err)
	}
	summaries, _ := pagerDutySummaries(pd)
	if !anyContains(summaries, "engine halted") || !anyContains(summaries, "refused") {
		t.Errorf("the repaired channel took %v, want the alerts it missed", summaries)
	}
	if after, _ := n.posted(); len(after) != len(first) {
		t.Errorf("the working channel took %v again", after[len(first):])
	}
}

// TestAnUnreadableStateFileStillNamesTheChannelsSetAside: the run's error
// carries every fault found before the state was read, not only the state's
// (ADR 0019, "Alerting").
func TestAnUnreadableStateFileStillNamesTheChannelsSetAside(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "s.json")
	if err := os.WriteFile(state, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-journal", writeJournal(t, dir), "-state", state, "-channel", "ntfy,pagerduty"}
	err := run(args, env(map[string]string{"TI_NTFY_TOPIC": "t"}), time.Now, &bytes.Buffer{}, http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "TI_PAGERDUTY_ROUTING_KEY") || !strings.Contains(err.Error(), "state") {
		t.Fatalf("err = %v, want the state fault and the channel set aside", err)
	}
}

// TestNoChannelThatCanDeliverStopsTheRunAndRecordsNothing: when every
// delivery channel named is set aside, nothing can reach a person, so the run
// fails naming each, and records nothing as delivered.
func TestNoChannelThatCanDeliverStopsTheRunAndRecordsNothing(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "s.json")
	args := []string{"-journal", writeJournal(t, dir), "-state", state, "-channel", "ntfy,pagerduty"}
	err := run(args, env(nil), time.Now, &bytes.Buffer{}, http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "TI_NTFY_TOPIC") || !strings.Contains(err.Error(), "TI_PAGERDUTY_ROUTING_KEY") || !strings.Contains(err.Error(), "no delivery channel") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(state); statErr == nil {
		t.Fatal("nothing was delivered, so nothing may be recorded as delivered")
	}
}

// TestAMalformedSecretsLineSetsAsideOnlyWhatItLeavesUnset: a line of the
// secrets file that is not KEY=VALUE is reported by its number, never its
// text, which may be a secret; the file's other lines are still read, so a
// channel whose secrets are intact still delivers.
func TestAMalformedSecretsLineSetsAsideOnlyWhatItLeavesUnset(t *testing.T) {
	n := &ntfyRecorder{}
	server := httptest.NewServer(http.HandlerFunc(n.handler))
	defer server.Close()
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets")
	if err := os.WriteFile(secrets, []byte("TI_NTFY_TOPIC=t\nTI_NTFY_SERVER="+server.URL+"\ns3cr3t-without-equals\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty", "-secrets-file", secrets}
	err := run(args, env(nil), func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, server.Client())
	if err == nil || !strings.Contains(err.Error(), "line 3") || strings.Contains(err.Error(), "s3cr3t") || !strings.Contains(err.Error(), "TI_PAGERDUTY_ROUTING_KEY") {
		t.Fatalf("err = %v", err)
	}
	titles, bodies := n.posted()
	if !anyContains(titles, "engine halted") {
		t.Errorf("ntfy took %v", titles)
	}
	if anyContains(bodies, "s3cr3t") || anyContains(titles, "s3cr3t") {
		t.Error("an alert carries the malformed line's text")
	}
}
