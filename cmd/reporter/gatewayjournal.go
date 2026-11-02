package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The Gateway's journals say whether it is logged in (ADR 0019, "Alerting").
// IBC's, ti-gateway, holds the login events; the adapter's holds the link
// state. An unreadable adapter journal cannot suppress an IBC-established
// outage; the firing returns its error after deciding and delivering the outage.
// An unreadable journal never implies a login (docs/manual/server.md,
// "The Gateway is logged out").

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
	// lost in one invocation, without restoration or reconciliation, to count as the Gateway being logged
	// out on its own evidence: IBKR's scheduled server resets and weekend
	// maintenance drop the link for shorter spells while the Gateway is logged
	// in, and a login failure shows in IBC's journal at once. A session dropped
	// silently on IBKR's side with no IBC event therefore pages only after
	// about this time plus gatewayPagesAfter (docs/manual/server.md,
	// "The Gateway is logged out"; ADR 0019, "Alerting").
	gatewayLinkLossAfter = 3 * time.Hour
)

// gatewayEvidence is what the journals show: logged out (out), or that they
// hold no login event at all (silent), or neither (logged in).
type gatewayEvidence struct {
	out, silent bool
	resetWait   bool
	said        string
	// ibkrUnreachable selects wording only, from this firing's evidence; it
	// never changes detection or persists (docs/manual/server.md, "Gateway healer").
	ibkrUnreachable bool
	// recoveredAt is the time of the newest positive event the journals show:
	// IBC's completed login when it is IBC's newest login event, or the
	// adapter's restored link or reconciliation. An open outage is closed only
	// by one newer than the outage's start, never by older events that remain
	// once the evidence of the outage has rotated away.
	recoveredAt time.Time
}

// gatewayChecker reads the evidence as of now. An IBC-established outage may
// accompany an adapter read error: decide the outage before returning that fault
// (docs/manual/server.md, "The Gateway is logged out"; ADR 0019, "Alerting").
type gatewayChecker func(now time.Time) (gatewayEvidence, error)

