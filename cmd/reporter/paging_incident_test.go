package main

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

func pagingGatewayFixture(t *testing.T) (standInGateway, string, time.Time) {
	t.Helper()
	g := newStandInGateway(t)
	start := time.Date(2026, 10, 31, 18, 0, 0, 0, newYork)
	g.ibc(stamp(start.Add(-time.Hour), ibcCompleted))
	return g, t.TempDir() + "/gateway.json", start
}

func pagingGatewayStep(t *testing.T, g standInGateway, path string, at time.Time, senders ...sender) error {
	t.Helper()
	sched, err := gatewayScheduleFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return gatewayStep(path, gatewayJournalChecker(g.path(), nil), func() (gatewayUnitSample, error) { return gatewayUnitSample{active: true}, nil }, "synthetic", sched, nil, "", nil, senders, nil, func() time.Time { return at }, &pagingPolicy{after: 30 * time.Minute})
}

func TestPagingCancellationRetrySurvivesWindow(t *testing.T) {
	for _, delay := range []time.Duration{0, 30 * time.Minute} {
		t.Run(delay.String(), func(t *testing.T) {
			r := &pagingReceiptTransport{}
			ps := &pushoverSender{url: "https://synthetic.test/1/messages.json", client: &http.Client{Transport: r}}
			start := time.Date(2026, 10, 31, 18, 0, 0, 0, newYork)
			schedule, err := report.ParseIBKRSchedule([]byte(`{"windows":[{"days":["daily"],"start":"18:01","end":"18:05","zone":"America/New_York"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			policy := &pagingPolicy{schedule: schedule, after: delay}
			a := report.GatewayLoggedOutAlert("synthetic", start, start, "synthetic")
			seen, sent, repeats := map[string]bool{}, map[string]time.Time{}, map[string]int{}
			if err := deliver([]sender{ps}, a, seen, sent, repeats, start, time.Hour, func() error { return nil }, &pagingPolicy{}); err != nil {
				t.Fatal(err)
			}
			path := t.TempDir() + "/receipts.json"
			step := func(mins int) error {
				result := deliver([]sender{ps}, a, seen, sent, repeats, start.Add(time.Duration(mins)*time.Minute), time.Hour, func() error { return saveState(path, seen, sent, repeats, nil) }, policy)
				var loadErr error
				seen, sent, repeats, _, loadErr = loadState(path)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				return result
			}
			r.fail = true
			if err := step(4); err == nil {
				t.Fatal("missing failed cancellation")
			}
			if err := step(5); err == nil {
				t.Error("failed cancellation was not retried at window end")
			}
			r.fail = false
			if err := step(6); err != nil {
				t.Fatal(err)
			}
			if len(r.paths) != 4 {
				t.Fatalf("cancellation requests=%v; want initial, failure, failure, success", r.paths)
			}
			if err := step(7); err != nil {
				t.Fatal(err)
			}
			if len(r.paths) != 4 {
				t.Fatal("successful cancellation repeated")
			}
		})
	}
}

func TestPagingNewOutageWhileResolutionPendingKeepsClock(t *testing.T) {
	g, path, start := pagingGatewayFixture(t)
	key := report.GatewayKey("synthetic")
	seen := map[string]bool{channelKey(receiptChannel("pushover", "synthetic", start), key): true}
	h := hostGateway{Since: &start, Resolving: true, delivered: deliveredOf(seen, map[string]time.Time{}, map[string]int{}, nil)}
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	g.ibc(stamp(start, ibcRefused))
	p := &policyRecorder{channel: "pushover"}
	for _, mins := range []int{10, 30} {
		if err := pagingGatewayStep(t, g, path, start.Add(time.Duration(mins)*time.Minute), p); err != nil {
			t.Fatal(err)
		}
	}
	h, err = loadHostGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range []string{"observed", "escalation"} {
		if !h.Sent[channelKey(ch, key)].Equal(start) {
			t.Errorf("%s clock restarted at page: %s", ch, h.Sent[channelKey(ch, key)])
		}
	}
	if len(p.alerts) != 1 {
		t.Fatal("new outage did not escalate from first observation")
	}
}

func TestPagingOpenIncidentKeepsClocksWhenOldReceiptExpires(t *testing.T) {
	g, path, start := pagingGatewayFixture(t)
	key := report.GatewayKey("synthetic")
	escalation := start.Add(20 * time.Minute)
	seen := map[string]bool{channelKey(receiptChannel("pushover", "synthetic", start.Add(-time.Hour)), key): true}
	sent := map[string]time.Time{channelKey("observed", key): start, channelKey("escalation", key): escalation}
	h := hostGateway{Since: &start, Resolving: true, delivered: deliveredOf(seen, sent, map[string]int{}, nil)}
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	g.ibc(stamp(start, ibcRefused))
	p := &policyRecorder{channel: "pushover"}
	if err := pagingGatewayStep(t, g, path, start.Add(30*time.Minute), p); err != nil {
		t.Fatal(err)
	}
	h, err = loadHostGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Sent[channelKey("escalation", key)].Equal(escalation) || len(p.alerts) != 0 {
		t.Fatal("expiry erased the open incident's escalation clock")
	}
}
