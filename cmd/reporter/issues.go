package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

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
	// alertLabel is the label every alert issue carries, and the one the open
	// issues are looked up by.
	alertLabel          = "alert"
	defaultGitHubAPIURL = "https://api.github.com"
	// issueEvidenceRows is how many of the journal's last records, and of each
	// query's last rows, an issue shows.
	issueEvidenceRows = 10
	maxIssueTitle     = 200
	// maxIssuePages bounds the look-up of open alert issues: 100 an issue
	// page, so 1,000 open alerts are matched.
	maxIssuePages = 10
)

// issueTracker is the issues of one repository, as the channel needs them.
// The reporter's tracker is GitHub's; tests use an in-memory one.
type issueTracker interface {
	// FindOpen returns the number of the open issue whose body holds marker.
	FindOpen(marker string) (number int, found bool, err error)
	// Create opens an issue and returns its number.
	Create(title, body string, labels []string) (int, error)
	// Comment adds a comment to an issue.
	Comment(number int, body string) error
	// Close adds a closing comment to an issue and closes it.
	Close(number int, body string) error
}

// issuesSender delivers an alert as an issue: the first delivery opens it, a
// repeat adds one comment to the open issue, and resolve closes it. A firing
// delivers an alert once per channel, so an issue gets one write per key per
// firing, inside GitHub's rate limits.
type issuesSender struct {
	tracker  issueTracker
	redactor report.Redactor
	// evidence, when set, is the journal's redacted records and query output
	// for the issue's body; the run sets it once the journal is read.
	evidence func() string
}

func (*issuesSender) name() string { return issuesChannel }

// issueMarker is the hidden line in an issue's body that ties it to its alert
// key. It carries the key's hash, never the key.
func issueMarker(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "<!-- ti-alert-key:" + hex.EncodeToString(sum[:]) + " -->"
}

// text is the alert's words for a public place: its public text when it has
// one, which carries no figure (owner decision 2026-10-17,
// docs/live-trading/plan.md section 7), redacted.
func (s *issuesSender) text(a report.Alert) string {
	body := a.Body
	if a.PublicBody != "" {
		body = a.PublicBody
	}
	return s.redactor.Text(body)
}

func (s *issuesSender) send(a report.Alert, repeat int) error {
	marker := issueMarker(a.Key)
	number, found, err := s.tracker.FindOpen(marker)
	if err != nil {
		return err
	}
	if found {
		return s.tracker.Comment(number, fmt.Sprintf("Still open (repeat %d).\n\n%s", repeat, s.text(a)))
	}
	title := s.redactor.Text(a.Title)
	if r := []rune(title); len(r) > maxIssueTitle {
		title = string(r[:maxIssueTitle])
	}
	body := s.text(a)
	if s.evidence != nil {
		body += "\n\n" + s.redactor.Text(s.evidence())
	}
	_, err = s.tracker.Create(title, body+"\n\n"+marker, []string{alertLabel})
	return err
}

// resolve closes the open issue of the alert keyed key. An alert with no open
// issue, one the owner closed already, is resolved.
func (s *issuesSender) resolve(key string) error {
	number, found, err := s.tracker.FindOpen(issueMarker(key))
	if err != nil || !found {
		return err
	}
	return s.tracker.Close(number, "Resolved: this alert has cleared.")
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
// in an error.
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
	return &issuesSender{tracker: &githubTracker{base: strings.TrimRight(api, "/") + "/repos/" + owner + "/" + name, token: token, client: client}, redactor: report.NewRedactor(append([]string{token}, hostLiterals()...))}, nil
}

// githubTracker is one repository's issues over GitHub's REST API
// (https://docs.github.com/en/rest/issues). The open alert issues are listed
// once per firing and kept, so a firing's lookups cost one request, not one
// per alert.
type githubTracker struct {
	base, token string
	client      *http.Client

	mu     sync.Mutex
	loaded bool
	open   map[string]int // marker -> issue number
}

// ghIssue is the part of an issue the channel reads.
type ghIssue struct {
	Number      int             `json:"number"`
	Body        string          `json:"body"`
	PullRequest json.RawMessage `json:"pull_request"`
}

