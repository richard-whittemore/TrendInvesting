package gatewayheal

// Invariant (docs/manual/server.md, Gateway healer; ADR 0025, scheduled
// interruptions; docs/development.md, Ending processes): never add a login
// gateway-start would refuse, run in an IBKR window or 23:40–01:00 ET,
// restart twice for an incident, or restart between order hand-over and its
// journal record. All clocks, journals and systemctl calls below are synthetic;
// the systemctl stand-in records calls and never executes or signals anything.
//
// Full case table:
// exit 3 + remedy, outside windows, logged in, no restart for 2h -> restart
// once, await Login has completed for at most 10m, then start adapter; log steps.
// same, restart within 2h -> nothing; existing operational page stands.
// scheduled window or 23:40–01:00 ET -> wait for end, re-check once.
// nonzero login-failures counter or retries stopped -> nothing.
// LOGGED_OUT -> nothing; existing logout handling.
// any other adapter exit status -> nothing.
// restarted, no login within 10m -> stop; existing page stands.

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

type rig struct {
	onRead                                                                    func()
	failAt                                                                    map[string]int
	seen                                                                      map[string]int
	onAcquire                                                                 func()
	onLines                                                                   func(string)
	at                                                                        time.Time
	input                                                                     Inputs
	state                                                                     State
	statuses                                                                  map[string]Status
	journals                                                                  map[string][]string
	calls                                                                     []string
	logs                                                                      []string
	waits                                                                     []time.Duration
	reads                                                                     int
	onWait                                                                    func()
	login                                                                     bool
	locked                                                                    bool
	lockErr, readErr, saveErr, journalErr, statusErr, actErr, logErr, waitErr error
}

