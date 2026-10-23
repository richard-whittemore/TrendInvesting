package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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

func TestALongBodyIsShortenedAndTheAlertKeyAndAccountAreAlwaysKept(t *testing.T) {
	po := &pushover{status: http.StatusOK, body: pushoverOK}
	server := httptest.NewServer(http.HandlerFunc(po.handler))
	defer server.Close()
	ps := &pushoverSender{url: server.URL, token: "t", user: "u", client: server.Client()}
	a := report.Alert{Key: "refusal:short-position", AccountID: "joint", Severity: report.Urgent, Title: "t", Body: strings.Repeat("é", 5000)}
	if err := ps.send(a, 0); err != nil {
		t.Fatal(err)
	}
	msg := po.received()[0].Get("message")
	if n := utf8.RuneCountInString(msg); n > 1024 || !utf8.ValidString(msg) {
		t.Errorf("message is %d characters", n)
	}
	if !strings.HasSuffix(msg, "\n\nAlert: refusal:short-position (joint)") {
		t.Errorf("the message must end with the alert key and account, got ...%q", msg[len(msg)-60:])
	}
	if !strings.Contains(msg, "…") {
		t.Error("the shortened body should end in an ellipsis")
	}
}

func TestAnAbsurdlyLongAlertKeyStillLeavesRoomForTheBody(t *testing.T) {
	a := report.Alert{Key: strings.Repeat("k", 5000), AccountID: strings.Repeat("a", 5000), Body: "the body"}
	msg := pushoverMessage(a)
	if n := utf8.RuneCountInString(msg); n > 1024 || !strings.HasPrefix(msg, "the body") {
		t.Errorf("message = %d characters, starting %q", n, msg[:10])
	}
}

func TestCutRunesGuardsANonPositiveLimit(t *testing.T) {
	for _, limit := range []int{0, -3} {
		if got := cutRunes("abc", limit); got != "" {
			t.Errorf("cutRunes(.., %d) = %q, want empty", limit, got)
		}
	}
	if got := cutRunes("abcdef", 3); got != "ab…" {
		t.Errorf("got %q", got)
	}
}

func TestATruncatedOrOversizedAnswerIsNotADelivery(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"cut off mid-body": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "200")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":1,`))
			w.(http.Flusher).Flush()
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		},
		"complete object but the body is cut short": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "400")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":1}`))
			w.(http.Flusher).Flush()
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		},
		"incomplete object": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":1`))
		},
		"oversized": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":1,"request":"` + strings.Repeat("x", 1<<17) + `"}`))
		},
		"trailing data": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":1} {"status":0}`))
		},
		"extra closing brace": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":1}}`))
		},
		"extra closing bracket": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":1,"receipt":"r"}]`))
		},
		"not an object": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`1`))
		},
	} {
		server := httptest.NewServer(handler)
		ps := &pushoverSender{url: server.URL, token: "t", user: "u", client: server.Client()}
		err := ps.send(report.Alert{Key: "k", Severity: report.Urgent, Title: "t", Body: "b"}, 0)
		server.Close()
		if err == nil {
			t.Errorf("%s: counted as delivered", name)
		}
	}
}

// leaky is a transport whose errors and status text carry the credentials.
type leaky struct {
	fail bool
}

func (l leaky) RoundTrip(r *http.Request) (*http.Response, error) {
	if l.fail {
		return nil, &url.Error{Op: "Post", URL: r.URL.String() + "?token=app-token-secret-3", Err: &leakyErr{}}
	}
	return &http.Response{StatusCode: http.StatusBadGateway, Status: "502 app-token-secret-3 user-key-secret-8", Body: http.NoBody, Header: http.Header{}, Request: r}, nil
}

type leakyErr struct{}

func (*leakyErr) Error() string { return "dial tcp: token=app-token-secret-3 user=user-key-secret-8" }

