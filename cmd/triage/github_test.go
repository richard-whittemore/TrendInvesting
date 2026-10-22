package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub is GitHub's REST API for one repository, in memory, recording
// every request it is sent.
type fakeGitHub struct {
	t        *testing.T
	mu       sync.Mutex
	issues   []map[string]any // newest first
	comments map[int][]map[string]any
	pulls    []map[string]any
	requests []string // "METHOD path"
	repos    []string // "owner/name METHOD path-in-the-repository"
	auth     []string
	selfID   int64
	pageSize int
	// listQueries are the issue listings' queries.
	listQueries []string
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *httptest.Server) {
	f := &fakeGitHub{t: t, comments: map[int][]map[string]any{}, selfID: 7, pageSize: 100}
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeGitHub) addIssue(number int, label, author string, created time.Time, body string) {
	f.issues = append(f.issues, map[string]any{
		"number": number, "title": fmt.Sprintf("alert %d", number), "body": body, "state": "open",
		"user": map[string]any{"login": author, "id": 99}, "created_at": created.Format(time.RFC3339),
		"labels": []map[string]any{{"name": label}},
	})
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	// Every repository is served alike; repo is the one the request named.
	repo, rest := "", r.URL.Path
	if p := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/repos/"), "/", 3); strings.HasPrefix(r.URL.Path, "/repos/") && len(p) == 3 {
		repo, rest = p[0]+"/"+p[1], "/"+p[2]
	}
	f.repos = append(f.repos, repo+" "+r.Method+" "+rest)
	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page == 0 {
		page = 1
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/user":
		writeJSON(w, map[string]any{"login": "owner", "id": f.selfID})
	case r.Method == http.MethodGet && repo != "" && rest == "/issues":
		f.listQueries = append(f.listQueries, r.URL.RawQuery)
		f.page(w, r, f.issues, page)
	case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "issues" && parts[2] == "comments":
		n, _ := strconv.Atoi(parts[1])
		f.page(w, r, f.comments[n], page)
	case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "issues" && parts[2] == "comments":
		n, _ := strconv.Atoi(parts[1])
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.comments[n] = append(f.comments[n], map[string]any{"body": in["body"], "user": map[string]any{"login": "owner", "id": f.selfID}})
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"id": 1})
	case r.Method == http.MethodPost && repo != "" && rest == "/pulls":
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.pulls = append(f.pulls, in)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"number": 600 + len(f.pulls), "html_url": fmt.Sprintf("https://github.com/%s/pull/%d", repo, 600+len(f.pulls))})
	default:
		http.Error(w, "not here", http.StatusNotFound)
	}
}

