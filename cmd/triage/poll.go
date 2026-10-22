package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// The triage job analyses each alert issue once (docs/manual/alerts-and-dashboard.md,
// "Alerts analysed by Claude"). It is fail closed throughout: a poll spends
// no model usage unless an alert issue has no triage comment yet; each issue
// gets at most one model call, recorded before the call is made; the day's
// calls are capped; Claude's words reach GitHub only through the reporter's
// redactor (report.Redactor); and an owner action is only ever stated.

const (
	// alertLabel and resolvedLabel are the reporter's labels: an open
	// incident's issue, and one the reporter resolved, which loses alertLabel,
	// so a job that only polled alertLabel would miss an incident that cleared
	// between two polls (docs/manual/alerts-and-dashboard.md, "Design note: a
	// resolved issue leaves the alert label").
	alertLabel    = "alert"
	resolvedLabel = "alert-resolved"
	// alertKeyLine begins the hidden line the reporter ends every alert
	// issue's body with; an issue without it is not the reporter's.
	alertKeyLine = "<!-- ti-alert-key:"
	// triageMarkerPrefix begins the hidden line of each of the job's own
	// comments; one, written by the token's own user, marks the issue done.
	triageMarkerPrefix = "<!-- ti-triage:"
	analysisMarker     = triageMarkerPrefix + "analysis -->"
	skippedMarker      = triageMarkerPrefix + "skipped -->"
	// The three kinds of finding the analysis may name.
	kindDefect = "code-defect"
	kindData   = "data-problem"
	kindOwner  = "owner-action"
	// maxField bounds each of Claude's answers in a comment, so a comment
	// stays well within GitHub's 65,536 characters.
	maxField = 8000
	// maxTitle bounds a PR title.
	maxTitle = 120
)

// kindLabels names each kind in a comment.
var kindLabels = map[string]string{
	kindDefect: "Code defect",
	kindData:   "Data problem",
	kindOwner:  "Owner action",
}

// errNotRed is a test that passed before the fix: the change is not tests
// first, so no PR is opened.
var errNotRed = errors.New("the new test did not fail before the fix")

// issue is an alert issue as the job reads it.
type issue struct {
	Number  int
	Title   string
	Body    string
	Author  string // the login of the user who opened it
	Created time.Time
}

// tracker is the repository's issues and pulls (GitHub's REST API; tests use
// an in-memory one).
type tracker interface {
	// Self is the login of the token's own user.
	Self(ctx context.Context) (string, error)
	// AlertIssues lists the issues carrying label, open and closed, opened
	// at or after since.
	AlertIssues(ctx context.Context, label string, since time.Time) ([]issue, error)
	// OwnComments is the bodies of the issue's comments the token's own user
	// wrote.
	OwnComments(ctx context.Context, number int) ([]string, error)
	Comment(ctx context.Context, number int, body string) error
	// OpenDraftPull opens a draft pull request from head into main and
	// returns its address.
	OpenDraftPull(ctx context.Context, head, title, body string) (string, error)
}

// analysis is Claude's finding on one alert.
type analysis struct {
	Kind         string `json:"kind"`
	WhatHappened string `json:"what_happened"`
	Rule         string `json:"rule"`
	OwnerAction  string `json:"owner_action"`
	FixTitle     string `json:"fix_title"`
	FixSummary   string `json:"fix_summary"`
}

// analysisRequest is what Claude is handed: the issue and the journal slice,
// each with every identifier taken out (report.Redactor.Identifiers), and
// the fresh worktree it may change.
type analysisRequest struct {
	Issue issue
	Slice string
	Dir   string
}

// analyst is the Claude Code session (claudeCLI); tests use a fake, so no
// test makes a model call.
type analyst interface {
	Analyse(ctx context.Context, req analysisRequest) (analysis, error)
}

// slicer fetches the journal slice over the read-only access path.
type slicer interface {
	Fetch(ctx context.Context) (string, error)
}

