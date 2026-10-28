package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Gateway's bounded login retry (docs/manual/server.md, "The Gateway is
// logged out"): IBKR disables a login after ten failed attempts in 24 hours, so
// at most Max refused logins in Window are allowed, counted durably before the
// child is stopped. The "IBC" here is a shell stand-in; nothing touches IBKR.

const refusal = "IBC: detected dialog entitled: Unrecognized Username or Password; event=Opened"

type rig struct {
	t     *testing.T
	dir   string
	state string
	now   time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	d := t.TempDir()
	return &rig{t: t, dir: d, state: filepath.Join(d, "login-failures"), now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
}

func (r *rig) cfg(script string) config {
	// Each launch leaves a mark, so a test can tell whether IBC was started.
	marked := "echo x >>" + filepath.Join(r.dir, "launches") + "\n" + script
	return config{
		Command:       []string{"sh", "-c", marked},
		StateFile:     r.state,
		Max:           3,
		Window:        24 * time.Hour,
		Pattern:       regexp.MustCompile(`Unrecognized Username or Password`),
		StopDeadline:  time.Second,
		DrainDeadline: time.Second,
		Now:           func() time.Time { return r.now },
	}
}

func (r *rig) run(c config) (code int, output string) {
	var out, errw bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- run(c, &out, &errw) }()
	select {
	case code := <-done:
		return code, out.String() + errw.String()
	case <-time.After(30 * time.Second):
		r.t.Fatal("run did not return within 30s")
		return 0, ""
	}
}

func (r *rig) launches() int {
	raw, _ := os.ReadFile(filepath.Join(r.dir, "launches"))
	return strings.Count(string(raw), "x")
}

func (r *rig) counter() []string {
	raw, err := os.ReadFile(r.state)
	if err != nil {
		return nil
	}
	return strings.Fields(string(raw))
}

