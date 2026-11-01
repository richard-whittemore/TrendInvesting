package gatewayheal

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// These scenarios pin docs/manual/server.md, Gateway healer: once per incident,
// a two-hour restart limit, bounded login, and fresh evidence before every
// mutation. The embedded rig only records actions; no command is executed.
type safetyRig struct {
	*rig
	onLoss    func()
	deadlines []time.Time
}

func (r *safetyRig) Loss(unit, invocation string) (time.Time, error) {
	if r.onLoss != nil {
		r.onLoss()
	}
	return r.loss, r.fault("loss")
}

func (r *safetyRig) SetDeadline(deadline time.Time) {
	r.deadlines = append(r.deadlines, deadline)
}

func eligibleRows(steps ...string) []string {
	var rows []string
	for _, step := range steps {
		rows = append(rows, "gateway-heal: row=eligible step="+step)
	}
	return rows
}

func assertSafetyTrace(t *testing.T, r *rig, rows, calls []string) {
	t.Helper()
	if !reflect.DeepEqual(r.logs, rows) {
		t.Errorf("journal=%q, want %q", r.logs, rows)
	}
	if !reflect.DeepEqual(r.calls, calls) {
		t.Errorf("systemctl actions=%q, want %q", r.calls, calls)
	}
	if r.locked {
		t.Error("relay locks leaked")
	}
}

func TestIncidentAndRateLimitSafetyRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
		same bool
		end  time.Duration
		row  string
	}{
		{"same-invocation-after-two-hours", 3 * time.Hour, true, time.Minute, "same-incident"},
		{"different-invocation-before-restart", 3 * time.Hour, false, -time.Second, "same-incident"},
		{"different-invocation-at-restart", 3 * time.Hour, false, 0, "same-incident"},
		{"just-under-two-hours", 2*time.Hour - time.Nanosecond, false, time.Minute, "rate-limit"},
		{"exactly-two-hours", 2 * time.Hour, false, time.Minute, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fixture()
			r.state = State{LastRestart: r.at.Add(-tc.age), Incident: "previous-invocation"}
			failed := r.statuses[AdapterUnits[0]]
			failed.Finished = r.state.LastRestart.Add(tc.end)
			r.statuses[AdapterUnits[0]] = failed
			r.loss = failed.Finished.Add(-time.Minute)
			if tc.same {
				r.state.Incident = failed.Invocation
			}
			prior := r.state
			if err := r.run(); err != nil {
				t.Fatal(err)
			}
			if tc.row != "" {
				assertSafetyTrace(t, r, []string{"gateway-heal: row=" + tc.row + " step=no-action"}, nil)
				if r.state != prior {
					t.Fatal("no-action changed the restart reservation")
				}
			} else {
				assertSafetyTrace(t, r, eligibleRows("reserve-restart", "restart", "wait-login", "login-completed", "adapter-start"), []string{"restart " + GatewayUnit, "start " + AdapterUnits[0]})
			}
		})
	}
}

func TestIncidentLossMustPrecedeFailure(t *testing.T) {
	for _, kind := range []string{"missing", "equal", "after"} {
		t.Run(kind, func(t *testing.T) {
			r := fixture()
			switch kind {
			case "missing":
				r.loss = time.Time{}
			case "equal":
				r.loss = r.statuses[AdapterUnits[0]].Finished
			case "after":
				r.loss = r.statuses[AdapterUnits[0]].Finished.Add(time.Nanosecond)
			}
			if err := r.run(); err != nil {
				t.Fatal(err)
			}
			assertSafetyTrace(t, r, []string{"gateway-heal: row=no-remedy step=missing-incident-loss"}, nil)
			if r.state != (State{}) {
				t.Fatal("missing incident consumed restart allowance")
			}
		})
	}
}

func TestIncidentLossReadRechecksRateLimit(t *testing.T) {
	r := &safetyRig{rig: fixture()}
	r.state.LastRestart = r.at.Add(-3 * time.Hour)
	prior := r.state
	r.onLoss = func() { r.at = r.at.Add(-2 * time.Hour) }
	if err := Run(AdapterUnits[0], r, r, r, r, r, r, r.Log); err != nil {
		t.Fatal(err)
	}
	assertSafetyTrace(t, r.rig, []string{"gateway-heal: row=rate-limit step=no-action"}, nil)
	if r.state != prior {
		t.Fatal("rate-limited incident reserved a restart")
	}
}

func TestSystemctlReceivesTheSingleLoginDeadline(t *testing.T) {
	r := &safetyRig{rig: fixture()}
	deadline := r.at.Add(10 * time.Minute)
	if err := Run(AdapterUnits[0], r, r, r, r, r, r, r.Log); err != nil {
		t.Fatal(err)
	}
	assertSafetyTrace(t, r.rig, eligibleRows("reserve-restart", "restart", "wait-login", "login-completed", "adapter-start"), []string{"restart " + GatewayUnit, "start " + AdapterUnits[0]})
	if !reflect.DeepEqual(r.deadlines, []time.Time{deadline}) {
		t.Fatalf("deadlines=%v, want only %v", r.deadlines, deadline)
	}
}

