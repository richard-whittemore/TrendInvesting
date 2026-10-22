package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
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
	// afterList, when set, runs after each issue listing is answered (the
	// lock held), with the label it listed ("" for none).
	afterList func(label string)
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
		"updated_at": created.Format(time.RFC3339),
		"labels":     []map[string]any{{"name": label}},
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
		label, state := r.URL.Query().Get("labels"), r.URL.Query().Get("state")
		var matching []map[string]any
		for _, is := range f.issues {
			if st, _ := is["state"].(string); state != "all" && state != "" && st != "" && st != state && (st == "open" || st == "closed") {
				continue
			}
			for _, l := range is["labels"].([]map[string]any) {
				if label == "" || l["name"] == label {
					matching = append(matching, is)
					break
				}
			}
		}
		if r.URL.Query().Get("sort") == "updated" {
			// Most recently updated first; the since filter is left to the
			// client, as a GitHub that ignored it would.
			slices.SortStableFunc(matching, func(a, b map[string]any) int {
				au, _ := a["updated_at"].(string)
				bu, _ := b["updated_at"].(string)
				return strings.Compare(bu, au)
			})
		}
		f.page(w, r, matching, page)
		if f.afterList != nil {
			f.afterList(label)
		}
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

// TestAlertIssuesListsTheReportersLabelsUntilTheStartAndSkipsPulls: the
// listing is of the reporter's two labels, open and closed, alert and then alert-resolved
// (so an alert that cleared, and swapped one for the other, before the job
// read it is still listed), newest first; each stops at the first issue
// older than since, and never takes a pull request for an issue.
func TestAlertIssuesListsTheReportersLabelsUntilTheStartAndSkipsPulls(t *testing.T) {
	f, srv := newFakeGitHub(t)
	f.pageSize = 2
	since := time.Date(2026, time.October, 22, 0, 0, 0, 0, time.UTC)
	f.addIssue(7, alertLabel, "owner", since.Add(3*time.Hour), alertBody)
	f.addIssue(6, alertLabel, "owner", since.Add(2*time.Hour), alertBody)
	f.addIssue(5, resolvedLabel, "owner", since.Add(90*time.Minute), alertBody)
	f.issues[len(f.issues)-1]["state"] = "closed"
	f.issues[len(f.issues)-1]["closed_at"] = since.Add(100 * time.Minute).Format(time.RFC3339)
	f.issues = append(f.issues, map[string]any{"number": 4, "pull_request": map[string]any{"url": "x"}, "labels": []map[string]any{{"name": alertLabel}}, "created_at": since.Add(time.Hour).Format(time.RFC3339), "user": map[string]any{"login": "owner"}})
	f.addIssue(2, alertLabel, "owner", since.Add(-time.Hour), alertBody)
	f.addIssue(1, alertLabel, "owner", since.Add(-2*time.Hour), alertBody)
	got, err := trackerFor(t, srv).AlertIssues(context.Background(), since, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Number != 7 || got[1].Number != 6 || got[2].Number != 5 || got[0].Author != "owner" || got[0].Body != alertBody {
		t.Fatalf("listed %+v", got)
	}
	if !got[0].Closed.IsZero() || !got[2].Closed.Equal(since.Add(100*time.Minute)) {
		t.Errorf("closing times %v and %v; want none for the open issue and the closed one's own", got[0].Closed, got[2].Closed)
	}
	if n := len(f.sent()); n != 5 {
		t.Errorf("%d requests; want each listing stopping at the page holding an issue older than the start", n)
	}
	var labels []string
	for _, q := range f.listQueries {
		v, _ := url.ParseQuery(q)
		labels = append(labels, v.Get("labels")+" "+v.Get("state"))
	}
	want := []string{alertLabel + " open", alertLabel + " open", alertLabel + " closed", resolvedLabel + " open", resolvedLabel + " closed"}
	if !slices.Equal(labels, want) {
		t.Errorf("listings by label and state %q, want %q: alert's open and closed, then alert-resolved's", labels, want)
	}
}

// TestAnAlertOpenedBetweenTheTwoListingsIsInTheNext: an alert opened after
// the alert listing was read, and one opened and resolved before the
// alert-resolved listing, may be missing from this listing, never from the
// next: each listing reads every alert issue since the start.
func TestAnAlertOpenedBetweenTheTwoListingsIsInTheNext(t *testing.T) {
	f, srv := newFakeGitHub(t)
	since := time.Date(2026, time.October, 22, 0, 0, 0, 0, time.UTC)
	f.addIssue(100, alertLabel, "owner", since.Add(time.Hour), alertBody)
	f.afterList = func(label string) {
		if label != alertLabel {
			return
		}
		f.afterList = nil
		older := f.issues
		f.issues = nil
		f.addIssue(102, resolvedLabel, "owner", since.Add(3*time.Hour), alertBody)
		f.addIssue(101, alertLabel, "owner", since.Add(2*time.Hour), alertBody)
		f.issues = append(f.issues, older...)
	}
	got, err := trackerFor(t, srv).AlertIssues(context.Background(), since, since)
	if err != nil || !slices.ContainsFunc(got, func(is issue) bool { return is.Number == 100 }) {
		t.Fatalf("listed %+v, %v; want the alert open before either listing", got, err)
	}
	got, err = trackerFor(t, srv).AlertIssues(context.Background(), since, since)
	listed := map[int]bool{}
	for _, is := range got {
		listed[is.Number] = true
	}
	if err != nil || len(got) != 3 || !listed[100] || !listed[101] || !listed[102] {
		t.Fatalf("the next listing: %+v, %v; want #100, #101 and #102", got, err)
	}
}

// TestUnrelatedIssuesAndPullsNeverHideAnOlderAlert: issues and pull
// requests that carry neither of the reporter's labels never count against
// the listing's bound, so a flood of them after an alert neither cuts the
// listing nor keeps the alert from being reached.
func TestUnrelatedIssuesAndPullsNeverHideAnOlderAlert(t *testing.T) {
	f, srv := newFakeGitHub(t)
	f.pageSize = 1
	since := time.Date(2026, time.October, 22, 0, 0, 0, 0, time.UTC)
	for n := 2*maxPages + 10; n > 1; n-- {
		item := map[string]any{"number": n, "title": "x", "body": "x", "labels": []map[string]any{{"name": "enhancement"}}, "created_at": since.Add(time.Duration(n) * time.Minute).Format(time.RFC3339), "user": map[string]any{"login": "someone", "id": 5}}
		if n%2 == 0 {
			item["pull_request"] = map[string]any{"url": "x"}
		}
		f.issues = append(f.issues, item)
	}
	f.addIssue(1, alertLabel, "owner", since.Add(time.Minute), alertBody)
	got, err := trackerFor(t, srv).AlertIssues(context.Background(), since, since)
	if err != nil || len(got) != 1 || got[0].Number != 1 {
		t.Fatalf("listed %+v, err %v; want the alert, reached", got, err)
	}
}

// TestAListingLongerThanItsBoundIsAnError: past its bound a label's
// listing fails whole, naming the bound; it is never cut short and taken as
// the whole list.
func TestAListingLongerThanItsBoundIsAnError(t *testing.T) {
	f, srv := newFakeGitHub(t)
	since := time.Date(2026, time.October, 22, 0, 0, 0, 0, time.UTC)
	for n := maxPages*pageSize + 1; n > 0; n-- {
		f.addIssue(n, alertLabel, "owner", since.Add(time.Duration(n)*time.Minute), alertBody)
	}
	got, err := trackerFor(t, srv).AlertIssues(context.Background(), since, since)
	if err == nil || got != nil || !strings.Contains(err.Error(), fmt.Sprintf("more than %d issues", maxPages*pageSize)) {
		t.Fatalf("listed %d, err %v; want the bound named and nothing listed", len(got), err)
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
			if _, err := trackerFor(t, srv).AlertIssues(context.Background(), since, since); err == nil || !strings.Contains(err.Error(), "creation time") {
				t.Fatalf("err = %v, want the missing creation time named", err)
			}
		})
	}
}

