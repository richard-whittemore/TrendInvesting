package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	if got := r.counter(); len(got) != 0 {
		t.Fatalf("count %v after a completed login, want empty", got)
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
	for _, v := range []string{"15s", "0", "-3", "", "61", "99999"} {
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
	if code != 1 || len(r.counter()) != 0 {
		t.Fatalf("exit %d with count %v, want IBC's 1 and an empty count", code, r.counter())
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

// A journal that stops draining stops IBC and asks for a restart (75; the count is already
// on disk): nothing is queued, nothing is dropped.
func TestAStuckJournalStopsIBCAndAsksForARestartWithTheCountOnDisk(t *testing.T) {
	r := newRig(t)
	b := blockedWriter{release: make(chan struct{})}
	defer close(b.release)
	c := r.cfg(`echo "` + refusal + `"; sleep 60`)
	c.SinkTimeout = time.Second
	done := make(chan int, 1)
	go func() { done <- run(c, b, &bytes.Buffer{}) }()
	select {
	case code := <-done:
		if code != 75 || len(r.counter()) != 1 {
			t.Fatalf("exit %d with %d attempts, want 75 (restartable) with 1", code, len(r.counter()))
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

// The executable itself, with a journal nobody reads: it exits 75 (restartable)
// with the count on disk (the boundary the unit sees), not merely run's return.
func TestMain(m *testing.M) {
	if os.Getenv("GATEWAY_START_AS_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func TestTheExecutableWithAJournalNobodyReadsExits75WithTheCountOnDisk(t *testing.T) {
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
		if !errors.As(err, &ee) || ee.ExitCode() != 75 {
			t.Fatalf("exit %v, want 75", err)
		}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the executable did not exit with a journal nobody reads")
	}
	if got := len(r.counter()); got != 1 {
		t.Fatalf("%d attempts on disk, want 1", got)
	}
}

// Every exec costs an attempt, whether or not IBC got as far as a login: a
// start that fails, or a wrapper that ends before IBC, spends one. Simple and
// fail-closed; the page says "attempts".
func TestEveryExecCostsAnAttemptEvenWhenIBCNeverRan(t *testing.T) {
	r := newRig(t)
	r.seed(r.now.Add(-time.Hour))
	c := r.cfg(`echo "xvfb-run: error: ibcstart.sh: No such file or directory" >&2; exit 127`)
	for i, want := range []int{127, 127, 77} {
		if code, out := r.run(c); code != want {
			t.Fatalf("start %d: exit %d, want %d: %s", i+1, code, want, out)
		}
	}
	c.Command = []string{filepath.Join(r.dir, "missing")}
	r2 := newRig(t)
	c.StateFile = r2.state
	for i, want := range []int{70, 70, 70, 77} {
		if code, out := r2.run(c); code != want {
			t.Fatalf("missing command, start %d: exit %d, want %d: %s", i+1, code, want, out)
		}
	}
}

// A write that errs, or takes only part of a line, is a failed journal.
type failingWriter struct{ short bool }

func (w failingWriter) Write(p []byte) (int, error) {
	if w.short {
		return len(p) / 2, nil
	}
	return 0, errors.New("broken pipe")
}

func TestAJournalWriteThatErrsOrIsShortStopsIBCAndAsksForARestart(t *testing.T) {
	for name, w := range map[string]failingWriter{"error": {}, "short write": {short: true}} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			code := run(r.cfg(`echo "IBC: Starting Gateway"; sleep 30`), w, &bytes.Buffer{})
			if code != 75 {
				t.Fatalf("exit %d, want 75", code)
			}
		})
	}
}

// A journal that is slow (each write inside the sink bound) when IBC exits,
// with several lines still buffered, does not turn a restartable exit into a
// permanent stop: the drain waits on progress, within a total cap.
type slowWriter2 struct {
	delay time.Duration
	mu    *sync.Mutex
	lines *int
}

func (w slowWriter2) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	w.mu.Lock()
	defer w.mu.Unlock()
	*w.lines += strings.Count(string(p), "\n")
	return len(p), nil
}

func TestASlowJournalWithManyBufferedLinesAtExitDoesNotTurnARestartIntoAStop(t *testing.T) {
	r := newRig(t)
	c := r.cfg(`echo "IBC: one"; echo "IBC: two"; echo "IBC: three"; echo "IBC: four"; exit 3`)
	c.DrainDeadline, c.SinkTimeout = time.Second, 10*time.Second
	var mu sync.Mutex
	lines := 0
	// Four writes of 3 s each: 12 s, past the old bound of 1 + 2 s and inside the sink bound each.
	if code := run(c, slowWriter2{3 * time.Second, &mu, &lines}, &bytes.Buffer{}); code != 3 {
		t.Fatalf("exit %d, want IBC's own 3", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if lines != 4 {
		t.Fatalf("%d lines reached the journal, want all 4", lines)
	}
}

// A quiet IBC (hours between lines) whose orphan writes a line after it exits
// is read for DrainDeadline: the idle clock starts when the drain does.
func TestAQuietRunsDrainStartsItsOwnClockAndReadsAnOrphansLateLine(t *testing.T) {
	r := newRig(t)
	c := r.cfg(`echo "IBC: hi"; sleep 8; (sleep 1; echo "IBC: late") & exit 3`)
	c.DrainDeadline, c.SinkTimeout = 3*time.Second, time.Second
	w := &recordingJournal{}
	if code := run(c, w, &bytes.Buffer{}); code != 3 {
		t.Fatalf("exit %d, want IBC's own 3", code)
	}
	if !strings.Contains(w.text(), "IBC: late") {
		t.Fatalf("the orphan's late line was not read: %q", w.text())
	}
}

type recordingJournal struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *recordingJournal) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *recordingJournal) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// The drain has a total cap: a journal slow on every line cannot keep the
// starter alive for the length of the buffer times the sink bound.
func TestTheDrainHasATotalCapAndAsksForARestartWhenItIsReached(t *testing.T) {
	r := newRig(t)
	c := r.cfg(`i=0; while [ $i -lt 40 ]; do echo "IBC: line $i"; i=$((i+1)); done; exit 3`)
	c.DrainDeadline, c.SinkTimeout = time.Second, 3*time.Second
	var mu sync.Mutex
	lines := 0
	start := time.Now()
	// The first line is slow enough that the rest are buffered when IBC exits.
	code := run(c, slowWriter2{2 * time.Second, &mu, &lines}, &bytes.Buffer{})
	if code != 75 {
		t.Fatalf("exit %d, want 75 when the cap is reached", code)
	}
	if time.Since(start) > 25*time.Second {
		t.Fatalf("took %s, want a total cap of about 2 x sink bound + drain", time.Since(start))
	}
}

// A renderer whose output cannot be journalled is a restartable failure, and
// IBC is not started.
func TestARendererWhoseOutputCannotBeJournalledIsARestartableFailureAndIBCDoesNotStart(t *testing.T) {
	r := newRig(t)
	c := r.cfg(`exit 0`)
	c.Render = []string{"sh", "-c", "echo 'render-ibc-config: wrote config'"}
	c.SinkTimeout = time.Second
	b := blockedWriter{release: make(chan struct{})}
	defer close(b.release)
	var errw bytes.Buffer
	if code := run(c, b, &errw); code != 75 || r.launches() != 0 {
		t.Fatalf("exit %d with %d launches, want 75 and none", code, r.launches())
	}
	// The health check keys on this phrase to read the Gateway as not logged in.
	if !strings.Contains(errw.String(), "gateway-start: cannot start IBC:") {
		t.Fatalf("the message lacks 'cannot start IBC:': %q", errw.String())
	}
}

// A refusal after an accepted login in the same run is its own attempt,
// recorded when it is seen with its own time.
func TestAReloginsRefusalIsRecordedAtTheTimeItIsSeen(t *testing.T) {
	r := newRig(t)
	// A clock that runs 3600 times faster than the wall: the start's attempt,
	// the accepted login and the refusal two seconds later carry different stamps.
	began := time.Now()
	c := r.cfg(`echo "IBC: Login has completed"; sleep 2; echo "` + refusal + `"; sleep 30`)
	c.Now = func() time.Time { return r.now.Add(time.Since(began) * 3600) }
	code, _ := r.run(c)
	got := r.counter()
	if code != 75 || len(got) != 1 {
		t.Fatalf("exit %d, count %v, want 75 and one attempt", code, got)
	}
	at, _ := strconv.ParseInt(got[0], 10, 64)
	if at-r.now.Unix() < int64(1.5*3600) {
		t.Fatalf("the attempt is stamped %ds after the start, want the refusal's time (about 2 s of wall time = 7200 s)", at-r.now.Unix())
	}
}

func TestTheStartersWarningsAreBoundedByTheSink(t *testing.T) {
	r := newRig(t)
	c := r.cfg(`exit 0`)
	c.Warnings = []string{"TI_GATEWAY_STOP_DEADLINE=\"x\" is not a whole number"}
	c.SinkTimeout = time.Second
	b := blockedWriter{release: make(chan struct{})}
	defer close(b.release)
	done := make(chan int, 1)
	go func() { done <- run(c, &bytes.Buffer{}, b) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("a journal that takes nothing held the starter at its warnings")
	}
}

// A chatty orphan keeps the reader alive until the drain's total cap: the cap's
// 75 must stand, never become 77 because closing the pipe woke a blocked read.
func TestTheTotalCapStaysRestartableWhenTheReaderIsWokenByTheClose(t *testing.T) {
	for i := 0; i < 20; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			c := r.cfg(`echo "IBC: hi"; (j=0; while [ $j -lt 24 ]; do sleep 0.5; echo "IBC: tick"; j=$((j+1)); done) & exit 3`)
			c.DrainDeadline, c.SinkTimeout = time.Second, time.Second // total cap 3 s
			if code := run(c, &bytes.Buffer{}, &bytes.Buffer{}); code != 75 {
				t.Fatalf("exit %d, want 75 from the cap", code)
			}
		})
	}
}

