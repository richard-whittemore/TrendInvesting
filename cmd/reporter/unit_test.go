package main

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The reporter's unit files and the settings they read, as provisioned.
const (
	reporterUnitPath  = "../../deploy/systemd/ti-reporter@.service"
	reporterTimerPath = "../../deploy/systemd/ti-reporter@.timer"
	runEnvExample     = "../../deploy/etc/engines/run.env.example"
	secretsExample    = "../../deploy/etc/secrets.env.example"
	provisionPath     = "../../deploy/provision.sh"
	// unlockedSecrets is where ti-unlock decrypts secrets.env after a boot,
	// on a root-only tmpfs: the units load it from there and from nowhere
	// else (ADR 0028; docs/manual/server.md, "Unlocking the
	// secrets after a boot").
	unlockedSecrets = "/run/trend-investing/secrets/secrets.env"
	unlockedDir     = "/run/trend-investing/secrets/"
)

// readAssignments reads KEY=VALUE lines, skipping comments and blanks.
func readAssignments(t *testing.T, path string) map[string]string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("%s: %q is not KEY=VALUE", path, line)
		}
		values[key] = value
	}
	return values
}

// unitDirective is one Key=Value line of a unit file and its section.
type unitDirective struct{ section, key, value string }

func unitDirectives(t *testing.T, path string) []unitDirective {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []unitDirective
	section := ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		out = append(out, unitDirective{section, key, value})
	}
	return out
}

var bracedVariable = regexp.MustCompile(`\$\{([A-Z_]+)\}`)

// expandExecStart expands an ExecStart line as systemd does for the forms the
// reporter's unit uses (systemd.service(5), "Command lines"): specifiers such
// as %i and %d, and ${NAME} within a word.
func expandExecStart(line string, specifiers, env map[string]string) []string {
	for spec, value := range specifiers {
		line = strings.ReplaceAll(line, spec, value)
	}
	var words []string
	for _, word := range strings.Fields(line) {
		words = append(words, bracedVariable.ReplaceAllStringFunc(word, func(v string) string {
			return env[bracedVariable.FindStringSubmatch(v)[1]]
		}))
	}
	return words
}

// countingServer answers every request with status and body and counts the
// requests by path.
type countingServer struct {
	mu    sync.Mutex
	paths []string
}

func (c *countingServer) serve(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		c.mu.Lock()
		c.paths = append(c.paths, r.URL.Path)
		c.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func (c *countingServer) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.paths)
}

