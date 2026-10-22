package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// githubTracker is the alerts repository's issues and the code repository's
// pulls over GitHub's REST API (https://docs.github.com/en/rest): the alert
// issues may live in a private repository and the code in the public one
// (docs/manual/alerts-and-dashboard.md, "Alerts analysed by Claude"). It
// keeps the reporter's own rules for the token
// (docs/manual/alerts-and-dashboard.md, "Alerts as GitHub Issues"):
// the token goes only to api.github.com over https, or to the one host the
// operator names; a redirect is refused, never followed; every answer is
// read to a bound and decoded whole or not at all; a listing longer than its
// bound is said to be cut, never taken as whole.
type githubTracker struct {
	// alerts and code are each repository's API address; alertsRepo and
	// codeRepo their owner/name; token is the alerts repository's and
	// codeToken the code repository's (the same one when one token covers
	// both).
	api, alerts, code, alertsRepo, codeRepo, token, codeToken string
	client                                                    *http.Client
	maxAnswer                                                 int64 // tests only: the read limit of every answer

	selfLogin string
	selfID    int64
}

const (
	defaultGitHubAPIURL = "https://api.github.com"
	// pageSize and maxPages bound a listing: 1,000 issues, or 1,000 comments
	// of one issue.
	pageSize = 100
	maxPages = 10
	// maxIssueText is GitHub's limit on an issue's body or a comment.
	maxIssueText = 65536
	// maxPageAnswer is the most one page is read to: its items, each with a
	// body of up to maxIssueText characters at 12 bytes a character (JSON's
	// widest escape), plus each item's own fields; maxItemAnswer is one item.
	maxItemAnswer = 12*maxIssueText + 16<<10
	maxPageAnswer = pageSize * maxItemAnswer
)

// validRepo reports whether repo is owner/name.
func validRepo(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	return ok && owner != "" && name != "" && !strings.Contains(name, "/") && !strings.ContainsAny(repo, " ?#%")
}

// newGitHubTracker builds the client for the alerts repository, with token,
// and the code repository, with codeToken (each owner/name), each sent only
// to api (checkAPIURL).
func newGitHubTracker(api, allow, alertsRepo, codeRepo, token, codeToken string, client *http.Client) (*githubTracker, error) {
	if token == "" || codeToken == "" {
		return nil, errors.New("triage: no GitHub token: the token file is empty")
	}
	if !validRepo(alertsRepo) || !validRepo(codeRepo) {
		return nil, errors.New("triage: -alerts-repo and -code-repo must be owner/name: the repository the alert issues are in, and the one the code is in")
	}
	if api == "" {
		api = defaultGitHubAPIURL
	}
	if err := checkAPIURL(api, allow); err != nil {
		return nil, err
	}
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	api = strings.TrimRight(api, "/")
	return &githubTracker{api: api, alerts: api + "/repos/" + alertsRepo, code: api + "/repos/" + codeRepo,
		alertsRepo: alertsRepo, codeRepo: codeRepo, token: token, codeToken: codeToken, client: &noRedirect}, nil
}

// checkAPIURL accepts the address the token is sent to: GitHub's API over
// https, or the one host allow names (over https, or http on a loopback
// address). The address's text is never in the error.
func checkAPIURL(api, allow string) error {
	refuse := errors.New("triage: TI_GITHUB_API_URL must be https://api.github.com, or an https address whose host TI_GITHUB_API_ALLOW_HOST names (http is allowed only for a loopback host)")
	u, err := url.Parse(api)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return refuse
	}
	if u.Host == "api.github.com" {
		if u.Scheme != "https" {
			return refuse
		}
		return nil
	}
	if allow == "" || !strings.EqualFold(allow, u.Host) {
		return refuse
	}
	if u.Scheme == "http" {
		if ip := net.ParseIP(u.Hostname()); u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return refuse
		}
	}
	return nil
}

// call is one request to the alerts repository, or to GitHub itself.
func (g *githubTracker) call(ctx context.Context, method, target string, limit int64, in, out any) (http.Header, error) {
	return g.callAs(ctx, g.token, method, target, limit, in, out)
}

