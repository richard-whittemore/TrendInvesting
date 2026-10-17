package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The engine's service file and the settings it reads, as provisioned.
const (
	unitPath       = "../../deploy/systemd/ti-engine@.service"
	boostEnvPath   = "../../deploy/etc/engines/boost.env"
	runEnvExample  = "../../deploy/etc/engines/run.env.example"
	boostDeployCfg = "../../deploy/etc/engines/boost.config"
)

// readAssignments reads KEY=VALUE lines, skipping comments and blanks.
func readAssignments(t *testing.T, path string) map[string]string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("%s: %q is not KEY=VALUE", path, line)
		}
		values[key] = value
	}
	return values
}

// unitDirectives returns every Key=Value directive of a unit file, in order.
func unitDirectives(t *testing.T, path string) [][2]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out [][2]string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		out = append(out, [2]string{key, value})
	}
	return out
}

var (
	bracedVariable = regexp.MustCompile(`\$\{([A-Z_]+)\}`)
	bareVariable   = regexp.MustCompile(`^\$([A-Z_]+)$`)
)

// expandExecStart expands an ExecStart command line as systemd does
// (systemd.service(5), "Command lines"): %i and %t specifiers; ${NAME} is
// replaced by its value within a word; a word that is exactly $NAME becomes
// the value split at whitespace, zero or more words.
func expandExecStart(line string, specifiers map[string]string, env map[string]string) []string {
	for spec, value := range specifiers {
		line = strings.ReplaceAll(line, spec, value)
	}
	var words []string
	for _, word := range strings.Fields(line) {
		if m := bareVariable.FindStringSubmatch(word); m != nil {
			words = append(words, strings.Fields(env[m[1]])...)
			continue
		}
		words = append(words, bracedVariable.ReplaceAllStringFunc(word, func(v string) string {
			return env[bracedVariable.FindStringSubmatch(v)[1]]
		}))
	}
	return words
}

// TestTheServiceFileStartsTheEngineWithExplicitSettings: the engine's unit
// never restarts by itself (a restart is the owner's step: a crash resumes,
// anything else is an approved fresh run), gives each engine a private 0700
// runtime directory for its socket, and starts the engine with explicit
// settings that the engine accepts: the provisioned Boost settings start a
// dry-run engine with its durable log.
func TestTheServiceFileStartsTheEngineWithExplicitSettings(t *testing.T) {
	directives := unitDirectives(t, unitPath)
	want := map[string]string{"Restart": "no", "RuntimeDirectory": "ti-engine-%i", "RuntimeDirectoryMode": "0700"}
	env := map[string]string{}
	var execStart string
	for _, d := range directives {
		switch d[0] {
		case "Restart", "RuntimeDirectory", "RuntimeDirectoryMode":
			if d[1] != want[d[0]] {
				t.Errorf("%s=%s, want %s", d[0], d[1], want[d[0]])
			}
			delete(want, d[0])
		case "Environment":
			key, value, _ := strings.Cut(d[1], "=")
			env[key] = value
		case "ExecStart":
			execStart = d[1]
		}
	}
	for key := range want {
		t.Errorf("the unit sets no %s", key)
	}

	for key, value := range readAssignments(t, boostEnvPath) {
		env[key] = value
	}
	for key, value := range readAssignments(t, runEnvExample) {
		env[key] = value
	}
	if env["TI_MODE"] != modeDryRun {
		t.Errorf("the Boost engine's settings declare TI_MODE=%q, want dry-run until the paper gate", env["TI_MODE"])
	}
	// The server's paths, moved to this test's directories.
	state := t.TempDir()
	env["TI_CONFIG"] = boostDeployCfg
	env["TI_STATE_DIR"] = state
	args := expandExecStart(execStart, map[string]string{"%i": "boost", "%t": shortSocketDir(t)}, env)
	if len(args) == 0 || args[0] != "/opt/trend-investing/bin/ti-engine" {
		t.Fatalf("ExecStart runs %v, want /opt/trend-investing/bin/ti-engine", args)
	}
	for _, flag := range []string{"-mode", "-config", "-socket", "-log", "-out", "-as-of"} {
		found := false
		for _, a := range args {
			found = found || a == flag
		}
		if !found {
			t.Errorf("ExecStart %v does not pass %s", args, flag)
		}
	}
	runtimeDir := filepath.Dir(args[indexOf(args, "-socket")+1])
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	stop := startEngineInvocation(t, func(ctx context.Context, out io.Writer) error {
		return parseAndRunWithBuild(ctx, args[1:], out, "test-build")
	})
	if err := stop(); err != nil {
		t.Fatalf("the engine as the unit starts it: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, env["TI_RUN"]+".log")); err != nil {
		t.Errorf("the unit's engine wrote no durable log in its state directory: %v", err)
	}
}

func indexOf(words []string, word string) int {
	for i, w := range words {
		if w == word {
			return i
		}
	}
	return -1
}