// page answers one page of items, with a Link header naming the next.
func (f *fakeGitHub) page(w http.ResponseWriter, r *http.Request, items []map[string]any, page int) {
	start := (page - 1) * f.pageSize
	end := min(start+f.pageSize, len(items))
	if start > len(items) {
		start = len(items)
	}
	if end < len(items) {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(page+1))
		w.Header().Set("Link", fmt.Sprintf(`<https://api.github.com%s?%s>; rel="next"`, r.URL.Path, q.Encode()))
	}
	out := items[start:end]
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGitHub) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// trackerFor is the real client, pointed at the fake.
func trackerFor(t *testing.T, srv *httptest.Server) *githubTracker {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	g, err := newGitHubTracker(srv.URL, u.Host, "owner/repo", "owner/repo", "ghp_testtokentesttokentesttoken00", "ghp_testtokentesttokentesttoken00", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestTheAPIAddressMustBeGitHubOverHTTPSOrTheNamedHost(t *testing.T) {
	for _, c := range []struct {
		api, allow string
		ok         bool
	}{
		{"https://api.github.com", "", true},
		{"http://api.github.com", "", false},
		{"https://evil.example", "", false},
		{"https://ghe.example", "ghe.example", true},
		{"http://ghe.example", "ghe.example", false},
		{"http://127.0.0.1:9000", "127.0.0.1:9000", true},
		{"https://user:pass@api.github.com", "", false},
		{"api.github.com", "", false},
	} {
		if err := checkAPIURL(c.api, c.allow); (err == nil) != c.ok {
			t.Errorf("checkAPIURL(%q, %q) = %v, want ok %v", c.api, c.allow, err, c.ok)
		}
	}
	if _, err := newGitHubTracker("https://api.github.com", "", "owner", "owner/repo", "t", "t", http.DefaultClient); err == nil {
		t.Error("a repository not owner/name was accepted")
	}
	if _, err := newGitHubTracker("https://api.github.com", "", "owner/repo", "owner/repo", "", "t", http.DefaultClient); err == nil {
		t.Error("an empty token was accepted")
	}
}

// TestAlertIssuesListsEveryIssueOnceUntilTheStartAndSkipsPulls: the
// listing is one listing of every issue, whatever its label (so an issue
// relabelled while the job reads is never missed), newest first; it stops
// at the first issue older than since, and never takes a pull request for
// an issue.
func TestAlertIssuesListsEveryIssueOnceUntilTheStartAndSkipsPulls(t *testing.T) {
	f, srv := newFakeGitHub(t)
	f.pageSize = 2
	since := time.Date(2026, time.October, 22, 0, 0, 0, 0, time.UTC)
	f.addIssue(5, alertLabel, "owner", since.Add(3*time.Hour), alertBody)
	f.addIssue(4, alertLabel, "owner", since.Add(2*time.Hour), alertBody)
	f.addIssue(9, resolvedLabel, "owner", since.Add(90*time.Minute), alertBody)
	f.issues = append(f.issues, map[string]any{"number": 3, "pull_request": map[string]any{"url": "x"}, "labels": []map[string]any{{"name": alertLabel}}, "created_at": since.Add(time.Hour).Format(time.RFC3339), "user": map[string]any{"login": "owner"}})
	f.addIssue(2, alertLabel, "owner", since.Add(-time.Hour), alertBody)
	f.addIssue(1, alertLabel, "owner", since.Add(-2*time.Hour), alertBody)
	got, cut, err := trackerFor(t, srv).AlertIssues(context.Background(), since, 0)
	if err != nil || cut {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Number != 5 || got[1].Number != 4 || got[2].Number != 9 || got[0].Author != "owner" || got[0].Body != alertBody {
		t.Fatalf("listed %+v", got)
	}
	if n := len(f.sent()); n != 3 {
		t.Errorf("%d requests; the listing should stop at the page holding an issue older than the start", n)
	}
	for _, q := range f.listQueries {
		if strings.Contains(q, "labels=") {
			t.Errorf("the listing is filtered by label: %s", q)
		}
	}
}

// TestAListingLongerThanItsBoundIsCutNotFailed: past its bound the listing
// returns the newest issues and says it was cut, so the job never stops;
// and it stops at the issues already handled (through), never reading them.
func TestAListingLongerThanItsBoundIsCutNotFailed(t *testing.T) {
	f, srv := newFakeGitHub(t)
	f.pageSize = 1
	since := time.Date(2026, time.October, 22, 0, 0, 0, 0, time.UTC)
	for n := maxPages + 2; n > 0; n-- {
		f.addIssue(n, alertLabel, "owner", since.Add(time.Duration(n)*time.Minute), alertBody)
	}
	got, cut, err := trackerFor(t, srv).AlertIssues(context.Background(), since, 0)
	if err != nil || !cut || len(got) != maxPages || got[0].Number != maxPages+2 {
		t.Fatalf("listed %d, cut %v, err %v; want the newest %d and the cut said", len(got), cut, err, maxPages)
	}
	before := len(f.sent())
	got, cut, err = trackerFor(t, srv).AlertIssues(context.Background(), since, maxPages)
	if err != nil || cut || len(got) != 2 || len(f.sent())-before != 3 {
		t.Fatalf("listed %v, cut %v, err %v, %d requests; want the two past through, stopping at it", got, cut, err, len(f.sent())-before)
	}
}

// TestAlertsAreListedFromOneRepositoryAndThePROpenedInAnother: the alerts
// live in a private repository and the code in the public one; the job
// lists and comments on issues in the first and opens its draft PR in the
// second, linking the private issue by its full address.
func TestAlertsAreListedFromOneRepositoryAndThePROpenedInAnother(t *testing.T) {
	f, srv := newFakeGitHub(t)
	f.addIssue(41, alertLabel, "owner", pollTime.Add(time.Minute), alertBody)
	u, _ := url.Parse(srv.URL)
	g, err := newGitHubTracker(srv.URL, u.Host, "owner/alerts", "owner/code", "ghp_alertstokenalertstokenalerts00", "ghp_codetokencodetokencodetokenco00", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	w := newWorld(t)
	w.job.tracker = g
	w.analyst.result = analysis{Kind: kindDefect, WhatHappened: "x", Rule: "ADR 0026", FixTitle: "t", FixSummary: "s"}
	w.fixer.changed = []string{"internal/x/a.go", "internal/x/a_test.go"}
	w.fixer.added = "+ok"
	w.poll(t)
	if len(f.pulls) != 1 || len(f.comments[41]) != 1 {
		t.Fatalf("pulls %v, comments %v", f.pulls, f.comments[41])
	}
	for _, r := range f.repos {
		switch {
		case strings.Contains(r, "/pulls"):
			if !strings.HasPrefix(r, "owner/code ") {
				t.Errorf("the PR went to %q", r)
			}
		case strings.Contains(r, "/issues"):
			if !strings.HasPrefix(r, "owner/alerts ") {
				t.Errorf("an issue call went to %q", r)
			}
		}
	}
	for i, r := range f.repos {
		want := "Bearer ghp_alertstokenalertstokenalerts00"
		if strings.Contains(r, "/pulls") {
			want = "Bearer ghp_codetokencodetokencodetokenco00"
		}
		if f.auth[i] != want {
			t.Errorf("%s was sent %q; want each repository's own token", r, f.auth[i])
		}
	}
	body := f.pulls[0]["body"].(string)
	if !strings.Contains(body, "https://github.com/owner/alerts/issues/41") || strings.Contains(body, "risk gate refused") {
		t.Errorf("PR body %q; want the private issue linked by its address and none of its text", body)
	}
	if c := f.comments[41][0]["body"].(string); !strings.Contains(c, "https://github.com/owner/code/pull/601") {
		t.Errorf("the comment does not link the PR in the code repository: %q", c)
	}
}

// TestOwnCommentsAreTheTokenUsersOnly: a marker someone else wrote never
// counts, and a comment GitHub names no author of fails the read, since it
// could be the job's own.
func TestOwnCommentsAreTheTokenUsersOnly(t *testing.T) {
	f, srv := newFakeGitHub(t)
	f.comments[41] = []map[string]any{
		{"body": "mine " + analysisMarker, "user": map[string]any{"login": "owner", "id": 7}},
		{"body": "theirs " + analysisMarker, "user": map[string]any{"login": "stranger", "id": 8}},
	}
	g := trackerFor(t, srv)
	own, err := g.OwnComments(context.Background(), 41)
	if err != nil || len(own) != 1 || !strings.HasPrefix(own[0], "mine") {
		t.Fatalf("own = %v, err = %v", own, err)
	}
	if login, err := g.Self(context.Background()); err != nil || login != "owner" {
		t.Fatalf("self = %q, %v", login, err)
	}
	f.comments[42] = []map[string]any{{"body": "ghost"}}
	if _, err := g.OwnComments(context.Background(), 42); err == nil {
		t.Fatal("a comment with no author was not an error")
	}
}

// TestARedirectIsRefusedAndTheTokenNeverFollowsIt: the token goes only to the
// checked address.
func TestARedirectIsRefusedAndTheTokenNeverFollowsIt(t *testing.T) {
	var leaked bool
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = leaked || r.Header.Get("Authorization") != ""
	}))
	defer other.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()
	if _, err := trackerFor(t, srv).Self(context.Background()); err == nil {
		t.Fatal("a redirect was taken as an answer")
	}
	if leaked {
		t.Fatal("the token followed the redirect")
	}
}

func TestAnAnswerOverItsLimitFails(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"login":"`+strings.Repeat("x", 100)+`","id":7}`)
	}))
	defer srv.Close()
	g := trackerFor(t, srv)
	g.maxAnswer = 50
	if _, err := g.Self(context.Background()); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v", err)
	}
}

func TestAFailingStatusIsAnErrorNamingIt(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()
	if err := trackerFor(t, srv).Comment(context.Background(), 1, "x"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
}

// TestADefectGoesThroughTheGateToADraftPullRequestOnGitHub: the defect case
// end to end against the fake GitHub: one comment, one draft PR into main,
// no merge, no review asked of anyone, and nothing more on a second poll.
func TestADefectGoesThroughTheGateToADraftPullRequestOnGitHub(t *testing.T) {
	f, srv := newFakeGitHub(t)
	f.addIssue(41, alertLabel, "owner", pollTime.Add(time.Minute), alertBody)
	w := newWorld(t)
	w.job.tracker = trackerFor(t, srv)
	w.analyst.result = analysis{Kind: kindDefect, WhatHappened: "x", Rule: "ADR 0026", FixTitle: "Fix @coderabbitai the side", FixSummary: "@greptileai review"}
	w.fixer.changed = []string{"internal/x/a.go", "internal/x/a_test.go"}
	w.fixer.added = "+ok"
	w.poll(t)
	w.poll(t)
	if len(f.pulls) != 1 || len(f.comments[41]) != 1 {
		t.Fatalf("pulls %v, comments %v", f.pulls, f.comments[41])
	}
	pr := f.pulls[0]
	if pr["draft"] != true || pr["base"] != "main" || pr["head"] != "triage/alert-41" {
		t.Errorf("PR %v; want a draft from the triage branch into main", pr)
	}
	for _, field := range []string{"title", "body"} {
		if s := pr[field].(string); strings.Contains(s, "@coderabbitai") || strings.Contains(s, "@greptileai") {
			t.Errorf("PR %s asks a bot: %q", field, s)
		}
	}
	for _, req := range f.sent() {
		if strings.Contains(req, "/merge") || strings.Contains(req, "requested_reviewers") || strings.HasPrefix(req, "PUT") || strings.HasPrefix(req, "DELETE") || strings.HasPrefix(req, "PATCH") {
			t.Errorf("the job asked GitHub %q", req)
		}
	}
}

// TestAnIssueWithoutItsCreationTimeIsAnError: the creation time is the
// listing's stopping point, so an item without one is a malformed answer,
// never the end of the history: the listing fails whole.
func TestAnIssueWithoutItsCreationTimeIsAnError(t *testing.T) {
	for name, created := range map[string]any{"absent": nil, "null": "null"} {
		t.Run(name, func(t *testing.T) {
			f, srv := newFakeGitHub(t)
			since := time.Date(2026, time.October, 22, 0, 0, 0, 0, time.UTC)
			bad := map[string]any{"number": 6, "title": "x", "body": alertBody, "user": map[string]any{"login": "owner", "id": 99}, "labels": []map[string]any{{"name": alertLabel}}}
			if created != nil {
				bad["created_at"] = nil
			}
			f.issues = append(f.issues, bad)
			f.addIssue(5, alertLabel, "owner", since.Add(time.Hour), alertBody)
			if _, _, err := trackerFor(t, srv).AlertIssues(context.Background(), since, 0); err == nil || !strings.Contains(err.Error(), "creation time") {
				t.Fatalf("err = %v, want the missing creation time named", err)
			}
		})
	}
}
