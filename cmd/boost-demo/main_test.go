package main

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const demoConfig = "../../deploy/etc/engines/boost.config"

// startEngine builds cmd/engine stamped, as the Makefile does, starts it in
// dry-run with a durable log, and returns the log's path and a stop function
// that sends SIGTERM and waits.
func startEngine(t *testing.T) (socket, log string, stop func()) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ti-engine")
	build := exec.Command("go", "build", "-o", bin,
		"-ldflags", "-X github.com/richard-whittemore/TrendInvesting/internal/buildinfo.Version=demo-test", "../engine")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the engine: %v\n%s", err, out)
	}
	socketDir, err := os.MkdirTemp("/tmp", "demo-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	dir := t.TempDir()
	socket, log = filepath.Join(socketDir, "engine.sock"), filepath.Join(dir, "run.log")
	engine := exec.Command(bin, "-mode", "dry-run", "-config", demoConfig, "-socket", socket,
		"-log", log, "-out", filepath.Join(dir, "run.journal"), "-as-of", "2024-01-02T21:00:00Z")
	stdout, err := engine.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || !strings.Contains(line, "listening on") {
		_ = engine.Process.Kill()
		t.Fatalf("the engine did not start: %q %v", line, err)
	}
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		_ = engine.Process.Signal(syscall.SIGTERM)
		if err := engine.Wait(); err != nil {
			t.Errorf("the engine did not stop cleanly: %v", err)
		}
	}
	t.Cleanup(stop)
	return socket, log, stop
}

// TestTheDemoDrivesTheRealEngineInDryRun: the demo plays the adapter for the
// one engine binary, over its socket, and prints the decision log from the
// engine's replies: the first session buys 98% of $4,000 at $400 (9.8
// shares, so 9) with send false, and the synthetic series enters a boost
// (0.98 x 1.5 = 1.47) and exits it.
func TestTheDemoDrivesTheRealEngineInDryRun(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the engine binary")
	}
	socket, log, stop := startEngine(t)
	var out bytes.Buffer
	if err := run([]string{"-socket", socket, "-log", log, "-config", demoConfig, "-sessions", "320"}, &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	stop()
	text := out.String()
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if got := strings.Count(text, "decided"); got != 320 {
		t.Errorf("%d decided sessions, want 320", got)
	}
	if !strings.Contains(lines[0], "target 0.98") || !strings.Contains(lines[0], "buy 9 ") || !strings.Contains(lines[0], "released (send false)") {
		t.Errorf("the first line should buy 9 SPY, 98%% from cash, released with send false: %q", lines[0])
	}
	if !strings.Contains(text, "target 1.47") || !strings.Contains(text, "sell") {
		t.Error("the synthetic series should enter and exit a boost")
	}
	if strings.Contains(text, "send true") {
		t.Error("a dry-run released an order to send")
	}
}

func TestDemoRefusesBadArguments(t *testing.T) {
	for _, args := range [][]string{
		{"-sessions", "0", "-socket", "s", "-log", "l", "-config", demoConfig},
		{"-nope"},
		{"-socket", "s", "-config", demoConfig},
		{"-socket", "s", "-log", filepath.Join(t.TempDir(), "missing.log"), "-config", demoConfig},
	} {
		if err := run(args, &bytes.Buffer{}); err == nil {
			t.Errorf("run(%v) succeeded", args)
		}
	}
}