// fixer is the repository workspace a fix is made in, and the gate it goes
// through before it is pushed (gitWorkspace).
type fixer interface {
	// Prepare makes a fresh worktree of main on a new local branch.
	Prepare(ctx context.Context, branch string) (dir string, err error)
	// Changed lists every file the session changed, added or removed.
	Changed(ctx context.Context, dir string) ([]string, error)
	// Gate commits the tests, then the fix, and fails unless the test command
	// fails at the tests' commit and the check passes at the fix's.
	Gate(ctx context.Context, dir string, tests, fix []string, title string) error
	// Added is the commits' messages and every line the commits add.
	Added(ctx context.Context, dir string) (string, error)
	// Push pushes the local branch, never forced, never to main.
	Push(ctx context.Context, dir, branch string) error
	// Remove removes the worktree and its local branch.
	Remove(ctx context.Context, dir string)
}

// job is one poll's wiring.
type job struct {
	tracker  tracker
	analyst  analyst
	slicer   slicer
	fixer    fixer
	redactor report.Redactor
	// statePath is the job's record of the day's calls and of each issue
	// invoked; offFile, when it exists, stops the job.
	statePath, offFile string
	dailyCap           int
	// author is the login whose issues are the reporter's; "" is the token's
	// own user.
	author string
	// location is where the day of the cap begins (New York).
	location *time.Location
	now      func() time.Time
	// testCmd and checkCmd are named in a PR's body as the gate ran them.
	testCmd, checkCmd string
	log               io.Writer
}

// state is the job's record, kept between polls.
type state struct {
	// Since is when the job first polled: an issue opened before it is not
	// analysed, so switching the job on never sweeps the backlog.
	Since time.Time `json:"since"`
	// Day and Calls are the day (in job.location) and the model calls made
	// in it.
	Day   string `json:"day"`
	Calls int    `json:"calls"`
	// Issues is each issue's record, by number.
	Issues map[string]issueRecord `json:"issues"`
}

