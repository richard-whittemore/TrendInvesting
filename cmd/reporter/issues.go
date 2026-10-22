package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/richard-whittemore/TrendInvesting/internal/journalquery"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// The github-issues channel turns each alert into one issue the owner reads
// on a phone (ADR 0019, "Alerting"; docs/manual/alerts-and-dashboard.md,
// "Alerts as GitHub Issues"). The repository is public, so an issue carries
// the alert's public text when it has one and nothing that is a secret, an
// account id, a Tailscale host or a figure (report.Redactor).

const (
	issuesChannel = "github-issues"
	// alertLabel is the label every alert issue carries, and the one the
	// alert issues are listed by.
	alertLabel          = "alert"
	defaultGitHubAPIURL = "https://api.github.com"
	// issueEvidenceRows is how many of the journal's last records, and of each
	// query's last rows, an issue shows.
	issueEvidenceRows = 10
	maxIssueTitle     = 200
	// maxIssueText is GitHub's limit on an issue's body and on a comment
	// (65,536 characters); counted in bytes, which is never fewer.
	maxIssueText = 65536
	// maxIssuePages bounds the listing of alert issues, 100 an issue page, so
	// 1,000 are matched; a longer listing is an error, never taken as whole.
	maxIssuePages = 10
	// maxDeliveryRequests bounds the requests of one delivery: the listing,
	// then at most two pages of the issue's comments, a comment and a close.
	maxDeliveryRequests = maxIssuePages + 4
)

// trackedIssue is the newest issue of an alert key, as the listing shows it.
type trackedIssue struct {
	Number   int
	Open     bool
	Comments int
}

// issueTracker is the issues of one repository, as the channel needs them.
// The reporter's tracker is GitHub's; tests use an in-memory one. Every call
// runs within ctx, the one deadline of the delivery it is part of.
type issueTracker interface {
	// Latest returns the newest issue, open or closed, whose body holds
	// marker.
	Latest(ctx context.Context, marker string) (issue trackedIssue, found bool, err error)
	// Create opens an issue and returns its number.
	Create(ctx context.Context, title, body string, labels []string) (int, error)
	// LastComments returns the bodies of the issue's latest comments, its
	// last one among them.
	LastComments(ctx context.Context, number int) ([]string, error)
	// Comment adds a comment to an issue.
	Comment(ctx context.Context, number int, body string) error
	// CloseIssue closes an issue.
	CloseIssue(ctx context.Context, number int) error
}

// issuesSender delivers an alert as an issue: the first delivery opens it, a
// repeat adds one comment to the open issue, and resolve closes it. Each
// write carries its delivery's identity (deliveryMarker), so a delivery sent
// again, after its answer was lost, writes nothing more. An issue the owner
// closed by hand is left closed: a repeat of the same incident neither
// reopens nor replaces it, and only an alert that resolved and came back, a
// new incident, opens a new issue (docs/manual/alerts-and-dashboard.md, "How
// an issue closes").
//
// One delivery runs under one deadline, the send timeout (-send-timeout, the
// longest one send to one channel may take), for all of its requests
// together, and a tracker that failed its listing or ran out of time is not
// asked again in the firing: the later alerts fail at once, so a GitHub that
// hangs costs the firing one timeout (ADR 0019, "Alerting": one channel's
// fault must not silence another).
type issuesSender struct {
	tracker  issueTracker
	redactor report.Redactor
	// evidence, when set, is the journal's redacted records and query output
	// for the issue's body; the run sets it once the journal is read.
	evidence func() string
	// timeout is the deadline of one delivery; zero is none.
	timeout time.Duration
	// down is the tracker's failure earlier in the firing.
	down error
}

func (*issuesSender) name() string { return issuesChannel }

// tracksIssues marks the channel that writes to an issue tracker: it is sent
// each alert after every other channel, so a slow or failing tracker never
// holds back a page.
func (*issuesSender) tracksIssues() {}

// issueMarker is the hidden line in an issue's body that ties it to its alert
// key. It carries the key's hash, never the key.
func issueMarker(key string) string {
	return "<!-- ti-alert-key:" + keyHash(key) + " -->"
}

func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// deliveryMarker is the hidden line of the comment of one delivery of an
// alert: its key's hash and its repeat ordinal.
func deliveryMarker(key string, repeat int) string {
	return fmt.Sprintf("<!-- ti-alert-delivery:%s#%d -->", keyHash(key), repeat)
}