func (r *rig) seed(lines ...time.Time) {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(strconv.FormatInt(l.Unix(), 10) + "\n")
	}
	if err := os.WriteFile(r.state, []byte(b.String()), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func TestARefusedLoginThatEndsIBCAtOnceIsCountedAndRetried(t *testing.T) {
	r := newRig(t)
	code, out := r.run(r.cfg(`echo "` + refusal + `"; exit 0`))
	if code != 75 {
		t.Fatalf("exit %d, want 75 (retry): %s", code, out)
	}
	if got := len(r.counter()); got != 1 {
		t.Fatalf("%d refusals counted, want 1", got)
	}
	if !strings.Contains(out, refusal) {
		t.Errorf("IBC's output did not reach the journal: %s", out)
	}
}

func TestAnUnterminatedFinalLineIsStillRead(t *testing.T) {
	r := newRig(t)
	code, _ := r.run(r.cfg(`printf 'Unrecognized Username or Password'; exit 0`))
	if code != 75 || len(r.counter()) != 1 {
		t.Fatalf("exit %d with %d refusals, want 75 with 1", code, len(r.counter()))
	}
}

func TestAnOrphanHoldingTheOutputDoesNotHideTheRefusalOrHangTheExit(t *testing.T) {
	r := newRig(t)
	start := time.Now()
	code, _ := r.run(r.cfg(`echo "` + refusal + `"; (sleep 8 &) ; exit 0`))
	if code != 75 || len(r.counter()) != 1 {
		t.Fatalf("exit %d with %d refusals, want 75 with 1", code, len(r.counter()))
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("waited %s for an orphan", time.Since(start))
	}
}

func TestARefusalLoggedManyTimesIsOneRefusalAndAfterABurstOfOutput(t *testing.T) {
	r := newRig(t)
	code, _ := r.run(r.cfg(`i=0; while [ $i -lt 3000 ]; do echo "noise line $i padding padding padding padding"; i=$((i+1)); done
for i in 1 2 3; do echo "` + refusal + ` $i"; done
sleep 30`))
	if code != 75 {
		t.Fatalf("exit %d, want 75", code)
	}
	if got := len(r.counter()); got != 1 {
		t.Fatalf("%d refusals counted for one login, want 1", got)
	}
}

func TestTheThirdRefusalInTheWindowStopsTheRetriesWithTheCheckedPhrase(t *testing.T) {
	r := newRig(t)
	r.seed(r.now.Add(-time.Hour), r.now.Add(-2*time.Hour))
	code, out := r.run(r.cfg(`echo "` + refusal + `"; exit 0`))
	if code != 77 || !strings.Contains(out, "login retries stopped") {
		t.Fatalf("exit %d, want 77 saying 'login retries stopped': %s", code, out)
	}
	// A start while the bound holds launches nothing.
	before := r.launches()
	code, out = r.run(r.cfg(`exit 0`))
	if code != 77 || r.launches() != before || !strings.Contains(out, "login retries stopped") {
		t.Fatalf("a start at the bound: exit %d, launches %d -> %d: %s", code, before, r.launches(), out)
	}
}

func TestRefusalsOlderThanTheWindowAreForgotten(t *testing.T) {
	r := newRig(t)
	old := r.now.Add(-25 * time.Hour)
	r.seed(old, old, old)
	code, out := r.run(r.cfg(`echo "` + refusal + `"; exit 0`))
	if code != 75 {
		t.Fatalf("exit %d, want 75: %s", code, out)
	}
}

func TestACompletedLoginEmptiesTheCount(t *testing.T) {
	r := newRig(t)
	r.seed(r.now.Add(-time.Hour), r.now.Add(-2*time.Hour))
	code, _ := r.run(r.cfg(`echo "IBC: Login has completed"; exit 0`))
	if code != 0 {
		t.Fatalf("exit %d, want IBC's own 0", code)
	}
	// One attempt stays reserved for a relogin IBC may make by itself.
	if got := r.counter(); len(got) != 1 {
		t.Fatalf("count %v after a completed login, want the 1 reserved", got)
	}
}

func TestAnIBCThatIgnoresTermIsKilledAfterTheDeadline(t *testing.T) {
	r := newRig(t)
	c := r.cfg(`trap '' TERM; echo "` + refusal + `"; while :; do sleep 1; done`)
	c.StopDeadline = time.Second
	start := time.Now()
	code, out := r.run(c)
	if code != 75 {
		t.Fatalf("exit %d, want 75: %s", code, out)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("took %s to end a deaf IBC", time.Since(start))
	}
}

func TestAnInvalidStopDeadlineFallsBackToTheDefaultNeverToNoEscalation(t *testing.T) {
	for _, v := range []string{"15s", "0", "-3", "", "99999"} {
		env := map[string]string{"TI_GATEWAY_STOP_DEADLINE": v}
		c, _ := configFromEnv(func(k string) string { return env[k] })
		if c.StopDeadline != 15*time.Second {
			t.Errorf("TI_GATEWAY_STOP_DEADLINE=%q gave %s, want the 15s default", v, c.StopDeadline)
		}
	}
	c, _ := configFromEnv(func(k string) string { return map[string]string{"TI_GATEWAY_STOP_DEADLINE": "30"}[k] })
	if c.StopDeadline != 30*time.Second {
		t.Errorf("a valid deadline gave %s", c.StopDeadline)
	}
}

func TestTheBoundCannotBeConfiguredToReachIBKRsLimit(t *testing.T) {
	for _, v := range []string{"0", "10", "12", "x"} {
		c, _ := configFromEnv(func(k string) string { return map[string]string{"TI_GATEWAY_LOGIN_MAX_FAILURES": v}[k] })
		if c.Max != 3 {
			t.Errorf("TI_GATEWAY_LOGIN_MAX_FAILURES=%q gave %d, want the default 3", v, c.Max)
		}
	}
}

func TestAnUntrustworthyCounterRefusesToLaunch(t *testing.T) {
	cases := map[string]func(r *rig){
		"malformed":    func(r *rig) { _ = os.WriteFile(r.state, []byte("abc\n"), 0o600) },
		"glued times":  func(r *rig) { _ = os.WriteFile(r.state, []byte("17800000001780000000\n"), 0o600) },
		"a directory":  func(r *rig) { _ = os.Mkdir(r.state, 0o700) },
		"a blank line": func(r *rig) { _ = os.WriteFile(r.state, []byte("1780000000\n\n"), 0o600) },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			setup(r)
			code, out := r.run(r.cfg(`exit 0`))
			if code != 77 || !strings.Contains(out, "login retries stopped") || !strings.Contains(out, "unreadable or malformed") {
				t.Fatalf("exit %d: %s", code, out)
			}
			if r.launches() != 0 {
				t.Fatal("IBC was launched with an untrustworthy count")
			}
		})
	}
}

func TestACounterThatCannotBeWrittenRefusesBeforeLaunch(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes anywhere")
	}
	r := newRig(t)
	ro := filepath.Join(r.dir, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	c := r.cfg(`exit 0`)
	c.StateFile = filepath.Join(ro, "login-failures")
	code, out := r.run(c)
	if code != 77 || !strings.Contains(out, "login retries stopped") || r.launches() != 0 {
		t.Fatalf("exit %d, launches %d: %s", code, r.launches(), out)
	}
}