// TestTheServiceFileRunsTheReporterOnTheEnginesLog: the unit, as systemd
// expands it for the Boost account, runs the reporter on the log the engine
// writes for the current run (the engine's state directory and TI_RUN from
// run.env, which systemd reads again at each start, so a fresh run's new log
// is followed), keeps its state file in the reports directory the unit may
// write, reads its secrets from the credential file systemd hands it, and
// delivers to both real channels and the daily summary. Run as the unit runs
// it, against a synthetic log, it succeeds and both channels are reached.
func TestTheServiceFileRunsTheReporterOnTheEnginesLog(t *testing.T) {
	env := map[string]string{}
	var execStart string
	readWrite := ""
	for _, d := range unitDirectives(t, reporterUnitPath) {
		if d.section != "[Service]" {
			continue
		}
		switch d.key {
		case "Environment":
			key, value, _ := strings.Cut(d.value, "=")
			env[key] = value
		case "ExecStart":
			execStart = d.value
		case "ReadWritePaths":
			readWrite = d.value
		case "EnvironmentFile":
			if strings.TrimPrefix(d.value, "-") != "/var/lib/trend-investing/engines/%i/run.env" {
				t.Errorf("EnvironmentFile=%s, want the engine's run.env, which names the current run", d.value)
			}
		case "LoadCredential":
			if d.value != "secrets:"+unlockedSecrets {
				t.Errorf("LoadCredential=%s, want secrets:%s, the copy ti-unlock decrypts", d.value, unlockedSecrets)
			}
		}
	}
	if env["TI_STATE_DIR"] != "/var/lib/trend-investing/engines/%i" || env["TI_REPORTS_DIR"] != "/var/lib/trend-investing/reports" {
		t.Fatalf("the unit's directories are %v", env)
	}
	if readWrite != env["TI_REPORTS_DIR"] {
		t.Errorf("ReadWritePaths=%s, want only the reports directory %s", readWrite, env["TI_REPORTS_DIR"])
	}
	for key, value := range readAssignments(t, runEnvExample) {
		env[key] = value
	}

	// The server's directories, moved to this test's.
	state, reports, credentials := t.TempDir(), t.TempDir(), t.TempDir()
	env["TI_STATE_DIR"], env["TI_REPORTS_DIR"] = state, reports
	args := expandExecStart(execStart, map[string]string{"%i": "boost", "%d": credentials}, env)
	if len(args) == 0 || args[0] != "/opt/trend-investing/bin/ti-reporter" {
		t.Fatalf("ExecStart runs %v, want /opt/trend-investing/bin/ti-reporter", args)
	}
	for flag, value := range map[string]string{
		"-journal":      state + "/" + env["TI_RUN"] + ".log",
		"-state":        reports + "/boost.reporter.json",
		"-secrets-file": credentials + "/secrets",
		"-channel":      "ntfy,pushover,github-issues",
		"-account":      "boost",
		"-clock-state":  reports + "/host-clock.json",
		"-cash-ledger":  "/var/lib/trend-investing/flex/boost/cash-movements.jsonl",
		"-run-env":      "/var/lib/trend-investing/engines/boost/run.env",
	} {
		if i := indexOf(args, flag); i < 0 || i+1 >= len(args) || args[i+1] != value {
			t.Errorf("ExecStart %v does not pass %s %s", args, flag, value)
		}
	}
	// The server's run.env is not on this machine: the same file in this
	// test's state directory, which a run has started.
	if i := indexOf(args, "-run-env"); i >= 0 && i+1 < len(args) {
		args[i+1] = filepath.Join(state, "run.env")
		writeFileT(t, args[i+1], "TI_RUN="+env["TI_RUN"]+"\n")
	}
	if i := indexOf(args, "-clock-check"); i < 0 || i+1 >= len(args) || args[i+1] != "/usr/local/libexec/trend-investing/clock-check.sh" {
		t.Errorf("ExecStart %v does not run the installed clock check", args)
	} else {
		// The server's script is not on this machine: a stand-in that passes.
		args[i+1] = newStandInCheck(t).path()
	}
	if indexOf(args, "-summary") < 0 {
		t.Errorf("ExecStart %v does not send the daily summary", args)
	}
	// The instance's engine and adapter are checked, and the reporter's own
	// failure page is resolved by a clean firing.
	for flag, value := range map[string]string{
		"-units":     "ti-engine@boost.service,ti-adapter-boost.service,ti-data.service",
		"-self-unit": "ti-reporter@boost.service",
	} {
		if i := indexOf(args, flag); i < 0 || i+1 >= len(args) || args[i+1] != value {
			t.Errorf("ExecStart %v does not pass %s %s", args, flag, value)
		}
	}
	// The server's systemctl is not on this machine: a stand-in with no unit failed.
	systemd := newStandInSystemd(t)
	systemd.fail()
	args = append(args, "-systemctl", systemd.systemctl(), "-journalctl", systemd.journalctl())

	if err := os.Rename(writeDurableLog(t, t.TempDir()), filepath.Join(state, env["TI_RUN"]+".log")); err != nil {
		t.Fatal(err)
	}
	var ntfy, pager countingServer
	ntfyServer := ntfy.serve(t, http.StatusOK, "")
	pagerServer := pager.serve(t, http.StatusOK, `{"status":1}`)
	gh := &fakeGitHub{}
	ghURL := fakeGitHubServer(t, gh)
	secrets := "TI_NTFY_TOPIC=topic\nTI_NTFY_SERVER=" + ntfyServer.URL + "\nTI_PUSHOVER_APP_TOKEN=token\nTI_PUSHOVER_USER_KEY=user\nTI_PUSHOVER_URL=" + pagerServer.URL + "\n" + githubSecrets(ghURL)
	if err := os.WriteFile(filepath.Join(credentials, "secrets"), []byte(secrets), 0o600); err != nil {
		t.Fatal(err)
	}
	// systemd passes no secret in the environment: only the credential file.
	noEnv := func(string) string { return "" }
	if err := run(args[1:], noEnv, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, http.DefaultClient); err != nil {
		t.Fatalf("the reporter as the unit runs it: %v", err)
	}
	if ntfy.count() == 0 || pager.count() == 0 {
		t.Errorf("ntfy took %d and Pushover %d alerts, want both reached", ntfy.count(), pager.count())
	}
	if len(gh.snapshot()) == 0 {
		t.Error("the alerts repository was opened no issue: the unit does not reach the github-issues channel")
	}
	if _, err := os.Stat(filepath.Join(reports, "boost.reporter.json")); err != nil {
		t.Errorf("no state file in the reports directory: %v", err)
	}
}

