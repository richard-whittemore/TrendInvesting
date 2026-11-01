package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

type policyRecorder struct {
	channel string
	alerts  []report.Alert
}

func (s *policyRecorder) name() string { return s.channel }
func (s *policyRecorder) send(a report.Alert, _ int) error {
	s.alerts = append(s.alerts, a)
	return nil
}

func TestPagingMatrix(t *testing.T) {
	for _, capital := range []bool{true, false} {
		for _, window := range []bool{true, false} {
			for _, near := range []bool{true, false} {
				for _, open := range []bool{true, false} {
					name := fmt.Sprintf("capital=%t/window=%t/near=%t/open=%t", capital, window, near, open)
					t.Run(name, func(t *testing.T) {
						start := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
						if near {
							start = time.Date(2026, 10, 30, 14, 0, 0, 0, time.UTC)
						}
						schedule := `{"windows":[]}`
						if window {
							schedule = `{"windows":[{"days":["daily"],"start":"00:00","end":"23:00","zone":"UTC"}]}`
						}
						windows, err := report.ParseIBKRSchedule([]byte(schedule))
						if err != nil {
							t.Fatal(err)
						}
						policy := &pagingPolicy{after: 30 * time.Minute, schedule: windows}
						var a report.Alert
						if capital {
							_, _, _, alerts, err := readJournal(writeJournal(t, t.TempDir()), "sample", start, time.Minute, nil)
							if err != nil {
								t.Fatal(err)
							}
							for _, alert := range alerts {
								if alert.Kind == report.KindHalted {
									a = alert
								}
							}
							if a.Key == "" {
								t.Fatal("synthetic journal did not produce Halted")
							}
						} else {
							journal := newStandInGateway(t)
							journal.ibc(stamp(start.Add(-time.Hour), ibcCompleted))
							journal.adapter(stamp(start.Add(-time.Minute), linkLost))
							ev, err := gatewayJournalChecker(journal.path(), []string{"ti-adapter-boost.service"})(start)
							if err != nil || !ev.connectivity {
								t.Fatalf("synthetic connectivity journal: %+v %v", ev, err)
							}
							a = report.IBKRNotice("synthetic", start, false, ev.said)
						}
						n, p, g := &policyRecorder{channel: "ntfy"}, &policyRecorder{channel: "pushover"}, &policyRecorder{channel: issuesChannel}
						senders := []sender{n, p, g}
						seen, sent, repeats := map[string]bool{}, map[string]time.Time{}, map[string]int{}
						send := func(at time.Time) {
							t.Helper()
							if err := deliver(senders, a, seen, sent, repeats, at, time.Hour, func() error { return nil }, policy); err != nil {
								t.Fatal(err)
							}
						}
						send(start)
						immediate := capital || near
						if len(n.alerts) != 1 || len(g.alerts) != 1 || len(p.alerts) != boolCount(immediate) {
							t.Fatalf("initial channels ntfy=%d pushover=%d issues=%d", len(n.alerts), len(p.alerts), len(g.alerts))
						}
						if window && !near && !capital && !strings.Contains(g.alerts[0].Body, "scheduled IBKR interruption") {
							t.Fatal("missing schedule label")
						}
						if open {
							send(start.Add(30 * time.Minute))
						} else {
							forgetAlert(a.Key, map[string]bool{}, seen, sent, repeats)
						}
						want := immediate || open && !window
						if len(p.alerts) != boolCount(want) {
							t.Fatalf("after delay pushover=%d want %d", len(p.alerts), boolCount(want))
						}
					})
				}
			}
		}
	}
}
func boolCount(v bool) int {
	if v {
		return 1
	}
	return 0
}

