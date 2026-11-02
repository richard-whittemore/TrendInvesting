//go:build gateway_heal_rehearsal

package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// The drill clock and systemd timestamps share one offset across invocations
// (docs/manual/server.md, Gateway healer). Production has no clock override.
type rehearsalClock struct{ offset time.Duration }

func (c rehearsalClock) Now() time.Time           { return time.Now().Add(c.offset) }
func (rehearsalClock) Wait(d time.Duration) error { time.Sleep(d); return nil }
func init() {
	raw, err := os.ReadFile("/run/heal-clock-offset-seconds")
	if err != nil {
		panic(err)
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		panic(err)
	}
	c := rehearsalClock{offset: time.Duration(seconds) * time.Second}
	clockForRun = c
	statusTimeForRun = func(at time.Time) time.Time {
		if at.IsZero() {
			return at
		}
		return at.Add(c.offset)
	}
}