func TestErrorsNeverCarryTheCredentials(t *testing.T) {
	for name, l := range map[string]leaky{"transport": {fail: true}, "status": {}} {
		ps := &pushoverSender{url: "https://api.pushover.net/1/messages.json", token: "app-token-secret-3", user: "user-key-secret-8", client: &http.Client{Transport: l}}
		for op, err := range map[string]error{
			"send":   ps.send(report.Alert{Key: "k", Severity: report.Urgent, Title: "t", Body: "b"}, 0),
			"cancel": ps.cancel("rcpt1"),
		} {
			if err == nil || strings.Contains(err.Error(), "secret-") || strings.Contains(err.Error(), "pushover.net") {
				t.Errorf("%s %s: error = %v", name, op, err)
			}
		}
	}
	err := (&pushoverSender{url: "https://x.test/1/messages.json", client: &http.Client{Transport: leaky{}}}).send(report.Alert{Key: "k", Title: "t", Body: "b"}, 0)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("a status error should carry the numeric code, got %v", err)
	}
}

// emergency sends a page through a fake whose answer carries a receipt and
// returns the sender, the state and the fake.
func emergency(t *testing.T) (ps *pushoverSender, seen map[string]bool, po *pushover, closeServer func()) {
	t.Helper()
	po = &pushover{status: http.StatusOK, body: `{"status":1,"request":"r","receipt":"rcpt1"}`}
	server := httptest.NewServer(http.HandlerFunc(po.handler))
	ps = &pushoverSender{url: server.URL + "/1/messages.json", token: "app-token-secret-3", user: "user-key-secret-8", client: server.Client()}
	seen = map[string]bool{}
	sent, repeats := map[string]time.Time{}, map[string]int{}
	a := report.Alert{Key: "clock-drift:host", Severity: report.Urgent, Title: "t", Body: "b"}
	if err := deliver([]sender{ps}, a, seen, sent, repeats, t0, time.Hour, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	return ps, seen, po, server.Close
}

func TestResolvingAnIncidentCancelsItsPushoverEmergency(t *testing.T) {
	ps, seen, po, closeServer := emergency(t)
	defer closeServer()
	if len(seen) != 2 {
		t.Fatalf("the state holds %v, want the delivery and the receipt", seen)
	}
	failed, err := resolveAll([]sender{ps}, "clock-drift:host", seen, t0.Add(time.Minute))
	if err != nil || len(failed) != 0 {
		t.Fatalf("resolve = %v, %v", failed, err)
	}
	if got := po.path; len(got) != 2 || !strings.HasPrefix(got[1], "/1/receipts/rcpt1/cancel.json ") {
		t.Fatalf("requests = %v, want the message then the receipt's cancel", got)
	}
	if form := po.received()[1]; form.Get("token") != "app-token-secret-3" || form.Has("user") {
		t.Errorf("cancel form = %v, want the app token alone", form)
	}
	for k := range seen {
		if strings.Contains(k, "rcpt1") {
			t.Errorf("a cancelled receipt is still recorded: %q", k)
		}
	}
}

func TestACancelThatFailsKeepsTheReceiptAndIsTriedAgain(t *testing.T) {
	ps, seen, po, closeServer := emergency(t)
	defer closeServer()
	po.answer(http.StatusInternalServerError, `{"status":0}`)
	failed, err := resolveAll([]sender{ps}, "clock-drift:host", seen, t0.Add(time.Minute))
	if err == nil || strings.Contains(err.Error(), "rcpt1") || len(failed) != 1 {
		t.Fatalf("resolve = %v, %v: want one failure naming no receipt", failed, err)
	}
	keep := map[string]bool{}
	for _, n := range failed {
		keep[n] = true
	}
	forgetAlert("clock-drift:host", keep, seen, map[string]time.Time{}, map[string]int{})
	if !deliveredAnywhere(seen, "clock-drift:host") {
		t.Fatal("the receipt was forgotten with the failed cancel, so it is never retried")
	}
	po.answer(http.StatusOK, `{"status":1}`)
	if failed, err := resolveAll([]sender{ps}, "clock-drift:host", seen, t0.Add(time.Minute)); err != nil || len(failed) != 0 || len(seen) != 0 {
		t.Fatalf("retry = %v, %v, state %v", failed, err, seen)
	}
}

func TestAReceiptPushoverNoLongerKnowsIsNothingLeftToCancel(t *testing.T) {
	ps, seen, po, closeServer := emergency(t)
	defer closeServer()
	po.answer(http.StatusBadRequest, `{"status":0,"errors":["receipt not found"]}`)
	if failed, err := resolveAll([]sender{ps}, "clock-drift:host", seen, t0.Add(time.Minute)); err != nil || len(failed) != 0 {
		t.Fatalf("resolve = %v, %v", failed, err)
	}
	if !seen[channelKey("pushover", "clock-drift:host")] || len(seen) != 1 {
		t.Errorf("state = %v, want only the delivery", seen)
	}
	for k := range seen {
		if strings.HasPrefix(k, "receipt:") {
			t.Errorf("the receipt Pushover no longer knows is still recorded: %q", k)
		}
	}
}

func TestAnAlertThatIsNotAnEmergencyRecordsNoReceipt(t *testing.T) {
	po := &pushover{status: http.StatusOK, body: pushoverOK}
	server := httptest.NewServer(http.HandlerFunc(po.handler))
	defer server.Close()
	ps := &pushoverSender{url: server.URL, token: "t", user: "u", client: server.Client()}
	seen := map[string]bool{}
	a := report.Alert{Key: "k", Severity: report.Routine, Title: "t", Body: "b"}
	if err := deliver([]sender{ps}, a, seen, map[string]time.Time{}, map[string]int{}, t0, time.Hour, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Errorf("state = %v", seen)
	}
}

func TestPushoverEndpointMustBeHTTPSUnlessItIsLoopback(t *testing.T) {
	for url, ok := range map[string]bool{
		"https://api.pushover.net/1/messages.json": true,
		"https://pushover.example.org/m":           true,
		"http://127.0.0.1:8099/1/messages.json":    true,
		"http://localhost:8099/1/messages.json":    true,
		"http://[::1]:8099/1/messages.json":        true,
		"http://pushover.example.org/m":            false,
		"http://10.0.0.5/m":                        false,
		"ftp://127.0.0.1/m":                        false,
		"https://user:pw@pushover.example.org/m":   false,
	} {
		_, err := pushoverFrom(env(map[string]string{"TI_PUSHOVER_APP_TOKEN": "a", "TI_PUSHOVER_USER_KEY": "u", "TI_PUSHOVER_URL": url}), http.DefaultClient)
		if (err == nil) != ok {
			t.Errorf("%s: err = %v, want accepted = %v", url, err, ok)
		}
		if err != nil && strings.Contains(err.Error(), "pw") {
			t.Errorf("%s: the error repeats the address: %v", url, err)
		}
	}
}

func TestPushoverDoesNotFollowARedirectAndItIsAFailure(t *testing.T) {
	var elsewhere []url.Values
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		elsewhere = append(elsewhere, r.PostForm)
		_, _ = w.Write([]byte(pushoverOK))
	}))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	ps := &pushoverSender{url: server.URL, token: "app-token-secret-3", user: "u", client: server.Client()}
	err := ps.send(report.Alert{Key: "k", Severity: report.Urgent, Title: "t", Body: "b"}, 0)
	if err == nil || strings.Contains(err.Error(), "secret-") {
		t.Errorf("a redirect must fail the send without leaking, got %v", err)
	}
	if len(elsewhere) != 0 {
		t.Errorf("the form, with the token, was posted to the redirect target: %v", elsewhere)
	}
}

