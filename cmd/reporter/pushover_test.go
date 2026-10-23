package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// pushover is a fake Pushover messages API: it records the form each message
// was posted with and answers as the real one does.
type pushover struct {
	mu     sync.Mutex
	status int
	body   string
	path   []string
	got    []url.Values
}

func (p *pushover) handler(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.path = append(p.path, r.URL.Path+" "+r.Header.Get("Content-Type"))
	p.got = append(p.got, r.PostForm)
	w.WriteHeader(p.status)
	_, _ = w.Write([]byte(p.body))
}

func (p *pushover) answer(status int, body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status, p.body, p.got, p.path = status, body, nil, nil
}

func (p *pushover) received() []url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]url.Values(nil), p.got...)
}

const pushoverOK = `{"status":1,"request":"647d2300-702c-8c38-5a97-5a2db0d2c1f7","receipt":"r"}`

// pushoverEnv configures the channel against a fake, with the two secrets
// the tests must never see in any output.
func pushoverEnv(server *httptest.Server) func(string) string {
	return env(map[string]string{"TI_PUSHOVER_APP_TOKEN": "app-token-secret-3", "TI_PUSHOVER_USER_KEY": "user-key-secret-8", "TI_PUSHOVER_URL": server.URL})
}

func TestPushoverPostsTheDocumentedFormFieldsAndPagesAtEmergencyPriority(t *testing.T) {
	po := &pushover{status: http.StatusOK, body: pushoverOK}
	server := httptest.NewServer(http.HandlerFunc(po.handler))
	defer server.Close()
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "pushover"}
	var out bytes.Buffer
	if err := run(args, pushoverEnv(server), func() time.Time { return t0.Add(time.Hour) }, &out, server.Client()); err != nil {
		t.Fatal(err)
	}
	got := po.received()
	if len(got) == 0 {
		t.Fatal("nothing was sent")
	}
	paged := false
	for _, form := range got {
		if form.Get("token") != "app-token-secret-3" || form.Get("user") != "user-key-secret-8" || form.Get("title") == "" || form.Get("message") == "" {
			t.Fatalf("form = %v", form)
		}
		switch form.Get("priority") {
		case "2":
			paged = true
			if form.Get("retry") != "60" || form.Get("expire") != "1800" {
				t.Errorf("a page must carry retry 60 and expire 1800, got %v", form)
			}
		case "1":
			if form.Has("retry") || form.Has("expire") {
				t.Errorf("only an emergency carries retry and expire, got %v", form)
			}
		default:
			t.Errorf("priority = %q, want 2 for a page and 1 otherwise", form.Get("priority"))
		}
	}
	if !paged {
		t.Errorf("the journal's halt was not paged at emergency priority: %v", got)
	}
	if !strings.HasPrefix(po.path[0], "/ application/x-www-form-urlencoded") {
		t.Errorf("request = %q, want a form post", po.path[0])
	}
	if strings.Contains(out.String(), "app-token-secret-3") || strings.Contains(out.String(), "user-key-secret-8") {
		t.Fatalf("a secret was printed:\n%s", out.String())
	}
}

func TestPushoverCarriesWhatThePagerDutyPayloadCarries(t *testing.T) {
	po := &pushover{status: http.StatusOK, body: pushoverOK}
	server := httptest.NewServer(http.HandlerFunc(po.handler))
	defer server.Close()
	sender := pushoverSender{url: server.URL, token: "t", user: "u", client: server.Client()}
	a := report.Alert{Key: "refusal:short", AccountID: "joint", Severity: report.Urgent, Title: "joint: halted", Body: "the engine halted"}
	if err := sender.send(a, 0); err != nil {
		t.Fatal(err)
	}
	form := po.received()[0]
	if form.Get("title") != "joint: halted" || !strings.Contains(form.Get("message"), "the engine halted") || !strings.Contains(form.Get("message"), "refusal:short") {
		t.Errorf("form = %v, want the title, the detail and the alert key", form)
	}
}

func TestPushoverNonPageIsHighPriorityAndMessagesStayWithinPushoversLimits(t *testing.T) {
	po := &pushover{status: http.StatusOK, body: pushoverOK}
	server := httptest.NewServer(http.HandlerFunc(po.handler))
	defer server.Close()
	sender := pushoverSender{url: server.URL, token: "t", user: "u", client: server.Client()}
	long := strings.Repeat("é", 2000)
	for _, a := range []report.Alert{
		{Key: "k1", Severity: report.Routine, Title: "short", Body: "b"},
		{Key: "k2", Severity: report.Urgent, Title: long, Body: long},
	} {
		if err := sender.send(a, 0); err != nil {
			t.Fatal(err)
		}
	}
	got := po.received()
	if len(got) != 2 {
		t.Fatalf("%d messages, want one per alert: a long alert must never become several emergencies", len(got))
	}
	if got[0].Get("priority") != "1" {
		t.Errorf("a non-page priority = %q, want 1", got[0].Get("priority"))
	}
	if got[1].Get("priority") != "2" {
		t.Errorf("a page priority = %q, want 2", got[1].Get("priority"))
	}
	for _, f := range got {
		if n := utf8.RuneCountInString(f.Get("message")); n > 1024 || !utf8.ValidString(f.Get("message")) {
			t.Errorf("message is %d characters, Pushover refuses over 1024", n)
		}
		if n := utf8.RuneCountInString(f.Get("title")); n > 250 || !utf8.ValidString(f.Get("title")) {
			t.Errorf("title is %d characters, Pushover refuses over 250", n)
		}
	}
}

