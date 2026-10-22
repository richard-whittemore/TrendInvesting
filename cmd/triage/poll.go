package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
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

// errNotRed is a tests' commit at which no new test failed by name: the
// change is not tests first, so no PR is opened.
var errNotRed = errors.New("the new test did not fail by name before the fix")

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
	// AlertIssues lists the issues carrying label, open and closed, newest
	// first, opened at or after since and numbered above through; cut is a
	// listing that reached its bound before either.
	AlertIssues(ctx context.Context, label string, since time.Time, through int) (issues []issue, cut bool, err error)
	// IssueURL is an alert issue's address on GitHub.
	IssueURL(number int) string
	// OwnComments is the bodies of the issue's comments the token's own user
	// wrote.
	OwnComments(ctx context.Context, number int) ([]string, error)
	Comment(ctx context.Context, number int, body string) error
	// OpenDraftPull opens a draft pull request from head into main of the
	// code repository and returns its address.
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
// each through the reporter's full redactor (report.Redactor.Text: no
// identifier and no figure), and the fresh worktree it may change.
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

// slicer fetches the journal slice of a window over the read-only access
// path.
type slicer interface {
	Fetch(ctx context.Context, from, to time.Time) (string, error)
}

// fixer is the repository workspace a fix is made in, and the gate it goes
// through before it is pushed (gitWorkspace).
type fixer interface {
	// GateAvailable reports whether the gate's user and sandbox can run.
	GateAvailable(ctx context.Context) error
	// Prepare makes a fresh worktree of main on a new local branch.
	Prepare(ctx context.Context, branch string) (dir string, err error)
	// Changed lists every file the session changed, added or removed.
	Changed(ctx context.Context, dir string) ([]string, error)
	// Gate commits the tests, then the fix, and fails unless a new test fails
	// by name at the tests' commit and the check passes at the fix's.
	Gate(ctx context.Context, dir string, tests, fix []string, title string) error
	// Added is the commits' messages, every path and every line the commits
	// add; a binary file is an error.
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
	// sliceWindow is how far before the alert the journal slice reaches.
	sliceWindow time.Duration
	// checkCmd is named in a PR's body as the gate ran it.
	checkCmd string
	log      io.Writer
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
	// Through is the highest issue number at and below which every alert
	// issue since Since is handled: a listing stops there, so a long history
	// never fills its bound.
	Through int `json:"through"`
	// Issues is each issue's record, by number.
	Issues map[string]issueRecord `json:"issues"`
}

