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
// failed run's reason and the manual row, keyed by the failed run. A run
// already paged is not paged again; the page of an earlier run of the unit is
// resolved first. A channel that fails fails the command, so the
// hook unit shows failed, and only that channel is tried again by the next
// reporter firing (ADR 0019, "Alerting").
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
		// A new failed run means every earlier run of the unit is over: its
		// incident is resolved before the new one is paged.
		var errs []error
		for invocation := range unitRecords(u, unit) {
			if invocation != run {
				errs = append(errs, resolveKey(u, report.UnitFailedKey(unit, invocation), senders, setAside))
			}
		}
		errs = append(errs, pageRun(u, unit, run, *journalctl, secrets, senders, at, *repeat))
		return errors.Join(errs...)
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
// from PID 1). It is never a line from an earlier run.
func unitReason(journalctl, unit, invocation string, secrets []string) string {
	if !invocationShape.MatchString(invocation) {
		return "(systemd named no invocation for the failed run, so its journal lines were not read)"
	}
	own, err := unitCommand(journalctl, "_SYSTEMD_UNIT="+unit, "_SYSTEMD_INVOCATION_ID="+invocation, "-n", "1", "-o", "cat", "--no-pager")
	if err != nil {
		return "(the unit's journal could not be read)"
	}
	if strings.TrimSpace(own) != "" {
		return report.UnitFailedReason(own, secrets)
	}
	systemd, err := unitCommand(journalctl, "_PID=1", "UNIT="+unit, "INVOCATION_ID="+invocation, "-n", "50", "-o", "cat", "--no-pager")
	if err != nil {
		return "(the unit's journal could not be read)"
	}
	for _, line := range strings.Split(systemd, "\n") {
		if strings.Contains(line, "Failed") {
			return report.UnitFailedReason(line, secrets)
		}
	}
	return report.UnitFailedReason("", secrets)
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
// not hand to OnFailure=) and resolves the page of every run that is over.
// samples are systemd's answers at the start of the firing, sampledAt their
// time: a page recorded since then is of a run newer than the sample, so it is
// never resolved on the sample's word. Both reporters and the hook share the
// record under a lock.
func unitStep(path string, units []string, samples map[string]unitSample, sampledAt time.Time, journalctl string, secrets []string, senders []sender, setAside []channelFault, at time.Time, every time.Duration) error {
	return withUnitState(path, func(u *unitState) error {
		var failures []error
		for _, unit := range units {
			sample := samples[unit]
			if sample.failed && sample.invocation == "" {
				sample.invocation = "unnamed"
			}
			for invocation, recorded := range unitRecords(u, unit) {
				if (sample.failed && invocation == sample.invocation) || !recorded.Before(sampledAt) {
					continue
				}
				if err := resolveKey(u, report.UnitFailedKey(unit, invocation), senders, setAside); err != nil {
					failures = append(failures, err)
				}
			}
			if sample.failed {
				if err := pageRun(u, unit, sample.invocation, journalctl, secrets, senders, at, every); err != nil {
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

// resolveKey closes a failed run's incident on each working PagerDuty channel
// that took it, since PagerDuty folds a trigger into an open incident under
// its key, then forgets it on every channel; a PagerDuty channel set aside,
// or whose resolve failed, keeps its record until a run in which it works (as
// clockStep does).
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
	return errors.Join(resolveErr, u.record())
}
