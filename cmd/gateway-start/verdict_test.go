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