// callAs is one request with token, its answer decoded into out, read to at
// most limit bytes. The token is never in an error.
func (g *githubTracker) callAs(ctx context.Context, token, method, target string, limit int64, in, out any) (http.Header, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, errors.New("build request")
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, errors.New("build request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return nil, fmt.Errorf("request failed: %w", urlErr.Err)
		}
		return nil, errors.New("request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		// The body of an error answer is never read.
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	if g.maxAnswer > 0 {
		limit = g.maxAnswer
	}
	if out != nil {
		lr := &io.LimitedReader{R: resp.Body, N: limit + 1}
		dec := json.NewDecoder(lr)
		err := dec.Decode(out)
		if err == nil {
			if _, tail := dec.Token(); !errors.Is(tail, io.EOF) {
				err = errors.New("data after the answer")
			}
		}
		switch {
		case lr.N <= 0:
			return nil, fmt.Errorf("GitHub's answer is larger than %d bytes", limit)
		case err != nil:
			return nil, errors.New("GitHub's answer was not understood")
		}
	}
	return resp.Header, nil
}

// nextPage reports whether GitHub's Link header names a next page.
func nextPage(h http.Header) bool {
	for _, link := range strings.Split(h.Get("Link"), ",") {
		if strings.Contains(link, `rel="next"`) {
			return true
		}
	}
	return false
}

type ghUser struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

// Self asks GitHub once for the token's own user.
func (g *githubTracker) Self(ctx context.Context) (string, error) {
	if g.selfID != 0 {
		return g.selfLogin, nil
	}
	var me ghUser
	if _, err := g.call(ctx, http.MethodGet, g.api+"/user", maxItemAnswer, nil, &me); err != nil {
		return "", err
	}
	if me.ID <= 0 || me.Login == "" {
		return "", errors.New("GitHub did not name the token's user")
	}
	g.selfLogin, g.selfID = me.Login, me.ID
	return me.Login, nil
}

// reporterLabels are the labels the reporter gives its issues, in the
// order they are listed: alert while the alert is open, alert-resolved once
// it cleared (docs/manual/alerts-and-dashboard.md, "Design note: a resolved
// issue leaves the alert label"). An issue moves only from the first to the
// second, so listing them in this order never misses one that moves while
// the job reads.
var reporterLabels = []string{"alert", "alert-resolved"}

// AlertIssues lists the alerts repository's issues that carry one of
// reporterLabels, label by label: the open ones, newest first, each to the
// first one opened before since, and the closed ones that closed at or
// after closedAfter (closed issues are read most recently updated first,
// and their reading stops at the first one last updated before
// closedAfter: an issue's last update is never before its closing). Only
// those count against a listing's bound: no other issue, no pull request
// and no issue closed before closedAfter. A label with more than
// maxPages*pageSize of them is an error, never a listing cut short.
//
// A listing read while labels change can miss an issue (one that moves to
// alert-resolved shifts the later pages of alert by one), so the job never
// takes a listing as proof an issue was handled: it keeps the issues it
// handled in its own record, and one missed now is listed by the next poll.
func (g *githubTracker) AlertIssues(ctx context.Context, since, closedAfter time.Time) ([]issue, error) {
	var out []issue
	for _, label := range reporterLabels {
		for _, state := range []string{"open", "closed"} {
			listed, err := g.labelIssues(ctx, label, state, since, closedAfter)
			if err != nil {
				return nil, err
			}
			out = append(out, listed...)
		}
	}
	return out, nil
}

// maxListPages bounds the pages of one listing, whatever they hold: a
// GitHub whose listing never ends is an error, never a poll that never
// ends.
const maxListPages = 10 * maxPages

// labelIssues is one label's and one state's listing of AlertIssues.
func (g *githubTracker) labelIssues(ctx context.Context, label, state string, since, closedAfter time.Time) ([]issue, error) {
	query := fmt.Sprintf("labels=%s&state=open&sort=created", url.QueryEscape(label))
	if state == "closed" {
		query = fmt.Sprintf("labels=%s&state=closed&sort=updated&since=%s", url.QueryEscape(label), url.QueryEscape(closedAfter.UTC().Format(time.RFC3339)))
	}
	var out []issue
	for page := 1; ; page++ {
		if page > maxListPages {
			return nil, fmt.Errorf("GitHub's listing of the %s issues labelled %s did not end within %d pages", state, label, maxListPages)
		}
		var items []struct {
			Number      int             `json:"number"`
			Title       string          `json:"title"`
			Body        string          `json:"body"`
			User        *ghUser         `json:"user"`
			State       string          `json:"state"`
			CreatedAt   time.Time       `json:"created_at"`
			UpdatedAt   time.Time       `json:"updated_at"`
			ClosedAt    *time.Time      `json:"closed_at"`
			PullRequest json.RawMessage `json:"pull_request"`
		}
		h, err := g.call(ctx, http.MethodGet, fmt.Sprintf("%s/issues?%s&direction=desc&per_page=%d&page=%d", g.alerts, query, pageSize, page), maxPageAnswer, nil, &items)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if len(it.PullRequest) > 0 {
				continue
			}
			// Every item is checked before it is used as the stopping point:
			// a missing time or state is a malformed answer, never the end.
			switch {
			case it.CreatedAt.IsZero():
				return nil, errors.New("GitHub's listing named an issue without its creation time")
			case it.Number <= 0 || it.User == nil:
				return nil, errors.New("GitHub's listing named an issue without its number or its author")
			case it.State != state:
				return nil, fmt.Errorf("GitHub's listing of %s issues named one whose state is %q", state, it.State)
			case state == "closed" && (it.ClosedAt == nil || it.ClosedAt.IsZero() || it.UpdatedAt.IsZero()):
				return nil, errors.New("GitHub's listing named a closed issue without its closing time or its last update")
			case state == "closed" && it.UpdatedAt.Before(*it.ClosedAt):
				return nil, errors.New("GitHub's listing named a closed issue whose last update is before its closing")
			}
			if state == "open" && it.CreatedAt.Before(since) {
				return out, nil
			}
			if state == "closed" && it.UpdatedAt.Before(closedAfter) {
				return out, nil
			}
			if it.CreatedAt.Before(since) || (state == "closed" && it.ClosedAt.Before(closedAfter)) {
				continue
			}
			if len(out) >= maxPages*pageSize {
				scope := "open and opened since the job began"
				if state == "closed" {
					scope = "closed in the last 30 days"
				}
				return nil, fmt.Errorf("more than %d issues carry the label %s and are %s, so none is analysed until the job's record is moved aside", maxPages*pageSize, label, scope)
			}
			is := issue{Number: it.Number, Title: it.Title, Body: it.Body, Author: it.User.Login, Created: it.CreatedAt}
			if state == "closed" {
				is.Closed = *it.ClosedAt
			}
			out = append(out, is)
		}
		if !nextPage(h) {
			return out, nil
		}
	}
}