func TestACancelIsDroppedOnceTheEmergencyHasExpiredAnyway(t *testing.T) {
	ps, seen, po, closeServer := emergency(t)
	defer closeServer()
	po.answer(http.StatusInternalServerError, `{"status":0}`)
	// Still within the emergency's life: the failure is kept for a retry.
	if failed, err := resolveAll([]sender{ps}, "clock-drift:host", seen, t0.Add(10*time.Minute)); err == nil || len(failed) != 1 {
		t.Fatalf("a failed cancel inside the emergency's life must be kept: %v, %v", failed, err)
	}
	// Past the expire time plus the margin: Pushover has stopped it; no request, no record.
	po.answer(http.StatusInternalServerError, `{"status":0}`)
	failed, err := resolveAll([]sender{ps}, "clock-drift:host", seen, t0.Add(pushoverExpire+time.Hour))
	if err != nil || len(failed) != 0 {
		t.Fatalf("an expired receipt is not a failure: %v, %v", failed, err)
	}
	if len(po.received()) != 0 {
		t.Errorf("a cancel was sent for an expired emergency")
	}
	if len(seen) != 1 {
		t.Errorf("state = %v, want only the delivery", seen)
	}
}

// receiptKey is the state-file key of a recorded receipt of the Pushover
// emergency sent at the time, for the alert key.
func receiptKey(receipt string, at time.Time, key string) string {
	return channelKey(receiptChannel("pushover", receipt, at), key)
}