// Evidence errors propagate to the composition root's evidence-error row;
// Run itself must preserve the journal prefix and stop further actions
// (docs/manual/server.md, Gateway healer, unreadable evidence).
func TestLateEvidenceFailuresPreserveTraceAndStopActions(t *testing.T) {
	for _, tc := range []struct {
		name, boundary string
		n, steps       int
		calls          []string
	}{
		{"incident-loss", "loss", 1, 0, nil},
		{"restart-status", "status", 7, 2, []string{"restart " + GatewayUnit}},
		{"login-status", "status", 8, 3, []string{"restart " + GatewayUnit}},
		{"login-journal", "journal", 4, 3, []string{"restart " + GatewayUnit}},
		{"login-counter", "read", 6, 3, []string{"restart " + GatewayUnit}},
		{"adapter-remedy", "journal", 5, 4, []string{"restart " + GatewayUnit}},
		{"adapter-recheck", "read", 7, 5, []string{"restart " + GatewayUnit}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &safetyRig{rig: fixture()}
			r.failAt = map[string]int{tc.boundary: tc.n}
			err := Run(AdapterUnits[0], r, r, r, r, r, r, r.Log)
			if err == nil || err.Error() != "synthetic boundary failure" {
				t.Fatalf("error=%v, want injected evidence failure", err)
			}
			var rows []string
			if tc.steps > 0 {
				rows = eligibleRows("reserve-restart", "restart", "wait-login", "login-completed", "adapter-start")[:tc.steps]
			}
			assertSafetyTrace(t, r.rig, rows, tc.calls)
		})
	}
}

func TestRestartIdentityMustBeConfirmedAndRemainStable(t *testing.T) {
	for _, kind := range []string{"missing", "unchanged", "old-start", "replacement-invocation", "replacement-start"} {
		t.Run(kind, func(t *testing.T) {
			r := fixture()
			r.onAct = func(_, _ string) {
				s := r.statuses[GatewayUnit]
				switch kind {
				case "missing":
					s.Invocation = ""
				case "unchanged":
					s.Invocation = "old-login"
				case "old-start":
					s.Started = r.at.Add(-time.Second)
				}
				r.statuses[GatewayUnit] = s
			}
			r.onStatus = func(unit string) {
				if unit != GatewayUnit || r.seen["status"] != 7 {
					return
				}
				s := r.statuses[unit]
				switch kind {
				case "replacement-invocation":
					s.Invocation = "replacement"
				case "replacement-start":
					s.Started = s.Started.Add(time.Second)
				}
				r.statuses[unit] = s
			}
			rows := eligibleRows("reserve-restart", "restart")
			step := "restart-identity-unconfirmed"
			if kind == "replacement-invocation" || kind == "replacement-start" {
				rows = append(rows, eligibleRows("wait-login")...)
				step = "gateway-changed"
			}
			if err := r.run(); err != nil {
				t.Fatal(err)
			}
			assertSafetyTrace(t, r, append(rows, "gateway-heal: row=logged-out step="+step), []string{"restart " + GatewayUnit})
		})
	}
}

func TestCounterReadAtDeadlineDoesNotWaitAgain(t *testing.T) {
	r := fixture()
	r.login = false
	r.onRead = func() {
		if r.reads == 6 {
			r.at = r.at.Add(10 * time.Minute)
		}
	}
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	rows := append(eligibleRows("reserve-restart", "restart", "wait-login"), "gateway-heal: row=login-timeout step=stop-existing-page-stands")
	assertSafetyTrace(t, r, rows, []string{"restart " + GatewayUnit})
	if len(r.waits) != 0 {
		t.Fatalf("waited after deadline: %v", r.waits)
	}
}

func allFailedRig() *rig {
	r := fixture()
	for _, unit := range AdapterUnits[1:] {
		r.statuses[unit] = Status{Active: "failed", Code: 1, Exit: 3, Invocation: unit + "-failure", Finished: r.at.Add(-time.Minute)}
		r.journals[unit+"-failure"] = []string{Remedy}
	}
	return r
}

func TestGatewayEachTriggerStartsOnlyOwnAdapter(t *testing.T) {
	for _, trigger := range AdapterUnits {
		t.Run(trigger, func(t *testing.T) {
			r := allFailedRig()
			if err := Run(trigger, r, r, r, r, r, r, r.Log); err != nil {
				t.Fatal(err)
			}
			assertSafetyTrace(t, r, eligibleRows("reserve-restart", "restart", "wait-login", "login-completed", "adapter-start"), []string{"restart " + GatewayUnit, "start " + trigger})
		})
	}
}

