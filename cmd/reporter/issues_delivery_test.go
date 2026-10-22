package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// The github-issues channel against GitHub's REST API, in memory: what one
// delivery may cost, what it does when it is sent again, and what a public
// issue may hold (ADR 0019, "Alerting"; docs/live-trading/plan.md section 7;
// docs/manual/alerts-and-dashboard.md, "Alerts as GitHub Issues").

// githubChannel is the channel as the reporter builds it, against the fake
// at url, with each request bounded by timeout. A new one is a new firing.
func githubChannel(t *testing.T, url string, timeout time.Duration) *issuesSender {
	t.Helper()
	s, err := githubIssuesFrom(env(issuesEnv(url)), &http.Client{Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return s.(*issuesSender)
}

func fakeGitHubServer(t *testing.T, gh *fakeGitHub) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(gh.handler))
	t.Cleanup(server.Close)
	return server.URL
}

func commentsOf(issues []fakeIssue) int {
	n := 0
	for _, i := range issues {
		n += len(i.comments)
	}
	return n
}

// A delivery sent again, in a later firing, because its answer was lost or
// its record was not saved, writes nothing more: the first delivery is the
// issue itself, never a "repeat 0" comment, and a repeat is one comment.
func TestADeliverySentAgainWritesNothingMore(t *testing.T) {
	gh := &fakeGitHub{}
	url := fakeGitHubServer(t, gh)
	a := haltAlert()
	for range 2 {
		if err := githubChannel(t, url, time.Second).send(a, 0); err != nil {
			t.Fatal(err)
		}
	}
	if issues := gh.snapshot(); len(issues) != 1 || commentsOf(issues) != 0 {
		t.Fatalf("the first delivery sent twice made %d issues and %d comments, want 1 and 0", len(issues), commentsOf(issues))
	}
	for range 2 {
		if err := githubChannel(t, url, time.Second).send(a, 1); err != nil {
			t.Fatal(err)
		}
	}
	if issues := gh.snapshot(); len(issues) != 1 || commentsOf(issues) != 1 {
		t.Fatalf("repeat 1 sent twice made %d issues and %d comments, want 1 and 1", len(issues), commentsOf(issues))
	}
}

// An issue the owner closed by hand is neither reopened nor replaced by a
// later repeat of the same incident; once the alert resolves and comes back,
// it is a new incident and a new issue.
func TestAHandClosedIssueIsNotReopenedOrDuplicated(t *testing.T) {
	gh := &fakeGitHub{}
	url := fakeGitHubServer(t, gh)
	a := haltAlert()
	if err := githubChannel(t, url, time.Second).send(a, 0); err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	gh.issues[0].open = false
	gh.mu.Unlock()
	for repeat := 1; repeat <= 2; repeat++ {
		if err := githubChannel(t, url, time.Second).send(a, repeat); err != nil {
			t.Fatal(err)
		}
	}
	issues := gh.snapshot()
	if len(issues) != 1 || issues[0].open || commentsOf(issues) != 0 {
		t.Fatalf("after a hand close: %d issues, first open=%v, %d comments; want the one closed issue untouched", len(issues), issues[0].open, commentsOf(issues))
	}
	s := githubChannel(t, url, time.Second)
	if err := s.resolve(a.Key); err != nil {
		t.Fatal(err)
	}
	if err := githubChannel(t, url, time.Second).send(a, 0); err != nil {
		t.Fatal(err)
	}
	if issues := gh.snapshot(); len(issues) != 2 || !issues[1].open {
		t.Fatalf("a recurrence after the alert resolved must open a new issue, got %d issues", len(issues))
	}
}

// A close whose comment was posted but whose state change failed posts the
// comment once: the next try only closes.
func TestAPartialCloseDoesNotPostItsCommentAgain(t *testing.T) {
	gh := &fakeGitHub{}
	url := fakeGitHubServer(t, gh)
	a := haltAlert()
	if err := githubChannel(t, url, time.Second).send(a, 0); err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	gh.patchFailures = 1
	gh.mu.Unlock()
	if err := githubChannel(t, url, time.Second).resolve(a.Key); err == nil {
		t.Fatal("a refused close must fail the resolve")
	}
	if err := githubChannel(t, url, time.Second).resolve(a.Key); err != nil {
		t.Fatal(err)
	}
	issues := gh.snapshot()
	if issues[0].open || len(issues[0].comments) != 1 {
		t.Fatalf("open=%v with %d closing comments, want closed with 1", issues[0].open, len(issues[0].comments))
	}
}