// The pipe deadline is renewed before each read: lines still in the kernel pipe
// after forwarding the first (slowly) are read and delivered.
func TestLinesStillInThePipeAfterASlowForwardAreDelivered(t *testing.T) {
	r := newRig(t)
	c := r.cfg(`l() { printf 'IBC: %s ' "$1"; head -c 20000 /dev/zero | tr '\\0' x; echo; }
l L1; sleep 0.3; l L2; l L3; exit 3`)
	c.DrainDeadline, c.SinkTimeout = time.Second, 10*time.Second
	var mu sync.Mutex
	var got strings.Builder
	w := writerFunc(func(p []byte) (int, error) {
		time.Sleep(3 * time.Second)
		mu.Lock()
		defer mu.Unlock()
		got.Write(p)
		return len(p), nil
	})
	if code := run(c, w, &bytes.Buffer{}); code != 3 {
		t.Fatalf("exit %d, want IBC's own 3", code)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"IBC: L1 ", "IBC: L2 ", "IBC: L3 "} {
		if !strings.Contains(got.String(), want) {
			t.Fatalf("the journal lacks %q: the line left in the pipe was dropped", want)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// The longest cycle of a failing start (a journal write at its bound, the stop
// and the kill at theirs, the drain's total cap, and the unit's 30 s) stays under
// ten minutes by construction, whatever the settings: the reporter catches loops
// that restart at least every ten minutes.
func TestTheSettingsCannotMakeAFailingStartsCycleLongerThanTenMinutes(t *testing.T) {
	for _, v := range []string{"61", "120", "300"} {
		c, warn := configFromEnv(func(k string) string {
			return map[string]string{"TI_GATEWAY_SINK_TIMEOUT": v, "TI_GATEWAY_STOP_DEADLINE": v}[k]
		})
		if c.SinkTimeout > 60*time.Second || c.StopDeadline > 60*time.Second || len(warn) != 2 {
			t.Errorf("%s s accepted: sink %s, stop %s, warnings %v", v, c.SinkTimeout, c.StopDeadline, warn)
		}
	}
	c, _ := configFromEnv(func(k string) string {
		return map[string]string{"TI_GATEWAY_SINK_TIMEOUT": "60", "TI_GATEWAY_STOP_DEADLINE": "60"}[k]
	})
	worst := 3*c.SinkTimeout + 2*c.StopDeadline + c.DrainDeadline + 30*time.Second
	if worst >= 10*time.Minute {
		t.Fatalf("worst cycle %s is not under ten minutes", worst)
	}
}

// Synthetic IBC output exercises the reset policy in docs/manual/server.md,
// "The Gateway is logged out"; the clock advances without contacting IBKR.
func resetClock(r *rig, c *config, at string, onWait func(time.Duration)) {
	r.t.Helper()
	now, err := time.Parse(time.RFC3339, at)
	if err != nil {
		r.t.Fatal(err)
	}
	var mu sync.Mutex
	c.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	c.After = func(d time.Duration) <-chan time.Time {
		if onWait != nil {
			onWait(d)
		}
		mu.Lock()
		now = now.Add(d)
		next := now
		mu.Unlock()
		ch := make(chan time.Time, 1)
		ch <- next
		return ch
	}
}

func TestNightlyResetBoundariesUseNewYorkRegardlessOfTheClocksZone(t *testing.T) {
	for _, tc := range []struct {
		at    string
		clock string
		reset bool
	}{
		{"2026-10-31T03:39:59Z", "23:39", false},
		{"2026-10-31T03:40:00Z", "23:40", true},
		{"2026-10-31T04:08:00Z", "00:08", true},
		{"2026-10-31T04:59:59Z", "00:59", true},
		{"2026-10-31T05:00:00Z", "01:00", false},
		{"2026-10-31T05:05:00Z", "01:05", false},
		{"2026-12-01T05:08:00Z", "00:08", true},
		{"2026-12-01T06:00:00Z", "01:00", false},
	} {
		t.Run(tc.at, func(t *testing.T) {
			r := newRig(t)
			c := r.cfg(`if [ "$(wc -l < ` + filepath.Join(r.dir, "launches") + `)" -eq 1 ]; then echo '` + refusal + `'; else echo 'IBC: Login has completed'; fi`)
			waits := 0
			resetClock(r, &c, tc.at, func(d time.Duration) {
				waits++
				if d <= 0 || d > 15*time.Minute || len(r.counter()) != 0 {
					t.Errorf("wait %s, count %v; want an uncounted reset and at most 15m", d, r.counter())
				}
			})
			code, out := r.run(c)
			if tc.reset {
				want := "login refused at " + tc.clock + " ET inside IBKR's nightly reset hour; waiting 15 minutes, not counted (1 waits so far)"
				if code != 0 || waits != 1 || r.launches() != 2 || !strings.Contains(out, want) || len(r.counter()) != 0 {
					t.Fatalf("reset: exit %d, waits %d, launches %d, count %v: %s", code, waits, r.launches(), r.counter(), out)
				}
			} else if code != 75 || waits != 0 || len(r.counter()) != 1 {
				t.Fatalf("outside reset: exit %d, waits %d, count %v: %s", code, waits, r.counter(), out)
			}
		})
	}
}

func TestThreeResetRefusalsPreserveOlderAttemptsAndNeverStopTheRetries(t *testing.T) {
	r := newRig(t)
	r.now = time.Date(2026, 10, 31, 4, 8, 0, 0, time.UTC)
	r.seed(r.now, r.now.Add(-time.Hour))
	before, _ := os.ReadFile(r.state)
	c := r.cfg(`if [ "$(wc -l < ` + filepath.Join(r.dir, "launches") + `)" -le 3 ]; then echo '` + refusal + `'; else echo 'IBC: Login has completed'; fi`)
	waits := 0
	resetClock(r, &c, "2026-10-31T04:08:00Z", func(d time.Duration) {
		waits++
		got, _ := os.ReadFile(r.state)
		if d != 15*time.Minute || !bytes.Equal(got, before) {
			t.Errorf("wait %d: duration %s, count %q; want 15m and unchanged %q", waits, d, got, before)
		}
	})
	code, out := r.run(c)
	if code != 0 || waits != 3 || r.launches() != 4 || len(r.counter()) != 0 || strings.Contains(out, stopPhrase) || !strings.Contains(out, "(3 waits so far)") {
		t.Fatalf("exit %d, waits %d, launches %d, count %v: %s", code, waits, r.launches(), r.counter(), out)
	}
}

func TestResetWaitEndsAtOneAndTheNextRefusalReachesTheExistingBound(t *testing.T) {
	r := newRig(t)
	r.now = time.Date(2026, 10, 31, 4, 58, 0, 0, time.UTC)
	r.seed(r.now, r.now.Add(-time.Hour))
	c := r.cfg(`echo '` + refusal + `'`)
	waits := 0
	resetClock(r, &c, "2026-10-31T04:58:00Z", func(d time.Duration) {
		waits++
		if d != 2*time.Minute {
			t.Errorf("wait %s; want 2m to the 01:00 ET cutoff", d)
		}
	})
	code, out := r.run(c)
	if code != 77 || waits != 1 || r.launches() != 2 || len(r.counter()) != 3 || !strings.Contains(out, stopPhrase) {
		t.Fatalf("exit %d, waits %d, launches %d, count %v: %s", code, waits, r.launches(), r.counter(), out)
	}
}

func TestRefusalsAtOneOhFiveKeepTheExistingRetryBound(t *testing.T) {
	r := newRig(t)
	c := r.cfg(`echo '` + refusal + `'`)
	resetClock(r, &c, "2026-10-31T05:05:00Z", func(time.Duration) { t.Error("wait outside the reset window") })
	for _, want := range []int{75, 75, 77} {
		if code, out := r.run(c); code != want {
			t.Fatalf("exit %d, want %d: %s", code, want, out)
		}
	}
}

func TestGatewayUnitAllowsTheResetWaitWithoutReadinessOrWatchdogNotifications(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/systemd/ti-gateway.service")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(raw)
	if !strings.Contains(unit, "Type=simple\n") || strings.Contains(unit, "WatchdogSec=") || strings.Contains(unit, "TimeoutStartSec=") {
		t.Fatal("reassess the in-process reset wait: unit type, start timeout or watchdog changed")
	}
	for _, want := range []string{"Restart=always\n", "RestartSec=30\n", "RestartPreventExitStatus=64 65 77 78\n"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q", want)
		}
	}
}

func TestResetRefusalAfterAnAcceptedLoginDoesNotRestoreTheOldCount(t *testing.T) {
	r := newRig(t)
	r.now = time.Date(2026, 10, 31, 4, 8, 0, 0, time.UTC)
	r.seed(r.now.Add(-time.Hour))
	c := r.cfg(`echo 'IBC: Login has completed'; if [ "$(wc -l < ` + filepath.Join(r.dir, "launches") + `)" -eq 1 ]; then echo '` + refusal + `'; fi`)
	waits := 0
	resetClock(r, &c, "2026-10-31T04:08:00Z", func(time.Duration) {
		waits++
		if len(r.counter()) != 0 {
			t.Errorf("accepted login's cleared count restored: %v", r.counter())
		}
	})
	code, out := r.run(c)
	if code != 0 || waits != 1 || len(r.counter()) != 0 {
		t.Fatalf("exit %d, waits %d, count %v: %s", code, waits, r.counter(), out)
	}
}

func TestResetRetryEndsIBCsDescendantsBeforeWaiting(t *testing.T) {
	r := newRig(t)
	pidFile := filepath.Join(r.dir, "child")
	c := r.cfg(`if [ "$(wc -l < ` + filepath.Join(r.dir, "launches") + `)" -eq 1 ]; then sleep 3 & echo $! > ` + pidFile + `; echo '` + refusal + `'; wait; else echo 'IBC: Login has completed'; fi`)
	waits := 0
	resetClock(r, &c, "2026-10-31T04:08:00Z", func(time.Duration) {
		waits++
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			t.Error(err)
			return
		}
		out, _ := exec.Command("ps", "-p", strings.TrimSpace(string(raw)), "-o", "stat=").Output()
		if status := strings.TrimSpace(string(out)); status != "" && !strings.HasPrefix(status, "Z") {
			t.Errorf("IBC descendant still running during the wait: %s", status)
		}
	})
	code, out := r.run(c)
	if code != 0 || waits != 1 {
		t.Fatalf("exit %d, waits %d: %s", code, waits, out)
	}
}

func TestServerManualNamesNightlyResetAndOrderedRecovery(t *testing.T) {
	raw, err := os.ReadFile("../../docs/manual/server.md")
	if err != nil {
		t.Fatal(err)
	}
	var row string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "| `ti-gateway` failed and stays down") {
			row = line
		}
	}
	for _, want := range []string{"00:10 ET", "Friday", "01:00 ET", "waiting 15 minutes, not counted", "sudo systemctl start ti-gateway.service", "Login has completed", "sudo systemctl start ti-adapter-boost.service"} {
		if !strings.Contains(row, want) {
			t.Errorf("gateway recovery row lacks %q", want)
		}
	}
}

