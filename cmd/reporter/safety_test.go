package main

import (
	"bytes"
	"errors"
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

func TestADeliveryThatHangsIsCutOffAndRetriedNextRun(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	dir := t.TempDir()
	state := filepath.Join(dir, "s.json")
	args := []string{"-journal", writeJournal(t, dir), "-state", state, "-channel", "ntfy", "-heartbeat-timeout", "1000h", "-send-timeout", "100ms"}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server.URL})
	done := make(chan error, 1)
	go func() {
		done <- run(args, environment, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, http.DefaultClient)
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "ntfy:") {
			t.Fatalf("a send that never answered must fail as that channel's failure, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled channel held the run: no later alert, the heartbeat's included, could go out")
	}
	raw, _ := os.ReadFile(state)
	if strings.Contains(string(raw), channelKey("ntfy", "")) {
		t.Fatalf("recorded as delivered: %s", raw)
	}
}

func TestTheSendTimeoutMustBePositive(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-send-timeout", "0s"}
	if err := run(args, fakeEnv, time.Now, &bytes.Buffer{}, fakeClient); err == nil || !strings.Contains(err.Error(), "-send-timeout must be positive") {
		t.Fatalf("an unbounded send must be refused, got %v", err)
	}
}

func TestAStateWriteThatIsNotDurableIsAnErrorAndKeepsTheOldState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	if err := saveState(path, map[string]bool{"ntfy|old": true}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	failing := errors.New("disk said no")
	saved := syncFile
	syncFile = func(*os.File) error { return failing }
	defer func() { syncFile = saved }()
	if err := saveState(path, map[string]bool{"ntfy|new": true}); !errors.Is(err, failing) {
		t.Fatalf("a state file not synced to disk can come back empty after a power cut, so it must fail: %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatalf("the old state was replaced by one not on disk: %s", after)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("a temporary file was left behind: %v", entries)
	}
}

// relay is a client whose every request is answered here, recording what
// would have gone to which host. PagerDuty's answer is an acknowledgement.
type relay struct {
	mu   sync.Mutex
	sent map[string][]string
}

func (r *relay) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.sent[req.URL.Host] = append(r.sent[req.URL.Host], req.Header.Get("Title")+"\n"+string(body))
	r.mu.Unlock()
	status, answer := http.StatusOK, ""
	if strings.Contains(req.URL.Host, "pagerduty") {
		status, answer = http.StatusAccepted, `{"status":"success"}`
	}
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(answer)), Header: http.Header{}, Request: req}, nil
}

func (r *relay) to(host string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent[host]
}

func TestTheDailySummaryNeverGoesThroughThePublicRelay(t *testing.T) {
	for name, server := range map[string]string{"default server": "", "named public server": "https://ntfy.sh/"} {
		t.Run(name, func(t *testing.T) {
			r := &relay{sent: map[string][]string{}}
			client := &http.Client{Transport: r}
			dir := t.TempDir()
			args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty", "-summary", "-heartbeat-timeout", "1000h"}
			environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": server, "TI_PAGERDUTY_ROUTING_KEY": "k", "TI_PAGERDUTY_URL": "https://events.pagerduty.test/v2/enqueue"})
			if err := run(args, environment, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, client); err != nil {
				t.Fatalf("the summary reached PagerDuty, a private channel: %v", err)
			}
			public := r.to("ntfy.sh")
			if len(public) == 0 {
				t.Fatal("the incident alerts must still go to ntfy")
			}
			for _, m := range public {
				if strings.Contains(m, "daily summary") || strings.Contains(m, "Net liquidation") {
					t.Fatalf("the summary, with balances, went to the public relay:\n%s", m)
				}
			}
			if got := strings.Join(r.to("events.pagerduty.test"), "\n"); !strings.Contains(got, "daily summary") {
				t.Fatalf("the summary did not reach PagerDuty:\n%s", got)
			}
		})
	}
}

func TestASummaryWithOnlyThePublicRelayIsRefusedButTheAlertsStillGo(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	state := filepath.Join(dir, "s.json")
	args := []string{"-journal", writeJournal(t, dir), "-state", state, "-channel", "ntfy", "-summary", "-heartbeat-timeout", "1000h"}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t"})
	err := run(args, environment, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, &http.Client{Transport: r})
	if err == nil || !strings.Contains(err.Error(), "public relay") {
		t.Fatalf("a summary no private channel took must be an error, got %v", err)
	}
	sent := strings.Join(r.to("ntfy.sh"), "\n")
	if !strings.Contains(sent, "engine halted") || strings.Contains(sent, "daily summary") {
		t.Fatalf("alerts must go and the summary must not:\n%s", sent)
	}
	raw, _ := os.ReadFile(state)
	if strings.Contains(string(raw), "summary:") {
		t.Fatalf("the summary was recorded as delivered: %s", raw)
	}
}