// IssueURL is an alert issue's address on github.com.
func (g *githubTracker) IssueURL(number int) string {
	return fmt.Sprintf("https://github.com/%s/issues/%d", g.alertsRepo, number)
}

// OwnComments reads every page of the issue's comments, at most maxPages,
// and returns the bodies of those the token's user wrote. A comment GitHub
// names no author of could be the job's own, so it fails the read.
func (g *githubTracker) OwnComments(ctx context.Context, number int) ([]string, error) {
	if _, err := g.Self(ctx); err != nil {
		return nil, err
	}
	var own []string
	for page := 1; ; page++ {
		if page > maxPages {
			return nil, fmt.Errorf("the issue has more than %d comments, so whether it was analysed cannot be told", maxPages*pageSize)
		}
		var items []struct {
			Body string  `json:"body"`
			User *ghUser `json:"user"`
		}
		h, err := g.call(ctx, http.MethodGet, fmt.Sprintf("%s/issues/%d/comments?per_page=%d&page=%d", g.alerts, number, pageSize, page), maxPageAnswer, nil, &items)
		if err != nil {
			return nil, err
		}
		for _, c := range items {
			if c.User == nil || c.User.ID <= 0 {
				return nil, errors.New("GitHub named a comment without its author, so whether the issue was analysed cannot be told")
			}
			if c.User.ID == g.selfID {
				own = append(own, c.Body)
			}
		}
		if !nextPage(h) {
			return own, nil
		}
	}
}

// Comment adds one comment to an issue.
func (g *githubTracker) Comment(ctx context.Context, number int, body string) error {
	_, err := g.call(ctx, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", g.alerts, number), maxItemAnswer, map[string]string{"body": body}, nil)
	return err
}

// OpenDraftPull opens a draft pull request from head into main of the code
// repository. The job
// never merges, never marks a PR ready and never asks for a review: this is
// its only pull request call.
func (g *githubTracker) OpenDraftPull(ctx context.Context, head, title, body string) (string, error) {
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if _, err := g.callAs(ctx, g.codeToken, http.MethodPost, g.code+"/pulls", maxItemAnswer, map[string]any{"title": title, "head": head, "base": "main", "body": body, "draft": true}, &out); err != nil {
		return "", err
	}
	if !regexp.MustCompile(`^https://github\.com/` + regexp.QuoteMeta(g.codeRepo) + `/pull/\d+$`).MatchString(out.HTMLURL) {
		return "", errors.New("GitHub's answer named no pull request of this repository")
	}
	return out.HTMLURL, nil
}
