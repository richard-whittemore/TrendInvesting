package main

import (
	"bufio"
	"bytes"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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
// engine's replies. The simulated account starts holding 14 SPY on margin
// at $400 (net liquidation $4,000, 140%), and a dry-run sends nothing, so
// nothing ever fills and it holds 14 throughout: the first session sells
// down to 98% (9.8 shares, so 4 to sell) with send false, and the synthetic
// series enters a boost (0.98 x 1.5 = 1.47) and exits it, each exit sized
// from the 14 held, never net of an earlier sell that was never sent. Every
// release is followed by the engine's synthetic cancel of it (ADR 0026,
// point 3, "Mode", amended 2026-10-17 and 2026-10-22).
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
	if !strings.Contains(lines[0], "target 0.98") || !strings.Contains(lines[0], "sell 4 ") || !strings.Contains(lines[0], "released (send false) canceled (synthetic, never sent)") {
		t.Errorf("the first line should sell 4 SPY down to 98%%, released with send false and canceled as never sent: %q", lines[0])
	}
	if !strings.Contains(text, "target 1.47") {
		t.Error("the synthetic series should enter a boost")
	}
	if got, want := strings.Count(text, "canceled (synthetic, never sent)"), strings.Count(text, "released (send false)"); got != want || want == 0 {
		t.Errorf("%d synthetic cancels for %d releases; every unsent release must end canceled", got, want)
	}
	// Nothing fills, so every sale down to 98% is sized from the 14 held:
	// 14 less 0.98 x (14 x close - $1,600 borrowed) / close, toward zero. A
	// fill reaching the simulated account would change the holding, and so
	// the size of every later sale.
	sale := regexp.MustCompile(`close ([0-9.]+) .* target 0\.98 \| sell (\d+) @`)
	exits := 0
	for i, line := range lines {
		m := sale.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		closed, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			t.Fatal(err)
		}
		sold, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if want := int64(math.Trunc(14 - 0.98*(float64(14*closed)-1600)/closed)); sold != want {
			t.Errorf("line %d sells %d, want %d from the 14 held: %q", i, sold, want, line)
		}
		if i > 0 {
			exits++
		}
	}
	if exits < 2 {
		t.Errorf("%d boost exits sell, want every exit after the first to sell too, sized from the holding", exits)
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
