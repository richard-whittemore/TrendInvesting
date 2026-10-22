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

// githubTracker is the repository's issues and pulls over GitHub's REST API
// (https://docs.github.com/en/rest), under the reporter's own rules for the
// token (docs/manual/alerts-and-dashboard.md, "Alerts as GitHub Issues"):
// the token goes only to api.github.com over https, or to the one host the
// operator names; a redirect is refused, never followed; every answer is
// read to a bound and decoded whole or not at all; a listing longer than its
// bound is an error, never taken as whole.
type githubTracker struct {
	api, base, owner, name, token string
	client                        *http.Client
	maxAnswer                     int64 // tests only: the read limit of every answer

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

// newGitHubTracker builds the client for repo (owner/name) with token, sent
// only to api (checkAPIURL).
func newGitHubTracker(api, allow, repo, token string, client *http.Client) (*githubTracker, error) {
	if token == "" {
		return nil, errors.New("triage: no GitHub token: the token file is empty")
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || strings.ContainsAny(repo, " ?#") {
		return nil, errors.New("triage: -repo must be owner/name, the repository the alert issues are in")
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
	return &githubTracker{api: api, base: api + "/repos/" + owner + "/" + name, owner: owner, name: name, token: token, client: &noRedirect}, nil
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

// call is one request, its answer decoded into out, read to at most limit
// bytes. The token is never in an error.
func (g *githubTracker) call(ctx context.Context, method, target string, limit int64, in, out any) (http.Header, error) {
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
	req.Header.Set("Authorization", "Bearer "+g.token)
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

// AlertIssues lists label's issues, open and closed, newest first, to the
// first one opened before since.
func (g *githubTracker) AlertIssues(ctx context.Context, label string, since time.Time) ([]issue, error) {
	var out []issue
	for page := 1; ; page++ {
		if page > maxPages {
			return nil, fmt.Errorf("more than %d issues carry the %s label since the job began, so the list is incomplete and none is analysed", maxPages*pageSize, label)
		}
		var items []struct {
			Number      int             `json:"number"`
			Title       string          `json:"title"`
			Body        string          `json:"body"`
			User        *ghUser         `json:"user"`
			CreatedAt   time.Time       `json:"created_at"`
			PullRequest json.RawMessage `json:"pull_request"`
		}
		h, err := g.call(ctx, http.MethodGet, fmt.Sprintf("%s/issues?labels=%s&state=all&sort=created&direction=desc&per_page=%d&page=%d", g.base, url.QueryEscape(label), pageSize, page), maxPageAnswer, nil, &items)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if len(it.PullRequest) > 0 {
				continue
			}
			if it.CreatedAt.Before(since) {
				return out, nil
			}
			if it.Number <= 0 || it.User == nil {
				return nil, errors.New("GitHub's listing named an issue without its number or its author")
			}
			out = append(out, issue{Number: it.Number, Title: it.Title, Body: it.Body, Author: it.User.Login, Created: it.CreatedAt})
		}
		if !nextPage(h) {
			return out, nil
		}
	}
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
		h, err := g.call(ctx, http.MethodGet, fmt.Sprintf("%s/issues/%d/comments?per_page=%d&page=%d", g.base, number, pageSize, page), maxPageAnswer, nil, &items)
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
	_, err := g.call(ctx, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", g.base, number), maxItemAnswer, map[string]string{"body": body}, nil)
	return err
}

// OpenDraftPull opens a draft pull request from head into main. The job
// never merges, never marks a PR ready and never asks for a review: this is
// its only pull request call.
func (g *githubTracker) OpenDraftPull(ctx context.Context, head, title, body string) (string, error) {
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if _, err := g.call(ctx, http.MethodPost, g.base+"/pulls", maxItemAnswer, map[string]any{"title": title, "head": head, "base": "main", "body": body, "draft": true}, &out); err != nil {
		return "", err
	}
	if !regexp.MustCompile(`^https://github\.com/` + regexp.QuoteMeta(g.owner) + `/` + regexp.QuoteMeta(g.name) + `/pull/\d+$`).MatchString(out.HTMLURL) {
		return "", errors.New("GitHub's answer named no pull request of this repository")
	}
	return out.HTMLURL, nil
}
