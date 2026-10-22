package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// alertKey is the hash of an alert's key, as the reporter writes it in the
// issue's hidden key line; alertBody is an alert issue's body as the
// reporter writes it: its text, then that line.
const (
	alertKey  = "5f2b0c1d9e8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3b2c"
	alertBody = "The risk gate refused one order.\n\n<!-- ti-alert-key:" + alertKey + " -->"
)

var pollTime = time.Date(2026, time.October, 22, 15, 0, 0, 0, time.UTC)

// alertLabel and resolvedLabel are the reporter's labels: an open
// incident's issue, and one the reporter resolved.
const (
	alertLabel    = "alert"
	resolvedLabel = "alert-resolved"
)

// fakeTracker is GitHub's issues and pulls held in memory.
type fakeTracker struct {
	self     string
	issues   map[string][]issue // by label
	comments map[int][]comment
	pulls    []fakePull
	// failComments fails every comment write.
	failComments error
	// bound is the most issues one listing returns before it is cut; 0 is
	// no bound. listed counts the issues every listing returned.
	bound, listed int
	// afterListing, when set, runs after each listing with its ordinal, as
	// GitHub changing while the job reads it.
	afterListing func(n int)
	listings     int
}

type comment struct{ author, body string }

type fakePull struct{ head, title, body string }

func (f *fakeTracker) Self(context.Context) (string, error) { return f.self, nil }

func (f *fakeTracker) AlertIssues(_ context.Context, since time.Time, through int) ([]issue, bool, error) {
	f.listings++
	if f.afterListing != nil {
		defer f.afterListing(f.listings)
	}
	// One listing of every issue, whatever its label, newest first, as
	// GitHub answers it.
	var all []issue
	for _, list := range f.issues {
		for _, is := range list {
			if !slices.ContainsFunc(all, func(o issue) bool { return o.Number == is.Number }) {
				all = append(all, is)
			}
		}
	}
	slices.SortFunc(all, func(a, b issue) int { return b.Created.Compare(a.Created) })
	var out []issue
	for _, is := range all {
		if is.Created.Before(since) || is.Number <= through {
			break
		}
		if f.bound > 0 && len(out) == f.bound {
			f.listed += len(out)
			return out, true, nil
		}
		out = append(out, is)
	}
	f.listed += len(out)
	return out, false, nil
}

func (f *fakeTracker) IssueURL(number int) string {
	return fmt.Sprintf("https://github.com/owner/alerts/issues/%d", number)
}

func (f *fakeTracker) OwnComments(_ context.Context, number int) ([]string, error) {
	var own []string
	for _, c := range f.comments[number] {
		if c.author == f.self {
			own = append(own, c.body)
		}
	}
	return own, nil
}

func (f *fakeTracker) Comment(_ context.Context, number int, body string) error {
	if f.failComments != nil {
		return f.failComments
	}
	if f.comments == nil {
		f.comments = map[int][]comment{}
	}
	f.comments[number] = append(f.comments[number], comment{author: f.self, body: body})
	return nil
}

func (f *fakeTracker) OpenDraftPull(_ context.Context, head, title, body string) (string, error) {
	f.pulls = append(f.pulls, fakePull{head: head, title: title, body: body})
	return fmt.Sprintf("https://github.com/owner/repo/pull/%d", 600+len(f.pulls)), nil
}

// ownComments is every comment the token's user wrote on the issue.
func (f *fakeTracker) ownComments(number int) []string {
	own, _ := f.OwnComments(context.Background(), number)
	return own
}

// fakeAnalyst is Claude: it returns one analysis and counts its calls.
type fakeAnalyst struct {
	result   analysis
	err      error
	requests []analysisRequest
}

func (f *fakeAnalyst) Analyse(_ context.Context, req analysisRequest) (analysis, error) {
	f.requests = append(f.requests, req)
	return f.result, f.err
}

type fakeSlicer struct {
	slice    string
	calls    int
	from, to time.Time
	alert    string
}

func (f *fakeSlicer) Fetch(_ context.Context, from, to time.Time, alert string) (string, error) {
	f.calls++
	f.from, f.to, f.alert = from, to, alert
	return f.slice, nil
}

// covering is a slice whose run, the one that raised the alert, covers the
// alerts of newWorld.
const covering = "## boost.log: covers 2026-10-22T14:00:00Z to 2026-10-22T15:30:00Z, and raised the alert\n\n## boost.log: journal-query refusals\n\nrefused order-size"

// fakeFixer is the repository workspace: the files the session changed and
// what each step did.
type fakeFixer struct {
	changed  []string
	gateErr  error
	added    string
	gated    [][]string
	pushed   []string
	removed  int
	prepared []string
	// dir, when set, is the worktree Prepare returns.
	dir string
	// unavailable is the gate's answer to GateAvailable; prepareErr
	// Prepare's.
	unavailable, prepareErr error
	// swept counts the sweeps of stale gate runs; onPrepare runs in Prepare.
	swept     int
	onPrepare func()
}

func (f *fakeFixer) GateAvailable(context.Context) error { return f.unavailable }

func (f *fakeFixer) Sweep(context.Context) error { f.swept++; return nil }

func (f *fakeFixer) Prepare(_ context.Context, branch string) (string, error) {
	f.prepared = append(f.prepared, branch)
	if f.onPrepare != nil {
		f.onPrepare()
	}
	if f.prepareErr != nil {
		return "", f.prepareErr
	}
	if f.dir != "" {
		return f.dir, nil
	}
	return "/work/" + branch, nil
}
func (f *fakeFixer) Changed(context.Context, string) ([]string, error) { return f.changed, nil }
func (f *fakeFixer) Gate(_ context.Context, _ string, tests, fix []string, _ string) error {
	f.gated = append(f.gated, append(slices.Clone(tests), fix...))
	return f.gateErr
}
func (f *fakeFixer) Added(context.Context, string) (string, error) { return f.added, nil }
func (f *fakeFixer) Push(_ context.Context, _, branch string) error {
	f.pushed = append(f.pushed, branch)
	return nil
}
func (f *fakeFixer) Remove(context.Context, string) { f.removed++ }

