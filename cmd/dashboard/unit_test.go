package main

import (
	"net"
	"os"
	"regexp"
	"strings"
	"testing"
)

const (
	dashboardUnitPath = "../../deploy/systemd/ti-dashboard.service"
	lightsailRunbook  = "../../docs/runbooks/lightsail-deployment.md"
	runEnvExample     = "../../deploy/etc/engines/run.env.example"
)

var bracedVariable = regexp.MustCompile(`\$\{([A-Z_]+)\}`)

// serviceSettings returns a unit's [Service] Environment values and its
// ExecStart line, with the run.env example's values for the files it reads.
func serviceSettings(t *testing.T, path string) (env map[string]string, execStart string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	env = map[string]string{}
	section := ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		if section != "[Service]" {
			continue
		}
		switch key {
		case "Environment":
			k, v, _ := strings.Cut(value, "=")
			env[k] = v
		case "ExecStart":
			execStart = value
		}
	}
	example, err := os.ReadFile(runEnvExample)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(example), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && !strings.HasPrefix(k, "#") {
			env[k] = v
		}
	}
	return env, execStart
}

// TestTheServiceFileServesTheBoostLogOnTheOnePort: the dashboard takes its
// settings as flags (it reads no environment), so the unit passes -journal,
// the Boost engine's current durable log, and -addr, a loopback address
// whose port is the one the Lightsail runbook publishes to the tailnet with
// `tailscale serve`. One port everywhere: the program's default, the unit
// and the runbook.
func TestTheServiceFileServesTheBoostLogOnTheOnePort(t *testing.T) {
	env, execStart := serviceSettings(t, dashboardUnitPath)
	for key := range env {
		if strings.HasPrefix(key, "TI_DASHBOARD") {
			t.Errorf("the unit sets %s, which the dashboard never reads", key)
		}
	}
	words := strings.Fields(bracedVariable.ReplaceAllStringFunc(execStart, func(v string) string {
		return env[bracedVariable.FindStringSubmatch(v)[1]]
	}))
	if len(words) == 0 || words[0] != "/opt/trend-investing/bin/ti-dashboard" {
		t.Fatalf("ExecStart runs %v", words)
	}
	flag := func(name string) string {
		for i, w := range words {
			if w == name && i+1 < len(words) {
				return words[i+1]
			}
		}
		return ""
	}
	if got, want := flag("-journal"), "/var/lib/trend-investing/engines/boost/"+env["TI_RUN"]+".log"; got != want {
		t.Errorf("-journal %q, want the Boost engine's durable log %q", got, want)
	}
	addr := flag("-addr")
	if err := checkListenAddr(addr, host); err != nil {
		t.Fatalf("the unit's -addr %q: %v", addr, err)
	}
	_, port, _ := net.SplitHostPort(addr)
	if _, defaultPort, _ := net.SplitHostPort(defaultAddr); port != defaultPort {
		t.Errorf("the unit listens on port %s, the program's default is %s", port, defaultPort)
	}
	runbook, err := os.ReadFile(lightsailRunbook)
	if err != nil {
		t.Fatal(err)
	}
	serve := regexp.MustCompile(`tailscale serve --bg (\d+)`).FindAllStringSubmatch(string(runbook), -1)
	if len(serve) == 0 {
		t.Fatal("the Lightsail runbook does not publish the dashboard with tailscale serve")
	}
	for _, m := range serve {
		if m[1] != port {
			t.Errorf("the runbook publishes port %s, the unit listens on %s", m[1], port)
		}
	}
}
