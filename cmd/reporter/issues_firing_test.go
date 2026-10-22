package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// The github-issues channel across one firing and across incidents: the
// firing's one deadline, the failures it remembers, what a hand close, a
// forgotten alert and a reminder do, and which comments count as the
// channel's own (ADR 0019, "Alerting"; docs/manual/alerts-and-dashboard.md,
// "Alerts as GitHub Issues").

// orderSender is a paging channel that notes, in log, each alert it takes.
type orderSender struct {
	label string
	log   *[]string
}

func (o orderSender) name() string { return o.label }
func (o orderSender) send(report.Alert, int) error {
	*o.log = append(*o.log, o.label)
	return nil
}

// A first delivery sent again after the owner closed its issue by hand is
// that issue's, and opens no second one.
func TestAFirstDeliveryRetriedAfterAHandCloseOpensNoSecondIssue(t *testing.T) {
	tr := &fakeTracker{}
	a := haltAlert()
	if err := newIssues(tr).send(a, 0); err != nil {
		t.Fatal(err)
	}
	tr.issues[0].open = false
	if err := newIssues(tr).send(a, 0); err != nil {
		t.Fatal(err)
	}
	if len(tr.issues) != 1 {
		t.Fatalf("%d issues after a retried first delivery, want the one closed by hand", len(tr.issues))
	}
}

// A repeat of an alert whose issue is gone from the listing (its label taken
// off, or the issue deleted) opens no new issue.
func TestARepeatOpensNoIssueWhenTheIssueIsGone(t *testing.T) {
	tr := &fakeTracker{}
	if err := newIssues(tr).send(haltAlert(), 2); err != nil {
		t.Fatal(err)
	}
	if len(tr.issues) != 0 {
		t.Fatalf("a repeat opened %d issues", len(tr.issues))
	}
}

