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

// alertBody is an alert issue's body as the reporter writes it: its text,
// then the hidden line tying it to its alert key.
const alertBody = "The risk gate refused one order.\n\n<!-- ti-alert-key:abc123 -->"

var pollTime = time.Date(2026, time.October, 22, 15, 0, 0, 0, time.UTC)

// fakeTracker is GitHub's issues and pulls held in memory.
type fakeTracker struct {
	self     string
	issues   map[string][]issue // by label, newest first
	comments map[int][]comment
	pulls    []fakePull
	// failComments fails every comment write.
	failComments error
}

type comment struct{ author, body string }

type fakePull struct{ head, title, body string }

func (f *fakeTracker) Self(context.Context) (string, error) { return f.self, nil }

func (f *fakeTracker) AlertIssues(_ context.Context, label string, since time.Time) ([]issue, error) {
	var out []issue
	for _, is := range f.issues[label] {
		if !is.Created.Before(since) {
			out = append(out, is)
		}
	}
	return out, nil
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
	slice string
	calls int
}

func (f *fakeSlicer) Fetch(context.Context) (string, error) {
	f.calls++
	return f.slice, nil
}

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
}

func (f *fakeFixer) Prepare(_ context.Context, branch string) (string, error) {
	f.prepared = append(f.prepared, branch)
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
		slicer:  &fakeSlicer{slice: "## boost.log: journal-query refusals\n\nrefused order-size"},
		fixer:   &fakeFixer{},
	}
	w.stateDir = dir
	w.job = &job{
		tracker: w.tracker, analyst: w.analyst, slicer: w.slicer, fixer: w.fixer,
		redactor:  report.NewRedactor(nil),
		statePath: filepath.Join(dir, "state.json"), offFile: filepath.Join(dir, "OFF"),
		dailyCap: 10, location: time.UTC, now: func() time.Time { return pollTime },
		testCmd: "go test ./...", checkCmd: "make check", log: &strings.Builder{},
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
// cleared between two polls is found by the resolved label, and analysed
// once even when the two listings both show it.
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
	for _, want := range []string{"#41", "The size check reads the side.", "Tests first", "never merged"} {
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
	w.fixer.changed = []string{"a.go", "a_test.go"}
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
	w.fixer.changed = []string{"a.go", "a_test.go"}
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
	w.tracker.issues[alertLabel][0].Body = "U7654321 refused\n\n<!-- ti-alert-key:abc123 -->"
	w.analyst.result = analysis{Kind: kindDefect, WhatHappened: leaky, Rule: leaky, OwnerAction: leaky, FixTitle: leaky, FixSummary: leaky}
	w.fixer.changed = []string{"a.go", "a_test.go"}
	w.fixer.added = "+x"
	w.poll(t)
	req := w.analyst.requests[0]
	for _, leak := range []string{"DU1234567", "U7654321", "ts.net"} {
		if strings.Contains(req.Slice+req.Issue.Body, leak) {
			t.Errorf("Claude was handed %q", leak)
		}
	}
	if !strings.Contains(req.Slice, "39 101.25") {
		t.Errorf("Claude was not handed the slice's figures: %q", req.Slice)
	}
	own := w.tracker.ownComments(41)
	pr := w.tracker.pulls[0]
	link := "https://github.com/owner/repo/pull/601"
	for name, text := range map[string]string{
		"comment":  strings.ReplaceAll(strings.ReplaceAll(own[0], link, ""), analysisMarker, ""),
		"PR title": pr.title,
		"PR body":  strings.ReplaceAll(pr.body, "#41", ""),
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
