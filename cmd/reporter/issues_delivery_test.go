package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
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
// The delivery is unbounded in time here: what is pinned is the cut, not the
// clock (the send timeout has its own test below).
func TestABodyOverGitHubsLimitIsCutNotRefused(t *testing.T) {
	gh := &fakeGitHub{}
	url := fakeGitHubServer(t, gh)
	s := githubChannel(t, url, 0)
	s.evidence = func() string { return strings.Repeat("evidence line\n", 10000) }
	a := haltAlert()
	a.Body = "The engine halted: risk limit breached. " + strings.Repeat("é", 30000)
	if err := s.send(a, 0); err != nil {
		t.Fatal(err)
	}
	if err := githubChannel(t, url, 0).send(a, 1); err != nil {
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
		fmt.Sprintf("%v", netLiq*10), fmt.Sprintf("%g", ref*1000),
	}
	entries := haltedEntries(t)
	snap := event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: t0, Currency: "USD", NetLiquidation: netLiq,
		Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: held}}}
	refused := event.OrderRefusedPayload{AccountID: "joint", PlanID: "p", Tag: "targets:0", ClientOrderID: "tw-a", InstrumentID: "SPY",
		Side: event.OrderSideBuy, Quantity: qty, LimitPrice: limit, Reason: event.RefusedOrderSize,
		Detail: fmt.Sprintf("sell-40 qty-12 at 12.35, 7 left, tw-17 in 5m; short %d shares at %s (qty:%d); %sUSD %s\u20ac \u00a3%d %d-%d sell-%d #%d %v %g x%d %d\u00a0shares \u2212%d %d\u2014%d limit-%s",
			short, strconv.FormatFloat(limit, 'f', -1, 64), qty, strconv.FormatFloat(limit, 'f', -1, 64), strconv.FormatFloat(ref, 'f', -1, 64),
			held, short, qty, qty, held, netLiq*10, ref*1000, held, short, qty, held, short, strconv.FormatFloat(netLiq, 'f', -1, 64)),
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

var (
	digitRun       = regexp.MustCompile(`\d+`)
	issueAllowList = regexp.MustCompile(`(?:19|20)\d\d-\d\d-\d\d(?:T\d\d:\d\d(?::\d\d)?(?:Z|[+-]\d\d:\d\d)?)?|\b\d\d:\d\d(?::\d\d)?Z?|ADR \d{4}`)
)

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
		hidden := regexp.MustCompile(`<!--.*?-->`)
		for _, i := range issues {
			// No digit of any length is left but the allow-list's: a date or
			// a time, and an ADR's number.
			rest := issueAllowList.ReplaceAllString(hidden.ReplaceAllString(i.title+"\n"+i.body+"\n"+strings.Join(i.comments, "\n"), ""), "")
			for _, f := range figures {
				for _, part := range digitRun.FindAllString(f, -1) {
					if strings.Contains(rest, part) {
						t.Errorf("round %d: issue %q holds %s of the figure %s:\n%s", round, i.title, part, f, rest)
					}
				}
			}
			if regexp.MustCompile(`\p{Nd}`).MatchString(rest) {
				t.Errorf("round %d: issue %q holds a digit outside the allow-list:\n%s", round, i.title, rest)
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
}

// The token goes only to GitHub's API over https, or to the one host the
// operator names (ADR 0019, "Alerting"; docs/live-trading/plan.md section 7).
func TestTheTokenGoesOnlyToGitHubOverHTTPS(t *testing.T) {
	base := map[string]string{"TI_GITHUB_ISSUES_TOKEN": issuesToken, "TI_GITHUB_ISSUES_REPO": "owner/repo"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for name, c := range map[string]struct {
		env map[string]string
		ok  bool
	}{
		"default":                  {with(), true},
		"github over https":        {with("TI_GITHUB_API_URL", "https://api.github.com"), true},
		"github over http":         {with("TI_GITHUB_API_URL", "http://api.github.com"), false},
		"other host unlisted":      {with("TI_GITHUB_API_URL", "https://evil.example"), false},
		"other host listed":        {with("TI_GITHUB_API_URL", "https://ghe.example", "TI_GITHUB_API_ALLOW_HOST", "ghe.example"), true},
		"other host, another name": {with("TI_GITHUB_API_URL", "https://evil.example", "TI_GITHUB_API_ALLOW_HOST", "ghe.example"), false},
		"listed host over http":    {with("TI_GITHUB_API_URL", "http://ghe.example", "TI_GITHUB_API_ALLOW_HOST", "ghe.example"), false},
		"loopback http listed":     {with("TI_GITHUB_API_URL", "http://127.0.0.1:8080", "TI_GITHUB_API_ALLOW_HOST", "127.0.0.1:8080"), true},
		"loopback http unlisted":   {with("TI_GITHUB_API_URL", "http://127.0.0.1:8080"), false},
		"userinfo trick":           {with("TI_GITHUB_API_URL", "https://api.github.com@evil.example"), false},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := githubIssuesFrom(env(c.env), http.DefaultClient)
			if (err == nil) != c.ok {
				t.Fatalf("accepted=%v, want %v (%v)", err == nil, c.ok, err)
			}
			if err != nil {
				for _, leak := range []string{"evil.example", "ghe.example", "127.0.0.1", issuesToken} {
					if strings.Contains(err.Error(), leak) {
						t.Fatalf("the message names %q: %v", leak, err)
					}
				}
			}
		})
	}
}

// An answer larger than the channel reads is an error, never a prefix handed
// to the parser.
func TestAnAnswerOverTheReadLimitIsAnError(t *testing.T) {
	gh := &fakeGitHub{}
	a := haltAlert()
	gh.issues = append(gh.issues, &fakeIssue{number: 1, body: strings.Repeat("x", 4096) + "\n\n" + issueMarker(a.Key), labels: []string{"alert"}, open: true})
	url := fakeGitHubServer(t, gh)
	s := githubChannel(t, url, 0)
	s.tracker.(*githubTracker).maxAnswer = 1024
	err := s.send(a, 0)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("an oversized answer was not refused as such: %v", err)
	}
	if issues := gh.snapshot(); len(issues) != 1 || commentsOf(issues) != 0 {
		t.Fatalf("an oversized answer wrote: %d issues", len(issues))
	}
}