func TestACountThatCannotBeReadAfterARefusalStopsIBCAndTheRetries(t *testing.T) {
	r := newRig(t)
	// The stand-in corrupts the counter, then refuses.
	c := r.cfg(`echo garbage >` + r.state + `; echo "` + refusal + `"; sleep 30`)
	code, out := r.run(c)
	if code != 77 || !strings.Contains(out, "login retries stopped") {
		t.Fatalf("exit %d, want 77 saying 'login retries stopped': %s", code, out)
	}
}

func TestIBCsOwnStatusPassesThroughWhenNothingWasRefused(t *testing.T) {
	r := newRig(t)
	code, _ := r.run(r.cfg(`echo "second factor timed out"; exit 3`))
	if code != 3 {
		t.Fatalf("exit %d, want IBC's own 3", code)
	}
}

func TestTheRenderersConfigurationExitIsPassedOnAndNothingLaunches(t *testing.T) {
	r := newRig(t)
	c := r.cfg(`exit 0`)
	c.Render = []string{"sh", "-c", "echo 'render: no login' >&2; exit 78"}
	code, _ := r.run(c)
	if code != 78 || r.launches() != 0 {
		t.Fatalf("exit %d, launches %d, want 78 and none", code, r.launches())
	}
}

// A failure to START IBC is not a configuration exit: 78 is in the unit's
// RestartPreventExitStatus, and a transient fork failure or a missing xvfb-run
// must be retried by systemd. The renderer's own exits still pass through.
func TestAFailureToStartIBCOrTheRendererIsRestartable(t *testing.T) {
	r := newRig(t)
	c := r.cfg(`exit 0`)
	c.Command = []string{filepath.Join(r.dir, "missing")}
	code, out := r.run(c)
	if code != 70 || !strings.Contains(out, "cannot start IBC") {
		t.Fatalf("a command that does not exist: exit %d, want 70 saying 'cannot start IBC': %s", code, out)
	}
	r = newRig(t)
	c = r.cfg(`exit 0`)
	c.Render = []string{filepath.Join(r.dir, "no-renderer")}
	if code, _ := r.run(c); code != 70 {
		t.Fatalf("a renderer that could not run: exit %d, want 70", code)
	}
}

// The attempt is recorded durably BEFORE IBC is launched, so a crash between
// the login and the parse of its refusal still counts.
func TestTheAttemptIsRecordedBeforeIBCIsLaunched(t *testing.T) {
	r := newRig(t)
	marker := filepath.Join(r.dir, "recorded")
	code, out := r.run(r.cfg(`if [ -s ` + r.state + ` ]; then echo yes >` + marker + `; fi; echo "` + refusal + `"; exit 0`))
	if code != 75 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if raw, _ := os.ReadFile(marker); !strings.Contains(string(raw), "yes") {
		t.Fatal("the counter was empty when IBC started: a crash after the login would go uncounted")
	}
	if got := len(r.counter()); got != 1 {
		t.Fatalf("%d attempts counted for one start, want 1 (the refusal adds nothing)", got)
	}
}

func TestARefusalWrittenToStderrIsRead(t *testing.T) {
	r := newRig(t)
	code, _ := r.run(r.cfg(`echo "` + refusal + `" >&2; exit 0`))
	if code != 75 {
		t.Fatalf("exit %d, want 75", code)
	}
}

// A prompt for the second factor means IBKR accepted the credentials, so the
// attempt is not a failed login and the count is cleared.
func TestASecondFactorPromptClearsTheCount(t *testing.T) {
	r := newRig(t)
	r.seed(r.now.Add(-time.Hour), r.now.Add(-2*time.Hour))
	code, _ := r.run(r.cfg(`echo "IBC: detected dialog entitled: Second Factor Authentication; event=Opened"; exit 1`))
	if code != 1 || len(r.counter()) != 1 {
		t.Fatalf("exit %d with count %v, want IBC's 1 and the 1 reserved", code, r.counter())
	}
}

// Output that ends while IBC still runs leaves nothing watching for the
// refusal: IBC is stopped and the retries stop.
func TestOutputThatEndsWhileIBCStillRunsStopsIBCAndTheRetries(t *testing.T) {
	r := newRig(t)
	start := time.Now()
	code, out := r.run(r.cfg(`exec >/dev/null 2>&1; sleep 60`))
	if code != 77 || !strings.Contains(out, "login retries stopped") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
}

type blockedWriter struct{ release chan struct{} }

func (b blockedWriter) Write(p []byte) (int, error) { <-b.release; return len(p), nil }

