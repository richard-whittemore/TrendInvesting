package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/procgroup"
)

// The Gateway's journals say whether it is logged in (ADR 0019, "Alerting").
// IBC's, ti-gateway, holds the login events; the adapter's holds the link
// state. The reader fails closed: a journal it cannot read is an error that
// fails the firing, never a login.

// The journal read is bounded as the clock check is (clock.go); tests shorten it.
var (
	gatewayCheckTimeout   = 40 * time.Second
	gatewayCheckWaitDelay = 5 * time.Second
)

const (
	gatewayUnit = "ti-gateway.service"
	// gatewayJournalSince bounds how far back the journals are read.
	gatewayJournalSince = "-14d"
	// gatewayLinkLossAfter is how long an adapter's link to IBKR must have been
	// lost, without one connection since, to count as the Gateway being logged
	// out on its own evidence: IBKR's scheduled server resets and weekend
	// maintenance drop the link for shorter spells while the Gateway is logged
	// in, and a login failure shows in IBC's journal at once. A session dropped
	// silently on IBKR's side with no IBC event therefore pages only after
	// about this time plus gatewayPagesAfter.
	gatewayLinkLossAfter = 3 * time.Hour
)

// gatewayEvidence is what the journals show: logged out (out), or that they
// hold no login event at all (silent), or neither (logged in).
type gatewayEvidence struct {
	out, silent bool
	said        string
}

// gatewayChecker reads the evidence as of now.
type gatewayChecker func(now time.Time) (gatewayEvidence, error)

var (
	// ibcEvent matches IBC's login events and the start script's stop, which
	// cmd/gateway-start always logs with this phrase.
	ibcEvent   = regexp.MustCompile(`Unrecognized Username or Password|LoginState is LOGGED_OUT|Login has completed|login retries stopped`)
	linkEvent  = regexp.MustCompile(`ibkr link state=(connectivity-lost|connected)`)
	lostAccess = regexp.MustCompile(`(?i)not seeing messages from other users|insufficient permissions|no journal files were opened`)
	shortISO   = "2006-01-02T15:04:05-0700"
)

// journalQuery returns the lines of a unit's journal that match the pattern,
// oldest first. A command that fails, or output that shows the reader lacks
// access (journalctl prints a hint, with a zero status, when the user is not
// in the systemd-journal group), is an error.
func journalQuery(journalctl, unit string, pattern *regexp.Regexp) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gatewayCheckTimeout)
	defer cancel()
	//nolint:gosec // G204: the binary is the operator's -journalctl flag; the unit is a fixed name or one -gateway-adapters named, and the pattern a fixed expression.
	cmd := exec.CommandContext(ctx, journalctl, "-u", unit, "-o", "short-iso", "--no-pager", "--since="+gatewayJournalSince, "-g", pattern.String())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return procgroup.Kill(cmd.Process.Pid) }
	cmd.WaitDelay = gatewayCheckWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	said := strings.TrimSpace(stderr.String())
	if lostAccess.MatchString(stdout.String()) || lostAccess.MatchString(said) {
		return nil, fmt.Errorf("reporter: the gateway check could not read the %s journal: journalctl says this user cannot see it (is the reporter in the systemd-journal group?): %s", unit, strings.TrimSpace(stdout.String()+" "+said))
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return nil, fmt.Errorf("reporter: the gateway check timed out after %s reading the %s journal", gatewayCheckTimeout, unit)
	case errors.As(err, &exit) && exit.ExitCode() == 1 && said == "":
		// journalctl -g exits 1 when nothing matches.
	default:
		return nil, fmt.Errorf("reporter: the gateway check could not read the %s journal: %w (it said: %s)", unit, err, said)
	}
	var lines []string
	for _, l := range strings.Split(stdout.String(), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "-- ") || !pattern.MatchString(l) {
			continue
		}
		lines = append(lines, l)
	}
	return lines, nil
}

// gatewayJournalChecker reads IBC's journal and each adapter's.
func gatewayJournalChecker(journalctl string, adapters []string) gatewayChecker {
	return func(now time.Time) (gatewayEvidence, error) {
		ibc, err := journalQuery(journalctl, gatewayUnit, ibcEvent)
		if err != nil {
			return gatewayEvidence{}, err
		}
		var reasons []string
		if n := len(ibc); n > 0 && !strings.Contains(ibc[n-1], "Login has completed") {
			reasons = append(reasons, "IBC's newest login event: "+ibc[n-1])
		}
		for _, unit := range adapters {
			lines, err := journalQuery(journalctl, unit, linkEvent)
			if err != nil {
				return gatewayEvidence{}, err
			}
			since, line, err := lostSince(lines)
			if err != nil {
				return gatewayEvidence{}, fmt.Errorf("reporter: the gateway check could not read %s's link events: %w", unit, err)
			}
			if !since.IsZero() && now.Sub(since) >= gatewayLinkLossAfter {
				reasons = append(reasons, fmt.Sprintf("%s has reported IBKR's servers lost (1100/2110) without a connection since, for over %s: %s", unit, gatewayLinkLossAfter, line))
			}
		}
		switch {
		case len(reasons) > 0:
			return gatewayEvidence{out: true, said: "gateway: logged out: " + strings.Join(reasons, "; ")}, nil
		case len(ibc) == 0:
			return gatewayEvidence{silent: true, said: "gateway: no login evidence: the ti-gateway journal holds no login event in the last 14 days"}, nil
		}
		return gatewayEvidence{said: "gateway: logged in (IBC's newest login event is a completed login)"}, nil
	}
}

// lostSince finds when the current uninterrupted loss began: the first
// connectivity-lost line after the newest connected one (zero when the link is
// connected, or the journal shows nothing), and that line.
func lostSince(lines []string) (time.Time, string, error) {
	var since time.Time
	var first string
	for _, l := range lines {
		field, _, _ := strings.Cut(l, " ")
		at, err := time.Parse(shortISO, field)
		if err != nil {
			// systemd 255 and later write the offset with a colon.
			at, err = time.Parse(time.RFC3339, field)
		}
		if err != nil {
			return time.Time{}, "", fmt.Errorf("no time in %q: %w", l, err)
		}
		if strings.Contains(l, "state=connected") {
			since, first = time.Time{}, ""
		} else if since.IsZero() {
			since, first = at, l
		}
	}
	return since, first, nil
}