// world is one job and its fakes.
type world struct {
	job      *job
	tracker  *fakeTracker
	analyst  *fakeAnalyst
	slicer   *fakeSlicer
	fixer    *fakeFixer
	stateDir string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	dir := t.TempDir()
	w := &world{
		tracker: &fakeTracker{self: "owner", issues: map[string][]issue{
			alertLabel: {{Number: 41, Title: "boost: order refused (order-size)", Body: alertBody, Author: "owner", Created: pollTime.Add(time.Minute)}},
		}},
		analyst: &fakeAnalyst{result: analysis{Kind: kindData, WhatHappened: "The order was larger than the limit.", Rule: "ADR 0019, Alerting"}},
		slicer:  &fakeSlicer{slice: covering},
		fixer:   &fakeFixer{},
	}
	w.stateDir = dir
	w.job = &job{
		tracker: w.tracker, analyst: w.analyst, slicer: w.slicer, fixer: w.fixer,
		redactor:  report.NewRedactor(nil),
		statePath: filepath.Join(dir, "state.json"), offFile: filepath.Join(dir, "OFF"),
		dailyCap: 10, location: time.UTC, now: func() time.Time { return pollTime }, sliceWindow: 72 * time.Hour,
		checkCmd: "make check", log: &strings.Builder{},
	}
	return w
}

