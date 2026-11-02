package main

// Gateway invariant (docs/manual/server.md, "The Gateway is logged out" and
// "Gateway healer"; ADR 0019, "Alerting"):
// (a) IBC logout or refused login keeps today's logged-out alert regardless of adapters.
// (b) Link evidence with IBC logged in uses the same key with IBKR-unreachable wording.
// (c) An adapter stop never resolves an IBC-established outage.
// (e) An adapter journal read failure never suppresses an IBC-established outage.
// (f) The existing state-file shape round-trips unchanged, with no new keys.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

func gatewayNoticeStep(t *testing.T, g standInGateway, path string, at time.Time, recorder *policyRecorder, adapters ...string) {
	t.Helper()
	if err := gatewayNoticeStepError(t, g, path, at, recorder, adapters...); err != nil {
		t.Fatal(err)
	}
}

func gatewayNoticeStepError(t *testing.T, g standInGateway, path string, at time.Time, recorder *policyRecorder, adapters ...string) error {
	t.Helper()
	sched, err := gatewayScheduleFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return gatewayStep(path, gatewayJournalChecker(g.path(), adapters), func() (gatewayUnitSample, error) {
		return gatewayUnitSample{active: true}, nil
	}, "synthetic", sched, nil, "", nil, []sender{recorder}, nil, func() time.Time { return at })
}

func TestGatewayIBCLogoutAlwaysKeepsLoggedOutAlert(t *testing.T) {
	start := tuesday(12, 0)
	for _, event := range []string{"host ibc[1]: LoginState is LOGGED_OUT", ibcRefused} {
		for name, lines := range map[string][]string{
			"silent":       nil,
			"lost":         {stamp(start.Add(-4*time.Hour), linkLost)},
			"restored":     {stamp(start, linkUp)},
			"reconciled":   {stamp(start, linkReconciled)},
			"read failure": nil,
		} {
			t.Run(event+"/"+name, func(t *testing.T) {
				g := newStandInGateway(t)
				g.ibc(stamp(start.Add(-5*time.Hour), ibcCompleted), stamp(start.Add(-time.Hour), event))
				g.adapter(lines...)
				g.flag("fail-ti-adapter-boost.service", name == "read failure")
				p := &policyRecorder{channel: "ntfy"}
				path := filepath.Join(t.TempDir(), "gateway.json")
				for _, at := range []time.Time{start, start.Add(10 * time.Minute)} {
					err := gatewayNoticeStepError(t, g, path, at, p, "ti-adapter-boost.service")
					if (err != nil) != (name == "read failure") {
						t.Fatalf("adapter read failure must fail the firing after deciding the outage: %v", err)
					}
				}
				if len(p.alerts) != 1 {
					t.Fatalf("delivered %d alerts, want one IBC-established outage", len(p.alerts))
				}
				a := p.alerts[0]
				prefix, evidence, ok := strings.Cut(a.Body, "The check said: ")
				if !ok || prefix == "" {
					t.Fatalf("missing logged-out check text: %s", a.Body)
				}
				evidence, _, ok = strings.Cut(evidence, " Remedy (")
				want := report.GatewayLoggedOutAlert("synthetic", start, start.Add(10*time.Minute), evidence)
				if !ok || !reflect.DeepEqual(a, want) || !strings.Contains(evidence, event) {
					t.Fatalf("IBC outage changed today's logged-out alert: %+v", a)
				}
			})
		}
	}
}

