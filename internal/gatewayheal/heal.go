// Package gatewayheal applies the operational recovery gates in
// docs/manual/server.md, Gateway healer, and ADR 0025's scheduled interruption
// remedy. All effects enter through interfaces; no process or file is opened here.
package gatewayheal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

const GatewayUnit = "ti-gateway.service"
const Remedy = "adapter stopped: MaintenanceExpired: IBKR connectivity did not return by the end of its scheduled window; a Gateway restart is the remedy"

var AdapterUnits = []string{"ti-adapter-boost.service", "ti-adapter@trad-ira.service", "ti-adapter@roth-ira.service"}

// Status identifies one systemd invocation, not a PID (docs/development.md,
// Ending processes). Finished is the failed main process's exit timestamp.
type Status struct {
	Active, Invocation string
	Code, Exit         int
	Finished           time.Time
}
type Inputs struct{ Schedule, Counter []byte }
type State struct {
	LastRestart time.Time `json:"last_restart"`
	Incident    string    `json:"incident"`
}
type Clock interface {
	Now() time.Time
	Wait(time.Duration) error
}
type Reader interface{ Read() (Inputs, error) }
type Store interface {
	Load() (State, error)
	Save(State) error
}
type Journal interface {
	Lines(unit, invocation string) ([]string, error)
}
type Systemctl interface {
	Status(string) (Status, error)
	Act(action, unit string) error
}

// Guard excludes every relay until release, including other accounts sharing
// Gateway (ADR 0026 point 8; adapter/ibkr/relay.py's lifetime pending lock).
type Guard interface {
	Acquire() (release func(), err error)
}

type healer struct{ log func(string) error }

func (h healer) say(row, step string) error {
	return h.log("gateway-heal: row=" + row + " step=" + step)
}

