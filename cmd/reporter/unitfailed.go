package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// unitCommandTimeout bounds each systemctl and journalctl call, so a hung one
// never holds back a page.
const unitCommandTimeout = 15 * time.Second

// unitReminderInterval is the shortest time between two reminders of one
// failure episode on one channel (ADR 0019, "Alerting": each incident once,
// and a fault that persists is not left silent).
const unitReminderInterval = time.Hour

// tradingUnit is the shape of every unit name the reporter hands systemctl or
// journalctl (-units, -self-unit and the hook's argument): the engine, the
// adapter (Boost's ti-adapter-boost.service, or an IRA's instance of the
// adapter template, ti-adapter@<instance>.service) or the reporter of one
// account, or one of the shared units: a data
// job (Boost's ti-data.service or an IRA's ti-data@<instance>.service) and the
// Gateway, or the shared memory sampler (lessons-2026-10.md L1/L8),
// nothing else, so no value can be
// read as an option or name another unit (ADR 0019, "Alerting"; ADR 0027,
// the restart model).
var tradingUnit = regexp.MustCompile(`^(ti-(engine@|adapter-|adapter@|reporter@|data@|flex@)[a-z0-9][a-z0-9-]*|ti-data|ti-gateway|ti-memory)\.service$`)

// invocationShape is systemd's invocation ID: 32 lowercase hex digits.
var invocationShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

// checkTradingUnits refuses any name that is not a trading unit's, before
// systemctl or journalctl is run on it.
func checkTradingUnits(flagName string, units []string) error {
	for _, unit := range units {
		if !tradingUnit.MatchString(unit) {
			return fmt.Errorf("reporter: %s %q is not a trading unit's name (ti-engine@<account>.service, ti-adapter-<account>.service, ti-adapter@<account>.service, ti-reporter@<account>.service, ti-gateway.service, ti-data.service, ti-data@<account>.service, ti-flex@<account>.service or ti-memory.service)", flagName, unit)
		}
	}
	return nil
}

// unitCommand runs systemctl or journalctl, bounded by unitCommandTimeout,
// and returns what it printed.
func unitCommand(bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), unitCommandTimeout)
	defer cancel()
	//nolint:gosec // G204: the binary is the operator's -systemctl or -journalctl flag, and every unit name in args matched tradingUnit and every invocation invocationShape before this call.
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = time.Second
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("%s timed out after %s", filepath.Base(bin), unitCommandTimeout)
	}
	return stdout.String(), err
}

// unitState is the record of the failed-unit pages, one file for the host
// beside the reporters' state files, in the reporter's own state format: the
// channels that took each page. The hook and every reporter instance share
// it, so each failure pages once between them.
type unitState struct {
	path      string
	seen      map[string]bool
	sent      map[string]time.Time
	repeats   map[string]int
	baselines map[string]time.Time
}

func (u *unitState) record() error {
	if err := saveState(u.path, u.seen, u.sent, u.repeats, u.baselines); err != nil {
		return persistError{err}
	}
	return nil
}

// withUnitState runs fn on the unit-failure record under an exclusive lock.
func withUnitState(path string, at time.Time, fn func(*unitState) error) error {
	return withUnitStateWait(path, at, 0, fn)
}