func TestEveryMutationRechecksAfterItsAuditWrite(t *testing.T) {
	for target := -1; target < 1; target++ {
		for _, change := range []string{"deadline", "counter", "gateway", "owner", "window", "audit-error"} {
			t.Run(fmt.Sprintf("target-%d/%s", target, change), func(t *testing.T) {
				r := allFailedRig()
				started := r.at
				rows := eligibleRows("reserve-restart", "restart")
				var calls []string
				step := "before-restart"
				if target >= 0 {
					rows = append(rows, eligibleRows("wait-login", "login-completed")...)
					calls = append(calls, "restart "+GatewayUnit)
					for i := 0; i < target; i++ {
						rows = append(rows, eligibleRows("adapter-start")...)
						calls = append(calls, "start "+AdapterUnits[i])
					}
					rows = append(rows, eligibleRows("adapter-start")...)
					step = "before-adapter-start"
				}
				row := map[string]string{"deadline": "login-timeout", "counter": "login-problem", "gateway": "logged-out", "owner": "owner-stopped", "window": "window", "audit-error": "login-timeout"}[change]
				targetLog := len(rows)
				log := func(line string) error {
					err := r.Log(line)
					if len(r.logs) == targetLog {
						switch change {
						case "deadline", "audit-error":
							r.at = started.Add(10 * time.Minute)
						case "counter":
							r.input.Counter = []byte("1\n")
						case "gateway":
							s := r.statuses[GatewayUnit]
							s.Invocation = "independent-login"
							r.statuses[GatewayUnit] = s
						case "owner":
							s := r.statuses[AdapterUnits[max(0, target)]]
							s.Active = "inactive"
							r.statuses[AdapterUnits[max(0, target)]] = s
						case "window":
							r.input.Schedule = []byte(`{"windows":[{"days":["Mon"],"start":"15:00","end":"16:00","zone":"UTC"}]}`)
						}
					}
					return err
				}
				if change == "audit-error" {
					r.failAt = map[string]int{"log": targetLog + 1}
				}
				rows = append(rows, "gateway-heal: row="+row+" step="+step)
				err := Run(AdapterUnits[0], r, r, r, r, r, r, log)
				if change == "audit-error" {
					if err == nil || err.Error() != "synthetic boundary failure" {
						t.Fatalf("error=%v, want audit failure", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				assertSafetyTrace(t, r, rows, calls)
				if r.state.LastRestart != started {
					t.Fatal("refusal discarded the durable reservation")
				}
			})
		}
	}
}

// The Sunday margin and the mandatory nightly window form a union, including
// when the cold restart's margin starts Saturday (server.md, Gateway healer).
func TestColdWindowQuotesAndBoundaryUnion(t *testing.T) {
	for _, tc := range []struct {
		name, env, at, end string
	}{
		{"double-quoted-start", "TI_GATEWAY_COLD_RESTART=\"12:00\"", "2026-11-01T16:50:00Z", "2026-11-01T17:10:00Z"},
		{"single-quoted-inside", "TI_GATEWAY_COLD_RESTART='12:00'", "2026-11-01T17:09:59Z", "2026-11-01T17:10:00Z"},
		{"before-margin", "TI_GATEWAY_COLD_RESTART='12:00'", "2026-11-01T16:49:59Z", ""},
		{"at-end", "TI_GATEWAY_COLD_RESTART='12:00'", "2026-11-01T17:10:00Z", ""},
		{"saturday-margin", "TI_GATEWAY_COLD_RESTART=00:05", "2026-11-08T04:55:00Z", "2026-11-08T06:00:00Z"},
		{"saturday-union-end", "TI_GATEWAY_COLD_RESTART=00:05", "2026-11-08T06:00:00Z", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fixture()
			var err error
			r.at, err = time.Parse(time.RFC3339, tc.at)
			if err != nil {
				t.Fatal(err)
			}
			r.input.GatewayEnv = []byte("# Gateway settings\nTI_GATEWAY_OTHER=ignored\n" + tc.env + "\n")
			var end time.Time
			if tc.end != "" {
				end, err = time.Parse(time.RFC3339, tc.end)
				if err != nil {
					t.Fatal(err)
				}
			}
			got, err := r.input.windowEnd(r.at)
			if err != nil || !got.Equal(end) {
				t.Fatalf("window end=%v error=%v, want %v", got, err, end)
			}
			var rows []string
			var waits []time.Duration
			if !end.IsZero() {
				rows = append(rows, "gateway-heal: row=window step=wait-until="+tc.end)
				waits = append(waits, end.Sub(r.at))
			}
			rows = append(rows, eligibleRows("reserve-restart", "restart", "wait-login", "login-completed", "adapter-start")...)
			if err := r.run(); err != nil {
				t.Fatal(err)
			}
			assertSafetyTrace(t, r, rows, []string{"restart " + GatewayUnit, "start " + AdapterUnits[0]})
			if !reflect.DeepEqual(r.waits, waits) {
				t.Fatalf("waits=%v, want %v", r.waits, waits)
			}
		})
	}
}
