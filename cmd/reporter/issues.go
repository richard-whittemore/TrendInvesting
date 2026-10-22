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
	"maps"
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
	alertLabel = "alert"
	// resolvedLabel replaces alertLabel on an issue the reporter resolved, so
	// the issue leaves the views and polls keyed by alertLabel (the reporter's
	// own listing, bounded) yet stays findable by a job that polls resolved
	// incidents by label.
	resolvedLabel       = "alert-resolved"
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
	// maxCommentPages bounds the reading of one issue's comments, 100 a page;
	// a longer one is an error, so a delivery whose comment cannot be ruled
	// out writes nothing.
	maxCommentPages = 10
	// maxDeliveryRequests bounds the requests of one delivery: the listing,
	// the channel's own identity, every page of the issue's comments, then a
	// write, a close, the resolved label's addition and the alert label's
	// removal.
	maxDeliveryRequests = maxIssuePages + 1 + maxCommentPages + 4
	// maxCloseTries is how many firings try to close the issue of an alert
	// the run forgot before its record is dropped, so that a close GitHub
	// keeps refusing never holds back the next incident (closeForgotten).
	maxCloseTries = 3
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
	// Listed returns the numbers of the issues in the alert listing, the
	// same listing Latest reads.
	Listed(ctx context.Context) (map[int]bool, error)
	// Create opens an issue and returns its number.
	Create(ctx context.Context, title, body string, labels []string) (int, error)
	// OwnComments returns the bodies of every comment on the issue written
	// by the channel's own identity, so a marker someone else wrote never
	// counts.
	OwnComments(ctx context.Context, number int) ([]string, error)
	// Comment adds a comment to an issue.
	Comment(ctx context.Context, number int, body string) error
	// CloseIssue closes an issue.
	CloseIssue(ctx context.Context, number int) error
	// MarkResolved gives an issue the resolved label and takes the alert
	// label off it, so it leaves the listing; an issue without the alert
	// label already is marked.
	MarkResolved(ctx context.Context, number int) error
}