func TestPushoverWithoutEitherSecretIsAConfigurationErrorNamingTheVariable(t *testing.T) {
	for name, tc := range map[string]struct {
		set  map[string]string
		want string
	}{
		"no token": {map[string]string{"TI_PUSHOVER_USER_KEY": "user-key-secret-8"}, "TI_PUSHOVER_APP_TOKEN"},
		"no user":  {map[string]string{"TI_PUSHOVER_APP_TOKEN": "app-token-secret-3"}, "TI_PUSHOVER_USER_KEY"},
		"bad url":  {map[string]string{"TI_PUSHOVER_APP_TOKEN": "app-token-secret-3", "TI_PUSHOVER_USER_KEY": "user-key-secret-8", "TI_PUSHOVER_URL": "::not a url"}, "TI_PUSHOVER_URL"},
	} {
		dir := t.TempDir()
		args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "pushover"}
		err := run(args, env(tc.set), time.Now, &bytes.Buffer{}, http.DefaultClient)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret-") {
			t.Errorf("%s: err = %v, want one naming %s and no value", name, err, tc.want)
		}
	}
}

func TestPushoverAnswerThatIsNotAcceptedIsNotADeliveryAndLeaksNothing(t *testing.T) {
	for name, po := range map[string]*pushover{
		"http error":     {status: http.StatusInternalServerError, body: `{"status":0,"errors":["app-token-secret-3 user-key-secret-8"]}`},
		"invalid":        {status: http.StatusBadRequest, body: `{"status":0,"errors":["application token is invalid"]}`},
		"ok but refused": {status: http.StatusOK, body: `{"status":0,"errors":["user key is invalid"]}`},
		"unreadable":     {status: http.StatusOK, body: `not json`},
	} {
		server := httptest.NewServer(http.HandlerFunc(po.handler))
		dir := t.TempDir()
		args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "pushover"}
		err := run(args, pushoverEnv(server), func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, server.Client())
		server.Close()
		if err == nil || strings.Contains(err.Error(), "secret-") {
			t.Errorf("%s: error = %v", name, err)
		}
		if raw, readErr := os.ReadFile(filepath.Join(dir, "s.json")); readErr == nil && strings.Contains(string(raw), channelKey("pushover", "")) {
			t.Errorf("%s: recorded as delivered: %s", name, raw)
		}
	}
}

func TestAFailedPushoverDeliveryIsRetriedAndOpensOneEmergencyPerAcceptedPage(t *testing.T) {
	po := &pushover{status: http.StatusInternalServerError, body: `{"status":0}`}
	server := httptest.NewServer(http.HandlerFunc(po.handler))
	defer server.Close()
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "pushover"}
	now := func() time.Time { return t0.Add(time.Hour) }
	if err := run(args, pushoverEnv(server), now, &bytes.Buffer{}, server.Client()); err == nil {
		t.Fatal("a refused message is not a delivery")
	}
	po.answer(http.StatusOK, pushoverOK)
	if err := run(args, pushoverEnv(server), now, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatal(err)
	}
	first := len(po.received())
	if first == 0 {
		t.Fatal("the failed delivery was not retried")
	}
	if err := run(args, pushoverEnv(server), now, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatal(err)
	}
	if len(po.received()) != first {
		t.Errorf("an accepted page was sent again: %d messages after the third run, %d after the second", len(po.received()), first)
	}
}

func TestPushoverNeverTakesTheDailySummary(t *testing.T) {
	po := &pushover{status: http.StatusOK, body: pushoverOK}
	server := httptest.NewServer(http.HandlerFunc(po.handler))
	defer server.Close()
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "pushover", "-summary"}
	if err := run(args, pushoverEnv(server), func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatal(err)
	}
	for _, form := range po.received() {
		// The notice that no channel takes the summary is routine and carries no balance.
		if title := form.Get("title"); strings.Contains(title, "daily summary") && !strings.Contains(title, "has no channel") {
			t.Errorf("the daily summary was sent through Pushover: %v", form)
		}
	}
}