func TestEveryRunDropsReceiptsPastTheirExpiryWhateverTheAlert(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "s.json")
	now := t0.Add(3 * time.Hour)
	old := receiptKey("old", t0, "halted:x")
	fresh := receiptKey("fresh", now.Add(-time.Minute), "halted:x")
	future := receiptKey("future", now.Add(time.Hour), "halted:x")
	raw := `{"keys":["` + old + `","` + fresh + `","` + future + `"],"baselines":{"ntfy":"2000-01-01T00:00:00Z"}}`
	if err := os.WriteFile(state, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeEnv, fakeClient := acceptingNtfy(t)
	args := []string{"-journal", writeJournal(t, dir), "-state", state, "-channel", "stdout,ntfy"}
	if err := run(args, fakeEnv, func() time.Time { return now }, &bytes.Buffer{}, fakeClient); err != nil {
		t.Fatal(err)
	}
	seen, _, _, _, err := loadState(state)
	if err != nil {
		t.Fatal(err)
	}
	if seen[old] {
		t.Errorf("a receipt past its expiry and margin survived a run")
	}
	if !seen[fresh] {
		t.Errorf("a receipt of a live emergency was dropped")
	}
	if !seen[future] {
		t.Errorf("a receipt stamped after the clock now reads was dropped: a clock that went back must not lose a live emergency")
	}
}

func TestPruningKeepsAReceiptStampedAfterNow(t *testing.T) {
	seen := map[string]bool{
		receiptKey("a", t0.Add(time.Hour), "k"):                                 true,
		receiptKey("b", t0.Add(-pushoverExpire-receiptMargin-time.Second), "k"): true,
		receiptKey("c", t0.Add(-pushoverExpire), "k"):                           true,
		"receipt:pushover:malformed|k":                                          true,
		channelKey("pushover", "k"):                                             true,
	}
	pruneReceipts(seen, map[string]time.Time{}, map[string]int{}, t0)
	if len(seen) != 4 || seen[receiptKey("b", t0.Add(-pushoverExpire-receiptMargin-time.Second), "k")] {
		t.Errorf("state = %v: want only the receipt past expiry and margin dropped", seen)
	}
}

func TestTheCancelOfAnUnrecognisedBadRequestKeepsTheReceipt(t *testing.T) {
	for name, body := range map[string]string{
		"invalid token": `{"token":"invalid","errors":["application token is invalid, see https://pushover.net/api"],"status":0,"request":"x"}`,
		"not json":      `<html>bad request</html>`,
		"empty":         ``,
	} {
		ps, seen, po, closeServer := emergency(t)
		po.answer(http.StatusBadRequest, body)
		failed, err := resolveAll([]sender{ps}, "clock-drift:host", seen, t0.Add(time.Minute))
		closeServer()
		if err == nil || len(failed) != 1 {
			t.Errorf("%s: resolve = %v, %v: want a failure", name, failed, err)
		}
		if strings.Contains(fmt.Sprint(err), "invalid, see") || strings.Contains(fmt.Sprint(err), "rcpt1") {
			t.Errorf("%s: the error carries the answer or the receipt: %v", name, err)
		}
		if !seen[receiptKey("rcpt1", t0, "clock-drift:host")] {
			t.Errorf("%s: the receipt was dropped on a 400 that does not say it is unknown", name)
		}
	}
}

