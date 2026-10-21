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
		!strings.Contains(string(makefile), "scripts/build-stamp.sh") {
		t.Fatalf("the Makefile does not stamp %s from scripts/build-stamp.sh", stampSymbol)
	}

	// Each child is bounded, so a guard that regresses and leaves an engine
	// serving fails this test instead of hanging it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	stamped := buildEngine(t, t.TempDir(), "-X "+stampSymbol+"=stamp-for-test")
	opts := boostOptions(t, dir)
	cmd := exec.CommandContext(ctx, stamped, "-mode", modeDryRun, "-socket", opts.socketPath, "-config", opts.configPath,
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
	out, err := exec.CommandContext(ctx, unstamped, "-mode", modeDryRun, "-socket", other.socketPath, "-config", other.configPath,
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

// TestADirtyBuildThatDoesNotIdentifyItsTreeNeverResumes: "<commit>-dirty"
// names every dirty tree at that commit alike, so two binaries built from
// different uncommitted changes would pass as one build, and a run never
// resumes across builds (ADR 0027, owner decision 2026-10-16). Such a build
// may run fresh, but -resume refuses it before anything is created.
func TestADirtyBuildThatDoesNotIdentifyItsTreeNeverResumes(t *testing.T) {
	dir := t.TempDir()
	first := boostOptions(t, dir)
	first.build = "abc1234-dirty"
	if err := startEngine(t, first)(); err != nil {
		t.Fatalf("a fresh run on a dirty build: %v", err)
	}
	next := boostOptions(t, dir)
	next.build, next.resumePath = first.build, first.logPath
	next.outPath, next.logPath = filepath.Join(dir, "next.journal"), filepath.Join(dir, "next.log")
	refuse(t, "a resume on a dirty build", next, "dirty")
}

// TestADirtyTreeIsStampedWithItsOwnTree: the stamp is `git describe
// --always --dirty`, and a dirty tree's adds the hash of its whole working
// tree, untracked files included, so two different uncommitted changes at
// one commit are two builds and a run never resumes across them (ADR 0027,
// owner decision 2026-10-16), while the same tree stamps alike every time.
func TestADirtyTreeIsStampedWithItsOwnTree(t *testing.T) {
	script, err := filepath.Abs("../../scripts/build-stamp.sh")
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	stamp := func() string {
		t.Helper()
		cmd := exec.Command(script)
		cmd.Dir = repo
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("build-stamp.sh: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("a.txt", "one\n")
	git("add", "a.txt")
	git("commit", "-q", "-m", "one")
	clean := git("describe", "--always", "--dirty")
	if got := stamp(); got != clean {
		t.Fatalf("a clean tree is stamped %q, want %q", got, clean)
	}
	write("a.txt", "two\n")
	first := stamp()
	if !strings.HasPrefix(first, clean+"-dirty-") || first == clean+"-dirty-" {
		t.Fatalf("a dirty tree is stamped %q, want %s-dirty-<tree>", first, clean)
	}
	if again := stamp(); again != first {
		t.Errorf("one dirty tree is stamped %q, then %q", first, again)
	}
	write("a.txt", "three\n")
	if second := stamp(); second == first {
		t.Errorf("two different dirty trees are both stamped %q", first)
	}
	write("a.txt", "two\n")
	write("b.txt", "untracked\n")
	if untracked := stamp(); untracked == first {
		t.Errorf("an untracked file leaves the stamp at %q", first)
	}
	if status := git("status", "--porcelain"); status != "M a.txt\n?? b.txt" && status != " M a.txt\n?? b.txt" {
		t.Errorf("stamping changed the index or tree: status %q", status)
	}
}

// TestAnUntrackedOnlyChangeIsStampedDirty: `git describe --dirty` ignores
// untracked files, but `go build` compiles an untracked Go file, so a tree
// whose only change is an untracked file is a different build from its
// commit and is stamped with its own tree hash; deploy/build-engine.sh then
// refuses it as dirty. Otherwise two binaries share a clean stamp and a run
// resumes across builds (ADR 0027, owner decision 2026-10-16). An ignored
// file is not part of the build and leaves the stamp clean.
func TestAnUntrackedOnlyChangeIsStampedDirty(t *testing.T) {
	script, err := filepath.Abs("../../scripts/build-stamp.sh")
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	stamp := func() string {
		t.Helper()
		cmd := exec.Command(script)
		cmd.Dir = repo
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("build-stamp.sh: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("a.go", "package a\n")
	write(".gitignore", "ignored.txt\n")
	git("add", "a.go", ".gitignore")
	git("commit", "-q", "-m", "one")
	clean := git("describe", "--always", "--dirty")
	write("ignored.txt", "not built\n")
	if got := stamp(); got != clean {
		t.Errorf("an ignored file stamps the tree %q, want the clean %q", got, clean)
	}
	write("b.go", "package a\n")
	first := stamp()
	if !strings.HasPrefix(first, clean+"-dirty-") || first == clean+"-dirty-" {
		t.Fatalf("a tree whose only change is an untracked file is stamped %q, want %s-dirty-<tree>", first, clean)
	}
	write("b.go", "package a\n\nconst B = 1\n")
	if second := stamp(); second == first {
		t.Errorf("two different untracked-only trees are both stamped %q", first)
	}
}