func (g *githubTracker) call(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return errors.New("build request")
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, g.base+path, body)
	if err != nil {
		return errors.New("build request")
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
			return fmt.Errorf("request failed: %w", urlErr.Err)
		}
		return errors.New("request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	if resp.StatusCode/100 != 2 {
		// A limit GitHub applies is the channel's failure like any other: the
		// alert is sent again at the next firing, which is later than the
		// Retry-After GitHub names.
		if resp.StatusCode == http.StatusTooManyRequests || resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" {
			return fmt.Errorf("GitHub answered %s: rate limited, tried again at the next firing", resp.Status)
		}
		return fmt.Errorf("GitHub answered %s", resp.Status)
	}
	if out != nil {
		if err := json.Unmarshal(answer, out); err != nil {
			return errors.New("GitHub's answer was not understood")
		}
	}
	return nil
}

// load lists the open alert issues once.
func (g *githubTracker) load() error {
	if g.loaded {
		return nil
	}
	open := map[string]int{}
	for page := 1; page <= maxIssuePages; page++ {
		var issues []ghIssue
		path := fmt.Sprintf("/issues?labels=%s&state=open&per_page=100&page=%d", alertLabel, page)
		if err := g.call(http.MethodGet, path, nil, &issues); err != nil {
			return err
		}
		for _, i := range issues {
			if len(i.PullRequest) > 0 {
				continue
			}
			if m := markerIn(i.Body); m != "" {
				open[m] = i.Number
			}
		}
		if len(issues) < 100 {
			break
		}
	}
	g.open, g.loaded = open, true
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

func (g *githubTracker) FindOpen(marker string) (number int, found bool, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.load(); err != nil {
		return 0, false, err
	}
	n, ok := g.open[marker]
	return n, ok, nil
}

func (g *githubTracker) Create(title, body string, labels []string) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out ghIssue
	if err := g.call(http.MethodPost, "/issues", map[string]any{"title": title, "body": body, "labels": labels}, &out); err != nil {
		return 0, err
	}
	if g.loaded {
		g.open[markerIn(body)] = out.Number
	}
	return out.Number, nil
}

func (g *githubTracker) Comment(number int, body string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.call(http.MethodPost, fmt.Sprintf("/issues/%d/comments", number), map[string]string{"body": body}, nil)
}

func (g *githubTracker) Close(number int, body string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.call(http.MethodPost, fmt.Sprintf("/issues/%d/comments", number), map[string]string{"body": body}, nil); err != nil {
		return err
	}
	if err := g.call(http.MethodPatch, fmt.Sprintf("/issues/%d", number), map[string]string{"state": "closed", "state_reason": "completed"}, nil); err != nil {
		return err
	}
	for m, n := range g.open {
		if n == number {
			delete(g.open, m)
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

// droppedColumns are the query columns that hold a quantity, a price or an
// account: with a price they give a position's size, and the issue is public.
var droppedColumns = map[string]bool{
	"account": true, "quantity": true, "price": true, "commission": true,
	"fills": true, "snapshot": true, "change": true, "position": true, "journal": true,
}

// issueEvidence is the body's journal section: the last records as redacted
// text, then the output of journal-query's reconcile, refusals and orders,
// each without its quantities and prices and cut to its last rows. A journal
// that cannot be read is said so; the alert still opens its issue.
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
		_ = trimmed(table).WriteText(&text)
		b.WriteString(r.Text(text.String()))
		if qerr != nil {
			b.WriteString(r.Text(qerr.Error()) + "\n")
		}
		b.WriteString("```\n")
	}
	return b.String()
}

// trimmed is t without its dropped columns and cut to its last rows.
func trimmed(t journalquery.Table) journalquery.Table {
	var keep []int
	var out journalquery.Table
	for i, c := range t.Columns {
		if !droppedColumns[c] {
			keep = append(keep, i)
			out.Columns = append(out.Columns, c)
		}
	}
	rows := t.Rows[max(0, len(t.Rows)-issueEvidenceRows):]
	for _, row := range rows {
		cells := make([]string, 0, len(keep))
		for _, i := range keep {
			cells = append(cells, row[i])
		}
		out.Rows = append(out.Rows, cells)
	}
	return out
}

// hostLiterals are this host's names, which never reach an issue: a Tailscale
// host is named by the machine's own name.
func hostLiterals() []string {
	h, err := os.Hostname()
	if err != nil || len(h) < 4 {
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
			is.redactor = report.NewRedactor(append(secrets, append([]string{}, hostLiterals()...)...))
		}
	}
}
