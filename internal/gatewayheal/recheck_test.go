package gatewayheal

import (
	"strings"
	"testing"
	"time"
)

// Synthetic schedules and systemd facts exercise server.md's Gateway healer
// invariant. Stand-ins record unit calls only; no process is signalled.
func TestResetFailedDuringWindowIsOwnerVeto(t *testing.T) {
	r := fixture()
	r.at = time.Date(2026, 11, 3, 5, 0, 0, 0, time.UTC)
	r.onWait = func() { s := r.statuses[AdapterUnits[0]]; s.Active = "inactive"; r.statuses[AdapterUnits[0]] = s }
	assertRun(t, r, "owner-stopped")
}
func TestGatewayReplacementDuringEvidencePreventsRestart(t *testing.T) {
	r := fixture()
	r.onLines = func(id string) {
		if id == "old-login" {
			r.statuses[GatewayUnit] = Status{Active: "active", Invocation: "independent-login"}
		}
	}
	assertRun(t, r, "logged-out")
}
func TestDurableSaveCrossingWindowRetainsReservation(t *testing.T) {
	r := fixture()
	r.at = time.Date(2026, 11, 3, 4, 39, 59, 0, time.UTC)
	r.onSave = func() { r.at = r.at.Add(2 * time.Second) }
	assertRun(t, r, "window")
	if r.state.LastRestart.IsZero() {
		t.Fatal("reservation discarded")
	}
}
func TestLastAdapterStatusCrossingWindowPreventsStart(t *testing.T) {
	r := fixture()
	r.at = time.Date(2026, 11, 3, 4, 39, 58, 0, time.UTC)
	r.onStatus = func(unit string) {
		if unit == AdapterUnits[0] && len(r.calls) > 0 {
			r.at = r.at.Add(3 * time.Second)
		}
	}
	assertRun(t, r, "window", "restart "+GatewayUnit)
}
func TestGatewayReplacementAfterRestartCannotAuthorizeAdapter(t *testing.T) {
	r := fixture()
	r.onLines = func(id string) {
		if id == "new-login" {
			r.statuses[GatewayUnit] = Status{Active: "active", Invocation: "independent-login", Started: r.at}
		}
	}
	assertRun(t, r, "logged-out", "restart "+GatewayUnit)
}
func TestGatewayNeverStartedSkipsJournal(t *testing.T) {
	r := fixture()
	r.statuses[GatewayUnit] = Status{Active: "inactive"}
	r.onLines = func(id string) {
		if id == "" {
			t.Error("queried missing invocation")
		}
	}
	assertRun(t, r, "logged-out")
}
func TestWinnerRecoversAllEligibleAccountsAfterIncidentLoss(t *testing.T) {
	r := fixture()
	for i, unit := range AdapterUnits[1:] {
		id := unit + "-failure"
		r.statuses[unit] = Status{Active: "failed", Code: 1, Exit: 3, Invocation: id, Finished: r.loss.Add(time.Duration(i+1) * time.Minute)}
		r.journals[id] = []string{Remedy, "later diagnostic"}
	}
	assertRun(t, r, "eligible", "restart "+GatewayUnit, "start "+AdapterUnits[0], "start "+AdapterUnits[1], "start "+AdapterUnits[2])
}
func TestPeersPredatingLossOrOwnerVetoRemainDown(t *testing.T) {
	r := fixture()
	r.statuses[AdapterUnits[1]] = Status{Active: "failed", Code: 1, Exit: 3, Invocation: "stale", Finished: r.loss.Add(-time.Second)}
	r.journals["stale"] = []string{Remedy}
	r.statuses[AdapterUnits[2]] = Status{Active: "failed", Code: 1, Exit: 3, Invocation: "veto", Finished: r.loss.Add(time.Second)}
	r.journals["veto"] = []string{Remedy}
	r.onLines = func(id string) {
		if id == "veto" {
			s := r.statuses[AdapterUnits[2]]
			s.Active = "inactive"
			r.statuses[AdapterUnits[2]] = s
		}
	}
	assertRun(t, r, "eligible", "restart "+GatewayUnit, "start "+AdapterUnits[0])
}
func TestGatewayLaunchProvisionalCounterSettlesBeforeLogin(t *testing.T) {
	r := fixture()
	r.onAct = func(action, _ string) {
		if action == "restart" {
			r.input.Counter = []byte("1793631600\n")
			r.journals["new-login"] = nil
		}
	}
	r.onWait = func() { r.input.Counter = nil; r.journals["new-login"] = []string{"IBC: Login has completed"} }
	assertRun(t, r, "eligible", "restart "+GatewayUnit, "start "+AdapterUnits[0])
	if len(r.waits) == 0 {
		t.Fatal("did not observe provisional launch")
	}
}
func TestProvisionalCounterDoesNotRefuseCompletedLogin(t *testing.T) {
	r := fixture()
	r.onAct = func(action, _ string) {
		if action == "restart" {
			r.input.Counter = []byte("1793631600\n")
		}
	}
	r.onWait = func() { r.input.Counter = nil }
	assertRun(t, r, "eligible", "restart "+GatewayUnit, "start "+AdapterUnits[0])
}
func TestRefusedLoginStopsEvenAfterCompletion(t *testing.T) {
	r := fixture()
	r.journals["new-login"] = []string{"IBC: Login has completed", "Unrecognized Username or Password"}
	r.onAct = func(action, _ string) {
		if action == "restart" {
			r.input.Counter = []byte("1793631600\n")
		}
	}
	assertRun(t, r, "login-problem", "restart "+GatewayUnit)
}
func TestSundayColdRestartIndependentOfOwnerSchedule(t *testing.T) {
	r := fixture()
	r.at = time.Date(2026, 11, 1, 16, 55, 0, 0, time.UTC)
	r.input.GatewayEnv = []byte("TI_GATEWAY_COLD_RESTART=12:00\n")
	assertRun(t, r, "window", "restart "+GatewayUnit, "start "+AdapterUnits[0])
	if len(r.waits) != 1 || r.waits[0] != 15*time.Minute {
		t.Fatal(r.waits)
	}
}
func TestUnreadableColdScheduleFailsClosed(t *testing.T) {
	for _, raw := range []string{"", "TI_GATEWAY_COLD_RESTART=broken", "TI_GATEWAY_COLD_RESTART=09:00\nTI_GATEWAY_COLD_RESTART=10:00", "TI_GATEWAY_COLD_RESTART=\nTI_GATEWAY_COLD_RESTART=09:00"} {
		r := fixture()
		r.input.GatewayEnv = []byte(raw)
		if r.run() == nil || len(r.calls) != 0 {
			t.Errorf("accepted %q: %v", raw, r.calls)
		}
	}
}
func TestEveryStartRechecksGatewayAndWindow(t *testing.T) {
	r := fixture()
	unit := AdapterUnits[1]
	r.statuses[unit] = Status{Active: "failed", Code: 1, Exit: 3, Invocation: "peer", Finished: r.loss.Add(time.Minute)}
	r.journals["peer"] = []string{Remedy}
	r.onAct = func(action, _ string) {
		if action == "start" {
			r.journals["new-login"] = []string{"LOGGED_OUT"}
		}
	}
	assertRun(t, r, "logged-out", "restart "+GatewayUnit, "start "+AdapterUnits[0])
}
func TestCaseTableSaysAnyNonemptyCounter(t *testing.T) {
	// The gate includes reset metadata; server.md forbids reinterpreting it.
	for _, value := range []string{"0", "reset 1 2 1 1\n"} {
		r := fixture()
		r.input.Counter = []byte(value)
		assertRun(t, r, "login-problem")
	}
}
func TestLateDiagnosticStillContainsExactRemedy(t *testing.T) {
	r := fixture()
	r.journals["failed-run"] = append(r.journals["failed-run"], "later diagnostic")
	assertRun(t, r, "eligible", "restart "+GatewayUnit, "start "+AdapterUnits[0])
	if !strings.Contains(strings.Join(r.logs, "\n"), "step=adapter-start") {
		t.Fatal(r.logs)
	}
}

