package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

func TestPagingReminderNeverShortens(t *testing.T) {
	for _, at := range []time.Time{time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC), time.Date(2026, 10, 30, 14, 0, 0, 0, time.UTC)} {
		for _, delay := range []time.Duration{0, 30 * time.Minute, 2 * time.Hour} {
			p := &pagingPolicy{after: delay}
			_, _, every, _, err := p.route(nil, report.Alert{Key: "synthetic", Kind: report.KindGatewayLoggedOut}, map[string]time.Time{}, at, time.Hour)
			want := time.Hour
			if delay > want {
				want = delay
			}
			if err != nil || every != want {
				t.Errorf("at=%s delay=%s reminder=%s want=%s err=%v", at, delay, every, want, err)
			}
		}
	}
}

func TestPagingInvalidConfigBypassesResetWait(t *testing.T) {
	for _, fault := range []string{"delay", "schedule", "env", "unreadable"} {
		t.Run(fault, func(t *testing.T) {
			dir := t.TempDir()
			config := dir + "/reporter.env"
			schedule := dir + "/schedule.json"
			settings := map[string]string{"TI_REPORTER_CONFIG": config, "TI_IBKR_MAINTENANCE_FILE": schedule}
			switch fault {
			case "delay":
				settings["TI_ESCALATE_AFTER"] = "broken"
			case "schedule":
				if err := os.WriteFile(schedule, []byte(`{"Windows":[]}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "env":
				if err := os.WriteFile(config, []byte("malformed\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				if err := os.Mkdir(config, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			p, err := loadPagingPolicy(env(settings))
			if err == nil {
				t.Fatal("invalid config accepted")
			}
			if (fault == "env" || fault == "unreadable") && !strings.Contains(err.Error(), config) {
				t.Errorf("wrong file diagnostic: %v", err)
			}
			n := &policyRecorder{channel: "ntfy"}
			h := &policyRecorder{channel: "pushover"}
			at := time.Date(2026, 10, 31, 0, 40, 0, 0, newYork)
			if err := deliver([]sender{n, h}, report.GatewayResetWaitAlert("synthetic", at, "refused"), map[string]bool{}, map[string]time.Time{}, map[string]int{}, at, time.Hour, func() error { return nil }, p); err != nil {
				t.Fatal(err)
			}
			if len(h.alerts) != 1 {
				t.Fatal("invalid config still suppresses reset-wait page")
			}
		})
	}
}

func TestPagingInitialJournalOnlyWhenEligible(t *testing.T) {
	systemd := newStandInSystemd(t)
	path := t.TempDir() + "/units.json"
	n := &policyRecorder{channel: "ntfy"}
	p := &policyRecorder{channel: "pushover"}
	start := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
	policy := &pagingPolicy{after: 30 * time.Minute}
	for _, tc := range []struct{ minutes, reads int }{{0, 1}, {5, 1}, {30, 2}, {35, 2}, {60, 2}, {90, 2}} {
		at := start.Add(time.Duration(tc.minutes) * time.Minute)
		if err := withUnitState(path, at, func(u *unitState) error {
			return failedRun(u, "ti-engine@sample.service", firstRun, systemd.journalctl(), nil, []sender{n, p}, nil, at, at, time.Hour, policy)
		}); err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(systemd.calls(), "_SYSTEMD_INVOCATION_ID="); got != tc.reads {
			t.Errorf("minute %d journal reads=%d want=%d", tc.minutes, got, tc.reads)
		}
	}
}

func TestPagingHookUsesClockAfterLock(t *testing.T) {
	for _, window := range []bool{false, true} {
		t.Run(fmt.Sprint(window), func(t *testing.T) {
			systemd := newStandInSystemd(t)
			dir := t.TempDir()
			path := dir + "/units.json"
			before := time.Date(2026, 10, 30, 5, 29, 0, 0, newYork)
			after := before.Add(2 * time.Minute)
			settings := map[string]string{"TI_ESCALATE_AFTER": "30m", "TI_REPORTER_CONFIG": dir + "/reporter.env", "TI_IBKR_MAINTENANCE_FILE": dir + "/schedule.json"}
			if window {
				before = time.Date(2026, 10, 31, 23, 39, 0, 0, newYork)
				after = before.Add(2 * time.Minute)
				settings["TI_ESCALATE_AFTER"] = "0"
				if err := os.WriteFile(settings["TI_IBKR_MAINTENANCE_FILE"], []byte(`{"windows":[{"days":["daily"],"start":"23:40","end":"01:00","zone":"America/New_York"}]}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lock.Close() }()
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			observed := make(chan struct{})
			resume := make(chan struct{})
			done := make(chan error, 1)
			calls := 0
			clock := func() time.Time {
				calls++
				if calls == 1 {
					return before
				}
				if calls == 2 {
					close(observed)
					<-resume
					return before
				}
				return after
			}
			r := &relay{sent: map[string][]string{}}
			get := func(k string) string {
				if v := settings[k]; v != "" {
					return v
				}
				return monitored("ti-engine@sample.service", firstRun)(k)
			}
			go func() {
				done <- notifyUnitFailed([]string{"-state", path, "-channel", "ntfy,pagerduty", "-journalctl", systemd.journalctl(), "-systemctl", systemd.systemctl(), "ti-engine@sample.service"}, get, clock, io.Discard, &http.Client{Transport: r})
			}()
			<-observed
			close(resume)
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			want := 1
			if window {
				want = 0
			}
			if got := len(r.to(pdHost)); got != want {
				t.Errorf("pages=%d want=%d after lock", got, want)
			}
			seen, sent, _, _, err := loadState(path)
			if err != nil {
				t.Fatal(err)
			}
			_ = seen
			if got := sent[channelKey("observed", report.UnitFailedKey("ti-engine@sample.service", firstRun))]; !got.Equal(before) {
				t.Errorf("observation=%s want=%s", got, before)
			}
		})
	}
}

func TestPagingResetWaitBelongsToOneNight(t *testing.T) {
	written := time.Date(2026, 10, 30, 23, 50, 0, 0, newYork)
	for _, tc := range []struct {
		name, message string
		at            time.Time
		want          bool
	}{
		{"before_midnight", "login refused at 23:50 ET", written, true},
		{"after_midnight", "login refused at 23:50 ET", written.Add(30 * time.Minute), true},
		{"deadline", "login refused at 23:50 ET", written.Add(70 * time.Minute), false},
		{"next_night", "login refused at 23:50 ET", written.Add(24*time.Hour + 30*time.Minute), false},
		{"stopped", "interrupted by stop", written.Add(30 * time.Minute), false},
		{"future", "login refused at 23:50 ET", written.Add(-time.Minute), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newStandInGateway(t)
			g.ibc(stamp(written, "gateway-start: reset wait: "+tc.message))
			ev, err := gatewayJournalChecker(g.path(), nil)(tc.at)
			if err != nil {
				t.Fatal(err)
			}
			if ev.resetWait != tc.want {
				t.Errorf("resetWait=%v want=%v", ev.resetWait, tc.want)
			}
		})
	}
}

func TestPagingGatewayGraceAndObservation(t *testing.T) {
	for _, near := range []bool{false, true} {
		t.Run(fmt.Sprint(near), func(t *testing.T) {
			path := t.TempDir() + "/gateway.json"
			p := &policyRecorder{channel: "pushover"}
			n := &policyRecorder{channel: "ntfy"}
			start := time.Date(2026, 10, 31, 18, 0, 0, 0, newYork)
			if near {
				start = time.Date(2026, 10, 30, 10, 0, 0, 0, newYork)
			}
			sched, err := gatewayScheduleFrom(func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			for _, minutes := range []int{0, 5, 10, 29, 30} {
				at := start.Add(time.Duration(minutes) * time.Minute)
				err := gatewayStep(path, func(time.Time) (gatewayEvidence, error) {
					return gatewayEvidence{out: true, said: "synthetic"}, nil
				}, func() (gatewayUnitSample, error) { return gatewayUnitSample{active: true}, nil }, "synthetic", sched, nil, "", nil, []sender{n, p}, nil, func() time.Time { return at }, &pagingPolicy{after: 30 * time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				want := 0
				if near && minutes >= 10 || !near && minutes >= 30 {
					want = 1
				}
				if len(p.alerts) != want {
					t.Errorf("minute %d pages=%d want=%d", minutes, len(p.alerts), want)
				}
			}
		})
	}
}

type pagingReceiptTransport struct {
	paths []string
	fail  bool
}

func (r *pagingReceiptTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.paths = append(r.paths, req.URL.Path)
	status := http.StatusOK
	if r.fail && strings.HasSuffix(req.URL.Path, "cancel.json") {
		status = http.StatusServiceUnavailable
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"status":1,"receipt":"synthetic"}`)), Request: req}, nil
}

func TestPagingQuietWindowCancelsEmergency(t *testing.T) {
	for _, mode := range []string{"deliver", "unit", "gateway", "capital"} {
		t.Run(mode, func(t *testing.T) {
			r := &pagingReceiptTransport{}
			ps := &pushoverSender{url: "https://synthetic.test/1/messages.json", client: &http.Client{Transport: r}}
			start := time.Date(2026, 10, 31, 23, 39, 0, 0, newYork)
			schedule, err := report.ParseIBKRSchedule([]byte(`{"windows":[{"days":["daily"],"start":"23:40","end":"01:00","zone":"America/New_York"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			policy := &pagingPolicy{schedule: schedule}
			seen, sent, repeats := map[string]bool{}, map[string]time.Time{}, map[string]int{}
			a := report.GatewayLoggedOutAlert("synthetic", start, start, "synthetic")
			if mode == "capital" {
				a = report.Alert{Key: "synthetic", Kind: report.KindHalted, Severity: report.Urgent, Repeat: true}
			}
			path := t.TempDir() + "/state.json"
			systemd := newStandInSystemd(t)
			if mode == "unit" {
				a = report.UnitFailedAlert("synthetic", "ti-data.service", firstRun, "synthetic", start)
			}
			if err := deliver([]sender{ps}, a, seen, sent, repeats, start, time.Hour, func() error { return nil }, policy); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "gateway":
				since := start.Add(-time.Hour)
				raw := fmt.Sprintf(`{"logged_out_since":%q}`, since.Format(time.RFC3339))
				if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
				h, err := loadHostGateway(path)
				if err != nil {
					t.Fatal(err)
				}
				h.delivered = deliveredOf(seen, sent, repeats, nil)
				rawBytes, err := json.Marshal(h)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, rawBytes, 0o600); err != nil {
					t.Fatal(err)
				}
			case "unit":
				if err := saveState(path, seen, sent, repeats, nil); err != nil {
					t.Fatal(err)
				}
			}
			step := func(at time.Time) error {
				switch mode {
				case "unit":
					return withUnitState(path, at, func(u *unitState) error {
						return failedRun(u, "ti-data.service", firstRun, systemd.journalctl(), nil, []sender{ps}, nil, at, at, time.Hour, policy)
					})
				case "gateway":
					sched, e := gatewayScheduleFrom(func(string) string { return "" })
					if e != nil {
						return e
					}
					return gatewayStep(path, func(time.Time) (gatewayEvidence, error) { return gatewayEvidence{out: true}, nil }, func() (gatewayUnitSample, error) { return gatewayUnitSample{active: true}, nil }, "synthetic", sched, nil, "", nil, []sender{ps}, nil, func() time.Time { return at }, policy)
				default:
					return deliver([]sender{ps}, a, seen, sent, repeats, at, time.Hour, func() error { return nil }, policy)
				}
			}
			r.fail = true
			err = step(start.Add(time.Minute))
			if mode == "capital" {
				if err != nil || len(r.paths) != 1 {
					t.Fatal("capital emergency was cancelled", r.paths, err)
				}
				return
			}
			if err == nil {
				t.Error("cancel failure was not surfaced")
			}
			r.fail = false
			if err := step(start.Add(2 * time.Minute)); err != nil {
				t.Fatal(err)
			}
			if len(r.paths) != 3 || r.paths[1] != "/1/receipts/synthetic/cancel.json" || r.paths[2] != r.paths[1] {
				t.Fatalf("requests=%v want initial, failed cancel, retry", r.paths)
			}
			if err := step(start.Add(3 * time.Minute)); err != nil {
				t.Fatal(err)
			}
			if len(r.paths) != 3 {
				t.Fatal("successful cancel was repeated")
			}
		})
	}
}

func TestPagingResetWaitBeforeMidnightIsQuiet(t *testing.T) {
	at := time.Date(2026, 10, 30, 23, 50, 0, 0, newYork)
	p := &policyRecorder{channel: "pushover"}
	n := &policyRecorder{channel: "ntfy"}
	sent := map[string]time.Time{}
	a := report.GatewayResetWaitAlert("synthetic", at, "synthetic")
	if err := deliver([]sender{n, p}, a, map[string]bool{}, sent, map[string]int{}, at, time.Hour, func() error { return nil }, &pagingPolicy{}); err != nil {
		t.Fatal(err)
	}
	if len(p.alerts) != 0 || len(n.alerts) != 1 {
		t.Fatal("pre-midnight reset wait did not route quietly")
	}
	want := time.Date(2026, 10, 31, 1, 0, 0, 0, newYork)
	if !sent[channelKey("escalation", a.Key)].Equal(want) {
		t.Fatal("wrong reset deadline")
	}
}

func TestPagingResetWaitDeadlineAndBadTime(t *testing.T) {
	for _, tc := range []struct {
		written, now string
		want         bool
	}{
		{"2026-10-31T23:40:00-04:00", "2026-11-01T00:59:59-04:00", true},
		{"2026-10-31T23:40:00-04:00", "2026-11-01T01:00:00-04:00", false},
		{"2026-10-31T23:40:00-04:00", "2026-11-01T01:00:00-05:00", false},
		{"2026-10-31T23:39:59-04:00", "2026-11-01T00:10:00-04:00", false},
	} {
		written, err := time.Parse(time.RFC3339, tc.written)
		if err != nil {
			t.Fatal(err)
		}
		at, err := time.Parse(time.RFC3339, tc.now)
		if err != nil {
			t.Fatal(err)
		}
		g := newStandInGateway(t)
		g.ibc(stamp(written, "gateway-start: reset wait: login refused at synthetic"))
		ev, err := gatewayJournalChecker(g.path(), nil)(at)
		if err != nil || ev.resetWait != tc.want {
			t.Errorf("%s at %s: reset=%v want=%v err=%v", tc.written, tc.now, ev.resetWait, tc.want, err)
		}
	}
	g := newStandInGateway(t)
	g.ibc("invalid gateway-start: reset wait: login refused at synthetic")
	if _, err := gatewayJournalChecker(g.path(), nil)(time.Date(2026, 10, 31, 0, 40, 0, 0, newYork)); err == nil {
		t.Fatal("invalid reset timestamp accepted")
	}
}
