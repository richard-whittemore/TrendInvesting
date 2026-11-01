// gateway-heal composes the recovery policy with systemd and durable files
// (docs/manual/server.md, Gateway healer). Only systemd names receive actions.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
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
	store := diskState{dir: "/var/lib/ti-gateway-heal", uid: 0}
	runner := &commandRunner{now: clockForRun.Now, execute: executeCommand}
	ctl := systemd{run: runner.run, setDeadline: func(at time.Time) { runner.deadline = at }}
	log := func(s string) error { _, err := fmt.Fprintln(os.Stdout, s); return err }
	return gatewayheal.Run(unit, clockForRun, files{schedule: "/etc/trend-investing/ibkr-maintenance.json", counter: "/var/lib/ti-gateway/login-failures", gatewayEnv: "/etc/trend-investing/gateway.env"}, store, ctl, ctl, relayGuard{root: "/var/lib/trend-investing/adapters"}, log)
}

// commandRunner bounds reads, including pipe drain, by remaining recovery time.
// Mutations wait for systemd's job result without killing the client, so locks
// outlive every submitted job (server.md, Gateway healer).
type commandRunner struct {
	now      func() time.Time
	deadline time.Time
	execute  func(context.Context, string, []string, io.Writer) error
}

func (r *commandRunner) run(name string, args ...string) ([]byte, error) {
	bound := 30 * time.Second
	if !r.deadline.IsZero() {
		bound = min(bound, r.deadline.Sub(r.now()))
	}
	if bound <= 0 {
		return nil, context.DeadlineExceeded
	}
	ctx := context.Background()
	if name != "systemctl" || (!containsArg(args, "restart") && !containsArg(args, "start")) {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, bound/2)
		defer cancel()
	}
	var out io.Writer
	reduced := &journalReduction{}
	bounded := &boundedOutput{}
	if name == "journalctl" && containsArg(args, "cat") {
		out = reduced
	} else {
		out = bounded
	}
	if err := r.execute(ctx, name, args, out); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if out == reduced {
		return reduced.result(), nil
	}
	return bounded.buffer.Bytes(), nil
}
func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
func executeCommand(ctx context.Context, name string, args []string, out io.Writer) error {
	var cmd *exec.Cmd
	switch name {
	case "systemctl":
		//nolint:gosec // G204: fixed binary; ValidateAction allowlists mutations (server.md, Gateway healer); no shell.
		cmd = exec.CommandContext(ctx, "systemctl", args...)
	case "journalctl":
		//nolint:gosec // G204: fixed binary and query switches; unit and invocation are journal match values (server.md, Gateway healer).
		cmd = exec.CommandContext(ctx, "journalctl", args...)
	default:
		return fmt.Errorf("unsupported executable")
	}
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC", "SYSTEMD_COLORS=0")
	cmd.Stdout = out
	diagnostic := &boundedStderr{}
	cmd.Stderr = diagnostic
	// The command context reserves half the total bound for pipe drain.
	// WaitDelay cannot extend the total command budget (server.md, Gateway healer).
	if deadline, bounded := ctx.Deadline(); bounded {
		cmd.WaitDelay = min(time.Second, time.Until(deadline))
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(diagnostic.buffer.String()))
	}
	return nil
}

// boundedStderr retains at most 4 KiB while draining all diagnostic output
// so command failures remain diagnosable (server.md, Gateway healer).
type boundedStderr struct{ buffer bytes.Buffer }

func (w *boundedStderr) Write(p []byte) (int, error) {
	n := min(len(p), 4096-w.buffer.Len())
	_, _ = w.buffer.Write(p[:n])
	return len(p), nil
}

// boundedOutput refuses oversized command properties or timestamped loss
// evidence instead of trusting truncated input (docs/development.md, fail closed).
type boundedOutput struct{ buffer bytes.Buffer }

func (w *boundedOutput) Write(p []byte) (int, error) {
	if w.buffer.Len()+len(p) > 1024*1024 {
		return 0, errors.New("command evidence exceeds bound")
	}
	return w.buffer.Write(p)
}

// journalReduction streams complete invocation evidence with bounded memory.
// Only the latest login transition, refusal evidence and exact remedy affect
// recovery (ADR 0019, Alerting; server.md, Gateway healer).
type journalReduction struct {
	pending                []byte
	login, problem, remedy string
}

func (w *journalReduction) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			end = len(p)
		} else {
			end++
		}
		if len(w.pending)+end > 1024*1024 {
			return 0, errors.New("journal line exceeds bound")
		}
		w.pending = append(w.pending, p[:end]...)
		p = p[end:]
		if w.pending[len(w.pending)-1] == '\n' {
			w.line(string(w.pending))
			w.pending = w.pending[:0]
		}
	}
	return n, nil
}
func (w *journalReduction) line(line string) {
	if strings.Contains(line, "Login has completed") || strings.Contains(line, "LOGGED_OUT") || strings.Contains(line, "Unrecognized Username or Password") || strings.Contains(line, "gateway-start: starting IBC") || strings.Contains(line, "gateway-start: cannot start IBC") {
		w.login = strings.TrimSpace(line)
	}
	if strings.Contains(line, "login retries stopped") || strings.Contains(line, "gateway-start: reset wait:") || strings.Contains(line, "Unrecognized Username or Password") {
		w.problem = strings.TrimSpace(line)
	}
	if strings.TrimSpace(line) == gatewayheal.Remedy {
		w.remedy = gatewayheal.Remedy
	}
}
func (w *journalReduction) result() []byte {
	if len(w.pending) > 0 {
		w.line(string(w.pending))
		w.pending = nil
	}
	return []byte(strings.Join([]string{w.login, w.problem, w.remedy}, "\n"))
}
