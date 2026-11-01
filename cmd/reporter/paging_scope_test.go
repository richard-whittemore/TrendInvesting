package main

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

func TestPagingLogoutCannotResolveWhenAdapterStops(t *testing.T) {
	g := newStandInGateway(t)
	path := t.TempDir() + "/gateway.json"
	start := tuesday(12, 0)
	g.ibc(stamp(start.Add(-5*time.Hour), ibcCompleted))
	g.adapter(stamp(start.Add(-4*time.Hour), linkLost))
	sched, err := gatewayScheduleFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	p := &policyRecorder{channel: "pushover"}
	step := func(at time.Time) {
		t.Helper()
		if err := gatewayStep(path, gatewayJournalChecker(g.path(), []string{"ti-adapter-boost.service"}), func() (gatewayUnitSample, error) { return gatewayUnitSample{active: true}, nil }, "synthetic", sched, nil, "", nil, []sender{p}, nil, func() time.Time { return at }, &pagingPolicy{}); err != nil {
			t.Fatal(err)
		}
	}
	step(start)
	g.ibc(stamp(start.Add(time.Minute), ibcRefused))
	step(start.Add(10 * time.Minute))
	g.ibc()
	g.adapter()
	g.unit(0, "inactive", "dead")
	step(start.Add(15 * time.Minute))
	h, err := loadHostGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	if h.Since == nil {
		t.Fatal("adapter stop falsely resolved the observed IBC logout")
	}
	g.ibc(stamp(start.Add(20*time.Minute), ibcCompleted))
	step(start.Add(20 * time.Minute))
	h, err = loadHostGateway(path)
	if err != nil || h.Since != nil {
		t.Fatalf("completed login did not resolve: %+v %v", h, err)
	}
}

func TestPagingPreviousBuildAdapterStateCannotSupplyRecovery(t *testing.T) {
	for _, losses := range []string{`{"retired.service":{}}`, `{"ti-adapter-boost.service":{}}`, `{"ti-adapter-boost.service":{"since":"2024-01-02T12:00:00-05:00","started":"2024-01-02T11:00:00-05:00"}}`} {
		t.Run(losses, func(t *testing.T) {
			g := newStandInGateway(t)
			g.ibc()
			g.flag("systemctl-fail", true)
			path := t.TempDir() + "/gateway.json"
			start := tuesday(12, 0)
			raw := fmt.Sprintf(`{"logged_out_since":%q,"notice_kind":"ibkr-connectivity","adapter_losses":%s}`, start.Format(time.RFC3339), losses)
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			sched, err := gatewayScheduleFrom(func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			n := &policyRecorder{channel: "ntfy"}
			err = gatewayStep(path, gatewayJournalChecker(g.path(), []string{"ti-adapter-boost.service"}), func() (gatewayUnitSample, error) { return gatewayUnitSample{active: true}, nil }, "synthetic", sched, nil, "", nil, []sender{n}, nil, func() time.Time { return start.Add(15 * time.Minute) }, &pagingPolicy{session: true})
			if err != nil {
				t.Fatalf("removed adapter sampling affected migration: %v", err)
			}
			h, err := loadHostGateway(path)
			if err != nil || h.Since == nil || !h.Since.Equal(start) {
				t.Fatalf("previous build lost open logout: %+v %v", h, err)
			}
			if len(n.alerts) != 1 || n.alerts[0].Kind != report.KindGatewayLoggedOut {
				t.Fatalf("previous build must route as operational Gateway incident: %+v", n.alerts)
			}
		})
	}
}
