package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
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
	// recoveredAt is the time of the newest positive event the journals show:
	// IBC's completed login when it is IBC's newest login event, or the
	// adapter's restored link or reconciliation. An open outage is closed only
	// by one newer than the outage's start, never by older events that remain
	// once the evidence of the outage has rotated away.
	recoveredAt time.Time
}

// gatewayChecker reads the evidence as of now.
type gatewayChecker func(now time.Time) (gatewayEvidence, error)

var (
	// ibcEvent matches IBC's login events and the start script's stop, which
	// cmd/gateway-start always logs with this phrase.
	// cmd/gateway-start logs "starting IBC" at every start and "cannot start
	// IBC" when it cannot, so a restart of the unit resets the login state
	// until a new "Login has completed"; IBC's own login screen after its
	// restart is "LoginState is LOGGED_OUT".
	ibcEvent = regexp.MustCompile(`Unrecognized Username or Password|LoginState is LOGGED_OUT|Login has completed|login retries stopped|gateway-start: starting IBC|gateway-start: cannot start IBC`)
	// The adapter's link lines (adapter/ibkr/connection.py _report and
	// loop.py): connectivity-lost is code 1100/2110; "connected reason=restored"
	// is IBKR's restoration (1101/1102); "reconciled" is a successful read of the
	// account after a connect, which needs IBKR. A plain "connected" (the API
	// handshake, logged at every reconnect, including every restore_timeout while
	// Gateway has no IBKR) proves nothing about IBKR and is not matched.
	linkEvent  = regexp.MustCompile(`ibkr link state=connectivity-lost|ibkr link state=connected reason=restored|ibkr reconciled client_id=`)
	lostAccess = regexp.MustCompile(`(?i)not seeing messages from other users|insufficient permissions|no journal files were opened`)
	shortISO   = "2006-01-02T15:04:05-0700" // fractional seconds are accepted when parsing
)

// journalQuery returns the lines of a unit's journal that match the pattern,
// oldest first. A command that fails, or output that shows the reader lacks
// access (journalctl prints a hint, with a zero status, when the user is not
// in the systemd-journal group), is an error.
func journalQuery(journalctl, unit string, pattern *regexp.Regexp) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gatewayCheckTimeout)
	defer cancel()
	//nolint:gosec // G204: the binary is the operator's -journalctl flag; the unit is a fixed name or one -gateway-adapters named, and the pattern a fixed expression.
	cmd := exec.CommandContext(ctx, journalctl, "-u", unit, "-o", "short-iso-precise", "--no-pager", "--since="+gatewayJournalSince, "-g", pattern.String())
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

// lineTime reads the time journalctl -o short-iso puts first on a line.
func lineTime(l string) (time.Time, error) {
	field, _, _ := strings.Cut(l, " ")
	at, err := time.Parse(shortISO, field)
	if err != nil {
		// systemd 255 and later write the offset with a colon.
		at, err = time.Parse(time.RFC3339, field)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("no time in %q: %w", l, err)
	}
	return at, nil
}

// gatewayJournalChecker reads IBC's journal and each adapter's.
func gatewayJournalChecker(journalctl string, adapters []string) gatewayChecker {
	return func(now time.Time) (gatewayEvidence, error) {
		ibc, err := journalQuery(journalctl, gatewayUnit, ibcEvent)
		if err != nil {
			return gatewayEvidence{}, err
		}
		var reasons []string
		var recovered time.Time
		if n := len(ibc); n > 0 {
			if strings.Contains(ibc[n-1], "Login has completed") {
				at, err := lineTime(ibc[n-1])
				if err != nil {
					return gatewayEvidence{}, fmt.Errorf("reporter: the gateway check could not read IBC's login time: %w", err)
				}
				recovered = at
			} else {
				reasons = append(reasons, "IBC's newest login event: "+ibc[n-1])
			}
		}
		for _, unit := range adapters {
			lines, err := journalQuery(journalctl, unit, linkEvent)
			if err != nil {
				return gatewayEvidence{}, err
			}
			since, line, restored, err := lostSince(lines)
			if err != nil {
				return gatewayEvidence{}, fmt.Errorf("reporter: the gateway check could not read %s's link events: %w", unit, err)
			}
			if restored.After(recovered) {
				recovered = restored
			}
			if !since.IsZero() && now.Sub(since) >= gatewayLinkLossAfter {
				reasons = append(reasons, fmt.Sprintf("%s has reported IBKR's servers lost (1100/2110) with no restoration since, for over %s: %s", unit, gatewayLinkLossAfter, line))
			}
		}
		switch {
		case len(reasons) > 0:
			return gatewayEvidence{out: true, said: "gateway: logged out: " + strings.Join(reasons, "; "), recoveredAt: recovered}, nil
		case len(ibc) == 0:
			return gatewayEvidence{silent: true, said: "gateway: no login evidence: the ti-gateway journal holds no login event in the last 14 days", recoveredAt: recovered}, nil
		}
		return gatewayEvidence{said: "gateway: logged in (IBC's newest login event is a completed login)", recoveredAt: recovered}, nil
	}
}

// lostSince finds when the current uninterrupted loss began: the first
// connectivity-lost line after the newest restoration or reconciliation (zero
// when there is none, or nothing is lost), that line, and the newest
// restoration's time.
func lostSince(lines []string) (since time.Time, first string, restored time.Time, err error) {
	for _, l := range lines {
		at, err := lineTime(l)
		if err != nil {
			return time.Time{}, "", time.Time{}, err
		}
		if strings.Contains(l, "state=connectivity-lost") {
			if since.IsZero() {
				since, first = at, l
			}
			continue
		}
		since, first, restored = time.Time{}, "", at
	}
	return since, first, restored, nil
}