func fixture() *rig {
	at := time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC)
	return &rig{at: at, input: Inputs{Schedule: []byte(`{"windows":[]}`)},
		statuses: map[string]Status{AdapterUnits[0]: {Active: "failed", Code: 1, Exit: 3, Invocation: "failed-run", Finished: at.Add(-time.Minute)}, GatewayUnit: {Active: "active", Invocation: "old-login"}},
		journals: map[string][]string{"failed-run": {Remedy}, "old-login": {"Login has completed"}, "new-login": {"Login has completed"}}, login: true}
}
func (r *rig) Now() time.Time { return r.at }
func (r *rig) Wait(d time.Duration) error {
	r.waits = append(r.waits, d)
	r.at = r.at.Add(d)
	if r.onWait != nil {
		r.onWait()
	}
	return errors.Join(r.waitErr, r.fault("wait"))
}
func (r *rig) Read() (Inputs, error) {
	r.reads++
	if r.onRead != nil {
		r.onRead()
	}
	return r.input, errors.Join(r.readErr, r.fault("read"))
}
func (r *rig) Load() (State, error) { return r.state, r.fault("load") }
func (r *rig) Save(s State) error {
	if r.saveErr == nil {
		r.state = s
	}
	return errors.Join(r.saveErr, r.fault("save"))
}
func (r *rig) Status(unit string) (Status, error) {
	return r.statuses[unit], errors.Join(r.statusErr, r.fault("status"))
}
func (r *rig) Lines(_, invocation string) ([]string, error) {
	if r.onLines != nil {
		r.onLines(invocation)
	}
	return r.journals[invocation], errors.Join(r.journalErr, r.fault("journal"))
}
func (r *rig) Act(action, unit string) error {
	r.calls = append(r.calls, action+" "+unit)
	if action == "restart" {
		if !r.locked {
			panic("restart without order exclusion")
		}
		r.statuses[GatewayUnit] = Status{Active: "active", Invocation: "new-login"}
		if !r.login {
			r.journals["new-login"] = nil
		}
	} else if r.locked {
		panic("adapter started while relay locks held")
	}
	return errors.Join(r.actErr, r.fault("act"))
}
func (r *rig) Acquire() (func(), error) {
	if r.lockErr != nil {
		return nil, r.lockErr
	}
	if r.onAcquire != nil {
		r.onAcquire()
	}
	r.locked = true
	return func() { r.locked = false }, nil
}
func (r *rig) Log(line string) error {
	r.logs = append(r.logs, line)
	return errors.Join(r.logErr, r.fault("log"))
}
func (r *rig) run() error { return Run(AdapterUnits[0], r, r, r, r, r, r, r.Log) }
func assertRun(t *testing.T, r *rig, row string, calls ...string) {
	t.Helper()
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.calls, calls) {
		t.Fatalf("calls=%v, want %v", r.calls, calls)
	}
	if !strings.Contains(strings.Join(r.logs, "\n"), "gateway-heal: row="+row) {
		t.Fatalf("missing decision %s: %v", row, r.logs)
	}
}
func TestRemedyRestartsOnceThenStartsAdapter(t *testing.T) {
	r := fixture()
	assertRun(t, r, "eligible", "restart "+GatewayUnit, "start "+AdapterUnits[0])
	if r.state.LastRestart.IsZero() || r.state.Incident != "failed-run" {
		t.Fatal("restart not durably reserved")
	}
	for _, step := range []string{"restart", "login-completed", "adapter-start"} {
		if !strings.Contains(strings.Join(r.logs, "\n"), "step="+step) {
			t.Errorf("missing %s", step)
		}
	}
}
func TestRestartWithinTwoHoursDoesNothing(t *testing.T) {
	r := fixture()
	r.state.LastRestart = r.at.Add(-119 * time.Minute)
	assertRun(t, r, "rate-limit")
}
func TestWindowWaitsAndRechecksOnce(t *testing.T) {
	for _, tc := range []struct {
		name, at, schedule string
		wait               time.Duration
	}{
		{"scheduled", "2026-11-02T15:00:00Z", `{"windows":[{"days":["Mon"],"start":"15:00","end":"16:00","zone":"UTC"}]}`, time.Hour},
		{"nightly", "2026-11-03T04:40:00Z", `{"windows":[]}`, 80 * time.Minute},
		{"DST", "2026-11-01T03:40:00Z", `{"windows":[]}`, 140 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fixture()
			r.at, _ = time.Parse(time.RFC3339, tc.at)
			r.input.Schedule = []byte(tc.schedule)
			assertRun(t, r, "window", "restart "+GatewayUnit, "start "+AdapterUnits[0])
			if len(r.waits) != 1 || r.waits[0] != tc.wait || r.reads < 2 {
				t.Fatalf("waits=%v reads=%d", r.waits, r.reads)
			}
		})
	}
	r := fixture()
	r.at = time.Date(2026, 11, 3, 5, 0, 0, 0, time.UTC)
	r.onWait = func() { r.input.Counter = []byte("1\n") }
	assertRun(t, r, "login-problem")
}
func TestLoginCounterOrStoppedRetriesDoesNothing(t *testing.T) {
	for _, raw := range []string{"1\n", "garbled", "reset 1 2 1 1\n"} {
		r := fixture()
		r.input.Counter = []byte(raw)
		assertRun(t, r, "login-problem")
	}
	r := fixture()
	r.journals["old-login"] = append(r.journals["old-login"], "gateway-start: login retries stopped")
	assertRun(t, r, "login-problem")
	r = fixture()
	r.journals["old-login"] = append(r.journals["old-login"], "gateway-start: reset wait: login refused")
	assertRun(t, r, "login-problem")
}
func TestLoggedOutDoesNothing(t *testing.T) {
	r := fixture()
	r.journals["old-login"] = append(r.journals["old-login"], "LOGGED_OUT")
	assertRun(t, r, "logged-out")
}
func TestOtherAdapterExitDoesNothing(t *testing.T) {
	for _, code := range []int{0, 1, 2, 77} {
		r := fixture()
		s := r.statuses[AdapterUnits[0]]
		s.Exit = code
		r.statuses[AdapterUnits[0]] = s
		assertRun(t, r, "other-exit")
	}
}
func TestNoLoginWithinTenMinutesStops(t *testing.T) {
	r := fixture()
	r.login = false
	before := r.at
	assertRun(t, r, "login-timeout", "restart "+GatewayUnit)
	if r.at.Sub(before) != 10*time.Minute {
		t.Fatalf("waited %s", r.at.Sub(before))
	}
}
func TestIncidentCannotRestartAgainAfterTwoHours(t *testing.T) {
	r := fixture()
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	r.at = r.at.Add(3 * time.Hour)
	r.calls = nil
	assertRun(t, r, "same-incident")
}
func TestInFlightOrderPreventsRestart(t *testing.T) {
	r := fixture()
	r.lockErr = fmt.Errorf("boost: %w", ErrOrderBusy)
	assertRun(t, r, "order-busy")
}

