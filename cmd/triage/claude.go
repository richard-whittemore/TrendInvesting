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
// (file tools are confined to the working folder by --restricted), and a
// few exact command lines: the tests, vet, gofmt's listing and git's
// read-only commands, with no flag of the session's choosing, so no -exec,
// -toolexec, -vettool or -o runs or leaves another program. The sandbox
// (sessionSettings) is the boundary; the list keeps the session to what it
// needs.
var allowedTools = []string{
	"Read", "Grep", "Glob", "Edit", "Write",
	"Bash(go test ./...)", "Bash(go vet ./...)", "Bash(gofmt -l .)",
	"Bash(git diff)", "Bash(git status)", "Bash(git log)", "Bash(git show)",
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

// errInterrupted is a session the job's own stop ended (a SIGTERM, or
// launchd's bootout): not a failed analysis, so the issue is not spent on it.
var errInterrupted = errors.New("the job was stopped during the session")

// untrustedBegin and untrustedEnd mark where the alert's and the journal's
// text begin and end in the prompt: everything between them is data.
const (
	untrustedBegin = "<<<TRIAGE-DATA-BEGIN>>>"
	untrustedEnd   = "<<<TRIAGE-DATA-END>>>"
)

// sessionSettings is the session's own settings (--settings): Claude Code's
// sandbox around every command the session runs, required
// (failIfUnavailable), with no command let out of it, no network, the
// token folders unreadable and writes allowed only in the worktree and the
// build cache; and file reads kept to the working folder.
func sessionSettings(denyRead, allowWrite []string) string {
	type network struct {
		AllowedDomains []string `json:"allowedDomains"`
	}
	type filesystem struct {
		DenyRead   []string `json:"denyRead"`
		AllowWrite []string `json:"allowWrite"`
	}
	type sandbox struct {
		Enabled                  bool       `json:"enabled"`
		FailIfUnavailable        bool       `json:"failIfUnavailable"`
		AllowUnsandboxedCommands bool       `json:"allowUnsandboxedCommands"`
		AutoAllowBashIfSandboxed bool       `json:"autoAllowBashIfSandboxed"`
		Network                  network    `json:"network"`
		Filesystem               filesystem `json:"filesystem"`
	}
	type permissions struct {
		BlockReads bool `json:"blockReadsOutsideWorkingDirectories"`
	}
	raw, _ := json.Marshal(struct {
		Sandbox     sandbox     `json:"sandbox"`
		Permissions permissions `json:"permissions"`
	}{
		Sandbox: sandbox{Enabled: true, FailIfUnavailable: true, Network: network{AllowedDomains: []string{}},
			Filesystem: filesystem{DenyRead: append([]string{}, denyRead...), AllowWrite: append([]string{}, allowWrite...)}},
		Permissions: permissions{BlockReads: true},
	}) // a struct of strings and bools always encodes
	return string(raw)
}

// data is text from the alert or the journal, between the marks, with any
// mark inside it broken, so it cannot close its own.
func data(s string) string {
	s = strings.ReplaceAll(s, untrustedBegin, "<<< TRIAGE-DATA-BEGIN >>>")
	s = strings.ReplaceAll(s, untrustedEnd, "<<< TRIAGE-DATA-END >>>")
	return untrustedBegin + "\n" + s + "\n" + untrustedEnd
}

// claudeCLI is one headless Claude Code session (`claude -p`) per analysis.
type claudeCLI struct {
	path     string
	model    string
	timeout  time.Duration
	maxTurns int
	// env is the session's whole environment (sessionEnv); settings its
	// settings (sessionSettings).
	env      []string
	settings string
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
	if c.settings != "" {
		args = append(args, "--settings", c.settings)
	}
	args = append(append(args, "--allowedTools"), allowedTools...)
	return append(append(args, "--disallowedTools"), disallowedTools...)
}

// Analyse runs the session in the request's worktree, handing it the prompt,
// the issue and the slice on its input, and reads its structured answer.
func (c claudeCLI) Analyse(parent context.Context, req analysisRequest) (analysis, error) {
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()
	//nolint:gosec // G204: the program is the operator's -claude flag and its arguments are the fixed list claudeArgs builds.
	cmd := exec.CommandContext(ctx, c.path, c.claudeArgs()...)
	cmd.Dir = req.Dir
	cmd.Env = c.env
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = strings.NewReader(prompt +
		"\n\n## The alert issue\n\n" + data("Title: "+req.Issue.Title+"\n\n"+req.Issue.Body) +
		"\n\n## The journal slice\n\n" + data(req.Slice) + "\n")
	var out bytes.Buffer
	cmd.Stdout = &limitedBuffer{buf: &out, left: maxAnswer}
	if err := cmd.Run(); err != nil {
		if parent.Err() != nil {
			return analysis{}, errInterrupted
		}
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

// keptNames are the only variables of the job's environment a session or
// the job's own git and go commands are handed: what they need to run. No
// GitHub token, Anthropic API key or helper, SSH agent or cloud credential
// is among them, nor any Go setting that could run another program
// (GOFLAGS) or reach the network (GOPROXY): the job sets those itself.
var keptNames = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR",
	"GOPATH", "GOCACHE", "GOMODCACHE",
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
// that does not exist, so it is not logged in, Go offline and read-only
// (the job filled the module cache), and Claude Code's own OAuth token when
// one is given, so the session bills the subscription and never an API key.
func sessionEnv(environ []string, oauthToken string) []string {
	env := append(gitEnv(kept(environ, keptNames)), "GH_CONFIG_DIR=/var/empty/ti-triage-no-gh",
		"GOFLAGS=-mod=readonly", "GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off")
	if oauthToken != "" {
		env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+oauthToken)
	}
	return env
}

// withEnv is env with name set to value, replacing any value it had.
func withEnv(env []string, name, value string) []string {
	out := slices.DeleteFunc(slices.Clone(env), func(e string) bool { return strings.HasPrefix(e, name+"=") })
	return append(out, name+"="+value)
}
