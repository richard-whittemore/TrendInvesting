package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// gatewayTarget identifies one independently paged Gateway (server.md,
// "The Traditional IRA's Gateway"). The empty instance is the joint Gateway;
// its unit, incident key, wording and state path retain their original names.
type gatewayTarget struct{ instance string }

func (g gatewayTarget) unit() string {
	if g.instance == "" {
		return gatewayUnit
	}
	return "ti-gateway@" + g.instance + ".service"
}
func (g gatewayTarget) scope(host string) string {
	if g.instance == "" {
		return host
	}
	return host + "/" + g.instance
}
func (g gatewayTarget) name() string {
	if g.instance == "" {
		return gatewayName
	}
	return gatewayName + " (" + g.instance + ")"
}

// present adapts the joint alert to its instance without changing its host or
// paging class (alerts-and-dashboard.md, "Paging invariant").
func (g gatewayTarget) present(a report.Alert) report.Alert {
	if g.instance == "" {
		return a
	}
	a.Key = report.GatewayKey(g.scope(a.AccountID))
	a.Title = strings.Replace(a.Title, gatewayName, g.name(), 1)
	a.Body = strings.NewReplacer(
		"every account's engine is blind and cannot trade", "the "+g.instance+" account's engine is blind and cannot trade",
		"/var/lib/ti-gateway/login-failures", "/var/lib/ti-gateway-"+g.instance+"/login-failures",
		"`sudo systemctl restart ti-gateway`", "`sudo systemctl restart ti-gateway@"+g.instance+"`",
		"`journalctl -u ti-gateway`", "`journalctl -u ti-gateway@"+g.instance+"`",
		"journalctl -u ti-gateway-heal@*", "journalctl -u ti-gateway-heal@ti-adapter@"+g.instance+".service.service",
	).Replace(a.Body)
	if !strings.Contains(a.Body, "ti-gateway-heal@ti-adapter@"+g.instance+".service.service") {
		a.Body += " Read this Gateway's healer journal with `journalctl -u ti-gateway-heal@ti-adapter@" + g.instance + ".service.service` (docs/manual/server.md, Gateway healer)."
	}
	return a
}

var gatewayInstanceName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

type gatewaySelection struct {
	target  gatewayTarget
	sched   gatewaySchedule
	path    string
	units   []string
	enabled bool
}

// selectGateway gives the joint check to its named owner and each other
// reporter only its own provisioned Gateway (server.md, "The Traditional
// IRA's Gateway"). Instance settings never inherit the joint restart times.
func selectGateway(account, owner, instanceEnv, state, reporterState, adapters string, getenv func(string) string) (selected gatewaySelection, err error) {
	if owner == "" {
		return
	}
	if account == owner {
		selected.path = state
		if selected.path == "" {
			selected.path = filepath.Join(filepath.Dir(reporterState), "host-gateway.json")
		}
		selected.units = splitUnits(adapters)
		selected.sched, err = gatewayScheduleFrom(getenv)
		selected.enabled = err == nil
		return
	}
	if instanceEnv == "" {
		return
	}
	if !gatewayInstanceName.MatchString(account) {
		err = fmt.Errorf("reporter: invalid Gateway instance %q", account)
		return
	}
	raw, readErr := os.ReadFile(instanceEnv)
	if errors.Is(readErr, os.ErrNotExist) {
		return
	}
	if readErr != nil {
		err = fmt.Errorf("reporter: read Gateway instance environment: %w", readErr)
		return
	}
	values := map[string]string{}
	for n, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) == "" {
			err = fmt.Errorf("reporter: Gateway instance environment line %d must be KEY=VALUE", n+1)
			return
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, `"`) || strings.HasPrefix(value, `'`) {
			if len(value) < 2 || value[len(value)-1] != value[0] {
				err = fmt.Errorf("reporter: Gateway instance environment line %d has unmatched quotes", n+1)
				return
			}
			value = value[1 : len(value)-1]
		}
		values[strings.TrimSpace(key)] = value
	}
	selected.target = gatewayTarget{instance: account}
	selected.sched, err = gatewayScheduleFrom(func(key string) string { return values[key] })
	selected.path = filepath.Join(filepath.Dir(reporterState), "host-gateway-"+account+".json")
	selected.units = []string{"ti-adapter@" + account + ".service"}
	selected.enabled = err == nil
	return
}