func TestGatewayIBKRUnreachableKeepsIncidentAndTiming(t *testing.T) {
	for _, code := range []string{"1100", "2110"} {
		t.Run(code, func(t *testing.T) {
			g := newStandInGateway(t)
			start := tuesday(12, 0)
			g.ibc(stamp(start.Add(-5*time.Hour), ibcCompleted))
			g.adapter(stamp(start.Add(-3*time.Hour), strings.ReplaceAll(linkLost, "2110", code)))
			p := &policyRecorder{channel: "ntfy"}
			path := filepath.Join(t.TempDir(), "gateway.json")
			for _, offset := range []time.Duration{-time.Second, 0, 9 * time.Minute} {
				gatewayNoticeStep(t, g, path, start.Add(offset), p, "ti-adapter-boost.service")
				if len(p.alerts) != 0 {
					t.Fatal("link evidence changed the three-hour or ten-minute grace")
				}
			}
			gatewayNoticeStep(t, g, path, start.Add(10*time.Minute), p, "ti-adapter-boost.service")
			if len(p.alerts) != 1 {
				t.Fatalf("delivered %d alerts, want one", len(p.alerts))
			}
			a := p.alerts[0]
			if a.Title != "synthetic: IB Gateway is logged in but IBKR's servers are unreachable" {
				t.Fatalf("wrong link-only title: %s", a.Title)
			}
			want := report.GatewayLoggedOutAlert("synthetic", start, start.Add(10*time.Minute), "")
			metadata := a
			metadata.Title, metadata.Body = want.Title, want.Body
			if !reflect.DeepEqual(metadata, want) {
				t.Fatalf("wording changed alert identity or routing: %+v", a)
			}
			for _, text := range []string{"Gateway healer", "only when an adapter stops at the end of a scheduled window", "Inside a scheduled IBKR window the adapter waits", "outside a window, check IBKR's system status", "over three hours", "IBKR's system status", "sudo systemctl restart ti-gateway", "1100/2110", "If the healer's journal (journalctl -u ti-gateway-heal@*) shows it stood down for login-problem, order-busy or owner-stopped, do not restart by hand; follow server.md, \"Gateway healer\"."} {
				if !strings.Contains(a.Body, text) {
					t.Errorf("missing %q in remedy: %s", text, a.Body)
				}
			}
			for _, text := range []string{"check the paper login", "login-failures", "let the scheduled window finish", "restarts the Gateway automatically", "has been logged out"} {
				if strings.Contains(a.Body, text) {
					t.Errorf("link-only notice gives logout remedy %q", text)
				}
			}
			gatewayNoticeStep(t, g, path, start.Add(69*time.Minute), p, "ti-adapter-boost.service")
			if len(p.alerts) != 1 {
				t.Fatal("reminded before the existing hourly interval")
			}
			gatewayNoticeStep(t, g, path, start.Add(70*time.Minute), p, "ti-adapter-boost.service")
			if len(p.alerts) != 2 || p.alerts[1].Key != a.Key {
				t.Fatal("reminder changed incident")
			}
			g.adapter(stamp(start.Add(71*time.Minute), linkUp))
			gatewayNoticeStep(t, g, path, start.Add(75*time.Minute), p, "ti-adapter-boost.service")
			h, err := loadHostGateway(path)
			if err != nil || h.Since != nil || len(h.Keys) != 0 {
				t.Fatalf("restoration did not resolve the same incident: %+v %v", h, err)
			}
		})
	}
}

func TestGatewayAdapterStopPreservesIBCOutage(t *testing.T) {
	TestPagingLogoutCannotResolveWhenAdapterStops(t)
}

func TestGatewayAdapterReadFailureDoesNotSuppressIBCOutage(t *testing.T) {
	for _, failure := range []string{"fail", "noaccess", "malformed"} {
		t.Run(failure, func(t *testing.T) {
			g := newStandInGateway(t)
			start := tuesday(12, 0)
			g.ibc(stamp(start.Add(-time.Hour), ibcRefused))
			if failure == "malformed" {
				g.adapter("not-a-time " + linkLost)
			} else {
				g.flag(failure+"-ti-adapter-boost.service", true)
			}
			g.write("ti-adapter@synthetic.service", stamp(start, linkUp))
			p := &policyRecorder{channel: "ntfy"}
			path := filepath.Join(t.TempDir(), "gateway.json")
			for _, offset := range []time.Duration{0, 10 * time.Minute} {
				err := gatewayNoticeStepError(t, g, path, start.Add(offset), p, "ti-adapter-boost.service", "ti-adapter@synthetic.service")
				if err == nil || !strings.Contains(err.Error(), "could not read") {
					t.Errorf("adapter read fault must fail the firing after deciding the outage: %v", err)
				}
			}
			if len(p.alerts) != 1 || p.alerts[0].Title != "synthetic: IB Gateway is logged out" {
				t.Fatalf("IBC outage suppressed: %+v", p.alerts)
			}
			if !strings.Contains(p.alerts[0].Body, "could not read") || !strings.Contains(p.alerts[0].Body, "ti-adapter-boost.service") {
				t.Fatalf("adapter read failure missing from alert: %s", p.alerts[0].Body)
			}
			calls, err := os.ReadFile(filepath.Join(g.dir, "argv"))
			if err != nil || !strings.Contains(string(calls), "-u ti-adapter@synthetic.service") {
				t.Fatalf("failed adapter prevented reading the next: %s %v", calls, err)
			}
			g.ibc(stamp(start.Add(-time.Hour), ibcCompleted))
			if _, err := gatewayJournalChecker(g.path(), []string{"ti-adapter-boost.service"})(start); err == nil {
				t.Fatal("without IBC outage, unreadable adapter must still fail closed")
			}
		})
	}
}

