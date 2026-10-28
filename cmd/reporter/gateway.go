package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/procgroup"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// The Gateway health check (deploy/libexec/gateway-check.sh) reads IBC's and
// the adapters' journals: exit 0 is logged in (or nothing to say), exit 1 is
// logged out, anything else is a check that could not tell. It is bounded as
// the clock check is (clock.go).
var (
	gatewayCheckTimeout   = 40 * time.Second
	gatewayCheckWaitDelay = 5 * time.Second
)

const (
	// gatewayPagesAfter is how long the Gateway must be seen logged out,
	// outside its restart windows, before it pages: three timer periods'
	// worth of margin over a login that is merely slow (ADR 0019, "Alerting").
	gatewayPagesAfter = 10 * time.Minute
	// gatewayRemindEvery is the reminder interval while it stays logged out.
	gatewayRemindEvery = time.Hour
)

// inGatewayRestartWindow reports whether t falls in a time the Gateway is
// documented to be logging in again: the nightly restart at 11:45 PM
// (23:40 to 00:30) and the Sunday 09:00 cold restart, which waits for the
// phone prompt (08:55 to 09:30), New York time (deploy/libexec/render-ibc-config.sh).
// The threshold counts from the window's end.
func inGatewayRestartWindow(t time.Time) bool {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return false
	}
	t = t.In(loc)
	mins := t.Hour()*60 + t.Minute()
	if mins >= 23*60+40 || mins < 30 {
		return true
	}
	return t.Weekday() == time.Sunday && mins >= 8*60+55 && mins < 9*60+30
}

// runGatewayCheck runs the check: out is true for exit 1. Any other failure,
// a missing command or a timeout is an error, not a logout.
func runGatewayCheck(command string) (out bool, said string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), gatewayCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return procgroup.Kill(cmd.Process.Pid) }
	cmd.WaitDelay = gatewayCheckWaitDelay
	raw, runErr := cmd.CombinedOutput()
	said = strings.TrimSpace(string(raw))
	if len(said) > maxClockOutput {
		said = said[:maxClockOutput]
	}
	var exit *exec.ExitError
	switch {
	case runErr == nil:
		return false, said, nil
	case ctx.Err() != nil:
		return false, said, fmt.Errorf("reporter: the gateway check timed out after %s", gatewayCheckTimeout)
	case errors.As(runErr, &exit) && exit.ExitCode() == 1:
		return true, said, nil
	default:
		return false, said, fmt.Errorf("reporter: the gateway check could not tell: %w (it said: %s)", runErr, said)
	}
}

// hostGateway is the host's record of the Gateway: the channels that took its
// page and since when it has been seen logged out.
type hostGateway struct {
	delivered
	Since *time.Time `json:"logged_out_since,omitempty"`
}

func loadHostGateway(path string) (hostGateway, error) {
	var h hostGateway
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err == nil {
		err = json.Unmarshal(raw, &h)
	}
	if err != nil {
		return h, fmt.Errorf("reporter: the gateway state %s is unreadable and is not reset, since a reset forgets a page already sent: %w", path, err)
	}
	return h, nil
}

// gatewayStep checks the Gateway under an exclusive lock on the host's
// gateway state, so a check is made by one reporter at a time (the caller
// runs it only for the owning instance). A logged-out Gateway is timed from
// the first firing that saw it, or from the end of a restart window it was
// seen in; it pages once the time passes gatewayPagesAfter outside a window,
// is reminded every gatewayRemindEvery, and a firing that finds it logged in
// resolves the page and forgets it, so the next outage pages anew.
func gatewayStep(path, command, host string, senders []sender, setAside []channelFault, clock func() time.Time) error {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("reporter: the gateway state's lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil
		}
		return fmt.Errorf("reporter: the gateway state's lock: %w", err)
	}
	h, err := loadHostGateway(path)
	if err != nil {
		return err
	}
	out, said, err := runGatewayCheck(command)
	if err != nil {
		return err
	}
	now := clock()
	seen, sent, repeats := map[string]bool{}, map[string]time.Time{}, map[string]int{}
	for _, k := range h.Keys {
		seen[k] = true
	}
	for k, at := range h.Sent {
		sent[k] = at
	}
	for k, n := range h.Repeats {
		repeats[k] = n
	}
	record := func() error {
		raw, err := json.Marshal(hostGateway{delivered: deliveredOf(seen, sent, repeats, nil), Since: h.Since})
		if err != nil {
			return err
		}
		return writeAtomic(path, raw)
	}
	if pruneReceipts(seen, sent, repeats, now) {
		if err := record(); err != nil {
			return err
		}
	}
	key := report.GatewayKey(host)
	if !out {
		h.Since = nil
		if !deliveredAnywhere(seen, key) {
			return record()
		}
		failed, resolveErr := resolveAll(senders, key, seen, now)
		keep := map[string]bool{}
		for _, f := range setAside {
			keep[f.channel] = f.channel == "pagerduty"
		}
		for _, name := range failed {
			keep[name] = true
		}
		forgetAlert(key, keep, seen, sent, repeats)
		return errors.Join(resolveErr, record())
	}
	paged := deliveredAnywhere(seen, key)
	if h.Since == nil || (!paged && inGatewayRestartWindow(now)) {
		t := now
		h.Since = &t
	}
	if err := record(); err != nil {
		return err
	}
	if inGatewayRestartWindow(now) || now.Sub(*h.Since) < gatewayPagesAfter {
		return nil
	}
	return deliver(senders, report.GatewayLoggedOutAlert(host, *h.Since, now, said), seen, sent, repeats, now, gatewayRemindEvery, record)
}