// resolvedMarker is the hidden line of the comment that closes an alert's
// issue.
func resolvedMarker(key string) string {
	return "<!-- ti-alert-resolved:" + keyHash(key) + " -->"
}

// text is the alert's words for a public place: its public text when it has
// one, which carries no figure (owner decision 2026-10-17,
// docs/live-trading/plan.md section 7), redacted all the same.
func (s *issuesSender) text(a report.Alert) string {
	body := a.Body
	if a.PublicBody != "" {
		body = a.PublicBody
	}
	return s.redactor.Text(body)
}

// begin starts one delivery: its deadline and its request budget, or the
// tracker's failure earlier in the firing.
func (s *issuesSender) begin() (context.Context, context.CancelFunc, error) {
	if s.down != nil {
		return nil, nil, fmt.Errorf("not tried: GitHub failed earlier in this firing (%w)", s.down)
	}
	ctx, cancel := context.Background(), context.CancelFunc(func() {})
	if s.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
	}
	return withRequestBudget(ctx, maxDeliveryRequests), cancel, nil
}

// note remembers, for the rest of the firing, a failed listing and a
// delivery that ran out of time, and returns err.
func (s *issuesSender) note(err error, listing bool) error {
	if err != nil && (listing || errors.Is(err, context.DeadlineExceeded)) {
		s.down = err
	}
	return err
}

func (s *issuesSender) send(a report.Alert, repeat int) error {
	ctx, cancel, err := s.begin()
	if err != nil {
		return err
	}
	defer cancel()
	marker := issueMarker(a.Key)
	issue, found, err := s.tracker.Latest(ctx, marker)
	if err != nil {
		return s.note(err, true)
	}
	switch {
	case found && issue.Open && repeat == 0:
		// The open issue is this delivery's, made by a send whose answer was
		// lost.
		return nil
	case found && issue.Open:
		id := deliveryMarker(a.Key, repeat)
		last, err := s.tracker.LastComments(ctx, issue.Number)
		if err != nil {
			return s.note(err, false)
		}
		if slices.ContainsFunc(last, func(c string) bool { return strings.Contains(c, id) }) {
			return nil
		}
		return s.note(s.tracker.Comment(ctx, issue.Number, fitIssueText(fmt.Sprintf("Still open (repeat %d).\n\n%s", repeat, s.text(a)), "", "\n\n"+id)), false)
	case found && repeat > 0:
		// Closed by hand while the incident goes on: left closed.
		return nil
	}
	title := s.redactor.Text(a.Title)
	if r := []rune(title); len(r) > maxIssueTitle {
		title = string(r[:maxIssueTitle])
	}
	evidence := ""
	if s.evidence != nil {
		evidence = s.redactor.Text(s.evidence())
	}
	_, err = s.tracker.Create(ctx, title, fitIssueText(s.text(a), evidence, "\n\n"+marker), []string{alertLabel})
	return s.note(err, false)
}

// resolve closes the open issue of the alert keyed key, with one closing
// comment: a close whose comment was posted but whose state change failed
// only closes when tried again. An alert with no open issue, one the owner
// closed already, is resolved.
func (s *issuesSender) resolve(key string) error {
	ctx, cancel, err := s.begin()
	if err != nil {
		return err
	}
	defer cancel()
	issue, found, err := s.tracker.Latest(ctx, issueMarker(key))
	if err != nil {
		return s.note(err, true)
	}
	if !found || !issue.Open {
		return nil
	}
	id := resolvedMarker(key)
	last, err := s.tracker.LastComments(ctx, issue.Number)
	if err != nil {
		return s.note(err, false)
	}
	if !slices.ContainsFunc(last, func(c string) bool { return strings.Contains(c, id) }) {
		if err := s.tracker.Comment(ctx, issue.Number, "Resolved: this alert has cleared.\n\n"+id); err != nil {
			return s.note(err, false)
		}
	}
	return s.note(s.tracker.CloseIssue(ctx, issue.Number), false)
}

// issueCutNote ends a text cut to fit GitHub's limit.
const issueCutNote = "\n\n[cut to fit GitHub's limit; the rest is in the journal on the server]"

