//go:build gateway_heal_rehearsal

package main

import "time"

// This clock exists only in the container drill binary. It puts the drill
// outside the nightly window without a production flag that can bypass it
// (docs/manual/server.md, Gateway healer invariant).
type rehearsalClock struct{ began time.Time }

func (c rehearsalClock) Now() time.Time {
	return time.Date(2026, 11, 2, 17, 0, 0, 0, time.UTC).Add(time.Since(c.began))
}
func (rehearsalClock) Wait(d time.Duration) error { time.Sleep(d); return nil }
func init()                                       { clockForRun = rehearsalClock{began: time.Now()} }
