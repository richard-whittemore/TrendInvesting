package main

import (
	"errors"
	"fmt"
	"os"
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

type incidentResolver struct {
	policyRecorder
	fail     bool
	resolved int
}

func (s *incidentResolver) resolve(string) error {
	s.resolved++
	if s.fail {
		return errors.New("synthetic resolution failure")
	}
	return nil
}

func TestPagingCancellationRetryTargetsOriginalReceipt(t *testing.T) {
	for _, aside := range []bool{false, true} {
		t.Run(fmt.Sprint(aside), func(t *testing.T) {
			g, path, start := adapterFixture(t)
			p := &incidentReceiptSender{policyRecorder: policyRecorder{channel: "pushover"}}
			// ADR 0019: a recovery cancels only emergencies belonging to that incident.
			g.ibc(stamp(start, ibcRefused))
			if err := adapterStep(t, g, path, start, p); err != nil {
				t.Fatal(err)
			}
			if err := adapterStep(t, g, path, start.Add(30*time.Minute), p); err != nil {
				t.Fatal(err)
			}
			p.fail = true
			g.ibc(stamp(start.Add(31*time.Minute), ibcCompleted))
			if aside {
				if err := adapterStep(t, g, path, start.Add(31*time.Minute)); err != nil {
					t.Fatal(err)
				}
			} else if err := adapterStep(t, g, path, start.Add(31*time.Minute), p); err == nil {
				t.Fatal("expected cancellation failure")
			}
			g.ibc(stamp(start.Add(32*time.Minute), ibcRefused))
			_ = adapterStep(t, g, path, start.Add(32*time.Minute), p)
			_ = adapterStep(t, g, path, start.Add(62*time.Minute), p)
			if len(p.alerts) != 2 {
				t.Fatalf("new incident pages=%d want 2", len(p.alerts))
			}
			p.fail = false
			p.cancelled = nil
			if err := adapterStep(t, g, path, start.Add(63*time.Minute), p); err != nil {
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

func TestPagingAdapterRestorationWithEmptyIBCJournal(t *testing.T) {
	g, path, start := adapterFixture(t)
	g.ibc()
	g.write(adapterA, stamp(start.Add(-time.Minute), linkLost))
	for _, at := range []time.Time{start, start.Add(30 * time.Minute)} {
		if err := adapterStep(t, g, path, at); err != nil {
			t.Fatal(err)
		}
	}
	g.write(adapterA, stamp(start.Add(31*time.Minute), linkUp))
	for _, minutes := range []int{31, 35} {
		if err := adapterStep(t, g, path, start.Add(time.Duration(minutes)*time.Minute)); err != nil {
			t.Fatal(err)
		}
		h, err := loadHostGateway(path)
		if err != nil || h.Since != nil || len(h.Links) != 0 {
			t.Fatalf("current restoration left incident open with empty IBC journal: %+v %v", h, err)
		}
	}
}

func TestPagingLegacyOwnershipIncludesRotatedAdapter(t *testing.T) {
	g, path, start := adapterFixture(t)
	raw := fmt.Sprintf(`{"logged_out_since":%q}`, start.Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	g.write(adapterA)
	g.write(adapterB, stamp(start.Add(-3*time.Hour), linkLost))
	adapterSample(t, g, adapterB, "inactive", start.Add(-4*time.Hour))
	for _, minutes := range []int{15, 20} {
		if err := adapterStep(t, g, path, start.Add(time.Duration(minutes)*time.Minute)); err != nil {
			t.Fatal(err)
		}
		h, err := loadHostGateway(path)
		if err != nil || h.Since == nil || h.Links[adapterA].Since.IsZero() {
			t.Fatalf("unrelated stopped adapter resolved legacy owner with rotated journal: %+v %v", h, err)
		}
	}
	g.write(adapterA, stamp(start.Add(21*time.Minute), linkUp))
	if err := adapterStep(t, g, path, start.Add(25*time.Minute)); err != nil {
		t.Fatal(err)
	}
	h, err := loadHostGateway(path)
	if err != nil || h.Since != nil {
		t.Fatalf("own restoration did not close migrated loss: %+v %v", h, err)
	}
}

func TestPagingRecurringSamplingFaultGetsFreshPage(t *testing.T) {
	g, path, start := adapterFixture(t)
	p := &incidentResolver{policyRecorder: policyRecorder{channel: "pagerduty"}, fail: true}
	adapterSample(t, g, adapterA, "unknown", start)
	for _, minutes := range []int{0, 30} {
		if err := adapterStep(t, g, path, start.Add(time.Duration(minutes)*time.Minute), p); err == nil {
			t.Fatal("missing sample failure")
		}
	}
	if len(p.alerts) != 1 {
		t.Fatal("first sampling fault did not page")
	}
	adapterSample(t, g, adapterA, "active", start.Add(-time.Hour))
	if err := adapterStep(t, g, path, start.Add(31*time.Minute), p); err == nil {
		t.Fatal("missing failed resolution")
	}
	adapterSample(t, g, adapterA, "unknown", start)
	for _, minutes := range []int{32, 61, 62} {
		if err := adapterStep(t, g, path, start.Add(time.Duration(minutes)*time.Minute), p); err == nil {
			t.Fatal("missing sample failure")
		}
		want := 1
		if minutes == 62 {
			want = 2
		}
		if len(p.alerts) != want {
			t.Fatalf("minute %d: recurring sampling fault pages=%d want %d; inherited old suppression", minutes, len(p.alerts), want)
		}
	}
}

func TestPagingSamplingLifecycleDuringIBCLogout(t *testing.T) {
	g, path, start := adapterFixture(t)
	p := &incidentReceiptSender{policyRecorder: policyRecorder{channel: "pushover"}}
	adapterSample(t, g, adapterA, "unknown", start)
	if err := adapterStep(t, g, path, start, p); err == nil {
		t.Fatal("missing sampling fault")
	}
	g.ibc(stamp(start.Add(time.Minute), ibcRefused))
	count := func() int {
		n := 0
		for _, a := range p.alerts {
			if a.Kind == report.KindAdapterSampleFault {
				n++
			}
		}
		return n
	}
	for _, minutes := range []int{30, 90} {
		if err := adapterStep(t, g, path, start.Add(time.Duration(minutes)*time.Minute), p); err == nil {
			t.Error("IBC logout stopped reporting adapter sampling failure")
		}
	}
	if count() != 2 {
		t.Errorf("sampling pages during logout=%d want escalation and reminder", count())
	}
	adapterSample(t, g, adapterA, "active", start.Add(-time.Hour))
	p.fail = true
	if err := adapterStep(t, g, path, start.Add(91*time.Minute), p); err == nil {
		t.Error("sampling recovery during logout did not try cancellation")
	}
	p.fail = false
	p.cancelled = nil
	if err := adapterStep(t, g, path, start.Add(92*time.Minute), p); err != nil {
		t.Fatal(err)
	}
	if len(p.cancelled) == 0 {
		t.Error("pending sampling cancellation did not retry during logout")
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

func TestPagingIBCLogoutStillSamplesWithoutAdapterCorroboration(t *testing.T) {
	g, _, start := adapterFixture(t)
	g.ibc(stamp(start, ibcRefused))
	g.write(adapterA, "invalid-time "+linkLost)
	adapterSample(t, g, adapterA, "unknown", start)
	evidence, err := gatewayJournalChecker(g.path(), []string{adapterA}, g.dir+"/systemctl")(start)
	if err != nil || !evidence.out || evidence.adapters[adapterA].sampleErr == nil {
		t.Fatalf("IBC logout must retain sampling lifecycle without requiring adapter journal corroboration: %+v %v", evidence, err)
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
	_, _, _, err := (&pagingPolicy{}).routeAndCancel([]sender{p}, report.IBKRNotice("synthetic", start, false, "synthetic"), seen, map[string]time.Time{}, start.Add(25*time.Minute), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range p.cancelled {
		if receipt == "synthetic-new" {
			t.Fatal("legacy channel marker guessed ownership and cancelled the new incident")
		}
	}
}