// fitIssueText is text, then evidence, then tail, within GitHub's limit
// (maxIssueText): the evidence is cut first, then the text, each at a whole
// character and said so, and tail, the hidden marker, is always kept whole.
func fitIssueText(text, evidence, tail string) string {
	whole := text
	if evidence != "" {
		whole += "\n\n" + evidence
	}
	if len(whole)+len(tail) <= maxIssueText {
		return whole + tail
	}
	room := maxIssueText - len(tail) - len(issueCutNote) - len("\n```")
	cut := whole[:room]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	if strings.Count(cut, "```")%2 == 1 {
		cut += "\n```"
	}
	return cut + issueCutNote + tail
}

// resolvedKey is the state-file key recording that a channel has closed the
// issue of an acknowledged alert, so the close is made once.
func resolvedKey(key string) string { return "resolved:" + key }

// closeAcknowledged closes, on every channel that can close what it opened,
// the issue of each repeating alert the owner has acknowledged (ADR 0019,
// amended 2026-09-24: an acknowledgement is recorded in the journal). The
// alert stays recorded as delivered, so it is not opened again; the close is
// recorded once it succeeds, and a failed close is tried at the next firing.
func closeAcknowledged(senders []sender, alerts []report.Alert, seen map[string]bool) error {
	var failures []error
	for _, a := range alerts {
		if !a.Repeat || !a.Acknowledged {
			continue
		}
		for _, s := range senders {
			r, ok := s.(*issuesSender)
			if !ok || !seen[channelKey(s.name(), a.Key)] || seen[channelKey(s.name(), resolvedKey(a.Key))] {
				continue
			}
			if err := r.resolve(a.Key); err != nil {
				failures = append(failures, fmt.Errorf("%s: resolve: %w", s.name(), err))
				continue
			}
			seen[channelKey(s.name(), resolvedKey(a.Key))] = true
		}
	}
	return errors.Join(failures...)
}

// githubIssuesFrom builds the channel from its secrets: the issues-only
// fine-grained token, the repository as owner/name, and GitHub's API address,
// api.github.com when unset. Neither the token nor the address's text appears
// in an error. One delivery's deadline is the client's timeout, the send
// timeout.
func githubIssuesFrom(getenv func(string) string, client *http.Client) (sender, error) {
	token := getenv("TI_GITHUB_ISSUES_TOKEN")
	if token == "" {
		return nil, errors.New("reporter: the github-issues channel needs TI_GITHUB_ISSUES_TOKEN (a fine-grained token with Issues read and write on the one repository; it is a secret)")
	}
	repo := getenv("TI_GITHUB_ISSUES_REPO")
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || strings.ContainsAny(repo, " ?#") {
		return nil, errors.New("reporter: the github-issues channel needs TI_GITHUB_ISSUES_REPO as owner/name, the repository the alert issues are opened in")
	}
	api := getenv("TI_GITHUB_API_URL")
	if api == "" {
		api = defaultGitHubAPIURL
	}
	if u, err := url.Parse(api); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("reporter: TI_GITHUB_API_URL must be an absolute URL such as https://api.github.com")
	}
	return &issuesSender{
		tracker:  &githubTracker{base: strings.TrimRight(api, "/") + "/repos/" + owner + "/" + name, token: token, client: client},
		redactor: report.NewRedactor(append([]string{token}, hostLiterals()...)),
		timeout:  client.Timeout,
	}, nil
}

// githubTracker is one repository's issues over GitHub's REST API
// (https://docs.github.com/en/rest/issues). The alert issues, open and
// closed, are listed once per firing, newest first, and kept, so a firing's
// lookups cost one listing, not one per alert.
type githubTracker struct {
	base, token string
	client      *http.Client

	mu     sync.Mutex
	loaded bool
	latest map[string]trackedIssue // marker -> the newest issue holding it
}