func TestAnEmergencyAnsweredWithoutAUsableReceiptIsStillDeliveredButWarnsAndCannotBeCancelled(t *testing.T) {
	for name, body := range map[string]string{
		"absent":    `{"status":1,"request":"r"}`,
		"malformed": `{"status":1,"request":"r","receipt":"bad/receipt-secret"}`,
	} {
		po := &pushover{status: http.StatusOK, body: body}
		server := httptest.NewServer(http.HandlerFunc(po.handler))
		var warn bytes.Buffer
		ps := &pushoverSender{url: server.URL + "/1/messages.json", token: "app-token-secret-3", user: "user-key-secret-8", client: server.Client(), warn: &warn}
		seen := map[string]bool{}
		a := report.Alert{Key: "clock-drift:host", Severity: report.Urgent, Title: "t", Body: "b"}
		if err := deliver([]sender{ps}, a, seen, map[string]time.Time{}, map[string]int{}, t0, time.Hour, func() error { return nil }); err != nil {
			t.Fatalf("%s: an emergency the phone rings for is a delivery: %v", name, err)
		}
		if !seen[channelKey("pushover", "clock-drift:host")] {
			t.Errorf("%s: not recorded as delivered: %v", name, seen)
		}
		if !seen[receiptKey(noReceipt, t0, "clock-drift:host")] {
			t.Errorf("%s: no record that it cannot be cancelled: %v", name, seen)
		}
		if !strings.Contains(warn.String(), "cannot be cancelled") || strings.Contains(warn.String(), "secret") {
			t.Errorf("%s: warning = %q: want a fixed message naming no secret", name, warn.String())
		}
		po.answer(http.StatusOK, pushoverOK)
		if failed, err := resolveAll([]sender{ps}, "clock-drift:host", seen, t0.Add(time.Minute)); err != nil || len(failed) != 0 || len(po.received()) != 0 || len(seen) != 1 {
			t.Errorf("%s: resolve = %v, %v, %d requests, state %v: want no request and the marker gone", name, failed, err, len(po.received()), seen)
		}
		server.Close()
	}
}