// A guard that cannot be checked (a permission or file fault) also does
// nothing, but the journal names the fault, never a busy order.
func TestAnUncheckableGuardIsJournalledAsSuch(t *testing.T) {
	r := fixture()
	r.lockErr = errors.New("open pending.json.lock: read-only file system")
	assertRun(t, r, "guard-unreadable")
	if !strings.Contains(strings.Join(r.logs, "\n"), "read-only file system") {
		t.Fatalf("the fault is not named: %v", r.logs)
	}
}
func TestRemedyMustBelongToFailedInvocation(t *testing.T) {
	r := fixture()
	r.journals["failed-run"] = nil
	assertRun(t, r, "no-remedy")
}
func TestUnknownEvidenceFailsClosed(t *testing.T) {
	for _, which := range []string{"read", "journal", "status", "save", "log", "wait", "act"} {
		t.Run(which, func(t *testing.T) {
			r := fixture()
			e := errors.New("synthetic fault")
			switch which {
			case "read":
				r.readErr = e
			case "journal":
				r.journalErr = e
			case "status":
				r.statusErr = e
			case "save":
				r.saveErr = e
			case "log":
				r.logErr = e
			case "wait":
				r.login = false
				r.waitErr = e
			case "act":
				r.actErr = e
			}
			if r.run() == nil {
				t.Fatal("fault was swallowed")
			}
			if which != "wait" && which != "act" && len(r.calls) != 0 {
				t.Fatal(r.calls)
			}
		})
	}
}