// ghIssue is the part of an issue the channel reads.
type ghIssue struct {
	Number      int             `json:"number"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	Comments    int             `json:"comments"`
	PullRequest json.RawMessage `json:"pull_request"`
}

// requestBudget is the count of requests one delivery may still make.
type requestBudget struct{}

func withRequestBudget(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, requestBudget{}, &n)
}

func (g *githubTracker) call(ctx context.Context, method, path string, in, out any) (http.Header, error) {
	if left, ok := ctx.Value(requestBudget{}).(*int); ok {
		if *left <= 0 {
			return nil, errors.New("this delivery used every request it may make")
		}
		*left--
	}
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, errors.New("build request")
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, body)
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
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	if resp.StatusCode/100 != 2 {
		// A limit GitHub applies is the channel's failure like any other: the
		// alert is sent again at a later firing, and a firing that comes
		// before the limit lifts fails the same way.
		if resp.StatusCode == http.StatusTooManyRequests || resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" {
			return nil, fmt.Errorf("GitHub answered %s: rate limited, tried again at a later firing", resp.Status)
		}
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	if out != nil {
		if err := json.Unmarshal(answer, out); err != nil {
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

// load lists the alert issues, open and closed, newest first, once. A
// listing longer than maxIssuePages is an error: an issue beyond it would be
// taken for absent and opened again.
func (g *githubTracker) load(ctx context.Context) error {
	if g.loaded {
		return nil
	}
	latest := map[string]trackedIssue{}
	for page := 1; ; page++ {
		if page > maxIssuePages {
			return fmt.Errorf("more than %d issues carry the %s label, so the list of alert issues is incomplete and none is written; remove the label from old closed ones", maxIssuePages*100, alertLabel)
		}
		var issues []ghIssue
		h, err := g.call(ctx, http.MethodGet, fmt.Sprintf("/issues?labels=%s&state=all&sort=created&direction=desc&per_page=100&page=%d", alertLabel, page), nil, &issues)
		if err != nil {
			return err
		}
		for _, i := range issues {
			if len(i.PullRequest) > 0 {
				continue
			}
			if m := markerIn(i.Body); m != "" {
				if _, newer := latest[m]; !newer {
					latest[m] = trackedIssue{Number: i.Number, Open: i.State == "open", Comments: i.Comments}
				}
			}
		}
		if !nextPage(h) {
			break
		}
	}
	g.latest, g.loaded = latest, true
	return nil
}

// markerIn returns the marker line an issue's body holds, "" if none.
func markerIn(body string) string {
	const prefix = "<!-- ti-alert-key:"
	i := strings.Index(body, prefix)
	if i < 0 {
		return ""
	}
	end := strings.Index(body[i:], "-->")
	if end < 0 {
		return ""
	}
	return body[i : i+end+3]
}

func (g *githubTracker) Latest(ctx context.Context, marker string) (trackedIssue, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.load(ctx); err != nil {
		return trackedIssue{}, false, err
	}
	i, ok := g.latest[marker]
	return i, ok, nil
}

// Create opens an issue. Only an answer that names the new issue is a
// delivery.
func (g *githubTracker) Create(ctx context.Context, title, body string, labels []string) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out ghIssue
	if _, err := g.call(ctx, http.MethodPost, "/issues", map[string]any{"title": title, "body": body, "labels": labels}, &out); err != nil {
		return 0, err
	}
	if out.Number <= 0 {
		return 0, errors.New("GitHub's answer named no issue")
	}
	if g.loaded {
		g.latest[markerIn(body)] = trackedIssue{Number: out.Number, Open: true}
	}
	return out.Number, nil
}

// LastComments reads the issue's last page of comments, found from the
// listing's count and followed to the end.
func (g *githubTracker) LastComments(ctx context.Context, number int) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	page := 1
	for _, i := range g.latest {
		if i.Number == number && i.Comments > 0 {
			page = (i.Comments + 99) / 100
		}
	}
	var bodies []string
	for {
		var comments []struct {
			Body string `json:"body"`
		}
		h, err := g.call(ctx, http.MethodGet, fmt.Sprintf("/issues/%d/comments?per_page=100&page=%d", number, page), nil, &comments)
		if err != nil {
			return nil, err
		}
		for _, c := range comments {
			bodies = append(bodies, c.Body)
		}
		if !nextPage(h) {
			return bodies, nil
		}
		page++
	}
}

func (g *githubTracker) Comment(ctx context.Context, number int, body string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.call(ctx, http.MethodPost, fmt.Sprintf("/issues/%d/comments", number), map[string]string{"body": body}, nil); err != nil {
		return err
	}
	for m, i := range g.latest {
		if i.Number == number {
			i.Comments++
			g.latest[m] = i
		}
	}
	return nil
}

func (g *githubTracker) CloseIssue(ctx context.Context, number int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.call(ctx, http.MethodPatch, fmt.Sprintf("/issues/%d", number), map[string]string{"state": "closed", "state_reason": "completed"}, nil); err != nil {
		return err
	}
	for m, i := range g.latest {
		if i.Number == number {
			i.Open = false
			g.latest[m] = i
		}
	}
	return nil
}

// withIssueEvidence gives the issues channel, once the journal reads, the
// text for its issues: the journal's last records and the journal-query
// output, built when first needed and redacted.
func withIssueEvidence(senders []sender, journalPath string) {
	for _, s := range senders {
		if is, ok := s.(*issuesSender); ok {
			var once sync.Once
			var text string
			is.evidence = func() string {
				once.Do(func() { text = issueEvidence(journalPath, is.redactor) })
				return text
			}
		}
	}
}

// issueEvidence is the body's journal section: the last records, then the
// output of journal-query's reconcile, refusals and orders cut to their last
// rows, all through one filter (report.Withheld names the fields and columns
// left out, and every figure left is removed), since the issue is public. A
// journal that cannot be read is said so; the alert still opens its issue.
func issueEvidence(path string, r report.Redactor) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "The journal could not be read for this issue."
	}
	_, envelopes, err := report.Load(raw)
	if err != nil {
		return "The journal could not be verified for this issue, so none of it is shown."
	}
	var b strings.Builder
	b.WriteString("### Last journal records (redacted)\n\n```text\n")
	for _, e := range envelopes[max(0, len(envelopes)-issueEvidenceRows):] {
		b.WriteString(r.Record(e) + "\n")
	}
	b.WriteString("```\n")
	loaded, err := journalquery.Load("journal", raw)
	if err != nil {
		return b.String()
	}
	journals := []journalquery.Journal{loaded}
	filter := journalquery.Filter{AllowTrailingFills: true}
	for _, q := range []struct {
		title string
		run   func([]journalquery.Journal, journalquery.Filter) (journalquery.Table, error)
	}{
		{"journal-query reconcile", journalquery.Reconcile},
		{"journal-query refusals", journalquery.Refusals},
		{"journal-query orders", journalquery.Orders},
	} {
		// Reconcile returns its table with the disagreement it found, which is
		// the reason to show it; a query that returned no table says so.
		table, qerr := q.run(journals, filter)
		fmt.Fprintf(&b, "\n### %s (redacted)\n\n```text\n", q.title)
		if len(table.Columns) == 0 {
			fmt.Fprintf(&b, "%s\n```\n", r.Text(fmt.Sprint(qerr)))
			continue
		}
		var text bytes.Buffer
		_ = redactedTable(table, r).WriteText(&text)
		b.WriteString(text.String())
		if qerr != nil {
			b.WriteString(r.Text(qerr.Error()) + "\n")
		}
		b.WriteString("```\n")
	}
	return b.String()
}

