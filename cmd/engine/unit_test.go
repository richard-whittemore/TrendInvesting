package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

// unitDirective is one Key=Value line of a unit file and the section it is in.
type unitDirective struct{ section, key, value string }

// unitDirectives returns every Key=Value directive of a unit file, in order,
// each with its section ("[Service]").
func unitDirectives(t *testing.T, path string) []unitDirective {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []unitDirective
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
		out = append(out, unitDirective{section, key, value})
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
func expandExecStart(line string, specifiers, env map[string]string) []string {
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
		if d.section != "[Service]" {
			if _, ok := want[d.key]; ok || d.key == "ExecStart" || d.key == "Environment" {
				t.Errorf("%s=%s is in %s, not [Service], where systemd reads it", d.key, d.value, d.section)
			}
			continue
		}
		switch d.key {
		case "Restart", "RuntimeDirectory", "RuntimeDirectoryMode":
			if d.value != want[d.key] {
				t.Errorf("%s=%s, want %s", d.key, d.value, want[d.key])
			}
			delete(want, d.key)
		case "Environment":
			key, value, _ := strings.Cut(d.value, "=")
			env[key] = value
		case "ExecStart":
			execStart = d.value
		}
	}
	for key := range want {
		t.Errorf("the unit's [Service] sets no %s", key)
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
	root := t.TempDir()
	state, flexDir := filepath.Join(root, "engines", "boost"), filepath.Join(root, "flex", "boost")
	for _, d := range []string{state, flexDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if env["TI_FLEX_DIR"] != "/var/lib/trend-investing/flex/%i" {
		t.Errorf("the unit's Flex folder is %q, want the instance's, where the provisioner creates it", env["TI_FLEX_DIR"])
	}
	env["TI_CONFIG"] = boostDeployCfg
	env["TI_STATE_DIR"] = state
	env["TI_FLEX_DIR"] = flexDir
	socketDir := shortSocketDir(t)
	args := expandExecStart(execStart, map[string]string{"%i": "boost", "%t": socketDir}, env)
	if len(args) == 0 || args[0] != "/opt/trend-investing/bin/ti-engine" {
		t.Fatalf("ExecStart runs %v, want /opt/trend-investing/bin/ti-engine", args)
	}
	for flag, value := range map[string]string{
		"-mode":   env["TI_MODE"],
		"-config": boostDeployCfg,
		"-socket": socketDir + "/ti-engine-boost/engine.sock",
		"-log":    state + "/" + env["TI_RUN"] + ".log",
		"-out":    state + "/" + env["TI_RUN"] + ".journal",
		"-as-of":  env["TI_AS_OF"],
		// The instance's Flex ledger (ADR 0019, amended 2026-10-21).
		"-cash-ledger": flexDir + "/cash-movements.jsonl",
	} {
		if i := indexOf(args, flag); i < 0 || i+1 >= len(args) || args[i+1] != value {
			t.Errorf("ExecStart %v does not pass %s %s", args, flag, value)
		}
	}
	// The unit passes the account's operator inbox (ADR 0026, kill-switch
	// amendment), in the state directory, where the provisioner creates it.
	inbox := filepath.Join(state, "operator")
	if i := indexOf(args, "-operator"); i < 0 || i+1 >= len(args) || args[i+1] != inbox {
		t.Fatalf("ExecStart %v does not pass -operator %s", args, inbox)
	}
	makeOperatorInbox(t, inbox)
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

// TestTheProvisionerLeavesNoEngineEnabled: every engine start is the owner's
// explicit step, a crash resumed and anything else an approved fresh run
// (ADR 0027, the restart model), so no engine starts at boot. A server an
// earlier provisioner enabled engines on keeps those links until they are
// removed, so the provisioner disables every engine instance as well as
// enabling none.
func TestTheProvisionerLeavesNoEngineEnabled(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/provision.sh")
	if err != nil {
		t.Fatal(err)
	}
	disabled := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "systemctl" {
			continue
		}
		for _, f := range fields[2:] {
			if !strings.HasPrefix(f, "ti-engine@") {
				continue
			}
			switch fields[1] {
			case "enable":
				t.Errorf("the provisioner enables %s: %q", f, strings.TrimSpace(line))
			case "disable":
				disabled[f] = true
			}
		}
	}
	for _, account := range []string{"boost", "trad-ira", "roth-ira"} {
		if unit := "ti-engine@" + account + ".service"; !disabled[unit] {
			t.Errorf("the provisioner does not disable %s, which an earlier provisioner enabled", unit)
		}
	}
}

// TestEverySystemctlArgumentIsAUnit: each word after a provisioner's
// `systemctl <verb>` is a unit name or a flag. A stray word is read as a unit
// that does not exist, the command fails, and `set -e` stops provisioning
// before the firewall step (deploy/provision.sh). A unit is named with its
// suffix, except the system daemons below, which systemd names as services.
func TestEverySystemctlArgumentIsAUnit(t *testing.T) {
	systemDaemons := map[string]bool{"tailscaled": true}
	raw, err := os.ReadFile("../../deploy/provision.sh")
	if err != nil {
		t.Fatal(err)
	}
	for n, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "systemctl" || fields[1] == "daemon-reload" {
			continue
		}
		for _, f := range fields[2:] {
			if f == "||" || f == "&&" || f == ";" {
				break // the shell's own operator ends the command's arguments
			}
			if strings.HasPrefix(f, "-") || strings.HasSuffix(f, ".service") || strings.HasSuffix(f, ".timer") || systemDaemons[f] {
				continue
			}
			t.Errorf("deploy/provision.sh:%d passes %q to systemctl %s as a unit: %q", n+1, f, fields[1], strings.TrimSpace(line))
		}
	}
}

// TestTheProvisionerCreatesTheOperatorInbox: the instruction's writer is a
// user of its own, ti-operator, with no login shell, in the engine's group
// so it can reach the account's state directory; each account's operator
// inbox is created by the services step inside that directory, owned by
// ti-operator and the engine's group ti-trader, mode 0750, so the engine
// reads it through its group and cannot write it, and nobody else can
// (ADR 0026, kill-switch amendment; docs/manual/operator.md, "Who can write
// an instruction").
func TestTheProvisionerCreatesTheOperatorInbox(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/provision.sh")
	if err != nil {
		t.Fatal(err)
	}
	lines := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		lines[strings.TrimSpace(line)] = true
	}
	for _, want := range []string{
		`useradd --system --gid ti-trader --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin ti-operator`,
		`install -d -o ti-operator -g ti-trader -m 0750 "/var/lib/trend-investing/engines/$a/operator"`,
	} {
		if !lines[want] {
			t.Errorf("deploy/provision.sh does not run: %s", want)
		}
	}
}