// issuesSender delivers an alert as an issue: the first delivery opens it, a
// repeat or a reminder adds one comment to the open issue, and resolve
// closes it. Each write carries its delivery's identity (deliveryMarker,
// reminderMarker), so a delivery sent again, after its answer was lost,
// writes nothing more. An issue the owner closed by hand is left closed: the
// same incident's first delivery tried again, a repeat or a reminder neither
// reopens nor replaces it, and only an alert the reporter resolved (its
// closing comment, resolvedMarker, is on the issue) that came back, a new
// incident, opens a new issue. A repeat or a reminder whose issue is gone
// from the listing opens none (docs/manual/alerts-and-dashboard.md, "How an
// issue closes").
//
// A firing's tracker work, every delivery together, is bounded by one send
// timeout (-send-timeout, the longest one send to one channel may take),
// spent only while the tracker is being asked, so the firing's local work
// between deliveries is never charged to a later one; and a tracker that
// failed its listing, ran out of time, rate-limited or answered with a
// server error is not asked again in the firing: the later alerts fail at
// once, so a GitHub that hangs costs the firing one timeout (ADR 0019,
// "Alerting": one channel's fault must not silence another).
type issuesSender struct {
	tracker  issueTracker
	redactor report.Redactor
	// evidence, when set, is the journal's redacted records and query output
	// for the issue's body; the run sets it once the journal is read.
	evidence func() string
	// timeout is the firing's time for the tracker; zero is none.
	timeout time.Duration
	// left is the firing's time for the tracker not yet spent, once its
	// first delivery has begun (started).
	left    time.Duration
	started bool
	// down is the tracker's failure earlier in the firing.
	down error
	// records is the run's state file's counts (repeats), where the channel
	// keeps, across firings, the numbers of the issues whose close was given up
	// (unclosedKey), so its next incident opens a fresh issue; the run sets
	// it once the state file is read.
	records map[string]int
	// closing is, for each key resolve has read the issue of in this firing,
	// that issue's number.
	closing map[string]int
	pruned  bool
	// asked is, for each key resolve has sent a write to GitHub for in this
	// firing (a comment, the close or the label swap), true: a close that
	// failed before any write was never asked of GitHub.
	asked map[string]bool
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

// reminderMarker is the hidden line of the comment of a failed unit's
// reminder: its key's hash and the reminder's ordinal.
func reminderMarker(key string, ordinal int) string {
	return fmt.Sprintf("<!-- ti-alert-reminder:%s#%d -->", keyHash(key), ordinal)
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

// errFiringOver is the firing's time for the tracker run out.
var errFiringOver = errors.New("this firing's time for GitHub (the send timeout) has run out")

// begin starts one delivery: bounded by the firing's time for the tracker
// still unspent, and by the delivery's request budget, or refused with the
// tracker's failure earlier in the firing. The returned end charges the
// delivery's own time, and only it, to the firing.
func (s *issuesSender) begin() (context.Context, context.CancelFunc, error) {
	if s.down != nil {
		return nil, nil, fmt.Errorf("not tried: GitHub failed earlier in this firing (%w)", s.down)
	}
	ctx, end := context.Background(), context.CancelFunc(func() {})
	if s.timeout > 0 {
		if !s.started {
			s.left, s.started = s.timeout, true
		}
		if s.left <= 0 {
			s.down = errFiringOver
			return nil, nil, fmt.Errorf("not tried: %w", errFiringOver)
		}
		began := time.Now()
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.left)
		end = func() {
			cancel()
			s.left -= time.Since(began)
		}
	}
	return withRequestBudget(ctx, maxDeliveryRequests), end, nil
}

// note remembers, for the rest of the firing, a failed listing, a delivery
// that ran out of time and a tracker that is unavailable (errUnavailable: a
// rate limit, a server error, a request or an answer that failed on the
// way), and returns err. A failed lookup of the channel's own identity is
// remembered by the tracker (githubTracker.OwnComments), so only the
// deliveries that need it fail on it.
func (s *issuesSender) note(err error, listing bool) error {
	if err != nil && (listing || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errUnavailable)) {
		s.down = err
	}
	return err
}

// hasComment reports whether the issue holds, among the channel's own
// comments, one carrying id.
func (s *issuesSender) hasComment(ctx context.Context, number int, id string) (bool, error) {
	own, err := s.tracker.OwnComments(ctx, number)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(own, func(c string) bool { return strings.Contains(c, id) }), nil
}

func (s *issuesSender) send(a report.Alert, repeat int) error {
	if repeat == 0 {
		return s.open(a)
	}
	return s.addComment(a, deliveryMarker(a.Key, repeat), fmt.Sprintf("Still open (repeat %d).", repeat))
}

// remind adds a failed unit's reminder, the ordinal-th, to its open issue:
// a reminder has its own identity, never the first delivery's.
func (s *issuesSender) remind(a report.Alert, ordinal int) error {
	return s.addComment(a, reminderMarker(a.Key, ordinal), fmt.Sprintf("Still failing (reminder %d).", ordinal))
}

// addComment adds one comment carrying id to the alert's open issue, unless
// one of the channel's own comments carries it already. An issue closed by
// hand is left closed, and an issue gone from the listing is not opened
// again.
func (s *issuesSender) addComment(a report.Alert, id, heading string) error {
	ctx, cancel, err := s.begin()
	if err != nil {
		return err
	}
	defer cancel()
	issue, found, err := s.latest(ctx, issueMarker(a.Key))
	if err != nil {
		return s.note(err, true)
	}
	if !found || !issue.Open {
		return nil
	}
	done, err := s.hasComment(ctx, issue.Number, id)
	if err != nil || done {
		return s.note(err, false)
	}
	return s.note(s.tracker.Comment(ctx, issue.Number, fitIssueText(heading+"\n\n"+s.text(a), "", "\n\n"+id)), false)
}