// A forgotten alert (a history repaired, a restored log cleared) closes its
// issue, so the same key arising again is a new issue that notifies.
func TestAForgottenAlertClosesItsIssue(t *testing.T) {
	tr := &fakeTracker{}
	s := newIssues(tr)
	pager := orderSender{label: "ntfy", log: &[]string{}}
	prefix := report.HistoryUnavailableKeyPrefix("joint")
	a := haltAlert()
	a.Key, a.Repeat = prefix+"7", false
	seen, sent, repeats := map[string]bool{}, map[string]time.Time{}, map[string]int{}
	senders := []sender{pager, s}
	if err := deliver(senders, a, seen, sent, repeats, t0, time.Minute, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	forget(prefix, seen, sent, repeats)
	if err := closeForgotten(senders, []string{prefix}, seen, sent, repeats); err != nil {
		t.Fatal(err)
	}
	if tr.issues[0].open {
		t.Fatal("the forgotten alert's issue is still open")
	}
	if len(seen) != 0 {
		t.Fatalf("records left after the close: %v", seen)
	}
	if err := deliver(senders, a, seen, sent, repeats, t0.Add(time.Hour), time.Minute, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(tr.issues) != 2 || !tr.issues[1].open {
		t.Fatalf("the same key arising again made %d issues, want a new open one", len(tr.issues))
	}
}

// A failed close keeps the tracker's record, so the next firing tries again.
func TestAForgottenAlertWhoseCloseFailsIsKept(t *testing.T) {
	tr := &fakeTracker{}
	s := newIssues(tr)
	prefix := report.HistoryUnavailableKeyPrefix("joint")
	a := haltAlert()
	a.Key, a.Repeat = prefix+"7", false
	seen, sent, repeats := map[string]bool{}, map[string]time.Time{}, map[string]int{}
	if err := deliver([]sender{s}, a, seen, sent, repeats, t0, time.Minute, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	forget(prefix, seen, sent, repeats)
	tr.failing = fmt.Errorf("GitHub answered 422")
	if err := closeForgotten([]sender{s}, []string{prefix}, seen, sent, repeats); err == nil {
		t.Fatal("a failed close is not an error")
	}
	if !seen[channelKey(issuesChannel, a.Key)] {
		t.Fatal("a failed close dropped the record it must try again")
	}
}

// One firing's tracker work runs within one send timeout, however many
// alerts it has: a slow tracker that answers every call in time still ends
// the firing by its deadline, and the later alerts fail at once.
func TestASlowTrackerEndsTheFiringByItsDeadline(t *testing.T) {
	tr := &fakeTracker{delay: 100 * time.Millisecond}
	s := newIssues(tr)
	s.timeout = 300 * time.Millisecond
	start := time.Now()
	var failed int
	for n := range 10 {
		a := haltAlert()
		a.Key = fmt.Sprintf("engine-state:joint@%d", n)
		if s.send(a, 0) != nil {
			failed++
		}
	}
	if took := time.Since(start); took > 600*time.Millisecond || failed == 0 {
		t.Fatalf("ten alerts on a tracker of 100ms calls under a 300ms send timeout took %v with %d failed", took, failed)
	}
}

// A rate limit or a server error is remembered for the firing like a failed
// listing: the later alerts do not ask GitHub again.
func TestARateLimitOrServerErrorIsRememberedForTheFiring(t *testing.T) {
	for name, gh := range map[string]*fakeGitHub{
		"502":         {writeStatus: http.StatusBadGateway},
		"429":         {writeStatus: http.StatusTooManyRequests},
		"retry-after": {writeStatus: http.StatusForbidden, retryAfter: true},
	} {
		url := fakeGitHubServer(t, gh)
		s := githubChannel(t, url, time.Second)
		a, b := haltAlert(), haltAlert()
		b.Key = "reconciliation:x@joint"
		if err := s.send(a, 0); err == nil {
			t.Fatalf("%s: a refused create counted as delivered", name)
		}
		gh.mu.Lock()
		before := len(gh.methods)
		gh.mu.Unlock()
		if err := s.send(b, 0); err == nil {
			t.Fatalf("%s: the later alert was not failed", name)
		}
		gh.mu.Lock()
		after := len(gh.methods)
		gh.mu.Unlock()
		if after != before {
			t.Errorf("%s: GitHub was asked %d more times after it answered %s", name, after-before, name)
		}
	}
}

// An answer whose body stops coming is a timeout like any other, and is
// remembered for the firing.
func TestAStalledAnswerIsRememberedForTheFiring(t *testing.T) {
	gh := &fakeGitHub{stallWrites: true}
	url := fakeGitHubServer(t, gh)
	s := githubChannel(t, url, 200*time.Millisecond)
	s.timeout = 10 * time.Second
	a, b := haltAlert(), haltAlert()
	b.Key = "reconciliation:x@joint"
	if err := s.send(a, 0); err == nil {
		t.Fatal("a stalled answer counted as delivered")
	}
	if err := s.send(b, 0); err == nil {
		t.Fatal("the later alert was not failed")
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.methods) != 2 {
		t.Fatalf("GitHub was asked %d times, want 2 (the listing and the stalled create): %v", len(gh.methods), gh.methods)
	}
}

// A listed issue whose state is missing is an error, never read as closed.
func TestAnIssueOfNoStateIsAnError(t *testing.T) {
	gh := &fakeGitHub{}
	url := fakeGitHubServer(t, gh)
	a := haltAlert()
	if err := githubChannel(t, url, time.Second).send(a, 0); err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	gh.omitState = true
	gh.mu.Unlock()
	if err := githubChannel(t, url, time.Second).send(a, 1); err == nil {
		t.Fatal("an issue of no state was taken as closed and the repeat as delivered")
	}
}

// A delivered repeat is found however many comments came after it: every
// page of comments is read, not only the last.
func TestADeliveredRepeatIsFoundOnAnyPage(t *testing.T) {
	gh := &fakeGitHub{}
	url := fakeGitHubServer(t, gh)
	a := haltAlert()
	for repeat := range 2 {
		if err := githubChannel(t, url, time.Second).send(a, repeat); err != nil {
			t.Fatal(err)
		}
	}
	gh.mu.Lock()
	for n := range 150 {
		gh.issues[0].comments = append(gh.issues[0].comments, fmt.Sprintf("a note %d", n))
	}
	gh.mu.Unlock()
	if err := githubChannel(t, url, time.Second).send(a, 1); err != nil {
		t.Fatal(err)
	}
	if n := len(gh.snapshot()[0].comments); n != 151 {
		t.Fatalf("%d comments, want 151: the repeat was posted again", n)
	}
}

// A delivery marker in a comment someone else wrote is not the channel's:
// the repeat is still posted.
func TestAForgedDeliveryMarkerIsIgnored(t *testing.T) {
	gh := &fakeGitHub{}
	url := fakeGitHubServer(t, gh)
	a := haltAlert()
	if err := githubChannel(t, url, time.Second).send(a, 0); err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	gh.issues[0].comments = append(gh.issues[0].comments, deliveryMarker(a.Key, 1))
	gh.issues[0].strangers = map[int]bool{0: true}
	gh.mu.Unlock()
	if err := githubChannel(t, url, time.Second).send(a, 1); err != nil {
		t.Fatal(err)
	}
	issue := gh.snapshot()[0]
	if len(issue.comments) != 2 || !strings.Contains(issue.comments[1], "repeat 1") {
		t.Fatalf("comments %q: a forged marker stopped the repeat", issue.comments)
	}
}

// A failed unit's reminder reaches the paging channels before the issue
// tracker, whatever the channel order, and is a comment on the open issue,
// one per reminder, never counted as delivered unwritten.
func TestAUnitReminderPagesFirstAndCommentsOnTheIssue(t *testing.T) {
	var log []string
	tr := &fakeTracker{}
	tr.onCall = func() { log = append(log, issuesChannel) }
	s := newIssues(tr)
	pager := orderSender{label: "pagerduty", log: &log}
	unit, episode := "ti-adapter-boost.service", "run-a"
	key := report.UnitFailedKey(unit, episode)
	u := &unitState{path: filepath.Join(t.TempDir(), "unit-failures.json"), seen: map[string]bool{}, sent: map[string]time.Time{}, repeats: map[string]int{}, baselines: map[string]time.Time{}}
	if err := s.send(report.UnitFailedAlert("host", unit, episode, "it failed", t0), 0); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{issuesChannel, "pagerduty"} {
		u.seen[channelKey(name, key)], u.sent[channelKey(name, key)] = true, t0
	}
	senders := []sender{s, pager}
	for n, at := range []time.Time{t0.Add(2 * time.Hour), t0.Add(4 * time.Hour)} {
		log = nil
		if err := remindRun(u, unit, episode, "", senders, at); err != nil {
			t.Fatal(err)
		}
		if len(log) == 0 || log[0] != "pagerduty" {
			t.Fatalf("reminder %d went out in the order %v, want PagerDuty first", n+1, log)
		}
		if got := len(tr.issues[0].comments); got != n+1 {
			t.Fatalf("after reminder %d the issue has %d comments, want %d", n+1, got, n+1)
		}
	}
}

// The firing's time for the tracker is spent only by tracker work: local
// work between two deliveries (a unit step, a clock check) is not charged to
// the later one.
func TestOnlyTrackerWorkSpendsTheFiringsTrackerTime(t *testing.T) {
	tr := &fakeTracker{delay: 50 * time.Millisecond}
	s := newIssues(tr)
	s.timeout = 300 * time.Millisecond
	a, b := haltAlert(), haltAlert()
	b.Key = "clock-drift:host"
	if err := s.send(a, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if err := s.send(b, 0); err != nil {
		t.Fatalf("a delivery after 400ms of local work, with 200ms of tracker time left, was refused: %v", err)
	}
}

// A close that keeps failing for an alert the run forgot (a history whole
// again, a restored log cleared) is tried again at every firing, whether or
// not the fault is back, and after maxCloseTries firings its record is
// dropped, so the next incident opens a fresh issue naming the one left
// open: an unfinished close never suppresses the next incident for ever.
func TestAForgottenAlertWhoseCloseKeepsFailingOpensAFreshIssue(t *testing.T) {
	tr := &fakeTracker{}
	s := newIssues(tr)
	prefix := report.HistoryUnavailableKeyPrefix("joint")
	a := haltAlert()
	a.Key, a.Repeat = prefix+"7", false
	seen, sent, repeats := map[string]bool{}, map[string]time.Time{}, map[string]int{}
	s.records = repeats
	record := func() error { return nil }
	if err := deliver([]sender{s}, a, seen, sent, repeats, t0, time.Minute, record); err != nil {
		t.Fatal(err)
	}
	forget(prefix, seen, sent, repeats)
	tr.closeFailing = fmt.Errorf("GitHub answered 422")
	if err := closeForgotten([]sender{s}, []string{prefix}, seen, sent, repeats); err == nil {
		t.Fatal("a failed close is not an error")
	}
	// The fault is back: the run forgets nothing, yet the close is tried.
	for n := 1; n < maxCloseTries; n++ {
		if err := deliver([]sender{s}, a, seen, sent, repeats, t0.Add(time.Duration(n)*time.Hour), time.Minute, record); err != nil {
			t.Fatal(err)
		}
		if err := closeForgotten([]sender{s}, nil, seen, sent, repeats); err == nil {
			t.Fatalf("try %d: a failed close is not an error", n+1)
		}
	}
	if seen[channelKey(issuesChannel, a.Key)] {
		t.Fatal("the record of the cleared incident is kept after every try failed")
	}
	if err := deliver([]sender{s}, a, seen, sent, repeats, t0.Add(10*time.Hour), time.Minute, record); err != nil {
		t.Fatal(err)
	}
	if len(tr.issues) != 2 || !tr.issues[1].open || !strings.Contains(tr.issues[1].body, "#1") {
		t.Fatalf("%d issues; the next incident must open a fresh issue naming #1: %+v", len(tr.issues), tr.issues)
	}
}

// A close tried again while the fault is back that succeeds drops the
// record, so the incident in force opens its own issue.
func TestAForgottenAlertsCloseIsRetriedWhileTheFaultIsBack(t *testing.T) {
	tr := &fakeTracker{}
	s := newIssues(tr)
	prefix := report.HistoryUnavailableKeyPrefix("joint")
	a := haltAlert()
	a.Key, a.Repeat = prefix+"7", false
	seen, sent, repeats := map[string]bool{}, map[string]time.Time{}, map[string]int{}
	s.records = repeats
	record := func() error { return nil }
	if err := deliver([]sender{s}, a, seen, sent, repeats, t0, time.Minute, record); err != nil {
		t.Fatal(err)
	}
	forget(prefix, seen, sent, repeats)
	tr.closeFailing = fmt.Errorf("GitHub answered 422")
	_ = closeForgotten([]sender{s}, []string{prefix}, seen, sent, repeats)
	tr.closeFailing = nil
	if err := closeForgotten([]sender{s}, nil, seen, sent, repeats); err != nil {
		t.Fatal(err)
	}
	if tr.issues[0].open || seen[channelKey(issuesChannel, a.Key)] || len(repeats) != 0 {
		t.Fatalf("open=%v seen=%v repeats=%v: the retried close must close and drop its records", tr.issues[0].open, seen, repeats)
	}
	if err := deliver([]sender{s}, a, seen, sent, repeats, t0.Add(time.Hour), time.Minute, record); err != nil {
		t.Fatal(err)
	}
	if len(tr.issues) != 2 || !tr.issues[1].open {
		t.Fatalf("%d issues, want a new open one for the incident in force", len(tr.issues))
	}
}

// A resolved issue loses the alert label, so closed incidents leave the
// listing and never fill it.
func TestAResolvedIssueLosesItsAlertLabel(t *testing.T) {
	gh := &fakeGitHub{}
	url := fakeGitHubServer(t, gh)
	a := haltAlert()
	if err := githubChannel(t, url, time.Second).send(a, 0); err != nil {
		t.Fatal(err)
	}
	if err := githubChannel(t, url, time.Second).resolve(a.Key); err != nil {
		t.Fatal(err)
	}
	if i := gh.snapshot()[0]; i.open || len(i.labels) != 0 {
		t.Fatalf("open=%v labels=%v, want closed with no alert label", i.open, i.labels)
	}
	if err := githubChannel(t, url, time.Second).send(a, 0); err != nil {
		t.Fatal(err)
	}
	if issues := gh.snapshot(); len(issues) != 2 || !issues[1].open {
		t.Fatalf("%d issues, want a new incident's open issue", len(issues))
	}
}

// overLimit gives the issue more comments than the channel reads.
func overLimit(gh *fakeGitHub, n int) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	for c := range maxCommentPages*100 + 1 {
		gh.issues[n-1].comments = append(gh.issues[n-1].comments, fmt.Sprintf("note %d", c))
	}
}

// An issue over the comment limit closes with no closing comment, so a
// close tried again after a failed state change posts nothing.
func TestAResolveOverTheCommentLimitPostsNoClosingComment(t *testing.T) {
	gh := &fakeGitHub{}
	url := fakeGitHubServer(t, gh)
	a := haltAlert()
	if err := githubChannel(t, url, time.Second).send(a, 0); err != nil {
		t.Fatal(err)
	}
	overLimit(gh, 1)
	gh.mu.Lock()
	gh.patchFailures = 1
	gh.mu.Unlock()
	if err := githubChannel(t, url, 5*time.Second).resolve(a.Key); err == nil {
		t.Fatal("a refused close must fail the resolve")
	}
	if err := githubChannel(t, url, 5*time.Second).resolve(a.Key); err != nil {
		t.Fatal(err)
	}
	if i := gh.snapshot()[0]; i.open || len(i.comments) != maxCommentPages*100+1 {
		t.Fatalf("open=%v with %d comments, want closed with no comment added", i.open, len(i.comments))
	}
}

// A closed issue over the comment limit is taken as closed by overflow: the
// next incident of its key opens a fresh issue that says why.
func TestAnIssueOverTheCommentLimitDoesNotBlockTheNextIncident(t *testing.T) {
	gh := &fakeGitHub{}
	url := fakeGitHubServer(t, gh)
	a := haltAlert()
	if err := githubChannel(t, url, time.Second).send(a, 0); err != nil {
		t.Fatal(err)
	}
	overLimit(gh, 1)
	gh.mu.Lock()
	gh.issues[0].open = false
	gh.mu.Unlock()
	if err := githubChannel(t, url, 5*time.Second).send(a, 0); err != nil {
		t.Fatal(err)
	}
	issues := gh.snapshot()
	if len(issues) != 2 || !issues[1].open || !strings.Contains(issues[1].body, "#1") {
		t.Fatalf("%d issues; want a fresh open issue naming #1", len(issues))
	}
}

// A comment GitHub names no author of makes the delivery fail closed: it is
// not taken for somebody else's, and nothing is written.
func TestACommentOfNoAuthorFailsTheDeliveryClosed(t *testing.T) {
	gh := &fakeGitHub{}
	url := fakeGitHubServer(t, gh)
	a := haltAlert()
	if err := githubChannel(t, url, time.Second).send(a, 0); err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	gh.issues[0].comments = append(gh.issues[0].comments, deliveryMarker(a.Key, 1))
	gh.issues[0].anonymous = map[int]bool{0: true}
	gh.mu.Unlock()
	if err := githubChannel(t, url, time.Second).send(a, 1); err == nil {
		t.Fatal("a comment of no author was taken as somebody else's")
	}
	if n := len(gh.snapshot()[0].comments); n != 1 {
		t.Fatalf("%d comments, want the one already there", n)
	}
}

// A failed lookup of the token's user is made once in a firing and remembered:
// the later deliveries that need it fail at once, and nothing is written.
func TestAFailedIdentityLookupIsMadeOnceAndWritesNothing(t *testing.T) {
	for name, gh := range map[string]*fakeGitHub{
		"401":        {userStatus: http.StatusUnauthorized},
		"invalid id": {userID: -1},
	} {
		url := fakeGitHubServer(t, gh)
		a, b := haltAlert(), haltAlert()
		b.Key = "reconciliation:x@joint"
		for _, x := range []report.Alert{a, b} {
			if err := githubChannel(t, url, time.Second).send(x, 0); err != nil {
				t.Fatal(err)
			}
		}
		gh.mu.Lock()
		gh.methods = nil
		gh.mu.Unlock()
		s := githubChannel(t, url, time.Second)
		for _, x := range []report.Alert{a, b} {
			if err := s.send(x, 1); err == nil {
				t.Fatalf("%s: a repeat was delivered with no identity to check it by", name)
			}
		}
		gh.mu.Lock()
		users := 0
		for _, m := range gh.methods {
			if m == "GET /user" {
				users++
			}
		}
		gh.mu.Unlock()
		if users != 1 || commentsOf(gh.snapshot()) != 0 {
			t.Fatalf("%s: the token's user was asked %d times and %d comments written, want once and none", name, users, commentsOf(gh.snapshot()))
		}
	}
}

// An issue closed by hand whose alert label was also removed is out of
// sight: resolving its alert writes nothing, and the next incident opens a
// new issue.
func TestAHandClosedUnlabelledIssueResolvesQuietly(t *testing.T) {
	gh := &fakeGitHub{}
	url := fakeGitHubServer(t, gh)
	a := haltAlert()
	if err := githubChannel(t, url, time.Second).send(a, 0); err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	gh.issues[0].open, gh.issues[0].labels = false, nil
	gh.mu.Unlock()
	if err := githubChannel(t, url, time.Second).resolve(a.Key); err != nil {
		t.Fatal(err)
	}
	if n := commentsOf(gh.snapshot()); n != 0 {
		t.Fatalf("resolving an issue out of sight wrote %d comments", n)
	}
	if err := githubChannel(t, url, time.Second).send(a, 0); err != nil {
		t.Fatal(err)
	}
	if issues := gh.snapshot(); len(issues) != 2 || !issues[1].open {
		t.Fatalf("%d issues, want the next incident's new open issue", len(issues))
	}
}
