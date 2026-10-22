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
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// unitCommandTimeout bounds each systemctl and journalctl call, so a hung one
// never holds back a page.
const unitCommandTimeout = 15 * time.Second

// unitName is the shape of a unit name the hook accepts: no leading dash (an
// option), no slash, space or shell character.
var unitName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.\\@-]*$`)

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
// fails: one urgent page through the reporter's channels, naming the unit, its
// last journal line and the manual row. A unit already paged and not yet
// resolved is not paged again. A channel that fails fails the command, so the
// hook unit shows failed, and only that channel is tried again by the next
// reporter firing (ADR 0019, "Alerting").
func notifyUnitFailed(args []string, getenv func(string) string, now func() time.Time, out io.Writer, client *http.Client) error {
	flags := flag.NewFlagSet("reporter notify-unit-failed", flag.ContinueOnError)
	flags.SetOutput(out)
	statePath := flags.String("state", "", "the host's record of failed-unit pages (unit-failures.json beside the reporters' state files)")
	channels := flags.String("channel", "ntfy,pagerduty", "comma-separated channels: ntfy, pagerduty")
	secretsFile := flags.String("secrets-file", "", "KEY=VALUE file of channel secrets (under systemd: %d/secrets, the credential file)")
	journalctl := flags.String("journalctl", "journalctl", "the journal reader")
	sendTimeout := flags.Duration("send-timeout", 30*time.Second, "longest one send to one channel may take before it counts as failed")
	repeat := flags.Duration("repeat-interval", defaultRepeatInterval, "unused by a page that does not repeat; kept so the hook and the reporter share one delivery code")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *statePath == "" || flags.NArg() != 1 || !unitName.MatchString(flags.Arg(0)) {
		return errors.New("reporter: notify-unit-failed needs -state and exactly one unit name, such as ti-adapter-boost.service")
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
	err = withUnitState(*statePath, func(u *unitState) error {
		return deliver(senders, report.UnitFailedAlert(hostName(), unit, unitReason(*journalctl, unit, secrets), at), u.seen, u.sent, u.repeats, at, *repeat, u.record)
	})
	return errors.Join(append(failures, err)...)
}

// unitReason is the unit's own last journal line, cleaned for a page. It asks
// for the unit's own output (_SYSTEMD_UNIT), not systemd's messages about it,
// whose last line only says the unit failed.
func unitReason(journalctl, unit string, secrets []string) string {
	ctx, cancel := context.WithTimeout(context.Background(), unitCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, journalctl, "_SYSTEMD_UNIT="+unit, "-n", "1", "-o", "cat", "--no-pager")
	cmd.WaitDelay = time.Second
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "(the unit's journal could not be read)"
	}
	return report.UnitFailedReason(stdout.String(), secrets)
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

// failedUnits asks systemd which of units are failed (`systemctl is-failed`
// exits 0 for a failed unit). A systemctl that cannot be run at all is an
// error, never "none failed": an unchecked unit is not a healthy one.
func failedUnits(systemctl string, units []string) (map[string]bool, error) {
	failed := map[string]bool{}
	var failures []error
	for _, unit := range units {
		ctx, cancel := context.WithTimeout(context.Background(), unitCommandTimeout)
		cmd := exec.CommandContext(ctx, systemctl, "is-failed", unit)
		cmd.WaitDelay = time.Second
		err := cmd.Run()
		cancel()
		var exit *exec.ExitError
		switch {
		case err == nil:
			failed[unit] = true
		case errors.As(err, &exit) && !errors.Is(ctx.Err(), context.DeadlineExceeded):
		default:
			failures = append(failures, fmt.Errorf("reporter: systemctl is-failed %s could not be run: the trading units were not checked", unit))
		}
	}
	return failed, errors.Join(failures...)
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

// unitStep pages each failed trading unit that has not been paged (the hook
// does this at the moment of failure; this catches a hook that could not
// deliver, and a unit that failed before the hook was installed) and resolves
// the page of each unit that is no longer failed, so its next failure pages
// again. Both reporters and the hook share the record under a lock.
func unitStep(path string, units []string, failed map[string]bool, journalctl string, secrets []string, senders []sender, setAside []channelFault, at time.Time, every time.Duration) error {
	return withUnitState(path, func(u *unitState) error {
		var failures []error
		for _, unit := range units {
			key := report.UnitFailedKey(unit)
			switch {
			case failed[unit]:
				if allTook(senders, key, u.seen) {
					continue
				}
				a := report.UnitFailedAlert(hostName(), unit, unitReason(journalctl, unit, secrets), at)
				if err := deliver(senders, a, u.seen, u.sent, u.repeats, at, every, u.record); err != nil {
					failures = append(failures, err)
				}
			case deliveredAnywhere(u.seen, key):
				if err := resolveUnit(u, unit, senders, setAside); err != nil {
					failures = append(failures, err)
				}
			}
		}
		return errors.Join(failures...)
	})
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

// clearUnit resolves and forgets the page of a unit, under the record's lock.
func clearUnit(path, unit string, senders []sender, setAside []channelFault) error {
	return withUnitState(path, func(u *unitState) error {
		if !deliveredAnywhere(u.seen, report.UnitFailedKey(unit)) {
			return nil
		}
		return resolveUnit(u, unit, senders, setAside)
	})
}

// resolveUnit closes a unit's incident on each working PagerDuty channel that
// took it, since PagerDuty folds a trigger into an open incident under its key
// and the unit's next failure would otherwise be silent, then forgets it on
// every channel; a PagerDuty channel set aside, or whose resolve failed, keeps
// its record until a run in which it works (as clockStep does).
func resolveUnit(u *unitState, unit string, senders []sender, setAside []channelFault) error {
	key := report.UnitFailedKey(unit)
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
