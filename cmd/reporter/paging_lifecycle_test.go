package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

type incidentReceiptSender struct {
	policyRecorder
	fail      bool
	cancelled []string
}

func (s *incidentReceiptSender) takeReceipt() string {
	return fmt.Sprintf("synthetic-%d", len(s.alerts))
}
func (s *incidentReceiptSender) expiry() time.Duration { return time.Hour }
func (s *incidentReceiptSender) cancel(receipt string) error {
	s.cancelled = append(s.cancelled, receipt)
	if s.fail {
		return errors.New("synthetic cancellation failure")
	}
	return nil
}

func TestPagingCancellationRetryTargetsOriginalReceipt(t *testing.T) {
	for _, aside := range []bool{false, true} {
		t.Run(fmt.Sprint(aside), func(t *testing.T) {
			g, path, start := pagingGatewayFixture(t)
			p := &incidentReceiptSender{policyRecorder: policyRecorder{channel: "pushover"}}
			// ADR 0019: a recovery cancels only emergencies belonging to that incident.
			g.ibc(stamp(start, ibcRefused))
			if err := pagingGatewayStep(t, g, path, start, p); err != nil {
				t.Fatal(err)
			}
			if err := pagingGatewayStep(t, g, path, start.Add(30*time.Minute), p); err != nil {
				t.Fatal(err)
			}
			p.fail = true
			g.ibc(stamp(start.Add(31*time.Minute), ibcCompleted))
			if aside {
				if err := pagingGatewayStep(t, g, path, start.Add(31*time.Minute)); err != nil {
					t.Fatal(err)
				}
			} else if err := pagingGatewayStep(t, g, path, start.Add(31*time.Minute), p); err == nil {
				t.Fatal("expected cancellation failure")
			}
			g.ibc(stamp(start.Add(32*time.Minute), ibcRefused))
			_ = pagingGatewayStep(t, g, path, start.Add(32*time.Minute), p)
			_ = pagingGatewayStep(t, g, path, start.Add(62*time.Minute), p)
			if len(p.alerts) != 2 {
				t.Fatalf("new incident pages=%d want 2", len(p.alerts))
			}
			p.fail = false
			p.cancelled = nil
			if err := pagingGatewayStep(t, g, path, start.Add(63*time.Minute), p); err != nil {
				t.Fatal(err)
			}
			if len(p.cancelled) != 1 || p.cancelled[0] != "synthetic-1" {
				t.Fatalf("retry cancelled %v; want only original receipt synthetic-1", p.cancelled)
			}
			h, err := loadHostGateway(path)
			if err != nil || h.Since == nil {
				t.Fatalf("new outage lost: %+v %v", h, err)
			}
		})
	}
}

func TestPagingSessionPolicyFirstPageAndReminder(t *testing.T) {
	for _, value := range []string{"", "session"} {
		for _, tc := range []struct{ name, start, first string }{
			{"overnight", "2026-11-02T18:00:00-05:00", "2026-11-03T05:30:00-05:00"},
			{"weekend", "2026-10-31T18:00:00-04:00", "2026-11-02T05:30:00-05:00"},
			{"holiday", "2026-12-24T18:00:00-05:00", "2026-12-28T05:30:00-05:00"},
			{"near_session", "2026-11-02T06:00:00-05:00", "2026-11-02T06:10:00-05:00"},
		} {
			t.Run(value+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				settings := map[string]string{"TI_ESCALATE_AFTER": value, "TI_REPORTER_CONFIG": dir + "/missing.env", "TI_IBKR_MAINTENANCE_FILE": dir + "/missing.json"}
				policy, err := loadPagingPolicy(func(key string) string { return settings[key] })
				if err != nil {
					t.Fatal(err)
				}
				start, _ := time.Parse(time.RFC3339, tc.start)
				first, _ := time.Parse(time.RFC3339, tc.first)
				sched, err := gatewayScheduleFrom(func(string) string { return "" })
				if err != nil {
					t.Fatal(err)
				}
				p, n, issues := &policyRecorder{channel: "pushover"}, &policyRecorder{channel: "ntfy"}, &policyRecorder{channel: issuesChannel}
				step := func(at time.Time) {
					t.Helper()
					err := gatewayStep(dir+"/gateway.json", func(time.Time) (gatewayEvidence, error) { return gatewayEvidence{out: true}, nil }, func() (gatewayUnitSample, error) { return gatewayUnitSample{active: true}, nil }, "synthetic", sched, nil, "", nil, []sender{p, n, issues}, nil, func() time.Time { return at }, policy)
					if err != nil {
						t.Fatal(err)
					}
				}
				for at := start; at.Before(first); at = at.Add(5 * time.Minute) {
					step(at)
					if len(p.alerts) != 0 {
						t.Fatalf("paged before session eligibility at %s; first allowed %s", at, first)
					}
				}
				step(first.Add(-time.Second))
				if len(p.alerts) != 0 {
					t.Fatal("page one second early")
				}
				step(first)
				if len(p.alerts) != 1 || len(n.alerts) == 0 || len(issues.alerts) == 0 {
					t.Fatalf("first eligible firing: pages=%d ntfy=%d issues=%d", len(p.alerts), len(n.alerts), len(issues.alerts))
				}
				step(first.Add(5 * time.Minute))
				step(first.Add(time.Hour - time.Second))
				if len(p.alerts) != 1 {
					t.Fatal("early reminder")
				}
				step(first.Add(time.Hour))
				if len(p.alerts) != 2 {
					t.Fatal("missing hourly reminder")
				}
			})
		}
	}
}

func TestPagingLegacyCancellationCannotCancelANewerIncident(t *testing.T) {
	start := time.Date(2026, 10, 31, 18, 0, 0, 0, newYork)
	key := report.GatewayKey("synthetic")
	seen := map[string]bool{
		channelKey("cancel:pushover", key):                                                      true,
		channelKey(receiptChannel("pushover", "synthetic-old", start), key):                     true,
		channelKey(receiptChannel("pushover", "synthetic-new", start.Add(20*time.Minute)), key): true,
	}
	p := &incidentReceiptSender{policyRecorder: policyRecorder{channel: "pushover"}}
	_, _, _, err := (&pagingPolicy{}).routeAndCancel([]sender{p}, report.GatewayLoggedOutAlert("synthetic", start, start, "synthetic"), seen, map[string]time.Time{}, start.Add(25*time.Minute), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range p.cancelled {
		if receipt == "synthetic-new" {
			t.Fatal("legacy channel marker guessed ownership and cancelled the new incident")
		}
	}
}