func TestAReceiptOutlivesAChannelSetAsideWhileTheIncidentRecovers(t *testing.T) {
	dir := t.TempDir()
	key := report.UnitFailedKey("ti-engine@boost.service", "inv1")
	u := &unitState{path: filepath.Join(dir, "u.json"), seen: map[string]bool{
		channelKey("pushover", key):  true,
		receiptKey("rcpt1", t0, key): true,
	}, sent: map[string]time.Time{}, repeats: map[string]int{}, baselines: map[string]time.Time{}}
	aside := []channelFault{{channel: "pushover", err: errors.New("TI_PUSHOVER_APP_TOKEN missing")}}
	if err := resolveKey(u, key, nil, aside, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !u.seen[receiptKey("rcpt1", t0, key)] {
		t.Fatalf("the receipt was forgotten while its channel was set aside: %v", u.seen)
	}
	// The channel is back at the next firing: the receipt is cancelled then.
	ps, _, po, closeServer := emergency(t)
	defer closeServer()
	po.answer(http.StatusOK, `{"status":1}`)
	if _, err := resolveAll([]sender{ps}, key, u.seen, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(po.received()) != 1 || u.seen[receiptKey("rcpt1", t0, key)] {
		t.Errorf("requests = %d, state %v: want one cancel and the receipt gone", len(po.received()), u.seen)
	}
}

func TestTheMacRehearsalsPushoverStandInPassesTheEndpointRule(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/mac-mini/container-checks.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^forward=(\S+)$`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("the rehearsal names no loopback address for the Pushover stand-in")
	}
	if !secureEndpoint(m[1] + "/1/messages.json") {
		t.Errorf("%s does not pass the endpoint rule", m[1])
	}
	if !strings.Contains(string(raw), "TI_PUSHOVER_URL=$forward/1/messages.json") {
		t.Errorf("the rehearsal does not give the reporter the loopback address")
	}
}

// Receipt records are self-contained and self-expiring: past expiry and margin
// a receipt goes whatever its channel's state, and the last one of a recovered
// incident takes the incident's recovery bookkeeping with it.
func TestAUnitThatRecoversWhilePushoverIsSetAsideLeavesNothingAfterExpiry(t *testing.T) {
	dir := t.TempDir()
	unit := "ti-engine@boost.service"
	key := report.UnitFailedKey(unit, "inv1")
	path := filepath.Join(dir, "u.json")
	u := &unitState{path: path, seen: map[string]bool{
		channelKey("pushover", key):  true,
		receiptKey("rcpt1", t0, key): true,
	}, sent: map[string]time.Time{channelKey("pushover", key): t0}, repeats: map[string]int{}, baselines: map[string]time.Time{}}
	aside := []channelFault{{channel: "pushover", err: errors.New("TI_PUSHOVER_APP_TOKEN missing")}}
	if err := resolveKey(u, key, nil, aside, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if u.repeats[recoveredKey(key)] != 1 {
		t.Fatalf("no recovery marker while the receipt is kept: %v", u.repeats)
	}
	late := t0.Add(pushoverExpire + receiptMargin + time.Minute)
	if err := clearUnit(path, unit, "", nil, aside, late); err != nil {
		t.Fatal(err)
	}
	seen, sent, repeats, _, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 0 || len(sent) != 0 || len(repeats) != 0 {
		t.Errorf("state after expiry = %v %v %v: want nothing left behind", seen, sent, repeats)
	}
}

func TestPruningALiveIncidentsExpiredReceiptKeepsTheIncident(t *testing.T) {
	key := "halted:x"
	seen := map[string]bool{receiptKey("a", t0, key): true, channelKey("pushover", key): true}
	sent := map[string]time.Time{channelKey("pushover", key): t0}
	if !pruneReceipts(seen, sent, map[string]int{}, t0.Add(time.Hour)) {
		t.Error("a pruned receipt was not reported as a change")
	}
	if len(seen) != 1 || len(sent) != 1 {
		t.Errorf("state = %v %v: want the delivery record kept", seen, sent)
	}
}

func TestAClearedClockAlertWithPushoverDisabledLeavesNothingAfterExpiry(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	check := newStandInCheck(t)
	dir := t.TempDir()
	clockFile := filepath.Join(dir, "host-clock.json")
	seed := `{"keys":["` + receiptKey("rcpt1", t0, report.ClockDriftKey(hostName())) + `"]}`
	if err := os.WriteFile(clockFile, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": "https://ntfy.private.test"})
	if err := runWith(t, r, dir, check, "ntfy", getenv, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	late := t0.Add(pushoverExpire + receiptMargin + time.Hour)
	if err := runWith(t, r, dir, check, "ntfy", getenv, late); err != nil {
		t.Fatal(err)
	}
	h, err := loadHostClock(clockFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Keys) != 0 || len(h.Sent) != 0 || len(h.Repeats) != 0 {
		t.Errorf("host clock state = %v %v %v: want no receipt left behind", h.Keys, h.Sent, h.Repeats)
	}
}

func TestOnlyJSONWhiteSpaceMayFollowTheAcknowledgement(t *testing.T) {
	for name, tail := range map[string]string{"form feed": "\f", "vertical tab": "\v", "no-break space": " ", "next line": "\u0085"} {
		if err := checkAck([]byte(`{"status":1}` + tail)); err == nil {
			t.Errorf("%s after the object was accepted", name)
		}
	}
	if err := checkAck([]byte(`{"status":1}` + " \t\r\n ")); err != nil {
		t.Errorf("JSON white space after the object was refused: %v", err)
	}
}

// The forwarder lives on the container's own disk, which a re-creation
// replaces; the rehearsal installs it after every up.sh and checks it after
// every restart and re-creation.
func TestTheRehearsalReinstallsTheForwarderAfterEveryCreateAndChecksItAfterEveryRestart(t *testing.T) {
	test, err := os.ReadFile("../../deploy/mac-mini/test-container.sh")
	if err != nil {
		t.Fatal(err)
	}
	up := regexp.MustCompile(`(?s)\nup\(\) \{.*?\n\}\n`).FindString(string(test))
	if !strings.Contains(up, "up.sh") || !strings.Contains(up, "inside forwarder") {
		t.Errorf("the rehearsal's up does not install the forwarder after up.sh: %q", up)
	}
	checks, err := os.ReadFile("../../deploy/mac-mini/container-checks.sh")
	if err != nil {
		t.Fatal(err)
	}
	after := regexp.MustCompile(`(?s)\n {2}after-restart\)\n.*?\n {4};;\n`).FindString(string(checks))
	if !strings.Contains(after, "ti-sink-forward.service") || !strings.Contains(after, `"$forward/posts"`) {
		t.Errorf("after-restart does not check that the forwarder runs and relays")
	}
}
