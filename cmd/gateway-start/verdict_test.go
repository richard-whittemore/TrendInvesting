package main

import "testing"

// A stop (77) always overrides an earlier retry (75), whichever is seen first;
// a retry never replaces a stop.
func TestAStopOverridesAnEarlierRetryAndNeverTheOtherWay(t *testing.T) {
	var v verdictState
	v.set(verdict{exitRetry, "refused"})
	v.set(verdict{exitStopped, "output incomplete"})
	if got := v.get(); got == nil || got.code != exitStopped {
		t.Fatalf("got %v, want 77 after retry then stop", got)
	}
	var w verdictState
	w.set(verdict{exitStopped, "bound"})
	w.set(verdict{exitRetry, "refused"})
	if got := w.get(); got == nil || got.code != exitStopped {
		t.Fatalf("got %v, want 77 kept after stop then retry", got)
	}
}

// Journal, reader and count failures must override a planned reset wait
// (docs/manual/server.md, "The Gateway is logged out").
func TestAFailureOverridesAResetWaitAndNeverTheOtherWay(t *testing.T) {
	for _, code := range []int{exitRetry, exitStopped} {
		var v verdictState
		v.set(verdict{resetRetry, "reset"})
		v.set(verdict{code, "failure"})
		v.set(verdict{resetRetry, "reset"})
		if got := v.get(); got == nil || got.code != code {
			t.Fatalf("got %v, want %d kept instead of reset wait", got, code)
		}
	}
}
