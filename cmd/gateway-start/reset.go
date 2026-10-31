package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/nysecalendar"
)

const maxResetWaits = 4

// resetWait is the durable cooldown and rolling 24-hour allowance in the window file
// (docs/manual/server.md, "The Gateway is logged out"). Accepted logins clear
// counted attempts, never this allowance. The epoch deadline survives DST.
type resetWait struct {
	refused  time.Time
	until    time.Time
	refusals []time.Time
}

func (w resetWait) withAttempts(attempts []byte) []byte {
	if len(w.refusals) == 0 {
		return append([]byte(nil), attempts...)
	}
	header := fmt.Sprintf("reset %d %d %d", w.refused.Unix(), w.until.Unix(), len(w.refusals))
	for _, at := range w.refusals {
		header += fmt.Sprintf(" %d", at.Unix())
	}
	return append([]byte(header+"\n"), attempts...)
}

// splitReset separates legacy epoch-only files and validates reset-header
// structure; awaitReset checks wait eligibility against the clock. Unknown
// history fails closed (docs/development.md, "Principles").
func splitReset(raw []byte) (resetWait, []byte, error) {
	var w resetWait
	if !strings.HasPrefix(string(raw), "reset ") {
		return w, raw, nil
	}
	line, rest, ok := strings.Cut(string(raw), "\n")
	fields := strings.Fields(line)
	if !ok || len(fields) < 4 {
		return w, nil, errors.New("malformed reset wait")
	}
	values := make([]int64, len(fields)-1)
	for i, field := range fields[1:] {
		if !epochLine.MatchString(field) {
			return w, nil, errors.New("invalid reset value")
		}
		values[i], _ = strconv.ParseInt(field, 10, 64)
	}
	if values[2] < 1 || values[2] > maxResetWaits {
		return w, nil, errors.New("invalid reset allowance")
	}
	w.refused, w.until = time.Unix(values[0], 0), time.Unix(values[1], 0)
	if len(values) == 3 {
		// Legacy nightly counts have no individual timestamps. Charging all
		// at the latest refusal conservatively preserves the rolling bound
		// (docs/manual/server.md, reset policy).
		for range int(values[2]) {
			w.refusals = append(w.refusals, w.refused)
		}
	} else {
		if len(values)-3 != int(values[2]) {
			return resetWait{}, nil, errors.New("invalid reset history length")
		}
		// The history is accounting: every entry is a real refusal at or before
		// the recorded one, in order, and the recorded refusal is its last. Any
		// other shape could replenish the allowance, so it fails closed (AGENTS.md).
		var previous int64
		for _, at := range values[3:] {
			if at <= 0 || at < previous || at > values[0] {
				return resetWait{}, nil, errors.New("invalid reset history")
			}
			previous = at
			w.refusals = append(w.refusals, time.Unix(at, 0))
		}
		if previous != values[0] {
			return resetWait{}, nil, errors.New("reset history does not end at the recorded refusal")
		}
	}
	return w, []byte(rest), nil
}

// resetClockSkew is how far a recorded refusal may lie ahead of the clock
// before the wait is treated as untrustworthy: an NTP step of seconds is
// ordinary, a refusal minutes in the future is a clock or state fault.
const resetClockSkew = 2 * time.Minute

// resetCutoff identifies a night by its first 01:00 New York time, including
// the repeated hour at DST's end (docs/manual/server.md, reset policy).
func resetCutoff(now time.Time) time.Time {
	et := now.In(nysecalendar.NewYork)
	midnight := time.Date(et.Year(), et.Month(), et.Day(), 0, 0, 0, 0, nysecalendar.NewYork)
	if et.Hour() == 23 {
		midnight = midnight.AddDate(0, 0, 1)
	}
	return midnight.Add(time.Hour)
}

func pendingStop(sigs <-chan os.Signal) os.Signal {
	select {
	case sig := <-sigs:
		return sig
	default:
		return nil
	}
}

