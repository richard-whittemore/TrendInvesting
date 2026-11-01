// gateway-heal composes the recovery policy with systemd and durable files
// (docs/manual/server.md, Gateway healer). Only systemd names receive actions.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/gatewayheal"
)

var clockForRun gatewayheal.Clock = wallClock{}

type wallClock struct{}

func (wallClock) Now() time.Time             { return time.Now() }
func (wallClock) Wait(d time.Duration) error { time.Sleep(d); return nil }

func main() {
	unit := flag.String("adapter", "", "failed adapter unit name")
	flag.Parse()
	if err := run(*unit); err != nil {
		fmt.Fprintln(os.Stderr, "gateway-heal: row=evidence-error step=stop:", err)
		os.Exit(1)
	}
}
func run(unit string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("root is required")
	}
	store := diskState{dir: "/var/lib/ti-gateway/heal", uid: 0}
	unlock, err := store.lock()
	if err != nil {
		return fmt.Errorf("healer serialization: %w", err)
	}
	defer unlock()
	ctl := systemd{run: func(name string, args ...string) ([]byte, error) {
		var cmd *exec.Cmd
		switch name {
		case "systemctl":
			//nolint:gosec // G204: fixed binary; Run fixes query units and ValidateAction allowlists mutations (server.md, Gateway healer); no shell.
			cmd = exec.Command("systemctl", args...)
		case "journalctl":
			//nolint:gosec // G204: fixed binary and query switches; unit/invocation are journal match values (server.md, Gateway healer); no shell.
			cmd = exec.Command("journalctl", args...)
		default:
			return nil, fmt.Errorf("unsupported executable")
		}
		cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC", "SYSTEMD_COLORS=0")
		return cmd.Output()
	}}
	log := func(s string) error { _, err := fmt.Fprintln(os.Stdout, s); return err }
	return gatewayheal.Run(unit, clockForRun, files{schedule: "/etc/trend-investing/ibkr-maintenance.json", counter: "/var/lib/ti-gateway/login-failures"}, store, ctl, ctl, relayGuard{root: "/var/lib/trend-investing/adapters"}, log)
}
