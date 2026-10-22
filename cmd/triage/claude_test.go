package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeClaude writes a stand-in claude program that records its arguments,
// environment, working folder and input, and prints answer; it makes no
// model call.
func fakeClaude(t *testing.T, answer string, exit int) (path, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "record")
	path = filepath.Join(dir, "claude")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + record + ".args'\n" +
		"env > '" + record + ".env'\n" +
		"pwd > '" + record + ".dir'\n" +
		"cat > '" + record + ".stdin'\n" +
		"cat <<'EOF'\n" + answer + "\nEOF\n" +
		"exit " + string(rune('0'+exit)) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, record
}

func readRecord(t *testing.T, record, part string) string {
	t.Helper()
	raw, err := os.ReadFile(record + "." + part)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

const goodAnswer = `{"type":"result","subtype":"success","is_error":false,"result":"done","structured_output":{"kind":"owner-action","what_happened":"The engine halted.","rule":"ADR 0019","owner_action":"Acknowledge it.","fix_title":"","fix_summary":""}}`

func requestIn(t *testing.T) analysisRequest {
	t.Helper()
	return analysisRequest{Issue: issue{Number: 41, Title: "boost: halted", Body: alertBody}, Slice: "## boost.log: journal-query refusals", Dir: t.TempDir()}
}

// TestTheClaudeSessionIsHeadlessWithItsToolsRestricted: the session runs
// with a fixed, explicit list of allowed tools and everything else denied,
// no MCP server, no settings of the user's, and an environment holding no
// GitHub token, no SSH agent and no git credential helper.
func TestTheClaudeSessionIsHeadlessWithItsToolsRestricted(t *testing.T) {
	path, record := fakeClaude(t, goodAnswer, 0)
	environ := []string{"PATH=" + os.Getenv("PATH"), "HOME=/Users/ti-triage", "TI_TRIAGE_GITHUB_TOKEN=ghp_x", "GH_TOKEN=ghp_y", "GITHUB_TOKEN=ghp_z", "SSH_AUTH_SOCK=/tmp/agent", "AWS_SECRET_ACCESS_KEY=k"}
	c := claudeCLI{path: path, timeout: time.Minute, env: sessionEnv(environ, "oauth-token-value"), maxTurns: 40}
	req := requestIn(t)
	a, err := c.Analyse(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != kindOwner || a.OwnerAction != "Acknowledge it." {
		t.Fatalf("analysis %+v", a)
	}
	args := strings.Split(strings.TrimSpace(readRecord(t, record, "args")), "\n")
	for _, want := range []string{"-p", "--restricted", "--strict-mcp-config", "--no-session-persistence", "--disable-slash-commands"} {
		if !slices.Contains(args, want) {
			t.Errorf("args lack %s: %q", want, args)
		}
	}
	after := func(flag string) string {
		i := slices.Index(args, flag)
		if i < 0 || i+1 >= len(args) {
			t.Fatalf("no %s in %q", flag, args)
		}
		return args[i+1]
	}
	if after("--permission-mode") != "dontAsk" || after("--permission-prompts") != "none" || after("--tools") != "Read,Grep,Glob,Edit,Write,Bash" || after("--max-turns") != "40" {
		t.Errorf("args %q", args)
	}
	allowed := args[slices.Index(args, "--allowedTools")+1 : slices.Index(args, "--disallowedTools")]
	if !slices.Equal(allowed, allowedTools) {
		t.Errorf("allowed tools %q", allowed)
	}
	for _, tool := range allowed {
		if strings.Contains(tool, "push") || strings.Contains(tool, "commit") || strings.HasPrefix(tool, "Bash(gh") || tool == "Bash" {
			t.Errorf("allowed tool %q writes beyond the worktree", tool)
		}
	}
	denied := strings.Join(args[slices.Index(args, "--disallowedTools")+1:], " ")
	for _, want := range []string{"WebFetch", "WebSearch", "Bash(git push *)", "Bash(git commit *)", "Bash(gh *)", "Bash(ssh *)", "Bash(curl *)", "Bash(tailscale *)", "mcp__*"} {
		if !strings.Contains(denied, want) {
			t.Errorf("not denied: %s", want)
		}
	}
	for _, a := range args {
		if strings.Contains(a, "dangerously") || strings.Contains(a, "bypassPermissions") {
			t.Errorf("args hold %q", a)
		}
	}
	env := readRecord(t, record, "env")
	for _, leak := range []string{"TI_TRIAGE_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN", "SSH_AUTH_SOCK", "AWS_SECRET_ACCESS_KEY", "ghp_"} {
		if strings.Contains(env, leak) {
			t.Errorf("the session's environment holds %s", leak)
		}
	}
	for _, want := range []string{"CLAUDE_CODE_OAUTH_TOKEN=oauth-token-value", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GH_CONFIG_DIR="} {
		if !strings.Contains(env, want) {
			t.Errorf("the session's environment lacks %s", want)
		}
	}
	if dir := strings.TrimSpace(readRecord(t, record, "dir")); filepath.Base(dir) != filepath.Base(req.Dir) {
		t.Errorf("ran in %q, want the worktree %q", dir, req.Dir)
	}
	stdin := readRecord(t, record, "stdin")
	for _, want := range []string{"never perform an owner action", "boost: halted", alertBody, "## boost.log: journal-query refusals"} {
		if !strings.Contains(stdin, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
}

func TestASessionThatFailsIsAnError(t *testing.T) {
	for name, c := range map[string]struct {
		answer string
		exit   int
	}{
		"exit":          {goodAnswer, 1},
		"is_error":      {`{"type":"result","subtype":"error_max_turns","is_error":true}`, 0},
		"no structured": {`{"type":"result","subtype":"success","is_error":false,"result":"hi"}`, 0},
		"not json":      {`hello`, 0},
	} {
		t.Run(name, func(t *testing.T) {
			path, _ := fakeClaude(t, c.answer, c.exit)
			cl := claudeCLI{path: path, timeout: time.Minute, env: sessionEnv([]string{"PATH=" + os.Getenv("PATH")}, ""), maxTurns: 5}
			if _, err := cl.Analyse(context.Background(), requestIn(t)); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

func TestASessionPastItsTimeIsStopped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cl := claudeCLI{path: path, timeout: 100 * time.Millisecond, env: sessionEnv([]string{"PATH=" + os.Getenv("PATH")}, ""), maxTurns: 5}
	start := time.Now()
	if _, err := cl.Analyse(context.Background(), requestIn(t)); err == nil {
		t.Fatal("no error")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the session was not stopped at its time")
	}
}

func TestSessionEnvKeepsOnlyTheNamesItNeeds(t *testing.T) {
	env := sessionEnv([]string{"PATH=/bin", "GOPATH=/go", "LANG=C", "RANDOM_SECRET=x", "malformed"}, "")
	for _, e := range env {
		if strings.HasPrefix(e, "RANDOM_SECRET") || e == "malformed" || strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN") {
			t.Errorf("kept %q", e)
		}
	}
	if !slices.Contains(env, "GOPATH=/go") || !slices.Contains(env, "PATH=/bin") {
		t.Errorf("env %q", env)
	}
	check := checkEnv([]string{"PATH=/bin", "TI_GOLDEN_DIR=/g", "CLAUDE_CODE_OAUTH_TOKEN=t", "GH_TOKEN=x"})
	if !slices.Contains(check, "TI_GOLDEN_DIR=/g") || slices.ContainsFunc(check, func(e string) bool { return strings.Contains(e, "TOKEN") }) {
		t.Errorf("check env %q", check)
	}
}