func TestAnOverLongLineStopsIBCAndTheRetries(t *testing.T) {
	r := newRig(t)
	code, out := r.run(r.cfg(`head -c 300000 /dev/zero | tr '\0' x; sleep 60`))
	if code != 77 || !strings.Contains(out, "login retries stopped") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestEveryMessageAtTheBoundSaysTheCheckedPhrase(t *testing.T) {
	r := newRig(t)
	r.seed(r.now.Add(-time.Hour), r.now.Add(-2*time.Hour))
	_, out := r.run(r.cfg(`echo "` + refusal + `"; exit 0`))
	if !strings.Contains(out, "login retries stopped") {
		t.Fatalf("%s", out)
	}
}

// Only an exec of IBC that actually ran is an attempt: a start that fails
// before launching IBC gives its count back, so a persistent start failure
// never turns into a permanent stop.
func TestAStartThatNeverRanIBCGivesItsAttemptBack(t *testing.T) {
	r := newRig(t)
	r.seed(r.now.Add(-time.Hour))
	c := r.cfg(`exit 0`)
	c.Command = []string{filepath.Join(r.dir, "missing")}
	for i := 0; i < 5; i++ {
		if code, out := r.run(c); code != 70 {
			t.Fatalf("start %d: exit %d, want 70 every time: %s", i+1, code, out)
		}
	}
	if got := len(r.counter()); got != 1 {
		t.Fatalf("%d attempts after five starts that never ran IBC, want the 1 seeded", got)
	}
}

// A refusal later in the same process, after the count was cleared by an
// accepted login (the nightly restart, the Sunday cold restart, a second-
// factor relogin), is attempt 1 of its own: the bound is 3 per episode.
func TestARefusalAfterAnAcceptedLoginInTheSameRunIsCounted(t *testing.T) {
	r := newRig(t)
	code, out := r.run(r.cfg(`echo "IBC: Login has completed"; echo "` + refusal + `"; sleep 30`))
	if code != 75 || len(r.counter()) != 1 || !strings.Contains(out, "attempt 1 of 3") {
		t.Fatalf("exit %d with %d attempts: %s", code, len(r.counter()), out)
	}
}

func TestTheBoundIsThreePerEpisodeAcrossAcceptedLoginsInOneRun(t *testing.T) {
	r := newRig(t)
	r.seed(r.now.Add(-time.Hour), r.now.Add(-2*time.Hour))
	// The start is attempt 3 of an episode that an accepted login then ends;
	// the refusal that follows is attempt 1 of the next, not a stop.
	code, _ := r.run(r.cfg(`echo "IBC: Login has completed"; echo "` + refusal + `"; sleep 30`))
	if code != 75 {
		t.Fatalf("exit %d, want 75", code)
	}
}

// The accepted patterns are IBC's exact lines, not a substring anywhere.
func TestOnlyIBCsExactLinesClearTheCount(t *testing.T) {
	for _, line := range []string{
		"note: Login has completed soon",
		"IBC: Login has completed? no",
		"the Second Factor Authentication dialog is discussed here",
		"detected dialog entitled: Second Factor Authentication; event=Closed",
	} {
		r := newRig(t)
		r.seed(r.now.Add(-time.Hour))
		r.run(r.cfg(`echo "IBC: starting"; echo '` + line + `'; exit 0`))
		if got := len(r.counter()); got != 2 {
			t.Errorf("%q cleared the count: %d attempts, want the seeded one and this start", line, got)
		}
	}
}

func TestEveryExitThatLeavesTheUnitDownSaysCannotStartIBC(t *testing.T) {
	r := newRig(t)
	c := r.cfg(`exit 0`)
	for _, code := range []int{64, 65, 78} {
		c.Render = []string{"sh", "-c", "echo 'render-ibc-config: bad' >&2; exit " + strconv.Itoa(code)}
		got, out := r.run(c)
		if got != code || !strings.Contains(out, "cannot start IBC") {
			t.Errorf("renderer exit %d: got %d, output %q, want the code and 'cannot start IBC'", code, got, out)
		}
	}
	c.Render, c.Command = nil, nil
	if got, out := r.run(c); got != 78 || !strings.Contains(out, "cannot start IBC") {
		t.Errorf("no command: got %d, output %q", got, out)
	}
}

// A relogin by IBC itself, after an accepted login, is reserved before IBC can
// make it: the counter is never empty while IBC runs, so a crash before the
// refusal is read still counts.
func TestAnAcceptedLoginLeavesAnAttemptReservedForARelogin(t *testing.T) {
	r := newRig(t)
	marker := filepath.Join(r.dir, "reserved")
	code, _ := r.run(r.cfg(`echo "IBC: Login has completed"; sleep 1; if [ -s ` + r.state + ` ]; then echo yes >` + marker + `; fi; exit 0`))
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if raw, _ := os.ReadFile(marker); !strings.Contains(string(raw), "yes") {
		t.Fatal("the counter was empty after the accepted login: a relogin's refusal would go uncounted across a crash")
	}
}

// xvfb-run ran but IBC never did (ibcstart.sh missing): no IBC line proves a
// login was submitted, so the attempt is given back.
func TestAWrapperThatExitsBeforeIBCRunsGivesTheAttemptBack(t *testing.T) {
	r := newRig(t)
	r.seed(r.now.Add(-time.Hour))
	for i := 0; i < 5; i++ {
		code, _ := r.run(r.cfg(`echo "xvfb-run: error: /opt/ibc/scripts/ibcstart.sh: No such file or directory" >&2; exit 127`))
		if code != 127 {
			t.Fatalf("start %d: exit %d, want the wrapper's 127 each time", i+1, code)
		}
	}
	if got := len(r.counter()); got != 1 {
		t.Fatalf("%d attempts after five wrapper failures, want the 1 seeded", got)
	}
}

func TestAnIBCLineProvesALoginWasSubmittedSoAnExitKeepsTheAttempt(t *testing.T) {
	r := newRig(t)
	code, _ := r.run(r.cfg(`echo "IBC: Starting Gateway"; exit 1`))
	if code != 1 || len(r.counter()) != 1 {
		t.Fatalf("exit %d with %d attempts, want 1 and 1", code, len(r.counter()))
	}
}

// A journal that stops draining fails the run closed (the count is already
// on disk): nothing is queued, nothing is dropped.
func TestAStuckJournalFailsTheRunClosedWithTheCountOnDisk(t *testing.T) {
	r := newRig(t)
	b := blockedWriter{release: make(chan struct{})}
	defer close(b.release)
	c := r.cfg(`echo "` + refusal + `"; sleep 60`)
	c.SinkTimeout = time.Second
	done := make(chan int, 1)
	go func() { done <- run(c, b, &bytes.Buffer{}) }()
	select {
	case code := <-done:
		if code != 77 || len(r.counter()) != 1 {
			t.Fatalf("exit %d with %d attempts, want 77 with 1", code, len(r.counter()))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a stuck journal held the run")
	}
}

// A slow but draining sink gets every line, evidence included, before run
// returns: the delivery is synchronous.
type slowWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(2 * time.Millisecond)
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func TestEveryLineIsInTheJournalWhenRunReturns(t *testing.T) {
	r := newRig(t)
	w := &slowWriter{}
	code := run(r.cfg(`i=0; while [ $i -lt 300 ]; do echo "noise $i"; i=$((i+1)); done; echo "IBC: LoginState is LOGGED_OUT"; echo "IBC: Login has completed"; exit 0`), w, &bytes.Buffer{})
	w.mu.Lock()
	got := w.buf.String()
	w.mu.Unlock()
	if code != 0 || !strings.Contains(got, "noise 299") || !strings.Contains(got, "LoginState is LOGGED_OUT") || !strings.Contains(got, "IBC: Login has completed") {
		t.Fatalf("exit %d, journal lacks lines at exit: %.200s", code, got)
	}
}

// The executable itself, with a journal nobody reads: it exits 77 with the
// count on disk (the boundary the unit sees), not merely run's return.
func TestMain(m *testing.M) {
	if os.Getenv("GATEWAY_START_AS_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func TestTheExecutableWithAJournalNobodyReadsExits77WithTheCountOnDisk(t *testing.T) {
	r := newRig(t)
	script := filepath.Join(r.dir, "ibc.sh")
	body := "#!/bin/sh\nhead -c 400000 /dev/zero | tr '\\0' x | fold -w 200\necho \"" + refusal + "\"\nsleep 60\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pr.Close() }()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "GATEWAY_START_AS_MAIN=1", "TI_GATEWAY_RENDER="+script+".render", "TI_GATEWAY_IBC="+script,
		"TI_GATEWAY_STATE_FILE="+r.state, "TI_GATEWAY_SINK_TIMEOUT=1", "TI_GATEWAY_STOP_DEADLINE=1")
	if err := os.WriteFile(script+".render", []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	err = cmd.Start()
	_ = pw.Close()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 77 {
			t.Fatalf("exit %v, want 77", err)
		}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the executable did not exit with a journal nobody reads")
	}
	if got := len(r.counter()); got != 1 {
		t.Fatalf("%d attempts on disk, want 1", got)
	}
}
