package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// Each Gateway follows its own login evidence, restart windows and incident
// lifecycle (docs/manual/server.md, "The Traditional IRA's Gateway").
func TestGatewayInstancesPagingMatrix(t *testing.T) {
	for _, account := range []string{"boost", "trad-ira"} {
		for _, status := range []string{"logged-in", "logged-out", "IBKR-unreachable"} {
			for _, inside := range []bool{false, true} {
				for _, present := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/window=%v/env=%v", account, status, inside, present), func(t *testing.T) {
						g := newStandInGateway(t)
						dir := t.TempDir()
						envPath := filepath.Join(dir, "gateway-instance.env")
						if present {
							if err := os.WriteFile(envPath, []byte("# instance schedule\nTI_GATEWAY_RESTART_TIME=\"11:55 PM\"\nTI_GATEWAY_COLD_RESTART=09:20\n"), 0o600); err != nil {
								t.Fatal(err)
							}
						}
						unit, adapter, name, state := gatewayUnit, "ti-adapter-boost.service", "IB Gateway", "host-gateway.json"
						if account != "boost" {
							unit, adapter, name, state = "ti-gateway@trad-ira.service", "ti-adapter@trad-ira.service", "IB Gateway (trad-ira)", "host-gateway-trad-ira.json"
						}
						at := time.Date(2024, 1, 7, 14, 0, 0, 0, newYork)
						if inside {
							at = time.Date(2024, 1, 7, 9, 15, 0, 0, newYork)
						}
						event := ibcCompleted
						if status == "logged-out" {
							event = ibcRefused
						}
						g.write(unit, stamp(at.Add(-4*time.Hour), event))
						if status == "IBKR-unreachable" {
							g.write(adapter, stamp(at.Add(-4*time.Hour), linkLost))
						}
						// The other Gateway always disagrees, so cross-reading cannot pass.
						other := "ti-gateway@trad-ira.service"
						if account != "boost" {
							other = gatewayUnit
						}
						opposite := ibcRefused
						if status != "logged-in" {
							opposite = ibcCompleted
						}
						g.write(other, stamp(at.Add(-4*time.Hour), opposite))
						r := &relay{sent: map[string][]string{}}
						for _, delta := range []time.Duration{0, 10 * time.Minute, 15 * time.Minute} {
							if err := gatewayRun(t, r, dir, g.path(), account, at.Add(delta), "-gateway-instance-env", envPath); err != nil {
								t.Fatal(err)
							}
						}
						want := status != "logged-in" && !inside && (account == "boost" || present)
						for _, host := range []string{ntfyHost, pdHost} {
							text := strings.Join(r.to(host), "\n")
							title := name + " is logged out"
							if status == "IBKR-unreachable" {
								title = name + " is logged in but IBKR"
							}
							n := strings.Count(text, title)
							if (want && n != 1) || (!want && n != 0) {
								t.Fatalf("pages=%d want page=%v: %s", n, want, text)
							}
							if want && account != "boost" {
								for _, s := range []string{"sudo systemctl restart ti-gateway@trad-ira", "ti-gateway-heal@ti-adapter@trad-ira.service.service"} {
									if !strings.Contains(text, s) {
										t.Errorf("missing remedy %s: %s", s, text)
									}
								}
							}
						}
						_, err := os.Stat(filepath.Join(dir, state))
						if account != "boost" && !present {
							if !os.IsNotExist(err) {
								t.Errorf("absent env created state: %v", err)
							}
						} else if err != nil {
							t.Fatal(err)
						}
						if want {
							g.write(unit, stamp(at.Add(20*time.Minute), ibcCompleted))
							g.write(adapter, stamp(at.Add(20*time.Minute), linkUp))
							if err := gatewayRun(t, r, dir, g.path(), account, at.Add(20*time.Minute), "-gateway-instance-env", envPath); err != nil {
								t.Fatal(err)
							}
							if n := count(r.to(pdHost), `"event_action":"resolve"`); n != 1 {
								t.Fatalf("resolutions=%d", n)
							}
						}
					})
				}
			}
		}
	}
}

func TestGatewayInstanceHookAndTemplate(t *testing.T) {
	hooks := directive(t, "../../deploy/systemd/ti-adapter@.service", "[Unit]", "OnFailure")
	expectedHealer := "ti-gateway-heal@ti-adapter@trad-ira.service.service"
	foundHealer := false
	for _, hook := range hooks {
		if strings.ReplaceAll(hook, "%n", "ti-adapter@trad-ira.service") == expectedHealer {
			foundHealer = true
		}
	}
	if !foundHealer {
		t.Fatalf("healer remedy disagrees with adapter hook: %v", hooks)
	}

	for _, bad := range []string{"ti-gateway@.service", "ti-gateway@Trad.service", "ti-gateway@-x.service", "ti-gateway@trad-ira.timer"} {
		if tradingUnit.MatchString(bad) {
			t.Errorf("accepted %s", bad)
		}
	}
	r := &relay{sent: map[string][]string{}}
	s := newStandInSystemd(t)
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		if err := notify(t, r, dir, s, twoChannels(), "ti-gateway@trad-ira.service"); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{ntfyHost, pdHost} {
		if n := pagesFor(r, host, "ti-gateway@trad-ira.service"); n != 1 {
			t.Errorf("pages=%d", n)
		}
	}
	starts := directive(t, reporterUnitPath, "[Service]", "ExecStart")
	for _, s := range []string{"-gateway-instance-env /etc/trend-investing/gateway-%i.env", "ti-gateway@trad-ira.service", "-gateway-adapters ti-adapter-boost.service -gateway-instance-env"} {
		if !strings.Contains(strings.Join(starts, "\n"), s) {
			t.Errorf("template missing %s", s)
		}
	}
}