// issueRecord is one issue's progress. Invoked is written before the model is
// called, so a run that dies during the call never calls it again; Pending is
// the comment, already redacted, kept until GitHub takes it.
// Interrupted is a session the job's own stop ended once: the issue is
// analysed again, once.
type issueRecord struct {
	Invoked     bool   `json:"invoked,omitempty"`
	Interrupted bool   `json:"interrupted,omitempty"`
	Pending     string `json:"pending,omitempty"`
	Done        bool   `json:"done,omitempty"`
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
	if st.Since.IsZero() {
		st.Since = j.now()
	}
	j.today(st)
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
	candidates, listed, cut, err := j.candidates(ctx, st, author)
	if err != nil {
		return err
	}
	if cut {
		_, _ = fmt.Fprintf(j.log, "triage: more than one listing's bound of alert issues are new since issue #%d; the newest are analysed, the older ones not reached: read those yourself\n", st.Through)
	}
	var failures []error
	for _, is := range candidates {
		if err := j.triage(ctx, st, is); err != nil {
			failures = append(failures, fmt.Errorf("triage: issue #%d: %w", is.Number, err))
		}
	}
	if !cut {
		j.advance(st, listed)
		if err := saveState(j.statePath, st); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// today starts the day's count afresh when the New York day has changed.
func (j *job) today(st *state) {
	if day := j.now().In(j.location).Format(time.DateOnly); st.Day != day {
		st.Day, st.Calls = day, 0
	}
}

// advance moves Through up over the listed issues, lowest first, while each
// is handled: done, or never the reporter's (handled by being left alone).
// An issue still owed its comment stops it, so the next listing reaches it.
func (j *job) advance(st *state, listed map[int]bool) {
	numbers := make([]int, 0, len(listed))
	for n := range listed {
		numbers = append(numbers, n)
	}
	slices.Sort(numbers)
	for _, n := range numbers {
		if listed[n] && !st.Issues[strconv.Itoa(n)].Done {
			return
		}
		st.Through = n
	}
}

// candidates are the reporter's alert issues, under either label, opened
// since the job began, numbered above Through and not yet recorded done,
// oldest first; listed is every issue the listings returned, true for the
// reporter's (false: one it leaves alone), and cut whether a listing was
// cut at its bound. An issue the reporter did not open (another author, or
// no hidden key line) is never read further: its words are not the
// reporter's.
func (j *job) candidates(ctx context.Context, st *state, author string) (out []issue, listed map[int]bool, cut bool, err error) {
	listed = map[int]bool{}
	for _, label := range []string{alertLabel, resolvedLabel} {
		list, c, err := j.tracker.AlertIssues(ctx, label, st.Since, st.Through)
		if err != nil {
			return nil, nil, false, fmt.Errorf("triage: list the %s issues: %w", label, err)
		}
		cut = cut || c
		for _, is := range list {
			if _, seen := listed[is.Number]; seen || is.Created.Before(st.Since) || is.Number <= st.Through {
				continue
			}
			ours := strings.EqualFold(is.Author, author) && strings.Contains(is.Body, alertKeyLine)
			listed[is.Number] = ours
			if !ours || st.Issues[strconv.Itoa(is.Number)].Done {
				continue
			}
			out = append(out, is)
		}
	}
	slices.SortFunc(out, func(a, b issue) int { return a.Number - b.Number })
	return out, listed, cut, nil
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
	// The cap's day is the day of this call, not of the poll's start.
	j.today(st)
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
		comment, err := j.analyse(ctx, is)
		if errors.Is(err, errInterrupted) {
			// The job was stopped during the session: the issue is analysed
			// again at the next poll, once; stopped twice, it is spent.
			if !rec.Interrupted {
				rec = issueRecord{Interrupted: true}
			}
			st.Issues[key] = rec
			if serr := saveState(j.statePath, st); serr != nil {
				return errors.Join(err, serr)
			}
			return err
		}
		rec.Pending = comment
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
// becomes the comment, never a second call; only the job's own stop during
// it is an error (errInterrupted), so the issue is not spent on a restart.
func (j *job) analyse(ctx context.Context, is issue) (string, error) {
	if err := j.fixer.GateAvailable(ctx); err != nil {
		return failedComment("gate unavailable: " + j.clean(err.Error()) + "; no session was started"), nil
	}
	slice, covered := j.slice(ctx, is)
	branch := fmt.Sprintf("triage/alert-%d", is.Number)
	dir, err := j.fixer.Prepare(ctx, branch)
	if err != nil {
		return failedComment("the repository could not be prepared: " + j.clean(err.Error())), nil
	}
	defer j.fixer.Remove(context.WithoutCancel(ctx), dir)
	is.Title, is.Body = j.redactor.Text(is.Title), j.redactor.Text(is.Body)
	a, err := j.analyst.Analyse(ctx, analysisRequest{Issue: is, Slice: slice, Dir: dir})
	if errors.Is(err, errInterrupted) {
		return "", err
	}
	if err != nil {
		return failedComment("the analysis failed: " + j.clean(err.Error())), nil
	}
	if _, known := kindLabels[a.Kind]; !known {
		return failedComment("the analysis named no known kind of finding, so none of it is posted"), nil
	}
	var b strings.Builder
	b.WriteString("### Triage\n\n")
	fmt.Fprintf(&b, "**What happened.** %s\n\n", j.clean(a.WhatHappened))
	fmt.Fprintf(&b, "**Which rule applied.** %s\n\n", j.clean(a.Rule))
	fmt.Fprintf(&b, "**Kind.** %s\n\n", kindLabels[a.Kind])
	if !covered {
		b.WriteString("**Journal slice.** " + slice + "\n\n")
	}
	switch a.Kind {
	case kindOwner:
		fmt.Fprintf(&b, "**What you must do.** %s The triage job has not done it and never will: it holds no way to.\n\n", j.clean(a.OwnerAction))
	case kindDefect:
		b.WriteString(j.openFix(ctx, is, dir, branch, a) + "\n\n")
	}
	if ctx.Err() != nil {
		// Stopped during the gate: what it says would not be the gate's.
		return "", errInterrupted
	}
	b.WriteString("_Analysed by the triage job on the Mac mini. It reads the alert and the journal slice; it never acts for you._\n\n" + analysisMarker)
	return b.String(), nil
}

// sliceCovers is a section's line of the span its run's records cover.
var sliceCovers = regexp.MustCompile(`(?m)^## [^\n]*: covers (\S+) to (\S+)$`)

// slice is the journal slice of the window before the alert, through the
// reporter's full redactor, and whether it covers the alert: a run whose
// records began before the issue opened and reach into the window. A slice
// that does not cover it is not handed over as if it did: it is replaced by
// a sentence saying so, which the comment repeats.
func (j *job) slice(ctx context.Context, is issue) (string, bool) {
	to := j.now()
	if to.Before(is.Created) {
		to = is.Created
	}
	raw, err := j.slicer.Fetch(ctx, is.Created.Add(-j.sliceWindow), to)
	if err != nil {
		return "The journal slice could not be fetched (" + j.clean(err.Error()) + "), so the analysis rests on the alert alone.", false
	}
	from := is.Created.Add(-j.sliceWindow)
	for _, m := range sliceCovers.FindAllStringSubmatch(raw, -1) {
		first, err1 := time.Parse(time.RFC3339, m[1])
		last, err2 := time.Parse(time.RFC3339, m[2])
		if err1 == nil && err2 == nil && !first.After(is.Created) && !last.Before(from) {
			return j.redactor.Text(raw), true
		}
	}
	return "The journal slice did not cover the alert's run and time (no run's records span the alert), so the analysis rests on the alert alone.", false
}

// openFix takes a code defect's change through the gate and opens it as a
// draft PR, and says what it did. The PR's link is the comment's only text
// not redacted: the job builds it from GitHub's own answer.
func (j *job) openFix(ctx context.Context, is issue, dir, branch string, a analysis) string {
	const none = "**Fix.** No fix PR was opened: "
	changed, err := j.fixer.Changed(ctx, dir)
	if err != nil {
		return none + "the session's changes could not be read (" + j.clean(err.Error()) + ")."
	}
	var tests, fix []string
	for _, f := range changed {
		switch {
		case isTestFile(f):
			tests = append(tests, f)
		case isFixFile(f):
			fix = append(fix, f)
		default:
			return none + "the session changed a file outside the Go code and docs/ (only *.go files under " + strings.Join(codeFolders, ", ") + " and files under docs/ are opened)."
		}
	}
	if err := textOnly(dir, changed); err != nil {
		return none + err.Error() + "."
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
		switch {
		case errors.Is(err, errNotRed):
			return none + "the new test did not fail by name before the fix (a test that passed, did not compile or ran out of time is not red), so the change is not tests first."
		case errors.Is(err, errNoNewTest):
			return none + "the change adds no new test function, so no test can show the defect by name."
		}
		return none + "the change did not pass the gate: " + j.clean(err.Error())
	}
	added, err := j.fixer.Added(ctx, dir)
	if errors.Is(err, errBinary) {
		return none + "the change holds a binary file, which cannot be checked, so it is not pushed."
	}
	if err != nil {
		return none + "the change could not be read back."
	}
	if j.redactor.Identifiers(added) != added {
		return none + "the change holds an account id, a token, a secret or a host name, so it is not pushed."
	}
	if err := j.fixer.Push(ctx, dir, branch); err != nil {
		return none + "the branch could not be pushed: " + j.clean(err.Error())
	}
	// The alert issue may be in a private repository: it is linked by its
	// address, and none of its text is quoted; only Claude's words, redacted.
	body := "A fix the triage job proposes for the alert " + j.tracker.IssueURL(is.Number) + ".\n\n" +
		"**What happened.** " + j.clean(a.WhatHappened) + "\n\n" +
		"**Which rule applied.** " + j.clean(a.Rule) + "\n\n" +
		"**The fix.** " + j.clean(a.FixSummary) + "\n\n" +
		"**Tests first.** The first commit holds only the tests, and `go test -run` over its new tests showed one failing by name; the second holds the fix, and `" + j.checkCmd + "` passed on it. Both ran in the gate's sandbox, as its own user, with no network.\n\n" +
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

// codeFolders are the repository's Go code folders: the only places a fix
// and its tests may change.
var codeFolders = []string{"cmd/", "internal/", "transport/"}

// inCode reports whether name is under one of the code folders.
func inCode(name string) bool {
	return slices.ContainsFunc(codeFolders, func(f string) bool { return strings.HasPrefix(name, f) })
}

// isTestFile reports whether a changed file is a test the gate can run: a
// Go test file, or a fixture under a testdata folder, in the code folders.
func isTestFile(name string) bool {
	return inCode(name) && (strings.HasSuffix(path.Base(name), "_test.go") || strings.Contains(name, "/testdata/"))
}

// isFixFile reports whether a changed file may be a fix: a Go file in the
// code folders, or a file under docs/. Nothing that defines the check (the
// Makefile, scripts), CI, Claude's settings, the deployment or the module's
// requirements is ever opened as a fix.
func isFixFile(name string) bool {
	return inCode(name) && strings.HasSuffix(name, ".go") || strings.HasPrefix(name, "docs/")
}

// maxChangedFile bounds a changed file the job reads to check it is text.
const maxChangedFile = 4 << 20

// textOnly refuses a change holding a file that is not plain text: a
// binary, a link, or one too large to read. A path that is gone was
// removed.
func textOnly(dir string, changed []string) error {
	for _, name := range changed {
		info, err := os.Lstat(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxChangedFile {
			return errors.New("the session left a binary, a link or an oversize file, which is never opened")
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
			return errors.New("the session left a binary file, which is never opened")
		}
	}
	return nil
}

// errBadState is a state file that cannot be trusted.
var errBadState = errors.New("triage: the state file is not understood, so nothing is done")

// loadState reads the job's record; a missing file is a first poll. A file
// that cannot be read, does not parse, or is incomplete or breaks an
// invariant stops the poll with errBadState: without it the job cannot tell
// which issues it has already called the model for.
func loadState(p string) (*state, error) {
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &state{Issues: map[string]issueRecord{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("triage: the state file cannot be read, so nothing is done: %w", err)
	}
	var st *state
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("%w (%s); move it aside only once you know which issues were analysed (each has the job's comment)", errBadState, p)
	}
	if why := invalid(st); why != "" {
		return nil, fmt.Errorf("%w (%s: %s); move it aside only once you know which issues were analysed (each has the job's comment)", errBadState, p, why)
	}
	return st, nil
}

// invalid says what a parsed record lacks or breaks, or "" when it is whole:
// the job's start, the day of the count, a count of no less than zero, the
// issues' records, each under an issue's number, and Through no less than
// zero.
func invalid(st *state) string {
	switch {
	case st == nil:
		return "it holds no record"
	case st.Since.IsZero():
		return "it names no start"
	case st.Calls < 0:
		return "its count of calls is below zero"
	case st.Through < 0:
		return "its handled-through number is below zero"
	case st.Issues == nil:
		return "it holds no issues' records"
	}
	if _, err := time.Parse(time.DateOnly, st.Day); err != nil {
		return "its day is not a date"
	}
	for k := range st.Issues {
		if n, err := strconv.Atoi(k); err != nil || n <= 0 || strconv.Itoa(n) != k {
			return "an issue's record is not under its number"
		}
	}
	return ""
}

// syncFile makes a file's contents durable (tests record it).
var syncFile = func(f *os.File) error { return f.Sync() }

// saveState writes the record whole, through a temporary file synced to disk
// and a rename, then syncs the folder, so a run that dies mid-write, or a
// power loss after it, leaves either the earlier record or this one, and a
// returned save is durable: an invocation is on disk before its model call.
func saveState(p string, st *state) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("triage: write state: %w", err)
	}
	_, err = f.Write(raw)
	if err == nil {
		err = syncFile(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("triage: write state: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("triage: write state: %w", err)
	}
	d, err := os.Open(filepath.Dir(p))
	if err != nil {
		return fmt.Errorf("triage: sync the state's folder: %w", err)
	}
	err = syncFile(d)
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("triage: sync the state's folder: %w", err)
	}
	return nil
}
