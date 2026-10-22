package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// endToEnd is a whole job: the fake GitHub, a stand-in dashboard serving the
// slice, a stand-in claude, and a real clone, wired by run.
type endToEnd struct {
	github   *fakeGitHub
	args     []string
	environ  []string
	client   *http.Client
	stateDir string
	record   string
}

func newEndToEnd(t *testing.T) *endToEnd {
	t.Helper()
	f, srv := newFakeGitHub(t)
	f.addIssue(41, alertLabel, "owner", time.Now().Add(time.Hour), alertBody)
	dash := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/journal-slice" {
			http.Error(w, "read-only dashboard", http.StatusMethodNotAllowed)
			return
		}
		now := time.Now().UTC()
		raised := ""
		if r.URL.Query().Get("alert") == alertKey {
			raised = ", and raised the alert"
		}
		_, _ = io.WriteString(w, "## boost.log: covers "+now.Add(-time.Hour).Format(time.RFC3339)+" to "+now.Add(2*time.Hour).Format(time.RFC3339)+raised+"\n\n## boost.log: journal-query refusals\n\nrefused DU1234567 order-size\n")
	}))
	t.Cleanup(dash.Close)
	ws, _, _ := gitWorld(t)
	// The gate user is nobody, a user that is not the test's own, as the
	// job requires; the stand-in sudo runs every command as the test's user
	// all the same, and refuses any signalling program, so the job's end of
	// the gate user's processes never reaches one (pkillGateUser refuses
	// nobody's UID on macOS, and the stand-in refuses pkill elsewhere).
	gate := standInGate(t)
	claude, record := fakeClaude(t, `{"type":"result","subtype":"success","is_error":false,"structured_output":{"kind":"data-problem","what_happened":"The bar arrived late.","rule":"ADR 0025","owner_action":"","fix_title":"","fix_summary":""}}`, 0)
	stateDir := t.TempDir()
	token := filepath.Join(stateDir, "token")
	if err := os.WriteFile(token, []byte("ghp_testtokentesttokentesttoken00\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gh, _ := url.Parse(srv.URL)
	du, _ := url.Parse(dash.URL)
	e := &endToEnd{
		github: f, client: srv.Client(), stateDir: stateDir, record: record,
		environ: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + stateDir, "TI_GITHUB_API_URL=" + srv.URL, "TI_GITHUB_API_ALLOW_HOST=" + gh.Host, "TI_TRIAGE_SLICE_ALLOW_HOST=" + du.Host,
			"TI_TRIAGE_SUDO=" + gate.sudo, "TI_TRIAGE_SANDBOX_EXEC=" + gate.sandboxExec,
			"TI_TRIAGE_NETSTAT=" + script(t, t.TempDir(), "netstat", "echo 'tcp4       0      0  127.0.0.1.4002         *.*                    LISTEN'\n")},
		args: []string{"-alerts-repo", "owner/repo", "-repo-dir", ws.repoDir, "-state-dir", stateDir, "-token-file", token,
			"-slice-url", dash.URL + "/api/journal-slice", "-claude", claude, "-gate-dir", gate.root, "-gate-user", "nobody"},
	}
	// The job's state starts before the issue, as a job switched on earlier.
	if err := saveState(filepath.Join(stateDir, "state.json"), &state{Since: time.Now().Add(-time.Hour), Day: "2026-10-22", Calls: new(int), Issues: map[string]issueRecord{}}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *endToEnd) run(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	var log strings.Builder
	err := run(append(append([]string{}, e.args...), extra...), e.environ, &log, e.client)
	return log.String(), err
}

// TestOnePollEndToEndCommentsOnceAndTheSecondAddsNothing: run, wired as
// launchd runs it, analyses the new alert issue into one comment and makes
// one session; the next run calls no session and writes nothing.
func TestOnePollEndToEndCommentsOnceAndTheSecondAddsNothing(t *testing.T) {
	e := newEndToEnd(t)
	if log, err := e.run(t); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if c := e.github.comments[41]; len(c) != 1 || !strings.Contains(c[0]["body"].(string), "The bar arrived late.") {
		t.Fatalf("comments %v", c)
	}
	stdin := readRecord(t, e.record, "stdin")
	if strings.Contains(stdin, "DU1234567") || !strings.Contains(stdin, "order-size") {
		t.Errorf("the session was handed %q", stdin)
	}
	if err := os.Remove(e.record + ".stdin"); err != nil {
		t.Fatal(err)
	}
	if log, err := e.run(t); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if len(e.github.comments[41]) != 1 {
		t.Fatalf("a second run wrote %v", e.github.comments[41])
	}
	if _, err := os.Stat(e.record + ".stdin"); !os.IsNotExist(err) {
		t.Fatal("a second run started a session")
	}
}

// TestTheOffFileStopsTheRunBeforeAnythingIsRead: the off switch works even
// with every other setting wrong.
func TestTheOffFileStopsTheRunBeforeAnythingIsRead(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "OFF"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	if err := run([]string{"-state-dir", state, "-token-file", "/nonexistent"}, nil, &log, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "off") {
		t.Errorf("log %q", log.String())
	}
}

func TestRunRefusesBadSettings(t *testing.T) {
	e := newEndToEnd(t)
	worldReadable := filepath.Join(e.stateDir, "open-token")
	if err := os.WriteFile(worldReadable, []byte("ghp_x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(worldReadable, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no state dir":      {[]string{"-state-dir", ""}, "-state-dir"},
		"no repo":           {[]string{"-alerts-repo", ""}, "-alerts-repo"},
		"no gate dir":       {[]string{"-gate-dir", ""}, "-gate-dir"},
		"no repo dir":       {[]string{"-repo-dir", ""}, "-repo-dir"},
		"no slice":          {[]string{"-slice-url", ""}, "-slice-url"},
		"open token":        {[]string{"-token-file", worldReadable}, "only you"},
		"missing token":     {[]string{"-token-file", filepath.Join(e.stateDir, "absent")}, "token"},
		"negative cap":      {[]string{"-daily-cap", "-1"}, "-daily-cap"},
		"no turns":          {[]string{"-max-turns", "0"}, "-max-turns"},
		"empty check":       {[]string{"-check-cmd", " "}, "-check-cmd"},
		"slice not tailnet": {[]string{"-slice-url", "https://example.com/api/journal-slice"}, "-slice-url"},
		"unknown flag":      {[]string{"-merge"}, "merge"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.run(t, c.args...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to name %q", err, c.want)
			}
		})
	}
}

// TestASecondRunWhileOneHoldsTheLockDoesNothing: two polls never run at
// once, so no issue can be invoked twice.
func TestASecondRunWhileOneHoldsTheLockDoesNothing(t *testing.T) {
	e := newEndToEnd(t)
	release, err := lock(e.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	log, err := e.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log, "another poll is running") || len(e.github.comments[41]) != 0 {
		t.Fatalf("log %q, comments %v", log, e.github.comments[41])
	}
}

// TestTheLaunchdJobPollsEveryFiveMinutesAndHoldsNoSecret: the job's
// definition runs one poll per firing, names the settings by file, and
// carries no token itself.
func TestTheLaunchdJobPollsEveryFiveMinutesAndHoldsNoSecret(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/mac-mini/triage/com.trendinvesting.triage.plist")
	if err != nil {
		t.Fatal(err)
	}
	plist := string(raw)
	for _, want := range []string{"<key>StartInterval</key>\n\t<integer>300</integer>", "-token-file", "-state-dir", "-slice-url", "<string>ti-triage</string>", "-alerts-repo", "-code-repo", "-gate-dir", "-gate-user"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q", want)
		}
	}
	if regexp.MustCompile(`gh[opsu]_|github_pat_|TOKEN</key>|sk-ant-`).MatchString(plist) {
		t.Error("the plist holds a token")
	}
}

// TestOneRepositoryNamesBoth: with only the alerts repository or only the
// code repository given, the other is the same one.
func TestOneRepositoryNamesBoth(t *testing.T) {
	for _, c := range []struct{ alerts, code, wantAlerts, wantCode string }{
		{"o/alerts", "", "o/alerts", "o/alerts"},
		{"", "o/code", "o/code", "o/code"},
		{"o/alerts", "o/code", "o/alerts", "o/code"},
	} {
		a, k := repositories(c.alerts, c.code)
		if a != c.wantAlerts || k != c.wantCode {
			t.Errorf("repositories(%q, %q) = %q, %q", c.alerts, c.code, a, k)
		}
	}
}
