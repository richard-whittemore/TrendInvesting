package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The outside heartbeat seam: at the end of every successful firing the
// reporter pings a Healthchecks-style endpoint off this server, whose own
// missed-ping alert pages the owner when the server, the timer or the
// reporter stops (ADR 0019, "Alerting": a heartbeat checked from outside the
// system). The endpoint here is a stand-in that records every request.

// standInEndpoint serves ntfy posts and heartbeat pings on one server, in the
// order they arrive.
type standInEndpoint struct {
	mu         sync.Mutex
	events     []string
	pingStatus int
	ntfyStatus int
	pings      []*http.Request
	pingBodies []string
}

func newStandInEndpoint(t *testing.T) (*standInEndpoint, *httptest.Server) {
	t.Helper()
	e := &standInEndpoint{pingStatus: http.StatusOK, ntfyStatus: http.StatusOK}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		defer e.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/ping/") {
			e.events = append(e.events, "ping")
			e.pings = append(e.pings, r.Clone(r.Context()))
			e.pingBodies = append(e.pingBodies, string(body))
			w.WriteHeader(e.pingStatus)
			return
		}
		e.events = append(e.events, "ntfy")
		w.WriteHeader(e.ntfyStatus)
	}))
	t.Cleanup(server.Close)
	return e, server
}

func (e *standInEndpoint) count(kind string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, k := range e.events {
		if k == kind {
			n++
		}
	}
	return n
}

const heartbeatPath = "/ping/9f1c2a3b-secret-uuid"

func heartbeatRun(t *testing.T, server *httptest.Server, dir string, extra map[string]string, out io.Writer) error {
	t.Helper()
	environment := map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL, "TI_HEARTBEAT_URL": server.URL + heartbeatPath}
	for k, v := range extra {
		environment[k] = v
	}
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy"}
	return run(args, env(environment), func() time.Time { return t0.Add(time.Hour) }, out, server.Client())
}

func TestAHealthyFiringPingsTheOutsideEndpointOnceAfterTheAlertsAreSent(t *testing.T) {
	e, server := newStandInEndpoint(t)
	if err := heartbeatRun(t, server, t.TempDir(), nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if e.count("ping") != 1 {
		t.Fatalf("pings = %d, want 1 (events %v)", e.count("ping"), e.events)
	}
	if e.count("ntfy") == 0 || e.events[len(e.events)-1] != "ping" {
		t.Errorf("the ping must come last, after the alerts: %v", e.events)
	}
}

func TestAFiringWithNoURLConfiguredPingsNothingAndFailsNothing(t *testing.T) {
	e, server := newStandInEndpoint(t)
	if err := heartbeatRun(t, server, t.TempDir(), map[string]string{"TI_HEARTBEAT_URL": ""}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if e.count("ping") != 0 {
		t.Errorf("pinged with no URL: %v", e.events)
	}
}

func TestAFiringThatFailsSendsNoPing(t *testing.T) {
	t.Run("a channel failed", func(t *testing.T) {
		e, server := newStandInEndpoint(t)
		e.ntfyStatus = http.StatusInternalServerError
		if err := heartbeatRun(t, server, t.TempDir(), nil, &bytes.Buffer{}); err == nil {
			t.Fatal("the run should fail when a channel fails")
		}
		if e.count("ping") != 0 {
			t.Errorf("an unhealthy firing pinged: %v", e.events)
		}
	})
	t.Run("the journal cannot be read", func(t *testing.T) {
		e, server := newStandInEndpoint(t)
		args := []string{"-journal", filepath.Join(t.TempDir(), "missing.log"), "-state", filepath.Join(t.TempDir(), "s.json"), "-channel", "ntfy"}
		environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL, "TI_HEARTBEAT_URL": server.URL + heartbeatPath})
		if err := run(args, environment, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, server.Client()); err == nil {
			t.Fatal("an unreadable journal should fail the run")
		}
		if e.count("ping") != 0 {
			t.Errorf("an unhealthy firing pinged: %v", e.events)
		}
	})
}