// withUnitStateWait is withUnitState that gives up, with an error, when the
// lock is not got within wait (0 waits as long as it takes). The Gateway's
// bookkeeping uses it so that a stalled lock holder never delays its page.
func withUnitStateWait(path string, at time.Time, wait time.Duration, fn func(*unitState) error) error {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("reporter: the unit-failure record's lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if wait <= 0 {
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			return fmt.Errorf("reporter: the unit-failure record's lock: %w", err)
		}
	} else {
		deadline := time.Now().Add(wait)
		for {
			err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				break
			}
			if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
				return fmt.Errorf("reporter: the unit-failure record's lock was not got within %s: %w", wait, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	seen, sent, repeats, baselines, err := loadState(path)
	if err != nil {
		return err
	}
	u := &unitState{path: path, seen: seen, sent: sent, repeats: repeats, baselines: baselines}
	// A receipt past its emergency is dropped, with the bookkeeping of an
	// episode it was the last record of, and the drop is saved here: the
	// callback may have nothing else to write.
	var pruneErr error
	if pruneReceipts(seen, sent, repeats, at) {
		pruneErr = u.record()
	}
	return errors.Join(pruneErr, fn(u))
}

// notifyUnitFailed is `reporter notify-unit-failed <unit>`, run by the
// OnFailure hook (deploy/systemd/ti-unit-failed@.service) when a trading unit
// fails: one urgent page through the reporter's channels, naming the unit, the
// failed run's reason and the manual row, once per failure episode
// (failedRun). A channel that fails fails the command, so the hook unit shows
// failed, and only that channel is tried again by the next reporter firing
// (ADR 0019, "Alerting").
func notifyUnitFailed(args []string, getenv func(string) string, now func() time.Time, out io.Writer, client *http.Client) error {
	flags := flag.NewFlagSet("reporter notify-unit-failed", flag.ContinueOnError)
	flags.SetOutput(out)
	statePath := flags.String("state", "", "the host's record of failed-unit pages (unit-failures.json beside the reporters' state files)")
	channels := flags.String("channel", "ntfy,pushover", "comma-separated channels: ntfy, pagerduty, pushover, github-issues (one issue per page, with every secret removed)")
	secretsFile := flags.String("secrets-file", "", "KEY=VALUE file of channel secrets (under systemd: %d/secrets, the credential file)")
	journalctl := flags.String("journalctl", "journalctl", "the journal reader")
	systemctl := flags.String("systemctl", "systemctl", "the systemctl that names the failed run when systemd did not (MONITOR_INVOCATION_ID)")
	sendTimeout := flags.Duration("send-timeout", 30*time.Second, "longest one send to one channel may take before it counts as failed")
	repeat := flags.Duration("repeat-interval", defaultRepeatInterval, "unused by a page that does not repeat; kept so the hook and the reporter share one delivery code")
	maintenanceFile := flags.String("maintenance", "", "the host's maintenance window marker (default: /run/ti-maintenance/maintenance.json): while a window is open the failure is recorded and not paged, and the first reporter firing after it closes pages the unit if it is still failed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *statePath == "" || flags.NArg() != 1 || !tradingUnit.MatchString(flags.Arg(0)) {
		return errors.New("reporter: notify-unit-failed needs -state and exactly one trading unit's name, such as ti-adapter-boost.service")
	}
	if *sendTimeout <= 0 {
		return errors.New("reporter: -send-timeout must be positive")
	}
	unit := flags.Arg(0)
	// Inside a maintenance window the failure is recorded and not paged: the
	// reporter's first firing after the window closes pages it if the unit is
	// still failed, and forgets it if the unit recovered (maintenance.go).
	if until := holdFor(report.KindUnitFailed, func() time.Time { return windowEnd(maintenancePath(*maintenanceFile), now()) }); !until.IsZero() {
		at := now()
		run := failedInvocation(getenv, *systemctl, unit, at)
		return withUnitState(*statePath, at, func(u *unitState) error {
			holdAlert(u.sent, until, at, report.UnitFailedKey(unit, run))
			return u.record()
		})
	}
	var failures []error
	secrets := secretValues(getenv, *secretsFile)
	if *secretsFile != "" {
		var secretsErr error
		if getenv, secretsErr = withSecretsFile(getenv, *secretsFile); secretsErr != nil {
			failures = append(failures, secretsErr)
		}
	}
	policy, policyErr := loadPagingPolicy(getenv)
	if policyErr != nil {
		failures = append(failures, policyErr)
	}
	bounded := *client
	bounded.Timeout = *sendTimeout
	senders, setAside, err := buildSenders(*channels, getenv, &bounded, out)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	for _, f := range setAside {
		failures = append(failures, f.err)
	}
	// An issue is public: it is given every secret the hook holds to remove,
	// as the reporter's firing gives its own.
	withRedaction(senders, secrets)
	at := now()
	run := failedInvocation(getenv, *systemctl, unit, at)
	err = withUnitState(*statePath, at, func(u *unitState) error {
		// A run the reporter already saw well (a held failure that recovered, or
		// an episode resolved) is not paged by a delayed hook (ADR 0019,
		// "Alerting": each incident once).
		if handledRun(u, unit, run) {
			return nil
		}
		return failedRun(u, unit, run, *journalctl, secrets, senders, setAside, at, time.Time{}, *repeat, policy)
	})
	return errors.Join(append(failures, err)...)
}

// failedInvocation is systemd's invocation ID of the unit's failed run: the
// one systemd hands an OnFailure= unit (MONITOR_UNIT and
// MONITOR_INVOCATION_ID), else the unit's current one from systemctl. When
// neither names one, the run is named by the time of the page, so the page
// still goes: a duplicate is better than a missed failure (ADR 0019,
// "Alerting").
func failedInvocation(getenv func(string) string, systemctl, unit string, at time.Time) string {
	if id := getenv("MONITOR_INVOCATION_ID"); getenv("MONITOR_UNIT") == unit && invocationShape.MatchString(id) {
		return id
	}
	if samples, err := sampleUnits(systemctl, []string{unit}); err == nil && invocationShape.MatchString(samples[unit].invocation) {
		return samples[unit].invocation
	}
	return "unnamed-" + at.UTC().Format("20060102T150405.000000000Z")
}

// failedRun pages run, a failed run of unit, unless it belongs to a failure
// episode already paged. An episode begins with a page and lasts until the
// unit is seen well (a reporter firing that samples it not failed, or, for the
// reporter itself, a firing that finishes without a fault): every failed run
// before that, such as each timer firing of a reporter that keeps failing, is
// the same episode, so its incident is neither paged again nor resolved and
// triggered again, and each channel is reminded at most once an
// unitReminderInterval (ADR 0019, "Alerting": each incident once). An episode
// seen well whose incident a channel could not resolve is over: it is
// resolved again and the run pages afresh. A run systemd named no invocation
// for (unnamedRun) joins the open episode uncounted, since it may be the same
// run, or else begins one under a name of its own, so no two episodes share a
// key. sampledAt is when the firing sampled systemd (zero for a hook, which
// names its run itself): a run the sample named joins uncounted when a later
// run was recorded at or after it, since that run is newer than the sample.
func failedRun(u *unitState, unit, run, journalctl string, secrets []string, senders []sender, setAside []channelFault, at, sampledAt time.Time, every time.Duration, policies ...*pagingPolicy) error {
	var errs []error
	// A run already resolved does not page again, whichever unit and however
	// old the sample that names it (the Gateway is sampled by every instance,
	// and one's stale sample can be applied after another resolved the episode).
	if handledRun(u, unit, run) && (!remindsUnit(unit) || !sampledAt.IsZero()) {
		return nil
	}
	episode, opened := "", time.Time{}
	for invocation, recorded := range unitRecords(u, unit) {
		key := report.UnitFailedKey(unit, invocation)
		if u.repeats[recoveredKey(key)] > 0 {
			errs = append(errs, resolveKey(u, key, senders, setAside, at))
			continue
		}
		if episode == "" || recorded.Before(opened) || (recorded.Equal(opened) && invocation < episode) {
			episode, opened = invocation, recorded
		}
	}
	if run == unnamedRun {
		if episode == "" {
			run = unnamedRun + "-" + at.UTC().Format("20060102T150405.000000000Z")
		} else {
			run = ""
		}
	}
	if episode != "" && run != "" && run != episode && !sampledAt.IsZero() && !latestRun(u, report.UnitFailedKey(unit, episode)).Before(sampledAt) {
		run = ""
	}
	if episode == "" {
		return errors.Join(append(errs, pageRun(u, unit, run, journalctl, secrets, senders, at, every, policies...))...)
	}
	if episode == run {
		// The unit is failed under the run that opened the episode: still
		// the same failure, so a reminder falls due even though no later run
		// was named.
		errs = append(errs, pageRun(u, unit, episode, journalctl, secrets, senders, at, every, policies...))
		if !remindsUnit(unit) {
			return errors.Join(errs...)
		}
		return errors.Join(append(errs, remindRun(u, unit, episode, "", senders, at, policies...))...)
	}
	// A channel that has not yet taken the episode's page is sent it first.
	errs = append(errs, pageRun(u, unit, episode, journalctl, secrets, senders, at, every, policies...))
	if !remindsUnit(unit) {
		noteLaterRun(u, unit, episode, run, at)
		return errors.Join(append(errs, u.record())...)
	}
	return errors.Join(append(errs, remindRun(u, unit, episode, run, senders, at, policies...))...)
}

// remindsUnit is whether a failed unit is reminded hourly. The data job is
// not: its timer runs it again, so one page is the incident, and the episode
// ends when a run ends well (ADR 0019, "Alerting": each incident once, and a
// fault that persists is not left silent). Its latest later failed run is
// still recorded (noteLaterRun), so that the episode's resolve marks that run
// handled (handleRuns) and it does not page again.
func remindsUnit(unit string) bool { return !dataJobUnit.MatchString(unit) }

// dataJobUnit is a timer-run job's unit: Boost's data job ti-data.service, an
// IRA's instance ti-data@<account>.service, or an account's Flex reader
// ti-flex@<account>.service, or ti-memory.service (lessons-2026-10.md
// L1/L8). A unit it matches is paged once by its
// OnFailure hook and then only resolved, never reminded, and never holds back
// the heartbeat, because its timer runs it again (ADR 0019, "Alerting": each
// incident once, and a fault that persists is not left silent). Only these
// timer-retried jobs may match: the engine, the adapter and the
// reporter must stay excluded, or their failures would lose their reminders.
var dataJobUnit = regexp.MustCompile(`^(ti-data(@[a-z0-9][a-z0-9-]*)?|ti-flex@[a-z0-9][a-z0-9-]*|ti-memory)\.service$`)

// unnamedRun is what a firing names a failed unit systemd gave no invocation
// for.
const unnamedRun = "unnamed"

// recoveredKey marks an episode whose unit was seen well while a channel kept
// its record because the resolve failed there: the episode is over.
func recoveredKey(key string) string { return channelKey("recovered", key) }

// failedRunKey records run as the latest later failed run of the episode
// keyed key, at the time it was recorded; only the latest is kept, and
// runsKey counts them, so the record stays bounded however long the episode.
func failedRunKey(run, key string) string { return channelKey("run:"+run, key) }

// runsKey counts the later failed runs of the episode keyed key.
func runsKey(key string) string { return channelKey("runs", key) }

// latestRun is when the episode keyed key last recorded a later failed run.
func latestRun(u *unitState, key string) time.Time {
	var latest time.Time
	for k, at := range u.sent {
		if strings.HasPrefix(k, "run:") && strings.HasSuffix(k, channelKey("", key)) && at.After(latest) {
			latest = at
		}
	}
	return latest
}

// remindedKey is when channel was last reminded of the episode keyed key.
func remindedKey(channel, key string) string { return channelKey("reminded:"+channel, key) }

// noteLaterRun records run, a later failed run of the episode begun by
// episode, as its latest and counts it once (none when run is empty): the
// record the reminders count and, for the data job, the one handleRuns reads
// when the episode resolves (ADR 0019, "Alerting").
func noteLaterRun(u *unitState, unit, episode, run string, at time.Time) {
	key := report.UnitFailedKey(unit, episode)
	if run == "" || !u.sent[failedRunKey(run, key)].IsZero() {
		return
	}
	for k := range u.sent {
		if strings.HasPrefix(k, "run:") && strings.HasSuffix(k, channelKey("", key)) {
			delete(u.sent, k)
		}
	}
	u.sent[failedRunKey(run, key)] = at
	u.repeats[runsKey(key)]++
}

// remindRun counts run, a later failed run of the episode begun by episode
// (none when run is empty), and reminds each channel that took the episode's page an
// unitReminderInterval or more since its page or its last reminder: "still
// failing since <time>", with ", N failed runs" once two or more runs are
// known, under the episode's key. A channel
// whose reminder fails is retried at the next firing that finds the unit
// still failed.
func remindRun(u *unitState, unit, episode, run string, senders []sender, at time.Time, policies ...*pagingPolicy) error {
	key := report.UnitFailedKey(unit, episode)
	noteLaterRun(u, unit, episode, run, at)
	interval := unitReminderInterval
	var policyErr error
	if p := policyOf(policies); p != nil {
		a := report.Alert{Key: key, Kind: report.KindUnitFailed, Repeat: true}
		senders, a, interval, policyErr = p.route(senders, a, u.sent, at, interval)
		if !a.Repeat {
			return errors.Join(policyErr, u.record())
		}
	}
	runs, since := 1+u.repeats[runsKey(key)], u.sent[channelKey("observed", key)]
	var due, echoes []sender
	for _, s := range senders {
		if _, echo := s.(echoOnly); echo {
			echoes = append(echoes, s)
			continue
		}
		took := u.sent[channelKey(s.name(), key)]
		if !u.seen[channelKey(s.name(), key)] || took.IsZero() {
			continue
		}
		if since.IsZero() || took.Before(since) {
			since = took
		}
		last := u.sent[remindedKey(s.name(), key)]
		if last.IsZero() {
			last = took
		}
		if at.Sub(last) >= interval {
			due = append(due, s)
		}
	}
	errs := []error{policyErr}
	if len(due) > 0 {
		a := report.UnitStillFailedAlert(hostName(), unit, episode, since, runs, at)
		for _, s := range echoes {
			_ = s.send(a, 0)
		}
		// The paging channels are reminded before an issue tracker, so a
		// slow tracker never holds back a page (ADR 0019, "Alerting"); a
		// tracker is given the reminder's own ordinal, so it comments on the
		// open issue rather than taking the reminder for the first page.
		for _, s := range trackersLast(due) {
			var err error
			if is, ok := s.(*issuesSender); ok {
				ordinal := u.repeats[remindedKey(s.name(), key)] + 1
				if err = is.remind(a, ordinal); err == nil {
					u.repeats[remindedKey(s.name(), key)] = ordinal
				}
			} else {
				if err = s.send(a, 0); err == nil {
					keepReceipt(s, a.Key, u.seen, at)
				}
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", s.name(), err))
				continue
			}
			u.sent[remindedKey(s.name(), key)] = at
		}
	}
	return errors.Join(append(errs, u.record())...)
}

// pageRun pages the failed run of unit on each channel that has not taken it.
func pageRun(u *unitState, unit, invocation, journalctl string, secrets []string, senders []sender, at time.Time, every time.Duration, policies ...*pagingPolicy) error {
	key := report.UnitFailedKey(unit, invocation)
	if policyOf(policies) == nil && allTook(senders, key, u.seen) {
		return nil
	}
	a := report.UnitFailedAlert(hostName(), unit, invocation, unitReason(journalctl, unit, invocation, secrets), at)
	// Each channel's taking is stamped with the page's time, which deliver
	// keeps only for a repeating alert: a firing resolves no page stamped at
	// or after its own sample of systemd.
	record := func() error {
		for _, s := range senders {
			if ck := channelKey(s.name(), key); u.seen[ck] && u.sent[ck].IsZero() {
				u.sent[ck] = at
			}
		}
		return u.record()
	}
	return deliver(senders, a, u.seen, u.sent, u.repeats, at, every, record, policies...)
}

// unitReason is the failed run's own last journal line
// (_SYSTEMD_INVOCATION_ID), cleaned for a page. A run that wrote nothing (a
// start systemd refused, such as a missing EnvironmentFile) is explained by
// systemd's own first "Failed" line about that run (UNIT= and INVOCATION_ID=
// from PID 1). It is never a line from an earlier run, nor journalctl's own
// notice that it found nothing ("-- No entries --"): it is run with -q, and a
// line it prints beginning "-- " is not the run's.
func unitReason(journalctl, unit, invocation string, secrets []string) string {
	if !invocationShape.MatchString(invocation) {
		return "(systemd named no invocation for the failed run, so its journal lines were not read)"
	}
	own, err := unitCommand(journalctl, "-q", "_SYSTEMD_UNIT="+unit, "_SYSTEMD_INVOCATION_ID="+invocation, "-n", "1", "-o", "cat", "--no-pager")
	if err != nil {
		return "(the unit's journal could not be read)"
	}
	if lines := journalLines(own); len(lines) > 0 {
		return report.UnitFailedReason(lines[0], secrets)
	}
	systemd, err := unitCommand(journalctl, "-q", "_PID=1", "UNIT="+unit, "INVOCATION_ID="+invocation, "-n", "50", "-o", "cat", "--no-pager")
	if err != nil {
		return "(the unit's journal could not be read)"
	}
	for _, line := range journalLines(systemd) {
		if strings.Contains(line, "Failed") {
			return report.UnitFailedReason(line, secrets)
		}
	}
	return report.UnitFailedReason("", secrets)
}

// journalLines are the non-blank lines journalctl printed, without its own
// notices, which begin "-- " ("-- No entries --").
func journalLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "-- ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// secretValues are the values of the secrets the reporter holds, so that a
// journal line echoing one is cleaned before it is sent: the environment's
// and every line of the secrets file.
func secretValues(getenv func(string) string, secretsFile string) []string {
	var values []string
	for _, k := range []string{"TI_NTFY_TOPIC", "TI_NTFY_SERVER", "TI_PAGERDUTY_ROUTING_KEY", "TI_PAGERDUTY_URL", "TI_PUSHOVER_APP_TOKEN", "TI_PUSHOVER_USER_KEY", "TI_PUSHOVER_URL", "TI_HEARTBEAT_URL", "TI_GITHUB_ISSUES_TOKEN"} {
		if v := getenv(k); v != "" {
			values = append(values, v)
		}
	}
	if secretsFile == "" {
		return values
	}
	raw, err := os.ReadFile(secretsFile)
	if err != nil {
		return values
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if _, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && strings.TrimSpace(v) != "" {
			values = append(values, strings.TrimSpace(v))
		}
	}
	return values
}

// unitSample is what systemd said of a unit at the start of a firing: whether
// it was failed, and its current invocation (the failed run's, when failed).
type unitSample struct {
	failed     bool
	state      string // systemd's ActiveState
	invocation string
}

// sampleUnits asks systemd for the state and invocation of each unit
// (`systemctl show -p ActiveState -p InvocationID`). A systemctl that cannot
// be run, or does not answer, is an error, never "none failed": an unchecked
// unit is not a healthy one.
func sampleUnits(systemctl string, units []string) (map[string]unitSample, error) {
	return sampleUnitsFor(systemctl, units, "the trading units were not checked")
}

// sampleUnitsFor is sampleUnits with the consequence its error names.
func sampleUnitsFor(systemctl string, units []string, consequence string) (map[string]unitSample, error) {
	samples := map[string]unitSample{}
	var failures []error
	for _, unit := range units {
		out, err := unitCommand(systemctl, "show", "-p", "ActiveState", "-p", "InvocationID", unit)
		props := map[string]string{}
		for _, line := range strings.Split(out, "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
				props[k] = v
			}
		}
		state, answered := props["ActiveState"]
		if err != nil || !answered {
			failures = append(failures, fmt.Errorf("reporter: systemctl show %s could not be run: %s", unit, consequence))
			continue
		}
		samples[unit] = unitSample{failed: state == "failed", state: state, invocation: props["InvocationID"]}
	}
	return samples, errors.Join(failures...)
}