func (w *world) poll(t *testing.T) {
	t.Helper()
	if err := w.job.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestAFakeAlertIssueGetsOneAnalysisCommentAndASecondPollAddsNothing: a new
// alert issue is analysed once, by one model call, into one comment carrying
// the hidden triage marker; the next poll calls no model and writes nothing.
func TestAFakeAlertIssueGetsOneAnalysisCommentAndASecondPollAddsNothing(t *testing.T) {
	w := newWorld(t)
	w.poll(t)
	own := w.tracker.ownComments(41)
	if len(own) != 1 || len(w.analyst.requests) != 1 {
		t.Fatalf("after one poll: %d comments, %d model calls; want 1 and 1", len(own), len(w.analyst.requests))
	}
	for _, want := range []string{"The order was larger than the limit.", "ADR 0019, Alerting", "Data problem", analysisMarker} {
		if !strings.Contains(own[0], want) {
			t.Errorf("comment lacks %q:\n%s", want, own[0])
		}
	}
	w.poll(t)
	if got := len(w.tracker.ownComments(41)); got != 1 || len(w.analyst.requests) != 1 {
		t.Fatalf("after a second poll: %d comments, %d model calls; want 1 and 1", got, len(w.analyst.requests))
	}
}

// TestTheMarkerOnGitHubDedupesWithoutTheStateFile: a state file lost or
// moved aside never repeats an analysis: the comment on GitHub, written by
// the token's own user, is what says the issue is done.
func TestTheMarkerOnGitHubDedupesWithoutTheStateFile(t *testing.T) {
	w := newWorld(t)
	w.poll(t)
	if err := os.Remove(w.job.statePath); err != nil {
		t.Fatal(err)
	}
	w.job.now = func() time.Time { return pollTime.Add(-time.Hour) } // the new state's start is before the issue
	w.poll(t)
	if got := len(w.tracker.ownComments(41)); got != 1 || len(w.analyst.requests) != 1 {
		t.Fatalf("%d comments, %d model calls; want 1 and 1", got, len(w.analyst.requests))
	}
}

// TestAMarkerSomeoneElseWroteDoesNotCount: only the token's own user's
// comment marks an issue analysed.
func TestAMarkerSomeoneElseWroteDoesNotCount(t *testing.T) {
	w := newWorld(t)
	w.tracker.comments = map[int][]comment{41: {{author: "stranger", body: analysisMarker}}}
	w.poll(t)
	if len(w.analyst.requests) != 1 || len(w.tracker.ownComments(41)) != 1 {
		t.Fatalf("a stranger's marker stopped the analysis: %d model calls", len(w.analyst.requests))
	}
}

// TestAPollWithNothingNewSpendsNoModelUsage: no issue, or only issues opened
// before the job began, or not the reporter's, means no model call and no
// workspace.
func TestAPollWithNothingNewSpendsNoModelUsage(t *testing.T) {
	w := newWorld(t)
	w.tracker.issues[alertLabel] = []issue{
		{Number: 7, Body: alertBody, Author: "owner", Created: pollTime.Add(-24 * time.Hour)},
		{Number: 42, Body: alertBody, Author: "stranger", Created: pollTime.Add(time.Minute)},
		{Number: 43, Body: "no hidden key line", Author: "owner", Created: pollTime.Add(time.Minute)},
	}
	w.poll(t)
	if len(w.analyst.requests) != 0 || w.slicer.calls != 0 || len(w.fixer.prepared) != 0 {
		t.Fatalf("model calls %d, slice fetches %d, workspaces %d; want none", len(w.analyst.requests), w.slicer.calls, len(w.fixer.prepared))
	}
	for _, n := range []int{7, 42, 43} {
		if c := w.tracker.ownComments(n); len(c) != 0 {
			t.Errorf("issue %d got a comment: %v", n, c)
		}
	}
}

// TestAResolvedIssueNeverAnalysedIsAnalysed: the reporter swaps the alert
// label for alert-resolved when the alert clears, so an incident that
// cleared between two polls is found whatever its label, and analysed once
// even when it carried both.
func TestAResolvedIssueNeverAnalysedIsAnalysed(t *testing.T) {
	w := newWorld(t)
	resolved := issue{Number: 44, Title: "boost: no engine heartbeat", Body: alertBody, Author: "owner", Created: pollTime.Add(time.Minute)}
	w.tracker.issues = map[string][]issue{alertLabel: {resolved}, resolvedLabel: {resolved}}
	w.poll(t)
	if len(w.tracker.ownComments(44)) != 1 || len(w.analyst.requests) != 1 {
		t.Fatalf("%d comments, %d model calls; want 1 and 1", len(w.tracker.ownComments(44)), len(w.analyst.requests))
	}
}

// TestTheDailyCapSkipsWithOneCommentAndNoModelCall: past the day's cap a new
// issue gets "triage skipped: daily limit" once and no model call, and is
// never analysed later; the next day the count starts again.
func TestTheDailyCapSkipsWithOneCommentAndNoModelCall(t *testing.T) {
	w := newWorld(t)
	w.job.dailyCap = 1
	w.tracker.issues[alertLabel] = []issue{
		{Number: 46, Body: alertBody, Author: "owner", Created: pollTime.Add(2 * time.Minute)},
		{Number: 45, Body: alertBody, Author: "owner", Created: pollTime.Add(time.Minute)},
	}
	w.poll(t)
	w.poll(t)
	if len(w.analyst.requests) != 1 || w.analyst.requests[0].Issue.Number != 45 {
		t.Fatalf("model calls %v; want one, for the older issue", w.analyst.requests)
	}
	skipped := w.tracker.ownComments(46)
	if len(skipped) != 1 || !strings.Contains(skipped[0], "triage skipped: daily limit") || !strings.Contains(skipped[0], skippedMarker) {
		t.Fatalf("issue over the cap: %v", skipped)
	}
	w.job.now = func() time.Time { return pollTime.Add(24 * time.Hour) }
	w.tracker.issues[alertLabel] = append([]issue{{Number: 47, Body: alertBody, Author: "owner", Created: pollTime.Add(23 * time.Hour)}}, w.tracker.issues[alertLabel]...)
	w.poll(t)
	if len(w.analyst.requests) != 2 || w.analyst.requests[1].Issue.Number != 47 {
		t.Fatalf("the next day: model calls %d; want the new issue analysed", len(w.analyst.requests))
	}
	if len(w.tracker.ownComments(46)) != 1 {
		t.Error("the skipped issue was written to again")
	}
}

// TestTheOffFileStopsTheJob: with the off file in place a poll reads
// nothing, calls no model and writes nothing.
func TestTheOffFileStopsTheJob(t *testing.T) {
	w := newWorld(t)
	if err := os.WriteFile(w.job.offFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w.poll(t)
	if len(w.analyst.requests) != 0 || len(w.tracker.ownComments(41)) != 0 {
		t.Fatal("the job ran with its off file in place")
	}
	if !strings.Contains(w.job.log.(*strings.Builder).String(), "off") {
		t.Errorf("the job did not say it is off: %q", w.job.log)
	}
}

// TestAFailedAnalysisIsNotRetried: at most one model call per issue; a failed
// one is said so in the issue's one comment.
func TestAFailedAnalysisIsNotRetried(t *testing.T) {
	w := newWorld(t)
	w.analyst.err = errors.New("claude exited 1")
	w.poll(t)
	w.poll(t)
	own := w.tracker.ownComments(41)
	if len(w.analyst.requests) != 1 || len(own) != 1 || !strings.Contains(own[0], "triage failed") {
		t.Fatalf("model calls %d, comments %v", len(w.analyst.requests), own)
	}
}

// TestACommentThatFailsIsPostedLaterWithNoSecondModelCall: the analysis is
// kept, redacted, until GitHub takes it.
func TestACommentThatFailsIsPostedLaterWithNoSecondModelCall(t *testing.T) {
	w := newWorld(t)
	w.tracker.failComments = errors.New("GitHub answered 502")
	if err := w.job.poll(context.Background()); err == nil {
		t.Fatal("a comment GitHub refused was not an error")
	}
	w.tracker.failComments = nil
	w.poll(t)
	own := w.tracker.ownComments(41)
	if len(w.analyst.requests) != 1 || len(own) != 1 || !strings.Contains(own[0], "The order was larger than the limit.") {
		t.Fatalf("model calls %d, comments %v", len(w.analyst.requests), own)
	}
}

// TestAnInvocationIsRecordedBeforeTheModelIsCalled: a run that dies while
// Claude works leaves the issue counted and invoked, so the next poll says
// so in a comment and calls no model again.
func TestAnInvocationIsRecordedBeforeTheModelIsCalled(t *testing.T) {
	w := newWorld(t)
	w.analyst.err = errCrash
	w.job.analyst = crashingAnalyst{w.analyst}
	func() {
		defer func() { _ = recover() }()
		_ = w.job.poll(context.Background())
	}()
	w.job.analyst = w.analyst
	w.analyst.err = nil
	w.poll(t)
	own := w.tracker.ownComments(41)
	if len(w.analyst.requests) != 1 || len(own) != 1 || !strings.Contains(own[0], "triage failed") {
		t.Fatalf("model calls %d, comments %v", len(w.analyst.requests), own)
	}
}

var errCrash = errors.New("crash")

// crashingAnalyst records the call, then dies as a killed process would.
type crashingAnalyst struct{ inner *fakeAnalyst }

func (c crashingAnalyst) Analyse(ctx context.Context, req analysisRequest) (analysis, error) {
	_, _ = c.inner.Analyse(ctx, req)
	panic(errCrash)
}

// TestAnUnreadableStateFileCallsNoModel: the record of what was invoked
// cannot be read, so nothing is invoked (fail closed).
func TestAnUnreadableStateFileCallsNoModel(t *testing.T) {
	w := newWorld(t)
	if err := os.WriteFile(w.job.statePath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.job.poll(context.Background()); err == nil {
		t.Fatal("an unreadable state file was not an error")
	}
	if len(w.analyst.requests) != 0 {
		t.Fatal("the model was called with no record of earlier calls")
	}
}

// TestADefectBecomesADraftPullRequestThroughTheGate: a code defect whose
// session changed a test and a fix goes through the gate (tests committed
// first, red there, the check green at the head), is pushed and opened as a
// draft PR, and the comment links it.
func TestADefectBecomesADraftPullRequestThroughTheGate(t *testing.T) {
	w := newWorld(t)
	w.analyst.result = analysis{Kind: kindDefect, WhatHappened: "The gate compared the wrong side.", Rule: "ADR 0026", FixTitle: "Compare the order's own side", FixSummary: "The size check reads the side."}
	w.fixer.changed = []string{"internal/risklimit/gate.go", "internal/risklimit/gate_test.go"}
	w.fixer.added = "+func TestTheSide(t *testing.T) {}"
	w.poll(t)
	if len(w.fixer.gated) != 1 || !slices.Equal(w.fixer.gated[0], []string{"internal/risklimit/gate_test.go", "internal/risklimit/gate.go"}) {
		t.Fatalf("gate saw %v; want the test first, then the fix", w.fixer.gated)
	}
	if len(w.fixer.pushed) != 1 || w.fixer.pushed[0] != "triage/alert-41" || len(w.tracker.pulls) != 1 {
		t.Fatalf("pushed %v, pulls %v", w.fixer.pushed, w.tracker.pulls)
	}
	pr := w.tracker.pulls[0]
	for _, want := range []string{"https://github.com/owner/alerts/issues/41", "The size check reads the side.", "Tests first", "never merged"} {
		if !strings.Contains(pr.body, want) {
			t.Errorf("PR body lacks %q:\n%s", want, pr.body)
		}
	}
	own := w.tracker.ownComments(41)
	if len(own) != 1 || !strings.Contains(own[0], "https://github.com/owner/repo/pull/601") || !strings.Contains(own[0], "Code defect") {
		t.Fatalf("comment %v", own)
	}
	if w.fixer.removed != 1 {
		t.Errorf("workspace removed %d times", w.fixer.removed)
	}
}

// TestADefectWithoutAFailingTestFirstOpensNoPR: the normal flow is tests
// first, so a session that changed no test, or only tests, or whose test
// did not fail before the fix, opens nothing, and the comment says why.
func TestADefectWithoutAFailingTestFirstOpensNoPR(t *testing.T) {
	for name, c := range map[string]struct {
		changed []string
		gate    error
		why     string
	}{
		"no test":    {changed: []string{"internal/risklimit/gate.go"}, why: "changed no test"},
		"only tests": {changed: []string{"internal/risklimit/gate_test.go"}, why: "changed only tests"},
		"not red":    {changed: []string{"internal/risklimit/gate.go", "internal/risklimit/gate_test.go"}, gate: errNotRed, why: "did not fail"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.analyst.result = analysis{Kind: kindDefect, WhatHappened: "x", Rule: "ADR 0026", FixTitle: "t"}
			w.fixer.changed, w.fixer.gateErr = c.changed, c.gate
			w.poll(t)
			own := w.tracker.ownComments(41)
			if len(w.tracker.pulls) != 0 || len(w.fixer.pushed) != 0 || len(own) != 1 || !strings.Contains(own[0], c.why) {
				t.Fatalf("pulls %v, pushed %v, comment %v", w.tracker.pulls, w.fixer.pushed, own)
			}
		})
	}
}

// TestAChangeHoldingAnAccountIDIsNeverPushed: the diff's added lines and
// commit messages are checked by the reporter's redactor before the push.
func TestAChangeHoldingAnAccountIDIsNeverPushed(t *testing.T) {
	w := newWorld(t)
	w.analyst.result = analysis{Kind: kindDefect, WhatHappened: "x", Rule: "ADR 0026", FixTitle: "t"}
	w.fixer.changed = []string{"internal/x/a.go", "internal/x/a_test.go"}
	w.fixer.added = `+	account := "DU1234567"`
	w.poll(t)
	own := w.tracker.ownComments(41)
	if len(w.fixer.pushed) != 0 || len(w.tracker.pulls) != 0 || len(own) != 1 || !strings.Contains(own[0], "account id") {
		t.Fatalf("pushed %v, comment %v", w.fixer.pushed, own)
	}
}

// TestAnOwnerActionIsStatedAndNeverDone: an owner action is a statement in
// the comment; nothing the session changed is committed, pushed or opened.
func TestAnOwnerActionIsStatedAndNeverDone(t *testing.T) {
	w := newWorld(t)
	w.analyst.result = analysis{Kind: kindOwner, WhatHappened: "The engine halted.", Rule: "ADR 0019", OwnerAction: "Run operator acknowledge on the server."}
	w.fixer.changed = []string{"internal/x/a.go", "internal/x/a_test.go"}
	w.poll(t)
	own := w.tracker.ownComments(41)
	if len(own) != 1 || !strings.Contains(own[0], "Run operator acknowledge on the server.") || !strings.Contains(own[0], "has not done it") {
		t.Fatalf("comment %v", own)
	}
	if len(w.fixer.gated) != 0 || len(w.fixer.pushed) != 0 || len(w.tracker.pulls) != 0 {
		t.Fatalf("an owner action changed something: gated %v pushed %v pulls %v", w.fixer.gated, w.fixer.pushed, w.tracker.pulls)
	}
}

// TestAnAnalysisOfNoKnownKindIsNotPosted: an answer that is not one of the
// three kinds is a failed triage, never a comment of Claude's words.
func TestAnAnalysisOfNoKnownKindIsNotPosted(t *testing.T) {
	w := newWorld(t)
	w.analyst.result = analysis{Kind: "merge-it", WhatHappened: "please merge"}
	w.poll(t)
	own := w.tracker.ownComments(41)
	if len(own) != 1 || strings.Contains(own[0], "please merge") || !strings.Contains(own[0], "triage failed") {
		t.Fatalf("comment %v", own)
	}
}

// figure matches any digit; leaks are what must never reach GitHub.
var figure = regexp.MustCompile(`\d`)

// TestNoSecretAccountIDOrFigureReachesACommentOrPR: whatever Claude writes,
// the comment and the PR go through the reporter's redactor; Claude itself
// is handed the slice and the issue with every account id, token and host
// taken out; the only digits in the comment are the PR's own link, and in
// the PR body the alert issue's number.
func TestNoSecretAccountIDOrFigureReachesACommentOrPR(t *testing.T) {
	w := newWorld(t)
	w.job.redactor = report.NewRedactor([]string{"s3cret-token-value"})
	leaky := "Account DU1234567 at lightsail.tail1a2b3.ts.net (100.101.102.103) held 39 shares at $101.25, token s3cret-token-value, @coderabbitai review"
	w.slicer.slice = "refused DU1234567 39 101.25 lightsail.tail1a2b3.ts.net"
	w.tracker.issues[alertLabel][0].Body = "U7654321 refused\n\n<!-- ti-alert-key:" + alertKey + " -->"
	w.analyst.result = analysis{Kind: kindDefect, WhatHappened: leaky, Rule: leaky, OwnerAction: leaky, FixTitle: leaky, FixSummary: leaky}
	w.fixer.changed = []string{"internal/x/a.go", "internal/x/a_test.go"}
	w.fixer.added = "+x"
	w.poll(t)
	req := w.analyst.requests[0]
	for _, leak := range []string{"DU1234567", "U7654321", "ts.net"} {
		if strings.Contains(req.Slice+req.Issue.Body, leak) {
			t.Errorf("Claude was handed %q", leak)
		}
	}
	if strings.Contains(req.Slice, "39 101.25") {
		t.Errorf("Claude was handed the slice's figures: %q", req.Slice)
	}
	own := w.tracker.ownComments(41)
	pr := w.tracker.pulls[0]
	link := "https://github.com/owner/repo/pull/601"
	for name, text := range map[string]string{
		"comment":  strings.ReplaceAll(strings.ReplaceAll(own[0], link, ""), analysisMarker, ""),
		"PR title": pr.title,
		"PR body":  strings.ReplaceAll(pr.body, "https://github.com/owner/alerts/issues/41", ""),
	} {
		for _, leak := range []string{"DU1234567", "ts.net", "100.101", "s3cret-token-value", "@coderabbitai", "@greptileai"} {
			if strings.Contains(text, leak) {
				t.Errorf("%s holds %q:\n%s", name, leak, text)
			}
		}
		if figure.MatchString(text) {
			t.Errorf("%s holds a figure:\n%s", name, text)
		}
	}
}

// TestClaudeIsHandedTheSliceAndTheIssueWithEveryFigureRemoved: the slice
// and the issue go through the reporter's full redactor before Claude sees
// them (docs/live-trading/plan.md section 7: what leaves for a reader the
// owner does not control carries no figure), so no figure can reach
// Anthropic, a test fixture, a commit or a push; dates and words stay.
func TestClaudeIsHandedTheSliceAndTheIssueWithEveryFigureRemoved(t *testing.T) {
	w := newWorld(t)
	w.slicer.slice = covering + "\nrefused order-size on 2026-10-22: 39 shares at 101.25, cash $12,345.67"
	w.tracker.issues[alertLabel][0].Title = "boost: refused 39 at 101.25"
	w.tracker.issues[alertLabel][0].Body = "bought 39 at 101.25\n\n<!-- ti-alert-key:" + alertKey + " -->"
	w.poll(t)
	req := w.analyst.requests[0]
	for _, leak := range []string{"39", "101.25", "12,345.67"} {
		if strings.Contains(req.Slice+req.Issue.Title+req.Issue.Body, leak) {
			t.Errorf("Claude was handed the figure %q:\n%s\n%s\n%s", leak, req.Issue.Title, req.Issue.Body, req.Slice)
		}
	}
	if !strings.Contains(req.Slice, "refused order-size on 2026-10-22") {
		t.Errorf("the slice's words and date were lost: %q", req.Slice)
	}
}

// TestTheDailyCapCountsTheDayOfEachCall: the cap's day is the New York day
// at each model call, not at the poll's start, so a poll that crosses
// midnight charges a call after it to the new day.
func TestTheDailyCapCountsTheDayOfEachCall(t *testing.T) {
	w := newWorld(t)
	w.job.dailyCap = 1
	beforeMidnight := time.Date(2026, time.October, 22, 23, 59, 0, 0, time.UTC)
	w.tracker.issues[alertLabel] = []issue{
		{Number: 46, Body: alertBody, Author: "owner", Created: beforeMidnight.Add(-2 * time.Minute)},
		{Number: 45, Body: alertBody, Author: "owner", Created: beforeMidnight.Add(-3 * time.Minute)},
	}
	clock := beforeMidnight.Add(-time.Hour)
	w.job.now = func() time.Time { return clock }
	w.job.analyst = analystFunc(func(ctx context.Context, req analysisRequest) (analysis, error) {
		a, err := w.analyst.Analyse(ctx, req)
		clock = beforeMidnight.Add(2 * time.Minute) // the first session ends after midnight
		return a, err
	})
	if err := w.job.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.analyst.requests) != 2 {
		t.Fatalf("model calls %d; want both: the second call falls on the next day", len(w.analyst.requests))
	}
	if own := w.tracker.ownComments(46); len(own) != 1 || strings.Contains(own[0], "daily limit") {
		t.Fatalf("the call after midnight was charged to the day before: %v", own)
	}
}

// analystFunc is an analyst made of a function.
type analystFunc func(context.Context, analysisRequest) (analysis, error)

func (f analystFunc) Analyse(ctx context.Context, req analysisRequest) (analysis, error) {
	return f(ctx, req)
}

// TestAnIncompleteOrInvalidStateFileStopsTheJob: a state file that parses
// but lacks a field or breaks an invariant stops the poll with a named
// error, as one that does not parse does: without the record the job cannot
// tell which issues it called the model for.
func TestAnIncompleteOrInvalidStateFileStopsTheJob(t *testing.T) {
	for name, raw := range map[string]string{
		"null":           `null`,
		"empty":          `{}`,
		"no since":       `{"day":"2026-10-22","calls":0,"issues":{}}`,
		"bad day":        `{"since":"2026-10-22T15:00:00Z","day":"Thursday","calls":0,"issues":{}}`,
		"negative calls": `{"since":"2026-10-22T15:00:00Z","day":"2026-10-22","calls":-1,"issues":{}}`,
		"no issues":      `{"since":"2026-10-22T15:00:00Z","day":"2026-10-22","calls":0}`,
		"no calls":       `{"since":"2026-10-22T15:00:00Z","day":"2026-10-22","issues":{}}`,
		"null calls":     `{"since":"2026-10-22T15:00:00Z","day":"2026-10-22","calls":null,"issues":{}}`,
		"bad key":        `{"since":"2026-10-22T15:00:00Z","day":"2026-10-22","calls":0,"issues":{"x":{"done":true}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			if err := os.WriteFile(w.job.statePath, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			err := w.job.poll(context.Background())
			if err == nil || !strings.Contains(err.Error(), "state file") {
				t.Fatalf("err = %v, want the state file named", err)
			}
			if len(w.analyst.requests) != 0 {
				t.Fatal("the model was called on an invalid record")
			}
		})
	}
	w := newWorld(t)
	if err := os.WriteFile(w.job.statePath, []byte(`{"since":"2026-10-22T15:00:00Z","day":"2026-10-22","calls":0,"issues":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w.poll(t)
	if len(w.analyst.requests) != 1 {
		t.Fatal("a complete record did not let the poll run")
	}
}

// TestOnlyGoCodeAndDocsAreOpenedAsAFix: the fix is only *.go files under
// the code folders and files under docs/, and its tests only *_test.go
// files and testdata/ under the code folders; a change touching anything
// else (what defines the check, CI, Claude's settings, the deployment, the
// module's requirements) or holding a binary file is refused before the
// gate.
func TestOnlyGoCodeAndDocsAreOpenedAsAFix(t *testing.T) {
	for _, path := range []string{"Makefile", "scripts/check-coverage.sh", ".github/workflows/ci.yml", ".claude/settings.json",
		"deploy/build-engine.sh", "go.mod", "go.sum", "tools/gate.go", "research/x.py", "adapter/ibkr/loop.py", "internal/x/x.json"} {
		t.Run(path, func(t *testing.T) {
			w := newWorld(t)
			w.analyst.result = analysis{Kind: kindDefect, WhatHappened: "x", Rule: "ADR 0026", FixTitle: "t"}
			w.fixer.changed = []string{"internal/risklimit/gate.go", "internal/risklimit/gate_test.go", path}
			w.poll(t)
			own := w.tracker.ownComments(41)
			if len(w.fixer.gated) != 0 || len(w.fixer.pushed) != 0 || len(own) != 1 || !strings.Contains(own[0], "outside") {
				t.Fatalf("gated %v, pushed %v, comment %v", w.fixer.gated, w.fixer.pushed, own)
			}
		})
	}
	t.Run("binary", func(t *testing.T) {
		w := newWorld(t)
		w.fixer.dir = t.TempDir()
		write(t, w.fixer.dir, "internal/risklimit/testdata/blob.bin", "\x00\x01\x02")
		w.analyst.result = analysis{Kind: kindDefect, WhatHappened: "x", Rule: "ADR 0026", FixTitle: "t"}
		w.fixer.changed = []string{"internal/risklimit/gate.go", "internal/risklimit/gate_test.go", "internal/risklimit/testdata/blob.bin"}
		w.poll(t)
		own := w.tracker.ownComments(41)
		if len(w.fixer.gated) != 0 || len(own) != 1 || !strings.Contains(own[0], "binary") {
			t.Fatalf("gated %v, comment %v", w.fixer.gated, own)
		}
	})
	t.Run("eligible", func(t *testing.T) {
		w := newWorld(t)
		w.analyst.result = analysis{Kind: kindDefect, WhatHappened: "x", Rule: "ADR 0026", FixTitle: "t"}
		w.fixer.changed = []string{"cmd/engine/run.go", "internal/risklimit/gate_test.go", "internal/risklimit/testdata/case.json", "transport/x/x.go", "docs/manual/x.md"}
		w.poll(t)
		if len(w.fixer.gated) != 1 {
			t.Fatalf("an eligible change was refused: %v", w.tracker.ownComments(41))
		}
	})
}

// TestAnInterruptedSessionLeavesTheIssueEligibleOnce: a session stopped by
// a SIGTERM or a bootout is not a failed analysis: the issue gets no
// comment and the next poll analyses it again, once; stopped a second time,
// it is spent, and says so.
func TestAnInterruptedSessionLeavesTheIssueEligibleOnce(t *testing.T) {
	w := newWorld(t)
	w.analyst.err = errInterrupted
	if err := w.job.poll(context.Background()); !errors.Is(err, errInterrupted) {
		t.Fatalf("err = %v, want errInterrupted", err)
	}
	if own := w.tracker.ownComments(41); len(own) != 0 {
		t.Fatalf("an interrupted session was commented on: %v", own)
	}
	w.analyst.err = nil
	w.poll(t)
	if own := w.tracker.ownComments(41); len(w.analyst.requests) != 2 || len(own) != 1 || !strings.Contains(own[0], "The order was larger than the limit.") {
		t.Fatalf("model calls %d, comments %v; want the issue analysed on the next poll", len(w.analyst.requests), own)
	}

	w = newWorld(t)
	w.analyst.err = errInterrupted
	_ = w.job.poll(context.Background())
	_ = w.job.poll(context.Background())
	w.analyst.err = nil
	w.poll(t)
	own := w.tracker.ownComments(41)
	if len(w.analyst.requests) != 2 || len(own) != 1 || !strings.Contains(own[0], "did not finish") {
		t.Fatalf("model calls %d, comments %v; want two tries, then the issue spent", len(w.analyst.requests), own)
	}
}

// TestTheStateIsSyncedBeforeTheModelIsCalled: the record of an invocation
// is synced to disk, file and folder, before the model is called, so a power
// loss never brings back a record without it; a sync that fails stops the
// call.
func TestTheStateIsSyncedBeforeTheModelIsCalled(t *testing.T) {
	w := newWorld(t)
	var synced []string
	saved := syncFile
	t.Cleanup(func() { syncFile = saved })
	syncFile = func(f *os.File) error {
		synced = append(synced, f.Name())
		return f.Sync()
	}
	w.job.analyst = analystFunc(func(ctx context.Context, req analysisRequest) (analysis, error) {
		if !slices.Contains(synced, w.job.statePath+".tmp") || !slices.Contains(synced, filepath.Dir(w.job.statePath)) {
			t.Errorf("the model was called before the record was synced: %v", synced)
		}
		return w.analyst.Analyse(ctx, req)
	})
	w.poll(t)

	w = newWorld(t)
	syncFile = func(*os.File) error { return errors.New("disk gone") }
	if err := w.job.poll(context.Background()); err == nil {
		t.Fatal("a failed sync was not an error")
	}
	if len(w.analyst.requests) != 0 {
		t.Fatal("the model was called on a record not synced")
	}
}

// TestHandledIssuesAreNeverListedAgain: the listing is newest first and
// stops at the issues already handled (state's Through), so a long history
// of handled alerts never fills the listing's bound and never stops the
// job; a listing cut at its bound is analysed, said in the log, and never
// lets Through pass the issues it did not reach.
func TestHandledIssuesAreNeverListedAgain(t *testing.T) {
	w := newWorld(t)
	w.tracker.issues[alertLabel] = []issue{
		{Number: 52, Body: alertBody, Author: "owner", Created: pollTime.Add(3 * time.Minute)},
		{Number: 51, Body: alertBody, Author: "stranger", Created: pollTime.Add(2 * time.Minute)},
		{Number: 50, Body: alertBody, Author: "owner", Created: pollTime.Add(time.Minute)},
	}
	w.poll(t)
	w.tracker.listed = 0
	w.poll(t)
	if w.tracker.listed != 0 {
		t.Fatalf("a second poll listed %d handled issues again", w.tracker.listed)
	}
	if len(w.analyst.requests) != 2 {
		t.Fatalf("model calls %d", len(w.analyst.requests))
	}

	cut := newWorld(t)
	cut.tracker.bound = 1
	cut.tracker.issues[alertLabel] = []issue{
		{Number: 61, Body: alertBody, Author: "owner", Created: pollTime.Add(2 * time.Minute)},
		{Number: 60, Body: alertBody, Author: "owner", Created: pollTime.Add(time.Minute)},
	}
	cut.poll(t)
	if len(cut.analyst.requests) != 1 || cut.analyst.requests[0].Issue.Number != 61 {
		t.Fatalf("a cut listing: model calls %v; want the newest analysed", cut.analyst.requests)
	}
	if !strings.Contains(cut.job.log.(*strings.Builder).String(), "not reached") {
		t.Errorf("the cut was not said in the log: %q", cut.job.log)
	}
	st, err := loadState(cut.job.statePath)
	if err != nil || st.Through >= 60 {
		t.Fatalf("Through %d past an issue never listed (err %v)", st.Through, err)
	}
}

// TestAnUnavailableGateSpendsNothing: without the gate's own user and
// sandbox no session is started, nothing is counted and nothing is posted:
// the poll reports "gate unavailable" in the log, and the issue is analysed
// once the gate can run.
func TestAnUnavailableGateSpendsNothing(t *testing.T) {
	w := newWorld(t)
	w.fixer.unavailable = fmt.Errorf("%w: no macOS user ti-triage-gate", errGateUnavailable)
	err := w.job.poll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "gate unavailable") {
		t.Fatalf("err = %v, want gate unavailable", err)
	}
	st, serr := loadState(w.job.statePath)
	if serr != nil {
		t.Fatal(serr)
	}
	if len(w.analyst.requests) != 0 || len(w.tracker.ownComments(41)) != 0 || *st.Calls != 0 || st.Issues["41"].Invoked {
		t.Fatalf("model calls %d, comments %v, calls counted %d, record %+v; want nothing spent", len(w.analyst.requests), w.tracker.ownComments(41), *st.Calls, st.Issues["41"])
	}
	w.fixer.unavailable = nil
	w.poll(t)
	if own := w.tracker.ownComments(41); len(w.analyst.requests) != 1 || len(own) != 1 || !strings.Contains(own[0], "The order was larger than the limit.") {
		t.Fatalf("model calls %d, comments %v; want the issue analysed once the gate runs", len(w.analyst.requests), own)
	}
}

// TestARepositoryThatCannotBePreparedSpendsNothing: a fetch or a module
// download that fails before the session starts makes no call and posts
// nothing; the next poll tries the issue again.
func TestARepositoryThatCannotBePreparedSpendsNothing(t *testing.T) {
	w := newWorld(t)
	w.fixer.prepareErr = errors.New("git fetch of main failed")
	if err := w.job.poll(context.Background()); err == nil {
		t.Fatal("a repository that could not be prepared was not an error")
	}
	if len(w.analyst.requests) != 0 || len(w.tracker.ownComments(41)) != 0 {
		t.Fatalf("model calls %d, comments %v; want nothing spent", len(w.analyst.requests), w.tracker.ownComments(41))
	}
	w.fixer.prepareErr = nil
	w.poll(t)
	if len(w.analyst.requests) != 1 || len(w.tracker.ownComments(41)) != 1 {
		t.Fatalf("model calls %d, comments %v; want the issue analysed on the next poll", len(w.analyst.requests), w.tracker.ownComments(41))
	}
}

// TestAGateThatCannotRunTheChecksDoesNotSpendTheIssue: a gate that cannot
// run the red or the green check (its run folder, the commit's copy, sudo or
// sandbox-exec failing) is said "gate unavailable" in the log, and the
// issue is analysed again at the next poll, once, as for the job's own
// stop; a second time, the analysis is posted with the fix said not opened
// because the gate was unavailable.
func TestAGateThatCannotRunTheChecksDoesNotSpendTheIssue(t *testing.T) {
	defect := func(w *world) {
		w.analyst.result = analysis{Kind: kindDefect, WhatHappened: "The gate compared the wrong side.", Rule: "ADR 0026", FixTitle: "t", FixSummary: "s"}
		w.fixer.changed = []string{"internal/x/a.go", "internal/x/a_test.go"}
		w.fixer.added = "+ok"
		w.fixer.gateErr = fmt.Errorf("%w: sudo refused the red check", errGateUnavailable)
	}
	w := newWorld(t)
	defect(w)
	if err := w.job.poll(context.Background()); !errors.Is(err, errGateUnavailable) {
		t.Fatalf("err = %v, want gate unavailable", err)
	}
	if own := w.tracker.ownComments(41); len(own) != 0 {
		t.Fatalf("the issue was spent on a gate that could not run: %v", own)
	}
	w.fixer.gateErr = nil
	w.poll(t)
	if own := w.tracker.ownComments(41); len(w.analyst.requests) != 2 || len(own) != 1 || len(w.tracker.pulls) != 1 {
		t.Fatalf("model calls %d, comments %v, pulls %d; want the issue analysed again and its fix opened", len(w.analyst.requests), own, len(w.tracker.pulls))
	}

	w = newWorld(t)
	defect(w)
	_ = w.job.poll(context.Background())
	_ = w.job.poll(context.Background())
	w.poll(t)
	own := w.tracker.ownComments(41)
	if len(w.analyst.requests) != 2 || len(own) != 1 || !strings.Contains(own[0], "gate unavailable") || !strings.Contains(own[0], "The gate compared the wrong side.") {
		t.Fatalf("model calls %d, comments %v; want two tries, then the analysis posted with the gate said unavailable", len(w.analyst.requests), own)
	}
}

// TestTheDailyCapChargesTheDayOfTheModelCallNotOfItsPreparation: the day is
// read, the cap checked and the call recorded only once the preparation
// (the gate's probe, the slice, the worktree, the module downloads) is
// done, immediately before the session starts, so a preparation that
// crosses midnight charges its call to the new day and the new day's cap
// holds.
func TestTheDailyCapChargesTheDayOfTheModelCallNotOfItsPreparation(t *testing.T) {
	w := newWorld(t)
	w.job.dailyCap = 1
	beforeMidnight := time.Date(2026, time.October, 22, 23, 59, 0, 0, time.UTC)
	w.tracker.issues[alertLabel] = []issue{
		{Number: 46, Body: alertBody, Author: "owner", Created: beforeMidnight.Add(-2 * time.Minute)},
		{Number: 45, Body: alertBody, Author: "owner", Created: beforeMidnight.Add(-3 * time.Minute)},
	}
	// The job began earlier that evening.
	if err := saveState(w.job.statePath, &state{Since: beforeMidnight.Add(-time.Hour), Day: "2026-10-22", Calls: new(int), Issues: map[string]issueRecord{}}); err != nil {
		t.Fatal(err)
	}
	clock := beforeMidnight
	w.job.now = func() time.Time { return clock }
	w.fixer.onPrepare = func() { clock = beforeMidnight.Add(2 * time.Minute) } // the first preparation ends after midnight
	_ = w.job.poll(context.Background())
	if len(w.analyst.requests) != 1 {
		t.Fatalf("model calls %d; want one: both calls fall on the new day, whose cap is one", len(w.analyst.requests))
	}
	st, err := loadState(w.job.statePath)
	if err != nil || st.Day != "2026-10-23" || *st.Calls != 1 {
		t.Fatalf("state %+v (err %v); want the call charged to 2026-10-23", st, err)
	}
	if own := w.tracker.ownComments(46); len(own) != 1 || !strings.Contains(own[0], "daily limit") {
		t.Fatalf("the second issue: %v; want it skipped at the new day's cap", own)
	}
}

// TestEachPollSweepsStaleGateRuns: every poll first sweeps the gate's run
// folders a killed job left behind.
func TestEachPollSweepsStaleGateRuns(t *testing.T) {
	w := newWorld(t)
	w.poll(t)
	w.poll(t)
	if w.fixer.swept != 2 {
		t.Fatalf("swept %d times in two polls", w.fixer.swept)
	}
}

// TestAnAlertOpenedBetweenListingsIsNeverSkipped: the job reads the alert
// issues in one consistent listing, so an alert opened while it reads (one
// still open, another opened and resolved at once) is never passed over by
// the handled-through mark: each gets its comment.
func TestAnAlertOpenedBetweenListingsIsNeverSkipped(t *testing.T) {
	w := newWorld(t)
	w.tracker.issues = map[string][]issue{}
	w.tracker.afterListing = func(n int) {
		if n != 1 {
			return
		}
		w.tracker.issues[alertLabel] = []issue{{Number: 101, Body: alertBody, Author: "owner", Created: pollTime.Add(time.Minute)}}
		w.tracker.issues[resolvedLabel] = []issue{{Number: 102, Body: alertBody, Author: "owner", Created: pollTime.Add(2 * time.Minute)}}
	}
	_ = w.job.poll(context.Background())
	w.poll(t)
	w.poll(t)
	for _, n := range []int{101, 102} {
		if own := w.tracker.ownComments(n); len(own) != 1 {
			t.Errorf("issue %d has %d comments; want one", n, len(own))
		}
	}
}

// TestASliceThatDoesNotCoverTheAlertIsSaidSo: the slice is asked for the
// window centred on the alert, naming the alert by its key's hash, and
// checked to cover it: the run that raised the alert (the alert's own
// account's journal, never another's) must span the issue's opening. A
// slice that does not is not handed to Claude as if it did: the analysis is
// told, and the comment says so.
func TestASliceThatDoesNotCoverTheAlertIsSaidSo(t *testing.T) {
	w := newWorld(t)
	w.poll(t)
	created := w.tracker.issues[alertLabel][0].Created
	if !w.slicer.from.Equal(created.Add(-72*time.Hour)) || !w.slicer.to.Equal(created.Add(72*time.Hour)) || w.slicer.alert != alertKey {
		t.Errorf("slice asked for %s to %s, alert %q; want the window centred on the alert, and the alert named", w.slicer.from, w.slicer.to, w.slicer.alert)
	}
	if req := w.analyst.requests[0]; !strings.Contains(req.Slice, "journal-query refusals") || strings.Contains(req.Slice, "did not cover") {
		t.Errorf("a covering slice was not handed over: %q", req.Slice)
	}

	for name, slice := range map[string]string{
		"another run":       "## boost.log: covers 2026-10-23T00:00:00Z to 2026-10-23T01:00:00Z, and raised the alert\n\nrows",
		"an older run":      "## boost.log: covers 2026-10-01T00:00:00Z to 2026-10-02T00:00:00Z, and raised the alert\n\nrows",
		"no span given":     "## boost.log: journal-query refusals\n\nrows",
		"another account's": "## joint.journal: covers 2026-10-22T14:00:00Z to 2026-10-22T15:30:00Z\n\n## boost.log: covers 2026-10-01T00:00:00Z to 2026-10-02T00:00:00Z, and raised the alert\n\nrows",
		"no journal raised": "## boost.log: covers 2026-10-22T14:00:00Z to 2026-10-22T15:30:00Z\n\nrows",
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.slicer.slice = slice
			w.poll(t)
			req := w.analyst.requests[0]
			own := w.tracker.ownComments(41)
			if !strings.Contains(req.Slice, "did not cover") || strings.Contains(req.Slice, "rows") {
				t.Errorf("Claude was handed %q", req.Slice)
			}
			if len(own) != 1 || !strings.Contains(own[0], "did not cover") {
				t.Errorf("the comment does not say the slice did not cover the alert: %v", own)
			}
		})
	}
}
