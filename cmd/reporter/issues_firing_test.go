package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
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
	if len(tr.issues) != 2 || !tr.issues[1].open || !strings.Contains(tr.issues[1].body, "#1") || !strings.Contains(tr.issues[1].body, "left open earlier") || strings.Contains(tr.issues[1].body, "could not be closed") {
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
	if tr.issues[0].open || seen[channelKey(issuesChannel, a.Key)] || repeats[closeTriesKey(a.Key)] != 0 || len(repeats) > 1 {
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
// listing, and gains alert-resolved, so a triage job polling by label still
// finds it.
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
	if i := gh.snapshot()[0]; i.open || len(i.labels) != 1 || i.labels[0] != "alert-resolved" {
		t.Fatalf("open=%v labels=%v, want closed labelled alert-resolved only", i.open, i.labels)
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

// giveUp runs the forgotten alert's close through every try GitHub refuses,
// and returns the state the run keeps.
func giveUp(t *testing.T, s *issuesSender, tr *fakeTracker, a report.Alert) (seen map[string]bool, sent map[string]time.Time, repeats map[string]int) {
	t.Helper()
	prefix := report.HistoryUnavailableKeyPrefix("joint")
	seen, sent, repeats = map[string]bool{}, map[string]time.Time{}, map[string]int{}
	s.records = repeats
	record := func() error { return nil }
	if err := deliver([]sender{s}, a, seen, sent, repeats, t0, time.Minute, record); err != nil {
		t.Fatal(err)
	}
	forget(prefix, seen, sent, repeats)
	tr.closeFailing = fmt.Errorf("GitHub answered 422")
	_ = closeForgotten([]sender{s}, []string{prefix}, seen, sent, repeats)
	for n := 1; n < maxCloseTries; n++ {
		if err := deliver([]sender{s}, a, seen, sent, repeats, t0.Add(time.Duration(n)*time.Hour), time.Minute, record); err != nil {
			t.Fatal(err)
		}
		_ = closeForgotten([]sender{s}, nil, seen, sent, repeats)
	}
	tr.closeFailing = nil
	return seen, sent, repeats
}

func forgottenAlert() report.Alert {
	a := haltAlert()
	a.Key, a.Repeat = report.HistoryUnavailableKeyPrefix("joint")+"7", false
	return a
}

// An issue whose close was given up never suppresses a later incident, even
// after its replacement is resolved and leaves the label-keyed listing: the
// replacement chain is the local record's, not the listing's (ADR 0019,
// "Alerting": once per incident).
func TestAnAbandonedIssueNeverSuppressesALaterIncident(t *testing.T) {
	tr := &fakeTracker{}
	s := newIssues(tr)
	a := forgottenAlert()
	seen, sent, repeats := giveUp(t, s, tr, a)
	record := func() error { return nil }
	if err := deliver([]sender{s}, a, seen, sent, repeats, t0.Add(10*time.Hour), time.Minute, record); err != nil {
		t.Fatal(err)
	}
	if len(tr.issues) != 2 {
		t.Fatalf("%d issues, want the replacement", len(tr.issues))
	}
	// The replacement's incident clears and is resolved.
	forget(report.HistoryUnavailableKeyPrefix("joint"), seen, sent, repeats)
	if err := closeForgotten([]sender{s}, []string{report.HistoryUnavailableKeyPrefix("joint")}, seen, sent, repeats); err != nil {
		t.Fatal(err)
	}
	if tr.issues[1].open || tr.issues[1].labelled() {
		t.Fatalf("the replacement is open=%v labelled=%v, want resolved", tr.issues[1].open, tr.issues[1].labelled())
	}
	if err := deliver([]sender{s}, a, seen, sent, repeats, t0.Add(20*time.Hour), time.Minute, record); err != nil {
		t.Fatal(err)
	}
	if len(tr.issues) != 3 || !tr.issues[2].open {
		t.Fatalf("%d issues, want a third, open, for the later incident", len(tr.issues))
	}
}

// A crash after the replacement was created, before the run saved its state,
// opens no second replacement on restart: the replacement is found by its
// marker, and the abandoned issue's number was saved when it was given up.
func TestACrashAfterTheReplacementOpensNoSecondOne(t *testing.T) {
	tr := &fakeTracker{}
	s := newIssues(tr)
	a := forgottenAlert()
	seen, sent, repeats := giveUp(t, s, tr, a)
	savedSeen, savedRepeats := maps.Clone(seen), maps.Clone(repeats)
	if err := deliver([]sender{s}, a, seen, sent, repeats, t0.Add(10*time.Hour), time.Minute, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	// The process died before the state was saved: the run restarts from it.
	s2 := newIssues(tr)
	s2.records = savedRepeats
	if err := deliver([]sender{s2}, a, savedSeen, map[string]time.Time{}, savedRepeats, t0.Add(10*time.Hour), time.Minute, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(tr.issues) != 2 {
		t.Fatalf("%d issues, want one replacement only", len(tr.issues))
	}
}

// A close that was not tried (GitHub down or the firing's time spent) is not
// one of the three tries.
func TestAnUntriedCloseIsNotCounted(t *testing.T) {
	for name, fail := range map[string]func(s *issuesSender, tr *fakeTracker){
		"not tried": func(s *issuesSender, _ *fakeTracker) { s.down = errUnavailable },
		"unavailable": func(_ *issuesSender, tr *fakeTracker) {
			tr.closeFailing = fmt.Errorf("request failed (%w)", errUnavailable)
		},
		"listing": func(_ *issuesSender, tr *fakeTracker) { tr.failing = fmt.Errorf("request failed (%w)", errUnavailable) },
	} {
		t.Run(name, func(t *testing.T) {
			tr := &fakeTracker{}
			s := newIssues(tr)
			prefix := report.HistoryUnavailableKeyPrefix("joint")
			a := forgottenAlert()
			seen, sent, repeats := map[string]bool{}, map[string]time.Time{}, map[string]int{}
			s.records = repeats
			if err := deliver([]sender{s}, a, seen, sent, repeats, t0, time.Minute, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			forget(prefix, seen, sent, repeats)
			for n := range maxCloseTries + 2 {
				s := newIssues(tr)
				s.records = repeats
				fail(s, tr)
				if err := closeForgotten([]sender{s}, []string{prefix}, seen, sent, repeats); err == nil {
					t.Fatalf("firing %d: an untried close is not an error", n)
				}
			}
			if !seen[channelKey(issuesChannel, a.Key)] || repeats[closeTriesKey(a.Key)] != 0 || len(repeats) > 1 {
				t.Fatalf("seen=%v repeats=%v: an untried close must keep its record", seen, repeats)
			}
			tr.failing, tr.closeFailing = nil, nil
			s = newIssues(tr)
			s.records = repeats
			if err := closeForgotten([]sender{s}, nil, seen, sent, repeats); err != nil || tr.issues[0].open {
				t.Fatalf("err=%v open=%v: the close must still be made", err, tr.issues[0].open)
			}
		})
	}
}

// A close counts as a try only when a close request was sent to GitHub and
// refused: a failed read of the comments or of the identity, or a listing
// GitHub refused, asked for no close, so it spends no try.
func TestOnlyARefusedCloseRequestIsCounted(t *testing.T) {
	refused := fmt.Errorf("GitHub answered 403")
	for name, c := range map[string]struct {
		fail    func(tr *fakeTracker)
		counted bool
	}{
		"comments unreadable": {func(tr *fakeTracker) { tr.commentsFailing = refused }, false},
		"identity unusable":   {func(tr *fakeTracker) { tr.commentsFailing = errors.New("GitHub did not name the token's user") }, false},
		"listing refused":     {func(tr *fakeTracker) { tr.failing = refused }, false},
		"close refused":       {func(tr *fakeTracker) { tr.closeFailing = refused }, true},
	} {
		t.Run(name, func(t *testing.T) {
			tr := &fakeTracker{}
			s := newIssues(tr)
			prefix := report.HistoryUnavailableKeyPrefix("joint")
			a := forgottenAlert()
			seen, sent, repeats := map[string]bool{}, map[string]time.Time{}, map[string]int{}
			s.records = repeats
			if err := deliver([]sender{s}, a, seen, sent, repeats, t0, time.Minute, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			forget(prefix, seen, sent, repeats)
			c.fail(tr)
			for n := range maxCloseTries - 1 {
				s := newIssues(tr)
				s.records = repeats
				if err := closeForgotten([]sender{s}, []string{prefix}, seen, sent, repeats); err == nil {
					t.Fatalf("firing %d: a failed close is not an error", n)
				}
			}
			want := 0
			if c.counted {
				want = maxCloseTries - 1
			}
			if got := repeats[closeTriesKey(a.Key)]; got != want || !seen[channelKey(issuesChannel, a.Key)] {
				t.Fatalf("tries=%d (want %d) seen=%v: only a refused close request is counted", got, want, seen)
			}
		})
	}
}

// The record of an issue left open stays while the issue is listed, or open
// on GitHub with its label off; it is pruned at the next listing once the
// issue is closed and a newer issue of its key exists.
func TestAnUnclosedRecordLeavesWithItsIssue(t *testing.T) {
	tr := &fakeTracker{}
	s := newIssues(tr)
	prefix := report.HistoryUnavailableKeyPrefix("joint")
	a := forgottenAlert()
	seen, sent, repeats := map[string]bool{}, map[string]time.Time{}, map[string]int{}
	s.records = repeats
	if err := deliver([]sender{s}, a, seen, sent, repeats, t0, time.Minute, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	forget(prefix, seen, sent, repeats)
	tr.closeFailing = fmt.Errorf("GitHub answered 422")
	for n := range maxCloseTries {
		s := newIssues(tr)
		s.records = repeats
		if err := closeForgotten([]sender{s}, []string{prefix}, seen, sent, repeats); err == nil {
			t.Fatalf("firing %d: a failed close is not an error", n)
		}
	}
	if repeats[unclosedKey(a.Key, 1)] != 1 {
		t.Fatalf("repeats=%v: the abandoned issue must be recorded", repeats)
	}
	// Still listed: the record stays through a listing.
	s = newIssues(tr)
	s.records = repeats
	other := haltAlert()
	if err := s.send(other, 0); err != nil {
		t.Fatal(err)
	}
	if repeats[unclosedKey(a.Key, 1)] != 1 {
		t.Fatalf("repeats=%v: the record of a listed issue must stay", repeats)
	}
	// The owner closes it by hand and takes its label off: it leaves the listing.
	tr.issues[0].open, tr.issues[0].labels = false, nil
	tr.issues = append(tr.issues, &fakeIssue{number: 3, body: issueMarker(a.Key), labels: []string{alertLabel}, open: true})
	s = newIssues(tr)
	s.records = repeats
	if err := s.send(other, 1); err != nil {
		t.Fatal(err)
	}
	if _, kept := repeats[unclosedKey(a.Key, 1)]; kept {
		t.Fatalf("repeats=%v: the record of an issue gone from the listing must be pruned", repeats)
	}
}

// An abandoned issue that is still open keeps its record while its alert
// label is off: when the label comes back, the alert's next incident opens its
// own issue and is not absorbed by the abandoned one (ADR 0019, "Alerting":
// once per incident).
func TestALabelRemovedAndRestoredStillLetsTheNextIncidentOpen(t *testing.T) {
	tr := &fakeTracker{}
	s := newIssues(tr)
	prefix := report.HistoryUnavailableKeyPrefix("joint")
	a := forgottenAlert()
	seen, sent, repeats := map[string]bool{}, map[string]time.Time{}, map[string]int{}
	s.records = repeats
	if err := deliver([]sender{s}, a, seen, sent, repeats, t0, time.Minute, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	forget(prefix, seen, sent, repeats)
	tr.closeFailing = fmt.Errorf("GitHub answered 422")
	for range maxCloseTries {
		s := newIssues(tr)
		s.records = repeats
		_ = closeForgotten([]sender{s}, []string{prefix}, seen, sent, repeats)
	}
	tr.closeFailing = nil
	if repeats[unclosedKey(a.Key, 1)] != 1 {
		t.Fatalf("repeats=%v: the abandoned issue must be recorded", repeats)
	}
	// The owner takes the alert label off the abandoned issue, which stays open.
	tr.issues[0].labels = nil
	s = newIssues(tr)
	s.records = repeats
	if err := s.send(haltAlert(), 0); err != nil {
		t.Fatal(err)
	}
	// The label goes back on, and the alert's next incident arrives.
	tr.issues[0].labels = []string{alertLabel}
	forget(prefix, seen, sent, repeats)
	s = newIssues(tr)
	s.records = repeats
	if err := deliver([]sender{s}, a, seen, sent, repeats, t0.Add(20*time.Hour), time.Minute, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	// Issue 2 is the other alert's; the next incident of a is issue 3.
	if len(tr.issues) != 3 || !tr.issues[2].open {
		t.Fatalf("%d issues, want a third, open issue for the next incident", len(tr.issues))
	}
}

// A firing starts its closed checks after the last record the one before
// checked, so a closed record behind records that keep answering open is
// reached within two firings (ADR 0019, "Alerting": bounded requests, no
// record left unchecked).
func TestTheClosedChecksRotateSoNoRecordStarves(t *testing.T) {
	key := forgottenAlert().Key
	tr, repeats := &fakeTracker{}, map[string]int{}
	for i := 1; i <= maxClosedChecks; i++ {
		tr.issues = append(tr.issues, &fakeIssue{number: i, body: issueMarker(key), open: true})
		repeats[unclosedKey(key, i)] = 1
	}
	closed := maxClosedChecks + 1
	tr.issues = append(tr.issues, &fakeIssue{number: closed, body: issueMarker(key), open: false})
	repeats[unclosedKey(key, closed)] = 1
	tr.issues = append(tr.issues, &fakeIssue{number: closed + 1, body: issueMarker(key), labels: []string{alertLabel}, open: true})
	pruned := 0
	for firing := 1; firing <= 2 && pruned == 0; firing++ {
		s := newIssues(tr)
		s.records = repeats
		if err := s.send(haltAlert(), 1); err != nil {
			t.Fatal(err)
		}
		if _, kept := repeats[unclosedKey(key, closed)]; !kept {
			pruned = firing
		}
	}
	if pruned == 0 {
		t.Fatalf("the closed record was not checked within two firings: %v", repeats)
	}
}

// The closed walk wraps: a cursor naming the last record in sort order hands
// the next firing's checks to the first record (ADR 0019, "Alerting": no
// record left unchecked). Records 1-5 are closed, so each check deletes its
// record; records 6 and 7 stay open and are never reached in this firing.
func TestTheClosedWalkWrapsPastTheLastRecord(t *testing.T) {
	key := forgottenAlert().Key
	tr, repeats := &fakeTracker{}, map[string]int{}
	for i := 1; i <= 7; i++ {
		tr.issues = append(tr.issues, &fakeIssue{number: i, body: issueMarker(key), open: i > 5})
		repeats[unclosedKey(key, i)] = 1
	}
	tr.issues = append(tr.issues, &fakeIssue{number: 8, body: issueMarker(key), labels: []string{alertLabel}, open: true})
	repeats[closedCursorKey(unclosedKey(key, 7))] = 1
	s := newIssues(tr)
	s.records = repeats
	if err := s.send(haltAlert(), 1); err != nil {
		t.Fatal(err)
	}
	if tr.closedCalls != maxClosedChecks {
		t.Fatalf("closed checks=%d, want %d: the walk must wrap to the first record", tr.closedCalls, maxClosedChecks)
	}
	for i := 1; i <= 7; i++ {
		_, kept := repeats[unclosedKey(key, i)]
		if kept != (i > 5) {
			t.Fatalf("record %d kept=%v, want %v: repeats=%v", i, kept, i > 5, repeats)
		}
	}
}

// A cursor naming a record that is no longer in the state (removed, or
// pruned) hands the walk to the next record in sort order, not to the first
// (ADR 0019, "Alerting": no record left unchecked). Record 3 is absent; the
// cursor names it. Records 4-8 are closed and are checked and deleted; 1 and 2
// are open and must be left alone.
func TestTheClosedWalkStartsAtTheNextKeyWhenTheCursorIsGone(t *testing.T) {
	key := forgottenAlert().Key
	tr, repeats := &fakeTracker{}, map[string]int{}
	for i := 1; i <= 8; i++ {
		tr.issues = append(tr.issues, &fakeIssue{number: i, body: issueMarker(key), open: i <= 3})
		if i != 3 {
			repeats[unclosedKey(key, i)] = 1
		}
	}
	tr.issues = append(tr.issues, &fakeIssue{number: 9, body: issueMarker(key), labels: []string{alertLabel}, open: true})
	repeats[closedCursorKey(unclosedKey(key, 3))] = 1
	s := newIssues(tr)
	s.records = repeats
	if err := s.send(haltAlert(), 1); err != nil {
		t.Fatal(err)
	}
	if tr.closedCalls != maxClosedChecks {
		t.Fatalf("closed checks=%d, want %d", tr.closedCalls, maxClosedChecks)
	}
	for i := 1; i <= 8; i++ {
		if i == 3 {
			continue
		}
		_, kept := repeats[unclosedKey(key, i)]
		if kept != (i <= 3) {
			t.Fatalf("record %d kept=%v, want %v: repeats=%v", i, kept, i <= 3, repeats)
		}
	}
}

// unlistedRecords is a tracker with n abandoned issues of one alert key, each
// closed with its label off (so unlisted), a record for each, and a newer open
// issue of the key that replaced them.
func unlistedRecords(n int) (tr *fakeTracker, repeats map[string]int) {
	key := forgottenAlert().Key
	tr, repeats = &fakeTracker{}, map[string]int{}
	for i := 1; i <= n; i++ {
		tr.issues = append(tr.issues, &fakeIssue{number: i, body: issueMarker(key), open: false})
		repeats[unclosedKey(key, i)] = 1
	}
	tr.issues = append(tr.issues, &fakeIssue{number: n + 1, body: issueMarker(key), labels: []string{alertLabel}, open: true})
	return tr, repeats
}

// The checks of unlisted records are part of the delivery's request budget
// and are bounded: at most maxClosedChecks a firing, the rest at the next
// (ADR 0019, "Alerting": bounded requests).
func TestTheClosedChecksOfAFiringAreBoundedAndBudgeted(t *testing.T) {
	if maxDeliveryRequests != maxIssuePages+1+maxCommentPages+4+maxClosedChecks {
		t.Fatalf("maxDeliveryRequests=%d does not budget the %d closed checks", maxDeliveryRequests, maxClosedChecks)
	}
	n := maxClosedChecks + 3
	tr, repeats := unlistedRecords(n)
	s := newIssues(tr)
	s.records = repeats
	if err := s.send(haltAlert(), 0); err != nil {
		t.Fatal(err)
	}
	if tr.closedCalls != maxClosedChecks || unclosedCount(repeats) != n-maxClosedChecks {
		t.Fatalf("closed checks=%d records left=%d, want %d checks and %d left", tr.closedCalls, unclosedCount(repeats), maxClosedChecks, n-maxClosedChecks)
	}
	s = newIssues(tr)
	s.records = repeats
	if err := s.send(haltAlert(), 1); err != nil {
		t.Fatal(err)
	}
	if unclosedCount(repeats) != 0 {
		t.Fatalf("repeats=%v: the next firing must check the rest", repeats)
	}
}

// A closed issue the owner relabels, with no newer issue of its key, is still
// the key's newest: its record stays until a replacement exists, so it
// cannot absorb the next incident (ADR 0019, "Alerting": once per incident).
func TestAClosedRecordStaysUntilAReplacementExists(t *testing.T) {
	tr := &fakeTracker{}
	key := forgottenAlert().Key
	tr.issues = []*fakeIssue{{number: 1, body: issueMarker(key), open: false}}
	repeats := map[string]int{unclosedKey(key, 1): 1}
	s := newIssues(tr)
	s.records = repeats
	if err := s.send(haltAlert(), 0); err != nil {
		t.Fatal(err)
	}
	if repeats[unclosedKey(key, 1)] != 1 {
		t.Fatalf("repeats=%v: a closed issue with no replacement keeps its record", repeats)
	}
	tr.issues = append(tr.issues, &fakeIssue{number: 2, body: issueMarker(key), labels: []string{alertLabel}, open: true})
	s = newIssues(tr)
	s.records = repeats
	if err := s.send(haltAlert(), 1); err != nil {
		t.Fatal(err)
	}
	if _, kept := repeats[unclosedKey(key, 1)]; kept {
		t.Fatalf("repeats=%v: the record goes once a replacement exists", repeats)
	}
}

// A failed check keeps the record.
func TestAFailedClosedCheckKeepsTheRecord(t *testing.T) {
	tr, repeats := unlistedRecords(1)
	tr.closedFailing = errors.New("GitHub answered 403 Forbidden")
	s := newIssues(tr)
	s.records = repeats
	if err := s.send(haltAlert(), 0); err != nil {
		t.Fatal(err)
	}
	if tr.closedCalls != 1 || unclosedCount(repeats) != 1 {
		t.Fatalf("checks=%d records=%v: a failed check must keep the record", tr.closedCalls, repeats)
	}
}

// githubTracker.Closed reads the issue itself: closed and gone (404, 410)
// are closed; open is not; any other answer is an error.
func TestGitHubTrackerClosedReadsTheIssueState(t *testing.T) {
	for _, c := range []struct {
		name    string
		status  int
		body    string
		want    bool
		wantErr bool
	}{
		{"open", 200, `{"number":7,"state":"open"}`, false, false},
		{"closed", 200, `{"number":7,"state":"closed"}`, true, false},
		{"not found", 404, `{}`, true, false},
		{"gone", 410, `{}`, true, false},
		{"forbidden", 403, `{}`, false, true},
		{"server error", 500, `{}`, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var path string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path = r.URL.Path
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer server.Close()
			g := &githubTracker{api: server.URL, base: server.URL + "/repos/o/r", token: "t", client: server.Client()}
			got, err := g.Closed(context.Background(), 7)
			if path != "/repos/o/r/issues/7" || got != c.want || (err != nil) != c.wantErr {
				t.Fatalf("path=%q closed=%v err=%v, want closed=%v err=%v", path, got, err, c.want, c.wantErr)
			}
		})
	}
}

// unclosedCount is how many unclosed records (unclosedKey) the state holds,
// not counting the closed-cursor record.
func unclosedCount(repeats map[string]int) int {
	n := 0
	for k := range repeats {
		if strings.HasPrefix(k, channelKey(issuesChannel, "unclosed:")) {
			n++
		}
	}
	return n
}