func TestFinalStatusCannotHideCounterOrScheduleChange(t *testing.T) {
	for _, change := range []string{"counter", "schedule"} {
		t.Run(change, func(t *testing.T) {
			r := fixture()
			r.onStatus = func(unit string) {
				if unit == GatewayUnit && !r.state.LastRestart.IsZero() && len(r.calls) == 0 {
					if change == "counter" {
						r.input.Counter = []byte("1\n")
					} else {
						r.input.Schedule = []byte(`{"windows":[{"days":["daily"],"start":"14:00","end":"16:00","zone":"UTC"}]}`)
					}
				}
			}
			row := "login-problem"
			if change == "schedule" {
				row = "window"
			}
			assertRun(t, r, row)
		})
	}
}
func TestClockRollbackDuringSaveCannotBypassRateLimit(t *testing.T) {
	r := fixture()
	r.state.LastRestart = r.at.Add(-3 * time.Hour)
	r.onSave = func() { r.at = r.at.Add(-2 * time.Hour) }
	assertRun(t, r, "rate-limit")
}
func TestSystemdSecondPrecisionStartStillQualifies(t *testing.T) {
	r := fixture()
	r.at = r.at.Add(100 * time.Millisecond)
	r.onAct = func(action, _ string) {
		if action == "restart" {
			s := r.statuses[GatewayUnit]
			s.Started = s.Started.Truncate(time.Second)
			r.statuses[GatewayUnit] = s
		}
	}
	assertRun(t, r, "eligible", "restart "+GatewayUnit, "start "+AdapterUnits[0])
}
func TestPeerFinishingDuringRestartIsRecovered(t *testing.T) {
	r := fixture()
	r.onAct = func(action, _ string) {
		if action == "restart" {
			r.at = r.at.Add(time.Second)
			r.statuses[AdapterUnits[1]] = Status{Active: "failed", Code: 1, Exit: 3, Invocation: "peer", Finished: r.at}
			r.journals["peer"] = []string{Remedy}
		}
	}
	assertRun(t, r, "eligible", "restart "+GatewayUnit, "start "+AdapterUnits[0], "start "+AdapterUnits[1])
}
