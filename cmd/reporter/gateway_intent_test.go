package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type gatewayIntentSender struct {
	policyRecorder
	resolutions []string
	fail        bool
}

func (s *gatewayIntentSender) resolve(key string) error {
	s.resolutions = append(s.resolutions, key)
	if s.fail {
		return errors.New("resolution failed")
	}
	return nil
}

// Provisioned instances follow owner intent, while the joint check always runs
// (docs/manual/server.md, "The Traditional IRA's Gateway").
func TestGatewayOwnerIntentMatrix(t *testing.T) {
	for _, instance := range []string{"", "trad-ira"} {
		for _, fileState := range []string{"enabled", "enabled-runtime", "disabled", "masked", "static", "not-found"} {
			for _, active := range []string{"active", "activating", "reloading", "inactive", "failed", "deactivating", "maintenance", "refreshing"} {
				for _, open := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/open=%v", instance, fileState, active, open), func(t *testing.T) {
						g := newStandInGateway(t)
						g.unit(0, active, "dead")
						if err := os.WriteFile(filepath.Join(g.dir, "unitfilestate"), []byte(fileState), 0o600); err != nil {
							t.Fatal(err)
						}
						target := gatewayTarget{instance: instance}
						path := filepath.Join(t.TempDir(), "gateway.json")
						start := tuesday(14, 0)
						at := start
						p := &gatewayIntentSender{policyRecorder: policyRecorder{channel: "pagerduty"}}
						tracker := &fakeTracker{}
						issues := newIssues(tracker)
						sched, err := gatewayScheduleFrom(func(string) string { return "" })
						if err != nil {
							t.Fatal(err)
						}
						var notes []string
						checks := 0
						check := func(time.Time) (gatewayEvidence, error) {
							checks++
							return gatewayEvidence{out: true, said: "old refused login"}, nil
						}
						step := func() error {
							return target.step(path, check, gatewayUnitSampler(filepath.Join(g.dir, "systemctl"), target), "synthetic", sched, nil, "", func(s string) { notes = append(notes, s) }, []sender{p, issues}, nil, func() time.Time { return at }, &pagingPolicy{})
						}
						// Seed an incident through the regular delivery path before switching off.
						if open {
							if err := os.WriteFile(filepath.Join(g.dir, "unitfilestate"), []byte("enabled"), 0o600); err != nil {
								t.Fatal(err)
							}
							if err := step(); err != nil {
								t.Fatal(err)
							}
							at = start.Add(10 * time.Minute)
							if err := step(); err != nil {
								t.Fatal(err)
							}
							if len(p.alerts) != 1 {
								t.Fatalf("seed pages=%d", len(p.alerts))
							}
							if err := os.WriteFile(filepath.Join(g.dir, "unitfilestate"), []byte(fileState), 0o600); err != nil {
								t.Fatal(err)
							}
						}
						p.alerts = nil
						checks = 0
						notes = nil
						skip := instance != "" && fileState != "enabled" && fileState != "enabled-runtime" && (active != "active" && active != "activating" && active != "reloading")
						for _, delta := range []time.Duration{2 * time.Hour, 3 * time.Hour} {
							at = start.Add(delta)
							if err := step(); err != nil {
								t.Fatal(err)
							}
						}
						if skip {
							if checks != 0 || len(p.alerts) != 0 {
								t.Fatalf("skipped check read journals %d times and paged %d times", checks, len(p.alerts))
							}
							want := "gateway (trad-ira): skipped: ti-gateway@trad-ira.service is " + fileState + " and not running"
							if len(notes) != 2 || notes[0] != want || notes[1] != want {
								t.Fatalf("notes=%q", notes)
							}
							wantResolves := 0
							if open {
								wantResolves = 1
							}
							if len(tracker.issues) != wantResolves || (open && tracker.issues[0].open) {
								t.Fatalf("skipped instance retained or opened an alert issue: %+v", tracker.issues)
							}
							if len(p.resolutions) != wantResolves {
								t.Fatalf("resolutions=%v", p.resolutions)
							}
							h, err := loadHostGateway(path)
							if err != nil || h.Since != nil || h.Resolving || len(h.Keys) != 0 {
								t.Fatalf("state=%+v err=%v", h, err)
							}
						} else if checks != 2 || len(notes) != 0 || len(p.alerts) == 0 || len(p.resolutions) != 0 {
							t.Fatalf("checks=%d notes=%v pages=%d resolutions=%v", checks, notes, len(p.alerts), p.resolutions)
						}
						raw, err := os.ReadFile(filepath.Join(g.dir, "systemctl-argv"))
						if err != nil {
							t.Fatal(err)
						}
						if instance != "" && !strings.Contains(string(raw), "-p UnitFileState") {
							t.Fatalf("missing intent property: %s", raw)
						}
					})
				}
			}
		}
	}
}

func TestGatewayOffResolutionRetriesAndSystemdErrorsFailClosed(t *testing.T) {
	g := newStandInGateway(t)
	g.write("ti-gateway@trad-ira.service", stamp(tuesday(0, 0), ibcRefused))
	dir := t.TempDir()
	file := filepath.Join(dir, "gateway.env")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &refusingResolve{relay: &relay{sent: map[string][]string{}}}
	fire := func(minute int) error {
		return gatewayRun(t, f, dir, g.path(), "trad-ira", tuesday(14, 0).Add(time.Duration(minute)*time.Minute), "-gateway-instance-env", file)
	}
	for _, minute := range []int{0, 10} {
		if err := fire(minute); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "host-gateway-trad-ira.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	g.unit(0, "inactive", "dead")
	if err := os.WriteFile(filepath.Join(g.dir, "unitfilestate"), []byte("disabled"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"systemctl-fail", "systemctl-raw"} {
		raw := []byte("NRestarts=0\nActiveState=inactive\nSubState=dead\n")
		if err := os.WriteFile(filepath.Join(g.dir, bad), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := fire(70); err == nil {
			t.Fatal("uncertain systemd sample succeeded")
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, before) {
			t.Fatalf("uncertain sample changed incident: %s %v", after, err)
		}
		if err := os.Remove(filepath.Join(g.dir, bad)); err != nil {
			t.Fatal(err)
		}
	}
	if err := fire(75); err == nil {
		t.Fatal("failed resolution did not fail firing")
	}
	h, err := loadHostGateway(path)
	if err != nil || h.Since != nil || !h.Resolving {
		t.Fatalf("resolution not retained: %+v %v", h, err)
	}
	f.mu.Lock()
	f.allow = true
	f.mu.Unlock()
	for _, minute := range []int{80, 140} {
		if err := fire(minute); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(f.to(pdHost), `"event_action":"resolve"`); n != 1 {
		t.Fatalf("resolutions=%d", n)
	}
	if n := strings.Count(strings.Join(f.to(pdHost), "\n"), "IB Gateway (trad-ira) is logged out"); n != 1 {
		t.Fatalf("pages=%d", n)
	}
	h, err = loadHostGateway(path)
	if err != nil || h.Since != nil || h.Resolving || len(h.Keys) != 0 {
		t.Fatalf("incident remains: %+v %v", h, err)
	}
}