func TestGatewayMaintenanceHoldsResolveIndependently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "units.json")
	at := tuesday(12, 0)
	for _, scope := range []string{"synthetic", "synthetic/trad-ira"} {
		if err := holdGateway(path, scope, at.Add(20*time.Minute), at); err != nil {
			t.Fatal(err)
		}
	}
	if err := closeGatewayHold(path, "synthetic", true, at.Add(30*time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	if held, err := gatewayHeld(path, "synthetic/trad-ira"); err != nil || !held {
		t.Fatalf("joint recovery cleared IRA hold: held=%v err=%v", held, err)
	}
	if held, err := gatewayHeld(path, "synthetic"); err != nil || held {
		t.Fatalf("IRA hold looks like joint hold: held=%v err=%v", held, err)
	}
	var lines []string
	if err := closeGatewayHold(path, "synthetic/trad-ira", true, at.Add(30*time.Minute), func(line string) { lines = append(lines, line) }, "IB Gateway (trad-ira)"); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "IB Gateway (trad-ira)") {
		t.Fatalf("wrong recovered line: %v", lines)
	}
}

func TestGatewaySelectionKeepsSchedulesStateAndAdaptersSeparate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.env")
	if err := os.WriteFile(path, []byte("TI_GATEWAY_RESTART_TIME=11:55 PM\nTI_GATEWAY_COLD_RESTART='09:20'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jointEnv := env(map[string]string{"TI_GATEWAY_RESTART_TIME": "11:45 PM", "TI_GATEWAY_COLD_RESTART": "09:00"})
	for _, account := range []string{"boost", "trad-ira"} {
		selected, err := selectGateway(account, "boost", path, dir+"/custom-joint.json", dir+"/reporter.json", "ti-adapter-boost.service", jointEnv)
		if err != nil || !selected.enabled {
			t.Fatalf("enabled=%v error=%v", selected.enabled, err)
		}
		target, sched, state, adapters := selected.target, selected.sched, selected.path, selected.units
		wantNight, wantCold, wantState, wantAdapter := 23*60+45, 9*60, dir+"/custom-joint.json", "ti-adapter-boost.service"
		if account != "boost" {
			wantNight, wantCold, wantState, wantAdapter = 23*60+55, 9*60+20, dir+"/host-gateway-trad-ira.json", "ti-adapter@trad-ira.service"
		}
		if sched.nightly != wantNight || sched.cold != wantCold || state != wantState || strings.Join(adapters, ",") != wantAdapter {
			t.Fatalf("wrong selection: %v %s %v", sched, state, adapters)
		}
		at := time.Date(2024, 1, 7, 9, 40, 0, 0, newYork)
		if sched.inWindow(at) != (account == "trad-ira") {
			t.Fatalf("%s did not use its own Sunday restart window", account)
		}
		if target.unit() == "" {
			t.Fatal("missing target")
		}
	}
	for _, tc := range []struct {
		account, owner, file, contents string
		wantErr, enabled               bool
	}{
		{"boost", "", path, "", false, false},
		{"roth-ira", "boost", dir + "/missing", "", false, false},
		{"trad-ira", "boost", "", "", false, false},
		{"../bad", "boost", path, "", true, false},
		{"trad-ira", "boost", dir, "", true, false},
		{"trad-ira", "boost", path, "not-an-assignment", true, false},
		{"trad-ira", "boost", path, "=bad", true, false},
		{"trad-ira", "boost", path, "TI_GATEWAY_COLD_RESTART='09:20", true, false},
		{"trad-ira", "boost", path, "TI_GATEWAY_COLD_RESTART=x", true, false},
		{"trad-ira", "boost", path, "# defaults\n", false, true},
		{"trad-ira", "boost", path, "; systemd comment\nTI_GATEWAY_COLD_RESTART=09:20\n", false, true},
	} {
		t.Run(fmt.Sprintf("%s/%s/%s", tc.account, tc.file, tc.contents), func(t *testing.T) {
			if tc.contents != "" {
				if err := os.WriteFile(tc.file, []byte(tc.contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			selected, err := selectGateway(tc.account, tc.owner, tc.file, "", dir+"/state.json", "", jointEnv)
			if (err != nil) != tc.wantErr || selected.enabled != tc.enabled {
				t.Fatalf("enabled=%v err=%v", selected.enabled, err)
			}
		})
	}
}

func TestInstanceGatewayResetWaitNamesItsOwnIncidentAndUnit(t *testing.T) {
	g := newStandInGateway(t)
	g.write("ti-gateway@trad-ira.service", stamp(tuesday(23, 45), "host gateway-start[1]: gateway-start: reset wait: login refused at 23:45"))
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	file := filepath.Join(dir, "gateway.env")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gatewayRun(t, r, dir, g.path(), "trad-ira", tuesday(23, 50), "-gateway-instance-env", file); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(r.to(ntfyHost), "\n")
	for _, s := range []string{"IB Gateway (trad-ira) reset wait", "ti-gateway-heal@ti-adapter@trad-ira.service.service"} {
		if !strings.Contains(text, s) {
			t.Errorf("missing %s: %s", s, text)
		}
	}
	record, err := loadHostGateway(filepath.Join(dir, "host-gateway-trad-ira.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(record.Keys, ","), "gateway-logged-out:"+hostName()+"/trad-ira") {
		t.Fatalf("wrong reset-wait key: %v", record.Keys)
	}
	calls, err := os.ReadFile(filepath.Join(g.dir, "systemctl-argv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "ti-gateway@trad-ira.service") || strings.Contains(string(calls), " ti-gateway.service") {
		t.Errorf("wrong unit sampled: %s", calls)
	}
}

func TestGatewayIncidentsRemindAndRecoverIndependentlyOnOneHost(t *testing.T) {
	g := newStandInGateway(t)
	g.logout()
	g.write("ti-gateway@trad-ira.service", stamp(tuesday(0, 0), ibcRefused))
	dir := t.TempDir()
	file := filepath.Join(dir, "gateway.env")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &relay{sent: map[string][]string{}}
	fire := func(account string, minute int) {
		t.Helper()
		if err := gatewayRun(t, r, dir, g.path(), account, tuesday(14, 0).Add(time.Duration(minute)*time.Minute), "-gateway-instance-env", file); err != nil {
			t.Fatal(err)
		}
	}
	for _, minute := range []int{0, 10, 15, 70} {
		for _, account := range []string{"boost", "trad-ira"} {
			fire(account, minute)
		}
	}
	for _, host := range []string{ntfyHost, pdHost} {
		text := strings.Join(r.to(host), "\n")
		for _, title := range []string{"IB Gateway is logged out", "IB Gateway (trad-ira) is logged out"} {
			if n := strings.Count(text, title); n != 2 {
				t.Fatalf("%s: %s pages=%d want first and reminder", host, title, n)
			}
		}
	}
	joint, err := loadHostGateway(filepath.Join(dir, "host-gateway.json"))
	if err != nil {
		t.Fatal(err)
	}
	ira, err := loadHostGateway(filepath.Join(dir, "host-gateway-trad-ira.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range joint.Keys {
		if strings.Contains(key, "/trad-ira") {
			t.Fatalf("joint holds IRA receipt %s", key)
		}
	}
	found := false
	for _, key := range ira.Keys {
		if strings.Contains(key, "gateway-logged-out:"+hostName()+"/trad-ira") {
			found = true
		}
	}
	if !found {
		t.Fatalf("IRA incident key is not scoped: %v", ira.Keys)
	}
	g.login(tuesday(15, 15))
	fire("boost", 75)
	fire("trad-ira", 75)
	if n := count(r.to(pdHost), `"event_action":"resolve"`); n != 1 {
		t.Fatalf("joint recovery resolved %d incidents", n)
	}
	ira, err = loadHostGateway(filepath.Join(dir, "host-gateway-trad-ira.json"))
	if err != nil || ira.Since == nil {
		t.Fatalf("joint recovery cleared IRA outage: %+v %v", ira, err)
	}
	g.logout()
	fire("boost", 80)
	fire("boost", 90)
	if n := strings.Count(strings.Join(r.to(ntfyHost), "\n"), "IB Gateway is logged out"); n != 3 {
		t.Fatalf("new joint outage pages=%d want third", n)
	}
}

// The instance's logout page names its own account, lockout file, restart and
// journal, never the joint Gateway's: a joint remedy on the IRA's page would
// clear the wrong lockout (docs/manual/server.md, "The Traditional IRA's Gateway").
func TestInstanceLogoutPageRewritesEveryJointRemedy(t *testing.T) {
	at := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	joint := report.GatewayLoggedOutAlert("host", at.Add(-time.Hour), at, "said")
	page := gatewayTarget{instance: "trad-ira"}.present(joint)
	for _, s := range []string{
		"the trad-ira account's engine is blind and cannot trade",
		"/var/lib/ti-gateway-trad-ira/login-failures",
		"`sudo systemctl restart ti-gateway@trad-ira`",
		"`journalctl -u ti-gateway@trad-ira`",
	} {
		if !strings.Contains(page.Body, s) {
			t.Errorf("instance page lacks %q: %s", s, page.Body)
		}
	}
	for _, s := range []string{
		"every account this Gateway serves",
		"/var/lib/ti-gateway/login-failures",
		"`sudo systemctl restart ti-gateway`",
		"`journalctl -u ti-gateway`",
	} {
		if strings.Contains(page.Body, s) {
			t.Errorf("instance page keeps the joint %q: %s", s, page.Body)
		}
	}
}
