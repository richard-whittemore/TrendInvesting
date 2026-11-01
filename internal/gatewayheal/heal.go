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
	Finished, Started  time.Time
}
type Inputs struct{ Schedule, Counter, GatewayEnv []byte }
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
	Lock() (func(), error)
	Load() (State, error)
	Save(State) error
}
type Journal interface {
	Lines(unit, invocation string) ([]string, error)
	Loss(unit, invocation string) (time.Time, error)
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

// ErrAnotherHealer requests a bounded wait for the global recovery lock
// (docs/manual/server.md, Gateway healer).
var ErrAnotherHealer = errors.New("another healer is running")

// ErrOrderBusy is the guard's report that an adapter holds its hand-over lock
// or has a hand-over without its journal record (ADR 0026 point 8). Any other
// guard error means the check itself could not be made; both stop recovery.
var ErrOrderBusy = errors.New("an order is between hand-over and its journal record")

type healer struct{ log func(string) error }

func (h healer) say(row, step string) error {
	return h.log("gateway-heal: row=" + row + " step=" + step)
}

// Run reserves a restart durably before issuing it and never clears that
// reservation, including after a failed command (server.md, Gateway healer).
// Store.Lock serializes recovery across accounts, including adapter starts.
func Run(unit string, clock Clock, reader Reader, store Store, journal Journal, ctl Systemctl, guard Guard, log func(string) error) error {
	h := healer{log: log}
	if !AllowedAdapter(unit) {
		return h.say("other-exit", "unknown-unit")
	}
	failed, err := ctl.Status(unit)
	if err != nil {
		return err
	}
	if !eligibleFailure(failed) {
		return h.say("other-exit", "no-action")
	}
	lines, err := journal.Lines(unit, failed.Invocation)
	if err != nil {
		return err
	}
	if !hasRemedy(lines) {
		return h.say("no-remedy", "no-action")
	}
	input, err := reader.Read()
	if err != nil {
		return err
	}
	end, err := input.windowEnd(clock.Now())
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
	// Maintenance waits may span days; serialization and recovery share one
	// ten-minute budget after that wait (server.md, Gateway healer).
	deadline := clock.Now().Add(10 * time.Minute)
	if bounded, ok := ctl.(interface{ SetDeadline(time.Time) }); ok {
		bounded.SetDeadline(deadline)
	}
	unlock, err := h.serialize(clock, store, deadline)
	if err != nil {
		return err
	}
	if unlock == nil {
		return nil
	}
	defer unlock()
	prior, err := store.Load()
	if err != nil {
		return err
	}
	gateway, err := ctl.Status(GatewayUnit)
	if err != nil {
		return err
	}
	loss, err := journal.Loss(unit, failed.Invocation)
	if err != nil {
		return err
	}
	if loss.IsZero() || !failed.Finished.After(loss) {
		return h.say("no-remedy", "missing-incident-loss")
	}
	checks := actionChecks{clock: clock, reader: reader, journal: journal, ctl: ctl, deadline: deadline}
	// A newer, logged-in invocation can recover this account even while another
	// account is already running. Only a restart needs relay exclusion and the
	// two-hour allowance (server.md, Gateway healer).
	if gateway.Started.After(loss) {
		return checks.startAdapter(h, "already-restarted", unit, failed, gateway)
	}
	if prior.Incident == failed.Invocation || (!prior.LastRestart.IsZero() && !failed.Finished.After(prior.LastRestart)) {
		return h.say("same-incident", "no-action")
	}
	if !prior.LastRestart.IsZero() && clock.Now().Sub(prior.LastRestart) < 2*time.Hour {
		return h.say("rate-limit", "no-action")
	}
	checks.priorRestart = prior.LastRestart
	release, err := guard.Acquire()
	if errors.Is(err, ErrOrderBusy) {
		return h.say("order-busy", "no-action")
	}
	if err != nil {
		return h.say("guard-unreadable", "no-action: "+strings.ReplaceAll(err.Error(), "\n", " "))
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	row, err := checks.check(unit, failed, gateway)
	if err != nil {
		return err
	}
	if row != "" {
		return h.say(row, "no-action")
	}
	started := clock.Now()
	if err := h.say("eligible", "reserve-restart"); err != nil {
		return err
	}
	if err := store.Save(State{LastRestart: started, Incident: failed.Invocation}); err != nil {
		return err
	}
	if err := h.say("eligible", "restart"); err != nil {
		return err
	}
	// Durable accounting and audit output may block. Re-read the gates after
	// both, immediately before mutation (server.md, Gateway healer invariant).
	row, err = checks.check(unit, failed, gateway)
	if err != nil {
		return err
	}
	if row != "" {
		return h.say(row, "before-restart")
	}
	if err := ctl.Act("restart", GatewayUnit); err != nil {
		return err
	}
	restarted, err := ctl.Status(GatewayUnit)
	if err != nil {
		return err
	}
	if restarted.Invocation == "" || restarted.Invocation == gateway.Invocation || restarted.Started.Before(started.Truncate(time.Second)) {
		return h.say("logged-out", "restart-identity-unconfirmed")
	}
	if err := h.say("eligible", "wait-login"); err != nil {
		return err
	}
	for clock.Now().Before(checks.deadline) {
		current, err := ctl.Status(GatewayUnit)
		if err != nil {
			return err
		}
		if current.Invocation != restarted.Invocation || current.Started != restarted.Started {
			return h.say("logged-out", "gateway-changed")
		}
		lines, err := journal.Lines(GatewayUnit, restarted.Invocation)
		if err != nil {
			return err
		}
		if !clock.Now().Before(checks.deadline) {
			break
		}
		if loginProblem(lines) {
			return h.say("login-problem", "after-restart")
		}
		input, err = reader.Read()
		if err != nil {
			return err
		}
		// gateway-start settles its provisional count before forwarding accepted
		// login output. A lagging observation never authorizes a start; poll within
		// the same deadline (server.md, Gateway healer; gateway-start main.go).
		if current.Active == "active" && LoggedIn(lines) && len(input.Counter) == 0 {
			if err := h.say("eligible", "login-completed"); err != nil {
				return err
			}
			release()
			release = nil
			checks.priorRestart = time.Time{}
			return checks.startAdapter(h, "eligible", unit, failed, restarted)
		}
		remaining := checks.deadline.Sub(clock.Now())
		if remaining <= 0 {
			break
		}
		if err := clock.Wait(min(5*time.Second, remaining)); err != nil {
			return err
		}
	}
	return h.say("login-timeout", "stop-existing-page-stands")
}

func eligibleFailure(s Status) bool {
	return s.Active == "failed" && s.Code == 1 && s.Exit == 3 && s.Invocation != "" && !s.Finished.IsZero()
}
func hasRemedy(lines []string) bool {
	for _, line := range lines {
		if strings.TrimSpace(line) == Remedy {
			return true
		}
	}
	return false
}

type actionChecks struct {
	clock        Clock
	reader       Reader
	journal      Journal
	ctl          Systemctl
	deadline     time.Time
	priorRestart time.Time
}

// check refreshes the evidence and identities for each named mutation. No
// durable save or audit write may intervene before the systemctl action
// (server.md, Gateway healer; ADR 0026 point 8).
func (c actionChecks) check(unit string, failed, gateway Status) (string, error) {
	if gateway.Active != "active" || gateway.Invocation == "" {
		return "logged-out", nil
	}
	lines, err := c.journal.Lines(GatewayUnit, gateway.Invocation)
	if err != nil {
		return "", err
	}
	if loginProblem(lines) {
		return "login-problem", nil
	}
	if !LoggedIn(lines) {
		return "logged-out", nil
	}
	input, err := c.reader.Read()
	if err != nil {
		return "", err
	}
	if len(input.Counter) != 0 {
		return "login-problem", nil
	}
	current, err := c.ctl.Status(GatewayUnit)
	if err != nil {
		return "", err
	}
	if current != gateway {
		return "logged-out", nil
	}
	// Status queries may block while a counter or schedule changes.
	input, err = c.reader.Read()
	if err != nil {
		return "", err
	}
	// Check the owner's reset-failed veto after the other evidence reads,
	// immediately before the clock gates and mutation (server.md, Gateway healer).
	current, err = c.ctl.Status(unit)
	if err != nil {
		return "", err
	}
	if current.Active != "failed" || current != failed {
		return "owner-stopped", nil
	}
	// Status queries can cross a window too. Evaluate the clock last.
	if !c.priorRestart.IsZero() && c.clock.Now().Sub(c.priorRestart) < 2*time.Hour {
		return "rate-limit", nil
	}
	if !c.deadline.IsZero() && !c.clock.Now().Before(c.deadline) {
		return "login-timeout", nil
	}
	end, err := input.windowEnd(c.clock.Now())
	if err != nil {
		return "", err
	}
	if !end.IsZero() {
		return "window", nil
	}
	if len(input.Counter) != 0 {
		return "login-problem", nil
	}
	return "", nil
}

// serialize waits for an account's predecessor without extending the recovery
// deadline (server.md, Gateway healer). The caller holds the lock until every
// submitted systemd mutation has returned.
func (h healer) serialize(clock Clock, store Store, deadline time.Time) (func(), error) {
	for clock.Now().Before(deadline) {
		release, err := store.Lock()
		if !errors.Is(err, ErrAnotherHealer) {
			return release, err
		}
		if err := h.say("another-healer", "wait"); err != nil {
			return nil, err
		}
		remaining := deadline.Sub(clock.Now())
		if remaining <= 0 {
			break
		}
		if err := clock.Wait(min(time.Second, remaining)); err != nil {
			return nil, err
		}
	}
	return nil, h.say("login-timeout", "waiting-for-healer")
}

func (c actionChecks) startAdapter(h healer, decision, unit string, failed, gateway Status) error {
	lines, err := c.journal.Lines(unit, failed.Invocation)
	if err != nil {
		return err
	}
	if !hasRemedy(lines) {
		return h.say("no-remedy", "before-adapter-start")
	}
	if err := h.say(decision, "adapter-start"); err != nil {
		return err
	}
	row, err := c.check(unit, failed, gateway)
	if err != nil {
		return err
	}
	if row != "" {
		return h.say(row, "before-adapter-start")
	}
	return c.ctl.Act("start", unit)
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
		if strings.Contains(line, "login retries stopped") || strings.Contains(line, "gateway-start: reset wait:") || strings.Contains(line, "Unrecognized Username or Password") {
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
		if strings.Contains(line, "LOGGED_OUT") || strings.Contains(line, "Unrecognized Username or Password") || strings.Contains(line, "gateway-start: starting IBC") || strings.Contains(line, "gateway-start: cannot start IBC") {
			logged = false
		}
	}
	return logged
}

// windowEnd includes the IBC Sunday cold restart configured in gateway.env,
// with ten minutes on either side, independently of the owner-editable adapter
// schedule (server.md, Gateway healer). Ambiguous or missing times fail closed.
func (input Inputs) windowEnd(now time.Time) (time.Time, error) {
	value := ""
	seen := false
	for _, line := range strings.Split(string(input.GatewayEnv), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, v, ok := strings.Cut(line, "=")
		if strings.TrimSpace(key) != "TI_GATEWAY_COLD_RESTART" {
			continue
		}
		if !ok || seen {
			return time.Time{}, errors.New("ambiguous Gateway cold restart time")
		}
		seen = true
		value = strings.TrimSpace(v)
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
	}
	cold, err := time.Parse("15:04", value)
	if err != nil || cold.Format("15:04") != value {
		return time.Time{}, errors.New("missing or invalid Gateway cold restart time")
	}
	start, end := cold.Add(-10*time.Minute), cold.Add(10*time.Minute)
	day := "Sun"
	if start.Day() != cold.Day() {
		day = "Sat"
	}
	extra := []byte(fmt.Sprintf(`{"windows":[{"days":[%q],"start":%q,"end":%q,"zone":"America/New_York"}]}`, day, start.Format("15:04"), end.Format("15:04")))
	return WindowEnd(input.Schedule, now, extra)
}

// WindowEnd reuses report's adapter-schema parser and unions its windows with
// the mandatory nightly reset hour (server.md, Gateway healer). A union with
// no gap within nine days fails closed, even when each schedule has a gap.
func WindowEnd(raw []byte, now time.Time, extra ...[]byte) (time.Time, error) {
	var schedules []report.IBKRSchedule
	for _, data := range append([][]byte{raw, []byte(`{"windows":[{"days":["daily"],"start":"23:40","end":"01:00","zone":"America/New_York"}]}`)}, extra...) {
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