// Run reserves a restart durably before issuing it and never clears that
// reservation, including after a failed command (server.md, Gateway healer).
// The caller must serialize Load through Save across all healer instances.
func Run(unit string, clock Clock, reader Reader, store Store, journal Journal, ctl Systemctl, guard Guard, log func(string) error) error {
	h := healer{log: log}
	if !AllowedAdapter(unit) {
		return h.say("other-exit", "unknown-unit")
	}
	failed, err := ctl.Status(unit)
	if err != nil {
		return err
	}
	if failed.Active != "failed" || failed.Code != 1 || failed.Exit != 3 || failed.Invocation == "" || failed.Finished.IsZero() {
		return h.say("other-exit", "no-action")
	}
	lines, err := journal.Lines(unit, failed.Invocation)
	if err != nil {
		return err
	}
	found := false
	for _, line := range lines {
		if strings.TrimSpace(line) == Remedy {
			found = true
		}
	}
	if !found {
		return h.say("no-remedy", "no-action")
	}
	prior, err := store.Load()
	if err != nil {
		return err
	}
	if prior.Incident == failed.Invocation || (!prior.LastRestart.IsZero() && !failed.Finished.After(prior.LastRestart)) {
		return h.say("same-incident", "no-action")
	}
	if !prior.LastRestart.IsZero() && clock.Now().Sub(prior.LastRestart) < 2*time.Hour {
		return h.say("rate-limit", "no-action")
	}
	input, err := reader.Read()
	if err != nil {
		return err
	}
	end, err := WindowEnd(input.Schedule, clock.Now())
	if err != nil {
		return err
	}
	if !end.IsZero() {
		if err := h.say("window", "wait-until="+end.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
		if err := clock.Wait(end.Sub(clock.Now())); err != nil {
			return err
		}
	}
	// Re-read every mutable gate after waiting and again under relay exclusion.
	release, err := guard.Acquire()
	if err != nil {
		return h.say("order-busy", "no-action")
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	current, err := ctl.Status(unit)
	if err != nil {
		return err
	}
	if current != failed {
		return h.say("same-incident", "adapter-changed")
	}
	input, err = reader.Read()
	if err != nil {
		return err
	}
	end, err = WindowEnd(input.Schedule, clock.Now())
	if err != nil {
		return err
	}
	if !end.IsZero() {
		return h.say("window", "still-blocked-after-recheck")
	}
	gateway, err := ctl.Status(GatewayUnit)
	if err != nil {
		return err
	}
	lines, err = journal.Lines(GatewayUnit, gateway.Invocation)
	if err != nil {
		return err
	}
	if len(input.Counter) != 0 || loginProblem(lines) {
		return h.say("login-problem", "no-action")
	}
	if gateway.Active != "active" || gateway.Invocation == "" || !LoggedIn(lines) {
		return h.say("logged-out", "no-action")
	}
	input, err = reader.Read()
	if err != nil {
		return err
	}
	if len(input.Counter) != 0 {
		return h.say("login-problem", "before-restart")
	}
	started := clock.Now()
	if !prior.LastRestart.IsZero() && started.Sub(prior.LastRestart) < 2*time.Hour {
		return h.say("rate-limit", "no-action")
	}
	end, err = WindowEnd(input.Schedule, started)
	if err != nil {
		return err
	}
	if !end.IsZero() {
		return h.say("window", "no-action")
	}
	if err := h.say("eligible", "reserve-restart"); err != nil {
		return err
	}
	if err := store.Save(State{LastRestart: started, Incident: failed.Invocation}); err != nil {
		return err
	}
	if err := h.say("eligible", "restart"); err != nil {
		return err
	}
	if err := ctl.Act("restart", GatewayUnit); err != nil {
		return err
	}
	if err := h.say("eligible", "wait-login"); err != nil {
		return err
	}
	for clock.Now().Sub(started) < 10*time.Minute {
		current, err = ctl.Status(GatewayUnit)
		if err != nil {
			return err
		}
		if current.Invocation != "" && current.Invocation != gateway.Invocation {
			lines, err = journal.Lines(GatewayUnit, current.Invocation)
			if err != nil {
				return err
			}
			if clock.Now().Sub(started) >= 10*time.Minute {
				return h.say("login-timeout", "stop-existing-page-stands")
			}
			if current.Active == "active" && LoggedIn(lines) && !loginProblem(lines) {
				if err := h.say("eligible", "login-completed"); err != nil {
					return err
				}
				// A slow login can cross into a new window. It cannot authorize an
				// adapter start in that window (server.md, Gateway healer invariant).
				input, err = reader.Read()
				if err != nil {
					return err
				}
				end, err = WindowEnd(input.Schedule, clock.Now())
				if err != nil {
					return err
				}
				if !end.IsZero() {
					return h.say("window", "login-crossed-window")
				}
				if len(input.Counter) != 0 {
					return h.say("login-problem", "after-restart")
				}
				current, err = ctl.Status(unit)
				if err != nil {
					return err
				}
				if current != failed {
					return h.say("same-incident", "adapter-changed")
				}
				if clock.Now().Sub(started) >= 10*time.Minute {
					return h.say("login-timeout", "stop-existing-page-stands")
				}
				release()
				release = nil
				if err := h.say("eligible", "adapter-start"); err != nil {
					return err
				}
				return ctl.Act("start", unit)
			}
		}
		remaining := 10*time.Minute - clock.Now().Sub(started)
		if err := clock.Wait(min(5*time.Second, remaining)); err != nil {
			return err
		}
	}
	return h.say("login-timeout", "stop-existing-page-stands")
}
func AllowedAdapter(unit string) bool {
	for _, name := range AdapterUnits {
		if unit == name {
			return true
		}
	}
	return false
}
func loginProblem(lines []string) bool {
	for _, line := range lines {
		if strings.Contains(line, "login retries stopped") || strings.Contains(line, "gateway-start: reset wait:") {
			return true
		}
	}
	return false
}

// LoggedIn uses ordered evidence from exactly one invocation. A logout or
// refused login after completion withdraws permission (ADR 0019, Alerting).
func LoggedIn(lines []string) bool {
	logged := false
	for _, line := range lines {
		if strings.Contains(line, "Login has completed") {
			logged = true
		}
		if strings.Contains(line, "LOGGED_OUT") || strings.Contains(line, "Unrecognized Username or Password") {
			logged = false
		}
	}
	return logged
}

// WindowEnd reuses report's adapter-schema parser and unions its windows with
// the mandatory nightly reset hour (server.md, Gateway healer). A union with
// no gap within nine days fails closed, even when each schedule has a gap.
func WindowEnd(raw []byte, now time.Time) (time.Time, error) {
	var schedules []report.IBKRSchedule
	for _, data := range [][]byte{raw, []byte(`{"windows":[{"days":["daily"],"start":"23:40","end":"01:00","zone":"America/New_York"}]}`)} {
		schedule, err := report.ParseIBKRSchedule(data)
		if err != nil {
			return time.Time{}, err
		}
		schedules = append(schedules, schedule)
	}
	at := now
	for at.Sub(now) <= 9*24*time.Hour {
		var end time.Time
		for _, schedule := range schedules {
			if candidate := schedule.WindowEnd(at); candidate.After(end) {
				end = candidate
			}
		}
		if end.IsZero() {
			if at.Equal(now) {
				return time.Time{}, nil
			}
			return at, nil
		}
		at = end
	}
	return time.Time{}, errors.New("maintenance and reset windows leave no bounded gap")
}

// ValidateAction limits mutations to the two named unit operations permitted
// by docs/development.md, Ending processes, and server.md, Gateway healer.
func ValidateAction(action, unit string) error {
	if (action == "restart" && unit == GatewayUnit) || (action == "start" && AllowedAdapter(unit)) {
		return nil
	}
	return fmt.Errorf("unapproved unit operation")
}

// HandoverClear refuses unresolved or malformed marks even after their relay
// has exited (adapter/ibkr/relay.py, PendingInput.handovers; ADR 0025).
func HandoverClear(raw []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for dec.More() {
		token, err = dec.Token()
		if err != nil {
			return false
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return false
		}
		seen[key] = true
		var mark string
		if err := dec.Decode(&mark); err != nil {
			return false
		}
		switch mark {
		case "sent", "never-sent", "at-broker":
		default:
			date, err := time.Parse("2006-01-02", strings.TrimPrefix(mark, "resend:"))
			if err != nil || mark != "resend:"+date.Format("2006-01-02") {
				return false
			}
		}
	}
	_, closingErr := dec.Token()
	_, trailingErr := dec.Token()
	return closingErr == nil && errors.Is(trailingErr, io.EOF)
}