// checkResolveUnits refuses, at startup, a -resolve-units name that is not a
// unit the reporter may only resolve (a timer-run job: remindsUnit
// is false) or that -units also names. A -resolve-units unit is left out of
// the heartbeat's hold and the state-at-start page, and its delivery errors
// are soft, so a trading unit listed there would lose both (ADR 0019,
// "Alerting": a fault that persists is not left silent); the two lists stay
// disjoint so a unit has one treatment.
func checkResolveUnits(resolveOnly, monitored []string) error {
	if err := checkTradingUnits("-resolve-units", resolveOnly); err != nil {
		return err
	}
	for _, unit := range resolveOnly {
		if slices.Contains(monitored, unit) {
			return fmt.Errorf("reporter: -resolve-units %q is also in -units: a unit is either monitored or only resolved", unit)
		}
		if remindsUnit(unit) {
			return fmt.Errorf("reporter: -resolve-units %q is a trading unit (the engine, the adapter or the reporter): only a data job, Flex reader or memory sampler (ti-data.service, ti-data@<account>.service, ti-flex@<account>.service or ti-memory.service) may be only resolved, since a trading unit must be paged, reminded and held against the heartbeat", unit)
		}
	}
	return nil
}

// splitUnits is the -units list.
func splitUnits(list string) []string {
	var units []string
	for _, u := range strings.Split(list, ",") {
		if u = strings.TrimSpace(u); u != "" {
			units = append(units, u)
		}
	}
	return units
}