func TestGatewayMainStateShapeRoundTripsUnchanged(t *testing.T) {
	const raw = `{"keys":["ntfy:gateway-logged-out:synthetic"],"sent":{"ntfy:gateway-logged-out:synthetic":"2024-01-02T17:00:00Z"},"repeats":{"ntfy:gateway-logged-out:synthetic":2},"baselines":{"ntfy":"2024-01-02T16:00:00Z"},"logged_out_since":"2024-01-02T16:50:00Z","resolving":true,"restarts":7,"baseline":4,"quiet":1,"start_failing":true,"restart_only":true,"held_in_maintenance_until":"2024-01-02T18:00:00Z"}`
	path := filepath.Join(t.TempDir(), "gateway.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := loadHostGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]any
	if err := json.Unmarshal([]byte(raw), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("main's state changed on round trip: %s", got)
	}
	for _, typ := range []reflect.Type{reflect.TypeFor[hostGateway](), reflect.TypeFor[delivered]()} {
		for field := range typ.Fields() {
			if field.Anonymous {
				continue
			}
			key, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if _, ok := before[key]; !ok {
				t.Errorf("new state field %s (%q) absent from main's shape", field.Name, key)
			}
		}
	}
}

func TestGatewayLinkNoticeNamesOnlyAffectedAccounts(t *testing.T) {
	g := newStandInGateway(t)
	start := tuesday(12, 0)
	g.ibc(stamp(start.Add(-5*time.Hour), ibcCompleted))
	g.adapter(stamp(start.Add(-4*time.Hour), linkLost))
	g.write("ti-adapter@trad-ira.service", stamp(start, linkReconciled))
	p := &policyRecorder{channel: "ntfy"}
	path := filepath.Join(t.TempDir(), "gateway.json")
	gatewayNoticeStep(t, g, path, start, p, "ti-adapter-boost.service", "ti-adapter@trad-ira.service")
	gatewayNoticeStep(t, g, path, start.Add(10*time.Minute), p, "ti-adapter-boost.service", "ti-adapter@trad-ira.service")
	if len(p.alerts) != 1 {
		t.Fatalf("want one page, got %+v", p.alerts)
	}
	body := p.alerts[0].Body
	if !strings.Contains(body, "ti-adapter-boost.service") || !strings.Contains(body, "Other accounts' trading status is not established by this check") || strings.Contains(body, "every account") || strings.Contains(body, "Nothing reaches IBKR") {
		t.Fatalf("link-only notice overstates affected accounts: %s", body)
	}
}

func TestGatewayAdapterReadFaultFailsDuringMaintenance(t *testing.T) {
	g := newStandInGateway(t)
	start := tuesday(12, 0)
	g.ibc(stamp(start.Add(-time.Hour), ibcRefused))
	g.adapter("not-a-time " + linkLost)
	sched, err := gatewayScheduleFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	p := &policyRecorder{channel: "ntfy"}
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.json")
	err = gatewayStep(path, gatewayJournalChecker(g.path(), []string{"ti-adapter-boost.service"}), func() (gatewayUnitSample, error) {
		return gatewayUnitSample{active: true}, nil
	}, "synthetic", sched, func() time.Time { return start.Add(time.Hour) }, filepath.Join(dir, "units.json"), nil, []sender{p}, nil, func() time.Time { return start })
	if err == nil || !strings.Contains(err.Error(), "could not read") {
		t.Errorf("maintenance swallowed adapter read fault: %v", err)
	}
	h, loadErr := loadHostGateway(path)
	if loadErr != nil || h.Since == nil || len(p.alerts) != 0 {
		t.Errorf("maintenance must retain outage without delivery: %+v %+v %v", h, p.alerts, loadErr)
	}
}