// A listing GitHub cuts short is an error, never a complete inventory: an
// alert beyond it would get a second issue.
func TestATruncatedListingIsAnError(t *testing.T) {
	gh := &fakeGitHub{}
	a := haltAlert()
	gh.issues = append(gh.issues, &fakeIssue{number: 1, body: "x\n\n" + issueMarker(a.Key), labels: []string{"alert"}, open: true})
	for n := 2; n <= 1200; n++ {
		gh.issues = append(gh.issues, &fakeIssue{number: n, body: "x\n\n" + issueMarker(fmt.Sprintf("other-%d", n)), labels: []string{"alert"}, open: true})
	}
	url := fakeGitHubServer(t, gh)
	err := githubChannel(t, url, 5*time.Second).send(a, 1)
	if err == nil {
		t.Fatal("an incomplete listing was taken as complete")
	}
	if issues := gh.snapshot(); len(issues) != 1200 || commentsOf(issues) != 0 {
		t.Fatalf("an incomplete listing wrote: %d issues, %d comments", len(issues), commentsOf(issues))
	}
}

// A 2xx answer that names no issue is not a delivery.
func TestAnAnswerWithNoIssueNumberIsNotADelivery(t *testing.T) {
	gh := &fakeGitHub{createAnswer: "{}"}
	url := fakeGitHubServer(t, gh)
	if err := githubChannel(t, url, time.Second).send(haltAlert(), 0); err == nil {
		t.Fatal("an answer naming no issue counted as delivered")
	}
}

// A body or comment longer than GitHub takes is cut, keeping the alert's text
// and the hidden key line, and saying so: it is never refused every firing.
func TestABodyOverGitHubsLimitIsCutNotRefused(t *testing.T) {
	gh := &fakeGitHub{}
	url := fakeGitHubServer(t, gh)
	s := githubChannel(t, url, time.Second)
	s.evidence = func() string { return strings.Repeat("evidence line\n", 10000) }
	a := haltAlert()
	a.Body = "The engine halted: risk limit breached. " + strings.Repeat("é", 30000)
	if err := s.send(a, 0); err != nil {
		t.Fatal(err)
	}
	if err := githubChannel(t, url, time.Second).send(a, 1); err != nil {
		t.Fatal(err)
	}
	issues := gh.snapshot()
	if len(issues) != 1 || len(issues[0].comments) != 1 {
		t.Fatalf("%d issues", len(issues))
	}
	body := issues[0].body
	if n := utf8.RuneCountInString(body); n > 65536 || !strings.Contains(body, "risk limit breached") || !strings.HasSuffix(body, issueMarker(a.Key)) || !strings.Contains(body, "cut") {
		t.Fatalf("body of %d characters, marker kept %v", n, strings.HasSuffix(body, issueMarker(a.Key)))
	}
	if !utf8.ValidString(body) || !utf8.ValidString(issues[0].comments[0]) {
		t.Fatal("a cut split a character")
	}
}

// The send timeout bounds one whole delivery, every request of it together,
// not each request alone.
func TestTheSendTimeoutBoundsTheWholeDelivery(t *testing.T) {
	gh := &fakeGitHub{}
	url := fakeGitHubServer(t, gh)
	a := haltAlert()
	if err := githubChannel(t, url, time.Second).send(a, 0); err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	gh.delay = 250 * time.Millisecond
	gh.mu.Unlock()
	start := time.Now()
	err := githubChannel(t, url, 400*time.Millisecond).send(a, 1)
	if took := time.Since(start); err == nil || took > 450*time.Millisecond {
		t.Fatalf("a delivery of several 250ms requests under a 400ms timeout: err %v after %v", err, took)
	}
}