// TestAnAlertShiftedPastAPageByARelabelIsAnalysedAtTheNextPoll: alerts #201
// to #1 are listed a hundred to a page; once the first page is read, #201
// is resolved (it loses alert), so the second page begins at #100 and #101
// is in neither listing. Nothing marks it handled: the next poll lists it
// and it is analysed, and every alert gets exactly one comment.
func TestAnAlertShiftedPastAPageByARelabelIsAnalysedAtTheNextPoll(t *testing.T) {
	f, srv := newFakeGitHub(t)
	w := newWorld(t)
	w.job.tracker = trackerFor(t, srv)
	w.job.dailyCap = 1000
	saved := syncFile
	t.Cleanup(func() { syncFile = saved })
	syncFile = func(*os.File) error { return nil }
	for n := 201; n >= 1; n-- {
		f.addIssue(n, alertLabel, "owner", pollTime.Add(time.Duration(n)*time.Minute), alertBody)
	}
	relabelled := false
	f.afterList = func(label string) {
		if label != alertLabel || relabelled {
			return
		}
		relabelled = true
		f.issues[0]["labels"] = []map[string]any{{"name": resolvedLabel}}
	}
	if err := w.job.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.poll(t)
	if !slices.ContainsFunc(w.analyst.requests, func(r analysisRequest) bool { return r.Issue.Number == 101 }) {
		t.Fatal("#101, shifted past a page by a relabel, was never analysed")
	}
	for n := 1; n <= 201; n++ {
		if c := f.comments[n]; len(c) != 1 || !strings.Contains(c[0]["body"].(string), triageMarkerPrefix) {
			t.Errorf("issue #%d has %d comments; want the job's one", n, len(c))
		}
	}
}