func TestPagingWindowEndAndDurableClock(t *testing.T) {
	schedule, err := report.ParseIBKRSchedule([]byte(`{"windows":[{"days":["Sat"],"start":"00:00","end":"12:00","zone":"America/New_York"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	policy := &pagingPolicy{after: 30 * time.Minute, schedule: schedule}
	a := report.Alert{Key: "synthetic", Kind: report.KindGatewayLoggedOut, Severity: report.Urgent, Repeat: true}
	p, g := &policyRecorder{channel: "pushover"}, &policyRecorder{channel: issuesChannel}
	seen, sent, repeats := map[string]bool{}, map[string]time.Time{}, map[string]int{}
	path := t.TempDir() + "/state.json"
	start := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
	for i, delta := range []time.Duration{0, 89 * time.Minute, 90 * time.Minute, 91 * time.Minute, 120 * time.Minute} {
		if err := deliver([]sender{p, g}, a, seen, sent, repeats, start.Add(delta), time.Hour, func() error { return saveState(path, seen, sent, repeats, nil) }, policy); err != nil {
			t.Fatal(err)
		}
		if i < 2 && len(p.alerts) != 0 {
			t.Fatal("paged before window end plus delay")
		}
		var err error
		seen, sent, repeats, _, err = loadState(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(p.alerts) != 2 {
		t.Fatalf("pages=%d want escalation and one reminder", len(p.alerts))
	}
	if len(g.alerts) != 3 {
		t.Fatalf("issues=%d want initial and two post-window reminders", len(g.alerts))
	}
}

func TestPagingGatewayJournalNotices(t *testing.T) {
	for _, tc := range []struct {
		name             string
		at               time.Time
		gateway, adapter []string
		want             string
		page             bool
	}{
		{"reset_before_one", time.Date(2026, 10, 31, 0, 40, 0, 0, newYork), []string{stamp(time.Date(2026, 10, 30, 23, 50, 0, 0, newYork), "gateway-start: reset wait: inside IBKR's nightly reset hour; waiting")}, nil, "reset wait", false},
		{"reset_at_one", time.Date(2026, 10, 31, 1, 0, 0, 0, newYork), []string{stamp(time.Date(2026, 10, 30, 23, 50, 0, 0, newYork), "gateway-start: reset wait: inside IBKR's nightly reset hour; waiting")}, nil, "logged out", true},
		{"later_start_overrides_reset", time.Date(2026, 10, 31, 0, 40, 0, 0, newYork), []string{stamp(time.Date(2026, 10, 30, 23, 50, 0, 0, newYork), "gateway-start: reset wait: waiting"), stamp(time.Date(2026, 10, 31, 0, 10, 0, 0, newYork), ibcStarting)}, nil, "logged out", true},
		{"weekend_connectivity", time.Date(2026, 10, 31, 9, 0, 0, 0, newYork), []string{stamp(time.Date(2026, 10, 31, 3, 35, 0, 0, newYork), ibcCompleted)}, []string{stamp(time.Date(2026, 10, 31, 5, 44, 0, 0, newYork), linkLost)}, "connectivity lost", false},
		{"session_connectivity", time.Date(2026, 10, 30, 8, 0, 0, 0, newYork), []string{stamp(time.Date(2026, 10, 30, 3, 35, 0, 0, newYork), ibcCompleted)}, []string{stamp(time.Date(2026, 10, 30, 7, 59, 0, 0, newYork), linkLost)}, "connectivity lost", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newStandInGateway(t)
			g.ibc(tc.gateway...)
			g.adapter(tc.adapter...)
			p, n, is := &policyRecorder{channel: "pushover"}, &policyRecorder{channel: "ntfy"}, &policyRecorder{channel: issuesChannel}
			sched, err := gatewayScheduleFrom(func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			windows, err := report.ParseIBKRSchedule([]byte(`{"windows":[{"days":["Sat"],"start":"05:00","end":"12:00","zone":"America/New_York"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			policy := &pagingPolicy{schedule: windows, after: 0}
			path := t.TempDir() + "/gateway.json"
			// Established incident: the threshold has already passed for genuine logout.
			since := tc.at.Add(-2 * time.Hour)
			raw := fmt.Sprintf(`{"logged_out_since":%q}`, since.Format(time.RFC3339))
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			step := func(at time.Time) {
				t.Helper()
				err := gatewayStep(path, gatewayJournalChecker(g.path(), []string{"ti-adapter-boost.service"}), func() (gatewayUnitSample, error) { return gatewayUnitSample{active: true}, nil }, "synthetic-host", sched, func() time.Time { return time.Time{} }, "", nil, []sender{p, n, is}, nil, func() time.Time { return at }, policy)
				if err != nil {
					t.Fatal(err)
				}
			}
			step(tc.at)
			if len(n.alerts) != 1 || len(is.alerts) != 1 || len(p.alerts) != boolCount(tc.page) {
				t.Fatalf("channels ntfy=%d issues=%d pushover=%d", len(n.alerts), len(is.alerts), len(p.alerts))
			}
			if !strings.Contains(n.alerts[0].Title, tc.want) {
				t.Fatalf("wrong title: %s", n.alerts[0].Title)
			}
			if tc.want != "logged out" && strings.Contains(n.alerts[0].Body, "check the paper login") {
				t.Fatal("notice tells owner to check password")
			}
			if tc.want == "connectivity lost" {
				g.adapter(append(tc.adapter, stamp(tc.at.Add(time.Minute), linkUp))...)
				step(tc.at.Add(time.Minute))
				h, err := loadHostGateway(path)
				if err != nil || h.Since != nil {
					t.Fatalf("restoration failed: %+v %v", h, err)
				}
			}
		})
	}
}

func TestPagingConfig(t *testing.T) {
	dir := t.TempDir()
	config := dir + "/reporter.env"
	schedule := dir + "/ibkr.json"
	get := func(key string) string {
		switch key {
		case "TI_REPORTER_CONFIG":
			return config
		case "TI_IBKR_MAINTENANCE_FILE":
			return schedule
		}
		return ""
	}
	p, err := loadPagingPolicy(get)
	if err != nil || p.after != 30*time.Minute {
		t.Fatalf("default=%+v %v", p, err)
	}
	for _, value := range []string{"0", "45m", "-1m", "invalid"} {
		if err := os.WriteFile(config, []byte("TI_ESCALATE_AFTER="+value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		p, err := loadPagingPolicy(get)
		invalid := value == "-1m" || value == "invalid"
		if (err != nil) != invalid {
			t.Fatalf("%s: %v", value, err)
		}
		if invalid && p.after != 0 {
			t.Fatal("invalid setting quiets pages")
		}
		if value == "0" && p.after != 0 {
			t.Fatal("zero ignored")
		}
		if value == "45m" && p.after != 45*time.Minute {
			t.Fatal("file ignored")
		}
	}
	if err := os.WriteFile(config, []byte("TI_ESCALATE_AFTER=45m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	override := func(key string) string {
		if key == "TI_ESCALATE_AFTER" {
			return "0"
		}
		return get(key)
	}
	p, err = loadPagingPolicy(override)
	if err != nil || p.after != 0 {
		t.Fatal("environment override", err)
	}
	if err := os.WriteFile(schedule, []byte(`{"invalid":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, err := loadPagingPolicy(get); err == nil || p.after != 0 {
		t.Fatal("invalid schedule must fail visibly and preserve immediate routing")
	}
}

func TestPagingFailedUnitEscalatesAndResolves(t *testing.T) {
	for _, unit := range []string{"ti-engine@sample.service", "ti-data.service", "ti-flex@sample.service", "ti-memory.service"} {
		t.Run(unit, func(t *testing.T) {
			systemd := newStandInSystemd(t)
			systemd.fail(unit)
			path := t.TempDir() + "/units.json"
			p, n, g := &policyRecorder{channel: "pushover"}, &policyRecorder{channel: "ntfy"}, &policyRecorder{channel: issuesChannel}
			senders := []sender{n, p, g}
			policy := &pagingPolicy{after: 30 * time.Minute}
			start := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
			for _, minutes := range []int{0, 5, 29, 30, 31, 60} {
				at := start.Add(time.Duration(minutes) * time.Minute)
				err := withUnitState(path, at, func(u *unitState) error {
					return failedRun(u, unit, firstRun, systemd.journalctl(), nil, senders, nil, at, at, time.Hour, policy)
				})
				if err != nil {
					t.Fatal(err)
				}
				if minutes < 30 && len(p.alerts) > 0 {
					t.Fatal("paged too soon")
				}
				if minutes == 30 && len(p.alerts) != 1 {
					t.Fatalf("no escalation: %d", len(p.alerts))
				}
			}
			if len(g.alerts) > 3 {
				t.Fatalf("too many issue reminders: %d", len(g.alerts))
			}
			if err := withUnitState(path, start.Add(61*time.Minute), func(u *unitState) error {
				return resolveKey(u, report.UnitFailedKey(unit, firstRun), senders, nil, start.Add(61*time.Minute))
			}); err != nil {
				t.Fatal(err)
			}
			_, sent, _, _, err := loadState(path)
			if err != nil {
				t.Fatal(err)
			}
			if !sent[channelKey("escalation", report.UnitFailedKey(unit, firstRun))].IsZero() {
				t.Fatal("resolution kept escalation clock")
			}
		})
	}
}

func TestPagingRecoveryBeforeDelayNeverReachesPushover(t *testing.T) {
	systemd := newStandInSystemd(t)
	unit := "ti-engine@sample.service"
	path := t.TempDir() + "/units.json"
	p, g := &policyRecorder{channel: "pushover"}, &policyRecorder{channel: issuesChannel}
	senders := []sender{p, g}
	start := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
	policy := &pagingPolicy{after: 30 * time.Minute}
	if err := withUnitState(path, start, func(u *unitState) error {
		return failedRun(u, unit, firstRun, systemd.journalctl(), nil, senders, nil, start, start, time.Hour, policy)
	}); err != nil {
		t.Fatal(err)
	}
	healthy := map[string]unitSample{unit: {state: "active"}}
	soft, hard := unitStep(path, []string{unit}, nil, healthy, start.Add(time.Minute), systemd.journalctl(), nil, senders, nil, start.Add(time.Minute), time.Hour, func() time.Time { return time.Time{} }, nil, nil, policy)
	if soft != nil || hard != nil {
		t.Fatal(soft, hard)
	}
	if len(p.alerts) != 0 {
		t.Fatal("recovery paged")
	}
	if err := withUnitState(path, start.Add(time.Hour), func(u *unitState) error {
		return failedRun(u, unit, firstRun, systemd.journalctl(), nil, senders, nil, start.Add(time.Hour), start.Add(time.Hour), time.Hour, policy)
	}); err != nil {
		t.Fatal(err)
	}
	if len(p.alerts) != 0 {
		t.Fatal("stale hook paged resolved incident")
	}
}

func TestPagingCapitalDeliverySurvivesStateWriteFailure(t *testing.T) {
	p := &policyRecorder{channel: "pushover"}
	err := deliver([]sender{p}, report.Alert{Key: "capital", Kind: report.KindHalted, Severity: report.Urgent}, map[string]bool{}, map[string]time.Time{}, map[string]int{}, tuesday(12, 0), time.Hour, func() error { return fmt.Errorf("synthetic state write failure") }, &pagingPolicy{after: 30 * time.Minute})
	if err == nil || len(p.alerts) != 1 {
		t.Fatalf("capital delivery=%d err=%v", len(p.alerts), err)
	}
}

func TestPagingRotatedConnectivityEvidenceKeepsNotice(t *testing.T) {
	g := newStandInGateway(t)
	start := time.Date(2026, 10, 31, 14, 0, 0, 0, newYork)
	g.ibc(stamp(start.Add(-time.Hour), ibcCompleted))
	g.adapter(stamp(start.Add(-time.Minute), linkLost))
	p := &policyRecorder{channel: "pushover"}
	n := &policyRecorder{channel: "ntfy"}
	path := t.TempDir() + "/gateway.json"
	sched, err := gatewayScheduleFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	step := func(at time.Time) {
		t.Helper()
		if err := gatewayStep(path, gatewayJournalChecker(g.path(), []string{"ti-adapter-boost.service"}), func() (gatewayUnitSample, error) { return gatewayUnitSample{active: true}, nil }, "synthetic", sched, func() time.Time { return time.Time{} }, "", nil, []sender{p, n}, nil, func() time.Time { return at }, &pagingPolicy{after: 30 * time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	step(start)
	g.adapter()
	step(start.Add(30 * time.Minute))
	if len(p.alerts) != 1 || !strings.Contains(p.alerts[0].Title, "connectivity lost") {
		t.Fatalf("escalation changed to password page: %+v", p.alerts)
	}
}

func TestPagingStateAtStartCannotBypassOperationalDelay(t *testing.T) {
	withBaseline(t)
	systemd := newStandInSystemd(t)
	unit := "ti-adapter@sample.service"
	systemd.fail(unit)
	dir := t.TempDir()
	path := writeRun(t, dir, haltedEntries(t)[:3])
	at := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	r := &relay{sent: map[string][]string{}}
	get := func(key string) string {
		if key == "TI_ESCALATE_AFTER" {
			return "30m"
		}
		return twoChannels()(key)
	}
	args := []string{"-journal", path, "-state", dir + "/state.json", "-channel", "ntfy,pagerduty", "-units", unit, "-systemctl", systemd.systemctl(), "-journalctl", systemd.journalctl()}
	if err := run(args, get, func() time.Time { return at }, io.Discard, &http.Client{Transport: r}); err != nil {
		t.Fatal(err)
	}
	if got := r.to(pdHost); len(got) != 0 {
		t.Fatalf("operational delay bypassed by standing: %v", got)
	}
	if got := r.to(ntfyHost); len(got) != 1 {
		t.Fatalf("want one unit notice, got %v", got)
	}
}

func TestPagingUndeliveredWindowIncidentCanResolve(t *testing.T) {
	unit := "ti-adapter@sample.service"
	systemd := newStandInSystemd(t)
	path := t.TempDir() + "/units.json"
	p := &policyRecorder{channel: "pushover"}
	start := time.Date(2026, 10, 31, 9, 0, 0, 0, newYork)
	windows, err := report.ParseIBKRSchedule([]byte(`{"windows":[{"days":["Sat"],"start":"00:00","end":"12:00","zone":"America/New_York"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	policy := &pagingPolicy{after: 30 * time.Minute, schedule: windows}
	if err := withUnitState(path, start, func(u *unitState) error {
		return failedRun(u, unit, firstRun, systemd.journalctl(), nil, []sender{p}, nil, start, start, time.Hour, policy)
	}); err != nil {
		t.Fatal(err)
	}
	healthy := map[string]unitSample{unit: {state: "active"}}
	soft, hard := unitStep(path, []string{unit}, nil, healthy, start.Add(time.Minute), systemd.journalctl(), nil, []sender{p}, nil, start.Add(time.Minute), time.Hour, func() time.Time { return time.Time{} }, nil, nil, policy)
	if soft != nil || hard != nil {
		t.Fatal(soft, hard)
	}
	if err := withUnitState(path, start.Add(4*time.Hour), func(u *unitState) error {
		return failedRun(u, unit, firstRun, systemd.journalctl(), nil, []sender{p}, nil, start.Add(4*time.Hour), start.Add(4*time.Hour), time.Hour, policy)
	}); err != nil {
		t.Fatal(err)
	}
	if len(p.alerts) != 0 {
		t.Fatal("stale hook escalated incident resolved inside window")
	}
}

func TestPagingForgottenIncidentGetsFreshDelay(t *testing.T) {
	p := &policyRecorder{channel: "pushover"}
	n := &policyRecorder{channel: "ntfy"}
	a := report.Alert{Key: "history:synthetic", Kind: report.KindHistoryUnavailable, Severity: report.Routine}
	seen, sent, repeats := map[string]bool{}, map[string]time.Time{}, map[string]int{}
	start := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{start, start.Add(time.Hour)} {
		if err := deliver([]sender{p, n}, a, seen, sent, repeats, at, time.Hour, func() error { return nil }, &pagingPolicy{after: 30 * time.Minute}); err != nil {
			t.Fatal(err)
		}
		forget("history:", seen, sent, repeats)
	}
	if len(p.alerts) != 0 {
		t.Fatal("new incident inherited old delay")
	}
}

func TestPagingMissedFiringsStillCountFromWindowEnd(t *testing.T) {
	schedule, err := report.ParseIBKRSchedule([]byte(`{"windows":[{"days":["Sat"],"start":"00:00","end":"12:00","zone":"America/New_York"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("reset=%t", reset), func(t *testing.T) {
			p := &policyRecorder{channel: "pushover"}
			g := &policyRecorder{channel: issuesChannel}
			seen, sent, repeats := map[string]bool{}, map[string]time.Time{}, map[string]int{}
			policy := &pagingPolicy{after: 30 * time.Minute, schedule: schedule}
			a := report.IBKRNotice("synthetic", time.Time{}, reset, "synthetic maintenance")
			start := time.Date(2026, 10, 30, 23, 50, 0, 0, newYork)
			if reset {
				start = time.Date(2026, 10, 31, 0, 40, 0, 0, newYork)
			}
			send := func(at time.Time) {
				t.Helper()
				if err := deliver([]sender{p, g}, a, seen, sent, repeats, at, time.Hour, func() error { return nil }, policy); err != nil {
					t.Fatal(err)
				}
			}
			send(start)
			// No reporter invocation during the rest of the scheduled window.
			a = report.IBKRNotice("synthetic", time.Time{}, false, "still unavailable")
			send(time.Date(2026, 10, 31, 12, 29, 0, 0, newYork))
			if len(p.alerts) != 0 {
				t.Fatal("missed firings shortened window-end delay")
			}
			send(time.Date(2026, 10, 31, 12, 30, 0, 0, newYork))
			if len(p.alerts) != 1 {
				t.Fatal("did not escalate at exact window end plus delay")
			}
		})
	}
}

func TestPagingSupersededResetWaitStopsQuieting(t *testing.T) {
	g := newStandInGateway(t)
	start := time.Date(2026, 10, 31, 0, 40, 0, 0, newYork)
	g.ibc(stamp(start.Add(-time.Minute), "gateway-start: reset wait: waiting"))
	path := t.TempDir() + "/gateway.json"
	p := &policyRecorder{channel: "pushover"}
	n := &policyRecorder{channel: "ntfy"}
	sched, err := gatewayScheduleFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	step := func(at time.Time) {
		t.Helper()
		if err := gatewayStep(path, gatewayJournalChecker(g.path(), nil), func() (gatewayUnitSample, error) { return gatewayUnitSample{active: true}, nil }, "synthetic", sched, func() time.Time { return time.Time{} }, "", nil, []sender{p, n}, nil, func() time.Time { return at }, &pagingPolicy{}); err != nil {
			t.Fatal(err)
		}
	}
	step(start)
	g.ibc(stamp(start.Add(time.Minute), "gateway-start: cannot start IBC: synthetic failure"))
	step(start.Add(10 * time.Minute))
	if len(p.alerts) != 1 || !strings.Contains(p.alerts[0].Title, "logged out") {
		t.Fatal("a superseded reset wait still quieted the genuine logout")
	}
}