// latest is the tracker's newest issue holding marker, and, the first time in
// a firing the listing is read, prunes the records of issues left open
// (unclosedKey) whose issue has left the listing, so a record stays only
// until its issue leaves it.
func (s *issuesSender) latest(ctx context.Context, marker string) (trackedIssue, bool, error) {
	issue, found, err := s.tracker.Latest(ctx, marker)
	if err != nil || s.pruned {
		return issue, found, err
	}
	listed, listErr := s.tracker.Listed(ctx)
	if listErr != nil {
		// The records are kept: a listing not read whole proves no issue gone.
		return issue, found, err
	}
	s.pruned = true
	prefix := channelKey(issuesChannel, "unclosed:")
	for k := range s.records {
		rest, ok := strings.CutPrefix(k, prefix)
		if !ok {
			continue
		}
		numText, _, _ := strings.Cut(rest, ":")
		if n, err := strconv.Atoi(numText); err == nil && !listed[n] {
			delete(s.records, k)
		}
	}
	return issue, found, nil
}

// unclosedKey is the record (issuesSender.records) that issue number n of an
// alert was left open when its close was given up. It stays until that issue
// leaves the listing, so the issue never suppresses a later incident, however
// the issues that replaced it end: the replacement chain is the local
// record's, never the listing's.
func unclosedKey(key string, n int) string {
	return channelKey(issuesChannel, fmt.Sprintf("unclosed:%d:%s", n, key))
}

// closeTriesKey is the record (closeForgotten's counts) of how many firings
// have tried and failed to close the issue of an alert the run forgot.
func closeTriesKey(key string) string { return channelKey(issuesChannel, "close-tries:"+key) }

// open is an alert's first delivery: a new issue, unless the newest issue of
// its key is this incident's, open, or closed by hand (no closing comment of
// the reporter's on it), made by a send whose answer was lost. Two older
// issues are replaced, with a note that names them: one closed with more
// comments than are read, taken as closed by overflow, since whether its
// closing comment is there cannot be told; and one whose close was given up
// (closeForgotten, unclosedKey), which is left open and is replaced however
// the issues that replaced it before have ended.
func (s *issuesSender) open(a report.Alert) error {
	ctx, cancel, err := s.begin()
	if err != nil {
		return err
	}
	defer cancel()
	marker := issueMarker(a.Key)
	issue, found, err := s.latest(ctx, marker)
	if err != nil {
		return s.note(err, true)
	}
	note := ""
	if found && s.records[unclosedKey(a.Key, issue.Number)] > 0 {
		// The newest issue is one left open when its close was given up: this
		// incident is new, whatever state that issue is in, and has its own.
		note = fmt.Sprintf("The earlier issue of this alert, #%d, was left open earlier; this new incident has its own issue.\n\n", issue.Number)
		if issue.Open {
			note = fmt.Sprintf("The earlier issue of this alert, #%d, was left open earlier; this new incident has its own issue; close #%d by hand.\n\n", issue.Number, issue.Number)
		}
	}
	if note == "" && found && issue.Open {
		return nil
	}
	if note == "" && found {
		resolved, err := s.hasComment(ctx, issue.Number, resolvedMarker(a.Key))
		switch {
		case errors.Is(err, errTooManyComments):
			note = fmt.Sprintf("The earlier issue of this alert, #%d, has more than %d comments, so it is taken as closed and this issue replaces it.\n\n", issue.Number, maxCommentPages*100)
		case err != nil || !resolved:
			return s.note(err, false)
		}
	}
	title := s.redactor.Text(a.Title)
	if r := []rune(title); len(r) > maxIssueTitle {
		title = string(r[:maxIssueTitle])
	}
	evidence := ""
	if s.evidence != nil {
		evidence = s.redactor.Text(s.evidence())
	}
	if _, err = s.tracker.Create(ctx, title, fitIssueText(note+s.text(a), evidence, "\n\n"+marker), []string{alertLabel}); err != nil {
		return s.note(err, false)
	}
	return nil
}