var (
	// ibcEvent matches IBC's login events and the start script's stop, which
	// cmd/gateway-start always logs with this phrase.
	// cmd/gateway-start logs "starting IBC" at every start and "cannot start
	// IBC" when it cannot, so a restart of the unit resets the login state
	// until a new "Login has completed"; IBC's own login screen after its
	// restart is "LoginState is LOGGED_OUT".
	ibcEvent = regexp.MustCompile(`Unrecognized Username or Password|LoginState is LOGGED_OUT|Login has completed|login retries stopped|gateway-start: starting IBC|gateway-start: cannot start IBC`)
	// resetWaitEvent reads reset-wait evidence separately from login events
	// (ADR 0019, "Alerting"); other start-script messages cannot imply logout.
	resetWaitEvent = regexp.MustCompile(`gateway-start: reset wait:`)
	// Adapter starts end preceding loss evidence; only losses
	// in the current invocation count (docs/manual/server.md, "The Gateway is
	// logged out"; ADR 0019, "Alerting"). Within an invocation, only restoration
	// (1101/1102) or reconciliation ends loss. Plain "connected" lines are API
	// handshakes to the local Gateway, not IBKR recovery: during an IBKR outage
	// the adapter reconnects every restore_timeout (five minutes), and the
	// Gateway immediately reports 1100/2110 again. Clearing loss at each
	// handshake would restart the three-hour clock forever and prevent paging.
	linkEvent  = regexp.MustCompile(`ibkr adapter starting|ibkr link connected|ibkr link state=connectivity-lost|ibkr link state=connected|ibkr reconciled client_id=`)
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
		resets, err := journalQuery(journalctl, gatewayUnit, resetWaitEvent)
		if err != nil {
			return gatewayEvidence{}, err
		}
		// A reset wait must be strictly newer than every completed-login
		// timestamp (ADR 0019, "Alerting"). Login events retain journal order;
		// wall-clock changes must not let an older reset override a login.
		var newestLogin time.Time
		for _, line := range ibc {
			if strings.Contains(line, "Login has completed") {
				at, err := lineTime(line)
				if err != nil {
					return gatewayEvidence{}, fmt.Errorf("reporter: the gateway check could not read IBC's login time: %w", err)
				}
				if at.After(newestLogin) {
					newestLogin = at
				}
			}
		}
		// Insert qualifying reset evidence by timestamp; unrelated
		// gateway-start diagnostics never participate in login state.
		for _, reset := range resets {
			at, err := lineTime(reset)
			if err != nil {
				return gatewayEvidence{}, fmt.Errorf("reporter: reset wait time: %w", err)
			}
			if !at.After(newestLogin) {
				continue
			}
			i := len(ibc)
			for i > 0 {
				previous, err := lineTime(ibc[i-1])
				if err != nil {
					return gatewayEvidence{}, err
				}
				if !previous.After(at) {
					break
				}
				i--
			}
			ibc = append(ibc, "")
			copy(ibc[i+1:], ibc[i:])
			ibc[i] = reset
		}
		var reasons []string
		var recovered time.Time
		resetWait := false
		for i := len(ibc) - 1; i >= 0; i-- {
			if strings.Contains(ibc[i], "gateway-start:") {
				loc, err := loadNewYork()
				if err != nil {
					return gatewayEvidence{}, err
				}
				if strings.Contains(ibc[i], "gateway-start: reset wait: login refused at") {
					at, err := lineTime(ibc[i])
					if err != nil {
						return gatewayEvidence{}, fmt.Errorf("reporter: reset wait time: %w", err)
					}
					end := resetNightEnd(at.In(loc))
					resetWait = !now.Before(at) && now.Before(end)
				}
				break
			}
		}
		if n := len(ibc); n > 0 {
			if strings.Contains(ibc[n-1], "Login has completed") {
				at, err := lineTime(ibc[n-1])
				if err != nil {
					return gatewayEvidence{}, fmt.Errorf("reporter: the gateway check could not read IBC's login time: %w", err)
				}
				recovered = at
				resetWait = false
			} else {
				reasons = append(reasons, "IBC's newest login event: "+ibc[n-1])
			}
		}
		ibcOut := len(reasons) > 0
		var diagnostics []string
		var readErr error
		for _, unit := range adapters {
			lines, err := journalQuery(journalctl, unit, linkEvent)
			if err != nil {
				if !ibcOut {
					return gatewayEvidence{}, err
				}
				readErr = errors.Join(readErr, err)
				diagnostics = append(diagnostics, err.Error())
				continue
			}
			since, line, restored, err := lostSince(lines)
			if err != nil {
				err = fmt.Errorf("reporter: the gateway check could not read %s's link events: %w", unit, err)
				if !ibcOut {
					return gatewayEvidence{}, err
				}
				readErr = errors.Join(readErr, err)
				diagnostics = append(diagnostics, err.Error())
				continue
			}
			if restored.After(recovered) {
				recovered = restored
			}
			if !since.IsZero() && now.Sub(since) >= gatewayLinkLossAfter {
				reasons = append(reasons, fmt.Sprintf("%s has reported IBKR's servers lost (1100/2110) with no restoration since, for over %s: %s", unit, gatewayLinkLossAfter, line))
			}
		}
		ev := gatewayEvidence{recoveredAt: recovered}
		switch {
		case len(reasons) > 0:
			ev.out, ev.resetWait = true, resetWait
			ev.ibkrUnreachable = !ibcOut && len(ibc) > 0
			status := "logged out"
			if ev.ibkrUnreachable {
				status = "logged in but IBKR's servers are unreachable"
			}
			ev.said = "gateway: " + status + ": " + strings.Join(reasons, "; ")
		case len(ibc) == 0:
			ev.silent = true
			ev.said = "gateway: no login evidence: the ti-gateway journal holds no login event in the last 14 days"
		default:
			ev.said = "gateway: logged in (IBC's newest login event is a completed login)"
		}
		if len(diagnostics) > 0 {
			ev.said += "; " + strings.Join(diagnostics, "; ")
		}
		return ev, readErr
	}
}