// unitStep pages each failed trading unit's failed run that has not been
// paged (the hook does this at the moment of failure; this catches a hook
// that could not deliver, a unit that failed before the hook was installed,
// and a start refused while the unit was already failed, which systemd does
// not hand to OnFailure=) unless its episode was paged (failedRun), and
// resolves the episode of every unit seen not failed. samples are systemd's
// answers at the start of the firing, sampledAt their time: an episode whose
// page or latest failed run was recorded since then has a run newer than the
// sample, so it is never ended on the sample's word. Both reporters and the hook share the record under a lock.
//
// A resolve-only unit (the data job) is judged by run identity instead of a
// clock: current asks systemd for its state under the lock, so no older
// sample can page or resolve on a newer run's word. A failed run is paged
// once (failedRun), and not again while it is among the newest resolved runs
// (the bounded handled record, written in the same save as the removal), and an inactive or active
// unit under any run resolves its episode (ADR 0019, "Alerting": each
// incident once). The returned errors split by cause: a failure to deliver or
// sample for a resolve-only unit is soft (logged by the caller and retried at
// the next firing), and every other error, including any failure to persist
// the record, is hard and fails the firing (ADR 0019: delivery state is
// durable).
func unitStep(path string, units, resolveOnly []string, samples map[string]unitSample, sampledAt time.Time, journalctl string, secrets []string, senders []sender, setAside []channelFault, at time.Time, every time.Duration, window func() time.Time, current func(string) (unitSample, error), note func(string), policies ...*pagingPolicy) (soft, hard error) {
	var softErrs, hardErrs []error
	err := withUnitState(path, at, func(u *unitState) error {
		// Held records are a window's. Whether one holds a failed unit is asked
		// of window at that unit's decision, under this lock, never once for
		// the step. A failed unit inside a window is recorded and not paged; with
		// none open it pages (failedRun). A unit that recovered has its older held
		// runs marked handled, so a stale redelivery of one never pages it, and,
		// once the window has closed, the hold is forgotten and note is given one
		// line saying it was held and recovered. A record newer than the sample,
		// or of a unit this instance does not sample, is left exactly as it is
		// (maintenance.go).
		changed := false
		for _, unit := range append(append([]string{}, units...), resolveOnly...) {
			sample := samples[unit]
			resolves := slices.Contains(resolveOnly, unit)
			fail := func(err error) {
				if err == nil {
					return
				}
				if !resolves {
					hardErrs = append(hardErrs, err)
					return
				}
				persist, other := splitPersist(err)
				if persist != nil {
					hardErrs = append(hardErrs, persist)
				}
				if other != nil {
					softErrs = append(softErrs, other)
				}
			}
			if resolves {
				now, err := current(unit)
				if err != nil {
					softErrs = append(softErrs, err)
					continue
				}
				sample = now
				if !sample.failed && sample.state != "inactive" && sample.state != "active" {
					// A retry in flight (activating): the failure is not over.
					continue
				}
			}
			if sample.failed {
				if sample.invocation == "" {
					sample.invocation = unnamedRun
				}
				if hold := holdFor(report.KindUnitFailed, window); !hold.IsZero() {
					holdAlert(u.sent, hold, at, report.UnitFailedKey(unit, sample.invocation))
					changed = true
					continue
				}
				// Still failed once the window is closed: the hold is forgotten and
				// the ordinary page follows, gated by nothing else.
				if len(takeHeld(u.sent, report.UnitFailedKeyPrefix(unit), time.Time{})) > 0 {
					changed = true
				}
				switch {
				case resolves && handledRun(u, unit, sample.invocation):
				case resolves:
					fail(failedRun(u, unit, sample.invocation, journalctl, secrets, senders, setAside, at, time.Time{}, every, policies...))
				default:
					fail(failedRun(u, unit, sample.invocation, journalctl, secrets, senders, setAside, at, sampledAt, every, policies...))
				}
				continue
			}
			// Only a sample newer than a held failure says anything about it.
			cutoff := sampledAt
			if resolves {
				cutoff = time.Time{}
			}
			for _, h := range heldOf(u.sent, report.UnitFailedKeyPrefix(unit)) {
				if !cutoff.IsZero() && !h.at.Before(cutoff) {
					continue
				}
				handleRuns(u, unit, h.alertKey, strings.TrimPrefix(h.alertKey, report.UnitFailedKeyPrefix(unit)), at)
				changed = true
			}
			if holdFor(report.KindUnitFailed, window).IsZero() {
				for _, h := range takeHeld(u.sent, report.UnitFailedKeyPrefix(unit), cutoff) {
					if note != nil {
						note(recoveredLine(unit, h.until))
					}
					changed = true
				}
			}
			for invocation, recorded := range unitRecords(u, unit) {
				key := report.UnitFailedKey(unit, invocation)
				if !resolves && (!recorded.Before(sampledAt) || !latestRun(u, key).Before(sampledAt)) {
					continue
				}
				handleRuns(u, unit, key, invocation, at)
				fail(resolveKey(u, key, senders, setAside, at))
			}
		}
		if changed {
			hardErrs = append(hardErrs, u.record())
		}
		return nil
	})
	return errors.Join(softErrs...), errors.Join(append(hardErrs, err)...)
}