// TestNoNetworkFacingUnitCanWriteAnInstruction: a unit that reads IB
// Gateway's data, Massive's answers or an HTTP request runs as ti-trader, the
// engine's user, so a defect in it must not reach the inbox or the engine's
// notes beside it: each is sandboxed (ProtectSystem=strict) with no writable
// path under the engines' state, and each account's inbox is inaccessible
// to it. The engine itself reads its own inbox read-only (ADR 0026,
// kill-switch amendment; docs/manual/server.md).
func TestNoNetworkFacingUnitCanWriteAnInstruction(t *testing.T) {
	const engines = "/var/lib/trend-investing/engines"
	for _, unit := range []string{"ti-adapter-boost.service", "ti-data.service", "ti-reporter@.service", "ti-dashboard.service"} {
		values := map[string][]string{}
		for _, d := range unitDirectives(t, "../../deploy/systemd/"+unit) {
			if d.section == "[Service]" {
				values[d.key] = append(values[d.key], strings.Fields(d.value)...)
			}
		}
		if !slices.Equal(values["ProtectSystem"], []string{"strict"}) {
			t.Errorf("%s: ProtectSystem=%v, want strict", unit, values["ProtectSystem"])
		}
		for _, path := range values["ReadWritePaths"] {
			path = strings.TrimLeft(path, "-+")
			if path == engines || strings.HasPrefix(path, engines+"/") || strings.HasPrefix(engines, path+"/") {
				t.Errorf("%s may write %s, which holds the operator inboxes", unit, path)
			}
		}
		for _, account := range []string{"boost", "trad-ira", "roth-ira"} {
			if want := "-" + engines + "/" + account + "/operator"; !slices.Contains(values["InaccessiblePaths"], want) {
				t.Errorf("%s: InaccessiblePaths=%v does not name %s", unit, values["InaccessiblePaths"], want)
			}
		}
	}
	var readOnly []string
	for _, d := range unitDirectives(t, unitPath) {
		if d.section == "[Service]" && d.key == "ReadOnlyPaths" {
			readOnly = append(readOnly, strings.Fields(d.value)...)
		}
	}
	if want := "-" + engines + "/%i/operator"; !slices.Contains(readOnly, want) {
		t.Errorf("the engine unit's ReadOnlyPaths=%v does not name %s", readOnly, want)
	}
}
