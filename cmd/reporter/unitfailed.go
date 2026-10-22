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
// adapter or the reporter of one account, nothing else, so no value can be
// read as an option or name another unit (ADR 0019, "Alerting"; ADR 0027,
// the restart model).
var tradingUnit = regexp.MustCompile(`^ti-(engine@|adapter-|reporter@)[a-z0-9][a-z0-9-]*\.service$`)

// invocationShape is systemd's invocation ID: 32 lowercase hex digits.
var invocationShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

// checkTradingUnits refuses any name that is not a trading unit's, before
// systemctl or journalctl is run on it.
func checkTradingUnits(flagName string, units []string) error {
	for _, unit := range units {
		if !tradingUnit.MatchString(unit) {
			return fmt.Errorf("reporter: %s %q is not a trading unit's name (ti-engine@<account>.service, ti-adapter-<account>.service or ti-reporter@<account>.service)", flagName, unit)
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
	return saveState(u.path, u.seen, u.sent, u.repeats, u.baselines)
}

// withUnitState runs fn on the unit-failure record under an exclusive lock.
func withUnitState(path string, fn func(*unitState) error) error {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("reporter: the unit-failure record's lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("reporter: the unit-failure record's lock: %w", err)
	}
	seen, sent, repeats, baselines, err := loadState(path)
	if err != nil {
		return err
	}
	return fn(&unitState{path: path, seen: seen, sent: sent, repeats: repeats, baselines: baselines})
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
	channels := flags.String("channel", "ntfy,pagerduty", "comma-separated channels: ntfy, pagerduty")
	secretsFile := flags.String("secrets-file", "", "KEY=VALUE file of channel secrets (under systemd: %d/secrets, the credential file)")
	journalctl := flags.String("journalctl", "journalctl", "the journal reader")
	systemctl := flags.String("systemctl", "systemctl", "the systemctl that names the failed run when systemd did not (MONITOR_INVOCATION_ID)")
	sendTimeout := flags.Duration("send-timeout", 30*time.Second, "longest one send to one channel may take before it counts as failed")
	repeat := flags.Duration("repeat-interval", defaultRepeatInterval, "unused by a page that does not repeat; kept so the hook and the reporter share one delivery code")
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
	var failures []error
	secrets := secretValues(getenv, *secretsFile)
	if *secretsFile != "" {
		var secretsErr error
		if getenv, secretsErr = withSecretsFile(getenv, *secretsFile); secretsErr != nil {
			failures = append(failures, secretsErr)
		}
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
	at := now()
	run := failedInvocation(getenv, *systemctl, unit, at)
	err = withUnitState(*statePath, func(u *unitState) error {
		return failedRun(u, unit, run, *journalctl, secrets, senders, setAside, at, *repeat)
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
// resolved again and the run pages afresh.
func failedRun(u *unitState, unit, run, journalctl string, secrets []string, senders []sender, setAside []channelFault, at time.Time, every time.Duration) error {
	var errs []error
	episode, opened := "", time.Time{}
	for invocation, recorded := range unitRecords(u, unit) {
		key := report.UnitFailedKey(unit, invocation)
		if u.repeats[recoveredKey(key)] > 0 {
			errs = append(errs, resolveKey(u, key, senders, setAside))
			continue
		}
		if episode == "" || recorded.Before(opened) || (recorded.Equal(opened) && invocation < episode) {
			episode, opened = invocation, recorded
		}
	}
	if episode == "" || episode == run {
		return errors.Join(append(errs, pageRun(u, unit, run, journalctl, secrets, senders, at, every))...)
	}
	// A channel that has not yet taken the episode's page is sent it first.
	errs = append(errs, pageRun(u, unit, episode, journalctl, secrets, senders, at, every))
	return errors.Join(append(errs, remindRun(u, unit, episode, run, senders, at))...)
}

// recoveredKey marks an episode whose unit was seen well while a channel kept
// its record because the resolve failed there: the episode is over.
func recoveredKey(key string) string { return channelKey("recovered", key) }

// failedRunKey records run as a later failed run of the episode keyed key,
// so its reminder counts the runs.
func failedRunKey(run, key string) string { return channelKey("run:"+run, key) }

// remindedKey is when channel was last reminded of the episode keyed key.
func remindedKey(channel, key string) string { return channelKey("reminded:"+channel, key) }

// remindRun counts run, a later failed run of the episode begun by episode,
// and reminds each channel that took the episode's page an
// unitReminderInterval or more since its page or its last reminder: "still
// failing since <time>, N failed runs", under the episode's key. A channel
// whose reminder fails is reminded at the next failed run.
func remindRun(u *unitState, unit, episode, run string, senders []sender, at time.Time) error {
	key := report.UnitFailedKey(unit, episode)
	if u.sent[failedRunKey(run, key)].IsZero() {
		u.sent[failedRunKey(run, key)] = at
	}
	runs, since := 1, time.Time{}
	for k := range u.sent {
		if strings.HasPrefix(k, "run:") && strings.HasSuffix(k, channelKey("", key)) {
			runs++
		}
	}
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
		if at.Sub(last) >= unitReminderInterval {
			due = append(due, s)
		}
	}
	var errs []error
	if len(due) > 0 {
		a := report.UnitStillFailedAlert(hostName(), unit, episode, since, runs, at)
		for _, s := range echoes {
			_ = s.send(a, 0)
		}
		for _, s := range due {
			if err := s.send(a, 0); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", s.name(), err))
				continue
			}
			u.sent[remindedKey(s.name(), key)] = at
		}
	}
	return errors.Join(append(errs, u.record())...)
}

// pageRun pages the failed run of unit on each channel that has not taken it.
func pageRun(u *unitState, unit, invocation, journalctl string, secrets []string, senders []sender, at time.Time, every time.Duration) error {
	key := report.UnitFailedKey(unit, invocation)
	if allTook(senders, key, u.seen) {
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
	return deliver(senders, a, u.seen, u.sent, u.repeats, at, every, record)
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
	for _, k := range []string{"TI_NTFY_TOPIC", "TI_NTFY_SERVER", "TI_PAGERDUTY_ROUTING_KEY", "TI_PAGERDUTY_URL", "TI_HEARTBEAT_URL"} {
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
	invocation string
}

// sampleUnits asks systemd for the state and invocation of each unit
// (`systemctl show -p ActiveState -p InvocationID`). A systemctl that cannot
// be run, or does not answer, is an error, never "none failed": an unchecked
// unit is not a healthy one.
func sampleUnits(systemctl string, units []string) (map[string]unitSample, error) {
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
			failures = append(failures, fmt.Errorf("reporter: systemctl show %s could not be run: the trading units were not checked", unit))
			continue
		}
		samples[unit] = unitSample{failed: state == "failed", invocation: props["InvocationID"]}
	}
	return samples, errors.Join(failures...)
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
// answers at the start of the firing, sampledAt their time: a page recorded
// since then is of a run newer than the sample, so it is never resolved on the
// sample's word. Both reporters and the hook share the record under a lock.
func unitStep(path string, units []string, samples map[string]unitSample, sampledAt time.Time, journalctl string, secrets []string, senders []sender, setAside []channelFault, at time.Time, every time.Duration) error {
	return withUnitState(path, func(u *unitState) error {
		var failures []error
		for _, unit := range units {
			sample := samples[unit]
			if sample.failed {
				if sample.invocation == "" {
					sample.invocation = "unnamed"
				}
				if err := failedRun(u, unit, sample.invocation, journalctl, secrets, senders, setAside, at, every); err != nil {
					failures = append(failures, err)
				}
				continue
			}
			for invocation, recorded := range unitRecords(u, unit) {
				if !recorded.Before(sampledAt) {
					continue
				}
				if err := resolveKey(u, report.UnitFailedKey(unit, invocation), senders, setAside); err != nil {
					failures = append(failures, err)
				}
			}
		}
		return errors.Join(failures...)
	})
}

// unitRecords are the failed runs of unit that some channel took, each with
// the latest time a channel took it.
func unitRecords(u *unitState, unit string) map[string]time.Time {
	prefix := report.UnitFailedKeyPrefix(unit)
	runs := map[string]time.Time{}
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
func clearUnit(path, unit, current string, senders []sender, setAside []channelFault) error {
	return withUnitState(path, func(u *unitState) error {
		var failures []error
		for invocation := range unitRecords(u, unit) {
			if invocation == current {
				continue
			}
			if err := resolveKey(u, report.UnitFailedKey(unit, invocation), senders, setAside); err != nil {
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
// run begins a new episode.
func resolveKey(u *unitState, key string, senders []sender, setAside []channelFault) error {
	failedNames, resolveErr := resolveAll(senders, key, u.seen)
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