// maxHandledRuns bounds the record of resolved failed runs kept per unit.
const maxHandledRuns = 16

// handledKey is where a resolved failed run of unit is recorded.
func handledKey(unit, invocation string) string { return channelKey("handled", unit+"/"+invocation) }

// handledRun is whether the failed run of unit is among the newest
// maxHandledRuns resolved ones: such a run does not page or open an episode
// again, however old the sample that names it (ADR 0019, "Alerting": each
// incident once). The de-duplication is bounded, not permanent: a run evicted
// by maxHandledRuns newer ones would page again if its hook were redelivered.
func handledRun(u *unitState, unit, invocation string) bool {
	return !u.sent[handledKey(unit, invocation)].IsZero()
}

// handleRuns records the failed runs of the episode keyed key (its first run
// and the latest later one noteLaterRun kept; intermediate runs are not
// remembered) as resolved, keeping the newest maxHandledRuns of unit, ties
// in time broken toward the greater invocation ID so eviction is
// deterministic (ADR 0019, "Alerting": each incident once, within that
// bound). It only changes u in memory: resolveKey's save then writes the record
// and the episode's removal together, so no stored state holds one without the
// other (ADR 0019: delivery state is durable).
func handleRuns(u *unitState, unit, key, episode string, at time.Time) {
	u.sent[handledKey(unit, episode)] = at
	for k := range u.sent {
		if run, ok := strings.CutPrefix(k, "run:"); ok && strings.HasSuffix(run, channelKey("", key)) {
			u.sent[handledKey(unit, strings.TrimSuffix(run, channelKey("", key)))] = at
		}
	}
	prefix := channelKey("handled", unit+"/")
	var mine []string
	for k := range u.sent {
		if strings.HasPrefix(k, prefix) {
			mine = append(mine, k)
		}
	}
	if len(mine) <= maxHandledRuns {
		return
	}
	sort.Slice(mine, func(i, j int) bool {
		if a, b := u.sent[mine[i]], u.sent[mine[j]]; !a.Equal(b) {
			return a.After(b)
		}
		return mine[i] > mine[j]
	})
	for _, k := range mine[maxHandledRuns:] {
		delete(u.sent, k)
	}
}