// A GitHub that hangs never holds back a page: PagerDuty and ntfy take every
// alert before GitHub is asked anything, and the failed listing is not tried
// again for the firing's later alerts.
func TestAHangingGitHubNeverHoldsBackAPage(t *testing.T) {
	gh := &fakeGitHub{hang: true}
	ghURL := fakeGitHubServer(t, gh)
	var pdTimes []time.Time
	pd := &pagerDuty{status: http.StatusAccepted, body: `{"status":"success"}`}
	pdServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pd.mu.Lock()
		pdTimes = append(pdTimes, time.Now())
		pd.mu.Unlock()
		pd.handler(w, r)
	}))
	defer pdServer.Close()
	ntfy := &ntfyRecorder{}
	ntfyServer := httptest.NewServer(http.HandlerFunc(ntfy.handler))
	defer ntfyServer.Close()
	environment := issuesEnv(ghURL)
	environment["TI_PAGERDUTY_ROUTING_KEY"], environment["TI_PAGERDUTY_URL"] = "k", pdServer.URL
	environment["TI_NTFY_TOPIC"], environment["TI_NTFY_SERVER"] = "t", ntfyServer.URL
	dir := t.TempDir()
	args := []string{"-journal", leakyJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "github-issues,ntfy,pagerduty", "-send-timeout", "300ms"}
	start := time.Now()
	err := run(args, env(environment), func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, http.DefaultClient)
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "github-issues") {
		t.Fatalf("a hanging GitHub must fail the run naming the channel, got %v", err)
	}
	summaries, _ := pagerDutySummaries(pd)
	if len(summaries) < 2 {
		t.Fatalf("PagerDuty took %d alerts, want every one: %v", len(summaries), summaries)
	}
	gh.mu.Lock()
	ghTimes, ghCalls := append([]time.Time(nil), gh.times...), len(gh.methods)
	gh.mu.Unlock()
	pd.mu.Lock()
	defer pd.mu.Unlock()
	for _, at := range pdTimes {
		if len(ghTimes) > 0 && at.After(ghTimes[0]) {
			t.Fatalf("a page went out after GitHub was asked: page %v, GitHub %v", at, ghTimes[0])
		}
	}
	if ghCalls != 1 {
		t.Errorf("GitHub was asked %d times in the firing, want 1: a failed listing is remembered", ghCalls)
	}
	if took > 2*time.Second {
		t.Errorf("the firing took %v", took)
	}
}

// figureJournal is a realistic journal slice whose every figure (a balance,
// a quantity, a price, a holding, a risk figure) is a random six- or
// seven-digit number, returned with the figures.
func figureJournal(t *testing.T, dir string, rng *rand.Rand) (path string, figures []string) {
	t.Helper()
	whole := func() int64 { return 100000 + rng.Int63n(900000) }
	price := func() float64 { return float64(100000+rng.Intn(900000)) + float64(1+rng.Intn(98))/100 }
	netLiq, held, qty, limit, ref, short := price(), whole(), whole(), price(), price(), whole()
	figures = []string{
		strconv.FormatFloat(netLiq, 'f', -1, 64), strconv.FormatInt(held, 10), strconv.FormatInt(qty, 10),
		strconv.FormatFloat(limit, 'f', -1, 64), strconv.FormatFloat(ref, 'f', -1, 64), strconv.FormatInt(short, 10),
	}
	entries := haltedEntries(t)
	snap := event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: t0, Currency: "USD", NetLiquidation: netLiq,
		Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: held}}}
	refused := event.OrderRefusedPayload{AccountID: "joint", PlanID: "p", Tag: "targets:0", ClientOrderID: "tw-a", InstrumentID: "SPY",
		Side: event.OrderSideBuy, Quantity: qty, LimitPrice: limit, Reason: event.RefusedOrderSize,
		Detail:  fmt.Sprintf("short %d shares at %s (qty:%d)", short, strconv.FormatFloat(limit, 'f', -1, 64), qty),
		Figures: event.RiskFigures{NetLiquidation: netLiq, ReferencePrice: ref, ExposureBefore: 0.5, ExposureAfter: 0.98, OrderFraction: 0.48, OrdersInDecision: 1}}
	entries[1] = journal.Entry{Kind: journal.KindInput, Envelope: envelope(t, 2, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "snap", t0, snap)}
	entries[3] = journal.Entry{Kind: journal.KindDecision, Envelope: envelope(t, 4, event.OrderRefusedEventType, event.OrderRefusedSchemaVersion, "refuse", t0, refused)}
	var buf bytes.Buffer
	if err := journal.Write(&buf, journal.NewHeader("h", "v", t0.Add(-time.Hour), t0), entries); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "joint.journal")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, figures
}