// resolve closes the issue of the alert keyed key, with one closing
// comment, and takes its alert label off, so a closed incident leaves the
// listing (load): a close whose comment was posted but whose state change
// or label removal failed only finishes when tried again. An issue the owner
// closed by hand is given the closing comment too, so the alert's next
// incident opens a new issue (send); an alert with no issue is resolved.
func (s *issuesSender) resolve(key string) error {
	ctx, cancel, err := s.begin()
	if err != nil {
		return err
	}
	defer cancel()
	issue, found, err := s.latest(ctx, issueMarker(key))
	if err != nil {
		return s.note(err, true)
	}
	if !found {
		return nil
	}
	if s.closing == nil {
		s.closing = map[string]int{}
	}
	s.closing[key] = issue.Number
	id := resolvedMarker(key)
	done, err := s.hasComment(ctx, issue.Number, id)
	overflow := errors.Is(err, errTooManyComments)
	if err != nil && !overflow {
		return s.note(err, false)
	}
	// From here the close is asked of GitHub: a comment, the close or the
	// label swap is written, and a refusal of any is a try.
	if s.asked == nil {
		s.asked = map[string]bool{}
	}
	s.asked[key] = true
	// An issue whose comments are too many to read closes with no closing
	// comment, so a close tried again posts nothing more: closed, it is
	// taken as closed by overflow (open), which needs no comment.
	if !done && !overflow {
		if err := s.tracker.Comment(ctx, issue.Number, "Resolved: this alert has cleared.\n\n"+id); err != nil {
			return s.note(err, false)
		}
	}
	if issue.Open {
		if err := s.tracker.CloseIssue(ctx, issue.Number); err != nil {
			return s.note(err, false)
		}
	}
	return s.note(s.tracker.MarkResolved(ctx, issue.Number), false)
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

// closeForgotten closes, on every issue tracker, the issue of each alert
// whose key begins with one of prefixes, the alerts the run has forgotten
// (forget), and of each whose close failed at an earlier firing, whether or
// not its fault is back, and then drops the tracker's record of it, so the
// same key arising again is a new incident: a new issue, which notifies (ADR
// 0019, "Alerting": once per incident). It runs after every alert is
// delivered, so a slow tracker never holds back a page. A close that
// GitHub refused is counted (closeTriesKey) and tried again at the next
// firing; one that was not tried (the tracker failed earlier in the firing or
// its time was spent) or found the tracker unavailable is tried again and
// not counted. After maxCloseTries the record is dropped all the same and
// the issue's number is recorded as unclosed (unclosedKey), so the next
// incident opens a fresh issue that names the one left open, and an
// unfinished close never suppresses it.
func closeForgotten(senders []sender, prefixes []string, seen map[string]bool, sent map[string]time.Time, repeats map[string]int) error {
	var failures []error
	for _, s := range senders {
		r, ok := s.(*issuesSender)
		if !ok {
			continue
		}
		due := map[string]bool{}
		for k := range seen {
			ch, key, _ := strings.Cut(k, "|")
			if ch == s.name() && slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(key, p) }) {
				due[key] = true
			}
		}
		for k := range repeats {
			if key, ok := strings.CutPrefix(k, closeTriesKey("")); ok {
				due[key] = true
			}
		}
		for _, key := range slices.Sorted(maps.Keys(due)) {
			drop := func() {
				for _, k := range []string{channelKey(s.name(), key), channelKey(s.name(), resolvedKey(key)), closeTriesKey(key)} {
					delete(seen, k)
					delete(sent, k)
					delete(repeats, k)
				}
			}
			delete(r.closing, key)
			delete(r.asked, key)
			if err := r.resolve(key); err != nil {
				// A close counts as a try only when a close request was sent
				// to GitHub and refused: not when it failed before any
				// request was sent (the listing, the comments or the
				// identity could not be read, the tracker failed earlier in
				// the firing, or the firing's time was spent) or was
				// unavailable (a rate limit, a server error, a timeout).
				number := r.closing[key]
				if !r.asked[key] || errors.Is(err, errUnavailable) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errFiringOver) {
					// The close stays due at the next firing, with no try spent.
					if _, ok := repeats[closeTriesKey(key)]; !ok {
						repeats[closeTriesKey(key)] = 0
					}
					failures = append(failures, fmt.Errorf("%s: resolve: %w", s.name(), err))
					continue
				}
				tries := repeats[closeTriesKey(key)] + 1
				if tries < maxCloseTries {
					repeats[closeTriesKey(key)] = tries
					failures = append(failures, fmt.Errorf("%s: resolve: %w", s.name(), err))
					continue
				}
				drop()
				repeats[unclosedKey(key, number)] = 1
				failures = append(failures, fmt.Errorf("%s: resolve: %w (given up after %d firings: the next incident opens a fresh issue)", s.name(), err, tries))
				continue
			}
			drop()
			if number, read := r.closing[key]; read {
				delete(repeats, unclosedKey(key, number))
			}
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
		tracker:  &githubTracker{api: strings.TrimRight(api, "/"), base: strings.TrimRight(api, "/") + "/repos/" + owner + "/" + name, token: token, client: client},
		redactor: report.NewRedactor(append([]string{token}, hostLiterals()...)),
		timeout:  client.Timeout,
	}, nil
}