// persistError marks a failure to save the unit-failure record, which is
// never soft: delivery state must be durable (ADR 0019, "Alerting"; unitStep).
type persistError struct{ error }

func (e persistError) Unwrap() error { return e.error }

// splitPersist separates the persistence failures of err (a joined tree),
// which stay hard, from its other, delivery, failures, which are soft for the
// data job (ADR 0019, "Alerting": delivery state must be durable).
func splitPersist(err error) (persist, other error) {
	var ps, others []error
	var walk func(error)
	walk = func(e error) {
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, c := range j.Unwrap() {
				walk(c)
			}
			return
		}
		var pe persistError
		if errors.As(e, &pe) {
			ps = append(ps, e)
		} else {
			others = append(others, e)
		}
	}
	walk(err)
	return errors.Join(ps...), errors.Join(others...)
}

// unitRecords are the failed runs of unit that some channel took, each with
// the latest time a channel took it.
func unitRecords(u *unitState, unit string) map[string]time.Time {
	prefix := report.UnitFailedKeyPrefix(unit)
	runs := map[string]time.Time{}
	// Observation survives even if every channel failed or was held. Use its
	// actual time, never the future window end (alerts-and-dashboard.md,
	// "Paging invariant").
	for k, at := range u.sent {
		if ch, key, ok := strings.Cut(k, "|"); ok && ch == "observed" && strings.HasPrefix(key, prefix) {
			runs[strings.TrimPrefix(key, prefix)] = at
		}
	}
	for k, took := range u.seen {
		_, key, ok := strings.Cut(k, "|")
		if !took || !ok || !strings.HasPrefix(key, prefix) {
			continue
		}
		invocation := strings.TrimPrefix(key, prefix)
		if at := u.sent[k]; at.After(runs[invocation]) {
			runs[invocation] = at
		} else if _, known := runs[invocation]; !known {
			runs[invocation] = at
		}
	}
	return runs
}