// issueRecord is one issue's progress. Invoked is written before the model is
// called, so a run that dies during the call never calls it again; Pending is
// the comment, already redacted, kept until GitHub takes it.
type issueRecord struct {
	Invoked bool   `json:"invoked,omitempty"`
	Pending string `json:"pending,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// poll is one run of the job: every alert issue with no triage comment yet
// gets one. An issue's failure is reported and the others are still tried.
func (j *job) poll(ctx context.Context) error {
	if _, err := os.Stat(j.offFile); err == nil {
		_, _ = fmt.Fprintf(j.log, "triage: off (%s exists); nothing read, nothing written\n", j.offFile)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("triage: cannot tell whether the off file is there, so nothing is done: %w", err)
	}
	st, err := loadState(j.statePath)
	if err != nil {
		return err
	}
	now := j.now()
	if st.Since.IsZero() {
		st.Since = now
	}
	if day := now.In(j.location).Format("2006-01-02"); st.Day != day {
		st.Day, st.Calls = day, 0
	}
	if err := saveState(j.statePath, st); err != nil {
		return err
	}
	self, err := j.tracker.Self(ctx)
	if err != nil {
		return fmt.Errorf("triage: GitHub did not name the token's user: %w", err)
	}
	author := j.author
	if author == "" {
		author = self
	}
	candidates, err := j.candidates(ctx, st, author)
	if err != nil {
		return err
	}
	var failures []error
	for _, is := range candidates {
		if err := j.triage(ctx, st, is); err != nil {
			failures = append(failures, fmt.Errorf("triage: issue #%d: %w", is.Number, err))
		}
	}
	return errors.Join(failures...)
}

// candidates are the reporter's alert issues, under either label, opened
// since the job began and not yet recorded done, oldest first. An issue the
// reporter did not open (another author, or no hidden key line) is never
// read further: its words are not the reporter's.
func (j *job) candidates(ctx context.Context, st *state, author string) ([]issue, error) {
	seen := map[int]bool{}
	var out []issue
	for _, label := range []string{alertLabel, resolvedLabel} {
		list, err := j.tracker.AlertIssues(ctx, label, st.Since)
		if err != nil {
			return nil, fmt.Errorf("triage: list the %s issues: %w", label, err)
		}
		for _, is := range list {
			if seen[is.Number] || is.Created.Before(st.Since) || st.Issues[strconv.Itoa(is.Number)].Done {
				continue
			}
			seen[is.Number] = true
			if !strings.EqualFold(is.Author, author) || !strings.Contains(is.Body, alertKeyLine) {
				continue
			}
			out = append(out, is)
		}
	}
	slices.SortFunc(out, func(a, b issue) int { return a.Number - b.Number })
	return out, nil
}

// triage brings one issue to its one comment: done if the token's user has
// commented already; the kept comment posted if there is one; a model call,
// counted and recorded first, if the day's cap allows; otherwise the
// skipped comment.
func (j *job) triage(ctx context.Context, st *state, is issue) error {
	key := strconv.Itoa(is.Number)
	own, err := j.tracker.OwnComments(ctx, is.Number)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(own, func(c string) bool { return strings.Contains(c, triageMarkerPrefix) }) {
		st.Issues[key] = issueRecord{Done: true}
		return saveState(j.statePath, st)
	}
	rec := st.Issues[key]
	switch {
	case rec.Pending != "":
	case rec.Invoked:
		// The call was made and its answer lost (the run died during it):
		// it is not made again.
		rec.Pending = failedComment("the analysis did not finish, and is not tried again")
	case st.Calls >= j.dailyCap:
		rec.Pending = "triage skipped: daily limit. The day's limit of analyses is reached, so this alert is not analysed; read it and its journal yourself.\n\n" + skippedMarker
	default:
		st.Calls++
		rec.Invoked = true
		st.Issues[key] = rec
		if err := saveState(j.statePath, st); err != nil {
			return fmt.Errorf("the call could not be recorded, so it is not made: %w", err)
		}
		rec.Pending = j.analyse(ctx, is)
	}
	st.Issues[key] = rec
	if err := saveState(j.statePath, st); err != nil {
		return err
	}
	if err := j.tracker.Comment(ctx, is.Number, rec.Pending); err != nil {
		return err
	}
	st.Issues[key] = issueRecord{Done: true}
	return saveState(j.statePath, st)
}

// analyse is the one model call for an issue, and its comment. Every failure
// becomes the comment, never a second call.
func (j *job) analyse(ctx context.Context, is issue) string {
	slice, err := j.slicer.Fetch(ctx)
	if err != nil {
		slice = "The journal slice could not be fetched: " + err.Error()
	}
	branch := fmt.Sprintf("triage/alert-%d", is.Number)
	dir, err := j.fixer.Prepare(ctx, branch)
	if err != nil {
		return failedComment("the repository could not be prepared: " + j.clean(err.Error()))
	}
	defer j.fixer.Remove(ctx, dir)
	is.Title, is.Body = j.redactor.Identifiers(is.Title), j.redactor.Identifiers(is.Body)
	a, err := j.analyst.Analyse(ctx, analysisRequest{Issue: is, Slice: j.redactor.Identifiers(slice), Dir: dir})
	if err != nil {
		return failedComment("the analysis failed: " + j.clean(err.Error()))
	}
	if _, known := kindLabels[a.Kind]; !known {
		return failedComment("the analysis named no known kind of finding, so none of it is posted")
	}
	var b strings.Builder
	b.WriteString("### Triage\n\n")
	fmt.Fprintf(&b, "**What happened.** %s\n\n", j.clean(a.WhatHappened))
	fmt.Fprintf(&b, "**Which rule applied.** %s\n\n", j.clean(a.Rule))
	fmt.Fprintf(&b, "**Kind.** %s\n\n", kindLabels[a.Kind])
	switch a.Kind {
	case kindOwner:
		fmt.Fprintf(&b, "**What you must do.** %s The triage job has not done it and never will: it holds no way to.\n\n", j.clean(a.OwnerAction))
	case kindDefect:
		b.WriteString(j.openFix(ctx, is, dir, branch, a) + "\n\n")
	}
	b.WriteString("_Analysed by the triage job on the Mac mini. It reads the alert and the journal slice; it never acts for you._\n\n" + analysisMarker)
	return b.String()
}

// openFix takes a code defect's change through the gate and opens it as a
// draft PR, and says what it did. The PR's link is the comment's only text
// not redacted: the job builds it from GitHub's own answer.
func (j *job) openFix(ctx context.Context, is issue, dir, branch string, a analysis) string {
	const none = "**Fix.** No fix PR was opened: "
	changed, err := j.fixer.Changed(ctx, dir)
	if err != nil {
		return none + "the session's changes could not be read."
	}
	var tests, fix []string
	for _, f := range changed {
		if isTestFile(f) {
			tests = append(tests, f)
		} else {
			fix = append(fix, f)
		}
	}
	switch {
	case len(tests) == 0:
		return none + "the session changed no test, and a fix is opened only with its failing test first."
	case len(fix) == 0:
		return none + "the session changed only tests."
	}
	title := j.clean(bounded(a.FixTitle, maxTitle))
	if strings.TrimSpace(title) == "" {
		title = "a fix for an alert"
	}
	if err := j.fixer.Gate(ctx, dir, tests, fix, title); err != nil {
		if errors.Is(err, errNotRed) {
			return none + "the new test did not fail before the fix, so the change is not tests first."
		}
		return none + "the change did not pass the gate: " + j.clean(err.Error())
	}
	added, err := j.fixer.Added(ctx, dir)
	if err != nil {
		return none + "the change could not be read back."
	}
	if j.redactor.Identifiers(added) != added {
		return none + "the change holds an account id, a token, a secret or a host name, so it is not pushed."
	}
	if err := j.fixer.Push(ctx, dir, branch); err != nil {
		return none + "the branch could not be pushed: " + j.clean(err.Error())
	}
	body := fmt.Sprintf("A fix the triage job proposes for the alert issue #%d.\n\n", is.Number) +
		"**What happened.** " + j.clean(a.WhatHappened) + "\n\n" +
		"**Which rule applied.** " + j.clean(a.Rule) + "\n\n" +
		"**The fix.** " + j.clean(a.FixSummary) + "\n\n" +
		"**Tests first.** The first commit holds only the tests, and `" + j.testCmd + "` failed on it; the second holds the fix, and `" + j.checkCmd + "` passed on it.\n\n" +
		"Opened as a draft by the triage job: it is never merged by the job, which never marks a PR ready either. Review it as any other.\n\n" +
		"🤖 Generated with [Claude Code](https://claude.com/claude-code)"
	link, err := j.tracker.OpenDraftPull(ctx, branch, "triage: "+title, body)
	if err != nil {
		return none + "GitHub refused the PR (" + j.clean(err.Error()) + "); the branch is pushed."
	}
	return "**Fix.** A draft fix PR is open: " + link
}

// mention is an @-mention, which would ask a person or a review bot for
// something; the job asks no one.
var mention = regexp.MustCompile(`@([A-Za-z0-9])`)

// clean is text for GitHub: bounded, through the reporter's redactor, with
// no mention left.
func (j *job) clean(s string) string {
	return mention.ReplaceAllString(j.redactor.Text(bounded(s, maxField)), "$1")
}

// failedComment is the issue's one comment when no analysis can be posted.
func failedComment(why string) string {
	return "triage failed: " + why + ". Read the alert and its journal yourself.\n\n" + analysisMarker
}

// bounded is s cut to at most n bytes at a whole character.
func bounded(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + " [cut]"
}

// isTestFile reports whether a changed file is a test: a Go or Python test,
// a shell test or a test fixture, as the repository names them.
func isTestFile(name string) bool {
	base := path.Base(name)
	return strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, "_test.py") ||
		(strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py")) ||
		(strings.HasPrefix(base, "test-") && strings.HasSuffix(base, ".sh")) ||
		strings.HasPrefix(name, "testdata/") || strings.Contains(name, "/testdata/")
}

// loadState reads the job's record; a missing file is a first poll. A file
// that cannot be read or understood stops the poll: without it the job
// cannot tell which issues it has already called the model for.
func loadState(p string) (*state, error) {
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &state{Issues: map[string]issueRecord{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("triage: the state file cannot be read, so nothing is done: %w", err)
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("triage: the state file %s is not understood, so nothing is done; move it aside only once you know which issues were analysed (each has the job's comment)", p)
	}
	if st.Issues == nil {
		st.Issues = map[string]issueRecord{}
	}
	return &st, nil
}

// saveState writes the record whole, through a temporary file and a rename,
// so a run that dies mid-write leaves the earlier record.
func saveState(p string, st *state) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("triage: write state: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("triage: write state: %w", err)
	}
	return nil
}
