package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// prompt is the fixed prompt of every session: built into the program, so
// nothing on the Mac mini can change what the session is told.
//
//go:embed prompt.md
var prompt string

// allowedTools are the only tools the session may use without asking, and
// in a headless session nothing can be asked (--permission-mode dontAsk,
// --permission-prompts none): reading the worktree, changing files in it
// (file tools are confined to the working folder by --restricted), and
// running the tests and git's read-only commands.
var allowedTools = []string{
	"Read", "Grep", "Glob", "Edit", "Write",
	"Bash(go test *)", "Bash(go vet *)", "Bash(go build *)", "Bash(gofmt *)",
	"Bash(git diff *)", "Bash(git status *)", "Bash(git log *)", "Bash(git show *)",
}

// disallowedTools are denied outright, whatever any other rule allows: the
// web, every way to change git's history or reach GitHub, the server or
// another host, and every MCP server's tools.
var disallowedTools = []string{
	"WebFetch", "WebSearch",
	"Bash(git push *)", "Bash(git commit *)", "Bash(git reset *)", "Bash(git checkout *)", "Bash(git remote *)", "Bash(git config *)",
	"Bash(gh *)", "Bash(ssh *)", "Bash(scp *)", "Bash(sftp *)", "Bash(rsync *)", "Bash(curl *)", "Bash(wget *)", "Bash(nc *)",
	"Bash(tailscale *)", "Bash(security *)", "Bash(sudo *)", "mcp__*",
}

// analysisSchema is the answer's shape, checked by Claude Code itself
// (--json-schema) and by the job (analysis.Kind).
const analysisSchema = `{"type":"object","additionalProperties":false,` +
	`"required":["kind","what_happened","rule","owner_action","fix_title","fix_summary"],` +
	`"properties":{"kind":{"type":"string","enum":["code-defect","data-problem","owner-action"]},` +
	`"what_happened":{"type":"string"},"rule":{"type":"string"},"owner_action":{"type":"string"},` +
	`"fix_title":{"type":"string"},"fix_summary":{"type":"string"}}}`

// maxAnswer bounds what the session's output is read to.
const maxAnswer = 1 << 20

// claudeCLI is one headless Claude Code session (`claude -p`) per analysis.
type claudeCLI struct {
	path     string
	model    string
	timeout  time.Duration
	maxTurns int
	// env is the session's whole environment (sessionEnv).
	env []string
}

// claudeArgs is the session's fixed command line.
func (c claudeCLI) claudeArgs() []string {
	args := []string{
		"-p", "--restricted", "--tools", "Read,Grep,Glob,Edit,Write,Bash",
		"--permission-mode", "dontAsk", "--permission-prompts", "none",
		"--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence",
		"--output-format", "json", "--json-schema", analysisSchema,
		"--max-turns", strconv.Itoa(c.maxTurns),
	}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}
	args = append(append(args, "--allowedTools"), allowedTools...)
	return append(append(args, "--disallowedTools"), disallowedTools...)
}

// Analyse runs the session in the request's worktree, handing it the prompt,
// the issue and the slice on its input, and reads its structured answer.
func (c claudeCLI) Analyse(ctx context.Context, req analysisRequest) (analysis, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	//nolint:gosec // G204: the program is the operator's -claude flag and its arguments are the fixed list claudeArgs builds.
	cmd := exec.CommandContext(ctx, c.path, c.claudeArgs()...)
	cmd.Dir = req.Dir
	cmd.Env = c.env
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = strings.NewReader(prompt +
		fmt.Sprintf("\n\n## The alert issue\n\nTitle: %s\n\n%s\n\n## The journal slice\n\n%s\n", req.Issue.Title, req.Issue.Body, req.Slice))
	var out bytes.Buffer
	cmd.Stdout = &limitedBuffer{buf: &out, left: maxAnswer}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return analysis{}, fmt.Errorf("the session ran past its %s", c.timeout)
		}
		return analysis{}, errors.New("the session exited with an error")
	}
	var result struct {
		Subtype          string          `json:"subtype"`
		IsError          bool            `json:"is_error"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		return analysis{}, errors.New("the session's answer was not understood")
	}
	if result.IsError {
		return analysis{}, fmt.Errorf("the session ended with %s", result.Subtype)
	}
	if len(result.StructuredOutput) == 0 {
		return analysis{}, errors.New("the session gave no structured answer")
	}
	var a analysis
	if err := json.Unmarshal(result.StructuredOutput, &a); err != nil {
		return analysis{}, errors.New("the session's answer was not the shape asked for")
	}
	return a, nil
}

// limitedBuffer keeps at most left bytes and drops the rest, so a runaway
// session cannot fill the job's memory; an answer cut short fails to parse.
type limitedBuffer struct {
	buf  *bytes.Buffer
	left int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	n := min(len(p), l.left)
	l.buf.Write(p[:n])
	l.left -= n
	return len(p), nil
}

// keptNames are the only variables of the job's environment a session or a
// check is handed: what Go, git and Claude Code need to run. No GitHub
// token, SSH agent or cloud credential is among them.
var keptNames = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR",
	"GOPATH", "GOCACHE", "GOMODCACHE", "GOTOOLCHAIN", "GOFLAGS", "GOPROXY",
}

// kept is environ's variables named in names.
func kept(environ, names []string) []string {
	var out []string
	for _, e := range environ {
		name, _, ok := strings.Cut(e, "=")
		if ok && slices.Contains(names, name) {
			out = append(out, e)
		}
	}
	return out
}

// sessionEnv is a session's whole environment: the kept variables, git cut
// off from every credential helper (gitEnv), gh pointed at a configuration
// that does not exist, so it is not logged in, and Claude Code's own token
// when one is given.
func sessionEnv(environ []string, oauthToken string) []string {
	env := append(gitEnv(kept(environ, keptNames)), "GH_CONFIG_DIR=/var/empty/ti-triage-no-gh")
	if oauthToken != "" {
		env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+oauthToken)
	}
	return env
}

// checkEnv is the gate's test and check commands' environment: the kept
// variables and the golden scenarios' folder, and no token of any kind.
func checkEnv(environ []string) []string {
	return append(gitEnv(kept(environ, append(slices.Clone(keptNames), "TI_GOLDEN_DIR"))), "GH_CONFIG_DIR=/var/empty/ti-triage-no-gh")
}