var digitToken = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)

// No figure of the journal reaches a public issue, wherever it sits: in the
// alert's text, in a journal record at any depth, in a record's free text,
// or in the query output. Each case is a journal of fresh random figures,
// delivered by the whole run.
func TestNoFigureOfTheJournalReachesAnIssue(t *testing.T) {
	rng := rand.New(rand.NewSource(20261022))
	for round := range 8 {
		gh := &fakeGitHub{}
		url := fakeGitHubServer(t, gh)
		dir := t.TempDir()
		path, figures := figureJournal(t, dir, rng)
		args := []string{"-journal", path, "-state", filepath.Join(dir, "s.json"), "-channel", "github-issues"}
		if err := run(args, env(issuesEnv(url)), func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, http.DefaultClient); err != nil {
			t.Fatal(err)
		}
		issues := gh.snapshot()
		if len(issues) == 0 {
			t.Fatal("no issue was opened")
		}
		withheld := map[string]bool{}
		for _, f := range figures {
			withheld[strings.ReplaceAll(f, ",", "")] = true
			if whole, _, ok := strings.Cut(f, "."); ok {
				withheld[whole] = true
			}
		}
		hidden := regexp.MustCompile(`<!--.*?-->`)
		for _, i := range issues {
			all := hidden.ReplaceAllString(i.title+"\n"+i.body+"\n"+strings.Join(i.comments, "\n"), "")
			for _, tok := range digitToken.FindAllString(all, -1) {
				if withheld[strings.ReplaceAll(tok, ",", "")] {
					t.Errorf("round %d: issue %q holds the figure %s:\n%s", round, i.title, tok, all)
				}
			}
		}
	}
}

// The OnFailure hook may name github-issues, and then its issue gets every
// secret the run holds removed, the short ones too.
func TestTheHookOffersGitHubIssuesWithTheFullRedactor(t *testing.T) {
	var help bytes.Buffer
	_ = run([]string{"notify-unit-failed", "-h"}, env(nil), time.Now, &help, http.DefaultClient)
	if !strings.Contains(help.String(), "github-issues") {
		t.Errorf("the hook's -channel help does not list github-issues:\n%s", help.String())
	}
	gh := &fakeGitHub{}
	url := fakeGitHubServer(t, gh)
	systemd := newStandInSystemd(t)
	systemd.say("login failed using key zq7 for the unit")
	environment := issuesEnv(url)
	environment["TI_PAGERDUTY_ROUTING_KEY"] = "zq7"
	dir := t.TempDir()
	args := []string{"notify-unit-failed", "-state", filepath.Join(dir, "unit-failures.json"), "-channel", "github-issues", "-journalctl", systemd.journalctl(), "-systemctl", systemd.systemctl(), "ti-adapter-boost.service"}
	if err := run(args, env(environment), func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	issues := gh.snapshot()
	if len(issues) != 1 {
		t.Fatalf("%d issues, want the unit's page as one", len(issues))
	}
	if all := issues[0].title + issues[0].body; regexp.MustCompile(`\bzq7\b`).MatchString(all) {
		t.Errorf("the hook's issue holds a secret:\n%s", all)
	}
}

// The redactor the channel is built with removes JSON-escaped secrets from
// the records it shows.
func TestAnEscapedSecretNeverReachesAnIssue(t *testing.T) {
	gh := &fakeGitHub{}
	url := fakeGitHubServer(t, gh)
	s := githubChannel(t, url, time.Second)
	withRedaction([]sender{s}, []string{`tok&en"value`})
	e := event.Envelope{Type: event.OrderRefusedEventType, Sequence: 4, Payload: []byte(`{"detail":"saw tok\u0026en\"value here"}`)}
	s.evidence = func() string { return s.redactor.Record(e) }
	if err := s.send(haltAlert(), 0); err != nil {
		t.Fatal(err)
	}
	body := gh.snapshot()[0].body
	for _, leak := range []string{`tok&en"value`, `tok\u0026en\"value`, `tok&en\"value`, "tok\\u0026en"} {
		if strings.Contains(body, leak) {
			t.Errorf("the issue holds %q:\n%s", leak, body)
		}
	}
	_ = report.Redacted
}
