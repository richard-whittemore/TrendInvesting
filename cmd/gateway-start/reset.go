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

// resetWait is the durable cooldown and nightly allowance in the window file
// (docs/manual/server.md, "The Gateway is logged out"). Accepted logins clear
// counted attempts, never this allowance. The epoch deadline survives DST.
type resetWait struct {
	refused time.Time
	until   time.Time
	count   int
}

func (w resetWait) withAttempts(attempts []byte) []byte {
	if w.count == 0 {
		return append([]byte(nil), attempts...)
	}
	return append([]byte(fmt.Sprintf("reset %d %d %d\n", w.refused.Unix(), w.until.Unix(), w.count)), attempts...)
}

// splitReset accepts legacy epoch-only files and one validated reset header;
// unknown state fails closed (docs/development.md, "Principles").
func splitReset(raw []byte) (resetWait, []byte, error) {
	var w resetWait
	if !strings.HasPrefix(string(raw), "reset ") {
		return w, raw, nil
	}
	line, rest, ok := strings.Cut(string(raw), "\n")
	fields := strings.Fields(line)
	if !ok || len(fields) != 4 {
		return w, nil, errors.New("malformed reset wait")
	}
	values := [3]int64{}
	for i, field := range fields[1:] {
		if !epochLine.MatchString(field) {
			return w, nil, errors.New("invalid reset value")
		}
		values[i], _ = strconv.ParseInt(field, 10, 64)
	}
	if values[2] < 1 || values[2] > maxResetWaits {
		return w, nil, errors.New("invalid reset allowance")
	}
	w = resetWait{time.Unix(values[0], 0), time.Unix(values[1], 0), int(values[2])}
	delay := nightlyResetDelay(w.refused)
	if delay <= 0 || !w.until.Equal(w.refused.Add(delay)) {
		return resetWait{}, nil, errors.New("invalid reset deadline or allowance")
	}
	return w, []byte(rest), nil
}

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
		if w.count > 0 {
			_ = errj.write("gateway-start: reset wait: interrupted by stop\n")
		}
		return 128 + int(sig.(syscall.Signal))
	}
	if sig := pendingStop(sigs); sig != nil {
		return interrupt(sig)
	}
	if w.count == 0 {
		return 0
	}
	delay := w.until.Sub(cfg.Now())
	if delay <= 0 {
		return 0
	}
	_ = errj.write(w.message(cfg.Now()))
	// Compute again after the bounded journal write: the durable deadline, not
	// the sink's speed, determines when another login is permitted.
	delay = w.until.Sub(cfg.Now())
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
	return fmt.Sprintf("gateway-start: reset wait: login refused at %s ET inside IBKR's nightly reset hour; waiting %gm until %s ET (uncounted %d of 4 tonight)\n",
		w.refused.In(nysecalendar.NewYork).Format("15:04"), max(0, w.until.Sub(now).Minutes()), w.until.In(nysecalendar.NewYork).Format("15:04"), w.count)
}