func TestUnsettledHandoverFailsClosed(t *testing.T) {
	for _, raw := range []string{`{"order":"handing-over"}`, `{"order":"handing-over","order":"sent"}`, `{"order":"unknown"}`, `null`, `[]`, `{`, `{} {}`} {
		if HandoverClear([]byte(raw)) {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{`{}`, `{"order":"sent"}`, `{"order":"never-sent","other":"at-broker"}`, `{"order":"resend:2026-11-02"}`} {
		if !HandoverClear([]byte(raw)) {
			t.Errorf("refused %s", raw)
		}
	}
}
func TestWindowUnionAndInvalidSchedules(t *testing.T) {
	now := time.Date(2026, 11, 2, 22, 0, 0, 0, time.UTC)
	end, err := WindowEnd([]byte(`{"windows":[{"days":["daily"],"start":"22:00","end":"05:30","zone":"UTC"}]}`), now)
	if err != nil || !end.Equal(time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("end=%s err=%v", end, err)
	}
	for _, raw := range []string{`broken`, `{"windows":[{"days":["daily"],"start":"01:00","end":"23:40","zone":"America/New_York"}]}`} {
		if _, err = WindowEnd([]byte(raw), now); err == nil {
			t.Fatal("unbounded or malformed windows accepted")
		}
	}
}
func TestOnlyNamedUnitActionsAreAllowed(t *testing.T) {
	for _, unit := range AdapterUnits {
		if err := ValidateAction("start", unit); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateAction("restart", GatewayUnit); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAction("stop", GatewayUnit); err == nil {
		t.Fatal("stop allowed")
	}
	r := fixture()
	if err := Run("other.service", r, r, r, r, r, r, r.Log); err != nil || len(r.calls) != 0 {
		t.Fatal(err, r.calls)
	}
}

func TestClockRollbackCannotBypassRestartLimit(t *testing.T) {
	r := fixture()
	r.state.LastRestart = r.at.Add(-3 * time.Hour)
	r.onAcquire = func() { r.at = r.at.Add(-2 * time.Hour) }
	assertRun(t, r, "rate-limit")
}
func TestSlowJournalCannotExtendLoginDeadline(t *testing.T) {
	r := fixture()
	r.onLines = func(invocation string) {
		if invocation == "new-login" {
			r.at = r.at.Add(11 * time.Minute)
		}
	}
	assertRun(t, r, "login-timeout", "restart "+GatewayUnit)
}
func TestWindowBeginningDuringEvidenceReadPreventsRestart(t *testing.T) {
	r := fixture()
	r.at = time.Date(2026, 11, 3, 4, 39, 59, 0, time.UTC)
	r.onLines = func(invocation string) {
		if invocation == "old-login" {
			r.at = r.at.Add(time.Second)
		}
	}
	assertRun(t, r, "window")
}

func (r *rig) fault(boundary string) error {
	if r.seen == nil {
		r.seen = map[string]int{}
	}
	r.seen[boundary]++
	if r.failAt[boundary] == r.seen[boundary] {
		return errors.New("synthetic boundary failure")
	}
	return nil
}
func TestEveryEvidenceAndAuditFailureStopsRecovery(t *testing.T) {
	for boundary, count := range map[string]int{"read": 4, "status": 5, "journal": 3, "log": 5, "load": 1, "save": 1, "act": 2} {
		for n := 1; n <= count; n++ {
			t.Run(fmt.Sprintf("%s-%d", boundary, n), func(t *testing.T) {
				r := fixture()
				r.failAt = map[string]int{boundary: n}
				if r.run() == nil {
					t.Fatal("failure swallowed")
				}
				if r.locked {
					t.Fatal("relay locks leaked")
				}
				if boundary == "act" && n == 1 && len(r.calls) != 1 {
					t.Fatal("started after failed restart")
				}
			})
		}
	}
	for _, boundary := range []string{"log", "wait"} {
		r := fixture()
		r.at = time.Date(2026, 11, 3, 5, 0, 0, 0, time.UTC)
		r.failAt = map[string]int{boundary: 1}
		if r.run() == nil || len(r.calls) != 0 {
			t.Fatal("window failure allowed restart", boundary)
		}
	}
}
func TestChangingEvidenceAfterWaitOrRestartFailsClosed(t *testing.T) {
	for _, phase := range []string{"before", "after"} {
		for _, change := range []string{"adapter", "schedule-invalid", "schedule-window", "counter"} {
			t.Run(phase+"-"+change, func(t *testing.T) {
				r := fixture()
				mutate := func() {
					switch change {
					case "adapter":
						s := r.statuses[AdapterUnits[0]]
						s.Invocation = "new-adapter"
						r.statuses[AdapterUnits[0]] = s
					case "schedule-invalid":
						r.input.Schedule = []byte("broken")
					case "schedule-window":
						r.input.Schedule = []byte(`{"windows":[{"days":["daily"],"start":"14:00","end":"16:00","zone":"UTC"}]}`)
					case "counter":
						r.input.Counter = []byte("1\n")
					}
				}
				if phase == "before" {
					r.onAcquire = mutate
				} else {
					r.onLines = func(id string) {
						if id == "new-login" {
							mutate()
						}
					}
				}
				err := r.run()
				if (err != nil) != (change == "schedule-invalid") {
					t.Fatal(err)
				}
				want := 0
				if phase == "after" {
					want = 1
				}
				if len(r.calls) != want || r.locked {
					t.Fatalf("calls=%v locked=%v", r.calls, r.locked)
				}
			})
		}
	}
	r := fixture()
	r.input.Schedule = []byte("broken")
	if r.run() == nil || len(r.calls) != 0 {
		t.Fatal("invalid initial schedule accepted")
	}
	r = fixture()
	r.onLines = func(id string) {
		if id == "old-login" {
			r.input.Schedule = []byte("broken")
		}
	}
	// A schedule edit during journal collection must be caught before restart.
	if r.run() == nil || len(r.calls) != 0 {
		t.Fatal("changed schedule accepted", r.calls)
	}
}
func TestTimeoutAlsoAppliesAfterFinalStatusRead(t *testing.T) {
	r := fixture()
	r.onLines = func(id string) {
		if id == "new-login" {
			r.at = r.at.Add(9 * time.Minute)
		}
	}
	r.onRead = func() {
		if r.reads == 4 {
			r.at = r.at.Add(time.Minute)
		}
	}
	assertRun(t, r, "login-timeout", "restart "+GatewayUnit)
}
func TestHandoverMalformedTokensFailClosed(t *testing.T) {
	for _, raw := range []string{`{`, `{"a":"sent","a":"sent"}`, `{12}`, `{"a":1}`, `{"a":"sent"`} {
		if HandoverClear([]byte(raw)) {
			t.Fatal(raw)
		}
	}
}

func TestCounterChangingDuringEvidenceReadPreventsRestart(t *testing.T) {
	r := fixture()
	r.onLines = func(id string) {
		if id == "old-login" {
			r.input.Counter = []byte("1\n")
		}
	}
	assertRun(t, r, "login-problem")
}