func TestResetWaitCanBeInterruptedWhenTheUnitStops(t *testing.T) {
	if os.Getenv("TI_TEST_RESET_WAIT") == "1" {
		dir := os.Getenv("TI_TEST_RESET_DIR")
		r := &rig{t: t, dir: dir, state: filepath.Join(dir, "login-failures")}
		c := r.cfg(`echo '` + refusal + `'`)
		resetClock(r, &c, "2026-10-31T04:08:00Z", nil)
		c.After = func(time.Duration) <-chan time.Time {
			fmt.Println("reset wait started")
			return nil
		}
		os.Exit(run(c, os.Stdout, os.Stderr))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestResetWaitCanBeInterruptedWhenTheUnitStops$")
	cmd.Env = append(os.Environ(), "TI_TEST_RESET_WAIT=1", "TI_TEST_RESET_DIR="+t.TempDir())
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := false
	scanner := bufio.NewScanner(pipe)
	for scanner.Scan() {
		if scanner.Text() == "reset wait started" {
			ready = true
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Error(err)
			}
			break
		}
	}
	err = cmd.Wait()
	var ee *exec.ExitError
	if !ready || !errors.As(err, &ee) || ee.ExitCode() != 143 {
		t.Fatalf("wait reached %v, exit %v; want SIGTERM to interrupt the wait (143)", ready, err)
	}
}

func TestAUnitStopDuringResetOutputDoesNotBecomeAnotherResetWait(t *testing.T) {
	r := newRig(t)
	c := r.cfg(`printf 'noise\n` + refusal + `\n'; while :; do :; done`)
	resetClock(r, &c, "2026-10-31T04:08:00Z", nil)
	c.Accepted = regexp.MustCompile(defaultAccepted)
	c.SinkTimeout = time.Second
	sigs := make(chan os.Signal, 1)
	out := &journal{w: writerFunc(func(p []byte) (int, error) {
		if strings.TrimSpace(string(p)) == "noise" {
			sigs <- syscall.SIGTERM
			// Hold the buffered refusal until the supervisor consumes the stop.
			deadline := time.Now().Add(time.Second)
			for len(sigs) > 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
		}
		return len(p), nil
	}), timeout: time.Second}
	var messages bytes.Buffer
	code := runIBC(c, out, &journal{w: &messages, timeout: time.Second}, sigs, 0)
	if code != 143 {
		t.Fatalf("exit %d, want 143 after a unit stop, never another reset wait: %s", code, messages.String())
	}
}
