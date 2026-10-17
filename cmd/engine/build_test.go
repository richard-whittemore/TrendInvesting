package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// TestAnUnstampedBuildRefusesEveryModeButBacktest: the build is part of every
// envelope's strategy version (ADR 0016), and "dev" is what an unstamped
// build reports, so two different unstamped builds would claim to be one and
// the rule that a run never resumes across builds (ADR 0027) could not fire.
// A backtest may still run on one.
func TestAnUnstampedBuildRefusesEveryModeButBacktest(t *testing.T) {
	for _, mode := range []string{modeDryRun, modePaper, modeLive} {
		opts := boostOptions(t, t.TempDir())
		opts.mode, opts.build = mode, "dev"
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := run(ctx, opts, io.Discard)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "unstamped build") || !strings.Contains(err.Error(), "make engine") {
			t.Errorf("-mode %s on build dev: run() = %v, want a refusal naming the unstamped build and how to stamp one", mode, err)
		}
	}
	dir := t.TempDir()
	opts := options{mode: modeBacktest, socketPath: filepath.Join(shortSocketDir(t), "engine.sock"), configPath: testConfigPath,
		asOf: testAsOf, outPath: filepath.Join(dir, "run.jsonl"), build: "dev"}
	if err := startEngine(t, opts)(); err != nil {
		t.Fatalf("a backtest on build dev: %v", err)
	}
}

// stampSymbol is the variable the Makefile's LDFLAGS must set.
const stampSymbol = "github.com/richard-whittemore/TrendInvesting/internal/buildinfo.Version"

// buildEngine builds this command into dir with the given -ldflags (none
// when empty) and returns the binary's path.
func buildEngine(t *testing.T, dir, ldflags string) string {
	t.Helper()
	binary := filepath.Join(dir, "ti-engine")
	args := []string{"build", "-o", binary}
	if ldflags != "" {
		args = append(args, "-ldflags", ldflags)
	}
	cmd := exec.Command("go", append(args, ".")...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return binary
}

// TestTheMakefileStampsTheBuildTheEngineRecords: the Makefile stamps the
// build into internal/buildinfo, a binary built that way records the stamp
// in its durable log's header, and a binary built without it refuses to
// start in dry-run. Both are the real executable, started as the service
// file starts it.
func TestTheMakefileStampsTheBuildTheEngineRecords(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the engine binary")
	}
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(makefile), "-X "+stampSymbol+"=$(BUILD_STAMP)") ||
		!strings.Contains(string(makefile), "git describe --always --dirty") {
		t.Fatalf("the Makefile does not stamp %s from git describe --always --dirty", stampSymbol)
	}

	dir := t.TempDir()
	stamped := buildEngine(t, t.TempDir(), "-X "+stampSymbol+"=stamp-for-test")
	opts := boostOptions(t, dir)
	cmd := exec.Command(stamped, "-mode", modeDryRun, "-socket", opts.socketPath, "-config", opts.configPath,
		"-as-of", opts.asOf, "-out", opts.outPath, "-log", opts.logPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || !strings.Contains(line, "listening on") {
		_ = cmd.Process.Kill()
		t.Fatalf("the stamped engine did not start: %q %v", line, err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the stamped engine did not stop cleanly: %v", err)
	}
	header := recoverFile(t, opts.logPath).Header
	if want := event.ComposeStrategyVersion("boost", targetweight.RulesVersion, "stamp-for-test"); header.StrategyVersion != want {
		t.Errorf("the durable log's header records %q, want %q", header.StrategyVersion, want)
	}

	unstamped := buildEngine(t, t.TempDir(), "")
	other := boostOptions(t, t.TempDir())
	out, err := exec.Command(unstamped, "-mode", modeDryRun, "-socket", other.socketPath, "-config", other.configPath,
		"-as-of", other.asOf, "-out", other.outPath, "-log", other.logPath).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "unstamped build") {
		t.Errorf("the unstamped engine in dry-run: %v\n%s\nwant it to refuse to start", err, out)
	}
}

// TestABoostRunDoesNotResumeAcrossBuilds: a dry-run Boost log recorded by
// one stamped build is refused by another, with the upgrade procedure named
// (ADR 0027's amendment of 2026-10-16), and nothing new is created.
func TestABoostRunDoesNotResumeAcrossBuilds(t *testing.T) {
	dir := t.TempDir()
	first := boostOptions(t, dir)
	first.build = "build-1"
	if err := startEngine(t, first)(); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	next := boostOptions(t, dir)
	next.build, next.resumePath = "build-2", first.logPath
	next.outPath, next.logPath = filepath.Join(dir, "next.journal"), filepath.Join(dir, "next.log")
	err := run(context.Background(), next, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "does not resume across builds") || !strings.Contains(err.Error(), "build-1") {
		t.Fatalf("resume on another build: run() = %v, want the cross-build refusal", err)
	}
	if _, statErr := os.Stat(next.logPath); !os.IsNotExist(statErr) {
		t.Error("the refused resume left a durable log behind")
	}
}
