package main

import (
	"os"
	"testing"
	"time"
)

// TestMain starts every test with no channel baseline, so a test that
// delivers a log's alerts to a fresh state file sees them sent. The tests of
// the baseline itself restore it (withBaseline).
func TestMain(m *testing.M) {
	channelBaseline = func(time.Time) time.Time { return time.Time{} }
	os.Exit(m.Run())
}

// withBaseline restores the reporter's real baseline for one test.
func withBaseline(t *testing.T) {
	t.Helper()
	saved := channelBaseline
	channelBaseline = func(start time.Time) time.Time { return start }
	t.Cleanup(func() { channelBaseline = saved })
}