// redactedTable is t without its withheld columns, cut to its last rows, and
// with every cell redacted; a record's sequence number is kept, as #n.
func redactedTable(t journalquery.Table, r report.Redactor) journalquery.Table {
	var keep []int
	var out journalquery.Table
	for i, c := range t.Columns {
		if !report.Withheld(c) {
			keep = append(keep, i)
			out.Columns = append(out.Columns, c)
		}
	}
	rows := t.Rows[max(0, len(t.Rows)-issueEvidenceRows):]
	for _, row := range rows {
		cells := make([]string, 0, len(keep))
		for _, i := range keep {
			cell := row[i]
			if _, err := strconv.ParseUint(cell, 10, 64); err == nil && t.Columns[i] == "seq" {
				// A record number reads #n, as in the records above, and is kept.
				cell = "#" + cell
			}
			cells = append(cells, r.Text(cell))
		}
		out.Rows = append(out.Rows, cells)
	}
	return out
}

// hostLiterals are this host's names, which never reach an issue: a Tailscale
// host is named by the machine's own name. A short name is kept too; the
// redactor removes it where it stands as a word.
func hostLiterals() []string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return nil
	}
	short, _, _ := strings.Cut(h, ".")
	return []string{h, short}
}

// withRedaction gives the issues channel every secret the run holds, from the
// environment and the secrets file, to remove from what it writes.
func withRedaction(senders []sender, secrets []string) {
	for _, s := range senders {
		if is, ok := s.(*issuesSender); ok {
			is.redactor = report.NewRedactor(append(slices.Clone(secrets), hostLiterals()...))
		}
	}
}