// githubTracker is one repository's issues over GitHub's REST API
// (https://docs.github.com/en/rest/issues). The alert issues, open and
// closed, are listed once per firing, newest first, and kept, so a firing's
// lookups cost one listing, not one per alert. The channel's own identity,
// the token's user, is asked once (GET /user), so only its own comments are
// taken as its deliveries.
type githubTracker struct {
	api, base, token string
	client           *http.Client

	mu     sync.Mutex
	loaded bool
	latest map[string]trackedIssue // marker -> the newest issue holding it
	listed map[int]bool            // the number of every issue in the listing
	self   int64                   // the token's user id, once asked
	// selfErr is the failed lookup of the token's user, remembered for the
	// firing so it is asked once.
	selfErr error
}

// ghIssue is the part of an issue the channel reads.
type ghIssue struct {
	Number      int             `json:"number"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	Comments    int             `json:"comments"`
	PullRequest json.RawMessage `json:"pull_request"`
}

// errTooManyComments is an issue whose comments are too many to read whole.
var errTooManyComments = fmt.Errorf("issue has more than %d comments, so whether this delivery was made cannot be told and nothing is written", maxCommentPages*100)

// errNoAuthor is a comment GitHub names no author of.
var errNoAuthor = errors.New("GitHub named a comment without its author, so whether this delivery was made cannot be told and nothing is written")

// ghUser is the part of a user the channel reads.
type ghUser struct {
	ID int64 `json:"id"`
}

// errUnavailable marks a tracker failure that is remembered for the firing:
// a rate limit, a server error, or a request or an answer that failed on the
// way.
var errUnavailable = errors.New("GitHub is not asked again in this firing")

// statusError is GitHub's answer of a status that is neither a success nor
// a fault remembered for the firing.
type statusError struct {
	code   int
	status string
}

func (e *statusError) Error() string { return "GitHub answered " + e.status }

// requestBudget is the count of requests one delivery may still make.
type requestBudget struct{}

func withRequestBudget(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, requestBudget{}, &n)
}

func (g *githubTracker) call(ctx context.Context, method, path string, in, out any) (http.Header, error) {
	return g.callURL(ctx, method, g.base+path, in, out)
}

func (g *githubTracker) callURL(ctx context.Context, method, target string, in, out any) (http.Header, error) {
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
			return nil, fmt.Errorf("request failed: %w (%w)", urlErr.Err, errUnavailable)
		}
		return nil, fmt.Errorf("request failed (%w)", errUnavailable)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	if err != nil {
		// An answer that stops on the way is a timeout like any other.
		return nil, fmt.Errorf("GitHub's answer did not arrive whole (%w)", errUnavailable)
	}
	if resp.StatusCode/100 != 2 {
		// A limit GitHub applies, or a fault of its own, is the channel's
		// failure like any other: the alert is sent again at a later firing,
		// and the rest of this firing does not ask again.
		if resp.StatusCode == http.StatusTooManyRequests || resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" {
			return nil, fmt.Errorf("GitHub answered %s: rate limited, tried again at a later firing (%w)", resp.Status, errUnavailable)
		}
		if resp.StatusCode >= 500 {
			return nil, fmt.Errorf("GitHub answered %s (%w)", resp.Status, errUnavailable)
		}
		return nil, &statusError{code: resp.StatusCode, status: resp.Status}
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
	latest, listed := map[string]trackedIssue{}, map[int]bool{}
	for page := 1; ; page++ {
		if page > maxIssuePages {
			return fmt.Errorf("more than %d issues carry the %s label, so the list of alert issues is incomplete and none is written; remove the label from old closed issues whose incident is over (resolved, or acknowledged and no longer repeating)", maxIssuePages*100, alertLabel)
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
			// An issue the listing does not describe whole is an error, never
			// read as closed: a repeat taken for delivered would be lost.
			if i.Number <= 0 || (i.State != "open" && i.State != "closed") || i.Comments < 0 {
				return errors.New("GitHub's listing named an issue without its number or its state")
			}
			listed[i.Number] = true
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
	g.latest, g.listed, g.loaded = latest, listed, true
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

// Listed returns the numbers of the issues in the alert listing.
func (g *githubTracker) Listed(ctx context.Context) (map[int]bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.load(ctx); err != nil {
		return nil, err
	}
	return maps.Clone(g.listed), nil
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
		g.listed[out.Number] = true
	}
	return out.Number, nil
}

// OwnComments reads every page of the issue's comments, at most
// maxCommentPages, and returns those the token's user wrote.
func (g *githubTracker) OwnComments(ctx context.Context, number int) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.selfErr != nil {
		return nil, g.selfErr
	}
	if g.self == 0 {
		var me ghUser
		if _, err := g.callURL(ctx, http.MethodGet, g.api+"/user", nil, &me); err != nil {
			g.selfErr = err
			return nil, err
		}
		if me.ID <= 0 {
			g.selfErr = errors.New("GitHub did not name the token's user")
			return nil, g.selfErr
		}
		g.self = me.ID
	}
	var bodies []string
	for page := 1; ; page++ {
		if page > maxCommentPages {
			return nil, errTooManyComments
		}
		var comments []struct {
			Body string  `json:"body"`
			User *ghUser `json:"user"`
		}
		h, err := g.call(ctx, http.MethodGet, fmt.Sprintf("/issues/%d/comments?per_page=100&page=%d", number, page), nil, &comments)
		if err != nil {
			return nil, err
		}
		for _, c := range comments {
			// A comment of no author cannot be ruled out as the channel's
			// own, so the delivery fails closed.
			if c.User == nil || c.User.ID <= 0 {
				return nil, errNoAuthor
			}
			if c.User.ID == g.self {
				bodies = append(bodies, c.Body)
			}
		}
		if !nextPage(h) {
			return bodies, nil
		}
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

// MarkResolved gives the issue the resolved label, then takes the alert label
// off it (https://docs.github.com/en/rest/issues/labels), in that order so a
// failure between the two leaves the issue listed and the call tried again;
// GitHub's 404, the alert label not on the issue, is taken as done. The issue
// then leaves the listing.
func (g *githubTracker) MarkResolved(ctx context.Context, number int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.call(ctx, http.MethodPost, fmt.Sprintf("/issues/%d/labels", number), map[string][]string{"labels": {resolvedLabel}}, nil); err != nil {
		return err
	}
	if _, err := g.call(ctx, http.MethodDelete, fmt.Sprintf("/issues/%d/labels/%s", number, alertLabel), nil, nil); err != nil {
		var status *statusError
		if !errors.As(err, &status) || status.code != http.StatusNotFound {
			return err
		}
	}
	for m, i := range g.latest {
		if i.Number == number {
			delete(g.latest, m)
		}
	}
	return nil
}

// withIssueRecords gives the issues channel the state file's counts, where
// it keeps the alerts whose close was given up (issuesSender.records).
func withIssueRecords(senders []sender, repeats map[string]int) {
	for _, s := range senders {
		if is, ok := s.(*issuesSender); ok {
			is.records = repeats
		}
	}
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
// with every cell redacted.
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
			cells = append(cells, r.Text(row[i]))
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
