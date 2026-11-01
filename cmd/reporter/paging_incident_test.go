package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

const adapterA = "ti-adapter-a.service"
const adapterB = "ti-adapter-b.service"

func adapterSample(t *testing.T, g standInGateway, unit, state string, start time.Time) {
	t.Helper()
	raw := fmt.Sprintf("ActiveState=%s\nExecMainStartTimestamp=@%d\n", state, start.Unix())
	if err := os.WriteFile(g.dir+"/"+unit+".sample", []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func adapterFixture(t *testing.T) (standInGateway, string, time.Time) {
	t.Helper()
	g := newStandInGateway(t)
	script := "#!/bin/sh\nfor unit; do :; done\ncat '" + g.dir + "/'\"$unit\".sample\n"
	if err := os.WriteFile(g.dir+"/systemctl", []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 31, 18, 0, 0, 0, newYork)
	g.ibc(stamp(start.Add(-time.Hour), ibcCompleted))
	for _, unit := range []string{adapterA, adapterB} {
		adapterSample(t, g, unit, "active", start.Add(-2*time.Hour))
	}
	return g, t.TempDir() + "/gateway.json", start
}

func adapterStep(t *testing.T, g standInGateway, path string, at time.Time, senders ...sender) error {
	t.Helper()
	sched, err := gatewayScheduleFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return gatewayStep(path, gatewayJournalChecker(g.path(), []string{adapterA, adapterB}, g.dir+"/systemctl"), func() (gatewayUnitSample, error) { return gatewayUnitSample{active: true}, nil }, "synthetic", sched, nil, "", nil, senders, nil, func() time.Time { return at }, &pagingPolicy{after: 30 * time.Minute})
}

func TestPagingAdapterEvidenceIsUnitScoped(t *testing.T) {
	for _, other := range []string{"obsolete", "restored", "lost"} {
		t.Run(other, func(t *testing.T) {
			g, path, start := adapterFixture(t)
			g.write(adapterA, stamp(start.Add(-time.Minute), linkLost))
			if err := adapterStep(t, g, path, start); err != nil {
				t.Fatal(err)
			}
			g.write(adapterA)
			switch other {
			case "obsolete":
				g.write(adapterB, stamp(start.Add(-3*time.Hour), linkLost))
			case "restored":
				g.write(adapterB, stamp(start.Add(time.Minute), linkUp))
			case "lost":
				g.write(adapterB, stamp(start.Add(time.Minute), linkLost))
				if err := adapterStep(t, g, path, start.Add(2*time.Minute)); err != nil {
					t.Fatal(err)
				}
				g.write(adapterB, stamp(start.Add(3*time.Minute), linkUp))
			}
			if err := adapterStep(t, g, path, start.Add(10*time.Minute)); err != nil {
				t.Fatal(err)
			}
			h, err := loadHostGateway(path)
			if err != nil {
				t.Fatal(err)
			}
			if h.Since == nil {
				t.Fatal("adapter B evidence resolved adapter A's unobserved recovery")
			}
			adapterSample(t, g, adapterA, "active", start.Add(11*time.Minute))
			if err := adapterStep(t, g, path, start.Add(15*time.Minute)); err != nil {
				t.Fatal(err)
			}
			h, err = loadHostGateway(path)
			if err != nil {
				t.Fatal(err)
			}
			if h.Since != nil {
				t.Fatal("adapter A's new process did not clear its own incident")
			}
		})
	}
}

func TestPagingEmptyJournalStoppedAdapter(t *testing.T) {
	g, path, start := adapterFixture(t)
	g.write(adapterA, stamp(start.Add(-time.Minute), linkLost))
	if err := adapterStep(t, g, path, start); err != nil {
		t.Fatal(err)
	}
	g.write(adapterA)
	adapterSample(t, g, adapterA, "inactive", start)
	if err := adapterStep(t, g, path, start.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	h, err := loadHostGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	if h.Since != nil {
		t.Fatal("empty journal prevented stopped adapter recovery")
	}
}

func TestPagingPreviousBuildStateClassification(t *testing.T) {
	for _, state := range []string{"inactive", "restarted", "active"} {
		t.Run(state, func(t *testing.T) {
			g, path, start := adapterFixture(t)
			// Previous build's on-disk schema has no notice_kind or adapter ownership.
			raw := fmt.Sprintf(`{"logged_out_since":%q}`, start.Format(time.RFC3339))
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			g.write(adapterA, stamp(start.Add(-time.Minute), linkLost))
			// Unknown legacy ownership includes B until its own invalidation
			// (alerts-and-dashboard.md, "Paging invariant").
			adapterSample(t, g, adapterB, "inactive", start.Add(-time.Hour))
			if state == "inactive" {
				adapterSample(t, g, adapterA, "inactive", start.Add(-time.Hour))
			}
			if state == "restarted" {
				adapterSample(t, g, adapterA, "active", start.Add(time.Minute))
			}
			n := &policyRecorder{channel: "ntfy"}
			if err := adapterStep(t, g, path, start.Add(15*time.Minute), n); err != nil {
				t.Fatal(err)
			}
			h, err := loadHostGateway(path)
			if err != nil {
				t.Fatal(err)
			}
			if state == "active" {
				if h.Notice != report.KindIBKRConnectivity {
					t.Fatalf("legacy loss not classified as connectivity: %+v", h)
				}
			} else if h.Since != nil {
				t.Fatalf("legacy %s adapter retained stale logout", state)
			}
		})
	}
}

func TestPagingAdapterTimestampGrammar(t *testing.T) {
	for _, value := range []string{"@1m", "@1h0", "@+123", "@1e3", "@1.", "@.1", "@-1", "@1.2.3", "@9223372036854775808", "@253402300800", "@0"} {
		t.Run(value, func(t *testing.T) {
			g := newStandInGateway(t)
			if err := os.WriteFile(g.dir+"/systemctl-raw", []byte("ActiveState=active\nExecMainStartTimestamp="+value+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := adapterStart(g.dir+"/systemctl", adapterA); err == nil {
				t.Fatalf("accepted malformed Unix timestamp %q", value)
			}
		})
	}
	for _, value := range []string{"@1793484000", "@1793484000.123456789", "@1793484000.123456789123"} {
		t.Run(value, func(t *testing.T) {
			g := newStandInGateway(t)
			if err := os.WriteFile(g.dir+"/systemctl-raw", []byte("ActiveState=active\nExecMainStartTimestamp="+value+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			active, at, err := adapterStart(g.dir+"/systemctl", adapterA)
			want := int64(0)
			if strings.Contains(value, ".") {
				want = 123456789
			}
			if err != nil || !active || at.Unix() != 1793484000 || int64(at.Nanosecond()) != want {
				t.Fatalf("numeric timestamp parsed incorrectly: %v %s %v", active, at, err)
			}
		})
	}
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
			a := report.IBKRNotice("synthetic", start, false, "synthetic")
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
	g, path, start := adapterFixture(t)
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
		if err := adapterStep(t, g, path, start.Add(time.Duration(mins)*time.Minute), p); err != nil {
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

func TestPagingAdapterSamplingFaultDoesNotStopRouting(t *testing.T) {
	for _, fault := range []string{"command", "state", "timestamp"} {
		t.Run(fault, func(t *testing.T) {
			g, path, start := adapterFixture(t)
			g.write(adapterA, stamp(start.Add(-time.Minute), linkLost))
			if err := adapterStep(t, g, path, start); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "command":
				if err := os.Remove(g.dir + "/" + adapterA + ".sample"); err != nil {
					t.Fatal(err)
				}
			case "state":
				adapterSample(t, g, adapterA, "unknown", start)
			case "timestamp":
				if err := os.WriteFile(g.dir+"/"+adapterA+".sample", []byte("ActiveState=active\nExecMainStartTimestamp=@1m\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			n := &policyRecorder{channel: "ntfy"}
			if err := adapterStep(t, g, path, start.Add(10*time.Minute), n); err == nil || !strings.Contains(err.Error(), adapterA) {
				t.Errorf("missing adapter-specific sampling error: %v", err)
			}
			var connectivity, operational bool
			for _, a := range n.alerts {
				if a.Kind == report.KindIBKRConnectivity {
					connectivity = true
				}
				if strings.Contains(a.Key, adapterA) && a.Class(true) == report.Operational {
					operational = true
				}
			}
			if !connectivity || !operational {
				t.Errorf("sampling failure suppressed routing: connectivity=%v operational=%v", connectivity, operational)
			}
			g.write(adapterA, stamp(start.Add(15*time.Minute), linkUp))
			if err := adapterStep(t, g, path, start.Add(15*time.Minute), n); err == nil {
				t.Error("sampling error lost on recovery")
			}
			h, err := loadHostGateway(path)
			if err != nil {
				t.Fatal(err)
			}
			if h.Since != nil {
				t.Error("sampling failure suppressed journal recovery")
			}
		})
	}
}

func TestPagingOpenIncidentKeepsClocksWhenOldReceiptExpires(t *testing.T) {
	g, path, start := adapterFixture(t)
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
	if err := adapterStep(t, g, path, start.Add(30*time.Minute), p); err != nil {
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
