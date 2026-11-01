package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// alerts-and-dashboard.md's Paging invariant preserves a capital-safety
// emergency until recovery, even when the clock changes the alert's class.
func TestPagingEmergencySurvivesSessionEnd(t *testing.T) {
	for _, unit := range []string{"ti-engine@sample.service", "ti-data.service"} {
		for _, window := range []bool{false, true} {
			name := unit + "/no_window"
			if window {
				name = unit + "/quiet_window"
			}
			t.Run(name, func(t *testing.T) {
				r := &pagingReceiptTransport{}
				ps := &pushoverSender{url: "https://synthetic.test/1/messages.json", client: &http.Client{Transport: r}}
				start := time.Date(2026, 10, 29, 15, 55, 0, 0, newYork)
				policy := &pagingPolicy{session: true}
				if window {
					var err error
					policy.schedule, err = report.ParseIBKRSchedule([]byte(`{"windows":[{"days":["daily"],"start":"16:00","end":"17:00","zone":"America/New_York"}]}`))
					if err != nil {
						t.Fatal(err)
					}
				}
				path := t.TempDir() + "/units.json"
				systemd := newStandInSystemd(t)
				for _, minutes := range []int{0, 5, 10} {
					at := start.Add(time.Duration(minutes) * time.Minute)
					if err := withUnitState(path, at, func(u *unitState) error {
						return failedRun(u, unit, firstRun, systemd.journalctl(), nil, []sender{ps}, nil, at, at, time.Hour, policy)
					}); err != nil {
						t.Fatal(err)
					}
				}
				want := 1
				if window && unit == "ti-data.service" {
					want = 2
				}
				if len(r.paths) != want {
					t.Fatalf("requests=%v; want %d (cancel only an operational receipt in a quiet window)", r.paths, want)
				}
				seen, _, _, _, err := loadState(path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := cancelReceipts([]sender{ps}, report.UnitFailedKey(unit, firstRun), seen, start.Add(11*time.Minute)); err != nil {
					t.Fatal(err)
				}
				if len(r.paths) != 2 || !strings.HasSuffix(r.paths[1], "/cancel.json") {
					t.Fatalf("recovery requests=%v; want exactly one cancellation", r.paths)
				}
			})
		}
	}
}

func TestPagingGatewayNonLoginLinePreservesCompletedLogin(t *testing.T) {
	for _, message := range []string{
		"waiting for IBC: synthetic error",
		"the output reader did not finish: synthetic error",
		"the journal is not taking IBC's output: synthetic error",
		"IBC did not end after SIGKILL",
		"synthetic configuration warning",
	} {
		t.Run(message, func(t *testing.T) {
			g := newStandInGateway(t)
			start := time.Date(2026, 10, 29, 15, 55, 0, 0, newYork)
			g.ibc(stamp(start, ibcCompleted), stamp(start.Add(time.Minute), "gateway-start: "+message))
			ev, err := gatewayJournalChecker(g.path(), nil)(start.Add(2 * time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if ev.out || ev.silent || ev.resetWait || !ev.recoveredAt.Equal(start) {
				t.Fatalf("non-login line changed completed login: %+v", ev)
			}
		})
	}
}

func TestPagingPersistedCancellationRequiresRecoveryUnlessOperational(t *testing.T) {
	for _, class := range []string{"", ":capital-safety", ":operational"} {
		t.Run("receipt_class"+class, func(t *testing.T) {
			start := time.Date(2026, 10, 29, 15, 55, 0, 0, newYork)
			a := report.UnitFailedAlert("synthetic", "ti-engine@sample.service", firstRun, "failed", start)
			key := channelKey(receiptChannel("pushover", "synthetic", start)+class, a.Key)
			path := t.TempDir() + "/receipts.json"
			if err := saveState(path, map[string]bool{key: true, "cancel:" + key: true}, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			seen, sent, _, _, err := loadState(path)
			if err != nil {
				t.Fatal(err)
			}
			p := &incidentReceiptSender{policyRecorder: policyRecorder{channel: "pushover"}}
			_, _, _, err = (&pagingPolicy{session: true}).routeAndCancel([]sender{p}, a, seen, sent, start.Add(10*time.Minute), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if class == ":operational" {
				want = 1
			}
			if len(p.cancelled) != want {
				t.Fatalf("open alert: cancel calls=%d, want %d", len(p.cancelled), want)
			}
			if want == 0 && (!seen[key] || !seen["cancel:"+key]) {
				t.Fatal("receipt and pending cancellation must survive until recovery")
			}
			if _, err := cancelReceipts([]sender{p}, a.Key, seen, start.Add(11*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if len(p.cancelled) != 1 || seen[key] || seen["cancel:"+key] {
				t.Fatal("recovery must cancel exactly once and clear the receipt and marker")
			}
		})
	}
}

func TestPagingOlderResetWaitCannotOverrideCompletedLogin(t *testing.T) {
	start := time.Date(2026, 10, 31, 0, 30, 0, 0, newYork)
	for _, tc := range []struct {
		name   string
		logins []time.Time
	}{
		{"later_login", []time.Time{start.Add(time.Minute)}},
		{"equal_timestamp", []time.Time{start}},
		{"clock_moves_backward", []time.Time{start.Add(time.Minute), start.Add(-time.Minute)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newStandInGateway(t)
			lines := []string{stamp(start, "gateway-start: reset wait: login refused at 00:30 ET; waiting")}
			for _, at := range tc.logins {
				lines = append(lines, stamp(at, ibcCompleted))
			}
			g.ibc(lines...)
			ev, err := gatewayJournalChecker(g.path(), nil)(start.Add(2 * time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if ev.out || ev.silent || ev.resetWait || !ev.recoveredAt.Equal(tc.logins[len(tc.logins)-1]) {
				t.Fatalf("older reset overrode completed login: %+v", ev)
			}
		})
	}
}