// TestAClosedIssueWithoutItsClosingTimeIsAnError: how long ago an issue
// closed decides whether its record is kept, so a closed issue without the
// time is a malformed answer: the listing fails whole.
func TestAClosedIssueWithoutItsClosingTimeIsAnError(t *testing.T) {
	f, srv := newFakeGitHub(t)
	since := time.Date(2026, time.October, 22, 0, 0, 0, 0, time.UTC)
	f.addIssue(5, alertLabel, "owner", since.Add(time.Hour), alertBody)
	f.issues[0]["state"] = "closed"
	if _, err := trackerFor(t, srv).AlertIssues(context.Background(), since, since); err == nil || !strings.Contains(err.Error(), "closing time") {
		t.Fatalf("err = %v, want the missing closing time named", err)
	}
}

// closeIssue closes the fake's issue number at closed (its last update).
func (f *fakeGitHub) closeIssue(number int, closed time.Time) {
	for _, is := range f.issues {
		if is["number"] == number {
			is["state"], is["closed_at"], is["updated_at"] = "closed", closed.Format(time.RFC3339), closed.Format(time.RFC3339)
		}
	}
}

// TestOldClosedIssuesNeverCountAgainstTheBound: open alert issues are
// listed whole, closed ones only if they closed within the last 30 days;
// the listing's bound counts only those, so any number of issues closed
// longer ago neither fails the listing nor hides a new alert.
func TestOldClosedIssuesNeverCountAgainstTheBound(t *testing.T) {
	f, srv := newFakeGitHub(t)
	f.pageSize = 1
	since := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	window := time.Date(2026, time.September, 22, 0, 0, 0, 0, time.UTC)
	f.addIssue(5000, alertLabel, "owner", window.Add(48*time.Hour), alertBody)
	f.addIssue(4999, resolvedLabel, "owner", window.Add(24*time.Hour), alertBody)
	f.closeIssue(4999, window.Add(30*time.Hour))
	for n := 4998; n > 4998-(maxPages*pageSize+20); n-- {
		f.addIssue(n, resolvedLabel, "owner", since.Add(time.Duration(n)*time.Second), alertBody)
		f.closeIssue(n, since.Add(time.Duration(n)*time.Second+time.Hour))
	}
	got, err := trackerFor(t, srv).AlertIssues(context.Background(), since, window)
	if err != nil {
		t.Fatalf("err = %v: issues closed before the window counted against the bound", err)
	}
	if len(got) != 2 || got[0].Number != 5000 || got[1].Number != 4999 {
		t.Fatalf("listed %d issues, %+v; want the open alert and the one closed in the window", len(got), got)
	}
	if n := len(f.sent()); n > 10 {
		t.Errorf("%d requests: the closed listing read past the window", n)
	}
}

// TestAnIssueWithoutAKnownStateIsAnError: an issue's state decides whether
// its closing time is read, so a missing or unknown one is a malformed
// answer, never taken as open: the listing fails whole.
func TestAnIssueWithoutAKnownStateIsAnError(t *testing.T) {
	for name, state := range map[string]any{"absent": nil, "null": "null", "unknown": "merged"} {
		t.Run(name, func(t *testing.T) {
			f, srv := newFakeGitHub(t)
			since := time.Date(2026, time.October, 22, 0, 0, 0, 0, time.UTC)
			f.addIssue(5, alertLabel, "owner", since.Add(time.Hour), alertBody)
			switch state {
			case nil:
				delete(f.issues[0], "state")
			case "null":
				f.issues[0]["state"] = nil
			default:
				f.issues[0]["state"] = state
			}
			if _, err := trackerFor(t, srv).AlertIssues(context.Background(), since, since); err == nil || !strings.Contains(err.Error(), "state") {
				t.Fatalf("err = %v, want the issue's state named", err)
			}
		})
	}
}