// A journal too large to hold in memory is not read for the issue's evidence,
// and the issue's body says the evidence is withheld.
func TestAJournalOverTheEvidenceLimitIsNotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxEvidenceJournal + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	const note = "evidence withheld: journal over the limit"
	if got := issueEvidence(path, report.NewRedactor(nil)); got != note {
		t.Fatalf("evidence of an oversized journal: %q, want the one-line note %q", got, note)
	}
	gh := &fakeGitHub{}
	s := githubChannel(t, fakeGitHubServer(t, gh), time.Second)
	withIssueEvidence([]sender{s}, path)
	if err := s.send(haltAlert(), 0); err != nil {
		t.Fatal(err)
	}
	if issues := gh.snapshot(); len(issues) != 1 || !strings.Contains(issues[0].body, note) {
		t.Fatalf("the issue does not say its evidence is withheld: %+v", issues)
	}
}

// One issue's comments page over the read limit fails that one delivery. It
// does not trip the channel's latch, so a second alert in the same firing is
// still delivered (only the listing, the identity and transport failures are
// remembered for the firing).
func TestAnOversizedCommentsPageFailsOnlyThatDelivery(t *testing.T) {
	gh := &fakeGitHub{}
	url := fakeGitHubServer(t, gh)
	a, b := haltAlert(), haltAlert()
	b.Key = "reconciliation:x@joint"
	for _, x := range []report.Alert{a, b} {
		if err := githubChannel(t, url, time.Second).send(x, 0); err != nil {
			t.Fatal(err)
		}
	}
	gh.mu.Lock()
	gh.issues[0].comments = []string{strings.Repeat("x", 20000)}
	gh.issues[0].strangers = map[int]bool{0: true}
	gh.mu.Unlock()
	s := githubChannel(t, url, time.Second)
	s.tracker.(*githubTracker).maxAnswer = 8192
	err := s.send(a, 1)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("the oversized comments page was not refused as such: %v", err)
	}
	if err := s.send(b, 1); err != nil {
		t.Fatalf("a second alert in the firing failed on the first one's page: %v", err)
	}
	if issues := gh.snapshot(); len(issues[1].comments) != 1 {
		t.Fatalf("the second alert's repeat was not delivered: %d comments", len(issues[1].comments))
	}
}

// The limit is the largest legitimate page: 100 items, each of up to
// maxIssueText characters, every one escaped at the widest the JSON encoding
// goes, plus the item's own fields.
func TestTheReadLimitHoldsAPageOfEscapedBodies(t *testing.T) {
	page := make([]map[string]string, 100)
	for i := range page {
		page[i] = map[string]string{"body": strings.Repeat("\x01", maxIssueText)}
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(raw)) > maxAnswerBytes {
		t.Fatalf("a legitimate page of %d bytes is over the read limit %d", len(raw), int64(maxAnswerBytes))
	}
}

// The token is sent only to the address that was checked: a redirect is
// refused, never followed, so it cannot carry the token to another address.
func TestARedirectIsNeverFollowed(t *testing.T) {
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(other.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	if err := githubChannel(t, redirector.URL, time.Second).send(haltAlert(), 0); err == nil {
		t.Fatal("a redirected answer counted as delivered")
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("the redirect was followed %d times", n)
	}
}

// A record's line in the evidence is cut to a bounded length, at a whole
// character.
func TestAnEvidenceLineIsBounded(t *testing.T) {
	line := strings.Repeat("é", maxEvidenceLine)
	got := boundedLine(line)
	if len(got) > maxEvidenceLine+len(evidenceLineCut) || !utf8.ValidString(got) || !strings.HasSuffix(got, evidenceLineCut) {
		t.Fatalf("line of %d bytes", len(got))
	}
	if short := "short"; boundedLine(short) != short {
		t.Fatal("a short line was changed")
	}
}