// lostSince returns the current invocation's uninterrupted loss, its first
// line, and the newest restoration/reconciliation time (docs/manual/server.md,
// "The Gateway is logged out"; ADR 0019, "Alerting"). Lines must be filtered
// by linkEvent in journal order. Starts end loss evidence without inventing
// positive recovery evidence; repeated losses keep their first time even
// across plain connects within an invocation.
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
		if strings.Contains(l, "ibkr adapter starting") {
			since, first = time.Time{}, ""
		}
		if strings.Contains(l, "state=connected reason=restored") || strings.Contains(l, "ibkr reconciled client_id=") {
			since, first = time.Time{}, ""
			restored = at
		}
	}
	return since, first, restored, nil
}

// gatewayUnitSample is what systemd says of ti-gateway.service: how many times
// it has restarted, and whether it is active.
type gatewayUnitSample struct {
	restarts int
	active   bool // ActiveState=active
}

// gatewayRestartsPage is how far NRestarts must stand above the quiet baseline
// for a start to count as failing, and gatewayQuietToClose how many consecutive
// good quiet firings end it (the rule is advanceRestartRule's; docs/manual/
// server.md, "The Gateway is logged out"; ADR 0019, "Alerting": fail closed on
// a unit that does not stay up).
const (
	gatewayRestartsPage = 3
	gatewayQuietToClose = 2
)

// knownActiveStates are the ActiveState values systemd defines.
var knownActiveStates = map[string]bool{"active": true, "activating": true, "inactive": true, "failed": true, "deactivating": true, "reloading": true, "maintenance": true, "refreshing": true}

// gatewayUnitSampler asks systemctl for the Gateway unit's restart count and
// state. This is what sees a start that keeps failing when the journal takes
// nothing. A systemctl that fails is an error, never a good sample.
func gatewayUnitSampler(systemctl string) func() (gatewayUnitSample, error) {
	return func() (gatewayUnitSample, error) {
		ctx, cancel := context.WithTimeout(context.Background(), gatewayCheckTimeout)
		defer cancel()
		//nolint:gosec // G204: the binary is the operator's -systemctl flag and the unit a fixed name.
		cmd := exec.CommandContext(ctx, systemctl, "show", "-p", "NRestarts", "-p", "ActiveState", "-p", "SubState", gatewayUnit)
		cmd.WaitDelay = gatewayCheckWaitDelay
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		raw, err := cmd.Output()
		if err != nil {
			return gatewayUnitSample{}, fmt.Errorf("reporter: the gateway check could not ask systemd about %s: %w (it said: %s)", gatewayUnit, err, strings.TrimSpace(stderr.String()))
		}
		var s gatewayUnitSample
		state, sub := "", ""
		haveRestarts := false
		for _, l := range strings.Split(string(raw), "\n") {
			k, v, _ := strings.Cut(strings.TrimSpace(l), "=")
			switch k {
			case "ActiveState":
				state = v
			case "SubState":
				sub = v
			case "NRestarts":
				n, err := strconv.Atoi(v)
				if err != nil || n < 0 {
					return gatewayUnitSample{}, fmt.Errorf("reporter: the gateway check read NRestarts=%q for %s, which is not a count", v, gatewayUnit)
				}
				s.restarts, haveRestarts = n, true
			}
		}
		if !haveRestarts || sub == "" || !knownActiveStates[state] {
			return gatewayUnitSample{}, fmt.Errorf("reporter: the gateway check could not trust what systemd said of %s (NRestarts present: %v, SubState %q, ActiveState %q)", gatewayUnit, haveRestarts, sub, state)
		}
		s.active = state == "active"
		return s, nil
	}
}

// resetNightEnd bounds a wait to its written night (ADR 0019, "Alerting";
// server.md, "The retry is bounded"). The caller supplies America/New_York time; the first 01:00 ends it.
func resetNightEnd(at time.Time) time.Time {
	mins := at.Hour()*60 + at.Minute()
	if mins >= 60 && mins < 23*60+40 {
		return time.Time{}
	}
	day := at.Day()
	if mins >= 23*60+40 {
		day++
	}
	return time.Date(at.Year(), at.Month(), day, 1, 0, 0, 0, at.Location())
}
