package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The server's units name the github-issues channel beside ntfy and Pushover,
// and an alerts repository not yet configured sets only that channel aside
// (ADR 0019, "Alerting"; docs/manual/alerts-and-dashboard.md, "Alerts as
// GitHub Issues").

// githubSecrets is the secrets file's lines for the channel, against the
// stand-in at url (ADR 0019, "Alerting": the token goes only to a host the
// secrets file names). The url must be the stand-in's http base URL, such as
// http://127.0.0.1:8099: the allowed host is derived by removing that literal
// prefix.
func githubSecrets(url string) string {
	return "TI_GITHUB_ISSUES_TOKEN=" + issuesToken + "\nTI_GITHUB_ISSUES_REPO=owner/repo\nTI_GITHUB_API_URL=" + url +
		"\nTI_GITHUB_API_ALLOW_HOST=" + strings.TrimPrefix(url, "http://") + "\n"
}

// channelsOf returns the -channel list of a unit's one ExecStart.
func channelsOf(t *testing.T, path string) []string {
	t.Helper()
	execStart := directive(t, path, "[Service]", "ExecStart")
	if len(execStart) != 1 {
		t.Fatalf("%s: ExecStart = %v", path, execStart)
	}
	words := strings.Fields(execStart[0])
	i := indexOf(words, "-channel")
	if i < 0 || i+1 >= len(words) {
		t.Fatalf("%s: ExecStart names no -channel", path)
	}
	return strings.Split(words[i+1], ",")
}

func TestBothUnitsNameTheGitHubIssuesChannelBesideNtfyAndPushover(t *testing.T) {
	for _, path := range []string{reporterUnitPath, hookUnitPath} {
		got := channelsOf(t, path)
		for _, want := range []string{"ntfy", "pushover", "github-issues"} {
			if indexOf(got, want) < 0 {
				t.Errorf("%s: -channel %v lacks %s", path, got, want)
			}
		}
		if indexOf(got, "pagerduty") >= 0 || indexOf(got, "stdout") >= 0 {
			t.Errorf("%s: -channel %v names a channel the server does not use", path, got)
		}
	}
}

// With the token empty, as the secrets example ships it, the hook's unit
// delivers on ntfy and Pushover, sets github-issues aside by name, and fails.
func TestTheHookWithAnEmptyGitHubTokenSetsOnlyThatChannelAside(t *testing.T) {
	reports, credentials := t.TempDir(), t.TempDir()
	line := strings.ReplaceAll(directive(t, hookUnitPath, "[Service]", "ExecStart")[0], "/var/lib/trend-investing/reports", reports)
	args := expandExecStart(line, map[string]string{"%i": "ti-engine@boost.service", "%d": credentials}, nil)
	var ntfy, pager countingServer
	ntfyServer := ntfy.serve(t, http.StatusOK, "")
	pagerServer := pager.serve(t, http.StatusOK, `{"status":1}`)
	secrets := "TI_NTFY_TOPIC=topic\nTI_NTFY_SERVER=" + ntfyServer.URL + "\nTI_PUSHOVER_APP_TOKEN=token\nTI_PUSHOVER_USER_KEY=user\nTI_PUSHOVER_URL=" + pagerServer.URL +
		"\nTI_GITHUB_ISSUES_TOKEN=\nTI_GITHUB_ISSUES_REPO=owner/repo\n"
	if err := os.WriteFile(filepath.Join(credentials, "secrets"), []byte(secrets), 0o600); err != nil {
		t.Fatal(err)
	}
	systemd := newStandInSystemd(t)
	args = append([]string{args[1]}, append([]string{"-journalctl", systemd.journalctl()}, args[2:]...)...)
	monitor := env(map[string]string{"MONITOR_UNIT": "ti-engine@boost.service", "MONITOR_INVOCATION_ID": firstRun})
	err := run(args, monitor, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "the github-issues channel needs TI_GITHUB_ISSUES_TOKEN") {
		t.Fatalf("the hook with an empty token returned %v, want the documented message", err)
	}
	if ntfy.count() != 1 || pager.count() != 1 {
		t.Errorf("ntfy took %d and Pushover %d pages, want 1 each while github-issues is set aside", ntfy.count(), pager.count())
	}
}

// The same for the reporter's own unit, with an empty repository.
func TestTheReporterWithAnEmptyGitHubRepositorySetsOnlyThatChannelAside(t *testing.T) {
	var ntfy, pager countingServer
	environment := map[string]string{
		"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": ntfy.serve(t, http.StatusOK, "").URL,
		"TI_PUSHOVER_APP_TOKEN": "a", "TI_PUSHOVER_USER_KEY": "u", "TI_PUSHOVER_URL": pager.serve(t, http.StatusOK, `{"status":1}`).URL,
		"TI_GITHUB_ISSUES_TOKEN": issuesToken, "TI_GITHUB_ISSUES_REPO": "",
	}
	dir := t.TempDir()
	args := []string{"-journal", leakyJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", strings.Join(channelsOf(t, reporterUnitPath), ",")}
	var out bytes.Buffer
	err := run(args, env(environment), func() time.Time { return t0.Add(time.Hour) }, &out, http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "the github-issues channel needs TI_GITHUB_ISSUES_REPO as owner/name") {
		t.Fatalf("the reporter with an empty repository returned %v, want the documented message", err)
	}
	if strings.Contains(out.String()+err.Error(), issuesToken) {
		t.Error("the token reached the output")
	}
	if ntfy.count() == 0 || pager.count() == 0 {
		t.Errorf("ntfy took %d and Pushover %d alerts, want both reached", ntfy.count(), pager.count())
	}
}
