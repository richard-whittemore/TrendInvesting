package gatewayheal

import (
	"fmt"
	"reflect"
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

func TestGatewayAlreadyRestartedRecoversOnlyOwnAdapter(t *testing.T) {
	for _, unit := range AdapterUnits {
		r := allFailedRig()
		r.state = State{LastRestart: r.at.Add(-time.Minute), Incident: "failed-run"}
		r.statuses[GatewayUnit] = Status{Active: "active", Invocation: "new-login", Started: r.loss.Add(time.Second)}
		prior := r.state
		if err := Run(unit, r, r, r, r, r, r, r.Log); err != nil {
			t.Fatal(err)
		}
		assertSafetyTrace(t, r, []string{"gateway-heal: row=already-restarted step=adapter-start"}, []string{"start " + unit})
		if r.state != prior {
			t.Fatal("adapter-only recovery changed restart reservation")
		}
	}
}
func TestGatewayResetFailedImmediatelyBeforeStartVetoes(t *testing.T) {
	r := fixture()
	r.onLog = func(line string) {
		if line == "gateway-heal: row=eligible step=adapter-start" {
			r.onStatus = func(unit string) {
				if unit == AdapterUnits[0] {
					s := r.statuses[unit]
					s.Active = "inactive"
					r.statuses[unit] = s
				}
			}
		}
	}
	assertRun(t, r, "owner-stopped", "restart "+GatewayUnit)
}

func TestGatewayWaitsForGlobalLockWithinRecoveryDeadline(t *testing.T) {
	r := fixture()
	attempts := 0
	r.onGlobalLock = func() (func(), error) {
		attempts++
		if attempts == 1 {
			return nil, ErrAnotherHealer
		}
		r.state = State{LastRestart: r.at.Add(-time.Second), Incident: "peer"}
		r.statuses[GatewayUnit] = Status{Active: "active", Invocation: "new-login", Started: r.at.Add(-time.Second)}
		return func() {}, nil
	}
	assertRun(t, r, "already-restarted", "start "+AdapterUnits[0])
	if attempts != 2 || len(r.waits) != 1 {
		t.Fatalf("attempts=%d waits=%v", attempts, r.waits)
	}
}
func TestGatewayGlobalLockWaitDoesNotResetDeadline(t *testing.T) {
	r := fixture()
	start := r.at
	r.onGlobalLock = func() (func(), error) { return nil, ErrAnotherHealer }
	assertRun(t, r, "login-timeout")
	if r.at.Sub(start) != 10*time.Minute {
		t.Fatalf("wait=%s", r.at.Sub(start))
	}
}

func TestGatewaySerializationFailuresStopBeforeMutation(t *testing.T) {
	for _, kind := range []string{"lock", "log", "wait", "slow-lock"} {
		t.Run(kind, func(t *testing.T) {
			r := fixture()
			r.onGlobalLock = func() (func(), error) {
				if kind == "lock" {
					return nil, fmt.Errorf("synthetic lock failure")
				}
				if kind == "slow-lock" {
					r.at = r.at.Add(10 * time.Minute)
				}
				return nil, ErrAnotherHealer
			}
			if kind == "log" {
				r.failAt = map[string]int{"log": 1}
			}
			if kind == "wait" {
				r.failAt = map[string]int{"wait": 1}
			}
			err := r.run()
			if (err != nil) != (kind != "slow-lock") || len(r.calls) != 0 || r.globalLocked {
				t.Fatalf("err=%v calls=%v", err, r.calls)
			}
			if kind == "slow-lock" && len(r.waits) != 0 {
				t.Fatal("waited after deadline")
			}
		})
	}
}
func TestGatewayLockWaitConsumesLoginBudget(t *testing.T) {
	r := fixture()
	start := r.at
	attempts := 0
	r.onGlobalLock = func() (func(), error) {
		attempts++
		if attempts == 1 {
			r.at = r.at.Add(9 * time.Minute)
			return nil, ErrAnotherHealer
		}
		return func() {}, nil
	}
	r.login = false
	assertRun(t, r, "login-timeout", "restart "+GatewayUnit)
	if r.at.Sub(start) != 10*time.Minute {
		t.Fatalf("recovery renewed deadline: %s", r.at.Sub(start))
	}
}
func TestGatewayOwnRemedyWithdrawnBeforeStart(t *testing.T) {
	r := fixture()
	r.onAct = func(action, _ string) {
		if action == "restart" {
			r.journals["failed-run"] = nil
		}
	}
	assertRun(t, r, "no-remedy", "restart "+GatewayUnit)
}
func TestGatewayAlreadyRestartedStillRequiresFreshGates(t *testing.T) {
	for _, kind := range []string{"logged-out", "inactive", "counter", "owner", "replacement", "deadline", "window", "loss-equal", "loss-after"} {
		t.Run(kind, func(t *testing.T) {
			r := fixture()
			r.state = State{LastRestart: r.at.Add(-time.Minute), Incident: "peer"}
			prior := r.state
			gateway := Status{Active: "active", Invocation: "new-login", Started: r.loss.Add(time.Second)}
			row := "logged-out"
			switch kind {
			case "logged-out":
				r.journals["new-login"] = []string{"LOGGED_OUT"}
			case "inactive":
				gateway.Active = "inactive"
			case "counter":
				r.input.Counter = []byte("retained metadata")
				row = "login-problem"
			case "loss-equal":
				gateway.Started = r.loss
				row = "same-incident"
			case "loss-after":
				gateway.Started = r.loss.Add(-time.Second)
				row = "same-incident"
			case "owner", "replacement", "deadline", "window":
				r.onLines = func(id string) {
					if id != "new-login" {
						return
					}
					switch kind {
					case "owner":
						s := r.statuses[AdapterUnits[0]]
						s.Active = "inactive"
						r.statuses[AdapterUnits[0]] = s
					case "replacement":
						s := r.statuses[GatewayUnit]
						s.Invocation = "replacement"
						r.statuses[GatewayUnit] = s
					case "deadline":
						r.at = r.at.Add(10 * time.Minute)
					case "window":
						r.input.Schedule = []byte(`{"windows":[{"days":["Mon"],"start":"15:00","end":"16:00","zone":"UTC"}]}`)
					}
				}
				row = map[string]string{"owner": "owner-stopped", "replacement": "logged-out", "deadline": "login-timeout", "window": "window"}[kind]
			}
			r.statuses[GatewayUnit] = gateway
			// Already recovered accounts can hold relay locks; an adapter-only start
			// must not acquire them (server.md, Gateway healer).
			r.lockErr = ErrOrderBusy
			assertRun(t, r, row)
			if r.state != prior {
				t.Fatal("gate refusal changed restart reservation")
			}
		})
	}
}
func TestGatewayAlreadyRestartedDoesNotNeedRelayExclusion(t *testing.T) {
	r := fixture()
	r.lockErr = ErrOrderBusy
	r.statuses[GatewayUnit] = Status{Active: "active", Invocation: "new-login", Started: r.loss.Add(time.Second)}
	assertRun(t, r, "already-restarted", "start "+AdapterUnits[0])
	if r.state != (State{}) {
		t.Fatal("adapter start reserved a restart")
	}
}

// The mutation gates follow server.md's fresh-evidence invariant: both status
// queries precede a fresh counter/schedule read, with the owner veto last.
func TestActionCheckEvidenceOrder(t *testing.T) {
	r := fixture()
	var steps []string
	r.onLines = func(string) { steps = append(steps, "journal") }
	r.onStatus = func(unit string) { steps = append(steps, "status "+unit) }
	r.onRead = func() { steps = append(steps, "read") }
	checks := actionChecks{clock: r, reader: r, journal: r, ctl: r}
	row, err := checks.check(AdapterUnits[0], r.statuses[AdapterUnits[0]], r.statuses[GatewayUnit])
	if err != nil || row != "" {
		t.Fatalf("row=%q err=%v", row, err)
	}
	want := []string{"journal", "status " + GatewayUnit, "read", "status " + AdapterUnits[0], "read"}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("evidence order=%v, want %v", steps, want)
	}
}

func TestAdapterStatusCannotHideCounterOrScheduleChange(t *testing.T) {
	for _, change := range []string{"counter", "schedule"} {
		t.Run(change, func(t *testing.T) {
			r := fixture()
			r.onStatus = func(unit string) {
				if unit == AdapterUnits[0] && !r.state.LastRestart.IsZero() {
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
			assertSafetyTrace(t, r, append(eligibleRows("reserve-restart", "restart"), "gateway-heal: row="+row+" step=before-restart"), nil)
		})
	}
}