func TestAPingThatFailsNeverFailsTheRunAndIsLoggedOnceWithoutTheURL(t *testing.T) {
	for name, status := range map[string]int{"server error": http.StatusInternalServerError, "not found": http.StatusNotFound} {
		t.Run(name, func(t *testing.T) {
			e, server := newStandInEndpoint(t)
			e.pingStatus = status
			var out bytes.Buffer
			if err := heartbeatRun(t, server, t.TempDir(), nil, &out); err != nil {
				t.Fatalf("a failed ping failed the run: %v", err)
			}
			if strings.Count(out.String(), "heartbeat") != 1 {
				t.Errorf("want exactly one log line about the ping, got:\n%s", out.String())
			}
			if strings.Contains(out.String(), "secret-uuid") || strings.Contains(out.String(), server.URL) {
				t.Errorf("the log shows the URL:\n%s", out.String())
			}
		})
	}
	t.Run("endpoint unreachable", func(t *testing.T) {
		_, server := newStandInEndpoint(t)
		var out bytes.Buffer
		dead := httptest.NewServer(http.NotFoundHandler())
		deadURL := dead.URL + heartbeatPath
		dead.Close()
		if err := heartbeatRun(t, server, t.TempDir(), map[string]string{"TI_HEARTBEAT_URL": deadURL}, &out); err != nil {
			t.Fatalf("an unreachable endpoint failed the run: %v", err)
		}
		if strings.Count(out.String(), "heartbeat") != 1 || strings.Contains(out.String(), "secret-uuid") || strings.Contains(out.String(), "127.0.0.1") {
			t.Errorf("want one log line without the URL, got:\n%s", out.String())
		}
	})
	t.Run("a URL that is not absolute", func(t *testing.T) {
		_, server := newStandInEndpoint(t)
		var out bytes.Buffer
		if err := heartbeatRun(t, server, t.TempDir(), map[string]string{"TI_HEARTBEAT_URL": "not a url ?key=hunter2"}, &out); err != nil {
			t.Fatalf("a bad URL failed the run: %v", err)
		}
		if strings.Count(out.String(), "heartbeat") != 1 || strings.Contains(out.String(), "hunter2") {
			t.Errorf("want one log line without the URL, got:\n%s", out.String())
		}
	})
}

func TestAHungEndpointIsStoppedAtTheSendTimeoutAndNeverFailsTheRun(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/ping/") {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
	}))
	t.Cleanup(func() { close(release); server.Close() })
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-send-timeout", "200ms"}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL, "TI_HEARTBEAT_URL": server.URL + heartbeatPath})
	start := time.Now()
	if err := run(args, environment, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatalf("a hung ping failed the run: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the ping held the run for %s", time.Since(start))
	}
}

// TestThePingCarriesNothingButTheEndpoint: the request is the configured
// address and nothing else: no body, no query, no header of ours, so no
// balance, account number or secret can ride on it.
func TestThePingCarriesNothingButTheEndpoint(t *testing.T) {
	e, server := newStandInEndpoint(t)
	if err := heartbeatRun(t, server, t.TempDir(), nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(e.pings) != 1 {
		t.Fatalf("pings = %d", len(e.pings))
	}
	p := e.pings[0]
	if p.URL.Path != heartbeatPath || p.URL.RawQuery != "" || e.pingBodies[0] != "" {
		t.Errorf("ping %s ? %s body %q: want only the configured path", p.URL.Path, p.URL.RawQuery, e.pingBodies[0])
	}
	for name := range p.Header {
		switch name {
		case "User-Agent", "Accept-Encoding", "Content-Length":
		default:
			t.Errorf("the ping sends the header %s", name)
		}
	}
}

// TestTheURLComesFromTheSecretsFile: under systemd the secrets arrive as a
// credential file, never in the environment.
func TestTheURLComesFromTheSecretsFile(t *testing.T) {
	e, server := newStandInEndpoint(t)
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets")
	writeFileT(t, secrets, "TI_NTFY_TOPIC=t\nTI_NTFY_SERVER="+server.URL+"\nTI_HEARTBEAT_URL="+server.URL+heartbeatPath+"\n")
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-secrets-file", secrets}
	if err := run(args, env(nil), func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatal(err)
	}
	if e.count("ping") != 1 {
		t.Errorf("pings = %d", e.count("ping"))
	}
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAFiringThatFindsATradingUnitFailedSendsNoPing: a ping means the engine
// and the reporter are healthy, so a firing that finds the engine or the
// adapter failed pings nothing even when its page was delivered; the next
// firing that finds every unit well pings again (ADR 0019, "Alerting": the
// outside heartbeat).
func TestAFiringThatFindsATradingUnitFailedSendsNoPing(t *testing.T) {
	e, server := newStandInEndpoint(t)
	systemd := newStandInSystemd(t)
	systemd.fail("ti-engine@boost.service")
	dir := t.TempDir()
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL, "TI_HEARTBEAT_URL": server.URL + heartbeatPath})
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy",
		"-units", "ti-engine@boost.service,ti-adapter-boost.service", "-systemctl", systemd.systemctl(), "-journalctl", systemd.journalctl()}
	if err := run(args, environment, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatal(err)
	}
	if e.count("ntfy") == 0 {
		t.Fatalf("the failed engine was not paged: %v", e.events)
	}
	if e.count("ping") != 0 {
		t.Errorf("a firing that found the engine failed pinged: %v", e.events)
	}
	systemd.fail()
	if err := run(args, environment, func() time.Time { return t0.Add(time.Hour + 5*time.Minute) }, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatal(err)
	}
	if e.count("ping") != 1 {
		t.Errorf("the firing after the engine was well again did not ping: %v", e.events)
	}
}