// allTook reports whether every delivery channel has taken the alert.
func allTook(senders []sender, key string, seen map[string]bool) bool {
	for _, s := range senders {
		if _, echo := s.(echoOnly); !echo && !seen[channelKey(s.name(), key)] {
			return false
		}
	}
	return true
}

// clearUnit resolves and forgets the page of every failed run of unit but
// current (the reporter's own run, which is not failed while it runs), under
// the record's lock.
func clearUnit(path, unit, current string, senders []sender, setAside []channelFault, at time.Time) error {
	return withUnitState(path, at, func(u *unitState) error {
		var failures []error
		// The unit's own held failures are seen well: handled, not forgotten, so
		// a delayed hook for one of those runs is silent.
		if taken := takeHeld(u.sent, report.UnitFailedKeyPrefix(unit), time.Time{}); len(taken) > 0 {
			for _, h := range taken {
				handleRuns(u, unit, h.alertKey, strings.TrimPrefix(h.alertKey, report.UnitFailedKeyPrefix(unit)), at)
			}
			failures = append(failures, u.record())
		}
		for invocation := range unitRecords(u, unit) {
			if invocation == current {
				continue
			}
			if err := resolveKey(u, report.UnitFailedKey(unit, invocation), senders, setAside, at); err != nil {
				failures = append(failures, err)
			}
		}
		return errors.Join(failures...)
	})
}

// resolveKey closes a failure episode's incident on each working PagerDuty
// channel that took it, since PagerDuty folds a trigger into an open incident
// under its key, then forgets it on every channel; a PagerDuty channel set
// aside, or whose resolve failed, keeps its record until a run in which it
// works (as clockStep does), marked over (recoveredKey) so the next failed
// run begins a new episode. Forgetting drops the episode's own bookkeeping
// too (its runs, reminders and over marker), so an ended episode leaves
// only the one marker, set afresh, while a channel keeps its record.
func resolveKey(u *unitState, key string, senders []sender, setAside []channelFault, at time.Time) error {
	failedNames, resolveErr := resolveAll(senders, key, u.seen, at)
	keep := map[string]bool{}
	for _, f := range setAside {
		keep[f.channel] = f.channel == "pagerduty"
	}
	for _, name := range failedNames {
		keep[name] = true
	}
	forgetAlert(key, keep, u.seen, u.sent, u.repeats)
	if deliveredAnywhere(u.seen, key) {
		u.repeats[recoveredKey(key)] = 1
	}
	return errors.Join(resolveErr, u.record())
}
