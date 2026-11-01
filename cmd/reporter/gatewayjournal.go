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
)

// gatewayEvidence is what the journals show: logged out (out), or that they
// hold no login event at all (silent), or neither (logged in).
type gatewayEvidence struct {
	out, silent             bool
	loggedIn                bool
	adapters                map[string]adapterEvidence
	resetWait, connectivity bool
	said                    string
	// recoveredAt is IBC's completed login when it is its newest login event.
	// Adapter restorations stay with their unit in adapters. Recovery must be
	// newer than the owning loss, never an older event left after rotation.
	recoveredAt time.Time
}

// adapterEvidence keeps link and process observations together by unit (ADR 0019,
// "Alerting"). A failed sample cannot invalidate that unit's journal evidence.
type adapterEvidence struct {
	active                                bool
	started, lost, restored, previousLoss time.Time
	sampleErr                             error
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
	ibcEvent = regexp.MustCompile(`Unrecognized Username or Password|LoginState is LOGGED_OUT|Login has completed|login retries stopped|gateway-start:`)
	// The adapter's link lines (adapter/ibkr/connection.py _report and
	// loop.py): connectivity-lost is code 1100/2110; "connected reason=restored"
	// is IBKR's restoration (1101/1102); "reconciled" is a successful read of the
	// account after a connect, which needs IBKR. A plain "connected" (the API
	// handshake, logged at every reconnect, including every restore_timeout while
	// Gateway has no IBKR) proves nothing about IBKR and is not matched.
	linkEvent      = regexp.MustCompile(`ibkr link state=connectivity-lost|ibkr link state=connected reason=restored|ibkr reconciled client_id=`)
	lostAccess     = regexp.MustCompile(`(?i)not seeing messages from other users|insufficient permissions|no journal files were opened`)
	unixStartStamp = regexp.MustCompile(`^@\d+(\.\d+)?$`)
	shortISO       = "2006-01-02T15:04:05-0700" // fractional seconds are accepted when parsing
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
func gatewayJournalChecker(journalctl string, adapters []string, systemctl string) gatewayChecker {
	return func(now time.Time) (gatewayEvidence, error) {
		ibc, err := journalQuery(journalctl, gatewayUnit, ibcEvent)
		if err != nil {
			return gatewayEvidence{}, err
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
		loggedOut := len(reasons) > 0
		loggedIn := len(ibc) > 0 && !loggedOut
		connectivity := false
		adaptersSeen := map[string]adapterEvidence{}
		for _, unit := range adapters {
			// IBC's own logout evidence needs no adapter corroboration
			// (server.md, "The Gateway is logged out").
			if loggedOut {
				continue
			}
			lines, err := journalQuery(journalctl, unit, linkEvent)
			if err != nil {
				return gatewayEvidence{}, err
			}
			active, started, err := adapterStart(systemctl, unit)
			evidence := adapterEvidence{active: active, started: started, sampleErr: err}
			evidence.previousLoss, _, _, err = lostSince(lines)
			if err != nil {
				return gatewayEvidence{}, fmt.Errorf("reporter: %s link events: %w", unit, err)
			}
			if evidence.sampleErr == nil && !active {
				adaptersSeen[unit] = evidence
				continue
			}
			var current []string
			for _, line := range lines {
				at, err := lineTime(line)
				if err != nil {
					return gatewayEvidence{}, fmt.Errorf("reporter: the gateway check could not read %s's link time: %w", unit, err)
				}
				if evidence.sampleErr != nil || at.After(started) {
					current = append(current, line)
				}
			}
			since, line, restored, err := lostSince(current)
			if err != nil {
				return gatewayEvidence{}, fmt.Errorf("reporter: the gateway check could not read %s's link events: %w", unit, err)
			}
			evidence.lost, evidence.restored = since, restored
			adaptersSeen[unit] = evidence
			if !since.IsZero() {
				connectivity = loggedIn
				reasons = append(reasons, fmt.Sprintf("%s has reported IBKR's servers lost (1100/2110) with no restoration since, for over %s: %s", unit, now.Sub(since), line))
			}
		}
		switch {
		case len(reasons) > 0:
			return gatewayEvidence{out: true, resetWait: resetWait, connectivity: connectivity, loggedIn: loggedIn, adapters: adaptersSeen, said: strings.Join(reasons, "; "), recoveredAt: recovered}, nil
		case len(ibc) == 0:
			return gatewayEvidence{silent: true, adapters: adaptersSeen, said: "gateway: no login evidence: the ti-gateway journal holds no login event in the last 14 days", recoveredAt: recovered}, nil
		}
		return gatewayEvidence{loggedIn: loggedIn, adapters: adaptersSeen, said: "gateway: logged in (IBC's newest login event is a completed login)", recoveredAt: recovered}, nil
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

// adapterStart bounds link evidence to an active adapter's current process
// (ADR 0019, "Alerting"; server.md, "The Gateway is logged out"). Unix timestamps
// avoid host locale and zone ambiguity; an unreadable sample is a sampling
// fault, never evidence of login or a reason to discard journal evidence.
func adapterStart(systemctl, unit string) (bool, time.Time, error) {
	raw, err := unitCommand(systemctl, "show", "--timestamp=unix", "-p", "ActiveState", "-p", "ExecMainStartTimestamp", unit)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("reporter: the gateway check could not sample adapter %s: %w", unit, err)
	}
	props := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		k, v, _ := strings.Cut(line, "=")
		props[k] = v
	}
	state := props["ActiveState"]
	if !knownActiveStates[state] {
		return false, time.Time{}, fmt.Errorf("reporter: the gateway check found adapter %s has invalid ActiveState", unit)
	}
	if state != "active" {
		return false, time.Time{}, nil
	}
	stamp := props["ExecMainStartTimestamp"]
	if !unixStartStamp.MatchString(stamp) {
		return false, time.Time{}, fmt.Errorf("reporter: the gateway check found adapter %s has invalid ExecMainStartTimestamp", unit)
	}
	seconds, fraction, _ := strings.Cut(stamp[1:], ".")
	sec, err := strconv.ParseInt(seconds, 10, 64)
	// Bound to time.Time's RFC3339 range so the persisted generation round-trips.
	if err != nil || sec > 253402300799 {
		return false, time.Time{}, fmt.Errorf("reporter: the gateway check found adapter %s has invalid ExecMainStartTimestamp", unit)
	}
	fraction = (fraction + "000000000")[:9]
	nanos, err := strconv.ParseInt(fraction, 10, 64)
	if err != nil || sec == 0 && nanos == 0 {
		return false, time.Time{}, fmt.Errorf("reporter: the gateway check found adapter %s has invalid ExecMainStartTimestamp", unit)
	}
	return true, time.Unix(sec, nanos).UTC(), nil
}
