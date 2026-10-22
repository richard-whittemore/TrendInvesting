package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// fakeTracker is the in-memory issue tracker the channel is tested against.
type fakeTracker struct {
	issues  []*fakeIssue
	failing error
	// closeFailing, when set, is the answer to every close.
	closeFailing error
	// commentsFailing, when set, is the answer to every read of an issue's
	// comments (the comments, or the identity that tells the channel's own).
	commentsFailing error
	writes          int
	// delay is how long each call takes, unless its deadline comes first.
	delay time.Duration
	// onCall, when set, is told of each call.
	onCall func()
}

// call is the start of every call: it waits out delay within ctx.
func (f *fakeTracker) call(ctx context.Context) error {
	if f.onCall != nil {
		f.onCall()
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.failing
}

type fakeIssue struct {
	number   int
	title    string
	body     string
	labels   []string
	comments []string
	open     bool
	// strangers are the indexes of the comments someone other than the
	// channel's own identity wrote; anonymous those GitHub names no author of.
	strangers, anonymous map[int]bool
}

// labelled reports whether the issue carries the alert label.
func (i *fakeIssue) labelled() bool {
	for _, l := range i.labels {
		if l == alertLabel {
			return true
		}
	}
	return false
}

func (f *fakeTracker) Latest(ctx context.Context, marker string) (trackedIssue, bool, error) {
	if err := f.call(ctx); err != nil {
		return trackedIssue{}, false, err
	}
	for n := len(f.issues) - 1; n >= 0; n-- {
		if i := f.issues[n]; i.labelled() && strings.Contains(i.body, marker) {
			return trackedIssue{Number: i.number, Open: i.open, Comments: len(i.comments)}, true, nil
		}
	}
	return trackedIssue{}, false, nil
}

func (f *fakeTracker) Create(ctx context.Context, title, body string, labels []string) (int, error) {
	if err := f.call(ctx); err != nil {
		return 0, err
	}
	f.writes++
	f.issues = append(f.issues, &fakeIssue{number: len(f.issues) + 1, title: title, body: body, labels: labels, open: true})
	return len(f.issues), nil
}

func (f *fakeTracker) Closed(ctx context.Context, number int) (bool, error) {
	if err := f.call(ctx); err != nil {
		return false, err
	}
	if number < 1 || number > len(f.issues) {
		return true, nil
	}
	return !f.issues[number-1].open, nil
}

func (f *fakeTracker) Listed(ctx context.Context) (map[int]bool, error) {
	if err := f.call(ctx); err != nil {
		return nil, err
	}
	listed := map[int]bool{}
	for _, i := range f.issues {
		if i.labelled() {
			listed[i.number] = true
		}
	}
	return listed, nil
}

func (f *fakeTracker) OwnComments(ctx context.Context, number int) ([]string, error) {
	if err := f.call(ctx); err != nil {
		return nil, err
	}
	if f.commentsFailing != nil {
		return nil, f.commentsFailing
	}
	var own []string
	for n, c := range f.issues[number-1].comments {
		if !f.issues[number-1].strangers[n] {
			own = append(own, c)
		}
	}
	return own, nil
}

func (f *fakeTracker) Comment(ctx context.Context, number int, body string) error {
	if err := f.call(ctx); err != nil {
		return err
	}
	f.writes++
	f.issues[number-1].comments = append(f.issues[number-1].comments, body)
	return nil
}

func (f *fakeTracker) CloseIssue(ctx context.Context, number int) error {
	if err := f.call(ctx); err != nil {
		return err
	}
	if f.closeFailing != nil {
		return f.closeFailing
	}
	f.writes++
	f.issues[number-1].open = false
	return nil
}

func (f *fakeTracker) MarkResolved(ctx context.Context, number int) error {
	if err := f.call(ctx); err != nil {
		return err
	}
	f.writes++
	f.issues[number-1].labels = []string{"alert-resolved"}
	return nil
}

func haltAlert() report.Alert {
	return report.Alert{Key: "engine-state:joint@2024-01-02T21:00:00Z", Kind: report.KindHalted, Severity: report.Urgent, AccountID: "joint",
		At: t0, Repeat: true, Title: "joint: engine halted", Body: "The engine halted: risk limit breached."}
}

func newIssues(tr issueTracker) *issuesSender {
	return &issuesSender{tracker: tr, redactor: report.NewRedactor(nil)}
}

// An alert key is one issue: its repeat is a comment on the open issue, and
// never a second issue.
func TestTheSameAlertKeyTwiceIsOneIssue(t *testing.T) {
	tr := &fakeTracker{}
	s := newIssues(tr)
	a := haltAlert()
	if err := s.send(a, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.send(a, 1); err != nil {
		t.Fatal(err)
	}
	if len(tr.issues) != 1 {
		t.Fatalf("%d issues, want 1", len(tr.issues))
	}
	i := tr.issues[0]
	if !i.open || len(i.labels) != 1 || i.labels[0] != "alert" {
		t.Errorf("issue open=%v labels=%v, want open with label alert", i.open, i.labels)
	}
	if !strings.Contains(i.body, "risk limit breached") || !strings.Contains(i.title, "engine halted") {
		t.Errorf("issue lacks the alert text:\n%s\n%s", i.title, i.body)
	}
	if len(i.comments) != 1 || !strings.Contains(i.comments[0], "repeat 1") {
		t.Errorf("the repeat is one comment naming it, got %q", i.comments)
	}
}

// Different keys are different issues.
func TestDifferentAlertKeysAreDifferentIssues(t *testing.T) {
	tr := &fakeTracker{}
	s := newIssues(tr)
	a, b := haltAlert(), haltAlert()
	b.Key = "reconciliation:x@joint"
	if err := errors.Join(s.send(a, 0), s.send(b, 0)); err != nil {
		t.Fatal(err)
	}
	if len(tr.issues) != 2 {
		t.Fatalf("%d issues, want 2", len(tr.issues))
	}
}

// Resolving closes the issue; an alert that comes back after that is a new
// incident and a new issue; resolving what is not open is not an error.
func TestResolveClosesTheIssueAndANewIncidentOpensAnother(t *testing.T) {
	tr := &fakeTracker{}
	s := newIssues(tr)
	a := haltAlert()
	if err := s.send(a, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.resolve(a.Key); err != nil {
		t.Fatal(err)
	}
	if tr.issues[0].open {
		t.Fatal("the resolved alert's issue is still open")
	}
	if err := s.resolve(a.Key); err != nil {
		t.Fatalf("resolving a closed issue: %v", err)
	}
	if err := s.send(a, 0); err != nil {
		t.Fatal(err)
	}
	if len(tr.issues) != 2 || !tr.issues[1].open {
		t.Fatalf("a recurrence after the close is a new open issue, got %d issues", len(tr.issues))
	}
}

// A tracker that fails is the channel's failure: the error is returned for
// the reporter to retry at the next firing.
func TestATrackerFailureIsReturned(t *testing.T) {
	tr := &fakeTracker{failing: errors.New("server answered 500")}
	if err := newIssues(tr).send(haltAlert(), 0); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}
}

// Nothing the journal or the alert holds that is a secret, an account id, a
// token, a Tailscale host or a figure reaches the issue's title, body or
// comments.
func TestNoSecretAccountOrHostReachesAnIssue(t *testing.T) {
	tr := &fakeTracker{}
	s := &issuesSender{tracker: tr, redactor: report.NewRedactor([]string{"topic-secret", "boxhost"})}
	s.evidence = func() string { return "#4 order refused DU1234567 ghp_abcdefghijklmnopqrstuvwxyz0123456789 on boxhost" }
	a := haltAlert()
	a.Title = "DU1234567: halted on lightsail.tail1a2b3.ts.net"
	a.Body = "Account U7654321 holds $9,999.00; topic-secret; 100.100.100.100"
	if err := errors.Join(s.send(a, 0), s.send(a, 1)); err != nil {
		t.Fatal(err)
	}
	i := tr.issues[0]
	all := i.title + "\n" + i.body + "\n" + strings.Join(i.comments, "\n")
	for _, leak := range []string{"DU1234567", "U7654321", "9,999", "topic-secret", "ghp_", "ts.net", "100.100.100.100", "boxhost"} {
		if strings.Contains(all, leak) {
			t.Errorf("the issue holds %q:\n%s", leak, all)
		}
	}
}

// A public body, which carries no figure, is the text when the alert has one:
// the issues are in a public repository.
func TestAnIssueCarriesThePublicTextWhenThereIsOne(t *testing.T) {
	tr := &fakeTracker{}
	a := haltAlert()
	a.Body, a.PublicBody = "cash differs by 1234 from the book", "cash differs; figures are in the journal"
	if err := newIssues(tr).send(a, 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tr.issues[0].body, "1234") || !strings.Contains(tr.issues[0].body, "figures are in the journal") {
		t.Fatalf("body:\n%s", tr.issues[0].body)
	}
}

// A repeating alert the owner has acknowledged resolves its issue, once.
func TestAnAcknowledgedAlertClosesItsIssueOnce(t *testing.T) {
	tr := &fakeTracker{}
	s := newIssues(tr)
	a := haltAlert()
	seen := map[string]bool{}
	if err := deliver([]sender{s}, a, seen, map[string]time.Time{}, map[string]int{}, t0, time.Minute, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	a.Acknowledged = true
	for range 2 {
		if err := closeAcknowledged([]sender{s}, []report.Alert{a}, seen); err != nil {
			t.Fatal(err)
		}
	}
	if tr.issues[0].open || tr.writes != 4 || len(tr.issues[0].comments) != 1 || tr.issues[0].labelled() {
		t.Fatalf("open=%v writes=%d comments=%d labels=%v, want closed with one create, one closing comment, one close and the label taken off", tr.issues[0].open, tr.writes, len(tr.issues[0].comments), tr.issues[0].labels)
	}
	// The record stops the alert being delivered again: it would reopen.
	if err := deliver([]sender{s}, a, seen, map[string]time.Time{}, map[string]int{}, t0.Add(time.Hour), time.Minute, func() error { return nil }); err != nil || len(tr.issues) != 1 {
		t.Fatalf("an acknowledged alert is sent again: %v, %d issues", err, len(tr.issues))
	}
}

// fakeGitHub is the GitHub REST API's issues endpoints, in memory, as far as
// the channel uses them: issues listed newest first, filtered by label and
// state, and comments, each paged with a Link header; a body or comment over
// GitHub's 65,536-character limit is refused with 422.
type fakeGitHub struct {
	mu      sync.Mutex
	issues  []*fakeIssue
	status  int // when not zero, every request is answered with it
	tokens  []string
	methods []string
	// hang makes every request wait until its client gives up.
	hang bool
	// delay is how long each request takes.
	delay time.Duration
	// patchFailures is how many closes to refuse before taking one.
	patchFailures int
	// createAnswer, when set, is the answer to an issue's creation.
	createAnswer string
	// times are when each request arrived.
	times []time.Time
	// writeStatus, when not zero, answers every write (a POST or a PATCH)
	// with it; retryAfter adds GitHub's Retry-After header to that answer.
	writeStatus int
	retryAfter  bool
	// omitState lists the issues without their state.
	omitState bool
	// stallWrites answers each write's headers, then stops sending its body
	// until the client gives up.
	stallWrites bool
	// userStatus, when not zero, answers the token's user (GET /user) with
	// it; userID, when not zero, is the id that answer names.
	userStatus int
	userID     int
}

// fakeSelf and fakeStranger are the ids of the channel's own identity and of
// another commenter.
const fakeSelf, fakeStranger = 41, 42

func (g *fakeGitHub) handler(w http.ResponseWriter, r *http.Request) {
	if g.hang {
		g.mu.Lock()
		g.times = append(g.times, time.Now())
		g.methods = append(g.methods, r.Method+" "+r.URL.Path)
		g.mu.Unlock()
		<-r.Context().Done()
		return
	}
	if g.delay > 0 {
		select {
		case <-time.After(g.delay):
		case <-r.Context().Done():
			return
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.times = append(g.times, time.Now())
	g.tokens = append(g.tokens, r.Header.Get("Authorization"))
	g.methods = append(g.methods, r.Method+" "+r.URL.Path)
	if g.status != 0 {
		w.WriteHeader(g.status)
		return
	}
	if g.stallWrites && (r.Method == http.MethodPost || r.Method == http.MethodPatch) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("["))
		w.(http.Flusher).Flush()
		g.mu.Unlock()
		<-r.Context().Done()
		g.mu.Lock()
		return
	}
	if g.writeStatus != 0 && (r.Method == http.MethodPost || r.Method == http.MethodPatch) {
		if g.retryAfter {
			w.Header().Set("Retry-After", "60")
		}
		w.WriteHeader(g.writeStatus)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/user" {
		if g.userStatus != 0 {
			w.WriteHeader(g.userStatus)
			return
		}
		id := fakeSelf
		if g.userID != 0 {
			id = g.userID
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "login": "reporter"})
		return
	}
	var in struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
		State  string   `json:"state"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if utf8.RuneCountInString(in.Body) > 65536 {
		http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
		return
	}
	const base = "/repos/owner/repo/issues"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == base:
		q := r.URL.Query()
		state := q.Get("state")
		if q.Get("labels") != "alert" || (state != "open" && state != "all") {
			http.Error(w, "unfiltered", http.StatusBadRequest)
			return
		}
		var out []any
		for n := len(g.issues) - 1; n >= 0; n-- {
			i := g.issues[n]
			if (state == "open" && !i.open) || !i.labelled() {
				continue
			}
			s := "closed"
			if i.open {
				s = "open"
			}
			issue := map[string]any{"number": i.number, "body": i.body, "state": s, "comments": len(i.comments)}
			if g.omitState {
				delete(issue, "state")
			}
			out = append(out, issue)
		}
		page(w, r, out)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
		var out []any
		i := g.issues[issueNumber(r.URL.Path)-1]
		for n, c := range i.comments {
			by := fakeSelf
			if i.strangers[n] {
				by = fakeStranger
			}
			if i.anonymous[n] {
				out = append(out, map[string]any{"body": c, "user": nil})
				continue
			}
			out = append(out, map[string]any{"body": c, "user": map[string]any{"id": by}})
		}
		page(w, r, out)
	case r.Method == http.MethodPost && r.URL.Path == base:
		g.issues = append(g.issues, &fakeIssue{number: len(g.issues) + 1, title: in.Title, body: in.Body, labels: in.Labels, open: true})
		w.WriteHeader(http.StatusCreated)
		if g.createAnswer != "" {
			_, _ = w.Write([]byte(g.createAnswer))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": len(g.issues)})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
		n := issueNumber(r.URL.Path)
		g.issues[n-1].comments = append(g.issues[n-1].comments, in.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{}"))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
		i := g.issues[issueNumber(r.URL.Path)-1]
		i.labels = append(i.labels, in.Labels...)
		_, _ = w.Write([]byte("[]"))
	case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/labels/"+alertLabel):
		i := g.issues[issueNumber(r.URL.Path)-1]
		if !i.labelled() {
			http.Error(w, `{"message":"Label does not exist"}`, http.StatusNotFound)
			return
		}
		i.labels = slices.DeleteFunc(i.labels, func(l string) bool { return l == alertLabel })
		_, _ = w.Write([]byte("[]"))
	case r.Method == http.MethodPatch:
		if g.patchFailures > 0 {
			g.patchFailures--
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		n := issueNumber(r.URL.Path)
		g.issues[n-1].open = in.State != "closed"
		_, _ = w.Write([]byte("{}"))
	default:
		http.NotFound(w, r)
	}
}

// page answers one page of all, as per_page and page ask, with GitHub's Link
// header naming the next and the last page.
func page(w http.ResponseWriter, r *http.Request, all []any) {
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if per <= 0 {
		per = 30
	}
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if p <= 0 {
		p = 1
	}
	last := max(1, (len(all)+per-1)/per)
	link := func(n int, rel string) string {
		u := *r.URL
		q := u.Query()
		q.Set("page", strconv.Itoa(n))
		u.RawQuery = q.Encode()
		return fmt.Sprintf("<http://%s%s>; rel=%q", r.Host, u.String(), rel)
	}
	var links []string
	if p < last {
		links = append(links, link(p+1, "next"))
	}
	links = append(links, link(last, "last"))
	w.Header().Set("Link", strings.Join(links, ", "))
	lo, hi := min(len(all), (p-1)*per), min(len(all), p*per)
	out := all[lo:hi]
	if out == nil {
		out = []any{}
	}
	_ = json.NewEncoder(w).Encode(out)
}

func issueNumber(path string) int {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	n, _ := strconv.Atoi(parts[4])
	return n
}

func (g *fakeGitHub) snapshot() []fakeIssue {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []fakeIssue
	for _, i := range g.issues {
		out = append(out, *i)
	}
	return out
}

const issuesToken = "github_pat_11TESTTOKEN0123456789_abcdefghijklmnopqrstuvwxyz"

func issuesEnv(url string) map[string]string {
	return map[string]string{"TI_GITHUB_ISSUES_TOKEN": issuesToken, "TI_GITHUB_ISSUES_REPO": "owner/repo", "TI_GITHUB_API_URL": url}
}

// A journal whose refusal names an IBKR account and a figure, so the issue's
// evidence has something to redact.
func leakyJournal(t *testing.T, dir string) string {
	t.Helper()
	entries := haltedEntries(t)
	refused := event.OrderRefusedPayload{AccountID: "DU1234567", PlanID: "p", Tag: "targets:0", ClientOrderID: "tw-a", InstrumentID: "SPY",
		Side: event.OrderSideBuy, Quantity: 39, LimitPrice: 101, Reason: event.RefusedOrderSize, Detail: "DU1234567 short $4,000.00 ts host lightsail.tail1a2b3.ts.net",
		Figures: event.RiskFigures{NetLiquidation: 4000, ReferencePrice: 100, ExposureBefore: 0.5, ExposureAfter: 0.98, OrderFraction: 0.48, OrdersInDecision: 1}}
	entries[3] = journal.Entry{Kind: journal.KindDecision, Envelope: envelope(t, 4, event.OrderRefusedEventType, event.OrderRefusedSchemaVersion, "refuse", t0, refused)}
	var buf bytes.Buffer
	if err := journal.Write(&buf, journal.NewHeader("h", "v", t0.Add(-time.Hour), t0), entries); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "joint.journal")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The whole run: every alert becomes an issue with the journal's evidence
// redacted, a GitHub outage never holds back PagerDuty or ntfy, the issue is
// made at the next firing when GitHub is back, and a repeat is a comment.
func TestGitHubIssuesChannelEndToEnd(t *testing.T) {
	gh := &fakeGitHub{status: http.StatusInternalServerError}
	ghServer := httptest.NewServer(http.HandlerFunc(gh.handler))
	defer ghServer.Close()
	pd := &pagerDuty{status: http.StatusAccepted, body: `{"status":"success"}`}
	pdServer := httptest.NewServer(http.HandlerFunc(pd.handler))
	defer pdServer.Close()
	ntfy := &ntfyRecorder{}
	ntfyServer := httptest.NewServer(http.HandlerFunc(ntfy.handler))
	defer ntfyServer.Close()

	environment := issuesEnv(ghServer.URL)
	environment["TI_PAGERDUTY_ROUTING_KEY"], environment["TI_PAGERDUTY_URL"] = "k", pdServer.URL
	environment["TI_NTFY_TOPIC"], environment["TI_NTFY_SERVER"] = "t", ntfyServer.URL
	dir := t.TempDir()
	args := []string{"-journal", leakyJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty,github-issues", "-repeat-interval", "30m"}
	runAt := func(after time.Duration) (string, error) {
		var out bytes.Buffer
		err := run(args, env(environment), func() time.Time { return t0.Add(after) }, &out, http.DefaultClient)
		return out.String() + fmt.Sprint(err), err
	}

	out, err := runAt(time.Hour)
	if err == nil || !strings.Contains(err.Error(), "github-issues") {
		t.Fatalf("the outage must fail the run naming the channel, got %v", err)
	}
	if strings.Contains(out, issuesToken) {
		t.Fatalf("the token reached the output:\n%s", out)
	}
	if summaries, _ := pagerDutySummaries(pd); !anyContains(summaries, "engine halted") {
		t.Errorf("PagerDuty was held back by the GitHub outage: %v", summaries)
	}
	if titles, _ := ntfy.posted(); !anyContains(titles, "engine halted") {
		t.Errorf("ntfy was held back by the GitHub outage: %v", titles)
	}

	gh.mu.Lock()
	gh.status = 0
	gh.mu.Unlock()
	if out, err = runAt(time.Hour + time.Minute); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	issues := gh.snapshot()
	if len(issues) != 2 {
		t.Fatalf("%d issues, want one for the refusal and one for the halt: %+v", len(issues), issues)
	}
	for _, i := range issues {
		if len(i.labels) != 1 || i.labels[0] != "alert" || !i.open {
			t.Errorf("issue %q: labels %v open %v", i.title, i.labels, i.open)
		}
	}
	for _, tok := range gh.tokens {
		if tok != "Bearer "+issuesToken {
			t.Fatalf("authorization = %q", tok)
		}
	}
	var halt fakeIssue
	for _, i := range issues {
		if strings.Contains(i.title, "engine halted") {
			halt = i
		}
	}
	for _, want := range []string{"journal records", "journal-query", "refused"} {
		if !strings.Contains(halt.body, want) {
			t.Errorf("the issue lacks %q:\n%s\n%+v", want, halt.body, issues)
		}
	}
	for _, i := range issues {
		all := i.title + "\n" + i.body + "\n" + strings.Join(i.comments, "\n")
		for _, leak := range []string{"DU1234567", "4,000", "4000", "ts.net", issuesToken, "TESTTOKEN"} {
			if strings.Contains(all, leak) {
				t.Errorf("issue %q holds %q:\n%s", i.title, leak, all)
			}
		}
	}

	// A repeat at the interval is a comment on the halt's issue.
	if out, err = runAt(2 * time.Hour); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	issues = gh.snapshot()
	if len(issues) != 2 {
		t.Fatalf("a repeat opened another issue: %d", len(issues))
	}
	comments := 0
	for _, i := range issues {
		comments += len(i.comments)
	}
	if comments != 1 {
		t.Errorf("%d comments after one repeat, want 1", comments)
	}
}

// The channel needs its token and a repository named owner/name; a fault sets
// only it aside, and the token is never in the message.
func TestGitHubIssuesChannelConfigurationFaultSetsOnlyItAside(t *testing.T) {
	for name, environment := range map[string]map[string]string{
		"no token":        {"TI_GITHUB_ISSUES_REPO": "owner/repo"},
		"no repository":   {"TI_GITHUB_ISSUES_TOKEN": issuesToken},
		"malformed repo":  {"TI_GITHUB_ISSUES_TOKEN": issuesToken, "TI_GITHUB_ISSUES_REPO": "owner"},
		"malformed api":   {"TI_GITHUB_ISSUES_TOKEN": issuesToken, "TI_GITHUB_ISSUES_REPO": "owner/repo", "TI_GITHUB_API_URL": "not a url"},
		"repo with slash": {"TI_GITHUB_ISSUES_TOKEN": issuesToken, "TI_GITHUB_ISSUES_REPO": "owner/repo/extra"},
	} {
		t.Run(name, func(t *testing.T) {
			environment["TI_NTFY_TOPIC"] = "t"
			senders, faults, err := buildSenders("ntfy,github-issues", env(environment), http.DefaultClient, &bytes.Buffer{})
			if err != nil || len(senders) != 1 || len(faults) != 1 || faults[0].channel != "github-issues" {
				t.Fatalf("senders %d faults %v err %v", len(senders), faults, err)
			}
			if strings.Contains(faults[0].err.Error(), issuesToken) {
				t.Fatalf("the token is in the message: %v", faults[0].err)
			}
		})
	}
}

// The daily summary carries balances and never goes to a public repository.
func TestTheDailySummaryNeverGoesToGitHubIssues(t *testing.T) {
	s := newIssues(&fakeTracker{})
	if _, ok := any(s).(summaryTaker); ok {
		t.Fatal("the issues channel takes the summary")
	}
	tr := &fakeTracker{}
	a := report.Alert{Key: "summary:x", Kind: kindSummary, Severity: report.Routine, Title: "summary", Body: "equity 100"}
	if err := deliver([]sender{newIssues(tr)}, a, map[string]bool{}, map[string]time.Time{}, map[string]int{}, t0, time.Minute, func() error { return nil }); err != nil || len(tr.issues) != 0 {
		t.Fatalf("summary reached the tracker: %v %d", err, len(tr.issues))
	}
}