// awaitReset resumes a durable wait before any new launch; journal failures
// cannot shorten it (docs/manual/server.md, "The Gateway is logged out").
func awaitReset(cfg config, errj *journal, sigs <-chan os.Signal) int {
	_, raw, err := loadCount(cfg.StateFile, cfg.Now(), cfg.Window)
	if err != nil {
		_ = errj.write(fmt.Sprintf("gateway-start: %s: %s is unreadable or malformed (%v)\n", stopPhrase, cfg.StateFile, err))
		return exitStopped
	}
	w, _, _ := splitReset(raw) // loadCount validated the header and attempt lines.
	interrupt := func(sig os.Signal) int {
		if len(w.refusals) > 0 {
			_ = errj.write("gateway-start: reset wait: interrupted by stop\n")
		}
		return 128 + int(sig.(syscall.Signal))
	}
	if sig := pendingStop(sigs); sig != nil {
		return interrupt(sig)
	}
	if len(w.refusals) == 0 {
		return 0
	}
	now := cfg.Now()
	if w.refused.After(now.Add(resetClockSkew)) {
		// A clock that went backwards past the recorded refusal cannot say how
		// much of the wait remains; another launch is the one thing not allowed.
		_ = errj.write(fmt.Sprintf("gateway-start: %s: the recorded reset refusal is %s ahead of the clock; not launching until the clock or %s is put right\n",
			stopPhrase, w.refused.Sub(now).Round(time.Second), cfg.StateFile))
		return exitStopped
	}
	cooldown := nightlyResetDelay(w.refused)
	if cooldown <= 0 || !w.until.Equal(w.refused.Add(cooldown)) {
		// A refusal outside its reset night, or a deadline that is not the one
		// the rule would have set, is state that cannot be trusted; it is kept
		// for inspection and nothing launches on it (AGENTS.md: fail closed).
		_ = errj.write(fmt.Sprintf("gateway-start: %s: the recorded reset wait is inconsistent (refusal %s, deadline %s); not launching until %s is put right\n",
			stopPhrase, resetClockLabel(w.refused), resetClockLabel(w.until), cfg.StateFile))
		return exitStopped
	}
	// The deadline is kept even when the clock stepped back a little; it can
	// only lengthen the wait, never shorten it, and never past a full step.
	bounded := func(at time.Time) time.Duration { return min(w.until.Sub(at), 15*time.Minute+resetClockSkew) }
	delay := bounded(now)
	if delay <= 0 {
		return 0
	}
	_ = errj.write(w.message(cfg.Now()))
	// Compute again after the bounded journal write: the durable deadline, not
	// the sink's speed, determines when another login is permitted; a clock
	// that stepped back meanwhile is judged exactly as it was above.
	now = cfg.Now()
	if w.refused.After(now.Add(resetClockSkew)) {
		_ = errj.write(fmt.Sprintf("gateway-start: %s: the clock stepped back %s past the recorded reset refusal; not launching until the clock or %s is put right\n",
			stopPhrase, w.refused.Sub(now).Round(time.Second), cfg.StateFile))
		return exitStopped
	}
	delay = bounded(now)
	if delay > 0 {
		select {
		case <-cfg.After(delay):
		case sig := <-sigs:
			return interrupt(sig)
		}
	}
	if sig := pendingStop(sigs); sig != nil {
		return interrupt(sig)
	}
	return 0
}

// message describes the remaining durable wait at the decision point, including
// cleanup that crossed the cutoff (docs/manual/server.md, reset policy).
func (w resetWait) message(now time.Time) string {
	remaining := max(time.Duration(0), w.until.Sub(now))
	unit, suffix := time.Minute, "m"
	if remaining < time.Minute {
		unit, suffix = time.Second, "s"
	}
	whole := (remaining + unit - 1) / unit
	return fmt.Sprintf("gateway-start: reset wait: login refused at %s ET inside IBKR's nightly reset hour; waiting %d%s until %s ET (consumed reset allowances %d of 4 in 24h)\n",
		resetClockLabel(w.refused), whole, suffix, resetClockLabel(w.until), len(w.refusals))
}

// recentRefusals retains consumed allowances across nights and DST; future
// timestamps also remain charged after a clock correction (docs/manual/server.md).
func (w resetWait) recentRefusals(now time.Time) []time.Time {
	var recent []time.Time
	for _, at := range w.refusals {
		if now.Sub(at) < 24*time.Hour {
			recent = append(recent, at)
		}
	}
	return recent
}

func resetClockLabel(at time.Time) string {
	format := "15:04"
	if at.Second() != 0 {
		format = "15:04:05"
	}
	return at.In(nysecalendar.NewYork).Format(format)
}

type resetDecision struct {
	discounted []byte
	wait       resetWait
}

// receiveReset retrieves the final cleanup decision before a reset return
// (docs/development.md, "Ending processes").
func receiveReset(v *verdict, decisions <-chan resetDecision) *resetDecision {
	if v != nil && v.code == resetRetry {
		decision := <-decisions // The reader publishes the decision before its verdict.
		return &decision
	}
	select {
	case decision := <-decisions:
		return &decision
	default:
		return nil
	}
}