func indexOf(words []string, word string) int {
	for i, w := range words {
		if w == word {
			return i
		}
	}
	return -1
}

// TestTheSecretsFileSuppliesTheChannelSecrets: under systemd the secrets
// arrive as a credential file, not the environment, so -secrets-file reads
// KEY=VALUE lines from it; a value set in the environment is used first.
func TestTheSecretsFileSuppliesTheChannelSecrets(t *testing.T) {
	var ntfy countingServer
	server := ntfy.serve(t, http.StatusOK, "")
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets")
	if err := os.WriteFile(secrets, []byte("# a comment\n\nIBKR_ACCOUNT_BOOST=\nTI_NTFY_TOPIC= file-topic \nTI_NTFY_SERVER="+server.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return t0.Add(time.Hour) }
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-secrets-file", secrets}
	if err := run(args, env(nil), now, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatal(err)
	}
	if ntfy.count() == 0 || ntfy.paths[0] != "/file-topic" {
		t.Fatalf("ntfy paths = %v, want the file's topic", ntfy.paths)
	}
	args[3] = filepath.Join(dir, "s2.json")
	if err := run(args, env(map[string]string{"TI_NTFY_TOPIC": "env-topic"}), now, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatal(err)
	}
	if last := ntfy.paths[len(ntfy.paths)-1]; last != "/env-topic" {
		t.Errorf("with the environment set, ntfy path = %q, want /env-topic", last)
	}
}

// TestASecretsFileThatCannotBeReadIsReportedWithoutPrintingIt: a missing
// file or a line that is not KEY=VALUE is an error, and the error never
// carries the file's text, which may be a secret. With no channel left that
// can deliver, the run stops; a channel whose secrets are intact still
// delivers (TestAMalformedSecretsLineSetsAsideOnlyWhatItLeavesUnset).
func TestASecretsFileThatCannotBeReadIsReportedWithoutPrintingIt(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("TI_PAGERDUTY_URL=https://example.org\ns3cr3t-without-equals\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"missing": filepath.Join(dir, "none"), "malformed": bad} {
		args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-secrets-file", path}
		err := run(args, env(nil), time.Now, &bytes.Buffer{}, http.DefaultClient)
		if err == nil || !strings.Contains(err.Error(), "secrets file") || strings.Contains(err.Error(), "s3cr3t") || !strings.Contains(err.Error(), "no delivery channel") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// TestTheSecretsExampleNamesWhatTheReporterReads: the stub provisioned as
// /etc/trend-investing/secrets.env names every secret the reporter reads for
// the channels its unit delivers to, and no alerting name nothing reads.
func TestTheSecretsExampleNamesWhatTheReporterReads(t *testing.T) {
	asked := map[string]bool{}
	recording := func(k string) string {
		asked[k] = true
		if strings.HasSuffix(k, "_SERVER") || strings.HasSuffix(k, "_URL") {
			return "https://example.org"
		}
		if strings.HasSuffix(k, "_ALLOW_HOST") {
			return "example.org"
		}
		if strings.HasSuffix(k, "_REPO") {
			return "owner/repo"
		}
		return "x"
	}
	if _, faults, err := buildSenders("ntfy,pagerduty,pushover,github-issues", recording, http.DefaultClient, io.Discard); err != nil || len(faults) != 0 {
		t.Fatal(err, faults)
	}
	// The outside heartbeat's address, read when a firing ends; a stand-in
	// transport answers, so nothing leaves this machine.
	pingHeartbeat(recording, &http.Client{Transport: &relay{sent: map[string][]string{}}}, io.Discard)
	example := readAssignments(t, secretsExample)
	for _, need := range []string{"TI_NTFY_TOPIC", "TI_NTFY_SERVER", "TI_PAGERDUTY_ROUTING_KEY", "TI_PUSHOVER_APP_TOKEN", "TI_PUSHOVER_USER_KEY", "TI_GITHUB_ISSUES_TOKEN", "TI_GITHUB_ISSUES_REPO", "TI_HEARTBEAT_URL"} {
		if _, ok := example[need]; !ok {
			t.Errorf("secrets.env.example has no %s line", need)
		}
	}
	// Read by other programs: the account numbers, the data step's Massive
	// key, and the Flex reader's token and query ID (adapter/ibkr/flex.py,
	// whose own test checks it reads both lines).
	others := map[string]bool{"IBKR_ACCOUNT_BOOST": true, "IBKR_ACCOUNT_TRAD_IRA": true, "IBKR_ACCOUNT_ROTH_IRA": true, "MASSIVE_API_KEY": true,
		"TI_IBKR_FLEX_TOKEN": true, "TI_IBKR_FLEX_QUERY_ID": true}
	for key := range example {
		if !asked[key] && !others[key] {
			t.Errorf("secrets.env.example names %s, which nothing reads", key)
		}
	}
}

// onCalendar is a calendar that fires every N minutes of every hour of
// every day, New York time.
var onCalendar = regexp.MustCompile(`^\*:0?0/(\d+) America/New_York$`)

// TestTheTimerMakesTheRepeatAndAcknowledgementBoundsTrue: a Degraded,
// Halted or reconciliation alert repeats every -repeat-interval until it is
// acknowledged, every transition out of Normal alerts at once, and an order
// unacknowledged for -ack-within pages the owner (ADR 0019, "Alerting"); the
// reporter only checks when its timer fires. The timer fires every few
// minutes around the clock, seven days a week, so an alert that arises
// overnight or at a weekend (a reconciliation at the nightly Gateway
// restart's reconnect) pages then and repeats as the rule says, a repeat is
// never later than one period after it is due, an unacknowledged order is
// reported within one period of its bound, and the day's summary goes the
// evening its session closes. Its period divides the repeat interval, and a
// run is stopped before the next firing, which systemd would otherwise skip.
func TestTheTimerMakesTheRepeatAndAcknowledgementBoundsTrue(t *testing.T) {
	var calendars []string
	unit := ""
	for _, d := range unitDirectives(t, reporterTimerPath) {
		switch d.key {
		case "OnCalendar":
			calendars = append(calendars, d.value)
		case "Unit":
			unit = d.value
		}
	}
	if unit != "ti-reporter@%i.service" {
		t.Errorf("the timer starts %q, want ti-reporter@%%i.service", unit)
	}
	if len(calendars) != 1 {
		t.Fatalf("OnCalendar = %v, want one periodic calendar", calendars)
	}
	m := onCalendar.FindStringSubmatch(calendars[0])
	if m == nil {
		t.Fatalf("OnCalendar=%s does not run around the clock every day: want *:0/N America/New_York", calendars[0])
	}
	step := time.Duration(atoi(t, m[1])) * time.Minute
	if step <= 0 || step > defaultAckWithin/2 || defaultRepeatInterval%step != 0 {
		t.Errorf("the timer's period %s must divide the repeat interval %s and be at most half the acknowledgement bound %s", step, defaultRepeatInterval, defaultAckWithin)
	}
	timeout := time.Duration(0)
	for _, d := range unitDirectives(t, reporterUnitPath) {
		if d.key == "TimeoutStartSec" {
			v, err := time.ParseDuration(strings.TrimSuffix(d.value, "in"))
			if err != nil {
				t.Fatalf("TimeoutStartSec=%s: %v", d.value, err)
			}
			timeout = v
		}
	}
	if timeout <= 0 || timeout >= step {
		t.Errorf("TimeoutStartSec is %s, want positive and below the timer's period %s", timeout, step)
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestTheProvisionerInstallsAndEnablesTheReporter: provisioning installs the
// reporter, the dashboard and the two owner tools from the build beside
// deploy/ (the server has no Go), and enables the Boost account's reporter
// timer, the one account with an engine configuration.
func TestTheProvisionerInstallsAndEnablesTheReporter(t *testing.T) {
	raw, err := os.ReadFile(provisionPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"ti-reporter@boost.timer", "ti-reporter ti-dashboard operator journal-query"} {
		if !strings.Contains(text, want) {
			t.Errorf("deploy/provision.sh does not mention %q", want)
		}
	}
	if strings.Contains(text, "enable ti-reporter.timer") || strings.Contains(text, " ti-reporter.timer ti-dashboard") {
		t.Error("deploy/provision.sh still enables the old ti-reporter.timer")
	}
}

// TestTheProvisionerStartsWhatItEnablesAndStopsWhatItRemoves: `systemctl
// enable` only creates the links a boot follows, so on a server that is not
// rebooted the reporter's timer and the dashboard are started too (the
// reporter is the owner's one eye on the engine, ADR 0019, "Alerting"), and
// the dashboard is restarted so it runs the build just installed. The old
// single reporter is stopped and disabled before its files are removed:
// systemd keeps a running unit loaded after its file is gone.
func TestTheProvisionerStartsWhatItEnablesAndStopsWhatItRemoves(t *testing.T) {
	raw, err := os.ReadFile(provisionPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	at := func(s string) int {
		t.Helper()
		i := strings.Index(text, s)
		if i < 0 {
			t.Errorf("deploy/provision.sh does not run %q", s)
		}
		return i
	}
	at("systemctl enable --now ti-data.timer ti-reporter@boost.timer")
	enable, restart := at("systemctl enable ti-dashboard.service"), at("systemctl restart ti-dashboard.service")
	if enable >= 0 && restart >= 0 && restart < enable {
		t.Error("the dashboard is restarted before it is enabled")
	}
	// The timer is stopped when active (a server whose unit file is already gone but whose
	// unit is still loaded), disabled only when its file exists; neither may
	// trip set -e.
	active := at("if systemctl is-active --quiet ti-reporter.timer; then systemctl stop ti-reporter.timer; fi")
	// A oneshot still starting is "activating", which is-active does not
	// report as active: its stop is unconditional.
	if strings.Contains(text, "is-active --quiet ti-reporter.service") {
		t.Error("the old reporter service is stopped only when is-active: a oneshot in activating is not active")
	}
	at("systemctl stop ti-reporter.service || true")
	disable := at("systemctl disable ti-reporter.timer")
	remove := at(`rm -f "$unit_dir/ti-reporter.service"`)
	if active >= 0 && disable >= 0 && remove >= 0 && (remove < active || remove < disable) {
		t.Error("the old single reporter's files are removed before it is stopped and disabled")
	}
}

// TestTheServiceFileHasNoStartCheckThatFailsBeforeTheFirstRun: with no run
// started the reporter's own firing waits quietly (-run-env), so the unit
// carries no ExecStartPre that exits 1 for a missing run.env, which would
// fail the unit and page the owner on a freshly provisioned server.
func TestTheServiceFileHasNoStartCheckThatFailsBeforeTheFirstRun(t *testing.T) {
	for _, d := range unitDirectives(t, reporterUnitPath) {
		if d.key == "ExecStartPre" {
			t.Errorf("ExecStartPre=%s: a missing run.env must not fail the unit", d.value)
		}
	}
}
